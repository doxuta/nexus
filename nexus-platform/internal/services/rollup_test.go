package services

// ENG-COMPUTED #3 (rollup) resolveRollup/applyRollups unit tests — fake RecordStore
// (AggregateScalar), fake rollupMetaResolver, fake RollupLinkedLister. Exact-DECIMAL /
// tenant-isolation / soft-delete / N-N-pivot correctness is SQL-level and covered by the
// integration suite (eng_computed_integration_test.go) instead — these unit tests exercise the
// orchestration logic (metric routing, ACL gating, cap enforcement, ?rollup= opt-in gating)
// against fakes that model AggregateScalar's/ListLinked's documented CONTRACT, not the SQL.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// fakeRollupStore is a minimal RecordStore double whose AggregateScalar records every call
// (filters + scope) and returns a canned AggregateScalar keyed by entity name, so a test can
// assert both the resolved value AND that the correct WHERE-shaping filter was built.
type fakeRollupStore struct {
	fakeRecordStore // embeds the inert stubs for the RecordStore methods rollup doesn't use
	byEntity        map[string]dynrepo.AggregateScalar
	err             error
	calls           []fakeRollupAggCall
}

type fakeRollupAggCall struct {
	entity string
	scope  dynrepo.RowScope
	params dynrepo.AggregateParams
}

func (f *fakeRollupStore) AggregateScalar(_ context.Context, entity, _ string, scope dynrepo.RowScope, p dynrepo.AggregateParams) (dynrepo.AggregateScalar, error) {
	f.calls = append(f.calls, fakeRollupAggCall{entity: entity, scope: scope, params: p})
	if f.err != nil {
		return dynrepo.AggregateScalar{}, f.err
	}
	return f.byEntity[entity], nil
}

// fakeRollupMeta is a minimal rollupMetaResolver double. The call counters let tests assert the
// per-field hoist: applyRollups must resolve child metadata ONCE per rollup field per request,
// never once per record (the production metaRepo is uncached — per-record resolution was a
// metadata N+1).
type fakeRollupMeta struct {
	entitiesByName      map[string]*metadata.EntityDefinition
	fieldsByEntityID    map[string][]metadata.FieldDefinition
	relationshipsByName map[string]*metadata.RelationshipDefinition
	entityErr           error
	entityCalls         int
	fieldsCalls         int
	relCalls            int
}

func (f *fakeRollupMeta) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	f.entityCalls++
	if f.entityErr != nil {
		return nil, f.entityErr
	}
	e, ok := f.entitiesByName[name]
	if !ok {
		return nil, metadata.ErrEntityNotFound
	}
	return e, nil
}

func (f *fakeRollupMeta) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	f.fieldsCalls++
	return f.fieldsByEntityID[entityID], nil
}

func (f *fakeRollupMeta) GetRelationshipByName(_ context.Context, _, name string) (*metadata.RelationshipDefinition, error) {
	f.relCalls++
	rel, ok := f.relationshipsByName[name]
	if !ok {
		return nil, metadata.ErrRelationshipNotFound
	}
	return rel, nil
}

// fakeRollupLinked is a minimal RollupLinkedLister double: pages through a static id slice.
type fakeRollupLinked struct {
	ids   []string
	err   error
	calls int
}

func (f *fakeRollupLinked) ListLinked(_ context.Context, _ string, _, _, _ string, _ dynrepo.RowScope, page, limit int) ([]string, int, error) {
	f.calls++
	if f.err != nil {
		return nil, 0, f.err
	}
	total := len(f.ids)
	start := (page - 1) * limit
	if start >= total {
		return nil, total, nil
	}
	end := start + limit
	if end > total {
		end = total
	}
	return f.ids[start:end], total, nil
}

// newFKRollupSpec builds a FieldSpec for a FK-on-child (1-N) rollup field.
func newFKRollupSpec(name, childEntity, via, metric, aggField string) FieldSpec {
	opts := map[string]any{"childEntity": childEntity, "viaRelation": via, "metric": metric}
	if aggField != "" {
		opts["aggField"] = aggField
	}
	return FieldSpec{Name: name, FieldType: "rollup", Options: opts}
}

// newNNRollupSpec builds a FieldSpec for an N-N rollup field.
func newNNRollupSpec(name, childEntity, relationship, metric, aggField string) FieldSpec {
	opts := map[string]any{"childEntity": childEntity, "relationship": relationship, "metric": metric}
	if aggField != "" {
		opts["aggField"] = aggField
	}
	return FieldSpec{Name: name, FieldType: "rollup", Options: opts}
}

