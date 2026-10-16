package services

// Wiring proof for #272: every read path (Get, List, cursor List, CSV export, report axis) must
// resolve relation labels through expandRecords, i.e. against the TARGET table's label field.
// TestWithTargetLabelFields pins the helper alone; these pin the call sites — revert any one of
// them to a bare applyExpand and the matching case goes red (UUID / "name" instead of the name).

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// wiringCatalog serves several entities by name (fakeRecordCatalog serves one for every name).
type wiringCatalog struct {
	ents   map[string]*metadata.EntityDefinition
	fields map[string][]metadata.FieldDefinition // by entity ID
}

func (c wiringCatalog) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	if e, ok := c.ents[name]; ok {
		return e, nil
	}
	return nil, metadata.ErrEntityNotFound
}
func (c wiringCatalog) ListFields(_ context.Context, _, id string) ([]metadata.FieldDefinition, error) {
	return c.fields[id], nil
}

// labelSpyFetcher records which field BatchFetchLabels was asked to read.
type labelSpyFetcher struct{ gotLabelFields []string }

func (f *labelSpyFetcher) BatchFetchLabels(_ context.Context, _, _, labelField string, ids []string) ([]dynrepo.LabelResult, error) {
	f.gotLabelFields = append(f.gotLabelFields, labelField)
	out := make([]dynrepo.LabelResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, dynrepo.LabelResult{ID: id, Label: "Công ty ABC"})
	}
	return out, nil
}

type getStore struct {
	*fakeRecordStore
	rec *dynrepo.Record
}

func (g getStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	c := *g.rec
	c.Data = map[string]any{"account": g.rec.Data["account"], "name": g.rec.Data["name"]}
	return &c, nil
}

// wiringFixture: quotes.account -> accounts; accounts has NO "name", the builder chose ten_kh
// (explicit) — the blind "name" default would label nothing.
func wiringFixture() (*RecordService, *labelSpyFetcher, *fakeRecordStore) {
	acctOpts := `{"relatedEntity":"accounts"}`
	accOptions := `{"labelField":"ten_kh"}`
	cat := wiringCatalog{
		ents: map[string]*metadata.EntityDefinition{
			"quotes":   {ID: "q", TenantID: "tenant-1", Name: "quotes", DataTable: "obj_quotes"},
			"accounts": {ID: "a", TenantID: "tenant-1", Name: "accounts", DataTable: "obj_accounts", Options: &accOptions},
		},
		fields: map[string][]metadata.FieldDefinition{
			"q": {{Name: "name", Label: "Tên", FieldType: "text"}, {Name: "account", Label: "Khách", FieldType: "relation", Options: &acctOpts}},
			"a": {{Name: "ten_kh", FieldType: "text", IsRequired: true}, {Name: "ma", FieldType: "text"}},
		},
	}
	rows := []dynrepo.Record{{ID: "q1", Data: map[string]any{"name": "BG 1", "account": "acc-1"}}}
	store := &fakeRecordStore{listRecords: rows, cursorRecords: rows}
	spy := &labelSpyFetcher{}
	return NewRecordService(cat, store).WithLabelFetcher(spy), spy, store
}

func assertTargetLabelField(t *testing.T, spy *labelSpyFetcher, path string) {
	t.Helper()
	if len(spy.gotLabelFields) == 0 {
		t.Fatalf("%s: BatchFetchLabels never ran", path)
	}
	for _, lf := range spy.gotLabelFields {
		if lf != "ten_kh" {
			t.Errorf("%s: label read from %q; want the target table's label field ten_kh", path, lf)
		}
	}
}

func TestExpandWiring_AllReadPathsUseTargetLabelField(t *testing.T) {
	scope := RecordScope{UserID: "u1", AccessLevel: "all"}
	ctx := context.Background()

	t.Run("Get", func(t *testing.T) {
		svc, spy, store := wiringFixture()
		svc.store = getStore{fakeRecordStore: store, rec: &store.listRecords[0]}
		if _, err := svc.Get(ctx, scope, "tenant-1", "quotes", "q1", []string{"account"}, nil); err != nil {
			t.Fatalf("Get: %v", err)
		}
		assertTargetLabelField(t, spy, "Get")
	})
	t.Run("List offset", func(t *testing.T) {
		svc, spy, _ := wiringFixture()
		if _, err := svc.List(ctx, scope, "tenant-1", "quotes", ListQuery{Page: 1, Limit: 10, Expand: []string{"account"}}); err != nil {
			t.Fatalf("List: %v", err)
		}
		assertTargetLabelField(t, spy, "List")
	})
	t.Run("List cursor", func(t *testing.T) {
		svc, spy, _ := wiringFixture()
		if _, err := svc.List(ctx, scope, "tenant-1", "quotes", ListQuery{Limit: 10, Paginate: "cursor", Expand: []string{"account"}}); err != nil {
			t.Fatalf("List cursor: %v", err)
		}
		assertTargetLabelField(t, spy, "List cursor")
	})
	t.Run("CSV export", func(t *testing.T) {
		svc, spy, _ := wiringFixture()
		var buf bytes.Buffer
		if err := svc.ExportCSV(ctx, scope, "tenant-1", "quotes", ExportCSVOptions{Columns: []string{"name", "account"}, Expand: []string{"account"}}, &buf); err != nil {
			t.Fatalf("ExportCSV: %v", err)
		}
		assertTargetLabelField(t, spy, "ExportCSV")
		if !strings.Contains(buf.String(), "Công ty ABC") {
			t.Errorf("csv = %q; want the resolved name, not the id", buf.String())
		}
	})
}

// A summary report grouped by a relation names its groups through the same expand: the target
// table here has no "name" field, only ten_kh.
func TestExpandWiring_ReportAxisUsesTargetLabelField(t *testing.T) {
	store := &axisLabelsStore{groups: []dynrepo.AggregateGroup{{Key: sp("acc-1"), Value: sp("10"), Count: 2}}}
	svc, fetcher := axisLabelService("summary", `{"groupBy":"account","metric":"count"}`, store, viewDealsAndAccounts, nil)
	cat := svc.records.catalog.(*rjCatalog)
	cat.fields["accounts"] = []metadata.FieldDefinition{{Name: "ten_kh", FieldType: "text", IsRequired: true}}
	if _, err := svc.RunReport(context.Background(), gridRep, "t1", "rep1", nil); err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if len(fetcher.gotLabelFields) == 0 {
		t.Fatal("axis label never fetched")
	}
	for _, lf := range fetcher.gotLabelFields {
		if lf != "ten_kh" {
			t.Errorf("axis label read from %q; want ten_kh", lf)
		}
	}
}
