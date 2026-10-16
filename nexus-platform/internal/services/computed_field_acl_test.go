package services

// ENG-COMPUTED #2/#3 × RM-42/A6 regression suite: field-level ACL MUST gate computed values
// (lookup + rollup) exactly like stored values. These tests drive the PUBLIC Get/List surface
// with a real FieldACLService over an entity-keyed fake loader, because the original hole was
// an ORDERING bug invisible to unit tests of the parts: stripReadFields ran BEFORE
// applyLookups/applyRollups wrote computed values, so a can_view=0 grant on a computed field
// was silently re-added, and a lookup mirrored a source field the role was denied on the
// TARGET entity (bulk PII harvest via List).

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// entityKeyedFieldACLLoader is a fieldACLLoader whose grant rows (and injected load errors) are
// keyed by entity name, so a single test can deny a field — or fail the restriction load — on
// the TARGET entity of a lookup while leaving the base entity healthy. It counts loads per
// entity so tests can assert the per-lookup-field hoist (one restriction load per target entity
// per request, not one per record).
type entityKeyedFieldACLLoader struct {
	rowsByEntity map[string][]auth.FieldACL
	errByEntity  map[string]error
	loads        map[string]int
}

func (f *entityKeyedFieldACLLoader) LoadFieldACLForEntity(_ context.Context, _, _, entity string) ([]auth.FieldACL, error) {
	if f.loads == nil {
		f.loads = make(map[string]int)
	}
	f.loads[entity]++
	if err := f.errByEntity[entity]; err != nil {
		return nil, err
	}
	return f.rowsByEntity[entity], nil
}

// fakeComputedStore returns canned records (Get/List) and a canned aggregate, counting
// AggregateScalar calls so tests can prove a denied rollup is never even resolved.
type fakeComputedStore struct {
	fakeRecordStore
	record   *dynrepo.Record
	records  []dynrepo.Record
	agg      dynrepo.AggregateScalar
	aggCalls int
}

func (f *fakeComputedStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return f.record, nil
}

func (f *fakeComputedStore) ListRecords(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.ListParams) ([]dynrepo.Record, int, error) {
	return f.records, len(f.records), nil
}

func (f *fakeComputedStore) AggregateScalar(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.AggregateParams) (dynrepo.AggregateScalar, error) {
	f.aggCalls++
	return f.agg, nil
}

// dealsComputedFixture wires a "deals" entity carrying one relation (contact → contacts), one
// lookup mirroring contacts.salary through it, and one FK-on-child sum rollup over
// payments.amount — the exact shapes the field-ACL bypass leaked through.
func dealsComputedFixture(t *testing.T) *fakeRecordCatalog {
	t.Helper()
	return &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-deals", TenantID: "tenant-1", Name: "deals", DataTable: "obj_deals"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text"},
			{Name: "contact", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "contacts"})},
			{Name: "contact_salary", FieldType: "lookup", Options: jsonOptsPtr(t, map[string]any{"via": "contact", "sourceField": "salary"})},
			{Name: "payment_total", FieldType: "rollup", Options: jsonOptsPtr(t, map[string]any{"childEntity": "payments", "viaRelation": "deal", "metric": "sum", "aggField": "amount"})},
		},
	}
}

