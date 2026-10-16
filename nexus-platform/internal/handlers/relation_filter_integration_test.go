//go:build integration

package handlers

// specs/081 (queue #304): options.filterBy is validated on all three config-time entry points —
// create entity, add field, edit field — through the real HTTP stack.

import (
	"net/http"
	"testing"
)

func TestRelationFilter_ConfigPaths_Integration(t *testing.T) {
	const target, host = "it_rf_target", "it_rf_host"
	r, sx := newFullRouter(t)
	for _, n := range []string{target, host, host + "2"} {
		cleanupNamed(sx, n)
	}
	t.Cleanup(func() {
		for _, n := range []string{target, host, host + "2"} {
			cleanupNamed(sx, n)
		}
	})

	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": target, "label": "Đích",
		"fields": []map[string]any{{"name": "title", "label": "Tên", "fieldType": "text"}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("create target: %d %s", w.Code, w.Body.String())
	}
	rel := func(name string, filterBy any) map[string]any {
		o := map[string]any{"relatedEntity": target}
		if filterBy != nil {
			o["filterBy"] = filterBy
		}
		return map[string]any{"name": name, "label": name, "fieldType": "relation", "options": o}
	}
	fb := func(parent string) map[string]any { return map[string]any{"field": "x", "parentField": parent} }
	want422 := func(label string, code int, body string) {
		t.Helper()
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s; want 422 FIELD_RELATION_FILTER_INVALID", label, code, body)
		}
	}

	// --- ValidateCreateEntity: missing parent, non-relation parent, cycle.
	createCases := map[string][]map[string]any{
		"create/missing parent":      {rel("a", nil), rel("b", fb("nope"))},
		"create/non-relation parent": {{"name": "txt", "label": "t", "fieldType": "text"}, rel("b", fb("txt"))},
		"create/cycle":               {rel("a", fb("b")), rel("b", fb("a"))},
	}
	for label, fields := range createCases {
		w := do(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{"name": host + "2", "label": "H2", "fields": fields})
		want422(label, w.Code, w.Body.String())
		if bodyCode(t, w) != "FIELD_RELATION_FILTER_INVALID" {
			t.Fatalf("%s: code = %s; want FIELD_RELATION_FILTER_INVALID", label, w.Body.String())
		}
		cleanupNamed(sx, host+"2")
	}

	// --- Valid create: parent declared AFTER the child.
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": host, "label": "Host",
		"fields": []map[string]any{
			rel("child", fb("parent")), rel("parent", nil), rel("alt", nil),
			{"name": "txt", "label": "t", "fieldType": "text"},
		},
	}); w.Code != http.StatusCreated {
		t.Fatalf("valid create: %d %s", w.Code, w.Body.String())
	}

	// --- AddField.
	addCases := map[string]map[string]any{
		"add/missing parent":      rel("c1", fb("nope")),
		"add/non-relation parent": rel("c2", fb("txt")),
		"add/self":                rel("c3", fb("c3")),
	}
	for label, f := range addCases {
		f["entityName"] = host
		w := do(t, r, http.MethodPost, "/api/v1/admin/fields", f)
		want422(label, w.Code, w.Body.String())
		if bodyCode(t, w) != "FIELD_RELATION_FILTER_INVALID" {
			t.Fatalf("%s: %s", label, w.Body.String())
		}
	}
	ok := rel("c4", fb("parent"))
	ok["entityName"] = host
	if w := do(t, r, http.MethodPost, "/api/v1/admin/fields", ok); w.Code != http.StatusCreated {
		t.Fatalf("add valid: %d %s; want 201", w.Code, w.Body.String())
	}

	// --- EditField.
	id := func(name string) string {
		var s string
		if err := sx.Get(&s, `SELECT f.id FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
			WHERE e.tenant_id = ? AND e.name = ? AND f.name = ?`, testTenant, host, name); err != nil {
			t.Fatalf("id %s: %v", name, err)
		}
		return s
	}
	put := func(name string, opts map[string]any) (int, string) {
		w := do(t, r, http.MethodPut, "/api/v1/admin/fields/"+id(name), map[string]any{"options": opts})
		return w.Code, w.Body.String()
	}
	opts := func(filterBy any) map[string]any {
		o := map[string]any{"relatedEntity": target}
		if filterBy != nil {
			o["filterBy"] = filterBy
		}
		return o
	}
	editCases := map[string]struct {
		field string
		fb    any
	}{
		"edit/missing parent":      {"parent", fb("nope")},
		"edit/non-relation parent": {"parent", fb("txt")},
		"edit/cycle":               {"parent", fb("child")}, // child already filters by parent
	}
	for label, c := range editCases {
		code, body := put(c.field, opts(c.fb))
		want422(label, code, body)
	}
	// A valid change (a no-op edit would hit UpdateFieldMutable's 0-rows-affected → 404).
	if code, body := put("parent", opts(fb("alt"))); code != http.StatusOK {
		t.Fatalf("edit valid: %d %s; want 200", code, body)
	}

	// --- Stored parent that has since vanished: an unchanged filterBy must not freeze the field.
	if w := do(t, r, http.MethodDelete, "/api/v1/admin/fields/"+id("parent"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("soft-delete parent: %d %s", w.Code, w.Body.String())
	}
	o := opts(fb("parent"))
	o["indexed"] = true
	if code, body := put("child", o); code != http.StatusOK {
		t.Fatalf("edit with vanished stored parent: %d %s; want 200", code, body)
	}
	// ...but pointing at a NEW missing parent is still judged.
	if code, body := put("child", opts(fb("ghost"))); code != http.StatusUnprocessableEntity {
		t.Fatalf("edit to new missing parent: %d %s; want 422", code, body)
	}
}
