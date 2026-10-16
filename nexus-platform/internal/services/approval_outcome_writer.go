package services

import (
	"context"
	"fmt"

	"nexus/platform/internal/bpm"
)

// ApprovalOutcomeWriter applies an approval's terminal write-back to the decided record.
//
// It runs with a SYSTEM scope (IsAdmin, access "all") for the same reason the payment-match writer
// does: the decision was already authorized — by the approver, against the step's approver rules —
// and the person who may sign off a quote is very often NOT the person with edit rights on it.
// Re-checking the approver's own record permissions here would make "approved" fail for exactly
// the managers approvals exist to involve.
//
// It still goes through RecordService.Update, so every VALUE rule holds: a picklist option that
// does not exist is refused, a computed field cannot be overwritten, and the change is versioned
// and audited like any other write. Only the row-visibility check is bypassed, never the validation.
type ApprovalOutcomeWriter struct {
	rec *RecordService
}

// NewApprovalOutcomeWriter wires the writer over the record service.
func NewApprovalOutcomeWriter(rec *RecordService) *ApprovalOutcomeWriter {
	return &ApprovalOutcomeWriter{rec: rec}
}

// SetField writes one field on one record. Satisfies bpm.RecordWriter.
func (w *ApprovalOutcomeWriter) SetField(ctx context.Context, tenantID, entityName, recordID, field string, value any, actorUserID string) error {
	if w == nil || w.rec == nil {
		return fmt.Errorf("services.ApprovalOutcomeWriter: record service not wired")
	}
	// UserID is the APPROVER, not blank. RecordService.Update writes it as updated_by and as the
	// audit event's actor, so leaving it empty would file every approval write-back as an
	// anonymous system change in the record's history — the one screen where "who decided this"
	// is the whole question. The payment-match writer threads its identity for the same reason.
	scope := RecordScope{UserID: actorUserID, AccessLevel: "all", IsAdmin: true}
	// specs/048 FR-019: the decision is final, so a before_save veto must not undo it — otherwise a
	// "only through approval" rule leaves the approval approved and the record unchanged.
	ctx = withSystemWriteOrigin(ctx, originApprovalOutcome)
	if err := w.rec.Update(ctx, scope, tenantID, entityName, recordID, map[string]any{field: value}, nil); err != nil {
		return fmt.Errorf("services.ApprovalOutcomeWriter.SetField: %w", err)
	}
	return nil
}

// SetFields writes several fields onto one record in ONE update — an auto node's whole `set`
// (specs/043 S7), all or nothing. Same system scope and same value rules as SetField; actorUserID is
// the engine's system actor, so the record's history says "the process did this", not a person.
//
// Like the outcome write-back it is exempt from before_save vetoes (FR-019): a rule that says "this
// status may only change through the approval" must not stall the very step the approval takes.
// Every VALUE rule still holds (picklist membership, computed-field refusal), so a wrong value is a
// stalled step with a readable reason and a "Thử lại", never a corrupted record. Satisfies bpm.FieldsWriter.
func (w *ApprovalOutcomeWriter) SetFields(ctx context.Context, tenantID, entityName, recordID string, fields map[string]any, actorUserID string) error {
	if w == nil || w.rec == nil {
		return fmt.Errorf("services.ApprovalOutcomeWriter: record service not wired")
	}
	scope := RecordScope{UserID: actorUserID, AccessLevel: "all", IsAdmin: true}
	ctx = withSystemWriteOrigin(ctx, originApprovalOutcome)
	if err := w.rec.Update(ctx, scope, tenantID, entityName, recordID, fields, nil); err != nil {
		return fmt.Errorf("services.ApprovalOutcomeWriter.SetFields: %w", err)
	}
	return nil
}

// ApprovalRecordReader reads a record's CURRENT values for a gateway (specs/043 S7). System scope:
// routing is a system decision on the record's true state, not on what the person who happened to
// press a button may see (the same reason SubmitForApproval evaluates criteria on a system read).
// tenantID pins the read.
type ApprovalRecordReader struct {
	rec *RecordService
}

// NewApprovalRecordReader wires the reader over the record service.
func NewApprovalRecordReader(rec *RecordService) *ApprovalRecordReader {
	return &ApprovalRecordReader{rec: rec}
}

// GetRecord returns the record's field map. Satisfies bpm.RecordReader.
func (r *ApprovalRecordReader) GetRecord(ctx context.Context, tenantID, entityName, recordID string) (map[string]any, error) {
	if r == nil || r.rec == nil {
		return nil, fmt.Errorf("services.ApprovalRecordReader: record service not wired")
	}
	rec, err := r.rec.Get(ctx, RecordScope{IsAdmin: true, AccessLevel: "all"}, tenantID, entityName, recordID, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalRecordReader.GetRecord: %w", err)
	}
	return rec.Data, nil
}

var (
	_ bpm.RecordWriter = (*ApprovalOutcomeWriter)(nil)
	_ bpm.FieldsWriter = (*ApprovalOutcomeWriter)(nil)
	_ bpm.RecordReader = (*ApprovalRecordReader)(nil)
)
