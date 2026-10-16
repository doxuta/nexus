//go:build integration

// ENG-COMPUTED #2/#3 (read-time lookup + rollup) integration tests — exercised end-to-end
// through EntityService/RecordService/RelationService over a freshly-migrated throwaway MySQL
// DB (o3aSetup helpers from metadata_cache_wired_integration_test.go). Covers everything that
// cannot be proven with fakes: tenant isolation, soft-delete exclusion, exact DECIMAL (no float
// drift), N+1 batching (real query-count spy via a counting wrapper), and the N-N pivot path.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestEngComputed -race -v
package services

import (
	"context"
	"sync/atomic"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

const (
	engTenantA = "cccccccc-0c0c-0c0c-0c0c-cccccccccccc"
	engTenantB = "dddddddd-0d0d-0d0d-0d0d-dddddddddddd"
)

func engAdmin(userID string) RecordScope {
	return RecordScope{UserID: userID, AccessLevel: "all", IsAdmin: true}
}

// countingFieldFetcher wraps a FieldValueFetcher (real *dynrepo.Repository) and counts calls
// per targetEntity, so a test can assert the N+1 batching contract against REAL SQL (not a
// fake's bookkeeping).
type countingFieldFetcher struct {
	inner FieldValueFetcher
	calls int64
}

func (c *countingFieldFetcher) BatchFetchField(ctx context.Context, targetEntity, tenantID, sourceField string, ids []string) ([]dynrepo.FieldValueResult, error) {
	atomic.AddInt64(&c.calls, 1)
	return c.inner.BatchFetchField(ctx, targetEntity, tenantID, sourceField, ids)
}

// engWireRecordService builds a RecordService wired exactly like production (main.go): the
// same *dynrepo.Repository backs LabelFetcher/FieldValueFetcher/RollupLinked, and
// *metadata.Repository backs rollupMeta — over a fresh throwaway DB. Returns the entity/field
// services too so a test can register entities/fields and the raw dynRepo for direct SQL
// helpers (e.g. wiring a counting fetcher).
func engWire(t *testing.T) (*EntityService, *FieldService, *RecordService, *dynrepo.Repository, *metadata.Repository) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	fieldSvc := NewFieldService(dynRepo, metaRepo)
	recordSvc := NewRecordService(metaRepo, dynRepo).
		WithLabelFetcher(dynRepo).
		WithFieldValueFetcher(dynRepo).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dynRepo)
	return entitySvc, fieldSvc, recordSvc, dynRepo, metaRepo
}

// ---------------------------------------------------------------------------
// Lookup — resolution, null-via, soft-delete, tenant isolation, N+1 batching.
// ---------------------------------------------------------------------------

func setupLookupEntities(t *testing.T, entitySvc *EntityService, tenant string) {
	t.Helper()
	if _, _, err := entitySvc.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name:  "accounts",
		Label: "Accounts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "industry", Label: "Industry", FieldType: "text"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity accounts: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name:  "contacts",
		Label: "Contacts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "account", Label: "Account", FieldType: "relation", Options: map[string]any{"relatedEntity": "accounts"}},
			{Name: "account_industry", Label: "Account Industry", FieldType: "lookup",
				Options: map[string]any{"via": "account", "sourceField": "industry"}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity contacts: %v", err)
	}
}

func TestEngComputed_Lookup_ResolvesFromRelatedRecord(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const user = "11111111-1111-1111-1111-111111111101"
	setupLookupEntities(t, entitySvc, engTenantA)

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{
		"name": "Acme Corp", "industry": "Manufacturing",
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	contactID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "contacts", map[string]any{
		"name": "Jane Doe", "account": accID,
	})
	if err != nil {
		t.Fatalf("create contact: %v", err)
	}

	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "contacts", contactID, nil, nil)
	if err != nil {
		t.Fatalf("Get contact: %v", err)
	}
	if got := rec.Data["account_industry"]; got != "Manufacturing" {
		t.Fatalf("account_industry = %v; want Manufacturing", got)
	}
}

func TestEngComputed_Lookup_NullViaFK_ResolvesNull(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const user = "11111111-1111-1111-1111-111111111102"
	setupLookupEntities(t, entitySvc, engTenantA)

	contactID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "contacts", map[string]any{
		"name": "No Account",
	})
	if err != nil {
		t.Fatalf("create contact: %v", err)
	}
	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "contacts", contactID, nil, nil)
	if err != nil {
		t.Fatalf("Get contact: %v", err)
	}
	if v, ok := rec.Data["account_industry"]; !ok || v != nil {
		t.Fatalf("account_industry = %v (ok=%v); want explicit nil (no via-FK)", v, ok)
	}
}

