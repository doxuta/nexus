package services

// Wave 3 / Validation-completeness unit tests (RM-29 through RM-33).
// Table-driven, no DB — covers config-time validation (entity_service.go) and
// record-time validation (record_service.go).

import (
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// ── RM-29: number range / text length+pattern ─────────────────────────────────

func TestCheckNumberRange_RM29(t *testing.T) {
	cases := []struct {
		name     string
		opts     map[string]any
		val      float64
		wantCode string
	}{
		{"no options: any value OK", nil, -999, ""},
		{"within min-max", map[string]any{"min": float64(0), "max": float64(100)}, 50, ""},
		{"equal to min: OK", map[string]any{"min": float64(0)}, 0, ""},
		{"equal to max: OK", map[string]any{"max": float64(10)}, 10, ""},
		{"below min", map[string]any{"min": float64(1)}, 0, "RECORD_INVALID_RANGE"},
		{"above max", map[string]any{"max": float64(10)}, 11, "RECORD_INVALID_RANGE"},
		{"step=1, value=3: OK", map[string]any{"step": float64(1)}, 3, ""},
		{"step=0.5, value=1.5: OK", map[string]any{"step": float64(0.5)}, 1.5, ""},
		{"step=0.5, value=1.3: fail", map[string]any{"step": float64(0.5)}, 1.3, "RECORD_INVALID_RANGE"},
		{"step=10, value=0: OK (zero is multiple of anything)", map[string]any{"step": float64(10)}, 0, ""},
		{"step=10, value=15: fail", map[string]any{"step": float64(10)}, 15, "RECORD_INVALID_RANGE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := FieldSpec{Name: "n", FieldType: "number", Options: tc.opts}
			err := checkType(f, tc.val)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("checkType number code=%q err=%v; want %q", got, err, tc.wantCode)
			}
		})
	}
}

func TestCheckTextConstraints_RM29(t *testing.T) {
	cases := []struct {
		name      string
		fieldType string
		opts      map[string]any
		val       string
		wantCode  string
	}{
		// maxLength (rune count, not bytes)
		{"text: no opts", "text", nil, "anything", ""},
		{"text: within maxLength", "text", map[string]any{"maxLength": float64(10)}, "hello", ""},
		{"text: exactly at maxLength", "text", map[string]any{"maxLength": float64(5)}, "hello", ""},
		{"text: exceeds maxLength", "text", map[string]any{"maxLength": float64(4)}, "hello", "RECORD_TOO_LONG"},
		{"text: Vietnamese rune count (3 chars)", "text", map[string]any{"maxLength": float64(3)}, "xin", ""},
		{"text: Vietnamese exceeds", "text", map[string]any{"maxLength": float64(2)}, "xin", "RECORD_TOO_LONG"},
		// pattern
		{"text: pattern match", "text", map[string]any{"pattern": `[A-Z]{3}`}, "ABC", ""},
		{"text: pattern mismatch", "text", map[string]any{"pattern": `[A-Z]{3}`}, "abc", "RECORD_INVALID_PATTERN"},
		{"text: pattern anchored (partial not allowed)", "text", map[string]any{"pattern": `\d+`}, "abc123", "RECORD_INVALID_PATTERN"},
		{"text: digits-only pattern match", "text", map[string]any{"pattern": `\d+`}, "123", ""},
		// rich_text shares same constraints
		{"rich_text: exceeds maxLength", "rich_text", map[string]any{"maxLength": float64(5)}, "toolong!", "RECORD_TOO_LONG"},
		{"rich_text: pattern match", "rich_text", map[string]any{"pattern": `.*`}, "any content", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := FieldSpec{Name: "s", FieldType: tc.fieldType, Options: tc.opts}
			err := checkType(f, tc.val)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("checkType %s code=%q err=%v; want %q", tc.fieldType, got, err, tc.wantCode)
			}
		})
	}
}

