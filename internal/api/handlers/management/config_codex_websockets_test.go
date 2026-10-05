package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestPatchCodexKeyUpdatesWebsockets(t *testing.T) {
	h := &Handler{
		cfg: &config.Config{CodexKey: []config.CodexKey{{
			APIKey:  "codex-key",
			BaseURL: "https://codex.example.com",
		}}},
		configFilePath: writeTestConfigFile(t),
	}
	patch := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/codex-api-key", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PatchCodexKey(ctx)
		return rec
	}

	if rec := patch(`{"index":0,"value":{"websockets":true}}`); rec.Code != http.StatusOK {
		t.Fatalf("true: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := h.cfg.CodexKey[0].Websockets; got == nil || !*got {
		t.Fatalf("websockets = %v, want true", got)
	}

	if rec := patch(`{"index":0,"value":{"priority":3}}`); rec.Code != http.StatusOK {
		t.Fatalf("absent: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := h.cfg.CodexKey[0].Websockets; got == nil || !*got {
		t.Fatalf("absent websockets field changed the override: %v", got)
	}

	if rec := patch(`{"index":0,"value":{"websockets":false}}`); rec.Code != http.StatusOK {
		t.Fatalf("false: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := h.cfg.CodexKey[0].Websockets; got == nil || *got {
		t.Fatalf("websockets = %v, want explicit false", got)
	}

	if rec := patch(`{"index":0,"value":{"websockets":null}}`); rec.Code != http.StatusOK {
		t.Fatalf("null: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := h.cfg.CodexKey[0].Websockets; got != nil {
		t.Fatalf("websockets = %v, want nil (inherit upstream.codex.websockets)", *got)
	}

	if rec := patch(`{"index":0,"value":{"websockets":"yes"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid: status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}
