//go:build integration

// Queue #31/#32 — the tenant-facing reads/writes of sys_jobs (the "Việc nền" admin surface) over a
// real throwaway MySQL 8 database (jqSetup). sys_jobs is a cross-tenant SYSTEM table whose
// tenant_id is NULL for system jobs by design, so these tests seed three populations — the
// caller's tenant, another tenant, and tenant-NULL system rows — and assert the caller only ever
// sees (or touches) its own.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
)

const (
	tjTenantA = "aaaaaaaa-0000-0000-0000-00000000000a"
	tjTenantB = "bbbbbbbb-0000-0000-0000-00000000000b"
)

// tjSeed enqueues one job and forces its status / created_at, returning its id.
func tjSeed(t *testing.T, db *sqlx.DB, store *SQLStore, tenant, jobType, status, payload string, created time.Time) string {
	t.Helper()
	var p []byte
	if payload != "" {
		p = []byte(payload)
	}
	id, err := store.Enqueue(context.Background(), Job{Type: jobType, TenantID: tenant, Payload: p})
	if err != nil {
		t.Fatalf("enqueue %s/%s: %v", tenant, jobType, err)
	}
	var finished any
	if status == StatusDone || status == StatusFailed {
		finished = created.UTC()
	}
	if _, err := db.Exec(`UPDATE sys_jobs SET status = ?, created_at = ?, finished_at = ?,
		last_error = IF(? = 'failed', 'status 422', NULL) WHERE id = ?`,
		status, created.UTC(), finished, status, id); err != nil {
		t.Fatalf("force state of %s: %v", id, err)
	}
	return id
}

// tjWalk pages through ListForTenant with the given page size and returns every row in order.
func tjWalk(t *testing.T, store *SQLStore, tenant string, f TenantJobFilter) []TenantJob {
	t.Helper()
	var all []TenantJob
	for page := 0; page < 50; page++ {
		rows, next, err := store.ListForTenant(context.Background(), tenant, f)
		if err != nil {
			t.Fatalf("list page %d (%+v): %v", page, f, err)
		}
		all = append(all, rows...)
		if next == "" {
			return all
		}
		f.After = next
	}
	t.Fatalf("pagination never ended for %+v", f)
	return nil
}

