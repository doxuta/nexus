package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/workflow"
)

// workflowLiveRepo is the slice of the RAW (not draft-wrapped) workflow store the live-only paths of
// WorkflowService need: read a live rule and switch it off. Satisfied by *workflow.SQLRepository —
// main.go hands it the SAME variable the engine's loader reads, never the draft-aware wrapper.
type workflowLiveRepo interface {
	Get(ctx context.Context, tenantID, id string) (*workflow.WorkflowRecord, error)
	SetInactive(ctx context.Context, tenantID, id, updatedBy string) error
}

// WithLiveRepo wires the raw store behind every path that must touch the LIVE rule whatever the
// draft layer says (today: DisableNow). One setter, one field: the next live-only path adds its
// method to workflowLiveRepo instead of introducing a second way in. Chainable.
func (s *WorkflowService) WithLiveRepo(r workflowLiveRepo) *WorkflowService {
	s.liveRepo = r
	return s
}

// WithRecorder arms DisableNow's direct version entry with the workflow document adapter. Chainable.
func (s *WorkflowService) WithRecorder(r *VersionRecorder, ad metaversion.DocAdapter) *WorkflowService {
	s.rec, s.recAdapter = r, ad
	return s
}

// DisableNow switches a LIVE rule off at once ("Tắt ngay cho khách", specs/063 FR-017) without
// publishing the other drafts: the one way a builder writes a rule of a delivered workspace
// directly, and it can only turn a rule OFF — never on, never edit it. It neither reads nor writes
// the draft table: if the rule has a draft, the live document now differs from the draft's base, so
// the next publish reports PUBLISH_CONFLICT on exactly that rule and the builder chooses. A rule that
// exists only as a draft is not live, so it is WORKFLOW_NOT_FOUND here. Returns the live record.
func (s *WorkflowService) DisableNow(ctx context.Context, tenantID, id, actorID string) (*workflow.WorkflowRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("WORKFLOW_ID_REQUIRED", "id workflow là bắt buộc")
	}
	if s.liveRepo == nil { // a wiring bug must be loud: falling back to s.repo would hit the draft layer
		return nil, errors.New("services.WorkflowService.DisableNow: live repository not configured")
	}
	prior, err := s.liveRepo.Get(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, workflow.ErrWorkflowNotFound) {
			return nil, invalid("WORKFLOW_NOT_FOUND", "không tìm thấy workflow")
		}
		return nil, fmt.Errorf("services.WorkflowService.DisableNow: load: %w", err)
	}
	wasActive, beforeSnap := prior.IsActive, workflowAuditSnapshot(prior) // before the write
	// The write is LIVE whatever the caller's draft mode, so it always leaves a direct entry (S7-5).
	if err := s.rec.TraceLive(ctx, metaversion.ModeLiveDirect, s.recAdapter, tenantID, "update", id, func() (string, error) {
		return id, s.liveRepo.SetInactive(ctx, tenantID, id, strings.TrimSpace(actorID))
	}); err != nil {
		if errors.Is(err, workflow.ErrWorkflowNotFound) {
			return nil, invalid("WORKFLOW_NOT_FOUND", "không tìm thấy workflow")
		}
		return nil, fmt.Errorf("services.WorkflowService.DisableNow: %w", err)
	}
	// After the write commits, like every other write of this service: the engine stops running it now.
	s.cache.Invalidate(tenantID, prior.EntityName)
	rec, err := s.liveRepo.Get(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("services.WorkflowService.DisableNow: reload: %w", err)
	}
	// The write is LIVE whatever the caller's draft mode, so it is audited directly (emitMetaAuditFor,
	// not emitDocAudit), and only when it actually switched an active rule off.
	if wasActive {
		emitMetaAuditFor(ctx, s.auditor, tenantID, audit.WorkflowRuleMeta, id, audit.ActionUpdate,
			beforeSnap, workflowAuditSnapshot(rec))
	}
	return rec, nil
}
