package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// USER-FIELD slice: ?expand= for a `user` field resolves {id, label, email} (expandedUserValue)
// via a SEPARATE fetch against the fixed `users` table (userLabelFetcher), mirroring RM-07's
// relation expand (rm07_expand_test.go) shape-for-shape.

// fakeUserExpandFetcher implements BOTH LabelFetcher (the applyExpand param type — a no-op stub
// here, since these tests only exercise the `user` branch) and userLabelFetcher (the interface
// applyExpand type-asserts for `user` fields), tracking call counts per field for the
// anti-N+1 assertion.
type fakeUserExpandFetcher struct {
	calls  int // BatchFetchUserLabels call count (anti-N+1: 1 call per user field, not per record)
	labels map[string]dynrepo.UserLabelResult
}

func (f *fakeUserExpandFetcher) BatchFetchLabels(_ context.Context, _, _, _ string, _ []string) ([]dynrepo.LabelResult, error) {
	return nil, nil // not exercised by these tests
}

func (f *fakeUserExpandFetcher) BatchFetchUserLabels(_ context.Context, _ string, ids []string) ([]dynrepo.UserLabelResult, error) {
	f.calls++
	results := make([]dynrepo.UserLabelResult, 0, len(ids))
	for _, id := range ids {
		if u, ok := f.labels[id]; ok {
			results = append(results, u)
		}
	}
	return results, nil
}

func makeUserFieldSpec(name string) FieldSpec {
	return FieldSpec{Name: name, FieldType: "user"}
}

// TestApplyExpand_UserField_HappyPath proves a `user` field expands to {id, label, email}.
func TestApplyExpand_UserField_HappyPath(t *testing.T) {
	fetcher := &fakeUserExpandFetcher{labels: map[string]dynrepo.UserLabelResult{
		"u-1": {ID: "u-1", Name: "Nguyễn Văn A", Email: "a@example.com"},
	}}
	specs := []FieldSpec{makeUserFieldSpec("assigned_to")}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"assigned_to": "u-1"}},
	}

	if err := applyExpand(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, []string{"assigned_to"}); err != nil {
		t.Fatalf("applyExpand error: %v", err)
	}

	v, ok := records[0].Data["assigned_to"]
	if !ok {
		t.Fatal("assigned_to field missing after expand")
	}
	uv, ok := v.(expandedUserValue)
	if !ok {
		t.Fatalf("assigned_to not an expandedUserValue, got %T: %v", v, v)
	}
	if uv.ID != "u-1" || uv.Label != "Nguyễn Văn A" || uv.Email != "a@example.com" {
		t.Errorf("expandedUserValue = %+v; want {u-1 Nguyễn Văn A a@example.com}", uv)
	}
}

// TestApplyExpand_UserField_NoBatchPerRecord proves N records assigned to users resolve in
// exactly ONE BatchFetchUserLabels call (RM-11-style batching, no N+1).
func TestApplyExpand_UserField_NoBatchPerRecord(t *testing.T) {
	fetcher := &fakeUserExpandFetcher{labels: map[string]dynrepo.UserLabelResult{
		"u-1": {ID: "u-1", Name: "A", Email: "a@example.com"},
		"u-2": {ID: "u-2", Name: "B", Email: "b@example.com"},
		"u-3": {ID: "u-3", Name: "C", Email: "c@example.com"},
	}}
	specs := []FieldSpec{makeUserFieldSpec("assigned_to")}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"assigned_to": "u-1"}},
		{ID: "r2", Data: map[string]any{"assigned_to": "u-2"}},
		{ID: "r3", Data: map[string]any{"assigned_to": "u-3"}},
	}

	if err := applyExpand(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, []string{"assigned_to"}); err != nil {
		t.Fatalf("applyExpand error: %v", err)
	}
	if fetcher.calls != 1 {
		t.Errorf("BatchFetchUserLabels calls = %d; want 1 (batched, not per-record)", fetcher.calls)
	}
}

// TestApplyExpand_UserField_UnresolvedIDLeftRaw proves a dangling/cross-tenant id (absent from
// the fetch result) leaves the raw id string untouched — same contract as the relation branch.
func TestApplyExpand_UserField_UnresolvedIDLeftRaw(t *testing.T) {
	fetcher := &fakeUserExpandFetcher{labels: map[string]dynrepo.UserLabelResult{}}
	specs := []FieldSpec{makeUserFieldSpec("assigned_to")}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"assigned_to": "ghost"}},
	}

	if err := applyExpand(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, []string{"assigned_to"}); err != nil {
		t.Fatalf("applyExpand error: %v", err)
	}
	if v := records[0].Data["assigned_to"]; v != "ghost" {
		t.Errorf("unresolved assigned_to = %v (%T); want raw string \"ghost\" untouched", v, v)
	}
}

// TestApplyExpand_UserField_FetcherWithoutUserSupport_SilentlySkipped proves a fetcher that
// does NOT implement userLabelFetcher (a legacy LabelFetcher-only fake) leaves `user` fields
// unresolved — mirrors an unknown expand field name (forward-compat, never an error).
type fakeLabelOnlyFetcher struct{}

func (f *fakeLabelOnlyFetcher) BatchFetchLabels(_ context.Context, _, _, _ string, _ []string) ([]dynrepo.LabelResult, error) {
	return nil, nil
}

func TestApplyExpand_UserField_FetcherWithoutUserSupport_SilentlySkipped(t *testing.T) {
	fetcher := &fakeLabelOnlyFetcher{}
	specs := []FieldSpec{makeUserFieldSpec("assigned_to")}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"assigned_to": "u-1"}},
	}

	if err := applyExpand(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, []string{"assigned_to"}); err != nil {
		t.Fatalf("applyExpand error: %v", err)
	}
	if v := records[0].Data["assigned_to"]; v != "u-1" {
		t.Errorf("assigned_to = %v; want raw id left untouched (fetcher has no user-label support)", v)
	}
}
