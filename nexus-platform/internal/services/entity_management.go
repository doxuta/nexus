package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// UpdateEntityInput carries the mutable fields for PUT /admin/entities/:name.
// Name and table_name are not here — they are immutable (schema-bound).
type UpdateEntityInput struct {
	Label       *string        // nil = leave unchanged
	Description *string        // nil = leave unchanged
	Options     map[string]any // nil = leave unchanged
}

// UpdateEntity applies mutable changes (label/description/options) to a tenant-scoped entity.
// It resolves the entity by tenant+name first (IDOR guard) then writes only the changed fields.
// Returns the updated entity definition on success.
func (s *EntityService) UpdateEntity(ctx context.Context, tenantID, entityName string, in UpdateEntityInput) (*metadata.EntityDefinition, error) {
	entity, err := s.meta.GetEntityByName(ctx, tenantID, entityName)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại", entityName)
		}
		return nil, fmt.Errorf("services.UpdateEntity: get entity: %w", err)
	}
	// Snapshot the before-image for the RM-19 audit diff: entity is mutated in place below.
	beforeEntity := *entity

	// Apply only the supplied (non-nil) fields.
	if in.Label != nil {
		entity.Label = *in.Label
	}
	if in.Description != nil {
		entity.Description = *in.Description
	}
	if in.Options != nil {
		// #272: a table-name choice must name a real text/email/auto_number field of THIS entity,
		// else every screen would silently fall back and the builder would never learn why.
		if lf, has := in.Options["labelField"]; has {
			fields, ferr := s.meta.ListFields(ctx, tenantID, entity.ID)
			if ferr != nil {
				return nil, fmt.Errorf("services.UpdateEntity: list fields: %w", ferr)
			}
			if verr := metadata.ValidateLabelFieldOption(lf, fields); verr != nil {
				return nil, invalid("ENTITY_LABEL_FIELD_INVALID", "%s", verr.Error())
			}
			// Store the value the validator checked (trimmed), else " ho_ten" passes here yet never
			// matches a field at read time and every screen silently falls back.
			if str, isStr := lf.(string); isStr {
				in.Options["labelField"] = strings.TrimSpace(str)
			}
		}
		raw, err := json.Marshal(in.Options)
		if err != nil {
			return nil, fmt.Errorf("services.UpdateEntity: marshal options: %w", err)
		}
		s := string(raw)
		entity.Options = &s
	}

	// Validate: label must remain non-empty after any update.
	if entity.Label == "" {
		return nil, invalid("ENTITY_LABEL_REQUIRED", "label entity là bắt buộc")
	}

	// The repo method is wired via the MetadataWriter interface. We need the concrete repo
	// here; use a type-assert on the meta field (which is *metadata.Repository in production)
	// or, more cleanly, extend the MetadataWriter interface. We extend it below.
	updater, ok := s.meta.(entityUpdater)
	if !ok {
		return nil, fmt.Errorf("services.UpdateEntity: metadata writer does not support UpdateEntityMutable")
	}
	if err := updater.UpdateEntityMutable(ctx, tenantID, entity); err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại", entityName)
		}
		return nil, fmt.Errorf("services.UpdateEntity: %w", err)
	}
	// Invalidate the cached entity definition AFTER the commit so RecordService's cached resolve
	// picks up the new label/description/options (O3).
	s.cache.InvalidateEntityDef(tenantID, entityName)
	// RM-19: audit the committed entity update (diff = changed metadata fields). Fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.EntityMeta, audit.ActionUpdate, beforeEntity, *entity)
	return entity, nil
}

