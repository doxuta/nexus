//go:build integration

package services

// publish_revert_integration_test.go - specs/063-draft-publish S7-7b (T113, T113a) against a real
// MySQL: delete a role that users hold and that hides a field, then "Hoan tac" brings back the SAME
// role with the same grants and the same field restriction, gives it back only to the people who
// hold no role now, and is atomic.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
)

type rvEnv struct {
	dpEnv
	tenant, builder string
	bctx            context.Context
	roles           *RoleService
	facl            *FieldACLAdminService
	apps            *AppService
	fRepo           *auth.FieldACLRepository
}

func rvSetup(t *testing.T) rvEnv {
	t.Helper()
	d := dpSetupWith(t, NewWorkflowDocAdapter())
	rec := NewVersionRecorder(d.sdb, d.vers, d.gate, nil)
	roleRepo := auth.NewRoleRepository(d.gdb)
	fRepo := auth.NewFieldACLRepository(d.gdb)
	e := rvEnv{dpEnv: d, tenant: d.newTenant(t), builder: uuid.NewString(), fRepo: fRepo}
	e.bctx = builderCtxOf(e.tenant, e.builder)
	e.roles = NewRoleService(roleRepo).WithRecorder(rec, NewRoleFootprint(d.sdb)).WithRecreator(roleRepo)
	e.facl = NewFieldACLAdminService(fRepo, roleRepo, d.meta).WithRecorder(rec)
	e.apps = NewAppService(appmeta.NewSQLRepository(d.sdb), nil, nil, nil, nil).WithRecorder(rec)
	d.pub.WithReverters(e.roles, e.facl, e.apps)
	e.deliver(t, e.tenant, e.builder)
	return e
}

