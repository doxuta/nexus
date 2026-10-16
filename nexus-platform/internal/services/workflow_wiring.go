package services

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/notify"
	"nexus/platform/internal/workflow"
)

// WorkflowRecordMutator adapts the dynamic record store to the workflow engine's RecordMutator
// interface (EPIC 4 field_update action). The engine acts as a SYSTEM principal — it patches the
// triggering record under an admin row-scope so an automation is never blocked by the original
// caller's row-level access, and stamps a synthetic actor as modified_by. It performs a single
// JSON_MERGE_PATCH per field via the store's UpdateRecord (parameterized, tenant-pinned). The
// embedded store is the same *dynrepo.Repository the record service uses.
type WorkflowRecordMutator struct {
	store fieldUpdater
	// catalog arms the execution-time validation (defense in depth): before writing, the target
	// field + value are validated against sys_fields through ValidatePartialRecord — the SAME
	// validator the record PUT write-path uses. This refuses (a) an unknown field, (b) a
	// computed/assigned target (formula/lookup/rollup/auto_number — WF-01: a BAD rule that
	// predates the config-time guard can never corrupt data, auto_number especially since it has
	// no read gate and is never re-assigned on Update → a workflow overwrite is permanent), and
	// (c) a value of the wrong type/range for the field (WF-VALUE-TYPECHECK: a rule setting a
	// string into a number field would otherwise merge junk straight into obj_*.data via dynrepo,
	// which bypasses ValidateRecord). Optional: nil skips validation (legacy unit fakes);
	// production wires *MetaCache via WithCatalog, so the check reads through the TTL'd metadata
	// cache — no extra MySQL round-trip per action.
	catalog EntityCatalog
	// auditor records each machine write in audit_log (workflow-writes-leave-no-trail, queue #86).
	// Because this adapter bypasses RecordService it also bypassed RecordService's RM-19 emit, so
	// a field_update / assign / set_deadline changed a record and the trail had no row. nil =
	// disabled (legacy unit fakes); production wires the shared audit dispatcher via WithAuditor.
	auditor audit.Auditor
	// afterWrite runs after a committed machine write with the before/after row images — the seam
	// that keeps stored roll-up totals (specs/083) in step when an automation edits a child row.
	// nil = off (unit fakes); production wires RecordService.RefreshStoredRollups via WithAfterWrite.
	afterWrite func(ctx context.Context, tenantID, entity string, images ...map[string]any)
	// emit hands the after_update of a RELATED record written by a field_update via (#330, specs/058
	// §2.4) to the workflow dispatcher. nil = related writes stay silent (unit fakes).
	emit func(ctx context.Context, evt workflow.Event)
}

// fieldUpdater is the minimal slice of the dynamic record store the mutator needs. Satisfied by
// *dynrepo.Repository. Declared consumer-side so the adapter stays unit-testable with a fake.
type fieldUpdater interface {
	UpdateRecord(ctx context.Context, entity, tenantID, id, userID string, scope dynrepo.RowScope, data map[string]any, expectedVersion *int, uniqueFields []dynrepo.UniqueFieldSpec) error
	SetOwner(ctx context.Context, entity, tenantID, id, userID string, scope dynrepo.RowScope, newOwnerID string) error
}

// NewWorkflowRecordMutator wires the adapter over the dynamic record store.
func NewWorkflowRecordMutator(store fieldUpdater) *WorkflowRecordMutator {
	return &WorkflowRecordMutator{store: store}
}

// WithCatalog injects the metadata catalog that arms the execution-time field-target + value
// validation. Chainable; nil leaves validation disarmed (pre-WF-01 behaviour).
func (m *WorkflowRecordMutator) WithCatalog(c EntityCatalog) *WorkflowRecordMutator {
	m.catalog = c
	return m
}

// WithAuditor injects the audit sink the machine writes are recorded through. Chainable; nil
// leaves auditing off.
func (m *WorkflowRecordMutator) WithAuditor(a audit.Auditor) *WorkflowRecordMutator {
	m.auditor = a
	return m
}

// WithAfterWrite injects the post-write hook (specs/083). Chainable.
func (m *WorkflowRecordMutator) WithAfterWrite(f func(ctx context.Context, tenantID, entity string, images ...map[string]any)) *WorkflowRecordMutator {
	m.afterWrite = f
	return m
}

