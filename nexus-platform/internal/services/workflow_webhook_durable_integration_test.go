//go:build integration

// DAILY-QUEUE #26 — durable outbound_webhook, exercised END-TO-END over a freshly migrated
// throwaway MySQL DB (o3aSetup):
//
//	RecordService.Update                           (after-commit Emit)
//	  → workflow.AsyncDispatcher                   (async worker, real sys_workflows rule)
//	    → EnqueuingWebhookQueue → jobs.SQLStore    (sys_jobs row: outbound_webhook, tenant-pinned,
//	                                                rule id + slot + frozen body, NO secret/url)
//	process #1: jobs.Manager + NewOutboundWebhookJobHandler, receiver DOWN → attempt 1 fails,
//	            job rescheduled; the manager stops (= Nexus restart)
//	process #2: a NEW manager over the same table, receiver UP → attempt 2 delivers; status=done
//	#27:        both attempts carry X-Nexus-Delivery-Id = the job id; requeueing the SAME row (a
//	            resend) POSTs a third time with that same id
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=true&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run OutboundWebhookDurable
package services

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// webhookHit is one request the fake receiver saw.
type webhookHit struct {
	status   int
	body     []byte
	sig      string
	delivery string
	ts, sig2 string // X-Nexus-Timestamp / X-Nexus-Signature-V2 (specs/050 S2a)
}

// webhookReceiver answers `status()` and records every request.
type webhookReceiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   []webhookHit
	status atomic.Int32
}

func newWebhookReceiver(t *testing.T, status int) *webhookReceiver {
	t.Helper()
	r := &webhookReceiver{}
	r.status.Store(int32(status))
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		st := int(r.status.Load())
		r.mu.Lock()
		r.hits = append(r.hits, webhookHit{
			status:   st,
			body:     b,
			sig:      req.Header.Get(workflow.SignatureHeader),
			delivery: req.Header.Get(workflow.DeliveryIDHeader),
			ts:       req.Header.Get(workflow.TimestampHeader),
			sig2:     req.Header.Get(workflow.SignatureV2Header),
		})
		r.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *webhookReceiver) snapshot() []webhookHit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]webhookHit(nil), r.hits...)
}

// webhookJobRow is the sys_jobs projection the assertions read.
type webhookJobRow struct {
	ID        string         `db:"id"`
	TenantID  sql.NullString `db:"tenant_id"`
	Status    string         `db:"status"`
	Attempts  int            `db:"attempts"`
	MaxAtt    int            `db:"max_attempts"`
	Payload   string         `db:"payload"`
	LastError sql.NullString `db:"last_error"`
}

func readWebhookJobs(t *testing.T, st *wfStack) []webhookJobRow {
	t.Helper()
	var rows []webhookJobRow
	if err := st.sdb.Select(&rows, `
		SELECT id, tenant_id, status, attempts, max_attempts, CAST(payload AS CHAR) AS payload, last_error
		FROM sys_jobs WHERE job_type = ?`, OutboundWebhookJobType); err != nil {
		t.Fatalf("select sys_jobs: %v", err)
	}
	return rows
}

// waitWebhookJobs polls the webhook jobs until cond holds (or fails after 20s).
func waitWebhookJobs(t *testing.T, st *wfStack, cond func([]webhookJobRow) bool) []webhookJobRow {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		rows := readWebhookJobs(t, st)
		if cond(rows) {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("webhook jobs never reached the expected state; rows = %+v", rows)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitWebhookJob polls the single webhook job until cond holds (or fails after 20s).
func waitWebhookJob(t *testing.T, st *wfStack, cond func(webhookJobRow) bool) webhookJobRow {
	t.Helper()
	return waitWebhookJobs(t, st, func(rows []webhookJobRow) bool { return len(rows) == 1 && cond(rows[0]) })[0]
}

// webhookWire is wfWire plus the durable webhook pieces (mirrors cmd/server/main.go): the jobs
// store, the enqueuing queue on the dispatcher, and a registry holding the job handler over the
// SAME HTTP sender the inline path would use.
func webhookWire(t *testing.T) (*wfStack, *jobs.SQLStore, *jobs.Registry) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, 500*time.Millisecond)
	jobsStore := jobs.NewSQLStore(sdb, 5)
	sender := workflow.NewHTTPWebhookSender(2*time.Second, 3, 5*time.Millisecond)

	dispatcher := workflow.NewAsyncDispatcher(workflow.DispatcherConfig{Buffer: 16, Workers: 1},
		wfCache, NewWorkflowRecordMutator(dynRepo).WithCatalog(mc), wfRepo, nil).
		WithWebhookSender(sender).
		WithWebhookQueue(NewEnqueuingWebhookQueue(jobsStore)).
		WithRunRecorder(wfRepo)
	reg := jobs.NewRegistry()
	if err := reg.Register(OutboundWebhookJobType, NewOutboundWebhookJobHandler(wfRepo, nil, sender)); err != nil {
		t.Fatalf("register handler: %v", err)
	}
	return &wfStack{
		sdb:         sdb,
		entitySvc:   NewEntityService(dynRepo, metaRepo).WithMetaCache(mc),
		recordSvc:   NewRecordService(mc, dynRepo).WithEngine(dispatcher),
		workflowSvc: NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache),
		dispatcher:  dispatcher,
		cache:       wfCache,
	}, jobsStore, reg
}

