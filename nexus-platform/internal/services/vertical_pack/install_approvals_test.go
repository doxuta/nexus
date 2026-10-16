package vertical_pack

// Unit tests for the approvals install pass (PACK-APPROVALS, ../specs/027-handover-wizard/spec.md §3.2) and the
// approvals export over in-memory fakes:
//   - install: role NAME → target-tenant role id (freshly created OR pre-existing), criteria/steps
//     mapped into CreateApprovalProcessInput, ActorID = install user;
//   - idempotency: skip-on-dup by (entity, name), re-install skips everything;
//   - fail-open: an unresolvable role → per-item warning + skip (never a failed install); a "user"
//     approver in a hand-authored pack → the same honest warning + skip;
//   - export: role approvers become approverRef = role NAME; a process with a user-approver step
//     is NOT exported (whole-process drop + the mandated warning); out-of-selection drops warn.
// Validator parity itself (CreateProcess's real validation) is covered by the MySQL round-trip
// integration test — the fakes here only record calls.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// fakeApprovalWriter is an in-memory approvalWriter: CreateProcess records the mapped input and
// makes the created process visible to a later ListProcesses (so a re-install skips it by
// (entity, name)). It does NOT validate — parity is proven against the real ApprovalService in
// the integration test.
type fakeApprovalWriter struct {
	mu       sync.Mutex
	seq      int
	existing []bpm.ProcessRecord
	created  []services.CreateApprovalProcessInput
}

func (f *fakeApprovalWriter) ListProcesses(_ context.Context, _ string) ([]bpm.ProcessRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bpm.ProcessRecord(nil), f.existing...), nil
}

func (f *fakeApprovalWriter) CreateProcess(_ context.Context, tenantID string, in services.CreateApprovalProcessInput) (*bpm.ProcessRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.created = append(f.created, in)
	rec := bpm.ProcessRecord{ID: fmt.Sprintf("ap-%d", f.seq), TenantID: tenantID, Name: in.Name, EntityName: in.EntityName, Trigger: in.Trigger}
	f.existing = append(f.existing, rec)
	return &rec, nil
}

// fakeApprovalRoleWriter is an in-memory roleWriter so the pack's roles pass mints ids the
// approvals pass can resolve. Pre-seeded roles back the "role already in the tenant" case.
type fakeApprovalRoleWriter struct {
	mu       sync.Mutex
	seq      int
	existing []services.RoleView
}

func (f *fakeApprovalRoleWriter) ListRoles(_ context.Context, _ string) ([]services.RoleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]services.RoleView(nil), f.existing...), nil
}

func (f *fakeApprovalRoleWriter) CreateRole(_ context.Context, tenantID string, in services.CreateRoleInput) (*services.RoleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	rv := services.RoleView{ID: fmt.Sprintf("role-%d", f.seq), TenantID: tenantID, Name: in.Name, Label: in.Label}
	f.existing = append(f.existing, rv)
	return &rv, nil
}