func rollupTestService(store RecordStore, meta rollupMetaResolver, linked RollupLinkedLister) *RecordService {
	cat := &fakeRecordCatalog{}
	svc := NewRecordService(cat, store)
	if meta != nil {
		svc = svc.WithRollupMeta(meta)
	}
	if linked != nil {
		svc = svc.WithRollupLinked(linked)
	}
	return svc
}

// ---------------------------------------------------------------------------
// FK-on-child metric routing.
// ---------------------------------------------------------------------------

func TestResolveRollup_FKOnChild_Sum(t *testing.T) {
	sumVal := "600.0000000000"
	store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{
		"deals": {Value: &sumVal, Count: 3},
	}}
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {{Name: "amount", FieldType: "currency", Options: jsonOptsPtr(t, map[string]any{"currencyCode": "USD"})}},
		},
	}
	svc := rollupTestService(store, meta, nil)
	spec := newFKRollupSpec("deal_total", "deals", "account", "sum", "amount")

	res, err := svc.resolveRollup(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", spec, "acc-1")
	if err != nil {
		t.Fatalf("resolveRollup: %v", err)
	}
	jv := res.ToJSONValue()
	m, ok := jv.(map[string]any)
	if !ok {
		t.Fatalf("ToJSONValue type = %T; want map (currency shape)", jv)
	}
	if m["amount"] != sumVal {
		t.Errorf("amount = %v; want %q (exact DECIMAL string, no float)", m["amount"], sumVal)
	}
	if m["currencyCode"] != "USD" {
		t.Errorf("currencyCode = %v; want USD", m["currencyCode"])
	}

	// Assert the filter shape: `<viaRelation>_id = parentID`.
	if len(store.calls) != 1 {
		t.Fatalf("AggregateScalar calls = %d; want 1", len(store.calls))
	}
	call := store.calls[0]
	if call.entity != "deals" {
		t.Errorf("entity = %q; want deals", call.entity)
	}
	if len(call.params.Filters) != 1 || call.params.Filters[0].Column != "account_id" {
		t.Fatalf("filters = %+v; want [account_id = acc-1]", call.params.Filters)
	}
	if call.params.Filters[0].Values[0] != "acc-1" {
		t.Errorf("filter value = %v; want acc-1", call.params.Filters[0].Values)
	}
}

func TestResolveRollup_FKOnChild_Count_ZeroChildren(t *testing.T) {
	zero := "0"
	store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{
		"deals": {Value: &zero, Count: 0},
	}}
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
	}
	svc := rollupTestService(store, meta, nil)
	spec := newFKRollupSpec("deal_count", "deals", "account", "count", "")

	res, err := svc.resolveRollup(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", spec, "acc-empty")
	if err != nil {
		t.Fatalf("resolveRollup: %v", err)
	}
	jv := res.ToJSONValue()
	n, ok := jv.(int64)
	if !ok || n != 0 {
		t.Fatalf("ToJSONValue = %v (%T); want int64(0)", jv, jv)
	}
}

func TestResolveRollup_FKOnChild_Sum_UndefinedOverZeroRows_IsNil(t *testing.T) {
	store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{
		"deals": {Value: nil, Count: 0}, // AggregateScalar's documented zero-rows contract
	}}
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {{Name: "amount", FieldType: "number"}},
		},
	}
	svc := rollupTestService(store, meta, nil)
	spec := newFKRollupSpec("deal_total", "deals", "account", "sum", "amount")

	res, err := svc.resolveRollup(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", spec, "acc-empty")
	if err != nil {
		t.Fatalf("resolveRollup: %v", err)
	}
	if jv := res.ToJSONValue(); jv != nil {
		t.Errorf("ToJSONValue = %v; want nil (undefined sum over zero rows, non-currency field)", jv)
	}
}

