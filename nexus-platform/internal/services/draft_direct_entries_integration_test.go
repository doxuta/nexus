//go:build integration

package services

// draft_direct_entries_integration_test.go - specs/063-draft-publish S7-5 (T087/T088) against a real
// MySQL: the REAL RoleService / WorkflowService over the real recorder and the delivered-tenant gate.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/workflow"
)

type directEntry struct {
	ID             string `db:"id"`
	Seq            uint   `db:"seq"`
	VersionNo      *uint  `db:"version_no"`
	CreatedBy      string `db:"created_by"`
	CreatedByLabel string `db:"created_by_label"`
}

func (e dpEnv) directEntries(t *testing.T, tenant string) []directEntry {
	t.Helper()
	var out []directEntry
	if err := e.sdb.Select(&out, `SELECT id, seq, version_no, created_by, created_by_label FROM sys_meta_versions
		WHERE tenant_id = ? AND kind = 'direct' ORDER BY seq`, tenant); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e dpEnv) legacyAdmin(t *testing.T, tenant, fullName string) (context.Context, string) {
	t.Helper()
	id := uuid.NewString()
	if _, err := e.sdb.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin) VALUES (?, ?, ?, 'x', ?, 'active', 1)`,
		id, tenant, "qtv"+id[:6]+"@x.vn", fullName); err != nil {
		t.Fatal(err)
	}
	return metaversion.WithWarnings(metaversion.WithCaller(context.Background(),
		metaversion.Caller{UserID: id, TenantID: tenant, HomeTenantID: tenant})), id
}

// T087: deleting a role writes ONE direct entry that carries the holders and BOTH field-permission
// rows - read before the FKs detach / cascade them.
func TestDirectEntry_RoleDeleteKeepsHoldersAndFieldACL_Integration(t *testing.T) {
	e := dpSetup(t)
	tenant := e.newTenant(t)
	e.deliver(t, tenant, uuid.NewString())
	rec := NewVersionRecorder(e.sdb, e.vers, e.gate, nil)
	svc := NewRoleService(auth.NewRoleRepository(e.gdb)).WithRecorder(rec, NewRoleFootprint(e.sdb))
	ctx, adminID := e.legacyAdmin(t, tenant, "Quản trị Khách")

	rv, err := svc.CreateRole(ctx, tenant, CreateRoleInput{Name: "sales", Label: "Sales"})
	if err != nil {
		t.Fatal(err)
	}
	holder := uuid.NewString()
	if _, err := e.sdb.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, status, role_id) VALUES (?, ?, ?, 'x', 'active', ?)`,
		holder, tenant, "h"+holder[:6]+"@x.vn", rv.ID); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"stage", "close_date"} {
		if _, err := e.sdb.Exec(`INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit) VALUES (?, ?, ?, 'hk_deals', ?, 1, 0)`,
			uuid.NewString(), tenant, rv.ID, f); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.DeleteRole(ctx, tenant, rv.ID); err != nil {
		t.Fatal(err)
	}
	if w := metaversion.Warnings(ctx); len(w) != 0 {
		t.Fatalf("unexpected warning %v", w)
	}

	entries := e.directEntries(t, tenant)
	if len(entries) != 2 { // create + delete
		t.Fatalf("direct entries = %d, want 2", len(entries))
	}
	last := entries[1]
	if last.VersionNo != nil || last.CreatedBy != adminID || last.CreatedByLabel != "Quản trị Khách" {
		t.Fatalf("entry = %+v", last)
	}
	var op string
	var meta, before json.RawMessage
	if err := e.sdb.QueryRow(`SELECT op, meta, before_json FROM sys_meta_version_docs WHERE tenant_id = ? AND version_id = ?`, tenant, last.ID).Scan(&op, &meta, &before); err != nil {
		t.Fatal(err)
	}
	var m struct {
		AssignedUserIDs []string          `json:"assignedUserIds"`
		FieldACL        []json.RawMessage `json:"fieldAcl"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		t.Fatal(err)
	}
	if op != "delete" || len(m.AssignedUserIDs) != 1 || m.AssignedUserIDs[0] != holder || len(m.FieldACL) != 2 || len(before) == 0 {
		t.Fatalf("op=%s meta=%s", op, meta)
	}
}

// T088: a legacy customer admin's workflow write on a delivered tenant applies at once AND leaves a
// direct entry in THEIR name with before/after; before delivery nothing is recorded.
func TestDirectEntry_LegacyAdminWorkflowWrite_Integration(t *testing.T) {
	e := dpSetup(t)
	tenant := e.newTenant(t)
	rec := NewVersionRecorder(e.sdb, e.vers, e.gate, nil)
	raw := workflow.NewSQLRepository(e.sdb)
	wf := NewWorkflowService(NewDraftWorkflowRepo(raw, e.drafts, e.gate).WithRecorder(rec, NewWorkflowDocAdapter())).
		WithLiveRepo(raw).WithEntityChecker(e.meta).WithCache(e.cache)
	ctx, adminID := e.legacyAdmin(t, tenant, "Chị Lan")

	created, err := wf.CreateWorkflow(ctx, tenant, lockedVeto("trước giao"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.directEntries(t, tenant)); n != 0 {
		t.Fatalf("a tenant that is not delivered recorded %d direct entries", n)
	}

	e.deliver(t, tenant, uuid.NewString())
	off := false
	if _, err := wf.UpdateWorkflow(ctx, tenant, created.ID, asUpdate(lockedVeto("trước giao"), "sau giao", off)); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := e.sdb.Get(&name, `SELECT name FROM sys_workflows WHERE tenant_id = ? AND id = ?`, tenant, created.ID); err != nil || name != "sau giao" {
		t.Fatalf("live name = %q, %v (the write must apply at once)", name, err)
	}
	entries := e.directEntries(t, tenant)
	if len(entries) != 1 || entries[0].CreatedBy != adminID || entries[0].CreatedByLabel != "Chị Lan" || entries[0].VersionNo != nil {
		t.Fatalf("entries = %+v", entries)
	}
	var before, after string
	if err := e.sdb.QueryRow(`SELECT CAST(before_json AS CHAR), CAST(after_json AS CHAR) FROM sys_meta_version_docs WHERE tenant_id = ? AND version_id = ?`,
		tenant, entries[0].ID).Scan(&before, &after); err != nil {
		t.Fatal(err)
	}
	if before == "" || after == "" || before == after {
		t.Fatalf("before/after = %q / %q", before, after)
	}
}