func (f *fakeApprovalRoleWriter) GetRole(_ context.Context, _, id string) (*services.RoleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.existing {
		if f.existing[i].ID == id {
			cp := f.existing[i]
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("role %q not found", id)
}

// UpdateRole mirrors the real service: a non-nil Actions REPLACES the grant set. The re-handover
// reconcile depends on that being true, since it merges before writing.
func (f *fakeApprovalRoleWriter) UpdateRole(_ context.Context, _, id string, in services.UpdateRoleInput) (*services.RoleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.existing {
		if f.existing[i].ID != id {
			continue
		}
		if in.Actions != nil {
			acts := make([]services.RoleActionView, 0, len(in.Actions))
			for _, a := range in.Actions {
				acts = append(acts, services.RoleActionView{
					EntityName: a.EntityName, Action: a.Action, AccessLevel: a.AccessLevel,
				})
			}
			f.existing[i].Actions = acts
		}
		cp := f.existing[i]
		return &cp, nil
	}
	return nil, fmt.Errorf("role %q not found", id)
}

// newApprovalTestService wires a Service over the shared entity fake + a role writer + the
// approval writer (nil = optional dep absent). NOTE: roleRd (fakeMintRoles) intentionally returns
// no roles — plan.roleIDByName mirrors the REAL repo read; pre-existing roles for the resolution
// test ride roleRd via fakeApRoleReader below instead.
func newApprovalTestService(ap *fakeApprovalWriter, roleRd roleReader) *Service {
	meta := &fakeMintMeta{}
	if roleRd == nil {
		roleRd = fakeMintRoles{}
	}
	d := Deps{
		EntityReader: meta,
		RoleReader:   roleRd,
		EntityWriter: meta,
		RoleWriter:   &fakeApprovalRoleWriter{},
	}
	if ap != nil {
		d.ApprovalWriter = ap // avoid a typed-nil interface tripping the s.approvalWr != nil guard
	}
	return NewService(d)
}

// approvalTestPack: one entity + one role + one approval process whose single step names the
// pack role by NAME, with entry criteria (amount > 1000).
func approvalTestPack() *Pack {
	active := true
	return &Pack{
		Entities: []PackEntity{
			{Name: "ap_orders", Label: "Orders", Fields: []PackField{
				{Name: "code", Label: "Code", FieldType: "text"},
				{Name: "amount", Label: "Amount", FieldType: "number"},
			}},
		},
		Roles: []PackRole{
			{Name: "ap_manager", Label: "Manager", Actions: []PackRoleAction{
				{EntityName: "*", Action: "view", AccessLevel: "all"},
			}},
		},
		Approvals: []PackApproval{
			{
				Name:       "Duyệt đơn giá cao",
				EntityName: "ap_orders",
				Trigger:    "after_create",
				Criteria: &PackApprovalCriteria{Match: "and", Rules: []PackWorkflowRule{
					{Field: "amount", Op: "gt", Value: float64(1000)},
				}},
				Steps: []PackApprovalStep{
					{Name: "Manager duyệt", ApproverType: "role", ApproverRef: "ap_manager", SLASeconds: 3600},
				},
				IsActive: &active,
			},
		},
	}
}

// TestInstall_ApprovalsCreated: the pack approval installs through CreateProcess with the step's
// role NAME resolved to the id the SAME install minted, criteria mapped through, ActorID = the
// install user; a re-install skips it by (entity, name).
func TestInstall_ApprovalsCreated(t *testing.T) {
	ap := &fakeApprovalWriter{}
	svc := newApprovalTestService(ap, nil)
	ctx := context.Background()

	res, err := svc.Install(ctx, "tenant-1", "user-1", approvalTestPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.ApprovalsCreated) != 1 || res.ApprovalsCreated[0] != "ap_orders:Duyệt đơn giá cao" {
		t.Fatalf("ApprovalsCreated = %v, want [ap_orders:Duyệt đơn giá cao]", res.ApprovalsCreated)
	}
	if len(ap.created) != 1 {
		t.Fatalf("CreateProcess called %d times, want 1", len(ap.created))
	}
	in := ap.created[0]
	if in.Name != "Duyệt đơn giá cao" || in.EntityName != "ap_orders" || in.Trigger != "after_create" {
		t.Errorf("mapped input = %+v, want name/entity/trigger carried through", in)
	}
	if in.ActorID != "user-1" {
		t.Errorf("ActorID = %q, want user-1", in.ActorID)
	}
	if len(in.Steps) != 1 || in.Steps[0].ApproverType != "role" || in.Steps[0].SLASeconds != 3600 {
		t.Fatalf("Steps = %+v, want one role step with SLA 3600", in.Steps)
	}
	// The role NAME resolved to the id the roles pass minted (fakeApprovalRoleWriter ids).
	if !strings.HasPrefix(in.Steps[0].ApproverID, "role-") {
		t.Errorf("step ApproverID = %q, want the freshly-minted role id (role-N)", in.Steps[0].ApproverID)
	}
	if in.Match != "and" || len(in.Rules) != 1 || in.Rules[0].Field != "amount" || in.Rules[0].Op != "gt" {
		t.Errorf("criteria mapped = match %q rules %+v, want and/[amount gt]", in.Match, in.Rules)
	}
	if in.IsActive == nil || !*in.IsActive {
		t.Errorf("IsActive = %v, want true carried through", in.IsActive)
	}

	// Re-install: the (entity, name) pair now exists → skipped, no second create.
	res2, err := svc.Install(ctx, "tenant-1", "user-1", approvalTestPack())
	if err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if len(res2.ApprovalsCreated) != 0 || len(res2.ApprovalsSkipped) != 1 {
		t.Errorf("re-install created=%v skipped=%v, want 0/1 (idempotent)", res2.ApprovalsCreated, res2.ApprovalsSkipped)
	}
	if len(ap.created) != 1 {
		t.Errorf("CreateProcess total calls = %d, want 1 (no new create on re-install)", len(ap.created))
	}
}

// unitItemStore is a minimal itemStore capturing SaveItems batches — the unit twin of the
// integration fake, but it keeps LiveID (which that fake drops).
type unitItemStore struct{ items []PackItem }

func (u *unitItemStore) SaveItems(_ context.Context, _, _, _ string, items []PackItem) error {
	u.items = append(u.items, items...)
	return nil
}

func (u *unitItemStore) ListItems(_ context.Context, _, _ string) ([]StoredPackItem, error) {
	return nil, nil
}

// TestInstall_ApprovalProvenanceRecorded: the approval rides sys_pack_items as ("approval",
// "<entity>:<name>") anchored to the created process id — the specs/011 retro-fit gap:
// PACK-APPROVALS shipped invisible to provenance and the diff report.
func TestInstall_ApprovalProvenanceRecorded(t *testing.T) {
	ap := &fakeApprovalWriter{}
	svc := newApprovalTestService(ap, nil)
	store := &unitItemStore{}
	svc.WithItemStore(store)

	pack := approvalTestPack()
	pack.Manifest = &Manifest{Name: "AP", Slug: "aptest", Version: "1.0.0"}
	if _, err := svc.Install(context.Background(), "tenant-1", "user-1", pack); err != nil {
		t.Fatalf("install: %v", err)
	}
	var got *PackItem
	for i := range store.items {
		if store.items[i].Type == ItemApproval {
			got = &store.items[i]
		}
	}
	if got == nil {
		t.Fatalf("no approval provenance item recorded: %+v", store.items)
	}
	if got.Key != "ap_orders:Duyệt đơn giá cao" || !got.Wrote || got.LiveID != "ap-1" {
		t.Fatalf("approval item = %+v, want key ap_orders:Duyệt đơn giá cao / wrote / live id ap-1", got)
	}
}

// fakeApRoleReader is a roleReader with pre-existing tenant roles, backing the "role already in
// the target tenant (not in the pack)" resolution path via plan.roleIDByName.
type fakeApRoleReader struct{ roles []struct{ id, name string } }

func (f fakeApRoleReader) ListRoles(_ context.Context, tenantID string) ([]auth.Role, error) {
	out := make([]auth.Role, 0, len(f.roles))
	for _, r := range f.roles {
		out = append(out, auth.Role{ID: r.id, TenantID: tenantID, Name: r.name})
	}
	return out, nil
}
func (fakeApRoleReader) ListActionsForRole(context.Context, string, string) ([]auth.RoleAction, error) {
	return nil, nil
}

// TestInstall_ApprovalResolvesPreexistingRole: the step's role exists in the TENANT (not the
// pack) — resolution goes through plan.roleIDByName captured in buildPlan's role read.
func TestInstall_ApprovalResolvesPreexistingRole(t *testing.T) {
	ap := &fakeApprovalWriter{}
	rd := fakeApRoleReader{roles: []struct{ id, name string }{{id: "pre-role-9", name: "ap_manager"}}}
	svc := newApprovalTestService(ap, rd)

	pack := approvalTestPack()
	pack.Roles = nil // role NOT in the pack — must resolve from the tenant

	res, err := svc.Install(context.Background(), "tenant-1", "user-1", pack)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.ApprovalsCreated) != 1 {
		t.Fatalf("ApprovalsCreated = %v, want 1", res.ApprovalsCreated)
	}
	if len(ap.created) != 1 || ap.created[0].Steps[0].ApproverID != "pre-role-9" {
		t.Fatalf("step ApproverID = %+v, want pre-role-9 (pre-existing tenant role)", ap.created)
	}
}

// TestInstall_ApprovalMissingRole_WarnsAndSkips: a step naming a role that exists in NEITHER the
// pack NOR the tenant skips THAT process with a warning — the install itself succeeds (fail-open
// like appMint), and CreateProcess is never called for it.
func TestInstall_ApprovalMissingRole_WarnsAndSkips(t *testing.T) {
	ap := &fakeApprovalWriter{}
	svc := newApprovalTestService(ap, nil)

	pack := approvalTestPack()
	pack.Approvals[0].Steps[0].ApproverRef = "ghost_role"

	res, err := svc.Install(context.Background(), "tenant-1", "user-1", pack)
	if err != nil {
		t.Fatalf("install must not fail on an unresolvable approver role: %v", err)
	}
	if len(ap.created) != 0 {
		t.Fatalf("CreateProcess called %d times, want 0", len(ap.created))
	}
	if len(res.ApprovalsSkipped) != 1 {
		t.Errorf("ApprovalsSkipped = %v, want the dropped process", res.ApprovalsSkipped)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "ghost_role") && strings.Contains(w, "gán lại người duyệt") {
			found = true
		}
	}
	if !found {
		t.Errorf("Warnings = %v, want a per-item missing-role warning naming ghost_role", res.Warnings)
	}
}