// TestResolveRollup_SumOverFormula (#262): a rollup whose aggField is a `formula` child field.
// A currency-resultType formula persists {amount,currencyCode} like a real currency field, so
// the aggregate must read the ".amount" sub-path and the result must come back in the currency
// shape carrying the FORMULA's currencyCode (default "USD" when omitted — the code
// applyComputedFields writes). A number-resultType formula stays a plain scalar path.
func TestResolveRollup_SumOverFormula(t *testing.T) {
	sumVal := "1200000.0000000000"
	cases := []struct {
		name        string
		nn          bool
		formulaOpts map[string]any
		wantSubPath string
		wantCode    any // nil = not the currency shape
	}{
		{"FK currency formula", false, map[string]any{"formula": "qty * price", "resultType": "currency", "currencyCode": "VND"}, "amount", "VND"},
		{"N-N currency formula", true, map[string]any{"formula": "qty * price", "resultType": "currency", "currencyCode": "VND"}, "amount", "VND"},
		{"FK currency formula without code", false, map[string]any{"formula": "qty * price", "resultType": "currency"}, "amount", "USD"},
		{"FK number formula", false, map[string]any{"formula": "qty * price", "resultType": "number"}, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{
				"lines": {Value: &sumVal, Count: 2},
			}}
			meta := &fakeRollupMeta{
				entitiesByName: map[string]*metadata.EntityDefinition{"lines": {ID: "ent-lines", Name: "lines"}},
				fieldsByEntityID: map[string][]metadata.FieldDefinition{
					"ent-lines": {{Name: "line_total", FieldType: "formula", Options: jsonOptsPtr(t, tc.formulaOpts)}},
				},
				relationshipsByName: map[string]*metadata.RelationshipDefinition{
					"invoices_lines": {Name: "invoices_lines", ParentEntityName: "invoices", ChildEntityName: "lines", RelationshipType: "N-N"},
				},
			}
			var linked RollupLinkedLister
			spec := newFKRollupSpec("total", "lines", "invoice", "sum", "line_total")
			if tc.nn {
				linked = &fakeRollupLinked{ids: []string{"l1", "l2"}}
				spec = newNNRollupSpec("total", "lines", "invoices_lines", "sum", "line_total")
			}
			svc := rollupTestService(store, meta, linked)

			res, err := svc.resolveRollup(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", spec, "inv-1")
			if err != nil {
				t.Fatalf("resolveRollup: %v", err)
			}
			if len(store.calls) != 1 {
				t.Fatalf("AggregateScalar calls = %d; want 1", len(store.calls))
			}
			if got := store.calls[0].params.FieldSubPath; got != tc.wantSubPath {
				t.Errorf("FieldSubPath = %q; want %q", got, tc.wantSubPath)
			}
			jv := res.ToJSONValue()
			if tc.wantCode == nil {
				if s, ok := jv.(string); !ok || s != sumVal {
					t.Fatalf("ToJSONValue = %v (%T); want plain %q", jv, jv, sumVal)
				}
				return
			}
			m, ok := jv.(map[string]any)
			if !ok {
				t.Fatalf("ToJSONValue type = %T; want map (currency shape)", jv)
			}
			if m["amount"] != sumVal || m["currencyCode"] != tc.wantCode {
				t.Errorf("ToJSONValue = %v; want {amount:%q currencyCode:%v}", m, sumVal, tc.wantCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Cross-entity metadata failure modes.
// ---------------------------------------------------------------------------

func TestResolveRollup_ChildEntityGone_Unavailable(t *testing.T) {
	store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{}}
	meta := &fakeRollupMeta{} // no entities registered
	svc := rollupTestService(store, meta, nil)
	spec := newFKRollupSpec("deal_total", "deals", "account", "sum", "amount")

	_, err := svc.resolveRollup(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", spec, "acc-1")
	if codeOf(err) != "RECORD_ROLLUP_UNAVAILABLE" {
		t.Fatalf("code = %q; want RECORD_ROLLUP_UNAVAILABLE; err=%v", codeOf(err), err)
	}
}

func TestResolveRollup_NoRollupMeta_Unavailable(t *testing.T) {
	store := &fakeRollupStore{}
	svc := rollupTestService(store, nil, nil) // rollupMeta never wired
	spec := newFKRollupSpec("deal_total", "deals", "account", "sum", "amount")

	_, err := svc.resolveRollup(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", spec, "acc-1")
	if codeOf(err) != "RECORD_ROLLUP_UNAVAILABLE" {
		t.Fatalf("code = %q; want RECORD_ROLLUP_UNAVAILABLE; err=%v", codeOf(err), err)
	}
}

// ---------------------------------------------------------------------------
// ACL gating: rollupChildScope re-derives the CHILD entity's scope (NN-01 mirror), never
// reusing the parent's scope.
// ---------------------------------------------------------------------------

func TestResolveRollup_DeniedChildEntity_ZeroResult(t *testing.T) {
	store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{
		"deals": {Value: strPtr("999"), Count: 5}, // would be wrong if this leaked through
	}}
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {{Name: "amount", FieldType: "number"}},
		},
	}
	authz := &fakeExpandAuthz{allow: map[string]bool{"deals": false}}
	svc := rollupTestService(store, meta, nil).WithACLAuthorizer(authz)
	spec := newFKRollupSpec("deal_total", "deals", "account", "sum", "amount")

	res, err := svc.resolveRollup(context.Background(), RecordScope{UserID: "u1", RoleID: "role-1"}, "tenant-1", spec, "acc-1")
	if err != nil {
		t.Fatalf("resolveRollup: %v", err)
	}
	if jv := res.ToJSONValue(); jv != nil {
		t.Errorf("denied child entity must yield nil (no leaked aggregate); got %v", jv)
	}
	if len(store.calls) != 0 {
		t.Errorf("AggregateScalar must not run for a denied child entity; calls=%d", len(store.calls))
	}
}

func TestResolveRollup_AdminBypassesChildACL(t *testing.T) {
	sumVal := "100.0000000000"
	store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{
		"deals": {Value: &sumVal, Count: 1},
	}}
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {{Name: "amount", FieldType: "number"}},
		},
	}
	authz := &fakeExpandAuthz{allow: map[string]bool{"deals": false}} // would deny a non-admin
	svc := rollupTestService(store, meta, nil).WithACLAuthorizer(authz)
	spec := newFKRollupSpec("deal_total", "deals", "account", "sum", "amount")

	res, err := svc.resolveRollup(context.Background(), RecordScope{UserID: "admin", IsAdmin: true}, "tenant-1", spec, "acc-1")
	if err != nil {
		t.Fatalf("resolveRollup: %v", err)
	}
	if jv, ok := res.ToJSONValue().(string); !ok || jv != sumVal {
		t.Errorf("admin bypass: ToJSONValue = %v; want %q", res.ToJSONValue(), sumVal)
	}
}

