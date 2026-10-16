package vertical_pack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workqueue"
)

// specs/043 S6b (T117, T257): a step handed to a role / team / work queue travels in a pack by NAME
// (ids are the source tenant's and mean nothing elsewhere); a named person does not travel at all —
// the process is left out with the same honest warning as before (F6b). Field / submitter rules and
// the strategy + quorum percent travel as they are.

type fakeApTeams struct{ teams []auth.Team }

func (f fakeApTeams) ListTeams(context.Context, string) ([]auth.Team, error) { return f.teams, nil }
func (f fakeApTeams) CreateTeam(context.Context, *auth.Team) error           { return nil }
func (f fakeApTeams) AddMember(context.Context, string, string, string, string) error {
	return nil
}

type fakeApQueues struct{ queues []workqueue.Queue }

func (f fakeApQueues) ListAdmin(context.Context, string) ([]workqueue.Queue, error) {
	return f.queues, nil
}
func (f fakeApQueues) Create(context.Context, string, services.WorkQueueInput) (*workqueue.Queue, error) {
	return nil, nil
}

func routedPackGraph(t *testing.T, nodes ...bpm.Node) []byte {
	t.Helper()
	return mustJSON(t, bpm.Graph{Nodes: nodes})
}

func assignNode(id string, a bpm.Assignment, c *bpm.Completion) bpm.Node {
	return bpm.Node{Step: bpm.Step{ID: id, Name: id, Assignment: &a, Completion: c, SLASeconds: 3600}, Kind: bpm.NodeApproval}
}

