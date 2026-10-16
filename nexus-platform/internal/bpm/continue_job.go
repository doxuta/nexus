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

// ContinueJobType is the sys_jobs type of the durable "go on walking" wake-up (specs/043 S7, D-11).
// One job per (instance, seq) is parked in the SAME transaction that leaves an instance standing
// on a machine node, so a server stopped between that commit and the walk (FR-009) does not strand
// it: the job picks the instance up where it stood. The synchronous walk after a decision / submit
// / arm is the fast path; the job is the safety net, and a no-op whenever the fast path won.
const ContinueJobType = "bpm_continue"

// continueDelay keeps the job from racing the synchronous walk that follows the commit: it fires a
// moment later, finds the seq moved on, and does nothing. A crash costs this much extra latency.
const continueDelay = 3 * time.Second

// continuePayload is the job payload. The tenant is NOT in it: it is the job row's tenant_id, the
// only value the handler trusts (a payload can never point a job at another tenant).
type continuePayload struct {
	InstanceID string `json:"instanceId"`
	Seq        int64  `json:"seq"`
}

// enqueueContinue parks the bpm_continue job of an instance a write left at `seq` standing on a
// machine node. A no-op while the queue is off (nobody would run it — the same posture as
// enqueueSLADue): the synchronous walk and the admin's "Thử lại" remain.
func (r *SQLRepository) enqueueContinue(ctx context.Context, ex sqlx.ExecerContext, tenantID, instanceID string, seq int64) error {
	if r.dueJobs == nil {
		return nil
	}
	payload, err := json.Marshal(continuePayload{InstanceID: instanceID, Seq: seq})
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.enqueueContinue: marshal: %w", err)
	}
	if _, err := r.dueJobs.EnqueueExec(ctx, ex, jobs.Job{
		TenantID: tenantID, Type: ContinueJobType, Payload: payload, RunAt: r.now().UTC().Add(continueDelay),
	}); err != nil {
		return fmt.Errorf("bpm.SQLRepository.enqueueContinue: %w", err)
	}
	return nil
}

// NewContinueJobHandler returns the sys_jobs handler of ContinueJobType. A job whose instance has
// moved on (seq changed), finished, stalled on an error or been deleted is a no-op; otherwise it
// walks the instance forward. A store failure is returned so the queue retries the job.
func NewContinueJobHandler(store DueStore, runner Continuer) jobs.HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var p continuePayload
		if job.TenantID == "" || json.Unmarshal(job.Payload, &p) != nil || p.InstanceID == "" {
			return jobs.Permanent(fmt.Errorf("bpm.continue: malformed job %s", job.ID))
		}
		inst, err := store.GetInstance(ctx, job.TenantID, p.InstanceID)
		if errors.Is(err, ErrInstanceNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("bpm.continue: %w", err)
		}
		if inst.Status != StatusPending || inst.Seq != p.Seq || inst.LastError != "" {
			return nil // stale: a later write of this instance moved it (and parked its own job if needed)
		}
		if err := runner.Continue(ctx, job.TenantID, p.InstanceID); err != nil {
			return fmt.Errorf("bpm.continue: %w", err)
		}
		return nil
	}
}

// AppendHistory appends one line to an instance's history WITHOUT moving it (no seq bump, any
// status): used to record, on an already-settled instance, that the effect after the settlement
// failed (FR-010). tenant_id pins the statement.
func (r *SQLRepository) AppendHistory(ctx context.Context, tenantID, id string, e HistoryEntry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.AppendHistory: marshal: %w", err)
	}
	const q = `UPDATE sys_approval_instances SET history = JSON_ARRAY_APPEND(history, '$', CAST(? AS JSON))
		WHERE tenant_id = ? AND id = ?`
	res, err := r.db.ExecContext(ctx, q, string(line), tenantID, id)
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.AppendHistory: %w", err)
	}
	return rowsAffectedOrNotFound(res, ErrInstanceNotFound, "bpm.SQLRepository.AppendHistory")
}
