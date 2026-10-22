package services

import (
	"fmt"
	"strings"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/formula"
	"nexus/platform/internal/metadata"
)

// AddFieldInput is the payload to add a single field to an already-registered entity.
// EntityName resolves the target entity (tenant-scoped); Field is the new field definition.
type AddFieldInput struct {
	EntityName string
	Field      FieldInput
}

// ValidateAddField checks an add-field payload before any DB/DDL work runs: the target
// entity name must be a valid identifier, and the field must pass the same name/type/
// picklist rules enforced at entity-create time. It returns a *ValidationError (with a
// code) on the first problem, or nil.
func ValidateAddField(in AddFieldInput) error {
	if _, err := ddl.SanitizeIdentifier(in.EntityName); err != nil {
		return invalid("ENTITY_INVALID_NAME", "tên entity không hợp lệ: %v", err)
	}
	return validateField(in.Field)
}

// validateField applies the per-field rules shared by entity-create and add-field:
// identifier allowlist, supported field type, per-type nested-JSON config (picklist
// values, multi_select options, currency/rating config, relation target, composite
// sub-fields) — all delegated to validateFieldConfig so the two paths never drift.
// RM-33a: currency default is also rejected here if a DefaultVal is provided.
func validateField(f FieldInput) error {
	name, err := ddl.SanitizeIdentifier(f.Name)
	if err != nil {
		return invalid("FIELD_INVALID_NAME", "tên field %q không hợp lệ: %v", f.Name, err)
	}
	// W5-C1: a dynamic field must not be named after a mandatory system column
	// (id/tenant_id/team_set_id/created_by/modified_by/version/created_at/updated_at/
	// deleted_at/data). Reject BEFORE any DDL: a name like "version" otherwise silently
	// shadows RM-24 optimistic locking (every write looks like a 409), and a system-column
	// collision trips ER_DUP_FIELDNAME (1060) → opaque 500. Single chokepoint for both
	// entity-create and add-field.
	if ddl.IsReservedColumn(name) {
		return invalid("FIELD_NAME_RESERVED",
			"tên field %q trùng cột hệ thống bắt buộc — chọn tên khác", name)
	}
	if err := checkFieldType(f.FieldType); err != nil {
		return err
	}
	if err := validateFieldConfig(name, f.FieldType, f.Options); err != nil {
		return err
	}
	// W5 builder v2: fieldOrder ≥ 0 when supplied; description ≤ 500 runes. Same gate as
	// entity-create (ValidateCreateEntity) so the two paths never drift.
	if err := validateFieldOrderDescription(name, f.FieldOrder, f.Description); err != nil {
		return err
	}
	// W5-B2 config-time: a unique/indexed text-ish field materializes a VARCHAR(255) generated
	// column, so options.maxLength may not exceed 255 (it would promise a length the physical
	// column cannot hold → silent truncation / 1406 at write). Plain JSON-only fields have no
	// such cap. indexed is read from options; unique from the field flag.
	if err := validateGenColumnMaxLength(name, f.FieldType, f.IsUnique, f.Options); err != nil {
		return err
	}
	// Type flag guards (file/image, computed, relation, auto_number, polymorphic cannot be
	// unique/indexed) — ONE shared gate with entity-create + edit-field so they never drift.
	if err := validateFieldFlagGuards(name, f.FieldType, f.IsUnique, f.Options); err != nil {
		return err
	}
	// RM-33a: reject a non-empty DefaultVal for types that cannot express a scalar default.
	// FieldInput carries no DefaultVal field (defaults are set via EditField / entity-create
	// does not yet accept defaults), so this is a forward-compat guard for when FieldInput
	// grows a DefaultVal. Currently a no-op but keeps validateField as the single gate.
	return nil
}

