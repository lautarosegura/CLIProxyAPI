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

func (e *stickyScriptExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	step := e.next(auth.ID)
	if step.err != nil {
		return cliproxyexecutor.Response{}, step.err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *stickyScriptExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
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

func (e *stickyScriptExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *stickyScriptExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type stickyCredentialQuotaError struct{}

func (stickyCredentialQuotaError) Error() string            { return "usage_limit_reached" }
func (stickyCredentialQuotaError) StatusCode() int          { return http.StatusTooManyRequests }
func (stickyCredentialQuotaError) IsCredentialScoped() bool { return true }

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

	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&Error{HTTPStatus: http.StatusInternalServerError}, true},
		{&Error{HTTPStatus: 529}, true},
		{&Error{HTTPStatus: http.StatusRequestTimeout}, true},
		{&Error{Code: "empty_stream"}, true},
		{errors.New("read tcp 1.2.3.4:443: connection reset by peer"), true},
		{&Error{HTTPStatus: http.StatusTooManyRequests}, false},
		{&Error{HTTPStatus: http.StatusUnauthorized}, false},
		{&Error{HTTPStatus: http.StatusForbidden}, false},
		{&Error{HTTPStatus: http.StatusBadRequest}, false},
		{stickyCredentialQuotaError{}, false},
		{context.Canceled, false},
	} {
		if got := isSessionStickyRetryableError(tc.err); got != tc.want {
			t.Fatalf("isSessionStickyRetryableError(%v) = %t, want %t", tc.err, got, tc.want)
		}
	}
}
