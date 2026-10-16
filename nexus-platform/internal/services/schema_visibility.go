package services

import (
	"time"

	"nexus/platform/internal/metadata"
)

// Schema visibility of a delivered workspace (specs/063-draft-publish S7-8, FR-060): a field, table
// or relationship created AFTER the last publish is not yet the customer's. The three helpers below
// are the one comparison every reader shares (layout service, GET /metadata/fields|entities|
// relationships); the cutoff itself comes from DraftGate.SchemaCutoff, which already answers
// "show everything" to the builder's draft overlay and to a tenant that was never delivered.
//
// The cutoff is cut to the second by DraftGate and the created_at columns have no fraction, so the
// comparison is a plain After.

// HideFieldsNewerThan drops the fields created after cutoff. The input slice is not mutated.
func HideFieldsNewerThan(fields []metadata.FieldDefinition, cutoff time.Time) []metadata.FieldDefinition {
	out := make([]metadata.FieldDefinition, 0, len(fields))
	for _, f := range fields {
		if !f.CreatedAt.After(cutoff) {
			out = append(out, f)
		}
	}
	return out
}

// HideEntitiesNewerThan drops the tables created after cutoff.
func HideEntitiesNewerThan(entities []metadata.EntityDefinition, cutoff time.Time) []metadata.EntityDefinition {
	out := make([]metadata.EntityDefinition, 0, len(entities))
	for _, e := range entities {
		if !e.CreatedAt.After(cutoff) {
			out = append(out, e)
		}
	}
	return out
}

// HideRelationshipsNewerThan drops the relationships created after cutoff: a new relation field on a
// child table writes a sys_relationships row that would open a new "Liên quan" card on the parent's
// detail page for the customer.
func HideRelationshipsNewerThan(rels []metadata.RelationshipDefinition, cutoff time.Time) []metadata.RelationshipDefinition {
	out := make([]metadata.RelationshipDefinition, 0, len(rels))
	for _, r := range rels {
		if !r.CreatedAt.After(cutoff) {
			out = append(out, r)
		}
	}
	return out
}
