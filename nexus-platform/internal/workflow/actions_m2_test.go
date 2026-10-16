package workflow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------------------------
// {{field}} template rendering (pure).
// ---------------------------------------------------------------------------------------------

func TestRenderTemplate(t *testing.T) {
	data := map[string]any{
		"name":   "Acme",
		"amount": float64(1500),
		"status": "won",
	}
	cases := []struct {
		name string
		tmpl string
		want string
	}{
		{"no placeholder", "hello world", "hello world"},
		{"single", "Deal {{name}} closed", "Deal Acme closed"},
		{"spaces inside braces", "Deal {{ name }} closed", "Deal Acme closed"},
		{"numeric renders cleanly", "Amount: {{amount}}", "Amount: 1500"},
		{"multiple", "{{name}} is {{status}}", "Acme is won"},
		{"unknown field → empty", "x={{missing}}=y", "x==y"},
		{"empty template", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderTemplate(tc.tmpl, data); got != tc.want {
				t.Errorf("renderTemplate(%q) = %q; want %q", tc.tmpl, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// email_alert action (fake sender).
// ---------------------------------------------------------------------------------------------

type fakeEmailSender struct {
	mu    sync.Mutex
	calls []struct{ to, subject, body string }
	err   error
}

func (f *fakeEmailSender) SendEmail(_ context.Context, to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, struct{ to, subject, body string }{to, subject, body})
	return nil
}

func TestRunEmailAlert_RendersAndSends(t *testing.T) {
	email := &fakeEmailSender{}
	a := Action{
		Type:    ActionEmailAlert,
		To:      "{{ownerEmail}}",
		Subject: "Deal {{name}} is {{status}}",
		Body:    "Amount {{amount}}",
	}
	evt := Event{Data: map[string]any{
		"ownerEmail": "boss@acme.test",
		"name":       "BigCo",
		"status":     "won",
		"amount":     float64(9000),
	}}
	if err := runEmailAlert(context.Background(), evt, a, email, nil); err != nil {
		t.Fatalf("runEmailAlert err = %v", err)
	}
	if len(email.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(email.calls))
	}
	c := email.calls[0]
	if c.to != "boss@acme.test" || c.subject != "Deal BigCo is won" || c.body != "Amount 9000" {
		t.Errorf("send = %+v; want rendered to/subject/body", c)
	}
}

func TestRunEmailAlert_NilSender(t *testing.T) {
	if err := runEmailAlert(context.Background(), Event{}, Action{To: "a@b.c"}, nil, nil); err == nil {
		t.Fatalf("nil sender must error")
	}
}

func TestRunEmailAlert_EmptyRecipientAfterRender(t *testing.T) {
	email := &fakeEmailSender{}
	// "to" renders to empty (unknown field) → error, no send.
	a := Action{Type: ActionEmailAlert, To: "{{missing}}", Subject: "s", Body: "b"}
	if err := runEmailAlert(context.Background(), Event{Data: map[string]any{}}, a, email, nil); err == nil {
		t.Fatalf("empty recipient after render must error")
	}
	if len(email.calls) != 0 {
		t.Errorf("no send expected on empty recipient, got %d", len(email.calls))
	}
}

// workflow-run-error-pii (queue #85): a failed send used to quote the RENDERED recipient in the
// error, and the dispatcher copies that text verbatim into sys_workflow_runs.error AND into
// actions_result[].error. With To = {{email_ca_nhan}} that is a personal address — possibly one the
// admin flagged sensitive — landing in a second, unguarded log. Measured on a real server before
// this fix (JOBS_ENABLED=false, scripts/acceptance-workflow-trail.py): the run row read
// `workflow.runEmailAlert: gửi tới "<the contact's personal address>" thất bại: ...` in BOTH
// `error` and `actionsResult`. With the durable queue on (the default) the run reads success and
// the failure lands in sys_jobs.last_error instead — see TestEmailAlertJobHandler.
//
// The fake error mimics what an SMTP server really says at RCPT TO — it echoes the address — so
// masking only the prefix would still leak through the wrapped cause.
func TestEmailAlertSendError_NeverRecordsRenderedRecipient(t *testing.T) {
	const personal = "nguyen.thi.canhan@gmail.com"
	sentinel := errors.New("notify: mail not configured")
	smtpEcho := fmt.Errorf("notify.SMTPSender.Send: rcpt to: 550 5.1.1 <%s>: Recipient address rejected: %w", personal, sentinel)

	loader := &fakeLoader{workflows: []Workflow{{
		ID: "wf1", TenantID: "t1", EntityName: "contacts", Trigger: TriggerAfterCreate, IsActive: true,
		Actions: []Action{{Type: ActionEmailAlert, To: "{{email_ca_nhan}}", Subject: "Chào {{first_name}}", Body: "b"}},
	}}}
	rec := &fakeRunRecorder{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, &fakeMutator{}, nil, zap.NewNop()).
		WithEmailSender(&fakeEmailSender{err: smtpEcho}).
		WithRunRecorder(rec)
	d.Emit(context.Background(), Event{
		TenantID: "t1", EntityName: "contacts", RecordID: "r1", Trigger: TriggerAfterCreate,
		Data: map[string]any{"email_ca_nhan": personal, "first_name": "Lan"},
	})
	drainDispatcher(t, d)

	runs := rec.all()
	if len(runs) != 1 || runs[0].Status != RunFailed {
		t.Fatalf("want exactly one FAILED run, got %+v", runs)
	}
	if strings.Contains(runs[0].Error, personal) {
		t.Errorf("sys_workflow_runs.error carries the personal address:\n  %s", runs[0].Error)
	}
	if strings.Contains(string(runs[0].ActionsResult), personal) {
		t.Errorf("sys_workflow_runs.actions_result carries the personal address:\n  %s", runs[0].ActionsResult)
	}
	// Masking must not make the failure undiagnosable: the admin still sees WHICH recipient
	// template failed (the rule's config, not the record's data) and the transport's reason.
	if !containsAll(runs[0].Error, "{{email_ca_nhan}}", "Recipient address rejected") {
		t.Errorf("error lost its diagnostic value: %q", runs[0].Error)
	}
}

func TestRunEmailAlert_SendErrorKeepsCauseChain(t *testing.T) {
	sentinel := errors.New("mail not configured")
	a := Action{Type: ActionEmailAlert, To: "{{email}}", Subject: "s", Body: "b"}
	err := runEmailAlert(context.Background(), Event{Data: map[string]any{"email": "a.b@c.vn"}}, a,
		&fakeEmailSender{err: fmt.Errorf("rcpt <a.b@c.vn>: %w", sentinel)}, nil)
	if err == nil {
		t.Fatal("send failure must surface as an error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("masking broke the error chain: errors.Is(err, cause) = false for %v", err)
	}
	if strings.Contains(err.Error(), "a.b@c.vn") {
		t.Errorf("error text carries the rendered recipient: %q", err.Error())
	}
}

func TestMaskRecipientError(t *testing.T) {
	tests := []struct {
		name, recipient, cause string
		leaks                  []string // substrings that must NOT survive
		keeps                  []string // substrings that must survive
	}{
		{name: "echoed address", recipient: "lan@gmail.com",
			cause: "550 <lan@gmail.com>: rejected", leaks: []string{"lan@gmail.com"}, keeps: []string{"550", "rejected"}},
		{name: "each address of a list", recipient: "a.b@x.vn, c.d@y.vn",
			cause: "rcpt <c.d@y.vn> refused", leaks: []string{"c.d@y.vn"}, keeps: []string{"refused"}},
		// Servers normalise case (Postfix lowercases the domain, some the whole address), so an
		// exact-bytes replace would let "Lan.Nguyen@Gmail.com" leak back as "lan.nguyen@gmail.com".
		{name: "echo in a different case", recipient: "Lan.Nguyen@Gmail.com",
			cause: "550 <lan.nguyen@gmail.com>: no such user", leaks: []string{"lan.nguyen@gmail.com"}, keeps: []string{"no such user"}},
		{name: "fragment too short to be an address is left alone", recipient: "ab",
			cause: "tab lab", keeps: []string{"tab lab"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaskRecipientError("p", tt.recipient, errors.New(tt.cause)).Error()
			for _, s := range tt.leaks {
				if strings.Contains(strings.ToLower(got), strings.ToLower(s)) {
					t.Errorf("%q leaked into %q", s, got)
				}
			}
			for _, s := range tt.keeps {
				if !strings.Contains(got, s) {
					t.Errorf("%q lost from %q", s, got)
				}
			}
		})
	}
}

// A literal recipient is the admin's own configuration, not record data — naming it is what lets
// the admin see which mailbox bounced, so it stays in the message.
func TestRunEmailAlert_LiteralRecipientStaysNamed(t *testing.T) {
	a := Action{Type: ActionEmailAlert, To: "sales@example.com", Subject: "s", Body: "b"}
	err := runEmailAlert(context.Background(), Event{Data: map[string]any{}}, a,
		&fakeEmailSender{err: errors.New("dial: connection refused")}, nil)
	if err == nil || !strings.Contains(err.Error(), "sales@example.com") {
		t.Errorf("literal recipient should stay named in the error, got %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// outbound_webhook action + HTTPWebhookSender (httptest).
// ---------------------------------------------------------------------------------------------

func TestHTTPWebhookSender_PostSuccessWithHMAC(t *testing.T) {
	const secret = "s3cr3t"
	var gotSig string
	var gotBody []byte
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get(SignatureHeader)
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 3, 10*time.Millisecond)
	payload := []byte(`{"hello":"world"}`)
	if err := sender.Post(context.Background(), WebhookRequest{URL: srv.URL, SigningSecret: secret, Headers: map[string]string{"X-Custom": "v"}, Body: payload}); err != nil {
		t.Fatalf("Post err = %v", err)
	}

	// Verify the HMAC the receiver would compute matches.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Errorf("signature = %q; want %q", gotSig, want)
	}
	if string(gotBody) != string(payload) {
		t.Errorf("body = %q; want %q", gotBody, payload)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q; want application/json", gotContentType)
	}
}

// bareRequest is an unsigned request with an empty JSON body.
func bareRequest(url string) WebhookRequest {
	return WebhookRequest{URL: url, Body: []byte(`{}`)}
}

func TestHTTPWebhookSender_NoSecretNoSignatureHeader(t *testing.T) {
	var had []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{SignatureHeader, TimestampHeader, SignatureV2Header, SignatureV2PreviousHeader} {
			if _, ok := r.Header[h]; ok {
				had = append(had, h)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 3, 10*time.Millisecond)
	if err := sender.Post(context.Background(), bareRequest(srv.URL)); err != nil {
		t.Fatalf("Post err = %v", err)
	}
	if len(had) != 0 {
		t.Errorf("no secret → no signing header expected, got %v", had)
	}
}

func TestHTTPWebhookSender_RetriesOn5xxThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError) // transient
			return
		}
		w.WriteHeader(http.StatusOK) // 3rd attempt succeeds
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 3, 5*time.Millisecond)
	if err := sender.Post(context.Background(), bareRequest(srv.URL)); err != nil {
		t.Fatalf("Post should succeed on 3rd attempt: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d; want 3 (2 failures + 1 success)", got)
	}
}

func TestHTTPWebhookSender_ExhaustsRetriesOn5xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway) // always 502
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 3, 5*time.Millisecond)
	if err := sender.Post(context.Background(), bareRequest(srv.URL)); err == nil {
		t.Fatalf("Post should fail after exhausting retries")
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d; want 3 (maxRetries)", got)
	}
}

