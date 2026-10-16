//go:build integration

package tenantbackup

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// checksums fingerprints every table of one customer: COUNT(*) and BIT_XOR(CRC32(all native
// columns)) pinned to tenant_id (a child through its parent). Read-only; shared by the "the check
// writes nothing" assertions here and by the restore slice.
func (e *env) checksums(c cust) map[string]string {
	e.t.Helper()
	tables, _, err := LoadSchema(context.Background(), e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	refs := map[string]TableRef{}
	for _, tb := range tables {
		r, _ := RuleFor(tb)
		refs[tb] = TableRef{Table: tb, Rule: r}
	}
	for tb, r := range rules {
		if r.Owner != nil {
			refs[tb] = TableRef{Table: tb, Rule: r}
		}
	}
	out := map[string]string{}
	for name, ref := range refs {
		cols, err := NativeColumns(context.Background(), e.db, name)
		if err != nil {
			continue // a policy name with no table in this schema
		}
		parts := make([]string, 0, len(cols))
		for _, col := range cols {
			x, err := projectExpr("`t`.`"+col.Name+"`", col.DataType, col.Name)
			if err != nil {
				e.t.Fatal(err)
			}
			parts = append(parts, x)
		}
		fw, err := ref.fromWhere()
		if err != nil {
			e.t.Fatal(err)
		}
		var row struct {
			N int64 `db:"n"`
			X int64 `db:"x"`
		}
		q := "SELECT COUNT(*) AS n, COALESCE(BIT_XOR(CRC32(CONCAT_WS('#', " + strings.Join(parts, ",") + "))), 0) AS x " + fw
		if err := e.db.Get(&row, q, c.ID); err != nil {
			e.t.Fatalf("checksum %s: %v", name, err)
		}
		out[name] = strconv.FormatInt(row.N, 10) + "/" + strconv.FormatInt(row.X, 10)
	}
	return out
}

func dirListing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			out = append(out, p+"|"+strconv.FormatInt(info.Size(), 10))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (e *env) preview(c cust, backupID string) (*RestorePlan, error) {
	e.svc.WithPlan(e.store, 0)
	return e.svc.Preview(context.Background(), PreviewInput{TenantID: c.ID, BackupID: backupID, CallerTenantID: e.W.ID})
}

func TestPreview_WritesNothing_Integration(t *testing.T) {
	e := newEnv(t, nil)
	e.putFile(e.A, "hello", false, "")
	b := e.create(e.A)
	if b.Status != StatusReady {
		t.Fatalf("backup status = %s", b.Status)
	}
	beforeA, beforeB := e.checksums(e.A), e.checksums(e.B)
	blobsBefore, bkBefore := dirListing(t, e.blobsDir), dirListing(t, e.backupsDir)

	plan, err := e.preview(e.A, b.ID)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !plan.Integrity.OK || !plan.Schema.OK || len(plan.Groups) == 0 {
		t.Errorf("plan = %+v", plan)
	}
	if plan.AccessChanges.Total != 0 {
		t.Errorf("nothing narrowed since the backup, got %+v", plan.AccessChanges)
	}
	afterA, afterB := e.checksums(e.A), e.checksums(e.B)
	for name, v := range beforeA {
		if afterA[name] != v {
			t.Errorf("A.%s changed by the check: %s -> %s", name, v, afterA[name])
		}
	}
	for name, v := range beforeB {
		if afterB[name] != v {
			t.Errorf("B.%s changed by the check", name)
		}
	}
	if strings.Join(blobsBefore, ",") != strings.Join(dirListing(t, e.blobsDir), ",") ||
		strings.Join(bkBefore, ",") != strings.Join(dirListing(t, e.backupsDir), ",") {
		t.Errorf("the file stores changed")
	}
}

func TestPreview_Refusals_Integration(t *testing.T) {
	e := newEnv(t, nil)
	bA := e.create(e.A)
	bB := e.create(e.B)

	t.Run("another customer's backup under this customer", func(t *testing.T) {
		_, err := e.preview(e.A, bB.ID)
		asErr(t, err, "BACKUP_NOT_FOUND")
	})
	t.Run("the workshop itself", func(t *testing.T) {
		e.svc.WithPlan(e.store, 0)
		_, err := e.svc.Preview(context.Background(), PreviewInput{TenantID: e.A.ID, BackupID: bA.ID, CallerTenantID: e.A.ID})
		asErr(t, err, "TENANT_RESTORE_SELF")
	})
	t.Run("one flipped byte", func(t *testing.T) {
		path := filepath.Join(e.backupsDir, *bA.StorageKey)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		flipped := append([]byte(nil), raw...)
		flipped[len(flipped)/2] ^= 0xff
		if err := os.WriteFile(path, flipped, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(path, raw, 0o600) })
		_, err = e.preview(e.A, bA.ID)
		asErr(t, err, "TENANT_BACKUP_CORRUPT")
	})
	t.Run("catalogue says an older schema", func(t *testing.T) {
		cur, _, _ := e.store.CurrentSchema(context.Background())
		e.exec("UPDATE sys_tenant_backups SET schema_version = ? WHERE id = ?", cur-1, bA.ID)
		t.Cleanup(func() { e.exec("UPDATE sys_tenant_backups SET schema_version = ? WHERE id = ?", cur, bA.ID) })
		_, err := e.preview(e.A, bA.ID)
		x := asErr(t, err, "TENANT_RESTORE_SCHEMA_MISMATCH")
		if x.Details["backup"] != cur-1 || x.Details["current"] != cur {
			t.Errorf("details = %v (current must be read from the database, not a constant)", x.Details)
		}
	})
	t.Run("a column the table lost", func(t *testing.T) {
		e.exec("ALTER TABLE `" + e.objTable + "` DROP COLUMN owner_id")
		_, err := e.preview(e.A, bA.ID)
		x := asErr(t, err, "TENANT_RESTORE_SCHEMA_DRIFT")
		if x.Details["table"] != e.objTable {
			t.Errorf("details = %v", x.Details)
		}
	})
}

