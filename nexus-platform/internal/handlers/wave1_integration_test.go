//go:build integration

// Integration tests for Wave 1 "Frontend-unblock" sprint (RM-02, RM-03, RM-05).
// Run with: go test -tags=integration ./internal/handlers/
//
// Requires a reachable MySQL 8 with all migrations applied (nexus-mysql docker container).
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// ---- shared helpers -------------------------------------------------------

// newW1Router builds a router with both Admin and Metadata handlers.
// The MetadataHandler is wired with a real RoleRepository so that the
// GET /metadata/permissions/me with-role happy path (RM-05 should-fix #2) is exercised.
func newW1Router(t *testing.T) (*gin.Engine, *sqlx.DB) {
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
	roleRepo := auth.NewRoleRepository(gdb)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:    NewAdminEntityHandler(svc, fieldSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()).WithRoleActionsLoader(roleRepo),
	}, zap.NewNop())
	return r, sx
}

// doReq is a generic HTTP helper.
func doReq(t *testing.T, r *gin.Engine, method, path string, body any, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody []byte
	if body != nil {
		var err error
		reqBody, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	setAuth(t, req, tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// parseW1Body decodes the response body into a map.
func parseW1Body(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("parse body: %v\nbody: %s", err, w.Body.String())
	}
	return m
}

// cleanupEntitiesLike removes all test entities matching a name prefix for a given tenant.
func cleanupEntitiesLike(t *testing.T, sx *sqlx.DB, tenant, prefix string) {
	t.Helper()
	t.Cleanup(func() {
		sx.Exec("DELETE FROM sys_fields WHERE entity_id IN (SELECT id FROM sys_entities WHERE tenant_id = ? AND name LIKE ?)", tenant, prefix+"%")
		// Hard-delete including soft-deleted (unscoped) to free the name for future runs.
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id = ? AND name LIKE ?", tenant, prefix+"%")
	})
}

// uniqueSuffix returns a short unique suffix for test entity names derived from t.Name.
func uniqueSuffix(t *testing.T) string {
	return fmt.Sprintf("%x", len(t.Name()))
}

// ---- RM-02: GET /metadata/fields/:entity ----------------------------------

// TestRM02_ListFields_ParsedOptions creates an entity with a picklist field, then calls
// GET /metadata/fields/:entity and asserts options is a JSON object (not a raw string).
func TestRM02_ListFields_ParsedOptions(t *testing.T) {
	r, sx := newW1Router(t)
	tenant := fmt.Sprintf("rm02-tenant-%s", uniqueSuffix(t))
	entityName := fmt.Sprintf("rm02_widgets_%s", uniqueSuffix(t))

	cleanupEntitiesLike(t, sx, tenant, "rm02_")
	t.Cleanup(func() { sx.Exec("DROP TABLE IF EXISTS `obj_" + entityName + "`") })

	// Create entity with a picklist field.
	w := doReq(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name":  entityName,
		"label": "RM-02 Widgets",
		"fields": []map[string]any{
			{
				"name":      "status",
				"label":     "Status",
				"fieldType": "picklist",
				"options":   map[string]any{"values": []string{"active", "inactive"}},
			},
		},
	}, tenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("create entity: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// GET /metadata/fields/:entity
	w = doReq(t, r, http.MethodGet, "/api/v1/metadata/fields/"+entityName, nil, tenant)
	if w.Code != http.StatusOK {
		t.Fatalf("list fields: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseW1Body(t, w)
	data, ok := resp["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("expected non-empty data array, got %v", resp["data"])
	}

	// Find the picklist field and check options is a map (not a string).
	for _, raw := range data {
		f, ok := raw.(map[string]any)
		if !ok || f["name"] != "status" {
			continue
		}
		opts, ok := f["options"].(map[string]any)
		if !ok {
			t.Fatalf("options should be a JSON object, got %T: %v", f["options"], f["options"])
		}
		vals, ok := opts["values"].([]any)
		if !ok || len(vals) == 0 {
			t.Fatalf("options.values should be non-empty array, got %v", opts["values"])
		}
		return
	}
	t.Fatal("picklist field 'status' not found in response")
}

// TestRM02_ListFields_CrossTenant404 verifies that requesting fields for an entity that
// belongs to a different tenant returns 404 (IDOR guard, RM-02).
func TestRM02_ListFields_CrossTenant404(t *testing.T) {
	r, sx := newW1Router(t)
	ownerTenant := fmt.Sprintf("rm02-owner-%s", uniqueSuffix(t))
	attackerTenant := fmt.Sprintf("rm02-atk-%s", uniqueSuffix(t))
	entityName := fmt.Sprintf("rm02_secret_%s", uniqueSuffix(t))

	cleanupEntitiesLike(t, sx, ownerTenant, "rm02_secret_")
	t.Cleanup(func() { sx.Exec("DROP TABLE IF EXISTS `obj_" + entityName + "`") })

	// Owner creates the entity.
	w := doReq(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": entityName, "label": "Secret Entity", "fields": []any{},
	}, ownerTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("create entity: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Attacker tenant tries to list fields — must get 404.
	w = doReq(t, r, http.MethodGet, "/api/v1/metadata/fields/"+entityName, nil, attackerTenant)
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant fields access: expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- RM-03: PUT + DELETE /admin/entities/:name ----------------------------

// TestRM03_UpdateEntity_Label updates the label and checks the response.
func TestRM03_UpdateEntity_Label(t *testing.T) {
	r, sx := newW1Router(t)
	tenant := fmt.Sprintf("rm03-upd-%s", uniqueSuffix(t))
	entityName := fmt.Sprintf("rm03_upd_%s", uniqueSuffix(t))

	cleanupEntitiesLike(t, sx, tenant, "rm03_upd_")
	t.Cleanup(func() { sx.Exec("DROP TABLE IF EXISTS `obj_" + entityName + "`") })

	// Create.
	w := doReq(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": entityName, "label": "Old Label", "fields": []any{},
	}, tenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	// PUT — update label only.
	w = doReq(t, r, http.MethodPut, "/api/v1/admin/entities/"+entityName,
		map[string]any{"label": "New Label"}, tenant)
	if w.Code != http.StatusOK {
		t.Fatalf("update entity: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseW1Body(t, w)
	data, _ := resp["data"].(map[string]any)
	entity, _ := data["entity"].(map[string]any)
	if entity["label"] != "New Label" {
		t.Errorf("expected label 'New Label', got %v", entity["label"])
	}
	// Name must be unchanged.
	if entity["name"] != entityName {
		t.Errorf("name should be immutable, got %v", entity["name"])
	}
}

// TestRM03_UpdateEntity_CrossTenant404 verifies that updating another tenant's entity
// returns 404 (IDOR guard, RM-03).
func TestRM03_UpdateEntity_CrossTenant404(t *testing.T) {
	r, sx := newW1Router(t)
	ownerTenant := fmt.Sprintf("rm03-own-%s", uniqueSuffix(t))
	attackerTenant := fmt.Sprintf("rm03-atk-%s", uniqueSuffix(t))
	entityName := fmt.Sprintf("rm03_idor_%s", uniqueSuffix(t))

	cleanupEntitiesLike(t, sx, ownerTenant, "rm03_idor_")
	t.Cleanup(func() { sx.Exec("DROP TABLE IF EXISTS `obj_" + entityName + "`") })

	w := doReq(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": entityName, "label": "Secret", "fields": []any{},
	}, ownerTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	// Attacker updates → 404.
	w = doReq(t, r, http.MethodPut, "/api/v1/admin/entities/"+entityName,
		map[string]any{"label": "Hacked"}, attackerTenant)
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant update: expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRM03_DeleteEntity_SoftDeletes verifies that DELETE soft-deletes the entity
// and the physical obj_* table is preserved.
func TestRM03_DeleteEntity_SoftDeletes(t *testing.T) {
	r, sx := newW1Router(t)
	tenant := fmt.Sprintf("rm03-del-%s", uniqueSuffix(t))
	entityName := fmt.Sprintf("rm03_del_%s", uniqueSuffix(t))

	t.Cleanup(func() {
		sx.Exec("DELETE FROM sys_fields WHERE entity_id IN (SELECT id FROM sys_entities WHERE tenant_id = ? AND name LIKE 'rm03_del_%')", tenant)
		sx.Exec("DELETE FROM sys_entities WHERE tenant_id = ? AND name LIKE 'rm03_del_%'", tenant)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entityName + "`")
	})

	// Create.
	w := doReq(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": entityName, "label": "To Delete", "fields": []any{},
	}, tenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	// DELETE.
	w = doReq(t, r, http.MethodDelete, "/api/v1/admin/entities/"+entityName, nil, tenant)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// GET /metadata/entities — deleted entity must not appear.
	w = doReq(t, r, http.MethodGet, "/api/v1/metadata/entities", nil, tenant)
	if w.Code != http.StatusOK {
		t.Fatalf("list entities: %d %s", w.Code, w.Body.String())
	}
	resp := parseW1Body(t, w)
	data, _ := resp["data"].([]any)
	for _, raw := range data {
		e, _ := raw.(map[string]any)
		if e["name"] == entityName {
			t.Errorf("deleted entity %q still appears in /metadata/entities", entityName)
		}
	}

	// Physical table must still exist (soft-delete only).
	var n int
	sx.QueryRow("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
		"obj_"+entityName).Scan(&n)
	if n == 0 {
		t.Errorf("physical table obj_%s should be preserved after soft-delete", entityName)
	}
}

// TestRM03_RecreateSameName verifies that after soft-delete the name is freed for reuse
// AND the recreated entity sees zero records (data-bleed fix, RM-03 BLOCKER).
//
// This test uses two tenants to also prove safety of the shared-table design: because
// obj_<name> is shared across tenants (not per-tenant), the fix must be DML-scoped —
// tenant A deleting its entity must NOT corrupt tenant B's live data in the same table.
func TestRM03_RecreateSameName(t *testing.T) {
	r, sx := newW1Router(t)
	tenantA := fmt.Sprintf("rm03-rca-%s", uniqueSuffix(t))
	tenantB := fmt.Sprintf("rm03-rcb-%s", uniqueSuffix(t))
	entityName := fmt.Sprintf("rm03_rc_%s", uniqueSuffix(t))

	t.Cleanup(func() {
		for _, tn := range []string{tenantA, tenantB} {
			sx.Exec("DELETE FROM sys_fields WHERE entity_id IN (SELECT id FROM sys_entities WHERE tenant_id = ? AND name LIKE 'rm03_rc_%')", tn)
			sx.Exec("DELETE FROM sys_entities WHERE tenant_id = ? AND name LIKE 'rm03_rc_%'", tn)
		}
		sx.Exec("DROP TABLE IF EXISTS `obj_" + entityName + "`")
	})

	// --- Tenant A: create entity, insert 2 records directly into the physical table. ---
	w := doReq(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": entityName, "label": "First", "fields": []any{},
	}, tenantA)
	if w.Code != http.StatusCreated {
		t.Fatalf("tenantA first create: %d %s", w.Code, w.Body.String())
	}
	// Insert 2 records for tenant A directly so we can later assert they're hidden.
	for i := 0; i < 2; i++ {
		sx.MustExec(
			fmt.Sprintf("INSERT INTO `obj_%s` (id, tenant_id, created_by, data) VALUES (?, ?, ?, '{}')", entityName),
			uuid.NewString(), tenantA, "test-user",
		)
	}

	// --- Tenant B: create the same entity name, insert 1 record. ---
	w = doReq(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": entityName, "label": "TenantB Entity", "fields": []any{},
	}, tenantB)
	if w.Code != http.StatusCreated {
		t.Fatalf("tenantB create: %d %s", w.Code, w.Body.String())
	}
	sx.MustExec(
		fmt.Sprintf("INSERT INTO `obj_%s` (id, tenant_id, created_by, data) VALUES (?, ?, ?, '{}')", entityName),
		uuid.NewString(), tenantB, "test-user",
	)

	// --- Tenant A: DELETE entity. ---
	w = doReq(t, r, http.MethodDelete, "/api/v1/admin/entities/"+entityName, nil, tenantA)
	if w.Code != http.StatusNoContent {
		t.Fatalf("tenantA delete: %d %s", w.Code, w.Body.String())
	}

	// --- Tenant B: its record must still be active (shared table safety). ---
	var bActive int
	sx.QueryRow(
		fmt.Sprintf("SELECT COUNT(*) FROM `obj_%s` WHERE tenant_id = ? AND deleted_at IS NULL", entityName),
		tenantB,
	).Scan(&bActive)
	if bActive != 1 {
		t.Errorf("tenantB record should be unaffected after tenantA delete, got %d active rows", bActive)
	}

	// --- Tenant A: recreate same name → must succeed (201). ---
	w = doReq(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": entityName, "label": "Second", "fields": []any{},
	}, tenantA)
	if w.Code != http.StatusCreated {
		t.Fatalf("tenantA recreate same name: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// --- DATA-BLEED CHECK: tenant A's new entity must see 0 records. ---
	var aActive int
	sx.QueryRow(
		fmt.Sprintf("SELECT COUNT(*) FROM `obj_%s` WHERE tenant_id = ? AND deleted_at IS NULL", entityName),
		tenantA,
	).Scan(&aActive)
	if aActive != 0 {
		t.Errorf("data-bleed: tenantA recreated entity sees %d old records — expected 0", aActive)
	}
}

// ---- RM-05: GET /metadata/permissions/me ----------------------------------

// TestRM05_AdminPermissions verifies that an admin user receives the wildcard grant.
func TestRM05_AdminPermissions(t *testing.T) {
	r, _ := newW1Router(t)
	// tokenForTenant mints IsAdmin=true tokens (see auth_bearer_test_helpers).
	w := doReq(t, r, http.MethodGet, "/api/v1/metadata/permissions/me", nil, "rm05-admin-tenant")
	if w.Code != http.StatusOK {
		t.Fatalf("permissions/me: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseW1Body(t, w)
	data, ok := resp["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("admin should receive at least one permission, got %v", resp["data"])
	}
	grant, _ := data[0].(map[string]any)
	if grant["entityName"] != "*" || grant["action"] != "*" || grant["accessLevel"] != "all" {
		t.Errorf("admin wildcard grant mismatch: %v", grant)
	}
}

// TestRM05_NoRole_EmptyPermissions verifies that a non-admin token without a roleId
// gets an empty permissions array (strict default-deny, RM-05).
func TestRM05_NoRole_EmptyPermissions(t *testing.T) {
	r, _ := newW1Router(t)
	// Build a non-admin token with no role.
	tok, err := accessVerifier(t).IssueAccessToken(auth.Subject{
		UserID:   "user-no-role",
		TenantID: "rm05-norole-tenant",
		IsAdmin:  false,
		RoleID:   "",
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metadata/permissions/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseW1Body(t, w)
	data, ok := resp["data"].([]any)
	if !ok {
		t.Fatalf("expected data array, got %T: %v", resp["data"], resp["data"])
	}
	if len(data) != 0 {
		t.Errorf("user with no role should have empty permissions, got %v", data)
	}
}

// TestRM05_WithRole_Permissions is the RM-05 core happy path (should-fix #2):
// a non-admin user WITH a role gets back that role's actual acl_role_actions grants —
// not empty, not wildcard. Exercises the real ListActionsForRole DB path via the wired
// RoleRepository. Verifies camelCase field names, tenant scoping, and exact grant shape.
func TestRM05_WithRole_Permissions(t *testing.T) {
	r, _ := newW1Router(t)
	gdb, err := gorm.Open(gormmysql.Open(dsn()), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	tenantID := uuid.NewString() // must be a UUID matching a real tenants row
	roleID := uuid.NewString()
	slug := fmt.Sprintf("rm05-role-%s", uniqueSuffix(t))

	// Seed tenant row (acl_roles.tenant_id has FK → tenants.id).
	if err := gdb.Exec(
		"INSERT INTO tenants (id, slug, name, status, created_at, updated_at) VALUES (?, ?, ?, 'active', NOW(), NOW())",
		tenantID, slug, "RM05 Role Test Tenant",
	).Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	// Seed role + 2 grants directly into the DB; clean up after the test.
	t.Cleanup(func() {
		gdb.Exec("DELETE FROM acl_role_actions WHERE role_id = ?", roleID)
		gdb.Exec("DELETE FROM acl_roles WHERE id = ?", roleID)
		gdb.Exec("DELETE FROM tenants WHERE id = ?", tenantID)
	})

	if err := gdb.Exec(
		"INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, ?, ?)",
		roleID, tenantID, "testrole", "Test Role",
	).Error; err != nil {
		t.Fatalf("seed role: %v", err)
	}
	grants := []struct{ entity, action, level string }{
		{"accounts", "view", "all"},
		{"accounts", "edit", "owner"},
	}
	for _, g := range grants {
		if err := gdb.Exec(
			"INSERT INTO acl_role_actions (id, role_id, entity_name, action, access_level) VALUES (?, ?, ?, ?, ?)",
			uuid.NewString(), roleID, g.entity, g.action, g.level,
		).Error; err != nil {
			t.Fatalf("seed grant %v: %v", g, err)
		}
	}

	// Mint a non-admin token carrying the seeded roleID, scoped to the same tenant.
	tok, err := accessVerifier(t).IssueAccessToken(auth.Subject{
		UserID:   "user-with-role",
		TenantID: tenantID,
		IsAdmin:  false,
		RoleID:   roleID,
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metadata/permissions/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseW1Body(t, w)
	data, ok := resp["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("expected non-empty data, got %v", resp["data"])
	}
	if len(data) != 2 {
		t.Errorf("expected 2 grants, got %d: %v", len(data), data)
	}

	// Verify camelCase field names and exact grant shape.
	foundView, foundEdit := false, false
	for _, raw := range data {
		g, _ := raw.(map[string]any)
		// Fields must be camelCase: entityName, action, accessLevel.
		if g["entityName"] != "accounts" {
			t.Errorf("entityName should be 'accounts', got %v", g["entityName"])
		}
		if _, ok := g["entity_name"]; ok {
			t.Errorf("snake_case 'entity_name' must not appear in response (want camelCase)")
		}
		switch g["action"] {
		case "view":
			foundView = true
			if g["accessLevel"] != "all" {
				t.Errorf("view grant: expected accessLevel 'all', got %v", g["accessLevel"])
			}
		case "edit":
			foundEdit = true
			if g["accessLevel"] != "owner" {
				t.Errorf("edit grant: expected accessLevel 'owner', got %v", g["accessLevel"])
			}
		}
	}
	if !foundView || !foundEdit {
		t.Errorf("missing grants — foundView=%v foundEdit=%v; data=%v", foundView, foundEdit, data)
	}

	// Tenant-scoping: a different tenant cannot see this role's grants.
	tok2, _ := accessVerifier(t).IssueAccessToken(auth.Subject{
		UserID:   "other-user",
		TenantID: "rm05-other-tenant",
		IsAdmin:  false,
		RoleID:   roleID, // same roleID, wrong tenant
	})
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/metadata/permissions/me", nil)
	req2.Header.Set("Authorization", "Bearer "+tok2)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("cross-tenant: expected 200, got %d", w2.Code)
	}
	resp2 := parseW1Body(t, w2)
	data2, _ := resp2["data"].([]any)
	if len(data2) != 0 {
		t.Errorf("cross-tenant: expected empty grants for wrong-tenant roleID, got %v", data2)
	}
}
