package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/project"
)

// TenantSlugPattern bounds a tenant login slug to a URL/CLI-safe token that also fits
// tenants.slug VARCHAR(64). It is what the customer types at the login screen. Shared by
// cmd/bootstrap-admin AND POST /admin/tenants (TENANT-PROVISION) so the two provisioning paths
// can never drift on what a valid slug is (../specs/024-tenant-provisioning/spec.md §4.1).
var TenantSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidTenantSlug reports whether slug is an acceptable tenant login slug.
func ValidTenantSlug(slug string) bool { return TenantSlugPattern.MatchString(slug) }

// maxTenantNameRunes caps the display name at the tenants.name VARCHAR(150) bound so an
// over-long name fails cleanly (422 TENANT_NAME_TOO_LONG) instead of a MySQL 1406 → 500.
const maxTenantNameRunes = 150

// errTenantSlugTaken is the in-transaction sentinel for a duplicate tenant slug (uq on
// tenants.slug). It is mapped to the public 409 TENANT_ALREADY_EXISTS by Provision.
var errTenantSlugTaken = errors.New("tenant slug already exists")

// TenantProvisionService creates a NEW customer tenant + its first admin in ONE transaction and
// lists the tenant registry for the deploy wizard. It is operator-only at the HTTP layer
// (middleware.RequirePlatformOperator); the service itself is HTTP-agnostic and assumes the caller
// is already authorized. It reuses the server's password policy (ValidatePassword, RM-17) and
// bcrypt cost (bcryptCost) so a provisioned admin is byte-for-byte a working, loggable-in admin —
// the SAME building blocks UserService.CreateUser uses, but assembled inside a tx so a partial
// failure can never leave a tenant with no admin (../specs/024-tenant-provisioning/spec.md §5.1).
type TenantProvisionService struct {
	db      *gorm.DB
	newID   func() string
	logger  *zap.Logger
	auditor audit.Auditor
	// blobs deletes a purged tenant's stored files (WithBlobRemover). Optional — see PurgeTenant,
	// which reports honestly when it is absent rather than implying the files went with the rows.
	blobs blobRemover
	// backups deletes a purged tenant's per-customer backup files (WithBackupRemover, specs/068).
	// Optional, like blobs: when absent the purge says the files remain.
	backups backupRemover
	// claims / revoker land an owner-tier switch on the converted accounts' very next request
	// (WithClaimCache / WithTokenRevoker, same wiring as UserService). Noop by default.
	claims  claimInvalidator
	revoker tokenRevoker
}

// NewTenantProvisionService wires the service over the GORM handle (server tables are GORM-managed).
// The auditor defaults to a noop until injected via WithAuditor.
func NewTenantProvisionService(db *gorm.DB) *TenantProvisionService {
	return &TenantProvisionService{db: db, newID: uuid.NewString, logger: zap.NewNop(), auditor: audit.NewNoopAuditor(),
		claims: noopClaimInvalidator{}, revoker: noopRevoker{}}
}

// WithClaimCache injects the live-row cache so a converted account's cached row is dropped at once.
func (s *TenantProvisionService) WithClaimCache(inv claimInvalidator) *TenantProvisionService {
	if inv != nil {
		s.claims = inv
	}
	return s
}

// WithTokenRevoker injects the refresh-token revoker used to end sessions after an owner-tier switch.
func (s *TenantProvisionService) WithTokenRevoker(rev tokenRevoker) *TenantProvisionService {
	if rev != nil {
		s.revoker = rev
	}
	return s
}

// WithLogger sets the structured logger (chainable). nil keeps the Nop logger.
func (s *TenantProvisionService) WithLogger(l *zap.Logger) *TenantProvisionService {
	if l != nil {
		s.logger = l
	}
	return s
}

