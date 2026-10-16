//go:build integration

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

const bookEntity = "it_books"

func cleanupNamed(db *sqlx.DB, name string) {
	db.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
	         WHERE e.tenant_id = ? AND e.name = ?`, testTenant, name)
	db.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, testTenant, name)
	db.Exec("DROP TABLE IF EXISTS `obj_" + name + "`")
	// RM-28 Slice B: uniqueness for a plain `is_unique` field now lives SOLELY in
	// sys_unique_index (the physical obj_* index is non-unique — dropping the table no longer
	// frees a value slot for free the way it did pre-Slice-B). Without this, a second local
	// run of a test that inserts the SAME deterministic unique value into the SAME entity name
	// trips a false RECORD_DUPLICATE against this run's own orphaned pivot rows, breaking
	// idempotent re-runs against a persistent dev DB (CI is unaffected — fresh DB every run).
	db.Exec(`DELETE FROM sys_unique_index WHERE tenant_id = ? AND entity_name = ?`, testTenant, name)
}

// newFullRouter mounts the admin AND dynamic-record handlers behind the REAL A2 middleware
// chain (AuthMiddleware → RequireTenant) via handlers.RegisterAPIRoutes — exactly as main.go
// wires production. The verifier is the package-shared access-token service (accessVerifier), so
// the routers verify the same tokens tokenForTenant mints. This is also where a gin
// static(/admin)-vs-param(/:entity) route conflict would surface, so the test de-risks wiring.
func newFullRouter(t *testing.T) (*gin.Engine, *sqlx.DB) {
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
	recordSvc := services.NewRecordService(meta, dyn)
	// EPIC 6 M2: wire the report service (admin CRUD + run) over the same record service so the
	// saved-report run path reuses the live aggregate/list engine. The report repo is sqlx-backed
	// (sys_reports); the entity-existence check uses the metadata repo.
	reportSvc := services.NewReportService(report.NewSQLRepository(sx), recordSvc).WithEntityChecker(meta)
	// W4: wire the dashboard handler too so the /dashboards/:id/run overlay contract is
	// exercised through the same production route registration.
	dashSvc := services.NewDashboardService(dashboard.NewSQLRepository(sx), recordSvc, zap.NewNop())
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:     NewAdminEntityHandler(services.NewEntityService(dyn, meta), fieldSvc, zap.NewNop()),
		Record:    NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		Report:    NewAdminReportHandler(reportSvc, zap.NewNop()),
		Dashboard: NewAdminDashboardHandler(dashSvc, zap.NewNop()),
	}, zap.NewNop())
	return r, sx
}

func do(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
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
	setAuth(t, req, testTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestDynamicRecordCRUD(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupNamed(sx, bookEntity)
	t.Cleanup(func() { cleanupNamed(sx, bookEntity) })

	// Arrange: create the entity via the admin endpoint.
	createBody := map[string]any{
		"name":  bookEntity,
		"label": "Sách",
		"fields": []map[string]any{
			{"name": "title", "label": "Tiêu đề", "fieldType": "text", "isRequired": true},
			{"name": "pages", "label": "Số trang", "fieldType": "number"},
			{"name": "is_active", "label": "Đang bán", "fieldType": "boolean"}, // multi-word snake_case round-trip (F1 guard)
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", createBody); w.Code != http.StatusCreated {
		t.Fatalf("setup create entity: %d %s", w.Code, w.Body.String())
	}

	// Create a record.
	w := do(t, r, http.MethodPost, "/api/v1/"+bookEntity, map[string]any{"title": "Go in Action", "pages": 380, "is_active": true})
	if w.Code != http.StatusCreated {
		t.Fatalf("create record: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	id := created.Data.ID
	if id == "" {
		t.Fatal("created record id empty")
	}

	// Get it back.
	if w := do(t, r, http.MethodGet, "/api/v1/"+bookEntity+"/"+id, nil); w.Code != http.StatusOK {
		t.Errorf("get record: %d %s", w.Code, w.Body.String())
	}

	// List shows total 1.
	w = do(t, r, http.MethodGet, "/api/v1/"+bookEntity, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if list.Pagination.Total != 1 {
		t.Errorf("list total = %d; want 1", list.Pagination.Total)
	}

	// Update (apply-when-present): a PARTIAL body changes only `title`; the absent `pages`
	// and `is_active` fields must survive (no full-replace data loss — GD1 round-3 #PHU4).
	if w := do(t, r, http.MethodPut, "/api/v1/"+bookEntity+"/"+id, map[string]any{"title": "Go in Action 2e"}); w.Code != http.StatusOK {
		t.Errorf("update: %d %s", w.Code, w.Body.String())
	}
	// Read back and assert the merge preserved the omitted fields.
	w = do(t, r, http.MethodGet, "/api/v1/"+bookEntity+"/"+id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get after partial update: %d %s", w.Code, w.Body.String())
	}
	var afterUpdate struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &afterUpdate)
	d := afterUpdate.Data.Data
	if d["title"] != "Go in Action 2e" {
		t.Errorf("partial update title = %v; want Go in Action 2e", d["title"])
	}
	if d["pages"] != float64(380) {
		t.Errorf("partial update wiped absent field pages = %v; want 380 preserved", d["pages"])
	}
	if d["is_active"] != true {
		t.Errorf("partial update wiped absent field is_active = %v; want true preserved", d["is_active"])
	}

	// Delete (soft).
	if w := do(t, r, http.MethodDelete, "/api/v1/"+bookEntity+"/"+id, nil); w.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", w.Code, w.Body.String())
	}
	// Gone after delete.
	if w := do(t, r, http.MethodGet, "/api/v1/"+bookEntity+"/"+id, nil); w.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d; want 404", w.Code)
	}

	// Unknown entity -> 404.
	if w := do(t, r, http.MethodGet, "/api/v1/no_such_entity", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown entity: %d; want 404", w.Code)
	}

	// Unknown field -> 422 (strict).
	if w := do(t, r, http.MethodPost, "/api/v1/"+bookEntity, map[string]any{"title": "x", "ghost": 1}); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown field: %d; want 422", w.Code)
	}
}
