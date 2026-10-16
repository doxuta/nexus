//go:build integration

// REL-RM21-NN repository-level integration suite: SoftDeleteEntity's broadened relationship
// sweep (EITHER side, ANY type).
//
// RED-first bug this proves fixed: step 3 of SoftDeleteEntity used to tombstone + soft-delete
// ONLY rows where the deleted entity was the CHILD of a 1-N/1-1. Rows where it was the PARENT
// and ALL N-N rows (either side) were left ACTIVE — phantom registry rows that kept surfacing
// through ListAllRelationships / ListRelationshipsForEntity → GET /metadata/relationships →
// ghost edges in the Studio ERD + Related tab, AND kept squatting both UNIQUE slots
// (uq_sysrel_tenant_name, uq_sysrel_tenant_join_table — migration 000025, both span
// soft-deleted rows), bricking a same-name re-registration with a 1062 duplicate.
//
// Run with:
//
//	go test -tags=integration ./internal/metadata/
//
// Requires a reachable MySQL 8 with the sys_* tables migrated INCLUDING migration 000025
// (unique indexes) and 000028 (inverse_field / child_fk_column). docker-compose dev DB.
package metadata

import (
	"context"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// rm21 fixture name constants — every seeded row is prefixed "it_rm21" so cleanup can sweep
// by prefix without touching other suites' fixtures.
const (
	rm21EntA = "it_rm21_a"
	rm21EntB = "it_rm21_b"
	rm21EntC = "it_rm21_c"

	rm21RelAB1N = "it_rm21_b_a"     // A→B 1-N (A is PARENT — the previously-leaked side)
	rm21RelCA1N = "it_rm21_a_c"     // C→A 1-N (A is CHILD — the side already covered pre-slice)
	rm21RelABNN = "it_rm21_a_b_nn"  // A↔B N-N (previously leaked entirely)
	rm21RelCB1N = "it_rm21_b_c"     // C→B 1-N control — does NOT involve A, must survive
	rm21JoinAB  = "rel_it_rm21_a_b" // N-N join table name (both tenants use the same value —
	// uq_sysrel_tenant_join_table is per-tenant)
)

// rm21Seed creates, for ONE tenant, the 3 entities and 4 relationship rows of the fixture and
// returns the entity A row id. Relationship ids are chosen so their first-8-char tombstone
// suffixes are distinct and recognizable per row.
func rm21Seed(t *testing.T, repo *Repository, tenant, tag string) (entAID string, relIDs map[string]string) {
	t.Helper()
	gdb := repo.db
	now := time.Now()

	entAID = "rm21ea" + tag
	ents := []EntityDefinition{
		{ID: entAID, TenantID: tenant, Name: rm21EntA, Label: "RM21 A", DataTable: "obj_" + rm21EntA, CreatedAt: now, UpdatedAt: now},
		{ID: "rm21eb" + tag, TenantID: tenant, Name: rm21EntB, Label: "RM21 B", DataTable: "obj_" + rm21EntB, CreatedAt: now, UpdatedAt: now},
		{ID: "rm21ec" + tag, TenantID: tenant, Name: rm21EntC, Label: "RM21 C", DataTable: "obj_" + rm21EntC, CreatedAt: now, UpdatedAt: now},
	}
	for i := range ents {
		if err := gdb.Create(&ents[i]).Error; err != nil {
			t.Fatalf("seed entity %s (%s): %v", ents[i].Name, tenant, err)
		}
	}

	join := rm21JoinAB
	rels := []RelationshipDefinition{
		// A is PARENT of a 1-N — the side the pre-slice sweep leaked.
		{ID: "rm21ab1n-" + tag, TenantID: tenant, Name: rm21RelAB1N,
			ParentEntityName: rm21EntA, ChildEntityName: rm21EntB, RelationshipType: "1-N",
			InverseField: rm21EntB, ChildFKColumn: "a_id", CreatedAt: now, UpdatedAt: now},
		// A is CHILD of a 1-N — the side already covered before this slice (must keep working).
		{ID: "rm21ca1n-" + tag, TenantID: tenant, Name: rm21RelCA1N,
			ParentEntityName: rm21EntC, ChildEntityName: rm21EntA, RelationshipType: "1-N",
			InverseField: rm21EntA, ChildFKColumn: "c_id", CreatedAt: now, UpdatedAt: now},
		// A↔B N-N — leaked entirely before this slice; carries BOTH unique slots.
		{ID: "rm21abnn-" + tag, TenantID: tenant, Name: rm21RelABNN,
			ParentEntityName: rm21EntA, ChildEntityName: rm21EntB, RelationshipType: "N-N",
			JoinTableName: &join, CreatedAt: now, UpdatedAt: now},
		// Control: C→B does NOT involve A and must survive A's delete untouched.
		{ID: "rm21cb1n-" + tag, TenantID: tenant, Name: rm21RelCB1N,
			ParentEntityName: rm21EntC, ChildEntityName: rm21EntB, RelationshipType: "1-N",
			InverseField: rm21EntB, ChildFKColumn: "c_id", CreatedAt: now, UpdatedAt: now},
	}
	relIDs = make(map[string]string, len(rels))
	for i := range rels {
		if err := repo.CreateRelationship(context.Background(), &rels[i]); err != nil {
			t.Fatalf("seed relationship %s (%s): %v", rels[i].Name, tenant, err)
		}
		relIDs[rels[i].Name] = rels[i].ID
	}
	return entAID, relIDs
}

// rm21Row is the raw (soft-delete-unscoped) shape of a sys_relationships row under test.
type rm21Row struct {
	Name          string     `gorm:"column:name"`
	JoinTableName *string    `gorm:"column:join_table_name"`
	DeletedAt     *time.Time `gorm:"column:deleted_at"`
}

func rm21Load(t *testing.T, repo *Repository, id string) rm21Row {
	t.Helper()
	var row rm21Row
	if err := repo.db.Raw(
		"SELECT name, join_table_name, deleted_at FROM sys_relationships WHERE id = ?", id,
	).Scan(&row).Error; err != nil {
		t.Fatalf("load relationship row %s: %v", id, err)
	}
	return row
}

// TestRepo_SoftDeleteEntity_SweepsRelationshipsEitherSideAnyType is the REL-RM21-NN core
// proof. Fixture (tenant A): entities A, B, C; relationships A→B 1-N (A parent), C→A 1-N
// (A child), A↔B N-N, plus a C→B 1-N control. Soft-deleting entity A must tombstone +
// soft-delete ALL 3 rows involving A (either side, any type), leave the control row active,
// hide the 3 rows from ListAllRelationships, free BOTH unique slots so an identical-name N-N
// re-registers cleanly, and leave an identically-named second tenant completely untouched.
func TestRepo_SoftDeleteEntity_SweepsRelationshipsEitherSideAnyType(t *testing.T) {
	repo, gdb := newInvRegRepo(t)
	ctx := context.Background()

	cleanup := func() {
		// Prefix match sweeps tombstoned variants too; tenant-pinned defense-in-depth.
		gdb.Exec("DELETE FROM sys_relationships WHERE tenant_id IN (?, ?) AND name LIKE 'it\\_rm21\\_%'",
			invRegTenantA, invRegTenantB)
		gdb.Exec("DELETE FROM sys_entities WHERE tenant_id IN (?, ?) AND name LIKE 'it\\_rm21\\_%'",
			invRegTenantA, invRegTenantB)
	}
	cleanup()
	t.Cleanup(cleanup)

	entAID, relIDs := rm21Seed(t, repo, invRegTenantA, "1")
	_, relIDsB := rm21Seed(t, repo, invRegTenantB, "2") // identical names under a second tenant

	if err := repo.SoftDeleteEntity(ctx, invRegTenantA, entAID); err != nil {
		t.Fatalf("SoftDeleteEntity(%s): %v", rm21EntA, err)
	}

	t.Run("all_three_rows_involving_A_tombstoned_and_soft_deleted", func(t *testing.T) {
		for _, name := range []string{rm21RelAB1N, rm21RelCA1N, rm21RelABNN} {
			id := relIDs[name]
			row := rm21Load(t, repo, id)
			if row.DeletedAt == nil {
				t.Errorf("%s: deleted_at IS NULL — row still ACTIVE (phantom edge survives)", name)
			}
			wantName := tombstoneName(name, 150, tombstoneSuffix(id))
			if row.Name != wantName {
				t.Errorf("%s: stored name = %q; want tombstone %q", name, row.Name, wantName)
			}
			if name == rm21RelABNN {
				if row.JoinTableName == nil {
					t.Errorf("%s: join_table_name IS NULL; want tombstoned value", name)
				} else if want := tombstoneName(rm21JoinAB, 100, tombstoneSuffix(id)); *row.JoinTableName != want {
					t.Errorf("%s: join_table_name = %q; want tombstone %q", name, *row.JoinTableName, want)
				}
			} else if row.JoinTableName != nil {
				t.Errorf("%s: join_table_name = %q; want SQL NULL kept for a 1-N row", name, *row.JoinTableName)
			}
		}
	})

	t.Run("control_row_not_involving_A_untouched", func(t *testing.T) {
		row := rm21Load(t, repo, relIDs[rm21RelCB1N])
		if row.DeletedAt != nil {
			t.Errorf("control C→B row was soft-deleted; the sweep over-matched")
		}
		if row.Name != rm21RelCB1N {
			t.Errorf("control C→B row name = %q; want untouched %q", row.Name, rm21RelCB1N)
		}
	})

	t.Run("list_all_relationships_shows_no_phantom_edges", func(t *testing.T) {
		all, err := repo.ListAllRelationships(ctx, invRegTenantA)
		if err != nil {
			t.Fatalf("ListAllRelationships: %v", err)
		}
		for _, name := range []string{rm21RelAB1N, rm21RelCA1N, rm21RelABNN} {
			if containsRelName(all, name) {
				t.Errorf("ListAllRelationships still advertises %q — ghost ERD edge", name)
			}
		}
		if !containsRelName(all, rm21RelCB1N) {
			t.Errorf("ListAllRelationships lost the unrelated control row %q", rm21RelCB1N)
		}
	})

	t.Run("identical_name_nn_reregisters_after_recreate", func(t *testing.T) {
		// Recreate entity A under its ORIGINAL name (the entity tombstone freed that slot),
		// then re-register the SAME-name, SAME-join-table N-N. Both sys_relationships unique
		// slots (tenant_id+name, tenant_id+join_table_name) must have been freed by the sweep
		// — before this slice the INSERT died with a 1062 duplicate → 409, no recovery.
		now := time.Now()
		reborn := EntityDefinition{ID: "rm21ea1r", TenantID: invRegTenantA, Name: rm21EntA,
			Label: "RM21 A reborn", DataTable: "obj_" + rm21EntA, CreatedAt: now, UpdatedAt: now}
		if err := gdb.Create(&reborn).Error; err != nil {
			t.Fatalf("recreate entity %s: %v", rm21EntA, err)
		}
		join := rm21JoinAB
		again := RelationshipDefinition{ID: "rm21abnn-1r", TenantID: invRegTenantA, Name: rm21RelABNN,
			ParentEntityName: rm21EntA, ChildEntityName: rm21EntB, RelationshipType: "N-N",
			JoinTableName: &join, CreatedAt: now, UpdatedAt: now}
		if err := repo.CreateRelationship(ctx, &again); err != nil {
			t.Fatalf("re-register identical-name N-N after entity delete: %v — a 1062 duplicate "+
				"here means the sweep did not tombstone name/join_table_name (unique slots still squatted)", err)
		}
	})

	t.Run("second_tenant_with_identical_names_untouched", func(t *testing.T) {
		for _, name := range []string{rm21RelAB1N, rm21RelCA1N, rm21RelABNN, rm21RelCB1N} {
			row := rm21Load(t, repo, relIDsB[name])
			if row.DeletedAt != nil {
				t.Errorf("tenant-B row %q was soft-deleted by tenant-A's entity delete — tenant bleed", name)
			}
			if row.Name != name || strings.Contains(row.Name, "__deleted_") {
				t.Errorf("tenant-B row %q name = %q; want untouched", name, row.Name)
			}
		}
	})
}
