package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

// lockoutFixture builds an AuthService over the in-memory fakes with a tight lockout policy
// (3 failures / 15 min) and an injectable clock, returning the service, the seeded user row
// (so tests can inspect/preload FailedLoginCount + LockedUntil) and the correct password.
func lockoutFixture(t *testing.T) (*AuthService, *auth.User, string) {
	t.Helper()
	svc, password := newTestService(t)
	svc.WithLockoutPolicy(3, 15*time.Minute)
	repo := svc.users.(*fakeUserRepo)
	return svc, repo.users["t-1\x00alice@acme.test"], password
}

func loginAttempt(svc *AuthService, password string) error {
	_, err := svc.Login(context.Background(), LoginInput{
		Tenant: "acme", Email: "alice@acme.test", Password: password,
	})
	return err
}

// Wrong passwords increment the persistent counter; crossing the threshold stamps
// locked_until, after which even the CORRECT password gets the same generic 401
// (anti-enumeration: locked is indistinguishable from a bad guess).
func TestLogin_LockoutAfterMaxFailures(t *testing.T) {
	svc, u, password := lockoutFixture(t)

	for i := 1; i <= 2; i++ {
		if err := loginAttempt(svc, "wrong-guess"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: err = %v, want ErrInvalidCredentials", i, err)
		}
		if u.LockedUntil != nil {
			t.Fatalf("attempt %d: locked too early (threshold is 3)", i)
		}
	}
	if u.FailedLoginCount != 2 {
		t.Fatalf("failed_login_count = %d, want 2", u.FailedLoginCount)
	}

	// Third failure crosses the threshold → locked.
	if err := loginAttempt(svc, "wrong-guess"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("attempt 3: err = %v, want ErrInvalidCredentials", err)
	}
	if u.LockedUntil == nil || !u.LockedUntil.After(time.Now()) {
		t.Fatal("expected locked_until stamped in the future after 3rd failure")
	}

	// While locked, the CORRECT password must be refused with the SAME sentinel — the
	// response can never reveal "locked" vs "wrong password" (anti-enumeration).
	if err := loginAttempt(svc, password); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("locked + correct password: err = %v, want ErrInvalidCredentials", err)
	}
	// And the correct-password attempt during the lock must NOT have cleared the state.
	if u.LockedUntil == nil || u.FailedLoginCount != 3 {
		t.Fatalf("lock state mutated during locked window: count=%d locked=%v", u.FailedLoginCount, u.LockedUntil)
	}
}

// Once the lock window has passed, a correct login succeeds and clears both columns.
func TestLogin_LockExpiryThenSuccessResets(t *testing.T) {
	svc, u, password := lockoutFixture(t)

	expired := time.Now().Add(-time.Minute)
	u.FailedLoginCount = 3
	u.LockedUntil = &expired

	out, err := loginOutput(svc, password)
	if err != nil {
		t.Fatalf("login after lock expiry: %v", err)
	}
	if out.AccessToken == "" {
		t.Fatal("expected a real token pair after lock expiry")
	}
	if u.FailedLoginCount != 0 || u.LockedUntil != nil {
		t.Fatalf("counters not reset on success: count=%d locked=%v", u.FailedLoginCount, u.LockedUntil)
	}
}

func loginOutput(svc *AuthService, password string) (*LoginOutput, error) {
	return svc.Login(context.Background(), LoginInput{
		Tenant: "acme", Email: "alice@acme.test", Password: password,
	})
}

// After the lock expires, ONE further wrong password re-locks immediately (the counter is
// only cleared by a successful login, so count+1 is still ≥ threshold).
func TestLogin_ExpiredLockRelocksOnNextFailure(t *testing.T) {
	svc, u, _ := lockoutFixture(t)

	expired := time.Now().Add(-time.Minute)
	u.FailedLoginCount = 3
	u.LockedUntil = &expired

	if err := loginAttempt(svc, "wrong-guess"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	if u.LockedUntil == nil || !u.LockedUntil.After(time.Now()) {
		t.Fatal("expected an immediate re-lock on the first failure after expiry")
	}
	if u.FailedLoginCount != 4 {
		t.Fatalf("failed_login_count = %d, want 4", u.FailedLoginCount)
	}
}

// A wrong password for an account that is disabled — or a correct password on a disabled
// account — must not corrupt the lockout bookkeeping: wrong password still increments
// (the row exists), but a correct password on a disabled account does NOT reset (no
// successful login happened).
func TestLogin_DisabledAccountDoesNotReset(t *testing.T) {
	svc, u, password := lockoutFixture(t)
	u.Status = "disabled"
	u.FailedLoginCount = 2

	if err := loginAttempt(svc, password); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	if u.FailedLoginCount != 2 {
		t.Fatalf("failed_login_count = %d, want 2 (correct password on disabled account must not reset)", u.FailedLoginCount)
	}
}

// Unknown tenant / unknown user keep the historical dummy-hash behavior with ZERO lockout
// writes — there is no user row to account against, and the write path must not become an
// oracle. The fake no-ops on a missing row exactly like the real 0-rows UPDATE, so this
// asserts the seeded user's state is untouched by foreign-email attempts.
func TestLogin_UnknownUserNoLockoutWrite(t *testing.T) {
	svc, u, _ := lockoutFixture(t)

	for i := 0; i < 5; i++ {
		_, err := svc.Login(context.Background(), LoginInput{
			Tenant: "acme", Email: "ghost@acme.test", Password: "whatever-123",
		})
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("err = %v, want ErrInvalidCredentials", err)
		}
	}
	if u.FailedLoginCount != 0 || u.LockedUntil != nil {
		t.Fatalf("unknown-user attempts leaked into another row: count=%d locked=%v", u.FailedLoginCount, u.LockedUntil)
	}
}

// WithLockoutPolicy must not allow disabling via zero/negative values (consistent with the
// AI_* knobs: env can tune, never turn off) and keeps defaults when given garbage.
func TestWithLockoutPolicy_RejectsNonPositive(t *testing.T) {
	svc, _, _ := lockoutFixture(t) // fixture set 3 / 15 min
	svc.WithLockoutPolicy(0, 0)
	if svc.lockoutMaxFailures != 3 || svc.lockoutDuration != 15*time.Minute {
		t.Fatalf("zero values must keep the previous policy, got %d / %v", svc.lockoutMaxFailures, svc.lockoutDuration)
	}
	svc.WithLockoutPolicy(-5, -time.Minute)
	if svc.lockoutMaxFailures != 3 || svc.lockoutDuration != 15*time.Minute {
		t.Fatalf("negative values must keep the previous policy, got %d / %v", svc.lockoutMaxFailures, svc.lockoutDuration)
	}
}
