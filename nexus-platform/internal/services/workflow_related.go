package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// specs/058 — field_update following a to-one relation: "hoá đơn Đã thanh toán ⇒ lịch hẹn đổi trạng
// thái". The engine side (workflow.runRelatedFieldUpdate) reads the related id off the triggering
// record; everything that needs metadata lives here, and the runtime and the save-time guard resolve
// the relation through the SAME resolveRelatedTarget, so a rule cannot pass the save and then aim
// somewhere else when it fires.

// relatedViaInvalidCode is the 422 code of a via that does not lead to exactly one related record.
const relatedViaInvalidCode = "WORKFLOW_RELATED_VIA_INVALID"

// resolveRelatedTarget returns the definition of the entity the to-one relation field `via` (one of
// specs — the rule entity's fields) points at. Refused with WORKFLOW_RELATED_VIA_INVALID, details.field
// = via: an unknown field, a field that is not a `relation` (polymorphic_relation included — its
// target is per-record, not per-field), a many-valued relation, a relation with no target, and a
// target entity that no longer exists in the tenant. Only a catalog failure is a plain error.
func resolveRelatedTarget(ctx context.Context, catalog EntityCatalog, tenantID string, specs []FieldSpec, via string) (*metadata.EntityDefinition, error) {
	var spec *FieldSpec
	for i := range specs {
		if specs[i].Name == via {
			spec = &specs[i]
			break
		}
	}
	switch {
	case spec == nil:
		return nil, invalidField(relatedViaInvalidCode, via, "trường %q không có trên entity của luật", via)
	case spec.FieldType != "relation":
		return nil, invalidField(relatedViaInvalidCode, via,
			"trường %q không phải trường quan hệ (kiểu %s) — không đi tới bản ghi liên quan được", via, spec.FieldType)
	}
	if card := relationCardinality(spec.Options); card != "1-N" && card != "1-1" {
		return nil, invalidField(relatedViaInvalidCode, via,
			"trường quan hệ %q trỏ tới nhiều bản ghi (%s) — chỉ sửa được bản ghi liên quan qua quan hệ trỏ tới một bản ghi", via, card)
	}
	target, ok := relatedEntityName(spec.Options)
	if !ok {
		return nil, invalidField(relatedViaInvalidCode, via, "trường quan hệ %q không khai entity đích", via)
	}
	def, err := catalog.GetEntityByName(ctx, tenantID, target)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, invalidField(relatedViaInvalidCode, via,
				"entity đích %q của trường quan hệ %q không còn trong tenant", target, via)
		}
		return nil, fmt.Errorf("services.resolveRelatedTarget: %w", err)
	}
	return def, nil
}

// entitySpecs lists an entity's fields as FieldSpecs through the (TTL-cached) catalog.
func entitySpecs(ctx context.Context, catalog EntityCatalog, tenantID string, def *metadata.EntityDefinition) ([]FieldSpec, error) {
	fields, err := catalog.ListFields(ctx, tenantID, def.ID)
	if err != nil {
		return nil, fmt.Errorf("services.entitySpecs: %w", err)
	}
	specs, err := toFieldSpecs(fields)
	if err != nil {
		return nil, fmt.Errorf("services.entitySpecs: %w", err)
	}
	return specs, nil
}

// RelatedEntity implements workflow.RecordMutator for a field_update that follows a relation: the
// entity `via` (a field of entity) points at, read from metadata at fire time. Without a catalog it
// refuses rather than guess — the write would otherwise land on an entity nobody resolved.
func (m *WorkflowRecordMutator) RelatedEntity(ctx context.Context, tenantID, entity, via string) (string, error) {
	if m == nil || m.catalog == nil {
		return "", fmt.Errorf("services.WorkflowRecordMutator.RelatedEntity: no metadata catalog wired — cannot resolve relation %q", via)
	}
	def, err := m.catalog.GetEntityByName(ctx, tenantID, entity)
	if err != nil {
		return "", fmt.Errorf("services.WorkflowRecordMutator.RelatedEntity: %w", err)
	}
	specs, err := entitySpecs(ctx, m.catalog, tenantID, def)
	if err != nil {
		return "", fmt.Errorf("services.WorkflowRecordMutator.RelatedEntity: %w", err)
	}
	target, err := resolveRelatedTarget(ctx, m.catalog, tenantID, specs, via)
	if err != nil {
		// The code goes into the TEXT too: the run-history row records err.Error(), and "via x
		// rejected" without the code does not tell an admin which of the refusals it was.
		var ve *ValidationError
		if errors.As(err, &ve) {
			return "", fmt.Errorf("services.WorkflowRecordMutator.RelatedEntity: via %q rejected (%s): %w", via, ve.Code, err)
		}
		return "", fmt.Errorf("services.WorkflowRecordMutator.RelatedEntity: %w", err)
	}
	return target.Name, nil
}

// checkRelatedTrigger refuses a related field_update on a before_save/before_delete rule: those run
// inside a save that can still be vetoed, and may only reshape the record being saved (specs/048
// FR-004/FR-005). It runs BEFORE checkBeforeRule so the reason given is this one, not "not yet".
func checkRelatedTrigger(t workflow.Trigger, actions []WorkflowActionInput) error {
	if !workflow.IsBeforeTrigger(t) {
		return nil
	}
	for _, a := range actions {
		if strings.TrimSpace(a.Type) == workflow.ActionFieldUpdate && (strings.TrimSpace(a.Via) != "" || strings.TrimSpace(a.ChildEntity) != "") {
			return actionNotAllowed(workflow.ActionFieldUpdate, t,
				"quy tắc chạy lúc lưu chỉ sửa được chính bản ghi đang lưu, không sửa bản ghi liên quan")
		}
	}
	return nil
}

