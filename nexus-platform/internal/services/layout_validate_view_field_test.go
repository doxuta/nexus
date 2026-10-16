package services

import (
	"testing"

	"nexus/platform/internal/metadata"
)

// listLayoutWith builds a minimal VALID list layout and merges the given extra top-level keys,
// so each case below differs from the baseline only by the key under test.
func listLayoutWith(extra map[string]any) map[string]any {
	l := map[string]any{
		"version":     1,
		"columns":     []any{map[string]any{"field": "subject"}},
		"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
	}
	for k, v := range extra {
		l[k] = v
	}
	return l
}

// ticketFields mirrors the shape that made the guess wrong in the field: THREE picklists and TWO
// date fields, where the business-correct choice is NOT the first of either kind.
func ticketFields() []metadata.FieldDefinition {
	return []metadata.FieldDefinition{
		makeField("subject", "text", false),
		makeField("priority", "picklist", false), // first picklist — the old guess
		makeField("channel", "picklist", false),  // second picklist
		makeField("status", "picklist", false),   // the one a maker actually wants on the board
		makeField("reported_at", "date", false),  // first date field — the old guess
		makeField("due_date", "datetime", false), // the one a maker actually wants on the calendar
	}
}

// TestValidateLayout_ListKanbanCalendarField pins the declared-view-field contract (DAILY-QUEUE #5):
// a list layout MAY declare which picklist the Kanban board groups by and which date field the
// Calendar lays out on, and the validator strict-rejects a declaration that names a missing field
// or a field of the wrong type — a silently-ignored declaration would recreate the very bug this
// slice fixes (the machine guesses and the maker cannot tell).
func TestValidateLayout_ListKanbanCalendarField(t *testing.T) {
	fields := ticketFields()

	valid := []struct {
		name   string
		layout map[string]any
	}{
		{"kanbanField names a picklist", listLayoutWith(map[string]any{"kanbanField": "status"})},
		{"calendarField names a date", listLayoutWith(map[string]any{"calendarField": "reported_at"})},
		{"calendarField names a datetime", listLayoutWith(map[string]any{"calendarField": "due_date"})},
		{"both declared together", listLayoutWith(map[string]any{"kanbanField": "status", "calendarField": "due_date"})},
		{"neither declared keeps the old guess", listLayoutWith(nil)},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateLayout("list", tc.layout, fields); err != nil {
				t.Fatalf("expected valid layout, got %v", err)
			}
		})
	}

	rejected := []struct {
		name   string
		layout map[string]any
	}{
		{"kanbanField names a missing field", listLayoutWith(map[string]any{"kanbanField": "nope"})},
		{"kanbanField names a non-picklist", listLayoutWith(map[string]any{"kanbanField": "subject"})},
		{"kanbanField names a date field", listLayoutWith(map[string]any{"kanbanField": "due_date"})},
		{"kanbanField is not a string", listLayoutWith(map[string]any{"kanbanField": 7})},
		{"kanbanField is blank", listLayoutWith(map[string]any{"kanbanField": "   "})},
		{"calendarField names a missing field", listLayoutWith(map[string]any{"calendarField": "nope"})},
		{"calendarField names a non-date", listLayoutWith(map[string]any{"calendarField": "status"})},
		{"calendarField is not a string", listLayoutWith(map[string]any{"calendarField": true})},
		// An untrimmed name must be REJECTED, not accepted-by-trim: it would save clean and then
		// be dropped at serve time for every caller, leaving the declaration silently dead.
		{"kanbanField has leading whitespace", listLayoutWith(map[string]any{"kanbanField": " status"})},
		{"calendarField has trailing whitespace", listLayoutWith(map[string]any{"calendarField": "due_date "})},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			if got := codeOf(validateLayout("list", tc.layout, fields)); got != "LAYOUT_INVALID" {
				t.Fatalf("expected LAYOUT_INVALID, got %q", got)
			}
		})
	}
}

// TestValidateLayout_ViewFieldsAreListOnly keeps the two keys off edit/detail layouts: they are a
// LIST-view concern, and the edit/detail allowlist stays closed.
func TestValidateLayout_ViewFieldsAreListOnly(t *testing.T) {
	for _, vt := range []string{"edit", "detail"} {
		layout := map[string]any{
			"version":     1,
			"kanbanField": "status",
			"sections": []any{map[string]any{
				"title": "S", "columns": 1, "fields": []any{"subject"},
			}},
		}
		if got := codeOf(validateLayout(vt, layout, ticketFields())); got != "LAYOUT_INVALID" {
			t.Fatalf("view %s: expected LAYOUT_INVALID for kanbanField, got %q", vt, got)
		}
	}
}

