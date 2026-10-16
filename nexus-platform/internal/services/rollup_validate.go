package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/formula"
	"nexus/platform/internal/metadata"
)

// rollupMetaResolver is the minimal cross-entity metadata read surface
// validateRollupCrossEntity needs: resolve the child entity, list its fields (to check
// viaRelation/aggField), and resolve a registered N-N relationship by name. Satisfied by both
// MetadataWriter (CreateEntity) and FieldMetadataRepo (AddField) — declared separately here so
// the cross-entity rollup check is a single source of truth shared by both orchestration paths,
// exactly like validateFormulaRefs / validateLookupRefs.
type rollupMetaResolver interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
	GetRelationshipByName(ctx context.Context, tenantID, name string) (*metadata.RelationshipDefinition, error)
}

// validateRollupCrossEntity validates a single rollup field's cross-entity references
// (ENG-COMPUTED #3) — the orchestration-layer counterpart of validateRollupConfig (which only
// checks the shape of options without any DB access):
//
//   - options.childEntity must exist for this tenant (FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND) —
//     UNLESS childEntity == parentEntityName (self-referential rollup, e.g. tasks rolling up
//     sub-tasks) AND selfFields is non-nil, in which case the child's field list is resolved
//     from the IN-MEMORY field list being created (selfFields) instead of a DB read — the
//     child (== parent) has not been persisted yet at CreateEntity time. Mirrors the
//     relation-target self-reference skip in CreateEntity (entity_orchestration.go), but
//     validates against selfFields instead of skipping outright.
//
//   - FK-on-child mode (options.viaRelation present): viaRelation must name an existing field
//     on the child entity (FIELD_ROLLUP_VIA_RELATION_UNKNOWN), that field must be type
//     "relation" (FIELD_ROLLUP_VIA_RELATION_NOT_RELATION), and its options.relatedEntity must
//     equal parentEntityName — i.e. it actually points back here (FIELD_ROLLUP_VIA_RELATION_WRONG_TARGET).
//
//   - N-N mode (options.relationship present): the named relationship must exist for this
//     tenant (FIELD_ROLLUP_RELATIONSHIP_NOT_FOUND) and its two sides must be exactly
//     {parentEntityName, childEntity} (FIELD_ROLLUP_RELATIONSHIP_WRONG_ENTITIES).
//
//   - aggField (required for every metric except count) must name an existing field on the
//     child entity (FIELD_ROLLUP_AGG_FIELD_UNKNOWN), of type number/currency/rating
//     (FIELD_ROLLUP_AGG_FIELD_TYPE_INVALID) — OR ROLLUP-OVER-FORMULA (2026-07-12 relaxation): a
//     `formula` field whose options.resultType is "number" (the default when resultType is
//     omitted, mirroring applyComputedFields/validateFormulaConfig). A number-resultType
//     formula's result is a plain float64 PERSISTED into obj_*.data at write time
//     (applyComputedFields runs on every Create/Update/import) — SUM/AVG/MIN/MAX reads it via
//     the exact same JSON_EXTRACT+CAST(DECIMAL) path as a real number field, so the aggregate is
//     genuinely correct, not a workaround. A `formula` field whose resultType is "currency" is
//     ALSO allowed (#262, 2026-09-30): it persists {amount,currencyCode} exactly like a currency
//     field, so the rollup sums its ".amount" sub-path and returns money (rollupAggCurrency). A
//     "text" formula stays REJECTED (FIELD_ROLLUP_AGG_FIELD_FORMULA_NOT_NUMBER) — it stores a string.
//     `lookup` and `rollup` aggFields are ALWAYS rejected (FIELD_FORMULA_NESTED — reused code,
//     mirrors the formula nested-ref cycle guard): both are genuinely compute-at-READ and never
//     land in `data` at all, so there is nothing for SUM to read.
func validateRollupCrossEntity(
	ctx context.Context,
	meta rollupMetaResolver,
	tenantID, parentEntityName string,
	selfFields []FieldInput,
	f FieldInput,
) error {
	child, _ := f.Options["childEntity"].(string)
	child = strings.TrimSpace(child)
	metric, _ := f.Options["metric"].(string)
	aggField, _ := f.Options["aggField"].(string)
	aggField = strings.TrimSpace(aggField)

	// Resolve the child entity's field list. Self-referential rollup (child == parent, e.g.
	// hierarchy/sub-tasks) reads the IN-MEMORY field list being created instead of the DB —
	// the row does not exist yet at CreateEntity time.
	var childFields []metadata.FieldDefinition
	selfRef := child == parentEntityName
	if selfRef && selfFields != nil {
		childFields = make([]metadata.FieldDefinition, 0, len(selfFields))
		for _, sf := range selfFields {
			opts, err := optionsJSON(sf.Options)
			if err != nil {
				return fmt.Errorf("services.validateRollupCrossEntity: marshal self-field options: %w", err)
			}
			childFields = append(childFields, metadata.FieldDefinition{
				Name:      sf.Name,
				FieldType: sf.FieldType,
				Options:   opts,
			})
		}
	} else {
		childEntity, err := meta.GetEntityByName(ctx, tenantID, child)
		if err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				return invalidField("FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND", f.Name,
					"field rollup %q có options.childEntity %q không tồn tại", f.Name, child)
			}
			return fmt.Errorf("services.validateRollupCrossEntity: resolve child entity: %w", err)
		}
		childFields, err = meta.ListFields(ctx, tenantID, childEntity.ID)
		if err != nil {
			return fmt.Errorf("services.validateRollupCrossEntity: list child fields: %w", err)
		}
	}
	childByName := make(map[string]metadata.FieldDefinition, len(childFields))
	for _, cf := range childFields {
		childByName[cf.Name] = cf
	}

	// --- Relationship mode: exactly one of viaRelation (1-N) / relationship (N-N). ---
	if viaRaw, ok := f.Options["viaRelation"]; ok {
		via, _ := viaRaw.(string)
		via = strings.TrimSpace(via)
		cf, ok := childByName[via]
		if !ok {
			return invalidField("FIELD_ROLLUP_VIA_RELATION_UNKNOWN", f.Name,
				"field rollup %q có options.viaRelation %q không tồn tại trên entity con %q", f.Name, via, child)
		}
		if cf.FieldType != "relation" {
			return invalidField("FIELD_ROLLUP_VIA_RELATION_NOT_RELATION", f.Name,
				"field rollup %q có options.viaRelation %q không phải field relation (kiểu %q) trên entity con %q", f.Name, via, cf.FieldType, child)
		}
		cfOpts, err := parseOptionsJSON(cf.Options)
		if err != nil {
			return fmt.Errorf("services.validateRollupCrossEntity: parse via-relation options: %w", err)
		}
		target, _ := cfOpts["relatedEntity"].(string)
		if strings.TrimSpace(target) != parentEntityName {
			return invalidField("FIELD_ROLLUP_VIA_RELATION_WRONG_TARGET", f.Name,
				"field rollup %q có options.viaRelation %q trên entity con %q trỏ về %q, không phải entity này (%q)",
				f.Name, via, child, target, parentEntityName)
		}
	} else if relRaw, ok := f.Options["relationship"]; ok {
		relName, _ := relRaw.(string)
		relName = strings.TrimSpace(relName)
		rel, err := meta.GetRelationshipByName(ctx, tenantID, relName)
		if err != nil {
			if errors.Is(err, metadata.ErrRelationshipNotFound) {
				return invalidField("FIELD_ROLLUP_RELATIONSHIP_NOT_FOUND", f.Name,
					"field rollup %q có options.relationship %q không tồn tại", f.Name, relName)
			}
			return fmt.Errorf("services.validateRollupCrossEntity: resolve relationship: %w", err)
		}
		// ENG-REL2 #4: GetRelationshipByName was broadened (migration 000028) to also resolve
		// 1-N registry rows. A rollup's options.relationship names an N-N join-table relation
		// ONLY (the 1-N case is expressed via options.viaRelation, handled in the branch above),
		// so a 1-N name here must be rejected exactly as it was invisible before the broadening.
		if rel.RelationshipType != "N-N" {
			return invalidField("FIELD_ROLLUP_RELATIONSHIP_NOT_FOUND", f.Name,
				"field rollup %q có options.relationship %q không tồn tại", f.Name, relName)
		}
		sides := map[string]struct{}{rel.ParentEntityName: {}, rel.ChildEntityName: {}}
		_, hasParent := sides[parentEntityName]
		_, hasChild := sides[child]
		if !hasParent || !hasChild {
			return invalidField("FIELD_ROLLUP_RELATIONSHIP_WRONG_ENTITIES", f.Name,
				"field rollup %q có options.relationship %q không nối entity này (%q) với entity con (%q)",
				f.Name, relName, parentEntityName, child)
		}
	}
	// Neither key present is already rejected by validateRollupConfig (structural pass runs
	// first in both CreateEntity and AddField) — unreachable here in practice, so no further
	// branch is needed.

	// --- aggField cross-entity check (skipped for count, which needs none). ---
	if metric != "count" && aggField != "" {
		cf, ok := childByName[aggField]
		if !ok {
			return invalidField("FIELD_ROLLUP_AGG_FIELD_UNKNOWN", f.Name,
				"field rollup %q có options.aggField %q không tồn tại trên entity con %q", f.Name, aggField, child)
		}
		if _, ok := rollupAggFieldAllowedTypes[cf.FieldType]; !ok {
			switch cf.FieldType {
			case "formula":
				// Rollup-over-formula: allowed when the formula's resultType is numeric. "number"
				// (2026-07-12) persists a plain float64 into obj_*.data; "currency" (#262,
				// 2026-09-30) persists {amount,currencyCode} exactly like a currency field, which
				// the rollup reads on its ".amount" sub-path (rollupAggCurrency). "text" stores a
				// string — SUM over it would CAST garbage to 0 — so it stays rejected.
				cfOpts, perr := parseOptionsJSON(cf.Options)
				if perr != nil {
					return fmt.Errorf("services.validateRollupCrossEntity: parse agg-field formula options: %w", perr)
				}
				resultType, _ := cfOpts["resultType"].(string)
				if resultType == "" {
					resultType = "number" // default per validateFormulaConfig/applyComputedFields
				}
				if resultType != "number" && resultType != "currency" {
					return invalidField("FIELD_ROLLUP_AGG_FIELD_FORMULA_NOT_NUMBER", f.Name,
						"field rollup %q có options.aggField %q là formula với resultType %q — chỉ formula kết quả số hoặc tiền tệ mới rollup được (công thức chữ lưu chuỗi, SUM sẽ đọc rác)",
						f.Name, aggField, resultType)
				}
				// queue #284: a formula over a lookup/rollup is computed at READ and never stored,
				// so SUM would read an empty column.
				expr, _ := cfOpts["formula"].(string)
				if len(formula.Derived(expr, formulaFieldInfos(childFields, nil))) > 0 {
					return invalidField("FIELD_ROLLUP_AGG_FIELD_FORMULA_DERIVED", f.Name,
						"field rollup %q có options.aggField %q là công thức đọc một trường tra cứu hoặc tổng hợp — giá trị đó chỉ tính khi xem, không cộng gộp được (hãy cộng từng trường gốc, hoặc tính lại ở nơi hiển thị)",
						f.Name, aggField)
				}
				// number / currency resultType: fall through, allowed.
			case "rollup", "lookup":
				return invalidField("FIELD_FORMULA_NESTED", f.Name,
					"field rollup %q có options.aggField %q trỏ tới field computed khác (%q) trên entity con — không hỗ trợ rollup lồng nhau", f.Name, aggField, cf.FieldType)
			default:
				return invalidField("FIELD_ROLLUP_AGG_FIELD_TYPE_INVALID", f.Name,
					"field rollup %q có options.aggField %q kiểu %q không hợp lệ (cần number/currency/rating)", f.Name, aggField, cf.FieldType)
			}
		}
	}

	stampStoredRollupShape(f, childByName)
	return nil
}

