// Package usageledger keeps a persistent, hourly aggregate of token usage so the
// management panel can chart usage over weeks, independently of the transient
// usage queue. Only token counts are stored; costs are estimated by clients.
package usageledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// FileName is the ledger file inside the auth directory. It avoids the .json
// extension so auth watchers and token stores never treat it as a credential.
const FileName = "usage-ledger.ulg"

// Retention bounds how long hourly buckets are kept.
const Retention = 400 * 24 * time.Hour

// MaxQueryDays is the largest range accepted by DailyTotals callers.
const MaxQueryDays = 400

// Bucket sources.
const (
	SourceProxy  = "proxy"
	SourceImport = "import"
)

const fileVersion = 1

// Tokens holds non-overlapping token buckets. InputTokens excludes cache reads
// and writes; ReasoningTokens is a subset of OutputTokens.
type Tokens struct {
	Requests         int64 `json:"requests"`
	InputTokens      int64 `json:"input_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
}

func (t *Tokens) add(o Tokens) {
	t.Requests += o.Requests
	t.InputTokens += o.InputTokens
	t.CacheReadTokens += o.CacheReadTokens
	t.CacheWriteTokens += o.CacheWriteTokens
	t.OutputTokens += o.OutputTokens
	t.ReasoningTokens += o.ReasoningTokens
}

func (t Tokens) valid() bool {
	return t.Requests >= 0 && t.InputTokens >= 0 && t.CacheReadTokens >= 0 && t.CacheWriteTokens >= 0 &&
		t.OutputTokens >= 0 && t.ReasoningTokens >= 0 && t.ReasoningTokens <= t.OutputTokens
}

// Bucket aggregates usage for one UTC hour, source, provider and model.
type Bucket struct {
	// Hour is the Unix time (seconds) of the start of the UTC hour.
	Hour     int64  `json:"hour"`
	Source   string `json:"source"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Tokens
}

// DayTotal aggregates usage for one local calendar day, source, provider and model.
type DayTotal struct {
	Date     string `json:"date"`
	Source   string `json:"source"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Tokens
}

type bucketKey struct {
	hour     int64
	source   string
	provider string
	model    string
}

type stateFile struct {
	Version   int        `json:"version"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	Buckets   []Bucket   `json:"buckets"`
}

// Ledger is a concurrency-safe hourly usage aggregate backed by a single file.
type Ledger struct {
	mu        sync.Mutex
	path      string
	startedAt time.Time
	buckets   map[bucketKey]*Tokens
	dirty     bool
	now       func() time.Time
}

// New creates an empty ledger persisted at path. An empty path keeps it in memory.
func New(path string) *Ledger {
	return &Ledger{path: strings.TrimSpace(path), buckets: map[bucketKey]*Tokens{}, now: time.Now}
}

// Load replaces the in-memory state with the persisted file. A missing file is empty state.
func (l *Ledger) Load() error {
	if l.path == "" {
		return nil
	}
	data, errRead := os.ReadFile(l.path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read usage ledger: %w", errRead)
	}
	var state stateFile
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		return fmt.Errorf("parse usage ledger: %w", errUnmarshal)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buckets = make(map[bucketKey]*Tokens, len(state.Buckets))
	l.startedAt = time.Time{}
	if state.StartedAt != nil {
		l.startedAt = state.StartedAt.UTC()
	}
	for _, b := range state.Buckets {
		if !b.valid() {
			continue
		}
		l.addLocked(bucketKey{hour: b.Hour, source: b.Source, provider: b.Provider, model: b.Model}, b.Tokens)
	}
	l.dirty = false
	return nil
}

// Record adds proxy-observed usage at the given time.
func (l *Ledger) Record(at time.Time, provider, model string, tokens Tokens) {
	if !tokens.valid() {
		return
	}
	if at.IsZero() {
		at = l.now()
	}
	hour := at.UTC().Truncate(time.Hour)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.startedAt.IsZero() {
		l.startedAt = hour
	}
	l.addLocked(bucketKey{hour: hour.Unix(), source: SourceProxy, provider: normalize(provider), model: normalize(model)}, tokens)
	l.dirty = true
}

