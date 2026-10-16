package services

import (
	"context"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/bpm"
)

// approvalActions are the audit_log.action values written for approval decisions. They extend the
// CRUD action vocabulary (create/update/delete/restore) with the BPM decision kinds so the audit
// trail records who approved/rejected/escalated which record, when, and why (spec §EPIC 5
// "Process/Approval history & audit trail").
const (
	auditActionApprove  = "approve"
	auditActionReject   = "reject"
	auditActionEscalate = "escalate"
	// auditActionComplete: a task step (not an approval) was completed (specs/043 S5).
	auditActionComplete = "complete"
)

// ApprovalAuditAdapter bridges the bpm.Recorder interface to the real internal/audit.Auditor, so
// every approval decision lands in the same append-only audit_log as record mutations. It is
// async + fail-open via the underlying Auditor (a write problem never blocks the decision). The
// decision's step + comment, the process id and the executed definition are carried in the
// diff's After map for the audit reader.
type ApprovalAuditAdapter struct {
	auditor audit.Auditor
}

// NewApprovalAuditAdapter wraps an Auditor as a bpm.Recorder. A nil auditor yields a no-op adapter.
func NewApprovalAuditAdapter(auditor audit.Auditor) *ApprovalAuditAdapter {
	if auditor == nil {
		auditor = audit.NewNoopAuditor()
	}
	return &ApprovalAuditAdapter{auditor: auditor}
}

// RecordApprovalDecision maps a bpm approval decision to an audit Event and records it.
func (a *ApprovalAuditAdapter) RecordApprovalDecision(ctx context.Context, ev bpm.ApprovalAuditEvent) {
	action := auditActionApprove
	switch ev.Action {
	case bpm.HistoryRejected:
		action = auditActionReject
	case bpm.HistoryEscalated:
		action = auditActionEscalate
	case bpm.HistoryCompleted, bpm.HistoryResubmitted:
		action = auditActionComplete
	}
	after := map[string]any{
		"approvalInstanceId": ev.InstanceID,
		"processId":          ev.ProcessID,
		"step":               ev.Step,
		"comment":            ev.Comment,
	}
	// The chain `step` indexes into, stored IN the row (DAILY-QUEUE #70): the instance's pinned
	// snapshot is not a durable record — purge the row or roll back 000065 and "approved step 1"
	// no longer says which step that was. Absent (not null) when unknown: an SLA escalation of an
	// instance armed before 000065 has no pin and the scheduler reads no live process.
	//
	// Steps only, never the outcome: non-admins read this row through the record History tab,
	// whose field-ACL filter strips hidden top-level field keys but cannot see inside
	// `definition` — an outcome there would name a hidden field and the value written into it.
	// The write-back is its own record-update audit row, redacted like any other edit.
	if ev.Definition != nil {
		// Chain(): the steps `step` indexes into in either storage form (a graph's nodes, S3a2).
		after["definition"] = bpm.Definition{Steps: ev.Definition.Chain()}
		// Which published version of the process that chain belongs to (specs/043 S10, #70). Absent
		// when the instance was pinned before versions existed — never a guessed "1".
		if ev.Definition.Version > 0 {
			after["processVersion"] = ev.Definition.Version
		}
	}
	a.auditor.Record(ctx, audit.Event{
		TenantID:    ev.TenantID,
		ActorUserID: ev.ActorUserID,
		EntityName:  ev.EntityName,
		RecordID:    ev.RecordID,
		Action:      action,
		Diff:        &audit.Diff{After: after},
		At:          ev.At,
	})
}
