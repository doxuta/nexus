//go:build integration

// DAILY-QUEUE #430 / specs/069 L2a, the #171 scenario end to end against real MySQL and the real
// workflow service: a tenant on Ticketing 1.7.0 "re-installed" 1.8.0 (the four SLA rules were
// skipped by name and still lack stampField). ApplyUpgrade brings the untouched ones to 1.8.0,
// leaves the one the builder switched off alone, records the baselines, and the new behaviour is
// live (a ticket is stamped, a priority change tightens the deadline).
//
//	TEST_DATABASE_URL=... bash scripts/run-integration.sh ./internal/services/vertical_pack/ -run UpgradeApply
package vertical_pack

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/services"
)

func TestUpgradeApply_Ticketing171(t *testing.T) {
	gdb, sdb := vpSetup(t)
	st := vpBuildSLA(gdb, sdb)
	defer st.drain(t)
	slaSeedInstaller(t, gdb)
	store := newUpgStore()
	st.pack.WithItemStore(store)
	ctx := context.Background()
	if _, err := services.SeedDefaultBusinessCalendar(ctx, gdb, vpTenantA); err != nil {
		t.Fatalf("seed default business calendar: %v", err)
	}
	if _, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, ticketing170(t)); err != nil {
		t.Fatalf("install 1.7.0: %v", err)
	}
	v180, _ := BuiltinPack("ticketing")
	if _, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, v180); err != nil { // "Cài lại" — what northwind did
		t.Fatalf("re-install 1.8.0: %v", err)
	}
	const normal = "Đặt hạn SLA — phiếu thường"
	if err := gdb.Exec("UPDATE sys_workflows SET is_active = 0 WHERE tenant_id = ? AND name = ?", vpTenantA, normal).Error; err != nil {
		t.Fatalf("hand-edit the rule: %v", err)
	}

	plan, err := st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", v180, "builtin")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	safe := 0
	for _, it := range plan.Items {
		switch {
		case it.ItemType == ItemWorkflow && it.ItemKey == normal:
			if it.Status != StatusConflict || it.DefaultAction != ActionKeep || !contains(it.Actions, ActionApply) {
				t.Errorf("%q = %s default=%s actions=%v; want conflict, keep by default, apply offered", normal, it.Status, it.DefaultAction, it.Actions)
			}
		case it.ItemType == ItemWorkflow && strings.HasPrefix(it.ItemKey, "Đặt hạn SLA"):
			safe++
			if it.Status != StatusUpdateSafe || it.DefaultAction != ActionApply {
				t.Errorf("%q = %s default=%s; want update_safe pre-selected", it.ItemKey, it.Status, it.DefaultAction)
			}
		}
	}
	if safe != 3 {
		t.Fatalf("update_safe SLA rules = %d, want 3", safe)
	}

	run, err := st.pack.ApplyUpgrade(ctx, vpTenantA, vpSeedUserID, "ticketing", v180, "builtin", plan.PlanHash, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if run.Status != RunDone || run.Counts[OutcomeApplied] != 3 {
		t.Fatalf("run = %s counts=%v results=%+v; want done, 3 applied", run.Status, run.Counts, run.Results)
	}

	// Baselines moved: the three are unchanged@1.8.0 now, the hand-edited one still differs.
	rep, err := st.pack.DiffInstalled(ctx, vpTenantA, "ticketing")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	for _, it := range rep.Items {
		if it.ItemType != ItemWorkflow || !strings.HasPrefix(it.ItemKey, "Đặt hạn SLA") {
			continue
		}
		want := "unchanged"
		if it.ItemKey == normal {
			want = "modified"
		}
		if it.Status != want {
			t.Errorf("diff %q = %s, want %s", it.ItemKey, it.Status, want)
		}
	}
	again, err := st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", v180, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	if again.Summary[StatusUpdateSafe] != 0 {
		t.Errorf("after the apply, update_safe = %d; want 0", again.Summary[StatusUpdateSafe])
	}
	// The same request again is refused (the plan moved), never applied twice.
	if _, err := st.pack.ApplyUpgrade(ctx, vpTenantA, vpSeedUserID, "ticketing", v180, "builtin", plan.PlanHash, nil); vcode(err) != "PACK_UPGRADE_PLAN_STALE" {
		t.Errorf("repeat of the same request: %v; want PACK_UPGRADE_PLAN_STALE", err)
	}
	var dup int
	gdb.Raw("SELECT COUNT(*) FROM sys_workflows WHERE tenant_id = ? AND name LIKE 'Đặt hạn SLA%' AND deleted_at IS NULL", vpTenantA).Row().Scan(&dup)
	if dup != 4 {
		t.Errorf("SLA rules = %d, want 4 (no duplicate)", dup)
	}

	// The behaviour is live: a Low ticket is stamped, raising it to Urgent tightens the deadline.
	st.restart(t)
	admin := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}
	id := st.raise(t, admin, "Medium") // "Low" belongs to the rule the builder switched off, left alone on purpose
	before, _, _ := st.ticketDeadline(t, admin, id)
	st.edit(t, admin, id, map[string]any{"priority": "Urgent"})
	after, stamp, _ := st.ticketDeadline(t, admin, id)
	if !after.Before(before) || !stamp.Equal(after) {
		t.Fatalf("Low→Urgent: due %s → %s (stamp %s); want an earlier deadline carried by the stamp", before, after, stamp)
	}
}

