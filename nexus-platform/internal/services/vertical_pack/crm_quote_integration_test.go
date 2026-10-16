//go:build integration

// Integration test for the CRM pack's QUOTE-LINE-ITEMS enrichment (v1.3.0): a Quote's total is
// the sum of its line items' (quantity × unit_price), composed from THREE existing engine
// primitives — a `formula` field (line_total, write-time, resultType "number"), a `rollup` field
// (total, read-time sum, opt-in via ?rollup=) over the 1-N `quote` relation, and the 2026-07-12
// rollup-over-formula guard relaxation (rollup_validate.go) that makes the composition legal.
//
// THE KEY ASSERTION (composition works E2E): install the CRM pack → read a seeded quote with
// ?rollup=total → assert total == the exact Σ(quantity × unit_price) across that quote's line
// items, AND a single line's line_total == quantity × unit_price. Sample values are picked so a
// broken composition (rollup summing 0/NULL, or line_total not computing) gives a visibly
// different number:
//   - @quote_acme: 3×500000 + 1×2000000 + 2×750000 = 1,500,000 + 2,000,000 + 1,500,000 = 5,000,000 VND.
//   - @quote_globex: 5×1200000 + 1×3500000 = 6,000,000 + 3,500,000 = 9,500,000 VND.
//
// It reuses the harness in vertical_pack_integration_test.go (vpSetup / vpBuildWF / vpTenantA /
// vpSeedUserID). vpBuildWF (not vpBuild) is used because crmPack ships Teams + Workflows
// (PACK-TEAMS/PACK-WORKFLOWS) — without WorkflowWriter/TeamWriter wired those passes silently
// skip, mirroring lms_integration_test.go / crm_automation_integration_test.go.
package vertical_pack

