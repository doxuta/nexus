package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/notify"
)

// ErrMFACodeInvalid is the single, generic failure for a wrong/expired TOTP code AND a wrong/
// already-used backup code, folded into one error (mirrors ErrInvalidCredentials) so a wrong
// guess reveals nothing about which check failed or why (anti-probe,
// ../specs/003-mfa-totp/spec.md §"#7").
var ErrMFACodeInvalid = errors.New("auth: mfa code invalid")

// ErrMFAUnavailable is returned by every MFA use case when the service was constructed
// without WithMFA (MFA_SECRET_ENC_KEY unset at boot — config.LoadMFAConfig().Enabled ==
// false). Mirrors the resetRepo == nil branches in auth_password_reset.go.
var ErrMFAUnavailable = errors.New("services: mfa support not configured")

// ErrMFATooManyAttempts is returned by MFAActivate/MFADisable when the caller's (tenant_id,
// user_id) has exceeded mfaAttemptMaxFailures wrong codes within MFAAttemptLockoutWindow (see
// mfa_attempt_limiter.go's file doc for why this guard exists on these two PROTECTED endpoints
// specifically). It deliberately reveals nothing about whether the code just presented would
// have been correct — the guard trips BEFORE the code is even checked.
var ErrMFATooManyAttempts = errors.New("auth: too many mfa attempts")

// mfaPendingTokenTTL bounds how long the two-step-login bridge token is usable
// (../specs/003-mfa-totp/spec.md §"#7": "pendingToken là token ngắn hạn (5 phút, một lần dùng)").
const mfaPendingTokenTTL = 5 * time.Minute

// mfaRepo is the subset of auth.UserMFARepository the MFA flows need. Consumer-side
// interface (mirrors userRepo/refreshRepo/resetRepo) so the service stays unit-testable with
// an in-memory fake.
type mfaRepo interface {
	Get(ctx context.Context, tenantID, userID string) (*auth.UserMFA, error)
	Create(ctx context.Context, m *auth.UserMFA) error
	UpdateSecret(ctx context.Context, tenantID, userID, secretEnc string) error
	// ActivateWithStepAndBackupCodes atomically flips enabled=1 AND persists the freshly
	// generated backup codes in ONE transaction (reviewer LOW-1 fix) — see
	// auth.UserMFARepository's doc for why the enable-flip and the backup-code insert must not
	// be two independent writes.
	ActivateWithStepAndBackupCodes(ctx context.Context, tenantID, userID string, step int64, codes []*auth.MFABackupCode) (bool, error)
	ConsumeStep(ctx context.Context, tenantID, userID string, step int64) (bool, error)
	Delete(ctx context.Context, tenantID, userID string) error
}

// mfaBackupRepo is the subset of auth.MFABackupCodeRepository the MFA flows need. StoreBatch
// is deliberately NOT part of this interface: since the reviewer LOW-1 fix, backup-code
// insertion happens inside mfaRepo.ActivateWithStepAndBackupCodes's own transaction (it needs
// to share a DB transaction with the enabled=1 flip, which this narrower interface cannot
// express) — the concrete auth.MFABackupCodeRepository.StoreBatch method still exists as a
// repository primitive, just unused by AuthService.
type mfaBackupRepo interface {
	ConsumeByHash(ctx context.Context, tenantID, userID, codeHash string) (bool, error)
	DeleteAllForUser(ctx context.Context, tenantID, userID string) error
}

// mfaPendingRepoIface is the subset of auth.MFAPendingTokenRepository the two-step-login flow
// needs. Named with the "Iface" suffix to avoid colliding with the auth.MFAPendingToken type's
// natural "pendingRepo" reading elsewhere in this file.
type mfaPendingRepoIface interface {
	Store(ctx context.Context, pt *auth.MFAPendingToken) error
	FindByHash(ctx context.Context, tokenHash string) (*auth.MFAPendingToken, error)
	ConsumeByHash(ctx context.Context, tenantID, tokenHash string) (bool, error)
	// DeleteAllForUser purges every pending token for (tenantID, userID) — called from
	// MFADisable (reviewer LOW-2 hygiene fix).
	DeleteAllForUser(ctx context.Context, tenantID, userID string) error
}

