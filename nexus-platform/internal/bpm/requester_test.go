package bpm

// The human verbs of an approval: ask, take back, hand on.
//
// The package's existing fakeLoader ignores the trigger argument entirely, so it CANNOT prove the
// property that matters most here — that Submit asks for manual processes rather than reusing the
// after_create set. A trigger-blind fake would pass either way. Hence triggerLoader below.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/workflow"
)

// triggerLoader returns processes keyed BY TRIGGER, so a caller asking for the wrong one gets
// nothing. This is the difference between testing the behaviour and testing the fake.
type triggerLoader struct {
	byTrigger map[Trigger][]Process
	asked     []Trigger
}

func (l *triggerLoader) ListActiveProcesses(_ context.Context, _, _ string, t Trigger) ([]Process, error) {
	l.asked = append(l.asked, t)
	return l.byTrigger[t], nil
}

func manualProc(criteria *workflow.Condition) Process {
	return Process{
		ID: "p-manual", TenantID: "t1", EntityName: "quotes", Trigger: TriggerManual,
		Criteria: criteria,
		Steps:    []Step{{Name: "Sếp duyệt", ApproverType: ApproverUser, ApproverID: "boss", SLASeconds: 3600}},
	}
}

func newRequesterFixture(procs map[Trigger][]Process) (*Requester, *fakeStore, *triggerLoader) {
	store := newFakeStore()
	loader := &triggerLoader{byTrigger: procs}
	return NewRequester(loader, store, store, store, nil), store, loader
}

const submitter = "alice"

func TestSubmit_AsksForManualProcessesOnly(t *testing.T) {
	// An after_create process exists but NO manual one. If Submit ever reused the automatic
	// trigger set, this record would be armed by a rule the person never asked for.
	r, _, loader := newRequesterFixture(map[Trigger][]Process{
		TriggerAfterCreate: {{ID: "p-auto", TenantID: "t1", EntityName: "quotes", Trigger: TriggerAfterCreate,
			Steps: []Step{{ApproverType: ApproverUser, ApproverID: "boss"}}}},
	})

	_, err := r.Submit(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter})
	if !errors.Is(err, ErrNoApprovalProcess) {
		t.Fatalf("err = %v, want ErrNoApprovalProcess — an after_create process must not answer a manual request", err)
	}
	if len(loader.asked) != 1 || loader.asked[0] != TriggerManual {
		t.Errorf("asked for %v, want exactly [manual]", loader.asked)
	}
}

func TestSubmit_ArmsAndOpensTheTimelineWithWhoSentIt(t *testing.T) {
	r, store, _ := newRequesterFixture(map[Trigger][]Process{TriggerManual: {manualProc(nil)}})

	inst, err := r.Submit(context.Background(), "t1", "quotes", "rec1", map[string]any{"total": 10}, Actor{UserID: submitter})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if inst.Status != StatusPending || inst.CurrentApproverID != "boss" {
		t.Errorf("instance = %+v, want pending on the first step's approver", inst)
	}
	if inst.SLADueAt == nil {
		t.Error("SLA deadline not set — a manually-started approval must be as accountable as an automatic one")
	}
	if len(inst.History) != 1 || inst.History[0].Action != HistorySubmitted || inst.History[0].Actor != submitter {
		t.Fatalf("history = %+v, want it to open with who sent it", inst.History)
	}
	if len(store.notes) != 1 || store.notes[0].UserID != "boss" {
		t.Errorf("notes = %+v, want the approver pinged", store.notes)
	}
}

// TestSubmit_PinsDefinition: the manual path is the SECOND way an instance comes into existence,
// and it must pin exactly what the automatic path pins — or a submitted approval re-wires under
// a process edit while an auto-armed one does not (DAILY-QUEUE #8).
func TestSubmit_PinsDefinition(t *testing.T) {
	p := manualProc(nil)
	p.Outcome = &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận"}
	r, store, _ := newRequesterFixture(map[Trigger][]Process{TriggerManual: {p}})

	inst, err := r.Submit(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatalf("want 1 instance armed, got %d", len(store.created))
	}
	def := store.created[0].Definition
	if def == nil || len(def.Steps) != 1 || def.Steps[0].Name != "Sếp duyệt" {
		t.Fatalf("submitted instance did not pin the process steps: %+v", def)
	}
	if def.Outcome == nil || def.Outcome.ApprovedValue != "Chấp nhận" {
		t.Fatalf("submitted instance did not pin the process outcome: %+v", def.Outcome)
	}
	if inst.Definition == nil {
		t.Fatal("the returned instance does not carry the pin the store received")
	}
}

