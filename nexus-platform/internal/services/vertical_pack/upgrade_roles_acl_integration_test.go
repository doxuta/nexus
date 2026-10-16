//go:build integration

// specs/069 L3 (T063-T065) against real MySQL, the real RoleService, the real field-ACL admin
// service and the real RecordService: a tenant on qa-upgrade 1.0.0 is upgraded to 1.1.0 and 1.2.0.
// Fixture: testdata/qa-upgrade-v{1,2,3}.json (also what the live
// acceptance uploads).
//
//	TEST_DATABASE_URL=... bash scripts/run-integration.sh ./internal/services/vertical_pack/ -run UpgradeRolesACL
package vertical_pack

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

func qaFixture(t *testing.T, n string) *Pack {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "qa-upgrade-v"+n+".json"))
	if err != nil {
		t.Fatalf("fixture v%s: %v", n, err)
	}
	var p Pack
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("fixture v%s: %v", n, err)
	}
	return &p
}

type qaStack struct {
	svc    *Service
	roles  *services.RoleService
	record *services.RecordService
}

// qaBuild is vpBuild plus what the production composition adds for this slice: the field-ACL admin
// service as pack writer AND reader, an item store that is also the upgrade store, and a
// RecordService that enforces field ACL on reads.
func qaBuild(gdb *gorm.DB, sdb *sqlx.DB) *qaStack {
	metaRepo := metadata.NewRepository(gdb)
	layoutRepo := metadata.NewLayoutRepository(gdb)
	roleRepo := auth.NewRoleRepository(gdb)
	dyn := dynrepo.New(sdb)
	mc := services.NewMetaCache(metaRepo, services.DefaultMetaTTL)
	entitySvc := services.NewEntityService(dyn, metaRepo).WithMetaCache(mc)
	fieldSvc := services.NewFieldService(dyn, metaRepo).WithMetaCache(mc)
	layoutSvc := services.NewLayoutService(metaRepo, layoutRepo, zap.NewNop())
	roleSvc := services.NewRoleService(roleRepo)
	relationSvc := services.NewRelationService(dyn, metaRepo)
	aclRepo := auth.NewFieldACLRepository(gdb)
	aclAdmin := services.NewFieldACLAdminService(aclRepo, roleRepo, mc)
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).WithFieldValueFetcher(dyn).WithRollupMeta(metaRepo).WithRollupLinked(dyn).WithSequenceClaimer(dyn).
		WithFieldACL(services.NewFieldACLService(aclRepo).WithCatalog(mc))
	dashSvc := services.NewDashboardService(dashboard.NewSQLRepository(sdb), recordSvc, zap.NewNop())
	svc := NewService(Deps{
		EntityReader: metaRepo, LayoutReader: layoutRepo, RoleReader: roleRepo, EntityWriter: entitySvc, EntityExists: metaRepo,
		FieldWriter: fieldSvc, LayoutWriter: layoutSvc, RoleWriter: roleSvc, RelationWriter: relationSvc, RecordWriter: recordSvc,
		Linker: relationSvc, RecordExists: dyn, RecordFetcher: dyn, DashboardWriter: dashSvc, RelationshipReader: metaRepo,
		RecordLister: dyn, ObjProber: dyn, FieldACLWriter: aclAdmin, FieldACLReader: aclAdmin,
	}).WithItemStore(newUpgStore())
	return &qaStack{svc: svc, roles: roleSvc, record: recordSvc}
}

