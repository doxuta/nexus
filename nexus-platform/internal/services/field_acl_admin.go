package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// W7 governance — FieldACLAdminService is the CONFIG surface for the A6 field-level security
// the FieldACLService enforces (field_acl.go): list / bulk-save / clear the sys_field_acl
// restriction rows per (role, entity, field). Until W7 those rows could only be INSERTed by
// hand — enforcement was live but invisible to admins (rescore E3's #1 governance gap).
//
// SEMANTICS (mirror of the enforcement's loadRestrictions, one mental model):
//   - default-allow: a field with NO row is fully visible + editable ("editable" tri-state).
//   - canView=false                  → "hidden": stripped from every read, un-writable.
//   - canView=true,  canEdit=false   → "read-only": visible, write payload rejected.
//   - canView=true,  canEdit=true    → "no restriction": the save DELETES the row instead of
//     storing it. A stored (true,true) row is a no-op the enforcement ignores; persisting it
//     would bloat the hot-path load and make the matrix lie ("exception exists" with zero
//     effect). GET therefore only ever returns rows that DO something.
//   - canView=false, canEdit=true    → contradictory (you cannot edit what you cannot see) →
//     422 FIELD_ACL_CONTRADICTORY. Hidden rows are normalized to canEdit=false on write.
//
// PROPAGATION: the enforcement loads grants from MySQL per request (no cache — see
// auth.LoadFieldACLForEntity), so a save here is live on the next record request with no
// invalidation step. If a cache ever fronts that loader, its invalidation MUST hook the two
// mutating methods here.
type FieldACLAdminService struct {
	repo    fieldACLAdminRepo
	roles   fieldACLRoleGetter
	catalog fieldACLCatalog
	auditor audit.Auditor
	rec     *VersionRecorder // S7-5: direct version entry per role touched; nil = none
}

// WithRecorder arms the direct version entries (specs/063 S7-5). Chainable.
func (s *FieldACLAdminService) WithRecorder(r *VersionRecorder) *FieldACLAdminService {
	s.rec = r
	return s
}

// snapshotRoles reads, per role, ALL its field-permission rows (the before/after image of an entry).
func (s *FieldACLAdminService) snapshotRoles(ctx context.Context, tenantID string, roleIDs []string) (map[string][]auth.FieldACL, error) {
	out := make(map[string][]auth.FieldACL, len(roleIDs))
	for _, id := range roleIDs {
		rows, err := s.repo.ListFieldACL(ctx, tenantID, id, "")
		if err != nil {
			return nil, fmt.Errorf("services.FieldACLAdminService.snapshotRoles: %w", err)
		}
		if rows == nil {
			rows = []auth.FieldACL{}
		}
		out[id] = rows
	}
	return out, nil
}

// recordRoles writes one direct entry (one doc per role whose rows changed) after a committed save.
func (s *FieldACLAdminService) recordRoles(ctx context.Context, tenantID string, before map[string][]auth.FieldACL, roleIDs []string) {
	after, err := s.snapshotRoles(ctx, tenantID, roleIDs)
	if err != nil {
		metaversion.AddWarning(ctx, VersionWarningMessage)
		return
	}
	var docs []DirectDoc
	for _, id := range roleIDs {
		if equalFieldACL(before[id], after[id]) {
			continue
		}
		label := id
		if r, gerr := s.roles.GetRoleByID(ctx, tenantID, id); gerr == nil && r != nil {
			label = r.Label
			if label == "" {
				label = r.Name
			}
		}
		docs = append(docs, DirectDoc{DocType: "field_acl", DocKey: id, Label: label, Op: "update", Before: before[id], After: after[id]})
	}
	s.rec.RecordDirect(ctx, tenantID, "", docs...)
}

// RoleRows returns every restriction row of one role (tenant-pinned).
func (s *FieldACLAdminService) RoleRows(ctx context.Context, tenantID, roleID string) ([]auth.FieldACL, error) {
	rows, err := s.repo.ListFieldACL(ctx, tenantID, roleID, "")
	if err != nil {
		return nil, fmt.Errorf("services.FieldACLAdminService.RoleRows: %w", err)
	}
	return rows, nil
}

