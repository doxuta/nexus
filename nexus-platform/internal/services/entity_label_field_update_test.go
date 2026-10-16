package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// updMeta embeds the writer interface so only the three calls UpdateEntity makes need bodies.
type updMeta struct {
	MetadataWriter
	fields  []metadata.FieldDefinition
	updated *metadata.EntityDefinition
}

func (m *updMeta) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	return &metadata.EntityDefinition{ID: "e1", Name: name, Label: "Bệnh nhân"}, nil
}
func (m *updMeta) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return m.fields, nil
}
func (m *updMeta) UpdateEntityMutable(_ context.Context, _ string, e *metadata.EntityDefinition) error {
	m.updated = e
	return nil
}

func TestUpdateEntity_LabelFieldOption(t *testing.T) {
	fields := []metadata.FieldDefinition{
		{Name: "ho_ten", FieldType: "text"}, {Name: "ngay", FieldType: "date"},
	}
	for _, tc := range []struct {
		name    string
		options map[string]any
		wantErr bool
		stored  string // expected options.labelField after save ("" = not checked)
	}{
		{"valid choice saved", map[string]any{"labelField": "ho_ten"}, false, "ho_ten"},
		{"padded name stored trimmed", map[string]any{"labelField": " ho_ten "}, false, "ho_ten"},
		{"unknown field refused", map[string]any{"labelField": "ghost"}, true, ""},
		{"date field refused", map[string]any{"labelField": "ngay"}, true, ""},
		{"non-string refused", map[string]any{"labelField": 7}, true, ""},
		{"options without labelField untouched", map[string]any{"icon": "user"}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &updMeta{fields: fields}
			svc := NewEntityService(nil, m)
			_, err := svc.UpdateEntity(context.Background(), "t1", "benh_nhan", UpdateEntityInput{Options: tc.options})
			var ve *ValidationError
			if tc.wantErr {
				if !errors.As(err, &ve) || ve.Code != "ENTITY_LABEL_FIELD_INVALID" || m.updated != nil {
					t.Fatalf("want ENTITY_LABEL_FIELD_INVALID and no write, got err=%v updated=%v", err, m.updated)
				}
				return
			}
			if err != nil || m.updated == nil {
				t.Fatalf("want saved, got err=%v updated=%v", err, m.updated)
			}
			if tc.stored != "" {
				if lf, _ := metadata.ResolveLabelField(m.updated.Options, fields); lf != tc.stored || !strings.Contains(*m.updated.Options, `"`+tc.stored+`"`) {
					t.Errorf("stored options %s resolve to %q; want %q", *m.updated.Options, lf, tc.stored)
				}
			}
		})
	}
}
