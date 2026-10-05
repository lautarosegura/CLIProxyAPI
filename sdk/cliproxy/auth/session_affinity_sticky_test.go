package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// stickyStep scripts one upstream attempt. A zero step succeeds.
type stickyStep struct {
	err          error
	bootstrapErr error
	midStreamErr error
}

type stickyScriptExecutor struct {
	provider string

	mu      sync.Mutex
	scripts map[string][]stickyStep
	calls   map[string]int
	// budgetSeen records whether an attempt ran with a request-scoped sticky budget.
	budgetSeen bool
}

func newStickyScriptExecutor(provider string) *stickyScriptExecutor {
	return &stickyScriptExecutor{provider: provider, scripts: make(map[string][]stickyStep), calls: make(map[string]int)}
}

func (e *stickyScriptExecutor) script(authID string, steps ...stickyStep) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scripts[authID] = append(e.scripts[authID], steps...)
}

func (e *stickyScriptExecutor) next(authID string) stickyStep {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls[authID]++
	steps := e.scripts[authID]
	if len(steps) == 0 {
		return stickyStep{}
	}
	e.scripts[authID] = steps[1:]
	return steps[0]
}

func (e *stickyScriptExecutor) callCount(authID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[authID]
}

func (e *stickyScriptExecutor) Identifier() string { return e.provider }

func (e *stickyScriptExecutor) recordBudget(ctx context.Context) {
	_, ok := ctx.Value(sessionStickyBudgetKey{}).(*sessionStickyBudget)
	e.mu.Lock()
	e.budgetSeen = e.budgetSeen || ok
	e.mu.Unlock()
}

func (e *stickyScriptExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recordBudget(ctx)
	step := e.next(auth.ID)
	if step.err != nil {
		return cliproxyexecutor.Response{}, step.err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *stickyScriptExecutor) ExecuteStream(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.recordBudget(ctx)
	step := e.next(auth.ID)
	if step.err != nil {
		return nil, step.err
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 2)
	if step.bootstrapErr != nil {
		ch <- cliproxyexecutor.StreamChunk{Err: step.bootstrapErr}
	} else {
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
		if step.midStreamErr != nil {
			ch <- cliproxyexecutor.StreamChunk{Err: step.midStreamErr}
		}
	}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *stickyScriptExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *stickyScriptExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	step := e.next(auth.ID)
	if step.err != nil {
		return cliproxyexecutor.Response{}, step.err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *stickyScriptExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type stickyCredentialQuotaError struct{}

func (stickyCredentialQuotaError) Error() string            { return "usage_limit_reached" }
func (stickyCredentialQuotaError) StatusCode() int          { return http.StatusTooManyRequests }
func (stickyCredentialQuotaError) IsCredentialScoped() bool { return true }

// stickyRateLimitError is a model-level 429 such as an ordinary Claude rate_limit_error.
type stickyRateLimitError struct{ retryAfter *time.Duration }

func (stickyRateLimitError) Error() string                { return "rate_limit_error" }
func (stickyRateLimitError) StatusCode() int              { return http.StatusTooManyRequests }
func (e stickyRateLimitError) RetryAfter() *time.Duration { return e.retryAfter }

type stickyFixture struct {
	manager  *Manager
	affinity *SessionAffinitySelector
	exec     *stickyScriptExecutor
	provider string
	model    string
	authA    string
	authB    string
	session  string

	mu     sync.Mutex
	delays []time.Duration
}

func newStickyFixture(t *testing.T, maxRetries int) *stickyFixture {
	t.Helper()
	withQuotaCooldownEnabled(t)
	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	f := &stickyFixture{
		provider: "sticky-provider",
		model:    "sticky-model-" + name,
		authA:    name + "-a",
		authB:    name + "-b",
		session:  "sess-" + name,
	}
	f.manager = NewManager(nil, nil, nil)
	f.affinity = NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback:   &RoundRobinSelector{},
		TTL:        time.Hour,
		MaxRetries: &maxRetries,
	})
	f.affinity.retryWait = func(ctx context.Context, delay time.Duration) error {
		f.mu.Lock()
		f.delays = append(f.delays, delay)
		f.mu.Unlock()
		return ctx.Err()
	}
	f.manager.SetSelector(f.affinity)
	t.Cleanup(f.affinity.Stop)
	f.exec = newStickyScriptExecutor(f.provider)
	f.manager.RegisterExecutor(f.exec)
	ctx := context.Background()
	for _, id := range []string{f.authA, f.authB} {
		if _, errRegister := f.manager.Register(WithSkipPersist(ctx), &Auth{ID: id, Provider: f.provider, Status: StatusActive}); errRegister != nil {
			t.Fatalf("Register(%s): %v", id, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, f.provider, []*registry.ModelInfo{{ID: f.model}})
		authID := id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	}
	return f
}

func (f *stickyFixture) sessionOpts() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{f.session}}}
}

