package auth

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	// soonestResetTieTolerance treats weekly resets this close together as simultaneous.
	soonestResetTieTolerance = time.Minute
	// minWeeklyQuotaWindowMinutes is the shortest Codex window accepted as the weekly window.
	minWeeklyQuotaWindowMinutes = 24 * 60
)

// SoonestResetSelector prefers the credential whose weekly quota window resets soonest
// ("use it or lose it"), based on the passive quota signals observed from upstream
// responses (Claude unified 7d headers, Codex primary/secondary windows).
//
// Ranking within the candidate tier:
//  1. Credentials without usable weekly data (no signals yet, unparsable, or a reset that
//     already passed) come first and rotate round-robin, so their reset time gets learned.
//  2. Credentials with remaining weekly quota, by reset time ascending. Resets within
//     soonestResetTieTolerance tie; ties prefer lower remaining quota, then auth ID.
//  3. Credentials whose weekly quota is exhausted, by reset time ascending, then auth ID.
//
// Providers that never report quota signals therefore behave like round-robin.
type SoonestResetSelector struct {
	mu          sync.Mutex
	lastUnknown map[string]string
	maxKeys     int
	// nowFunc overrides the clock for tests.
	nowFunc func() time.Time
}

// weeklyQuotaWindow is the parsed weekly quota state of one credential.
type weeklyQuotaWindow struct {
	ResetAt time.Time
	// Remaining is the fraction of the weekly quota left, clamped to [0, 1].
	Remaining float64
	Exhausted bool
}

// Pick selects the credential whose weekly quota resets soonest.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := s.now()
	available, errAvailable := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if errAvailable != nil {
		return nil, errAvailable
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	var unknown []*Auth
	var withQuota, exhausted []soonestResetCandidate
	for _, candidate := range available {
		if candidate == nil {
			continue
		}
		window, ok := weeklyQuotaWindowForAuth(candidate, model, now)
		switch {
		case !ok:
			unknown = append(unknown, candidate)
		case window.Exhausted:
			exhausted = append(exhausted, soonestResetCandidate{auth: candidate, window: window})
		default:
			withQuota = append(withQuota, soonestResetCandidate{auth: candidate, window: window})
		}
	}

	if len(unknown) > 0 {
		return s.rotateUnknown(provider, model, unknown), nil
	}
	if picked := pickSoonestWithQuota(withQuota); picked != nil {
		return picked, nil
	}
	if picked := pickSoonestExhausted(exhausted); picked != nil {
		return picked, nil
	}
	return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
}

type soonestResetCandidate struct {
	auth   *Auth
	window weeklyQuotaWindow
}

func (s *SoonestResetSelector) now() time.Time {
	if s != nil && s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// rotateUnknown round-robins across credentials without weekly data, resuming after the
// previously picked one so repeated picks spread across them.
func (s *SoonestResetSelector) rotateUnknown(provider, model string, unknown []*Auth) *Auth {
	sorted := make([]*Auth, len(unknown))
	copy(sorted, unknown)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}
	if s.lastUnknown == nil {
		s.lastUnknown = make(map[string]string)
	}
	if _, ok := s.lastUnknown[key]; !ok && len(s.lastUnknown) >= limit {
		s.lastUnknown = make(map[string]string)
	}
	picked := sorted[successorIndex(sorted, s.lastUnknown[key])]
	s.lastUnknown[key] = picked.ID
	return picked
}

// pickSoonestWithQuota returns the credential with the earliest weekly reset. Resets within
// soonestResetTieTolerance of the earliest one tie and prefer lower remaining quota, then ID.
func pickSoonestWithQuota(candidates []soonestResetCandidate) *Auth {
	if len(candidates) == 0 {
		return nil
	}
	earliest := candidates[0].window.ResetAt
	for _, candidate := range candidates[1:] {
		if candidate.window.ResetAt.Before(earliest) {
			earliest = candidate.window.ResetAt
		}
	}
	var best *soonestResetCandidate
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.window.ResetAt.Sub(earliest) > soonestResetTieTolerance {
			continue
		}
		if best == nil ||
			candidate.window.Remaining < best.window.Remaining ||
			(candidate.window.Remaining == best.window.Remaining && candidate.auth.ID < best.auth.ID) {
			best = candidate
		}
	}
	return best.auth
}

// pickSoonestExhausted returns the exhausted credential that recovers first, then by ID.
func pickSoonestExhausted(candidates []soonestResetCandidate) *Auth {
	var best *soonestResetCandidate
	for i := range candidates {
		candidate := &candidates[i]
		if best == nil ||
			candidate.window.ResetAt.Before(best.window.ResetAt) ||
			(candidate.window.ResetAt.Equal(best.window.ResetAt) && candidate.auth.ID < best.auth.ID) {
			best = candidate
		}
	}
	if best == nil {
		return nil
	}
	return best.auth
}

