package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLog(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	return path
}

func TestCodexSkipsRepeatedTotalsAndForkedCopies(t *testing.T) {
	dir := t.TempDir()
	content := `{"timestamp":"2026-10-23T14:22:18Z","type":"turn_context","payload":{"model":"gpt-6.1-sol"}}
{"timestamp":"2026-10-23T14:22:24Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":100},"last_token_usage":{"input_tokens":90,"cached_input_tokens":60,"output_tokens":10,"reasoning_output_tokens":4,"total_tokens":100}}}}
{"timestamp":"2026-10-23T14:22:25Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":100},"last_token_usage":{"input_tokens":90,"cached_input_tokens":60,"output_tokens":10,"total_tokens":100}}}}
`
	a := newAggregator()
	for _, name := range []string{"a.jsonl", "fork.jsonl"} {
		if errAdd := a.addCodexFile(writeLog(t, dir, name, content)); errAdd != nil {
			t.Fatal(errAdd)
		}
	}
	buckets := a.buckets()
	if len(buckets) != 1 {
		t.Fatalf("buckets: %+v", buckets)
	}
	b := buckets[0]
	if b.Model != "gpt-6.1-sol" || b.Requests != 1 || b.InputTokens != 30 || b.CacheReadTokens != 60 || b.OutputTokens != 10 || b.ReasoningTokens != 4 {
		t.Fatalf("bucket: %+v", b)
	}
}

func TestClaudeCountsEachMessageOnce(t *testing.T) {
	dir := t.TempDir()
	line := `{"timestamp":"2026-10-23T02:56:06Z","type":"assistant","requestId":"req_1","message":{"id":"msg_1","model":"claude-opus-5-5","usage":{"input_tokens":2,"cache_creation_input_tokens":37,"cache_read_input_tokens":12,"output_tokens":178}}}
`
	synthetic := `{"timestamp":"2026-10-23T02:56:07Z","type":"assistant","message":{"id":"msg_2","model":"<synthetic>","usage":{"input_tokens":5}}}
`
	a := newAggregator()
	if errAdd := a.addClaudeFile(writeLog(t, dir, "s.jsonl", line+line+synthetic)); errAdd != nil {
		t.Fatal(errAdd)
	}
	buckets := a.buckets()
	if len(buckets) != 1 {
		t.Fatalf("buckets: %+v", buckets)
	}
	b := buckets[0]
	if b.Requests != 1 || b.InputTokens != 2 || b.CacheWriteTokens != 37 || b.CacheReadTokens != 12 || b.OutputTokens != 178 {
		t.Fatalf("bucket: %+v", b)
	}
}
