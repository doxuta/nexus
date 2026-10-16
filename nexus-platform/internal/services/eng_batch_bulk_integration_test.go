//go:build integration

// ENG-BATCH #1 — bulk (multi-row) record create, end-to-end integration tests over a freshly
// migrated throwaway MySQL DB (o3aSetup helper — mirrors the a4 splitDSN discipline: GORM +
// migrate both derive their DSN from the SAME TEST_DATABASE_URL, never a second source). These
// prove what a fake store cannot: a REAL atomic rollback (COUNT(*) stays put), a REAL race
// between two concurrent batches on MySQL's own unique index, REAL tenant isolation on a
// SHARED obj_* table, and REAL sequential auto_number claims (sys_sequences).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestEngBatchBulk -race -v
package services

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// bulkWire builds an EntityService + RecordService wired exactly like production (main.go):
// dynRepo backs InsertRecord/UpdateRecord, ClaimSequence (WithSequenceClaimer) AND
// InsertRecordsAtomic (WithBulkInserter), over a fresh throwaway DB.
func bulkWire(t *testing.T) (*EntityService, *RecordService) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	recordSvc := NewRecordService(metaRepo, dynRepo).WithSequenceClaimer(dynRepo).WithBulkInserter(dynRepo)
	return entitySvc, recordSvc
}

func setupWidgetsEntity(t *testing.T, entitySvc *EntityService, tenant string) {
	t.Helper()
	if _, _, err := entitySvc.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name:  "widgets",
		Label: "Widgets",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "email", Label: "Email", FieldType: "text", IsUnique: true},
			{Name: "code", Label: "Code", FieldType: "auto_number",
				Options: map[string]any{"prefix": "W-", "padding": float64(3)}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity widgets: %v", err)
	}
}

// setupWidgetsWithOwnerEntity registers "accounts" (a bare target entity) THEN "widgets" with a
// relation field "owner" pointing at it — used by the cross-tenant relation test.
func setupWidgetsWithOwnerEntity(t *testing.T, entitySvc *EntityService, tenant string) {
	t.Helper()
	if _, _, err := entitySvc.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name:   "accounts",
		Label:  "Accounts",
		Fields: []FieldInput{{Name: "label", Label: "Label", FieldType: "text"}},
	}); err != nil {
		t.Fatalf("CreateEntity accounts: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name:  "widgets",
		Label: "Widgets",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "account", Label: "Account", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "accounts"}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity widgets: %v", err)
	}
}

func countWidgets(t *testing.T, recordSvc *RecordService, scope RecordScope, tenant string) int {
	t.Helper()
	res, err := recordSvc.List(context.Background(), scope, tenant, "widgets", ListQuery{Page: 1, Limit: 100})
	if err != nil {
		t.Fatalf("List widgets: %v", err)
	}
	return len(res.Records)
}