// WithEmitter injects the workflow event sink for cascading related writes (#330). Chainable.
func (m *WorkflowRecordMutator) WithEmitter(f func(ctx context.Context, evt workflow.Event)) *WorkflowRecordMutator {
	m.emit = f
	return m
}

// UpdateRelatedField is UpdateField for a record reached through a relation (workflow.RelatedWriter,
// #330): after the machine write it emits the target's after_update at chain depth `depth`, so the
// target entity's own rules run — and the depth guard in runRelatedFieldUpdate bounds A→B→A cycles.
// Nothing is emitted when the write changed nothing (a rule re-stamping the value already there must
// not re-fire the target's rules) or when the record cannot be read (fail-open, like every emit).
// ponytail: two extra admin-scope reads of one row per related write; fold into writeFields if hot.
func (m *WorkflowRecordMutator) UpdateRelatedField(ctx context.Context, entity, tenantID, recordID, systemActor, field string, value any, depth int) error {
	reader, _ := m.store.(recordReader)
	if m.emit == nil || reader == nil {
		return m.UpdateField(ctx, entity, tenantID, recordID, systemActor, field, value)
	}
	before, rerr := reader.GetRecord(ctx, entity, tenantID, recordID, dynrepo.AdminScope())
	if err := m.UpdateField(ctx, entity, tenantID, recordID, systemActor, field, value); err != nil {
		return err
	}
	if rerr != nil || before == nil {
		return nil // no before-image ⇒ no honest event; the write itself already landed
	}
	after, err := reader.GetRecord(ctx, entity, tenantID, recordID, dynrepo.AdminScope())
	if err != nil || after == nil || reflect.DeepEqual(before.Data, after.Data) {
		return nil
	}
	m.emit(ctx, workflow.Event{
		TenantID:    tenantID,
		ActorUserID: systemActor,
		EntityName:  entity,
		RecordID:    recordID,
		Trigger:     workflow.TriggerAfterUpdate,
		Data:        after.Data,
		Before:      before.Data,
		OwnerUserID: after.OwnerID,
		Depth:       depth,
		CreatedAt:   after.CreatedAt,
	})
	return nil
}

// UpdateField sets field=value on (entity, tenantID, recordID) using an admin row-scope (so RLS
// never blocks the automation) and the engine's synthetic system actor as modified_by. It passes
// expectedVersion=nil — an automation does not participate in optimistic-locking (it reacts to an
// already-committed mutation, not a client edit). A not-found record (e.g. raced delete) surfaces
// as the store's error, which the dispatcher logs fail-open. When a catalog is wired the target
// field AND value are validated FIRST (validateFieldUpdate) — a computed/assigned or unknown
// target (WF-01), or a value of the wrong type/range for the field (WF-VALUE-TYPECHECK), errors
// out (logged fail-open by the dispatcher, recorded as a FAILED run per W6) instead of merging
// junk into obj_*.data.
func (m *WorkflowRecordMutator) UpdateField(ctx context.Context, entity, tenantID, recordID, systemActor, field string, value any) error {
	if m == nil || m.store == nil {
		return fmt.Errorf("services.WorkflowRecordMutator.UpdateField: no store wired")
	}
	return m.writeFields(ctx, "UpdateField", entity, tenantID, recordID, systemActor, map[string]any{field: value}, nil)
}

// ReadForUpdate returns the record's current data and optimistic-lock version under an admin
// scope — what a tighten_only set_deadline decides from (DAILY-QUEUE #172).
func (m *WorkflowRecordMutator) ReadForUpdate(ctx context.Context, entity, tenantID, recordID string) (map[string]any, int, error) {
	var reader recordReader
	if m != nil {
		reader, _ = m.store.(recordReader)
	}
	if reader == nil {
		return nil, 0, fmt.Errorf("services.WorkflowRecordMutator.ReadForUpdate: no record reader wired")
	}
	rec, err := reader.GetRecord(ctx, entity, tenantID, recordID, dynrepo.AdminScope())
	if err != nil {
		return nil, 0, fmt.Errorf("services.WorkflowRecordMutator.ReadForUpdate: %w", err)
	}
	return rec.Data, rec.Version, nil
}