// seedLegacyWebhookActions rewrites a rule's actions straight in sys_workflows, the way a rule saved
// before specs/050 S3a still carries an inline signing key (no write path accepts one now). A caller
// whose engine caches rules drops the cached copy afterwards.
func seedLegacyWebhookActions(t *testing.T, sdb *sqlx.DB, tenantID, id string, acts []workflow.Action) {
	t.Helper()
	raw, err := json.Marshal(acts)
	if err != nil {
		t.Fatalf("marshal legacy actions: %v", err)
	}
	res, err := sdb.Exec(`UPDATE sys_workflows SET actions = ? WHERE tenant_id = ? AND id = ?`, string(raw), tenantID, id)
	if err != nil {
		t.Fatalf("seed legacy actions: %v", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("seed legacy actions: %d row(s), err %v; want exactly the rule", n, err)
	}
}

// fireShippedWebhook registers "status → shipped ⇒ outbound_webhook(url, secret)" — the secret as a
// rule saved before S3a carries it — and ships one order, then drains the dispatcher so the enqueue
// has happened.
func fireShippedWebhook(t *testing.T, st *wfStack, url, secret string) {
	t.Helper()
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000261"
	st.createEntity(t, ctx, tenant, "orders", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	active := true
	rec, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name: "push-to-erp", EntityName: "orders", Trigger: "after_update", IsActive: &active,
		Rules:   []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "shipped"}},
		Actions: []WorkflowActionInput{{Type: "outbound_webhook", URL: url}},
		ActorID: user,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	if secret != "" { // an unsigned rule is exactly what CreateWorkflow stored
		seedLegacyWebhookActions(t, st.sdb, tenant, rec.ID, []workflow.Action{{Type: workflow.ActionWebhook, URL: url, Secret: secret}})
		st.cache.Invalidate(tenant, "orders")
	}
	id, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "orders", map[string]any{"status": "new"})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "orders", id, map[string]any{"status": "shipped"}, nil); err != nil {
		t.Fatalf("ship order: %v", err)
	}
	st.drain(t)
}

// newWebhookManager mirrors main.go: outbound_webhook is claimed by its own lane only.
func newWebhookManager(store *jobs.SQLStore, reg *jobs.Registry) *jobs.Manager {
	return jobs.NewManager(jobs.ManagerConfig{Workers: 1, PollInterval: time.Second, Lease: time.Minute,
		Dedicated: map[string]int{OutboundWebhookJobType: 1}}, store, reg, nil)
}

func stopManager(t *testing.T, m *jobs.Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		t.Fatalf("manager stop: %v", err)
	}
}

