package services

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Default rating bounds applied when options.min / options.max are not declared on the
// field. They mirror the schema-side default scale (validateRatingConfig allows max 2..10).
const (
	defaultRatingMin = 1
	defaultRatingMax = 5
)

// FieldSpec is a persisted field definition (from sys_fields) used to validate the
// dynamic records of an entity. It is the record-time counterpart of FieldInput.
type FieldSpec struct {
	Name string
	// Label is the field's human display name (sys_fields.label) — what the admin typed in the
	// builder, in the tenant's language. Validation never reads it; it is carried here because
	// toFieldSpecs is the single conversion point every read path already runs, so a caller that
	// must show a column heading (report run, CSV header) has the label without a second
	// metadata round-trip. May be empty for a field saved before labels were required — every
	// consumer falls back to Name.
	Label      string
	FieldType  string
	IsRequired bool
	// IsUnique signals that the field has a physical generated column named after the field
	// (unique fields) or <field>_id (relation fields). The service uses this to build the
	// sort allowlist (RM-09) and to rewrite filter predicates to the generated column for
	// indexed lookups (RM-13).
	IsUnique bool
	// IsIndexed signals that the field has a non-unique STORED generated column (D1) typed
	// per the D1 design table (text→VARCHAR, number/rating/currency→DECIMAL,
	// date/datetime→DATETIME). When true, the field is added to the sort allowlist (RM-09)
	// and its filters are routed to the physical column instead of JSON_EXTRACT (RM-13),
	// making sort and range filters use the B-tree index. Ignored when IsUnique is also true
	// — the unique column already provides a physical index.
	IsIndexed bool
	// DefaultVal is the field's persisted default (sys_fields.default_value), stored as a
	// string regardless of field type. When a record payload omits the field (or supplies an
	// empty value) and DefaultVal is non-empty, ValidateRecord fills it in — coerced to the
	// field's Go type — BEFORE the required check, so a defaulted field satisfies required.
	DefaultVal string
	Options    map[string]any
}

// emailPattern is a deliberately loose RFC-ish check — enough to reject obvious
// non-emails without pretending to fully validate addresses.
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// W5-B2/B3: physical-column bounds for fields that materialize a STORED generated column
// (unique OR indexed). The DDL builder types these columns as VARCHAR(255) for string-ish
// fields and DECIMAL(38,10) for numeric fields, so a value the JSON `data` column accepts
// can still overflow the generated column and trip MySQL 1406 (too long) / 1264 (out of
// range) — which surfaces as an opaque 500. We pre-reject at record-time (422) so a value
// is never accepted only to blow up at the storage layer. Plain JSON-only fields (neither
// unique nor indexed) have no generated column and are NOT subject to these caps.
const (
	// genColTextMaxRunes is the VARCHAR(255) cap on a unique/indexed string generated column.
	genColTextMaxRunes = 255
	// genColDecimalMaxAbs is the magnitude bound for a DECIMAL(38,10) generated column:
	// 38 total digits, 10 after the point → 28 integer digits → |v| < 1e28.
	genColDecimalMaxAbs = 1e28
	// genColDecimalScale is the number of fractional digits a DECIMAL(38,10) column keeps.
	genColDecimalScale = 10
)

// hasGenColumn reports whether the field materializes a physical STORED generated column
// (unique OR indexed) and is therefore subject to the genCol* physical-storage bounds.
func hasGenColumn(f FieldSpec) bool { return f.IsUnique || f.IsIndexed }

// checkGenColumnText rejects a string value that would overflow a unique/indexed field's
// VARCHAR(255) generated column (W5-B2). Applies only to fields with a generated column.
func checkGenColumnText(f FieldSpec, s string) error {
	if !hasGenColumn(f) {
		return nil
	}
	if utf8.RuneCountInString(s) > genColTextMaxRunes {
		return invalid("RECORD_TOO_LONG",
			"field %q vượt độ dài tối đa %d ký tự cho cột unique/indexed", f.Name, genColTextMaxRunes)
	}
	return nil
}

// checkGenColumnDecimal rejects a numeric value that would overflow a unique/indexed field's
// DECIMAL(38,10) generated column (W5-B3): magnitude ≥ 1e28, more than 10 fractional digits,
// or a non-finite value (NaN/±Inf). Applies only to fields with a generated column.
func checkGenColumnDecimal(f FieldSpec, n float64) error {
	if !hasGenColumn(f) {
		return nil
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return invalid("RECORD_INVALID_RANGE",
			"field %q có giá trị không hữu hạn (NaN/Inf) không lưu được vào cột số", f.Name)
	}
	if math.Abs(n) >= genColDecimalMaxAbs {
		return invalid("RECORD_INVALID_RANGE",
			"field %q có giá trị %v vượt giới hạn DECIMAL(38,10) (|v| < 1e28)", f.Name, n)
	}
	// Reject more than 10 fractional digits — but only where float64 can actually represent
	// fractions. Beyond ~2^52 a float64 has no fractional bits at all (it is already an exact
	// integer multiple of a power of two), and scaling by 10^10 would overflow precision and
	// false-reject legitimate large integers that DECIMAL(38,10) stores fine. So the scale
	// check is meaningful only for |n| below that threshold; larger magnitudes are integers
	// to float64 and pass the scale constraint trivially.
	if math.Abs(n) < 1e15 {
		factor := math.Pow(10, genColDecimalScale)
		scaled := n * factor
		if math.Abs(scaled-math.Round(scaled)) > 1e-3 {
			return invalid("RECORD_INVALID_RANGE",
				"field %q có quá %d chữ số thập phân cho cột DECIMAL(38,10)", f.Name, genColDecimalScale)
		}
	}
	return nil
}

// patternCache memoizes compiled *regexp.Regexp keyed by the anchored pattern string (O5).
// Without it, checkTextConstraints recompiled options.pattern on EVERY record write — recompiling
// the same RE2 program once per row in a bulk insert. A *regexp.Regexp is safe for concurrent
// MatchString and sync.Map handles concurrent Load/Store, so the compile happens once per distinct
// pattern process-wide. The key space is bounded by the number of admin-configured field patterns
// (small, not attacker-controlled). Only successful compiles are cached; a (rare) bad pattern is
// re-attempted each time and fails closed.
var patternCache sync.Map // map[string]*regexp.Regexp

