package services

// specs/052-dependent-picklist — a picklist whose allowed values depend on a sibling picklist
// (Tỉnh → Quận/Huyện). Config lives on the CHILD field's options:
//
//	controllingField: "province"
//	dependentValues:  {"Hà Nội": ["Ba Đình", …], "Đà Nẵng": ["Hải Châu", …]}
//
// Config-time: FIELD_DEPENDENT_PICKLIST_INVALID. Record-time: RECORD_DEPENDENT_PICKLIST_INVALID
// with details.field = the child, judged on the pair the write SENDS (a resend of the stored pair
// is not an edit), never on the pair that merely sits in storage.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"nexus/platform/internal/metadata"
)

const depCode = "FIELD_DEPENDENT_PICKLIST_INVALID"
const depRecCode = "RECORD_DEPENDENT_PICKLIST_INVALID"

func provinceOpts() map[string]any {
	return map[string]any{"values": []any{"Hà Nội", "Đà Nẵng", "Huế"}}
}

// districtOpts is the child config. "Huế" deliberately has no entry: a parent value the map does
// not mention allows no child at all.
func districtOpts() map[string]any {
	return map[string]any{
		"values":           []any{"Ba Đình", "Hoàn Kiếm", "Hải Châu", "Sơn Trà"},
		"controllingField": "province",
		"dependentValues": map[string]any{
			"Hà Nội":  []any{"Ba Đình", "Hoàn Kiếm"},
			"Đà Nẵng": []any{"Hải Châu", "Sơn Trà"},
		},
	}
}

func withOpt(base map[string]any, key string, val any) map[string]any {
	out := make(map[string]any, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	if val == nil {
		delete(out, key)
	} else {
		out[key] = val
	}
	return out
}

// --- config-time, structural (single field — validateFieldConfig) ---

func TestDependentPicklist_FieldConfig(t *testing.T) {
	tests := []struct {
		name      string
		fieldType string
		opts      map[string]any
		want      string // "" = accepted
	}{
		{"plain picklist without the keys is untouched", "picklist", provinceOpts(), ""},
		{"valid child config", "picklist", districtOpts(), ""},
		{"a parent with an empty child list is allowed", "picklist",
			withOpt(districtOpts(), "dependentValues", map[string]any{"Hà Nội": []any{}}), ""},
		{"controllingField without dependentValues", "picklist", withOpt(districtOpts(), "dependentValues", nil), depCode},
		{"dependentValues without controllingField", "picklist", withOpt(districtOpts(), "controllingField", nil), depCode},
		{"controllingField not a string", "picklist", withOpt(districtOpts(), "controllingField", 7.0), depCode},
		{"controllingField blank", "picklist", withOpt(districtOpts(), "controllingField", "  "), depCode},
		{"controllingField is the field itself", "picklist", withOpt(districtOpts(), "controllingField", "district"), depCode},
		{"dependentValues not an object", "picklist", withOpt(districtOpts(), "dependentValues", []any{"Hà Nội"}), depCode},
		{"dependentValues empty object", "picklist", withOpt(districtOpts(), "dependentValues", map[string]any{}), depCode},
		{"dependentValues entry not an array", "picklist",
			withOpt(districtOpts(), "dependentValues", map[string]any{"Hà Nội": "Ba Đình"}), depCode},
		{"dependentValues entry holds a non-string", "picklist",
			withOpt(districtOpts(), "dependentValues", map[string]any{"Hà Nội": []any{1.0}}), depCode},
		{"dependentValues blank parent key", "picklist",
			withOpt(districtOpts(), "dependentValues", map[string]any{" ": []any{"Ba Đình"}}), depCode},
		{"child value typo not in own values", "picklist",
			withOpt(districtOpts(), "dependentValues", map[string]any{"Đà Nẵng": []any{"Hai Chau"}}), depCode},
		{"multi_select cannot be dependent", "multi_select", districtOpts(), depCode},
		{"text cannot carry dependentValues", "text",
			map[string]any{"dependentValues": map[string]any{"a": []any{"b"}}}, depCode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateField(FieldInput{Name: "district", Label: "Quận", FieldType: tc.fieldType, Options: tc.opts})
			if got := codeOf(err); got != tc.want {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tc.want)
			}
		})
	}
}

