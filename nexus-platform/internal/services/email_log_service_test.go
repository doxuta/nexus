package services

// specs/066 L2 (T050): EmailLogService.Resend — the resend policy, the dry-run default, the one-shot
// CAS, the origin the sender receives, and the audit row. Fakes only; the real CAS and the queue are
// in email_resend_integration_test.go.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/workflow"
)

func rptr[T any](v T) *T { return &v }

// fakeLogStore is a tenant-pinned in-memory sys_email_log with the same CAS the SQL does.
type fakeLogStore struct {
	mu       sync.Mutex
	rows     map[string]*emaillog.Row
	locks    int
	released int
	// afterLock runs right after a successful lock (simulates the admin's browser going away).
	afterLock func()
}

func (f *fakeLogStore) ListForTenant(_ context.Context, tenantID string, _ emaillog.Filter) ([]emaillog.Row, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []emaillog.Row
	for id, r := range f.rows {
		if tenantOfID(id) == tenantID {
			out = append(out, *r)
		}
	}
	return out, "", nil
}

// ids are "<tenant>/<id>" internally so a cross-tenant Get cannot find the row.
func tenantOfID(key string) string {
	for i := range key {
		if key[i] == '/' {
			return key[:i]
		}
	}
	return ""
}

func (f *fakeLogStore) Get(_ context.Context, tenantID, id string) (*emaillog.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[tenantID+"/"+id]
	if !ok {
		return nil, emaillog.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *fakeLogStore) LockResend(_ context.Context, tenantID, id string, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[tenantID+"/"+id]
	if !ok || r.Status != emaillog.StatusFailed || r.ResentAt != nil {
		return false, nil
	}
	r.ResentAt = &at
	f.locks++
	if f.afterLock != nil {
		f.afterLock()
	}
	return true, nil
}

func (f *fakeLogStore) ReleaseResend(_ context.Context, tenantID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[tenantID+"/"+id]; ok {
		r.ResentAt = nil
		f.released++
	}
	return nil
}

// fakeRenderer answers RenderEmailForRecord.
type fakeRenderer struct {
	mail   workflow.RenderedEmail
	action workflow.Action
	err    error
	calls  atomic.Int32
}

func (f *fakeRenderer) RenderEmailForRecord(_ context.Context, _, _ string, _ int, _ string) (workflow.RenderedEmail, workflow.Action, error) {
	f.calls.Add(1)
	return f.mail, f.action, f.err
}

// fakeResendSender records the origin it was handed and writes the new row the way the real senders do.
type fakeResendSender struct {
	mu      sync.Mutex
	store   *fakeLogStore
	tenant  string
	calls   int
	origin  workflow.EmailOrigin
	to, sub string
	err     error
	noRow   bool // simulate a fail-open log write that was lost
	ctxErr  error
}

func (f *fakeResendSender) SendEmail(ctx context.Context, to, subject, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.to, f.sub = to, subject
	f.ctxErr = ctx.Err()
	o, _ := workflow.EmailOriginFrom(ctx)
	f.origin = o
	if f.store != nil && !f.noRow {
		st := emaillog.StatusQueued
		if f.err != nil {
			st = emaillog.StatusFailed
		}
		f.store.mu.Lock()
		f.store.rows[o.TenantID+"/"+o.LogID] = &emaillog.Row{ID: o.LogID, Source: emaillog.SourceWorkflow, Status: st,
			RecipientMasked: workflow.MaskRecipients(to), ResendOf: rptr(o.ResendOf), CreatedBy: rptr(o.CreatedBy)}
		f.store.mu.Unlock()
	}
	return f.err
}

type auditSpy struct {
	mu     sync.Mutex
	events []audit.Event
}

func (a *auditSpy) Record(_ context.Context, e audit.Event) {
	a.mu.Lock()
	a.events = append(a.events, e)
	a.mu.Unlock()
}

const (
	rTenant = "ten-a"
	rOther  = "ten-b"
	rRow    = "log-1"
)

var rAction = workflow.Action{Type: workflow.ActionEmailAlert, To: "{{owner}}", Subject: "Chào {{ho_ten}}", Body: "b"}

func failedRow() *emaillog.Row {
	return &emaillog.Row{
		ID: rRow, Source: emaillog.SourceWorkflow, Status: emaillog.StatusFailed, RecipientMasked: "l***@x.vn",
		WorkflowID: rptr("wf-1"), WorkflowName: rptr("Chào khách"), ActionIndex: rptr(1), EntityName: rptr("deals"),
		RecordID: rptr("rec-1"), ActionHash: rptr(workflow.EmailActionHash(rAction)),
	}
}

type resendEnv struct {
	svc    *EmailLogService
	store  *fakeLogStore
	rend   *fakeRenderer
	sender *fakeResendSender
	aud    *auditSpy
}

func newResendEnv(mut func(r *emaillog.Row)) *resendEnv {
	row := failedRow()
	if mut != nil {
		mut(row)
	}
	store := &fakeLogStore{rows: map[string]*emaillog.Row{rTenant + "/" + rRow: row}}
	rend := &fakeRenderer{mail: workflow.RenderedEmail{To: "lan@x.vn", Subject: "Chào Lan", Body: "b"}, action: rAction}
	sender := &fakeResendSender{store: store}
	aud := &auditSpy{}
	svc := NewEmailLogService(store).WithMailConfigured(true).WithQueueEnabled(true).
		WithRenderer(rend).WithSender(sender).WithAuditor(aud)
	return &resendEnv{svc: svc, store: store, rend: rend, sender: sender, aud: aud}
}

func TestResend_PolicyMatrix(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		mut  func(r *emaillog.Row)
	}{
		{"password reset mail", func(r *emaillog.Row) { r.Source = emaillog.SourcePasswordReset }},
		{"reset link mail", func(r *emaillog.Row) { r.Source = emaillog.SourceResetLink }},
		{"sent", func(r *emaillog.Row) { r.Status = emaillog.StatusSent }},
		{"still queued", func(r *emaillog.Row) { r.Status = emaillog.StatusQueued }},
		{"already resent", func(r *emaillog.Row) { r.ResentAt = &now }},
		{"no rule id", func(r *emaillog.Row) { r.WorkflowID = nil }},
		{"no record id", func(r *emaillog.Row) { r.RecordID = nil }},
		{"no action slot", func(r *emaillog.Row) { r.ActionIndex = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newResendEnv(tc.mut)
			for _, dry := range []*bool{nil, rptr(true), rptr(false)} {
				_, err := e.svc.Resend(context.Background(), rTenant, "u1", rRow, dry)
				wantCode(t, err, "EMAIL_NOT_RESENDABLE")
			}
			if e.rend.calls.Load() != 0 || e.sender.calls != 0 || e.store.locks != 0 {
				t.Fatalf("refused row still rendered=%d sent=%d locked=%d", e.rend.calls.Load(), e.sender.calls, e.store.locks)
			}
		})
	}
}