// TestStripHiddenLayoutFields_DropsDeclaredViewFields keeps the declared view fields under the
// SAME field-ACL prune as columns: a maker may declare kanbanField on a field this particular user
// is not allowed to see, and serving the name back would leak it. Dropping the key hands the
// renderer the honest old guess instead of a field it will never receive.
func TestStripHiddenLayoutFields_DropsDeclaredViewFields(t *testing.T) {
	layout := map[string]any{
		"version":       1,
		"columns":       []any{map[string]any{"field": "subject"}, map[string]any{"field": "status"}},
		"defaultSort":   map[string]any{"field": "created_at", "order": "desc"},
		"kanbanField":   "status",
		"calendarField": "due_date",
	}
	out := StripHiddenLayoutFields(layout, LayoutViewList, map[string]struct{}{"status": {}})

	if _, still := out["kanbanField"]; still {
		t.Fatalf("kanbanField names a hidden field but survived the prune: %v", out["kanbanField"])
	}
	if got, _ := out["calendarField"].(string); got != "due_date" {
		t.Fatalf("calendarField names a VISIBLE field and must survive, got %q", got)
	}
	if _, untouched := layout["kanbanField"]; !untouched {
		t.Fatal("prune is serve-time only and must not mutate the stored layout")
	}
}

// TestStripHiddenLayoutFields_PrunesViewFieldsEvenWithoutColumns covers a row that could only be
// written straight to the DB (the validator requires a columns array on every list layout). The
// column prune returns early on such a row; the declared view fields must still be dropped, or a
// malformed row would serve a hidden field's NAME to a user who cannot see that field.
func TestStripHiddenLayoutFields_PrunesViewFieldsEvenWithoutColumns(t *testing.T) {
	hidden := map[string]struct{}{"status": {}}

	noColumns := map[string]any{"version": 1, "kanbanField": "status"}
	if out := StripHiddenLayoutFields(noColumns, LayoutViewList, hidden); out["kanbanField"] != nil {
		t.Fatalf("layout with no columns key still served the hidden name: %v", out["kanbanField"])
	}
	badColumns := map[string]any{"version": 1, "columns": "not-an-array", "kanbanField": "status"}
	if out := StripHiddenLayoutFields(badColumns, LayoutViewList, hidden); out["kanbanField"] != nil {
		t.Fatalf("layout with a malformed columns key still served the hidden name: %v", out["kanbanField"])
	}
	// Nothing hidden and nothing declared: handed straight back, no clone, no mutation.
	plain := map[string]any{"version": 1, "columns": []any{}}
	if out := StripHiddenLayoutFields(plain, LayoutViewList, hidden); len(out) != len(plain) {
		t.Fatalf("a layout declaring nothing must be served unchanged, got %v", out)
	}
}

// TestStripHiddenLayoutFields_DefaultSortOnHiddenFieldFallsBack: defaultSort may name any sortable
// field (#273) but the list allowlist excludes field-ACL-hidden ones, so serving it = 422 on open.
func TestStripHiddenLayoutFields_DefaultSortOnHiddenFieldFallsBack(t *testing.T) {
	mk := func(f string) map[string]any {
		return map[string]any{
			"version":     1,
			"columns":     []any{map[string]any{"field": "subject"}},
			"defaultSort": map[string]any{"field": f, "order": "asc"},
		}
	}
	hidden := map[string]struct{}{"stage": {}}

	out := StripHiddenLayoutFields(mk("stage"), LayoutViewList, hidden)
	ds, _ := out["defaultSort"].(map[string]any)
	if ds["field"] != "created_at" || ds["order"] != "desc" {
		t.Fatalf("hidden defaultSort must fall back to created_at desc, got %v", ds)
	}
	in := mk("stage")
	StripHiddenLayoutFields(in, LayoutViewList, hidden)
	if in["defaultSort"].(map[string]any)["field"] != "stage" {
		t.Fatal("strip must not mutate the stored layout")
	}
	keep := StripHiddenLayoutFields(mk("subject"), LayoutViewList, hidden)
	if keep["defaultSort"].(map[string]any)["field"] != "subject" {
		t.Fatalf("visible defaultSort must survive, got %v", keep["defaultSort"])
	}
}
