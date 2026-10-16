//go:build integration

package tenantbackup

import (
	"errors"
	"strings"
	"testing"
)

// FR-068 / T010(a): on the schema golang-migrate builds, every table with tenant_id has a rule,
// every tenant-less child has an Owner, and every FK between classes obeys the restore rules.
// This is the gate that goes red IN THE COMMIT of whichever lane adds an unclassified table.
func TestCheckSchema_MigratedSchemaIsClassified(t *testing.T) {
	db := itDB(t)
	if err := CheckSchema(ctxT(t), db); err != nil {
		t.Fatalf("CheckSchema on the migrated schema: %v", err)
	}
}

// An unclassified table is refused with its name; the table is temporary and cleaned up.
func TestCheckSchema_RefusesAnUnclassifiedTable(t *testing.T) {
	db := itDB(t)
	name := "tbk_it_" + randSuffix(t)
	if _, err := db.Exec("CREATE TABLE `" + name + "` (id CHAR(36) NOT NULL PRIMARY KEY, tenant_id CHAR(36) NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS `" + name + "`") })

	err := CheckSchema(ctxT(t), db)
	var e *Error
	if !errors.As(err, &e) || e.Code != "TENANT_BACKUP_SCHEMA_UNCLASSIFIED" {
		t.Fatalf("want TENANT_BACKUP_SCHEMA_UNCLASSIFIED, got %v", err)
	}
	if !strings.Contains(e.Msg, name) {
		t.Errorf("the refusal must name the table %s: %s", name, e.Msg)
	}
}

// A KeepExport table whose FK CASCADE-deletes from a Restore parent would be wiped by a restore:
// drift, refused. The rule for the temp table is injected for the test only.
func TestCheckSchema_RefusesCascadeFromKeepIntoRestore(t *testing.T) {
	db := itDB(t)
	name := "tbk_it_" + randSuffix(t)
	if _, err := db.Exec("CREATE TABLE `" + name + "` (id CHAR(36) NOT NULL PRIMARY KEY, tenant_id CHAR(36) NOT NULL, " +
		"role_id CHAR(36) NULL, CONSTRAINT `fk_" + name + "` FOREIGN KEY (role_id) REFERENCES acl_roles(id) ON DELETE CASCADE)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS `" + name + "`") })
	undo := registerTestRule(name, Rule{Class: KeepExport})
	t.Cleanup(undo)

	err := CheckSchema(ctxT(t), db)
	var e *Error
	if !errors.As(err, &e) || e.Code != "TENANT_RESTORE_SCHEMA_DRIFT" {
		t.Fatalf("want TENANT_RESTORE_SCHEMA_DRIFT, got %v", err)
	}
	if !strings.Contains(e.Msg, name) {
		t.Errorf("the refusal must name the table %s: %s", name, e.Msg)
	}
}