// UpdateFieldsIfVersion writes every field in ONE store update carrying the RM-24 version
// predicate: a write that bumped the version since ReadForUpdate makes it match nothing, and the
// store's *VersionConflictError comes back as workflow.ErrRecordChanged (nothing written). Each
// field is validated exactly as UpdateField validates it.
func (m *WorkflowRecordMutator) UpdateFieldsIfVersion(ctx context.Context, entity, tenantID, recordID, systemActor string, fields map[string]any, version int) error {
	if m == nil || m.store == nil {
		return fmt.Errorf("services.WorkflowRecordMutator.UpdateFieldsIfVersion: no store wired")
	}
	err := m.writeFields(ctx, "UpdateFieldsIfVersion", entity, tenantID, recordID, systemActor, fields, &version)
	var vc *dynrepo.VersionConflictError
	if errors.As(err, &vc) {
		return workflow.ErrRecordChanged
	}
	return err
}

// writeFields is the one machine write both entry points share: validate each field (validation
// errors come back unwrapped, as UpdateField always returned them), fold formulas, write once —
// conditionally when expectedVersion is set — and audit the diff. op names the caller in a store
// error.
func (m *WorkflowRecordMutator) writeFields(ctx context.Context, op, entity, tenantID, recordID, systemActor string, fields map[string]any, expectedVersion *int) error {
	data := map[string]any{}
	var specs []FieldSpec
	var uniqueFields []dynrepo.UniqueFieldSpec
	for field, value := range fields {
		d, sp, uf, err := m.validateFieldUpdate(ctx, entity, tenantID, field, value)
		if err != nil {
			return err
		}
		for k, v := range d {
			data[k] = v
		}
		specs, uniqueFields = sp, uf // same entity: every call resolves the same specs
	}
	if len(data) == 0 {
		// The value validated but produced nothing to write — an optional field set to an empty,
		// non-null value is a no-op under partial-update (PUT) semantics. Skip the store write.
		return nil
	}
	// FORMULA-RECOMPUTE (follow-up #83): fold any formula field whose input this automation just
	// changed back into the SAME write so it is not left stale in storage. Because a field_update
	// bypasses RecordService (see below), the formula recompute RecordService.Update runs never
	// happened for this write — so a formula like line_total = quantity * unit_price stayed at its
	// old value after a workflow edited quantity, and any rollup summing it (quote.total) then read
	// a stale sum until the next client write. recomputeFormulas mutates `data` in place; it emits
	// NOTHING (only rewrites data via the single store.UpdateRecord below), keeping writeFields itself
	// event-free. The cascade (#330) lives in UpdateRelatedField, which emits after_update with
	// Depth+1 only for a via write; a plain UpdateField still emits nothing.
	//
	// ONE before-image read serves both the recompute and the audit diff (queue #86), and it is
	// skipped entirely when neither needs it (no auditor wired, no formula field).
	var before map[string]any
	haveBefore := false
	if m.auditor != nil || m.afterWrite != nil || hasFormulaField(specs) || hasDependentPicklist(specs) {
		before, haveBefore = m.beforeImage(ctx, entity, tenantID, recordID)
	}
	// Queue #229 (specs/052): each field above was judged alone, so a rule that moved the parent
	// picklist could leave the child of the old parent behind. Judge the pairs the write forms
	// against the before-image, as a user's PUT does. An unreadable record judges the write alone:
	// a write carrying both keys is still judged, but a parent-only write passes (child "" — fail
	// OPEN; the record read is best-effort, as for formulas and the audit diff).
	if hasDependentPicklist(specs) {
		if err := checkDependentPicklists(specs, data, before); err != nil {
			var ve *ValidationError
			if errors.As(err, &ve) {
				return fmt.Errorf("services.WorkflowRecordMutator.%s: dependent picklist rejected (%s): %w", op, ve.Code, err)
			}
			return fmt.Errorf("services.WorkflowRecordMutator.%s: %w", op, err)
		}
	}
	if haveBefore {
		recomputeFormulas(specs, before, data)
	}
	// RM-28: this is the workflow field_update BYPASS of RecordService — the repo choke-point still
	// maintains the pivot, but it needs the entity's unique fields, which validateFieldUpdate
	// resolved from the catalog. A nil catalog (legacy fakes) yields nil uniqueFields (no pivot).
	if err := m.store.UpdateRecord(ctx, entity, tenantID, recordID, systemActor, dynrepo.AdminScope(), data, expectedVersion, uniqueFields); err != nil {
		return fmt.Errorf("services.WorkflowRecordMutator.%s: %w", op, err)
	}
	if m.afterWrite != nil {
		m.afterWrite(ctx, tenantID, entity, before, data) // specs/083: an automation moved a child row
	}
	// Queue #86 (workflow-writes-leave-no-trail): bypassing RecordService used to bypass its RM-19
	// emit too, so a machine write changed a record and the trail had no row. Same diff rule as a
	// human PUT (auditUpdateDiff), actor = the engine's system actor. A write that changed nothing
	// — the rule re-stamped the value already there, as an after_update rule does on every later
	// edit — has a nil diff and records no row; a row per firing would bury the real changes.
	if m.auditor != nil {
		if diff := auditUpdateDiff(specs, before, data); diff != nil {
			m.auditor.Record(ctx, audit.Event{
				TenantID:    tenantID,
				ActorUserID: systemActor,
				EntityName:  entity,
				RecordID:    recordID,
				Action:      audit.ActionUpdate,
				Diff:        diff,
				At:          time.Now().UTC(),
			})
		}
	}
	return nil
}