// compileAnchoredPattern returns the compiled, full-match-anchored regexp for a raw options.pattern,
// memoized via patternCache. The anchoring ("^(?:…)$") matches the field-def-time validation
// (validateTextConfig) so record-time and config-time agree on full-match semantics.
func compileAnchoredPattern(raw string) (*regexp.Regexp, error) {
	anchored := "^(?:" + raw + ")$"
	if v, ok := patternCache.Load(anchored); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(anchored)
	if err != nil {
		return nil, err
	}
	patternCache.Store(anchored, re)
	return re, nil
}

// computedReadonlyCode reports, for a field type that is never client-writable, the
// ValidationError code a client write attempt must be rejected with — and whether fieldType is
// readonly-from-client at all. Centralizes the formula/lookup/rollup/auto_number readonly
// contract so ValidateRecord, ValidatePartialRecord and checkType's safety net can never drift on
// which types are readonly or which code each maps to.
//
// Despite the name, not every type here is "computed" in the same sense: formula is computed at
// write time (applyComputedFields); lookup/rollup are computed fresh at READ time and never land
// in `data` at all (ENG-COMPUTED #2/#3); auto_number (ENG #6) is neither — it IS a plain
// server-ASSIGNED value written to `data` exactly once, at Create time only, by
// services.assignSequences (record_orchestration.go) — never by applyComputedFields, and never
// re-assigned on Update. All four share the one property this function exists to centralize:
// a client payload may never set them directly.
// assignTargetTypeOK reports whether a workflow `assign` action may write a userId into a field
// of this type. TWO types qualify and the pair is deliberate:
//
//   - "user"  — the correct one. Write-time it must be a live ACTIVE user of the tenant, and
//     ?expand= turns it into a name on screen.
//   - "text"  — legacy. It was the ONLY option before the `user` type existed, so every pack
//     and every tenant installed to date routes into a text field. Dropping it would silently
//     break their routing at fire time.
//
// Shared by the config-time validator (buildWorkflowRule) and the dry-run preview so the two can
// never disagree about what a valid target is — the parity rule the project already applies to
// every other validator.
func assignTargetTypeOK(fieldType string) bool {
	return fieldType == "user" || fieldType == "text"
}

func computedReadonlyCode(fieldType string) (code string, readonly bool) {
	switch fieldType {
	case "formula":
		return "RECORD_FORMULA_READONLY", true
	case "lookup":
		return "RECORD_LOOKUP_READONLY", true
	case "rollup":
		return "RECORD_ROLLUP_READONLY", true
	case "auto_number":
		return "RECORD_AUTONUMBER_READONLY", true
	default:
		return "", false
	}
}

// ComputedReadonlyFieldType is the exported predicate over the SAME source of truth as
// computedReadonlyCode: a field whose value is server-assigned / read-computed and may never be
// set by a client payload. Exposed so cross-package guards (the vertical-pack sample-export strip)
// can assert parity against this list instead of duplicating it (security 🔵-1 drift guard).
func ComputedReadonlyFieldType(fieldType string) bool {
	_, ro := computedReadonlyCode(fieldType)
	return ro
}

// ValidateRecord checks a record payload against an entity's field specs and returns a
// cleaned data map containing only declared fields. It is strict: any key not declared
// in fields is rejected (RECORD_UNKNOWN_FIELD) so junk cannot leak into the data JSON.
func ValidateRecord(fields []FieldSpec, payload map[string]any) (map[string]any, error) {
	return validateRecord(fields, payload, nil)
}

// validateRecord is ValidateRecord for a caller that may not VIEW some fields (hidden): a payload
// key naming one of them is refused exactly like a key the entity does not declare (queue #317,
// api-conventions §4.4b). The required/default checks below still see the full field list — a hidden
// required field is the create path's #143 business, not this loop's. hidden == nil is ValidateRecord.
func validateRecord(fields []FieldSpec, payload map[string]any, hidden map[string]struct{}) (map[string]any, error) {
	clean, errs := validateRecordAll(fields, payload, hidden)
	if len(errs) > 0 {
		return nil, errs[0]
	}
	return clean, nil
}

