package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/jobs"
	"nexus/platform/internal/notify"
	"nexus/platform/internal/workflow"
)

// fakeJobEnqueuer records the enqueued jobs (or fails on demand), and answers the pending probe.
type fakeJobEnqueuer struct {
	jobs []jobs.Job
	err  error

	pending bool
	probes  []string // "jobType|tenant|workflow"
}

func (f *fakeJobEnqueuer) HasPendingForWorkflow(_ context.Context, jobType, tenantID, workflowID string) (bool, error) {
	f.probes = append(f.probes, jobType+"|"+tenantID+"|"+workflowID)
	return f.pending, f.err
}

func (f *fakeJobEnqueuer) FailedBySlot(context.Context, string, string, string) (map[int]int, error) {
	return nil, f.err
}

func (f *fakeJobEnqueuer) Enqueue(_ context.Context, j jobs.Job) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.jobs = append(f.jobs, j)
	return "id-1", nil
}

// fakeMailTransport records the sent mail (or fails on demand). Locked: the integration tests
// hand it to a real queue worker goroutine.
type fakeMailTransport struct {
	mu   sync.Mutex
	sent []notify.Mail
	err  error
}

func (f *fakeMailTransport) Send(_ context.Context, m notify.Mail) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeMailTransport) mails() []notify.Mail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// TestEnqueuingWorkflowEmailSender: the rendered mail is parked as an email_alert system job with
// the exact payload shape the handler consumes.
func TestEnqueuingWorkflowEmailSender(t *testing.T) {
	q := &fakeJobEnqueuer{}
	s := NewEnqueuingWorkflowEmailSender(q)

	if err := s.SendEmail(context.Background(), "a@b.vn", "Hi", "Body"); err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	if len(q.jobs) != 1 {
		t.Fatalf("enqueued %d jobs, want 1", len(q.jobs))
	}
	j := q.jobs[0]
	if j.Type != EmailAlertJobType {
		t.Fatalf("job type = %q, want %q", j.Type, EmailAlertJobType)
	}
	if j.TenantID != "" {
		t.Fatalf("outside a recorded run email_alert stays a system job (empty tenant), got %q", j.TenantID)
	}
	var p emailAlertJobPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if p.To != "a@b.vn" || p.Subject != "Hi" || p.Body != "Body" {
		t.Fatalf("payload = %+v, want the rendered mail", p)
	}
}

// TestEnqueuingWorkflowEmailSenderErrors: enqueue failure and missing queue surface as errors so
// the workflow action records them (fail-open in the dispatcher).
func TestEnqueuingWorkflowEmailSenderErrors(t *testing.T) {
	if err := NewEnqueuingWorkflowEmailSender(nil).SendEmail(context.Background(), "a@b.vn", "s", "b"); err == nil {
		t.Fatal("nil queue must error")
	}
	q := &fakeJobEnqueuer{err: errors.New("db down")}
	if err := NewEnqueuingWorkflowEmailSender(q).SendEmail(context.Background(), "a@b.vn", "s", "b"); err == nil {
		t.Fatal("enqueue failure must propagate")
	}
}

