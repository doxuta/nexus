//go:build integration

// M3-T3 — end-to-end acceptance for the 1-N relation engine (spec 9.1.3). Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// Scenario (all over the real admin + dynamic-record HTTP handlers against MySQL 8):
//  1. Create a PARENT entity (`it_accounts`) and a CHILD entity (`it_tickets`) that carries
//     >=5 field types (text/number/boolean/date/email/picklist) PLUS a 1-N relation field
//     `account` -> it_accounts.
//  2. Insert a parent record; link a child record to it via the relation field -> 201.
//  3. A child whose relation value points at a non-existent parent id -> 422
//     RECORD_RELATION_INVALID (FK validation, M3-T2).
//  4. Cross-tenant isolation: a parent id that exists under tenant A is NOT a valid FK target
//     for a child created under tenant B (which has its own metadata for the same tables) ->
//     422 — the tenant-scoped FK probe never sees another tenant's parent.
//  5. The linked child is queryable by its FK via the dynamic ?field=value filter on the
//     relation field, and the stored value round-trips through GET.
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

const (
	relParentEntity = "it_accounts"
	relChildEntity  = "it_tickets"
	tenantOther     = "88888888-8888-8888-8888-888888888888"
)

func TestM3_RelationEndToEnd(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanAll := func() {
		cleanupTenantNamed(sx, testTenant, relChildEntity)
		cleanupTenantNamed(sx, testTenant, relParentEntity)
		cleanupTenantNamed(sx, tenantOther, relChildEntity)
		cleanupTenantNamed(sx, tenantOther, relParentEntity)
		sx.Exec("DROP TABLE IF EXISTS `obj_" + relChildEntity + "`")
		sx.Exec("DROP TABLE IF EXISTS `obj_" + relParentEntity + "`")
	}
	cleanAll()
	t.Cleanup(cleanAll)

	parentBody := func() map[string]any {
		return map[string]any{
			"name":  relParentEntity,
			"label": "Khách hàng",
			"fields": []map[string]any{
				{"name": "name", "label": "Tên", "fieldType": "text", "isRequired": true},
			},
		}
	}
	childBody := func() map[string]any {
		return map[string]any{
			"name":  relChildEntity,
			"label": "Ticket",
			"fields": []map[string]any{
				{"name": "subject", "label": "Tiêu đề", "fieldType": "text", "isRequired": true},
				{"name": "priority", "label": "Ưu tiên", "fieldType": "number"},
				{"name": "is_open", "label": "Đang mở", "fieldType": "boolean"},
				{"name": "due_date", "label": "Hạn", "fieldType": "date"},
				{"name": "contact", "label": "Email", "fieldType": "email"},
				{"name": "status", "label": "Trạng thái", "fieldType": "picklist",
					"options": map[string]any{"values": []any{"new", "done"}}},
				{"name": "account", "label": "Khách", "fieldType": "relation",
					"options": map[string]any{"relatedEntity": relParentEntity}},
			},
		}
	}

	// 1) Parent + child entities under tenant A (testTenant).
	if w := doWithHeader(t, r, testTenant, http.MethodPost, "/api/v1/admin/entities", parentBody()); w.Code != http.StatusCreated {
		t.Fatalf("create parent entity (A): %d %s", w.Code, w.Body.String())
	}
	if w := doWithHeader(t, r, testTenant, http.MethodPost, "/api/v1/admin/entities", childBody()); w.Code != http.StatusCreated {
		t.Fatalf("create child entity (A): %d %s", w.Code, w.Body.String())
	}

	// The FK column + index must exist on the child table.
	var nCol int
	if err := sx.Get(&nCol, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		"obj_"+relChildEntity, "account_id"); err != nil {
		t.Fatalf("check FK column: %v", err)
	}
	if nCol != 1 {
		t.Fatalf("child must have account_id FK column")
	}

	// 2) Insert a parent record under tenant A.
	parentID := createRecord(t, r, testTenant, relParentEntity, map[string]any{"name": "Acme"})

	// 3) Link a child to the live parent -> 201, and the relation value round-trips on GET.
	childID := createRecord(t, r, testTenant, relChildEntity, map[string]any{
		"subject": "Cannot login", "priority": 2, "is_open": true,
		"due_date": "2026-07-01", "contact": "a@b.com", "status": "new",
		"account": parentID,
	})
	g := doWithHeader(t, r, testTenant, http.MethodGet, "/api/v1/"+relChildEntity+"/"+childID, nil)
	if g.Code != http.StatusOK {
		t.Fatalf("get child: %d %s", g.Code, g.Body.String())
	}
	var gotChild struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	json.Unmarshal(g.Body.Bytes(), &gotChild)
	if got := gotChild.Data.Data["account"]; got != parentID {
		t.Errorf("child.account = %v; want parent id %q", got, parentID)
	}

	// 4) A child pointing at a non-existent parent -> 422 RECORD_RELATION_INVALID.
	bad := doWithHeader(t, r, testTenant, http.MethodPost, "/api/v1/"+relChildEntity, map[string]any{
		"subject": "Ghost link", "account": "deadbeef-0000-0000-0000-000000000000",
	})
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("ghost FK: %d %s; want 422", bad.Code, bad.Body.String())
	}
	if code := errCode(bad); code != "RECORD_RELATION_INVALID" {
		t.Errorf("ghost FK: code = %q; want RECORD_RELATION_INVALID", code)
	}

	// 5) Cross-tenant: tenant B registers its OWN metadata for the same parent+child tables
	//    (the obj_ tables already exist — CREATE IF NOT EXISTS), then tries to link a child to
	//    tenant A's parentID. The tenant-scoped FK probe must not see A's parent -> 422.
	if w := doWithHeader(t, r, tenantOther, http.MethodPost, "/api/v1/admin/entities", parentBody()); w.Code != http.StatusCreated {
		t.Fatalf("create parent entity (B): %d %s", w.Code, w.Body.String())
	}
	if w := doWithHeader(t, r, tenantOther, http.MethodPost, "/api/v1/admin/entities", childBody()); w.Code != http.StatusCreated {
		t.Fatalf("create child entity (B): %d %s", w.Code, w.Body.String())
	}
	cross := doWithHeader(t, r, tenantOther, http.MethodPost, "/api/v1/"+relChildEntity, map[string]any{
		"subject": "Cross tenant", "account": parentID,
	})
	if cross.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cross-tenant FK: %d %s; want 422", cross.Code, cross.Body.String())
	}
	if code := errCode(cross); code != "RECORD_RELATION_INVALID" {
		t.Errorf("cross-tenant FK: code = %q; want RECORD_RELATION_INVALID", code)
	}

	// 6) Query the child by its FK via the dynamic ?field=value filter on the relation field.
	list := doWithHeader(t, r, testTenant, http.MethodGet,
		"/api/v1/"+relChildEntity+"?account="+parentID, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("filter by FK: %d %s", list.Code, list.Body.String())
	}
	var listResp struct {
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	json.Unmarshal(list.Body.Bytes(), &listResp)
	if listResp.Pagination.Total != 1 {
		t.Errorf("filter by FK total = %d; want 1", listResp.Pagination.Total)
	}

	// 7) Sort the child list by the relation field THROUGH THE REAL STORE (DAILY-QUEUE #4).
	//    buildSortAllowlist admits "account" → "account_id", but the service used to hand the
	//    offset store the translated column while dynrepo's normalize() looks Sort up by the wire
	//    name — so a sort the layout now advertises as sortable:true answered 500
	//    RECORD_OP_FAILED. The fake-store unit tests pin the contract; this is the wire-level proof.
	for _, order := range []string{"asc", "desc"} {
		sorted := doWithHeader(t, r, testTenant, http.MethodGet,
			"/api/v1/"+relChildEntity+"?sort=account&order="+order, nil)
		if sorted.Code != http.StatusOK {
			t.Errorf("sort by relation field (order=%s): %d %s; want 200", order, sorted.Code, sorted.Body.String())
		}
	}
}

