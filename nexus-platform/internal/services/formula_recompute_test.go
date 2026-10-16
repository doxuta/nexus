package services

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// fakeFormulaRows is an in-memory formulaRowStore: rows in created_at order, keyset pages of
// `Limit` addressed by an index cursor, a version-guarded SetComputedValues with JSON_MERGE_PATCH
// semantics (a nil value removes the key), and an optional concurrent save injected before the
// first write of one row.
type fakeFormulaRows struct {
	rows      []dynrepo.Record
	lists     int
	sets      []string // ids written
	listErr   error
	setErr    error
	gotScopes []dynrepo.RowScope
	gotTenant string
	// raceID, when set, simulates a user save landing on that row between the sweep's read and
	// its first write: raceData is merged in and the version bumped, so the write loses.
	raceID   string
	raceData map[string]any
	raced    bool
}

func (f *fakeFormulaRows) ListRecordsCursor(_ context.Context, _, tenantID string, scope dynrepo.RowScope, p dynrepo.CursorParams) ([]dynrepo.Record, string, error) {
	f.lists++
	f.gotTenant = tenantID
	f.gotScopes = append(f.gotScopes, scope)
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	start := 0
	if p.After != "" {
		start, _ = strconv.Atoi(p.After)
	}
	end := min(start+p.Limit, len(f.rows))
	page := make([]dynrepo.Record, 0, end-start)
	for _, r := range f.rows[start:end] {
		r.Data = cloneData(r.Data)
		page = append(page, r)
	}
	next := ""
	if end < len(f.rows) {
		next = strconv.Itoa(end)
	}
	return page, next, nil
}

func (f *fakeFormulaRows) GetRecord(_ context.Context, _, _, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	for _, r := range f.rows {
		if r.ID == id {
			r.Data = cloneData(r.Data)
			return &r, nil
		}
	}
	return nil, dynrepo.ErrRecordNotFound
}

func (f *fakeFormulaRows) SetComputedValues(_ context.Context, _, _, id string, version int, values map[string]any) (bool, error) {
	if f.setErr != nil {
		return false, f.setErr
	}
	for i := range f.rows {
		r := &f.rows[i]
		if r.ID != id {
			continue
		}
		if id == f.raceID && !f.raced {
			f.raced = true
			for k, v := range f.raceData {
				r.Data[k] = v
			}
			r.Version++
		}
		if r.Version != version {
			return false, nil
		}
		for k, v := range values {
			if v == nil {
				delete(r.Data, k)
				continue
			}
			r.Data[k] = v
		}
		f.sets = append(f.sets, id)
		return true, nil
	}
	return false, nil
}

func sweepRow(id string, data map[string]any) dynrepo.Record {
	return dynrepo.Record{ID: id, Data: data}
}

var qtySpecs = []FieldSpec{
	{Name: "qty", FieldType: "number"},
	{Name: "total", FieldType: "formula", Options: map[string]any{"formula": "qty * 10"}},
}

