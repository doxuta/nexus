//go:build integration

// ENG-REL2 #4 (bidirectional / inverse-field relationship registry, migration 000028)
// handler-level integration test: GET /metadata/relationships must surface a 1-N registry row
// (written by POST /admin/fields for a `relation` field) INCLUDING its inverseField and
// childFkColumn keys, camelCase, exactly as GetRelationshipByName/List* now resolve them
// (the pre-000028 hardcoded relationship_type='N-N' predicate would have hidden this row
// entirely from the endpoint).
//
// Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// Requires a reachable MySQL 8 with the sys_* tables migrated INCLUDING migration 000028.
// Reuses the newAdminRouter/postEntity/addFieldReq/setAuth harness from
// admin_entity_handler_integration_test.go / admin_field_readd_integration_test.go.
package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const (
	relInvRegParent = "it_relinvreg_customers"
	relInvRegChild  = "it_relinvreg_orders"
)

// newAdminRouterWithMetadata is newAdminRouter (admin entity/field routes) PLUS the
// MetadataHandler (GET /metadata/relationships), needed only by this file's test.
func newAdminRouterWithMetadata(t *testing.T) (*gin.Engine, *sqlx.DB) {
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
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:    NewAdminEntityHandler(svc, fieldSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
	}, zap.NewNop())
	return r, sx
}

