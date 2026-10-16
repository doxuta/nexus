//go:build integration

// Handover wizard BE integration (../specs/027-handover-wizard/spec.md §6, real MySQL):
//
//	(a) MS-C ISOLATION INVARIANT — installing a builtin into tenant B touches ONLY B: zero new
//	    entities/roles/ledger rows in tenant A, zero obj_* data rows under A's tenant_id (the
//	    obj tables are physically shared; isolation is the tenant_id pin), and B's ledger row
//	    records the acting user (result_json.installedBy — the MS-C operator trace).
//	(e) APPROVALS ROUND-TRIP — a process with a ROLE approver survives export → install into a
//	    fresh tenant through the REAL ApprovalService both ways (validator parity by
//	    construction): the target process is active, criteria intact, and its step resolves to
//	    the TARGET tenant's own role id. A process with a USER approver is NOT exported — the
//	    honest warning fires and the pack carries no trace of it. Re-install skips by
//	    (entity, name).
//
// The HTTP-layer gate (403 non-operator / 404 unknown slug / self ≡ no param) is unit-tested
// with fakes in internal/handlers/admin_vertical_msc_test.go — the service below IS the code
// the handler runs with tenantID = target, so §6a here + those gates cover the §3.1 contract.
package vertical_pack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// vpBuildApprovals mirrors vpBuild but ALSO wires a real ApprovalService as the ApprovalWriter,
// so pack approvals export from + install through the SAME validated CreateProcess path as
// POST /admin/approval-processes. Returns the vertical-pack service and the approval service
// (to author source processes / query the target back).
func vpBuildApprovals(gdb *gorm.DB, sdb *sqlx.DB) (*Service, *services.ApprovalService, *fakeInstallStore) {
	metaRepo := metadata.NewRepository(gdb)
	layoutRepo := metadata.NewLayoutRepository(gdb)
	roleRepo := auth.NewRoleRepository(gdb)
	dyn := dynrepo.New(sdb)

	mc := services.NewMetaCache(metaRepo, services.DefaultMetaTTL) // as cmd/server: a field added at (re)install is visible to the validators at once
	entitySvc := services.NewEntityService(dyn, metaRepo).WithMetaCache(mc)
	fieldSvc := services.NewFieldService(dyn, metaRepo).WithMetaCache(mc)
	layoutSvc := services.NewLayoutService(metaRepo, layoutRepo, zap.NewNop())
	roleSvc := services.NewRoleService(roleRepo)
	relationSvc := services.NewRelationService(dyn, metaRepo)
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dyn).
		WithSequenceClaimer(dyn)
	dashboardRepo := dashboard.NewSQLRepository(sdb)
	dashboardSvc := services.NewDashboardService(dashboardRepo, recordSvc, zap.NewNop())

	bpmRepo := bpm.NewSQLRepository(sdb, nil)
	// Decider nil: this test never approves/rejects — CreateProcess/ListProcesses don't touch it.
	approvalSvc := services.NewApprovalService(bpmRepo, bpmRepo, nil).WithEntityChecker(metaRepo)

	store := &fakeInstallStore{}
	svc := NewService(Deps{
		EntityReader:       metaRepo,
		LayoutReader:       layoutRepo,
		RoleReader:         roleRepo,
		EntityWriter:       entitySvc,
		EntityExists:       metaRepo,
		FieldWriter:        fieldSvc,
		LayoutWriter:       layoutSvc,
		RoleWriter:         roleSvc,
		RelationWriter:     relationSvc,
		RecordWriter:       recordSvc,
		Linker:             relationSvc,
		RecordExists:       dyn,
		RecordFetcher:      dyn,
		DashboardWriter:    dashboardSvc,
		ApprovalWriter:     approvalSvc,
		RelationshipReader: metaRepo,
		RecordLister:       dyn,
		InstallStore:       store,
		ObjProber:          dyn,
	})
	return svc, approvalSvc, store
}

