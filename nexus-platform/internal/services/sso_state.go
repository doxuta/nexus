package services

import (
	"encoding/json"
	"fmt"
	"time"
)

// ssoStateTTL bounds how long a /start-issued state envelope stays acceptable at /callback —
// long enough for a human to complete an interactive IdP login (credentials, consent, maybe a
// second factor at the IdP), short enough that a captured-but-unused redirect URL is not a
// standing replay window. Mirrors the MFA pendingToken's TTL discipline (5 min there; SSO gets
// a slightly longer budget since the round-trip involves a THIRD PARTY's login UI, not just the
// caller re-typing a code).
const ssoStateTTL = 10 * time.Minute

// SSOStateTTL is the exported view of ssoStateTTL for the HTTP layer: the browser-session bind
// cookie set at /start (SSO-STATE-COOKIE, handlers/sso_handler.go) must expire together with the
// sealed state it binds — one constant, two enforcement points (cookie Max-Age here, ExpiresAt
// inside the sealed envelope), so they can never drift apart.
const SSOStateTTL = ssoStateTTL

// ssoSealer is the consumer-side view of internal/crypto.AESGCM the SSO flows need: seal the
// {tenant, provider, nonce, expiry} state envelope opaquely into the `state` query param carried
// through the IdP redirect round-trip, and seal/open the OAuth2 client_secret at rest. Mirrors
// mfaSealer's interface shape (auth_mfa.go) so both features depend on the same narrow contract
// rather than the concrete internal/crypto.AESGCM type.
type ssoSealer interface {
	Seal(plaintext []byte) (string, error)
	Open(sealed string) ([]byte, error)
}

// ssoState is the server-side-signed payload carried opaquely through the OIDC authorization
// redirect round-trip. It is the SINGLE SOURCE OF TRUTH for which tenant and provider a
// /callback request belongs to — the callback endpoint deliberately accepts no tenant/provider
// from its own query string (../specs/004-sso-oidc/spec.md §"#8" MUST-FIX: "callback KHÔNG được đọc
// tenant từ query, phải lấy từ state đã ký"). Sealing it with AES-256-GCM (not just HMAC-signing)
// also keeps the payload's shape opaque to the browser/IdP hop, though confidentiality is not
// the primary property being relied on here — tamper-evidence and non-forgeability are.
type ssoState struct {
	TenantID   string    `json:"tenantId"`
	ProviderID string    `json:"providerId"`
	Nonce      string    `json:"nonce"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// sealState serializes and seals st into the opaque string placed in the `state` query param.
func (s *SSOService) sealState(st ssoState) (string, error) {
	if s.sealer == nil {
		return "", fmt.Errorf("services.SSOService.sealState: %w", ErrSSOUnavailable)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("services.SSOService.sealState: marshal: %w", err)
	}
	sealed, err := s.sealer.Seal(raw)
	if err != nil {
		return "", fmt.Errorf("services.SSOService.sealState: seal: %w", err)
	}
	return sealed, nil
}

// openState reverses sealState AND enforces the expiry: it returns ErrSSOStateInvalid-wrapped
// errors uniformly for every failure mode (malformed envelope, GCM authentication failure /
// tamper, wrong-key, unmarshal failure, or an expired-but-otherwise-valid envelope) so the
// caller (HandleCallback) cannot distinguish "tampered" from "expired" from "stale key" — all of
// them collapse to the same 400 SSO_STATE_INVALID at the HTTP layer (anti-probe, mirrors the
// resetToken / mfaPendingToken sentinel-folding discipline elsewhere in this package).
func (s *SSOService) openState(sealed string) (*ssoState, error) {
	if s.sealer == nil {
		return nil, fmt.Errorf("services.SSOService.openState: %w", ErrSSOUnavailable)
	}
	raw, err := s.sealer.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("services.SSOService.openState: open: %w", err)
	}
	var st ssoState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("services.SSOService.openState: unmarshal: %w", err)
	}
	if !st.ExpiresAt.After(s.clock()) {
		return nil, fmt.Errorf("services.SSOService.openState: state expired at %s", st.ExpiresAt)
	}
	return &st, nil
}
