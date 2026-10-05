package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var soonestResetTestNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func newSoonestResetTestSelector() *SoonestResetSelector {
	return &SoonestResetSelector{nowFunc: func() time.Time { return soonestResetTestNow }}
}

func claudeWeeklySignals(utilization string, resetAt time.Time, status string) map[string]string {
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(resetAt.Unix(), 10),
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.1",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(soonestResetTestNow.Add(time.Hour).Unix(), 10),
	}
	if utilization != "" {
		signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = utilization
	}
	if status != "" {
		signals["Anthropic-Ratelimit-Unified-7d-Status"] = status
	}
	return signals
}

func claudeQuotaAuth(id, utilization string, resetIn time.Duration) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Quota: QuotaState{
			ObservedAt: soonestResetTestNow.Add(-time.Minute),
			Signals:    claudeWeeklySignals(utilization, soonestResetTestNow.Add(resetIn), "allowed"),
		},
	}
}

func pickSoonestReset(t *testing.T, selector Selector, provider string, auths []*Auth) string {
	t.Helper()
	picked, errPick := selector.Pick(context.Background(), provider, "test-model", cliproxyexecutor.Options{}, auths)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if picked == nil {
		t.Fatal("Pick() returned nil")
	}
	return picked.ID
}

func TestParseClaudeWeeklyQuotaWindow(t *testing.T) {
	resetAt := soonestResetTestNow.Add(48 * time.Hour)
	tests := []struct {
		name          string
		signals       map[string]string
		wantOK        bool
		wantRemaining float64
		wantExhausted bool
	}{
		{name: "half used", signals: claudeWeeklySignals("0.5", resetAt, "allowed"), wantOK: true, wantRemaining: 0.5},
		{name: "fully used", signals: claudeWeeklySignals("1.0", resetAt, "allowed_warning"), wantOK: true, wantExhausted: true},
		{name: "rejected without utilization", signals: claudeWeeklySignals("", resetAt, "rejected"), wantOK: true, wantExhausted: true},
		{name: "rejected with low utilization", signals: claudeWeeklySignals("0.2", resetAt, "rejected"), wantOK: true, wantExhausted: true},
		{name: "missing utilization", signals: claudeWeeklySignals("", resetAt, "allowed"), wantOK: false},
		{name: "unparsable utilization", signals: claudeWeeklySignals("abc", resetAt, ""), wantOK: false},
		{name: "missing reset", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.3"}, wantOK: false},
		{name: "unparsable reset", signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.3",
			"Anthropic-Ratelimit-Unified-7d-Reset":       "soon",
		}, wantOK: false},
		{name: "rfc3339 reset", signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.25",
			"Anthropic-Ratelimit-Unified-7d-Reset":       resetAt.Format(time.RFC3339),
		}, wantOK: true, wantRemaining: 0.75},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			window, ok := parseWeeklyQuotaWindow(tt.signals, soonestResetTestNow)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (window %+v)", ok, tt.wantOK, window)
			}
			if !ok {
				return
			}
			if !window.ResetAt.Equal(resetAt) {
				t.Fatalf("ResetAt = %v, want %v", window.ResetAt, resetAt)
			}
			if window.Exhausted != tt.wantExhausted {
				t.Fatalf("Exhausted = %v, want %v", window.Exhausted, tt.wantExhausted)
			}
			if !tt.wantExhausted && window.Remaining != tt.wantRemaining {
				t.Fatalf("Remaining = %v, want %v", window.Remaining, tt.wantRemaining)
			}
		})
	}
}

