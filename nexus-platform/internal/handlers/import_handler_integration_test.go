//go:build integration

// Integration tests for CSV import M1. Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// Requires a reachable MySQL 8 with sys_* tables migrated (docker-compose dev DB or CI).
// Set TEST_DATABASE_URL to override the DSN.
//
// Coverage:
//   - 90 valid / 10 invalid rows (partial success)
//   - Missing required field in mapping → 400
//   - Tenant isolation (import into tenant A cannot be seen from tenant B)
//   - IMPORT_MAX_ROWS exceeded → 422
//   - Dedup: rows with existing dedupField value are skipped
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
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

// importEntity is the test entity name used by all import tests.
const importEntity = "it_import_leads"

// importTenantB is a second tenant used for isolation tests.
const importTenantB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

// cleanupImportEntity removes test data for importEntity under a given tenant.
func cleanupImportEntity(db *sqlx.DB, tenant string) {
	db.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
	         WHERE e.tenant_id = ? AND e.name = ?`, tenant, importEntity)
	db.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, tenant, importEntity)
	db.Exec("DROP TABLE IF EXISTS `obj_" + importEntity + "`")
}

// newImportRouter builds a full router with the import handler wired, using the real
// ACL-less path (nil aclChecker) so integration tests authenticate but skip per-action
// RBAC (no role rows needed). Authentication still requires a valid JWT.
func newImportRouter(t *testing.T) (*gin.Engine, *sqlx.DB) {
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

	entitySvc := services.NewEntityService(dyn, meta)
	fieldSvc := services.NewFieldService(dyn, meta)
	recordSvc := services.NewRecordService(meta, dyn)
	importSvc := services.NewImportService(meta, dyn, zap.NewNop())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:  NewAdminEntityHandler(entitySvc, fieldSvc, zap.NewNop()),
		Record: NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		Import: NewImportHandler(importSvc, zap.NewNop()),
	}, zap.NewNop())
	return r, sx
}

// createImportEntity creates the test entity with name+email fields via the admin API.
// name is required; email is optional. Returns when the entity exists (skips if already
// present by using the admin create endpoint and ignoring 409).
func createImportEntity(t *testing.T, r *gin.Engine, tenant string) {
	t.Helper()
	payload := map[string]any{
		"name":  importEntity,
		"label": "Import Leads",
		"fields": []map[string]any{
			{"name": "name", "label": "Name", "fieldType": "text", "isRequired": true},
			{"name": "email", "label": "Email", "fieldType": "email", "isRequired": false},
			{"name": "status", "label": "Status", "fieldType": "picklist", "isRequired": false,
				"options": map[string]any{"values": []string{"new", "contacted", "closed"}}},
			// Queue #7: a VND-configured currency field for the code-mismatch import test.
			{"name": "budget", "label": "Budget", "fieldType": "currency", "isRequired": false,
				"options": map[string]any{"currencyCode": "VND"}},
		},
	}
	buf, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/entities", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	setAuthTenant(t, req, tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// 201 = created, 409 = already exists — both are fine.
	if w.Code != http.StatusCreated && w.Code != http.StatusConflict {
		t.Fatalf("createImportEntity: got %d — %s", w.Code, w.Body.String())
	}
}

// setAuthTenant attaches a Bearer token for a specific tenant (helper for import tests that
// need to control tenant explicitly, bypassing the package-level testTenant constant).
func setAuthTenant(t *testing.T, req *http.Request, tenant string) {
	t.Helper()
	req.Header.Set("Authorization", "Bearer "+tokenForTenant(t, tenant))
}

// doImport sends a multipart import request.
func doImport(t *testing.T, r *gin.Engine, tenant, entity, csvContent, mappingJSON, dedupField string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// file field
	fw, err := w.CreateFormFile("file", "data.csv")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte(csvContent)); err != nil {
		t.Fatalf("write csv: %v", err)
	}

	// mapping field
	if err := w.WriteField("mapping", mappingJSON); err != nil {
		t.Fatalf("write mapping: %v", err)
	}

	// dedupField (optional)
	if dedupField != "" {
		if err := w.WriteField("dedupField", dedupField); err != nil {
			t.Fatalf("write dedupField: %v", err)
		}
	}
	w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/"+entity+"/import", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	setAuthTenant(t, req, tenant)
	rw := httptest.NewRecorder()
	r.ServeHTTP(rw, req)
	return rw
}

// TestImport_PartialSuccess verifies that 90 valid rows are inserted and 10 invalid rows
// (bad email format) are captured in errors.
func TestImport_PartialSuccess(t *testing.T) {
	r, sx := newImportRouter(t)
	t.Cleanup(func() { cleanupImportEntity(sx, testTenant) })
	createImportEntity(t, r, testTenant)

	var sb strings.Builder
	sb.WriteString("csv_name,csv_email\n")
	for i := 1; i <= 90; i++ {
		sb.WriteString(fmt.Sprintf("Lead%d,lead%d@example.com\n", i, i))
	}
	for i := 91; i <= 100; i++ {
		// Bad email — will fail ValidateRecord.
		sb.WriteString(fmt.Sprintf("Lead%d,not-an-email\n", i))
	}

	mappingJSON := `{"csv_name":"name","csv_email":"email"}`
	res := doImport(t, r, testTenant, importEntity, sb.String(), mappingJSON, "")

	if res.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 — %s", res.Code, res.Body.String())
	}
	var body struct {
		Data struct {
			Total    int `json:"total"`
			Inserted int `json:"inserted"`
			Failed   int `json:"failed"`
			Errors   []struct {
				Row  int    `json:"row"`
				Code string `json:"code"`
			} `json:"errors"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse response: %v — %s", err, res.Body.String())
	}
	if body.Data.Total != 100 {
		t.Errorf("total: got %d, want 100", body.Data.Total)
	}
	if body.Data.Inserted != 90 {
		t.Errorf("inserted: got %d, want 90", body.Data.Inserted)
	}
	if body.Data.Failed != 10 {
		t.Errorf("failed: got %d, want 10", body.Data.Failed)
	}
	if len(body.Data.Errors) != 10 {
		t.Errorf("errors len: got %d, want 10", len(body.Data.Errors))
	}
}

