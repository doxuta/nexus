package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrActorNotLive is returned by ActorLive when the builder behind a delegated token is no longer
// allowed to act: not active, not admin, not platform operator, deleted, home tenant not active,
// or the login session has no live refresh token left. Callers answer with the uniform 401/403.
var ErrActorNotLive = errors.New("auth: builder is not live")

// LiveActor is the builder row ActorLive confirmed, with the home tenant's name (label material
// for the audit trail of specs/062-customer-workspace).
type LiveActor struct {
	ID         string
	Email      string
	FullName   string
	TenantName string
}

// WorkspaceSessionRepository is the repository of builder sessions of
// specs/062-customer-workspace (plan §2 / §M1). S9-1 of specs/064-preview-as-role-sample-data
// landed BEFORE that slice's L1a, so — as specs/062 plan "Hợp đồng" rules for a reversed order —
// it lands exactly the shared method ActorLive here, with the agreed signature and SQL (no second
// file, no second query). L1a adds Create / FindOpen / FindResumable / GetLive / Close to this
// same type and reuses ActorLive for its resolver.
type WorkspaceSessionRepository struct {
	db *gorm.DB
}

// NewWorkspaceSessionRepository wires the repository over an existing GORM connection.
func NewWorkspaceSessionRepository(db *gorm.DB) *WorkspaceSessionRepository {
	return &WorkspaceSessionRepository{db: db}
}

// ActorLive answers "may this builder still act, right now?" for a delegated token — ONE
// parameterized statement, never cached, so every revocation lands on the very next request:
//
//   - the user row (att, act) is active, admin, platform operator and not deleted — a demotion,
//     lock, delete, or the operator flag removed by CLI/SQL all count;
//   - its home tenant is active and not deleted;
//   - the login session sid still has a refresh token that is NOT revoked and NOT expired. This is
//     "the session is alive", not "the session was not ended": logout, ending the session,
//     logout-all, a password change / admin password set / reset and the E5 reuse alarm (all
//     revoke_reason='ended' since #310) plus role change, deactivation and user delete
//     (RevokeAllForUser, revoke_reason NULL) all revoke refresh rows, and every one of them is
//     caught — a check on revoke_reason='ended' alone misses the RevokeAllForUser paths.
//
// sid is ALWAYS the builder's home login session: the `sid` claim of a typ=access token, or the
// sys_preview_sessions.actor_session_id column for a preview token (whose own `sid` claim is the
// preview-session id). ErrActorNotLive when no row matches; a DB error is returned wrapped (the
// caller fails closed).
func (r *WorkspaceSessionRepository) ActorLive(ctx context.Context, att, act, sid string, now time.Time) (*LiveActor, error) {
	var rows []LiveActor
	err := r.db.WithContext(ctx).Raw(`
SELECT u.id AS id, u.email AS email, u.full_name AS full_name, t.name AS tenant_name
FROM users u
JOIN tenants t ON t.id = u.tenant_id AND t.status = 'active' AND t.deleted_at IS NULL
WHERE u.tenant_id = ? AND u.id = ? AND u.status = 'active' AND u.is_admin = 1
  AND u.is_platform_operator = 1 AND u.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM refresh_tokens rt
              WHERE rt.tenant_id = ? AND rt.user_id = ? AND rt.session_id = ?
                AND rt.revoked_at IS NULL AND rt.expires_at > ?)
LIMIT 1`, att, act, att, act, sid, now).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("auth.WorkspaceSessionRepository.ActorLive: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrActorNotLive
	}
	return &rows[0], nil
}

// Create inserts a new session row (synchronously, before the token is minted).
func (r *WorkspaceSessionRepository) Create(ctx context.Context, s *WorkspaceSession) error {
	if err := r.db.WithContext(ctx).Create(s).Error; err != nil {
		return fmt.Errorf("auth.WorkspaceSessionRepository.Create: %w", err)
	}
	return nil
}