func TestParseCodexWeeklyQuotaWindow(t *testing.T) {
	observedAt := soonestResetTestNow.Add(-10 * time.Minute)
	absoluteReset := soonestResetTestNow.Add(72 * time.Hour)
	tests := []struct {
		name          string
		signals       map[string]string
		wantOK        bool
		wantReset     time.Time
		wantRemaining float64
		wantExhausted bool
	}{
		{
			name: "secondary weekly relative reset",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":          "90",
				"X-Codex-Primary-Window-Minutes":        "300",
				"X-Codex-Primary-Reset-After-Seconds":   "600",
				"X-Codex-Secondary-Used-Percent":        "25",
				"X-Codex-Secondary-Window-Minutes":      "10080",
				"X-Codex-Secondary-Reset-After-Seconds": "7200",
			},
			wantOK:        true,
			wantReset:     observedAt.Add(2 * time.Hour),
			wantRemaining: 0.75,
		},
		{
			name: "absolute reset preferred over relative",
			signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":        "40",
				"X-Codex-Secondary-Window-Minutes":      "10080",
				"X-Codex-Secondary-Reset-After-Seconds": "60",
				"X-Codex-Secondary-Reset-At":            strconv.FormatInt(absoluteReset.Unix(), 10),
			},
			wantOK:        true,
			wantReset:     absoluteReset,
			wantRemaining: 0.6,
		},
		{
			name: "weekly primary only",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":   "100",
				"X-Codex-Primary-Window-Minutes": "10080",
				"X-Codex-Primary-Reset-At":       strconv.FormatInt(absoluteReset.Unix(), 10),
			},
			wantOK:        true,
			wantReset:     absoluteReset,
			wantExhausted: true,
		},
		{
			name: "only short window",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":        "10",
				"X-Codex-Primary-Window-Minutes":      "300",
				"X-Codex-Primary-Reset-After-Seconds": "600",
			},
			wantOK: false,
		},
		{
			name: "missing used percent",
			signals: map[string]string{
				"X-Codex-Secondary-Window-Minutes":      "10080",
				"X-Codex-Secondary-Reset-After-Seconds": "600",
			},
			wantOK: false,
		},
		{
			name: "missing reset",
			signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":   "10",
				"X-Codex-Secondary-Window-Minutes": "10080",
			},
			wantOK: false,
		},
		{name: "no signals", signals: nil, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			window, ok := parseWeeklyQuotaWindow(tt.signals, observedAt)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (window %+v)", ok, tt.wantOK, window)
			}
			if !ok {
				return
			}
			if !window.ResetAt.Equal(tt.wantReset) {
				t.Fatalf("ResetAt = %v, want %v", window.ResetAt, tt.wantReset)
			}
			if window.Exhausted != tt.wantExhausted {
				t.Fatalf("Exhausted = %v, want %v", window.Exhausted, tt.wantExhausted)
			}
			if !tt.wantExhausted && window.Remaining != tt.wantRemaining {
				t.Fatalf("Remaining = %v, want %v", window.Remaining, tt.wantRemaining)
			}
		})
	}
}

func TestWeeklyQuotaWindowForAuthTreatsPastResetAsUnknown(t *testing.T) {
	auth := claudeQuotaAuth("stale", "0.9", -time.Minute)
	if window, ok := weeklyQuotaWindowForAuth(auth, "test-model", soonestResetTestNow); ok {
		t.Fatalf("weeklyQuotaWindowForAuth() = %+v, true; want unknown for a reset in the past", window)
	}

	// Relative Codex resets are anchored to the observation time, so an old snapshot expires.
	codex := &Auth{ID: "codex", Quota: QuotaState{
		ObservedAt: soonestResetTestNow.Add(-3 * time.Hour),
		Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":        "10",
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": "3600",
		},
	}}
	if window, ok := weeklyQuotaWindowForAuth(codex, "test-model", soonestResetTestNow); ok {
		t.Fatalf("weeklyQuotaWindowForAuth() = %+v, true; want unknown for an expired relative reset", window)
	}
}

func TestWeeklyQuotaWindowForAuthUsesNewestSnapshot(t *testing.T) {
	auth := claudeQuotaAuth("auth", "0.2", 48*time.Hour)
	auth.ModelStates = map[string]*ModelState{
		"test-model": {Quota: QuotaState{
			ObservedAt: soonestResetTestNow,
			Signals:    claudeWeeklySignals("0.7", soonestResetTestNow.Add(24*time.Hour), "allowed"),
		}},
	}
	window, ok := weeklyQuotaWindowForAuth(auth, "test-model", soonestResetTestNow)
	if !ok {
		t.Fatal("weeklyQuotaWindowForAuth() reported unknown")
	}
	if !window.ResetAt.Equal(soonestResetTestNow.Add(24 * time.Hour)) {
		t.Fatalf("ResetAt = %v, want newer model-state snapshot", window.ResetAt)
	}

	// An older model snapshot must not override a newer credential snapshot.
	auth.ModelStates["test-model"].Quota.ObservedAt = soonestResetTestNow.Add(-time.Hour)
	window, ok = weeklyQuotaWindowForAuth(auth, "test-model", soonestResetTestNow)
	if !ok || !window.ResetAt.Equal(soonestResetTestNow.Add(48*time.Hour)) {
		t.Fatalf("window = %+v, %v; want credential snapshot", window, ok)
	}
}

