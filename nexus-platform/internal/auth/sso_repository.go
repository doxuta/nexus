package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// SSOProviderRepository persists and resolves sso_providers rows over GORM. Every query is
// parameterized and pinned to tenant_id (rules/database.md) — there is no tenant-agnostic
// lookup on this repository (unlike the pending-token/reset-token repos, a provider is always
// resolved by an already-known tenant: the path slug on /start, or the tenant embedded in the
// signed state on /callback — never a bare client-supplied id with no tenant context).
type SSOProviderRepository struct {
	db *gorm.DB
}

// NewSSOProviderRepository wires an SSOProviderRepository over an existing GORM connection.
func NewSSOProviderRepository(db *gorm.DB) *SSOProviderRepository {
	return &SSOProviderRepository{db: db}
}

// Create inserts a brand-new provider row. A duplicate (tenant_id, issuer) is reported as
// ErrSSOProviderAlreadyExists (migration 000031's UNIQUE(tenant_id, issuer)); any other
// failure is wrapped.
func (r *SSOProviderRepository) Create(ctx context.Context, p *SSOProvider) error {
	if err := r.db.WithContext(ctx).Create(p).Error; err != nil {
		if isDuplicateEntry(err) {
			return ErrSSOProviderAlreadyExists
		}
		return fmt.Errorf("auth.SSOProviderRepository.Create: %w", err)
	}
	return nil
}

// List returns every provider registered for a tenant (enabled AND disabled — an admin needs
// to see a disabled row in order to re-enable it), ordered by created_at then id for a stable
// page. Always pinned to tenant_id.
func (r *SSOProviderRepository) List(ctx context.Context, tenantID string) ([]*SSOProvider, error) {
	var rows []*SSOProvider
	err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("created_at ASC, id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("auth.SSOProviderRepository.List: %w", err)
	}
	return rows, nil
}

// GetByID returns the tenant-scoped provider row for id regardless of its enabled flag — used
// by admin Delete's existence check, where an admin must be able to target a disabled provider
// too. Returns ErrSSOProviderNotFound for an unknown id or one belonging to another tenant.
func (r *SSOProviderRepository) GetByID(ctx context.Context, tenantID, id string) (*SSOProvider, error) {
	var p SSOProvider
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSSOProviderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.SSOProviderRepository.GetByID: %w", err)
	}
	return &p, nil
}

// GetEnabledByID returns the tenant-scoped, ENABLED provider row for id. Used by the public
// /start and /callback resolution: a disabled provider (mid-flow disable included — /callback
// re-resolves independently of whatever /start saw) must not complete a login.
func (r *SSOProviderRepository) GetEnabledByID(ctx context.Context, tenantID, id string) (*SSOProvider, error) {
	var p SSOProvider
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ? AND enabled = ?", tenantID, id, true).
		First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSSOProviderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.SSOProviderRepository.GetEnabledByID: %w", err)
	}
	return &p, nil
}

// GetEnabledByIssuer returns the tenant-scoped, ENABLED provider row whose issuer matches
// exactly. Used by /start as a fallback when the `provider` query param is not a known
// provider id (the caller passed the issuer URL itself instead — see sso_handler.go doc).
func (r *SSOProviderRepository) GetEnabledByIssuer(ctx context.Context, tenantID, issuer string) (*SSOProvider, error) {
	var p SSOProvider
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND issuer = ? AND enabled = ?", tenantID, issuer, true).
		First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSSOProviderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.SSOProviderRepository.GetEnabledByIssuer: %w", err)
	}
	return &p, nil
}

