// Package services holds the business logic of the metadata engine: validating and
// orchestrating entity/field definitions and dynamic records. Handlers call into here;
// this layer knows nothing about HTTP and talks to repositories/DDL below it.
package services

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"unicode/utf8"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/formula"
)

// ValidationError carries a stable machine code alongside a human message, so handlers
// can map it to an HTTP status/error code without string-matching the message.
// Field (optional) names the specific request field that triggered the error —
// populated for field-level rejects (e.g. RECORD_REQUIRED_MISSING), empty otherwise.
// Details (optional) carries additional structured context (e.g. {"field":"email"}).
type ValidationError struct {
	Code    string
	Msg     string
	Field   string         // optional: the request field that caused the error
	Details map[string]any // optional: extra structured context

	// hiddenKeys is set ONLY by FieldACLService's write guard when a payload names a field the
	// caller may not view (queue #317). The error itself already reads as RECORD_UNKNOWN_FIELD; the
	// set lets the write door re-run ITS OWN validator with those names treated as undeclared, so a
	// multi-key payload is refused in exactly the order/shape an unknown name would be (otherwise
	// which key the message names would tell a hidden name from a missing one). Never serialized.
	hiddenKeys map[string]struct{}
}

func (e *ValidationError) Error() string { return e.Msg }

func invalid(code, format string, args ...any) error {
	return &ValidationError{
		Code:    code,
		Msg:     fmt.Sprintf(format, args...),
		Details: map[string]any{},
	}
}

// invalidField is like invalid but also populates the Field hint for field-level errors,
// and sets Details["field"] so the error envelope always has a consistent shape.
func invalidField(code, field, format string, args ...any) error {
	return &ValidationError{
		Code:    code,
		Msg:     fmt.Sprintf(format, args...),
		Field:   field,
		Details: map[string]any{"field": field},
	}
}

// withField names field on a validation error that does not name one yet, so the form, the bulk
// grid and the import report can all point at the input (specs/061 FR-009). A composite sub-path
// already set by the checker ("fullname.first") is kept.
func withField(err error, field string) error {
	var ve *ValidationError
	if errors.As(err, &ve) && ve.Field == "" {
		ve.Field = field
		if ve.Details == nil {
			ve.Details = map[string]any{}
		}
		ve.Details["field"] = field
	}
	return err
}

// FieldInput is a field definition as received from the API (camelCase mapped upstream).
type FieldInput struct {
	Name       string
	Label      string
	FieldType  string
	IsRequired bool
	IsUnique   bool
	Options    map[string]any
	// FieldOrder (W5) is the explicit display position. nil = auto-assign: entity-create
	// numbers by slice position ((i+1)*10) — which also gives vertical-pack installs their
	// pack order for free — and add-field appends after the entity's current max (+10).
	// A non-nil value must be ≥ 0 (FIELD_ORDER_INVALID otherwise).
	FieldOrder *int
	// Description (W5) is the per-field admin description (≤500 runes,
	// FIELD_DESCRIPTION_TOO_LONG otherwise). Optional.
	Description string
}

// CreateEntityInput is the payload to register a new entity plus its initial fields.
type CreateEntityInput struct {
	Name        string
	Label       string
	Description string
	Fields      []FieldInput
}

// allowedFieldTypes is the set of field types this slice supports. `relation` (1-N,
// FK-on-child) is supported as of M3: it materializes a `<f>_id` FK column from
// options.relatedEntity (see validateRelationTarget) rather than living in the JSON `data`.
//
// Đ2 extends the catalog with nested-JSON config types — all stored inside the row's JSON
// `data` (no new columns, no migration): `multi_select` (array of allowed options),
// `currency` ({amount, currencyCode}), `rating` (bounded integer), `rich_text` (HTML/markdown
// blob), and `composite` (a flat object of scalar sub-fields, e.g. fullName = {firstName,
// lastName}). Per-type structural rules live in validateFieldConfig.
// File-upload M1 adds `file` and `image` — attachment fields whose per-record value is a JSON
// array of lightweight references [{id,name}] (the blobs live on the StorageBackend, cataloged
// in sys_files). They live entirely in the JSON `data` column: they can NOT be unique or indexed
// (a [{id,name}] array has no scalar projection for a generated column), enforced by
// validateFieldConfig + validateGenColumnMaxLength below.
var allowedFieldTypes = map[string]struct{}{
	"text": {}, "number": {}, "boolean": {}, "date": {}, "datetime": {},
	"email": {}, "picklist": {}, "relation": {},
	"multi_select": {}, "currency": {}, "rating": {}, "rich_text": {}, "composite": {},
	"file": {}, "image": {},
	// formula: computed field — arithmetic over the record's own number/currency fields.
	// The expression and ref-type validation happens at the orchestration layer (needs
	// sibling field metadata); this allowlist entry only gates checkFieldType.
	"formula": {},
	// ENG-COMPUTED #2/#3 (read-time only — see ../specs/005-computed-lookup-rollup/spec.md §"#2/#3"):
	//   lookup: mirrors options.sourceField from the related record reached via the sibling
	//     options.via (to-one) relation field. Resolved in record_expand.go applyLookups.
	//   rollup: SUM/COUNT/AVG/MIN/MAX over options.childEntity's records that point back at
	//     this record via options.viaRelation. Resolved in rollup.go, gated behind ?rollup=.
	// Both are server-computed (readonly-from-client) and never persisted to a physical
	// column — the cross-field/cross-entity ref checks happen at the orchestration layer
	// (validateLookupConfig / validateRollupConfig here only do the DB-free structural check).
	"lookup": {}, "rollup": {},
	// ENG #6 (../specs/006-auto-number/spec.md §"#6 auto_number"): auto_number is a per-tenant,
	// per-entity, per-field atomic sequence (sys_sequences, migration 000029). Unlike
	// lookup/rollup it IS persisted into the JSON `data` column like an ordinary field — but the
	// value is always SERVER-ASSIGNED (readonly-from-client, see computedReadonlyCode in
	// record_service.go) and only ever assigned at Create time (services.assignSequences in
	// record_orchestration.go), never recomputed on Update. options.prefix (string, optional) and
	// options.padding (int 0..12, optional) control formatting; see validateAutoNumberConfig.
	"auto_number": {},
	// polymorphic_relation (ACTIVITY-CORE #3a): a field pointing at a record in ANY entity of an
	// allowlist (options.targetEntities), e.g. activities.relatedTo → account/contact/opportunity.
	// Unlike `relation` (one fixed target) its per-record value is a JSON object {type, id} stored
	// under the bare field name; it materializes TWO STORED generated columns (<f>_type, <f>_id) +
	// a composite index (see ddl.BuildAddPolymorphicColumns). Config = validatePolymorphicConfig;
	// record shape/existence = checkPolymorphic + checkPolymorphicTargets (record_service.go /
	// record_orchestration.go). Never unique/indexed (no single scalar projection).
	"polymorphic_relation": {},
	// user (USER-FIELD slice, ../specs/017-user-field-type/spec.md): a field pointing at a record in the
	// FIXED `users` system table — never a sys_entity, so it carries no options.relatedEntity
	// and needs no target-existence/registry machinery (unlike `relation`). It reuses
	// `relation`'s plain 1-N physical shape as-is (CHAR(36) `<f>_id` STORED generated column +
	// non-unique tenant-scoped index — see ddl.BuildCreateTable/BuildAddRelationColumn). Record-time
	// value shape = checkType's "user" case (record_service.go); existence (must be an ACTIVE user
	// in the SAME tenant) = checkUserTargets (record_orchestration.go). Never unique (see
	// validateUserFieldFlags) — many records are routinely assigned to the same user.
	"user": {},
}

// AllowedFieldTypes returns the engine's field-type allowlist, sorted. It exists so a surface
// that advertises field types on its own (the AI Builder's fieldType enum in ai/tools.go) can be
// pinned to the engine by a strict-equality parity test instead of drifting as a hand-copied list.
func AllowedFieldTypes() []string {
	return slices.Sorted(maps.Keys(allowedFieldTypes))
}

// compositeSubAllowedTypes is the set of field types permitted as a `composite` sub-field.
// It deliberately EXCLUDES `composite` itself (G3: no recursion / unbounded nesting depth)
// and `relation` (G5: relation needs a real FK column, which a JSON-nested sub-field cannot
// have). Sub-fields are flat scalars (and, pragmatically, multi_select/picklist option lists)
// living together under one JSON object key.
var compositeSubAllowedTypes = map[string]struct{}{
	"text": {}, "number": {}, "boolean": {}, "date": {}, "datetime": {},
	"email": {}, "picklist": {}, "multi_select": {}, "currency": {},
	"rating": {}, "rich_text": {},
}

// checkFieldType validates a field's declared type against the allowlist. An unrecognized
// type fails with FIELD_INVALID_TYPE; a supported type passes. Per-type structural rules
// (picklist values, relation target) are enforced separately by the caller. Shared by
// entity-create and add-field.
func checkFieldType(fieldType string) error {
	if _, ok := allowedFieldTypes[fieldType]; !ok {
		return invalid("FIELD_INVALID_TYPE", "kiểu field %q không hỗ trợ", fieldType)
	}
	return nil
}

// relatedEntityName extracts the trimmed options.relatedEntity string, reporting whether it
// was present and non-empty. A relation field's FK target lives here (never in
// sys_relationships, which M3 1-N deliberately does not touch).
func relatedEntityName(options map[string]any) (string, bool) {
	if options == nil {
		return "", false
	}
	v, ok := options["relatedEntity"].(string)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	return v, v != ""
}

