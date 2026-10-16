package services

// Security regression (ACTIVITY-CORE #3a, review HIGH): a caller who cannot VIEW a
// polymorphic_relation field must not disclose its stored values by GROUPing an aggregate on its
// `<f>_type` / `<f>_id` sub-columns. Locks polymorphicGuardField into the Aggregate groupBy guard
// — a regression that dropped it would let `?groupBy=<f>_id` leak the hidden ids as group keys.
// Reuses the rj* fakes from report_join_test.go (same package).

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
)

// polyACLService builds a RecordService over an "events" entity carrying a polymorphic field
// `linked`, with the given field-ACL fake. No authorizer is needed (a base — non-joined —
// aggregate never calls authz).
func polyACLService(store RecordStore, fACL *rjFieldACL) *RecordService {
	cat := &rjCatalog{
		entities: map[string]*metadata.EntityDefinition{"events": ent("events")},
		fields: map[string][]metadata.FieldDefinition{
			"events": {{Name: "linked", FieldType: "polymorphic_relation"}},
		},
	}
	return NewRecordService(cat, store).WithFieldACL(fACL)
}

func TestAggregate_PolymorphicGroupBy_FieldACL(t *testing.T) {
	ctx := context.Background()
	scope := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "all"}
	q := AggregateQuery{Metric: "count", GroupBy: "linked_id"}

	t.Run("hidden polymorphic field → groupBy on its sub-column is FIELD_FORBIDDEN, store not reached", func(t *testing.T) {
		store := &rjStore{}
		fACL := &rjFieldACL{hidden: map[string]bool{"events.linked": true}}
		s := polyACLService(store, fACL)
		_, err := s.Aggregate(ctx, scope, "t1", "events", q)
		if codeOf(err) != "FIELD_FORBIDDEN" {
			t.Fatalf("code = %q, want FIELD_FORBIDDEN (err=%v)", codeOf(err), err)
		}
		if store.groupedCalled || store.scalarCalled {
			t.Fatalf("store must NOT be reached once field-ACL rejects the hidden groupBy")
		}
		// Proves the guard asked about the BASE polymorphic field, not the raw sub-column.
		var askedBase bool
		for _, k := range fACL.asked {
			if k == "events.linked" {
				askedBase = true
			}
			if k == "events.linked_id" {
				t.Fatalf("field-ACL asked about the raw sub-column 'events.linked_id' — guard-mapping bug")
			}
		}
		if !askedBase {
			t.Fatalf("field-ACL was never asked about the base field events.linked; asked=%v", fACL.asked)
		}
	})

	t.Run("visible polymorphic field → groupBy reaches the store", func(t *testing.T) {
		store := &rjStore{}
		s := polyACLService(store, &rjFieldACL{hidden: map[string]bool{}})
		if _, err := s.Aggregate(ctx, scope, "t1", "events", q); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !store.groupedCalled {
			t.Fatalf("store.AggregateGrouped should be reached once field-ACL passes")
		}
	})
}
