package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/notify"
)

// RefreshOutput is the result of a successful token refresh: a fresh access token plus a
// freshly rotated refresh token (the presented one is revoked, spec §5 "rotate mỗi lần dùng").
type RefreshOutput struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// Refresh exchanges a valid refresh token for a new access token and a rotated refresh token.
//
// Flow (spec §5): verify the JWT (signature, HS256, not expired, type=refresh) → look up the
// token's hash among the LIVE rows for its tenant (must exist, not revoked, not expired) →
// verify the owning tenant is still active → mint a new pair and rotate: auth.RefreshTokenRepository
// .Rotate claims (revokes) the presented row and stores the successor in ONE transaction. Every
// failure collapses to ErrInvalidRefresh so a leaked-but-dead token cannot be probed. Single-use is
// enforced atomically: the conditional UPDATE inside Rotate flips exactly one live row, so under
// concurrent replay only the winner proceeds; every loser gets ErrInvalidRefresh.
func (s *AuthService) Refresh(ctx context.Context, rawRefresh string) (*RefreshOutput, error) {
	raw := strings.TrimSpace(rawRefresh)
	if raw == "" {
		return nil, ErrInvalidRefresh
	}

	// 1. Cryptographic + structural validity (also pins token type = refresh).
	claims, err := s.tokens.VerifyRefreshToken(raw)
	if err != nil {
		return nil, ErrInvalidRefresh
	}

	// 2. Server-side liveness: the hash must match a non-revoked, non-expired row for this
	//    tenant. This is what makes refresh tokens revocable (logout / password change) and
	//    single-use (a rotated token's row is revoked, so its replay misses here).
	tokenHash := auth.HashRefreshToken(raw)
	rt, err := s.refresh.FindLiveByHash(ctx, claims.TenantID, tokenHash)
	if err != nil {
		if errors.Is(err, auth.ErrRefreshTokenNotFound) {
			// E5 reuse-detection (spec §2 / ../specs/014-security-alerts/spec.md §2):
			// distinguish "replay of an already-revoked token" (theft signal) from
			// "token was never issued or has naturally expired" (not a theft signal).
			//
			// We query for a row that EXISTS and has revoked_at IS NOT NULL. A row that
			// is merely expired but was never explicitly revoked does NOT appear here
			// (FindByHashIncludingRevoked gates on revoked_at IS NOT NULL), so a client
			// retrying an expired-but-legitimately-unrevoked token is NOT flagged as theft.
			//
			// RFC 6819 / Google-GitHub behaviour: a client that retries the very same
			// now-dead (revoked) token IS treated as reuse. This is correct — once a token
			// is rotated its row is revoked, so any second presentation of that token is
			// indistinguishable from a stolen-token replay. The family kill is the safe
			// default; the legitimate client will simply log in again.
			dead, findErr := s.refresh.FindByHashIncludingRevoked(ctx, claims.TenantID, tokenHash)
			if findErr == nil && dead != nil {
				// Theft signal confirmed: kill the entire token family for this user so
				// the attacker's live tokens (if any) also die. endAllSessions is the
				// remediation (#310: the sessions are ENDED, so the attacker's still-valid access
				// token is refused at once instead of living ≤ 15 min); a failure here is a real
				// error (we must not silently skip it).
				if revokeErr := s.endAllSessions(ctx, claims.TenantID, dead.UserID, ""); revokeErr != nil {
					return nil, fmt.Errorf("services.AuthService.Refresh: reuse-detection family revoke: %w", revokeErr)
				}
				// Load the user to obtain their email for the alert snapshot. Failure here is
				// best-effort: the family is already revoked, so we log and skip the email.
				if alertUser, loadErr := s.users.GetUserByID(ctx, claims.TenantID, dead.UserID); loadErr == nil {
					s.enqueueAlert(ctx, notify.SecurityEvent{
						Type:     notify.AlertRefreshReuse,
						TenantID: claims.TenantID,
						UserID:   dead.UserID,
						Email:    alertUser.Email,
						At:       time.Now().UTC(),
					})
				} else {
					s.logger.Warn("services.AuthService.Refresh: could not load user for reuse alert, skipping email",
						zap.String("tenant_id", claims.TenantID),
						zap.String("user_id", dead.UserID),
					)
				}
			}
			// Always return the generic error — no enumeration, no response change.
			return nil, ErrInvalidRefresh
		}
		return nil, fmt.Errorf("services.AuthService.Refresh: find live token: %w", err)
	}

	// 2b. Tenant-active gate: the refresh path never touches the tenant row otherwise, so a
	//     tenant suspended AFTER tokens were issued could keep refreshing indefinitely. Re-load
	//     the tenant (active-only) and reject if it is gone/suspended. GetTenantByID folds
	//     unknown/soft-deleted/non-active into ErrTenantNotFound → collapse to ErrInvalidRefresh
	//     so the dead-token response stays generic (GD1 bug #10).
	if _, err := s.users.GetTenantByID(ctx, rt.TenantID); err != nil {
		if errors.Is(err, auth.ErrTenantNotFound) {
			return nil, ErrInvalidRefresh
		}
		return nil, fmt.Errorf("services.AuthService.Refresh: load tenant: %w", err)
	}

	// 2c. User-freshness gate (defense-in-depth): reload the user row so the rotated token
	//     reflects the CURRENT account state rather than the claims frozen into the presented
	//     JWT. Two reasons: (1) a user disabled (status != "active") after the token was issued
	//     must not be able to keep refreshing — fail-closed, mirroring Login's status gate
	//     (auth_service.go:173); (2) the new Subject's RoleID/IsAdmin are taken from the fresh
	//     DB row, not the stale claims, so a privilege change is reflected immediately on the
	//     next refresh. GetUserByID is tenant-scoped (folds unknown/cross-tenant/soft-deleted
	//     into ErrUserNotFound → ErrInvalidRefresh) so this read cannot leak across tenants.
	//     NOTE: this gate does NOT close the password-change window (a password reset leaves
	//     status="active") — that is cut at the source in UserService.UpdateUser (§5 revoke);
	//     these are two independent fixes.
	u, err := s.users.GetUserByID(ctx, rt.TenantID, rt.UserID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, ErrInvalidRefresh
		}
		return nil, fmt.Errorf("services.AuthService.Refresh: load user: %w", err)
	}
	if u.Status != statusActive {
		return nil, ErrInvalidRefresh
	}

	// 3. Rotate happens inside issueTokenPair (step 4): AuthRepo.Rotate claims the presented row
	//    (UPDATE ... SET revoked_at WHERE ... revoked_at IS NULL) and inserts its successor in ONE
	//    transaction. Under concurrent replay of the same raw token both requests pass step 2 (the
	//    row is still live) but only one claim flips a row; every loser gets ErrInvalidRefresh, so a
	//    single-use token cannot mint two live families (GD1 bug #2 / spec §5). We deliberately do
	//    NOT nuke the whole family on a lost race: a benign client double-submit must not log the
	//    user out. One transaction also means (a) EndSession / EndAllSessions / RevokeAllForUser can
	//    no longer land between the claim and the insert and miss the successor, and (b) a failing
	//    insert rolls the claim back instead of dropping the session (specs/055 #312).

	// 4. Mint a new pair. The subject is rebuilt from the freshly-loaded user row (step 2c),
	//    not the client's claims, so a tampered body cannot escalate tenant/role/admin AND a
	//    role/is_admin change since the token was issued is picked up on this refresh.
	sub := auth.Subject{
		UserID:   u.ID,
		TenantID: u.TenantID,
		IsAdmin:  u.IsAdmin,
	}
	if u.RoleID != nil {
		sub.RoleID = *u.RoleID
	}
	// rt continues its login session (specs/055): same session id in the new tokens' `sid`.
	access, newRefresh, err := s.issueTokenPair(ctx, sub, rt)
	if err != nil {
		if errors.Is(err, ErrInvalidRefresh) {
			return nil, ErrInvalidRefresh // lost the rotation claim
		}
		return nil, fmt.Errorf("services.AuthService.Refresh: %w", err)
	}

	return &RefreshOutput{AccessToken: access, RefreshToken: newRefresh}, nil
}

