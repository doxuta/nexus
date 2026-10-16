package auth

import (
	"errors"
	"time"
)

// ErrMFANotEnrolled is returned when no user_mfa row exists for (tenant, user) — the caller
// has never called enroll, or a previous enrollment was disabled/deleted.
var ErrMFANotEnrolled = errors.New("auth: mfa not enrolled")

// ErrMFAAlreadyEnabled is returned by Enroll/Activate when the caller's MFA is already active.
var ErrMFAAlreadyEnabled = errors.New("auth: mfa already enabled")

// ErrMFAPendingInvalid is the single, generic failure for the mfa-pending-token bridge: a
// missing, malformed, expired, wrong-tenant, or already-consumed token. Folded into one error
// (mirrors ErrResetTokenInvalid) so a leaked-but-dead pending token reveals nothing about why
// it failed (anti-probe, ../specs/003-mfa-totp/spec.md §"#7").
var ErrMFAPendingInvalid = errors.New("auth: mfa pending token invalid")

// UserMFA is the server-side record of one user's TOTP enrollment. It is a fixed system
// table → GORM-managed (rules/database.md) and always carries tenant_id.
//
// SecretEnc holds the AES-256-GCM SEALED (reversible) TOTP seed — see internal/crypto — never
// the raw base32 secret. Enabled is false while enrolled-but-not-yet-activated (the user has
// scanned a QR code but not yet confirmed a code); it flips to true on activation.
// LastUsedStep is the RFC 6238 time-step of the most recently ACCEPTED code (the replay
// guard) — NOT wall-clock "now" (../specs/003-mfa-totp/spec.md §"#7" MUST-FIX 2).
type UserMFA struct {
	ID           string    `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID     string    `gorm:"type:char(36)" json:"tenantId"`
	UserID       string    `gorm:"type:char(36)" json:"userId"`
	SecretEnc    string    `gorm:"type:text" json:"-"`
	Enabled      bool      `gorm:"type:tinyint(1);default:0" json:"enabled"`
	LastUsedStep int64     `gorm:"type:bigint;default:0" json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// TableName pins the fixed system table.
func (UserMFA) TableName() string { return "user_mfa" }

// MFABackupCode is one single-use recovery code issued at MFA activation time. Like
// RefreshToken/PasswordResetToken, only the SHA-256 hash of the raw code is ever persisted.
type MFABackupCode struct {
	ID        string     `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID  string     `gorm:"type:char(36)" json:"tenantId"`
	UserID    string     `gorm:"type:char(36)" json:"userId"`
	CodeHash  string     `gorm:"type:char(64)" json:"-"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// TableName pins the fixed system table.
func (MFABackupCode) TableName() string { return "mfa_backup_codes" }

// MFAPendingToken is the short-lived, single-use, OPAQUE bridge between "password verified"
// and "MFA code verified" in the two-step login flow (../specs/003-mfa-totp/spec.md §"#7"
// MUST-FIX — deliberately NOT a JWT/refresh token, see migration 000030 header). Only the
// SHA-256 hash of the raw token is persisted, mirroring PasswordResetToken exactly.
type MFAPendingToken struct {
	ID        string     `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID  string     `gorm:"type:char(36)" json:"tenantId"`
	UserID    string     `gorm:"type:char(36)" json:"userId"`
	TokenHash string     `gorm:"type:char(64);uniqueIndex:uq_mfa_pending_token_hash" json:"-"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// TableName pins the fixed system table.
func (MFAPendingToken) TableName() string { return "mfa_pending_tokens" }
