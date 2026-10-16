package services

import (
	"encoding/json"
	"errors"
	"testing"

	"nexus/platform/internal/metaversion"
)

// specs/063 S7-10 (T136, T136b): the rollback planner lists the fields / tables a published delete
// removed as schemaRestores, carries the recorded Meta to the adapter, and never plans a restore it
// cannot do.

const (
	fieldDocZ = `{"entityId":"e1","label":"Z","isRequired":false,"defaultValue":"","options":null,"fieldOrder":3,"description":""}`
	entDocW   = `{"name":"w","label":"W","description":""}`
	fieldMeta = `{"sweptAt":"2026-10-14T09:00:00Z","fieldDef":{"id":"z1","name":"z","entity_id":"e1","is_unique":1}}`
	entMeta   = `{"sweptAt":"2026-10-14T09:00:00Z","sweptCount":7,"entityDef":{"id":"w1","name":"w"},"relationships":[{"id":"r1","name":"w_rel","join_table_name":null}]}`
)

// rbInput's target version was written 2026-10-01T09:00:00Z.
func bornMeta(createdAt string) string {
	return `{"sweptAt":"2026-10-14T09:00:00Z","sweptCount":7,"entityDef":{"id":"w1","name":"w","label":"W","description":"","created_at":"` + createdAt + `"}}`
}

func bornFieldMeta(createdAt string) string {
	return `{"sweptAt":"2026-10-14T09:00:00Z","fieldDef":{"id":"z1","name":"z","entity_id":"e1","label":"Z","is_required":0,"field_order":30,"options":"{\"indexed\":true}","created_at":"` + createdAt + `"}}`
}

func schemaInput(live rbLive, target, after []metaversion.HistoryDoc, meta map[string]string) rollbackInput {
	in := rbInput(live, target, after, DocTypeField, DocTypeEntity)
	in.Types = []string{DocTypeField, DocTypeEntity}
	in.RestoreMeta = func(docType, key string) (json.RawMessage, bool, error) {
		m, ok := meta[docType+"|"+key]
		return json.RawMessage(m), ok, nil
	}
	return in
}