func TestResend_NotFoundIsTheSameForAnotherTenantAndAGhost(t *testing.T) {
	e := newResendEnv(nil)
	_, errOther := e.svc.Resend(context.Background(), rOther, "u2", rRow, rptr(false))
	_, errGhost := e.svc.Resend(context.Background(), rTenant, "u1", "ghost", rptr(false))
	wantCode(t, errOther, "EMAIL_LOG_NOT_FOUND")
	wantCode(t, errGhost, "EMAIL_LOG_NOT_FOUND")
	var a, b *ValidationError
	if !errors.As(errOther, &a) || !errors.As(errGhost, &b) || a.Msg != b.Msg || len(a.Details) != 0 || len(b.Details) != 0 {
		t.Fatalf("not-found bodies differ: %+v vs %+v", a, b)
	}
	if e.sender.calls != 0 || e.store.locks != 0 {
		t.Fatal("another tenant's row was touched")
	}
}

func TestResend_MailNotConfigured(t *testing.T) {
	e := newResendEnv(nil)
	e.svc.WithMailConfigured(false)
	for _, dry := range []*bool{nil, rptr(false)} {
		_, err := e.svc.Resend(context.Background(), rTenant, "u1", rRow, dry)
		wantCode(t, err, "MAIL_NOT_CONFIGURED")
	}
	if e.store.locks != 0 || e.sender.calls != 0 {
		t.Fatal("resent with no mail configured")
	}
}

func TestResend_DryRunIsAPreviewWithNoEffect(t *testing.T) {
	for name, dry := range map[string]*bool{"absent": nil, "true": rptr(true)} {
		t.Run(name, func(t *testing.T) {
			e := newResendEnv(nil)
			res, err := e.svc.Resend(context.Background(), rTenant, "u1", rRow, dry)
			if err != nil {
				t.Fatal(err)
			}
			if !res.DryRun || res.RecipientMasked != "l***@x.vn" || res.Subject != "Chào Lan" || res.RuleChangedSinceSend || res.Entry != nil {
				t.Fatalf("preview = %+v", res)
			}
			if e.store.locks != 0 || e.sender.calls != 0 || len(e.aud.events) != 0 {
				t.Fatalf("a preview locked=%d sent=%d audited=%d", e.store.locks, e.sender.calls, len(e.aud.events))
			}
		})
	}
}