// EditFieldInput is the payload to edit an existing field, addressed by FieldID. Only
// label / isRequired / defaultValue / options are mutable; FieldType is carried solely so
// the service can detect (and reject, 422) an attempt to change it — it is never written.
//
// Label (*string), IsRequired (*bool) and DefaultVal (*string) use apply-when-non-nil
// semantics so a partial PUT body can update one attribute without wiping the others (spec
// §7.2 B-2): a nil pointer means "field omitted, keep stored value"; a non-nil pointer (even
// to the zero value) is an explicit set. Options follows the same rule (nil map = unchanged).
type EditFieldInput struct {
	FieldID    string
	Label      *string
	IsRequired *bool
	DefaultVal *string
	Options    map[string]any
	// FieldType echoes the caller's intended type. Empty means "unchanged"; a non-empty
	// value different from the stored type is rejected as FIELD_TYPE_IMMUTABLE.
	FieldType string
	// FieldOrder / Description (W5) are MUTABLE (unlike fieldType) and follow the same
	// apply-when-non-nil semantics as Label/IsRequired: nil = keep stored value; a non-nil
	// pointer is an explicit set (FieldOrder ≥ 0, Description ≤ 500 runes — validated with
	// the same codes as create/add: FIELD_ORDER_INVALID / FIELD_DESCRIPTION_TOO_LONG).
	FieldOrder  *int
	Description *string
	// Indexed (#56) sets options.indexed and nothing else of the options: EditField lays it onto the
	// options as the caller reads them (the builder's draft overlay included), so a pending field
	// draft keeps its options while the flag applies live. Sent with Options it is a 422.
	Indexed *bool
}

// ValidateEditField checks an edit payload before any DB work runs: the field id must be
// present, and — when the (resolved) field is a picklist — the new options must still
// carry a non-empty values list. fieldType immutability is enforced in EditField, which
// needs the stored type to compare against. Returns a *ValidationError or nil.
func ValidateEditField(in EditFieldInput, storedFieldType string) error {
	if strings.TrimSpace(in.FieldID) == "" {
		return invalid("FIELD_ID_REQUIRED", "thiếu id field cần sửa")
	}
	// The effective type after the edit is the stored type (fieldType is immutable). If a
	// picklist's options are being changed, they must remain non-empty.
	if storedFieldType == "picklist" && in.Options != nil && !hasPicklistValues(in.Options) {
		return invalid("FIELD_PICKLIST_NO_OPTIONS", "field picklist cần options.values không rỗng")
	}
	// W5: fieldOrder ≥ 0 / description ≤ 500 runes — same single-source gate as create/add.
	// The field name is not known at this pre-resolve stage; EditField re-runs nothing here
	// (order/description carry no per-type rules), so a generic placeholder name is fine.
	var desc string
	if in.Description != nil {
		desc = *in.Description
	}
	if err := validateFieldOrderDescription(in.FieldID, in.FieldOrder, desc); err != nil {
		return err
	}
	return nil
}

