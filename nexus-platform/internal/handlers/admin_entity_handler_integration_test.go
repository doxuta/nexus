//go:build integration

// Integration tests for the admin entity handler. Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// Requires a reachable MySQL 8 with the sys_* tables migrated (docker-compose dev DB).
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

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const testTenant = "99999999-9999-9999-9999-999999999999"
const testEntity = "it_widgets"

// dsn returns the DSN of the throwaway migrated database provisioned by TestMain (see
// main_test.go). Before #84 this read TEST_DATABASE_URL directly, which broke against a db-less
// DSN ("No database selected"); TestMain now creates+migrates a throwaway DB and points testDSN
// at it. The self-provisioning suites (a4/rm25/mig000012/rm24/layout) strip the db name off this
// value via splitDSN/baseDSNNoDB, so they keep creating their own per-test throwaway DBs.
func dsn() string {
	return testDSN
}

func cleanupEntity(db *sqlx.DB) {
	// Finding B: a `relation` field on/pointing-at testEntity auto-registers a companion 1-N
	// sys_relationships row (ENG-REL2 #4) that this cleanup must also remove — otherwise a
	// second local run of a relation-field test (e.g. admin_field_relation_integration_test.go)
	// re-creates testEntity, AddField tries to re-register the SAME derived registry name, and
	// trips the (tenant_id, name) unique index against the row THIS run's own prior pass left
	// behind, breaking idempotent re-runs without a manual DB reset.
	db.Exec(`DELETE FROM sys_relationships WHERE tenant_id = ? AND (parent_entity_name = ? OR child_entity_name = ?)`,
		testTenant, testEntity, testEntity)
	db.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
	         WHERE e.tenant_id = ? AND e.name = ?`, testTenant, testEntity)
	db.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, testTenant, testEntity)
	db.Exec("DROP TABLE IF EXISTS `obj_" + testEntity + "`")
	// RM-28 Slice B: same idempotent-re-run reasoning as Finding B above, for uniqueness —
	// sys_unique_index is the SOLE enforcement for a plain `is_unique` field now (the physical
	// obj_* index is non-unique), so orphaned pivot rows from a prior local run would falsely
	// 409 a fresh run's deterministic test values. CI unaffected (fresh DB every run).
	db.Exec(`DELETE FROM sys_unique_index WHERE tenant_id = ? AND entity_name = ?`, testTenant, testEntity)
}

func newAdminRouter(t *testing.T) (*gin.Engine, *sqlx.DB) {
	t.Helper()
	sx, err := sqlx.Connect("mysql", dsn())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	gdb, err := gorm.Open(gormmysql.Open(dsn()), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	svc := services.NewEntityService(dynrepo.New(sx), metadata.NewRepository(gdb))
	fieldSvc := services.NewFieldService(dynrepo.New(sx), metadata.NewRepository(gdb))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Mount behind the real A2 chain so admin routes derive tenant from the verified claim.
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin: NewAdminEntityHandler(svc, fieldSvc, zap.NewNop()),
	}, zap.NewNop())
	return r, sx
}

func postEntity(t *testing.T, r *gin.Engine, tenant string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/entities", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	setAuth(t, req, tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func validBody() map[string]any {
	return map[string]any{
		"name":  testEntity,
		"label": "Widget",
		"fields": []map[string]any{
			{"name": "title", "label": "Tên", "fieldType": "text", "isRequired": true},
			{"name": "sku", "label": "Mã", "fieldType": "text", "isUnique": true},
		},
	}
}

func TestAdminCreateEntity(t *testing.T) {
	r, sx := newAdminRouter(t)
	cleanupEntity(sx)
	t.Cleanup(func() { cleanupEntity(sx) })

	// 201 + physical table created
	if w := postEntity(t, r, testTenant, validBody()); w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d; body = %s", w.Code, w.Body.String())
	}
	var n int
	if err := sx.Get(&n, `SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, "obj_"+testEntity); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if n != 1 {
		t.Errorf("obj_%s should exist after create", testEntity)
	}

	// 409 on duplicate name
	if w := postEntity(t, r, testTenant, validBody()); w.Code != http.StatusConflict {
		t.Errorf("duplicate: status = %d; want 409", w.Code)
	}

	// 401 when no Bearer token (post-A2: tenant comes from the verified claim, so an
	// unauthenticated request is rejected at the auth gate before the handler runs).
	if w := postEntity(t, r, "", validBody()); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d; want 401", w.Code)
	}

	// 422 on invalid field type
	bad := validBody()
	bad["name"] = "it_widgets_bad"
	bad["fields"] = []map[string]any{{"name": "x", "label": "X", "fieldType": "nope"}}
	if w := postEntity(t, r, testTenant, bad); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad field type: status = %d; want 422", w.Code)
	}
}