func getRelationships(t *testing.T, r *gin.Engine, tenant, entity string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/v1/metadata/relationships"
	if entity != "" {
		url += "?entity=" + entity
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	setAuth(t, req, tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestGetMetadataRelationships_SurfacesOneToNInverseRegistry is the plan's mandated test:
// GET /metadata/relationships must include a 1-N row (created by POST /admin/fields for a
// `relation` field) with camelCase inverseField/childFkColumn keys and the right values.
func TestGetMetadataRelationships_SurfacesOneToNInverseRegistry(t *testing.T) {
	r, sx := newAdminRouterWithMetadata(t)
	cleanup := func() {
		sx.Exec(`DELETE FROM sys_relationships WHERE tenant_id = ? AND (name LIKE 'it_relinvreg_%')`, testTenant)
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name IN (?, ?)`, testTenant, relInvRegParent, relInvRegChild)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name IN (?, ?)`, testTenant, relInvRegParent, relInvRegChild)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + relInvRegChild + "`")
		sx.Exec("DROP TABLE IF EXISTS `obj_" + relInvRegParent + "`")
	}
	cleanup()
	t.Cleanup(cleanup)

	if w := postEntity(t, r, testTenant, map[string]any{
		"name":   relInvRegParent,
		"label":  "Customers",
		"fields": []map[string]any{{"name": "name", "label": "Tên", "fieldType": "text"}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("seed parent entity: status = %d; body = %s", w.Code, w.Body.String())
	}
	if w := postEntity(t, r, testTenant, map[string]any{
		"name":   relInvRegChild,
		"label":  "Orders",
		"fields": []map[string]any{{"name": "total", "label": "Total", "fieldType": "number"}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("seed child entity: status = %d; body = %s", w.Code, w.Body.String())
	}

	if w := addFieldReq(t, r, testTenant, map[string]any{
		"entityName": relInvRegChild,
		"name":       "customer", "label": "Customer", "fieldType": "relation",
		"options": map[string]any{"relatedEntity": relInvRegParent},
	}); w.Code != http.StatusCreated {
		t.Fatalf("AddField(relation): status = %d; body = %s", w.Code, w.Body.String())
	}

	w := getRelationships(t, r, testTenant, relInvRegParent)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metadata/relationships: status = %d; body = %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data []struct {
			Name             string `json:"name"`
			ParentEntityName string `json:"parentEntityName"`
			ChildEntityName  string `json:"childEntityName"`
			RelationshipType string `json:"relationshipType"`
			InverseField     string `json:"inverseField"`
			ChildFkColumn    string `json:"childFkColumn"`
			JoinTableName    string `json:"joinTableName"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; raw = %s", err, w.Body.String())
	}

	wantName := relInvRegChild + "_customer"
	var found *struct {
		Name             string `json:"name"`
		ParentEntityName string `json:"parentEntityName"`
		ChildEntityName  string `json:"childEntityName"`
		RelationshipType string `json:"relationshipType"`
		InverseField     string `json:"inverseField"`
		ChildFkColumn    string `json:"childFkColumn"`
		JoinTableName    string `json:"joinTableName"`
	}
	for i := range resp.Data {
		if resp.Data[i].Name == wantName {
			found = &resp.Data[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("1-N row %q not found in GET /metadata/relationships response: %s", wantName, w.Body.String())
	}
	if found.RelationshipType != "1-N" {
		t.Errorf("relationshipType = %q; want 1-N", found.RelationshipType)
	}
	if found.ParentEntityName != relInvRegParent || found.ChildEntityName != relInvRegChild {
		t.Errorf("parent/child = %q/%q; want %s/%s", found.ParentEntityName, found.ChildEntityName, relInvRegParent, relInvRegChild)
	}
	if found.ChildFkColumn != "customer_id" {
		t.Errorf("childFkColumn = %q; want customer_id", found.ChildFkColumn)
	}
	if found.InverseField != relInvRegChild {
		t.Errorf("inverseField = %q; want %s (default = child entity name)", found.InverseField, relInvRegChild)
	}
	if found.JoinTableName != "" {
		t.Errorf("joinTableName = %q; want empty/omitted (1-N has no join table)", found.JoinTableName)
	}
}

// TestAddField_DerivedRelationshipNameCollidesWithExistingRow_Returns409 is Finding A's
// regression test (reviewer + security-auditor, both flagged this): the auto-registered 1-N
// sys_relationships row for a `relation` field can collide with an EXISTING relationship row on
// (tenant_id, name) — e.g. an admin previously registered an (unrelated) N-N relationship
// literally named the same as the derived <childEntity>_<fieldName>, or a 1-N row survived an
// entity recreate after soft-delete (RM-21). Before Finding A's fix, that MySQL 1062
// (uq_sysrel_tenant_name) was unclassified and wrapped into a generic 500 (FIELD_ADD_FAILED).
// This proves POST /admin/fields now returns a clean 409 RELATIONSHIP_ALREADY_EXISTS instead —
// the SAME code the N-N RegisterRelationship duplicate-name path already returns — AND that the
// DDL compensation (dropping the just-added `<field>_id` FK column) still runs, leaving no
// orphan column or sys_fields row behind, and the pre-existing row is untouched.
func TestAddField_DerivedRelationshipNameCollidesWithExistingRow_Returns409(t *testing.T) {
	r, sx := newAdminRouter(t)

	const (
		parentEntity = "it_reldup_accounts"
		childEntity  = "it_reldup_contacts"
		fieldName    = "account"
	)
	derivedRelName := childEntity + "_" + fieldName

	cleanup := func() {
		sx.Exec(`DELETE FROM sys_relationships WHERE tenant_id = ? AND name = ?`, testTenant, derivedRelName)
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name IN (?, ?)`, testTenant, parentEntity, childEntity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name IN (?, ?)`, testTenant, parentEntity, childEntity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + childEntity + "`")
		sx.Exec("DROP TABLE IF EXISTS `obj_" + parentEntity + "`")
	}
	cleanup()
	t.Cleanup(cleanup)

	if w := postEntity(t, r, testTenant, map[string]any{
		"name":   parentEntity,
		"label":  "Accounts",
		"fields": []map[string]any{{"name": "name", "label": "Tên", "fieldType": "text"}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("seed parent entity: status = %d; body = %s", w.Code, w.Body.String())
	}
	if w := postEntity(t, r, testTenant, map[string]any{
		"name":   childEntity,
		"label":  "Contacts",
		"fields": []map[string]any{{"name": "name", "label": "Tên", "fieldType": "text"}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("seed child entity: status = %d; body = %s", w.Code, w.Body.String())
	}

	// Simulate a PRE-EXISTING, UNRELATED N-N relationship literally named the same as the 1-N
	// registry row AddField is about to auto-register below — e.g. an admin previously ran
	// POST /admin/relationships with this exact name for a completely different entity pair.
	// Direct INSERT (rather than driving RegisterRelationship) keeps this test focused on the
	// AddField/CreateFieldWithRelationship classification bug, not RegisterRelationship's own
	// (separately tested) correctness.
	if _, err := sx.Exec(`INSERT INTO sys_relationships
		(id, tenant_id, name, parent_entity_name, child_entity_name, relationship_type, join_table_name)
		VALUES (?, ?, ?, ?, ?, 'N-N', ?)`,
		uuid.NewString(), testTenant, derivedRelName, "it_reldup_other_a", "it_reldup_other_b",
		"rel_it_reldup_other_a_it_reldup_other_b"); err != nil {
		t.Fatalf("seed colliding N-N relationship row: %v", err)
	}

	w := addFieldReq(t, r, testTenant, map[string]any{
		"entityName": childEntity,
		"name":       fieldName, "label": "Account", "fieldType": "relation",
		"options": map[string]any{"relatedEntity": parentEntity},
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("AddField with colliding relationship name: status = %d; want 409; body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v; raw = %s", err, w.Body.String())
	}
	if resp.Code != "RELATIONSHIP_ALREADY_EXISTS" {
		t.Errorf("code = %q; want RELATIONSHIP_ALREADY_EXISTS", resp.Code)
	}

	// State must stay consistent: no orphan `<field>_id` FK column left on the child table
	// (compensation must have dropped it), and no orphan sys_fields row for the rejected field.
	var nCol int
	if err := sx.Get(&nCol, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		"obj_"+childEntity, fieldName+"_id"); err != nil {
		t.Fatalf("check column: %v", err)
	}
	if nCol != 0 {
		t.Errorf("obj_%s must NOT have %s_id column after a rejected AddField (compensation must drop it)", childEntity, fieldName)
	}
	var nField int
	if err := sx.Get(&nField, `SELECT COUNT(*) FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ?`, testTenant, childEntity, fieldName); err != nil {
		t.Fatalf("check field row: %v", err)
	}
	if nField != 0 {
		t.Errorf("sys_fields must NOT have an orphan %q row after a rejected AddField", fieldName)
	}

	// The pre-existing N-N row must be untouched by the rejected attempt.
	var relType string
	if err := sx.Get(&relType, `SELECT relationship_type FROM sys_relationships WHERE tenant_id = ? AND name = ?`,
		testTenant, derivedRelName); err != nil {
		t.Fatalf("check pre-existing relationship row: %v", err)
	}
	if relType != "N-N" {
		t.Errorf("pre-existing relationship row's type changed to %q; want unchanged N-N", relType)
	}
}