// validateRelationTarget enforces the structural rule for a 1-N `relation` field: its
// options.relatedEntity must be present (FIELD_RELATION_TARGET_REQUIRED, 422) and a valid
// entity identifier (FIELD_RELATION_TARGET_INVALID, 422). It does NOT verify the target
// entity exists in metadata — that is the orchestration layer's job (it has repo access);
// this is the pure, DB-free structural check shared by entity-create and add-field.
// validateDerivedIDColumnName guards field types whose DDL creates a derived `<field>_id` column
// (relation, user). SanitizeIdentifier caps identifiers at 26 chars and the "_id" suffix costs 3,
// so a 24+ char field name produces a column the engine can create but can never address again —
// the filter/sort paths re-sanitize the physical name and refuse it. That is the accept-then-swallow
// shape: accepted at config time, broken at use time, with nothing pointing back at the cause.
// Mirrors the polymorphic_relation guard above (which budgets for the longer "_type" suffix).
func validateDerivedIDColumnName(fieldType, name string) error {
	if len(name) > 23 {
		return invalid("FIELD_RELATION_NAME_TOO_LONG",
			"tên field %s %q quá dài (tối đa 23 ký tự — cột phụ `%s_id` phải vừa giới hạn định danh)",
			fieldType, name, name)
	}
	return nil
}

func validateRelationTarget(fieldName string, options map[string]any) error {
	target, ok := relatedEntityName(options)
	if !ok {
		return invalid("FIELD_RELATION_TARGET_REQUIRED",
			"field relation %q cần options.relatedEntity (entity đích)", fieldName)
	}
	if _, err := ddl.SanitizeIdentifier(target); err != nil {
		return invalid("FIELD_RELATION_TARGET_INVALID",
			"field relation %q có options.relatedEntity không hợp lệ: %v", fieldName, err)
	}
	return nil
}

// relationCardinality reads options.cardinality off a `relation` field, defaulting to
// "1-N" (the pre-existing, ordinary FK-on-child shape) when absent or not a string.
// ENG-REL2 #5 introduces "1-1" as the only other accepted value (validated by
// validateRelationCardinality below); callers that only need to branch DDL/registry
// behaviour compare the returned string against "1-1" directly.
func relationCardinality(options map[string]any) string {
	if options == nil {
		return "1-N"
	}
	if v, ok := options["cardinality"].(string); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return "1-N"
}

// validateRelationCardinality enforces the ENG-REL2 #5 structural rule for a `relation`
// field's options.cardinality: absent (defaults to "1-N"), or explicitly "1-N" / "1-1".
// Any other value (a typo, or a not-yet-supported shape like "N-N") is rejected 422
// FIELD_RELATION_CARDINALITY_INVALID before any DDL runs — config-time parity with every
// other opt-in field option this engine validates (RM-31 discipline: a mistyped value
// must fail loudly, not silently fall through to a default that was never intended).
func validateRelationCardinality(fieldName string, options map[string]any) error {
	if options == nil {
		return nil
	}
	raw, ok := options["cardinality"]
	if !ok {
		return nil
	}
	s, isString := raw.(string)
	if !isString {
		return invalid("FIELD_RELATION_CARDINALITY_INVALID",
			"field relation %q có options.cardinality không phải chuỗi (chỉ nhận \"1-N\" hoặc \"1-1\")", fieldName)
	}
	switch s {
	case "1-N", "1-1":
		return nil
	default:
		return invalid("FIELD_RELATION_CARDINALITY_INVALID",
			"field relation %q có options.cardinality %q không hợp lệ (chỉ nhận \"1-N\" hoặc \"1-1\")", fieldName, s)
	}
}

// polymorphicTargetList extracts options.targetEntities as a slice of trimmed, non-empty entity
// names (ACTIVITY-CORE #3a). ok=false when the key is absent, not an array, empty, or contains a
// non-string / blank element — the string-only, non-empty contract mirrors hasPicklistValues so
// junk like [1, "", null] is rejected at config time rather than surfacing later as a confusing
// record-time RECORD_POLYMORPHIC_TARGET_INVALID. Identifier validity + tenant existence of each
// name are checked at the orchestration layer (FIELD_POLYMORPHIC_TARGET_NOT_FOUND).
func polymorphicTargetList(options map[string]any) ([]string, bool) {
	if options == nil {
		return nil, false
	}
	raw, ok := options["targetEntities"].([]any)
	if !ok || len(raw) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, false
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, true
}

// validatePolymorphicConfig enforces the config-time structural rule for a `polymorphic_relation`
// field (ACTIVITY-CORE #3a): options.targetEntities must be a NON-EMPTY []string of entity names.
// Missing/empty/malformed → 422 FIELD_POLYMORPHIC_NO_TARGETS. The FE attach-picker needs this
// allowlist to know which entities a record may point at (mirror of relation.relatedEntity being
// required). Per-target existence is verified with repo access at CreateEntity / AddField.
func validatePolymorphicConfig(name string, options map[string]any) error {
	// The two derived sub-columns `<name>_type` / `<name>_id` must survive SanitizeIdentifier's
	// 26-char cap so the timeline-filter path (which re-sanitizes the physical column name) can
	// reach them. `_type` (+5) is the binding suffix → the field name must be ≤ 21 chars, else the
	// DDL would create a column the filter can never address (silent broken timeline). Name is
	// ASCII (SanitizeIdentifier restricts to [a-z0-9_]), so byte length == rune length.
	if len(name) > 21 {
		return invalid("FIELD_POLYMORPHIC_NAME_TOO_LONG",
			"tên field polymorphic_relation %q quá dài (tối đa 21 ký tự — cột phụ `%s_type` phải vừa giới hạn định danh)", name, name)
	}
	if _, ok := polymorphicTargetList(options); !ok {
		return invalid("FIELD_POLYMORPHIC_NO_TARGETS",
			"field polymorphic_relation %q cần options.targetEntities không rỗng (mảng tên entity)", name)
	}
	return nil
}

// isPolymorphicFieldType reports whether a field type is the ACTIVITY-CORE polymorphic relation.
// Kept as a tiny helper (mirrors isFileFieldType / isComputedFieldType) so the flag-guard call
// sites read consistently.
func isPolymorphicFieldType(fieldType string) bool {
	return fieldType == "polymorphic_relation"
}

// validatePolymorphicFieldFlags rejects a polymorphic_relation field declared unique or indexed
// (ACTIVITY-CORE #3a). Its per-record value is a JSON {type, id} object with no single scalar
// projection, so it cannot back the ONE STORED generated column unique/indexed require — the pair
// of gen-columns it DOES materialize (<f>_type, <f>_id) are engine-owned, not a unique/index the
// admin toggles. Mirrors validateFileFieldFlags for file/image. unique is the field-level flag;
// indexed is read from options. FIELD_POLYMORPHIC_NOT_UNIQUE / FIELD_POLYMORPHIC_NOT_INDEXED are
// flag-guard codes (parity with file/computed/relation), beyond the spec's three semantic codes.
func validatePolymorphicFieldFlags(name, fieldType string, isUnique bool, options map[string]any) error {
	if !isPolymorphicFieldType(fieldType) {
		return nil
	}
	if isUnique {
		return invalid("FIELD_POLYMORPHIC_NOT_UNIQUE",
			"field %q kiểu polymorphic_relation không thể đặt unique (giá trị là object {type, id})", name)
	}
	if boolOpt(options, "indexed") {
		return invalid("FIELD_POLYMORPHIC_NOT_INDEXED",
			"field %q kiểu polymorphic_relation không thể đánh index (giá trị là object {type, id})", name)
	}
	return nil
}

// validateFieldFlagGuards runs every "this type cannot be unique/indexed" guard (file/image,
// computed, relation, auto_number, polymorphic) in one place, so entity-create, add-field, AND
// edit-field share ONE gate and can never drift (root-cause fix for B1 — the edit path had been
// silently skipping all but the polymorphic guard, turning an edit with options{indexed:true} into
// an opaque 500 from the indexed-DDL branch instead of the documented 422). Each guard no-ops for a
// type it does not govern. `isUnique` is the field-level flag (immutable after create); `options`
// carries the (possibly changed) `indexed` toggle.
func validateFieldFlagGuards(name, fieldType string, isUnique bool, options map[string]any) error {
	if err := validateFileFieldFlags(name, fieldType, isUnique, options); err != nil {
		return err
	}
	if err := validateComputedFieldFlags(name, fieldType, isUnique, options); err != nil {
		return err
	}
	if err := validateRelationFieldFlags(name, fieldType, isUnique); err != nil {
		return err
	}
	if err := validateUserFieldFlags(name, fieldType, isUnique); err != nil {
		return err
	}
	if err := validateAutoNumberFieldFlags(name, fieldType, isUnique); err != nil {
		return err
	}
	return validatePolymorphicFieldFlags(name, fieldType, isUnique, options)
}

