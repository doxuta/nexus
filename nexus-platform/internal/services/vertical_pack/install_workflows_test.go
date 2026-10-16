package vertical_pack

// Unit tests for the workflows install pass (PACK-WORKFLOWS) over in-memory fakes: a pack with N
// workflows → CreateWorkflow called N times (mapped from PackWorkflow to CreateWorkflowInput); a
// workflow whose name already exists in the tenant → skipped by name (idempotent); a re-install →
// every workflow skipped; a nil workflow writer → the pack's workflows are silently skipped
// (optional dep absent, back-compat). Validation itself (buildWorkflowRule / WF-01) is covered by
// the integration test against a real WorkflowService — the fake here only records calls.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// fakeWorkflowWriter is an in-memory workflowWriter: CreateWorkflow records the mapped input and
// makes the created rule visible to a later ListWorkflows (so a re-install skips it by name). It
// does NOT validate — parity with the real service's validation is proven in the integration test.
type fakeWorkflowWriter struct {
	mu       sync.Mutex
	seq      int
	existing []workflow.WorkflowRecord      // pre-seeded rules (for the idempotent-skip test)
	created  []services.CreateWorkflowInput // every CreateWorkflow input, in call order
	failWith error                          // when set, CreateWorkflow returns it (after recording nothing)
}

func (f *fakeWorkflowWriter) ListWorkflows(_ context.Context, _ string) ([]workflow.WorkflowRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]workflow.WorkflowRecord(nil), f.existing...), nil
}

func (f *fakeWorkflowWriter) CreateWorkflow(_ context.Context, tenantID string, in services.CreateWorkflowInput) (*workflow.WorkflowRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	f.seq++
	f.created = append(f.created, in)
	rec := workflow.WorkflowRecord{ID: fmt.Sprintf("wf-%d", f.seq), TenantID: tenantID, Name: in.Name, EntityName: in.EntityName, Trigger: in.Trigger}
	// Make the new rule visible to a later ListWorkflows so a re-install skips it by name.
	f.existing = append(f.existing, rec)
	return &rec, nil
}

// newWorkflowTestService wires a Service over the shared entity/role fakes plus the workflow writer
// (nil = optional dep absent). Returns the service and the entity fake (for rollback assertions).
func newWorkflowTestService(wf *fakeWorkflowWriter) (*Service, *fakeMintMeta) {
	meta := &fakeMintMeta{}
	d := Deps{
		EntityReader: meta,
		RoleReader:   fakeMintRoles{},
		EntityWriter: meta,
	}
	if wf != nil {
		d.WorkflowWriter = wf // avoid a typed-nil interface tripping the s.workflowWr != nil guard
	}
	return NewService(d), meta
}

// workflowTestPack: one entity + two workflows — an after_create field_update (routing) and a
// date_offset SLA timer anchored on created_at (composes without adding a date field to the entity).
func workflowTestPack() *Pack {
	active := true
	return &Pack{
		Entities: []PackEntity{
			{Name: "wf_tickets", Label: "Tickets", Fields: []PackField{
				{Name: "subject", Label: "Subject", FieldType: "text"},
				{Name: "priority", Label: "Priority", FieldType: "text"},
			}},
		},
		Workflows: []PackWorkflow{
			{
				Name:       "Escalate on create",
				EntityName: "wf_tickets",
				Trigger:    "after_create",
				Actions: []PackWorkflowAction{
					{Type: "field_update", Field: "priority", Value: "high"},
				},
				IsActive: &active,
			},
			{
				Name:       "SLA timer",
				EntityName: "wf_tickets",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{
					Timer: &PackWorkflowTimer{
						DateField: "created_at",
						Offset:    PackWorkflowTimerOffset{Amount: 2, Unit: "days"},
						Direction: "after",
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "field_update", Field: "priority", Value: "urgent"},
				},
			},
		},
	}
}