func TestSoonestResetSelectorPrefersSoonerResetOverMoreRemaining(t *testing.T) {
	selector := newSoonestResetTestSelector()
	auths := []*Auth{
		claudeQuotaAuth("b-full-3d", "0.0", 72*time.Hour),
		claudeQuotaAuth("a-half-2h", "0.5", 2*time.Hour),
	}
	for i := 0; i < 3; i++ {
		if got := pickSoonestReset(t, selector, "claude", auths); got != "a-half-2h" {
			t.Fatalf("pick #%d = %q, want a-half-2h", i, got)
		}
	}
}

func TestSoonestResetSelectorRanksUnknownFirstAndRotates(t *testing.T) {
	selector := newSoonestResetTestSelector()
	auths := []*Auth{
		claudeQuotaAuth("known", "0.5", 2*time.Hour),
		{ID: "unknown-b", Provider: "claude"},
		claudeQuotaAuth("stale-a", "0.9", -time.Hour),
		{ID: "unknown-c", Provider: "claude"},
	}
	want := []string{"stale-a", "unknown-b", "unknown-c", "stale-a", "unknown-b"}
	for i, wantID := range want {
		if got := pickSoonestReset(t, selector, "claude", auths); got != wantID {
			t.Fatalf("pick #%d = %q, want %q", i, got, wantID)
		}
	}

	// Once every credential has fresh data, the known ranking applies.
	known := []*Auth{auths[0], claudeQuotaAuth("unknown-b", "0.1", 24*time.Hour)}
	if got := pickSoonestReset(t, selector, "claude", known); got != "known" {
		t.Fatalf("pick with full data = %q, want known", got)
	}
}

func TestSoonestResetSelectorTieBreaks(t *testing.T) {
	selector := newSoonestResetTestSelector()
	// Resets within the tolerance tie; lower remaining quota wins.
	auths := []*Auth{
		claudeQuotaAuth("a-more-left", "0.2", 5*time.Hour),
		claudeQuotaAuth("b-less-left", "0.8", 5*time.Hour+30*time.Second),
		claudeQuotaAuth("c-later", "0.95", 6*time.Hour),
	}
	if got := pickSoonestReset(t, selector, "claude", auths); got != "b-less-left" {
		t.Fatalf("pick = %q, want b-less-left", got)
	}

	// Equal reset and remaining fall back to the auth ID.
	auths = []*Auth{
		claudeQuotaAuth("z-auth", "0.5", 5*time.Hour),
		claudeQuotaAuth("m-auth", "0.5", 5*time.Hour+10*time.Second),
	}
	for i := 0; i < 3; i++ {
		if got := pickSoonestReset(t, selector, "claude", auths); got != "m-auth" {
			t.Fatalf("pick #%d = %q, want m-auth", i, got)
		}
	}

	// Outside the tolerance the earlier reset wins regardless of remaining quota.
	auths = []*Auth{
		claudeQuotaAuth("a-earlier", "0.1", 5*time.Hour),
		claudeQuotaAuth("b-later", "0.9", 5*time.Hour+2*time.Minute),
	}
	if got := pickSoonestReset(t, selector, "claude", auths); got != "a-earlier" {
		t.Fatalf("pick = %q, want a-earlier", got)
	}
}

func TestSoonestResetSelectorRanksExhaustedLast(t *testing.T) {
	selector := newSoonestResetTestSelector()
	exhaustedSoon := claudeQuotaAuth("a-exhausted-soon", "1.0", time.Hour)
	exhaustedLater := claudeQuotaAuth("b-exhausted-later", "0.3", 2*time.Hour)
	exhaustedLater.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Status"] = "rejected"
	available := claudeQuotaAuth("c-available", "0.1", 96*time.Hour)

	if got := pickSoonestReset(t, selector, "claude", []*Auth{exhaustedSoon, exhaustedLater, available}); got != "c-available" {
		t.Fatalf("pick = %q, want c-available", got)
	}
	if got := pickSoonestReset(t, selector, "claude", []*Auth{exhaustedLater, exhaustedSoon}); got != "a-exhausted-soon" {
		t.Fatalf("pick among exhausted = %q, want a-exhausted-soon", got)
	}
}