func TestHTTPWebhookSender_NoRetryOn4xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest) // permanent 400
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 3, 5*time.Millisecond)
	if err := sender.Post(context.Background(), bareRequest(srv.URL)); err == nil {
		t.Fatalf("Post should fail on 400")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d; want 1 (4xx is non-retryable)", got)
	}
}

func TestHTTPWebhookSender_429IsRetryable(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 3, 5*time.Millisecond)
	if err := sender.Post(context.Background(), bareRequest(srv.URL)); err != nil {
		t.Fatalf("429 then 200 should succeed: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d; want 2 (429 retried)", got)
	}
}

// ---------------------------------------------------------------------------------------------
// runWebhook marshals + routes through the sender.
// ---------------------------------------------------------------------------------------------

type fakeWebhookSender struct {
	mu    sync.Mutex
	req   WebhookRequest
	err   error
	calls int
}

func (f *fakeWebhookSender) Post(_ context.Context, req WebhookRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.req = req
	return nil
}

func TestRunWebhook_BuildsPayloadAndPosts(t *testing.T) {
	wh := &fakeWebhookSender{}
	wf := Workflow{ID: "wf9"}
	a := Action{Type: ActionWebhook, URL: " https://u:p@hook.test/x?token=abc ", Secret: "k", Headers: map[string]string{"Authorization": "Bearer t"}}
	evt := Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate, Data: map[string]any{"status": "won"}}
	if err := runWebhook(context.Background(), wf, evt, a, 0, actionDeps{webhook: wh}); err != nil {
		t.Fatalf("runWebhook err = %v", err)
	}
	r := wh.req
	if wh.calls != 1 || r.URL != "https://u:p@hook.test/x?token=abc" || r.SigningSecret != "k" || r.Headers["Authorization"] != "Bearer t" {
		t.Fatalf("post call = url=%q secret=%q headers=%v calls=%d; want the action's own url/secret/headers once", r.URL, r.SigningSecret, r.Headers, wh.calls)
	}
	if r.DisplayURL != "https://hook.test" || r.PrevSigningSecret != "" {
		t.Fatalf("display=%q prev=%q; want the bare origin and no previous key (only a vault key has one)", r.DisplayURL, r.PrevSigningSecret)
	}
	if len(r.Body) == 0 || !containsAll(string(r.Body), `"recordId":"r1"`, `"tenantId":"t1"`, `"trigger":"after_update"`) {
		t.Errorf("payload missing expected fields: %s", r.Body)
	}
}