// stampStoredRollupShape records, on a stored roll-up's own options, which plain shape its value is
// kept in (options.valueType "number"|"currency" and, for money, options.currencyCode). The child's
// aggField is only visible here, but filters and reports later see the roll-up's FieldSpec alone —
// so the answer travels with the field. The caller's options map is mutated on purpose: it is the
// one persisted right after this check.
func stampStoredRollupShape(f FieldInput, childByName map[string]metadata.FieldDefinition) {
	if !boolOpt(f.Options, "stored") {
		return
	}
	valueType, code := "number", ""
	if metric, _ := f.Options["metric"].(string); metric != "count" {
		agg, _ := f.Options["aggField"].(string)
		if cf, ok := childByName[strings.TrimSpace(agg)]; ok {
			if specs, err := toFieldSpecs([]metadata.FieldDefinition{cf}); err == nil && len(specs) == 1 {
				if isCur, c := rollupAggCurrency(&specs[0]); isCur {
					valueType, code = "currency", c
				}
			}
		}
	}
	f.Options["valueType"] = valueType
	if code != "" {
		f.Options["currencyCode"] = code
	} else {
		delete(f.Options, "currencyCode")
	}
}

// storedRollupIdentityKeys are the options that decide WHAT a stored roll-up totals; changing one
// would leave every stored number describing the old definition.
var storedRollupIdentityKeys = []string{"stored", "childEntity", "viaRelation", "metric", "aggField"}

