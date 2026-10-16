//go:build integration

// IMPORT-AUTONUM — end-to-end integration test proving CSV import assigns auto_number values
// through the SAME atomic per-tenant/entity/field sequence claim (ENG #6, dynrepo.ClaimSequence)
// the Create write-path uses. Before this fix the bulk InsertRecords path bypassed assignSequences
// entirely, so imported records were persisted with a permanently-blank auto_number. A unit fake
// cannot prove a DB sequence primitive, so this runs against a freshly-migrated throwaway MySQL DB
// (o3aSetup) wired exactly like main.go (ImportService.WithSequenceClaimer(dynRepo)).
//
// Covers: two consecutive import batches receive DISTINCT, MONOTONIC-increasing, correctly
// prefixed/padded auto_number values with NO duplicates; the assignment is tenant-scoped (a second
// tenant's import starts its own sequence at 1); and a formula field still fills (regression — the
// import path must not lose applyComputedFields when it gained assignSequenceValues).
//
// Run with:
//
//	mysql -h127.0.0.1 -uroot -e "CREATE DATABASE IF NOT EXISTS nexus_it"
//	/opt/homebrew/bin/migrate -path migrations -database "mysql://root@tcp(127.0.0.1:3306)/nexus_it" up
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_it?parseTime=true&multiStatements=true&charset=utf8mb4" \
//	  A4_MIGRATE_BIN=/opt/homebrew/bin/migrate \
//	  go test -tags=integration ./internal/services/ -run TestImportAutoNumber -count=1
package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// importAutonumWire wires an ImportService (WithSequenceClaimer, exactly like main.go) plus an
// EntityService (to register entities) and a RecordService (to read records back), all over one
// fresh throwaway MySQL DB. dynRepo backs the import store AND ClaimSequence — same concrete type,
// mirroring production.
func importAutonumWire(t *testing.T) (*ImportService, *EntityService, *RecordService) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	importSvc := NewImportService(metaRepo, dynRepo, nil).WithSequenceClaimer(dynRepo)
	recordSvc := NewRecordService(metaRepo, dynRepo)
	return importSvc, entitySvc, recordSvc
}

