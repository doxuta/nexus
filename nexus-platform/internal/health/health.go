// Package health provides liveness and readiness probes for the Nexus API (W5-E2).
//
// Two endpoints, deliberately split:
//
//   - GET /health/live  — a static 200. The process is up and the HTTP server can answer; it
//     says nothing about dependencies. Orchestrators use it to decide whether to RESTART a pod.
//   - GET /health/ready — pings the database (SELECT 1) under a short timeout. 200 when the DB
//     answers in time, 503 (code DB_NOT_READY) when it is slow/down. Orchestrators and load
//     balancers use it to decide whether to ROUTE TRAFFIC to a pod, so a node with a hung DB is
//     taken out of rotation instead of stalling requests.
//
// The readiness ping reuses the same statement-timeout posture as the rest of the app
// (config.DBQueryTimeout): a hung MySQL makes /health/ready return 503 fast rather than hang.
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Pinger is the minimal dependency the readiness probe needs: a context-aware DB ping.
// *sqlx.DB and *sql.DB both satisfy it (PingContext). Declared consumer-side so the health
// package does not depend on a concrete driver type and is trivially faked in tests.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// defaultReadyTimeout bounds the readiness DB ping when the caller passes a non-positive value,
// so /health/ready can never hang on a wedged connection even if misconfigured.
const defaultReadyTimeout = 2 * time.Second

// Handler wires the /health/live and /health/ready routes. db is the readiness Pinger (the
// production sqlx pool); timeout bounds the readiness ping (falls back to defaultReadyTimeout
// when ≤ 0); logger may be nil.
type Handler struct {
	db      Pinger
	timeout time.Duration
	logger  *zap.Logger
}

// NewHandler builds a health Handler. A nil db disables the DB ping: /health/ready then reports
// ready (there is nothing to check) so a DB-less deployment is not wedged at "not ready".
func NewHandler(db Pinger, timeout time.Duration, logger *zap.Logger) *Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}
	return &Handler{db: db, timeout: timeout, logger: logger}
}

// RegisterRoutes mounts GET /health/live and GET /health/ready on rg. These are PUBLIC probes
// (no auth) and are mounted at the engine root, not under /api/v1, so a probe never needs a token.
func (h *Handler) RegisterRoutes(rg gin.IRoutes) {
	rg.GET("/health/live", h.Live)
	rg.GET("/health/ready", h.Ready)
}

// Live is the static liveness probe: always 200. It proves the process and HTTP server are up
// and does NOT touch the DB, so a slow DB never affects the restart decision.
func (h *Handler) Live(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "live"})
}

// Ready is the readiness probe: it pings the DB under h.timeout. 200 {status:ready} when the DB
// answers; 503 {status:not_ready, code:DB_NOT_READY} when the ping errors or times out. The
// timeout guarantees the handler returns fast on a hung DB instead of hanging the probe.
func (h *Handler) Ready(c *gin.Context) {
	if h.db == nil {
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	if err := h.db.PingContext(ctx); err != nil {
		h.logger.Warn("health.Ready: DB ping failed", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "not_ready",
			"code":   "DB_NOT_READY",
			"error":  "database not reachable",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}