func TestRecomputeStoredFormulas(t *testing.T) {
	currencySpecs := []FieldSpec{
		{Name: "qty", FieldType: "number"},
		{Name: "total", FieldType: "formula", Options: map[string]any{"formula": "qty * 10", "resultType": "currency", "currencyCode": "VND"}},
	}
	divSpecs := []FieldSpec{
		{Name: "qty", FieldType: "number"},
		{Name: "total", FieldType: "formula", Options: map[string]any{"formula": "10 / qty"}},
	}
	many := make([]dynrepo.Record, 150)
	for i := range many {
		many[i] = sweepRow(fmt.Sprintf("r%03d", i), map[string]any{"qty": float64(1)})
	}
	cases := []struct {
		name        string
		specs       []FieldSpec
		store       *fakeFormulaRows
		wantWritten int
		wantLists   int
		want        map[string]map[string]any // id -> expected data after the sweep
		wantErr     string
	}{
		{
			name:  "rows written before the formula existed get their value",
			specs: qtySpecs,
			store: &fakeFormulaRows{rows: []dynrepo.Record{
				sweepRow("a", map[string]any{"qty": float64(2)}),
				sweepRow("b", map[string]any{"qty": float64(3)}),
			}},
			wantWritten: 2, wantLists: 1,
			want: map[string]map[string]any{
				"a": {"qty": float64(2), "total": float64(20)},
				"b": {"qty": float64(3), "total": float64(30)},
			},
		},
		{
			name:        "rows already holding the result are not written",
			specs:       qtySpecs,
			store:       &fakeFormulaRows{rows: []dynrepo.Record{sweepRow("a", map[string]any{"qty": float64(2), "total": float64(20)})}},
			wantWritten: 0, wantLists: 1,
		},
		{
			name:  "a resultType change re-stores the new shape",
			specs: currencySpecs,
			store: &fakeFormulaRows{rows: []dynrepo.Record{sweepRow("a", map[string]any{"qty": float64(2), "total": float64(20)})}},
			want: map[string]map[string]any{
				"a": {"qty": float64(2), "total": map[string]any{"amount": float64(20), "currencyCode": "VND"}},
			},
			wantWritten: 1, wantLists: 1,
		},
		{
			name:        "a result that is now undefined removes the stale value",
			specs:       divSpecs,
			store:       &fakeFormulaRows{rows: []dynrepo.Record{sweepRow("a", map[string]any{"qty": float64(0), "total": float64(5)})}},
			want:        map[string]map[string]any{"a": {"qty": float64(0)}},
			wantWritten: 1, wantLists: 1,
		},
		{
			name:        "undefined and absent are the same: nothing to write",
			specs:       divSpecs,
			store:       &fakeFormulaRows{rows: []dynrepo.Record{sweepRow("a", map[string]any{"qty": float64(0)})}},
			wantWritten: 0, wantLists: 1,
		},
		{
			name:        "an entity without formulas is not read at all",
			specs:       []FieldSpec{{Name: "qty", FieldType: "number"}},
			store:       &fakeFormulaRows{rows: []dynrepo.Record{sweepRow("a", map[string]any{"qty": float64(2)})}},
			wantWritten: 0, wantLists: 0,
		},
		{
			name:        "every page is swept",
			specs:       qtySpecs,
			store:       &fakeFormulaRows{rows: many},
			wantWritten: 150, wantLists: 2,
		},
		{
			name:  "a save landing between read and write wins, the row is recomputed from it",
			specs: qtySpecs,
			store: &fakeFormulaRows{
				rows:   []dynrepo.Record{sweepRow("a", map[string]any{"qty": float64(2)})},
				raceID: "a", raceData: map[string]any{"qty": float64(7)},
			},
			want:        map[string]map[string]any{"a": {"qty": float64(7), "total": float64(70)}},
			wantWritten: 1, wantLists: 1,
		},
		{
			name:    "a list failure is reported",
			specs:   qtySpecs,
			store:   &fakeFormulaRows{listErr: errors.New("boom")},
			wantErr: "services.recomputeStoredFormulas: list it_lines: boom", wantLists: 1,
		},
		{
			name:    "a write failure is reported with the row",
			specs:   qtySpecs,
			store:   &fakeFormulaRows{rows: []dynrepo.Record{sweepRow("a", map[string]any{"qty": float64(2)})}, setErr: errors.New("lock")},
			wantErr: "services.recomputeStoredFormulas: it_lines a: lock", wantLists: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := recomputeStoredFormulas(context.Background(), tc.store, "tenant-1", "it_lines", tc.specs)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v; want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("recomputeStoredFormulas: %v", err)
			}
			if n != tc.wantWritten {
				t.Errorf("written = %d; want %d (sets %v)", n, tc.wantWritten, tc.store.sets)
			}
			if tc.store.lists != tc.wantLists {
				t.Errorf("list calls = %d; want %d", tc.store.lists, tc.wantLists)
			}
			for id, want := range tc.want {
				got, _ := tc.store.GetRecord(context.Background(), "", "", id, dynrepo.RowScope{})
				if got == nil || !reflect.DeepEqual(got.Data, want) {
					t.Errorf("row %s = %v; want %v", id, got, want)
				}
			}
			// The sweep reads the whole tenant (admin scope) and never another tenant.
			for _, sc := range tc.store.gotScopes {
				if !sc.IsAdmin {
					t.Errorf("sweep scope = %+v; want the system (admin) scope", sc)
				}
			}
			if tc.store.lists > 0 && tc.store.gotTenant != "tenant-1" {
				t.Errorf("tenant = %q; want tenant-1", tc.store.gotTenant)
			}
		})
	}
}

// addMetaSeesCreated makes the AddField fake return the just-created field from ListFields, as
// the real repository does once the row is committed.
type addMetaSeesCreated struct{ *fakeFieldMeta }

func (m addMetaSeesCreated) ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	existing, err := m.fakeFieldMeta.ListFields(ctx, tenantID, entityID)
	return append(append([]metadata.FieldDefinition{}, existing...), m.created...), err
}