// mfaSealer is the consumer-side view of internal/crypto.AESGCM the MFA flows need: seal a
// freshly generated TOTP secret at enrollment time, open it back to raw bytes at verify time.
type mfaSealer interface {
	Seal(plaintext []byte) (string, error)
	Open(sealed string) ([]byte, error)
}

// WithMFA injects MFA/TOTP support (ENG-AUTH2 #7). ALL FOUR arguments are required together —
// if any is nil, MFA support stays entirely inert (repo/backup/pending/crypto are left nil),
// which is what keeps every existing 3-arg NewAuthService call site — and Login's non-MFA
// path — BYTE-IDENTICAL whether or not this is ever called (Login only branches into the MFA
// path when s.mfaRepo != nil). main.go calls this only when config.LoadMFAConfig().Enabled.
func (s *AuthService) WithMFA(repo mfaRepo, backup mfaBackupRepo, pending mfaPendingRepoIface, sealer mfaSealer) *AuthService {
	if repo == nil || backup == nil || pending == nil || sealer == nil {
		return s
	}
	s.mfaRepo = repo
	s.mfaBackup = backup
	s.mfaPending = pending
	s.mfaCrypto = sealer
	// Reviewer MED-1 / security review 🟡#4: wire the default in-memory per-account
	// failed-code throttle automatically, so every WithMFA call site (main.go, every test
	// fixture) gets the guard for free — no separate opt-in wiring step to forget.
	// WithMFAAttemptLimiter can override this afterward (tests needing a controllable clock, or
	// a future Redis-backed limiter).
	s.mfaAttemptLimiter = newMFAMemAttemptLimiter(mfaAttemptMaxFailures, MFAAttemptLockoutWindow)
	return s
}

// WithMFAAttemptLimiter overrides the default in-memory per-account failed-MFA-code limiter
// wired by WithMFA. No-op if lim is nil or if MFA support was never wired (mfaRepo == nil) —
// mirrors the other With* setters' guard style in this file.
func (s *AuthService) WithMFAAttemptLimiter(lim mfaAttemptLimiter) *AuthService {
	if lim == nil || s.mfaRepo == nil {
		return s
	}
	s.mfaAttemptLimiter = lim
	return s
}

// allowMFAAttempt reports whether (tenantID, userID) is under the failed-MFA-code threshold.
// Nil-safe: if no limiter is wired (should not happen once WithMFA has run, but defensive),
// attempts are allowed through unthrottled rather than panicking.
func (s *AuthService) allowMFAAttempt(tenantID, userID string) bool {
	if s.mfaAttemptLimiter == nil {
		return true
	}
	return s.mfaAttemptLimiter.Allow(tenantID, userID)
}

// recordMFAFailure registers a wrong code against (tenantID, userID). Nil-safe (see
// allowMFAAttempt).
func (s *AuthService) recordMFAFailure(tenantID, userID string) {
	if s.mfaAttemptLimiter != nil {
		s.mfaAttemptLimiter.RecordFailure(tenantID, userID)
	}
}

// resetMFAAttempts clears the failure count for (tenantID, userID) after a successful verify.
// Nil-safe (see allowMFAAttempt).
func (s *AuthService) resetMFAAttempts(tenantID, userID string) {
	if s.mfaAttemptLimiter != nil {
		s.mfaAttemptLimiter.Reset(tenantID, userID)
	}
}

// openMFASecret decrypts a sealed TOTP secret back to its base32 string form.
func (s *AuthService) openMFASecret(sealedSecret string) (string, error) {
	raw, err := s.mfaCrypto.Open(sealedSecret)
	if err != nil {
		return "", fmt.Errorf("open mfa secret: %w", err)
	}
	return string(raw), nil
}

