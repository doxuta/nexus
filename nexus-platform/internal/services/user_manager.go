package services

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/notify"
)

// The customer-owner tier (specs/065-customer-owner-tier lát B). An owner is a NON-admin user with
// users.can_manage_users: they see every account of their own tenant, assign an existing role and
// lock/reopen plain staff. They never create admins, never touch roles' content, never see the tenant
// id or the operator flag. Every method takes the verified tenant id and the actor id from the token
// and re-reads the actor's row (defense in depth beside middleware.RequireUserManager).

var errUserTierConflict = invalid("USER_TIER_CONFLICT", "tài khoản quản trị không thể đồng thời là chủ khách")

// OrgUserView is what an owner sees of a tenant account: no operator flag, no tenant id, no password.
type OrgUserView struct {
	ID       string  `json:"id"`
	Email    string  `json:"email"`
	FullName string  `json:"fullName,omitempty"`
	Status   string  `json:"status"`
	RoleID   *string `json:"roleId,omitempty"`
	// Tier is "admin" (any is_admin, operators included), "owner" or "staff".
	Tier string `json:"tier"`
	// Manageable is auth.User.ManageableBy for the calling owner: false rows are read-only in the UI.
	Manageable bool `json:"manageable"`
}

// OrgRoleView is one entry of the owner's role picker: label + whether it may be assigned. No grants.
type OrgRoleView struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Assignable bool   `json:"assignable"`
}

// OrgUpdateInput is everything an owner may change. There is deliberately no isAdmin, password or
// canManageUsers field: a request key for them is dropped by the struct, not checked.
type OrgUpdateInput struct {
	FullName *string
	Status   *string // "active" | "inactive"
	RoleID   *string // "" clears to NULL
}

func toOrgUserView(u *auth.User, actorID string) OrgUserView {
	tier := "staff"
	switch {
	case u.IsAdmin:
		tier = "admin"
	case u.CanManageUsers:
		tier = "owner"
	}
	return OrgUserView{
		ID: u.ID, Email: u.Email, FullName: u.FullName, Status: u.Status, RoleID: u.RoleID,
		Tier: tier, Manageable: u.ManageableBy(actorID),
	}
}

// loadManager re-reads the actor and requires an ACTIVE, non-admin owner; anything else is ErrUserForbidden.
func (s *UserService) loadManager(ctx context.Context, tenantID, actorID string) error {
	actor, err := s.repo.GetUserByID(ctx, tenantID, strings.TrimSpace(actorID))
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return ErrUserForbidden
		}
		return fmt.Errorf("services.UserService.loadManager: %w", err)
	}
	if actor.Status != statusActive || actor.IsAdmin || !actor.CanManageUsers {
		return ErrUserForbidden
	}
	return nil
}

// ListOrgUsers returns every live account of the owner's tenant (tenant-pinned in the repo).
func (s *UserService) ListOrgUsers(ctx context.Context, tenantID, actorID string) ([]OrgUserView, error) {
	if err := s.loadManager(ctx, tenantID, actorID); err != nil {
		return nil, err
	}
	rows, err := s.repo.ListUsers(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.UserService.ListOrgUsers: %w", err)
	}
	out := make([]OrgUserView, len(rows))
	for i := range rows {
		out[i] = toOrgUserView(&rows[i], actorID)
	}
	return out, nil
}

// GetOrgUser returns one account; an unknown or other-tenant id is USER_NOT_FOUND.
func (s *UserService) GetOrgUser(ctx context.Context, tenantID, actorID, id string) (*OrgUserView, error) {
	if err := s.loadManager(ctx, tenantID, actorID); err != nil {
		return nil, err
	}
	u, err := s.repo.GetUserByID(ctx, tenantID, strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, invalid("USER_NOT_FOUND", "không tìm thấy user")
		}
		return nil, fmt.Errorf("services.UserService.GetOrgUser: %w", err)
	}
	v := toOrgUserView(u, actorID)
	return &v, nil
}