func TestAddField_FormulaRecomputesExistingRows(t *testing.T) {
	cases := []struct {
		name        string
		field       FieldInput
		setErr      error
		wantSets    int
		wantLists   int
		wantErrPart string
	}{
		{
			name:     "a formula added over existing rows gives them their value",
			field:    FieldInput{Name: "total", Label: "Thành tiền", FieldType: "formula", Options: map[string]any{"formula": "qty * 10"}},
			wantSets: 2, wantLists: 1,
		},
		{
			name:      "a plain field sweeps nothing",
			field:     FieldInput{Name: "note", Label: "Ghi chú", FieldType: "text"},
			wantSets:  0,
			wantLists: 0,
		},
		{
			name:        "a failed sweep is reported, the field is kept",
			field:       FieldInput{Name: "total", Label: "Thành tiền", FieldType: "formula", Options: map[string]any{"formula": "qty * 10"}},
			setErr:      errors.New("lock"),
			wantLists:   1,
			wantErrPart: "services.AddField: field saved, stored formulas not recomputed: services.recomputeFormulas: services.recomputeStoredFormulas: courses a: lock",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := &fakeFieldMeta{entity: entityFixture(), existing: []metadata.FieldDefinition{
				{ID: "f-qty", EntityID: "ent-1", Name: "qty", FieldType: "number"},
			}}
			rows := &fakeFormulaRows{setErr: tc.setErr, rows: []dynrepo.Record{
				sweepRow("a", map[string]any{"qty": float64(2)}),
				sweepRow("b", map[string]any{"qty": float64(3)}),
			}}
			svc := NewFieldService(&fakeFieldSchema{}, addMetaSeesCreated{meta}).WithFormulaRows(rows)
			_, err := svc.AddField(context.Background(), "tenant-1", AddFieldInput{EntityName: "courses", Field: tc.field})
			if tc.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("err = %v; want it to contain %q", err, tc.wantErrPart)
				}
				if len(meta.created) != 1 {
					t.Fatalf("the field must stay saved; created=%v", meta.created)
				}
			} else if err != nil {
				t.Fatalf("AddField: %v", err)
			}
			if len(rows.sets) != tc.wantSets || rows.lists != tc.wantLists {
				t.Errorf("sets=%v lists=%d; want %d sets, %d lists", rows.sets, rows.lists, tc.wantSets, tc.wantLists)
			}
			if rows.lists > 0 && rows.gotTenant != "tenant-1" {
				t.Errorf("sweep tenant = %q; want tenant-1", rows.gotTenant)
			}
		})
	}
}

// editMetaSeesUpdate makes the EditField fake's ListFields reflect the committed edit.
type editMetaSeesUpdate struct{ *fakeEditMeta }

func (m editMetaSeesUpdate) ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	out, err := m.fakeEditMeta.ListFields(ctx, tenantID, entityID)
	out = append([]metadata.FieldDefinition{}, out...)
	for _, u := range m.updated {
		for i := range out {
			if out[i].ID == u.ID {
				out[i] = u
			}
		}
	}
	return out, err
}

func TestEditField_FormulaRecomputesStoredRows(t *testing.T) {
	opts := `{"formula":"qty * 10"}`
	stored := metadata.FieldDefinition{ID: "f-total", EntityID: "ent-1", Name: "total", FieldType: "formula", Options: &opts}
	cases := []struct {
		name    string
		in      EditFieldInput
		want    any
		wantSet bool
	}{
		{
			name: "a new result type re-stores every row in the new shape",
			in: EditFieldInput{FieldID: "f-total", Options: map[string]any{
				"formula": "qty * 10", "resultType": "currency", "currencyCode": "VND",
			}},
			want: map[string]any{"amount": float64(20), "currencyCode": "VND"}, wantSet: true,
		},
		{
			name:    "a new expression re-stores every row",
			in:      EditFieldInput{FieldID: "f-total", Options: map[string]any{"formula": "qty * 100"}},
			want:    float64(200),
			wantSet: true,
		},
		{
			name: "a label-only edit sweeps nothing",
			in:   EditFieldInput{FieldID: "f-total", Label: strPtr("Thành tiền")},
			want: float64(20),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := &fakeEditMeta{field: &stored, siblings: []metadata.FieldDefinition{
				{ID: "f-qty", EntityID: "ent-1", Name: "qty", FieldType: "number"},
				stored,
			}}
			rows := &fakeFormulaRows{rows: []dynrepo.Record{sweepRow("a", map[string]any{"qty": float64(2), "total": float64(20)})}}
			svc := NewFieldService(&fakeFieldSchema{}, editMetaSeesUpdate{meta}).WithFormulaRows(rows)
			if _, err := svc.EditField(context.Background(), "tenant-1", tc.in); err != nil {
				t.Fatalf("EditField: %v", err)
			}
			if got := rows.rows[0].Data["total"]; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("total = %#v; want %#v", got, tc.want)
			}
			if (len(rows.sets) > 0) != tc.wantSet {
				t.Errorf("sets = %v; want written=%v", rows.sets, tc.wantSet)
			}
		})
	}
}
