package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// newAccessLogRouter mirrors the production onion (RequestID → AccessLog → …) over an observer
// core so tests assert the exact fields emitted. A claims seeder mimics AuthMiddleware placing
// the verified tenant into context AFTER the access-log middleware mounted (the production
// ordering — the tenant must still appear in the log line because it is read post-c.Next()).
func newAccessLogRouter() (*gin.Engine, *observer.ObservedLogs) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(RequestID())
	r.Use(AccessLog(zap.New(core)))
	r.Use(func(c *gin.Context) { // stands in for AuthMiddleware, mounted downstream
		if tid := c.GetHeader("X-Test-Tenant"); tid != "" {
			c.Set(ctxKeyTenantID, tid)
		}
		c.Next()
	})
	r.GET("/api/v1/:entity", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/boom", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })
	r.GET("/health/live", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/metrics", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r, logs
}

func fieldString(t *testing.T, entry observer.LoggedEntry, key string) string {
	t.Helper()
	for _, f := range entry.Context {
		if f.Key == key {
			return f.String
		}
	}
	t.Fatalf("field %q missing from log entry %v", key, entry.Context)
	return ""
}

// TestAccessLog_FieldsPresent: one Info entry per request with route TEMPLATE (not the raw
// path), status, request_id and tenant_id (read after handlers ran).
func TestAccessLog_FieldsPresent(t *testing.T) {
	r, logs := newAccessLogRouter()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	req.Header.Set(HeaderRequestID, "req-123")
	req.Header.Set("X-Test-Tenant", "tenant-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 access-log entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Message != "http_request" {
		t.Errorf("message = %q, want http_request", e.Message)
	}
	if got := fieldString(t, e, "route"); got != "/api/v1/:entity" {
		t.Errorf("route = %q, want the template /api/v1/:entity (never the raw path)", got)
	}
	if got := fieldString(t, e, "method"); got != http.MethodGet {
		t.Errorf("method = %q, want GET", got)
	}
	if got := fieldString(t, e, "request_id"); got != "req-123" {
		t.Errorf("request_id = %q, want req-123", got)
	}
	if got := fieldString(t, e, "tenant_id"); got != "tenant-a" {
		t.Errorf("tenant_id = %q, want tenant-a (read post-handlers)", got)
	}
	var status int64 = -1
	for _, f := range e.Context {
		if f.Key == "status" {
			status = f.Integer
		}
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
}

// TestAccessLog_StatusRecorded: a non-200 handler status lands in the entry (the field the 5xx
// alerting pipeline keys on).
func TestAccessLog_StatusRecorded(t *testing.T) {
	r, logs := newAccessLogRouter()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom", nil))

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	for _, f := range entries[0].Context {
		if f.Key == "status" && f.Integer != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", f.Integer)
		}
	}
}

// TestAccessLog_SkipsProbesAndMetrics: /health, /health/*, /metrics produce NO access-log line
// (probes fire every few seconds and would drown the stream).
func TestAccessLog_SkipsProbesAndMetrics(t *testing.T) {
	r, logs := newAccessLogRouter()
	for _, path := range []string{"/health/live", "/health", "/metrics"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, w.Code)
		}
	}
	if n := logs.Len(); n != 0 {
		t.Errorf("expected 0 access-log entries for probe/metrics paths, got %d", n)
	}
}

// TestAccessLog_NilLoggerNoop: a nil logger must not panic (defensive parity with the other
// middlewares in this package).
func TestAccessLog_NilLoggerNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(AccessLog(nil))
	r.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with nil logger, got %d", w.Code)
	}
}
