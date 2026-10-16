//go:build integration

// JOBS-1 store integration tests — run against a real throwaway MySQL 8 database.
//
// Proves the queue's concurrency contract on live InnoDB:
//   - enqueue → claim flips status/claimed_by and increments attempts atomically;
//   - FOR UPDATE SKIP LOCKED: a row locked by a concurrent claim transaction is SKIPPED (no
//     block, no double-claim) — and N racing claimers over M jobs claim each job exactly once;
//   - a failing handler walks the full retry ladder (reschedule with backoff → terminal failed);
//   - the reaper requeues a stale running job (crashed worker) and terminally fails one whose
//     attempt budget is already burnt;
//   - an unknown job_type fails immediately (never retried);
//   - Manager end-to-end: Start → enqueue → fail-once-then-succeed handler → done → Stop.
//
// Run with (B7: sqlx + migrate CLI share ONE TEST_DATABASE_URL source via jqSplitDSN):
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/jobs/ -race -v
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	_ "github.com/go-sql-driver/mysql"
)

const jqMigrationsDir = "../../migrations"

var jqDBCounter int64

func jqUniqueDBName() string {
	n := atomic.AddInt64(&jqDBCounter, 1)
	return fmt.Sprintf("nexus_jobs_it_%d_%d", os.Getpid(), n)
}

// jqSplitDSN splits TEST_DATABASE_URL into (creds, host, params) so the admin connection, the
// throwaway-DB connection, and the migrate CLI all derive from ONE source (B7).
func jqSplitDSN(t *testing.T) (creds, host, params string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		base = "root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local"
	}
	at := strings.Index(base, "@tcp(")
	if at < 0 {
		t.Fatalf("jqSplitDSN: unexpected DSN shape %q", base)
	}
	creds = base[:at]
	rest := base[at:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		t.Fatalf("jqSplitDSN: no db segment in DSN %q", base)
	}
	host = rest[:slash+1]
	if q := strings.Index(rest, "?"); q >= 0 {
		params = rest[q:]
	}
	return creds, host, params
}

func jqAdminDSN(t *testing.T) string { c, h, p := jqSplitDSN(t); return c + h + p }
func jqDSN(t *testing.T, db string) string {
	c, h, p := jqSplitDSN(t)
	return c + h + db + p
}
func jqMigrateDSN(t *testing.T, db string) string {
	c, h, p := jqSplitDSN(t)
	return "mysql://" + c + h + db + p
}