// TestEngBatchBulk_AtomicAllValid_SequentialAutoNumber proves the atomic happy path against
// real MySQL: 3 valid rows, ONE InsertRecordsAtomic transaction, 3 sequential auto_number
// claims, all 3 rows durably readable afterward.
func TestEngBatchBulk_AtomicAllValid_SequentialAutoNumber(t *testing.T) {
	entitySvc, recordSvc := bulkWire(t)
	ctx := context.Background()
	const tenant = "eeeeeeee-0b0b-0b0b-0b0b-eeeeeeeeee01"
	const user = "11111111-1111-1111-1111-111111111301"
	setupWidgetsEntity(t, entitySvc, tenant)
	scope := RecordScope{UserID: user, AccessLevel: "all", IsAdmin: true}

	payloads := []map[string]any{
		{"name": "Row0", "email": "row0@example.com"},
		{"name": "Row1", "email": "row1@example.com"},
		{"name": "Row2", "email": "row2@example.com"},
	}
	result, err := recordSvc.CreateBulk(ctx, scope, tenant, user, "widgets", payloads, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Created != 3 || result.Failed != 0 {
		t.Fatalf("Created=%d Failed=%d; want 3/0 (results=%+v)", result.Created, result.Failed, result.Results)
	}
	wantCodes := map[string]string{"row0@example.com": "W-001", "row1@example.com": "W-002", "row2@example.com": "W-003"}
	for _, r := range result.Results {
		rec, err := recordSvc.Get(ctx, scope, tenant, "widgets", r.ID, nil, nil)
		if err != nil {
			t.Fatalf("Get %s: %v", r.ID, err)
		}
		email, _ := rec.Data["email"].(string)
		if got := rec.Data["code"]; got != wantCodes[email] {
			t.Errorf("record %s (email=%s) code = %v; want %v", r.ID, email, got, wantCodes[email])
		}
	}
	if got := countWidgets(t, recordSvc, scope, tenant); got != 3 {
		t.Errorf("countWidgets = %d; want 3", got)
	}
}

// TestEngBatchBulk_AtomicRollback_RealDBAndGapBurn proves a REAL atomic rollback: row 1 of a
// 3-row batch collides with a value ALREADY IN THE TABLE (not another row in the same batch —
// so the intra-batch pre-check cannot catch it; only MySQL's own UNIQUE index can). The
// transaction rolls back in FULL: zero of the 3 batch rows are ever persisted (COUNT stays at
// the 1 pre-existing seed row), and the auto_number claims already spent on Phase 2 (BEFORE the
// insert attempt) are burned — the next successful create skips straight over them (documented
// gaps-allowed contract, ../specs/006-auto-number/spec.md §"#6", now proven across a WHOLE aborted batch).
func TestEngBatchBulk_AtomicRollback_RealDBAndGapBurn(t *testing.T) {
	entitySvc, recordSvc := bulkWire(t)
	ctx := context.Background()
	const tenant = "eeeeeeee-0b0b-0b0b-0b0b-eeeeeeeeee02"
	const user = "11111111-1111-1111-1111-111111111302"
	setupWidgetsEntity(t, entitySvc, tenant)
	scope := RecordScope{UserID: user, AccessLevel: "all", IsAdmin: true}

	// Seed row: consumes auto_number W-001 and occupies email "seed@example.com".
	if _, err := recordSvc.Create(ctx, scope, tenant, user, "widgets", map[string]any{"name": "Seed", "email": "seed@example.com"}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	payloads := []map[string]any{
		{"name": "Row0", "email": "unique-a@example.com"},
		{"name": "Row1", "email": "seed@example.com"}, // collides with the PRE-EXISTING seed row
		{"name": "Row2", "email": "unique-b@example.com"},
	}
	result, err := recordSvc.CreateBulk(ctx, scope, tenant, user, "widgets", payloads, BulkModeAtomic)
	if err != nil {
		t.Fatalf("CreateBulk top-level error: %v (want nil)", err)
	}
	if result.Created != 0 || result.Failed != 1 || result.Skipped != 2 {
		t.Fatalf("Created=%d Failed=%d Skipped=%d; want 0/1/2 (Row1 collides at insert; Row0,2 skipped) (results=%+v)", result.Created, result.Failed, result.Skipped, result.Results)
	}
	if result.Results[1].Code != "RECORD_DUPLICATE" || result.Results[1].Details["field"] != "email" {
		t.Errorf("Results[1] = %+v; want code=RECORD_DUPLICATE details.field=email", result.Results[1])
	}
	if result.Results[0].Status != "skipped" || result.Results[2].Status != "skipped" {
		t.Errorf("Results[0]/[2] must be skipped; got %+v / %+v", result.Results[0], result.Results[2])
	}

	// Real rollback: only the seed row survives.
	if got := countWidgets(t, recordSvc, scope, tenant); got != 1 {
		t.Fatalf("countWidgets after aborted batch = %d; want 1 (only the seed row)", got)
	}

	// Gap-burn: the aborted batch claimed W-002, W-003, W-004 (seed already took W-001) BEFORE
	// the insert attempt — the next successful create must skip straight to W-005.
	id, err := recordSvc.Create(ctx, scope, tenant, user, "widgets", map[string]any{"name": "AfterAbort", "email": "after-abort@example.com"})
	if err != nil {
		t.Fatalf("post-abort create: %v", err)
	}
	rec, err := recordSvc.Get(ctx, scope, tenant, "widgets", id, nil, nil)
	if err != nil {
		t.Fatalf("Get post-abort record: %v", err)
	}
	if got := rec.Data["code"]; got != "W-005" {
		t.Errorf("post-abort record code = %v; want W-005 (gap over the 3 burned claims)", got)
	}
}

// TestEngBatchBulk_Partial_RealDB proves mode=partial against real MySQL: 3 valid + 2 invalid
// rows insert exactly the 3 valid ones (durably readable), report the 2 invalid ones at their
// original indices, and hand out sequential auto_number values only to the inserted rows.
func TestEngBatchBulk_Partial_RealDB(t *testing.T) {
	entitySvc, recordSvc := bulkWire(t)
	ctx := context.Background()
	const tenant = "eeeeeeee-0b0b-0b0b-0b0b-eeeeeeeeee03"
	const user = "11111111-1111-1111-1111-111111111303"
	setupWidgetsEntity(t, entitySvc, tenant)
	scope := RecordScope{UserID: user, AccessLevel: "all", IsAdmin: true}

	payloads := []map[string]any{
		{"name": "Row0", "email": "p0@example.com"}, // valid
		{"email": "no-name@example.com"},            // invalid: name required
		{"name": "Row2", "email": "p2@example.com"}, // valid
		{"name": "Row3", "unknown_field": "x"},      // invalid: unknown field
		{"name": "Row4", "email": "p4@example.com"}, // valid
	}
	result, err := recordSvc.CreateBulk(ctx, scope, tenant, user, "widgets", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Created != 3 || result.Failed != 2 {
		t.Fatalf("Created=%d Failed=%d; want 3/2 (results=%+v)", result.Created, result.Failed, result.Results)
	}
	if result.Results[1].Code != "RECORD_REQUIRED_MISSING" {
		t.Errorf("Results[1].Code = %q; want RECORD_REQUIRED_MISSING", result.Results[1].Code)
	}
	if result.Results[3].Code != "RECORD_UNKNOWN_FIELD" {
		t.Errorf("Results[3].Code = %q; want RECORD_UNKNOWN_FIELD", result.Results[3].Code)
	}
	if got := countWidgets(t, recordSvc, scope, tenant); got != 3 {
		t.Fatalf("countWidgets = %d; want 3", got)
	}
	wantCodes := map[int]string{0: "W-001", 2: "W-002", 4: "W-003"}
	for idx, wantCode := range wantCodes {
		id := result.Results[idx].ID
		if id == "" {
			t.Fatalf("Results[%d].ID empty", idx)
		}
		rec, err := recordSvc.Get(ctx, scope, tenant, "widgets", id, nil, nil)
		if err != nil {
			t.Fatalf("Get Results[%d]: %v", idx, err)
		}
		if got := rec.Data["code"]; got != wantCode {
			t.Errorf("Results[%d] code = %v; want %v", idx, got, wantCode)
		}
	}
}

// TestEngBatchBulk_RaceTwoConcurrentAtomicBatches_UniqueBackstop races two goroutines, each
// submitting an atomic batch of 2 rows that are internally unique but SHARE one colliding email
// value across the two batches — something NEITHER batch's own intra-batch pre-check can see
// (each batch only knows about its own rows). Real MySQL's UNIQUE index is the ONLY thing that
// can catch this. Exactly one batch must fully succeed (2 rows) and the other must fully abort
// (0 rows) with a clean 409-class RECORD_DUPLICATE — never a 500, never a half-written batch.
func TestEngBatchBulk_RaceTwoConcurrentAtomicBatches_UniqueBackstop(t *testing.T) {
	entitySvc, recordSvc := bulkWire(t)
	ctx := context.Background()
	const tenant = "eeeeeeee-0b0b-0b0b-0b0b-eeeeeeeeee04"
	const user = "11111111-1111-1111-1111-111111111304"
	setupWidgetsEntity(t, entitySvc, tenant)
	scope := RecordScope{UserID: user, AccessLevel: "all", IsAdmin: true}

	const raceEmail = "race@example.com"
	var wg sync.WaitGroup
	results := make([]BulkCreateResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payloads := []map[string]any{
				{"name": fmt.Sprintf("Batch%d-Row0", i), "email": raceEmail},
				{"name": fmt.Sprintf("Batch%d-Row1", i), "email": fmt.Sprintf("batch%d-unique@example.com", i)},
			}
			results[i], errs[i] = recordSvc.CreateBulk(ctx, scope, tenant, user, "widgets", payloads, BulkModeAtomic)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("batch %d top-level error: %v (want nil — a race must map to a clean per-row code, not a Go error)", i, err)
		}
	}

	successCount, failCount := 0, 0
	for i, r := range results {
		switch r.Created {
		case 2:
			successCount++
		case 0:
			failCount++
			// The losing batch's row touching raceEmail must be a clean 409-class mapping.
			foundDup := false
			for _, row := range r.Results {
				if row.Code == "RECORD_DUPLICATE" {
					foundDup = true
				}
			}
			if !foundDup {
				t.Errorf("batch %d lost the race but no row reports RECORD_DUPLICATE: %+v", i, r.Results)
			}
		default:
			t.Fatalf("batch %d Created=%d; want exactly 0 or 2 (no half-written batch)", i, r.Created)
		}
	}
	if successCount != 1 || failCount != 1 {
		t.Fatalf("successCount=%d failCount=%d; want exactly 1/1 (one batch wins the race, one fully rolls back)", successCount, failCount)
	}
	if got := countWidgets(t, recordSvc, scope, tenant); got != 2 {
		t.Fatalf("countWidgets after race = %d; want 2 (only the winning batch's 2 rows)", got)
	}
}

// TestEngBatchBulk_TenantIsolation_RealDB proves that a bulk-create for tenant A never leaks
// into tenant B's row count on the SAME PHYSICAL obj_widgets table (shared-by-entity-name,
// row-scoped by tenant_id — rules/database.md), and that each tenant's auto_number sequence is
// independent (tenant B's first bulk row also starts at W-001, not continuing tenant A's count).
func TestEngBatchBulk_TenantIsolation_RealDB(t *testing.T) {
	entitySvc, recordSvc := bulkWire(t)
	ctx := context.Background()
	const tenantA = "eeeeeeee-0b0b-0b0b-0b0b-eeeeeeeeeeaa"
	const tenantB = "eeeeeeee-0b0b-0b0b-0b0b-eeeeeeeeeebb"
	const user = "11111111-1111-1111-1111-111111111305"
	setupWidgetsEntity(t, entitySvc, tenantA)
	setupWidgetsEntity(t, entitySvc, tenantB)
	scopeA := RecordScope{UserID: user, AccessLevel: "all", IsAdmin: true}
	scopeB := scopeA

	payloadsA := []map[string]any{
		{"name": "A0", "email": "a0@example.com"},
		{"name": "A1", "email": "a1@example.com"},
		{"name": "A2", "email": "a2@example.com"},
	}
	resultA, err := recordSvc.CreateBulk(ctx, scopeA, tenantA, user, "widgets", payloadsA, BulkModeAtomic)
	if err != nil || resultA.Created != 3 {
		t.Fatalf("tenantA CreateBulk: err=%v result=%+v", err, resultA)
	}

	if got := countWidgets(t, recordSvc, scopeB, tenantB); got != 0 {
		t.Fatalf("tenantB countWidgets AFTER tenantA's bulk create = %d; want 0 (no cross-tenant leak)", got)
	}

	payloadsB := []map[string]any{{"name": "B0", "email": "b0@example.com"}}
	resultB, err := recordSvc.CreateBulk(ctx, scopeB, tenantB, user, "widgets", payloadsB, BulkModeAtomic)
	if err != nil || resultB.Created != 1 {
		t.Fatalf("tenantB CreateBulk: err=%v result=%+v", err, resultB)
	}
	recB, err := recordSvc.Get(ctx, scopeB, tenantB, "widgets", resultB.Results[0].ID, nil, nil)
	if err != nil {
		t.Fatalf("Get tenantB record: %v", err)
	}
	if got := recB.Data["code"]; got != "W-001" {
		t.Errorf("tenantB first bulk row code = %v; want W-001 (independent per-tenant sequence, NOT W-004)", got)
	}
	if got := countWidgets(t, recordSvc, scopeA, tenantA); got != 3 {
		t.Errorf("tenantA countWidgets AFTER tenantB's bulk create = %d; want 3 (unaffected)", got)
	}
}

// TestEngBatchBulk_RelationTargetCrossTenant_ReturnsRelationInvalid proves the tenant-scoped
// relation-FK backstop holds on the BATCHED bulk path: tenant B creates a real "accounts"
// record; tenant A's bulk-create references that SAME id (the physical obj_accounts table is
// shared by entity name) — BatchRecordExists is tenant-scoped, so tenant A must NOT be able to
// "borrow" tenant B's row. The row is rejected RECORD_RELATION_INVALID, never silently linked.
func TestEngBatchBulk_RelationTargetCrossTenant_ReturnsRelationInvalid(t *testing.T) {
	entitySvc, recordSvc := bulkWire(t)
	ctx := context.Background()
	const tenantA = "eeeeeeee-0b0b-0b0b-0b0b-eeeeeeeeeecc"
	const tenantB = "eeeeeeee-0b0b-0b0b-0b0b-eeeeeeeeeedd"
	const user = "11111111-1111-1111-1111-111111111306"
	setupWidgetsWithOwnerEntity(t, entitySvc, tenantA)
	setupWidgetsWithOwnerEntity(t, entitySvc, tenantB)
	scopeA := RecordScope{UserID: user, AccessLevel: "all", IsAdmin: true}
	scopeB := scopeA

	// A real account that exists ONLY in tenant B.
	acctBID, err := recordSvc.Create(ctx, scopeB, tenantB, user, "accounts", map[string]any{"label": "TenantB Account"})
	if err != nil {
		t.Fatalf("create tenantB account: %v", err)
	}
	// A real account that exists in tenant A (for the "valid" row in the same batch).
	acctAID, err := recordSvc.Create(ctx, scopeA, tenantA, user, "accounts", map[string]any{"label": "TenantA Account"})
	if err != nil {
		t.Fatalf("create tenantA account: %v", err)
	}

	payloads := []map[string]any{
		{"name": "Widget0", "account": acctAID}, // valid: same-tenant target
		{"name": "Widget1", "account": acctBID}, // invalid: cross-tenant borrow attempt
	}
	result, err := recordSvc.CreateBulk(ctx, scopeA, tenantA, user, "widgets", payloads, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if result.Results[0].Status != "created" {
		t.Errorf("Results[0] = %+v; want status=created (same-tenant owner)", result.Results[0])
	}
	if result.Results[1].Status != "failed" || result.Results[1].Code != "RECORD_RELATION_INVALID" {
		t.Errorf("Results[1] = %+v; want status=failed code=RECORD_RELATION_INVALID (cross-tenant owner must be rejected)", result.Results[1])
	}
}
