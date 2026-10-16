package config

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// mfaKeySize is the required decoded length of MFA_SECRET_ENC_KEY: 32 bytes (AES-256).
const mfaKeySize = 32

// defaultMFAKeyID is used when MFA_SECRET_ENC_KEY_ID is unset — a single-key deployment
// never needs to name its key explicitly. A future rotation sets MFA_SECRET_ENC_KEY_ID to a
// new label while historical envelopes keep decrypting under whatever key their embedded
// keyID resolves to (internal/crypto.AESGCM).
const defaultMFAKeyID = "v1"

// MFAConfig holds the verified MFA-at-rest encryption configuration read from the
// environment (../specs/003-mfa-totp/spec.md §"#7" MUST-FIX 1).
//
// UNLIKE AlertsConfig/ResetConfig there is no separate "enabled" boolean env var: the mere
// PRESENCE of MFA_SECRET_ENC_KEY is the on/off switch. This keeps the surface small — MFA is
// off by default (no var set → server boots normally, MFA endpoints answer 503
// MFA_UNAVAILABLE) and turns on the moment an operator provisions a real key. A key that IS
// present but malformed (not base64, or does not decode to exactly 32 bytes) fails boot fast
// — a broken key must never be silently ignored, which would let a half-configured MFA
// surface accept enrollments it can never decrypt back to verify a login.
type MFAConfig struct {
	Enabled bool
	KeyID   string
	Key     []byte // exactly 32 raw bytes when Enabled; nil otherwise
	// FailOpen mirrors MFA_FAIL_OPEN (default false) — the MFA-ENFORCE INFRA-EMERGENCY
	// kill-switch. When the encryption key is ABSENT (Enabled=false) an MFA-ENROLLED account is
	// fail-closed at login (503 MFA_UNAVAILABLE): losing the key must never silently downgrade
	// a two-factor account to one-step login. FailOpen=true is the deliberate operator override
	// for an infrastructure emergency ("key is gone, business must log in NOW"): enrolled users
	// may log in one-step, but every such login is logged at zap.Error so the downgrade is
	// impossible to miss. It has no effect while the key is present (full MFA support wins).
	FailOpen bool
}

// LoadMFAConfig reads MFA_SECRET_ENC_KEY (base64-encoded 32-byte AES-256 key), the optional
// MFA_SECRET_ENC_KEY_ID label, and the MFA_FAIL_OPEN emergency override from the environment.
// A malformed MFA_FAIL_OPEN value fails boot fast (same discipline as a malformed key): a
// mistyped kill-switch must never be silently interpreted as "off" — or "on".
func LoadMFAConfig() (*MFAConfig, error) {
	failOpen, err := parseBool("MFA_FAIL_OPEN", getEnv("MFA_FAIL_OPEN", ""), false)
	if err != nil {
		return nil, fmt.Errorf("config.LoadMFAConfig: %w", err)
	}

	raw := strings.TrimSpace(getEnv("MFA_SECRET_ENC_KEY", ""))
	if raw == "" {
		return &MFAConfig{Enabled: false, FailOpen: failOpen}, nil
	}

	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("config.LoadMFAConfig: MFA_SECRET_ENC_KEY is not valid base64: %w", err)
	}
	if len(key) != mfaKeySize {
		return nil, fmt.Errorf("config.LoadMFAConfig: MFA_SECRET_ENC_KEY must decode to %d bytes (AES-256), got %d", mfaKeySize, len(key))
	}

	keyID := strings.TrimSpace(getEnv("MFA_SECRET_ENC_KEY_ID", defaultMFAKeyID))
	if keyID == "" {
		keyID = defaultMFAKeyID
	}

	return &MFAConfig{Enabled: true, KeyID: keyID, Key: key, FailOpen: failOpen}, nil
}
