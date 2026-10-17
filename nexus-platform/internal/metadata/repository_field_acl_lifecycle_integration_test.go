//go:build integration

// Queue #322 — sys_field_acl rows must not outlive the field/entity they restrict. The rows are
// keyed by NAME (entity_name, field_name), so a row left behind by a delete was silently adopted
// by whatever got created next under the same name (Studio re-create, vertical-pack reinstall),
// skipping both dead-end guards (PutGrants / EditField). SoftDeleteField and SoftDeleteEntity now
// drop the rows in the SAME transaction as the tombstone.
//
// Run with: go test -tags=integration ./internal/metadata/ -run FieldACL
package metadata

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
)

const (
	faclTenantA = "55555555-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	faclTenantB = "66666666-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	faclEntity  = "it_facl_widgets"
)

// seedFieldACLFixture: for each tenant an entity with fields status+note, one role, and two
// restriction rows (status hidden, note read-only). Returns A's entity id and field ids.
func seedFieldACLFixture(t *testing.T, gdb *gorm.DB) (entA, statusA, noteA string) {
	t.Helper()
	cleanup := func() {
		gdb.Exec(`DELETE FROM sys_field_acl WHERE tenant_id IN (?, ?)`, faclTenantA, faclTenantB)
		gdb.Exec(`DELETE FROM acl_roles WHERE tenant_id IN (?, ?)`, faclTenantA, faclTenantB)
		gdb.Exec(`DELETE FROM sys_fields WHERE tenant_id IN (?, ?)`, faclTenantA, faclTenantB)
		gdb.Exec(`DELETE FROM sys_entities WHERE tenant_id IN (?, ?)`, faclTenantA, faclTenantB)
		gdb.Exec(`DELETE FROM tenants WHERE id IN (?, ?)`, faclTenantA, faclTenantB)
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now()
	for _, tn := range [][2]string{{faclTenantA, "it-facl-a"}, {faclTenantB, "it-facl-b"}} {
		tenant, tag := tn[0], tn[1]
		if err := gdb.Exec(`INSERT INTO tenants (id, slug, name) VALUES (?, ?, ?)`, tenant, tag, "IT FieldACL").Error; err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
		roleID := "facl-role-" + tag
		if err := gdb.Exec(`INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, 'rep', 'rep')`, roleID, tenant).Error; err != nil {
			t.Fatalf("seed role: %v", err)
		}
		entID := "facl-ent-" + tag
		if err := gdb.Create(&EntityDefinition{
			ID: entID, TenantID: tenant, Name: faclEntity, Label: "W", DataTable: "obj_" + faclEntity, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		for _, fn := range []string{"status", "note"} {
			if err := gdb.Create(&FieldDefinition{
				ID: "facl-fld-" + fn + "-" + tag, EntityID: entID, TenantID: tenant, Name: fn, Label: fn, FieldType: "text", CreatedAt: now, UpdatedAt: now,
			}).Error; err != nil {
				t.Fatalf("seed field: %v", err)
			}
		}
		rows := []struct {
			field    string
			canView  bool
			canEdit  bool
			idSuffix string
		}{{"status", false, false, "s"}, {"note", true, false, "n"}}
		for _, r := range rows {
			if err := gdb.Exec(`INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, "facl-row-"+r.idSuffix+"-"+tag, tenant, roleID, faclEntity, r.field, r.canView, r.canEdit).Error; err != nil {
				t.Fatalf("seed acl row: %v", err)
			}
		}
	}
	return "facl-ent-it-facl-a", "facl-fld-status-it-facl-a", "facl-fld-note-it-facl-a"
}

func faclCount(t *testing.T, gdb *gorm.DB, tenant, field string) int64 {
	t.Helper()
	var n int64
	q := gdb.Table("sys_field_acl").Where("tenant_id = ? AND entity_name = ?", tenant, faclEntity)
	if field != "" {
		q = q.Where("field_name = ?", field)
	}
	if err := q.Count(&n).Error; err != nil {
		t.Fatalf("count acl rows: %v", err)
	}
	return n
}

// Deleting a locked field drops its ACL rows. Only that field's rows go; its sibling's row and the
// other tenant's row stay.
func TestRepo_SoftDeleteField_DropsFieldACLRows_IT(t *testing.T) {
	repo, gdb := newRepinRepo(t)
	_, statusA, _ := seedFieldACLFixture(t, gdb)
	ctx := context.Background()

	if err := repo.SoftDeleteField(ctx, faclTenantA, statusA); err != nil {
		t.Fatalf("SoftDeleteField: %v", err)
	}
	if n := faclCount(t, gdb, faclTenantA, "status"); n != 0 {
		t.Fatalf("status ACL rows after delete = %d; want 0 (a recreated field would inherit the hide)", n)
	}
	if n := faclCount(t, gdb, faclTenantA, "note"); n != 1 {
		t.Fatalf("sibling field's ACL rows = %d; want 1 (untouched)", n)
	}
	if n := faclCount(t, gdb, faclTenantB, "status"); n != 1 {
		t.Fatalf("other tenant's status ACL rows = %d; want 1 (tenant-pinned)", n)
	}

	// (Re-creating the same field name on the same entity is blocked today by the 000012 unique
	// key, which spans soft-deleted rows — so the visible harm of a stale field row is the phantom
	// matrix cell / dead-end list entry, and the re-attach path is the entity one below. The
	// cleanup holds either way, should that key ever be relaxed.)

	// A second delete of the (already gone) field is ErrFieldNotFound and must not touch rows.
	if err := repo.SoftDeleteField(ctx, faclTenantA, statusA); err != ErrFieldNotFound {
		t.Fatalf("second SoftDeleteField err = %v; want ErrFieldNotFound", err)
	}
	if n := faclCount(t, gdb, faclTenantA, "note"); n != 1 {
		t.Fatalf("sibling ACL rows after no-op delete = %d; want 1", n)
	}
}

// Soft-delete the entity, create/install it again: no field of the new entity starts restricted.
func TestRepo_SoftDeleteEntity_DropsFieldACLRows_IT(t *testing.T) {
	repo, gdb := newRepinRepo(t)
	entA, _, _ := seedFieldACLFixture(t, gdb)

	if err := repo.SoftDeleteEntity(context.Background(), faclTenantA, entA); err != nil {
		t.Fatalf("SoftDeleteEntity: %v", err)
	}
	if n := faclCount(t, gdb, faclTenantA, ""); n != 0 {
		t.Fatalf("entity ACL rows after delete = %d; want 0 (a reinstalled pack would inherit them)", n)
	}
	if n := faclCount(t, gdb, faclTenantB, ""); n != 2 {
		t.Fatalf("other tenant's ACL rows = %d; want 2 (tenant-pinned)", n)
	}
}

// Issue #2, verifier round 1: acl_role_actions rows are keyed by the entity NAME too. A table deleted
// and re-created under that name (Studio, vertical pack, the assistant) silently inherited every role's
// view/create/edit/delete on it. SoftDeleteEntity drops them in the SAME transaction; the role is pinned
// to the tenant through acl_roles, so the other tenant's grants on the same name stay.
func TestRepo_SoftDeleteEntity_DropsRoleGrants_IT(t *testing.T) {
	repo, gdb := newRepinRepo(t)
	entA, _, _ := seedFieldACLFixture(t, gdb)
	grant := func(id, roleID, entity, action string) {
		t.Helper()
		if err := gdb.Exec(`INSERT INTO acl_role_actions (id, role_id, entity_name, action, access_level) VALUES (?, ?, ?, ?, 'all')`,
			id, roleID, entity, action).Error; err != nil {
			t.Fatalf("seed grant: %v", err)
		}
	}
	grant("facl-ga-1", "facl-role-it-facl-a", faclEntity, "view")
	grant("facl-ga-2", "facl-role-it-facl-a", faclEntity, "edit")
	grant("facl-ga-3", "facl-role-it-facl-a", "it_facl_other", "view")
	grant("facl-gb-1", "facl-role-it-facl-b", faclEntity, "view")
	count := func(roleID, entity string) int64 {
		t.Helper()
		var n int64
		if err := gdb.Table("acl_role_actions").Where("role_id = ? AND entity_name = ?", roleID, entity).Count(&n).Error; err != nil {
			t.Fatalf("count grants: %v", err)
		}
		return n
	}

	if err := repo.SoftDeleteEntity(context.Background(), faclTenantA, entA); err != nil {
		t.Fatalf("SoftDeleteEntity: %v", err)
	}
	if n := count("facl-role-it-facl-a", faclEntity); n != 0 {
		t.Fatalf("grants on the deleted table = %d; want 0 (a re-created table of that name would inherit them)", n)
	}
	if n := count("facl-role-it-facl-a", "it_facl_other"); n != 1 {
		t.Fatalf("the role's grant on another table = %d; want 1 (untouched)", n)
	}
	if n := count("facl-role-it-facl-b", faclEntity); n != 1 {
		t.Fatalf("the other tenant's grant on the same name = %d; want 1 (tenant-pinned)", n)
	}
}
