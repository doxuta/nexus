package bpm

// Reject → return (specs/043 S8, r12): a rejection of a node may send the approval BACK instead of
// ending it — to an earlier node (a new visit, a new deadline) or to the submitter, who fixes the
// record and sends it again ("Gửi lại").
//
// The instance stays PENDING all the way: that is what keeps a second approval from being armed
// while the submitter edits the record (FR-029) — arming is idempotent on a pending instance. The
// state of "with the submitter" is NOT a new column: the instance still stands on the rejector's
// step (current_step), and the open seat there is a `rework` seat held by the submitter. Resuming
// reads the pinned node's onReject.resumeAt; why it came back is the rework seat's note and the
// `rejected` history entry (ReturnedTo = "@submitter").

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// onReject targets and resume points (the values of OnReject.To / OnReject.ResumeAt).
const (
	RejectToEnd       = "@end"       // the rejection is final (the behaviour before S8)
	RejectToSubmitter = "@submitter" // the submitter fixes the record and sends it again
	ResumeRejector    = "rejector"   // after "Gửi lại" the rejector decides again (default)
	ResumeStart       = "start"      // after "Gửi lại" the approval starts over at the first node
)

// OnReject is a node's answer to a rejection.
type OnReject struct {
	// To is "@end", "@submitter" or the id of an EARLIER node of the same graph.
	To string `json:"to"`
	// ResumeAt applies to "@submitter" only: "rejector" (default) or "start".
	ResumeAt string `json:"resumeAt,omitempty"`
}

// Terminal reports whether the rejection ends the approval (no onReject, or "@end").
func (o *OnReject) Terminal() bool { return o == nil || o.To == RejectToEnd }

// ToSubmitter reports whether the rejection returns the record to its submitter.
func (o *OnReject) ToSubmitter() bool { return o != nil && o.To == RejectToSubmitter }

// CheckOnReject judges an onReject configuration of node i (kind) among steps (the chain's ids, in
// order): ONE body for the engine (a stored graph it cannot honour is refused) and the API (what it
// refuses to run it refuses to store). Returns the offending field ("to" / "resumeAt") and why, or
// "" when the configuration is runnable.
func CheckOnReject(o *OnReject, kind NodeKind, i int, steps []Step) (field, reason string) {
	if o == nil {
		return "", ""
	}
	if kind != NodeApproval {
		return "to", "chỉ bước duyệt mới có cấu hình từ chối (việc cần làm không bị từ chối)"
	}
	switch o.To {
	case RejectToEnd:
		if o.ResumeAt != "" {
			return "resumeAt", "resumeAt chỉ dùng khi từ chối trả về người gửi (@submitter)"
		}
	case RejectToSubmitter:
		if o.ResumeAt != "" && o.ResumeAt != ResumeRejector && o.ResumeAt != ResumeStart {
			return "resumeAt", fmt.Sprintf("resumeAt phải là %q hoặc %q", ResumeRejector, ResumeStart)
		}
	default:
		if o.ResumeAt != "" {
			return "resumeAt", "resumeAt chỉ dùng khi từ chối trả về người gửi (@submitter)"
		}
		if o.To == "" {
			return "to", fmt.Sprintf("thiếu nơi trả về: %q, %q hoặc mã một bước đứng trước", RejectToEnd, RejectToSubmitter)
		}
		found := false
		for j := 0; j < i && j < len(steps); j++ {
			if steps[j].ID == o.To {
				if steps[j].NodeKind == NodeEnd {
					return "to", fmt.Sprintf("%q là nút kết thúc: từ chối không thể trả về đó (sẽ thành duyệt)", o.To)
				}
				found = true
				break
			}
		}
		if !found {
			return "to", fmt.Sprintf("%q không phải mã của một bước đứng trước bước này (chỉ trả về bước trước đó)", o.To)
		}
	}
	return "", ""
}

func (o *OnReject) validAt(kind NodeKind, i int, steps []Step) bool {
	f, _ := CheckOnReject(o, kind, i, steps)
	return f == ""
}

// ErrNoSubmitter: a rejection asked to return the record to its submitter, and nobody who could
// receive it was found (the creator is not a live user and the record has no live owner).
var ErrNoSubmitter = errors.New("no submitter to return the record to")