func TestValidateTextConfig_RM29(t *testing.T) {
	cases := []struct {
		name     string
		opts     map[string]any
		wantCode string
	}{
		{"nil opts: OK", nil, ""},
		{"valid maxLength", map[string]any{"maxLength": float64(100)}, ""},
		{"maxLength zero: invalid", map[string]any{"maxLength": float64(0)}, "FIELD_TEXT_MAXLENGTH_INVALID"},
		{"maxLength negative: invalid", map[string]any{"maxLength": float64(-1)}, "FIELD_TEXT_MAXLENGTH_INVALID"},
		{"maxLength float: invalid", map[string]any{"maxLength": float64(1.5)}, "FIELD_TEXT_MAXLENGTH_INVALID"},
		{"valid pattern", map[string]any{"pattern": `\d+`}, ""},
		{"bad pattern (unclosed group)", map[string]any{"pattern": `(`}, "FIELD_TEXT_PATTERN_INVALID"},
		{"empty pattern string: invalid", map[string]any{"pattern": ""}, "FIELD_TEXT_PATTERN_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTextConfig("f", tc.opts)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("validateTextConfig code=%q err=%v; want %q", got, err, tc.wantCode)
			}
		})
	}
}

func TestValidateNumberConfig_RM29(t *testing.T) {
	cases := []struct {
		name     string
		opts     map[string]any
		wantCode string
	}{
		{"nil opts: OK", nil, ""},
		{"only min: OK", map[string]any{"min": float64(0)}, ""},
		{"only max: OK", map[string]any{"max": float64(100)}, ""},
		{"min < max: OK", map[string]any{"min": float64(1), "max": float64(10)}, ""},
		{"min == max: OK", map[string]any{"min": float64(5), "max": float64(5)}, ""},
		{"min > max: invalid", map[string]any{"min": float64(10), "max": float64(1)}, "FIELD_NUMBER_RANGE_INVALID"},
		{"min is string: invalid", map[string]any{"min": "zero"}, "FIELD_NUMBER_RANGE_INVALID"},
		{"valid step", map[string]any{"step": float64(0.5)}, ""},
		{"step zero: invalid", map[string]any{"step": float64(0)}, "FIELD_NUMBER_STEP_INVALID"},
		{"step negative: invalid", map[string]any{"step": float64(-1)}, "FIELD_NUMBER_STEP_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNumberConfig("f", tc.opts)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("validateNumberConfig code=%q err=%v; want %q", got, err, tc.wantCode)
			}
		})
	}
}

// RM-29 config-time: validateFieldConfig delegates to validateTextConfig / validateNumberConfig.
func TestValidateFieldConfig_RM29_ConfigTime(t *testing.T) {
	cases := []struct {
		name      string
		fieldType string
		opts      map[string]any
		wantCode  string
	}{
		{"text bad pattern", "text", map[string]any{"pattern": `(`}, "FIELD_TEXT_PATTERN_INVALID"},
		{"text valid", "text", map[string]any{"maxLength": float64(50)}, ""},
		{"rich_text bad maxLength", "rich_text", map[string]any{"maxLength": float64(-1)}, "FIELD_TEXT_MAXLENGTH_INVALID"},
		{"number min>max", "number", map[string]any{"min": float64(10), "max": float64(5)}, "FIELD_NUMBER_RANGE_INVALID"},
		{"number valid step", "number", map[string]any{"step": float64(1)}, ""},
		{"number bad step", "number", map[string]any{"step": float64(0)}, "FIELD_NUMBER_STEP_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFieldConfig("f", tc.fieldType, tc.opts)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("validateFieldConfig(%s) code=%q err=%v; want %q", tc.fieldType, got, err, tc.wantCode)
			}
		})
	}
}

// ── RM-30: extractDuplicateField ─────────────────────────────────────────────

func TestExtractDuplicateField_RM30(t *testing.T) {
	cases := []struct {
		name       string
		indexName  string
		entityName string
		want       string
	}{
		{"standard match", "uq_obj_accounts_email", "accounts", "email"},
		{"multi-word entity", "uq_obj_crm_leads_phone", "crm_leads", "phone"},
		{"multi-word field", "uq_obj_accounts_primary_email", "accounts", "primary_email"},
		{"MySQL 8.0.19 table-qualified (already stripped by caller)", "uq_obj_accounts_email", "accounts", "email"},
		{"PRIMARY key: no match", "PRIMARY", "accounts", ""},
		{"wrong entity: no match", "uq_obj_contacts_email", "accounts", ""},
		{"empty index: no match", "", "accounts", ""},
		{"empty entity: no match", "uq_obj_accounts_email", "", ""},
		{"custom index (no uq_ prefix): no match", "custom_unique_idx", "accounts", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractDuplicateField(tc.indexName, tc.entityName)
			if got != tc.want {
				t.Fatalf("extractDuplicateField(%q, %q) = %q; want %q", tc.indexName, tc.entityName, got, tc.want)
			}
		})
	}
}