// verifyMFACode checks a presented code against the enrolled account: it tries the code as a
// TOTP code first (replay-safe via the ConsumeStep CAS), then falls back to a single-use
// backup code. Returns (true, nil) on success; (false, nil) when the code matches neither
// (the caller maps this to ErrMFACodeInvalid — never a "which one failed" signal, anti-probe).
func (s *AuthService) verifyMFACode(ctx context.Context, tenantID, userID, secretEnc, code string) (bool, error) {
	secret, err := s.openMFASecret(secretEnc)
	if err != nil {
		return false, fmt.Errorf("services.AuthService.verifyMFACode: %w", err)
	}
	if step, ok := matchTOTPCode(secret, code, s.now()); ok {
		consumed, err := s.mfaRepo.ConsumeStep(ctx, tenantID, userID, step)
		if err != nil {
			return false, fmt.Errorf("services.AuthService.verifyMFACode: consume step: %w", err)
		}
		if consumed {
			return true, nil
		}
		// Matched but lost the replay-guard CAS (this exact step was already accepted) — fall
		// through to the backup-code check rather than returning early, so the error shape
		// stays identical to "wrong code" (anti-probe).
	}

	hash := auth.HashRefreshToken(strings.TrimSpace(code))
	consumed, err := s.mfaBackup.ConsumeByHash(ctx, tenantID, userID, hash)
	if err != nil {
		return false, fmt.Errorf("services.AuthService.verifyMFACode: consume backup code: %w", err)
	}
	return consumed, nil
}

// issueMFAPending mints an opaque, single-use, 5-minute bridge token bound to (tenantID,
// userID) for the two-step login flow. It is DELIBERATELY not a JWT/refresh token — see
// migration 000030's header for why reusing the refresh-token store would be a second-factor
// bypass. Reuses newResetToken (crypto/rand, 256-bit) — the same entropy floor as the
// password-reset bearer token.
func (s *AuthService) issueMFAPending(ctx context.Context, tenantID, userID string) (string, error) {
	raw, err := newResetToken()
	if err != nil {
		return "", fmt.Errorf("issue mfa pending token: %w", err)
	}
	pt := &auth.MFAPendingToken{
		ID:        s.newID(),
		TenantID:  tenantID,
		UserID:    userID,
		TokenHash: auth.HashRefreshToken(raw),
		ExpiresAt: s.now().Add(mfaPendingTokenTTL),
		CreatedAt: s.now(),
	}
	if err := s.mfaPending.Store(ctx, pt); err != nil {
		return "", fmt.Errorf("store mfa pending token: %w", err)
	}
	return raw, nil
}

// verifyCallerPassword is the MFA-STEPUP re-authentication gate shared by MFAEnroll/
// MFAActivate/MFADisable: the caller is already authenticated (verified JWT), but planting or
// removing a second factor is a security-sensitive mutation, so a STOLEN access token alone
// must not suffice — the caller must additionally prove knowledge of the account password
// (mirrors ChangePassword, the existing re-verify-password-of-a-logged-in-caller exemplar).
//
// Failure semantics (deliberate):
//   - A wrong password returns ErrInvalidCredentials (the SAME sentinel Login/ChangePassword
//     use → 401 INVALID_CREDENTIALS at the handler) AND consumes one attempt in the per-account
//     MFA limiter — brute-forcing the password through these endpoints hits the exact same
//     5-wrong/15-min lockout as brute-forcing a TOTP code (shared counter, mfa_attempt_limiter.go).
//   - When the user row cannot be loaded at all (should not happen for a real verified caller),
//     a dummy bcrypt compare still runs so this branch costs the same ~cost-12 CPU as a real
//     mismatch — mirroring Login's anti-timing pattern (auth_service.go dummyBcryptHash doc).
//   - A CORRECT password alone deliberately does NOT reset the limiter: the counter is shared
//     with the TOTP-code failures of activate/disable, and resetting it on every good password
//     would let a password-knowing attacker guess codes forever. Every fully successful use
//     case — enroll, activate, disable — DOES reset it (each calls resetMFAAttempts itself once
//     its own use-case-specific work is done), exactly as before this slice.
//
// On success it returns the loaded user row so callers can reuse it (email for enroll's
// otpauth:// label and for the security alerts) without a second query.
func (s *AuthService) verifyCallerPassword(ctx context.Context, tenantID, userID, password string) (*auth.User, error) {
	u, err := s.users.GetUserByID(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			// Constant-time: same dummy compare Login runs on its not-found branches, so a
			// missing row is indistinguishable by timing from a wrong password (bug #16 pattern).
			_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(password))
			s.recordMFAFailure(tenantID, userID)
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("load user: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			s.recordMFAFailure(tenantID, userID)
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("verify password: %w", err)
	}
	return u, nil
}

