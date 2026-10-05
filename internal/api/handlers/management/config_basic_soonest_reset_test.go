package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestNormalizeRoutingStrategySoonestReset(t *testing.T) {
	for _, input := range []string{"soonest-reset", "SoonestReset", " sr "} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "soonest-reset" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want soonest-reset, true", input, got, ok)
		}
	}
	if got, ok := normalizeRoutingStrategy("soonest"); ok {
		t.Fatalf("normalizeRoutingStrategy(%q) = %q, true; want rejection", "soonest", got)
	}
}

func TestPutRoutingStrategyRejectsUnknownStrategy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/routing/strategy", bytes.NewBufferString(`{"value":"soonest"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutRoutingStrategy(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}