// ListAssignableRoles is the owner's role picker: label (falls back to the name) + assignable. The
// reserved owner role is listed but not assignable. No grant is exposed.
func (s *UserService) ListAssignableRoles(ctx context.Context, tenantID, actorID string) ([]OrgRoleView, error) {
	if err := s.loadManager(ctx, tenantID, actorID); err != nil {
		return nil, err
	}
	if s.roleList == nil {
		return []OrgRoleView{}, nil
	}
	rows, err := s.roleList.ListRoles(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.UserService.ListAssignableRoles: %w", err)
	}
	out := make([]OrgRoleView, 0, len(rows))
	for _, r := range rows {
		label := strings.TrimSpace(r.Label)
		if label == "" {
			label = r.Name
		}
		out = append(out, OrgRoleView{ID: r.ID, Label: label, Assignable: !auth.IsOwnerRoleName(r.Name)})
	}
	return out, nil
}

// UpdateOrgUser changes a plain staff account's name / status / role on behalf of an owner.
//
// Order: actor gate → read target (404) → manageable (403, before touching the input) → normalise input
// (422) → ONE conditional UPDATE (repo.UpdateUserIfManageable closes the race with an operator who
// promotes the target in between) → invalidate the claim cache → audit → revoke + alert only when the
// role or the status REALLY changed (same detection as the admin path).
func (s *UserService) UpdateOrgUser(ctx context.Context, tenantID, actorID, id string, in OrgUpdateInput) (*OrgUserView, error) {
	if err := s.loadManager(ctx, tenantID, actorID); err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	target, err := s.repo.GetUserByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, invalid("USER_NOT_FOUND", "không tìm thấy user")
		}
		return nil, fmt.Errorf("services.UserService.UpdateOrgUser: %w", err)
	}
	if !target.ManageableBy(actorID) {
		return nil, auth.ErrUserNotManageable
	}
	before := toView(target)

	fields := map[string]any{}
	if in.FullName != nil {
		fields["full_name"] = strings.TrimSpace(*in.FullName)
	}
	newStatus := ""
	if in.Status != nil {
		newStatus = strings.TrimSpace(*in.Status)
		if newStatus != statusActive && newStatus != "inactive" {
			return nil, invalid("USER_STATUS_INVALID", "status phải là active hoặc inactive")
		}
		fields["status"] = newStatus
	}
	roleChanged := false
	if in.RoleID != nil {
		rid := normalizeRoleID(in.RoleID)
		if rid == nil {
			fields["role_id"] = nil
		} else {
			role, rerr := s.roles.GetRoleByID(ctx, tenantID, *rid)
			if rerr != nil {
				if errors.Is(rerr, auth.ErrRoleNotFound) {
					return nil, invalid("ROLE_INVALID", "role_id không hợp lệ hoặc không thuộc tổ chức")
				}
				return nil, fmt.Errorf("services.UserService.UpdateOrgUser: role lookup: %w", rerr)
			}
			if role != nil && auth.IsOwnerRoleName(role.Name) {
				return nil, invalid("ROLE_NOT_ASSIGNABLE", "vai này do người dựng giữ, không thể gán")
			}
			fields["role_id"] = *rid
		}
		roleChanged = !sameRole(rid, target.RoleID)
	}
	statusLeavesActive := in.Status != nil && newStatus != target.Status && newStatus != statusActive

	u, err := s.repo.UpdateUserIfManageable(ctx, tenantID, id, actorID, fields)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrUserNotFound):
			return nil, invalid("USER_NOT_FOUND", "không tìm thấy user")
		case errors.Is(err, auth.ErrUserNotManageable):
			return nil, auth.ErrUserNotManageable
		}
		return nil, fmt.Errorf("services.UserService.UpdateOrgUser: %w", err)
	}
	s.claims.Invalidate(tenantID, id)
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.UserMeta, id, audit.ActionUpdate, before, toView(u))

	if roleChanged || statusLeavesActive {
		if err := s.revoker.RevokeAllForUser(ctx, tenantID, id); err != nil {
			return nil, fmt.Errorf("services.UserService.UpdateOrgUser: revoke refresh tokens: %w", err)
		}
	}
	now := time.Now().UTC()
	if roleChanged {
		s.enqueueAlert(ctx, notify.SecurityEvent{Type: notify.AlertRoleChanged, TenantID: tenantID, UserID: id, Email: u.Email, At: now})
	}
	if statusLeavesActive {
		s.enqueueAlert(ctx, notify.SecurityEvent{Type: notify.AlertAccountDisabled, TenantID: tenantID, UserID: id, Email: u.Email, At: now})
	}
	v := toOrgUserView(u, actorID)
	return &v, nil
}