// ── RM-31: currency precision + non-negative ──────────────────────────────────

func TestCheckCurrency_RM31(t *testing.T) {
	cases := []struct {
		name     string
		opts     map[string]any
		val      map[string]any
		wantCode string
	}{
		// type checks (existing behaviour must not regress)
		{"not an object", nil, nil, "RECORD_INVALID_TYPE"},
		{"missing amount", nil, map[string]any{"currencyCode": "USD"}, "RECORD_INVALID_TYPE"},
		{"missing currencyCode", nil, map[string]any{"amount": float64(10)}, "RECORD_INVALID_TYPE"},
		{"empty currencyCode", nil, map[string]any{"amount": float64(10), "currencyCode": "  "}, "RECORD_INVALID_TYPE"},
		// RM-31 (revised): currency is unbounded by default — negatives are valid (refund / credit note).
		{"negative amount, no bounds: OK (refund/credit)", nil, map[string]any{"amount": float64(-1), "currencyCode": "USD"}, ""},
		{"zero amount: OK", nil, map[string]any{"amount": float64(0), "currencyCode": "USD"}, ""},
		{"positive amount: OK", nil, map[string]any{"amount": float64(19.99), "currencyCode": "USD"}, ""},
		// RM-31 (revised): per-field opt-in floor/ceiling via options.min / options.max.
		{"min=0 forbids negative: fail", map[string]any{"min": float64(0)}, map[string]any{"amount": float64(-0.01), "currencyCode": "USD"}, "RECORD_INVALID_RANGE"},
		{"min=0, zero: OK (boundary inclusive)", map[string]any{"min": float64(0)}, map[string]any{"amount": float64(0), "currencyCode": "USD"}, ""},
		{"min=-100 allows -50: OK within floor", map[string]any{"min": float64(-100)}, map[string]any{"amount": float64(-50), "currencyCode": "USD"}, ""},
		{"min=-100 forbids -100.01: fail", map[string]any{"min": float64(-100)}, map[string]any{"amount": float64(-100.01), "currencyCode": "USD"}, "RECORD_INVALID_RANGE"},
		{"max=1000 allows 1000: OK (boundary inclusive)", map[string]any{"max": float64(1000)}, map[string]any{"amount": float64(1000), "currencyCode": "USD"}, ""},
		{"max=1000 forbids 1000.01: fail", map[string]any{"max": float64(1000)}, map[string]any{"amount": float64(1000.01), "currencyCode": "USD"}, "RECORD_INVALID_RANGE"},
		{"min=0,max=1000 in-range: OK", map[string]any{"min": float64(0), "max": float64(1000)}, map[string]any{"amount": float64(500), "currencyCode": "USD"}, ""},
		// RM-31: precision enforcement (reject, not round)
		{"precision=2, 19.99: OK", map[string]any{"precision": float64(2)}, map[string]any{"amount": float64(19.99), "currencyCode": "USD"}, ""},
		{"precision=2, 19.999: fail", map[string]any{"precision": float64(2)}, map[string]any{"amount": 19.999, "currencyCode": "USD"}, "RECORD_INVALID_PRECISION"},
		{"precision=0, 5.0: OK", map[string]any{"precision": float64(0)}, map[string]any{"amount": float64(5), "currencyCode": "VND"}, ""},
		{"precision=0, 5.1: fail", map[string]any{"precision": float64(0)}, map[string]any{"amount": 5.1, "currencyCode": "VND"}, "RECORD_INVALID_PRECISION"},
		{"precision=3, 1.234: OK", map[string]any{"precision": float64(3)}, map[string]any{"amount": 1.234, "currencyCode": "EUR"}, ""},
		{"no precision option: any decimals OK", nil, map[string]any{"amount": 1.23456789, "currencyCode": "USD"}, ""},
		// Queue #7 (Q7, Founder 2026-09-05): the row's currencyCode must EQUAL the field's configured
		// options.currencyCode — REJECT the row, never relabel it (a USD 100 rewritten to VND 100 is
		// the exact wrong total this guard exists to prevent). No configured code (legacy sys_fields
		// row / hand-built spec) → guard skipped, which the nil-opts cases above already pin.
		{"Q7 configured VND, row USD: mismatch", map[string]any{"currencyCode": "VND"}, map[string]any{"amount": float64(100), "currencyCode": "USD"}, "RECORD_CURRENCY_MISMATCH"},
		{"Q7 configured VND, row vnd: mismatch (config is canonical uppercase; no case-folding rewrite)", map[string]any{"currencyCode": "VND"}, map[string]any{"amount": float64(100), "currencyCode": "vnd"}, "RECORD_CURRENCY_MISMATCH"},
		{"Q7 configured VND, row VND: OK", map[string]any{"currencyCode": "VND"}, map[string]any{"amount": float64(100), "currencyCode": "VND"}, ""},
		{"Q7 configured VND, row ' VND ': OK (trimmed like the non-empty check)", map[string]any{"currencyCode": "VND"}, map[string]any{"amount": float64(100), "currencyCode": " VND "}, ""},
		{"Q7 configured code empty string: guard skipped", map[string]any{"currencyCode": ""}, map[string]any{"amount": float64(100), "currencyCode": "USD"}, ""},
		{"Q7 mismatch is checked before precision (code wins)", map[string]any{"currencyCode": "VND", "precision": float64(0)}, map[string]any{"amount": 5.1, "currencyCode": "USD"}, "RECORD_CURRENCY_MISMATCH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := FieldSpec{Name: "price", FieldType: "currency", Options: tc.opts}
			var val any = tc.val
			if tc.val == nil {
				val = "not-an-object"
			}
			err := checkType(f, val)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("checkCurrency code=%q err=%v; want %q", got, err, tc.wantCode)
			}
		})
	}
}

