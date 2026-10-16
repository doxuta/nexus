//go:build integration

package handlers

// specs/054-layout-visible-when through the real HTTP stack on a migrated throwaway MySQL: the
// condition is stored by POST /admin/layouts, bad configs are refused with 422
// LAYOUT_VISIBLE_WHEN_INVALID, GET serves it back, and on create a required field the condition
// hides is not demanded (the hole a later `isRequired: true` opens) while a visible one still is.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

func vwDo(t *testing.T, r *gin.Engine, tok, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestLayoutVisibleWhen_Integration(t *testing.T) {
	gdb, sx := layoutSetupDB(t)
	metaRepo := metadata.NewRepository(gdb)
	layoutRepo := metadata.NewLayoutRepository(gdb)
	dyn := dynrepo.New(sx)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:  NewAdminEntityHandler(services.NewEntityService(dyn, metaRepo), services.NewFieldService(dyn, metaRepo), zap.NewNop()),
		Layout: NewAdminLayoutHandler(services.NewLayoutService(metaRepo, layoutRepo, zap.NewNop()), zap.NewNop()),
		Record: NewDynamicRecordHandler(services.NewRecordService(metaRepo, dyn).WithLayoutReader(layoutRepo), zap.NewNop()),
	}, zap.NewNop())

	const tenant = "c5300000-0000-0000-0000-000000000053"
	const entity = "it_vw_leads"
	admin := mintToken(t, tenant, true)
	staff := mintToken(t, tenant, false) // non-admin: the person who fills the form

	layoutCreateEntity(t, r, admin, entity, []map[string]any{
		{"name": "full_name", "label": "Họ tên", "fieldType": "text", "isRequired": true},
		{"name": "customer_type", "label": "Loại khách", "fieldType": "picklist",
			"options": map[string]any{"values": []any{"individual", "business"}}},
		{"name": "tax_code", "label": "Mã số thuế", "fieldType": "text"},
	})

	editLayout := func(taxCell map[string]any) map[string]any {
		return map[string]any{"entityName": entity, "viewType": "edit", "layoutJson": map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"columns": 2, "fields": []any{"full_name", "customer_type", taxCell}}},
		}}
	}
	cond := map[string]any{"field": "customer_type", "op": "eq", "value": "business"}

	// US1: a valid condition saves and is served back verbatim.
	if w := layoutDoPOST(t, r, admin, editLayout(map[string]any{"field": "tax_code", "visibleWhen": cond})); w.Code != http.StatusOK {
		t.Fatalf("save layout with visibleWhen: %d %s; want 200", w.Code, w.Body.String())
	}
	served := func(tok string) map[string]any {
		t.Helper()
		w := layoutDoGET(t, r, tok, entity, "edit")
		if w.Code != http.StatusOK {
			t.Fatalf("GET layout: %d %s", w.Code, w.Body.String())
		}
		data := parseBody(t, w)["data"].(map[string]any)
		secs := data["views"].(map[string]any)["edit"].(map[string]any)["sections"].([]any)
		for _, c := range secs[0].(map[string]any)["fields"].([]any) {
			if m, ok := c.(map[string]any); ok && m["field"] == "tax_code" {
				return m
			}
		}
		t.Fatalf("GET layout: no tax_code cell in %s", w.Body.String())
		return nil
	}
	if got, _ := json.Marshal(served(staff)["visibleWhen"]); string(got) != `{"field":"customer_type","op":"eq","value":"business"}` {
		t.Fatalf("served visibleWhen = %s; want the stored condition", got)
	}

	// US1.2/US1.3: required target, self-reference, typo'd picklist value → 422, nothing saved.
	for name, body := range map[string]map[string]any{
		"on a required field": {"entityName": entity, "viewType": "edit", "layoutJson": map[string]any{
			"version": 1, "sections": []any{map[string]any{"columns": 1, "fields": []any{
				map[string]any{"field": "full_name", "visibleWhen": cond}, "customer_type", "tax_code"}}},
		}},
		"cell required:true": editLayout(map[string]any{"field": "tax_code", "required": true, "visibleWhen": cond}),
		"points at itself": editLayout(map[string]any{"field": "tax_code",
			"visibleWhen": map[string]any{"field": "tax_code", "op": "notEmpty"}}),
		"typo'd picklist value": editLayout(map[string]any{"field": "tax_code",
			"visibleWhen": map[string]any{"field": "customer_type", "op": "eq", "value": "Business"}}),
	} {
		w := layoutDoPOST(t, r, admin, body)
		if w.Code != http.StatusUnprocessableEntity || errorCode(parseBody(t, w)) != "LAYOUT_VISIBLE_WHEN_INVALID" {
			t.Fatalf("%s: %d %s; want 422 LAYOUT_VISIBLE_WHEN_INVALID", name, w.Code, w.Body.String())
		}
	}
	if _, ok := served(admin)["visibleWhen"]; !ok {
		t.Fatal("a refused save overwrote the stored condition")
	}

	// The hole US2 closes: tax_code made required AFTER the condition was saved.
	var taxID string
	if err := sx.Get(&taxID, `SELECT f.id FROM sys_fields f JOIN sys_entities e ON e.id = f.entity_id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = 'tax_code'`, tenant, entity); err != nil {
		t.Fatalf("tax_code id: %v", err)
	}
	if w := vwDo(t, r, admin, http.MethodPut, "/api/v1/admin/fields/"+taxID, map[string]any{"isRequired": true}); w.Code != http.StatusOK {
		t.Fatalf("make tax_code required: %d %s", w.Code, w.Body.String())
	}

	// US2.1: the form hides tax_code for an individual and does not send it → 201.
	if w := vwDo(t, r, staff, http.MethodPost, "/api/v1/"+entity, map[string]any{"full_name": "An", "customer_type": "individual"}); w.Code != http.StatusCreated {
		t.Fatalf("individual without tax_code: %d %s; want 201", w.Code, w.Body.String())
	}
	// US2.2: visible for a business → still required.
	w := vwDo(t, r, staff, http.MethodPost, "/api/v1/"+entity, map[string]any{"full_name": "Bình", "customer_type": "business"})
	if w.Code != http.StatusUnprocessableEntity || bodyCode(t, w) != "RECORD_REQUIRED_MISSING" || bodyDetails(t, w)["field"] != "tax_code" {
		t.Fatalf("business without tax_code: %d %s; want 422 RECORD_REQUIRED_MISSING details.field=tax_code", w.Code, w.Body.String())
	}
	if w := vwDo(t, r, staff, http.MethodPost, "/api/v1/"+entity, map[string]any{"full_name": "Bình", "customer_type": "business", "tax_code": "0101234567"}); w.Code != http.StatusCreated {
		t.Fatalf("business with tax_code: %d %s; want 201", w.Code, w.Body.String())
	}
	// An unconditional required field keeps its rule.
	if w := vwDo(t, r, staff, http.MethodPost, "/api/v1/"+entity, map[string]any{"customer_type": "individual"}); w.Code != http.StatusUnprocessableEntity || bodyDetails(t, w)["field"] != "full_name" {
		t.Fatalf("missing full_name: %d %s; want 422 details.field=full_name", w.Code, w.Body.String())
	}

	// US2.3: bulk, per row.
	w = vwDo(t, r, staff, http.MethodPost, "/api/v1/"+entity+"/bulk?mode=partial", map[string]any{"records": []any{
		map[string]any{"full_name": "Chi", "customer_type": "individual"},
		map[string]any{"full_name": "Dũng", "customer_type": "business"},
	}})
	var bulk struct {
		Data services.BulkCreateResult `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &bulk); err != nil || w.Code != http.StatusOK || len(bulk.Data.Results) != 2 ||
		bulk.Data.Results[0].Status != "created" || bulk.Data.Results[1].Code != "RECORD_REQUIRED_MISSING" {
		t.Fatalf("bulk: %d %s; want row 0 created, row 1 RECORD_REQUIRED_MISSING", w.Code, w.Body.String())
	}

	var stored int
	if err := sx.Get(&stored, "SELECT COUNT(*) FROM `obj_"+entity+"` WHERE `tenant_id` = ?", tenant); err != nil || stored != 3 {
		t.Fatalf("stored rows = %d (err=%v); want 3 (An, Bình, Chi)", stored, err)
	}

	// FR-9 + FR-8: the controller is deleted → GET serves tax_code without its condition, and
	// tax_code is unconditional again (required for everyone).
	softDeleteField(t, r, admin, tenant, entity, "customer_type", sx)
	if _, ok := served(staff)["visibleWhen"]; ok {
		t.Fatal("GET still serves a condition on a deleted controller")
	}
	w = vwDo(t, r, staff, http.MethodPost, "/api/v1/"+entity, map[string]any{"full_name": "Em"})
	if w.Code != http.StatusUnprocessableEntity || bodyDetails(t, w)["field"] != "tax_code" {
		t.Fatalf("after deleting the controller: %d %s; want 422 details.field=tax_code", w.Code, w.Body.String())
	}
}