// ValidateCreateEntity checks an entity-creation payload before any DB/DDL work runs:
// entity name allowlist, non-empty label, and per-field name/type/uniqueness rules.
// It returns a *ValidationError (with a code) on the first problem, or nil.
func ValidateCreateEntity(in CreateEntityInput) error {
	if _, err := ddl.SanitizeIdentifier(in.Name); err != nil {
		return invalid("ENTITY_INVALID_NAME", "tên entity không hợp lệ: %v", err)
	}
	if strings.TrimSpace(in.Label) == "" {
		return invalid("ENTITY_LABEL_REQUIRED", "label entity là bắt buộc")
	}
	// Defense-in-depth (ENG-REL2 #4 security audit): bound the declared field list. Each
	// relation field costs 1–3 pre-flight metadata queries (target existence + inverse-field
	// availability) BEFORE any DDL, so a single 1 MiB body packed with thousands of relation
	// fields would otherwise fan out into tens of thousands of sequential queries. The cap is
	// far above any real vertical (builtin packs top out at ~15 fields per entity).
	if len(in.Fields) > maxFieldsPerEntity {
		return invalid("ENTITY_TOO_MANY_FIELDS",
			"entity khai báo %d field — tối đa %d field mỗi entity", len(in.Fields), maxFieldsPerEntity)
	}

	// seen tracks declared field names; reserved tracks the physical column names a relation
	// field implicitly materializes ("<rel>_id"). A relation field <rel> creates an FK column
	// <rel>_id, so a literal field named "<rel>_id" (or a second relation deriving the same
	// column) would collide at CREATE TABLE and surface as a 500. We catch the clash here as a
	// 422 (FIELD_RELATION_COLUMN_CONFLICT) — mirroring the add-field guard — so the contract is
	// symmetric across entity-create and add-field.
	seen := make(map[string]struct{}, len(in.Fields))
	reserved := make(map[string]string, len(in.Fields)) // column -> owning relation field name
	for _, f := range in.Fields {
		name, err := ddl.SanitizeIdentifier(f.Name)
		if err != nil {
			return invalid("FIELD_INVALID_NAME", "tên field %q không hợp lệ: %v", f.Name, err)
		}
		// W5-C1: reject a field named after a mandatory system column before any DDL — a
		// "version" field shadows RM-24 optimistic locking, a system-column collision trips
		// 1060 → 500. Same gate as add-field (ddl.IsReservedColumn) so both paths agree.
		if ddl.IsReservedColumn(name) {
			return invalid("FIELD_NAME_RESERVED",
				"tên field %q trùng cột hệ thống bắt buộc — chọn tên khác", name)
		}
		if _, dup := seen[name]; dup {
			return invalid("FIELD_DUPLICATE", "field %q bị khai trùng", name)
		}
		// A relation's derived column must not already be claimed by a real field of the same name.
		if owner, clash := reserved[name]; clash {
			return invalid("FIELD_RELATION_COLUMN_CONFLICT",
				"field %q trùng cột FK %q do field relation %q tạo ra", name, name, owner)
		}
		seen[name] = struct{}{}

		if err := checkFieldType(f.FieldType); err != nil {
			return err
		}
		if err := validateFieldConfig(name, f.FieldType, f.Options); err != nil {
			return err
		}
		// W5 builder v2: fieldOrder must be ≥ 0 when supplied; description ≤ 500 runes.
		// Same gate as add-field (validateField) so the two paths never drift.
		if err := validateFieldOrderDescription(name, f.FieldOrder, f.Description); err != nil {
			return err
		}
		// W5-B2 config-time: unique/indexed text-ish field → VARCHAR(255) generated column;
		// reject options.maxLength > 255 (same gate as add-field).
		if err := validateGenColumnMaxLength(name, f.FieldType, f.IsUnique, f.Options); err != nil {
			return err
		}
		// Type flag guards (file/image, computed, relation, auto_number, polymorphic cannot be
		// unique/indexed) — ONE shared gate with add-field + edit-field so they never drift.
		if err := validateFieldFlagGuards(name, f.FieldType, f.IsUnique, f.Options); err != nil {
			return err
		}

		// ACTIVITY-CORE #3a: a polymorphic_relation field <f> materializes physical columns
		// `<f>_type` AND `<f>_id`, so a literal field named after either (or a second field
		// deriving the same column) would collide at CREATE TABLE (1060 → 500). Reserve both,
		// mirroring the relation `<f>_id` reservation below.
		if f.FieldType == "polymorphic_relation" {
			for _, col := range []string{name + "_type", name + "_id"} {
				if ddl.IsReservedColumn(col) {
					return invalid("FIELD_NAME_RESERVED",
						"field polymorphic_relation %q tạo cột %q trùng cột hệ thống bắt buộc — chọn tên khác", name, col)
				}
				if _, dup := seen[col]; dup {
					return invalid("FIELD_RELATION_COLUMN_CONFLICT",
						"field polymorphic_relation %q tạo cột %q nhưng đã có field cùng tên", name, col)
				}
				if owner, dup := reserved[col]; dup {
					return invalid("FIELD_RELATION_COLUMN_CONFLICT",
						"field polymorphic_relation %q tạo cột %q trùng với field %q", name, col, owner)
				}
				reserved[col] = name
			}
		}

		// USER-FIELD: a `user` field derives the IDENTICAL `<f>_id` physical column shape as a
		// plain `relation` field (reused DDL), so it needs the SAME collision reservation.
		if f.FieldType == "relation" || f.FieldType == "user" {
			col := name + "_id"
			// W5-C1: the derived FK column must not collide with a system column either
			// (e.g. a field named "team_set" derives "team_set_id").
			if ddl.IsReservedColumn(col) {
				return invalid("FIELD_NAME_RESERVED",
					"field %s %q tạo cột %q trùng cột hệ thống bắt buộc — chọn tên khác", f.FieldType, name, col)
			}
			// The derived FK column collides with an already-declared field, or another
			// relation/user field already reserved the same column.
			if _, dup := seen[col]; dup {
				return invalid("FIELD_RELATION_COLUMN_CONFLICT",
					"field %s %q tạo cột %q nhưng đã có field cùng tên", f.FieldType, name, col)
			}
			if owner, dup := reserved[col]; dup {
				return invalid("FIELD_RELATION_COLUMN_CONFLICT",
					"field %s %q tạo cột %q trùng với field %q", f.FieldType, name, col, owner)
			}
			reserved[col] = name
		}
	}
	// specs/052: a dependent picklist names a sibling — checked once every declared field is
	// known, so the parent may be declared after its child. Here (not in the orchestrator) so the
	// AI Builder's propose_entity, which calls this same function, gets it too.
	for _, f := range in.Fields {
		if err := validateDependentRefs(f, in.Fields, nil); err != nil {
			return err
		}
		if err := validateRelationFilterRefs(f, in.Fields, nil); err != nil {
			return err
		}
	}
	return nil
}

// hasPicklistValues reports whether options carries a non-empty "values" array in which
// EVERY element is a non-empty string. A picklist/multi_select whose values contain a
// non-string element (number, bool, null, object) or a blank/whitespace-only string is
// rejected as malformed config — otherwise junk like `[1, "", null]` would be persisted and
// only surface later as a confusing record-time RECORD_INVALID_PICKLIST. The string-only,
// non-empty contract here matches what record-time picklistContains compares against.
func hasPicklistValues(options map[string]any) bool {
	if options == nil {
		return false
	}
	values, ok := options["values"].([]any)
	if !ok || len(values) == 0 {
		return false
	}
	for _, v := range values {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return false
		}
	}
	return true
}

// validateValueLabels checks the OPTIONAL options.valueLabels map on picklist/multi_select: when
// present it must be an object of non-empty-string → non-empty-string. It is the display-label
// layer over the stored codes ("New" → "Mới") — the codes in options.values never change, so the
// packs' work queues, report filters and workflow conditions (all comparing codes verbatim) are
// untouched by translation.
//
// Deliberately TOLERANT about which keys appear: a label whose code was later removed from
// `values` is a harmless orphan, not a 422 — an admin deleting one option must not be blocked by
// a stale label entry. What IS rejected is junk SHAPE (non-string keys can't happen in JSON;
// non-string or blank values can), because PUT /admin/fields/:id accepts arbitrary options and
// anything stored here is rendered on the customer face.
func validateValueLabels(name string, options map[string]any) error {
	if options == nil {
		return nil
	}
	raw, present := options["valueLabels"]
	if !present {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return invalid("FIELD_VALUE_LABELS_INVALID",
			"field %q: options.valueLabels phải là object mã→nhãn (chuỗi→chuỗi)", name)
	}
	for k, v := range m {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" || strings.TrimSpace(k) == "" {
			return invalid("FIELD_VALUE_LABELS_INVALID",
				"field %q: options.valueLabels[%q] phải là chuỗi không rỗng", name, k)
		}
	}
	return nil
}