// WithAuditor injects the RM-19 audit dispatcher so a successful provision emits a persistent
// audit row into the OPERATOR's tenant (the detection control the no-rate-limit posture relies on).
func (s *TenantProvisionService) WithAuditor(a audit.Auditor) *TenantProvisionService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// ProvisionTenantInput is the create-workspace request. Exactly ONE of AdminPassword /
// GeneratePassword must be set (a caller either brings a password or asks the server to mint one).
type ProvisionTenantInput struct {
	Slug             string
	Name             string
	AdminEmail       string
	AdminFullName    string
	AdminPassword    string
	GeneratePassword bool
	// ActorTenantID is the OPERATOR's tenant — the tenant the RM-19 provision audit row is written
	// into (the actor user id is read from ctx). Empty skips the audit (e.g. a non-HTTP caller).
	ActorTenantID string
	// Project, when set, writes the customer's registry profile row (specs/062 L6) in the SAME
	// transaction as the tenant, its first user and its calendar. nil keeps the historic behaviour.
	Project *ProjectSeed
}

// ProjectSeed is the registry profile a project-creating caller hands to Provision. Origin is one
// of project.OriginPack / OriginBlank / OriginAI; CreatedBy is the builder behind the call.
type ProjectSeed struct {
	Industry   *string
	Origin     string
	OriginPack *string
	CreatedBy  string
}

// TenantView is the safe public projection of a tenants row (no soft-delete/internal columns).
type TenantView struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

// TenantListItem is a registry row for the deploy wizard's workspace selector: the tenant plus a
// count of its usable (active, non-deleted) admins — enough to tell "this workspace has a login"
// WITHOUT leaking any email/user of another tenant (spec §4.2).
type TenantListItem struct {
	TenantView
	UsableAdminCount int `json:"usableAdminCount"`
	// UsableOwnerCount is the active customer owners (can_manage_users); ConvertibleAdminCount the
	// non-operator admins without a role that "Chuyển sang chủ khách" would convert (specs/065 lát D).
	UsableOwnerCount      int `json:"usableOwnerCount"`
	ConvertibleAdminCount int `json:"convertibleAdminCount"`
	// RetiredAt is set for a soft-deleted workspace, nil for a live one. The registry MUST be able
	// to see retired workspaces: purge only accepts an already-retired tenant, so a list that hid
	// them left the second half of the lifecycle unreachable from the product — retire it and it
	// vanishes, with no way back to finish the job. Consumers that pick a DEPLOY TARGET must skip
	// any item with this set: a retired workspace refuses logins and inbound signals, so installing
	// into one silently produces a dead handover.
	RetiredAt *time.Time `json:"retiredAt,omitempty"`
	// MaintenanceUntil is set while the workspace is write-frozen (specs/068 slice B) and the instant
	// has not passed yet; nil otherwise. The studio panel shows "Đang bảo trì đến HH:MM" from it.
	MaintenanceUntil *time.Time `json:"maintenanceUntil,omitempty"`
}

// ProvisionedTenant is the result of Provision: the new tenant + admin views and, ONLY when the
// server minted the password, the plaintext ONCE (never persisted, logged, or re-fetchable —
// mirrors the show-once secret contract of POST /admin/api-keys, spec §4.1).
type ProvisionedTenant struct {
	Tenant          TenantView
	Admin           UserView
	OneTimePassword string
}

