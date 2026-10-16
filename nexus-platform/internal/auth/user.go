package auth

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// Tenant is a customer/organization. It is a fixed system table → GORM-managed
// (rules/database.md). Login resolves a human-friendly slug to the tenant id, which
// becomes the trust boundary for every downstream query (../specs/001-auth-security/spec.md §2 A1).
type Tenant struct {
	ID     string `gorm:"type:char(36);primaryKey" json:"id"`
	Slug   string `gorm:"type:varchar(64);uniqueIndex" json:"slug"`
	Name   string `gorm:"type:varchar(150)" json:"name"`
	Status string `gorm:"type:varchar(20);default:active" json:"status"`
	// Branding is the white-label document (AO-THUONG-HIEU p1, mig 000055): raw JSON
	// {appName?, logoUrl?, accentColor?}, validated by services.TenantBrandingService.
	// NULL/empty = default Nexus branding. Never serialized here — the branding endpoints
	// return the parsed document, not the row.
	Branding json.RawMessage `gorm:"type:json" json:"-"`
	// Payment is the VietQR bank config (DONG-TIEN-KHEP-KIN p1, mig 000057): raw JSON
	// {bankBin, accountNumber, accountName?}, validated by services.TenantPaymentService.
	// NULL = not configured.
	Payment json.RawMessage `gorm:"type:json" json:"-"`
	// DraftState is set only by the draft layer's overlay read of the branding document
	// (specs/063-draft-publish S7-3): "live" or "edited". Never stored.
	DraftState string `gorm:"-" json:"-"`
	// MaintenanceUntil / MaintenanceReason: the per-tenant write freeze (specs/068 slice B, mig 000093).
	// A deadline, not a flag: it lapses by itself. NULL = not frozen. Read through the freeze checker,
	// never serialized with the row.
	MaintenanceUntil  *time.Time     `gorm:"type:datetime(3)" json:"-"`
	MaintenanceReason *string        `gorm:"type:varchar(200)" json:"-"`
	CreatedAt         time.Time      `json:"createdAt"`
	UpdatedAt         time.Time      `json:"updatedAt"`
	DeletedAt         gorm.DeletedAt `gorm:"index" json:"-"`
}

// TableName pins the fixed system table.
func (Tenant) TableName() string { return "tenants" }

// User is an authenticatable principal scoped to exactly one tenant. The email is unique
// only within a tenant (uq_users_tenant_email), so login takes {tenant, email, password}.
// password_hash holds a bcrypt digest and is NEVER serialized to API responses (json:"-").
type User struct {
	ID           string `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID     string `gorm:"type:char(36);index" json:"tenantId"`
	Email        string `gorm:"type:varchar(255)" json:"email"`
	PasswordHash string `gorm:"type:varchar(255)" json:"-"`
	FullName     string `gorm:"type:varchar(150)" json:"fullName,omitempty"`
	Status       string `gorm:"type:varchar(20);default:active" json:"status"`
	IsAdmin      bool   `gorm:"type:tinyint(1);default:0" json:"isAdmin"`
	// IsPlatformOperator (TENANT-PROVISION, mig 000052) gates the operator-only /admin/tenants
	// provisioning routes — an authority tier ABOVE tenant-admin. Read FRESH per request by
	// middleware.RequirePlatformOperator (never a JWT claim → instant revoke). json:"-" so it is
	// never serialized AND cannot be set by any request-bound JSON; no `default:1` tag (the GORM
	// zero-value trap), so a normal CreateUser lands 0. Grantable ONLY via bootstrap-admin / SQL.
	IsPlatformOperator bool `gorm:"type:tinyint(1);not null;default:0" json:"-"`
	// CanManageUsers (specs/065-customer-owner-tier, mig 000097) marks the customer owner: a NON-admin
	// who manages the other accounts of their own tenant through /org-admin/*. Read FRESH per request by
	// middleware.RequireUserManager (never a JWT claim, so revoking is instant). json:"-" and no
	// `default:1` tag, like IsPlatformOperator: it cannot be bound from any request JSON. chk_users_tier
	// keeps it mutually exclusive with IsAdmin.
	CanManageUsers bool    `gorm:"type:tinyint(1);not null;default:0" json:"-"`
	RoleID         *string `gorm:"type:char(36)" json:"roleId,omitempty"`
	// FailedLoginCount / LockedUntil implement the persistent account lockout (prod-ops
	// slice b, mig 000043): consecutive wrong-password attempts and the moment until which
	// login is refused (NULL = not locked). Written ONLY via RecordFailedLogin /
	// ResetLoginFailures; never serialized to API responses (json:"-").
	FailedLoginCount int            `gorm:"type:int;not null;default:0" json:"-"`
	LockedUntil      *time.Time     `gorm:"type:timestamp(3)" json:"-"`
	CreatedAt        time.Time      `json:"createdAt"`
	UpdatedAt        time.Time      `json:"updatedAt"`
	DeletedAt        gorm.DeletedAt `gorm:"index" json:"-"`
}

// TableName pins the fixed system table.
func (User) TableName() string { return "users" }

// ManageableBy is THE rule for "may this actor (a customer owner) change this account": a plain staff
// account that is not the actor, not an admin/owner/operator, and not parked as disabled (the builder's
// and the acceptance scripts' state, never the owner's to reopen). One definition shared by the list
// projection, the service pre-check and the repository's re-classification after a conditional UPDATE
// (specs/065 D-4), so the three can never drift.
func (u *User) ManageableBy(actorID string) bool {
	return !u.IsAdmin && !u.CanManageUsers && !u.IsPlatformOperator && u.Status != "disabled" && u.ID != actorID
}
