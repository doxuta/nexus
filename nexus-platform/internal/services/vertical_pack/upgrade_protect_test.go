package vertical_pack

// "Luôn giữ" (specs/069 L2b, FR-024): the plan offers keep_always only where an overwrite is on
// offer and the item owns a baseline row; applying it flags the row without touching the live
// config; taking the pack's version afterwards clears the flag.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// protStore adds the optional SetProtected capability to the in-memory item store.
type protStore struct {
	*upgStore
	prot map[string]bool
	err  error
}

func (p *protStore) SetProtected(_ context.Context, tenantID, slug, typ, key string, v bool) error {
	if p.err != nil {
		return p.err
	}
	if _, ok := p.upgStore.rows[tenantID+"|"+slug+"|"+typ+"|"+key]; !ok {
		return fmt.Errorf("no baseline row %s/%s", typ, key)
	}
	p.prot[typ+"/"+key] = v
	k := tenantID + "|" + slug + "|" + typ + "|" + key
	row := p.upgStore.rows[k]
	row.Protected = v
	p.upgStore.rows[k] = row
	return nil
}

// SaveItems keeps the flag across an upsert, like the real ON DUPLICATE KEY UPDATE (it never lists protected).
func (p *protStore) SaveItems(ctx context.Context, tenantID, slug, version string, items []PackItem) error {
	keep := map[string]bool{}
	for _, it := range items {
		k := tenantID + "|" + slug + "|" + it.Type + "|" + it.Key
		keep[k] = p.upgStore.rows[k].Protected
	}
	if err := p.upgStore.SaveItems(ctx, tenantID, slug, version, items); err != nil {
		return err
	}
	for k, v := range keep {
		row := p.upgStore.rows[k]
		row.Protected = v
		p.upgStore.rows[k] = row
	}
	return nil
}

func TestPlan_KeepAlwaysOfferedOnlyWhereOverwriteIsOnOffer(t *testing.T) {
	f := newFixture()
	f.in.ready = func(string) bool { return true }
	cases := map[string]struct {
		s    side
		want bool
	}{
		"update_safe":      {side{"a", "b", "a"}, true},
		"kept_custom":      {side{"a", "a", "b"}, true},
		"conflict":         {side{"a", "b", "c"}, true},
		"conflict_no_base": {side{"", "a", "b"}, true},
		"conflict_deleted": {side{"a", "b", ""}, true},
		"added":            {side{"", "a", ""}, false},
		"removed_in_pack":  {side{"a", "", "a"}, false},
		"deleted_builder":  {side{"a", "a", ""}, false},
	}
	for name, c := range cases {
		f.workflow(name, c.s)
	}
	p := f.plan(t)
	for name, c := range cases {
		it := find(t, p, ItemWorkflow, name)
		if got := contains(it.Actions, ActionKeepAlways); got != c.want {
			t.Errorf("%s (%s): keep_always offered=%v want %v (actions %v)", name, it.Status, got, c.want, it.Actions)
		}
		if it.DefaultAction == ActionKeepAlways {
			t.Errorf("%s: keep_always must never be pre-selected", name)
		}
	}
}

func TestPlan_KeepAlwaysNeverForSubItemsAndAdapterlessTypes(t *testing.T) {
	f := newFixture()
	f.in.ready = func(string) bool { return true }
	// field / role_grant have no baseline row of their own.
	for _, typ := range []string{ItemField, ItemRoleGrant, ItemRole} {
		if keepAlwaysTypes(typ) {
			t.Errorf("%s must not offer keep_always", typ)
		}
	}
	f.workflow("w", side{"a", "b", "c"})
	f.in.ready = func(string) bool { return false } // no adapter ⇒ nothing to overwrite ⇒ nothing to protect
	if it := find(t, f.plan(t), ItemWorkflow, "w"); contains(it.Actions, ActionKeepAlways) {
		t.Errorf("adapterless: %v", it.Actions)
	}
}

func TestPlan_ProtectedRowOffersApplyButDefaultsToKeep(t *testing.T) {
	f := newFixture()
	f.in.ready = func(string) bool { return true }
	f.workflow("prot", side{"a", "b", "a"})
	row := f.in.base[ItemWorkflow+"/prot"]
	row.Protected = true
	f.in.base[ItemWorkflow+"/prot"] = row
	it := find(t, f.plan(t), ItemWorkflow, "prot")
	if it.Status != StatusProtected || it.DefaultAction != ActionKeep || !contains(it.Actions, ActionApply) || contains(it.Actions, ActionKeepAlways) {
		t.Fatalf("protected row: %+v", it)
	}
}

