//go:build integration

package dynrepo

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// TestRowLevelSecurity_A5 is the end-to-end proof of A5 team-based row-level security against a
// real MySQL 8: it seeds two tenants, three users (two on overlapping teams, one with no team)
// and team_sets via InsertRecord's resolve-or-create, then asserts every access level + the
// cross-tenant / cross-team / no-team / multi-team edge cases from the coverage map (§B2/§B5).
//
// Layout (tenant A):
//   - team Alpha, team Beta.
//   - userOwner  ∈ {Alpha}
//   - userBeta   ∈ {Beta}
//   - userMulti  ∈ {Alpha, Beta}
//   - userNoTeam ∈ {}
//
// Records (all created in tenant A):
//   - recOwner  created by userOwner  → team_set {Alpha}
//   - recBeta   created by userBeta   → team_set {Beta}
//   - recNoTeam created by userNoTeam → team_set NULL
//
// Tenant B gets its own user + record to prove cross-tenant isolation.
func TestRowLevelSecurity_A5(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_a5_rls"

	tenantA := newID()
	tenantB := newID()
	userOwner := newID()
	userBeta := newID()
	userMulti := newID()
	userNoTeam := newID()
	userTenantB := newID()
	teamAlpha := newID()
	teamBeta := newID()

	seedTenant(t, db, tenantA, "a5-rls-a")
	seedTenant(t, db, tenantB, "a5-rls-b")
	seedUser(t, db, userOwner, tenantA)
	seedUser(t, db, userBeta, tenantA)
	seedUser(t, db, userMulti, tenantA)
	seedUser(t, db, userNoTeam, tenantA)
	seedUser(t, db, userTenantB, tenantB)
	seedTeam(t, db, teamAlpha, tenantA, "Alpha")
	seedTeam(t, db, teamBeta, tenantA, "Beta")
	seedMembership(t, db, userOwner, teamAlpha)
	seedMembership(t, db, userBeta, teamBeta)
	seedMembership(t, db, userMulti, teamAlpha)
	seedMembership(t, db, userMulti, teamBeta)

	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		cleanupA5(db, tenantA, tenantB,
			[]string{userOwner, userBeta, userMulti, userNoTeam, userTenantB},
			[]string{teamAlpha, teamBeta})
	})

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	// Inserts derive team_set_id from the creator's memberships.
	recOwner, err := repo.InsertRecord(ctx, entity, tenantA, userOwner, map[string]any{"name": "owner"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord owner: %v", err)
	}
	recBeta, err := repo.InsertRecord(ctx, entity, tenantA, userBeta, map[string]any{"name": "beta"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord beta: %v", err)
	}
	recNoTeam, err := repo.InsertRecord(ctx, entity, tenantA, userNoTeam, map[string]any{"name": "noteam"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord noteam: %v", err)
	}
	recTenantB, err := repo.InsertRecord(ctx, entity, tenantB, userTenantB, map[string]any{"name": "tenantB"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord tenantB: %v", err)
	}

	// team_set_id assignment: owner/beta got a non-null set; noteam stayed NULL.
	assertTeamSetNotNull(t, db, entity, recOwner)
	assertTeamSetNotNull(t, db, entity, recBeta)
	assertTeamSetNull(t, db, entity, recNoTeam)

	listIDs := func(scope RowScope, tenant string) map[string]bool {
		recs, total, err := repo.ListRecords(ctx, entity, tenant, scope, ListParams{Page: 1, Limit: 50})
		if err != nil {
			t.Fatalf("ListRecords: %v", err)
		}
		got := make(map[string]bool, len(recs))
		for _, r := range recs {
			got[r.ID] = true
		}
		if len(got) != total {
			t.Fatalf("total %d != distinct rows %d (count/page mismatch)", total, len(got))
		}
		return got
	}

	// --- access_level = all: tenant A sees every tenant-A row, never tenant B's. ----------------
	t.Run("all sees all tenant rows, not other tenant", func(t *testing.T) {
		got := listIDs(RowScope{UserID: userOwner, Level: AccessAll}, tenantA)
		for _, id := range []string{recOwner, recBeta, recNoTeam} {
			if !got[id] {
				t.Errorf("all: missing %s", id)
			}
		}
		if got[recTenantB] {
			t.Errorf("all: leaked tenant B row %s", recTenantB)
		}
	})

	// --- access_level = owner: a user sees only rows they created. ------------------------------
	t.Run("owner sees only own rows", func(t *testing.T) {
		got := listIDs(RowScope{UserID: userOwner, Level: AccessOwner}, tenantA)
		if !got[recOwner] || got[recBeta] || got[recNoTeam] {
			t.Errorf("owner scope got %v; want only recOwner", got)
		}
	})

	// --- access_level = team: own rows + rows whose team_set shares a team with the user. -------
	t.Run("team: single-team user sees own + same-team", func(t *testing.T) {
		// userOwner ∈ Alpha. recOwner is Alpha. recBeta is Beta (not shared). recNoTeam NULL.
		got := listIDs(RowScope{UserID: userOwner, Level: AccessTeam}, tenantA)
		if !got[recOwner] {
			t.Errorf("team(owner): missing own record")
		}
		if got[recBeta] {
			t.Errorf("team(owner): leaked Beta record %s across teams", recBeta)
		}
		if got[recNoTeam] {
			t.Errorf("team(owner): leaked no-team record %s", recNoTeam)
		}
	})

	t.Run("team: multi-team user sees both teams' rows", func(t *testing.T) {
		// userMulti ∈ {Alpha, Beta}: sees recOwner (Alpha set) AND recBeta (Beta set).
		got := listIDs(RowScope{UserID: userMulti, Level: AccessTeam}, tenantA)
		if !got[recOwner] || !got[recBeta] {
			t.Errorf("team(multi) got %v; want recOwner AND recBeta", got)
		}
		if got[recNoTeam] {
			t.Errorf("team(multi): leaked no-team record %s", recNoTeam)
		}
	})

	t.Run("team: no-team user sees only own rows", func(t *testing.T) {
		// userNoTeam has no membership → team branch matches nothing; sees only its own record.
		got := listIDs(RowScope{UserID: userNoTeam, Level: AccessTeam}, tenantA)
		if !got[recNoTeam] {
			t.Errorf("team(noteam): missing own record")
		}
		if got[recOwner] || got[recBeta] {
			t.Errorf("team(noteam) got %v; want only recNoTeam", got)
		}
	})

	// --- access_level = none: sees nothing. -----------------------------------------------------
	t.Run("none sees nothing", func(t *testing.T) {
		got := listIDs(RowScope{UserID: userOwner, Level: AccessNone}, tenantA)
		if len(got) != 0 {
			t.Errorf("none scope got %v; want empty", got)
		}
	})

	// --- GetRecord enforces the same scope: a cross-team Get is not-found. ----------------------
	t.Run("Get under team scope hides cross-team record", func(t *testing.T) {
		if _, err := repo.GetRecord(ctx, entity, tenantA, recBeta, RowScope{UserID: userOwner, Level: AccessTeam}); !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("Get cross-team err = %v; want ErrRecordNotFound", err)
		}
		// But the multi-team user CAN Get it.
		if _, err := repo.GetRecord(ctx, entity, tenantA, recBeta, RowScope{UserID: userMulti, Level: AccessTeam}); err != nil {
			t.Errorf("Get same-team (multi) err = %v; want success", err)
		}
	})

	// --- Update/Delete are scoped too: a user cannot mutate a row outside their visibility. -----
	t.Run("Update under owner scope cannot touch another user's row", func(t *testing.T) {
		err := repo.UpdateRecord(ctx, entity, tenantA, recBeta, userOwner,
			RowScope{UserID: userOwner, Level: AccessOwner}, map[string]any{"name": "hacked"}, nil, nil)
		if !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("Update out-of-scope err = %v; want ErrRecordNotFound", err)
		}
	})

	t.Run("Delete under team scope cannot touch cross-team row", func(t *testing.T) {
		err := repo.SoftDeleteRecord(ctx, entity, tenantA, recBeta, RowScope{UserID: userOwner, Level: AccessTeam})
		if !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("Delete cross-team err = %v; want ErrRecordNotFound", err)
		}
	})

	// --- Cross-tenant isolation under team scope: tenant A user never sees tenant B rows. -------
	t.Run("cross-tenant isolation holds under team scope", func(t *testing.T) {
		got := listIDs(RowScope{UserID: userOwner, Level: AccessTeam}, tenantB)
		if len(got) != 0 {
			t.Errorf("tenant A user listing under tenant B got %v; want empty", got)
		}
	})

	// --- resolve-or-create dedupe: a second record by a same-team-set user reuses the set. ------
	t.Run("team_set dedupe by team_md5", func(t *testing.T) {
		// userMulti and a fresh user on the SAME pair {Alpha,Beta} must share one team_sets row.
		userMulti2 := newID()
		seedUser(t, db, userMulti2, tenantA)
		seedMembership(t, db, userMulti2, teamAlpha)
		seedMembership(t, db, userMulti2, teamBeta)
		defer cleanupA5(db, "", "", []string{userMulti2}, nil)

		r1, err := repo.InsertRecord(ctx, entity, tenantA, userMulti, map[string]any{"name": "m1"}, nil)
		if err != nil {
			t.Fatalf("insert m1: %v", err)
		}
		r2, err := repo.InsertRecord(ctx, entity, tenantA, userMulti2, map[string]any{"name": "m2"}, nil)
		if err != nil {
			t.Fatalf("insert m2: %v", err)
		}
		s1 := teamSetOf(t, db, entity, r1)
		s2 := teamSetOf(t, db, entity, r2)
		if s1 == "" || s1 != s2 {
			t.Errorf("identical team sets did not dedupe: %q vs %q", s1, s2)
		}
	})
}

