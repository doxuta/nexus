package services

import (
	"context"
	"errors"
	"fmt"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/formula"
	"nexus/platform/internal/metadata"
)

// ValidateFormulaInput is the payload for POST /admin/formula/validate (W5 live formula
// editor dry-run). EntityName scopes the sibling-field ref check (tenant-scoped);
// Formula is the expression; ResultType is optional ("number" | "currency" | "text", default "number").
type ValidateFormulaInput struct {
	EntityName string
	Formula    string
	ResultType string
}

// FormulaValidation is the success result of a formula dry-run: the referenced sibling
// field names (in first-appearance order, deduplicated — exactly what formula.Validate
// returns to the field-create path) and the effective resultType.
type FormulaValidation struct {
	Valid      bool     `json:"valid"`
	Fields     []string `json:"fields"`
	ResultType string   `json:"resultType"`
}

// formulaPreviewFieldName is the placeholder field name used in dry-run error messages.
// The endpoint validates an expression BEFORE its field exists, so there is no real name;
// the codes are identical to the create path, only this display name differs.
const formulaPreviewFieldName = "formula"

// ValidateFormula dry-runs the config-time formula validation the field-create path performs
// (W5 §3.2 "validate-before-save"), WITHOUT writing anything. It reuses the exact same
// validators — validateFormulaConfig (syntax + resultType) and formula.Validate wrapped by
// formulaValidationError (ref checks) — so the live editor sees the SAME 422 codes/messages
// a real POST /admin/fields would return:
//
//	FIELD_FORMULA_INVALID · FIELD_FORMULA_TYPE_MISMATCH · FIELD_FORMULA_UNKNOWN_REF ·
//	FIELD_FORMULA_NON_NUMERIC_REF · FIELD_FORMULA_NESTED  (+ ENTITY_NOT_FOUND 404 for an
//	unknown/cross-tenant entity).
//
// The self-reference guard uses an empty selfName: the field being edited may not exist yet,
// and "" can never be a formula identifier, so every identifier is checked purely against
// the entity's ACTIVE sibling fields (a formula field among them still trips NESTED).
func (s *FieldService) ValidateFormula(ctx context.Context, tenantID string, in ValidateFormulaInput) (*FormulaValidation, error) {
	entityName, err := ddl.SanitizeIdentifier(in.EntityName)
	if err != nil {
		return nil, invalid("ENTITY_INVALID_NAME", "tên entity không hợp lệ: %v", err)
	}

	// Structural check first (same order as validateField): options.formula non-empty +
	// parses and matches options.resultType ∈ {number, currency, text}. Building the options map
	// exactly like a field-create payload keeps validateFormulaConfig the single source.
	options := map[string]any{"formula": in.Formula}
	if in.ResultType != "" {
		options["resultType"] = in.ResultType
	}
	if err := validateFormulaConfig(formulaPreviewFieldName, options); err != nil {
		return nil, err
	}

	// Resolve the entity tenant-scoped — unknown/cross-tenant name → 404.
	entity, err := s.meta.GetEntityByName(ctx, tenantID, entityName)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại", entityName)
		}
		return nil, fmt.Errorf("services.ValidateFormula: resolve entity: %w", err)
	}

	// Ref check against the entity's ACTIVE fields — the same infos the create path builds.
	active, err := s.meta.ListFields(ctx, tenantID, entity.ID)
	if err != nil {
		return nil, fmt.Errorf("services.ValidateFormula: list fields: %w", err)
	}
	lookupTypes, err := ResolveLookupTypes(ctx, s.meta, tenantID, entity.Name, active)
	if err != nil {
		return nil, fmt.Errorf("services.ValidateFormula: %w", err)
	}
	refs, err := formula.Validate(in.Formula, "", formulaFieldInfos(active, lookupTypes))
	if err != nil {
		return nil, formulaValidationError(formulaPreviewFieldName, err)
	}
	if refs == nil {
		refs = []string{}
	}

	resultType := in.ResultType
	if resultType == "" {
		resultType = "number" // record-time default — see computed_fields.go
	}
	return &FormulaValidation{Valid: true, Fields: refs, ResultType: resultType}, nil
}

// formulaFieldInfos is the sibling list formula.Validate checks identifiers against, built from
// an entity's ACTIVE field definitions. Shared by the dry-run above and the edit path
// (ValidateEditFieldChanges) so both judge refs against the same view of the entity.
func formulaFieldInfos(fields []metadata.FieldDefinition, lookupTypes map[string]string) []formula.FieldInfo {
	infos := make([]formula.FieldInfo, 0, len(fields))
	for _, f := range fields {
		infos = append(infos, formula.FieldInfo{Name: f.Name, FieldType: f.FieldType, ValueType: lookupTypes[f.Name]})
	}
	return infos
}

// ResolveLookupTypes maps each lookup among an entity's fields to the type of the value it
// mirrors — what a formula over that lookup needs to be vetted (queue #284). Exported for the AI
// Builder's propose_edit_field, which runs ValidateEditFieldChanges with the same inputs.
func ResolveLookupTypes(ctx context.Context, meta interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}, tenantID, entityName string, fields []metadata.FieldDefinition) (map[string]string, error) {
	inputs, err := liveFieldInputs(fields)
	if err != nil {
		return nil, fmt.Errorf("services.ResolveLookupTypes: %w", err)
	}
	return lookupValueTypes(ctx, meta, tenantID, entityName, inputs)
}
