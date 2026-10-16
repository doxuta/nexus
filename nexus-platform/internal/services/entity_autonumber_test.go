package services

// ENG #6 auto_number — config-time validation (entity-create + add-field share the single
// validateFieldConfig/validateAutoNumberFieldFlags source of truth). Mirrors the table-driven
// style of entity_rating_relation_test.go's TestValidateRatingConfig_Min.

import (
	"strings"
	"testing"
)

func TestValidateAutoNumberConfig(t *testing.T) {
	cases := []struct {
		name     string
		options  map[string]any
		wantCode string // "" = success
	}{
		{"no options ok", nil, ""},
		{"empty options ok", map[string]any{}, ""},
		{"prefix only", map[string]any{"prefix": "INV-"}, ""},
		{"padding only", map[string]any{"padding": float64(5)}, ""},
		{"prefix and padding", map[string]any{"prefix": "INV-", "padding": float64(5)}, ""},
		{"padding zero ok (no padding)", map[string]any{"padding": float64(0)}, ""},
		{"padding max boundary (12) ok", map[string]any{"padding": float64(12)}, ""},
		{"prefix exactly 20 chars ok", map[string]any{"prefix": "12345678901234567890"}, ""},
		{"prefix 21 chars rejected", map[string]any{"prefix": "123456789012345678901"}, "FIELD_AUTONUMBER_PREFIX_INVALID"},
		// Review LOW-1: length is counted in RUNES (codebase maxLength convention), not bytes —
		// 20 two-byte characters (40 bytes) must pass; 21 runes must fail regardless of bytes.
		{"prefix 20 multibyte runes ok", map[string]any{"prefix": strings.Repeat("Đ", 20)}, ""},
		{"prefix 21 multibyte runes rejected", map[string]any{"prefix": strings.Repeat("Đ", 21)}, "FIELD_AUTONUMBER_PREFIX_INVALID"},
		{"prefix wrong type", map[string]any{"prefix": float64(42)}, "FIELD_AUTONUMBER_PREFIX_INVALID"},
		{"padding negative rejected", map[string]any{"padding": float64(-1)}, "FIELD_AUTONUMBER_PADDING_INVALID"},
		{"padding above max (13) rejected", map[string]any{"padding": float64(13)}, "FIELD_AUTONUMBER_PADDING_INVALID"},
		{"padding non-integer rejected", map[string]any{"padding": float64(2.5)}, "FIELD_AUTONUMBER_PADDING_INVALID"},
		{"padding wrong type", map[string]any{"padding": "5"}, "FIELD_AUTONUMBER_PADDING_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAutoNumberConfig("invoice_no", tc.options)
			if got := codeOf(err); got != tc.wantCode {
				t.Errorf("validateAutoNumberConfig code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}

// auto_number MAY now be unique — the rule that forbade it named its own expiry condition and
// that condition has been met.
//
// It was never structural: an auto_number value is a formatted string, the same shape a unique
// `text` field projects into its generated column. It existed because the physical obj_* table is
// shared across tenants while auto_number counts PER tenant, so one shared UNIQUE index would
// reject a second tenant's legitimate "INV-00001". RM-28 removed exactly that: migration 000050
// added sys_unique_index with uq_pivot (tenant_id, entity_name, field_name, value) — uniqueness
// per tenant — and 000051 demoted the physical UNIQUE indexes to plain idx_ ones.
//
// This matters concretely: PaymentMatch.refField (bank-transfer reconciliation) REQUIRES a unique
// field, and the reference a human types into a transfer memo is exactly the document number. The
// old rule made the flagship money loop impossible to wire to the flagship numbering field.
func TestValidateAutoNumberFieldFlags_UniqueNowAllowed(t *testing.T) {
	if err := validateAutoNumberFieldFlags("invoice_no", "auto_number", true); err != nil {
		t.Fatalf("auto_number + unique must be allowed since RM-28 (per-tenant pivot); got %v", err)
	}
}

func TestValidateAutoNumberFieldFlags_NonUniqueOK(t *testing.T) {
	if err := validateAutoNumberFieldFlags("invoice_no", "auto_number", false); err != nil {
		t.Fatalf("unexpected error for non-unique auto_number field: %v", err)
	}
}

func TestValidateAutoNumberFieldFlags_OtherTypesIgnored(t *testing.T) {
	// The guard is a no-op for every other field type — it must never reject a unique text/email
	// field, which is exactly what auto_number's own rule forbids ONLY for itself.
	if err := validateAutoNumberFieldFlags("sku", "text", true); err != nil {
		t.Fatalf("unexpected error for unique text field: %v", err)
	}
}

// The entity-create path must accept it too — a rule lifted in the validator but still enforced
// one layer up would just move the wall.
func TestValidateCreateEntity_AutoNumberUniqueAccepted(t *testing.T) {
	in := CreateEntityInput{
		Name:  "invoices",
		Label: "Invoices",
		Fields: []FieldInput{
			{Name: "invoice_no", Label: "Invoice No", FieldType: "auto_number", IsUnique: true},
		},
	}
	if err := ValidateCreateEntity(in); err != nil {
		t.Fatalf("unique auto_number must pass entity-create validation; got %v", err)
	}
}

// TestValidateCreateEntity_AutoNumberHappyPath proves a well-formed auto_number field (no
// options, or prefix+padding, non-unique) passes entity-create validation cleanly.
func TestValidateCreateEntity_AutoNumberHappyPath(t *testing.T) {
	in := CreateEntityInput{
		Name:  "invoices",
		Label: "Invoices",
		Fields: []FieldInput{
			{Name: "invoice_no", Label: "Invoice No", FieldType: "auto_number",
				Options: map[string]any{"prefix": "INV-", "padding": float64(5)}},
			{Name: "amount", Label: "Amount", FieldType: "number"},
		},
	}
	if err := ValidateCreateEntity(in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidateFieldDefaultNotSupported_AutoNumber locks the RM-33a-style parity: auto_number is
// server-ASSIGNED at Create time, so a client-supplied default would never be used — same
// reasoning as lookup/rollup/currency/rich_text.
func TestValidateFieldDefaultNotSupported_AutoNumber(t *testing.T) {
	if err := ValidateFieldDefaultNotSupported("auto_number", ""); err != nil {
		t.Fatalf("empty default must be allowed (no-op): %v", err)
	}
	err := ValidateFieldDefaultNotSupported("auto_number", "INV-00001")
	if codeOf(err) != "FIELD_DEFAULT_NOT_SUPPORTED" {
		t.Fatalf("code = %q; want FIELD_DEFAULT_NOT_SUPPORTED (err=%v)", codeOf(err), err)
	}
}
