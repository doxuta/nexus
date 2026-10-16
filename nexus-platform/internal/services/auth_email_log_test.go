package services

// specs/066 L3 (US3): the reset mail and the admin-issued link are lines in the tenant's email log —
// written INSIDE the detached send goroutine, never on the request, so the log opens no account-probe
// channel (MED-1 of specs/002-password-reset). The suite reuses the forgot-password fakes.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/notify"
)

// authLogSpy is a concurrency-safe emailLogInserter that remembers, per Insert, how many mails the
// transport had already delivered (proving "after Send") and the state of the ctx it was given.
type authLogSpy struct {
	mu      sync.Mutex
	sender  *spySender
	entries []emaillog.Entry
	sentAt  []int
	ctxErrs []error
	err     error
}

func (s *authLogSpy) Insert(ctx context.Context, e emaillog.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, e)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	if s.sender != nil {
		s.sentAt = append(s.sentAt, s.sender.count())
	}
	return nil
}

func (s *authLogSpy) snapshot() []emaillog.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]emaillog.Entry(nil), s.entries...)
}

// funcSender is a notify.EmailSender backed by a function.
type funcSender func(ctx context.Context, m notify.Mail) error

func (f funcSender) Send(ctx context.Context, m notify.Mail) error { return f(ctx, m) }

func newResetLogService(t *testing.T) (*AuthService, *authLogSpy, *spySender) {
	t.Helper()
	svc, _, sender := newResetTestService(t)
	spy := &authLogSpy{sender: sender}
	svc.WithEmailLog(spy)
	return svc, spy, sender
}

func resetSubject() string {
	s, _, _ := notify.RenderResetMail("x")
	return s
}

func TestRequestPasswordReset_WritesEmailLogAfterSend(t *testing.T) {
	svc, spy, _ := newResetLogService(t)
	if err := svc.RequestPasswordReset(context.Background(), "acme", "alice@acme.test"); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	got := spy.snapshot()
	if len(got) != 1 {
		t.Fatalf("log rows = %d, want 1", len(got))
	}
	e := got[0]
	if e.Source != emaillog.SourcePasswordReset || e.Status != emaillog.StatusSent || e.TenantID != "t-1" || e.UserID != "u-1" {
		t.Errorf("row = %+v; want password_reset/sent for tenant t-1, user u-1", e)
	}
	if e.RecipientMasked != "a***@acme.test" {
		t.Errorf("recipientMasked = %q, want a***@acme.test (the address itself is never stored)", e.RecipientMasked)
	}
	if e.SubjectTemplate != resetSubject() {
		t.Errorf("subjectTemplate = %q, want the fixed reset subject", e.SubjectTemplate)
	}
	if e.CreatedBy != "" {
		t.Errorf("createdBy = %q: an unauthenticated forgot-password has no actor", e.CreatedBy)
	}
	if !e.Finished || e.Attempts != 1 {
		t.Errorf("row must be terminal after one attempt, got finished=%v attempts=%d", e.Finished, e.Attempts)
	}
	if spy.sentAt[0] != 1 {
		t.Errorf("Insert ran after %d deliveries, want 1: the row records the outcome, so it follows Send", spy.sentAt[0])
	}
	for _, s := range []string{e.RecipientMasked, e.SubjectTemplate, e.LastError, e.UserID} {
		if strings.Contains(s, "token") || strings.Contains(s, "alice@") {
			t.Errorf("row field %q leaks the address or the token", s)
		}
	}
}