// LogoutEverywhere ENDS every live login session of one user in one tenant — the
// "đăng xuất mọi thiết bị" action, for the case where a laptop is lost or a shared machine was
// left logged in.
//
// Identity comes from the caller's verified claim, never a body field: a user may only end their
// own sessions, and there is nothing to look up or guess.
//
// Since specs/055 (FR-010) this is immediate, not "within 15 minutes": the rows are revoked with
// revoke_reason = 'ended' and every ended session is invalidated in the guard the auth stage
// reads, so each device's still-valid access token is refused on its very next request (another
// replica converges within the guard's 30 s backstop). An access token minted before 055 carries
// no session id and still rides out its own short TTL.
func (s *AuthService) LogoutEverywhere(ctx context.Context, tenantID, userID string) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" {
		return ErrUnauthorized
	}
	if err := s.endAllSessions(ctx, tenantID, userID, ""); err != nil {
		return fmt.Errorf("services.AuthService.LogoutEverywhere: %w", err)
	}
	return nil
}

// endAllSessions is the ONE way a credential-level event ends sessions immediately (#310): logout-all,
// a password change / reset / admin set, the E5 reuse alarm. It revokes every live row of the user as
// 'ended' — except keepSessionID when not blank — and invalidates the session guard for each ended
// session, so the auth stage refuses their access tokens on the very next request instead of after
// the access TTL. A plain revoke (RevokeAllForUser, revoke_reason NULL) left those tokens alive ≤ 15'.
func (s *AuthService) endAllSessions(ctx context.Context, tenantID, userID, keepSessionID string) error {
	sids, err := s.refresh.EndAllSessions(ctx, tenantID, userID, keepSessionID)
	if err != nil {
		return err
	}
	for _, sid := range sids {
		s.sessions.Invalidate(tenantID, userID, sid)
	}
	return nil
}

