package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/notify"
)

// bcryptCost is the work factor for hashing user passwords (../specs/001-auth-security/spec.md §5:
// "Password: bcrypt cost 12"). It is a package constant so create and password-change paths
// stay in lockstep and a future tuning is a single edit.
const bcryptCost = 12

// ErrUserForbidden is returned by the privileged user-management use cases (CreateUser,
// UpdateUser, DeleteUser) when the acting principal is NOT an admin. It is the service-layer
// half of the is_admin gate (defense-in-depth alongside the RequireAdmin middleware): even if a
// non-admin request somehow reached the service, creating, mutating (is_admin/role_id is an
// escalation) or deleting a user is refused here so a privilege-escalation path cannot open up
// if the route were ever mis-mounted. The handler maps it to a 403.
var ErrUserForbidden = errors.New("services: admin privilege required")

// statusActive is the user.status value that marks a usable, logged-in-capable account. The
// lock-out guards (last-admin / self-delete) only care about admins in THIS state: a disabled
// or otherwise non-active admin cannot log in, so it does not count toward "is there still a
// way into the admin surface".
const statusActive = "active"

// validUserStatus reports whether st is in the closed set the admin user screen can produce
// (spec 065 FR-002). The auth stage only admits "active", so an unknown value silently locks the
// account out of every request while looking fine in the list — reject it at the write instead.
func validUserStatus(st string) bool {
	switch st {
	case statusActive, "inactive", "disabled":
		return true
	}
	return false
}

// userCRUDRepo is the subset of auth.UserRepository the user-management service needs. It is
// declared consumer-side so the service is unit-testable with an in-memory fake and does not
// import a concrete DB type. Every method is tenant-scoped in the real implementation.
//
// The *GuardLastAdmin methods power the anti-lock-out guards race-free: each takes a FOR UPDATE
// lock on the tenant's active-admin set and performs the write in the SAME transaction, so two
// concurrent demote/delete requests racing at the count==2 boundary can never both drop the
// last door into the /admin surface to zero (auth.ErrLastAdmin signals the refusal). The
// service routes an active-admin demotion/deletion through them; benign writes use the plain
// UpdateUser / SoftDeleteUser.
type userCRUDRepo interface {
	ListUsers(ctx context.Context, tenantID string) ([]auth.User, error)
	GetUserByID(ctx context.Context, tenantID, id string) (*auth.User, error)
	CreateUser(ctx context.Context, u *auth.User) error
	UpdateUser(ctx context.Context, tenantID, id string, fields map[string]any) (*auth.User, error)
	// UpdateUserIfManageable is the customer owner's conditional write (specs/065 D-4).
	UpdateUserIfManageable(ctx context.Context, tenantID, id, actorID string, fields map[string]any) (*auth.User, error)
	SoftDeleteUser(ctx context.Context, tenantID, id string) error
	UpdateUserGuardLastAdmin(ctx context.Context, tenantID, id string, fields map[string]any) (*auth.User, error)
	SoftDeleteUserGuardLastAdmin(ctx context.Context, tenantID, id string) error
}

// tokenRevoker is the subset of the refresh-token repository the user service needs to enforce
// the W1 "immediate revoke" rule (../specs/001-auth-security/spec.md §5 claim-freshness): because role_id /
// is_admin live in the short-lived access token, a privilege change or account removal only
// takes effect once the access token expires — UNLESS we cut the user's refresh tokens so they
// cannot mint a fresh access token carrying the OLD (now over-privileged) claims. Declared
// consumer-side and tenant-scoped (a token from tenant A can never be revoked under B). It is
// optional: a nil revoker (noopRevoker) keeps the user service usable without the auth wiring
// (e.g. in focused unit tests), so the W1 path is opt-in via WithTokenRevoker.
type tokenRevoker interface {
	RevokeAllForUser(ctx context.Context, tenantID, userID string) error
}

// roleValidator is the subset of the role repository the user service needs to enforce the
// cross-tenant role guard: a user's role_id MUST name a role that lives in the SAME tenant as
// the user (R3 — no cross-tenant privilege escalation). Declared consumer-side and tenant-scoped
// so the service stays unit-testable with an in-memory fake and never imports a concrete DB type.
// GetRoleByID returns auth.ErrRoleNotFound for an unknown OR cross-tenant id (the real
// auth.RoleRepository pins the query to tenant_id), which the service maps to a 422 ROLE_INVALID.
// It is optional: a nil validator (noopRoleValidator) skips the check, keeping the user service
// usable without the A4 wiring (e.g. focused unit tests); production opts in via WithRoleValidator.
type roleValidator interface {
	GetRoleByID(ctx context.Context, tenantID, id string) (*auth.Role, error)
}

