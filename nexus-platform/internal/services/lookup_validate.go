package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/metadata"
)

// lookupMetaResolver is the minimal cross-entity metadata read surface
// validateLookupSourceField needs: resolve the target entity (the `via` relation's
// relatedEntity) and list its fields, to check that options.sourceField exists and is NOT
// itself another computed field. Satisfied by both MetadataWriter (CreateEntity) and
// FieldMetadataRepo (AddField) — mirrors rollupMetaResolver exactly (a strict subset of it).
type lookupMetaResolver interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}

// validateLookupSourceField closes the cycle guard validateLookupConfig's doc-comment
// promised but never implemented: options.sourceField must name an existing field on the
// TARGET entity (the `via` relation field's options.relatedEntity), and that field must NOT
// itself be another computed field (lookup/formula/rollup) — a lookup mirroring a
// lookup/formula/rollup is "computed of computed", the exact class validateRollupCrossEntity's
// aggField check already rejects for rollup (FIELD_FORMULA_NESTED). Without this guard a
// lookup whose sourceField names another lookup passes config-time validation silently, then
// resolves to null forever at read time (BatchFetchField reads `data.<sourceField>`, but a
// computed field is NEVER persisted into `data` — see ValidateRecord's readonly skip) — a
// silently-broken field an admin has no way to diagnose from the config alone.
//
// This is the orchestration-layer counterpart of validateLookupConfig (which only checks
// via/sourceField are non-empty strings) and validateLookupRefs (which checks `via` names a
// sibling to-one relation field) — together the three form the same split
// validateFormulaConfig/validateFormulaRefs/(cross-entity) uses for formula and rollup.
//
// f must be a lookup field whose options.via already resolved to a valid sibling relation
// field (i.e. validateLookupRefs has already passed) — via names the sibling relation field,
// relSpecs maps sibling relation-field name -> its FieldInput (used to read
// options.relatedEntity without a second DB round trip). selfFields, when non-nil, is the
// IN-MEMORY field list of a self-referential target (relatedEntity == the entity being
// created) — mirrors validateRollupCrossEntity's selfFields parameter for CreateEntity, where
// the target has not been persisted yet.
func validateLookupSourceField(
	ctx context.Context,
	meta lookupMetaResolver,
	tenantID string,
	relSpecs map[string]FieldInput,
	selfFields []FieldInput,
	selfEntityName string,
	f FieldInput,
) error {
	via, _ := f.Options["via"].(string)
	via = strings.TrimSpace(via)
	if via == "" {
		return nil // validateLookupConfig already rejects a blank via; skip here.
	}
	sourceField, _ := f.Options["sourceField"].(string)
	sourceField = strings.TrimSpace(sourceField)
	if sourceField == "" {
		return nil // validateLookupConfig already rejects a blank sourceField; skip here.
	}
	relSpec, ok := relSpecs[via]
	if !ok {
		return nil // validateLookupRefs already rejects an unknown/non-relation via; skip here.
	}
	target, _ := relSpec.Options["relatedEntity"].(string)
	target = strings.TrimSpace(target)
	if target == "" {
		return nil // malformed relation config — validateRelationTarget already rejects this.
	}

	// Resolve the target entity's field list. Self-referential lookup (target == the entity
	// being created, e.g. a "parent" relation pointing back at itself) reads the IN-MEMORY
	// field list instead of the DB — the row does not exist yet at CreateEntity time. Mirrors
	// validateRollupCrossEntity's selfRef branch exactly.
	var targetFields []metadata.FieldDefinition
	if target == selfEntityName && selfFields != nil {
		targetFields = make([]metadata.FieldDefinition, 0, len(selfFields))
		for _, sf := range selfFields {
			opts, err := optionsJSON(sf.Options)
			if err != nil {
				return fmt.Errorf("services.validateLookupSourceField: marshal self-field options: %w", err)
			}
			targetFields = append(targetFields, metadata.FieldDefinition{
				Name:      sf.Name,
				FieldType: sf.FieldType,
				Options:   opts,
			})
		}
	} else {
		targetEntity, err := meta.GetEntityByName(ctx, tenantID, target)
		if err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				// The relation's target-entity-exists check already runs elsewhere
				// (validateRelationTarget / CreateEntity's relation-target-exists loop); if we
				// get here with a not-found target, defer to that check rather than duplicating
				// FIELD_RELATION_TARGET_NOT_FOUND under a different code path.
				return nil
			}
			return fmt.Errorf("services.validateLookupSourceField: resolve target entity: %w", err)
		}
		targetFields, err = meta.ListFields(ctx, tenantID, targetEntity.ID)
		if err != nil {
			return fmt.Errorf("services.validateLookupSourceField: list target fields: %w", err)
		}
	}

	var srcField *metadata.FieldDefinition
	for i := range targetFields {
		if targetFields[i].Name == sourceField {
			srcField = &targetFields[i]
			break
		}
	}
	if srcField == nil {
		return invalidField("FIELD_LOOKUP_SOURCE_FIELD_UNKNOWN", f.Name,
			"field lookup %q có options.sourceField %q không tồn tại trên entity liên kết %q", f.Name, sourceField, target)
	}
	if srcField.FieldType == "lookup" || srcField.FieldType == "formula" || srcField.FieldType == "rollup" {
		return invalidField("FIELD_FORMULA_NESTED", f.Name,
			"field lookup %q có options.sourceField %q trỏ tới field computed khác (%q) trên entity liên kết — không hỗ trợ lookup lồng nhau", f.Name, sourceField, srcField.FieldType)
	}
	return nil
}
