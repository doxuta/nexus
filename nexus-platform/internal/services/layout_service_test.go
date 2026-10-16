package services

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/metadata"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeField(name, fieldType string, required bool) metadata.FieldDefinition {
	return metadata.FieldDefinition{Name: name, FieldType: fieldType, IsRequired: required}
}

// liveFields builds a slice of non-required text fields with the given names.
func liveFieldsOf(names ...string) []metadata.FieldDefinition {
	fields := make([]metadata.FieldDefinition, len(names))
	for i, n := range names {
		fields[i] = makeField(n, "text", false)
	}
	return fields
}

// jsonRoundTrip marshals and unmarshals a map so all number values become float64,
// matching what encoding/json produces when decoding into map[string]any from HTTP body.
func jsonRoundTrip(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("jsonRoundTrip marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("jsonRoundTrip unmarshal: %v", err)
	}
	return out
}

// cellFieldName extracts the field name from a cell (string or object).
func cellFieldName(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case map[string]any:
		if f, ok := v["field"].(string); ok {
			return f
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// generateDefault unit tests
// ---------------------------------------------------------------------------

func TestGenerateDefault_EditColumns(t *testing.T) {
	cases := []struct {
		name     string
		fields   []metadata.FieldDefinition
		wantCols int
	}{
		{"0 fields → columns=1", nil, 1},
		{"1 field → columns=1", []metadata.FieldDefinition{makeField("a", "text", false)}, 1},
		{"3 fields → columns=1", []metadata.FieldDefinition{
			makeField("a", "text", false), makeField("b", "text", false), makeField("c", "text", false),
		}, 1},
		{"4 fields → columns=2", []metadata.FieldDefinition{
			makeField("a", "text", false), makeField("b", "text", false),
			makeField("c", "text", false), makeField("d", "text", false),
		}, 2},
		{"5 fields → columns=2", []metadata.FieldDefinition{
			makeField("a", "text", false), makeField("b", "text", false),
			makeField("c", "text", false), makeField("d", "text", false),
			makeField("e", "text", false),
		}, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layout := generateDefault("test", "edit", tc.fields)
			sections := layout["sections"].([]any)
			if len(sections) == 0 {
				t.Fatal("sections is empty")
			}
			sec := sections[0].(map[string]any)
			cols, _ := sec["columns"].(int)
			if cols != tc.wantCols {
				t.Errorf("columns = %d; want %d", cols, tc.wantCols)
			}
		})
	}
}

func TestGenerateDefault_SystemFieldExclusion(t *testing.T) {
	fields := []metadata.FieldDefinition{
		makeField("id", "text", false),
		makeField("tenant_id", "text", false),
		makeField("created_at", "datetime", false),
		makeField("updated_at", "datetime", false),
		makeField("deleted_at", "datetime", false),
		makeField("created_by", "text", false),
		makeField("modified_by", "text", false),
		makeField("team_set_id", "text", false),
		makeField("name", "text", false), // only non-system field
	}

	for _, vt := range []string{"edit", "detail"} {
		t.Run(vt, func(t *testing.T) {
			layout := generateDefault("test", vt, fields)
			sections := layout["sections"].([]any)
			sec := sections[0].(map[string]any)
			cells := sec["fields"].([]any)
			for _, c := range cells {
				name := cellFieldName(c)
				if isSystemField(name) {
					t.Errorf("system field %q found in generated cells", name)
				}
			}
		})
	}
}

func TestGenerateDefault_NameASCOrder(t *testing.T) {
	// Fields returned by ListFields are name ASC; generated cells must preserve that order.
	fields := []metadata.FieldDefinition{
		makeField("alpha", "text", false),
		makeField("beta", "text", false),
		makeField("gamma", "text", false),
	}
	layout := generateDefault("test", "edit", fields)
	sections := layout["sections"].([]any)
	sec := sections[0].(map[string]any)
	cells := sec["fields"].([]any)
	want := []string{"alpha", "beta", "gamma"}
	for i, c := range cells {
		got := cellFieldName(c)
		if got != want[i] {
			t.Errorf("cells[%d] = %q; want %q (name ASC)", i, got, want[i])
		}
	}
}

func TestGenerateDefault_RichTextFullWidth(t *testing.T) {
	fields := []metadata.FieldDefinition{
		makeField("body", "rich_text", false),
		makeField("title", "text", false),
	}
	layout := generateDefault("test", "edit", fields)
	sections := layout["sections"].([]any)
	sec := sections[0].(map[string]any)
	cells := sec["fields"].([]any)

	for _, cellRaw := range cells {
		obj, ok := cellRaw.(map[string]any)
		if !ok {
			continue
		}
		if obj["field"] == "body" {
			fw, _ := obj["fullWidth"].(bool)
			if !fw {
				t.Error("rich_text field should have fullWidth:true in generated layout")
			}
		}
	}
}

func TestGenerateDefault_ZeroFields(t *testing.T) {
	for _, vt := range []string{"edit", "detail", "list"} {
		t.Run(vt, func(t *testing.T) {
			layout := generateDefault("test", vt, nil)
			if layout == nil {
				t.Fatal("generateDefault returned nil for 0 fields")
			}
			if layout["version"] == nil {
				t.Error("version must be present")
			}
			switch vt {
			case "edit", "detail":
				sections, _ := layout["sections"].([]any)
				if len(sections) == 0 {
					t.Error("sections must be non-empty for 0 user fields")
				}
				// spec §4.2: 0-field entity gets "id" as identifier fallback cell
				sec0, _ := sections[0].(map[string]any)
				cells, _ := sec0["fields"].([]any)
				if len(cells) == 0 {
					t.Error("section[0].fields must be non-empty (id fallback)")
				} else if cells[0] != "id" {
					t.Errorf("section[0].fields[0] = %q; want \"id\" (identifier fallback)", cells[0])
				}
			case "list":
				cols, _ := layout["columns"].([]any)
				if len(cols) == 0 {
					t.Error("columns must be non-empty for 0 user fields (id fallback)")
				}
				firstCol, _ := cols[0].(map[string]any)
				if firstCol["field"] != "id" {
					t.Errorf("first list column = %q; want %q (id fallback)", firstCol["field"], "id")
				}
			}
		})
	}
}

func TestGenerateDefault_ListFallbackChain(t *testing.T) {
	t.Run("first text field", func(t *testing.T) {
		fields := []metadata.FieldDefinition{
			makeField("amount", "number", false),
			makeField("title", "text", false),
		}
		layout := generateDefault("test", "list", fields)
		cols := layout["columns"].([]any)
		first := cols[0].(map[string]any)
		if first["field"] != "title" {
			t.Errorf("first col = %q; want title", first["field"])
		}
	})

	t.Run("field named name when no text", func(t *testing.T) {
		fields := []metadata.FieldDefinition{
			makeField("name", "number", false),
		}
		layout := generateDefault("test", "list", fields)
		cols := layout["columns"].([]any)
		first := cols[0].(map[string]any)
		if first["field"] != "name" {
			t.Errorf("first col = %q; want name", first["field"])
		}
	})

	t.Run("id fallback when no text and no name", func(t *testing.T) {
		fields := []metadata.FieldDefinition{
			makeField("amount", "number", false),
		}
		layout := generateDefault("test", "list", fields)
		cols := layout["columns"].([]any)
		first := cols[0].(map[string]any)
		if first["field"] != "id" {
			t.Errorf("first col = %q; want id (fallback)", first["field"])
		}
	})
}

func TestGenerateDefault_ListMaxFiveCols(t *testing.T) {
	fields := make([]metadata.FieldDefinition, 10)
	for i := range fields {
		fields[i] = makeField(string(rune('a'+i)), "text", false)
	}
	layout := generateDefault("test", "list", fields)
	cols := layout["columns"].([]any)
	if len(cols) > 5 {
		t.Errorf("list columns = %d; want ≤5", len(cols))
	}
}

func TestGenerateDefault_ListExcludesHeavyTypes(t *testing.T) {
	fields := []metadata.FieldDefinition{
		makeField("title", "text", false),
		makeField("body", "rich_text", false),
		makeField("tags", "multi_select", false),
		makeField("addr", "composite", false),
		makeField("amount", "number", false),
	}
	layout := generateDefault("test", "list", fields)
	cols := layout["columns"].([]any)
	for _, colRaw := range cols {
		col := colRaw.(map[string]any)
		switch col["field"] {
		case "body", "tags", "addr":
			t.Errorf("heavy field %q should not be in list columns", col["field"])
		}
	}
}

// DAILY-QUEUE #273: a relation/user field is a legal list column (the FE expands it to a label);
// the default list used to drop them, so "lịch hẹn của bệnh nhân nào" was unanswerable.
func TestGenerateDefault_ListKeepsRelationAndUserColumns(t *testing.T) {
	fields := []metadata.FieldDefinition{
		makeField("title", "text", false),
		makeField("patient", "relation", false),
		makeField("doctor", "user", false),
		makeField("body", "rich_text", false),
	}
	cols := generateDefault("test", "list", fields)["columns"].([]any)
	got := map[string]bool{}
	for _, c := range cols {
		got[c.(map[string]any)["field"].(string)] = true
	}
	if !got["patient"] || !got["doctor"] || got["body"] {
		t.Errorf("columns = %v; want patient+doctor in, body out", got)
	}
}

// DAILY-QUEUE #273: defaultSort.field accepts exactly what the engine can sort (the SAME allowlist
// buildSortAllowlist backs ?sort= with): unique, relation/user FK, indexed — not plain JSON fields.
func TestValidateLayout_DefaultSortFollowsEngineSortAllowlist(t *testing.T) {
	indexed := `{"indexed":true}`
	fields := []metadata.FieldDefinition{
		{Name: "name", FieldType: "text"},
		{Name: "starts_at", FieldType: "datetime", Options: &indexed},
		{Name: "code", FieldType: "text", IsUnique: true},
		{Name: "patient", FieldType: "relation"},
	}
	cases := []struct {
		field   string
		wantErr bool
	}{
		{"created_at", false}, {"starts_at", false}, {"code", false}, {"patient", false},
		{"name", true}, {"nope", true},
	}
	for _, tc := range cases {
		layout := map[string]any{
			"version":     float64(1),
			"columns":     []any{map[string]any{"field": "name"}},
			"defaultSort": map[string]any{"field": tc.field, "order": "asc"},
		}
		err := validateLayout("list", layout, fields)
		if (err != nil) != tc.wantErr {
			t.Errorf("defaultSort %q: err=%v wantErr=%v", tc.field, err, tc.wantErr)
		}
	}
}

// A saved defaultSort that stopped being sortable (field un-indexed or deleted later) must not
// turn every list load into a 422 SORT_NOT_ALLOWED: serve time falls back to created_at desc.
func TestApplyServeTimeSortable_StaleDefaultSortFallsBack(t *testing.T) {
	sortable := func(f string) bool { return f == "created_at" || f == "starts_at" }
	mk := func(f string) map[string]any {
		return map[string]any{
			"columns":     []any{map[string]any{"field": "name"}},
			"defaultSort": map[string]any{"field": f, "order": "asc"},
		}
	}
	ds := applyServeTimeSortable(mk("gone"), LayoutViewList, sortable)["defaultSort"].(map[string]any)
	if ds["field"] != "created_at" || ds["order"] != "desc" {
		t.Errorf("stale defaultSort = %v; want created_at desc", ds)
	}
	ds = applyServeTimeSortable(mk("starts_at"), LayoutViewList, sortable)["defaultSort"].(map[string]any)
	if ds["field"] != "starts_at" || ds["order"] != "asc" {
		t.Errorf("live defaultSort = %v; want untouched", ds)
	}
}

// DAILY-QUEUE #4 (sortable-columns-from-grid): default-gen is a WRITER of layout JSON, and no
// writer may decide `sortable` — the key used to be hard-coded false on every column, which made
// every header a dead button even for fields the engine sorts (indexed picklist, unique, relation).
// Serve time (GetLayout) is the only decider, so the generator must not emit the key at all.
func TestGenerateDefault_ListEmitsNoSortableKey(t *testing.T) {
	fields := []metadata.FieldDefinition{makeField("title", "text", false)}
	for _, fs := range [][]metadata.FieldDefinition{fields, nil} { // user field lead, and the "id" fallback lead
		layout := generateDefault("test", "list", fs)
		cols := layout["columns"].([]any)
		for _, colRaw := range cols {
			col := colRaw.(map[string]any)
			if _, has := col["sortable"]; has {
				t.Errorf("column %q carries a sortable key (%v); default-gen must not decide sortable — serve time does", col["field"], col["sortable"])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Serve-time sortable (DAILY-QUEUE #4) — GetLayout stamps `sortable` on every list column from
// the engine's own sort allowlist (buildSortAllowlist: system cols + unique + relation/user +
// indexed). A stored layout's own `sortable` value — pack literal, old builder output — is
// overridden both ways, so layouts saved before this slice self-heal without a re-save.
// ---------------------------------------------------------------------------

type sortableFakeMeta struct{ fields []metadata.FieldDefinition }

func (f *sortableFakeMeta) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	return &metadata.EntityDefinition{ID: "ent-1", Name: name}, nil
}
func (f *sortableFakeMeta) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return f.fields, nil
}

type sortableFakeRepo struct{ stored []metadata.LayoutDefinition }

func (r *sortableFakeRepo) GetByEntity(_ context.Context, _, _ string) ([]metadata.LayoutDefinition, error) {
	return r.stored, nil
}
func (r *sortableFakeRepo) GetByID(_ context.Context, _, _ string) (*metadata.LayoutDefinition, error) {
	return nil, metadata.ErrLayoutNotFound
}
func (r *sortableFakeRepo) Upsert(_ context.Context, _ *metadata.LayoutDefinition) error { return nil }
func (r *sortableFakeRepo) UpdateByID(_ context.Context, _, _, _ string) error           { return nil }
func (r *sortableFakeRepo) HardDelete(_ context.Context, _, _ string) error              { return nil }

func sortableFixtureFields() []metadata.FieldDefinition {
	indexed := `{"indexed":true,"values":["a","b"]}`
	return []metadata.FieldDefinition{
		{Name: "name", FieldType: "text"},                         // plain JSON text → NOT sortable
		{Name: "stage", FieldType: "picklist", Options: &indexed}, // D1 indexed → sortable
		{Name: "code", FieldType: "text", IsUnique: true},         // unique gen-column → sortable
		{Name: "account", FieldType: "relation"},                  // FK <f>_id → sortable
		{Name: "amount", FieldType: "currency"},                   // not indexed → NOT sortable (honest: "Giá trị" stays dead)
	}
}

func listColumnSortable(t *testing.T, layout map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	cols, _ := layout["columns"].([]any)
	for _, colRaw := range cols {
		col := colRaw.(map[string]any)
		name, _ := col["field"].(string)
		v, has := col["sortable"]
		if !has {
			t.Errorf("served column %q has no sortable key — serve time must stamp every list column", name)
			continue
		}
		b, ok := v.(bool)
		if !ok {
			t.Errorf("served column %q sortable=%v (%T); want bool", name, v, v)
		}
		out[name] = b
	}
	return out
}

func TestGetLayout_ListSortableDecidedAtServeTime(t *testing.T) {
	// Stored layout LIES both ways: name says sortable:true (pack literal on a plain text field),
	// stage says nothing, code says false, account/amount say nothing.
	stored := `{"version":1,"columns":[
		{"field":"name","sortable":true},
		{"field":"stage"},
		{"field":"code","sortable":false},
		{"field":"account"},
		{"field":"amount"}
	],"defaultSort":{"field":"created_at","order":"desc"}}`
	svc := NewLayoutService(
		&sortableFakeMeta{fields: sortableFixtureFields()},
		&sortableFakeRepo{stored: []metadata.LayoutDefinition{{ID: "lay-1", ViewType: LayoutViewList, LayoutJSON: stored}}},
		nil,
	)
	res, err := svc.GetLayout(context.Background(), "tenant-1", "deals", LayoutViewList)
	if err != nil {
		t.Fatalf("GetLayout: %v", err)
	}
	view := res.Views[LayoutViewList]
	if view.IsDefault {
		t.Fatal("stored list layout must serve as non-default")
	}
	got := listColumnSortable(t, view.LayoutData)
	want := map[string]bool{"name": false, "stage": true, "code": true, "account": true, "amount": false}
	for f, w := range want {
		if got[f] != w {
			t.Errorf("served sortable[%q] = %v; want %v (engine allowlist decides, stored value ignored)", f, got[f], w)
		}
	}
}

func TestGetLayout_DefaultListColumnsCarrySortable(t *testing.T) {
	// No stored layout → generated default; serve time must still stamp the key on every column.
	svc := NewLayoutService(&sortableFakeMeta{fields: sortableFixtureFields()}, &sortableFakeRepo{}, nil)
	res, err := svc.GetLayout(context.Background(), "tenant-1", "deals", LayoutViewList)
	if err != nil {
		t.Fatalf("GetLayout: %v", err)
	}
	view := res.Views[LayoutViewList]
	if !view.IsDefault {
		t.Fatal("no stored layout → must serve the generated default")
	}
	got := listColumnSortable(t, view.LayoutData)
	if got["name"] {
		t.Error("default lead column name (plain text) must serve sortable:false")
	}
	if !got["stage"] {
		t.Error("indexed picklist stage must serve sortable:true even on a generated default")
	}
	if got["amount"] {
		t.Error("currency amount without index must serve sortable:false")
	}
}

func TestGenerateDefault_DefaultSort(t *testing.T) {
	layout := generateDefault("test", "list", nil)
	ds, _ := layout["defaultSort"].(map[string]any)
	if ds == nil {
		t.Fatal("defaultSort is nil")
	}
	if ds["field"] != "created_at" {
		t.Errorf("defaultSort.field = %q; want created_at", ds["field"])
	}
	if ds["order"] != "desc" {
		t.Errorf("defaultSort.order = %q; want desc", ds["order"])
	}
}

func TestGenerateDefault_Version1(t *testing.T) {
	for _, vt := range []string{"edit", "detail", "list"} {
		layout := generateDefault("test", vt, nil)
		if layout["version"] != 1 {
			t.Errorf("view %s: version = %v; want 1", vt, layout["version"])
		}
	}
}

// ---------------------------------------------------------------------------
// systemFieldSet shared between gen and validate (spec §6.2)
// ---------------------------------------------------------------------------

func TestSystemFieldSet_SharedBetweenGenAndValidate(t *testing.T) {
	for name := range systemFieldSet {
		if !isSystemField(name) {
			t.Errorf("isSystemField(%q) = false; want true", name)
		}
	}
}

// ---------------------------------------------------------------------------
// validateLayout unit tests — one case per §4.4 rule
// ---------------------------------------------------------------------------

// validEditLayout builds a minimal valid edit layout referencing the given fields.
func validEditLayout(fields ...string) map[string]any {
	cells := make([]any, len(fields))
	for i, f := range fields {
		cells[i] = f
	}
	return map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{
				"columns": float64(1),
				"fields":  cells,
			},
		},
	}
}

// validListLayout builds a minimal valid list layout with one field column.
func validListLayout(field string) map[string]any {
	return map[string]any{
		"version": float64(1),
		"columns": []any{
			map[string]any{"field": field},
		},
		"defaultSort": map[string]any{
			"field": "created_at",
			"order": "desc",
		},
	}
}

func TestValidateLayout_Rule1_ViewType(t *testing.T) {
	cases := []struct {
		viewType string
		layout   map[string]any
		wantCode string
	}{
		{"edit", validEditLayout("a"), ""},
		{"detail", validEditLayout("a"), ""},
		{"list", validListLayout("a"), ""},
		{"Edit", validEditLayout("a"), "LAYOUT_INVALID"},
		{" list ", validListLayout("a"), "LAYOUT_INVALID"},
		{"foo", validEditLayout("a"), "LAYOUT_INVALID"},
		{"", validEditLayout("a"), "LAYOUT_INVALID"},
	}
	for _, tc := range cases {
		t.Run("vt="+tc.viewType, func(t *testing.T) {
			err := validateLayout(tc.viewType, tc.layout, liveFieldsOf("a"))
			if codeOf(err) != tc.wantCode {
				t.Errorf("viewType=%q: code=%q; want %q (err=%v)", tc.viewType, codeOf(err), tc.wantCode, err)
			}
		})
	}
}

func TestValidateLayout_Rule2_Version(t *testing.T) {
	makeSections := func() []any {
		return []any{map[string]any{"columns": float64(1), "fields": []any{"a"}}}
	}
	cases := []struct {
		name     string
		layout   map[string]any
		wantCode string
	}{
		{"version 1 ok", map[string]any{"version": float64(1), "sections": makeSections()}, ""},
		{"version missing", map[string]any{"sections": makeSections()}, "LAYOUT_INVALID"},
		{"version 0 invalid", map[string]any{"version": float64(0), "sections": makeSections()}, "LAYOUT_INVALID"},
		{"version 2 future", map[string]any{"version": float64(2), "sections": makeSections()}, "LAYOUT_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLayout("edit", tc.layout, liveFieldsOf("a"))
			if codeOf(err) != tc.wantCode {
				t.Errorf("code=%q; want %q (err=%v)", codeOf(err), tc.wantCode, err)
			}
		})
	}
}

func TestValidateLayout_Rule3_ShapePerView(t *testing.T) {
	// edit/detail must have sections, not columns
	editWithColumns := map[string]any{
		"version": float64(1),
		"columns": []any{map[string]any{"field": "a"}},
	}
	if err := validateLayout("edit", editWithColumns, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("edit with columns[] should be LAYOUT_INVALID")
	}

	// list must have columns, not sections
	listWithSections := map[string]any{
		"version":  float64(1),
		"sections": []any{map[string]any{"columns": float64(1), "fields": []any{"a"}}},
	}
	if err := validateLayout("list", listWithSections, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("list with sections[] should be LAYOUT_INVALID")
	}
}

func TestValidateLayout_Rule4_FieldAllowlist(t *testing.T) {
	layout := validEditLayout("unknown_field")
	err := validateLayout("edit", layout, liveFieldsOf("real_field"))
	if codeOf(err) != "LAYOUT_INVALID" {
		t.Errorf("unknown field should be LAYOUT_INVALID, got code=%q", codeOf(err))
	}
}

func TestValidateLayout_Rule5_CellShape(t *testing.T) {
	// cell with both field and spacer
	layout := map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{
				"columns": float64(1),
				"fields":  []any{map[string]any{"field": "a", "spacer": true}},
			},
		},
	}
	if err := validateLayout("edit", layout, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("cell with both field and spacer should be LAYOUT_INVALID")
	}

	// cell with neither
	layout2 := map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{
				"columns": float64(1),
				"fields":  []any{map[string]any{"labelOverride": "X"}},
			},
		},
	}
	if err := validateLayout("edit", layout2, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("cell with neither field nor spacer should be LAYOUT_INVALID")
	}
}