// weeklyQuotaWindowForAuth parses the newest quota snapshot of the credential and the
// requested model. It reports false when no usable, still current weekly window is known.
func weeklyQuotaWindowForAuth(auth *Auth, model string, now time.Time) (weeklyQuotaWindow, bool) {
	if auth == nil {
		return weeklyQuotaWindow{}, false
	}
	quota := auth.Quota
	if modelKey := canonicalModelKey(model); modelKey != "" {
		for stateModel, state := range auth.ModelStates {
			if state == nil || canonicalModelKey(stateModel) != modelKey {
				continue
			}
			if state.Quota.ObservedAt.After(quota.ObservedAt) {
				quota = state.Quota
			}
		}
	}
	window, ok := parseWeeklyQuotaWindow(quota.Signals, quota.ObservedAt)
	if !ok || !window.ResetAt.After(now) {
		// A reset in the past means the window rolled over and the snapshot is stale.
		return weeklyQuotaWindow{}, false
	}
	return window, true
}

// parseWeeklyQuotaWindow extracts the weekly window from Claude or Codex quota signals.
func parseWeeklyQuotaWindow(signals map[string]string, observedAt time.Time) (weeklyQuotaWindow, bool) {
	if len(signals) == 0 {
		return weeklyQuotaWindow{}, false
	}
	if window, ok := parseClaudeWeeklyQuotaWindow(signals); ok {
		return window, true
	}
	return parseCodexWeeklyQuotaWindow(signals, observedAt)
}

// parseClaudeWeeklyQuotaWindow reads the Anthropic unified 7d window. Utilization is a
// fraction in [0, 1]; the reset is an absolute timestamp.
func parseClaudeWeeklyQuotaWindow(signals map[string]string) (weeklyQuotaWindow, bool) {
	resetAt, ok := parseQuotaResetTimestamp(quotaSignal(signals, "Anthropic-Ratelimit-Unified-7d-Reset"))
	if !ok {
		return weeklyQuotaWindow{}, false
	}
	window := weeklyQuotaWindow{ResetAt: resetAt}
	rejected := strings.EqualFold(quotaSignal(signals, "Anthropic-Ratelimit-Unified-7d-Status"), "rejected")
	utilization, okUtilization := parseQuotaFloat(quotaSignal(signals, "Anthropic-Ratelimit-Unified-7d-Utilization"))
	switch {
	case okUtilization:
		window.Remaining = clampQuotaFraction(1 - utilization)
	case !rejected:
		return weeklyQuotaWindow{}, false
	}
	window.Exhausted = rejected || window.Remaining <= 0
	if window.Exhausted {
		window.Remaining = 0
	}
	return window, true
}

// parseCodexWeeklyQuotaWindow reads the longest Codex primary/secondary window, accepted
// only when it spans at least a day. Used percent is in [0, 100]; Reset-At is absolute
// and preferred, Reset-After-Seconds is relative to the observation time.
func parseCodexWeeklyQuotaWindow(signals map[string]string, observedAt time.Time) (weeklyQuotaWindow, bool) {
	bestMinutes := 0.0
	bestPrefix := ""
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		minutes, ok := parseQuotaFloat(quotaSignal(signals, prefix+"Window-Minutes"))
		if ok && minutes > bestMinutes {
			bestMinutes = minutes
			bestPrefix = prefix
		}
	}
	if bestPrefix == "" || bestMinutes < minWeeklyQuotaWindowMinutes {
		return weeklyQuotaWindow{}, false
	}
	usedPercent, ok := parseQuotaFloat(quotaSignal(signals, bestPrefix+"Used-Percent"))
	if !ok {
		return weeklyQuotaWindow{}, false
	}
	resetAt, ok := parseQuotaResetTimestamp(quotaSignal(signals, bestPrefix+"Reset-At"))
	if !ok {
		resetAfter, okAfter := parseQuotaFloat(quotaSignal(signals, bestPrefix+"Reset-After-Seconds"))
		if !okAfter || resetAfter < 0 || observedAt.IsZero() {
			return weeklyQuotaWindow{}, false
		}
		resetAt = observedAt.Add(time.Duration(resetAfter * float64(time.Second)))
	}
	window := weeklyQuotaWindow{
		ResetAt:   resetAt,
		Remaining: clampQuotaFraction(1 - usedPercent/100),
	}
	window.Exhausted = window.Remaining <= 0
	return window, true
}

func quotaSignal(signals map[string]string, name string) string {
	return strings.TrimSpace(signals[http.CanonicalHeaderKey(name)])
}

func parseQuotaFloat(raw string) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// parseQuotaResetTimestamp accepts unix seconds (optionally fractional), RFC3339, or an
// HTTP date, mirroring the formats accepted for Claude rate-limit resets.
func parseQuotaResetTimestamp(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, ok := parseQuotaFloat(raw); ok {
		if seconds <= 0 {
			return time.Time{}, false
		}
		whole := math.Floor(seconds)
		return time.Unix(int64(whole), int64((seconds-whole)*1e9)), true
	}
	if parsed, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
		return parsed, true
	}
	if parsed, errParse := http.ParseTime(raw); errParse == nil {
		return parsed, true
	}
	return time.Time{}, false
}

func clampQuotaFraction(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}
