package services

// MFA-ENFORCE — close the fail-open where losing MFA_SECRET_ENC_KEY silently downgrades
// MFA-enrolled accounts to one-step login.
//
// Before this slice, an ABSENT encryption key meant main.go never called WithMFA, so
// AuthService.mfaRepo stayed nil and Login's MFA branch was skipped ENTIRELY — an enrolled
// user logged in one-step with zero signal. The /auth/mfa/* endpoints correctly answered 503,
// but the LOGIN gate (the one that actually protects the account) vanished with the key.
//
// The fix has two parts:
//  1. The ENROLLMENT READER is always wired (WithMFAEnrollmentReader, called unconditionally
//     from main.go): checking "is this account enrolled?" needs only the DB — user_mfa.enabled
//     is a plain column; openMFASecret/decryption only runs in VERIFY paths. So the reader is
//     constructible and correct even when the AES key is gone.
//  2. Login (and SSO's found-identity path — both flow through mfaLoginGate below) fail
//     CLOSED for an enrolled account when full MFA support is unavailable: 503 MFA_UNAVAILABLE
//     with an honest message, never a silent one-step login. MFA_FAIL_OPEN=true (config,
//     default false) is the documented INFRA-EMERGENCY override — it allows the one-step login
//     but logs zap.Error on EVERY such login so the downgrade is impossible to miss.
//
// Deliberate scope: /auth/refresh is NOT gated here — refresh exchanges an EXISTING session
// (whose creation already passed this gate, or predates enrollment); the second factor is a
// login-time control, and re-checking enrollment on every refresh would add a DB read per
// refresh without closing any new-session path. See Login's comment marker.

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

// ErrMFAEnrolledButUnavailable is returned by the login-time MFA gate (mfaLoginGate) when the
// account IS enrolled in MFA but the server cannot run the second step (no sealer — the
// MFA_SECRET_ENC_KEY is absent/lost). Handlers map it to 503 MFA_UNAVAILABLE with an honest
// "the system is missing its MFA key" message: the credential was CORRECT, the account is
// fine — the SERVER is degraded, so 5xx (not 401) is the truthful status family, and blocking
// (not downgrading to one-step) is the only safe default.
var ErrMFAEnrolledButUnavailable = errors.New("auth: account enrolled in mfa but mfa support unavailable")

// mfaEnrollmentReader is the minimal read-only capability needed to answer "does this account
// have MFA enabled?". It is the crypto-free subset of mfaRepo: Get returns the user_mfa row
// (auth.ErrMFANotEnrolled when none) without ever touching the sealed secret, so a repo
// satisfying it can be constructed with only a DB handle — no AES key required.
type mfaEnrollmentReader interface {
	Get(ctx context.Context, tenantID, userID string) (*auth.UserMFA, error)
}

// WithMFAEnrollmentReader injects the always-available enrollment reader plus the
// MFA_FAIL_OPEN emergency flag (MFA-ENFORCE). main.go calls this UNCONDITIONALLY — key present
// or not — because the reader needs only the database. A nil reader leaves the field unset
// ("enrollment unknowable — skip the gate"), which is what keeps every existing test fixture
// that builds a bare 3-arg NewAuthService byte-identical: for them, both mfaRepo and
// mfaEnrollment are nil, so mfaLoginGate is a no-op exactly like the pre-slice nil-mfaRepo
// skip. PRODUCTION wiring always passes a real reader, so production can never be in the
// "unknowable" state.
func (s *AuthService) WithMFAEnrollmentReader(r mfaEnrollmentReader, failOpen bool) *AuthService {
	if r == nil {
		return s
	}
	s.mfaEnrollment = r
	s.mfaFailOpen = failOpen
	return s
}

// mfaEnrollmentSource picks the enrollment-check source: the full mfaRepo when MFA support is
// wired (it is a superset of the reader), else the always-wired standalone reader, else nil
// (legacy test fixtures — gate skipped).
func (s *AuthService) mfaEnrollmentSource() mfaEnrollmentReader {
	if s.mfaRepo != nil {
		return s.mfaRepo
	}
	return s.mfaEnrollment
}