func TestRunWebhook_NilSenderAndEmptyURL(t *testing.T) {
	if err := runWebhook(context.Background(), Workflow{}, Event{}, Action{URL: "https://x"}, 0, actionDeps{}); err == nil {
		t.Fatalf("nil sender must error")
	}
	if err := runWebhook(context.Background(), Workflow{}, Event{}, Action{URL: ""}, 0, actionDeps{webhook: &fakeWebhookSender{}}); err == nil {
		t.Fatalf("empty url must error")
	}
	// The durable path refuses a blank URL too: parking a job that can only fail would just move
	// the misconfiguration out of the run history and into the queue.
	q := &fakeWebhookQueue{}
	if err := runWebhook(context.Background(), Workflow{}, Event{}, Action{URL: " "}, 0, actionDeps{webhookQueue: q}); err == nil || len(q.parked) != 0 {
		t.Fatalf("empty url must error before enqueue, err=%v parked=%d", err, len(q.parked))
	}
}

func TestRunWebhook_SenderErrorPropagates(t *testing.T) {
	wh := &fakeWebhookSender{err: errors.New("down")}
	a := Action{Type: ActionWebhook, URL: "https://hook.test/x"}
	if err := runWebhook(context.Background(), Workflow{ID: "w"}, Event{}, a, 0, actionDeps{webhook: wh}); err == nil {
		t.Fatalf("sender error must propagate (logged fail-open by caller)")
	}
	q := &fakeWebhookQueue{err: errors.New("db down")}
	if err := runWebhook(context.Background(), Workflow{ID: "w"}, Event{}, a, 0, actionDeps{webhookQueue: q}); err == nil {
		t.Fatalf("enqueue error must propagate (the run history must say the delivery was never parked)")
	}
}