// recordReader is the minimal read slice the formula recompute and the audit diff need: fetch the
// triggering record's current data so a partial field_update can recompute formulas against the
// FULL record image (a formula may reference fields this action did not touch) and diff the written
// keys against their prior values. Satisfied by *dynrepo.Repository. Declared consumer-side (mirrors
// relationExister) so a legacy unit fake without GetRecord simply disarms both — the same
// nil-catalog disarm convention this adapter already uses.
type recordReader interface {
	GetRecord(ctx context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error)
}

// beforeImage reads the record's current data under an ADMIN scope (the engine is a system
// principal, exactly like the write itself). ok=false when the store cannot read (legacy fake) or
// the read failed — callers then skip the recompute and diff after-only, see recomputeFormulas.
func (m *WorkflowRecordMutator) beforeImage(ctx context.Context, entity, tenantID, recordID string) (map[string]any, bool) {
	reader, ok := m.store.(recordReader)
	if !ok {
		return nil, false
	}
	rec, err := reader.GetRecord(ctx, entity, tenantID, recordID, dynrepo.AdminScope())
	if err != nil || rec == nil {
		return nil, false
	}
	return rec.Data, true
}

// recomputeFormulas folds the entity's formula fields into `data` (in place) so a field_update /
// assign that changed a formula's INPUT does not leave the formula stale. It mirrors
// RecordService.Update's recompute — merge the before-image with the patch, applyComputedFields,
// copy the formula results back into the patch. It resolves ONLY formula fields
// (compute-at-WRITE, persisted); rollup/lookup are compute-at-READ, so a rollup on the PARENT that
// sums this record's now-fresh formula value is correct on its next read with NO parent write.
//
// A no-op when the entity declares no formula field. One DELIBERATE divergence from
// RecordService.Update: the caller only invokes this with a before-image it actually READ — on a
// GetRecord failure the recompute is SKIPPED (formula fields stay out of the patch, so storage keeps
// their current values) rather than computed from the partial patch alone. A field_update is ALWAYS
// a single-field partial write, so computing from it alone would resolve the formula's other inputs
// to 0 and persist a CORRUPTED value (e.g. line_total = quantity * 0). Skipping is strictly safer —
// no worse than the pre-fix stale value.
func recomputeFormulas(specs []FieldSpec, before, data map[string]any) {
	if !hasFormulaField(specs) {
		return
	}
	merged := mergeAfterImage(before, data)
	applyComputedFields(specs, merged)
	// Copy EVERY formula result from merged into the patch (including a nil div-by-zero result,
	// mirroring RecordService.Update) so the two write paths cannot drift.
	for _, sp := range specs {
		if sp.FieldType == "formula" {
			data[sp.Name] = merged[sp.Name]
		}
	}
}

