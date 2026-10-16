package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// validRoleActions is the closed set of verbs a grant may authorize (spec ../specs/001-auth-security/spec.md
// §A4 line 90). A grant naming anything outside this set is rejected, so a role can never be
// minted with an action the ACL middleware does not understand (which would be dead config) or
// with a forged privilege verb.
var validRoleActions = map[string]struct{}{
	"view": {}, "create": {}, "edit": {}, "delete": {},
}

// validAccessLevels is the closed set of row-level scopes a grant may carry (spec §A4 line 91,
// consumed by A5). A4 does not interpret these but validates them so an A5 caller never finds a
// garbage value persisted.
var validAccessLevels = map[string]struct{}{
	"none": {}, "owner": {}, "team": {}, "all": {},
}

// userFieldChecker answers "is <field> a live user-type field of <entity>" for a grant at access_level
// "field:<name>" (#283). Declared consumer-side like roleRepo; CatalogUserFields is the production one.
type userFieldChecker interface {
	UserFieldExists(ctx context.Context, tenantID, entity, field string) (bool, error)
}

// CatalogUserFields adapts the entity catalog (sys_entities/sys_fields, read at runtime) to
// userFieldChecker.
type CatalogUserFields struct{ Catalog EntityCatalog }

// UserFieldExists reports whether entity has a (non-deleted) field named field of type "user".
// An unknown entity is (false, nil): the grant simply names nothing that exists.
func (c CatalogUserFields) UserFieldExists(ctx context.Context, tenantID, entity, field string) (bool, error) {
	e, err := c.Catalog.GetEntityByName(ctx, tenantID, entity)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("services.CatalogUserFields.UserFieldExists: entity: %w", err)
	}
	fields, err := c.Catalog.ListFields(ctx, tenantID, e.ID)
	if err != nil {
		return false, fmt.Errorf("services.CatalogUserFields.UserFieldExists: fields: %w", err)
	}
	for _, f := range fields {
		if f.Name == field && f.FieldType == "user" {
			return true, nil
		}
	}
	return false, nil
}

// errRoleNameReserved: the owner-tier role (auth.OwnerRoleName) cannot be created, renamed to/from or
// deleted through the role CRUD (specs/065 D-18) — every consumer recognises that role BY NAME.
var errRoleNameReserved = invalid("ROLE_NAME_RESERVED", "tên vai này được hệ thống giữ riêng cho chủ khách")

// defaultAccessLevel is applied when a grant omits access_level (matches the DB column default).
const defaultAccessLevel = "all"

// roleRepo is the subset of the role repository the role-management service needs. It is declared
// consumer-side so the service is unit-testable with an in-memory fake and does not import a
// concrete DB type. EVERY method is tenant-scoped in the real implementation (acl_roles.tenant_id),
// so no use case can read or mutate another tenant's role (R2).
type roleRepo interface {
	ListRoles(ctx context.Context, tenantID string) ([]auth.Role, error)
	GetRoleByID(ctx context.Context, tenantID, id string) (*auth.Role, error)
	CreateRole(ctx context.Context, role *auth.Role, actions []auth.RoleAction) error
	UpdateRole(ctx context.Context, tenantID, id string, fields map[string]any, actions []auth.RoleAction) (*auth.Role, error)
	DeleteRole(ctx context.Context, tenantID, id string) error
	ListActionsForRole(ctx context.Context, tenantID, roleID string) ([]auth.RoleAction, error)
}

// RoleService implements the A4 role-management use cases (CRUD over /admin/roles, spec §3 line
// 114). Every method takes the verified tenant id and pins all repository calls to it, so no use
// case can touch another tenant's role and a grant can never be attached to a role outside the
// caller's tenant (R3: no cross-tenant privilege escalation). It is HTTP-agnostic (the handler
// maps its typed errors to status codes) and holds no per-request state.
type RoleService struct {
	repo  roleRepo
	newID func() string // injectable id generator; defaults to uuid
	// aclCache, when set, is the shared ACL grants cache. Role writes invalidate the affected
	// role's entry AFTER commit so a changed/revoked grant takes effect on the next authorize.
	// Nil = no caching (Invalidate is nil-safe).
	aclCache *CachedACLLoader
	// auditor records committed role-metadata mutations (RM-19), async + fail-open.
	// Defaults to a noop so existing wiring/tests run unchanged; injected via WithAuditor.
	auditor audit.Auditor
	// userFields validates "field:<name>" grants (#283). Nil = such a grant is refused (fail closed).
	userFields userFieldChecker
	// rec writes the direct version entry of a delivered workspace (S7-5); foot reads what a role
	// delete takes with it. Both nil-safe: unset = no entry.
	rec  *VersionRecorder
	foot roleFootprint
	// recreator undoes a role delete in one transaction (S7-7b). Nil = Recreate is refused.
	recreator roleRecreator
}

