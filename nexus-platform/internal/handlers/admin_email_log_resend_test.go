package handlers

// httptest — POST /admin/email-log/:id/resend (specs/066 L2, T053) through the REAL route builder: the
// body-less request is a preview, only {"dryRun":false} sends, staff get 403 before anything runs, the
// service's refusals map to the contract's status/code, and the list carries canResend.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// ---- the resend half of hFakeEmailLogStore (the list half is in admin_email_log_handler_test.go) ----

func (f *hFakeEmailLogStore) Get(_ context.Context, tenantID, id string) (*emaillog.Row, error) {
	f.gets++
	f.lastTenant = tenantID
	r, ok := f.byTenantID[tenantID+"/"+id]
	if !ok {
		return nil, emaillog.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *hFakeEmailLogStore) LockResend(_ context.Context, tenantID, id string, at time.Time) (bool, error) {
	r, ok := f.byTenantID[tenantID+"/"+id]
	if !ok || r.ResentAt != nil || r.Status != emaillog.StatusFailed {
		return false, nil
	}
	r.ResentAt = &at
	f.locks++
	return true, nil
}

func (f *hFakeEmailLogStore) ReleaseResend(context.Context, string, string) error { return nil }

type hRenderer struct {
	mail workflow.RenderedEmail
	err  error
}

func (h hRenderer) RenderEmailForRecord(context.Context, string, string, int, string) (workflow.RenderedEmail, workflow.Action, error) {
	return h.mail, workflow.Action{Type: workflow.ActionEmailAlert, To: "{{x}}", Subject: "S", Body: "b"}, h.err
}

type hSender struct {
	store *hFakeEmailLogStore
	calls int
	err   error
}

func (h *hSender) SendEmail(ctx context.Context, to, subject, _ string) error {
	h.calls++
	o, _ := workflow.EmailOriginFrom(ctx)
	st := emaillog.StatusQueued
	if h.err != nil {
		st = emaillog.StatusFailed
	}
	h.store.byTenantID[o.TenantID+"/"+o.LogID] = &emaillog.Row{ID: o.LogID, Source: "workflow", Status: st, RecipientMasked: workflow.MaskRecipients(to)}
	return h.err
}

func resendFailedRow() *emaillog.Row {
	return &emaillog.Row{ID: "log-1", Source: "workflow", Status: "failed", RecipientMasked: "l***@x.vn",
		WorkflowID: strPtr("wf-1"), WorkflowName: strPtr("Chào khách"), ActionIndex: intPtr(0), EntityName: strPtr("deals"), RecordID: strPtr("rec-1")}
}

func intPtr(n int) *int { return &n }

type resendEnv struct {
	r      *gin.Engine
	store  *hFakeEmailLogStore
	sender *hSender
}

func newResendEnv(t *testing.T, mailConfigured bool, rend hRenderer, senderErr error) *resendEnv {
	t.Helper()
	store := &hFakeEmailLogStore{byTenantID: map[string]*emaillog.Row{"t1/log-1": resendFailedRow()}}
	sender := &hSender{store: store, err: senderErr}
	svc := services.NewEmailLogService(store).WithMailConfigured(mailConfigured).WithQueueEnabled(true).
		WithRenderer(rend).WithSender(sender)
	return &resendEnv{r: buildEmailLogEngine(t, svc), store: store, sender: sender}
}

func post(t *testing.T, r *gin.Engine, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// A chunked request with an empty body has ContentLength -1; it is "no body" (a preview), not a 400.
func TestResend_ChunkedEmptyBodyIsAPreview(t *testing.T) {
	e := newResendEnv(t, true, okMail, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/email-log/log-1/resend", strings.NewReader(""))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer tok-admin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"dryRun":true`) {
		t.Fatalf("got %d %s; want a 200 preview", w.Code, w.Body)
	}
}

var okMail = hRenderer{mail: workflow.RenderedEmail{To: "lan@x.vn", Subject: "Chào Lan", Body: "b"}}

func TestResend_PreviewIsTheDefault(t *testing.T) {
	for name, body := range map[string]string{"no body": "", "empty object": "{}", "explicit true": `{"dryRun":true}`} {
		t.Run(name, func(t *testing.T) {
			e := newResendEnv(t, true, okMail, nil)
			w := post(t, e.r, "/api/v1/admin/email-log/log-1/resend", "tok-admin", body)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var got struct {
				Data map[string]any `json:"data"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if got.Data["dryRun"] != true || got.Data["recipientMasked"] != "l***@x.vn" || got.Data["subject"] != "Chào Lan" || got.Data["ruleChangedSinceSend"] != false {
				t.Fatalf("data = %v", got.Data)
			}
			if _, leaked := got.Data["entry"]; leaked || e.sender.calls != 0 || e.store.locks != 0 {
				t.Fatalf("a preview sent or locked: %v calls=%d locks=%d", got.Data, e.sender.calls, e.store.locks)
			}
		})
	}
}

func TestResend_DryRunFalseSendsAndReturnsTheNewRow(t *testing.T) {
	e := newResendEnv(t, true, okMail, nil)
	w := post(t, e.r, "/api/v1/admin/email-log/log-1/resend", "tok-admin", `{"dryRun":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got struct {
		Data struct {
			DryRun bool           `json:"dryRun"`
			Entry  map[string]any `json:"entry"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Data.DryRun || got.Data.Entry == nil || got.Data.Entry["status"] != "queued" || got.Data.Entry["recipientMasked"] != "l***@x.vn" {
		t.Fatalf("data = %+v", got.Data)
	}
	if e.sender.calls != 1 {
		t.Fatalf("sent %d times", e.sender.calls)
	}
	// Second press: 409.
	w = post(t, e.r, "/api/v1/admin/email-log/log-1/resend", "tok-admin", `{"dryRun":false}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"EMAIL_NOT_RESENDABLE"`) || e.sender.calls != 1 {
		t.Fatalf("second press = %d %s (sends %d)", w.Code, w.Body, e.sender.calls)
	}
}

func TestResend_GateAndTenantPin(t *testing.T) {
	e := newResendEnv(t, true, okMail, nil)
	if w := post(t, e.r, "/api/v1/admin/email-log/log-1/resend", "tok-user", `{"dryRun":false}`); w.Code != http.StatusForbidden || e.store.gets != 0 {
		t.Fatalf("staff POST = %d (store reads %d); want 403 before anything runs", w.Code, e.store.gets)
	}
	// Tenant B's admin on tenant A's id: the SAME 404 body as an id that never existed.
	other := post(t, e.r, "/api/v1/admin/email-log/log-1/resend", "tok-admin-t2", `{"dryRun":false}`)
	ghost := post(t, e.r, "/api/v1/admin/email-log/does-not-exist/resend", "tok-admin", `{"dryRun":false}`)
	if other.Code != http.StatusNotFound || ghost.Code != http.StatusNotFound || other.Body.String() != ghost.Body.String() {
		t.Fatalf("cross-tenant %d %s vs ghost %d %s; want identical 404s", other.Code, other.Body, ghost.Code, ghost.Body)
	}
	if e.sender.calls != 0 || e.store.locks != 0 {
		t.Fatal("another tenant's row was touched")
	}
}

func TestResend_ErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		mail    bool
		rend    hRenderer
		sendErr error
		status  int
		code    string
	}{
		{"mail not configured", false, okMail, nil, 503, "MAIL_NOT_CONFIGURED"},
		{"rule gone", true, hRenderer{err: services.ErrEmailRuleGone}, nil, 409, "EMAIL_RESEND_RULE_GONE"},
		{"rule switched off", true, hRenderer{err: services.ErrEmailRuleDisabled}, nil, 409, "EMAIL_RESEND_RULE_DISABLED"},
		{"record gone", true, hRenderer{err: services.ErrEmailRecordGone}, nil, 409, "EMAIL_RESEND_RECORD_GONE"},
		{"no recipient", true, hRenderer{mail: workflow.RenderedEmail{To: " "}}, nil, 422, "EMAIL_RESEND_NO_RECIPIENT"},
		{"not wired", true, hRenderer{err: services.ErrEmailResendUnavailable}, nil, 500, "EMAIL_RESEND_UNAVAILABLE"},
		{"render blew up", true, hRenderer{err: errors.New("db down")}, nil, 500, "EMAIL_RESEND_FAILED"},
		{"sender failed", true, okMail, errors.New("queue down"), 500, "EMAIL_RESEND_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newResendEnv(t, tc.mail, tc.rend, tc.sendErr)
			w := post(t, e.r, "/api/v1/admin/email-log/log-1/resend", "tok-admin", `{"dryRun":false}`)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("got %d %s; want %d %s", w.Code, w.Body, tc.status, tc.code)
			}
			if tc.name == "sender failed" && !strings.Contains(w.Body.String(), `"entry"`) {
				t.Errorf("a failed resend must carry the new failed row so the screen can show it: %s", w.Body)
			}
		})
	}
}

func TestResend_BadBody(t *testing.T) {
	e := newResendEnv(t, true, okMail, nil)
	w := post(t, e.r, "/api/v1/admin/email-log/log-1/resend", "tok-admin", `{"dryRun":"yes"}`)
	if w.Code != http.StatusBadRequest || e.sender.calls != 0 {
		t.Fatalf("status %d (sends %d): %s; want 400 and no send", w.Code, e.sender.calls, w.Body)
	}
	// The body can never choose the recipient or the content (FR-038): unknown keys are refused.
	w = post(t, e.r, "/api/v1/admin/email-log/log-1/resend", "tok-admin", `{"dryRun":false,"to":"evil@x.com"}`)
	if w.Code != http.StatusBadRequest || e.sender.calls != 0 {
		t.Fatalf("status %d (sends %d): %s; want 400 for an unknown key", w.Code, e.sender.calls, w.Body)
	}
}

func TestEmailLogList_CarriesCanResend(t *testing.T) {
	e := newResendEnv(t, true, okMail, nil)
	e.store.rows = map[string][]emaillog.Row{"t1": {*resendFailedRow()}}
	w := jobsReq(t, e.r, http.MethodGet, "/api/v1/admin/email-log", "tok-admin")
	var got struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || len(got.Data) != 1 || got.Data[0]["canResend"] != true {
		t.Fatalf("list = %d %s; want canResend=true on the failed workflow row", w.Code, w.Body)
	}
	if _, leaked := got.Data[0]["actionHash"]; leaked {
		t.Fatalf("the action hash is internal: %v", got.Data[0])
	}
}