// A linked record that never had the source field set: JSON_EXTRACT returns SQL NULL for the
// missing path. Found 2026-09-30 in the #263 acceptance — a rep opening their own opportunity whose
// account had no industry got "Không tải được biểu mẫu" (500: `Scan error … storing driver.Value
// type <nil> into type *json.RawMessage`). The lookup must read as null, the record must load.
func TestEngComputed_Lookup_SourceFieldNeverSet_ResolvesNull(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const user = "11111111-1111-1111-1111-111111111109"
	setupLookupEntities(t, entitySvc, engTenantA)

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{"name": "No Industry Ltd"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	contactID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "contacts", map[string]any{"name": "Jane", "account": accID})
	if err != nil {
		t.Fatalf("create contact: %v", err)
	}
	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "contacts", contactID, nil, nil)
	if err != nil {
		t.Fatalf("Get contact whose account has no industry: %v", err)
	}
	if v, ok := rec.Data["account_industry"]; !ok || v != nil {
		t.Fatalf("account_industry = %v (ok=%v); want explicit nil (source field never set)", v, ok)
	}
}

func TestEngComputed_Lookup_SoftDeletedSource_ResolvesNull(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const user = "11111111-1111-1111-1111-111111111103"
	setupLookupEntities(t, entitySvc, engTenantA)

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{
		"name": "Doomed Co", "industry": "Retail",
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	contactID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "contacts", map[string]any{
		"name": "Jane Doe", "account": accID,
	})
	if err != nil {
		t.Fatalf("create contact: %v", err)
	}

	// Sanity: resolves before the source is deleted.
	before, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "contacts", contactID, nil, nil)
	if err != nil {
		t.Fatalf("Get before delete: %v", err)
	}
	if before.Data["account_industry"] != "Retail" {
		t.Fatalf("precondition: account_industry = %v; want Retail", before.Data["account_industry"])
	}

	if err := recordSvc.Delete(ctx, engAdmin(user), engTenantA, "accounts", accID); err != nil {
		t.Fatalf("soft-delete account: %v", err)
	}

	after, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "contacts", contactID, nil, nil)
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if v, ok := after.Data["account_industry"]; !ok || v != nil {
		t.Fatalf("account_industry after soft-delete = %v (ok=%v); want nil (deleted_at IS NULL guard)", v, ok)
	}
}

