//go:build integration

// Queue #63 — the D2 report-only currency sweep over real MySQL 8 (drWire throwaway DB). Proofs a
// unit fake cannot give: the JSON predicate itself (nested key, JSON_TYPE, TRIM, the utf8mb4_bin
// exact comparison that makes "vnd" a finding like the write guard does), the soft-delete filter,
// and tenant isolation on a SHARED obj_ table.
package services

import (
	"context"
	"testing"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
)

func TestDoctorCurrencyCodeMismatch_ReportsLegacyRowsTenantScoped(t *testing.T) {
	svc, db := drWire(t)
	dyn := dynrepo.New(db)
	ctx := context.Background()
	const tenantA = "d2a2a2a2-0000-0000-0000-00000000000a"
	const tenantB = "d2b2b2b2-0000-0000-0000-00000000000b"
	const vnd = `{"currencyCode":"VND"}`

	// Shared entity d2_deals, VND-configured `amount`, active for BOTH tenants.
	entA := dsSeedEntity(t, db, tenantA, "d2_deals")
	dsSeedField(t, db, tenantA, entA, "amount", "currency", false, vnd)
	entB := dsSeedEntity(t, db, tenantB, "d2_deals")
	dsSeedField(t, db, tenantB, entB, "amount", "currency", false, vnd)
	if err := dyn.CreateTable(ctx, "d2_deals", []ddl.FieldDef{{Name: "amount", FieldType: "currency"}}); err != nil {
		t.Fatalf("create obj_d2_deals: %v", err)
	}

	// InsertRecord writes straight to the store (no validation) — the only way such rows exist
	// since the Q7 guard.
	insert := func(tenant string, amount any) string {
		t.Helper()
		id, err := dyn.InsertRecord(ctx, "d2_deals", tenant, "u-"+tenant[:4], map[string]any{"amount": amount}, nil)
		if err != nil {
			t.Fatalf("insert %s %v: %v", tenant, amount, err)
		}
		return id
	}
	money := func(a float64, code string) map[string]any { return map[string]any{"amount": a, "currencyCode": code} }

	idUSD := insert(tenantA, money(100, "USD"))
	idLower := insert(tenantA, money(5, "vnd"))
	insert(tenantA, money(250000, "VND")) // clean
	insert(tenantA, money(7, " VND "))    // same code after TRIM — clean (write-guard parity)
	insert(tenantA, money(9, ""))         // no code — not a relabel candidate
	insert(tenantA, 42.0)                 // bare legacy number, no code at all
	idDeleted := insert(tenantA, money(1, "EUR"))
	dsExec(t, db, "UPDATE `obj_d2_deals` SET `deleted_at` = NOW() WHERE `tenant_id` = ? AND `id` = ?", tenantA, idDeleted)
	insert(tenantB, money(300, "USD")) // co-tenant's dirty row on the SAME table — never tenant A's finding

	got := drScanCounts(t, svc, tenantA)[DoctorCheckCurrencyCodeMismatch]
	if !got.ReportOnly {
		t.Fatalf("D2 = %+v; want reportOnly", got)
	}
	keys := map[string]bool{}
	for _, it := range got.Items {
		keys[it.Key] = true
	}
	if got.Count != 2 || !keys["d2_deals.amount:"+idUSD] || !keys["d2_deals.amount:"+idLower] {
		t.Fatalf("D2 tenant A = %+v; want exactly the USD row %s and the lowercase vnd row %s", got, idUSD, idLower)
	}

	if b := drScanCounts(t, svc, tenantB)[DoctorCheckCurrencyCodeMismatch]; b.Count != 1 {
		t.Fatalf("D2 tenant B = %+v; want its own single USD row", b)
	}

	// Report-only: the fix endpoint refuses before any mutation, and the rows stay as they are.
	if _, err := svc.Fix(ctx, tenantA, []string{DoctorCheckCurrencyCodeMismatch}, true); codeOf(err) != "DOCTOR_REPORT_ONLY" {
		t.Fatalf("fix D2 err = %v; want DOCTOR_REPORT_ONLY", err)
	}
	if again := drScanCounts(t, svc, tenantA)[DoctorCheckCurrencyCodeMismatch]; again.Count != 2 {
		t.Fatalf("D2 after refused fix = %+v; want the same 2 findings", again)
	}
}
