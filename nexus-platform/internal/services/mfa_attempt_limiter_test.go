package services

import (
	"testing"
	"time"
)

// --- Pure limiter unit tests (no AuthService involved) -------------------------------------

func TestMFAMemAttemptLimiter_AllowsUpToThreshold_ThenBlocks(t *testing.T) {
	lim := newMFAMemAttemptLimiter(5, 15*time.Minute)
	tenantID, userID := "t-1", "u-1"

	for i := 0; i < 5; i++ {
		if !lim.Allow(tenantID, userID) {
			t.Fatalf("attempt %d: Allow = false; want true (under threshold)", i+1)
		}
		lim.RecordFailure(tenantID, userID)
	}
	// The 6th check, after 5 recorded failures, must be blocked.
	if lim.Allow(tenantID, userID) {
		t.Fatal("Allow after 5 failures = true; want false (threshold reached)")
	}
}

func TestMFAMemAttemptLimiter_Reset_ReopensTheWindow(t *testing.T) {
	lim := newMFAMemAttemptLimiter(5, 15*time.Minute)
	tenantID, userID := "t-1", "u-1"

	for i := 0; i < 3; i++ {
		lim.RecordFailure(tenantID, userID)
	}
	lim.Reset(tenantID, userID)

	// After Reset, 5 fresh failures should be allowed again before the 6th is blocked.
	for i := 0; i < 5; i++ {
		if !lim.Allow(tenantID, userID) {
			t.Fatalf("post-reset attempt %d: Allow = false; want true", i+1)
		}
		lim.RecordFailure(tenantID, userID)
	}
	if lim.Allow(tenantID, userID) {
		t.Fatal("Allow after post-reset 5 failures = true; want false")
	}
}

func TestMFAMemAttemptLimiter_IndependentPerTenantUser(t *testing.T) {
	lim := newMFAMemAttemptLimiter(5, 15*time.Minute)

	for i := 0; i < 5; i++ {
		lim.RecordFailure("t-1", "u-A")
	}
	if lim.Allow("t-1", "u-A") {
		t.Fatal("user A: Allow after 5 failures = true; want false")
	}
	// A different user in the SAME tenant must be unaffected.
	if !lim.Allow("t-1", "u-B") {
		t.Fatal("user B (same tenant, no failures): Allow = false; want true")
	}
	// The SAME user id in a DIFFERENT tenant must also be unaffected (keyed on the pair).
	if !lim.Allow("t-2", "u-A") {
		t.Fatal("user A in tenant 2 (no failures there): Allow = false; want true")
	}
}

func TestMFAMemAttemptLimiter_WindowExpiry_ReopensAfterWindow(t *testing.T) {
	lim := newMFAMemAttemptLimiter(5, 15*time.Minute)
	tenantID, userID := "t-1", "u-1"

	fakeNow := time.Unix(1_700_000_000, 0).UTC()
	lim.now = func() time.Time { return fakeNow }

	for i := 0; i < 5; i++ {
		lim.RecordFailure(tenantID, userID)
	}
	if lim.Allow(tenantID, userID) {
		t.Fatal("Allow within the window after 5 failures = true; want false")
	}

	// Advance the clock past the window: the old failures must age out.
	fakeNow = fakeNow.Add(16 * time.Minute)
	if !lim.Allow(tenantID, userID) {
		t.Fatal("Allow after the window elapsed = false; want true (failures aged out)")
	}
}
