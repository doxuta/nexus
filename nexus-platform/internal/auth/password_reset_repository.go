package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrResetTokenInvalid is returned when no live password-reset-token row matches a presented
// token hash (unknown, already consumed, or expired). Callers fold it into a single generic
// rejection so a leaked-but-dead token reveals nothing about why it failed
// (../specs/002-password-reset/spec.md §"#1", mirrors ErrRefreshTokenNotFound / ErrInvalidRefresh).
var ErrResetTokenInvalid = errors.New("auth: password reset token invalid")

// PasswordResetTokenRepository persists and consumes password-reset-token records in the
// fixed password_reset_tokens system table over GORM. Every query is parameterized; all but
// FindByHash are pinned to tenant_id (a token row from tenant A can never be resolved or
// consumed under tenant B).
//
// FindByHash is deliberately tenant-agnostic: the reset-password request body is
// {token, newPassword} with NO tenant (the client cannot know it — the link is emailed).
// The service derives tenant_id FROM the row returned here and then passes it into every
// subsequent tenant-pinned call (ConsumeByHash / UpdatePasswordHash / RevokeAllForUser).
// UNIQUE(token_hash) (migration 000027) is what makes this lookup unambiguous.
type PasswordResetTokenRepository struct {
	db  *gorm.DB
	now func() time.Time // injectable clock; defaults to time.Now
}

// NewPasswordResetTokenRepository wires a password-reset-token repository over an existing
// GORM connection.
func NewPasswordResetTokenRepository(db *gorm.DB) *PasswordResetTokenRepository {
	return &PasswordResetTokenRepository{db: db, now: time.Now}
}

// Store inserts a new password-reset-token record. The caller passes the already-hashed
// token (HashRefreshToken) — the raw token is never handed to the repository, so there is no
// path that could accidentally persist it in the clear.
func (r *PasswordResetTokenRepository) Store(ctx context.Context, prt *PasswordResetToken) error {
	if err := r.db.WithContext(ctx).Create(prt).Error; err != nil {
		return fmt.Errorf("auth.PasswordResetTokenRepository.Store: %w", err)
	}
	return nil
}

// FindByHash returns the password-reset-token row for a token hash REGARDLESS of tenant
// (tenant-agnostic by design — see type doc). It does not filter on used_at/expires_at either:
// the caller (AuthService.ResetPassword) inspects those fields itself and/or relies on the
// atomic ConsumeByHash to enforce single-use + expiry. Returns ErrResetTokenInvalid when no
// row matches the hash at all.
func (r *PasswordResetTokenRepository) FindByHash(ctx context.Context, tokenHash string) (*PasswordResetToken, error) {
	var prt PasswordResetToken
	err := r.db.WithContext(ctx).
		Where("token_hash = ?", tokenHash).
		First(&prt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrResetTokenInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("auth.PasswordResetTokenRepository.FindByHash: %w", err)
	}
	return &prt, nil
}

// ConsumeByHash atomically claims a live token: it flips used_at via a single conditional
// UPDATE ... WHERE tenant_id = ? AND token_hash = ? AND used_at IS NULL AND expires_at > ?
// and reports whether THIS call affected a row. tenantID here MUST come from the row already
// resolved by FindByHash (never from client input) — this is the tenant-pinned write MUST-FIX 1
// requires. The atomic UPDATE is the single-use enforcement point: under a concurrent
// double-submit for the same raw token, only the call whose UPDATE actually flips the row wins;
// every other caller sees affected=0 and must be treated as ErrResetTokenInvalid by the caller.
func (r *PasswordResetTokenRepository) ConsumeByHash(ctx context.Context, tenantID, tokenHash string) (bool, error) {
	res := r.db.WithContext(ctx).
		Model(&PasswordResetToken{}).
		Where("tenant_id = ? AND token_hash = ? AND used_at IS NULL AND expires_at > ?",
			tenantID, tokenHash, r.now()).
		Update("used_at", r.now())
	if res.Error != nil {
		return false, fmt.Errorf("auth.PasswordResetTokenRepository.ConsumeByHash: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}