// A composite sub-field would carry the keys with nobody reading them at record time.
func TestDependentPicklist_CompositeSubFieldRejected(t *testing.T) {
	err := validateField(FieldInput{Name: "address", Label: "Địa chỉ", FieldType: "composite", Options: map[string]any{
		"subFields": []any{
			map[string]any{"name": "province", "fieldType": "picklist", "options": provinceOpts()},
			map[string]any{"name": "district", "fieldType": "picklist", "options": districtOpts()},
		},
	}})
	if got := codeOf(err); got != "FIELD_COMPOSITE_SUBFIELD_INVALID" {
		t.Fatalf("code = %q (err=%v); want FIELD_COMPOSITE_SUBFIELD_INVALID", got, err)
	}
}

// --- config-time, cross-field (entity-create carries every sibling) ---

func depEntity(fields ...FieldInput) CreateEntityInput {
	return CreateEntityInput{Name: "contacts", Label: "Liên hệ", Fields: append([]FieldInput{
		{Name: "full_name", Label: "Tên", FieldType: "text"},
	}, fields...)}
}

func TestDependentPicklist_CreateEntityCrossField(t *testing.T) {
	province := FieldInput{Name: "province", Label: "Tỉnh", FieldType: "picklist", Options: provinceOpts()}
	district := FieldInput{Name: "district", Label: "Quận", FieldType: "picklist", Options: districtOpts()}
	ward := FieldInput{Name: "ward", Label: "Phường", FieldType: "picklist", Options: map[string]any{
		"values":           []any{"Phúc Xá", "Hàng Bạc"},
		"controllingField": "district",
		"dependentValues":  map[string]any{"Ba Đình": []any{"Phúc Xá"}, "Hoàn Kiếm": []any{"Hàng Bạc"}},
	}}
	tests := []struct {
		name string
		in   CreateEntityInput
		want string
	}{
		{"valid pair", depEntity(province, district), ""},
		{"child declared before its parent", depEntity(district, province), ""},
		{"three-level chain", depEntity(province, district, ward), ""},
		{"parent missing", depEntity(district), depCode},
		{"parent is not a picklist", depEntity(
			FieldInput{Name: "province", Label: "Tỉnh", FieldType: "text"}, district), depCode},
		{"map key not among the parent's values", depEntity(province, FieldInput{
			Name: "district", Label: "Quận", FieldType: "picklist",
			Options: withOpt(districtOpts(), "dependentValues", map[string]any{"Hanoi": []any{"Ba Đình"}}),
		}), depCode},
		{"cycle between two dependents", depEntity(
			FieldInput{Name: "province", Label: "Tỉnh", FieldType: "picklist", Options: map[string]any{
				"values":           []any{"Hà Nội", "Đà Nẵng", "Huế"},
				"controllingField": "district",
				"dependentValues":  map[string]any{"Ba Đình": []any{"Hà Nội"}},
			}}, district), depCode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCreateEntity(tc.in)
			if got := codeOf(err); got != tc.want {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tc.want)
			}
		})
	}
}

// --- config-time, cross-field on AddField (siblings come from storage) ---

func storedPicklist(name, opts string) metadata.FieldDefinition {
	return metadata.FieldDefinition{ID: "f-" + name, EntityID: "ent-1", Name: name, FieldType: "picklist", Options: strPtr(opts)}
}

const provinceJSON = `{"values":["Hà Nội","Đà Nẵng","Huế"]}`
const districtJSON = `{"values":["Ba Đình","Hoàn Kiếm","Hải Châu","Sơn Trà"],"controllingField":"province","dependentValues":{"Hà Nội":["Ba Đình","Hoàn Kiếm"],"Đà Nẵng":["Hải Châu","Sơn Trà"]}}`

