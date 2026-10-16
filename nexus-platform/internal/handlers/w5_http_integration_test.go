//go:build integration

// W5 HTTP-contract integration tests — the wire-level shapes the W5 FE (Data Model builder +
// Layout WYSIWYG + live formula editor) consumes, through the REAL router chain
// (AuthMiddleware → RequireTenant → RequireAdmin), against the shared MySQL from dsn():
//
//   - GET  /metadata/fields/:entity           → sorted by fieldOrder (tie-break name),
//     carries fieldOrder + description.
//   - PUT  /admin/fields/reorder              → 200 {data:{entityName,updated}} / 422 codes;
//     static segment coexists with PUT /admin/fields/:id (gin route-conflict de-risk).
//   - POST /admin/formula/validate            → 200 {data:{valid,fields,resultType}} / 422
//     with the SAME FIELD_FORMULA_* codes as POST /admin/fields.
//   - POST /admin/layouts with cell helpText  → 201; oversized helpText → 422 LAYOUT_INVALID.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=True&loc=Local" \
//	  go test -tags=integration ./internal/handlers/ -run TestW5HTTP -race -v
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const w5Entity = "it_w5_builder"

// newW5Router mounts Admin + Metadata + Layout handlers behind the real chain — the three
// surfaces this wave touches.
func newW5Router(t *testing.T) (*gin.Engine, *sqlx.DB) {
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
	dyn := dynrepo.New(sx)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:    NewAdminEntityHandler(services.NewEntityService(dyn, metaRepo), services.NewFieldService(dyn, metaRepo), zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
		Layout:   NewAdminLayoutHandler(services.NewLayoutService(metaRepo, metadata.NewLayoutRepository(gdb), zap.NewNop()), zap.NewNop()),
	}, zap.NewNop())
	return r, sx
}

func w5Cleanup(sx *sqlx.DB) {
	sx.Exec(`DELETE FROM sys_layouts WHERE tenant_id = ? AND entity_name = ?`, testTenant, w5Entity)
	cleanupNamed(sx, w5Entity)
}

type w5FieldDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	FieldOrder  int    `json:"fieldOrder"`
	Description string `json:"description"`
}

