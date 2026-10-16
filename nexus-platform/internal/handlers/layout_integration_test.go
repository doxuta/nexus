//go:build integration

// Layout Engine L1 integration tests — 24 cases TestLayout_1_… → TestLayout_24_…: the 22 of the
// original §6.1 list plus #23 (upsert returns the persisted id) and #24 (sortable decided at
// serve time). Run with:
//
//	go test -tags=integration ./internal/handlers/ -run TestLayout -v -race
//
// Requires MySQL 8 reachable at 127.0.0.1:3306 (root, no password) and the golang-migrate
// CLI (auto-located at $GOPATH/bin/migrate or A4_MIGRATE_BIN). Creates a unique throwaway
// DB per top-level test, migrates from zero to the latest migration (incl. 000008's UNIQUE
// constraint), and drops the DB in cleanup.
//
// The UNIQUE constraint from 000008 is the critical gate: without it the ON DUPLICATE KEY
// UPDATE upsert is not race-safe and test #20 would be meaningless.
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// ---------------------------------------------------------------------------
// Shared DB counter
// ---------------------------------------------------------------------------

var layoutItDBCounter int64

// layoutItDBName is PID-scoped like every sibling harness (a4UniqueDBName, o3aUniqueDBName): a
// bare counter restarts at 1 in each test process, so two worktrees running this suite at once
// dropped and migrated each other's nexus_layout_it_N (Error 1049 Unknown database, 409 entity
// already exists). The name still matches ittest's stale-sweep pattern _it_<pid>_<n>.
func layoutItDBName() string {
	n := atomic.AddInt64(&layoutItDBCounter, 1)
	return fmt.Sprintf("nexus_layout_it_%d_%d", os.Getpid(), n)
}

// layoutSetupDB spins up a fresh migrated throwaway DB for one test suite.
// It skips (not fails) when MySQL or the migrate CLI is unavailable.
func layoutSetupDB(t *testing.T) (*gorm.DB, *sqlx.DB) {
	t.Helper()

	bin := a4MigrateBin(t) // from a4_rbac_harness_integration_test.go (same pkg)
	if bin == "" {
		t.Skip("golang-migrate CLI not found — skipping layout integration test")
	}

	dbName := layoutItDBName()

	// Create fresh DB
	admin, err := sqlx.Connect("mysql", a4AdminDSN(t))
	if err != nil {
		t.Skipf("MySQL unreachable (%v) — skipping layout integration test", err)
	}
	_, _ = admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
	if _, err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create db %s: %v", dbName, err)
	}
	_ = admin.Close()
	t.Cleanup(func() {
		if a, err2 := sqlx.Connect("mysql", a4AdminDSN(t)); err2 == nil {
			_, _ = a.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
			_ = a.Close()
		}
	})

	// Real migrate up (000001..000008) — includes 000008 UNIQUE constraint.
	cmd := exec.Command(bin, "-path", migrationsDir(t), "-database", a4MigratedDSN(t, dbName), "up")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("migrate up failed: %v\n%s", err, out)
	}
	t.Logf("migrate up (db=%s)", dbName)

	gormDSN := a4GormDSN(t, dbName)
	gdb, err := gorm.Open(gormmysql.Open(gormDSN), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	sx, err := sqlx.Connect("mysql", gormDSN)
	if err != nil {
		t.Fatalf("sqlx connect: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, _ := gdb.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
		_ = sx.Close()
	})

	// Assert migration landed at version 8, dirty=false.
	var ver struct {
		Version int
		Dirty   bool
	}
	if err := sx.Get(&ver, "SELECT version, dirty FROM schema_migrations LIMIT 1"); err != nil {
		t.Fatalf("schema_migrations query: %v", err)
	}
	if ver.Version < 8 || ver.Dirty {
		t.Fatalf("schema_migrations version=%d dirty=%v; want >=8 dirty=false", ver.Version, ver.Dirty)
	}

	return gdb, sx
}

// ---------------------------------------------------------------------------
// Router builder
// ---------------------------------------------------------------------------

// newLayoutTestRouter wires the full /api/v1 layout surface (entity + layout).
func newLayoutTestRouter(t *testing.T, gdb *gorm.DB, sx *sqlx.DB) *gin.Engine {
	t.Helper()
	metaRepo := metadata.NewRepository(gdb)
	layoutRepo := metadata.NewLayoutRepository(gdb)
	entitySvc := services.NewEntityService(dynrepo.New(sx), metaRepo)
	fieldSvc := services.NewFieldService(dynrepo.New(sx), metaRepo)
	layoutSvc := services.NewLayoutService(metaRepo, layoutRepo, zap.NewNop())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:  NewAdminEntityHandler(entitySvc, fieldSvc, zap.NewNop()),
		Layout: NewAdminLayoutHandler(layoutSvc, zap.NewNop()),
	}, zap.NewNop())
	return r
}

// mintToken mints an access token for a given tenant with the given admin flag.
func mintToken(t *testing.T, tenantID string, isAdmin bool) string {
	t.Helper()
	tok, err := accessVerifier(t).IssueAccessToken(auth.Subject{
		UserID:   uuid.NewString(),
		TenantID: tenantID,
		IsAdmin:  isAdmin,
	})
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	return tok
}