func TestDependentPicklist_AddFieldCrossField(t *testing.T) {
	tests := []struct {
		name     string
		existing []metadata.FieldDefinition
		opts     map[string]any
		want     string
	}{
		{"parent exists", []metadata.FieldDefinition{storedPicklist("province", provinceJSON)}, districtOpts(), ""},
		{"parent absent", nil, districtOpts(), depCode},
		{"parent soft-deleted", []metadata.FieldDefinition{{
			ID: "f-province", EntityID: "ent-1", Name: "province", FieldType: "picklist", Options: strPtr(provinceJSON),
			DeletedAt: gorm.DeletedAt{Time: time.Now(), Valid: true},
		}}, districtOpts(), depCode},
		{"parent not a picklist", []metadata.FieldDefinition{{ID: "f-p", EntityID: "ent-1", Name: "province", FieldType: "text"}},
			districtOpts(), depCode},
		{"key not among the stored parent's values", []metadata.FieldDefinition{storedPicklist("province", `{"values":["Hà Nội"]}`)},
			districtOpts(), depCode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			meta := &fakeFieldMeta{entity: entityFixture(), existing: tc.existing}
			svc := NewFieldService(&fakeFieldSchema{}, meta)
			_, err := svc.AddField(context.Background(), "tenant-1", AddFieldInput{
				EntityName: "courses",
				Field:      FieldInput{Name: "district", Label: "Quận", FieldType: "picklist", Options: tc.opts},
			})
			if got := codeOf(err); got != tc.want {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tc.want)
			}
			if tc.want != "" && len(meta.created) != 0 {
				t.Fatalf("a rejected dependent config still persisted %d field(s)", len(meta.created))
			}
		})
	}
}

// --- config-time, cross-field on EditField ---

// depEditMeta is fakeEditMeta with a real sibling list (fakeEditMeta.ListFields errors "unused").
type depEditMeta struct {
	fakeEditMeta
	siblings []metadata.FieldDefinition
	listed   int
}

func (d *depEditMeta) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	d.listed++
	return d.siblings, nil
}

func TestDependentPicklist_EditFieldCrossField(t *testing.T) {
	province := storedPicklist("province", provinceJSON)
	district := storedPicklist("district", districtJSON)
	tests := []struct {
		name     string
		editing  metadata.FieldDefinition
		siblings []metadata.FieldDefinition
		opts     map[string]any
		want     string
	}{
		{"make a plain picklist dependent", storedPicklist("district", `{"values":["Ba Đình","Hoàn Kiếm","Hải Châu","Sơn Trà"]}`),
			[]metadata.FieldDefinition{province}, districtOpts(), ""},
		{"point at a missing parent", district, []metadata.FieldDefinition{district},
			withOpt(districtOpts(), "controllingField", "city"), depCode},
		{"parent edit closing a cycle", province, []metadata.FieldDefinition{province, district},
			map[string]any{
				"values":           []any{"Hà Nội", "Đà Nẵng", "Huế"},
				"controllingField": "district",
				"dependentValues":  map[string]any{"Ba Đình": []any{"Hà Nội"}},
			}, depCode},
		// FR-4: removing a parent value that a child's map still names is NOT blocked (orphan key
		// is inert) — the parent carries no dependent config of its own, so no sibling read at all.
		{"parent drops a value a child maps: tolerated", province, []metadata.FieldDefinition{province, district},
			map[string]any{"values": []any{"Hà Nội", "Huế"}}, ""},
		// The CHILD's own later edits carry the stored map verbatim (the Studio inspector spreads
		// every stored key into options). A carried key the parent has since dropped, or a parent
		// soft-deleted since, must not freeze the child's options (FR-4) — only what this edit
		// CHANGES is judged: a new key, or a re-pointed controllingField.
		{"child edit carries a key the parent has since dropped: tolerated", district,
			[]metadata.FieldDefinition{storedPicklist("province", `{"values":["Hà Nội","Huế"]}`), district},
			withOpt(districtOpts(), "values", []any{"Ba Đình", "Hoàn Kiếm", "Hải Châu", "Sơn Trà", "Cầu Giấy"}), ""},
		{"child edit after the parent was soft-deleted: tolerated", district, []metadata.FieldDefinition{district},
			withOpt(districtOpts(), "indexed", true), ""},
		{"child edit adds a key the parent lacks: rejected", district,
			[]metadata.FieldDefinition{storedPicklist("province", `{"values":["Hà Nội","Huế"]}`), district},
			withOpt(districtOpts(), "dependentValues", map[string]any{
				"Hà Nội":  []any{"Ba Đình"},
				"Đà Nẵng": []any{"Hải Châu"},
				"Cần Thơ": []any{"Sơn Trà"},
			}), depCode},
		{"child re-pointed at another parent: carried keys judged against it", district,
			[]metadata.FieldDefinition{province, storedPicklist("city", `{"values":["Hà Nội"]}`), district},
			withOpt(districtOpts(), "controllingField", "city"), depCode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			meta := &depEditMeta{fakeEditMeta: fakeEditMeta{field: &tc.editing}, siblings: tc.siblings}
			svc := NewFieldService(&fakeFieldSchema{}, meta)
			_, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{FieldID: tc.editing.ID, Options: tc.opts})
			if got := codeOf(err); got != tc.want {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tc.want)
			}
			if tc.want != "" && len(meta.updated) != 0 {
				t.Fatal("a rejected dependent config was still persisted")
			}
			if tc.want == "" && len(meta.updated) != 1 {
				t.Fatalf("an accepted edit persisted %d time(s); want 1", len(meta.updated))
			}
		})
	}
}