// roleRecreator is the repository slice Recreate needs; *auth.RoleRepository in production.
type roleRecreator interface {
	RecreateRole(ctx context.Context, role *auth.Role, actions []auth.RoleAction, fieldACL []auth.FieldACL, userIDs []string) (int64, error)
}

// WithRecreator arms Recreate (the undo of a role delete, specs/063 S7-7b). Chainable.
func (s *RoleService) WithRecreator(r roleRecreator) *RoleService {
	s.recreator = r
	return s
}

// WithRecorder arms the direct version entries (specs/063 S7-5). foot reads, BEFORE a delete, the
// users holding the role and its field-permission rows (FKs detach / cascade them). Chainable.
func (s *RoleService) WithRecorder(r *VersionRecorder, foot roleFootprint) *RoleService {
	s.rec, s.foot = r, foot
	return s
}

// NewRoleService wires the role service over a role repository.
func NewRoleService(repo roleRepo) *RoleService {
	return &RoleService{repo: repo, newID: uuid.NewString, auditor: audit.NewNoopAuditor()}
}

// WithACLCache attaches the shared ACL grants cache so role writes invalidate a role's cached grants
// after commit (O3 — revocation must not lag). Chainable.
func (s *RoleService) WithACLCache(c *CachedACLLoader) *RoleService {
	s.aclCache = c
	return s
}

// WithUserFieldChecker arms the "field:<name>" access level: without it every such grant is refused.
// Chainable.
func (s *RoleService) WithUserFieldChecker(c userFieldChecker) *RoleService {
	s.userFields = c
	return s
}

// WithAuditor injects the RM-19 audit dispatcher so committed role mutations emit an
// append-only audit row (async, fail-open). The actor user id is read from the request
// context (audit.WithActor, set by the admin handler). Chainable; a nil auditor is ignored.
func (s *RoleService) WithAuditor(a audit.Auditor) *RoleService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// RoleActionInput is a single (entity, action) grant as received from the API. AccessLevel is
// optional (defaults to 'all').
type RoleActionInput struct {
	EntityName  string
	Action      string
	AccessLevel string
}

// RoleActionView is the safe public projection of a grant returned to the API.
type RoleActionView struct {
	EntityName  string `json:"entityName"`
	Action      string `json:"action"`
	AccessLevel string `json:"accessLevel"`
}

// RoleView is the safe public projection of a role plus its grants.
type RoleView struct {
	ID       string           `json:"id"`
	TenantID string           `json:"tenantId"`
	Name     string           `json:"name"`
	Label    string           `json:"label"`
	Actions  []RoleActionView `json:"actions"`
}

// CreateRoleInput is the payload to create a role: a name+label plus its initial grants.
type CreateRoleInput struct {
	Name    string
	Label   string
	Actions []RoleActionInput
}

// UpdateRoleInput carries the mutable role attributes. Name/Label use apply-when-non-nil
// semantics (an omitted key leaves the stored value untouched). Actions is REPLACE-semantics:
// a nil slice leaves grants untouched; a non-nil slice (even empty) replaces them entirely.
type UpdateRoleInput struct {
	Name    *string
	Label   *string
	Actions []RoleActionInput
}

// ListRoles returns the safe view of every role in the tenant (without grants, for a list page).
func (s *RoleService) ListRoles(ctx context.Context, tenantID string) ([]RoleView, error) {
	rows, err := s.repo.ListRoles(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.RoleService.ListRoles: %w", err)
	}
	views := make([]RoleView, len(rows))
	for i := range rows {
		views[i] = roleToView(&rows[i], nil)
	}
	return views, nil
}

