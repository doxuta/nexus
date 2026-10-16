package vertical_pack

// Branch tests for ApplyUpgrade (specs/069 L2a review): the destructive and safety paths the
// happy-path file does not reach — delete, restore (recreate + in-place), adopt, run status,
// the install ledger, the run deadline and the keyed-webhook default.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func branchEnv(svc *Service, theirs map[string]json.RawMessage) *applyEnv {
	return newApplyEnv(svc, upgTenant, "u1", "p", "1.1.0", &planArtifacts{theirs: theirs})
}

func TestApplyItem_DeleteDropsRuleAndBaseline(t *testing.T) {
	svc, st, wf := newUpgSvc(t)
	env := branchEnv(svc, nil)
	it := &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "gone", LiveID: "wf-9", Status: StatusRemovedInPack}
	res := UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, it, ActionDelete, &res)
	if res.Outcome != OutcomeDeleted || len(wf.deleted) != 1 || wf.deleted[0] != "wf-9" {
		t.Fatalf("res=%+v deleted=%v; want deleted wf-9", res, wf.deleted)
	}
	if len(st.deleted) != 1 || st.deleted[0] != [2]string{ItemWorkflow, "gone"} {
		t.Fatalf("baseline drop = %v; want workflow/gone", st.deleted)
	}
	// Without a live id nothing may be deleted.
	res = UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "x"}, ActionDelete, &res)
	if res.Outcome != OutcomeFailed || res.Code != "UPGRADE_LIVE_ID_UNKNOWN" || len(wf.deleted) != 1 {
		t.Fatalf("res=%+v deleted=%v; want failed UPGRADE_LIVE_ID_UNKNOWN and no extra delete", res, wf.deleted)
	}
}

func TestApplyItem_RestoreRecreatesOrUpdatesInPlace(t *testing.T) {
	for _, tc := range []struct {
		name, status, liveID string
		wantCreated, wantUpd int
	}{
		{"deleted by builder → recreated", StatusDeletedByBuilder, "", 1, 0},
		{"kept custom → updated in place", StatusKeptCustom, "wf-7", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, wf := newUpgSvc(t)
			env := branchEnv(svc, map[string]json.RawMessage{ItemWorkflow + "/r": upgWFJSON("r", false)})
			it := &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "r", Status: tc.status, LiveID: tc.liveID}
			res := UpgradeResult{}
			svc.applyItem(context.Background(), context.Background(), env, it, ActionRestore, &res)
			if res.Outcome != OutcomeRestored || len(wf.created) != tc.wantCreated || len(wf.updated) != tc.wantUpd {
				t.Fatalf("res=%+v created=%d updated=%d", res, len(wf.created), len(wf.updated))
			}
			if tc.wantUpd == 1 && wf.updated[0].IntervalSeconds != 600 {
				t.Errorf("IntervalSeconds = %d; the live cadence (600) must be carried over", wf.updated[0].IntervalSeconds)
			}
			if row := st.rows[upgTenant+"|p|workflow|r"]; row.PackVersion != "1.1.0" || row.LiveID == "" {
				t.Errorf("baseline = %+v; want 1.1.0 with a live id", row)
			}
		})
	}
}

func TestAdoptItem_RecordsBaselineOrDropsIt(t *testing.T) {
	svc, st, _ := newUpgSvc(t)
	env := branchEnv(svc, map[string]json.RawMessage{ItemWorkflow + "/kept": upgWFJSON("kept", false)})
	res := UpgradeResult{}
	svc.adoptItem(context.Background(), env, &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "kept", LiveID: "wf-3"}, &res)
	if res.Outcome != OutcomeAdopted || st.rows[upgTenant+"|p|workflow|kept"].LiveID != "wf-3" {
		t.Fatalf("res=%+v row=%+v; want adopted with live id", res, st.rows[upgTenant+"|p|workflow|kept"])
	}
	res = UpgradeResult{}
	svc.adoptItem(context.Background(), env, &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "both-dropped"}, &res)
	if res.Outcome != OutcomeAdopted || len(st.deleted) != 1 || st.deleted[0][1] != "both-dropped" {
		t.Fatalf("res=%+v deleted=%v; both sides dropped it → baseline row removed", res, st.deleted)
	}
}

