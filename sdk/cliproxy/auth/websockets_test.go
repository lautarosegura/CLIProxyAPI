package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestWebsocketsEnabledResolution(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		auth         *Auth
		codexDefault bool
		want         bool
	}{
		{"nil auth", nil, true, false},
		{"codex inherits global true", &Auth{Provider: "codex"}, true, true},
		{"codex inherits global false", &Auth{Provider: "codex"}, false, false},
		{"codex attribute false overrides global true", &Auth{Provider: "codex", Attributes: map[string]string{"websockets": "false"}}, true, false},
		{"codex metadata false overrides global true", &Auth{Provider: "codex", Metadata: map[string]any{"websockets": false}}, true, false},
		{"codex metadata string false overrides global true", &Auth{Provider: "codex", Metadata: map[string]any{"websockets": "false"}}, true, false},
		{"codex attribute true with global false", &Auth{Provider: "codex", Attributes: map[string]string{"websockets": "true"}}, false, true},
		{"codex metadata true with global false", &Auth{Provider: "codex", Metadata: map[string]any{"websockets": true}}, false, true},
		{"attribute wins over metadata", &Auth{Provider: "codex", Attributes: map[string]string{"websockets": "true"}, Metadata: map[string]any{"websockets": false}}, false, true},
		{"invalid attribute falls back to metadata", &Auth{Provider: "codex", Attributes: map[string]string{"websockets": "maybe"}, Metadata: map[string]any{"websockets": false}}, true, false},
		{"xai ignores codex default", &Auth{Provider: "xai"}, true, false},
		{"xai explicit true", &Auth{Provider: "xai", Attributes: map[string]string{"websockets": "true"}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WebsocketsEnabled(tc.auth, tc.codexDefault); got != tc.want {
				t.Fatalf("WebsocketsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// delegatingSelector is a custom selector that forces the legacy selector path while
// still relying on the built-in Codex websocket preference.
type delegatingSelector struct{ inner *RoundRobinSelector }

func (s *delegatingSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	return s.inner.Pick(ctx, provider, model, opts, auths)
}

func TestManagerCodexWebsocketPreferenceFollowsConfigReload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector Selector
	}{
		{"scheduler fast path", &RoundRobinSelector{}},
		{"selector path", &delegatingSelector{inner: &RoundRobinSelector{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const model = "codex-websocket-default-model"
			inheritID := "codex-ws-inherit-" + tc.name
			httpID := "codex-ws-explicit-off-" + tc.name
			registerSchedulerModels(t, "codex", model, inheritID, httpID)

			manager := NewManager(nil, tc.selector, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
			for _, auth := range []*Auth{
				{ID: inheritID, Provider: "codex", Status: StatusActive},
				{ID: httpID, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"websockets": false}},
			} {
				if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
					t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
				}
			}

			wsCtx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
			pickedIDs := func() map[string]int {
				t.Helper()
				picked := make(map[string]int)
				for i := 0; i < 4; i++ {
					auth, _, errPick := manager.pickNext(wsCtx, "codex", model, cliproxyexecutor.Options{}, nil)
					if errPick != nil {
						t.Fatalf("pickNext() error = %v", errPick)
					}
					picked[auth.ID]++
				}
				return picked
			}

			manager.SetConfig(&internalconfig.Config{})
			if picked := pickedIDs(); picked[inheritID] == 0 || picked[httpID] == 0 {
				t.Fatalf("default off: picked = %v, want both credentials", picked)
			}

			manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{Websockets: true}})
			inheritAuth, _ := manager.GetByID(inheritID)
			httpAuth, _ := manager.GetByID(httpID)
			if !manager.WebsocketsEnabled(inheritAuth) || manager.WebsocketsEnabled(httpAuth) {
				t.Fatal("manager resolution must follow upstream.codex.websockets with explicit false winning")
			}
			if picked := pickedIDs(); picked[inheritID] != 4 {
				t.Fatalf("default on: picked = %v, want only %s", picked, inheritID)
			}

			manager.SetConfig(&internalconfig.Config{})
			if picked := pickedIDs(); picked[inheritID] == 0 || picked[httpID] == 0 {
				t.Fatalf("default off after reload: picked = %v, want both credentials", picked)
			}

			// HTTP downstream requests never filter by websocket eligibility.
			manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{Websockets: true}})
			picked := make(map[string]int)
			for i := 0; i < 4; i++ {
				auth, _, errPick := manager.pickNext(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
				if errPick != nil {
					t.Fatalf("pickNext(http) error = %v", errPick)
				}
				picked[auth.ID]++
			}
			if picked[inheritID] == 0 || picked[httpID] == 0 {
				t.Fatalf("http downstream: picked = %v, want both credentials", picked)
			}
		})
	}
}

func TestSchedulerCodexWebsocketsDefaultRefreshesReadyViews(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-off", Provider: "codex", Attributes: map[string]string{"websockets": "false"}},
		&Auth{ID: "codex-inherit", Provider: "codex"},
	)
	scheduler.setCodexWebsocketsDefault(true)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	for i := 0; i < 3; i++ {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", i, errPick)
		}
		if got.ID != "codex-inherit" {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want codex-inherit", i, got.ID)
		}
	}

	// New auths upserted after the default changes inherit it as well.
	scheduler.upsertAuth(&Auth{ID: "codex-new", Provider: "codex"})
	seen := make(map[string]bool)
	for i := 0; i < 4; i++ {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() after upsert #%d error = %v", i, errPick)
		}
		seen[got.ID] = true
	}
	if !seen["codex-new"] || !seen["codex-inherit"] || seen["codex-off"] {
		t.Fatalf("picked = %v, want codex-new and codex-inherit only", seen)
	}
}