// ---------------------------------------------------------------------------
// Request helpers
// ---------------------------------------------------------------------------

func layoutDoGET(t *testing.T, r *gin.Engine, tok, entity, viewFilter string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/v1/metadata/layouts/" + entity
	if viewFilter != "" {
		url += "?view=" + viewFilter
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func layoutDoPOST(t *testing.T, r *gin.Engine, tok string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/layouts", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func layoutDoPUT(t *testing.T, r *gin.Engine, tok, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/layouts/"+id, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func layoutDoDELETE(t *testing.T, r *gin.Engine, tok, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/layouts/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func layoutCreateEntity(t *testing.T, r *gin.Engine, tok, entityName string, fields []map[string]any) {
	t.Helper()
	body := map[string]any{"name": entityName, "label": entityName, "fields": fields}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/entities", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("createEntity %q: status=%d body=%s", entityName, w.Code, w.Body.String())
	}
}

// parseBody decodes a response body into a map.
func parseBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("parseBody: %v (body=%s)", err, w.Body.String())
	}
	return out
}

func errorCode(body map[string]any) string {
	s, _ := body["code"].(string)
	return s
}

// extractLayoutID extracts data.id from a POST /admin/layouts response.
func extractLayoutID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	body := parseBody(t, w)
	data, _ := body["data"].(map[string]any)
	id, _ := data["id"].(string)
	if id == "" {
		t.Fatalf("extractLayoutID: no id in response body=%s", w.Body.String())
	}
	return id
}

// softDeleteField helper — finds a field id for (entity, fieldName) then calls DELETE /admin/fields/:id.
func softDeleteField(t *testing.T, r *gin.Engine, tok, tenant, entityName, fieldName string, sx *sqlx.DB) {
	t.Helper()
	// Fetch field id via SQL (direct query since there's no GET /fields endpoint yet).
	var fieldID string
	err := sx.Get(&fieldID, `SELECT f.id FROM sys_fields f
		JOIN sys_entities e ON e.id = f.entity_id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ? AND f.deleted_at IS NULL`,
		tenant, entityName, fieldName)
	if err != nil {
		t.Fatalf("softDeleteField: resolve field id for %q.%q: %v", entityName, fieldName, err)
	}
	buf := []byte(`{}`)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/fields/"+fieldID, bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("softDeleteField %q.%q: status=%d body=%s", entityName, fieldName, w.Code, w.Body.String())
	}
}

