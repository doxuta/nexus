package notify

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// ---- test doubles -----------------------------------------------------------

// fakeSender records every Send call. It can be configured to block (to fill the
// dispatcher buffer) and/or to fail.
type fakeSender struct {
	mu       sync.Mutex
	sent     []Mail
	failNext bool
	gate     chan struct{} // when non-nil, Send blocks until a value is received
	calls    int32
}

func (f *fakeSender) Send(_ context.Context, m Mail) error {
	atomic.AddInt32(&f.calls, 1)
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	if f.failNext {
		return context.DeadlineExceeded // any non-nil error
	}
	return nil
}

func (f *fakeSender) count() int { return int(atomic.LoadInt32(&f.calls)) }

func (f *fakeSender) messages() []Mail {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Mail, len(f.sent))
	copy(out, f.sent)
	return out
}

// fakeRepo records audit calls in memory, tenant-keyed, so tests can assert the
// pending→sent/failed lifecycle without a database.
type fakeRepo struct {
	mu       sync.Mutex
	inserted []SecurityEvent
	sent     []string
	failed   []string
}

func (r *fakeRepo) InsertPending(_ context.Context, evt SecurityEvent) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inserted = append(r.inserted, evt)
	return "audit-" + evt.Type, nil
}

func (r *fakeRepo) MarkSent(_ context.Context, _ string, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, id)
	return nil
}

func (r *fakeRepo) MarkFailed(_ context.Context, _ string, id, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, id)
	return nil
}

func (r *fakeRepo) counts() (ins, sent, failed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inserted), len(r.sent), len(r.failed)
}

func obsLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// ---- template tests ---------------------------------------------------------

func TestRenderAlert_AllTypes_ViContentNoSecrets(t *testing.T) {
	at := time.Date(2026, 6, 24, 9, 30, 0, 0, time.UTC)
	// Meta intentionally carries secret-looking keys to prove they never leak.
	secretMeta := map[string]any{
		"token":    "super-secret-token-abc123",
		"hash":     "$2a$10$deadbeefdeadbeefdeadbeef",
		"password": "hunter2",
	}

	types := []string{
		AlertPasswordChanged,
		AlertRoleChanged,
		AlertAccountDisabled,
		AlertAccountDeleted,
		AlertRefreshReuse,
	}

	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			evt := SecurityEvent{
				Type:     typ,
				TenantID: "t1",
				UserID:   "u1",
				Email:    "user@example.com",
				At:       at,
				Meta:     secretMeta,
			}
			subject, text, htmlBody := renderAlert(evt)

			if subject == "" || text == "" || htmlBody == "" {
				t.Fatalf("empty render for %s: subject=%q text=%q html=%q", typ, subject, text, htmlBody)
			}

			// Timestamp present in vi format.
			const wantTime = "09:30 24/06/2026"
			if !strings.Contains(text, wantTime) {
				t.Errorf("text missing timestamp %q: %s", wantTime, text)
			}
			if !strings.Contains(htmlBody, wantTime) {
				t.Errorf("html missing timestamp %q", wantTime)
			}

			// No secret substrings anywhere.
			for _, body := range []string{subject, text, htmlBody} {
				for _, bad := range []string{
					"super-secret-token-abc123", "$2a$10$deadbeefdeadbeefdeadbeef", "hunter2",
					"token", "hash", "password",
				} {
					if strings.Contains(strings.ToLower(body), strings.ToLower(bad)) {
						t.Errorf("%s render leaked forbidden substring %q in %q", typ, bad, body)
					}
				}
			}
		})
	}
}

func TestRenderAlert_UnknownType_FallsBackSafely(t *testing.T) {
	subject, text, htmlBody := renderAlert(SecurityEvent{Type: "totally_unknown", At: time.Now()})
	if subject == "" || text == "" || htmlBody == "" {
		t.Fatal("unknown type should still render a generic notice")
	}
	if strings.Contains(subject, "totally_unknown") {
		t.Errorf("raw type leaked into subject: %q", subject)
	}
}

// ---- NoopSender -------------------------------------------------------------

func TestNoopSender_AuditsAsNotSent(t *testing.T) {
	// Wiring a dispatcher with a NoopSender (ALERTS_ENABLED=false path) must not error
	// and must not "send" anywhere observable. We assert the fake repo still audits but
	// the noop sender returns nil so MarkSent runs.
	repo := &fakeRepo{}
	logger, _ := obsLogger()
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, NewNoopSender(), repo, logger)

	d.EnqueueSecurityAlert(context.Background(), SecurityEvent{
		Type: AlertPasswordChanged, TenantID: "t1", UserID: "u1", Email: "u@x.io", At: time.Now(),
	})
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// The audit must record what HAPPENED. With no mail configured the message was discarded, so
	// pending→failed is the truthful transition; the old expectation (pending→sent) meant the
	// notification log claimed a security alert had reached a user who was never told.
	ins, sent, failed := repo.counts()
	if ins != 1 || sent != 0 || failed != 1 {
		t.Fatalf("a discarded message must audit pending->failed: ins=%d sent=%d failed=%d", ins, sent, failed)
	}
}