func jqMigrateBin(t *testing.T) string {
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

// jqSetup creates a throwaway DB, migrates it up, and returns a sqlx handle over it.
func jqSetup(t *testing.T) *sqlx.DB {
	t.Helper()
	dbName := jqUniqueDBName()

	admin, err := sqlx.Connect("mysql", jqAdminDSN(t))
	if err != nil {
		t.Skipf("jobs: MySQL not reachable (%v) — skipping", err)
	}
	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`"); err != nil {
		t.Fatalf("jobs: drop throwaway DB: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("jobs: create throwaway DB: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		_ = admin.Close()
	})

	migrateBin := jqMigrateBin(t)
	if migrateBin == "" {
		t.Skip("jobs: migrate binary not found — skipping")
	}
	cmd := exec.Command(migrateBin, "-path", jqMigrationsDir, "-database", jqMigrateDSN(t, dbName), "up")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("jobs: migrate up: %v\n%s", err, out)
	}

	sdb, err := sqlx.Connect("mysql", jqDSN(t, dbName))
	if err != nil {
		t.Fatalf("jobs: open sqlx: %v", err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	return sdb
}

// jobState is the assertion scan shape of one sys_jobs row.
type jobState struct {
	Status      string     `db:"status"`
	Attempts    int        `db:"attempts"`
	ClaimedBy   *string    `db:"claimed_by"`
	ClaimedAt   *time.Time `db:"claimed_at"`
	LastError   *string    `db:"last_error"`
	FinishedAt  *time.Time `db:"finished_at"`
	RunAt       time.Time  `db:"run_at"`
	TenantID    *string    `db:"tenant_id"`
	MaxAttempts int        `db:"max_attempts"`
}

func jqState(t *testing.T, db *sqlx.DB, id string) jobState {
	t.Helper()
	var st jobState
	if err := db.Get(&st, `SELECT status, attempts, claimed_by, claimed_at, last_error, finished_at,
		run_at, tenant_id, max_attempts FROM sys_jobs WHERE id = ?`, id); err != nil {
		t.Fatalf("read job %s: %v", id, err)
	}
	return st
}

// TestJobsEnqueueClaimDone_Integration walks the happy path: enqueue (tenant-pinned + system) →
// claim (status/claimed_by/attempts flip atomically, payload round-trips) → done (finished_at
// set) → a stale MarkDone from the old claim reports applied=false.
func TestJobsEnqueueClaimDone_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()

	tenantJobID, err := store.Enqueue(ctx, Job{Type: "noop", TenantID: "11111111-1111-1111-1111-111111111111",
		Payload: []byte(`{"k":"v"}`)})
	if err != nil {
		t.Fatalf("enqueue tenant job: %v", err)
	}
	if _, err := store.Enqueue(ctx, Job{Type: "noop", RunAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("enqueue future job: %v", err)
	}
	if _, err := store.Enqueue(ctx, Job{}); err == nil {
		t.Fatal("empty job type must be rejected")
	}

	// Tenant pinning persisted; the future job must NOT be claimable yet.
	st := jqState(t, db, tenantJobID)
	if st.TenantID == nil || *st.TenantID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("tenant_id = %v, want pinned", st.TenantID)
	}

	job, err := store.ClaimNext(ctx, "worker-a", time.Now(), Lane{})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job == nil || job.ID != tenantJobID {
		t.Fatalf("claimed %+v, want the due tenant job %s (not the future one)", job, tenantJobID)
	}
	// Compare the payload PARSED, not byte-for-byte: MySQL's JSON column re-serializes on read.
	var payload map[string]string
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload["k"] != "v" {
		t.Fatalf("claimed payload = %s (parse err %v), want k=v", job.Payload, err)
	}
	if job.Attempts != 1 || job.TenantID == "" {
		t.Fatalf("claimed job = %+v, want attempts=1 + tenant", job)
	}
	st = jqState(t, db, tenantJobID)
	if st.Status != StatusRunning || st.ClaimedBy == nil || *st.ClaimedBy != "worker-a" || st.Attempts != 1 || st.ClaimedAt == nil {
		t.Fatalf("row after claim = %+v, want running/worker-a/attempts=1", st)
	}

	// Only the future job remains → nothing due.
	if extra, err := store.ClaimNext(ctx, "worker-b", time.Now(), Lane{}); err != nil || extra != nil {
		t.Fatalf("second claim = (%+v, %v), want (nil, nil): future job not due", extra, err)
	}

	applied, err := store.MarkDone(ctx, job.ID, "worker-a", time.Now())
	if err != nil || !applied {
		t.Fatalf("MarkDone = (%v, %v), want applied", applied, err)
	}
	st = jqState(t, db, tenantJobID)
	if st.Status != StatusDone || st.FinishedAt == nil {
		t.Fatalf("row after done = %+v", st)
	}

	// A second (stale) MarkDone must be a no-op: the row is no longer running under this claim.
	applied, err = store.MarkDone(ctx, job.ID, "worker-a", time.Now())
	if err != nil || applied {
		t.Fatalf("stale MarkDone = (%v, %v), want applied=false", applied, err)
	}
}

// TestJobsClaimSkipLocked_Integration proves the SKIP LOCKED contract two ways:
//  1. a row locked by an open FOR UPDATE transaction is SKIPPED by ClaimNext (returns nil
//     immediately instead of blocking on the lock);
//  2. 4 goroutines racing ClaimNext over 20 jobs claim each job EXACTLY once (no double-claim).
func TestJobsClaimSkipLocked_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()

	// (1) Lock the only queued row in an open transaction, then try to claim.
	id, err := store.Enqueue(ctx, Job{Type: "noop"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var locked string
	if err := tx.Get(&locked, `SELECT id FROM sys_jobs WHERE id = ? FOR UPDATE`, id); err != nil {
		t.Fatalf("lock row: %v", err)
	}
	claimCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	job, err := store.ClaimNext(claimCtx, "worker-a", time.Now(), Lane{})
	cancel()
	if err != nil {
		t.Fatalf("claim against locked row errored (must skip, not block/fail): %v", err)
	}
	if job != nil {
		t.Fatalf("claimed %s while another tx held its lock — SKIP LOCKED violated", job.ID)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// (2) Concurrency: 4 racing claimers over 20 jobs → each job claimed exactly once.
	if _, err := db.Exec(`DELETE FROM sys_jobs`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	const jobCount = 20
	for i := 0; i < jobCount; i++ {
		if _, err := store.Enqueue(ctx, Job{Type: "noop"}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			workerID := fmt.Sprintf("racer-%d", w)
			for {
				job, err := store.ClaimNext(ctx, workerID, time.Now(), Lane{})
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if job == nil {
					return // drained
				}
				mu.Lock()
				claimed[job.ID]++
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(claimed) != jobCount {
		t.Fatalf("claimed %d distinct jobs, want %d", len(claimed), jobCount)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Fatalf("job %s claimed %d times — SKIP LOCKED must make claims exclusive", id, n)
		}
	}
}

// TestJobsRetryBackoffThenFail_Integration drives a job with max_attempts=2 through the real
// manager transition logic against live SQL: attempt 1 fails → requeued with run_at in the
// future + last_error; attempt 2 fails → terminal status=failed with finished_at.
func TestJobsRetryBackoffThenFail_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()

	reg := NewRegistry()
	if err := reg.Register("flaky", func(context.Context, Job) error {
		return fmt.Errorf("smtp: connection refused")
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	m := NewManager(ManagerConfig{Workers: 1, PollInterval: time.Second, Lease: time.Minute}, store, reg, nil)
	m.backoffBase = 50 * time.Millisecond
	m.backoffCap = time.Second

	id, err := store.Enqueue(ctx, Job{Type: "flaky", MaxAttempts: 2})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Attempt 1: claim + run → rescheduled with backoff.
	before := time.Now()
	job, err := store.ClaimNext(ctx, "w0", time.Now(), Lane{})
	if err != nil || job == nil {
		t.Fatalf("claim 1 = (%+v, %v)", job, err)
	}
	m.runJob("w0", job)
	st := jqState(t, db, id)
	if st.Status != StatusQueued || st.Attempts != 1 {
		t.Fatalf("after attempt 1 = %+v, want queued/attempts=1", st)
	}
	if st.LastError == nil || !strings.Contains(*st.LastError, "connection refused") {
		t.Fatalf("last_error = %v, want the handler error", st.LastError)
	}
	if !st.RunAt.After(before) {
		t.Fatalf("run_at = %s not pushed into the future (backoff missing)", st.RunAt)
	}

	// Not due yet: claiming right now must return nothing.
	if early, err := store.ClaimNext(ctx, "w0", before, Lane{}); err != nil || early != nil {
		t.Fatalf("claim before backoff elapsed = (%+v, %v), want nil", early, err)
	}

	// Attempt 2 (budget exhausted): wait out the backoff, claim, run → terminal failed.
	time.Sleep(80 * time.Millisecond)
	job, err = store.ClaimNext(ctx, "w0", time.Now(), Lane{})
	if err != nil || job == nil {
		t.Fatalf("claim 2 = (%+v, %v)", job, err)
	}
	m.runJob("w0", job)
	st = jqState(t, db, id)
	if st.Status != StatusFailed || st.Attempts != 2 || st.FinishedAt == nil {
		t.Fatalf("after attempt 2 = %+v, want failed/attempts=2/finished", st)
	}
}

// TestJobsReaper_Integration: a stale 'running' job (lease expired) with budget left goes back to
// 'queued' and is claimable again; one at its attempt budget is terminally failed; a FRESH running
// job is untouched.
func TestJobsReaper_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()

	seed := func(id string, attempts, maxAttempts int, claimedAgo time.Duration) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO sys_jobs (id, job_type, status, run_at, attempts, max_attempts, claimed_at, claimed_by)
			VALUES (?, 'noop', 'running', NOW(3) - INTERVAL 1 HOUR, ?, ?, ?, 'dead-worker')`,
			id, attempts, maxAttempts, time.Now().UTC().Add(-claimedAgo)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("stale-retryable", 1, 5, 10*time.Minute)
	seed("stale-exhausted", 5, 5, 10*time.Minute)
	seed("fresh-running", 1, 5, 10*time.Second)

	requeued, failed, err := store.ReapStuck(ctx, time.Now().Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if requeued != 1 || failed != 1 {
		t.Fatalf("reap = (requeued=%d, failed=%d), want (1, 1)", requeued, failed)
	}

	st := jqState(t, db, "stale-retryable")
	if st.Status != StatusQueued || st.ClaimedBy != nil || st.ClaimedAt != nil {
		t.Fatalf("stale-retryable = %+v, want queued with claim cleared", st)
	}
	if st.LastError == nil || !strings.Contains(*st.LastError, "lease expired") {
		t.Fatalf("stale-retryable last_error = %v, want the reap marker", st.LastError)
	}
	if st = jqState(t, db, "stale-exhausted"); st.Status != StatusFailed || st.FinishedAt == nil {
		t.Fatalf("stale-exhausted = %+v, want terminal failed", st)
	}
	if st = jqState(t, db, "fresh-running"); st.Status != StatusRunning {
		t.Fatalf("fresh-running = %+v, must be untouched", st)
	}

	// The reclaimed job is claimable again — and burns another attempt on claim.
	job, err := store.ClaimNext(ctx, "worker-b", time.Now(), Lane{})
	if err != nil || job == nil || job.ID != "stale-retryable" || job.Attempts != 2 {
		t.Fatalf("reclaim = (%+v, %v), want stale-retryable at attempts=2", job, err)
	}
}

// TestJobsUnknownType_Integration: a claimed job with no registered handler is terminally failed
// on the spot (never requeued — an unregistered type would otherwise retry forever).
func TestJobsUnknownType_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()

	id, err := store.Enqueue(ctx, Job{Type: "ghost"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	m := NewManager(ManagerConfig{Workers: 1, PollInterval: time.Second, Lease: time.Minute}, store, NewRegistry(), nil)
	job, err := store.ClaimNext(ctx, "w0", time.Now(), Lane{})
	if err != nil || job == nil {
		t.Fatalf("claim = (%+v, %v)", job, err)
	}
	m.runJob("w0", job)

	st := jqState(t, db, id)
	if st.Status != StatusFailed || st.FinishedAt == nil {
		t.Fatalf("unknown type = %+v, want terminal failed", st)
	}
	if st.LastError == nil || !strings.Contains(*st.LastError, "no handler registered") {
		t.Fatalf("last_error = %v, want the unregistered-type message", st.LastError)
	}
}

// TestJobsManagerEndToEnd_Integration runs the REAL worker pool against live MySQL: a
// fail-once-then-succeed handler (the SMTP-blip shape) ends done with attempts=2, and a
// plain job is processed exactly once. Start/Stop bracket the whole cycle under -race.
func TestJobsManagerEndToEnd_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()

	var flakyRuns, steadyRuns int32
	reg := NewRegistry()
	if err := reg.Register("flaky-once", func(context.Context, Job) error {
		if atomic.AddInt32(&flakyRuns, 1) == 1 {
			return fmt.Errorf("smtp blip")
		}
		return nil
	}); err != nil {
		t.Fatalf("register flaky-once: %v", err)
	}
	if err := reg.Register("steady", func(context.Context, Job) error {
		atomic.AddInt32(&steadyRuns, 1)
		return nil
	}); err != nil {
		t.Fatalf("register steady: %v", err)
	}

	m := NewManager(ManagerConfig{Workers: 2, PollInterval: time.Second, Lease: time.Minute}, store, reg, nil)
	m.backoffBase = 50 * time.Millisecond // keep the retry inside the test budget
	m.backoffCap = time.Second

	flakyID, err := store.Enqueue(ctx, Job{Type: "flaky-once"})
	if err != nil {
		t.Fatalf("enqueue flaky: %v", err)
	}
	steadyID, err := store.Enqueue(ctx, Job{Type: "steady"})
	if err != nil {
		t.Fatalf("enqueue steady: %v", err)
	}

	m.Start()
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		flaky := jqState(t, db, flakyID)
		steady := jqState(t, db, steadyID)
		if flaky.Status == StatusDone && steady.Status == StatusDone {
			if flaky.Attempts != 2 {
				t.Fatalf("flaky attempts = %d, want 2 (one blip + one success)", flaky.Attempts)
			}
			if got := atomic.LoadInt32(&steadyRuns); got != 1 {
				t.Fatalf("steady handler ran %d times, want 1", got)
			}
			return
		}
		if flaky.Status == StatusFailed || steady.Status == StatusFailed {
			t.Fatalf("unexpected terminal failure: flaky=%+v steady=%+v", flaky, steady)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool did not finish: flaky=%+v steady=%+v", flaky, steady)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestJobsHasJobWithPayload_Integration pins the TIMER-1 dedup probe's answers. The probe narrows
// by payload_hash (queue #83) and keeps `payload = CAST(? AS JSON)` as the exact check, so a hash
// computed differently on the two sides (charset, serialization) shows up here as a false "not
// found" — the timer would be parked twice. The Vietnamese text is the charset guard: under a
// latin1 cast on either side it hashes differently from the stored utf8mb4 row.
func TestJobsHasJobWithPayload_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	const tenant, other = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"

	id, err := store.Enqueue(ctx, Job{Type: "workflow_timer", TenantID: tenant,
		Payload: []byte(`{"recordId":"r-1","note":"Hạn xử lý: Nguyễn Văn Á","occurrence":3}`)})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	setStatus := func(s string) {
		t.Helper()
		if _, err := db.Exec(`UPDATE sys_jobs SET status = ? WHERE id = ?`, s, id); err != nil {
			t.Fatalf("set status %s: %v", s, err)
		}
	}

	sameReordered := `{ "occurrence": 3,  "note": "Hạn xử lý: Nguyễn Văn Á", "recordId": "r-1" }`
	cases := []struct {
		name, status, jobType, tenant, payload string
		want                                   bool
	}{
		{"same payload, other key order and whitespace", StatusQueued, "workflow_timer", tenant, sameReordered, true},
		{"running still counts", StatusRunning, "workflow_timer", tenant, sameReordered, true},
		{"done counts: the wake-up already fired", StatusDone, "workflow_timer", tenant, sameReordered, true},
		{"failed does not: it may be re-parked", StatusFailed, "workflow_timer", tenant, sameReordered, false},
		{"different value", StatusQueued, "workflow_timer", tenant, `{"recordId":"r-1","note":"Hạn xử lý: Nguyễn Văn A","occurrence":3}`, false},
		{"other tenant", StatusQueued, "workflow_timer", other, sameReordered, false},
		{"other job type", StatusQueued, "noop", tenant, sameReordered, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setStatus(c.status)
			got, err := store.HasJobWithPayload(ctx, c.jobType, c.tenant, []byte(c.payload))
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if got != c.want {
				t.Fatalf("HasJobWithPayload(%s, %s, %s) with row %s = %v, want %v", c.jobType, c.tenant, c.payload, c.status, got, c.want)
			}
		})
	}
}

// TestJobsDedupProbeUsesHashIndex_Integration pins queue #83: the dedup probe seeks
// idx_sys_jobs_dedup (tenant_id, job_type, payload_hash) instead of walking every row of the
// tenant. The index shape is checked first; then the probe's own SQL is EXPLAINed over a tenant
// holding enough rows of one job type that riding idx_sys_jobs_tenant would be a real scan. The
// before/after EXPLAIN ANALYZE on 75k rows lives in the *_sys_jobs_dedup_index migration header.
func TestJobsDedupProbeUsesHashIndex_Integration(t *testing.T) {
	db := jqSetup(t)
	var cols []string
	if err := db.Select(&cols, `
		SELECT COLUMN_NAME FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_jobs' AND INDEX_NAME = 'idx_sys_jobs_dedup'
		ORDER BY SEQ_IN_INDEX`); err != nil {
		t.Fatalf("read index columns: %v", err)
	}
	if got, want := strings.Join(cols, ","), "tenant_id,job_type,payload_hash"; got != want {
		t.Fatalf("idx_sys_jobs_dedup columns = %q, want %q", got, want)
	}

	store := NewSQLStore(db, 5)
	ctx := context.Background()
	const tenant = "11111111-1111-1111-1111-111111111111"
	for i := 0; i < 40; i++ {
		if _, err := store.Enqueue(ctx, Job{Type: "workflow_timer", TenantID: tenant,
			Payload: []byte(fmt.Sprintf(`{"recordId":"r-%d"}`, i))}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if _, err := db.Exec(`ANALYZE TABLE sys_jobs`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	probe := `{"recordId":"r-7"}`
	var plan string
	if err := db.Get(&plan, "EXPLAIN FORMAT=TREE "+dedupProbeSQL,
		tenant, "workflow_timer", probe, StatusQueued, StatusRunning, StatusDone, probe); err != nil {
		t.Fatalf("explain probe: %v", err)
	}
	// The seek must bind ALL THREE key parts. A probe without the payload_hash predicate still
	// rides this index — through its (tenant_id, job_type) prefix, i.e. every timer row of the
	// tenant — so the index name alone would pass a probe that scans.
	if !strings.Contains(plan, "using idx_sys_jobs_dedup (") || !strings.Contains(plan, "payload_hash=") {
		t.Fatalf("dedup probe does not seek idx_sys_jobs_dedup on payload_hash (a tenant-wide scan per ticket write):\n%s", plan)
	}
}

// TestJobsScrubPayloadOnFinish_Integration pins queue #95 (email-job-payload-pii-retention): a job
// type registered with ScrubPayloadOnFinish keeps its payload exactly as long as a delivery can
// still need it — through every retry — and loses it on EVERY terminal transition (done, failed,
// reaper-failed). Every other job type keeps its payload: workflow_timer's dedup probe reads the
// payload of its DONE rows, so a blanket scrub would re-fire timers.
func TestJobsScrubPayloadOnFinish_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	if err := store.ScrubPayloadOnFinish("email_alert"); err != nil {
		t.Fatalf("ScrubPayloadOnFinish: %v", err)
	}
	ctx := context.Background()
	const secret = `{"to":"a.canhan@gmail.com","subject":"Chào Lan","body":"b"}`

	payloadOf := func(id string) *string {
		t.Helper()
		var p *string
		if err := db.Get(&p, `SELECT CAST(payload AS CHAR) FROM sys_jobs WHERE id = ?`, id); err != nil {
			t.Fatalf("read payload %s: %v", id, err)
		}
		return p
	}
	enqueueAndClaim := func(jobType string, maxAttempts int) string {
		t.Helper()
		id, err := store.Enqueue(ctx, Job{Type: jobType, Payload: []byte(secret), MaxAttempts: maxAttempts})
		if err != nil {
			t.Fatalf("enqueue %s: %v", jobType, err)
		}
		job, err := store.ClaimNext(ctx, "w0", time.Now(), Lane{})
		if err != nil || job == nil || job.ID != id {
			t.Fatalf("claim %s = (%+v, %v), want %s", jobType, job, err, id)
		}
		return id
	}

	t.Run("done scrubs the payload", func(t *testing.T) {
		id := enqueueAndClaim("email_alert", 5)
		if applied, err := store.MarkDone(ctx, id, "w0", time.Now()); err != nil || !applied {
			t.Fatalf("MarkDone = (%v, %v)", applied, err)
		}
		if p := payloadOf(id); p != nil {
			t.Fatalf("payload after done = %s, want NULL", *p)
		}
		if st := jqState(t, db, id); st.Status != StatusDone || st.FinishedAt == nil {
			t.Fatalf("row after done = %+v, want done/finished", st)
		}
	})

	t.Run("a retry keeps the payload, the terminal failure scrubs it", func(t *testing.T) {
		id := enqueueAndClaim("email_alert", 2)
		if applied, err := store.Reschedule(ctx, id, "w0", time.Now().Add(-time.Second), "550 [redacted]"); err != nil || !applied {
			t.Fatalf("Reschedule = (%v, %v)", applied, err)
		}
		if p := payloadOf(id); p == nil || !strings.Contains(*p, "a.canhan@gmail.com") {
			t.Fatalf("payload after reschedule = %v, want kept (the retry must still send)", p)
		}
		job, err := store.ClaimNext(ctx, "w0", time.Now(), Lane{})
		if err != nil || job == nil || job.ID != id || !strings.Contains(string(job.Payload), "a.canhan@gmail.com") {
			t.Fatalf("reclaim = (%+v, %v), want %s with its payload", job, err, id)
		}
		if applied, err := store.MarkFailed(ctx, id, "w0", "550 [redacted]", time.Now()); err != nil || !applied {
			t.Fatalf("MarkFailed = (%v, %v)", applied, err)
		}
		if p := payloadOf(id); p != nil {
			t.Fatalf("payload after failed = %s, want NULL", *p)
		}
		if st := jqState(t, db, id); st.Status != StatusFailed || st.LastError == nil || *st.LastError != "550 [redacted]" {
			t.Fatalf("row after failed = %+v, want failed with its error kept", st)
		}
	})

	t.Run("a lost claim scrubs nothing", func(t *testing.T) {
		id := enqueueAndClaim("email_alert", 5)
		if applied, err := store.MarkDone(ctx, id, "someone-else", time.Now()); err != nil || applied {
			t.Fatalf("MarkDone by a stale owner = (%v, %v), want applied=false", applied, err)
		}
		if p := payloadOf(id); p == nil {
			t.Fatal("a MarkDone that did not apply must not scrub the live job's payload")
		}
		if _, err := store.MarkDone(ctx, id, "w0", time.Now()); err != nil {
			t.Fatalf("cleanup MarkDone: %v", err)
		}
	})

	t.Run("other job types keep their payload when done", func(t *testing.T) {
		id := enqueueAndClaim("workflow_timer", 5)
		if applied, err := store.MarkDone(ctx, id, "w0", time.Now()); err != nil || !applied {
			t.Fatalf("MarkDone = (%v, %v)", applied, err)
		}
		if p := payloadOf(id); p == nil {
			t.Fatal("workflow_timer payload scrubbed at done — its dedup probe reads done rows")
		}
		// Kept means kept AS JSON: the IF(...) in the scrub-aware UPDATE must not turn the column
		// into a JSON string, or the timer dedup probe stops matching its done row (re-fire bug).
		const tenant = "11111111-1111-1111-1111-111111111111"
		tid, err := store.Enqueue(ctx, Job{Type: "workflow_timer", TenantID: tenant, Payload: []byte(secret)})
		if err != nil {
			t.Fatalf("enqueue tenant timer: %v", err)
		}
		if job, err := store.ClaimNext(ctx, "w0", time.Now(), Lane{}); err != nil || job == nil || job.ID != tid {
			t.Fatalf("claim tenant timer = (%+v, %v)", job, err)
		}
		if applied, err := store.MarkDone(ctx, tid, "w0", time.Now()); err != nil || !applied {
			t.Fatalf("MarkDone tenant timer = (%v, %v)", applied, err)
		}
		var jsonType string
		if err := db.Get(&jsonType, `SELECT JSON_TYPE(payload) FROM sys_jobs WHERE id = ?`, tid); err != nil || jsonType != "OBJECT" {
			t.Fatalf("JSON_TYPE(payload) after done = %q (%v), want OBJECT", jsonType, err)
		}
		if dup, err := store.HasJobWithPayload(ctx, "workflow_timer", tenant, []byte(secret)); err != nil || !dup {
			t.Fatalf("dedup probe after done = (%v, %v), want true — a kept payload must still match", dup, err)
		}
	})

	t.Run("the reaper's terminal failure scrubs, its requeue does not", func(t *testing.T) {
		seed := func(id, jobType string, attempts int) {
			t.Helper()
			if _, err := db.Exec(`INSERT INTO sys_jobs (id, job_type, payload, status, run_at, attempts, max_attempts, claimed_at, claimed_by)
				VALUES (?, ?, CAST(? AS JSON), 'running', NOW(3) - INTERVAL 1 HOUR, ?, 5, ?, 'dead-worker')`,
				id, jobType, secret, attempts, time.Now().UTC().Add(-10*time.Minute)); err != nil {
				t.Fatalf("seed %s: %v", id, err)
			}
		}
		seed("mail-exhausted", "email_alert", 5)
		seed("mail-retryable", "email_alert", 1)
		seed("noop-exhausted", "noop", 5)
		if _, _, err := store.ReapStuck(ctx, time.Now().Add(-5*time.Minute)); err != nil {
			t.Fatalf("reap: %v", err)
		}
		if p := payloadOf("mail-exhausted"); p != nil {
			t.Fatalf("reaper-failed email_alert payload = %s, want NULL", *p)
		}
		if p := payloadOf("mail-retryable"); p == nil {
			t.Fatal("reaper-requeued email_alert lost its payload — the retry could not send")
		}
		if p := payloadOf("noop-exhausted"); p == nil {
			t.Fatal("reaper-failed noop lost its payload — only registered types are scrubbed")
		}
	})

	t.Run("invalid job types are refused", func(t *testing.T) {
		for _, bad := range []string{"", "a,b"} {
			if err := NewSQLStore(db, 5).ScrubPayloadOnFinish(bad); err == nil {
				t.Fatalf("ScrubPayloadOnFinish(%q) = nil, want an error", bad)
			}
		}
	})
}

// TestJobsClaimNextHonoursLane_Integration (review of #26): a dedicated lane claims ONLY its type,
// and the shared lane never claims an excluded type — even when that type holds the oldest due row
// (FIFO would otherwise hand the shared pool the slow job first).
func TestJobsClaimNextHonoursLane_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)
	hook, err := store.Enqueue(ctx, Job{Type: "outbound_webhook", TenantID: "11111111-1111-1111-1111-111111111111", RunAt: past})
	if err != nil {
		t.Fatalf("enqueue hook: %v", err)
	}
	mail, err := store.Enqueue(ctx, Job{Type: "email_alert", RunAt: past.Add(time.Second)})
	if err != nil {
		t.Fatalf("enqueue mail: %v", err)
	}
	shared := Lane{Exclude: []string{"outbound_webhook"}}
	webhooks := Lane{Only: "outbound_webhook"}

	got, err := store.ClaimNext(ctx, "shared-w0", time.Now(), shared)
	if err != nil || got == nil || got.ID != mail {
		t.Fatalf("shared lane claimed %+v (err %v), want the email job %s — never the older webhook", got, err, mail)
	}
	if next, err := store.ClaimNext(ctx, "shared-w1", time.Now(), shared); err != nil || next != nil {
		t.Fatalf("shared lane claimed %+v (err %v) with only a webhook left, want nothing", next, err)
	}
	got, err = store.ClaimNext(ctx, "hook-w0", time.Now(), webhooks)
	if err != nil || got == nil || got.ID != hook {
		t.Fatalf("webhook lane claimed %+v (err %v), want the webhook job %s", got, err, hook)
	}
	if next, err := store.ClaimNext(ctx, "hook-w1", time.Now(), webhooks); err != nil || next != nil {
		t.Fatalf("webhook lane claimed %+v (err %v) with nothing of its type left, want nothing", next, err)
	}
}

// TestJobsLaneClaimsSeek_Integration: both lane claims stop at the first row they may take. The
// dedicated lane seeks idx_sys_jobs_lane (status, job_type, run_at) — riding (status, run_at) it
// would walk and row-lock every due row of OTHER types ahead of its own, and a SKIP LOCKED claim
// racing it on the shared lane would then find nothing. The shared lane keeps idx_sys_jobs_claim,
// whose order is the ORDER BY: an index + filesort there would lock every queued row of its types,
// parked timers included. Neither plan may sort.
func TestJobsLaneClaimsSeek_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		typ := []string{"email_alert", "workflow_timer", "outbound_webhook"}[i%3]
		runAt := time.Now().Add(time.Duration(i-30) * time.Minute) // half due, half parked in the future
		if _, err := store.Enqueue(ctx, Job{Type: typ, TenantID: "11111111-1111-1111-1111-111111111111", RunAt: runAt}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if _, err := db.Exec(`ANALYZE TABLE sys_jobs`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	for _, tc := range []struct {
		name  string
		lane  Lane
		index string
	}{
		{"dedicated", Lane{Only: "outbound_webhook"}, "idx_sys_jobs_lane"},
		{"shared", Lane{Exclude: []string{"outbound_webhook"}}, "idx_sys_jobs_claim"},
	} {
		query, args := claimSelect(tc.lane, time.Now())
		var plan string
		if err := db.Get(&plan, "EXPLAIN FORMAT=TREE "+query, args...); err != nil {
			t.Fatalf("%s: explain: %v", tc.name, err)
		}
		t.Logf("%s lane plan:\n%s", tc.name, plan)
		if !strings.Contains(plan, "using "+tc.index) || strings.Contains(strings.ToLower(plan), "sort") {
			t.Fatalf("%s lane claim does not ride %s in order:\n%s", tc.name, tc.index, plan)
		}
	}
}

// TestJobsManagerDedicatedLane_Integration: the review's case (a) over the real store — a flood of
// webhook jobs whose receiver never answers holds every worker of their lane, and a mail job
// enqueued AFTER the lane is saturated still runs at once on the shared pool (in one FIFO pool it
// would wait behind the whole flood), while the shared pool never takes a webhook.
func TestJobsManagerDedicatedLane_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	release := make(chan struct{})
	reg := NewRegistry()
	if err := reg.Register("outbound_webhook", func(ctx context.Context, _ Job) error {
		select { // a receiver that swallows packets: the attempt waits out its timeout
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}); err != nil {
		t.Fatalf("register webhook: %v", err)
	}
	if err := reg.Register("email_alert", func(context.Context, Job) error { return nil }); err != nil {
		t.Fatalf("register mail: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := store.Enqueue(ctx, Job{Type: "outbound_webhook", TenantID: "11111111-1111-1111-1111-111111111111",
			RunAt: time.Now().Add(-time.Minute)}); err != nil {
			t.Fatalf("enqueue webhook %d: %v", i, err)
		}
	}
	runningWebhooks := func() int {
		t.Helper()
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM sys_jobs WHERE job_type = 'outbound_webhook' AND status = ?`, StatusRunning); err != nil {
			t.Fatalf("count running webhooks: %v", err)
		}
		return n
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (running webhooks = %d)", what, runningWebhooks())
			}
			time.Sleep(25 * time.Millisecond)
		}
	}

	m := NewManager(ManagerConfig{Workers: 2, PollInterval: time.Second, Lease: time.Minute,
		Dedicated: map[string]int{"outbound_webhook": 2}}, store, reg, nil)
	m.Start()
	defer func() {
		close(release)
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	waitFor("the webhook lane to saturate", func() bool { return runningWebhooks() >= 2 })
	mailID, err := store.Enqueue(ctx, Job{Type: "email_alert"})
	if err != nil {
		t.Fatalf("enqueue mail: %v", err)
	}
	waitFor("the mail job to run on the shared pool", func() bool { return jqState(t, db, mailID).Status == StatusDone })
	if n := runningWebhooks(); n != 2 {
		t.Fatalf("%d webhook jobs running, want exactly the 2 of their lane (the shared pool must never take one)", n)
	}
}

// TestJobsHasPendingForWorkflow_Integration pins the scheduled-webhook back-pressure probe (review
// of #26): only a QUEUED or RUNNING job of the same type, tenant and rule counts — a delivered
// (done) or dead-lettered (failed) one must not hold the rule, or it would never fire again.
func TestJobsHasPendingForWorkflow_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	const tenant, other = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	id, err := store.Enqueue(ctx, Job{Type: "outbound_webhook", TenantID: tenant,
		Payload: []byte(`{"workflowId":"wf-1","actionIndex":0,"body":"{\"workflowId\":\"wf-2\"}"}`)})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	cases := []struct {
		name, status, jobType, tenant, workflow string
		want                                    bool
	}{
		{"queued counts", StatusQueued, "outbound_webhook", tenant, "wf-1", true},
		{"running counts", StatusRunning, "outbound_webhook", tenant, "wf-1", true},
		{"done does not: delivered", StatusDone, "outbound_webhook", tenant, "wf-1", false},
		{"failed does not: dead-lettered", StatusFailed, "outbound_webhook", tenant, "wf-1", false},
		{"other rule (even one named inside the body)", StatusQueued, "outbound_webhook", tenant, "wf-2", false},
		{"other tenant", StatusQueued, "outbound_webhook", other, "wf-1", false},
		{"other job type", StatusQueued, "workflow_timer", tenant, "wf-1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := db.Exec(`UPDATE sys_jobs SET status = ? WHERE id = ?`, c.status, id); err != nil {
				t.Fatalf("set status: %v", err)
			}
			got, err := store.HasPendingForWorkflow(ctx, c.jobType, c.tenant, c.workflow)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if got != c.want {
				t.Fatalf("HasPendingForWorkflow(%s, %s, %s) with row %s = %v, want %v", c.jobType, c.tenant, c.workflow, c.status, got, c.want)
			}
		})
	}
	if _, err := store.HasPendingForWorkflow(ctx, "outbound_webhook", "", "wf-1"); err == nil {
		t.Fatal("a probe without a tenant must be refused")
	}
}

// TestJobsFailedBySlot_Integration pins Doctor C10's warning count (specs/050 S3b review): per action
// slot, the FAILED jobs of the type, tenant and rule that name no vault key and still hold a payload —
// exactly the rows "Gửi lại" (RequeueFinished) could send and the job handler drops once the slot names
// a key. Queued/done rows, rows naming a key, scrubbed rows and other tenants/rules/types do not count.
func TestJobsFailedBySlot_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	const tenant, other = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	park := func(jobType, tenantID, payload, status string) string {
		t.Helper()
		id, err := store.Enqueue(ctx, Job{Type: jobType, TenantID: tenantID, Payload: []byte(payload)})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if _, err := db.Exec(`UPDATE sys_jobs SET status = ? WHERE id = ?`, status, id); err != nil {
			t.Fatalf("set status: %v", err)
		}
		return id
	}
	park("outbound_webhook", tenant, `{"workflowId":"wf-1","actionIndex":1,"body":"a"}`, StatusFailed)
	park("outbound_webhook", tenant, `{"workflowId":"wf-1","actionIndex":1,"credentialId":"","body":"b"}`, StatusFailed)
	park("outbound_webhook", tenant, `{"workflowId":"wf-1","actionIndex":0,"body":"c"}`, StatusFailed)
	// None of these count.
	park("outbound_webhook", tenant, `{"workflowId":"wf-1","actionIndex":1,"credentialId":"cred-1","body":"d"}`, StatusFailed)
	park("outbound_webhook", tenant, `{"workflowId":"wf-1","actionIndex":1,"body":"e"}`, StatusQueued)
	park("outbound_webhook", tenant, `{"workflowId":"wf-1","actionIndex":1,"body":"f"}`, StatusDone)
	park("outbound_webhook", other, `{"workflowId":"wf-1","actionIndex":1,"body":"g"}`, StatusFailed)
	park("outbound_webhook", tenant, `{"workflowId":"wf-2","actionIndex":1,"body":"h"}`, StatusFailed)
	park("workflow_timer", tenant, `{"workflowId":"wf-1","actionIndex":1}`, StatusFailed)
	scrubbed := park("outbound_webhook", tenant, `{"workflowId":"wf-1","actionIndex":1,"body":"i"}`, StatusFailed)
	if _, err := db.Exec(`UPDATE sys_jobs SET payload = NULL WHERE id = ?`, scrubbed); err != nil {
		t.Fatalf("scrub payload: %v", err)
	}

	got, err := store.FailedBySlot(ctx, "outbound_webhook", tenant, "wf-1")
	if err != nil {
		t.Fatalf("FailedBySlot: %v", err)
	}
	if len(got) != 2 || got[1] != 2 || got[0] != 1 {
		t.Fatalf("FailedBySlot = %v; want map[0:1 1:2]", got)
	}
	if _, err := store.FailedBySlot(ctx, "outbound_webhook", "", "wf-1"); err == nil {
		t.Fatal("a count without a tenant must be refused")
	}
}

// TestJobsPendingForWorkflowProbeSeeksLane_Integration: the probe runs once per tick per scheduled
// webhook rule and must read only the PENDING rows of ITS type — through idx_sys_jobs_lane — not
// the tenant's delivered history (done rows are kept JOBS_RETENTION_DAYS) and not every other
// pending row either (parked SLA timers sit queued for days).
func TestJobsPendingForWorkflowProbeSeeksLane_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	const tenant = "11111111-1111-1111-1111-111111111111"
	for i := 0; i < 80; i++ {
		id, err := store.Enqueue(ctx, Job{Type: "outbound_webhook", TenantID: tenant,
			Payload: []byte(fmt.Sprintf(`{"workflowId":"wf-%d","actionIndex":0,"body":"{}"}`, i%4))})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if i >= 3 { // the history: delivered long ago
			if _, err := db.Exec(`UPDATE sys_jobs SET status = ?, finished_at = NOW(3) WHERE id = ?`, StatusDone, id); err != nil {
				t.Fatalf("mark done: %v", err)
			}
		}
	}
	for i := 0; i < 60; i++ { // parked timers: queued, due days from now
		if _, err := store.Enqueue(ctx, Job{Type: "workflow_timer", TenantID: tenant, RunAt: time.Now().Add(72 * time.Hour),
			Payload: []byte(fmt.Sprintf(`{"workflowId":"wf-t","recordId":"r-%d"}`, i))}); err != nil {
			t.Fatalf("enqueue timer %d: %v", i, err)
		}
	}
	if _, err := db.Exec(`ANALYZE TABLE sys_jobs`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	var plan string
	if err := db.Get(&plan, "EXPLAIN FORMAT=TREE "+pendingForWorkflowSQL,
		StatusQueued, StatusRunning, "outbound_webhook", tenant, "wf-1"); err != nil {
		t.Fatalf("explain: %v", err)
	}
	t.Logf("probe plan:\n%s", plan)
	if !strings.Contains(plan, "using idx_sys_jobs_lane") {
		t.Fatalf("pending probe does not seek idx_sys_jobs_lane (it would read the tenant's delivered history):\n%s", plan)
	}
}

// TestJobsPendingProbeSurvivesMissingIndexes_Integration: code deployed before its migration must
// keep working. The probe and FailedBySlot name idx_sys_jobs_tenant_list (000075) in an optimizer
// hint; with the index dropped they must still run (hint = warning), where IGNORE INDEX would fail
// with ERROR 1176.
func TestJobsPendingProbeSurvivesMissingIndexes_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	const tenant = "11111111-1111-1111-1111-111111111111"
	if _, err := store.Enqueue(ctx, Job{Type: "outbound_webhook", TenantID: tenant,
		Payload: []byte(`{"workflowId":"wf-1","actionIndex":0,"body":"{}"}`)}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE sys_jobs DROP INDEX idx_sys_jobs_tenant_list`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	got, err := store.HasPendingForWorkflow(ctx, "outbound_webhook", tenant, "wf-1")
	if err != nil || !got {
		t.Fatalf("HasPendingForWorkflow on a schema without 000075 = %v, %v; want true, nil", got, err)
	}
	if _, err := store.FailedBySlot(ctx, "outbound_webhook", tenant, "wf-1"); err != nil {
		t.Fatalf("FailedBySlot on a schema without 000075: %v", err)
	}
}

// specs/068 T036 (real InnoDB): a frozen tenant's job claimed then postponed ends queued, run_at at the
// freeze deadline, and attempts EQUAL to what they were before the claim; a Reschedule in its place
// would leave attempts one higher (and drain the budget a whole maintenance window at a time).
func TestJobsPostponeGivesTheAttemptBack_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()

	id, err := store.Enqueue(ctx, Job{Type: "t", TenantID: "ten-frozen", MaxAttempts: 3})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, err := store.ClaimNext(ctx, "w0", time.Now(), Lane{})
	if err != nil || job == nil {
		t.Fatalf("claim = (%+v, %v)", job, err)
	}
	if st := jqState(t, db, id); st.Attempts != 1 {
		t.Fatalf("claim must add an attempt, got %+v", st)
	}
	runAt := time.Now().Add(10 * time.Minute)
	applied, err := store.Postpone(ctx, id, "w0", runAt)
	if err != nil || !applied {
		t.Fatalf("Postpone = (%v, %v)", applied, err)
	}
	st := jqState(t, db, id)
	if st.Status != StatusQueued || st.Attempts != 0 {
		t.Fatalf("after Postpone = %+v, want queued/attempts=0", st)
	}
	if d := st.RunAt.Sub(runAt); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("run_at = %s, want ~%s", st.RunAt, runAt)
	}
	if early, err := store.ClaimNext(ctx, "w0", time.Now(), Lane{}); err != nil || early != nil {
		t.Fatalf("postponed job claimable early: (%+v, %v)", early, err)
	}
	// Lost claim: a second Postpone by a worker that no longer owns the row is a no-op.
	if applied, err := store.Postpone(ctx, id, "w0", runAt); err != nil || applied {
		t.Fatalf("second Postpone = (%v, %v), want not applied", applied, err)
	}
}