// TestMSC_CrossTenantInstall_IsolationInvariant — §6a: install builtin clinic with
// tenantID = TARGET (tenant B; exactly what the handler passes for ?targetTenant) and prove the
// SOURCE tenant A gained nothing.
func TestMSC_CrossTenantInstall_IsolationInvariant(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _, _ := vpBuildApprovals(gdb, sdb)
	ctx := context.Background()

	pack, ok := BuiltinPack("clinic")
	if !ok {
		t.Fatal("builtin clinic pack not found")
	}

	// Snapshot tenant A BEFORE: fresh DB, so everything must be empty — pinned explicitly so a
	// later harness change can't silently weaken the invariant.
	metaRepo := metadata.NewRepository(gdb)
	entsA, err := metaRepo.ListEntities(vpTenantA)
	if err != nil {
		t.Fatalf("list entities A: %v", err)
	}
	if len(entsA) != 0 {
		t.Fatalf("precondition: tenant A already has entities: %v", entsA)
	}

	// The operator install: tenantID = TARGET (B). The installer takes the tenant strictly by
	// parameter — nothing below the handler reads a context tenant.
	res, err := svc.Install(ctx, vpTenantB, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install clinic into tenant B: %v", err)
	}
	if len(res.EntitiesCreated) != 2 {
		t.Fatalf("EntitiesCreated in B = %v, want the 2 clinic entities", res.EntitiesCreated)
	}

	// Target side: metadata + ledger row present, with the acting user recorded.
	entsB, err := metaRepo.ListEntities(vpTenantB)
	if err != nil {
		t.Fatalf("list entities B: %v", err)
	}
	if len(entsB) != 2 {
		t.Fatalf("tenant B entities = %d, want 2", len(entsB))
	}
	ledgerB, err := svc.ListInstalls(ctx, vpTenantB)
	if err != nil {
		t.Fatalf("ledger B: %v", err)
	}
	if len(ledgerB) != 1 {
		t.Fatalf("ledger rows in B = %d, want 1", len(ledgerB))
	}
	if got := ledgerB[0].Result["installedBy"]; got != vpSeedUserID {
		t.Errorf("ledger installedBy = %v, want the acting user %s (MS-C operator trace)", got, vpSeedUserID)
	}

	// Source side: ZERO new rows of every kind in tenant A.
	entsA, err = metaRepo.ListEntities(vpTenantA)
	if err != nil {
		t.Fatalf("re-list entities A: %v", err)
	}
	if len(entsA) != 0 {
		t.Errorf("tenant A entities after cross-install = %v, want none", entsA)
	}
	rolesA, err := auth.NewRoleRepository(gdb).ListRoles(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list roles A: %v", err)
	}
	if len(rolesA) != 0 {
		t.Errorf("tenant A roles after cross-install = %v, want none", rolesA)
	}
	ledgerA, err := svc.ListInstalls(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("ledger A: %v", err)
	}
	if len(ledgerA) != 0 {
		t.Errorf("ledger rows in A = %d, want 0", len(ledgerA))
	}
	// The obj tables are physically SHARED across tenants — the isolation boundary is the
	// tenant_id pin. Assert no clinic data row landed under A's tenant_id.
	var nA int
	if err := sdb.Get(&nA, "SELECT COUNT(*) FROM obj_clinic_patients WHERE tenant_id = ?", vpTenantA); err != nil {
		t.Fatalf("count A rows in shared obj table: %v", err)
	}
	if nA != 0 {
		t.Errorf("obj_clinic_patients rows under tenant A = %d, want 0", nA)
	}
}

