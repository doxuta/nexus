//go:build integration

// ASSIGN-1 — the concurrency + round-robin acceptance tests for AssignmentService over REAL MySQL
// (o3aSetup throwaway DB, migrated to head incl. 000049). The no-double-assign test is the gate the
// advisor called out: an in-memory fake would pass even with FOR UPDATE dropped, so it MUST run
// against a real InnoDB row lock. Run with:
//
//	go test -tags=integration ./internal/services/ -run TestAssignment -race
package services

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

func asgExec(t *testing.T, db *sqlx.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// asgSeedTeam creates a tenant, `active` members + one disabled + one soft-deleted member (both must
// be EXCLUDED), a team, and memberships. Returns the sorted list of eligible member ids (the ring).
func asgSeedTeam(t *testing.T, db *sqlx.DB, tenantID, teamID string, activeIDs []string) []string {
	t.Helper()
	asgExec(t, db, "INSERT INTO tenants (id, slug, name) VALUES (?, ?, ?)", tenantID, "t-"+tenantID[:8], "T-"+tenantID[:8])
	mkUser := func(id, status string, deleted bool) {
		del := "NULL"
		if deleted {
			del = "NOW()"
		}
		asgExec(t, db, "INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin, deleted_at) VALUES (?, ?, ?, ?, ?, ?, 0, "+del+")",
			id, tenantID, id+"@t.vn", "x", "U "+id[:4], status)
	}
	for _, id := range activeIDs {
		mkUser(id, "active", false)
	}
	// Two ineligible members that share the team but must never be assigned. IDs derived from the
	// tenant so the helper can be called for more than one tenant in a test without a PK collision.
	disabledID := "dddddddd-0000-0000-0000-" + tenantID[:12]
	deletedID := "dddddddd-0001-0000-0000-" + tenantID[:12]
	mkUser(disabledID, "disabled", false)
	mkUser(deletedID, "active", true)

	asgExec(t, db, "INSERT INTO teams (id, tenant_id, name) VALUES (?, ?, ?)", teamID, tenantID, "Team")
	i := 0
	for _, uid := range append(append([]string{}, activeIDs...), disabledID, deletedID) {
		// Membership id must be unique across tenants (helper called per-tenant in one test).
		asgExec(t, db, "INSERT INTO team_memberships (id, user_id, team_id) VALUES (?, ?, ?)",
			"eeee"+teamID[4:24]+pad12(i), uid, teamID)
		i++
	}
	ring := append([]string{}, activeIDs...)
	sort.Strings(ring) // the resolver orders by user_id
	return ring
}

func pad12(i int) string {
	s := "000000000000"
	d := []byte(s)
	n := len(d) - 1
	for i > 0 && n >= 0 {
		d[n] = byte('0' + i%10)
		i /= 10
		n--
	}
	return string(d)
}

func TestAssignment_RoundRobinSequenceAndFilter(t *testing.T) {
	_, sdb := o3aSetup(t)
	svc := NewAssignmentService(sdb, zap.NewNop())
	ctx := context.Background()
	const tenant = "aaaaaaaa-1111-1111-1111-aaaaaaaaaaaa"
	const team = "bbbbbbbb-1111-1111-1111-bbbbbbbbbbbb"
	ring := asgSeedTeam(t, sdb, tenant, team, []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
	})

	// 4 sequential fires → ring[0],[1],[2],[0] (wrap). Disabled + deleted members never appear.
	want := []string{ring[0], ring[1], ring[2], ring[0]}
	for i, w := range want {
		got, ok, err := svc.NextAssignee(ctx, tenant, "wf-1", "a0", team)
		if err != nil || !ok {
			t.Fatalf("fire %d: got=%q ok=%v err=%v", i, got, ok, err)
		}
		if got != w {
			t.Fatalf("fire %d: assigned %q, want %q (ring=%v)", i, got, w, ring)
		}
		if got == "dddddddd-0000-0000-0000-000000000001" || got == "dddddddd-0000-0000-0000-000000000002" {
			t.Fatalf("fire %d assigned an ineligible (disabled/deleted) member: %q", i, got)
		}
	}
}

// TestAssignment_EmptyTeamNoOp covers spec §9 #4 against real MySQL: a team whose only members are
// ineligible (disabled + soft-deleted) yields NO assignee → NextAssignee commits the harmless ensure
// and returns ok=false, err=nil (fail-open), leaving a committed cursor row (no dangling tx).
func TestAssignment_EmptyTeamNoOp(t *testing.T) {
	_, sdb := o3aSetup(t)
	svc := NewAssignmentService(sdb, zap.NewNop())
	ctx := context.Background()
	const tenant = "aaaaaaaa-5555-5555-5555-aaaaaaaaaaaa"
	const team = "bbbbbbbb-5555-5555-5555-bbbbbbbbbbbb"
	asgSeedTeam(t, sdb, tenant, team, nil) // only the disabled + soft-deleted members exist

	got, ok, err := svc.NextAssignee(ctx, tenant, "wf-empty", "a0", team)
	if err != nil {
		t.Fatalf("empty team should not error (fail-open): %v", err)
	}
	if ok || got != "" {
		t.Fatalf("empty team: got=%q ok=%v; want ok=false (no eligible member)", got, ok)
	}
	// The ensure INSERT must have been committed (row exists) — no dangling tx.
	var n int
	if err := sdb.Get(&n, "SELECT COUNT(*) FROM sys_assignment_cursors WHERE tenant_id=? AND workflow_id=? AND action_key=?",
		tenant, "wf-empty", "a0"); err != nil {
		t.Fatalf("count cursor: %v", err)
	}
	if n != 1 {
		t.Fatalf("cursor rows = %d; want 1 (ensure committed even on the no-op path)", n)
	}
}

