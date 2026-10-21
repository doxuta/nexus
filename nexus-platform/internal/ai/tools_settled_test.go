package ai

// Issue #42: an MCP thread replays only the cards still pending, so what an APPLIED card left behind
// must be read where Apply reads it. A records card's "$ref" lives in the record engine's registry
// (AccessProbes.Refs); a field applied then deleted still holds its name (ListFieldsIncludingDeleted).

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"nexus/platform/internal/metadata"
)

// fakeRefs stands in for *services.RecordService.RefEntity: token -> entity of the created row.
type fakeRefs struct {
	have    map[string]string
	tenants []string
}

func (f *fakeRefs) RefEntity(tenantID, token string) (string, bool) {
	f.tenants = append(f.tenants, tenantID)
	e, ok := f.have[token]
	return e, ok
}

func TestTools_ProposeRecords_RefOfAnAppliedCard(t *testing.T) {
	applied := map[string]string{"@sess-1:bn1": "patients"} // the parent card was applied: its row is registered
	tests := []struct {
		name     string
		refs     *fakeRefs // nil = seam not wired
		args     string
		wantCode string // "" = buffered
	}{
		{"an applied parent's ref resolves", &fakeRefs{have: applied},
			`{"entity":"tickets","rows":[{"subject":"Khám","patient":"@bn1"}]}`, ""},
		{"a polymorphic id names the applied parent's entity", &fakeRefs{have: applied},
			`{"entity":"tickets","rows":[{"subject":"Gọi lại","related_to":{"type":"patients","id":"@bn1"}}]}`, ""},
		{"a polymorphic id with the wrong type", &fakeRefs{have: applied},
			`{"entity":"tickets","rows":[{"subject":"Gọi lại","related_to":{"type":"tickets","id":"@bn1"}}]}`, "RECORD_REF_UNRESOLVED"},
		{"a ref the registry does not hold (discarded, expired, never applied)", &fakeRefs{have: applied},
			`{"entity":"tickets","rows":[{"subject":"Khám","patient":"@bn2"}]}`, "RECORD_REF_UNRESOLVED"},
		{"seam not wired: only cards in the draft resolve", nil,
			`{"entity":"tickets","rows":[{"subject":"Khám","patient":"@bn1"}]}`, "RECORD_REF_UNRESOLVED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reader MetadataReader = polyRefReader()
			if tt.refs != nil {
				reader = ReaderWithAccessProbes(reader, &AccessProbes{Refs: tt.refs})
			}
			e := &toolExecutor{reader: reader, tenantID: "tenant-A", turnTag: "sess-1"}
			raw := e.execute(context.Background(), "propose_records", tt.args)
			ok, _ := parseToolResult(t, raw)
			if tt.wantCode == "" {
				if !ok || len(e.draft) != 1 {
					t.Fatalf("want buffered, got %s", raw)
				}
				if got := e.draft[0].Records.Rows[0]; got["patient"] != nil && got["patient"] != "@sess-1:bn1" {
					t.Fatalf("the ref must be the mangled token Apply resolves, got %v", got["patient"])
				}
			} else if ok || !strings.Contains(raw, tt.wantCode) {
				t.Fatalf("want %s, got %s", tt.wantCode, raw)
			}
			if tt.refs != nil {
				for _, ten := range tt.refs.tenants {
					if ten != "tenant-A" {
						t.Fatalf("registry read for tenant %q; want the executor's tenant", ten)
					}
				}
			}
		})
	}
}

func TestTools_ProposeField_NameOfADeletedField(t *testing.T) {
	r := editFixtureReader()
	r.deleted = map[string]map[string][]metadata.FieldDefinition{"tenant-A": {"eA": {
		{ID: "f-web", Name: "website", FieldType: "text", DeletedAt: gorm.DeletedAt(sql.NullTime{Time: time.Now(), Valid: true})},
	}}}
	e := &toolExecutor{reader: r, tenantID: "tenant-A"}
	raw := e.execute(context.Background(), "propose_field", `{"entity":"patients","name":"website","label":"Web","fieldType":"text"}`)
	if ok, _ := parseToolResult(t, raw); ok || !strings.Contains(raw, "FIELD_ALREADY_EXISTS") || !strings.Contains(raw, "xoá") {
		t.Fatalf("a deleted field's name: %s; want FIELD_ALREADY_EXISTS saying it was deleted", raw)
	}
	if ok, m := parseToolResult(t, e.execute(context.Background(), "propose_field", `{"entity":"patients","name":"homepage","label":"Web","fieldType":"text"}`)); !ok {
		t.Fatalf("a free name: %+v", m)
	}
}
