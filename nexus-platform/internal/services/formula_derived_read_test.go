package services

// queue #284 review: the read-path wiring of derived formulas (Get, List offset, List cursor,
// ExportCSV) and their ACL behaviour, driven through the PUBLIC RecordService surface. Removing an
// applyDerivedFormulas/withDerivedRollups call in any of the four paths turns these red.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// derivedStore serves fresh records per call (the apply* helpers mutate Data in place) on every
// read surface, including the keyset walk List cursor mode and ExportCSV use.
type derivedStore struct {
	fakeComputedStore
}

func derivedDeal(id string) dynrepo.Record {
	return dynrepo.Record{ID: id, Data: map[string]any{"name": "Deal " + id, "contact": "c-1", "so_luong": float64(2)}}
}

func (s *derivedStore) GetRecord(context.Context, string, string, string, dynrepo.RowScope) (*dynrepo.Record, error) {
	r := derivedDeal("d1")
	return &r, nil
}

func (s *derivedStore) ListRecords(context.Context, string, string, dynrepo.RowScope, dynrepo.ListParams) ([]dynrepo.Record, int, error) {
	return []dynrepo.Record{derivedDeal("d1")}, 1, nil
}

func (s *derivedStore) ListRecordsCursor(_ context.Context, _, _ string, _ dynrepo.RowScope, p dynrepo.CursorParams) ([]dynrepo.Record, string, error) {
	return []dynrepo.Record{derivedDeal("d1")}, "", nil
}

// derivedSvc: deals + qty + two derived formulas, one over the contact_salary LOOKUP and one over
// the payment_total ROLLUP (never requested via ?rollup=).
func derivedSvc(t *testing.T, loader *entityKeyedFieldACLLoader) *RecordService {
	t.Helper()
	sum := "1200.5000000000"
	store := &derivedStore{fakeComputedStore{agg: dynrepo.AggregateScalar{Value: &sum, Count: 3}}}
	cat := dealsComputedFixture(t)
	cat.fields = append(cat.fields,
		metadata.FieldDefinition{Name: "so_luong", FieldType: "number"},
		metadata.FieldDefinition{Name: "thanh_tien", FieldType: "formula", Options: jsonOptsPtr(t, map[string]any{"formula": "so_luong * contact_salary"})},
		metadata.FieldDefinition{Name: "con_lai", FieldType: "formula", Options: jsonOptsPtr(t, map[string]any{"formula": "payment_total - so_luong"})},
	)
	return NewRecordService(cat, store).
		WithFieldACL(NewFieldACLService(loader)).
		WithFieldValueFetcher(newFakeFieldValueFetcher(map[string]any{"c-1": float64(50000)})).
		WithRollupMeta(paymentsRollupMeta())
}

func assertDerived(t *testing.T, surface string, d map[string]any, wantThanhTien, wantConLai any) {
	t.Helper()
	if d["thanh_tien"] != wantThanhTien {
		t.Errorf("%s: thanh_tien = %v; want %v", surface, d["thanh_tien"], wantThanhTien)
	}
	if d["con_lai"] != wantConLai {
		t.Errorf("%s: con_lai = %v; want %v", surface, d["con_lai"], wantConLai)
	}
}

func TestDerivedFormulas_AllReadSurfaces(t *testing.T) {
	ctx := context.Background()
	svc := derivedSvc(t, &entityKeyedFieldACLLoader{})
	scope := nonAdminScope()

	got, err := svc.Get(ctx, scope, "tenant-1", "deals", "d1", nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertDerived(t, "Get", got.Data, float64(100000), 1198.5)

	res, err := svc.List(ctx, scope, "tenant-1", "deals", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("List offset: %v", err)
	}
	assertDerived(t, "List offset", res.Records[0].Data, float64(100000), 1198.5)

	res, err = svc.List(ctx, scope, "tenant-1", "deals", ListQuery{Limit: 20, Paginate: "cursor"})
	if err != nil {
		t.Fatalf("List cursor: %v", err)
	}
	assertDerived(t, "List cursor", res.Records[0].Data, float64(100000), 1198.5)

	var buf bytes.Buffer
	if err := svc.ExportCSV(ctx, scope, "tenant-1", "deals", ExportCSVOptions{Columns: []string{"thanh_tien", "con_lai"}}, &buf); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	if !strings.Contains(buf.String(), "100000") || !strings.Contains(buf.String(), "1198.5") {
		t.Errorf("ExportCSV missing derived values:\n%s", buf.String())
	}
}

// An operand the caller may not see (lookup source denied on the target; rollup aggField denied
// on the child) must make the formula read null on every surface — not 0-based wrong numbers.
func TestDerivedFormulas_DeniedOperandReadsNull(t *testing.T) {
	ctx := context.Background()
	loader := &entityKeyedFieldACLLoader{rowsByEntity: map[string][]auth.FieldACL{
		"contacts": {{FieldName: "salary", CanView: false}},
		"payments": {{FieldName: "amount", CanView: false}},
	}}
	svc := derivedSvc(t, loader)
	scope := nonAdminScope()

	got, err := svc.Get(ctx, scope, "tenant-1", "deals", "d1", nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, f := range []string{"thanh_tien", "con_lai"} {
		if v, present := got.Data[f]; !present || v != nil {
			t.Errorf("Get: %s = %v (present=%v); want explicit nil", f, v, present)
		}
	}
	res, err := svc.List(ctx, scope, "tenant-1", "deals", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertDerived(t, "List offset", res.Records[0].Data, nil, nil)
}
