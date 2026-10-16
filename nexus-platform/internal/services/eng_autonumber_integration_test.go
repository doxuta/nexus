//go:build integration

// ENG #6 auto_number — end-to-end integration tests, exercised through
// EntityService/RecordService over a freshly-migrated throwaway MySQL DB (o3aSetup helpers from
// metadata_cache_wired_integration_test.go). Mirrors eng_computed_integration_test.go's engWire
// pattern. Covers what cannot be proven with fakes: real sequential claims against real MySQL
// (dynrepo.ClaimSequence), and the documented gaps-allowed behaviour when InsertRecord fails
// AFTER a number has already been claimed (assignSequences runs outside the InsertRecord
// transaction — see migrations/000029 and record_orchestration.go's assignSequences doc comment).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestEngAutoNumber -race -v
package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// autoNumWire builds an EntityService + RecordService wired exactly like production (main.go):
// dynRepo backs both InsertRecord/UpdateRecord AND ClaimSequence (WithSequenceClaimer), over a
// fresh throwaway DB.
func autoNumWire(t *testing.T) (*EntityService, *RecordService) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	recordSvc := NewRecordService(metaRepo, dynRepo).WithSequenceClaimer(dynRepo)
	return entitySvc, recordSvc
}

func setupInvoicesEntity(t *testing.T, entitySvc *EntityService, tenant string) {
	t.Helper()
	if _, _, err := entitySvc.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name:  "invoices",
		Label: "Invoices",
		Fields: []FieldInput{
			{Name: "code", Label: "Code", FieldType: "text", IsUnique: true},
			{Name: "invoice_no", Label: "Invoice No", FieldType: "auto_number",
				Options: map[string]any{"prefix": "INV-", "padding": float64(5)}},
			{Name: "amount", Label: "Amount", FieldType: "number"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity invoices: %v", err)
	}
}

// TestEngAutoNumber_SequentialCreateFormatsIncrement proves the full Create -> Get round trip:
// two consecutive records against a real MySQL sys_sequences claim receive strictly increasing,
// correctly formatted invoice numbers.
func TestEngAutoNumber_SequentialCreateFormatsIncrement(t *testing.T) {
	entitySvc, recordSvc := autoNumWire(t)
	ctx := context.Background()
	const tenant = "eeeeeeee-0e0e-0e0e-0e0e-eeeeeeeeeeee"
	const user = "11111111-1111-1111-1111-111111111201"
	setupInvoicesEntity(t, entitySvc, tenant)
	scope := RecordScope{UserID: user, AccessLevel: "all", IsAdmin: true}

	id1, err := recordSvc.Create(ctx, scope, tenant, user, "invoices", map[string]any{"code": "A", "amount": float64(100)})
	if err != nil {
		t.Fatalf("create invoice 1: %v", err)
	}
	id2, err := recordSvc.Create(ctx, scope, tenant, user, "invoices", map[string]any{"code": "B", "amount": float64(200)})
	if err != nil {
		t.Fatalf("create invoice 2: %v", err)
	}

	rec1, err := recordSvc.Get(ctx, scope, tenant, "invoices", id1, nil, nil)
	if err != nil {
		t.Fatalf("Get invoice 1: %v", err)
	}
	if got := rec1.Data["invoice_no"]; got != "INV-00001" {
		t.Errorf("invoice 1 invoice_no = %v; want INV-00001", got)
	}
	rec2, err := recordSvc.Get(ctx, scope, tenant, "invoices", id2, nil, nil)
	if err != nil {
		t.Fatalf("Get invoice 2: %v", err)
	}
	if got := rec2.Data["invoice_no"]; got != "INV-00002" {
		t.Errorf("invoice 2 invoice_no = %v; want INV-00002 (sequential increment)", got)
	}
}

// TestEngAutoNumber_TwoTenantsCountIndependently proves the same tenant-isolation contract as
// TestClaimSequence_TenantIsolation, but through the full record-write path: two tenants
// registering the SAME entity name ("invoices", sharing the physical obj_invoices table per
// rules/database.md) each start their own auto_number sequence at 1.
func TestEngAutoNumber_TwoTenantsCountIndependently(t *testing.T) {
	entitySvc, recordSvc := autoNumWire(t)
	ctx := context.Background()
	const tenantA = "eeeeeeee-0e0e-0e0e-0e0e-eeeeeeeeeeaa"
	const tenantB = "eeeeeeee-0e0e-0e0e-0e0e-eeeeeeeeeebb"
	const user = "11111111-1111-1111-1111-111111111202"
	setupInvoicesEntity(t, entitySvc, tenantA)
	setupInvoicesEntity(t, entitySvc, tenantB)
	scopeA := RecordScope{UserID: user, AccessLevel: "all", IsAdmin: true}
	scopeB := scopeA

	idA1, err := recordSvc.Create(ctx, scopeA, tenantA, user, "invoices", map[string]any{"code": "A1", "amount": float64(1)})
	if err != nil {
		t.Fatalf("create tenantA invoice 1: %v", err)
	}
	idA2, err := recordSvc.Create(ctx, scopeA, tenantA, user, "invoices", map[string]any{"code": "A2", "amount": float64(2)})
	if err != nil {
		t.Fatalf("create tenantA invoice 2: %v", err)
	}
	idB1, err := recordSvc.Create(ctx, scopeB, tenantB, user, "invoices", map[string]any{"code": "B1", "amount": float64(1)})
	if err != nil {
		t.Fatalf("create tenantB invoice 1: %v", err)
	}

	recA1, _ := recordSvc.Get(ctx, scopeA, tenantA, "invoices", idA1, nil, nil)
	recA2, _ := recordSvc.Get(ctx, scopeA, tenantA, "invoices", idA2, nil, nil)
	recB1, _ := recordSvc.Get(ctx, scopeB, tenantB, "invoices", idB1, nil, nil)

	if got := recA1.Data["invoice_no"]; got != "INV-00001" {
		t.Errorf("tenantA invoice 1 = %v; want INV-00001", got)
	}
	if got := recA2.Data["invoice_no"]; got != "INV-00002" {
		t.Errorf("tenantA invoice 2 = %v; want INV-00002", got)
	}
	if got := recB1.Data["invoice_no"]; got != "INV-00001" {
		t.Errorf("tenantB invoice 1 = %v; want INV-00001 (own independent counter, NOT INV-00003)", got)
	}
}

// TestEngAutoNumber_GapAfterInsertFailure proves the documented gaps-allowed contract
// (../specs/006-auto-number/spec.md §"#6", migrations/000029 header): assignSequences claims a number BEFORE
// InsertRecord runs. When InsertRecord subsequently fails for an UNRELATED reason (here: a
// distinct unique field collision), the already-claimed number is burned — no record ever holds
// it, and the NEXT successful create skips straight over it.
func TestEngAutoNumber_GapAfterInsertFailure(t *testing.T) {
	entitySvc, recordSvc := autoNumWire(t)
	ctx := context.Background()
	const tenant = "eeeeeeee-0e0e-0e0e-0e0e-eeeeeeeeee6a"
	const user = "11111111-1111-1111-1111-111111111203"
	setupInvoicesEntity(t, entitySvc, tenant)
	scope := RecordScope{UserID: user, AccessLevel: "all", IsAdmin: true}

	// Record #1 succeeds: claims 1 -> INV-00001.
	id1, err := recordSvc.Create(ctx, scope, tenant, user, "invoices", map[string]any{"code": "DUP", "amount": float64(1)})
	if err != nil {
		t.Fatalf("create invoice 1: %v", err)
	}
	rec1, err := recordSvc.Get(ctx, scope, tenant, "invoices", id1, nil, nil)
	if err != nil {
		t.Fatalf("Get invoice 1: %v", err)
	}
	if got := rec1.Data["invoice_no"]; got != "INV-00001" {
		t.Fatalf("invoice 1 invoice_no = %v; want INV-00001", got)
	}

	// Record #2 reuses code "DUP" -> InsertRecord fails on the unique `code` constraint (409
	// RECORD_DUPLICATE) AFTER assignSequences already claimed 2 for this attempt. That claim is
	// burned: no record ever gets INV-00002.
	_, err = recordSvc.Create(ctx, scope, tenant, user, "invoices", map[string]any{"code": "DUP", "amount": float64(2)})
	if codeOf(err) != "RECORD_DUPLICATE" {
		t.Fatalf("code = %q; want RECORD_DUPLICATE (err=%v)", codeOf(err), err)
	}

	// Record #3 (distinct code) succeeds and MUST skip straight to INV-00003 — proving the gap
	// left by the burned #2 claim, not a re-use of it.
	id3, err := recordSvc.Create(ctx, scope, tenant, user, "invoices", map[string]any{"code": "OK", "amount": float64(3)})
	if err != nil {
		t.Fatalf("create invoice 3: %v", err)
	}
	rec3, err := recordSvc.Get(ctx, scope, tenant, "invoices", id3, nil, nil)
	if err != nil {
		t.Fatalf("Get invoice 3: %v", err)
	}
	if got := rec3.Data["invoice_no"]; got != "INV-00003" {
		t.Errorf("invoice 3 invoice_no = %v; want INV-00003 (gap over the burned INV-00002 — gaps-allowed by design)", got)
	}
}