func (f *stickyFixture) execute(t *testing.T, opts cliproxyexecutor.Options) (string, error) {
	t.Helper()
	resp, errExec := f.manager.Execute(context.Background(), []string{f.provider}, cliproxyexecutor.Request{Model: f.model}, opts)
	return string(resp.Payload), errExec
}

// establish binds the session to authA with one successful request.
func (f *stickyFixture) establish(t *testing.T) {
	t.Helper()
	got, errExec := f.execute(t, f.sessionOpts())
	if errExec != nil {
		t.Fatalf("establishing Execute failed: %v", errExec)
	}
	if got != f.authA {
		t.Fatalf("establishing Execute served by %q, want %q", got, f.authA)
	}
	f.requireBound(t, f.authA)
}

func (f *stickyFixture) requireBound(t *testing.T, want string) {
	t.Helper()
	got, status := f.affinity.LookupAffinity("mixed", f.model, f.session)
	if status != "bound" || got != want {
		t.Fatalf("session binding = %q (%s), want %q", got, status, want)
	}
}

func (f *stickyFixture) recordedDelays() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.delays...)
}

func (f *stickyFixture) authBlocked(t *testing.T, authID string) bool {
	t.Helper()
	auth, ok := f.manager.GetByID(authID)
	if !ok {
		t.Fatalf("auth %s not found", authID)
	}
	blocked, _, _ := isAuthBlockedForModel(auth, f.model, time.Now())
	return blocked
}

func TestSessionStickyRetryBoundSessionRecoversOnSameCredential(t *testing.T) {
	f := newStickyFixture(t, 2)
	f.establish(t)

	f.exec.script(f.authA, stickyStep{err: &Error{HTTPStatus: http.StatusInternalServerError, Message: "internal error"}})
	got, errExec := f.execute(t, f.sessionOpts())
	if errExec != nil {
		t.Fatalf("Execute failed: %v", errExec)
	}
	if got != f.authA {
		t.Fatalf("request served by %q, want bound credential %q", got, f.authA)
	}
	if calls := f.exec.callCount(f.authA); calls != 3 {
		t.Fatalf("bound credential calls = %d, want 3 (establish + failure + sticky retry)", calls)
	}
	if calls := f.exec.callCount(f.authB); calls != 0 {
		t.Fatalf("other credential calls = %d, want 0", calls)
	}
	if delays := f.recordedDelays(); len(delays) != 1 || delays[0] != 500*time.Millisecond {
		t.Fatalf("sticky backoff delays = %v, want [500ms]", delays)
	}
	f.requireBound(t, f.authA)
	if f.authBlocked(t, f.authA) {
		t.Fatal("bound credential was cooled by a transient failure that was retried successfully")
	}
}

func TestSessionStickyRetryExhaustedFailsOverAndRebinds(t *testing.T) {
	f := newStickyFixture(t, 2)
	f.establish(t)

	overloaded := &Error{HTTPStatus: 529, Message: "overloaded"}
	f.exec.script(f.authA, stickyStep{err: overloaded}, stickyStep{err: overloaded}, stickyStep{err: overloaded})
	got, errExec := f.execute(t, f.sessionOpts())
	if errExec != nil {
		t.Fatalf("Execute failed: %v", errExec)
	}
	if got != f.authB {
		t.Fatalf("request served by %q, want failover credential %q", got, f.authB)
	}
	if calls := f.exec.callCount(f.authA); calls != 4 {
		t.Fatalf("bound credential calls = %d, want 4 (establish + 1 + 2 sticky retries)", calls)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second}
	if delays := f.recordedDelays(); len(delays) != len(want) || delays[0] != want[0] || delays[1] != want[1] {
		t.Fatalf("sticky backoff delays = %v, want %v", delays, want)
	}
	f.requireBound(t, f.authB)
	if !f.authBlocked(t, f.authA) {
		t.Fatal("credential that exhausted sticky retries should enter the transient cooldown")
	}
}

