//go:build integration

// KHUON-SONG-P1 integration gate (../specs/011-pack-provenance/spec.md §6): install the REAL builtin CRM pack
// into a real MySQL tenant, then run the provenance diff.
//
//  1. THE gate: a fresh builtin install must diff 100% UNCHANGED — this is the anti-false-positive
//     bar the adversarial vet set (key presence / array order artifacts must not read as edits).
//  2. A real edit (workflow toggled inactive) reads as modified with the precise path.
//  3. A deleted dashboard reads as missing.
//  4. Re-install keeps the skipped items' ancestor rows (insert-if-absent semantics).
//
// The itemStore here is an in-memory fake implementing the EXACT §4.1 write semantics — the SQL
// implementation lives in packinstall (which imports this package, so it cannot be used here
// without a cycle); its ON DUPLICATE KEY/INSERT IGNORE statements mirror the same contract.
package vertical_pack

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeItemStore implements itemStore with §4.1 semantics: Wrote → upsert; !Wrote → insert-if-absent.
type fakeItemStore struct {
	rows map[string]StoredPackItem // key: tenant|slug|type|key
}

func newFakeItemStore() *fakeItemStore { return &fakeItemStore{rows: map[string]StoredPackItem{}} }

func (f *fakeItemStore) SaveItems(_ context.Context, tenantID, slug, version string, items []PackItem) error {
	for _, it := range items {
		k := tenantID + "|" + slug + "|" + it.Type + "|" + it.Key
		if _, exists := f.rows[k]; exists && !it.Wrote {
			continue // insert-if-absent: never overwrite the recorded ancestor
		}
		live := it.LiveID
		if prev, ok := f.rows[k]; ok && live == "" {
			live = prev.LiveID // SQL parity: COALESCE(VALUES(live_id), live_id)
		}
		f.rows[k] = StoredPackItem{Type: it.Type, Key: it.Key, PackVersion: version, Content: append(json.RawMessage(nil), it.Content...), LiveID: live}
	}
	return nil
}

func (f *fakeItemStore) ListItems(_ context.Context, tenantID, slug string) ([]StoredPackItem, error) {
	var out []StoredPackItem
	prefix := tenantID + "|" + slug + "|"
	for k, v := range f.rows {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, v)
		}
	}
	return out, nil
}

