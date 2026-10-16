package services

// specs/083-rollup-stored-value unit tests: config validation, the stored shape, filter/aggregate
// typing, the read path skipping a stored roll-up, and the refresh orchestration against fakes.
// The SQL-level proof (real children, real totals, tenant isolation) is the integration suite.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

func sPtr(s string) *string { return &s }

func TestRollupResult_StoredValue(t *testing.T) {
	five, zero := int64(5), int64(0)
	cases := []struct {
		name string
		res  RollupResult
		want any
	}{
		{"count", RollupResult{Metric: "count", IntValue: &five}, float64(5)},
		{"count nil is zero", RollupResult{Metric: "count"}, float64(0)},
		{"count zero", RollupResult{Metric: "count", IntValue: &zero}, float64(0)},
		{"sum number", RollupResult{Metric: "sum", DecimalValue: sPtr("600.5000000000")}, 600.5},
		{"sum no rows", RollupResult{Metric: "sum"}, nil},
		{"sum currency", RollupResult{Metric: "sum", DecimalValue: sPtr("1200000"), IsCurrency: true, CurrencyCode: "VND"},
			map[string]any{"amount": float64(1200000), "currencyCode": "VND"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.res.storedValue()
			if err != nil {
				t.Fatalf("storedValue: %v", err)
			}
			if !storedEqual(got, tc.want) {
				t.Fatalf("storedValue = %#v; want %#v", got, tc.want)
			}
		})
	}
	if _, err := (RollupResult{Metric: "sum", DecimalValue: sPtr("abc")}).storedValue(); err == nil {
		t.Fatal("a non-numeric decimal must be an error, not a silent 0")
	}
}

func TestValidateRollupConfig_Stored(t *testing.T) {
	base := func(extra map[string]any) map[string]any {
		o := map[string]any{"childEntity": "enrollments", "metric": "count"}
		for k, v := range extra {
			o[k] = v
		}
		return o
	}
	cases := []struct {
		name string
		opts map[string]any
		code string // "" = ok
	}{
		{"stored 1-N ok", base(map[string]any{"viaRelation": "class", "stored": true}), ""},
		{"stored false N-N ok", base(map[string]any{"relationship": "class_students", "stored": false}), ""},
		{"stored not a bool", base(map[string]any{"viaRelation": "class", "stored": "yes"}), "FIELD_ROLLUP_STORED_INVALID"},
		{"stored N-N refused", base(map[string]any{"relationship": "class_students", "stored": true}), "FIELD_ROLLUP_STORED_NN_UNSUPPORTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRollupConfig("si_so", tc.opts)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
		})
	}
}

func TestValidateRollupCrossEntity_StampsStoredShape(t *testing.T) {
	meta := &fakeRollupMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{"invoices": {ID: "ent-inv", Name: "invoices"}},
		fieldsByEntityID: map[string][]metadata.FieldDefinition{"ent-inv": {
			{Name: "customer", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "customers"})},
			{Name: "total", FieldType: "currency", Options: jsonOptsPtr(t, map[string]any{"currencyCode": "VND"})},
			{Name: "qty", FieldType: "number"},
		}},
	}
	cases := []struct {
		name       string
		metric     string
		agg        string
		wantType   string
		wantCode   string
		wantStored bool
	}{
		{"sum money", "sum", "total", "currency", "VND", true},
		{"sum plain", "sum", "qty", "number", "", true},
		{"count", "count", "", "number", "", true},
		{"not stored is untouched", "sum", "total", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := map[string]any{"childEntity": "invoices", "viaRelation": "customer", "metric": tc.metric}
			if tc.agg != "" {
				opts["aggField"] = tc.agg
			}
			if tc.wantStored {
				opts["stored"] = true
			}
			f := FieldInput{Name: "debt", FieldType: "rollup", Options: opts}
			if err := validateRollupCrossEntity(context.Background(), meta, "t1", "customers", nil, f); err != nil {
				t.Fatalf("validateRollupCrossEntity: %v", err)
			}
			gotType, _ := opts["valueType"].(string)
			gotCode, _ := opts["currencyCode"].(string)
			if gotType != tc.wantType || gotCode != tc.wantCode {
				t.Fatalf("valueType=%q currencyCode=%q; want %q %q", gotType, gotCode, tc.wantType, tc.wantCode)
			}
		})
	}
}

