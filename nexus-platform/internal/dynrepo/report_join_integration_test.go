//go:build integration

// Integration tests for REPORT-JOIN v1 (../specs/025-report-join/spec.md) at the dynrepo layer — the
// SQL that makes a cross-object aggregate JOIN safe. These are the DISCRIMINATING tests the spec
// (§7) mandates: they run against MySQL and prove the per-hop tenant filter + A5 row-level
// predicate are actually enforced in the emitted SQL, not merely present in the Go.
//
// Run with:  go test -tags=integration ./internal/dynrepo/
package dynrepo

import (
	"context"
	"testing"

	"nexus/platform/internal/ddl"
)

// seedJoinFixture builds a parent (accounts: industry) + child (contacts: account relation)
// pair and returns the repo. Child.account_id is a STORED generated column from data.account.
func seedJoinFixture(t *testing.T) (*Repository, context.Context, func()) {
	t.Helper()
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()

	_ = repo.DropTable(ctx, "it_rj_contacts") // child first (no real FK constraint, but tidy)
	_ = repo.DropTable(ctx, "it_rj_accounts")
	if err := repo.CreateTable(ctx, "it_rj_accounts", []ddl.FieldDef{
		{Name: "industry", FieldType: "picklist", Indexed: true}, // physical `industry` col for groupBy
	}); err != nil {
		t.Fatalf("CreateTable accounts: %v", err)
	}
	if err := repo.CreateTable(ctx, "it_rj_contacts", []ddl.FieldDef{
		{Name: "account", FieldType: "relation", RelatedEntity: "it_rj_accounts"}, // → account_id
	}); err != nil {
		t.Fatalf("CreateTable contacts: %v", err)
	}
	cleanup := func() {
		_ = repo.DropTable(ctx, "it_rj_contacts")
		_ = repo.DropTable(ctx, "it_rj_accounts")
	}
	return repo, ctx, cleanup
}

const (
	rjTenantA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	rjTenantB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	rjUser1   = "11111111-1111-1111-1111-111111111111"
	rjUser2   = "22222222-2222-2222-2222-222222222222"
)

func groupsByKey(gs []AggregateGroup) map[string]int64 {
	m := map[string]int64{}
	for _, g := range gs {
		k := ""
		if g.Key != nil {
			k = *g.Key
		}
		m[k] = g.Count
	}
	return m
}