// noopRoleValidator is the default validator when none is injected: it treats every role_id as
// valid. It keeps the validation call site unconditional (no nil checks) while leaving the
// cross-tenant guard inert until the real role repository is wired in via WithRoleValidator.
type noopRoleValidator struct{}

func (noopRoleValidator) GetRoleByID(context.Context, string, string) (*auth.Role, error) {
	return nil, nil
}

// noopRevoker is the default revoker when none is injected: it does nothing. It keeps every
// call site unconditional (no nil checks) while leaving the W1 revoke a no-op until the real
// refresh-token repository is wired in via WithTokenRevoker.
type noopRevoker struct{}

func (noopRevoker) RevokeAllForUser(context.Context, string, string) error { return nil }

// claimInvalidator drops a user's cached LIVE row after a committed user mutation (queue #14
// claim-freshness): middleware.AuthMiddlewareLive rebuilds every request's Subject from that
// row through services.CachedUserLoader, so invalidating here is what makes a role change /
// demotion / disable / delete land on the user's very next request instead of after the cache
// TTL. Declared consumer-side and tenant-scoped like tokenRevoker; optional (noop default) so
// the service stays usable in focused unit tests and pre-#14 wirings.
type claimInvalidator interface {
	Invalidate(tenantID, userID string)
}

// noopClaimInvalidator is the default when no claim cache is wired: it does nothing.
type noopClaimInvalidator struct{}

func (noopClaimInvalidator) Invalidate(string, string) {}

// UserService implements the A3 user-management use cases (CRUD over /admin/users). Every
// method takes the verified tenant id and pins all repository calls to it, so no use case can
// ever touch another tenant's users. CreateUser, UpdateUser and DeleteUser additionally enforce
// the is_admin gate (privilege-escalation guard): a non-admin actor is refused with
// ErrUserForbidden — setting/clearing is_admin or role_id is a direct escalation, so update is
// gated alongside create/delete. It is HTTP-agnostic (the handler maps its typed errors to
// status codes) and holds no per-request state, so it is safe for concurrent use.
type UserService struct {
	repo     userCRUDRepo
	revoker  tokenRevoker     // W1: revokes a user's refresh tokens on privilege change / deletion
	sessions sessionEnder     // #310: ends every session at once on a password set (nil ⇒ revoker only)
	claims   claimInvalidator // #14: drops the user's cached live row after every committed mutation
	roles    roleValidator    // cross-tenant role guard: role_id must live in the user's tenant
	roleList roleLister       // owner tier: the assignable-role picker (nil ⇒ empty list)
	notifier notify.Notifier
	logger   *zap.Logger   // used to log recovered panics from the notifier (fail-open)
	newID    func() string // injectable id generator; defaults to uuid
	// auditor records committed user mutations (RM-19 __user__), async + fail-open. Defaults
	// to a noop so existing wiring/tests run unchanged; injected via WithAuditor.
	auditor audit.Auditor
}

// NewUserService wires the user service over a user CRUD repository. The token revoker defaults
// to a no-op (the W1 immediate-revoke path stays inert) until WithTokenRevoker injects the real
// refresh-token repository. The role validator defaults to a no-op (the cross-tenant role guard
// stays inert) until WithRoleValidator injects the real role repository. The notifier defaults to
// a no-op (security alerts stay inert) until WithNotifier injects the real dispatcher. The logger
// defaults to a no-op until WithLogger injects the real zap logger.
func NewUserService(repo userCRUDRepo) *UserService {
	return &UserService{
		repo:     repo,
		revoker:  noopRevoker{},
		claims:   noopClaimInvalidator{},
		roles:    noopRoleValidator{},
		notifier: notify.NewNoopNotifier(),
		logger:   zap.NewNop(),
		newID:    uuid.NewString,
		auditor:  audit.NewNoopAuditor(),
	}
}

// WithLogger injects the structured logger used to record recovered panics from the notifier
// (fail-open contract: a panicking notifier logs via zap but never surfaces to the caller).
// A nil argument leaves the default noop logger in place. Returns the service for fluent wiring.
func (s *UserService) WithLogger(l *zap.Logger) *UserService {
	if l != nil {
		s.logger = l
	}
	return s
}

// WithNotifier injects the security-alert notifier that emits E1–E4 events (spec §2 /
// ../specs/014-security-alerts/spec.md). A nil argument leaves the default noop in place. Returns the service
// for fluent wiring in main. The notifier is called fail-open: a panic from the notifier is
// recovered and logged, never surfaced to the caller.
func (s *UserService) WithNotifier(n notify.Notifier) *UserService {
	if n != nil {
		s.notifier = n
	}
	return s
}

