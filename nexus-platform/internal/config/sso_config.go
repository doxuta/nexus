package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// ssoKeySize is the required decoded length of SSO_SECRET_ENC_KEY: 32 bytes (AES-256).
const ssoKeySize = 32

// defaultSSOKeyID is used when SSO_SECRET_ENC_KEY_ID is unset — a single-key deployment never
// needs to name its key explicitly (mirrors config.MFAConfig's defaultMFAKeyID).
const defaultSSOKeyID = "v1"

// SSOConfig holds the verified SSO/OIDC configuration read from the environment
// (../specs/004-sso-oidc/spec.md §"#8"). ONE key backs BOTH the OAuth2 client_secret-at-rest
// sealing (sso_providers.client_secret_enc) AND the server-side signed state used to carry
// {tenant, provider, nonce, expiry} opaquely through the IdP redirect round-trip — the brief's
// "reuse MFA_SECRET_ENC_KEY OR a dedicated key" choice is resolved here as a DEDICATED
// SSO_SECRET_ENC_KEY (not shared with MFA), since coupling two unrelated features' at-rest
// secrets to the same key would make a future independent rotation of either one harder.
//
// Like MFAConfig, there is no separate "enabled" boolean: the mere PRESENCE of
// SSO_SECRET_ENC_KEY is the on/off switch (absent -> SSO support left entirely inert, every
// SSO endpoint answers 503 SSO_NOT_CONFIGURED; boot never crashes). A key that IS present but
// malformed (not base64, wrong length) fails boot fast — mirrors LoadMFAConfig.
//
// SSO_REDIRECT_BASE is the backend's own public base URL the callback redirect_uri is built
// from: "<RedirectBase>/auth/sso/callback". Providers live in the DATABASE (created via the
// admin API after boot), not in static config, so there is no boot-time way to know whether
// "a provider exists" — LoadSSOConfig approximates the RESET_URL_BASE MUST-FIX 6 fail-fast
// (../specs/002-password-reset/spec.md §"#1") by treating "key configured" as the enabling condition: if
// an operator has gone to the trouble of provisioning SSO_SECRET_ENC_KEY, SSO_REDIRECT_BASE is
// required too, so a half-configured deployment fails at boot rather than admin-creating a
// provider that can never build a valid redirect_uri.
type SSOConfig struct {
	Enabled      bool
	KeyID        string
	Key          []byte // exactly 32 raw bytes when Enabled; nil otherwise
	RedirectBase string
	// FrontendBase (SSO_FRONTEND_BASE, SSO-FE-REDIRECT) is the frontend app's base URL the
	// /callback redirects the browser to after a successful identity resolution:
	// "<FrontendBase>/auth/sso/complete?code=…" (success) / "<FrontendBase>/login?ssoError=…"
	// (failure). OPTIONAL and backward-compatible: when empty, /callback keeps returning the
	// legacy JSON LoginOutput (no redirect) — so this slice is opt-in and never breaks an
	// existing SSO deployment that has not set it. When present it must be an absolute http(s)
	// URL (validated at boot).
	FrontendBase string
}

// LoadSSOConfig reads SSO_SECRET_ENC_KEY (base64-encoded 32-byte AES-256 key), the optional
// SSO_SECRET_ENC_KEY_ID label, and SSO_REDIRECT_BASE from the environment.
func LoadSSOConfig() (*SSOConfig, error) {
	raw := strings.TrimSpace(getEnv("SSO_SECRET_ENC_KEY", ""))
	if raw == "" {
		return &SSOConfig{Enabled: false}, nil
	}

	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("config.LoadSSOConfig: SSO_SECRET_ENC_KEY is not valid base64: %w", err)
	}
	if len(key) != ssoKeySize {
		return nil, fmt.Errorf("config.LoadSSOConfig: SSO_SECRET_ENC_KEY must decode to %d bytes (AES-256), got %d", ssoKeySize, len(key))
	}

	keyID := strings.TrimSpace(getEnv("SSO_SECRET_ENC_KEY_ID", defaultSSOKeyID))
	if keyID == "" {
		keyID = defaultSSOKeyID
	}

	redirectBase := strings.TrimSpace(getEnv("SSO_REDIRECT_BASE", ""))
	if redirectBase == "" {
		return nil, fmt.Errorf("config.LoadSSOConfig: SSO_REDIRECT_BASE is required when SSO_SECRET_ENC_KEY is configured")
	}
	// Fail fast on a malformed base rather than silently deriving a broken callback
	// redirect_uri / sso_state cookie Path (SSO-STATE-COOKIE review NIT-1): it must be an
	// absolute http(s) URL with a host, otherwise every callback dies at the IdP or as a
	// hard-to-diagnose 403 SSO_STATE_MISMATCH instead of a clear boot error.
	if u, perr := url.Parse(redirectBase); perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("config.LoadSSOConfig: SSO_REDIRECT_BASE must be an absolute http(s) URL with a host, got %q", redirectBase)
	}

	// SSO_FRONTEND_BASE (SSO-FE-REDIRECT) — optional; empty leaves /callback in legacy JSON mode.
	frontendBase := strings.TrimSpace(getEnv("SSO_FRONTEND_BASE", ""))
	if frontendBase != "" {
		if u, perr := url.Parse(frontendBase); perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("config.LoadSSOConfig: SSO_FRONTEND_BASE must be an absolute http(s) URL with a host, got %q", frontendBase)
		}
		frontendBase = strings.TrimRight(frontendBase, "/")
	}

	return &SSOConfig{Enabled: true, KeyID: keyID, Key: key, RedirectBase: redirectBase, FrontendBase: frontendBase}, nil
}
