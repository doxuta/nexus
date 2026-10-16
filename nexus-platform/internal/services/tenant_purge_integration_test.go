//go:build integration

package services

// Tenant purge against a REAL, freshly-migrated MySQL — the only place its correctness can be
// shown, because the whole design rests on two facts about the live schema that no fake reproduces:
//
//  1. three tables hold tenant data with NO tenant_id column of their own (acl_role_actions,
//     team_memberships, team_sets_teams). A "DELETE WHERE tenant_id = ?" sweep cannot reach them;
//     they survive only if the FK cascade does the work. This test seeds all three and asserts
//     they are gone — the assertion that a unit test with a mock DB would fake into passing.
//
//  2. every non-cascading FK points at `tenants`, which is why the tenants row is deleted last.
//     If that order were wrong, MySQL itself would refuse and the transaction would roll back.
//
// It also pins the guards that make the endpoint safe to expose (self, not-retired, wrong slug),
// and — most important — that a dry run leaves the data EXACTLY where it was.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"nexus/platform/internal/auth"
)

// seedPurgeTenant creates one tenant with rows in a spread of tenant-scoped tables PLUS the three
// cascade-only children, and returns its id.
func seedPurgeTenant(t *testing.T, db *gorm.DB, slug string) string {
	t.Helper()
	tenantID := "tn-" + slug
	roleID := "rl-" + slug
	userID := "us-" + slug
	teamID := "tm-" + slug
	setID := "ts-" + slug

	exec := func(q string, args ...any) {
		t.Helper()
		if err := db.Exec(q, args...).Error; err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
	exec("INSERT INTO tenants (id, slug, name, status, created_at, updated_at) VALUES (?,?,?,'active',NOW(),NOW())",
		tenantID, slug, "Khách "+slug)
	exec("INSERT INTO acl_roles (id, tenant_id, name, label, created_at, updated_at) VALUES (?,?,?,?,NOW(),NOW())",
		roleID, tenantID, "sales", "Sales")
	exec("INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin, role_id, created_at, updated_at) "+
		"VALUES (?,?,?,?,?,'active',0,?,NOW(),NOW())",
		userID, tenantID, "u@"+slug+".vn", "x", "U "+slug, roleID)
	exec("INSERT INTO teams (id, tenant_id, name, created_at, updated_at) VALUES (?,?,?,NOW(),NOW())",
		teamID, tenantID, "Team "+slug)
	// team_md5 is the de-dup hash of the set's team composition — NOT NULL with no default.
	exec("INSERT INTO team_sets (id, tenant_id, team_md5, created_at, updated_at) VALUES (?,?,MD5(?),NOW(),NOW())",
		setID, tenantID, teamID)

	// The three cascade-only children — no tenant_id anywhere on these rows.
	exec("INSERT INTO acl_role_actions (id, role_id, entity_name, action, access_level) VALUES (?,?,?,?,?)",
		"ara-"+slug, roleID, "crm_accounts", "view", "owner")
	exec("INSERT INTO team_memberships (id, team_id, user_id, created_at) VALUES (?,?,?,NOW())",
		"tmb-"+slug, teamID, userID)
	exec("INSERT INTO team_sets_teams (team_set_id, team_id) VALUES (?,?)", setID, teamID)
	return tenantID
}

func countRows(t *testing.T, db *gorm.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(q, args...).Scan(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestPurgeTenant_RemovesEverythingIncludingCascadeOnlyChildren(t *testing.T) {
	db, _ := o3aSetup(t)
	svc := NewTenantProvisionService(db)
	ctx := context.Background()

	victim := seedPurgeTenant(t, db, "victim")
	bystander := seedPurgeTenant(t, db, "bystander")

	// Purge only works on a retired tenant — that two-step lifecycle is the point.
	if err := svc.RetireTenant(ctx, "operator-tenant", victim); err != nil {
		t.Fatalf("retire: %v", err)
	}

	t.Run("dry run counts and changes NOTHING", func(t *testing.T) {
		rep, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", victim, "victim", true)
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if !rep.DryRun || rep.TotalRows == 0 {
			t.Fatalf("dry run report looks empty: %+v", rep)
		}
		if n := countRows(t, db, "SELECT COUNT(*) FROM acl_roles WHERE tenant_id = ?", victim); n != 1 {
			t.Errorf("dry run deleted rows: acl_roles = %d, want 1", n)
		}
		if n := countRows(t, db, "SELECT COUNT(*) FROM acl_role_actions WHERE role_id = ?", "rl-victim"); n != 1 {
			t.Errorf("dry run touched a cascade child: acl_role_actions = %d, want 1", n)
		}
	})

	t.Run("real purge empties every tenant-scoped table AND the cascade-only children", func(t *testing.T) {
		rep, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", victim, "victim", false)
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if rep.DryRun || rep.TotalRows == 0 {
			t.Fatalf("purge report looks wrong: %+v", rep)
		}

		for _, c := range []struct {
			what string
			q    string
			args []any
		}{
			{"acl_roles", "SELECT COUNT(*) FROM acl_roles WHERE tenant_id = ?", []any{victim}},
			{"users", "SELECT COUNT(*) FROM users WHERE tenant_id = ?", []any{victim}},
			{"teams", "SELECT COUNT(*) FROM teams WHERE tenant_id = ?", []any{victim}},
			{"team_sets", "SELECT COUNT(*) FROM team_sets WHERE tenant_id = ?", []any{victim}},
			// The three that carry NO tenant_id — these are the ones a naive sweep leaves behind.
			{"acl_role_actions (cascade-only)", "SELECT COUNT(*) FROM acl_role_actions WHERE role_id = ?", []any{"rl-victim"}},
			{"team_memberships (cascade-only)", "SELECT COUNT(*) FROM team_memberships WHERE team_id = ?", []any{"tm-victim"}},
			{"team_sets_teams (cascade-only)", "SELECT COUNT(*) FROM team_sets_teams WHERE team_set_id = ?", []any{"ts-victim"}},
			// The tenants row itself goes hard, not soft: a purged tenant must not linger.
			{"tenants row", "SELECT COUNT(*) FROM tenants WHERE id = ?", []any{victim}},
		} {
			if n := countRows(t, db, c.q, c.args...); n != 0 {
				t.Errorf("%s still has %d row(s) after purge", c.what, n)
			}
		}
	})

	t.Run("the tenant next door is untouched", func(t *testing.T) {
		for _, c := range []struct {
			what string
			q    string
			args []any
		}{
			{"acl_roles", "SELECT COUNT(*) FROM acl_roles WHERE tenant_id = ?", []any{bystander}},
			{"users", "SELECT COUNT(*) FROM users WHERE tenant_id = ?", []any{bystander}},
			{"acl_role_actions", "SELECT COUNT(*) FROM acl_role_actions WHERE role_id = ?", []any{"rl-bystander"}},
			{"team_memberships", "SELECT COUNT(*) FROM team_memberships WHERE team_id = ?", []any{"tm-bystander"}},
			{"team_sets_teams", "SELECT COUNT(*) FROM team_sets_teams WHERE team_set_id = ?", []any{"ts-bystander"}},
			{"tenants row", "SELECT COUNT(*) FROM tenants WHERE id = ?", []any{bystander}},
		} {
			if n := countRows(t, db, c.q, c.args...); n != 1 {
				t.Errorf("bystander %s = %d row(s), want 1 — the purge crossed a tenant boundary", c.what, n)
			}
		}
	})
}

func TestPurgeTenant_Guards(t *testing.T) {
	db, _ := o3aSetup(t)
	svc := NewTenantProvisionService(db)
	ctx := context.Background()

	live := seedPurgeTenant(t, db, "stillrunning")

	t.Run("a LIVE workspace cannot be purged — retire is step one", func(t *testing.T) {
		_, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", live, "stillrunning", false)
		if codeOf(err) != "TENANT_PURGE_NOT_RETIRED" {
			t.Fatalf("err = %v, want TENANT_PURGE_NOT_RETIRED", err)
		}
		if n := countRows(t, db, "SELECT COUNT(*) FROM users WHERE tenant_id = ?", live); n != 1 {
			t.Errorf("data was deleted despite the refusal (users = %d)", n)
		}
	})

	if err := svc.RetireTenant(ctx, "operator-tenant", live); err != nil {
		t.Fatalf("retire: %v", err)
	}

	t.Run("wrong slug typed → refused, nothing deleted", func(t *testing.T) {
		_, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", live, "stillrunnin", false)
		if codeOf(err) != "TENANT_PURGE_CONFIRM_MISMATCH" {
			t.Fatalf("err = %v, want TENANT_PURGE_CONFIRM_MISMATCH", err)
		}
		if n := countRows(t, db, "SELECT COUNT(*) FROM users WHERE tenant_id = ?", live); n != 1 {
			t.Errorf("data was deleted despite the refusal (users = %d)", n)
		}
	})

	t.Run("cannot purge the workspace you are standing in", func(t *testing.T) {
		_, err := svc.PurgeTenant(ctx, live, "op-1", live, "stillrunning", false)
		if codeOf(err) != "TENANT_SELF_PURGE" {
			t.Fatalf("err = %v, want TENANT_SELF_PURGE", err)
		}
	})

	t.Run("unknown id is not-found, not a silent success", func(t *testing.T) {
		_, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", "no-such-tenant", "whatever", true)
		if codeOf(err) != "TENANT_NOT_FOUND" {
			t.Fatalf("err = %v, want TENANT_NOT_FOUND", err)
		}
	})
}

// TestPurgeSchemaDrift_PassesOnTheShippedSchema: the guard must be SILENT on the schema as
// migrated today (all cascade-only children are ON DELETE CASCADE). A guard that fires on a
// healthy schema would block every purge and get deleted by the next person; this is what keeps
// it honest in both directions.
func TestPurgeSchemaDrift_PassesOnTheShippedSchema(t *testing.T) {
	db, _ := o3aSetup(t)
	svc := NewTenantProvisionService(db)
	if err := svc.purgeSchemaDrift(context.Background()); err != nil {
		t.Fatalf("drift guard fired on the shipped schema: %v", err)
	}
	tables, err := svc.tenantScopedTables(context.Background())
	if err != nil {
		t.Fatalf("tenantScopedTables: %v", err)
	}
	if len(tables) < 20 {
		t.Fatalf("only %d tenant-scoped tables found — the discovery query is wrong", len(tables))
	}
	// tenants itself must NOT be in the sweep list: it is deleted last, by primary key.
	for _, tb := range tables {
		if tb == "tenants" {
			t.Fatal("`tenants` is in the tenant_id sweep list — it has no tenant_id and must go last")
		}
	}
	var _ = auth.Tenant{} // keep the auth import meaningful if the assertions above change
}

// CheckSchemaShape (specs/068 T016) is the purge guard exported for the tenant backup: same
// answer, no second copy of the SQL.
func TestCheckSchemaShape_IsThePurgeGuard(t *testing.T) {
	db, _ := o3aSetup(t)
	svc := NewTenantProvisionService(db)
	if err := svc.CheckSchemaShape(context.Background()); err != nil {
		t.Fatalf("CheckSchemaShape fired on the shipped schema: %v", err)
	}
}

// TestPurgeSchemaDrift_FiresWhenAChildWouldBeOrphaned is the OTHER direction, and it is the one
// that matters: the test above only proves the guard is silent on a healthy schema, which an
// always-silent guard would also pass. A guard that cannot be shown to fire is indistinguishable
// from no guard — and this one stands between a customer being told "your data is deleted" and
// their rows quietly surviving.
//
// It builds exactly the shape the guard hunts: a child table with NO tenant_id, FK'd to a
// tenant-scoped parent, with a delete rule that is NOT cascade. The tenant_id sweep cannot reach
// such a table and the cascade will not clear it, so purging would leave the customer's rows
// behind. The guard must refuse rather than half-delete.
func TestPurgeSchemaDrift_FiresWhenAChildWouldBeOrphaned(t *testing.T) {
	db, _ := o3aSetup(t)
	svc := NewTenantProvisionService(db)
	ctx := context.Background()

	if err := svc.purgeSchemaDrift(ctx); err != nil {
		t.Fatalf("guard already firing before the drift was introduced: %v", err)
	}

	// teams is tenant-scoped; this child is not, and RESTRICT means no cascade will clean it.
	if err := db.Exec(`CREATE TABLE drift_child (
		id CHAR(36) NOT NULL PRIMARY KEY,
		team_id CHAR(36) NOT NULL,
		INDEX idx_dc_team (team_id),
		CONSTRAINT fk_dc_team FOREIGN KEY (team_id) REFERENCES teams(id) ON DELETE RESTRICT
	) ENGINE=InnoDB`).Error; err != nil {
		t.Fatalf("create drifted child table: %v", err)
	}

	err := svc.purgeSchemaDrift(ctx)
	if codeOf(err) != "TENANT_PURGE_SCHEMA_DRIFT" {
		t.Fatalf("guard stayed SILENT on a child that would be orphaned: err = %v", err)
	}
	if !strings.Contains(err.Error(), "drift_child") {
		t.Errorf("refusal does not name the offending table: %v", err)
	}

	// And the refusal must stop a real purge, not merely exist as a helper nobody calls.
	victim := seedPurgeTenant(t, db, "drifted")
	if rerr := svc.RetireTenant(ctx, "operator-tenant", victim); rerr != nil {
		t.Fatalf("retire: %v", rerr)
	}
	if _, perr := svc.PurgeTenant(ctx, "operator-tenant", "op-1", victim, "drifted", false); codeOf(perr) != "TENANT_PURGE_SCHEMA_DRIFT" {
		t.Fatalf("PurgeTenant ran despite the drift: %v", perr)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM users WHERE tenant_id = ?", victim); n != 1 {
		t.Errorf("rows were deleted despite the refusal (users = %d)", n)
	}
}

// TestPurgeTenant_BlobsAreNotSilentlyLeftBehind pins the honesty rule that the first version of
// this purge broke: it deleted the sys_files CATALOG rows and left every byte on disk, while the
// report — counting rows only — read as though the customer's documents were gone. Telling someone
// their data was deleted when it was not is worse than failing to delete it.
func TestPurgeTenant_BlobsAreNotSilentlyLeftBehind(t *testing.T) {
	db, _ := o3aSetup(t)
	ctx := context.Background()

	seed := func(slug string) string {
		id := seedPurgeTenant(t, db, slug)
		// Two catalog rows with no bytes behind them — enough to exercise the accounting.
		for i, fid := range []string{"f1-" + slug, "f2-" + slug} {
			if err := db.Exec(`INSERT INTO sys_files
				(id, tenant_id, entity_name, record_id, field_name, name, mime, size, path, created_by, created_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,NOW())`,
				fid, id, "crm_accounts", "rec-1", "avatar",
				"a.png", "image/png", int64(10+i), id+"/"+fid, "us-"+slug).Error; err != nil {
				t.Fatalf("seed file: %v", err)
			}
		}
		return id
	}

	t.Run("no blob remover wired -> report SAYS the files remain", func(t *testing.T) {
		svc := NewTenantProvisionService(db) // deliberately unwired
		id := seed("noremover")
		if err := svc.RetireTenant(ctx, "operator-tenant", id); err != nil {
			t.Fatalf("retire: %v", err)
		}
		rep, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", id, "noremover", false)
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if rep.BlobsRemoved != 0 {
			t.Errorf("BlobsRemoved = %d with no remover wired", rep.BlobsRemoved)
		}
		if rep.BlobsNote == "" {
			t.Fatal("purge stayed SILENT about 2 files it could not delete — the report implies they went with the rows")
		}
		if !strings.Contains(rep.BlobsNote, "VẪN CÒN") {
			t.Errorf("note does not say the files survive: %q", rep.BlobsNote)
		}
	})

	t.Run("remover wired -> it is called with every file id, and the count is reported", func(t *testing.T) {
		var gotTenant string
		var gotIDs []string
		svc := NewTenantProvisionService(db).WithBlobRemover(
			func(_ context.Context, tenantID string, ids []string) (int, error) {
				gotTenant, gotIDs = tenantID, ids
				return len(ids), nil
			})
		id := seed("withremover")
		if err := svc.RetireTenant(ctx, "operator-tenant", id); err != nil {
			t.Fatalf("retire: %v", err)
		}
		rep, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", id, "withremover", false)
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if gotTenant != id || len(gotIDs) != 2 {
			t.Fatalf("remover got tenant=%q ids=%v; want the purged tenant and its 2 files", gotTenant, gotIDs)
		}
		if rep.BlobsRemoved != 2 {
			t.Errorf("BlobsRemoved = %d; want 2", rep.BlobsRemoved)
		}
		if rep.BlobsNote != "" {
			t.Errorf("clean run still warned: %q", rep.BlobsNote)
		}
	})

	t.Run("partial failure is REPORTED, not swallowed", func(t *testing.T) {
		svc := NewTenantProvisionService(db).WithBlobRemover(
			func(_ context.Context, _ string, ids []string) (int, error) { return len(ids) - 1, nil })
		id := seed("partial")
		if err := svc.RetireTenant(ctx, "operator-tenant", id); err != nil {
			t.Fatalf("retire: %v", err)
		}
		rep, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", id, "partial", false)
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if rep.BlobsNote == "" {
			t.Error("one file survived and the report said nothing")
		}
	})
}

// specs/068 T011: the purge and the customer's backup zips. A purge that deleted the catalogue rows
// but left the files would keep a "deleted" customer's real data on the backup disk.
func TestPurgeTenant_BackupFilesFollowTheCustomer(t *testing.T) {
	db, _ := o3aSetup(t)
	ctx := context.Background()

	// A backup catalogue row (+ optionally a file under dir/<tenant>/). status 'running' takes the
	// op_lock; any other status does not.
	seedBackup := func(tenantID, id, status, dir string, withFile bool) string {
		key := tenantID + "/" + id + ".zip"
		if err := db.Exec(`INSERT INTO sys_tenant_backups (id, tenant_id, kind, status, storage_key, format_version, schema_version)
			VALUES (?,?,'manual',?,?,1,85)`, id, tenantID, status, key).Error; err != nil {
			t.Fatalf("seed backup: %v", err)
		}
		if withFile {
			if err := os.MkdirAll(filepath.Join(dir, tenantID), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, key), []byte("zip"), 0o640); err != nil {
				t.Fatal(err)
			}
		}
		return key
	}
	// The closure main.go wires is tenantbackup.NewRemover (tested in that package; it cannot be
	// imported here — tenantbackup imports files imports services). This stand-in has the same
	// contract: remove each key, then sweep the customer's directory.
	remover := func(dir string) func(context.Context, string, []string) (int, error) {
		return func(_ context.Context, tenantID string, keys []string) (int, error) {
			n := 0
			for _, k := range keys {
				if err := os.Remove(filepath.Join(dir, k)); err == nil {
					n++
				}
			}
			return n, os.RemoveAll(filepath.Join(dir, tenantID))
		}
	}
	retire := func(svc *TenantProvisionService, id string) {
		t.Helper()
		if err := svc.RetireTenant(ctx, "operator-tenant", id); err != nil {
			t.Fatalf("retire: %v", err)
		}
	}

	t.Run("two backups: rows and files go, backupsRemoved = 2, a stray file under the directory goes too", func(t *testing.T) {
		dir := t.TempDir()
		svc := NewTenantProvisionService(db).WithBackupRemover(remover(dir))
		id := seedPurgeTenant(t, db, "bk2")
		seedBackup(id, "bk2-a", "ready", dir, true)
		seedBackup(id, "bk2-b", "failed", dir, true)
		stray := filepath.Join(dir, id, "stray.zip") // a race left it; no row names it
		if err := os.WriteFile(stray, []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
		retire(svc, id)

		dry, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", id, "bk2", true)
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if dry.BackupsNote == "" || dry.BackupsRemoved != 0 {
			t.Errorf("dry run must announce the backups and remove none: %+v", dry)
		}
		if _, err := os.Stat(stray); err != nil {
			t.Fatalf("a dry run must not touch files: %v", err)
		}
		if countRows(t, db, "SELECT COUNT(*) FROM sys_tenant_backups WHERE tenant_id = ?", id) != 2 {
			t.Fatal("a dry run must not delete catalogue rows")
		}

		rep, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", id, "bk2", false)
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if rep.BackupsRemoved != 2 || rep.BackupsNote != "" {
			t.Errorf("report = removed %d note %q; want 2 and no note", rep.BackupsRemoved, rep.BackupsNote)
		}
		if countRows(t, db, "SELECT COUNT(*) FROM sys_tenant_backups WHERE tenant_id = ?", id) != 0 {
			t.Error("the catalogue rows must be purged with the tenant")
		}
		if _, err := os.Stat(filepath.Join(dir, id)); !os.IsNotExist(err) {
			t.Errorf("the customer's backup directory (stray file included) must be gone: %v", err)
		}
	})

	t.Run("a running backup refuses the purge and removes nothing", func(t *testing.T) {
		dir := t.TempDir()
		svc := NewTenantProvisionService(db).WithBackupRemover(remover(dir))
		id := seedPurgeTenant(t, db, "bkrun")
		key := seedBackup(id, "bkrun-a", "running", dir, true)
		retire(svc, id)
		_, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", id, "bkrun", false)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "BACKUP_IN_PROGRESS" {
			t.Fatalf("want BACKUP_IN_PROGRESS, got %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, key)); err != nil {
			t.Errorf("the file must still exist: %v", err)
		}
		if countRows(t, db, "SELECT COUNT(*) FROM users WHERE tenant_id = ?", id) != 1 {
			t.Error("a refused purge must delete nothing")
		}
		// the DRY run is a read and still answers
		if _, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", id, "bkrun", true); err != nil {
			t.Errorf("dry run while a backup runs: %v", err)
		}
	})

	t.Run("no remover wired: the report says the backup files remain", func(t *testing.T) {
		dir := t.TempDir()
		svc := NewTenantProvisionService(db)
		id := seedPurgeTenant(t, db, "bknone")
		seedBackup(id, "bknone-a", "ready", dir, true)
		retire(svc, id)
		rep, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", id, "bknone", false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rep.BackupsNote, "VẪN CÒN") {
			t.Errorf("purge stayed silent about backup files it did not remove: %q", rep.BackupsNote)
		}
	})

	t.Run("a tenant with no backups still calls the remover (sweeps a stray directory)", func(t *testing.T) {
		dir := t.TempDir()
		var called bool
		svc := NewTenantProvisionService(db).WithBackupRemover(func(c context.Context, tenantID string, keys []string) (int, error) {
			called = true
			return remover(dir)(c, tenantID, keys)
		})
		id := seedPurgeTenant(t, db, "bkzero")
		retire(svc, id)
		if _, err := svc.PurgeTenant(ctx, "operator-tenant", "op-1", id, "bkzero", false); err != nil {
			t.Fatal(err)
		}
		if !called {
			t.Error("the remover must run even with no known keys: a racing backup may have written a file after the keys were read")
		}
	})
}

// TestPurgeSchemaDrift_FiresOnAnUnscopedUnknownTable covers the guard's POSITIVE half. The FK-based
// check can only see a table the schema connects to a tenant-scoped parent; a table with NEITHER a
// tenant_id column NOR any FK is invisible to it — and such a table can still hold per-customer
// rows, keyed by a user id or an entity name. Three tables legitimately have that shape
// (schema_migrations, tenants, sys_unique_demote_log) and are allowlisted ON PURPOSE; a fourth must
// stop the purge until someone opens it and decides.
func TestPurgeSchemaDrift_FiresOnAnUnscopedUnknownTable(t *testing.T) {
	db, _ := o3aSetup(t)
	svc := NewTenantProvisionService(db)
	ctx := context.Background()

	if err := svc.purgeSchemaDrift(ctx); err != nil {
		t.Fatalf("guard fired on the shipped schema before the drift was introduced: %v", err)
	}

	// No tenant_id, no FK — the engine cannot tell whether this holds customer rows.
	if err := db.Exec(`CREATE TABLE mystery_log (
		id CHAR(36) NOT NULL PRIMARY KEY,
		entity_name VARCHAR(64) NOT NULL,
		note TEXT
	) ENGINE=InnoDB`).Error; err != nil {
		t.Fatalf("create unscoped table: %v", err)
	}

	err := svc.purgeSchemaDrift(ctx)
	if codeOf(err) != "TENANT_PURGE_SCHEMA_DRIFT" {
		t.Fatalf("guard stayed silent on an unscoped unknown table: err = %v", err)
	}
	if !strings.Contains(err.Error(), "mystery_log") {
		t.Errorf("refusal does not name the table: %v", err)
	}
}