// --- helpers (relation E2E) --------------------------------------------------------------

// doWithHeader issues a request authenticated as `tenant` (the package `do` hardcodes
// testTenant). The tenant is carried in a verified Bearer access token minted by setAuth — never
// an X-Tenant-Id header — so the handler derives it from the claim, exactly as in production. An
// empty tenant sends no token (the unauthenticated case). Body is JSON-encoded when non-nil.
func doWithHeader(t *testing.T, r *gin.Engine, tenant, method, path string, body any) *httptest.ResponseRecorder {
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
	setAuth(t, req, tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// createRecord posts a record and returns its id, failing the test on a non-201.
func createRecord(t *testing.T, r *gin.Engine, tenant, entity string, body map[string]any) string {
	t.Helper()
	w := doWithHeader(t, r, tenant, http.MethodPost, "/api/v1/"+entity, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create %s record: %d %s", entity, w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Data.ID == "" {
		t.Fatalf("create %s record: empty id", entity)
	}
	return created.Data.ID
}

// errCode pulls the `code` field out of an error response body.
func errCode(w *httptest.ResponseRecorder) string {
	var resp struct {
		Code string `json:"code"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Code
}

// cleanupTenantNamed removes an entity's metadata for an arbitrary tenant (the shared obj_
// table is dropped separately by the caller).
//
// Finding B: also removes any sys_relationships row involving this (tenant, entity) as either
// side. TestM3_RelationEndToEnd's `account` relation field auto-registers a companion 1-N
// registry row (ENG-REL2 #4); without this delete, a second local run re-creates the parent+
// child entities, AddField/CreateEntityWithFields re-derives the SAME registry name, and trips
// the (tenant_id, name) unique index against the row the PRIOR run left behind — breaking
// idempotent re-runs without a manual DB reset.
func cleanupTenantNamed(db *sqlx.DB, tenant, name string) {
	db.Exec(`DELETE FROM sys_relationships WHERE tenant_id = ? AND (parent_entity_name = ? OR child_entity_name = ?)`,
		tenant, name, name)
	db.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
	         WHERE e.tenant_id = ? AND e.name = ?`, tenant, name)
	db.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, tenant, name)
}