// GetRole returns one tenant-scoped role WITH its grants, or a ROLE_NOT_FOUND ValidationError
// for an unknown/cross-tenant id.
func (s *RoleService) GetRole(ctx context.Context, tenantID, id string) (*RoleView, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("ROLE_ID_REQUIRED", "id role là bắt buộc")
	}
	role, err := s.repo.GetRoleByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrRoleNotFound) {
			return nil, invalid("ROLE_NOT_FOUND", "không tìm thấy role")
		}
		return nil, fmt.Errorf("services.RoleService.GetRole: %w", err)
	}
	actions, err := s.repo.ListActionsForRole(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrRoleNotFound) {
			return nil, invalid("ROLE_NOT_FOUND", "không tìm thấy role")
		}
		return nil, fmt.Errorf("services.RoleService.GetRole: load actions: %w", err)
	}
	v := roleToView(role, actions)
	return &v, nil
}

// CreateRole registers a new role and its grants in the tenant. The name is required and unique
// within the tenant (409 ROLE_ALREADY_EXISTS). Each grant is validated (known action verb, known
// access_level, non-empty entity — '*' allowed) BEFORE any write, and its access_level defaults
// to 'all'. The created role's safe view (with grants) is returned.
func (s *RoleService) CreateRole(ctx context.Context, tenantID string, in CreateRoleInput) (*RoleView, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, invalid("ROLE_NAME_REQUIRED", "name role là bắt buộc")
	}
	if auth.IsOwnerRoleName(name) {
		return nil, errRoleNameReserved
	}
	actions, err := s.buildActions(ctx, tenantID, in.Actions)
	if err != nil {
		return nil, err
	}

	role := &auth.Role{
		ID:       s.newID(),
		TenantID: tenantID,
		Name:     name,
		Label:    strings.TrimSpace(in.Label),
	}
	if err := s.repo.CreateRole(ctx, role, actions); err != nil {
		if errors.Is(err, auth.ErrRoleAlreadyExists) {
			return nil, invalid("ROLE_ALREADY_EXISTS", "tên role đã tồn tại trong tổ chức")
		}
		return nil, fmt.Errorf("services.RoleService.CreateRole: %w", err)
	}
	v := roleToView(role, actions)
	// Invalidate the new role's cached grants AFTER commit (defensive — nothing cached yet) (O3).
	s.aclCache.Invalidate(tenantID, role.ID)
	// RM-19: audit the committed role create (after-only diff). Async + fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.RoleMeta, audit.ActionCreate, nil, v)
	s.rec.RecordDirect(ctx, tenantID, "", DirectDoc{DocType: "role", DocKey: role.ID, Label: roleLabel(v), Op: "create", After: v})
	return &v, nil
}

