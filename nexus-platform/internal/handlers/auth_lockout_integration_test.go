//go:build integration

// Integration coverage for the persistent account lockout (prod-ops slice b, mig 000043)
// against a real MySQL 8: exercises the REAL auth.UserRepository.RecordFailedLogin /
// ResetLoginFailures SQL (whose SET-list ORDER is load-bearing — the locked_until CASE must
// see the OLD failed_login_count) and the full Login flow over the live handler:
//
//	wrong password ×N → failed_login_count rises, locked_until stamped at the threshold →
//	the CORRECT password now answers the SAME generic 401 (anti-enumeration) →
//	lock expiry + correct password → 200 and both columns reset.
//
// Run: go test -tags=integration ./internal/handlers/ (needs MySQL up + migrated ≥ v43).
package handlers

import (
	"context"
	"net/http"
	"testing"
	"time"

	"gorm.io/gorm"

	"nexus/platform/internal/auth"
)

// lockoutUserRow reloads the seeded Alice row bypassing the repo (raw GORM) so assertions
// read exactly what is persisted.
func lockoutUserRow(t *testing.T, gdb *gorm.DB) *auth.User {
	t.Helper()
	var u auth.User
	if err := gdb.Where("tenant_id = ? AND email = ?", authTenantAID, authUserEmail).First(&u).Error; err != nil {
		t.Fatalf("reload user: %v", err)
	}
	return &u
}

func TestLockout_RepoIncrementAndThreshold(t *testing.T) {
	gdb := authGormDB(t)
	cleanup := seedAuthFixtures(t, gdb)
	t.Cleanup(cleanup)

	repo := auth.NewUserRepository(gdb)
	u := lockoutUserRow(t, gdb)
	lockUntil := time.Now().Add(15 * time.Minute).UTC()

	// Two failures at threshold 3: counter rises, no lock yet — proves the CASE evaluated
	// the OLD count (a wrong SET order would have locked on the 2nd call already).
	for i := 1; i <= 2; i++ {
		if err := repo.RecordFailedLogin(context.Background(), u.TenantID, u.ID, 3, lockUntil); err != nil {
			t.Fatalf("RecordFailedLogin #%d: %v", i, err)
		}
	}
	row := lockoutUserRow(t, gdb)
	if row.FailedLoginCount != 2 || row.LockedUntil != nil {
		t.Fatalf("after 2 failures: count=%d locked=%v, want 2/nil", row.FailedLoginCount, row.LockedUntil)
	}

	// Third failure crosses the threshold → locked_until stamped.
	if err := repo.RecordFailedLogin(context.Background(), u.TenantID, u.ID, 3, lockUntil); err != nil {
		t.Fatalf("RecordFailedLogin #3: %v", err)
	}
	row = lockoutUserRow(t, gdb)
	if row.FailedLoginCount != 3 || row.LockedUntil == nil {
		t.Fatalf("after 3 failures: count=%d locked=%v, want 3/stamped", row.FailedLoginCount, row.LockedUntil)
	}

	// Reset clears both columns.
	if err := repo.ResetLoginFailures(context.Background(), u.TenantID, u.ID); err != nil {
		t.Fatalf("ResetLoginFailures: %v", err)
	}
	row = lockoutUserRow(t, gdb)
	if row.FailedLoginCount != 0 || row.LockedUntil != nil {
		t.Fatalf("after reset: count=%d locked=%v, want 0/nil", row.FailedLoginCount, row.LockedUntil)
	}

	// Cross-tenant pin: a foreign tenant id must be a silent no-op, never a write.
	if err := repo.RecordFailedLogin(context.Background(), authTenantBID, u.ID, 3, lockUntil); err != nil {
		t.Fatalf("RecordFailedLogin cross-tenant: %v", err)
	}
	row = lockoutUserRow(t, gdb)
	if row.FailedLoginCount != 0 {
		t.Fatalf("cross-tenant write leaked: count=%d, want 0", row.FailedLoginCount)
	}
}

func TestLockout_LoginEndToEnd(t *testing.T) {
	gdb := authGormDB(t)
	cleanup := seedAuthFixtures(t, gdb)
	t.Cleanup(cleanup)

	r, _ := newAuthRouter(t, gdb)
	u := lockoutUserRow(t, gdb)

	// Pre-load the account one failure below the default threshold (10), then miss once more
	// through the LIVE handler → locked.
	lockUntil := time.Now().Add(15 * time.Minute).UTC()
	repo := auth.NewUserRepository(gdb)
	for i := 0; i < 9; i++ {
		if err := repo.RecordFailedLogin(context.Background(), u.TenantID, u.ID, 10, lockUntil); err != nil {
			t.Fatalf("preload failure #%d: %v", i, err)
		}
	}

	w := postLogin(t, r, `{"tenant":"`+authTenantASlug+`","email":"`+authUserEmail+`","password":"totally-wrong"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("10th wrong password: status = %d, want 401", w.Code)
	}
	wrongPwBody := w.Body.String()
	row := lockoutUserRow(t, gdb)
	if row.LockedUntil == nil {
		t.Fatal("expected locked_until stamped after the 10th failure")
	}

	// Locked + CORRECT password → the SAME generic 401 INVALID_CREDENTIALS (anti-enumeration).
	w = postLogin(t, r, `{"tenant":"`+authTenantASlug+`","email":"`+authUserEmail+`","password":"`+authPassword+`"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("locked + correct password: status = %d, want 401", w.Code)
	}
	// Byte-level anti-enumeration proof: the locked response must be INDISTINGUISHABLE from a
	// plain wrong-password response — same status asserted above, and the exact same body bytes
	// (any divergent code/message/details here would be a lockout oracle).
	if got := w.Body.String(); got != wrongPwBody {
		t.Fatalf("locked response differs from wrong-password response (enumeration oracle):\n locked: %s\n wrong:  %s", got, wrongPwBody)
	}

	// Expire the lock in place, then the correct password logs in and resets the columns.
	past := time.Now().Add(-time.Minute).UTC()
	if err := gdb.Model(&auth.User{}).
		Where("tenant_id = ? AND id = ?", u.TenantID, u.ID).
		Update("locked_until", past).Error; err != nil {
		t.Fatalf("expire lock: %v", err)
	}
	w = postLogin(t, r, `{"tenant":"`+authTenantASlug+`","email":"`+authUserEmail+`","password":"`+authPassword+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("after lock expiry: status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	row = lockoutUserRow(t, gdb)
	if row.FailedLoginCount != 0 || row.LockedUntil != nil {
		t.Fatalf("after successful login: count=%d locked=%v, want 0/nil", row.FailedLoginCount, row.LockedUntil)
	}
}