func TestValidateLayout_Rule6_FloorRule(t *testing.T) {
	fields := []metadata.FieldDefinition{makeField("name", "text", true)}
	layout := map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{
				"columns": float64(1),
				"fields":  []any{map[string]any{"field": "name", "required": false}},
			},
		},
	}
	if err := validateLayout("edit", layout, fields); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("required:false on a sys-required field should be LAYOUT_INVALID")
	}
}

func TestValidateLayout_Rule7_RequiredOmitted(t *testing.T) {
	fields := []metadata.FieldDefinition{
		makeField("name", "text", true),
		makeField("optional", "text", false),
	}
	// edit without required field "name"
	layout := validEditLayout("optional")
	if err := validateLayout("edit", layout, fields); codeOf(err) != "REQUIRED_FIELD_OMITTED" {
		t.Errorf("missing required field should be REQUIRED_FIELD_OMITTED, got code=%q (err=%v)", codeOf(err), err)
	}
	// detail: rule does NOT apply
	if err := validateLayout("detail", layout, fields); codeOf(err) != "" {
		t.Errorf("detail should not enforce required-omitted, got code=%q", codeOf(err))
	}
}

func TestValidateLayout_Rule8_DuplicateField(t *testing.T) {
	// duplicate in same section
	layout := map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{"columns": float64(1), "fields": []any{"a", "a"}},
		},
	}
	if err := validateLayout("edit", layout, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("duplicate field in section should be LAYOUT_INVALID")
	}

	// duplicate in list columns
	layout2 := map[string]any{
		"version":     float64(1),
		"columns":     []any{map[string]any{"field": "a"}, map[string]any{"field": "a"}},
		"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
	}
	if err := validateLayout("list", layout2, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("duplicate field in list columns should be LAYOUT_INVALID")
	}
}

