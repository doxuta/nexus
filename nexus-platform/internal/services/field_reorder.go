package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/ddl"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// FieldReorderRepo renumbers an entity's fields in ONE transaction (W5 builder v2). It is a
// SEPARATE, narrow seam (not part of FieldMetadataRepo) so existing unit-test fakes stay
// untouched; the concrete *metadata.Repository implements it and NewFieldService auto-wires
// it via a type assertion.
type FieldReorderRepo interface {
	// ReorderFields sets field_order = (i+1)*10 for the i-th id, all-or-nothing. Every UPDATE
	// re-pins tenant_id AND entity_id; an id that matches 0 rows (forged / cross-tenant /
	// cross-entity / soft-deleted / concurrently removed) rolls the whole transaction back
	// with metadata.ErrFieldNotFound.
	ReorderFields(ctx context.Context, tenantID, entityID string, orderedIDs []string) error
}

// maxReorderIDs bounds the reorder payload — mirrors maxFieldsPerEntity (an entity can never
// legitimately hold more fields than that, so a longer list is malformed by construction).
const maxReorderIDs = maxFieldsPerEntity

// ReorderFieldsInput is the payload for PUT /admin/fields/reorder: the target entity (by
// name, tenant-scoped) and the field IDs in their new display order.
type ReorderFieldsInput struct {
	EntityName string
	Order      []string
}

// ReorderFields renumbers the display order of an entity's fields in one transaction (W5
// Data Model builder drag-reorder). Contract:
//
//   - The entity is resolved tenant-scoped — unknown/cross-tenant name → 404 ENTITY_NOT_FOUND.
//   - Order must be a non-empty list of unique field IDs, each an ACTIVE field of THIS entity
//     (422 FIELD_REORDER_INVALID for shape problems, 422 FIELD_REORDER_UNKNOWN_FIELD naming
//     the offending id — a cross-tenant/cross-entity id is indistinguishable from an unknown
//     one, so nothing leaks).
//   - The i-th id gets field_order = (i+1)*10 — same gap-of-10 numbering as entity-create.
//     Fields NOT in the list keep their stored order (the FE should send the complete list;
//     GET /metadata/fields tie-breaks equal orders by name, so a partial list still yields a
//     deterministic total order).
//   - All-or-nothing: any row that vanishes between the membership check and the UPDATE
//     (concurrent delete) rolls the whole transaction back.
//
// Returns the number of fields renumbered.
func (s *FieldService) ReorderFields(ctx context.Context, tenantID string, in ReorderFieldsInput) (int, error) {
	if s.reorder == nil {
		return 0, fmt.Errorf("services.ReorderFields: no reorder repository wired")
	}
	entityName, err := ddl.SanitizeIdentifier(in.EntityName)
	if err != nil {
		return 0, invalid("ENTITY_INVALID_NAME", "tên entity không hợp lệ: %v", err)
	}
	if len(in.Order) == 0 {
		return 0, invalid("FIELD_REORDER_INVALID", "order phải là danh sách id field không rỗng")
	}
	if len(in.Order) > maxReorderIDs {
		return 0, invalid("FIELD_REORDER_INVALID",
			"order khai %d id — tối đa %d field mỗi entity", len(in.Order), maxReorderIDs)
	}

	// Resolve the target entity tenant-scoped (also the tenant-isolation guard).
	entity, err := s.meta.GetEntityByName(ctx, tenantID, entityName)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return 0, invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại", entityName)
		}
		return 0, fmt.Errorf("services.ReorderFields: resolve entity: %w", err)
	}

	// Membership check: every id must be an ACTIVE field of THIS entity, no duplicates.
	active, err := s.meta.ListFields(ctx, tenantID, entity.ID)
	if err != nil {
		return 0, fmt.Errorf("services.ReorderFields: list fields: %w", err)
	}
	activeIDs := make(map[string]struct{}, len(active))
	for _, f := range active {
		activeIDs[f.ID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(in.Order))
	for _, id := range in.Order {
		id = strings.TrimSpace(id)
		if id == "" {
			return 0, invalid("FIELD_REORDER_INVALID", "order chứa id rỗng")
		}
		if _, dup := seen[id]; dup {
			return 0, invalid("FIELD_REORDER_INVALID", "order chứa id %q lặp lại", id)
		}
		seen[id] = struct{}{}
		if _, ok := activeIDs[id]; !ok {
			return 0, &ValidationError{
				Code:    "FIELD_REORDER_UNKNOWN_FIELD",
				Msg:     fmt.Sprintf("id %q không phải field đang hoạt động của entity %q", id, entityName),
				Details: map[string]any{"field": id},
			}
		}
	}

	// specs/063 S7-8 (FR-063): on a delivered workspace a builder's reorder is a set of drafts — the
	// live field_order stays, so the customer's default form keeps its order until Publish.
	if s.drafts != nil {
		mode, merr := s.drafts.gate.WriteMode(ctx, tenantID)
		if merr != nil {
			return 0, fmt.Errorf("services.ReorderFields: %w", merr)
		}
		if mode == metaversion.ModeDraft {
			if err := s.drafts.SaveReorder(ctx, tenantID, entity.ID, in.Order); err != nil {
				if errors.Is(err, metadata.ErrFieldNotFound) {
					return 0, invalid("FIELD_REORDER_UNKNOWN_FIELD", "một field trong order vừa bị xoá — tải lại danh sách field và thử lại")
				}
				return 0, fmt.Errorf("services.ReorderFields: draft: %w", err)
			}
			return len(in.Order), nil // no audit row: nothing live changed
		}
	}

	// Renumber in one transaction (all-or-nothing). A row that vanished since the membership
	// check (concurrent delete) rolls everything back → surface as the same 422.
	if err := s.reorder.ReorderFields(ctx, tenantID, entity.ID, in.Order); err != nil {
		if errors.Is(err, metadata.ErrFieldNotFound) {
			return 0, invalid("FIELD_REORDER_UNKNOWN_FIELD",
				"một field trong order vừa bị xoá — tải lại danh sách field và thử lại")
		}
		return 0, fmt.Errorf("services.ReorderFields: renumber: %w", err)
	}

	// Invalidate the entity's cached field list AFTER commit (O3) so metadata reads see the
	// new order immediately.
	s.cache.InvalidateFields(tenantID, entity.ID)
	// RM-19: one audit event for the whole reorder (after-only diff carries the new order).
	emitMetaAudit(ctx, s.auditor, tenantID, audit.FieldMeta, audit.ActionUpdate, nil, struct {
		EntityName string   `json:"entityName"`
		Order      []string `json:"order"`
	}{EntityName: entityName, Order: in.Order})
	return len(in.Order), nil
}