// RestoreRole sets a role's restriction rows to exactly rows (the undo of a saved matrix, specs/063
// S7-7b): rows absent from the target are deleted, the rest upserted, in ONE transaction, and the
// change leaves its own direct version entry. The role must exist in the tenant.
func (s *FieldACLAdminService) RestoreRole(ctx context.Context, tenantID, roleID string, rows []auth.FieldACL) error {
	if _, err := s.roles.GetRoleByID(ctx, tenantID, roleID); err != nil {
		if errors.Is(err, auth.ErrRoleNotFound) {
			return invalid("ROLE_NOT_FOUND", "không tìm thấy role")
		}
		return fmt.Errorf("services.FieldACLAdminService.RestoreRole: role: %w", err)
	}
	if err := s.checkRestorable(ctx, tenantID, rows); err != nil {
		return err
	}
	cur, err := s.RoleRows(ctx, tenantID, roleID)
	if err != nil {
		return err
	}
	want := map[auth.FieldACLKey]bool{}
	upserts := make([]auth.FieldACL, 0, len(rows))
	for _, r := range rows {
		want[auth.FieldACLKey{RoleID: roleID, EntityName: r.EntityName, FieldName: r.FieldName}] = true
		upserts = append(upserts, auth.FieldACL{RoleID: roleID, EntityName: r.EntityName, FieldName: r.FieldName, CanView: r.CanView, CanEdit: r.CanEdit})
	}
	var deletes []auth.FieldACLKey
	for _, c := range cur {
		if k := (auth.FieldACLKey{RoleID: roleID, EntityName: c.EntityName, FieldName: c.FieldName}); !want[k] {
			deletes = append(deletes, k)
		}
	}
	var before map[string][]auth.FieldACL
	traced := s.rec.Active(ctx, tenantID)
	if traced {
		before = map[string][]auth.FieldACL{roleID: cur}
	}
	if err := s.repo.ReplaceFieldACL(ctx, tenantID, upserts, deletes); err != nil {
		return fmt.Errorf("services.FieldACLAdminService.RestoreRole: %w", err)
	}
	if traced {
		s.recordRoles(ctx, tenantID, before, []string{roleID})
	}
	// Same audit row a save leaves: a revert can hide or reveal a field.
	s.auditor.Record(ctx, audit.Event{
		TenantID:    tenantID,
		ActorUserID: audit.ActorFromContext(ctx),
		EntityName:  audit.FieldACLMeta,
		Action:      audit.ActionUpdate,
		Diff:        &audit.Diff{After: map[string]any{"revert": true, "roleId": roleID, "applied": len(upserts), "cleared": len(deletes)}},
		At:          time.Now().UTC(),
	})
	return nil
}

// checkRestorable re-runs the PutGrants per-cell guards on a revert target: the entity and field must
// still exist, and a locked/hidden field must not have become required-without-default since (that
// role could not create any record). The revert is refused, not silently trimmed.
func (s *FieldACLAdminService) checkRestorable(ctx context.Context, tenantID string, rows []auth.FieldACL) error {
	fieldsByEntity := map[string]map[string]metadata.FieldDefinition{}
	for i, g := range rows {
		defs, ok := fieldsByEntity[g.EntityName]
		if !ok {
			ent, err := s.catalog.GetEntityByName(ctx, tenantID, g.EntityName)
			if err != nil {
				if errors.Is(err, metadata.ErrEntityNotFound) {
					return &ValidationError{Code: "FIELD_ACL_ENTITY_NOT_FOUND", Msg: fmt.Sprintf("entity %q không còn tồn tại: không hoàn tác được quyền theo trường", g.EntityName), Details: map[string]any{"index": i}}
				}
				return fmt.Errorf("services.FieldACLAdminService.RestoreRole: entity: %w", err)
			}
			fields, err := s.catalog.ListFields(ctx, tenantID, ent.ID)
			if err != nil {
				return fmt.Errorf("services.FieldACLAdminService.RestoreRole: fields: %w", err)
			}
			defs = make(map[string]metadata.FieldDefinition, len(fields))
			for _, f := range fields {
				defs[f.Name] = f
			}
			fieldsByEntity[g.EntityName] = defs
		}
		def, ok := defs[g.FieldName]
		if !ok {
			return &ValidationError{Code: "FIELD_ACL_FIELD_UNKNOWN", Msg: fmt.Sprintf("field %q không còn trên entity %q: không hoàn tác được quyền theo trường", g.FieldName, g.EntityName), Field: g.FieldName, Details: map[string]any{"index": i}}
		}
		if g.CanEdit {
			continue
		}
		deadEnd, err := requiredEngineCannotFill(def)
		if err != nil {
			return fmt.Errorf("services.FieldACLAdminService.RestoreRole: %w", err)
		}
		if deadEnd {
			return &ValidationError{Code: "FIELD_ACL_LOCKED_REQUIRED",
				Msg:     fmt.Sprintf("%s.%s hiện là field bắt buộc không có giá trị mặc định: hoàn tác sẽ khoá/ẩn nó và vai này không tạo được bản ghi nào. Cách gỡ: %s", g.EntityName, g.FieldName, lockedRequiredRemedy(def.FieldType)),
				Field:   g.FieldName,
				Details: map[string]any{"index": i, "field": g.FieldName, "defaultSupported": studioCanSetDefault(def.FieldType)}}
		}
	}
	return nil
}

