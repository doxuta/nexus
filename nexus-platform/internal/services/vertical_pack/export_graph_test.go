package vertical_pack

// specs/043 S3a2 → S4: a process stored in GRAPH form (steps '[]' + graph).
//   - S3a2 left every graph process out of the pack by name (no portable node shape yet);
//   - S4 (T083) ships the node CHAIN as the pack's `steps` — a parallel step carries
//     `approvers: [{type: "role", ref: <role NAME>}]` + `completion` — and install lifts it back to
//     a graph through the shared validator. A graph this binary cannot run (a node kind or key of a
//     later slice) is STILL left out by name, never shipped as a shorter chain.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/services"
)

func graphExportService(t *testing.T, graph []byte) (*Service, *fakeApprovalWriter) {
	t.Helper()
	meta := &fakeMintMeta{}
	ctx := context.Background()
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "ap_orders", Label: "Orders"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	ap := &fakeApprovalWriter{existing: []bpm.ProcessRecord{
		{ID: "p1", Name: "Duyệt đồ thị", EntityName: "ap_orders", Trigger: "manual",
			Steps: json.RawMessage(`[]`), Graph: graph, IsActive: true},
	}}
	return NewService(Deps{
		EntityReader: meta,
		LayoutReader: fakeApLayouts{},
		RoleReader: fakeApRoleReader{roles: []struct{ id, name string }{
			{id: "role-77", name: "ap_manager"}, {id: "role-78", name: "ap_legal"}, {id: "role-79", name: "ap_finance"}}},
		ApprovalWriter: ap,
	}), ap
}

// An unrunnable graph is left out under its own warning (never "has no approval step").
func TestExport_UnrunnableGraphProcessSkippedWithItsOwnWarning(t *testing.T) {
	graph := mustJSON(t, bpm.Graph{Nodes: []bpm.Node{
		{Step: bpm.Step{ID: "a", ApproverType: bpm.ApproverRole, ApproverID: "role-77"}, Kind: bpm.NodeApproval},
		{Step: bpm.Step{ID: "re-nhanh"}, Kind: "gateway"},
	}})
	svc, _ := graphExportService(t, graph)
	pack, warnings, err := svc.Export(context.Background(), "tenant-1", []string{"ap_orders"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Approvals) != 0 {
		t.Fatalf("a graph this binary cannot run was exported as %+v", pack.Approvals)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "Duyệt đồ thị") && strings.Contains(w, "dạng đồ thị") {
			found = true
		}
		if strings.Contains(w, "Duyệt đồ thị") && strings.Contains(w, "không có bước phê duyệt") {
			t.Fatalf("warning %q calls a graph process step-less — it has nodes", w)
		}
	}
	if !found {
		t.Fatalf("warnings %v; want one naming the graph process and why it was left out", warnings)
	}
}