// TestEngComputed_Lookup_TenantIsolation: a contact's via-FK stores a raw UUID that, purely by
// coincidence of test fixtures, could collide across tenants if BatchFetchField ever forgot its
// tenant_id filter. Here we plant a tenant-B account with the SAME id as a tenant-A contact's
// via-FK points at NOTHING in tenant A — proving a cross-tenant id never resolves.
// TestEngComputed_Lookup_TenantIsolation plants a cross-tenant FK directly via dynRepo
// (bypassing RecordService.Update's relation-target-tenant validation, which would legitimately
// reject a cross-tenant id at WRITE time — exactly the guard this test isolates AROUND, to prove
// the READ-time BatchFetchField tenant filter independently) and asserts a tenant-A contact
// whose stored `account` FK points at tenant B's account resolves the lookup to nil, never B's
// data.
func TestEngComputed_Lookup_TenantIsolation(t *testing.T) {
	entitySvc, _, recordSvc, dynRepo, _ := engWire(t)
	ctx := context.Background()
	const userA = "11111111-1111-1111-1111-111111111104"
	const userB = "22222222-2222-2222-2222-222222222204"
	setupLookupEntities(t, entitySvc, engTenantA)
	setupLookupEntities(t, entitySvc, engTenantB)

	// Tenant B account — this must NEVER be readable through tenant A's lookup.
	accB, err := recordSvc.Create(ctx, engAdmin(userB), engTenantB, userB, "accounts", map[string]any{
		"name": "B Corp", "industry": "Finance",
	})
	if err != nil {
		t.Fatalf("create B account: %v", err)
	}

	accA, err := recordSvc.Create(ctx, engAdmin(userA), engTenantA, userA, "accounts", map[string]any{
		"name": "A Corp", "industry": "Manufacturing",
	})
	if err != nil {
		t.Fatalf("create A account: %v", err)
	}
	contactID, err := recordSvc.Create(ctx, engAdmin(userA), engTenantA, userA, "contacts", map[string]any{
		"name": "Jane Doe", "account": accA,
	})
	if err != nil {
		t.Fatalf("create A contact: %v", err)
	}

	// Sanity: resolves correctly against A's own account.
	rec, err := recordSvc.Get(ctx, engAdmin(userA), engTenantA, "contacts", contactID, nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Data["account_industry"] != "Manufacturing" {
		t.Fatalf("precondition: account_industry = %v; want Manufacturing", rec.Data["account_industry"])
	}

	// Directly rewrite the stored via-FK to point at tenant B's account id (goes straight
	// through dynRepo, bypassing RecordService's relation-target-tenant check — simulates a
	// forged/stale cross-tenant FK already sitting in storage, isolating the READ path).
	allScope := dynrepo.RowScope{Level: dynrepo.AccessAll, IsAdmin: true}
	if err := dynRepo.UpdateRecord(ctx, "contacts", engTenantA, contactID, userA, allScope,
		map[string]any{"account": accB}, nil, nil); err != nil {
		t.Fatalf("raw update via-FK to cross-tenant id: %v", err)
	}

	after, err := recordSvc.Get(ctx, engAdmin(userA), engTenantA, "contacts", contactID, nil, nil)
	if err != nil {
		t.Fatalf("Get after cross-tenant FK plant: %v", err)
	}
	if v, ok := after.Data["account_industry"]; !ok || v != nil {
		t.Fatalf("CROSS-TENANT LEAK: account_industry = %v (ok=%v); want nil (B's account must not resolve under A's tenant)", v, ok)
	}
}

func TestEngComputed_Lookup_BatchedNoNPlusOne(t *testing.T) {
	entitySvc, _, recordSvc, dynRepo, _ := engWire(t)
	ctx := context.Background()
	const user = "11111111-1111-1111-1111-111111111105"
	setupLookupEntities(t, entitySvc, engTenantA)

	// 3 source accounts.
	accIDs := make([]string, 3)
	industries := []string{"Manufacturing", "Retail", "Finance"}
	for i := range accIDs {
		id, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{
			"name": "Acc", "industry": industries[i],
		})
		if err != nil {
			t.Fatalf("create account %d: %v", i, err)
		}
		accIDs[i] = id
	}

	// 100 contacts spread across the 3 accounts.
	for i := 0; i < 100; i++ {
		if _, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "contacts", map[string]any{
			"name": "Contact", "account": accIDs[i%3],
		}); err != nil {
			t.Fatalf("create contact %d: %v", i, err)
		}
	}

	// Rewire recordSvc with a counting fetcher wrapping the SAME dynRepo, so List's applyLookups
	// query count is observable.
	counting := &countingFieldFetcher{inner: dynRepo}
	spySvc := NewRecordService(recordSvc.catalog, dynRepo).
		WithFieldValueFetcher(counting)

	res, err := spySvc.List(ctx, engAdmin(user), engTenantA, "contacts", ListQuery{Page: 1, Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Total != 100 {
		t.Fatalf("total = %d; want 100", res.Total)
	}
	if got := atomic.LoadInt64(&counting.calls); got != 1 {
		t.Fatalf("BatchFetchField calls = %d; want exactly 1 (100 parents -> 3 sources -> 1 batched query)", got)
	}
}

// ---------------------------------------------------------------------------
// Rollup — exact DECIMAL, soft-delete exclusion, tenant isolation, N-N pivot, ?rollup= opt-in.
// ---------------------------------------------------------------------------

func setupRollupEntities(t *testing.T, entitySvc *EntityService, tenant string) {
	t.Helper()
	if _, _, err := entitySvc.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name:  "accounts",
		Label: "Accounts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity accounts: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name:  "deals",
		Label: "Deals",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "account", Label: "Account", FieldType: "relation", Options: map[string]any{"relatedEntity": "accounts"}},
			{Name: "amount", Label: "Amount", FieldType: "currency", Options: map[string]any{"currencyCode": "USD"}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity deals: %v", err)
	}
}