// ---------------------------------------------------------------------------
// N-N rollup (pivot id-set via ListLinked).
// ---------------------------------------------------------------------------

func TestResolveRollup_NN_Count_UsesListLinkedTotal(t *testing.T) {
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		relationshipsByName: map[string]*metadata.RelationshipDefinition{
			"accounts_deals": {Name: "accounts_deals", ParentEntityName: "accounts", ChildEntityName: "deals", RelationshipType: "N-N"},
		},
	}
	linked := &fakeRollupLinked{ids: []string{"d1", "d2", "d3"}}
	store := &fakeRollupStore{}
	svc := rollupTestService(store, meta, linked)
	spec := newNNRollupSpec("deal_count", "deals", "accounts_deals", "count", "")

	res, err := svc.resolveRollup(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", spec, "acc-1")
	if err != nil {
		t.Fatalf("resolveRollup: %v", err)
	}
	if jv, ok := res.ToJSONValue().(int64); !ok || jv != 3 {
		t.Fatalf("ToJSONValue = %v (%T); want int64(3)", res.ToJSONValue(), res.ToJSONValue())
	}
	// count must never assemble the id-set (single ListLinked(1,1) call), and never touch
	// AggregateScalar.
	if linked.calls != 1 {
		t.Errorf("ListLinked calls = %d; want 1 (count uses total directly)", linked.calls)
	}
	if len(store.calls) != 0 {
		t.Errorf("AggregateScalar must not run for count; calls=%d", len(store.calls))
	}
}