// enqueueAlert fires a security alert fail-open: if the notifier panics, it recovers and logs
// the panic value via the structured logger (tenant_id, user_id, alert_type) so the drop is
// auditable, then continues — the originating business flow is never interrupted (spec §1).
// No secret (token, hash, password) is ever included in the log fields.
func (s *UserService) enqueueAlert(ctx context.Context, evt notify.SecurityEvent) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Warn("services.UserService.enqueueAlert: notifier panicked, alert dropped",
				zap.String("alert_type", evt.Type),
				zap.String("tenant_id", evt.TenantID),
				zap.String("user_id", evt.UserID),
				zap.Any("panic", r),
			)
		}
	}()
	s.notifier.EnqueueSecurityAlert(ctx, evt)
}

// WithTokenRevoker injects the refresh-token revoker that powers the W1 rule: revoke a user's
// live refresh tokens when an admin changes that user's role_id / is_admin, or deletes the user.
// A nil argument leaves the default no-op in place. Returns the service for fluent wiring in main.
func (s *UserService) WithTokenRevoker(rev tokenRevoker) *UserService {
	if rev != nil {
		s.revoker = rev
	}
	return s
}

// sessionEnder ends every live login session of a user at once: refresh rows revoked as 'ended' AND
// the auth stage's session guard invalidated, so their access tokens are refused on the very next
// request (specs/055 FR-010). AuthService.LogoutEverywhere is the production implementation.
type sessionEnder interface {
	LogoutEverywhere(ctx context.Context, tenantID, userID string) error
}

// WithSessionEnder injects the session ender behind the admin "set password" path (#310): the
// account's every device is signed out immediately instead of riding out the access TTL. nil keeps
// the plain W1 revoke. Returns the service for fluent wiring in main.
func (s *UserService) WithSessionEnder(e sessionEnder) *UserService {
	if e != nil {
		s.sessions = e
	}
	return s
}

// WithClaimCache injects the live-row cache the auth stage reads (services.CachedUserLoader,
// queue #14 claim-freshness) so every committed user create / update / delete drops the target's
// cached row and the change lands on that user's very next request. A nil argument leaves the
// default no-op in place. Returns the service for fluent wiring in main.
func (s *UserService) WithClaimCache(inv claimInvalidator) *UserService {
	if inv != nil {
		s.claims = inv
	}
	return s
}

// WithRoleValidator injects the role repository that powers the cross-tenant role guard: on
// CreateUser / UpdateUser, a supplied role_id is verified to belong to the SAME tenant as the
// user (the lookup is tenant-pinned, so a role from another tenant misses → 422 ROLE_INVALID).
// A nil argument leaves the default no-op in place (validation skipped). Returns the service for
// fluent wiring in main.
func (s *UserService) WithRoleValidator(rv roleValidator) *UserService {
	if rv != nil {
		s.roles = rv
	}
	return s
}

// roleLister is the slice of the role repository the owner's role picker needs (tenant-pinned).
type roleLister interface {
	ListRoles(ctx context.Context, tenantID string) ([]auth.Role, error)
}

// WithRoleLister injects the role repository behind GET /org-admin/roles. A nil argument leaves the
// picker empty. Returns the service for fluent wiring in main.
func (s *UserService) WithRoleLister(rl roleLister) *UserService {
	if rl != nil {
		s.roleList = rl
	}
	return s
}

// WithAuditor injects the RM-19 audit dispatcher so every committed user mutation (create /
// update / delete) emits an append-only __user__ audit row (async, fail-open). The actor user
// id is read from the request context (audit.WithActor, set by the admin handler via
// auditContext); an internal caller without an actor (e.g. the bootstrap-admin CLI) records
// an empty actor — the trail tolerates it. Chainable; a nil auditor is ignored.
func (s *UserService) WithAuditor(a audit.Auditor) *UserService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// UserView is the safe public projection of a user returned to the API. It deliberately omits
// password_hash so it can be serialized straight to a response with no risk of leaking the digest.
type UserView struct {
	ID       string `json:"id"`
	TenantID string `json:"tenantId"`
	Email    string `json:"email"`
	FullName string `json:"fullName,omitempty"`
	Status   string `json:"status"`
	IsAdmin  bool   `json:"isAdmin"`
	// CanManageUsers (specs/065) shows the builder, on /admin/users, which accounts are customer owners.
	CanManageUsers bool    `json:"canManageUsers"`
	RoleID         *string `json:"roleId,omitempty"`
}

