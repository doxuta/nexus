package services

import (
	"context"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/metaversion"
)

// emitMetaAudit records an admin-meta mutation (entity / field / role) to the RM-19 audit
// trail. The actor user id is read from the request context (placed by the admin handler via
// audit.WithActor); record_id stays empty (admin-meta mutations have no obj_* record). The
// call is async + fail-open inside the dispatcher, so it never blocks the admin flow. The
// auditor is never nil (services default it to a noop in their constructors).
//
// before/after are the metadata struct snapshots; nil before = create (after-only diff), nil
// after = delete (before-only diff). Structs are converted to maps so ComputeDiff reports only
// the fields that changed, matching the record-data diff semantics. tenantID pins the audit
// row tenant-scope like every other query.
func emitMetaAudit(ctx context.Context, a audit.Auditor, tenantID, metaEntity, action string, before, after any) {
	emitMetaAuditFor(ctx, a, tenantID, metaEntity, "", action, before, after)
}

// emitMetaAuditFor is emitMetaAudit for a meta mutation that DOES target one identifiable row
// (today: a user account — __user__). recordID lands in audit_log.record_id so the row names
// its target even though an update diff carries only the changed keys: a `{isAdmin:true}` or
// `{password:"[redacted]"}` row is attributable to the account it happened to, and
// GET /audit-log?entityName=__user__&recordId=<id> returns the whole account history alongside
// the W7 reset-link row (auth_password_reset.go), which already uses RecordID=userId.
func emitMetaAuditFor(ctx context.Context, a audit.Auditor, tenantID, metaEntity, recordID, action string, before, after any) {
	if a == nil { // a service built by struct literal in a test has no auditor; the trail is best-effort anyway
		return
	}
	a.Record(ctx, audit.Event{
		TenantID:    tenantID,
		ActorUserID: audit.ActorFromContext(ctx),
		EntityName:  metaEntity,
		RecordID:    recordID,
		Action:      action,
		Diff:        audit.ComputeDiff(audit.StructToMap(before), audit.StructToMap(after)),
		At:          time.Now().UTC(),
	})
}

// docWriteModer is the consumer-side slice of *DraftGate that emitDocAudit needs (specs/063
// DraftGate.WriteMode satisfies it). Declared here so the services that emit configuration-document
// audit rows never import the gate itself.
type docWriteModer interface {
	WriteMode(ctx context.Context, tenantID string) (metaversion.Mode, error)
}

// emitDocAudit is emitMetaAuditFor for a configuration DOCUMENT (layout today; workflow rule,
// report, dashboard, branding, app in L4). It is the single place that applies FR-022: a change
// that only reached the builder's draft is not yet the customer's business, so it leaves no audit
// row (the version history records it instead). Only ModeDraft skips; a nil mode, a live write and
// ANY WriteMode error still write — the service has already answered (or will answer) for the
// error, and a surplus audit row is safer than a missing one.
func emitDocAudit(ctx context.Context, a audit.Auditor, mode docWriteModer, tenantID, metaEntity, recordID, action string, before, after any) {
	if mode != nil {
		if m, err := mode.WriteMode(ctx, tenantID); err == nil && m == metaversion.ModeDraft {
			return
		}
	}
	emitMetaAuditFor(ctx, a, tenantID, metaEntity, recordID, action, before, after)
}

// priorDoc reads the stored document BEFORE a write so the audit row can carry its "before" side. The
// read is best-effort by design: the trail is fail-open, so an unreadable prior (a transient error,
// or an id that does not exist and will 404 on the write itself) yields nil and the write answers for
// itself — it must never turn a save that would have succeeded into an error.
func priorDoc[T any](get func() (*T, error)) *T {
	p, err := get()
	if err != nil {
		return nil
	}
	return p
}