func equalFieldACL(a, b []auth.FieldACL) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].EntityName != b[i].EntityName || a[i].FieldName != b[i].FieldName || a[i].CanView != b[i].CanView || a[i].CanEdit != b[i].CanEdit {
			return false
		}
	}
	return true
}

// fieldACLAdminRepo is the consumer-side slice of auth.FieldACLRepository the admin surface
// needs (list / transactional replace / filtered delete).
type fieldACLAdminRepo interface {
	ListFieldACL(ctx context.Context, tenantID, roleID, entity string) ([]auth.FieldACL, error)
	ReplaceFieldACL(ctx context.Context, tenantID string, upserts []auth.FieldACL, deletes []auth.FieldACLKey) error
	DeleteFieldACLWhere(ctx context.Context, tenantID, roleID, entity, field string) (int64, error)
}

// fieldACLRoleGetter validates that a grant's role exists in the SAME tenant (the real
// implementation is auth.RoleRepository.GetRoleByID — tenant-pinned, ErrRoleNotFound on miss).
type fieldACLRoleGetter interface {
	GetRoleByID(ctx context.Context, tenantID, id string) (*auth.Role, error)
}

// fieldACLCatalog validates that a grant's entity exists and its field is a real metadata
// field of that entity. Satisfied by both *metadata.Repository and the MetaCache decorator
// (production wires the cache — validation reads are the cold admin path either way).
type fieldACLCatalog interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}

// NewFieldACLAdminService wires the admin surface over the field-ACL repository, the role
// lookup and the metadata catalog. The auditor defaults to noop (WithAuditor injects the real
// dispatcher in main.go).
func NewFieldACLAdminService(repo fieldACLAdminRepo, roles fieldACLRoleGetter, catalog fieldACLCatalog) *FieldACLAdminService {
	return &FieldACLAdminService{repo: repo, roles: roles, catalog: catalog, auditor: audit.NewNoopAuditor()}
}

// WithAuditor injects the RM-19 audit dispatcher so permission changes land in the same
// append-only trail as record/role mutations (__field_acl__ meta rows). Nil is ignored.
func (s *FieldACLAdminService) WithAuditor(a audit.Auditor) *FieldACLAdminService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// FieldACLGrantInput is one row of the PUT /admin/field-acl body (the matrix editor's save).
type FieldACLGrantInput struct {
	RoleID     string `json:"roleId"`
	EntityName string `json:"entityName"`
	FieldName  string `json:"fieldName"`
	CanView    bool   `json:"canView"`
	CanEdit    bool   `json:"canEdit"`
}

// FieldACLGrantView is one restriction row as served by GET /admin/field-acl.
type FieldACLGrantView struct {
	RoleID     string `json:"roleId"`
	EntityName string `json:"entityName"`
	FieldName  string `json:"fieldName"`
	CanView    bool   `json:"canView"`
	CanEdit    bool   `json:"canEdit"`
}

// FieldACLPutResult reports what one save did: Applied = restriction rows upserted
// (hidden/read-only), Cleared = rows deleted because their new state is full access.
type FieldACLPutResult struct {
	Applied int `json:"applied"`
	Cleared int `json:"cleared"`
}

