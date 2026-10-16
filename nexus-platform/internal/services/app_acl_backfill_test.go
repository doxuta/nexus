package services

// Unit tests for AppService.ACLBackfill (TRỤ 2 mục 4 gap-closer, POST
// /admin/apps/:id/acl-backfill). Reuses newAppSvcFixture/fakeAppStore/fakeAppEntities
// (app_service_test.go) — tenant-a owns entities "accounts"/"contacts" and dashboard "dash-1".

import (
	"context"
	"testing"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/audit"
)

// fakeACLSeeder is a scriptable appACLSeeder: per (appID, entityName) it answers a configured
// (seeded, warning, err) triple (default: seeded=true), and records every call so a test can
// assert exactly which entities were attempted, and in what order.
type fakeACLSeeder struct {
	responses map[string]seedResponse // key: appID + "/" + entityName
	calls     []seedCall
}

type seedResponse struct {
	seeded  bool
	warning string
	err     error
}

type seedCall struct {
	tenantID, appID, entityName string
}

func (f *fakeACLSeeder) SeedForEntity(_ context.Context, tenantID, appID, entityName string) (bool, string, error) {
	f.calls = append(f.calls, seedCall{tenantID: tenantID, appID: appID, entityName: entityName})
	if r, ok := f.responses[appID+"/"+entityName]; ok {
		return r.seeded, r.warning, r.err
	}
	return true, "", nil
}

// spyAuditor records every audit.Event handed to it, so a test can prove dry-run writes NOTHING.
type spyAuditor struct{ events []audit.Event }

func (s *spyAuditor) Record(_ context.Context, evt audit.Event) { s.events = append(s.events, evt) }

// backfillFixture wires an AppService (tenant-a: entities accounts/contacts, dashboard dash-1 —
// see newAppSvcFixture) with appID "app-1"'s nav set to [entity accounts, entity contacts,
// dashboard dash-1, orphan entity ref "ent-gone"] — one of each classification ACLBackfill must
// produce (seeded ×2, skipped ×2: one dashboard, one orphan).
func backfillFixture(t *testing.T) (*AppService, *fakeAppStore, *fakeACLSeeder, *spyAuditor) {
	t.Helper()
	svc, store := newAppSvcFixture(nil)
	store.apps["app-1"] = &appmeta.App{ID: "app-1", TenantID: "tenant-a", Name: "crm", Label: "CRM"}
	store.nav["app-1"] = []appmeta.NavItem{
		{ID: "nav-1", TenantID: "tenant-a", AppID: "app-1", ItemType: appmeta.ItemTypeEntity, RefID: "ent-accounts"},
		{ID: "nav-2", TenantID: "tenant-a", AppID: "app-1", ItemType: appmeta.ItemTypeEntity, RefID: "ent-contacts"},
		{ID: "nav-3", TenantID: "tenant-a", AppID: "app-1", ItemType: appmeta.ItemTypeDashboard, RefID: "dash-1"},
		{ID: "nav-4", TenantID: "tenant-a", AppID: "app-1", ItemType: appmeta.ItemTypeEntity, RefID: "ent-gone"},
	}
	seeder := &fakeACLSeeder{responses: map[string]seedResponse{}}
	auditor := &spyAuditor{}
	svc.WithACLSeeder(seeder).WithAuditor(auditor, nil)
	return svc, store, seeder, auditor
}

// TestACLBackfill_DryRun_WritesNothing proves ?dryRun=true computes the exact classification
// WITHOUT calling the seeder (no grant/role write) and without an audit row.
func TestACLBackfill_DryRun_WritesNothing(t *testing.T) {
	svc, _, seeder, auditor := backfillFixture(t)
	result, err := svc.ACLBackfill(context.Background(), "tenant-a", "app-1", "actor-1", true)
	if err != nil {
		t.Fatalf("ACLBackfill: %v", err)
	}
	if len(seeder.calls) != 0 {
		t.Fatalf("seeder.calls = %v; dry-run must call SeedForEntity ZERO times", seeder.calls)
	}
	if len(auditor.events) != 0 {
		t.Fatalf("auditor.events = %v; dry-run must write NO audit row", auditor.events)
	}
	if len(result.Seeded) != 2 || !contains(result.Seeded, "accounts") || !contains(result.Seeded, "contacts") {
		t.Fatalf("Seeded = %v; want [accounts contacts]", result.Seeded)
	}
	if len(result.Skipped) != 2 || !contains(result.Skipped, "dashboard:dash-1") || !contains(result.Skipped, "orphan:ent-gone") {
		t.Fatalf("Skipped = %v; want [dashboard:dash-1 orphan:ent-gone]", result.Skipped)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v; dry-run must never fail (nothing was attempted)", result.Failed)
	}
}

