// Package middleware — access_log.go implements the structured zap access log (RM-37, OBS-1).
// gin.Default()'s stdout logger wrote plain-text lines invisible to a structured log pipeline;
// this middleware emits ONE zap Info entry per request with the fields ops actually greps:
// method, route template, status, duration, request id, tenant id, client IP.
//
// Deliberate choices:
//   - route is c.FullPath() (the TEMPLATE, e.g. /api/v1/:entity/:id), read AFTER c.Next() so
//     gin has resolved the route. Unmatched requests (404 no-route) log route="" — the raw
//     path is NOT logged for them (an attacker-controlled string; the template is enough).
//   - tenant_id is read via TenantFromContext AFTER handlers ran: AuthMiddleware mounts later
//     in the chain, so the claim is only in context once the inner chain executed.
//   - request bodies are NEVER logged (PII).
//   - /health/* and /metrics are skipped: probes fire every few seconds and would drown the
//     signal (they are still observable via the Prometheus histogram if needed).
package middleware

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// AccessLog returns the zap access-log middleware. It must be mounted AFTER RequestID (to pick
// up the correlation id) and BEFORE Recovery (so a panic converted to 500 by the inner recovery
// still produces an access-log line with status=500). logger may be nil (no-op).
func AccessLog(logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/metrics" || path == "/health" || strings.HasPrefix(path, "/health/") {
			c.Next()
			return
		}

		start := time.Now()
		c.Next()

		fields := []zap.Field{
			zap.String("method", c.Request.Method),
			zap.String("route", c.FullPath()),
			zap.Int("status", c.Writer.Status()),
			zap.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000.0),
			zap.String("client_ip", c.ClientIP()),
		}
		if id, ok := RequestIDFromContext(c); ok {
			fields = append(fields, zap.String("request_id", id))
		}
		if tenantID, ok := TenantFromContext(c); ok {
			fields = append(fields, zap.String("tenant_id", tenantID))
		}
		logger.Info("http_request", fields...)
	}
}
