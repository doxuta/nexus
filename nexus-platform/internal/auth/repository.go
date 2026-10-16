package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrTenantNotFound is returned when no active tenant matches a login slug.
var ErrTenantNotFound = errors.New("auth: tenant not found")

// ErrUserNotFound is returned when no active user matches a (tenant_id, email) pair.
var ErrUserNotFound = errors.New("auth: user not found")

// tenantStatusActive is the only tenant status that may authenticate / mint tokens. Tenant
// lookups filter on it so suspended/disabled tenants resolve to ErrTenantNotFound everywhere.
const tenantStatusActive = "active"

// UserRepository reads the fixed auth system tables (tenants, users) over GORM.
// All lookups are parameterized (GORM ? placeholders) and the user lookup is always
// scoped by tenant_id — there is no code path that resolves a user across tenants.
type UserRepository struct {
	db *gorm.DB
}

// NewUserRepository wires a user repository over an existing GORM connection.
func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{db: db}
}

// ResolveTenantBySlug maps a login slug to its ACTIVE tenant. Returns ErrTenantNotFound
// when the slug is unknown, the tenant is soft-deleted, or the tenant's status is not
// 'active' (suspended/disabled). Folding the suspended case into the same sentinel lets the
// service return a single generic "invalid credentials" response (spec §4: never reveal
// which part failed) and prevents a suspended-but-existing tenant from being distinguished
// from an unknown one. The status filter is what makes the doc-comment's "active tenant"
// promise actually enforced (GD1 bug #10).
func (r *UserRepository) ResolveTenantBySlug(ctx context.Context, slug string) (*Tenant, error) {
	var t Tenant
	err := r.db.WithContext(ctx).
		Where("slug = ? AND status = ?", slug, tenantStatusActive).
		First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.UserRepository.ResolveTenantBySlug: %w", err)
	}
	return &t, nil
}

// GetTenantByID returns the ACTIVE tenant for an id, or ErrTenantNotFound when the id is
// unknown, the tenant is soft-deleted, or its status is not 'active'. The refresh flow uses
// this to re-check that the token's tenant is still active before minting a new access token,
// so suspending a tenant also cuts off its users' refresh path — not just login (GD1 bug #10).
func (r *UserRepository) GetTenantByID(ctx context.Context, id string) (*Tenant, error) {
	var t Tenant
	err := r.db.WithContext(ctx).
		Where("id = ? AND status = ?", id, tenantStatusActive).
		First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.UserRepository.GetTenantByID: %w", err)
	}
	return &t, nil
}

// UpdateTenantBranding full-replaces the tenant's white-label document (AO-THUONG-HIEU p1).
// nil/empty raw stores SQL NULL (back to default Nexus branding). Scoped by id — the caller
// passes the JWT-claim tenant, so an admin can only ever brand their OWN tenant.
func (r *UserRepository) UpdateTenantBranding(ctx context.Context, tenantID string, raw json.RawMessage) error {
	n, err := SetBrandingExec(ctx, r.db.WithContext(ctx).Statement.ConnPool, tenantID, raw)
	if err != nil {
		return fmt.Errorf("auth.UserRepository.UpdateTenantBranding: %w", err)
	}
	if n == 0 {
		// Same-value update also reports 0 — disambiguate with an existence read.
		if _, err := r.GetTenantByID(ctx, tenantID); err != nil {
			return err
		}
	}
	return nil
}