func (e rvEnv) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.sdb.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (e rvEnv) user(t *testing.T, tenant, role string) string {
	t.Helper()
	id := uuid.NewString()
	var r any
	if role != "" {
		r = role
	}
	if _, err := e.sdb.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, status, role_id) VALUES (?, ?, ?, 'x', 'active', ?)`,
		id, tenant, id[:8]+"@rv.vn", r); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func (e rvEnv) roleOf(t *testing.T, user string) string {
	t.Helper()
	var r *string
	if err := e.sdb.Get(&r, `SELECT role_id FROM users WHERE id = ?`, user); err != nil {
		t.Fatalf("role of %s: %v", user, err)
	}
	if r == nil {
		return ""
	}
	return *r
}

// latestDirect is the id of the newest direct entry of the tenant.
func (e rvEnv) latestDirect(t *testing.T, tenant string) string {
	t.Helper()
	var id string
	if err := e.sdb.Get(&id, `SELECT id FROM sys_meta_versions WHERE tenant_id = ? AND kind = 'direct' ORDER BY seq DESC LIMIT 1`, tenant); err != nil {
		t.Fatalf("latest direct: %v", err)
	}
	return id
}

func (e rvEnv) code(err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

func TestRevert_RoleDelete_RestoresRoleFieldRestrictionAndOnlyRoleLessHolders_Integration(t *testing.T) {
	e := rvSetup(t)
	v, err := e.roles.CreateRole(e.bctx, e.tenant, CreateRoleInput{Name: "staff", Label: "Staff",
		Actions: []RoleActionInput{{EntityName: "hk_deals", Action: "view", AccessLevel: "all"}, {EntityName: "hk_deals", Action: "edit", AccessLevel: "owner"}}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.roles.CreateRole(e.bctx, e.tenant, CreateRoleInput{Name: "viewer", Label: "Viewer"})
	if err != nil {
		t.Fatal(err)
	}
	u1, u2, u3 := e.user(t, e.tenant, v.ID), e.user(t, e.tenant, v.ID), e.user(t, e.tenant, v.ID)
	// the field "code" is hidden from role staff
	if _, err := e.facl.PutGrants(e.bctx, e.tenant, []FieldACLGrantInput{{RoleID: v.ID, EntityName: "hk_deals", FieldName: "code", CanView: false}}); err != nil {
		t.Fatal(err)
	}
	if err := e.roles.DeleteRole(e.bctx, e.tenant, v.ID); err != nil {
		t.Fatal(err)
	}
	del := e.latestDirect(t, e.tenant)
	if n := e.count(t, `SELECT COUNT(*) FROM sys_field_acl WHERE tenant_id = ? AND role_id = ?`, e.tenant, v.ID); n != 0 {
		t.Fatalf("field restriction survived the delete: %d", n)
	}
	// u3 was given another role after the delete: the undo must not take it away
	e.exec(t, `UPDATE users SET role_id = ? WHERE id = ?`, other.ID, u3)

	// GetVersion shows who would get the role back, and who they are now (FR-056)
	d, err := e.pub.GetVersion(e.bctx, e.tenant, del)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Docs) != 1 || len(d.Docs[0].Reassign) != 3 || d.CreatedByLabel != d.CreatedByName {
		t.Fatalf("detail = %+v", d)
	}
	for _, r := range d.Docs[0].Reassign {
		if (r.UserID == u3) != (r.CurrentRoleID == other.ID) {
			t.Fatalf("reassign %+v: only u3 holds another role", r)
		}
	}

	res, err := e.pub.Revert(e.bctx, e.tenant, del)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if res.Reassigned != 2 || res.Entry == nil || res.Entry.Kind != "direct" {
		t.Fatalf("result = %+v", res)
	}
	back, err := e.roles.GetRole(e.bctx, e.tenant, v.ID) // same id
	if err != nil || back.Name != "staff" || len(back.Actions) != 2 {
		t.Fatalf("role back = %+v err %v", back, err)
	}
	rows, err := e.fRepo.ListFieldACL(e.bctx, e.tenant, v.ID, "hk_deals")
	if err != nil || len(rows) != 1 || rows[0].FieldName != "code" || rows[0].CanView {
		t.Fatalf("field restriction after undo = %+v err %v", rows, err)
	}
	if e.roleOf(t, u1) != v.ID || e.roleOf(t, u2) != v.ID || e.roleOf(t, u3) != other.ID {
		t.Fatalf("holders: u1=%s u2=%s u3=%s", e.roleOf(t, u1), e.roleOf(t, u2), e.roleOf(t, u3))
	}
	// a second click: the role exists again, so the entry is stale and nothing changes
	if _, err := e.pub.Revert(e.bctx, e.tenant, del); e.code(err) != "REVERT_STALE" {
		t.Fatalf("second revert err = %v", err)
	}
	// the undo is itself an entry that can be undone: deleting the role again takes the holders off it
	if _, err := e.pub.Revert(e.bctx, e.tenant, res.Entry.ID); err != nil {
		t.Fatalf("undo the undo: %v", err)
	}
	if e.roleOf(t, u1) != "" {
		t.Fatalf("u1 still holds %s", e.roleOf(t, u1))
	}
}

// A failure at the field-permission step rolls back the WHOLE recreate: no role, nobody reassigned.
func TestRevert_RecreateIsAtomic_Integration(t *testing.T) {
	e := rvSetup(t)
	id := uuid.NewString()
	u := e.user(t, e.tenant, "")
	dup := []auth.FieldACL{{EntityName: "hk_deals", FieldName: "code"}, {EntityName: "hk_deals", FieldName: "code"}} // violates uq_sfa_tenant_role_entity_field
	_, err := e.roles.Recreate(e.bctx, e.tenant, id, RoleView{Name: "ghost", Label: "G", Actions: []RoleActionView{{EntityName: "hk_deals", Action: "view", AccessLevel: "all"}}}, dup, []string{u})
	if err == nil {
		t.Fatal("recreate with a broken field-permission row succeeded")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM acl_roles WHERE tenant_id = ? AND id = ?`, e.tenant, id); n != 0 {
		t.Fatalf("role left behind: %d", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM acl_role_actions WHERE role_id = ?`, id); n != 0 {
		t.Fatalf("grants left behind: %d", n)
	}
	if e.roleOf(t, u) != "" {
		t.Fatal("a user was handed a role whose restrictions never landed")
	}
}

// T113a / FR-081: another tenant's entry id is a 404, and nothing of either tenant moves.
func TestRevert_CrossTenantAndKinds_Integration(t *testing.T) {
	a, b := rvSetup(t), rvSetup(t)
	r, err := a.roles.CreateRole(a.bctx, a.tenant, CreateRoleInput{Name: "x", Label: "X"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.roles.DeleteRole(a.bctx, a.tenant, r.ID); err != nil {
		t.Fatal(err)
	}
	entry := a.latestDirect(t, a.tenant)
	if _, err := b.pub.Revert(b.bctx, b.tenant, entry); b.code(err) != "VERSION_NOT_FOUND" {
		t.Fatalf("cross-tenant revert err = %v", err)
	}
	if _, err := b.pub.Revert(b.bctx, b.tenant, "not-a-uuid"); b.code(err) != "VERSION_NOT_FOUND" {
		t.Fatalf("malformed id err = %v", err)
	}
	if n := a.count(t, `SELECT COUNT(*) FROM acl_roles WHERE id = ?`, r.ID); n != 0 {
		t.Fatalf("the role came back through another tenant's call: %d", n)
	}
	// a numbered version is rolled back, not reverted
	var no string
	if err := a.sdb.Get(&no, `SELECT id FROM sys_meta_versions WHERE tenant_id = ? AND version_no IS NOT NULL LIMIT 1`, a.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pub.Revert(a.bctx, a.tenant, no); a.code(err) != "REVERT_USE_ROLLBACK" {
		t.Fatalf("numbered revert err = %v", err)
	}
	// a non-builder is refused before anything is read
	if _, err := a.pub.Revert(legacyCtxOf(a.tenant), a.tenant, entry); a.code(err) != "FORBIDDEN" {
		t.Fatalf("non-builder err = %v", err)
	}
}

// App create is undone by delete, app delete by restore; a taken name fails the restore and changes nothing.
func TestRevert_App_Integration(t *testing.T) {
	e := rvSetup(t)
	app, err := e.apps.CreateApp(e.bctx, e.tenant, CreateAppInput{Name: "crm", Label: "CRM"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.apps.DeleteApp(e.bctx, e.tenant, app.ID); err != nil {
		t.Fatal(err)
	}
	del := e.latestDirect(t, e.tenant)
	if _, err := e.apps.CreateApp(e.bctx, e.tenant, CreateAppInput{Name: "crm", Label: "Other CRM"}); err != nil { // the name is taken now
		t.Fatal(err)
	}
	if _, err := e.pub.Revert(e.bctx, e.tenant, del); e.code(err) != "ROLLBACK_NAME_TAKEN" {
		t.Fatalf("name-taken err = %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_apps WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, e.tenant, app.ID); n != 0 {
		t.Fatal("the app came back despite the taken name")
	}
	e.exec(t, `UPDATE sys_apps SET deleted_at = NOW(), name = CONCAT(name, '__x') WHERE tenant_id = ? AND name = 'crm' AND deleted_at IS NULL`, e.tenant)
	if _, err := e.pub.Revert(e.bctx, e.tenant, del); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_apps WHERE tenant_id = ? AND id = ? AND name = 'crm' AND label = 'CRM' AND deleted_at IS NULL`, e.tenant, app.ID); n != 1 {
		t.Fatal("app not restored under its own id")
	}
	// undo of the restore entry (a create) deletes the app again
	if _, err := e.pub.Revert(e.bctx, e.tenant, e.latestDirect(t, e.tenant)); err != nil {
		t.Fatalf("undo create: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_apps WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, e.tenant, app.ID); n != 0 {
		t.Fatal("app still live")
	}
}

func (e rvEnv) restrictions(t *testing.T, role string) map[string]bool {
	t.Helper()
	rows, err := e.fRepo.ListFieldACL(e.bctx, e.tenant, role, "hk_deals")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.FieldName] = r.CanView
	}
	return out
}