// specs/043 S6a/S6b: a step handed to a NAMED PERSON and the process's fallback assignee reference
// ids that mean nothing in another tenant and have no portable pack shape — such a process is left
// out BY NAME, never exported as a plain step that would install with the wrong (or no) holder.
// (A role / team / queue step travels by name since S6b — export_assignment_refs_test.go.)
func TestExport_RoutedProcessSkippedWithItsOwnWarning(t *testing.T) {
	routed := mustJSON(t, bpm.Graph{Nodes: []bpm.Node{
		{Step: bpm.Step{ID: "a", ApproverType: bpm.ApproverRole, ApproverID: "role-77"}, Kind: bpm.NodeApproval},
		{Step: bpm.Step{ID: "b", Assignment: &bpm.Assignment{Type: bpm.AssignUser, ID: "u-1"}}, Kind: bpm.NodeApproval},
	}})
	svc, _ := graphExportService(t, routed)
	pack, warnings, err := svc.Export(context.Background(), "tenant-1", []string{"ap_orders"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Approvals) != 0 {
		t.Fatalf("a routed process was exported as %+v", pack.Approvals)
	}
	if !warningMentions(warnings, "Duyệt đồ thị", "đích danh") {
		t.Fatalf("warnings %v; want one naming the process and the named person", warnings)
	}

	// a plain linear process that carries a fallback assignee is left out too, not exported without it
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(context.Background(), "tenant-1", services.CreateEntityInput{Name: "ap_orders", Label: "Orders"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	ap := &fakeApprovalWriter{existing: []bpm.ProcessRecord{{
		ID: "p2", Name: "Có người dự phòng", EntityName: "ap_orders", Trigger: "manual", IsActive: true,
		Steps:   mustJSON(t, []bpm.Step{{ID: "s1", ApproverType: bpm.ApproverRole, ApproverID: "role-77"}}),
		Options: mustJSON(t, bpm.Options{FallbackAssignee: &bpm.Approver{Type: bpm.ApproverUser, ID: "u-1"}}),
	}}}
	svc = NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{},
		RoleReader:     fakeApRoleReader{roles: []struct{ id, name string }{{id: "role-77", name: "ap_manager"}}},
		ApprovalWriter: ap})
	pack, warnings, err = svc.Export(context.Background(), "tenant-1", []string{"ap_orders"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Approvals) != 0 || !warningMentions(warnings, "Có người dự phòng", "người dự phòng") {
		t.Fatalf("approvals %+v warnings %v; want the fallback process left out by name", pack.Approvals, warnings)
	}
}

func warningMentions(warnings []string, parts ...string) bool {
	for _, w := range warnings {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(w, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func parallelGraph(t *testing.T) []byte {
	return mustJSON(t, bpm.Graph{Nodes: []bpm.Node{
		{Step: bpm.Step{ID: "truong-phong", Name: "Trưởng phòng", ApproverType: bpm.ApproverRole, ApproverID: "role-77"}, Kind: bpm.NodeApproval},
		{Step: bpm.Step{ID: "hoi-dong", Name: "Hội đồng", SLASeconds: 7200,
			Approvers:  []bpm.Approver{{Type: bpm.ApproverRole, ID: "role-78"}, {Type: bpm.ApproverRole, ID: "role-79"}},
			Completion: &bpm.Completion{Mode: bpm.CompletionAny}}, Kind: bpm.NodeApproval},
	}})
}

// TestPackApproval_ParallelRoundTrip (T083): export a parallel (graph) process → the pack carries
// role NAMES + completion; install that pack into another tenant → the writer receives the same
// chain with the TARGET tenant's role ids, ready for the shared validator to lift to a graph.
func TestPackApproval_ParallelRoundTrip(t *testing.T) {
	svc, _ := graphExportService(t, parallelGraph(t))
	pack, warnings, err := svc.Export(context.Background(), "tenant-1", []string{"ap_orders"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Approvals) != 1 {
		t.Fatalf("exported approvals = %+v (warnings %v); want the parallel process", pack.Approvals, warnings)
	}
	steps := pack.Approvals[0].Steps
	if len(steps) != 2 || steps[0].ApproverRef != "ap_manager" || steps[0].ID != "truong-phong" {
		t.Fatalf("steps = %+v; want the plain step first, by role name", steps)
	}
	council := steps[1]
	if council.ID != "hoi-dong" || council.ApproverType != "" || council.ApproverRef != "" || council.Completion != "any" ||
		len(council.Approvers) != 2 || council.Approvers[0] != (PackApprover{Type: "role", Ref: "ap_legal"}) || council.Approvers[1].Ref != "ap_finance" {
		t.Fatalf("council step = %+v; want 2 role-name approvers, completion any, no single approver", council)
	}

	// The pack survives a JSON round trip (it is a file) …
	raw, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	var back Pack
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}

	// … and installs into a tenant whose role ids are different.
	meta := &fakeMintMeta{}
	roles := &fakeApprovalRoleWriter{existing: []services.RoleView{
		{ID: "t2-mgr", Name: "ap_manager"}, {ID: "t2-legal", Name: "ap_legal"}, {ID: "t2-fin", Name: "ap_finance"}}}
	target := &fakeApprovalWriter{}
	inst := NewService(Deps{EntityReader: meta, RoleReader: fakeApRoleReader{roles: []struct{ id, name string }{
		{id: "t2-mgr", name: "ap_manager"}, {id: "t2-legal", name: "ap_legal"}, {id: "t2-fin", name: "ap_finance"}}},
		EntityWriter: meta, RoleWriter: roles, ApprovalWriter: target})
	if _, err := inst.Install(context.Background(), "tenant-2", "user-1", &back); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(target.created) != 1 {
		t.Fatalf("created %d processes; want 1", len(target.created))
	}
	got := target.created[0].Steps
	if len(got) != 2 || got[0].ApproverID != "t2-mgr" || len(got[1].Approvers) != 2 ||
		got[1].Approvers[0] != (services.ApprovalApproverInput{Type: "role", ID: "t2-legal"}) || got[1].Approvers[1].ID != "t2-fin" ||
		got[1].Completion == nil || got[1].Completion.Mode != "any" || got[1].ID != "hoi-dong" || got[1].SLASeconds != 7200 {
		t.Fatalf("installed steps = %+v; want the chain with the target tenant's role ids", got)
	}
}

// A parallel step with a named person is not portable: the whole process is left out, warned.
func TestExport_ParallelStepWithAUserApproverIsDroppedWithAWarning(t *testing.T) {
	graph := mustJSON(t, bpm.Graph{Nodes: []bpm.Node{
		{Step: bpm.Step{ID: "hoi-dong", Approvers: []bpm.Approver{{Type: bpm.ApproverRole, ID: "role-78"}, {Type: bpm.ApproverUser, ID: "u-1"}}}, Kind: bpm.NodeApproval},
	}})
	svc, _ := graphExportService(t, graph)
	pack, warnings, err := svc.Export(context.Background(), "tenant-1", []string{"ap_orders"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Approvals) != 0 {
		t.Fatalf("exported %+v; a named person is not portable", pack.Approvals)
	}
	ok := false
	for _, w := range warnings {
		if strings.Contains(w, "đích danh") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("warnings %v; want the named-person warning", warnings)
	}
}

// A hand-authored pack naming a person (or an unknown role) in a council skips the process with a
// warning instead of failing the committed install.
func TestInstall_ParallelStepUnresolvableApproverSkipsWithAWarning(t *testing.T) {
	for name, approver := range map[string]PackApprover{
		"a person":     {Type: "user", Ref: "boss@example.com"},
		"unknown role": {Type: "role", Ref: "khong_co"},
	} {
		t.Run(name, func(t *testing.T) {
			meta := &fakeMintMeta{}
			target := &fakeApprovalWriter{}
			svc := NewService(Deps{EntityReader: meta, RoleReader: fakeMintRoles{}, EntityWriter: meta, RoleWriter: &fakeApprovalRoleWriter{}, ApprovalWriter: target})
			p := &Pack{
				Entities: []PackEntity{{Name: "ap_orders", Label: "Orders", Fields: []PackField{{Name: "code", Label: "Code", FieldType: "text"}}}},
				Roles:    []PackRole{{Name: "ap_manager", Label: "Manager"}},
				Approvals: []PackApproval{{Name: "H", EntityName: "ap_orders", Trigger: "manual", Steps: []PackApprovalStep{
					{Approvers: []PackApprover{{Type: "role", Ref: "ap_manager"}, approver}}}}},
			}
			res, err := svc.Install(context.Background(), "tenant-1", "user-1", p)
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			if len(target.created) != 0 || len(res.ApprovalsSkipped) != 1 || len(res.Warnings) == 0 {
				t.Fatalf("created=%d skipped=%v warnings=%v; want the process skipped with a warning", len(target.created), res.ApprovalsSkipped, res.Warnings)
			}
		})
	}
}

// A malformed council in a pack (one approver, a stray approverType beside approvers, a duplicate, a bad
// mode) is refused BEFORE any write with the API's code - not found by CreateProcess in the late
// approvals pass, after entities and roles committed with no rollback.
func TestInstall_MalformedCouncilRefusedBeforeAnyWrite(t *testing.T) {
	role := func(r string) PackApprover { return PackApprover{Type: "role", Ref: r} }
	for name, step := range map[string]PackApprovalStep{
		"one approver":       {Approvers: []PackApprover{role("ap_manager")}},
		"stray approverType": {ApproverType: "role", ApproverRef: "ap_manager", Approvers: []PackApprover{role("ap_manager"), role("ap_legal")}},
		"duplicate":          {Approvers: []PackApprover{role("ap_manager"), role("ap_manager")}},
		"bad mode":           {Approvers: []PackApprover{role("ap_manager"), role("ap_legal")}, Completion: "majority"}, // "quorum" was this row until S6b
		"mode on one person": {ApproverType: "role", ApproverRef: "ap_manager", Completion: "all"},
	} {
		t.Run(name, func(t *testing.T) {
			meta := &fakeMintMeta{}
			target := &fakeApprovalWriter{}
			svc := NewService(Deps{EntityReader: meta, RoleReader: fakeMintRoles{}, EntityWriter: meta, RoleWriter: &fakeApprovalRoleWriter{}, ApprovalWriter: target})
			p := &Pack{
				Entities:  []PackEntity{{Name: "ap_orders", Label: "Orders", Fields: []PackField{{Name: "code", Label: "Code", FieldType: "text"}}}},
				Roles:     []PackRole{{Name: "ap_manager", Label: "Manager"}, {Name: "ap_legal", Label: "Legal"}},
				Approvals: []PackApproval{{Name: "H", EntityName: "ap_orders", Trigger: "manual", Steps: []PackApprovalStep{step}}},
			}
			_, err := svc.Install(context.Background(), "tenant-1", "user-1", p)
			var ve *services.ValidationError
			if !errors.As(err, &ve) || ve.Code != "APPROVAL_STEP_INVALID" {
				t.Fatalf("err = %v; want APPROVAL_STEP_INVALID", err)
			}
			if got, _ := meta.ListEntities("tenant-1"); len(got) != 0 {
				t.Fatalf("%d entities were written before the refusal", len(got))
			}
		})
	}
}
