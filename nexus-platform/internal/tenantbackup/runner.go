package tenantbackup

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
)

// ErrStopping is returned by Runner.Go once the server is shutting down.
var ErrStopping = errors.New("tenantbackup: runner is stopping")

// defaultBeat is how often a backup that is waiting for its turn proves it is alive. It stays well
// under the reaper's 30-minute threshold so a queued backup is never failed as abandoned.
const defaultBeat = 60 * time.Second

// Runner runs backup jobs in goroutines, ONE at a time per process (a semaphore of one): a backup
// streams a whole customer through memory-bounded buffers and the disk, and two at once would only
// slow both. It is deliberately NOT sys_jobs: that queue's 5-minute lease would let its reaper
// start a second copy of a backup that takes longer than five minutes. Cross-replica exclusion is
// the database's job (op_lock), not this semaphore's.
//
// The job's context is detached from the HTTP request (the request returned 202 long ago) but
// carries the actor for the audit trail, and is cancelled by Stop.
type Runner struct {
	sem    chan struct{}
	base   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
	beat   time.Duration
	logger *zap.Logger
}

// NewRunner builds a runner. logger may be nil.
func NewRunner(logger *zap.Logger) *Runner {
	if logger == nil {
		logger = zap.NewNop()
	}
	base, cancel := context.WithCancel(context.Background())
	return &Runner{sem: make(chan struct{}, 1), base: base, cancel: cancel, beat: defaultBeat, logger: logger}
}

// Go schedules job. While it waits for the semaphore it calls beat every r.beat; a beat that
// reports alive=false (the catalogue row is gone or no longer running) abandons the job without
// running it. If the runner is stopped while waiting, job runs once with a cancelled context so it
// can record the cancellation itself.
func (r *Runner) Go(actorUserID string, beat func(ctx context.Context) (alive bool), job func(ctx context.Context)) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrStopping
	}
	r.wg.Add(1)
	r.mu.Unlock()

	ctx := audit.WithActor(r.base, actorUserID)
	go func() {
		defer r.wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				r.logger.Error("tenantbackup.Runner: job panicked", zap.Any("panic", rec))
			}
		}()
		ticker := time.NewTicker(r.beat)
		defer ticker.Stop()
		for {
			select {
			case r.sem <- struct{}{}:
				defer func() { <-r.sem }()
				job(ctx)
				return
			case <-ticker.C:
				if !beat(ctx) {
					return
				}
			case <-ctx.Done():
				job(ctx) // observes the cancellation and fails its own row
				return
			}
		}
	}()
	return nil
}

// Stop cancels running jobs and waits for them to finish recording their outcome, up to ctx.
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