// Provision creates a new customer tenant + its first admin atomically and returns the views (plus
// the one-time password when generated). All PURE validations run BEFORE any I/O, so a bad input
// leaves zero rows behind. The first account is the CUSTOMER OWNER (specs/065 lát D2): is_admin=0,
// can_manage_users=1, carrying the reserved tenant_owner role seeded in the same transaction — the
// customer never gets the builder's admin area. is_platform_operator=0 ALWAYS — provisioning never
// mints another operator.
func (s *TenantProvisionService) Provision(ctx context.Context, in ProvisionTenantInput) (*ProvisionedTenant, error) {
	// 1. Pure validations (no DB touch until every one passes — spec §5.1 step 1).
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	if slug == "" {
		return nil, requiredFieldErr("slug")
	}
	if !ValidTenantSlug(slug) {
		return nil, invalid("TENANT_SLUG_INVALID", "slug %q không hợp lệ — dùng chữ thường, số, '-' hoặc '_' (bắt đầu bằng chữ/số, tối đa 64 ký tự)", in.Slug)
	}
	email := strings.TrimSpace(in.AdminEmail)
	if email == "" {
		return nil, requiredFieldErr("adminEmail")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = slug
	}
	if len([]rune(name)) > maxTenantNameRunes {
		return nil, invalid("TENANT_NAME_TOO_LONG", "tên workspace vượt %d ký tự", maxTenantNameRunes)
	}
	fullName := strings.TrimSpace(in.AdminFullName)
	if fullName == "" {
		fullName = defaultFullNameFromEmail(email)
	}
	// Password mode is a strict XOR: exactly one of AdminPassword / GeneratePassword.
	hasPassword := in.AdminPassword != ""
	if hasPassword == in.GeneratePassword {
		return nil, invalid("TENANT_PASSWORD_CONFLICT", "cung cấp ĐÚNG MỘT trong 'adminPassword' hoặc 'generatePassword'")
	}
	password := in.AdminPassword
	generated := false
	if in.GeneratePassword {
		p, err := generateInitialPassword()
		if err != nil {
			return nil, fmt.Errorf("services.TenantProvisionService.Provision: mint password: %w", err)
		}
		password, generated = p, true
	}
	// RM-17 policy — the SAME body every set-password path enforces (never trimmed).
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}

	// 2. bcrypt OUTSIDE the transaction (CPU-heavy; never hold a tx across it).
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("services.TenantProvisionService.Provision: hash: %w", err)
	}

	// 3. Atomic insert: tenant + first admin. A duplicate slug rolls back the WHOLE tx (no orphan
	//    tenant) and surfaces as 409 — re-POST is deliberately NOT idempotent (spec §4.4).
	tenant := auth.Tenant{ID: s.newID(), Slug: slug, Name: name, Status: "active"}
	admin := auth.User{
		ID:                 s.newID(),
		TenantID:           tenant.ID,
		Email:              email,
		PasswordHash:       string(hash),
		FullName:           fullName,
		Status:             "active",
		IsAdmin:            false, // customer owner, not a builder admin (specs/065 D2)
		CanManageUsers:     true,
		IsPlatformOperator: false, // ALWAYS — provisioning never mints another operator
	}
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&tenant).Error; err != nil {
			if isMySQLDuplicate(err) {
				return errTenantSlugTaken
			}
			return fmt.Errorf("create tenant: %w", err)
		}
		// The role goes in AFTER the tenant (fk_acl_roles_tenant) and BEFORE the user that carries it.
		ownerRoleID, _, err := SeedOwnerRole(ctx, tx, tenant.ID)
		if err != nil {
			return fmt.Errorf("seed owner role: %w", err)
		}
		admin.RoleID = &ownerRoleID
		if err := tx.Create(&admin).Error; err != nil {
			return fmt.Errorf("create admin: %w", err)
		}
		// Default working calendar (DAILY-QUEUE #10 / Q9). Inside the SAME tx so a duplicate slug
		// rolls the calendar back too — the zero-orphan guarantee TestProvision_DuplicateSlug pins
		// covers every row this transaction writes, not just the tenant.
		if _, err := SeedDefaultBusinessCalendar(ctx, tx, tenant.ID); err != nil {
			return fmt.Errorf("seed business calendar: %w", err)
		}
		if in.Project != nil {
			origin := in.Project.Origin
			if err := tx.Create(&project.Profile{
				ID: s.newID(), TenantID: tenant.ID, Industry: in.Project.Industry, Origin: &origin,
				OriginPack: in.Project.OriginPack, CreatedBy: in.Project.CreatedBy, UpdatedBy: in.Project.CreatedBy,
			}).Error; err != nil {
				return fmt.Errorf("create project profile: %w", err)
			}
		}
		return nil
	})
	if txErr != nil {
		if errors.Is(txErr, errTenantSlugTaken) {
			return nil, invalid("TENANT_ALREADY_EXISTS", "workspace với slug %q đã tồn tại", slug)
		}
		return nil, fmt.Errorf("services.TenantProvisionService.Provision: %w", txErr)
	}

	// RM-19: persistent audit of the provision into the OPERATOR's tenant (actor id from ctx). The
	// `after` carries ONLY the non-secret shape — never the password (§6 #11; this is the detection
	// control the no-rate-limit decision, §11 #4, leans on).
	if in.ActorTenantID != "" {
		emitMetaAudit(ctx, s.auditor, in.ActorTenantID, "__tenant__", "provision", nil, struct {
			Slug       string `json:"slug"`
			Name       string `json:"name"`
			AdminEmail string `json:"adminEmail"`
		}{Slug: tenant.Slug, Name: tenant.Name, AdminEmail: admin.Email})
	}

	out := &ProvisionedTenant{
		Tenant: TenantView{ID: tenant.ID, Slug: tenant.Slug, Name: tenant.Name, Status: tenant.Status, CreatedAt: tenant.CreatedAt},
		Admin:  UserView{ID: admin.ID, TenantID: admin.TenantID, Email: admin.Email, FullName: admin.FullName, Status: admin.Status, IsAdmin: admin.IsAdmin, CanManageUsers: admin.CanManageUsers, RoleID: admin.RoleID},
	}
	if generated {
		out.OneTimePassword = password // caller (handler) echoes it ONCE; never stored/logged
	}
	return out, nil
}

