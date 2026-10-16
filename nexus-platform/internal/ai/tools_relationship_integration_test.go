//go:build integration

// Integration tests for the M5 relationship tools against the REAL *metadata.Repository — the
// production MetadataReader. They prove what the in-memory fake cannot: (a) *metadata.Repository
// structurally satisfies the widened MetadataReader interface (ListAllRelationships) with no
// adapter, (b) list_relationships is tenant-scoped by the actual SQL predicates, and (c)
// propose_relationship's entity-existence parity check runs against real sys_entities rows —
// cross-tenant entities are indistinguishable from missing ones.
//
// Run with (throwaway DB migrated per the gate recipe — single-source TEST_DATABASE_URL, B7):
//
//	mysql -h127.0.0.1 -uroot -e "CREATE DATABASE IF NOT EXISTS nexus_it"
//	/opt/homebrew/bin/migrate -path migrations -database "mysql://root@tcp(127.0.0.1:3306)/nexus_it" up
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_it?parseTime=true&multiStatements=true&charset=utf8mb4" \
//	  go test -tags=integration ./internal/ai/ -run TestToolsRelationship_IT -count=1
package ai

import (
	"context"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/metadata"
)

const (
	relITTenantA = "aiaiaiai-m5m5-4444-8888-tenanta00001"
	relITTenantB = "aiaiaiai-m5m5-4444-8888-tenantb00002"
	relITEntityA = "it_ai_m5_students" // 17 chars — within SanitizeIdentifier's 26-char cap
	relITEntityB = "it_ai_m5_courses"  // 16 chars
	relITRelName = "it_ai_m5_students_courses"
)

// newRelITReader connects GORM over the same single-source DSN the ai ledger integration tests
// use (aiTestDSN, runs_integration_test.go) and returns the REAL metadata repository.
func newRelITReader(t *testing.T) (*metadata.Repository, *gorm.DB) {
	t.Helper()
	gdb, err := gorm.Open(gormmysql.Open(aiTestDSN()), &gorm.Config{})
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping M5 relationship-tool integration", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil || sqlDB.Ping() != nil {
		t.Skipf("MySQL not pingable — skipping M5 relationship-tool integration")
	}
	return metadata.NewRepository(gdb), gdb
}

// cleanupRelIT hard-deletes every row the suite seeds, keyed by its throwaway tenant IDs, so
// reruns are idempotent and nothing leaks into other suites sharing the DB.
func cleanupRelIT(gdb *gorm.DB) {
	gdb.Exec(`DELETE FROM sys_relationships WHERE tenant_id IN (?, ?)`, relITTenantA, relITTenantB)
	gdb.Exec(`DELETE FROM sys_entities WHERE tenant_id IN (?, ?)`, relITTenantA, relITTenantB)
}

// seedRelIT registers the two tenant-A entities and one N-N relationship between them.
func seedRelIT(t *testing.T, gdb *gorm.DB, repo *metadata.Repository) {
	t.Helper()
	cleanupRelIT(gdb)
	t.Cleanup(func() { cleanupRelIT(gdb) })

	for i, name := range []string{relITEntityA, relITEntityB} {
		ent := metadata.EntityDefinition{
			ID:        "itai-m5e" + string(rune('1'+i)) + "-0000-0000-000000000001",
			TenantID:  relITTenantA,
			Name:      name,
			Label:     "M5 IT " + name,
			DataTable: "obj_" + name,
		}
		if err := gdb.Create(&ent).Error; err != nil {
			t.Fatalf("seed entity %s: %v", name, err)
		}
	}

	join := "rel_" + relITEntityB + "_" + relITEntityA // canonical alphabetical order (courses < students)
	rel := &metadata.RelationshipDefinition{
		ID:               "itai-m5r1-0000-0000-000000000001",
		TenantID:         relITTenantA,
		Name:             relITRelName,
		ParentEntityName: relITEntityA,
		ChildEntityName:  relITEntityB,
		RelationshipType: "N-N",
		JoinTableName:    &join,
	}
	if err := repo.CreateRelationship(context.Background(), rel); err != nil {
		t.Fatalf("seed N-N relationship: %v", err)
	}
}