// hasFormulaField reports whether specs declares at least one formula field, so recomputeFormulas
// can short-circuit (and skip the before-image read) for entities that have none.
func hasFormulaField(specs []FieldSpec) bool {
	for _, sp := range specs {
		if sp.FieldType == "formula" {
			return true
		}
	}
	return false
}

// validateFieldUpdate resolves the entity's field specs through the catalog and validates the
// single field=value pair with ValidatePartialRecord — the SAME validator the record PUT
// write-path uses (record_orchestration Update). It returns the cleaned, type-normalized data map
// to merge. This is one source of truth for BOTH guards at the workflow execution seam, inherited
// from ValidatePartialRecord (no duplication):
//
//   - WF-01 (defense in depth): an unknown target (RECORD_UNKNOWN_FIELD) or a computed/assigned
//     target — formula/lookup/rollup/auto_number, via computedReadonlyCode — is rejected before
//     any write. auto_number especially: no read gate, never re-assigned on Update, so a stray
//     workflow overwrite is PERMANENT corruption. (Config-time already rejects such rules in
//     buildActionsJSON; this is the execution-time backstop for a rule that predates that guard.)
//   - WF-VALUE-TYPECHECK: the value is type-checked against the target field's spec (a string
//     into a number field → RECORD_INVALID_TYPE; an out-of-range number → RECORD_INVALID_RANGE;
//     etc.) so a bad rule can no longer merge junk straight into obj_*.data via dynrepo, which
//     bypasses ValidateRecord. A relation-FK value is additionally probed for existence in the
//     tenant (checkRelationTargets → RECORD_RELATION_INVALID), the SAME guard the PUT path runs, so
//     an automation cannot write a dangling/cross-tenant FK. NOTE: field/row-level ACL is
//     intentionally NOT applied here — the engine acts as a system principal (see the type doc).
//
// A nil catalog (legacy unit fakes / pre-WF-01 wiring) skips validation and passes {field:value}
// through verbatim, preserving prior behaviour. All errors are ValidationErrors carrying a machine
// code; the dispatcher logs them fail-open and records a FAILED run (W6) — the executor never
// crashes and the triggering mutation (already committed) is unaffected.
func (m *WorkflowRecordMutator) validateFieldUpdate(ctx context.Context, entity, tenantID, field string, value any) (map[string]any, []FieldSpec, []dynrepo.UniqueFieldSpec, error) {
	if m.catalog == nil {
		// No catalog (legacy fakes / pre-WF-01 wiring): pass the value through and return nil specs
		// so the caller's formula recompute is disarmed too (it needs specs to know what to compute).
		return map[string]any{field: value}, nil, nil, nil
	}
	def, err := m.catalog.GetEntityByName(ctx, tenantID, entity)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("services.WorkflowRecordMutator.validateFieldUpdate: %w", err)
	}
	fields, err := m.catalog.ListFields(ctx, tenantID, def.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("services.WorkflowRecordMutator.validateFieldUpdate: %w", err)
	}
	specs, err := toFieldSpecs(fields)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("services.WorkflowRecordMutator.validateFieldUpdate: %w", err)
	}
	uniqueFields := uniqueFieldSpecs(specs)
	clean, err := ValidatePartialRecord(specs, map[string]any{field: value})
	if err != nil {
		// Surface the machine code in the error TEXT so the W6 run-history row (which records
		// runErr.Error()) tells an admin WHY the action failed (e.g. RECORD_INVALID_TYPE,
		// RECORD_AUTONUMBER_READONLY) — ValidationError.Error() alone returns only the message.
		// The ValidationError is still wrapped with %w so errors.As can recover the typed code.
		var ve *ValidationError
		if errors.As(err, &ve) {
			return nil, nil, nil, fmt.Errorf("services.WorkflowRecordMutator.validateFieldUpdate: field %q rejected (%s): %w", field, ve.Code, err)
		}
		return nil, nil, nil, fmt.Errorf("services.WorkflowRecordMutator.validateFieldUpdate: %w", err)
	}
	// WF-VALUE-TYPECHECK (relation parity, review 2026-07-03): also verify any relation-FK in the
	// clean map points to a live in-tenant record — the SAME check the HTTP PUT write-path runs
	// (checkRelationTargets) — so an automation cannot merge a dangling or cross-tenant FK that a
	// manual PUT would reject with RECORD_RELATION_INVALID. ValidatePartialRecord only asserts a
	// relation value is a string; it does NOT probe existence. Requires a store that supports
	// existence probing (production *dynrepo.Repository does); a legacy fake without it skips this,
	// matching the nil-catalog disarm above.
	if rex, ok := m.store.(relationExister); ok {
		if err := checkRelationTargets(ctx, rex, tenantID, specs, clean, nil); err != nil {
			var ve *ValidationError
			if errors.As(err, &ve) {
				return nil, nil, nil, fmt.Errorf("services.WorkflowRecordMutator.validateFieldUpdate: field %q rejected (%s): %w", field, ve.Code, err)
			}
			return nil, nil, nil, fmt.Errorf("services.WorkflowRecordMutator.validateFieldUpdate: %w", err)
		}
		// ACTIVITY-CORE #3a: same tenant-scoped existence probe for a polymorphic_relation
		// field_update value — an automation must not merge a dangling/cross-tenant {type,id}
		// that a manual PUT would reject with RECORD_POLYMORPHIC_TARGET_INVALID.
		if err := checkPolymorphicTargets(ctx, rex, tenantID, specs, clean, nil); err != nil {
			var ve *ValidationError
			if errors.As(err, &ve) {
				return nil, nil, nil, fmt.Errorf("services.WorkflowRecordMutator.validateFieldUpdate: field %q rejected (%s): %w", field, ve.Code, err)
			}
			return nil, nil, nil, fmt.Errorf("services.WorkflowRecordMutator.validateFieldUpdate: %w", err)
		}
	}
	// USER-FIELD: same tenant-scoped ACTIVE-user existence probe for a `user` field_update value
	// — an automation must not merge a dangling/cross-tenant/disabled user id that a manual PUT
	// would reject with RECORD_USER_INVALID. Separate type-assertion from relationExister above
	// (a different method, userExister.BatchActiveUserExists); a legacy fake without it skips this.
	if uex, ok := m.store.(userExister); ok {
		if err := checkUserTargets(ctx, uex, tenantID, specs, clean); err != nil {
			var ve *ValidationError
			if errors.As(err, &ve) {
				return nil, nil, nil, fmt.Errorf("services.WorkflowRecordMutator.validateFieldUpdate: field %q rejected (%s): %w", field, ve.Code, err)
			}
			return nil, nil, nil, fmt.Errorf("services.WorkflowRecordMutator.validateFieldUpdate: %w", err)
		}
	}
	return clean, specs, uniqueFields, nil
}

