package cliproxy

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestNormalizedRoutingRuntimeStateSessionAffinityMaxRetries(t *testing.T) {
	zero := 0
	large := 99
	for _, tc := range []struct {
		name     string
		affinity bool
		value    *int
		want     int
	}{
		{name: "default", affinity: true, want: coreauth.DefaultSessionAffinityMaxRetries},
		{name: "disabled", affinity: true, value: &zero, want: 0},
		{name: "capped", affinity: true, value: &large, want: coreauth.MaxSessionAffinityMaxRetries},
		{name: "affinity off ignores value", affinity: false, value: &large, want: coreauth.DefaultSessionAffinityMaxRetries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
				SessionAffinity:           tc.affinity,
				SessionAffinityMaxRetries: tc.value,
			}})
			if state.sessionAffinityMaxRetry != tc.want {
				t.Fatalf("max retries = %d, want %d", state.sessionAffinityMaxRetry, tc.want)
			}
		})
	}
}

func TestApplyManagerConfigHotReloadKeepsSessionBindings(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	apply := func(cfg *internalconfig.Config) *coreauth.SessionAffinitySelector {
		t.Helper()
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg, sequence: 1}) {
			t.Fatal("applyManagerConfig failed")
		}
		affinity, ok := service.coreManager.Selector().(*coreauth.SessionAffinitySelector)
		if !ok {
			t.Fatalf("selector = %T, want *auth.SessionAffinitySelector", service.coreManager.Selector())
		}
		return affinity
	}

	first := apply(&internalconfig.Config{Routing: internalconfig.RoutingConfig{SessionAffinity: true, SessionAffinityTTL: "1h"}})
	t.Cleanup(first.Stop)
	auths := []*coreauth.Auth{{ID: "auth-a", Status: coreauth.StatusActive}, {ID: "auth-b", Status: coreauth.StatusActive}}
	headers := http.Header{"X-Session-Id": []string{"sess-hot-reload"}}
	bound, errPick := first.Pick(context.Background(), "mixed", "model-x", cliproxyexecutor.Options{Headers: headers}, auths)
	if errPick != nil || bound == nil {
		t.Fatalf("Pick: %v", errPick)
	}

	maxRetries := 4
	second := apply(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
		SessionAffinity:           true,
		SessionAffinityTTL:        "24h",
		SessionAffinityMaxRetries: &maxRetries,
	}})
	t.Cleanup(second.Stop)
	if second == first {
		t.Fatal("routing change did not rebuild the selector")
	}
	if got, status := second.LookupAffinity("mixed", "model-x", "sess-hot-reload"); status != "bound" || got != bound.ID {
		t.Fatalf("binding after hot reload = %q (%s), want %q", got, status, bound.ID)
	}
}
