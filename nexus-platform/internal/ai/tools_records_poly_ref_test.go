package ai

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// polyRefReader extends editFixtureReader: tickets gains a polymorphic_relation "related_to"
// ("liên quan tới") that may point at a patient OR another ticket.
func polyRefReader() *fakeMetaReader {
	r := editFixtureReader()
	r.fields["tenant-A"]["eT"] = append(r.fields["tenant-A"]["eT"], metadata.FieldDefinition{
		ID: "f-rel", Name: "related_to", Label: "Liên quan tới", FieldType: "polymorphic_relation",
		Options: strPtr(`{"targetEntities":["patients","tickets"]}`),
	})
	return r
}

// TestTools_ProposeRecords_PolymorphicRef (queue #140): a polymorphic_relation value {type, id}
// may carry "@name" in its id, exactly like a relation value — it resolves only against a "$ref"
// declared by an EARLIER propose_records call this turn, is mangled to the same turn-scoped token,
// and takes part in the same-call sibling / cycle refusal. Before, the id was never inspected: a
// "@name" id buffered raw (unmangled) and could never match anything at Apply.
func TestTools_ProposeRecords_PolymorphicRef(t *testing.T) {
	const parent = `{"entity":"patients","rows":[{"full_name":"Nguyễn Văn An","$ref":"@bn1"}]}`
	tests := []struct {
		name     string
		first    string // an earlier propose_records call this turn ("" = none)
		args     string
		wantCode string // "" = must buffer
		wantMsg  string
	}{
		{
			name:  "earlier call's ref resolves and is mangled in the nested id",
			first: parent,
			args:  `{"entity":"tickets","rows":[{"subject":"Gọi lại","related_to":{"type":"patients","id":"@bn1"}}]}`,
		},
		{
			name:     "dangling ref refused",
			args:     `{"entity":"tickets","rows":[{"subject":"Gọi lại","related_to":{"type":"patients","id":"@nope"}}]}`,
			wantCode: "RECORD_REF_UNRESOLVED",
		},
		{
			name:     "ref of another entity than {type} refused, naming the real entity",
			first:    parent,
			args:     `{"entity":"tickets","rows":[{"subject":"Gọi lại","related_to":{"type":"tickets","id":"@bn1"}}]}`,
			wantCode: "RECORD_REF_UNRESOLVED",
			wantMsg:  `"patients"`,
		},
		{
			name: "same-call sibling refused",
			args: `{"entity":"tickets","rows":[
				{"subject":"T1","$ref":"@t1"},
				{"subject":"T2","related_to":{"type":"tickets","id":"@t1"}}]}`,
			wantCode: "RECORD_REF_UNRESOLVED",
			wantMsg:  "dòng 2",
		},
		{
			name: "cycle through polymorphic ids refused",
			args: `{"entity":"tickets","rows":[
				{"subject":"T1","$ref":"@t1","related_to":{"type":"tickets","id":"@t2"}},
				{"subject":"T2","$ref":"@t2","related_to":{"type":"tickets","id":"@t1"}}]}`,
			wantCode: "RECORD_REF_CYCLE",
		},
		{
			name: "cycle mixing a relation value and a polymorphic id refused",
			args: `{"entity":"tickets","rows":[
				{"subject":"T1","$ref":"@t1","patient":"@t2"},
				{"subject":"T2","$ref":"@t2","related_to":{"type":"tickets","id":"@t1"}}]}`,
			wantCode: "RECORD_REF_CYCLE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &toolExecutor{reader: polyRefReader(), tenantID: "tenant-A", turnTag: "turn1"}
			if tc.first != "" {
				mustBuffer(t, e, "propose_records", tc.first)
			}
			before := len(e.draft)
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_records", tc.args))
			if tc.wantCode != "" {
				errStr, _ := m["error"].(string)
				if ok || !strings.HasPrefix(errStr, tc.wantCode) || !strings.Contains(errStr, tc.wantMsg) {
					t.Fatalf("want %s (containing %q), got ok=%v %q", tc.wantCode, tc.wantMsg, ok, errStr)
				}
				if len(e.draft) != before {
					t.Fatalf("rejected call must not buffer: %+v", e.draft)
				}
				return
			}
			if !ok {
				t.Fatalf("want buffered, got %+v", m)
			}
			declared, _ := e.draft[0].Records.Rows[0]["$ref"].(string)
			rel, _ := e.draft[1].Records.Rows[0]["related_to"].(map[string]any)
			if declared != "@turn1:bn1" || rel["id"] != declared || rel["type"] != "patients" {
				t.Fatalf("related_to = %+v; want {type:patients, id:%q}", rel, declared)
			}
		})
	}
}
