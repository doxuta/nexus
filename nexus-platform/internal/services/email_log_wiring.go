package services

import (
	"context"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/workflow"
)

// logEmailSkip writes the row of a rule email that never reached SendEmail (specs/066 L3): the
// template did not render or the recipient rendered empty. A terminal "failed" row with NO address
// and no attempt, carrying the rule/record coordinates so Resend can rebuild the mail once the
// record is fixed. reason is the text the run history got (it names the template, never data).
// Fail-open like every log write; no tenant means no row.
func logEmailSkip(ctx context.Context, log emailLogWriter, logger *zap.Logger, reason string) {
	if log == nil {
		return
	}
	e, ok := emailLogEntry(ctx, "", emaillog.StatusFailed)
	if !ok {
		return
	}
	e.LastError, e.Finished = reason, true
	if err := log.Insert(ctx, e); err != nil {
		logger.Warn("email log: skip row insert failed", zap.String("tenant_id", e.TenantID),
			zap.String("workflow_id", e.WorkflowID), zap.Error(err))
	}
}

// RecordEmailSkipped implements workflow.EmailSkipRecorder for the direct sender.
func (s *WorkflowEmailSender) RecordEmailSkipped(ctx context.Context, reason string) {
	logEmailSkip(ctx, s.log, s.logger, reason)
}

// RecordEmailSkipped implements workflow.EmailSkipRecorder for the queue sender.
func (s *EnqueuingWorkflowEmailSender) RecordEmailSkipped(ctx context.Context, reason string) {
	logEmailSkip(ctx, s.log, s.logger, reason)
}

var (
	_ workflow.EmailSkipRecorder = (*WorkflowEmailSender)(nil)
	_ workflow.EmailSkipRecorder = (*EnqueuingWorkflowEmailSender)(nil)
)

// emailLogWriter is the consumer-side slice of the email log the senders and the queue handler
// write through (specs/066). Satisfied by *emaillog.Repository; every method pins the tenant.
type emailLogWriter interface {
	Insert(ctx context.Context, e emaillog.Entry) error
	AttachJob(ctx context.Context, tenantID, id, jobID string) error
	MarkAttempt(ctx context.Context, tenantID, id string, attempts int, lastErr string) error
	MarkSent(ctx context.Context, tenantID, id string, attempts int) error
	MarkFailed(ctx context.Context, tenantID, id string, attempts int, lastErr string) error
}

// emailTenant is the tenant a mail belongs to: the recorded run's, else the origin's (FR-013 — a
// mail with either is never an untenanted system job). "" = neither (a direct call, the dry-run).
func emailTenant(ctx context.Context) string {
	if ref, ok := workflow.RunRefFrom(ctx); ok && ref.TenantID != "" {
		return ref.TenantID
	}
	if o, ok := workflow.EmailOriginFrom(ctx); ok {
		return o.TenantID
	}
	return ""
}

// emailLogEntry builds the log row for one mail from what ctx knows — config ids, the masked
// recipient and the subject TEMPLATE, never the rendered subject or body (specs/066 D-2). The row's
// id is the origin's pre-minted LogID (a resend) or a fresh uuid. ok=false when the mail has no
// tenant: nothing could ever read or clean such a row.
func emailLogEntry(ctx context.Context, to, status string) (emaillog.Entry, bool) {
	tenant := emailTenant(ctx)
	if tenant == "" {
		return emaillog.Entry{}, false
	}
	origin, _ := workflow.EmailOriginFrom(ctx)
	ref, inRun := workflow.RunRefFrom(ctx)
	e := emaillog.Entry{
		ID: origin.LogID, TenantID: tenant, Source: origin.Source, Status: status,
		RecipientMasked: workflow.MaskRecipients(to), SubjectTemplate: origin.SubjectTemplate,
		WorkflowID: origin.WorkflowID, ActionHash: origin.ActionHash, EntityName: origin.EntityName,
		RecordID: origin.RecordID, UserID: origin.UserID, ResendOf: origin.ResendOf, CreatedBy: origin.CreatedBy,
	}
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.Source == "" {
		e.Source = emaillog.SourceWorkflow
	}
	idx := origin.ActionIndex
	if inRun {
		e.RunID = ref.RunID
		if e.WorkflowID == "" {
			e.WorkflowID = ref.WorkflowID
		}
		if origin.WorkflowID == "" {
			idx = ref.ActionIndex
		}
	}
	if e.WorkflowID != "" {
		e.ActionIndex = &idx
	}
	return e, true
}
