package vertical_pack

// Unit tests for specs/069 L7 (upgrade into a delivered project): applyMode is a pure mapping of
// metaversion.ClassOf, a delivered project is applied only by its own builder, an item with a
// draft is draft_pending (Giữ only), a change that went into a draft keeps its baseline, and the
// plan reads the running configuration.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// T104: every item type the upgrade can write joins to a doc type, every doc type of the draft
// registry has a class, and the wire string is exactly the ClassOf mapping.
func TestApplyMode_MapsEveryItemTypeAndMatchesClassOf(t *testing.T) {
	for typ := range typeRank {
		if itemDocType[typ] == "" {
			t.Errorf("item type %q has no docType join (new upgrade type without a class)", typ)
		}
	}
	registry := []string{
		services.NewLayoutDocAdapter().Type(), services.NewWorkflowDocAdapter().Type(), services.NewReportDocAdapter().Type(),
		services.NewDashboardDocAdapter().Type(), services.NewBrandingDocAdapter().Type(), services.NewAppDocAdapter().Type(),
		services.NewFieldDocAdapter().Type(), services.NewEntityDocAdapter().Type(),
	}
	for _, dt := range registry {
		if metaversion.ClassOf(dt).Kind != metaversion.Draft {
			t.Errorf("registry doc type %q is not classified draft in metaversion.ClassOf", dt)
		}
	}
	if got := len(metaversion.TypesOfKind(metaversion.Draft)); got != len(registry) {
		t.Errorf("ClassOf knows %d draft types, registry has %d - they must move together", got, len(registry))
	}

	cases := []struct {
		name   string
		typ    string
		status string
		paths  []string
		want   string
	}{
		{"workflow edit", ItemWorkflow, StatusUpdateSafe, nil, ApplyModeDraft},
		{"workflow new", ItemWorkflow, StatusAdded, nil, ApplyModeDraft},
		{"report", ItemReport, StatusUpdateSafe, nil, ApplyModeDraft},
		{"layout", ItemLayout, StatusUpdateSafe, nil, ApplyModeDraft},
		{"field edit", ItemField, StatusUpdateSafe, []string{"label"}, ApplyModeDraft},
		{"field new is hidden", ItemField, StatusAdded, nil, ApplyModeHidden},
		{"field indexed only", ItemField, StatusUpdateSafe, []string{"options.indexed"}, ApplyModeUntraced},
		{"role", ItemRole, StatusUpdateSafe, nil, ApplyModeTraced},
		{"grant", ItemRoleGrant, StatusUpdateSafe, nil, ApplyModeTraced},
		{"field acl", ItemFieldACL, StatusAdded, nil, ApplyModeTraced},
		{"entity edit", ItemEntity, StatusUpdateSafe, nil, ApplyModeUntraced},
		{"entity new is hidden", ItemEntity, StatusAdded, nil, ApplyModeHidden},
		{"work queue", ItemWorkQueue, StatusAdded, nil, ApplyModeUntraced},
		{"approval", ItemApproval, StatusUpdateSafe, nil, ApplyModeUntraced},
		{"inbound hook", ItemInboundHook, StatusAdded, nil, ApplyModeUntraced},
		{"relation new is hidden", ItemNN, StatusAdded, nil, ApplyModeHidden},
		{"relation edit", ItemNN, StatusUpdateSafe, nil, ApplyModeUntraced},
	}
	for _, c := range cases {
		it := &UpgradeItem{ItemType: c.typ, Status: c.status, TheirsPaths: c.paths}
		if got := applyModeOf(it); got != c.want {
			t.Errorf("%s: applyMode = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNeedsImmediateConfirm_OnlyUntracedWrites(t *testing.T) {
	q := &UpgradeItem{ItemType: ItemWorkQueue, ItemKey: "q", ApplyMode: ApplyModeUntraced}
	r := &UpgradeItem{ItemType: ItemRole, ItemKey: "r", ApplyMode: ApplyModeTraced}
	w := &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "w", ApplyMode: ApplyModeDraft}
	if got := needsImmediateConfirm([]upgradeTodo{{r, ActionApply}, {w, ActionApply}, {q, ActionKeepAlways}, {q, "adopt"}}); len(got) != 0 {
		t.Fatalf("hit = %v; traced / draft / non-write choices need no tick", got)
	}
	if got := needsImmediateConfirm([]upgradeTodo{{q, ActionApply}}); len(got) != 1 {
		t.Fatalf("hit = %v; an untraced apply needs the tick", got)
	}
}

func builderCtx(tenant string) context.Context {
	return metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "u1", TenantID: tenant, HomeTenantID: "home", IsBuilder: true})
}

