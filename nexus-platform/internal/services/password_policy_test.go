package services

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestValidatePassword drives the shared RM-17 policy body table-style: every rejection
// carries code PASSWORD_TOO_WEAK (→ 422 via statusForCode's default) and never echoes the
// password; acceptable inputs return nil.
func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name    string
		pw      string
		wantErr bool
	}{
		{"empty", "", true},
		{"all whitespace", "        ", true},
		{"whitespace shorter than min after trim is still all-whitespace", " \t\n  ", true},
		{"too short (7 runes)", "abc1234", true},
		{"too short unicode (7 runes, >8 bytes)", "mậtkhẩu", true},
		{"exactly min length (8 runes)", "abcd1234", false},
		{"unicode counted by rune not byte", "mậtkhẩu8", false},
		{"long but within bcrypt bound (72 bytes)", strings.Repeat("a", 72), false},
		{"over bcrypt 72-byte bound", strings.Repeat("a", 73), true},
		{"multibyte over 72 bytes but under 72 runes", strings.Repeat("ộ", 25), true}, // 25 runes × 3 bytes = 75 B
		{"interior spaces are legal characters", "pass word 123", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePassword(tt.pw)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ValidatePassword(%q) = %v, want nil", tt.pw, err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("ValidatePassword(%q) = %T (%v), want *ValidationError", tt.pw, err, err)
			}
			if ve.Code != "PASSWORD_TOO_WEAK" {
				t.Errorf("code = %q, want PASSWORD_TOO_WEAK", ve.Code)
			}
			if tt.pw != "" && strings.Contains(ve.Msg, tt.pw) {
				t.Errorf("error message echoes the password: %q", ve.Msg)
			}
		})
	}
}

// TestPasswordPolicy_CreateAndUpdateUser pins the RM-17 fix at the two entry points that
// previously accepted ANY non-empty password: an admin creating or re-passwording a user is
// now held to the SAME shared policy as ChangePassword/ResetPassword (which have their own
// weak-password tests). Empty-on-update stays "no change", not a violation.
func TestPasswordPolicy_CreateAndUpdateUser(t *testing.T) {
	t.Run("CreateUser rejects weak password", func(t *testing.T) {
		svc, repo := newUserSvc()
		_, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{
			Email: "weak@acme.test", Password: "short",
		})
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "PASSWORD_TOO_WEAK" {
			t.Fatalf("CreateUser weak pw: got %v, want PASSWORD_TOO_WEAK", err)
		}
		if len(repo.rows) != 0 {
			t.Errorf("no user row must be written on rejection, got %d", len(repo.rows))
		}
	})

	t.Run("UpdateUser rejects weak password", func(t *testing.T) {
		svc, repo := newUserSvc()
		seedUser(repo, "t1", "u1", false, "active")
		weak := "short"
		_, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &weak})
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "PASSWORD_TOO_WEAK" {
			t.Fatalf("UpdateUser weak pw: got %v, want PASSWORD_TOO_WEAK", err)
		}
	})

	t.Run("UpdateUser empty password means no change, not a violation", func(t *testing.T) {
		svc, repo := newUserSvc()
		seedUser(repo, "t1", "u1", false, "active")
		before := repo.rows[repo.key("t1", "u1")].PasswordHash
		empty := ""
		if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &empty}); err != nil {
			t.Fatalf("empty password on update must be a no-op: %v", err)
		}
		if repo.rows[repo.key("t1", "u1")].PasswordHash != before {
			t.Error("empty password on update must not touch the stored hash")
		}
	})
}