func TestSoonestResetSelectorWithoutSignalsBehavesRoundRobin(t *testing.T) {
	selector := newSoonestResetTestSelector()
	auths := []*Auth{{ID: "gem-c"}, {ID: "gem-a"}, {ID: "gem-b"}}
	want := []string{"gem-a", "gem-b", "gem-c", "gem-a", "gem-b", "gem-c"}
	for i, wantID := range want {
		if got := pickSoonestReset(t, selector, "gemini", auths); got != wantID {
			t.Fatalf("pick #%d = %q, want %q", i, got, wantID)
		}
	}
}

func claudeAPIKeyAuth(id string) *Auth {
	return &Auth{ID: id, Provider: "claude", Attributes: map[string]string{AttributeAPIKey: "sk-" + id}}
}

func TestSoonestResetSelectorSpendsSubscriptionBeforeAPIKeys(t *testing.T) {
	selector := newSoonestResetTestSelector()
	apiKey := claudeAPIKeyAuth("a-api-key")
	// Even a weekly-looking snapshot does not move an API key ahead of subscription quota.
	apiKey.Quota = claudeQuotaAuth("ignored", "0.9", time.Minute).Quota
	oauthKnown := claudeQuotaAuth("b-oauth-known", "0.0", 96*time.Hour)
	oauthUnknown := &Auth{ID: "c-oauth-unknown", Provider: "claude", Metadata: map[string]any{"email": "user@example.com"}}
	exhausted := claudeQuotaAuth("d-oauth-exhausted", "1.0", time.Hour)

	if got := pickSoonestReset(t, selector, "claude", []*Auth{apiKey, oauthKnown, oauthUnknown, exhausted}); got != "c-oauth-unknown" {
		t.Fatalf("pick = %q, want c-oauth-unknown", got)
	}
	if got := pickSoonestReset(t, selector, "claude", []*Auth{apiKey, oauthKnown, exhausted}); got != "b-oauth-known" {
		t.Fatalf("pick = %q, want b-oauth-known", got)
	}
	if got := pickSoonestReset(t, selector, "claude", []*Auth{apiKey, exhausted}); got != "a-api-key" {
		t.Fatalf("pick = %q, want a-api-key ahead of exhausted subscription", got)
	}

	// Non-reporting credentials rotate among themselves once subscription quota is gone.
	secondKey := claudeAPIKeyAuth("e-api-key")
	want := []string{"e-api-key", "a-api-key", "e-api-key"}
	for i, wantID := range want {
		if got := pickSoonestReset(t, selector, "claude", []*Auth{apiKey, secondKey, exhausted}); got != wantID {
			t.Fatalf("pick #%d = %q, want %q", i, got, wantID)
		}
	}
}

func TestSoonestResetSelectorAPIKeyOnlyPoolBehavesRoundRobin(t *testing.T) {
	selector := newSoonestResetTestSelector()
	auths := []*Auth{claudeAPIKeyAuth("key-c"), claudeAPIKeyAuth("key-a"), claudeAPIKeyAuth("key-b")}
	auths[1].Quota = claudeQuotaAuth("ignored", "0.5", time.Hour).Quota
	want := []string{"key-a", "key-b", "key-c", "key-a", "key-b", "key-c"}
	for i, wantID := range want {
		if got := pickSoonestReset(t, selector, "claude", auths); got != wantID {
			t.Fatalf("pick #%d = %q, want %q", i, got, wantID)
		}
	}
}