func paymentsRollupMeta() *fakeRollupMeta {
	return &fakeRollupMeta{
		entitiesByName:   map[string]*metadata.EntityDefinition{"payments": {ID: "ent-pay", Name: "payments"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{"ent-pay": {{Name: "amount", FieldType: "number"}}},
	}
}

func computedACLService(t *testing.T, store RecordStore, loader *entityKeyedFieldACLLoader, fetcher FieldValueFetcher) *RecordService {
	t.Helper()
	return NewRecordService(dealsComputedFixture(t), store).
		WithFieldACL(NewFieldACLService(loader)).
		WithFieldValueFetcher(fetcher).
		WithRollupMeta(paymentsRollupMeta())
}

func nonAdminScope() RecordScope {
	return RecordScope{UserID: "u1", AccessLevel: "all", RoleID: "role-1"}
}

func dealRecord(id string) dynrepo.Record {
	return dynrepo.Record{ID: id, Data: map[string]any{"name": "Deal " + id, "contact": "c-1"}}
}

// --- (a) can_view=0 on the computed field itself ------------------------------------------------

// A lookup field the role may not view must be ABSENT from a Get response (exactly like a
// stripped stored field) — and must not even be resolved against the target entity.
func TestGet_LookupFieldHiddenByFieldACL_StrippedAndNotResolved(t *testing.T) {
	loader := &entityKeyedFieldACLLoader{rowsByEntity: map[string][]auth.FieldACL{
		"deals": {{FieldName: "contact_salary", CanView: false}},
	}}
	fetcher := newFakeFieldValueFetcher(map[string]any{"c-1": float64(50000)})
	rec := dealRecord("d1")
	store := &fakeComputedStore{record: &rec}
	svc := computedACLService(t, store, loader, fetcher)

	got, err := svc.Get(context.Background(), nonAdminScope(), "tenant-1", "deals", "d1", nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v, present := got.Data["contact_salary"]; present {
		t.Errorf("contact_salary must be STRIPPED for can_view=0 role; got %v", v)
	}
	if fetcher.calls["contacts"] != 0 {
		t.Errorf("hidden lookup must not be resolved at all; BatchFetchField calls = %d", fetcher.calls["contacts"])
	}
}

// Same strip on the List (bulk) surface — the harvest path.
func TestList_LookupFieldHiddenByFieldACL_StrippedOnEveryRow(t *testing.T) {
	loader := &entityKeyedFieldACLLoader{rowsByEntity: map[string][]auth.FieldACL{
		"deals": {{FieldName: "contact_salary", CanView: false}},
	}}
	fetcher := newFakeFieldValueFetcher(map[string]any{"c-1": float64(50000)})
	store := &fakeComputedStore{records: []dynrepo.Record{dealRecord("d1"), dealRecord("d2")}}
	svc := computedACLService(t, store, loader, fetcher)

	res, err := svc.List(context.Background(), nonAdminScope(), "tenant-1", "deals", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range res.Records {
		if v, present := r.Data["contact_salary"]; present {
			t.Errorf("record %s: contact_salary must be STRIPPED; got %v", r.ID, v)
		}
	}
	if fetcher.calls["contacts"] != 0 {
		t.Errorf("hidden lookup must not be resolved at all; BatchFetchField calls = %d", fetcher.calls["contacts"])
	}
}

// A rollup field the role may not view must be ABSENT even when explicitly requested via
// ?rollup= — and its aggregate must never run.
func TestGet_RollupFieldHiddenByFieldACL_StrippedAndNotResolved(t *testing.T) {
	loader := &entityKeyedFieldACLLoader{rowsByEntity: map[string][]auth.FieldACL{
		"deals": {{FieldName: "payment_total", CanView: false}},
	}}
	sum := "1200.5000000000"
	rec := dealRecord("d1")
	store := &fakeComputedStore{record: &rec, agg: dynrepo.AggregateScalar{Value: &sum, Count: 3}}
	svc := computedACLService(t, store, loader, newFakeFieldValueFetcher(nil))

	got, err := svc.Get(context.Background(), nonAdminScope(), "tenant-1", "deals", "d1", nil, []string{"payment_total"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v, present := got.Data["payment_total"]; present {
		t.Errorf("payment_total must be STRIPPED for can_view=0 role; got %v", v)
	}
	if store.aggCalls != 0 {
		t.Errorf("hidden rollup must not be resolved at all; AggregateScalar calls = %d", store.aggCalls)
	}
}

func TestList_RollupFieldHiddenByFieldACL_StrippedOnEveryRow(t *testing.T) {
	loader := &entityKeyedFieldACLLoader{rowsByEntity: map[string][]auth.FieldACL{
		"deals": {{FieldName: "payment_total", CanView: false}},
	}}
	sum := "1200.5000000000"
	store := &fakeComputedStore{records: []dynrepo.Record{dealRecord("d1"), dealRecord("d2")}, agg: dynrepo.AggregateScalar{Value: &sum, Count: 3}}
	svc := computedACLService(t, store, loader, newFakeFieldValueFetcher(nil))

	res, err := svc.List(context.Background(), nonAdminScope(), "tenant-1", "deals", ListQuery{Page: 1, Limit: 20, Rollup: []string{"payment_total"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range res.Records {
		if v, present := r.Data["payment_total"]; present {
			t.Errorf("record %s: payment_total must be STRIPPED; got %v", r.ID, v)
		}
	}
	if store.aggCalls != 0 {
		t.Errorf("hidden rollup must not be resolved at all; AggregateScalar calls = %d", store.aggCalls)
	}
}

// --- (b) sourceField denied on the lookup's TARGET entity ---------------------------------------

// The lookup field itself is visible, but the mirrored sourceField (contacts.salary) carries
// can_view=0 on the TARGET entity → the lookup resolves to null (present-as-null, like an
// unresolvable FK) and the value is never fetched. Without this gate, a lookup field is a
// side-door around the target entity's field-level ACL.
func TestGet_LookupSourceFieldDeniedOnTargetEntity_ResolvesNull(t *testing.T) {
	loader := &entityKeyedFieldACLLoader{rowsByEntity: map[string][]auth.FieldACL{
		"contacts": {{FieldName: "salary", CanView: false}},
	}}
	fetcher := newFakeFieldValueFetcher(map[string]any{"c-1": float64(50000)})
	rec := dealRecord("d1")
	store := &fakeComputedStore{record: &rec}
	svc := computedACLService(t, store, loader, fetcher)

	got, err := svc.Get(context.Background(), nonAdminScope(), "tenant-1", "deals", "d1", nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	v, present := got.Data["contact_salary"]
	if !present || v != nil {
		t.Errorf("contact_salary = %v (present=%v); want explicit nil (source field denied on target)", v, present)
	}
	if fetcher.calls["contacts"] != 0 {
		t.Errorf("denied source field must never be fetched; BatchFetchField calls = %d", fetcher.calls["contacts"])
	}
}

// List = the bulk-harvest surface: every row resolves to null, the value is never fetched, and
// the target entity's restriction set is loaded ONCE for the whole page (hoisted), not per row.
func TestList_LookupSourceFieldDeniedOnTargetEntity_NullOnEveryRow(t *testing.T) {
	loader := &entityKeyedFieldACLLoader{rowsByEntity: map[string][]auth.FieldACL{
		"contacts": {{FieldName: "salary", CanView: false}},
	}}
	fetcher := newFakeFieldValueFetcher(map[string]any{"c-1": float64(50000)})
	store := &fakeComputedStore{records: []dynrepo.Record{dealRecord("d1"), dealRecord("d2"), dealRecord("d3")}}
	svc := computedACLService(t, store, loader, fetcher)

	res, err := svc.List(context.Background(), nonAdminScope(), "tenant-1", "deals", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range res.Records {
		v, present := r.Data["contact_salary"]
		if !present || v != nil {
			t.Errorf("record %s: contact_salary = %v (present=%v); want explicit nil", r.ID, v, present)
		}
	}
	if fetcher.calls["contacts"] != 0 {
		t.Errorf("denied source field must never be fetched; BatchFetchField calls = %d", fetcher.calls["contacts"])
	}
	if loader.loads["contacts"] != 1 {
		t.Errorf("target-entity restrictions must be loaded ONCE per page (hoisted); loads = %d", loader.loads["contacts"])
	}
}

// --- rollup aggField denied on the CHILD entity --------------------------------------------------

// The rollup field itself is visible, but the aggregated child field (payments.amount) carries
// can_view=0 → the rollup resolves to null (undefined) and the aggregate never runs. Mirrors
// the /aggregate endpoint's AssertFieldVisible rule: a SUM over a hidden column discloses it.
func TestGet_RollupAggFieldDeniedOnChildEntity_ResolvesNull(t *testing.T) {
	loader := &entityKeyedFieldACLLoader{rowsByEntity: map[string][]auth.FieldACL{
		"payments": {{FieldName: "amount", CanView: false}},
	}}
	sum := "1200.5000000000"
	rec := dealRecord("d1")
	store := &fakeComputedStore{record: &rec, agg: dynrepo.AggregateScalar{Value: &sum, Count: 3}}
	svc := computedACLService(t, store, loader, newFakeFieldValueFetcher(nil))

	got, err := svc.Get(context.Background(), nonAdminScope(), "tenant-1", "deals", "d1", nil, []string{"payment_total"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	v, present := got.Data["payment_total"]
	if !present || v != nil {
		t.Errorf("payment_total = %v (present=%v); want explicit nil (aggField denied on child)", v, present)
	}
	if store.aggCalls != 0 {
		t.Errorf("aggregate over a denied child field must never run; AggregateScalar calls = %d", store.aggCalls)
	}
}

// List parity for the aggField gate: every row null, zero aggregates, and the CHILD entity's
// restriction set loaded ONCE for the whole page (hoisted into the per-field plan).
func TestList_RollupAggFieldDeniedOnChildEntity_NullOnEveryRow(t *testing.T) {
	loader := &entityKeyedFieldACLLoader{rowsByEntity: map[string][]auth.FieldACL{
		"payments": {{FieldName: "amount", CanView: false}},
	}}
	sum := "1200.5000000000"
	store := &fakeComputedStore{records: []dynrepo.Record{dealRecord("d1"), dealRecord("d2"), dealRecord("d3")}, agg: dynrepo.AggregateScalar{Value: &sum, Count: 3}}
	svc := computedACLService(t, store, loader, newFakeFieldValueFetcher(nil))

	res, err := svc.List(context.Background(), nonAdminScope(), "tenant-1", "deals", ListQuery{Page: 1, Limit: 20, Rollup: []string{"payment_total"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range res.Records {
		v, present := r.Data["payment_total"]
		if !present || v != nil {
			t.Errorf("record %s: payment_total = %v (present=%v); want explicit nil", r.ID, v, present)
		}
	}
	if store.aggCalls != 0 {
		t.Errorf("aggregate over a denied child field must never run; AggregateScalar calls = %d", store.aggCalls)
	}
	if loader.loads["payments"] != 1 {
		t.Errorf("child-entity restrictions must be loaded ONCE per rollup field (hoisted); loads = %d", loader.loads["payments"])
	}
}

// --- fail-closed: a restriction-load ERROR must refuse the page ----------------------------------

// The TARGET entity's restriction load failing mid-lookup must abort the read (never "assume
// allowed" and serve the mirrored value) — the fail-closed half of the sourceField gate.
func TestGet_LookupTargetACLLoadError_FailsClosed(t *testing.T) {
	boom := errors.New("acl store down")
	loader := &entityKeyedFieldACLLoader{errByEntity: map[string]error{"contacts": boom}}
	fetcher := newFakeFieldValueFetcher(map[string]any{"c-1": float64(50000)})
	rec := dealRecord("d1")
	store := &fakeComputedStore{record: &rec}
	svc := computedACLService(t, store, loader, fetcher)

	_, err := svc.Get(context.Background(), nonAdminScope(), "tenant-1", "deals", "d1", nil, nil)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Get error = %v; want wrapped %v (fail-closed: refuse the page)", err, boom)
	}
	if fetcher.calls["contacts"] != 0 {
		t.Errorf("no value may be fetched while the ACL verdict is unknown; BatchFetchField calls = %d", fetcher.calls["contacts"])
	}
}

func TestList_LookupTargetACLLoadError_FailsClosed(t *testing.T) {
	boom := errors.New("acl store down")
	loader := &entityKeyedFieldACLLoader{errByEntity: map[string]error{"contacts": boom}}
	fetcher := newFakeFieldValueFetcher(map[string]any{"c-1": float64(50000)})
	store := &fakeComputedStore{records: []dynrepo.Record{dealRecord("d1"), dealRecord("d2")}}
	svc := computedACLService(t, store, loader, fetcher)

	_, err := svc.List(context.Background(), nonAdminScope(), "tenant-1", "deals", ListQuery{Page: 1, Limit: 20})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("List error = %v; want wrapped %v (fail-closed: refuse the page)", err, boom)
	}
	if fetcher.calls["contacts"] != 0 {
		t.Errorf("no value may be fetched while the ACL verdict is unknown; BatchFetchField calls = %d", fetcher.calls["contacts"])
	}
}

// The CHILD entity's restriction load failing during rollup preparation must abort the read —
// the fail-closed half of the aggField gate.
func TestGet_RollupChildACLLoadError_FailsClosed(t *testing.T) {
	boom := errors.New("acl store down")
	loader := &entityKeyedFieldACLLoader{errByEntity: map[string]error{"payments": boom}}
	sum := "1200.5000000000"
	rec := dealRecord("d1")
	store := &fakeComputedStore{record: &rec, agg: dynrepo.AggregateScalar{Value: &sum, Count: 3}}
	svc := computedACLService(t, store, loader, newFakeFieldValueFetcher(nil))

	_, err := svc.Get(context.Background(), nonAdminScope(), "tenant-1", "deals", "d1", nil, []string{"payment_total"})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Get error = %v; want wrapped %v (fail-closed: refuse the page)", err, boom)
	}
	if store.aggCalls != 0 {
		t.Errorf("no aggregate may run while the ACL verdict is unknown; AggregateScalar calls = %d", store.aggCalls)
	}
}

// --- admin bypass --------------------------------------------------------------------------------

// An admin sees every computed value even when deny rows exist for the role — matching
// stripReadFields' admin bypass exactly (over-hiding for admins would be its own regression).
func TestGet_AdminSeesComputedFieldsDespiteDenyRows(t *testing.T) {
	loader := &entityKeyedFieldACLLoader{rowsByEntity: map[string][]auth.FieldACL{
		"deals":    {{FieldName: "contact_salary", CanView: false}, {FieldName: "payment_total", CanView: false}},
		"contacts": {{FieldName: "salary", CanView: false}},
		"payments": {{FieldName: "amount", CanView: false}},
	}}
	fetcher := newFakeFieldValueFetcher(map[string]any{"c-1": float64(50000)})
	sum := "1200.5000000000"
	rec := dealRecord("d1")
	store := &fakeComputedStore{record: &rec, agg: dynrepo.AggregateScalar{Value: &sum, Count: 3}}
	svc := computedACLService(t, store, loader, fetcher)

	admin := RecordScope{UserID: "root", AccessLevel: "all", IsAdmin: true, RoleID: "role-admin"}
	got, err := svc.Get(context.Background(), admin, "tenant-1", "deals", "d1", nil, []string{"payment_total"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v := got.Data["contact_salary"]; v != float64(50000) {
		t.Errorf("admin contact_salary = %v; want 50000", v)
	}
	if v := got.Data["payment_total"]; v != sum {
		t.Errorf("admin payment_total = %v; want %q", v, sum)
	}
}

// List parity for the admin bypass — both List branches gate independently of Get, so an
// over-hide regression there would slip past the Get-only test.
func TestList_AdminSeesComputedFieldsDespiteDenyRows(t *testing.T) {
	loader := &entityKeyedFieldACLLoader{rowsByEntity: map[string][]auth.FieldACL{
		"deals":    {{FieldName: "contact_salary", CanView: false}, {FieldName: "payment_total", CanView: false}},
		"contacts": {{FieldName: "salary", CanView: false}},
		"payments": {{FieldName: "amount", CanView: false}},
	}}
	fetcher := newFakeFieldValueFetcher(map[string]any{"c-1": float64(50000)})
	sum := "1200.5000000000"
	store := &fakeComputedStore{records: []dynrepo.Record{dealRecord("d1"), dealRecord("d2")}, agg: dynrepo.AggregateScalar{Value: &sum, Count: 3}}
	svc := computedACLService(t, store, loader, fetcher)

	admin := RecordScope{UserID: "root", AccessLevel: "all", IsAdmin: true, RoleID: "role-admin"}
	res, err := svc.List(context.Background(), admin, "tenant-1", "deals", ListQuery{Page: 1, Limit: 20, Rollup: []string{"payment_total"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range res.Records {
		if v := r.Data["contact_salary"]; v != float64(50000) {
			t.Errorf("record %s: admin contact_salary = %v; want 50000", r.ID, v)
		}
		if v := r.Data["payment_total"]; v != sum {
			t.Errorf("record %s: admin payment_total = %v; want %q", r.ID, v, sum)
		}
	}
}