// passwordRedacted is the marker the __user__ audit diff carries in place of a changed
// password: the trail shows THAT the credential changed, never what it changed to.
const passwordRedacted = "[redacted]"

// userAuditView is the __user__ audit payload for an update: the safe UserView (no hash) plus
// the redacted marker, set only when the password changed, so a credential rotation is visible
// in the trail without any secret material (plaintext or bcrypt digest).
type userAuditView struct {
	UserView
	Password string `json:"password,omitempty"`
}

// toView projects an auth.User to the safe public view (no password hash).
func toView(u *auth.User) UserView {
	return UserView{
		ID:       u.ID,
		TenantID: u.TenantID,
		Email:    u.Email,
		FullName: u.FullName,
		Status:   u.Status,
		IsAdmin:  u.IsAdmin,

		CanManageUsers: u.CanManageUsers,
		RoleID:         u.RoleID,
	}
}

// normalizeRoleID trims a supplied role_id pointer and collapses an empty/whitespace value to
// nil (no role). This keeps "role_id present but blank" and "role_id absent" equivalent on the
// create path, so the cross-tenant guard only runs for a genuinely supplied role id.
func normalizeRoleID(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return nil
	}
	return &v
}

// sameRole compares two normalized role ids; NULL equals NULL (no role stays no role).
func sameRole(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// validateRole enforces the cross-tenant role guard: when roleID is non-nil, the named role MUST
// belong to tenantID, or the call is a 422 ROLE_INVALID. The lookup is tenant-pinned (the real
// auth.RoleRepository.GetRoleByID returns auth.ErrRoleNotFound for an unknown OR cross-tenant
// id), so a role forged from another tenant can never be attached to a user (R3). A nil roleID
// (no role assigned / cleared) is always valid and skips the lookup.
func (s *UserService) validateRole(ctx context.Context, tenantID string, roleID *string) error {
	if roleID == nil {
		return nil
	}
	if _, err := s.roles.GetRoleByID(ctx, tenantID, *roleID); err != nil {
		if errors.Is(err, auth.ErrRoleNotFound) {
			return invalid("ROLE_INVALID", "role_id không hợp lệ hoặc không thuộc tổ chức")
		}
		return fmt.Errorf("services.UserService.validateRole: %w", err)
	}
	return nil
}

// ListUsers returns the safe public view of every user in the tenant.
func (s *UserService) ListUsers(ctx context.Context, tenantID string) ([]UserView, error) {
	rows, err := s.repo.ListUsers(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.UserService.ListUsers: %w", err)
	}
	views := make([]UserView, len(rows))
	for i := range rows {
		views[i] = toView(&rows[i])
	}
	return views, nil
}

// GetUser returns the safe public view of one tenant-scoped user, or a USER_NOT_FOUND
// ValidationError for an unknown/cross-tenant/soft-deleted id.
func (s *UserService) GetUser(ctx context.Context, tenantID, id string) (*UserView, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("USER_ID_REQUIRED", "id user là bắt buộc")
	}
	u, err := s.repo.GetUserByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, invalid("USER_NOT_FOUND", "không tìm thấy user")
		}
		return nil, fmt.Errorf("services.UserService.GetUser: %w", err)
	}
	v := toView(u)
	return &v, nil
}

// CreateUserInput is the payload to create a user. Email and Password are required; RoleID is
// optional (nil = no role). IsAdmin lets an admin mint another admin.
type CreateUserInput struct {
	Email    string
	Password string
	FullName string
	Status   string
	IsAdmin  bool
	RoleID   *string
}

