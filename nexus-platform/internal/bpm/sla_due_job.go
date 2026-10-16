package bpm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/jobs"
)

// SLADueJobType is the sys_jobs type of the durable approval-breach wake-up (specs/043 S3c). One
// job per (instance, seq): it fires at the step's sla_due_at and runs the same claim + notify the
// sweep does. The sweep (SLAScheduler ticker) stays as the fallback for rows no job covers (armed
// by an older binary, queue off at the time, job failed).
const SLADueJobType = "bpm_sla_due"

// breachNotifyCap bounds the people one breach pings (a role of thousands must not fan out).
const breachNotifyCap = 50

// slaDuePayload is the job payload. The tenant is NOT in it: it is the job row's tenant_id, which
// is the only value the handler trusts (a payload can never point a job at another tenant).
type slaDuePayload struct {
	InstanceID string `json:"instanceId"`
	Seq        int64  `json:"seq"`
}

// DueJobQueue is what the repository needs of the job queue: insert on the caller's transaction.
// Satisfied by *jobs.SQLStore.
type DueJobQueue interface {
	EnqueueExec(ctx context.Context, ex sqlx.ExecerContext, j jobs.Job) (string, error)
}

// WithSLADueJobs makes every state write that leaves a step with a deadline park its breach job in
// the same transaction. Leave unset when the queue is off — nobody would run the job (no black
// hole); the sweep then remains the only path.
func (r *SQLRepository) WithSLADueJobs(q DueJobQueue) *SQLRepository {
	r.dueJobs = q
	return r
}

// enqueueSLADue parks the breach job of the step an instance stands on after a write that left it
// at `seq`. It is called from the two — and only two — statements that write sla_due_at
// (CreateInstance, DecideTaskTx): handlers skip a job whose seq has moved on, so every write that
// bumps seq while keeping or setting a deadline must leave a fresh job. A step with no deadline
// or an already-escalated step has nothing to wake up for.
func (r *SQLRepository) enqueueSLADue(ctx context.Context, ex sqlx.ExecerContext, tenantID, instanceID string, seq int64, due *time.Time, escalated bool) error {
	if r.dueJobs == nil || due == nil || escalated {
		return nil
	}
	payload, err := json.Marshal(slaDuePayload{InstanceID: instanceID, Seq: seq})
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.enqueueSLADue: marshal: %w", err)
	}
	if _, err := r.dueJobs.EnqueueExec(ctx, ex, jobs.Job{
		TenantID: tenantID, Type: SLADueJobType, Payload: payload, RunAt: due.UTC(),
	}); err != nil {
		return fmt.Errorf("bpm.SQLRepository.enqueueSLADue: %w", err)
	}
	return nil
}

// DueStore is the job handler's read of an instance. Satisfied by *SQLRepository.
type DueStore interface {
	GetInstance(ctx context.Context, tenantID, id string) (*Instance, error)
}

// NewSLADueJobHandler returns the sys_jobs handler of SLADueJobType. A job whose instance moved
// on (decided, delegated, escalated, deleted) or whose deadline changed is a no-op; one that
// fires before its deadline (clock skew) errors so the queue retries it; a claim failure errors
// too — the job must not be lost to a transient DB fault.
func (s *SLAScheduler) NewSLADueJobHandler(store DueStore) jobs.HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var p slaDuePayload
		if job.TenantID == "" || json.Unmarshal(job.Payload, &p) != nil || p.InstanceID == "" {
			return jobs.Permanent(fmt.Errorf("bpm.SLAScheduler.sladue: malformed job %s", job.ID))
		}
		inst, err := store.GetInstance(ctx, job.TenantID, p.InstanceID)
		if errors.Is(err, ErrInstanceNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("bpm.SLAScheduler.sladue: %w", err)
		}
		if inst.Status != StatusPending || inst.Escalated || inst.Seq != p.Seq || inst.SLADueAt == nil || inst.SLAPausedAt != nil {
			return nil // stale: a later write of this instance parked its own job
		}
		now := s.now()
		if inst.SLADueAt.After(now) {
			return fmt.Errorf("bpm.SLAScheduler.sladue: deadline %s not reached at %s", inst.SLADueAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
		}
		if err := s.fireBreach(ctx, *inst, now); err != nil {
			return fmt.Errorf("bpm.SLAScheduler.sladue: %w", err)
		}
		return nil
	}
}
