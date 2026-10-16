//go:build integration

// specs/069-pack-apply-upgrade L1 integration (T012, T013, T017a) against real MySQL, over the
// SAME fake itemStore the 011 provenance gate uses (the SQL store lives in packinstall, which
// imports this package).
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=true&multiStatements=true&loc=Local&clientFoundRows=true" \
//	  go test -tags=integration -count=1 -run 'TestUpgradePlan' ./internal/services/vertical_pack/
package vertical_pack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gorm.io/gorm"

	"nexus/platform/internal/services"
)

// sysCounts snapshots the row counts the plan must never move (FR-017).
func sysCounts(t *testing.T, gdb *gorm.DB, tenantID string, store *fakeItemStore) map[string]int64 {
	t.Helper()
	out := map[string]int64{"fake_items": int64(len(store.rows))}
	for _, tbl := range []string{"sys_workflows", "sys_fields", "sys_layouts", "sys_entities", "sys_dashboards", "sys_pack_installs"} {
		var n int64
		if err := gdb.Raw("SELECT COUNT(*) FROM "+tbl+" WHERE tenant_id = ?", tenantID).Scan(&n).Error; err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	return out
}

func equalCounts(t *testing.T, before, after map[string]int64) {
	t.Helper()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("PlanUpgrade wrote: %s %d → %d", k, v, after[k])
		}
	}
}

// T012: every builtin pack, freshly installed, plans as 100% unchanged against ITSELF, with a
// stable planHash and zero writes. This is the anti-"ghost change" bar of the plan (SC-001).
func TestUpgradePlan_FreshInstallAllUnchanged(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildWF(gdb, sdb)
	store := newFakeItemStore()
	svc.WithItemStore(store)
	ctx := context.Background()
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "upg@it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}

	for _, slug := range BuiltinSlugs() {
		t.Run(slug, func(t *testing.T) {
			pack, ok := BuiltinPack(slug)
			if !ok {
				t.Fatalf("builtin %s not found", slug)
			}
			if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack); err != nil {
				t.Fatalf("install %s: %v", slug, err)
			}
			before := sysCounts(t, gdb, vpTenantA, store)
			again, _ := BuiltinPack(slug)
			p1, err := svc.PlanUpgrade(ctx, vpTenantA, slug, again, "builtin")
			if err != nil {
				t.Fatalf("PlanUpgrade: %v", err)
			}
			p2, err := svc.PlanUpgrade(ctx, vpTenantA, slug, again, "builtin")
			if err != nil {
				t.Fatalf("PlanUpgrade (2nd): %v", err)
			}
			if len(p1.Items) == 0 {
				t.Fatalf("plan has no items — the gate would pass vacuously")
			}
			for _, it := range p1.Items {
				if it.Status != StatusUnchanged || len(it.Actions) != 0 || it.Adopt {
					t.Errorf("FRESH %s NOT CLEAN: %s/%s status=%s actions=%v adopt=%v theirsPaths=%v oursPaths=%v",
						slug, it.ItemType, it.ItemKey, it.Status, it.Actions, it.Adopt, it.TheirsPaths, it.OursPaths)
				}
			}
			if p1.PlanHash == "" || p1.PlanHash != p2.PlanHash {
				t.Errorf("planHash not stable: %q vs %q", p1.PlanHash, p2.PlanHash)
			}
			if p1.Summary["writes"] != 0 || p1.Summary["adoptable"] != 0 {
				t.Errorf("summary = %v, want no writes / adoptable", p1.Summary)
			}
			equalCounts(t, before, sysCounts(t, gdb, vpTenantA, store))
		})
	}
}

