package workflow

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// specs/084: field_update with ChildEntity+ChildVia writes EVERY child record (rows of ChildEntity
// whose ChildVia points at the triggering record) — "lớp Đã đóng ⇒ mọi ghi danh đổi trạng thái" —
// bounded by a per-run budget.

// childMutator is fakeMutator + the ChildLister face. children maps "tenant|parent" to the child
// ids; the fake pages by id like the real store (keyset, id > afterID).
type childMutator struct {
	*fakeMutator
	children map[string][]string
}

func (m *childMutator) ListChildIDs(_ context.Context, tenantID, parentEntity, childEntity, childVia, parentID, afterID string, limit int) ([]string, error) {
	if parentEntity != "classes" || childEntity != "enrollments" || childVia != "class" {
		return nil, fmt.Errorf("childMutator: %s.%s does not point at %s", childEntity, childVia, parentEntity)
	}
	all := append([]string(nil), m.children[tenantID+"|"+parentID]...)
	sort.Strings(all)
	var out []string
	for _, id := range all {
		if id > afterID && len(out) < limit {
			out = append(out, id)
		}
	}
	return out, nil
}

func closedClassWorkflow() Workflow {
	return Workflow{ID: "wf1", Actions: []Action{{Type: ActionFieldUpdate, ChildEntity: "enrollments", ChildVia: "class", Field: "status", Value: "closed"}}}
}

func closedClassEvent() Event {
	return Event{TenantID: "t1", EntityName: "classes", RecordID: "cl-1", Trigger: TriggerAfterUpdate, Data: map[string]any{"status": "closed"}}
}

func TestRunActions_FieldUpdateChildren_WritesEveryChildOfThisParentOnly(t *testing.T) {
	mut := &childMutator{fakeMutator: &fakeMutator{}, children: map[string][]string{
		"t1|cl-1": {"e1", "e2", "e3"},
		"t1|cl-2": {"x1"}, // another class: untouched
		"t2|cl-1": {"y1"}, // another tenant, same class id: untouched
	}}
	results, err := runActions(context.Background(), closedClassWorkflow(), closedClassEvent(), actionDeps{mut: mut})
	if err != nil {
		t.Fatalf("runActions = %v", err)
	}
	var got []string
	for _, c := range mut.calls {
		if c.entity != "enrollments" || c.tenant != "t1" || c.field != "status" || c.value != "closed" {
			t.Errorf("write = %+v; want enrollments/t1/status/closed", c)
		}
		got = append(got, c.record)
	}
	if strings.Join(got, ",") != "e1,e2,e3" {
		t.Errorf("written ids = %v; want e1,e2,e3 (and never x1 / y1)", got)
	}
	if len(results) != 1 || !results[0].OK || !strings.Contains(results[0].Warning, "3") {
		t.Errorf("results = %+v; want OK with a note naming 3 updated rows", results)
	}
}

func TestRunActions_FieldUpdateChildren_NoChildrenIsAnOKNote(t *testing.T) {
	mut := &childMutator{fakeMutator: &fakeMutator{}}
	results, err := runActions(context.Background(), closedClassWorkflow(), closedClassEvent(), actionDeps{mut: mut})
	if err != nil || len(mut.calls) != 0 || !results[0].OK || results[0].Warning == "" {
		t.Fatalf("err=%v calls=%d results=%+v; want OK, no writes, a note", err, len(mut.calls), results)
	}
}

func TestRunActions_FieldUpdateChildren_StopsAtTheBudgetAndSaysHowManyWereDone(t *testing.T) {
	ids := make([]string, 0, MaxChildUpdatesPerRun+5)
	for i := 0; i < MaxChildUpdatesPerRun+5; i++ {
		ids = append(ids, fmt.Sprintf("e%05d", i))
	}
	mut := &childMutator{fakeMutator: &fakeMutator{}, children: map[string][]string{"t1|cl-1": ids}}
	results, err := runActions(context.Background(), closedClassWorkflow(), closedClassEvent(), actionDeps{mut: mut})
	if err == nil || !strings.Contains(err.Error(), "WORKFLOW_CHILDREN_BUDGET_EXCEEDED") {
		t.Fatalf("err = %v; want WORKFLOW_CHILDREN_BUDGET_EXCEEDED", err)
	}
	if len(mut.calls) != MaxChildUpdatesPerRun {
		t.Errorf("writes = %d; want exactly the budget %d", len(mut.calls), MaxChildUpdatesPerRun)
	}
	if results[0].OK || !strings.Contains(results[0].Error, fmt.Sprint(MaxChildUpdatesPerRun)) {
		t.Errorf("result = %+v; want FAILED naming the %d rows already updated", results[0], MaxChildUpdatesPerRun)
	}
}

func TestRunActions_FieldUpdateChildren_ExactlyBudgetIsNotAnOverrun(t *testing.T) {
	ids := make([]string, 0, MaxChildUpdatesPerRun)
	for i := 0; i < MaxChildUpdatesPerRun; i++ {
		ids = append(ids, fmt.Sprintf("e%05d", i))
	}
	mut := &childMutator{fakeMutator: &fakeMutator{}, children: map[string][]string{"t1|cl-1": ids}}
	if _, err := runActions(context.Background(), closedClassWorkflow(), closedClassEvent(), actionDeps{mut: mut}); err != nil {
		t.Fatalf("err = %v; want nil at exactly the budget", err)
	}
	if len(mut.calls) != MaxChildUpdatesPerRun {
		t.Errorf("writes = %d; want %d", len(mut.calls), MaxChildUpdatesPerRun)
	}
}

func TestRunActions_FieldUpdateChildren_WriteErrorStopsAndNamesTheChild(t *testing.T) {
	mut := &childMutator{fakeMutator: &fakeMutator{err: fmt.Errorf("boom")}, children: map[string][]string{"t1|cl-1": {"e1", "e2"}}}
	results, err := runActions(context.Background(), closedClassWorkflow(), closedClassEvent(), actionDeps{mut: mut})
	if err == nil || results[0].OK {
		t.Fatalf("err=%v results=%+v; want a FAILED action", err, results)
	}
}

func TestRunActions_FieldUpdateChildren_MutatorWithoutListerFails(t *testing.T) {
	_, err := runActions(context.Background(), closedClassWorkflow(), closedClassEvent(), actionDeps{mut: &fakeMutator{}})
	if err == nil {
		t.Fatal("err = nil; want an error when the mutator cannot list children")
	}
}

// A re-run must make progress: children that already hold the target value are skipped and do NOT
// spend the budget, so the 5 stragglers behind 200 already-done children still get written.
func TestRunActions_FieldUpdateChildren_RerunSkipsAlreadyDoneChildrenWithoutSpendingBudget(t *testing.T) {
	n := MaxChildUpdatesPerRun + 5
	ids := make([]string, 0, n)
	rows := map[string]map[string]any{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("e%05d", i)
		ids = append(ids, id)
		if i < MaxChildUpdatesPerRun {
			rows[id] = map[string]any{"status": "closed"} // done by the first, cut-off run
		}
	}
	mut := &childMutator{fakeMutator: &fakeMutator{rows: rows}, children: map[string][]string{"t1|cl-1": ids}}
	results, err := runActions(context.Background(), closedClassWorkflow(), closedClassEvent(), actionDeps{mut: mut})
	if err != nil || !results[0].OK {
		t.Fatalf("rerun = %v, %+v; want success", err, results)
	}
	if len(mut.calls) != 5 {
		t.Errorf("writes = %d; want only the 5 stragglers", len(mut.calls))
	}
}
