package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSoonestResetRoutingSelector(t *testing.T) {
	for _, input := range []string{"soonest-reset", "SoonestReset", " sr "} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: input},
		})
		if state.strategy != "soonest-reset" {
			t.Fatalf("strategy for %q = %q, want soonest-reset", input, state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector); !ok {
			t.Fatalf("selector type = %T, want *auth.SoonestResetSelector", newRoutingSelector(state))
		}
	}
}

func TestSoonestResetRoutingSelectorWithSessionAffinity(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset", SessionAffinity: true},
	})
	selector := newRoutingSelector(state)
	affinity, ok := selector.(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", selector)
	}
	affinity.Stop()
}

func TestApplyManagerConfigSwitchesToAndFromSoonestReset(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)}
	apply := func(strategy string, sequence uint64) coreauth.Selector {
		t.Helper()
		commit := configCommit{
			cfg:      &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: strategy}},
			sequence: sequence,
		}
		if !service.applyManagerConfig(context.Background(), commit) {
			t.Fatalf("applyManagerConfig(%q) failed", strategy)
		}
		return service.coreManager.Selector()
	}

	if selector := apply("soonest-reset", 1); selector == nil {
		t.Fatal("selector is nil")
	} else if _, ok := selector.(*coreauth.SoonestResetSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.SoonestResetSelector", selector)
	}
	if _, ok := apply("round-robin", 2).(*coreauth.RoundRobinSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.RoundRobinSelector", service.coreManager.Selector())
	}
	if _, ok := apply("sr", 3).(*coreauth.SoonestResetSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.SoonestResetSelector", service.coreManager.Selector())
	}
}