func TestValidateLayout_Rule9_ColspanBeyondColumns(t *testing.T) {
	layout := map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{
				"columns": float64(2),
				"fields":  []any{map[string]any{"field": "a", "colspan": float64(3)}},
			},
		},
	}
	if err := validateLayout("edit", layout, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("colspan > columns should be LAYOUT_INVALID")
	}
}

func TestValidateLayout_Rule10_DefaultSortField(t *testing.T) {
	layout := map[string]any{
		"version":     float64(1),
		"columns":     []any{map[string]any{"field": "name"}},
		"defaultSort": map[string]any{"field": "name", "order": "desc"},
	}
	if err := validateLayout("list", layout, liveFieldsOf("name")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("defaultSort.field=name should be LAYOUT_INVALID")
	}
}

func TestValidateLayout_Rule11_WidthBounds(t *testing.T) {
	cases := []struct {
		width    any
		wantCode string
	}{
		{float64(100), ""},
		{float64(2000), ""},
		{float64(0), "LAYOUT_INVALID"},
		{float64(2001), "LAYOUT_INVALID"},
		{"43%", "LAYOUT_INVALID"},
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			layout := map[string]any{
				"version":     float64(1),
				"columns":     []any{map[string]any{"field": "a", "width": tc.width}},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}
			err := validateLayout("list", layout, liveFieldsOf("a"))
			if codeOf(err) != tc.wantCode {
				t.Errorf("width=%v: code=%q; want %q (err=%v)", tc.width, codeOf(err), tc.wantCode, err)
			}
		})
	}
}