// OrgCreateInput is everything an owner supplies to add a staff account. No password, isAdmin or
// canManageUsers: the first is generated server-side and never returned, the others are always false.
type OrgCreateInput struct {
	Email    string
	FullName string
	RoleID   *string
}

// AssertManageable gates the owner's invite / session routes: active owner actor (403), target in the
// same tenant (404), and a plain staff account (403 USER_NOT_MANAGEABLE) — admins, operators, other
// owners, disabled accounts and the owner themself are refused.
func (s *UserService) AssertManageable(ctx context.Context, tenantID, actorID, targetID string) error {
	if err := s.loadManager(ctx, tenantID, actorID); err != nil {
		return err
	}
	target, err := s.repo.GetUserByID(ctx, tenantID, strings.TrimSpace(targetID))
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return invalid("USER_NOT_FOUND", "không tìm thấy user")
		}
		return fmt.Errorf("services.UserService.AssertManageable: %w", err)
	}
	if !target.ManageableBy(actorID) {
		return auth.ErrUserNotManageable
	}
	return nil
}

// CreateStaff adds a plain, active staff account on behalf of an owner. The password is a random
// secret that is hashed and dropped: the new person sets their own through the invite link.
//
// ponytail: 144 bits is enough BECAUSE it is never returned, logged or audited; the reset link replaces it.
func (s *UserService) CreateStaff(ctx context.Context, tenantID, actorID string, in OrgCreateInput) (*OrgUserView, error) {
	if err := s.loadManager(ctx, tenantID, actorID); err != nil {
		return nil, err
	}
	email := strings.TrimSpace(in.Email)
	if email == "" {
		return nil, invalid("USER_FIELD_REQUIRED", "email là bắt buộc")
	}
	// The invite mail goes to this address: refuse anything that is not a bare address.
	if a, perr := mail.ParseAddress(email); perr != nil || a.Address != email {
		return nil, invalid("USER_EMAIL_INVALID", "email không đúng định dạng")
	}
	roleID := normalizeRoleID(in.RoleID)
	if roleID != nil {
		role, err := s.roles.GetRoleByID(ctx, tenantID, *roleID)
		if err != nil {
			if errors.Is(err, auth.ErrRoleNotFound) {
				return nil, invalid("ROLE_INVALID", "role_id không hợp lệ hoặc không thuộc tổ chức")
			}
			return nil, fmt.Errorf("services.UserService.CreateStaff: role lookup: %w", err)
		}
		if role != nil && auth.IsOwnerRoleName(role.Name) {
			return nil, invalid("ROLE_NOT_ASSIGNABLE", "vai này do người dựng giữ, không thể gán")
		}
	}
	pw, err := generateInitialPassword()
	if err != nil {
		return nil, fmt.Errorf("services.UserService.CreateStaff: generate password: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("services.UserService.CreateStaff: hash password: %w", err)
	}
	u := &auth.User{
		ID: s.newID(), TenantID: tenantID, Email: email, PasswordHash: string(hash),
		FullName: strings.TrimSpace(in.FullName), Status: statusActive, RoleID: roleID,
	}
	if err := s.repo.CreateUser(ctx, u); err != nil {
		if errors.Is(err, auth.ErrUserAlreadyExists) {
			return nil, invalid("USER_ALREADY_EXISTS", "email đã tồn tại trong tổ chức")
		}
		return nil, fmt.Errorf("services.UserService.CreateStaff: %w", err)
	}
	s.claims.Invalidate(tenantID, u.ID)
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.UserMeta, u.ID, audit.ActionCreate, nil, toView(u))
	v := toOrgUserView(u, actorID)
	return &v, nil
}