// validateRecordAll is validateRecord's engine: it returns EVERY problem of the payload, not just the
// first (queue #362 — the CSV preview names all of a row's bad cells at once). validateRecord keeps the
// record routes' contract by taking errs[0]. Undeclared keys are judged alone (the rest of the image
// is meaningless around them); the cross-field checks run only on an otherwise clean image.
func validateRecordAll(fields []FieldSpec, payload map[string]any, hidden map[string]struct{}) (map[string]any, []error) {
	byName := make(map[string]FieldSpec, len(fields))
	for _, f := range fields {
		byName[f.Name] = f
	}

	// Strict: reject any undeclared key up front.
	// Also reject formula/lookup/rollup fields explicitly supplied by the client — they are
	// always server-computed and must never be set from a payload (RECORD_FORMULA_READONLY /
	// RECORD_LOOKUP_READONLY / RECORD_ROLLUP_READONLY — ENG-COMPUTED #2/#3 mirror formula).
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys) // deterministic: the same payload always reports the same first error
	var errs []error
	for _, key := range keys {
		f, ok := byName[key]
		if _, isHidden := hidden[key]; !ok || isHidden {
			errs = append(errs, unknownFieldError(key))
			continue
		}
		if code, readonly := computedReadonlyCode(f.FieldType); readonly {
			errs = append(errs, invalidField(code, key,
				"field %q là computed field (%s) — không thể ghi từ client", key, f.FieldType))
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}

	clean := make(map[string]any, len(payload))
	var defaulted map[string]bool // fields filled from their default (queue #255)
	for _, f := range fields {
		// Formula/lookup/rollup fields are always server-computed; skip them here entirely.
		// applyComputedFields (formula) / applyLookups (lookup) / rollup.go (rollup, read-time
		// only) fill them in after validation / at read time — never from this clean-loop.
		if _, readonly := computedReadonlyCode(f.FieldType); readonly {
			continue
		}
		val, present := payload[f.Name]
		if !present || isEmpty(val) {
			// The payload omitted the field (or sent an empty value). If the field declares a
			// default, materialize it — coerced to the field's Go type — so the default both
			// satisfies a required check and lands in the stored data. A default that fails
			// coercion (e.g. non-numeric default on a number field) is a schema bug; surface it
			// as RECORD_INVALID_TYPE rather than silently dropping the value.
			if dv, ok, err := defaultValue(f); err != nil {
				errs = append(errs, err)
				continue
			} else if ok {
				clean[f.Name] = dv
				if defaulted == nil {
					defaulted = map[string]bool{}
				}
				defaulted[f.Name] = true
				continue
			}
			// #350: file/image can only be uploaded once the record exists (files API), so "required"
			// cannot be judged at create — enforcing it made every create 422 for such entities.
			if f.IsRequired && f.FieldType != "file" && f.FieldType != "image" {
				errs = append(errs, invalidField("RECORD_REQUIRED_MISSING", f.Name, "field %q là bắt buộc", f.Name))
			}
			continue
		}
		if err := checkType(f, val); err != nil {
			errs = append(errs, withField(err, f.Name))
			continue
		}
		clean[f.Name] = normalizeForStore(f, val)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	// specs/052 / queue #255: a child default the parent in the create image does not allow is
	// skipped (as Salesforce does), not judged — the user left that cell blank.
	if name := dropDisallowedChildDefaults(fields, clean, defaulted); name != "" {
		return nil, []error{invalidField("RECORD_REQUIRED_MISSING", name, "field %q là bắt buộc", name)}
	}
	// specs/052: dependent picklist pairs, judged on the full create image (defaults included).
	if err := checkDependentPicklists(fields, clean, nil); err != nil {
		return nil, []error{err}
	}
	return clean, nil
}

// ValidatePartialRecord validates a PUT payload under apply-when-present (partial-merge)
// semantics: only the keys ACTUALLY present in the body are validated and returned. Unlike
// ValidateRecord it does NOT fill defaults for absent fields and does NOT enforce isRequired
// on absent fields — an omitted field is left untouched by the caller's merge, so it can be
// neither defaulted (defaults are a create-time concern, ../specs/040-dynamic-entity-engine/spec.md §4.3.1) nor
// reported missing (it already has a stored value). It stays strict on the keys that ARE
// present: undeclared keys are rejected (RECORD_UNKNOWN_FIELD) and present values are
// type-checked. A present-but-empty value (nil / blank string / [] / {}) on a required field
// is rejected (RECORD_REQUIRED_MISSING) so a client cannot blank out a required field through
// a partial update.
func ValidatePartialRecord(fields []FieldSpec, payload map[string]any) (map[string]any, error) {
	return validatePartialRecord(fields, payload, nil)
}

// validatePartialRecord is ValidatePartialRecord for a caller that may not VIEW some fields
// (hidden): see validateRecord.
func validatePartialRecord(fields []FieldSpec, payload map[string]any, hidden map[string]struct{}) (map[string]any, error) {
	byName := make(map[string]FieldSpec, len(fields))
	for _, f := range fields {
		byName[f.Name] = f
	}

	clean := make(map[string]any, len(payload))
	for key, val := range payload {
		f, ok := byName[key]
		if _, isHidden := hidden[key]; !ok || isHidden {
			return nil, unknownFieldError(key)
		}
		// Formula/lookup/rollup fields are always server-computed — reject any client attempt
		// to set them (ENG-COMPUTED #2/#3 mirror formula's RECORD_FORMULA_READONLY).
		if code, readonly := computedReadonlyCode(f.FieldType); readonly {
			return nil, invalidField(code, key,
				"field %q là computed field (%s) — không thể ghi từ client", key, f.FieldType)
		}
		// D2 — partial-PUT clear semantics. Distinguish a key the client sent with an
		// EXPLICIT JSON null (intent: clear the field) from a key sent empty in some other
		// way (blank string / [] / {}). Only an explicit null clears an optional field.
		//   - required + empty (null or otherwise) → 422 (a client can't blank a required field).
		//   - optional + explicit null            → forward nil so JSON_MERGE_PATCH removes the key.
		//   - optional + other-empty ("", [], {}) → skip (legacy no-op; not a clear).
		// A key ABSENT from the payload never enters this loop, so it is preserved by the merge.
		if val == nil {
			if f.IsRequired {
				return nil, invalidField("RECORD_REQUIRED_MISSING", f.Name, "field %q là bắt buộc", f.Name)
			}
			// Explicit null on an optional field: emit it so the store's JSON_MERGE_PATCH
			// receives {field: null}, which MySQL interprets as "remove this key".
			clean[f.Name] = nil
			continue
		}
		if isEmpty(val) {
			// The key was sent explicitly but carries a non-null empty value (blank string,
			// [] or {}). Blanking a required field is rejected; for an optional field this is
			// a no-op for the merge (we skip it — to CLEAR, send an explicit null instead).
			if f.IsRequired {
				return nil, invalidField("RECORD_REQUIRED_MISSING", f.Name, "field %q là bắt buộc", f.Name)
			}
			continue
		}
		if err := checkType(f, val); err != nil {
			return nil, withField(err, f.Name)
		}
		clean[f.Name] = normalizeForStore(f, val)
	}
	return clean, nil
}

// defaultValue resolves a field's persisted default into a typed value ready for the data map.
// It reports (value, true, nil) when the field declares a non-empty default coerced to its Go
// type, (nil, false, nil) when there is no default to apply, and a RECORD_INVALID_TYPE error
// when the stored default cannot be coerced to the field's type (a schema defect). Scalar
// primitive types support defaults: number, rating (validated through checkType so an
// out-of-range/non-integer default is rejected), boolean, and the string-valued types. The
// remaining nested-JSON types (multi_select/currency/composite) carry no string-encodable
// default and are skipped.
func defaultValue(f FieldSpec) (any, bool, error) {
	if strings.TrimSpace(f.DefaultVal) == "" {
		return nil, false, nil
	}
	raw := f.DefaultVal
	switch f.FieldType {
	case "number":
		n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, false, invalid("RECORD_INVALID_TYPE", "field %q có default không phải số", f.Name)
		}
		return n, true, nil
	case "rating":
		// A rating default must clear the SAME bar as an explicitly-sent rating value:
		// integer-valued and within [min, max]. Parse to float64 first, then run it through
		// checkType (→ checkRating) so an out-of-range or non-integer schema default is
		// rejected at field-edit time instead of silently persisting junk into a record.
		n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, false, invalid("RECORD_INVALID_TYPE", "field %q có default không phải số", f.Name)
		}
		if err := checkType(f, n); err != nil {
			return nil, false, err
		}
		return n, true, nil
	case "boolean":
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return nil, false, invalid("RECORD_INVALID_TYPE", "field %q có default không phải boolean", f.Name)
		}
		return b, true, nil
	case "text", "email", "picklist", "date", "datetime", "relation", "rich_text":
		// String-valued types: the default is applied verbatim and still passes checkType
		// (picklist membership, email shape, …) below.
		if err := checkType(f, raw); err != nil {
			return nil, false, err
		}
		return raw, true, nil
	case "multi_select":
		// RM-33 (b): multi_select default is stored as a JSON array string (e.g. `["a","b"]`).
		// Decode it, validate each element is in the picklist, then return as []any.
		// Design choice: any element outside the picklist is rejected at config-edit time
		// (EditField re-runs defaultValue) — it is a schema defect, not a user error.
		var items []string
		if err := json.Unmarshal([]byte(raw), &items); err != nil {
			return nil, false, invalid("RECORD_INVALID_TYPE",
				"field %q có default multi_select không phải mảng JSON hợp lệ", f.Name)
		}
		result := make([]any, len(items))
		for i, item := range items {
			if !picklistContains(f.Options, item) {
				return nil, false, invalid("RECORD_INVALID_PICKLIST",
					"field %q có default %q ngoài danh sách cho phép", f.Name, item)
			}
			result[i] = item
		}
		return result, true, nil
	case "formula":
		// Formula fields are always server-computed — they have no stored default and
		// are never populated from the DefaultVal string. applyComputedFields fills them.
		return nil, false, nil
	default:
		// currency and composite have no scalar-encodable default form.
		// RM-33 (a): currency/rich_text defaults are rejected at field-def time
		// (validateFieldConfig), so they never reach here with a non-empty DefaultVal.
		return nil, false, nil
	}
}

