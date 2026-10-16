//go:build integration

// Integration tests for PUT /admin/fields/:id (edit mutable field attributes). Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// Requires a reachable MySQL 8 with the sys_* tables migrated (docker-compose dev DB).
// Reuses the shared dsn()/testTenant/otherTenant/postEntity/newAdminRouter helpers from
// admin_entity_handler_integration_test.go and admin_field_delete_integration_test.go.
package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

const editEntity = "it_edit_widgets"

// putFieldReq issues PUT /api/v1/admin/fields/:id with the given tenant header + JSON body.
func putFieldReq(t *testing.T, r *gin.Engine, tenant, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/fields/"+id, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	setAuth(t, req, tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// cleanupEditEntity removes the editEntity across both tenants and drops its obj_* table.
func cleanupEditEntity(sx *sqlx.DB) {
	for _, ten := range []string{testTenant, otherTenant} {
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name = ?`, ten, editEntity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, ten, editEntity)
	}
	sx.Exec("DROP TABLE IF EXISTS `obj_" + editEntity + "`")
}

// resolveFieldID returns the active field id for (testTenant, editEntity, fieldName).
func resolveFieldID(t *testing.T, sx *sqlx.DB, fieldName string) string {
	t.Helper()
	var id string
	if err := sx.Get(&id, `SELECT f.id FROM sys_fields f
		JOIN sys_entities e ON e.id = f.entity_id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ? AND f.deleted_at IS NULL`,
		testTenant, editEntity, fieldName); err != nil {
		t.Fatalf("resolve %q field id: %v", fieldName, err)
	}
	return id
}

// seedEditEntity creates editEntity with a text 'status' field and a picklist 'stage'
// field, returning the router + sqlx handle and registering cleanup.
func seedEditEntity(t *testing.T) (*gin.Engine, *sqlx.DB) {
	t.Helper()
	r, sx := newAdminRouter(t)
	cleanupEditEntity(sx)
	t.Cleanup(func() { cleanupEditEntity(sx) })

	body := map[string]any{
		"name":  editEntity,
		"label": "Edit Widget",
		"fields": []map[string]any{
			{"name": "status", "label": "Trạng thái", "fieldType": "text", "isRequired": false},
			{"name": "stage", "label": "Giai đoạn", "fieldType": "picklist",
				"options": map[string]any{"values": []any{"a", "b"}}},
		},
	}
	if w := postEntity(t, r, testTenant, body); w.Code != http.StatusCreated {
		t.Fatalf("create entity: status = %d; body = %s", w.Code, w.Body.String())
	}
	return r, sx
}

// TestAdminEditField_UpdatesMutableAttributes drives the full HTTP→service→DB path,
// asserting each mutable attribute (label, required, default, options) is persisted and
// that NO schema ALTER occurs (the obj_* table is unchanged by an edit).
func TestAdminEditField_UpdatesMutableAttributes(t *testing.T) {
	r, sx := seedEditEntity(t)
	statusID := resolveFieldID(t, sx, "status")

	// Snapshot the obj_* column set before the edit to prove no DDL runs.
	var colsBefore int
	if err := sx.Get(&colsBefore, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, "obj_"+editEntity); err != nil {
		t.Fatalf("count columns before: %v", err)
	}

	// Edit label + required + default on the 'status' text field. fieldType is echoed
	// unchanged so the immutability guard passes.
	w := putFieldReq(t, r, testTenant, statusID, map[string]any{
		"label":        "Tình trạng",
		"isRequired":   true,
		"defaultValue": "active",
		"fieldType":    "text",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("edit status: status = %d; body = %s", w.Code, w.Body.String())
	}

	var (
		label      string
		isRequired bool
		defaultVal string
	)
	if err := sx.Get(&label, `SELECT label FROM sys_fields WHERE id = ?`, statusID); err != nil {
		t.Fatalf("read label: %v", err)
	}
	if err := sx.Get(&isRequired, `SELECT is_required FROM sys_fields WHERE id = ?`, statusID); err != nil {
		t.Fatalf("read is_required: %v", err)
	}
	if err := sx.Get(&defaultVal, `SELECT default_val FROM sys_fields WHERE id = ?`, statusID); err != nil {
		t.Fatalf("read default_val: %v", err)
	}
	if label != "Tình trạng" {
		t.Errorf("label = %q; want %q", label, "Tình trạng")
	}
	if !isRequired {
		t.Errorf("is_required = %v; want true", isRequired)
	}
	if defaultVal != "active" {
		t.Errorf("default_val = %q; want %q", defaultVal, "active")
	}

	// Edit the picklist 'stage' field's options.
	stageID := resolveFieldID(t, sx, "stage")
	w = putFieldReq(t, r, testTenant, stageID, map[string]any{
		"label":     "Giai đoạn",
		"fieldType": "picklist",
		"options":   map[string]any{"values": []any{"a", "b", "c"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("edit stage options: status = %d; body = %s", w.Code, w.Body.String())
	}
	var opts string
	if err := sx.Get(&opts, `SELECT options FROM sys_fields WHERE id = ?`, stageID); err != nil {
		t.Fatalf("read options: %v", err)
	}
	if opts != `{"values": ["a", "b", "c"]}` && opts != `{"values":["a","b","c"]}` {
		t.Errorf("options = %q; want updated 3-value picklist", opts)
	}

	// NO DDL: the obj_* column set is identical after edits.
	var colsAfter int
	if err := sx.Get(&colsAfter, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, "obj_"+editEntity); err != nil {
		t.Fatalf("count columns after: %v", err)
	}
	if colsAfter != colsBefore {
		t.Errorf("edit must not ALTER obj_* (cols before=%d after=%d)", colsBefore, colsAfter)
	}
}

// TestAdminEditField_RejectsFieldTypeChange asserts that attempting to change a field's
// fieldType is rejected with HTTP 422 and the stored type is untouched.
func TestAdminEditField_RejectsFieldTypeChange(t *testing.T) {
	r, sx := seedEditEntity(t)
	statusID := resolveFieldID(t, sx, "status")

	w := putFieldReq(t, r, testTenant, statusID, map[string]any{
		"label":     "Trạng thái",
		"fieldType": "number", // text -> number: forbidden
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("fieldType change: status = %d; want 422; body = %s", w.Code, w.Body.String())
	}

	// The stored type must still be 'text' — the rejected edit changed nothing.
	var ft string
	if err := sx.Get(&ft, `SELECT field_type FROM sys_fields WHERE id = ?`, statusID); err != nil {
		t.Fatalf("read field_type: %v", err)
	}
	if ft != "text" {
		t.Errorf("field_type = %q; want unchanged 'text'", ft)
	}
}

// TestAdminEditField_NotFoundAndTenantIsolation covers the error/security edges:
//   - no token -> 401,
//   - unknown field id -> 404,
//   - another tenant's field id -> 404 and the field is NOT modified (no cross-tenant write).
func TestAdminEditField_NotFoundAndTenantIsolation(t *testing.T) {
	r, sx := seedEditEntity(t)
	statusID := resolveFieldID(t, sx, "status")

	// No token -> 401 (post-A2: tenant comes from the verified claim).
	if w := putFieldReq(t, r, "", statusID, map[string]any{"label": "x"}); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d; want 401", w.Code)
	}

	// Unknown field id -> 404.
	if w := putFieldReq(t, r, testTenant, "00000000-0000-0000-0000-000000000000",
		map[string]any{"label": "x"}); w.Code != http.StatusNotFound {
		t.Errorf("unknown field: status = %d; want 404", w.Code)
	}

	// Cross-tenant: otherTenant tries to edit testTenant's field -> 404, and the field's
	// label must remain unchanged (no cross-tenant write).
	if w := putFieldReq(t, r, otherTenant, statusID,
		map[string]any{"label": "hijacked", "fieldType": "text"}); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant edit: status = %d; want 404", w.Code)
	}
	var label string
	if err := sx.Get(&label, `SELECT label FROM sys_fields WHERE id = ?`, statusID); err != nil {
		t.Fatalf("read label after cross-tenant attempt: %v", err)
	}
	if label == "hijacked" {
		t.Errorf("cross-tenant edit must NOT modify the field; label was overwritten to %q", label)
	}
}
