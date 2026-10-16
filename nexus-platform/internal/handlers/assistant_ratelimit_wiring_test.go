package handlers

// assistant_ratelimit_wiring_test.go — DAILY-QUEUE #156: WithAssistantRateLimiter had no
// router-level test (the Auth/SSO/Data options each have one), so the middleware could drift off
// POST /assistant/ask with every gate green. Through the REAL RegisterAPIRoutes: the per-user cap
// bites on ask, and only on ask. cmd/server/main_wiring_test.go locks that main.go passes the
// option at all.
//
// Fixture: a nil-agent AssistantHandler panics on any real dispatch and gin's recovery turns that
// into 500 — so "not 429" means the request got PAST the limiter, "429" means it did not.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
)

func TestAssistantRateLimitWiring_CapsAskOnly(t *testing.T) {
	const max = 2
	gin.SetMode(gin.TestMode)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"jwt": claimsFor("t1", "sales", false)}}
	r := gin.New()
	r.Use(gin.RecoveryWithWriter(io.Discard))
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, APIHandlers{
		Assistant: NewAssistantHandler(nil, nil, nil, zap.NewNop()),
	}, zap.NewNop(),
		WithAssistantRateLimiter(middleware.AssistantRateLimit(middleware.AssistantRateLimitConfig{MaxAttempts: max, Window: time.Minute})),
	)
	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(`{"message":"hi"}`))
		req.Header.Set("Authorization", "Bearer jwt")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	for i := 1; i <= max; i++ {
		if w := do(http.MethodPost, "/api/v1/assistant/ask"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("ask %d within the per-user budget got 429", i)
		}
	}
	w := do(http.MethodPost, "/api/v1/assistant/ask")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("ask %d = %d, want 429 (limiter not mounted on /assistant/ask)", max+1, w.Code)
	}
	if !strings.Contains(w.Body.String(), `"code":"RATE_LIMITED"`) {
		t.Errorf("429 body = %s, want code RATE_LIMITED", w.Body.String())
	}
	// The session routes carry no LLM spend and stay outside the cap.
	if w := do(http.MethodGet, "/api/v1/assistant/sessions"); w.Code == http.StatusTooManyRequests {
		t.Errorf("GET /assistant/sessions after the ask cap = 429, want it uncounted")
	}
}
