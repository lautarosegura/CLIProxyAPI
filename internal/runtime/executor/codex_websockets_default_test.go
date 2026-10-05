package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// TestCodexAutoExecutorRoutesByGlobalWebsocketsDefault verifies that upstream.codex.websockets
// enables the upstream websocket only for downstream websocket requests and that explicit
// per-credential values win.
func TestCodexAutoExecutorRoutesByGlobalWebsocketsDefault(t *testing.T) {
	for _, tc := range []struct {
		name                string
		globalDefault       bool
		credentialFlag      string
		downstreamWebsocket bool
		wantUpgrade         bool
	}{
		{name: "global default with downstream websocket", globalDefault: true, downstreamWebsocket: true, wantUpgrade: true},
		{name: "global default with downstream http", globalDefault: true, downstreamWebsocket: false, wantUpgrade: false},
		{name: "explicit false overrides global default", globalDefault: true, credentialFlag: "false", downstreamWebsocket: true, wantUpgrade: false},
		{name: "explicit true without global default", globalDefault: false, credentialFlag: "true", downstreamWebsocket: true, wantUpgrade: true},
		{name: "explicit true with downstream http", globalDefault: false, credentialFlag: "true", downstreamWebsocket: false, wantUpgrade: false},
		{name: "no default and no flag", globalDefault: false, downstreamWebsocket: true, wantUpgrade: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var upgrades, httpPosts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					upgrades.Add(1)
				} else if r.Method == http.MethodPost {
					httpPosts.Add(1)
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"routing probe"}}`))
			}))
			defer server.Close()

			cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
			cfg.Codex.Websockets = tc.globalDefault
			exec := NewCodexAutoExecutor(cfg)
			exec.wsExec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			attrs := map[string]string{"api_key": "sk-test", "base_url": server.URL}
			if tc.credentialFlag != "" {
				attrs[cliproxyauth.AttributeWebsockets] = tc.credentialFlag
			}
			auth := &cliproxyauth.Auth{ID: "codex-routing-" + tc.name, Provider: "codex", Attributes: attrs}

			ctx := context.Background()
			if tc.downstreamWebsocket {
				ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
			}
			stream, errExecute := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
				Model:   "gpt-5.4",
				Payload: []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hello"}]}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
			if errExecute == nil && stream != nil {
				for range stream.Chunks {
				}
			}

			if gotUpgrade := upgrades.Load() > 0; gotUpgrade != tc.wantUpgrade {
				t.Fatalf("websocket upgrade attempted = %v, want %v (http posts = %d)", gotUpgrade, tc.wantUpgrade, httpPosts.Load())
			}
			if !tc.wantUpgrade && httpPosts.Load() == 0 {
				t.Fatal("expected the request to use the HTTP transport")
			}
		})
	}
}