func TestOutboundWebhookDurable_SurvivesReceiverOutageAndRestart(t *testing.T) {
	const secret = "wh-secret-261"
	recv := newWebhookReceiver(t, http.StatusServiceUnavailable) // receiver is down
	st, store, reg := webhookWire(t)
	fireShippedWebhook(t, st, recv.srv.URL+"/erp/in", secret)

	// Parked, not posted: the dispatcher made no HTTP call, and the row carries neither the key
	// nor the URL — only the tenant, the rule/slot and the frozen body.
	if n := len(recv.snapshot()); n != 0 {
		t.Fatalf("receiver saw %d requests before any queue worker ran; the dispatcher must not post inline", n)
	}
	parked := waitWebhookJob(t, st, func(r webhookJobRow) bool { return r.Status == jobs.StatusQueued })
	if !parked.TenantID.Valid || parked.TenantID.String != o3aTenantA {
		t.Fatalf("tenant_id = %+v, want %s (a webhook job is tenant-pinned)", parked.TenantID, o3aTenantA)
	}
	if parked.MaxAtt != OutboundWebhookMaxAttempts {
		t.Fatalf("max_attempts = %d, want %d", parked.MaxAtt, OutboundWebhookMaxAttempts)
	}
	if strings.Contains(parked.Payload, secret) || strings.Contains(parked.Payload, recv.srv.URL) {
		t.Fatalf("sys_jobs.payload carries delivery config: %s", parked.Payload)
	}

	// Process #1: one attempt against the down receiver, then the process "restarts".
	m1 := newWebhookManager(store, reg)
	m1.Start()
	failed := waitWebhookJob(t, st, func(r webhookJobRow) bool { return r.Attempts == 1 && r.Status == jobs.StatusQueued })
	stopManager(t, m1)
	if !failed.LastError.Valid || !strings.Contains(failed.LastError.String, "status 503") {
		t.Fatalf("last_error = %+v, want the 503 recorded", failed.LastError)
	}
	// specs/050 FR-017: the error names the receiver by its ORIGIN (what the Việc nền screen of
	// #31–#33 shows next to the status) and never by anything after the host — a path, query or
	// userinfo may be the partner's key.
	if !strings.Contains(failed.LastError.String, "post "+recv.srv.URL+": status 503") || strings.Contains(failed.LastError.String, "/erp/in") {
		t.Fatalf("last_error = %q, want the origin + status and no path", failed.LastError.String)
	}

	// The receiver comes back; the backoff window passes (fast-forwarded, the queue's own 30s
	// ladder is pinned by the jobs package tests); process #2 picks the job up.
	recv.status.Store(http.StatusOK)
	if _, err := st.sdb.Exec(`UPDATE sys_jobs SET run_at = ? WHERE id = ?`, time.Now().Add(-time.Second), failed.ID); err != nil {
		t.Fatalf("fast-forward backoff: %v", err)
	}
	m2 := newWebhookManager(store, reg)
	m2.Start()
	done := waitWebhookJob(t, st, func(r webhookJobRow) bool { return r.Status == jobs.StatusDone })
	stopManager(t, m2)

	if done.Attempts != 2 || !done.TenantID.Valid {
		t.Fatalf("done row = attempts %d tenant %+v; want attempts 2 and a tenant", done.Attempts, done.TenantID)
	}
	hits := recv.snapshot()
	var delivered []webhookHit
	for _, h := range hits {
		if h.status == http.StatusOK {
			delivered = append(delivered, h)
		}
	}
	if len(hits) != 2 || len(delivered) != 1 {
		t.Fatalf("receiver saw %d requests (%d accepted); want 2 (1 refused while down + exactly 1 accepted)", len(hits), len(delivered))
	}
	got := delivered[0]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(got.body)
	if want := hex.EncodeToString(mac.Sum(nil)); got.sig != want {
		t.Fatalf("X-Nexus-Signature = %q, want the body-only HMAC %q", got.sig, want)
	}
	// specs/050 S2a: the queue path signs V2 over a timestamp taken by EACH attempt, so a retry
	// after a long outage still lands inside the receiver's replay window.
	for i, h := range hits {
		v2 := hmac.New(sha256.New, []byte(secret))
		v2.Write([]byte(h.ts + "."))
		v2.Write(h.body)
		if h.ts == "" || h.sig2 != hex.EncodeToString(v2.Sum(nil)) {
			t.Fatalf("POST %d: ts %q V2 %q; want V2 = HMAC(secret, ts + \".\" + body)", i+1, h.ts, h.sig2)
		}
	}
	if !strings.Contains(string(got.body), `"status":"shipped"`) || !strings.Contains(string(got.body), `"tenantId":"`+o3aTenantA+`"`) {
		t.Fatalf("delivered body lost the event: %s", got.body)
	}
	if string(hits[0].body) != string(got.body) {
		t.Fatalf("the retry changed the body:\n first %s\n retry %s", hits[0].body, got.body)
	}

	// #27: both attempts named the same delivery — the job id — so a receiver that already
	// processed it can drop a repeat.
	for i, h := range hits {
		if h.delivery != done.ID {
			t.Fatalf("POST %d carried %s %q, want the job id %q", i+1, workflow.DeliveryIDHeader, h.delivery, done.ID)
		}
	}
	// Resending the SAME job (what a "gửi lại" button does: requeue the row, never enqueue a copy)
	// re-POSTs with the SAME delivery id — the receiver sees a duplicate it can drop.
	if _, err := st.sdb.Exec(`UPDATE sys_jobs SET status = ?, run_at = ?, finished_at = NULL WHERE id = ?`,
		jobs.StatusQueued, time.Now().Add(-time.Second), done.ID); err != nil {
		t.Fatalf("requeue for resend: %v", err)
	}
	m3 := newWebhookManager(store, reg)
	m3.Start()
	waitWebhookJob(t, st, func(r webhookJobRow) bool { return r.Status == jobs.StatusDone && r.Attempts == 3 })
	stopManager(t, m3)
	hits = recv.snapshot()
	if len(hits) != 3 || hits[2].delivery != done.ID || string(hits[2].body) != string(got.body) {
		t.Fatalf("resend = %d POSTs, last delivery %q; want a 3rd POST with delivery %q and the same body",
			len(hits), hits[len(hits)-1].delivery, done.ID)
	}
}

