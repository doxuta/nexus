package metrics

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
)

// newMetricsRouter builds an engine mirroring production: the instrumentation middleware plus
// GET /metrics gated by token, and one instrumented business route.
func newMetricsRouter(m *Metrics, token string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(m.Middleware())
	r.GET("/metrics", m.Handler(token))
	r.GET("/api/v1/:entity", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func doGet(r *gin.Engine, path, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestMetrics_TokenGate is the table-driven gate check: empty configured token → 404
// (fail-closed OFF); wrong/missing bearer → 401; exact bearer → 200 exposition.
func TestMetrics_TokenGate(t *testing.T) {
	tests := []struct {
		name        string
		configured  string
		sentBearer  string
		wantStatus  int
		wantMetrics bool
	}{
		{"empty token = endpoint off (404)", "", "anything", http.StatusNotFound, false},
		{"empty token + no auth = 404 too", "", "", http.StatusNotFound, false},
		{"wrong token = 401", "s3cret", "wrong", http.StatusUnauthorized, false},
		{"missing auth = 401", "s3cret", "", http.StatusUnauthorized, false},
		{"right token = 200 + exposition", "s3cret", "s3cret", http.StatusOK, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newMetricsRouter(New(), tt.configured)
			w := doGet(r, "/metrics", tt.sentBearer)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			gotMetrics := strings.Contains(w.Body.String(), "http_requests_in_flight")
			if gotMetrics != tt.wantMetrics {
				t.Errorf("exposition present = %v, want %v (body: %.120s)", gotMetrics, tt.wantMetrics, w.Body.String())
			}
		})
	}
}

// TestMetrics_HistogramObservesLabeledRequest: hitting an instrumented route produces one
// observation under (method=GET, route=TEMPLATE, status_class=2xx) — the raw path with the
// entity name must NOT appear as a label value (cardinality guard).
func TestMetrics_HistogramObservesLabeledRequest(t *testing.T) {
	m := New()
	r := newMetricsRouter(m, "tok")

	if w := doGet(r, "/api/v1/accounts", ""); w.Code != http.StatusOK {
		t.Fatalf("business route: expected 200, got %d", w.Code)
	}

	body := doGet(r, "/metrics", "tok").Body.String()
	want := `http_request_duration_seconds_count{method="GET",route="/api/v1/:entity",status_class="2xx"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("exposition missing %q", want)
	}
	if strings.Contains(body, `route="/api/v1/accounts"`) {
		t.Error("raw path leaked into the route label — must be the gin template")
	}
}

// TestMetrics_UnmatchedRouteSingleBucket: 404 no-route requests collapse into ONE label value
// ("unmatched") instead of one series per probed path.
func TestMetrics_UnmatchedRouteSingleBucket(t *testing.T) {
	m := New()
	r := newMetricsRouter(m, "tok")

	doGet(r, "/nope-1", "")
	doGet(r, "/nope-2", "")

	body := doGet(r, "/metrics", "tok").Body.String()
	want := `http_request_duration_seconds_count{method="GET",route="unmatched",status_class="4xx"} 2`
	if !strings.Contains(body, want) {
		t.Errorf("exposition missing %q", want)
	}
	if strings.Contains(body, "nope-1") {
		t.Error("probed 404 path leaked into a label value")
	}
}

// TestMetrics_DBPoolGauges: both pools appear under distinct db_name labels. sql.Open does not
// dial, and the DBStats collector only reads Stats(), so no live MySQL is needed.
func TestMetrics_DBPoolGauges(t *testing.T) {
	m := New()
	gormPool, err := sql.Open("mysql", "root:@tcp(127.0.0.1:0)/none")
	if err != nil {
		t.Fatalf("sql.Open gorm stand-in: %v", err)
	}
	defer gormPool.Close()
	sqlxPool, err := sql.Open("mysql", "root:@tcp(127.0.0.1:0)/none")
	if err != nil {
		t.Fatalf("sql.Open sqlx stand-in: %v", err)
	}
	defer sqlxPool.Close()

	m.RegisterDBPool(gormPool, "gorm_sys")
	m.RegisterDBPool(sqlxPool, "sqlx_obj")

	r := newMetricsRouter(m, "tok")
	body := doGet(r, "/metrics", "tok").Body.String()
	for _, label := range []string{`db_name="gorm_sys"`, `db_name="sqlx_obj"`} {
		if !strings.Contains(body, label) {
			t.Errorf("exposition missing DB pool gauges for %s", label)
		}
	}
}

// TestStatusClass covers the fold including the buckets alerting keys on.
func TestStatusClass(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{200, "2xx"}, {201, "2xx"}, {301, "3xx"}, {404, "4xx"}, {422, "4xx"},
		{500, "5xx"}, {503, "5xx"}, {101, "1xx"}, {0, "unknown"},
	}
	for _, tt := range tests {
		if got := statusClass(tt.status); got != tt.want {
			t.Errorf("statusClass(%d) = %q, want %q", tt.status, got, tt.want)
		}
	}
}