// TestEmailAlertJobHandler: the queue-side consumer delivers through the notify transport; a
// transport failure propagates so the queue retries; malformed payload / blank recipient error.
func TestEmailAlertJobHandler(t *testing.T) {
	mkJob := func(payload string) jobs.Job {
		return jobs.Job{ID: "j1", Type: EmailAlertJobType, Payload: json.RawMessage(payload)}
	}

	t.Run("delivers the payload", func(t *testing.T) {
		tr := &fakeMailTransport{}
		h := NewEmailAlertJobHandler(tr, nil, nil, nil)
		if err := h(context.Background(), mkJob(`{"to":"a@b.vn","subject":"Hi","body":"B"}`)); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if len(tr.sent) != 1 || tr.sent[0].To != "a@b.vn" || tr.sent[0].Subject != "Hi" || tr.sent[0].Body != "B" {
			t.Fatalf("sent = %+v, want the payload mail", tr.sent)
		}
	})

	t.Run("SMTP failure propagates (queue retries)", func(t *testing.T) {
		tr := &fakeMailTransport{err: errors.New("connection refused")}
		h := NewEmailAlertJobHandler(tr, nil, nil, nil)
		if err := h(context.Background(), mkJob(`{"to":"a@b.vn"}`)); err == nil {
			t.Fatal("transport failure must propagate so the queue retries with backoff")
		}
	})

	// workflow-run-error-pii (#85): the queue copies this error into sys_jobs.last_error and the
	// worker logs it. An SMTP server rejecting RCPT TO echoes the address, so the text must be
	// masked while the chain stays intact for errors.Is.
	t.Run("SMTP failure text never carries the recipient", func(t *testing.T) {
		sentinel := errors.New("mailbox unavailable")
		tr := &fakeMailTransport{err: fmt.Errorf("rcpt to: 550 <a.canhan@gmail.com>: %w", sentinel)}
		err := NewEmailAlertJobHandler(tr, nil, nil, nil)(context.Background(), mkJob(`{"to":"a.canhan@gmail.com"}`))
		if err == nil || strings.Contains(err.Error(), "a.canhan@gmail.com") {
			t.Fatalf("job error must not carry the recipient, got %v", err)
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("masking broke the error chain: %v", err)
		}
	})

	t.Run("malformed payload errors", func(t *testing.T) {
		h := NewEmailAlertJobHandler(&fakeMailTransport{}, nil, nil, nil)
		if err := h(context.Background(), mkJob(`not-json`)); err == nil {
			t.Fatal("malformed payload must error (lands in last_error, never silently dropped)")
		}
	})

	t.Run("blank recipient errors", func(t *testing.T) {
		h := NewEmailAlertJobHandler(&fakeMailTransport{}, nil, nil, nil)
		if err := h(context.Background(), mkJob(`{"subject":"s"}`)); err == nil {
			t.Fatal("blank recipient must error")
		}
	})

	t.Run("nil transport errors", func(t *testing.T) {
		h := NewEmailAlertJobHandler(nil, nil, nil, nil)
		if err := h(context.Background(), mkJob(`{"to":"a@b.vn"}`)); err == nil {
			t.Fatal("nil transport must error")
		}
	})
}

// ---- specs/059: a queued email that gives up flips ITS run to failed ------------------------

