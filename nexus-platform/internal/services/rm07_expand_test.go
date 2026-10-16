package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// fakeFetcher is a test double for LabelFetcher that records call counts per entity and
// returns a preset label map. Used to assert no N+1 (one call per unique target entity).
type fakeFetcher struct {
	// calls tracks how many times BatchFetchLabels was called per targetEntity.
	calls map[string]int
	// labels maps id → label for the mock resolution.
	labels map[string]string
}

func newFakeFetcher(labels map[string]string) *fakeFetcher {
	return &fakeFetcher{calls: make(map[string]int), labels: labels}
}

func (f *fakeFetcher) BatchFetchLabels(_ context.Context, targetEntity, _, _ string, ids []string) ([]dynrepo.LabelResult, error) {
	f.calls[targetEntity]++
	results := make([]dynrepo.LabelResult, 0, len(ids))
	for _, id := range ids {
		if label, ok := f.labels[id]; ok {
			results = append(results, dynrepo.LabelResult{ID: id, Label: label})
		}
	}
	return results, nil
}

// makeRelationSpec builds a FieldSpec for a relation field pointing to targetEntity,
// with an optional labelField override.
func makeRelationSpec(name, targetEntity, labelField string) FieldSpec {
	opts := map[string]any{"relatedEntity": targetEntity}
	if labelField != "" {
		opts["labelField"] = labelField
	}
	return FieldSpec{Name: name, FieldType: "relation", Options: opts}
}

func TestRM07_ApplyExpand_HappyPath(t *testing.T) {
	fetcher := newFakeFetcher(map[string]string{
		"acc-1": "Acme Corp",
		"acc-2": "Globex",
	})
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
	}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"account": "acc-1"}},
		{ID: "r2", Data: map[string]any{"account": "acc-2"}},
	}

	if err := applyExpand(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, []string{"account"}); err != nil {
		t.Fatalf("applyExpand error: %v", err)
	}

	// Check that the relation field was expanded to {id, label}.
	for _, rec := range records {
		v, ok := rec.Data["account"]
		if !ok {
			t.Fatalf("record %s: 'account' field missing after expand", rec.ID)
		}
		ev, ok := v.(expandedValue)
		if !ok {
			t.Fatalf("record %s: 'account' not an expandedValue, got %T: %v", rec.ID, v, v)
		}
		if ev.ID == "acc-1" && ev.Label != "Acme Corp" {
			t.Errorf("record %s: expected label 'Acme Corp', got %q", rec.ID, ev.Label)
		}
		if ev.ID == "acc-2" && ev.Label != "Globex" {
			t.Errorf("record %s: expected label 'Globex', got %q", rec.ID, ev.Label)
		}
	}
}

// TestRM07_NoBatchPerRecord verifies that N records pointing to the same target entity
// result in exactly ONE BatchFetchLabels call (not N calls).
func TestRM07_NoBatchPerRecord(t *testing.T) {
	fetcher := newFakeFetcher(map[string]string{
		"acc-1": "Acme Corp",
		"acc-2": "Globex",
		"acc-3": "Initech",
	})
	specs := []FieldSpec{
		makeRelationSpec("account", "accounts", "name"),
	}
	// Three records all expanding the same "accounts" entity.
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"account": "acc-1"}},
		{ID: "r2", Data: map[string]any{"account": "acc-2"}},
		{ID: "r3", Data: map[string]any{"account": "acc-3"}},
	}

	if err := applyExpand(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, []string{"account"}); err != nil {
		t.Fatalf("applyExpand error: %v", err)
	}

	// Must be exactly 1 call to "accounts", not 3.
	if fetcher.calls["accounts"] != 1 {
		t.Errorf("expected 1 BatchFetchLabels call for 'accounts', got %d", fetcher.calls["accounts"])
	}
}

// TestRM07_UnknownExpandField_Ignored verifies that an expand field that does not exist
// in the entity's declared fields is silently skipped (no error, no change to records).
func TestRM07_UnknownExpandField_Ignored(t *testing.T) {
	fetcher := newFakeFetcher(nil)
	specs := []FieldSpec{
		{Name: "title", FieldType: "text"},
	}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"title": "Hello"}},
	}

	if err := applyExpand(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, []string{"nonexistent"}); err != nil {
		t.Fatalf("applyExpand should not error on unknown field, got: %v", err)
	}

	// No fetcher calls should have been made.
	if len(fetcher.calls) != 0 {
		t.Errorf("expected 0 fetcher calls for unknown field, got %v", fetcher.calls)
	}

	// Records must be unchanged.
	if records[0].Data["title"] != "Hello" {
		t.Error("record data was modified unexpectedly")
	}
}

// TestRM07_DefaultLabelField verifies that when labelField is absent from options, "name"
// is used as the default label field.
func TestRM07_DefaultLabelField(t *testing.T) {
	fetcher := newFakeFetcher(map[string]string{"contact-1": "Alice"})
	// No labelField in options → should default to "name".
	specs := []FieldSpec{
		makeRelationSpec("contact", "contacts", ""), // empty labelField → default "name"
	}
	records := []dynrepo.Record{
		{ID: "r1", Data: map[string]any{"contact": "contact-1"}},
	}

	if err := applyExpand(context.Background(), fetcher, nil, nil, RecordScope{}, "tenant-1", records, specs, []string{"contact"}); err != nil {
		t.Fatalf("applyExpand error: %v", err)
	}

	ev, ok := records[0].Data["contact"].(expandedValue)
	if !ok {
		t.Fatalf("expected expandedValue, got %T", records[0].Data["contact"])
	}
	if ev.Label != "Alice" {
		t.Errorf("expected label 'Alice', got %q", ev.Label)
	}
}