func TestValidateStoredRollupImmutable(t *testing.T) {
	stored := map[string]any{"childEntity": "enrollments", "viaRelation": "class", "metric": "count",
		"stored": true, "valueType": "number"}
	t.Run("relabel-only edit keeps the derived keys", func(t *testing.T) {
		newOpts := map[string]any{"childEntity": "enrollments", "viaRelation": "class", "metric": "count", "stored": true}
		got, err := validateStoredRollupImmutable("si_so", stored, newOpts)
		if err != nil {
			t.Fatalf("want ok, got %v", err)
		}
		if got["valueType"] != "number" {
			t.Fatalf("valueType lost on edit: %#v", got)
		}
	})
	for _, k := range []string{"metric", "childEntity", "viaRelation", "stored"} {
		t.Run("change "+k, func(t *testing.T) {
			newOpts := map[string]any{"childEntity": "enrollments", "viaRelation": "class", "metric": "count", "stored": true}
			if k == "stored" {
				newOpts["stored"] = false
			} else {
				newOpts[k] = "other"
			}
			_, err := validateStoredRollupImmutable("si_so", stored, newOpts)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "FIELD_ROLLUP_STORED_IMMUTABLE" {
				t.Fatalf("want FIELD_ROLLUP_STORED_IMMUTABLE, got %v", err)
			}
		})
	}
	t.Run("plain roll-up is unrestricted", func(t *testing.T) {
		plain := map[string]any{"childEntity": "a", "viaRelation": "b", "metric": "count"}
		if _, err := validateStoredRollupImmutable("x", plain, map[string]any{"childEntity": "c", "viaRelation": "b", "metric": "sum"}); err != nil {
			t.Fatalf("a read-time roll-up stays freely editable, got %v", err)
		}
	})
	t.Run("turning storing on is refused", func(t *testing.T) {
		plain := map[string]any{"childEntity": "a", "viaRelation": "b", "metric": "count"}
		if _, err := validateStoredRollupImmutable("x", plain, map[string]any{"childEntity": "a", "viaRelation": "b", "metric": "count", "stored": true}); err == nil {
			t.Fatal("flipping stored on after creation must be refused (no valueType stamped, no backfill)")
		}
	})
}

func TestValidateFilterOperator_StoredRollup(t *testing.T) {
	stored := FieldSpec{Name: "si_so", FieldType: "rollup", Options: map[string]any{"stored": true, "valueType": "number"}}
	money := FieldSpec{Name: "no", FieldType: "rollup", Options: map[string]any{"stored": true, "valueType": "currency"}}
	plain := FieldSpec{Name: "si_so", FieldType: "rollup", Options: map[string]any{}}

	flags, sub, err := validateFilterOperator(stored, dynrepo.FilterGte, []string{"30"})
	if err != nil || !flags.CastNumeric || sub != "" {
		t.Fatalf("stored number roll-up gte: flags=%+v sub=%q err=%v; want numeric cast, no sub-path", flags, sub, err)
	}
	flags, sub, err = validateFilterOperator(money, dynrepo.FilterGt, []string{"100"})
	if err != nil || !flags.CastNumeric || sub != "amount" {
		t.Fatalf("stored currency roll-up gt: flags=%+v sub=%q err=%v; want numeric cast on .amount", flags, sub, err)
	}
	if _, _, err := validateFilterOperator(plain, dynrepo.FilterGte, []string{"30"}); err == nil {
		t.Fatal("a read-time roll-up must still refuse ordered filters")
	}
}

func TestResolveMetricField_StoredRollup(t *testing.T) {
	byName := map[string]FieldSpec{
		"si_so": {Name: "si_so", FieldType: "rollup", Options: map[string]any{"stored": true, "valueType": "number"}},
		"no":    {Name: "no", FieldType: "rollup", Options: map[string]any{"stored": true, "valueType": "currency", "currencyCode": "VND"}},
		"live":  {Name: "live", FieldType: "rollup", Options: map[string]any{}},
	}
	if col, sub, code, err := resolveMetricField(AggMetricSum, "si_so", byName); err != nil || col != "si_so" || sub != "" || code != "" {
		t.Fatalf("stored number: %q %q %q %v", col, sub, code, err)
	}
	if col, sub, code, err := resolveMetricField(AggMetricSum, "no", byName); err != nil || col != "no" || sub != "amount" || code != "VND" {
		t.Fatalf("stored currency: %q %q %q %v", col, sub, code, err)
	}
	var ve *ValidationError
	if _, _, _, err := resolveMetricField(AggMetricSum, "live", byName); !errors.As(err, &ve) || ve.Code != "AGGREGATE_FIELD_NOT_NUMERIC" {
		t.Fatalf("read-time roll-up must stay refused, got %v", err)
	}
}

