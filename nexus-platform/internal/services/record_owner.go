package services

import (
	"context"
	"errors"
	"fmt"
	"go.uber.org/zap"
	"nexus/platform/internal/workflow"
	"strings"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
)

// SetOwner reassigns a record's CURRENT owner — TRỤ 2 mục 2 "Giao việc & đổi chủ"
// (docs/NEXUS-MOT-XUONG-2026-07-29.md §3d mục 2): PUT /:entity/:id/owner.
//
// Authorization (stricter than the route's own "edit" ACL gate): the caller must hold the
// entity's "edit" grant at access_level "all" (a manager role) — or be admin. A regular staff
// member whose "edit" grant is scoped to "owner"/"team" is refused 403 FORBIDDEN even over a
// record they themselves currently own, because reassignment is a management action, not an
// ordinary edit (spec: "chỉ người có quyền edit ở mức all... Người thường -> 403"). The route's
// RequireACL middleware (PUT → "edit") already keeps out a caller with NO edit grant at all;
// this is an ADDITIONAL check on top, not a replacement.
//
// newOwnerID must be the id of a LIVE, ACTIVE user of the SAME tenant — verified via the SAME
// BatchActiveUserExists primitive the `user` field type's write-time existence probe uses
// (checkUserTargets, record_orchestration.go), so "who counts as a valid owner" can never drift
// between the two. An unknown / cross-tenant / disabled id is rejected 422 RECORD_OWNER_INVALID
// (details.field="userId") — the SAME anti-enumeration shape as RECORD_USER_INVALID (one code
// for every "doesn't count" reason).
//
// created_by is NEVER touched (services/record_owner.go writes ONLY owner_id via
// dynrepo.Repository.SetOwner) — see rowscope.go's rowFilter doc for why repurposing created_by
// would be dangerous: it is documented+relied-on as the record's immutable creator and feeds the
// workflow engine's "$owner" notification-recipient sentinel.
//
// CLOSED 2026-07-29 (was a KNOWN FOLLOW-UP here): "$owner" used to resolve to rec.CreatedBy, so a
// reassigned record kept notifying its ORIGINAL CREATOR. dynrepo.Record now carries a resolved
// OwnerID (COALESCE(owner_id, created_by), folded in recordRow.toRecord and selected by both
// GetRecord and ListRecords), and every consumer reads it: the Update/Delete emitters here, the
// workflow scheduler's ScheduledRecord, the timer, and the dry-run preview. One column, one
// meaning, everywhere — including the screen, which never showed an owner at all before.
//
// Related, same day: a workflow `assign` action now ALSO calls SetOwner (via the engine's
// RecordMutator, not this method — see workflow_wiring.go's note on why the management-permission
// check above is a rule about human callers). Founder's rule: giving someone work gives them the
// right to edit it.
func (s *RecordService) SetOwner(ctx context.Context, scope RecordScope, tenantID, entityName, id, newOwnerID string) error {
	e, _, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return err
	}

	if !scope.IsAdmin && scope.AccessLevel != "all" {
		return invalid("FORBIDDEN",
			"chỉ quản lý (quyền sửa ở mức toàn bộ) hoặc quản trị viên mới được đổi chủ bản ghi")
	}

	newOwnerID = strings.TrimSpace(newOwnerID)
	if newOwnerID == "" {
		return invalidField("RECORD_OWNER_INVALID", "userId", "userId là bắt buộc")
	}
	if s.userStore == nil {
		return invalid("RECORD_USER_UNAVAILABLE",
			"kiểm tra người dùng hiện không khả dụng — vui lòng thử lại sau")
	}
	found, err := s.userStore.BatchActiveUserExists(ctx, tenantID, []string{newOwnerID})
	if err != nil {
		return fmt.Errorf("services.RecordService.SetOwner: batch probe: %w", err)
	}
	if !found[newOwnerID] {
		return invalidField("RECORD_OWNER_INVALID", "userId",
			"userId phải là id của một người dùng đang hoạt động trong tổ chức")
	}

	if err := s.store.SetOwner(ctx, e.Name, tenantID, id, scope.UserID, scope.toRowScope(), newOwnerID); err != nil {
		if errors.Is(err, dynrepo.ErrRecordNotFound) {
			return ErrRecordNotFound
		}
		if errors.Is(err, ErrWriteConflict) {
			return err
		}
		return fmt.Errorf("services.RecordService.SetOwner: %w", err)
	}

	// Tell the person they were given work. Handing someone a record changed WHOSE list it is and
	// left them no way to find out: the audit row records it for whoever goes looking, and the
	// only reason the inbox has a "Được giao việc" filter is that it was expected to. A silent
	// assignment is work nobody knows they own.
	//
	// Best-effort: the ownership change is committed and correct; a notification failure must not
	// undo it or surface as an error on an action that succeeded.
	if s.inbox != nil && newOwnerID != scope.UserID {
		if _, nerr := s.inbox.InsertNotification(ctx, workflow.Notification{
			TenantID:   tenantID,
			UserID:     newOwnerID,
			Title:      "Bạn được giao một bản ghi",
			Type:       "assignment",
			Message:    fmt.Sprintf("Một bản ghi %s vừa được giao cho bạn phụ trách.", e.Label),
			EntityName: e.Name,
			RecordID:   id,
		}); nerr != nil && s.logger != nil {
			s.logger.Warn("services.RecordService.SetOwner: assignment notice failed (fail-open)",
				zap.String("tenant_id", tenantID), zap.String("record_id", id), zap.Error(nerr))
		}
	}

	// RM-19: this is a management action over someone else's record, not an ordinary field
	// edit — worth its own audit row even though the diff is a single logical field.
	s.auditor.Record(ctx, audit.Event{
		TenantID:    tenantID,
		ActorUserID: scope.UserID,
		EntityName:  e.Name,
		RecordID:    id,
		Action:      audit.ActionUpdate,
		Diff:        audit.ComputeDiff(nil, map[string]any{"ownerId": newOwnerID}),
		At:          time.Now().UTC(),
	})
	return nil
}