// readdFieldSQL re-activates a soft-deleted field by clearing its deleted_at via SQL.
// This simulates the "re-add cùng tên" scenario described in spec §4.5 B-1.
// The field service currently rejects re-add of soft-deleted names (FIELD_ALREADY_EXISTS),
// so we restore directly in DB to test the layout's serve-time tolerance reactivation.
func readdFieldSQL(t *testing.T, sx *sqlx.DB, tenant, entityName, fieldName string) {
	t.Helper()
	res, err := sx.Exec(`UPDATE sys_fields f
		JOIN sys_entities e ON e.id = f.entity_id
		SET f.deleted_at = NULL
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ?`,
		tenant, entityName, fieldName)
	if err != nil {
		t.Fatalf("readdFieldSQL %q.%q: %v", entityName, fieldName, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		t.Fatalf("readdFieldSQL %q.%q: 0 rows affected", entityName, fieldName)
	}
}

// ---------------------------------------------------------------------------
// Test #1: Entity with no custom layout → GET returns 3 default views, all isDefault:true
// ---------------------------------------------------------------------------

func TestLayout_1_DefaultViews(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_1"
	const tenantA = "c1111111-0000-0000-0000-000000000001"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text"},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	w := layoutDoGET(t, r, tok, entity, "")
	if w.Code != http.StatusOK {
		t.Fatalf("#1 GET: status=%d body=%s", w.Code, w.Body.String())
	}
	body := parseBody(t, w)
	data := body["data"].(map[string]any)
	views := data["views"].(map[string]any)

	for _, vt := range []string{"edit", "detail", "list"} {
		vr, ok := views[vt].(map[string]any)
		if !ok {
			t.Errorf("#1: views[%s] is nil or not an object", vt)
			continue
		}
		isDefault, _ := vr["isDefault"].(bool)
		if !isDefault {
			t.Errorf("#1: views[%s].isDefault = false; want true", vt)
		}
	}
}

// ---------------------------------------------------------------------------
// Test #2: GET-default → POST verbatim → 200 (round-trip invariant)
// ---------------------------------------------------------------------------

func TestLayout_2_RoundTrip(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_2"
	const tenantA = "c2222222-0000-0000-0000-000000000002"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	// GET to obtain default layout for edit view
	wGet := layoutDoGET(t, r, tok, entity, "edit")
	if wGet.Code != http.StatusOK {
		t.Fatalf("#2 GET: status=%d body=%s", wGet.Code, wGet.Body.String())
	}
	bodyGet := parseBody(t, wGet)
	data := bodyGet["data"].(map[string]any)
	views := data["views"].(map[string]any)
	editView := views["edit"].(map[string]any)

	// Build POST body from the default layout — verbatim (round-trip)
	layoutJson := map[string]any{}
	for k, v := range editView {
		if k == "isDefault" {
			continue
		}
		layoutJson[k] = v
	}

	postBody := map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": layoutJson,
	}
	w := layoutDoPOST(t, r, tok, postBody)
	if w.Code != http.StatusOK {
		t.Fatalf("#2 round-trip POST: status=%d body=%s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test #3: POST custom edit → GET shows isDefault:false for edit, true for others
// ---------------------------------------------------------------------------

func TestLayout_3_CustomLayout(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_3"
	const tenantA = "c3333333-0000-0000-0000-000000000003"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	postBody := map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version": 1,
			"sections": []any{
				map[string]any{
					"title":   "Custom",
					"columns": 1,
					"fields":  []any{"title"},
				},
			},
		},
	}
	wPost := layoutDoPOST(t, r, tok, postBody)
	if wPost.Code != http.StatusOK {
		t.Fatalf("#3 POST: status=%d body=%s", wPost.Code, wPost.Body.String())
	}

	wGet := layoutDoGET(t, r, tok, entity, "")
	if wGet.Code != http.StatusOK {
		t.Fatalf("#3 GET: status=%d", wGet.Code)
	}
	body := parseBody(t, wGet)
	views := body["data"].(map[string]any)["views"].(map[string]any)

	editView := views["edit"].(map[string]any)
	if isDefault, _ := editView["isDefault"].(bool); isDefault {
		t.Error("#3: edit isDefault should be false after POST")
	}
	for _, vt := range []string{"detail", "list"} {
		vr := views[vt].(map[string]any)
		if isDefault, _ := vr["isDefault"].(bool); !isDefault {
			t.Errorf("#3: %s isDefault should be true", vt)
		}
	}
}

// ---------------------------------------------------------------------------
// Test #4: POST unknown field → 422 LAYOUT_INVALID
// ---------------------------------------------------------------------------

func TestLayout_4_UnknownField(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_4"
	const tenantA = "c4444444-0000-0000-0000-000000000004"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text"},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	w := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version": 1,
			"sections": []any{
				map[string]any{
					"columns": 1,
					"fields":  []any{"no_such_field"},
				},
			},
		},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("#4: status=%d; want 422 (body=%s)", w.Code, w.Body.String())
	}
	if errorCode(parseBody(t, w)) != "LAYOUT_INVALID" {
		t.Errorf("#4: code=%q; want LAYOUT_INVALID", errorCode(parseBody(t, w)))
	}
}

// ---------------------------------------------------------------------------
// Test #5: POST invalid view_type → 422
// ---------------------------------------------------------------------------

