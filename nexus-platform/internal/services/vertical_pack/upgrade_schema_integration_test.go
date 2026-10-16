//go:build integration

// specs/069 L4 (T076-T078) against real MySQL, the real entity / field / role / app services:
// a tenant on qa-upgrade 1.0.0 is upgraded to 1.3.0 (a new table, a new field, a relabelled field,
// a type change that must NOT be written, a menu with a hand-made order), and CRM's "Ngày chốt"
// (#239) is brought back to `indexed`.
//
//	TEST_DATABASE_URL=... bash scripts/run-integration.sh ./internal/services/vertical_pack/ -run UpgradeSchema
package vertical_pack

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// failingEntityWr injects a create failure for one table; every other method is the real service's.
type failingEntityWr struct {
	*services.EntityService
	fail string
}

func (f failingEntityWr) CreateEntity(ctx context.Context, tenantID string, in services.CreateEntityInput) (*metadata.EntityDefinition, []metadata.FieldDefinition, error) {
	if in.Name == f.fail {
		return nil, nil, invalid("ENTITY_INJECTED_FAILURE", "injected")
	}
	return f.EntityService.CreateEntity(ctx, tenantID, in)
}

type schemaStack struct {
	*qaStack
	apps  *services.AppService
	meta  *metadata.Repository
	store *upgStore
	field *services.FieldService
}

// schemaBuild is qaBuild plus the production app service and an optional injected create failure.
func schemaBuild(gdb *gorm.DB, sdb *sqlx.DB, failEntity string) *schemaStack {
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
	appSvc := services.NewAppService(appmeta.NewSQLRepository(sdb), metaRepo, dashboard.NewSQLRepository(sdb), nil, zap.NewNop())
	var ew entityWriter = entitySvc
	if failEntity != "" {
		ew = failingEntityWr{EntityService: entitySvc, fail: failEntity}
	}
	store := newUpgStore()
	svc := NewService(Deps{
		EntityReader: metaRepo, LayoutReader: layoutRepo, RoleReader: roleRepo, EntityWriter: ew, EntityExists: metaRepo,
		FieldWriter: fieldSvc, LayoutWriter: layoutSvc, RoleWriter: roleSvc, RelationWriter: relationSvc, RecordWriter: recordSvc,
		Linker: relationSvc, RecordExists: dyn, RecordFetcher: dyn, DashboardWriter: dashSvc, RelationshipReader: metaRepo,
		RecordLister: dyn, ObjProber: dyn, FieldACLWriter: aclAdmin, FieldACLReader: aclAdmin,
	}).WithItemStore(store).WithAppService(appSvc)
	return &schemaStack{qaStack: &qaStack{svc: svc, roles: roleSvc, record: recordSvc}, apps: appSvc, meta: metaRepo, store: store, field: fieldSvc}
}

func (q *schemaStack) fieldOf(t *testing.T, entity, field string) metadata.FieldDefinition {
	t.Helper()
	e, err := q.meta.GetEntityByName(context.Background(), vpTenantA, entity)
	if err != nil {
		t.Fatalf("entity %s: %v", entity, err)
	}
	fs, err := q.meta.ListFields(context.Background(), vpTenantA, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Name == field {
			return f
		}
	}
	t.Fatalf("field %s.%s not found", entity, field)
	return metadata.FieldDefinition{}
}

func (q *schemaStack) hasEntity(t *testing.T, name string) bool {
	t.Helper()
	ents, err := q.meta.ListEntities(vpTenantA)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name == name {
			return true
		}
	}
	return false
}

