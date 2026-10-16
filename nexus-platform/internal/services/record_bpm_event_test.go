package services

// specs/043 S2b (T043) — the join between the record write path and the approval engine: the
// bpm.Event a mutation hands the engine must carry the before-image (so "vừa đổi thành" can be
// judged at all) and the automation chain depth (so the cap shared with workflows can bite). The
// engine's own tests prove what it does WITH those fields; this proves somebody fills them.

import (
	"context"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// captureBPM records every event handed to the approval engine (Emit is called synchronously).
type captureBPM struct{ events []bpm.Event }

func (c *captureBPM) Emit(_ context.Context, evt bpm.Event) { c.events = append(c.events, evt) }

// beforeImageStore serves one canned before-image to Update's read.
type beforeImageStore struct {
	crCountingStore
	before map[string]any
}

func (s *beforeImageStore) GetRecord(_ context.Context, _, _, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return &dynrepo.Record{ID: id, TenantID: "t1", Data: s.before, OwnerID: "u-owner"}, nil
}

func TestRecordService_ApprovalEventCarriesBeforeAndDepth(t *testing.T) {
	catalog := &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-q", TenantID: "t1", Name: "crm_quotes", DataTable: "obj_crm_quotes"},
		fields: []metadata.FieldDefinition{{Name: "status", FieldType: "text"}, {Name: "note", FieldType: "text"}},
	}
	store := &beforeImageStore{crCountingStore: crCountingStore{hardCap: 10}, before: map[string]any{"status": "Nháp", "note": "a"}}
	eng := &captureBPM{}
	svc := NewRecordService(catalog, store).WithApprovalEngine(eng)
	admin := RecordScope{UserID: "u-1", AccessLevel: "all", IsAdmin: true}

	// A record an automation wrote two hops down the chain.
	ctx := workflow.WithChainDepth(context.Background(), 2)
	if err := svc.Update(ctx, admin, "t1", "crm_quotes", "q1", map[string]any{"status": "Chờ duyệt"}, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := svc.Create(workflow.WithChainDepth(context.Background(), 3), admin, "t1", "u-1", "crm_quotes", map[string]any{"status": "Nháp"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(eng.events) != 2 {
		t.Fatalf("approval engine got %d events, want 2 (update + create)", len(eng.events))
	}
	upd, crt := eng.events[0], eng.events[1]
	if upd.Trigger != bpm.TriggerAfterUpdate || upd.Before["status"] != "Nháp" || upd.Data["status"] != "Chờ duyệt" || upd.Depth != 2 {
		t.Errorf("update event = {trigger %s, before %v, data %v, depth %d}; want after_update, before status Nháp, data status Chờ duyệt, depth 2",
			upd.Trigger, upd.Before, upd.Data, upd.Depth)
	}
	if crt.Trigger != bpm.TriggerAfterCreate || crt.Before != nil || crt.Depth != 3 {
		t.Errorf("create event = {trigger %s, before %v, depth %d}; want after_create, no before-image, depth 3",
			crt.Trigger, crt.Before, crt.Depth)
	}
}