// CreateUser registers a new user in the tenant. 🔒 GATE: only an admin actor may create a
// user (actorIsAdmin) — a non-admin is refused with ErrUserForbidden BEFORE any write, so an
// ordinary user can never escalate by minting accounts. The password is hashed with bcrypt
// cost 12 and only the digest is persisted. A duplicate (tenant, email) is a 409
// (USER_ALREADY_EXISTS). The created user's safe view is returned.
func (s *UserService) CreateUser(ctx context.Context, tenantID string, actorIsAdmin bool, in CreateUserInput) (*UserView, error) {
	if !actorIsAdmin {
		return nil, ErrUserForbidden
	}

	email := strings.TrimSpace(in.Email)
	if email == "" || in.Password == "" {
		return nil, invalid("USER_FIELD_REQUIRED", "email và password là bắt buộc")
	}
	// RM-17: shared password policy (password_policy.go) — the SAME body ChangePassword and
	// ResetPassword enforce, so an admin-created account can no longer start with a password
	// weaker than what the user themselves would be allowed to set.
	if err := ValidatePassword(in.Password); err != nil {
		return nil, err
	}

	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = statusActive
	}
	if !validUserStatus(status) {
		return nil, invalid("USER_STATUS_INVALID", "status phải là active, inactive hoặc disabled")
	}

	// 🔒 CROSS-TENANT ROLE GUARD (R3): a supplied role_id must name a role in THIS tenant.
	// Skipped when no role is assigned (nil or empty). A role from another tenant is a 422
	// ROLE_INVALID — assigning it would grant the new user that foreign role's privileges.
	roleID := normalizeRoleID(in.RoleID)
	if err := s.validateRole(ctx, tenantID, roleID); err != nil {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("services.UserService.CreateUser: hash password: %w", err)
	}

	u := &auth.User{
		ID:           s.newID(),
		TenantID:     tenantID,
		Email:        email,
		PasswordHash: string(hash),
		FullName:     strings.TrimSpace(in.FullName),
		Status:       status,
		IsAdmin:      in.IsAdmin,
		RoleID:       roleID,
	}
	if err := s.repo.CreateUser(ctx, u); err != nil {
		if errors.Is(err, auth.ErrUserAlreadyExists) {
			return nil, invalid("USER_ALREADY_EXISTS", "email đã tồn tại trong tổ chức")
		}
		return nil, fmt.Errorf("services.UserService.CreateUser: %w", err)
	}
	v := toView(u)
	// #14 claim-freshness: publish-after-commit — drop any cached live row for the new id
	// (defensive; nothing can be cached yet, but the rule "every committed __user__ mutation
	// invalidates" stays uniform across the three sites).
	s.claims.Invalidate(tenantID, u.ID)
	// RM-19: audit the committed user create (after-only diff of the safe view — never the
	// hash; the password itself is implied by the create, not recorded). record_id = the new
	// user's id, like every __user__ row. Async + fail-open.
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.UserMeta, u.ID, audit.ActionCreate, nil, v)
	return &v, nil
}

// UpdateUserInput carries the mutable user attributes. Pointer fields use apply-when-non-nil
// semantics: an omitted key leaves the stored value untouched, while an explicit value (even
// false / "") is applied. Password, when non-nil and non-empty, is re-hashed (bcrypt cost 12).
type UpdateUserInput struct {
	FullName *string
	Status   *string
	IsAdmin  *bool
	RoleID   *string // when non-nil, sets/clears the role (empty string clears to NULL)
	Password *string
}