// UpdateRole applies the supplied non-nil scalar fields to a tenant-scoped role and, when
// Actions is non-nil, REPLACES its grant set (each grant re-validated). An unknown/cross-tenant
// id is ROLE_NOT_FOUND; a name collision is 409. The refreshed safe view (with grants) is returned.
func (s *RoleService) UpdateRole(ctx context.Context, tenantID, id string, in UpdateRoleInput) (*RoleView, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("ROLE_ID_REQUIRED", "id role là bắt buộc")
	}

	// Snapshot the before-image (best-effort) so the RM-19 audit diff reflects what changed.
	// A read failure here must not block the update — the audit just records an after-only
	// diff in that case (fail-open).
	// specs/065 D-18: the read is NOT best-effort any more — the reserved-name guard below needs the
	// prior name, and a failed read must refuse rather than let a rename slip through (fail closed).
	priorRole, gerr := s.repo.GetRoleByID(ctx, tenantID, id)
	if gerr != nil {
		if errors.Is(gerr, auth.ErrRoleNotFound) {
			return nil, invalid("ROLE_NOT_FOUND", "không tìm thấy role")
		}
		return nil, fmt.Errorf("services.RoleService.UpdateRole: read role: %w", gerr)
	}
	var beforeView any
	if priorRole != nil {
		if priorActions, aerr := s.repo.ListActionsForRole(ctx, tenantID, id); aerr == nil {
			beforeView = roleToView(priorRole, priorActions)
		} else {
			beforeView = roleToView(priorRole, nil)
		}
	}

	fields := map[string]any{}
	if in.Name != nil {
		nm := strings.TrimSpace(*in.Name)
		if nm == "" {
			return nil, invalid("ROLE_NAME_REQUIRED", "name role không được rỗng")
		}
		// Renaming TO or FROM the reserved name is refused; resending the same name is a no-op.
		if priorRole != nil && nm != priorRole.Name && (auth.IsOwnerRoleName(nm) || auth.IsOwnerRoleName(priorRole.Name)) {
			return nil, errRoleNameReserved
		}
		fields["name"] = nm
	}
	if in.Label != nil {
		fields["label"] = strings.TrimSpace(*in.Label)
	}

	// nil Actions → leave grants untouched; non-nil → validate + replace (even empty clears).
	var actions []auth.RoleAction
	if in.Actions != nil {
		built, err := s.buildActions(ctx, tenantID, in.Actions)
		if err != nil {
			return nil, err
		}
		// Ensure a non-nil empty slice flows through so the repo distinguishes "clear" from "leave".
		if built == nil {
			built = []auth.RoleAction{}
		}
		actions = built
	}

	role, err := s.repo.UpdateRole(ctx, tenantID, id, fields, actions)
	if err != nil {
		if errors.Is(err, auth.ErrRoleNotFound) {
			return nil, invalid("ROLE_NOT_FOUND", "không tìm thấy role")
		}
		if errors.Is(err, auth.ErrRoleAlreadyExists) {
			return nil, invalid("ROLE_ALREADY_EXISTS", "tên role đã tồn tại trong tổ chức")
		}
		return nil, fmt.Errorf("services.RoleService.UpdateRole: %w", err)
	}

	// Re-read grants for the returned view (the update may have replaced them).
	current, err := s.repo.ListActionsForRole(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("services.RoleService.UpdateRole: reload actions: %w", err)
	}
	v := roleToView(role, current)
	// Invalidate the role's cached grants AFTER commit so changed/revoked grants take effect now (O3).
	s.aclCache.Invalidate(tenantID, id)
	// RM-19: audit the committed role update. We record the after-view; the diff highlights the
	// fields present (name/label/actions). Async + fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.RoleMeta, audit.ActionUpdate, beforeView, v)
	s.rec.RecordDirect(ctx, tenantID, "", DirectDoc{DocType: "role", DocKey: id, Label: roleLabel(v), Op: "update", Before: beforeView, After: v})
	return &v, nil
}

// Recreate brings a deleted role back under its own id with its grants and field permissions, then
// gives it to the users in userIDs who hold no role now (specs/063 FR-056). The role, grants, field
// permissions and reassignment are ONE transaction. A role id or name that exists again is
// ROLE_ALREADY_EXISTS. Returns how many users got the role. The owner role is never recreated here.
func (s *RoleService) Recreate(ctx context.Context, tenantID, id string, before RoleView, fieldACL []auth.FieldACL, userIDs []string) (int, error) {
	if s.recreator == nil {
		return 0, errors.New("services.RoleService.Recreate: recreator not wired")
	}
	name := strings.TrimSpace(before.Name)
	if name == "" {
		return 0, invalid("ROLE_NAME_REQUIRED", "name role là bắt buộc")
	}
	if auth.IsOwnerRoleName(name) {
		return 0, errRoleNameReserved
	}
	actions, err := s.buildActions(ctx, tenantID, actionInputs(before.Actions))
	if err != nil {
		return 0, err
	}
	role := &auth.Role{ID: id, TenantID: tenantID, Name: name, Label: strings.TrimSpace(before.Label)}
	n, err := s.recreator.RecreateRole(ctx, role, actions, fieldACL, userIDs)
	if err != nil {
		if errors.Is(err, auth.ErrRoleAlreadyExists) {
			return 0, invalid("ROLE_ALREADY_EXISTS", "tên role đã tồn tại trong tổ chức")
		}
		return 0, fmt.Errorf("services.RoleService.Recreate: %w", err)
	}
	s.aclCache.Invalidate(tenantID, id)
	v := roleToView(role, actions)
	emitMetaAudit(ctx, s.auditor, tenantID, audit.RoleMeta, audit.ActionCreate, nil, v)
	s.rec.RecordDirect(ctx, tenantID, "", DirectDoc{DocType: "role", DocKey: id, Label: roleLabel(v), Op: "create", After: v,
		Meta: map[string]any{"reassignedUserIds": userIDs, "reassigned": n}})
	return int(n), nil
}