// MFAEnroll generates a fresh TOTP secret for (tenantID, userID), seals it at rest, and
// persists an enabled=0 row (or overwrites a previous not-yet-activated enrollment — a
// re-enroll before the user ever confirmed the first secret). Returns the raw base32 secret
// and an otpauth:// URL for the caller to render as a QR code. Returns auth.ErrMFAAlreadyEnabled
// when the account already has MFA active (the caller must Disable first to re-enroll).
//
// MFA-STEPUP: the caller must re-prove the account password (see verifyCallerPassword) — a
// stolen access token alone can no longer plant an attacker-known TOTP secret. Enroll therefore
// also sits behind the per-account attempt limiter now (it previously had no code to guess, so
// no guard; with a password to guess it needs the same 5/15min lockout as activate/disable).
func (s *AuthService) MFAEnroll(ctx context.Context, tenantID, userID, password string) (secret, otpauthURL string, err error) {
	if s.mfaRepo == nil {
		return "", "", ErrMFAUnavailable
	}
	if password == "" {
		// A missing required field is a client error (mirrors Login's LOGIN_FIELD_REQUIRED) —
		// never a credential mismatch, and it must not burn a limiter attempt.
		return "", "", invalid("MFA_PASSWORD_REQUIRED", "password là bắt buộc")
	}
	if !s.allowMFAAttempt(tenantID, userID) {
		return "", "", ErrMFATooManyAttempts
	}

	u, err := s.verifyCallerPassword(ctx, tenantID, userID, password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			return "", "", ErrInvalidCredentials
		}
		return "", "", fmt.Errorf("services.AuthService.MFAEnroll: %w", err)
	}

	row, getErr := s.mfaRepo.Get(ctx, tenantID, userID)
	switch {
	case getErr == nil && row.Enabled:
		return "", "", auth.ErrMFAAlreadyEnabled
	case getErr == nil && !row.Enabled:
		// Re-enroll before activation: fall through to generate+overwrite below.
	case errors.Is(getErr, auth.ErrMFANotEnrolled):
		// First-time enroll: fall through to generate+create below.
	default:
		return "", "", fmt.Errorf("services.AuthService.MFAEnroll: load mfa: %w", getErr)
	}

	rawSecret, url, genErr := generateTOTPSecret(u.Email)
	if genErr != nil {
		return "", "", fmt.Errorf("services.AuthService.MFAEnroll: %w", genErr)
	}
	sealed, sealErr := s.mfaCrypto.Seal([]byte(rawSecret))
	if sealErr != nil {
		return "", "", fmt.Errorf("services.AuthService.MFAEnroll: seal secret: %w", sealErr)
	}

	if errors.Is(getErr, auth.ErrMFANotEnrolled) {
		if err := s.mfaRepo.Create(ctx, &auth.UserMFA{
			ID: s.newID(), TenantID: tenantID, UserID: userID, SecretEnc: sealed,
		}); err != nil {
			return "", "", fmt.Errorf("services.AuthService.MFAEnroll: create: %w", err)
		}
	} else {
		if err := s.mfaRepo.UpdateSecret(ctx, tenantID, userID, sealed); err != nil {
			return "", "", fmt.Errorf("services.AuthService.MFAEnroll: update: %w", err)
		}
	}

	// Reviewer LOW-1 (dual-review): a fully successful enroll resets the shared attempt
	// counter, exactly like activate/disable do on their own success. Safe here specifically
	// because (a) enroll cannot succeed while MFA is already enabled — the ErrMFAAlreadyEnabled
	// branch above returns before this point, so there is no "reset gives a disable budget"
	// path — and (b) a caller who just proved the password AND received a freshly generated
	// secret has nothing further to gain from a refreshed activate-guessing budget (they still
	// need a valid TOTP code, which resetting this counter does not help produce).
	s.resetMFAAttempts(tenantID, userID)

	return rawSecret, url, nil
}

