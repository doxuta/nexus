//go:build integration

// DAILY-QUEUE #433 / specs/069 L5 against real MySQL and the real queue, approval, team and inbound
// hook services: the pack ships a new version of a work queue, an approval process (one more step),
// a webhook receiver and a team; the upgrade carries them over while (1) a request already waiting
// for approval keeps its one-step snapshot and a new request gets two steps, (2) the old receiver
// still accepts a signal signed with the OLD key, (3) a NEW receiver gets its own non-empty key,
// (4) a queue the builder switched off is "undiffable" and offers Keep only.
//
//	TEST_DATABASE_URL=... bash scripts/run-integration.sh ./internal/services/vertical_pack/ -run UpgradeOps
package vertical_pack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const opsSlug = "ops-hooks"

type opsResolver struct{}

func (opsResolver) ResolveTenantBySlug(_ context.Context, slug string) (*auth.Tenant, error) {
	if slug == opsSlug {
		return &auth.Tenant{ID: vpTenantA, Slug: slug}, nil
	}
	return nil, auth.ErrTenantNotFound
}

func opsSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

type opsStack struct {
	st    *slaStack
	store *protStore
	appr  *services.ApprovalService
	bpm   *bpm.SQLRepository
	hooks *services.InboundHookService
}

func opsSetup(t *testing.T) *opsStack {
	t.Helper()
	gdb, sdb := vpSetup(t)
	st := vpBuildSLA(gdb, sdb)
	t.Cleanup(func() { st.drain(t) })
	slaSeedInstaller(t, gdb)
	if _, err := services.SeedDefaultBusinessCalendar(context.Background(), gdb, vpTenantA); err != nil {
		t.Fatalf("seed calendar: %v", err)
	}
	o := &opsStack{st: st, store: &protStore{upgStore: newUpgStore(), prot: map[string]bool{}}}
	o.bpm = bpm.NewSQLRepository(sdb, nil)
	o.appr = services.NewApprovalService(o.bpm, o.bpm, nil).WithEntityChecker(metadata.NewRepository(gdb))
	o.hooks = services.NewInboundHookService(inboundhook.New(gdb), st.records, opsResolver{}, zap.NewNop())
	st.pack.WithItemStore(o.store)
	st.pack.approvalWr = o.appr
	st.pack.hookWr = o.hooks
	return o
}

// opsPack is the ticketing pack with an approval and a receiver added, shaped per version.
func opsPack(t *testing.T, version string, v2 bool) *Pack {
	return ticketingNext(t, version, func(p *Pack) {
		on := true
		steps := []PackApprovalStep{{ID: "s1", Name: "Nhân viên", ApproverType: "role", ApproverRef: "tk_agent"}}
		mapping := []PackHookMapping{{Source: "title", Field: "subject"}}
		if v2 {
			steps = append(steps, PackApprovalStep{ID: "s2", Name: "Trưởng nhóm", ApproverType: "role", ApproverRef: "tk_manager"})
			mapping = append(mapping, PackHookMapping{Source: "body", Field: "description"})
			for i := range p.WorkQueues {
				if p.WorkQueues[i].Name == "ticket_khan_dang_mo" {
					p.WorkQueues[i].Filters = []PackQueueFilter{{Field: "priority", Operator: "eq", Value: "Urgent"}}
				}
			}
			p.Teams = append(p.Teams, PackTeam{Name: "Đội mới"})
			p.InboundHooks = append(p.InboundHooks, PackInboundHook{Name: "intake-b", EntityName: "tk_tickets",
				Mapping: []PackHookMapping{{Source: "title", Field: "subject"}}, IsActive: &on})
		}
		p.Approvals = []PackApproval{{Name: "Duyệt đóng phiếu", EntityName: "tk_tickets", Trigger: "manual", Steps: steps, IsActive: &on}}
		p.InboundHooks = append([]PackInboundHook{{Name: "intake-a", EntityName: "tk_tickets", Mapping: mapping, IsActive: &on}}, p.InboundHooks...)
	})
}

func (o *opsStack) snapshotSteps(t *testing.T, instanceID string) int {
	t.Helper()
	var raw string
	if err := o.st.sdb.GetContext(context.Background(), &raw,
		"SELECT process_snapshot FROM sys_approval_instances WHERE tenant_id = ? AND id = ?", vpTenantA, instanceID); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var def bpm.Definition
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return len(def.Steps)
}

