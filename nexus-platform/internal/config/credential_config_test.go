package config

import (
	"strings"
	"testing"
)

// clearCredentialEnv blanks every named-credential variable so a test never inherits the
// runner's environment (queue lesson "test inherits the env it judges").
func clearCredentialEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"CREDENTIAL_ENC_KEY", "CREDENTIAL_ENC_KEY_ID", "CREDENTIAL_ENC_RETIRED_KEYS", "CREDENTIAL_ALLOW_INSECURE_HTTP"} {
		t.Setenv(k, "")
	}
}

// TestLoadCredentialConfig_AbsentIsInert: no master key ⇒ the server still boots with the vault
// inert (specs/050 FR-011) — the API answers 503 CREDENTIALS_NOT_CONFIGURED from S1b on.
func TestLoadCredentialConfig_AbsentIsInert(t *testing.T) {
	clearCredentialEnv(t)
	cfg, err := LoadCredentialConfig()
	if err != nil {
		t.Fatalf("LoadCredentialConfig() error = %v; want nil (absent key must boot cleanly)", err)
	}
	if cfg.Enabled || cfg.Key != nil || len(cfg.Retired) != 0 {
		t.Fatalf("cfg = %+v; want the vault disabled with no key material", cfg)
	}

	t.Setenv("CREDENTIAL_ENC_KEY", b64Key(32))
	t.Setenv("CREDENTIAL_ENC_RETIRED_KEYS", "old:"+b64Key(32))
	cfg, err = LoadCredentialConfig()
	if err != nil || !cfg.Enabled || cfg.KeyID != "v1" || len(cfg.Key) != 32 || len(cfg.Retired["old"]) != 32 {
		t.Fatalf("valid key + one retired = %+v, %v; want enabled, default id v1, retired[old] decoded", cfg, err)
	}
}

// TestLoadCredentialConfig_MalformedFailsBoot: a key that is present but wrong must stop the
// boot with the variable's name — a silently ignored key would seal nothing and send nothing.
func TestLoadCredentialConfig_MalformedFailsBoot(t *testing.T) {
	for _, tc := range []struct {
		name, key, keyID, retired, wantVar string
	}{
		{"not base64", "abc!!", "", "", "CREDENTIAL_ENC_KEY"},
		{"short key", b64Key(16), "", "", "CREDENTIAL_ENC_KEY"},
		{"long key", b64Key(33), "", "", "CREDENTIAL_ENC_KEY"},
		{"key id with colon", b64Key(32), "v1:x", "", "CREDENTIAL_ENC_KEY_ID"},
		{"key id uppercase", b64Key(32), "V1", "", "CREDENTIAL_ENC_KEY_ID"},
		// 23 = VARCHAR(32) key_id minus the ".<8 hex>" fingerprint crypto.NewKeyring appends.
		{"key id too long", b64Key(32), strings.Repeat("a", 24), "", "CREDENTIAL_ENC_KEY_ID"},
		{"retired id too long", b64Key(32), "", strings.Repeat("a", 24) + ":" + b64Key(32), "CREDENTIAL_ENC_RETIRED_KEYS"},
		{"retired without id", b64Key(32), "", b64Key(32), "CREDENTIAL_ENC_RETIRED_KEYS"},
		{"retired bad length", b64Key(32), "", "old:" + b64Key(31), "CREDENTIAL_ENC_RETIRED_KEYS"},
		{"retired bad id", b64Key(32), "", "Old:" + b64Key(32), "CREDENTIAL_ENC_RETIRED_KEYS"},
		{"retired without active key", "", "", "old:" + b64Key(32), "CREDENTIAL_ENC_RETIRED_KEYS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearCredentialEnv(t)
			t.Setenv("CREDENTIAL_ENC_KEY", tc.key)
			t.Setenv("CREDENTIAL_ENC_KEY_ID", tc.keyID)
			t.Setenv("CREDENTIAL_ENC_RETIRED_KEYS", tc.retired)
			_, err := LoadCredentialConfig()
			if err == nil {
				t.Fatal("LoadCredentialConfig() error = nil; want fail-fast")
			}
			if !strings.Contains(err.Error(), tc.wantVar) {
				t.Fatalf("error %q does not name %s — the operator cannot tell which variable is wrong", err, tc.wantVar)
			}
		})
	}
}

