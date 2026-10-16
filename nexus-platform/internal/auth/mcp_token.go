package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// MCPToken is one scoped MCP credential (sys_mcp_tokens, migration 000098 — issue #3): it lets an
// outside agent call /mcp as the builder who issued it, and proposals land in SessionID, that
// builder's assistant thread. Only the SHA-256 of the raw `nxm_…` token is stored (TokenHash, never
// serialized); TokenPrefix is the public slice the Studio list shows.
type MCPToken struct {
	ID          string     `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID    string     `gorm:"type:char(36)" json:"-"`
	UserID      string     `gorm:"type:char(36)" json:"-"`
	SessionID   string     `gorm:"type:char(36)" json:"sessionId"`
	TokenPrefix string     `gorm:"type:varchar(16)" json:"tokenPrefix"`
	TokenHash   string     `gorm:"type:char(64);uniqueIndex:uq_sys_mcp_tokens_hash" json:"-"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// TableName pins the fixed system table.
func (MCPToken) TableName() string { return "sys_mcp_tokens" }

// ErrMCPTokenInvalid is the one opaque failure of a presented MCP token: unknown, revoked, expired,
// or issued by someone who is no longer a builder all fold into it, and /mcp answers one 401.
var ErrMCPTokenInvalid = errors.New("auth: mcp token invalid")

// ErrMCPTokenNotFound is the Studio's miss: revoke by an id that is not one of the caller's tokens.
var ErrMCPTokenNotFound = errors.New("auth: mcp token not found")

// MCPTokenRepository persists sys_mcp_tokens over GORM. The Studio queries are pinned to
// (tenant_id, user_id): a builder sees and revokes only the tokens they issued. FindByHash is the one
// lookup without a tenant (the credential IS the identity, as APIKeyRepository.FindByHash); the
// caller takes the tenant from the row it returns.
type MCPTokenRepository struct {
	db *gorm.DB
}

// NewMCPTokenRepository wires the repository over an existing GORM connection.
func NewMCPTokenRepository(db *gorm.DB) *MCPTokenRepository {
	return &MCPTokenRepository{db: db}
}

// Create inserts a new token row (already hashed: the raw token is never written).
func (r *MCPTokenRepository) Create(ctx context.Context, t *MCPToken) error {
	if err := r.db.WithContext(ctx).Create(t).Error; err != nil {
		return fmt.Errorf("auth.MCPTokenRepository.Create: %w", err)
	}
	return nil
}

// FindByHash returns the row of a token hash in any tenant, revoked and expired rows included (the
// service checks those, so the log can say why); ErrMCPTokenInvalid when nothing matches.
func (r *MCPTokenRepository) FindByHash(ctx context.Context, hash string) (*MCPToken, error) {
	var t MCPToken
	err := r.db.WithContext(ctx).Where("token_hash = ?", hash).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrMCPTokenInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("auth.MCPTokenRepository.FindByHash: %w", err)
	}
	return &t, nil
}

// ListLive returns the caller's tokens that still work at `now` (not revoked, not expired), newest
// first.
func (r *MCPTokenRepository) ListLive(ctx context.Context, tenantID, userID string, now time.Time) ([]MCPToken, error) {
	rows := make([]MCPToken, 0)
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ? AND revoked_at IS NULL AND expires_at > ?", tenantID, userID, now).
		Order("created_at DESC, id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("auth.MCPTokenRepository.ListLive: %w", err)
	}
	return rows, nil
}

// Revoke soft-revokes one of the caller's tokens; a second revoke keeps the first time. The DSN's
// clientFoundRows makes RowsAffected count MATCHED rows, so 0 means the id is not the caller's:
// ErrMCPTokenNotFound.
func (r *MCPTokenRepository) Revoke(ctx context.Context, tenantID, userID, id string, at time.Time) error {
	res := r.db.WithContext(ctx).Model(&MCPToken{}).
		Where("tenant_id = ? AND user_id = ? AND id = ?", tenantID, userID, id).
		Update("revoked_at", gorm.Expr("COALESCE(revoked_at, ?)", at))
	if res.Error != nil {
		return fmt.Errorf("auth.MCPTokenRepository.Revoke: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrMCPTokenNotFound
	}
	return nil
}

// Touch records a verified request. tenantID comes from the row FindByHash returned.
func (r *MCPTokenRepository) Touch(ctx context.Context, tenantID, id string, at time.Time) error {
	err := r.db.WithContext(ctx).Model(&MCPToken{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Update("last_used_at", at).Error
	if err != nil {
		return fmt.Errorf("auth.MCPTokenRepository.Touch: %w", err)
	}
	return nil
}