// defaultFills reports whether ValidateRecord fills f from its default when a create payload
// omits it — the SAME defaultValue call, so a guard asking "can the engine satisfy this required
// field without the caller writing it?" (the field-ACL lock guard, queue #62) cannot drift from
// what Create actually does. A default that fails coercion fills nothing: Create 422s on it.
func defaultFills(f FieldSpec) bool {
	_, ok, err := defaultValue(f)
	return ok && err == nil
}

// isEmpty treats nil, whitespace-only strings, empty arrays ([]) and empty objects ({}) as
// "not provided" so a required field can't be satisfied by blanks. The array/object cases close
// a required-bypass: a required multi_select sent as [] or a required composite/currency sent as
// {} previously slipped through (only nil/blank-string were caught), letting empty structured
// values masquerade as present.
func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	default:
		return false
	}
}

// checkType enforces the value's Go type against the field type. JSON numbers arrive as
// float64 and booleans as bool, which is what the dynamic handler will hand us.
func checkType(f FieldSpec, val any) error {
	switch f.FieldType {
	case "number":
		n, ok := val.(float64)
		if !ok {
			return invalid("RECORD_INVALID_TYPE", "field %q phải là số", f.Name)
		}
		// W5-B3: a unique/indexed number maps to a DECIMAL(38,10) generated column — reject
		// a value that would overflow it (1264) before the storage layer 500s.
		if err := checkGenColumnDecimal(f, n); err != nil {
			return err
		}
		// RM-29: enforce options.min / options.max / options.step when declared.
		return checkNumberRange(f, n)
	case "boolean":
		if _, ok := val.(bool); !ok {
			return invalid("RECORD_INVALID_TYPE", "field %q phải là boolean", f.Name)
		}
	case "text":
		s, ok := val.(string)
		if !ok {
			return invalid("RECORD_INVALID_TYPE", "field %q phải là chuỗi", f.Name)
		}
		// W5-B2: a unique/indexed text maps to a VARCHAR(255) generated column — reject an
		// over-255-rune value (1406) before the storage layer 500s.
		if err := checkGenColumnText(f, s); err != nil {
			return err
		}
		// RM-29: enforce options.maxLength and options.pattern when declared.
		return checkTextConstraints(f, s)
	case "relation":
		if _, ok := val.(string); !ok {
			return invalid("RECORD_INVALID_TYPE", "field %q phải là chuỗi", f.Name)
		}
	case "user":
		// USER-FIELD: same DB-free shape check as relation (a userId string). Existence — the
		// id must resolve to an ACTIVE user in the SAME tenant — is a separate probe
		// (checkUserTargets, record_orchestration.go), mirroring relation's checkType/
		// checkRelationTargets split.
		if _, ok := val.(string); !ok {
			return invalid("RECORD_INVALID_TYPE", "field %q phải là chuỗi", f.Name)
		}
	case "date":
		s, ok := val.(string)
		if !ok {
			return invalid("RECORD_INVALID_TYPE", "field %q phải là chuỗi", f.Name)
		}
		t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
		if err != nil {
			return invalid("RECORD_INVALID_DATE", "field %q không phải ngày hợp lệ (YYYY-MM-DD)", f.Name)
		}
		if t.Year() <= 0 { // reject the zero/negative-year date MySQL would coerce under NO_ZERO_DATE (RM-48)
			return invalid("RECORD_INVALID_DATE", "field %q có năm không hợp lệ", f.Name)
		}
	case "datetime":
		s, ok := val.(string)
		if !ok {
			return invalid("RECORD_INVALID_TYPE", "field %q phải là chuỗi", f.Name)
		}
		if !isValidDatetime(strings.TrimSpace(s)) {
			return invalid("RECORD_INVALID_DATE", "field %q không phải datetime hợp lệ (ISO-8601/RFC3339)", f.Name)
		}
	case "email":
		s, ok := val.(string)
		if !ok {
			return invalid("RECORD_INVALID_TYPE", "field %q phải là chuỗi", f.Name)
		}
		s = strings.TrimSpace(s) // #365: judge (and store, see normalizeForStore) the trimmed form
		if !emailPattern.MatchString(s) {
			return invalid("RECORD_INVALID_EMAIL", "field %q không phải email hợp lệ", f.Name)
		}
		// W5-B2: a unique/indexed email maps to a VARCHAR(255) generated column.
		if err := checkGenColumnText(f, s); err != nil {
			return err
		}
	case "picklist":
		s, ok := val.(string)
		if !ok {
			return invalid("RECORD_INVALID_TYPE", "field %q phải là chuỗi", f.Name)
		}
		if !picklistContains(f.Options, s) {
			return invalid("RECORD_INVALID_PICKLIST", "field %q có giá trị %q ngoài danh sách", f.Name, s)
		}
		// W5-B2: a unique/indexed picklist maps to a VARCHAR(255) generated column.
		if err := checkGenColumnText(f, s); err != nil {
			return err
		}
	case "rich_text":
		s, ok := val.(string)
		if !ok {
			return invalid("RECORD_INVALID_TYPE", "field %q phải là chuỗi", f.Name)
		}
		// RM-29: rich_text shares maxLength / pattern enforcement with text.
		return checkTextConstraints(f, s)
	case "multi_select":
		return checkMultiSelect(f, val)
	case "currency":
		return checkCurrency(f, val)
	case "rating":
		return checkRating(f, val)
	case "composite":
		return checkComposite(f, val)
	case "polymorphic_relation":
		return checkPolymorphic(f, val)
	case "file", "image":
		return checkFile(f, val)
	case "formula", "lookup", "rollup", "auto_number":
		// Formula/lookup/rollup/auto_number fields are server-computed/assigned and must never
		// be validated as client input. The guard in ValidateRecord / ValidatePartialRecord
		// rejects them before checkType is reached; this case is a final safety net (e.g. if
		// called from the defaultValue path).
		code, _ := computedReadonlyCode(f.FieldType)
		return invalidField(code, f.Name,
			"field %q là computed field (%s) — không thể ghi từ client", f.Name, f.FieldType)
	}
	return nil
}

