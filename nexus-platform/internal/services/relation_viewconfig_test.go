package services

// Unit tests for ENG-RELVIEW — RelationLifecycleService.SetViewConfig over the same
// in-memory fakes as relation_lifecycle_test.go. The DB-backed round-trip (PUT → stored
// JSON → GET /metadata/relationships) lives in handlers/eng_relview_integration_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"nexus/platform/internal/metadata"
)

// UpdateRelationshipViewConfig records the write so tests can assert both the target row
// and the exact canonical JSON (or nil = clear) the service persisted. Defined here (not in
// relation_lifecycle_test.go) because only the ENG-RELVIEW tests inspect it — same package,
// same fake.
func (f *fakeLifecycleMeta) UpdateRelationshipViewConfig(_ context.Context, tenantID, relID string, viewConfig *string) error {
	if _, ok := f.rels[tenantID+"/"+relID]; !ok {
		return metadata.ErrRelationshipNotFound
	}
	f.viewConfigWrites = append(f.viewConfigWrites, viewConfigWrite{relID: relID, config: viewConfig})
	return nil
}

type viewConfigWrite struct {
	relID  string
	config *string
}

// newViewConfigFixture seeds one 1-N relationship customers ← orders.customer plus the
// child entity's fields: name (plain text), code (unique), status (D1 indexed), customer
// (relation). Sortable per buildSortAllowlist parity: code, status, customer (+ system cols).
func newViewConfigFixture() (*fakeLifecycleMeta, *RelationLifecycleService) {
	meta := &fakeLifecycleMeta{
		rels: map[string]*metadata.RelationshipDefinition{
			"t1/rel-1": {
				ID: "rel-1", TenantID: "t1", Name: "orders_customer",
				ParentEntityName: "customers", ChildEntityName: "orders",
				RelationshipType: "1-N", ChildFKColumn: "customer_id",
			},
		},
		entities: map[string]*metadata.EntityDefinition{
			"t1/orders": {ID: "ent-orders", TenantID: "t1", Name: "orders"},
		},
		fields: map[string][]metadata.FieldDefinition{
			"t1/ent-orders": {
				{Name: "name", FieldType: "text"},
				{Name: "code", FieldType: "text", IsUnique: true},
				{Name: "status", FieldType: "picklist", Options: strPtr(`{"indexed": true}`)},
				{Name: "customer", FieldType: "relation", Options: strPtr(`{"relatedEntity":"customers"}`)},
			},
		},
	}
	svc := NewRelationLifecycleService(meta, &fakeImpactCounts{}, &fakeFieldDeleter{})
	return meta, svc
}

func TestSetViewConfig_ValidPersistsCanonicalJSON(t *testing.T) {
	meta, svc := newViewConfigFixture()

	cfg, err := svc.SetViewConfig(context.Background(), "t1", "rel-1", json.RawMessage(
		`{"label":"Đơn hàng","order":2,"columns":["name","status"],"sort":{"field":"code","dir":"desc"},"allowCreate":true,"allowLink":false}`))
	if err != nil {
		t.Fatalf("SetViewConfig: %v", err)
	}
	if cfg["label"] != "Đơn hàng" || cfg["allowCreate"] != true {
		t.Errorf("echoed config = %+v; want label + allowCreate round-tripped", cfg)
	}
	if len(meta.viewConfigWrites) != 1 {
		t.Fatalf("writes = %d; want 1", len(meta.viewConfigWrites))
	}
	w := meta.viewConfigWrites[0]
	if w.relID != "rel-1" || w.config == nil {
		t.Fatalf("write = %+v; want rel-1 with non-nil config", w)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(*w.config), &stored); err != nil {
		t.Fatalf("stored config is not valid JSON: %v (raw=%q)", err, *w.config)
	}
	if stored["order"] != float64(2) {
		t.Errorf("stored order = %v; want 2", stored["order"])
	}
	sort, _ := stored["sort"].(map[string]any)
	if sort["field"] != "code" || sort["dir"] != "desc" {
		t.Errorf("stored sort = %v; want {code desc}", stored["sort"])
	}
}

func TestSetViewConfig_NullClears(t *testing.T) {
	meta, svc := newViewConfigFixture()

	cfg, err := svc.SetViewConfig(context.Background(), "t1", "rel-1", json.RawMessage(`null`))
	if err != nil {
		t.Fatalf("SetViewConfig(null): %v", err)
	}
	if cfg != nil {
		t.Errorf("cleared config echo = %v; want nil", cfg)
	}
	if len(meta.viewConfigWrites) != 1 || meta.viewConfigWrites[0].config != nil {
		t.Fatalf("writes = %+v; want one nil (clear) write", meta.viewConfigWrites)
	}
}