func deliveredSvc(t *testing.T) (*Service, *upgStore, *upgWF) {
	t.Helper()
	svc, st, wf := newUpgSvc(t)
	svc.WithDeliveryGate(gateFn(func(context.Context, string) (bool, error) { return true, nil }))
	return svc, st, wf
}

func TestApplyUpgrade_Delivered_GateByCaller(t *testing.T) {
	for _, c := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"no caller", context.Background(), "PROJECT_DELIVERED_INSTALL_BLOCKED"},
		{"non-builder", metaversion.WithCaller(context.Background(), metaversion.Caller{TenantID: upgTenant}), "PROJECT_DELIVERED_INSTALL_BLOCKED"},
		{"builder from another tenant (?targetTenant=)", builderCtx("operator-home"), "PACK_UPGRADE_OPEN_WORKSPACE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, st, wf := deliveredSvc(t)
			_, err := svc.ApplyUpgrade(c.ctx, upgTenant, "u1", "p", upgPack("1.1.0", "r"), "upload", "x", nil)
			if vcode(err) != c.want {
				t.Fatalf("err = %v; want %s", err, c.want)
			}
			if st.count("claim") != 0 || len(wf.created) != 0 {
				t.Fatalf("claim=%d created=%d; refused before the lock, 0 writes", st.count("claim"), len(wf.created))
			}
		})
	}
}

