package auth

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

const (
	// DefaultSessionAffinityMaxRetries is the default number of additional attempts on the
	// bound credential of an established session after a transient upstream failure.
	DefaultSessionAffinityMaxRetries = 2
	// MaxSessionAffinityMaxRetries caps the sticky retry budget to bound request latency.
	MaxSessionAffinityMaxRetries = 10

	// sessionAffinityEstablishedAuthMetadataKey records the credential of a binding that
	// existed before the current pick. Fresh bindings never set it, so a cold session
	// keeps the regular eager failover because it has no prompt cache to protect yet.
	sessionAffinityEstablishedAuthMetadataKey = "session_affinity_established_auth"

	sessionStickyRetryBaseDelay = 500 * time.Millisecond
	sessionStickyRetryMaxDelay  = 4 * time.Second
)

// NormalizeSessionAffinityMaxRetries resolves the configured sticky retry budget.
// Nil selects the default; negative values disable sticky retries.
func NormalizeSessionAffinityMaxRetries(value *int) int {
	if value == nil {
		return DefaultSessionAffinityMaxRetries
	}
	retries := *value
	if retries < 0 {
		return 0
	}
	if retries > MaxSessionAffinityMaxRetries {
		return MaxSessionAffinityMaxRetries
	}
	return retries
}

// sessionStickyRetryDelay returns the exponential backoff before sticky attempt n (1-based).
func sessionStickyRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := sessionStickyRetryBaseDelay
	for i := 1; i < attempt && delay < sessionStickyRetryMaxDelay; i++ {
		delay *= 2
	}
	if delay > sessionStickyRetryMaxDelay {
		delay = sessionStickyRetryMaxDelay
	}
	return delay
}

// waitSessionStickyRetry waits for delay unless the request context ends first.
func waitSessionStickyRetry(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isSessionStickyTransientStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		520, 521, 522, 523, 524, 525, 526,
		529:
		return true
	default:
		return false
	}
}

// isSessionStickyRetryableError reports transient upstream failures that do not prove the
// bound credential is unable to serve the session.
func isSessionStickyRetryableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if isRequestInvalidError(err) || isCredentialScopedError(err) || isModelSupportError(err) ||
		isCloudflareChallengeError(err) || isInvalidGrantError(err) {
		return false
	}
	if status := statusCodeFromError(err); status != 0 {
		return isSessionStickyTransientStatus(status)
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && authErr.Code == "empty_stream" {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || isTransientTransportError(err)
}

// isSessionStickyTransientResultError is the recorded-result counterpart of
// isSessionStickyRetryableError used when a result no longer carries the original error.
func isSessionStickyTransientResultError(err *Error) bool {
	if err == nil || err.Code == ErrorCodeForceCooldown || err.Code == ErrorCodeRequestScoped {
		return false
	}
	if isModelSupportResultError(err) || isCloudflareChallengeResultError(err) || isInvalidGrantResultError(err) {
		return false
	}
	if status := statusCodeFromResult(err); status != 0 {
		return isSessionStickyTransientStatus(status)
	}
	switch err.Code {
	case transientTransportErrorCode, connectionLifecycleErrorCode, "empty_stream":
		return true
	}
	return isTransientTransportResultError(err)
}

// sessionStickyRetry tracks same-credential retries for one request whose session was
// already bound to the selected credential.
type sessionStickyRetry struct {
	selector *SessionAffinitySelector
	authID   string
	attempts int
}

// sessionStickyRetryFor returns a retry budget when the selected auth is the established
// binding of the request's session, or nil when sticky retries do not apply.
func (m *Manager) sessionStickyRetryFor(metadata map[string]any, auth *Auth) *sessionStickyRetry {
	if m == nil || auth == nil || auth.ID == "" || m.HomeEnabled() {
		return nil
	}
	affinity, ok := m.Selector().(*SessionAffinitySelector)
	if !ok || affinity == nil || affinity.maxRetries <= 0 {
		return nil
	}
	bound, _ := metadata[sessionAffinityEstablishedAuthMetadataKey].(string)
	if bound == "" || bound != auth.ID {
		return nil
	}
	return &sessionStickyRetry{selector: affinity, authID: auth.ID}
}

// shouldRetrySessionSticky reports whether a failed attempt on the bound credential must be
// retried on that credential. It consumes one unit of the budget when it returns true.
// User-configured request-scoped error rules always take precedence.
func (m *Manager) shouldRetrySessionSticky(sticky *sessionStickyRetry, auth *Auth, err error) bool {
	if sticky == nil || auth == nil || auth.ID != sticky.authID {
		return false
	}
	if sticky.attempts >= sticky.selector.maxRetries || !isSessionStickyRetryableError(err) {
		return false
	}
	if _, okAction := matchRequestScopedErrorAction(auth, err, m.runtimeConfigSnapshot()); okAction {
		return false
	}
	sticky.attempts++
	return true
}

// awaitSessionStickyRetry records the transient failure without cooling the credential or
// releasing the binding, then waits for the backoff before the same-credential retry.
func (m *Manager) awaitSessionStickyRetry(ctx context.Context, sticky *sessionStickyRetry, result Result, err error) error {
	result.Success = false
	if result.Error == nil {
		result.Error = resultErrorFromError(err)
	}
	m.recordAvailabilityNeutralResult(ctx, result)
	delay := sessionStickyRetryDelay(sticky.attempts)
	logEntryWithRequestID(ctx).Infof("session-affinity: transient failure on bound auth, retrying same credential | auth=%s attempt=%d/%d status=%d backoff=%s",
		sticky.authID, sticky.attempts, sticky.selector.maxRetries, statusCodeFromError(err), delay)
	wait := sticky.selector.retryWait
	if wait == nil {
		wait = waitSessionStickyRetry
	}
	return wait(ctx, delay)
}

// peekExplicitBoundAuthID returns the credential currently bound to the request's explicit
// harness session without refreshing, creating, or moving any binding.
func (s *SessionAffinitySelector) peekExplicitBoundAuthID(provider, model string, opts cliproxyexecutor.Options) string {
	if s == nil || s.cache == nil || s.maxRetries <= 0 {
		return ""
	}
	// Session extraction may annotate metadata; work on a copy to stay side-effect free.
	metadata := maps.Clone(opts.Metadata)
	explicitID, _ := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, metadata)
	if explicitID == "" {
		return ""
	}
	cacheKey := provider + "::" + cliproxysession.BoundSessionIdentity(explicitID) + "::" + canonicalModelKey(model)
	authID, _ := s.cache.Get(cacheKey)
	return authID
}

