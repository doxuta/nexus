package vertical_pack

// Unit tests for the queue / approval / team / inbound-hook upgrade adapters (specs/069 L5, T087):
// each writes through the admin service, resolves role names to ids, a team is only ever created,
// and an inbound hook keeps its signing key on update and never gets one carried in.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workqueue"
)

type opsWQ struct {
	created []services.WorkQueueInput
	updated map[string]services.WorkQueueInput
	deleted []string
}

func (f *opsWQ) ListAdmin(context.Context, string) ([]workqueue.Queue, error) { return nil, nil }
func (f *opsWQ) Create(_ context.Context, _ string, in services.WorkQueueInput) (*workqueue.Queue, error) {
	f.created = append(f.created, in)
	return &workqueue.Queue{ID: "q-new"}, nil
}
func (f *opsWQ) Update(_ context.Context, _, id string, in services.WorkQueueInput) (*workqueue.Queue, error) {
	if f.updated == nil {
		f.updated = map[string]services.WorkQueueInput{}
	}
	f.updated[id] = in
	return &workqueue.Queue{ID: id}, nil
}
func (f *opsWQ) Delete(_ context.Context, _, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type opsAP struct {
	created []services.CreateApprovalProcessInput
	updated map[string]services.UpdateApprovalProcessInput
	deleted []string
}

func (f *opsAP) ListProcesses(context.Context, string) ([]bpm.ProcessRecord, error) { return nil, nil }
func (f *opsAP) CreateProcess(_ context.Context, _ string, in services.CreateApprovalProcessInput) (*bpm.ProcessRecord, error) {
	f.created = append(f.created, in)
	return &bpm.ProcessRecord{ID: "ap-new"}, nil
}
func (f *opsAP) UpdateProcess(_ context.Context, _, id string, in services.UpdateApprovalProcessInput) (*bpm.ProcessRecord, error) {
	if f.updated == nil {
		f.updated = map[string]services.UpdateApprovalProcessInput{}
	}
	f.updated[id] = in
	return &bpm.ProcessRecord{ID: id}, nil
}
func (f *opsAP) DeleteProcess(_ context.Context, _, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type opsTeam struct {
	made      []auth.Team
	memberErr error
}

func (f *opsTeam) ListTeams(context.Context, string) ([]auth.Team, error) { return nil, nil }
func (f *opsTeam) CreateTeam(_ context.Context, t *auth.Team) error {
	f.made = append(f.made, *t)
	return nil
}
func (f *opsTeam) AddMember(context.Context, string, string, string, string) error {
	return f.memberErr
}

type opsHook struct {
	created []services.InboundHookInput
	updated map[string]services.InboundHookInput
}

func (f *opsHook) ListAdmin(context.Context, string) ([]inboundhook.Hook, error) { return nil, nil }
func (f *opsHook) Create(_ context.Context, _, _ string, in services.InboundHookInput) (*inboundhook.Hook, error) {
	f.created = append(f.created, in)
	return &inboundhook.Hook{ID: "hk-new"}, nil
}
func (f *opsHook) Update(_ context.Context, _, id string, in services.InboundHookInput) (*inboundhook.Hook, error) {
	if f.updated == nil {
		f.updated = map[string]services.InboundHookInput{}
	}
	f.updated[id] = in
	return &inboundhook.Hook{ID: id}, nil
}

func newOpsSvc(t *testing.T) (*Service, *opsWQ, *opsAP, *opsTeam, *opsHook) {
	t.Helper()
	wq, ap, tm, hk := &opsWQ{}, &opsAP{}, &opsTeam{}, &opsHook{}
	roles := &upgRoleWr{live: map[string]*services.RoleView{
		"role-lead": {ID: "role-lead", Name: "Trưởng nhóm"}, "role-qa": {ID: "role-qa", Name: "QA"}}}
	svc := NewService(Deps{EntityReader: &fakeMintMeta{}, LayoutReader: fakeApLayouts{}, RoleReader: fakeApRoleReader{}, RoleWriter: roles,
		WorkQueueWriter: wq, ApprovalWriter: ap, TeamWriter: tm, InboundHookWriter: hk}).WithItemStore(newUpgStore())
	return svc, wq, ap, tm, hk
}

func runOps(t *testing.T, svc *Service, theirs map[string]json.RawMessage, it UpgradeItem, action string) UpgradeResult {
	t.Helper()
	res := UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), branchEnv(svc, theirs), &it, action, &res)
	return res
}

