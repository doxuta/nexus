package services

// specs/054-layout-visible-when: a layout cell's visibleWhen — config-time (FR-1..FR-6), the shared
// evaluator (FR-7, golden cases shared with the FE), the create path (FR-8) and serve-time (FR-9).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

const vwCode = "LAYOUT_VISIBLE_WHEN_INVALID"

// vwFields is the Lead-like fixture every config case validates against.
func vwFields(t *testing.T) []metadata.FieldDefinition {
	t.Helper()
	return []metadata.FieldDefinition{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "customer_type", FieldType: "picklist", Options: jsonOptsPtr(t, map[string]any{"values": []any{"individual", "business"}})},
		{Name: "tax_code", FieldType: "text"},
		{Name: "vip", FieldType: "boolean"},
		{Name: "score", FieldType: "number"},
		{Name: "stars", FieldType: "rating"},
		{Name: "email", FieldType: "email"},
		{Name: "note", FieldType: "text"},
		{Name: "birthday", FieldType: "date"},
		{Name: "tags", FieldType: "multi_select", Options: jsonOptsPtr(t, map[string]any{"values": []any{"a", "b"}})},
		{Name: "code", FieldType: "text", Options: jsonOptsPtr(t, map[string]any{"maxLength": float64(3)})},
	}
}

// vwLayout is a one-section layout listing every fixture field; cells overrides individual cells
// (by field name) with an object cell.
func vwLayout(cells map[string]map[string]any) map[string]any {
	names := []string{"name", "customer_type", "tax_code", "vip", "score", "stars", "email", "note", "birthday", "tags", "code"}
	out := make([]any, 0, len(names))
	for _, n := range names {
		if c, ok := cells[n]; ok {
			if c == nil {
				continue // nil = leave the field off the layout
			}
			c["field"] = n
			out = append(out, c)
			continue
		}
		out = append(out, n)
	}
	return map[string]any{
		"version":  float64(1),
		"sections": []any{map[string]any{"columns": float64(2), "fields": out}},
	}
}

func vw(field, op string, value ...any) map[string]any {
	c := map[string]any{"field": field, "op": op}
	if len(value) == 1 {
		c["value"] = value[0]
	}
	return c
}