// ---------------------------------------------------------------------------------------------
// Durable delivery (DAILY-QUEUE #26): with a queue wired, the action PARKS the delivery instead of
// posting from the dispatcher goroutine.
// ---------------------------------------------------------------------------------------------

type fakeWebhookQueue struct {
	mu     sync.Mutex
	parked []WebhookDelivery
	err    error

	pending  bool     // what HasPendingWebhooks answers
	probeErr error    // …or the error it fails with
	probes   []string // "tenant|workflow" per probe
}

func (f *fakeWebhookQueue) HasPendingWebhooks(_ context.Context, tenantID, workflowID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes = append(f.probes, tenantID+"|"+workflowID)
	return f.pending, f.probeErr
}

func (f *fakeWebhookQueue) EnqueueWebhook(_ context.Context, d WebhookDelivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.parked = append(f.parked, d)
	return nil
}

// TestRunWebhook_QueueWiredParksDeliveryInsteadOfPosting: the queue gets the tenant, the rule, the
// action slot and the EXACT body the inline path would have posted (byte-identical to the dry-run
// preview); the HTTP sender is never touched. WebhookDelivery has no Secret/URL/Headers field at
// all, so a signing key cannot reach sys_jobs by construction — the compiler is the test for that.
func TestRunWebhook_QueueWiredParksDeliveryInsteadOfPosting(t *testing.T) {
	wh := &fakeWebhookSender{}
	q := &fakeWebhookQueue{}
	wf := Workflow{ID: "wf9"}
	a := Action{Type: ActionWebhook, URL: "https://hook.test/x", Secret: "k", Headers: map[string]string{"Authorization": "Bearer t"}}
	// Stamped, as every mutation event is by Emit — an unstamped event takes the fire time, which
	// differs between two calls by construction.
	evt := Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate, Data: map[string]any{"status": "won"},
		OccurredAt: time.Date(2026, 9, 29, 7, 0, 0, 0, time.UTC)}

	if err := runWebhook(context.Background(), wf, evt, a, 3, actionDeps{webhook: wh, webhookQueue: q}); err != nil {
		t.Fatalf("runWebhook err = %v", err)
	}
	if wh.calls != 0 {
		t.Fatalf("sender called %d times; with a queue wired the dispatcher must not post inline", wh.calls)
	}
	if len(q.parked) != 1 {
		t.Fatalf("parked %d deliveries, want 1", len(q.parked))
	}
	d := q.parked[0]
	if d.TenantID != "t1" || d.WorkflowID != "wf9" || d.ActionIndex != 3 {
		t.Fatalf("delivery = tenant %q rule %q slot %d; want t1/wf9/3", d.TenantID, d.WorkflowID, d.ActionIndex)
	}
	want, err := WebhookPayloadPreview("wf9", evt)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if string(d.Body) != string(want) {
		t.Fatalf("body = %s\nwant the dry-run preview byte for byte: %s", d.Body, want)
	}
}

// TestRunActions_WebhookDeliveryCarriesItsActionSlot: the slot is the action's position in the
// rule, so the job handler re-reads THIS action's url/secret/headers and not a sibling's.
func TestRunActions_WebhookDeliveryCarriesItsActionSlot(t *testing.T) {
	q := &fakeWebhookQueue{}
	wf := Workflow{ID: "wf1", Actions: []Action{
		{Type: ActionEmailAlert, To: "a@b.vn", Subject: "s", Body: "b"},
		{Type: ActionWebhook, URL: "https://one.test"},
		{Type: ActionWebhook, URL: "https://two.test"},
	}}
	results, err := runActions(context.Background(), wf, Event{TenantID: "t1"},
		actionDeps{email: &fakeEmailSender{}, webhookQueue: q})
	if err != nil {
		t.Fatalf("runActions err = %v (results %+v)", err, results)
	}
	if len(q.parked) != 2 || q.parked[0].ActionIndex != 1 || q.parked[1].ActionIndex != 2 {
		t.Fatalf("parked = %+v; want slots 1 and 2", q.parked)
	}
}

// TestHTTPWebhookSender_DeliverIsOneAttempt: the job handler's entry point makes exactly ONE POST
// and says whether a retry could help — the queue owns the retry. Calling Post from a job would be
// retry × retry (3 in-process × 10 queue attempts = 30 POSTs) and would pin a worker in time.After.
func TestHTTPWebhookSender_DeliverIsOneAttempt(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		wantErr       bool
		wantRetryable bool
	}{
		{"2xx delivered", http.StatusNoContent, false, false},
		{"5xx retryable", http.StatusServiceUnavailable, true, true},
		{"429 retryable", http.StatusTooManyRequests, true, true},
		{"4xx permanent", http.StatusGone, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			var gotSig string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				gotSig = r.Header.Get(SignatureHeader)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			body := []byte(`{"a":1}`)
			sender := NewHTTPWebhookSender(2*time.Second, 3, time.Millisecond)
			retryable, err := sender.Deliver(context.Background(), WebhookRequest{URL: srv.URL, SigningSecret: "k", Body: body, DeliveryID: "job-1"})
			if (err != nil) != tc.wantErr || retryable != tc.wantRetryable {
				t.Fatalf("Deliver = (retryable %v, err %v); want (%v, err? %v)", retryable, err, tc.wantRetryable, tc.wantErr)
			}
			if n := atomic.LoadInt32(&hits); n != 1 {
				t.Fatalf("POSTs = %d, want exactly 1 (the queue owns the retry)", n)
			}
			mac := hmac.New(sha256.New, []byte("k"))
			mac.Write(body)
			if want := hex.EncodeToString(mac.Sum(nil)); gotSig != want {
				t.Fatalf("signature = %q, want the unchanged body-only HMAC %q", gotSig, want)
			}
		})
	}
}