// SoftDeleteEntity soft-deletes a tenant-scoped entity in two steps:
//  1. Tombstone the sys_entities row: rename `name` to free the unique (tenant_id, name) index
//     and set deleted_at via the metadata repo (GORM transaction).
//  2. Soft-delete the tenant's records in the shared obj_* table: UPDATE ... SET deleted_at
//     WHERE tenant_id = ? AND deleted_at IS NULL.
//
// Data-bleed design note: obj_<name> tables are shared by entity name across ALL tenants
// (architecture §8). Renaming the physical table is therefore NOT safe — it would sever
// every other tenant from their live records. Instead, DML-only: mark THIS tenant's records
// as deleted so a future recreate-same-name sees 0 rows (GET filters deleted_at IS NULL).
// The old records survive in the shared table for potential restore (soft-delete principle).
// sys_fields rows are left active (orphaned) — reversible; RM-21 cleanup deferred.
// ENG-REL2 #4 + REL-RM21-NN: EVERY sys_relationships row referencing this entity on EITHER
// side (parent or child, any type — 1-1, 1-N, N-N) IS tombstoned + soft-deleted inside the
// repo transaction (step 1) — otherwise the (tenant_id, name) / (tenant_id, join_table_name)
// unique slots would brick a recreate-same-name (vertical-pack retry), and the stale rows
// would keep surfacing as ghost edges in the Studio ERD / Related tab (phantom edges).
// STILL deferred (by design): when this entity was the PARENT of a 1-N/1-1 row, the owning
// relation FIELD on the OTHER entity's sys_fields is NOT deleted (sys_fields orphan cleanup)
// — writing a value into it now fails record validation (target not found); an admin can
// delete the field.
func (s *EntityService) SoftDeleteEntity(ctx context.Context, tenantID, entityName string) error {
	entity, err := s.meta.GetEntityByName(ctx, tenantID, entityName)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại", entityName)
		}
		return fmt.Errorf("services.SoftDeleteEntity: get entity: %w", err)
	}

	deleter, ok := s.meta.(entityDeleter)
	if !ok {
		return fmt.Errorf("services.SoftDeleteEntity: metadata writer does not support SoftDeleteEntity")
	}

	// specs/063 S7-9: on a delivered workspace the builder's delete is a "sẽ xoá khi xuất bản" draft; the
	// customer keeps using the table until Publish calls this again with WithPublishApply.
	if s.draftFields != nil {
		drafted, derr := s.draftFields.SaveEntityDelete(ctx, tenantID, entity)
		if derr != nil {
			return fmt.Errorf("services.SoftDeleteEntity: draft delete: %w", derr)
		}
		if drafted {
			return nil
		}
	}

	// Step 1: sweep this tenant's records in the SHARED physical table so a recreated entity does
	// not inherit the old data (data-bleed fix — RM-03 blocker). The table name derives from the
	// entity name BEFORE the tombstone-rename, which is another reason it must run first.
	//
	// It runs BEFORE the metadata tombstone, and that ordering is the fix. The other way round, a
	// failed sweep was UNRECOVERABLE: the tombstone had already renamed the row to
	// "<name>__deleted_<id8>", so a retry resolves nothing (422 ENTITY_NOT_FOUND) and there is no
	// restore path anywhere in the product — the tenant was left with live rows owned by an entity
	// that no longer exists, which a recreated same-name entity then adopts wholesale (obj_* tables
	// carry no entity_id; rows are keyed by tenant only). The sweep is idempotent
	// (WHERE deleted_at IS NULL), so failing here now leaves the entity live and the whole call
	// simply retryable. dynrepo classifies 1213/1205 on this UPDATE as a write conflict, i.e. the
	// authors already expected it to fail transiently.
	//
	// S7-9: a publish that deletes this table picked ONE instant, recorded it in the version BEFORE
	// calling here and carries it on ctx (and a retry re-reads the same one), so every row of one
	// deletion shares a deleted_at that a rollback can find. Outside a publish: now.
	at, ok := metaversion.SweepAtFrom(ctx)
	if !ok {
		at = time.Now().UTC()
	}
	swept, err := s.schema.SoftDeleteRecordsAt(ctx, entityName, tenantID, at)
	if err != nil {
		return fmt.Errorf("services.SoftDeleteEntity: soft-delete records: %w", err)
	}
	metaversion.AddSweepCount(ctx, swept)

	// The entity's fields vanish with it: drop their pending drafts (idempotent, so retryable).
	if s.draftFields != nil {
		if err := s.draftFields.DropEntityDrafts(ctx, tenantID, entity.ID); err != nil {
			return fmt.Errorf("services.SoftDeleteEntity: drop field drafts: %w", err)
		}
	}

	// Step 2: tombstone the metadata. Landing this only after the rows are gone means there is no
	// window in which the entity is unresolvable but its data is still live.
	if err := deleter.SoftDeleteEntity(ctx, tenantID, entity.ID); err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại", entityName)
		}
		return fmt.Errorf("services.SoftDeleteEntity: %w", err)
	}
	// Invalidate AFTER both commits so the cached resolve immediately sees the entity gone
	// (GetEntityByName → not-found) and drops its field list (O3).
	s.cache.InvalidateEntityDef(tenantID, entityName)
	s.cache.InvalidateFields(tenantID, entity.ID)
	// The same commit dropped every role's grants on the table: cached grants must not keep authorizing them until
	// the 30 s backstop. ponytail: this drops every tenant's cached grants (a table delete is a rare builder act);
	// a tenant-scoped TTLCache invalidation is the upgrade if it ever shows in a profile.
	s.aclCache.InvalidateAll()
	// RM-19: audit the committed entity delete (before-only diff). Fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.EntityMeta, audit.ActionDelete, *entity, nil)
	return nil
}

// entityUpdater is a capability interface satisfied by *metadata.Repository.
// Declaring it here keeps EntityService's MetadataWriter interface minimal while
// allowing the service to call the concrete update method in production.
type entityUpdater interface {
	UpdateEntityMutable(ctx context.Context, tenantID string, e *metadata.EntityDefinition) error
}

// entityDeleter is a capability interface satisfied by *metadata.Repository.
type entityDeleter interface {
	SoftDeleteEntity(ctx context.Context, tenantID, entityID string) error
}

// (SoftDeleteRecords is now part of the SchemaRepo interface — see entity_orchestration.go —
// so the entity-delete data-bleed sweep is compile-time guaranteed, not a runtime assert.)
