package usageledger

import (
	"context"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

var active atomic.Pointer[Ledger]

func init() {
	coreusage.RegisterNamedPlugin("usage-ledger", ledgerPlugin{})
}

// SetActive installs the ledger that receives proxy usage and serves the
// management API. Passing nil stops recording.
func SetActive(l *Ledger) { active.Store(l) }

// Active returns the installed ledger, or nil when none is running.
func Active() *Ledger { return active.Load() }

type ledgerPlugin struct{}

// HandleUsage records proxy usage while `usage-statistics-enabled` is on.
func (ledgerPlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	l := Active()
	if l == nil || !redisqueue.UsageStatisticsEnabled() {
		return
	}
	tokens, ok := tokensFromRecord(record)
	if !ok {
		return
	}
	l.Record(record.RequestedAt, record.Provider, record.Model, tokens)
}

func tokensFromRecord(record coreusage.Record) (Tokens, bool) {
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	b := detail.TokenBreakdown
	tokens := Tokens{
		Requests:         1,
		InputTokens:      b.Input.UncachedTokens,
		CacheReadTokens:  b.Input.CacheReadTokens,
		CacheWriteTokens: b.Input.CacheWriteTokens,
		OutputTokens:     b.Output.TotalTokens,
		ReasoningTokens:  b.Output.ReasoningTokens,
	}
	// Tokens a provider reports without a known split are counted as uncached
	// input so totals stay complete; this slightly overestimates cost.
	tokens.InputTokens += b.UnclassifiedTokens
	if record.Failed && tokens.InputTokens+tokens.CacheReadTokens+tokens.CacheWriteTokens+tokens.OutputTokens == 0 {
		return Tokens{}, false
	}
	return tokens, true
}