// TestCheckCurrency_Q7MismatchNamesField pins the wire shape of the Q7 reject: Field and
// Details["field"] carry the field name, so the direct create/update envelope shows
// details.field and the CSV import row-error table shows the "Field" column instead of "—".
func TestCheckCurrency_Q7MismatchNamesField(t *testing.T) {
	f := FieldSpec{Name: "list_price", FieldType: "currency", Options: map[string]any{"currencyCode": "VND"}}
	err := checkType(f, map[string]any{"amount": float64(100), "currencyCode": "USD"})
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("checkCurrency: want *ValidationError, got %T (%v)", err, err)
	}
	if ve.Code != "RECORD_CURRENCY_MISMATCH" {
		t.Fatalf("code=%q; want RECORD_CURRENCY_MISMATCH", ve.Code)
	}
	if ve.Field != "list_price" || ve.Details["field"] != "list_price" {
		t.Fatalf("Field=%q Details=%v; want both to name list_price", ve.Field, ve.Details)
	}
	for _, want := range []string{"list_price", "VND", "USD"} {
		if !strings.Contains(ve.Msg, want) {
			t.Fatalf("message %q must name %q so the user knows what to fix", ve.Msg, want)
		}
	}
}

// ── RM-32: composite sub-field required ────────────────────────────────────────

func compositeFieldWith(subFields []map[string]any) FieldSpec {
	subs := make([]any, len(subFields))
	for i, s := range subFields {
		subs[i] = s
	}
	return FieldSpec{
		Name:      "name",
		FieldType: "composite",
		Options:   map[string]any{"subFields": subs},
	}
}