func (q *qaStack) roleView(t *testing.T, name string) *services.RoleView {
	t.Helper()
	ctx := context.Background()
	rs, err := q.roles.ListRoles(ctx, vpTenantA)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Name == name {
			v, err := q.roles.GetRole(ctx, vpTenantA, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("role %q not found", name)
	return nil
}

func levelOf(v *services.RoleView, entity, action string) string {
	for _, a := range v.Actions {
		if a.EntityName == entity && a.Action == action {
			return a.AccessLevel
		}
	}
	return ""
}

func (q *qaStack) plan(t *testing.T, p *Pack) *UpgradePlan {
	t.Helper()
	plan, err := q.svc.PlanUpgrade(context.Background(), vpTenantA, "qa-upgrade", p, "upload")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return plan
}

func (q *qaStack) apply(t *testing.T, p *Pack, plan *UpgradePlan, d ...UpgradeDecision) *UpgradeRun {
	t.Helper()
	run, err := q.svc.ApplyUpgrade(context.Background(), vpTenantA, vpSeedUserID, "qa-upgrade", p, "upload", plan.PlanHash, d)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return run
}

// T063: a freshly installed pack (now WITH its field restriction recorded) plans 100% unchanged.
func TestUpgradeRolesACL_FreshInstallAllUnchanged(t *testing.T) {
	gdb, sdb := vpSetup(t)
	q := qaBuild(gdb, sdb)
	if _, err := q.svc.Install(context.Background(), vpTenantA, vpSeedUserID, qaFixture(t, "1")); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	p := q.plan(t, qaFixture(t, "1"))
	sawACL := false
	for _, it := range p.Items {
		if it.Status != StatusUnchanged {
			t.Errorf("%s/%s = %s, want unchanged (fresh install must plan nothing)", it.ItemType, it.ItemKey, it.Status)
		}
		if it.Adopt {
			t.Errorf("%s/%s is adoptable on a fresh install", it.ItemType, it.ItemKey)
		}
		sawACL = sawACL || it.ItemType == ItemFieldACL
	}
	if !sawACL {
		t.Fatalf("the install did not record the field restriction: %d items, none field_acl", len(p.Items))
	}
}

// T064 + T065 end to end: v1 -> v2 -> v3.
func TestUpgradeRolesACL_V1V2V3(t *testing.T) {
	gdb, sdb := vpSetup(t)
	q := qaBuild(gdb, sdb)
	ctx := context.Background()
	if _, err := q.svc.Install(ctx, vpTenantA, vpSeedUserID, qaFixture(t, "1")); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	staff := q.roleView(t, "qa_staff")
	// The builder widens qa_devices edit owner -> all by hand.
	acts := make([]services.RoleActionInput, 0, len(staff.Actions))
	for _, a := range staff.Actions {
		lvl := a.AccessLevel
		if a.EntityName == "qa_devices" && a.Action == "edit" {
			lvl = "all"
		}
		acts = append(acts, services.RoleActionInput{EntityName: a.EntityName, Action: a.Action, AccessLevel: lvl})
	}
	if _, err := q.roles.UpdateRole(ctx, vpTenantA, staff.ID, services.UpdateRoleInput{Actions: acts}); err != nil {
		t.Fatalf("hand-widen: %v", err)
	}

	// ── v2: the pack opens Hợp đồng to qa_staff. Default apply adds it and keeps the widened grant.
	v2 := qaFixture(t, "2")
	p2 := q.plan(t, v2)
	added := find(t, p2, ItemRoleGrant, "qa_staff/qa_contracts/view")
	if added.Status != StatusAdded || added.DefaultAction != ActionApply {
		t.Fatalf("contracts grant: %s/%s, want added/apply", added.Status, added.DefaultAction)
	}
	if w := find(t, p2, ItemRoleGrant, "qa_staff/qa_devices/edit"); w.Status != StatusKeptCustom || w.DefaultAction != ActionKeep {
		t.Fatalf("hand-widened grant: %s/%s, want kept_custom/keep", w.Status, w.DefaultAction)
	}
	if run := q.apply(t, v2, p2); run.Status != RunDone {
		t.Fatalf("apply v2: %s %+v", run.Status, run.Results)
	}
	got := q.roleView(t, "qa_staff")
	if levelOf(got, "qa_contracts", "view") != "all" || levelOf(got, "qa_devices", "edit") != "all" || levelOf(got, "qa_devices", "view") != "all" {
		t.Fatalf("after v2 grants = %+v; want contracts view all, devices edit all (widened kept), devices view all", got.Actions)
	}

	// The restriction on 'Giá vốn' still holds for qa_staff in list, detail and CSV.
	admin := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}
	rec, err := q.record.Create(ctx, admin, vpTenantA, vpSeedUserID, "qa_devices", map[string]any{"name": "Máy A", "cost_price": 777.0})
	if err != nil {
		t.Fatalf("seed record: %v", err)
	}
	st := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", RoleID: got.ID}
	lr, err := q.record.List(ctx, st, vpTenantA, "qa_devices", services.ListQuery{Page: 1, Limit: 20})
	if err != nil || len(lr.Records) != 1 {
		t.Fatalf("staff list: %v n=%d", err, len(lr.Records))
	}
	if _, leaked := lr.Records[0].Data["cost_price"]; leaked {
		t.Errorf("cost_price leaked into the staff list: %v", lr.Records[0].Data)
	}
	one, err := q.record.Get(ctx, st, vpTenantA, "qa_devices", rec, nil, nil)
	if err != nil {
		t.Fatalf("staff get: %v", err)
	}
	if _, leaked := one.Data["cost_price"]; leaked {
		t.Errorf("cost_price leaked into the staff detail: %v", one.Data)
	}
	var csv bytes.Buffer
	if err := q.record.ExportCSV(ctx, st, vpTenantA, "qa_devices", services.ExportCSVOptions{}, &csv); err != nil {
		t.Fatalf("staff csv: %v", err)
	}
	if strings.Contains(csv.String(), "777") || strings.Contains(csv.String(), "Giá vốn") {
		t.Errorf("cost_price leaked into the staff CSV:\n%s", csv.String())
	}
	if !strings.Contains(csv.String(), "Máy A") {
		t.Errorf("staff CSV lost the visible column:\n%s", csv.String())
	}
	// After the apply the plan against v2 is quiet and the widened grant is still "yours".
	again := q.plan(t, v2)
	if again.Summary["writes"] != 0 {
		t.Errorf("after v2, writes = %d, want 0", again.Summary["writes"])
	}
	if w := find(t, again, ItemRoleGrant, "qa_staff/qa_devices/edit"); w.Status != StatusKeptCustom {
		t.Errorf("widened grant after v2: %s, want kept_custom (baseline must not absorb it)", w.Status)
	}

	// ── v3: the pack LOWERS Hợp đồng view all -> owner and relaxes the 'Giá vốn' restriction.
	v3 := qaFixture(t, "3")
	v3.FieldACLs[0].CanView = true // read-only instead of hidden
	p3 := q.plan(t, v3)
	low := find(t, p3, ItemRoleGrant, "qa_staff/qa_contracts/view")
	if low.DefaultAction != ActionKeep || !hasWarn(low, "GRANT_NARROWING") {
		t.Fatalf("lowered grant: default=%s warnings=%v; want keep + GRANT_NARROWING", low.DefaultAction, low.Warnings)
	}
	open := find(t, p3, ItemFieldACL, "qa_staff:qa_devices.cost_price")
	if open.DefaultAction != ActionKeep || !hasWarn(open, "FIELD_ACL_OPENING") {
		t.Fatalf("relaxed restriction: default=%s warnings=%v; want keep + FIELD_ACL_OPENING", open.DefaultAction, open.Warnings)
	}
	if _, err := q.svc.ApplyUpgrade(ctx, vpTenantA, vpSeedUserID, "qa-upgrade", v3, "upload", p3.PlanHash, nil); vcode(err) != "PACK_UPGRADE_NOTHING_TO_APPLY" {
		t.Fatalf("default apply of v3: %v; want nothing to apply (both steps are confirm-only)", err)
	}
	if levelOf(q.roleView(t, "qa_staff"), "qa_contracts", "view") != "all" {
		t.Fatal("a default apply narrowed the grant")
	}
	// The builder picks apply on the narrowing by hand: it really lowers; the widened grant stays.
	run := q.apply(t, v3, p3, UpgradeDecision{ItemType: ItemRoleGrant, ItemKey: "qa_staff/qa_contracts/view", Action: ActionApply})
	if run.Status != RunDone {
		t.Fatalf("apply v3: %s %+v", run.Status, run.Results)
	}
	end := q.roleView(t, "qa_staff")
	if levelOf(end, "qa_contracts", "view") != "owner" || levelOf(end, "qa_devices", "edit") != "all" {
		t.Fatalf("after v3 grants = %+v; want contracts view owner, devices edit still all", end.Actions)
	}
	lr, err = q.record.List(ctx, st, vpTenantA, "qa_devices", services.ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := lr.Records[0].Data["cost_price"]; leaked {
		t.Errorf("the relaxed restriction was applied without being chosen: %v", lr.Records[0].Data)
	}
}
