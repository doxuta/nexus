//go:build integration

// Integration tests for the M4 daily-budget ledger reader (spec §12). They prove what the fake
// TokenLedger cannot: TokensSince really sums prompt+output over MySQL, isolates tenants, treats
// NULL token columns as 0 (a gateway_error turn contributes nothing, not a scan failure), and
// respects the `since` boundary against the UTC created_at that Record writes.
//
// Run with the same recipe as runs_integration_test.go (throwaway DB migrated, single-source
// TEST_DATABASE_URL, B7).
package ai

import (
	"context"
	"testing"
	"time"
)

func TestSQLRunRepository_TokensSince_SumsAndIsolates(t *testing.T) {
	db := connectAI(t)
	const tenantA = "aim4tena-0000-0000-0000-000000000001"
	const tenantB = "aim4tenb-0000-0000-0000-000000000002"

	for _, tenant := range []string{tenantA, tenantB} {
		db.Exec(`DELETE FROM sys_ai_runs WHERE tenant_id = ?`, tenant)
	}
	t.Cleanup(func() {
		for _, tenant := range []string{tenantA, tenantB} {
			db.Exec(`DELETE FROM sys_ai_runs WHERE tenant_id = ?`, tenant)
		}
	})

	ctx := context.Background()
	repo := NewSQLRunRepository(db)
	since := time.Now().UTC().Add(-time.Minute)

	// An OLDER repo whose injected clock stamps rows 2h in the past — proves the since boundary
	// really filters on created_at, not on insertion order.
	oldRepo := NewSQLRunRepository(db)
	oldRepo.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }

	base := Run{
		UserID:     "aim4user-0000-0000-0000-000000000001",
		Feature:    FeatureBuilderChat,
		Model:      "cc/claude-sonnet-4-6",
		PromptHash: sha256Hex("m4 budget"),
		Outcome:    outcomeOK,
		LatencyMS:  100,
	}
	insert := func(r *SQLRunRepository, id, tenant string, prompt, output int) {
		t.Helper()
		run := base
		run.ID, run.TenantID, run.PromptTokens, run.OutputTokens = id, tenant, prompt, output
		if err := r.Record(ctx, run); err != nil {
			t.Fatalf("Record(%s): %v", id, err)
		}
	}

	// Tenant A today: 100+50 and 20+10 = 180 total, plus a NULL-token row (0,0 → stored NULL —
	// see nullableInt) that must contribute exactly 0.
	insert(repo, "aim4run0-0000-0000-0000-000000000001", tenantA, 100, 50)
	insert(repo, "aim4run0-0000-0000-0000-000000000002", tenantA, 20, 10)
	insert(repo, "aim4run0-0000-0000-0000-000000000003", tenantA, 0, 0)
	// Tenant A, 2h old: must be excluded by `since`, included by an earlier boundary.
	insert(oldRepo, "aim4run0-0000-0000-0000-000000000004", tenantA, 5000, 0)
	// Tenant B today: 999+1 = 1000, must never bleed into A's total.
	insert(repo, "aim4run0-0000-0000-0000-000000000005", tenantB, 999, 1)

	gotA, err := repo.TokensSince(ctx, tenantA, since)
	if err != nil {
		t.Fatalf("TokensSince(A): %v", err)
	}
	if gotA != 180 {
		t.Fatalf("TokensSince(A, -1m) = %d; want 180 (100+50+20+10, NULL row = 0, old row excluded)", gotA)
	}

	gotB, err := repo.TokensSince(ctx, tenantB, since)
	if err != nil {
		t.Fatalf("TokensSince(B): %v", err)
	}
	if gotB != 1000 {
		t.Fatalf("TokensSince(B, -1m) = %d; want 1000 (tenant isolation)", gotB)
	}

	// Widen the window past the old row: it joins A's total (prompt 5000 + NULL output = 5000).
	gotAWide, err := repo.TokensSince(ctx, tenantA, time.Now().UTC().Add(-3*time.Hour))
	if err != nil {
		t.Fatalf("TokensSince(A, -3h): %v", err)
	}
	if gotAWide != 5180 {
		t.Fatalf("TokensSince(A, -3h) = %d; want 5180 (old 5000-token row included)", gotAWide)
	}

	// A tenant with no rows sums to 0 (outer COALESCE), not NULL/error.
	gotNone, err := repo.TokensSince(ctx, "aim4none-0000-0000-0000-000000000009", since)
	if err != nil {
		t.Fatalf("TokensSince(no-rows tenant): %v", err)
	}
	if gotNone != 0 {
		t.Fatalf("TokensSince(no-rows tenant) = %d; want 0", gotNone)
	}
}