// validateFieldConfig enforces the per-type structural rules for a field's nested-JSON
// config (options). It assumes fieldType already passed checkFieldType. It is the single
// source of truth shared by entity-create (ValidateCreateEntity) and add-field
// (validateField) so the two paths never drift. `name` is the already-sanitized field name,
// used only for error messages. A nil error means the config is well-formed (or the type
// needs no config).
func validateFieldConfig(name, fieldType string, options map[string]any) error {
	// datetime fields cannot be indexed: the RFC3339 format used at record-write time
	// (e.g. "2026-07-15T10:30:00Z") is rejected by MySQL STRICT_TRANS_TABLES when cast to
	// DATETIME inside a STORED generated column (Error 1292). Normalizing RFC3339 to MySQL
	// datetime within a generated column expression is out of scope; reject early with 422
	// instead of letting every insert/alter backfill 500 (D1 blocker fix).
	if fieldType == "datetime" && boolOpt(options, "indexed") {
		return invalid("FIELD_INDEX_UNSUPPORTED_TYPE",
			"field datetime không thể đánh index — RFC3339 chưa được chuẩn hoá cho generated column; dùng date hoặc filter datetime không cần index")
	}

	// pii-sensitive-audit-redact: options.sensitive is a SECURITY flag — audit_redact.go reads it
	// with boolOpt, which returns false for anything that is not a real bool. A mis-typed value
	// ({"sensitive":"true"} from a hand-written PUT, an AI-proposed field or a pack manifest)
	// would therefore store happily and fail OPEN: PII keeps flowing into the append-only trail
	// with no 422, no log and no visible difference in the builder. Type-checked here, for every
	// field type, so having the key at all means having it working.
	if v, present := options["sensitive"]; present {
		if _, isBool := v.(bool); !isBool {
			return invalid("FIELD_SENSITIVE_INVALID",
				"field %q có options.sensitive không hợp lệ (cần true hoặc false, không phải chuỗi hay số)", name)
		}
	}

	// specs/052: controllingField/dependentValues are read at record time for `picklist` only —
	// on any other type they would be silently ignored, so having the keys means having it work.
	if fieldType != "picklist" && hasDependentKeys(options) {
		return invalid(codeFieldDependentInvalid,
			"field %s %q: options.controllingField/dependentValues chỉ dùng cho field picklist", fieldType, name)
	}

	if err := validateRelationFilterConfig(name, fieldType, options); err != nil {
		return err
	}

	switch fieldType {
	case "picklist":
		if !hasPicklistValues(options) {
			return invalid("FIELD_PICKLIST_NO_OPTIONS", "field picklist %q cần options.values không rỗng", name)
		}
		if err := validateValueLabels(name, options); err != nil {
			return err
		}
		return validateDependentConfig(name, options)
	case "multi_select":
		if !hasPicklistValues(options) {
			return invalid("FIELD_MULTI_SELECT_NO_OPTIONS", "field multi_select %q cần options.values không rỗng", name)
		}
		return validateValueLabels(name, options)
	case "currency":
		return validateCurrencyConfig(name, options)
	case "rating":
		return validateRatingConfig(name, options)
	case "relation":
		if err := validateDerivedIDColumnName("relation", name); err != nil {
			return err
		}
		if err := validateRelationTarget(name, options); err != nil {
			return err
		}
		// ENG-REL2 #5: options.cardinality ("1-N"|"1-1"|absent) structural check.
		return validateRelationCardinality(name, options)
	case "polymorphic_relation":
		// ACTIVITY-CORE #3a: options.targetEntities must be a non-empty []string. Per-target
		// existence (a real active entity in the tenant) is verified at the orchestration layer
		// (CreateEntity / AddField) which has repo access — mirrors relation's structural/DB split.
		return validatePolymorphicConfig(name, options)
	case "composite":
		return validateCompositeConfig(name, options)
	case "text", "rich_text":
		// RM-29: validate options.maxLength (positive integer) and options.pattern (valid regex).
		if err := validateTextConfig(name, options); err != nil {
			return err
		}
	case "number":
		// RM-29: validate options.min, options.max, options.step consistency.
		if err := validateNumberConfig(name, options); err != nil {
			return err
		}
	case "boolean", "date", "datetime", "email":
		// Scalar types carry no required structural config.
	case "user":
		// USER-FIELD: no options.relatedEntity to validate — the target is the FIXED `users`
		// system table, never a config choice (unlike `relation`, which requires one). It DOES
		// share relation's derived `<field>_id` column, so it shares the length budget too.
		return validateDerivedIDColumnName("user", name)
	case "formula":
		return validateFormulaConfig(name, options)
	case "lookup":
		return validateLookupConfig(name, options)
	case "rollup":
		return validateRollupConfig(name, options)
	case "auto_number":
		return validateAutoNumberConfig(name, options)
	}
	return nil
}

// validateAutoNumberConfig checks the per-field structural config for an "auto_number" field at
// definition time (ENG #6). Both options are optional:
//   - options.prefix, when present, must be a string of at most 20 characters
//     (FIELD_AUTONUMBER_PREFIX_INVALID).
//   - options.padding, when present, must be an integer in [0, 12]
//     (FIELD_AUTONUMBER_PADDING_INVALID).
//
// No options at all is valid (the sequence number is emitted as a bare decimal string).
// IsUnique is NOT checked here: auto_number + unique is legal since RM-28 (per-tenant pivot) —
// validateAutoNumberFieldFlags is the documented no-op that records why.
func validateAutoNumberConfig(name string, options map[string]any) error {
	if options == nil {
		return nil
	}
	if prefixRaw, ok := options["prefix"]; ok {
		prefix, isString := prefixRaw.(string)
		// Rune count, not byte count (review LOW-1) — the codebase's length convention
		// (api-conventions: maxLength "đếm theo rune, không byte") so a multi-byte prefix
		// (e.g. "HĐ-") gets the full 20 visible characters.
		if !isString || utf8.RuneCountInString(prefix) > 20 {
			return invalid("FIELD_AUTONUMBER_PREFIX_INVALID",
				"field auto_number %q có options.prefix không hợp lệ (cần chuỗi tối đa 20 ký tự)", name)
		}
	}
	if paddingRaw, ok := options["padding"]; ok {
		padding, isNumber := paddingRaw.(float64)
		if !isNumber || padding != float64(int(padding)) || padding < 0 || padding > 12 {
			return invalid("FIELD_AUTONUMBER_PADDING_INVALID",
				"field auto_number %q có options.padding không hợp lệ (cần số nguyên 0..12)", name)
		}
	}
	return nil
}

// isAutoNumberFieldType reports whether a field type is the ENG #6 server-assigned atomic
// sequence. Kept as a tiny helper (mirrors isComputedFieldType / isFileFieldType) so the several
// flag-guard call sites read consistently.
func isAutoNumberFieldType(fieldType string) bool {
	return fieldType == "auto_number"
}

// validateAutoNumberFieldFlags used to reject auto_number + unique. It no longer does, and the
// reason it existed is worth keeping written down because the condition it named has been met.
//
// The old rule was NOT structural — an auto_number value is a formatted string and backs a
// generated column exactly like `text`. It was a consequence of the shared obj_* table: a real
// UNIQUE index on that gencol was ONE constraint shared by every tenant registering the entity,
// while auto_number counts per tenant (sys_sequences is tenant-scoped). Two tenants both starting
// at 1 legitimately produce the same "INV-00001", and the shared index would reject the second
// tenant's insert. The comment ended: "Until the pivot-uniqueness fix (RM-28) lands".
//
// RM-28 landed. Migration 000050 created sys_unique_index with
// UNIQUE KEY uq_pivot (tenant_id, entity_name, field_name, value) — uniqueness is now enforced
// PER TENANT by the pivot — and 000051 demoted the physical obj_* UNIQUE indexes to plain idx_
// ones. The AddField unique branch (field_orchestration.go) accordingly materializes a NON-unique
// generated column + index. Both halves of the old objection are gone: no shared constraint, and
// two tenants may now each hold "INV-00001".
//
// The dispatch-order hazard that keeps `relation` and `user` permanently non-unique does NOT
// apply here: those two need a CHAR(36) <field>_id FK column, so the unique branch would
// materialize the wrong shape. auto_number's value lives in data.<field> as a string, which is
// exactly what the unique branch's VARCHAR gencol projects.
//
// Why it matters beyond tidiness: a document number (quote/invoice/order) is the natural
// reference a human types into a bank transfer memo, and the payment-reconciliation step
// (inboundhook.PaymentMatch) REQUIRES its refField to be unique. With this rule in place the
// flagship money loop could not be wired to the flagship numbering field.
func validateAutoNumberFieldFlags(_, _ string, _ bool) error {
	return nil
}

// maxFieldDescriptionRunes caps a field's admin description (W5, sys_fields.description
// VARCHAR(500)) — counted in runes, not bytes, per the codebase length convention
// (api-conventions: "đếm theo rune, không byte"). NOTE: 500 runes of multi-byte Vietnamese
// fit VARCHAR(500) because MySQL VARCHAR(n) counts CHARACTERS under utf8mb4, not bytes.
const maxFieldDescriptionRunes = 500

// validateFieldOrderDescription enforces the W5 builder-v2 per-field attributes shared by
// entity-create, add-field AND edit-field (single source so the three paths never drift):
//   - fieldOrder, when supplied (non-nil), must be ≥ 0 (FIELD_ORDER_INVALID). nil = auto.
//   - description must be ≤ 500 runes (FIELD_DESCRIPTION_TOO_LONG).
func validateFieldOrderDescription(name string, order *int, description string) error {
	if order != nil && *order < 0 {
		return invalid("FIELD_ORDER_INVALID",
			"field %q có fieldOrder %d không hợp lệ (cần số nguyên ≥ 0)", name, *order)
	}
	if n := utf8.RuneCountInString(description); n > maxFieldDescriptionRunes {
		return invalid("FIELD_DESCRIPTION_TOO_LONG",
			"field %q có description dài %d ký tự — tối đa %d", name, n, maxFieldDescriptionRunes)
	}
	return nil
}

