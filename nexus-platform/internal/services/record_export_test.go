package services

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// newExportService wires a RecordService over the fake store with labelled fields, so the header
// contract can be checked without a database.
func newExportService() (*RecordService, *fakeRecordStore) {
	store := &fakeRecordStore{}
	cat := &fakeRecordCatalog{entity: courseEntityFixture(), fields: []metadata.FieldDefinition{
		{Name: "title", Label: "Tên khoá học", FieldType: "text"},
		{Name: "seats", FieldType: "number"}, // no label → header falls back to the name
		{Name: "level", Label: "=Cấp độ", FieldType: "picklist"},
	}}
	return NewRecordService(cat, store), store
}

// The handler can reset to a JSON error envelope only while nothing has been written. csv.Writer
// buffers until Flush, so that window has always covered the whole first page — a DB timeout or
// a field-ACL load failure on page 1 surfaces as 503/500 JSON, not as a "successful" download.
// The BOM must not shrink that window: a first-page store failure leaves the writer at 0 bytes.
func TestExportCSV_FirstPageErrorWritesNothing(t *testing.T) {
	svc, store := newExportService()
	store.cursorErr = errors.New("db timeout")

	var buf bytes.Buffer
	err := svc.ExportCSV(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ExportCSVOptions{}, &buf)
	if err == nil || !strings.Contains(err.Error(), "db timeout") {
		t.Fatalf("ExportCSV err = %v; want the store error wrapped", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("export wrote %d bytes (%q) before the first-page failure — the handler can no longer reset to a JSON error envelope", buf.Len(), buf.String())
	}
}

// Happy path without a database: BOM first, then labels (falling back to the field name when a
// field has none), csvGuard on a label that starts with '=', and the leading id column kept for
// the whole-entity export.
func TestExportCSV_BOMThenLabelledHeader(t *testing.T) {
	svc, store := newExportService()
	store.cursorRecords = []dynrepo.Record{{ID: "rec-1", Data: map[string]any{"title": "Go căn bản", "seats": float64(12), "level": "basic"}}}

	var buf bytes.Buffer
	if err := svc.ExportCSV(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ExportCSVOptions{}, &buf); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	got := buf.String()
	const want = "\xEF\xBB\xBFid,Tên khoá học,seats,'=Cấp độ\nrec-1,Go căn bản,12,basic\n"
	if got != want {
		t.Fatalf("export =\n%q\nwant\n%q", got, want)
	}
}

// A relation / user column resolved by applyExpand reaches csvCell as the in-process struct
// (expandedValue / expandedUserValue / expandedIDOnly), never as a map. The cell must carry the
// human label — not the UUID the screen never shows, and not a JSON blob of the struct. A label
// is tenant-authored text like any other cell, so it still passes csvGuard.
func TestCSVCell_ExpandedValuesRenderLabel(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"relation label", expandedValue{ID: "acc-1", Label: "Công ty ABC"}, "Công ty ABC"},
		{"relation empty label falls back to id", expandedValue{ID: "acc-1"}, "acc-1"},
		{"relation label is csvGuard-ed", expandedValue{ID: "acc-1", Label: "=HYPERLINK()"}, "'=HYPERLINK()"},
		{"user label", expandedUserValue{ID: "u-1", Label: "Nguyễn Văn A", Email: "a@example.com"}, "Nguyễn Văn A"},
		{"user without a name falls back to email", expandedUserValue{ID: "u-1", Email: "a@example.com"}, "a@example.com"},
		{"user without name or email falls back to id", expandedUserValue{ID: "u-1"}, "u-1"},
		{"field-ACL denied labelField exports the id only", expandedIDOnly{ID: "acc-1"}, "acc-1"},
	}
	for _, tc := range cases {
		if got := csvCell(tc.in); got != tc.want {
			t.Errorf("%s: csvCell(%#v) = %q; want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// The export walks ListRecordsCursor directly and never passes through List, so the label
// resolution must run per cursor page inside the streaming loop (next to stripReadFields). A
// projected relation/user column named in opts.Expand exports its label; an FK whose target no
// longer resolves keeps the stored id (the documented expand tri-state), never an error.
func TestExportCSV_RelationColumnsExportLabels(t *testing.T) {
	store := &fakeRecordStore{cursorRecords: []dynrepo.Record{
		{ID: "q1", Data: map[string]any{"name": "Báo giá 1", "account": "acc-1", "owner": "u-1"}},
		{ID: "q2", Data: map[string]any{"name": "Báo giá 2", "account": "acc-gone", "owner": ""}},
	}}
	cat := &fakeRecordCatalog{entity: courseEntityFixture(), fields: quoteFieldDefs()}
	fetcher := newFakeLabelAndUserFetcher(
		map[string]string{"acc-1": "Công ty ABC"},
		map[string]dynrepo.UserLabelResult{"u-1": {ID: "u-1", Name: "Nguyễn Văn A", Email: "a@example.com"}},
	)
	svc := NewRecordService(cat, store).WithLabelFetcher(fetcher)

	var buf bytes.Buffer
	err := svc.ExportCSV(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ExportCSVOptions{
		Columns: []string{"name", "account", "owner"},
		Expand:  []string{"account", "owner"},
	}, &buf)
	if err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	const want = "\xEF\xBB\xBFTiêu đề báo giá,Khách hàng,Người phụ trách\n" +
		"Báo giá 1,Công ty ABC,Nguyễn Văn A\n" +
		"Báo giá 2,acc-gone,\n"
	if got := buf.String(); got != want {
		t.Fatalf("export =\n%q\nwant\n%q", got, want)
	}
	if fetcher.calls["accounts"] != 1 || fetcher.calls["users"] != 1 {
		t.Errorf("fetcher calls = %v; want one batched call per target per page", fetcher.calls)
	}
}

// A `lookup` column is resolved at read time (ENG-COMPUTED #2) and List resolves it always-on,
// so a projected lookup column shows a value on screen. The export walks the cursor pages itself,
// so it has to run the SAME resolution per page — otherwise the file disagrees with the screen
// (an empty cell where the screen shows "Manufacturing"). It must run BEFORE applyExpand, which
// mutates the via-FK into an expandedValue the lookup reader cannot read.
func TestExportCSV_LookupColumnResolves(t *testing.T) {
	lookupOpts := `{"via":"account","sourceField":"industry"}`
	fields := append(quoteFieldDefs(), metadata.FieldDefinition{Name: "account_industry", Label: "Ngành", FieldType: "lookup", Options: &lookupOpts})
	store := &fakeRecordStore{cursorRecords: []dynrepo.Record{
		{ID: "q1", Data: map[string]any{"name": "Báo giá 1", "account": "acc-1"}},
	}}
	cat := &fakeRecordCatalog{entity: courseEntityFixture(), fields: fields}
	svc := NewRecordService(cat, store).
		WithLabelFetcher(newFakeLabelAndUserFetcher(map[string]string{"acc-1": "Công ty ABC"}, nil)).
		WithFieldValueFetcher(newFakeFieldValueFetcher(map[string]any{"acc-1": "Manufacturing"}))

	var buf bytes.Buffer
	err := svc.ExportCSV(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ExportCSVOptions{
		Columns: []string{"name", "account", "account_industry"},
		Expand:  []string{"account"},
	}, &buf)
	if err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	const want = "\xEF\xBB\xBFTiêu đề báo giá,Khách hàng,Ngành\nBáo giá 1,Công ty ABC,Manufacturing\n"
	if got := buf.String(); got != want {
		t.Fatalf("export =\n%q\nwant\n%q", got, want)
	}
}
