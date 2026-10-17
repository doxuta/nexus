package services

import (
	"context"
	"errors"
	"fmt"
	"nexus/platform/internal/audit"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/notify"
)

// ErrInvalidCredentials is the single, generic authentication failure returned by Login
// for ANY rejection — unknown tenant, unknown user, wrong password, or disabled account.
// Folding every case into one error (and one 401 at the handler) prevents account/tenant
// enumeration: the caller can never tell which part of {tenant, email, password} was wrong
// (../specs/001-auth-security/spec.md §4 login flow, §5 security).
var ErrInvalidCredentials = errors.New("auth: invalid credentials")

// ErrInvalidRefresh is the generic failure for the refresh flow: a missing, malformed,
// expired, wrong-type, or already-revoked/rotated refresh token. Like ErrInvalidCredentials
// it is intentionally undifferentiated so a leaked-but-dead token cannot be probed.
var ErrInvalidRefresh = errors.New("auth: invalid refresh token")

// ErrUnauthorized is returned by token-protected use cases (Me) when the presented access
// token is missing/invalid/expired. The handler maps it to a 401.
var ErrUnauthorized = errors.New("auth: unauthorized")

// dummyBcryptHash is a fixed cost-12 bcrypt digest of a throwaway string. On the
// tenant-not-found and user-not-found branches of Login, where there is no stored hash to
// compare against, we still run one bcrypt.CompareHashAndPassword against this constant so
// every failed login pays the same ~cost-12 CPU as a real password mismatch. Without it the
// not-found branches return after a few cheap milliseconds while the user-exists branch runs
// the slow bcrypt, leaking a timing side-channel an attacker can use to enumerate valid
// tenant slugs / emails (GD1 bug #16). Cost is 12 to match prod hashing (bcryptCost in
// user_service.go / spec §5). The compare always fails; its result is intentionally ignored.
var dummyBcryptHash = []byte("$2a$12$P7ue/7NFkydmE8Qs.aF0qO3fMi863pOU8u7C0PzBxqKmxiQ4oBuxS")

// userRepo is the subset of the auth user repository the auth flows need. Defining it as
// an interface here (consumer-side) keeps the service unit-testable with an in-memory fake
// and inverts the dependency so services does not import a concrete DB type.
type userRepo interface {
	ResolveTenantBySlug(ctx context.Context, slug string) (*auth.Tenant, error)
	GetTenantByID(ctx context.Context, id string) (*auth.Tenant, error)
	GetUserByEmail(ctx context.Context, tenantID, email string) (*auth.User, error)
	GetUserByID(ctx context.Context, tenantID, id string) (*auth.User, error)
	// UpdatePasswordHash replaces the stored bcrypt hash for (tenant, user). Used by ChangePassword.
	UpdatePasswordHash(ctx context.Context, tenantID, id, newHash string) error
	// RecordFailedLogin / ResetLoginFailures maintain the persistent lockout columns
	// (failed_login_count, locked_until — mig 000043). RecordFailedLogin is one atomic UPDATE:
	// increment + stamp locked_until=lockUntil when the new count reaches threshold.
	RecordFailedLogin(ctx context.Context, tenantID, id string, threshold int, lockUntil time.Time) error
	ResetLoginFailures(ctx context.Context, tenantID, id string) error
}

// refreshRepo is the subset of the refresh-token repository the auth flows need: persisting
// a freshly minted token's hash, resolving a live token by hash, and revoking tokens. All
// methods are tenant-scoped in the implementation.
type refreshRepo interface {
	Store(ctx context.Context, rt *auth.RefreshToken) error
	FindLiveByHash(ctx context.Context, tenantID, tokenHash string) (*auth.RefreshToken, error)
	// FindByHashIncludingRevoked looks up a row that is already revoked (revoked_at IS NOT NULL).
	// Used by the E5 reuse-detection path to distinguish a replayed already-dead token (theft
	// signal) from a token that was never issued or has naturally expired (not a theft signal).
	FindByHashIncludingRevoked(ctx context.Context, tenantID, tokenHash string) (*auth.RefreshToken, error)
	RevokeByHash(ctx context.Context, tenantID, tokenHash string) (bool, error)
	// Rotate claims (revokes) the live row of oldHash AND inserts its successor in ONE transaction;
	// false = the claim matched no live row (lost a rotation race / session ended), nothing written.
	// A failing insert rolls the claim back, so the presented token stays usable (#312).
	Rotate(ctx context.Context, tenantID, oldHash string, next *auth.RefreshToken) (bool, error)
	RevokeAllForUser(ctx context.Context, tenantID, userID string) error
	// ListLiveSessions returns the live rows of one user — one per login session (specs/055).
	ListLiveSessions(ctx context.Context, tenantID, userID string, limit int) ([]auth.RefreshToken, error)
	// EndSession / EndAllSessions revoke with revoke_reason = 'ended' (specs/055 FR-007/FR-010):
	// one session of one user, or every live session of that user (returning their ids).
	EndSession(ctx context.Context, tenantID, userID, sessionID string) (bool, error)
	// exceptSessionID (when not blank) keeps that one session alive: a user changing their own password.
	EndAllSessions(ctx context.Context, tenantID, userID, exceptSessionID string) ([]string, error)
}

