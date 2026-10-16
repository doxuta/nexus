//go:build integration

package handlers

// specs/052-dependent-picklist through the real HTTP stack on a migrated MySQL: the config is
// stored in sys_fields.options by the admin endpoints, read back at record time, and enforced on
// POST/PUT with details.field — including the legacy-record carve-out (a pair mismatched BEFORE the
// config existed still saves when the write does not change it).

import (
	"encoding/json"
	"net/http"
	"testing"
)

const depEntity = "it_dep_picklist"

func TestDependentPicklist_Integration(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupNamed(sx, depEntity)
	t.Cleanup(func() { cleanupNamed(sx, depEntity) })

	// The child is declared BEFORE its parent: the cross-field check runs once every field is known.
	w := do(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": depEntity, "label": "Liên hệ (phụ thuộc)",
		"fields": []map[string]any{
			{"name": "full_name", "label": "Họ tên", "fieldType": "text"},
			{"name": "district", "label": "Quận/Huyện", "fieldType": "picklist", "options": map[string]any{
				"values":           []any{"Ba Đình", "Hoàn Kiếm", "Hải Châu", "Sơn Trà"},
				"controllingField": "province",
				"dependentValues": map[string]any{
					"Hà Nội":  []any{"Ba Đình", "Hoàn Kiếm"},
					"Đà Nẵng": []any{"Hải Châu", "Sơn Trà"},
				},
			}},
			{"name": "province", "label": "Tỉnh/TP", "fieldType": "picklist",
				"options": map[string]any{"values": []any{"Hà Nội", "Đà Nẵng"}}},
			{"name": "level", "label": "Hạng", "fieldType": "picklist",
				"options": map[string]any{"values": []any{"A", "B"}}},
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}

	// The stored config round-trips: what the record path reads is what the admin wrote.
	var stored string
	if err := sx.Get(&stored, `SELECT f.options FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ?`, testTenant, depEntity, "district"); err != nil {
		t.Fatalf("read stored options: %v", err)
	}
	var opts map[string]any
	if err := json.Unmarshal([]byte(stored), &opts); err != nil || opts["controllingField"] != "province" || opts["dependentValues"] == nil {
		t.Fatalf("stored district options = %s (err=%v); want controllingField + dependentValues", stored, err)
	}

	// Mismatched pair → 422 on the child.
	w = do(t, r, http.MethodPost, "/api/v1/"+depEntity, map[string]any{"full_name": "An", "province": "Hà Nội", "district": "Hải Châu"})
	if w.Code != http.StatusUnprocessableEntity || bodyCode(t, w) != "RECORD_DEPENDENT_PICKLIST_INVALID" || bodyDetails(t, w)["field"] != "district" {
		t.Fatalf("mismatched create: %d %s; want 422 RECORD_DEPENDENT_PICKLIST_INVALID details.field=district", w.Code, w.Body.String())
	}
	// Child without parent → 422.
	w = do(t, r, http.MethodPost, "/api/v1/"+depEntity, map[string]any{"full_name": "An", "district": "Ba Đình"})
	if w.Code != http.StatusUnprocessableEntity || bodyCode(t, w) != "RECORD_DEPENDENT_PICKLIST_INVALID" {
		t.Fatalf("child without parent: %d %s; want 422", w.Code, w.Body.String())
	}
	// A plain picklist is untouched.
	w = do(t, r, http.MethodPost, "/api/v1/"+depEntity, map[string]any{"full_name": "Bình", "level": "B"})
	if w.Code != http.StatusCreated {
		t.Fatalf("plain picklist create: %d %s", w.Code, w.Body.String())
	}
	// Matching pair → 201.
	w = do(t, r, http.MethodPost, "/api/v1/"+depEntity, map[string]any{"full_name": "An", "province": "Hà Nội", "district": "Ba Đình"})
	if w.Code != http.StatusCreated {
		t.Fatalf("matching create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("parse create: %v", err)
	}
	id, _ := created.Data["id"].(string)
	if id == "" {
		t.Fatalf("create: no id — %s", w.Body.String())
	}

	// Plant the legacy state the API now refuses: a pair saved before the config existed.
	res, err := sx.Exec("UPDATE `obj_"+depEntity+"` SET `data` = JSON_SET(`data`, '$.district', 'Hải Châu') WHERE `tenant_id` = ? AND `id` = ?", testTenant, id)
	if err != nil {
		t.Fatalf("plant legacy pair: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("plant legacy pair: %d rows; want 1", n)
	}
	read := func() map[string]any {
		t.Helper()
		w := do(t, r, http.MethodGet, "/api/v1/"+depEntity+"/"+id, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("get: %d %s", w.Code, w.Body.String())
		}
		var got struct {
			Data struct {
				Data map[string]any `json:"data"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("parse get: %v", err)
		}
		return got.Data.Data
	}

	// Partial PUT not touching the pair → 200.
	if w = do(t, r, http.MethodPut, "/api/v1/"+depEntity+"/"+id, map[string]any{"full_name": "An 2"}); w.Code != http.StatusOK {
		t.Fatalf("partial PUT on a legacy record: %d %s; want 200", w.Code, w.Body.String())
	}
	// The form's save: every field resent, the stored (mismatched) pair unchanged → 200.
	w = do(t, r, http.MethodPut, "/api/v1/"+depEntity+"/"+id, map[string]any{
		"full_name": "An 3", "province": "Hà Nội", "district": "Hải Châu", "level": "A",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("form resend on a legacy record: %d %s; want 200", w.Code, w.Body.String())
	}
	if d := read(); d["full_name"] != "An 3" || d["district"] != "Hải Châu" {
		t.Fatalf("after resend: %v; want the name edit saved and the legacy district verbatim", d)
	}
	// Changing the child to another mismatch → 422.
	w = do(t, r, http.MethodPut, "/api/v1/"+depEntity+"/"+id, map[string]any{"district": "Sơn Trà"})
	if w.Code != http.StatusUnprocessableEntity || bodyDetails(t, w)["field"] != "district" {
		t.Fatalf("child changed to a mismatch: %d %s; want 422 details.field=district", w.Code, w.Body.String())
	}
	// Changing only the parent: the effective pair (Đà Nẵng, stored Hải Châu) fits → 200.
	if w = do(t, r, http.MethodPut, "/api/v1/"+depEntity+"/"+id, map[string]any{"province": "Đà Nẵng"}); w.Code != http.StatusOK {
		t.Fatalf("parent changed to fit the stored child: %d %s; want 200", w.Code, w.Body.String())
	}
	if d := read(); d["province"] != "Đà Nẵng" || d["district"] != "Hải Châu" {
		t.Fatalf("after parent change: %v", d)
	}

	// Config-time through the admin endpoints.
	w = do(t, r, http.MethodPost, "/api/v1/admin/fields", map[string]any{
		"entityName": depEntity, "name": "ward", "label": "Phường", "fieldType": "picklist", "options": map[string]any{
			"values": []any{"Phúc Xá"}, "controllingField": "quarter", "dependentValues": map[string]any{"x": []any{"Phúc Xá"}},
		},
	})
	if w.Code != http.StatusUnprocessableEntity || bodyCode(t, w) != "FIELD_DEPENDENT_PICKLIST_INVALID" {
		t.Fatalf("add field with a missing parent: %d %s; want 422 FIELD_DEPENDENT_PICKLIST_INVALID", w.Code, w.Body.String())
	}
	var provinceID string
	if err := sx.Get(&provinceID, `SELECT f.id FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ?`, testTenant, depEntity, "province"); err != nil {
		t.Fatalf("province id: %v", err)
	}
	w = do(t, r, http.MethodPut, "/api/v1/admin/fields/"+provinceID, map[string]any{"options": map[string]any{
		"values": []any{"Hà Nội", "Đà Nẵng"}, "controllingField": "district",
		"dependentValues": map[string]any{"Ba Đình": []any{"Hà Nội"}},
	}})
	if w.Code != http.StatusUnprocessableEntity || bodyCode(t, w) != "FIELD_DEPENDENT_PICKLIST_INVALID" {
		t.Fatalf("parent edit closing a cycle: %d %s; want 422 FIELD_DEPENDENT_PICKLIST_INVALID", w.Code, w.Body.String())
	}

	// FR-4 on the CHILD's later edits: the Studio inspector resends every stored key, so the
	// child's map travels with each unrelated options edit. A key the parent has since dropped, or
	// a parent soft-deleted since, must not freeze the child.
	var districtID string
	if err := sx.Get(&districtID, `SELECT f.id FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ?`, testTenant, depEntity, "district"); err != nil {
		t.Fatalf("district id: %v", err)
	}
	if w = do(t, r, http.MethodPut, "/api/v1/admin/fields/"+provinceID, map[string]any{"options": map[string]any{
		"values": []any{"Hà Nội"},
	}}); w.Code != http.StatusOK {
		t.Fatalf("parent drops a mapped value: %d %s; want 200", w.Code, w.Body.String())
	}
	childOpts := func(extra map[string]any) map[string]any {
		o := map[string]any{
			"values":           []any{"Ba Đình", "Hoàn Kiếm", "Hải Châu", "Sơn Trà", "Cầu Giấy"},
			"controllingField": "province",
			"dependentValues": map[string]any{
				"Hà Nội":  []any{"Ba Đình", "Hoàn Kiếm"},
				"Đà Nẵng": []any{"Hải Châu", "Sơn Trà"},
			},
		}
		for k, v := range extra {
			o[k] = v
		}
		return o
	}
	if w = do(t, r, http.MethodPut, "/api/v1/admin/fields/"+districtID, map[string]any{"options": childOpts(nil)}); w.Code != http.StatusOK {
		t.Fatalf("child adds a value, map carried with a key the parent dropped: %d %s; want 200", w.Code, w.Body.String())
	}
	// A NEW key the parent lacks is still judged.
	w = do(t, r, http.MethodPut, "/api/v1/admin/fields/"+districtID, map[string]any{"options": childOpts(map[string]any{
		"dependentValues": map[string]any{"Hà Nội": []any{"Ba Đình"}, "Cần Thơ": []any{"Sơn Trà"}},
	})})
	if w.Code != http.StatusUnprocessableEntity || bodyCode(t, w) != "FIELD_DEPENDENT_PICKLIST_INVALID" {
		t.Fatalf("child adds a key the parent lacks: %d %s; want 422 FIELD_DEPENDENT_PICKLIST_INVALID", w.Code, w.Body.String())
	}
	if w = do(t, r, http.MethodDelete, "/api/v1/admin/fields/"+provinceID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("soft-delete parent: %d %s", w.Code, w.Body.String())
	}
	if w = do(t, r, http.MethodPut, "/api/v1/admin/fields/"+districtID, map[string]any{"options": childOpts(map[string]any{"indexed": true})}); w.Code != http.StatusOK {
		t.Fatalf("child edit after the parent was soft-deleted: %d %s; want 200", w.Code, w.Body.String())
	}
}
