package services

// Manual record shares (DAILY-QUEUE #34 record-manual-share-a, specs/051-record-manual-share) — the
// RecordService half: the gate every share-management call passes (ResolveShareTarget) and the
// read-only probe that turns a write on a shared-only record into 403 instead of 404. Storage of the
// share rows lives in internal/shares; the row filter that makes a share visible lives in
// dynrepo/rowscope.go.

import (
	"context"
	"errors"
	"fmt"

	"nexus/platform/internal/dynrepo"
)

// ErrRecordSharedReadOnly: the caller reaches this record only through a manual share, and shares
// are read-only (Q4 2026-09-05) — a write is refused with 403 instead of the bare 404 so the client
// can say "chỉ xem". Only ever returned AFTER the caller's own write scope missed the record, so it
// tells them nothing they could not already read.
var ErrRecordSharedReadOnly = invalid("RECORD_SHARED_READ_ONLY", "Hồ sơ này được chia sẻ để xem, không sửa được")

// ErrRecordEditForbidden: the caller's write scope missed a record their VIEW scope reaches (issue #18:
// view=all, edit=own — a rep saving a colleague's ticket). They can already read it, so 403 tells them
// nothing new, where the bare 404 told them it did not exist. Code and sentence are the ACL stage's 403
// (middleware.abortForbidden); the record handler answers it with that exact body.
var ErrRecordEditForbidden = invalid("FORBIDDEN", "Bạn không có quyền thực hiện thao tác này")

// ResolveShareTarget authorizes managing the shares of (entityName, recordID) — Q4(a): whoever may
// EDIT the record may share it. It is ResolveWriteTarget for "edit"; returns the canonical entity
// name to store on the share row.
func (s *RecordService) ResolveShareTarget(ctx context.Context, scope RecordScope, tenantID, entityName, recordID string) (string, error) {
	return s.ResolveWriteTarget(ctx, scope, tenantID, entityName, recordID, "edit")
}

// ResolveWriteTarget authorizes a write of kind action ("edit" for share management, "view" for a
// note — specs/060 #349) on the EXISTING record (entityName, recordID). Like ResolveNoteTarget it does
// not trust the route's own ACL mount:
//
//  1. re-resolve the caller's grant for action (authorizeAction; denied → 403 FORBIDDEN before any
//     read — admins and a nil authorizer pass through, the codebase-wide convention);
//  2. resolve the entity (ErrEntityNotFound);
//  3. read the record under that scope with shares OFF — a write never reaches a row through a
//     share (specs/051) — and answer a miss with ExplainWriteMiss: 403 when the caller can read the
//     record, the 404 a direct GET gives otherwise.
//
// Returns the entity's canonical name.
func (s *RecordService) ResolveWriteTarget(ctx context.Context, scope RecordScope, tenantID, entityName, recordID, action string) (string, error) {
	writeScope, err := s.authorizeAction(ctx, tenantID, scope, entityName, action)
	if err != nil {
		return "", err
	}
	writeScope.IncludeShares = false
	e, _, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return "", err
	}
	if _, err := s.store.GetRecord(ctx, e.Name, tenantID, recordID, writeScope.toRowScope()); err != nil {
		return "", s.ExplainWriteMiss(ctx, writeScope, tenantID, e.Name, recordID, err)
	}
	return e.Name, nil
}

// ExplainWriteMiss is THE answer for a write whose own scope missed the record (#18, #43): record
// PUT/DELETE, share management, note create, file upload, approval submit and relation link/unlink
// all ask it, so "viewable but not writable" reads the same on every surface. It asks what the
// caller may READ — never more than a GET of the same record would have told them:
//   - the SAME level plus the manual-share clause reaches it → they see it only because it was shared
//     → ErrRecordSharedReadOnly (the share clause only widens owner/team levels, so only those probe);
//   - their VIEW scope reaches it → ErrRecordEditForbidden (issue #18);
//   - otherwise writeErr unchanged: the 404 stays a 404, also for a record deleted since the write.
//
// Admins skip both probes (a miss means the record is gone), and so does a write level of "all" (no
// view level is wider). A probe or ACL failure is returned wrapped rather than guessed at.
func (s *RecordService) ExplainWriteMiss(ctx context.Context, writeScope RecordScope, tenantID, entity, id string, writeErr error) error {
	return explainWriteMiss(ctx, s.store, s.authz, writeScope, tenantID, entity, id, writeErr)
}

// explainWriteMiss is ExplainWriteMiss over explicit dependencies, for a service that holds the
// store and the authorizer but not a RecordService (RelationService). A nil authz skips the view probe.
func explainWriteMiss(ctx context.Context, store RecordFetcher, authz entityAuthorizer, writeScope RecordScope, tenantID, entity, id string, writeErr error) error {
	lvl := dynrepo.AccessLevel(writeScope.AccessLevel)
	if !errors.Is(writeErr, ErrRecordNotFound) || writeScope.IsAdmin || lvl == dynrepo.AccessAll {
		return writeErr
	}
	if lvl.IsOwnerScoped() {
		probe := writeScope.toRowScope()
		probe.IncludeShares = true
		if _, err := store.GetRecord(ctx, entity, tenantID, id, probe); err == nil {
			return ErrRecordSharedReadOnly
		} else if !errors.Is(err, ErrRecordNotFound) {
			return fmt.Errorf("services.explainWriteMiss: %w", err)
		}
	}
	if authz == nil {
		return writeErr
	}
	allowed, viewLevel, err := authz.Authorize(ctx, tenantID, writeScope.RoleID, entity, "view")
	if err != nil {
		return fmt.Errorf("services.explainWriteMiss: %w", err)
	}
	// A level equal to the write level was just probed (or is "none": 1=0) — nothing new to ask.
	if !allowed || viewLevel == writeScope.AccessLevel || dynrepo.AccessLevel(viewLevel) == dynrepo.AccessNone {
		return writeErr
	}
	view := writeScope // identity verbatim; only the level changes, with the share clause a view read carries
	view.AccessLevel, view.IncludeShares = viewLevel, true
	if _, err := store.GetRecord(ctx, entity, tenantID, id, view.toRowScope()); err != nil {
		if errors.Is(err, ErrRecordNotFound) {
			return writeErr
		}
		return fmt.Errorf("services.explainWriteMiss: %w", err)
	}
	return ErrRecordEditForbidden
}