// TestLoadCredentialConfig_RetiredDuplicateIDFailsBoot: a retired id that repeats the active id or
// another retired id makes "which key opens v1 envelopes" ambiguous — refuse to boot.
func TestLoadCredentialConfig_RetiredDuplicateIDFailsBoot(t *testing.T) {
	for _, tc := range []struct{ name, keyID, retired string }{
		{"retired repeats default active id", "", "v1:" + b64Key(32)},
		{"retired repeats explicit active id", "k2026", "k2026:" + b64Key(32)},
		{"retired id listed twice", "v2", "v1:" + b64Key(32) + ",v1:" + b64Key(32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearCredentialEnv(t)
			t.Setenv("CREDENTIAL_ENC_KEY", b64Key(32))
			t.Setenv("CREDENTIAL_ENC_KEY_ID", tc.keyID)
			t.Setenv("CREDENTIAL_ENC_RETIRED_KEYS", tc.retired)
			if _, err := LoadCredentialConfig(); err == nil || !strings.Contains(err.Error(), "CREDENTIAL_ENC_RETIRED_KEYS") {
				t.Fatalf("LoadCredentialConfig() error = %v; want a fail-fast naming CREDENTIAL_ENC_RETIRED_KEYS", err)
			}
		})
	}
}

// TestLoadCredentialConfig_InsecureHTTPFlag: the dev-only escape hatch for plain http to a
// non-loopback host defaults OFF and a mistyped value fails boot rather than guessing.
func TestLoadCredentialConfig_InsecureHTTPFlag(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"false", false, false},
		{"true", true, false},
		{"1", true, false},
		{"yes", false, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			clearCredentialEnv(t)
			t.Setenv("CREDENTIAL_ENC_KEY", b64Key(32))
			t.Setenv("CREDENTIAL_ALLOW_INSECURE_HTTP", tc.raw)
			cfg, err := LoadCredentialConfig()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "CREDENTIAL_ALLOW_INSECURE_HTTP") {
					t.Fatalf("error = %v; want a fail-fast naming CREDENTIAL_ALLOW_INSECURE_HTTP", err)
				}
				return
			}
			if err != nil || cfg.AllowInsecureHTTP != tc.want {
				t.Fatalf("AllowInsecureHTTP = %v, err = %v; want %v", cfg.AllowInsecureHTTP, err, tc.want)
			}
		})
	}
}

// TestLoadCredentialConfig_KeyIDErrorNeverEchoesValue: an operator who pastes the master key into
// CREDENTIAL_ENC_KEY_ID must not find it in the fatal boot log (queue #222 LOW 2). The message names
// the variable and the value's length only.
func TestLoadCredentialConfig_KeyIDErrorNeverEchoesValue(t *testing.T) {
	clearCredentialEnv(t)
	master := b64Key(32)
	t.Setenv("CREDENTIAL_ENC_KEY", master)
	t.Setenv("CREDENTIAL_ENC_KEY_ID", master)
	_, err := LoadCredentialConfig()
	if err == nil {
		t.Fatal("LoadCredentialConfig() error = nil; want fail-fast")
	}
	if strings.Contains(err.Error(), master) {
		t.Fatalf("error %q echoes the key id value; want name + length only", err)
	}
	if !strings.Contains(err.Error(), "CREDENTIAL_ENC_KEY_ID") || !strings.Contains(err.Error(), "44") {
		t.Fatalf("error %q: want the variable name and the value length (44)", err)
	}
}