func TestSessionStickyRetryCredentialQuotaFailsOverImmediately(t *testing.T) {
	f := newStickyFixture(t, 2)
	f.establish(t)

	f.exec.script(f.authA, stickyStep{err: stickyCredentialQuotaError{}})
	got, errExec := f.execute(t, f.sessionOpts())
	if errExec != nil {
		t.Fatalf("Execute failed: %v", errExec)
	}
	if got != f.authB {
		t.Fatalf("request served by %q, want failover credential %q", got, f.authB)
	}
	if calls := f.exec.callCount(f.authA); calls != 2 {
		t.Fatalf("bound credential calls = %d, want 2 (no sticky retry on quota)", calls)
	}
	if delays := f.recordedDelays(); len(delays) != 0 {
		t.Fatalf("sticky backoff delays = %v, want none", delays)
	}
	f.requireBound(t, f.authB)
}

func TestSessionStickyRetryAuthFailureFailsOverImmediately(t *testing.T) {
	f := newStickyFixture(t, 2)
	f.establish(t)

	f.exec.script(f.authA, stickyStep{err: &Error{HTTPStatus: http.StatusForbidden, Message: "forbidden"}})
	got, errExec := f.execute(t, f.sessionOpts())
	if errExec != nil {
		t.Fatalf("Execute failed: %v", errExec)
	}
	if got != f.authB || f.exec.callCount(f.authA) != 2 || len(f.recordedDelays()) != 0 {
		t.Fatalf("served by %q after %d bound calls and delays %v; want immediate failover to %q", got, f.exec.callCount(f.authA), f.recordedDelays(), f.authB)
	}
	f.requireBound(t, f.authB)
}