// ListTenants returns the tenant registry (newest first) for the deploy wizard's target selector,
// each row carrying a count of usable (active, non-deleted) admins — no email/user of any tenant
// is exposed (spec §4.2). Includes non-active tenants so an operator can see a suspended workspace.
func (s *TenantProvisionService) ListTenants(ctx context.Context) ([]TenantListItem, error) {
	// Unscoped: retired workspaces stay in the registry, flagged. They are exactly the ones the
	// operator still has work to do on (purge), and hiding them made that work unreachable.
	var tenants []auth.Tenant
	if err := s.db.WithContext(ctx).Unscoped().Order("created_at DESC").Find(&tenants).Error; err != nil {
		return nil, fmt.Errorf("services.TenantProvisionService.ListTenants: list: %w", err)
	}
	// One grouped query for all three counts (GORM scopes users to deleted_at IS NULL).
	type countRow struct {
		TenantID    string
		Admins      int
		Owners      int
		Convertible int
	}
	var rows []countRow
	if err := s.db.WithContext(ctx).
		Model(&auth.User{}).
		Select("tenant_id, " +
			"COALESCE(SUM(is_admin = 1 AND status = 'active'), 0) AS admins, " +
			"COALESCE(SUM(can_manage_users = 1 AND status = 'active'), 0) AS owners, " +
			"COALESCE(SUM(is_admin = 1 AND is_platform_operator = 0 AND role_id IS NULL), 0) AS convertible").
		Group("tenant_id").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("services.TenantProvisionService.ListTenants: count accounts: %w", err)
	}
	countByTenant := make(map[string]countRow, len(rows))
	for _, r := range rows {
		countByTenant[r.TenantID] = r
	}
	out := make([]TenantListItem, 0, len(tenants))
	for i := range tenants {
		item := TenantListItem{
			TenantView: TenantView{
				ID: tenants[i].ID, Slug: tenants[i].Slug, Name: tenants[i].Name,
				Status: tenants[i].Status, CreatedAt: tenants[i].CreatedAt,
			},
			UsableAdminCount:      countByTenant[tenants[i].ID].Admins,
			UsableOwnerCount:      countByTenant[tenants[i].ID].Owners,
			ConvertibleAdminCount: countByTenant[tenants[i].ID].Convertible,
		}
		if mu := tenants[i].MaintenanceUntil; mu != nil && mu.After(time.Now()) {
			item.MaintenanceUntil = mu
		}
		if tenants[i].DeletedAt.Valid {
			at := tenants[i].DeletedAt.Time
			item.RetiredAt = &at
			// A retired workspace's admins cannot log in at all, whatever the users table says.
			// Reporting "2 admin dùng được" next to a dead workspace would be a plain lie.
			item.UsableAdminCount, item.UsableOwnerCount, item.ConvertibleAdminCount = 0, 0, 0
		}
		out = append(out, item)
	}
	return out, nil
}

