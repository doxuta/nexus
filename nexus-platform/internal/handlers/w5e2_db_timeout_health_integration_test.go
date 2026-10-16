//go:build integration

// W5-E2 integration tests: statement-timeout (DSN readTimeout) + /health/ready DB-ping.
// Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// Requires a reachable MySQL 8 (docker-compose dev DB). The suite SKIPS (not fails) when MySQL
// is unreachable so it degrades gracefully off-CI.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"nexus/platform/internal/config"
	"nexus/platform/internal/db"
	"nexus/platform/internal/health"
)

// TestW5E2_QueryTimeout_FailsFastNotHang proves the DSN readTimeout (config.ApplyDBTimeouts)
// aborts a slow query with a driver i/o-timeout that db.IsTimeout classifies — and that it
// returns near the timeout, NOT after the full sleep. This is the repro for workflow#28: a slow
// MySQL must fail fast (→ 503 DB_TIMEOUT at the handler), not pile up requests until they hang.
func TestW5E2_QueryTimeout_FailsFastNotHang(t *testing.T) {
	// A 1s readTimeout against a 3s server-side SLEEP: the driver must give up at ~1s.
	timedDSN := config.ApplyDBTimeouts(dsn(), 1*time.Second, 2*time.Second)
	sx, err := sqlx.Connect("mysql", timedDSN)
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping W5-E2 query-timeout integration", err)
	}
	defer sx.Close()

	start := time.Now()
	var dummy int
	// SELECT SLEEP(3) blocks server-side 3s; the 1s readTimeout fires first.
	err = sx.GetContext(context.Background(), &dummy, "SELECT SLEEP(3)")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected a timeout error from SELECT SLEEP(3) under a 1s readTimeout; got nil after %v", elapsed)
	}
	if !db.IsTimeout(err) {
		t.Fatalf("db.IsTimeout = false for %v; want true (a wire i/o timeout must classify as DB timeout)", err)
	}
	// Fail-fast: returned because the readTimeout fired (~1s), not after the full 3s sleep.
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("query took %v; expected to abort near the 1s readTimeout, not hang to the 3s sleep", elapsed)
	}
}

// TestW5E2_HealthReady_LiveDB_200 confirms /health/ready returns 200 ready against a live DB.
func TestW5E2_HealthReady_LiveDB_200(t *testing.T) {
	sx, err := sqlx.Connect("mysql", dsn())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping W5-E2 health/ready live integration", err)
	}
	defer sx.Close()

	r := newHealthRouter(health.NewHandler(sx, 2*time.Second, zap.NewNop()))
	w := getReq(t, r, "/health/ready")
	if w.Code != http.StatusOK {
		t.Fatalf("/health/ready (live db) = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if body := decode(t, w); body["status"] != "ready" {
		t.Errorf("status = %v, want ready", body["status"])
	}
}

// TestW5E2_HealthReady_DownDB_503 confirms /health/ready returns 503 DB_NOT_READY when the DB is
// unreachable — here simulated by closing the pool before the probe, so PingContext errors.
func TestW5E2_HealthReady_DownDB_503(t *testing.T) {
	sx, err := sqlx.Connect("mysql", dsn())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping W5-E2 health/ready down integration", err)
	}
	// Close the pool so every subsequent PingContext fails (DB "down" from the app's view).
	_ = sx.Close()

	r := newHealthRouter(health.NewHandler(sx, 2*time.Second, zap.NewNop()))
	w := getReq(t, r, "/health/ready")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/health/ready (down db) = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if body := decode(t, w); body["code"] != "DB_NOT_READY" {
		t.Errorf("code = %v, want DB_NOT_READY", body["code"])
	}
}

// TestW5E2_HealthReady_SlowDB_503NotHang confirms a hung/slow DB makes /health/ready return 503
// promptly (the readiness timeout fires) rather than hanging the probe. We point the readiness
// pool at a tiny readTimeout and an unroutable host so the dial/read blocks, then bound the wait.
func TestW5E2_HealthReady_SlowDB_503NotHang(t *testing.T) {
	// Sanity: ensure MySQL is reachable on the normal DSN, else this environment has no DB and
	// the test is not meaningful (skip to stay consistent with the suite's degrade-off-CI rule).
	if probe, err := sqlx.Connect("mysql", dsn()); err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping W5-E2 health/ready slow integration", err)
	} else {
		_ = probe.Close()
	}

	// 198.51.100.10 is TEST-NET-2 (RFC 5737) — guaranteed unroutable, so a connect blocks until
	// the dial timeout. A 500ms dial timeout bounds the probe; readiness must 503, fast.
	deadDSN := config.ApplyDBTimeouts(
		"root:@tcp(198.51.100.10:3306)/nexus_platform?parseTime=True",
		500*time.Millisecond, 500*time.Millisecond,
	)
	sx, err := sqlx.Open("mysql", deadDSN) // Open (lazy) — no connection attempted yet.
	if err != nil {
		t.Fatalf("sqlx.Open dead DSN: %v", err)
	}
	defer sx.Close()

	r := newHealthRouter(health.NewHandler(sx, 1*time.Second, zap.NewNop()))
	start := time.Now()
	w := getReq(t, r, "/health/ready")
	elapsed := time.Since(start)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/health/ready (slow/unreachable db) = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("/health/ready took %v on an unreachable DB; expected to fail fast near the timeout, not hang", elapsed)
	}
}

// --- helpers -----------------------------------------------------------------------------------

func newHealthRouter(h *health.Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterRoutes(r)
	return r
}

func getReq(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return body
}
