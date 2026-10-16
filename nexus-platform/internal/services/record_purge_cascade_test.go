package services

// specs/056 US2 (#47 record-hard-delete-b) — RecordService.Purge resolves the dependents graph from
// metadata (sys_relationships + the child's live relation field) and turns the repository's
// cascade outcome into the report, the refusals and the trail.

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// purgeCatalog is a multi-entity EntityCatalog + relationship registry.
type purgeCatalog struct {
	entities map[string][]metadata.FieldDefinition // entity name → live fields (entity id = "id-"+name)
	rels     []metadata.RelationshipDefinition
}

func (c *purgeCatalog) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	if _, ok := c.entities[name]; !ok {
		return nil, metadata.ErrEntityNotFound
	}
	return &metadata.EntityDefinition{ID: "id-" + name, TenantID: tenantID, Name: name, DataTable: "obj_" + name}, nil
}

func (c *purgeCatalog) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	return c.entities[entityID[len("id-"):]], nil
}

func (c *purgeCatalog) ListEntities(tenantID string) ([]metadata.EntityDefinition, error) {
	var out []metadata.EntityDefinition
	for name := range c.entities {
		out = append(out, metadata.EntityDefinition{ID: "id-" + name, TenantID: tenantID, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *purgeCatalog) ListRelationshipsForEntity(_ context.Context, _, name string) ([]metadata.RelationshipDefinition, error) {
	var out []metadata.RelationshipDefinition
	for _, r := range c.rels {
		if r.ParentEntityName == name || r.ChildEntityName == name {
			out = append(out, r)
		}
	}
	return out, nil
}

func relField(t *testing.T, name string, required bool, cardinality string) metadata.FieldDefinition {
	t.Helper()
	opts := map[string]any{"relatedEntity": "x"}
	if cardinality != "" {
		opts["cardinality"] = cardinality
	}
	return metadata.FieldDefinition{Name: name, FieldType: "relation", IsRequired: required, Options: jsonOptsPtr(t, opts)}
}

func rel1N(parent, child, fk string) metadata.RelationshipDefinition {
	return metadata.RelationshipDefinition{RelationshipType: "1-N", ParentEntityName: parent, ChildEntityName: child, ChildFKColumn: fk}
}

// customers ← orders (required) ← order_lines (required); ← activities (optional); ← profiles (1-1,
// required); customers ↔ tags (N-N); a registry row whose field was removed (invoices.old); a
// relation where customers is the CHILD (customers.company → companies) — not a dependent; and a
// polymorphic field events.about → {customers | orders} that registers NO relationship row.
func cascadeCatalog(t *testing.T) *purgeCatalog {
	t.Helper()
	return &purgeCatalog{
		entities: map[string][]metadata.FieldDefinition{
			"customers":   {{Name: "name", FieldType: "text"}, relField(t, "company", false, "")},
			"orders":      {{Name: "code", FieldType: "text"}, relField(t, "customer", true, "")},
			"order_lines": {{Name: "qty", FieldType: "number"}, relField(t, "order", true, "")},
			"activities":  {{Name: "note", FieldType: "text", Options: jsonOptsPtr(t, map[string]any{"sensitive": true})}, relField(t, "regarding", false, "")},
			"profiles":    {relField(t, "customer", true, "1-1")},
			"invoices":    {{Name: "total", FieldType: "number"}},
			"tags":        {{Name: "label", FieldType: "text"}},
			"companies":   {{Name: "name", FieldType: "text"}},
			"events": {{Name: "about", FieldType: "polymorphic_relation",
				Options: jsonOptsPtr(t, map[string]any{"targetEntities": []any{"customers", "orders"}})}},
		},
		rels: []metadata.RelationshipDefinition{
			rel1N("customers", "orders", "customer_id"),
			rel1N("orders", "order_lines", "order_id"),
			rel1N("customers", "activities", "regarding_id"),
			{RelationshipType: "1-1", ParentEntityName: "customers", ChildEntityName: "profiles", ChildFKColumn: "customer_id"},
			{RelationshipType: "N-N", ParentEntityName: "customers", ChildEntityName: "tags"},
			rel1N("customers", "invoices", "old_id"),
			rel1N("companies", "customers", "company_id"),
		},
	}
}

func cascadeSvc(t *testing.T, store RecordStore) (*RecordService, *captureAuditor) {
	t.Helper()
	cat := cascadeCatalog(t)
	aud := &captureAuditor{}
	return NewRecordService(cat, store).WithAuditor(aud).WithRelationshipLister(cat), aud
}

func TestRecordPurge_GraphFromMetadata(t *testing.T) {
	store := &purgeStore{res: &dynrepo.PurgeResult{Records: []dynrepo.PurgedRecord{{Entity: "customers", ID: "c1"}}}}
	svc, _ := cascadeSvc(t, store)
	if _, err := svc.Purge(context.Background(), operatorScope(), "tenant-1", "customers", "c1", true); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(store.got) != 1 {
		t.Fatalf("store calls = %d; want 1", len(store.got))
	}
	p := store.got[0]
	if p.ActorID != "op-1" {
		t.Fatalf("ActorID = %q; want the operator (stamped on cleared references)", p.ActorID)
	}
	want := map[string]dynrepo.PurgeEntityDeps{
		"customers": {
			Children: []dynrepo.PurgeChildRef{
				{Entity: "orders", Field: "customer", Required: true},
				{Entity: "activities", Field: "regarding"},
				{Entity: "profiles", Field: "customer", Required: true, OneToOne: true},
				{Entity: "events", Field: "about", Polymorphic: true},
			},
			JoinPeers: []string{"tags"},
		},
		"orders": {Children: []dynrepo.PurgeChildRef{
			{Entity: "order_lines", Field: "order", Required: true},
			{Entity: "events", Field: "about", Polymorphic: true},
		}},
		"order_lines": {},
		"profiles":    {},
	}
	for name, deps := range p.Graph {
		sort.Slice(deps.Children, func(i, j int) bool { return deps.Children[i].Entity < deps.Children[j].Entity })
		p.Graph[name] = deps
	}
	for name, deps := range want {
		sort.Slice(deps.Children, func(i, j int) bool { return deps.Children[i].Entity < deps.Children[j].Entity })
		want[name] = deps
	}
	if !reflect.DeepEqual(p.Graph, want) {
		t.Fatalf("graph =\n %+v\nwant\n %+v", p.Graph, want)
	}
}

func TestRecordPurge_CascadeReportAndTrail(t *testing.T) {
	store := &purgeStore{res: &dynrepo.PurgeResult{
		Records: []dynrepo.PurgedRecord{
			{Entity: "customers", ID: "c1", Data: map[string]any{"name": "An"}},
			{Entity: "orders", ID: "o1", Data: map[string]any{"code": "DH-1", "customer": "c1"}},
		},
		Cleared: []dynrepo.ClearedRef{{Entity: "activities", ID: "a1", Field: "regarding", Was: "c1"}},
		Tables:  []dynrepo.PurgeTableCount{{Table: "obj_customers", Rows: 1}, {Table: "obj_orders", Rows: 1}},
	}}
	svc, aud := cascadeSvc(t, store)
	rep, err := svc.Purge(context.Background(), operatorScope(), "tenant-1", "customers", "c1", false)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(rep.Records) != 2 || len(rep.Cleared) != 1 || rep.Cleared[0] != (PurgeClearedRef{Entity: "activities", ID: "a1", Field: "regarding"}) {
		t.Fatalf("report records=%+v cleared=%+v", rep.Records, rep.Cleared)
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	if len(aud.events) != 3 {
		t.Fatalf("audit events = %d; want 2 hard_delete + 1 update", len(aud.events))
	}
	byRec := map[string]audit.Event{}
	for _, e := range aud.events {
		byRec[e.EntityName+"/"+e.RecordID] = e
	}
	if e := byRec["orders/o1"]; e.Action != audit.ActionHardDelete || e.Diff == nil || e.Diff.Before["code"] != "DH-1" || e.ActorUserID != "op-1" {
		t.Fatalf("child hard_delete row = %+v; want before-only under its OWN entity", e)
	}
	e := byRec["activities/a1"]
	if e.Action != audit.ActionUpdate || e.Diff == nil || e.Diff.Before["regarding"] != "c1" {
		t.Fatalf("cleared reference row = %+v; want update regarding c1 → null", e)
	}
	if v, ok := e.Diff.After["regarding"]; !ok || v != nil {
		t.Fatalf("cleared reference after-side = %v; want regarding: null", e.Diff.After)
	}
}

func TestRecordPurge_CascadeRefusalsMap(t *testing.T) {
	blockers := []dynrepo.PurgeBlocker{{Entity: "orders", Field: "customer", Count: 2}}
	tests := []struct {
		name       string
		storeErr   error
		wantCode   string
		wantDetail string
	}{
		{"live required children", &dynrepo.PurgeBlockedError{Blocked: blockers}, "RECORD_PURGE_LIVE_CHILDREN", "blockers"},
		{"cascade too large", &dynrepo.PurgeTooLargeError{Max: 500}, "RECORD_PURGE_TOO_LARGE", "max"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, aud := cascadeSvc(t, &purgeStore{err: tt.storeErr})
			_, err := svc.Purge(context.Background(), operatorScope(), "tenant-1", "customers", "c1", false)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v; want ValidationError %s", err, tt.wantCode)
			}
			if _, ok := ve.Details[tt.wantDetail]; !ok {
				t.Fatalf("details = %+v; want key %q", ve.Details, tt.wantDetail)
			}
			if len(aud.events) != 0 {
				t.Fatalf("a refused purge audited %d rows", len(aud.events))
			}
		})
	}
	// A dry run REPORTS blockers instead of failing: the operator reads what to bin first.
	svc, _ := cascadeSvc(t, &purgeStore{res: &dynrepo.PurgeResult{Records: []dynrepo.PurgedRecord{{Entity: "customers", ID: "c1"}}, Blocked: blockers}})
	rep, err := svc.Purge(context.Background(), operatorScope(), "tenant-1", "customers", "c1", true)
	if err != nil || !reflect.DeepEqual(rep.Blocked, blockers) {
		t.Fatalf("dry run: rep.Blocked=%+v err=%v; want the blockers in the report", rep, err)
	}
}