// L6 (specs/069 T098): the builder switched one SLA rule off (isActive), the pack added stampField
// to its action. "Gộp" keeps BOTH: the rule stays off and carries the new stampField; the merge is
// the new baseline (the next plan has nothing left to decide for it).
func TestUpgradeApply_MergeKeepsBuilderEditAndPackChange(t *testing.T) {
	gdb, sdb := vpSetup(t)
	st := vpBuildSLA(gdb, sdb)
	defer st.drain(t)
	slaSeedInstaller(t, gdb)
	store := newUpgStore()
	st.pack.WithItemStore(store)
	ctx := context.Background()
	if _, err := services.SeedDefaultBusinessCalendar(ctx, gdb, vpTenantA); err != nil {
		t.Fatalf("seed default business calendar: %v", err)
	}
	if _, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, ticketing170(t)); err != nil {
		t.Fatalf("install 1.7.0: %v", err)
	}
	v180, _ := BuiltinPack("ticketing")
	if _, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, v180); err != nil {
		t.Fatalf("re-install 1.8.0: %v", err)
	}
	const normal = "Đặt hạn SLA — phiếu thường"
	if err := gdb.Exec("UPDATE sys_workflows SET is_active = 0 WHERE tenant_id = ? AND name = ?", vpTenantA, normal).Error; err != nil {
		t.Fatalf("hand-edit the rule: %v", err)
	}
	plan, err := st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", v180, "builtin")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	it := findItem(t, plan, ItemWorkflow, normal)
	if !contains(it.Actions, ActionMerge) || it.DefaultAction != ActionKeep || len(it.Merged) == 0 {
		t.Fatalf("%q: actions=%v default=%s merged=%d bytes; want merge offered, keep by default", normal, it.Actions, it.DefaultAction, len(it.Merged))
	}
	run, err := st.pack.ApplyUpgrade(ctx, vpTenantA, vpSeedUserID, "ticketing", v180, "builtin", plan.PlanHash,
		[]UpgradeDecision{{ItemType: ItemWorkflow, ItemKey: normal, Action: ActionMerge}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if run.Counts[OutcomeMerged] != 1 || run.Status != RunDone {
		t.Fatalf("run = %s counts=%v results=%+v; want done with 1 merged", run.Status, run.Counts, run.Results)
	}
	var active bool
	var actions string
	row := gdb.Raw("SELECT is_active, CAST(actions AS CHAR) FROM sys_workflows WHERE tenant_id = ? AND name = ? AND deleted_at IS NULL", vpTenantA, normal).Row()
	if err := row.Scan(&active, &actions); err != nil {
		t.Fatalf("read rule: %v", err)
	}
	if active || !strings.Contains(actions, "stampField") {
		t.Fatalf("after merge: is_active=%v actions=%s; want builder's off kept AND the pack's stampField", active, actions)
	}
	again, err := st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", v180, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	if got := findItem(t, again, ItemWorkflow, normal); got.Status != StatusKeptCustom || contains(got.Actions, ActionMerge) {
		t.Errorf("after merge %q = %s actions=%v; want kept_custom (baseline = the merge), no merge left", normal, got.Status, got.Actions)
	}
}