func TestLayout_5_InvalidViewType(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_5"
	const tenantA = "c5555555-0000-0000-0000-000000000005"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, nil)
	t.Cleanup(func() {
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	for _, vt := range []string{"foo", "Edit", " list "} {
		w := layoutDoPOST(t, r, tok, map[string]any{
			"entityName": entity,
			"viewType":   vt,
			"layoutJson": map[string]any{"version": 1, "sections": []any{}},
		})
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("#5 viewType=%q: status=%d; want 422", vt, w.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Test #6: Floor rule — required:false on sys-required field → 422
// ---------------------------------------------------------------------------

func TestLayout_6_FloorRule(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_6"
	const tenantA = "c6666666-0000-0000-0000-000000000006"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	w := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version": 1,
			"sections": []any{
				map[string]any{
					"columns": 1,
					"fields":  []any{map[string]any{"field": "title", "required": false}},
				},
			},
		},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("#6: status=%d; want 422 (body=%s)", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test #7: required-omitted (edit) — missing a required field → 422 REQUIRED_FIELD_OMITTED
// ---------------------------------------------------------------------------

func TestLayout_7_RequiredOmitted(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_7"
	const tenantA = "c7777777-0000-0000-0000-000000000007"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
		{"name": "notes", "label": "Notes", "fieldType": "text"},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	// Include only "notes" — omit required "title"
	w := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version": 1,
			"sections": []any{
				map[string]any{
					"columns": 1,
					"fields":  []any{"notes"},
				},
			},
		},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("#7: status=%d; want 422 (body=%s)", w.Code, w.Body.String())
	}
	if errorCode(parseBody(t, w)) != "REQUIRED_FIELD_OMITTED" {
		t.Errorf("#7: code=%q; want REQUIRED_FIELD_OMITTED", errorCode(parseBody(t, w)))
	}
}

// ---------------------------------------------------------------------------
// Test #8: Duplicate field → 422
// ---------------------------------------------------------------------------

func TestLayout_8_DuplicateField(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_8"
	const tenantA = "c8888888-0000-0000-0000-000000000008"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text"},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	w := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "detail",
		"layoutJson": map[string]any{
			"version": 1,
			"sections": []any{
				map[string]any{
					"columns": 1,
					"fields":  []any{"title", "title"}, // duplicate
				},
			},
		},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("#8: status=%d; want 422 (body=%s)", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test #9: colspan > columns → 422
// ---------------------------------------------------------------------------

func TestLayout_9_ColspanBeyondColumns(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_9"
	const tenantA = "c9999999-0000-0000-0000-000000000009"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text"},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	w := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version": 1,
			"sections": []any{
				map[string]any{
					"columns": 2,
					"fields":  []any{map[string]any{"field": "title", "colspan": 3}}, // 3 > 2
				},
			},
		},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("#9: status=%d; want 422 (body=%s)", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test #10: defaultSort.field = data field → 422
// ---------------------------------------------------------------------------

func TestLayout_10_DefaultSortDataField(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_10"
	const tenantA = "ca000000-0000-0000-0000-00000000000a"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "name", "label": "Name", "fieldType": "text"},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	w := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "list",
		"layoutJson": map[string]any{
			"version": 1,
			"columns": []any{map[string]any{"field": "name"}},
			"defaultSort": map[string]any{
				"field": "name", // not in {created_at,updated_at,id}
				"order": "desc",
			},
		},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("#10: status=%d; want 422 (body=%s)", w.Code, w.Body.String())
	}
}

// DAILY-QUEUE #273: a defaultSort on an indexed business field saves (201) and is served back
// verbatim; a plain JSON field still 422s (TestLayout_10).
func TestLayout_10b_DefaultSortIndexedField(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_10b"
	const tenantA = "ca000000-0000-0000-0000-0000000000ab"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "name", "label": "Name", "fieldType": "text"},
		{"name": "stage", "label": "Stage", "fieldType": "picklist", "options": map[string]any{"values": []any{"new", "won"}, "indexed": true}},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	w := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "list",
		"layoutJson": map[string]any{
			"version":     1,
			"columns":     []any{map[string]any{"field": "name"}, map[string]any{"field": "stage"}},
			"defaultSort": map[string]any{"field": "stage", "order": "asc"},
		},
	})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("#10b: status=%d; want 2xx (body=%s)", w.Code, w.Body.String())
	}
	// Served back verbatim (200 on upsert-update, 201 on first insert): GET must still say stage/asc,
	// not the created_at desc serve-time fallback.
	g := layoutDoGET(t, r, tok, entity, "list")
	if g.Code != http.StatusOK || !strings.Contains(g.Body.String(), `"defaultSort":{"field":"stage","order":"asc"}`) {
		t.Fatalf("#10b: GET status=%d; defaultSort not served verbatim (body=%s)", g.Code, g.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test #11: Unknown key → 422
// ---------------------------------------------------------------------------

func TestLayout_11_UnknownKey(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_11"
	const tenantA = "cb000000-0000-0000-0000-00000000000b"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text"},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	w := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"columns": 1, "fields": []any{"title"}}},
			"unknown":  "value", // unknown top-level key
		},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("#11: status=%d; want 422 (body=%s)", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test #12: Payload > 200 cells → 422
// ---------------------------------------------------------------------------

func TestLayout_12_PayloadBounds(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_12"
	const tenantA = "cc000000-0000-0000-0000-00000000000c"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text"},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	// Build 201 cells (all "title" — duplicate error fires before bounds in this case,
	// so use 201 spacers + 1 real field to hit the bounds check cleanly).
	cells := make([]any, 202)
	cells[0] = "title"
	for i := 1; i < 202; i++ {
		cells[i] = map[string]any{"spacer": true}
	}
	w := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"columns": 1, "fields": cells}},
		},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("#12: status=%d; want 422 (body=%s)", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test #13: DELETE → GET returns default (hard-delete revert)
// ---------------------------------------------------------------------------

func TestLayout_13_DeleteRevertsToDefault(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_13"
	const tenantA = "cd000000-0000-0000-0000-00000000000d"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	// Save a custom layout
	wPost := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"title": "Custom", "columns": 1, "fields": []any{"title"}}},
		},
	})
	if wPost.Code != http.StatusOK {
		t.Fatalf("#13 POST: status=%d body=%s", wPost.Code, wPost.Body.String())
	}
	layoutID := extractLayoutID(t, wPost)

	// Verify it's custom
	wGet1 := layoutDoGET(t, r, tok, entity, "edit")
	body1 := parseBody(t, wGet1)
	editView := body1["data"].(map[string]any)["views"].(map[string]any)["edit"].(map[string]any)
	if isDefault, _ := editView["isDefault"].(bool); isDefault {
		t.Fatal("#13: after POST, edit should not be default")
	}

	// DELETE
	wDel := layoutDoDELETE(t, r, tok, layoutID)
	if wDel.Code != http.StatusNoContent {
		t.Fatalf("#13 DELETE: status=%d body=%s", wDel.Code, wDel.Body.String())
	}

	// GET again → should be default
	wGet2 := layoutDoGET(t, r, tok, entity, "edit")
	body2 := parseBody(t, wGet2)
	editView2 := body2["data"].(map[string]any)["views"].(map[string]any)["edit"].(map[string]any)
	if isDefault, _ := editView2["isDefault"].(bool); !isDefault {
		t.Error("#13: after DELETE, edit should revert to default")
	}
}