// TestHTTPWebhookSender_TransportErrorOmitsURL: a webhook URL often IS a credential (Slack/Teams
// put the token in the path). A dial failure's text lands in sys_jobs.last_error — a cross-tenant
// table — and in the queue's retry log line, so it must name the failure, not the URL.
func TestHTTPWebhookSender_TransportErrorOmitsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	target := srv.URL + "/services/T0/B0/SECRETTOKEN"
	srv.Close() // connection refused from here on

	sender := NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond)
	retryable, err := sender.Deliver(context.Background(), WebhookRequest{URL: target, Body: []byte(`{}`), DeliveryID: "job-1"})
	if err == nil || !retryable {
		t.Fatalf("Deliver to a closed port = (retryable %v, err %v); want a retryable error", retryable, err)
	}
	if strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Fatalf("transport error leaks the URL: %v", err)
	}
	if err := sender.Post(context.Background(), bareRequest(target)); err == nil || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Fatalf("inline Post error leaks the URL (or did not fail): %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// Delivery id (DAILY-QUEUE #27): the queue is at-least-once, so every POST names its delivery and
// a receiver can drop a repeat.
// ---------------------------------------------------------------------------------------------

// TestHTTPWebhookSender_DeliverCarriesDeliveryID: the durable path sends the id it is given (the
// job id) on X-Nexus-Delivery-Id, and a rule's own custom headers cannot overwrite it — a fixed
// value copied into a rule would make the receiver drop every delivery after the first as a
// duplicate.
func TestHTTPWebhookSender_DeliverCarriesDeliveryID(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(DeliveryIDHeader))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 3, time.Millisecond)
	custom := map[string]string{DeliveryIDHeader: "copied-from-an-old-rule", "X-Api-Key": "abc"}
	for i := 0; i < 2; i++ { // the same job delivered twice (a retry, a reaped claim, a resend)
		if _, err := sender.Deliver(context.Background(), WebhookRequest{URL: srv.URL, Headers: custom, Body: []byte(`{}`), DeliveryID: "job-123"}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
	}
	if len(got) != 2 || got[0] != "job-123" || got[1] != "job-123" {
		t.Fatalf("%s per POST = %q, want the job id on both", DeliveryIDHeader, got)
	}
}

// TestHTTPWebhookSender_PostDeliveryIDStableAcrossRetries: the inline path (JOBS_ENABLED=false)
// honours the same contract — one id per Post call, repeated on each of its in-process retries,
// and a different event gets a different id.
func TestHTTPWebhookSender_PostDeliveryIDStableAcrossRetries(t *testing.T) {
	var mu sync.Mutex
	var got []string
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get(DeliveryIDHeader))
		mu.Unlock()
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 3, time.Millisecond)
	if err := sender.Post(context.Background(), bareRequest(srv.URL)); err != nil {
		t.Fatalf("Post (3rd attempt succeeds): %v", err)
	}
	if err := sender.Post(context.Background(), bareRequest(srv.URL)); err != nil {
		t.Fatalf("second Post: %v", err)
	}
	if len(got) != 4 || got[0] == "" || got[0] != got[1] || got[1] != got[2] {
		t.Fatalf("delivery ids = %q, want one non-empty id repeated on the 3 attempts of the first event", got)
	}
	if got[3] == "" || got[3] == got[0] {
		t.Fatalf("second event reused id %q; each event needs its own", got[3])
	}
}

// ---------------------------------------------------------------------------------------------
// Signature V2 + timestamp and redacted errors (specs/050 S2a, T029). A receiver can reject a
// replayed request only if the signature covers WHEN it was sent, and every signing header must
// be computed inside one attempt so the durable job handler (which calls attempt once per claim,
// via Deliver) and the inline retry loop can never disagree.
// ---------------------------------------------------------------------------------------------

// goldenBody / goldenSecret / goldenV1 pin X-Nexus-Signature byte for byte. goldenV1 was produced
// by the sender as it was BEFORE S2a (body-only HMAC), cross-checked with Python's hmac, and recomputed
// with Python's hmac when the sample address in goldenBody changed — V1
// must never change, receivers in production verify it today.
const (
	goldenSecret = "nexus-golden-secret"
	goldenBody   = `{"workflowId":"wf-golden","tenantId":"t1","entityName":"crm_contacts","recordId":"r1","trigger":"after_create","occurredAt":"2026-09-30T00:00:00.000000Z","data":{"email":"lan@example.com"}}`
	goldenV1     = "dcd97195ef1dd238908772d1452f169b697d38dc6d89d1214afdef9f3770bd86"
)