func w5ListFields(t *testing.T, r *gin.Engine) []w5FieldDTO {
	t.Helper()
	w := do(t, r, http.MethodGet, "/api/v1/metadata/fields/"+w5Entity, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list fields: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []w5FieldDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	return resp.Data
}

func TestW5HTTP_FieldOrderReorderAndFormulaValidate(t *testing.T) {
	r, sx := newW5Router(t)
	w5Cleanup(sx)
	t.Cleanup(func() { w5Cleanup(sx) })

	// Entity with fields deliberately NOT in alphabetical order + an explicit description.
	create := map[string]any{
		"name": w5Entity, "label": "W5 Builder",
		"fields": []map[string]any{
			{"name": "zeta", "label": "Zeta", "fieldType": "text", "description": "mô tả zeta"},
			{"name": "price", "label": "Price", "fieldType": "number"},
			{"name": "qty", "label": "Qty", "fieldType": "number"},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", create); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}

	// E2 contract: declaration order (10/20/30), fieldOrder + description on the wire.
	fields := w5ListFields(t, r)
	if len(fields) != 3 || fields[0].Name != "zeta" || fields[1].Name != "price" || fields[2].Name != "qty" {
		t.Fatalf("serve order = %+v; want zeta,price,qty (declaration order, not alphabetical)", fields)
	}
	if fields[0].FieldOrder != 10 || fields[0].Description != "mô tả zeta" {
		t.Errorf("zeta = order %d desc %q; want 10 / mô tả zeta", fields[0].FieldOrder, fields[0].Description)
	}

	// Reorder (reversed) → 200 {entityName, updated:3}; list serves the new order.
	reorder := map[string]any{
		"entityName": w5Entity,
		"order":      []string{fields[2].ID, fields[1].ID, fields[0].ID},
	}
	w := do(t, r, http.MethodPut, "/api/v1/admin/fields/reorder", reorder)
	if w.Code != http.StatusOK {
		t.Fatalf("reorder: %d %s", w.Code, w.Body.String())
	}
	var ro struct {
		Data struct {
			EntityName string `json:"entityName"`
			Updated    int    `json:"updated"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &ro)
	if ro.Data.EntityName != w5Entity || ro.Data.Updated != 3 {
		t.Errorf("reorder envelope = %+v; want {%s 3}", ro.Data, w5Entity)
	}
	after := w5ListFields(t, r)
	if after[0].Name != "qty" || after[1].Name != "price" || after[2].Name != "zeta" {
		t.Errorf("post-reorder order = %+v; want qty,price,zeta", after)
	}

	// Idempotent re-submit of the SAME order (value-identical updates) must still be 200 —
	// pins the RowsAffected-on-no-op MySQL gotcha the repo guards against.
	if w := do(t, r, http.MethodPut, "/api/v1/admin/fields/reorder", reorder); w.Code != http.StatusOK {
		t.Errorf("re-submit same order: %d %s; want 200 (no-op update is not a missing row)", w.Code, w.Body.String())
	}

	// Forged id → 422 FIELD_REORDER_UNKNOWN_FIELD; empty list → 422 FIELD_REORDER_INVALID;
	// unknown entity → 404.
	bad := map[string]any{"entityName": w5Entity, "order": []string{after[0].ID, "00000000-0000-0000-0000-00000000beef"}}
	if w := do(t, r, http.MethodPut, "/api/v1/admin/fields/reorder", bad); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("forged id: %d; want 422", w.Code)
	} else if code := w5ErrCode(w.Body.Bytes()); code != "FIELD_REORDER_UNKNOWN_FIELD" {
		t.Errorf("forged id code = %q; want FIELD_REORDER_UNKNOWN_FIELD", code)
	}
	if w := do(t, r, http.MethodPut, "/api/v1/admin/fields/reorder", map[string]any{"entityName": w5Entity, "order": []string{}}); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty order: %d; want 422", w.Code)
	}
	if w := do(t, r, http.MethodPut, "/api/v1/admin/fields/reorder", map[string]any{"entityName": "it_w5_ghost", "order": []string{after[0].ID}}); w.Code != http.StatusNotFound {
		t.Errorf("unknown entity: %d; want 404", w.Code)
	}

	// Cross-tenant: another tenant reordering this entity name → 404 (entity not theirs).
	if w := doAs(t, r, http.MethodPut, "/api/v1/admin/fields/reorder", w5OtherTenantID, reorder); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant reorder: %d; want 404", w.Code)
	}

	// PUT /admin/fields/:id still routes (static /reorder must not shadow the param route):
	// edit qty's fieldOrder + description through the ordinary edit endpoint.
	editBody := map[string]any{"fieldOrder": 5, "description": "số lượng"}
	if w := do(t, r, http.MethodPut, "/api/v1/admin/fields/"+after[0].ID, editBody); w.Code != http.StatusOK {
		t.Fatalf("edit field order/description: %d %s", w.Code, w.Body.String())
	}
	final := w5ListFields(t, r)
	if final[0].Name != "qty" || final[0].FieldOrder != 5 || final[0].Description != "số lượng" {
		t.Errorf("edited qty = %+v; want order 5 + description", final[0])
	}

	// Formula validate: happy → 200 {valid,fields,resultType}; same-code 422s; 404 entity.
	w = do(t, r, http.MethodPost, "/api/v1/admin/formula/validate",
		map[string]any{"entityName": w5Entity, "formula": "price * qty"})
	if w.Code != http.StatusOK {
		t.Fatalf("formula validate: %d %s", w.Code, w.Body.String())
	}
	var fv struct {
		Data struct {
			Valid      bool     `json:"valid"`
			Fields     []string `json:"fields"`
			ResultType string   `json:"resultType"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &fv)
	if !fv.Data.Valid || len(fv.Data.Fields) != 2 || fv.Data.ResultType != "number" {
		t.Errorf("formula validate envelope = %+v; want valid, [price qty], number", fv.Data)
	}
	w = do(t, r, http.MethodPost, "/api/v1/admin/formula/validate",
		map[string]any{"entityName": w5Entity, "formula": "price * missing"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown ref: %d; want 422", w.Code)
	} else if code := w5ErrCode(w.Body.Bytes()); code != "FIELD_FORMULA_UNKNOWN_REF" {
		t.Errorf("unknown ref code = %q; want FIELD_FORMULA_UNKNOWN_REF", code)
	}
	w = do(t, r, http.MethodPost, "/api/v1/admin/formula/validate",
		map[string]any{"entityName": w5Entity, "formula": "zeta + 1"})
	if code := w5ErrCode(w.Body.Bytes()); w.Code != http.StatusUnprocessableEntity || code != "FIELD_FORMULA_NON_NUMERIC_REF" {
		t.Errorf("non-numeric ref: %d %q; want 422 FIELD_FORMULA_NON_NUMERIC_REF", w.Code, code)
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/formula/validate",
		map[string]any{"entityName": "it_w5_ghost", "formula": "1+1"}); w.Code != http.StatusNotFound {
		t.Errorf("unknown entity formula: %d; want 404", w.Code)
	}
}

func TestW5HTTP_LayoutCellHelpText(t *testing.T) {
	r, sx := newW5Router(t)
	w5Cleanup(sx)
	t.Cleanup(func() { w5Cleanup(sx) })

	create := map[string]any{
		"name": w5Entity, "label": "W5 Builder",
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "fieldType": "text"},
			{"name": "level", "label": "Level", "fieldType": "number"},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", create); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}

	layout := func(helpText any) map[string]any {
		return map[string]any{
			"entityName": w5Entity, "viewType": "edit",
			"layoutJson": map[string]any{
				"version": 1,
				"sections": []any{map[string]any{
					"columns": 4,
					"fields": []any{
						map[string]any{"field": "title", "colspan": 4, "helpText": helpText},
						"level",
					},
				}},
			},
		}
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/layouts", layout("Tên hiển thị")); w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("save layout with helpText + 4 cols: %d %s", w.Code, w.Body.String())
	}
	// Read back through the metadata endpoint — helpText persisted verbatim.
	w := do(t, r, http.MethodGet, "/api/v1/metadata/layouts/"+w5Entity+"?view=edit", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get layout: %d %s", w.Code, w.Body.String())
	}
	// The per-view envelope is FLATTENED (buildViewResponse): version/sections sit at the
	// view level next to isDefault/id, not under a nested layoutData key.
	var got struct {
		Data struct {
			Views map[string]map[string]any `json:"views"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	sections := got.Data.Views["edit"]["sections"].([]any)
	cell := sections[0].(map[string]any)["fields"].([]any)[0].(map[string]any)
	if cell["helpText"] != "Tên hiển thị" {
		t.Errorf("helpText round-trip = %v; want verbatim", cell["helpText"])
	}

	// Oversized helpText (>200 runes) → 422 LAYOUT_INVALID.
	long := make([]rune, 201)
	for i := range long {
		long[i] = 'ệ'
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/layouts", layout(string(long))); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("oversized helpText: %d; want 422", w.Code)
	} else if code := w5ErrCode(w.Body.Bytes()); code != "LAYOUT_INVALID" {
		t.Errorf("oversized helpText code = %q; want LAYOUT_INVALID", code)
	}
}

// w5OtherTenantID is a second tenant for the cross-tenant probe.
const w5OtherTenantID = "abcdef00-0000-0000-0000-0000000000f5"

// errCode extracts the machine code from an error envelope body.
func w5ErrCode(body []byte) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Code
}