// TestInstall_ApprovalUserApprover_WarnsAndSkips: a hand-authored pack carrying a "user" approver
// (export never emits one) is skipped with the same honest warning export uses — a named person
// is not portable across tenants.
func TestInstall_ApprovalUserApprover_WarnsAndSkips(t *testing.T) {
	ap := &fakeApprovalWriter{}
	svc := newApprovalTestService(ap, nil)

	pack := approvalTestPack()
	pack.Approvals[0].Steps = []PackApprovalStep{
		{ApproverType: "user", ApproverRef: "11111111-1111-1111-1111-111111111111"},
	}

	res, err := svc.Install(context.Background(), "tenant-1", "user-1", pack)
	if err != nil {
		t.Fatalf("install must not fail on a user-approver pack process: %v", err)
	}
	if len(ap.created) != 0 {
		t.Fatalf("CreateProcess called %d times, want 0", len(ap.created))
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "người duyệt chỉ định đích danh không đóng gói được") {
			found = true
		}
	}
	if !found {
		t.Errorf("Warnings = %v, want the user-approver honesty warning", res.Warnings)
	}
}

// TestInstall_ApprovalsNilWriter: a pack with approvals + no approval writer wired → the section
// is silently skipped (optional dep absent, back-compat), never a panic or failure.
func TestInstall_ApprovalsNilWriter(t *testing.T) {
	svc := newApprovalTestService(nil, nil)
	res, err := svc.Install(context.Background(), "tenant-1", "user-1", approvalTestPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.ApprovalsCreated) != 0 || len(res.ApprovalsSkipped) != 0 {
		t.Errorf("approvals result = %v/%v, want empty with no writer", res.ApprovalsCreated, res.ApprovalsSkipped)
	}
}