// wantV2 is what a receiver recomputes: hex(HMAC-SHA256(key, ts + "." + rawBody)).
func wantV2(key, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// signedHit is the signing headers of one request the test server received.
type signedHit struct{ v1, ts, v2, v2prev string }

func signingHeaders(r *http.Request) signedHit {
	return signedHit{r.Header.Get(SignatureHeader), r.Header.Get(TimestampHeader),
		r.Header.Get(SignatureV2Header), r.Header.Get(SignatureV2PreviousHeader)}
}

func TestHTTPWebhookSender_V1SignatureByteIdentical(t *testing.T) {
	var got signedHit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = signingHeaders(r)
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond)
	for _, path := range []string{"Post", "Deliver"} {
		req := WebhookRequest{URL: srv.URL, Body: []byte(goldenBody), SigningSecret: goldenSecret, DeliveryID: "d-1"}
		var err error
		if path == "Post" {
			err = sender.Post(context.Background(), req)
		} else {
			_, err = sender.Deliver(context.Background(), req)
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got.v1 != goldenV1 {
			t.Fatalf("%s: %s = %q, want the pre-S2a body-only HMAC %q byte for byte", path, SignatureHeader, got.v1, goldenV1)
		}
	}
}

// TestHTTPWebhookSender_V2TimestampPerAttempt: a retried request carries a FRESH timestamp and a
// V2 over that timestamp — a timestamp fixed at the first attempt would age past the receiver's
// 5-minute window during a long backoff and every retry would be rejected as a replay.
func TestHTTPWebhookSender_V2TimestampPerAttempt(t *testing.T) {
	var mu sync.Mutex
	var hits []signedHit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, signingHeaders(r))
		n := len(hits)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 3, time.Millisecond)
	clock := time.Unix(1_790_000_000, 0)
	sender.now = func() time.Time { // every read of the clock is 7 s later than the previous one
		clock = clock.Add(7 * time.Second)
		return clock
	}
	body := []byte(goldenBody)
	if err := sender.Post(context.Background(), WebhookRequest{URL: srv.URL, Body: body, SigningSecret: "k"}); err != nil {
		t.Fatalf("Post (2nd attempt succeeds): %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("attempts = %d, want 2", len(hits))
	}
	if hits[0].ts == "" || hits[0].ts == hits[1].ts {
		t.Fatalf("timestamps = %q, %q; want a fresh one per attempt", hits[0].ts, hits[1].ts)
	}
	for i, h := range hits {
		if want := wantV2("k", h.ts, body); h.v2 != want {
			t.Fatalf("attempt %d: V2 = %q, want HMAC(k, %s + \".\" + body) = %q", i+1, h.v2, h.ts, want)
		}
		if want := signBody("k", body); h.v1 != want {
			t.Fatalf("attempt %d: V1 = %q, want the body-only %q", i+1, h.v1, want)
		}
	}
}

// TestHTTPWebhookSender_AttemptComputesAllSignatureHeaders pins the queue contract (#26): ONE call
// of attempt — what Deliver does per job claim — sends V1, the timestamp, V2 and, in a rotation
// grace, V2 under the previous key. Nothing is computed outside attempt that a caller could forget.
func TestHTTPWebhookSender_AttemptComputesAllSignatureHeaders(t *testing.T) {
	var got signedHit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = signingHeaders(r)
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond)
	body := []byte(goldenBody)
	before := time.Now().Unix()
	if _, err := sender.attempt(context.Background(), WebhookRequest{URL: srv.URL, Body: body,
		SigningSecret: goldenSecret, PrevSigningSecret: "the-old-key", DeliveryID: "d-1"}); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if got.v1 != goldenV1 {
		t.Fatalf("V1 = %q, want %q", got.v1, goldenV1)
	}
	ts, err := strconv.ParseInt(got.ts, 10, 64)
	if err != nil || ts < before || ts > time.Now().Unix() {
		t.Fatalf("%s = %q, want the Unix second of this attempt", TimestampHeader, got.ts)
	}
	if want := wantV2(goldenSecret, got.ts, body); got.v2 != want {
		t.Fatalf("V2 = %q, want %q", got.v2, want)
	}
	if want := wantV2("the-old-key", got.ts, body); got.v2prev != want {
		t.Fatalf("V2-Previous = %q, want HMAC under the previous key %q", got.v2prev, want)
	}
}