// ---------------------------------------------------------------------------
// Test #14: IDOR — tenant B PUT/DELETE layout owned by tenant A → 404
// ---------------------------------------------------------------------------

func TestLayout_14_IDOR(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_14"
	const tenantA = "ce000000-0000-0000-0000-00000000000e"
	const tenantB = "cf000000-0000-0000-0000-00000000000f"
	tokA := mintToken(t, tenantA, true)
	tokB := mintToken(t, tenantB, true)

	// Tenant A creates entity + custom layout
	layoutCreateEntity(t, r, tokA, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
	})
	// Tenant B also creates the entity (same name, different tenant)
	layoutCreateEntity(t, r, tokB, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
	})
	t.Cleanup(func() {
		for _, ten := range []string{tenantA, tenantB} {
			sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", ten, entity)
			sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", ten, entity)
			sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", ten, entity)
		}
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	wPost := layoutDoPOST(t, r, tokA, map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"columns": 1, "fields": []any{"title"}}},
		},
	})
	if wPost.Code != http.StatusOK {
		t.Fatalf("#14 POST (tenantA): status=%d body=%s", wPost.Code, wPost.Body.String())
	}
	layoutID := extractLayoutID(t, wPost)

	// Tenant B tries to PUT layout owned by tenant A
	wPut := layoutDoPUT(t, r, tokB, layoutID, map[string]any{
		"layoutJson": map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"columns": 1, "fields": []any{"title"}}},
		},
	})
	if wPut.Code != http.StatusNotFound {
		t.Errorf("#14 PUT by tenantB: status=%d; want 404", wPut.Code)
	}

	// Tenant B tries to DELETE layout owned by tenant A
	wDel := layoutDoDELETE(t, r, tokB, layoutID)
	if wDel.Code != http.StatusNotFound {
		t.Errorf("#14 DELETE by tenantB: status=%d; want 404", wDel.Code)
	}
}

// Test #23 (regression B1): a re-POST (upsert UPDATE path) must return the CANONICAL persisted
// id, not a fresh discarded uuid — else PUT/DELETE by the returned id 404s on an existing layout.
func TestLayout_23_UpsertReturnsPersistedID(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_23"
	const tenant = "23000000-0000-0000-0000-000000000023"
	tok := mintToken(t, tenant, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenant, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenant, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenant, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	body := map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"columns": 1, "fields": []any{"title"}}},
		},
	}

	// POST #1 — INSERT path.
	w1 := layoutDoPOST(t, r, tok, body)
	if w1.Code != http.StatusOK {
		t.Fatalf("#23 POST#1: status=%d body=%s", w1.Code, w1.Body.String())
	}
	id1 := extractLayoutID(t, w1)

	// POST #2 same (tenant,entity,view) — UPDATE path via ON DUPLICATE KEY.
	w2 := layoutDoPOST(t, r, tok, body)
	if w2.Code != http.StatusOK {
		t.Fatalf("#23 POST#2 (upsert): status=%d body=%s", w2.Code, w2.Body.String())
	}
	id2 := extractLayoutID(t, w2)

	// The upsert path MUST return the canonical persisted id, not a fresh discarded uuid (B1).
	if id2 != id1 {
		t.Fatalf("#23 upsert returned id2=%q; want canonical id1=%q (B1 ghost-id regression)", id2, id1)
	}

	// GET must also surface the same by-id handle for the custom view (S1).
	wGet := layoutDoGET(t, r, tok, entity, "edit")
	editView := parseBody(t, wGet)["data"].(map[string]any)["views"].(map[string]any)["edit"].(map[string]any)
	if gotID, _ := editView["id"].(string); gotID != id1 {
		t.Fatalf("#23 GET edit view id=%q; want %q (S1: by-id handle exposed)", gotID, id1)
	}

	// DELETE by the returned id MUST resolve (proves the id is real, not a ghost).
	wDel := layoutDoDELETE(t, r, tok, id2)
	if wDel.Code != http.StatusNoContent {
		t.Fatalf("#23 DELETE by returned id: status=%d; want 204 (id must be persisted)", wDel.Code)
	}
}

// ---------------------------------------------------------------------------
// Test #15: GET entity that does not exist / belongs to another tenant → 404
// ---------------------------------------------------------------------------