func TestValidateLayout_Rule12_TypeOverrides(t *testing.T) {
	// labelOverride must be string
	layout := map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{
				"columns": float64(1),
				"fields":  []any{map[string]any{"field": "a", "labelOverride": 123}},
			},
		},
	}
	if err := validateLayout("edit", layout, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("labelOverride non-string should be LAYOUT_INVALID")
	}

	// required must be bool
	layout2 := map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{
				"columns": float64(1),
				"fields":  []any{map[string]any{"field": "a", "required": "yes"}},
			},
		},
	}
	if err := validateLayout("edit", layout2, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("required non-bool should be LAYOUT_INVALID")
	}
}

func TestValidateLayout_Rule13_UnknownKey(t *testing.T) {
	// unknown top-level key
	layout := map[string]any{
		"version":  float64(1),
		"sections": []any{map[string]any{"columns": float64(1), "fields": []any{"a"}}},
		"foo":      "bar",
	}
	if err := validateLayout("edit", layout, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("unknown top-level key should be LAYOUT_INVALID")
	}

	// unknown key in section
	layout2 := map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{"columns": float64(1), "fields": []any{"a"}, "unknown": true},
		},
	}
	if err := validateLayout("edit", layout2, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("unknown section key should be LAYOUT_INVALID")
	}

	// unknown key in cell object
	layout3 := map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{
				"columns": float64(1),
				"fields":  []any{map[string]any{"field": "a", "extra": 1}},
			},
		},
	}
	if err := validateLayout("edit", layout3, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("unknown cell key should be LAYOUT_INVALID")
	}
}