// TestJobsListForTenant_Integration pins #31's store contract: tenant-pinned (another tenant's rows
// and tenant-NULL system rows never appear), newest-first with an id tie-break that paging can
// cross without a gap or a duplicate, filters by type/status, the rule id lifted out of the
// payload (the payload itself never leaves), and a foreign cursor refused.
func TestJobsListForTenant_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	base := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	tie := base.Add(3 * time.Minute) // four A rows share this instant: pages must split them cleanly

	want := map[string]bool{}
	seedA := func(jobType, status, payload string, at time.Time) string {
		id := tjSeed(t, db, store, tjTenantA, jobType, status, payload, at)
		want[id] = true
		return id
	}
	webhookFailed := seedA("outbound_webhook", StatusFailed, `{"workflowId":"wf-1","actionIndex":0,"body":"{}"}`, base)
	seedA("outbound_webhook", StatusDone, `{"workflowId":"wf-1","actionIndex":0,"body":"{}"}`, base.Add(time.Minute))
	seedA("workflow_timer", StatusQueued, `{"workflowId":"wf-2","recordId":"r1"}`, base.Add(2*time.Minute))
	for i := 0; i < 4; i++ {
		seedA([]string{"outbound_webhook", "workflow_timer"}[i%2], []string{StatusFailed, StatusDone}[i%2],
			fmt.Sprintf(`{"workflowId":"wf-tie-%d"}`, i), tie)
	}
	seedA("outbound_webhook", StatusQueued, "", base.Add(4*time.Minute)) // NULL payload ⇒ no workflowId
	// Noise the caller must never see: another tenant, and system (tenant-NULL) jobs.
	tjSeed(t, db, store, tjTenantB, "outbound_webhook", StatusFailed, `{"workflowId":"wf-b"}`, base.Add(5*time.Minute))
	tjSeed(t, db, store, "", "email_alert", StatusFailed, `{"to":"x@y.z"}`, base.Add(6*time.Minute))
	tjSeed(t, db, store, "", "email_alert", StatusDone, "", base.Add(7*time.Minute))

	// Unfiltered, one big page: exactly A's rows, newest first, id DESC inside a tie.
	all, next, err := store.ListForTenant(context.Background(), tjTenantA, TenantJobFilter{Limit: 100})
	if err != nil || next != "" {
		t.Fatalf("list all: next=%q err=%v", next, err)
	}
	if len(all) != len(want) {
		t.Fatalf("got %d rows, want %d (only tenant A; never tenant B or tenant-NULL system jobs): %+v", len(all), len(want), all)
	}
	for i, r := range all {
		if !want[r.ID] {
			t.Fatalf("row %d (%s, %s) is not tenant A's", i, r.ID, r.Type)
		}
		if i > 0 {
			prev := all[i-1]
			if r.CreatedAt.After(prev.CreatedAt) || (r.CreatedAt.Equal(prev.CreatedAt) && r.ID > prev.ID) {
				t.Fatalf("rows %d/%d out of (created_at DESC, id DESC) order: %v %s then %v %s",
					i-1, i, prev.CreatedAt, prev.ID, r.CreatedAt, r.ID)
			}
		}
	}
	if all[0].WorkflowID != nil {
		t.Fatalf("a job with no payload must list workflowId=null, got %q", *all[0].WorkflowID)
	}
	last := all[len(all)-1]
	if last.ID != webhookFailed || last.WorkflowID == nil || *last.WorkflowID != "wf-1" ||
		last.Status != StatusFailed || last.LastError == nil || *last.LastError != "status 422" ||
		last.MaxAttempts != 5 || last.FinishedAt == nil {
		t.Fatalf("oldest row = %+v, want the failed webhook with workflowId wf-1, lastError and finishedAt", last)
	}

	// Paging by 2 crosses the four-way tie: same rows, same order, nothing lost or repeated.
	paged := tjWalk(t, store, tjTenantA, TenantJobFilter{Limit: 2})
	if len(paged) != len(all) {
		t.Fatalf("paged walk returned %d rows, want %d", len(paged), len(all))
	}
	for i := range all {
		if paged[i].ID != all[i].ID {
			t.Fatalf("paged row %d = %s, want %s (gap or duplicate at a page boundary)", i, paged[i].ID, all[i].ID)
		}
	}

	// Filters, also paged by 1 so every boundary is exercised.
	for _, tc := range []struct {
		f    TenantJobFilter
		keep func(TenantJob) bool
	}{
		{TenantJobFilter{Status: StatusFailed}, func(j TenantJob) bool { return j.Status == StatusFailed }},
		{TenantJobFilter{Type: "workflow_timer"}, func(j TenantJob) bool { return j.Type == "workflow_timer" }},
		{TenantJobFilter{Type: "outbound_webhook", Status: StatusFailed}, func(j TenantJob) bool {
			return j.Type == "outbound_webhook" && j.Status == StatusFailed
		}},
	} {
		var expect []string
		for _, r := range all {
			if tc.keep(r) {
				expect = append(expect, r.ID)
			}
		}
		f := tc.f
		f.Limit = 1
		var got []string
		for _, r := range tjWalk(t, store, tjTenantA, f) {
			got = append(got, r.ID)
		}
		if strings.Join(got, ",") != strings.Join(expect, ",") || len(expect) == 0 {
			t.Fatalf("filter %+v = %v, want %v", tc.f, got, expect)
		}
	}

	// The other tenant sees only its own row.
	b, _, err := store.ListForTenant(context.Background(), tjTenantB, TenantJobFilter{})
	if err != nil || len(b) != 1 || b[0].WorkflowID == nil || *b[0].WorkflowID != "wf-b" {
		t.Fatalf("tenant B list = %+v (err %v), want its one row", b, err)
	}

	// No tenant is a caller bug, never "every system job".
	if _, _, err := store.ListForTenant(context.Background(), "", TenantJobFilter{}); err == nil {
		t.Fatal("an empty tenant must be refused")
	}
	// A record-list cursor (same column name, other surface) is refused, not misread.
	foreign := dynrepo.EncodeCursor(dynrepo.DefaultCursorSort, "DESC", false, "2026-09-30 08:00:00.000", webhookFailed)
	if _, _, err := store.ListForTenant(context.Background(), tjTenantA, TenantJobFilter{After: foreign}); !errors.Is(err, dynrepo.ErrInvalidCursor) {
		t.Fatalf("foreign cursor err = %v, want ErrInvalidCursor", err)
	}
}

