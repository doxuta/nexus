//go:build integration

// Integration tests for Wave-5 W5-D query-engine correctness fixes, exercised end-to-end
// through the RecordService over a freshly-migrated throwaway MySQL DB (o3aSetup/o3aWire).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run W5D
//
// Covered:
//   - D1: eq/neq on number/currency/rating CAST to DECIMAL, and on date/datetime CAST to
//     DATETIME (after UTC canonicalization), so 10 == 10.0 and same-instant-different-offset
//     datetimes match. BEFORE the fix these compared raw strings and silently missed.
//   - D2: a PUT carrying an explicit null on an OPTIONAL field clears it (JSON_REMOVE via
//     JSON_MERGE_PATCH null), while a key absent from the body is preserved untouched, and an
//     explicit null on a REQUIRED field is rejected 422 RECORD_REQUIRED_MISSING.
package services

import (
	"context"
	"errors"
	"testing"
)

// w5dAdmin is the admin record scope used by these tests (full visibility, bypasses RLS).
func w5dAdmin(userID string) RecordScope {
	return RecordScope{UserID: userID, AccessLevel: "all", IsAdmin: true}
}

// ---------------------------------------------------------------------------
// D1 — eq/neq numeric + datetime correctness (instant/value, not string bytes).
// ---------------------------------------------------------------------------

func TestW5D_D1_EqNumberCastsToDecimal(t *testing.T) {
	_, _, entitySvc, _, recordSvc := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000001"

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "d1_nums",
		Label: "D1 Nums",
		Fields: []FieldInput{
			{Name: "label", Label: "Label", FieldType: "text"},
			{Name: "score", Label: "Score", FieldType: "number"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity d1_nums: %v", err)
	}

	// Stored as the JSON number 10 (float64 from a decoded body).
	if _, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "d1_nums", map[string]any{"label": "ten", "score": float64(10)}); err != nil {
		t.Fatalf("Create score=10: %v", err)
	}
	if _, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "d1_nums", map[string]any{"label": "seven", "score": float64(7)}); err != nil {
		t.Fatalf("Create score=7: %v", err)
	}

	// BEFORE the D1 fix: eq compared the JSON string "10" against the literal "10.0" → no match.
	// AFTER: both sides CAST to DECIMAL → 10 == 10.0 → exactly one row (the "ten" record).
	res, err := recordSvc.List(ctx, w5dAdmin(user), tenant, "d1_nums", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "score", Operator: "eq", Values: []string{"10.0"}}},
	})
	if err != nil {
		t.Fatalf("List eq score=10.0: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("eq score=10.0 total=%d; want 1 (10 == 10.0 by DECIMAL cast)", res.Total)
	}
	if got, _ := res.Records[0].Data["label"].(string); got != "ten" {
		t.Fatalf("eq score=10.0 matched label=%q; want \"ten\"", got)
	}

	// neq 10.0 must return the other row (seven), proving symmetry.
	resNeq, err := recordSvc.List(ctx, w5dAdmin(user), tenant, "d1_nums", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "score", Operator: "neq", Values: []string{"10.0"}}},
	})
	if err != nil {
		t.Fatalf("List neq score=10.0: %v", err)
	}
	if resNeq.Total != 1 {
		t.Fatalf("neq score=10.0 total=%d; want 1 (only seven)", resNeq.Total)
	}
	if got, _ := resNeq.Records[0].Data["label"].(string); got != "seven" {
		t.Fatalf("neq score=10.0 matched label=%q; want \"seven\"", got)
	}
}

func TestW5D_D1_EqDatetimeSameInstantAcrossOffsets(t *testing.T) {
	_, _, entitySvc, _, recordSvc := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000002"

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "d1_dt",
		Label: "D1 Datetime",
		Fields: []FieldInput{
			{Name: "label", Label: "Label", FieldType: "text"},
			{Name: "occurred_at", Label: "Occurred", FieldType: "datetime"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity d1_dt: %v", err)
	}

	// Stored as +07:00 → canonicalizes to 2026-06-01 05:00:00 UTC.
	if _, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "d1_dt", map[string]any{
		"label": "bangkok", "occurred_at": "2026-06-01T12:00:00+07:00",
	}); err != nil {
		t.Fatalf("Create occurred_at +07:00: %v", err)
	}
	// A different instant (one hour later, UTC) — must NOT match.
	if _, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "d1_dt", map[string]any{
		"label": "other", "occurred_at": "2026-06-01T06:00:00Z",
	}); err != nil {
		t.Fatalf("Create occurred_at Z (other): %v", err)
	}

	// Query with the SAME instant expressed in a DIFFERENT offset (Z). BEFORE the D1 fix this
	// compared raw strings ("2026-06-01T12:00:00+07:00" != "2026-06-01T05:00:00Z") → 0 rows.
	// AFTER: both canonicalize to the same UTC literal → exactly the bangkok row.
	res, err := recordSvc.List(ctx, w5dAdmin(user), tenant, "d1_dt", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "occurred_at", Operator: "eq", Values: []string{"2026-06-01T05:00:00Z"}}},
	})
	if err != nil {
		t.Fatalf("List eq datetime same-instant: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("eq datetime same-instant total=%d; want 1 (instant match across offsets)", res.Total)
	}
	if got, _ := res.Records[0].Data["label"].(string); got != "bangkok" {
		t.Fatalf("eq datetime matched label=%q; want \"bangkok\"", got)
	}

	// And the naive (offset-less) form of the same instant also matches.
	resNaive, err := recordSvc.List(ctx, w5dAdmin(user), tenant, "d1_dt", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "occurred_at", Operator: "eq", Values: []string{"2026-06-01T05:00:00"}}},
	})
	if err != nil {
		t.Fatalf("List eq datetime naive: %v", err)
	}
	if resNaive.Total != 1 {
		t.Fatalf("eq datetime naive total=%d; want 1", resNaive.Total)
	}
}