// TestInstall_WorkflowsCreated: a pack with 2 workflows → CreateWorkflow called twice with the
// mapped inputs (ActorID = install user, timer/actions carried through), and a re-install skips
// both by name.
func TestInstall_WorkflowsCreated(t *testing.T) {
	wf := &fakeWorkflowWriter{}
	svc, _ := newWorkflowTestService(wf)
	ctx := context.Background()

	res, err := svc.Install(ctx, "tenant-1", "user-1", workflowTestPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.WorkflowsCreated) != 2 {
		t.Fatalf("WorkflowsCreated = %v, want 2", res.WorkflowsCreated)
	}
	if len(res.WorkflowsSkipped) != 0 {
		t.Errorf("WorkflowsSkipped = %v, want empty on first install", res.WorkflowsSkipped)
	}
	if len(wf.created) != 2 {
		t.Fatalf("CreateWorkflow called %d times, want 2", len(wf.created))
	}
	// Mapping: names preserved in pack order, ActorID is the install user.
	if wf.created[0].Name != "Escalate on create" || wf.created[1].Name != "SLA timer" {
		t.Errorf("created names = [%q %q], want [Escalate on create, SLA timer]", wf.created[0].Name, wf.created[1].Name)
	}
	for i, in := range wf.created {
		if in.ActorID != "user-1" {
			t.Errorf("created[%d].ActorID = %q, want user-1", i, in.ActorID)
		}
	}
	// The field_update action mapped through with its target + value.
	if len(wf.created[0].Actions) != 1 || wf.created[0].Actions[0].Type != "field_update" ||
		wf.created[0].Actions[0].Field != "priority" || wf.created[0].Actions[0].Value != "high" {
		t.Errorf("created[0].Actions = %+v, want one field_update priority=high", wf.created[0].Actions)
	}
	// The date_offset timer mapped through from condition.timer.
	if wf.created[1].Timer == nil {
		t.Fatalf("created[1].Timer = nil, want the date_offset timer config")
	}
	if wf.created[1].Timer.DateField != "created_at" || wf.created[1].Timer.Offset.Amount != 2 ||
		wf.created[1].Timer.Offset.Unit != "days" || wf.created[1].Timer.Direction != "after" {
		t.Errorf("created[1].Timer = %+v, want {created_at, {2 days}, after}", *wf.created[1].Timer)
	}

	// --- Re-install: both workflows now exist by name → all skipped, no new CreateWorkflow. ---
	res2, err := svc.Install(ctx, "tenant-1", "user-1", workflowTestPack())
	if err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if len(res2.WorkflowsCreated) != 0 {
		t.Errorf("re-install WorkflowsCreated = %v, want empty (idempotent)", res2.WorkflowsCreated)
	}
	if len(res2.WorkflowsSkipped) != 2 {
		t.Errorf("re-install WorkflowsSkipped = %v, want 2", res2.WorkflowsSkipped)
	}
	if len(wf.created) != 2 {
		t.Errorf("CreateWorkflow total calls = %d, want 2 (no new creates on re-install)", len(wf.created))
	}
}

// TestInstall_WorkflowsSkippedByName: a workflow whose name already exists in the tenant is skipped
// by name (buildPlan's wfSkip), the other is created.
func TestInstall_WorkflowsSkippedByName(t *testing.T) {
	wf := &fakeWorkflowWriter{
		existing: []workflow.WorkflowRecord{{ID: "pre-1", TenantID: "tenant-1", Name: "SLA timer"}},
	}
	svc, _ := newWorkflowTestService(wf)

	res, err := svc.Install(context.Background(), "tenant-1", "user-1", workflowTestPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.WorkflowsCreated) != 1 || res.WorkflowsCreated[0] != "Escalate on create" {
		t.Errorf("WorkflowsCreated = %v, want [Escalate on create]", res.WorkflowsCreated)
	}
	if len(res.WorkflowsSkipped) != 1 || res.WorkflowsSkipped[0] != "SLA timer" {
		t.Errorf("WorkflowsSkipped = %v, want [SLA timer]", res.WorkflowsSkipped)
	}
	// Only the non-existing rule went through CreateWorkflow.
	if len(wf.created) != 1 || wf.created[0].Name != "Escalate on create" {
		t.Errorf("CreateWorkflow calls = %+v, want one (Escalate on create)", wf.created)
	}
}

