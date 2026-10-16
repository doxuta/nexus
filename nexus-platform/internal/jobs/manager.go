package jobs

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Store is the manager's consumer-side view of the queue persistence. Satisfied by *SQLStore; a
// fake drives the unit tests. All timestamps travel from the manager (injectable clock).
type Store interface {
	// ClaimNext atomically claims the oldest due queued job for workerID, or returns (nil, nil).
	ClaimNext(ctx context.Context, workerID string, now time.Time, lane Lane) (*Job, error)
	// MarkDone finishes a claimed job as done; applied=false means the claim was lost (reaped).
	MarkDone(ctx context.Context, id, workerID string, finishedAt time.Time) (bool, error)
	// Reschedule requeues a failed-attempt job for runAt with the error recorded.
	Reschedule(ctx context.Context, id, workerID string, runAt time.Time, lastError string) (bool, error)
	// Postpone puts a claimed job back in the queue for runAt WITHOUT spending an attempt: ClaimNext
	// already added one, Postpone takes it back (specs/068 plan D-14). Same lost-claim contract.
	Postpone(ctx context.Context, id, workerID string, runAt time.Time) (bool, error)
	// MarkFailed finishes a claimed job as terminally failed.
	MarkFailed(ctx context.Context, id, workerID, lastError string, finishedAt time.Time) (bool, error)
	// ReapStuck requeues/fails running jobs whose claimed_at is older than cutoff.
	ReapStuck(ctx context.Context, cutoff time.Time) (requeued, failed int64, err error)
}

// ManagerConfig tunes the worker pool. Non-positive values are clamped to safe defaults (the
// JOBS_* env parsing already degrades-to-default; this is the second belt).
type ManagerConfig struct {
	Workers      int           // pool size (JOBS_WORKERS, default 2)
	PollInterval time.Duration // idle sleep between empty claims + reaper cadence (JOBS_POLL_INTERVAL_SEC)
	Lease        time.Duration // running-job lease before the reaper reclaims it (JOBS_LEASE_SEC)
	// Dedicated gives a job type workers of its own (review of DAILY-QUEUE #26): only they claim
	// it and the shared pool of Workers never does. For a type whose attempt can hold a worker for
	// a long time — outbound_webhook against a receiver that swallows packets waits out the whole
	// HTTP timeout — so a flood of it saturates ITS lane and never delays another type's jobs.
	// A count < 1 is clamped to 1 (the type is excluded from the shared pool, so zero workers
	// would strand it). Empty = one shared pool, as before.
	Dedicated map[string]int
}

// Manager runs the queue's in-process side: a pool of claim→run workers plus one reaper ticker.
// It shares the janitor's lifecycle contract: idempotent Start/Stop, graceful drain (a worker
// finishes its in-flight job before exiting), injectable clock for tests.
type Manager struct {
	store    Store
	registry *Registry
	cfg      ManagerConfig

	// backoffBase/backoffCap shape the retry delay (Backoff). Package defaults in production;
	// overridden in tests to keep retries fast.
	backoffBase time.Duration
	backoffCap  time.Duration

	now          func() time.Time
	logger       *zap.Logger
	workerPrefix string

	// tenantGate (specs/068 slice B), when set, answers "is this tenant write-frozen, until when?"
	// before a job runs; a frozen tenant's job is Postponed past the deadline.
	tenantGate TenantGate

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	wg        sync.WaitGroup
}

