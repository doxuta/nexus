//go:build integration

// B3-perf dedup integration test — proves the batched dedup path works against a real
// MySQL database, using a unique generated-column field (email) so the physical column
// index is exercised, and that the outcome is identical to the old per-value path:
// rows whose dedup value already exists in the DB are rejected as RECORD_DUPLICATE;
// new rows insert. Also verifies that BatchExistingValuesForField is called exactly
// O(⌈N/chunk⌉) times (once for a batch under DedupChunkSize) rather than once per
// distinct value (the old O(N) pattern).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run TestDedupBatch -v
package services

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// ─── counting wrapper ─────────────────────────────────────────────────────────

// countingImportStore wraps *dynrepo.Repository to count BatchExistingValuesForField calls
// while using the real DB implementation for all operations.
type countingImportStore struct {
	inner           *dynrepo.Repository
	batchDedupCalls int
}

func (c *countingImportStore) InsertRecords(ctx context.Context, entity, tenantID, userID string, rows []map[string]any, uniqueFields []dynrepo.UniqueFieldSpec) ([]string, error) {
	return c.inner.InsertRecords(ctx, entity, tenantID, userID, rows, uniqueFields)
}

func (c *countingImportStore) BatchExistingValuesForField(ctx context.Context, entity, tenantID, fieldName string, spec dynrepo.DedupFieldSpec, values []string) (map[string]struct{}, error) {
	c.batchDedupCalls++
	return c.inner.BatchExistingValuesForField(ctx, entity, tenantID, fieldName, spec, values)
}

// BatchRecordExists forwards to the real repo (satisfies RelationBatchChecker).
func (c *countingImportStore) BatchRecordExists(ctx context.Context, entity, tenantID string, ids []string) (map[string]bool, error) {
	return c.inner.BatchRecordExists(ctx, entity, tenantID, ids)
}

// ─── setup ────────────────────────────────────────────────────────────────────

// dedupIntegrationSetup creates a throwaway DB (via o3aSetup), registers a "contacts"
// entity with a unique "email" field, pre-inserts one record so the dedup path has
// something to find, and returns the fully-wired ImportService alongside the counting
// store wrapper (for call-count assertions).
func dedupIntegrationSetup(t *testing.T) (svc *ImportService, store *countingImportStore, tenantID, preExistingEmail string) {
	t.Helper()

	gdb, sdb := o3aSetup(t)
	dynRepo := dynrepo.New(sdb)
	tenantID = uuid.NewString()
	userID := uuid.NewString()
	ctx := context.Background()

	// Use the real metadata repo (GORM) backed by the migrated throwaway DB.
	// EntityService and FieldService are wired exactly as in production.
	metaRepo := metadata.NewRepository(gdb)
	entSvc := NewEntityService(dynRepo, metaRepo)

	// Register "contacts" entity with "name" (required text) and "email" (unique) fields.
	_, _, err := entSvc.CreateEntity(ctx, tenantID, CreateEntityInput{
		Name:  "contacts",
		Label: "Contacts",
		Fields: []FieldInput{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "email", FieldType: "email", IsUnique: true},
		},
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	// Ensure the obj_contacts table is dropped at the end of the test only if we
	// created it (throwaway DB is fully dropped by o3aSetup cleanup anyway, so this
	// is belt-and-suspenders for the physical table name).
	// o3aSetup already schedules DROP DATABASE, so no extra cleanup needed here.

	// Pre-insert one record with a known email so the dedup probe has an existing value.
	preExistingEmail = fmt.Sprintf("existing-%s@example.com", uuid.NewString()[:8])
	_, err = dynRepo.InsertRecords(ctx, "contacts", tenantID, userID, []map[string]any{
		{"name": "Pre-existing", "email": preExistingEmail},
	}, nil)
	if err != nil {
		t.Fatalf("InsertRecords pre-existing: %v", err)
	}

	// Wire ImportService with the real metadata catalog and the counting store.
	countStore := &countingImportStore{inner: dynRepo}
	svc = NewImportService(metaRepo, countStore, nil)
	svc.WithRelationChecker(countStore)

	return svc, countStore, tenantID, preExistingEmail
}

// ─── test ─────────────────────────────────────────────────────────────────────

// TestDedupBatch_BatchQueryAndOutcome verifies end-to-end against a real MySQL DB:
//  1. A row whose email already exists in the DB is detected as RECORD_DUPLICATE.
//  2. A row with a new email inserts successfully.
//  3. BatchExistingValuesForField is called exactly ONCE (not once per value) — proving
//     the O(⌈N/chunk⌉) query pattern instead of the old O(N) per-value pattern.
func TestDedupBatch_BatchQueryAndOutcome(t *testing.T) {
	svc, store, tenantID, existingEmail := dedupIntegrationSetup(t)
	ctx := context.Background()

	newEmail := fmt.Sprintf("new-%s@example.com", uuid.NewString()[:8])

	// CSV: one row whose email is already in the DB, one that is new.
	csvData := "col_name,col_email\n" +
		fmt.Sprintf("Existing User,%s\n", existingEmail) +
		fmt.Sprintf("New User,%s\n", newEmail)

	in := ImportInput{
		TenantID:   tenantID,
		UserID:     uuid.NewString(),
		EntityName: "contacts",
		Mapping:    map[string]string{"col_name": "name", "col_email": "email"},
		DedupField: "email",
		Reader:     strings.NewReader(csvData),
		FileSize:   int64(len(csvData)),
	}

	result, err := svc.Import(ctx, in)
	if err != nil {
		t.Fatalf("Import returned unexpected error: %v", err)
	}

	// New-email row must insert; existing-email row must be detected as a duplicate.
	if result.Inserted != 1 {
		t.Errorf("Inserted: got %d, want 1 (new-email row only)", result.Inserted)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors: got %d, want 1 (existing-email must be RECORD_DUPLICATE)", len(result.Errors))
	}
	if result.Errors[0].Code != "RECORD_DUPLICATE" {
		t.Errorf("Errors[0].Code = %q; want RECORD_DUPLICATE", result.Errors[0].Code)
	}

	// KEY assertion: exactly ONE call to BatchExistingValuesForField for 2 distinct
	// values — not 2 calls (the old per-value pattern would have issued 2 round-trips).
	if store.batchDedupCalls != 1 {
		t.Errorf("BatchExistingValuesForField calls: got %d, want 1 (batched, not per-value)",
			store.batchDedupCalls)
	}
}
