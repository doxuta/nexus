package config

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func newCORSTestEngine(allowedOrigins []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(cors.New(BuildCORSConfig(allowedOrigins)))
	r.OPTIONS("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	r.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

// TestCORS_AllowedOrigin_Preflight verifies that an OPTIONS preflight from an explicitly
// allowed origin receives Access-Control-Allow-Origin in the response (RM-04).
func TestCORS_AllowedOrigin_Preflight(t *testing.T) {
	allowed := "https://app.nexus.io"
	r := newCORSTestEngine([]string{allowed})

	req := httptest.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", allowed)
	req.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	acao := w.Header().Get("Access-Control-Allow-Origin")
	if acao != allowed {
		t.Errorf("expected ACAO=%q, got %q (status %d)", allowed, acao, w.Code)
	}
}

// specs/063-draft-publish T022: the browser sends X-Nexus-Draft on the builder's preview reads; a
// preflight that asks for it must be answered with it allowed, or the cross-origin FE never sees drafts.
func TestCORS_Preflight_AllowsDraftHeader(t *testing.T) {
	allowed := "https://app.nexus.io"
	r := newCORSTestEngine([]string{allowed})
	req := httptest.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", allowed)
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization,x-nexus-draft")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if got := strings.ToLower(w.Header().Get("Access-Control-Allow-Headers")); !strings.Contains(got, "x-nexus-draft") {
		t.Errorf("Access-Control-Allow-Headers = %q, want x-nexus-draft allowed", got)
	}
}

// TestCORS_DisallowedOrigin_Preflight verifies that a preflight from an origin NOT in the
// allow-list does NOT receive a matching Access-Control-Allow-Origin header (RM-04).
func TestCORS_DisallowedOrigin_Preflight(t *testing.T) {
	r := newCORSTestEngine([]string{"https://app.nexus.io"})

	req := httptest.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	acao := w.Header().Get("Access-Control-Allow-Origin")
	if acao == "https://evil.example.com" {
		t.Error("disallowed origin should NOT receive ACAO header")
	}
}

// TestCORS_EmptyOrigins_FailClosed verifies that an empty allowed-origins list results in
// no origin being echoed back (fail-closed, RM-04).
func TestCORS_EmptyOrigins_FailClosed(t *testing.T) {
	r := newCORSTestEngine(nil)

	req := httptest.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", "https://anything.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	acao := w.Header().Get("Access-Control-Allow-Origin")
	if acao == "https://anything.example.com" {
		t.Error("empty origins list should deny all origins (fail-closed)")
	}
}

// TestCORS_NeverWildcard verifies that AllowAllOrigins is never set to true
// (ACAO * combined with AllowCredentials=true violates the CORS spec, RM-04).
func TestCORS_NeverWildcard(t *testing.T) {
	cfg := BuildCORSConfig([]string{"https://app.nexus.io"})
	if cfg.AllowAllOrigins {
		t.Error("AllowAllOrigins must never be true (incompatible with AllowCredentials=true)")
	}
	if !cfg.AllowCredentials {
		t.Error("AllowCredentials must be true for cookie/auth header flows")
	}
}

// specs/073 so1b: the Studio board sends PATCH /admin/projects/:tenantId (trial switch, industry). A cross-origin
// preflight asking for PATCH must be answered with it allowed, or the browser blocks the call before it leaves.
func TestCORS_Preflight_AllowsPatch(t *testing.T) {
	allowed := "https://app.nexus.io"
	r := newCORSTestEngine([]string{allowed})
	req := httptest.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", allowed)
	req.Header.Set("Access-Control-Request-Method", "PATCH")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if got := w.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "PATCH") {
		t.Errorf("Access-Control-Allow-Methods = %q, want PATCH allowed", got)
	}
}
