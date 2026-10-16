package services

// ENG-COMPUTED #2 (lookup) applyLookups unit tests — mirrors rm07_expand_test.go /
// wave5_a2_expand_acl_test.go's style exactly (fake FieldValueFetcher, fake entityAuthorizer),
// since applyLookups is the read-time-resolution twin of applyExpand.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// fakeFieldValueFetcher is a test double for FieldValueFetcher: records call counts per
// (targetEntity) and returns a preset id->value map, so tests can assert batching (no N+1)
// without a database.
type fakeFieldValueFetcher struct {
	calls  map[string]int
	values map[string]any // id -> value (native Go type, mirrors BatchFetchField's contract)
	err    error
}

func newFakeFieldValueFetcher(values map[string]any) *fakeFieldValueFetcher {
	return &fakeFieldValueFetcher{calls: make(map[string]int), values: values}
}

func (f *fakeFieldValueFetcher) BatchFetchField(_ context.Context, targetEntity, _, _ string, ids []string) ([]dynrepo.FieldValueResult, error) {
	f.calls[targetEntity]++
	if f.err != nil {
		return nil, f.err
	}
	results := make([]dynrepo.FieldValueResult, 0, len(ids))
	for _, id := range ids {
		if v, ok := f.values[id]; ok {
			results = append(results, dynrepo.FieldValueResult{ID: id, Value: v})
		}
	}
	return results, nil
}

// makeLookupSpec builds a FieldSpec for a lookup field mirroring sourceField from the related
// record reached via the sibling relation field `via`.
func makeLookupSpec(name, via, sourceField string) FieldSpec {
	return FieldSpec{Name: name, FieldType: "lookup", Options: map[string]any{"via": via, "sourceField": sourceField}}
}

// TestApplyLookups_HappyPath: a lookup field mirrors the target's sourceField value for each
// record whose via-FK resolves.
func TestApplyLookups_HappyPath(t *testing.T) {
	fetcher := newFakeFieldValueFetcher(map[string]any{
		"acc-1": "Manufacturing",
		"acc-2": "Retail",
	})
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
		makeLookupSpec("account_industry", "account", "industry"),
	}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"account": "acc-1"}},
		{ID: "r2", Data: map[string]any{"account": "acc-2"}},
	}

	if err := applyLookups(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, nil, nil); err != nil {
		t.Fatalf("applyLookups error: %v", err)
	}

	if got := records[0].Data["account_industry"]; got != "Manufacturing" {
		t.Errorf("r1 account_industry = %v; want Manufacturing", got)
	}
	if got := records[1].Data["account_industry"]; got != "Retail" {
		t.Errorf("r2 account_industry = %v; want Retail", got)
	}
}

// TestApplyLookups_NullViaFK_ResolvesNull: a record whose via-FK is absent/empty gets a null
// lookup value and the fetcher is never even asked to resolve it (no wasted lookup for an id
// that can never match — proven by an empty ids set, not just a null result).
func TestApplyLookups_NullViaFK_ResolvesNull(t *testing.T) {
	fetcher := newFakeFieldValueFetcher(map[string]any{"acc-1": "Manufacturing"})
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
		makeLookupSpec("account_industry", "account", "industry"),
	}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{}},               // via key absent entirely
		{ID: "r2", Data: map[string]any{"account": ""}},  // via present but empty
		{ID: "r3", Data: map[string]any{"account": nil}}, // via explicit null
	}

	if err := applyLookups(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, nil, nil); err != nil {
		t.Fatalf("applyLookups error: %v", err)
	}
	for _, rec := range records {
		if v, ok := rec.Data["account_industry"]; !ok || v != nil {
			t.Errorf("record %s: account_industry = %v (ok=%v); want explicit nil", rec.ID, v, ok)
		}
	}
	// No via-FK ever resolved to a non-empty id, so BatchFetchField must never have been called.
	if fetcher.calls["accounts"] != 0 {
		t.Errorf("fetcher must not run when every via-FK is null/empty; calls=%d", fetcher.calls["accounts"])
	}
}

// TestApplyLookups_SoftDeletedSource_ResolvesNull: BatchFetchField's contract (deleted_at IS
// NULL in the SQL) omits a soft-deleted source row from its result set — applyLookups must
// resolve that id to null, never error, never a stale/leaked value.
func TestApplyLookups_SoftDeletedSource_ResolvesNull(t *testing.T) {
	// "acc-deleted" is intentionally ABSENT from fetcher.values — mirrors what
	// BatchFetchField actually returns for a soft-deleted/cross-tenant/missing row: omitted,
	// not zero-valued.
	fetcher := newFakeFieldValueFetcher(map[string]any{"acc-1": "Manufacturing"})
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
		makeLookupSpec("account_industry", "account", "industry"),
	}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"account": "acc-deleted"}},
	}

	if err := applyLookups(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, nil, nil); err != nil {
		t.Fatalf("applyLookups error: %v", err)
	}
	if v, ok := records[0].Data["account_industry"]; !ok || v != nil {
		t.Errorf("account_industry = %v (ok=%v); want nil (soft-deleted source)", v, ok)
	}
}

