//go:build integration

// Integration tests for the B-1 fix (spec §7.2): re-adding a field after a soft-delete.
// Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// Requires a reachable MySQL 8 with the sys_* tables migrated (docker-compose dev DB).
// Uses the shared dsn()/newAdminRouter/postEntity helpers from
// admin_entity_handler_integration_test.go and deleteFieldReq from the delete test.
package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

const readdEntity = "it_readd_widgets"

// addFieldReq issues POST /api/v1/admin/fields with the given tenant header + JSON body.
func addFieldReq(t *testing.T, r *gin.Engine, tenant string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/fields", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	setAuth(t, req, tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestAdminReAddField_AfterSoftDelete is the end-to-end acceptance for B-1. It drives the
// full HTTP→service→DB path:
//
//  1. create an entity with a UNIQUE field 'sku' (materializes a physical generated column),
//  2. soft-delete 'sku' (the obj_* column is preserved per §4.4),
//  3. re-add 'sku' via POST /admin/fields.
//
// Before the fix the re-add (a) slipped past the dup-check (ListFields hides the
// soft-deleted row) and (b) drove ALTER ADD COLUMN onto the still-present generated column
// -> 500. After the fix the dup-check sees the soft-deleted row and rejects the re-add with
// 409 — no second sys_fields row, no 500, the physical column untouched.
func TestAdminReAddField_AfterSoftDelete(t *testing.T) {
	r, sx := newAdminRouter(t)

	cleanup := func() {
		// Unscoped delete of all (incl. soft-deleted) sys_fields rows for this entity.
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name = ?`, testTenant, readdEntity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, testTenant, readdEntity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + readdEntity + "`")
	}
	cleanup()
	t.Cleanup(cleanup)

	// (1) Create the entity with a unique 'sku'.
	body := map[string]any{
		"name":  readdEntity,
		"label": "Re-add Widget",
		"fields": []map[string]any{
			{"name": "title", "label": "Tên", "fieldType": "text", "isRequired": true},
			{"name": "sku", "label": "Mã", "fieldType": "text", "isUnique": true},
		},
	}
	if w := postEntity(t, r, testTenant, body); w.Code != http.StatusCreated {
		t.Fatalf("create entity: status = %d; body = %s", w.Code, w.Body.String())
	}

	// Resolve the 'sku' field id (tenant-scoped, active).
	var skuID string
	if err := sx.Get(&skuID, `SELECT f.id FROM sys_fields f
		JOIN sys_entities e ON e.id = f.entity_id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ? AND f.deleted_at IS NULL`,
		testTenant, readdEntity, "sku"); err != nil {
		t.Fatalf("resolve sku field id: %v", err)
	}

	// The physical generated column exists.
	var colBefore int
	if err := sx.Get(&colBefore, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		"obj_"+readdEntity, "sku"); err != nil {
		t.Fatalf("count column before: %v", err)
	}
	if colBefore != 1 {
		t.Fatalf("unique 'sku' should have a physical column before delete; got %d", colBefore)
	}

	// (2) Soft-delete 'sku'.
	if w := deleteFieldReq(t, r, testTenant, skuID); w.Code != http.StatusNoContent {
		t.Fatalf("soft-delete sku: status = %d; want 204; body = %s", w.Code, w.Body.String())
	}
	// Sanity: active read no longer sees 'sku', but the column survives.
	var active int
	if err := sx.Get(&active, `SELECT COUNT(*) FROM sys_fields WHERE id = ? AND deleted_at IS NULL`, skuID); err != nil {
		t.Fatalf("count active after delete: %v", err)
	}
	if active != 0 {
		t.Fatalf("soft-deleted 'sku' must be excluded from active reads; active=%d", active)
	}

	// (3) Re-add 'sku' as a unique field -> must be 409 (dup-check sees the soft-deleted row).
	reAdd := map[string]any{
		"entityName": readdEntity,
		"name":       "sku",
		"label":      "Mã (lại)",
		"fieldType":  "text",
		"isUnique":   true,
	}
	w := addFieldReq(t, r, testTenant, reAdd)
	if w.Code != http.StatusConflict {
		t.Fatalf("re-add soft-deleted unique field: status = %d (body=%s); want 409 (not 500/201)",
			w.Code, w.Body.String())
	}

	// No second sys_fields row was created (active still 0; total still exactly 1 incl. deleted).
	var total int
	if err := sx.Get(&total, `SELECT COUNT(*) FROM sys_fields f
		JOIN sys_entities e ON e.id = f.entity_id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ?`,
		testTenant, readdEntity, "sku"); err != nil {
		t.Fatalf("count total sku rows: %v", err)
	}
	if total != 1 {
		t.Errorf("re-add must NOT create a phantom row; total 'sku' rows (incl. deleted) = %d; want 1", total)
	}

	// The physical column is untouched (no DDL ran on the re-add).
	var colAfter int
	if err := sx.Get(&colAfter, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		"obj_"+readdEntity, "sku"); err != nil {
		t.Fatalf("count column after: %v", err)
	}
	if colAfter != 1 {
		t.Errorf("re-add must not alter the physical column; sku column count = %d; want 1", colAfter)
	}
}

// TestAdminReAddOrdinaryField_AfterSoftDelete covers the ordinary-field half of B-1: an
// ordinary (non-unique) field that was soft-deleted must also be rejected on re-add (409),
// so no duplicate sys_fields row is created. Ordinary fields run no DDL either way.
func TestAdminReAddOrdinaryField_AfterSoftDelete(t *testing.T) {
	r, sx := newAdminRouter(t)

	cleanup := func() {
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name = ?`, testTenant, readdEntity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, testTenant, readdEntity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + readdEntity + "`")
	}
	cleanup()
	t.Cleanup(cleanup)

	body := map[string]any{
		"name":  readdEntity,
		"label": "Re-add Widget",
		"fields": []map[string]any{
			{"name": "title", "label": "Tên", "fieldType": "text", "isRequired": true},
			{"name": "summary", "label": "Tóm tắt", "fieldType": "text"},
		},
	}
	if w := postEntity(t, r, testTenant, body); w.Code != http.StatusCreated {
		t.Fatalf("create entity: status = %d; body = %s", w.Code, w.Body.String())
	}

	var sumID string
	if err := sx.Get(&sumID, `SELECT f.id FROM sys_fields f
		JOIN sys_entities e ON e.id = f.entity_id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ? AND f.deleted_at IS NULL`,
		testTenant, readdEntity, "summary"); err != nil {
		t.Fatalf("resolve summary field id: %v", err)
	}

	if w := deleteFieldReq(t, r, testTenant, sumID); w.Code != http.StatusNoContent {
		t.Fatalf("soft-delete summary: status = %d; want 204; body = %s", w.Code, w.Body.String())
	}

	reAdd := map[string]any{
		"entityName": readdEntity,
		"name":       "summary",
		"label":      "Tóm tắt (lại)",
		"fieldType":  "text",
	}
	if w := addFieldReq(t, r, testTenant, reAdd); w.Code != http.StatusConflict {
		t.Fatalf("re-add soft-deleted ordinary field: status = %d (body=%s); want 409",
			w.Code, w.Body.String())
	}

	var total int
	if err := sx.Get(&total, `SELECT COUNT(*) FROM sys_fields f
		JOIN sys_entities e ON e.id = f.entity_id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ?`,
		testTenant, readdEntity, "summary"); err != nil {
		t.Fatalf("count total summary rows: %v", err)
	}
	if total != 1 {
		t.Errorf("ordinary re-add must NOT create a phantom row; total = %d; want 1", total)
	}
}

// TestAdminAddNewUniqueField_StillWorks is a regression guard: the B-1 precheck must not
// break the normal happy path — adding a brand-new unique field to an existing entity still
// materializes its physical column and returns 201.
func TestAdminAddNewUniqueField_StillWorks(t *testing.T) {
	r, sx := newAdminRouter(t)

	cleanup := func() {
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name = ?`, testTenant, readdEntity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, testTenant, readdEntity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + readdEntity + "`")
	}
	cleanup()
	t.Cleanup(cleanup)

	body := map[string]any{
		"name":  readdEntity,
		"label": "Re-add Widget",
		"fields": []map[string]any{
			{"name": "title", "label": "Tên", "fieldType": "text", "isRequired": true},
		},
	}
	if w := postEntity(t, r, testTenant, body); w.Code != http.StatusCreated {
		t.Fatalf("create entity: status = %d; body = %s", w.Code, w.Body.String())
	}

	// Add a brand-new unique field 'barcode' -> 201 + physical column appears.
	add := map[string]any{
		"entityName": readdEntity,
		"name":       "barcode",
		"label":      "Barcode",
		"fieldType":  "text",
		"isUnique":   true,
	}
	if w := addFieldReq(t, r, testTenant, add); w.Code != http.StatusCreated {
		t.Fatalf("add new unique field: status = %d; body = %s", w.Code, w.Body.String())
	}
	var col int
	if err := sx.Get(&col, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		"obj_"+readdEntity, "barcode"); err != nil {
		t.Fatalf("count barcode column: %v", err)
	}
	if col != 1 {
		t.Errorf("new unique field must materialize a physical column; got %d", col)
	}
}