// TestOutboundWebhookDurable_4xxFailsWithoutRetry: a receiver that REJECTS the request (4xx) will
// reject it again — the job dead-letters on the first attempt instead of hammering it 10 times.
func TestOutboundWebhookDurable_4xxFailsWithoutRetry(t *testing.T) {
	recv := newWebhookReceiver(t, http.StatusUnprocessableEntity)
	st, store, reg := webhookWire(t)
	fireShippedWebhook(t, st, recv.srv.URL, "")

	m := newWebhookManager(store, reg)
	m.Start()
	row := waitWebhookJob(t, st, func(r webhookJobRow) bool { return r.Status == jobs.StatusFailed })
	stopManager(t, m)
	if row.Attempts != 1 || len(recv.snapshot()) != 1 {
		t.Fatalf("attempts = %d, receiver hits = %d; want 1 and 1 (4xx is permanent)", row.Attempts, len(recv.snapshot()))
	}
	if !strings.Contains(row.LastError.String, "status 422") {
		t.Fatalf("last_error = %q, want the 422 recorded", row.LastError.String)
	}
}

// TestOutboundWebhookDurable_ResendAfterFailureKeepsDeliveryID pins "Gửi lại" (#32) end to end over
// real MySQL: a delivery the receiver rejected dead-letters; the receiver is fixed; the tenant's
// admin resends through BackgroundJobService — the SAME row is requeued (never a copy), the queue
// delivers it once, and the receiver sees the SAME X-Nexus-Delivery-Id and body as the rejected
// attempt, so it can deduplicate. Another tenant's admin cannot resend it by id; a second press on
// the delivered job is refused and posts nothing.
func TestOutboundWebhookDurable_ResendAfterFailureKeepsDeliveryID(t *testing.T) {
	recv := newWebhookReceiver(t, http.StatusUnprocessableEntity)
	st, store, reg := webhookWire(t)
	fireShippedWebhook(t, st, recv.srv.URL, "")
	ctx := context.Background()

	m1 := newWebhookManager(store, reg)
	m1.Start()
	failed := waitWebhookJob(t, st, func(r webhookJobRow) bool { return r.Status == jobs.StatusFailed })
	stopManager(t, m1)

	spy := &spyAuditor{}
	svc := NewBackgroundJobService(store).WithAuditor(spy)
	var ve *ValidationError
	if _, err := svc.Retry(ctx, o3aTenantB, "admin-of-b", failed.ID); !errors.As(err, &ve) || ve.Code != "JOB_NOT_FOUND" {
		t.Fatalf("another tenant's resend err = %v; want JOB_NOT_FOUND", err)
	}
	if row := readWebhookJobs(t, st)[0]; row.Status != jobs.StatusFailed {
		t.Fatalf("another tenant's resend moved the row to %s", row.Status)
	}

	recv.status.Store(http.StatusOK)
	job, err := svc.Retry(ctx, o3aTenantA, "admin-of-a", failed.ID)
	if err != nil || job.ID != failed.ID || job.Status != jobs.StatusQueued {
		t.Fatalf("resend = %+v, %v; want the same row queued", job, err)
	}
	m2 := newWebhookManager(store, reg)
	m2.Start()
	done := waitWebhookJob(t, st, func(r webhookJobRow) bool { return r.Status == jobs.StatusDone })
	stopManager(t, m2)

	if rows := readWebhookJobs(t, st); len(rows) != 1 {
		t.Fatalf("sys_jobs holds %d webhook rows; a resend must requeue the row, never copy it", len(rows))
	}
	if done.ID != failed.ID || done.Attempts != 1 {
		t.Fatalf("done row = %s attempts %d; want %s delivered on the first attempt of the new cycle", done.ID, done.Attempts, failed.ID)
	}
	hits := recv.snapshot()
	if len(hits) != 2 || hits[1].status != http.StatusOK {
		t.Fatalf("receiver saw %d requests; want the rejected one and exactly one resend", len(hits))
	}
	if hits[0].delivery != failed.ID || hits[1].delivery != failed.ID || string(hits[1].body) != string(hits[0].body) {
		t.Fatalf("resend carried delivery %q (rejected attempt %q, job %s) — the receiver could not deduplicate",
			hits[1].delivery, hits[0].delivery, failed.ID)
	}
	if len(spy.events) != 1 || spy.events[0].ActorUserID != "admin-of-a" || spy.events[0].RecordID != failed.ID {
		t.Fatalf("audit = %+v; want one __job__ row by admin-of-a", spy.events)
	}

	// Since #256 a DELIVERED webhook is resendable too (see the next test), so this one is pressed a
	// second time and the third delivery is awaited there. What stays refused is a press on a row
	// that is already back in the queue: nothing more is posted until a worker runs it.
	if _, err := svc.Retry(ctx, o3aTenantA, "admin-of-a", failed.ID); err != nil {
		t.Fatalf("second press on the delivered job err = %v; want it resent (#256)", err)
	}
	if _, err := svc.Retry(ctx, o3aTenantA, "admin-of-a", failed.ID); !errors.As(err, &ve) || ve.Code != "JOB_NOT_RETRYABLE" {
		t.Fatalf("press while queued err = %v; want JOB_NOT_RETRYABLE", err)
	}
	if n := len(recv.snapshot()); n != 2 {
		t.Fatalf("receiver saw %d requests before any worker ran the requeued row; want 2", n)
	}
}