// field_acl: the undo of a matrix save deletes rows absent from `before` and keeps the ones it had; a
// restriction added AFTER makes the older entry stale; the revert leaves an audit-visible entry.
func TestRevert_FieldACL_Integration(t *testing.T) {
	e := rvSetup(t)
	r, err := e.roles.CreateRole(e.bctx, e.tenant, CreateRoleInput{Name: "staff", Label: "Staff"})
	if err != nil {
		t.Fatal(err)
	}
	put := func(field string, view bool) {
		t.Helper()
		if _, err := e.facl.PutGrants(e.bctx, e.tenant, []FieldACLGrantInput{{RoleID: r.ID, EntityName: "hk_deals", FieldName: field, CanView: view}}); err != nil {
			t.Fatal(err)
		}
	}
	put("code", false)
	first := e.latestDirect(t, e.tenant)
	put("stage", false)
	second := e.latestDirect(t, e.tenant)
	// the first save is stale now (stage was added after it)
	if _, err := e.pub.Revert(e.bctx, e.tenant, first); e.code(err) != "REVERT_STALE" {
		t.Fatalf("older save err = %v", err)
	}
	if got := e.restrictions(t, r.ID); len(got) != 2 {
		t.Fatalf("stale revert moved rows: %v", got)
	}
	// undo the second: stage restriction goes, code stays
	if _, err := e.pub.Revert(e.bctx, e.tenant, second); err != nil {
		t.Fatalf("revert second: %v", err)
	}
	if got := e.restrictions(t, r.ID); len(got) != 1 || got["code"] {
		t.Fatalf("after undo of second = %v, want only code hidden", got)
	}
	// undo the first (now current again via the revert entry? no: revert wrote a newer entry -> stale); undo THAT entry instead
	if _, err := e.pub.Revert(e.bctx, e.tenant, e.latestDirect(t, e.tenant)); err != nil {
		t.Fatalf("undo the undo: %v", err)
	}
	if got := e.restrictions(t, r.ID); len(got) != 2 {
		t.Fatalf("redo = %v, want code+stage", got)
	}
}

