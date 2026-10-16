//go:build integration

// LENS R — FIELD-MUTATION invalidation gate for the O3a metadata-cache. The existing wired test
// (metadata_cache_wired_integration_test.go) covers cross-tenant serve, single-flight non-merge,
// invalidate-mid-load, and SoftDeleteEntity. It does NOT exercise the three FIELD write hooks:
// AddField / EditField / DeleteField. Those are exactly the LENS R primed scenarios — a missing or
// misplaced InvalidateFields hook there means RecordService.resolve serves a STALE field list:
//   - AddField missing  -> a newly-added field is rejected as RECORD_UNKNOWN_FIELD.
//   - EditField missing -> a changed constraint (e.g. now-required) never takes effect.
//   - DeleteField missing -> a removed field is still accepted.
//
// This file REUSES the production wiring from o3aWire (ONE shared MetaCache feeding the entity +
// field write services AND the read-through RecordService — mirroring cmd/server/main.go) and the
// countingCatalogReal decorator (so a "warm" read is provably a cache HIT, not a fresh load).
//
// Teeth: each scenario WARMS the cache first (resolve through it with the OLD metadata, asserted
// hot via 0 extra DB hits on a repeat read), uses the LONG DefaultMetaTTL (60s — so only an
// invalidation hook, never TTL expiry, can make the new metadata visible mid-test), then mutates
// and asserts the NEW behavior end-to-end through RecordService.Create.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestO3aFieldInval -race -v
package services

import (
	"context"
	"sync/atomic"
	"testing"
)

// warmEntity resolves the entity through the cache (entity + fields), then proves the entry is hot
// by a repeat read that adds ZERO entity DB hits. It returns after the OLD field list is cached.
func warmEntity(t *testing.T, ctx context.Context, counter *countingCatalogReal, recordSvc *RecordService, tenant, user, entity string, payload map[string]any) {
	t.Helper()
	if _, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, entity, payload); err != nil {
		t.Fatalf("warm Create(%s): %v", entity, err)
	}
	hits := atomic.LoadInt64(&counter.entityCalls)
	if _, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, entity, payload); err != nil {
		t.Fatalf("warm repeat Create(%s): %v", entity, err)
	}
	if got := atomic.LoadInt64(&counter.entityCalls); got != hits {
		t.Fatalf("warm did NOT cache %s: entity DB hits %d -> %d on repeat read (cache not serving — test has no teeth)", entity, hits, got)
	}
}

// TestO3aFieldInval_AddField: warm (resolve with field A cached), AddField B, then a record using
// the NEW field B must SUCCEED — proving InvalidateFields fired after the AddField commit. A stale
// field list would reject B as RECORD_UNKNOWN_FIELD.
func TestO3aFieldInval_AddField(t *testing.T) {
	counter, _, entitySvc, fieldSvc, recordSvc := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "fi_add", Label: "FI Add",
		Fields: []FieldInput{{Name: "a_field", Label: "A", FieldType: "text"}},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	warmEntity(t, ctx, counter, recordSvc, o3aTenantA, "u-a", "fi_add", map[string]any{"a_field": "x"})

	// Sanity: BEFORE adding b_field, the cached resolve must reject it (proves cache is hot & B unknown).
	if _, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, o3aTenantA, "u-a", "fi_add", map[string]any{"a_field": "y", "b_field": "z"}); codeOf(err) != "RECORD_UNKNOWN_FIELD" {
		t.Fatalf("pre-add sanity: insert with b_field gave code=%q (err=%v); want RECORD_UNKNOWN_FIELD (field not yet added)", codeOf(err), err)
	}

	// MUTATE: add b_field (InvalidateFields fires after commit).
	if _, err := fieldSvc.AddField(ctx, o3aTenantA, AddFieldInput{
		EntityName: "fi_add", Field: FieldInput{Name: "b_field", Label: "B", FieldType: "text"},
	}); err != nil {
		t.Fatalf("AddField b_field: %v", err)
	}

	// BEHAVIORAL: insert USING the new field must now SUCCEED through the cached resolve.
	if _, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, o3aTenantA, "u-a", "fi_add", map[string]any{"a_field": "y", "b_field": "newval"}); err != nil {
		t.Errorf("STALE-CACHE BUG: insert with newly-added b_field failed after AddField (err=%v); InvalidateFields hook missing/misplaced?", err)
	}
	t.Logf("OK AddField: pre-add rejects b_field (cache hot); post-add accepts it (hook fired)")
}