// Ensure the adapter satisfies the engine's interface at compile time.
var _ workflow.RecordMutator = (*WorkflowRecordMutator)(nil)

// ---------------------------------------------------------------------------------------------
// CREATE-RECORD adapter: the create_record action's full-create seam.
// ---------------------------------------------------------------------------------------------

// WorkflowRecordCreator adapts *RecordService to the workflow engine's RecordCreator interface
// (CREATE-RECORD action). Unlike the field_update mutator — which BYPASSES RecordService and writes
// through the repo (RM-28) — create_record deliberately routes through the SAME RecordService.Create
// the /:entity API uses, so a workflow-created record goes through one write path (validation,
// relation checks, auto_number, pivot maintenance, and the after_create emit that advances the
// recursion-guard depth). It acts as a SYSTEM principal: RecordScope{IsAdmin:true} bypasses field-ACL
// and row-scope so an automation is never blocked, mirroring the field_update mutator's admin scope.
//
// The construction cycle (RecordService → engine.WithEngine → creator wraps RecordService) is broken
// by wiring this AFTER the service is built and injecting it via engine.WithRecordCreator.
type WorkflowRecordCreator struct {
	svc *RecordService
}

// NewWorkflowRecordCreator wires the adapter over the record service.
func NewWorkflowRecordCreator(svc *RecordService) *WorkflowRecordCreator {
	return &WorkflowRecordCreator{svc: svc}
}

