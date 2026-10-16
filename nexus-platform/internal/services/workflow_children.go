package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// specs/084 — field_update fanned out to every CHILD record. The engine side
// (workflow.runChildrenFieldUpdate) pages the ids and writes; everything that needs metadata lives
// here, and the runtime (ListChildIDs) and the save-time guard (checkRelatedFieldTargets) resolve the
// relation through the SAME resolveChildRelation, so a rule cannot pass the save and then aim
// somewhere else when it fires.

// childViaInvalidCode is the 422 code of a childEntity/childVia pair that is not a to-one relation
// of the child entity pointing back at the rule's entity.
const childViaInvalidCode = "WORKFLOW_CHILD_VIA_INVALID"

// resolveChildRelation returns the child entity's definition and field specs after proving that
// childVia is a to-one `relation` (1-N / 1-1) field of childEntity whose relatedEntity is parentEntity.
// Refused with WORKFLOW_CHILD_VIA_INVALID, details.field = childVia. Only a catalog failure is a plain error.
func resolveChildRelation(ctx context.Context, catalog EntityCatalog, tenantID, parentEntity, childEntity, childVia string) (*metadata.EntityDefinition, []FieldSpec, error) {
	def, err := catalog.GetEntityByName(ctx, tenantID, childEntity)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, nil, invalidField(childViaInvalidCode, childVia, "entity con %q không có trong tenant", childEntity)
		}
		return nil, nil, fmt.Errorf("services.resolveChildRelation: %w", err)
	}
	specs, err := entitySpecs(ctx, catalog, tenantID, def)
	if err != nil {
		return nil, nil, fmt.Errorf("services.resolveChildRelation: %w", err)
	}
	for i := range specs {
		sp := specs[i]
		if sp.Name != childVia {
			continue
		}
		if sp.FieldType != "relation" {
			return nil, nil, invalidField(childViaInvalidCode, childVia, "trường %q của %q không phải trường quan hệ (kiểu %s)", childVia, childEntity, sp.FieldType)
		}
		if card := relationCardinality(sp.Options); card != "1-N" && card != "1-1" {
			return nil, nil, invalidField(childViaInvalidCode, childVia, "trường quan hệ %q của %q trỏ tới nhiều bản ghi (%s)", childVia, childEntity, card)
		}
		if target, _ := relatedEntityName(sp.Options); target != parentEntity {
			return nil, nil, invalidField(childViaInvalidCode, childVia, "trường %q của %q trỏ tới %q, không phải %q", childVia, childEntity, target, parentEntity)
		}
		return def, specs, nil
	}
	return nil, nil, invalidField(childViaInvalidCode, childVia, "trường %q không có trên entity con %q", childVia, childEntity)
}

// previewChildrenFieldUpdate is the dry-run trace of a children field_update (specs/084 §2.6). It
// reads no child (the rehearsal has no tenant-wide read), so it cannot count them; it names what the
// live run would write and the per-run budget.
func previewChildrenFieldUpdate(a workflow.Action, evt workflow.Event, condMatched bool) (map[string]any, bool) {
	value, resolved := workflow.ResolveFieldUpdateValue(a, evt)
	detail := map[string]any{
		"type": a.Type, "childEntity": a.ChildEntity, "childVia": a.ChildVia, "field": a.Field,
		"after": value, "budget": workflow.MaxChildUpdatesPerRun,
	}
	wouldRun := condMatched
	if !resolved {
		detail["after"] = nil
		detail["note"] = fmt.Sprintf("giá trị %v rỗng trên bản ghi kích hoạt — không cập nhật", a.Value)
		wouldRun = false
	}
	detail["wouldApply"] = wouldRun
	return detail, wouldRun
}

// relationLister is the minimal store slice ListChildIDs needs. Satisfied by *dynrepo.Repository.
type relationLister interface {
	ListIDsByRelation(ctx context.Context, entity, tenantID, field, parentID, afterID string, limit int) ([]string, error)
}

// ListChildIDs implements workflow.ChildLister: a keyset page of the live children of parentID in
// tenantID. The relation is re-resolved from metadata on every page (the TTL-cached catalog), so a
// childVia deleted or retargeted after the rule was saved FAILS the run instead of listing nothing.
func (m *WorkflowRecordMutator) ListChildIDs(ctx context.Context, tenantID, parentEntity, childEntity, childVia, parentID, afterID string, limit int) ([]string, error) {
	if m == nil || m.catalog == nil {
		return nil, fmt.Errorf("services.WorkflowRecordMutator.ListChildIDs: no metadata catalog wired — cannot resolve %s.%s", childEntity, childVia)
	}
	lister, ok := m.store.(relationLister)
	if !ok {
		return nil, fmt.Errorf("services.WorkflowRecordMutator.ListChildIDs: the record store cannot list children")
	}
	if _, _, err := resolveChildRelation(ctx, m.catalog, tenantID, parentEntity, childEntity, childVia); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			return nil, fmt.Errorf("services.WorkflowRecordMutator.ListChildIDs: %s.%s rejected (%s): %w", childEntity, childVia, ve.Code, err)
		}
		return nil, fmt.Errorf("services.WorkflowRecordMutator.ListChildIDs: %w", err)
	}
	ids, err := lister.ListIDsByRelation(ctx, childEntity, tenantID, strings.TrimSpace(childVia), parentID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("services.WorkflowRecordMutator.ListChildIDs: %w", err)
	}
	return ids, nil
}
