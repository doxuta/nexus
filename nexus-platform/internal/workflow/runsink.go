package workflow

// specs/048 S3 — run history of the synchronous before rules (FR-030..032). A before rule runs INSIDE
// a save, so its run row must not cost that save a DB round trip: RecordService hands the run to a
// RunSink and returns; one worker writes it through the same RunRecorder the async engine uses
// (sys_workflow_runs). Fail-open like every other run write: a full buffer drops the run with a
// warning — the history loses a line, the save never waits.

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// runSinkBuffer bounds the runs waiting for the worker. One refused all-or-nothing batch or CSV
// import is one summary run per rule, so only a flood of single refused saves can fill it.
const runSinkBuffer = 1024

// runSinkWriteTimeout bounds one INSERT so a stuck database cannot hold the drain on Shutdown forever.
const runSinkWriteTimeout = 5 * time.Second

// RunSink is the asynchronous writer of before-rule runs. A nil *RunSink records nothing, so an
// unwired RecordService (unit tests, tools) needs no guard at every call site.
type RunSink struct {
	ch   chan RunInput
	rec  RunRecorder
	log  *zap.Logger
	done chan struct{}

	// mu guards closed AND the send: Record holds the read lock across its non-blocking send, so
	// Close can never close the channel between the check and the send.
	mu     sync.RWMutex
	closed bool
}

// NewRunSink starts the worker. Close drains it.
func NewRunSink(rec RunRecorder, log *zap.Logger) *RunSink {
	if log == nil {
		log = zap.NewNop()
	}
	s := &RunSink{ch: make(chan RunInput, runSinkBuffer), rec: rec, log: log, done: make(chan struct{})}
	go s.work()
	return s
}

// Record queues one run and returns at once. A full buffer, or a sink already closed, drops the run
// with a warning — never a field value in the log line, only which rule and tenant.
func (s *RunSink) Record(in RunInput) {
	if s == nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.closed {
		select {
		case s.ch <- in:
			return
		default:
		}
	}
	s.log.Warn("workflow.RunSink: before-rule run dropped (buffer full or shutting down)",
		zap.String("tenant_id", in.TenantID), zap.String("workflow_id", in.WorkflowID),
		zap.String("status", string(in.Status)), zap.Int("buffer", cap(s.ch)))
}

func (s *RunSink) work() {
	defer close(s.done)
	for in := range s.ch {
		ctx, cancel := context.WithTimeout(context.Background(), runSinkWriteTimeout)
		if err := s.rec.InsertRun(ctx, in); err != nil {
			s.log.Warn("workflow.RunSink: failed to record before-rule run (fail-open)",
				zap.String("tenant_id", in.TenantID), zap.String("workflow_id", in.WorkflowID), zap.Error(err))
		}
		cancel()
	}
}

// Close stops accepting runs and waits for the buffered ones to be written, up to ctx. Idempotent.
func (s *RunSink) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