// ---- dispatcher happy path --------------------------------------------------

func TestDispatcher_SendsAndAudits(t *testing.T) {
	sender := &fakeSender{}
	repo := &fakeRepo{}
	logger, _ := obsLogger()
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 16, Workers: 3}, sender, repo, logger)

	const n = 5
	for i := 0; i < n; i++ {
		d.EnqueueSecurityAlert(context.Background(), SecurityEvent{
			Type: AlertRoleChanged, TenantID: "t1", UserID: "u1", Email: "u@x.io", At: time.Now(),
		})
	}
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if got := sender.count(); got != n {
		t.Fatalf("expected %d sends, got %d", n, got)
	}
	ins, sent, failed := repo.counts()
	if ins != n || sent != n || failed != 0 {
		t.Fatalf("audit lifecycle wrong: ins=%d sent=%d failed=%d", ins, sent, failed)
	}
	for _, m := range sender.messages() {
		if m.To != "u@x.io" {
			t.Errorf("wrong recipient: %q", m.To)
		}
	}
}

func TestDispatcher_SendFailure_AuditsFailed_NoPanic(t *testing.T) {
	sender := &fakeSender{failNext: true}
	repo := &fakeRepo{}
	logger, logs := obsLogger()
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 4, Workers: 1}, sender, repo, logger)

	d.EnqueueSecurityAlert(context.Background(), SecurityEvent{
		Type: AlertRefreshReuse, TenantID: "t1", UserID: "u1", Email: "u@x.io", At: time.Now(),
	})
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	_, sent, failed := repo.counts()
	if sent != 0 || failed != 1 {
		t.Fatalf("send failure should audit failed: sent=%d failed=%d", sent, failed)
	}
	if logs.FilterMessageSnippet("send failed").Len() == 0 {
		t.Error("expected a 'send failed' warning log")
	}
}

// ---- buffer-full drop -------------------------------------------------------

func TestEnqueue_BufferFull_DropsWithLog_NoBlock(t *testing.T) {
	// One worker, buffer of 1, and a gated sender: the first event occupies the worker
	// (blocked in Send), the second fills the buffer, every further enqueue must DROP
	// (non-blocking) and log a warning — never block, never panic.
	sender := &fakeSender{gate: make(chan struct{})}
	logger, logs := obsLogger()
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 1, Workers: 1}, sender, nil, logger)

	enq := func() {
		d.EnqueueSecurityAlert(context.Background(), SecurityEvent{
			Type: AlertPasswordChanged, TenantID: "t1", UserID: "u1", Email: "u@x.io", At: time.Now(),
		})
	}

	// First event: worker picks it up and blocks in Send on the gate.
	enq()
	// Give the worker a moment to dequeue and block.
	waitFor(t, func() bool { return sender.count() == 1 }, time.Second)

	// Second event fills the buffer (worker still busy).
	enq()

	// Subsequent enqueues must not block — run several with a hard deadline guard.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			enq()
		}
		close(done)
	}()
	select {
	case <-done:
		// good: returned promptly, never blocked
	case <-time.After(2 * time.Second):
		t.Fatal("EnqueueSecurityAlert blocked when buffer was full (should drop, not block)")
	}

	if logs.FilterMessageSnippet("buffer full").Len() == 0 {
		t.Error("expected at least one 'buffer full' drop warning")
	}

	// Release the gate so the worker can finish, then drain.
	close(sender.gate)
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// ---- shutdown drain ---------------------------------------------------------

func TestShutdown_DrainsBufferedEvents(t *testing.T) {
	sender := &fakeSender{}
	logger, _ := obsLogger()
	// Large buffer, single worker: enqueue many, then Shutdown must process ALL before
	// returning (graceful drain).
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 256, Workers: 1}, sender, nil, logger)

	const n = 100
	for i := 0; i < n; i++ {
		d.EnqueueSecurityAlert(context.Background(), SecurityEvent{
			Type: AlertAccountDisabled, TenantID: "t1", UserID: "u1", Email: "u@x.io", At: time.Now(),
		})
	}
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := sender.count(); got != n {
		t.Fatalf("drain incomplete: expected %d sends, got %d", n, got)
	}
}

func TestShutdown_Idempotent_DropsAfterClose(t *testing.T) {
	sender := &fakeSender{}
	logger, logs := obsLogger()
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 4, Workers: 1}, sender, nil, logger)

	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	// Second shutdown must not panic on a re-closed channel.
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
	// Enqueue after shutdown: dropped + logged, never a send-on-closed-channel panic.
	d.EnqueueSecurityAlert(context.Background(), SecurityEvent{Type: AlertPasswordChanged, Email: "u@x.io", At: time.Now()})
	if sender.count() != 0 {
		t.Error("event after shutdown should be dropped, not sent")
	}
	if logs.FilterMessageSnippet("shut down").Len() == 0 {
		t.Error("expected a 'dispatcher shut down' drop log")
	}
}

// ---- helpers ----------------------------------------------------------------

func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