func TestSessionStickyRetryDoesNotApplyWithoutEstablishedBinding(t *testing.T) {
	t.Run("no session", func(t *testing.T) {
		f := newStickyFixture(t, 2)
		f.exec.script(f.authA, stickyStep{err: &Error{HTTPStatus: http.StatusInternalServerError, Message: "internal error"}})
		got, errExec := f.execute(t, cliproxyexecutor.Options{})
		if errExec != nil {
			t.Fatalf("Execute failed: %v", errExec)
		}
		if got != f.authB || f.exec.callCount(f.authA) != 1 || len(f.recordedDelays()) != 0 {
			t.Fatalf("served by %q after %d calls and delays %v; want immediate failover", got, f.exec.callCount(f.authA), f.recordedDelays())
		}
	})
	t.Run("cold session", func(t *testing.T) {
		f := newStickyFixture(t, 2)
		f.exec.script(f.authA, stickyStep{err: &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"}})
		got, errExec := f.execute(t, f.sessionOpts())
		if errExec != nil {
			t.Fatalf("Execute failed: %v", errExec)
		}
		if got != f.authB || f.exec.callCount(f.authA) != 1 || len(f.recordedDelays()) != 0 {
			t.Fatalf("served by %q after %d calls and delays %v; want immediate failover", got, f.exec.callCount(f.authA), f.recordedDelays())
		}
		f.requireBound(t, f.authB)
	})
	t.Run("max retries zero", func(t *testing.T) {
		f := newStickyFixture(t, 0)
		f.establish(t)
		f.exec.script(f.authA, stickyStep{err: &Error{HTTPStatus: http.StatusInternalServerError, Message: "internal error"}})
		got, errExec := f.execute(t, f.sessionOpts())
		if errExec != nil {
			t.Fatalf("Execute failed: %v", errExec)
		}
		if got != f.authB || f.exec.callCount(f.authA) != 2 || len(f.recordedDelays()) != 0 {
			t.Fatalf("served by %q after %d calls and delays %v; want legacy immediate failover", got, f.exec.callCount(f.authA), f.recordedDelays())
		}
	})
}

func TestSessionStickyRetryRequestScopedErrorDoesNotRetryOrUnbind(t *testing.T) {
	f := newStickyFixture(t, 2)
	f.establish(t)

	f.exec.script(f.authA, stickyStep{err: &Error{HTTPStatus: http.StatusBadRequest, Message: `{"error":{"type":"invalid_request_error","message":"bad input"}}`}})
	if _, errExec := f.execute(t, f.sessionOpts()); errExec == nil {
		t.Fatal("Execute succeeded, want request error")
	}
	if f.exec.callCount(f.authA) != 2 || f.exec.callCount(f.authB) != 0 || len(f.recordedDelays()) != 0 {
		t.Fatalf("calls a=%d b=%d delays=%v; want a single attempt without retries", f.exec.callCount(f.authA), f.exec.callCount(f.authB), f.recordedDelays())
	}
	f.requireBound(t, f.authA)
}

func TestSessionStickyRetryBackoffRespectsContextCancellation(t *testing.T) {
	f := newStickyFixture(t, 2)
	f.establish(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.affinity.retryWait = func(waitCtx context.Context, _ time.Duration) error {
		cancel()
		return waitSessionStickyRetry(waitCtx, time.Hour)
	}
	f.exec.script(f.authA, stickyStep{err: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "unavailable"}})
	_, errExec := f.manager.Execute(ctx, []string{f.provider}, cliproxyexecutor.Request{Model: f.model}, f.sessionOpts())
	if !errors.Is(errExec, context.Canceled) {
		t.Fatalf("Execute error = %v, want context.Canceled", errExec)
	}
	if f.exec.callCount(f.authA) != 2 || f.exec.callCount(f.authB) != 0 {
		t.Fatalf("calls a=%d b=%d; want no attempts after cancellation", f.exec.callCount(f.authA), f.exec.callCount(f.authB))
	}
	f.requireBound(t, f.authA)
}

func TestSessionStickyRetryStreamRetriesBeforeOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		step stickyStep
	}{
		{name: "execute error", step: stickyStep{err: &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"}}},
		{name: "bootstrap error", step: stickyStep{bootstrapErr: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "unavailable"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStickyFixture(t, 2)
			f.establish(t)
			f.exec.script(f.authA, tc.step)

			result, errStream := f.manager.ExecuteStream(context.Background(), []string{f.provider}, cliproxyexecutor.Request{Model: f.model}, f.sessionOpts())
			if errStream != nil {
				t.Fatalf("ExecuteStream failed: %v", errStream)
			}
			var payload strings.Builder
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk error: %v", chunk.Err)
				}
				payload.Write(chunk.Payload)
			}
			if payload.String() != f.authA {
				t.Fatalf("stream served by %q, want bound credential %q", payload.String(), f.authA)
			}
			if f.exec.callCount(f.authA) != 3 || f.exec.callCount(f.authB) != 0 || len(f.recordedDelays()) != 1 {
				t.Fatalf("calls a=%d b=%d delays=%v; want one sticky retry on the bound credential", f.exec.callCount(f.authA), f.exec.callCount(f.authB), f.recordedDelays())
			}
			f.requireBound(t, f.authA)
		})
	}
}

func TestSessionStickyMidStreamTransientErrorKeepsBinding(t *testing.T) {
	f := newStickyFixture(t, 2)
	f.establish(t)
	f.exec.script(f.authA, stickyStep{midStreamErr: &Error{HTTPStatus: http.StatusInternalServerError, Message: "stream interrupted"}})

	result, errStream := f.manager.ExecuteStream(context.Background(), []string{f.provider}, cliproxyexecutor.Request{Model: f.model}, f.sessionOpts())
	if errStream != nil {
		t.Fatalf("ExecuteStream failed: %v", errStream)
	}
	var sawErr bool
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected the mid-stream error to reach the client")
	}
	f.requireBound(t, f.authA)

	// The next turn returns to the bound credential even though it is in a transient cooldown.
	got, errExec := f.execute(t, f.sessionOpts())
	if errExec != nil {
		t.Fatalf("follow-up Execute failed: %v", errExec)
	}
	if got != f.authA {
		t.Fatalf("follow-up turn served by %q, want bound credential %q", got, f.authA)
	}
}