// ReplaceImported replaces all imported buckets. Buckets at or after the hour the
// proxy started recording are dropped so the same traffic is never counted twice.
// It returns how many buckets were accepted and dropped.
func (l *Ledger) ReplaceImported(buckets []Bucket) (accepted, dropped int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key := range l.buckets {
		if key.source == SourceImport {
			delete(l.buckets, key)
		}
	}
	oldest := l.now().Add(-Retention).Unix()
	for _, b := range buckets {
		hour := time.Unix(b.Hour, 0).UTC().Truncate(time.Hour).Unix()
		if !b.valid() || hour < oldest || (!l.startedAt.IsZero() && hour >= l.startedAt.Unix()) {
			dropped++
			continue
		}
		l.addLocked(bucketKey{hour: hour, source: SourceImport, provider: normalize(b.Provider), model: normalize(b.Model)}, b.Tokens)
		accepted++
	}
	l.dirty = true
	return accepted, dropped
}

// StartedAt returns the hour the proxy recorded its first usage, or zero.
func (l *Ledger) StartedAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.startedAt
}

// DailyTotals aggregates the last days calendar days (including today) in a
// fixed UTC offset, sorted by date, source, provider and model.
func (l *Ledger) DailyTotals(days int, offset time.Duration) []DayTotal {
	if days <= 0 {
		return nil
	}
	zone := time.FixedZone("", int(offset.Seconds()))
	now := l.now().In(zone)
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, zone).AddDate(0, 0, -(days - 1))

	type dayKey struct {
		date, source, provider, model string
	}
	totals := map[dayKey]*Tokens{}
	l.mu.Lock()
	for key, tokens := range l.buckets {
		at := time.Unix(key.hour, 0).In(zone)
		if at.Before(from) {
			continue
		}
		dk := dayKey{date: at.Format("2006-01-02"), source: key.source, provider: key.provider, model: key.model}
		if totals[dk] == nil {
			totals[dk] = &Tokens{}
		}
		totals[dk].add(*tokens)
	}
	l.mu.Unlock()

	out := make([]DayTotal, 0, len(totals))
	for key, tokens := range totals {
		out = append(out, DayTotal{Date: key.date, Source: key.source, Provider: key.provider, Model: key.model, Tokens: *tokens})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	return out
}

// Save writes the ledger atomically when it changed since the last save.
func (l *Ledger) Save() error {
	if l.path == "" {
		return nil
	}
	l.mu.Lock()
	if !l.dirty {
		l.mu.Unlock()
		return nil
	}
	oldest := l.now().Add(-Retention).Unix()
	state := stateFile{Version: fileVersion, Buckets: make([]Bucket, 0, len(l.buckets))}
	if !l.startedAt.IsZero() {
		startedAt := l.startedAt
		state.StartedAt = &startedAt
	}
	for key, tokens := range l.buckets {
		if key.hour < oldest {
			delete(l.buckets, key)
			continue
		}
		state.Buckets = append(state.Buckets, Bucket{Hour: key.hour, Source: key.source, Provider: key.provider, Model: key.model, Tokens: *tokens})
	}
	l.dirty = false
	l.mu.Unlock()

	sort.Slice(state.Buckets, func(i, j int) bool { return state.Buckets[i].Hour < state.Buckets[j].Hour })
	data, errMarshal := json.Marshal(state)
	if errMarshal == nil {
		errMarshal = writeFileAtomic(l.path, data)
	}
	if errMarshal != nil {
		l.mu.Lock()
		l.dirty = true
		l.mu.Unlock()
		return fmt.Errorf("save usage ledger: %w", errMarshal)
	}
	return nil
}

// Run saves the ledger every interval until ctx is done.
func (l *Ledger) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if errSave := l.Save(); errSave != nil {
				log.Warnf("usage ledger: %v", errSave)
			}
		}
	}
}

func (l *Ledger) addLocked(key bucketKey, tokens Tokens) {
	existing := l.buckets[key]
	if existing == nil {
		existing = &Tokens{}
		l.buckets[key] = existing
	}
	existing.add(tokens)
}

func normalize(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	return value
}

func writeFileAtomic(path string, data []byte) error {
	tmp, errCreate := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if errCreate != nil {
		return errCreate
	}
	tmpName := tmp.Name()
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return errWrite
	}
	if errClose := tmp.Close(); errClose != nil {
		_ = os.Remove(tmpName)
		return errClose
	}
	if errRename := os.Rename(tmpName, path); errRename != nil {
		_ = os.Remove(tmpName)
		return errRename
	}
	return nil
}