func TestPlanRollback_SchemaRestores(t *testing.T) {
	tests := []struct {
		name         string
		live         rbLive
		target       []metaversion.HistoryDoc
		after        []metaversion.HistoryDoc
		meta         map[string]string
		wantChanges  map[string]string
		wantRestores int
		wantKept     int
		wantCount    *int64
	}{
		{"deleted field comes back with its meta", rbLive{},
			[]metaversion.HistoryDoc{hd("field", "z1", "snapshot", "baseline", 1, fieldDocZ)},
			[]metaversion.HistoryDoc{hd("field", "z1", "delete", "publish", 3, "")},
			map[string]string{"field|z1": fieldMeta}, map[string]string{"field|z1": "create"}, 1, 0, nil},
		{"deleted table lists how many records come back", rbLive{},
			[]metaversion.HistoryDoc{hd("entity", "w1", "snapshot", "baseline", 1, entDocW)},
			[]metaversion.HistoryDoc{hd("entity", "w1", "delete", "publish", 3, "")},
			map[string]string{"entity|w1": entMeta}, map[string]string{"entity|w1": "create"}, 1, 0, ptr64(7)},
		{"delete without a recorded meta cannot be undone: kept as is", rbLive{},
			[]metaversion.HistoryDoc{hd("field", "z1", "snapshot", "baseline", 1, fieldDocZ)},
			[]metaversion.HistoryDoc{hd("field", "z1", "delete", "publish", 3, "")},
			map[string]string{}, map[string]string{}, 0, 1, nil},
		{"table meta without a sweep instant cannot be undone", rbLive{},
			[]metaversion.HistoryDoc{hd("entity", "w1", "snapshot", "baseline", 1, entDocW)},
			[]metaversion.HistoryDoc{hd("entity", "w1", "delete", "publish", 3, "")},
			map[string]string{"entity|w1": `{"entityDef":{"id":"w1","name":"w"}}`}, map[string]string{}, 0, 1, nil},
		{"table created live after the baseline (no history doc) and born before N comes back", rbLive{},
			nil,
			[]metaversion.HistoryDoc{hd("entity", "w1", "delete", "publish", 3, "")},
			map[string]string{"entity|w1": bornMeta("2026-09-30T08:00:00Z")}, map[string]string{"entity|w1": "create"}, 1, 0, ptr64(7)},
		{"field created live after the baseline and born before N comes back", rbLive{},
			nil,
			[]metaversion.HistoryDoc{hd("field", "z1", "delete", "publish", 3, "")},
			map[string]string{"field|z1": bornFieldMeta("2026-09-30 08:00:00")}, map[string]string{"field|z1": "create"}, 1, 0, nil},
		{"field born before N, edited through a draft, then deleted: history [update, delete] still comes back", rbLive{},
			nil,
			[]metaversion.HistoryDoc{hd("field", "z1", "update", "publish", 2, fieldDocZ), hd("field", "z1", "delete", "publish", 3, "")},
			map[string]string{"field|z1": bornFieldMeta("2026-09-30 08:00:00")}, map[string]string{"field|z1": "create"}, 1, 0, nil},
		{"a table born AFTER N is not restored by a rollback to N", rbLive{},
			nil,
			[]metaversion.HistoryDoc{hd("entity", "w1", "delete", "publish", 3, "")},
			map[string]string{"entity|w1": bornMeta("2026-10-02T08:00:00Z")}, map[string]string{}, 0, 0, nil},
		{"a delete whose definition has no creation time is left alone", rbLive{},
			nil,
			[]metaversion.HistoryDoc{hd("field", "z1", "delete", "publish", 3, "")},
			map[string]string{"field|z1": fieldMeta}, map[string]string{}, 0, 0, nil},
		{"a field edited after N is not rolled back (only deletes are)", rbLive{"field|z1": json.RawMessage(`{"entityId":"e1","label":"Z2","isRequired":false,"defaultValue":"","options":null,"fieldOrder":3,"description":""}`)},
			[]metaversion.HistoryDoc{hd("field", "z1", "snapshot", "baseline", 1, fieldDocZ)},
			[]metaversion.HistoryDoc{hd("field", "z1", "update", "publish", 3, "")},
			map[string]string{}, map[string]string{}, 0, 0, nil},
		{"a field that is alive and equal plans nothing", rbLive{"field|z1": json.RawMessage(fieldDocZ)},
			[]metaversion.HistoryDoc{hd("field", "z1", "snapshot", "baseline", 1, fieldDocZ)},
			nil, map[string]string{}, map[string]string{}, 0, 0, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := planRollback(schemaInput(tc.live, tc.target, tc.after, tc.meta))
			if err != nil {
				t.Fatal(err)
			}
			got := opsOf(p)
			if len(got) != len(tc.wantChanges) {
				t.Fatalf("changes = %v, want %v", got, tc.wantChanges)
			}
			for k, v := range tc.wantChanges {
				if got[k] != v {
					t.Fatalf("changes = %v, want %v", got, tc.wantChanges)
				}
			}
			if len(p.View.SchemaRestores) != tc.wantRestores || len(p.View.KeptAsIs) != tc.wantKept {
				t.Fatalf("schemaRestores=%+v keptAsIs=%+v, want %d / %d", p.View.SchemaRestores, p.View.KeptAsIs, tc.wantRestores, tc.wantKept)
			}
			for _, c := range p.Changes {
				if len(c.Restore) == 0 {
					t.Errorf("restore change %s/%s carries no meta", c.DocType, c.DocKey)
				}
			}
			if tc.wantRestores == 1 {
				r := p.View.SchemaRestores[0]
				if (tc.wantCount == nil) != (r.SweptCount == nil) || (tc.wantCount != nil && *tc.wantCount != *r.SweptCount) {
					t.Errorf("sweptCount = %v, want %v", r.SweptCount, tc.wantCount)
				}
			}
		})
	}
}

func TestPlanRollback_SchemaRestoreMetaError(t *testing.T) {
	in := schemaInput(rbLive{}, []metaversion.HistoryDoc{hd("field", "z1", "snapshot", "baseline", 1, fieldDocZ)}, nil, nil)
	in.RestoreMeta = func(string, string) (json.RawMessage, bool, error) { return nil, false, errors.New("db down") }
	if _, err := planRollback(in); err == nil {
		t.Fatal("a failed meta read must fail the plan, not silently skip the restore")
	}
}

func TestRollbackPlanView_TokenMovesWithSchemaRestores(t *testing.T) {
	a := RollbackPlanView{SchemaRestores: []RollbackSchemaRestore{}}
	b := RollbackPlanView{SchemaRestores: []RollbackSchemaRestore{{DocType: "entity", DocKey: "w1", SweptCount: ptr64(7)}}}
	c := RollbackPlanView{SchemaRestores: []RollbackSchemaRestore{{DocType: "entity", DocKey: "w1", SweptCount: ptr64(8)}}}
	if a.token() == b.token() || b.token() == c.token() {
		t.Fatal("token must change when a restore appears or its record count moves")
	}
}

func TestSchemaRestoreMeta_Relationships(t *testing.T) {
	m, err := parseRestoreMeta(json.RawMessage(`{"relationships":[{"id":"a","name":"n1","join_table_name":"j"},{"id":"","name":"x"},{"id":"b","name":"n2","join_table_name":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	rels := m.relationships()
	if len(rels) != 2 || rels[0].JoinTableName == nil || *rels[0].JoinTableName != "j" || rels[1].JoinTableName != nil {
		t.Fatalf("relationships = %+v", rels)
	}
	for in, want := range map[any]bool{int64(1): true, float64(0): false, "1": true, nil: false, true: true} {
		if truthy(in) != want {
			t.Errorf("truthy(%v) = %v", in, !want)
		}
	}
}

func ptr64(n int64) *int64 { return &n }