func TestSessionAffinityBoundPickToleratesTransientCooldownOnly(t *testing.T) {
	t.Run("transient cooldown from unrelated traffic", func(t *testing.T) {
		f := newStickyFixture(t, 2)
		f.establish(t)
		f.manager.MarkResult(context.Background(), Result{AuthID: f.authA, Provider: f.provider, Model: f.model, Error: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "unavailable"}})
		if !f.authBlocked(t, f.authA) {
			t.Fatal("expected unrelated transient failure to cool the credential")
		}

		got, errExec := f.execute(t, f.sessionOpts())
		if errExec != nil || got != f.authA {
			t.Fatalf("bound session served by %q (%v), want %q", got, errExec, f.authA)
		}
		f.requireBound(t, f.authA)

		// The bound request's success cleared the cooldown; cool it again for unbound traffic.
		f.manager.MarkResult(context.Background(), Result{AuthID: f.authA, Provider: f.provider, Model: f.model, Error: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "unavailable"}})
		got, errExec = f.execute(t, cliproxyexecutor.Options{})
		if errExec != nil || got != f.authB {
			t.Fatalf("unbound request served by %q (%v), want %q while the bound credential cools", got, errExec, f.authB)
		}
	})
	t.Run("short rate limit cooldown from unrelated traffic", func(t *testing.T) {
		f := newStickyFixture(t, 2)
		f.establish(t)
		retryAfter := 15 * time.Second
		f.manager.MarkResult(context.Background(), Result{AuthID: f.authA, Provider: f.provider, Model: f.model, RetryAfter: &retryAfter, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"}})
		if !f.authBlocked(t, f.authA) {
			t.Fatal("expected unrelated rate limit to cool the credential")
		}

		got, errExec := f.execute(t, f.sessionOpts())
		if errExec != nil || got != f.authA {
			t.Fatalf("bound session served by %q (%v), want %q", got, errExec, f.authA)
		}
		f.requireBound(t, f.authA)
	})
	t.Run("quota cooldown fails over", func(t *testing.T) {
		f := newStickyFixture(t, 2)
		f.establish(t)
		retryAfter := time.Hour
		f.manager.MarkResult(context.Background(), Result{AuthID: f.authA, Provider: f.provider, Model: f.model, RetryAfter: &retryAfter, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}})

		got, errExec := f.execute(t, f.sessionOpts())
		if errExec != nil || got != f.authB {
			t.Fatalf("bound session served by %q (%v), want failover to %q", got, errExec, f.authB)
		}
		f.requireBound(t, f.authB)
	})
}

func TestSessionStickyRetryHelpers(t *testing.T) {
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	for i, expected := range want {
		if got := sessionStickyRetryDelay(i + 1); got != expected {
			t.Fatalf("sessionStickyRetryDelay(%d) = %s, want %s", i+1, got, expected)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if errWait := waitSessionStickyRetry(ctx, time.Hour); !errors.Is(errWait, context.Canceled) {
		t.Fatalf("waitSessionStickyRetry on canceled context = %v, want context.Canceled", errWait)
	}

	negative, large := -1, 50
	if got := NormalizeSessionAffinityMaxRetries(nil); got != DefaultSessionAffinityMaxRetries {
		t.Fatalf("default max retries = %d", got)
	}
	if got := NormalizeSessionAffinityMaxRetries(&negative); got != 0 {
		t.Fatalf("negative max retries = %d, want 0", got)
	}
	if got := NormalizeSessionAffinityMaxRetries(&large); got != MaxSessionAffinityMaxRetries {
		t.Fatalf("large max retries = %d, want %d", got, MaxSessionAffinityMaxRetries)
	}

	shortWait, longWait := 3*time.Second, 10*time.Minute
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&Error{HTTPStatus: http.StatusInternalServerError}, true},
		{&Error{HTTPStatus: 529}, true},
		{&Error{HTTPStatus: http.StatusRequestTimeout}, true},
		{&Error{Code: "empty_stream"}, true},
		{errors.New("read tcp 1.2.3.4:443: connection reset by peer"), true},
		{&Error{HTTPStatus: http.StatusTooManyRequests}, true},
		{stickyRateLimitError{retryAfter: &shortWait}, true},
		{stickyRateLimitError{retryAfter: &longWait}, false},
		{&Error{HTTPStatus: http.StatusUnauthorized}, false},
		{&Error{HTTPStatus: http.StatusForbidden}, false},
		{&Error{HTTPStatus: http.StatusBadRequest}, false},
		{stickyCredentialQuotaError{}, false},
		{context.Canceled, false},
	} {
		if _, got := sessionStickyRetryWait(tc.err); got != tc.want {
			t.Fatalf("sessionStickyRetryWait(%v) = %t, want %t", tc.err, got, tc.want)
		}
	}
}