func TestReportsWeeklyQuota(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want bool
	}{
		{name: "claude oauth", auth: &Auth{Provider: "claude", Metadata: map[string]any{"email": "a@b.c"}}, want: true},
		{name: "codex file", auth: &Auth{Provider: "codex"}, want: true},
		{name: "claude api key", auth: claudeAPIKeyAuth("k"), want: false},
		{name: "codex explicit api key kind", auth: &Auth{Provider: "codex", Attributes: map[string]string{AttributeAuthKind: AuthKindAPIKey}}, want: false},
		{name: "gemini oauth", auth: &Auth{Provider: "gemini-cli", Metadata: map[string]any{"email": "a@b.c"}}, want: false},
		{name: "nil", auth: nil, want: false},
	}
	for _, tt := range tests {
		if got := reportsWeeklyQuota(tt.auth); got != tt.want {
			t.Fatalf("%s: reportsWeeklyQuota() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSoonestResetSelectorSkipsBlockedAndKeepsPriorityTier(t *testing.T) {
	selector := newSoonestResetTestSelector()
	cooling := claudeQuotaAuth("a-cooling", "0.1", time.Hour)
	cooling.Quota.Exceeded = true
	cooling.Quota.Reason = "credential_quota"
	cooling.Quota.NextRecoverAt = soonestResetTestNow.Add(time.Hour)
	lowPriority := claudeQuotaAuth("b-low-priority", "0.1", 2*time.Hour)
	lowPriority.Attributes = map[string]string{"priority": "0"}
	highPriority := claudeQuotaAuth("c-high-priority", "0.1", 96*time.Hour)
	highPriority.Attributes = map[string]string{"priority": "10"}

	if got := pickSoonestReset(t, selector, "claude", []*Auth{cooling, lowPriority, highPriority}); got != "c-high-priority" {
		t.Fatalf("pick = %q, want c-high-priority", got)
	}
}

func TestSoonestResetSelectorSessionAffinityKeepsBoundCredential(t *testing.T) {
	fallback := newSoonestResetTestSelector()
	selector := NewSessionAffinitySelector(fallback)
	defer selector.Stop()

	payload := []byte(`{"metadata":{"user_id":"user_xxx_account__session_ac980658-63bd-4fb3-97ba-8da64cb1e344"}}`)
	opts := cliproxyexecutor.Options{OriginalRequest: payload}
	pick := func(auths []*Auth) string {
		t.Helper()
		picked, errPick := selector.Pick(context.Background(), "claude", "claude-sonnet-4-5", opts, auths)
		if errPick != nil {
			t.Fatalf("Pick() error = %v", errPick)
		}
		return picked.ID
	}

	first := []*Auth{
		claudeQuotaAuth("a-soon", "0.5", 2*time.Hour),
		claudeQuotaAuth("b-later", "0.0", 72*time.Hour),
	}
	if got := pick(first); got != "a-soon" {
		t.Fatalf("cold bind = %q, want a-soon", got)
	}

	// b-later now resets sooner, but the bound session keeps its credential.
	second := []*Auth{
		claudeQuotaAuth("a-soon", "0.6", 72*time.Hour),
		claudeQuotaAuth("b-later", "0.0", time.Hour),
	}
	if got := pick(second); got != "a-soon" {
		t.Fatalf("bound session pick = %q, want a-soon", got)
	}

	// A new session uses the strategy for its cold bind.
	otherOpts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_0f2c7a51-5b0e-4b8e-9d55-8c1c3f8a9b21"}}`)}
	picked, errPick := selector.Pick(context.Background(), "claude", "claude-sonnet-4-5", otherOpts, second)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if picked.ID != "b-later" {
		t.Fatalf("new session cold bind = %q, want b-later", picked.ID)
	}
}

func TestManagerSoonestResetTrustsAliasAwareAvailability(t *testing.T) {
	const routeModel, targetModel = "soonest-route", "soonest-target"
	manager := NewManager(nil, &SoonestResetSelector{}, nil)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "claude"})
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: targetModel, Alias: routeModel, Fork: true}},
	})
	now := time.Now()
	weekly := func(resetIn time.Duration) QuotaState {
		return QuotaState{ObservedAt: now, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.5",
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(resetIn).Unix(), 10),
		}}
	}
	soonID, laterID := "a-soon-"+t.Name(), "b-later-"+t.Name()
	soon := &Auth{ID: soonID, Provider: "claude", Status: StatusActive, Quota: weekly(2 * time.Hour),
		ModelStates: map[string]*ModelState{
			// A stale state keyed by the route model must not hide the credential: requests
			// are served by the alias target, which the manager already validated.
			routeModel: {Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour)},
		},
	}
	later := &Auth{ID: laterID, Provider: "claude", Status: StatusActive, Quota: weekly(72 * time.Hour)}
	for _, candidate := range []*Auth{soon, later} {
		registry.GetGlobalRegistry().RegisterClient(candidate.ID, "claude", []*registry.ModelInfo{{ID: routeModel}, {ID: targetModel}})
		candidateID := candidate.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidateID) })
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatal(errRegister)
		}
	}

	selected, errSelect := manager.SelectAuth(context.Background(), "claude", routeModel, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectAuth() error = %v", errSelect)
	}
	if selected.ID != soonID {
		t.Fatalf("SelectAuth() = %q, want %q", selected.ID, soonID)
	}
}