// SubmitterResolver names the user a returned record goes back to: createdBy when it is a live
// user of the tenant (an API key or a deleted user is not), else the record's owner, else "".
// Optional on the Decider: nil trusts createdBy as it stands (unit tests).
//
// ponytail: options.fallbackAssignee (S6a) is not in the chain yet — a record whose creator and
// owner are both gone is refused (ErrNoSubmitter), not parked on a stuck instance.
type SubmitterResolver interface {
	ResolveSubmitter(ctx context.Context, tenantID, entityName, recordID, createdBy string) (string, error)
}

// WithSubmitterResolver arms the lookup of the submitter of a returned record. Chainable.
func (d *Decider) WithSubmitterResolver(r SubmitterResolver) *Decider {
	d.submitters = r
	return d
}

// returnPlan is a rejection that does not end the approval: the transition to write (seats and
// projection included, the close side is the caller's) and what the history entry records.
type returnPlan struct {
	next       InstanceTransition
	basis      SLABasis
	returnedTo string // node id, or RejectToSubmitter
	submitter  string // the user the record went back to ("" for a return to a node)
}

// planReturn builds the transition of a rejection that returns instead of ending. note is the
// rejector's reason (it travels on the rework seat).
func (d *Decider) planReturn(ctx context.Context, inst *Instance, def *Definition, or *OnReject, note string, now time.Time) (*returnPlan, error) {
	chain := def.Chain()
	if or.ToSubmitter() {
		who := inst.CreatedBy
		if d.submitters != nil {
			var err error
			if who, err = d.submitters.ResolveSubmitter(ctx, inst.TenantID, inst.EntityName, inst.RecordID, inst.CreatedBy); err != nil {
				return nil, fmt.Errorf("bpm.Decider.planReturn: resolve submitter: %w", err)
			}
		}
		if who == "" {
			return nil, ErrNoSubmitter
		}
		seat := Seat{NodeID: seatNodeID(inst), AssigneeType: ApproverUser, AssigneeID: who, Kind: SeatKindRework, Note: note}
		t, id := projection([]Seat{seat})
		return &returnPlan{
			next: InstanceTransition{
				CurrentStep: inst.CurrentStep, Status: StatusPending, CurrentApproverType: t, CurrentApproverID: id,
				Seats: SeatWrite{Open: []Seat{seat}}, // no deadline: the clock restarts when it is sent again
			},
			returnedTo: RejectToSubmitter, submitter: who,
		}, nil
	}
	to := -1
	for j, s := range chain {
		if s.ID == or.To {
			to = j
			break
		}
	}
	if to < 0 || to >= inst.CurrentStep { // the pin passed CheckOnReject; a stale one is a refusal, never a guess
		return nil, fmt.Errorf("bpm.Decider.planReturn: onReject.to %q is not an earlier node of the pinned chain", or.To)
	}
	return d.enterStep(ctx, inst, def, chain, to, now, or.To)
}

// enterStep opens chain[to] as a NEW visit with its own deadline: the transition of a return to a
// node and of a resubmission. A machine node (a gateway the approval starts with, S7) has neither
// seats nor a deadline: the instance stands on it with Continue set, and the caller walks on.
func (d *Decider) enterStep(ctx context.Context, inst *Instance, def *Definition, chain []Step, to int, now time.Time, returnedTo string) (*returnPlan, error) {
	if chain[to].IsMachine() {
		return &returnPlan{
			next:       InstanceTransition{CurrentStep: to, Status: StatusPending, Continue: true},
			returnedTo: returnedTo,
		}, nil
	}
	due, basis, err := stepDue(ctx, d.deadlines, inst.TenantID, def.CalendarID(), chain, to, inst.Definition != nil, now)
	if err != nil {
		return nil, fmt.Errorf("bpm.Decider.enterStep: deadline: %w", err)
	}
	// Who holds it: the named holder, or the one a rule finds as the approval ENTERS the node (a field,
	// the submitter, a team's turn — S6a/S6b). Nobody found stays visible on the instance (last_error).
	seats, stuck, err := d.seatsFor(ctx, inst, def, chain[to], due, basis)
	if err != nil {
		return nil, fmt.Errorf("bpm.Decider.enterStep: route: %w", err)
	}
	t, id := projection(seats)
	return &returnPlan{
		next: InstanceTransition{
			CurrentStep: to, Status: StatusPending, CurrentApproverType: t, CurrentApproverID: id,
			SLADueAt: due, Seats: SeatWrite{Open: seats}, LastError: stuck,
		},
		basis: basis, returnedTo: returnedTo,
	}, nil
}

