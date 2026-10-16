package auth

import (
	"errors"
	"time"
)

// ErrSSOProviderNotFound is returned when no sso_providers row matches a (tenant, id) or
// (tenant, issuer) lookup — unknown id/issuer, or one belonging to another tenant.
var ErrSSOProviderNotFound = errors.New("auth: sso provider not found")

// ErrSSOProviderAlreadyExists is returned by Create when the (tenant_id, issuer) unique
// index fires — a tenant may register a given issuer at most once (migration 000031).
var ErrSSOProviderAlreadyExists = errors.New("auth: sso provider already exists")

// ErrSSOIdentityNotFound is returned when no sso_identities row matches (tenant, provider,
// subject) — the caller's IdP-asserted `sub` has never logged in before under this provider.
var ErrSSOIdentityNotFound = errors.New("auth: sso identity not found")

// SSOProvider is one tenant's registered OIDC identity provider (ENG-AUTH2 #8,
// ../specs/004-sso-oidc/spec.md §"#8"). It is a fixed system table -> GORM-managed
// (rules/database.md) and always carries tenant_id.
//
// ClientSecretEnc holds the AES-256-GCM SEALED (reversible, NOT hashed — internal/crypto)
// OAuth2 client secret; it must be reversible because the token-exchange call needs the raw
// secret back to authenticate against the IdP (a one-way hash cannot support that, mirrors
// UserMFA.SecretEnc). json:"-" so a handler can never accidentally serialize it even if a
// future refactor forgets to strip it in a DTO.
//
// Enabled gates BOTH admin visibility (a disabled provider still lists, so an admin can
// re-enable it) and the public /start + /callback resolution (a disabled provider answers
// SSO_PROVIDER_NOT_FOUND, closing the window where disabling mid-flow would otherwise still
// complete a login). JITProvisioning is the per-provider opt-in for auto-creating a local user
// on first login from an unrecognized `sub` with no matching local email — it defaults to
// false (off) at the database level; see ../specs/004-sso-oidc/spec.md §"#8" safe-default.
//
// Enabled deliberately carries NO `gorm:"default:..."` tag (reviewer FIX 1, HIGH blocker): GORM
// omits a field from the generated INSERT (and, for a literal default, even overwrites the Go
// struct field back to that literal) whenever the field's value equals its zero value AND the
// field has a `default` tag. bool's zero value is `false` — so a `default:1` tag meant
// `CreateProvider(..., Enabled: false)` silently persisted (and even echoed back in the create
// response) as `enabled=1`, letting a provider an admin explicitly created disabled go live
// immediately (fail-open on the auth surface). CreateSSOProviderInput/CreateProvider already
// resolve Enabled to a concrete, always-set bool before it reaches this model (nil -> true,
// non-nil -> *value — see sso_service.go), so a GORM-level default was always redundant here; the
// SQL-level `DEFAULT 1` in migration 000031 remains the schema's own documentation of the
// column's default, it is just never relied on via this Go path. Compare UserMFA.Enabled
// (mfa.go), whose tag is `default:0` — that one is harmless because 0 IS bool's zero value, so
// GORM's skip-and-let-the-DB-default-apply behavior never diverges from the caller's intent.
//
// TrustIdpMfa (SSO-MFA-POLICY, migration 000040) is the per-provider opt-in that lets a
// found-identity SSO login SKIP the local MFA gate (mfaLoginGate) because an admin explicitly
// trusts this IdP to run its own strong MFA. Defaults to false (off) — the MFA-ENFORCE-safe
// default: local MFA is ALWAYS enforced for enrolled accounts unless a Founder/admin
// deliberately flips this flag via PATCH /admin/sso/providers/:id (it is NOT settable at
// create). SECURITY ASSUMPTION (deliberate, documented — docs/api-conventions.md §3c.8): this
// is BLANKET trust in the provider — the callback does NOT inspect the id_token's amr/acr
// claims per login. Tag `default:0` is safe for the same reason JITProvisioning's is (0 IS
// bool's zero value — see the Enabled paragraph above).
type SSOProvider struct {
	ID              string    `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID        string    `gorm:"type:char(36)" json:"tenantId"`
	Issuer          string    `gorm:"type:varchar(512)" json:"issuer"`
	ClientID        string    `gorm:"type:varchar(255)" json:"clientId"`
	ClientSecretEnc string    `gorm:"type:text" json:"-"`
	DisplayName     string    `gorm:"type:varchar(150)" json:"displayName,omitempty"`
	Enabled         bool      `gorm:"type:tinyint(1)" json:"enabled"`
	JITProvisioning bool      `gorm:"column:jit_provisioning;type:tinyint(1);default:0" json:"jitProvisioning"`
	TrustIdpMfa     bool      `gorm:"column:trust_idp_mfa;type:tinyint(1);default:0" json:"trustIdpMfa"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// TableName pins the fixed system table.
func (SSOProvider) TableName() string { return "sso_providers" }

// SSOIdentity links one IdP-asserted subject (`sub`, unique within the issuing provider) to
// exactly one local user, scoped to a tenant. It is a fixed system table -> GORM-managed and
// always carries tenant_id. UNIQUE(tenant_id, provider_id, subject) (migration 000031) is what
// makes the callback resolution lookup unambiguous and prevents the same IdP identity from
// silently double-linking under one provider.
//
// Email is the IdP-asserted email AT LINK TIME (informational / audit trail only) — it is
// never re-consulted for authorization after the row exists; every subsequent login resolves
// strictly by (tenant_id, provider_id, subject), never by email again (../specs/004-sso-oidc/spec.md
// §"#8" safe-default: no implicit re-linking by email drift).
type SSOIdentity struct {
	ID         string    `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID   string    `gorm:"type:char(36)" json:"tenantId"`
	ProviderID string    `gorm:"type:char(36)" json:"providerId"`
	UserID     string    `gorm:"type:char(36)" json:"userId"`
	Subject    string    `gorm:"type:varchar(255)" json:"subject"`
	Email      string    `gorm:"type:varchar(255)" json:"email,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// TableName pins the fixed system table.
func (SSOIdentity) TableName() string { return "sso_identities" }

// SSOHandoffToken is the short-lived, single-use, OPAQUE bridge between a successful SSO
// /callback identity resolution and the frontend /exchange XHR that mints the session
// (SSO-FE-REDIRECT, migration 000041). It carries ONLY (tenant_id, user_id) — the identity SSO
// already resolved — and only the SHA-256 hash of the raw code is persisted (mirrors
// MFAPendingToken / PasswordResetToken exactly). Session issuance + the local-MFA gate are
// re-run at exchange from this row, so a handoff code is NOT a session and cannot be redeemed at
// /auth/refresh; the raw code travels through the redirect URL but the session tokens never do.
type SSOHandoffToken struct {
	ID         string     `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID   string     `gorm:"type:char(36)" json:"tenantId"`
	UserID     string     `gorm:"type:char(36)" json:"userId"`
	ProviderID string     `gorm:"type:char(36)" json:"providerId"`
	TokenHash  string     `gorm:"type:char(64);uniqueIndex:uq_sso_handoff_token_hash" json:"-"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	UsedAt     *time.Time `json:"usedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// TableName pins the fixed system table.
func (SSOHandoffToken) TableName() string { return "sso_handoff_tokens" }