func TestValidateLayout_Rule14_PayloadBounds(t *testing.T) {
	// >50 sections
	sections := make([]any, 51)
	for i := range sections {
		sections[i] = map[string]any{"columns": float64(1), "fields": []any{"a"}}
	}
	if err := validateLayout("edit", map[string]any{"version": float64(1), "sections": sections}, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error(">50 sections should be LAYOUT_INVALID")
	}

	// >200 cells total
	cells := make([]any, 201)
	for i := range cells {
		cells[i] = "a"
	}
	layout2 := map[string]any{
		"version":  float64(1),
		"sections": []any{map[string]any{"columns": float64(1), "fields": cells}},
	}
	if err := validateLayout("edit", layout2, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error(">200 cells should be LAYOUT_INVALID")
	}

	// >100 list columns
	cols := make([]any, 101)
	for i := range cols {
		cols[i] = map[string]any{"field": "a"}
	}
	layout3 := map[string]any{
		"version":     float64(1),
		"columns":     cols,
		"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
	}
	if err := validateLayout("list", layout3, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error(">100 list columns should be LAYOUT_INVALID")
	}
}

func TestValidateLayout_Rule15_EmptySection(t *testing.T) {
	// Section with only spacers
	layout := map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{
				"columns": float64(1),
				"fields":  []any{map[string]any{"spacer": true}},
			},
		},
	}
	if err := validateLayout("edit", layout, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("section with only spacers should be LAYOUT_INVALID")
	}

	// Totally empty fields array
	layout2 := map[string]any{
		"version": float64(1),
		"sections": []any{
			map[string]any{"columns": float64(1), "fields": []any{}},
		},
	}
	if err := validateLayout("edit", layout2, liveFieldsOf("a")); codeOf(err) != "LAYOUT_INVALID" {
		t.Error("section with empty fields should be LAYOUT_INVALID")
	}
}