func addDealTotalRollupField(t *testing.T, fieldSvc *FieldService, tenant string) {
	t.Helper()
	if _, err := fieldSvc.AddField(context.Background(), tenant, AddFieldInput{
		EntityName: "accounts",
		Field: FieldInput{
			Name: "deal_total", Label: "Deal Total", FieldType: "rollup",
			Options: map[string]any{"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "amount"},
		},
	}); err != nil {
		t.Fatalf("AddField deal_total rollup: %v", err)
	}
}

func TestEngComputed_Rollup_SumExactDecimal_NoFloatDrift(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const user = "33333333-3333-3333-3333-333333333301"
	setupRollupEntities(t, entitySvc, engTenantA)
	addDealTotalRollupField(t, fieldSvc, engTenantA)

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	// 0.10 + 0.20 + 0.30 would float-drift to 0.6000000000000001 under naive float64 summation.
	amounts := []float64{0.10, 0.20, 0.30}
	for _, amt := range amounts {
		if _, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "deals", map[string]any{
			"name": "Deal", "account": accID,
			"amount": map[string]any{"amount": amt, "currencyCode": "USD"},
		}); err != nil {
			t.Fatalf("create deal amount=%v: %v", amt, err)
		}
	}

	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "accounts", accID, nil, []string{"deal_total"})
	if err != nil {
		t.Fatalf("Get with ?rollup=deal_total: %v", err)
	}
	m, ok := rec.Data["deal_total"].(map[string]any)
	if !ok {
		t.Fatalf("deal_total type = %T; want map (currency shape)", rec.Data["deal_total"])
	}
	amtStr, ok := m["amount"].(string)
	if !ok {
		t.Fatalf("deal_total.amount type = %T; want string (never float64 — no drift)", m["amount"])
	}
	if amtStr != "0.6000000000" {
		t.Fatalf("deal_total.amount = %q; want exact DECIMAL(38,10) \"0.6000000000\" (0.10+0.20+0.30, no float drift)", amtStr)
	}
	if m["currencyCode"] != "USD" {
		t.Fatalf("deal_total.currencyCode = %v; want USD", m["currencyCode"])
	}
}

func TestEngComputed_Rollup_Sum3Children_600Exact(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const user = "33333333-3333-3333-3333-333333333302"
	setupRollupEntities(t, entitySvc, engTenantA)
	addDealTotalRollupField(t, fieldSvc, engTenantA)

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	for _, amt := range []float64{100.00, 200.00, 300.00} {
		if _, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "deals", map[string]any{
			"name": "Deal", "account": accID,
			"amount": map[string]any{"amount": amt, "currencyCode": "USD"},
		}); err != nil {
			t.Fatalf("create deal amount=%v: %v", amt, err)
		}
	}

	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "accounts", accID, nil, []string{"deal_total"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	m := rec.Data["deal_total"].(map[string]any)
	if m["amount"] != "600.0000000000" {
		t.Fatalf("deal_total.amount = %v; want \"600.0000000000\"", m["amount"])
	}
}