func TestResolveRollup_NN_Sum_AssemblesIDSetThenOneAggregate(t *testing.T) {
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {{Name: "amount", FieldType: "number"}},
		},
		relationshipsByName: map[string]*metadata.RelationshipDefinition{
			"accounts_deals": {Name: "accounts_deals", ParentEntityName: "accounts", ChildEntityName: "deals", RelationshipType: "N-N"},
		},
	}
	linked := &fakeRollupLinked{ids: []string{"d1", "d2", "d3"}}
	sumVal := "600.0000000000"
	store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{
		"deals": {Value: &sumVal, Count: 3},
	}}
	svc := rollupTestService(store, meta, linked)
	spec := newNNRollupSpec("deal_total", "deals", "accounts_deals", "sum", "amount")

	res, err := svc.resolveRollup(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", spec, "acc-1")
	if err != nil {
		t.Fatalf("resolveRollup: %v", err)
	}
	if jv, ok := res.ToJSONValue().(string); !ok || jv != sumVal {
		t.Fatalf("ToJSONValue = %v; want %q", res.ToJSONValue(), sumVal)
	}
	if len(store.calls) != 1 {
		t.Fatalf("AggregateScalar calls = %d; want exactly 1 (id-set assembled, one aggregate)", len(store.calls))
	}
	call := store.calls[0]
	if len(call.params.Filters) != 1 || call.params.Filters[0].Column != "id" || call.params.Filters[0].Operator != dynrepo.FilterIn {
		t.Fatalf("filters = %+v; want [id IN (d1,d2,d3)]", call.params.Filters)
	}
	// PhysicalColumn MUST be "id" too — `id` is a physical system column, not a JSON `data`
	// key; without PhysicalColumn set, buildPredicates falls back to
	// JSON_EXTRACT(data,'$.id'), which never matches any row (a real-SQL-only bug the fake
	// store here cannot itself detect — see eng_computed_integration_test.go's
	// TestEngComputed_Rollup_NN_PivotIDSet for the end-to-end proof).
	if call.params.Filters[0].PhysicalColumn != "id" {
		t.Fatalf("PhysicalColumn = %q; want \"id\" (JSON_EXTRACT fallback never matches a real row)", call.params.Filters[0].PhysicalColumn)
	}
	gotIDs := call.params.Filters[0].Values
	if len(gotIDs) != 3 {
		t.Fatalf("IN-list len = %d; want 3", len(gotIDs))
	}
}

func TestResolveRollup_NN_Sum_ZeroLinkedChildren_NoAggregateCall(t *testing.T) {
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {{Name: "amount", FieldType: "number"}},
		},
		relationshipsByName: map[string]*metadata.RelationshipDefinition{
			"accounts_deals": {Name: "accounts_deals", ParentEntityName: "accounts", ChildEntityName: "deals", RelationshipType: "N-N"},
		},
	}
	linked := &fakeRollupLinked{ids: nil}
	store := &fakeRollupStore{}
	svc := rollupTestService(store, meta, linked)
	spec := newNNRollupSpec("deal_total", "deals", "accounts_deals", "sum", "amount")

	res, err := svc.resolveRollup(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", spec, "acc-empty")
	if err != nil {
		t.Fatalf("resolveRollup: %v", err)
	}
	if jv := res.ToJSONValue(); jv != nil {
		t.Errorf("zero linked children must yield nil sum; got %v", jv)
	}
	if len(store.calls) != 0 {
		t.Errorf("AggregateScalar must not run with an empty id-set (avoids an invalid empty IN); calls=%d", len(store.calls))
	}
}

// TestResolveRollup_NN_TooManyChildren_Errors: exceeding maxRollupChildren errors (never
// silently truncates-and-returns a wrong "total").
func TestResolveRollup_NN_TooManyChildren_Errors(t *testing.T) {
	ids := make([]string, maxRollupChildren+1)
	for i := range ids {
		ids[i] = "d" + string(rune('a'+i%26)) + string(rune('0'+i%10))
	}
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {{Name: "amount", FieldType: "number"}},
		},
		relationshipsByName: map[string]*metadata.RelationshipDefinition{
			"accounts_deals": {Name: "accounts_deals", ParentEntityName: "accounts", ChildEntityName: "deals", RelationshipType: "N-N"},
		},
	}
	linked := &fakeRollupLinked{ids: ids}
	store := &fakeRollupStore{}
	svc := rollupTestService(store, meta, linked)
	spec := newNNRollupSpec("deal_total", "deals", "accounts_deals", "sum", "amount")

	_, err := svc.resolveRollup(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", spec, "acc-big")
	if codeOf(err) != "RECORD_ROLLUP_TOO_MANY_CHILDREN" {
		t.Fatalf("code = %q; want RECORD_ROLLUP_TOO_MANY_CHILDREN; err=%v", codeOf(err), err)
	}
	if len(store.calls) != 0 {
		t.Errorf("AggregateScalar must NEVER run on a truncated id-set; calls=%d", len(store.calls))
	}
}

