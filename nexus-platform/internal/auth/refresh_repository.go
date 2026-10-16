package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// mysqlDeadlock is ER_LOCK_DEADLOCK: InnoDB rolled this statement's transaction back as the victim.
const mysqlDeadlock = 1213

// deadlockRetries bounds how often a session write is re-run after being picked as a deadlock victim.
const deadlockRetries = 3

// retryOnDeadlock re-runs fn while it fails with a MySQL deadlock. Rotate (claim, then insert) and the
// session-ending UPDATEs (scan live rows of a user/session) take the same rows and gaps in opposite
// order, so racing each other InnoDB kills one of them; the loser lost nothing (its statement or
// transaction rolled back whole) and simply goes again, now seeing the winner's committed rows.
func retryOnDeadlock(fn func() error) error {
	var err error
	for i := 0; i < deadlockRetries; i++ {
		err = fn()
		var me *mysql.MySQLError
		if err == nil || !errors.As(err, &me) || me.Number != mysqlDeadlock {
			return err
		}
	}
	return err
}

// ErrRefreshTokenNotFound is returned when no live refresh-token row matches a presented
// token hash (unknown, already rotated/revoked, or expired). Callers fold it into a
// generic "invalid refresh" 401 so a leaked-but-dead token reveals nothing.
var ErrRefreshTokenNotFound = errors.New("auth: refresh token not found")

// RefreshTokenRepository persists and revokes refresh-token records in the fixed
// refresh_tokens system table over GORM. Every query is parameterized and pinned to
// tenant_id (a refresh token from tenant A can never be resolved under tenant B).
type RefreshTokenRepository struct {
	db  *gorm.DB
	now func() time.Time // injectable clock; defaults to time.Now
	// afterClaim runs inside Rotate between the claim and the successor insert. Test seam only
	// (nil in production): it lets a test land an EndSession exactly in the old two-statement gap.
	afterClaim func()
}

// NewRefreshTokenRepository wires a refresh-token repository over an existing GORM connection.
func NewRefreshTokenRepository(db *gorm.DB) *RefreshTokenRepository {
	return &RefreshTokenRepository{db: db, now: time.Now}
}

// Store inserts a new refresh-token record. The caller passes the already-hashed token
// (HashRefreshToken) — the raw token is never handed to the repository, so there is no
// path that could accidentally persist it in the clear (spec §5).
func (r *RefreshTokenRepository) Store(ctx context.Context, rt *RefreshToken) error {
	if err := r.db.WithContext(ctx).Create(rt).Error; err != nil {
		return fmt.Errorf("auth.RefreshTokenRepository.Store: %w", err)
	}
	return nil
}

// Rotate swaps a live refresh row for its successor in ONE transaction: the claim (revoke the row
// matching oldHash while still live, tenant-pinned) and the insert of next commit together or not at
// all. It reports false, writing nothing, when the claim matched no live row (the caller lost a
// rotation race, or the session was ended first). The old claim-then-Store pair had two faults, both
// closed here: an EndSession / EndAllSessions / RevokeAllForUser landing between the two statements
// found no live row for the session and missed the successor (now it waits on the claim's row lock,
// then sees the committed successor), and a Store failure left the old row revoked with no successor,
// silently dropping the session (now the claim rolls back with it).
func (r *RefreshTokenRepository) Rotate(ctx context.Context, tenantID, oldHash string, next *RefreshToken) (bool, error) {
	claimed := false
	err := retryOnDeadlock(func() error {
		return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			claimed = false
			res := tx.Model(&RefreshToken{}).
				Where("tenant_id = ? AND token_hash = ? AND revoked_at IS NULL", tenantID, oldHash).
				Update("revoked_at", r.now())
			if res.Error != nil {
				return fmt.Errorf("claim: %w", res.Error)
			}
			if res.RowsAffected == 0 {
				return nil
			}
			claimed = true
			if r.afterClaim != nil {
				r.afterClaim()
			}
			if err := tx.Create(next).Error; err != nil {
				return fmt.Errorf("insert successor: %w", err)
			}
			return nil
		})
	})
	if err != nil {
		return false, fmt.Errorf("auth.RefreshTokenRepository.Rotate: %w", err)
	}
	return claimed, nil
}