func TestEngComputed_Rollup_Count_ZeroChildren(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const user = "33333333-3333-3333-3333-333333333303"
	setupRollupEntities(t, entitySvc, engTenantA)
	if _, err := fieldSvc.AddField(ctx, engTenantA, AddFieldInput{
		EntityName: "accounts",
		Field: FieldInput{
			Name: "deal_count", Label: "Deal Count", FieldType: "rollup",
			Options: map[string]any{"childEntity": "deals", "viaRelation": "account", "metric": "count"},
		},
	}); err != nil {
		t.Fatalf("AddField deal_count: %v", err)
	}

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{"name": "Empty Co"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "accounts", accID, nil, []string{"deal_count"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// RollupResult.ToJSONValue() returns a Go int64 for count.
	n, ok := rec.Data["deal_count"].(int64)
	if !ok {
		t.Fatalf("deal_count type = %T; want int64(0)", rec.Data["deal_count"])
	}
	if n != 0 {
		t.Fatalf("deal_count = %v; want 0", n)
	}
}

func TestEngComputed_Rollup_SoftDeletedChild_Excluded(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const user = "33333333-3333-3333-3333-333333333304"
	setupRollupEntities(t, entitySvc, engTenantA)
	addDealTotalRollupField(t, fieldSvc, engTenantA)

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	keepID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "deals", map[string]any{
		"name": "Keep", "account": accID, "amount": map[string]any{"amount": 100.00, "currencyCode": "USD"},
	})
	if err != nil {
		t.Fatalf("create deal keep: %v", err)
	}
	dropID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "deals", map[string]any{
		"name": "Drop", "account": accID, "amount": map[string]any{"amount": 900.00, "currencyCode": "USD"},
	})
	if err != nil {
		t.Fatalf("create deal drop: %v", err)
	}
	_ = keepID

	if err := recordSvc.Delete(ctx, engAdmin(user), engTenantA, "deals", dropID); err != nil {
		t.Fatalf("soft-delete deal: %v", err)
	}

	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "accounts", accID, nil, []string{"deal_total"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	m := rec.Data["deal_total"].(map[string]any)
	if m["amount"] != "100.0000000000" {
		t.Fatalf("deal_total.amount = %v; want \"100.0000000000\" (900 deal soft-deleted, must be excluded)", m["amount"])
	}
}

func TestEngComputed_Rollup_TenantIsolation(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const userA = "33333333-3333-3333-3333-333333333305"
	const userB = "44444444-4444-4444-4444-444444444405"
	setupRollupEntities(t, entitySvc, engTenantA)
	setupRollupEntities(t, entitySvc, engTenantB)
	addDealTotalRollupField(t, fieldSvc, engTenantA)
	addDealTotalRollupField(t, fieldSvc, engTenantB)

	accA, err := recordSvc.Create(ctx, engAdmin(userA), engTenantA, userA, "accounts", map[string]any{"name": "A Co"})
	if err != nil {
		t.Fatalf("create A account: %v", err)
	}
	accB, err := recordSvc.Create(ctx, engAdmin(userB), engTenantB, userB, "accounts", map[string]any{"name": "B Co"})
	if err != nil {
		t.Fatalf("create B account: %v", err)
	}
	if _, err := recordSvc.Create(ctx, engAdmin(userA), engTenantA, userA, "deals", map[string]any{
		"name": "A Deal", "account": accA, "amount": map[string]any{"amount": 50.00, "currencyCode": "USD"},
	}); err != nil {
		t.Fatalf("create A deal: %v", err)
	}
	// Tenant B has a much larger deal that must NEVER count toward tenant A's rollup, even
	// though both `deals` rows live in the SAME shared obj_deals table.
	if _, err := recordSvc.Create(ctx, engAdmin(userB), engTenantB, userB, "deals", map[string]any{
		"name": "B Deal", "account": accB, "amount": map[string]any{"amount": 99999.00, "currencyCode": "USD"},
	}); err != nil {
		t.Fatalf("create B deal: %v", err)
	}

	recA, err := recordSvc.Get(ctx, engAdmin(userA), engTenantA, "accounts", accA, nil, []string{"deal_total"})
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	mA := recA.Data["deal_total"].(map[string]any)
	if mA["amount"] != "50.0000000000" {
		t.Fatalf("CROSS-TENANT LEAK: A's deal_total = %v; want \"50.0000000000\" (B's deal must not count)", mA["amount"])
	}
}

// ---------------------------------------------------------------------------
// ?rollup= opt-in — absent vs present.
// ---------------------------------------------------------------------------

func TestEngComputed_Rollup_OptIn_AbsentNotResolved(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const user = "33333333-3333-3333-3333-333333333306"
	setupRollupEntities(t, entitySvc, engTenantA)
	addDealTotalRollupField(t, fieldSvc, engTenantA)

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if _, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "deals", map[string]any{
		"name": "Deal", "account": accID, "amount": map[string]any{"amount": 10.00, "currencyCode": "USD"},
	}); err != nil {
		t.Fatalf("create deal: %v", err)
	}

	// Get WITHOUT ?rollup= — the field must be absent (or nil), never resolved.
	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "accounts", accID, nil, nil)
	if err != nil {
		t.Fatalf("Get without rollup param: %v", err)
	}
	if v, ok := rec.Data["deal_total"]; ok && v != nil {
		t.Fatalf("deal_total resolved WITHOUT ?rollup= opt-in: %v (must stay absent/nil — avoids N+1 by default)", v)
	}
}