func TestCheckComposite_RM32(t *testing.T) {
	f := compositeFieldWith([]map[string]any{
		{"name": "firstName", "fieldType": "text", "required": true},
		{"name": "lastName", "fieldType": "text", "required": false},
		{"name": "middleName", "fieldType": "text"}, // no required key → optional
	})

	cases := []struct {
		name      string
		val       map[string]any
		wantCode  string
		wantField string // expected Details["field"] when error has one
	}{
		{"all sub-fields present: OK", map[string]any{"firstName": "A", "lastName": "B", "middleName": "C"}, "", ""},
		{"optional absent: OK", map[string]any{"firstName": "A"}, "", ""},
		{"required present: OK", map[string]any{"firstName": "A", "lastName": "B"}, "", ""},
		{"required absent: fail", map[string]any{"lastName": "B"}, "RECORD_REQUIRED_MISSING", "name.firstName"},
		{"required empty string: fail", map[string]any{"firstName": "  ", "lastName": "B"}, "RECORD_REQUIRED_MISSING", "name.firstName"},
		{"unknown sub-key: fail", map[string]any{"firstName": "A", "unknown": "x"}, "RECORD_UNKNOWN_FIELD", ""},
		{"not an object: fail", nil, "RECORD_INVALID_TYPE", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var val any = tc.val
			if tc.val == nil {
				val = "not-an-object"
			}
			err := checkType(f, val)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("checkComposite code=%q err=%v; want %q", got, err, tc.wantCode)
			}
			if tc.wantField != "" {
				ve, ok := err.(*ValidationError)
				if !ok {
					t.Fatalf("expected *ValidationError, got %T", err)
				}
				if ve.Field != tc.wantField {
					t.Fatalf("Field=%q; want %q", ve.Field, tc.wantField)
				}
				if ve.Details["field"] != tc.wantField {
					t.Fatalf("Details[field]=%q; want %q", ve.Details["field"], tc.wantField)
				}
			}
		})
	}
}

func TestCompositeSubSpecs_RequiredExtracted_RM32(t *testing.T) {
	opts := map[string]any{
		"subFields": []any{
			map[string]any{"name": "a", "fieldType": "text", "required": true},
			map[string]any{"name": "b", "fieldType": "text", "required": false},
			map[string]any{"name": "c", "fieldType": "text"}, // absent = false
		},
	}
	subs := compositeSubSpecs(opts)
	if !subs["a"].IsRequired {
		t.Error("sub 'a' should be required")
	}
	if subs["b"].IsRequired {
		t.Error("sub 'b' should not be required")
	}
	if subs["c"].IsRequired {
		t.Error("sub 'c' (no required key) should not be required")
	}
}

// TestCompositeAtomicWrite_RM32 pins the "composites are atomic-write" contract: when a
// composite field IS present in the payload (ValidatePartialRecord — PUT), every required
// sub-field of that composite must be present too. Sending only a partial sub-set (e.g. only
// lastName, omitting required firstName) MUST fail with RECORD_REQUIRED_MISSING, because
// JSON_MERGE_PATCH would silently overwrite the composite with an incomplete value otherwise.
// This is intentional: update the composite fully or omit it entirely.
func TestCompositeAtomicWrite_RM32(t *testing.T) {
	fullnameSpec := FieldSpec{
		Name:      "fullname",
		FieldType: "composite",
		Options: map[string]any{
			"subFields": []any{
				map[string]any{"name": "first", "fieldType": "text", "required": true},
				map[string]any{"name": "last", "fieldType": "text", "required": true},
			},
		},
	}
	fields := []FieldSpec{
		fullnameSpec,
		{Name: "age", FieldType: "number"},
	}

	cases := []struct {
		name     string
		payload  map[string]any
		wantCode string
	}{
		{
			"full composite present: PUT OK",
			map[string]any{"fullname": map[string]any{"first": "A", "last": "B"}},
			"",
		},
		{
			"composite omitted entirely: PUT OK (leave stored value untouched)",
			map[string]any{"age": float64(25)},
			"",
		},
		{
			"partial composite — missing required first: MUST reject",
			map[string]any{"fullname": map[string]any{"last": "B"}},
			"RECORD_REQUIRED_MISSING",
		},
		{
			"partial composite — missing required last: MUST reject",
			map[string]any{"fullname": map[string]any{"first": "A"}},
			"RECORD_REQUIRED_MISSING",
		},
		{
			"partial composite — both empty strings: MUST reject",
			map[string]any{"fullname": map[string]any{"first": "", "last": ""}},
			"RECORD_REQUIRED_MISSING",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidatePartialRecord(fields, tc.payload)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("ValidatePartialRecord code=%q err=%v; want %q", got, err, tc.wantCode)
			}
		})
	}
}

// ── RM-33: multi_select default + currency default rejected ───────────────────

