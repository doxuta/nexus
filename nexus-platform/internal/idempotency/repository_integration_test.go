//go:build integration

// Integration tests for the sys_idempotency_keys store (migration 000069) against real MySQL 8:
//
//	go test -tags=integration -race ./internal/idempotency/
package idempotency

import (
	"context"
	"errors"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

func testDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLRepository_ClaimLifecycle(t *testing.T) {
	db := testDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()
	const (
		tA, tB = "aaaa1111-0000-0000-0000-0000000idem1", "bbbb2222-0000-0000-0000-0000000idem2"
		user   = "user-idem-1"
	)
	clean := func() { db.ExecContext(ctx, "DELETE FROM sys_idempotency_keys WHERE tenant_id IN (?, ?)", tA, tB) }
	clean()
	t.Cleanup(clean)

	exp := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	claim := Claim{TenantID: tA, PrincipalID: user, Key: "abc-1", EntityName: "crm_contacts", RequestHash: "h1", RecordID: "rec-1", ExpiresAt: exp}
	if err := repo.Claim(ctx, claim); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := repo.Claim(ctx, claim); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("second claim = %v, want ErrKeyExists", err)
	}
	// The key is scoped: another tenant, another principal, another case are all free.
	for _, c := range []Claim{
		{TenantID: tB, PrincipalID: user, Key: "abc-1", EntityName: "crm_contacts", RequestHash: "h1", RecordID: "rec-b", ExpiresAt: exp},
		{TenantID: tA, PrincipalID: "user-idem-2", Key: "abc-1", EntityName: "crm_contacts", RequestHash: "h1", RecordID: "rec-2", ExpiresAt: exp},
		{TenantID: tA, PrincipalID: user, Key: "ABC-1", EntityName: "crm_contacts", RequestHash: "h1", RecordID: "rec-3", ExpiresAt: exp},
	} {
		if err := repo.Claim(ctx, c); err != nil {
			t.Fatalf("claim %+v: %v (keys are per tenant+principal and case-sensitive)", c, err)
		}
	}

	got, err := repo.Get(ctx, tA, user, "abc-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// specs/047 FR-11: the claim names its record id from the start.
	if got.Status != StatusProcessing || got.RecordID != "rec-1" || got.RequestHash != "h1" || got.EntityName != "crm_contacts" || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("claim read back = %+v, want processing/rec-1/h1/crm_contacts expiring %s", got, exp)
	}
	if _, err := repo.Get(ctx, tB, "user-idem-2", "abc-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get of an unclaimed (tenant, principal) = %v, want ErrNotFound", err)
	}

	// Complete and Release are fenced by the claim's record id: a request that no longer owns the
	// claim (it was taken over after its lease) cannot complete or release it.
	if err := repo.Complete(ctx, tA, user, "abc-1", "rec-other"); err != nil {
		t.Fatalf("complete (foreign id): %v", err)
	}
	if err := repo.Release(ctx, tA, user, "abc-1", "rec-other"); err != nil {
		t.Fatalf("release (foreign id): %v", err)
	}
	if got, _ = repo.Get(ctx, tA, user, "abc-1"); got == nil || got.Status != StatusProcessing || got.RecordID != "rec-1" {
		t.Fatalf("a foreign record id completed or released the claim: %+v", got)
	}

	if err := repo.Complete(ctx, tA, user, "abc-1", "rec-1"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, _ = repo.Get(ctx, tA, user, "abc-1")
	if got.Status != StatusCompleted || got.RecordID != "rec-1" {
		t.Fatalf("after complete = %+v, want completed rec-1", got)
	}
	// Release never removes a completed claim; tenant B's twin is untouched by tenant A's writes.
	if err := repo.Release(ctx, tA, user, "abc-1", "rec-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := repo.Get(ctx, tA, user, "abc-1"); err != nil {
		t.Fatalf("a completed claim was released: %v", err)
	}
	// Nor is a completed claim ever taken over.
	if won, err := repo.Takeover(ctx, tA, user, "abc-1", "rec-1", "rec-9", exp); err != nil || won {
		t.Fatalf("takeover of a completed claim = (%v, %v), want (false, nil)", won, err)
	}
	if twin, _ := repo.Get(ctx, tB, user, "abc-1"); twin == nil || twin.Status != StatusProcessing {
		t.Fatalf("tenant B's claim changed by tenant A's complete: %+v", twin)
	}

	// Takeover: exactly one of two racers holding the stale id wins; the claim then carries the
	// winner's id and expiry, and the loser's id no longer releases it.
	later := exp.Add(time.Hour)
	if won, err := repo.Takeover(ctx, tB, user, "abc-1", "rec-b", "rec-b2", later); err != nil || !won {
		t.Fatalf("first takeover = (%v, %v), want (true, nil)", won, err)
	}
	if won, err := repo.Takeover(ctx, tB, user, "abc-1", "rec-b", "rec-b3", later); err != nil || won {
		t.Fatalf("second takeover with the same stale id = (%v, %v), want (false, nil)", won, err)
	}
	if twin, _ := repo.Get(ctx, tB, user, "abc-1"); twin == nil || twin.RecordID != "rec-b2" || !twin.ExpiresAt.Equal(later) {
		t.Fatalf("claim after takeover = %+v, want rec-b2 expiring %s", twin, later)
	}
	if err := repo.Release(ctx, tB, user, "abc-1", "rec-b"); err != nil {
		t.Fatalf("release B (stale id): %v", err)
	}
	if _, err := repo.Get(ctx, tB, user, "abc-1"); err != nil {
		t.Fatalf("the taken-over claim was released by its previous owner: %v", err)
	}
	if err := repo.Release(ctx, tB, user, "abc-1", "rec-b2"); err != nil {
		t.Fatalf("release B: %v", err)
	}
	if _, err := repo.Get(ctx, tB, user, "abc-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("processing claim survived release: %v", err)
	}

	// FR-12 bulk / import claims hold no record id: Release matches them NULL-safely, and
	// CompleteResponse stores the response byte for byte (no JSON normalisation) for the replay.
	bulk := Claim{TenantID: tB, PrincipalID: user, Key: "bulk-1", EntityName: "crm_contacts", RequestHash: "hb", ExpiresAt: exp}
	if err := repo.Claim(ctx, bulk); err != nil {
		t.Fatalf("bulk claim: %v", err)
	}
	if err := repo.Release(ctx, tB, user, "bulk-1", ""); err != nil {
		t.Fatalf("release bulk: %v", err)
	}
	if _, err := repo.Get(ctx, tB, user, "bulk-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a bulk claim (no record id) survived Release: %v", err)
	}
	if err := repo.Claim(ctx, bulk); err != nil {
		t.Fatalf("bulk re-claim: %v", err)
	}
	stored := []byte(`{"mode":"partial","created":1,"results":[{"index":0,"status":"created","id":"r-1"}],"note":"Tiếng Việt"}`)
	if err := repo.CompleteResponse(ctx, tB, user, "bulk-1", "", stored); err != nil {
		t.Fatalf("complete response: %v", err)
	}
	got, err = repo.Get(ctx, tB, user, "bulk-1")
	if err != nil || got.Status != StatusCompleted || string(got.Response) != string(stored) {
		t.Fatalf("bulk claim after CompleteResponse = (%+v, %v), want completed with the exact stored bytes", got, err)
	}
	// CompleteResponse never touches a create claim (which holds a record id).
	if err := repo.Claim(ctx, Claim{TenantID: tB, PrincipalID: user, Key: "one-1", EntityName: "crm_contacts", RequestHash: "h1", RecordID: "rec-9", ExpiresAt: exp}); err != nil {
		t.Fatalf("create claim: %v", err)
	}
	if err := repo.CompleteResponse(ctx, tB, user, "one-1", "", stored); err != nil {
		t.Fatalf("complete response on a create claim: %v", err)
	}
	if got, _ = repo.Get(ctx, tB, user, "one-1"); got == nil || got.Status != StatusProcessing || got.Response != nil {
		t.Fatalf("CompleteResponse changed a create claim: %+v", got)
	}

	// DeleteExpired only removes a claim that is past its window.
	if err := repo.DeleteExpired(ctx, tA, user, "abc-1", time.Now()); err != nil {
		t.Fatalf("delete expired (live): %v", err)
	}
	if _, err := repo.Get(ctx, tA, user, "abc-1"); err != nil {
		t.Fatalf("a live claim was deleted as expired: %v", err)
	}
	if err := repo.DeleteExpired(ctx, tA, user, "abc-1", exp.Add(time.Second)); err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	if _, err := repo.Get(ctx, tA, user, "abc-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired claim survived DeleteExpired: %v", err)
	}
}
