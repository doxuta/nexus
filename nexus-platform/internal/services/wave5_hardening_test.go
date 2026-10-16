package services

// Wave-5 pre-FE hardening unit tests (W5-B / W5-C). Table-driven, no DB.
// Covers:
//   W5-C1 reserved field-name → FIELD_NAME_RESERVED (entity-create + add-field)
//   W5-B1 AddField unique 1061 DDL race → FIELD_ALREADY_EXISTS (still-live regression guard;
//         the sibling 1062-on-shared-dup → CROSS_TENANT_SHARED_UNIQUE scenario is RETIRED by
//         RM-28 Slice B — AddField's unique branch now materializes a non-unique idx_ index,
//         which cannot 1062. See TestAddField_UniqueDupKeyName_StillFieldExists.)
//   W5-B2 record-time VARCHAR(255) cap on unique/indexed text-ish fields → RECORD_TOO_LONG
//   W5-B2 config-time options.maxLength > 255 on unique/indexed → FIELD_MAXLENGTH_EXCEEDS_COLUMN
//   W5-B3 record-time DECIMAL(38,10) magnitude/scale bound on unique/indexed number/currency

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// ── W5-C1: reserved field names ───────────────────────────────────────────────

// reservedNames is the set the validator must reject, mirroring ddl.reservedColumns.
var reservedNames = []string{
	"id", "tenant_id", "team_set_id", "created_by", "modified_by",
	"version", "created_at", "updated_at", "deleted_at", "data",
}

func TestValidateAddField_ReservedName_W5C1(t *testing.T) {
	for _, name := range reservedNames {
		t.Run(name, func(t *testing.T) {
			in := ordinaryField()
			in.Field.Name = name
			err := ValidateAddField(in)
			if got := codeOf(err); got != "FIELD_NAME_RESERVED" {
				t.Fatalf("ValidateAddField(%q) code=%q (err=%v); want FIELD_NAME_RESERVED", name, got, err)
			}
		})
	}
	// "version" is the headline case: it used to slip through and shadow RM-24.
	// (Uppercase identifiers are rejected earlier by SanitizeIdentifier as FIELD_INVALID_NAME,
	// so by the time IsReservedColumn runs the name is already lowercase snake_case.)
	t.Run("non-reserved ok", func(t *testing.T) {
		in := ordinaryField()
		in.Field.Name = "ver" // not "version"
		if err := ValidateAddField(in); err != nil {
			t.Fatalf("ValidateAddField(ver) unexpected err=%v", err)
		}
	})
}

func TestValidateCreateEntity_ReservedName_W5C1(t *testing.T) {
	for _, name := range reservedNames {
		t.Run(name, func(t *testing.T) {
			in := validEntity()
			in.Fields = []FieldInput{{Name: name, Label: "X", FieldType: "text"}}
			err := ValidateCreateEntity(in)
			if got := codeOf(err); got != "FIELD_NAME_RESERVED" {
				t.Fatalf("ValidateCreateEntity field %q code=%q (err=%v); want FIELD_NAME_RESERVED", name, got, err)
			}
		})
	}
	// A relation whose derived <name>_id column hits a system column is also rejected.
	t.Run("relation derived team_set_id", func(t *testing.T) {
		in := validEntity()
		in.Fields = []FieldInput{{
			Name:      "team_set",
			Label:     "TS",
			FieldType: "relation",
			Options:   map[string]any{"relatedEntity": "teams"},
		}}
		err := ValidateCreateEntity(in)
		if got := codeOf(err); got != "FIELD_NAME_RESERVED" {
			t.Fatalf("relation team_set code=%q (err=%v); want FIELD_NAME_RESERVED", got, err)
		}
	})
}

// ── W5-B1: AddField unique 1061 DDL race → FIELD_ALREADY_EXISTS ───────────────

// The 1061 (ErrDuplicateKeyName) race still maps to FIELD_ALREADY_EXISTS (regression guard).
func TestAddField_UniqueDupKeyName_StillFieldExists(t *testing.T) {
	schema := &fakeFieldSchema{addErr: dynrepo.ErrDuplicateKeyName}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", uniqueField())
	if got := codeOf(err); got != "FIELD_ALREADY_EXISTS" {
		t.Fatalf("AddField unique 1061: code=%q; want FIELD_ALREADY_EXISTS", got)
	}
}

// ── W5-B2: record-time VARCHAR(255) cap on unique/indexed string-ish fields ────

