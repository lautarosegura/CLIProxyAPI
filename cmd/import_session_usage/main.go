// Command import_session_usage backfills the proxy usage ledger from local Codex
// and Claude Code session logs, so the panel's usage chart has history from
// before the ledger existed. It aggregates hourly buckets locally and replaces
// the server's imported buckets; the server drops any bucket at or after the hour
// it started recording, so running it again never double counts.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageledger"
)

func main() {
	home, _ := os.UserHomeDir()
	server := flag.String("server", "http://127.0.0.1:8317", "proxy base URL")
	key := flag.String("key", os.Getenv("MANAGEMENT_KEY"), "management key (default $MANAGEMENT_KEY)")
	codexDir := flag.String("codex-dir", filepath.Join(home, ".codex", "sessions"), "Codex sessions directory (empty to skip)")
	claudeDir := flag.String("claude-dir", filepath.Join(home, ".claude", "projects"), "Claude Code projects directory (empty to skip)")
	dryRun := flag.Bool("dry-run", false, "print totals without uploading")
	flag.Parse()

	if errRun := run(*server, *key, *codexDir, *claudeDir, *dryRun); errRun != nil {
		fmt.Fprintln(os.Stderr, "error:", errRun)
		os.Exit(1)
	}
}

func run(server, key, codexDir, claudeDir string, dryRun bool) error {
	agg := newAggregator()
	if codexDir != "" {
		if errScan := scanDir(codexDir, agg.addCodexFile); errScan != nil {
			return fmt.Errorf("scan codex sessions: %w", errScan)
		}
	}
	if claudeDir != "" {
		if errScan := scanDir(claudeDir, agg.addClaudeFile); errScan != nil {
			return fmt.Errorf("scan claude projects: %w", errScan)
		}
	}
	buckets := agg.buckets()
	printSummary(buckets)
	if dryRun {
		return nil
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("management key required (-key or $MANAGEMENT_KEY)")
	}
	return upload(server, key, buckets)
}

// scanDir runs fn for every .jsonl file under dir using a worker per CPU.
func scanDir(dir string, fn func(path string) error) error {
	if _, errStat := os.Stat(dir); errors.Is(errStat, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "skipping missing %s\n", dir)
		return nil
	}
	paths := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range paths {
				if errFile := fn(path); errFile != nil {
					fmt.Fprintf(os.Stderr, "skipping %s: %v\n", path, errFile)
				}
			}
		}()
	}
	errWalk := filepath.WalkDir(dir, func(path string, d fs.DirEntry, errEntry error) error {
		if errEntry != nil {
			return errEntry
		}
		if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			paths <- path
		}
		return nil
	})
	close(paths)
	wg.Wait()
	return errWalk
}

type bucketKey struct {
	hour            int64
	provider, model string
}

type aggregator struct {
	mu      sync.Mutex
	totals  map[bucketKey]*usageledger.Tokens
	seen    map[string]struct{}
	minHour int64
}

func newAggregator() *aggregator {
	return &aggregator{totals: map[bucketKey]*usageledger.Tokens{}, seen: map[string]struct{}{}}
}

// add records tokens once per dedupe id; forked or resumed sessions repeat events.
func (a *aggregator) add(id string, at time.Time, provider, model string, tokens usageledger.Tokens) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, dup := a.seen[id]; dup {
		return
	}
	a.seen[id] = struct{}{}
	key := bucketKey{hour: at.UTC().Truncate(time.Hour).Unix(), provider: provider, model: model}
	t := a.totals[key]
	if t == nil {
		t = &usageledger.Tokens{}
		a.totals[key] = t
	}
	t.Requests += tokens.Requests
	t.InputTokens += tokens.InputTokens
	t.CacheReadTokens += tokens.CacheReadTokens
	t.CacheWriteTokens += tokens.CacheWriteTokens
	t.OutputTokens += tokens.OutputTokens
	t.ReasoningTokens += tokens.ReasoningTokens
}

func (a *aggregator) buckets() []usageledger.Bucket {
	out := make([]usageledger.Bucket, 0, len(a.totals))
	for key, t := range a.totals {
		out = append(out, usageledger.Bucket{Hour: key.hour, Provider: key.provider, Model: key.model, Tokens: *t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hour < out[j].Hour })
	return out
}

// eachLine calls fn for lines containing marker; session logs can have very long lines.
func eachLine(path string, marker []byte, fn func(line []byte)) error {
	f, errOpen := os.Open(path)
	if errOpen != nil {
		return errOpen
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, errRead := r.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, marker) {
			fn(line)
		}
		if errRead == io.EOF {
			return nil
		}
		if errRead != nil {
			return errRead
		}
	}
}

type codexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

type codexLine struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type  string `json:"type"`
		Model string `json:"model"`
		Info  *struct {
			Total codexUsage `json:"total_token_usage"`
			Last  codexUsage `json:"last_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

// addCodexFile counts each token_count event whose cumulative total advanced;
// Codex re-emits the same totals when only rate limits change.
func (a *aggregator) addCodexFile(path string) error {
	model := "unknown"
	var lastTotal int64
	return eachLine(path, []byte(`"payload"`), func(line []byte) {
		if !bytes.Contains(line, []byte(`"turn_context"`)) && !bytes.Contains(line, []byte(`"token_count"`)) {
			return
		}
		var entry codexLine
		if json.Unmarshal(line, &entry) != nil {
			return
		}
		if entry.Type == "turn_context" {
			if m := strings.TrimSpace(entry.Payload.Model); m != "" {
				model = strings.ToLower(m)
			}
			return
		}
		info := entry.Payload.Info
		if entry.Payload.Type != "token_count" || info == nil || info.Total.TotalTokens == lastTotal {
			return
		}
		lastTotal = info.Total.TotalTokens
		u := info.Last
		cached := u.CachedInputTokens + u.CacheWriteInputTokens
		tokens := usageledger.Tokens{
			Requests:         1,
			InputTokens:      max(u.InputTokens-cached, 0),
			CacheReadTokens:  u.CachedInputTokens,
			CacheWriteTokens: u.CacheWriteInputTokens,
			OutputTokens:     u.OutputTokens,
			ReasoningTokens:  min(u.ReasoningOutputTokens, u.OutputTokens),
		}
		id := fmt.Sprintf("codex:%s:%d:%d", entry.Timestamp.Format(time.RFC3339Nano), info.Total.TotalTokens, u.TotalTokens)
		a.add(id, entry.Timestamp, "codex", model, tokens)
	})
}

type claudeLine struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	RequestID string    `json:"requestId"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// addClaudeFile counts each assistant message once; Claude Code writes one line
// per content block with the same message id and usage.
func (a *aggregator) addClaudeFile(path string) error {
	return eachLine(path, []byte(`"usage"`), func(line []byte) {
		var entry claudeLine
		if json.Unmarshal(line, &entry) != nil || entry.Type != "assistant" || entry.Message.Usage == nil || entry.Message.ID == "" {
			return
		}
		model := strings.ToLower(strings.TrimSpace(entry.Message.Model))
		if model == "" || model == "<synthetic>" {
			return
		}
		u := entry.Message.Usage
		tokens := usageledger.Tokens{
			Requests:         1,
			InputTokens:      u.InputTokens,
			CacheReadTokens:  u.CacheReadInputTokens,
			CacheWriteTokens: u.CacheCreationInputTokens,
			OutputTokens:     u.OutputTokens,
		}
		a.add("claude:"+entry.Message.ID+":"+entry.RequestID, entry.Timestamp, "claude", model, tokens)
	})
}

func printSummary(buckets []usageledger.Bucket) {
	type total struct{ requests, processed int64 }
	byProvider := map[string]*total{}
	var first, last int64
	for _, b := range buckets {
		t := byProvider[b.Provider]
		if t == nil {
			t = &total{}
			byProvider[b.Provider] = t
		}
		t.requests += b.Requests
		t.processed += b.InputTokens + b.CacheReadTokens + b.CacheWriteTokens + b.OutputTokens
		if first == 0 || b.Hour < first {
			first = b.Hour
		}
		last = max(last, b.Hour)
	}
	if len(buckets) == 0 {
		fmt.Println("no usage found")
		return
	}
	fmt.Printf("%d hourly buckets from %s to %s\n", len(buckets),
		time.Unix(first, 0).UTC().Format(time.DateOnly), time.Unix(last, 0).UTC().Format(time.DateOnly))
	for provider, t := range byProvider {
		fmt.Printf("  %-8s %10d requests %16d tokens\n", provider, t.requests, t.processed)
	}
}

func upload(server, key string, buckets []usageledger.Bucket) error {
	body, errMarshal := json.Marshal(map[string]any{"buckets": buckets})
	if errMarshal != nil {
		return errMarshal
	}
	url := strings.TrimRight(server, "/") + "/v8/management/observability/usage/history/import"
	req, errReq := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if errReq != nil {
		return errReq
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, errDo := http.DefaultClient.Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload failed: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	fmt.Println("uploaded:", strings.TrimSpace(string(respBody)))
	return nil
}