// ---------------------------------------------------------------------------
// D2 — partial-PUT clear semantics: explicit null clears optional; required rejects.
// ---------------------------------------------------------------------------

func TestW5D_D2_ExplicitNullClearsOptionalField(t *testing.T) {
	_, _, entitySvc, _, recordSvc := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "22222222-0000-0000-0000-000000000001"

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "d2_clear",
		Label: "D2 Clear",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "nickname", Label: "Nickname", FieldType: "text"}, // optional
		},
	}); err != nil {
		t.Fatalf("CreateEntity d2_clear: %v", err)
	}

	id, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "d2_clear", map[string]any{
		"name": "Tai", "nickname": "T",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Sanity: nickname is present before the clearing PUT.
	rec, err := recordSvc.Get(ctx, w5dAdmin(user), tenant, "d2_clear", id, nil, nil)
	if err != nil {
		t.Fatalf("Get before clear: %v", err)
	}
	if _, ok := rec.Data["nickname"]; !ok {
		t.Fatalf("precondition: nickname missing before clear; data=%v", rec.Data)
	}

	// PUT {nickname: null} — BEFORE the D2 fix isEmpty(nil) skipped it → field never cleared.
	// AFTER: explicit null is forwarded so JSON_MERGE_PATCH removes the key.
	if err := recordSvc.Update(ctx, w5dAdmin(user), tenant, "d2_clear", id, map[string]any{
		"nickname": nil,
	}, nil); err != nil {
		t.Fatalf("Update {nickname:null}: %v", err)
	}

	after, err := recordSvc.Get(ctx, w5dAdmin(user), tenant, "d2_clear", id, nil, nil)
	if err != nil {
		t.Fatalf("Get after clear: %v", err)
	}
	if v, ok := after.Data["nickname"]; ok {
		t.Fatalf("nickname NOT cleared by explicit null; still present = %v", v)
	}
	// The required field must be untouched (key-absent → preserved).
	if got, _ := after.Data["name"].(string); got != "Tai" {
		t.Fatalf("name changed unexpectedly: %q; want \"Tai\"", got)
	}
}

func TestW5D_D2_KeyAbsentPreservesOptionalField(t *testing.T) {
	_, _, entitySvc, _, recordSvc := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "22222222-0000-0000-0000-000000000002"

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "d2_keep",
		Label: "D2 Keep",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "nickname", Label: "Nickname", FieldType: "text"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity d2_keep: %v", err)
	}

	id, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "d2_keep", map[string]any{
		"name": "Tai", "nickname": "keepme",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// PUT that does NOT mention nickname at all → it must be PRESERVED (merge-only).
	if err := recordSvc.Update(ctx, w5dAdmin(user), tenant, "d2_keep", id, map[string]any{
		"name": "Tai2",
	}, nil); err != nil {
		t.Fatalf("Update {name:Tai2}: %v", err)
	}

	after, err := recordSvc.Get(ctx, w5dAdmin(user), tenant, "d2_keep", id, nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, _ := after.Data["nickname"].(string); got != "keepme" {
		t.Fatalf("absent key clobbered nickname=%q; want \"keepme\" (merge must preserve)", got)
	}
	if got, _ := after.Data["name"].(string); got != "Tai2" {
		t.Fatalf("name=%q; want \"Tai2\"", got)
	}
}

func TestW5D_D2_ExplicitNullOnRequiredRejected(t *testing.T) {
	_, _, entitySvc, _, recordSvc := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "22222222-0000-0000-0000-000000000003"

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "d2_req",
		Label: "D2 Required",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity d2_req: %v", err)
	}

	id, err := recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "d2_req", map[string]any{"name": "Tai"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// PUT {name: null} on a REQUIRED field → 422 RECORD_REQUIRED_MISSING (can't blank required).
	err = recordSvc.Update(ctx, w5dAdmin(user), tenant, "d2_req", id, map[string]any{
		"name": nil,
	}, nil)
	if err == nil {
		t.Fatalf("Update {name:null} on required field succeeded; want RECORD_REQUIRED_MISSING")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "RECORD_REQUIRED_MISSING" {
		t.Fatalf("Update {name:null} required: got err=%v; want ValidationError RECORD_REQUIRED_MISSING", err)
	}

	// The stored value must be unchanged after the rejected update.
	after, err := recordSvc.Get(ctx, w5dAdmin(user), tenant, "d2_req", id, nil, nil)
	if err != nil {
		t.Fatalf("Get after reject: %v", err)
	}
	if got, _ := after.Data["name"].(string); got != "Tai" {
		t.Fatalf("name changed after rejected null update: %q; want \"Tai\"", got)
	}
}