func TestVisibleWhen_Config(t *testing.T) {
	cell := func(cond any) map[string]any { return map[string]any{"visibleWhen": cond} }
	tests := []struct {
		name  string
		cells map[string]map[string]any
		view  string // default edit
		want  string
	}{
		{"eq on a picklist value", map[string]map[string]any{"tax_code": cell(vw("customer_type", "eq", "business"))}, "", ""},
		{"detail view accepts it too", map[string]map[string]any{"tax_code": cell(vw("customer_type", "eq", "business"))}, "detail", ""},
		{"neq", map[string]map[string]any{"tax_code": cell(vw("customer_type", "neq", "individual"))}, "", ""},
		{"in", map[string]map[string]any{"tax_code": cell(vw("customer_type", "in", []any{"business"}))}, "", ""},
		{"notIn", map[string]map[string]any{"tax_code": cell(vw("customer_type", "notIn", []any{"individual"}))}, "", ""},
		{"empty", map[string]map[string]any{"tax_code": cell(vw("note", "empty"))}, "", ""},
		{"notEmpty", map[string]map[string]any{"tax_code": cell(vw("email", "notEmpty"))}, "", ""},
		{"boolean eq true", map[string]map[string]any{"tax_code": cell(vw("vip", "eq", true))}, "", ""},
		{"number eq", map[string]map[string]any{"tax_code": cell(vw("score", "eq", float64(5)))}, "", ""},
		{"rating in", map[string]map[string]any{"tax_code": cell(vw("stars", "in", []any{float64(4), float64(5)}))}, "", ""},
		{"a chain A -> B -> C is fine", map[string]map[string]any{
			"note":     cell(vw("tax_code", "notEmpty")),
			"tax_code": cell(vw("customer_type", "eq", "business")),
		}, "", ""},
		{"alongside the other cell props", map[string]map[string]any{"tax_code": {
			"visibleWhen": vw("customer_type", "eq", "business"), "labelOverride": "MST", "helpText": "10 số", "readonly": false,
		}}, "", ""},

		// FR-6: a required field is never hidden.
		{"target is sys_fields.is_required", map[string]map[string]any{"name": cell(vw("customer_type", "eq", "business"))}, "", vwCode},
		{"target cell says required:true", map[string]map[string]any{"tax_code": {
			"visibleWhen": vw("customer_type", "eq", "business"), "required": true,
		}}, "", vwCode},
		// FR-3
		{"points at itself", map[string]map[string]any{"tax_code": cell(vw("tax_code", "notEmpty"))}, "", vwCode},
		{"controller not on this layout", map[string]map[string]any{
			"tax_code": cell(vw("customer_type", "eq", "business")), "customer_type": nil,
		}, "", vwCode},
		{"controller does not exist", map[string]map[string]any{"tax_code": cell(vw("nope", "eq", "x"))}, "", vwCode},
		{"controller is the system id", map[string]map[string]any{"tax_code": cell(vw("id", "notEmpty"))}, "", vwCode},
		{"controller type not supported (date)", map[string]map[string]any{"tax_code": cell(vw("birthday", "notEmpty"))}, "", vwCode},
		{"controller type not supported (multi_select)", map[string]map[string]any{"tax_code": cell(vw("tags", "eq", "a"))}, "", vwCode},
		// FR-2
		{"not an object", map[string]map[string]any{"tax_code": cell("customer_type=business")}, "", vwCode},
		{"unknown key", map[string]map[string]any{"tax_code": cell(map[string]any{"field": "customer_type", "op": "eq", "value": "business", "or": true})}, "", vwCode},
		{"field not a string", map[string]map[string]any{"tax_code": cell(map[string]any{"field": 3, "op": "notEmpty"})}, "", vwCode},
		{"unknown op", map[string]map[string]any{"tax_code": cell(vw("customer_type", "contains", "bus"))}, "", vwCode},
		{"eq without value", map[string]map[string]any{"tax_code": cell(vw("customer_type", "eq"))}, "", vwCode},
		{"eq with null", map[string]map[string]any{"tax_code": cell(vw("customer_type", "eq", nil))}, "", vwCode},
		{"eq with an empty string", map[string]map[string]any{"tax_code": cell(vw("note", "eq", ""))}, "", vwCode},
		{"empty with a value", map[string]map[string]any{"tax_code": cell(vw("note", "empty", "x"))}, "", vwCode},
		{"in with a scalar", map[string]map[string]any{"tax_code": cell(vw("customer_type", "in", "business"))}, "", vwCode},
		{"in with an empty list", map[string]map[string]any{"tax_code": cell(vw("customer_type", "in", []any{}))}, "", vwCode},
		{"eq with a list", map[string]map[string]any{"tax_code": cell(vw("customer_type", "eq", []any{"business"}))}, "", vwCode},
		// FR-4: the operand must be a value the controller could hold.
		{"picklist value not among values (typo)", map[string]map[string]any{"tax_code": cell(vw("customer_type", "eq", "Business"))}, "", vwCode},
		{"in with one bad member", map[string]map[string]any{"tax_code": cell(vw("customer_type", "in", []any{"business", "company"}))}, "", vwCode},
		{"number compared to a string", map[string]map[string]any{"tax_code": cell(vw("score", "eq", "5"))}, "", vwCode},
		{"boolean compared to a string", map[string]map[string]any{"tax_code": cell(vw("vip", "eq", "true"))}, "", vwCode},
		{"text operand breaks the field's own maxLength", map[string]map[string]any{"tax_code": cell(vw("code", "eq", "ABCD"))}, "", vwCode},
		// FR-5
		{"two cells controlling each other", map[string]map[string]any{
			"tax_code": cell(vw("note", "notEmpty")),
			"note":     cell(vw("tax_code", "notEmpty")),
		}, "", vwCode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			view := tc.view
			if view == "" {
				view = "edit"
			}
			layout := jsonRoundTrip(t, vwLayout(tc.cells))
			err := ValidateLayoutConfig(view, layout, vwFields(t))
			if got := codeOf(err); got != tc.want {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tc.want)
			}
		})
	}
}

// A list column has no visibleWhen: the key stays an unknown column key.
func TestVisibleWhen_ListColumnStaysUnknownKey(t *testing.T) {
	layout := validListLayout("tax_code")
	layout["columns"].([]any)[0].(map[string]any)["visibleWhen"] = vw("customer_type", "eq", "business")
	if err := ValidateLayoutConfig("list", jsonRoundTrip(t, layout), vwFields(t)); codeOf(err) != "LAYOUT_INVALID" {
		t.Fatalf("code = %q (err=%v); want LAYOUT_INVALID", codeOf(err), err)
	}
}