// UpdateUser applies the supplied non-nil fields to a tenant-scoped user and returns the
// refreshed safe view. An unknown/cross-tenant id is USER_NOT_FOUND. A password change is
// re-hashed with bcrypt cost 12; the plaintext is never stored. Email is intentionally NOT
// updatable here (it is the login identity; changing it is out of A3 scope).
//
// 🔒 GATE: only an admin actor may update a user (actorIsAdmin) — a non-admin is refused with
// ErrUserForbidden BEFORE any read or write. Setting is_admin / role_id is a direct
// privilege-escalation operation, so this carries the SAME service-layer defense-in-depth gate
// as CreateUser / DeleteUser (in case the route were ever mis-mounted off the RequireAdmin
// middleware) — the prior asymmetry that left update un-gated was bug §17.
//
// 🔒 ANTI-LOCK-OUT: an update that would strip the LAST active admin of its access — either by
// demoting it (is_admin=false) or disabling it (status != "active") — is refused with a 422
// LAST_ADMIN, so a tenant can never edit itself out of every admin (self-demotion included).
// The check+write run atomically under a FOR UPDATE lock in the repo (UpdateUserGuardLastAdmin)
// so two concurrent demotions cannot both pass at the count==2 boundary (TOCTOU fix, bug §19).
func (s *UserService) UpdateUser(ctx context.Context, tenantID, actorUserID string, actorIsAdmin bool, id string, in UpdateUserInput) (*UserView, error) {
	if !actorIsAdmin {
		return nil, ErrUserForbidden
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("USER_ID_REQUIRED", "id user là bắt buộc")
	}

	// Pre-read the target so we can tell whether this update demotes an active admin (which
	// must go through the locking guard). An unknown/cross-tenant/soft-deleted id is a
	// not-found here (same as the write path below).
	target, err := s.repo.GetUserByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, invalid("USER_NOT_FOUND", "không tìm thấy user")
		}
		return nil, fmt.Errorf("services.UserService.UpdateUser: %w", err)
	}
	// specs/065 B2: an admin without the operator flag may not touch an operator account.
	if err := s.adminMayAct(ctx, tenantID, actorUserID, target); err != nil {
		return nil, err
	}
	// RM-19 before-image: a value snapshot taken NOW, before the write, so the audit diff can
	// never alias a repository that mutates the returned row in place.
	before := toView(target)

	// Does this update demote the target out of the active-admin set? That is true when the
	// target IS currently an active admin AND the request either clears is_admin or moves it
	// off "active". Only then must the write go through the last-admin locking guard.
	demotesAdmin := target.IsAdmin && target.Status == statusActive &&
		((in.IsAdmin != nil && !*in.IsAdmin) ||
			(in.Status != nil && strings.TrimSpace(*in.Status) != statusActive))

	// spec 065 FR-001: revoke + E2/E3 key off a REAL change versus the stored row, not off a key
	// being present — the admin form resends role/isAdmin/status on every save, so presence
	// alone logged the user out (and mailed "role changed") on a bare rename. Computed from the
	// pre-read BEFORE the write: a repo may mutate the returned row in place.
	roleChanged := in.RoleID != nil && !sameRole(normalizeRoleID(in.RoleID), target.RoleID)
	adminChanged := in.IsAdmin != nil && *in.IsAdmin != target.IsAdmin
	// specs/065: admin and customer owner are mutually exclusive (CHECK chk_users_tier). Refuse BEFORE
	// the write so the CHECK's 3819 never surfaces as a 500; the repo maps it too as the last net.
	if adminChanged && *in.IsAdmin && target.CanManageUsers {
		return nil, errUserTierConflict
	}
	statusLeavesActive := in.Status != nil && strings.TrimSpace(*in.Status) != target.Status &&
		strings.TrimSpace(*in.Status) != statusActive

	fields := map[string]any{}
	if in.FullName != nil {
		fields["full_name"] = strings.TrimSpace(*in.FullName)
	}
	if in.Status != nil {
		st := strings.TrimSpace(*in.Status)
		if st == "" || !validUserStatus(st) {
			return nil, invalid("USER_STATUS_INVALID", "status phải là active, inactive hoặc disabled")
		}
		fields["status"] = st
	}
	if in.IsAdmin != nil {
		fields["is_admin"] = *in.IsAdmin
	}
	if in.RoleID != nil {
		rid := strings.TrimSpace(*in.RoleID)
		if rid == "" {
			fields["role_id"] = nil // explicit clear to NULL
		} else {
			// 🔒 CROSS-TENANT ROLE GUARD (R3): re-assigning a role_id must name a role in THIS
			// tenant, or it is a 422 ROLE_INVALID. Clearing to NULL needs no check.
			if err := s.validateRole(ctx, tenantID, &rid); err != nil {
				return nil, err
			}
			fields["role_id"] = rid
		}
	}
	if in.Password != nil && *in.Password != "" {
		// RM-17: shared password policy — same body as CreateUser/ChangePassword/ResetPassword.
		if err := ValidatePassword(*in.Password); err != nil {
			return nil, err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*in.Password), bcryptCost)
		if err != nil {
			return nil, fmt.Errorf("services.UserService.UpdateUser: hash password: %w", err)
		}
		fields["password_hash"] = string(hash)
		// Security MED-1: an admin password reset is the standard remediation for a locked-out /
		// compromised account, so it MUST clear the durable lockout in the SAME atomic UPDATE —
		// otherwise the new password login still 401s against a stale locked_until. GORM map-Updates
		// writes a nil value as SQL NULL.
		fields["failed_login_count"] = 0
		fields["locked_until"] = nil
	}

	// Demotion of an active admin goes through the locking guard (atomic count-then-write under
	// FOR UPDATE); every other update is a plain tenant-scoped write.
	var u *auth.User
	if demotesAdmin {
		u, err = s.repo.UpdateUserGuardLastAdmin(ctx, tenantID, id, fields)
	} else {
		u, err = s.repo.UpdateUser(ctx, tenantID, id, fields)
	}
	if err != nil {
		if errors.Is(err, auth.ErrLastAdmin) {
			return nil, invalid("LAST_ADMIN", "không thể hạ quyền hoặc vô hiệu hoá admin active cuối cùng")
		}
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, invalid("USER_NOT_FOUND", "không tìm thấy user")
		}
		if errors.Is(err, auth.ErrUserAlreadyExists) {
			return nil, invalid("USER_ALREADY_EXISTS", "email đã tồn tại trong tổ chức")
		}
		if errors.Is(err, auth.ErrUserTierConflict) {
			return nil, errUserTierConflict
		}
		return nil, fmt.Errorf("services.UserService.UpdateUser: %w", err)
	}
	v := toView(u)

	// #14 claim-freshness: publish-after-commit — the auth stage rebuilds this user's Subject
	// from the cached live row, so drop it NOW (before audit/revoke, which may still fail the
	// request) and the new role / is_admin / status is what their very next request runs under.
	s.claims.Invalidate(tenantID, target.ID)

	// RM-19: audit the committed user update — before = the pre-write snapshot (the same read
	// the last-admin guard needs), after = the refreshed safe view, so the diff holds only what
	// changed. A password change is visible solely as the after-side marker
	// {password:"[redacted]"}: UserView carries no hash and the plaintext never reaches the
	// trail. record_id = the TARGET user's id: the diff alone cannot say WHICH account an
	// isAdmin/roleId/status/password flip happened to. Emitted right after the commit so the
	// trail reflects the DB even if the revoke below fails the request. Async + fail-open.
	passwordChanged := in.Password != nil && *in.Password != ""
	after := userAuditView{UserView: v}
	if passwordChanged {
		after.Password = passwordRedacted
	}
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.UserMeta, target.ID, audit.ActionUpdate, before, after)

	// 🔒 W1 (claim-freshness): if this update either changed the user's PRIVILEGE claims —
	// role_id or is_admin — OR deprovisioned them by moving status off "active" (disable) — OR
	// rotated the user's password, revoke all their live refresh tokens. The old access token
	// still carries the stale claim in its PAYLOAD, but since #14 the Auth stage re-reads
	// is_admin/role_id (and refuses a non-active user) from the users row on every request
	// (middleware.refreshSubject), so a privilege change or a disable takes effect on the NEXT
	// request — not after the access TTL. Revoking refresh is still required: it stops the user
	// minting a fresh token, and it is the ONLY bound for a PASSWORD change, which
	// refreshSubject does not look at (the old access token keeps working until it expires).
	// (../specs/001-auth-security/spec.md §5). Disabling a user is the strongest
	// deprovisioning, so — like role/is_admin change — it MUST cut refresh, or the disabled user
	// could keep refreshing for the full RefreshTTL (bug §6). A password change MUST also cut
	// refresh: spec §5 requires "đổi mật khẩu thu hồi hết", and an admin password reset is the
	// standard remediation for a compromised account — leaving the attacker's live refresh token
	// alive for the full RefreshTTL would defeat that remediation. The password term mirrors the
	// persist guard at line 291 (in.Password != nil && *in.Password != "") so revoke fires iff a
	// new password was actually written, and the `||` chain revokes exactly once when password
	// and a privilege claim change together. A revoke failure fails the request so a privilege
	// change / disable / password change is never silently un-enforced.
	//
	// #310: a PASSWORD set additionally ENDS every session (EndAllSessions + guard invalidation, the
	// same path as logout-all) so the old credential's access tokens die on their very next request
	// instead of surviving ≤ 15 min — refreshSubject does not look at the password. The other
	// triggers keep the plain revoke: a role change must land as a 403 on the next request, not as a
	// sign-out (TestA4_ClaimFreshness_RoleDowngradeIsNextRequest403).
	switch {
	case passwordChanged && s.sessions != nil:
		if err := s.sessions.LogoutEverywhere(ctx, tenantID, id); err != nil {
			return nil, fmt.Errorf("services.UserService.UpdateUser: end sessions: %w", err)
		}
	case roleChanged || adminChanged || statusLeavesActive || passwordChanged:
		if err := s.revoker.RevokeAllForUser(ctx, tenantID, id); err != nil {
			return nil, fmt.Errorf("services.UserService.UpdateUser: revoke refresh tokens: %w", err)
		}
	}

	// Security alerts (E1–E3, spec §2, ../specs/014-security-alerts/spec.md). Emitted fail-open AFTER the
	// write + revoke succeed, so a notifier failure can never undo the business operation.
	// Each distinct sensitive event emits its own alert (E1 + E2 may fire together).
	emailSnap := u.Email
	now := time.Now().UTC()
	if passwordChanged {
		// E1: password change
		s.enqueueAlert(ctx, notify.SecurityEvent{
			Type:     notify.AlertPasswordChanged,
			TenantID: tenantID,
			UserID:   id,
			Email:    emailSnap,
			At:       now,
		})
	}
	if roleChanged || adminChanged {
		// E2: privilege change (role_id or is_admin really changed)
		s.enqueueAlert(ctx, notify.SecurityEvent{
			Type:     notify.AlertRoleChanged,
			TenantID: tenantID,
			UserID:   id,
			Email:    emailSnap,
			At:       now,
		})
	}
	if statusLeavesActive {
		// E3: account disabled / suspended
		s.enqueueAlert(ctx, notify.SecurityEvent{
			Type:     notify.AlertAccountDisabled,
			TenantID: tenantID,
			UserID:   id,
			Email:    emailSnap,
			At:       now,
		})
	}

	return &v, nil
}