// MFAActivate confirms an enrollment: the presented code must be a valid CURRENT-window TOTP
// code (backup codes are not accepted here — they do not exist until activation succeeds). On
// success it atomically flips enabled=1 (CAS-guarded — a concurrent double-submit can only
// activate once), generates mfaBackupCodeCount fresh single-use backup codes, persists their
// hashes, and returns the PLAINTEXT codes to the caller exactly once (they are never
// recoverable again). Emits notify.AlertMFAEnabled (best-effort, fail-open).
//
// MFA-STEPUP: the caller must re-prove the account password IN ADDITION to the TOTP code (see
// verifyCallerPassword). The password check runs inside the limiter-guarded region, BEFORE any
// MFA state is loaded, so a wrong password reveals nothing about enrollment state.
func (s *AuthService) MFAActivate(ctx context.Context, tenantID, userID, password, code string) ([]string, error) {
	if s.mfaRepo == nil {
		return nil, ErrMFAUnavailable
	}
	if password == "" {
		return nil, invalid("MFA_PASSWORD_REQUIRED", "password là bắt buộc")
	}
	// Reviewer MED-1 / security review 🟡#4: the per-account failed-code throttle runs FIRST,
	// before touching the repo or crypto at all (ACL/guard-before-business-logic discipline) —
	// a caller already over threshold is rejected without the presented code ever being
	// evaluated, regardless of whether it would have been correct.
	if !s.allowMFAAttempt(tenantID, userID) {
		return nil, ErrMFATooManyAttempts
	}

	u, err := s.verifyCallerPassword(ctx, tenantID, userID, password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("services.AuthService.MFAActivate: %w", err)
	}

	row, err := s.mfaRepo.Get(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, auth.ErrMFANotEnrolled) {
			return nil, auth.ErrMFANotEnrolled
		}
		return nil, fmt.Errorf("services.AuthService.MFAActivate: load mfa: %w", err)
	}
	if row.Enabled {
		return nil, auth.ErrMFAAlreadyEnabled
	}

	secret, err := s.openMFASecret(row.SecretEnc)
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.MFAActivate: %w", err)
	}
	step, ok := matchTOTPCode(secret, code, s.now())
	if !ok {
		s.recordMFAFailure(tenantID, userID)
		return nil, ErrMFACodeInvalid
	}
	s.resetMFAAttempts(tenantID, userID)

	// Generate the backup codes BEFORE attempting the flip so a CSPRNG failure aborts cleanly
	// without touching the database at all. The flip (enabled=0 -> 1) and the backup-code
	// insert are then committed together in ONE transaction (reviewer LOW-1 fix): if the insert
	// fails, the flip rolls back too, so the row is guaranteed to still read enabled=0 and the
	// caller can simply retry — see auth.UserMFARepository.ActivateWithStepAndBackupCodes's doc
	// for why the pre-fix two-independent-writes shape could strand an account MFA-enabled with
	// zero backup codes.
	rawCodes, err := generateBackupCodes()
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.MFAActivate: generate backup codes: %w", err)
	}
	rows := make([]*auth.MFABackupCode, 0, len(rawCodes))
	for _, c := range rawCodes {
		rows = append(rows, &auth.MFABackupCode{
			ID: s.newID(), TenantID: tenantID, UserID: userID, CodeHash: auth.HashRefreshToken(c),
		})
	}

	activated, err := s.mfaRepo.ActivateWithStepAndBackupCodes(ctx, tenantID, userID, step, rows)
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.MFAActivate: activate: %w", err)
	}
	if !activated {
		// Lost a race against a concurrent activate (or the row flipped to enabled between our
		// Get above and this CAS) — the caller sees the same conflict a fresh Get would report.
		return nil, auth.ErrMFAAlreadyEnabled
	}

	// u was already loaded (and password-verified) by verifyCallerPassword above — reuse it
	// for the alert snapshot instead of a second query.
	s.enqueueAlert(ctx, notify.SecurityEvent{
		Type: notify.AlertMFAEnabled, TenantID: tenantID, UserID: userID, Email: u.Email, At: s.now(),
	})

	return rawCodes, nil
}

