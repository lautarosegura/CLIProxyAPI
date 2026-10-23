package usageledger

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func newTestLedger(t *testing.T, now time.Time) *Ledger {
	t.Helper()
	l := New(filepath.Join(t.TempDir(), FileName))
	l.now = func() time.Time { return now }
	return l
}

func TestDailyTotalsGroupsByLocalDay(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	l := newTestLedger(t, now)
	// 01:30 UTC on Oct 23 is still Oct 22 at UTC-3.
	l.Record(time.Date(2026, 10, 23, 1, 30, 0, 0, time.UTC), "Codex", "gpt-6.1-sol", Tokens{Requests: 1, InputTokens: 10, OutputTokens: 5})
	l.Record(time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC), "codex", "gpt-6.1-sol", Tokens{Requests: 1, CacheReadTokens: 100})
	l.Record(time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC), "codex", "gpt-6.1-sol", Tokens{Requests: 1, InputTokens: 1})

	got := l.DailyTotals(7, -3*time.Hour)
	if len(got) != 2 {
		t.Fatalf("expected 2 days, got %+v", got)
	}
	if got[0].Date != "2026-10-22" || got[0].InputTokens != 10 || got[0].Provider != "codex" {
		t.Fatalf("unexpected first day: %+v", got[0])
	}
	if got[1].Date != "2026-10-23" || got[1].CacheReadTokens != 100 {
		t.Fatalf("unexpected second day: %+v", got[1])
	}
}

func TestReplaceImportedDropsOverlapWithProxy(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	l := newTestLedger(t, now)
	l.Record(time.Date(2026, 10, 23, 10, 15, 0, 0, time.UTC), "claude", "claude-opus-5-5", Tokens{Requests: 1, InputTokens: 1})

	before := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC).Unix()
	overlap := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC).Unix()
	accepted, dropped := l.ReplaceImported([]Bucket{
		{Hour: before, Provider: "claude", Model: "claude-opus-5-5", Tokens: Tokens{Requests: 2, InputTokens: 7}},
		{Hour: overlap, Provider: "claude", Model: "claude-opus-5-5", Tokens: Tokens{Requests: 1, InputTokens: 99}},
		{Hour: before, Provider: "claude", Model: "x", Tokens: Tokens{OutputTokens: 1, ReasoningTokens: 2}},
	})
	if accepted != 1 || dropped != 2 {
		t.Fatalf("accepted=%d dropped=%d", accepted, dropped)
	}

	// A second import replaces the first one instead of adding to it.
	l.ReplaceImported([]Bucket{{Hour: before, Provider: "claude", Model: "claude-opus-5-5", Tokens: Tokens{Requests: 1, InputTokens: 3}}})
	var imported, proxied int64
	for _, day := range l.DailyTotals(1, 0) {
		switch day.Source {
		case SourceImport:
			imported += day.InputTokens
		case SourceProxy:
			proxied += day.InputTokens
		}
	}
	if imported != 3 || proxied != 1 {
		t.Fatalf("imported=%d proxied=%d", imported, proxied)
	}
}

func TestSaveLoadRoundTripAndRetention(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	l := newTestLedger(t, now)
	l.Record(now, "codex", "gpt-6.1-sol", Tokens{Requests: 1, InputTokens: 4, OutputTokens: 2, ReasoningTokens: 1})
	l.ReplaceImported([]Bucket{
		{Hour: now.Add(-48 * time.Hour).Unix(), Provider: "codex", Model: "gpt-5.5", Tokens: Tokens{Requests: 1, InputTokens: 8}},
		{Hour: now.Add(-Retention - time.Hour).Unix(), Provider: "codex", Model: "gpt-5.5", Tokens: Tokens{Requests: 1}},
	})
	if errSave := l.Save(); errSave != nil {
		t.Fatalf("save: %v", errSave)
	}

	loaded := New(l.path)
	loaded.now = l.now
	if errLoad := loaded.Load(); errLoad != nil {
		t.Fatalf("load: %v", errLoad)
	}
	if !loaded.StartedAt().Equal(now) {
		t.Fatalf("started_at=%v", loaded.StartedAt())
	}
	got := loaded.DailyTotals(7, 0)
	if len(got) != 2 || got[0].InputTokens != 8 || got[1].ReasoningTokens != 1 {
		t.Fatalf("unexpected totals: %+v", got)
	}
}

func TestTokensFromRecordUsesCanonicalBreakdown(t *testing.T) {
	// Codex reports cached tokens inside the input total (fields as set by ParseCodexUsage).
	codex, ok := tokensFromRecord(coreusage.Record{
		Provider: "codex",
		Detail:   coreusage.Detail{InputTokens: 1000, CachedTokens: 800, CacheReadTokens: 800, OutputTokens: 50, ReasoningTokens: 20},
	})
	if !ok || codex.InputTokens != 200 || codex.CacheReadTokens != 800 || codex.OutputTokens != 50 || codex.ReasoningTokens != 20 {
		t.Fatalf("codex tokens: %+v", codex)
	}
	// Claude reports uncached input separately from cache reads and writes.
	claude, ok := tokensFromRecord(coreusage.Record{
		Provider: "claude",
		Detail:   coreusage.Detail{InputTokens: 3, CacheReadTokens: 500, CacheCreationTokens: 40, OutputTokens: 9},
	})
	if !ok || claude.InputTokens != 3 || claude.CacheReadTokens != 500 || claude.CacheWriteTokens != 40 || claude.OutputTokens != 9 {
		t.Fatalf("claude tokens: %+v", claude)
	}
	if _, ok := tokensFromRecord(coreusage.Record{Provider: "claude", Failed: true}); ok {
		t.Fatal("failed request without tokens should be skipped")
	}
}

func TestConcurrentSavesKeepNewestState(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	l := newTestLedger(t, now)
	const writers = 50
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Record(now, "codex", "gpt-6.1-sol", Tokens{Requests: 1, InputTokens: 1})
			if errSave := l.Save(); errSave != nil {
				t.Error(errSave)
			}
		}()
	}
	wg.Wait()
	if errSave := l.Save(); errSave != nil {
		t.Fatal(errSave)
	}

	loaded := New(l.path)
	loaded.now = l.now
	if errLoad := loaded.Load(); errLoad != nil {
		t.Fatal(errLoad)
	}
	got := loaded.DailyTotals(1, 0)
	if len(got) != 1 || got[0].Requests != writers {
		t.Fatalf("persisted totals lost updates: %+v", got)
	}
}