// CreateRecord inserts a new record of entity in tenantID (never cross-tenant), owned by createdBy
// (the trigger record's owner, resolved upstream), through RecordService.Create as a system
// principal. ctx carries the automation chain depth (WithChainDepth) which Create's emit stamps onto
// the created record's after_create event — that is how the recursion guard advances. Errors
// (validation, missing required target field) are wrapped and surfaced to the dispatcher, which logs
// them fail-open and records a FAILED run (W6) — the triggering mutation is unaffected.
func (c *WorkflowRecordCreator) CreateRecord(ctx context.Context, tenantID, entity, createdBy string, values map[string]any) (string, error) {
	if c == nil || c.svc == nil {
		return "", fmt.Errorf("services.WorkflowRecordCreator.CreateRecord: no record service wired")
	}
	id, err := c.svc.Create(ctx, RecordScope{IsAdmin: true}, tenantID, createdBy, entity, values)
	if err != nil {
		return "", fmt.Errorf("services.WorkflowRecordCreator.CreateRecord: %w", err)
	}
	return id, nil
}

// Ensure the adapter satisfies the engine's interface at compile time.
var _ workflow.RecordCreator = (*WorkflowRecordCreator)(nil)

// ---------------------------------------------------------------------------------------------
// M2 adapters: email_alert sender + scheduler record lister.
// ---------------------------------------------------------------------------------------------

// workflowEmailTransport is the minimal slice of internal/notify's email transport the email_alert
// adapter needs. Satisfied by *notify.SMTPSender / *notify.NoopSender. Declared consumer-side so
// the adapter stays unit-testable with a fake.
type workflowEmailTransport interface {
	Send(ctx context.Context, m notify.Mail) error
}

// WorkflowEmailSender adapts the internal/notify email transport to the workflow engine's
// EmailSender interface (EPIC 4 M2 email_alert action). It reuses the SAME SMTP sender the
// security-alert dispatcher uses — one provider-agnostic transport, configured once. The body is
// plain text (workflow templates are plain {{field}} interpolation, not HTML).
type WorkflowEmailSender struct {
	transport workflowEmailTransport
	log       emailLogWriter
	logger    *zap.Logger
}

// WithEmailLog makes the direct sender (JOBS_ENABLED=false) write ONE final row per mail (specs/066,
// D-13): sent, or failed with the masked error. A log failure never changes the send result.
func (s *WorkflowEmailSender) WithEmailLog(log emailLogWriter, logger *zap.Logger) *WorkflowEmailSender {
	if logger == nil {
		logger = zap.NewNop()
	}
	s.log, s.logger = log, logger
	return s
}

// NewWorkflowEmailSender wires the adapter over a notify email transport (SMTP or noop).
func NewWorkflowEmailSender(transport workflowEmailTransport) *WorkflowEmailSender {
	return &WorkflowEmailSender{transport: transport}
}

// SendEmail delivers one plain-text message through the underlying transport. A nil transport is
// an error (logged fail-open by the dispatcher) rather than a panic.
func (s *WorkflowEmailSender) SendEmail(ctx context.Context, to, subject, body string) error {
	if s == nil || s.transport == nil {
		return fmt.Errorf("services.WorkflowEmailSender.SendEmail: no transport wired")
	}
	sendErr := s.transport.Send(ctx, notify.Mail{To: to, Subject: subject, Body: body})
	s.logFinal(ctx, to, sendErr)
	if sendErr != nil {
		return fmt.Errorf("services.WorkflowEmailSender.SendEmail: %w", sendErr)
	}
	return nil
}

// logFinal writes the one terminal row of a direct send. Fail-open: errors only warn.
func (s *WorkflowEmailSender) logFinal(ctx context.Context, to string, sendErr error) {
	if s.log == nil {
		return
	}
	status := emaillog.StatusSent
	if sendErr != nil {
		status = emaillog.StatusFailed
	}
	e, ok := emailLogEntry(ctx, to, status)
	if !ok {
		return
	}
	e.Attempts, e.Finished = 1, true
	if sendErr != nil {
		e.LastError = workflow.MaskRecipientError("services.WorkflowEmailSender.SendEmail", to, sendErr).Error()
	}
	if err := s.log.Insert(ctx, e); err != nil {
		s.logger.Warn("email log: insert failed (send result unchanged)", zap.String("tenant_id", e.TenantID),
			zap.String("workflow_id", e.WorkflowID), zap.Error(err))
	}
}

// Ensure the adapter satisfies the engine's interface at compile time.
var _ workflow.EmailSender = (*WorkflowEmailSender)(nil)