func TestResend_RuleChangedSinceSend(t *testing.T) {
	for name, tc := range map[string]struct {
		hash *string
		want bool
	}{
		"same templates":        {rptr(workflow.EmailActionHash(rAction)), false},
		"templates edited":      {rptr(workflow.EmailActionHash(workflow.Action{To: "{{owner}}", Subject: "Cũ", Body: "b"})), true},
		"row predates the hash": {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newResendEnv(func(r *emaillog.Row) { r.ActionHash = tc.hash })
			res, err := e.svc.Resend(context.Background(), rTenant, "u1", rRow, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.RuleChangedSinceSend != tc.want {
				t.Fatalf("ruleChangedSinceSend = %v, want %v", res.RuleChangedSinceSend, tc.want)
			}
		})
	}
}

func TestResend_RenderRefusals(t *testing.T) {
	cases := []struct {
		name string
		err  error
		to   string
		code string
	}{
		{"rule gone", ErrEmailRuleGone, "", "EMAIL_RESEND_RULE_GONE"},
		{"rule switched off", ErrEmailRuleDisabled, "", "EMAIL_RESEND_RULE_DISABLED"},
		{"record gone", ErrEmailRecordGone, "", "EMAIL_RESEND_RECORD_GONE"},
		{"no recipient after render", nil, "  ", "EMAIL_RESEND_NO_RECIPIENT"},
	}
	for _, tc := range cases {
		for _, dry := range []*bool{nil, rptr(false)} {
			e := newResendEnv(nil)
			e.rend.err, e.rend.mail.To = tc.err, tc.to
			_, err := e.svc.Resend(context.Background(), rTenant, "u1", rRow, dry)
			wantCode(t, err, tc.code)
			if e.store.locks != 0 || e.sender.calls != 0 {
				t.Fatalf("%s: refused resend locked the row or sent", tc.name)
			}
		}
	}
}

func TestResend_UnwiredIsALoudError(t *testing.T) {
	e := newResendEnv(nil)
	e.rend.err = ErrEmailResendUnavailable
	_, err := e.svc.Resend(context.Background(), rTenant, "u1", rRow, nil)
	if !errors.Is(err, ErrEmailResendUnavailable) {
		t.Fatalf("err = %v; want ErrEmailResendUnavailable (never a silent fallback)", err)
	}
	e = newResendEnv(nil)
	e.svc.sender = nil
	_, err = e.svc.Resend(context.Background(), rTenant, "u1", rRow, rptr(false))
	if !errors.Is(err, ErrEmailResendUnavailable) || e.store.locks != 0 {
		t.Fatalf("nil sender: err=%v locks=%d; want unavailable before any lock", err, e.store.locks)
	}
}

func TestResend_SendsOnceWithTheNewOriginAndAudits(t *testing.T) {
	e := newResendEnv(nil)
	res, err := e.svc.Resend(context.Background(), rTenant, "actor-1", rRow, rptr(false))
	if err != nil {
		t.Fatal(err)
	}
	if res.DryRun || res.Entry == nil || res.Entry.Status != emaillog.StatusQueued {
		t.Fatalf("result = %+v; want the new row", res)
	}
	o := e.sender.origin
	if e.sender.calls != 1 || e.sender.to != "lan@x.vn" || e.sender.sub != "Chào Lan" {
		t.Fatalf("sender called %d times with %q/%q", e.sender.calls, e.sender.to, e.sender.sub)
	}
	if o.TenantID != rTenant || o.WorkflowID != "wf-1" || o.ActionIndex != 1 || o.EntityName != "deals" || o.RecordID != "rec-1" ||
		o.ResendOf != rRow || o.CreatedBy != "actor-1" || o.LogID == "" || o.LogID == rRow ||
		o.SubjectTemplate != rAction.Subject || o.ActionHash != workflow.EmailActionHash(rAction) {
		t.Fatalf("origin = %+v", o)
	}
	if res.Entry.ID != o.LogID {
		t.Fatalf("entry id %q != pre-minted %q", res.Entry.ID, o.LogID)
	}
	old, _ := e.store.Get(context.Background(), rTenant, rRow)
	if old.ResentAt == nil {
		t.Fatal("the old row was not locked")
	}
	if len(e.aud.events) != 1 {
		t.Fatalf("audit events = %+v; want exactly one", e.aud.events)
	}
	ev := e.aud.events[0]
	if ev.EntityName != audit.EmailMeta || ev.TenantID != rTenant || ev.ActorUserID != "actor-1" || ev.RecordID != rRow ||
		ev.Action != audit.ActionCreate || ev.Diff == nil || ev.Diff.After["resendOf"] != rRow || ev.Diff.After["newLogId"] != o.LogID {
		t.Fatalf("audit = %+v", ev)
	}
}

