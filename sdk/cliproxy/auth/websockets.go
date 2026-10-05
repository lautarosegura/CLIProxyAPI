package auth

import (
	"context"
	"strconv"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// AttributeWebsockets is the per-credential attribute and metadata key that enables or
// disables the upstream Responses API websocket transport.
const AttributeWebsockets = "websockets"

// ExplicitWebsockets returns the per-credential websockets flag and whether the credential
// sets it. A parseable attribute wins over metadata.
func ExplicitWebsockets(auth *Auth) (enabled bool, ok bool) {
	if auth == nil {
		return false, false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes[AttributeWebsockets]); raw != "" {
			if parsed, errParse := strconv.ParseBool(raw); errParse == nil {
				return parsed, true
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false, false
	}
	switch value := auth.Metadata[AttributeWebsockets].(type) {
	case bool:
		return value, true
	case string:
		if parsed, errParse := strconv.ParseBool(strings.TrimSpace(value)); errParse == nil {
			return parsed, true
		}
	}
	return false, false
}

// WebsocketsEnabled reports whether auth uses the upstream Responses API websocket transport.
// An explicit per-credential value always wins. Otherwise Codex credentials inherit
// codexDefault (upstream.codex.websockets) and every other provider stays on HTTP.
func WebsocketsEnabled(auth *Auth, codexDefault bool) bool {
	if enabled, ok := ExplicitWebsockets(auth); ok {
		return enabled
	}
	return codexDefault && auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex")
}

// CodexWebsocketsDefault returns the configured websocket default for Codex credentials.
func CodexWebsocketsDefault(cfg *internalconfig.Config) bool {
	return cfg != nil && cfg.Codex.Websockets
}

// WebsocketsEnabled resolves auth against the manager's current runtime configuration,
// so a config reload that flips upstream.codex.websockets is observed immediately.
func (m *Manager) WebsocketsEnabled(auth *Auth) bool {
	return WebsocketsEnabled(auth, m.codexWebsocketsDefault())
}

func (m *Manager) codexWebsocketsDefault() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return CodexWebsocketsDefault(cfg)
}

type codexWebsocketsDefaultContextKey struct{}

// withCodexWebsocketsDefault carries the manager's Codex websocket default to built-in
// selectors, which receive only the candidate auths.
func withCodexWebsocketsDefault(ctx context.Context, enabled bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexWebsocketsDefaultContextKey{}, enabled)
}

func codexWebsocketsDefaultFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(codexWebsocketsDefaultContextKey{}).(bool)
	return enabled
}