func TestToolsRelationship_IT_ListAndTenantIsolation(t *testing.T) {
	repo, gdb := newRelITReader(t)
	seedRelIT(t, gdb, repo)
	ctx := context.Background()

	// Tenant A sees the seeded N-N through the REAL repository (no adapter).
	execA := &toolExecutor{reader: repo, tenantID: relITTenantA}
	res := execA.execute(ctx, "list_relationships", "{}")
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("list_relationships (tenant A) should succeed: %s", res)
	}
	for _, want := range []string{relITRelName, relITEntityA, relITEntityB, "N-N"} {
		if !strings.Contains(res, want) {
			t.Fatalf("tenant A list missing %q: %s", want, res)
		}
	}

	// The entity filter narrows through the same tenant-scoped read.
	resF := execA.execute(ctx, "list_relationships", `{"entity":"`+relITEntityB+`"}`)
	if ok, _ := parseToolResult(t, resF); !ok || !strings.Contains(resF, relITRelName) {
		t.Fatalf("filtered list should surface the seeded N-N: %s", resF)
	}

	// F2 regression: sys_entities.name is utf8mb4_unicode_ci (case-insensitive), so
	// GetEntityByName resolves a case-variant like "IT_AI_M5_COURSES" to the stored
	// "it_ai_m5_courses" row and reports success. The filter MUST use that resolved canonical
	// name, not the raw model-supplied string — a raw-string filter would compare the canonical
	// RelationshipDefinition rows against the case-variant input, match nothing, and return an
	// empty list the model could misread as "no relationships yet" (leading it to propose a
	// duplicate). This case is unreachable with the in-memory fake (fakeMetaReader's map lookup
	// is exact-string), which is why it lives here against the real collation.
	resCI := execA.execute(ctx, "list_relationships", `{"entity":"`+strings.ToUpper(relITEntityB)+`"}`)
	okCI, _ := parseToolResult(t, resCI)
	if !okCI {
		t.Fatalf("case-variant entity name should resolve via ci collation: %s", resCI)
	}
	if !strings.Contains(resCI, relITRelName) {
		t.Fatalf("F2 regression: case-variant filter on %q returned an EMPTY/wrong list instead of the seeded relationship: %s",
			strings.ToUpper(relITEntityB), resCI)
	}

	// Tenant B sees NOTHING of tenant A's relationship — SQL-level isolation, not fake-level.
	execB := &toolExecutor{reader: repo, tenantID: relITTenantB}
	resB := execB.execute(ctx, "list_relationships", "{}")
	if ok, _ := parseToolResult(t, resB); !ok {
		t.Fatalf("list_relationships (tenant B) should succeed with an empty list: %s", resB)
	}
	if strings.Contains(resB, relITRelName) || strings.Contains(resB, relITEntityA) {
		t.Fatalf("tenant B leaked tenant A relationship rows: %s", resB)
	}
}

func TestToolsRelationship_IT_ProposeRoundTrip(t *testing.T) {
	repo, gdb := newRelITReader(t)
	seedRelIT(t, gdb, repo)
	ctx := context.Background()

	// Happy path: both entities resolve via the REAL GetEntityByName → buffered draft.
	execA := &toolExecutor{reader: repo, tenantID: relITTenantA}
	args := `{"name":"it_ai_m5_stu_cou_extra","entityA":"` + relITEntityA + `","entityB":"` + relITEntityB + `"}`
	res := execA.execute(ctx, "propose_relationship", args)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("propose_relationship should succeed against real metadata: %s", res)
	}
	if len(execA.draft) != 1 || execA.draft[0].Kind != "relationship" || execA.draft[0].Relationship == nil {
		t.Fatalf("expected 1 buffered relationship draft, got %+v", execA.draft)
	}
	// HITL: nothing was written — the draft is a proposal, not a sys_relationships row.
	var n int64
	if err := gdb.Raw(`SELECT COUNT(*) FROM sys_relationships WHERE tenant_id = ? AND name = ?`,
		relITTenantA, "it_ai_m5_stu_cou_extra").Scan(&n).Error; err != nil {
		t.Fatalf("count check: %v", err)
	}

	// F6: propose_relationship must ALSO not create the physical join table — the DDL side of
	// RegisterRelationship (dynRepo.CreateJoinTable) is never invoked by the tool. Derive the
	// canonical join-table name with the exact same helper the apply path uses, so this
	// assertion tracks JoinParts's naming rule instead of duplicating it by hand.
	joinTable, _, _, jpErr := ddl.JoinParts(relITEntityA, relITEntityB)
	if jpErr != nil {
		t.Fatalf("ddl.JoinParts: %v", jpErr)
	}
	var joinTableCount int64
	if err := gdb.Raw(
		`SELECT COUNT(*) FROM information_schema.TABLES WHERE table_schema = DATABASE() AND table_name = ?`,
		joinTable).Scan(&joinTableCount).Error; err != nil {
		t.Fatalf("information_schema check: %v", err)
	}
	if joinTableCount != 0 {
		t.Fatalf("propose_relationship must NOT create the join table %q; found %d", joinTable, joinTableCount)
	}

	if n != 0 {
		t.Fatalf("propose_relationship must NOT write sys_relationships; found %d rows", n)
	}

	// Tenant B proposing over tenant A's entities: real GetEntityByName makes the cross-tenant
	// entity indistinguishable from a missing one.
	execB := &toolExecutor{reader: repo, tenantID: relITTenantB}
	resB := execB.execute(ctx, "propose_relationship", args)
	okB, m := parseToolResult(t, resB)
	if okB {
		t.Fatalf("tenant B must not resolve tenant A's entities: %s", resB)
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
		t.Fatalf("expected ENTITY_NOT_FOUND for cross-tenant entities, got %q", errStr)
	}
	if len(execB.draft) != 0 {
		t.Fatalf("rejected cross-tenant proposal must not buffer, got %+v", execB.draft)
	}
}