// tokenIssuer is the subset of auth.TokenService the auth flows need: minting access and
// refresh tokens, verifying both kinds, and reporting the refresh TTL (to stamp the stored
// row's expiry in lockstep with the JWT's exp claim).
type tokenIssuer interface {
	IssueAccessToken(sub auth.Subject) (string, error)
	IssueRefreshToken(sub auth.Subject) (string, error)
	VerifyAccessToken(tok string) (*auth.Claims, error)
	VerifyRefreshToken(tok string) (*auth.Claims, error)
	RefreshTTL() time.Duration
}

// previewProfiler is the preview half of Me (*PreviewService.Profile).
type previewProfiler interface {
	Profile(ctx context.Context, tok string) (*LoginUser, error)
}

// AuthService implements the A1 auth use cases (../specs/001-auth-security/spec.md §3): login, refresh
// (rotate), logout (revoke), and "me". It authenticates {tenant, email, password} or a
// presented token against the repositories and mints/rotates/revokes JWTs via the issuer.
// It is HTTP-agnostic (the handler maps its errors to status codes) and holds no per-request
// state, so it is safe for concurrent use.
type AuthService struct {
	users    userRepo
	refresh  refreshRepo
	tokens   tokenIssuer
	notifier notify.Notifier  // E5 security alerts; defaults to noop
	auditor  audit.Auditor    // W7: governance trail for credential-granting admin acts; noop default
	logger   *zap.Logger      // logs recovered panics from the notifier (fail-open)
	newID    func() string    // injectable id generator; defaults to uuid
	now      func() time.Time // injectable clock; defaults to time.Now

	// agentKeys (issue #3, WithAgentKeys) ends the user's MCP agent keys with their sessions. nil = none.
	agentKeys agentKeyRevoker

	// specs/062-customer-workspace L1b: resolves a builder workspace-session token for GET /auth/me
	// (the same resolver the Auth stage uses). nil ⇒ such a token is ErrUnauthorized (fail closed).
	workspace workspaceMeResolver

	// ENG-AUTH2 #1 forgot/reset-password (auth_password_reset.go). resetRepo defaults to nil
	// (reset support inert) so every existing 3-arg NewAuthService call site is unaffected
	// until WithResetRepo/WithResetMailer are called (main.go).
	resetRepo    resetRepo
	resetSender  notify.EmailSender // token-capable sender, injected via WithResetMailer
	resetFrom    string             // envelope/header From for reset mail
	resetURLBase string             // "<resetURLBase>?token=<raw>"
	// runAsync dispatches the fire-and-forget reset email off the request path (nil ⇒ `go fn()`,
	// overridable via WithMailDispatcher; tests inject a synchronous runner). Keeps
	// RequestPasswordReset latency + HTTP status independent of account existence
	// (anti-enumeration, MED-1).
	runAsync func(func())
	// specs/066 L3: the tenant's email log; the reset mail and the admin-issued link are written
	// to it from INSIDE the detached send goroutine (WithEmailLog). nil ⇒ no logging.
	emailLog emailLogInserter

	// ENG-AUTH2 #7 MFA/TOTP (auth_mfa.go). All four are nil by default (MFA support inert) so
	// every existing 3-arg NewAuthService call site — and Login's non-MFA path — is
	// BYTE-IDENTICAL whether or not WithMFA is ever called: Login only branches into the MFA
	// path when mfaRepo != nil AND the caller's row is enabled. main.go calls WithMFA only
	// when config.LoadMFAConfig().Enabled (MFA_SECRET_ENC_KEY configured).
	mfaRepo    mfaRepo
	mfaBackup  mfaBackupRepo
	mfaPending mfaPendingRepoIface
	mfaCrypto  mfaSealer
	// MFA-ENFORCE (auth_mfa_enforce.go): mfaEnrollment is the ALWAYS-wired, crypto-free
	// enrollment reader (needs only the DB), injected unconditionally by main.go via
	// WithMFAEnrollmentReader so losing MFA_SECRET_ENC_KEY can no longer silently skip the
	// login-time MFA gate for enrolled accounts. Nil in bare test fixtures ⇒ gate skipped
	// (same as the historical nil-mfaRepo behavior). mfaFailOpen mirrors MFA_FAIL_OPEN
	// (INFRA-EMERGENCY override, default false) — see config.MFAConfig.FailOpen.
	mfaEnrollment mfaEnrollmentReader
	mfaFailOpen   bool
	// mfaAttemptLimiter is the per-account failed-MFA-code throttle guarding
	// MFAActivate/MFADisable (reviewer MED-1 / security review 🟡#4, mfa_attempt_limiter.go).
	// Wired automatically by WithMFA; overridable via WithMFAAttemptLimiter.
	mfaAttemptLimiter mfaAttemptLimiter

	// Persistent account lockout (prod-ops slice b, mig 000043): after lockoutMaxFailures
	// consecutive wrong passwords on an EXISTING user, login is refused (generic 401) until
	// lockoutDuration has passed. Complements the in-memory sliding-window throttle (RM-15,
	// middleware/auth_ratelimit.go): that one is per-(ip,email) and resets on restart; this
	// one is per-ACCOUNT and survives restarts / multi-instance. Defaults 10 / 15 min
	// (NewAuthService); main.go overrides from env via WithLockoutPolicy.
	lockoutMaxFailures int
	lockoutDuration    time.Duration

	// sessions (specs/055) is the ended-session guard the auth stage reads; EndSession /
	// LogoutEverywhere invalidate it, Me consults it. noop until WithSessionGuard.
	sessions sessionGuard
	// previews (specs/064 S9-2) answers /auth/me for a preview token; nil ⇒ such a token is a 401.
	previews previewProfiler
}