// navNames: the pack app's menu as entity names, in order.
func (q *schemaStack) navNames(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	apps, err := q.apps.ListApps(ctx, vpTenantA)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := q.meta.ListEntities(vpTenantA)
	if err != nil {
		t.Fatal(err)
	}
	nameOf := map[string]string{}
	for _, e := range ents {
		nameOf[e.ID] = e.Name
	}
	for _, a := range apps {
		if a.Name != "qa_upgrade" {
			continue
		}
		_, nav, err := q.apps.GetApp(ctx, vpTenantA, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range nav {
			out = append(out, nameOf[n.RefID])
		}
		return out
	}
	t.Fatal("app qa_upgrade not found")
	return nil
}

// reverseNav is the builder arranging the menu by hand.
func (q *schemaStack) reverseNav(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	apps, _ := q.apps.ListApps(ctx, vpTenantA)
	for _, a := range apps {
		if a.Name != "qa_upgrade" {
			continue
		}
		_, nav, err := q.apps.GetApp(ctx, vpTenantA, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		var in []services.AppNavItemInput
		for i := len(nav) - 1; i >= 0; i-- {
			in = append(in, services.AppNavItemInput{ItemType: nav[i].ItemType, RefID: nav[i].RefID})
		}
		if _, err := q.apps.ReplaceNav(ctx, vpTenantA, a.ID, in); err != nil {
			t.Fatal(err)
		}
	}
}

func outcomeOf(run *UpgradeRun, typ, key string) string {
	for _, r := range run.Results {
		if r.ItemType == typ && r.ItemKey == key {
			return r.Outcome
		}
	}
	return "(no result)"
}

// T077 + T078: 1.0.0 -> 1.3.0 with a hand-ordered menu and one field the builder keeps.
func TestUpgradeSchema_V1ToV4_NewTableFieldMenuAndKeptField(t *testing.T) {
	gdb, sdb := vpSetup(t)
	q := schemaBuild(gdb, sdb, "")
	ctx := context.Background()
	if _, err := q.svc.Install(ctx, vpTenantA, vpSeedUserID, qaFixture(t, "1")); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	q.reverseNav(t)
	before := q.navNames(t)
	if len(before) != 2 || before[0] != "qa_devices" {
		t.Fatalf("hand-ordered menu = %v; want [qa_devices qa_contracts]", before)
	}

	v4 := qaFixture(t, "4")
	p := q.plan(t, v4)
	if it := find(t, p, ItemEntity, "qa_warranties"); it.Status != StatusAdded || it.DefaultAction != ActionApply {
		t.Fatalf("new table: %s/%s; want added/apply", it.Status, it.DefaultAction)
	}
	if it := find(t, p, ItemField, "qa_contracts.amount"); it.Status != StatusUnsupported || it.Reason != ReasonFieldTypeChange {
		t.Fatalf("type change: %s/%s; want unsupported/FIELD_TYPE_CHANGE", it.Status, it.Reason)
	}
	// The builder keeps "Số seri" out of this run, takes everything else the plan pre-selects.
	run := q.apply(t, v4, p, UpgradeDecision{ItemType: ItemField, ItemKey: "qa_devices.serial_no", Action: ActionKeep})
	if run.Status != RunDone {
		t.Fatalf("apply: %s %+v", run.Status, run.Results)
	}
	if !q.hasEntity(t, "qa_warranties") {
		t.Fatal("qa_warranties was not created")
	}
	if got := q.fieldOf(t, "qa_devices", "name").Label; got != "Tên thiết bị" {
		t.Errorf("relabelled field = %q", got)
	}
	if got := q.fieldOf(t, "qa_contracts", "amount").FieldType; got != "number" {
		t.Errorf("type change was written: amount is now %q", got)
	}
	staff := q.roleView(t, "qa_staff")
	if levelOf(staff, "qa_warranties", "view") != "all" {
		t.Errorf("no grant on the new table: %+v", staff.Actions)
	}
	nav := q.navNames(t)
	if len(nav) != 3 || nav[0] != "qa_devices" || nav[1] != "qa_contracts" || nav[2] != "qa_warranties" {
		t.Errorf("menu after = %v; want the hand order kept and qa_warranties LAST", nav)
	}

	// T078: the kept field is still proposed, the applied ones are quiet.
	again := q.plan(t, v4)
	if it := find(t, again, ItemField, "qa_devices.serial_no"); it.Status != StatusAdded || it.DefaultAction != ActionApply {
		t.Errorf("kept field next plan: %s/%s; want added/apply (still proposed)", it.Status, it.DefaultAction)
	}
	if it := find(t, again, ItemField, "qa_devices.name"); it.Status != StatusUnchanged {
		t.Errorf("applied field next plan: %s; want unchanged", it.Status)
	}
	if it := find(t, again, ItemEntity, "qa_warranties"); it.Status != StatusUnchanged {
		t.Errorf("created table next plan: %s; want unchanged", it.Status)
	}
	// Taking it later adds the column; the menu is not touched again.
	run2 := q.apply(t, v4, again)
	if run2.Status != RunDone || q.fieldOf(t, "qa_devices", "serial_no").Label != "Số seri" {
		t.Fatalf("second apply: %s %+v", run2.Status, run2.Results)
	}
	if got := q.navNames(t); len(got) != 3 {
		t.Errorf("menu changed by a field-only run: %v", got)
	}
}

// T077 (injection): the table cannot be created -> its grants are `blocked`, nothing is guessed;
// independent items (the new field on another table) still land.
func TestUpgradeSchema_FailedTableBlocksItsGrants(t *testing.T) {
	gdb, sdb := vpSetup(t)
	q := schemaBuild(gdb, sdb, "qa_warranties")
	ctx := context.Background()
	if _, err := q.svc.Install(ctx, vpTenantA, vpSeedUserID, qaFixture(t, "1")); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	v4 := qaFixture(t, "4")
	run := q.apply(t, v4, q.plan(t, v4))
	if run.Status != RunPartial {
		t.Fatalf("status = %s, want partial: %+v", run.Status, run.Results)
	}
	if got := outcomeOf(run, ItemEntity, "qa_warranties"); got != OutcomeFailed {
		t.Errorf("table outcome = %s, want failed", got)
	}
	if got := outcomeOf(run, ItemRoleGrant, "qa_staff/qa_warranties/view"); got != OutcomeBlocked {
		t.Errorf("grant on the missing table = %s, want blocked", got)
	}
	if got := outcomeOf(run, ItemField, "qa_devices.serial_no"); got != OutcomeApplied {
		t.Errorf("independent field = %s, want applied", got)
	}
	if levelOf(q.roleView(t, "qa_staff"), "qa_warranties", "view") != "" {
		t.Error("a grant on a table that does not exist was written")
	}
	// Next plan still proposes the table and its grants.
	again := q.plan(t, v4)
	if it := find(t, again, ItemEntity, "qa_warranties"); it.Status != StatusAdded {
		t.Errorf("failed table next plan: %s, want added", it.Status)
	}
	if it := find(t, again, ItemRoleGrant, "qa_staff/qa_warranties/view"); it.Status != StatusAdded {
		t.Errorf("blocked grant next plan: %s, want added", it.Status)
	}
}

// T076 (#239): a CRM tenant whose "Ngày chốt" lost `indexed` (northwind's state, written by an old
// install) gets it back through a plain upgrade.
func TestUpgradeSchema_CRMCloseDateIndexedComesBack(t *testing.T) {
	gdb, sdb := vpSetup(t)
	q := schemaBuild(gdb, sdb, "")
	ctx := context.Background()
	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm missing")
	}
	if _, err := q.svc.Install(ctx, vpTenantA, vpSeedUserID, crm); err != nil {
		t.Fatalf("install crm: %v", err)
	}
	cd := q.fieldOf(t, "crm_opportunities", "close_date")
	opts := fieldOpts(t, cd)
	if opts["indexed"] != true {
		t.Skipf("builtin crm close_date is not indexed (%v) — nothing to reproduce", opts)
	}
	delete(opts, "indexed")
	if _, err := q.field.EditField(ctx, vpTenantA, services.EditFieldInput{FieldID: cd.ID, Options: opts}); err != nil {
		t.Fatalf("un-index: %v", err)
	}
	// An old install recorded the table WITHOUT indexed: rewrite the baseline the same way.
	rows, _ := q.store.ListItems(ctx, vpTenantA, "crm")
	for _, r := range rows {
		if r.Type == ItemEntity && r.Key == "crm_opportunities" {
			old := stripIndexed(t, r.Content, "close_date")
			q.store.rows[vpTenantA+"|crm|entity|crm_opportunities"] = StoredPackItem{Type: r.Type, Key: r.Key, PackVersion: r.PackVersion, Content: old, LiveID: r.LiveID}
		}
	}
	again, _ := BuiltinPack("crm")
	plan, err := q.svc.PlanUpgrade(ctx, vpTenantA, "crm", again, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	it := find(t, plan, ItemField, "crm_opportunities.close_date")
	if it.Status != StatusUpdateSafe || it.DefaultAction != ActionApply {
		t.Fatalf("close_date: %s/%s; want update_safe/apply", it.Status, it.DefaultAction)
	}
	run, err := q.svc.ApplyUpgrade(ctx, vpTenantA, vpSeedUserID, "crm", again, "builtin", plan.PlanHash, nil)
	if err != nil || run.Status != RunDone {
		t.Fatalf("apply: %v %+v", err, run)
	}
	if got := fieldOpts(t, q.fieldOf(t, "crm_opportunities", "close_date"))["indexed"]; got != true {
		t.Errorf("close_date options.indexed = %v after the upgrade, want true", got)
	}
	after, _ := q.svc.PlanUpgrade(ctx, vpTenantA, "crm", again, "builtin")
	if it := find(t, after, ItemField, "crm_opportunities.close_date"); it.Status != StatusUnchanged {
		t.Errorf("close_date after the upgrade: %s, want unchanged", it.Status)
	}
}

// fieldOpts decodes a field row's options JSON.
func fieldOpts(t *testing.T, f metadata.FieldDefinition) map[string]any {
	t.Helper()
	out := map[string]any{}
	if f.Options != nil {
		if err := json.Unmarshal([]byte(*f.Options), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// stripIndexed removes options.indexed of one field from a stored table baseline.
func stripIndexed(t *testing.T, raw json.RawMessage, field string) json.RawMessage {
	t.Helper()
	var pe PackEntity
	if err := json.Unmarshal(raw, &pe); err != nil {
		t.Fatal(err)
	}
	for i := range pe.Fields {
		if pe.Fields[i].Name == field {
			delete(pe.Fields[i].Options, "indexed")
		}
	}
	out, err := json.Marshal(pe)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