// TestImport_CurrencyCodeMismatch (queue #7, Q7) — the acceptance of the slice through the exact
// request ImportScreen issues: a CSV whose currency cell says USD for the VND-configured
// `budget` field. The response is still 200 partial-success (import_handler.go), the USD row
// sits in errors[] with the field named and RECORD_CURRENCY_MISMATCH, the VND row is inserted,
// and SUM(budget) afterwards equals the hand total of the accepted rows only — the mislabelled
// row was rejected, never relabelled into the sum.
func TestImport_CurrencyCodeMismatch(t *testing.T) {
	r, sx := newImportRouter(t)
	cleanupImportEntity(sx, testTenant) // a crashed earlier run must not leak rows into the SUM
	t.Cleanup(func() { cleanupImportEntity(sx, testTenant) })
	createImportEntity(t, r, testTenant)

	csvContent := "csv_name,csv_budget\n" +
		`Alice,"{""amount"":100,""currencyCode"":""USD""}"` + "\n" +
		`Bob,"{""amount"":200,""currencyCode"":""VND""}"` + "\n" +
		`Carol,"{""amount"":50,""currencyCode"":""VND""}"` + "\n"
	mappingJSON := `{"csv_name":"name","csv_budget":"budget"}`
	res := doImport(t, r, testTenant, importEntity, csvContent, mappingJSON, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 — %s", res.Code, res.Body.String())
	}
	var body struct {
		Data struct {
			Total    int `json:"total"`
			Inserted int `json:"inserted"`
			Failed   int `json:"failed"`
			Errors   []struct {
				Row     int    `json:"row"`
				Field   string `json:"field"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse response: %v — %s", err, res.Body.String())
	}
	if body.Data.Total != 3 || body.Data.Inserted != 2 || body.Data.Failed != 1 {
		t.Fatalf("total/inserted/failed = %d/%d/%d; want 3/2/1 — %s",
			body.Data.Total, body.Data.Inserted, body.Data.Failed, res.Body.String())
	}
	if len(body.Data.Errors) != 1 {
		t.Fatalf("errors = %+v; want exactly 1 — %s", body.Data.Errors, res.Body.String())
	}
	e := body.Data.Errors[0]
	if e.Row != 1 || e.Code != "RECORD_CURRENCY_MISMATCH" || e.Field != "budget" {
		t.Fatalf("row error = %+v; want {row:1 field:budget code:RECORD_CURRENCY_MISMATCH}", e)
	}
	if !strings.Contains(e.Message, "budget") || !strings.Contains(e.Message, "VND") || !strings.Contains(e.Message, "USD") {
		t.Fatalf("message %q must name the field and both codes", e.Message)
	}

	// SUM(budget) = 200 + 50: the USD 100 row is absent, not counted as VND 100.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/"+importEntity+"/aggregate?metric=sum&field=budget", nil)
	setAuthTenant(t, req, testTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("aggregate: got %d — %s", w.Code, w.Body.String())
	}
	var agg struct {
		Data struct {
			Value *string `json:"value"`
			Count int     `json:"count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &agg); err != nil {
		t.Fatalf("parse aggregate: %v — %s", err, w.Body.String())
	}
	if agg.Data.Value == nil || !strings.HasPrefix(*agg.Data.Value, "250.") {
		t.Fatalf("SUM(budget) = %v; want 250.* (VND rows only) — %s", agg.Data.Value, w.Body.String())
	}
}

// TestImport_MissingRequiredField verifies that when a required field (name) is not in
// the mapping, the endpoint returns 400 with IMPORT_REQUIRED_FIELD_NOT_MAPPED.
func TestImport_MissingRequiredField(t *testing.T) {
	r, sx := newImportRouter(t)
	t.Cleanup(func() { cleanupImportEntity(sx, testTenant) })
	createImportEntity(t, r, testTenant)

	// Only map email, NOT name (which is required).
	csvContent := "csv_email\nalice@example.com\n"
	mappingJSON := `{"csv_email":"email"}`
	res := doImport(t, r, testTenant, importEntity, csvContent, mappingJSON, "")

	if res.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 — %s", res.Code, res.Body.String())
	}
	var body map[string]any
	json.Unmarshal(res.Body.Bytes(), &body)
	if body["code"] != "IMPORT_REQUIRED_FIELD_NOT_MAPPED" {
		t.Errorf("code: got %v, want IMPORT_REQUIRED_FIELD_NOT_MAPPED", body["code"])
	}
}

// TestImport_TenantIsolation verifies that records imported under tenant A are NOT
// visible when listing records under tenant B.
func TestImport_TenantIsolation(t *testing.T) {
	r, sx := newImportRouter(t)
	t.Cleanup(func() {
		cleanupImportEntity(sx, testTenant)
		cleanupImportEntity(sx, importTenantB)
	})
	// Create entity for both tenants.
	createImportEntity(t, r, testTenant)
	createImportEntity(t, r, importTenantB)

	csvContent := "csv_name,csv_email\nAlice,alice@example.com\n"
	mappingJSON := `{"csv_name":"name","csv_email":"email"}`

	// Import into tenant A.
	res := doImport(t, r, testTenant, importEntity, csvContent, mappingJSON, "")
	if res.Code != http.StatusOK {
		t.Fatalf("import tenant A: status %d — %s", res.Code, res.Body.String())
	}

	// List records as tenant B — should see 0 records.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/"+importEntity, nil)
	setAuthTenant(t, req, importTenantB)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list tenant B: status %d", w.Code)
	}
	var listBody struct {
		Data       []any `json:"data"`
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	json.Unmarshal(w.Body.Bytes(), &listBody)
	if listBody.Pagination.Total != 0 {
		t.Errorf("tenant isolation: tenant B sees %d records, want 0", listBody.Pagination.Total)
	}
}

// TestImport_MaxRowsExceeded verifies that importing more rows than IMPORT_MAX_ROWS
// returns 422 with IMPORT_TOO_LARGE.
func TestImport_MaxRowsExceeded(t *testing.T) {
	r, sx := newImportRouter(t)
	t.Cleanup(func() { cleanupImportEntity(sx, testTenant) })
	createImportEntity(t, r, testTenant)

	// Temporarily set IMPORT_MAX_ROWS to a tiny value via env (reset after test).
	t.Setenv("IMPORT_MAX_ROWS", "2")

	var sb strings.Builder
	sb.WriteString("csv_name\n")
	for i := 1; i <= 5; i++ {
		sb.WriteString(fmt.Sprintf("Lead%d\n", i))
	}

	// Only map name; no email required.
	mappingJSON := `{"csv_name":"name"}`
	res := doImport(t, r, testTenant, importEntity, sb.String(), mappingJSON, "")

	if res.Code != http.StatusUnprocessableEntity {
		t.Errorf("status: got %d, want 422 — %s", res.Code, res.Body.String())
	}
	var body map[string]any
	json.Unmarshal(res.Body.Bytes(), &body)
	if body["code"] != "IMPORT_TOO_LARGE" {
		t.Errorf("code: got %v, want IMPORT_TOO_LARGE", body["code"])
	}
}

// TestImport_Dedup verifies that when dedupField is provided, duplicate values (both
// DB-existing and in-file) are skipped and reported as RECORD_DUPLICATE.
func TestImport_Dedup(t *testing.T) {
	r, sx := newImportRouter(t)
	t.Cleanup(func() { cleanupImportEntity(sx, testTenant) })
	createImportEntity(t, r, testTenant)

	// First, insert Alice via normal import (no dedup).
	firstCSV := "csv_name,csv_email\nAlice,alice@example.com\n"
	mappingJSON := `{"csv_name":"name","csv_email":"email"}`
	res := doImport(t, r, testTenant, importEntity, firstCSV, mappingJSON, "")
	if res.Code != http.StatusOK {
		t.Fatalf("first import: status %d — %s", res.Code, res.Body.String())
	}

	// Now import 3 rows: Alice (dup in DB), Bob (new), Alice (in-file dup of prev row in
	// this batch — but here it's dup of DB). To test in-file dup we send Bob twice.
	// Row 1: Alice — dup in DB → RECORD_DUPLICATE
	// Row 2: Bob — new → inserted
	// Row 3: Bob — in-file dup → RECORD_DUPLICATE
	secondCSV := "csv_name,csv_email\nAlice,alice@example.com\nBob,bob@example.com\nBob,bob@example.com\n"
	res = doImport(t, r, testTenant, importEntity, secondCSV, mappingJSON, "email")
	if res.Code != http.StatusOK {
		t.Fatalf("dedup import: status %d — %s", res.Code, res.Body.String())
	}
	var body struct {
		Data struct {
			Total    int `json:"total"`
			Inserted int `json:"inserted"`
			Failed   int `json:"failed"`
			Errors   []struct {
				Code string `json:"code"`
			} `json:"errors"`
		} `json:"data"`
	}
	json.Unmarshal(res.Body.Bytes(), &body)
	if body.Data.Inserted != 1 {
		t.Errorf("inserted: got %d, want 1", body.Data.Inserted)
	}
	if body.Data.Failed != 2 {
		t.Errorf("failed: got %d, want 2 (2 dups)", body.Data.Failed)
	}
	for _, e := range body.Data.Errors {
		if e.Code != "RECORD_DUPLICATE" {
			t.Errorf("error code: got %q, want RECORD_DUPLICATE", e.Code)
		}
	}
}
