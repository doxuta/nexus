//go:build integration

package dynrepo

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// TestRowLevelSecurity_TeamHierarchy is the MySQL proof of queue #20 (team-hierarchy row scope):
// an access_level=team principal who is a member ONLY of an ancestor team sees — and can edit —
// rows whose team_set holds a descendant team, while a leaf member never inherits upward or
// sideways. It also pins the two shapes that must not hang or hide data: a parent cycle written
// straight into `teams` (pre-guard data, parent_id has no hard FK) and a deep chain.
//
// Tree (tenant A):
//
//	region ─┬─ branch ─── office        uRegion ∈ {region}   uOffice ∈ {office}
//	        └─ branch2 ── office2       uBranch2 ∈ {branch2} uOffice2 ∈ {office2}
//
// Records: recOffice (by uOffice → team_set {office}), recOffice2 (by uOffice2), recRegion (by uRegion).
func TestRowLevelSecurity_TeamHierarchy(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_a5_tree"

	tenant := newID()
	seedTenant(t, db, tenant, "a5-tree-"+tenant[:8])

	region, branch, office, branch2, office2 := newID(), newID(), newID(), newID(), newID()
	seedTeamUnder(t, db, region, tenant, "Vùng Bắc", "")
	seedTeamUnder(t, db, branch, tenant, "Chi nhánh Hà Nội", region)
	seedTeamUnder(t, db, office, tenant, "PGD Cầu Giấy", branch)
	seedTeamUnder(t, db, branch2, tenant, "Chi nhánh Hải Phòng", region)
	seedTeamUnder(t, db, office2, tenant, "PGD Lê Chân", branch2)

	uRegion, uOffice, uBranch2, uOffice2 := newID(), newID(), newID(), newID()
	for _, u := range []string{uRegion, uOffice, uBranch2, uOffice2} {
		seedUser(t, db, u, tenant)
	}
	seedMembership(t, db, uRegion, region)
	seedMembership(t, db, uOffice, office)
	seedMembership(t, db, uBranch2, branch2)
	seedMembership(t, db, uOffice2, office2)

	teams := []string{region, branch, office, branch2, office2}
	users := []string{uRegion, uOffice, uBranch2, uOffice2}
	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		cleanupA5(db, tenant, "", users, teams)
	})

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	mustInsert := func(user, name string) string {
		t.Helper()
		id, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": name}, nil)
		if err != nil {
			t.Fatalf("InsertRecord %s: %v", name, err)
		}
		return id
	}
	recOffice := mustInsert(uOffice, "office")
	recOffice2 := mustInsert(uOffice2, "office2")
	recRegion := mustInsert(uRegion, "region")

	teamScope := func(uid string) RowScope { return RowScope{UserID: uid, Level: AccessTeam} }
	listIDs := func(scope RowScope) map[string]bool {
		t.Helper()
		recs, total, err := repo.ListRecords(ctx, entity, tenant, scope, ListParams{Page: 1, Limit: 50})
		if err != nil {
			t.Fatalf("ListRecords: %v", err)
		}
		got := make(map[string]bool, len(recs))
		for _, r := range recs {
			got[r.ID] = true
		}
		if len(got) != total {
			t.Fatalf("total %d != rows %d", total, len(got))
		}
		return got
	}

	t.Run("region member sees every descendant branch's rows (READ)", func(t *testing.T) {
		got := listIDs(teamScope(uRegion))
		for _, want := range []string{recOffice, recOffice2, recRegion} {
			if !got[want] {
				t.Errorf("region scope missing %s (got %v)", want, got)
			}
		}
		if _, err := repo.GetRecord(ctx, entity, tenant, recOffice, teamScope(uRegion)); err != nil {
			t.Errorf("GetRecord region→office: %v", err)
		}
	})

	t.Run("leaf member does not inherit upward or sideways", func(t *testing.T) {
		got := listIDs(teamScope(uOffice))
		if !got[recOffice] || got[recOffice2] || got[recRegion] {
			t.Errorf("office scope = %v; want only recOffice", got)
		}
		if _, err := repo.GetRecord(ctx, entity, tenant, recRegion, teamScope(uOffice)); !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("GetRecord office→region err = %v; want ErrRecordNotFound", err)
		}
		got2 := listIDs(teamScope(uBranch2))
		if !got2[recOffice2] || got2[recOffice] || got2[recRegion] {
			t.Errorf("branch2 scope = %v; want only recOffice2", got2)
		}
	})

	t.Run("region member can edit a descendant's row and the creator still sees it (WRITE)", func(t *testing.T) {
		if err := repo.UpdateRecord(ctx, entity, tenant, recOffice, uRegion, teamScope(uRegion),
			map[string]any{"name": "office-edited-by-region"}, nil, nil); err != nil {
			t.Fatalf("UpdateRecord region→office: %v", err)
		}
		rec, err := repo.GetRecord(ctx, entity, tenant, recOffice, teamScope(uOffice))
		if err != nil {
			t.Fatalf("creator lost its own row after the ancestor's edit: %v", err)
		}
		if rec.Data["name"] != "office-edited-by-region" {
			t.Errorf("edit not persisted: %v", rec.Data["name"])
		}
		// The leaf cannot edit the ancestor's row (same filter on the write leg).
		if err := repo.UpdateRecord(ctx, entity, tenant, recRegion, uOffice, teamScope(uOffice),
			map[string]any{"name": "hacked"}, nil, nil); !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("office editing region row err = %v; want ErrRecordNotFound", err)
		}
	})

	t.Run("aggregate (aliased path) counts the same rows as the list", func(t *testing.T) {
		agg, err := repo.AggregateScalar(ctx, entity, tenant, teamScope(uRegion), AggregateParams{Metric: MetricCount})
		if err != nil {
			t.Fatalf("AggregateScalar: %v", err)
		}
		if agg.Count != 3 {
			t.Errorf("region count = %d; want 3", agg.Count)
		}
		agg, err = repo.AggregateScalar(ctx, entity, tenant, teamScope(uOffice), AggregateParams{Metric: MetricCount})
		if err != nil {
			t.Fatalf("AggregateScalar: %v", err)
		}
		if agg.Count != 1 {
			t.Errorf("office count = %d; want 1", agg.Count)
		}
	})

	t.Run("cycle written straight into teams terminates and still resolves", func(t *testing.T) {
		cx, cy := newID(), newID()
		seedTeamUnder(t, db, cx, tenant, "cycle-x", cy) // x.parent = y
		seedTeamUnder(t, db, cy, tenant, "cycle-y", cx) // y.parent = x  → x→y→x
		uCx, uCy := newID(), newID()
		seedUser(t, db, uCx, tenant)
		seedUser(t, db, uCy, tenant)
		seedMembership(t, db, uCx, cx)
		seedMembership(t, db, uCy, cy)
		t.Cleanup(func() { cleanupA5(db, "", "", []string{uCx, uCy}, []string{cx, cy}) })
		recCy := mustInsert(uCy, "cycle-record")

		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		recs, _, err := repo.ListRecords(cctx, entity, tenant, teamScope(uCx), ListParams{Page: 1, Limit: 50})
		if err != nil {
			t.Fatalf("ListRecords under a data cycle: %v", err)
		}
		found := false
		for _, r := range recs {
			found = found || r.ID == recCy
		}
		if !found {
			t.Errorf("x member did not see y's row although y is below x in the (cyclic) tree")
		}
	})

	t.Run("deep chain: the top member reaches the leaf's row", func(t *testing.T) {
		const depth = 40
		ids := make([]string, depth)
		for i := range ids {
			ids[i] = newID()
			parent := ""
			if i > 0 {
				parent = ids[i-1]
			}
			seedTeamUnder(t, db, ids[i], tenant, fmt.Sprintf("deep-%02d", i), parent)
		}
		uTop, uLeaf := newID(), newID()
		seedUser(t, db, uTop, tenant)
		seedUser(t, db, uLeaf, tenant)
		seedMembership(t, db, uTop, ids[0])
		seedMembership(t, db, uLeaf, ids[depth-1])
		t.Cleanup(func() { cleanupA5(db, "", "", []string{uTop, uLeaf}, ids) })
		recLeaf := mustInsert(uLeaf, "leaf")

		if _, err := repo.GetRecord(ctx, entity, tenant, recLeaf, teamScope(uTop)); err != nil {
			t.Errorf("top of a %d-deep chain cannot see the leaf row: %v", depth, err)
		}
		if _, err := repo.GetRecord(ctx, entity, tenant, recLeaf, teamScope(uRegion)); !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("unrelated tree leaked the leaf row: %v", err)
		}
	})
}

// seedTeamUnder inserts a teams row with an optional parent (""=root). parent_id has no hard FK,
// so a parent that does not exist yet — or a cycle — can be written on purpose.
func seedTeamUnder(t *testing.T, db *sqlx.DB, id, tenantID, name, parentID string) {
	t.Helper()
	var parent *string
	if parentID != "" {
		parent = &parentID
	}
	if _, err := db.Exec(
		`INSERT INTO teams (id, tenant_id, name, parent_id) VALUES (?, ?, ?, ?)`, id, tenantID, name, parent); err != nil {
		t.Fatalf("seedTeamUnder(%s): %v", name, err)
	}
}
