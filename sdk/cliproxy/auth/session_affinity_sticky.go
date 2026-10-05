package auth

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"sync"
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
	// sessionAffinityReadOnlyMetadataKey marks auxiliary requests (count_tokens) that may
	// reuse a session binding but must never create, move, or release it.
	sessionAffinityReadOnlyMetadataKey = "session_affinity_read_only"

	sessionStickyRetryBaseDelay = 500 * time.Millisecond
	sessionStickyRetryMaxDelay  = 4 * time.Second
	// sessionStickyRateLimitMaxWait is the longest model-level rate limit a bound session
	// waits out on its credential instead of moving to another account.
	sessionStickyRateLimitMaxWait = 30 * time.Second
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

// sessionStickyRetryWait reports whether a failure on the bound credential should be retried
// on that credential, plus the minimum wait the upstream asked for. Transient failures and
// short model-level rate limits do not prove the bound credential is unable to serve the
// session; credential-scoped limits (5h/7d windows, usage limits) always fail over.
func sessionStickyRetryWait(err error) (time.Duration, bool) {
	if err == nil || errors.Is(err, context.Canceled) {
		return 0, false
	}
	if isRequestInvalidError(err) || isCredentialScopedError(err) || isModelSupportError(err) ||
		isCloudflareChallengeError(err) || isInvalidGrantError(err) {
		return 0, false
	}
	if status := statusCodeFromError(err); status != 0 {
		if status == http.StatusTooManyRequests {
			return sessionStickyRateLimitWait(err)
		}
		return 0, isSessionStickyTransientStatus(status)
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && authErr.Code == "empty_stream" {
		return 0, true
	}
	return 0, errors.Is(err, context.DeadlineExceeded) || isTransientTransportError(err)
}

// sessionStickyRateLimitWait accepts a model-level 429 whose retry hint, when present, fits
// within sessionStickyRateLimitMaxWait.
func sessionStickyRateLimitWait(err error) (time.Duration, bool) {
	retryAfter := retryAfterFromError(err)
	if retryAfter == nil || *retryAfter <= 0 {
		return 0, true
	}
	if *retryAfter > sessionStickyRateLimitMaxWait {
		return 0, false
	}
	return *retryAfter, true
}

// isSessionStickyTransientResultError is the recorded-result counterpart of
// sessionStickyRetryWait used when a result no longer carries the original error.
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

type sessionStickyBudgetKey struct{}

// sessionStickyBudget counts sticky retries per credential across every request-retry round
// of one client request, so the configured budget bounds the whole request.
type sessionStickyBudget struct {
	mu       sync.Mutex
	attempts map[string]int
}

// withSessionStickyBudget attaches a request-scoped sticky budget unless ctx already has one.
func withSessionStickyBudget(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(sessionStickyBudgetKey{}).(*sessionStickyBudget); ok {
		return ctx
	}
	return context.WithValue(ctx, sessionStickyBudgetKey{}, &sessionStickyBudget{attempts: make(map[string]int)})
}

// sessionStickyBudgetFrom returns the request budget, or a fresh one for callers that did
// not attach it.
func sessionStickyBudgetFrom(ctx context.Context) *sessionStickyBudget {
	if ctx != nil {
		if budget, ok := ctx.Value(sessionStickyBudgetKey{}).(*sessionStickyBudget); ok && budget != nil {
			return budget
		}
	}
	return &sessionStickyBudget{attempts: make(map[string]int)}
}

// consume takes one retry for authID and returns its 1-based attempt number, or false when
// the budget is spent.
func (b *sessionStickyBudget) consume(authID string, limit int) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.attempts[authID] >= limit {
		return b.attempts[authID], false
	}
	b.attempts[authID]++
	return b.attempts[authID], true
}

// sessionStickyRetry tracks same-credential retries for one request whose session was
// already bound to the selected credential.
type sessionStickyRetry struct {
	selector *SessionAffinitySelector
	authID   string
	budget   *sessionStickyBudget
	attempts int
	delay    time.Duration
}

// sessionStickyApplies reports whether auth is the established binding of the request's
// session and sticky retries are enabled.
func (m *Manager) sessionStickyApplies(metadata map[string]any, auth *Auth) (*SessionAffinitySelector, bool) {
	if m == nil || auth == nil || auth.ID == "" || m.HomeEnabled() {
		return nil, false
	}
	affinity, ok := m.Selector().(*SessionAffinitySelector)
	if !ok || affinity == nil || affinity.maxRetries <= 0 {
		return nil, false
	}
	bound, _ := metadata[sessionAffinityEstablishedAuthMetadataKey].(string)
	if bound == "" || bound != auth.ID {
		return nil, false
	}
	return affinity, true
}