// SetEnabled flips ONLY the `enabled` column for a tenant-scoped provider row (admin re-enable
// gap closed — reviewer FIX 1: api-conventions.md §3c.4 promises "List hiện disabled để admin BẬT
// LẠI" but no endpoint could actually do it). Deliberately uses a single-column
// `.Model(...).Update("enabled", ...)` rather than `.Updates(&SSOProvider{Enabled: enabled})` —
// GORM's `Updates` with a STRUCT argument has the SAME zero-value-skip behavior Create does (any
// field equal to its zero value is omitted from the SQL, regardless of whether it carries a
// `default` tag): `Updates(&SSOProvider{Enabled: false})` would silently generate an UPDATE with
// no SET clause for `enabled` at all, making "disable" a permanent no-op. `.Update("enabled", v)`
// (single column, explicit value) has no such ambiguity — it always writes exactly what was
// asked. Returns ErrSSOProviderNotFound when id does not exist in this tenant (mirrors Delete).
func (r *SSOProviderRepository) SetEnabled(ctx context.Context, tenantID, id string, enabled bool) error {
	res := r.db.WithContext(ctx).Model(&SSOProvider{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Update("enabled", enabled)
	if res.Error != nil {
		return fmt.Errorf("auth.SSOProviderRepository.SetEnabled: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrSSOProviderNotFound
	}
	return nil
}

// SetTrustIdpMfa flips ONLY the `trust_idp_mfa` column for a tenant-scoped provider row
// (SSO-MFA-POLICY, migration 000040 — the per-provider "trust the IdP's own MFA, skip local"
// opt-in). Deliberately a single-column `.Update("trust_idp_mfa", ...)` for the exact same
// reason SetEnabled is (see its doc): a struct-based `Updates(&SSOProvider{TrustIdpMfa: false})`
// would hit GORM's zero-value-skip and make "turn trust back OFF" a silent no-op — on THIS flag
// that failure mode is a security bug (a provider an admin believes is back on enforce-local-MFA
// would keep skipping it). Returns ErrSSOProviderNotFound when id does not exist in this tenant.
func (r *SSOProviderRepository) SetTrustIdpMfa(ctx context.Context, tenantID, id string, trust bool) error {
	res := r.db.WithContext(ctx).Model(&SSOProvider{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Update("trust_idp_mfa", trust)
	if res.Error != nil {
		return fmt.Errorf("auth.SSOProviderRepository.SetTrustIdpMfa: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrSSOProviderNotFound
	}
	return nil
}

// Delete hard-deletes the tenant-scoped provider row. A provider is admin configuration, not
// business data, so unlike obj_* records this is a genuine delete (not a soft-delete) — mirrors
// UserMFARepository.Delete. FK ON DELETE CASCADE (migration 000031) removes every sso_identities
// row that pointed at it too: revoking a provider revokes every session-origin tied to it.
// Returns ErrSSOProviderNotFound when id does not exist in this tenant.
func (r *SSOProviderRepository) Delete(ctx context.Context, tenantID, id string) error {
	res := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Delete(&SSOProvider{})
	if res.Error != nil {
		return fmt.Errorf("auth.SSOProviderRepository.Delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrSSOProviderNotFound
	}
	return nil
}

// SSOIdentityRepository persists and resolves sso_identities rows over GORM. Every query is
// tenant-pinned.
type SSOIdentityRepository struct {
	db *gorm.DB
}

// NewSSOIdentityRepository wires an SSOIdentityRepository over an existing GORM connection.
func NewSSOIdentityRepository(db *gorm.DB) *SSOIdentityRepository {
	return &SSOIdentityRepository{db: db}
}

// FindBySubject resolves the identity row for (tenant, provider, subject) — the callback
// resolution's primary lookup. Returns ErrSSOIdentityNotFound when no row matches (first login
// from this subject under this provider; the caller falls through to deny-link/JIT).
func (r *SSOIdentityRepository) FindBySubject(ctx context.Context, tenantID, providerID, subject string) (*SSOIdentity, error) {
	var id SSOIdentity
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND provider_id = ? AND subject = ?", tenantID, providerID, subject).
		First(&id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSSOIdentityNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.SSOIdentityRepository.FindBySubject: %w", err)
	}
	return &id, nil
}

// CreateUserWithIdentity atomically inserts a brand-new local user AND its linking identity row
// in ONE database transaction (JIT provisioning). This is deliberately NOT two independent
// Create calls: if the user insert committed but the identity insert failed afterward (a
// transient DB error, a CSPRNG failure surfacing between the two, etc.), the next login attempt
// would find no identity, see the email now matches an existing local user, and hit the
// deny-link safe-default — permanently bricking the account with no way back except manual admin
// intervention. Wrapping both writes in one transaction means a failed identity insert rolls the
// user insert back too, so the caller can simply retry (mirrors
// UserMFARepository.ActivateWithStepAndBackupCodes's same-class fix, ENG-AUTH2 #7 reviewer
// LOW-1). A duplicate email (tenant_id, email) surfaces as ErrUserAlreadyExists (a race against
// a concurrent local signup/link); a duplicate (tenant_id, provider_id, subject) surfaces as
// ErrSSOIdentityAlreadyExists-shaped duplicate error via isDuplicateEntry — folded into the
// generic wrap since the caller (SSOService) treats any race here as "deny, retry the flow".
func (r *SSOIdentityRepository) CreateUserWithIdentity(ctx context.Context, u *User, identity *SSOIdentity) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(u).Error; err != nil {
			if isDuplicateEntry(err) {
				return ErrUserAlreadyExists
			}
			return fmt.Errorf("create user: %w", err)
		}
		if err := tx.Create(identity).Error; err != nil {
			return fmt.Errorf("create identity: %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrUserAlreadyExists) {
			return ErrUserAlreadyExists
		}
		return fmt.Errorf("auth.SSOIdentityRepository.CreateUserWithIdentity: %w", err)
	}
	return nil
}

// ErrSSOHandoffInvalid is the single opaque failure for an SSO handoff code that is unknown,
// already consumed, or expired (SSO-FE-REDIRECT). Folded into one error like ErrMFAPendingInvalid
// so a probed code reveals nothing about why it failed.
var ErrSSOHandoffInvalid = errors.New("auth: sso handoff token invalid")

// SSOHandoffTokenRepository persists and consumes sso_handoff_tokens rows over GORM
// (SSO-FE-REDIRECT). FindByHash is tenant-agnostic (mirrors MFAPendingTokenRepository): the
// /exchange XHR carries only {code}, no tenant, and UNIQUE(token_hash) makes the lookup
// unambiguous; the caller derives tenant_id FROM the returned row and passes it into every
// subsequent tenant-pinned call.
type SSOHandoffTokenRepository struct {
	db  *gorm.DB
	now func() time.Time
}

// NewSSOHandoffTokenRepository wires an SSOHandoffTokenRepository over an existing GORM connection.
func NewSSOHandoffTokenRepository(db *gorm.DB) *SSOHandoffTokenRepository {
	return &SSOHandoffTokenRepository{db: db, now: time.Now}
}

// Store inserts a new handoff-token record. The caller passes the already-hashed code; the raw
// code is handed to the client (via the redirect URL) and never written to the database.
func (r *SSOHandoffTokenRepository) Store(ctx context.Context, t *SSOHandoffToken) error {
	if err := r.db.WithContext(ctx).Create(t).Error; err != nil {
		return fmt.Errorf("auth.SSOHandoffTokenRepository.Store: %w", err)
	}
	return nil
}

// FindByHash returns the handoff-token row for a code hash regardless of tenant (see type doc).
// It does not itself filter used_at/expires_at — the caller relies on the atomic ConsumeByHash to
// enforce single-use + expiry. Returns ErrSSOHandoffInvalid when no row matches the hash at all.
func (r *SSOHandoffTokenRepository) FindByHash(ctx context.Context, tokenHash string) (*SSOHandoffToken, error) {
	var t SSOHandoffToken
	err := r.db.WithContext(ctx).Where("token_hash = ?", tokenHash).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSSOHandoffInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("auth.SSOHandoffTokenRepository.FindByHash: %w", err)
	}
	return &t, nil
}

// ConsumeByHash atomically claims a live handoff code: a single conditional UPDATE ... WHERE
// tenant_id = ? AND token_hash = ? AND used_at IS NULL AND expires_at > ?, reporting whether THIS
// call affected a row (single-winner under concurrent double-submit). tenantID MUST come from the
// row already resolved by FindByHash, never from client input — mirrors
// MFAPendingTokenRepository.ConsumeByHash exactly.
func (r *SSOHandoffTokenRepository) ConsumeByHash(ctx context.Context, tenantID, tokenHash string) (bool, error) {
	res := r.db.WithContext(ctx).
		Model(&SSOHandoffToken{}).
		Where("tenant_id = ? AND token_hash = ? AND used_at IS NULL AND expires_at > ?",
			tenantID, tokenHash, r.now()).
		Update("used_at", r.now())
	if res.Error != nil {
		return false, fmt.Errorf("auth.SSOHandoffTokenRepository.ConsumeByHash: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// DeleteExpired hard-deletes handoff rows past their expiry — the janitor purge path. Consumed
// rows (used_at set) are left until they also expire so the same code can never be replayed even
// within its TTL window; expiry is the single cleanup axis.
func (r *SSOHandoffTokenRepository) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Where("expires_at <= ?", before).Delete(&SSOHandoffToken{})
	if res.Error != nil {
		return 0, fmt.Errorf("auth.SSOHandoffTokenRepository.DeleteExpired: %w", res.Error)
	}
	return res.RowsAffected, nil
}