// checkNumberRange enforces options.min / options.max / options.step for a number field
// (RM-29). Each bound is only applied when declared in options; absent options are skipped
// for backward-compatibility. step is enforced only when present and positive; the check
// uses an epsilon to avoid float64 modulo false-rejects.
func checkNumberRange(f FieldSpec, n float64) error {
	if f.Options == nil {
		return nil
	}
	if minRaw, ok := f.Options["min"].(float64); ok {
		if n < minRaw {
			return invalid("RECORD_INVALID_RANGE",
				"field %q có giá trị %v nhỏ hơn min %v", f.Name, n, minRaw)
		}
	}
	if maxRaw, ok := f.Options["max"].(float64); ok {
		if n > maxRaw {
			return invalid("RECORD_INVALID_RANGE",
				"field %q có giá trị %v lớn hơn max %v", f.Name, n, maxRaw)
		}
	}
	if stepRaw, ok := f.Options["step"].(float64); ok && stepRaw > 0 {
		// Compute how far n is from the nearest multiple of step.
		// Use epsilon = step * 1e-9 to tolerate float64 rounding.
		remainder := math.Mod(math.Abs(n), stepRaw)
		if remainder > stepRaw*1e-9 && (stepRaw-remainder) > stepRaw*1e-9 {
			return invalid("RECORD_INVALID_RANGE",
				"field %q có giá trị %v không phải bội số của step %v", f.Name, n, stepRaw)
		}
	}
	return nil
}

// checkTextConstraints enforces options.maxLength (rune count, not byte length) and
// options.pattern (anchored full-match regex) for text and rich_text fields (RM-29).
// Each constraint is only applied when declared in options.
func checkTextConstraints(f FieldSpec, s string) error {
	if f.Options == nil {
		return nil
	}
	if mlRaw, ok := f.Options["maxLength"].(float64); ok && mlRaw > 0 {
		if utf8.RuneCountInString(s) > int(mlRaw) {
			return invalid("RECORD_TOO_LONG",
				"field %q vượt độ dài tối đa %d ký tự", f.Name, int(mlRaw))
		}
	}
	if patRaw, ok := f.Options["pattern"].(string); ok && patRaw != "" {
		// Pattern is stored as a raw regex; compile once (cached) with full-match anchoring.
		re, err := compileAnchoredPattern(patRaw)
		if err != nil {
			// Config defect: bad pattern was not caught at field-def time (should not happen
			// after RM-29 config-time validation). Fail safe: reject the value.
			return invalid("RECORD_INVALID_PATTERN",
				"field %q có pattern không hợp lệ trong cấu hình", f.Name)
		}
		if !re.MatchString(s) {
			return invalid("RECORD_INVALID_PATTERN",
				"field %q không khớp pattern yêu cầu", f.Name)
		}
	}
	return nil
}

// datetimeLayouts are the accepted datetime encodings: RFC3339 (with timezone offset, the
// canonical wire form) and the timezone-less local form many frontends emit from a
// datetime-local input. A value matching either layout is accepted; anything else is junk.
var datetimeLayouts = []string{time.RFC3339, "2006-01-02T15:04:05"}

// isValidDatetime reports whether s parses under any accepted datetime layout AND has a real
// (positive) year — rejecting the zero/negative-year instant MySQL would otherwise coerce under
// NO_ZERO_DATE (RM-48).
func isValidDatetime(s string) bool {
	for _, layout := range datetimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Year() > 0
		}
	}
	return false
}