type vwGoldenFile struct {
	Cases []struct {
		Name       string                    `json:"name"`
		Conditions map[string]map[string]any `json:"conditions"`
		Values     map[string]any            `json:"values"`
		Visible    map[string]bool           `json:"visible"`
	} `json:"cases"`
	CreateCases []struct {
		Name   string `json:"name"`
		Fields []struct {
			Name         string         `json:"name"`
			FieldType    string         `json:"fieldType"`
			IsRequired   bool           `json:"isRequired"`
			DefaultValue string         `json:"defaultValue"`
			Options      map[string]any `json:"options"`
		} `json:"fields"`
		Conditions map[string]map[string]any `json:"conditions"`
		Payload    map[string]any            `json:"payload"`
		Visible    map[string]bool           `json:"visible"`
	} `json:"createCases"`
}

func vwGoldenConds(t *testing.T, raw map[string]map[string]any) map[string]visibleWhen {
	t.Helper()
	conds := make(map[string]visibleWhen, len(raw))
	for target, rawCond := range raw {
		vc, ok := parseVisibleWhen(rawCond)
		if !ok {
			t.Fatalf("condition on %q did not parse: %v", target, rawCond)
		}
		conds[target] = vc
	}
	return conds
}

// TestVisibleWhen_GoldenCases runs the FE/BE parity file (FR-7). The FE port runs the same file.
func TestVisibleWhen_GoldenCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/visible_when_cases.json")
	if err != nil {
		t.Fatalf("read golden cases: %v", err)
	}
	var file vwGoldenFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode golden cases: %v", err)
	}
	if len(file.Cases) < 20 {
		t.Fatalf("only %d golden cases decoded — the file lost its cases", len(file.Cases))
	}
	for _, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			conds := vwGoldenConds(t, c.Conditions)
			for field, want := range c.Visible {
				if got := fieldVisible(field, conds, c.Values); got != want {
					t.Errorf("fieldVisible(%q) = %v; want %v", field, got, want)
				}
			}
		})
	}
	// FR-7 "one image": at create time the condition is judged on the image the server stores
	// (payload + defaults), which is what the FE must judge on too — not on what its widgets show.
	if len(file.CreateCases) < 8 {
		t.Fatalf("only %d golden createCases decoded — the file lost its create-image cases", len(file.CreateCases))
	}
	for _, c := range file.CreateCases {
		t.Run("create image: "+c.Name, func(t *testing.T) {
			specs := make([]FieldSpec, len(c.Fields))
			for i, f := range c.Fields {
				specs[i] = FieldSpec{Name: f.Name, FieldType: f.FieldType, IsRequired: f.IsRequired, DefaultVal: f.DefaultValue, Options: f.Options}
			}
			conds := vwGoldenConds(t, c.Conditions)
			image, err := createImage(specs, c.Payload, conds)
			if err != nil {
				t.Fatalf("createImage: %v", err)
			}
			for field, want := range c.Visible {
				if got := fieldVisible(field, conds, image); got != want {
					t.Errorf("fieldVisible(%q) on the create image %v = %v; want %v", field, image, got, want)
				}
			}
		})
	}
}

// --- FR-8: the create path ---

func vwSpecs() []FieldSpec {
	return []FieldSpec{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "customer_type", FieldType: "picklist", Options: map[string]any{"values": []any{"individual", "business"}}},
		{Name: "tax_code", FieldType: "text", IsRequired: true},
		{Name: "score", FieldType: "number"},
	}
}

func TestVisibleWhen_ValidateRecordWithVisibility(t *testing.T) {
	conds := map[string]visibleWhen{"tax_code": {Field: "customer_type", Op: "eq", Value: "business"}}
	tests := []struct {
		name      string
		specs     []FieldSpec
		payload   map[string]any
		want      string
		wantField string
	}{
		{"hidden required field absent: accepted", vwSpecs(), map[string]any{"name": "An", "customer_type": "individual"}, "", ""},
		{"controller absent: condition false, field hidden", vwSpecs(), map[string]any{"name": "An"}, "", ""},
		{"visible required field absent: still demanded", vwSpecs(),
			map[string]any{"name": "An", "customer_type": "business"}, "RECORD_REQUIRED_MISSING", "tax_code"},
		{"visible required field blank: still demanded", vwSpecs(),
			map[string]any{"name": "An", "customer_type": "business", "tax_code": "  "}, "RECORD_REQUIRED_MISSING", "tax_code"},
		{"visible required field present: accepted", vwSpecs(),
			map[string]any{"name": "An", "customer_type": "business", "tax_code": "0101"}, "", ""},
		{"an unconditional required field keeps its rule", vwSpecs(),
			map[string]any{"customer_type": "individual"}, "RECORD_REQUIRED_MISSING", "name"},
		{"the controller's default counts (create image)", func() []FieldSpec {
			s := vwSpecs()
			s[1].DefaultVal = "business"
			return s
		}(), map[string]any{"name": "An"}, "RECORD_REQUIRED_MISSING", "tax_code"},
		{"other errors pass through", vwSpecs(),
			map[string]any{"name": "An", "customer_type": "individual", "score": "x"}, "RECORD_INVALID_TYPE", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clean, err := validateRecordWithVisibility(tc.specs, tc.payload, conds)
			if got := codeOf(err); got != tc.want {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tc.want)
			}
			if tc.wantField != "" && detailsField(err) != tc.wantField {
				t.Fatalf("details.field = %v; want %q", detailsField(err), tc.wantField)
			}
			if tc.want == "" && clean["name"] != "An" {
				t.Fatalf("clean = %v; want the validated create image", clean)
			}
		})
	}
	// The caller's specs are never mutated (the relaxed copy is private).
	s := vwSpecs()
	_, _ = validateRecordWithVisibility(s, map[string]any{"name": "An"}, conds)
	if !s[2].IsRequired {
		t.Fatal("validateRecordWithVisibility cleared IsRequired on the caller's slice")
	}
}

