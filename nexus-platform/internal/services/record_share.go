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
// EDIT the record may share it. Like ResolveNoteTarget it does not trust the route's own ACL mount:
//
//  1. re-resolve the caller's "edit" grant on the entity (authorizeAction; denied → 403 FORBIDDEN
//     before any read — admins and a nil authorizer pass through, the codebase-wide convention);
//  2. resolve the entity (ErrEntityNotFound);
//  3. read the record under that EDIT scope with shares OFF — a record visible to them only
//     through a share is RECORD_SHARED_READ_ONLY (403, they may not re-share or revoke), anything
//     else out of scope is the same 404 a direct GET gives.
//
// Returns the canonical entity name to store on the share row.
func (s *RecordService) ResolveShareTarget(ctx context.Context, scope RecordScope, tenantID, entityName, recordID string) (string, error) {
	editScope, err := s.authorizeAction(ctx, tenantID, scope, entityName, "edit")
	if err != nil {
		return "", err
	}
	e, _, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return "", err
	}
	if _, err := s.store.GetRecord(ctx, e.Name, tenantID, recordID, editScope.toRowScope()); err != nil {
		return "", s.sharedReadOnlyOr(ctx, editScope, tenantID, e.Name, recordID, err)
	}
	return e.Name, nil
}

// sharedReadOnlyOr maps a write that missed the record under the caller's write scope, by asking what
// the caller may READ — never more than a GET of the same record would have told them:
//   - the SAME level plus the manual-share clause reaches it → they see it only because it was shared
//     → ErrRecordSharedReadOnly (the share clause only widens owner/team levels, so only those probe);
//   - their VIEW scope reaches it → ErrRecordEditForbidden (issue #18);
//   - otherwise writeErr unchanged: the 404 stays a 404, also for a record deleted since the write.
//
// Admins skip both probes (a miss means the record is gone), and so does a write level of "all" (no
// view level is wider). A probe or ACL failure is returned wrapped rather than guessed at.
func (s *RecordService) sharedReadOnlyOr(ctx context.Context, writeScope RecordScope, tenantID, entity, id string, writeErr error) error {
	lvl := dynrepo.AccessLevel(writeScope.AccessLevel)
	if !errors.Is(writeErr, ErrRecordNotFound) || writeScope.IsAdmin || lvl == dynrepo.AccessAll {
		return writeErr
	}
	if lvl.IsOwnerScoped() {
		probe := writeScope.toRowScope()
		probe.IncludeShares = true
		if _, err := s.store.GetRecord(ctx, entity, tenantID, id, probe); err == nil {
			return ErrRecordSharedReadOnly
		} else if !errors.Is(err, ErrRecordNotFound) {
			return fmt.Errorf("services.RecordService.sharedReadOnlyOr: %w", err)
		}
	}
	if s.authz == nil {
		return writeErr
	}
	allowed, viewLevel, err := s.authz.Authorize(ctx, tenantID, writeScope.RoleID, entity, "view")
	if err != nil {
		return fmt.Errorf("services.RecordService.sharedReadOnlyOr: %w", err)
	}
	// A level equal to the write level was just probed (or is "none": 1=0) — nothing new to ask.
	if !allowed || viewLevel == writeScope.AccessLevel || dynrepo.AccessLevel(viewLevel) == dynrepo.AccessNone {
		return writeErr
	}
	view := writeScope // identity verbatim; only the level changes, with the share clause a view read carries
	view.AccessLevel, view.IncludeShares = viewLevel, true
	if _, err := s.store.GetRecord(ctx, entity, tenantID, id, view.toRowScope()); err != nil {
		if errors.Is(err, ErrRecordNotFound) {
			return writeErr
		}
		return fmt.Errorf("services.RecordService.sharedReadOnlyOr: %w", err)
	}
	return ErrRecordEditForbidden
}
