//go:build integration

// ENG-RELVIEW HTTP-contract integration tests (real MySQL):
//
//	PUT /admin/relationships/:id/view-config  → validate + persist / clear
//	GET /metadata/relationships               → viewConfig exposed as a parsed object
//
// Rides the same router composition + seed helpers as the W2 lifecycle suite
// (admin_relation_lifecycle_integration_test.go): the 1-N registry row is created through
// the REAL add-field flow and discovered through the REAL metadata endpoint. Run with:
//
//	go test -tags=integration ./internal/handlers/
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestRelViewConfigHTTP_Integration_RoundTrip(t *testing.T) {
	r, sx := newRelLifecycleRouter(t)
	cleanup := relLCCleanup(sx)
	cleanup()
	t.Cleanup(cleanup)

	relID := relLCSetupOneToN(t, r, sx, 0)
	url := "/api/v1/admin/relationships/" + relID + "/view-config"

	// Registry row: parent=customers, child=orders; the child's only business fields are
	// `total` (number) and `customer` (relation). Discovery of a stored config goes through
	// the SAME endpoint the FE RelatedTab uses.
	readViewConfig := func(tenant string) map[string]any {
		t.Helper()
		w := getRelationships(t, r, tenant, relLCParent)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /metadata/relationships: %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Data []struct {
				ID         string         `json:"id"`
				ViewConfig map[string]any `json:"viewConfig"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode relationships: %v", err)
		}
		for _, row := range resp.Data {
			if row.ID == relID {
				return row.ViewConfig
			}
		}
		t.Fatalf("registry row %s not listed: %s", relID, w.Body.String())
		return nil
	}

	// 1. PUT a valid config: columns from the CHILD entity, sort on a system column,
	//    allowCreate on. 200 + echo.
	w := relLCDo(t, r, http.MethodPut, url, testTenant, map[string]any{
		"label": "Đơn hàng", "order": 1,
		"columns":     []string{"total", "customer"},
		"sort":        map[string]any{"field": "created_at", "dir": "desc"},
		"allowCreate": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT view-config: %d %s", w.Code, w.Body.String())
	}

	// 2. The stored config is readable through the metadata discovery endpoint as a REAL
	//    object (never an escaped JSON string).
	vc := readViewConfig(testTenant)
	if vc == nil {
		t.Fatalf("viewConfig missing from GET /metadata/relationships after PUT")
	}
	if vc["label"] != "Đơn hàng" || vc["allowCreate"] != true {
		t.Errorf("stored viewConfig = %+v; want label + allowCreate round-tripped", vc)
	}
	cols, _ := vc["columns"].([]any)
	if len(cols) != 2 || cols[0] != "total" {
		t.Errorf("stored columns = %v; want [total customer]", vc["columns"])
	}

	// 3. Validation: a column that is not a child-entity field → 422 RELVIEW_COLUMN_UNKNOWN,
	//    and the previously stored config survives untouched.
	w = relLCDo(t, r, http.MethodPut, url, testTenant, map[string]any{"columns": []string{"ghost"}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT bad column: %d; want 422; body=%s", w.Code, w.Body.String())
	}
	var errResp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Code != "RELVIEW_COLUMN_UNKNOWN" {
		t.Errorf("code = %q; want RELVIEW_COLUMN_UNKNOWN", errResp.Code)
	}
	if vc := readViewConfig(testTenant); vc == nil || vc["label"] != "Đơn hàng" {
		t.Errorf("rejected PUT must not clobber the stored config; got %+v", vc)
	}

	// 4. Sort parity: a plain (non-indexed) child field is refused at CONFIG time with the
	//    dedicated code — never stored to 422 later at render time (RM-09 parity).
	w = relLCDo(t, r, http.MethodPut, url, testTenant, map[string]any{
		"sort": map[string]any{"field": "total", "dir": "asc"},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT unsortable sort: %d; want 422; body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Code != "RELVIEW_SORT_NOT_SORTABLE" {
		t.Errorf("code = %q; want RELVIEW_SORT_NOT_SORTABLE", errResp.Code)
	}

	// 5. Cross-tenant isolation: a tenant-B admin token addressing tenant A's relationship
	//    id gets 404, and tenant A's config is untouched.
	w = relLCDo(t, r, http.MethodPut, url, relLCOtherTenant, map[string]any{"label": "HACKED"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant PUT: %d; want 404; body=%s", w.Code, w.Body.String())
	}
	if vc := readViewConfig(testTenant); vc == nil || vc["label"] != "Đơn hàng" {
		t.Errorf("cross-tenant PUT must not touch tenant A's config; got %+v", vc)
	}

	// 6. PUT null clears the config → viewConfig disappears from the metadata read (the FE
	//    falls back to its default rendering).
	w = relLCDo(t, r, http.MethodPut, url, testTenant, json.RawMessage("null"))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT null (clear): %d %s", w.Code, w.Body.String())
	}
	if vc := readViewConfig(testTenant); vc != nil {
		t.Errorf("cleared config still served: %+v", vc)
	}
}
