// Package metrics implements the Prometheus instrumentation surface (RM-38, OBS-1).
//
// Dependency choice: github.com/prometheus/client_golang is the OFFICIAL Prometheus client —
// it ships the DBStats, Go-runtime and process collectors we need out of the box (hand-rolling
// those three costs more code than the dep saves), and every scraper/dashboard understands its
// exposition format natively. Lighter alternatives were considered and rejected:
// VictoriaMetrics/metrics has no DBStats collector and non-standard histogram conventions;
// gin-metrics (the roadmap's first sketch) wraps client_golang anyway while adding opinions
// (its own routes/labels) we would fight. Standard library has no metrics primitive (expvar
// has no histograms/labels).
//
// Metric set v1:
//   - http_request_duration_seconds{method,route,status_class} — histogram. route is the gin
//     TEMPLATE (c.FullPath(), e.g. /api/v1/:entity/:id), NEVER the raw path: raw paths carry
//     ids/entity names and would explode label cardinality. status_class is "2xx".."5xx" (plus
//     "1xx"/"unknown") — per-code granularity is not worth 5× the series.
//   - http_requests_in_flight — gauge of currently-executing requests.
//   - go_sql_* DBStats gauges for BOTH pools (GORM sys_* and sqlx obj_*), distinguished by the
//     db_name label (RegisterDBPool).
//   - Go runtime (go_*) + process (process_*) collectors.
//
// The /metrics endpoint is minimally protected by METRICS_BEARER_TOKEN (see Handler): empty
// token (default) = endpoint answers 404 (fail-closed OFF); set = requires
// `Authorization: Bearer <token>`. Metrics leak topology (routes, DB pool sizing, request
// volume) so an unauthenticated exposure is not acceptable even on an "internal" port.
package metrics

import (
	"crypto/subtle"
	"database/sql"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns a dedicated Prometheus registry plus the HTTP-level instruments. A dedicated
// registry (not prometheus.DefaultRegisterer) keeps tests hermetic — two Metrics instances
// never collide on duplicate registration — at zero extra cost in main.
type Metrics struct {
	registry *prometheus.Registry
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

// New builds the registry with the HTTP instruments plus the Go-runtime and process collectors.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by method, route template and status class.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route", "status_class"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Number of HTTP requests currently being served.",
		}),
	}
	m.registry.MustRegister(
		m.duration,
		m.inFlight,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// RegisterDBPool exposes sql.DBStats gauges (open/idle/in-use conns, wait counts…) for one
// connection pool under the given db_name label. Called twice in main: once for the GORM
// (sys_*) pool and once for the sqlx (obj_*) pool — RM-20 sized both, this makes both visible.
func (m *Metrics) RegisterDBPool(db *sql.DB, name string) {
	m.registry.MustRegister(collectors.NewDBStatsCollector(db, name))
}

// Middleware instruments every request: in-flight gauge up/down plus one histogram observation
// labeled (method, route template, status class). Mounted BEFORE Recovery so a panic converted
// to 500 by the inner recovery is observed with status_class="5xx" (the observation runs after
// the inner chain returned normally — recovery already wrote the 500). The observation is
// deferred so the in-flight gauge can never leak even if something outside Recovery's cover
// panics.
func (m *Metrics) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		m.inFlight.Inc()
		defer func() {
			m.inFlight.Dec()
			route := c.FullPath() // template, not raw path — bounded cardinality
			if route == "" {
				route = "unmatched" // single bucket for every 404 no-route probe
			}
			m.duration.WithLabelValues(c.Request.Method, route, statusClass(c.Writer.Status())).
				Observe(time.Since(start).Seconds())
		}()
		c.Next()
	}
}

// Handler serves GET /metrics, gated by bearerToken:
//   - bearerToken == "" (default): 404 — the endpoint is OFF, indistinguishable from an
//     unregistered route (fail-closed; nothing to probe).
//   - wrong/missing Authorization: 401 (constant-time compare, no token echo).
//   - exact "Bearer <token>": the Prometheus exposition for this registry.
func (m *Metrics) Handler(bearerToken string) gin.HandlerFunc {
	promHandler := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return func(c *gin.Context) {
		if bearerToken == "" {
			c.String(http.StatusNotFound, "404 page not found")
			return
		}
		expected := "Bearer " + bearerToken
		got := c.GetHeader("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Yêu cầu xác thực không hợp lệ",
				"code":  "UNAUTHORIZED",
			})
			return
		}
		promHandler.ServeHTTP(c.Writer, c.Request)
	}
}

// statusClass folds an HTTP status code into its class label ("2xx".."5xx").
func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	case status >= 100:
		return "1xx"
	default:
		return "unknown"
	}
}