// ListGrants returns the tenant's restriction rows, optionally narrowed by role and/or entity.
// Provided filters are validated (an unknown roleId/entity is a client error, not "no
// restrictions" — a typo must not silently render as full access): unknown role → 422
// FIELD_ACL_ROLE_NOT_FOUND, unknown entity → 422 FIELD_ACL_ENTITY_NOT_FOUND.
func (s *FieldACLAdminService) ListGrants(ctx context.Context, tenantID, roleID, entity string) ([]FieldACLGrantView, error) {
	roleID = strings.TrimSpace(roleID)
	entity = strings.TrimSpace(entity)
	if roleID != "" {
		if _, err := s.roles.GetRoleByID(ctx, tenantID, roleID); err != nil {
			if errors.Is(err, auth.ErrRoleNotFound) {
				return nil, invalid("FIELD_ACL_ROLE_NOT_FOUND", "role %q không tồn tại trong tenant", roleID)
			}
			return nil, fmt.Errorf("services.FieldACLAdminService.ListGrants: role: %w", err)
		}
	}
	if entity != "" {
		if _, err := s.catalog.GetEntityByName(ctx, tenantID, entity); err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				return nil, invalid("FIELD_ACL_ENTITY_NOT_FOUND", "entity %q không tồn tại", entity)
			}
			return nil, fmt.Errorf("services.FieldACLAdminService.ListGrants: entity: %w", err)
		}
	}
	rows, err := s.repo.ListFieldACL(ctx, tenantID, roleID, entity)
	if err != nil {
		return nil, fmt.Errorf("services.FieldACLAdminService.ListGrants: %w", err)
	}
	out := make([]FieldACLGrantView, 0, len(rows))
	for _, r := range rows {
		out = append(out, FieldACLGrantView{
			RoleID: r.RoleID, EntityName: r.EntityName, FieldName: r.FieldName,
			CanView: r.CanView, CanEdit: r.CanEdit,
		})
	}
	return out, nil
}

// DeadEndFields returns the names of the entity's fields a lock would turn into "this role can
// create nothing" — required, client-written and not filled by the engine — decided by the SAME
// predicate PutGrants, EditField and Doctor C9 use (requiredEngineCannotFill), so the matrix reads
// the server's answer instead of mirroring it (queue #306). Admin-only surface: it names fields
// regardless of any role's visibility. Unknown entity → 422 FIELD_ACL_ENTITY_NOT_FOUND.
func (s *FieldACLAdminService) DeadEndFields(ctx context.Context, tenantID, entity string) ([]string, error) {
	entity = strings.TrimSpace(entity)
	ent, err := s.catalog.GetEntityByName(ctx, tenantID, entity)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, invalid("FIELD_ACL_ENTITY_NOT_FOUND", "entity %q không tồn tại", entity)
		}
		return nil, fmt.Errorf("services.FieldACLAdminService.DeadEndFields: entity: %w", err)
	}
	fields, err := s.catalog.ListFields(ctx, tenantID, ent.ID)
	if err != nil {
		return nil, fmt.Errorf("services.FieldACLAdminService.DeadEndFields: fields: %w", err)
	}
	out := []string{}
	for _, f := range fields {
		dead, err := requiredEngineCannotFill(f)
		if err != nil {
			return nil, fmt.Errorf("services.FieldACLAdminService.DeadEndFields: %w", err)
		}
		if dead {
			out = append(out, f.Name)
		}
	}
	return out, nil
}