func TestApplyUpgrade_Delivered_BuilderGoesToDraftAndKeepsTheBaseline(t *testing.T) {
	svc, st, wf := deliveredSvc(t)
	pack := upgPack("1.1.0", "plain rule")
	ctx := builderCtx(upgTenant)
	plan, err := svc.PlanUpgrade(ctx, upgTenant, "p", pack, "upload")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Delivered || plan.Items[len(plan.Items)-1].ApplyMode != ApplyModeDraft {
		t.Fatalf("plan delivered=%v items=%+v; want delivered with applyMode draft", plan.Delivered, plan.Items)
	}
	run, err := svc.ApplyUpgrade(ctx, upgTenant, "u1", "p", pack, "upload", plan.PlanHash, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if run.Mode != "draft" || run.Status != RunDone || run.Counts[OutcomeDrafted] != 1 || run.Counts[OutcomeApplied] != 0 {
		t.Fatalf("run mode=%s status=%s counts=%v; want a draft run with 1 drafted", run.Mode, run.Status, run.Counts)
	}
	if len(wf.created) != 1 {
		t.Fatalf("created %d, want 1 (the draft routing is the service's job)", len(wf.created))
	}
	if _, ok := st.rows[upgTenant+"|p|workflow|plain rule"]; ok {
		t.Fatal("baseline row written for an item that only entered a draft (FR-105)")
	}
	if st.rows[upgTenant+"|p|workflow|seed"].PackVersion != "1.0.0" {
		t.Fatal("seed baseline bumped")
	}
}

type fakeDrafts struct{ rows []metaversion.DraftRow }

func (f fakeDrafts) ListRead(context.Context, string) ([]metaversion.DraftRow, error) {
	return f.rows, nil
}

func TestPlanUpgrade_Delivered_ItemWithADraftIsDraftPending(t *testing.T) {
	svc, _, _ := deliveredSvc(t)
	pack := upgPack("1.1.0", "plain rule", "other rule")
	before, err := svc.PlanUpgrade(builderCtx(upgTenant), upgTenant, "p", pack, "upload")
	if err != nil {
		t.Fatal(err)
	}
	svc.WithDraftReader(fakeDrafts{rows: []metaversion.DraftRow{{DocType: "workflow", DocKey: "draft-id", Label: "plain rule"}}})
	after, err := svc.PlanUpgrade(builderCtx(upgTenant), upgTenant, "p", pack, "upload")
	if err != nil {
		t.Fatal(err)
	}
	if before.PlanHash == after.PlanHash {
		t.Fatal("a draft appearing did not change the plan hash")
	}
	for _, it := range after.Items {
		switch it.ItemKey {
		case "plain rule":
			if it.Status != StatusDraftPending || len(it.Actions) != 1 || it.Actions[0] != ActionKeep || it.DefaultAction != ActionKeep {
				t.Errorf("plain rule = %s %v default %s; want draft_pending, keep only", it.Status, it.Actions, it.DefaultAction)
			}
		case "other rule":
			if it.Status != StatusAdded {
				t.Errorf("other rule = %s; a draft elsewhere must not touch it", it.Status)
			}
		}
	}
	// Choosing apply on a draft_pending item is refused: the builder's draft is never overwritten.
	_, err = svc.ApplyUpgrade(builderCtx(upgTenant), upgTenant, "u1", "p", pack, "upload", after.PlanHash,
		[]UpgradeDecision{{ItemType: ItemWorkflow, ItemKey: "plain rule", Action: ActionApply}})
	if vcode(err) != "PACK_UPGRADE_DECISION_INVALID" {
		t.Fatalf("err = %v; want PACK_UPGRADE_DECISION_INVALID", err)
	}
}

// liveReadWF records whether the plan listed rules under WithLiveRead (FR-101).
type liveReadWF struct {
	upgWF
	sawLive, sawOverlay bool
}

func (f *liveReadWF) ListWorkflows(ctx context.Context, _ string) ([]workflow.WorkflowRecord, error) {
	if metaversion.IsLiveRead(ctx) {
		f.sawLive = true
	} else {
		f.sawOverlay = true
	}
	return nil, nil
}

func TestPlanUpgrade_ReadsLiveNeverTheOverlay(t *testing.T) {
	st := newUpgStore()
	st.rows[upgTenant+"|p|workflow|seed"] = StoredPackItem{Type: ItemWorkflow, Key: "seed", PackVersion: "1.0.0", Content: json.RawMessage(upgWFJSON("seed", false))}
	wf := &liveReadWF{}
	svc := NewService(Deps{EntityReader: &fakeMintMeta{}, LayoutReader: fakeApLayouts{}, RoleReader: fakeApRoleReader{}, WorkflowWriter: wf}).WithItemStore(st)
	ctx := metaversion.WithCaller(context.Background(), metaversion.Caller{TenantID: upgTenant, IsBuilder: true, ReadDraft: true})
	if _, err := svc.PlanUpgrade(ctx, upgTenant, "p", upgPack("1.1.0", "r"), "upload"); err != nil {
		t.Fatal(err)
	}
	if !wf.sawLive || wf.sawOverlay {
		t.Fatalf("sawLive=%v sawOverlay=%v; every read of the plan must be live", wf.sawLive, wf.sawOverlay)
	}
	if strings.Contains(string(upgWFJSON("seed", false)), "draft") {
		t.Fatal("fixture drift")
	}
}

// T107 (unit): a queue change reaches the customer at once with no trace, so it needs the tick.
func TestApplyUpgrade_Delivered_UntracedChangeNeedsTheTick(t *testing.T) {
	svc, wq, _, _, _ := newOpsSvc(t)
	st := newUpgStore()
	st.rows[upgTenant+"|p|workflow|seed"] = StoredPackItem{Type: ItemWorkflow, Key: "seed", PackVersion: "1.0.0", Content: upgWFJSON("seed", false)}
	svc.WithItemStore(st).WithDeliveryGate(gateFn(func(context.Context, string) (bool, error) { return true, nil }))
	pack := &Pack{Manifest: &Manifest{Slug: "p", Version: "1.1.0"},
		WorkQueues: []PackWorkQueue{{Name: "today", Label: "Hôm nay", EntityName: "e", RoleName: "QA"}}}
	ctx := builderCtx(upgTenant)
	plan, err := svc.PlanUpgrade(ctx, upgTenant, "p", pack, "upload")
	if err != nil {
		t.Fatal(err)
	}
	var q *UpgradeItem
	for i := range plan.Items {
		if plan.Items[i].ItemType == ItemWorkQueue {
			q = &plan.Items[i]
		}
	}
	if q == nil || q.ApplyMode != ApplyModeUntraced || q.DefaultAction != ActionApply {
		t.Fatalf("queue item = %+v; want immediate_untraced pre-selected", q)
	}
	_, err = svc.ApplyUpgrade(ctx, upgTenant, "u1", "p", pack, "upload", plan.PlanHash, nil)
	if vcode(err) != "PACK_UPGRADE_IMMEDIATE_UNCONFIRMED" {
		t.Fatalf("err = %v; want PACK_UPGRADE_IMMEDIATE_UNCONFIRMED", err)
	}
	if len(wq.created) != 0 || st.running != nil || st.count("begin") != 0 {
		t.Fatalf("created=%d running=%v begin=%d; an unconfirmed apply writes nothing and holds no lock", len(wq.created), st.running, st.count("begin"))
	}
	run, err := svc.ApplyUpgrade(WithConfirmImmediate(ctx), upgTenant, "u1", "p", pack, "upload", plan.PlanHash, nil)
	if err != nil {
		t.Fatalf("confirmed apply: %v", err)
	}
	if len(wq.created) != 1 || run.Counts[OutcomeApplied] != 1 || run.Counts[OutcomeDrafted] != 0 {
		t.Fatalf("created=%d counts=%v; want the queue applied at once", len(wq.created), run.Counts)
	}
	if _, ok := st.rows[upgTenant+"|p|work_queue|today"]; !ok {
		t.Fatal("an applied-at-once item must get its baseline row")
	}
}