// TestResolveRollup_NN_RelationshipIsOneToN_Unavailable (ENG-REL2 #4 defense-in-depth):
// GetRelationshipByName was broadened (migration 000028) to also resolve 1-N registry rows.
// Config-time validation (validateRollupCrossEntity) already rejects this at field-create
// time, so resolveRollup should never see it in practice — but if it ever does (e.g. a
// relationship's type changed after the field was configured), it must refuse with
// RECORD_ROLLUP_UNAVAILABLE (mirroring the not-found message) rather than run N-N pivot logic
// against a relation with no join table.
func TestResolveRollup_NN_RelationshipIsOneToN_Unavailable(t *testing.T) {
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		relationshipsByName: map[string]*metadata.RelationshipDefinition{
			"accounts_deals": {Name: "accounts_deals", ParentEntityName: "accounts", ChildEntityName: "deals", RelationshipType: "1-N"},
		},
	}
	linked := &fakeRollupLinked{ids: []string{"d1"}}
	store := &fakeRollupStore{}
	svc := rollupTestService(store, meta, linked)
	spec := newNNRollupSpec("deal_count", "deals", "accounts_deals", "count", "")

	_, err := svc.resolveRollup(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", spec, "acc-1")
	if codeOf(err) != "RECORD_ROLLUP_UNAVAILABLE" {
		t.Fatalf("code = %q; want RECORD_ROLLUP_UNAVAILABLE", codeOf(err))
	}
	if linked.calls != 0 {
		t.Errorf("resolveRollup must never call ListLinked for a 1-N relationship name; calls=%d", linked.calls)
	}
}

// ---------------------------------------------------------------------------
// applyRollups — the ?rollup= opt-in gate (avoids N+1 by default).
// ---------------------------------------------------------------------------

func TestApplyRollups_OptIn_AbsentParam_NotResolved(t *testing.T) {
	store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{
		"deals": {Value: strPtr("100"), Count: 1},
	}}
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {{Name: "amount", FieldType: "number"}},
		},
	}
	svc := rollupTestService(store, meta, nil)
	specs := []FieldSpec{newFKRollupSpec("deal_total", "deals", "account", "sum", "amount")}
	records := []dynrepo.Record{{ID: "acc-1", Data: map[string]any{}}}

	// rollupFields is nil/empty — the caller never asked for ?rollup=.
	if err := svc.applyRollups(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", records, specs, nil, nil, nil); err != nil {
		t.Fatalf("applyRollups: %v", err)
	}
	if _, ok := records[0].Data["deal_total"]; ok {
		t.Errorf("absent ?rollup= must NOT resolve the field; data=%v", records[0].Data)
	}
	if len(store.calls) != 0 {
		t.Errorf("absent ?rollup= must issue ZERO aggregate queries (no N+1 by default); calls=%d", len(store.calls))
	}
}

func TestApplyRollups_OptIn_PresentParam_Resolved(t *testing.T) {
	sumVal := "100.0000000000"
	store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{
		"deals": {Value: &sumVal, Count: 1},
	}}
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {{Name: "amount", FieldType: "number"}},
		},
	}
	svc := rollupTestService(store, meta, nil)
	specs := []FieldSpec{newFKRollupSpec("deal_total", "deals", "account", "sum", "amount")}
	records := []dynrepo.Record{{ID: "acc-1", Data: map[string]any{}}}

	if err := svc.applyRollups(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", records, specs, []string{"deal_total"}, nil, nil); err != nil {
		t.Fatalf("applyRollups: %v", err)
	}
	if got := records[0].Data["deal_total"]; got != sumVal {
		t.Errorf("deal_total = %v; want %q", got, sumVal)
	}
	if len(store.calls) != 1 {
		t.Errorf("AggregateScalar calls = %d; want 1", len(store.calls))
	}
}

func TestApplyRollups_UnknownRollupField_SkippedForwardCompat(t *testing.T) {
	store := &fakeRollupStore{}
	svc := rollupTestService(store, &fakeRollupMeta{}, nil)
	specs := []FieldSpec{{Name: "title", FieldType: "text"}}
	records := []dynrepo.Record{{ID: "r1", Data: map[string]any{}}}

	if err := svc.applyRollups(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", records, specs, []string{"nonexistent"}, nil, nil); err != nil {
		t.Fatalf("applyRollups: %v", err)
	}
	if len(store.calls) != 0 {
		t.Errorf("unknown rollup field must not trigger a query; calls=%d", len(store.calls))
	}
}