// openScope is the shared "still open" predicate of FindOpen / FindResumable: pinned to the customer
// tenant and to the builder's identity + home login session, not closed, not past the hard deadline.
func (r *WorkspaceSessionRepository) openScope(ctx context.Context, tenantID, act, att, sid string, now time.Time) *gorm.DB {
	return r.db.WithContext(ctx).
		Where("tenant_id = ? AND actor_user_id = ? AND actor_tenant_id = ? AND actor_session_id = ?"+
			" AND closed_at IS NULL AND expires_at > ?", tenantID, act, att, sid, now)
}

// FindOpen returns the newest still-open session of this builder login in the customer tenant, so
// a second "open" reuses it. ErrWorkspaceSessionNotLive when there is none.
func (r *WorkspaceSessionRepository) FindOpen(ctx context.Context, tenantID, act, att, sid string, now time.Time) (*WorkspaceSession, error) {
	var s WorkspaceSession
	err := r.openScope(ctx, tenantID, act, att, sid, now).Order("created_at DESC").First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrWorkspaceSessionNotLive
	}
	if err != nil {
		return nil, fmt.Errorf("auth.WorkspaceSessionRepository.FindOpen: %w", err)
	}
	return &s, nil
}

// FindResumable returns the session id if it is still open for this builder login — never a closed
// or expired one, never another builder's or another login's. ErrWorkspaceSessionNotLive otherwise.
func (r *WorkspaceSessionRepository) FindResumable(ctx context.Context, tenantID, id, act, att, sid string, now time.Time) (*WorkspaceSession, error) {
	var s WorkspaceSession
	err := r.openScope(ctx, tenantID, act, att, sid, now).Where("id = ?", id).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrWorkspaceSessionNotLive
	}
	if err != nil {
		return nil, fmt.Errorf("auth.WorkspaceSessionRepository.FindResumable: %w", err)
	}
	return &s, nil
}

// GetLive is the per-request half of the session check: ONE parameterized statement, never cached.
// The row must match every claim of the token (customer tenant, wsid, builder, home tenant, home
// login session), be open and inside its hard deadline, and the customer tenant must still be
// active and not deleted — suspending a customer kills its sessions on the very next request.
// ErrWorkspaceSessionNotLive when no row matches; a DB error is returned wrapped (callers fail closed).
func (r *WorkspaceSessionRepository) GetLive(ctx context.Context, tenantID, id, act, att, sid string, now time.Time) (*WorkspaceSessionLive, error) {
	var rows []WorkspaceSessionLive
	err := r.db.WithContext(ctx).Raw(`
SELECT ws.id AS id, ws.tenant_id AS tenant_id, ws.actor_user_id AS actor_user_id,
       ws.actor_tenant_id AS actor_tenant_id, ws.actor_session_id AS actor_session_id,
       ws.actor_label AS actor_label, ws.expires_at AS expires_at, ws.created_at AS created_at,
       t.slug AS tenant_slug, t.name AS tenant_name
FROM sys_workspace_sessions ws
JOIN tenants t ON t.id = ws.tenant_id AND t.status = 'active' AND t.deleted_at IS NULL
WHERE ws.tenant_id = ? AND ws.id = ? AND ws.actor_user_id = ? AND ws.actor_tenant_id = ?
  AND ws.actor_session_id = ? AND ws.closed_at IS NULL AND ws.expires_at > ? AND ws.deleted_at IS NULL
LIMIT 1`, tenantID, id, act, att, sid, now).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("auth.WorkspaceSessionRepository.GetLive: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrWorkspaceSessionNotLive
	}
	return &rows[0], nil
}

// Close stamps closed_at / closed_reason on a session that is not closed yet and reports whether
// THIS call closed it. The WHERE excludes the state the SET writes (closed_at IS NULL), so with
// clientFoundRows on, a second concurrent close matches nothing and reports false — only the winner
// writes the audit rows.
func (r *WorkspaceSessionRepository) Close(ctx context.Context, tenantID, id, act, reason string, now time.Time) (bool, error) {
	res := r.db.WithContext(ctx).
		Model(&WorkspaceSession{}).
		Where("tenant_id = ? AND id = ? AND actor_user_id = ? AND closed_at IS NULL", tenantID, id, act).
		Updates(map[string]any{"closed_at": now, "closed_reason": reason})
	if res.Error != nil {
		return false, fmt.Errorf("auth.WorkspaceSessionRepository.Close: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}