// TestPackApprovals_RoundTrip — §6e: role-approver process → export → install into a fresh
// tenant → active process whose step resolves to the TARGET tenant's role; user-approver
// process → warning + absent from the pack. Re-install skips by (entity, name).
func TestPackApprovals_RoundTrip(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, approvalSvc, _ := vpBuildApprovals(gdb, sdb)
	ctx := context.Background()

	// Source tenant A: entity + role via a base pack (the same validated install path).
	basePack := &Pack{
		Entities: []PackEntity{
			{Name: "ap_orders", Label: "Đơn hàng", Fields: []PackField{
				{Name: "code", Label: "Mã đơn", FieldType: "text", IsUnique: true, IsRequired: true},
				{Name: "amount", Label: "Giá trị", FieldType: "number"},
			}},
		},
		Roles: []PackRole{
			{Name: "ap_manager", Label: "Quản lý duyệt", Actions: []PackRoleAction{
				{EntityName: "ap_orders", Action: "view", AccessLevel: "all"},
			}},
		},
	}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, basePack); err != nil {
		t.Fatalf("seed source tenant A: %v", err)
	}
	rolesA, err := auth.NewRoleRepository(gdb).ListRoles(ctx, vpTenantA)
	if err != nil || len(rolesA) != 1 {
		t.Fatalf("roles A = %v (err %v), want the seeded ap_manager", rolesA, err)
	}
	roleIDinA := rolesA[0].ID

	// Author BOTH processes in A through the REAL admin service (same body as the HTTP path).
	activeTrue := true
	if _, err := approvalSvc.CreateProcess(ctx, vpTenantA, services.CreateApprovalProcessInput{
		Name:       "Duyệt đơn giá cao",
		EntityName: "ap_orders",
		Trigger:    "after_create",
		Match:      "and",
		Rules:      []services.ApprovalCriteriaRuleInput{{Field: "amount", Op: "gt", Value: float64(1000)}},
		Steps: []services.ApprovalStepInput{
			{Name: "Manager duyệt", ApproverType: "role", ApproverID: roleIDinA, SLASeconds: 3600},
		},
		IsActive: &activeTrue,
		ActorID:  vpSeedUserID,
	}); err != nil {
		t.Fatalf("create role-approver process in A: %v", err)
	}
	if _, err := approvalSvc.CreateProcess(ctx, vpTenantA, services.CreateApprovalProcessInput{
		Name:       "Duyệt đích danh",
		EntityName: "ap_orders",
		Trigger:    "after_update",
		Steps: []services.ApprovalStepInput{
			{ApproverType: "user", ApproverID: vpSeedUserID},
		},
		ActorID: vpSeedUserID,
	}); err != nil {
		t.Fatalf("create user-approver process in A: %v", err)
	}

	// Export A. The role process rides the pack (approverRef = role NAME); the user process is
	// dropped whole with the mandated honesty warning.
	pack, warnings, err := svc.Export(ctx, vpTenantA, []string{"ap_orders"}, false, nil)
	if err != nil {
		t.Fatalf("export A: %v", err)
	}
	if len(pack.Approvals) != 1 {
		t.Fatalf("pack.Approvals = %+v, want only the role-approver process", pack.Approvals)
	}
	ap := pack.Approvals[0]
	if ap.Name != "Duyệt đơn giá cao" || len(ap.Steps) != 1 || ap.Steps[0].ApproverRef != "ap_manager" || ap.Steps[0].ApproverType != "role" {
		t.Fatalf("exported approval = %+v, want role step approverRef=ap_manager", ap)
	}
	if ap.Criteria == nil || len(ap.Criteria.Rules) != 1 {
		t.Fatalf("exported criteria = %+v, want the amount rule", ap.Criteria)
	}
	wantWarn := "quy trình duyệt 'Duyệt đích danh': người duyệt chỉ định đích danh không đóng gói được — gán lại sau khi cài"
	found := false
	for _, w := range warnings {
		if w == wantWarn {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want %q", warnings, wantWarn)
	}

	// Install the exported pack into FRESH tenant B.
	res, err := svc.Install(ctx, vpTenantB, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install exported pack into B: %v", err)
	}
	if len(res.ApprovalsCreated) != 1 || res.ApprovalsCreated[0] != "ap_orders:Duyệt đơn giá cao" {
		t.Fatalf("ApprovalsCreated in B = %v, want [ap_orders:Duyệt đơn giá cao]", res.ApprovalsCreated)
	}

	// The process in B is runnable config: active, right trigger, criteria intact, and the step
	// resolves to B's OWN ap_manager role id (≠ A's — no identity crossed tenants).
	rolesB, err := auth.NewRoleRepository(gdb).ListRoles(ctx, vpTenantB)
	if err != nil {
		t.Fatalf("roles B: %v", err)
	}
	roleIDinB := ""
	for _, r := range rolesB {
		if r.Name == "ap_manager" {
			roleIDinB = r.ID
		}
	}
	if roleIDinB == "" {
		t.Fatalf("ap_manager role not installed in B (roles: %v)", rolesB)
	}
	if roleIDinB == roleIDinA {
		t.Fatalf("role id in B equals A's (%s) — ids must be minted per-tenant", roleIDinA)
	}
	procsB, err := approvalSvc.ListProcesses(ctx, vpTenantB)
	if err != nil {
		t.Fatalf("list processes B: %v", err)
	}
	if len(procsB) != 1 {
		t.Fatalf("processes in B = %d, want 1", len(procsB))
	}
	pb := procsB[0]
	if !pb.IsActive || pb.Trigger != "after_create" || pb.EntityName != "ap_orders" {
		t.Errorf("installed process = %+v, want active after_create on ap_orders", pb)
	}
	var stepsB []bpm.Step
	if err := json.Unmarshal(pb.Steps, &stepsB); err != nil {
		t.Fatalf("decode B steps: %v", err)
	}
	if len(stepsB) != 1 || stepsB[0].ApproverType != bpm.ApproverRole || stepsB[0].ApproverID != roleIDinB {
		t.Errorf("B steps = %+v, want one role step bound to B's role id %s", stepsB, roleIDinB)
	}
	if stepsB[0].SLASeconds != 3600 {
		t.Errorf("B step SLA = %d, want 3600 carried through", stepsB[0].SLASeconds)
	}
	if len(pb.Criteria) == 0 || !strings.Contains(string(pb.Criteria), "amount") {
		t.Errorf("B criteria = %s, want the amount rule carried through", string(pb.Criteria))
	}

	// The user-approver process never crossed: B holds no trace of it.
	for _, p := range procsB {
		if p.Name == "Duyệt đích danh" {
			t.Errorf("user-approver process leaked into B: %+v", p)
		}
	}

	// Re-install into B: skip-on-dup by (entity, name).
	res2, err := svc.Install(ctx, vpTenantB, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("re-install into B: %v", err)
	}
	if len(res2.ApprovalsCreated) != 0 || len(res2.ApprovalsSkipped) != 1 {
		t.Errorf("re-install created=%v skipped=%v, want 0/1", res2.ApprovalsCreated, res2.ApprovalsSkipped)
	}
	procsB2, err := approvalSvc.ListProcesses(ctx, vpTenantB)
	if err != nil {
		t.Fatalf("re-list processes B: %v", err)
	}
	if len(procsB2) != 1 {
		t.Errorf("processes in B after re-install = %d, want still 1 (no dup)", len(procsB2))
	}
}