// actionInputs turns a stored role view's grants back into inputs, so Recreate re-validates them.
func actionInputs(in []RoleActionView) []RoleActionInput {
	out := make([]RoleActionInput, len(in))
	for i, a := range in {
		out[i] = RoleActionInput{EntityName: a.EntityName, Action: a.Action, AccessLevel: a.AccessLevel}
	}
	return out
}

// DeleteRole removes a tenant-scoped role. The DB FK closes the loose ends: acl_role_actions
// cascades and users.role_id is SET NULL (orphaned users keep their is_admin super-gate). An
// unknown/cross-tenant id is ROLE_NOT_FOUND, never a 500 — so deleting a role currently held by
// users is a clean, consistent operation (R3 orphan guard).
func (s *RoleService) DeleteRole(ctx context.Context, tenantID, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return invalid("ROLE_ID_REQUIRED", "id role là bắt buộc")
	}
	// Snapshot the before-image (best-effort) for a delete = before-only audit diff.
	// specs/065 D-18: the read gates the reserved-name guard, so a failed read refuses (fail closed).
	priorRole, gerr := s.repo.GetRoleByID(ctx, tenantID, id)
	if gerr != nil {
		if errors.Is(gerr, auth.ErrRoleNotFound) {
			return invalid("ROLE_NOT_FOUND", "không tìm thấy role")
		}
		return fmt.Errorf("services.RoleService.DeleteRole: read role: %w", gerr)
	}
	if priorRole != nil && auth.IsOwnerRoleName(priorRole.Name) {
		return errRoleNameReserved
	}
	var beforeView any
	if priorRole != nil {
		if priorActions, aerr := s.repo.ListActionsForRole(ctx, tenantID, id); aerr == nil {
			beforeView = roleToView(priorRole, priorActions)
		} else {
			beforeView = roleToView(priorRole, nil)
		}
	}
	// FR-032: the FKs detach the holders (users.role_id SET NULL) and cascade the field permissions,
	// so both are read BEFORE the delete, pinned to the tenant, for the direct entry's meta.
	var meta map[string]any
	rec := s.rec
	if s.foot != nil && s.rec.Active(ctx, tenantID) {
		users, uerr := s.foot.AssignedUserIDs(ctx, tenantID, id)
		facl, ferr := s.foot.FieldACLRows(ctx, tenantID, id)
		if uerr != nil || ferr != nil {
			metaversion.AddWarning(ctx, VersionWarningMessage)
			rec = nil // cannot describe what the delete takes with it: record nothing rather than a lie
		}
		meta = map[string]any{"assignedUserIds": users, "fieldAcl": facl}
	}
	if err := s.repo.DeleteRole(ctx, tenantID, id); err != nil {
		if errors.Is(err, auth.ErrRoleNotFound) {
			return invalid("ROLE_NOT_FOUND", "không tìm thấy role")
		}
		return fmt.Errorf("services.RoleService.DeleteRole: %w", err)
	}
	// Invalidate the deleted role's cached grants AFTER commit so it can no longer authorize (O3).
	s.aclCache.Invalidate(tenantID, id)
	// RM-19: audit the committed role delete (before-only diff). Async + fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.RoleMeta, audit.ActionDelete, beforeView, nil)
	rec.RecordDirect(ctx, tenantID, "", DirectDoc{DocType: "role", DocKey: id, Label: roleLabelAny(beforeView), Op: "delete", Before: beforeView, Meta: meta})
	return nil
}

