package auth

import "time"

// PasswordResetToken is a server-side record of an issued password-reset token. Like
// RefreshToken, the raw token is NEVER stored: only its SHA-256 hex digest lives in
// token_hash, so a database leak cannot be replayed to reset a password
// (../specs/002-password-reset/spec.md §"#1" MUST-FIX 2). Each row is a fixed system table →
// GORM-managed (rules/database.md) and always carries tenant_id.
//
// Lifecycle: created by RequestPasswordReset; UsedAt is stamped exactly once by the atomic
// conditional UPDATE in ConsumeByHash (single-use). A token is usable iff UsedAt is nil and
// ExpiresAt is in the future.
type PasswordResetToken struct {
	ID        string     `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID  string     `gorm:"type:char(36);index:idx_prt_tenant" json:"tenantId"`
	UserID    string     `gorm:"type:char(36);index:idx_prt_user" json:"userId"`
	TokenHash string     `gorm:"type:char(64);uniqueIndex:uq_prt_token_hash" json:"-"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// TableName pins the fixed system table.
func (PasswordResetToken) TableName() string { return "password_reset_tokens" }