// TestApplyLookups_BatchedOneQueryPerTargetEntity: N records sharing one target entity resolve
// via exactly ONE BatchFetchField call (no N+1) — mirrors TestRM07_NoBatchPerRecord.
func TestApplyLookups_BatchedOneQueryPerTargetEntity(t *testing.T) {
	fetcher := newFakeFieldValueFetcher(map[string]any{
		"acc-1": "A", "acc-2": "B", "acc-3": "C",
	})
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
		makeLookupSpec("account_industry", "account", "industry"),
	}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"account": "acc-1"}},
		{ID: "r2", Data: map[string]any{"account": "acc-2"}},
		{ID: "r3", Data: map[string]any{"account": "acc-3"}},
	}

	if err := applyLookups(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, nil, nil); err != nil {
		t.Fatalf("applyLookups error: %v", err)
	}
	if fetcher.calls["accounts"] != 1 {
		t.Errorf("expected exactly 1 BatchFetchField call for 'accounts', got %d", fetcher.calls["accounts"])
	}
}

// TestApplyLookups_PreservesNativeType: a lookup mirroring a non-string field (currency object,
// number) must preserve the native Go type, not stringify it — the key difference from
// applyExpand's BatchFetchLabels (which always stringifies via JSON_UNQUOTE).
func TestApplyLookups_PreservesNativeType(t *testing.T) {
	currencyVal := map[string]any{"amount": "1234.50", "currencyCode": "USD"}
	fetcher := newFakeFieldValueFetcher(map[string]any{
		"acc-1": currencyVal,
		"acc-2": float64(42),
	})
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
		makeLookupSpec("account_arr", "account", "annual_revenue"),
	}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"account": "acc-1"}},
		{ID: "r2", Data: map[string]any{"account": "acc-2"}},
	}

	if err := applyLookups(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, nil, nil); err != nil {
		t.Fatalf("applyLookups error: %v", err)
	}
	gotMap, ok := records[0].Data["account_arr"].(map[string]any)
	if !ok {
		t.Fatalf("r1 account_arr type = %T; want map[string]any (currency object preserved)", records[0].Data["account_arr"])
	}
	if gotMap["amount"] != "1234.50" {
		t.Errorf("r1 currency amount = %v; want \"1234.50\"", gotMap["amount"])
	}
	gotNum, ok := records[1].Data["account_arr"].(float64)
	if !ok || gotNum != 42 {
		t.Errorf("r2 account_arr = %v (%T); want float64(42)", records[1].Data["account_arr"], records[1].Data["account_arr"])
	}
}

// TestApplyLookups_FetcherError_Propagates: a repository error from BatchFetchField propagates
// (wrapped) instead of being swallowed.
func TestApplyLookups_FetcherError_Propagates(t *testing.T) {
	boom := errors.New("boom")
	fetcher := &fakeFieldValueFetcher{calls: map[string]int{}, err: boom}
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
		makeLookupSpec("account_industry", "account", "industry"),
	}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}

	err := applyLookups(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, nil, nil)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("applyLookups error = %v; want wrapped %v", err, boom)
	}
}

// TestApplyLookups_NilFetcher_NoOp: a nil FieldValueFetcher (lookup disabled) leaves records
// untouched instead of panicking.
func TestApplyLookups_NilFetcher_NoOp(t *testing.T) {
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
		makeLookupSpec("account_industry", "account", "industry"),
	}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}

	if err := applyLookups(context.Background(), nil, nil, nil, RecordScope{}, "tenant-1", records, specs, nil, nil); err != nil {
		t.Fatalf("applyLookups with nil fetcher errored: %v", err)
	}
	if _, ok := records[0].Data["account_industry"]; ok {
		t.Errorf("nil fetcher must leave the lookup field unset; data=%v", records[0].Data)
	}
}

// ---------------------------------------------------------------------------
// ACL gate on the SOURCE entity (mirrors W5-A2 for expand).
// ---------------------------------------------------------------------------