// setupImportInvoicesEntity registers an "invoices" entity with a unique code, a number amount, an
// auto_number invoice_no (prefix "INV-", padding 5) and a formula double_amount (= amount * 2). The
// auto_number + formula fields are both computed-readonly, so neither is CSV-mappable; both must be
// filled server-side.
func setupImportInvoicesEntity(t *testing.T, entitySvc *EntityService, tenant string) {
	t.Helper()
	if _, _, err := entitySvc.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name:  "invoices",
		Label: "Invoices",
		Fields: []FieldInput{
			{Name: "code", Label: "Code", FieldType: "text", IsUnique: true},
			{Name: "amount", Label: "Amount", FieldType: "number"},
			{Name: "invoice_no", Label: "Invoice No", FieldType: "auto_number",
				Options: map[string]any{"prefix": "INV-", "padding": float64(5)}},
			{Name: "double_amount", Label: "Double", FieldType: "formula",
				Options: map[string]any{"formula": "amount * 2"}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity invoices: %v", err)
	}
}

// importInvoiceBatch imports one CSV batch of (code, amount) rows and asserts every row inserted
// with no per-row errors.
func importInvoiceBatch(t *testing.T, importSvc *ImportService, tenant string, rows [][2]string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("code,amount\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%s,%s\n", r[0], r[1])
	}
	csvData := b.String()
	res, err := importSvc.Import(context.Background(), ImportInput{
		TenantID:   tenant,
		UserID:     uuid.NewString(),
		EntityName: "invoices",
		Mapping:    map[string]string{"code": "code", "amount": "amount"},
		Reader:     strings.NewReader(csvData),
		FileSize:   int64(len(csvData)),
	})
	if err != nil {
		t.Fatalf("Import batch: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("Import batch: unexpected per-row errors: %+v", res.Errors)
	}
	if res.Inserted != len(rows) {
		t.Fatalf("Import batch: inserted %d; want %d", res.Inserted, len(rows))
	}
}

// listInvoicesByCode reads every invoice for tenant and returns them keyed by their unique code.
func listInvoicesByCode(t *testing.T, recordSvc *RecordService, tenant string) map[string]dynrepo.Record {
	t.Helper()
	scope := RecordScope{UserID: uuid.NewString(), AccessLevel: "all", IsAdmin: true}
	res, err := recordSvc.List(context.Background(), scope, tenant, "invoices", ListQuery{
		Page: 1, Limit: 100, Sort: "created_at", Order: "asc",
	})
	if err != nil {
		t.Fatalf("List invoices: %v", err)
	}
	byCode := make(map[string]dynrepo.Record, len(res.Records))
	for _, r := range res.Records {
		code, _ := r.Data["code"].(string)
		byCode[code] = r
	}
	return byCode
}

// TestImportAutoNumber_TwoBatchesDistinctMonotonic imports two CSV batches into the SAME tenant and
// proves the assigned auto_number values across both batches are distinct, monotonic-increasing,
// correctly prefixed + zero-padded, and free of duplicates — and that the formula field still fills.
func TestImportAutoNumber_TwoBatchesDistinctMonotonic(t *testing.T) {
	importSvc, entitySvc, recordSvc := importAutonumWire(t)
	const tenant = "cccccccc-0c0c-0c0c-0c0c-cccccccccc01"
	setupImportInvoicesEntity(t, entitySvc, tenant)

	// Batch 1: rows A, B → INV-00001, INV-00002 (assignment follows CSV/validRow order).
	importInvoiceBatch(t, importSvc, tenant, [][2]string{{"A", "100"}, {"B", "200"}})
	// Batch 2: rows C, D → INV-00003, INV-00004 (sequence continues across batches).
	importInvoiceBatch(t, importSvc, tenant, [][2]string{{"C", "300"}, {"D", "400"}})

	byCode := listInvoicesByCode(t, recordSvc, tenant)
	if len(byCode) != 4 {
		t.Fatalf("expected 4 invoices, got %d", len(byCode))
	}

	// Per-code monotonic assignment (rows numbered in validRow order across the two batches).
	wantByCode := map[string]string{
		"A": "INV-00001",
		"B": "INV-00002",
		"C": "INV-00003",
		"D": "INV-00004",
	}
	for code, wantNo := range wantByCode {
		rec, ok := byCode[code]
		if !ok {
			t.Fatalf("invoice with code %q not found", code)
		}
		if got := rec.Data["invoice_no"]; got != wantNo {
			t.Errorf("code %q: invoice_no = %v; want %v (prefixed + padded, monotonic)", code, got, wantNo)
		}
	}

	// Collect ALL auto_number values → sorted set must be exactly the distinct, monotonic,
	// prefixed/padded range with no duplicates.
	var nos []string
	seen := make(map[string]struct{})
	for _, rec := range byCode {
		no, _ := rec.Data["invoice_no"].(string)
		if no == "" {
			t.Fatalf("an imported record has a blank invoice_no (auto_number not assigned): %+v", rec.Data)
		}
		if _, dup := seen[no]; dup {
			t.Fatalf("duplicate invoice_no %q across imported records", no)
		}
		seen[no] = struct{}{}
		nos = append(nos, no)
	}
	sort.Strings(nos)
	wantNos := []string{"INV-00001", "INV-00002", "INV-00003", "INV-00004"}
	if strings.Join(nos, ",") != strings.Join(wantNos, ",") {
		t.Errorf("invoice_no set = %v; want %v (distinct + monotonic + prefixed + padded)", nos, wantNos)
	}

	// Regression: the formula field must still fill on the import path (double_amount = amount * 2).
	wantDouble := map[string]float64{"A": 200, "B": 400, "C": 600, "D": 800}
	for code, want := range wantDouble {
		got, ok := byCode[code].Data["double_amount"].(float64)
		if !ok {
			t.Fatalf("code %q: double_amount missing or not a number: %v", code, byCode[code].Data["double_amount"])
		}
		if got != want {
			t.Errorf("code %q: double_amount = %v; want %v (formula regression)", code, got, want)
		}
	}
}

// TestImportAutoNumber_TenantScoped proves the sequence is tenant-scoped on the import path: two
// tenants registering the SAME entity name (sharing the physical obj_invoices table) each start
// their auto_number sequence at 1 — a second tenant's import never continues the first's counter.
func TestImportAutoNumber_TenantScoped(t *testing.T) {
	importSvc, entitySvc, recordSvc := importAutonumWire(t)
	const tenantA = "cccccccc-0c0c-0c0c-0c0c-ccccccccc0aa"
	const tenantB = "cccccccc-0c0c-0c0c-0c0c-ccccccccc0bb"
	setupImportInvoicesEntity(t, entitySvc, tenantA)
	setupImportInvoicesEntity(t, entitySvc, tenantB)

	// Tenant A imports two rows → INV-00001, INV-00002.
	importInvoiceBatch(t, importSvc, tenantA, [][2]string{{"A1", "10"}, {"A2", "20"}})
	// Tenant B imports one row → must be its OWN INV-00001, not INV-00003.
	importInvoiceBatch(t, importSvc, tenantB, [][2]string{{"B1", "30"}})

	byCodeA := listInvoicesByCode(t, recordSvc, tenantA)
	byCodeB := listInvoicesByCode(t, recordSvc, tenantB)

	if got := byCodeA["A1"].Data["invoice_no"]; got != "INV-00001" {
		t.Errorf("tenantA A1 invoice_no = %v; want INV-00001", got)
	}
	if got := byCodeA["A2"].Data["invoice_no"]; got != "INV-00002" {
		t.Errorf("tenantA A2 invoice_no = %v; want INV-00002", got)
	}
	if got := byCodeB["B1"].Data["invoice_no"]; got != "INV-00001" {
		t.Errorf("tenantB B1 invoice_no = %v; want INV-00001 (own independent counter, NOT INV-00003)", got)
	}
}
