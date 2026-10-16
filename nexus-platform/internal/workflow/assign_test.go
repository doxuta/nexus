package workflow

import (
	"context"
	"errors"
	"testing"
)

// fakeResolver is an in-memory AssignmentResolver: it returns the values in seq in order (wrapping),
// so a test can assert runAssign writes whatever the resolver picks into the target field and
// threads the right (tenant, workflowID, actionKey, team). ok=false is simulated with noMember.
// The round-robin ADVANCE correctness lives in services.nextInRing's unit test + the lead's real
// FOR UPDATE integration test; this fake only needs to be deterministic.
type fakeResolver struct {
	seq      []string
	idx      int
	noMember bool
	err      error
	calls    []struct{ tenant, wf, actionKey, team string }
	// loadCalls records LeastLoadedAssignee invocations so a test can assert runAssign dispatches to
	// the load_based path (with the trigger entity + target field threaded through). It serves the
	// SAME seq/noMember/err knobs as NextAssignee.
	loadCalls []struct{ tenant, wf, actionKey, entity, field, team string }
}

func (r *fakeResolver) NextAssignee(_ context.Context, tenantID, workflowID, actionKey, teamID string) (string, bool, error) {
	r.calls = append(r.calls, struct{ tenant, wf, actionKey, team string }{tenantID, workflowID, actionKey, teamID})
	if r.err != nil {
		return "", false, r.err
	}
	if r.noMember || len(r.seq) == 0 {
		return "", false, nil
	}
	v := r.seq[r.idx%len(r.seq)]
	r.idx++
	return v, true, nil
}

func (r *fakeResolver) LeastLoadedAssignee(_ context.Context, tenantID, workflowID, actionKey, entity, field, teamID string) (string, bool, error) {
	r.loadCalls = append(r.loadCalls, struct{ tenant, wf, actionKey, entity, field, team string }{tenantID, workflowID, actionKey, entity, field, teamID})
	if r.err != nil {
		return "", false, r.err
	}
	if r.noMember || len(r.seq) == 0 {
		return "", false, nil
	}
	v := r.seq[r.idx%len(r.seq)]
	r.idx++
	return v, true, nil
}

func assignWF() Workflow {
	return Workflow{ID: "wf-assign", TenantID: "t1", Actions: []Action{
		{Type: ActionAssign, Strategy: StrategyRoundRobin, Team: "team-1", Field: "handler"},
	}}
}

// TestRunAssign_RoundRobinSequence proves spec §9 test #1 at the unit level: 4 fires of a rule
// whose team rotates u1,u2,u3 write u1,u2,u3,u1 into the target field of the record (via the SAME
// mutator field_update uses), and the resolver is called with the rule's id + the "a0" action key.
func TestRunAssign_RoundRobinSequence(t *testing.T) {
	mut := &fakeMutator{}
	res := &fakeResolver{seq: []string{"u1", "u2", "u3"}}
	wf := assignWF()
	deps := actionDeps{mut: mut, assign: res}

	want := []string{"u1", "u2", "u3", "u1"}
	for i := range want {
		evt := Event{TenantID: "t1", EntityName: "tickets", RecordID: "r" + string(rune('0'+i)), Trigger: TriggerAfterCreate}
		if _, err := runActions(context.Background(), wf, evt, deps); err != nil {
			t.Fatalf("fire %d: runActions err = %v", i, err)
		}
	}
	if len(mut.calls) != len(want) {
		t.Fatalf("expected %d UpdateField calls, got %d", len(want), len(mut.calls))
	}
	for i, w := range want {
		c := mut.calls[i]
		if c.field != "handler" {
			t.Errorf("fire %d: wrote field %q; want the target field %q", i, c.field, "handler")
		}
		if c.value != w {
			t.Errorf("fire %d: wrote value %v; want assignee %q", i, c.value, w)
		}
		if c.entity != "tickets" || c.tenant != "t1" {
			t.Errorf("fire %d: wrote to %s/%s; want tickets/t1", i, c.entity, c.tenant)
		}
	}
	// The resolver saw the rule id + the "a0" action key (single action) each fire.
	for i, call := range res.calls {
		if call.wf != "wf-assign" || call.actionKey != "a0" || call.team != "team-1" || call.tenant != "t1" {
			t.Errorf("resolver call %d = %+v; want wf-assign/a0/team-1/t1", i, call)
		}
	}
}

// TestRunAssign_LoadBasedDispatch proves the strategy switch routes a load_based action to
// LeastLoadedAssignee (NOT NextAssignee), threading the trigger entity + target field, and writes
// whatever that resolver picks into the target field. The pick semantics themselves (least-loaded +
// tie-break) live in the real-MySQL integration test; this only asserts the dispatch + write plumbing.
func TestRunAssign_LoadBasedDispatch(t *testing.T) {
	mut := &fakeMutator{}
	res := &fakeResolver{seq: []string{"u9"}}
	wf := Workflow{ID: "wf-load", TenantID: "t1", Actions: []Action{
		{Type: ActionAssign, Strategy: StrategyLoadBased, Team: "team-7", Field: "handler"},
	}}
	evt := Event{TenantID: "t1", EntityName: "tickets", RecordID: "r1", Trigger: TriggerAfterCreate}

	if _, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut, assign: res}); err != nil {
		t.Fatalf("load_based runActions err = %v", err)
	}
	if len(res.loadCalls) != 1 || len(res.calls) != 0 {
		t.Fatalf("expected 1 LeastLoadedAssignee + 0 NextAssignee calls, got load=%d rr=%d", len(res.loadCalls), len(res.calls))
	}
	lc := res.loadCalls[0]
	if lc.entity != "tickets" || lc.field != "handler" || lc.team != "team-7" || lc.wf != "wf-load" || lc.actionKey != "a0" {
		t.Errorf("LeastLoadedAssignee call = %+v; want tickets/handler/team-7/wf-load/a0", lc)
	}
	if len(mut.calls) != 1 || mut.calls[0].field != "handler" || mut.calls[0].value != "u9" {
		t.Fatalf("expected the picked assignee written to handler=u9, got %+v", mut.calls)
	}
}