func TestLayout_15_EntityNotFound(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const tenantA = "d0000000-0000-0000-0000-000000000001"
	tok := mintToken(t, tenantA, true)
	_ = sx

	// Non-existent entity
	w := layoutDoGET(t, r, tok, "no_such_entity_xyz", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("#15: status=%d; want 404 for unknown entity", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Test #16: Non-admin POST/PUT/DELETE → 403
// ---------------------------------------------------------------------------

func TestLayout_16_NonAdmin(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_16"
	const tenantA = "d1000000-0000-0000-0000-000000000001"
	tokAdmin := mintToken(t, tenantA, true)
	tokUser := mintToken(t, tenantA, false)
	_ = sx

	layoutCreateEntity(t, r, tokAdmin, entity, nil)
	t.Cleanup(func() {
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	w := layoutDoPOST(t, r, tokUser, map[string]any{
		"entityName": entity,
		"viewType":   "list",
		"layoutJson": map[string]any{"version": 1, "columns": []any{}, "defaultSort": map[string]any{"field": "created_at", "order": "desc"}},
	})
	if w.Code != http.StatusForbidden {
		t.Errorf("#16 POST non-admin: status=%d; want 403", w.Code)
	}

	wPut := layoutDoPUT(t, r, tokUser, uuid.NewString(), map[string]any{
		"layoutJson": map[string]any{"version": 1},
	})
	if wPut.Code != http.StatusForbidden {
		t.Errorf("#16 PUT non-admin: status=%d; want 403", wPut.Code)
	}

	wDel := layoutDoDELETE(t, r, tokUser, uuid.NewString())
	if wDel.Code != http.StatusForbidden {
		t.Errorf("#16 DELETE non-admin: status=%d; want 403", wDel.Code)
	}
}

// ---------------------------------------------------------------------------
// Test #17: Tenant B sees default (not A's custom layout) for same entity name
// ---------------------------------------------------------------------------

func TestLayout_17_TenantIsolation(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_17"
	const tenantA = "d2000000-0000-0000-0000-000000000002"
	const tenantB = "d3000000-0000-0000-0000-000000000003"
	tokA := mintToken(t, tenantA, true)
	tokB := mintToken(t, tenantB, true)

	layoutCreateEntity(t, r, tokA, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
	})
	layoutCreateEntity(t, r, tokB, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
	})
	t.Cleanup(func() {
		for _, ten := range []string{tenantA, tenantB} {
			sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", ten, entity)
			sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", ten, entity)
			sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", ten, entity)
		}
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	// Tenant A saves a custom layout
	wPost := layoutDoPOST(t, r, tokA, map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"columns": 1, "fields": []any{"title"}}},
		},
	})
	if wPost.Code != http.StatusOK {
		t.Fatalf("#17 POST (tenantA): status=%d body=%s", wPost.Code, wPost.Body.String())
	}

	// Tenant B should still see default
	wGet := layoutDoGET(t, r, tokB, entity, "edit")
	body := parseBody(t, wGet)
	editView := body["data"].(map[string]any)["views"].(map[string]any)["edit"].(map[string]any)
	if isDefault, _ := editView["isDefault"].(bool); !isDefault {
		t.Error("#17: tenantB should see default (not tenantA's custom layout)")
	}
}

// ---------------------------------------------------------------------------
// Test #18: serve-time tolerance — soft-deleted field disappears from layout
// ---------------------------------------------------------------------------

func TestLayout_18_ServeTimeTolerance(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_18"
	const tenantA = "d4000000-0000-0000-0000-000000000004"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
		{"name": "notes", "label": "Notes", "fieldType": "text"},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	// Save custom edit layout including both fields
	wPost := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"columns": 1, "fields": []any{"notes", "title"}}},
		},
	})
	if wPost.Code != http.StatusOK {
		t.Fatalf("#18 POST: status=%d body=%s", wPost.Code, wPost.Body.String())
	}

	// Soft-delete "notes"
	softDeleteField(t, r, tok, tenantA, entity, "notes", sx)

	// GET should not 500 and should not show "notes"
	wGet := layoutDoGET(t, r, tok, entity, "edit")
	if wGet.Code != http.StatusOK {
		t.Fatalf("#18 GET after soft-delete: status=%d body=%s", wGet.Code, wGet.Body.String())
	}
	body := parseBody(t, wGet)
	editView := body["data"].(map[string]any)["views"].(map[string]any)["edit"].(map[string]any)
	sections, _ := editView["sections"].([]any)
	for _, secRaw := range sections {
		sec := secRaw.(map[string]any)
		cells, _ := sec["fields"].([]any)
		for _, cellRaw := range cells {
			var fieldName string
			switch v := cellRaw.(type) {
			case string:
				fieldName = v
			case map[string]any:
				fieldName, _ = v["field"].(string)
			}
			if fieldName == "notes" {
				t.Error("#18: soft-deleted field 'notes' still appears in layout GET response")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Test #19: re-add soft-deleted field → layout references come back
// ---------------------------------------------------------------------------

func TestLayout_19_ReaddField(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_19"
	const tenantA = "d5000000-0000-0000-0000-000000000005"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
		{"name": "notes", "label": "Notes", "fieldType": "text"},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	// Save custom layout with "notes"
	layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"columns": 1, "fields": []any{"notes", "title"}}},
		},
	})

	// Soft-delete notes
	softDeleteField(t, r, tok, tenantA, entity, "notes", sx)

	// Re-activate notes by clearing deleted_at (simulates re-add cùng tên — spec §4.5 B-1).
	// The field service rejects re-add of soft-deleted names (FIELD_ALREADY_EXISTS), so we
	// restore directly in DB to test the layout serve-time reactivation behavior.
	readdFieldSQL(t, sx, tenantA, entity, "notes")

	// GET: "notes" should appear again (re-add reactivates the reference)
	wGet := layoutDoGET(t, r, tok, entity, "edit")
	if wGet.Code != http.StatusOK {
		t.Fatalf("#19 GET: status=%d body=%s", wGet.Code, wGet.Body.String())
	}
	body := parseBody(t, wGet)
	editView := body["data"].(map[string]any)["views"].(map[string]any)["edit"].(map[string]any)
	found := false
	sections, _ := editView["sections"].([]any)
	for _, secRaw := range sections {
		sec := secRaw.(map[string]any)
		cells, _ := sec["fields"].([]any)
		for _, cellRaw := range cells {
			var fieldName string
			switch v := cellRaw.(type) {
			case string:
				fieldName = v
			case map[string]any:
				fieldName, _ = v["field"].(string)
			}
			if fieldName == "notes" {
				found = true
			}
		}
	}
	if !found {
		t.Error("#19: 're-added notes' should appear in layout after re-add (spec B-1)")
	}
}

