package openai

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestResponsesWebsocketCodexPassthroughFollowsGlobalWebsocketsDefault(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&websocketProviderCaptureExecutor{provider: "codex"})

	const modelName = "codex-global-websockets-model"
	inherit := &coreauth.Auth{ID: "auth-codex-ws-inherit", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), inherit); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(inherit.ID, inherit.Provider, []*registry.ModelInfo{{ID: modelName}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(inherit.ID)
	})

	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	if h.responsesWebsocketUsesUpstreamWebsocketPassthrough(modelName) {
		t.Fatal("codex auth without websockets flag must not use passthrough while the global default is off")
	}

	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{Websockets: true}})
	if !h.responsesWebsocketUsesUpstreamWebsocketPassthrough(modelName) {
		t.Fatal("codex auth must inherit upstream.codex.websockets=true")
	}
	if !h.websocketUpstreamSupportsIncrementalInputForModel(modelName) {
		t.Fatal("codex auth must report incremental input support when inheriting the global default")
	}

	optOut := &coreauth.Auth{ID: "auth-codex-ws-opt-out", Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"websockets": false}}
	if h.responsesWebsocketAuthSupportsIncrementalInput(optOut) {
		t.Fatal("explicit websockets=false must override upstream.codex.websockets=true")
	}

	manager.SetConfig(&internalconfig.Config{})
	if h.responsesWebsocketUsesUpstreamWebsocketPassthrough(modelName) {
		t.Fatal("config reload disabling the global default must be observed")
	}
}