// mysqlDatetimeLayout is the MySQL DATETIME literal form ("2006-01-02 15:04:05"): no 'T'
// separator, no 'Z'/offset suffix. CAST(... AS DATETIME) parses this cleanly, whereas an
// RFC3339 string with a 'T'/'Z'/offset FAILS the cast under STRICT_TRANS_TABLES (Error 1292
// — see indexed_datetime_blocker_integration_test.go). Canonicalizing every datetime to this
// UTC form is what makes both store-time values and filter operands instant-comparable.
const mysqlDatetimeLayout = "2006-01-02 15:04:05"

// canonicalDatetimeUTC converts an accepted datetime string (RFC3339 with offset, or the
// timezone-less local form) into the MySQL DATETIME literal in UTC ("2006-01-02 15:04:05").
//
// D1: this is the linchpin of instant-based datetime equality/ordering. Two RFC3339 values
// that denote the SAME instant in different offsets ("2026-06-01T12:00:00+07:00" and
// "2026-06-01T05:00:00Z") both canonicalize to "2026-06-01 05:00:00", so eq matches and
// gt/gte/lt/lte order by instant rather than by raw bytes. A timezone-less value is
// interpreted as UTC (time.Parse defaults to UTC), giving a stable, offset-free literal that
// MySQL's CAST(... AS DATETIME) accepts. Returns (canonical, true) on success; (s, false) for
// an unparseable string so the caller leaves it untouched (validation rejects it elsewhere).
func canonicalDatetimeUTC(s string) (string, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range datetimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(mysqlDatetimeLayout), true
		}
	}
	return s, false
}

// normalizeForStore canonicalizes a validated value before it lands in the data JSON. checkType
// validates date/datetime on the TRIMMED string, so the stored value must be trimmed too —
// otherwise " 2026-01-01 " is accepted but persisted verbatim and re-read inconsistently (RM-48).
// Non-date types and non-string values pass through unchanged.
func normalizeForStore(f FieldSpec, val any) any {
	switch f.FieldType {
	case "date", "email": // email: #365, checkType judges the trimmed form
		if s, ok := val.(string); ok {
			return strings.TrimSpace(s)
		}
	case "datetime":
		// D1: canonicalize datetime to the UTC MySQL literal ("2006-01-02 15:04:05") so the
		// stored value is offset-free and CAST(... AS DATETIME)-safe, and two same-instant
		// values in different offsets persist identically. This makes eq/neq and ordered
		// filters compare by instant. An unparseable string (already rejected by checkType)
		// falls back to the trimmed input.
		if s, ok := val.(string); ok {
			if canon, ok := canonicalDatetimeUTC(s); ok {
				return canon
			}
			return strings.TrimSpace(s)
		}
	}
	return val
}

// checkMultiSelect enforces a multi_select value: it must be a JSON array ([]any) and every
// element must be a string present in options.values (RECORD_INVALID_PICKLIST). An empty
// array is accepted — emptiness is handled upstream by the required-field check.
func checkMultiSelect(f FieldSpec, val any) error {
	arr, ok := val.([]any)
	if !ok {
		return invalid("RECORD_INVALID_TYPE", "field %q phải là mảng", f.Name)
	}
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			return invalid("RECORD_INVALID_TYPE", "field %q có phần tử không phải chuỗi", f.Name)
		}
		if !picklistContains(f.Options, s) {
			return invalid("RECORD_INVALID_PICKLIST", "field %q có giá trị %q ngoài danh sách", f.Name, s)
		}
	}
	return nil
}

// checkCurrency enforces a currency value: it must be a JSON object carrying a numeric
// `amount` (float64) and a non-empty string `currencyCode` that equals the field's configured
// options.currencyCode when one is set (RECORD_CURRENCY_MISMATCH — queue #7, see below).
//
// RM-31 (revised 2026-06-25): the amount is UNBOUNDED by default — negatives are a
// first-class accounting concept (refunds, credit notes, write-offs, debit/credit), not an
// error. A platform-wide `amount >= 0` floor would make an accounting/ERP vertical impossible,
// contradicting the product-agnostic charter. An admin opts into a floor/ceiling PER FIELD via
// options.min / options.max — e.g. set min:0 on a "unit price" field to forbid negatives —
// mirroring how rating resolves its bounds (see checkRating / currencyBounds). Evidence:
// Dataverse MoneyAttributeMetadata (signed range, per-column MinValue), Salesforce (non-negative
// is an admin validation rule, not a field type), Airtable (per-field "allow negative" toggle),
// SQL Server / PostgreSQL money (signed by definition), ISO 4217 (silent on sign).
//
// When options.precision is declared (integer in [0,8]), amount must not carry more decimal
// places than allowed (RECORD_INVALID_PRECISION). Design choice: REJECT, not round — silently
// rounding would violate the "never accept then store wrong" theme. Precision is checked by
// multiplying by 10^precision and comparing to the rounded value within a float64 epsilon
// (1e-9 * 10^precision). Sign and precision are orthogonal.
func checkCurrency(f FieldSpec, val any) error {
	m, ok := val.(map[string]any)
	if !ok {
		return invalid("RECORD_INVALID_TYPE", "field %q phải là object {amount, currencyCode}", f.Name)
	}
	amount, ok := m["amount"].(float64)
	if !ok {
		return invalid("RECORD_INVALID_TYPE", "field %q cần amount là số", f.Name)
	}
	// W5-B3: a unique/indexed currency maps to a DECIMAL(38,10) generated column (its amount
	// projection). Reject an amount that would overflow it before the storage layer 500s.
	if err := checkGenColumnDecimal(f, amount); err != nil {
		return err
	}
	// RM-31 (revised): no hardcoded sign floor — enforce only the per-field bounds an admin
	// declared via options.min / options.max. Absent bounds → full signed range allowed.
	if min, hasMin, max, hasMax := currencyBounds(f.Options); hasMin || hasMax {
		if hasMin && amount < min {
			return invalid("RECORD_INVALID_RANGE",
				"field %q có amount %v nhỏ hơn min %v cho phép", f.Name, amount, min)
		}
		if hasMax && amount > max {
			return invalid("RECORD_INVALID_RANGE",
				"field %q có amount %v lớn hơn max %v cho phép", f.Name, amount, max)
		}
	}
	code, ok := m["currencyCode"].(string)
	if !ok || strings.TrimSpace(code) == "" {
		return invalid("RECORD_INVALID_TYPE", "field %q cần currencyCode là chuỗi không rỗng", f.Name)
	}
	// Queue #7 (Q7, Founder 2026-09-05): the row's code must EQUAL the field's configured
	// options.currencyCode. Every reader (aggregate, rollup, computed, hook) labels with the
	// configured code and sums `.amount` blind, so a USD row inside a VND field is a silently
	// wrong total. REJECT the row — never relabel it (USD 100 rewritten to VND 100 is the exact
	// wrong number this guard exists to prevent) and never convert (no FX source). Skipped when
	// the field carries no configured code (legacy sys_fields row / hand-built spec): nothing to
	// compare against. validateCurrencyConfig pins the configured code to 3 uppercase letters, so
	// an exact comparison is the right one — "vnd" is a mismatch, not a case to fold.
	if want, _ := f.Options["currencyCode"].(string); want != "" && strings.TrimSpace(code) != want {
		return invalidField("RECORD_CURRENCY_MISMATCH", f.Name,
			"field %q cấu hình mã tiền tệ %q nhưng dòng gửi %q — dòng bị từ chối, không tự đổi mã",
			f.Name, want, code)
	}
	// RM-31: enforce options.precision when declared.
	if f.Options != nil {
		if precRaw, ok := f.Options["precision"].(float64); ok {
			prec := int(precRaw)
			if prec >= 0 && prec <= 8 {
				factor := math.Pow(10, float64(prec))
				rounded := math.Round(amount * factor)
				if math.Abs(amount*factor-rounded) > factor*1e-9 {
					return invalid("RECORD_INVALID_PRECISION",
						"field %q có amount %v vượt quá %d chữ số thập phân cho phép", f.Name, amount, prec)
				}
			}
		}
	}
	return nil
}

