//go:build integration

// Integration test for M3-T1 (FK-on-child relation field at the admin/entity-create layer).
// Replaces spec §7.2 B-4's pre-M3 fail-fast: as of M3 a `relation` field is a SUPPORTED type
// when options.relatedEntity names a target entity. Creating an entity with such a field must
// succeed (201) and materialize the FK column `<f>_id` + its index on the obj_* table. A
// relation field missing options.relatedEntity is still rejected 422
// (FIELD_RELATION_TARGET_REQUIRED) — the "not yet" code is gone, replaced by a structural one.
//
// Run with: go test -tags=integration ./internal/handlers/
// Requires a reachable MySQL 8 (docker-compose dev DB). Reuses the harness in
// admin_entity_handler_integration_test.go (newAdminRouter, postEntity, cleanupEntity).
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestAdminCreateEntity_RelationFieldAccepted(t *testing.T) {
	r, sx := newAdminRouter(t)
	cleanupEntity(sx)
	// The relation field below points at a target entity that must already exist in THIS tenant.
	// Seed it ourselves so the test is self-contained + re-runnable on a clean migrated DB. Use a
	// unique it_* name — NOT the migration-seeded "accounts", which lives in a different tenant and
	// shares the physical obj_accounts table (the shared-table architecture keyed by entity name).
	const relTarget = "it_accounts"
	cleanupTarget := func() {
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, testTenant, relTarget)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + relTarget + "`")
	}
	cleanupTarget()
	t.Cleanup(func() { cleanupEntity(sx); cleanupTarget() })

	if w := postEntity(t, r, testTenant, map[string]any{
		"name":   relTarget,
		"label":  "Accounts",
		"fields": []map[string]any{{"name": "name", "label": "Tên", "fieldType": "text"}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("seed relation target %q: status = %d; body = %s", relTarget, w.Code, w.Body.String())
	}

	body := map[string]any{
		"name":  testEntity,
		"label": "Widget",
		"fields": []map[string]any{
			{"name": "title", "label": "Tên", "fieldType": "text", "isRequired": true},
			{"name": "manager", "label": "Quản lý", "fieldType": "relation",
				"options": map[string]any{"relatedEntity": relTarget}},
		},
	}

	w := postEntity(t, r, testTenant, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("relation field: status = %d; want 201; body = %s", w.Code, w.Body.String())
	}

	// The FK column `manager_id` and its index must exist on the child table.
	var nCol int
	if err := sx.Get(&nCol, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		"obj_"+testEntity, "manager_id"); err != nil {
		t.Fatalf("check column: %v", err)
	}
	if nCol != 1 {
		t.Errorf("obj_%s must have the manager_id FK column after a relation create", testEntity)
	}
	var nIdx int
	if err := sx.Get(&nIdx, `SELECT COUNT(DISTINCT INDEX_NAME) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?`,
		"obj_"+testEntity, "idx_obj_"+testEntity+"_manager_id"); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if nIdx != 1 {
		t.Errorf("obj_%s must have the FK index idx_obj_%s_manager_id", testEntity, testEntity)
	}
}

func TestAdminCreateEntity_RelationFieldMissingTargetRejected(t *testing.T) {
	r, sx := newAdminRouter(t)
	cleanupEntity(sx)
	t.Cleanup(func() { cleanupEntity(sx) })

	body := map[string]any{
		"name":  testEntity,
		"label": "Widget",
		"fields": []map[string]any{
			{"name": "title", "label": "Tên", "fieldType": "text", "isRequired": true},
			{"name": "manager", "label": "Quản lý", "fieldType": "relation"}, // no options.relatedEntity
		},
	}

	w := postEntity(t, r, testTenant, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("relation no target: status = %d; want 422; body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v; raw = %s", err, w.Body.String())
	}
	if resp.Code != "FIELD_RELATION_TARGET_REQUIRED" {
		t.Errorf("relation no target: code = %q; want FIELD_RELATION_TARGET_REQUIRED", resp.Code)
	}
	// No table should have been created for a rejected payload.
	var n int
	if err := sx.Get(&n, `SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, "obj_"+testEntity); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if n != 0 {
		t.Errorf("obj_%s must NOT exist after a rejected create", testEntity)
	}
}
