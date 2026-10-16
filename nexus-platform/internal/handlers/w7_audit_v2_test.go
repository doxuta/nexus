package handlers

// W7 audit v2 — handler-level unit tests over a FAKE reader/exporter (no DB): the new
// ?dateFrom/?dateTo calendar-day filters, the from/dateFrom mixing guard, the limit echo in
// the pagination envelope, and the export.csv content type + filter passthrough. The MySQL
// truth (NULL-diff scan regression, real trail) lives in the W7 integration suite.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
)

// w7FakeAuditReader records the filter it was called with and returns a fixed page.
type w7FakeAuditReader struct {
	got  audit.QueryFilter
	rows []audit.AuditRow
}

func (f *w7FakeAuditReader) Query(_ context.Context, _ string, q audit.QueryFilter) ([]audit.AuditRow, string, error) {
	f.got = q
	return f.rows, "", nil
}

// w7FakeExporter records the filter and writes a fixed body.
type w7FakeExporter struct {
	got audit.QueryFilter
}

func (f *w7FakeExporter) ExportCSV(_ context.Context, _ string, q audit.QueryFilter, w io.Writer) (int, error) {
	f.got = q
	_, err := w.Write([]byte("id,at\n"))
	return 0, err
}

// w7AuditTestRouter mounts the audit handler behind a stub that injects a verified tenant claim
// (the admin gate itself is proven by the integration suite; here we unit-test parsing).
func w7AuditTestRouter(reader AuditReader, exporter AuditExporter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		// Same injection pattern as admin_user_handler_test.go: a verified tenant claim.
		c.Set("tenantID", "tenant-w7")
		c.Next()
	})
	h := NewAuditHandler(reader, zap.NewNop())
	if exporter != nil {
		h.WithExporter(exporter)
	}
	h.RegisterRoutes(r.Group("/api/v1"))
	return r
}

func TestAuditList_DateParamsAndLimitEcho(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantStatus int
		check      func(t *testing.T, f audit.QueryFilter, body map[string]any)
	}{
		{
			name:       "dateFrom/dateTo become UTC day bounds (dateTo exclusive +1d)",
			query:      "?dateFrom=2026-07-01&dateTo=2026-07-02",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f audit.QueryFilter, _ map[string]any) {
				wantFrom := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
				wantToX := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
				if f.From == nil || !f.From.Equal(wantFrom) {
					t.Fatalf("From = %v; want %v", f.From, wantFrom)
				}
				if f.ToExclusive == nil || !f.ToExclusive.Equal(wantToX) {
					t.Fatalf("ToExclusive = %v; want %v", f.ToExclusive, wantToX)
				}
				if f.To != nil {
					t.Fatalf("To = %v; want nil (dateTo maps to the exclusive bound)", f.To)
				}
			},
		},
		{
			name:       "malformed dateFrom → 400",
			query:      "?dateFrom=2026-7-1",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "mixing from (RFC3339) and dateFrom → 400",
			query:      "?from=2026-07-01T00:00:00Z&dateFrom=2026-07-01",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "inverted dateFrom > dateTo → 400",
			query:      "?dateFrom=2026-07-05&dateTo=2026-07-01",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "limit above cap echoes the clamped value",
			query:      "?limit=9999",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, _ audit.QueryFilter, body map[string]any) {
				pg, _ := body["pagination"].(map[string]any)
				if got, _ := pg["limit"].(float64); int(got) != 200 {
					t.Fatalf("pagination.limit = %v; want 200 (clamped)", pg["limit"])
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &w7FakeAuditReader{}
			r := w7AuditTestRouter(reader, nil)
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/api/v1/audit-log"+tt.query, nil)
			r.ServeHTTP(w, req)
			if w.Code != tt.wantStatus {
				t.Fatalf("HTTP %d; want %d; body=%s", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.check != nil {
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				tt.check(t, reader.got, body)
			}
		})
	}
}

func TestAuditExportCSVHandler(t *testing.T) {
	t.Run("streams text/csv with the list's filters", func(t *testing.T) {
		exp := &w7FakeExporter{}
		r := w7AuditTestRouter(&w7FakeAuditReader{}, exp)
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v1/audit-log/export.csv?entityName=accounts&action=update&dateTo=2026-07-02", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP %d; want 200; body=%s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
			t.Fatalf("Content-Type = %q; want text/csv", ct)
		}
		if cd := w.Header().Get("Content-Disposition"); cd == "" {
			t.Fatalf("missing Content-Disposition attachment header")
		}
		if exp.got.EntityName != "accounts" || exp.got.Action != "update" || exp.got.ToExclusive == nil {
			t.Fatalf("export filter drifted from the list parser: %+v", exp.got)
		}
	})

	t.Run("unwired exporter → 500 AUDIT_EXPORT_UNAVAILABLE, not 404", func(t *testing.T) {
		r := w7AuditTestRouter(&w7FakeAuditReader{}, nil)
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v1/audit-log/export.csv", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("HTTP %d; want 500; body=%s", w.Code, w.Body.String())
		}
	})
}

// specs/062-customer-workspace T071 — ?actor=builder ("Người dựng Nexus") reaches the SAME QueryFilter on
// the list and the CSV export (one parser), an unknown value is a 400 (a filter that silently shows
// everything is worse than an error), and absent means "everyone".
func TestAuditActorBuilderFilter(t *testing.T) {
	for _, route := range []string{"/api/v1/audit-log", "/api/v1/audit-log/export.csv"} {
		t.Run(route, func(t *testing.T) {
			reader, exp := &w7FakeAuditReader{}, &w7FakeExporter{}
			r := w7AuditTestRouter(reader, exp)
			got := func() audit.QueryFilter {
				if route == "/api/v1/audit-log" {
					return reader.got
				}
				return exp.got
			}
			for _, tc := range []struct {
				query       string
				wantStatus  int
				wantBuilder bool
			}{
				{"", http.StatusOK, false},
				{"?actor=builder", http.StatusOK, true},
				{"?actor=", http.StatusOK, false},
				{"?actor=everyone", http.StatusBadRequest, false},
			} {
				*reader, *exp = w7FakeAuditReader{}, w7FakeExporter{}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest("GET", route+tc.query, nil))
				if w.Code != tc.wantStatus {
					t.Fatalf("%q: HTTP %d; want %d; body=%s", tc.query, w.Code, tc.wantStatus, w.Body.String())
				}
				if tc.wantStatus == http.StatusOK && got().BuilderOnly != tc.wantBuilder {
					t.Fatalf("%q: BuilderOnly = %v; want %v", tc.query, got().BuilderOnly, tc.wantBuilder)
				}
			}
		})
	}
}
