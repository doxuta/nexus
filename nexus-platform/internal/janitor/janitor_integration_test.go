//go:build integration

// Janitor integration tests — run against a real throwaway MySQL 8 database.
//
// Proves the retention sweep against a live schema (all migrations applied): rows OLDER than the
// cutoff are deleted, NEWER rows survive, and the sweep is TENANT-AGNOSTIC (an old row from tenant B
// is deleted just like tenant A's — these are cross-tenant system tables). The token tables have
// FKs to tenants/users, so the seed inserts those parents first.
//
// Run with (B7: GORM/sqlx/migrate all share ONE TEST_DATABASE_URL source via janSplitDSN):
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/janitor/ -run TestJanitorSweep_Integration -race -v
package janitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	_ "github.com/go-sql-driver/mysql"
)

const janMigrationsDir = "../../migrations"

var janDBCounter int64

func janUniqueDBName() string {
	n := atomic.AddInt64(&janDBCounter, 1)
	return fmt.Sprintf("nexus_jan_it_%d_%d", os.Getpid(), n)
}

// janSplitDSN splits TEST_DATABASE_URL into (creds, host, params) so the admin connection, the
// throwaway-DB connection, and the migrate CLI all derive from ONE source (B7).
func janSplitDSN(t *testing.T) (creds, host, params string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		base = "root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local"
	}
	at := strings.Index(base, "@tcp(")
	if at < 0 {
		t.Fatalf("janSplitDSN: unexpected DSN shape %q", base)
	}
	creds = base[:at]
	rest := base[at:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		t.Fatalf("janSplitDSN: no db segment in DSN %q", base)
	}
	host = rest[:slash+1]
	if q := strings.Index(rest, "?"); q >= 0 {
		params = rest[q:]
	}
	return creds, host, params
}

func janAdminDSN(t *testing.T) string { c, h, p := janSplitDSN(t); return c + h + p }
func janDSN(t *testing.T, db string) string {
	c, h, p := janSplitDSN(t)
	return c + h + db + p
}
func janMigrateDSN(t *testing.T, db string) string {
	c, h, p := janSplitDSN(t)
	return "mysql://" + c + h + db + p
}

func janMigrateBin(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("migrate"); err == nil {
		return p
	}
	if home, _ := os.UserHomeDir(); home != "" {
		cand := home + "/go/bin/migrate"
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

// janSetup creates a throwaway DB, migrates it up, and returns a sqlx handle over it.
func janSetup(t *testing.T) *sqlx.DB {
	t.Helper()
	dbName := janUniqueDBName()

	admin, err := sqlx.Connect("mysql", janAdminDSN(t))
	if err != nil {
		t.Skipf("janitor: MySQL not reachable (%v) — skipping", err)
	}
	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`"); err != nil {
		t.Fatalf("janitor: drop throwaway DB: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("janitor: create throwaway DB: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		_ = admin.Close()
	})

	migrateBin := janMigrateBin(t)
	if migrateBin == "" {
		t.Skip("janitor: migrate binary not found — skipping")
	}
	cmd := exec.Command(migrateBin, "-path", janMigrationsDir, "-database", janMigrateDSN(t, dbName), "up")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("janitor: migrate up: %v\n%s", err, out)
	}

	sdb, err := sqlx.Connect("mysql", janDSN(t, dbName))
	if err != nil {
		t.Fatalf("janitor: open sqlx: %v", err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	return sdb
}

// seedTenantUser inserts a tenant + one user (FK parents for the token tables).
func seedTenantUser(t *testing.T, db *sqlx.DB, tenantID, userID, slug string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO tenants (id, slug, name, status) VALUES (?, ?, ?, 'active')`,
		tenantID, slug, slug); err != nil {
		t.Fatalf("seed tenant %s: %v", tenantID, err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, 'x', 'active')`,
		userID, tenantID, slug+"@t.test"); err != nil {
		t.Fatalf("seed user %s: %v", userID, err)
	}
}

func seedRun(t *testing.T, db *sqlx.DB, id, tenantID string, finishedAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO sys_workflow_runs
		(id, tenant_id, workflow_id, trigger_type, status, started_at, finished_at)
		VALUES (?, ?, ?, 'after_create', 'success', ?, ?)`,
		id, tenantID, "wf-"+id, finishedAt.Add(-time.Second), finishedAt); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
}

func seedToken(t *testing.T, db *sqlx.DB, table, id, tenantID, userID string, expiresAt time.Time) {
	t.Helper()
	q := fmt.Sprintf(`INSERT INTO %s (id, tenant_id, user_id, token_hash, expires_at) VALUES (?, ?, ?, ?, ?)`, table)
	if _, err := db.Exec(q, id, tenantID, userID, id+"-hash", expiresAt); err != nil {
		t.Fatalf("seed %s %s: %v", table, id, err)
	}
}

// seedJob inserts one sys_jobs row (JOBS-1 retention). finishedAt nil = still queued/running.
func seedJob(t *testing.T, db *sqlx.DB, id, status string, finishedAt *time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO sys_jobs (id, job_type, status, run_at, finished_at)
		VALUES (?, 'noop', ?, NOW(3), ?)`, id, status, finishedAt); err != nil {
		t.Fatalf("seed sys_jobs %s: %v", id, err)
	}
}

// seedIdemKey inserts one sys_idempotency_keys claim (specs/047) expiring at expiresAt.
func seedIdemKey(t *testing.T, db *sqlx.DB, id, tenantID, userID string, expiresAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO sys_idempotency_keys
		(id, tenant_id, principal_id, idem_key, entity_name, request_hash, status, record_id, expires_at)
		VALUES (?, ?, ?, ?, 'crm_contacts', ?, 'completed', ?, ?)`,
		id, tenantID, userID, "key-"+id, strings.Repeat("a", 64), "rec-"+id, expiresAt); err != nil {
		t.Fatalf("seed sys_idempotency_keys %s: %v", id, err)
	}
}

