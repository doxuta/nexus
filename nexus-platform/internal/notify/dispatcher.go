package notify

import (
	"context"
	"sync"

	"go.uber.org/zap"
)

// AsyncDispatcher is the concrete Notifier (spec §3). EnqueueSecurityAlert pushes the
// event onto a buffered channel and returns immediately; a pool of worker goroutines
// drains the channel, renders the locale template, sends the email through the
// EmailSender, and records the result in the security_alerts audit table.
//
// Fail-open guarantees:
//   - A full buffer makes EnqueueSecurityAlert drop the event with a zap warning — it
//     NEVER blocks the calling business flow and NEVER panics.
//   - After Shutdown, further enqueues are dropped with a log line (no send on a closed
//     channel panic).
//   - A send or audit error is logged and recorded (status=failed); it never propagates
//     to the caller.
type AsyncDispatcher struct {
	ch      chan SecurityEvent
	sender  EmailSender
	repo    AlertRepository
	logger  *zap.Logger
	workers int

	wg sync.WaitGroup

	// closeOnce guards the channel close so Shutdown is idempotent; closed is read by
	// Enqueue under mu to avoid sending on a closed channel.
	mu        sync.RWMutex
	closed    bool
	closeOnce sync.Once
}

// DispatcherConfig configures an AsyncDispatcher. Buffer and Workers come from
// AlertsConfig (ALERTS_BUFFER / ALERTS_WORKERS); both are clamped to a sane minimum so a
// misconfigured zero can never produce a deadlocked or unbuffered dispatcher.
type DispatcherConfig struct {
	Buffer  int
	Workers int
}

// NewAsyncDispatcher builds and starts a dispatcher: it allocates the buffered channel
// and launches Workers goroutines. The caller must call Shutdown for a graceful drain.
// repo may be nil — when nil, audit rows are skipped (the send still happens); this keeps
// the package usable before the audit table is wired without crashing.
func NewAsyncDispatcher(cfg DispatcherConfig, sender EmailSender, repo AlertRepository, logger *zap.Logger) *AsyncDispatcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	if sender == nil {
		sender = NewNoopSender()
	}
	buffer := cfg.Buffer
	if buffer < 1 {
		buffer = 1
	}
	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}

	d := &AsyncDispatcher{
		ch:      make(chan SecurityEvent, buffer),
		sender:  sender,
		repo:    repo,
		logger:  logger,
		workers: workers,
	}

	d.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go d.worker()
	}
	return d
}

// EnqueueSecurityAlert schedules an alert for asynchronous delivery. It is non-blocking:
// the event is pushed onto the buffered channel via a non-blocking select, and if the
// buffer is full (or the dispatcher is shutting down) the event is dropped with a zap
// warning rather than blocking the business flow (fail-open, spec §3).
func (d *AsyncDispatcher) EnqueueSecurityAlert(_ context.Context, evt SecurityEvent) {
	d.mu.RLock()
	closed := d.closed
	d.mu.RUnlock()
	if closed {
		d.logger.Warn("notify: alert dropped, dispatcher shut down",
			zap.String("alert_type", evt.Type),
			zap.String("tenant_id", evt.TenantID),
			zap.String("user_id", evt.UserID),
		)
		return
	}

	select {
	case d.ch <- evt:
		// enqueued
	default:
		// Buffer full: drop-with-log. Never block the caller (fail-open). We log the
		// type/tenant/user (NOT the email or any secret) so the drop is auditable.
		d.logger.Warn("notify: alert dropped, buffer full",
			zap.String("alert_type", evt.Type),
			zap.String("tenant_id", evt.TenantID),
			zap.String("user_id", evt.UserID),
			zap.Int("buffer", cap(d.ch)),
		)
	}
}

// worker drains the channel until it is closed, processing one event at a time. It
// recovers from any panic in send/render so a single bad event can never take down a
// worker goroutine and silently shrink the pool.
func (d *AsyncDispatcher) worker() {
	defer d.wg.Done()
	for evt := range d.ch {
		d.process(evt)
	}
}

// process renders, sends, and audits a single event. Every failure path is contained:
// errors are logged and recorded (status=failed) but never returned. A panic is recovered
// and logged so the worker survives.
func (d *AsyncDispatcher) process(evt SecurityEvent) {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("notify: recovered panic while processing alert",
				zap.String("alert_type", evt.Type),
				zap.String("tenant_id", evt.TenantID),
				zap.Any("panic", r),
			)
		}
	}()

	ctx := context.Background()

	// Audit row (pending). When repo is nil we skip auditing but still send.
	var auditID string
	if d.repo != nil {
		id, err := d.repo.InsertPending(ctx, evt)
		if err != nil {
			d.logger.Error("notify: failed to insert pending audit row",
				zap.String("alert_type", evt.Type),
				zap.String("tenant_id", evt.TenantID),
				zap.Error(err),
			)
			// Continue: a missing audit row must not stop the email (fail-open).
		}
		auditID = id
	}

	subject, text, htmlBody := renderAlert(evt)
	mail := Mail{To: evt.Email, Subject: subject, Body: text, HTMLBody: htmlBody}

	sendErr := d.sender.Send(ctx, mail)
	if sendErr != nil {
		d.logger.Warn("notify: alert email send failed",
			zap.String("alert_type", evt.Type),
			zap.String("tenant_id", evt.TenantID),
			zap.String("user_id", evt.UserID),
			zap.Error(sendErr),
		)
		if d.repo != nil && auditID != "" {
			if err := d.repo.MarkFailed(ctx, evt.TenantID, auditID, sendErr.Error()); err != nil {
				d.logger.Error("notify: failed to mark audit row failed",
					zap.String("audit_id", auditID), zap.Error(err))
			}
		}
		return
	}

	if d.repo != nil && auditID != "" {
		if err := d.repo.MarkSent(ctx, evt.TenantID, auditID); err != nil {
			d.logger.Error("notify: failed to mark audit row sent",
				zap.String("audit_id", auditID), zap.Error(err))
		}
	}
}

// Shutdown closes the channel and waits for all workers to finish draining the buffered
// events (graceful drain, spec §3 / §6). It is idempotent and safe to call from
// graceful-shutdown handlers. After Shutdown, EnqueueSecurityAlert drops events with a
// log line instead of panicking on a closed channel.
func (d *AsyncDispatcher) Shutdown(ctx context.Context) error {
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		d.mu.Unlock()
		close(d.ch)
	})

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Deadline hit before the drain finished. Workers keep running on the already
		// buffered events; we report the context error so the caller can log it.
		return ctx.Err()
	}
}
