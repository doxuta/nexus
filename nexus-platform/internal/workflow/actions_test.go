package workflow

// specs/043 S9b: the start_approval action — a rule (event, scheduled or date_offset) sends the
// record it fired for through a named manual approval process.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type startCall struct {
	tenant, entity, record, process, submitter string
	data                                       map[string]any
}

// fakeStarter is the consumer-side ApprovalStarter: it records what the action asked for.
type fakeStarter struct {
	calls   []startCall
	started bool // false = "the record already waits on an approval"
	err     error
	depths  []int // chain depth carried by each call's ctx
}

func (f *fakeStarter) StartApproval(ctx context.Context, tenantID, entity, recordID, processID, submitterID string, data map[string]any) (bool, error) {
	f.depths = append(f.depths, ChainDepthFromContext(ctx))
	f.calls = append(f.calls, startCall{tenantID, entity, recordID, processID, submitterID, data})
	return f.started, f.err
}

func startApprovalWF(trigger Trigger) Workflow {
	return Workflow{
		ID: "wf-start", TenantID: "t1", EntityName: "crm_contracts", Trigger: trigger,
		Actions: []Action{{Type: ActionStartApproval, ProcessID: "proc-1"}},
	}
}

func startApprovalEvent(trigger Trigger) Event {
	return Event{
		TenantID: "t1", EntityName: "crm_contracts", RecordID: "rec-1", Trigger: trigger,
		OwnerUserID: "owner-1", Data: map[string]any{"status": "Chờ duyệt"},
	}
}

func TestStartApprovalAction_DateOffsetWorkflowArms(t *testing.T) {
	for _, trig := range []Trigger{TriggerDateOffset, TriggerScheduled, TriggerAfterUpdate} {
		t.Run(string(trig), func(t *testing.T) {
			st := &fakeStarter{started: true}
			res, err := runActions(context.Background(), startApprovalWF(trig), startApprovalEvent(trig),
				actionDeps{approvals: st, maxDepth: DefaultMaxChainDepth})
			if err != nil {
				t.Fatalf("runActions: %v", err)
			}
			if len(st.calls) != 1 {
				t.Fatalf("starter called %d times, want 1", len(st.calls))
			}
			c := st.calls[0]
			if c.tenant != "t1" || c.entity != "crm_contracts" || c.record != "rec-1" || c.process != "proc-1" {
				t.Fatalf("asked for %+v, want t1/crm_contracts/rec-1 through proc-1", c)
			}
			if c.submitter != "owner-1" {
				t.Errorf("submitter = %q, want the record's owner (a rule has no person pressing a button)", c.submitter)
			}
			if c.data["status"] != "Chờ duyệt" {
				t.Errorf("criteria data = %v, want the record's data", c.data)
			}
			if !res[0].OK {
				t.Errorf("result = %+v, want OK", res[0])
			}
		})
	}
}

func TestStartApprovalAction_SubmitterFallsBackToTheSystemActor(t *testing.T) {
	st := &fakeStarter{started: true}
	evt := startApprovalEvent(TriggerDateOffset)
	evt.OwnerUserID = ""
	if _, err := runActions(context.Background(), startApprovalWF(TriggerDateOffset), evt, actionDeps{approvals: st, maxDepth: 5}); err != nil {
		t.Fatal(err)
	}
	if st.calls[0].submitter != SystemActorID {
		t.Fatalf("submitter = %q, want the system actor when the owner cannot be resolved", st.calls[0].submitter)
	}
}

func TestStartApprovalAction_AlreadyWaitingIsNotAFailure(t *testing.T) {
	// A scheduled rule fires again every tick: a record that already waits is the expected steady
	// state, not an error the run history should be full of.
	st := &fakeStarter{started: false}
	res, err := runActions(context.Background(), startApprovalWF(TriggerScheduled), startApprovalEvent(TriggerScheduled),
		actionDeps{approvals: st, maxDepth: 5})
	if err != nil || !res[0].OK {
		t.Fatalf("err=%v result=%+v, want a clean no-op", err, res[0])
	}
	if !strings.Contains(res[0].Warning, "đang chờ duyệt") {
		t.Errorf("warning = %q, want it to say the record already waits", res[0].Warning)
	}
}

func TestStartApprovalAction_ChainDepthGuard(t *testing.T) {
	st := &fakeStarter{started: true}
	evt := startApprovalEvent(TriggerAfterUpdate)
	evt.Depth = 5
	res, err := runActions(context.Background(), startApprovalWF(TriggerAfterUpdate), evt, actionDeps{approvals: st, maxDepth: 5})
	if err != nil {
		t.Fatalf("a cap-skip is not an error: %v", err)
	}
	if len(st.calls) != 0 {
		t.Fatalf("started an approval at the depth cap: %+v", st.calls)
	}
	if !res[0].OK {
		t.Errorf("result = %+v", res[0])
	}
}

func TestStartApprovalAction_Failures(t *testing.T) {
	cases := []struct {
		name string
		deps actionDeps
		wf   Workflow
		want string
	}{
		{"no starter wired", actionDeps{maxDepth: 5}, startApprovalWF(TriggerDateOffset), "no approval starter"},
		{"empty process id", actionDeps{approvals: &fakeStarter{}, maxDepth: 5},
			Workflow{ID: "w", TenantID: "t1", EntityName: "crm_contracts", Trigger: TriggerDateOffset, Actions: []Action{{Type: ActionStartApproval}}}, "empty process"},
		{"engine refuses", actionDeps{approvals: &fakeStarter{err: errors.New("boom")}, maxDepth: 5}, startApprovalWF(TriggerDateOffset), "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := runActions(context.Background(), tc.wf, startApprovalEvent(TriggerDateOffset), tc.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if res[0].OK {
				t.Error("a failed start must be recorded as a failed action")
			}
		})
	}
}

// The write-back of an auto-completing approval re-enters the engine through the ctx the starter
// receives, so that ctx must carry depth+1 or the loop cap never advances.
func TestStartApprovalAction_CarriesChainDepth(t *testing.T) {
	st := &fakeStarter{started: true}
	evt := startApprovalEvent(TriggerAfterUpdate)
	evt.Depth = 2
	if _, err := runActions(context.Background(), startApprovalWF(TriggerAfterUpdate), evt, actionDeps{approvals: st, maxDepth: 5}); err != nil {
		t.Fatal(err)
	}
	if len(st.depths) != 1 || st.depths[0] != 3 {
		t.Fatalf("starter ctx depth = %v, want [3]", st.depths)
	}
}