// A label-only edit of a dependent field neither re-validates nor reads siblings.
func TestDependentPicklist_EditWithoutOptionsSkipsSiblingRead(t *testing.T) {
	district := storedPicklist("district", districtJSON)
	meta := &depEditMeta{fakeEditMeta: fakeEditMeta{field: &district}}
	svc := NewFieldService(&fakeFieldSchema{}, meta)
	label := "Quận/Huyện"
	if _, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{FieldID: district.ID, Label: &label}); err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if meta.listed != 0 {
		t.Fatalf("label-only edit listed siblings %d time(s); want 0", meta.listed)
	}
}

// --- record-time, create (ValidateRecord) ---

func depSpecs() []FieldSpec {
	return []FieldSpec{
		{Name: "full_name", FieldType: "text"},
		{Name: "level", FieldType: "picklist", Options: map[string]any{"values": []any{"a", "b"}}},
		{Name: "province", FieldType: "picklist", Options: provinceOpts()},
		{Name: "district", FieldType: "picklist", Options: districtOpts()},
	}
}

// detailsField reads details.field — the key the FE highlights (fieldOf reads the Field hint).
func detailsField(err error) any {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Details["field"]
	}
	return nil
}

func TestDependentPicklist_ValidateRecord(t *testing.T) {
	tests := []struct {
		name    string
		specs   []FieldSpec
		payload map[string]any
		want    string
	}{
		{"matching pair", depSpecs(), map[string]any{"province": "Hà Nội", "district": "Ba Đình"}, ""},
		{"mismatched pair", depSpecs(), map[string]any{"province": "Hà Nội", "district": "Hải Châu"}, depRecCode},
		{"child without parent", depSpecs(), map[string]any{"district": "Ba Đình"}, depRecCode},
		{"parent without child", depSpecs(), map[string]any{"province": "Hà Nội"}, ""},
		{"neither", depSpecs(), map[string]any{"full_name": "An"}, ""},
		{"parent value the map does not list allows no child", depSpecs(),
			map[string]any{"province": "Huế", "district": "Ba Đình"}, depRecCode},
		{"plain picklist unaffected", depSpecs(), map[string]any{"level": "b"}, ""},
		// FR-4: the parent field was soft-deleted (absent from the live specs) — the child is flat.
		{"parent field gone: child is flat", depSpecs()[3:], map[string]any{"district": "Hải Châu"}, ""},
		// The flat values check still runs first: a value outside `values` keeps its old code.
		{"value outside values keeps the old code", depSpecs(),
			map[string]any{"province": "Hà Nội", "district": "Cầu Giấy"}, "RECORD_INVALID_PICKLIST"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateRecord(tc.specs, tc.payload)
			if got := codeOf(err); got != tc.want {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tc.want)
			}
			if tc.want == depRecCode && detailsField(err) != "district" {
				t.Fatalf("details.field = %v; want \"district\" (the FE highlights the child)", detailsField(err))
			}
		})
	}
}

// --- record-time, update (RecordService.Update, judged on the pair SENT) ---

const depProvinceJSON = `{"values":["Hà Nội","Đà Nẵng","Huế"]}`