func TestSetViewConfig_ValidationRejects(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantCode  string
		wantField string // "" = don't check
	}{
		{"not an object", `[1,2]`, "RELVIEW_INVALID", ""},
		{"unknown top-level key", `{"labels":"x"}`, "RELVIEW_INVALID", "labels"},
		{"label wrong type", `{"label":5}`, "RELVIEW_INVALID", "label"},
		{"order not integer", `{"order":1.5}`, "RELVIEW_INVALID", "order"},
		{"allowCreate wrong type", `{"allowCreate":"yes"}`, "RELVIEW_INVALID", "allowCreate"},
		{"columns wrong type", `{"columns":"name"}`, "RELVIEW_INVALID", "columns"},
		{"columns duplicate", `{"columns":["name","name"]}`, "RELVIEW_INVALID", "columns"},
		{"column not a child field", `{"columns":["ghost"]}`, "RELVIEW_COLUMN_UNKNOWN", "ghost"},
		{"sort missing field", `{"sort":{"dir":"asc"}}`, "RELVIEW_INVALID", "sort"},
		{"sort bad dir", `{"sort":{"field":"code","dir":"up"}}`, "RELVIEW_INVALID", "sort"},
		{"sort unknown key", `{"sort":{"field":"code","direction":"asc"}}`, "RELVIEW_INVALID", "sort"},
		{"sort field unknown", `{"sort":{"field":"ghost"}}`, "RELVIEW_COLUMN_UNKNOWN", "sort.field"},
		{"sort field not sortable", `{"sort":{"field":"name"}}`, "RELVIEW_SORT_NOT_SORTABLE", "sort.field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, svc := newViewConfigFixture()
			_, err := svc.SetViewConfig(context.Background(), "t1", "rel-1", json.RawMessage(tt.body))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v; want *ValidationError", err)
			}
			if ve.Code != tt.wantCode {
				t.Errorf("code = %q; want %q (msg=%q)", ve.Code, tt.wantCode, ve.Msg)
			}
			if tt.wantField != "" && ve.Field != tt.wantField {
				t.Errorf("field = %q; want %q", ve.Field, tt.wantField)
			}
			if len(meta.viewConfigWrites) != 0 {
				t.Errorf("rejected config must not be persisted; writes = %+v", meta.viewConfigWrites)
			}
		})
	}
}

func TestSetViewConfig_SortableFields(t *testing.T) {
	// unique (code), relation (customer), indexed (status) and system columns all pass;
	// label ≤100 runes passes at the boundary.
	for _, body := range []string{
		`{"sort":{"field":"code"}}`,
		`{"sort":{"field":"customer","dir":"asc"}}`,
		`{"sort":{"field":"status","dir":"desc"}}`,
		`{"sort":{"field":"created_at","dir":"desc"}}`,
		`{"sort":{"field":"updated_at"}}`,
		`{"sort":{"field":"id"}}`,
	} {
		if _, err := func() (map[string]any, error) {
			_, svc := newViewConfigFixture()
			return svc.SetViewConfig(context.Background(), "t1", "rel-1", json.RawMessage(body))
		}(); err != nil {
			t.Errorf("SetViewConfig(%s) = %v; want ok", body, err)
		}
	}
}

func TestSetViewConfig_TenantPinned(t *testing.T) {
	meta, svc := newViewConfigFixture()

	// A tenant-B caller addressing tenant A's relationship id resolves not-found — no
	// existence leak, no write.
	_, err := svc.SetViewConfig(context.Background(), "t2", "rel-1", json.RawMessage(`{"label":"X"}`))
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "RELATIONSHIP_NOT_FOUND" {
		t.Fatalf("cross-tenant err = %v; want RELATIONSHIP_NOT_FOUND", err)
	}
	if len(meta.viewConfigWrites) != 0 {
		t.Errorf("cross-tenant attempt must not write; writes = %+v", meta.viewConfigWrites)
	}
}

func TestSetViewConfig_LabelOnlySkipsFieldLoad(t *testing.T) {
	// A config that never references child fields must validate even when the child entity
	// is gone (lazy allowlist) — and a field-referencing one must fail with ENTITY_NOT_FOUND.
	meta, svc := newViewConfigFixture()
	delete(meta.entities, "t1/orders")

	if _, err := svc.SetViewConfig(context.Background(), "t1", "rel-1", json.RawMessage(`{"label":"Chỉ nhãn","order":1}`)); err != nil {
		t.Fatalf("label-only config with dead child entity: %v; want ok (lazy field load)", err)
	}

	_, err := svc.SetViewConfig(context.Background(), "t1", "rel-1", json.RawMessage(`{"columns":["name"]}`))
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "ENTITY_NOT_FOUND" {
		t.Fatalf("columns config with dead child entity = %v; want ENTITY_NOT_FOUND", err)
	}
}