func TestApplyRollups_SkipsStoredRollup(t *testing.T) {
	store := &fakeRollupStore{}
	meta := &fakeRollupMeta{}
	svc := rollupTestService(store, meta, nil)
	spec := newFKRollupSpec("si_so", "enrollments", "class", "count", "")
	spec.Options["stored"] = true
	recs := []dynrepo.Record{{ID: "c1", Data: map[string]any{"si_so": float64(7)}}}

	if err := svc.applyRollups(context.Background(), RecordScope{IsAdmin: true}, "t1", recs, []FieldSpec{spec}, []string{"si_so"}, nil, nil); err != nil {
		t.Fatalf("applyRollups: %v", err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("a stored roll-up must not be re-aggregated on read; %d aggregate calls", len(store.calls))
	}
	if recs[0].Data["si_so"] != float64(7) {
		t.Fatalf("stored value overwritten: %v", recs[0].Data["si_so"])
	}
}

// --- refresh orchestration -------------------------------------------------------------------

type storedRefreshStore struct {
	fakeRollupStore
	parents map[string]*dynrepo.Record // by parent id
	writes  []map[string]any
	writeID []string
	casMiss int // SetComputedValues reports "version moved" this many times first
}

func (f *storedRefreshStore) GetRecord(_ context.Context, _, _, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	if r, ok := f.parents[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, dynrepo.ErrRecordNotFound
}

func (f *storedRefreshStore) SetComputedValues(_ context.Context, _, _, id string, _ int, values map[string]any) (bool, error) {
	if f.casMiss > 0 {
		f.casMiss--
		return false, nil
	}
	f.writes = append(f.writes, values)
	f.writeID = append(f.writeID, id)
	if r, ok := f.parents[id]; ok {
		for k, v := range values {
			r.Data[k] = v
		}
	}
	return true, nil
}

type storedRefreshMeta struct {
	fakeRollupMeta
	refs []metadata.StoredRollupField
	got  []string // tenant/child the finder was asked for
}

func (f *storedRefreshMeta) ListStoredRollupsOfChild(_ context.Context, tenantID, child string) ([]metadata.StoredRollupField, error) {
	f.got = append(f.got, tenantID+"/"+child)
	return f.refs, nil
}

func newStoredRefreshFixture(t *testing.T, count int64) (*RecordService, *storedRefreshStore, *storedRefreshMeta) {
	t.Helper()
	store := &storedRefreshStore{
		fakeRollupStore: fakeRollupStore{byEntity: map[string]dynrepo.AggregateScalar{"enrollments": {Count: count}}},
		parents: map[string]*dynrepo.Record{
			"c1": {ID: "c1", Version: 3, Data: map[string]any{}},
			"c2": {ID: "c2", Version: 1, Data: map[string]any{"si_so": float64(9)}},
		},
	}
	meta := &storedRefreshMeta{
		fakeRollupMeta: fakeRollupMeta{},
		refs: []metadata.StoredRollupField{{
			ParentEntity: "classes", Name: "si_so",
			Options: sPtr(`{"childEntity":"enrollments","viaRelation":"class","metric":"count","stored":true}`),
		}},
	}
	return rollupTestService(store, meta, nil), store, meta
}

func TestRefreshStoredRollups_WritesNewTotalOnTenantScopedParent(t *testing.T) {
	svc, store, meta := newStoredRefreshFixture(t, 4)

	svc.refreshStoredRollups(context.Background(), "t1", "enrollments", map[string]any{"class": "c1", "student": "s1"})

	if len(store.writes) != 1 || store.writeID[0] != "c1" || store.writes[0]["si_so"] != float64(4) {
		t.Fatalf("writes=%v ids=%v; want one write si_so=4 on c1", store.writes, store.writeID)
	}
	if len(meta.got) != 1 || meta.got[0] != "t1/enrollments" {
		t.Fatalf("finder asked %v; want tenant-scoped t1/enrollments", meta.got)
	}
	// The count ran as the system over the whole tenant: admin scope, filter on the parent only.
	call := store.calls[0]
	if call.entity != "enrollments" || !call.scope.IsAdmin || call.params.Filters[0].Column != "class_id" || call.params.Filters[0].Values[0] != "c1" {
		t.Fatalf("aggregate call = %+v", call)
	}
}

func TestRefreshStoredRollups_EditMovingChildRefreshesBothParents(t *testing.T) {
	svc, store, _ := newStoredRefreshFixture(t, 4)

	svc.refreshStoredRollups(context.Background(), "t1", "enrollments",
		map[string]any{"class": "c1"}, map[string]any{"class": "c2"}, map[string]any{"class": "c2"})

	if len(store.writeID) != 2 || store.writeID[0] != "c1" || store.writeID[1] != "c2" {
		t.Fatalf("written parents = %v; want c1 then c2 once each", store.writeID)
	}
}

func TestRefreshStoredRollups_SkipsWhenNothingToDo(t *testing.T) {
	t.Run("value unchanged -> no write", func(t *testing.T) {
		svc, store, _ := newStoredRefreshFixture(t, 9) // c2 already holds 9
		svc.refreshStoredRollups(context.Background(), "t1", "enrollments", map[string]any{"class": "c2"})
		if len(store.writes) != 0 {
			t.Fatalf("unchanged total was rewritten: %v", store.writes)
		}
	})
	t.Run("missing parent -> no write, no panic", func(t *testing.T) {
		svc, store, _ := newStoredRefreshFixture(t, 4)
		svc.refreshStoredRollups(context.Background(), "t1", "enrollments", map[string]any{"class": "gone"})
		if len(store.writes) != 0 {
			t.Fatalf("wrote to a missing parent: %v", store.writes)
		}
	})
	t.Run("image without the relation -> no aggregate at all", func(t *testing.T) {
		svc, store, _ := newStoredRefreshFixture(t, 4)
		svc.refreshStoredRollups(context.Background(), "t1", "enrollments", map[string]any{"student": "s1"}, nil)
		if len(store.calls) != 0 || len(store.writes) != 0 {
			t.Fatalf("calls=%d writes=%d; want none", len(store.calls), len(store.writes))
		}
	})
	t.Run("metadata without the finder -> off", func(t *testing.T) {
		store := &storedRefreshStore{}
		svc := rollupTestService(store, &fakeRollupMeta{}, nil)
		svc.refreshStoredRollups(context.Background(), "t1", "enrollments", map[string]any{"class": "c1"})
		if len(store.calls) != 0 {
			t.Fatal("refresh ran without a finder")
		}
	})
}

func TestRefreshStoredRollups_RetriesWhenParentVersionMoves(t *testing.T) {
	svc, store, _ := newStoredRefreshFixture(t, 4)
	store.casMiss = 2 // a user save beat us twice

	svc.refreshStoredRollups(context.Background(), "t1", "enrollments", map[string]any{"class": "c1"})

	if len(store.writes) != 1 {
		t.Fatalf("writes=%v; want the write to land on the third attempt", store.writes)
	}
}

func TestRecomputeStoredRollup_RejectsPlainRollup(t *testing.T) {
	cat := &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-c", Name: "classes"},
		fields: []metadata.FieldDefinition{{Name: "live", FieldType: "rollup",
			Options: jsonOptsPtr(t, map[string]any{"childEntity": "enrollments", "viaRelation": "class", "metric": "count"})}},
	}
	svc := NewRecordService(cat, &storedRefreshStore{}).WithRollupMeta(&fakeRollupMeta{})
	_, err := svc.RecomputeStoredRollup(context.Background(), "t1", "classes", "live")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "FIELD_ROLLUP_NOT_STORED" {
		t.Fatalf("want FIELD_ROLLUP_NOT_STORED, got %v", err)
	}
}

func TestSeedStoredCounts(t *testing.T) {
	cnt := FieldSpec{Name: "si_so", FieldType: "rollup", Options: map[string]any{"stored": true, "metric": "count"}}
	sum := FieldSpec{Name: "tong", FieldType: "rollup", Options: map[string]any{"stored": true, "metric": "sum"}}
	live := FieldSpec{Name: "live", FieldType: "rollup", Options: map[string]any{"metric": "count"}}
	d := map[string]any{"name": "A"}
	seedStoredCounts([]FieldSpec{cnt, sum, live}, d)
	if d["si_so"] != float64(0) {
		t.Fatalf("si_so = %#v; want 0", d["si_so"])
	}
	if _, ok := d["tong"]; ok {
		t.Fatal("sum must stay absent")
	}
	if _, ok := d["live"]; ok {
		t.Fatal("read-time roll-up must not be stored")
	}
	d2 := map[string]any{"si_so": float64(7)}
	seedStoredCounts([]FieldSpec{cnt}, d2)
	if d2["si_so"] != float64(7) {
		t.Fatal("explicit value overwritten")
	}
}
