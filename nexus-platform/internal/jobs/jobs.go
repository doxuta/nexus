// Package jobs is the durable job queue (JOBS-1, engine-depth primitive #0): a sys_jobs row per
// unit of background work, an atomic claim (SELECT ... FOR UPDATE SKIP LOCKED, MySQL 8), a small
// in-process worker pool, retry with capped exponential backoff, and a reaper that requeues jobs
// whose worker crashed mid-run. It replaces fire-and-forget goroutines for work that must survive
// a restart, and is the substrate the timer/deadline engine and campaign send will stand on
// (../specs/021-durable-job-queue/spec.md).
//
// Delivery semantics are AT-LEAST-ONCE: a worker that dies after finishing the work but before
// marking the row done leaves the job to be reaped and re-run, so handlers should be idempotent
// where re-execution matters. attempts is incremented AT CLAIM TIME, so even a job that
// hard-crashes the whole process burns its budget across reap/reclaim cycles — a poison job can
// never retry forever.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Job statuses persisted in sys_jobs.status. A job is claimable when status=queued AND
// run_at <= now; done/failed are terminal (swept by the retention janitor via finished_at).
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Default backoff shape for retries: delay = min(DefaultBackoffBase * 2^(attempts-1),
// DefaultBackoffCap). With the default 5 max attempts the retry ladder is 30s, 1m, 2m, 4m.
const (
	DefaultBackoffBase = 30 * time.Second
	DefaultBackoffCap  = time.Hour
)

// Job is one unit of background work. As Enqueue input only Type is required (TenantID empty =
// system job, RunAt zero = now, MaxAttempts <= 0 = store default); as ClaimNext output it carries
// the claimed row (Attempts already includes the claim in flight).
type Job struct {
	ID          string
	TenantID    string // empty = system job (NULL in sys_jobs.tenant_id)
	Type        string // handler registry key
	Payload     json.RawMessage
	Status      string
	RunAt       time.Time
	Attempts    int // attempts made so far, INCLUDING the in-flight one (incremented at claim)
	MaxAttempts int
	ClaimedBy   string
}

// Lane scopes which job types a worker's claim may take (ManagerConfig.Dedicated). The zero Lane
// takes every type.
type Lane struct {
	Only    string   // non-empty: this job type and nothing else (a dedicated lane)
	Exclude []string // every type except these (the shared lane); ignored when Only is set
}

// HandlerFunc executes one claimed job. A returned error makes the queue retry the job with
// backoff until MaxAttempts, then mark it failed with the error text as last_error — unless the
// error is marked Permanent, which fails the job on the spot. The ctx carries the worker-lease
// deadline — a handler that outlives the lease risks the reaper handing the job to another
// worker, so it must honour ctx cancellation.
type HandlerFunc func(ctx context.Context, job Job) error

// permanentError marks a handler failure that no retry can fix (a 4xx from a webhook receiver, a
// job pointing at a rule that no longer exists). Error() is the cause's text unchanged, so
// last_error reads exactly as the handler wrote it.
type permanentError struct{ cause error }

func (e *permanentError) Error() string { return e.cause.Error() }
func (e *permanentError) Unwrap() error { return e.cause }

// Permanent marks err as not worth retrying: the manager fails the job at once instead of burning
// the rest of its attempt budget (and the receiver's patience) on a request that cannot succeed.
// The marker survives %w wrapping. Permanent(nil) is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{cause: err}
}

// IsPermanent reports whether err (or anything it wraps) was marked by Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// Registry maps a job_type to its HandlerFunc. Registration happens at boot (before the manager
// starts); lookups happen concurrently from every worker, hence the RWMutex. A claimed job whose
// type has no handler is marked failed immediately (never retried — an unregistered type is a
// wiring bug, not a transient fault).
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]HandlerFunc
}

// NewRegistry builds an empty handler registry.
func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]HandlerFunc)}
}

// Register binds jobType to h. An empty type, nil handler, or duplicate registration is an error
// (a duplicate would silently shadow the first handler — a boot-time wiring bug worth failing on).
func (r *Registry) Register(jobType string, h HandlerFunc) error {
	if jobType == "" {
		return fmt.Errorf("jobs.Registry.Register: empty job type")
	}
	if h == nil {
		return fmt.Errorf("jobs.Registry.Register: nil handler for job type %q", jobType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[jobType]; exists {
		return fmt.Errorf("jobs.Registry.Register: job type %q already registered", jobType)
	}
	r.handlers[jobType] = h
	return nil
}

// Handler returns the handler registered for jobType, if any.
func (r *Registry) Handler(jobType string) (HandlerFunc, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[jobType]
	return h, ok
}

// Backoff returns the delay before the NEXT attempt after `attempts` attempts have been made:
// min(base * 2^(attempts-1), max) — capped exponential. Non-positive base/max fall back to the
// package defaults; attempts < 1 is clamped to 1. The doubling loop exits at the cap, so a huge
// attempts value can never overflow the duration.
func Backoff(base, max time.Duration, attempts int) time.Duration {
	if base <= 0 {
		base = DefaultBackoffBase
	}
	if max <= 0 {
		max = DefaultBackoffCap
	}
	if attempts < 1 {
		attempts = 1
	}
	d := base
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}
