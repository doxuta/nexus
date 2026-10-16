package services

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
)

// labelCatalog serves one entity per name with its options + fields.
type labelCatalog struct {
	opts   map[string]string
	fields map[string][]metadata.FieldDefinition
}

func (c labelCatalog) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	e := &metadata.EntityDefinition{ID: name, Name: name}
	if o, ok := c.opts[name]; ok {
		e.Options = &o
	}
	return e, nil
}
func (c labelCatalog) ListFields(_ context.Context, _, id string) ([]metadata.FieldDefinition, error) {
	return c.fields[id], nil
}

func TestWithTargetLabelFields(t *testing.T) {
	// patients has no "name"; the VN fields name the record. appointments' relation carries no labelField.
	cat := labelCatalog{
		opts: map[string]string{"benh_nhan": `{"labelField":"email"}`},
		fields: map[string][]metadata.FieldDefinition{
			"benh_nhan": {
				{Name: "ho_ten", FieldType: "text", IsRequired: true, FieldOrder: 1},
				{Name: "email", FieldType: "email", FieldOrder: 2},
			},
			"khach": {{Name: "ten_khach", FieldType: "text", IsRequired: true, FieldOrder: 1}},
		},
	}
	specs := []FieldSpec{
		makeRelationSpec("bn", "benh_nhan", "ho_ten"), // relation choice loses to the table's explicit choice
		makeRelationSpec("kh", "khach", ""),           // nothing chosen -> derived default, not "name"
		makeRelationSpec("pk", "khach", "ten_khach"),
		makeRelationSpec("other", "khach", ""), // not in expand -> untouched
	}
	got := withTargetLabelFields(context.Background(), cat, "t1", specs, []string{"bn", "kh", "pk"})
	want := map[string]string{"bn": "email", "kh": "ten_khach", "pk": "ten_khach"}
	for _, s := range got {
		if w, ok := want[s.Name]; ok {
			if lf, _ := s.Options["labelField"].(string); lf != w {
				t.Errorf("%s labelField = %q want %q", s.Name, lf, w)
			}
		}
	}
	if _, set := got[3].Options["labelField"]; set {
		t.Error("spec outside expand must not be touched")
	}
	if lf, _ := specs[0].Options["labelField"].(string); lf != "ho_ten" {
		t.Error("input specs (shared/cached) must not be mutated")
	}
}

func TestWithTargetLabelFields_NilCatalogIsNoop(t *testing.T) {
	specs := []FieldSpec{makeRelationSpec("kh", "khach", "")}
	got := withTargetLabelFields(context.Background(), nil, "t1", specs, []string{"kh"})
	if _, set := got[0].Options["labelField"]; set {
		t.Error("nil catalog must keep pre-fix behaviour")
	}
}