// TestOutboundWebhookDurable_ResendDeliveredKeepsDeliveryID pins #256 end to end over real MySQL: the
// receiver answered 200 (the job is done) and then lost the order. The tenant's admin presses
// "Gửi lại" on the DELIVERED job — the SAME row is requeued, the receiver gets the body once more
// under the SAME X-Nexus-Delivery-Id (so a receiver that did keep the id can still deduplicate);
// another tenant's admin gets JOB_NOT_FOUND; a second press while the row waits is refused.
func TestOutboundWebhookDurable_ResendDeliveredKeepsDeliveryID(t *testing.T) {
	recv := newWebhookReceiver(t, http.StatusOK)
	st, store, reg := webhookWire(t)
	fireShippedWebhook(t, st, recv.srv.URL, "")
	ctx := context.Background()

	m1 := newWebhookManager(store, reg)
	m1.Start()
	delivered := waitWebhookJob(t, st, func(r webhookJobRow) bool { return r.Status == jobs.StatusDone })
	stopManager(t, m1)
	if n := len(recv.snapshot()); n != 1 {
		t.Fatalf("receiver saw %d requests for the first delivery; want 1", n)
	}

	spy := &spyAuditor{}
	svc := NewBackgroundJobService(store).WithAuditor(spy)
	var ve *ValidationError
	if _, err := svc.Retry(ctx, o3aTenantB, "admin-of-b", delivered.ID); !errors.As(err, &ve) || ve.Code != "JOB_NOT_FOUND" {
		t.Fatalf("another tenant's resend err = %v; want JOB_NOT_FOUND", err)
	}
	if row := readWebhookJobs(t, st)[0]; row.Status != jobs.StatusDone {
		t.Fatalf("another tenant's resend moved the row to %s", row.Status)
	}

	job, err := svc.Retry(ctx, o3aTenantA, "admin-of-a", delivered.ID)
	if err != nil || job.ID != delivered.ID || job.Status != jobs.StatusQueued {
		t.Fatalf("resend of a delivered webhook = %+v, %v; want the same row queued", job, err)
	}
	if _, err := svc.Retry(ctx, o3aTenantA, "admin-of-a", delivered.ID); !errors.As(err, &ve) || ve.Code != "JOB_NOT_RETRYABLE" {
		t.Fatalf("press while queued err = %v; want JOB_NOT_RETRYABLE", err)
	}
	m2 := newWebhookManager(store, reg)
	m2.Start()
	waitWebhookJob(t, st, func(r webhookJobRow) bool { return r.Status == jobs.StatusDone && r.Attempts == 1 })
	stopManager(t, m2)

	if rows := readWebhookJobs(t, st); len(rows) != 1 {
		t.Fatalf("sys_jobs holds %d webhook rows; a resend must requeue the row, never copy it", len(rows))
	}
	hits := recv.snapshot()
	if len(hits) != 2 {
		t.Fatalf("receiver saw %d requests; want the original and exactly one resend", len(hits))
	}
	if hits[0].delivery != delivered.ID || hits[1].delivery != delivered.ID || string(hits[1].body) != string(hits[0].body) {
		t.Fatalf("resend carried delivery %q (original %q, job %s) — the receiver could not deduplicate",
			hits[1].delivery, hits[0].delivery, delivered.ID)
	}
	if len(spy.events) != 1 || spy.events[0].Diff.Before["status"] != jobs.StatusDone || spy.events[0].ActorUserID != "admin-of-a" {
		t.Fatalf("audit = %+v; want one __job__ row by admin-of-a recording the done cycle it replaced", spy.events)
	}
}