// MFADisable verifies the presented code (TOTP or backup, either accepted) and, on success,
// hard-deletes the user_mfa row and every backup code for the account. Disable is a genuine
// secret-removal operation — see auth.UserMFARepository.Delete's doc for why this is a hard
// delete rather than the usual soft-delete convention. Emits notify.AlertMFADisabled
// (best-effort, fail-open) on success.
//
// MFA-STEPUP: the caller must re-prove the account password IN ADDITION to the TOTP/backup
// code (see verifyCallerPassword) — a stolen access token alone can no longer strip the
// second factor off an account. Password check inside the limiter-guarded region, before any
// MFA state is loaded.
func (s *AuthService) MFADisable(ctx context.Context, tenantID, userID, password, code string) error {
	if s.mfaRepo == nil {
		return ErrMFAUnavailable
	}
	if password == "" {
		return invalid("MFA_PASSWORD_REQUIRED", "password là bắt buộc")
	}
	// Reviewer MED-1 / security review 🟡#4: same guard-before-business-logic throttle as
	// MFAActivate — see its comment for the rationale (PROTECTED endpoint, no RM-15 coverage).
	if !s.allowMFAAttempt(tenantID, userID) {
		return ErrMFATooManyAttempts
	}

	u, err := s.verifyCallerPassword(ctx, tenantID, userID, password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			return ErrInvalidCredentials
		}
		return fmt.Errorf("services.AuthService.MFADisable: %w", err)
	}

	row, err := s.mfaRepo.Get(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, auth.ErrMFANotEnrolled) {
			return auth.ErrMFANotEnrolled
		}
		return fmt.Errorf("services.AuthService.MFADisable: load mfa: %w", err)
	}

	ok, err := s.verifyMFACode(ctx, tenantID, userID, row.SecretEnc, code)
	if err != nil {
		return fmt.Errorf("services.AuthService.MFADisable: %w", err)
	}
	if !ok {
		s.recordMFAFailure(tenantID, userID)
		return ErrMFACodeInvalid
	}
	s.resetMFAAttempts(tenantID, userID)

	if err := s.mfaRepo.Delete(ctx, tenantID, userID); err != nil {
		return fmt.Errorf("services.AuthService.MFADisable: delete mfa row: %w", err)
	}
	if err := s.mfaBackup.DeleteAllForUser(ctx, tenantID, userID); err != nil {
		return fmt.Errorf("services.AuthService.MFADisable: delete backup codes: %w", err)
	}
	// Reviewer LOW-2 hygiene fix: also purge any still-live two-step-login bridge tokens for
	// this user. Not a security hole even without this — CompleteMFALogin re-checks
	// mfa.Enabled and rejects with auth.ErrMFAPendingInvalid once the row above is gone — but a
	// disabled account should not leave orphaned pending-login rows lying around.
	if err := s.mfaPending.DeleteAllForUser(ctx, tenantID, userID); err != nil {
		return fmt.Errorf("services.AuthService.MFADisable: delete pending tokens: %w", err)
	}

	// u was already loaded (and password-verified) by verifyCallerPassword above — reuse it
	// for the alert snapshot instead of a second query.
	s.enqueueAlert(ctx, notify.SecurityEvent{
		Type: notify.AlertMFADisabled, TenantID: tenantID, UserID: userID, Email: u.Email, At: s.now(),
	})

	return nil
}

