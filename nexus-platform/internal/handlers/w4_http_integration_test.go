//go:build integration

// W4 HTTP-contract integration tests — the wire-level counterpart of the services suite
// (internal/services/w4_overlay_bucket_integration_test.go): the exact query params, error
// codes, envelope shapes and CSV headers the FE consumes, driven through the REAL router
// (AuthMiddleware → RequireTenant → handlers), against the shared MySQL from dsn().
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_platform?..." \
//	  go test -tags=integration ./internal/handlers/ -run TestW4HTTP -race -v
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

// w4Entity is the fixture entity for the HTTP suite. Cleaned per-test via cleanupNamed.
const w4Entity = "it_w4_deals"

// w4OtherTenant is a different tenant used for the isolation probes.
const w4OtherTenant = "abcdef00-0000-0000-0000-0000000000f4"

// Fixed owner ids so ?ownerId narrowing is testable (tokenForTenant mints a random user per
// request — records created through it scatter created_by).
const (
	w4UserA = "44444444-aaaa-0000-0000-0000000000aa"
	w4UserB = "44444444-bbbb-0000-0000-0000000000bb"
)

// doAsUser performs a request authenticated as a SPECIFIC (tenant, user) principal, unlike
// do()/doAs() which mint a random user id per request.
func doAsUser(t *testing.T, r *gin.Engine, method, path, tenant, userID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		reader = bytes.NewReader(buf)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	tok, err := accessVerifier(t).IssueAccessToken(auth.Subject{UserID: userID, TenantID: tenant, IsAdmin: true})
	if err != nil {
		t.Fatalf("doAsUser: issue token: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestW4HTTP_DashboardRunFilters(t *testing.T) {
	r, sx := newFullRouter(t)
	clean := func() {
		sx.Exec(`DELETE FROM sys_dashboards WHERE tenant_id IN (?, ?)`, testTenant, w4OtherTenant)
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name = ?`, w4OtherTenant, w4Entity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, w4OtherTenant, w4Entity)
		cleanupNamed(sx, w4Entity)
	}
	clean()
	t.Cleanup(clean)

	// Entity + 3 records with pinned created_at and fixed owners.
	createBody := map[string]any{
		"name":  w4Entity,
		"label": "W4 Deals",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "fieldType": "number"},
			{"name": "stage", "label": "Stage", "fieldType": "text", "options": map[string]any{"indexed": true}},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", createBody); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}
	type seedRow struct {
		amount    float64
		stage     string
		createdAt string
		owner     string
	}
	for _, row := range []seedRow{
		{100, "open", "2026-01-30 10:00:00", w4UserA},
		{200, "open", "2026-01-31 10:00:00", w4UserA},
		{300, "won", "2026-02-01 10:00:00", w4UserB},
	} {
		w := doAsUser(t, r, http.MethodPost, "/api/v1/"+w4Entity, testTenant, row.owner,
			map[string]any{"amount": row.amount, "stage": row.stage})
		if w.Code != http.StatusCreated {
			t.Fatalf("seed record: %d %s", w.Code, w.Body.String())
		}
		var created struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &created)
		if _, err := sx.Exec("UPDATE `obj_"+w4Entity+"` SET `created_at` = ? WHERE `id` = ?", row.createdAt, created.Data.ID); err != nil {
			t.Fatalf("pin created_at: %v", err)
		}
	}

	// Dashboard with one KPI count widget.
	w := do(t, r, http.MethodPost, "/api/v1/admin/dashboards", map[string]any{
		"name": "W4 dash",
		"widgets": []map[string]any{
			{"id": "w1", "title": "Deals", "type": "kpi", "entity": w4Entity, "metric": "count"},
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create dashboard: %d %s", w.Code, w.Body.String())
	}
	var dash struct {
		Data struct {
			Dashboard struct {
				ID string `json:"id"`
			} `json:"dashboard"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &dash)
	dashID := dash.Data.Dashboard.ID

	type runResp struct {
		Data struct {
			Widgets []struct {
				Result *struct {
					Grouped bool  `json:"grouped"`
					Count   int64 `json:"count"`
				} `json:"result"`
				Code string `json:"code"`
			} `json:"widgets"`
			AppliedFilters map[string]string `json:"appliedFilters"`
		} `json:"data"`
	}
	runCount := func(query string) (int64, runResp) {
		t.Helper()
		w := do(t, r, http.MethodGet, "/api/v1/dashboards/"+dashID+"/run"+query, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("run %q: %d %s", query, w.Code, w.Body.String())
		}
		var res runResp
		json.Unmarshal(w.Body.Bytes(), &res)
		if len(res.Data.Widgets) != 1 || res.Data.Widgets[0].Result == nil {
			t.Fatalf("run %q: unexpected widgets %s", query, w.Body.String())
		}
		return res.Data.Widgets[0].Result.Count, res
	}

	// Backward compatible: no params → all 3; no appliedFilters key.
	if n, res := runCount(""); n != 3 || res.Data.AppliedFilters != nil {
		t.Errorf("plain run: count=%d appliedFilters=%v; want 3 / absent", n, res.Data.AppliedFilters)
	}
	// dateFrom+dateTo narrows to the boundary pair (Jan 31 + Feb 1) and echoes appliedFilters.
	if n, res := runCount("?dateFrom=2026-01-31&dateTo=2026-02-01"); n != 2 ||
		res.Data.AppliedFilters["dateFrom"] != "2026-01-31" || res.Data.AppliedFilters["dateTo"] != "2026-02-01" {
		t.Errorf("range run: count=%d applied=%v; want 2 + echo", n, res.Data.AppliedFilters)
	}
	// ownerId narrows to w4UserA's 2 rows.
	if n, _ := runCount("?ownerId=" + w4UserA); n != 2 {
		t.Errorf("owner run: count=%d; want 2", n)
	}
	// Validation: malformed date → 422 RUN_DATE_INVALID; inverted range → 422 RUN_DATE_RANGE_INVALID.
	if w := do(t, r, http.MethodGet, "/api/v1/dashboards/"+dashID+"/run?dateFrom=31-01-2026", nil); w.Code != http.StatusUnprocessableEntity ||
		errCodeOf(w.Body.Bytes()) != "RUN_DATE_INVALID" {
		t.Errorf("bad dateFrom: %d %s; want 422 RUN_DATE_INVALID", w.Code, w.Body.String())
	}
	if w := do(t, r, http.MethodGet, "/api/v1/dashboards/"+dashID+"/run?dateFrom=2026-02-01&dateTo=2026-01-01", nil); w.Code != http.StatusUnprocessableEntity ||
		errCodeOf(w.Body.Bytes()) != "RUN_DATE_RANGE_INVALID" {
		t.Errorf("inverted range: %d %s; want 422 RUN_DATE_RANGE_INVALID", w.Code, w.Body.String())
	}
	// Tenant isolation: another tenant + wide overlay → 404 (id invisible).
	if w := doAs(t, r, http.MethodGet, "/api/v1/dashboards/"+dashID+"/run?dateFrom=2020-01-01&dateTo=2030-01-01", w4OtherTenant, nil); w.Code != http.StatusNotFound {
		t.Errorf("other-tenant run: %d; want 404", w.Code)
	}

	// Config-time widget bucket validation over the wire (B6 parity).
	badBucket := map[string]any{
		"name": "bad", "widgets": []map[string]any{
			{"id": "w1", "title": "x", "type": "bar", "entity": w4Entity, "metric": "count",
				"groupBy": "created_at", "bucket": "fortnight"},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/dashboards", badBucket); w.Code != http.StatusUnprocessableEntity ||
		errCodeOf(w.Body.Bytes()) != "WIDGET_BUCKET_INVALID" {
		t.Errorf("bad widget bucket: %d %s; want 422 WIDGET_BUCKET_INVALID", w.Code, w.Body.String())
	}
	noGroup := map[string]any{
		"name": "bad2", "widgets": []map[string]any{
			{"id": "w1", "title": "x", "type": "bar", "entity": w4Entity, "metric": "count", "bucket": "month"},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/dashboards", noGroup); w.Code != http.StatusUnprocessableEntity ||
		errCodeOf(w.Body.Bytes()) != "WIDGET_BUCKET_REQUIRES_GROUPBY" {
		t.Errorf("bucket w/o groupBy: %d %s; want 422 WIDGET_BUCKET_REQUIRES_GROUPBY", w.Code, w.Body.String())
	}

	// A bucketed widget runs end-to-end: groups keyed by month, chronological.
	w = do(t, r, http.MethodPost, "/api/v1/admin/dashboards", map[string]any{
		"name": "W4 series",
		"widgets": []map[string]any{
			{"id": "w1", "title": "By month", "type": "line", "entity": w4Entity, "metric": "count",
				"groupBy": "created_at", "bucket": "month"},
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create bucketed dashboard: %d %s", w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &dash)
	w = do(t, r, http.MethodGet, "/api/v1/dashboards/"+dash.Data.Dashboard.ID+"/run", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("run bucketed: %d %s", w.Code, w.Body.String())
	}
	var series struct {
		Data struct {
			Widgets []struct {
				Result *struct {
					Grouped bool `json:"grouped"`
					Groups  []struct {
						Key   *string `json:"key"`
						Count int64   `json:"count"`
					} `json:"groups"`
				} `json:"result"`
				Code string `json:"code"`
			} `json:"widgets"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &series)
	sw := series.Data.Widgets[0]
	if sw.Result == nil || !sw.Result.Grouped || len(sw.Result.Groups) != 2 {
		t.Fatalf("bucketed widget = %s", w.Body.String())
	}
	if *sw.Result.Groups[0].Key != "2026-01" || sw.Result.Groups[0].Count != 2 ||
		*sw.Result.Groups[1].Key != "2026-02" || sw.Result.Groups[1].Count != 1 {
		t.Errorf("bucketed groups = %+v; want 2026-01/2 then 2026-02/1 (chronological)", sw.Result.Groups)
	}
}

func TestW4HTTP_AggregateBucketParams(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupNamed(sx, w4Entity)
	t.Cleanup(func() { cleanupNamed(sx, w4Entity) })

	createBody := map[string]any{
		"name":  w4Entity,
		"label": "W4 Deals",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "fieldType": "number"},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", createBody); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}
	pin := func(createdAt string, amount float64) {
		w := do(t, r, http.MethodPost, "/api/v1/"+w4Entity, map[string]any{"amount": amount})
		if w.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", w.Code, w.Body.String())
		}
		var created struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &created)
		sx.Exec("UPDATE `obj_"+w4Entity+"` SET `created_at` = ? WHERE `id` = ?", createdAt, created.Data.ID)
	}
	pin("2026-01-31 09:00:00", 100)
	pin("2026-02-01 09:00:00", 200)

	// Happy path: month bucket over created_at, echoed in the envelope.
	w := do(t, r, http.MethodGet, "/api/v1/"+w4Entity+"/aggregate?metric=sum&field=amount&groupBy=created_at&bucket=month", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("aggregate bucket: %d %s", w.Code, w.Body.String())
	}
	var agg struct {
		Data struct {
			Bucket string `json:"bucket"`
			Groups []struct {
				Key   *string `json:"key"`
				Value *string `json:"value"`
			} `json:"groups"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &agg)
	if agg.Data.Bucket != "month" || len(agg.Data.Groups) != 2 {
		t.Fatalf("aggregate bucket envelope = %s", w.Body.String())
	}
	if *agg.Data.Groups[0].Key != "2026-01" || !strings.HasPrefix(*agg.Data.Groups[0].Value, "100") ||
		*agg.Data.Groups[1].Key != "2026-02" || !strings.HasPrefix(*agg.Data.Groups[1].Value, "200") {
		t.Errorf("aggregate bucket groups = %+v; want Jan=100 then Feb=200", agg.Data.Groups)
	}

	// Error codes over the wire.
	for _, tc := range []struct{ query, wantCode string }{
		{"?metric=count&groupBy=created_at&bucket=decade", "AGGREGATE_BUCKET_INVALID"},
		{"?metric=count&bucket=month", "AGGREGATE_BUCKET_REQUIRES_GROUPBY"},
		{"?metric=count&groupBy=id&bucket=month", "AGGREGATE_BUCKET_FIELD_NOT_DATE"},
	} {
		w := do(t, r, http.MethodGet, "/api/v1/"+w4Entity+"/aggregate"+tc.query, nil)
		if w.Code != http.StatusUnprocessableEntity || errCodeOf(w.Body.Bytes()) != tc.wantCode {
			t.Errorf("aggregate %q: %d %s; want 422 %s", tc.query, w.Code, w.Body.String(), tc.wantCode)
		}
	}
}

func TestW4HTTP_ReportRunExportParity(t *testing.T) {
	r, sx := newFullRouter(t)
	clean := func() {
		sx.Exec(`DELETE FROM sys_reports WHERE tenant_id IN (?, ?) AND entity_name = ?`,
			testTenant, w4OtherTenant, w4Entity)
		cleanupNamed(sx, w4Entity)
	}
	clean()
	t.Cleanup(clean)

	createBody := map[string]any{
		"name":  w4Entity,
		"label": "W4 Deals",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "fieldType": "number"},
			{"name": "stage", "label": "Stage", "fieldType": "text", "options": map[string]any{"indexed": true}},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", createBody); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}
	seed := func(amount float64, stage, createdAt, owner string) {
		w := doAsUser(t, r, http.MethodPost, "/api/v1/"+w4Entity, testTenant, owner,
			map[string]any{"amount": amount, "stage": stage})
		if w.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", w.Code, w.Body.String())
		}
		var created struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &created)
		sx.Exec("UPDATE `obj_"+w4Entity+"` SET `created_at` = ? WHERE `id` = ?", createdAt, created.Data.ID)
	}
	seed(100, "open", "2026-01-30 10:00:00", w4UserA)
	seed(200, "open", "2026-01-31 10:00:00", w4UserA)
	seed(300, "won", "2026-02-01 10:00:00", w4UserB)

	// Tabular report over all stages.
	w := do(t, r, http.MethodPost, "/api/v1/admin/reports", map[string]any{
		"name": "W4 listing", "entityName": w4Entity, "type": "tabular",
		"config": map[string]any{"columns": []string{"stage", "amount"}},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create report: %d %s", w.Code, w.Body.String())
	}
	var rep struct {
		Data struct {
			Report struct {
				ID string `json:"id"`
			} `json:"report"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &rep)
	repID := rep.Data.Report.ID

	// Run with overlay: ownerId=w4UserA + dateTo=2026-01-30 → only the first row.
	params := "?dateTo=2026-01-30&ownerId=" + w4UserA
	w = do(t, r, http.MethodGet, "/api/v1/reports/"+repID+"/run"+params, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("run report: %d %s", w.Code, w.Body.String())
	}
	var run struct {
		Data struct {
			Total          int               `json:"total"`
			Rows           []map[string]any  `json:"rows"`
			AppliedFilters map[string]string `json:"appliedFilters"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &run)
	if run.Data.Total != 1 || len(run.Data.Rows) != 1 {
		t.Fatalf("run with overlay = %s; want exactly 1 row", w.Body.String())
	}
	if run.Data.AppliedFilters["ownerId"] != w4UserA || run.Data.AppliedFilters["dateTo"] != "2026-01-30" {
		t.Errorf("run appliedFilters = %v; want echo of both params", run.Data.AppliedFilters)
	}
	runRowID, _ := run.Data.Rows[0]["id"].(string)

	// Export with the SAME params → same single row (screen/file parity), CSV headers set.
	w = do(t, r, http.MethodGet, "/api/v1/reports/"+repID+"/export.csv"+params, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export report: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("export Content-Type = %q; want text/csv", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, w4Entity+"-report.csv") {
		t.Errorf("export Content-Disposition = %q; want %s-report.csv", cd, w4Entity)
	}
	rows := parseDownloadedCSV(t, w.Body.String())
	if len(rows) != 2 { // header + 1 row
		t.Fatalf("export rows = %d; want 2 (parity with run)\n%s", len(rows), w.Body.String())
	}
	// The report export carries the projected columns under their LABELS, and nothing else: the
	// raw id column the screen never shows is not in the file (see services.ExportCSV).
	if strings.Join(rows[0], ",") != "Stage,Amount" {
		t.Errorf("export header = %v; want the projected labels [Stage Amount]", rows[0])
	}
	// Row parity with the run, now proven on the projected VALUES rather than the dropped id.
	if rows[1][0] != run.Data.Rows[0]["stage"] || !decEqual(rows[1][1], fmt.Sprint(run.Data.Rows[0]["amount"])) {
		t.Errorf("export row = %v; want the run's row (stage=%v amount=%v, id %s)",
			rows[1], run.Data.Rows[0]["stage"], run.Data.Rows[0]["amount"], runRowID)
	}

	// Export WITHOUT params covers all 3 rows — the params really did narrow above.
	w = do(t, r, http.MethodGet, "/api/v1/reports/"+repID+"/export.csv", nil)
	rows = parseDownloadedCSV(t, w.Body.String())
	if len(rows) != 4 {
		t.Errorf("full export rows = %d; want 4 (header + 3)", len(rows))
	}

	// Overlay validation on both endpoints.
	if w := do(t, r, http.MethodGet, "/api/v1/reports/"+repID+"/run?dateFrom=junk", nil); w.Code != http.StatusUnprocessableEntity ||
		errCodeOf(w.Body.Bytes()) != "RUN_DATE_INVALID" {
		t.Errorf("run bad date: %d %s; want 422 RUN_DATE_INVALID", w.Code, w.Body.String())
	}
	if w := do(t, r, http.MethodGet, "/api/v1/reports/"+repID+"/export.csv?dateFrom=junk", nil); w.Code != http.StatusUnprocessableEntity ||
		errCodeOf(w.Body.Bytes()) != "RUN_DATE_INVALID" {
		t.Errorf("export bad date: %d %s; want 422 RUN_DATE_INVALID", w.Code, w.Body.String())
	}

	// Tenant isolation: tenant B gets 404 on run AND export of A's report id.
	if w := doAs(t, r, http.MethodGet, "/api/v1/reports/"+repID+"/run", w4OtherTenant, nil); w.Code != http.StatusUnprocessableEntity && w.Code != http.StatusNotFound {
		t.Errorf("other-tenant run: %d; want 404/422 REPORT_NOT_FOUND", w.Code)
	}
	w = doAs(t, r, http.MethodGet, "/api/v1/reports/"+repID+"/export.csv", w4OtherTenant, nil)
	if w.Code == http.StatusOK {
		t.Errorf("other-tenant export: 200; want error (isolation breach)")
	}

	// Report-config bucket validation over the wire: summary + bad bucket → 422.
	badSummary := map[string]any{
		"name": "bad", "entityName": w4Entity, "type": "summary",
		"config": map[string]any{"groupBy": "created_at", "metric": "count", "bucket": "decade"},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/reports", badSummary); w.Code != http.StatusUnprocessableEntity ||
		errCodeOf(w.Body.Bytes()) != "REPORT_CONFIG_INVALID" {
		t.Errorf("bad report bucket: %d %s; want 422 REPORT_CONFIG_INVALID", w.Code, w.Body.String())
	}

	// Summary + month bucket: run groups == export CSV rows.
	w = do(t, r, http.MethodPost, "/api/v1/admin/reports", map[string]any{
		"name": "W4 by month", "entityName": w4Entity, "type": "summary",
		"config": map[string]any{"groupBy": "created_at", "metric": "sum", "field": "amount", "bucket": "month"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create summary: %d %s", w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &rep)
	sumID := rep.Data.Report.ID

	w = do(t, r, http.MethodGet, "/api/v1/reports/"+sumID+"/run", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("run summary: %d %s", w.Code, w.Body.String())
	}
	var sumRun struct {
		Data struct {
			Groups []struct {
				Key   *string `json:"key"`
				Value *string `json:"value"`
				Count int64   `json:"count"`
			} `json:"groups"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &sumRun)
	if len(sumRun.Data.Groups) != 2 || *sumRun.Data.Groups[0].Key != "2026-01" || *sumRun.Data.Groups[1].Key != "2026-02" {
		t.Fatalf("summary run groups = %s; want [2026-01 2026-02]", w.Body.String())
	}
	if !decEqual(*sumRun.Data.Groups[0].Value, "300") || !decEqual(*sumRun.Data.Groups[1].Value, "300") {
		t.Errorf("summary sums = %v; want 300/300", sumRun.Data.Groups)
	}

	w = do(t, r, http.MethodGet, "/api/v1/reports/"+sumID+"/export.csv", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export summary: %d %s", w.Code, w.Body.String())
	}
	rows = parseDownloadedCSV(t, w.Body.String())
	if len(rows) != 3 || strings.Join(rows[0], ",") != "key,value,count" {
		t.Fatalf("summary export = %v; want header key,value,count + 2 groups", rows)
	}
	for i, g := range sumRun.Data.Groups {
		if rows[i+1][0] != *g.Key || rows[i+1][1] != *g.Value {
			t.Errorf("summary export row[%d] = %v; want run group %v/%v", i, rows[i+1], *g.Key, *g.Value)
		}
	}
}