func exportRouted(t *testing.T, graph []byte, teams []auth.Team, queues []workqueue.Queue) (*Pack, []string) {
	t.Helper()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(context.Background(), "tenant-1", services.CreateEntityInput{Name: "ap_orders", Label: "Orders"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	ap := &fakeApprovalWriter{existing: []bpm.ProcessRecord{
		{ID: "p1", Name: "Có nhóm", EntityName: "ap_orders", Trigger: "manual", Steps: json.RawMessage(`[]`), Graph: graph, IsActive: true},
	}}
	svc := NewService(Deps{
		EntityReader: meta, LayoutReader: fakeApLayouts{},
		RoleReader:      fakeApRoleReader{roles: []struct{ id, name string }{{id: "role-77", name: "ap_manager"}}},
		TeamWriter:      fakeApTeams{teams: teams},
		WorkQueueWriter: fakeApQueues{queues: queues},
		ApprovalWriter:  ap,
	})
	pack, warnings, err := svc.Export(context.Background(), "tenant-1", []string{"ap_orders"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return pack, warnings
}

func TestPackApproval_TeamQueueRefsByName_UserRefDroppedWithWarning(t *testing.T) {
	teams := []auth.Team{{ID: "team-1", Name: "Ban duyệt"}}
	queues := []workqueue.Queue{{ID: "queue-1", Name: "ke_toan"}}
	graph := routedPackGraph(t,
		assignNode("role-step", bpm.Assignment{Type: bpm.AssignRole, ID: "role-77"}, nil),
		assignNode("rr", bpm.Assignment{Type: bpm.AssignTeam, ID: "team-1", Strategy: bpm.StrategyRoundRobin}, nil),
		assignNode("hoi-dong", bpm.Assignment{Type: bpm.AssignTeam, ID: "team-1", Strategy: bpm.StrategyEachMember}, &bpm.Completion{Mode: bpm.CompletionQuorum, Percent: 60}),
		assignNode("kt", bpm.Assignment{Type: bpm.AssignQueue, ID: "queue-1"}, nil),
		assignNode("chu", bpm.Assignment{Type: bpm.AssignField, Field: "owner_id"}, nil),
		assignNode("gui", bpm.Assignment{Type: bpm.AssignSubmitter}, nil),
	)
	pack, warnings := exportRouted(t, graph, teams, queues)
	if len(pack.Approvals) != 1 {
		t.Fatalf("approvals = %+v (warnings %v); want the routed process exported", pack.Approvals, warnings)
	}
	steps := pack.Approvals[0].Steps
	if len(steps) != 6 {
		t.Fatalf("steps = %+v; want 6", steps)
	}
	want := []PackAssignment{
		{Type: "role", Ref: "ap_manager"},
		{Type: "team", Ref: "Ban duyệt", Strategy: "round_robin"},
		{Type: "team", Ref: "Ban duyệt", Strategy: "each_member"},
		{Type: "queue", Ref: "ke_toan"},
		{Type: "field", Field: "owner_id"},
		{Type: "submitter"},
	}
	for i, w := range want {
		if steps[i].Assignment == nil || *steps[i].Assignment != w || steps[i].ApproverType != "" || steps[i].ApproverRef != "" {
			t.Errorf("step %d assignment = %+v; want %+v and no single approver", i, steps[i].Assignment, w)
		}
	}
	if steps[2].Completion != "quorum" || steps[2].CompletionPercent != 60 {
		t.Errorf("each_member step completion = %q/%d; want quorum/60", steps[2].Completion, steps[2].CompletionPercent)
	}
	if steps[1].Completion != "" || steps[1].CompletionPercent != 0 {
		t.Errorf("a one-person strategy step carries a completion: %q/%d", steps[1].Completion, steps[1].CompletionPercent)
	}

	// a named person is NOT portable: the whole process is left out, warned (F6b)
	pack, warnings = exportRouted(t, routedPackGraph(t,
		assignNode("a", bpm.Assignment{Type: bpm.AssignRole, ID: "role-77"}, nil),
		assignNode("b", bpm.Assignment{Type: bpm.AssignUser, ID: "u-1"}, nil)), teams, queues)
	if len(pack.Approvals) != 0 || !warningMentions(warnings, "Có nhóm", "đích danh") {
		t.Fatalf("approvals %+v warnings %v; want the process left out for the named person", pack.Approvals, warnings)
	}
	// a team / queue that no longer exists cannot be named: left out too, never exported with a blank
	for name, a := range map[string]bpm.Assignment{
		"team":  {Type: bpm.AssignTeam, ID: "team-gone"},
		"queue": {Type: bpm.AssignQueue, ID: "queue-gone"},
	} {
		pack, warnings = exportRouted(t, routedPackGraph(t, assignNode("a", a, nil)), teams, queues)
		if len(pack.Approvals) != 0 || !warningMentions(warnings, "Có nhóm", "không còn tồn tại") {
			t.Fatalf("%s: approvals %+v warnings %v; want the process left out as unresolvable", name, pack.Approvals, warnings)
		}
	}
}

// The other half of T257: the pack installs into a tenant whose team / queue / role have other ids,
// and the writer receives THAT tenant's ids (the shared validator then lifts the chain to a graph).
func TestPackApproval_AssignmentRoundTripResolvesTargetIds(t *testing.T) {
	teams := []auth.Team{{ID: "team-1", Name: "Ban duyệt"}}
	queues := []workqueue.Queue{{ID: "queue-1", Name: "ke_toan"}}
	graph := routedPackGraph(t,
		assignNode("hoi-dong", bpm.Assignment{Type: bpm.AssignTeam, ID: "team-1", Strategy: bpm.StrategyEachMember}, &bpm.Completion{Mode: bpm.CompletionQuorum, Percent: 60}),
		assignNode("kt", bpm.Assignment{Type: bpm.AssignQueue, ID: "queue-1"}, nil),
		assignNode("rr", bpm.Assignment{Type: bpm.AssignTeam, ID: "team-1", Strategy: bpm.StrategyLoadBased}, nil),
		assignNode("chu", bpm.Assignment{Type: bpm.AssignField, Field: "owner_id"}, nil),
	)
	pack, warnings := exportRouted(t, graph, teams, queues)
	if len(pack.Approvals) != 1 {
		t.Fatalf("export: %+v %v", pack.Approvals, warnings)
	}
	raw, err := json.Marshal(pack) // a pack is a file
	if err != nil {
		t.Fatal(err)
	}
	var back Pack
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}

	meta := &fakeMintMeta{}
	target := &fakeApprovalWriter{}
	inst := NewService(Deps{
		EntityReader: meta, EntityWriter: meta,
		RoleReader: fakeApRoleReader{}, RoleWriter: &fakeApprovalRoleWriter{},
		TeamWriter:      fakeApTeams{teams: []auth.Team{{ID: "t2-team", Name: "Ban duyệt"}}},
		WorkQueueWriter: fakeApQueues{queues: []workqueue.Queue{{ID: "t2-queue", Name: "ke_toan"}}},
		ApprovalWriter:  target,
	})
	res, err := inst.Install(context.Background(), "tenant-2", "user-1", &back)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(target.created) != 1 {
		t.Fatalf("created %d processes (skipped %v, warnings %v); want 1", len(target.created), res.ApprovalsSkipped, res.Warnings)
	}
	got := target.created[0].Steps
	if len(got) != 4 {
		t.Fatalf("installed steps = %+v", got)
	}
	a0, a1, a2, a3 := got[0].Assignment, got[1].Assignment, got[2].Assignment, got[3].Assignment
	if a0 == nil || a0.Type != "team" || a0.ID != "t2-team" || a0.Strategy != "each_member" ||
		got[0].Completion == nil || got[0].Completion.Mode != "quorum" || got[0].Completion.Percent != 60 {
		t.Errorf("each_member step = %+v / %+v; want team t2-team, each_member, quorum 60", a0, got[0].Completion)
	}
	if a1 == nil || a1.Type != "queue" || a1.ID != "t2-queue" {
		t.Errorf("queue step = %+v; want queue t2-queue", a1)
	}
	if a2 == nil || a2.ID != "t2-team" || a2.Strategy != "load_based" || got[2].Completion != nil {
		t.Errorf("load_based step = %+v / %+v; want team t2-team, no completion", a2, got[2].Completion)
	}
	if a3 == nil || a3.Type != "field" || a3.Field != "owner_id" {
		t.Errorf("field step = %+v; want the field rule as it is", a3)
	}
}

// A hand-authored pack naming a team or queue the target does not have, or a person, skips the
// process with a warning (the committed install is not failed), as an unknown role does.
func TestInstall_AssignmentUnresolvableSkipsWithAWarning(t *testing.T) {
	for name, tc := range map[string]struct {
		a    PackAssignment
		word string
	}{
		"unknown team":  {PackAssignment{Type: "team", Ref: "Không có"}, "nhóm"},
		"unknown queue": {PackAssignment{Type: "queue", Ref: "khong_co"}, "hàng đợi"},
		"unknown role":  {PackAssignment{Type: "role", Ref: "khong_co"}, "role"},
		"a person":      {PackAssignment{Type: "user", Ref: "boss@example.com"}, "đích danh"},
	} {
		t.Run(name, func(t *testing.T) {
			meta := &fakeMintMeta{}
			target := &fakeApprovalWriter{}
			svc := NewService(Deps{EntityReader: meta, EntityWriter: meta, RoleReader: fakeMintRoles{}, RoleWriter: &fakeApprovalRoleWriter{},
				TeamWriter: fakeApTeams{}, WorkQueueWriter: fakeApQueues{}, ApprovalWriter: target})
			p := &Pack{
				Entities: []PackEntity{{Name: "ap_orders", Label: "Orders", Fields: []PackField{{Name: "code", Label: "Code", FieldType: "text"}}}},
				Approvals: []PackApproval{{Name: "H", EntityName: "ap_orders", Trigger: "manual", Steps: []PackApprovalStep{
					{Assignment: &tc.a}}}},
			}
			res, err := svc.Install(context.Background(), "tenant-1", "user-1", p)
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			if len(target.created) != 0 || len(res.ApprovalsSkipped) != 1 {
				t.Fatalf("created %d skipped %v; want the process skipped", len(target.created), res.ApprovalsSkipped)
			}
			if !warningMentions(res.Warnings, "H", tc.word) {
				t.Fatalf("warnings %v; want one naming the process and %q", res.Warnings, tc.word)
			}
		})
	}
}

// The shape of a pack assignment is judged by the engine's own check BEFORE anything is written.
func TestInstall_AssignmentShapeIsRefusedBeforeAnyWrite(t *testing.T) {
	for name, st := range map[string]PackApprovalStep{
		"round_robin on a role":          {Assignment: &PackAssignment{Type: "role", Ref: "r", Strategy: "round_robin"}},
		"each_member without completion": {Assignment: &PackAssignment{Type: "team", Ref: "t", Strategy: "each_member"}, CompletionPercent: 60},
		"quorum without a percent":       {Assignment: &PackAssignment{Type: "team", Ref: "t", Strategy: "each_member"}, Completion: "quorum"},
		"assignment beside approverRef":  {ApproverType: "role", ApproverRef: "r", Assignment: &PackAssignment{Type: "team", Ref: "t"}},
		"completion beside a pool team":  {Assignment: &PackAssignment{Type: "team", Ref: "t"}, Completion: "all"},
		"unknown assignment type":        {Assignment: &PackAssignment{Type: "boss", Ref: "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := approvalShapeGate([]PackApproval{{Name: "H", EntityName: "e", Trigger: "manual", Steps: []PackApprovalStep{st}}})
			if err == nil {
				t.Fatal("a malformed pack assignment passed the shape gate")
			}
			if !strings.Contains(err.Error(), "H") && !strings.Contains(err.Error(), "quy trình") {
				t.Fatalf("error %v does not name the process", err)
			}
		})
	}
	ok := PackApprovalStep{Assignment: &PackAssignment{Type: "team", Ref: "t", Strategy: "each_member"}, Completion: "quorum", CompletionPercent: 60}
	if err := approvalShapeGate([]PackApproval{{Name: "H", EntityName: "e", Trigger: "manual", Steps: []PackApprovalStep{ok}}}); err != nil {
		t.Fatalf("a well-formed team council was refused: %v", err)
	}
}