// TestJobsTenantListSeeks_Integration pins the plan of every listing shape (B1): it rides
// idx_sys_jobs_tenant_list in index order — no sort — with or without filters and cursor, so a
// page costs the rows it walks, not a sort of the tenant's whole 30-day history.
func TestJobsTenantListSeeks_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	base := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 300; i++ {
		tenant := []string{tjTenantA, tjTenantB, ""}[i%3]
		jobType := []string{"outbound_webhook", "workflow_timer", "email_alert"}[i%3]
		status := []string{StatusDone, StatusDone, StatusDone, StatusFailed, StatusQueued}[i%5]
		tjSeed(t, db, store, tenant, jobType, status, `{"workflowId":"wf"}`, base.Add(time.Duration(i)*time.Minute))
	}
	if _, err := db.Exec(`ANALYZE TABLE sys_jobs`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	cursor := dynrepo.EncodeCursor(tenantListCursorSort, "DESC", false, base.Add(time.Hour).Format(tenantListTimeLayout),
		"ffffffff-ffff-ffff-ffff-ffffffffffff")
	for _, f := range []TenantJobFilter{
		{},
		{Status: StatusFailed},
		{Type: "outbound_webhook"},
		{Type: "outbound_webhook", Status: StatusFailed},
		{Status: StatusFailed, After: cursor},
		{After: cursor},
	} {
		query, args, err := tenantListSelect(tjTenantA, f)
		if err != nil {
			t.Fatalf("%+v: build: %v", f, err)
		}
		var plan string
		if err := db.Get(&plan, "EXPLAIN FORMAT=TREE "+query, args...); err != nil {
			t.Fatalf("%+v: explain: %v", f, err)
		}
		if !strings.Contains(plan, "idx_sys_jobs_tenant_list") || strings.Contains(strings.ToLower(plan), "sort") {
			t.Fatalf("listing %+v does not walk idx_sys_jobs_tenant_list in order:\n%s", f, plan)
		}
		// A filter checked after the row read would fetch every stepped-over row (a failed-only
		// walk through the tenant's done history); it must be pushed into the index walk.
		if (f.Status != "" || f.Type != "") && !strings.Contains(plan, "with index condition") {
			t.Fatalf("listing %+v filters after reading rows (no index condition pushdown):\n%s", f, plan)
		}
		if testing.Verbose() {
			t.Logf("%+v:\n%s", f, plan)
		}
	}
}

