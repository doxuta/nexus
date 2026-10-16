package vertical_pack

// specs/043 S1: a step timed on the wall clock ON PURPOSE keeps that choice across export →
// install. Dropped in transit, the installed step would silently run in working hours — the
// "configured one thing, runs another" class the D-7 gate exists to prevent.

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/services"
)

func TestPackApprovals_RoundTripKeepsSLAClock(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "ap_orders", Label: "Orders"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	steps := []bpm.Step{
		{ID: "s1", ApproverType: bpm.ApproverRole, ApproverID: "role-77", SLASeconds: 14400, SLAClock: bpm.SLAClockWall},
		{ID: "s2", ApproverType: bpm.ApproverRole, ApproverID: "role-77", SLASeconds: 14400},
	}
	src := &fakeApprovalWriter{existing: []bpm.ProcessRecord{{ID: "p1", Name: "Duyệt báo giá", EntityName: "ap_orders",
		Trigger: "manual", Steps: mustJSON(t, steps), IsActive: true}}}
	rd := fakeApRoleReader{roles: []struct{ id, name string }{{id: "role-77", name: "ap_manager"}}}
	pack, _, err := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: rd, ApprovalWriter: src}).
		Export(ctx, "tenant-1", []string{"ap_orders"}, false, nil)
	if err != nil || len(pack.Approvals) != 1 {
		t.Fatalf("export: %v %+v", err, pack)
	}
	if s := pack.Approvals[0].Steps; len(s) != 2 || s[0].SLAClock != bpm.SLAClockWall || s[1].SLAClock != "" {
		t.Fatalf("exported clocks = %+v, want [wall \"\"]", s)
	}

	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}
	wire.Roles = []PackRole{{Name: "ap_manager", Label: "Manager"}}
	dst := &fakeApprovalWriter{}
	if _, err := newApprovalTestService(dst, nil).Install(ctx, "tenant-2", "user-2", &wire); err != nil {
		t.Fatalf("install (slaClock must pass the shape gate now): %v", err)
	}
	if len(dst.created) != 1 || dst.created[0].Steps[0].SLAClock != bpm.SLAClockWall || dst.created[0].Steps[1].SLAClock != "" {
		t.Fatalf("installed = %+v, want the wall clock carried into CreateProcess", dst.created)
	}
}