// DeleteUser soft-deletes a tenant-scoped user. 🔒 GATE: only an admin actor may delete a user
// (actorIsAdmin) — a non-admin is refused with ErrUserForbidden BEFORE any write, mirroring the
// create gate so account removal is never available to ordinary users. An unknown/cross-tenant
// id is USER_NOT_FOUND.
//
// 🔒 ANTI-LOCK-OUT: two further guards run before the delete:
//   - SELF_DELETE_FORBIDDEN (422): an admin may not delete its OWN account (actorUserID == id),
//     so a slip can't strand the actor mid-session.
//   - LAST_ADMIN (422): the target may not be the tenant's LAST active admin (target is an
//     active admin && CountActiveAdmins == 1), so the tenant can never lock itself out of /admin.
func (s *UserService) DeleteUser(ctx context.Context, tenantID, actorUserID string, actorIsAdmin bool, id string) error {
	if !actorIsAdmin {
		return ErrUserForbidden
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return invalid("USER_ID_REQUIRED", "id user là bắt buộc")
	}

	// Block self-deletion: an admin removing its own account is almost always a mistake and can
	// orphan the live session. Compared on the verified actor subject, never a client field.
	if strings.TrimSpace(actorUserID) == id {
		return invalid("SELF_DELETE_FORBIDDEN", "không thể tự xoá chính tài khoản của mình")
	}

	// Read the target to see whether it is an active admin; if so it may be the last door into
	// /admin and the soft-delete must go through the locking guard. An unknown/cross-tenant/
	// soft-deleted id is a not-found.
	target, err := s.repo.GetUserByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return invalid("USER_NOT_FOUND", "không tìm thấy user")
		}
		return fmt.Errorf("services.UserService.DeleteUser: %w", err)
	}
	// specs/065 B2: an admin without the operator flag may not delete an operator account.
	if err := s.adminMayAct(ctx, tenantID, actorUserID, target); err != nil {
		return err
	}
	// RM-19 before-image: value snapshot before the write (see UpdateUser).
	before := toView(target)

	// Deleting an active admin goes through the last-admin locking guard (atomic count-then-
	// delete under FOR UPDATE), so two concurrent admin deletes cannot both pass at the
	// count==2 boundary (TOCTOU fix, bug §19). Deleting a non-admin / disabled target cannot
	// lock the tenant out of /admin, so it is a plain tenant-scoped soft-delete.
	if target.IsAdmin && target.Status == statusActive {
		err = s.repo.SoftDeleteUserGuardLastAdmin(ctx, tenantID, id)
	} else {
		err = s.repo.SoftDeleteUser(ctx, tenantID, id)
	}
	if err != nil {
		if errors.Is(err, auth.ErrLastAdmin) {
			return invalid("LAST_ADMIN", "không thể xoá admin active cuối cùng")
		}
		if errors.Is(err, auth.ErrUserNotFound) {
			return invalid("USER_NOT_FOUND", "không tìm thấy user")
		}
		return fmt.Errorf("services.UserService.DeleteUser: %w", err)
	}

	// #14 claim-freshness: publish-after-commit — a soft-deleted row is ErrUserNotFound to the
	// auth stage's reader, so dropping the cached row makes the removed user's very next
	// request 401 instead of riding the cache for its TTL.
	s.claims.Invalidate(tenantID, target.ID)

	// RM-19: audit the committed soft-delete (before-only diff of the target's safe view,
	// record_id = the removed user's id). Emitted right after the commit so the trail reflects
	// the DB even if the revoke below fails the request. Async + fail-open.
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.UserMeta, target.ID, audit.ActionDelete, before, nil)

	// 🔒 W1 (claim-freshness): a deleted user must lose the ability to come back via refresh.
	// Soft-delete hides the row from auth lookups, but any live refresh token would still let
	// the holder mint a fresh access token — so revoke them all (../specs/001-auth-security/spec.md §5). A
	// revoke failure fails the request so a removed account is never left able to re-authenticate.
	if err := s.revoker.RevokeAllForUser(ctx, tenantID, id); err != nil {
		return fmt.Errorf("services.UserService.DeleteUser: revoke refresh tokens: %w", err)
	}

	// E4: account deleted alert (fail-open, AFTER the business operations succeed).
	s.enqueueAlert(ctx, notify.SecurityEvent{
		Type:     notify.AlertAccountDeleted,
		TenantID: tenantID,
		UserID:   id,
		Email:    target.Email,
		At:       time.Now().UTC(),
	})
	return nil
}