// arm starts a request on the process exactly as the engine does: it pins the live steps.
func (o *opsStack) arm(t *testing.T, recordID string) string {
	t.Helper()
	ctx := context.Background()
	procs, err := o.appr.ListProcesses(ctx, vpTenantA)
	if err != nil || len(procs) == 0 {
		t.Fatalf("list processes: %v (%d)", err, len(procs))
	}
	var steps []bpm.Step
	if err := json.Unmarshal(procs[0].Steps, &steps); err != nil {
		t.Fatal(err)
	}
	id, err := o.bpm.CreateInstance(ctx, bpm.CreateInstanceInput{
		TenantID: vpTenantA, ProcessID: procs[0].ID, EntityName: "tk_tickets", RecordID: recordID,
		CurrentApproverType: steps[0].ApproverType, CurrentApproverID: steps[0].ApproverID,
		Definition: &bpm.Definition{Steps: steps}, CreatedBy: vpSeedUserID,
	})
	if err != nil {
		t.Fatalf("arm instance: %v", err)
	}
	return id
}

func TestUpgradeOps_QueueApprovalHookTeamCarryOverAndKeepTheOldKey(t *testing.T) {
	o := opsSetup(t)
	ctx := context.Background()
	v1 := opsPack(t, "1.8.0", false)
	if _, err := o.st.pack.Install(ctx, vpTenantA, vpSeedUserID, v1); err != nil {
		t.Fatalf("install: %v", err)
	}

	// T090 gate: a fresh install is 100% unchanged, the receiver included.
	rep, err := o.st.pack.DiffInstalled(ctx, vpTenantA, "ticketing")
	if err != nil {
		t.Fatal(err)
	}
	sawHook := false
	for _, it := range rep.Items {
		if it.ItemType == ItemInboundHook {
			sawHook = true
		}
		if it.Status != "unchanged" {
			t.Errorf("FRESH INSTALL NOT CLEAN: %s/%s status=%s changed=%v", it.ItemType, it.ItemKey, it.Status, it.ChangedPaths)
		}
	}
	if !sawHook {
		t.Fatal("the diff has no inbound_hook row: the receiver is not tracked")
	}

	// The partner already sends, signed with the key of the first install.
	hooks, err := o.hooks.ListAdmin(ctx, vpTenantA)
	if err != nil {
		t.Fatal(err)
	}
	oldSecret := ""
	for _, h := range hooks {
		if h.Name == "intake-a" {
			oldSecret = h.Secret
		}
	}
	if oldSecret == "" {
		t.Fatal("install did not mint a key for intake-a")
	}
	body := []byte(`{"title":"từ đối tác","body":"x"}`)
	if _, err := o.hooks.Receive(ctx, opsSlug, "intake-a", opsSign(oldSecret, body), body); err != nil {
		t.Fatalf("before upgrade: %v", err)
	}
	waiting := o.arm(t, "rec-waiting")
	if n := o.snapshotSteps(t, waiting); n != 1 {
		t.Fatalf("waiting request snapshot has %d steps; want 1", n)
	}

	v2 := opsPack(t, "1.9.0", true)
	plan, err := o.st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", v2, "upload")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := map[string]string{
		ItemWorkQueue + "/ticket_khan_dang_mo":        StatusUpdateSafe,
		ItemApproval + "/tk_tickets:Duyệt đóng phiếu": StatusUpdateSafe,
		ItemInboundHook + "/intake-a":                 StatusUpdateSafe,
		ItemInboundHook + "/intake-b":                 StatusAdded,
		ItemTeam + "/Đội mới":                         StatusAdded,
	}
	for _, it := range plan.Items {
		w, ok := want[it.ItemType+"/"+it.ItemKey]
		if !ok {
			continue
		}
		if it.Status != w || it.DefaultAction != ActionApply {
			t.Errorf("%s/%s = %s default=%s; want %s/apply", it.ItemType, it.ItemKey, it.Status, it.DefaultAction, w)
		}
		if it.ItemType == ItemInboundHook && contains(it.Actions, ActionDelete) {
			t.Errorf("%s offers delete; a receiver is never deleted by an upgrade", it.ItemKey)
		}
		if it.ItemKey == "intake-b" {
			found := false
			for _, w := range it.Warnings {
				found = found || w.Code == "NEW_HOOK_SECRET"
			}
			if !found {
				t.Errorf("a new receiver must warn NEW_HOOK_SECRET in the plan: %+v", it.Warnings)
			}
		}
		delete(want, it.ItemType+"/"+it.ItemKey)
	}
	if len(want) != 0 {
		t.Fatalf("plan is missing %v", want)
	}

	run, err := o.st.pack.ApplyUpgrade(ctx, vpTenantA, vpSeedUserID, "ticketing", v2, "upload", plan.PlanHash, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if run.Status != RunDone {
		t.Fatalf("run = %s counts=%v results=%+v", run.Status, run.Counts, run.Results)
	}
	for _, r := range run.Results {
		if r.Outcome == OutcomeFailed {
			t.Errorf("failed item %s/%s: %s %s", r.ItemType, r.ItemKey, r.Code, r.Message)
		}
	}

	// (1) the request that was waiting keeps one step; a new request gets two.
	if n := o.snapshotSteps(t, waiting); n != 1 {
		t.Errorf("waiting request snapshot changed to %d steps; want it frozen at 1", n)
	}
	if n := o.snapshotSteps(t, o.arm(t, "rec-new")); n != 2 {
		t.Errorf("new request snapshot has %d steps; want 2", n)
	}

	// (2) the old receiver still takes a signal signed with the old key; (3) the new one has its own key.
	hooks, err = o.hooks.ListAdmin(ctx, vpTenantA)
	if err != nil {
		t.Fatal(err)
	}
	var newSecret string
	for _, h := range hooks {
		switch h.Name {
		case "intake-a":
			if h.Secret != oldSecret {
				t.Errorf("intake-a key changed by the upgrade")
			}
		case "intake-b":
			newSecret = h.Secret
		}
	}
	if newSecret == "" || newSecret == oldSecret {
		t.Errorf("intake-b key = %q; want a fresh non-empty key different from %q", newSecret, oldSecret)
	}
	if _, err := o.hooks.Receive(ctx, opsSlug, "intake-a", opsSign(oldSecret, body), body); err != nil {
		t.Errorf("signal signed with the OLD key after the upgrade: %v", err)
	}
	for _, r := range run.Results {
		if r.ItemKey == "intake-b" && r.Code != "NEW_HOOK_SECRET" {
			t.Errorf("intake-b result code = %q; want NEW_HOOK_SECRET", r.Code)
		}
	}

	// The queue filter changed and the team exists.
	qs, _ := o.st.pack.workQueueWr.ListAdmin(ctx, vpTenantA)
	for _, q := range qs {
		if q.Name == "ticket_khan_dang_mo" && len(q.Filters) != 1 {
			t.Errorf("queue filters = %+v; want the single new filter", q.Filters)
		}
	}
	ts, _ := o.st.pack.teamWr.ListTeams(ctx, vpTenantA)
	hasTeam := false
	for _, tm := range ts {
		hasTeam = hasTeam || tm.Name == "Đội mới"
	}
	if !hasTeam {
		t.Error("the new team was not created")
	}

	// After applying, the next plan has nothing left to decide for these items.
	plan2, err := o.st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", v2, "upload")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range plan2.Items {
		if it.ItemType == ItemInboundHook || it.ItemType == ItemApproval || it.ItemType == ItemWorkQueue || it.ItemType == ItemTeam {
			if len(it.Actions) > 0 && it.Status != StatusInSync && it.Status != StatusUnchanged {
				t.Errorf("after apply %s/%s = %s actions=%v; want nothing to decide", it.ItemType, it.ItemKey, it.Status, it.Actions)
			}
		}
	}
}