// Logout ENDS the login session of the presented refresh token (../specs/055-session-list-kill/spec.md
// FR-012), exactly like "đăng xuất thiết bị này": its live row is revoked with revoke_reason =
// 'ended' and the session guard is invalidated. Two tabs of one browser share that session, so the
// tab that did not click keeps neither a working access token (refused on its next request) nor a
// refresh token whose replay reads as theft — an ordinary revoke made that replay trip E5, which
// signed the user out of EVERY device and mailed a security alert.
//
// Only the LIVE token ends its session: a rotated-away, ended, expired or garbage token is a no-op.
// It is idempotent and never reveals whether the token existed (nil either way), so logout cannot
// be used to probe token validity.
func (s *AuthService) Logout(ctx context.Context, rawRefresh string) error {
	raw := strings.TrimSpace(rawRefresh)
	if raw == "" {
		return nil
	}
	// Verify only to extract the tenant for the scoped revoke. A token that fails verification
	// (expired/garbage/wrong-type) has nothing live to revoke anyway → treat as a no-op success.
	claims, err := s.tokens.VerifyRefreshToken(raw)
	if err != nil {
		return nil
	}
	hash := auth.HashRefreshToken(raw)
	rt, err := s.refresh.FindLiveByHash(ctx, claims.TenantID, hash)
	if errors.Is(err, auth.ErrRefreshTokenNotFound) {
		return nil // rotated away, ended, expired or unknown: nothing live to end
	}
	if err != nil {
		return fmt.Errorf("services.AuthService.Logout: %w", err)
	}
	if rt.SessionID == "" {
		// A row with no session (written by a pre-055 binary after the backfill ran): an ordinary
		// revoke is all there is to do, and no access token names it.
		if _, err := s.refresh.RevokeByHash(ctx, claims.TenantID, hash); err != nil {
			return fmt.Errorf("services.AuthService.Logout: %w", err)
		}
		return nil
	}
	if _, err := s.refresh.EndSession(ctx, rt.TenantID, rt.UserID, rt.SessionID); err != nil {
		return fmt.Errorf("services.AuthService.Logout: %w", err)
	}
	s.sessions.Invalidate(rt.TenantID, rt.UserID, rt.SessionID)
	return nil
}