func TestApplyUpgrade_RunStatusAndLedger(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fail       []string
		wantStatus string
		wantLedger int
	}{
		{"all ok → done, ledger row", nil, RunDone, 1},
		{"one fails → partial, no ledger", []string{"rule b"}, RunPartial, 0},
		{"all fail → failed, no ledger", []string{"rule a", "rule b"}, RunFailed, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, wf := newUpgSvc(t)
			led := &ledgerFake{}
			svc.installs = led
			wf.failFor = map[string]bool{}
			for _, n := range tc.fail {
				wf.failFor[n] = true
			}
			pack := upgPack("1.1.0", "rule a", "rule b")
			run, err := svc.ApplyUpgrade(context.Background(), upgTenant, "u1", "p", pack, "upload", planHashOf(t, svc, pack), nil)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if run.Status != tc.wantStatus || len(led.recs) != tc.wantLedger {
				t.Fatalf("status=%s ledger=%d; want %s, %d (counts %v)", run.Status, len(led.recs), tc.wantStatus, tc.wantLedger, run.Counts)
			}
			if tc.wantLedger == 1 && led.recs[0].Result["kind"] != "upgrade" {
				t.Errorf("ledger kind = %v; want upgrade", led.recs[0].Result["kind"])
			}
		})
	}
}

func TestApplyUpgrade_DeadlineMarksRestNotRun(t *testing.T) {
	old := upgradeRunBudget
	upgradeRunBudget = 30 * time.Millisecond
	defer func() { upgradeRunBudget = old }()
	svc, _, wf := newUpgSvc(t)
	wf.onMake = func() { time.Sleep(80 * time.Millisecond) }
	pack := upgPack("1.1.0", "rule a", "rule b")
	run, err := svc.ApplyUpgrade(context.Background(), upgTenant, "u1", "p", pack, "upload", planHashOf(t, svc, pack), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	notRun := 0
	for _, r := range run.Results {
		if r.Outcome == OutcomeNotRun && r.Code == "UPGRADE_DEADLINE" {
			notRun++
		}
	}
	if run.Status != RunPartial || notRun != 1 || run.Counts[OutcomeApplied] != 1 {
		t.Fatalf("status=%s notRun=%d counts=%v; want partial, 1 not_run, 1 applied", run.Status, notRun, run.Counts)
	}
}

// A live rule that still holds its own webhook signing key must NOT be pre-selected for Apply:
// the save cannot carry the key over, so it would silently break the hook.
func TestPlan_KeyedWebhookIsNotPreselected(t *testing.T) {
	hook := func(v string) json.RawMessage {
		return json.RawMessage(`{"name":"h","entityName":"e","trigger":"after_create","actions":[{"type":"outbound_webhook","url":"https://x.test/` + v + `"}]}`)
	}
	for _, tc := range []struct {
		keyed    bool
		wantDef  string
		wantWarn bool
	}{{true, ActionKeep, true}, {false, ActionApply, false}} {
		f := newFixture()
		k := ItemWorkflow + "/h"
		f.in.base[k] = StoredPackItem{Type: ItemWorkflow, Key: "h", PackVersion: "1.0.0", Content: hook("a"), LiveID: "id-h"}
		f.in.theirs[k] = hook("b")
		f.in.live[k] = hook("a")
		f.in.idx.add(ItemWorkflow, "h", "id-h")
		f.in.idx.keyedHook["id-h"] = tc.keyed
		f.in.ready = func(string) bool { return true }
		it := find(t, f.plan(t), ItemWorkflow, "h")
		if it.Status != StatusUpdateSafe || it.DefaultAction != tc.wantDef || hasConfirm(it.Warnings) != tc.wantWarn {
			t.Errorf("keyed=%v: status=%s default=%s warnings=%+v; want update_safe/%s warn=%v", tc.keyed, it.Status, it.DefaultAction, it.Warnings, tc.wantDef, tc.wantWarn)
		}
	}
}

func TestApplyUpgrade_OverLongInputIs422BeforeTheLock(t *testing.T) {
	svc, st, _ := newUpgSvc(t)
	_, err := svc.ApplyUpgrade(context.Background(), upgTenant, "u1", "p", upgPack("1.1.0", "r"), "upload", strings.Repeat("a", 65), nil)
	if vcode(err) != "PACK_UPGRADE_INPUT_INVALID" || st.count("claim") != 0 {
		t.Fatalf("err=%v claim=%d; want PACK_UPGRADE_INPUT_INVALID with no lock taken", err, st.count("claim"))
	}
}
