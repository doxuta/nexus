//go:build integration

// W2 relationship lifecycle HTTP-contract integration tests:
//
//	GET    /admin/relationships/:id/impact
//	PATCH  /admin/relationships/:id
//	DELETE /admin/relationships/:id
//
// The routes are mounted through RegisterAPIRoutes on the SAME admin-gated group as
// POST /admin/relationships (Auth → Tenant → RequireAdmin), and the tenant always comes
// from the verified Bearer claim (setAuth) — the cross-tenant test proves a tenant-B token
// cannot address tenant-A's relationship id. Deep DB-state assertions (tombstones, grace
// window, dependents gate) live in services/relation_lifecycle_integration_test.go; this
// file pins the HTTP contract the FE canvas consumes.
//
// Run with:
//
//	go test -tags=integration ./internal/handlers/
package handlers

import (
	"bytes"
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
	relLCParent      = "it_rellc_customers"
	relLCChild       = "it_rellc_orders"
	relLCOtherTenant = "88888888-8888-8888-8888-888888888888"
)

// newRelLifecycleRouter wires the admin entity/field routes + the full AdminRelationHandler
// (registration + lifecycle) + the metadata read routes — the production composition of the
// W2 canvas surface.
func newRelLifecycleRouter(t *testing.T) (*gin.Engine, *sqlx.DB) {
	t.Helper()
	sx, err := sqlx.Connect("mysql", dsn())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	gdb, err := gorm.Open(gormmysql.Open(dsn()), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sx)
	entitySvc := services.NewEntityService(dynRepo, metaRepo)
	fieldSvc := services.NewFieldService(dynRepo, metaRepo)
	relationSvc := services.NewRelationService(dynRepo, metaRepo)
	lifecycleSvc := services.NewRelationLifecycleService(metaRepo, dynRepo, fieldSvc)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:         NewAdminEntityHandler(entitySvc, fieldSvc, zap.NewNop()),
		AdminRelation: NewAdminRelationHandler(relationSvc, lifecycleSvc, zap.NewNop()),
		Metadata:      NewMetadataHandler(gdb, zap.NewNop()),
	}, zap.NewNop())
	return r, sx
}

func relLCCleanup(sx *sqlx.DB) func() {
	return func() {
		for _, tenant := range []string{testTenant, relLCOtherTenant} {
			sx.Exec(`DELETE FROM sys_relationships WHERE tenant_id = ? AND (parent_entity_name LIKE 'it_rellc_%' OR child_entity_name LIKE 'it_rellc_%')`, tenant)
			sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
			         WHERE e.tenant_id = ? AND e.name LIKE 'it_rellc_%'`, tenant)
			sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name LIKE 'it_rellc_%'`, tenant)
		}
		sx.Exec("DROP TABLE IF EXISTS `obj_" + relLCChild + "`")
		sx.Exec("DROP TABLE IF EXISTS `obj_" + relLCParent + "`")
	}
}