// recordScanner is the minimal slice of the dynamic record store the scheduler lister needs:
// list the records of one entity in one tenant. Satisfied by *dynrepo.Repository.
type recordScanner interface {
	ListRecords(ctx context.Context, entity, tenantID string, scope dynrepo.RowScope, p dynrepo.ListParams) ([]dynrepo.Record, int, error)
}

// WorkflowRecordLister adapts the dynamic record store to the scheduler's RecordLister interface
// (EPIC 4 M2). It lists an entity's records under an ADMIN row-scope (a scheduled automation must
// see every row in the tenant, not just one user's) so the scheduler can evaluate the rule's
// condition against each. tenant_id is always pinned by the store.
type WorkflowRecordLister struct {
	store recordScanner
}

// NewWorkflowRecordLister wires the scheduler lister over the dynamic record store.
func NewWorkflowRecordLister(store recordScanner) *WorkflowRecordLister {
	return &WorkflowRecordLister{store: store}
}

// ListRecordsForScheduler returns up to `limit` records (id + data) of the entity in the tenant,
// under an admin scope. It maps the store's Record to the engine's minimal ScheduledRecord.
func (l *WorkflowRecordLister) ListRecordsForScheduler(ctx context.Context, entity, tenantID string, limit int) ([]workflow.ScheduledRecord, error) {
	if l == nil || l.store == nil {
		return nil, fmt.Errorf("services.WorkflowRecordLister.ListRecordsForScheduler: no store wired")
	}
	if limit < 1 {
		limit = 1
	}
	recs, _, err := l.store.ListRecords(ctx, entity, tenantID, dynrepo.AdminScope(), dynrepo.ListParams{
		Page:  1,
		Limit: limit,
		Sort:  "created_at",
		Order: "asc",
		// specs/064 S9-4: sample rows never feed a scheduled rule (no email / webhook on demo data).
		ExcludeSamples: true,
	})
	if err != nil {
		return nil, fmt.Errorf("services.WorkflowRecordLister.ListRecordsForScheduler: %w", err)
	}
	out := make([]workflow.ScheduledRecord, len(recs))
	for i := range recs {
		out[i] = workflow.ScheduledRecord{ID: recs[i].ID, Data: recs[i].Data, OwnerID: recs[i].OwnerID, CreatedAt: recs[i].CreatedAt}
	}
	return out, nil
}

// Ensure the adapter satisfies the engine's interface at compile time.
var _ workflow.RecordLister = (*WorkflowRecordLister)(nil)

// SetOwner implements the workflow engine's RecordMutator half that hands a record over to the
// user an assign action just picked. It writes owner_id ONLY — created_by stays the immutable
// creator, as rowscope.go's rowFilter doc requires.
//
// It deliberately does NOT go through RecordService.SetOwner: that method's first act is a
// management-permission check ("chỉ quản lý... mới được đổi chủ"), which is a rule about HUMAN
// callers. The engine is the system principal, and the assignee it passes came from
// NextAssignee/LeastLoadedAssignee, which only ever return an ACTIVE member of the configured
// team — the same liveness bar RecordService.SetOwner enforces on a hand-typed userId.
func (m *WorkflowRecordMutator) SetOwner(ctx context.Context, entity, tenantID, recordID, systemActor, newOwnerID string) error {
	if m == nil || m.store == nil {
		return fmt.Errorf("services.WorkflowRecordMutator.SetOwner: no store wired")
	}
	if err := m.store.SetOwner(ctx, entity, tenantID, recordID, systemActor, dynrepo.AdminScope(), newOwnerID); err != nil {
		return fmt.Errorf("services.WorkflowRecordMutator.SetOwner: %w", err)
	}
	// Queue #86: the same row a human "Đổi chủ" writes (record_owner.go), by the system actor —
	// handing a record over is the change "who gave this lead to her" asks about.
	if m.auditor != nil {
		m.auditor.Record(ctx, audit.Event{
			TenantID:    tenantID,
			ActorUserID: systemActor,
			EntityName:  entity,
			RecordID:    recordID,
			Action:      audit.ActionUpdate,
			Diff:        audit.ComputeDiff(nil, map[string]any{"ownerId": newOwnerID}),
			At:          time.Now().UTC(),
		})
	}
	return nil
}
