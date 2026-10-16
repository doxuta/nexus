// Package crypto provides AES-256-GCM authenticated encryption for secrets that must be
// stored ENCRYPTED (reversible), not hashed — e.g. a TOTP seed (ENG-AUTH2 #7) or an OIDC
// client_secret (ENG-AUTH2 #8, ../specs/004-sso-oidc/spec.md). A one-way hash cannot support
// these use cases because the plaintext must be recovered later to run the verification
// algorithm (TOTP) or to authenticate against a third party (OIDC token exchange).
//
// The sealed envelope format is versioned and self-describing so a future key rotation can
// introduce a new key under a new key id while old ciphertext (still tagged with the
// previous key id) stays decryptable by whichever sealer still holds that key:
//
//	v1:<keyID>:<base64(nonce || ciphertext)>
//
// A fresh random nonce (crypto/rand) is generated on every Seal call — GCM's security bound
// depends on the (key, nonce) pair never repeating.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// sealFormatVersion is the envelope format version prefix. Bumped only if the envelope
// SHAPE changes (e.g. a different AEAD); it is independent of key rotation, which is
// tracked by keyID instead.
const sealFormatVersion = "v1"

// aes256KeySize is the required raw key length in bytes for AES-256.
const aes256KeySize = 32

// AESGCM seals and opens secrets with a single AES-256-GCM key, tagged with a keyID so the
// resulting envelope is self-describing (../specs/003-mfa-totp/spec.md §"#7" MUST-FIX 1). It
// holds no I/O state and is safe for concurrent use (crypto/cipher.AEAD implementations are
// safe for concurrent use once constructed).
type AESGCM struct {
	keyID string
	aead  cipher.AEAD
}

// New builds an AESGCM sealer for one (keyID, key) pair. key32 MUST be exactly 32 bytes
// (AES-256); keyID MUST be non-empty (it is embedded in every envelope Seal produces, and
// checked on every Open). A caller that must decrypt envelopes sealed under several historical
// keys uses Keyring (keyring.go), which dispatches Open by the envelope's keyID segment.
func New(keyID string, key32 []byte) (*AESGCM, error) {
	if strings.TrimSpace(keyID) == "" {
		return nil, fmt.Errorf("crypto.New: keyID must not be empty")
	}
	if len(key32) != aes256KeySize {
		return nil, fmt.Errorf("crypto.New: key must be %d bytes (AES-256), got %d", aes256KeySize, len(key32))
	}
	block, err := aes.NewCipher(key32)
	if err != nil {
		return nil, fmt.Errorf("crypto.New: init AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto.New: init GCM mode: %w", err)
	}
	return &AESGCM{keyID: keyID, aead: gcm}, nil
}

// KeyID reports the key identifier this sealer was constructed with.
func (c *AESGCM) KeyID() string { return c.keyID }

// Seal encrypts plaintext with a fresh crypto/rand nonce and returns the versioned,
// self-describing envelope string ("v1:<keyID>:<base64(nonce||ciphertext)>"). Two calls on
// identical plaintext never produce identical output (random nonce per call).
func (c *AESGCM) Seal(plaintext []byte) (string, error) { return c.SealAAD(plaintext, nil) }

// Open decrypts an envelope produced by Seal on a sealer holding THIS key. It returns a
// non-nil error (never panics) on: a malformed envelope (wrong shape/version), a keyID that
// does not match this sealer's own keyID, a payload that fails base64 decoding or is too
// short to contain a nonce, or a GCM authentication failure (tampered ciphertext or a wrong
// key presented under the same keyID label).
func (c *AESGCM) Open(sealed string) ([]byte, error) { return c.OpenAAD(sealed, nil) }

// SealAAD is Seal with GCM additional data bound into the tag (../specs/050-named-credentials
// plan D-3): the envelope then opens ONLY under the same aad, so a ciphertext copied onto
// another row, tenant or slot fails authentication instead of decrypting there. The aad is not
// stored — the caller rebuilds it from the row. nil aad is GCM's empty additional data, which is
// why Seal/Open (TOTP seeds, SSO client secrets) stay byte-compatible.
func (c *AESGCM) SealAAD(plaintext, aad []byte) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("crypto.AESGCM.SealAAD: read nonce: %w", err)
	}
	ciphertext := c.aead.Seal(nil, nonce, plaintext, aad)
	payload := make([]byte, 0, len(nonce)+len(ciphertext))
	payload = append(payload, nonce...)
	payload = append(payload, ciphertext...)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return sealFormatVersion + ":" + c.keyID + ":" + encoded, nil
}

// OpenAAD opens an envelope produced by SealAAD with the same aad (see Open for the error cases;
// a different aad is a GCM authentication failure).
func (c *AESGCM) OpenAAD(sealed string, aad []byte) ([]byte, error) {
	parts := strings.SplitN(sealed, ":", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("crypto.AESGCM.OpenAAD: malformed envelope (want 3 colon-separated segments)")
	}
	version, keyID, encoded := parts[0], parts[1], parts[2]
	if version != sealFormatVersion {
		return nil, fmt.Errorf("crypto.AESGCM.OpenAAD: unsupported envelope version %q", version)
	}
	if keyID != c.keyID {
		return nil, fmt.Errorf("crypto.AESGCM.OpenAAD: key id mismatch (envelope=%q, sealer=%q)", keyID, c.keyID)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("crypto.AESGCM.OpenAAD: decode payload: %w", err)
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize {
		return nil, fmt.Errorf("crypto.AESGCM.OpenAAD: payload shorter than nonce size")
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("crypto.AESGCM.OpenAAD: decrypt/authenticate: %w", err)
	}
	return plaintext, nil
}