// TestAssignment_TenantIsolation closes the security-audit LOW: the ONLY thing isolating tenants in
// the member query is `u.tenant_id = ?`. This seeds tenant B's user as a (corrupt) member of tenant
// A's team and asserts A's assignment NEVER returns B's user — and that A and B keep INDEPENDENT
// cursors under the SAME (workflow_id, action_key).
func TestAssignment_TenantIsolation(t *testing.T) {
	_, sdb := o3aSetup(t)
	svc := NewAssignmentService(sdb, zap.NewNop())
	ctx := context.Background()
	const tenantA = "aaaaaaaa-3333-3333-3333-aaaaaaaaaaaa"
	const tenantB = "cccccccc-3333-3333-3333-cccccccccccc"
	const teamA = "bbbbbbbb-3333-3333-3333-bbbbbbbbbbbb"
	const teamB = "bbbbbbbb-4444-4444-4444-bbbbbbbbbbbb"
	ringA := asgSeedTeam(t, sdb, tenantA, teamA, []string{
		"a1111111-1111-1111-1111-111111111111",
		"a2222222-2222-2222-2222-222222222222",
	})
	ringB := asgSeedTeam(t, sdb, tenantB, teamB, []string{
		"b1111111-1111-1111-1111-111111111111",
	})

	// Corrupt cross-link: tenant B's user is ALSO a membership row of tenant A's team. Only the
	// u.tenant_id = A filter keeps it out of A's ring.
	asgExec(t, sdb, "INSERT INTO team_memberships (id, user_id, team_id) VALUES (?, ?, ?)",
		"ffffffff-0000-0000-0000-000000000001", ringB[0], teamA)

	aSet := map[string]bool{ringA[0]: true, ringA[1]: true}
	// SAME workflow_id + action_key for both tenants → cursors distinguished only by tenant_id.
	for i := 0; i < 6; i++ {
		got, ok, err := svc.NextAssignee(ctx, tenantA, "wf-shared", "a0", teamA)
		if err != nil || !ok {
			t.Fatalf("A fire %d: ok=%v err=%v", i, ok, err)
		}
		if got == ringB[0] {
			t.Fatalf("CROSS-TENANT LEAK: tenant A assigned tenant B's user %q (u.tenant_id filter dropped)", got)
		}
		if !aSet[got] {
			t.Fatalf("A fire %d returned %q, not one of A's members %v", i, got, ringA)
		}
	}
	// Tenant B on the SAME (workflow_id, action_key) gets its own user from its own cursor.
	gotB, ok, err := svc.NextAssignee(ctx, tenantB, "wf-shared", "a0", teamB)
	if err != nil || !ok || gotB != ringB[0] {
		t.Fatalf("B assign = %q ok=%v err=%v; want %q (independent cursor)", gotB, ok, err, ringB[0])
	}
}

// TestAssignment_ConcurrentNoDoubleAssign is THE gate: N goroutines fire the SAME rule concurrently.
// The FOR UPDATE row lock serializes the advance, so with N == len(ring) the picks must be EXACTLY
// the ring (each member once, no duplicate). If FOR UPDATE were dropped, concurrent fires would read
// the same last_user_id and collide on one member. Run under -race.
func TestAssignment_ConcurrentNoDoubleAssign(t *testing.T) {
	_, sdb := o3aSetup(t)
	// A slightly larger pool + higher concurrency stresses the lock harder.
	sdb.SetMaxOpenConns(16)
	svc := NewAssignmentService(sdb, zap.NewNop())
	ctx := context.Background()
	const tenant = "aaaaaaaa-2222-2222-2222-aaaaaaaaaaaa"
	const team = "bbbbbbbb-2222-2222-2222-bbbbbbbbbbbb"
	active := []string{
		"11111111-0000-0000-0000-000000000001",
		"11111111-0000-0000-0000-000000000002",
		"11111111-0000-0000-0000-000000000003",
		"11111111-0000-0000-0000-000000000004",
		"11111111-0000-0000-0000-000000000005",
	}
	ring := asgSeedTeam(t, sdb, tenant, team, active)

	const n = 5 // == len(ring): one full rotation, concurrently
	picks := make([]string, n)
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together for maximum contention
			got, ok, err := svc.NextAssignee(ctx, tenant, "wf-cc", "a0", team)
			if err != nil {
				errs[i] = err
				return
			}
			if !ok {
				t.Errorf("goroutine %d: ok=false unexpectedly", i)
			}
			picks[i] = got
		}(i)
	}
	close(start)
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			t.Fatalf("concurrent NextAssignee error: %v", e)
		}
	}

	got := append([]string{}, picks...)
	sort.Strings(got)
	// Exactly one full rotation → the sorted picks must equal the ring (no member assigned twice).
	if len(got) != len(ring) {
		t.Fatalf("picks=%d, want %d", len(got), len(ring))
	}
	for i := range ring {
		if got[i] != ring[i] {
			t.Fatalf("DOUBLE-ASSIGN: %d concurrent fires did not cover the ring exactly.\n got=%v\nring=%v\n(a duplicate ⇒ FOR UPDATE is not serializing)", n, got, ring)
		}
	}
}
