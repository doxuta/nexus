//go:build integration

package idempotency

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// specs/047 FR-13: a leased batch claim holds the batch's base id. SavePlan stores the response it
// will answer with BEFORE the write; CompleteResponse is fenced to the owner's base id (a taken-over
// run cannot complete its successor's claim); Takeover drops the previous attempt's plan.
func TestSQLRepository_LeasedBatchClaim(t *testing.T) {
	db := testDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()
	tenant, user := uuid.NewString(), uuid.NewString()
	t.Cleanup(func() { db.Exec(`DELETE FROM sys_idempotency_keys WHERE tenant_id = ?`, tenant) })
	exp := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	base, base2 := uuid.NewString(), uuid.NewString()
	if err := repo.Claim(ctx, Claim{TenantID: tenant, PrincipalID: user, Key: "imp-1", EntityName: "crm_contacts", RequestHash: "h", RecordID: base, ExpiresAt: exp}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	plan := []byte(`{"total":3,"inserted":2,"failed":1,"errors":[{"row":2,"code":"X","message":"Tiếng Việt"}]}`)
	if err := repo.SavePlan(ctx, tenant, user, "imp-1", "not-the-owner", plan); err != nil {
		t.Fatalf("save plan (foreign id): %v", err)
	}
	if got, _ := repo.Get(ctx, tenant, user, "imp-1"); got == nil || got.Response != nil {
		t.Fatalf("a foreign id saved a plan: %+v", got)
	}
	if err := repo.SavePlan(ctx, tenant, user, "imp-1", base, plan); err != nil {
		t.Fatalf("save plan: %v", err)
	}
	got, err := repo.Get(ctx, tenant, user, "imp-1")
	if err != nil || got.Status != StatusProcessing || string(got.Response) != string(plan) || got.RecordID != base {
		t.Fatalf("claim after SavePlan = (%+v, %v), want processing holding the exact plan bytes", got, err)
	}

	// Takeover hands the claim to the next attempt WITHOUT the dead attempt's plan.
	if won, err := repo.Takeover(ctx, tenant, user, "imp-1", base, base2, exp); err != nil || !won {
		t.Fatalf("takeover = (%v, %v), want (true, nil)", won, err)
	}
	if got, _ = repo.Get(ctx, tenant, user, "imp-1"); got == nil || got.RecordID != base2 || got.Response != nil {
		t.Fatalf("claim after takeover = %+v, want base2 with no plan", got)
	}

	// The previous owner cannot complete its successor's claim.
	if err := repo.CompleteResponse(ctx, tenant, user, "imp-1", base, plan); err != nil {
		t.Fatalf("complete (stale owner): %v", err)
	}
	if got, _ = repo.Get(ctx, tenant, user, "imp-1"); got == nil || got.Status != StatusProcessing {
		t.Fatalf("a taken-over run completed its successor's claim: %+v", got)
	}
	if err := repo.CompleteResponse(ctx, tenant, user, "imp-1", base2, plan); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got, _ = repo.Get(ctx, tenant, user, "imp-1"); got == nil || got.Status != StatusCompleted || string(got.Response) != string(plan) {
		t.Fatalf("claim after CompleteResponse = %+v, want completed with the plan bytes", got)
	}
	// A completed claim keeps its response: a late SavePlan does not rewrite it.
	if err := repo.SavePlan(ctx, tenant, user, "imp-1", base2, []byte(`{}`)); err != nil {
		t.Fatalf("late save plan: %v", err)
	}
	if got, _ = repo.Get(ctx, tenant, user, "imp-1"); string(got.Response) != string(plan) {
		t.Fatalf("SavePlan rewrote a completed claim's response: %s", got.Response)
	}
}