// --- seed / assertion helpers ------------------------------------------------------------------

func newID() string { return uuid.NewString() }

func seedTenant(t *testing.T, db *sqlx.DB, id, slug string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO tenants (id, slug, name, status, created_at, updated_at) VALUES (?, ?, ?, 'active', NOW(3), NOW(3))`,
		id, slug, slug); err != nil {
		t.Fatalf("seedTenant: %v", err)
	}
}

func seedUser(t *testing.T, db *sqlx.DB, id, tenantID string) {
	t.Helper()
	// password_hash is NOT NULL with no default in the REAL migration (000003) — a raw seed
	// INSERT must supply it (the auth service normally writes a bcrypt digest here). This RLS
	// suite never logs in, so any non-null placeholder is fine; the value is never verified.
	if _, err := db.Exec(
		`INSERT INTO users (id, tenant_id, email, password_hash, status, is_admin, created_at, updated_at) VALUES (?, ?, ?, ?, 'active', 0, NOW(3), NOW(3))`,
		id, tenantID, id+"@example.test", "seed-no-login"); err != nil {
		t.Fatalf("seedUser: %v", err)
	}
}

func seedTeam(t *testing.T, db *sqlx.DB, id, tenantID, name string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO teams (id, tenant_id, name) VALUES (?, ?, ?)`, id, tenantID, name); err != nil {
		t.Fatalf("seedTeam: %v", err)
	}
}