// TestRunAssign_NoEligibleMember proves the fail-open no-op: ok=false (empty team / all disabled)
// writes NOTHING and returns no error (the run still records success).
func TestRunAssign_NoEligibleMember(t *testing.T) {
	mut := &fakeMutator{}
	res := &fakeResolver{noMember: true}
	evt := Event{TenantID: "t1", EntityName: "tickets", RecordID: "r1", Trigger: TriggerAfterCreate}

	if _, err := runActions(context.Background(), assignWF(), evt, actionDeps{mut: mut, assign: res}); err != nil {
		t.Fatalf("no-member assign should be a fail-open no-op, got err = %v", err)
	}
	if len(mut.calls) != 0 {
		t.Fatalf("no eligible member must write nothing, got %d writes", len(mut.calls))
	}
	if len(res.calls) != 1 {
		t.Fatalf("resolver should still be consulted once, got %d", len(res.calls))
	}
}

// TestRunAssign_SkippedOnDelete proves assign on after_delete is a no-op (no live record to assign,
// like field_update) — the resolver is never even consulted.
func TestRunAssign_SkippedOnDelete(t *testing.T) {
	mut := &fakeMutator{}
	res := &fakeResolver{seq: []string{"u1"}}
	evt := Event{TenantID: "t1", EntityName: "tickets", RecordID: "r1", Trigger: TriggerAfterDelete}

	if _, err := runActions(context.Background(), assignWF(), evt, actionDeps{mut: mut, assign: res}); err != nil {
		t.Fatalf("assign on after_delete err = %v", err)
	}
	if len(mut.calls) != 0 || len(res.calls) != 0 {
		t.Fatalf("after_delete assign must no-op (writes=%d, resolver calls=%d)", len(mut.calls), len(res.calls))
	}
}

// TestRunAssign_NilResolver proves a rule that reaches runAssign with no resolver wired errors
// (fail-open, logged by the dispatcher) rather than panicking or silently dropping the assignment.
func TestRunAssign_NilResolver(t *testing.T) {
	mut := &fakeMutator{}
	evt := Event{TenantID: "t1", EntityName: "tickets", RecordID: "r1", Trigger: TriggerAfterCreate}

	_, err := runActions(context.Background(), assignWF(), evt, actionDeps{mut: mut}) // assign resolver absent
	if err == nil {
		t.Fatal("expected an error when the assignment resolver is not wired")
	}
	if len(mut.calls) != 0 {
		t.Fatalf("no record should be written when the resolver is missing, got %d", len(mut.calls))
	}
}

// TestRunAssign_ResolverError proves a resolver fault propagates (so the run records FAILED) and
// no partial write happens.
func TestRunAssign_ResolverError(t *testing.T) {
	mut := &fakeMutator{}
	res := &fakeResolver{err: errors.New("db down")}
	evt := Event{TenantID: "t1", EntityName: "tickets", RecordID: "r1", Trigger: TriggerAfterCreate}

	_, err := runActions(context.Background(), assignWF(), evt, actionDeps{mut: mut, assign: res})
	if err == nil {
		t.Fatal("expected the resolver error to propagate")
	}
	if len(mut.calls) != 0 {
		t.Fatalf("a resolver error must not write a record, got %d", len(mut.calls))
	}
}

// MỘT XƯỞNG 2026-07-29, Founder's rule: "giao việc = trao quyền sửa".
//
// Routing used to write ONLY the target field, which is ordinary data. Permissions read owner_id
// (the row filter is COALESCE(owner_id, created_by)), so the rep an automation named could SEE the
// lead and could not edit it — under the default staff role (view all / edit own) automatic
// routing handed people work they were locked out of. These pin the grant so it cannot regress to
// a label again.
func TestRunAssign_GrantsOwnershipNotJustALabel(t *testing.T) {
	mut := &fakeMutator{}
	res := &fakeResolver{seq: []string{"u7"}}
	evt := Event{TenantID: "t1", EntityName: "tickets", RecordID: "r1", Trigger: TriggerAfterCreate}

	if _, err := runActions(context.Background(), assignWF(), evt, actionDeps{mut: mut, assign: res}); err != nil {
		t.Fatalf("runActions err = %v", err)
	}
	if got := mut.owners["r1"]; got != "u7" {
		t.Fatalf("owner of r1 = %q; want %q — the assignee must be able to EDIT what they were given", got, "u7")
	}
	// The display field is still written: installed tenants render it as "Phân công cho", and
	// dropping it would blank their screens.
	if len(mut.calls) != 1 || mut.calls[0].value != "u7" {
		t.Fatalf("target field write = %+v; want the assignee written to the field too", mut.calls)
	}
}

func TestRunAssign_OwnershipFailureIsReported(t *testing.T) {
	// A failed hand-over must surface as an error (the dispatcher records a FAILED run) rather
	// than leaving a record labelled for someone who cannot touch it — the silent half-success is
	// exactly the failure mode this slice exists to remove.
	mut := &fakeMutator{ownerErr: errors.New("boom")}
	res := &fakeResolver{seq: []string{"u7"}}
	evt := Event{TenantID: "t1", EntityName: "tickets", RecordID: "r1", Trigger: TriggerAfterCreate}

	if _, err := runActions(context.Background(), assignWF(), evt, actionDeps{mut: mut, assign: res}); err == nil {
		t.Fatal("runActions err = nil; want the SetOwner failure reported")
	}
}
