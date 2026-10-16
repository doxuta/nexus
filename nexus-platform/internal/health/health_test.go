package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// fakePinger is a Pinger whose PingContext returns a configured error (or blocks until the
// context deadline when block is true), letting the readiness tests drive the ok / down /
// slow paths without a real database.
type fakePinger struct {
	err   error
	block bool
}

func (f fakePinger) PingContext(ctx context.Context) error {
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func newTestRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterRoutes(r)
	return r
}

func do(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestLive_Always200 confirms /health/live is a static 200 that never touches the DB (the
// pinger here would error, but Live must not call it).
func TestLive_Always200(t *testing.T) {
	h := NewHandler(fakePinger{err: errors.New("db down")}, time.Second, nil)
	w := do(t, newTestRouter(h), "/health/live")
	if w.Code != http.StatusOK {
		t.Fatalf("/health/live = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestReady_DBOK returns 200 ready when the ping succeeds.
func TestReady_DBOK(t *testing.T) {
	h := NewHandler(fakePinger{err: nil}, time.Second, nil)
	w := do(t, newTestRouter(h), "/health/ready")
	if w.Code != http.StatusOK {
		t.Fatalf("/health/ready = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ready" {
		t.Errorf("status = %v, want ready", body["status"])
	}
}

// TestReady_DBDown returns 503 DB_NOT_READY when the ping errors.
func TestReady_DBDown(t *testing.T) {
	h := NewHandler(fakePinger{err: errors.New("connection refused")}, time.Second, nil)
	w := do(t, newTestRouter(h), "/health/ready")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/health/ready = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "DB_NOT_READY" {
		t.Errorf("code = %v, want DB_NOT_READY", body["code"])
	}
}

// TestReady_DBSlow_503NotHang confirms a hung DB makes /health/ready return 503 promptly
// (the readiness timeout fires) rather than hanging the probe. A short timeout bounds the wait.
func TestReady_DBSlow_503NotHang(t *testing.T) {
	h := NewHandler(fakePinger{block: true}, 100*time.Millisecond, nil)
	start := time.Now()
	w := do(t, newTestRouter(h), "/health/ready")
	elapsed := time.Since(start)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/health/ready (slow) = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	// Must have returned because the readiness timeout fired, not hung.
	if elapsed > time.Second {
		t.Fatalf("/health/ready took %v on a hung DB; expected to fail fast near the 100ms timeout", elapsed)
	}
}

// TestReady_NilDB_ReportsReady confirms a nil Pinger (DB-less deployment) reports ready rather
// than wedging at not-ready.
func TestReady_NilDB_ReportsReady(t *testing.T) {
	h := NewHandler(nil, time.Second, nil)
	w := do(t, newTestRouter(h), "/health/ready")
	if w.Code != http.StatusOK {
		t.Fatalf("/health/ready (nil db) = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestNewHandler_TimeoutFallback confirms a ≤0 timeout falls back to the default so the probe
// can never hang on a misconfigured zero timeout.
func TestNewHandler_TimeoutFallback(t *testing.T) {
	h := NewHandler(fakePinger{}, 0, nil)
	if h.timeout != defaultReadyTimeout {
		t.Errorf("timeout = %v, want fallback %v", h.timeout, defaultReadyTimeout)
	}
}