// buildActions validates and normalizes a list of grant inputs into auth.RoleAction rows. It
// rejects an empty entity_name, an unknown action verb (ROLE_ACTION_INVALID), and an unknown
// access_level (ROLE_ACCESS_LEVEL_INVALID); an omitted access_level defaults to 'all'. The
// returned rows carry NO role id yet — the repository force-sets it at insert time so a grant
// can never be smuggled onto a different role (R3).
func (s *RoleService) buildActions(ctx context.Context, tenantID string, in []RoleActionInput) ([]auth.RoleAction, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]auth.RoleAction, 0, len(in))
	for _, a := range in {
		entity := strings.TrimSpace(a.EntityName)
		if entity == "" {
			return nil, invalid("ROLE_ENTITY_REQUIRED", "entityName của grant là bắt buộc ('*' = mọi entity)")
		}
		action := strings.TrimSpace(a.Action)
		if _, ok := validRoleActions[action]; !ok {
			return nil, invalid("ROLE_ACTION_INVALID", "action không hợp lệ: %q (chỉ view/create/edit/delete)", a.Action)
		}
		level := strings.TrimSpace(a.AccessLevel)
		if level == "" {
			level = defaultAccessLevel
		}
		if strings.HasPrefix(level, "field:") {
			if err := s.checkFieldLevel(ctx, tenantID, entity, level); err != nil {
				return nil, err
			}
		} else if _, ok := validAccessLevels[level]; !ok {
			return nil, invalid("ROLE_ACCESS_LEVEL_INVALID", "accessLevel không hợp lệ: %q (chỉ none/owner/team/all hoặc field:<tên trường người dùng>)", a.AccessLevel)
		}
		out = append(out, auth.RoleAction{
			ID:          s.newID(),
			EntityName:  entity,
			Action:      action,
			AccessLevel: level,
		})
	}
	return out, nil
}

// checkFieldLevel validates one "field:<name>" grant (#283): the name must be a valid identifier, the
// grant must name ONE entity (a '*' wildcard cannot carry a per-entity field), and that entity must
// have a live user-type field of that name — otherwise the row filter would read a column that does
// not exist. The same check runs on every role save, so a field removed later surfaces as an error
// the next time the matrix is saved rather than as a silent no-op.
func (s *RoleService) checkFieldLevel(ctx context.Context, tenantID, entity, level string) error {
	field, ok := dynrepo.AccessLevel(level).UserField()
	if !ok {
		return invalid("ROLE_ACCESS_LEVEL_INVALID", "accessLevel không hợp lệ: %q (tên trường phải là chữ thường/số/gạch dưới)", level)
	}
	if entity == wildcardEntity || s.userFields == nil {
		return invalid("ROLE_FIELD_SCOPE_INVALID", "quyền theo trường %q cần chọn một bảng cụ thể và một trường Người dùng của bảng đó", field)
	}
	exists, err := s.userFields.UserFieldExists(ctx, tenantID, entity, field)
	if err != nil {
		return fmt.Errorf("services.RoleService.checkFieldLevel: %w", err)
	}
	if !exists {
		return invalid("ROLE_FIELD_SCOPE_INVALID", "bảng %q không có trường Người dùng tên %q", entity, field)
	}
	return nil
}

// roleToView projects a role (+optional grants) to the safe public view.
func roleToView(r *auth.Role, actions []auth.RoleAction) RoleView {
	v := RoleView{
		ID:       r.ID,
		TenantID: r.TenantID,
		Name:     r.Name,
		Label:    r.Label,
		Actions:  []RoleActionView{},
	}
	for _, a := range actions {
		v.Actions = append(v.Actions, RoleActionView{
			EntityName:  a.EntityName,
			Action:      a.Action,
			AccessLevel: a.AccessLevel,
		})
	}
	return v
}

func roleLabel(v RoleView) string {
	if v.Label != "" {
		return v.Label
	}
	return v.Name
}

func roleLabelAny(v any) string {
	if rv, ok := v.(RoleView); ok {
		return roleLabel(rv)
	}
	return ""
}

// roleFootprint reads what deleting a role takes with it (tenant-pinned).
type roleFootprint interface {
	AssignedUserIDs(ctx context.Context, tenantID, roleID string) ([]string, error)
	FieldACLRows(ctx context.Context, tenantID, roleID string) ([]auth.FieldACL, error)
}