func newDepUpdateService(store *relabelStore) *RecordService {
	cat := &fakeRecordCatalog{
		entity: courseEntityFixture(),
		fields: append(courseFieldDefs(),
			metadata.FieldDefinition{Name: "province", FieldType: "picklist", Options: strPtr(depProvinceJSON)},
			metadata.FieldDefinition{Name: "district", FieldType: "picklist", Options: strPtr(districtJSON)},
		),
	}
	return NewRecordService(cat, store)
}

func TestDependentPicklist_Update(t *testing.T) {
	legacy := map[string]any{"title": "cũ", "province": "Hà Nội", "district": "Hải Châu"} // mismatched, pre-config
	clean := map[string]any{"title": "cũ", "province": "Hà Nội", "district": "Ba Đình"}
	tests := []struct {
		name    string
		current map[string]any // nil = the best-effort read failed
		payload map[string]any
		want    string
	}{
		{"legacy mismatch, edit another field only", legacy, map[string]any{"title": "mới"}, ""},
		{"legacy mismatch, form resends the stored pair", legacy,
			map[string]any{"title": "mới", "province": "Hà Nội", "district": "Hải Châu"}, ""},
		{"legacy mismatch, child changed to another mismatch", legacy,
			map[string]any{"district": "Sơn Trà"}, depRecCode},
		{"legacy mismatch, child fixed", legacy, map[string]any{"district": "Hoàn Kiếm"}, ""},
		{"parent changed, stored child no longer fits", clean, map[string]any{"province": "Đà Nẵng"}, depRecCode},
		{"parent changed and child cleared", clean, map[string]any{"province": "Đà Nẵng", "district": nil}, ""},
		{"child changed within the stored parent", clean, map[string]any{"district": "Hoàn Kiếm"}, ""},
		{"parent cleared while child stays", clean, map[string]any{"province": nil}, depRecCode},
		{"read failed: judged on the payload alone (fail closed)", nil, map[string]any{"district": "Hoàn Kiếm"}, depRecCode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &relabelStore{current: tc.current}
			svc := newDepUpdateService(store)
			err := svc.Update(context.Background(), RecordScope{IsAdmin: true, AccessLevel: "all"}, "tenant-1", "courses", "rec-1", tc.payload, nil)
			if got := codeOf(err); got != tc.want {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tc.want)
			}
			if tc.want != "" {
				if store.updated {
					t.Fatal("a rejected pair still reached the store")
				}
				if detailsField(err) != "district" {
					t.Fatalf("details.field = %v; want \"district\"", detailsField(err))
				}
			} else if !store.updated {
				t.Fatal("an accepted update never reached the store")
			}
		})
	}
}

// A value the write did not send is the STORED one — possibly in a field hidden from the caller's
// role — so the 422 sentence must never read it back.
func TestDependentPicklist_MessageNeverEchoesUnsentValues(t *testing.T) {
	stored := map[string]any{"title": "cũ", "province": "Hà Nội", "district": "Ba Đình"}
	for _, tc := range []struct {
		payload map[string]any
		secret  string
	}{
		{map[string]any{"district": "Sơn Trà"}, "Hà Nội"},  // stored parent
		{map[string]any{"province": "Đà Nẵng"}, "Ba Đình"}, // stored child
	} {
		svc := newDepUpdateService(&relabelStore{current: stored})
		err := svc.Update(context.Background(), RecordScope{IsAdmin: true, AccessLevel: "all"}, "tenant-1", "courses", "rec-1", tc.payload, nil)
		if codeOf(err) != depRecCode {
			t.Fatalf("payload %v: err=%v; want %s", tc.payload, err, depRecCode)
		}
		if strings.Contains(err.Error(), tc.secret) {
			t.Fatalf("payload %v: message %q echoes the unsent stored value %q", tc.payload, err.Error(), tc.secret)
		}
	}
}