// Each of these returns before the goroutine: no row, and the (nil) answer is unchanged.
func TestRequestPasswordReset_WritesNoRowWhenNothingIsSent(t *testing.T) {
	cases := []struct {
		name          string
		tenant, email string
		prep          func(svc *AuthService)
	}{
		{name: "unknown email", tenant: "acme", email: "nobody@acme.test"},
		{name: "unknown tenant", tenant: "ghost", email: "alice@acme.test"},
		{name: "missing fields", tenant: "acme", email: " "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, spy, sender := newResetLogService(t)
			_ = svc.RequestPasswordReset(context.Background(), tc.tenant, tc.email)
			if n := len(spy.snapshot()); n != 0 {
				t.Fatalf("log rows = %d, want 0 — a row for an unknown account would be a probe channel", n)
			}
			if sender.count() != 0 {
				t.Fatalf("sent %d mails", sender.count())
			}
		})
	}
	t.Run("no mailer wired", func(t *testing.T) {
		svc, _ := newTestService(t)
		spy := &authLogSpy{}
		svc.WithResetRepo(newFakeResetRepo()).WithEmailLog(spy)
		if err := svc.RequestPasswordReset(context.Background(), "acme", "alice@acme.test"); err != nil {
			t.Fatalf("RequestPasswordReset: %v", err)
		}
		if n := len(spy.snapshot()); n != 0 {
			t.Fatalf("log rows = %d, want 0 when no mail was sent", n)
		}
	})
	t.Run("log not wired", func(t *testing.T) {
		svc, _, _ := newResetTestService(t)
		if err := svc.RequestPasswordReset(context.Background(), "acme", "alice@acme.test"); err != nil {
			t.Fatalf("RequestPasswordReset: %v", err)
		}
	})
}