// TestJobsRequeueFinishedForTenant_Integration pins #32's store contract: "Gửi lại" puts the SAME
// failed row back in the queue — same id (so the receiver sees the same X-Nexus-Delivery-Id), same
// payload — with a fresh attempt budget, runnable now; and it touches nothing else: not another
// tenant's row (a tenant-pinned UPDATE, not a list-side filter), not a system job, not a row that is
// queued/running/done, not a failed row whose payload was scrubbed (nothing left to send).
func TestJobsRequeueFinishedForTenant_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	const payload = `{"workflowId":"wf-1","actionIndex":0,"body":"{\"id\":\"r1\"}"}`

	failed := tjSeed(t, db, store, tjTenantA, "outbound_webhook", StatusFailed, payload, past)
	if _, err := db.Exec(`UPDATE sys_jobs SET attempts = 10, max_attempts = 10, claimed_by = 'w-old',
		claimed_at = ?, run_at = ? WHERE id = ?`, past.UTC(), past.UTC(), failed); err != nil {
		t.Fatalf("burn the budget: %v", err)
	}
	otherTenant := tjSeed(t, db, store, tjTenantB, "outbound_webhook", StatusFailed, payload, past)
	system := tjSeed(t, db, store, "", "outbound_webhook", StatusFailed, payload, past)
	scrubbed := tjSeed(t, db, store, tjTenantA, "email_alert", StatusFailed, "", past)
	notFailed := map[string]string{}
	for _, st := range []string{StatusQueued, StatusRunning, StatusDone} {
		notFailed[st] = tjSeed(t, db, store, tjTenantA, "outbound_webhook", st, payload, past)
	}
	// Park the queued one in the future so the claim below can only find the requeued row.
	if _, err := db.Exec(`UPDATE sys_jobs SET run_at = ? WHERE id = ?`, time.Now().Add(time.Hour).UTC(), notFailed[StatusQueued]); err != nil {
		t.Fatalf("park queued row: %v", err)
	}

	// Refusals first: none of them may change a byte.
	refused := map[string]string{"another tenant's row": otherTenant, "a system job": system,
		"a failed row with no payload": scrubbed, "an unknown id": "00000000-0000-0000-0000-000000000000"}
	for st, id := range notFailed {
		refused["a "+st+" row"] = id
	}
	for what, id := range refused {
		var before, after jobState
		if what != "an unknown id" {
			before = jqState(t, db, id)
		}
		applied, err := store.RequeueFinished(ctx, tjTenantA, id, StatusFailed, time.Now())
		if err != nil || applied {
			t.Fatalf("requeue of %s: applied=%v err=%v; want refused", what, applied, err)
		}
		if what != "an unknown id" {
			if after = jqState(t, db, id); after.Status != before.Status || after.Attempts != before.Attempts {
				t.Fatalf("refused requeue of %s changed the row: %+v -> %+v", what, before, after)
			}
		}
	}
	if _, err := store.RequeueFinished(ctx, "", failed, StatusFailed, time.Now()); err == nil {
		t.Fatal("an empty tenant must be refused")
	}

	// The real one: same row, fresh budget, runnable now, error of the old cycle cleared.
	now := time.Now()
	applied, err := store.RequeueFinished(ctx, tjTenantA, failed, StatusFailed, now)
	if err != nil || !applied {
		t.Fatalf("requeue own failed job: applied=%v err=%v", applied, err)
	}
	st := jqState(t, db, failed)
	if st.Status != StatusQueued || st.Attempts != 0 || st.MaxAttempts != 10 || st.FinishedAt != nil ||
		st.LastError != nil || st.ClaimedBy != nil || st.ClaimedAt != nil || st.RunAt.After(now.Add(time.Second)) {
		t.Fatalf("row after requeue = %+v; want queued, attempts 0/10, runnable now, no finish/error/claim", st)
	}
	// A second press on the same (now queued) row is refused: one resend, not two.
	if again, err := store.RequeueFinished(ctx, tjTenantA, failed, StatusFailed, time.Now()); err != nil || again {
		t.Fatalf("second requeue: applied=%v err=%v; want refused", again, err)
	}
	// The queue takes it like any other due job, and it is still the same delivery.
	job, err := store.ClaimNext(ctx, "w-new", time.Now().Add(time.Second), Lane{Only: "outbound_webhook"})
	if err != nil || job == nil || job.ID != failed || job.Attempts != 1 || !strings.Contains(string(job.Payload), `"wf-1"`) {
		t.Fatalf("claim after requeue = %+v (err %v); want the same row, attempt 1, payload intact", job, err)
	}

	// GetForTenant: own row yes; another tenant's, a system job, an unknown id: ErrJobNotFound.
	got, err := store.GetForTenant(ctx, tjTenantA, notFailed[StatusDone])
	if err != nil || got.ID != notFailed[StatusDone] || got.Status != StatusDone || got.WorkflowID == nil {
		t.Fatalf("GetForTenant own = %+v, %v", got, err)
	}
	for what, id := range map[string]string{"another tenant's": otherTenant, "a system": system, "an unknown": "nope"} {
		if _, err := store.GetForTenant(ctx, tjTenantA, id); !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("GetForTenant(%s job) err = %v; want ErrJobNotFound", what, err)
		}
	}
	if _, err := store.GetForTenant(ctx, "", failed); err == nil || errors.Is(err, ErrJobNotFound) {
		t.Fatalf("GetForTenant with no tenant err = %v; want a refusal, not a not-found", err)
	}
}

