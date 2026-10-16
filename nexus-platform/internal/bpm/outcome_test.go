package bpm

// A decision that changes the record it decided about.
//
// Before this, StatusApproved had one producer and zero consumers outside this package: the quote
// everyone signed off kept whatever status it had, and nothing downstream could react. "Approved"
// has to mean something to the record, or the workflow is theatre.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type spyWriter struct {
	calls  []string
	actors []string
	err    error
}

func (w *spyWriter) SetField(_ context.Context, tenantID, entity, recordID, field string, value any, actorUserID string) error {
	w.calls = append(w.calls, entity+"."+field+"="+value.(string))
	w.actors = append(w.actors, actorUserID)
	return w.err
}

// outcomeFixture arms a one-step process (so a single approve is terminal) with the given outcome.
func outcomeFixture(t *testing.T, out *Outcome, steps int) (*Decider, *fakeStore, *spyWriter) {
	t.Helper()
	store := newFakeStore()
	st := make([]Step, steps)
	for i := range st {
		st[i] = Step{ApproverType: ApproverUser, ApproverID: "boss"}
	}
	stepsJSON, _ := json.Marshal(st)
	var outJSON json.RawMessage
	if out != nil {
		b, _ := json.Marshal(out)
		outJSON = b
	}
	store.processes["p1"] = &ProcessRecord{ID: "p1", TenantID: "t1", Steps: stepsJSON, Outcome: outJSON}
	store.instances["i1"] = &Instance{
		ID: "i1", TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "q1",
		CurrentStep: 0, Status: StatusPending, CurrentApproverType: ApproverUser, CurrentApproverID: "boss",
	}
	w := &spyWriter{}
	return NewDecider(store, store, store, nil, nil).WithRecordWriter(w), store, w
}

func TestOutcome_TerminalDecisionWritesTheRecord(t *testing.T) {
	out := &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận", RejectedValue: "Từ chối"}

	t.Run("approve writes the approved value", func(t *testing.T) {
		d, _, w := outcomeFixture(t, out, 1)
		if _, err := d.Decide(context.Background(), "t1", "i1", DecisionApprove, "", Actor{UserID: "boss"}); err != nil {
			t.Fatalf("decide: %v", err)
		}
		if len(w.calls) != 1 || w.calls[0] != "crm_quotes.status=Chấp nhận" {
			t.Errorf("writes = %v, want the approved value on the decided record", w.calls)
		}
		// The APPROVER must be the actor: this becomes updated_by and the audit actor, so a blank
		// here files the change as an anonymous system edit on the exact screen someone opens to
		// ask who decided it.
		if len(w.actors) != 1 || w.actors[0] != "boss" {
			t.Errorf("actors = %v, want the approver", w.actors)
		}
	})

	t.Run("reject writes the rejected value", func(t *testing.T) {
		d, _, w := outcomeFixture(t, out, 1)
		if _, err := d.Decide(context.Background(), "t1", "i1", DecisionReject, "", Actor{UserID: "boss"}); err != nil {
			t.Fatalf("decide: %v", err)
		}
		if len(w.calls) != 1 || w.calls[0] != "crm_quotes.status=Từ chối" {
			t.Errorf("writes = %v, want the rejected value", w.calls)
		}
	})

	t.Run("a NON-terminal approve writes nothing — the decision is not final yet", func(t *testing.T) {
		d, _, w := outcomeFixture(t, out, 2) // two steps: approving the first only advances
		if _, err := d.Decide(context.Background(), "t1", "i1", DecisionApprove, "", Actor{UserID: "boss"}); err != nil {
			t.Fatalf("decide: %v", err)
		}
		if len(w.calls) != 0 {
			t.Errorf("writes = %v, want none until the last step clears", w.calls)
		}
	})

	t.Run("no outcome configured → nothing is written", func(t *testing.T) {
		d, _, w := outcomeFixture(t, nil, 1)
		if _, err := d.Decide(context.Background(), "t1", "i1", DecisionApprove, "", Actor{UserID: "boss"}); err != nil {
			t.Fatalf("decide: %v", err)
		}
		if len(w.calls) != 0 {
			t.Errorf("writes = %v, want none — this is the pre-existing behaviour", w.calls)
		}
	})

	t.Run("only one direction configured → the other writes nothing", func(t *testing.T) {
		d, _, w := outcomeFixture(t, &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận"}, 1)
		if _, err := d.Decide(context.Background(), "t1", "i1", DecisionReject, "", Actor{UserID: "boss"}); err != nil {
			t.Fatalf("decide: %v", err)
		}
		if len(w.calls) != 0 {
			t.Errorf("writes = %v, want none — plenty of processes only automate one direction", w.calls)
		}
	})
}

// TestOutcome_WriteFailureDoesNotUndoTheDecision: the decision is committed before the write-back
// runs and is what people rely on. A failed write must not turn a completed approval into an error
// the approver has to redo — it is logged loudly and the decision stands.
func TestOutcome_WriteFailureDoesNotUndoTheDecision(t *testing.T) {
	d, store, w := outcomeFixture(t, &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận"}, 1)
	w.err = errors.New("record write refused")

	inst, err := d.Decide(context.Background(), "t1", "i1", DecisionApprove, "", Actor{UserID: "boss"})
	if err != nil {
		t.Fatalf("a failed write-back must not fail the decision: %v", err)
	}
	if inst.Status != StatusApproved || store.instances["i1"].Status != StatusApproved {
		t.Errorf("status = %q / stored %q, want approved to stand", inst.Status, store.instances["i1"].Status)
	}
}

func TestOutcome_ValueFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    *Outcome
		st   InstanceStatus
		want string
		ok   bool
	}{
		{"nil outcome", nil, StatusApproved, "", false},
		{"blank field", &Outcome{ApprovedValue: "X"}, StatusApproved, "", false},
		{"approved", &Outcome{StatusField: "s", ApprovedValue: "A"}, StatusApproved, "A", true},
		{"rejected", &Outcome{StatusField: "s", RejectedValue: "R"}, StatusRejected, "R", true},
		{"cancelled is not a decision", &Outcome{StatusField: "s", ApprovedValue: "A"}, StatusCancelled, "", false},
		{"pending is not terminal", &Outcome{StatusField: "s", ApprovedValue: "A"}, StatusPending, "", false},
	} {
		got, ok := tc.o.ValueFor(tc.st)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: ValueFor = (%q,%v), want (%q,%v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