// seedPreviewSession inserts one sys_preview_sessions row (specs/064) expiring at expiresAt.
func seedPreviewSession(t *testing.T, db *sqlx.DB, id, tenantID, userID string, expiresAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO sys_preview_sessions
		(id, tenant_id, role_id, principal_id, actor_user_id, actor_tenant_id, actor_session_id, expires_at, created_by)
		VALUES (?, ?, 'r', 'p', ?, ?, 's', ?, ?)`, id, tenantID, userID, tenantID, expiresAt, userID); err != nil {
		t.Fatalf("seed sys_preview_sessions %s: %v", id, err)
	}
}

// seedEmailLog inserts one sys_email_log row (specs/066) created at createdAt.
func seedEmailLog(t *testing.T, db *sqlx.DB, id, tenantID string, createdAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO sys_email_log (id, tenant_id, source, status, recipient_masked, created_at)
		VALUES (?, ?, 'workflow', 'sent', 'a***@x.vn', ?)`, id, tenantID, createdAt); err != nil {
		t.Fatalf("seed sys_email_log %s: %v", id, err)
	}
}

// seedUsageDaily inserts one sys_usage_daily row (specs/066 L4) for the calendar day `day`.
func seedUsageDaily(t *testing.T, db *sqlx.DB, id, tenantID string, day time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO sys_usage_daily (id, tenant_id, day, metric, dim, value)
		VALUES (?, ?, ?, 'workflow_runs', '', 7)`, id, tenantID, day.UTC().Format("2006-01-02")); err != nil {
		t.Fatalf("seed sys_usage_daily %s: %v", id, err)
	}
}

func countRows(t *testing.T, db *sqlx.DB, table string) int {
	t.Helper()
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM "+table); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func rowExists(t *testing.T, db *sqlx.DB, table, id string) bool {
	t.Helper()
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM "+table+" WHERE id = ?", id); err != nil {
		t.Fatalf("exists %s %s: %v", table, id, err)
	}
	return n > 0
}

// TestJanitorSweep_Integration seeds old+new rows across two tenants in every table the sweep covers (workflow runs, the two token tables, jobs,
// idempotency keys, preview sessions, email log) and asserts
// the sweep deletes ONLY the old rows (both tenants), keeps the new rows, and is tenant-agnostic.
func TestJanitorSweep_Integration(t *testing.T) {
	db := janSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const (
		tA, uA = "11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		tB, uB = "22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	)
	seedTenantUser(t, db, tA, uA, "tenant-a")
	seedTenantUser(t, db, tB, uB, "tenant-b")

	old := now.Add(-100 * 24 * time.Hour) // > 90d runs retention
	recent := now.Add(-24 * time.Hour)    // within retention
	tokenOld := now.Add(-48 * time.Hour)  // expired > 1d grace ago
	tokenNew := now.Add(1 * time.Hour)    // not yet expired

	// sys_workflow_runs: one old + one recent per tenant.
	seedRun(t, db, "run-old-a", tA, old)
	seedRun(t, db, "run-new-a", tA, recent)
	seedRun(t, db, "run-old-b", tB, old)
	seedRun(t, db, "run-new-b", tB, recent)

	// token tables: one expired-past-grace + one live per tenant.
	for _, tbl := range []string{"mfa_pending_tokens", "password_reset_tokens"} {
		seedToken(t, db, tbl, "tok-old-a", tA, uA, tokenOld)
		seedToken(t, db, tbl, "tok-new-a", tA, uA, tokenNew)
		seedToken(t, db, tbl, "tok-old-b", tB, uB, tokenOld)
		seedToken(t, db, tbl, "tok-new-b", tB, uB, tokenNew)
	}

	// sys_jobs (JOBS-1): finished-old is swept; finished-recent survives; a stale RUNNING row
	// survives regardless of age (finished_at NULL — in-flight work is the reaper's business,
	// never the janitor's).
	jobOld := now.Add(-40 * 24 * time.Hour) // > 30d jobs retention
	jobNew := now.Add(-24 * time.Hour)
	seedJob(t, db, "job-old-done", "done", &jobOld)
	seedJob(t, db, "job-old-failed", "failed", &jobOld)
	seedJob(t, db, "job-new-done", "done", &jobNew)
	seedJob(t, db, "job-running", "running", nil)

	// sys_idempotency_keys (specs/047): a claim past its 24 h window is swept (both tenants); a
	// claim still inside it survives — its replay promise still stands.
	seedIdemKey(t, db, "idem-old-a", tA, uA, now.Add(-time.Minute))
	seedIdemKey(t, db, "idem-old-b", tB, uB, now.Add(-48*time.Hour))
	seedIdemKey(t, db, "idem-new-a", tA, uA, now.Add(23*time.Hour))
	seedIdemKey(t, db, "idem-new-b", tB, uB, now.Add(time.Minute))

	// sys_preview_sessions (specs/064): a trail row past the 90 d runs retention is swept; a row that
	// expired recently (trail still wanted) and a live session survive.
	seedPreviewSession(t, db, "prev-old-a", tA, uA, now.Add(-100*24*time.Hour))
	seedPreviewSession(t, db, "prev-old-b", tB, uB, now.Add(-91*24*time.Hour))
	seedPreviewSession(t, db, "prev-recent-a", tA, uA, now.Add(-time.Hour))
	seedPreviewSession(t, db, "prev-live-b", tB, uB, now.Add(20*time.Minute))

	// sys_email_log (specs/066): rows past the 90 d runs retention are swept, newer ones kept.
	seedEmailLog(t, db, "mail-old-a", tA, now.Add(-100*24*time.Hour))
	seedEmailLog(t, db, "mail-old-b", tB, now.Add(-91*24*time.Hour))
	seedEmailLog(t, db, "mail-new-a", tA, now.Add(-24*time.Hour))

	// sys_usage_daily (specs/066 L4): its own 730 d retention — the 91-day-old figure that the runs
	// retention would have taken must survive; only the rows older than two years go (both tenants).
	seedUsageDaily(t, db, "use-old-a", tA, now.Add(-800*24*time.Hour))
	seedUsageDaily(t, db, "use-old-b", tB, now.Add(-731*24*time.Hour))
	seedUsageDaily(t, db, "use-91d-a", tA, now.Add(-91*24*time.Hour))
	seedUsageDaily(t, db, "use-new-b", tB, now.Add(-24*time.Hour))

	// Run one deterministic sweep with a small batch size so the batch LOOP is exercised against
	// real DELETE ... LIMIT (2 old rows per table → at least two iterations at batchSize=1).
	j := New(Config{Interval: time.Hour, RunsRetention: 90 * 24 * time.Hour, TokenGrace: 24 * time.Hour,
		JobsRetention: 30 * 24 * time.Hour}, NewSQLSweeper(db), nil)
	j.batchSize = 1
	j.Sweep(ctx, now)

	// sys_workflow_runs: old rows (both tenants) gone, new rows survive.
	if n := countRows(t, db, "sys_workflow_runs"); n != 2 {
		t.Fatalf("sys_workflow_runs remaining = %d, want 2 (both new)", n)
	}
	for _, id := range []string{"run-old-a", "run-old-b"} {
		if rowExists(t, db, "sys_workflow_runs", id) {
			t.Fatalf("old run %s should have been deleted (tenant-agnostic)", id)
		}
	}
	for _, id := range []string{"run-new-a", "run-new-b"} {
		if !rowExists(t, db, "sys_workflow_runs", id) {
			t.Fatalf("recent run %s must survive retention", id)
		}
	}

	// sys_jobs: finished-old rows gone; finished-recent and in-flight running rows survive.
	if n := countRows(t, db, "sys_jobs"); n != 2 {
		t.Fatalf("sys_jobs remaining = %d, want 2 (recent done + running)", n)
	}
	for _, id := range []string{"job-old-done", "job-old-failed"} {
		if rowExists(t, db, "sys_jobs", id) {
			t.Fatalf("old finished job %s should have been deleted", id)
		}
	}
	for _, id := range []string{"job-new-done", "job-running"} {
		if !rowExists(t, db, "sys_jobs", id) {
			t.Fatalf("job %s must survive retention", id)
		}
	}

	// sys_idempotency_keys: expired claims (both tenants) gone, live claims survive.
	if n := countRows(t, db, "sys_idempotency_keys"); n != 2 {
		t.Fatalf("sys_idempotency_keys remaining = %d, want 2 (both live)", n)
	}
	for _, id := range []string{"idem-old-a", "idem-old-b"} {
		if rowExists(t, db, "sys_idempotency_keys", id) {
			t.Fatalf("expired idempotency claim %s should have been deleted (tenant-agnostic)", id)
		}
	}
	for _, id := range []string{"idem-new-a", "idem-new-b"} {
		if !rowExists(t, db, "sys_idempotency_keys", id) {
			t.Fatalf("live idempotency claim %s must survive — its replay window is still open", id)
		}
	}

	// sys_preview_sessions: only the two rows past retention are gone.
	if n := countRows(t, db, "sys_preview_sessions"); n != 2 {
		t.Fatalf("sys_preview_sessions remaining = %d, want 2 (recent + live)", n)
	}
	for _, id := range []string{"prev-old-a", "prev-old-b"} {
		if rowExists(t, db, "sys_preview_sessions", id) {
			t.Fatalf("old preview session %s should have been deleted (tenant-agnostic)", id)
		}
	}
	for _, id := range []string{"prev-recent-a", "prev-live-b"} {
		if !rowExists(t, db, "sys_preview_sessions", id) {
			t.Fatalf("preview session %s must survive retention", id)
		}
	}

	// sys_email_log: only the rows past retention are gone (both tenants).
	if n := countRows(t, db, "sys_email_log"); n != 1 || !rowExists(t, db, "sys_email_log", "mail-new-a") {
		t.Fatalf("sys_email_log remaining = %d, want only mail-new-a", n)
	}

	if n := countRows(t, db, "sys_usage_daily"); n != 2 || !rowExists(t, db, "sys_usage_daily", "use-91d-a") || !rowExists(t, db, "sys_usage_daily", "use-new-b") {
		t.Fatalf("sys_usage_daily remaining = %d, want only use-91d-a and use-new-b", n)
	}

	// token tables: expired-past-grace rows (both tenants) gone, live rows survive.
	for _, tbl := range []string{"mfa_pending_tokens", "password_reset_tokens"} {
		if n := countRows(t, db, tbl); n != 2 {
			t.Fatalf("%s remaining = %d, want 2 (both live)", tbl, n)
		}
		for _, id := range []string{"tok-old-a", "tok-old-b"} {
			if rowExists(t, db, tbl, id) {
				t.Fatalf("%s expired token %s should have been deleted", tbl, id)
			}
		}
		for _, id := range []string{"tok-new-a", "tok-new-b"} {
			if !rowExists(t, db, tbl, id) {
				t.Fatalf("%s live token %s must survive", tbl, id)
			}
		}
	}
}