// ---------------------------------------------------------------------------
// Test #20: upsert race — 2 concurrent POSTs → exactly 1 row, no error
// ---------------------------------------------------------------------------

func TestLayout_20_UpsertRace(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_20"
	const tenantA = "d6000000-0000-0000-0000-000000000006"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	postBody := map[string]any{
		"entityName": entity,
		"viewType":   "edit",
		"layoutJson": map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"columns": 1, "fields": []any{"title"}}},
		},
	}

	const n = 5
	var wg sync.WaitGroup
	results := make(chan int, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := layoutDoPOST(t, r, tok, postBody)
			results <- w.Code
		}()
	}
	wg.Wait()
	close(results)

	for code := range results {
		if code != http.StatusOK {
			t.Errorf("#20 concurrent POST: one returned status=%d; want 200", code)
		}
	}

	// Assert exactly 1 row in sys_layouts
	var count int
	if err := sx.Get(&count, "SELECT COUNT(*) FROM sys_layouts WHERE tenant_id=? AND entity_name=? AND view_type=?", tenantA, entity, "edit"); err != nil {
		t.Fatalf("#20 count query: %v", err)
	}
	if count != 1 {
		t.Errorf("#20: sys_layouts rows for (tenantA, %s, edit) = %d; want exactly 1", entity, count)
	}
}

// ---------------------------------------------------------------------------
// Test #21: Entity with 0 user fields → valid non-empty layout
// ---------------------------------------------------------------------------