// validateStoredRollupImmutable refuses (422 FIELD_ROLLUP_STORED_IMMUTABLE) an edit that changes
// what a stored roll-up totals, or turns storing on/off. The stored values of the existing rows
// would silently describe the old definition; a new field (and "Tính lại") is the honest path.
// Options are a full replacement on edit, so the derived shape keys the validator stamped
// (valueType/currencyCode) are carried over from storedOpts into the returned copy when the roll-up
// is stored — an options payload that omits them must not erase them.
func validateStoredRollupImmutable(name string, storedOpts, newOpts map[string]any) (map[string]any, error) {
	if !boolOpt(storedOpts, "stored") && !boolOpt(newOpts, "stored") {
		return newOpts, nil
	}
	for _, k := range storedRollupIdentityKeys {
		if fmt.Sprint(storedOpts[k]) != fmt.Sprint(newOpts[k]) {
			return nil, invalidField("FIELD_ROLLUP_STORED_IMMUTABLE", name,
				"không đổi được cách tính của trường tổng hợp %q khi đang lưu giá trị (đổi %q) — các số đã lưu sẽ lệch; hãy tạo trường mới", name, k)
		}
	}
	out := make(map[string]any, len(newOpts)+2)
	for k, v := range newOpts {
		out[k] = v
	}
	for _, k := range []string{"valueType", "currencyCode"} {
		if v, ok := storedOpts[k]; ok {
			out[k] = v
		} else {
			delete(out, k)
		}
	}
	return out, nil
}

