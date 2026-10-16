package vertical_pack

// specs/043 S8: a step's answer to a rejection (onReject) and the outcome's returnedValue survive
// export → install. Dropped in transit, the installed process would turn "send it back to the
// submitter" into a final reject — the "configured one thing, runs another" class the D-7 gate
// exists to prevent — and a malformed one is refused BEFORE any write.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/services"
)

func TestPackApprovals_RoundTripKeepsOnReject(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "ap_orders", Label: "Orders"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	graph := bpm.Graph{Nodes: []bpm.Node{
		{Step: bpm.Step{ID: "truong-phong", ApproverType: bpm.ApproverRole, ApproverID: "role-77"}, Kind: bpm.NodeApproval},
		{Step: bpm.Step{ID: "ke-toan", ApproverType: bpm.ApproverRole, ApproverID: "role-77"}, Kind: bpm.NodeApproval,
			OnReject: &bpm.OnReject{To: bpm.RejectToSubmitter, ResumeAt: bpm.ResumeStart}},
		{Step: bpm.Step{ID: "giam-doc", ApproverType: bpm.ApproverRole, ApproverID: "role-77"}, Kind: bpm.NodeApproval,
			OnReject: &bpm.OnReject{To: "truong-phong"}},
	}}
	outcome := bpm.Outcome{StatusField: "status", ApprovedValue: "Chấp nhận", ReturnedValue: "Cần bổ sung"}
	src := &fakeApprovalWriter{existing: []bpm.ProcessRecord{{ID: "p1", Name: "Duyệt báo giá", EntityName: "ap_orders",
		Trigger: "manual", Steps: json.RawMessage("[]"), Graph: mustJSON(t, graph), Outcome: mustJSON(t, outcome), IsActive: true}}}
	rd := fakeApRoleReader{roles: []struct{ id, name string }{{id: "role-77", name: "ap_manager"}}}
	pack, _, err := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: rd, ApprovalWriter: src}).
		Export(ctx, "tenant-1", []string{"ap_orders"}, false, nil)
	if err != nil || len(pack.Approvals) != 1 {
		t.Fatalf("export: %v %+v", err, pack)
	}
	s := pack.Approvals[0].Steps
	if len(s) != 3 || s[0].OnReject != nil || s[1].OnReject == nil || s[1].OnReject.To != "@submitter" || s[1].OnReject.ResumeAt != "start" ||
		s[2].OnReject == nil || s[2].OnReject.To != "truong-phong" {
		t.Fatalf("exported steps = %+v; want onReject on steps 2 and 3 only", s)
	}
	if o := pack.Approvals[0].Outcome; o == nil || o.ReturnedValue != "Cần bổ sung" {
		t.Fatalf("exported outcome = %+v; want returnedValue carried", o)
	}

	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}
	wire.Roles = []PackRole{{Name: "ap_manager", Label: "Manager"}}
	dst := &fakeApprovalWriter{}
	if _, err := newApprovalTestService(dst, nil).Install(ctx, "tenant-2", "user-2", &wire); err != nil {
		t.Fatalf("install (onReject must pass the shape gate): %v", err)
	}
	if len(dst.created) != 1 {
		t.Fatalf("created = %+v; want one process", dst.created)
	}
	in := dst.created[0]
	if in.Steps[0].OnReject != nil || in.Steps[1].OnReject == nil || in.Steps[1].OnReject.To != "@submitter" || in.Steps[1].OnReject.ResumeAt != "start" ||
		in.Steps[2].OnReject == nil || in.Steps[2].OnReject.To != "truong-phong" {
		t.Fatalf("installed steps = %+v; want onReject carried into CreateProcess", in.Steps)
	}
	if in.Outcome == nil || in.Outcome.ReturnedValue != "Cần bổ sung" {
		t.Fatalf("installed outcome = %+v; want returnedValue carried", in.Outcome)
	}
}

// A pack whose onReject the engine would not run is refused up front — nothing written.
func TestPackApprovals_OnRejectGate(t *testing.T) {
	ctx := context.Background()
	for name, steps := range map[string][]PackApprovalStep{
		"unknown target": {{ID: "a", ApproverType: "role", ApproverRef: "ap_manager"},
			{ApproverType: "role", ApproverRef: "ap_manager", OnReject: &PackOnReject{To: "zzz"}}},
		"later step": {{ID: "a", ApproverType: "role", ApproverRef: "ap_manager", OnReject: &PackOnReject{To: "b"}},
			{ID: "b", ApproverType: "role", ApproverRef: "ap_manager"}},
		"resume beside a node target": {{ID: "a", ApproverType: "role", ApproverRef: "ap_manager"},
			{ApproverType: "role", ApproverRef: "ap_manager", OnReject: &PackOnReject{To: "a", ResumeAt: "start"}}},
	} {
		t.Run(name, func(t *testing.T) {
			dst := &fakeApprovalWriter{}
			wire := Pack{Roles: []PackRole{{Name: "ap_manager", Label: "Manager"}},
				Approvals: []PackApproval{{Name: "P", EntityName: "ap_orders", Trigger: "manual", Steps: steps}}}
			_, err := newApprovalTestService(dst, nil).Install(ctx, "tenant-2", "user-2", &wire)
			if err == nil {
				t.Fatalf("install accepted a pack the engine would not run")
			}
			if len(dst.created) != 0 {
				t.Fatalf("a refused pack wrote %+v", dst.created)
			}
		})
	}
}

// returnedValue with no @submitter step is refused BEFORE any write (finding: it used to surface
// only in CreateProcess, after entities were committed).
func TestPackApprovals_ReturnedValueUnusedGate(t *testing.T) {
	dst := &fakeApprovalWriter{}
	wire := Pack{Roles: []PackRole{{Name: "ap_manager", Label: "Manager"}},
		Approvals: []PackApproval{{Name: "P", EntityName: "ap_orders", Trigger: "manual",
			Outcome: &PackApprovalOutcome{StatusField: "status", ReturnedValue: "returned"},
			Steps:   []PackApprovalStep{{ApproverType: "role", ApproverRef: "ap_manager"}}}}}
	err := approvalShapeGate(wire.Approvals)
	var ve *services.ValidationError
	if !errors.As(err, &ve) || ve.Code != "APPROVAL_OUTCOME_VALUE_UNUSED" {
		t.Fatalf("gate err = %v; want APPROVAL_OUTCOME_VALUE_UNUSED", err)
	}
	if _, err := newApprovalTestService(dst, nil).Install(context.Background(), "tenant-2", "user-2", &wire); err == nil || len(dst.created) != 0 {
		t.Fatalf("install err=%v wrote %+v", err, dst.created)
	}
}