// TestSQLRunRepository_TokensSinceByUser_SumsAndIsolates proves the per-user daily-share reader
// (ASSISTANT_DAILY_TOKEN_SHARE_PCT): the sum is pinned to (tenant, user), a second user of the
// SAME tenant never bleeds in, the same user id under ANOTHER tenant never bleeds in, and a
// no-rows user sums to 0.
func TestSQLRunRepository_TokensSinceByUser_SumsAndIsolates(t *testing.T) {
	db := connectAI(t)
	const tenantA = "aim4tena-0000-0000-0000-000000000011"
	const tenantB = "aim4tenb-0000-0000-0000-000000000012"
	const user1 = "aim4user-0000-0000-0000-000000000021"
	const user2 = "aim4user-0000-0000-0000-000000000022"

	for _, tenant := range []string{tenantA, tenantB} {
		db.Exec(`DELETE FROM sys_ai_runs WHERE tenant_id = ?`, tenant)
	}
	t.Cleanup(func() {
		for _, tenant := range []string{tenantA, tenantB} {
			db.Exec(`DELETE FROM sys_ai_runs WHERE tenant_id = ?`, tenant)
		}
	})

	ctx := context.Background()
	repo := NewSQLRunRepository(db)
	since := time.Now().UTC().Add(-time.Minute)

	insert := func(id, tenant, user string, prompt, output int) {
		t.Helper()
		if err := repo.Record(ctx, Run{
			ID: id, TenantID: tenant, UserID: user,
			Feature: FeatureBuilderChat, Model: "cc/claude-sonnet-4-6",
			PromptHash: sha256Hex("m4 user share"), Outcome: outcomeOK, LatencyMS: 100,
			PromptTokens: prompt, OutputTokens: output,
		}); err != nil {
			t.Fatalf("Record(%s): %v", id, err)
		}
	}

	insert("aim4run1-0000-0000-0000-000000000001", tenantA, user1, 100, 50) // user1@A: 150
	insert("aim4run1-0000-0000-0000-000000000002", tenantA, user1, 20, 10)  // user1@A: +30 = 180
	insert("aim4run1-0000-0000-0000-000000000003", tenantA, user2, 400, 0)  // user2@A (excluded)
	insert("aim4run1-0000-0000-0000-000000000004", tenantB, user1, 999, 1)  // user1@B (excluded)

	got, err := repo.TokensSinceByUser(ctx, tenantA, user1, since)
	if err != nil {
		t.Fatalf("TokensSinceByUser(A, user1): %v", err)
	}
	if got != 180 {
		t.Fatalf("TokensSinceByUser(A, user1) = %d; want 180 (user2 + tenantB rows excluded)", got)
	}

	// The tenant-wide sum still sees BOTH users (the per-user reader narrows, never replaces).
	gotTenant, err := repo.TokensSince(ctx, tenantA, since)
	if err != nil {
		t.Fatalf("TokensSince(A): %v", err)
	}
	if gotTenant != 580 {
		t.Fatalf("TokensSince(A) = %d; want 580 (180 user1 + 400 user2)", gotTenant)
	}

	// A user with no rows sums to 0, not NULL/error.
	gotNone, err := repo.TokensSinceByUser(ctx, tenantA, "aim4user-0000-0000-0000-000000000029", since)
	if err != nil {
		t.Fatalf("TokensSinceByUser(no-rows user): %v", err)
	}
	if gotNone != 0 {
		t.Fatalf("TokensSinceByUser(no-rows user) = %d; want 0", gotNone)
	}
}
