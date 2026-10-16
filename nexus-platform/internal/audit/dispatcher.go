package audit

import (
	"context"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

// Writer is the dispatcher's consumer-side view of the audit_log table: a single
// append-only insert. Defining the interface here (not in the concrete repo) lets unit
// tests substitute a fake (including one that returns an error, to prove fail-open)
// without a database.
type Writer interface {
	// InsertEvent appends one row to audit_log. It is the only write the trail ever
	// performs (append-only). Returning an error must NOT propagate to the business flow —
	// the dispatcher logs it and moves on.
	InsertEvent(ctx context.Context, evt Event) error
}

// AsyncDispatcher is the concrete Auditor. Record pushes the event onto a buffered channel
// and returns immediately; a pool of worker goroutines drains the channel and appends each
// event to audit_log via the Writer.
//
// Fail-open guarantees (spec §3/§5):
//   - A full buffer makes Record drop the event with a zap warning — it NEVER blocks the
//     calling business flow and NEVER panics.
//   - After Shutdown, further Records are dropped with a log line (no send on a closed
//     channel panic).
//   - A write error is logged; it never propagates to the caller.
//   - A panic in the writer is recovered so one bad event cannot shrink the worker pool.
type AsyncDispatcher struct {
	ch      chan Event
	writer  Writer
	logger  *zap.Logger
	workers int

	// dropped counts events the buffer refused. Fail-open is right for the hot path, but a
	// drop means a mutation happened with NO record of it — and until this counter existed the
	// only trace was one warn line per event, which is exactly the shape nobody reads. A number
	// that can be asserted in a test, and read after a mass operation, is what makes "we might
	// have lost history" a question with an answer.
	dropped atomic.Int64

	wg sync.WaitGroup

	// closeOnce guards the channel close so Shutdown is idempotent; closed is read by
	// Record under mu to avoid sending on a closed channel.
	mu        sync.RWMutex
	closed    bool
	closeOnce sync.Once
}

// DispatcherConfig configures an AsyncDispatcher. Buffer and Workers are clamped to a sane
// minimum so a misconfigured zero can never produce a deadlocked or unbuffered dispatcher.
type DispatcherConfig struct {
	Buffer  int
	Workers int
}

// NewAsyncDispatcher builds and starts a dispatcher: it allocates the buffered channel and
// launches Workers goroutines. The caller must call Shutdown for a graceful drain. writer
// may be nil — when nil, events are accepted and silently discarded after draining (keeps
// the package usable before the audit table is wired without crashing); a nil-writer
// dispatcher is effectively a logging no-op.
func NewAsyncDispatcher(cfg DispatcherConfig, writer Writer, logger *zap.Logger) *AsyncDispatcher {
	if logger == nil {
		logger = zap.NewNop()
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
		ch:      make(chan Event, buffer),
		writer:  writer,
		logger:  logger,
		workers: workers,
	}

	d.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go d.worker()
	}
	return d
}

// Record schedules an audit event for asynchronous, append-only persistence. It is
// non-blocking: the event is pushed onto the buffered channel via a non-blocking select,
// and if the buffer is full (or the dispatcher is shutting down) the event is dropped with
// a zap warning rather than blocking the business flow (fail-open, spec §3).
func (d *AsyncDispatcher) Record(_ context.Context, evt Event) {
	d.mu.RLock()
	closed := d.closed
	d.mu.RUnlock()
	if closed {
		d.dropped.Add(1)
		d.logger.Warn("audit: event dropped, dispatcher shut down",
			zap.String("action", evt.Action),
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName),
		)
		return
	}

	select {
	case d.ch <- evt:
		// enqueued
	default:
		// Buffer full: drop-with-log. Never block the caller (fail-open). We log the
		// action/tenant/entity (NOT the diff, which may carry record data) so the drop is
		// auditable without leaking field values.
		d.dropped.Add(1)
		d.logger.Warn("audit: event dropped, buffer full",
			zap.String("action", evt.Action),
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName),
			zap.Int("buffer", cap(d.ch)),
		)
	}
}

// worker drains the channel until it is closed, processing one event at a time.
func (d *AsyncDispatcher) worker() {
	defer d.wg.Done()
	for evt := range d.ch {
		d.process(evt)
	}
}

// process appends a single event. Every failure path is contained: a write error is logged
// but never returned, and a panic is recovered so the worker survives.
func (d *AsyncDispatcher) process(evt Event) {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("audit: recovered panic while writing event",
				zap.String("action", evt.Action),
				zap.String("tenant_id", evt.TenantID),
				zap.Any("panic", r),
			)
		}
	}()

	if d.writer == nil {
		return
	}
	if err := d.writer.InsertEvent(context.Background(), evt); err != nil {
		d.logger.Warn("audit: failed to write audit row",
			zap.String("action", evt.Action),
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName),
			zap.String("record_id", evt.RecordID),
			zap.Error(err),
		)
	}
}

// Shutdown closes the channel and waits for all workers to finish draining the buffered
// events (graceful drain, spec §3). It is idempotent and safe to call from
// graceful-shutdown handlers. After Shutdown, Record drops events with a log line instead
// of panicking on a closed channel.
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

// Dropped is the number of events this dispatcher refused since start (buffer full, or after
// shutdown). Non-zero means mutations happened that the trail does not record — read it after a
// mass operation, and assert it in tests that care whether history survived a burst.
func (d *AsyncDispatcher) Dropped() int64 { return d.dropped.Load() }

// BufferFor sizes an audit buffer so that ONE burst of the largest size the app is allowed to
// produce cannot overflow it on its own. It exists because those numbers were configured in
// different files and drifted: the buffer took the alert dispatcher's default (256) while a single
// POST /:entity/bulk-update is allowed MAX_BULK_OPS rows (default 500), each emitting its own
// event. A 500-row mass update could therefore silently lose ~half its before-images — and a
// before-image is the ONLY thing that makes a mass edit reconstructible, which is exactly why the
// "clear to empty" switch was held back on safety grounds.
//
// bursts is VARIADIC because there is more than one bulk door and each has its own independently
// settable cap (MAX_BULK_OPS for bulk-update/delete, BULK_MAX_ROWS for bulk-create). Today the
// second is smaller, so flooring on the first alone happens to be enough — but "happens to be
// enough because of a default someone can change in another file" is the exact shape of the bug
// this function exists to end. Pass every cap; the floor is their maximum.
//
// This does not make drops impossible — several concurrent bursts still can — but it removes the
// case where the product's OWN documented limits overrun its own trail.
func BufferFor(configured int, bursts ...int) int {
	out := configured
	for _, b := range bursts {
		if b > out {
			out = b
		}
	}
	return out
}