func TestLayout_21_ZeroFields(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_21"
	const tenantA = "d7000000-0000-0000-0000-000000000007"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, nil) // no user fields
	t.Cleanup(func() {
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	w := layoutDoGET(t, r, tok, entity, "")
	if w.Code != http.StatusOK {
		t.Fatalf("#21 GET: status=%d body=%s", w.Code, w.Body.String())
	}
	body := parseBody(t, w)
	views := body["data"].(map[string]any)["views"].(map[string]any)

	// All 3 views must be present and non-null
	for _, vt := range []string{"edit", "detail", "list"} {
		vr, ok := views[vt]
		if !ok || vr == nil {
			t.Errorf("#21: views[%s] is nil or missing", vt)
		}
	}

	// Round-trip invariant (spec §1.2 / §4.2): POST the default back verbatim — must be 200.
	// This is the discriminating check: it fails if the generator emits layout that the
	// validator rejects (e.g. "id" fallback blocked by allowlist, empty section rejected).
	for _, vt := range []string{"edit", "detail", "list"} {
		vr := views[vt].(map[string]any)
		// Strip the envelope-only "isDefault" key — the saved layout_json never has it.
		layoutJson := map[string]any{}
		for k, v := range vr {
			if k != "isDefault" {
				layoutJson[k] = v
			}
		}
		postBody := map[string]any{
			"entityName": entity,
			"viewType":   vt,
			"layoutJson": layoutJson,
		}
		pw := layoutDoPOST(t, r, tok, postBody)
		if pw.Code != http.StatusOK {
			t.Errorf("#21 round-trip POST view=%s: status=%d body=%s", vt, pw.Code, pw.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// Test #22: Entity with only rich_text/relation fields → edit fullWidth; list ≥id column
// ---------------------------------------------------------------------------

func TestLayout_22_RichTextAndRelation(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_22"
	const entityTarget = "it_layout_22_target"
	const tenantA = "d8000000-0000-0000-0000-000000000008"
	tok := mintToken(t, tenantA, true)

	// Create the relation target entity first
	layoutCreateEntity(t, r, tok, entityTarget, nil)
	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "body", "label": "Body", "fieldType": "rich_text"},
	})
	t.Cleanup(func() {
		for _, e := range []string{entity, entityTarget} {
			sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, e)
			sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, e)
			sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, e)
		}
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entityTarget + "`")
	})

	w := layoutDoGET(t, r, tok, entity, "")
	if w.Code != http.StatusOK {
		t.Fatalf("#22 GET: status=%d body=%s", w.Code, w.Body.String())
	}
	body := parseBody(t, w)
	views := body["data"].(map[string]any)["views"].(map[string]any)

	// Edit: body should be fullWidth
	editView := views["edit"].(map[string]any)
	sections, _ := editView["sections"].([]any)
	foundFullWidth := false
	for _, secRaw := range sections {
		sec := secRaw.(map[string]any)
		cells, _ := sec["fields"].([]any)
		for _, cellRaw := range cells {
			if obj, ok := cellRaw.(map[string]any); ok {
				if obj["field"] == "body" {
					if fw, _ := obj["fullWidth"].(bool); fw {
						foundFullWidth = true
					}
				}
			}
		}
	}
	if !foundFullWidth {
		t.Error("#22: rich_text 'body' should have fullWidth:true in edit layout")
	}

	// List: must have ≥1 column (id fallback or body)
	listView := views["list"].(map[string]any)
	cols, _ := listView["columns"].([]any)
	if len(cols) == 0 {
		t.Error("#22: list columns should not be empty even for heavy-only entity")
	}

	// Round-trip invariant: POST default back verbatim for all views — must be 200.
	for _, vt := range []string{"edit", "detail", "list"} {
		vr := views[vt].(map[string]any)
		layoutJson := map[string]any{}
		for k, v := range vr {
			if k != "isDefault" {
				layoutJson[k] = v
			}
		}
		postBody := map[string]any{
			"entityName": entity,
			"viewType":   vt,
			"layoutJson": layoutJson,
		}
		pw := layoutDoPOST(t, r, tok, postBody)
		if pw.Code != http.StatusOK {
			t.Errorf("#22 round-trip POST view=%s: status=%d body=%s", vt, pw.Code, pw.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// Test #24 (DAILY-QUEUE #4 sortable-columns-from-grid): `sortable` is decided at SERVE time from
// the field's real sort capability — a stored layout's own value (pack literal, old builder
// output) is overridden both ways, so layouts saved before the slice self-heal on the next GET.
// ---------------------------------------------------------------------------

func TestLayout_24_SortableDecidedAtServeTime(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	r := newLayoutTestRouter(t, gdb, sx)

	const entity = "it_layout_24"
	const tenantA = "e2400000-0000-0000-0000-000000000024"
	tok := mintToken(t, tenantA, true)

	layoutCreateEntity(t, r, tok, entity, []map[string]any{
		{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
		{"name": "stage", "label": "Stage", "fieldType": "picklist", "options": map[string]any{"values": []any{"new", "won"}, "indexed": true}},
		{"name": "code", "label": "Code", "fieldType": "text", "isUnique": true},
		{"name": "notes", "label": "Notes", "fieldType": "text"},
	})
	t.Cleanup(func() {
		sx.Exec("DELETE f FROM sys_fields f JOIN sys_entities e ON e.id=f.entity_id WHERE e.tenant_id=? AND e.name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id=? AND name=?", tenantA, entity)
		sx.Exec("DELETE FROM sys_layouts WHERE tenant_id=? AND entity_name=?", tenantA, entity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entity + "`")
	})

	sortableOf := func(w *httptest.ResponseRecorder, step string) map[string]any {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("#24 %s GET: status=%d body=%s", step, w.Code, w.Body.String())
		}
		listView := parseBody(t, w)["data"].(map[string]any)["views"].(map[string]any)["list"].(map[string]any)
		cols, _ := listView["columns"].([]any)
		out := map[string]any{}
		for _, colRaw := range cols {
			col := colRaw.(map[string]any)
			v, has := col["sortable"]
			if !has {
				t.Errorf("#24 %s: column %q served without a sortable key", step, col["field"])
			}
			out[col["field"].(string)] = v
		}
		return out
	}

	// 1. Generated default (no stored layout): every column carries the engine's verdict.
	def := sortableOf(layoutDoGET(t, r, tok, entity, "list"), "default")
	if def["title"] != false {
		t.Errorf("#24 default: title (plain text) sortable=%v; want false", def["title"])
	}

	// 2. A stored layout that lies both ways (pack-style literal on a text field; nothing on the
	//    fields the engine CAN sort) is accepted — the key is tolerated on write, never trusted.
	wPost := layoutDoPOST(t, r, tok, map[string]any{
		"entityName": entity,
		"viewType":   "list",
		"layoutJson": map[string]any{
			"version": 1,
			"columns": []any{
				map[string]any{"field": "title", "sortable": true},
				map[string]any{"field": "stage"},
				map[string]any{"field": "code", "sortable": false},
				map[string]any{"field": "notes", "sortable": true},
			},
			"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
		},
	})
	if wPost.Code != http.StatusOK {
		t.Fatalf("#24 POST: status=%d body=%s", wPost.Code, wPost.Body.String())
	}

	// 3. Served flags come from buildSortAllowlist, not from what was stored.
	got := sortableOf(layoutDoGET(t, r, tok, entity, "list"), "stored")
	want := map[string]any{"title": false, "stage": true, "code": true, "notes": false}
	for f, w := range want {
		if got[f] != w {
			t.Errorf("#24 stored: sortable[%q] = %v; want %v", f, got[f], w)
		}
	}
}