// --- export -----------------------------------------------------------------------------------

// mustJSON marshals v for fake ProcessRecord fixtures.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// TestExport_Approvals: a role-approver process on a selected entity exports with approverRef =
// role NAME; a user-approver process is dropped whole with the mandated warning; a process on an
// entity outside the selection is dropped with a warning.
func TestExport_Approvals(t *testing.T) {
	meta := &fakeMintMeta{}
	ctx := context.Background()
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "ap_orders", Label: "Orders"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	roleSteps := []bpm.Step{{Name: "Manager duyệt", ApproverType: bpm.ApproverRole, ApproverID: "role-77", SLASeconds: 900}}
	userSteps := []bpm.Step{{ApproverType: bpm.ApproverUser, ApproverID: "user-42"}}
	criteria := map[string]any{"match": "and", "rules": []map[string]any{{"field": "amount", "op": "gt", "value": 1000}}}

	ap := &fakeApprovalWriter{existing: []bpm.ProcessRecord{
		{ID: "p1", Name: "Duyệt đơn giá cao", EntityName: "ap_orders", Trigger: "after_create",
			Steps: mustJSON(t, roleSteps), Criteria: mustJSON(t, criteria), IsActive: true},
		{ID: "p2", Name: "Duyệt đích danh", EntityName: "ap_orders", Trigger: "after_update",
			Steps: mustJSON(t, userSteps), IsActive: true},
		{ID: "p3", Name: "Ngoài selection", EntityName: "other_entity", Trigger: "after_create",
			Steps: mustJSON(t, roleSteps), IsActive: true},
	}}
	rd := fakeApRoleReader{roles: []struct{ id, name string }{{id: "role-77", name: "ap_manager"}}}

	svc := NewService(Deps{
		EntityReader:   meta,
		LayoutReader:   fakeApLayouts{},
		RoleReader:     rd,
		ApprovalWriter: ap,
	})

	pack, warnings, err := svc.Export(ctx, "tenant-1", []string{"ap_orders"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Approvals) != 1 {
		t.Fatalf("Approvals = %+v, want exactly the role-approver process", pack.Approvals)
	}
	got := pack.Approvals[0]
	if got.Name != "Duyệt đơn giá cao" || got.EntityName != "ap_orders" || got.Trigger != "after_create" {
		t.Errorf("exported approval = %+v, want name/entity/trigger carried", got)
	}
	if len(got.Steps) != 1 || got.Steps[0].ApproverType != "role" || got.Steps[0].ApproverRef != "ap_manager" || got.Steps[0].SLASeconds != 900 {
		t.Errorf("exported steps = %+v, want one role step approverRef=ap_manager sla=900", got.Steps)
	}
	if got.Criteria == nil || len(got.Criteria.Rules) != 1 || got.Criteria.Rules[0].Field != "amount" {
		t.Errorf("exported criteria = %+v, want the amount>1000 rule", got.Criteria)
	}

	wantUserWarn := "quy trình duyệt 'Duyệt đích danh': người duyệt chỉ định đích danh không đóng gói được — gán lại sau khi cài"
	hasUserWarn, hasSelWarn := false, false
	for _, w := range warnings {
		if w == wantUserWarn {
			hasUserWarn = true
		}
		if strings.Contains(w, "1 quy trình duyệt") && strings.Contains(w, "ngoài phạm vi xuất") {
			hasSelWarn = true
		}
	}
	if !hasUserWarn {
		t.Errorf("warnings = %v, want exact user-approver warning %q", warnings, wantUserWarn)
	}
	if !hasSelWarn {
		t.Errorf("warnings = %v, want ONE out-of-scope count line covering p3 (#303), not a per-item warning", warnings)
	}
}

// fakeApLayouts is a no-layout layoutReader for the export test.
type fakeApLayouts struct{}

func (fakeApLayouts) GetByEntity(context.Context, string, string) ([]metadata.LayoutDefinition, error) {
	return nil, nil
}