// ValidateEditFieldChanges runs the FULL pre-DDL validation-and-apply sequence of an edit
// against an already-resolved stored field: fieldType immutability (FIELD_TYPE_IMMUTABLE),
// ValidateEditField, per-type options config + flag guards, relation-options immutability
// (FIELD_RELATION_OPTIONS_IMMUTABLE), then the mutable attributes applied onto a COPY and
// the effective default re-validated (RM-33a + defaultValue coercion). It is THE single
// body both the apply path (FieldService.EditField, PUT /admin/fields/:id) and the AI
// Builder's propose_edit_field run — the M6/M7 validator-parity extraction pattern — so
// propose and apply can never drift on what an acceptable edit is.
//
// siblings is the entity's ACTIVE field list (the edited field may be among it). Only a formula
// field reads it — its identifiers are checked against it (queue #107) — so callers fetch it for
// formula fields; nil for a formula whose expression changes fails closed (every name unknown).
// lookupTypes (lookup name → type of the value it mirrors; ResolveLookupTypes) lets that formula
// read a numeric lookup (queue #284); nil = every lookup reads as unresolved and is refused.
//
// It returns the EFFECTIVE (post-apply) field copy plus the parsed STORED options map: the
// apply path needs storedOpts for its D1 indexed-toggle diff, and the AI tool needs both
// sides for its old→new diff card. The stored field value is never mutated.
func ValidateEditFieldChanges(stored metadata.FieldDefinition, in EditFieldInput, siblings []metadata.FieldDefinition, lookupTypes map[string]string) (*metadata.FieldDefinition, map[string]any, error) {
	// fieldType is immutable. An empty FieldType means "unchanged"; a non-empty value that
	// differs from the stored type is a forbidden migration -> 422.
	if in.FieldType != "" && in.FieldType != stored.FieldType {
		// specs/063 S7-8 FR-061: a type change stays out of scope; the hint names the way out.
		return nil, nil, &ValidationError{
			Code:    "FIELD_TYPE_IMMUTABLE",
			Msg:     fmt.Sprintf("không thể đổi kiểu field từ %q sang %q (cần migrate dữ liệu)", stored.FieldType, in.FieldType),
			Details: map[string]any{"hint": "Thêm một trường mới đúng kiểu cần dùng, chuyển dữ liệu sang đó, rồi xoá trường cũ."},
		}
	}

	if err := ValidateEditField(in, stored.FieldType); err != nil {
		return nil, nil, err
	}

	// When the caller supplies options, re-run the single-source structural validator used
	// by entity-create and add-field (spec §7.2: edit must not bypass G3 composite /
	// picklist / multi_select / currency / rating / relation rules). The effective type is
	// the stored type (fieldType is immutable), and the field name is taken from storage —
	// neither is client-controlled here.
	if in.Options != nil {
		if err := validateFieldConfig(stored.Name, stored.FieldType, in.Options); err != nil {
			return nil, nil, err
		}
		// Flag guards on the EDIT path too (B1 + EDITFIELD-FLAG-PARITY): file/image/computed/
		// relation/auto_number/polymorphic fields cannot gain indexed/unique via options.
		if err := validateFieldFlagGuards(stored.Name, stored.FieldType, stored.IsUnique, in.Options); err != nil {
			return nil, nil, err
		}
	}

	storedOpts, err := parseOptionsJSON(stored.Options)
	if err != nil {
		return nil, nil, fmt.Errorf("services.ValidateEditFieldChanges: parse stored options: %w", err)
	}

	// specs/052: new options declaring a dependent picklist are checked against the live siblings
	// (parent exists + is a picklist, keys among its values, no cycle - which is also what stops a
	// PARENT edit from pointing back at its own child). Only when options change and carry the
	// keys. storedOpts lets it judge only what this edit changes: the child's later edits resend
	// the stored map verbatim, and a key the parent has since dropped must not freeze them (FR-4).
	// Shared by EditField and the AI Builder propose_edit_field preview (queue #230).
	if in.Options != nil && (hasDependentKeys(in.Options) || hasRelationFilter(in.Options)) {
		live, err := liveFieldInputs(siblings)
		if err != nil {
			return nil, nil, fmt.Errorf("services.ValidateEditFieldChanges: %w", err)
		}
		edited := FieldInput{Name: stored.Name, FieldType: stored.FieldType, Options: in.Options}
		if err := validateDependentRefs(edited, live, storedOpts); err != nil {
			return nil, nil, err
		}
		if err := validateRelationFilterRefs(edited, live, storedOpts); err != nil {
			return nil, nil, err
		}
	}

	// Queue #107: the sibling ref check create and add-field run (validateFormulaRefs) — every
	// identifier names an active number/currency sibling, not another formula, not itself.
	// Without it an edit to IF(amout > 1, 1, 0) (typo for amount) was saved and read 0 forever.
	// Judged only when the edit CHANGES the expression (queue #62 rule): DeleteField guards formula
	// dependants since #233, but a formula stored before that may already name a gone field, and resending
	// it unchanged (Studio and the AI Apply both send full options on a relabel) is not a new
	// mistake to refuse.
	if stored.FieldType == "formula" && in.Options != nil {
		expr, _ := in.Options["formula"].(string) // validateFieldConfig above: non-empty string
		if prev, _ := storedOpts["formula"].(string); expr != prev {
			if _, err := formula.Validate(expr, stored.Name, formulaFieldInfos(siblings, lookupTypes)); err != nil {
				return nil, nil, formulaValidationError(stored.Name, err)
			}
		}
	}

	// ENG-REL2 #4 (review MED-2): a relation field's relatedEntity/inverseField/cardinality are
	// pinned to its 1-N registry row — an options payload changing any of them is rejected 422.
	if stored.FieldType == "relation" && in.Options != nil {
		if err := validateRelationOptionsImmutable(stored.Name, storedOpts, in.Options); err != nil {
			return nil, nil, err
		}
	}

	// specs/083: what a stored roll-up totals is fixed once rows hold its numbers.
	if stored.FieldType == "rollup" && in.Options != nil {
		opts, err := validateStoredRollupImmutable(stored.Name, storedOpts, in.Options)
		if err != nil {
			return nil, nil, err
		}
		in.Options = opts
	}

	// Apply the mutable attributes onto a copy. Label/IsRequired/DefaultVal/FieldOrder/
	// Description use apply-when-non-nil semantics (spec §7.2 B-2); Options are only
	// rewritten when supplied.
	field := stored
	if in.Label != nil {
		field.Label = *in.Label
	}
	if in.IsRequired != nil {
		field.IsRequired = *in.IsRequired
	}
	if in.DefaultVal != nil {
		field.DefaultVal = *in.DefaultVal
	}
	if in.FieldOrder != nil {
		field.FieldOrder = *in.FieldOrder
	}
	if in.Description != nil {
		field.Description = *in.Description
	}
	if in.Options != nil {
		opts, err := optionsJSON(in.Options)
		if err != nil {
			return nil, nil, err
		}
		field.Options = opts
	}

	// Re-validate the EFFECTIVE default against the EFFECTIVE type+options (spec §7.2): a
	// default that is wrong for the field's type must be rejected NOW (422), not persisted
	// only to blow up at record-create. Same record-time coercion (defaultValue → checkType)
	// so edit and create never drift. RM-33a runs first so the unsupported-default error wins
	// over a confusing RECORD_INVALID_TYPE.
	//
	// queue #62: RM-33a judges the default this edit CHANGES, not one it merely carries. A stored
	// default of a type the gate rejects (rows saved before user/composite/file/image joined it)
	// is inert — defaultValue drops it. Re-judging it refused EVERY edit of the field, including
	// turning off "bắt buộc": the exit the field-ACL lock guard names. "Carries" includes sending
	// the stored value back verbatim: the AI Builder's Apply does exactly that (its diff card's
	// `after.defaultValue` is always on the wire). Clearing it (defaultValue: "") still passes.
	if in.DefaultVal != nil && *in.DefaultVal != stored.DefaultVal {
		if err := ValidateFieldDefaultNotSupported(field.FieldType, field.DefaultVal); err != nil {
			return nil, nil, err
		}
	}
	if strings.TrimSpace(field.DefaultVal) != "" {
		effOpts, err := parseOptionsJSON(field.Options)
		if err != nil {
			return nil, nil, fmt.Errorf("services.ValidateEditFieldChanges: parse effective options: %w", err)
		}
		spec := FieldSpec{
			Name:       field.Name,
			FieldType:  field.FieldType,
			Options:    effOpts,
			DefaultVal: field.DefaultVal,
		}
		if _, _, err := defaultValue(spec); err != nil {
			return nil, nil, err // a *ValidationError (e.g. RECORD_INVALID_TYPE/PICKLIST) -> 422
		}
	}

	return &field, storedOpts, nil
}