// reworkSeatOf reports whether the instance is with its submitter: a trusted open seat of its
// current step is a rework seat.
func reworkSeatOf(inst *Instance, seats []Seat) bool {
	for _, s := range trustedSeats(inst, seats) {
		if s.Kind == SeatKindRework {
			return true
		}
	}
	return false
}

// resubmit is "Gửi lại": the holder of the rework seat finished fixing the record. The approval goes
// on at the rejector's step (or the first, onReject.resumeAt) as a new visit with a new deadline.
func (d *Decider) resubmit(ctx context.Context, inst *Instance, def *Definition, comment string, actor Actor) (*Instance, error) {
	chain := def.Chain()
	now := d.now().UTC()
	at := inst.CurrentStep
	if or := def.OnRejectAt(at); or != nil && or.ResumeAt == ResumeStart {
		at = max(def.StartIndex(), 0) // the first node of the walk: the graph's start (S7), else step 0
	}
	plan, err := d.enterStep(ctx, inst, def, chain, at, now, chain[at].ID)
	if err != nil {
		return nil, err
	}
	next := plan.next
	next.Seats.Close, next.Seats.Node, next.Seats.By, next.Seats.Comment = SeatCompleted, seatNodeID(inst), actor.UserID, comment
	node, stepName := def.StepRef(inst.CurrentStep)
	history := append(inst.History, HistoryEntry{
		Step: inst.CurrentStep, Action: HistoryResubmitted, Actor: actor.UserID, Comment: comment,
		Node: node, StepName: stepName, ReturnedTo: plan.returnedTo, SLABasis: plan.basis, Version: def.version(), At: now,
	})
	historyJSON, err := json.Marshal(history)
	if err != nil {
		return nil, fmt.Errorf("bpm.Decider.resubmit: marshal history: %w", err)
	}
	if err := d.store.DecideTaskTx(ctx, inst.TenantID, inst.ID, TaskGuard{ExpectSeq: inst.Seq, Actor: &actor}, next, historyJSON); err != nil {
		return nil, txError("bpm.Decider.resubmit", err)
	}
	d.audit(ctx, inst, def, HistoryResubmitted, actor.UserID, comment, now)
	d.notifySeats(ctx, inst, chain[at], next.Seats.Open)
	inst.History, inst.CurrentStep, inst.Status = history, next.CurrentStep, next.Status
	inst.CurrentApproverType, inst.CurrentApproverID = next.CurrentApproverType, next.CurrentApproverID
	inst.SLADueAt, inst.Escalated, inst.UpdatedAt = next.SLADueAt, false, now
	return d.walk(ctx, inst, next), nil // starting over at a gateway: it routes on the record as it is NOW
}

// afterReturn is the best-effort effect side of a rejection that returned: tell whoever has the
// work now, and (a return to the submitter) write the outcome's returnedValue onto the record.
func (d *Decider) afterReturn(ctx context.Context, inst *Instance, def *Definition, plan *returnPlan, comment, actorUserID string) {
	if plan.returnedTo != RejectToSubmitter {
		d.notifySeats(ctx, inst, def.Chain()[plan.next.CurrentStep], plan.next.Seats.Open)
		return
	}
	if d.inbox != nil {
		msg := fmt.Sprintf("Một bản ghi %s bị trả về để bạn sửa lại.", inst.EntityName)
		if c := strings.TrimSpace(comment); c != "" {
			msg += " Lý do: " + c
		}
		if _, err := d.inbox.InsertNotification(ctx, Notification{
			TenantID: inst.TenantID, UserID: plan.submitter, Title: "Cần sửa lại và gửi lại", Type: "approval",
			Message: msg, EntityName: inst.EntityName, RecordID: inst.RecordID,
		}); err != nil {
			d.logger.Warn("bpm: rework notice failed (fail-open)",
				zap.String("tenant_id", inst.TenantID), zap.String("record_id", inst.RecordID), zap.Error(err))
		}
	}
	if d.records == nil || def.Outcome == nil {
		return
	}
	if v, ok := def.Outcome.ReturnedValueFor(); ok {
		// Best-effort like every outcome write: the return stands even when the field refuses the value.
		if err := d.records.SetField(ctx, inst.TenantID, inst.EntityName, inst.RecordID, def.Outcome.StatusField, v, actorUserID); err != nil {
			d.logger.Error("bpm: approval returned but the record write-back FAILED",
				zap.String("tenant_id", inst.TenantID), zap.String("instance_id", inst.ID),
				zap.String("field", def.Outcome.StatusField), zap.String("value", v), zap.Error(err))
		}
	}
}