// TestJobsRequeueDoneForTenant_Integration pins #256's store contract: the SAME UPDATE that resends a
// failed row also resends a DELIVERED one when the caller names from=done (a receiver that answered
// 200 and then lost the order) — same id, same payload, fresh budget — and the `from` it names is
// part of the guard: a done press never moves a failed/running row, a failed press never moves a
// done row, and a status that is not finished (queued/running/unknown) is refused outright so no
// caller can requeue a row a worker may hold.
func TestJobsRequeueDoneForTenant_Integration(t *testing.T) {
	db := jqSetup(t)
	store := NewSQLStore(db, 5)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	const payload = `{"workflowId":"wf-1","actionIndex":0,"body":"{\"id\":\"r1\"}"}`

	done := tjSeed(t, db, store, tjTenantA, "outbound_webhook", StatusDone, payload, past)
	if _, err := db.Exec(`UPDATE sys_jobs SET attempts = 3, max_attempts = 10, last_error = 'status 503',
		run_at = ? WHERE id = ?`, past.UTC(), done); err != nil {
		t.Fatalf("shape the done row: %v", err)
	}
	otherTenant := tjSeed(t, db, store, tjTenantB, "outbound_webhook", StatusDone, payload, past)
	system := tjSeed(t, db, store, "", "outbound_webhook", StatusDone, payload, past)
	noPayload := tjSeed(t, db, store, tjTenantA, "email_alert", StatusDone, "", past)
	failed := tjSeed(t, db, store, tjTenantA, "outbound_webhook", StatusFailed, payload, past)
	running := tjSeed(t, db, store, tjTenantA, "outbound_webhook", StatusRunning, payload, past)

	refused := map[string]string{"another tenant's done row": otherTenant, "a done system job": system,
		"a done row with no payload": noPayload, "a failed row asked as done": failed, "a running row asked as done": running}
	for what, id := range refused {
		before := jqState(t, db, id)
		applied, err := store.RequeueFinished(ctx, tjTenantA, id, StatusDone, time.Now())
		if err != nil || applied {
			t.Fatalf("done-requeue of %s: applied=%v err=%v; want refused", what, applied, err)
		}
		if after := jqState(t, db, id); after.Status != before.Status || after.Attempts != before.Attempts {
			t.Fatalf("refused done-requeue of %s changed the row: %+v -> %+v", what, before, after)
		}
	}
	// A failed press never reaches a done row.
	if applied, err := store.RequeueFinished(ctx, tjTenantA, done, StatusFailed, time.Now()); err != nil || applied {
		t.Fatalf("failed-requeue of a done row: applied=%v err=%v; want refused", applied, err)
	}
	// Only finished statuses may be named.
	for _, bad := range []string{StatusQueued, StatusRunning, "", "nope"} {
		if _, err := store.RequeueFinished(ctx, tjTenantA, running, bad, time.Now()); err == nil {
			t.Fatalf("RequeueFinished(from=%q) must be refused with an error", bad)
		}
	}
	if st := jqState(t, db, running); st.Status != StatusRunning {
		t.Fatalf("a refused from-status moved the running row to %s", st.Status)
	}

	now := time.Now()
	applied, err := store.RequeueFinished(ctx, tjTenantA, done, StatusDone, now)
	if err != nil || !applied {
		t.Fatalf("requeue own done job: applied=%v err=%v", applied, err)
	}
	st := jqState(t, db, done)
	if st.Status != StatusQueued || st.Attempts != 0 || st.MaxAttempts != 10 || st.FinishedAt != nil ||
		st.LastError != nil || st.ClaimedBy != nil || st.RunAt.After(now.Add(time.Second)) {
		t.Fatalf("row after requeue = %+v; want queued, attempts 0/10, runnable now, no finish/error/claim", st)
	}
	if again, err := store.RequeueFinished(ctx, tjTenantA, done, StatusDone, time.Now()); err != nil || again {
		t.Fatalf("second press while queued: applied=%v err=%v; want refused", again, err)
	}
	job, err := store.ClaimNext(ctx, "w-new", time.Now().Add(time.Second), Lane{Only: "outbound_webhook"})
	if err != nil || job == nil || job.ID != done || job.Attempts != 1 || !strings.Contains(string(job.Payload), `"wf-1"`) {
		t.Fatalf("claim after requeue = %+v (err %v); want the same row, attempt 1, payload intact", job, err)
	}
}
