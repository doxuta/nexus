//go:build integration

// propose_access against a real MySQL (issue #2): the validator reads the real roles, users and
// publish cutoff; the apply does what lib/aiApply.ts does (read the role, merge additively, write it
// back through RoleService.UpdateRole), which leaves a traced "role" version entry; the revert
// (planRevertRole) puts the role back. The role's grants on its other tables survive both.
package ai

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
)

func TestProposeAccess_IT_ApplyThenRevertKeepsTheOtherGrants(t *testing.T) {
	repo, gdb := newRelITReader(t)
	sdb := connectAI(t)
	tenant, builder := uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM sys_meta_version_docs WHERE tenant_id = ?`, `DELETE FROM sys_meta_versions WHERE tenant_id = ?`,
			`DELETE FROM sys_meta_drafts WHERE tenant_id = ?`, `DELETE FROM users WHERE tenant_id = ?`,
			`DELETE FROM acl_roles WHERE tenant_id = ?`, `DELETE FROM sys_entities WHERE tenant_id = ?`, `DELETE FROM tenants WHERE id = ?`,
		} {
			gdb.Exec(q, tenant)
		}
	})
	if err := gdb.Exec(`INSERT INTO tenants (id, slug, name) VALUES (?, ?, 'propose access it')`, tenant, "acc-"+tenant[:8]).Error; err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	ctx := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: builder, TenantID: tenant, HomeTenantID: tenant, IsBuilder: true, ReadDraft: true})

	reg := metaversion.NewRegistry(services.NewWorkflowDocAdapter())
	vers := metaversion.NewVersionRepo(sdb)
	gate := services.NewDraftGate(vers)
	pub := services.NewPublishService(sdb, metaversion.NewDraftRepo(sdb, reg), vers, reg, gate, nil, nil)
	roleRepo := auth.NewRoleRepository(gdb)
	roles := services.NewRoleService(roleRepo).WithRecorder(services.NewVersionRecorder(sdb, vers, gate, nil), services.NewRoleFootprint(sdb)).WithRecreator(roleRepo)
	pub.WithReverters(roles, nil, nil)

	// Before delivery: a CRM table, the rep's role with its CRM grants, a manager role an admin holds.
	if err := gdb.Create(&metadata.EntityDefinition{ID: uuid.NewString(), TenantID: tenant, Name: "it_acc_accounts", Label: "Khách hàng", DataTable: "obj_it_acc_accounts"}).Error; err != nil {
		t.Fatalf("seed table: %v", err)
	}
	crm := []services.RoleActionInput{
		{EntityName: "it_acc_accounts", Action: "view", AccessLevel: "all"},
		{EntityName: "it_acc_accounts", Action: "create", AccessLevel: "all"},
		{EntityName: "it_acc_accounts", Action: "edit", AccessLevel: "owner"},
		{EntityName: "it_acc_accounts", Action: "delete", AccessLevel: "owner"},
	}
	rep, err := roles.CreateRole(context.Background(), tenant, services.CreateRoleInput{Name: "it_sales_rep", Label: "Nhân viên kinh doanh", Actions: crm})
	if err != nil {
		t.Fatalf("create rep role: %v", err)
	}
	mgr, err := roles.CreateRole(context.Background(), tenant, services.CreateRoleInput{Name: "it_sales_manager", Label: "Quản lý"})
	if err != nil {
		t.Fatalf("create manager role: %v", err)
	}
	for _, u := range []struct {
		role  string
		admin int
	}{{rep.ID, 0}, {mgr.ID, 1}} {
		if _, err := sdb.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, status, role_id, is_admin) VALUES (?, ?, ?, 'x', 'active', ?, ?)`,
			uuid.NewString(), tenant, uuid.NewString()[:8]+"@acc.it", u.role, u.admin); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
	if _, err := pub.Baseline(ctx, tenant, "first"); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	// After delivery: the table the agent just built is born after the cutoff, so it is still hidden.
	cutoff, err := gate.HeadCutoff(ctx, tenant)
	if err != nil || cutoff == nil {
		t.Fatalf("cutoff = %v, %v; want the baseline's", cutoff, err)
	}
	if err := gdb.Create(&metadata.EntityDefinition{ID: uuid.NewString(), TenantID: tenant, Name: "it_acc_tickets", Label: "Phiếu hỗ trợ",
		DataTable: "obj_it_acc_tickets", CreatedAt: cutoff.Add(2 * time.Second)}).Error; err != nil {
		t.Fatalf("seed new table: %v", err)
	}

	e := &toolExecutor{reader: ReaderWithAccessProbes(repo, &AccessProbes{Users: auth.NewUserRepository(gdb), Roles: roleRepo, Published: gate}), tenantID: tenant}
	for _, c := range []struct{ args, code string }{
		{grantArgs(grant("it_sales_rep", "it_acc_accounts", "all", "view")), "ACCESS_ENTITY_PUBLISHED"},
		{grantArgs(grant("it_sales_manager", "it_acc_tickets", "all", "view")), "ACCESS_ROLE_ADMIN"},
	} {
		mustReject(t, e, "propose_access", c.args, c.code)
	}
	mustBuffer(t, e, "propose_access", grantArgs(grant("it_sales_rep", "it_acc_tickets", "all", "view", "create"), grant("it_sales_rep", "it_acc_tickets", "owner", "edit")))
	proposal := e.draft[0].Access

	// Apply, as lib/aiApply.ts does it: read the role, merge, write the whole set back.
	live, err := roles.GetRole(ctx, tenant, rep.ID)
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeAccessGrants(live.Actions, proposal.Grants)
	in := make([]services.RoleActionInput, len(merged))
	for i, g := range merged {
		in[i] = services.RoleActionInput{EntityName: g.EntityName, Action: g.Action, AccessLevel: g.AccessLevel}
	}
	if _, err := roles.UpdateRole(ctx, tenant, rep.ID, services.UpdateRoleInput{Name: &live.Name, Label: &live.Label, Actions: in}); err != nil {
		t.Fatalf("apply (PUT /admin/roles/:id): %v", err)
	}

	grantsOf := func(entity string) map[string]string {
		t.Helper()
		v, err := roles.GetRole(ctx, tenant, rep.ID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, a := range v.Actions {
			if a.EntityName == entity {
				out[a.Action] = a.AccessLevel
			}
		}
		return out
	}
	crmWant := map[string]string{"view": "all", "create": "all", "edit": "owner", "delete": "owner"}
	if got := grantsOf("it_acc_accounts"); !maps.Equal(got, crmWant) {
		t.Fatalf("after apply the rep's other table = %v; want it untouched %v", got, crmWant)
	}
	if got := grantsOf("it_acc_tickets"); !maps.Equal(got, map[string]string{"view": "all", "create": "all", "edit": "owner"}) {
		t.Fatalf("after apply tickets = %v; want view all, create all, edit owner, no delete", got)
	}

	// The apply is a traced "role" document of a direct entry, whose before is the role as it was.
	var entry string
	if err := sdb.Get(&entry, `SELECT id FROM sys_meta_versions WHERE tenant_id = ? AND kind = 'direct' ORDER BY seq DESC LIMIT 1`, tenant); err != nil {
		t.Fatalf("no direct entry after the apply: %v", err)
	}
	var doc struct {
		DocType string `db:"doc_type"`
		DocKey  string `db:"doc_key"`
		Op      string `db:"op"`
		Before  []byte `db:"before_json"`
	}
	if err := sdb.Get(&doc, `SELECT doc_type, doc_key, op, before_json FROM sys_meta_version_docs WHERE tenant_id = ? AND version_id = ?`, tenant, entry); err != nil {
		t.Fatalf("entry doc: %v", err)
	}
	var before services.RoleView
	if doc.DocType != "role" || doc.DocKey != rep.ID || doc.Op != "update" || json.Unmarshal(doc.Before, &before) != nil || len(before.Actions) != len(crm) {
		t.Fatalf("entry doc = %s %s %s before=%s; want role %s update with the 4 CRM grants before", doc.DocType, doc.DocKey, doc.Op, doc.Before, rep.ID)
	}

	// Revert (Bảo trì › Lịch sử › Hoàn tác, planRevertRole): the tickets grants go, the CRM ones stay.
	if _, err := pub.Revert(ctx, tenant, entry); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := grantsOf("it_acc_accounts"); !maps.Equal(got, crmWant) {
		t.Fatalf("after revert the rep's other table = %v; want %v", got, crmWant)
	}
	if got := grantsOf("it_acc_tickets"); len(got) != 0 {
		t.Fatalf("after revert tickets = %v; want no grant", got)
	}
	if v, _ := roles.GetRole(ctx, tenant, rep.ID); !slices.Equal(sortedGrants(v.Actions), sortedGrants(before.Actions)) {
		t.Fatalf("after revert the role = %v; want exactly its before image %v", v.Actions, before.Actions)
	}
}

func sortedGrants(gs []services.RoleActionView) []services.RoleActionView {
	out := slices.Clone(gs)
	slices.SortFunc(out, func(a, b services.RoleActionView) int {
		return cmp.Or(cmp.Compare(a.EntityName, b.EntityName), cmp.Compare(a.Action, b.Action))
	})
	return out
}
