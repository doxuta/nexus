//go:build integration

// ENG #6 auto_number — atomic sequence claim (dynrepo.ClaimSequence) integration tests.
//
// Run with:
//
//	go test -tags=integration -race ./internal/dynrepo/ -run TestClaimSequence
//
// Requires a reachable MySQL 8 (TEST_DATABASE_URL or the local compose DSN) with migration
// 000029 (sys_sequences) applied. Every test claims from its OWN unique (tenant, entity, field)
// triple and cleans its own rows up in t.Cleanup, so tests never collide with each other or with
// a shared nexus_platform dev DB.
package dynrepo

import (
	"context"
	"sync"
	"testing"
)

// TestClaimSequence_FirstClaimReturnsOne proves the atomic claim's core arithmetic: a brand-new
// (tenant, entity, field) triple's FIRST claim returns 1 (not 0, not 2), and each subsequent
// claim on the same triple returns the next integer in order. This is the empirical proof for
// the LAST_INSERT_ID(1) correction documented in migrations/000029 and internal/dynrepo/sequence.go
// (the task's literal "VALUES (?,?,?,2) ON DUPLICATE KEY UPDATE next_val=LAST_INSERT_ID(next_val)+1"
// SQL returns 0 on a fresh insert — the ON DUPLICATE branch never runs on the first row, so
// LAST_INSERT_ID(expr) is never invoked and the driver's LastInsertId() stays 0).
func TestClaimSequence_FirstClaimReturnsOne(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const tenant = "11111111-seq1-0000-0000-000000000001"
	const entity = "it_seq_invoices"
	const field = "invoice_no"

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM sys_sequences WHERE tenant_id = ? AND entity_name = ? AND field_name = ?", tenant, entity, field)
	})

	first, err := repo.ClaimSequence(ctx, tenant, entity, field)
	if err != nil {
		t.Fatalf("ClaimSequence (first): %v", err)
	}
	if first != 1 {
		t.Fatalf("first claim = %d; want 1", first)
	}

	second, err := repo.ClaimSequence(ctx, tenant, entity, field)
	if err != nil {
		t.Fatalf("ClaimSequence (second): %v", err)
	}
	if second != 2 {
		t.Fatalf("second claim = %d; want 2", second)
	}

	third, err := repo.ClaimSequence(ctx, tenant, entity, field)
	if err != nil {
		t.Fatalf("ClaimSequence (third): %v", err)
	}
	if third != 3 {
		t.Fatalf("third claim = %d; want 3", third)
	}
}

// TestClaimSequence_ConcurrentClaims_NoDuplicates fires 8 goroutines at the SAME (tenant, entity,
// field) triple concurrently (start-barrier synchronized so the overlap is real, not incidental)
// and asserts every claimed number is distinct and the set is exactly {1..8} — no duplicate, no
// gap, no lost update under real MySQL row-level locking on the UPSERT.
func TestClaimSequence_ConcurrentClaims_NoDuplicates(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const tenant = "11111111-seq2-0000-0000-000000000002"
	const entity = "it_seq_orders"
	const field = "order_no"
	const n = 8

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM sys_sequences WHERE tenant_id = ? AND entity_name = ? AND field_name = ?", tenant, entity, field)
	})

	var (
		wg      sync.WaitGroup
		barrier = make(chan struct{})
		mu      sync.Mutex
		results = make([]int64, 0, n)
		errs    = make([]error, 0)
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier // all goroutines release together — forces real overlap
			v, err := repo.ClaimSequence(ctx, tenant, entity, field)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, v)
		}()
	}
	close(barrier)
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("ClaimSequence errors under concurrency: %v", errs)
	}
	if len(results) != n {
		t.Fatalf("got %d results; want %d", len(results), n)
	}
	seen := make(map[int64]bool, n)
	for _, v := range results {
		if seen[v] {
			t.Errorf("duplicate claimed value %d — concurrent claims collided", v)
		}
		seen[v] = true
	}
	for want := int64(1); want <= n; want++ {
		if !seen[want] {
			t.Errorf("missing claimed value %d — expected {1..%d} exactly", want, n)
		}
	}
}

// TestClaimSequence_TenantIsolation proves two tenants sharing the same entity+field NAME (the
// shared obj_* table model, per rules/database.md §obj_* tables) count INDEPENDENTLY: claiming
// for tenant A must never advance tenant B's counter.
func TestClaimSequence_TenantIsolation(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const tenantA = "11111111-seq3-0000-0000-00000000000a"
	const tenantB = "11111111-seq3-0000-0000-00000000000b"
	const entity = "it_seq_tickets"
	const field = "ticket_no"

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM sys_sequences WHERE entity_name = ? AND field_name = ? AND tenant_id IN (?, ?)", entity, field, tenantA, tenantB)
	})

	a1, err := repo.ClaimSequence(ctx, tenantA, entity, field)
	if err != nil {
		t.Fatalf("ClaimSequence tenantA (1st): %v", err)
	}
	if a1 != 1 {
		t.Fatalf("tenantA first claim = %d; want 1", a1)
	}
	a2, err := repo.ClaimSequence(ctx, tenantA, entity, field)
	if err != nil {
		t.Fatalf("ClaimSequence tenantA (2nd): %v", err)
	}
	if a2 != 2 {
		t.Fatalf("tenantA second claim = %d; want 2", a2)
	}

	// tenantB's first claim must be 1 too (own counter), not 3 (would prove leakage into A's).
	b1, err := repo.ClaimSequence(ctx, tenantB, entity, field)
	if err != nil {
		t.Fatalf("ClaimSequence tenantB (1st): %v", err)
	}
	if b1 != 1 {
		t.Fatalf("tenantB first claim = %d; want 1 (tenant isolation broken — leaked tenantA's counter)", b1)
	}

	// tenantA's next claim must be 3 (tenantB's claim did not advance it).
	a3, err := repo.ClaimSequence(ctx, tenantA, entity, field)
	if err != nil {
		t.Fatalf("ClaimSequence tenantA (3rd): %v", err)
	}
	if a3 != 3 {
		t.Fatalf("tenantA third claim = %d; want 3 (tenantB's claim must not advance tenantA's counter)", a3)
	}
}