// SetUserManager is the operator's switch for the customer-owner capability. actorIsOperator comes
// from the handler's single callerIsOperator helper. Turning it OFF writes only the flag: the user's
// role is untouched, so an owner who still carries the all-data role keeps seeing the data (FR-008).
func (s *UserService) SetUserManager(ctx context.Context, tenantID string, actorIsOperator bool, id string, enabled bool) (*UserView, error) {
	if !actorIsOperator {
		return nil, ErrUserForbidden
	}
	id = strings.TrimSpace(id)
	target, err := s.repo.GetUserByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, invalid("USER_NOT_FOUND", "không tìm thấy user")
		}
		return nil, fmt.Errorf("services.UserService.SetUserManager: %w", err)
	}
	if enabled && target.IsAdmin {
		return nil, errUserTierConflict
	}
	if target.CanManageUsers == enabled {
		v := toView(target)
		return &v, nil
	}
	before := toView(target)
	u, err := s.repo.UpdateUser(ctx, tenantID, id, map[string]any{"can_manage_users": enabled})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrUserNotFound):
			return nil, invalid("USER_NOT_FOUND", "không tìm thấy user")
		case errors.Is(err, auth.ErrUserTierConflict):
			return nil, errUserTierConflict
		}
		return nil, fmt.Errorf("services.UserService.SetUserManager: %w", err)
	}
	s.claims.Invalidate(tenantID, id)
	v := toView(u)
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.UserMeta, id, audit.ActionUpdate, before, v)
	if err := s.revoker.RevokeAllForUser(ctx, tenantID, id); err != nil {
		return nil, fmt.Errorf("services.UserService.SetUserManager: revoke refresh tokens: %w", err)
	}
	s.enqueueAlert(ctx, notify.SecurityEvent{Type: notify.AlertRoleChanged, TenantID: tenantID, UserID: id, Email: u.Email, At: time.Now().UTC()})
	return &v, nil
}

// IsPlatformOperator reads the caller's row FRESH and reports the operator flag (handler helper
// callerIsOperator, specs/065 D-14). A missing user is simply not an operator.
func (s *UserService) IsPlatformOperator(ctx context.Context, tenantID, userID string) (bool, error) {
	u, err := s.repo.GetUserByID(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("services.UserService.IsPlatformOperator: %w", err)
	}
	return u.IsAdmin && u.IsPlatformOperator && u.Status == statusActive, nil
}

// AssertAdminMayAct is rule B2 (FR-018): an admin without the operator flag cannot act on an operator
// account. The target is read first (404 if unknown); the actor is read ONLY when the target is an
// operator. A vanished actor fails closed. Used by UpdateUser / DeleteUser and by the handler before
// reset-link and session routes.
func (s *UserService) AssertAdminMayAct(ctx context.Context, tenantID, actorID, targetID string) error {
	target, err := s.repo.GetUserByID(ctx, tenantID, strings.TrimSpace(targetID))
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return invalid("USER_NOT_FOUND", "không tìm thấy user")
		}
		return fmt.Errorf("services.UserService.AssertAdminMayAct: %w", err)
	}
	return s.adminMayAct(ctx, tenantID, actorID, target)
}

func (s *UserService) adminMayAct(ctx context.Context, tenantID, actorID string, target *auth.User) error {
	if !target.IsPlatformOperator {
		return nil
	}
	op, err := s.IsPlatformOperator(ctx, tenantID, strings.TrimSpace(actorID))
	if err != nil {
		return fmt.Errorf("services.UserService.adminMayAct: %w", err)
	}
	if !op {
		return auth.ErrUserNotManageable
	}
	return nil
}
