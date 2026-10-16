package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// RefreshToken is a server-side record of an issued refresh token. The raw token is
// NEVER stored: only its SHA-256 hex digest lives in token_hash, so a database leak
// cannot be replayed to mint access tokens (../specs/001-auth-security/spec.md §5). Each row is a
// fixed system table → GORM-managed (rules/database.md) and always carries tenant_id.
//
// Lifecycle: created at login / on each refresh rotation; RevokedAt is stamped when the
// token is rotated away, on logout, or on password change. A token is usable iff it is
// not revoked and not past ExpiresAt.
//
// Session (../specs/055-session-list-kill/spec.md, mig 000076): SessionID names the device chain a
// row belongs to. It is minted once per login (password, MFA step 2, SSO) and copied onto every
// rotated successor, so one browser is one session for its whole life while the token string
// changes on every refresh — and at any moment a chain has exactly one live row. UserAgent / IP
// (already masked, see services.maskIP) / LastUsedAt describe that device for GET /auth/sessions.
type RefreshToken struct {
	ID         string     `gorm:"type:char(36);primaryKey" json:"id"`
	UserID     string     `gorm:"type:char(36);index:idx_rt_user" json:"userId"`
	TenantID   string     `gorm:"type:char(36)" json:"tenantId"`
	SessionID  string     `gorm:"type:char(36)" json:"sessionId"`
	TokenHash  string     `gorm:"type:char(64);index:idx_rt_hash" json:"-"`
	UserAgent  string     `gorm:"type:varchar(255)" json:"userAgent,omitempty"`
	IP         string     `gorm:"type:varchar(64)" json:"ip,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	// RevokeReason is NULL for an ordinary revoke (rotation, logout, W1, password change) and
	// RevokeReasonEnded when the SESSION was ended (DELETE /auth/sessions/:id, the admin twin,
	// logout-all): the auth stage then refuses the session's live access tokens, and the E5
	// reuse alarm ignores a replay of that token (the ended device retrying is not a thief).
	RevokeReason *string   `gorm:"type:varchar(16)" json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
}

// RevokeReasonEnded marks the rows of a session that was deliberately ended (see RevokeReason).
const RevokeReasonEnded = "ended"

// TableName pins the fixed system table.
func (RefreshToken) TableName() string { return "refresh_tokens" }

// HashRefreshToken returns the lowercase hex SHA-256 digest of a raw refresh token.
// This is the only form persisted (token_hash CHAR(64)); the raw token is handed to
// the client and never written to the database (spec §5: "store as hash, not raw").
func HashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