// CompleteMFALogin is step 2 of the two-step login flow: it redeems a pendingToken minted by
// Login (MFA branch) for a real token pair, gated on a correct TOTP or backup code.
//
// Flow (../specs/003-mfa-totp/spec.md §"#7", order is load-bearing, mirrors ResetPassword):
//  1. Tenant-agnostic FindByHash to locate the pending row and derive tenant_id FROM it — the
//     request body carries {pendingToken, code} with no tenant.
//  2. Load the MFA row and verify the presented code (TOTP-then-backup). A WRONG code returns
//     ErrMFACodeInvalid WITHOUT consuming the pendingToken, so a mistyped code can be retried
//     within the same 5-minute window (RM-15's /auth/mfa/* rate limiter bounds guess attempts).
//  3. Only on a CORRECT code: atomically consume the pendingToken (single-use CAS) — a race
//     loser (already-consumed) collapses to auth.ErrMFAPendingInvalid.
//  4. Mint + store a token pair exactly like Login's non-MFA success path.
func (s *AuthService) CompleteMFALogin(ctx context.Context, pendingToken, code string) (*LoginOutput, error) {
	if s.mfaRepo == nil {
		return nil, ErrMFAUnavailable
	}

	raw := strings.TrimSpace(pendingToken)
	if raw == "" {
		return nil, auth.ErrMFAPendingInvalid
	}

	tokenHash := auth.HashRefreshToken(raw)
	pt, err := s.mfaPending.FindByHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, auth.ErrMFAPendingInvalid) {
			return nil, auth.ErrMFAPendingInvalid
		}
		return nil, fmt.Errorf("services.AuthService.CompleteMFALogin: find pending token: %w", err)
	}
	// Belt-and-suspenders: FindByHash is tenant-agnostic; a pending row with UsedAt already set
	// or ExpiresAt in the past must be rejected here even before ConsumeByHash runs (a benign
	// race window between this read and the CAS below still resolves correctly because
	// ConsumeByHash re-checks both conditions atomically).
	if pt.UsedAt != nil || !pt.ExpiresAt.After(s.now()) {
		return nil, auth.ErrMFAPendingInvalid
	}

	mfa, err := s.mfaRepo.Get(ctx, pt.TenantID, pt.UserID)
	if err != nil {
		// MFA was disabled (or never existed) after the pending token was issued — the pending
		// token's precondition no longer holds. Treat as invalid-pending (not code-invalid): the
		// caller cannot distinguish this from a stale/expired token either way.
		return nil, auth.ErrMFAPendingInvalid
	}
	if !mfa.Enabled {
		return nil, auth.ErrMFAPendingInvalid
	}

	ok, err := s.verifyMFACode(ctx, pt.TenantID, pt.UserID, mfa.SecretEnc, code)
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.CompleteMFALogin: %w", err)
	}
	if !ok {
		return nil, ErrMFACodeInvalid
	}

	consumed, err := s.mfaPending.ConsumeByHash(ctx, pt.TenantID, tokenHash)
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.CompleteMFALogin: consume pending token: %w", err)
	}
	if !consumed {
		// Lost a race against a concurrent completion of the SAME pending token.
		return nil, auth.ErrMFAPendingInvalid
	}

	u, err := s.users.GetUserByID(ctx, pt.TenantID, pt.UserID)
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.CompleteMFALogin: load user: %w", err)
	}
	if u.Status != statusActive {
		return nil, auth.ErrMFAPendingInvalid
	}

	sub := auth.Subject{UserID: u.ID, TenantID: u.TenantID, IsAdmin: u.IsAdmin}
	if u.RoleID != nil {
		sub.RoleID = *u.RoleID
	}
	access, refresh, err := s.issueTokenPair(ctx, sub, nil)
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.CompleteMFALogin: %w", err)
	}

	lu := &LoginUser{ID: u.ID, TenantID: u.TenantID, Email: u.Email, FullName: u.FullName, IsAdmin: u.IsAdmin}
	if u.RoleID != nil {
		lu.RoleID = *u.RoleID
	}
	return &LoginOutput{AccessToken: access, RefreshToken: refresh, User: lu}, nil
}