func TestUpgradeOps_QueueTheBuilderSwitchedOffIsUndiffableAndKeepOnly(t *testing.T) {
	o := opsSetup(t)
	ctx := context.Background()
	v1 := opsPack(t, "1.8.0", false)
	if _, err := o.st.pack.Install(ctx, vpTenantA, vpSeedUserID, v1); err != nil {
		t.Fatalf("install: %v", err)
	}
	qs, err := o.st.pack.workQueueWr.ListAdmin(ctx, vpTenantA)
	if err != nil {
		t.Fatal(err)
	}
	off := false
	for _, q := range qs {
		if q.Name != "ticket_khan_dang_mo" {
			continue
		}
		var pq PackWorkQueue
		for _, c := range v1.WorkQueues {
			if c.Name == q.Name {
				pq = c
			}
		}
		in := toWorkQueueInput(pq, "")
		in.IsActive = &off
		u, ok := o.st.pack.workQueueWr.(workQueueUpdater)
		if !ok {
			t.Fatal("work queue writer cannot update")
		}
		if _, err := u.Update(ctx, vpTenantA, q.ID, in); err != nil {
			t.Fatalf("switch off: %v", err)
		}
	}
	plan, err := o.st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", opsPack(t, "1.9.0", true), "upload")
	if err != nil {
		t.Fatal(err)
	}
	it := find(t, plan, ItemWorkQueue, "ticket_khan_dang_mo")
	if it.Status != StatusUndiffable || len(it.Actions) != 1 || it.Actions[0] != ActionKeep {
		t.Fatalf("switched-off queue = %s actions=%v; want undiffable with Keep only", it.Status, it.Actions)
	}
}