// checkRating enforces a rating value: it must be an integer-valued float64 within the field's
// [min, max] bounds (RECORD_INVALID_RANGE). Bounds come from options.min / options.max when
// declared, otherwise the engine defaults (defaultRatingMin / defaultRatingMax).
func checkRating(f FieldSpec, val any) error {
	n, ok := val.(float64)
	if !ok || n != float64(int(n)) {
		return invalid("RECORD_INVALID_TYPE", "field %q phải là số nguyên", f.Name)
	}
	min, max := ratingBounds(f.Options)
	if n < float64(min) || n > float64(max) {
		return invalid("RECORD_INVALID_RANGE", "field %q có giá trị %v ngoài khoảng [%d, %d]", f.Name, n, min, max)
	}
	return nil
}

// ratingBounds resolves the [min, max] range for a rating field from its options, falling back
// to engine defaults for any bound not declared (or not an integer).
func ratingBounds(options map[string]any) (int, int) {
	min, max := defaultRatingMin, defaultRatingMax
	if options != nil {
		if v, ok := options["min"].(float64); ok && v == float64(int(v)) {
			min = int(v)
		}
		if v, ok := options["max"].(float64); ok && v == float64(int(v)) {
			max = int(v)
		}
	}
	return min, max
}

// currencyBounds resolves OPTIONAL [min, max] amount bounds for a currency field from its options.
// Unlike rating, currency has NO default bound — a currency amount is unbounded (full signed range)
// unless an admin explicitly declares options.min and/or options.max. hasMin/hasMax report whether
// the corresponding bound was declared, so the caller enforces only the bounds that exist (and
// negatives stay valid by default — see checkCurrency / RM-31 revision). Bounds accept any float64,
// including negative floors (e.g. min:-100) and fractional limits.
func currencyBounds(options map[string]any) (min float64, hasMin bool, max float64, hasMax bool) {
	if options != nil {
		if v, ok := options["min"].(float64); ok {
			min, hasMin = v, true
		}
		if v, ok := options["max"].(float64); ok {
			max, hasMax = v, true
		}
	}
	return min, hasMin, max, hasMax
}

// checkComposite enforces a composite value: it must be a JSON object whose keys match the
// sub-fields declared in options.subFields, and each present sub-value is validated by the
// sub-field's own type via checkType. Recursion terminates because the schema (validateComposite-
// Config) forbids composite/relation sub-fields (G3/G5), so a composite never nests a composite.
//
// RM-32: sub-fields with required:true in their config are now enforced — a required sub-field
// that is absent or empty produces RECORD_REQUIRED_MISSING with Field="<composite>.<sub>".
// Sub-required is only evaluated when the composite object itself is present (isEmpty is checked
// upstream by ValidateRecord / ValidatePartialRecord before checkType is called).
func checkComposite(f FieldSpec, val any) error {
	m, ok := val.(map[string]any)
	if !ok {
		return invalid("RECORD_INVALID_TYPE", "field %q phải là object", f.Name)
	}
	subs := compositeSubSpecs(f.Options)
	for key := range m {
		if _, ok := subs[key]; !ok {
			return invalid("RECORD_UNKNOWN_FIELD", "field %q có sub-field %q không khai trong metadata", f.Name, key)
		}
	}
	for subName, spec := range subs {
		sub, present := m[subName]
		if !present || isEmpty(sub) {
			// RM-32: required sub-field missing or empty → reject with qualified path.
			if spec.IsRequired {
				qualifiedName := f.Name + "." + subName
				return invalidField("RECORD_REQUIRED_MISSING", qualifiedName,
					"sub-field %q của %q là bắt buộc", subName, f.Name)
			}
			continue
		}
		if err := checkType(spec, sub); err != nil {
			return err
		}
	}
	return nil
}