// NewAuthService wires the auth service over a user repo, a refresh-token repo, and a
// token issuer. The notifier defaults to noop (security alerts inert) until WithNotifier
// injects the real dispatcher. The logger defaults to noop until WithLogger injects the real one.
func NewAuthService(users userRepo, refresh refreshRepo, tokens tokenIssuer) *AuthService {
	return &AuthService{
		users:    users,
		refresh:  refresh,
		tokens:   tokens,
		notifier: notify.NewNoopNotifier(),
		auditor:  audit.NewNoopAuditor(),
		logger:   zap.NewNop(),
		newID:    uuid.NewString,
		now:      time.Now,

		// Persistent lockout defaults (AUTH_LOCKOUT_MAX_FAILURES / AUTH_LOCKOUT_DURATION_MIN).
		// Set here — not only in main.go — so the protection is on by default in every fixture;
		// there is deliberately NO way to disable it (0/negative in WithLockoutPolicy keeps these).
		lockoutMaxFailures: defaultLockoutMaxFailures,
		lockoutDuration:    defaultLockoutDuration,

		sessions: noopSessionGuard{},
	}
}

// Persistent-lockout defaults — mirrored by config.LoadConfig's env defaults so an unset env
// and a bare NewAuthService behave identically.
const (
	defaultLockoutMaxFailures = 10
	defaultLockoutDuration    = 15 * time.Minute
)

// WithLockoutPolicy overrides the persistent-lockout thresholds (main.go wires the
// AUTH_LOCKOUT_MAX_FAILURES / AUTH_LOCKOUT_DURATION_MIN env knobs through here). Non-positive
// values keep the defaults — consistent with the AI_* knobs, the env cannot turn the
// protection OFF, only tune it. Returns the service for fluent wiring.
func (s *AuthService) WithLockoutPolicy(maxFailures int, duration time.Duration) *AuthService {
	if maxFailures > 0 {
		s.lockoutMaxFailures = maxFailures
	}
	if duration > 0 {
		s.lockoutDuration = duration
	}
	return s
}

