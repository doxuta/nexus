package services

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TOTP parameters (RFC 6238 defaults, compatible with every major authenticator app).
const (
	totpPeriodSeconds = 30 // seconds per time-step
	totpSkewSteps     = 1  // +-1 step tolerance for clock drift
	totpDigits        = otp.DigitsSix
	totpAlgorithm     = otp.AlgorithmSHA1
	totpIssuer        = "Nexus"
)

// mfaBackupCodeCount is how many single-use recovery codes are generated at activation time.
const mfaBackupCodeCount = 10

// mfaBackupCodeBytes is the raw entropy (before hex-encoding) of one backup code — 10 bytes ⇒
// a 20-hex-character code, i.e. ~80 bits of entropy. Bumped up from the original 4-byte/8-hex/
// ~32-bit spec (security review 🟡#1, reviewer LOW-2): the codes are persisted as an UNSALTED
// SHA-256 hash (auth.HashRefreshToken — mfa_backup_codes.code_hash), so if the database ever
// leaks, an attacker can attempt an OFFLINE brute force with no rate limit at all. At ~32 bits
// that offline search (2^32 SHA-256 hashes) is trivially fast on commodity GPU hardware — the
// online defenses (RM-15's rate limiter + single-use CAS) do nothing to slow an offline attack
// against a leaked hash. At ~80 bits, an offline brute force (2^80 hashes) is computationally
// infeasible with any realistic hardware, closing that gap WITHOUT switching to a salted/slow
// hash scheme. RM-15's rate limiter and single-use consumption remain the (now redundant but
// still valid) defenses for the ONLINE guessing case.
const mfaBackupCodeBytes = 10

// generateTOTPSecret creates a fresh random TOTP seed for accountName (typically the user's
// email) and returns it as (base32 secret, otpauth:// URL) — the secret for round-tripping
// through matchTOTPCode/sealing, the URL for the frontend to render as a QR code
// (format otpauth://totp/Nexus:<email>?secret=...&issuer=Nexus — self-evident from the
// totp.GenerateOpts below; ../specs/003-mfa-totp/spec.md §"#7").
func generateTOTPSecret(accountName string) (secret, otpauthURL string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: accountName,
		Period:      totpPeriodSeconds,
		Digits:      totpDigits,
		Algorithm:   totpAlgorithm,
	})
	if err != nil {
		return "", "", fmt.Errorf("services.generateTOTPSecret: %w", err)
	}
	return key.Secret(), key.String(), nil
}

// matchTOTPCode checks a presented code against secret at time now, trying the current
// time-step and up to totpSkewSteps steps on either side. It returns the EXACT step the code
// matched (not the wall-clock step) so the caller can perform a replay-safe CAS keyed on that
// matched step (../specs/003-mfa-totp/spec.md §"#7" MUST-FIX 2 — recording wall-clock "now" instead
// of the matched step would let a code accepted at step N be replayed at step N+1, since skew
// still validates it there while a naive "last_used_step < now" CAS would let it through).
// Comparison is constant-time (crypto/subtle) to avoid a timing side-channel on the code guess.
func matchTOTPCode(secret, code string, now time.Time) (matchedStep int64, ok bool) {
	code = strings.TrimSpace(code)
	if code == "" {
		return 0, false
	}
	current := now.Unix() / totpPeriodSeconds
	for offset := -int64(totpSkewSteps); offset <= int64(totpSkewSteps); offset++ {
		step := current + offset
		if step < 0 {
			continue
		}
		candidate, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriodSeconds, 0).UTC(), totp.ValidateOpts{
			Period:    totpPeriodSeconds,
			Digits:    totpDigits,
			Algorithm: totpAlgorithm,
		})
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// generateBackupCodes returns mfaBackupCodeCount fresh, distinct, lowercase-hex recovery
// codes (mfaBackupCodeBytes random bytes each). Errors from crypto/rand.Read are never
// ignored — a CSPRNG failure must abort issuance rather than hand out weak/predictable codes.
func generateBackupCodes() ([]string, error) {
	codes := make([]string, 0, mfaBackupCodeCount)
	seen := make(map[string]struct{}, mfaBackupCodeCount)
	for len(codes) < mfaBackupCodeCount {
		buf := make([]byte, mfaBackupCodeBytes)
		if _, err := rand.Read(buf); err != nil {
			return nil, fmt.Errorf("services.generateBackupCodes: read random bytes: %w", err)
		}
		code := hex.EncodeToString(buf)
		if _, dup := seen[code]; dup {
			// Astronomically unlikely at 80 bits of entropy for 10 draws, but re-roll rather
			// than ever hand out two identical backup codes to the same user.
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}
	return codes, nil
}