// TestReportJoin_CrossTenantIsolation is THE bug-class gate. Tenant B has a contact whose
// account_id COLLIDES with a tenant-A account id. A joined aggregate for tenant B (count contacts
// grouped by account.industry) must count that contact's account as ABSENT (its account belongs
// to tenant A, filtered out by the per-hop `j0.tenant_id = ?`) — never surface tenant A's
// industry value. If the per-hop tenant filter were missing, B's report would leak "tech".
func TestReportJoin_CrossTenantIsolation(t *testing.T) {
	repo, ctx, cleanup := seedJoinFixture(t)
	defer cleanup()

	// Tenant A: one account (industry=tech) + a contact pointing at it.
	accA, err := repo.InsertRecord(ctx, "it_rj_accounts", rjTenantA, rjUser1, map[string]any{"industry": "tech"}, nil)
	if err != nil {
		t.Fatalf("insert accA: %v", err)
	}
	if _, err := repo.InsertRecord(ctx, "it_rj_contacts", rjTenantA, rjUser1, map[string]any{"account": accA}, nil); err != nil {
		t.Fatalf("insert contactA: %v", err)
	}

	// Tenant B: one account (industry=secret) + a contact pointing at it, PLUS a contact pointing
	// at tenant A's account id (the cross-tenant collision).
	accB, err := repo.InsertRecord(ctx, "it_rj_accounts", rjTenantB, rjUser1, map[string]any{"industry": "secret"}, nil)
	if err != nil {
		t.Fatalf("insert accB: %v", err)
	}
	if _, err := repo.InsertRecord(ctx, "it_rj_contacts", rjTenantB, rjUser1, map[string]any{"account": accB}, nil); err != nil {
		t.Fatalf("insert contactB1: %v", err)
	}
	if _, err := repo.InsertRecord(ctx, "it_rj_contacts", rjTenantB, rjUser1, map[string]any{"account": accA}, nil); err != nil {
		t.Fatalf("insert contactB2 (collision): %v", err)
	}

	relTable, _ := TableFor("it_rj_accounts")
	// INNER join: B's contact→accA is dropped (accA is tenant A's, invisible to B), leaving only
	// B's own account (secret). A leak would add a "tech" group.
	groupsB, err := repo.AggregateGrouped(ctx, "it_rj_contacts", rjTenantB, AdminScope(), AggregateParams{
		Metric: MetricCount, GroupByPhysical: "industry", GroupByAlias: "j0",
		Joins: []JoinSpec{{
			Alias: "j0", RelatedTable: relTable, Scope: AdminScope(),
			LeftCol: "account_id", RightCol: "id",
		}},
	})
	if err != nil {
		t.Fatalf("AggregateGrouped tenantB: %v", err)
	}
	gb := groupsByKey(groupsB)
	if _, leaked := gb["tech"]; leaked {
		t.Fatalf("CROSS-TENANT LEAK: tenant B report surfaced tenant A's industry 'tech': %v", gb)
	}
	if gb["secret"] != 1 {
		t.Fatalf("tenant B: want secret=1, got %v", gb)
	}
	if len(gb) != 1 {
		t.Fatalf("tenant B: want exactly 1 group {secret}, got %v", gb)
	}

	// Tenant A sees only its own: {tech:1}.
	groupsA, err := repo.AggregateGrouped(ctx, "it_rj_contacts", rjTenantA, AdminScope(), AggregateParams{
		Metric: MetricCount, GroupByPhysical: "industry", GroupByAlias: "j0",
		Joins: []JoinSpec{{
			Alias: "j0", RelatedTable: relTable, Scope: AdminScope(),
			LeftCol: "account_id", RightCol: "id",
		}},
	})
	if err != nil {
		t.Fatalf("AggregateGrouped tenantA: %v", err)
	}
	if ga := groupsByKey(groupsA); ga["tech"] != 1 || len(ga) != 1 {
		t.Fatalf("tenant A: want {tech:1}, got %v", ga)
	}
}

// TestReportJoin_RowLevelScopeOnJoinedEntity proves the joined hop honours A5 row-level access on
// the RELATED entity: an owner-scoped caller counting contacts grouped by account.industry only
// sees contacts whose ACCOUNT they own (created). Accounts owned by another user are filtered by
// the hop's owner predicate in the JOIN ON clause.
func TestReportJoin_RowLevelScopeOnJoinedEntity(t *testing.T) {
	repo, ctx, cleanup := seedJoinFixture(t)
	defer cleanup()

	// Two accounts in one tenant, owned by different users; a contact under each.
	accU1, _ := repo.InsertRecord(ctx, "it_rj_accounts", rjTenantA, rjUser1, map[string]any{"industry": "u1ind"}, nil)
	accU2, _ := repo.InsertRecord(ctx, "it_rj_accounts", rjTenantA, rjUser2, map[string]any{"industry": "u2ind"}, nil)
	// Both contacts created by rjUser1 (so base owner-scope sees both); the FILTER is the hop.
	if _, err := repo.InsertRecord(ctx, "it_rj_contacts", rjTenantA, rjUser1, map[string]any{"account": accU1}, nil); err != nil {
		t.Fatalf("insert c1: %v", err)
	}
	if _, err := repo.InsertRecord(ctx, "it_rj_contacts", rjTenantA, rjUser1, map[string]any{"account": accU2}, nil); err != nil {
		t.Fatalf("insert c2: %v", err)
	}

	relTable, _ := TableFor("it_rj_accounts")
	// Base admin (sees both contacts), but the HOP is owner-scoped to rjUser1 → only the account
	// rjUser1 owns (accU1, industry=u1ind) passes; the contact under accU2 is dropped (INNER).
	groups, err := repo.AggregateGrouped(ctx, "it_rj_contacts", rjTenantA, AdminScope(), AggregateParams{
		Metric: MetricCount, GroupByPhysical: "industry", GroupByAlias: "j0",
		Joins: []JoinSpec{{
			Alias: "j0", RelatedTable: relTable,
			Scope:   RowScope{Level: AccessOwner, UserID: rjUser1},
			LeftCol: "account_id", RightCol: "id",
		}},
	})
	if err != nil {
		t.Fatalf("AggregateGrouped owner-hop: %v", err)
	}
	g := groupsByKey(groups)
	if g["u1ind"] != 1 {
		t.Fatalf("owner-hop: want u1ind=1, got %v", g)
	}
	if _, leaked := g["u2ind"]; leaked {
		t.Fatalf("ROW-LEVEL LEAK: owner-scoped hop surfaced another owner's account: %v", g)
	}
}