// Me returns the current user's safe public view for a presented ACCESS token. It verifies
// the token (signature, HS256, not expired, type=access), then loads the user by the verified
// (tenant_id, user_id) so a soft-deleted/cross-tenant subject resolves to ErrUnauthorized
// rather than leaking. The password hash is never included. /me deliberately self-verifies the
// presented Bearer access token (the A1 contract): it sits BEFORE AuthMiddleware on the public
// group because it needs no tenant-from-claim, so A2 intentionally kept this self-verify path
// rather than moving it behind the middleware.
func (s *AuthService) Me(ctx context.Context, rawAccess string) (*LoginUser, error) {
	raw := strings.TrimSpace(rawAccess)
	if raw == "" {
		return nil, ErrUnauthorized
	}
	claims, err := s.tokens.VerifyAccessToken(raw)
	if err != nil {
		// specs/064 S9-2: a preview token is the one other credential /me answers for (synthetic
		// profile, same uncached session + builder checks as the data path).
		if s.previews != nil && errors.Is(err, auth.ErrWrongTokenType) {
			return s.previewMe(ctx, raw)
		}
		return nil, ErrUnauthorized
	}
	// specs/062 L1b: a builder workspace-session token. MUST come before the ended-session guard below
	// — that guard asks about (claims.TenantID, claims.UserID), i.e. the CUSTOMER tenant, where the
	// builder has no login session; the resolver asks the right questions (session row + home login).
	if claims.HasDelegationClaims() {
		return s.meWorkspace(ctx, claims)
	}
	// specs/055 FR-008: /auth/me self-verifies (it sits before the auth stage), so it asks the same
	// session guard itself — an ended session's boot probe is a 401 like its data requests.
	if claims.SessionID != "" {
		ended, err := s.sessions.SessionEnded(ctx, claims.TenantID, claims.UserID, claims.SessionID)
		if err != nil {
			return nil, fmt.Errorf("services.AuthService.Me: session check: %w", err)
		}
		if ended {
			return nil, ErrUnauthorized
		}
	}
	u, err := s.users.GetUserByID(ctx, claims.TenantID, claims.UserID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("services.AuthService.Me: load user: %w", err)
	}
	lu := &LoginUser{
		ID:       u.ID,
		TenantID: u.TenantID,
		Email:    u.Email,
		FullName: u.FullName,
		IsAdmin:  u.IsAdmin,
	}
	if u.RoleID != nil {
		lu.RoleID = *u.RoleID
	}
	// FE-CATCHUP-1: /auth/me is the ONE place that carries the honest MFA state (always
	// present, true or false) so the FE MfaCard opens knowing on/off instead of probing via
	// enroll-409/activate-422. Resolved from the verified claim's (tenant,user) via the same
	// crypto-free enrollment reader as the login gate — no secret decryption, key-absent safe.
	enrolled, err := s.mfaEnabledFor(ctx, u.TenantID, u.ID)
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.Me: mfa flag: %w", err)
	}
	lu.MFAEnabled = &enrolled
	// EASE Nước 6: honest operator flag for the FE's Builder-face gate — already on the loaded
	// user row (zero extra query). Same Me-only *bool discipline as MFAEnabled.
	op := u.IsPlatformOperator
	lu.IsPlatformOperator = &op
	owner := u.CanManageUsers
	lu.CanManageUsers = &owner
	return lu, nil
}

// workspaceMeResolver is the slice of services.WorkspaceSessionService (middleware.WorkspaceResolver)
// GET /auth/me needs for a builder workspace-session token.
type workspaceMeResolver interface {
	Resolve(ctx context.Context, claims *auth.Claims) (*auth.ResolvedWorkspace, error)
}

// WithWorkspaceResolver wires the resolver GET /auth/me uses for workspace-session tokens.
func (s *AuthService) WithWorkspaceResolver(r workspaceMeResolver) *AuthService {
	s.workspace = r
	return s
}

// meWorkspace answers GET /auth/me for a workspace-session token: the builder's identity (id, e-mail,
// name, MFA) from the HOME user row (att, act), the tenant the CUSTOMER, plus the workspace block.
// A malformed token, no resolver, or a session/builder that is no longer live is ErrUnauthorized; a
// resolver DB error is returned wrapped (500), never folded into 401.
func (s *AuthService) meWorkspace(ctx context.Context, claims *auth.Claims) (*LoginUser, error) {
	if s.workspace == nil || !claims.DelegationShapeOK() {
		return nil, ErrUnauthorized
	}
	res, err := s.workspace.Resolve(ctx, claims)
	if errors.Is(err, auth.ErrWorkspaceSessionNotLive) || errors.Is(err, auth.ErrActorNotLive) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.Me: resolve workspace session: %w", err)
	}
	u, err := s.users.GetUserByID(ctx, claims.ActorTenantID, claims.ActorUserID)
	if errors.Is(err, auth.ErrUserNotFound) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.Me: load builder: %w", err)
	}
	enrolled, err := s.mfaEnabledFor(ctx, u.TenantID, u.ID)
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.Me: mfa flag: %w", err)
	}
	op := true // the resolver just confirmed the operator flag on the live home row
	return &LoginUser{
		ID: u.ID, TenantID: claims.TenantID, Email: u.Email, FullName: u.FullName, IsAdmin: true,
		MFAEnabled: &enrolled, IsPlatformOperator: &op,
		Workspace: &WorkspaceInfo{
			SessionID: res.Session.ID, TenantID: res.Session.TenantID, TenantSlug: res.Session.TenantSlug,
			TenantName: res.Session.TenantName, ExpiresAt: res.Session.ExpiresAt.UTC(),
		},
	}, nil
}