// TestACLBackfill_Real_SeedsEveryEntityInNav proves a real (non-dry-run) call seeds EVERY
// entity currently in the app's nav — the exact entity names, not ids — via SeedForEntity, and
// writes ONE audit row for the whole call.
func TestACLBackfill_Real_SeedsEveryEntityInNav(t *testing.T) {
	svc, _, seeder, auditor := backfillFixture(t)
	result, err := svc.ACLBackfill(context.Background(), "tenant-a", "app-1", "actor-1", false)
	if err != nil {
		t.Fatalf("ACLBackfill: %v", err)
	}
	if len(seeder.calls) != 2 {
		t.Fatalf("seeder.calls = %v; want exactly 2 (accounts, contacts)", seeder.calls)
	}
	for _, c := range seeder.calls {
		if c.tenantID != "tenant-a" || c.appID != "app-1" {
			t.Errorf("call %+v: tenant/app mismatch", c)
		}
		if c.entityName != "accounts" && c.entityName != "contacts" {
			t.Errorf("call %+v: unexpected entity", c)
		}
	}
	if len(result.Seeded) != 2 || !contains(result.Seeded, "accounts") || !contains(result.Seeded, "contacts") {
		t.Fatalf("Seeded = %v; want [accounts contacts]", result.Seeded)
	}
	if len(result.Skipped) != 2 {
		t.Fatalf("Skipped = %v; want 2 (dashboard + orphan)", result.Skipped)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v; want empty", result.Failed)
	}
	if len(auditor.events) != 1 {
		t.Fatalf("auditor.events = %d; want exactly 1 (one row per backfill CALL)", len(auditor.events))
	}
	if auditor.events[0].EntityName != audit.AppACLMeta || auditor.events[0].ActorUserID != "actor-1" {
		t.Fatalf("audit event = %+v; want EntityName=%q ActorUserID=actor-1", auditor.events[0], audit.AppACLMeta)
	}
}

// TestACLBackfill_FailOpenPerEntity proves ONE entity's SeedForEntity failure (fail-open by
// AppACLSeedService's own contract) lands in Failed WITHOUT aborting the remaining entities —
// the sibling entity still gets seeded.
func TestACLBackfill_FailOpenPerEntity(t *testing.T) {
	svc, _, seeder, _ := backfillFixture(t)
	seeder.responses["app-1/accounts"] = seedResponse{seeded: false, warning: "không thể tạo/tìm vai Nhân viên mặc định"}
	result, err := svc.ACLBackfill(context.Background(), "tenant-a", "app-1", "actor-1", false)
	if err != nil {
		t.Fatalf("ACLBackfill: %v", err)
	}
	if len(result.Failed) != 1 || result.Failed[0].Entity != "accounts" || result.Failed[0].Reason == "" {
		t.Fatalf("Failed = %+v; want 1 entry for accounts with a non-empty reason", result.Failed)
	}
	if len(result.Seeded) != 1 || result.Seeded[0] != "contacts" {
		t.Fatalf("Seeded = %v; want [contacts] — one entity's failure must not block the other", result.Seeded)
	}
}

// TestACLBackfill_UnavailableWhenSeederNotWired proves the fail-closed-unavailable contract:
// no WithACLSeeder call ever happened → every call (dry-run or not) is refused up front, never
// silently no-ops.
func TestACLBackfill_UnavailableWhenSeederNotWired(t *testing.T) {
	svc, _ := newAppSvcFixture(nil) // no WithACLSeeder
	_, err := svc.ACLBackfill(context.Background(), "tenant-a", "app-1", "actor-1", true)
	if codeOf(err) != "APP_ACL_BACKFILL_UNAVAILABLE" {
		t.Fatalf("code = %q; want APP_ACL_BACKFILL_UNAVAILABLE", codeOf(err))
	}
}

// TestACLBackfill_UnknownApp proves an unknown/cross-tenant app id is APP_NOT_FOUND (via the
// shared GetApp path), before the seeder is ever consulted.
func TestACLBackfill_UnknownApp(t *testing.T) {
	svc, _, seeder, _ := backfillFixture(t)
	_, err := svc.ACLBackfill(context.Background(), "tenant-a", "app-ghost", "actor-1", false)
	if codeOf(err) != "APP_NOT_FOUND" {
		t.Fatalf("code = %q; want APP_NOT_FOUND", codeOf(err))
	}
	if len(seeder.calls) != 0 {
		t.Fatalf("seeder.calls = %v; an unknown app must never reach the seeder", seeder.calls)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
