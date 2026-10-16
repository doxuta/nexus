package config

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// credentialKeySize is the decoded length of every named-credential master key: 32 bytes (AES-256).
const credentialKeySize = 32

// defaultCredentialKeyID labels CREDENTIAL_ENC_KEY when CREDENTIAL_ENC_KEY_ID is unset. Every
// environment may keep it: crypto.NewKeyring appends a fingerprint of the key itself, so two
// different keys labelled "v1" still get two different ids.
const defaultCredentialKeyID = "v1"

// credentialKeyIDPattern bounds a master-key LABEL: crypto.NewKeyring turns it into the id
// "<label>.<8 hex fingerprint>", embedded in every envelope ("v1:<id>:…", so never a colon) and
// stored in sys_named_credentials.key_id VARCHAR(32) — hence at most 32-9 = 23 characters.
var credentialKeyIDPattern = regexp.MustCompile(`^[a-z0-9]{1,23}$`)

// CredentialConfig is the named-credential vault's master keyring material
// (../specs/050-named-credentials plan D-2). A DEDICATED key, never shared with SSO/MFA: one leak
// must not open two unrelated features' secrets, and each rotates on its own schedule.
//
// Like SSO/MFA, the PRESENCE of CREDENTIAL_ENC_KEY is the on/off switch: absent ⇒ Enabled=false,
// the server boots with the vault inert (API 503 CREDENTIALS_NOT_CONFIGURED, webhooks that use a
// key send nothing). Present but malformed ⇒ LoadCredentialConfig fails and boot stops, naming the
// variable — a silently ignored key would seal nothing and send nothing.
type CredentialConfig struct {
	Enabled bool
	KeyID   string // CREDENTIAL_ENC_KEY_ID label, default "v1" (the sealing id adds a key fingerprint)
	Key     []byte // exactly 32 raw bytes when Enabled; nil otherwise
	// Retired are decrypt-only keys by label (CREDENTIAL_ENC_RETIRED_KEYS="id:b64,id:b64") kept
	// through a master-key rotation until every row is resealed under Key (S7).
	Retired map[string][]byte
	// AllowInsecureHTTP (CREDENTIAL_ALLOW_INSECURE_HTTP) accepts plain-http base URLs to
	// non-loopback hosts — dev / internal docker network only, NEVER in production.
	AllowInsecureHTTP bool
}

// LoadCredentialConfig reads CREDENTIAL_ENC_KEY, CREDENTIAL_ENC_KEY_ID, CREDENTIAL_ENC_RETIRED_KEYS
// and CREDENTIAL_ALLOW_INSECURE_HTTP. Errors name the variable and never echo key material.
func LoadCredentialConfig() (*CredentialConfig, error) {
	insecure, err := parseBool("CREDENTIAL_ALLOW_INSECURE_HTTP", getEnv("CREDENTIAL_ALLOW_INSECURE_HTTP", ""), false)
	if err != nil {
		return nil, fmt.Errorf("config.LoadCredentialConfig: %w", err)
	}
	rawRetired := strings.TrimSpace(getEnv("CREDENTIAL_ENC_RETIRED_KEYS", ""))

	raw := strings.TrimSpace(getEnv("CREDENTIAL_ENC_KEY", ""))
	if raw == "" {
		if rawRetired != "" {
			// Half a rotation: the active key was removed but the retired list kept. Booting inert
			// would silently stop every webhook that uses a key — refuse instead.
			return nil, fmt.Errorf("config.LoadCredentialConfig: CREDENTIAL_ENC_RETIRED_KEYS is set but CREDENTIAL_ENC_KEY is empty — set the active key too")
		}
		return &CredentialConfig{Enabled: false, AllowInsecureHTTP: insecure}, nil
	}
	key, err := decodeCredentialKey(raw)
	if err != nil {
		return nil, fmt.Errorf("config.LoadCredentialConfig: CREDENTIAL_ENC_KEY %w", err)
	}

	keyID := strings.TrimSpace(getEnv("CREDENTIAL_ENC_KEY_ID", defaultCredentialKeyID))
	if !credentialKeyIDPattern.MatchString(keyID) {
		return nil, fmt.Errorf("config.LoadCredentialConfig: CREDENTIAL_ENC_KEY_ID (%d chars) must match [a-z0-9]{1,23}; its value is not echoed in case it is key material", len(keyID))
	}

	retired := map[string][]byte{}
	if rawRetired != "" {
		for _, entry := range strings.Split(rawRetired, ",") {
			id, b64, ok := strings.Cut(strings.TrimSpace(entry), ":")
			if !ok || !credentialKeyIDPattern.MatchString(id) {
				return nil, fmt.Errorf("config.LoadCredentialConfig: CREDENTIAL_ENC_RETIRED_KEYS entries must be <id>:<base64 key> with id matching [a-z0-9]{1,23}")
			}
			if id == keyID {
				return nil, fmt.Errorf("config.LoadCredentialConfig: CREDENTIAL_ENC_RETIRED_KEYS id %q repeats CREDENTIAL_ENC_KEY_ID", id)
			}
			if _, dup := retired[id]; dup {
				return nil, fmt.Errorf("config.LoadCredentialConfig: CREDENTIAL_ENC_RETIRED_KEYS lists id %q twice", id)
			}
			k, err := decodeCredentialKey(b64)
			if err != nil {
				return nil, fmt.Errorf("config.LoadCredentialConfig: CREDENTIAL_ENC_RETIRED_KEYS id %q %w", id, err)
			}
			retired[id] = k
		}
	}
	return &CredentialConfig{Enabled: true, KeyID: keyID, Key: key, Retired: retired, AllowInsecureHTTP: insecure}, nil
}

// decodeCredentialKey decodes one base64 AES-256 key. The error wording completes a sentence
// that starts with the variable name, and never carries the value.
func decodeCredentialKey(b64 string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("is not valid base64 (generate with `openssl rand -base64 32`): %w", err)
	}
	if len(key) != credentialKeySize {
		return nil, fmt.Errorf("must decode to %d bytes (AES-256), got %d", credentialKeySize, len(key))
	}
	return key, nil
}
