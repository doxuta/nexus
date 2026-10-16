package services

// ResolveNoteTarget backs the RECORD-NOTES engine capability (docs/NEXUS-MOT-XUONG-2026-07-29.md
// §3d mục 1): it is the single authorization chokepoint every notes operation (list/create/delete)
// calls before touching sys_record_notes, mirroring ResolveFileField's role for the file subsystem.

import (
	"context"
)

// ResolveNoteTarget authorizes the notes sub-resource of (entityName, recordID) for the caller.
//
// WHY THIS EXISTS (not just "reuse scopeOf(c)"): the notes routes are mounted on the same
// RequireACL-gated record group as everything else, but gin's default method→action map (GET→view,
// POST→create, DELETE→delete — middleware/acl.go) is the WRONG grant for a POST: "create" authorizes
// creating a NEW top-level record, not attaching a note to an EXISTING one. A role holding
// create:all + view:owner (a real seeded shape — see a5_team_rls_integration_test.go) would
// otherwise attach notes to records it cannot view, turning 201-vs-404 into an A5 existence oracle.
// So every notes method re-resolves the caller's VIEW grant here, via AuthorizeView — the SAME
// primitive report-run (report_service.go) and the dashboard runner already use for exactly this
// "entity/route mismatch" reason — regardless of which action the route's own middleware resolved.
// This makes the route mounting defense-in-depth rather than the load-bearing check.
//
// Order mirrors RunReport: AuthorizeView FIRST (a caller with no matching grant at all is refused
// before any existence probe — the least amount of information leaked on the most common deny path),
// THEN resolve() (entity existence — ErrEntityNotFound), THEN store.GetRecord under the
// VIEW-resolved A5 row scope (record existence + row-level visibility — ErrRecordNotFound). A
// record the caller may not see is indistinguishable from one that does not exist, exactly like a
// direct GET /:entity/:id.
//
// Returns the entity's canonical name (for storing on the note row) and the VIEW-resolved scope
// (identity fields verbatim, AccessLevel = the resolved view grant level; admin/nil-authz pass
// through unchanged, matching AuthorizeView).
func (s *RecordService) ResolveNoteTarget(ctx context.Context, scope RecordScope, tenantID, entityName, recordID string) (canonicalEntity string, viewScope RecordScope, err error) {
	viewScope, err = s.AuthorizeView(ctx, tenantID, scope, entityName)
	if err != nil {
		return "", RecordScope{}, err
	}
	e, _, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return "", RecordScope{}, err
	}
	// Sentinel pass-through, not wrapped — mirrors Get/ResolveFileField: the caller branches on
	// errors.Is(err, ErrRecordNotFound), and resolve()/GetRecord() already wrap any genuine
	// (non-sentinel) infrastructure failure themselves before it reaches here.
	if _, err := s.store.GetRecord(ctx, e.Name, tenantID, recordID, viewScope.toRowScope()); err != nil {
		return "", RecordScope{}, err
	}
	return e.Name, viewScope, nil
}