// vwLayoutReader serves one stored edit layout and counts reads.
type vwLayoutReader struct {
	json  string
	err   error
	reads int
}

func (r *vwLayoutReader) GetByEntity(_ context.Context, _, _ string) ([]metadata.LayoutDefinition, error) {
	r.reads++
	if r.err != nil {
		return nil, r.err
	}
	return []metadata.LayoutDefinition{
		{ViewType: "list", LayoutJSON: `{"version":1,"columns":[{"field":"name"}],"defaultSort":{"field":"created_at","order":"desc"}}`},
		{ViewType: "edit", LayoutJSON: r.json},
	}, nil
}

const vwEditLayoutJSON = `{"version":1,"sections":[{"columns":2,"fields":["name","customer_type",
	{"field":"tax_code","visibleWhen":{"field":"customer_type","op":"eq","value":"business"}},"score"]}]}`

func vwRecordService(t *testing.T, layouts *vwLayoutReader) *RecordService {
	t.Helper()
	cat := &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-lead", TenantID: "tenant-1", Name: "leads", DataTable: "obj_leads"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "customer_type", FieldType: "picklist", Options: jsonOptsPtr(t, map[string]any{"values": []any{"individual", "business"}})},
			{Name: "tax_code", FieldType: "text", IsRequired: true},
			{Name: "score", FieldType: "number"},
		},
	}
	svc := NewRecordService(cat, &fakeRecordStore{})
	if layouts != nil {
		svc = svc.WithLayoutReader(layouts)
	}
	return svc
}