func TestValidateLayout_RoundTrip(t *testing.T) {
	// Default layout produced by generateDefault must pass validateLayout without error.
	// This is the round-trip invariant: GET-default → POST verbatim → 200.
	tests := []struct {
		name   string
		fields []metadata.FieldDefinition
	}{
		{
			name: "normal entity",
			fields: []metadata.FieldDefinition{
				makeField("amount", "number", false),
				makeField("description", "rich_text", false),
				makeField("status", "text", false),
				makeField("title", "text", true),
			},
		},
		{
			name:   "zero user fields (id-fallback path)",
			fields: nil,
		},
		{
			name: "heavy-only (rich_text) — list uses id fallback",
			fields: []metadata.FieldDefinition{
				makeField("body", "rich_text", false),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, vt := range []string{"edit", "detail", "list"} {
				t.Run(vt, func(t *testing.T) {
					layout := generateDefault("test", vt, tc.fields)
					userFields := filterUserFields(tc.fields)

					// JSON round-trip to convert int→float64 (matching production decode)
					roundTripped := jsonRoundTrip(t, layout)

					if err := validateLayout(vt, roundTripped, userFields); err != nil {
						t.Errorf("round-trip failed: %v", err)
					}
				})
			}
		})
	}
}

// MỘT XƯỞNG (2026-07-29, spec "màn hình đẹp sẵn"): a real business entity used to render as ONE
// flat "Thông tin" wall, so every app Nexus generated looked identical. Entities big enough to
// benefit are now grouped by field semantics; small ones keep the original single section.
func TestGenerateDefault_GroupsBigEntity(t *testing.T) {
	fields := []metadata.FieldDefinition{
		makeField("name", "text", false),
		makeField("stage", "picklist", false),
		makeField("amount", "currency", false),
		makeField("probability", "number", false),
		makeField("close_date", "date", false),
		makeField("account", "relation", false),
		makeField("notes", "rich_text", false),
	}
	layout := generateDefault("test", "edit", fields)
	sections := layout["sections"].([]any)

	titles := make([]string, 0, len(sections))
	seen := map[string]bool{}
	for _, raw := range sections {
		sec := raw.(map[string]any)
		titles = append(titles, sec["title"].(string))
		for _, c := range sec["fields"].([]any) {
			seen[cellFieldName(c)] = true
		}
	}

	want := []string{"Thông tin chung", "Số liệu", "Thời gian", "Liên kết", "Mô tả & tệp"}
	if len(titles) != len(want) {
		t.Fatalf("sections = %v; want %v", titles, want)
	}
	for i, w := range want {
		if titles[i] != w {
			t.Errorf("sections[%d] = %q; want %q", i, titles[i], w)
		}
	}
	// No field may be dropped — silently swallowing a field is the bug class this campaign kills.
	for _, f := range fields {
		if !seen[f.Name] {
			t.Errorf("field %q disappeared from the generated layout", f.Name)
		}
	}
}

// Below the threshold the output must stay EXACTLY as before — fragmenting a 3-field table into
// three headed groups is worse, not better.
func TestGenerateDefault_SmallEntityStaysSingleSection(t *testing.T) {
	fields := []metadata.FieldDefinition{
		makeField("name", "text", false),
		makeField("amount", "currency", false),
		makeField("due", "date", false),
	}
	sections := generateDefault("test", "edit", fields)["sections"].([]any)
	if len(sections) != 1 {
		t.Fatalf("small entity produced %d sections; want 1", len(sections))
	}
	if got := sections[0].(map[string]any)["title"]; got != "Thông tin" {
		t.Errorf("title = %v; want \"Thông tin\"", got)
	}
}