func TestSubmit_RefusesWhenSomethingIsAlreadyPending(t *testing.T) {
	r, store, _ := newRequesterFixture(map[Trigger][]Process{TriggerManual: {manualProc(nil)}})
	store.pending[pkey("t1", "quotes", "rec1")] = true

	_, err := r.Submit(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter})
	if !errors.Is(err, ErrAlreadyPending) {
		t.Fatalf("err = %v, want ErrAlreadyPending", err)
	}
	if len(store.created) != 0 {
		t.Error("a second instance was armed on a record already awaiting a decision")
	}
}

// TestSubmit_HonoursProcessCriteria: the criteria language is shared with the automatic path, so
// "over 50 million needs the director" must mean the same thing however the approval was started.
func TestSubmit_HonoursProcessCriteria(t *testing.T) {
	criteria := &workflow.Condition{Match: "and", Rules: []workflow.Rule{{Field: "total", Op: "gt", Value: 100}}}
	r, _, _ := newRequesterFixture(map[Trigger][]Process{TriggerManual: {manualProc(criteria)}})

	if _, err := r.Submit(context.Background(), "t1", "quotes", "small", map[string]any{"total": 10}, Actor{UserID: submitter}); !errors.Is(err, ErrNoApprovalProcess) {
		t.Fatalf("below-threshold record: err = %v, want ErrNoApprovalProcess", err)
	}
	if _, err := r.Submit(context.Background(), "t1", "quotes", "big", map[string]any{"total": 500}, Actor{UserID: submitter}); err != nil {
		t.Fatalf("above-threshold record: %v", err)
	}
}

func TestRecall_OnlyTheSubmitter(t *testing.T) {
	r, store, _ := newRequesterFixture(map[Trigger][]Process{TriggerManual: {manualProc(nil)}})
	inst, err := r.Submit(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// The APPROVER may not recall — they have reject, which says something different and is
	// recorded differently.
	if _, err := r.Recall(context.Background(), "t1", inst.ID, "", Actor{UserID: "boss"}); !errors.Is(err, ErrNotSubmitter) {
		t.Fatalf("approver recall: err = %v, want ErrNotSubmitter", err)
	}
	if store.instances[inst.ID].Status != StatusPending {
		t.Fatal("a refused recall still changed the instance")
	}

	got, err := r.Recall(context.Background(), "t1", inst.ID, "gửi nhầm", Actor{UserID: submitter})
	if err != nil {
		t.Fatalf("submitter recall: %v", err)
	}
	if got.Status != StatusCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if last := got.History[len(got.History)-1]; last.Action != HistoryRecalled || last.Comment != "gửi nhầm" {
		t.Errorf("history tail = %+v, want the recall with its reason", last)
	}
	// Terminal: recalling twice is not a silent success.
	if _, err := r.Recall(context.Background(), "t1", inst.ID, "", Actor{UserID: submitter}); !errors.Is(err, ErrNotPending) {
		t.Errorf("second recall: err = %v, want ErrNotPending", err)
	}
}

func TestDelegate_NeedsTheAuthorityToDecide(t *testing.T) {
	r, store, _ := newRequesterFixture(map[Trigger][]Process{TriggerManual: {manualProc(nil)}})
	inst, _ := r.Submit(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter})
	before := store.instances[inst.ID].SLADueAt

	// The SUBMITTER cannot hand on a decision that was never theirs to make.
	if _, err := r.Delegate(context.Background(), "t1", inst.ID, "carol", "", Actor{UserID: submitter}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("submitter delegate: err = %v, want ErrNotAuthorized", err)
	}
	if _, err := r.Delegate(context.Background(), "t1", inst.ID, "boss", "", Actor{UserID: "boss"}); !errors.Is(err, ErrDelegateToSelf) {
		t.Fatalf("self delegate: err = %v, want ErrDelegateToSelf", err)
	}

	got, err := r.Delegate(context.Background(), "t1", inst.ID, "carol", "nhờ em", Actor{UserID: "boss"})
	if err != nil {
		t.Fatalf("approver delegate: %v", err)
	}
	if got.CurrentApproverType != ApproverUser || got.CurrentApproverID != "carol" {
		t.Errorf("approver = %s/%s, want the named person", got.CurrentApproverType, got.CurrentApproverID)
	}
	if got.CurrentStep != inst.CurrentStep || got.Status != StatusPending {
		t.Error("delegation advanced the step — the same decision is still owed")
	}
	// The deadline must NOT restart, or a chain of delegations is an unbounded way to defeat it.
	if store.instances[inst.ID].SLADueAt != before {
		t.Error("delegation moved the SLA deadline")
	}
	if last := got.History[len(got.History)-1]; last.Action != HistoryDelegated {
		t.Errorf("history tail = %+v, want the delegation recorded", last)
	}
	if len(store.notes) != 2 || store.notes[1].UserID != "carol" {
		t.Errorf("notes = %+v, want the new approver told", store.notes)
	}
}

