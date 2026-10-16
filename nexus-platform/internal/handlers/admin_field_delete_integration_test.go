//go:build integration

// Integration tests for DELETE /admin/fields/:id (soft-delete). Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// Requires a reachable MySQL 8 with the sys_* tables migrated (docker-compose dev DB).
// Uses the shared dsn()/testTenant helpers from admin_entity_handler_integration_test.go.
package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

const delEntity = "it_del_widgets"
const otherTenant = "88888888-8888-8888-8888-888888888888"

// deleteFieldReq issues DELETE /api/v1/admin/fields/:id authenticated as `tenant` (Bearer
// access token; empty tenant => no token).
func deleteFieldReq(t *testing.T, r *gin.Engine, tenant, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/fields/"+id, nil)
	setAuth(t, req, tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestAdminDeleteField_SoftDeletePreservesColumn drives the full HTTP→service→DB path:
//   - create an entity (with a unique field 'sku' that materializes a physical column),
//   - DELETE that field via the API,
//   - assert 200, sys_fields.deleted_at is set, the field disappears from active GORM
//     reads, and the obj_* table + the field's physical column are STILL present (no DDL).
func TestAdminDeleteField_SoftDeletePreservesColumn(t *testing.T) {
	r, sx := newAdminRouter(t)

	// Use the shared cleanup for the delEntity name across both tenants.
	cleanup := func() {
		for _, ten := range []string{testTenant, otherTenant} {
			sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
			         WHERE e.tenant_id = ? AND e.name = ?`, ten, delEntity)
			sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, ten, delEntity)
		}
		sx.Exec("DROP TABLE IF EXISTS `obj_" + delEntity + "`")
	}
	cleanup()
	t.Cleanup(cleanup)

	body := map[string]any{
		"name":  delEntity,
		"label": "Del Widget",
		"fields": []map[string]any{
			{"name": "title", "label": "Tên", "fieldType": "text", "isRequired": true},
			{"name": "sku", "label": "Mã", "fieldType": "text", "isUnique": true},
		},
	}
	if w := postEntity(t, r, testTenant, body); w.Code != http.StatusCreated {
		t.Fatalf("create entity: status = %d; body = %s", w.Code, w.Body.String())
	}

	// Resolve the 'sku' field id (tenant-scoped) directly from the DB.
	var skuID string
	if err := sx.Get(&skuID, `SELECT f.id FROM sys_fields f
		JOIN sys_entities e ON e.id = f.entity_id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ? AND f.deleted_at IS NULL`,
		testTenant, delEntity, "sku"); err != nil {
		t.Fatalf("resolve sku field id: %v", err)
	}

	// Sanity: the unique field materialized a physical column on obj_*.
	var colsBefore int
	if err := sx.Get(&colsBefore, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		"obj_"+delEntity, "sku"); err != nil {
		t.Fatalf("count column before: %v", err)
	}
	if colsBefore != 1 {
		t.Fatalf("unique field 'sku' should have a physical column before delete; got %d", colsBefore)
	}

	// DELETE the field via the API (204 No Content on success per api-conventions).
	if w := deleteFieldReq(t, r, testTenant, skuID); w.Code != http.StatusNoContent {
		t.Fatalf("delete field: status = %d; want 204; body = %s", w.Code, w.Body.String())
	}

	// (a) sys_fields row is soft-deleted: deleted_at is set (row still physically present).
	var deletedAt *string
	if err := sx.Get(&deletedAt, `SELECT deleted_at FROM sys_fields WHERE id = ?`, skuID); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if deletedAt == nil {
		t.Errorf("sys_fields.deleted_at must be set after soft-delete; got NULL")
	}

	// (b) the field is excluded from ACTIVE reads (deleted_at IS NULL filter).
	var active int
	if err := sx.Get(&active, `SELECT COUNT(*) FROM sys_fields WHERE id = ? AND deleted_at IS NULL`, skuID); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 0 {
		t.Errorf("soft-deleted field must be excluded from active listings; active=%d", active)
	}

	// (c) NO DDL ran: the obj_* table and the 'sku' column are STILL present (data preserved).
	var tbls int
	if err := sx.Get(&tbls, `SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, "obj_"+delEntity); err != nil {
		t.Fatalf("count table after: %v", err)
	}
	if tbls != 1 {
		t.Errorf("obj_%s table must survive a soft-delete; got %d", delEntity, tbls)
	}
	var colsAfter int
	if err := sx.Get(&colsAfter, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		"obj_"+delEntity, "sku"); err != nil {
		t.Fatalf("count column after: %v", err)
	}
	if colsAfter != 1 {
		t.Errorf("soft-delete must NOT drop the physical column; sku column count after = %d", colsAfter)
	}
}

// TestAdminDeleteField_NotFoundAndTenantIsolation covers the error/security edges:
//   - no token -> 401,
//   - unknown field id -> 404,
//   - another tenant's field id -> 404 and the field is NOT deleted (no cross-tenant leak).
func TestAdminDeleteField_NotFoundAndTenantIsolation(t *testing.T) {
	r, sx := newAdminRouter(t)
	cleanup := func() {
		for _, ten := range []string{testTenant, otherTenant} {
			sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
			         WHERE e.tenant_id = ? AND e.name = ?`, ten, delEntity)
			sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, ten, delEntity)
		}
		sx.Exec("DROP TABLE IF EXISTS `obj_" + delEntity + "`")
	}
	cleanup()
	t.Cleanup(cleanup)

	// Create the entity under testTenant.
	body := map[string]any{
		"name":  delEntity,
		"label": "Del Widget",
		"fields": []map[string]any{
			{"name": "title", "label": "Tên", "fieldType": "text"},
		},
	}
	if w := postEntity(t, r, testTenant, body); w.Code != http.StatusCreated {
		t.Fatalf("create entity: status = %d; body = %s", w.Code, w.Body.String())
	}
	var titleID string
	if err := sx.Get(&titleID, `SELECT f.id FROM sys_fields f
		JOIN sys_entities e ON e.id = f.entity_id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ? AND f.deleted_at IS NULL`,
		testTenant, delEntity, "title"); err != nil {
		t.Fatalf("resolve title field id: %v", err)
	}

	// No token -> 401 (post-A2: tenant comes from the verified claim, so an unauthenticated
	// request is rejected at the auth gate before the handler runs).
	if w := deleteFieldReq(t, r, "", titleID); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d; want 401", w.Code)
	}

	// Unknown field id -> 404.
	if w := deleteFieldReq(t, r, testTenant, "00000000-0000-0000-0000-000000000000"); w.Code != http.StatusNotFound {
		t.Errorf("unknown field: status = %d; want 404", w.Code)
	}

	// Cross-tenant: otherTenant tries to delete testTenant's field -> 404, and the field
	// must remain active (not soft-deleted) — no cross-tenant write.
	if w := deleteFieldReq(t, r, otherTenant, titleID); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant delete: status = %d; want 404", w.Code)
	}
	var stillActive int
	if err := sx.Get(&stillActive, `SELECT COUNT(*) FROM sys_fields WHERE id = ? AND deleted_at IS NULL`, titleID); err != nil {
		t.Fatalf("count active after cross-tenant attempt: %v", err)
	}
	if stillActive != 1 {
		t.Errorf("cross-tenant delete must NOT soft-delete the field; active=%d", stillActive)
	}
}
