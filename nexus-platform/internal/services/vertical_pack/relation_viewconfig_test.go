package vertical_pack

// DAILY-QUEUE #720 (3/4 of #295): a relationship's related-list view config travels with the pack.

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

type fakeRelRd struct {
	rels []metadata.RelationshipDefinition
}

func (f fakeRelRd) ListAllRelationships(context.Context, string) ([]metadata.RelationshipDefinition, error) {
	return f.rels, nil
}

type fakeRelView struct {
	set map[string]string
	err error
}

func (f *fakeRelView) SetViewConfig(_ context.Context, _, relID string, raw json.RawMessage) (map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.set[relID] = string(raw)
	return nil, nil
}

func TestExport_NNRelationshipCarriesViewConfig(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	for _, n := range []string{"rv_a", "rv_b"} {
		if _, _, err := meta.CreateEntity(ctx, "t1", services.CreateEntityInput{Name: n, Label: n}); err != nil {
			t.Fatal(err)
		}
	}
	vc := `{"label":"Lớp","columns":["name"]}`
	rd := fakeRelRd{rels: []metadata.RelationshipDefinition{
		{ID: "r1", Name: "rv_a_rv_b", RelationshipType: "N-N", ParentEntityName: "rv_a", ChildEntityName: "rv_b", ViewConfig: &vc},
	}}
	pack, _, err := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}, RelationshipReader: rd}).
		Export(ctx, "t1", []string{"rv_a", "rv_b"}, false, nil)
	if err != nil || len(pack.NNRelationships) != 1 {
		t.Fatalf("export: %v %+v", err, pack)
	}
	if got := string(pack.NNRelationships[0].ViewConfig); got != vc {
		t.Fatalf("exported viewConfig = %q, want %q", got, vc)
	}
}

func TestApplyRelationViewConfigs(t *testing.T) {
	existing := `{"label":"tuned"}`
	rd := fakeRelRd{rels: []metadata.RelationshipDefinition{
		{ID: "id-1n", Name: "child_parent"},
		{ID: "id-nn", Name: "nn_rel"},
		{ID: "id-tuned", Name: "child_other", ViewConfig: &existing},
	}}
	pack := &Pack{
		Relationships: []PackRelationship{
			{Name: "parent", ChildEntity: "child", ViewConfig: json.RawMessage(`{"label":"A"}`)},
			{Name: "other", ChildEntity: "child", ViewConfig: json.RawMessage(`{"label":"B"}`)},
			{Name: "none", ChildEntity: "child"},
		},
		NNRelationships: []PackNNRelationship{{Name: "nn_rel", ViewConfig: json.RawMessage(`{"label":"C"}`)}},
	}
	w := &fakeRelView{set: map[string]string{}}
	s := NewService(Deps{RelationshipReader: rd, RelationViewConfigWriter: w})
	res := &InstallResult{}
	if err := s.applyRelationViewConfigs(context.Background(), "t1", pack, res); err != nil {
		t.Fatal(err)
	}
	if w.set["id-1n"] != `{"label":"A"}` || w.set["id-nn"] != `{"label":"C"}` {
		t.Fatalf("applied = %v", w.set)
	}
	if _, touched := w.set["id-tuned"]; touched {
		t.Fatalf("overwrote a config the customer already tuned: %v", w.set)
	}

	// a validation failure is a warning, not an install failure
	w2 := &fakeRelView{set: map[string]string{}, err: &services.ValidationError{Code: "RELVIEW_COLUMN_UNKNOWN", Msg: "x"}}
	res2 := &InstallResult{}
	if err := NewService(Deps{RelationshipReader: rd, RelationViewConfigWriter: w2}).applyRelationViewConfigs(context.Background(), "t1", pack, res2); err != nil {
		t.Fatalf("validation error must not fail install: %v", err)
	}
	if len(res2.Warnings) != 2 {
		t.Fatalf("warnings = %v, want 2", res2.Warnings)
	}
}