func TestApplyLookups_DeniedSourceEntity_LeavesNull(t *testing.T) {
	fetcher := newFakeFieldValueFetcher(map[string]any{"acc-1": "Manufacturing"})
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
		makeLookupSpec("account_industry", "account", "industry"),
	}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}
	authz := &fakeExpandAuthz{allow: map[string]bool{"accounts": false}}
	scope := RecordScope{UserID: "u1", RoleID: "role-1", IsAdmin: false}

	if err := applyLookups(context.Background(), fetcher, authz, nil, scope, "tenant-1", records, specs, nil, nil); err != nil {
		t.Fatalf("applyLookups: %v", err)
	}
	if _, ok := records[0].Data["account_industry"]; ok {
		t.Errorf("denied source entity must leave the lookup field unset (no leak); data=%v", records[0].Data)
	}
	if fetcher.calls["accounts"] != 0 {
		t.Errorf("fetcher must not run for a denied source entity; calls=%d", fetcher.calls["accounts"])
	}
}

func TestApplyLookups_AllowedSourceEntity_Resolves(t *testing.T) {
	fetcher := newFakeFieldValueFetcher(map[string]any{"acc-1": "Manufacturing"})
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
		makeLookupSpec("account_industry", "account", "industry"),
	}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}
	authz := &fakeExpandAuthz{allow: map[string]bool{"accounts": true}}
	scope := RecordScope{UserID: "u1", RoleID: "role-1"}

	if err := applyLookups(context.Background(), fetcher, authz, nil, scope, "tenant-1", records, specs, nil, nil); err != nil {
		t.Fatalf("applyLookups: %v", err)
	}
	if got := records[0].Data["account_industry"]; got != "Manufacturing" {
		t.Errorf("account_industry = %v; want Manufacturing", got)
	}
}

func TestApplyLookups_AdminBypassesAuthz(t *testing.T) {
	fetcher := newFakeFieldValueFetcher(map[string]any{"acc-1": "Manufacturing"})
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
		makeLookupSpec("account_industry", "account", "industry"),
	}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}
	authz := &fakeExpandAuthz{allow: map[string]bool{"accounts": false}} // would deny a non-admin
	scope := RecordScope{UserID: "admin", IsAdmin: true}

	if err := applyLookups(context.Background(), fetcher, authz, nil, scope, "tenant-1", records, specs, nil, nil); err != nil {
		t.Fatalf("applyLookups: %v", err)
	}
	if got := records[0].Data["account_industry"]; got != "Manufacturing" {
		t.Errorf("admin must bypass authz; account_industry = %v", got)
	}
	if authz.calls != 0 {
		t.Errorf("admin path must not consult authz; calls=%d", authz.calls)
	}
}

// TestApplyLookups_UnknownViaSibling_SkippedDefensively: a lookup whose `via` no longer names
// a sibling relation field (e.g. the relation was deleted after the lookup was configured) is
// skipped defensively — no panic, no error, field simply unset.
func TestApplyLookups_UnknownViaSibling_SkippedDefensively(t *testing.T) {
	fetcher := newFakeFieldValueFetcher(nil)
	specs := []FieldSpec{
		makeLookupSpec("account_industry", "ghost_via", "industry"),
	}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{}}}

	if err := applyLookups(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, nil, nil); err != nil {
		t.Fatalf("applyLookups: %v", err)
	}
	if _, ok := records[0].Data["account_industry"]; ok {
		t.Errorf("unknown via sibling must leave the field unset; data=%v", records[0].Data)
	}
}

// TestApplyLookups_OrderingBeforeExpand: applyLookups reads the RAW via-FK id string; if it ran
// AFTER applyExpand mutated record.Data[via] into an expandedValue struct, the type assertion
// `val.(string)` would fail silently and every lookup would resolve to null. This test proves
// the documented ordering contract holds by running applyLookups BEFORE applyExpand (as every
// production caller does) and asserting the lookup still resolves.
func TestApplyLookups_OrderingBeforeExpand(t *testing.T) {
	lookupFetcher := newFakeFieldValueFetcher(map[string]any{"acc-1": "Manufacturing"})
	labelFetcher := newFakeFetcher(map[string]string{"acc-1": "Acme Corp"})
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
		makeLookupSpec("account_industry", "account", "industry"),
	}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{"account": "acc-1"}}}

	// applyLookups FIRST (production ordering).
	if err := applyLookups(context.Background(), lookupFetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, nil, nil); err != nil {
		t.Fatalf("applyLookups: %v", err)
	}
	// THEN applyExpand mutates record.Data["account"] into an expandedValue.
	if err := applyExpand(context.Background(), labelFetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, []string{"account"}); err != nil {
		t.Fatalf("applyExpand: %v", err)
	}

	if got := records[0].Data["account_industry"]; got != "Manufacturing" {
		t.Errorf("account_industry = %v; want Manufacturing (lookup must resolve before expand mutates the via field)", got)
	}
	if _, ok := records[0].Data["account"].(expandedValue); !ok {
		t.Errorf("account field should be expanded after applyExpand runs; got %#v", records[0].Data["account"])
	}
}