func TestResend_SecondPressAndRaceSendExactlyOne(t *testing.T) {
	e := newResendEnv(nil)
	if _, err := e.svc.Resend(context.Background(), rTenant, "u1", rRow, rptr(false)); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Resend(context.Background(), rTenant, "u1", rRow, rptr(false))
	wantCode(t, err, "EMAIL_NOT_RESENDABLE")

	for i := 0; i < 20; i++ {
		e := newResendEnv(nil)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[j] = e.svc.Resend(context.Background(), rTenant, "u1", rRow, rptr(false))
			}()
		}
		wg.Wait()
		wins := 0
		for _, err := range errs {
			if err == nil {
				wins++
			} else {
				wantCode(t, err, "EMAIL_NOT_RESENDABLE")
			}
		}
		if wins != 1 || e.sender.calls != 1 {
			t.Fatalf("round %d: wins=%d sends=%d; want exactly one of each", i, wins, e.sender.calls)
		}
	}
}

func TestResend_SenderFailsAfterTheLock(t *testing.T) {
	// The sender wrote its failed row: the answer carries it and it can be resent again; the old row
	// stays locked (the new failed row is the retry handle).
	e := newResendEnv(nil)
	e.sender.err = errors.New("queue down")
	res, err := e.svc.Resend(context.Background(), rTenant, "u1", rRow, rptr(false))
	if !errors.Is(err, ErrEmailResendFailed) || res == nil || res.Entry == nil || res.Entry.Status != emaillog.StatusFailed {
		t.Fatalf("res=%+v err=%v; want ErrEmailResendFailed carrying the failed row", res, err)
	}
	if e.store.released != 0 {
		t.Fatal("released the lock although a failed row now carries the retry")
	}
	if len(e.aud.events) != 1 {
		t.Fatalf("a locked attempt must be audited; events = %d", len(e.aud.events))
	}

	// The sender left NO row (log write lost): nothing carries the retry, so the lock is released.
	e = newResendEnv(nil)
	e.sender.err, e.sender.noRow = errors.New("queue down"), true
	res, err = e.svc.Resend(context.Background(), rTenant, "u1", rRow, rptr(false))
	if !errors.Is(err, ErrEmailResendFailed) || (res != nil && res.Entry != nil) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if old, _ := e.store.Get(context.Background(), rTenant, rRow); old.ResentAt != nil || e.store.released != 1 {
		t.Fatal("the old row stayed locked with no new row: the mail would be lost for good")
	}
}

// The admin's browser goes away right after the lock is taken: the hand-over and its log row must
// still complete on a live context (a cancelled one would fail the enqueue AND the "mark failed").
func TestResend_SendPhaseSurvivesACancelledRequest(t *testing.T) {
	e := newResendEnv(nil)
	ctx, cancel := context.WithCancel(context.Background())
	e.store.afterLock = cancel
	res, err := e.svc.Resend(ctx, rTenant, "u1", rRow, rptr(false))
	if err != nil || res.Entry == nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if e.sender.calls != 1 || e.sender.ctxErr != nil {
		t.Fatalf("sender calls=%d ctx.Err at send=%v; want one send on a live context", e.sender.calls, e.sender.ctxErr)
	}
}

func TestList_CanResend(t *testing.T) {
	e := newResendEnv(nil)
	e.store.rows[rTenant+"/sent"] = &emaillog.Row{ID: "sent", Source: emaillog.SourceWorkflow, Status: emaillog.StatusSent,
		WorkflowID: rptr("wf-1"), WorkflowName: rptr("r"), ActionIndex: rptr(0), RecordID: rptr("rec")}
	e.store.rows[rTenant+"/orphan"] = &emaillog.Row{ID: "orphan", Source: emaillog.SourceWorkflow, Status: emaillog.StatusFailed,
		WorkflowID: rptr("wf-gone"), ActionIndex: rptr(0), RecordID: rptr("rec")} // WorkflowName nil: rule deleted
	e.store.rows[rTenant+"/auth"] = &emaillog.Row{ID: "auth", Source: emaillog.SourcePasswordReset, Status: emaillog.StatusFailed}
	rows, _, err := e.svc.List(context.Background(), rTenant, emaillog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.ID] = r.CanResend
	}
	want := map[string]bool{rRow: true, "sent": false, "orphan": false, "auth": false}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("canResend[%s] = %v, want %v", id, got[id], w)
		}
	}
}
