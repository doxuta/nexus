package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownKeyID reports an envelope sealed under a master key this keyring does not hold — e.g.
// a DB restored into an environment with a different key, even one configured under the SAME label
// (the key id carries the key's fingerprint, see keyIDFor). ../specs/050-named-credentials US7:
// callers map it to CREDENTIAL_KEY_UNAVAILABLE and never fall back to sending unauthenticated.
var ErrUnknownKeyID = errors.New("crypto: envelope sealed under a key this keyring does not hold")

// ErrNotConfigured reports a call on a nil *Keyring — the "vault inert" value main.go passes when
// CREDENTIAL_ENC_KEY is absent. Seal/Open fail closed with it instead of dereferencing nil.
var ErrNotConfigured = errors.New("crypto: no master key configured")

// keyIDContext domain-separates the key fingerprint from any other use of the master key.
const keyIDContext = "nexus/crypto/keyring/key-id/v1"

// keyIDFor is the id a keyring seals under: "<label>.<first 8 hex of HMAC-SHA256(key,
// keyIDContext)>". A bare operator label is not enough — two environments that both keep the
// default label "v1" over different keys would share an id, so Has would claim a key it cannot
// use and a restored dump would fail GCM generically instead of reading as ErrUnknownKeyID. The
// fingerprint is a PRF output of the key, so it reveals nothing a ciphertext does not already.
// ponytail: 32-bit fingerprint — two keys under one label collide at 2^-32; that residue still
// fails closed (GCM authentication error, never a send).
func keyIDFor(label string, key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(keyIDContext))
	return label + "." + hex.EncodeToString(mac.Sum(nil)[:4])
}

// Keyring is a master-key rotation set: ONE active key seals, every held key (active + retired)
// opens, dispatched by the keyID segment of the envelope. Safe for concurrent use; immutable once
// built. A nil *Keyring is a valid "not configured" value — Has answers false, ActiveID is empty,
// SealAAD/OpenAAD return ErrNotConfigured.
type Keyring struct {
	active *AESGCM
	byID   map[string]*AESGCM
}

// NewKeyring builds a keyring whose active key is (activeLabel, activeKey) and which can still open
// envelopes sealed under each retired label → key. Every id is keyIDFor(label, key), so the id
// names the key material, not just the label. A retired label equal to the active label is
// refused: an operator could not tell which key a "v1" row belongs to.
func NewKeyring(activeLabel string, activeKey []byte, retired map[string][]byte) (*Keyring, error) {
	active, err := New(keyIDFor(activeLabel, activeKey), activeKey)
	if err != nil {
		return nil, fmt.Errorf("crypto.NewKeyring: active key: %w", err)
	}
	k := &Keyring{active: active, byID: map[string]*AESGCM{active.KeyID(): active}}
	for label, key := range retired {
		if label == activeLabel {
			return nil, fmt.Errorf("crypto.NewKeyring: retired key label %q repeats the active key label", label)
		}
		s, err := New(keyIDFor(label, key), key)
		if err != nil {
			return nil, fmt.Errorf("crypto.NewKeyring: retired key %q: %w", label, err)
		}
		k.byID[s.KeyID()] = s
	}
	return k, nil
}

// ActiveID is the key id ("<label>.<fingerprint>") every new envelope is sealed under.
// Empty on a nil keyring (callers seal first, so they never read it).
func (k *Keyring) ActiveID() string {
	if k == nil {
		return ""
	}
	return k.active.KeyID()
}

// Has reports whether the keyring can open envelopes sealed under keyID.
func (k *Keyring) Has(keyID string) bool { return k != nil && k.byID[keyID] != nil }

// SealAAD seals under the active key, bound to aad (see AESGCM.SealAAD).
func (k *Keyring) SealAAD(plaintext, aad []byte) (string, error) {
	if k == nil {
		return "", fmt.Errorf("crypto.Keyring.SealAAD: %w", ErrNotConfigured)
	}
	env, err := k.active.SealAAD(plaintext, aad)
	if err != nil {
		return "", fmt.Errorf("crypto.Keyring.SealAAD: %w", err)
	}
	return env, nil
}

// OpenAAD opens an envelope with whichever held key sealed it; ErrUnknownKeyID when none did. Any
// other error under a held id is a tampered envelope or a different aad (row copied elsewhere) —
// callers fail closed on every error, not only ErrUnknownKeyID.
func (k *Keyring) OpenAAD(envelope string, aad []byte) ([]byte, error) {
	if k == nil {
		return nil, fmt.Errorf("crypto.Keyring.OpenAAD: %w", ErrNotConfigured)
	}
	id, err := KeyIDOf(envelope)
	if err != nil {
		return nil, fmt.Errorf("crypto.Keyring.OpenAAD: %w", err)
	}
	s := k.byID[id]
	if s == nil {
		return nil, fmt.Errorf("crypto.Keyring.OpenAAD: key id %q: %w", id, ErrUnknownKeyID)
	}
	pt, err := s.OpenAAD(envelope, aad)
	if err != nil {
		return nil, fmt.Errorf("crypto.Keyring.OpenAAD: %w", err)
	}
	return pt, nil
}

// KeyIDOf returns the key id segment of a "v1:<keyID>:<payload>" envelope without opening it.
func KeyIDOf(envelope string) (string, error) {
	parts := strings.SplitN(envelope, ":", 3)
	if len(parts) != 3 || parts[0] != sealFormatVersion || parts[1] == "" {
		return "", fmt.Errorf("crypto.KeyIDOf: malformed envelope")
	}
	return parts[1], nil
}
