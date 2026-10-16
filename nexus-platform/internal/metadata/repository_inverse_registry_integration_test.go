//go:build integration

// ENG-REL2 #4 (bidirectional / inverse-field relationship registry, migration 000028)
// repository-level integration tests.
//
// These prove the RED-first bug the slice fixes: GetRelationshipByName /
// ListRelationshipsForEntity / ListAllRelationships used to hardcode
// `relationship_type = 'N-N'`, so a 1-N registry row (the ONLY kind this slice ever writes)
// would have been silently invisible to all three. They also prove the JoinTableName *string
// fix: a 1-N row's JoinTableName is stored as SQL NULL (not ""), so a SECOND 1-N row for the
// same tenant does not collide with the 000025 UNIQUE(tenant_id, join_table_name) index.
//
// Run with:
//
//	go test -tags=integration ./internal/metadata/
//
// Requires a reachable MySQL 8 with the sys_* tables migrated INCLUDING migration 000028
// (sys_relationships.inverse_field / child_fk_column). docker-compose dev DB.
package metadata

import (
	"context"
	"errors"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

const (
	invRegTenantA = "aaaaaaaa-1111-1111-1111-111111111111"
	invRegTenantB = "bbbbbbbb-2222-2222-2222-222222222222"
)

func invRegDSN() string {
	return repinDSN() // reuse the same env-or-default DSN helper as the repin suite.
}

func newInvRegRepo(t *testing.T) (*Repository, *gorm.DB) {
	t.Helper()
	gdb, err := gorm.Open(gormmysql.Open(invRegDSN()), &gorm.Config{})
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil || sqlDB.Ping() != nil {
		t.Skipf("MySQL not pingable — skipping")
	}
	return NewRepository(gdb), gdb
}

func cleanupInvReg(gdb *gorm.DB, names ...string) {
	for _, n := range names {
		gdb.Exec(`DELETE FROM sys_relationships WHERE name = ?`, n)
	}
}

// TestRepo_OneToNRelationship_RoundTripsThroughBroadenedLookups is the RED-first proof: before
// broadening, all three lookups hardcoded relationship_type='N-N' and would have returned
// ErrRelationshipNotFound / an empty slice for this 1-N row. After broadening they must all
// resolve it.
func TestRepo_OneToNRelationship_RoundTripsThroughBroadenedLookups(t *testing.T) {
	repo, gdb := newInvRegRepo(t)
	const name = "it_invreg_orders_customer"
	cleanupInvReg(gdb, name)
	t.Cleanup(func() { cleanupInvReg(gdb, name) })

	now := time.Now()
	rel := &RelationshipDefinition{
		ID:               "it-invreg-rel-1",
		TenantID:         invRegTenantA,
		Name:             name,
		ParentEntityName: "it_invreg_customers",
		ChildEntityName:  "it_invreg_orders",
		RelationshipType: "1-N",
		InverseField:     "it_invreg_orders",
		ChildFKColumn:    "customer_id",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := repo.CreateRelationship(context.Background(), rel); err != nil {
		t.Fatalf("seed 1-N relationship: %v", err)
	}

	t.Run("GetRelationshipByName", func(t *testing.T) {
		got, err := repo.GetRelationshipByName(context.Background(), invRegTenantA, name)
		if err != nil {
			t.Fatalf("GetRelationshipByName: %v (would be ErrRelationshipNotFound under the old N-N-only hardcode)", err)
		}
		if got.RelationshipType != "1-N" || got.InverseField != "it_invreg_orders" || got.ChildFKColumn != "customer_id" {
			t.Errorf("got = %+v; want RelationshipType=1-N InverseField=it_invreg_orders ChildFKColumn=customer_id", got)
		}
	})

	t.Run("ListRelationshipsForEntity_parent", func(t *testing.T) {
		rels, err := repo.ListRelationshipsForEntity(context.Background(), invRegTenantA, "it_invreg_customers")
		if err != nil {
			t.Fatalf("ListRelationshipsForEntity: %v", err)
		}
		if !containsRelName(rels, name) {
			t.Errorf("ListRelationshipsForEntity(parent) did not surface the 1-N row; got %d rows", len(rels))
		}
	})

	t.Run("ListRelationshipsForEntity_child", func(t *testing.T) {
		rels, err := repo.ListRelationshipsForEntity(context.Background(), invRegTenantA, "it_invreg_orders")
		if err != nil {
			t.Fatalf("ListRelationshipsForEntity: %v", err)
		}
		if !containsRelName(rels, name) {
			t.Errorf("ListRelationshipsForEntity(child) did not surface the 1-N row; got %d rows", len(rels))
		}
	})

	t.Run("ListAllRelationships", func(t *testing.T) {
		rels, err := repo.ListAllRelationships(context.Background(), invRegTenantA)
		if err != nil {
			t.Fatalf("ListAllRelationships: %v", err)
		}
		if !containsRelName(rels, name) {
			t.Errorf("ListAllRelationships did not surface the 1-N row; got %d rows", len(rels))
		}
	})
}

func containsRelName(rels []RelationshipDefinition, name string) bool {
	for _, r := range rels {
		if r.Name == name {
			return true
		}
	}
	return false
}

// TestRepo_GetRelationshipByInverseField_TenantScoped proves the new lookup resolves an
// active row by (tenant, parent, inverse_field) and is tenant-scoped: the SAME
// parent+inverse_field combination under a DIFFERENT tenant resolves to ErrRelationshipNotFound
// (never cross-tenant data).
func TestRepo_GetRelationshipByInverseField_TenantScoped(t *testing.T) {
	repo, gdb := newInvRegRepo(t)
	const name = "it_invreg_invoices_customer"
	cleanupInvReg(gdb, name)
	t.Cleanup(func() { cleanupInvReg(gdb, name) })

	now := time.Now()
	rel := &RelationshipDefinition{
		ID:               "it-invreg-rel-2",
		TenantID:         invRegTenantA,
		Name:             name,
		ParentEntityName: "it_invreg_customers2",
		ChildEntityName:  "it_invreg_invoices",
		RelationshipType: "1-N",
		InverseField:     "it_invreg_invoices",
		ChildFKColumn:    "customer_id",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := repo.CreateRelationship(context.Background(), rel); err != nil {
		t.Fatalf("seed 1-N relationship: %v", err)
	}

	got, err := repo.GetRelationshipByInverseField(context.Background(), invRegTenantA, "it_invreg_customers2", "it_invreg_invoices")
	if err != nil {
		t.Fatalf("GetRelationshipByInverseField (same tenant): %v", err)
	}
	if got.Name != name {
		t.Errorf("got.Name = %q; want %q", got.Name, name)
	}

	_, err = repo.GetRelationshipByInverseField(context.Background(), invRegTenantB, "it_invreg_customers2", "it_invreg_invoices")
	if !errors.Is(err, ErrRelationshipNotFound) {
		t.Errorf("cross-tenant GetRelationshipByInverseField: err = %v; want ErrRelationshipNotFound", err)
	}
}

// TestRepo_TwoOneToNRelationships_SameTenant_JoinTableNameNullDoesNotCollide is the
// discriminating test for the JoinTableName *string fix: TWO 1-N relationship rows for the
// SAME tenant (different fields/entities) must BOTH insert successfully. Under the old plain
// `string` JoinTableName, GORM would insert an empty string "" (not NULL) for both rows, and
// the 000025 UNIQUE(tenant_id, join_table_name) index would reject the SECOND insert with a
// 1062 duplicate — silently breaking the 1-N registry after the very first field. NULLs are
// distinct under a MySQL unique index, so both must succeed when JoinTableName is nil.
func TestRepo_TwoOneToNRelationships_SameTenant_JoinTableNameNullDoesNotCollide(t *testing.T) {
	repo, gdb := newInvRegRepo(t)
	const name1 = "it_invreg_nulljoin_orders_customer"
	const name2 = "it_invreg_nulljoin_invoices_customer"
	cleanupInvReg(gdb, name1)
	cleanupInvReg(gdb, name2)
	t.Cleanup(func() { cleanupInvReg(gdb, name1); cleanupInvReg(gdb, name2) })

	now := time.Now()
	rel1 := &RelationshipDefinition{
		ID:               "it-invreg-rel-nulljoin-1",
		TenantID:         invRegTenantA,
		Name:             name1,
		ParentEntityName: "it_invreg_nulljoin_customers",
		ChildEntityName:  "it_invreg_nulljoin_orders",
		RelationshipType: "1-N",
		InverseField:     "it_invreg_nulljoin_orders",
		ChildFKColumn:    "customer_id",
		CreatedAt:        now,
		UpdatedAt:        now,
		// JoinTableName intentionally left nil (zero value of *string) — mirrors how
		// AddField/CreateEntity build a 1-N registry row (no join table).
	}
	if err := repo.CreateRelationship(context.Background(), rel1); err != nil {
		t.Fatalf("first 1-N relationship insert (JoinTableName nil): %v", err)
	}

	rel2 := &RelationshipDefinition{
		ID:               "it-invreg-rel-nulljoin-2",
		TenantID:         invRegTenantA, // SAME tenant as rel1
		Name:             name2,
		ParentEntityName: "it_invreg_nulljoin_customers", // even the SAME parent
		ChildEntityName:  "it_invreg_nulljoin_invoices",
		RelationshipType: "1-N",
		InverseField:     "it_invreg_nulljoin_invoices", // different inverse field -> no collision there
		ChildFKColumn:    "customer_id",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := repo.CreateRelationship(context.Background(), rel2); err != nil {
		t.Fatalf("second 1-N relationship insert (same tenant, JoinTableName nil): %v — "+
			"if this fails with a 1062 duplicate on uq_sysrel_tenant_join_table, JoinTableName "+
			"is being stored as '' instead of NULL", err)
	}

	// Read back both rows directly and confirm join_table_name is SQL NULL, not "".
	var joinVal1, joinVal2 *string
	if err := gdb.Raw("SELECT join_table_name FROM sys_relationships WHERE id = ?", rel1.ID).Scan(&joinVal1).Error; err != nil {
		t.Fatalf("read back rel1 join_table_name: %v", err)
	}
	if joinVal1 != nil {
		t.Errorf("rel1 join_table_name = %v; want SQL NULL", *joinVal1)
	}
	if err := gdb.Raw("SELECT join_table_name FROM sys_relationships WHERE id = ?", rel2.ID).Scan(&joinVal2).Error; err != nil {
		t.Fatalf("read back rel2 join_table_name: %v", err)
	}
	if joinVal2 != nil {
		t.Errorf("rel2 join_table_name = %v; want SQL NULL", *joinVal2)
	}
}
