package services

import (
	"fmt"
	"strings"
)

// Password policy (RM-17) — the ONE shared body for every path that sets a password:
// UserService.CreateUser, UserService.UpdateUser, AuthService.ChangePassword and
// AuthService.ResetPassword all call ValidatePassword, so the rules can never drift
// between admin-set and self-set passwords again (the pre-RM-17 state enforced the
// minimum only on change/reset — CreateUser/UpdateUser accepted any non-empty string).
//
// Login deliberately does NOT validate: it must keep accepting whatever was stored
// historically; the policy gates only the paths that WRITE a new password.
const (
	// minPasswordLength is the minimum rune length for a new password. Kept at 8 (the
	// historical value). Raising the default to 12 — ideally env-tunable
	// (AUTH_PASSWORD_MIN_LENGTH) — is a tracked follow-up, not a silent change here.
	minPasswordLength = 8

	// maxPasswordBytes mirrors bcrypt's hard input limit: bcrypt only uses the first
	// 72 BYTES of the password, and x/crypto's GenerateFromPassword (≥ v0.17) returns
	// ErrPasswordTooLong outright for longer inputs — which every existing set-password
	// path surfaced as a wrapped 500. Rejecting > 72 bytes here turns that into a clean
	// 422 without changing what passwords can actually be set (they never could). This
	// bound also subsumes any rune-count ceiling: ≤ 72 bytes ⇒ ≤ 72 runes.
	maxPasswordBytes = 72
)

// ValidatePassword applies the shared password policy to a candidate NEW password and
// returns a *ValidationError with code PASSWORD_TOO_WEAK (→ 422) describing the first
// violated rule, or nil when the password is acceptable. The password itself is never
// included in the error or logged.
func ValidatePassword(pw string) error {
	if strings.TrimSpace(pw) == "" {
		return &ValidationError{Code: "PASSWORD_TOO_WEAK", Msg: "mật khẩu không được rỗng hoặc toàn khoảng trắng"}
	}
	if len([]rune(pw)) < minPasswordLength {
		return &ValidationError{Code: "PASSWORD_TOO_WEAK", Msg: fmt.Sprintf("mật khẩu phải có ít nhất %d ký tự", minPasswordLength)}
	}
	if len(pw) > maxPasswordBytes {
		return &ValidationError{Code: "PASSWORD_TOO_WEAK", Msg: fmt.Sprintf("mật khẩu quá dài (tối đa %d byte — giới hạn bcrypt)", maxPasswordBytes)}
	}
	return nil
}
