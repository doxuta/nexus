//go:build integration

// Integration tests for the M10 snapshot_model tool against the REAL *metadata.Repository — the
// production MetadataReader. They prove what the in-memory fake cannot: (a) the snapshot composes
// the REAL ListEntities + ListFields SQL (tenant predicates included) into one result that matches
// the seeded model exactly, and (b) two tenants seeding entities with the SAME name each snapshot
// ONLY their own rows — the strongest isolation probe, because only the tenant_id predicates
// separate them.
//
// Run with (throwaway DB migrated per the gate recipe — single-source TEST_DATABASE_URL, B7):
//
//	mysql -h127.0.0.1 -uroot -e "CREATE DATABASE IF NOT EXISTS nexus_it"
//	/opt/homebrew/bin/migrate -path migrations -database "mysql://root@tcp(127.0.0.1:3306)/nexus_it" up
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_it?parseTime=true&multiStatements=true&charset=utf8mb4" \
//	  go test -tags=integration ./internal/ai/ -run TestToolsSnapshot_IT -count=1
package ai

import (
	"context"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/metadata"
)

const (
	snapITTenantA = "aiaiaiai-mten-4444-8888-tenanta00001"
	snapITTenantB = "aiaiaiai-mten-4444-8888-tenantb00002"
	// The SAME entity name is seeded under BOTH tenants (different rows, different fields) — only
	// the tenant_id predicates in ListEntities/ListFields keep the two models apart.
	snapITShared = "it_ai_m10_patients" // 18 chars — within SanitizeIdentifier's 26-char cap
	snapITAOnly  = "it_ai_m10_visits"   // exists ONLY under tenant A
	snapITEntShA = "itai-mAe1-0000-0000-000000000001"
	snapITEntViA = "itai-mAe2-0000-0000-000000000002"
	snapITEntShB = "itai-mBe1-0000-0000-000000000003"
)

// cleanupSnapIT hard-deletes every row the suite seeds, keyed by its throwaway tenant IDs, so
// reruns are idempotent and nothing leaks into other suites sharing the DB.
func cleanupSnapIT(gdb *gorm.DB) {
	gdb.Exec(`DELETE FROM sys_fields WHERE tenant_id IN (?, ?)`, snapITTenantA, snapITTenantB)
	gdb.Exec(`DELETE FROM sys_entities WHERE tenant_id IN (?, ?)`, snapITTenantA, snapITTenantB)
}

// seedSnapIT registers tenant A's two entities (patients: 2 fields, visits: 1 field) and tenant
// B's same-named patients entity with a DIFFERENT field.
func seedSnapIT(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	cleanupSnapIT(gdb)
	t.Cleanup(func() { cleanupSnapIT(gdb) })

	ents := []metadata.EntityDefinition{
		{ID: snapITEntShA, TenantID: snapITTenantA, Name: snapITShared, Label: "M10 IT patients A", DataTable: "obj_" + snapITShared},
		{ID: snapITEntViA, TenantID: snapITTenantA, Name: snapITAOnly, Label: "M10 IT visits A", DataTable: "obj_" + snapITAOnly},
		{ID: snapITEntShB, TenantID: snapITTenantB, Name: snapITShared, Label: "M10 IT patients B", DataTable: "obj_" + snapITShared},
	}
	for i := range ents {
		if err := gdb.Create(&ents[i]).Error; err != nil {
			t.Fatalf("seed entity %s/%s: %v", ents[i].TenantID, ents[i].Name, err)
		}
	}

	fields := []metadata.FieldDefinition{
		{ID: "itai-mAf1-0000-0000-000000000001", EntityID: snapITEntShA, TenantID: snapITTenantA,
			Name: "full_name", Label: "Họ tên", FieldType: "text", IsRequired: true, FieldOrder: 10},
		{ID: "itai-mAf2-0000-0000-000000000002", EntityID: snapITEntShA, TenantID: snapITTenantA,
			Name: "birth_date", Label: "Ngày sinh", FieldType: "date", FieldOrder: 20},
		{ID: "itai-mAf3-0000-0000-000000000003", EntityID: snapITEntViA, TenantID: snapITTenantA,
			Name: "visit_at", Label: "Giờ khám", FieldType: "datetime", FieldOrder: 10},
		{ID: "itai-mBf1-0000-0000-000000000004", EntityID: snapITEntShB, TenantID: snapITTenantB,
			Name: "secret_note", Label: "Ghi chú B", FieldType: "text", FieldOrder: 10},
	}
	for i := range fields {
		if err := gdb.Create(&fields[i]).Error; err != nil {
			t.Fatalf("seed field %s: %v", fields[i].Name, err)
		}
	}
}