// T013 (+ #171 shape): a tenant on Ticketing 1.7.0 where the builder edited one SLA rule by hand.
// 1.8.0 adds stampField to the four creation tiers. The three untouched tiers are update_safe at
// exactly actions[0].stampField; the hand-edited one is a conflict; due_date_auto is new; nothing
// is written.
func TestUpgradePlan_Ticketing170To180(t *testing.T) {
	gdb, sdb := vpSetup(t)
	st := vpBuildSLA(gdb, sdb)
	defer st.drain(t)
	slaSeedInstaller(t, gdb)
	store := newFakeItemStore()
	st.pack.WithItemStore(store)
	ctx := context.Background()
	if _, err := services.SeedDefaultBusinessCalendar(ctx, gdb, vpTenantA); err != nil {
		t.Fatalf("seed default business calendar: %v", err)
	}
	if _, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, ticketing170(t)); err != nil {
		t.Fatalf("install ticketing 1.7.0: %v", err)
	}

	// The builder hand-edits "phiếu thường": switches the rule off (a path 1.8.0 does not touch).
	const edited = "Đặt hạn SLA — phiếu thường"
	if err := gdb.Exec("UPDATE sys_workflows SET is_active = 0 WHERE tenant_id = ? AND name = ?", vpTenantA, edited).Error; err != nil {
		t.Fatalf("hand-edit the rule: %v", err)
	}

	before := sysCounts(t, gdb, vpTenantA, store)
	v180, _ := BuiltinPack("ticketing")
	plan, err := st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", v180, "builtin")
	if err != nil {
		t.Fatalf("PlanUpgrade: %v", err)
	}
	if len(plan.FromVersions) != 1 || plan.FromVersions[0] != "1.7.0" || plan.ToVersion != "1.8.0" {
		t.Errorf("versions = %v → %s, want [1.7.0] → 1.8.0", plan.FromVersions, plan.ToVersion)
	}
	for _, name := range []string{"Đặt hạn SLA — phiếu Khẩn", "Đặt hạn SLA — phiếu Cao", "Đặt hạn SLA — phiếu Trung bình"} {
		it := findItem(t, plan, ItemWorkflow, name)
		if it.Status != StatusUpdateSafe || strings.Join(it.TheirsPaths, ",") != "actions[0].stampField" {
			t.Errorf("%s: status=%s theirsPaths=%v, want update_safe at actions[0].stampField", name, it.Status, it.TheirsPaths)
		}
	}
	// The hand-edited tier: pack changed stampField, builder changed isActive ⇒ both changed,
	// no overlap — Cả hai đã đổi.
	it := findItem(t, plan, ItemWorkflow, edited)
	if it.Status != StatusConflict || strings.Join(it.TheirsPaths, ",") != "actions[0].stampField" || len(it.Overlap) != 0 {
		t.Errorf("%s: status=%s theirs=%v ours=%v overlap=%v, want conflict / stampField / no overlap", edited, it.Status, it.TheirsPaths, it.OursPaths, it.Overlap)
	}
	if got := findItem(t, plan, ItemField, "tk_tickets.due_date_auto"); got.Status != StatusAdded {
		t.Errorf("due_date_auto: %s, want added", got.Status)
	}
	added := 0
	for _, w := range plan.Items {
		if w.ItemType == ItemWorkflow && strings.HasPrefix(w.ItemKey, "Tính lại hạn SLA") {
			added++
			if w.Status != StatusAdded {
				t.Errorf("%s: %s, want added", w.ItemKey, w.Status)
			}
		}
	}
	if added != 4 {
		t.Errorf("want 4 'Tính lại hạn SLA' rules added, got %d", added)
	}
	for _, w := range plan.Items {
		// Workflow (L2a) and the schema family (L4) have adapters; every other type still offers only keep until its slice.
		if w.ItemType != ItemWorkflow && w.ItemType != ItemEntity && w.ItemType != ItemField && w.ItemType != ItemDeferredField && w.ItemType != ItemNN && len(w.Actions) > 0 && (len(w.Actions) != 1 || w.Actions[0] != ActionKeep) {
			t.Errorf("%s/%s: no adapter yet, want keep only, got %v", w.ItemType, w.ItemKey, w.Actions)
		}
	}
	equalCounts(t, before, sysCounts(t, gdb, vpTenantA, store))
}

func findItem(t *testing.T, p *UpgradePlan, typ, key string) UpgradeItem {
	t.Helper()
	for _, it := range p.Items {
		if it.ItemType == typ && it.ItemKey == key {
			return it
		}
	}
	t.Fatalf("plan has no %s/%s", typ, key)
	return UpgradeItem{}
}

// T017a: an uploaded file that still carries a webhook signing key + auth header (an old export)
// never has them echoed by the plan, whichever side the rule lands on.
func TestUpgradePlan_UploadedFileSecretsNeverInPayload(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildWF(gdb, sdb)
	store := newFakeItemStore()
	svc.WithItemStore(store)
	ctx := context.Background()
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "sec@it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}
	pack := wfPack()
	pack.Manifest = &Manifest{Slug: "wfsec", Version: "1.0.0"}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack); err != nil {
		t.Fatalf("install: %v", err)
	}
	up := wfPack()
	up.Manifest = &Manifest{Slug: "wfsec", Version: "1.1.0"}
	for i := range up.Workflows {
		up.Workflows[i].Actions = append(up.Workflows[i].Actions, PackWorkflowAction{
			Type: "outbound_webhook", URL: "https://hook.example.com/y", Secret: "SECRET-MARKER-FILE-1",
			Headers: map[string]string{"Authorization": "MARKER-HEADER"},
		})
	}
	plan, err := svc.PlanUpgrade(ctx, vpTenantA, "wfsec", up, "upload")
	if err != nil {
		t.Fatalf("PlanUpgrade: %v", err)
	}
	changed := 0
	for _, it := range plan.Items {
		if it.ItemType == ItemWorkflow && it.Status == StatusUpdateSafe {
			changed++
		}
	}
	if changed == 0 {
		t.Fatalf("fixture must produce changed workflows so their bodies are echoed; items=%d", len(plan.Items))
	}
	b, _ := json.Marshal(plan)
	for _, marker := range []string{"SECRET-MARKER-FILE-1", "MARKER-HEADER"} {
		if strings.Contains(string(b), marker) {
			t.Fatalf("plan payload leaks %q", marker)
		}
	}
}