// The 422 sentence reaches the customer's face: it names fields by label, never by engine name.
func TestDependentPicklist_MessageUsesLabels(t *testing.T) {
	specs := depSpecs()
	specs[2].Label, specs[3].Label = "Tỉnh/TP", "Quận/Huyện"
	for _, payload := range []map[string]any{
		{"province": "Hà Nội", "district": "Hải Châu"},
		{"district": "Hải Châu"},
	} {
		_, err := ValidateRecord(specs, payload)
		if err == nil {
			t.Fatalf("payload %v accepted; want %s", payload, depRecCode)
		}
		msg := err.Error()
		if !strings.Contains(msg, "Quận/Huyện") || !strings.Contains(msg, "Tỉnh/TP") || strings.Contains(msg, "district") {
			t.Fatalf("message %q; want both labels and no engine field name", msg)
		}
	}
}

// Queue #255: the child default is filled only when the parent in the create image allows it (as
// Salesforce does); otherwise it is skipped and the create succeeds with the child blank.
func TestDependentPicklist_ChildDefaultSkippedWhenParentDisallows(t *testing.T) {
	specs := func(parentDefault string, childRequired bool) []FieldSpec {
		s := depSpecs()
		s[2].DefaultVal = parentDefault
		s[3].DefaultVal = "Ba Đình"
		s[3].IsRequired = childRequired
		return s
	}
	tests := []struct {
		name     string
		specs    []FieldSpec
		payload  map[string]any
		wantCode string
		wantDist any // nil = child absent from the stored image
	}{
		{"parent allows default: filled", specs("", false), map[string]any{"province": "Hà Nội"}, "", "Ba Đình"},
		{"parent disallows default: skipped", specs("", false), map[string]any{"province": "Đà Nẵng"}, "", nil},
		{"parent has no child list: skipped", specs("", false), map[string]any{"province": "Huế"}, "", nil},
		{"parent omitted, no default: skipped", specs("", false), map[string]any{"full_name": "An"}, "", nil},
		{"parent blank: skipped", specs("", false), map[string]any{"province": ""}, "", nil},
		{"parent from its own default allows: filled", specs("Hà Nội", false), map[string]any{"full_name": "An"}, "", "Ba Đình"},
		{"parent from its own default disallows: skipped", specs("Đà Nẵng", false), map[string]any{"full_name": "An"}, "", nil},
		{"child the user picked is still judged", specs("", false), map[string]any{"province": "Đà Nẵng", "district": "Ba Đình"}, depRecCode, nil},
		{"required child whose default was skipped is missing", specs("", true), map[string]any{"province": "Đà Nẵng"}, "RECORD_REQUIRED_MISSING", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateRecord(tc.specs, tc.payload)
			if c := codeOf(err); c != tc.wantCode {
				t.Fatalf("code = %q (err=%v); want %q", c, err, tc.wantCode)
			}
			if tc.wantCode == "" && got["district"] != tc.wantDist {
				t.Fatalf("district = %v; want %v", got["district"], tc.wantDist)
			}
		})
	}
}

// A chain c -> b -> a whose b and a both carry defaults: c='y' does not allow b's default, so b is
// dropped, and then a's default (needs b='b1') must go too — whatever order the fields come in.
func TestDependentPicklist_ChildDefaultSkip_ChainAnyOrder(t *testing.T) {
	c := FieldSpec{Name: "c", FieldType: "picklist", Options: map[string]any{"values": []any{"x", "y"}}}
	b := FieldSpec{Name: "b", FieldType: "picklist", DefaultVal: "b1", Options: map[string]any{
		"values": []any{"b1"}, "controllingField": "c", "dependentValues": map[string]any{"x": []any{"b1"}}}}
	a := FieldSpec{Name: "a", FieldType: "picklist", DefaultVal: "a1", Options: map[string]any{
		"values": []any{"a1"}, "controllingField": "b", "dependentValues": map[string]any{"b1": []any{"a1"}}}}
	for name, order := range map[string][]FieldSpec{"c,b,a": {c, b, a}, "c,a,b": {c, a, b}} {
		t.Run(name, func(t *testing.T) {
			got, err := ValidateRecord(order, map[string]any{"c": "y"})
			if err != nil {
				t.Fatalf("ValidateRecord = %v; want the create to pass with both defaults skipped", err)
			}
			if _, ok := got["a"]; ok {
				t.Fatalf("a = %v; want absent", got["a"])
			}
			if _, ok := got["b"]; ok {
				t.Fatalf("b = %v; want absent", got["b"])
			}
		})
	}
}
