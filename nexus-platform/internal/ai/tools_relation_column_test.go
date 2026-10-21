package ai

// Issue #42 (Related): propose_field refuses a relation or user field whose `<name>_id` column the
// table already has, with Apply's own check and code (services.CheckRelationColumn), instead of
// buffering a card that turns red at Apply with FIELD_RELATION_COLUMN_CONFLICT.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeColumns stands in for *dynrepo.Repository.ColumnExists.
type fakeColumns struct {
	have []string // "<entity>.<column>"
	err  error
	read []string
}

func (f *fakeColumns) ColumnExists(_ context.Context, entity, column string) (bool, error) {
	f.read = append(f.read, entity+"."+column)
	return slices.Contains(f.have, entity+"."+column), f.err
}

func TestTools_ProposeField_RelationColumnTaken(t *testing.T) {
	taken := func() *fakeColumns { return &fakeColumns{have: []string{"tickets.owner_id", "tickets.doctor_id"}} }
	tests := []struct {
		name     string
		cols     *fakeColumns // nil = seam not wired (plain reader)
		args     string
		wantCode string // "" = proposal buffered
		wantRead []string
	}{
		{"user field on a system column", taken(),
			`{"entity":"tickets","name":"owner","label":"Chủ","fieldType":"user"}`, "FIELD_RELATION_COLUMN_CONFLICT", []string{"tickets.owner_id"}},
		{"relation next to an indexed doctor_id", taken(),
			`{"entity":"tickets","name":"doctor","label":"Bác sĩ","fieldType":"relation","options":{"relatedEntity":"patients"}}`, "FIELD_RELATION_COLUMN_CONFLICT", []string{"tickets.doctor_id"}},
		{"free column passes", taken(),
			`{"entity":"tickets","name":"assignee","label":"Người nhận","fieldType":"user"}`, "", []string{"tickets.assignee_id"}},
		{"a text field derives no column", taken(),
			`{"entity":"tickets","name":"owner_note","label":"Ghi chú","fieldType":"text"}`, "", nil},
		{"column read failure fails closed", &fakeColumns{err: errors.New("db down")},
			`{"entity":"tickets","name":"owner","label":"Chủ","fieldType":"user"}`, "VALIDATION_FAILED", []string{"tickets.owner_id"}},
		{"seam not wired keeps old behaviour", nil,
			`{"entity":"tickets","name":"owner","label":"Chủ","fieldType":"user"}`, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reader MetadataReader = editFixtureReader()
			if tt.cols != nil {
				reader = ReaderWithAccessProbes(reader, &AccessProbes{Columns: tt.cols})
			}
			e := &toolExecutor{reader: reader, tenantID: "tenant-A"}
			raw := e.execute(context.Background(), "propose_field", tt.args)
			ok, _ := parseToolResult(t, raw)
			if tt.cols != nil && !slices.Equal(tt.cols.read, tt.wantRead) {
				t.Fatalf("columns read = %v, want %v (%s)", tt.cols.read, tt.wantRead, raw)
			}
			if tt.wantCode == "" {
				if !ok || len(e.draft) != 1 {
					t.Fatalf("want a buffered proposal, got %s", raw)
				}
				return
			}
			if ok || len(e.draft) != 0 || !strings.Contains(raw, tt.wantCode) {
				t.Fatalf("want %s and no draft, got %s", tt.wantCode, raw)
			}
		})
	}

	// A table only proposed in this draft has no columns yet: nothing to read, Apply creates it first.
	cols := taken()
	e := &toolExecutor{reader: ReaderWithAccessProbes(editFixtureReader(), &AccessProbes{Columns: cols}), tenantID: "tenant-A"}
	if ok, m := parseToolResult(t, e.execute(context.Background(), "propose_entity", `{"name":"visits","label":"Lượt khám","fields":[{"name":"note","label":"Ghi chú","fieldType":"text"}]}`)); !ok {
		t.Fatalf("propose_entity: %+v", m)
	}
	if ok, m := parseToolResult(t, e.execute(context.Background(), "propose_field", `{"entity":"visits","name":"assignee","label":"Người nhận","fieldType":"user"}`)); !ok || len(cols.read) != 0 {
		t.Fatalf("a drafted table: ok=%v %+v, columns read %v; want buffered without a read", ok, m, cols.read)
	}
}