func TestOpsAdapters_ReadyAndRemovable(t *testing.T) {
	svc, _, _, _, _ := newOpsSvc(t)
	for _, typ := range []string{ItemWorkQueue, ItemApproval, ItemTeam, ItemInboundHook} {
		if !svc.adapterReady(typ) {
			t.Errorf("%s: adapter should be ready", typ)
		}
	}
	if upgradeAdapters[ItemInboundHook].remove != nil {
		t.Error("an inbound hook must never be deletable by an upgrade (the partner's key would die)")
	}
	if upgradeAdapters[ItemTeam].remove != nil {
		t.Error("a team must never be deletable by an upgrade")
	}
	if upgradeAdapters[ItemWorkQueue].remove == nil || upgradeAdapters[ItemApproval].remove == nil {
		t.Error("queues and approvals soft-delete like the screen")
	}
	bare := NewService(Deps{EntityReader: &fakeMintMeta{}, LayoutReader: fakeApLayouts{}, RoleReader: fakeApRoleReader{}})
	for _, typ := range []string{ItemWorkQueue, ItemApproval, ItemTeam, ItemInboundHook} {
		if bare.adapterReady(typ) {
			t.Errorf("%s: must not be ready without a writer", typ)
		}
	}
}

func TestWorkQueueAdapter_ResolvesRoleNameAndUpdatesInPlace(t *testing.T) {
	svc, wq, _, _, _ := newOpsSvc(t)
	pq := PackWorkQueue{Name: "today", Label: "Hôm nay", EntityName: "e", RoleName: "QA", Filters: []PackQueueFilter{{Field: "status", Value: "open"}}}
	theirs := map[string]json.RawMessage{ItemWorkQueue + "/today": docRaw(t, pq)}

	res := runOps(t, svc, theirs, UpgradeItem{ItemType: ItemWorkQueue, ItemKey: "today", Status: StatusUpdateSafe, LiveID: "q-7"}, ActionApply)
	if res.Outcome != OutcomeApplied || wq.updated["q-7"].RoleID != "role-qa" || len(wq.created) != 0 {
		t.Fatalf("update: res=%+v updated=%+v", res, wq.updated)
	}
	res = runOps(t, svc, theirs, UpgradeItem{ItemType: ItemWorkQueue, ItemKey: "today", Status: StatusAdded}, ActionApply)
	if res.Outcome != OutcomeApplied || len(wq.created) != 1 || res.LiveID != "q-new" {
		t.Fatalf("create: res=%+v", res)
	}
	pq.RoleName = "Không có vai này"
	theirs[ItemWorkQueue+"/today"] = docRaw(t, pq)
	res = runOps(t, svc, theirs, UpgradeItem{ItemType: ItemWorkQueue, ItemKey: "today", Status: StatusUpdateSafe, LiveID: "q-7"}, ActionApply)
	if res.Outcome != OutcomeFailed || res.Code != "WORKQUEUE_ROLE_NOT_FOUND" {
		t.Fatalf("unknown role must fail with its code: %+v", res)
	}
	res = runOps(t, svc, theirs, UpgradeItem{ItemType: ItemWorkQueue, ItemKey: "gone", Status: StatusRemovedInPack, LiveID: "q-9"}, ActionDelete)
	if res.Outcome != OutcomeDeleted || len(wq.deleted) != 1 || wq.deleted[0] != "q-9" {
		t.Fatalf("delete: %+v %v", res, wq.deleted)
	}
}

func TestApprovalAdapter_RoleNamesBecomeIDsAndUpdateKeepsID(t *testing.T) {
	svc, _, ap, _, _ := newOpsSvc(t)
	pa := PackApproval{Name: "Duyệt", EntityName: "e", Trigger: "manual", Steps: []PackApprovalStep{
		{ID: "s1", Name: "QA", ApproverType: "role", ApproverRef: "QA"},
		{ID: "s2", Name: "Trưởng nhóm", ApproverType: "role", ApproverRef: "Trưởng nhóm"},
	}}
	theirs := map[string]json.RawMessage{ItemApproval + "/e:Duyệt": docRaw(t, pa)}

	res := runOps(t, svc, theirs, UpgradeItem{ItemType: ItemApproval, ItemKey: "e:Duyệt", Status: StatusUpdateSafe, LiveID: "ap-3"}, ActionApply)
	if res.Outcome != OutcomeApplied || len(ap.created) != 0 {
		t.Fatalf("update: %+v", res)
	}
	got := ap.updated["ap-3"]
	if len(got.Steps) != 2 || got.Steps[0].ApproverID != "role-qa" || got.Steps[1].ApproverID != "role-lead" || got.ActorID != "u1" {
		t.Fatalf("steps = %+v", got.Steps)
	}
	res = runOps(t, svc, theirs, UpgradeItem{ItemType: ItemApproval, ItemKey: "e:Duyệt", Status: StatusAdded}, ActionApply)
	if res.Outcome != OutcomeApplied || len(ap.created) != 1 || res.LiveID != "ap-new" {
		t.Fatalf("create: %+v", res)
	}
	pa.Steps[1].ApproverRef = "Vai không có"
	theirs[ItemApproval+"/e:Duyệt"] = docRaw(t, pa)
	res = runOps(t, svc, theirs, UpgradeItem{ItemType: ItemApproval, ItemKey: "e:Duyệt", Status: StatusUpdateSafe, LiveID: "ap-3"}, ActionApply)
	if res.Outcome != OutcomeFailed || res.Code != "APPROVAL_APPROVER_NOT_FOUND" {
		t.Fatalf("unresolved approver must fail, never save a process nobody can approve: %+v", res)
	}
}