// validateFormulaConfig checks the per-field structural config for a "formula" field at
// definition time. It validates only what is statically knowable without DB access:
//   - options.formula must be a non-empty string (FIELD_FORMULA_INVALID).
//   - options.formula must be at most formula.MaxLen runes (FIELD_FORMULA_INVALID).
//   - options.formula must parse (FIELD_FORMULA_INVALID).
//   - options.resultType, when present, must be "number", "currency" or "text"
//     (FIELD_FORMULA_INVALID); absent means "number".
//   - options.formula must type-check — a comparison only as a condition, and a result that
//     matches resultType: a number for number/currency, text for text (specs/053)
//     (FIELD_FORMULA_TYPE_MISMATCH). This stage runs on create, add-field AND edit
//     (ValidateEditFieldChanges), so a mistyped formula is refused on every save path.
//   - A formula field may not be IsRequired (config-time reject: ValidateRecord enforces
//     required BEFORE applyComputedFields runs, so a required formula field would always
//     trip RECORD_REQUIRED_MISSING on every write). Callers reject IsRequired at the
//     field-input validation layer (validateField calls this from the switch).
//
// Cross-field ref validation (UNKNOWN_REF / NON_NUMERIC_REF / NESTED) requires sibling
// field metadata and is therefore done at the orchestration layer (CreateEntity /
// AddField) which has access to all field definitions, not here.
func validateFormulaConfig(name string, options map[string]any) error {
	if options == nil {
		return invalid("FIELD_FORMULA_INVALID",
			"field formula %q cần options.formula (biểu thức)", name)
	}
	exprRaw, ok := options["formula"]
	if !ok {
		return invalid("FIELD_FORMULA_INVALID",
			"field formula %q cần options.formula (biểu thức)", name)
	}
	expr, ok := exprRaw.(string)
	if !ok || strings.TrimSpace(expr) == "" {
		return invalid("FIELD_FORMULA_INVALID",
			"field formula %q có options.formula không phải chuỗi không rỗng", name)
	}
	// Length cap before any parsing: the evaluation cache is shared by every tenant and a
	// parsed tree costs ~100× its source (review 2026-09-30).
	if n := utf8.RuneCountInString(expr); n > formula.MaxLen {
		return invalid("FIELD_FORMULA_INVALID",
			"field formula %q có options.formula dài %d ký tự (tối đa %d)", name, n, formula.MaxLen)
	}
	// resultType first: it decides what the expression must produce.
	want := formula.ResultNumber
	if rtRaw, ok := options["resultType"]; ok {
		switch rt, _ := rtRaw.(string); rt {
		case "number", "currency":
		case "text":
			want = formula.ResultText
		default:
			return invalid("FIELD_FORMULA_INVALID",
				"field formula %q có options.resultType không hợp lệ (cần \"number\", \"currency\" hoặc \"text\")", name)
		}
	}
	// Syntax + types, before any DB work. Types need no sibling metadata (every identifier is
	// a number at record time); the cross-field ref checks happen at the orchestration layer,
	// which supplies the real sibling fields. formula.Check is uncached on purpose: this judges
	// what an admin typed, not a saved formula.
	if err := formula.Check(expr, want); err != nil {
		if errors.Is(err, formula.ErrTypeMismatch) {
			return invalid("FIELD_FORMULA_TYPE_MISMATCH",
				"field formula %q: %v", name, err)
		}
		return invalid("FIELD_FORMULA_INVALID",
			"field formula %q có biểu thức không hợp lệ: %v", name, err)
	}
	return nil
}

// validateLookupConfig checks the per-field structural config for a "lookup" field at
// definition time (ENG-COMPUTED #2). It validates only what is statically knowable without DB
// access — options.via and options.sourceField must be non-empty strings:
//   - options.via must name a field (FIELD_LOOKUP_VIA_REQUIRED when absent/blank).
//   - options.sourceField must be a non-empty string (FIELD_LOOKUP_SOURCE_FIELD_REQUIRED).
//
// Cross-field checks that need sibling/cross-entity metadata — `via` actually naming an
// existing sibling RELATION field (FIELD_LOOKUP_VIA_UNKNOWN), that relation being to-one and
// not an N-N join (FIELD_LOOKUP_VIA_NOT_RELATION / FIELD_LOOKUP_VIA_MUST_BE_TO_ONE), and the
// cycle guard against sourceField naming another lookup/formula (FIELD_LOOKUP_SOURCE_NESTED) —
// are done at the orchestration layer (CreateEntity / AddField), which has access to the full
// field list and (for the target entity) a repo. Mirrors the formula validateFormulaConfig /
// validateFormulaRefs split.
func validateLookupConfig(name string, options map[string]any) error {
	if options == nil {
		return invalid("FIELD_LOOKUP_VIA_REQUIRED",
			"field lookup %q cần options.via (tên field relation cùng entity)", name)
	}
	viaRaw, ok := options["via"]
	if !ok {
		return invalid("FIELD_LOOKUP_VIA_REQUIRED",
			"field lookup %q cần options.via (tên field relation cùng entity)", name)
	}
	via, ok := viaRaw.(string)
	if !ok || strings.TrimSpace(via) == "" {
		return invalid("FIELD_LOOKUP_VIA_REQUIRED",
			"field lookup %q có options.via không phải chuỗi không rỗng", name)
	}
	sfRaw, ok := options["sourceField"]
	if !ok {
		return invalid("FIELD_LOOKUP_SOURCE_FIELD_REQUIRED",
			"field lookup %q cần options.sourceField (tên field trên entity liên kết)", name)
	}
	sf, ok := sfRaw.(string)
	if !ok || strings.TrimSpace(sf) == "" {
		return invalid("FIELD_LOOKUP_SOURCE_FIELD_REQUIRED",
			"field lookup %q có options.sourceField không phải chuỗi không rỗng", name)
	}
	return nil
}

// rollupMetrics is the allowed set of options.metric values for a rollup field (ENG-COMPUTED #3).
var rollupMetrics = map[string]struct{}{
	"count": {}, "sum": {}, "avg": {}, "min": {}, "max": {},
}

// rollupAggFieldAllowedTypes is the set of child-entity field types a rollup may UNCONDITIONALLY
// aggregate with sum/avg/min/max (mirrors dynrepo.AggregateScalar's CAST(... AS DECIMAL(38,10)) —
// only these types have a numeric JSON scalar to CAST). Cross-checked against the child entity's
// metadata at the orchestration layer (validateRollupCrossEntity), not here.
//
// NOTE: "formula" is deliberately NOT in this set even though a number- or currency-resultType
// formula is also rollup-able (2026-07-12 / #262) — its eligibility depends on options.resultType, which this
// static set cannot express. validateRollupCrossEntity special-cases FieldType=="formula" with its
// own resultType check instead of a blanket membership test.
var rollupAggFieldAllowedTypes = map[string]struct{}{
	"number": {}, "currency": {}, "rating": {},
}

// validateRollupConfig checks the per-field structural config for a "rollup" field at
// definition time (ENG-COMPUTED #3). It validates only what is statically knowable without DB
// access:
//   - options.childEntity must be a non-empty valid entity identifier (FIELD_ROLLUP_CHILD_ENTITY_REQUIRED / _INVALID).
//   - options.viaRelation must be a non-empty string (FIELD_ROLLUP_VIA_RELATION_REQUIRED).
//   - options.metric must be one of count|sum|avg|min|max (FIELD_ROLLUP_METRIC_INVALID).
//   - options.aggField is required for every metric except count (FIELD_ROLLUP_AGG_FIELD_REQUIRED).
//
// Cross-entity checks — childEntity exists for this tenant, viaRelation names a relation field
// ON the child entity whose relatedEntity points back at this entity, aggField exists on the
// child entity and is number/currency/rating, and the cycle guard against aggField naming
// another rollup/formula/lookup — are done at the orchestration layer (CreateEntity / AddField),
// which has repo access to read the child entity's field list. Mirrors the formula
// validateFormulaConfig / validateFormulaRefs split.
func validateRollupConfig(name string, options map[string]any) error {
	if options == nil {
		return invalid("FIELD_ROLLUP_CHILD_ENTITY_REQUIRED",
			"field rollup %q cần options.childEntity (entity con)", name)
	}
	childRaw, ok := options["childEntity"]
	if !ok {
		return invalid("FIELD_ROLLUP_CHILD_ENTITY_REQUIRED",
			"field rollup %q cần options.childEntity (entity con)", name)
	}
	child, ok := childRaw.(string)
	if !ok || strings.TrimSpace(child) == "" {
		return invalid("FIELD_ROLLUP_CHILD_ENTITY_REQUIRED",
			"field rollup %q có options.childEntity không phải chuỗi không rỗng", name)
	}
	if _, err := ddl.SanitizeIdentifier(child); err != nil {
		return invalid("FIELD_ROLLUP_CHILD_ENTITY_INVALID",
			"field rollup %q có options.childEntity không hợp lệ: %v", name, err)
	}

	// Rollup supports two distinct relationship shapes, disambiguated by which options KEY is
	// present (never by probing whether a name happens to resolve to a field vs a
	// relationship — that would need a DB read at config time and would be ambiguous if a
	// name collided across both). Exactly one of the two must be present:
	//   - FK-on-child (1-N, sys_fields):    options.viaRelation — a `relation` field ON THE
	//     CHILD entity whose options.relatedEntity points back at this entity.
	//   - N-N (sys_relationships, join table): options.relationship — the registered N-N
	//     relationship NAME (RegisterRelationship) whose two sides are this entity and
	//     childEntity. Resolved via RelationService.ListLinked's pivot id-set at read time.
	viaRaw, hasVia := options["viaRelation"]
	relRaw, hasRel := options["relationship"]
	if hasVia && hasRel {
		return invalid("FIELD_ROLLUP_RELATION_AMBIGUOUS",
			"field rollup %q chỉ được khai một trong options.viaRelation (1-N) hoặc options.relationship (N-N), không phải cả hai", name)
	}
	if !hasVia && !hasRel {
		return invalid("FIELD_ROLLUP_VIA_RELATION_REQUIRED",
			"field rollup %q cần options.viaRelation (1-N, field relation trên entity con) hoặc options.relationship (N-N, tên quan hệ đã đăng ký)", name)
	}
	if hasVia {
		via, ok := viaRaw.(string)
		if !ok || strings.TrimSpace(via) == "" {
			return invalid("FIELD_ROLLUP_VIA_RELATION_REQUIRED",
				"field rollup %q có options.viaRelation không phải chuỗi không rỗng", name)
		}
	}
	if hasRel {
		rel, ok := relRaw.(string)
		if !ok || strings.TrimSpace(rel) == "" {
			return invalid("FIELD_ROLLUP_RELATIONSHIP_REQUIRED",
				"field rollup %q có options.relationship không phải chuỗi không rỗng", name)
		}
	}

	// specs/083: options.stored keeps the total in the record so it can be filtered and summed in
	// a report. Type first (B6 parity with the other options), then the shape it supports: a 1-N
	// roll-up only — an N-N total would also have to follow link/unlink, which has no hook yet.
	if storedRaw, has := options["stored"]; has {
		stored, isBool := storedRaw.(bool)
		if !isBool {
			return invalid("FIELD_ROLLUP_STORED_INVALID",
				"field rollup %q có options.stored không phải true/false", name)
		}
		if stored && hasRel {
			return invalid("FIELD_ROLLUP_STORED_NN_UNSUPPORTED",
				"field rollup %q: chỉ lưu được giá trị của tổng hợp theo quan hệ 1-N (viaRelation), chưa hỗ trợ quan hệ N-N", name)
		}
	}

	metricRaw, ok := options["metric"]
	if !ok {
		return invalid("FIELD_ROLLUP_METRIC_INVALID",
			"field rollup %q cần options.metric (count|sum|avg|min|max)", name)
	}
	metric, ok := metricRaw.(string)
	if !ok {
		return invalid("FIELD_ROLLUP_METRIC_INVALID",
			"field rollup %q có options.metric không phải chuỗi", name)
	}
	if _, ok := rollupMetrics[metric]; !ok {
		return invalid("FIELD_ROLLUP_METRIC_INVALID",
			"field rollup %q có options.metric %q không hợp lệ (cần count|sum|avg|min|max)", name, metric)
	}

	if metric != "count" {
		aggRaw, ok := options["aggField"]
		if !ok {
			return invalid("FIELD_ROLLUP_AGG_FIELD_REQUIRED",
				"field rollup %q metric %q cần options.aggField (field số trên entity con)", name, metric)
		}
		agg, ok := aggRaw.(string)
		if !ok || strings.TrimSpace(agg) == "" {
			return invalid("FIELD_ROLLUP_AGG_FIELD_REQUIRED",
				"field rollup %q có options.aggField không phải chuỗi không rỗng", name)
		}
	}
	return nil
}