func TestDefaultValue_MultiSelect_RM33(t *testing.T) {
	opts := map[string]any{"values": []any{"a", "b", "c"}}

	cases := []struct {
		name       string
		defaultVal string
		wantCode   string
		wantLen    int // length of returned []any on success
	}{
		{"valid JSON array default", `["a","b"]`, "", 2},
		{"single valid item", `["c"]`, "", 1},
		{"empty array default", `[]`, "", 0},
		{"item outside picklist: fail", `["a","z"]`, "RECORD_INVALID_PICKLIST", 0},
		{"not a JSON array: fail", `a,b`, "RECORD_INVALID_TYPE", 0},
		{"no default (empty string): no-op", "", "", -1}, // wantLen=-1 means ok=false
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := FieldSpec{Name: "tags", FieldType: "multi_select", Options: opts, DefaultVal: tc.defaultVal}
			val, ok, err := defaultValue(f)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("defaultValue multi_select code=%q err=%v; want %q", got, err, tc.wantCode)
			}
			if tc.wantLen == -1 {
				if ok {
					t.Fatalf("expected ok=false for empty DefaultVal, got ok=true val=%v", val)
				}
				return
			}
			if tc.wantCode == "" {
				arr, isArr := val.([]any)
				if !isArr {
					t.Fatalf("expected []any, got %T", val)
				}
				if len(arr) != tc.wantLen {
					t.Fatalf("len=%d; want %d", len(arr), tc.wantLen)
				}
			}
		})
	}
}

func TestValidateFieldDefaultNotSupported_RM33(t *testing.T) {
	cases := []struct {
		name       string
		fieldType  string
		defaultVal string
		wantCode   string
	}{
		{"currency with default: fail", "currency", "100", "FIELD_DEFAULT_NOT_SUPPORTED"},
		{"currency empty default: OK", "currency", "", ""},
		{"currency whitespace default: OK", "currency", "   ", ""},
		{"rich_text with default: fail (RM-33a)", "rich_text", "<b>hello</b>", "FIELD_DEFAULT_NOT_SUPPORTED"},
		{"rich_text empty default: OK", "rich_text", "", ""},
		{"number with default: OK (supported)", "number", "42", ""},
		{"text with default: OK", "text", "hello", ""},
		{"picklist with default: OK", "picklist", "active", ""},
		// queue #62: accepted at config time, then never applied (defaultValue has no branch) —
		// the "accept-then-swallow" class the `user` case fixed. Worse than dead config: the
		// field-ACL lock guard read the non-empty string as "the engine fills this field".
		{"composite with default: fail (#62)", "composite", `{"first":"A"}`, "FIELD_DEFAULT_NOT_SUPPORTED"},
		{"file with default: fail (#62)", "file", `[{"id":"x","name":"a.pdf"}]`, "FIELD_DEFAULT_NOT_SUPPORTED"},
		{"image with default: fail (#62)", "image", `[{"id":"x","name":"a.png"}]`, "FIELD_DEFAULT_NOT_SUPPORTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFieldDefaultNotSupported(tc.fieldType, tc.defaultVal)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("ValidateFieldDefaultNotSupported code=%q err=%v; want %q", got, err, tc.wantCode)
			}
		})
	}
}

// TestFieldDefault_ConfigGateMatchesRecordTime pins the parity queue #62 depends on: every field
// type the config gate lets carry a default must be one defaultValue actually materializes at
// Create. A type in neither list is a default the builder stores and the engine drops — and the
// field-ACL guard (PutGrants) would then allow locking a required field the engine never fills.
// "Handled" = defaultValue returned a value OR rejected the probe; (nil, false, nil) means the
// switch fell through to its no-scalar default branch.
func TestFieldDefault_ConfigGateMatchesRecordTime(t *testing.T) {
	for ft := range allowedFieldTypes {
		if ComputedReadonlyFieldType(ft) {
			continue // server-assigned: no role ever writes it, a default is meaningless
		}
		if fieldTypeRejectsDefault(ft) {
			continue
		}
		_, ok, err := defaultValue(FieldSpec{Name: "probe", FieldType: ft, DefaultVal: "x"})
		if !ok && err == nil {
			t.Errorf("field type %q: config gate accepts a default but defaultValue never applies one", ft)
		}
	}
}