// DeleteFieldInput is the payload to soft-delete an existing field, addressed by FieldID.
// Deletion is a pure metadata operation (no DDL): the field row is marked deleted and the
// underlying obj_* data is preserved per §4.4.
type DeleteFieldInput struct {
	FieldID string
}

// ValidateAddFieldRefs is the cross-field half of AddField's config check (specs/052): a dependent
// picklist's controllingField must name a live sibling picklist, its map keys among that parent's
// values, no cycle. siblings are the entity's other fields (live rows, or the fields an AI draft
// declared this turn). Exported for the AI Builder propose_field preview (queue #230).
func ValidateAddFieldRefs(f FieldInput, siblings []FieldInput) error {
	if err := validateDependentRefs(f, siblings, nil); err != nil {
		return err
	}
	return validateRelationFilterRefs(f, siblings, nil)
}

// LiveFieldInputs converts stored field definitions to FieldInputs for ValidateAddFieldRefs.
func LiveFieldInputs(fields []metadata.FieldDefinition) ([]FieldInput, error) {
	return liveFieldInputs(fields)
}

// CheckDependentPicklists is the record-write pair check (FR-5/FR-6) for callers that judge a
// partial update before it is applied: clean is the validated partial payload, current the stored
// record. Exported for the propose_record_update preview (queue #230).
func CheckDependentPicklists(specs []FieldSpec, clean, current map[string]any) error {
	return checkDependentPicklists(specs, clean, current)
}
