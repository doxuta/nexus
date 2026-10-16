package services

import (
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

const testTOTPSecret = "JBSWY3DPEHPK3PXP" // fixed base32 seed used across the skew/replay tests

func codeAtStep(t *testing.T, secret string, step int64) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriodSeconds, 0).UTC(), totp.ValidateOpts{
		Period:    totpPeriodSeconds,
		Digits:    totpDigits,
		Algorithm: totpAlgorithm,
	})
	if err != nil {
		t.Fatalf("codeAtStep(%d): %v", step, err)
	}
	return code
}

func TestMatchTOTPCode_CurrentStepMatches(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	step := now.Unix() / totpPeriodSeconds
	code := codeAtStep(t, testTOTPSecret, step)

	matched, ok := matchTOTPCode(testTOTPSecret, code, now)
	if !ok {
		t.Fatal("matchTOTPCode: want ok=true for the current-step code")
	}
	if matched != step {
		t.Fatalf("matched step = %d; want %d", matched, step)
	}
}

func TestMatchTOTPCode_SkewWindow_PreviousAndNextStepMatch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	step := now.Unix() / totpPeriodSeconds

	prevCode := codeAtStep(t, testTOTPSecret, step-1)
	if matched, ok := matchTOTPCode(testTOTPSecret, prevCode, now); !ok || matched != step-1 {
		t.Fatalf("previous-step code: matched=%d ok=%v; want %d,true", matched, ok, step-1)
	}

	nextCode := codeAtStep(t, testTOTPSecret, step+1)
	if matched, ok := matchTOTPCode(testTOTPSecret, nextCode, now); !ok || matched != step+1 {
		t.Fatalf("next-step code: matched=%d ok=%v; want %d,true", matched, ok, step+1)
	}
}

func TestMatchTOTPCode_OutsideSkewWindow_Rejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	step := now.Unix() / totpPeriodSeconds

	tooOld := codeAtStep(t, testTOTPSecret, step-2)
	if _, ok := matchTOTPCode(testTOTPSecret, tooOld, now); ok {
		t.Fatal("matchTOTPCode: want ok=false for a code 2 steps in the past (outside +-1 skew)")
	}

	tooNew := codeAtStep(t, testTOTPSecret, step+2)
	if _, ok := matchTOTPCode(testTOTPSecret, tooNew, now); ok {
		t.Fatal("matchTOTPCode: want ok=false for a code 2 steps in the future (outside +-1 skew)")
	}
}

func TestMatchTOTPCode_WrongCode_Rejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	if _, ok := matchTOTPCode(testTOTPSecret, "000000", now); ok {
		// Vanishingly unlikely to collide, but guard determinism by also trying a second value.
		if _, ok2 := matchTOTPCode(testTOTPSecret, "111111", now); ok2 {
			t.Fatal("matchTOTPCode: want ok=false for arbitrary wrong codes")
		}
	}
}

func TestMatchTOTPCode_EmptyCode_Rejected(t *testing.T) {
	if _, ok := matchTOTPCode(testTOTPSecret, "", time.Now()); ok {
		t.Fatal("matchTOTPCode: want ok=false for empty code")
	}
}

func TestGenerateTOTPSecret_ReturnsUsableSecretAndOtpauthURL(t *testing.T) {
	secret, otpauthURL, err := generateTOTPSecret("alice@acme.test")
	if err != nil {
		t.Fatalf("generateTOTPSecret: %v", err)
	}
	if secret == "" {
		t.Fatal("secret is empty")
	}
	if otpauthURL == "" {
		t.Fatal("otpauthURL is empty")
	}
	if got, want := otpauthURL[:len("otpauth://totp/Nexus:alice@acme.test?")], "otpauth://totp/Nexus:alice@acme.test?"; got != want {
		t.Fatalf("otpauthURL prefix = %q; want %q (full url: %s)", got, want, otpauthURL)
	}

	// The returned secret must actually work with matchTOTPCode (round-trip sanity).
	now := time.Now().UTC()
	step := now.Unix() / totpPeriodSeconds
	code := codeAtStep(t, secret, step)
	if _, ok := matchTOTPCode(secret, code, now); !ok {
		t.Fatal("generated secret does not validate its own current-step code")
	}
}

func TestGenerateBackupCodes_ReturnsTenDistinctTwentyHexCodes(t *testing.T) {
	codes, err := generateBackupCodes()
	if err != nil {
		t.Fatalf("generateBackupCodes: %v", err)
	}
	if len(codes) != mfaBackupCodeCount {
		t.Fatalf("len(codes) = %d; want %d", len(codes), mfaBackupCodeCount)
	}
	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		// 20 hex chars = 10 raw bytes = 80 bits of entropy (security review 🟡#1 / reviewer
		// LOW-2: bumped up from the original 8-hex/32-bit spec — see mfa_totp.go's
		// mfaBackupCodeBytes doc for the offline-brute-force rationale).
		if len(c) != 20 {
			t.Errorf("code %q: len = %d; want 20", c, len(c))
		}
		for _, r := range c {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				t.Errorf("code %q contains non-hex rune %q", c, r)
			}
		}
		if seen[c] {
			t.Errorf("duplicate backup code generated: %q", c)
		}
		seen[c] = true
	}
}