// RollupAggFinder lists the rollup fields ("entity.field") of a tenant that aggregate
// <childEntity>.<aggField>. Satisfied by *metadata.Repository; an edit path whose repo does not
// implement it (unit fakes) skips the guard, like the other optional seams.
type RollupAggFinder interface {
	ListRollupsAggregating(ctx context.Context, tenantID, childEntity, aggField string) ([]string, error)
}

// FormulaResultType returns a formula's effective resultType ("number" when omitted — same default
// as applyComputedFields/validateFormulaConfig).
func FormulaResultType(opts map[string]any) string {
	if rt, _ := opts["resultType"].(string); rt != "" {
		return rt
	}
	return "number"
}

// FormulaResultTypeChanged reports whether an edit moves a formula's resultType. Only an edit that
// supplies options can; options are a full replacement, so an omitted resultType means "number".
func FormulaResultTypeChanged(stored metadata.FieldDefinition, storedOpts, newOpts map[string]any) bool {
	return stored.FieldType == "formula" && newOpts != nil && FormulaResultType(newOpts) != FormulaResultType(storedOpts)
}

// GuardFormulaResultTypeInUse refuses (422 FIELD_ROLLUP_AGG_FIELD_RESULT_TYPE_IN_USE) changing the
// resultType of a formula that a rollup uses as its aggField — in ANY direction (queue #234).
// validateRollupCrossEntity judged resultType once, at rollup creation; the stored rows keep the
// OLD shape (plain number vs {amount,currencyCode} vs string) and SUM/AVG reads the metadata's
// CURRENT shape, so after the change the aggregate silently dropped rows (NULL) or read 0.
// It is the one body EditField (PUT /admin/fields/:id) and the AI propose_edit_field both run.
func GuardFormulaResultTypeInUse(ctx context.Context, finder RollupAggFinder, tenantID, entityName string, stored metadata.FieldDefinition, storedOpts, newOpts map[string]any) error {
	if finder == nil || !FormulaResultTypeChanged(stored, storedOpts, newOpts) {
		return nil
	}
	users, err := finder.ListRollupsAggregating(ctx, tenantID, entityName, stored.Name)
	if err != nil {
		return fmt.Errorf("services.GuardFormulaResultTypeInUse: %w", err)
	}
	if len(users) == 0 {
		return nil
	}
	label := stored.Label
	if label == "" {
		label = stored.Name
	}
	from, to := FormulaResultType(storedOpts), FormulaResultType(newOpts)
	return &ValidationError{
		Code: "FIELD_ROLLUP_AGG_FIELD_RESULT_TYPE_IN_USE",
		Msg: fmt.Sprintf("Không đổi được kiểu kết quả của công thức %q từ %s sang %s: trường tổng hợp %s đang cộng/tính trung bình theo nó, các bản ghi đã lưu giữ dạng cũ nên tổng sẽ sai âm thầm. Gỡ hoặc sửa trường tổng hợp đó trước, hoặc tạo một công thức mới có kiểu kết quả mong muốn.",
			label, from, to, strings.Join(users, ", ")),
		Field:   stored.Name,
		Details: map[string]any{"field": stored.Name, "from": from, "to": to, "rollups": users},
	}
}