// TestEditField_LegacyUnsupportedDefault_DoesNotBlockOtherEdits: queue #62 widened the config gate
// (composite/file/image), and `user` joined it earlier — but rows saved before either change can
// still carry such a default. Studio never sends defaultValue for these types (fieldOptions.ts
// defaultInputKind → null), so if the edit re-judged the STORED default, every edit of that field
// would 422 — including turning off "bắt buộc", the exit the field-ACL lock guard now names. The
// gate must judge the default an edit CHANGES; clearing it must stay possible. The AI Builder's
// Apply resends the stored default verbatim (DraftFieldState.DefaultValue has no omitempty and
// aiBuilder.ts forwards every `after` key), so "sent back unchanged" must pass too.
func TestEditField_LegacyUnsupportedDefault_DoesNotBlockOtherEdits(t *testing.T) {
	no, empty, again := false, "", `{"first":"B"}`
	label := "Hồ sơ"
	legacyComposite, legacyUser := `{"first":"A"}`, "b1551a1a-5b97-4640-91b5-f49d13bb118f"
	tests := []struct {
		name      string
		fieldType string
		legacy    string
		in        EditFieldInput
		wantCode  string
	}{
		{name: "composite: turn off required, default untouched → OK", fieldType: "composite", legacy: `{"first":"A"}`,
			in: EditFieldInput{FieldID: "f1", IsRequired: &no}},
		{name: "user: relabel, default untouched → OK", fieldType: "user", legacy: "b1551a1a-5b97-4640-91b5-f49d13bb118f",
			in: EditFieldInput{FieldID: "f1", Label: &label}},
		{name: "composite: clear the legacy default → OK", fieldType: "composite", legacy: `{"first":"A"}`,
			in: EditFieldInput{FieldID: "f1", DefaultVal: &empty}},
		{name: "composite: WRITING a default is still refused", fieldType: "composite", legacy: `{"first":"A"}`,
			in: EditFieldInput{FieldID: "f1", DefaultVal: &again}, wantCode: "FIELD_DEFAULT_NOT_SUPPORTED"},
		// AI Builder Apply: the stored default rides along unchanged with the real edit.
		{name: "composite: turn off required, stored default resent unchanged → OK", fieldType: "composite", legacy: legacyComposite,
			in: EditFieldInput{FieldID: "f1", IsRequired: &no, DefaultVal: &legacyComposite}},
		{name: "user: relabel, stored default resent unchanged → OK", fieldType: "user", legacy: legacyUser,
			in: EditFieldInput{FieldID: "f1", Label: &label, DefaultVal: &legacyUser}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := metadata.FieldDefinition{ID: "f1", Name: "profile", FieldType: tt.fieldType, IsRequired: true, DefaultVal: tt.legacy}
			_, _, err := ValidateEditFieldChanges(stored, tt.in, nil, nil)
			if got := codeOf(err); got != tt.wantCode {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tt.wantCode)
			}
		})
	}
}

// ── backward-compat: existing valid payloads still pass ───────────────────────

func TestBackwardCompat_Wave3(t *testing.T) {
	// number with no options: any float still passes
	f := FieldSpec{Name: "x", FieldType: "number"}
	if err := checkType(f, float64(-999999)); err != nil {
		t.Errorf("number no-opts: unexpected error %v", err)
	}
	// text with no options: any string still passes
	f2 := FieldSpec{Name: "s", FieldType: "text"}
	if err := checkType(f2, "anything"); err != nil {
		t.Errorf("text no-opts: unexpected error %v", err)
	}
	// currency with no precision: any decimal places OK
	f3 := FieldSpec{Name: "p", FieldType: "currency"}
	if err := checkType(f3, map[string]any{"amount": 1.23456789, "currencyCode": "USD"}); err != nil {
		t.Errorf("currency no-precision: unexpected error %v", err)
	}
	// composite with no required subs: all absent still passes
	f4 := compositeFieldWith([]map[string]any{
		{"name": "a", "fieldType": "text"},
	})
	if err := checkType(f4, map[string]any{}); err != nil {
		t.Errorf("composite no required: unexpected error for empty object: %v", err)
	}
}