func TestVisibleWhen_Create(t *testing.T) {
	nonAdmin := RecordScope{UserID: "u-1", RoleID: "r-sales", AccessLevel: "all"}
	ctx := context.Background()

	t.Run("hidden required field is not demanded", func(t *testing.T) {
		layouts := &vwLayoutReader{json: vwEditLayoutJSON}
		_, err := vwRecordService(t, layouts).Create(ctx, nonAdmin, "tenant-1", "u-1", "leads",
			map[string]any{"name": "An", "customer_type": "individual"})
		if err != nil {
			t.Fatalf("create with the hidden required field absent: %v; want accepted", err)
		}
	})
	t.Run("visible required field is demanded", func(t *testing.T) {
		layouts := &vwLayoutReader{json: vwEditLayoutJSON}
		_, err := vwRecordService(t, layouts).Create(ctx, nonAdmin, "tenant-1", "u-1", "leads",
			map[string]any{"name": "An", "customer_type": "business"})
		if codeOf(err) != "RECORD_REQUIRED_MISSING" || detailsField(err) != "tax_code" {
			t.Fatalf("err = %v; want RECORD_REQUIRED_MISSING details.field=tax_code", err)
		}
	})
	t.Run("the happy path never reads the layout", func(t *testing.T) {
		layouts := &vwLayoutReader{json: vwEditLayoutJSON}
		if _, err := vwRecordService(t, layouts).Create(ctx, nonAdmin, "tenant-1", "u-1", "leads",
			map[string]any{"name": "An", "customer_type": "business", "tax_code": "0101"}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if layouts.reads != 0 {
			t.Fatalf("layout read %d times on a create that passed ValidateRecord; want 0", layouts.reads)
		}
	})
	t.Run("layout read failure keeps the 422 (fail closed)", func(t *testing.T) {
		layouts := &vwLayoutReader{err: errors.New("db down")}
		_, err := vwRecordService(t, layouts).Create(ctx, nonAdmin, "tenant-1", "u-1", "leads",
			map[string]any{"name": "An", "customer_type": "individual"})
		if codeOf(err) != "RECORD_REQUIRED_MISSING" {
			t.Fatalf("err = %v; want RECORD_REQUIRED_MISSING kept", err)
		}
	})
	t.Run("no layout reader wired: unchanged behaviour", func(t *testing.T) {
		_, err := vwRecordService(t, nil).Create(ctx, nonAdmin, "tenant-1", "u-1", "leads",
			map[string]any{"name": "An", "customer_type": "individual"})
		if codeOf(err) != "RECORD_REQUIRED_MISSING" {
			t.Fatalf("err = %v; want RECORD_REQUIRED_MISSING", err)
		}
	})
	t.Run("a condition on a deleted controller is ignored", func(t *testing.T) {
		layouts := &vwLayoutReader{json: strings.ReplaceAll(vwEditLayoutJSON, `"field":"customer_type","op"`, `"field":"gone","op"`)}
		_, err := vwRecordService(t, layouts).Create(ctx, nonAdmin, "tenant-1", "u-1", "leads",
			map[string]any{"name": "An", "customer_type": "individual"})
		if codeOf(err) != "RECORD_REQUIRED_MISSING" {
			t.Fatalf("err = %v; want RECORD_REQUIRED_MISSING (the field is unconditional once its controller is gone)", err)
		}
	})
}

func TestVisibleWhen_CreateBulk(t *testing.T) {
	layouts := &vwLayoutReader{json: vwEditLayoutJSON}
	svc := vwRecordService(t, layouts)
	res, err := svc.CreateBulk(context.Background(), RecordScope{UserID: "u-1", AccessLevel: "all"}, "tenant-1", "u-1", "leads",
		[]map[string]any{
			{"name": "An", "customer_type": "individual"},
			{"name": "Bình", "customer_type": "business"},
			{"name": "Chi", "customer_type": "individual"},
		}, BulkModePartial)
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	got := make([]string, len(res.Results))
	for i, r := range res.Results {
		got[i] = r.Status + ":" + r.Code
	}
	want := []string{"created:", "failed:RECORD_REQUIRED_MISSING", "created:"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("results = %v; want %v", got, want)
	}
}

// --- FR-9: serve-time ---

func TestVisibleWhen_ServeTimeDropsUnservedController(t *testing.T) {
	decode := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return m
	}
	in := `{"version":1,"sections":[{"columns":2,"fields":["name","customer_type",
		{"field":"tax_code","labelOverride":"MST","visibleWhen":{"field":"customer_type","op":"eq","value":"business"}},
		{"field":"note","visibleWhen":{"field":"tax_code","op":"notEmpty"}}]}]}`

	// ACL: customer_type hidden from the caller — its cell goes, and tax_code's condition goes with
	// it (no hidden name served, no inference of its value). note keeps its condition on tax_code.
	got, _ := json.Marshal(StripHiddenLayoutFields(decode(in), LayoutViewEdit, map[string]struct{}{"customer_type": {}}))
	want, _ := json.Marshal(decode(`{"version":1,"sections":[{"columns":2,"fields":["name",
		{"field":"tax_code","labelOverride":"MST"},
		{"field":"note","visibleWhen":{"field":"tax_code","op":"notEmpty"}}]}]}`))
	if string(got) != string(want) {
		t.Fatalf("ACL strip:\ngot  %s\nwant %s", got, want)
	}

	// Dead controller (soft-deleted field): same treatment through the serve-time tolerance.
	live := []metadata.FieldDefinition{
		{Name: "name", FieldType: "text"}, {Name: "tax_code", FieldType: "text"}, {Name: "note", FieldType: "text"},
	}
	pruned, err := applyServeTimeTolerance(in, LayoutViewEdit, live)
	if err != nil {
		t.Fatalf("tolerance: %v", err)
	}
	got, _ = json.Marshal(pruned)
	if string(got) != string(want) {
		t.Fatalf("dead controller:\ngot  %s\nwant %s", got, want)
	}

	// The stored layout is never mutated.
	orig := decode(in)
	_ = StripHiddenLayoutFields(orig, LayoutViewEdit, map[string]struct{}{"customer_type": {}})
	cells := orig["sections"].([]any)[0].(map[string]any)["fields"].([]any)
	if _, ok := cells[2].(map[string]any)["visibleWhen"]; !ok {
		t.Fatal("StripHiddenLayoutFields mutated the input cell")
	}
}