// FindLiveByHash returns the refresh-token row for a token hash that is still usable:
// not revoked AND not past its expiry. It is pinned to tenant_id so a token cannot be
// resolved across tenants. Returns ErrRefreshTokenNotFound for unknown/dead/expired rows.
func (r *RefreshTokenRepository) FindLiveByHash(ctx context.Context, tenantID, tokenHash string) (*RefreshToken, error) {
	var rt RefreshToken
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND token_hash = ? AND revoked_at IS NULL AND expires_at > ?",
			tenantID, tokenHash, r.now()).
		First(&rt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRefreshTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.RefreshTokenRepository.FindLiveByHash: %w", err)
	}
	return &rt, nil
}

// RevokeByHash stamps revoked_at on the live row matching the token hash (tenant-scoped)
// and reports whether a row was actually revoked. It is idempotent: revoking an already
// revoked/expired/unknown token affects zero rows and returns (false, nil) — logout of a
// dead token is not an error.
func (r *RefreshTokenRepository) RevokeByHash(ctx context.Context, tenantID, tokenHash string) (bool, error) {
	res := r.db.WithContext(ctx).
		Model(&RefreshToken{}).
		Where("tenant_id = ? AND token_hash = ? AND revoked_at IS NULL", tenantID, tokenHash).
		Update("revoked_at", r.now())
	if res.Error != nil {
		return false, fmt.Errorf("auth.RefreshTokenRepository.RevokeByHash: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// FindByHashIncludingRevoked looks up a refresh-token row by hash regardless of its revocation
// state. It is used exclusively by the E5 reuse-detection path in AuthService.Refresh: when
// FindLiveByHash returns no live row, this method distinguishes "token was rotated/revoked
// (replay of an already-dead token = theft signal)" from "token was never issued or has
// naturally expired (not a theft signal)". The query gates on `revoked_at IS NOT NULL` so
// naturally-expired-but-never-revoked tokens do NOT trigger a theft response — replaying an
// expired but legitimately un-revoked token is NOT a reuse-detection event.
// A row whose SESSION was ended (revoke_reason = 'ended', ../specs/055-session-list-kill/spec.md
// FR-009) is excluded too: the device that was logged out presenting its own token is the expected
// next step of that device, not a theft — treating it as one would kill every OTHER session of the
// user, including the one that did the ending.
// Returns ErrRefreshTokenNotFound when no revoked row matches the (tenant_id, hash) pair.
func (r *RefreshTokenRepository) FindByHashIncludingRevoked(ctx context.Context, tenantID, tokenHash string) (*RefreshToken, error) {
	var rt RefreshToken
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND token_hash = ? AND revoked_at IS NOT NULL AND (revoke_reason IS NULL OR revoke_reason <> ?)",
			tenantID, tokenHash, RevokeReasonEnded).
		First(&rt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRefreshTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.RefreshTokenRepository.FindByHashIncludingRevoked: %w", err)
	}
	return &rt, nil
}

// RevokeAllForUser revokes every live refresh token for a user within a tenant. Used on
// password change (spec §5: "đổi mật khẩu thu hồi hết") and as the hard-logout path.
func (r *RefreshTokenRepository) RevokeAllForUser(ctx context.Context, tenantID, userID string) error {
	err := retryOnDeadlock(func() error {
		return r.db.WithContext(ctx).
			Model(&RefreshToken{}).
			Where("tenant_id = ? AND user_id = ? AND revoked_at IS NULL", tenantID, userID).
			Update("revoked_at", r.now()).Error
	})
	if err != nil {
		return fmt.Errorf("auth.RefreshTokenRepository.RevokeAllForUser: %w", err)
	}
	return nil
}

// ListLiveSessions returns the live (not revoked, not expired) rows of one user in one tenant,
// most recently used first, capped at limit. Rotation swaps the presented row for its
// successor atomically (Rotate), so this is one row per login session (../specs/055-session-list-kill/spec.md). Rides
// idx_rt_user_live (tenant_id, user_id, revoked_at): only live rows are touched, however long the
// user's rotated-away history has grown.
func (r *RefreshTokenRepository) ListLiveSessions(ctx context.Context, tenantID, userID string, limit int) ([]RefreshToken, error) {
	var rows []RefreshToken
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ? AND revoked_at IS NULL AND expires_at > ?", tenantID, userID, r.now()).
		Order("last_used_at DESC").
		Order("id DESC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("auth.RefreshTokenRepository.ListLiveSessions: %w", err)
	}
	return rows, nil
}

// endedUpdate is the SET clause of a session end: revoked like any revoke, plus the reason the
// auth stage and the E5 reuse check read.
func (r *RefreshTokenRepository) endedUpdate() map[string]any {
	return map[string]any{"revoked_at": r.now(), "revoke_reason": RevokeReasonEnded}
}

// EndSession ends one login session: the live row of (tenant, user, session) is revoked with
// revoke_reason = 'ended'. Reports whether a row was ended — false for another user's session,
// another tenant's, an unknown or an already-dead one (the caller folds all of those into one 404).
func (r *RefreshTokenRepository) EndSession(ctx context.Context, tenantID, userID, sessionID string) (bool, error) {
	var affected int64
	err := retryOnDeadlock(func() error {
		res := r.db.WithContext(ctx).
			Model(&RefreshToken{}).
			Where("tenant_id = ? AND user_id = ? AND session_id = ? AND revoked_at IS NULL", tenantID, userID, sessionID).
			Updates(r.endedUpdate())
		affected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return false, fmt.Errorf("auth.RefreshTokenRepository.EndSession: %w", err)
	}
	return affected > 0, nil
}

// EndAllSessions ends every live login session of one user in one tenant — logout-all, a password
// change / reset, the E5 reuse alarm — except exceptSessionID when it is not blank (a user changing
// their own password keeps the device that just proved the old one), and returns the ended session
// ids so the caller can invalidate the auth stage's cache for each. Both statements ride
// idx_rt_user_live.
func (r *RefreshTokenRepository) EndAllSessions(ctx context.Context, tenantID, userID, exceptSessionID string) ([]string, error) {
	// A row without a session id (pre-055 binary) is never "the one to keep": the IS NULL branch is
	// needed because `session_id <> ?` is NULL, not true, for it.
	keep := "1 = 1"
	args := []any{tenantID, userID}
	if exceptSessionID != "" {
		keep = "(session_id IS NULL OR session_id <> ?)"
		args = append(args, exceptSessionID)
	}
	var sids []string
	if err := r.db.WithContext(ctx).
		Model(&RefreshToken{}).
		Where("tenant_id = ? AND user_id = ? AND revoked_at IS NULL AND session_id IS NOT NULL AND "+keep, args...).
		Distinct("session_id").
		Pluck("session_id", &sids).Error; err != nil {
		return nil, fmt.Errorf("auth.RefreshTokenRepository.EndAllSessions: list: %w", err)
	}
	if err := retryOnDeadlock(func() error {
		return r.db.WithContext(ctx).
			Model(&RefreshToken{}).
			Where("tenant_id = ? AND user_id = ? AND revoked_at IS NULL AND "+keep, args...).
			Updates(r.endedUpdate()).Error
	}); err != nil {
		return nil, fmt.Errorf("auth.RefreshTokenRepository.EndAllSessions: %w", err)
	}
	return sids, nil
}

// IsSessionEnded reports whether the login session (tenant, user, session) was ended — the probe
// behind the auth stage's ended-session check (services.CachedSessionGuard). One seek on
// idx_rt_session (tenant_id, session_id, revoke_reason).
func (r *RefreshTokenRepository) IsSessionEnded(ctx context.Context, tenantID, userID, sessionID string) (bool, error) {
	var n int64
	err := r.db.WithContext(ctx).
		Model(&RefreshToken{}).
		Where("tenant_id = ? AND session_id = ? AND revoke_reason = ? AND user_id = ?", tenantID, sessionID, RevokeReasonEnded, userID).
		Count(&n).Error
	if err != nil {
		return false, fmt.Errorf("auth.RefreshTokenRepository.IsSessionEnded: %w", err)
	}
	return n > 0, nil
}

// GetUserByID returns the active user for a (tenant_id, id) pair, or ErrUserNotFound.
// Used by GET /auth/me to load the current user from the verified token subject; the
// query is always pinned to tenant_id so a forged user_id from another tenant misses.
func (r *UserRepository) GetUserByID(ctx context.Context, tenantID, id string) (*User, error) {
	var u User
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.UserRepository.GetUserByID: %w", err)
	}
	return &u, nil
}