// TestDelegate_RoleStepBecomesAPerson: a role-targeted step delegated "to a role" would be a no-op
// (it already belonged to everyone holding it), so delegation always names an individual.
func TestDelegate_RoleStepBecomesAPerson(t *testing.T) {
	store := newFakeStore()
	loader := &triggerLoader{byTrigger: map[Trigger][]Process{TriggerManual: {{
		ID: "p-role", TenantID: "t1", EntityName: "quotes", Trigger: TriggerManual,
		Steps: []Step{{ApproverType: ApproverRole, ApproverID: "role-mgr"}},
	}}}}
	r := NewRequester(loader, store, store, store, nil)
	inst, err := r.Submit(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	got, err := r.Delegate(context.Background(), "t1", inst.ID, "carol", "", Actor{UserID: "mgr", RoleIDs: []string{"role-mgr"}})
	if err != nil {
		t.Fatalf("role-holder delegate: %v", err)
	}
	if got.CurrentApproverType != ApproverUser || got.CurrentApproverID != "carol" {
		t.Errorf("approver = %s/%s, want a named user", got.CurrentApproverType, got.CurrentApproverID)
	}
}

// TestCanDecide_IsOneBody guards the shared authorization body against re-forking: delegate and
// decide must answer identically for the same instance/actor.
func TestCanDecide_IsOneBody(t *testing.T) {
	userStep := &Instance{CurrentApproverType: ApproverUser, CurrentApproverID: "boss"}
	roleStep := &Instance{CurrentApproverType: ApproverRole, CurrentApproverID: "role-mgr"}
	for _, tc := range []struct {
		name  string
		inst  *Instance
		actor Actor
		want  bool
	}{
		{"named approver", userStep, Actor{UserID: "boss"}, true},
		{"someone else", userStep, Actor{UserID: "alice"}, false},
		{"role holder", roleStep, Actor{UserID: "m", RoleIDs: []string{"role-mgr"}}, true},
		{"wrong role", roleStep, Actor{UserID: "m", RoleIDs: []string{"role-sales"}}, false},
		{"admin always", roleStep, Actor{UserID: "root", IsAdmin: true}, true},
		{"empty actor never", userStep, Actor{}, false},
	} {
		if got := CanDecide(tc.inst, tc.actor); got != tc.want {
			t.Errorf("%s: CanDecide = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDecide_StaleReadAfterEscalation_Loses pins the escalated-flag guard on the seq CAS: a
// decision that read escalated=false must LOSE cleanly (ErrConcurrent) if an SLA escalation flipped
// it in the window between GetInstance and the write — the breach would otherwise fire a second
// time. The escalation bumps seq (ClaimBreach), which is the key DecideTaskTx compares.
func TestDecide_StaleReadAfterEscalation_Loses(t *testing.T) {
	store, d, id := twoStepFixture(t)
	fired := false
	store.onGet = func(f *fakeStore, _ *Instance) {
		if fired {
			return
		}
		fired = true
		in := f.instances[id]
		in.Escalated = true
		in.Seq++
	}
	_, err := d.Decide(context.Background(), "t1", id, DecisionApprove, "", Actor{UserID: "u1"})
	if !errors.Is(err, ErrConcurrent) {
		t.Fatalf("a decision that read escalated=false: err = %v; want ErrConcurrent", err)
	}
	if in := store.instances[id]; in.Status != StatusPending || in.CurrentStep != 0 || !in.Escalated {
		t.Fatalf("the losing decision mutated the instance: %+v", in)
	}
}