// TestInstall_CreateRecordActionCarriesEntityAndValues pins the PACK-EXPORT-FULL round-trip
// blocker: a create_record workflow action must carry its target entity + values through
// toWorkflowInput to CreateWorkflowInput. Before the fix the consumer dropped both, so install
// failed the whole batch at the workflows pass (WORKFLOW_ACTION_INVALID: missing entityName) with
// no rollback — a half-installed tenant. This asserts the mapping so the regression can't return.
func TestInstall_CreateRecordActionCarriesEntityAndValues(t *testing.T) {
	wf := &fakeWorkflowWriter{}
	svc, _ := newWorkflowTestService(wf)
	active := true
	pack := &Pack{
		Entities: []PackEntity{
			{Name: "wf_tickets", Label: "Tickets", Fields: []PackField{
				{Name: "subject", Label: "Subject", FieldType: "text"},
			}},
		},
		Workflows: []PackWorkflow{{
			Name:       "Auto follow-up",
			EntityName: "wf_tickets",
			Trigger:    "after_create",
			Actions: []PackWorkflowAction{{
				Type:       "create_record",
				EntityName: "wf_tickets",
				Values:     map[string]any{"subject": "Follow up"},
			}},
			IsActive: &active,
		}},
	}
	if _, err := svc.Install(context.Background(), "tenant-1", "user-1", pack); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(wf.created) != 1 || len(wf.created[0].Actions) != 1 {
		t.Fatalf("CreateWorkflow actions = %+v, want one action", wf.created)
	}
	a := wf.created[0].Actions[0]
	if a.Type != "create_record" || a.CreateEntity != "wf_tickets" {
		t.Errorf("action = {Type:%q CreateEntity:%q}, want create_record → wf_tickets", a.Type, a.CreateEntity)
	}
	if a.Values["subject"] != "Follow up" {
		t.Errorf("action.Values = %+v, want subject=Follow up", a.Values)
	}
}

// TestPackFiltersToInput pins the second round-trip blocker's building block: pack report/widget
// filters convert to the service filter shape (Field/Op/Values) intact, and an empty input yields
// nil (a widget with no filter stays unfiltered — not a spurious empty-filter object).
func TestPackFiltersToInput(t *testing.T) {
	got := packFiltersToInput([]PackReportFilter{{Field: "status", Op: "eq", Values: []string{"won"}}})
	if len(got) != 1 || got[0].Field != "status" || got[0].Op != "eq" ||
		len(got[0].Values) != 1 || got[0].Values[0] != "won" {
		t.Errorf("packFiltersToInput = %+v, want one {status eq [won]}", got)
	}
	if packFiltersToInput(nil) != nil {
		t.Errorf("packFiltersToInput(nil) = non-nil, want nil (no filters => unfiltered)")
	}
}

// TestInstall_NilWorkflowWriter_NoWorkflows: with the optional dep absent, a pack's workflows are
// silently skipped (no create, no skip counters) — back-compat with pre-PACK-WORKFLOWS behavior.
func TestInstall_NilWorkflowWriter_NoWorkflows(t *testing.T) {
	svc, _ := newWorkflowTestService(nil) // optional dep absent

	res, err := svc.Install(context.Background(), "tenant-1", "user-1", workflowTestPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.WorkflowsCreated) != 0 || len(res.WorkflowsSkipped) != 0 {
		t.Errorf("nil-writer result = created:%v skipped:%v, want both empty", res.WorkflowsCreated, res.WorkflowsSkipped)
	}
	// The entity still installed (workflows are the last data pass, absent here).
	if len(res.EntitiesCreated) != 1 {
		t.Errorf("EntitiesCreated = %v, want 1 (install proceeds without workflow writer)", res.EntitiesCreated)
	}
}