// WithNotifier injects the security-alert notifier for E5 (refresh-token reuse detection).
// A nil argument leaves the default noop in place. Returns the service for fluent wiring in main.
func (s *AuthService) WithNotifier(n notify.Notifier) *AuthService {
	if n != nil {
		s.notifier = n
	}
	return s
}

// WithAuditor injects the governance audit sink (W7): reset-link mints are recorded as
// __user__ events so credential-granting admin acts are attributable. Noop by default.
func (s *AuthService) WithAuditor(a audit.Auditor) *AuthService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// WithLogger injects the structured logger used to record recovered panics from the notifier
// (fail-open contract). A nil argument leaves the default noop logger in place.
func (s *AuthService) WithLogger(l *zap.Logger) *AuthService {
	if l != nil {
		s.logger = l
	}
	return s
}

// enqueueAlert fires a security alert fail-open: if the notifier panics, it recovers and logs
// the panic value (alert_type, tenant_id, user_id — never secrets) via the structured logger,
// then continues so the originating business flow is never interrupted (spec §1).
func (s *AuthService) enqueueAlert(ctx context.Context, evt notify.SecurityEvent) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Warn("services.AuthService.enqueueAlert: notifier panicked, alert dropped",
				zap.String("alert_type", evt.Type),
				zap.String("tenant_id", evt.TenantID),
				zap.String("user_id", evt.UserID),
				zap.Any("panic", r),
			)
		}
	}()
	s.notifier.EnqueueSecurityAlert(ctx, evt)
}

// LoginInput is the login payload: a tenant slug plus the user's email and password.
type LoginInput struct {
	Tenant   string
	Email    string
	Password string
}

// LoginUser is the safe public view of an authenticated user returned to the caller.
// It deliberately omits the password hash (and any other secret) so it can be serialized
// straight to the API response. RoleID (RM-05) is the user's assigned RBAC role, or empty
// when the user has no role.
type LoginUser struct {
	ID       string `json:"id"`
	TenantID string `json:"tenantId"`
	Email    string `json:"email"`
	FullName string `json:"fullName,omitempty"`
	IsAdmin  bool   `json:"isAdmin"`
	RoleID   string `json:"roleId,omitempty"` // empty when user has no assigned role (RM-05)
	// MFAEnabled reports whether the account currently has MFA (TOTP) enabled — set ONLY by
	// Me (GET /auth/me) so the FE can render the real MFA state instead of discovering it via
	// action error responses (FE-CATCHUP-1). It is a *bool with omitempty on purpose: every
	// OTHER LoginUser producer (Login, CompleteMFALogin, SSO) leaves it nil so those response
	// shapes stay byte-identical (TestLogin_NonMFA_ResponseByteIdentical) — a plain bool would
	// also be DISHONEST there (e.g. CompleteMFALogin's user is enrolled by definition, but its
	// LoginUser would say false). Resolved via the same crypto-free enrollment reader as the
	// login gate (mfaEnabledFor, auth_mfa_enforce.go), so it works with MFA_SECRET_ENC_KEY absent.
	MFAEnabled *bool `json:"mfaEnabled,omitempty"`
	// IsPlatformOperator (EASE Nước 6, ../specs/034-customer-admin-tier/spec.md §3) reports whether the
	// account carries the platform-operator flag — set ONLY by Me (GET /auth/me), same *bool
	// omitempty discipline as MFAEnabled so every other producer's response shape stays
	// byte-identical. FE uses it to gate the Builder face UI; the BE never trusts it —
	// RequirePlatformOperator re-reads the DB per request (revoke takes effect immediately).
	IsPlatformOperator *bool `json:"isPlatformOperator,omitempty"`
	// CanManageUsers (specs/065 D-12) tells the FE whether to show "Cài đặt › Người dùng" — set ONLY by
	// Me, same *bool omitempty discipline as IsPlatformOperator so Login/MFA/SSO stay byte-identical.
	// Display only: RequireUserManager re-reads the column per request.
	CanManageUsers *bool `json:"canManageUsers,omitempty"`
	// Preview is set ONLY by Me for a preview-as-role token (specs/064 S9-2) — omitempty keeps every
	// other producer, and Me for a normal token, byte-identical.
	Preview *PreviewInfo `json:"preview,omitempty"`
	// Workspace (specs/062-customer-workspace L1b) names the customer workspace a builder is working
	// in — set ONLY by Me for a workspace-session token (a pointer with omitempty: every other
	// producer's JSON stays byte-identical). The FE reads it to draw the "Đang dựng cho" banner.
	Workspace *WorkspaceInfo `json:"workspace,omitempty"`
	// PasswordHash is unexported-by-omission: it never has a JSON tag and is never set.
	PasswordHash string `json:"-"`
}