// TestHTTPWebhookSender_LegacyInlineSecretGetsV2: a rule whose key sits on the action itself (set
// by API before the vault) gets V2 + timestamp ON TOP of what it sent before — V1 and its own
// headers go out unchanged, so a receiver verifying V1 today does not notice the upgrade.
func TestHTTPWebhookSender_LegacyInlineSecretGetsV2(t *testing.T) {
	var got signedHit
	var auth, custom string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, auth, custom = signingHeaders(r), r.Header.Get("Authorization"), r.Header.Get("X-Doi-Tac")
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	a := Action{Type: ActionWebhook, URL: srv.URL + "/legacy", Secret: "inline-key",
		Headers: map[string]string{"Authorization": "Bearer partner-token", "X-Doi-Tac": "northwind"}}
	evt := Event{TenantID: "t1", EntityName: "crm_contacts", RecordID: "r1", Trigger: TriggerAfterCreate,
		OccurredAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)}
	deps := actionDeps{webhook: NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond)}
	if err := runWebhook(context.Background(), Workflow{ID: "wf1"}, evt, a, 0, deps); err != nil {
		t.Fatalf("runWebhook: %v", err)
	}
	if auth != "Bearer partner-token" || custom != "northwind" {
		t.Fatalf("rule headers = Authorization %q, X-Doi-Tac %q; want them unchanged", auth, custom)
	}
	if got.v1 != signBody("inline-key", body) {
		t.Fatalf("V1 = %q, want the body-only HMAC", got.v1)
	}
	if got.ts == "" || got.v2 != wantV2("inline-key", got.ts, body) {
		t.Fatalf("ts %q V2 %q; want both, V2 over ts + \".\" + body", got.ts, got.v2)
	}
	if got.v2prev != "" {
		t.Fatalf("V2-Previous = %q on an action with no previous key", got.v2prev)
	}
}

