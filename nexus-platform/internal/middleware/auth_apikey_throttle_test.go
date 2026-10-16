package middleware

// Queue #356 (specs/045 FR-9): the Auth stage hands the API-key verifier the client IP (the same
// one the per-IP limiters key on) and answers its "throttled" verdict with the ONE canonical 429 —
// never a 401, never a signal about whether the string was a key.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

// ctxVerifier is a context-aware verifier (the composite's shape) recording the client IP it saw.
type ctxVerifier struct {
	err   error
	gotIP string
	calls int
}

func (v *ctxVerifier) VerifyAccessToken(string) (*auth.Claims, error) { return nil, v.err }

func (v *ctxVerifier) VerifyAccessTokenContext(ctx context.Context, _ string) (*auth.Claims, error) {
	v.calls++
	v.gotIP = auth.ClientIPFrom(ctx)
	return nil, v.err
}

func getWithBearer(v tokenVerifier, remote, xff string, opts ...AuthOption) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(AuthMiddleware(v, nil, opts...))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer nxk_whatever")
	req.RemoteAddr = remote
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAuthMiddleware_APIKeyThrottledIs429(t *testing.T) {
	throttled := getWithBearer(&ctxVerifier{err: auth.ErrAPIKeyThrottled}, "203.0.113.9:4000", "")
	if throttled.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled verdict = %d %s, want 429", throttled.Code, throttled.Body.String())
	}
	if throttled.Header().Get("Retry-After") == "" || !strings.Contains(throttled.Body.String(), `"code":"RATE_LIMITED"`) {
		t.Fatalf("429 = %v %s, want Retry-After and the canonical RATE_LIMITED body", throttled.Header(), throttled.Body.String())
	}
	// Byte-identical to every other limiter's refusal: nothing in it says "this was an API key".
	ref := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(ref)
	writeRateLimited429(ginCtx, apiKeyThrottleRetryAfter)
	if throttled.Body.String() != ref.Body.String() {
		t.Fatalf("body %q differs from the shared 429 %q", throttled.Body.String(), ref.Body.String())
	}

	if w := getWithBearer(&ctxVerifier{err: auth.ErrAPIKeyInvalid}, "203.0.113.9:4000", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key = %d, want the usual 401", w.Code)
	}
}

func TestAuthMiddleware_HandsTheVerifierTheClientIP(t *testing.T) {
	_, proxy, _ := net.ParseCIDR("10.0.0.0/8")
	tests := []struct {
		name   string
		remote string
		xff    string
		opts   []AuthOption
		want   string
	}{
		{"direct peer", "203.0.113.9:4000", "", nil, "203.0.113.9"},
		{"X-Forwarded-For is not trusted by default", "203.0.113.9:4000", "1.2.3.4", nil, "203.0.113.9"},
		{"behind a trusted proxy, the real client", "10.1.1.1:4000", "198.51.100.7", []AuthOption{WithTrustedProxies([]*net.IPNet{proxy})}, "198.51.100.7"},
		{"a spoofed XFF from an untrusted peer is ignored", "203.0.113.9:4000", "198.51.100.7", []AuthOption{WithTrustedProxies([]*net.IPNet{proxy})}, "203.0.113.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &ctxVerifier{err: auth.ErrAPIKeyInvalid}
			getWithBearer(v, tt.remote, tt.xff, tt.opts...)
			if v.gotIP != tt.want {
				t.Fatalf("verifier saw client IP %q, want %q", v.gotIP, tt.want)
			}
		})
	}
}