// relLCSetupOneToN seeds parent+child entities, the relation field child.customer →
// parent, and n child rows holding the FK; returns the sys_relationships row id (read
// through GET /metadata/relationships — the same discovery path the FE canvas uses).
func relLCSetupOneToN(t *testing.T, r *gin.Engine, sx *sqlx.DB, linkedRows int) string {
	t.Helper()
	if w := postEntity(t, r, testTenant, map[string]any{
		"name": relLCParent, "label": "Customers",
		"fields": []map[string]any{{"name": "name", "label": "Tên", "fieldType": "text"}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("seed parent: %d %s", w.Code, w.Body.String())
	}
	if w := postEntity(t, r, testTenant, map[string]any{
		"name": relLCChild, "label": "Orders",
		"fields": []map[string]any{{"name": "total", "label": "Total", "fieldType": "number"}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("seed child: %d %s", w.Code, w.Body.String())
	}
	if w := addFieldReq(t, r, testTenant, map[string]any{
		"entityName": relLCChild,
		"name":       "customer", "label": "Customer", "fieldType": "relation",
		"options": map[string]any{"relatedEntity": relLCParent},
	}); w.Code != http.StatusCreated {
		t.Fatalf("AddField(relation): %d %s", w.Code, w.Body.String())
	}

	// Populate child rows holding the FK (direct SQL — the record surface is not part of
	// this contract test; only the COUNT matters). `customer_id` is a STORED generated
	// column over data->'$.customer', so the FK travels inside the data JSON.
	parentID := uuid.NewString()
	if _, err := sx.Exec("INSERT INTO `obj_"+relLCParent+"` (`id`,`tenant_id`,`created_by`) VALUES (?,?,?)",
		parentID, testTenant, uuid.NewString()); err != nil {
		t.Fatalf("seed parent row: %v", err)
	}
	for i := 0; i < linkedRows; i++ {
		if _, err := sx.Exec("INSERT INTO `obj_"+relLCChild+"` (`id`,`tenant_id`,`created_by`,`data`) VALUES (?,?,?,JSON_OBJECT('customer', ?))",
			uuid.NewString(), testTenant, uuid.NewString(), parentID); err != nil {
			t.Fatalf("seed child row %d: %v", i, err)
		}
	}

	// Discover the registry row id the way the canvas does.
	w := getRelationships(t, r, testTenant, relLCParent)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metadata/relationships: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode relationships: %v", err)
	}
	for _, row := range resp.Data {
		if row.Name == relLCChild+"_customer" {
			return row.ID
		}
	}
	t.Fatalf("registry row %q not found in %s", relLCChild+"_customer", w.Body.String())
	return ""
}

func relLCDo(t *testing.T, r *gin.Engine, method, url, tenant string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		buf, _ := json.Marshal(body)
		req = httptest.NewRequest(method, url, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, url, nil)
	}
	setAuth(t, req, tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRelLifecycleHTTP_ImpactDeleteRedelete_OneToN(t *testing.T) {
	r, sx := newRelLifecycleRouter(t)
	cleanup := relLCCleanup(sx)
	cleanup()
	t.Cleanup(cleanup)

	relID := relLCSetupOneToN(t, r, sx, 2)
	base := "/api/v1/admin/relationships/" + relID

	// Impact BEFORE the destructive confirm: count + deletable verdict.
	w := relLCDo(t, r, http.MethodGet, base+"/impact", testTenant, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET impact: %d %s", w.Code, w.Body.String())
	}
	var impResp struct {
		Data struct {
			AffectedRecords  int `json:"affectedRecords"`
			DependentRollups []struct {
				EntityName string `json:"entityName"`
				FieldName  string `json:"fieldName"`
			} `json:"dependentRollups"`
			Deletable    bool `json:"deletable"`
			Relationship struct {
				ID               string `json:"id"`
				RelationshipType string `json:"relationshipType"`
			} `json:"relationship"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &impResp); err != nil {
		t.Fatalf("decode impact: %v; raw = %s", err, w.Body.String())
	}
	if impResp.Data.AffectedRecords != 2 {
		t.Errorf("affectedRecords = %d; want 2", impResp.Data.AffectedRecords)
	}
	if !impResp.Data.Deletable || len(impResp.Data.DependentRollups) != 0 {
		t.Errorf("deletable/dependentRollups = %v/%v; want true/empty",
			impResp.Data.Deletable, impResp.Data.DependentRollups)
	}
	if impResp.Data.Relationship.ID != relID || impResp.Data.Relationship.RelationshipType != "1-N" {
		t.Errorf("relationship echo = %+v; want id=%s type=1-N", impResp.Data.Relationship, relID)
	}

	// Cross-tenant: a tenant-B admin token cannot delete tenant A's relationship — 404, and
	// the row must remain fully addressable by tenant A afterwards.
	if w := relLCDo(t, r, http.MethodDelete, base, relLCOtherTenant, nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant DELETE: %d; want 404; body = %s", w.Code, w.Body.String())
	}
	if w := relLCDo(t, r, http.MethodGet, base+"/impact", testTenant, nil); w.Code != http.StatusOK {
		t.Fatalf("tenant A's relationship must survive cross-tenant delete: %d %s", w.Code, w.Body.String())
	}

	// The real delete: 204, no body.
	if w := relLCDo(t, r, http.MethodDelete, base, testTenant, nil); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d; want 204; body = %s", w.Code, w.Body.String())
	}

	// The registry row disappears from the canvas discovery endpoint.
	w = getRelationships(t, r, testTenant, relLCParent)
	if bytes.Contains(w.Body.Bytes(), []byte(relLCChild+"_customer")) {
		t.Errorf("deleted relationship still listed by GET /metadata/relationships: %s", w.Body.String())
	}

	// The owning relation field is soft-deleted; the child FK VALUES survive (documented).
	var fieldDeleted, fkKept int
	if err := sx.Get(&fieldDeleted, `SELECT COUNT(*) FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = 'customer' AND f.deleted_at IS NOT NULL`,
		testTenant, relLCChild); err != nil {
		t.Fatalf("check field soft-delete: %v", err)
	}
	if fieldDeleted != 1 {
		t.Errorf("relation field must be soft-deleted (mirror of DELETE /admin/fields/:id)")
	}
	if err := sx.Get(&fkKept, "SELECT COUNT(*) FROM `obj_"+relLCChild+"` WHERE `tenant_id` = ? AND `customer_id` IS NOT NULL",
		testTenant); err != nil {
		t.Fatalf("check FK values: %v", err)
	}
	if fkKept != 2 {
		t.Errorf("child FK values kept = %d; want 2 (no DDL, data preserved)", fkKept)
	}

	// Re-delete → 404 RELATIONSHIP_NOT_FOUND (idempotency contract: gone is gone).
	w = relLCDo(t, r, http.MethodDelete, base, testTenant, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("re-DELETE: %d; want 404; body = %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode re-delete error: %v", err)
	}
	if errResp.Code != "RELATIONSHIP_NOT_FOUND" {
		t.Errorf("re-delete code = %q; want RELATIONSHIP_NOT_FOUND", errResp.Code)
	}
}

func TestRelLifecycleHTTP_PatchImmutableContract(t *testing.T) {
	r, sx := newRelLifecycleRouter(t)
	cleanup := relLCCleanup(sx)
	cleanup()
	t.Cleanup(cleanup)

	relID := relLCSetupOneToN(t, r, sx, 0)
	base := "/api/v1/admin/relationships/" + relID

	// Changing inverseField is refused with the canonical immutability code.
	w := relLCDo(t, r, http.MethodPatch, base, testTenant, map[string]any{"inverseField": "renamed"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH inverseField: %d; want 422; body = %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Code != "FIELD_RELATION_OPTIONS_IMMUTABLE" {
		t.Errorf("code = %q; want FIELD_RELATION_OPTIONS_IMMUTABLE", errResp.Code)
	}
	if errResp.Details["field"] != "inverseField" {
		t.Errorf("details.field = %v; want inverseField", errResp.Details["field"])
	}

	// Changing cardinality (relationshipType) is refused the same way.
	w = relLCDo(t, r, http.MethodPatch, base, testTenant, map[string]any{"relationshipType": "N-N"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH relationshipType: %d; want 422; body = %s", w.Code, w.Body.String())
	}

	// Echoing the stored values back is a no-op 200 that returns the row.
	w = relLCDo(t, r, http.MethodPatch, base, testTenant, map[string]any{
		"relationshipType": "1-N",
		"inverseField":     relLCChild, // default inverse = child entity name
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH echo-equal: %d; want 200; body = %s", w.Code, w.Body.String())
	}
	var okResp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &okResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if okResp.Data.ID != relID {
		t.Errorf("PATCH echo id = %q; want %q", okResp.Data.ID, relID)
	}

	// Unknown id → 404 with the canonical code.
	w = relLCDo(t, r, http.MethodPatch, "/api/v1/admin/relationships/"+uuid.NewString(), testTenant, map[string]any{})
	if w.Code != http.StatusNotFound {
		t.Errorf("PATCH unknown id: %d; want 404; body = %s", w.Code, w.Body.String())
	}
}