// TestOutboundWebhookDurable_OlderEventArrivesLastAndSaysSo (review of #26): the queue does NOT
// keep order. Two updates of ONE record while the receiver is down: the older event has already
// burned an attempt, so its retry backs off longer and the NEWER event lands first. Without an
// instant in the body a receiver syncing state keeps whatever arrived last — the stale one. With
// occurredAt it drops the older snapshot and ends on the true final state.
func TestOutboundWebhookDurable_OlderEventArrivesLastAndSaysSo(t *testing.T) {
	recv := newWebhookReceiver(t, http.StatusServiceUnavailable) // receiver is down
	st, store, reg := webhookWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000262"
	st.createEntity(t, ctx, tenant, "orders", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	active := true
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name: "sync-to-erp", EntityName: "orders", Trigger: "after_update", IsActive: &active,
		Rules:   []WorkflowRuleInput{{Field: "status", Op: "neq", Value: "new"}},
		Actions: []WorkflowActionInput{{Type: "outbound_webhook", URL: recv.srv.URL}},
		ActorID: user,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	id, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "orders", map[string]any{"status": "new"})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	update := func(status string, wantJobs int) {
		t.Helper()
		if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "orders", id, map[string]any{"status": status}, nil); err != nil {
			t.Fatalf("update order to %s: %v", status, err)
		}
		waitWebhookJobs(t, st, func(rows []webhookJobRow) bool { return len(rows) == wantJobs })
	}

	// Event A (shipped) is parked and fails its first attempt: it backs off.
	update("shipped", 1)
	m1 := newWebhookManager(store, reg)
	m1.Start()
	a := waitWebhookJob(t, st, func(r webhookJobRow) bool { return r.Attempts == 1 && r.Status == jobs.StatusQueued })
	stopManager(t, m1)

	// Event B (delivered) is parked AFTER A, still during the outage; the receiver then recovers.
	update("delivered", 2)
	recv.status.Store(http.StatusOK)
	m2 := newWebhookManager(store, reg)
	m2.Start()
	// B is due now, A only when its backoff ends: B is delivered first.
	waitWebhookJobs(t, st, func(rows []webhookJobRow) bool {
		for _, r := range rows {
			if r.ID != a.ID && r.Status == jobs.StatusDone {
				return true
			}
		}
		return false
	})
	if _, err := st.sdb.Exec(`UPDATE sys_jobs SET run_at = ? WHERE id = ?`, time.Now().Add(-time.Second), a.ID); err != nil {
		t.Fatalf("fast-forward A's backoff: %v", err)
	}
	waitWebhookJobs(t, st, func(rows []webhookJobRow) bool {
		return len(rows) == 2 && rows[0].Status == jobs.StatusDone && rows[1].Status == jobs.StatusDone
	})
	stopManager(t, m2)
	st.drain(t)

	type received struct {
		RecordID   string         `json:"recordId"`
		OccurredAt string         `json:"occurredAt"`
		Data       map[string]any `json:"data"`
	}
	var accepted []received
	for _, h := range recv.snapshot() {
		if h.status != http.StatusOK {
			continue
		}
		var r received
		if err := json.Unmarshal(h.body, &r); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		accepted = append(accepted, r)
	}
	if len(accepted) != 2 || accepted[0].Data["status"] != "delivered" || accepted[1].Data["status"] != "shipped" {
		t.Fatalf("accepted = %+v; the scenario must reproduce the reorder (delivered first, the older shipped last)", accepted)
	}

	// The receiver contract (api-conventions §16): keep, per record, the event with the latest
	// occurredAt and drop anything older. Last-arrival-wins would end on "shipped".
	type state struct {
		at     time.Time
		status any
	}
	latest := map[string]state{}
	for _, r := range accepted {
		at, err := time.Parse(time.RFC3339Nano, r.OccurredAt)
		if err != nil || at.IsZero() {
			t.Fatalf("occurredAt = %q (err %v); every body must carry the instant its event happened", r.OccurredAt, err)
		}
		if cur, ok := latest[r.RecordID]; ok && !at.After(cur.at) {
			continue // older than what the receiver already has: dropped
		}
		latest[r.RecordID] = state{at: at, status: r.Data["status"]}
	}
	if got := latest[id].status; got != "delivered" {
		t.Fatalf("receiver ended on status %v, want delivered (occurredAt must order the two events by when they happened)", got)
	}
}

