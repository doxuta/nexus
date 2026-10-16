package services

// pii-sensitive-audit-redact (queue #19) — write-side PII redaction for the audit trail, the
// second half of the stance ../specs/029-audit-log/spec.md §5 used to defer to "Đợt-2".
//
// audit_log is append-only and has no field-level ACL of its own, so any value copied into
// audit_log.diff is a second, unguarded home for that value — exactly the finding that fails a
// medical/financial data review for fields like số CCCD, mã bệnh án or a bank account number.
// An admin marks such a field `options.sensitive = true` (Studio → "Trường nhạy cảm"; the JSON
// options column already exists, so no migration), and from that moment the engine writes the
// marker instead of the value on every record mutation it audits.
//
// Bound (honest scope): this covers the RecordService mutation seams — create (single + bulk),
// update, delete, restore. The CSV import path emits NO audit rows at all today
// (`grep audit internal/services/record_import.go` → 0), so imported rows leave no trail to
// redact; that is a separate specs/029 gap, not something this helper reaches.

import (
	"context"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/metadata"
)

// deletedFieldLister is the optional capability a catalog advertises when it can return
// soft-deleted field definitions too. *metadata.Repository has it (it backs the add-field
// dup-check), and MetaCache forwards it — so the production wiring
// (NewRecordService(metaCache, …)) takes the complete path. It is deliberately an optional
// interface rather than a widening of EntityCatalog: EntityCatalog is implemented by ~15 test
// fakes across four packages, and none of them has anything to say about soft-deleted fields.
type deletedFieldLister interface {
	ListFieldsIncludingDeleted(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}

// auditRedactedMarker is the placeholder written in place of a sensitive field's value. Same
// literal as the credential-rotation marker in user_service.go so one grep finds every masked
// value in the trail, not two competing shapes.
const auditRedactedMarker = "[redacted]"

// redactSensitiveDiff masks, IN PLACE, the value of every key in d that belongs to a field
// declared `options.sensitive = true`, on both the before and the after side. It returns d so
// a call site reads `Diff: redactSensitiveDiff(specs, audit.ComputeDiff(before, after))`.
//
// It runs AFTER the diff is computed, never on the input maps, and this ordering is the whole
// design. Masking before ComputeDiff would make before == after for a changed sensitive field;
// DeepEqual would call it unchanged and drop the key entirely (audit.go ComputeDiff), so the
// trail would no longer record that the field changed at all — strictly worse than storing the
// value, because the auditable event disappears. Masking after keeps the key (who changed what
// field, when) and removes only the value.
//
// Keys are only ever OVERWRITTEN, never added: ComputeDiff nils out an empty side for a
// one-sided diff (create/delete) and assigning into a nil map panics.
// redactionSpecs returns the field list to redact a RAW STORED BLOB against — the before-image
// of a delete, the read-back of a restore. Those two diffs are not bounded by a validated payload
// the way create and update are, so they carry every key sitting in obj_*.data, INCLUDING keys
// whose field definition has since been soft-deleted (deleting a field removes the definition and
// keeps the data). ListFields hides those definitions, so the sensitive flag would go invisible
// at exactly the moment the value is still present and about to be copied into an append-only
// table that the CSV export serves with no read-side field filter.
//
// Fail-safe on error and on a catalog without the capability: fall back to the live specs, which
// still redact every field the app currently declares — strictly better than no redaction, and
// audit is fail-open so this must never break a delete. But the fallback is NOT silent: taking it
// means this PII control is running degraded, and the wiring that causes it (a catalog that does
// not forward ListFieldsIncludingDeleted) is invisible to every unit test — the failure mode this
// whole function exists to close. The warn makes a future rewiring findable by log search; the
// acceptance script proves the good path on the real server.
func (s *RecordService) redactionSpecs(ctx context.Context, tenantID, entityID string, live []FieldSpec) []FieldSpec {
	return redactionSpecsFor(ctx, s.catalog, s.logger, tenantID, entityID, live)
}

// redactionSpecsFor is redactionSpecs over any catalog + logger, so the CSV import's audit job
// (specs/048 S7a) redacts a stored blob against EXACTLY the field list delete/restore use.
func redactionSpecsFor(ctx context.Context, catalog EntityCatalog, logger *zap.Logger, tenantID, entityID string, live []FieldSpec) []FieldSpec {
	lister, ok := catalog.(deletedFieldLister)
	if !ok {
		warnDegradedRedaction(logger, tenantID, entityID, "catalog does not expose ListFieldsIncludingDeleted")
		return live
	}
	all, err := lister.ListFieldsIncludingDeleted(ctx, tenantID, entityID)
	if err != nil {
		warnDegradedRedaction(logger, tenantID, entityID, "ListFieldsIncludingDeleted: "+err.Error())
		return live
	}
	specs, err := toFieldSpecs(all)
	if err != nil {
		warnDegradedRedaction(logger, tenantID, entityID, "toFieldSpecs: "+err.Error())
		return live
	}
	return specs
}

// warnDegradedRedaction reports that a delete/restore audit diff was redacted against the LIVE
// field list only, so a sensitive field that has since been soft-deleted may have had its value
// written to audit_log in the clear. Nil-logger safe (unit tests wire no logger).
func warnDegradedRedaction(logger *zap.Logger, tenantID, entityID, reason string) {
	if logger == nil {
		return
	}
	logger.Warn("services.redactionSpecs: PII redaction degraded to live fields only — a soft-deleted sensitive field may leak into audit_log",
		zap.String("tenant_id", tenantID), zap.String("entity_id", entityID), zap.String("reason", reason))
}

// auditUpdateDiff is the ONE rule for how a partial update is diffed into the trail, shared by the
// human PUT (RecordService.Update) and the workflow engine's machine write
// (WorkflowRecordMutator.UpdateField) so the two cannot drift: only the keys actually written, each
// against its prior value, sensitive values masked after the diff. before == nil (the before-image
// read failed) yields an after-only diff; nothing changed yields nil.
func auditUpdateDiff(specs []FieldSpec, before, written map[string]any) *audit.Diff {
	var beforeChanged map[string]any
	if before != nil {
		beforeChanged = make(map[string]any, len(written))
		for k := range written {
			if v, ok := before[k]; ok {
				beforeChanged[k] = v
			}
		}
	}
	return redactSensitiveDiff(specs, audit.ComputeDiff(beforeChanged, written))
}

func redactSensitiveDiff(specs []FieldSpec, d *audit.Diff) *audit.Diff {
	if d == nil {
		return nil
	}
	for _, sp := range specs {
		if !boolOpt(sp.Options, "sensitive") {
			continue
		}
		if _, ok := d.Before[sp.Name]; ok {
			d.Before[sp.Name] = auditRedactedMarker
		}
		if _, ok := d.After[sp.Name]; ok {
			d.After[sp.Name] = auditRedactedMarker
		}
	}
	return d
}
