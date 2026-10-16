package bpm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"nexus/platform/internal/jobs"
)

// ArmJobType is the sys_jobs type of the durable "start this approval" event (specs/043 S9a, D-11).
// The in-memory buffer is the fast path; when it is full the event is parked here instead of being
// dropped, so a busy system delays an approval rather than losing it.
const ArmJobType = "bpm_arm"

// armParkTimeout bounds the insert of a parked event: Emit runs on the request path after the
// record committed, and must stay cheap even when the database is the thing that is slow.
const armParkTimeout = 500 * time.Millisecond

// ArmJobQueue is what the engine needs of the job queue. Satisfied by *jobs.SQLStore.
type ArmJobQueue interface {
	Enqueue(ctx context.Context, j jobs.Job) (string, error)
}

// WithArmJobs makes a full buffer park its event as a bpm_arm job. Leave unset when the queue is
// off — nobody would run the job. Chainable.
func (e *AsyncEngine) WithArmJobs(q ArmJobQueue) *AsyncEngine {
	e.armJobs = q
	return e
}

// armPayload is the job payload: an Event without its tenant (that is the job row's tenant_id, the
// only value the handler trusts — a payload can never point a job at another tenant).
type armPayload struct {
	ActorUserID string         `json:"actorUserId,omitempty"`
	EntityName  string         `json:"entityName"`
	RecordID    string         `json:"recordId"`
	Trigger     Trigger        `json:"trigger"`
	Data        map[string]any `json:"data,omitempty"`
	Before      map[string]any `json:"before,omitempty"`
	Depth       int            `json:"depth,omitempty"`
}

// parkArm writes the event as a queued bpm_arm job. The context is detached from the request's
// cancellation (the record is already committed; a client that hangs up must not cost it the
// approval) and bounded by armParkTimeout.
func (e *AsyncEngine) parkArm(ctx context.Context, evt Event) error {
	payload, err := json.Marshal(armPayload{
		ActorUserID: evt.ActorUserID, EntityName: evt.EntityName, RecordID: evt.RecordID,
		Trigger: evt.Trigger, Data: evt.Data, Before: evt.Before, Depth: evt.Depth,
	})
	if err != nil {
		return fmt.Errorf("bpm.parkArm: marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), armParkTimeout)
	defer cancel()
	if _, err := e.armJobs.Enqueue(ctx, jobs.Job{TenantID: evt.TenantID, Type: ArmJobType, Payload: payload}); err != nil {
		return fmt.Errorf("bpm.parkArm: %w", err)
	}
	return nil
}

// NewArmJobHandler returns the sys_jobs handler of ArmJobType: it runs the parked event through the
// same path the in-memory worker uses. An event whose record is already waiting is a success (the
// duplicate-arm guard, uq_ai_open); a store failure is returned so the queue retries; a payload
// that cannot be read is permanent (retrying garbage helps nobody).
func NewArmJobHandler(e *AsyncEngine) jobs.HandlerFunc {
	return func(_ context.Context, job jobs.Job) error {
		var p armPayload
		if job.TenantID == "" || json.Unmarshal(job.Payload, &p) != nil || p.EntityName == "" || p.RecordID == "" {
			return jobs.Permanent(fmt.Errorf("bpm.arm: malformed job %s", job.ID))
		}
		if err := e.tryArm(Event{
			TenantID: job.TenantID, ActorUserID: p.ActorUserID, EntityName: p.EntityName, RecordID: p.RecordID,
			Trigger: p.Trigger, Data: p.Data, Before: p.Before, Depth: p.Depth,
		}); err != nil {
			return fmt.Errorf("bpm.arm: %w", err)
		}
		return nil
	}
}