func TestPreview_ForeignKeyDrift_Integration(t *testing.T) {
	e := newEnv(t, nil)
	b := e.create(e.A)
	tbl := "tbk_it_fk_" + randSuffix(t)
	e.exec("CREATE TABLE `" + tbl + "` (id CHAR(36) PRIMARY KEY, tenant_id CHAR(36) NOT NULL, role_id CHAR(36) NOT NULL, " +
		"CONSTRAINT fk_" + tbl + " FOREIGN KEY (role_id) REFERENCES acl_roles(id) ON DELETE CASCADE)")
	rules[tbl] = Rule{Class: KeepExport}
	t.Cleanup(func() {
		delete(rules, tbl)
		_, _ = e.db.Exec("DROP TABLE IF EXISTS `" + tbl + "`")
	})
	_, err := e.preview(e.A, b.ID)
	asErr(t, err, "TENANT_RESTORE_SCHEMA_DRIFT")
}

func TestPreview_RetiredCustomer_Integration(t *testing.T) {
	e := newEnv(t, nil)
	b := e.create(e.A)
	e.exec("UPDATE tenants SET deleted_at = NOW() WHERE id = ?", e.A.ID)
	_, err := e.preview(e.A, b.ID)
	asErr(t, err, "TENANT_RESTORE_RETIRED")
}

// SC-002a: the access narrowed AFTER the backup shows up per kind, and the check still writes nothing.
func TestPreview_AccessChangesPerKind_Integration(t *testing.T) {
	e := newEnv(t, nil)
	var roleID string
	if err := e.db.Get(&roleID, "SELECT id FROM acl_roles WHERE tenant_id = ?", e.A.ID); err != nil {
		t.Fatal(err)
	}
	// state that exists at backup time
	shareID, entID, fieldID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	e.exec("INSERT INTO sys_record_shares (id, tenant_id, entity_name, record_id, user_id, shared_by, created_by) VALUES (?, ?, 'accounts', ?, ?, ?, ?)",
		shareID, e.A.ID, uuid.NewString(), e.uA, e.opUser, e.opUser)
	e.exec("INSERT INTO sys_entities (id, tenant_id, name, label, table_name) VALUES (?, ?, ?, 'X', ?)", entID, e.A.ID, "tbk_ent_"+randSuffix(t), "obj_tbk_none_"+randSuffix(t))
	e.exec("INSERT INTO sys_fields (id, entity_id, name, label, field_type, options, tenant_id) VALUES (?, ?, 'secret', 'Secret', 'text', '{}', ?)", fieldID, entID, e.A.ID)
	b := e.create(e.A)

	// narrow six kinds after the backup
	e.exec("UPDATE acl_role_actions SET access_level = 'owner' WHERE role_id = ? ORDER BY entity_name LIMIT 1", roleID) // owner now, all in the backup: same row count
	e.exec("DELETE FROM acl_role_actions WHERE role_id = ? AND entity_name = 'contacts_a'", roleID)                     // a grant removed
	e.exec("INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit) VALUES (?, ?, ?, 'accounts', 'salary', 0, 0)",
		uuid.NewString(), e.A.ID, roleID) // a hidden field (no row in the backup = open)
	e.exec("UPDATE sys_record_shares SET deleted_at = NOW() WHERE id = ?", shareID)
	e.exec("DELETE FROM team_memberships WHERE user_id = ?", e.uA)
	e.exec("UPDATE sys_fields SET options = '{\"sensitive\": true}' WHERE id = ?", fieldID)
	e.exec("UPDATE sys_workflows SET is_active = 0 WHERE tenant_id = ?", e.A.ID)

	before := e.checksums(e.A)
	plan, err := e.preview(e.A, b.ID)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	ac := plan.AccessChanges
	if ac.RoleGrantsWidened != 2 || ac.FieldACLReopened != 1 || ac.SharesRegranted != 1 || ac.MembershipsReadded != 1 ||
		ac.SensitiveFieldsCleared != 1 || ac.WorkflowsReenabled != 1 || ac.OutboundDestinationsReenabled != 1 {
		t.Errorf("accessChanges = %+v", ac)
	}
	if ac.Total != 8 || ac.UsersAffected != 1 || ac.APIKeysAffected != 1 {
		t.Errorf("totals = %+v", ac)
	}
	if len(ac.OutboundHosts) != 1 || ac.OutboundHosts[0] != "partner.example" {
		t.Errorf("hosts = %v", ac.OutboundHosts)
	}
	for k, v := range e.checksums(e.A) {
		if before[k] != v {
			t.Errorf("A.%s changed by the check", k)
		}
	}
}
