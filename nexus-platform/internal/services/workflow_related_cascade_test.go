package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

// #330: UpdateRelatedField emits the target's after_update only when the write changed the record.
type cascadeStore struct {
	trailStore
	reads  int
	images []map[string]any // image returned by the 1st, 2nd... GetRecord
}

func (s *cascadeStore) GetRecord(_ context.Context, _, _, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	i := s.reads
	if i >= len(s.images) {
		i = len(s.images) - 1
	}
	s.reads++
	return &dynrepo.Record{ID: id, Data: s.images[i], OwnerID: "owner-1"}, nil
}

func TestUpdateRelatedField_EmitsOnlyWhenChanged(t *testing.T) {
	tests := []struct {
		name     string
		after    string
		wantEmit bool
	}{
		{"changed value emits after_update at the given depth", "Công ty Mới", true},
		{"unchanged value emits nothing", "Công ty Cũ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// no auditor ⇒ writeFields reads nothing: read 1 = before-image, read 2 = after-image.
			store := &cascadeStore{images: []map[string]any{{"name": "Công ty Cũ"}, {"name": tt.after}}}
			var got []workflow.Event
			mut := NewWorkflowRecordMutator(store).WithCatalog(sensitiveFixture(t)).
				WithEmitter(func(_ context.Context, e workflow.Event) { got = append(got, e) })
			if err := mut.UpdateRelatedField(context.Background(), "customers", "tenant-1", "rec-9", workflow.SystemActorID, "name", tt.after, 3); err != nil {
				t.Fatal(err)
			}
			if tt.wantEmit != (len(got) == 1) {
				t.Fatalf("events = %d, wantEmit %v", len(got), tt.wantEmit)
			}
			if tt.wantEmit {
				e := got[0]
				if e.Depth != 3 || e.Trigger != workflow.TriggerAfterUpdate || e.TenantID != "tenant-1" || e.RecordID != "rec-9" ||
					e.OwnerUserID != "owner-1" || e.Before["name"] != "Công ty Cũ" || e.Data["name"] != "Công ty Mới" {
					t.Errorf("event = %+v", e)
				}
			}
		})
	}
}