// The request path may not wait for, or even touch, the log: a known and an unknown address must
// return at the same speed with the same (nil) answer while the transport is still stuck.
func TestRequestPasswordReset_LogIsNotWrittenOnTheRequest(t *testing.T) {
	svc, _ := newTestService(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var sent sync.WaitGroup
	sent.Add(1)
	transport := &spySender{}
	gated := funcSender(func(ctx context.Context, m notify.Mail) error {
		<-release
		defer sent.Done()
		return transport.Send(ctx, m)
	})
	spy := &authLogSpy{sender: transport}
	// No WithMailDispatcher: the real `go fn()` path.
	svc.WithResetRepo(newFakeResetRepo()).WithResetMailer(gated, "reset@nexus.test", "https://app.nexus.test/reset").WithEmailLog(spy)

	for _, email := range []string{"alice@acme.test", "nobody@acme.test"} {
		start := time.Now()
		err := svc.RequestPasswordReset(context.Background(), "acme", email)
		if took := time.Since(start); took > 100*time.Millisecond {
			t.Errorf("%s: returned in %v, want < 100ms with the transport stuck", email, took)
		}
		if err != nil {
			t.Errorf("%s: err = %v, want nil", email, err)
		}
	}
	if n := len(spy.snapshot()); n != 0 {
		t.Fatalf("%d log rows written on the request path while the send is still in flight", n)
	}
	release <- struct{}{} // let the one goroutine finish
	sent.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for len(spy.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(spy.snapshot()); n != 1 {
		t.Fatalf("rows after the send = %d, want exactly 1 (the known address)", n)
	}
}

func TestRequestPasswordReset_FailedSendIsLoggedMasked(t *testing.T) {
	svc, _ := newTestService(t)
	// A transport error that echoes the address AND the mail body (which carries the raw token).
	echo := funcSender(func(_ context.Context, m notify.Mail) error {
		return errors.New("550 5.1.1 <" + m.To + ">: rejected; body was: " + m.Body)
	})
	spy := &authLogSpy{}
	svc.WithResetRepo(newFakeResetRepo()).WithResetMailer(echo, "reset@nexus.test", "https://app.nexus.test/reset").
		WithMailDispatcher(func(fn func()) { fn() }).WithEmailLog(spy)

	if err := svc.RequestPasswordReset(context.Background(), "acme", "alice@acme.test"); err != nil {
		t.Fatalf("a failed send must not change the answer: %v", err)
	}
	got := spy.snapshot()
	if len(got) != 1 || got[0].Status != emaillog.StatusFailed {
		t.Fatalf("rows = %+v, want one failed row", got)
	}
	for _, leak := range []string{"alice@acme.test", "token=", "app.nexus.test/reset"} {
		if strings.Contains(got[0].LastError, leak) {
			t.Errorf("lastError %q leaks %q", got[0].LastError, leak)
		}
	}
	if got[0].LastError == "" {
		t.Error("a failed row must say why")
	}
}

// sendCtx (15 s) may be spent by a slow transport; the row is the evidence and still has to land.
func TestRequestPasswordReset_LogUsesItsOwnContext(t *testing.T) {
	old := resetMailSendTimeout
	resetMailSendTimeout = 20 * time.Millisecond
	t.Cleanup(func() { resetMailSendTimeout = old })

	svc, _ := newTestService(t)
	waitsForDeadline := funcSender(func(ctx context.Context, _ notify.Mail) error {
		<-ctx.Done()
		return ctx.Err()
	})
	spy := &authLogSpy{}
	svc.WithResetRepo(newFakeResetRepo()).WithResetMailer(waitsForDeadline, "reset@nexus.test", "https://app.nexus.test/reset").
		WithMailDispatcher(func(fn func()) { fn() }).WithEmailLog(spy)

	if err := svc.RequestPasswordReset(context.Background(), "acme", "alice@acme.test"); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	got := spy.snapshot()
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1 even though sendCtx expired", len(got))
	}
	if got[0].Status != emaillog.StatusFailed {
		t.Errorf("status = %q, want failed", got[0].Status)
	}
	if spy.ctxErrs[0] != nil {
		t.Errorf("Insert got a dead ctx (%v): the log write must not inherit the send deadline", spy.ctxErrs[0])
	}
}

// A lost log write never changes what the requester or the admin sees.
func TestRequestPasswordReset_LogFailureIsFailOpen(t *testing.T) {
	svc, spy, sender := newResetLogService(t)
	spy.err = errors.New("db down")
	if err := svc.RequestPasswordReset(context.Background(), "acme", "alice@acme.test"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if sender.count() != 1 {
		t.Errorf("mail count = %d, want 1: the mail goes out whatever the log does", sender.count())
	}
}

func TestIssueResetLinkForUser_WritesResetLinkRow(t *testing.T) {
	svc, spy, _ := newResetLogService(t)
	ctx := audit.WithActor(context.Background(), "admin-7")
	issue, err := svc.IssueResetLinkForUser(ctx, "t-1", "u-1")
	if err != nil {
		t.Fatalf("IssueResetLinkForUser: %v", err)
	}
	got := spy.snapshot()
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	e := got[0]
	if e.Source != emaillog.SourceResetLink || e.Status != emaillog.StatusSent || e.TenantID != "t-1" || e.UserID != "u-1" {
		t.Errorf("row = %+v; want reset_link/sent for t-1/u-1", e)
	}
	if e.CreatedBy != "admin-7" {
		t.Errorf("createdBy = %q, want the issuing admin (read from ctx before the goroutine)", e.CreatedBy)
	}
	if e.RecipientMasked != "a***@acme.test" || e.SubjectTemplate != resetSubject() {
		t.Errorf("recipient/subject = %q / %q", e.RecipientMasked, e.SubjectTemplate)
	}
	for _, s := range []string{e.RecipientMasked, e.SubjectTemplate, e.LastError} {
		if strings.Contains(s, issue.Token) {
			t.Errorf("row field %q holds the raw token", s)
		}
	}
	if spy.sentAt[0] != 1 {
		t.Errorf("Insert ran after %d deliveries, want 1", spy.sentAt[0])
	}
}

func TestIssueResetLinkForUser_NoMailerNoRow(t *testing.T) {
	svc, _ := newTestService(t)
	spy := &authLogSpy{}
	svc.WithResetRepo(newFakeResetRepo()).WithEmailLog(spy)
	if _, err := svc.IssueResetLinkForUser(context.Background(), "t-1", "u-1"); err != nil {
		t.Fatalf("IssueResetLinkForUser: %v", err)
	}
	if n := len(spy.snapshot()); n != 0 {
		t.Fatalf("rows = %d, want 0 — the link was only handed to the admin, no mail left the server", n)
	}
}

func TestIssueResetLinkForUser_FailedCopyIsLoggedFailed(t *testing.T) {
	svc, spy, sender := newResetLogService(t)
	sender.err = errors.New("smtp down")
	if _, err := svc.IssueResetLinkForUser(context.Background(), "t-1", "u-1"); err != nil {
		t.Fatalf("a failed courtesy copy must not fail the call: %v", err)
	}
	got := spy.snapshot()
	if len(got) != 1 || got[0].Status != emaillog.StatusFailed || got[0].CreatedBy != "" {
		t.Fatalf("rows = %+v, want one failed row with no actor (none in ctx)", got)
	}
}