import (
	"context"
	"strconv"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// TestVerticalPack_InstallCRMQuoteLineItems installs the built-in CRM pack and asserts the
// QUOTE-LINE-ITEMS composition: crm_quotes.total (rollup sum) correctly aggregates
// crm_quote_lines.line_total (formula quantity×unit_price) end to end against real MySQL.
func TestVerticalPack_InstallCRMQuoteLineItems(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildWF(gdb, sdb)
	ctx := context.Background()

	// PACK-TEAMS needs a REAL install user (auth.TeamRepository.AddMember rejects a non-live
	// user). tenants(vpTenantA) is seeded by vpSetup.
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "installer@crm-quote-it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}

	pack, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install builtin crm: %v", err)
	}

	// --- #1: crm_quotes / crm_quote_lines entities + the deferred crm_quotes.total rollup exist. ---
	metaRepo := metadata.NewRepository(gdb)
	entities, err := metaRepo.ListEntities(vpTenantA)
	if err != nil {
		t.Fatalf("list entities: %v", err)
	}
	entByName := map[string]metadata.EntityDefinition{}
	for _, e := range entities {
		entByName[e.Name] = e
	}
	for _, want := range []string{"crm_quotes", "crm_quote_lines"} {
		if _, ok := entByName[want]; !ok {
			t.Fatalf("crm: entity %q not found after install; got %v", want, keys(entByName))
		}
	}
	quoteFields, err := metaRepo.ListFields(ctx, vpTenantA, entByName["crm_quotes"].ID)
	if err != nil {
		t.Fatalf("list crm_quotes fields: %v", err)
	}
	var totalField *metadata.FieldDefinition
	for i := range quoteFields {
		if quoteFields[i].Name == "total" {
			totalField = &quoteFields[i]
		}
	}
	if totalField == nil {
		t.Fatal("crm_quotes.total (deferred rollup) missing")
	}
	if totalField.FieldType != "rollup" {
		t.Errorf("crm_quotes.total fieldType = %q, want rollup", totalField.FieldType)
	}
	lineFields, err := metaRepo.ListFields(ctx, vpTenantA, entByName["crm_quote_lines"].ID)
	if err != nil {
		t.Fatalf("list crm_quote_lines fields: %v", err)
	}
	var lineTotalField *metadata.FieldDefinition
	for i := range lineFields {
		if lineFields[i].Name == "line_total" {
			lineTotalField = &lineFields[i]
		}
	}
	if lineTotalField == nil {
		t.Fatal("crm_quote_lines.line_total (formula) missing")
	}
	if lineTotalField.FieldType != "formula" {
		t.Errorf("crm_quote_lines.line_total fieldType = %q, want formula", lineTotalField.FieldType)
	}

	// Read-side RecordService with the SAME production wiring as the LMS/CRM tests' record
	// writer (rollup opt-in needs WithRollupMeta/WithRollupLinked).
	dyn := dynrepo.New(sdb)
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dyn).
		WithSequenceClaimer(dyn)
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}

	// --- #2: samples seeded — 2 quotes + 5 quote_lines. ---
	quoteList, err := recordSvc.List(ctx, scope, vpTenantA, "crm_quotes", services.ListQuery{Page: 1, Limit: 20, Sort: "created_at", Order: "asc"})
	if err != nil {
		t.Fatalf("list crm_quotes: %v", err)
	}
	if len(quoteList.Records) != 2 {
		t.Fatalf("crm: %d quotes, want 2", len(quoteList.Records))
	}
	lineList, err := recordSvc.List(ctx, scope, vpTenantA, "crm_quote_lines", services.ListQuery{Page: 1, Limit: 20, Sort: "created_at", Order: "asc"})
	if err != nil {
		t.Fatalf("list crm_quote_lines: %v", err)
	}
	if len(lineList.Records) != 5 {
		t.Fatalf("crm: %d quote_lines, want 5", len(lineList.Records))
	}

	// --- #3: a single line's line_total == quantity × unit_price (proves the formula computes). ---
	// "Nexus License" on @quote_acme: quantity=3, unit_price=500000 → line_total = 1,500,000.
	var nexusLine *dynrepo.Record
	for i := range lineList.Records {
		if name, _ := lineList.Records[i].Data["product_name"].(string); name == "Nexus License" {
			nexusLine = &lineList.Records[i]
		}
	}
	if nexusLine == nil {
		t.Fatal("could not find quote line 'Nexus License'")
	}
	gotQty, _ := nexusLine.Data["quantity"].(float64)
	if gotQty != 3 {
		t.Fatalf("Nexus License quantity = %v, want 3 (fixture drifted)", nexusLine.Data["quantity"])
	}
	unitPrice, ok := nexusLine.Data["unit_price"].(map[string]any)
	if !ok {
		t.Fatalf("Nexus License unit_price = %v (%T), want currency object", nexusLine.Data["unit_price"], nexusLine.Data["unit_price"])
	}
	gotUnitPrice, _ := unitPrice["amount"].(float64)
	if gotUnitPrice != 500000 {
		t.Fatalf("Nexus License unit_price.amount = %v, want 500000 (fixture drifted)", unitPrice["amount"])
	}
	gotLineTotal, ok := nexusLine.Data["line_total"].(float64)
	if !ok {
		t.Fatalf("Nexus License line_total = %v (%T), want a plain number (formula resultType \"number\")", nexusLine.Data["line_total"], nexusLine.Data["line_total"])
	}
	const wantLineTotal = 3 * 500000.0
	if gotLineTotal != wantLineTotal {
		t.Errorf("Nexus License line_total = %v, want %v (quantity × unit_price — formula not computing)", gotLineTotal, wantLineTotal)
	}

	// --- #4: THE KEY ASSERTION — crm_quotes.total (rollup sum) == Σ(quantity × unit_price). ---
	// Locate quotes by name (install does not expose the refMap).
	quoteByName := map[string]string{}
	for _, r := range quoteList.Records {
		name, _ := r.Data["name"].(string)
		quoteByName[name] = r.ID
	}
	for _, tc := range []struct {
		name      string
		wantTotal float64
	}{
		// @quote_acme: 3×500000 + 1×2000000 + 2×750000 = 1,500,000+2,000,000+1,500,000 = 5,000,000.
		{"Báo giá mở rộng Acme Q1", 5000000},
		// @quote_globex: 5×1200000 + 1×3500000 = 6,000,000+3,500,000 = 9,500,000.
		{"Báo giá triển khai Globex Enterprise", 9500000},
	} {
		id, ok := quoteByName[tc.name]
		if !ok {
			t.Fatalf("seed quote %q not found; got %v", tc.name, keys(quoteByName))
		}
		quote, err := recordSvc.Get(ctx, scope, vpTenantA, "crm_quotes", id, nil, []string{"total"})
		if err != nil {
			t.Fatalf("get quote %q with rollup=total: %v", tc.name, err)
		}
		// sum on a non-currency (number) aggField → a bare exact DECIMAL string (ToJSONValue's
		// non-currency branch), mirroring the LMS gradebook avg_grade assertion.
		totalRaw, ok := quote.Data["total"].(string)
		if !ok {
			t.Fatalf("quote %q total = %v (%T), want a bare DECIMAL string (sum over a number-resultType formula aggField)", tc.name, quote.Data["total"], quote.Data["total"])
		}
		gotTotal, err := strconv.ParseFloat(totalRaw, 64)
		if err != nil {
			t.Fatalf("quote %q total %q does not parse as a number: %v", tc.name, totalRaw, err)
		}
		if gotTotal != tc.wantTotal {
			t.Errorf("quote %q total = %v (%q), want %v — broken composition (rollup summing 0/NULL, or line_total not computing) would give a visibly different number", tc.name, gotTotal, totalRaw, tc.wantTotal)
		}
	}

	// --- #5: WITHOUT ?rollup= the total field must not resolve (opt-in contract, G12). ---
	acmeID := quoteByName["Báo giá mở rộng Acme Q1"]
	plain, err := recordSvc.Get(ctx, scope, vpTenantA, "crm_quotes", acmeID, nil, nil)
	if err != nil {
		t.Fatalf("get quote without rollup: %v", err)
	}
	if v, present := plain.Data["total"]; present && v != nil {
		t.Errorf("without ?rollup=, total = %v — must be absent or null (rollup is opt-in)", v)
	}

	// --- #6: idempotent re-install — the 2 new entities are skipped, no duplicate samples math
	// (mirrors TestVerticalPack_InstallBuiltin's precedent). ---
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("re-install crm: %v", err)
	}
	skippedSet := map[string]bool{}
	for _, e := range res2.EntitiesSkipped {
		skippedSet[e] = true
	}
	for _, want := range []string{"crm_quotes", "crm_quote_lines"} {
		if !skippedSet[want] {
			t.Errorf("re-install: %q not in EntitiesSkipped (%v)", want, res2.EntitiesSkipped)
		}
	}
	if len(res.EntitiesCreated) == 0 {
		t.Fatal("sanity: first install created zero entities")
	}
}