// TestHTTPWebhookSender_ErrorRedactsURL: the delivery error is copied into sys_workflow_runs,
// actions_result, the server log and sys_jobs.last_error (a cross-tenant table). A url action's
// target may carry a credential anywhere after the host — user:pass@, ?token=, or the path itself
// (Slack/Telegram) — so the error names the ORIGIN only. The receiver's HTTP status stays verbatim:
// it is what the operator needs to see.
func TestHTTPWebhookSender_ErrorRedactsURL(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downHost := strings.TrimPrefix(down.URL, "http://")
	down.Close() // connection refused from here on
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer busy.Close()
	busyHost := strings.TrimPrefix(busy.URL, "http://")

	secrets := []string{"pa55word", "abc123", "SECRETPATH", "user:"}
	cases := []struct {
		name, url, wantIn string
	}{
		{"connection refused", "http://user:pa55word@" + downHost + "/hook/SECRETPATH?token=abc123", "http://" + downHost},
		{"dns failure", "https://user:pa55word@nexus-webhook-test.invalid/SECRETPATH?token=abc123", "https://nexus-webhook-test.invalid"},
		{"receiver 503", "http://user:pa55word@" + busyHost + "/SECRETPATH?token=abc123", "status 503"},
		{"unparsable url", "http://user:pa55word@[::1/SECRETPATH?token=abc123", "build request"},
	}
	sender := NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := WebhookRequestFromURLAction(Action{Type: ActionWebhook, URL: tc.url}, []byte(`{}`))
			req.DeliveryID = "d-1"
			_, derr := sender.Deliver(context.Background(), req)
			perr := sender.Post(context.Background(), req)
			bare := req
			bare.DisplayURL = "" // a caller that forgot DisplayURL still cannot leak the target
			_, berr := sender.Deliver(context.Background(), bare)
			for _, err := range []error{derr, perr, berr} {
				if err == nil {
					t.Fatalf("want a delivery error")
				}
				for _, s := range secrets {
					if strings.Contains(err.Error(), s) {
						t.Fatalf("error leaks %q: %v", s, err)
					}
				}
				if !strings.Contains(err.Error(), tc.wantIn) {
					t.Fatalf("error %q does not name %q", err, tc.wantIn)
				}
			}
			if tc.name == "receiver 503" && !strings.Contains(derr.Error(), "http://"+busyHost) {
				t.Fatalf("status error %q does not name the origin", derr)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// occurredAt (review of #26): retries back off by attempt count, so an event parked during an
// outage can reach the receiver AFTER a newer event of the same record. The body names when its
// event happened so the receiver can drop the older snapshot instead of keeping it.
// ---------------------------------------------------------------------------------------------

// webhookOccurredAt pulls occurredAt out of a webhook body.
func webhookOccurredAt(t *testing.T, body []byte) string {
	t.Helper()
	var p struct {
		OccurredAt *string `json:"occurredAt"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	if p.OccurredAt == nil {
		t.Fatalf("body has no occurredAt: %s", body)
	}
	return *p.OccurredAt
}

// TestRunWebhook_BodyCarriesOccurredAt: the event's instant rides the body in a fixed-width UTC
// form (so a receiver comparing the strings orders them right); an event nobody stamped gets the
// fire time, never year 1 — a zero instant would make "drop the older one" drop it forever.
func TestRunWebhook_BodyCarriesOccurredAt(t *testing.T) {
	stamped := time.Date(2026, 9, 29, 1, 2, 3, 4000, time.FixedZone("ICT", 7*3600))
	cases := []struct {
		name  string
		at    time.Time
		exact string
	}{
		{"stamped event keeps its instant, in UTC, micro-second fixed width", stamped, "2026-09-28T18:02:03.000004Z"},
		{"unstamped event gets the fire time", time.Time{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeWebhookQueue{}
			evt := Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate, OccurredAt: tc.at}
			before := time.Now().UTC()
			if err := runWebhook(context.Background(), Workflow{ID: "wf9"}, evt, Action{URL: "https://hook.test"}, 0, actionDeps{webhookQueue: q}); err != nil {
				t.Fatalf("runWebhook: %v", err)
			}
			after := time.Now().UTC()
			got := webhookOccurredAt(t, q.parked[0].Body)
			if tc.exact != "" {
				if got != tc.exact {
					t.Fatalf("occurredAt = %q, want %q", got, tc.exact)
				}
				return
			}
			at, err := time.Parse(time.RFC3339Nano, got)
			if err != nil || at.Before(before.Truncate(time.Microsecond)) || at.After(after) || len(got) != len("2026-09-28T18:02:03.000004Z") {
				t.Fatalf("occurredAt = %q (err %v), want the fire time in [%s, %s] in the fixed-width form", got, err, before, after)
			}
		})
	}
}

// blockingLoader holds the dispatcher worker until release is closed, so a test can tell the
// moment an event was EMITTED from the moment it was processed.
type blockingLoader struct {
	release chan struct{}
	wf      Workflow
}

func (l *blockingLoader) ListActive(context.Context, string, string, Trigger) ([]Workflow, error) {
	<-l.release
	return []Workflow{l.wf}, nil
}

// TestDispatcher_EmitStampsOccurredAt: the instant is taken when the mutation hands the event to
// the engine, not when a dispatcher worker gets round to it — two updates of one record keep their
// commit order even if the buffer holds them for a while. A caller-set instant is left alone.
func TestDispatcher_EmitStampsOccurredAt(t *testing.T) {
	q := &fakeWebhookQueue{}
	loader := &blockingLoader{release: make(chan struct{}), wf: Workflow{
		ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterUpdate, IsActive: true,
		Actions: []Action{{Type: ActionWebhook, URL: "https://hook.test"}},
	}}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, nil, nil, zap.NewNop()).WithWebhookQueue(q)

	preset := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate})
	emitted := time.Now().UTC()
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate, OccurredAt: preset})
	time.Sleep(30 * time.Millisecond) // the worker is parked in the loader; "processing" happens later
	close(loader.release)
	drainDispatcher(t, d)

	if len(q.parked) != 2 {
		t.Fatalf("parked %d deliveries, want 2", len(q.parked))
	}
	first, err := time.Parse(time.RFC3339Nano, webhookOccurredAt(t, q.parked[0].Body))
	if err != nil || first.IsZero() || first.After(emitted) {
		t.Fatalf("occurredAt = %s (err %v); want the Emit instant (<= %s), not the later processing time", first, err, emitted)
	}
	if got := webhookOccurredAt(t, q.parked[1].Body); got != "2026-01-02T03:04:05.000006Z" {
		t.Fatalf("preset occurredAt came out as %q; Emit must not overwrite an instant the caller set", got)
	}
}

// containsAll reports whether s contains every substring.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestResolve_URLTokenSubstitutesEscapedSecret (specs/050 S5, T086): a url_token credential's request
// reaches the partner at <base with the token>/<path>; the display form keeps {secret}.
func TestResolve_URLTokenSubstitutesEscapedSecret(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	res := &fakeCredResolver{auth: map[string]WebhookAuth{"t1|cred-1": {
		BaseURL: srv.URL + "/bot123456:ABC-def", DisplayBase: srv.URL + "/bot{secret}"}}}
	req, err := WebhookRequestFor(context.Background(), res, "t1", Action{Type: ActionWebhook, CredentialID: "cred-1", Path: "/sendMessage?chat_id=7"}, []byte(`{}`))
	if err != nil {
		t.Fatalf("WebhookRequestFor: %v", err)
	}
	if req.DisplayURL != srv.URL+"/bot{secret}/sendMessage" || strings.Contains(req.DisplayURL, "ABC-def") {
		t.Fatalf("display = %q; want the template with {secret}", req.DisplayURL)
	}
	if _, err := NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond).Deliver(context.Background(), req); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if gotPath != "/bot123456:ABC-def/sendMessage" {
		t.Fatalf("partner saw %q; want /bot123456:ABC-def/sendMessage", gotPath)
	}
}

// TestHTTPWebhookSender_URLTokenErrorShowsTemplateOnly: a url_token target fails in every shape
// (refused connection, 4xx, 5xx, 3xx, unbuildable URL) without the token reaching the error text.
func TestHTTPWebhookSender_URLTokenErrorShowsTemplateOnly(t *testing.T) {
	const token = "SECRETTOKEN-9f3"
	status := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status == 302 {
			http.Redirect(w, r, "/x", status)
			return
		}
		w.WriteHeader(status)
	}))
	base := srv.URL
	sender := NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond)
	mk := func() WebhookRequest {
		return WebhookRequest{URL: base + "/bot" + token + "/send", DisplayURL: base + "/bot{secret}/send",
			Body: []byte(`{}`), NoRedirect: true}
	}
	for _, st := range []int{401, 404, 500, 302} {
		status = st
		_, err := sender.Deliver(context.Background(), mk())
		if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "{secret}") {
			t.Fatalf("status %d: err = %v; want a failure naming the template, not the token", st, err)
		}
	}
	srv.Close() // connection refused from here on
	_, err := sender.Deliver(context.Background(), mk())
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "{secret}") {
		t.Fatalf("transport error = %v; want the template, not the token", err)
	}
	bad := mk()
	bad.URL = "http://[::1" + token // unparseable
	if _, err := sender.Deliver(context.Background(), bad); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("build error = %v; must not echo the URL", err)
	}
}
