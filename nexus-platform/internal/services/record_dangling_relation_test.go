package services

// A record whose relation TARGET was deleted must stay editable.
//
// The form resends every field on update (that is what lets a user clear one), so an untouched
// relation FK came back verbatim and was re-probed on every save. Once its target was soft-deleted
// the probe failed forever: the record became permanently unsavable, and for a REQUIRED relation
// there was no way out at all — clearing it answers 422 RECORD_REQUIRED_MISSING, resending it
// answers 422 RECORD_RELATION_INVALID. Deleting ONE record took every record pointing at it away.
//
// This is the same bug the field-ACL path already fixed with the unchanged-value rule ("marking one
// field read-only took the whole record away"); the relation FK never got the same treatment.

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// danglingStore reports which relation ids still exist, and what the record currently holds.
type danglingStore struct {
	fakeRecordStore
	live    map[string]bool // relation id → target still exists
	current map[string]any  // what the stored record holds today
	updated bool
}

func (d *danglingStore) BatchRecordExists(_ context.Context, _, _ string, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = d.live[id]
	}
	return out, nil
}

func (d *danglingStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return &dynrepo.Record{ID: "rec-1", Data: d.current}, nil
}

func (d *danglingStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	d.updated = true
	return nil
}

// Options on a FieldDefinition is raw JSON (*string), not a map — the map form is FieldSpec.
var relOpts = `{"relatedEntity":"crm_accounts"}`

func newDanglingService(store *danglingStore) *RecordService {
	cat := &fakeRecordCatalog{
		entity: courseEntityFixture(),
		fields: append(courseFieldDefs(), metadata.FieldDefinition{
			Name: "khach_hang", FieldType: "relation", Options: &relOpts,
		}),
	}
	return NewRecordService(cat, store)
}

func TestUpdate_UnchangedRelationWithDeadTargetDoesNotBlockTheSave(t *testing.T) {
	store := &danglingStore{
		live:    map[string]bool{}, // the target is GONE
		current: map[string]any{"khach_hang": "acc-deleted", "title": "cũ"},
	}
	svc := newDanglingService(store)

	// The form resends the dangling id verbatim, and edits something else.
	err := svc.Update(context.Background(), RecordScope{IsAdmin: true, AccessLevel: "all"},
		"tenant-1", "courses", "rec-1",
		map[string]any{"khach_hang": "acc-deleted", "title": "mới"}, nil)
	if err != nil {
		t.Fatalf("a record with a dead relation target could not be edited at all: %v", err)
	}
	if !store.updated {
		t.Error("the update never reached the store")
	}
}

func TestUpdate_CHANGINGARelationToADeadTargetIsStillRefused(t *testing.T) {
	store := &danglingStore{
		live:    map[string]bool{"acc-live": true},
		current: map[string]any{"khach_hang": "acc-live", "title": "cũ"},
	}
	svc := newDanglingService(store)

	// This IS the caller's edit — pointing the record at something that does not exist.
	err := svc.Update(context.Background(), RecordScope{IsAdmin: true, AccessLevel: "all"},
		"tenant-1", "courses", "rec-1",
		map[string]any{"khach_hang": "acc-nonexistent"}, nil)
	if codeOf(err) != "RECORD_RELATION_INVALID" {
		t.Fatalf("err = %v; want RECORD_RELATION_INVALID — the skip must only cover values the caller did NOT touch", err)
	}
	if store.updated {
		t.Error("a record was pointed at a nonexistent target")
	}
}
