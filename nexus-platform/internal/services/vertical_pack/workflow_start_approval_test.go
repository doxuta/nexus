package vertical_pack

import (
	"context"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/services"
)

// specs/043 S9b: a pack names the approval process a rule starts; install turns the name into the
// target tenant's id, per (entity, name), and defers such rules until the approvals exist.
func TestToWorkflowInput_StartApprovalResolvesProcessName(t *testing.T) {
	pw := PackWorkflow{
		Name: "Gia hạn", EntityName: "crm_contracts", Trigger: "date_offset",
		Actions: []PackWorkflowAction{{Type: "start_approval", ProcessName: "Duyệt gia hạn"}},
	}
	ids := map[string]string{approvalDupKey("crm_contracts", "Duyệt gia hạn"): "proc-123"}
	in, err := toWorkflowInput(pw, "u1", nil, ids, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := in.Actions[0].ProcessID; got != "proc-123" {
		t.Fatalf("ProcessID = %q, want the target tenant's id", got)
	}
	// Same name on ANOTHER entity is a different process.
	pw.EntityName = "crm_quotes"
	in, err = toWorkflowInput(pw, "u1", nil, ids, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := in.Actions[0].ProcessID; got != "" {
		t.Fatalf("ProcessID = %q, want empty (unresolved → the save refuses it)", got)
	}
}

func TestPackWorkflowStartsApproval(t *testing.T) {
	yes := PackWorkflow{Actions: []PackWorkflowAction{{Type: "field_update"}, {Type: " start_approval "}}}
	no := PackWorkflow{Actions: []PackWorkflowAction{{Type: "field_update"}}}
	if !packWorkflowStartsApproval(yes) || packWorkflowStartsApproval(no) {
		t.Fatal("only a rule with a start_approval action waits for the approvals pass")
	}
}

// A start_approval naming a process that exists but is not usable (switched off / automatic) is
// refused by CreateWorkflow with WF_START_APPROVAL_INVALID; at Pass 8d' the install cannot roll
// back, so it must skip THAT rule with a warning instead of aborting.
func TestInstall_StartApprovalInvalidProcessSkipsWithWarning(t *testing.T) {
	wf := &fakeWorkflowWriter{failWith: &services.ValidationError{Code: "WF_START_APPROVAL_INVALID", Msg: "quy trình đang tắt"}}
	ap := &fakeApprovalWriter{existing: []bpm.ProcessRecord{{ID: "p1", Name: "Duyệt", EntityName: "wf_tickets"}}}
	meta := &fakeMintMeta{}
	svc := NewService(Deps{EntityReader: meta, RoleReader: fakeMintRoles{}, EntityWriter: meta, WorkflowWriter: wf, ApprovalWriter: ap})
	pack := &Pack{
		Entities: []PackEntity{{Name: "wf_tickets", Label: "Tickets", Fields: []PackField{{Name: "subject", Label: "Subject", FieldType: "text"}}}},
		Workflows: []PackWorkflow{{
			Name: "Mở duyệt", EntityName: "wf_tickets", Trigger: "after_update",
			Actions: []PackWorkflowAction{{Type: "start_approval", ProcessName: "Duyệt"}},
		}},
	}
	res, err := svc.Install(context.Background(), "tenant-1", "user-1", pack)
	if err != nil {
		t.Fatalf("install must not abort: %v", err)
	}
	if len(res.WorkflowsSkipped) != 1 || len(res.WorkflowsCreated) != 0 || len(res.Warnings) == 0 {
		t.Fatalf("skipped=%v created=%v warnings=%v", res.WorkflowsSkipped, res.WorkflowsCreated, res.Warnings)
	}
}