// validateCurrencyConfig enforces a currency field's nested config: options.currencyCode is a
// required 3-letter uppercase ISO-4217-style code (FIELD_CURRENCY_CODE_*), the optional
// options.precision, when present, must be an integer in [0, 8] (FIELD_CURRENCY_PRECISION_INVALID),
// and the optional opt-in bounds options.min/options.max (RM-31 revised) must be numbers with
// min <= max (FIELD_CURRENCY_RANGE_INVALID). The per-record value lives in `data` as
// {amount, currencyCode}; this validates only the schema.
func validateCurrencyConfig(name string, options map[string]any) error {
	codeRaw, ok := options["currencyCode"]
	if !ok {
		return invalid("FIELD_CURRENCY_CODE_REQUIRED",
			"field currency %q cần options.currencyCode", name)
	}
	code, _ := codeRaw.(string)
	if !isCurrencyCode(code) {
		return invalid("FIELD_CURRENCY_CODE_INVALID",
			"field currency %q có options.currencyCode không hợp lệ (cần mã 3 chữ in hoa, vd VND/USD)", name)
	}
	if precRaw, ok := options["precision"]; ok {
		prec, ok := precRaw.(float64)
		if !ok || prec != float64(int(prec)) || prec < 0 || prec > 8 {
			return invalid("FIELD_CURRENCY_PRECISION_INVALID",
				"field currency %q có options.precision không hợp lệ (cần số nguyên 0..8)", name)
		}
	}
	// RM-31 (revised): validate the opt-in floor/ceiling at config time. Without this, a mistyped
	// bound (e.g. min:"0" as a JSON string, or a bool) is silently dropped by currencyBounds at
	// record time — hasMin/hasMax fall to false and the admin's opt-in floor VANISHES, letting
	// negatives through on a field that explicitly forbade them. Fail loudly here instead. Mirrors
	// validateNumberConfig; currency bounds are plain numbers (no integer/step constraint).
	var minVal, maxVal float64
	hasMin, hasMax := false, false
	if minRaw, ok := options["min"]; ok {
		v, ok := minRaw.(float64)
		if !ok {
			return invalid("FIELD_CURRENCY_RANGE_INVALID",
				"field currency %q có options.min không phải số", name)
		}
		minVal, hasMin = v, true
	}
	if maxRaw, ok := options["max"]; ok {
		v, ok := maxRaw.(float64)
		if !ok {
			return invalid("FIELD_CURRENCY_RANGE_INVALID",
				"field currency %q có options.max không phải số", name)
		}
		maxVal, hasMax = v, true
	}
	if hasMin && hasMax && minVal > maxVal {
		return invalid("FIELD_CURRENCY_RANGE_INVALID",
			"field currency %q có options.min (%v) lớn hơn options.max (%v)", name, minVal, maxVal)
	}
	return nil
}

// isCurrencyCode reports whether s is a 3-character uppercase A–Z code (ISO-4217 shape). It
// does NOT check membership in a real currency table — that is an out-of-scope value concern.
func isCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// validateRatingConfig enforces a rating field's optional nested config: options.max, when
// present, must be an integer in [2, 10] (FIELD_RATING_MAX_INVALID); options.min, when present,
// must be an integer >= 1 (FIELD_RATING_MIN_INVALID). When both are present, min must not exceed
// the effective max (FIELD_RATING_MIN_INVALID) — otherwise the field is unsatisfiable, since the
// record-time bounds (ratingBounds) would yield min > max and reject every value. When a bound is
// absent the engine applies a default scale at value-validation time (defaultRatingMin/Max).
func validateRatingConfig(name string, options map[string]any) error {
	if options == nil {
		return nil
	}
	// Resolve max first (declared or default) so min can be range-checked against it.
	max := defaultRatingMax
	if maxRaw, ok := options["max"]; ok {
		m, ok := maxRaw.(float64)
		if !ok || m != float64(int(m)) || m < 2 || m > 10 {
			return invalid("FIELD_RATING_MAX_INVALID",
				"field rating %q có options.max không hợp lệ (cần số nguyên 2..10)", name)
		}
		max = int(m)
	}
	if minRaw, ok := options["min"]; ok {
		mn, ok := minRaw.(float64)
		if !ok || mn != float64(int(mn)) || mn < 1 || int(mn) > max {
			return invalid("FIELD_RATING_MIN_INVALID",
				"field rating %q có options.min không hợp lệ (cần số nguyên >=1 và <= max %d)", name, max)
		}
	}
	return nil
}

// validateCompositeConfig enforces a composite field's nested config: options.subFields is a
// required non-empty array (FIELD_COMPOSITE_NO_SUBFIELDS), and each entry is an object with a
// valid identifier `name` and a permitted scalar `fieldType` (FIELD_COMPOSITE_SUBFIELD_INVALID).
// Sub-field names must be unique within the composite (FIELD_COMPOSITE_SUBFIELD_DUPLICATE).
//
// A composite is flat-by-design: `composite` and `relation` are rejected as sub-field types
// (see compositeSubAllowedTypes — G3 no recursion, G5 no nested FK). The composite value lives
// in `data` as a single JSON object (e.g. data.fullName = {firstName, lastName}); no new column.
func validateCompositeConfig(name string, options map[string]any) error {
	var subs []any
	if options != nil {
		subs, _ = options["subFields"].([]any)
	}
	if len(subs) == 0 {
		return invalid("FIELD_COMPOSITE_NO_SUBFIELDS",
			"field composite %q cần options.subFields không rỗng", name)
	}
	seen := make(map[string]struct{}, len(subs))
	for _, raw := range subs {
		sub, ok := raw.(map[string]any)
		if !ok {
			return invalid("FIELD_COMPOSITE_SUBFIELD_INVALID",
				"field composite %q có sub-field không phải object", name)
		}
		subName, _ := sub["name"].(string)
		clean, err := ddl.SanitizeIdentifier(subName)
		if err != nil {
			return invalid("FIELD_COMPOSITE_SUBFIELD_INVALID",
				"field composite %q có sub-field tên %q không hợp lệ: %v", name, subName, err)
		}
		if _, dup := seen[clean]; dup {
			return invalid("FIELD_COMPOSITE_SUBFIELD_DUPLICATE",
				"field composite %q có sub-field %q bị khai trùng", name, clean)
		}
		seen[clean] = struct{}{}

		subType, _ := sub["fieldType"].(string)
		if _, ok := compositeSubAllowedTypes[subType]; !ok {
			return invalid("FIELD_COMPOSITE_SUBFIELD_INVALID",
				"field composite %q có sub-field %q với kiểu %q không hỗ trợ trong composite", name, clean, subType)
		}
		// specs/052: record-time never reads a sub-field's dependent config (the pair check walks
		// top-level fields only), so a sub-field carrying it would be config that silently does nothing.
		if hasDependentKeys(optionsOf(sub["options"])) {
			return invalid("FIELD_COMPOSITE_SUBFIELD_INVALID",
				"field composite %q có sub-field %q khai picklist phụ thuộc — chưa hỗ trợ trong composite", name, clean)
		}
		// Validate the sub-field's own nested config (e.g. a multi_select sub needs values,
		// a currency sub needs currencyCode). compositeSubAllowedTypes already excludes
		// composite/relation, so this recursion terminates at depth 1.
		if err := validateFieldConfig(clean, subType, optionsOf(sub["options"])); err != nil {
			return invalid("FIELD_COMPOSITE_SUBFIELD_INVALID",
				"field composite %q có sub-field %q cấu hình sai: %v", name, clean, err)
		}
	}
	return nil
}

