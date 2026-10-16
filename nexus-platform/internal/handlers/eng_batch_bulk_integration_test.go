//go:build integration

// ENG-BATCH #1 — bulk (multi-row) record create, HTTP-level integration tests. Complements
// internal/services/eng_batch_bulk_integration_test.go (which proves rollback/race/tenant/
// relation at the service layer against real MySQL): this file proves the HTTP surface —
// POST /:entity/bulk routes to CreateBulk (not Create), the ?mode= query is honored, the
// response envelope shape/status codes match ../specs/009-bulk-create/spec.md §2, and the cap/empty
// guards surface as clean JSON errors.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&loc=Local" \
//	  go test -tags=integration ./internal/handlers/ -run TestEngBatchBulkHTTP -race -v
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const bulkWidgetEntity = "it_bulk_widgets"

// newBulkRouter mirrors newFullRouter (dynamic_record_handler_integration_test.go) but ALSO
// wires WithSequenceClaimer + WithBulkInserter so the HTTP-level bulk-create tests can exercise
// auto_number + the atomic Phase-2 insert path exactly like production (main.go).
func newBulkRouter(t *testing.T) (*gin.Engine, *sqlx.DB) {
	t.Helper()
	sx, err := sqlx.Connect("mysql", dsn())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	gdb, err := gorm.Open(gormmysql.Open(dsn()), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	fieldSvc := services.NewFieldService(dyn, meta)
	recordSvc := services.NewRecordService(meta, dyn).WithSequenceClaimer(dyn).WithBulkInserter(dyn)
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:  NewAdminEntityHandler(services.NewEntityService(dyn, meta), fieldSvc, zap.NewNop()),
		Record: NewDynamicRecordHandler(recordSvc, zap.NewNop()),
	}, zap.NewNop())
	return r, sx
}

func setupBulkWidgetsEntityHTTP(t *testing.T, r *gin.Engine) {
	t.Helper()
	createBody := map[string]any{
		"name":  bulkWidgetEntity,
		"label": "Bulk Widgets",
		"fields": []map[string]any{
			{"name": "name", "label": "Name", "fieldType": "text", "isRequired": true},
			{"name": "email", "label": "Email", "fieldType": "text", "isUnique": true},
			{"name": "code", "label": "Code", "fieldType": "auto_number",
				"options": map[string]any{"prefix": "W-", "padding": 3}},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", createBody); w.Code != http.StatusCreated {
		t.Fatalf("setup create entity: %d %s", w.Code, w.Body.String())
	}
}

type bulkHTTPResponse struct {
	Data struct {
		Mode    string `json:"mode"`
		Created int    `json:"created"`
		Failed  int    `json:"failed"`
		Skipped int    `json:"skipped"`
		Results []struct {
			Index   int            `json:"index"`
			Status  string         `json:"status"`
			ID      string         `json:"id"`
			Code    string         `json:"code"`
			Error   string         `json:"error"`
			Details map[string]any `json:"details"`
		} `json:"results"`
	} `json:"data"`
}

// TestEngBatchBulkHTTP_AtomicAllOk_Returns201WithEnvelope proves the route dispatches to
// CreateBulk (the bulk envelope shape — {mode,created,failed,results} — is unique to it; plain
// POST /:entity returns {id}), and the atomic-all-ok status is 201.
func TestEngBatchBulkHTTP_AtomicAllOk_Returns201WithEnvelope(t *testing.T) {
	r, sx := newBulkRouter(t)
	cleanupNamed(sx, bulkWidgetEntity)
	t.Cleanup(func() { cleanupNamed(sx, bulkWidgetEntity) })
	setupBulkWidgetsEntityHTTP(t, r)

	body := map[string]any{"records": []map[string]any{
		{"name": "Row0", "email": "http-row0@example.com"},
		{"name": "Row1", "email": "http-row1@example.com"},
	}}
	w := do(t, r, http.MethodPost, "/api/v1/"+bulkWidgetEntity+"/bulk", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("bulk create: %d %s", w.Code, w.Body.String())
	}
	var resp bulkHTTPResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
	}
	if resp.Data.Mode != "atomic" || resp.Data.Created != 2 || resp.Data.Failed != 0 {
		t.Fatalf("envelope = %+v; want mode=atomic created=2 failed=0", resp.Data)
	}
	if len(resp.Data.Results) != 2 || resp.Data.Results[0].Status != "created" || resp.Data.Results[0].ID == "" {
		t.Fatalf("Results = %+v; want 2 created rows with ids", resp.Data.Results)
	}
	// Read one row back to confirm the auto_number claim landed via the real HTTP path.
	getW := do(t, r, http.MethodGet, "/api/v1/"+bulkWidgetEntity+"/"+resp.Data.Results[0].ID, nil)
	if getW.Code != http.StatusOK {
		t.Fatalf("get created row: %d %s", getW.Code, getW.Body.String())
	}
}