func TestApplyRollups_PropagatesError(t *testing.T) {
	boom := errors.New("boom")
	store := &fakeRollupStore{err: boom}
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {{Name: "amount", FieldType: "number"}},
		},
	}
	svc := rollupTestService(store, meta, nil)
	specs := []FieldSpec{newFKRollupSpec("deal_total", "deals", "account", "sum", "amount")}
	records := []dynrepo.Record{{ID: "acc-1", Data: map[string]any{}}}

	err := svc.applyRollups(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", records, specs, []string{"deal_total"}, nil, nil)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("applyRollups error = %v; want wrapped %v", err, boom)
	}
}

// ---------------------------------------------------------------------------
// Metadata hoisting (N+1 guard): the production rollupMeta is the UNCACHED
// *metadata.Repository, so per-record re-resolution multiplies real queries.
// ---------------------------------------------------------------------------

// applyRollups must prepare each rollup field's child metadata ONCE per request and reuse the
// plan across every record — the per-record cost is the aggregate query only.
func TestApplyRollups_MetadataHoistedOncePerField(t *testing.T) {
	sumVal := "100.0000000000"
	store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{"deals": {Value: &sumVal, Count: 2}}}
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"deals": {ID: "ent-deals", Name: "deals"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{
			"ent-deals": {{Name: "amount", FieldType: "number"}},
		},
	}
	svc := rollupTestService(store, meta, nil)
	specs := []FieldSpec{newFKRollupSpec("deal_total", "deals", "account", "sum", "amount")}
	records := []dynrepo.Record{
		{ID: "p1", Data: map[string]any{}},
		{ID: "p2", Data: map[string]any{}},
		{ID: "p3", Data: map[string]any{}},
	}

	if err := svc.applyRollups(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", records, specs, []string{"deal_total"}, nil, nil); err != nil {
		t.Fatalf("applyRollups: %v", err)
	}
	if meta.entityCalls != 1 || meta.fieldsCalls != 1 {
		t.Errorf("child metadata must be resolved ONCE per rollup field (hoisted); GetEntityByName=%d ListFields=%d; want 1/1",
			meta.entityCalls, meta.fieldsCalls)
	}
	// The aggregate itself is inherently per-record (documented applyRollups contract).
	if len(store.calls) != 3 {
		t.Errorf("AggregateScalar calls = %d; want 3 (one per record)", len(store.calls))
	}
}

// Same hoist for the N-N mode: the relationship definition is resolved once per field, not once
// per record.
func TestApplyRollups_NNRelationshipHoistedOncePerField(t *testing.T) {
	sumVal := "50.0000000000"
	store := &fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{"contacts": {Value: &sumVal, Count: 1}}}
	meta := &fakeRollupMeta{
		entitiesByName:   map[string]*metadata.EntityDefinition{"contacts": {ID: "ent-c", Name: "contacts"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{"ent-c": {{Name: "score", FieldType: "number"}}},
		relationshipsByName: map[string]*metadata.RelationshipDefinition{
			"deals_contacts": {Name: "deals_contacts", ParentEntityName: "deals", ChildEntityName: "contacts", RelationshipType: "N-N"},
		},
	}
	linked := &fakeRollupLinked{ids: []string{"c1", "c2"}}
	svc := rollupTestService(store, meta, linked)
	specs := []FieldSpec{newNNRollupSpec("contact_score_sum", "contacts", "deals_contacts", "sum", "score")}
	records := []dynrepo.Record{{ID: "p1", Data: map[string]any{}}, {ID: "p2", Data: map[string]any{}}}

	if err := svc.applyRollups(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", records, specs, []string{"contact_score_sum"}, nil, nil); err != nil {
		t.Fatalf("applyRollups: %v", err)
	}
	if meta.relCalls != 1 {
		t.Errorf("N-N relationship must be resolved ONCE per rollup field (hoisted); GetRelationshipByName=%d; want 1", meta.relCalls)
	}
	if meta.entityCalls != 1 || meta.fieldsCalls != 1 {
		t.Errorf("child metadata must be resolved ONCE per rollup field (hoisted); GetEntityByName=%d ListFields=%d; want 1/1",
			meta.entityCalls, meta.fieldsCalls)
	}
}