// checkRelatedFieldTargets is the DB-dependent half of a related field_update's save-time check
// (apply path, next to checkCreateRecordTargets): the via must resolve to a live target entity, and
// the field + value must be valid for THAT entity — known, not computed/assigned (the same
// computedReadonlyCode family as every other write path), and a value ValidatePartialRecord accepts.
// The pure half (via is a relation of the rule's entity) ran in buildActionsJSON. No-op when the
// checker cannot list fields (partial unit fakes) or the rule's entity was not resolved.
func (s *WorkflowService) checkRelatedFieldTargets(ctx context.Context, tenantID string, def *metadata.EntityDefinition, actions []WorkflowActionInput) error {
	catalog, ok := s.entites.(EntityCatalog)
	if !ok || def == nil {
		return nil
	}
	var ruleSpecs []FieldSpec
	for _, a := range actions {
		via := strings.TrimSpace(a.Via)
		childEntity := strings.TrimSpace(a.ChildEntity)
		if strings.TrimSpace(a.Type) != workflow.ActionFieldUpdate || (via == "" && childEntity == "") {
			continue
		}
		if ruleSpecs == nil {
			specs, err := entitySpecs(ctx, catalog, tenantID, def)
			if err != nil {
				return fmt.Errorf("services.WorkflowService.checkRelatedFieldTargets: %w", err)
			}
			ruleSpecs = specs
		}
		var target *metadata.EntityDefinition
		var targetSpecs []FieldSpec
		var err error
		if childEntity != "" {
			// specs/084: the field belongs to the CHILD entity; the relation lives on the child.
			via = strings.TrimSpace(a.ChildVia)
			target, targetSpecs, err = resolveChildRelation(ctx, catalog, tenantID, def.Name, childEntity, via)
			if err != nil {
				return err
			}
		} else {
			target, err = resolveRelatedTarget(ctx, catalog, tenantID, ruleSpecs, via)
			if err != nil {
				return err
			}
			targetSpecs, err = entitySpecs(ctx, catalog, tenantID, target)
			if err != nil {
				return fmt.Errorf("services.WorkflowService.checkRelatedFieldTargets: %w", err)
			}
		}
		field := strings.TrimSpace(a.Field)
		known := false
		for _, sp := range targetSpecs {
			if sp.Name == field {
				known = true
				break
			}
		}
		if !known {
			return invalidField("RECORD_UNKNOWN_FIELD", field,
				"field %q không tồn tại trong entity %q (bản ghi liên quan qua %q)", field, target.Name, via)
		}
		// A value resolved at fire time ($record.<field>, {{template}}) is checked against the RULE's
		// entity (the source) and the target's type instead of as a literal (queue #333).
		ruleTypes := make(map[string]string, len(ruleSpecs))
		for _, sp := range ruleSpecs {
			ruleTypes[sp.Name] = sp.FieldType
		}
		targetType := ""
		for _, sp := range targetSpecs {
			if sp.Name == field {
				targetType = sp.FieldType
			}
		}
		if dyn, err := checkDynamicFieldUpdateValue(a, ruleTypes, field, targetType); err != nil {
			return err
		} else if dyn {
			continue
		}
		// ValidatePartialRecord refuses a computed/assigned target with the computedReadonlyCode
		// family and a value of the wrong type/range/picklist with its RECORD_INVALID_* code.
		if _, err := ValidatePartialRecord(targetSpecs, map[string]any{field: a.Value}); err != nil {
			return err
		}
	}
	return nil
}

// previewRelatedFieldUpdate is the dry-run trace of a related field_update (specs/058 §2.7). It reads
// the related id the live run would follow but not the related record itself, so it names no
// "before" value; an unlinked record would not apply and says why in `note`.
func previewRelatedFieldUpdate(a workflow.Action, evt workflow.Event, condMatched bool, fieldTypes map[string]string) (map[string]any, bool) {
	value, resolved := workflow.ResolveFieldUpdateValue(a, evt) // the value the live run writes (#333)
	detail := map[string]any{"type": a.Type, "via": a.Via, "field": a.Field, "after": value}
	wouldRun := condMatched
	relatedID, _ := evt.Data[a.Via].(string)
	switch {
	case fieldTypes != nil && fieldTypes[a.Via] != "relation":
		detail["error"] = fmt.Sprintf("trường %q không còn là trường quan hệ của entity (%s)", a.Via, relatedViaInvalidCode)
		wouldRun = false
	case strings.TrimSpace(relatedID) == "":
		detail["note"] = fmt.Sprintf("bản ghi chưa liên kết %q — không có bản ghi liên quan để cập nhật", a.Via)
		wouldRun = false
	case !resolved:
		detail["after"] = nil
		detail["note"] = fmt.Sprintf("giá trị %v rỗng trên bản ghi kích hoạt — không cập nhật", a.Value)
		wouldRun = false
	default:
		detail["relatedId"] = strings.TrimSpace(relatedID)
	}
	detail["wouldApply"] = wouldRun
	return detail, wouldRun
}