// MFAGateWired reports whether an enrollment source is present so the login-time fail-closed
// gate is active. main.go asserts this at boot (MFA-ENFORCE MED-1) — a refactor that forgot to
// wire the reader would otherwise silently reopen the fail-open downgrade with every test green.
func (s *AuthService) MFAGateWired() bool {
	return s.mfaEnrollmentSource() != nil
}

// mfaEnabledFor answers "does (tenant,user) currently have MFA enabled?" for read-only
// surfaces (GET /auth/me → LoginUser.MFAEnabled). It uses the SAME crypto-free enrollment
// source as the login gate (mfaEnrollmentSource) and NEVER opens the sealed secret, so it is
// correct even when MFA_SECRET_ENC_KEY is absent. Honest fallbacks: no reader wired (bare test
// fixtures — production always wires one, see MFAGateWired) → false; not-enrolled → false.
// A genuine read fault is returned (not folded into false) so /auth/me can never silently
// claim "MFA off" for an enrolled account while the user_mfa read is failing.
func (s *AuthService) mfaEnabledFor(ctx context.Context, tenantID, userID string) (bool, error) {
	reader := s.mfaEnrollmentSource()
	if reader == nil {
		return false, nil
	}
	mfa, err := reader.Get(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, auth.ErrMFANotEnrolled) {
			return false, nil
		}
		return false, fmt.Errorf("load mfa enrollment: %w", err)
	}
	return mfa.Enabled, nil
}

// mfaLoginGate is the SINGLE login-time MFA decision, shared verbatim by AuthService.Login and
// SSOService.HandleCallback's found-identity branch (SSO parity by construction — a divergent
// copy in sso_service.go was exactly how a bypass would creep back in). It runs strictly AFTER
// the caller has authenticated (password verified / IdP identity resolved) so it can never
// leak enrollment state ahead of a real credential check.
//
// Outcomes:
//   - ("", false, nil): account not enrolled (or enrollment unknowable in bare test fixtures)
//     → caller proceeds with the normal one-step session.
//   - (token, true, nil): enrolled + full MFA support available → caller returns the two-step
//     {mfaPending, pendingToken} shape.
//   - ("", false, ErrMFAEnrolledButUnavailable): enrolled but the server lacks the MFA key
//     (fail-closed, MFA-ENFORCE) → caller surfaces 503 MFA_UNAVAILABLE. With MFA_FAIL_OPEN=true
//     this arm instead logs zap.Error and reports "not enrolled" so login proceeds one-step —
//     the documented emergency downgrade, loud on every use.
//   - ("", false, other error): internal fault (DB error loading the row / minting the pending
//     token) — caller wraps with its own prefix.
func (s *AuthService) mfaLoginGate(ctx context.Context, tenantID, userID string) (pendingToken string, mfaPending bool, err error) {
	reader := s.mfaEnrollmentSource()
	if reader == nil {
		return "", false, nil
	}

	mfa, mfaErr := reader.Get(ctx, tenantID, userID)
	if mfaErr != nil {
		if errors.Is(mfaErr, auth.ErrMFANotEnrolled) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("load mfa: %w", mfaErr)
	}
	if !mfa.Enabled {
		return "", false, nil
	}

	// Enrolled. Full support (sealer + pending-token store) is present exactly when mfaRepo is
	// non-nil — WithMFA refuses partial wiring, so this one check covers the whole capability set.
	if s.mfaRepo == nil {
		if s.mfaFailOpen {
			s.logger.Error("services.AuthService.mfaLoginGate: MFA fail-open active — enrolled user logged in WITHOUT MFA (MFA_FAIL_OPEN=true, MFA_SECRET_ENC_KEY absent)",
				zap.String("tenant_id", tenantID),
				zap.String("user_id", userID),
			)
			return "", false, nil
		}
		s.logger.Error("services.AuthService.mfaLoginGate: MFA-enrolled account blocked at login — MFA support unavailable (MFA_SECRET_ENC_KEY absent); fail-closed (MFA-ENFORCE)",
			zap.String("tenant_id", tenantID),
			zap.String("user_id", userID),
		)
		return "", false, ErrMFAEnrolledButUnavailable
	}

	token, issueErr := s.issueMFAPending(ctx, tenantID, userID)
	if issueErr != nil {
		return "", false, issueErr
	}
	return token, true, nil
}