// TestEnqueuingWorkflowEmailSender_PinsTheTenantAndTheRunRow: inside a recorded run the job is
// tenant-pinned (the tenant admin's "Việc nền" lists it) and names the run row + action slot its
// outcome belongs to. Config ids only — the address stays in the scrubbed-on-finish fields.
func TestEnqueuingWorkflowEmailSender_PinsTheTenantAndTheRunRow(t *testing.T) {
	q := &fakeJobEnqueuer{}
	ctx := workflow.WithRunRef(context.Background(), workflow.RunRef{TenantID: "t1", WorkflowID: "wf-1", RunID: "run-1", ActionIndex: 2})
	if err := NewEnqueuingWorkflowEmailSender(q).SendEmail(ctx, "lan@nhakhoa.vn", "s", "b"); err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	if len(q.jobs) != 1 || q.jobs[0].TenantID != "t1" {
		t.Fatalf("jobs = %+v; want one job pinned to tenant t1", q.jobs)
	}
	var p emailAlertJobPayload
	if err := json.Unmarshal(q.jobs[0].Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.WorkflowID != "wf-1" || p.RunID != "run-1" || p.ActionIndex != 2 || p.To != "lan@nhakhoa.vn" {
		t.Fatalf("payload = %+v; want the run row + slot and the rendered mail", p)
	}
}

// fakeRunAnnotator records MarkRunActionFailed calls; found answers each call in turn (the last
// answer repeats) so a test can make the run row appear only after a few tries.
type fakeRunAnnotator struct {
	calls []string // "tenant|run|idx|msg"
	sent  []string // "tenant|run|idx"
	found []bool
}

// sent records MarkRunActionSent calls ("tenant|run|idx").
func (f *fakeRunAnnotator) MarkRunActionSent(_ context.Context, tenantID, runID string, actionIndex int) (bool, error) {
	f.sent = append(f.sent, tenantID+"|"+runID+"|"+strconv.Itoa(actionIndex))
	return true, nil
}

func (f *fakeRunAnnotator) MarkRunActionFailed(_ context.Context, tenantID, runID string, actionIndex int, _, msg string) (bool, error) {
	f.calls = append(f.calls, tenantID+"|"+runID+"|"+strconv.Itoa(actionIndex)+"|"+msg)
	if len(f.found) == 0 {
		return true, nil
	}
	ok := f.found[0]
	if len(f.found) > 1 {
		f.found = f.found[1:]
	}
	return ok, nil
}

func TestEmailAlertJobHandler_GivingUpMarksTheRun(t *testing.T) {
	const personal = "lan.canhan@gmail.com"
	payload := `{"to":"` + personal + `","subject":"s","body":"b","workflowId":"wf-1","runId":"run-1","actionIndex":1}`
	job := func(attempt, max int, p string) jobs.Job {
		return jobs.Job{ID: "j1", TenantID: "t1", Type: EmailAlertJobType, Payload: json.RawMessage(p), Attempts: attempt, MaxAttempts: max}
	}
	echo := fmt.Errorf("rcpt to: 550 <%s>: Recipient address rejected", personal)

	t.Run("a retry is still coming: the run is left alone", func(t *testing.T) {
		runs := &fakeRunAnnotator{}
		err := NewEmailAlertJobHandler(&fakeMailTransport{err: echo}, runs, nil, nil)(context.Background(), job(1, 5, payload))
		if err == nil || jobs.IsPermanent(err) {
			t.Fatalf("err = %v; want a retryable failure", err)
		}
		if len(runs.calls) != 0 {
			t.Errorf("run annotated before the queue gave up: %v", runs.calls)
		}
	})

	t.Run("last attempt: the run's email slot turns failed, address masked", func(t *testing.T) {
		runs := &fakeRunAnnotator{}
		err := NewEmailAlertJobHandler(&fakeMailTransport{err: echo}, runs, nil, nil)(context.Background(), job(5, 5, payload))
		if err == nil {
			t.Fatal("the delivery failure must still be returned to the queue")
		}
		if len(runs.calls) != 1 || !strings.HasPrefix(runs.calls[0], "t1|run-1|1|") {
			t.Fatalf("annotations = %v; want one on tenant t1, run-1, slot 1", runs.calls)
		}
		msg := runs.calls[0]
		if strings.Contains(strings.ToLower(msg), personal) || !containsAllStr(msg, "[redacted]", "5/5", "Recipient address rejected") {
			t.Errorf("run message %q must be masked and say the queue gave up after 5/5 tries", msg)
		}
	})

	t.Run("server has no mail configured: permanent at once, run marked on attempt 1", func(t *testing.T) {
		runs := &fakeRunAnnotator{}
		err := NewEmailAlertJobHandler(&fakeMailTransport{err: notify.ErrMailNotConfigured}, runs, nil, nil)(context.Background(), job(1, 5, payload))
		if !jobs.IsPermanent(err) || !errors.Is(err, notify.ErrMailNotConfigured) {
			t.Fatalf("err = %v; want a PERMANENT failure wrapping ErrMailNotConfigured — retrying cannot configure a server", err)
		}
		if len(runs.calls) != 1 {
			t.Errorf("annotations = %v; want the run marked now", runs.calls)
		}
	})

	t.Run("run row missing: ONE try, no waiting (#338 — the row is written before the action runs)", func(t *testing.T) {
		runs := &fakeRunAnnotator{found: []bool{false, false, true}}
		begin := time.Now()
		_ = NewEmailAlertJobHandler(&fakeMailTransport{err: echo}, runs, nil, nil)(context.Background(), job(5, 5, payload))
		if len(runs.calls) != 1 || time.Since(begin) > 100*time.Millisecond {
			t.Errorf("annotation tries = %d in %v; want exactly 1 and no poll", len(runs.calls), time.Since(begin))
		}
	})

	t.Run("no run row named (system job of an older binary): nothing to mark", func(t *testing.T) {
		runs := &fakeRunAnnotator{}
		_ = NewEmailAlertJobHandler(&fakeMailTransport{err: echo}, runs, nil, nil)(context.Background(),
			jobs.Job{ID: "j0", Type: EmailAlertJobType, Payload: json.RawMessage(`{"to":"a@b.vn"}`), Attempts: 5, MaxAttempts: 5})
		if len(runs.calls) != 0 {
			t.Errorf("annotated %v without a run ref", runs.calls)
		}
	})
}

// #339: a delivered mail settles its run slot as sent; a failing attempt does not.
func TestEmailAlertJobHandler_DeliveredMarksTheSlotSent(t *testing.T) {
	payload := `{"to":"lan@nhakhoa.vn","subject":"s","body":"b","workflowId":"wf-1","runId":"run-1","actionIndex":1}`
	job := jobs.Job{ID: "j1", TenantID: "t1", Type: EmailAlertJobType, Payload: json.RawMessage(payload), Attempts: 1, MaxAttempts: 5}

	runs := &fakeRunAnnotator{}
	if err := NewEmailAlertJobHandler(&fakeMailTransport{}, runs, nil, nil)(context.Background(), job); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(runs.sent) != 1 || runs.sent[0] != "t1|run-1|1" {
		t.Fatalf("sent marks = %v; want one for tenant t1, run-1, slot 1", runs.sent)
	}

	retrying := &fakeRunAnnotator{}
	_ = NewEmailAlertJobHandler(&fakeMailTransport{err: errors.New("451 try again")}, retrying, nil, nil)(context.Background(), job)
	if len(retrying.sent) != 0 || len(retrying.calls) != 0 {
		t.Errorf("a retryable failure marked the run: sent=%v failed=%v", retrying.sent, retrying.calls)
	}
}

// #339: an enqueued run mail tells the engine its work is queued (the slot reads "queued"); a mail
// outside a run (no run id) does not.
func TestEnqueuingEmailSender_NotesTheQueuedDelivery(t *testing.T) {
	q := &fakeJobEnqueuer{}
	s := NewEnqueuingWorkflowEmailSender(q)

	ctx, note := workflow.WithDeliveryNote(workflow.WithRunRef(context.Background(),
		workflow.RunRef{TenantID: "t1", WorkflowID: "wf1", RunID: "run-1", ActionIndex: 1}))
	if err := s.SendEmail(ctx, "lan@nhakhoa.vn", "s", "b"); err != nil || !note.Queued() {
		t.Fatalf("in a run: err %v queued %v; want queued", err, note.Queued())
	}
	ctx, note = workflow.WithDeliveryNote(context.Background())
	if err := s.SendEmail(ctx, "lan@nhakhoa.vn", "s", "b"); err != nil || note.Queued() {
		t.Fatalf("outside a run: err %v queued %v; want not queued (no row to settle it)", err, note.Queued())
	}
}

func containsAllStr(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// ---- outbound_webhook durable delivery (DAILY-QUEUE #26) --------------------------------------

// TestEnqueuingWebhookQueue: the delivery is parked TENANT-PINNED (never NULL — the queue screens
// of ranks 31-33 filter by it), with the webhook's own attempt budget, and a payload that names
// where to re-read the config plus the body as a JSON STRING (so MySQL's JSON normalisation cannot
// reorder the bytes that get signed). No url/secret/headers key exists in the payload.
func TestEnqueuingWebhookQueue(t *testing.T) {
	q := &fakeJobEnqueuer{}
	body := []byte(`{"workflowId":"wf1","tenantId":"t1","data":{"z":1,"a":"<b>"}}`)
	err := NewEnqueuingWebhookQueue(q).EnqueueWebhook(context.Background(), workflow.WebhookDelivery{
		TenantID: "t1", WorkflowID: "wf1", ActionIndex: 2, Body: body,
	})
	if err != nil {
		t.Fatalf("EnqueueWebhook: %v", err)
	}
	if len(q.jobs) != 1 {
		t.Fatalf("enqueued %d jobs, want 1", len(q.jobs))
	}
	j := q.jobs[0]
	if j.Type != OutboundWebhookJobType || j.TenantID != "t1" || j.MaxAttempts != OutboundWebhookMaxAttempts {
		t.Fatalf("job = type %q tenant %q maxAttempts %d; want %q/t1/%d",
			j.Type, j.TenantID, j.MaxAttempts, OutboundWebhookJobType, OutboundWebhookMaxAttempts)
	}
	var raw map[string]any
	if err := json.Unmarshal(j.Payload, &raw); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	for _, k := range []string{"url", "secret", "headers"} {
		if _, ok := raw[k]; ok {
			t.Fatalf("payload carries %q — delivery config must be re-read from the rule, never copied into sys_jobs", k)
		}
	}
	var p outboundWebhookJobPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		t.Fatalf("payload shape: %v", err)
	}
	if p.WorkflowID != "wf1" || p.ActionIndex != 2 || p.Body != string(body) {
		t.Fatalf("payload = %+v, want wf1 / slot 2 / the body verbatim", p)
	}
}

func TestEnqueuingWebhookQueueErrors(t *testing.T) {
	d := workflow.WebhookDelivery{TenantID: "t1", WorkflowID: "wf1", Body: []byte(`{}`)}
	if err := NewEnqueuingWebhookQueue(nil).EnqueueWebhook(context.Background(), d); err == nil {
		t.Fatal("nil queue must error")
	}
	if err := NewEnqueuingWebhookQueue(&fakeJobEnqueuer{err: errors.New("db down")}).EnqueueWebhook(context.Background(), d); err == nil {
		t.Fatal("enqueue failure must propagate")
	}
	q := &fakeJobEnqueuer{}
	for _, bad := range []workflow.WebhookDelivery{
		{WorkflowID: "wf1", Body: []byte(`{}`)}, // no tenant: would land tenant-NULL
		{TenantID: "t1", Body: []byte(`{}`)},    // no rule: the handler could never find the config
	} {
		if err := NewEnqueuingWebhookQueue(q).EnqueueWebhook(context.Background(), bad); err == nil {
			t.Fatalf("delivery %+v must be rejected", bad)
		}
	}
	if len(q.jobs) != 0 {
		t.Fatalf("rejected deliveries were still enqueued: %d", len(q.jobs))
	}
}

// TestEnqueuingWebhookQueue_HasPendingWebhooks: the scheduler's back-pressure probe asks the store
// about THIS rule's outbound_webhook jobs in THIS tenant; a store error reaches the caller.
func TestEnqueuingWebhookQueue_HasPendingWebhooks(t *testing.T) {
	store := &fakeJobEnqueuer{pending: true}
	got, err := NewEnqueuingWebhookQueue(store).HasPendingWebhooks(context.Background(), "t1", "wf1")
	if err != nil || !got {
		t.Fatalf("HasPendingWebhooks = %v, %v; want true, nil", got, err)
	}
	if len(store.probes) != 1 || store.probes[0] != OutboundWebhookJobType+"|t1|wf1" {
		t.Fatalf("probes = %q, want the outbound_webhook jobs of t1/wf1", store.probes)
	}
	if _, err := NewEnqueuingWebhookQueue(&fakeJobEnqueuer{err: errors.New("db down")}).HasPendingWebhooks(context.Background(), "t1", "wf1"); err == nil {
		t.Fatal("a store error must propagate")
	}
	if _, err := NewEnqueuingWebhookQueue(nil).HasPendingWebhooks(context.Background(), "t1", "wf1"); err == nil {
		t.Fatal("nil store must error")
	}
}

// fakeDeliverer records the single-attempt deliveries the job handler makes.
type fakeDeliverer struct {
	calls     int
	req       workflow.WebhookRequest
	retryable bool
	err       error
}

func (f *fakeDeliverer) Deliver(_ context.Context, req workflow.WebhookRequest) (bool, error) {
	f.calls++
	f.req = req
	return f.retryable, f.err
}

// webhookRule is a persisted rule row whose actions are exactly acts.
func webhookRule(t *testing.T, tenant, id string, acts []workflow.Action) *workflow.WorkflowRecord {
	t.Helper()
	b, err := json.Marshal(acts)
	if err != nil {
		t.Fatalf("marshal actions: %v", err)
	}
	return &workflow.WorkflowRecord{ID: id, TenantID: tenant, EntityName: "orders", Trigger: "after_update", Actions: b, IsActive: true}
}

// TestOutboundWebhookJobHandler: one attempt per claim, config re-read from the rule at delivery
// time, and the retry decision handed to the queue — Permanent for everything no retry can fix.
func TestOutboundWebhookJobHandler(t *testing.T) {
	hook := workflow.Action{Type: workflow.ActionWebhook, URL: "https://erp.test/in", Secret: "k2",
		Headers: map[string]string{"X-Api-Key": "abc"}}
	rules := &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{
		"wf1": webhookRule(t, "t1", "wf1", []workflow.Action{
			{Type: workflow.ActionEmailAlert, To: "a@b.vn", Subject: "s", Body: "b"}, hook,
		}),
		"blank": webhookRule(t, "t1", "blank", []workflow.Action{{Type: workflow.ActionWebhook, URL: " "}}),
	}}
	const body = `{"workflowId":"wf1","data":{"status":"shipped"}}`
	mkJob := func(tenant, payload string) jobs.Job {
		return jobs.Job{ID: "job-1", Type: OutboundWebhookJobType, TenantID: tenant, Payload: json.RawMessage(payload)}
	}
	okPayload := `{"workflowId":"wf1","actionIndex":1,"body":` + strconv.Quote(body) + `}`

	cases := []struct {
		name          string
		job           jobs.Job
		rules         *fakeTimerRules
		deliverer     *fakeDeliverer
		wantErr       bool
		wantPermanent bool
		wantCalls     int
	}{
		{"delivered", mkJob("t1", okPayload), rules, &fakeDeliverer{}, false, false, 1},
		{"receiver 5xx: queue retries", mkJob("t1", okPayload), rules,
			&fakeDeliverer{retryable: true, err: errors.New("status 503")}, true, false, 1},
		{"receiver 4xx: failed now", mkJob("t1", okPayload), rules,
			&fakeDeliverer{err: errors.New("status 400")}, true, true, 1},
		{"rule deleted", mkJob("t1", `{"workflowId":"gone","actionIndex":0,"body":"{}"}`), rules,
			&fakeDeliverer{}, true, true, 0},
		{"other tenant's rule is not found", mkJob("t2", okPayload), rules, &fakeDeliverer{}, true, true, 0},
		{"rule store down: queue retries", mkJob("t1", okPayload), &fakeTimerRules{err: errors.New("db down")},
			&fakeDeliverer{}, true, false, 0},
		{"slot out of range", mkJob("t1", `{"workflowId":"wf1","actionIndex":7,"body":"{}"}`), rules,
			&fakeDeliverer{}, true, true, 0},
		{"slot is no longer a webhook", mkJob("t1", `{"workflowId":"wf1","actionIndex":0,"body":"{}"}`), rules,
			&fakeDeliverer{}, true, true, 0},
		{"url blanked since", mkJob("t1", `{"workflowId":"blank","actionIndex":0,"body":"{}"}`), rules,
			&fakeDeliverer{}, true, true, 0},
		{"no tenant on the row", mkJob("", okPayload), rules, &fakeDeliverer{}, true, true, 0},
		{"malformed payload", mkJob("t1", `not-json`), rules, &fakeDeliverer{}, true, true, 0},
		// #27: the job id IS the delivery id; a row without one could not be deduplicated.
		{"no job id", jobs.Job{Type: OutboundWebhookJobType, TenantID: "t1", Payload: json.RawMessage(okPayload)},
			rules, &fakeDeliverer{}, true, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NewOutboundWebhookJobHandler(tc.rules, nil, tc.deliverer)(context.Background(), tc.job)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error? %v", err, tc.wantErr)
			}
			if jobs.IsPermanent(err) != tc.wantPermanent {
				t.Fatalf("permanent = %v (err %v), want %v", jobs.IsPermanent(err), err, tc.wantPermanent)
			}
			if tc.deliverer.calls != tc.wantCalls {
				t.Fatalf("deliveries = %d, want %d", tc.deliverer.calls, tc.wantCalls)
			}
		})
	}

	t.Run("config comes from the rule, body from the job", func(t *testing.T) {
		d := &fakeDeliverer{}
		if err := NewOutboundWebhookJobHandler(rules, nil, d)(context.Background(), mkJob("t1", okPayload)); err != nil {
			t.Fatalf("handler: %v", err)
		}
		r := d.req
		if r.URL != hook.URL || r.SigningSecret != hook.Secret || r.Headers["X-Api-Key"] != "abc" || string(r.Body) != body {
			t.Fatalf("delivered url=%q secret=%q headers=%v body=%q; want the rule's slot-1 config and the frozen body",
				r.URL, r.SigningSecret, r.Headers, r.Body)
		}
		// #27: every attempt of this job — retry, reaped claim, resend — names the same delivery.
		if r.DeliveryID != "job-1" {
			t.Fatalf("delivery id = %q, want the job id job-1", r.DeliveryID)
		}
		// specs/050 S2a: the queue path builds the SAME request as the inline path — an error it
		// returns names the origin, never the path/query/userinfo.
		if r.DisplayURL != "https://erp.test" {
			t.Fatalf("display url = %q, want the bare origin https://erp.test", r.DisplayURL)
		}
	})

	t.Run("not wired", func(t *testing.T) {
		if err := NewOutboundWebhookJobHandler(nil, nil, nil)(context.Background(), mkJob("t1", okPayload)); err == nil {
			t.Fatal("an unwired handler must error")
		}
	})
}