func seedMembership(t *testing.T, db *sqlx.DB, userID, teamID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO team_memberships (id, user_id, team_id) VALUES (?, ?, ?)`, newID(), userID, teamID); err != nil {
		t.Fatalf("seedMembership: %v", err)
	}
}

func teamSetOf(t *testing.T, db *sqlx.DB, entity, recID string) string {
	t.Helper()
	var ts *string
	if err := db.Get(&ts, "SELECT team_set_id FROM `obj_"+entity+"` WHERE id = ?", recID); err != nil {
		t.Fatalf("teamSetOf: %v", err)
	}
	if ts == nil {
		return ""
	}
	return *ts
}

func assertTeamSetNotNull(t *testing.T, db *sqlx.DB, entity, recID string) {
	t.Helper()
	if teamSetOf(t, db, entity, recID) == "" {
		t.Errorf("record %s: team_set_id is NULL; want a resolved set", recID)
	}
}

func assertTeamSetNull(t *testing.T, db *sqlx.DB, entity, recID string) {
	t.Helper()
	if got := teamSetOf(t, db, entity, recID); got != "" {
		t.Errorf("record %s: team_set_id = %q; want NULL (creator on no team)", recID, got)
	}
}

// cleanupA5 removes seeded rows in FK-safe order. Empty tenant ids are skipped so partial
// cleanups (e.g. a single extra user) work too.
func cleanupA5(db *sqlx.DB, tenantA, tenantB string, users, teams []string) {
	for _, u := range users {
		db.Exec("DELETE FROM team_memberships WHERE user_id = ?", u)
	}
	for _, tm := range teams {
		db.Exec("DELETE FROM team_sets_teams WHERE team_id = ?", tm)
	}
	for _, tn := range []string{tenantA, tenantB} {
		if tn == "" {
			continue
		}
		db.Exec("DELETE FROM team_sets WHERE tenant_id = ?", tn)
	}
	for _, tm := range teams {
		db.Exec("DELETE FROM teams WHERE id = ?", tm)
	}
	for _, u := range users {
		db.Exec("DELETE FROM users WHERE id = ?", u)
	}
	for _, tn := range []string{tenantA, tenantB} {
		if tn == "" {
			continue
		}
		db.Exec("DELETE FROM tenants WHERE id = ?", tn)
	}
}