// optionsOf coerces a JSON-decoded options value to map[string]any, returning nil for any
// non-object (including a missing key). Used when descending into a composite sub-field's
// own options.
func optionsOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// boolOpt reads a boolean flag from an options map by key, returning false for any
// non-boolean or missing value. Used to read options["indexed"] (D1) without panicking
// on heterogeneous JSON-decoded maps.
func boolOpt(opts map[string]any, key string) bool {
	v, ok := opts[key]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// genColumnTextMaxLength is the VARCHAR length of the STORED generated column the DDL builder
// materializes for a unique/indexed string-ish field (text/rich_text/email/picklist). Kept in
// sync with the record-time bound (services.genColTextMaxRunes) and the DDL builder.
const genColumnTextMaxLength = 255

// isGenColumnTextType reports whether a field type materializes a VARCHAR generated column
// when unique or indexed (and is therefore subject to the 255 maxLength config cap, W5-B2).
func isGenColumnTextType(fieldType string) bool {
	switch fieldType {
	case "text", "rich_text", "email", "picklist":
		return true
	default:
		return false
	}
}

// validateGenColumnMaxLength rejects options.maxLength > 255 on a unique OR indexed string-ish
// field at config time (W5-B2): such a field gets a VARCHAR(255) generated column, so a larger
// declared maxLength is a promise the physical column cannot keep (it would truncate/1406 at
// write). Plain JSON-only fields (neither unique nor indexed) carry no generated column and may
// declare any positive maxLength. Called from validateField (add-field) and ValidateCreateEntity.
func validateGenColumnMaxLength(name, fieldType string, isUnique bool, options map[string]any) error {
	if !isGenColumnTextType(fieldType) {
		return nil
	}
	if !isUnique && !boolOpt(options, "indexed") {
		return nil
	}
	mlRaw, ok := options["maxLength"].(float64)
	if !ok {
		return nil
	}
	if mlRaw > genColumnTextMaxLength {
		return invalid("FIELD_MAXLENGTH_EXCEEDS_COLUMN",
			"field %q là unique/indexed nên options.maxLength không được vượt %d (cột VARCHAR(%d))",
			name, genColumnTextMaxLength, genColumnTextMaxLength)
	}
	return nil
}

// isFileFieldType reports whether a field type is a file-attachment type (File-upload M1). Such
// a field stores a JSON array of [{id,name}] references in `data` and has no scalar projection,
// so it can never back a unique/indexed generated column.
func isFileFieldType(fieldType string) bool {
	return fieldType == "file" || fieldType == "image"
}

// validateFileFieldFlags rejects a file/image field declared unique or indexed (File-upload M1).
// A file value is a [{id,name}] JSON array with no scalar form, so it cannot materialize the
// STORED generated column unique/indexed require — allowing the flag would either trip an opaque
// DDL error or silently mis-index. Caught at config time (entity-create + add-field) so the
// contract is symmetric. unique is the field-level flag; indexed is read from options.
func validateFileFieldFlags(name, fieldType string, isUnique bool, options map[string]any) error {
	if !isFileFieldType(fieldType) {
		return nil
	}
	if isUnique {
		return invalid("FIELD_FILE_NOT_UNIQUE",
			"field %q kiểu %s không thể đặt unique (giá trị là mảng tham chiếu file)", name, fieldType)
	}
	if boolOpt(options, "indexed") {
		return invalid("FIELD_FILE_NOT_INDEXED",
			"field %q kiểu %s không thể đánh index (giá trị là mảng tham chiếu file)", name, fieldType)
	}
	return nil
}

// isComputedFieldType reports whether a field type is server-computed at record-write time
// (formula) or read time (lookup/rollup, ENG-COMPUTED #2/#3). None of the three ever land a
// value in the JSON `data` column at write time — formula writes its result there but ONLY via
// applyComputedFields server-side; lookup/rollup never write to `data` at all, they resolve
// fresh on every read — so none can back a STORED generated column.
func isComputedFieldType(fieldType string) bool {
	return fieldType == "lookup" || fieldType == "rollup"
}

// validateComputedFieldFlags rejects a lookup/rollup field declared unique or indexed
// (ENG-COMPUTED #2/#3). A lookup/rollup value is resolved at READ time and never written to the
// JSON `data` column, so it cannot materialize the STORED generated column unique/indexed
// require — mirrors validateFileFieldFlags for file/image. unique is the field-level flag;
// indexed is read from options.
func validateComputedFieldFlags(name, fieldType string, isUnique bool, options map[string]any) error {
	if !isComputedFieldType(fieldType) {
		return nil
	}
	if isUnique {
		return invalid("FIELD_COMPUTED_NOT_UNIQUE",
			"field %q kiểu %s là computed (read-time) nên không thể đặt unique", name, fieldType)
	}
	if boolOpt(options, "indexed") {
		return invalid("FIELD_COMPUTED_NOT_INDEXED",
			"field %q kiểu %s là computed (read-time) nên không thể đánh index", name, fieldType)
	}
	return nil
}

// maxFieldsPerEntity bounds the field list a single CreateEntity request may declare
// (ENG-REL2 #4 security audit, defense-in-depth) AND the live field count AddField may grow
// an entity to (FIELD_LIMIT_EXCEEDED). Each relation field runs pre-flight metadata queries
// before any DDL; the cap keeps one request from fanning out into an unbounded query storm.
// Generous by design — builtin vertical packs use ~15 fields/entity.
const maxFieldsPerEntity = 200

// validateRelationFieldFlags rejects a relation field declared unique (ENG-REL2 #4, review
// MED-1). AddField's DDL dispatch tests `case IsUnique` BEFORE `case relation`, so an allowed
// relation+unique would materialize a VARCHAR(255) unique column named <field> — NOT the
// CHAR(36) <field>_id FK column — while the 1-N registry row (written independently of the
// dispatch) still advertises child_fk_column="<field>_id": a physical/registry drift, plus an
// ErrDuplicateRelationship in that branch would map to an opaque 500 instead of 409. Plan #5
// models 1-1 exclusivity via options.cardinality (tenant-scoped UNIQUE on the FK column), so
// the field-level unique flag stays permanently meaningless for relation fields.
func validateRelationFieldFlags(name, fieldType string, isUnique bool) error {
	if fieldType != "relation" {
		return nil
	}
	if isUnique {
		return invalid("FIELD_RELATION_UNIQUE_UNSUPPORTED",
			"field relation %q không thể đặt unique — quan hệ 1-1 dùng options.cardinality (xem api-conventions §6)", name)
	}
	return nil
}

// validateUserFieldFlags rejects a `user` field declared unique (USER-FIELD slice). TWO
// independent reasons, mirroring validateRelationFieldFlags's for `relation`:
//  1. Dispatch-order hazard: AddField's DDL switch tests `case IsUnique` BEFORE `case
//     FieldType == "user"` (field_orchestration.go) — an allowed user+unique field would
//     materialize the WRONG physical column (a VARCHAR(255) unique gencol named <field>,
//     not the CHAR(36) <field>_id FK column), a physical-shape drift.
//  2. Semantic: a `user` field means "assigned to" — many records are routinely assigned to
//     the SAME user (a rep's whole book of accounts). Unlike a 1-1 `relation` (which models
//     exclusivity via options.cardinality), `user` offers no cardinality option in this slice —
//     it is always many-to-one, so the field-level unique flag has no meaningful shape to
//     express and stays permanently unsupported.
func validateUserFieldFlags(name, fieldType string, isUnique bool) error {
	if fieldType != "user" {
		return nil
	}
	if isUnique {
		return invalid("FIELD_USER_UNIQUE_UNSUPPORTED",
			"field user %q không thể đặt unique — nhiều bản ghi có thể cùng được gán cho một người dùng", name)
	}
	return nil
}

// validateTextConfig enforces the optional structural rules for text and rich_text fields
// (RM-29 config-time). It is called from validateFieldConfig so bad config is caught at
// entity-create, add-field, AND edit-field time.
//
//   - options.maxLength, when present, must be a positive integer
//     (FIELD_TEXT_MAXLENGTH_INVALID).
//   - options.pattern, when present, must compile as a valid Go/RE2 regular expression
//     (FIELD_TEXT_PATTERN_INVALID). The anchored form used at record-time is tested here.
func validateTextConfig(name string, options map[string]any) error {
	if options == nil {
		return nil
	}
	if mlRaw, ok := options["maxLength"]; ok {
		ml, ok := mlRaw.(float64)
		if !ok || ml != float64(int(ml)) || ml <= 0 {
			return invalid("FIELD_TEXT_MAXLENGTH_INVALID",
				"field %q có options.maxLength không hợp lệ (cần số nguyên dương)", name)
		}
	}
	if patRaw, ok := options["pattern"]; ok {
		pat, ok := patRaw.(string)
		if !ok || strings.TrimSpace(pat) == "" {
			return invalid("FIELD_TEXT_PATTERN_INVALID",
				"field %q có options.pattern không hợp lệ (cần chuỗi regex không rỗng)", name)
		}
		// Test the anchored form used at record-time so a pattern that only fails after
		// anchoring (e.g. look-ahead not supported by RE2) is caught here.
		if _, err := regexp.Compile("^(?:" + pat + ")$"); err != nil {
			return invalid("FIELD_TEXT_PATTERN_INVALID",
				"field %q có options.pattern không compile được: %v", name, err)
		}
	}
	return nil
}

// validateNumberConfig enforces the optional structural rules for number fields (RM-29
// config-time). It is called from validateFieldConfig so bad config is caught at
// entity-create, add-field, AND edit-field time.
//
//   - options.min and options.max, when both present, must satisfy min <= max
//     (FIELD_NUMBER_RANGE_INVALID).
//   - options.step, when present, must be a positive number (FIELD_NUMBER_STEP_INVALID).
func validateNumberConfig(name string, options map[string]any) error {
	if options == nil {
		return nil
	}
	var minVal, maxVal float64
	hasMin, hasMax := false, false
	if minRaw, ok := options["min"]; ok {
		v, ok := minRaw.(float64)
		if !ok {
			return invalid("FIELD_NUMBER_RANGE_INVALID",
				"field %q có options.min không phải số", name)
		}
		minVal, hasMin = v, true
	}
	if maxRaw, ok := options["max"]; ok {
		v, ok := maxRaw.(float64)
		if !ok {
			return invalid("FIELD_NUMBER_RANGE_INVALID",
				"field %q có options.max không phải số", name)
		}
		maxVal, hasMax = v, true
	}
	if hasMin && hasMax && minVal > maxVal {
		return invalid("FIELD_NUMBER_RANGE_INVALID",
			"field %q có options.min (%v) lớn hơn options.max (%v)", name, minVal, maxVal)
	}
	if stepRaw, ok := options["step"]; ok {
		step, ok := stepRaw.(float64)
		if !ok || step <= 0 {
			return invalid("FIELD_NUMBER_STEP_INVALID",
				"field %q có options.step không hợp lệ (cần số dương)", name)
		}
	}
	return nil
}

// ValidateFieldDefaultNotSupported rejects a non-empty default value for field types that
// cannot express a meaningful scalar default (RM-33a). Called by field_service.go (AddField
// / EditField) after validateFieldConfig so config-time + default-time share the same gate.
//
// Design choice (RM-33):
//   - currency: the default would need to encode {amount, currencyCode} as a string — no
//     compact, unambiguous representation exists; reject with FIELD_DEFAULT_NOT_SUPPORTED.
//   - rich_text: already handled in defaultValue's string case (it goes through checkType
//     which only validates string type, not content length/pattern at default-time). The
//     spec marks it as unsupported default; we reject here for parity.
func ValidateFieldDefaultNotSupported(fieldType, defaultVal string) error {
	if strings.TrimSpace(defaultVal) == "" {
		return nil
	}
	if fieldTypeRejectsDefault(fieldType) {
		return invalid("FIELD_DEFAULT_NOT_SUPPORTED",
			"kiểu field %q không hỗ trợ default value (dùng placeholder UI thay thế)", fieldType)
	}
	return nil
}

// fieldTypeRejectsDefault is the type list behind ValidateFieldDefaultNotSupported, shared within
// the package so the field-ACL lock guard (field_acl_admin.go) can tell an admin the truth about
// the way out: for these types "set a default first" is advice the builder will refuse.
func fieldTypeRejectsDefault(fieldType string) bool {
	switch fieldType {
	case "currency", "rich_text", "lookup", "rollup", "auto_number", "polymorphic_relation", "user",
		"composite", "file", "image":
		// composite / file / image (queue #62): same accept-then-swallow as `user` below —
		// defaultValue has no branch for them (no scalar form), so a stored default never reached
		// a record. It did reach the field-ACL lock guard, which read the non-empty string as
		// "the engine fills this required field" and allowed a lock that left the role unable to
		// create anything. TestFieldDefault_ConfigGateMatchesRecordTime pins the parity.
		// user (MOT-XUONG TRỤ 2 #1): a default would be a hard-coded user id — it dangles the
		// moment that person leaves, and record_service.defaultValue has no branch for "user"
		// so it was ACCEPTED at config-time then silently never applied at Create. Reject at the
		// trust boundary instead of swallowing (bug-class "accept-then-swallow", found by review).
		// polymorphic_relation (ACTIVITY-CORE #3a): {type,id} object pointing at a live record —
		// no meaningful scalar default (a default parent would dangle); use the FE attach-picker.
		// currency: {amount,currencyCode} cannot be encoded as a scalar default string.
		// rich_text: HTML/markdown blobs may be arbitrarily large and contain structure
		// (RM-33a); use a UI placeholder/hint instead of a stored default.
		// lookup/rollup (ENG-COMPUTED #2/#3): server-computed at read time — a client-supplied
		// default would never be used (the value is always resolved fresh), so a non-empty
		// default is a config mistake, not a silently-ignored one.
		// auto_number (ENG #6): server-ASSIGNED at Create time (services.assignSequences) —
		// same reasoning as lookup/rollup, a client-supplied default would never be used.
		return true
	}
	return false
}

// validateFormulaRefs checks every formula field in the provided field list against
// its sibling fields. This is the orchestration-layer counterpart of validateFormulaConfig
// (which only does a syntax check): it confirms every identifier in the formula names an
// existing sibling field of type "number" or "currency" (not another formula — single-pass
// MVP). Called from CreateEntity (has all fields) and AddField (passes existing + new field).
//
// Errors returned use the codes defined in the spec:
//   - FIELD_FORMULA_INVALID    — expression does not parse (should not reach here after
//     validateFormulaConfig, but guarded defensively).
//   - FIELD_FORMULA_TYPE_MISMATCH — boolean/number mixed (same defensive note).
//   - FIELD_FORMULA_UNKNOWN_REF — identifier not in the field list / self-reference.
//   - FIELD_FORMULA_NON_NUMERIC_REF — referenced field is not number/currency.
//   - FIELD_FORMULA_NESTED     — referenced field is another formula.
func validateFormulaRefs(fields []FieldInput, lookupTypes map[string]string) error {
	// Build the FieldInfo slice the formula.Validate function expects; lookupTypes (lookup name →
	// type of the value it mirrors, see lookupValueTypes) lets a formula read a numeric lookup.
	infos := formulaInfos(fields, lookupTypes)

	for _, f := range fields {
		if f.FieldType != "formula" {
			continue
		}
		expr, _ := f.Options["formula"].(string)
		if strings.TrimSpace(expr) == "" {
			// validateFormulaConfig already rejects blank exprs; skip here.
			continue
		}
		if _, err := formula.Validate(expr, f.Name, infos); err != nil {
			return formulaValidationError(f.Name, err)
		}
	}
	return nil
}

// formulaValidationError converts a formula.Validate error into the typed ValidationError
// the field-create path returns (code extracted from the "CODE: message" prefix). Shared by
// validateFormulaRefs and the W5 POST /admin/formula/validate dry-run endpoint so both emit
// the SAME codes/messages — the live formula editor sees exactly what save would reject.
func formulaValidationError(fieldName string, err error) error {
	msg := err.Error()
	code := extractFormulaErrCode(msg)
	// The code travels in Code; the sentence a person reads in Studio must not start with it.
	human := strings.TrimPrefix(msg, code+": ")
	return &ValidationError{
		Code:    code,
		Msg:     fmt.Sprintf("công thức %q: %s", fieldName, human),
		Details: map[string]any{},
	}
}

// extractFormulaErrCode extracts the FIELD_FORMULA_* code from a formula.Validate error
// message. formula.Validate formats errors as "CODE: human message". If no recognized code
// prefix is found, falls back to FIELD_FORMULA_INVALID.
func extractFormulaErrCode(msg string) string {
	for _, code := range []string{
		"FIELD_FORMULA_INVALID",
		"FIELD_FORMULA_TYPE_MISMATCH",
		"FIELD_FORMULA_UNKNOWN_REF",
		"FIELD_FORMULA_NON_NUMERIC_REF",
		"FIELD_FORMULA_NESTED",
	} {
		if strings.HasPrefix(msg, code+":") {
			return code
		}
	}
	return "FIELD_FORMULA_INVALID"
}

// validateLookupRefs checks every lookup field in the provided field list against its sibling
// fields (ENG-COMPUTED #2). This is the orchestration-layer counterpart of validateLookupConfig
// (which only checks via/sourceField are non-empty strings): it confirms options.via names an
// existing SIBLING field of type "relation" (not itself another lookup/formula/rollup masquerading
// as a relation — impossible today since those types are never "relation", but checked by type
// name for clarity), and that the relation is to-one (every `relation` field is FK-on-child /
// to-one today — there is no N-N sys_fields type — so this also future-proofs against a relation
// field whose options.cardinality marks it multi-target, e.g. a future 1-1 inverse-N side).
// sourceField's cycle guard (must not name another lookup/formula/rollup ON THE TARGET entity)
// needs the TARGET entity's field list — a DB read — so it lives in validateLookupSourceField
// (CreateEntity / AddField), not here.
//
// Called from CreateEntity (has all fields of THIS entity) and AddField (passes existing + new
// field) — exactly mirroring validateFormulaRefs.
func validateLookupRefs(fields []FieldInput) error {
	byName := make(map[string]FieldInput, len(fields))
	for _, f := range fields {
		byName[f.Name] = f
	}

	for _, f := range fields {
		if f.FieldType != "lookup" {
			continue
		}
		via, _ := f.Options["via"].(string)
		via = strings.TrimSpace(via)
		if via == "" {
			continue // validateLookupConfig already rejects a blank via; skip here.
		}
		sib, ok := byName[via]
		if !ok {
			return invalidField("FIELD_LOOKUP_VIA_UNKNOWN", f.Name,
				"field lookup %q có options.via %q không tồn tại trên entity này", f.Name, via)
		}
		if sib.FieldType != "relation" {
			return invalidField("FIELD_LOOKUP_VIA_NOT_RELATION", f.Name,
				"field lookup %q có options.via %q không phải field relation (kiểu %q)", f.Name, via, sib.FieldType)
		}
		// Defense-in-depth / forward-compat: reject a relation explicitly marked
		// multi-target (e.g. a future inverse/"many" side via options.cardinality). Every
		// `relation` field today is FK-on-child / to-one from the CHILD's perspective
		// regardless of its declared cardinality — "1-N" (the default/explicit ordinary
		// shape) and "1-1" (ENG-REL2 #5, exclusivity via a tenant-scoped UNIQUE on the same
		// FK column) are BOTH to-one, so neither is rejected here. validateRelationCardinality
		// (config-time) now rejects any OTHER value on a NEWLY declared/edited relation field,
		// so "N-N"/"N-1-inverse" can only reach this branch via a sibling field's ALREADY
		// STORED options (data written before that lock existed, or the still-hypothetical
		// parent-side "many" field a future milestone might add) — this guard stays as the
		// belt for that legacy/forward case.
		if card, _ := sib.Options["cardinality"].(string); card == "N-N" || card == "N-1-inverse" {
			return invalidField("FIELD_LOOKUP_VIA_MUST_BE_TO_ONE", f.Name,
				"field lookup %q có options.via %q là quan hệ nhiều-đối-tượng, lookup chỉ hỗ trợ quan hệ to-one", f.Name, via)
		}
	}
	return nil
}