// sessionStickyRetryFor returns the sticky retry state when the selected auth is the
// established binding of the request's session, or nil when sticky retries do not apply.
func (m *Manager) sessionStickyRetryFor(ctx context.Context, metadata map[string]any, auth *Auth) *sessionStickyRetry {
	affinity, ok := m.sessionStickyApplies(metadata, auth)
	if !ok {
		return nil
	}
	return &sessionStickyRetry{selector: affinity, authID: auth.ID, budget: sessionStickyBudgetFrom(ctx)}
}

// shouldRetrySessionSticky reports whether a failed attempt on the bound credential must be
// retried on that credential. It consumes one unit of the budget when it returns true.
// User-configured request-scoped error rules always take precedence. A rate-limited model
// is not waited out while the bound credential still has another pool model to try.
func (m *Manager) shouldRetrySessionSticky(sticky *sessionStickyRetry, auth *Auth, err error, hasAlternativeModel bool) bool {
	if sticky == nil || auth == nil || auth.ID != sticky.authID {
		return false
	}
	minWait, retryable := sessionStickyRetryWait(err)
	if !retryable || (hasAlternativeModel && statusCodeFromError(err) == http.StatusTooManyRequests) {
		return false
	}
	if _, okAction := matchRequestScopedErrorAction(auth, err, m.runtimeConfigSnapshot()); okAction {
		return false
	}
	attempt, ok := sticky.budget.consume(sticky.authID, sticky.selector.maxRetries)
	if !ok {
		return false
	}
	sticky.attempts = attempt
	sticky.delay = max(sessionStickyRetryDelay(attempt), minWait)
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
	delay := sticky.delay
	logEntryWithRequestID(ctx).Infof("session-affinity: transient failure on bound auth, retrying same credential | auth=%s attempt=%d/%d status=%d backoff=%s",
		sticky.authID, sticky.attempts, sticky.selector.maxRetries, statusCodeFromError(err), delay)
	wait := sticky.selector.retryWait
	if wait == nil {
		wait = waitSessionStickyRetry
	}
	return wait(ctx, delay)
}

// withSessionAffinityReadOnly marks opts so session affinity reuses an existing binding
// without creating, moving, or releasing it.
func withSessionAffinityReadOnly(opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	meta := cloneRequestMetadata(opts.Metadata)
	meta[sessionAffinityReadOnlyMetadataKey] = true
	opts.Metadata = meta
	return opts
}

func isSessionAffinityReadOnly(metadata map[string]any) bool {
	readOnly, _ := metadata[sessionAffinityReadOnlyMetadataKey].(bool)
	return readOnly
}

// pickReadOnly serves auxiliary requests that share a generation thread's session: it reuses
// the thread's bound credential when available but never touches the binding, so their
// failures and failovers cannot evict the thread from its account.
func (s *SessionAffinitySelector) pickReadOnly(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	availabilityCandidates := auths
	if _, weighted := s.fallback.(*WeightedRoundRobinSelector); weighted {
		availabilityCandidates = positiveWeightAuths(auths)
	}
	available, errAvailable := getSelectorAvailableAuthsAcrossPriorities(ctx, availabilityCandidates, provider, model, time.Now())
	if errAvailable != nil {
		return nil, errAvailable
	}
	if sessionID := ExtractSessionID(opts.Headers, opts.OriginalRequest, maps.Clone(opts.Metadata)); sessionID != "" && s.cache != nil {
		sessionID = cliproxysession.BoundSessionIdentity(sessionID)
		if opts.Metadata != nil {
			opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] = sessionID
		}
		cacheKey := provider + "::" + sessionID + "::" + canonicalModelKey(model)
		if boundID, ok := s.cache.Get(cacheKey); ok {
			for _, auth := range available {
				if auth.ID == boundID {
					return auth, nil
				}
			}
		}
	}
	return s.fallback.Pick(ctx, provider, model, opts, highestPriorityAuths(available))
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
// because of a transient-error cooldown or a model-level rate limit that ends within
// sessionStickyRateLimitMaxWait. Credential quota, authorization, disabled, and permanent
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
	if !blocked || !next.After(now) {
		return false
	}
	if reason == blockReasonCooldown {
		return next.Sub(now) <= sessionStickyRateLimitMaxWait
	}
	if reason != blockReasonOther {
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
// while that credential only sits in a transient-error or short rate-limit cooldown (see
// isAuthOnlyTransientlyCooledForModel), which may have been caused by unrelated traffic. The credential is appended to the selector candidates so the
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
		if _, isSelectable := selectable[candidate.ID]; isSelectable {
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
