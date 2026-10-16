package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// newRequestIDRouter mounts RequestID plus a probe handler that reports what
// RequestIDFromContext saw, so tests assert both the header echo and the context stash.
func newRequestIDRouter(t *testing.T) (*gin.Engine, *string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var seen string
	r := gin.New()
	r.Use(RequestID())
	r.GET("/ping", func(c *gin.Context) {
		id, ok := RequestIDFromContext(c)
		if !ok {
			t.Error("RequestIDFromContext: expected an id in context, got none")
		}
		seen = id
		c.Status(http.StatusOK)
	})
	return r, &seen
}

func doRequestID(r *gin.Engine, inbound string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	if inbound != "" {
		req.Header.Set(HeaderRequestID, inbound)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestRequestID_HonorsValidInbound: a well-formed inbound id survives end-to-end — same value
// in the response header and in the context accessor.
func TestRequestID_HonorsValidInbound(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"uuid", "550e8400-e29b-41d4-a716-446655440000"},
		{"alnum with dash and underscore", "gw-7_ReQ-42"},
		{"single char", "a"},
		{"max length 64", strings.Repeat("x", 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, seen := newRequestIDRouter(t)
			w := doRequestID(r, tt.id)
			if got := w.Header().Get(HeaderRequestID); got != tt.id {
				t.Errorf("response header = %q, want inbound id %q", got, tt.id)
			}
			if *seen != tt.id {
				t.Errorf("context id = %q, want inbound id %q", *seen, tt.id)
			}
		})
	}
}

// TestRequestID_RegeneratesInvalid: a malformed inbound id is NEVER reflected — a fresh UUID
// replaces it in both the header and the context (the id lands in logs and response headers,
// so garbage/injection payloads must not pass through).
func TestRequestID_RegeneratesInvalid(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"absent", ""},
		{"too long 65", strings.Repeat("x", 65)},
		{"space", "abc def"},
		{"newline injection", "abc\ndef"},
		{"non-ascii", "réq-1"},
		{"header-ish payload", "x: evil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, seen := newRequestIDRouter(t)
			w := doRequestID(r, tt.id)
			got := w.Header().Get(HeaderRequestID)
			if got == tt.id {
				t.Fatalf("invalid inbound id %q was reflected verbatim", tt.id)
			}
			if _, err := uuid.Parse(got); err != nil {
				t.Errorf("regenerated id %q is not a UUID: %v", got, err)
			}
			if *seen != got {
				t.Errorf("context id %q != response header id %q", *seen, got)
			}
		})
	}
}

// TestRequestIDFromContext_AbsentFailsClosed: a request that skipped the middleware reports
// ("", false) — downstream log enrichment then simply omits the field, never panics.
func TestRequestIDFromContext_AbsentFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if id, ok := RequestIDFromContext(c); ok || id != "" {
		t.Errorf("expected (\"\", false) without middleware, got (%q, %v)", id, ok)
	}
}