// WorkspaceInfo is the "workspace" block of GET /auth/me inside a builder workspace session.
type WorkspaceInfo struct {
	SessionID  string    `json:"sessionId"`
	TenantID   string    `json:"tenantId"`
	TenantSlug string    `json:"tenantSlug"`
	TenantName string    `json:"tenantName"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// LoginOutput is the successful login result: the freshly minted token pair plus the
// authenticated user's safe public view — OR, when the account has MFA enabled
// (ENG-AUTH2 #7), the two-step-login "pending" shape instead.
//
// Every field carries `omitempty` so the two shapes never bleed into each other on the wire:
//   - Non-MFA success (unchanged since A1): {"accessToken","refreshToken","user"} — MFAPending
//     and PendingToken are always zero on this path, so they are omitted entirely. This is
//     what makes the non-MFA login response BYTE-IDENTICAL before and after ENG-AUTH2 #7
//     landed (see TestLogin_NonMFA_ResponseByteIdentical).
//   - MFA-pending (new): {"mfaPending":true,"pendingToken":"..."} — AccessToken/RefreshToken/
//     User are always zero/nil on this path (Login never mints a token pair for an
//     MFA-enabled account until CompleteMFALogin succeeds), so those three keys are absent —
//     no half-populated response can ever leak a usable token alongside mfaPending:true.
type LoginOutput struct {
	AccessToken  string     `json:"accessToken,omitempty"`
	RefreshToken string     `json:"refreshToken,omitempty"`
	User         *LoginUser `json:"user,omitempty"`
	// MFAPending is true when the credentials were correct but a second factor is required
	// before a token pair is issued. PendingToken then carries the opaque, short-lived (5 min),
	// single-use bridge credential for POST /auth/mfa/verify (CompleteMFALogin).
	MFAPending   bool   `json:"mfaPending,omitempty"`
	PendingToken string `json:"pendingToken,omitempty"`
}

// Login authenticates the supplied credentials and returns a token pair on success.
//
// Flow (spec §4): resolve the tenant slug → load the user by (tenant_id, email) →
// verify the bcrypt password → check the account is active → mint access + refresh tokens.
// Every failure along the way collapses to ErrInvalidCredentials so the response cannot be
// used to enumerate tenants or accounts. A missing required field is a client error
// (ValidationError) rather than a credential mismatch.
func (s *AuthService) Login(ctx context.Context, in LoginInput) (*LoginOutput, error) {
	tenant := strings.TrimSpace(in.Tenant)
	email := strings.TrimSpace(in.Email)
	if tenant == "" || email == "" || in.Password == "" {
		return nil, invalid("LOGIN_FIELD_REQUIRED", "tenant, email và password là bắt buộc")
	}

	t, err := s.users.ResolveTenantBySlug(ctx, tenant)
	if err != nil {
		if errors.Is(err, auth.ErrTenantNotFound) {
			// Constant-time: run a dummy bcrypt compare so the unknown-tenant branch costs
			// the same as a real password check, closing the timing side-channel (bug #16).
			_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(in.Password))
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("services.AuthService.Login: resolve tenant: %w", err)
	}

	u, err := s.users.GetUserByEmail(ctx, t.ID, email)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			// Constant-time: same dummy bcrypt compare on the unknown-user branch so it is
			// indistinguishable by timing from a wrong password for an existing user (bug #16).
			// No lockout bookkeeping on this branch — there is no user row to write to.
			_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(in.Password))
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("services.AuthService.Login: load user: %w", err)
	}

	// Persistent lockout gate (mig 000043) — BEFORE the real bcrypt compare. A locked account
	// answers the SAME generic 401 as a wrong password (anti-enumeration: the response must not
	// reveal that the account exists, let alone that it is locked — RM-15 fix-direction), and
	// pays the same dummy bcrypt cost so timing does not leak "locked" either. The detail is
	// logged server-side only. A correct password during the lock window neither resets the
	// counter nor extends the lock (we return before both).
	if u.LockedUntil != nil && u.LockedUntil.After(s.now()) {
		_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(in.Password))
		s.logger.Warn("services.AuthService.Login: attempt on locked account",
			zap.String("tenant_id", u.TenantID),
			zap.String("user_id", u.ID),
			zap.Time("locked_until", *u.LockedUntil),
		)
		return nil, ErrInvalidCredentials
	}

	// Verify the password against the stored bcrypt digest. A mismatch is a credential
	// failure; any other bcrypt error (e.g. malformed hash in the DB) is an internal fault.
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			// Wrong password on an EXISTING user: bump the persistent failure counter (one
			// atomic UPDATE; stamps locked_until when the new count reaches the threshold).
			// Fail-open on the bookkeeping write: the login is already being refused with the
			// generic 401, and the in-memory RM-15 throttle still stands — a DB hiccup here
			// must not turn a 401 into a 500 (but it is logged loudly).
			if lockErr := s.users.RecordFailedLogin(ctx, u.TenantID, u.ID, s.lockoutMaxFailures, s.now().Add(s.lockoutDuration)); lockErr != nil {
				s.logger.Error("services.AuthService.Login: record failed login",
					zap.String("tenant_id", u.TenantID), zap.String("user_id", u.ID), zap.Error(lockErr))
			} else if u.FailedLoginCount+1 >= s.lockoutMaxFailures {
				// Best-effort lock signal (the pre-attempt count we loaded may lag a concurrent
				// attempt by one, which only makes this log fire late, never the lock itself).
				s.logger.Warn("services.AuthService.Login: account locked after repeated failures",
					zap.String("tenant_id", u.TenantID),
					zap.String("user_id", u.ID),
					zap.Int("failed_login_count", u.FailedLoginCount+1),
					zap.Duration("lockout", s.lockoutDuration),
				)
			}
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("services.AuthService.Login: verify password: %w", err)
	}

	// Only active accounts may obtain tokens; disabled/invited users are rejected as a
	// generic credential failure (no signal that the account merely exists-but-disabled).
	// Deliberately NO failure-counter increment here: the password was CORRECT.
	if u.Status != "active" {
		return nil, ErrInvalidCredentials
	}

	// Correct password on an active account: clear the lockout state. This is the right seam
	// for BOTH login shapes — the MFA path below has already proven the password here (step 2,
	// CompleteMFALogin, only verifies the second factor; its guesses are bounded by the
	// separate mfaAttemptLimiter), and the SSO path (issueSessionForSubject) never touches the
	// password counter at all: the lock guards the PASSWORD credential, and an IdP login must
	// neither trip it nor clear it. Skipped entirely when the row is already clean so the
	// common happy path costs zero extra writes. Fail-open: a bookkeeping failure must not
	// refuse a correct login (logged loudly instead).
	if u.FailedLoginCount > 0 || u.LockedUntil != nil {
		if resetErr := s.users.ResetLoginFailures(ctx, u.TenantID, u.ID); resetErr != nil {
			s.logger.Error("services.AuthService.Login: reset login failures",
				zap.String("tenant_id", u.TenantID), zap.String("user_id", u.ID), zap.Error(resetErr))
		}
	}

	// ENG-AUTH2 #7 two-step login + MFA-ENFORCE fail-closed (mfaLoginGate, auth_mfa_enforce.go).
	// This runs ONLY after the password has verified and the account is active — never earlier —
	// so an MFA check can never leak "this account exists" or "this account has MFA" ahead of a
	// real credential failure (mirrors the constant-time discipline above). The gate is skipped
	// only when NEITHER mfaRepo NOR the always-wired enrollment reader is present (bare test
	// fixtures), which is what keeps the non-MFA path byte-identical. An ENROLLED account whose
	// server has lost MFA_SECRET_ENC_KEY is fail-closed (ErrMFAEnrolledButUnavailable → 503),
	// never silently downgraded to one-step login.
	//
	// Deliberate scope (MFA-ENFORCE): /auth/refresh is NOT gated on enrollment — refresh
	// exchanges an EXISTING session; the second factor is enforced here, at session CREATION.
	pendingToken, mfaPendingRequired, gateErr := s.mfaLoginGate(ctx, u.TenantID, u.ID)
	if gateErr != nil {
		if errors.Is(gateErr, ErrMFAEnrolledButUnavailable) {
			return nil, ErrMFAEnrolledButUnavailable
		}
		return nil, fmt.Errorf("services.AuthService.Login: %w", gateErr)
	}
	if mfaPendingRequired {
		return &LoginOutput{MFAPending: true, PendingToken: pendingToken}, nil
	}

	sub := auth.Subject{
		UserID:   u.ID,
		TenantID: u.TenantID,
		IsAdmin:  u.IsAdmin,
	}
	if u.RoleID != nil {
		sub.RoleID = *u.RoleID
	}

	access, refresh, err := s.issueTokenPair(ctx, sub, nil)
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.Login: %w", err)
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
	return &LoginOutput{
		AccessToken:  access,
		RefreshToken: refresh,
		User:         lu,
	}, nil
}

// issueTokenPair mints the access + refresh pair of ONE login session — the single mint path of
// every login shape (password Login, CompleteMFALogin, SSO issueSessionForSubject) and of Refresh
// (../specs/055-session-list-kill/spec.md FR-002/FR-006). prev == nil opens a NEW session; prev (the
// row being rotated away) continues ITS session. Either way the session id is stamped into BOTH
// tokens as the `sid` claim before they are signed, so the access token names its session: GET
// /auth/sessions can mark "phiên này", and the auth stage can refuse it the moment the session is
// ended. A pre-migration row with no session id adopts a fresh one here.
func (s *AuthService) issueTokenPair(ctx context.Context, sub auth.Subject, prev *auth.RefreshToken) (access, refresh string, err error) {
	sub.SessionID = s.newID()
	if prev != nil && prev.SessionID != "" {
		sub.SessionID = prev.SessionID
	}
	access, err = s.tokens.IssueAccessToken(sub)
	if err != nil {
		return "", "", fmt.Errorf("issue access token: %w", err)
	}
	refresh, err = s.issueAndStoreRefresh(ctx, sub, prev)
	if err != nil {
		return "", "", err
	}
	return access, refresh, nil
}

// issueAndStoreRefresh mints a refresh token for sub and persists ONLY its SHA-256 hash in
// refresh_tokens, stamping expires_at in lockstep with the JWT's exp (spec §5). The raw token
// is returned to the caller and never written to the database. A persistence failure aborts
// the flow rather than handing out an unrevocable token.
//
// The row records the login session sub.SessionID (set by issueTokenPair, its only caller) and
// the device (specs/055 FR-002): a new session takes the device on ctx (WithClientDevice); a
// rotation (prev != nil) keeps the chain's device label, takes the IP the refresh came from
// (falling back to the chain's), and stamps last_used_at = now.
func (s *AuthService) issueAndStoreRefresh(ctx context.Context, sub auth.Subject, prev *auth.RefreshToken) (string, error) {
	refresh, err := s.tokens.IssueRefreshToken(sub)
	if err != nil {
		return "", fmt.Errorf("issue refresh token: %w", err)
	}
	now := s.now()
	dev := clientDeviceFrom(ctx)
	rt := &auth.RefreshToken{
		ID:         s.newID(),
		UserID:     sub.UserID,
		TenantID:   sub.TenantID,
		SessionID:  sub.SessionID,
		TokenHash:  auth.HashRefreshToken(refresh),
		UserAgent:  truncateUserAgent(dev.UserAgent),
		IP:         maskIP(dev.IP),
		LastUsedAt: &now,
		ExpiresAt:  now.Add(s.tokens.RefreshTTL()),
		CreatedAt:  now,
	}
	if prev != nil && prev.SessionID != "" {
		if prev.UserAgent != "" {
			rt.UserAgent = prev.UserAgent // the device label is the chain's, not this request's
		}
		if rt.IP == "" {
			rt.IP = prev.IP
		}
	}
	if prev == nil {
		if err := s.refresh.Store(ctx, rt); err != nil {
			return "", fmt.Errorf("store refresh token: %w", err)
		}
		return refresh, nil
	}
	// A rotation: claim the presented row and store its successor atomically (#312). Losing the claim
	// (a concurrent replay won it, or the session was ended meanwhile) is ErrInvalidRefresh.
	ok, err := s.refresh.Rotate(ctx, prev.TenantID, prev.TokenHash, rt)
	if err != nil {
		return "", fmt.Errorf("rotate refresh token: %w", err)
	}
	if !ok {
		return "", ErrInvalidRefresh
	}
	return refresh, nil
}
