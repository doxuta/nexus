package services

import (
	"errors"
	"testing"
)

// codeOf extracts the stable error code from a ValidationError, or "" if the error
// is not one. Tests assert on codes, not message text, so wording can change freely.
func codeOf(err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

func validEntity() CreateEntityInput {
	return CreateEntityInput{
		Name:  "courses",
		Label: "Khoá học",
		Fields: []FieldInput{
			{Name: "title", Label: "Tên", FieldType: "text", IsRequired: true},
			{Name: "code", Label: "Mã", FieldType: "text", IsUnique: true},
			{Name: "level", Label: "Cấp", FieldType: "picklist", Options: map[string]any{"values": []any{"a", "b"}}},
		},
	}
}

func TestValidateCreateEntity(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(in *CreateEntityInput)
		wantCode string // "" = expect success
	}{
		{"valid", func(in *CreateEntityInput) {}, ""},
		{"empty name", func(in *CreateEntityInput) { in.Name = "" }, "ENTITY_INVALID_NAME"},
		{"reserved name", func(in *CreateEntityInput) { in.Name = "select" }, "ENTITY_INVALID_NAME"},
		{"injection name", func(in *CreateEntityInput) { in.Name = "x; drop" }, "ENTITY_INVALID_NAME"},
		{"empty label", func(in *CreateEntityInput) { in.Label = "  " }, "ENTITY_LABEL_REQUIRED"},
		{"no fields is allowed", func(in *CreateEntityInput) { in.Fields = nil }, ""},
		{"bad field name", func(in *CreateEntityInput) { in.Fields[0].Name = "Bad Name" }, "FIELD_INVALID_NAME"},
		{"duplicate field", func(in *CreateEntityInput) { in.Fields[1].Name = "title" }, "FIELD_DUPLICATE"},
		{"unknown field type", func(in *CreateEntityInput) { in.Fields[0].FieldType = "foobar" }, "FIELD_INVALID_TYPE"},
		{"picklist without options", func(in *CreateEntityInput) { in.Fields[2].Options = nil }, "FIELD_PICKLIST_NO_OPTIONS"},
		{"picklist empty values", func(in *CreateEntityInput) {
			in.Fields[2].Options = map[string]any{"values": []any{}}
		}, "FIELD_PICKLIST_NO_OPTIONS"},

		// --- valueLabels (B-PICKLIST-VN): display-label layer over stored codes. The codes in
		// `values` never change (translating them orphans stored records and breaks every pack
		// queue/report/workflow comparing codes verbatim); Vietnamese lives in this OPTIONAL map,
		// and PUT /admin/fields accepts arbitrary options, so its SHAPE is a real trust boundary —
		// junk stored here renders on the customer face.
		{"picklist with valueLabels", func(in *CreateEntityInput) {
			in.Fields[2].Options = map[string]any{"values": []any{"New", "Lost"}, "valueLabels": map[string]any{"New": "Mới", "Lost": "Mất"}}
		}, ""},
		{"valueLabels orphan key tolerated (a label whose code was deleted must not block the admin)", func(in *CreateEntityInput) {
			in.Fields[2].Options = map[string]any{"values": []any{"New"}, "valueLabels": map[string]any{"Gone": "Đã xoá"}}
		}, ""},
		{"valueLabels wrong shape (array)", func(in *CreateEntityInput) {
			in.Fields[2].Options = map[string]any{"values": []any{"New"}, "valueLabels": []any{"Mới"}}
		}, "FIELD_VALUE_LABELS_INVALID"},
		{"valueLabels non-string label", func(in *CreateEntityInput) {
			in.Fields[2].Options = map[string]any{"values": []any{"New"}, "valueLabels": map[string]any{"New": 5}}
		}, "FIELD_VALUE_LABELS_INVALID"},
		{"valueLabels blank label", func(in *CreateEntityInput) {
			in.Fields[2].Options = map[string]any{"values": []any{"New"}, "valueLabels": map[string]any{"New": "  "}}
		}, "FIELD_VALUE_LABELS_INVALID"},

		// --- Đ2: extended catalog (nested-JSON config) field types ---

		// multi_select: list of allowed options, same shape as picklist (options.values).
		{"multi_select valid", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "multi_select"
			in.Fields[0].Options = map[string]any{"values": []any{"go", "py"}}
		}, ""},
		{"multi_select without options", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "multi_select"
			in.Fields[0].Options = nil
		}, "FIELD_MULTI_SELECT_NO_OPTIONS"},
		{"multi_select empty values", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "multi_select"
			in.Fields[0].Options = map[string]any{"values": []any{}}
		}, "FIELD_MULTI_SELECT_NO_OPTIONS"},

		// currency: nested config {currencyCode: string, precision: int>=0}.
		{"currency valid", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "currency"
			in.Fields[0].Options = map[string]any{"currencyCode": "VND", "precision": float64(0)}
		}, ""},
		{"currency precision optional", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "currency"
			in.Fields[0].Options = map[string]any{"currencyCode": "USD"}
		}, ""},
		{"currency missing code", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "currency"
			in.Fields[0].Options = map[string]any{"precision": float64(2)}
		}, "FIELD_CURRENCY_CODE_REQUIRED"},
		{"currency bad code", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "currency"
			in.Fields[0].Options = map[string]any{"currencyCode": "vnd"}
		}, "FIELD_CURRENCY_CODE_INVALID"},
		{"currency negative precision", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "currency"
			in.Fields[0].Options = map[string]any{"currencyCode": "VND", "precision": float64(-1)}
		}, "FIELD_CURRENCY_PRECISION_INVALID"},
		{"currency precision too large", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "currency"
			in.Fields[0].Options = map[string]any{"currencyCode": "VND", "precision": float64(99)}
		}, "FIELD_CURRENCY_PRECISION_INVALID"},
		// RM-31 (revised): opt-in floor/ceiling must be validated at config time so a mistyped
		// bound fails loudly instead of silently vanishing at record time (gate-found defect).
		{"currency min valid float", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "currency"
			in.Fields[0].Options = map[string]any{"currencyCode": "USD", "min": float64(0)}
		}, ""},
		{"currency min+max valid", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "currency"
			in.Fields[0].Options = map[string]any{"currencyCode": "USD", "min": float64(-100), "max": float64(100)}
		}, ""},
		{"currency min string: invalid", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "currency"
			in.Fields[0].Options = map[string]any{"currencyCode": "USD", "min": "0"}
		}, "FIELD_CURRENCY_RANGE_INVALID"},
		{"currency max bool: invalid", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "currency"
			in.Fields[0].Options = map[string]any{"currencyCode": "USD", "max": true}
		}, "FIELD_CURRENCY_RANGE_INVALID"},
		{"currency min>max: invalid (dead field)", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "currency"
			in.Fields[0].Options = map[string]any{"currencyCode": "USD", "min": float64(100), "max": float64(0)}
		}, "FIELD_CURRENCY_RANGE_INVALID"},

		// rating: nested config {max: int in [2,10]}.
		{"rating valid", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "rating"
			in.Fields[0].Options = map[string]any{"max": float64(5)}
		}, ""},
		{"rating default max", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "rating"
			in.Fields[0].Options = nil
		}, ""},
		{"rating max too small", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "rating"
			in.Fields[0].Options = map[string]any{"max": float64(1)}
		}, "FIELD_RATING_MAX_INVALID"},
		{"rating max too large", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "rating"
			in.Fields[0].Options = map[string]any{"max": float64(11)}
		}, "FIELD_RATING_MAX_INVALID"},

		// rich_text: scalar, no config required.
		{"rich_text valid", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "rich_text"
			in.Fields[0].Options = nil
		}, ""},

		// composite: nested sub-fields (options.subFields []{name,fieldType}).
		{"composite valid", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "composite"
			in.Fields[0].Options = map[string]any{"subFields": []any{
				map[string]any{"name": "first_name", "fieldType": "text"},
				map[string]any{"name": "last_name", "fieldType": "text"},
			}}
		}, ""},
		{"composite empty subFields", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "composite"
			in.Fields[0].Options = map[string]any{"subFields": []any{}}
		}, "FIELD_COMPOSITE_NO_SUBFIELDS"},
		{"composite missing subFields", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "composite"
			in.Fields[0].Options = nil
		}, "FIELD_COMPOSITE_NO_SUBFIELDS"},
		{"composite bad sub name", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "composite"
			in.Fields[0].Options = map[string]any{"subFields": []any{
				map[string]any{"name": "Bad Name", "fieldType": "text"},
			}}
		}, "FIELD_COMPOSITE_SUBFIELD_INVALID"},
		{"composite duplicate sub name", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "composite"
			in.Fields[0].Options = map[string]any{"subFields": []any{
				map[string]any{"name": "x", "fieldType": "text"},
				map[string]any{"name": "x", "fieldType": "number"},
			}}
		}, "FIELD_COMPOSITE_SUBFIELD_DUPLICATE"},
		{"composite sub bad type", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "composite"
			in.Fields[0].Options = map[string]any{"subFields": []any{
				map[string]any{"name": "x", "fieldType": "foobar"},
			}}
		}, "FIELD_COMPOSITE_SUBFIELD_INVALID"},
		// G3: composite must not nest a composite (no recursion / unbounded depth).
		{"composite sub composite rejected", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "composite"
			in.Fields[0].Options = map[string]any{"subFields": []any{
				map[string]any{"name": "x", "fieldType": "composite"},
			}}
		}, "FIELD_COMPOSITE_SUBFIELD_INVALID"},
		// G5: relation must not appear inside a composite (relation needs FK column).
		{"composite sub relation rejected", func(in *CreateEntityInput) {
			in.Fields[0].FieldType = "composite"
			in.Fields[0].Options = map[string]any{"subFields": []any{
				map[string]any{"name": "x", "fieldType": "relation"},
			}}
		}, "FIELD_COMPOSITE_SUBFIELD_INVALID"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validEntity()
			tc.mutate(&in)
			err := ValidateCreateEntity(in)
			if got := codeOf(err); got != tc.wantCode {
				t.Errorf("ValidateCreateEntity code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}

// TestFieldConfig_DerivedIDColumnLength pins an accept-then-swallow: `relation` and `user` fields
// create a derived `<field>_id` column, SanitizeIdentifier caps identifiers at 26 chars, and "_id"
// costs 3 — so a 24+ character field name yields a column the engine can CREATE but can never
// address again, because every filter/sort path re-sanitizes the physical name and refuses it.
// Accepted at config time, broken at use time, with nothing pointing back at the cause. The
// polymorphic_relation arm has budgeted for its own "_type" suffix since it shipped; these two
// did not.
func TestFieldConfig_DerivedIDColumnLength(t *testing.T) {
	long := "cong_ty_phu_trach_chinh_a" // 25 chars → 25+3 = 28 > 26
	if len(long) <= 23 {
		t.Fatalf("fixture no longer over the limit: %d chars", len(long))
	}
	ok := "cong_ty_phu_trach" // 17 chars → fits with room to spare

	for _, tc := range []struct {
		ft      string
		options map[string]any
	}{
		{"relation", map[string]any{"relatedEntity": "crm_accounts"}},
		{"user", nil},
	} {
		t.Run(tc.ft+" too long", func(t *testing.T) {
			err := validateFieldConfig(long, tc.ft, tc.options)
			if codeOf(err) != "FIELD_RELATION_NAME_TOO_LONG" {
				t.Fatalf("err = %v; want FIELD_RELATION_NAME_TOO_LONG — a column nobody can query later must be refused NOW", err)
			}
		})
		t.Run(tc.ft+" fits", func(t *testing.T) {
			if err := validateFieldConfig(ok, tc.ft, tc.options); err != nil {
				t.Fatalf("a legal name was refused: %v", err)
			}
		})
	}
}
