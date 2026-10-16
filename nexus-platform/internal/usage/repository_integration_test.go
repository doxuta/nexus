//go:build integration

package usage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const (
	tenA = "aaaaaaaa-0000-0000-0000-00000000000a"
	tenB = "bbbbbbbb-0000-0000-0000-00000000000b"
)

func usageDB(t *testing.T) (*sqlx.DB, *Repository) {
	t.Helper()
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("usage: MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewRepository(db)
}

func must(t *testing.T, db *sqlx.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// Day edges of 2026-10-14 in Asia/Ho_Chi_Minh: [2026-10-13T17:00Z, 2026-10-14T17:00Z).
var (
	dayFrom = time.Date(2026, 10, 13, 17, 0, 0, 0, time.UTC)
	dayTo   = time.Date(2026, 10, 14, 17, 0, 0, 0, time.UTC)
)

func seedRun(t *testing.T, db *sqlx.DB, tenant, status string, at time.Time) {
	t.Helper()
	must(t, db, `INSERT INTO sys_workflow_runs (id, tenant_id, workflow_id, trigger_type, status, started_at, finished_at)
		VALUES (?, ?, 'wf', 'after_create', ?, ?, ?)`, uuid.NewString(), tenant, status, at, at)
}

func seedMail(t *testing.T, db *sqlx.DB, tenant, source, status string, at time.Time) {
	t.Helper()
	must(t, db, `INSERT INTO sys_email_log (id, tenant_id, source, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		uuid.NewString(), tenant, source, status, at)
}

func seedJob(t *testing.T, db *sqlx.DB, tenant any, jobType, status string, at time.Time) {
	t.Helper()
	must(t, db, `INSERT INTO sys_jobs (id, tenant_id, job_type, status, run_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), tenant, jobType, status, at, at)
}

func seedHook(t *testing.T, db *sqlx.DB, tenant, status string, at time.Time) {
	t.Helper()
	must(t, db, `INSERT INTO sys_hook_deliveries (id, tenant_id, hook_id, status, created_at) VALUES (?, ?, 'h', ?, ?)`,
		uuid.NewString(), tenant, status, at)
}

func TestCountersForDay_Integration(t *testing.T) {
	db, r := usageDB(t)
	ctx := context.Background()
	edgeIn := dayFrom                    // 17:00:00Z: first instant of the VN day
	edgeOut := dayFrom.Add(-time.Second) // 16:59:59Z: still the day before
	last := dayTo.Add(-time.Second)      // 16:59:59Z: last second of the VN day
	next := dayTo                        // 17:00:00Z: already the next VN day

	for _, at := range []time.Time{edgeIn, last, edgeOut, next} {
		seedRun(t, db, tenA, "success", at)
		seedMail(t, db, tenA, "workflow", "sent", at)
		seedJob(t, db, tenA, "outbound_webhook", "done", at)
		seedHook(t, db, tenA, "created", at)
	}
	seedRun(t, db, tenA, "failed", edgeIn)
	seedRun(t, db, tenA, "blocked", edgeIn)
	seedRun(t, db, tenA, "skipped", edgeIn)
	seedMail(t, db, tenA, "password_reset", "failed", edgeIn)
	seedMail(t, db, tenA, "workflow", "queued", edgeIn)
	seedJob(t, db, tenA, "outbound_webhook", "failed", edgeIn)
	seedJob(t, db, tenA, "email_alert", "done", edgeIn)
	seedJob(t, db, nil, "outbound_webhook", "done", edgeIn) // system job: no tenant
	seedHook(t, db, tenA, "updated", edgeIn)
	seedHook(t, db, tenA, "error", edgeIn)
	// another tenant never leaks in
	seedRun(t, db, tenB, "success", edgeIn)
	seedMail(t, db, tenB, "workflow", "sent", edgeIn)
	seedJob(t, db, tenB, "outbound_webhook", "done", edgeIn)
	seedHook(t, db, tenB, "created", edgeIn)

	got, err := r.CountersForDay(ctx, tenA, dayFrom, dayTo)
	if err != nil {
		t.Fatalf("CountersForDay: %v", err)
	}
	want := map[Key]int64{
		{WorkflowRuns, ""}:               4, // 2 edge success + failed + blocked; skipped not counted
		{WorkflowRunsFailed, ""}:         1,
		{EmailsSent, "workflow"}:         2,
		{EmailsFailed, "password_reset"}: 1,
		{WebhooksDelivered, ""}:          2,
		{WebhooksFailed, ""}:             1,
		{HooksReceived, ""}:              3,
		{HooksFailed, ""}:                1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%v = %d; want %d (all: %v)", k, got[k], v, got)
		}
	}
	// the next / previous day sees its own edge rows
	prev, err := r.CountersForDay(ctx, tenA, dayFrom.Add(-24*time.Hour), dayFrom)
	if err != nil {
		t.Fatal(err)
	}
	if prev[Key{WorkflowRuns, ""}] != 1 || prev[Key{HooksReceived, ""}] != 1 || prev[Key{WebhooksDelivered, ""}] != 1 {
		t.Errorf("previous day = %v; want the single 16:59:59Z row of each source", prev)
	}
	// an empty tenant still gets every metric, zero
	empty, err := r.CountersForDay(ctx, "cccccccc-0000-0000-0000-00000000000c", dayFrom, dayTo)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range Metrics {
		dims := []string{""}
		if m == EmailsSent || m == EmailsFailed {
			dims = EmailSources
		}
		for _, d := range dims {
			if v, ok := empty[Key{m, d}]; !ok || v != 0 {
				t.Errorf("empty tenant %s/%s = %v,%v; want 0 present", m, d, v, ok)
			}
		}
	}
}

// explain returns the "key" column of every plan row of the query.
func explainKeys(t *testing.T, db *sqlx.DB, q string, args ...any) string {
	t.Helper()
	rows, err := db.Queryx("EXPLAIN "+q, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		m := map[string]any{}
		if err := rows.MapScan(m); err != nil {
			t.Fatal(err)
		}
		if k, ok := m["key"].([]byte); ok {
			keys = append(keys, string(k))
		}
	}
	return strings.Join(keys, ",")
}

func TestCountersPlans_Integration(t *testing.T) {
	db, _ := usageDB(t)
	// With a handful of rows the optimizer may prefer a scan; pad so the tenant range is selective.
	for i := 0; i < 300; i++ {
		seedHook(t, db, tenB, "created", dayFrom.Add(time.Duration(i)*time.Minute))
		seedJob(t, db, tenB, "outbound_webhook", "done", dayFrom.Add(time.Duration(i)*time.Minute))
	}
	k := explainKeys(t, db, `SELECT /*+ INDEX(sys_jobs idx_sys_jobs_tenant_list) */ status, COUNT(*) FROM sys_jobs
		WHERE tenant_id = ? AND created_at >= ? AND created_at < ? AND job_type = 'outbound_webhook' GROUP BY status`,
		tenB, dayFrom, dayTo)
	if !strings.Contains(k, "idx_sys_jobs_tenant_list") {
		t.Errorf("sys_jobs plan key = %q; want idx_sys_jobs_tenant_list", k)
	}
	k = explainKeys(t, db, `SELECT /*+ INDEX(sys_hook_deliveries idx_hookdel_tenant_created) */ status, COUNT(*) FROM sys_hook_deliveries
		WHERE tenant_id = ? AND created_at >= ? AND created_at < ? GROUP BY status`, tenB, dayFrom, dayTo)
	if !strings.Contains(k, "idx_hookdel_tenant_created") {
		t.Errorf("sys_hook_deliveries plan key = %q; want idx_hookdel_tenant_created", k)
	}
	k = explainKeys(t, db, `SELECT COUNT(*) FROM sys_workflow_runs
		WHERE tenant_id = ? AND started_at >= ? AND started_at < ? AND status <> 'skipped'`, tenB, dayFrom, dayTo)
	if !strings.Contains(k, "idx_wfr_tenant") {
		t.Errorf("sys_workflow_runs plan key = %q; want idx_wfr_tenant_started", k)
	}
	k = explainKeys(t, db, `SELECT status, source, COUNT(*) FROM sys_email_log
		WHERE tenant_id = ? AND created_at >= ? AND created_at < ? GROUP BY status, source`, tenB, dayFrom, dayTo)
	if !strings.Contains(k, "idx_email_log") {
		t.Errorf("sys_email_log plan key = %q; want an idx_email_log_* index", k)
	}
}

func valueOf(t *testing.T, db *sqlx.DB, tenant, day, metric, dim string) (int64, bool) {
	t.Helper()
	var v int64
	err := db.Get(&v, `SELECT value FROM sys_usage_daily WHERE tenant_id=? AND day=? AND metric=? AND dim=?`, tenant, day, metric, dim)
	if err != nil {
		return 0, false
	}
	return v, true
}

func TestUpsert_Integration(t *testing.T) {
	db, r := usageDB(t)
	ctx := context.Background()
	row := func(v int64) []Row { return []Row{{TenantID: tenA, Day: "2026-10-01", Metric: WorkflowRuns, Value: v}} }

	if err := r.Upsert(ctx, row(5), Overwrite); err != nil {
		t.Fatal(err)
	}
	if err := r.Upsert(ctx, row(0), Overwrite); err != nil { // a figure that fell to 0 is written
		t.Fatal(err)
	}
	if v, ok := valueOf(t, db, tenA, "2026-10-01", WorkflowRuns, ""); !ok || v != 0 {
		t.Fatalf("overwrite to 0 = %d,%v; want 0", v, ok)
	}
	if err := r.Upsert(ctx, row(9), Overwrite); err != nil {
		t.Fatal(err)
	}
	if err := r.Upsert(ctx, row(3), FillMissing); err != nil { // never lowers an existing figure
		t.Fatal(err)
	}
	if v, _ := valueOf(t, db, tenA, "2026-10-01", WorkflowRuns, ""); v != 9 {
		t.Fatalf("fillMissing replaced the stored 9 with %d", v)
	}
	if err := r.Upsert(ctx, []Row{{TenantID: tenA, Day: "2026-10-02", Metric: HooksFailed, Value: 4}}, FillMissing); err != nil {
		t.Fatal(err)
	}
	if v, ok := valueOf(t, db, tenA, "2026-10-02", HooksFailed, ""); !ok || v != 4 {
		t.Fatalf("fillMissing new row = %d,%v; want 4", v, ok)
	}
	// a real error is reported, not swallowed (INSERT IGNORE would hide it)
	long := strings.Repeat("x", 101)
	if err := r.Upsert(ctx, []Row{{TenantID: tenA, Day: "2026-10-03", Metric: EmailsSent, Dim: long, Value: 1}}, FillMissing); err == nil {
		t.Fatal("dim of 101 chars was accepted; want an error")
	}
	// two runs over the same data end in one row per key
	for i := 0; i < 2; i++ {
		done := make(chan error, 2)
		for j := 0; j < 2; j++ {
			go func() { done <- r.Upsert(ctx, row(7), Overwrite) }()
		}
		for j := 0; j < 2; j++ {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM sys_usage_daily WHERE tenant_id=? AND day='2026-10-01'`, tenA); err != nil || n != 1 {
		t.Fatalf("rows for the key = %d (%v); want 1", n, err)
	}
	// Range sums dims, pins the tenant, LastRollupAt sees the writes
	must(t, db, `INSERT INTO sys_usage_daily (id, tenant_id, day, metric, dim, value) VALUES (?, ?, '2026-10-05', 'emails_sent', 'a', 2), (?, ?, '2026-10-05', 'emails_sent', 'b', 3), (?, ?, '2026-10-05', 'emails_sent', '', 100)`,
		uuid.NewString(), tenA, uuid.NewString(), tenA, uuid.NewString(), tenB)
	got, err := r.Range(ctx, tenA, "2026-10-05", "2026-10-05")
	if err != nil || len(got) != 1 || got[0].Value != 5 || got[0].Day != "2026-10-05" {
		t.Fatalf("Range = %v, %v; want one day with 5", got, err)
	}
	if at, err := r.LastRollupAt(ctx, tenA, "2026-10-01"); err != nil || at == nil {
		t.Fatalf("LastRollupAt = %v, %v; want a time", at, err)
	}
	if at, err := r.LastRollupAt(ctx, "dddddddd-0000-0000-0000-00000000000d", "2026-10-01"); err != nil || at != nil {
		t.Fatalf("LastRollupAt of empty tenant = %v, %v; want nil", at, err)
	}
}
