package auth

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// ErrPreviewSessionNotFound is returned when no (tenant, id) preview-session row exists. The
// verifier folds it into the uniform 401 of the auth stage.
var ErrPreviewSessionNotFound = errors.New("auth: preview session not found")

// PreviewSession is one "preview as role" session (specs/064-preview-as-role-sample-data S9-1,
// migration 000079): a builder looking at the customer app exactly as a member of RoleID sees it.
// Fixed system table → GORM-managed; every read is pinned to (tenant_id, id).
//
// The row is the durable trail of the session and the authority the preview token is re-checked
// against on EVERY request (services.PreviewService.Verify): EndedAt / ExpiresAt / the claim
// cross-checks, then the builder's own login session ActorSessionID through
// WorkspaceSessionRepository.ActorLive. Expiry writes nothing — the row states its own deadline.
type PreviewSession struct {
	ID       string `gorm:"type:char(36);primaryKey"`
	TenantID string `gorm:"type:char(36)"`
	RoleID   string `gorm:"type:char(36)"`
	// PrincipalID is the synthetic subject the token acts as: UUIDv5(ns, tenant|role), no users row.
	PrincipalID string `gorm:"type:char(36)"`
	// ActorUserID / ActorTenantID / ActorSessionID name the real builder, their home tenant and the
	// login session (refresh_tokens.session_id) the preview dies with.
	ActorUserID    string `gorm:"type:char(36)"`
	ActorTenantID  string `gorm:"type:char(36)"`
	ActorSessionID string `gorm:"type:char(36)"`
	// WorkspaceSessionID is the builder's workspace session (sys_workspace_sessions.id) when the preview
	// was opened INSIDE a customer's workspace (S9-8; then ActorTenantID ≠ TenantID); NULL for a preview
	// from the builder's own workshop. Never a claim of the preview token — only this column.
	WorkspaceSessionID *string `gorm:"type:char(36)"`
	AppID              *string `gorm:"type:char(36)"`
	// Draft: this session reads the builder's draft layer (S9-9); mirrored in the token's pdr claim.
	Draft     bool
	ExpiresAt time.Time
	EndedAt   *time.Time
	EndReason *string `gorm:"type:varchar(16)"`
	CreatedBy string  `gorm:"type:char(36)"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt gorm.DeletedAt
}

// TableName pins the fixed system table.
func (PreviewSession) TableName() string { return "sys_preview_sessions" }