// TestReportJoin_DirectionAndLeftInner covers both hop directions and LEFT vs INNER semantics:
// base=accounts (parent) fanning out to contacts (child). A parent with no child contributes to a
// LEFT join (NULL/0 metric bucket via the account's own base groupBy) but not to an INNER join.
func TestReportJoin_DirectionAndLeftInner(t *testing.T) {
	repo, ctx, cleanup := seedJoinFixture(t)
	defer cleanup()

	accHot, _ := repo.InsertRecord(ctx, "it_rj_accounts", rjTenantA, rjUser1, map[string]any{"industry": "hot"}, nil)
	accCold, _ := repo.InsertRecord(ctx, "it_rj_accounts", rjTenantA, rjUser1, map[string]any{"industry": "cold"}, nil)
	// Two contacts under accHot, none under accCold.
	if _, err := repo.InsertRecord(ctx, "it_rj_contacts", rjTenantA, rjUser1, map[string]any{"account": accHot}, nil); err != nil {
		t.Fatalf("c1: %v", err)
	}
	if _, err := repo.InsertRecord(ctx, "it_rj_contacts", rjTenantA, rjUser1, map[string]any{"account": accHot}, nil); err != nil {
		t.Fatalf("c2: %v", err)
	}
	_ = accCold

	childTable, _ := TableFor("it_rj_contacts")
	// Base = accounts, groupBy the account's OWN industry (base field, alias base), COUNT the
	// joined contacts. INNER: only accHot has contacts → {hot:2}. accCold absent.
	inner, err := repo.AggregateGrouped(ctx, "it_rj_accounts", rjTenantA, AdminScope(), AggregateParams{
		Metric: MetricCount, GroupByPhysical: "industry", // base field → GroupByAlias "" defaults to base
		Joins: []JoinSpec{{
			Alias: "j0", RelatedTable: childTable, Scope: AdminScope(),
			LeftCol: "id", RightCol: "account_id", // base is PARENT: j0.account_id = base.id
		}},
	})
	if err != nil {
		t.Fatalf("AggregateGrouped inner: %v", err)
	}
	gi := groupsByKey(inner)
	if gi["hot"] != 2 {
		t.Fatalf("inner: want hot=2, got %v", gi)
	}
	if _, present := gi["cold"]; present {
		t.Fatalf("inner: childless account 'cold' must be absent, got %v", gi)
	}

	// LEFT: accCold now appears (count 1 — the base row with a NULL contact side still counts as a
	// row under COUNT(*); the point is the group KEY 'cold' is present, proving the LEFT join kept
	// the childless parent).
	left, err := repo.AggregateGrouped(ctx, "it_rj_accounts", rjTenantA, AdminScope(), AggregateParams{
		Metric: MetricCount, GroupByPhysical: "industry",
		Joins: []JoinSpec{{
			Alias: "j0", RelatedTable: childTable, Scope: AdminScope(), Optional: true,
			LeftCol: "id", RightCol: "account_id",
		}},
	})
	if err != nil {
		t.Fatalf("AggregateGrouped left: %v", err)
	}
	gl := groupsByKey(left)
	if _, present := gl["cold"]; !present {
		t.Fatalf("LEFT: childless account 'cold' must appear, got %v", gl)
	}
	if gl["hot"] != 2 {
		t.Fatalf("LEFT: want hot=2, got %v", gl)
	}
}
