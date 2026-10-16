//go:build integration

// Integration test for Repository.ListRoleNames (the AI Builder's list_roles grounding read,
// 86d596fa) against the REAL migrated schema. The unit tests cover it only through the ai
// package's in-memory fake reader, which is exactly how the original defect shipped: the query
// addressed a `roles` table (and a `deleted_at` column) that exist nowhere in the migrations —
// RBAC roles live in acl_roles (000005) and are hard-deleted. Against real MySQL every call
// failed Error 1146 and the tool degraded to ROLES_READ_FAILED, silently defeating its purpose
// (the model went back to guessing role names). This suite pins: (a) the query runs on the real
// schema at all, (b) tenant isolation, (c) ascending name order.
package metadata

import (
	"context"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

const (
	lrnTenantA = "33333333-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	lrnTenantB = "44444444-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

// seedListRoleNamesFixture creates tenants A and B (acl_roles FKs tenants) plus two roles for A
// and one for B, and registers cleanup.
func seedListRoleNamesFixture(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	cleanup := func() {
		gdb.Exec(`DELETE FROM acl_roles WHERE tenant_id IN (?, ?)`, lrnTenantA, lrnTenantB)
		gdb.Exec(`DELETE FROM tenants WHERE id IN (?, ?)`, lrnTenantA, lrnTenantB)
	}
	cleanup()
	t.Cleanup(cleanup)

	for _, row := range [][2]string{{lrnTenantA, "it-lrn-a"}, {lrnTenantB, "it-lrn-b"}} {
		if err := gdb.Exec(`INSERT INTO tenants (id, slug, name) VALUES (?, ?, ?)`,
			row[0], row[1], "IT ListRoleNames").Error; err != nil {
			t.Fatalf("seed tenant %s: %v", row[0], err)
		}
	}
	roles := []struct{ id, tenant, name string }{
		{"lrn-role-a2", lrnTenantA, "sales_rep"},
		{"lrn-role-a1", lrnTenantA, "manager"},
		{"lrn-role-b1", lrnTenantB, "accountant"},
	}
	for _, r := range roles {
		if err := gdb.Exec(`INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, ?, ?)`,
			r.id, r.tenant, r.name, r.name).Error; err != nil {
			t.Fatalf("seed role %s: %v", r.name, err)
		}
	}
}

// TestRepo_ListRoleNames_IT proves the read works on the real acl_roles table, returns ONLY the
// caller tenant's role names, ascending — and never another tenant's.
func TestRepo_ListRoleNames_IT(t *testing.T) {
	repo, gdb := newRepinRepo(t)
	seedListRoleNamesFixture(t, gdb)

	names, err := repo.ListRoleNames(context.Background(), lrnTenantA)
	if err != nil {
		t.Fatalf("ListRoleNames: %v", err)
	}
	if len(names) != 2 || names[0] != "manager" || names[1] != "sales_rep" {
		t.Fatalf("want [manager sales_rep], got %v", names)
	}
	for _, n := range names {
		if n == "accountant" {
			t.Fatalf("tenant B's role leaked into tenant A's list: %v", names)
		}
	}
}