func TestProvenance_FreshBuiltinInstallDiffsUnchanged(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildWF(gdb, sdb)
	store := newFakeItemStore()
	svc.WithItemStore(store)
	ctx := context.Background()

	// Seed the install user (teams pass adds it as first member — see crm_automation test).
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "prov@it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("install builtin crm: %v", err)
	}
	for _, w := range res.Warnings {
		t.Logf("install warning: %s", w)
	}
	if len(store.rows) == 0 {
		t.Fatal("no provenance rows recorded")
	}

	// ── THE GATE: fresh install → every tracked item unchanged. ──────────────────────────────
	report, err := svc.DiffInstalled(ctx, vpTenantA, "crm")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if report.Untracked {
		t.Fatal("report says untracked despite provenance rows")
	}
	for _, it := range report.Items {
		if it.Status != "unchanged" {
			t.Errorf("FRESH INSTALL NOT CLEAN: %s/%s status=%s changed=%v", it.ItemType, it.ItemKey, it.Status, it.ChangedPaths)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// ── A real edit: toggle one pack workflow inactive → modified at isActive. ───────────────
	if err := gdb.Exec(
		"UPDATE sys_workflows SET is_active = 0 WHERE tenant_id = ? AND name = ?",
		vpTenantA, "Định tuyến lead mới",
	).Error; err != nil {
		t.Fatalf("toggle workflow: %v", err)
	}
	report, err = svc.DiffInstalled(ctx, vpTenantA, "crm")
	if err != nil {
		t.Fatalf("diff after edit: %v", err)
	}
	foundEdit := false
	for _, it := range report.Items {
		if it.ItemType == ItemWorkflow && it.ItemKey == "Định tuyến lead mới" {
			foundEdit = true
			if it.Status != "modified" {
				t.Fatalf("edited workflow status = %s, want modified (paths %v)", it.Status, it.ChangedPaths)
			}
			hasPath := false
			for _, p := range it.ChangedPaths {
				if p == "isActive" {
					hasPath = true
				}
			}
			if !hasPath {
				t.Fatalf("edited workflow changedPaths = %v, want to include isActive", it.ChangedPaths)
			}
		}
	}
	if !foundEdit {
		t.Fatal("edited workflow not present in report")
	}

	// ── A deleted dashboard → missing. ───────────────────────────────────────────────────────
	if err := gdb.Exec(
		"UPDATE sys_dashboards SET deleted_at = NOW() WHERE tenant_id = ? AND name = ?",
		vpTenantA, "CRM Overview",
	).Error; err != nil {
		t.Fatalf("delete dashboard: %v", err)
	}
	report, err = svc.DiffInstalled(ctx, vpTenantA, "crm")
	if err != nil {
		t.Fatalf("diff after delete: %v", err)
	}
	foundMissing := false
	for _, it := range report.Items {
		if it.ItemType == ItemDashboard && it.ItemKey == "CRM Overview" {
			foundMissing = true
			if it.Status != "missing" {
				t.Fatalf("deleted dashboard status = %s, want missing", it.Status)
			}
		}
	}
	if !foundMissing {
		t.Fatal("deleted dashboard not present in report")
	}

	// ── Re-install: ancestor rows survive (insert-if-absent for skipped items). ──────────────
	before := make(map[string]string, len(store.rows))
	for k, v := range store.rows {
		before[k] = string(v.Content)
	}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	for k, v := range before {
		after, ok := store.rows[k]
		if !ok {
			t.Fatalf("re-install dropped provenance row %s", k)
		}
		// Same pack version re-installed → content identical either way; the invariant that
		// matters is the row survived and was not corrupted.
		if string(after.Content) != v {
			t.Fatalf("re-install rewrote ancestor content for %s", k)
		}
	}

	// ── Unknown slug → ErrInstallNotFound. ───────────────────────────────────────────────────
	if _, err := svc.DiffInstalled(ctx, vpTenantA, "khong-ton-tai"); err == nil {
		t.Fatal("unknown slug should error")
	}
}

// TestProvenance_ApprovalFreshInstallDiffsUnchanged — the specs/011 retro-fit gap: PACK-APPROVALS
// (install Pass 8d) shipped INVISIBLE to provenance and the diff report. The fresh-install-100%-
// unchanged gate now covers approvals end-to-end against the REAL ApprovalService + MySQL:
//
//  1. a pack approval lands in sys_pack_items and diffs UNCHANGED right after install — including
//     the write-path defaults the pack omitted (isActive→true, criteria.match→"and");
//  2. a real edit (process toggled inactive) reads as modified at exactly "isActive";
//  3. a deleted process reads as missing (existence-probe over ListProcesses, not export absence).
func TestProvenance_ApprovalFreshInstallDiffsUnchanged(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _, _ := vpBuildApprovals(gdb, sdb)
	store := newFakeItemStore()
	svc.WithItemStore(store)
	ctx := context.Background()

	pack := &Pack{
		Manifest: &Manifest{Name: "AP Test", Slug: "aptest", Version: "1.0.0"},
		Entities: []PackEntity{
			{Name: "ap_orders", Label: "Đơn hàng", Fields: []PackField{
				{Name: "code", Label: "Mã", FieldType: "text"},
				{Name: "amount", Label: "Giá trị", FieldType: "number"},
			}},
		},
		Roles: []PackRole{
			{Name: "ap_manager", Label: "Quản lý duyệt", Actions: []PackRoleAction{
				{EntityName: "ap_orders", Action: "view", AccessLevel: "all"},
			}},
		},
		// IsActive and Criteria.Match deliberately OMITTED: the write path fills true/"and" and
		// export re-emits them — canonicalize must absorb exactly that, nothing more.
		Approvals: []PackApproval{
			{
				Name:       "Duyệt đơn giá cao",
				EntityName: "ap_orders",
				Trigger:    "after_create",
				Criteria: &PackApprovalCriteria{Rules: []PackWorkflowRule{
					{Field: "amount", Op: "gt", Value: float64(1000)},
				}},
				Steps: []PackApprovalStep{
					{Name: "Manager duyệt", ApproverType: "role", ApproverRef: "ap_manager", SLASeconds: 3600},
				},
			},
		},
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	for _, w := range res.Warnings {
		t.Logf("install warning: %s", w)
	}
	const apKey = "ap_orders:Duyệt đơn giá cao"
	if _, ok := store.rows[vpTenantA+"|aptest|"+ItemApproval+"|"+apKey]; !ok {
		t.Fatalf("approval provenance row missing; rows: %v", store.rows)
	}

	// ── THE GATE, approval included: fresh install → every tracked item unchanged. ───────────
	report, err := svc.DiffInstalled(ctx, vpTenantA, "aptest")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	foundAp := false
	for _, it := range report.Items {
		if it.Status != "unchanged" {
			t.Errorf("FRESH INSTALL NOT CLEAN: %s/%s status=%s changed=%v", it.ItemType, it.ItemKey, it.Status, it.ChangedPaths)
		}
		if it.ItemType == ItemApproval && it.ItemKey == apKey {
			foundAp = true
		}
	}
	if !foundAp {
		t.Fatal("approval item absent from the diff report")
	}
	if t.Failed() {
		t.FailNow()
	}

	// ── A real edit: toggle the process inactive → modified at isActive. ─────────────────────
	if err := gdb.Exec(
		"UPDATE sys_approval_processes SET is_active = 0 WHERE tenant_id = ? AND name = ?",
		vpTenantA, "Duyệt đơn giá cao",
	).Error; err != nil {
		t.Fatalf("toggle process: %v", err)
	}
	report, err = svc.DiffInstalled(ctx, vpTenantA, "aptest")
	if err != nil {
		t.Fatalf("diff after edit: %v", err)
	}
	for _, it := range report.Items {
		if it.ItemType == ItemApproval && it.ItemKey == apKey {
			if it.Status != "modified" || len(it.ChangedPaths) != 1 || it.ChangedPaths[0] != "isActive" {
				t.Fatalf("edited approval = %s %v, want modified at [isActive]", it.Status, it.ChangedPaths)
			}
		}
	}

	// ── Deleted process → missing (probed over ListProcesses, deleted_at IS NULL). ───────────
	if err := gdb.Exec(
		"UPDATE sys_approval_processes SET deleted_at = NOW() WHERE tenant_id = ? AND name = ?",
		vpTenantA, "Duyệt đơn giá cao",
	).Error; err != nil {
		t.Fatalf("delete process: %v", err)
	}
	report, err = svc.DiffInstalled(ctx, vpTenantA, "aptest")
	if err != nil {
		t.Fatalf("diff after delete: %v", err)
	}
	for _, it := range report.Items {
		if it.ItemType == ItemApproval && it.ItemKey == apKey {
			if it.Status != "missing" {
				t.Fatalf("deleted approval status = %s, want missing", it.Status)
			}
		}
	}
}