// TestO3aFieldInval_EditField: warm (field opt NOT required, omission accepted), EditField opt ->
// required, then an insert OMITTING opt must now be rejected RECORD_REQUIRED_MISSING.
func TestO3aFieldInval_EditField(t *testing.T) {
	counter, _, entitySvc, fieldSvc, recordSvc := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()

	_, fields, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "fi_edit", Label: "FI Edit",
		Fields: []FieldInput{
			{Name: "keep", Label: "Keep", FieldType: "text"},
			{Name: "opt", Label: "Opt", FieldType: "text"}, // not required yet
		},
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	var optID string
	for _, f := range fields {
		if f.Name == "opt" {
			optID = f.ID
		}
	}
	if optID == "" {
		t.Fatalf("opt field id not found")
	}

	// WARM: omitting opt is fine now (not required) — caches the OLD (non-required) field list.
	warmEntity(t, ctx, counter, recordSvc, o3aTenantA, "u-a", "fi_edit", map[string]any{"keep": "k"})

	// MUTATE: make opt required (InvalidateFields fires after commit).
	req := true
	if _, err := fieldSvc.EditField(ctx, o3aTenantA, EditFieldInput{FieldID: optID, IsRequired: &req}); err != nil {
		t.Fatalf("EditField make-required: %v", err)
	}

	// BEHAVIORAL: omitting opt must now be rejected — the changed constraint took effect.
	if _, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, o3aTenantA, "u-a", "fi_edit", map[string]any{"keep": "k2"}); codeOf(err) != "RECORD_REQUIRED_MISSING" {
		t.Errorf("STALE-CACHE BUG: new required constraint did NOT take effect after EditField (code=%q, err=%v); want RECORD_REQUIRED_MISSING", codeOf(err), err)
	}
	t.Logf("OK EditField: required constraint enforced after invalidation (RECORD_REQUIRED_MISSING)")
}

// TestO3aFieldInval_DeleteField: warm (field gone present & accepted), DeleteField gone, then an
// insert USING gone must now be rejected RECORD_UNKNOWN_FIELD.
func TestO3aFieldInval_DeleteField(t *testing.T) {
	counter, _, entitySvc, fieldSvc, recordSvc := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()

	_, fields, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "fi_del", Label: "FI Del",
		Fields: []FieldInput{
			{Name: "keep", Label: "Keep", FieldType: "text"},
			{Name: "gone", Label: "Gone", FieldType: "text"},
		},
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	var goneID string
	for _, f := range fields {
		if f.Name == "gone" {
			goneID = f.ID
		}
	}
	if goneID == "" {
		t.Fatalf("gone field id not found")
	}

	// WARM: insert USING gone — succeeds now and caches the OLD field list (gone present).
	warmEntity(t, ctx, counter, recordSvc, o3aTenantA, "u-a", "fi_del", map[string]any{"keep": "k", "gone": "g"})

	// MUTATE: delete the gone field (InvalidateFields fires after commit).
	if err := fieldSvc.DeleteField(ctx, o3aTenantA, DeleteFieldInput{FieldID: goneID}); err != nil {
		t.Fatalf("DeleteField: %v", err)
	}

	// BEHAVIORAL: insert USING the deleted field must now be rejected as unknown.
	if _, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, o3aTenantA, "u-a", "fi_del", map[string]any{"keep": "k2", "gone": "g2"}); codeOf(err) != "RECORD_UNKNOWN_FIELD" {
		t.Errorf("STALE-CACHE BUG: deleted field still accepted after DeleteField (code=%q, err=%v); want RECORD_UNKNOWN_FIELD", codeOf(err), err)
	}
	t.Logf("OK DeleteField: removed field rejected after invalidation (RECORD_UNKNOWN_FIELD)")
}