// isAuthOnlyTransientlyCooledForModel reports whether auth is unavailable for model solely
// because of a transient-error cooldown. Quota, authorization, disabled, and permanent
// blocks are never considered transient.
func isAuthOnlyTransientlyCooledForModel(auth *Auth, model string, now time.Time) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
		return false
	}
	if exp, ok := auth.AccessTokenExpirationTime(); ok && !exp.IsZero() && !exp.After(now) {
		return false
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		return false
	}
	blocked, reason, next := isAuthBlockedForModel(auth, model, now)
	if !blocked || reason != blockReasonOther || !next.After(now) {
		return false
	}
	if model != "" && len(auth.ModelStates) > 0 {
		modelKey := canonicalModelKey(model)
		for stateModel, state := range auth.ModelStates {
			if state == nil || canonicalModelKey(stateModel) != modelKey {
				continue
			}
			stateBlocked, _, _ := availabilityBlock(state.Unavailable, state.Quota.Exceeded, state.NextRetryAfter, state.Quota.NextRecoverAt, now)
			if !stateBlocked {
				continue
			}
			if state.Status == StatusDisabled || state.Quota.Exceeded || !isSessionStickyTransientResultError(state.LastError) {
				return false
			}
		}
		return true
	}
	return !auth.Quota.Exceeded && isSessionStickyTransientResultError(auth.LastError)
}

// includeSessionBoundTransientAuthLocked keeps an established session on its credential
// while that credential only sits in a transient-error cooldown, which may have been caused
// by unrelated traffic. The credential is appended to the selector candidates so the
// affinity cache hit can reuse it; it never becomes a candidate for other sessions because
// only the session bound to it can resolve it from the cache. Callers must hold m.mu.
func (m *Manager) includeSessionBoundTransientAuthLocked(selector Selector, candidates, available, selectorAuths []*Auth, errAvailable error, provider, routeModel string, opts cliproxyexecutor.Options, now time.Time) ([]*Auth, []*Auth, error) {
	affinity, ok := selector.(*SessionAffinitySelector)
	if !ok || affinity == nil || affinity.maxRetries <= 0 {
		return available, selectorAuths, errAvailable
	}
	if errAvailable == nil && len(selectorAuths) >= len(candidates) {
		// Every candidate is available; nothing can be hidden by a cooldown.
		return available, selectorAuths, errAvailable
	}
	selectable := make(map[string]struct{}, len(selectorAuths))
	for _, candidate := range selectorAuths {
		if candidate != nil {
			selectable[candidate.ID] = struct{}{}
		}
	}
	// Collect transiently cooled candidates first so session extraction, which parses the
	// request payload, only runs when a binding could actually be affected.
	var cooled []*Auth
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if _, ok := selectable[candidate.ID]; ok {
			continue
		}
		if isAuthOnlyTransientlyCooledForModel(candidate, m.selectionModelForAuth(candidate, routeModel), now) {
			cooled = append(cooled, candidate)
		}
	}
	if len(cooled) == 0 {
		return available, selectorAuths, errAvailable
	}
	boundID := affinity.peekExplicitBoundAuthID(provider, selectionArgForSelector(selector, routeModel), opts)
	if boundID == "" {
		return available, selectorAuths, errAvailable
	}
	for _, candidate := range cooled {
		if candidate.ID != boundID {
			continue
		}
		bound := candidate.Clone()
		extended := make([]*Auth, 0, len(selectorAuths)+1)
		extended = append(extended, selectorAuths...)
		extended = append(extended, bound)
		if errAvailable != nil {
			available = []*Auth{bound}
		}
		return available, extended, nil
	}
	return available, selectorAuths, errAvailable
}