func TestTeamAdapter_OnlyCreatesAndWarnsWhenNoMember(t *testing.T) {
	svc, _, _, tm, _ := newOpsSvc(t)
	theirs := map[string]json.RawMessage{ItemTeam + "/Đội A": docRaw(t, PackTeam{Name: "Đội A"})}

	res := runOps(t, svc, theirs, UpgradeItem{ItemType: ItemTeam, ItemKey: "Đội A", Status: StatusAdded}, ActionApply)
	if res.Outcome != OutcomeApplied || len(tm.made) != 1 || tm.made[0].Name != "Đội A" || tm.made[0].TenantID != upgTenant || res.LiveID != tm.made[0].ID || res.Code != "" {
		t.Fatalf("create: res=%+v made=%+v", res, tm.made)
	}
	tm.memberErr = errors.New("member user not found in tenant")
	res = runOps(t, svc, theirs, UpgradeItem{ItemType: ItemTeam, ItemKey: "Đội A", Status: StatusAdded}, ActionApply)
	if res.Outcome != OutcomeApplied || res.Code != "TEAM_NO_MEMBERS" || res.Message == "" {
		t.Fatalf("a team without a member is applied with an info code: %+v", res)
	}
	made := len(tm.made)
	res = runOps(t, svc, theirs, UpgradeItem{ItemType: ItemTeam, ItemKey: "Đội A", Status: StatusUpdateSafe, LiveID: "t-1"}, ActionApply)
	if res.Outcome != OutcomeApplied || len(tm.made) != made || res.LiveID != "t-1" {
		t.Fatalf("an existing team is never rewritten: %+v", res)
	}
}

func TestInboundHookAdapter_CreateMintsKeyUpdateKeepsIt(t *testing.T) {
	svc, _, _, _, hk := newOpsSvc(t)
	on := true
	ph := PackInboundHook{Name: "bank", EntityName: "e", Mapping: []PackHookMapping{{Source: "a.b", Field: "f"}}, MatchField: "f", IsActive: &on}
	theirs := map[string]json.RawMessage{ItemInboundHook + "/bank": docRaw(t, ph)}

	res := runOps(t, svc, theirs, UpgradeItem{ItemType: ItemInboundHook, ItemKey: "bank", Status: StatusAdded}, ActionApply)
	if res.Outcome != OutcomeApplied || len(hk.created) != 1 || res.LiveID != "hk-new" || res.Code != "NEW_HOOK_SECRET" {
		t.Fatalf("create must mint a key and say so: %+v", res)
	}
	res = runOps(t, svc, theirs, UpgradeItem{ItemType: ItemInboundHook, ItemKey: "bank", Status: StatusUpdateSafe, LiveID: "hk-4"}, ActionApply)
	if res.Outcome != OutcomeApplied || len(hk.created) != 1 || res.LiveID != "hk-4" || res.Code == "NEW_HOOK_SECRET" {
		t.Fatalf("update must not create (a new key would break the partner): %+v", res)
	}
	if hk.updated["hk-4"].EntityName != "e" || hk.updated["hk-4"].MatchField != "f" || len(hk.updated["hk-4"].Mapping) != 1 {
		t.Fatalf("updated input = %+v", hk.updated["hk-4"])
	}
	res = runOps(t, svc, theirs, UpgradeItem{ItemType: ItemInboundHook, ItemKey: "bank", Status: StatusRemovedInPack, LiveID: "hk-4"}, ActionDelete)
	if res.Outcome != OutcomeFailed {
		t.Fatalf("delete of a hook is refused: %+v", res)
	}
}