func TestEngComputed_Rollup_OptIn_PresentResolved(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const user = "33333333-3333-3333-3333-333333333307"
	setupRollupEntities(t, entitySvc, engTenantA)
	addDealTotalRollupField(t, fieldSvc, engTenantA)

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if _, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "deals", map[string]any{
		"name": "Deal", "account": accID, "amount": map[string]any{"amount": 10.00, "currencyCode": "USD"},
	}); err != nil {
		t.Fatalf("create deal: %v", err)
	}

	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "accounts", accID, nil, []string{"deal_total"})
	if err != nil {
		t.Fatalf("Get with ?rollup=deal_total: %v", err)
	}
	m, ok := rec.Data["deal_total"].(map[string]any)
	if !ok || m["amount"] != "10.0000000000" {
		t.Fatalf("deal_total = %v; want resolved currency map with amount 10.0000000000", rec.Data["deal_total"])
	}
}

// TestEngComputed_Rollup_SumOverCurrencyFormula (#262): the clinic journey's "Tổng tiền hoá đơn =
// cộng Thành tiền các dòng" built exactly as Studio does it — the line's Thành tiền is a
// CURRENCY-resultType formula (qty * price) and the invoice total is a rollup added via AddField
// (POST /admin/fields). Real SQL must sum the formula's persisted ".amount" and return money in
// the formula's currency; before #262 AddField rejected it FIELD_ROLLUP_AGG_FIELD_FORMULA_NOT_NUMBER.
func TestEngComputed_Rollup_SumOverCurrencyFormula(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	const user = "33333333-3333-3333-3333-333333333309"
	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
		Name: "invoices", Label: "Hoá đơn",
		Fields: []FieldInput{{Name: "name", Label: "Name", FieldType: "text", IsRequired: true}},
	}); err != nil {
		t.Fatalf("CreateEntity invoices: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
		Name: "invoice_lines", Label: "Dòng hoá đơn",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "invoice", Label: "Hoá đơn", FieldType: "relation", Options: map[string]any{"relatedEntity": "invoices"}},
			{Name: "qty", Label: "SL", FieldType: "number"},
			{Name: "price", Label: "Đơn giá", FieldType: "currency", Options: map[string]any{"currencyCode": "VND"}},
			{Name: "line_total", Label: "Thành tiền", FieldType: "formula",
				Options: map[string]any{"formula": "qty * price", "resultType": "currency", "currencyCode": "VND"}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity invoice_lines: %v", err)
	}
	if _, err := fieldSvc.AddField(ctx, engTenantA, AddFieldInput{
		EntityName: "invoices",
		Field: FieldInput{Name: "total", Label: "Tổng tiền", FieldType: "rollup",
			Options: map[string]any{"childEntity": "invoice_lines", "viaRelation": "invoice", "metric": "sum", "aggField": "line_total"}},
	}); err != nil {
		t.Fatalf("AddField rollup over currency formula: %v", err)
	}

	invID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "invoices", map[string]any{"name": "HD-001"})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	for _, l := range []struct{ qty, price float64 }{{2, 300000}, {3, 200000}} {
		if _, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "invoice_lines", map[string]any{
			"name": "Dòng", "invoice": invID, "qty": l.qty,
			"price": map[string]any{"amount": l.price, "currencyCode": "VND"},
		}); err != nil {
			t.Fatalf("create line: %v", err)
		}
	}

	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "invoices", invID, nil, []string{"total"})
	if err != nil {
		t.Fatalf("Get with ?rollup=total: %v", err)
	}
	m, ok := rec.Data["total"].(map[string]any)
	if !ok {
		t.Fatalf("total = %v (%T); want currency map", rec.Data["total"], rec.Data["total"])
	}
	if m["amount"] != "1200000.0000000000" || m["currencyCode"] != "VND" {
		t.Fatalf("total = %v; want {amount:\"1200000.0000000000\" currencyCode:VND}", m)
	}
}

// ---------------------------------------------------------------------------
// N-N rollup — pivot id-set via RelationService/ListLinked.
// ---------------------------------------------------------------------------