// compositeSubSpecs converts options.subFields ([]{name, fieldType, required, options}) into
// a map of FieldSpec keyed by sub-field name. Malformed entries are skipped (the schema
// validator already rejected them at create time).
//
// RM-32: the optional `required` bool per sub-field entry is now extracted and propagated
// into FieldSpec.IsRequired so checkComposite can enforce it.
func compositeSubSpecs(options map[string]any) map[string]FieldSpec {
	out := map[string]FieldSpec{}
	if options == nil {
		return out
	}
	subs, _ := options["subFields"].([]any)
	for _, raw := range subs {
		sub, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := sub["name"].(string)
		if name == "" {
			continue
		}
		ft, _ := sub["fieldType"].(string)
		opts, _ := sub["options"].(map[string]any)
		// RM-32: extract the per-sub required flag; absent means optional (false).
		required, _ := sub["required"].(bool)
		out[name] = FieldSpec{Name: name, FieldType: ft, IsRequired: required, Options: opts}
	}
	return out
}

// checkPolymorphic validates a polymorphic_relation value (ACTIVITY-CORE #3a): an ATOMIC 2-key
// object {type, id} where type is one of options.targetEntities. It mirrors the composite
// atomic-write contract:
//   - value is not a map, or carries any key other than type/id → RECORD_INVALID_TYPE (wrong shape).
//   - a sub-value present but not a string → RECORD_INVALID_TYPE (the key IS present, so not "missing").
//   - type or id absent/blank → RECORD_REQUIRED_MISSING with details.field = "<f>.type" / "<f>.id".
//   - type not in options.targetEntities → RECORD_POLYMORPHIC_TARGET_INVALID (details.field="<f>.type").
//
// EXISTENCE of the target record (a live row in obj_<type> for THIS tenant) is a DB probe done
// separately at the orchestration layer (checkPolymorphicTargets), exactly as relation FK
// existence is validated apart from checkType — checkType stays DB-free.
func checkPolymorphic(f FieldSpec, val any) error {
	m, ok := val.(map[string]any)
	if !ok {
		return invalid("RECORD_INVALID_TYPE", "field %q phải là object {type, id}", f.Name)
	}
	// Atomic 2-key object: no key other than type/id (mirrors composite's unknown-sub-field reject).
	for key := range m {
		if key != "type" && key != "id" {
			return invalid("RECORD_INVALID_TYPE", "field %q chỉ nhận 2 khóa {type, id}", f.Name)
		}
	}
	// A present-but-non-string sub-value is a shape error (the key IS present), not "required missing".
	typRaw, hasType := m["type"]
	if hasType {
		if _, isStr := typRaw.(string); !isStr {
			return invalidField("RECORD_INVALID_TYPE", f.Name+".type", "field %q có type không phải chuỗi", f.Name)
		}
	}
	idRaw, hasID := m["id"]
	if hasID {
		if _, isStr := idRaw.(string); !isStr {
			return invalidField("RECORD_INVALID_TYPE", f.Name+".id", "field %q có id không phải chuỗi", f.Name)
		}
	}
	typ, _ := typRaw.(string)
	id, _ := idRaw.(string)
	if strings.TrimSpace(typ) == "" {
		return invalidField("RECORD_REQUIRED_MISSING", f.Name+".type", "field %q thiếu type", f.Name)
	}
	if strings.TrimSpace(id) == "" {
		return invalidField("RECORD_REQUIRED_MISSING", f.Name+".id", "field %q thiếu id", f.Name)
	}
	if !polymorphicTargetAllowed(f.Options, typ) {
		return invalidField("RECORD_POLYMORPHIC_TARGET_INVALID", f.Name+".type",
			"field %q có type %q ngoài danh sách targetEntities cho phép", f.Name, typ)
	}
	return nil
}

// polymorphicTargetAllowed reports whether typ is one of options.targetEntities. It reads the
// SAME string-array config polymorphicTargetList validates at config time — a record-time
// membership check, DB-free (existence is a separate probe).
func polymorphicTargetAllowed(options map[string]any, typ string) bool {
	if options == nil {
		return false
	}
	targets, ok := options["targetEntities"].([]any)
	if !ok {
		return false
	}
	for _, t := range targets {
		if s, ok := t.(string); ok && s == typ {
			return true
		}
	}
	return false
}

// picklistContains reports whether s is among options.values.
func picklistContains(options map[string]any, s string) bool {
	if options == nil {
		return false
	}
	values, ok := options["values"].([]any)
	if !ok {
		return false
	}
	for _, v := range values {
		if str, ok := v.(string); ok && str == s {
			return true
		}
	}
	return false
}

// checkFile validates a file/image field value (File-upload M1). The stored value is a JSON
// array of attachment references, each a {id, name} object: id is the sys_files row id (set
// server-side when a blob is uploaded via POST /:entity/:id/files), name is the display
// filename. A record write that carries a file field validates the SHAPE here so junk cannot
// leak into `data`; the actual blob/catalog lifecycle is owned by the file API, not the record
// API. An empty array is accepted (emptiness/required is handled upstream by isEmpty).
//
// Acceptable forms:
//   - []any of map[string]any, each with a non-empty string "id" and a string "name".
//
// Anything else (a bare string, a number, an array element missing id) is RECORD_INVALID_TYPE.
func checkFile(f FieldSpec, val any) error {
	arr, ok := val.([]any)
	if !ok {
		return invalid("RECORD_INVALID_TYPE",
			"field %q phải là mảng tham chiếu file [{id,name}]", f.Name)
	}
	for _, item := range arr {
		ref, ok := item.(map[string]any)
		if !ok {
			return invalid("RECORD_INVALID_TYPE",
				"field %q có phần tử không phải object {id,name}", f.Name)
		}
		id, ok := ref["id"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			return invalid("RECORD_INVALID_TYPE",
				"field %q có tham chiếu file thiếu id", f.Name)
		}
		// name is optional but, when present, must be a string (a display label).
		if nameVal, present := ref["name"]; present {
			if _, ok := nameVal.(string); !ok {
				return invalid("RECORD_INVALID_TYPE",
					"field %q có tham chiếu file với name không phải chuỗi", f.Name)
			}
		}
	}
	return nil
}