func newProtSvc(t *testing.T) (*Service, *protStore, *upgWF) {
	t.Helper()
	svc, st, wf := newUpgSvc(t)
	ps := &protStore{upgStore: st, prot: map[string]bool{}}
	svc.WithItemStore(ps)
	return svc, ps, wf
}

func TestApplyItem_KeepAlwaysFlagsBaselineAndWritesNothingLive(t *testing.T) {
	svc, ps, wf := newProtSvc(t)
	env := branchEnv(svc, nil)
	key := upgTenant + "|p|workflow|seed"
	before := ps.rows[key]
	it := &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "seed", Status: StatusKeptCustom, Base: upgWFJSON("seed", false)}
	res := UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, it, ActionKeepAlways, &res)
	if res.Outcome != OutcomeProtected || !ps.prot["workflow/seed"] {
		t.Fatalf("res=%+v prot=%v", res, ps.prot)
	}
	if len(wf.created)+len(wf.updated)+len(wf.deleted) != 0 {
		t.Fatalf("keep_always touched the live config: %+v", wf)
	}
	if string(ps.rows[key].Content) != string(before.Content) || ps.rows[key].PackVersion != before.PackVersion {
		t.Errorf("baseline content/version changed: %+v", ps.rows[key])
	}
}

func TestApplyItem_KeepAlwaysWithoutBaselineInsertsThePackVersion(t *testing.T) {
	svc, ps, _ := newProtSvc(t)
	theirs := upgWFJSON("fresh", false)
	env := branchEnv(svc, map[string]json.RawMessage{ItemWorkflow + "/fresh": theirs})
	it := &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "fresh", Status: StatusConflictNoBase, LiveID: "wf-5"}
	res := UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, it, ActionKeepAlways, &res)
	row := ps.rows[upgTenant+"|p|workflow|fresh"]
	if res.Outcome != OutcomeProtected || row.PackVersion != "1.1.0" || row.LiveID != "wf-5" || !ps.prot["workflow/fresh"] {
		t.Fatalf("res=%+v row=%+v prot=%v", res, row, ps.prot)
	}
}

func TestApplyItem_ApplyingAProtectedItemClearsTheFlag(t *testing.T) {
	svc, ps, wf := newProtSvc(t)
	ps.prot["workflow/r"] = true
	ps.rows[upgTenant+"|p|workflow|r"] = StoredPackItem{Type: ItemWorkflow, Key: "r", PackVersion: "1.0.0", Content: upgWFJSON("r", false), LiveID: "wf-7", Protected: true}
	env := branchEnv(svc, map[string]json.RawMessage{ItemWorkflow + "/r": upgWFJSON("r", false)})
	it := &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "r", Status: StatusProtected, LiveID: "wf-7", Live: upgWFJSON("r", false)}
	res := UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, it, ActionApply, &res)
	if res.Outcome != OutcomeApplied || len(wf.updated) != 1 || ps.prot["workflow/r"] {
		t.Fatalf("res=%+v updated=%d prot=%v; want applied in place and flag cleared", res, len(wf.updated), ps.prot)
	}
}

func TestApplyItem_KeepAlwaysFailsCleanlyWithoutCapabilityOrOnError(t *testing.T) {
	svc, st, _ := newUpgSvc(t) // plain store: no SetProtected
	_ = st
	env := branchEnv(svc, nil)
	res := UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "seed", Base: upgWFJSON("seed", false)}, ActionKeepAlways, &res)
	if res.Outcome != OutcomeFailed || res.Code != "ADAPTER_NOT_READY" {
		t.Errorf("no capability: %+v", res)
	}
	svc2, ps, _ := newProtSvc(t)
	ps.err = fmt.Errorf("db down")
	res = UpgradeResult{}
	svc2.applyItem(context.Background(), context.Background(), branchEnv(svc2, nil), &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "seed", Base: upgWFJSON("seed", false)}, ActionKeepAlways, &res)
	if res.Outcome != OutcomeFailed || res.Code != "UPGRADE_ITEM_FAILED" {
		t.Errorf("store error: %+v", res)
	}
}