// NewManager builds (does NOT start) a manager. A nil logger is replaced with a no-op.
func NewManager(cfg ManagerConfig, store Store, registry *Registry, logger *zap.Logger) *Manager {
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.Workers < 1 {
		cfg.Workers = 2
	}
	if cfg.PollInterval < time.Second {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.Lease < time.Second {
		cfg.Lease = 5 * time.Minute
	}
	dedicated := make(map[string]int, len(cfg.Dedicated))
	for jobType, n := range cfg.Dedicated {
		dedicated[jobType] = max(n, 1)
	}
	cfg.Dedicated = dedicated
	host, _ := os.Hostname()
	return &Manager{
		store:        store,
		registry:     registry,
		cfg:          cfg,
		backoffBase:  DefaultBackoffBase,
		backoffCap:   DefaultBackoffCap,
		now:          time.Now,
		logger:       logger,
		workerPrefix: buildWorkerPrefix(host, os.Getpid()),
		stop:         make(chan struct{}),
	}
}

// TenantGate reports whether a tenant is write-frozen and until when. The caller (main.go) adapts
// services.TenantFreeze; a read error must already be folded into the answer (FR-030a).
type TenantGate func(ctx context.Context, tenantID string) (until time.Time, frozen bool)

// postponeJitter spreads the jobs of a just-unfrozen tenant so they do not all fire in the same second.
const postponeJitter = 30 * time.Second

// WithTenantGate arms the per-tenant freeze check in runJob. Chainable; nil keeps it off.
func (m *Manager) WithTenantGate(g TenantGate) *Manager {
	m.tenantGate = g
	return m
}

// buildWorkerPrefix builds the "<host>-<pid>" claim-identity prefix. claimed_by IS the lost-lease
// guard (`WHERE claimed_by = ?`), and the full worker id is "<prefix>-w<N>", so the prefix MUST
// leave room for the distinguishing "-w<N>" suffix inside the sys_jobs.claimed_by VARCHAR(64)
// column. A k8s pod hostname is a 63-char DNS label; the host is clamped to 40 so
// "<host≤40>-<pid up to 7 digits>-w<N up to 2 digits>" is always ≤64 with the suffix intact —
// otherwise every worker in a pod (and across restarts on the same host) would collapse to one
// claimed_by and the guard would let a reaped-then-reclaimed job be clobbered by its stale owner.
func buildWorkerPrefix(host string, pid int) string {
	if host == "" {
		host = "nexus"
	}
	if len(host) > 40 {
		host = host[:40]
	}
	return fmt.Sprintf("%s-%d", host, pid)
}

// Start launches the worker pool + the reaper goroutine (idempotent).
func (m *Manager) Start() {
	m.startOnce.Do(func() {
		lanes := m.workerLanes()
		m.wg.Add(len(lanes) + 1)
		for i, lane := range lanes {
			// One running index across lanes: the id stays "<prefix>-w<N>" (see buildWorkerPrefix).
			go m.worker(fmt.Sprintf("%s-w%d", m.workerPrefix, i), lane)
		}
		go m.reaper()
	})
}

// workerLanes is the lane of every worker: Workers shared ones claiming every type except the
// dedicated types, then each dedicated type's own (sorted, so worker ids are stable across boots).
func (m *Manager) workerLanes() []Lane {
	types := make([]string, 0, len(m.cfg.Dedicated))
	for jobType := range m.cfg.Dedicated {
		types = append(types, jobType)
	}
	sort.Strings(types)
	var lanes []Lane
	for i := 0; i < m.cfg.Workers; i++ {
		lanes = append(lanes, Lane{Exclude: types})
	}
	for _, jobType := range types {
		for i := 0; i < m.cfg.Dedicated[jobType]; i++ {
			lanes = append(lanes, Lane{Only: jobType})
		}
	}
	return lanes
}

// worker is the claim→run loop: claim a due job its lane may take, run it, and immediately try to
// claim the next (fast drain); when nothing is due (or the claim errored), sleep one poll
// interval. It exits on Stop, finishing its in-flight job first (graceful drain).
func (m *Manager) worker(workerID string, lane Lane) {
	defer m.wg.Done()
	for {
		select {
		case <-m.stop:
			return
		default:
		}
		job, err := m.store.ClaimNext(context.Background(), workerID, m.now().UTC(), lane)
		if err != nil {
			m.logger.Warn("jobs: claim failed (retrying after poll interval)",
				zap.String("worker_id", workerID), zap.Error(err))
			if !m.sleep(m.cfg.PollInterval) {
				return
			}
			continue
		}
		if job == nil {
			if !m.sleep(m.cfg.PollInterval) {
				return
			}
			continue
		}
		m.runJob(workerID, job)
	}
}

// runJob executes one claimed job and applies the outcome transition:
//
//	no handler          → failed immediately (an unregistered type is a wiring bug — never retried)
//	handler nil error   → done
//	Permanent(error)    → failed immediately (the handler knows no retry can succeed)
//	handler error/panic → attempts < max: requeue at now + Backoff(attempts); else: failed
//
// A lost claim (the reaper handed the job to another worker while we ran) is logged and dropped —
// the re-execution owns the row. Store errors are logged fail-open: the job stays 'running' and
// the reaper recovers it after the lease.
func (m *Manager) runJob(workerID string, job *Job) {
	now := m.now().UTC()
	h, ok := m.registry.Handler(job.Type)
	if !ok {
		m.finishLogged("mark failed", workerID, job, func() (bool, error) {
			return m.store.MarkFailed(context.Background(), job.ID, workerID,
				fmt.Sprintf("jobs: no handler registered for job_type %q", job.Type), now)
		})
		m.logger.Error("jobs: unregistered job_type (failed without retry)",
			zap.String("job_id", job.ID), zap.String("job_type", job.Type),
			zap.String("tenant_id", job.TenantID))
		return
	}

	// specs/068: a frozen tenant's job waits for the freeze to lapse, and keeps its attempt budget.
	if m.tenantGate != nil && job.TenantID != "" {
		if until, frozen := m.tenantGate(context.Background(), job.TenantID); frozen {
			runAt := until.UTC().Add(rand.N(postponeJitter))
			m.finishLogged("postpone", workerID, job, func() (bool, error) {
				return m.store.Postpone(context.Background(), job.ID, workerID, runAt)
			})
			m.logger.Info("jobs: tenant under maintenance, job postponed",
				zap.String("job_id", job.ID), zap.String("job_type", job.Type),
				zap.String("tenant_id", job.TenantID), zap.Time("run_at", runAt))
			return
		}
	}

	err := m.safeRun(h, job)
	now = m.now().UTC()
	if err == nil {
		m.finishLogged("mark done", workerID, job, func() (bool, error) {
			return m.store.MarkDone(context.Background(), job.ID, workerID, now)
		})
		return
	}

	if permanent := IsPermanent(err); permanent || job.Attempts >= job.MaxAttempts {
		m.finishLogged("mark failed", workerID, job, func() (bool, error) {
			return m.store.MarkFailed(context.Background(), job.ID, workerID, err.Error(), now)
		})
		reason := "attempt budget exhausted"
		if permanent {
			reason = "handler marked the error permanent"
		}
		m.logger.Error("jobs: job failed permanently",
			zap.String("job_id", job.ID), zap.String("job_type", job.Type),
			zap.String("tenant_id", job.TenantID), zap.String("reason", reason),
			zap.Int("attempts", job.Attempts), zap.Error(err))
		return
	}

	delay := Backoff(m.backoffBase, m.backoffCap, job.Attempts)
	m.finishLogged("reschedule", workerID, job, func() (bool, error) {
		return m.store.Reschedule(context.Background(), job.ID, workerID, now.Add(delay), err.Error())
	})
	m.logger.Warn("jobs: attempt failed, rescheduled with backoff",
		zap.String("job_id", job.ID), zap.String("job_type", job.Type),
		zap.String("tenant_id", job.TenantID),
		zap.Int("attempts", job.Attempts), zap.Int("max_attempts", job.MaxAttempts),
		zap.Duration("retry_in", delay), zap.Error(err))
}

// finishLogged runs one outcome transition and logs the two non-happy paths: a store error
// (fail-open — the reaper will recover the still-running row after the lease) and a lost claim.
func (m *Manager) finishLogged(op string, workerID string, job *Job, fn func() (bool, error)) {
	applied, err := fn()
	if err != nil {
		m.logger.Error("jobs: "+op+" failed (job stays running; reaper will recover it)",
			zap.String("job_id", job.ID), zap.String("job_type", job.Type), zap.Error(err))
		return
	}
	if !applied {
		m.logger.Warn("jobs: claim lost before "+op+" (lease expired; another execution owns the job)",
			zap.String("job_id", job.ID), zap.String("job_type", job.Type),
			zap.String("worker_id", workerID))
	}
}

// safeRun executes the handler under the lease deadline, converting a panic into an error so one
// bad job can never kill a worker goroutine. The ctx deadline (= lease) nudges well-behaved
// handlers to stop before the reaper could hand their job to another worker.
func (m *Manager) safeRun(h HandlerFunc, job *Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("jobs: handler panic: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Lease)
	defer cancel()
	return h(ctx, *job)
}

// reaper ticks every poll interval and recovers running jobs whose claim is older than the lease
// (crashed worker). Errors are logged fail-open — a transient DB error must not kill the ticker.
func (m *Manager) reaper() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			cutoff := m.now().UTC().Add(-m.cfg.Lease)
			requeued, failed, err := m.store.ReapStuck(context.Background(), cutoff)
			if err != nil {
				m.logger.Warn("jobs: reap failed (fail-open)", zap.Error(err))
				continue
			}
			if requeued > 0 || failed > 0 {
				m.logger.Info("jobs: reaped stale running jobs",
					zap.Int64("requeued", requeued), zap.Int64("failed", failed),
					zap.Duration("lease", m.cfg.Lease))
			}
		}
	}
}

// sleep waits d or until Stop; it reports false when the manager is stopping.
func (m *Manager) sleep(d time.Duration) bool {
	select {
	case <-m.stop:
		return false
	case <-time.After(d):
		return true
	}
}

// Stop signals every goroutine to exit and waits (up to the context deadline) for the drain: a
// worker finishes its in-flight job first. Idempotent. A job still running past the deadline is
// NOT lost — it stays 'running' and the reaper of the next boot reclaims it after the lease
// (that is the durability story).
func (m *Manager) Stop(ctx context.Context) error {
	m.stopOnce.Do(func() { close(m.stop) })
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