// PutGrants applies one bulk save (all-or-nothing): every grant is validated FIRST (role
// exists tenant-scoped, entity exists, field exists on the entity, no contradiction, no
// duplicate key in the payload), then the whole set is written in ONE transaction. Full-access
// grants (canView && canEdit) DELETE their restriction row (see the type doc). Errors carry
// details.field / details.index so the matrix can mark the offending cell.
func (s *FieldACLAdminService) PutGrants(ctx context.Context, tenantID string, grants []FieldACLGrantInput) (*FieldACLPutResult, error) {
	if len(grants) == 0 {
		return nil, invalid("FIELD_ACL_EMPTY", "grants không được rỗng")
	}

	// Per-request lookup caches so an N-cell save costs one role lookup per distinct role and
	// one entity+fields lookup per distinct entity (not per cell).
	roleSeen := map[string]bool{}
	fieldsByEntity := map[string]map[string]metadata.FieldDefinition{}

	var upserts []auth.FieldACL
	var deletes []auth.FieldACLKey
	seen := map[auth.FieldACLKey]struct{}{}

	for i := range grants {
		g := &grants[i]
		g.RoleID = strings.TrimSpace(g.RoleID)
		g.EntityName = strings.TrimSpace(g.EntityName)
		g.FieldName = strings.TrimSpace(g.FieldName)
		if g.RoleID == "" || g.EntityName == "" || g.FieldName == "" {
			return nil, &ValidationError{
				Code: "FIELD_ACL_GRANT_INVALID", Msg: fmt.Sprintf("grant #%d thiếu roleId/entityName/fieldName", i),
				Details: map[string]any{"index": i},
			}
		}
		if !g.CanView && g.CanEdit {
			// Contradiction: cannot edit what you cannot see (the enforcement would treat the
			// row as hidden+locked anyway — reject so the stored config never lies).
			return nil, &ValidationError{
				Code: "FIELD_ACL_CONTRADICTORY", Msg: fmt.Sprintf("grant #%d (%s.%s): canEdit=true với canView=false là mâu thuẫn — không thể sửa field không được xem", i, g.EntityName, g.FieldName),
				Field: g.FieldName, Details: map[string]any{"index": i, "field": g.FieldName},
			}
		}

		key := auth.FieldACLKey{RoleID: g.RoleID, EntityName: g.EntityName, FieldName: g.FieldName}
		if _, dup := seen[key]; dup {
			return nil, &ValidationError{
				Code: "FIELD_ACL_DUPLICATE_GRANT", Msg: fmt.Sprintf("grant #%d lặp lại (roleId, entityName, fieldName) đã có trong payload", i),
				Field: g.FieldName, Details: map[string]any{"index": i, "field": g.FieldName},
			}
		}
		seen[key] = struct{}{}

		// Role must exist in THIS tenant (a cross-tenant role id misses the tenant-pinned read).
		if !roleSeen[g.RoleID] {
			if _, err := s.roles.GetRoleByID(ctx, tenantID, g.RoleID); err != nil {
				if errors.Is(err, auth.ErrRoleNotFound) {
					return nil, &ValidationError{
						Code: "FIELD_ACL_ROLE_NOT_FOUND", Msg: fmt.Sprintf("grant #%d: role %q không tồn tại trong tenant", i, g.RoleID),
						Details: map[string]any{"index": i},
					}
				}
				return nil, fmt.Errorf("services.FieldACLAdminService.PutGrants: role: %w", err)
			}
			roleSeen[g.RoleID] = true
		}

		// Entity must exist; field must be a real metadata field of it (computed fields are
		// legit targets — hiding a formula/rollup is enforced today; only UNKNOWN names reject).
		defs, ok := fieldsByEntity[g.EntityName]
		if !ok {
			ent, err := s.catalog.GetEntityByName(ctx, tenantID, g.EntityName)
			if err != nil {
				if errors.Is(err, metadata.ErrEntityNotFound) {
					return nil, &ValidationError{
						Code: "FIELD_ACL_ENTITY_NOT_FOUND", Msg: fmt.Sprintf("grant #%d: entity %q không tồn tại", i, g.EntityName),
						Details: map[string]any{"index": i},
					}
				}
				return nil, fmt.Errorf("services.FieldACLAdminService.PutGrants: entity: %w", err)
			}
			fields, err := s.catalog.ListFields(ctx, tenantID, ent.ID)
			if err != nil {
				return nil, fmt.Errorf("services.FieldACLAdminService.PutGrants: fields: %w", err)
			}
			defs = make(map[string]metadata.FieldDefinition, len(fields))
			for _, f := range fields {
				defs[f.Name] = f
			}
			fieldsByEntity[g.EntityName] = defs
		}
		def, ok := defs[g.FieldName]
		if !ok {
			return nil, &ValidationError{
				Code: "FIELD_ACL_FIELD_UNKNOWN", Msg: fmt.Sprintf("grant #%d: field %q không tồn tại trên entity %q", i, g.FieldName, g.EntityName),
				Field: g.FieldName, Details: map[string]any{"index": i, "field": g.FieldName},
			}
		}

		if g.CanView && g.CanEdit {
			// Full access = default state = no restriction row (see type doc).
			deletes = append(deletes, key)
			continue
		}

		// field-lock-signal (Q2, 2026-09-05): a restriction row is can_edit=0 for the enforcement
		// (hidden implies locked), and can_edit=0 applies to CREATE too — CheckWriteFields refuses
		// any locked key present, and a required field the form may not send fails
		// RECORD_REQUIRED_MISSING. So locking a REQUIRED field that has NO default hands that
		// role a create form nothing can submit. The bug is the configuration; refuse it at the
		// source, per cell, so the matrix editor can point at it. A default lets the engine fill
		// the field without the role ever writing it, so that shape stays allowed — and so is a
		// server-assigned type (formula/lookup/rollup/auto_number): no role ever writes those.
		//
		// queue #62: "has a default" is asked of defaultValue — the function Create runs — not of
		// the raw string (a stored default the engine drops is no default), and the way out named
		// in the refusal depends on the type: for currency/user/… the builder refuses any default,
		// so advising one sent the admin into a second 422 with no exit.
		deadEnd, err := requiredEngineCannotFill(def)
		if err != nil {
			return nil, fmt.Errorf("services.FieldACLAdminService.PutGrants: %w", err)
		}
		if deadEnd {
			return nil, &ValidationError{
				Code: "FIELD_ACL_LOCKED_REQUIRED",
				Msg: fmt.Sprintf("grant #%d (%s.%s): không thể khoá/ẩn field bắt buộc mà máy không tự điền được — vai này sẽ không tạo được bản ghi nào. Cách gỡ: %s",
					i, g.EntityName, g.FieldName, lockedRequiredRemedy(def.FieldType)),
				Field:   g.FieldName,
				Details: map[string]any{"index": i, "field": g.FieldName, "defaultSupported": studioCanSetDefault(def.FieldType)},
			}
		}
		upserts = append(upserts, auth.FieldACL{
			RoleID:     g.RoleID,
			EntityName: g.EntityName,
			FieldName:  g.FieldName,
			CanView:    g.CanView,
			// Normalize: hidden implies locked; never store (false, true).
			CanEdit: g.CanView && g.CanEdit,
		})
	}

	var traced []string
	var beforeImg map[string][]auth.FieldACL
	if s.rec.Active(ctx, tenantID) {
		set := map[string]bool{}
		for _, g := range grants {
			if !set[g.RoleID] {
				set[g.RoleID] = true
				traced = append(traced, g.RoleID)
			}
		}
		var serr error
		if beforeImg, serr = s.snapshotRoles(ctx, tenantID, traced); serr != nil {
			metaversion.AddWarning(ctx, VersionWarningMessage)
			traced = nil
		}
	}
	if err := s.repo.ReplaceFieldACL(ctx, tenantID, upserts, deletes); err != nil {
		return nil, fmt.Errorf("services.FieldACLAdminService.PutGrants: %w", err)
	}
	if len(traced) > 0 {
		s.recordRoles(ctx, tenantID, beforeImg, traced)
	}

	// One __field_acl__ audit row per save (after-only diff carrying the applied matrix cells).
	s.auditor.Record(ctx, audit.Event{
		TenantID:    tenantID,
		ActorUserID: audit.ActorFromContext(ctx),
		EntityName:  audit.FieldACLMeta,
		Action:      audit.ActionUpdate,
		Diff:        &audit.Diff{After: map[string]any{"grants": grants, "applied": len(upserts), "cleared": len(deletes)}},
		At:          time.Now().UTC(),
	})

	return &FieldACLPutResult{Applied: len(upserts), Cleared: len(deletes)}, nil
}

