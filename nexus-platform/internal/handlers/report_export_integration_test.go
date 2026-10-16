//go:build integration

package handlers

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// reportEntity is the entity the report/export tests provision. `status` is an INDEXED picklist
// (groupBy-able for a summary report), `amount` is a plain number (sum metric), `note` is a plain
// text field (used to exercise CSV RFC-4180 escaping with embedded comma/quote/newline).
const reportEntity = "it_rep_orders"

// reportOtherTenant is a DIFFERENT tenant than testTenant, used to prove export tenant isolation.
const reportOtherTenant = "abcdef00-0000-0000-0000-0000000000ee"

// TestReportAndCSVExport drives the EPIC 6 M2 surface end-to-end through the real router:
//  1. admin report CRUD round-trip (create summary + tabular, get, list, update, delete),
//  2. run a SUMMARY report (grouped sum) → matches the aggregate engine,
//  3. run a TABULAR report (filtered + sorted + projected columns),
//  4. CSV export: correct data + RFC-4180 escaping (comma/quote/newline) + header from metadata,
//  5. CSV export tenant isolation (a co-tenant on the shared obj_* table sees only its own rows).
func TestReportAndCSVExport(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupNamed(sx, reportEntity)
	// Clean any sys_reports rows for both tenants + the co-tenant's metadata.
	clean := func() {
		sx.Exec(`DELETE FROM sys_reports WHERE tenant_id IN (?, ?) AND entity_name = ?`,
			testTenant, reportOtherTenant, reportEntity)
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name = ?`, reportOtherTenant, reportEntity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, reportOtherTenant, reportEntity)
		cleanupNamed(sx, reportEntity)
	}
	clean()
	t.Cleanup(clean)

	// --- Arrange: create the entity (indexed picklist status, number amount, text note). -------
	createBody := map[string]any{
		"name":  reportEntity,
		"label": "Đơn hàng",
		"fields": []map[string]any{
			{"name": "status", "label": "Trạng thái", "fieldType": "picklist",
				"options": map[string]any{"values": []string{"won", "lost", "open"}, "indexed": true}},
			{"name": "amount", "label": "Giá trị", "fieldType": "number",
				"options": map[string]any{"indexed": true}},
			{"name": "note", "label": "Ghi chú", "fieldType": "text"},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", createBody); w.Code != http.StatusCreated {
		t.Fatalf("setup create entity: %d %s", w.Code, w.Body.String())
	}

	mustCreate := func(data map[string]any) {
		if w := do(t, r, http.MethodPost, "/api/v1/"+reportEntity, data); w.Code != http.StatusCreated {
			t.Fatalf("create record %v: %d %s", data, w.Code, w.Body.String())
		}
	}
	// won: 100+200+300=600 (×3); lost: 50+50=100 (×2). The "tricky" note exercises CSV escaping.
	mustCreate(map[string]any{"status": "won", "amount": 100, "note": "plain"})
	mustCreate(map[string]any{"status": "won", "amount": 200, "note": "has,comma"})
	mustCreate(map[string]any{"status": "won", "amount": 300, "note": "has\"quote"})
	mustCreate(map[string]any{"status": "lost", "amount": 50, "note": "line1\nline2"})
	mustCreate(map[string]any{"status": "lost", "amount": 50, "note": "plain2"})

	// =========================================================================================
	// 1. ADMIN REPORT CRUD round-trip.
	// =========================================================================================
	summaryCreate := map[string]any{
		"name":       "Doanh thu theo trạng thái",
		"entityName": reportEntity,
		"type":       "summary",
		"config": map[string]any{
			"groupBy": "status",
			"metric":  "sum",
			"field":   "amount",
		},
	}
	w := do(t, r, http.MethodPost, "/api/v1/admin/reports", summaryCreate)
	if w.Code != http.StatusCreated {
		t.Fatalf("create summary report: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			Report struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"report"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	summaryID := created.Data.Report.ID
	if summaryID == "" || created.Data.Report.Type != "summary" {
		t.Fatalf("created summary report id=%q type=%q", summaryID, created.Data.Report.Type)
	}

	// GET it back.
	if w := do(t, r, http.MethodGet, "/api/v1/admin/reports/"+summaryID, nil); w.Code != http.StatusOK {
		t.Errorf("get report: %d %s", w.Code, w.Body.String())
	}
	// LIST shows at least our report.
	if w := do(t, r, http.MethodGet, "/api/v1/admin/reports", nil); w.Code != http.StatusOK {
		t.Errorf("list reports: %d %s", w.Code, w.Body.String())
	}

	// Invalid summary config (no metric) → 422 REPORT_CONFIG_INVALID.
	bad := map[string]any{
		"name": "x", "entityName": reportEntity, "type": "summary",
		"config": map[string]any{"groupBy": "status"},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/reports", bad); w.Code != http.StatusUnprocessableEntity ||
		errCodeOf(w.Body.Bytes()) != "REPORT_CONFIG_INVALID" {
		t.Errorf("bad summary config: %d %s; want 422 REPORT_CONFIG_INVALID", w.Code, w.Body.String())
	}
	// Unknown entity → 422 REPORT_ENTITY_NOT_FOUND.
	badEntity := map[string]any{
		"name": "x", "entityName": "no_such_rep_entity", "type": "tabular",
		"config": map[string]any{"columns": []string{"a"}},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/reports", badEntity); w.Code != http.StatusUnprocessableEntity ||
		errCodeOf(w.Body.Bytes()) != "REPORT_ENTITY_NOT_FOUND" {
		t.Errorf("bad entity: %d %s; want 422 REPORT_ENTITY_NOT_FOUND", w.Code, w.Body.String())
	}

	// Create a TABULAR report: project status+amount+note, filter status=won, sort amount desc.
	tabularCreate := map[string]any{
		"name":       "Đơn won",
		"entityName": reportEntity,
		"type":       "tabular",
		"config": map[string]any{
			// "ghost" is a column with NO matching field — a saved report survives the field
			// being deleted (tabular columns are deliberately unchecked at save time, see
			// ValidateReportConfig), so the run MUST still emit it rather than drop it: dropping
			// changes the column count and breaks the viewer's colSpan.
			"columns": []string{"status", "amount", "note", "ghost"},
			"filters": []map[string]any{{"field": "status", "op": "eq", "values": []string{"won"}}},
			"sort":    map[string]any{"field": "amount", "dir": "desc"},
		},
	}
	w = do(t, r, http.MethodPost, "/api/v1/admin/reports", tabularCreate)
	if w.Code != http.StatusCreated {
		t.Fatalf("create tabular report: %d %s", w.Code, w.Body.String())
	}
	var tcreated struct {
		Data struct {
			Report struct {
				ID string `json:"id"`
			} `json:"report"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &tcreated)
	tabularID := tcreated.Data.Report.ID

	// =========================================================================================
	// 2. RUN SUMMARY report → grouped sum (won=600/3, lost=100/2; value DESC).
	// =========================================================================================
	w = do(t, r, http.MethodGet, "/api/v1/reports/"+summaryID+"/run", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("run summary: %d %s", w.Code, w.Body.String())
	}
	var sumRun struct {
		Data struct {
			Type   string `json:"type"`
			Groups []struct {
				Key   *string `json:"key"`
				Value *string `json:"value"`
				Count int64   `json:"count"`
			} `json:"groups"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &sumRun)
	if sumRun.Data.Type != "summary" || len(sumRun.Data.Groups) != 2 {
		t.Fatalf("summary run = %+v; want type summary, 2 groups (%s)", sumRun.Data, w.Body.String())
	}
	g0 := sumRun.Data.Groups[0]
	if g0.Key == nil || *g0.Key != "won" || g0.Value == nil || !decEqual(*g0.Value, "600") || g0.Count != 3 {
		t.Errorf("summary group[0] = %+v; want won/600/3", g0)
	}
	g1 := sumRun.Data.Groups[1]
	if g1.Key == nil || *g1.Key != "lost" || g1.Value == nil || !decEqual(*g1.Value, "100") || g1.Count != 2 {
		t.Errorf("summary group[1] = %+v; want lost/100/2", g1)
	}

	// =========================================================================================
	// 3. RUN TABULAR report → 3 won rows, projected columns, sorted amount desc (300,200,100).
	// =========================================================================================
	w = do(t, r, http.MethodGet, "/api/v1/reports/"+tabularID+"/run", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("run tabular: %d %s", w.Code, w.Body.String())
	}
	var tabRun struct {
		Data struct {
			Type    string `json:"type"`
			Columns []struct {
				Name  string `json:"name"`
				Label string `json:"label"`
			} `json:"columns"`
			Rows  []map[string]any `json:"rows"`
			Total int              `json:"total"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &tabRun)
	if tabRun.Data.Type != "tabular" || tabRun.Data.Total != 3 || len(tabRun.Data.Rows) != 3 {
		t.Fatalf("tabular run = type %q total %d rows %d; want tabular/3/3 (%s)",
			tabRun.Data.Type, tabRun.Data.Total, len(tabRun.Data.Rows), w.Body.String())
	}
	// BOTH-ENDS CONTRACT: the viewer reads `col.label || col.name` and `row[col.name]`, so
	// `columns` MUST be objects carrying the metadata label — a bare string leaves every header
	// blank and every cell "—" on screen. Labels come from sys_fields.label of the projected
	// fields, in the report's declared column order.
	wantCols := []struct{ name, label string }{
		{"status", "Trạng thái"}, {"amount", "Giá trị"}, {"note", "Ghi chú"},
		// No field named "ghost" ⇒ no label to look up ⇒ label falls back to the name.
		{"ghost", "ghost"},
	}
	if len(tabRun.Data.Columns) != len(wantCols) {
		t.Fatalf("tabular columns = %+v; want %d objects (%s)",
			tabRun.Data.Columns, len(wantCols), w.Body.String())
	}
	for i, want := range wantCols {
		got := tabRun.Data.Columns[i]
		if got.Name != want.name || got.Label != want.label {
			t.Errorf("tabular column[%d] = {name:%q label:%q}; want {name:%q label:%q}",
				i, got.Name, got.Label, want.name, want.label)
		}
	}
	// Sorted amount DESC: 300, 200, 100. Each row carries only the projected columns + id.
	wantAmounts := []float64{300, 200, 100}
	for i, row := range tabRun.Data.Rows {
		if row["id"] == nil {
			t.Errorf("tabular row[%d] missing id", i)
		}
		amt, _ := row["amount"].(float64)
		if amt != wantAmounts[i] {
			t.Errorf("tabular row[%d] amount = %v; want %v", i, row["amount"], wantAmounts[i])
		}
		if row["status"] != "won" {
			t.Errorf("tabular row[%d] status = %v; want won", i, row["status"])
		}
		// `note` is a projected column; a non-projected field would be absent — assert it exists.
		if _, ok := row["note"]; !ok {
			t.Errorf("tabular row[%d] missing projected column note", i)
		}
	}

	// Update the summary report to inactive → run returns 422 REPORT_INACTIVE.
	upd := map[string]any{
		"name": "Doanh thu (tắt)", "entityName": reportEntity, "type": "summary",
		"config":   map[string]any{"groupBy": "status", "metric": "count"},
		"isActive": false,
	}
	if w := do(t, r, http.MethodPut, "/api/v1/admin/reports/"+summaryID, upd); w.Code != http.StatusOK {
		t.Fatalf("update report inactive: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, r, http.MethodGet, "/api/v1/reports/"+summaryID+"/run", nil); w.Code != http.StatusUnprocessableEntity ||
		errCodeOf(w.Body.Bytes()) != "REPORT_INACTIVE" {
		t.Errorf("run inactive report: %d %s; want 422 REPORT_INACTIVE", w.Code, w.Body.String())
	}
	// Delete the tabular report → run is 422 REPORT_NOT_FOUND.
	if w := do(t, r, http.MethodDelete, "/api/v1/admin/reports/"+tabularID, nil); w.Code != http.StatusNoContent {
		t.Errorf("delete tabular: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, r, http.MethodGet, "/api/v1/reports/"+tabularID+"/run", nil); w.Code != http.StatusUnprocessableEntity ||
		errCodeOf(w.Body.Bytes()) != "REPORT_NOT_FOUND" {
		t.Errorf("run deleted report: %d; want 422 REPORT_NOT_FOUND (%s)", w.Code, w.Body.String())
	}

	// =========================================================================================
	// 4. CSV EXPORT — header from metadata, all rows, RFC-4180 escaping.
	// =========================================================================================
	w = do(t, r, http.MethodGet, "/api/v1/"+reportEntity+"/export.csv", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export.csv: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("export Content-Type = %q; want text/csv", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, reportEntity+".csv") {
		t.Errorf("export Content-Disposition = %q; want attachment %s.csv", cd, reportEntity)
	}
	// The file is opened by double-clicking it in Excel, which reads a bare .csv as UTF-8 only
	// when it starts with a BOM — without it every Vietnamese value is mojibake. Parse the rest
	// with encoding/csv to prove it is well-formed RFC-4180 (the embedded comma, quote, and
	// newline must round-trip intact through a standard CSV reader).
	rows := parseDownloadedCSV(t, w.Body.String())
	if len(rows) != 6 { // 1 header + 5 data rows
		t.Fatalf("export CSV rows = %d; want 6 (1 header + 5 data)\n%s", len(rows), w.Body.String())
	}
	// Header = id + each declared field's LABEL (what the grid shows), not the machine name.
	header := rows[0]
	wantHeader := map[string]bool{"id": true, "Trạng thái": true, "Giá trị": true, "Ghi chú": true}
	if len(header) != 4 {
		t.Fatalf("export CSV header = %v; want 4 cols (id + 3 labels)", header)
	}
	for _, h := range header {
		if !wantHeader[h] {
			t.Errorf("export CSV header has unexpected col %q; want the field LABELS", h)
		}
	}
	noteIdx := colIndex(header, "Ghi chú")
	// Collect the note cells — the tricky values must survive the CSV round-trip exactly.
	gotNotes := map[string]bool{}
	for _, row := range rows[1:] {
		gotNotes[row[noteIdx]] = true
	}
	for _, want := range []string{"plain", "has,comma", "has\"quote", "line1\nline2", "plain2"} {
		if !gotNotes[want] {
			t.Errorf("export CSV missing note value %q (escaping lost it); got %v", want, gotNotes)
		}
	}

	// CSV export with a filter → only won rows (3).
	w = do(t, r, http.MethodGet, "/api/v1/"+reportEntity+"/export.csv?status=won", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export.csv filtered: %d %s", w.Code, w.Body.String())
	}
	frows := parseDownloadedCSV(t, w.Body.String())
	if len(frows) != 4 { // 1 header + 3 won
		t.Errorf("filtered export rows = %d; want 4 (1 header + 3 won)", len(frows))
	}

	// -----------------------------------------------------------------------------------------
	// 4b. REPORT CSV EXPORT — the file a manager is handed: BOM, label headings, and exactly the
	// on-screen columns (no leading raw UUID column, which the report screen never shows).
	// -----------------------------------------------------------------------------------------
	w = do(t, r, http.MethodPost, "/api/v1/admin/reports", tabularCreate)
	if w.Code != http.StatusCreated {
		t.Fatalf("re-create tabular report for export: %d %s", w.Code, w.Body.String())
	}
	var tcreated2 struct {
		Data struct {
			Report struct {
				ID string `json:"id"`
			} `json:"report"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &tcreated2)
	w = do(t, r, http.MethodGet, "/api/v1/reports/"+tcreated2.Data.Report.ID+"/export.csv", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("report export.csv: %d %s", w.Code, w.Body.String())
	}
	rprows := parseDownloadedCSV(t, w.Body.String())
	// status=won filter → 3 rows; columns = the 4 projected ones, no id.
	if len(rprows) != 4 {
		t.Fatalf("report export rows = %d; want 4 (1 header + 3 won)\n%s", len(rprows), w.Body.String())
	}
	wantRepHeader := []string{"Trạng thái", "Giá trị", "Ghi chú", "ghost"}
	if len(rprows[0]) != len(wantRepHeader) {
		t.Fatalf("report export header = %v; want %v (projected columns only, no id)", rprows[0], wantRepHeader)
	}
	for i, want := range wantRepHeader {
		if rprows[0][i] != want {
			t.Errorf("report export header[%d] = %q; want %q", i, rprows[0][i], want)
		}
	}

	// Export of an unknown entity → 404 (clean JSON error before any stream byte).
	if w := do(t, r, http.MethodGet, "/api/v1/no_such_rep_entity/export.csv", nil); w.Code != http.StatusNotFound {
		t.Errorf("export unknown entity: %d; want 404", w.Code)
	}

	// =========================================================================================
	// 5. CSV EXPORT TENANT ISOLATION — a co-tenant on the shared obj_* table sees ONLY its rows.
	// =========================================================================================
	if w := doAs(t, r, http.MethodPost, "/api/v1/admin/entities", reportOtherTenant, createBody); w.Code != http.StatusCreated && w.Code != http.StatusConflict {
		t.Fatalf("other-tenant register entity: %d %s", w.Code, w.Body.String())
	}
	wo := doAs(t, r, http.MethodGet, "/api/v1/"+reportEntity+"/export.csv", reportOtherTenant, nil)
	if wo.Code != http.StatusOK {
		t.Fatalf("other-tenant export: %d %s", wo.Code, wo.Body.String())
	}
	orows := parseDownloadedCSV(t, wo.Body.String())
	if len(orows) != 1 { // header only — the co-tenant has zero rows
		t.Fatalf("tenant isolation breach: other-tenant export has %d rows; want 1 (header only)\n%s",
			len(orows), wo.Body.String())
	}
	// testTenant still exports its full 5 rows after the co-tenant joined the shared table.
	w = do(t, r, http.MethodGet, "/api/v1/"+reportEntity+"/export.csv", nil)
	rrows := parseDownloadedCSV(t, w.Body.String())
	if len(rrows) != 6 {
		t.Errorf("testTenant export after co-tenant register = %d rows; want 6", len(rrows))
	}
}

// parseDownloadedCSV asserts a streamed CSV download opens with the UTF-8 BOM Excel needs and
// returns the rows behind it.
func parseDownloadedCSV(t *testing.T, body string) [][]string {
	t.Helper()
	if !strings.HasPrefix(body, "\xEF\xBB\xBF") {
		t.Fatalf("download does not start with a UTF-8 BOM — Excel will mojibake it: %q", body)
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\xEF\xBB\xBF"))).ReadAll()
	if err != nil {
		t.Fatalf("download is not valid RFC-4180: %v\n%s", err, body)
	}
	return rows
}

// colIndex returns the index of col in header, or -1.
func colIndex(header []string, col string) int {
	for i, h := range header {
		if h == col {
			return i
		}
	}
	return -1
}