// RetireTenant soft-deletes a customer workspace: the row keeps its data, but every door closes.
// Verified against the live engine before this route existed (both checks are why soft-delete is
// enough and a hard delete is not needed): login to a soft-deleted tenant answers 401, and an
// inbound webhook for it answers 401 even with a CORRECT signature — the receive path resolves
// the tenant first and a deleted one resolves to nothing.
//
// Two refusals, both about foot-guns rather than permissions:
//
//   - a tenant may not retire ITSELF. The caller is a platform operator working inside the Nexus
//     factory tenant; letting that call succeed would lock every operator out of the factory —
//     including out of this endpoint — with no route back except SQL, which is the exact hole
//     this route exists to close.
//   - an unknown or already-retired id is TENANT_NOT_FOUND, so a double-click is not an error and
//     nothing reports success for work it did not do.
//
// Recovery is deliberately NOT exposed here: clearing deleted_at is a one-line SQL for an operator
// who can already reach the database, and an un-delete button is a much larger blast radius than
// the delete one (it revives credentials, hooks and data in one click, with no way to tell whether
// the workspace was retired for a security reason).
func (s *TenantProvisionService) RetireTenant(ctx context.Context, callerTenantID, tenantID string) error {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return &ValidationError{Code: "TENANT_NOT_FOUND", Msg: "không tìm thấy workspace"}
	}
	if tenantID == strings.TrimSpace(callerTenantID) {
		return &ValidationError{
			Code: "TENANT_SELF_RETIRE",
			Msg:  "không thể xoá chính workspace bạn đang đứng — sẽ tự khoá mình ra ngoài",
		}
	}

	res := s.db.WithContext(ctx).Where("id = ?", tenantID).Delete(&auth.Tenant{})
	if res.Error != nil {
		return fmt.Errorf("services.TenantProvisionService.RetireTenant: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return &ValidationError{Code: "TENANT_NOT_FOUND", Msg: "không tìm thấy workspace"}
	}
	if s.logger != nil {
		s.logger.Warn("services.TenantProvisionService.RetireTenant: workspace retired",
			zap.String("tenant_id", tenantID), zap.String("by_tenant", callerTenantID))
	}
	return nil
}

// requiredFieldErr builds the 422 TENANT_FIELD_REQUIRED with details.field so the FE can highlight.
func requiredFieldErr(field string) error {
	return &ValidationError{
		Code:    "TENANT_FIELD_REQUIRED",
		Msg:     fmt.Sprintf("thiếu trường bắt buộc %q", field),
		Details: map[string]any{"field": field},
	}
}

// defaultFullNameFromEmail derives a cosmetic full name from the email local-part (mirrors
// bootstrap-admin.defaultName). full_name is nullable, so this is best-effort only.
func defaultFullNameFromEmail(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

// generateInitialPassword mints a 144-bit URL-safe password (24 base64url chars) from crypto/rand.
// It comfortably clears RM-17 (≥8 runes, ≤72 bytes) and exists ONLY in the response returned once.
func generateInitialPassword() (string, error) {
	buf := make([]byte, 18) // 18 bytes → 24 base64url chars, 144 bits of entropy
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// isMySQLDuplicate reports whether err is a MySQL duplicate-key (1062) violation.
func isMySQLDuplicate(err error) bool {
	var me *gomysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