// A revert that would hide a field that is required-without-default now is refused, and nothing moves.
func TestRevert_FieldACL_RefusesDeadEnd_Integration(t *testing.T) {
	e := rvSetup(t)
	r, err := e.roles.CreateRole(e.bctx, e.tenant, CreateRoleInput{Name: "staff", Label: "Staff"})
	if err != nil {
		t.Fatal(err)
	}
	put := func(view bool) {
		if _, err := e.facl.PutGrants(e.bctx, e.tenant, []FieldACLGrantInput{{RoleID: r.ID, EntityName: "hk_deals", FieldName: "name", CanView: view, CanEdit: view}}); err != nil {
			t.Fatal(err)
		}
	}
	put(false)
	put(true) // clears the restriction: this entry's before = {name hidden}
	clear := e.latestDirect(t, e.tenant)
	e.exec(t, `UPDATE sys_fields f JOIN sys_entities en ON en.id = f.entity_id SET f.is_required = 1 WHERE en.tenant_id = ? AND en.name = 'hk_deals' AND f.name = 'name'`, e.tenant)
	if _, err := e.pub.Revert(e.bctx, e.tenant, clear); e.code(err) != "FIELD_ACL_LOCKED_REQUIRED" {
		t.Fatalf("dead-end revert err = %v", err)
	}
	if got := e.restrictions(t, r.ID); len(got) != 0 {
		t.Fatalf("refused revert still wrote: %v", got)
	}
}

// Role update / create reverts put back the grants / remove the role.
func TestRevert_RoleUpdateAndCreate_Integration(t *testing.T) {
	e := rvSetup(t)
	r, err := e.roles.CreateRole(e.bctx, e.tenant, CreateRoleInput{Name: "staff", Label: "Staff",
		Actions: []RoleActionInput{{EntityName: "hk_deals", Action: "view", AccessLevel: "all"}}})
	if err != nil {
		t.Fatal(err)
	}
	created := e.latestDirect(t, e.tenant)
	if _, err := e.roles.UpdateRole(e.bctx, e.tenant, r.ID, UpdateRoleInput{Actions: []RoleActionInput{
		{EntityName: "hk_deals", Action: "view", AccessLevel: "owner"}, {EntityName: "hk_deals", Action: "delete", AccessLevel: "all"}}}); err != nil {
		t.Fatal(err)
	}
	upd := e.latestDirect(t, e.tenant)
	if _, err := e.pub.Revert(e.bctx, e.tenant, created); e.code(err) != "REVERT_STALE" {
		t.Fatalf("create undo after update err = %v", err)
	}
	if _, err := e.pub.Revert(e.bctx, e.tenant, upd); err != nil {
		t.Fatalf("revert update: %v", err)
	}
	back, err := e.roles.GetRole(e.bctx, e.tenant, r.ID)
	if err != nil || len(back.Actions) != 1 || back.Actions[0].AccessLevel != "all" || back.Actions[0].Action != "view" {
		t.Fatalf("grants after undo = %+v err %v", back, err)
	}
	// undo-the-undo twice brings the original image back, so the create entry matches live again and removes the role
	for i := 0; i < 2; i++ {
		if _, err := e.pub.Revert(e.bctx, e.tenant, e.latestDirect(t, e.tenant)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.pub.Revert(e.bctx, e.tenant, created); err != nil {
		t.Fatalf("create undo err = %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM acl_roles WHERE tenant_id = ? AND id = ?`, e.tenant, r.ID); n != 0 {
		t.Fatal("role survived the undo of its creation")
	}
}

// E1 delete, R1 revert, edit, E2 delete: the old E1 must not resurrect the older image.
func TestRevert_OldDeleteEntryIsStaleAfterRedelete_Integration(t *testing.T) {
	e := rvSetup(t)
	r, err := e.roles.CreateRole(e.bctx, e.tenant, CreateRoleInput{Name: "staff", Label: "Staff",
		Actions: []RoleActionInput{{EntityName: "hk_deals", Action: "view", AccessLevel: "all"}}})
	if err != nil {
		t.Fatal(err)
	}
	u := e.user(t, e.tenant, r.ID)
	if err := e.roles.DeleteRole(e.bctx, e.tenant, r.ID); err != nil {
		t.Fatal(err)
	}
	e1 := e.latestDirect(t, e.tenant)
	if _, err := e.pub.Revert(e.bctx, e.tenant, e1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.roles.UpdateRole(e.bctx, e.tenant, r.ID, UpdateRoleInput{Actions: []RoleActionInput{}}); err != nil {
		t.Fatal(err)
	}
	if err := e.roles.DeleteRole(e.bctx, e.tenant, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pub.Revert(e.bctx, e.tenant, e1); e.code(err) != "REVERT_STALE" {
		t.Fatalf("old delete entry err = %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM acl_roles WHERE tenant_id = ? AND id = ?`, e.tenant, r.ID); n != 0 {
		t.Fatal("the old image came back")
	}
	if e.roleOf(t, u) != "" {
		t.Fatal("a user was handed the old role")
	}
}