// snapEntityByName pulls one entity row out of a parsed snapshot, or fails the test.
func snapEntityByName(t *testing.T, s snapshotResult, name string) (found struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Fields []struct {
		Name      string `json:"name"`
		Label     string `json:"label"`
		FieldType string `json:"fieldType"`
	} `json:"fields"`
	FieldsTruncated bool `json:"fieldsTruncated"`
	FieldsTotal     int  `json:"fieldsTotal"`
}) {
	t.Helper()
	for _, en := range s.Result.Entities {
		if en.Name == name {
			return en
		}
	}
	t.Fatalf("snapshot missing entity %q: %+v", name, s.Result.Entities)
	return
}

func TestToolsSnapshot_IT_MatchesSeededModel(t *testing.T) {
	repo, gdb := newRelITReader(t) // same single-source DSN helper as the M5/M6 suites
	seedSnapIT(t, gdb)
	ctx := context.Background()

	// Compile-time + runtime proof: the REAL *metadata.Repository drives snapshot_model with no
	// adapter (snapshot composes the interface methods the repo already satisfies).
	execA := &toolExecutor{reader: repo, tenantID: snapITTenantA}
	raw := execA.execute(ctx, "snapshot_model", "{}")
	s := parseSnapshot(t, raw)
	if !s.OK {
		t.Fatalf("snapshot_model against real metadata should succeed: %s", raw)
	}

	// Exactly the two seeded tenant-A entities — no more, no less.
	if len(s.Result.Entities) != 2 {
		t.Fatalf("expected exactly 2 entities in tenant A's snapshot, got %d: %s", len(s.Result.Entities), raw)
	}

	pat := snapEntityByName(t, s, snapITShared)
	if pat.Label != "M10 IT patients A" {
		t.Fatalf("patients label mismatch (tenant B's row leaked?): %+v", pat)
	}
	if len(pat.Fields) != 2 ||
		pat.Fields[0].Name != "full_name" || pat.Fields[0].FieldType != "text" ||
		pat.Fields[1].Name != "birth_date" || pat.Fields[1].FieldType != "date" {
		t.Fatalf("patients fields must match the seeded rows in field_order: %+v", pat.Fields)
	}

	vis := snapEntityByName(t, s, snapITAOnly)
	if len(vis.Fields) != 1 || vis.Fields[0].Name != "visit_at" || vis.Fields[0].FieldType != "datetime" {
		t.Fatalf("visits fields must match the seeded row: %+v", vis.Fields)
	}

	// Read-only proof: composing the snapshot wrote nothing anywhere (entity/field counts stand).
	var n int64
	if err := gdb.Raw(`SELECT COUNT(*) FROM sys_entities WHERE tenant_id = ?`, snapITTenantA).Scan(&n).Error; err != nil {
		t.Fatalf("count check: %v", err)
	}
	if n != 2 {
		t.Fatalf("snapshot_model must not write sys_entities; found %d rows", n)
	}
}

func TestToolsSnapshot_IT_TenantIsolation(t *testing.T) {
	repo, gdb := newRelITReader(t)
	seedSnapIT(t, gdb)
	ctx := context.Background()

	// Tenant A's snapshot must NOT carry tenant B's field on the same-named entity.
	execA := &toolExecutor{reader: repo, tenantID: snapITTenantA}
	sA := parseSnapshot(t, execA.execute(ctx, "snapshot_model", "{}"))
	if !sA.OK {
		t.Fatal("tenant A snapshot should succeed")
	}
	for _, en := range sA.Result.Entities {
		for _, f := range en.Fields {
			if f.Name == "secret_note" {
				t.Fatalf("tenant A's snapshot leaked tenant B's field: %+v", en)
			}
		}
	}

	// Tenant B's snapshot: ONLY its own patients row with ONLY its own field — tenant A's
	// same-named entity, its fields, and its visits entity must all be absent.
	execB := &toolExecutor{reader: repo, tenantID: snapITTenantB}
	sB := parseSnapshot(t, execB.execute(ctx, "snapshot_model", "{}"))
	if !sB.OK {
		t.Fatal("tenant B snapshot should succeed")
	}
	if len(sB.Result.Entities) != 1 {
		t.Fatalf("tenant B must see exactly its 1 entity, got %d: %+v", len(sB.Result.Entities), sB.Result.Entities)
	}
	patB := snapEntityByName(t, sB, snapITShared)
	if patB.Label != "M10 IT patients B" {
		t.Fatalf("tenant B resolved tenant A's same-named entity: %+v", patB)
	}
	if len(patB.Fields) != 1 || patB.Fields[0].Name != "secret_note" {
		t.Fatalf("tenant B's fields must be ONLY its own: %+v", patB.Fields)
	}
}