func TestSessionStickyRetryShortRateLimitWaitsOnBoundCredential(t *testing.T) {
	f := newStickyFixture(t, 2)
	f.establish(t)

	retryAfter := 3 * time.Second
	f.exec.script(f.authA, stickyStep{err: stickyRateLimitError{retryAfter: &retryAfter}})
	got, errExec := f.execute(t, f.sessionOpts())
	if errExec != nil {
		t.Fatalf("Execute failed: %v", errExec)
	}
	if got != f.authA {
		t.Fatalf("request served by %q, want bound credential %q", got, f.authA)
	}
	if calls := f.exec.callCount(f.authB); calls != 0 {
		t.Fatalf("other credential calls = %d, want 0", calls)
	}
	if delays := f.recordedDelays(); len(delays) != 1 || delays[0] != retryAfter {
		t.Fatalf("sticky delays = %v, want [%s] from the upstream retry hint", delays, retryAfter)
	}
	f.requireBound(t, f.authA)
	if f.authBlocked(t, f.authA) {
		t.Fatal("bound credential was cooled by a rate limit that was waited out")
	}
}

func TestSessionStickyRetryLongRateLimitFailsOver(t *testing.T) {
	f := newStickyFixture(t, 2)
	f.establish(t)

	retryAfter := 10 * time.Minute
	f.exec.script(f.authA, stickyStep{err: stickyRateLimitError{retryAfter: &retryAfter}})
	got, errExec := f.execute(t, f.sessionOpts())
	if errExec != nil {
		t.Fatalf("Execute failed: %v", errExec)
	}
	if got != f.authB || f.exec.callCount(f.authA) != 2 || len(f.recordedDelays()) != 0 {
		t.Fatalf("served by %q after %d bound calls and delays %v; want immediate failover to %q", got, f.exec.callCount(f.authA), f.recordedDelays(), f.authB)
	}
	f.requireBound(t, f.authB)
}

func TestSessionStickyRetryBudgetSpansRequestRetryRounds(t *testing.T) {
	f := newStickyFixture(t, 2)
	f.establish(t)
	if !f.exec.budgetSeen {
		t.Fatal("Execute did not attach a request-scoped sticky budget")
	}
	f.exec.budgetSeen = false
	stream, errStream := f.manager.ExecuteStream(context.Background(), []string{f.provider}, cliproxyexecutor.Request{Model: f.model}, f.sessionOpts())
	if errStream != nil {
		t.Fatalf("ExecuteStream failed: %v", errStream)
	}
	discardStreamChunks(stream.Chunks)
	if !f.exec.budgetSeen {
		t.Fatal("ExecuteStream did not attach a request-scoped sticky budget")
	}

	auth, ok := f.manager.GetByID(f.authA)
	if !ok {
		t.Fatalf("auth %s not found", f.authA)
	}
	metadata := map[string]any{sessionAffinityEstablishedAuthMetadataKey: f.authA}
	transient := &Error{HTTPStatus: http.StatusInternalServerError, Message: "internal error"}

	ctx := withSessionStickyBudget(context.Background())
	firstRound := f.manager.sessionStickyRetryFor(ctx, metadata, auth)
	for i := 0; i < 2; i++ {
		if !f.manager.shouldRetrySessionSticky(firstRound, auth, transient, false) {
			t.Fatalf("first round sticky retry %d refused", i+1)
		}
	}
	if f.manager.shouldRetrySessionSticky(firstRound, auth, transient, false) {
		t.Fatal("first round exceeded the sticky budget")
	}
	// A later request-retry round of the same request reselects the bound credential.
	secondRound := f.manager.sessionStickyRetryFor(withSessionStickyBudget(ctx), metadata, auth)
	if f.manager.shouldRetrySessionSticky(secondRound, auth, transient, false) {
		t.Fatal("request-retry round reset the sticky budget of the same request")
	}

	nextRequest := f.manager.sessionStickyRetryFor(withSessionStickyBudget(context.Background()), metadata, auth)
	if !f.manager.shouldRetrySessionSticky(nextRequest, auth, transient, false) {
		t.Fatal("a new request did not get a fresh sticky budget")
	}
}

