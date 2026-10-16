package vertical_pack

import (
	"context"
	"reflect"
	"testing"

	"nexus/platform/internal/workflow"
)

// TestPlanPredictsInstall_Workflows is the property that matters, stated directly: what the
// dry-run PROMISES must equal what the install DOES.
//
// Asserting "the plan has a workflowsToCreate field" would pass while the field held anything at
// all. So this runs DryRunInstall and Install over the SAME service state and compares the plan's
// lists to the result's lists. If the two ever drift — a new pass that creates something the plan
// does not mention, or a plan that names something install skips — this test is what notices.
//
// Why it matters beyond tidiness: an operator clicking "Cài" is consenting to a list. Workflows
// fire, queues route work, approval processes gate records. A plan that omits them collects
// consent for something narrower than what happens.
func TestPlanPredictsInstall_Workflows(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh tenant: everything the pack ships is promised as create", func(t *testing.T) {
		planSvc, _ := newWorkflowTestService(&fakeWorkflowWriter{})
		plan, err := planSvc.DryRunInstall(ctx, "tenant-1", workflowTestPack())
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}

		// A SEPARATE service with identical state — Install must not see the dry run's writes
		// (DryRunInstall writes nothing, but sharing the fake would hide it if it ever did).
		instSvc, _ := newWorkflowTestService(&fakeWorkflowWriter{})
		res, err := instSvc.Install(ctx, "tenant-1", "user-1", workflowTestPack())
		if err != nil {
			t.Fatalf("install: %v", err)
		}

		if !reflect.DeepEqual(plan.WorkflowsToCreate, res.WorkflowsCreated) {
			t.Errorf("plan promised %v, install created %v", plan.WorkflowsToCreate, res.WorkflowsCreated)
		}
		if len(plan.WorkflowsToSkip) != len(res.WorkflowsSkipped) {
			t.Errorf("plan skips %v, install skipped %v", plan.WorkflowsToSkip, res.WorkflowsSkipped)
		}
		if len(plan.WorkflowsToCreate) != 2 {
			t.Fatalf("WorkflowsToCreate = %v, want both pack workflows", plan.WorkflowsToCreate)
		}
	})

	t.Run("re-install: a workflow already present is promised as skip, not create", func(t *testing.T) {
		pre := func() *fakeWorkflowWriter {
			return &fakeWorkflowWriter{
				existing: []workflow.WorkflowRecord{{ID: "pre-1", TenantID: "tenant-1", Name: "SLA timer"}},
			}
		}
		planSvc, _ := newWorkflowTestService(pre())
		plan, err := planSvc.DryRunInstall(ctx, "tenant-1", workflowTestPack())
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		instSvc, _ := newWorkflowTestService(pre())
		res, err := instSvc.Install(ctx, "tenant-1", "user-1", workflowTestPack())
		if err != nil {
			t.Fatalf("install: %v", err)
		}

		if !reflect.DeepEqual(plan.WorkflowsToCreate, res.WorkflowsCreated) {
			t.Errorf("plan promised create %v, install created %v", plan.WorkflowsToCreate, res.WorkflowsCreated)
		}
		if !reflect.DeepEqual(plan.WorkflowsToSkip, res.WorkflowsSkipped) {
			t.Errorf("plan promised skip %v, install skipped %v", plan.WorkflowsToSkip, res.WorkflowsSkipped)
		}
		if len(plan.WorkflowsToSkip) != 1 || plan.WorkflowsToSkip[0] != "SLA timer" {
			t.Errorf("WorkflowsToSkip = %v, want exactly the pre-existing rule", plan.WorkflowsToSkip)
		}
	})
}

// TestSplitPlanNames_IntraPackDuplicate covers the case the live-row skip set cannot: a pack that
// names the same resource twice. sys_workflows/sys_dashboards/sys_reports have no
// UNIQUE(tenant_id,name), so execute() carries its own createdX map and inserts the first only.
// A plan that ignored that would over-promise by exactly the number of duplicates.
func TestSplitPlanNames_IntraPackDuplicate(t *testing.T) {
	type item struct{ name string }
	id := func(i item) (string, string) { return i.name, i.name }

	toCreate, toSkip := splitPlanNames([]item{{"a"}, {"b"}, {"a"}}, map[string]bool{}, id)
	if !reflect.DeepEqual(toCreate, []string{"a", "b"}) {
		t.Errorf("toCreate = %v, want the first of each name", toCreate)
	}
	if !reflect.DeepEqual(toSkip, []string{"a"}) {
		t.Errorf("toSkip = %v, want the repeated name", toSkip)
	}

	// An already-present name skips even on its first appearance.
	toCreate, toSkip = splitPlanNames([]item{{"a"}, {"b"}}, map[string]bool{"a": true}, id)
	if !reflect.DeepEqual(toCreate, []string{"b"}) || !reflect.DeepEqual(toSkip, []string{"a"}) {
		t.Errorf("live-row skip not honoured: create=%v skip=%v", toCreate, toSkip)
	}
}