// TestEngBatchBulkHTTP_AtomicWithFailure_Returns422AndWritesNothing proves an atomic batch with
// one bad row returns 422 with the WHOLE envelope (not the generic {error,code} shape), and the
// good row is reported "skipped"/BULK_ATOMIC_ABORTED, never persisted.
func TestEngBatchBulkHTTP_AtomicWithFailure_Returns422AndWritesNothing(t *testing.T) {
	r, sx := newBulkRouter(t)
	cleanupNamed(sx, bulkWidgetEntity)
	t.Cleanup(func() { cleanupNamed(sx, bulkWidgetEntity) })
	setupBulkWidgetsEntityHTTP(t, r)

	body := map[string]any{"records": []map[string]any{
		{"name": "Good", "email": "good@example.com"},
		{"email": "no-name@example.com"}, // missing required name
	}}
	w := do(t, r, http.MethodPost, "/api/v1/"+bulkWidgetEntity+"/bulk", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bulk create: %d %s; want 422", w.Code, w.Body.String())
	}
	var resp bulkHTTPResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
	}
	if resp.Data.Created != 0 || resp.Data.Failed != 1 || resp.Data.Skipped != 1 {
		t.Fatalf("envelope = %+v; want created=0 failed=1 skipped=1 (Good row skipped, no-name row failed)", resp.Data)
	}
	if resp.Data.Results[0].Status != "skipped" || resp.Data.Results[0].Code != "BULK_ATOMIC_ABORTED" {
		t.Errorf("Results[0] = %+v; want skipped/BULK_ATOMIC_ABORTED", resp.Data.Results[0])
	}
	if resp.Data.Results[1].Code != "RECORD_REQUIRED_MISSING" {
		t.Errorf("Results[1].Code = %q; want RECORD_REQUIRED_MISSING", resp.Data.Results[1].Code)
	}

	// Nothing was written — list must be empty.
	listW := do(t, r, http.MethodGet, "/api/v1/"+bulkWidgetEntity, nil)
	var list struct {
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	json.Unmarshal(listW.Body.Bytes(), &list)
	if list.Pagination.Total != 0 {
		t.Errorf("list total after aborted atomic bulk = %d; want 0", list.Pagination.Total)
	}
}

// TestEngBatchBulkHTTP_PartialMode_Returns200WithMixedResults proves ?mode=partial always
// returns 200 with a mixed created/failed envelope.
func TestEngBatchBulkHTTP_PartialMode_Returns200WithMixedResults(t *testing.T) {
	r, sx := newBulkRouter(t)
	cleanupNamed(sx, bulkWidgetEntity)
	t.Cleanup(func() { cleanupNamed(sx, bulkWidgetEntity) })
	setupBulkWidgetsEntityHTTP(t, r)

	body := map[string]any{"records": []map[string]any{
		{"name": "Good", "email": "good-partial@example.com"},
		{"email": "no-name@example.com"},
	}}
	w := do(t, r, http.MethodPost, "/api/v1/"+bulkWidgetEntity+"/bulk?mode=partial", body)
	if w.Code != http.StatusOK {
		t.Fatalf("bulk create partial: %d %s; want 200", w.Code, w.Body.String())
	}
	var resp bulkHTTPResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
	}
	if resp.Data.Mode != "partial" || resp.Data.Created != 1 || resp.Data.Failed != 1 {
		t.Fatalf("envelope = %+v; want mode=partial created=1 failed=1", resp.Data)
	}

	listW := do(t, r, http.MethodGet, "/api/v1/"+bulkWidgetEntity, nil)
	var list struct {
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	json.Unmarshal(listW.Body.Bytes(), &list)
	if list.Pagination.Total != 1 {
		t.Errorf("list total after partial bulk = %d; want 1", list.Pagination.Total)
	}
}

// TestEngBatchBulkHTTP_EmptyAndTooLarge_ReturnCleanErrors proves the request-level guards
// surface as the standard {error,code,details} envelope (NOT the bulk results envelope) at
// 422, exactly like every other ValidationError on this API.
func TestEngBatchBulkHTTP_EmptyAndTooLarge_ReturnCleanErrors(t *testing.T) {
	r, sx := newBulkRouter(t)
	cleanupNamed(sx, bulkWidgetEntity)
	t.Cleanup(func() { cleanupNamed(sx, bulkWidgetEntity) })
	setupBulkWidgetsEntityHTTP(t, r)

	w := do(t, r, http.MethodPost, "/api/v1/"+bulkWidgetEntity+"/bulk", map[string]any{"records": []map[string]any{}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty records: %d %s; want 422", w.Code, w.Body.String())
	}
	var errResp struct {
		Code string `json:"code"`
	}
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Code != "BULK_EMPTY" {
		t.Errorf("code = %q; want BULK_EMPTY", errResp.Code)
	}

	records := make([]map[string]any, 0, 150)
	for i := 0; i < 150; i++ {
		records = append(records, map[string]any{"name": "Row"})
	}
	w2 := do(t, r, http.MethodPost, "/api/v1/"+bulkWidgetEntity+"/bulk", map[string]any{"records": records})
	if w2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("too-large records: %d %s; want 422", w2.Code, w2.Body.String())
	}
	var errResp2 struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	json.Unmarshal(w2.Body.Bytes(), &errResp2)
	if errResp2.Code != "BULK_TOO_LARGE" {
		t.Errorf("code = %q; want BULK_TOO_LARGE", errResp2.Code)
	}
	if errResp2.Details["max"] == nil {
		t.Errorf("details.max missing: %+v", errResp2.Details)
	}
}

// TestEngBatchBulkHTTP_PlainCreateRouteUnaffected proves POST /:entity (without /bulk) is
// unaffected — still returns the single-record {id} envelope (route-conflict guard).
func TestEngBatchBulkHTTP_PlainCreateRouteUnaffected(t *testing.T) {
	r, sx := newBulkRouter(t)
	cleanupNamed(sx, bulkWidgetEntity)
	t.Cleanup(func() { cleanupNamed(sx, bulkWidgetEntity) })
	setupBulkWidgetsEntityHTTP(t, r)

	w := do(t, r, http.MethodPost, "/api/v1/"+bulkWidgetEntity, map[string]any{"name": "Solo", "email": "solo@example.com"})
	if w.Code != http.StatusCreated {
		t.Fatalf("plain create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Data.ID == "" {
		t.Fatalf("plain create id empty — body=%s", w.Body.String())
	}
}