// TestOutboundWebhookDurable_ScheduledWebhooksHeldWhileDeliveriesPending (review of #26, case b):
// a scheduled rule used to POST inline and so could not fire again before its deliveries finished.
// Enqueued, a firing parks up to 5000 jobs at once whether or not the lane drained the previous
// ones — sys_jobs grew without bound even against a healthy receiver. A firing's deliveries are
// now held while the rule's earlier ones are pending, and parked again once those are delivered.
func TestOutboundWebhookDurable_ScheduledWebhooksHeldWhileDeliveriesPending(t *testing.T) {
	st := schedWire(t, workflow.NewHTTPWebhookSender(time.Second, 1, time.Millisecond),
		func(engine *workflow.AsyncDispatcher, sdb *sqlx.DB) {
			engine.WithWebhookQueue(NewEnqueuingWebhookQueue(jobs.NewSQLStore(sdb, 5)))
		})
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000263"
	st.createEntity(t, ctx, tenant, "invoices", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	active := true
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name: "push-overdue", EntityName: "invoices", Trigger: "scheduled", IntervalSeconds: 60, IsActive: &active,
		Rules:   []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "overdue"}},
		Actions: []WorkflowActionInput{{Type: "outbound_webhook", URL: "https://erp.example.test/in"}},
		ActorID: user,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "invoices",
			map[string]any{"status": "overdue"}); err != nil {
			t.Fatalf("create invoice %d: %v", i, err)
		}
	}
	count := func(status string) int {
		t.Helper()
		var n int
		if err := st.sdb.Get(&n, `SELECT COUNT(*) FROM sys_jobs WHERE job_type = ? AND tenant_id = ? AND status = ?`,
			OutboundWebhookJobType, tenant, status); err != nil {
			t.Fatalf("count jobs: %v", err)
		}
		return n
	}

	t0 := time.Now()
	st.scheduler.RunDueForTest(ctx, t0)
	if n := count(jobs.StatusQueued); n != 3 {
		t.Fatalf("first firing parked %d deliveries, want 3 (one per matching invoice)", n)
	}
	// Two intervals later the rule is due again, but its 3 deliveries have not gone out yet.
	st.scheduler.RunDueForTest(ctx, t0.Add(2*time.Minute))
	if n := count(jobs.StatusQueued); n != 3 {
		t.Fatalf("second firing parked more deliveries (%d queued) while the first 3 were still pending; they must be held", n)
	}
	// The lane delivers them; the next tick fires the rule again.
	if _, err := st.sdb.Exec(`UPDATE sys_jobs SET status = ?, finished_at = NOW(3) WHERE job_type = ?`, jobs.StatusDone, OutboundWebhookJobType); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	st.scheduler.RunDueForTest(ctx, t0.Add(4*time.Minute))
	if q, d := count(jobs.StatusQueued), count(jobs.StatusDone); q != 3 || d != 3 {
		t.Fatalf("after the deliveries drained: %d queued / %d done, want a new firing of 3 on top of the 3 delivered", q, d)
	}
}
