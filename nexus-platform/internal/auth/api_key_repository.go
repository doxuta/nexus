package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrAPIKeyInvalid is the single opaque failure for a presented API key that buys nothing —
// unknown hash, revoked, or expired all fold into it (anti-enumeration, mirroring
// ErrSSOHandoffInvalid): a probed credential must not reveal WHY it failed, and the HTTP layer
// collapses it into the same undifferentiated 401 a bad JWT gets.
var ErrAPIKeyInvalid = errors.New("auth: api key invalid")

// ErrAPIKeyNotFound is the admin-surface miss: GET/DELETE by id found no row in the caller's
// tenant. Distinct from ErrAPIKeyInvalid because the admin CRUD may honestly 404 (the caller is
// an authenticated admin of the tenant — there is nothing to enumerate).
var ErrAPIKeyNotFound = errors.New("auth: api key not found")

// APIKeyRepository persists and resolves sys_api_keys rows over GORM (fixed system table,
// rules/database.md). Every admin-surface query is pinned to tenant_id; FindByHash is the ONE
// deliberate tenant-agnostic lookup (mirrors SSOHandoffTokenRepository.FindByHash): the verify
// path has no tenant context yet — the credential itself IS the identity — and
// UNIQUE(key_hash) makes the point read unambiguous. The caller derives tenant_id FROM the
// returned row; it never comes from client input.
type APIKeyRepository struct {
	db *gorm.DB
}

// NewAPIKeyRepository wires an APIKeyRepository over an existing GORM connection.
func NewAPIKeyRepository(db *gorm.DB) *APIKeyRepository {
	return &APIKeyRepository{db: db}
}

// Create inserts a brand-new API-key row. The caller passes the already-hashed key; the raw
// secret is handed to the admin once and never written to the database.
func (r *APIKeyRepository) Create(ctx context.Context, k *APIKey) error {
	if err := r.db.WithContext(ctx).Create(k).Error; err != nil {
		return fmt.Errorf("auth.APIKeyRepository.Create: %w", err)
	}
	return nil
}

// FindByHash returns the key row for a SHA-256 hash regardless of tenant (see type doc). It
// deliberately does NOT filter revoked_at/expires_at — the verifier checks those in Go so the
// distinct failure reasons can be logged server-side while the client sees one opaque error.
// Returns ErrAPIKeyInvalid when no row matches the hash.
func (r *APIKeyRepository) FindByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	var k APIKey
	err := r.db.WithContext(ctx).Where("key_hash = ?", keyHash).First(&k).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrAPIKeyInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("auth.APIKeyRepository.FindByHash: %w", err)
	}
	return &k, nil
}

// List returns every key of a tenant (active AND revoked — an admin needs revoked rows for
// audit), newest first with id tie-break for a stable order. Always pinned to tenant_id.
func (r *APIKeyRepository) List(ctx context.Context, tenantID string) ([]APIKey, error) {
	rows := make([]APIKey, 0)
	err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("created_at DESC, id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("auth.APIKeyRepository.List: %w", err)
	}
	return rows, nil
}

// GetByID returns the tenant-scoped key row for id (revoked included — revoke must be able to
// report already-revoked idempotently). Unknown or cross-tenant id → ErrAPIKeyNotFound.
func (r *APIKeyRepository) GetByID(ctx context.Context, tenantID, id string) (*APIKey, error) {
	var k APIKey
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&k).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrAPIKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.APIKeyRepository.GetByID: %w", err)
	}
	return &k, nil
}

// Revoke soft-revokes a live key: a single conditional UPDATE pinned to (tenant_id, id) that
// only fires when revoked_at IS NULL, reporting whether THIS call flipped the row (single
// winner under concurrent double-submit — mirrors ConsumeByHash). The row is kept for audit.
func (r *APIKeyRepository) Revoke(ctx context.Context, tenantID, id string, at time.Time) (bool, error) {
	res := r.db.WithContext(ctx).
		Model(&APIKey{}).
		Where("tenant_id = ? AND id = ? AND revoked_at IS NULL", tenantID, id).
		Update("revoked_at", at)
	if res.Error != nil {
		return false, fmt.Errorf("auth.APIKeyRepository.Revoke: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// TouchLastUsed records a successful verify. tenantID comes from the row FindByHash already
// resolved (defense-in-depth pin, never client input). Called by the service's throttled
// write-behind — NOT per request — so this is a rare, tiny point UPDATE.
func (r *APIKeyRepository) TouchLastUsed(ctx context.Context, tenantID, id string, at time.Time) error {
	err := r.db.WithContext(ctx).
		Model(&APIKey{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Update("last_used_at", at).Error
	if err != nil {
		return fmt.Errorf("auth.APIKeyRepository.TouchLastUsed: %w", err)
	}
	return nil
}