// Execer is the one capability SetBrandingExec needs (a pool, a tx or a gorm.ConnPool).
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// SetBrandingExec full-replaces the white-label document of an ACTIVE tenant and returns the rows
// changed (0 = unknown / inactive tenant, or a same-value write). The repository calls it on its
// pool; the draft layer's publish (specs/063-draft-publish S7-3) calls the SAME statement on its
// transaction. nil/empty raw stores SQL NULL. Scoped by id only — never by a draft's doc_key.
func SetBrandingExec(ctx context.Context, ex Execer, tenantID string, raw json.RawMessage) (int64, error) {
	var val any
	if len(raw) > 0 {
		val = string(raw)
	}
	res, err := ex.ExecContext(ctx,
		`UPDATE tenants SET branding = ?, updated_at = NOW() WHERE id = ? AND status = ? AND deleted_at IS NULL`,
		val, tenantID, tenantStatusActive)
	if err != nil {
		return 0, fmt.Errorf("auth.SetBrandingExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("auth.SetBrandingExec: rows affected: %w", err)
	}
	return n, nil
}

// UpdateTenantPayment full-replaces the tenant's VietQR bank config (DONG-TIEN-KHEP-KIN p1).
// Mirrors UpdateTenantBranding: nil/empty raw stores SQL NULL; scoped by JWT-claim tenant id.
func (r *UserRepository) UpdateTenantPayment(ctx context.Context, tenantID string, raw json.RawMessage) error {
	var val any
	if len(raw) > 0 {
		val = string(raw)
	}
	res := r.db.WithContext(ctx).Model(&Tenant{}).
		Where("id = ? AND status = ?", tenantID, tenantStatusActive).
		Update("payment", val)
	if res.Error != nil {
		return fmt.Errorf("auth.UserRepository.UpdateTenantPayment: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		if _, err := r.GetTenantByID(ctx, tenantID); err != nil {
			return err
		}
	}
	return nil
}

// GetUserByEmail returns the active user for a (tenant_id, email) pair, or ErrUserNotFound.
// Multi-tenancy: the query is always pinned to tenant_id, so an email belonging to another
// tenant resolves to not-found, never to a cross-tenant row. Soft-deleted users are excluded
// by GORM via the DeletedAt field. Password verification is the service's job, not the repo's.
func (r *UserRepository) GetUserByEmail(ctx context.Context, tenantID, email string) (*User, error) {
	var u User
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND email = ?", tenantID, email).
		First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.UserRepository.GetUserByEmail: %w", err)
	}
	return &u, nil
}

// GetMaintenance reads the write-freeze deadline of one tenant (specs/068 slice B). Raw table read:
// a retired tenant is still a tenant, and the freeze gates must see it. nil until = not frozen.
func (r *UserRepository) GetMaintenance(ctx context.Context, tenantID string) (*time.Time, string, error) {
	var row struct {
		MaintenanceUntil  *time.Time
		MaintenanceReason *string
	}
	res := r.db.WithContext(ctx).Table("tenants").
		Select("maintenance_until, maintenance_reason").Where("id = ?", tenantID).Take(&row)
	if errors.Is(res.Error, gorm.ErrRecordNotFound) {
		return nil, "", ErrTenantNotFound
	}
	if res.Error != nil {
		return nil, "", fmt.Errorf("auth.UserRepository.GetMaintenance: %w", res.Error)
	}
	reason := ""
	if row.MaintenanceReason != nil {
		reason = *row.MaintenanceReason
	}
	return row.MaintenanceUntil, reason, nil
}

// SetMaintenance writes the freeze deadline (UTC) + reason for one tenant.
func (r *UserRepository) SetMaintenance(ctx context.Context, tenantID string, until time.Time, reason string) error {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE tenants SET maintenance_until = ?, maintenance_reason = ? WHERE id = ?", until.UTC(), reason, tenantID)
	if res.Error != nil {
		return fmt.Errorf("auth.UserRepository.SetMaintenance: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrTenantNotFound
	}
	return nil
}

// ClearMaintenance lifts the freeze (NULL deadline + reason). Idempotent: an already-clear tenant is fine.
func (r *UserRepository) ClearMaintenance(ctx context.Context, tenantID string) error {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE tenants SET maintenance_until = NULL, maintenance_reason = NULL WHERE id = ?", tenantID)
	if res.Error != nil {
		return fmt.Errorf("auth.UserRepository.ClearMaintenance: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrTenantNotFound
	}
	return nil
}
