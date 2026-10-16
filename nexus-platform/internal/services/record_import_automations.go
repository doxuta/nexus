package services

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/workflow"
)

// fireImportAutomations emits the record's after_create event to the workflow dispatcher and the BPM
// engine, owned by the importer (Depth 0, like a person's create). Delivery is at-least-once: the
// workflow event is emitted first, so when the BPM emit (or the audit marker the caller writes next)
// fails, the retry emits the workflow event again for the same record (spec FR-035). A consumer that
// is not wired fails the job before the record is marked handled, never silently skipped.
func fireImportAutomations(ctx context.Context, a ImportAutomation, tenantID string, p ImportEventsPayload, id string, row map[string]any, logger *zap.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, importEmitTimeout)
	defer cancel()
	if a.Workflow == nil || a.BPM == nil {
		logger.Error("services.fireImportAutomations: automations requested but a consumer is not wired",
			zap.String("tenant_id", tenantID), zap.String("entity_name", p.EntityName))
		return jobs.Permanent(fmt.Errorf("services.fireImportAutomations: automation consumer not wired"))
	}
	if err := a.Workflow.EmitWait(ctx, workflow.Event{
		TenantID: tenantID, ActorUserID: p.UserID, EntityName: p.EntityName, RecordID: id,
		Trigger: workflow.TriggerAfterCreate, Data: cloneData(row), OwnerUserID: p.UserID,
		CreatedAt: time.Now().UTC(), // ponytail: job time, not created_at; set_elapsed rules anchor late on a delayed job
	}); err != nil {
		return fmt.Errorf("workflow: %w", err)
	}
	if err := a.BPM.EmitWait(ctx, bpm.Event{
		TenantID: tenantID, ActorUserID: p.UserID, EntityName: p.EntityName, RecordID: id,
		Trigger: bpm.TriggerAfterCreate, Data: cloneData(row),
	}); err != nil {
		return fmt.Errorf("bpm: %w", err)
	}
	return nil
}