func TestSessionAffinityCountTokensNeverMovesBinding(t *testing.T) {
	count := func(t *testing.T, f *stickyFixture, opts cliproxyexecutor.Options) string {
		t.Helper()
		resp, errCount := f.manager.ExecuteCount(context.Background(), []string{f.provider}, cliproxyexecutor.Request{Model: f.model}, opts)
		if errCount != nil {
			t.Fatalf("ExecuteCount failed: %v", errCount)
		}
		return string(resp.Payload)
	}

	t.Run("uses the bound credential", func(t *testing.T) {
		f := newStickyFixture(t, 2)
		f.establish(t)
		for i := 0; i < 3; i++ {
			if got := count(t, f, f.sessionOpts()); got != f.authA {
				t.Fatalf("count %d served by %q, want bound credential %q", i+1, got, f.authA)
			}
		}
		f.requireBound(t, f.authA)
	})
	t.Run("failover keeps the thread binding", func(t *testing.T) {
		f := newStickyFixture(t, 2)
		f.establish(t)
		f.exec.script(f.authA, stickyStep{err: &Error{HTTPStatus: http.StatusForbidden, Message: "forbidden"}})
		if got := count(t, f, f.sessionOpts()); got != f.authB {
			t.Fatalf("count served by %q, want failover credential %q", got, f.authB)
		}
		f.requireBound(t, f.authA)
	})
	t.Run("cold session stays unbound", func(t *testing.T) {
		f := newStickyFixture(t, 2)
		count(t, f, f.sessionOpts())
		if got, status := f.affinity.LookupAffinity("mixed", f.model, f.session); status == "bound" {
			t.Fatalf("count bound a cold session to %q", got)
		}
	})
}

func TestSessionStickyRateLimitPrefersFreePoolModels(t *testing.T) {
	f := newStickyFixture(t, 2)
	auth, ok := f.manager.GetByID(f.authA)
	if !ok {
		t.Fatalf("auth %s not found", f.authA)
	}
	metadata := map[string]any{sessionAffinityEstablishedAuthMetadataKey: f.authA}
	retryAfter := 3 * time.Second
	rateLimited := stickyRateLimitError{retryAfter: &retryAfter}

	sticky := f.manager.sessionStickyRetryFor(context.Background(), metadata, auth)
	if f.manager.shouldRetrySessionSticky(sticky, auth, rateLimited, true) {
		t.Fatal("rate-limited pool model was retried while another pool model remained")
	}
	if !f.manager.shouldRetrySessionSticky(sticky, auth, rateLimited, false) {
		t.Fatal("rate-limited last pool model was not retried on the bound credential")
	}
	if !f.manager.shouldRetrySessionSticky(sticky, auth, &Error{HTTPStatus: http.StatusInternalServerError}, true) {
		t.Fatal("transient failure lost its sticky retry inside a pool")
	}

	now := time.Now()
	cooled := &Auth{ID: "pool-auth", Provider: f.provider, Status: StatusActive, ModelStates: map[string]*ModelState{
		"pool-m1": {Status: StatusError, Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(10 * time.Second)}, NextRetryAfter: now.Add(10 * time.Second)},
	}}
	got := f.manager.filterExecutionModelsWithTolerance(cooled, "pool-route", []string{"pool-m1", "pool-m2"}, true, true)
	if len(got) != 2 || got[0] != "pool-m2" || got[1] != "pool-m1" {
		t.Fatalf("pool order = %v, want free model first then the briefly cooled one", got)
	}
	if got := f.manager.filterExecutionModelsWithTolerance(cooled, "pool-route", []string{"pool-m1", "pool-m2"}, true, false); len(got) != 1 || got[0] != "pool-m2" {
		t.Fatalf("unbound pool models = %v, want only the free model", got)
	}
}

func TestSessionAffinityShortRateLimitCooldownStillSkippedForUnboundTraffic(t *testing.T) {
	f := newStickyFixture(t, 2)
	retryAfter := 15 * time.Second
	f.manager.MarkResult(context.Background(), Result{AuthID: f.authA, Provider: f.provider, Model: f.model, RetryAfter: &retryAfter, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"}})
	for i := 0; i < 3; i++ {
		got, errExec := f.execute(t, cliproxyexecutor.Options{})
		if errExec != nil || got != f.authB {
			t.Fatalf("unbound request %d served by %q (%v), want %q", i+1, got, errExec, f.authB)
		}
	}
}
