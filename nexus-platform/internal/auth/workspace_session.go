package auth

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// ErrWorkspaceSessionNotLive is returned by WorkspaceSessionRepository.GetLive / FindOpen /
// FindResumable when no matching session row stands: never opened, closed, past its hard deadline,
// or its customer tenant is no longer active. The auth stage folds it into the uniform 401.
var ErrWorkspaceSessionNotLive = errors.New("auth: workspace session is not live")

// Close reasons stamped on sys_workspace_sessions.closed_reason.
const WorkspaceClosedByBuilder = "closed"

// WorkspaceSession is one builder workspace session (specs/062-customer-workspace, migration
// 000084): a Nexus builder working directly inside a customer's tenant. Fixed system table →
// GORM-managed; every read is pinned to tenant_id (the CUSTOMER — the token's `tid`).
//
// The row is the authority the session token is re-checked against on EVERY request (GetLive, with
// ActorLive for the builder behind it). Expiry and revocation write nothing: ExpiresAt is the hard
// deadline of the row itself, and ending the home login / removing the operator flag are caught by
// ActorLive, not by a column here. Only an explicit "Về Nexus" stamps ClosedAt.
type WorkspaceSession struct {
	ID       string `gorm:"type:char(36);primaryKey"`
	TenantID string `gorm:"type:char(36)"`
	// ActorUserID / ActorTenantID / ActorSessionID name the real builder, their home workshop and
	// the home login session (refresh_tokens.session_id) the workspace session dies with.
	ActorUserID    string `gorm:"type:char(36)"`
	ActorTenantID  string `gorm:"type:char(36)"`
	ActorSessionID string `gorm:"type:char(36)"`
	// ActorLabel is "Full name (Workshop)" stamped at open time — what the customer's audit trail
	// shows. Never an e-mail.
	ActorLabel   string
	ExpiresAt    time.Time
	ClosedAt     *time.Time
	ClosedReason *string `gorm:"type:varchar(16)"`
	CreatedBy    string  `gorm:"type:char(36)"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    gorm.DeletedAt
}

// TableName pins the fixed system table.
func (WorkspaceSession) TableName() string { return "sys_workspace_sessions" }

// WorkspaceSessionLive is the row GetLive confirmed, joined with the customer tenant's slug and
// name (what /auth/me and the banner show).
type WorkspaceSessionLive struct {
	ID             string
	TenantID       string
	ActorUserID    string
	ActorTenantID  string
	ActorSessionID string
	ActorLabel     string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	TenantSlug     string
	TenantName     string
}

// HasDelegationClaims reports whether the token carries ANY of act/att/wsid. Presence of one routes
// it to the workspace resolver, which refuses a partial set — a token cannot dodge the resolver by
// omitting a claim. (A preview token also carries act/att, but it is a different token type and is
// handled before this is asked.)
func (s Subject) HasDelegationClaims() bool {
	return s.ActorUserID != "" || s.ActorTenantID != "" || s.WorkspaceSessionID != ""
}

// DelegationShapeOK is the structural half of validating a workspace-session token, shared by the
// Auth stage and GET /auth/me so the two cannot drift: all three claims, a home login session
// (sid), the real builder is the subject (act == uid), and the token's tenant is NOT the builder's
// own (att != tid — a "delegation" into the home tenant is meaningless). The live half is the
// resolver (session row + builder), never the token.
func (s Subject) DelegationShapeOK() bool {
	return s.ActorUserID != "" && s.ActorTenantID != "" && s.WorkspaceSessionID != "" &&
		s.SessionID != "" && s.ActorUserID == s.UserID && s.ActorTenantID != s.TenantID
}

// ResolvedWorkspace is the answer of a successful session resolution: the live session row plus
// the live builder behind it.
type ResolvedWorkspace struct {
	Session WorkspaceSessionLive
	Actor   LiveActor
}
