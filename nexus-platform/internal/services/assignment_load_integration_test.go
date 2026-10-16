//go:build integration

// ASSIGN-2 — load_based strategy acceptance tests for AssignmentService over REAL MySQL. These prove
// the pick is LOAD-driven (not round-robin), the cursor tie-break rotates deterministically among
// equal-load members, soft-deleted assigned records do NOT count as load, empty team no-ops, and the
// count query is tenant-scoped. Run with:
//
//	go test -tags=integration ./internal/services/ -run TestAssignment -race
//
// They reuse ASSIGN-1's scaffolding (o3aSetup throwaway DB, asgSeedTeam, asgExec) and add two
// obj_* helpers (asgCreateObjTable, asgSeedLoad) to seed the "load" the resolver counts.
package services

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

// asgCreateObjTable creates a minimal obj_<entity> table carrying only the columns the load count
// touches: tenant_id, deleted_at, and the JSON data column. It mirrors the mandatory obj_* shape
// (database.md) without the full field set — enough for countActiveLoads.
func asgCreateObjTable(t *testing.T, db *sqlx.DB, entity string) {
	t.Helper()
	asgExec(t, db, "CREATE TABLE IF NOT EXISTS `obj_"+entity+"` ("+
		"`id` CHAR(36) NOT NULL PRIMARY KEY,"+
		"`tenant_id` CHAR(36) NOT NULL,"+
		"`created_by` CHAR(36) NOT NULL,"+
		"`deleted_at` TIMESTAMP NULL DEFAULT NULL,"+
		"`data` JSON DEFAULT NULL,"+
		"INDEX `idx_tenant` (`tenant_id`),"+
		"INDEX `idx_deleted_at` (`deleted_at`)"+
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci")
}

// asgSeedLoad inserts `active` live + `deleted` soft-deleted records of `entity` in `tenant`, each
// assigned to `assignee` at JSON field `field`. The soft-deleted rows must NOT count as load (the
// deleted_at IS NULL filter), so seeding both proves the exclusion.
func asgSeedLoad(t *testing.T, db *sqlx.DB, tenant, entity, field, assignee string, active, deleted int) {
	t.Helper()
	data := fmt.Sprintf("{%q:%q}", field, assignee)
	for i := 0; i < active; i++ {
		asgExec(t, db, "INSERT INTO `obj_"+entity+"` (id, tenant_id, created_by, data) VALUES (?, ?, ?, ?)",
			uuid.NewString(), tenant, "creator", data)
	}
	for i := 0; i < deleted; i++ {
		asgExec(t, db, "INSERT INTO `obj_"+entity+"` (id, tenant_id, created_by, deleted_at, data) VALUES (?, ?, ?, NOW(), ?)",
			uuid.NewString(), tenant, "creator", data)
	}
}

// TestAssignment_LoadBasedPicksLeastLoaded is THE discriminating load test: with the ring ordered
// m1<m2<m3 by id and seeded loads m1=3, m2=1, m3=2, round-robin's first pick would be m1 (ring[0]) —
// load_based instead picks m2 (the fewest). That divergence proves the pick is load-DRIVEN, and the
// most-loaded member (m1) is never chosen while a lighter one exists. After simulating the m2
// assignment landing (m2→2), the next fire moves to a new minimum (m3), not back to m1.
func TestAssignment_LoadBasedPicksLeastLoaded(t *testing.T) {
	_, sdb := o3aSetup(t)
	svc := NewAssignmentService(sdb, zap.NewNop())
	ctx := context.Background()
	const tenant = "aaaaaaaa-6666-6666-6666-aaaaaaaaaaaa"
	const team = "bbbbbbbb-6666-6666-6666-bbbbbbbbbbbb"
	const entity = "tickets"
	const field = "handler"
	m1 := "11111111-1111-1111-1111-111111111111"
	m2 := "22222222-2222-2222-2222-222222222222"
	m3 := "33333333-3333-3333-3333-333333333333"
	ring := asgSeedTeam(t, sdb, tenant, team, []string{m1, m2, m3})
	if ring[0] != m1 { // sanity: ring[0] is what round-robin would pick first
		t.Fatalf("ring order unexpected: %v", ring)
	}
	asgCreateObjTable(t, sdb, entity)
	asgSeedLoad(t, sdb, tenant, entity, field, m1, 3, 0) // most loaded — must never be picked here
	asgSeedLoad(t, sdb, tenant, entity, field, m2, 1, 0) // least loaded
	asgSeedLoad(t, sdb, tenant, entity, field, m3, 2, 0)

	// Fire 1: least-loaded is m2 (load 1). Round-robin would have picked m1 (ring[0]) — this is the
	// load-vs-round-robin discriminator.
	got, ok, err := svc.LeastLoadedAssignee(ctx, tenant, "wf-load", "a0", entity, field, team)
	if err != nil || !ok {
		t.Fatalf("fire 1: got=%q ok=%v err=%v", got, ok, err)
	}
	if got != m2 {
		t.Fatalf("fire 1: assigned %q; want m2 (%q) — the least-loaded, NOT round-robin's ring[0]=%q", got, m2, m1)
	}

	// Simulate the assignment landing (runAssign writes the field after the resolver returns): m2 now
	// has 2 active records, tying m3 at 2 while m1 stays at 3.
	asgSeedLoad(t, sdb, tenant, entity, field, m2, 1, 0)

	// Fire 2: min is now 2 (m2 and m3 tied); the cursor advanced to m2 on fire 1, so nextInRing among
	// the tied set [m2,m3] rotates to m3. m1 (load 3) is still never chosen.
	got2, ok, err := svc.LeastLoadedAssignee(ctx, tenant, "wf-load", "a0", entity, field, team)
	if err != nil || !ok {
		t.Fatalf("fire 2: got=%q ok=%v err=%v", got2, ok, err)
	}
	if got2 == m1 {
		t.Fatalf("fire 2 picked the MOST-loaded member m1 (%q) — load ordering broken", m1)
	}
	if got2 != m3 {
		t.Fatalf("fire 2: assigned %q; want the new min m3 (%q) via cursor tie-break", got2, m3)
	}
}

// TestAssignment_LoadBasedTieBreakRotates proves the tie-break is deterministic AND rotating: with
// two members at equal minimum load (m1=m2=0) and a third heavier (m3=5), two consecutive fires —
// with the load held fixed (the resolver does NOT write the assignment) — rotate m1 → m2 via the
// round-robin cursor, never touching the heavier m3.
func TestAssignment_LoadBasedTieBreakRotates(t *testing.T) {
	_, sdb := o3aSetup(t)
	svc := NewAssignmentService(sdb, zap.NewNop())
	ctx := context.Background()
	const tenant = "aaaaaaaa-7777-7777-7777-aaaaaaaaaaaa"
	const team = "bbbbbbbb-7777-7777-7777-bbbbbbbbbbbb"
	const entity = "tickets"
	const field = "handler"
	m1 := "11111111-1111-1111-1111-111111111111"
	m2 := "22222222-2222-2222-2222-222222222222"
	m3 := "33333333-3333-3333-3333-333333333333"
	asgSeedTeam(t, sdb, tenant, team, []string{m1, m2, m3})
	asgCreateObjTable(t, sdb, entity)
	asgSeedLoad(t, sdb, tenant, entity, field, m3, 5, 0) // heavier — excluded from the min tie set

	// Fire 1: tied set is [m1, m2] (both load 0), cursor empty → nextInRing picks m1.
	g1, ok, err := svc.LeastLoadedAssignee(ctx, tenant, "wf-tie", "a0", entity, field, team)
	if err != nil || !ok {
		t.Fatalf("fire 1: ok=%v err=%v", ok, err)
	}
	// Fire 2: load unchanged (no write), cursor now at m1 → nextInRing rotates to m2.
	g2, ok, err := svc.LeastLoadedAssignee(ctx, tenant, "wf-tie", "a0", entity, field, team)
	if err != nil || !ok {
		t.Fatalf("fire 2: ok=%v err=%v", ok, err)
	}
	if g1 == m3 || g2 == m3 {
		t.Fatalf("heavier member m3 must never be picked; got g1=%q g2=%q", g1, g2)
	}
	if g1 != m1 || g2 != m2 {
		t.Fatalf("tie-break rotation: got g1=%q g2=%q; want m1 (%q) then m2 (%q)", g1, g2, m1, m2)
	}
	if g1 == g2 {
		t.Fatalf("two equal-load fires must ROTATE, both picked %q", g1)
	}
}

// TestAssignment_LoadBasedExcludesDeleted proves soft-deleted assigned records are NOT counted as
// load: m1 has 1 live + 2 soft-deleted records, m2 has 2 live. If the deleted rows counted, m1's load
// would be 3 (> m2's 2) and load_based would pick m2; excluding them makes m1's load 1 (< 2), so the
// pick is m1. Asserting m1 proves the deleted_at IS NULL filter.
func TestAssignment_LoadBasedExcludesDeleted(t *testing.T) {
	_, sdb := o3aSetup(t)
	svc := NewAssignmentService(sdb, zap.NewNop())
	ctx := context.Background()
	const tenant = "aaaaaaaa-8888-8888-8888-aaaaaaaaaaaa"
	const team = "bbbbbbbb-8888-8888-8888-bbbbbbbbbbbb"
	const entity = "tickets"
	const field = "handler"
	m1 := "11111111-1111-1111-1111-111111111111"
	m2 := "22222222-2222-2222-2222-222222222222"
	asgSeedTeam(t, sdb, tenant, team, []string{m1, m2})
	asgCreateObjTable(t, sdb, entity)
	asgSeedLoad(t, sdb, tenant, entity, field, m1, 1, 2) // 1 live + 2 soft-deleted → real load 1
	asgSeedLoad(t, sdb, tenant, entity, field, m2, 2, 0) // 2 live → load 2

	got, ok, err := svc.LeastLoadedAssignee(ctx, tenant, "wf-del", "a0", entity, field, team)
	if err != nil || !ok {
		t.Fatalf("got=%q ok=%v err=%v", got, ok, err)
	}
	if got != m1 {
		t.Fatalf("assigned %q; want m1 (%q) — its 2 soft-deleted records must NOT count as load", got, m1)
	}
}

// TestAssignment_LoadBasedEmptyTeamNoOp mirrors the round-robin empty-team no-op for load_based: a
// team whose only members are ineligible yields no assignee → ok=false, err=nil (fail-open), with the
// cursor ensure still committed (no dangling tx). No obj_ rows exist — the resolver must short-circuit
// on the empty member list BEFORE the count query.
func TestAssignment_LoadBasedEmptyTeamNoOp(t *testing.T) {
	_, sdb := o3aSetup(t)
	svc := NewAssignmentService(sdb, zap.NewNop())
	ctx := context.Background()
	const tenant = "aaaaaaaa-9999-9999-9999-aaaaaaaaaaaa"
	const team = "bbbbbbbb-9999-9999-9999-bbbbbbbbbbbb"
	asgSeedTeam(t, sdb, tenant, team, nil) // only disabled + soft-deleted members

	got, ok, err := svc.LeastLoadedAssignee(ctx, tenant, "wf-empty-load", "a0", "tickets", "handler", team)
	if err != nil {
		t.Fatalf("empty team should not error (fail-open): %v", err)
	}
	if ok || got != "" {
		t.Fatalf("empty team: got=%q ok=%v; want ok=false (no eligible member)", got, ok)
	}
	var n int
	if err := sdb.Get(&n, "SELECT COUNT(*) FROM sys_assignment_cursors WHERE tenant_id=? AND workflow_id=? AND action_key=?",
		tenant, "wf-empty-load", "a0"); err != nil {
		t.Fatalf("count cursor: %v", err)
	}
	if n != 1 {
		t.Fatalf("cursor rows = %d; want 1 (ensure committed even on the no-op path)", n)
	}
}

// TestAssignment_LoadBasedTenantIsolation proves the count query is tenant-scoped: another tenant's
// records assigned to the SAME user id must NOT inflate this tenant's load. In tenant A, m1 has ZERO
// records and m2 has 3, so load_based must pick m1. Tenant B holds 5 records assigned to m1's exact id
// in the SAME shared obj_tickets table — if the count leaked across tenants, m1 would read 5 (> m2's
// 3) and the pick would flip to m2. Asserting m1 proves the `tenant_id = ?` filter.
func TestAssignment_LoadBasedTenantIsolation(t *testing.T) {
	_, sdb := o3aSetup(t)
	svc := NewAssignmentService(sdb, zap.NewNop())
	ctx := context.Background()
	const tenantA = "aaaaaaaa-abab-abab-abab-aaaaaaaaaaaa"
	const tenantB = "cccccccc-abab-abab-abab-cccccccccccc"
	const team = "bbbbbbbb-abab-abab-abab-bbbbbbbbbbbb"
	const entity = "tickets"
	const field = "handler"
	m1 := "11111111-1111-1111-1111-111111111111"
	m2 := "22222222-2222-2222-2222-222222222222"
	asgSeedTeam(t, sdb, tenantA, team, []string{m1, m2})
	asgCreateObjTable(t, sdb, entity)
	// Tenant A: m1 has NO records, m2 has 3.
	asgSeedLoad(t, sdb, tenantA, entity, field, m2, 3, 0)
	// Tenant B: 5 records assigned to m1's exact id — must be invisible to tenant A's count.
	asgSeedLoad(t, sdb, tenantB, entity, field, m1, 5, 0)

	got, ok, err := svc.LeastLoadedAssignee(ctx, tenantA, "wf-iso", "a0", entity, field, team)
	if err != nil || !ok {
		t.Fatalf("got=%q ok=%v err=%v", got, ok, err)
	}
	if got != m1 {
		t.Fatalf("CROSS-TENANT LOAD LEAK: assigned %q; want m1 (%q) — tenant B's 5 records for m1 must not count in tenant A", got, m1)
	}
}