func TestEngComputed_Rollup_NN_PivotIDSet(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, dynRepo, metaRepo := engWire(t)
	ctx := context.Background()
	const user = "55555555-5555-5555-5555-555555555501"
	setupRollupEntities(t, entitySvc, engTenantA)

	relSvc := NewRelationService(dynRepo, metaRepo)
	if _, err := relSvc.RegisterRelationship(ctx, engTenantA, RegisterRelationshipInput{
		Name: "accounts_deals", EntityA: "accounts", EntityB: "deals",
	}); err != nil {
		t.Fatalf("RegisterRelationship: %v", err)
	}

	if _, err := fieldSvc.AddField(ctx, engTenantA, AddFieldInput{
		EntityName: "accounts",
		Field: FieldInput{
			Name: "deal_total_nn", Label: "Deal Total NN", FieldType: "rollup",
			Options: map[string]any{"childEntity": "deals", "relationship": "accounts_deals", "metric": "sum", "aggField": "amount"},
		},
	}); err != nil {
		t.Fatalf("AddField deal_total_nn: %v", err)
	}

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	var dealIDs []string
	for _, amt := range []float64{10.00, 20.00, 30.00} {
		id, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "deals", map[string]any{
			"name": "Deal", "amount": map[string]any{"amount": amt, "currencyCode": "USD"},
		})
		if err != nil {
			t.Fatalf("create deal amount=%v: %v", amt, err)
		}
		dealIDs = append(dealIDs, id)
	}

	// Link only 2 of the 3 deals via the N-N pivot — the third must NOT count.
	sourceScope := dynrepo.RowScope{Level: dynrepo.AccessAll, IsAdmin: true}
	for _, dealID := range dealIDs[:2] {
		if _, err := relSvc.Link(ctx, engTenantA, user, "admin-role", true,
			"accounts", accID, "accounts_deals", dealID,
			dynRepo, nil, dynRepo, sourceScope); err != nil {
			t.Fatalf("Link deal %s: %v", dealID, err)
		}
	}

	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "accounts", accID, nil, []string{"deal_total_nn"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	m, ok := rec.Data["deal_total_nn"].(map[string]any)
	if !ok {
		t.Fatalf("deal_total_nn type = %T; want map", rec.Data["deal_total_nn"])
	}
	if m["amount"] != "30.0000000000" {
		t.Fatalf("deal_total_nn.amount = %v; want \"30.0000000000\" (10+20, third unlinked deal excluded)", m["amount"])
	}
}

func TestEngComputed_Rollup_NN_Count(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, dynRepo, metaRepo := engWire(t)
	ctx := context.Background()
	const user = "55555555-5555-5555-5555-555555555502"
	setupRollupEntities(t, entitySvc, engTenantA)

	relSvc := NewRelationService(dynRepo, metaRepo)
	if _, err := relSvc.RegisterRelationship(ctx, engTenantA, RegisterRelationshipInput{
		Name: "accounts_deals2", EntityA: "accounts", EntityB: "deals",
	}); err != nil {
		t.Fatalf("RegisterRelationship: %v", err)
	}
	if _, err := fieldSvc.AddField(ctx, engTenantA, AddFieldInput{
		EntityName: "accounts",
		Field: FieldInput{
			Name: "deal_count_nn", Label: "Deal Count NN", FieldType: "rollup",
			Options: map[string]any{"childEntity": "deals", "relationship": "accounts_deals2", "metric": "count"},
		},
	}); err != nil {
		t.Fatalf("AddField deal_count_nn: %v", err)
	}

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	var dealIDs []string
	for i := 0; i < 4; i++ {
		id, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "deals", map[string]any{"name": "Deal"})
		if err != nil {
			t.Fatalf("create deal %d: %v", i, err)
		}
		dealIDs = append(dealIDs, id)
	}
	sourceScope := dynrepo.RowScope{Level: dynrepo.AccessAll, IsAdmin: true}
	for _, dealID := range dealIDs[:3] {
		if _, err := relSvc.Link(ctx, engTenantA, user, "admin-role", true,
			"accounts", accID, "accounts_deals2", dealID,
			dynRepo, nil, dynRepo, sourceScope); err != nil {
			t.Fatalf("Link deal %s: %v", dealID, err)
		}
	}

	rec, err := recordSvc.Get(ctx, engAdmin(user), engTenantA, "accounts", accID, nil, []string{"deal_count_nn"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// RollupResult.ToJSONValue() returns a Go int64 for count; the handler's JSON encoding turns
	// it into a JSON number either way, but in-process (as read here) the concrete type is int64.
	n, ok := rec.Data["deal_count_nn"].(int64)
	if !ok {
		t.Fatalf("deal_count_nn type = %T; want int64(3)", rec.Data["deal_count_nn"])
	}
	if n != 3 {
		t.Fatalf("deal_count_nn = %v; want 3", n)
	}
}
