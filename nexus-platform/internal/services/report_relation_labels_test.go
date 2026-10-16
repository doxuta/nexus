package services

// REPORT-RELATION-LABELS (queue #15/#16): a tabular report whose columns include a `relation`
// or `user` field must hand the caller the human label, not the 36-char UUID the record stores.
// The list engine already resolves labels behind ListQuery.Expand (applyExpand: batch, target
// entity ACL, field-ACL on the labelField); runTabular simply never set it. These tests pin the
// on-screen half without a database; the CSV half lives in record_export_test.go and the
// field-ACL end-to-end proof in report_relation_labels_integration_test.go.

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
)

// fakeLabelAndUserFetcher resolves BOTH relation labels (LabelFetcher) and user labels
// (userLabelFetcher) from preset maps — the two halves production's *dynrepo.Repository serves —
// and counts BatchFetchLabels calls per target entity so a test can prove a report with no
// relation column never fetches.
type fakeLabelAndUserFetcher struct {
	labels map[string]string
	users  map[string]dynrepo.UserLabelResult
	calls  map[string]int
	// gotLabelFields is every labelField BatchFetchLabels was asked to read (#272 wiring proof).
	gotLabelFields []string
}

func newFakeLabelAndUserFetcher(labels map[string]string, users map[string]dynrepo.UserLabelResult) *fakeLabelAndUserFetcher {
	return &fakeLabelAndUserFetcher{labels: labels, users: users, calls: map[string]int{}}
}

func (f *fakeLabelAndUserFetcher) BatchFetchLabels(_ context.Context, targetEntity, _, labelField string, ids []string) ([]dynrepo.LabelResult, error) {
	f.calls[targetEntity]++
	f.gotLabelFields = append(f.gotLabelFields, labelField)
	out := make([]dynrepo.LabelResult, 0, len(ids))
	for _, id := range ids {
		if l, ok := f.labels[id]; ok {
			out = append(out, dynrepo.LabelResult{ID: id, Label: l})
		}
	}
	return out, nil
}

func (f *fakeLabelAndUserFetcher) BatchFetchUserLabels(_ context.Context, _ string, ids []string) ([]dynrepo.UserLabelResult, error) {
	f.calls["users"]++
	out := make([]dynrepo.UserLabelResult, 0, len(ids))
	for _, id := range ids {
		if u, ok := f.users[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

// quoteFieldDefs is quotes{name text, account relation→accounts, owner user, status picklist}
// as the catalog serves it (Options as the JSON text sys_fields stores).
func quoteFieldDefs() []metadata.FieldDefinition {
	accountOpts := `{"relatedEntity":"accounts"}`
	return []metadata.FieldDefinition{
		{Name: "name", Label: "Tiêu đề báo giá", FieldType: "text"},
		{Name: "account", Label: "Khách hàng", FieldType: "relation", Options: &accountOpts},
		{Name: "owner", Label: "Người phụ trách", FieldType: "user"},
		{Name: "status", Label: "Trạng thái", FieldType: "picklist"},
	}
}

func newRelationReportService(columns []string, rows []dynrepo.Record) (*ReportService, *fakeLabelAndUserFetcher) {
	store := &fakeRecordStore{listRecords: rows, cursorRecords: rows}
	cat := &fakeRecordCatalog{entity: courseEntityFixture(), fields: quoteFieldDefs()}
	fetcher := newFakeLabelAndUserFetcher(
		map[string]string{"acc-1": "Công ty ABC"},
		map[string]dynrepo.UserLabelResult{"u-1": {ID: "u-1", Name: "Nguyễn Văn A", Email: "a@example.com"}},
	)
	records := NewRecordService(cat, store).WithLabelFetcher(fetcher)
	cfg, _ := json.Marshal(map[string]any{"columns": columns})
	rep := report.Record{
		ID: "rep-1", TenantID: "tenant-1", Name: "Đơn hàng tháng này", EntityName: "courses",
		Type: "tabular", Config: cfg, IsActive: true,
	}
	return NewReportService(&fakeReportRepo{rec: rep}, records), fetcher
}

func TestRunTabular_ExpandsRelationAndUserColumns(t *testing.T) {
	svc, fetcher := newRelationReportService(
		[]string{"name", "account", "owner"},
		[]dynrepo.Record{{ID: "q1", Data: map[string]any{"name": "Báo giá 1", "account": "acc-1", "owner": "u-1", "status": "Nháp"}}},
	)
	res, err := svc.RunReport(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "rep-1", nil)
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if res.Tabular == nil || len(res.Tabular.Records) != 1 {
		t.Fatalf("Tabular = %+v; want 1 row", res.Tabular)
	}
	row := res.Tabular.Records[0].Data
	acc, ok := row["account"].(expandedValue)
	if !ok || acc.Label != "Công ty ABC" || acc.ID != "acc-1" {
		t.Errorf("account = %#v; want expandedValue{acc-1, Công ty ABC} — the report still prints the UUID", row["account"])
	}
	own, ok := row["owner"].(expandedUserValue)
	if !ok || own.Label != "Nguyễn Văn A" {
		t.Errorf("owner = %#v; want expandedUserValue{label: Nguyễn Văn A}", row["owner"])
	}
	// Scalars are untouched by the expand.
	if row["name"] != "Báo giá 1" {
		t.Errorf("name = %#v; want the stored text verbatim", row["name"])
	}
	if fetcher.calls["accounts"] != 1 {
		t.Errorf("BatchFetchLabels(accounts) calls = %d; want exactly 1 (batched per page)", fetcher.calls["accounts"])
	}
}

// Only PROJECTED relation/user columns are expanded: a report that does not show `account`
// must not pay for (or leak) its label resolution.
func TestRunTabular_NoRelationColumn_NoLabelFetch(t *testing.T) {
	svc, fetcher := newRelationReportService(
		[]string{"name", "status"},
		[]dynrepo.Record{{ID: "q1", Data: map[string]any{"name": "Báo giá 1", "account": "acc-1", "owner": "u-1", "status": "Nháp"}}},
	)
	res, err := svc.RunReport(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "rep-1", nil)
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if got := res.Tabular.Records[0].Data["account"]; got != "acc-1" {
		t.Errorf("non-projected account = %#v; want the raw id untouched", got)
	}
	if fetcher.calls["accounts"] != 0 || fetcher.calls["users"] != 0 {
		t.Errorf("fetcher ran for a report with no relation/user column: %v", fetcher.calls)
	}
}