// ChangePassword verifies the caller's current password and replaces it with a new bcrypt hash
// (cost 12 — matches bcryptCost in user_service.go). On success every OTHER login session of this
// user is ENDED (#310, specs/055 FR-013): other devices are refused on their very next request, not
// after the 15-minute access TTL. keepSessionID is the session that made the call (the verified
// claim's `sid`): the device that just proved the old password stays signed in, so changing a
// password is not an unexplained sign-out. Blank (a token minted before 055 carries no `sid`) ends
// every session including the caller's own.
//
// Errors:
//   - ErrInvalidCredentials  — currentPassword does not match the stored hash (401)
//   - *ValidationError{Code:"PASSWORD_TOO_WEAK"} — newPassword violates the shared policy (422)
//   - auth.ErrUserNotFound   — userID not found in tenant (should not happen for a real caller)
func (s *AuthService) ChangePassword(ctx context.Context, tenantID, userID, currentPassword, newPassword, keepSessionID string) error {
	// 1. Load the user (tenant-pinned — cross-tenant access is structurally impossible).
	u, err := s.users.GetUserByID(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return ErrInvalidCredentials
		}
		return fmt.Errorf("services.AuthService.ChangePassword: load user: %w", err)
	}

	// 2. Verify current password with constant-time bcrypt comparison (anti-timing).
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(currentPassword)); err != nil {
		return ErrInvalidCredentials
	}

	// 3. Validate the new password against the SHARED policy (RM-17, password_policy.go) —
	//    the same body CreateUser/UpdateUser/ResetPassword use, so the rules cannot drift.
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}

	// 4. Hash the new password at cost 12 (bcryptCost constant — defined in user_service.go).
	hashed, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcryptCost)
	if err != nil {
		return fmt.Errorf("services.AuthService.ChangePassword: hash: %w", err)
	}

	// 5. Persist the new hash (tenant-pinned WHERE).
	if err := s.users.UpdatePasswordHash(ctx, tenantID, userID, string(hashed)); err != nil {
		return fmt.Errorf("services.AuthService.ChangePassword: update hash: %w", err)
	}

	// 6. End every other session (refresh rows 'ended' + guard invalidated) so no device keeps
	//    working on the old credential. Failure here is a real error (the password is already
	//    changed — we must not silently leave live tokens that carry the old credential). Return the
	//    wrapped error so the handler can surface a 500 rather than falsely claiming success.
	if err := s.endAllSessions(ctx, tenantID, userID, keepSessionID); err != nil {
		return fmt.Errorf("services.AuthService.ChangePassword: end sessions: %w", err)
	}

	// 7. Emit a security alert (best-effort — fail-open so a slow SMTP never blocks the response).
	s.enqueueAlert(ctx, notify.SecurityEvent{
		Type:     notify.AlertPasswordChanged,
		TenantID: tenantID,
		UserID:   userID,
		Email:    u.Email,
		At:       time.Now().UTC(),
	})

	return nil
}

// previewMe answers /auth/me for a preview token. A token that does not stand is ErrUnauthorized
// (401 — the FE leaves preview mode); an infrastructure error stays an error (500).
func (s *AuthService) previewMe(ctx context.Context, raw string) (*LoginUser, error) {
	u, err := s.previews.Profile(ctx, raw)
	if err != nil {
		if errors.Is(err, ErrPreviewUnauthorized) {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("services.AuthService.Me: preview: %w", err)
	}
	return u, nil
}