func TestCheckType_GenColumnTextCap_W5B2(t *testing.T) {
	long := strings.Repeat("a", 256)
	exactly := strings.Repeat("b", 255)

	cases := []struct {
		name      string
		fieldType string
		unique    bool
		indexed   bool
		val       string
		wantCode  string
	}{
		{"unique text 256 → too long", "text", true, false, long, "RECORD_TOO_LONG"},
		{"indexed text 256 → too long", "text", false, true, long, "RECORD_TOO_LONG"},
		{"unique email 256 → too long", "email", true, false, "u" + strings.Repeat("x", 250) + "@a.io", "RECORD_TOO_LONG"},
		{"unique text exactly 255 → ok", "text", true, false, exactly, ""},
		// A plain JSON-only field (neither unique nor indexed) has no gen column → no cap.
		{"plain text 256 → ok (no gen column)", "text", false, false, long, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := FieldSpec{Name: "s", FieldType: tc.fieldType, IsUnique: tc.unique, IsIndexed: tc.indexed}
			err := checkType(f, tc.val)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("checkType code=%q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}

// indexed picklist value must still be a member AND within 255 runes.
func TestCheckType_IndexedPicklistCap_W5B2(t *testing.T) {
	long := strings.Repeat("p", 256)
	f := FieldSpec{
		Name:      "p",
		FieldType: "picklist",
		IsIndexed: true,
		Options:   map[string]any{"values": []any{long}},
	}
	err := checkType(f, long)
	if got := codeOf(err); got != "RECORD_TOO_LONG" {
		t.Fatalf("indexed picklist 256: code=%q (err=%v); want RECORD_TOO_LONG", got, err)
	}
}

// ── W5-B2 config-time: options.maxLength > 255 on unique/indexed → reject ──────

func TestValidateGenColumnMaxLength_W5B2(t *testing.T) {
	cases := []struct {
		name      string
		fieldType string
		unique    bool
		indexed   bool
		maxLength float64
		wantCode  string
	}{
		{"unique text maxLength 256 → reject", "text", true, false, 256, "FIELD_MAXLENGTH_EXCEEDS_COLUMN"},
		{"indexed text maxLength 1000 → reject", "text", false, true, 1000, "FIELD_MAXLENGTH_EXCEEDS_COLUMN"},
		{"unique text maxLength 255 → ok", "text", true, false, 255, ""},
		{"plain text maxLength 1000 → ok (no gen column)", "text", false, false, 1000, ""},
		{"unique number maxLength irrelevant → ok", "number", true, false, 1000, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := map[string]any{"maxLength": tc.maxLength}
			if tc.indexed {
				opts["indexed"] = true
			}
			err := validateGenColumnMaxLength("f", tc.fieldType, tc.unique, opts)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("validateGenColumnMaxLength code=%q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}

// The config gate is wired into ValidateAddField and ValidateCreateEntity end-to-end.
func TestValidateAddField_MaxLengthExceedsColumn_W5B2(t *testing.T) {
	in := ordinaryField()
	in.Field.FieldType = "text"
	in.Field.IsUnique = true
	in.Field.Options = map[string]any{"maxLength": float64(500)}
	err := ValidateAddField(in)
	if got := codeOf(err); got != "FIELD_MAXLENGTH_EXCEEDS_COLUMN" {
		t.Fatalf("ValidateAddField maxLength 500 unique: code=%q (err=%v); want FIELD_MAXLENGTH_EXCEEDS_COLUMN", got, err)
	}
}

// ── W5-B3: record-time DECIMAL(38,10) bound on unique/indexed number/currency ──

func TestCheckType_GenColumnDecimal_W5B3(t *testing.T) {
	cases := []struct {
		name      string
		fieldType string
		unique    bool
		indexed   bool
		val       any
		wantCode  string
	}{
		{"unique number 1e30 → out of range", "number", true, false, 1e30, "RECORD_INVALID_RANGE"},
		{"indexed number -1e29 → out of range", "number", false, true, -1e29, "RECORD_INVALID_RANGE"},
		{"unique number 12.5 → ok", "number", true, false, 12.5, ""},
		// plain number (no gen column) accepts huge magnitude (lives in JSON).
		{"plain number 1e30 → ok (no gen column)", "number", false, false, 1e30, ""},
		// indexed currency amount overflow → out of range.
		{"indexed currency 1e30 → out of range", "currency", false, true,
			map[string]any{"amount": 1e30, "currencyCode": "USD"}, "RECORD_INVALID_RANGE"},
		{"indexed currency normal → ok", "currency", false, true,
			map[string]any{"amount": float64(199), "currencyCode": "USD"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := FieldSpec{Name: "n", FieldType: tc.fieldType, IsUnique: tc.unique, IsIndexed: tc.indexed}
			err := checkType(f, tc.val)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("checkType code=%q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}

// ── W5-B4: repo classifier maps MySQL constraint codes to typed errors ─────────

func TestAsDataConstraint_W5B4(t *testing.T) {
	// too-long → RECORD_TOO_LONG with details.field from the parsed column name.
	tooLong := &dynrepo.DataConstraintError{Kind: dynrepo.DataConstraintTooLong, Field: "sku"}
	if err := asDataConstraint(tooLong); codeOf(err) != "RECORD_TOO_LONG" {
		t.Fatalf("too-long: code=%q; want RECORD_TOO_LONG", codeOf(err))
	}
	// range/out-of-range → RECORD_INVALID_RANGE.
	rng := &dynrepo.DataConstraintError{Kind: dynrepo.DataConstraintRange, Field: "amount"}
	if err := asDataConstraint(rng); codeOf(err) != "RECORD_INVALID_RANGE" {
		t.Fatalf("range: code=%q; want RECORD_INVALID_RANGE", codeOf(err))
	}
	// a non-constraint error passes through unchanged.
	other := context.Canceled
	if asDataConstraint(other) != other {
		t.Fatalf("non-constraint error must pass through unchanged")
	}
}