// DeleteGrants removes restriction rows by filter: the single (role, entity, field), all of a
// (role, entity), or every restriction of a role. roleId is required; field requires entity
// (deleting "field X on any entity" would be ambiguous). Removing zero rows is idempotent
// success — the state "no restriction" already holds. Returns the number of rows removed.
func (s *FieldACLAdminService) DeleteGrants(ctx context.Context, tenantID, roleID, entity, field string) (int64, error) {
	roleID = strings.TrimSpace(roleID)
	entity = strings.TrimSpace(entity)
	field = strings.TrimSpace(field)
	if roleID == "" {
		return 0, invalid("FIELD_ACL_ROLE_REQUIRED", "roleId là bắt buộc")
	}
	if field != "" && entity == "" {
		return 0, invalid("FIELD_ACL_FILTER_INVALID", "field yêu cầu entity đi kèm")
	}

	var beforeImg map[string][]auth.FieldACL
	traced := s.rec.Active(ctx, tenantID)
	if traced {
		var serr error
		if beforeImg, serr = s.snapshotRoles(ctx, tenantID, []string{roleID}); serr != nil {
			metaversion.AddWarning(ctx, VersionWarningMessage)
			traced = false
		}
	}
	n, err := s.repo.DeleteFieldACLWhere(ctx, tenantID, roleID, entity, field)
	if err != nil {
		return 0, fmt.Errorf("services.FieldACLAdminService.DeleteGrants: %w", err)
	}
	if traced && n > 0 {
		s.recordRoles(ctx, tenantID, beforeImg, []string{roleID})
	}
	if n > 0 {
		s.auditor.Record(ctx, audit.Event{
			TenantID:    tenantID,
			ActorUserID: audit.ActorFromContext(ctx),
			EntityName:  audit.FieldACLMeta,
			Action:      audit.ActionDelete,
			Diff:        &audit.Diff{Before: map[string]any{"roleId": roleID, "entityName": entity, "fieldName": field, "deleted": n}},
			At:          time.Now().UTC(),
		})
	}
	return n, nil
}
