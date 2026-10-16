package workflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/calendar"
)

// RelatedWriter is the optional second face of a RecordMutator (#330): UpdateRelatedField writes like
// UpdateField and then emits the target record's after_update at the given chain depth, so the
// target entity's rules fire. A mutator without it keeps the old no-cascade write (production always wires the RelatedWriter).
type RelatedWriter interface {
	UpdateRelatedField(ctx context.Context, entity, tenantID, recordID, systemActor, field string, value any, depth int) error
}

// RecordMutator is the engine's consumer-side view of the dynamic record store needed by the
// field_update action: patch one field on the SAME record that triggered the workflow. The real
// implementation is *dynrepo.Repository.UpdateRecord — but the engine acts as a SYSTEM principal
// (admin scope + a synthetic actor) so an automation is never blocked by the triggering user's
// row-level access. Declaring the interface here keeps the workflow package free of a hard
// dependency on dynrepo's concrete types and lets unit tests substitute a fake.
type RecordMutator interface {
	// UpdateField sets field=value on the record (entity, tenantID, recordID) using a system
	// (admin) scope. It is the only write the field_update action performs. systemActor is the
	// synthetic user id stamped as modified_by so the change is attributable to the engine.
	UpdateField(ctx context.Context, entity, tenantID, recordID, systemActor, field string, value any) error
	// SetOwner reassigns the record's owner_id — the column the ACL row filter enforces on
	// (COALESCE(owner_id, created_by)). Required, not optional: Founder's rule is that giving
	// someone work gives them the right to edit it, so an assign that only wrote a display
	// field would hand a rep a lead they can see and cannot touch. Declaring it here means a
	// half-wired mutator fails to COMPILE rather than silently skipping the grant.
	SetOwner(ctx context.Context, entity, tenantID, recordID, systemActor, newOwnerID string) error
	// RelatedEntity resolves the entity a to-one `relation` field (via) of entity points at, from
	// metadata (options.relatedEntity) in tenantID — the target of a field_update that follows a
	// relation (specs/058). Resolved at fire time, never copied into the rule, so the rule follows
	// the metadata. An unknown / non-relation / multi-target via is an error (a FAILED run).
	RelatedEntity(ctx context.Context, tenantID, entity, via string) (string, error)
	// ReadForUpdate returns the record's CURRENT data and its optimistic-lock version, read with
	// the same system scope as the writes (DAILY-QUEUE #172). A rule that decides from what is on
	// the record — tighten_only — decides from this, not from the event snapshot.
	ReadForUpdate(ctx context.Context, entity, tenantID, recordID string) (map[string]any, int, error)
	// UpdateFieldsIfVersion writes every field in ONE write, and only while the record is still
	// at version; a write that landed in between makes it change nothing and return
	// ErrRecordChanged. Values are validated like UpdateField's.
	UpdateFieldsIfVersion(ctx context.Context, entity, tenantID, recordID, systemActor string, fields map[string]any, version int) error
}

// ErrRecordChanged is UpdateFieldsIfVersion's "someone wrote the record after you read it":
// nothing was written, read again and decide again.
var ErrRecordChanged = errors.New("workflow: record changed since it was read")

// NotificationWriter is the engine's consumer-side view of the notification sink needed by the
// in_app_notification action. Satisfied by *SQLRepository.InsertNotification (wrapped). Declared
// as an interface so the dispatcher can be unit-tested without a database.
type NotificationWriter interface {
	InsertNotification(ctx context.Context, n Notification) (string, error)
}

// AssignmentResolver is the engine's consumer-side view of the ASSIGN-1 round-robin resolver
// needed by the assign action: given the rule + the action's slot key + the team, atomically
// advance the per-rule cursor (sys_assignment_cursors) and return the NEXT active member of the
// team. Satisfied by *services.AssignmentService. Declared here so the workflow package keeps no
// database dependency and unit tests can substitute a fake ring.
//
// ok=false means "no eligible member" (empty team / every member disabled) — runAssign then
// no-ops (fail-open), like every other action when there is nothing to do. The advance MUST run
// under a row lock (FOR UPDATE) so two records firing the same rule concurrently pick different
// members (spec §4) — that guarantee lives in the implementation, not this interface.
type AssignmentResolver interface {
	NextAssignee(ctx context.Context, tenantID, workflowID, actionKey, teamID string) (userID string, ok bool, err error)
	// LeastLoadedAssignee resolves the load_based strategy (ASSIGN-2): pick the team's active
	// member with the FEWEST active assigned records in `entity` at `field` (the assign target
	// field), tie-broken by the SAME per-rule round-robin cursor so repeated equal-load fires still
	// rotate. entity is the trigger entity (evt.EntityName); field is the assign action's target
	// field (where the picked userId is written and thus what "load" is counted over). Same
	// tenant-scoped, fail-open (ok=false → runAssign no-ops) contract as NextAssignee.
	LeastLoadedAssignee(ctx context.Context, tenantID, workflowID, actionKey, entity, field, teamID string) (userID string, ok bool, err error)
}

// RecordCreator is the engine's consumer-side view of the FULL record-create path needed by the
// create_record action (CREATE-RECORD): insert a brand-new record of a target entity through the
// SAME validation / ACL / tenant scoping / pivot maintenance the API uses — no second write path.
// The real implementation wraps *services.RecordService.Create acting as a SYSTEM principal (admin
// scope, so an automation is never blocked by a caller's row access). Declared here so the workflow
// package keeps no dependency on the services layer and unit tests can substitute a fake.
//
// ctx MUST carry the automation chain depth (WithChainDepth) so the created record's after_create
// event inherits depth+1 — that is how the recursion guard's counter advances across the async
// dispatch boundary. A bad value / missing required target field surfaces as an error the caller
// logs fail-open (recorded as a FAILED run per W6); the triggering mutation is unaffected.
type RecordCreator interface {
	// CreateRecord inserts a new record of entity in tenantID owned by createdBy, returning its id.
	// values is the field→value map (already $owner-resolved by the caller).
	CreateRecord(ctx context.Context, tenantID, entity, createdBy string, values map[string]any) (string, error)
}

// ApprovalStarter is the engine's consumer-side view of the approval engine needed by the
// start_approval action (specs/043 S9b): send the record through the named manual process as
// submitterID. The real implementation is bpm.Requester behind a record read — declared here so the
// workflow package imports nothing of bpm (bpm already imports workflow).
//
// started=false with a nil error means the record already waits on an approval: the caller notes it
// and moves on. A process that does not apply to the record (wrong entity, criteria not met,
// deleted, switched off) is an error — the run history must show the rule is pointing at nothing.
type ApprovalStarter interface {
	StartApproval(ctx context.Context, tenantID, entityName, recordID, processID, submitterID string, data map[string]any) (started bool, err error)
}

// ScheduleResolver is the engine's consumer-side view of the CALENDAR-1 store: given a tenant (and
// optionally a pinned calendar id, "" = the tenant's default), hand back the working schedule the
// business_hours arithmetic walks. The real implementation is services.WorkflowScheduleResolver,
// the SAME body the date_offset timer producer resolves with — one lookup, so a deadline and the
// reminder about it can never disagree about when the office is open.
//
// (nil, nil) means "this tenant has no calendar": the documented wall-clock fallback, NOT an error.
// A store failure must come back as an error — see runSetDeadline for why the difference matters.
type ScheduleResolver interface {
	ResolveSchedule(ctx context.Context, tenantID, calendarID string) (*calendar.Schedule, error)
}

// SystemActorID is the synthetic actor id stamped as modified_by when the engine applies a
// field_update. It marks the change as engine-driven (automation) rather than a human edit, and
// is a stable, recognizable sentinel in audit/debug output. It is NOT a real user row. The form
// is a valid CHAR(36) UUID (the "f10000..." marker reads as "flow") so it fits modified_by.
// Exported for the TIMER-1 job handler (services/workflow_timer.go), which builds the fire-time
// Event outside this package.
const SystemActorID = "f1000000-0000-0000-0000-000000000000"

// actionDeps bundles every consumer-side dependency the action runners need. Passing one struct
// (instead of a growing positional list) keeps runActions/runAction stable as M2+ adds actions,
// and lets a caller wire only the deps it has (a nil dep makes its action error fail-open, never
// panics). The AsyncDispatcher builds one actionDeps once and reuses it for every event.
type actionDeps struct {
	mut   RecordMutator      // field_update target store
	notif NotificationWriter // in_app_notification sink
	email EmailSender        // email_alert transport (M2)
	// templates answers email placeholders the record cannot — {{relation.field}} and lookup
	// fields (specs/059). nil = the record's own data only (pre-059 behaviour).
	templates TemplateResolver
	webhook   WebhookSender // outbound_webhook inline transport (M2; the JOBS_ENABLED=false path)
	// webhookQueue parks outbound_webhook deliveries on sys_jobs (DAILY-QUEUE #26). When set it
	// wins over webhook: the dispatcher never posts inline while a durable queue is wired.
	webhookQueue WebhookQueue
	// creds resolves an outbound_webhook's vault key at send time (specs/050 S2b). nil makes a
	// vault action fail CREDENTIALS_NOT_CONFIGURED — it never sends without its key.
	creds  CredentialResolver
	assign AssignmentResolver // assign round-robin resolver (ASSIGN-1)
	// recipients expands $role: / $team: recipients of an in_app_notification (queue #281).
	recipients RecipientResolver
	creator    RecordCreator // create_record full-create seam (CREATE-RECORD)
	// approvals backs start_approval (specs/043 S9b). nil = the action fails with a clear error.
	approvals ApprovalStarter
	// sched resolves the tenant's working calendar for a business_hours offset (DAILY-QUEUE #10).
	// A nil sched is NOT "this tenant has no calendar" — it is "nobody wired the seam", and
	// runSetDeadline errors on it rather than computing wall-clock under a business_hours label.
	sched ScheduleResolver
	// warnedOnce dedups the wall-clock-fallback WARN across one scheduled firing (#445). The
	// scheduler fires rules on one goroutine, so a plain map needs no lock. nil (event path) = log
	// every time.
	warnedOnce map[string]struct{}
	// maxDepth bounds create_record recursion: the action is SKIPPED once evt.Depth >= maxDepth.
	// Sourced from the dispatcher (WithRecordCreator clamps a non-positive value to a safe default).
	maxDepth int
	// logger lets runCreateRecord log the recursion-cap skip (which is a designed no-op, NOT an
	// action error, so it must not go through the fail-open action-error log). Never nil: the
	// dispatcher passes its own logger (defaulted to zap.NewNop()).
	logger *zap.Logger
}

// runActions executes a workflow's actions in order against the event. It is the action
// dispatcher invoked by the AsyncDispatcher AFTER a rule's condition matched. Each action runs
// independently: an error in one action is returned to the caller (which logs it, fail-open) but
// the engine continues to the next action so one bad action does not silently drop the rest.
// runActions returns the per-action outcomes (recorded into sys_workflow_runs.actions_result,
// W6) plus the FIRST error encountered (for logging) after attempting every action.
func runActions(ctx context.Context, wf Workflow, evt Event, deps actionDeps) ([]ActionResult, error) {
	var firstErr error
	results := make([]ActionResult, 0, len(wf.Actions))
	ref, inRun := RunRefFrom(ctx)
	runNote := runWarningFrom(ctx) // #105: a note about the whole run (a wall-clock timer wake-up)
	for i, a := range wf.Actions {
		res := ActionResult{Type: a.Type, Field: a.Field, Via: a.Via, OK: true}
		actx := ctx
		if inRun {
			// specs/059: the action's own slot, so a late outcome lands on actions_result[i].
			ref.ActionIndex = i
			actx = WithRunRef(ctx, ref)
		}
		actx, note := WithDeliveryNote(actx)
		var warning string
		var err error
		if a.Type == ActionAssign {
			// #280: the actions after an assign see the NEW owner — "$owner" is whoever holds the
			// record now, not whoever created it.
			var assignee string
			assignee, err = runAssignPick(actx, wf, evt, a, "a"+strconv.Itoa(i), deps.assign, deps.mut)
			if err == nil && assignee != "" {
				evt = withAssignee(evt, strings.TrimSpace(a.Field), assignee)
			}
		} else {
			warning, err = runAction(actx, wf, evt, a, i, deps)
		}
		res.Warning = strings.TrimSpace(warning + " " + runNote)
		if err == nil && note.Queued() {
			res.Delivery = DeliveryQueued // #339: the work is on the queue, not done
		}
		if err != nil {
			res.OK = false
			res.Error = err.Error()
			if firstErr == nil {
				firstErr = err
			}
		}
		results = append(results, res)
	}
	return results, firstErr
}

// runAction executes a single action. Unknown action types are a no-op error (fail-safe): config
// validation rejects them on write, so this is unreachable in practice but never panics.
// The string is a non-fatal note for the run history (ActionResult.Warning) — empty for every
// action except a set_deadline that fell back to wall-clock. idx is the action's slot in
// wf.Actions.
func runAction(ctx context.Context, wf Workflow, evt Event, a Action, idx int, deps actionDeps) (string, error) {
	// actionKey is the action's stable slot within the rule ("a0", "a1", …). It labels the
	// ASSIGN-1 round-robin cursor so a rule with two assign actions keeps two independent
	// cursors (spec §3). Deriving it from the index means reordering a rule's actions
	// re-labels the slots (documented benign — round-robin stays approximately fair).
	actionKey := "a" + strconv.Itoa(idx)
	switch a.Type {
	case ActionFieldUpdate:
		return runFieldUpdate(ctx, evt, a, deps.mut, deps.maxDepth)
	case ActionInAppNotification:
		return "", runInAppNotification(ctx, wf, evt, a, deps.notif, deps.templates, deps.recipients)
	case ActionEmailAlert:
		// specs/066: tell the sender where this mail comes from (config ids only) so it can write
		// the tenant's email log. A resend pre-sets an origin with a LogID — never overwrite it.
		if o, ok := EmailOriginFrom(ctx); !ok || o.LogID == "" {
			ctx = WithEmailOrigin(ctx, EmailOrigin{
				TenantID: evt.TenantID, WorkflowID: wf.ID, ActionIndex: idx,
				EntityName: evt.EntityName, RecordID: evt.RecordID,
				SubjectTemplate: a.Subject, ActionHash: EmailActionHash(a),
			})
		}
		return "", runEmailAlert(ctx, evt, a, deps.email, deps.templates)
	case ActionWebhook:
		return "", runWebhook(ctx, wf, evt, a, idx, deps)
	case ActionAssign:
		return "", runAssign(ctx, wf, evt, a, actionKey, deps.assign, deps.mut)
	case ActionCreateRecord:
		return "", runCreateRecord(ctx, evt, a, deps)
	case ActionSetDeadline:
		return runSetDeadline(ctx, wf, evt, a, deps)
	case ActionSetElapsed:
		return runSetElapsed(ctx, wf, evt, a, deps)
	case ActionStartApproval:
		return runStartApproval(ctx, evt, a, deps)
	default:
		return "", fmt.Errorf("workflow.runAction: unknown action type %q", a.Type)
	}
}

// runStartApproval executes a start_approval action (specs/043 S9b). The sender is the record's
// owner (a rule has no person pressing a button; the approval engine falls back to the owner when
// the sender later leaves), else the engine's synthetic actor. Depth: starting an approval is one
// more hop of rule → record → approval → write-back → rule, so it obeys the same cap as
// create_record — skipped, not failed, once evt.Depth reaches it.
func runStartApproval(ctx context.Context, evt Event, a Action, deps actionDeps) (string, error) {
	if deps.approvals == nil {
		return "", fmt.Errorf("workflow.runStartApproval: no approval starter wired")
	}
	processID := strings.TrimSpace(a.ProcessID)
	if processID == "" {
		return "", fmt.Errorf("workflow.runStartApproval: empty process id")
	}
	if evt.Trigger == TriggerAfterDelete {
		return "", nil // no live record left to approve
	}
	maxDepth := deps.maxDepth
	if maxDepth < 1 { // a dispatcher without WithRecordCreator must not skip every start
		maxDepth = DefaultMaxChainDepth
	}
	if evt.Depth >= maxDepth {
		logger := deps.logger
		if logger == nil {
			logger = zap.NewNop()
		}
		logger.Warn("workflow: start_approval skipped — chain depth cap reached",
			zap.String("tenant_id", evt.TenantID), zap.String("entity_name", evt.EntityName),
			zap.String("record_id", evt.RecordID), zap.Int("depth", evt.Depth), zap.Int("max_depth", maxDepth))
		return "", nil
	}
	// Sender = evt.OwnerUserID: the creator/acting user on live after_create/after_update events, the
	// effective owner (owner_id ?? created_by) on scheduler/timer events. Known asymmetry (specs/043 note 30d).
	submitter := strings.TrimSpace(evt.OwnerUserID)
	if submitter == "" {
		submitter = SystemActorID
	}
	started, err := deps.approvals.StartApproval(WithChainDepth(ctx, evt.Depth+1), evt.TenantID, evt.EntityName, evt.RecordID, processID, submitter, evt.Data)
	if err != nil {
		return "", fmt.Errorf("workflow.runStartApproval: %w", err)
	}
	if !started {
		return "bản ghi đang chờ duyệt — không khởi động thêm hồ sơ", nil
	}
	return "", nil
}

// runCreateRecord executes a create_record action (CREATE-RECORD): insert a NEW record of the
// action's target entity, with the action's field values, through the SAME RecordService.Create the
// API uses (full validation, tenant scoping, pivot maintenance — no second write path).
//
// RECURSION GUARD (the crux): a create_record rule can create a record that re-triggers the same (or
// another) create_record rule — an infinite loop. The engine bounds it by the automation chain depth
// carried on the Event: this action is SKIPPED (logged, NOT an error) once evt.Depth >= deps.maxDepth.
// Otherwise it invokes the nested Create with ctx carrying depth+1 (WithChainDepth), so the created
// record's after_create event inherits the higher depth and the chain terminates after maxDepth hops
// (exactly maxDepth records created by a self-referential A→A rule: firings at depth 0..maxDepth-1).
//
// created_by is the trigger record's owner (evt.OwnerUserID) so the new record is owned by a REAL
// user (row-level security works — e.g. a follow-up task belongs to the lead's owner); it falls back
// to the engine's SystemActorID when the owner could not be resolved. The record is always created in
// evt.TenantID — never cross-tenant. Unlike field_update, create_record is NOT skipped on
// after_delete: creating an archive/follow-up record in reaction to a delete is valid (the delete
// event still carries the before-image owner in evt.OwnerUserID).
func runCreateRecord(ctx context.Context, evt Event, a Action, deps actionDeps) error {
	if deps.creator == nil {
		return fmt.Errorf("workflow.runCreateRecord: no record creator wired")
	}
	entity := strings.TrimSpace(a.CreateEntity)
	if entity == "" {
		return fmt.Errorf("workflow.runCreateRecord: empty target entity")
	}
	// RECURSION GUARD: stop the chain at the configured max depth. A cap-skip is NOT an error (the
	// rule worked as designed — the cap fired), so it returns nil; it is logged so an admin can see
	// the chain was truncated rather than silently swallowed.
	if evt.Depth >= deps.maxDepth {
		logger := deps.logger
		if logger == nil {
			logger = zap.NewNop()
		}
		logger.Warn("workflow: create_record skipped — chain depth cap reached",
			zap.String("tenant_id", evt.TenantID),
			zap.String("entity_name", evt.EntityName),
			zap.String("target_entity", entity),
			zap.Int("depth", evt.Depth),
			zap.Int("max_depth", deps.maxDepth),
		)
		return nil
	}
	owner := strings.TrimSpace(evt.OwnerUserID)
	createdBy := owner
	if createdBy == "" {
		createdBy = SystemActorID
	}
	values, err := RenderCreateValues(ctx, evt, a, deps.templates)
	if err != nil {
		return fmt.Errorf("workflow.runCreateRecord: %w", err)
	}
	// Carry depth+1 into the nested Create so its emitted after_create event advances the guard.
	nestedCtx := WithChainDepth(ctx, evt.Depth+1)
	if _, err := deps.creator.CreateRecord(nestedCtx, evt.TenantID, entity, createdBy, values); err != nil {
		return fmt.Errorf("workflow.runCreateRecord: %w", err)
	}
	return nil
}

// resolveCreateValues returns a fresh copy of the action's values with the "$owner" sentinel replaced
// by the resolved owner id. It never mutates the stored action map (the dispatcher reuses it across
// events). An unresolvable "$owner" (empty owner) is OMITTED rather than written as the literal
// string — a downstream required-field check then surfaces the omission as a FAILED run (W6) instead
// of persisting a bogus "$owner" value into a user field.
func resolveCreateValues(in map[string]any, owner string, evt Event) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		str, isStr := v.(string)
		if !isStr {
			out[k] = v // numbers, booleans, objects travel untouched
			continue
		}
		switch {
		case str == OwnerSentinel:
			if owner == "" {
				continue // unresolvable $owner — omit rather than write the literal sentinel
			}
			out[k] = owner
		case str == RecordSentinel:
			// The child's link BACK to the record that spawned it. Without this a created record
			// was an orphan: the rule could make a contract from a quote and had no way to say
			// which quote, so the two rows sat unconnected and the whole convert flow was
			// inexpressible.
			if evt.RecordID == "" {
				continue
			}
			out[k] = evt.RecordID
		case strings.HasPrefix(str, RecordFieldPrefix):
			// $record.<field> copies the TYPED value out of the source record. Going through the
			// text renderer instead would stringify a currency object or a number, and the write
			// would then be rejected — or worse, coerced.
			field := strings.TrimPrefix(str, RecordFieldPrefix)
			if val, ok := evt.Data[field]; ok && val != nil {
				out[k] = val
			}
		default:
			// Everything else runs through the SAME renderer the email/webhook actions use, so
			// "{{name}} — bản hợp đồng" means here exactly what it means there.
			//
			// This is not only a missing feature: the builder already ships a values editor that
			// accepts free text, so a person typing {{name}} got the literal five characters
			// written into the record. A silent wrong write, not an absence.
			out[k] = renderTemplate(str, evt.Data)
		}
	}
	return out
}

// runFieldUpdate applies a field_update: set a.Field = a.Value on the SAME record (evt.RecordID)
// in the SAME tenant, via a system (admin) scope so RLS never blocks the automation. A delete
// event has no live record to patch, so field_update on after_delete is skipped (no-op) rather
// than erroring — the rule simply has nothing to update.
//
// With a.Via set (specs/058) the write lands on the RELATED record instead — see
// runRelatedFieldUpdate. The returned string is the run-history note (ActionResult.Warning).
func runFieldUpdate(ctx context.Context, evt Event, a Action, mut RecordMutator, maxDepth int) (string, error) {
	if mut == nil {
		return "", fmt.Errorf("workflow.runFieldUpdate: no record mutator wired")
	}
	via := strings.TrimSpace(a.Via)
	if via == "" && evt.Trigger == TriggerAfterDelete && strings.TrimSpace(a.ChildEntity) == "" {
		return "", nil // nothing to update on a deleted record
	}
	field := strings.TrimSpace(a.Field)
	if field == "" {
		return "", fmt.Errorf("workflow.runFieldUpdate: empty target field")
	}
	value, resolved := ResolveFieldUpdateValue(a, evt)
	if !resolved {
		// A copy whose source is empty is skipped, never written as null: clearing the target on
		// every run because the source happened to be blank would be a silent data loss.
		return emptySourceWarning(a.Value), nil
	}
	if via != "" {
		return runRelatedFieldUpdate(ctx, evt, via, field, value, mut, maxDepth)
	}
	if strings.TrimSpace(a.ChildEntity) != "" {
		return runChildrenFieldUpdate(ctx, evt, strings.TrimSpace(a.ChildEntity), strings.TrimSpace(a.ChildVia), field, value, mut, maxDepth)
	}
	if err := mut.UpdateField(ctx, evt.EntityName, evt.TenantID, evt.RecordID, SystemActorID, field, value); err != nil {
		return "", fmt.Errorf("workflow.runFieldUpdate: %w", err)
	}
	return "", nil
}

// ResolveFieldUpdateValue reads a field_update's value the way create_record reads its values (queue
// #333): "$owner", "$record", "$record.<field>" (typed copy off the triggering record) and a string with
// {{field}} placeholders are resolved at fire time; anything else is the constant it always was.
// resolved=false means the value named a source that is empty on this record. Only after_* rules come
// here — a before_save rule reads its value through ValueSource (before.go) and a "$..." there is a
// literal — so a rule that sets ValueSource is left untouched. One function for the live run and the
// dry-run, so "Chạy thử" can never show a different value than the run writes.
func ResolveFieldUpdateValue(a Action, evt Event) (value any, resolved bool) {
	str, isStr := a.Value.(string)
	if !isStr || (a.ValueSource != "" && a.ValueSource != ValueSourceConstant) {
		return a.Value, true
	}
	t := strings.TrimSpace(str)
	if t != OwnerSentinel && t != RecordSentinel && !strings.HasPrefix(t, RecordFieldPrefix) && !strings.Contains(t, "{{") {
		return a.Value, true
	}
	// The trimmed text is what the save-time check judged, so it is what runs (" $record.name" is a copy).
	out := resolveCreateValues(map[string]any{"v": t}, evt.OwnerUserID, evt)
	v, ok := out["v"]
	if !ok {
		return nil, false
	}
	// A template whose placeholders ALL render empty has no source value: skip, never write ""
	// (or "co-hoi-") over the target — the same no-silent-clear rule as an empty $record.<f>.
	if names := TemplatePlaceholders(t); len(names) > 0 && t != OwnerSentinel && t != RecordSentinel && !strings.HasPrefix(t, RecordFieldPrefix) {
		allEmpty := true
		for _, n := range names {
			if renderTemplate("{{"+n+"}}", evt.Data) != "" {
				allEmpty = false
				break
			}
		}
		if allEmpty {
			return nil, false
		}
	}
	return v, true
}

func emptySourceWarning(v any) string {
	return fmt.Sprintf("giá trị %v rỗng trên bản ghi kích hoạt — không cập nhật (giữ nguyên trường đích)", v)
}

// runRelatedFieldUpdate is field_update following a to-one relation (specs/058): "hoá đơn Đã thanh
// toán ⇒ lịch hẹn đổi trạng thái". Each input comes from exactly one place:
//   - the target ENTITY from metadata (mut.RelatedEntity — options.relatedEntity of via), never
//     from the rule or the record, so a rule cannot aim at an entity its relation does not;
//   - the target RECORD id from the triggering record's via value (FK-checked in-tenant when it
//     was written);
//   - the TENANT from the event — the write is pinned to it, so an id from another tenant matches
//     no row and fails as not-found.
//
// The write goes through the SAME mut.UpdateField as a same-record field_update (field/value
// validated against the TARGET entity, formula recompute, audit row on the target). That path emits
// no workflow event, so a rule on the target entity is not re-triggered by it: two rules pointing
// at each other through relations cannot loop.
//
// An unlinked record (via empty) is not a failure — there is nothing to update — but the run says
// so. Unlike the same-record path this runs on after_delete: the event carries the before-image and
// the related record is still alive.
//
// The relation is resolved BEFORE the record's via value is read: a via deleted after the rule was
// saved is absent from every record created since, and reading first would report that broken rule
// as "chưa liên kết" (OK) instead of FAILED WORKFLOW_RELATED_VIA_INVALID — the same order the
// dry-run (services.previewRelatedFieldUpdate) checks in. The lookup is the TTL-cached catalog.
func runRelatedFieldUpdate(ctx context.Context, evt Event, via, field string, value any, mut RecordMutator, maxDepth int) (string, error) {
	target, err := mut.RelatedEntity(ctx, evt.TenantID, evt.EntityName, via)
	if err != nil {
		return "", fmt.Errorf("workflow.runRelatedFieldUpdate: %w", err)
	}
	raw := evt.Data[via]
	if raw == nil {
		return unlinkedWarning(via), nil
	}
	relatedID, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("workflow.runRelatedFieldUpdate: trường quan hệ %q không chứa id bản ghi (giá trị kiểu %T)", via, raw)
	}
	relatedID = strings.TrimSpace(relatedID)
	if relatedID == "" {
		return unlinkedWarning(via), nil
	}
	note, err := writeRelatedRecord(ctx, evt, target, relatedID, field, value, mut, maxDepth)
	if err != nil {
		return "", fmt.Errorf("workflow.runRelatedFieldUpdate: %s %s: %w", target, relatedID, err)
	}
	return note, nil
}

// writeRelatedRecord is the one write a field_update makes on a record other than the triggering
// one (via and children share it). #330: it emits the target's own after_update (Depth+1) so the
// TARGET entity's rules run — bounded like create_record. At the cap the value still lands (dropping
// the write would lose data), only the event is withheld, and the returned note says so.
func writeRelatedRecord(ctx context.Context, evt Event, entity, recordID, field string, value any, mut RecordMutator, maxDepth int) (string, error) {
	if maxDepth < 1 {
		maxDepth = DefaultMaxChainDepth
	}
	note := ""
	if rw, ok := mut.(RelatedWriter); ok && evt.Depth < maxDepth {
		return "", rw.UpdateRelatedField(ctx, entity, evt.TenantID, recordID, SystemActorID, field, value, evt.Depth+1)
	} else if ok {
		note = depthCapWarning(maxDepth)
	}
	return note, mut.UpdateField(ctx, entity, evt.TenantID, recordID, SystemActorID, field, value)
}

// MaxChildUpdatesPerRun is the per-run budget of a children field_update (specs/084): a run writes
// at most this many child records. ponytail: a constant, not per-rule config — raise it (or move it
// to the rule) when a real customer class outgrows it.
const MaxChildUpdatesPerRun = 200

// childPageSize is how many child ids one ListChildIDs call fetches (keyset by id).
const childPageSize = 50

// ChildLister is the optional third face of a RecordMutator (specs/084): the ids of the live child
// records of one parent, a page at a time. The implementation resolves the relation from metadata
// at fire time — childVia must be a to-one relation of childEntity pointing at parentEntity, else
// an error (a FAILED run, WORKFLOW_CHILD_VIA_INVALID) — and pins every read to tenantID. Ids come
// back in ascending order, strictly after afterID ("" = from the start), at most limit.
type ChildLister interface {
	ListChildIDs(ctx context.Context, tenantID, parentEntity, childEntity, childVia, parentID, afterID string, limit int) ([]string, error)
}

// runChildrenFieldUpdate writes field=value on every child of the triggering record (specs/084).
// Pages through the children by id so memory is O(page); stops with an error — after naming how many
// rows were already updated — when more than MaxChildUpdatesPerRun children exist. A write error
// stops the run at that child (the rows before it stay written; the error names the count).
func runChildrenFieldUpdate(ctx context.Context, evt Event, childEntity, childVia, field string, value any, mut RecordMutator, maxDepth int) (string, error) {
	lister, ok := mut.(ChildLister)
	if !ok {
		return "", fmt.Errorf("workflow.runChildrenFieldUpdate: the record mutator cannot list child records")
	}
	if childVia == "" {
		return "", fmt.Errorf("workflow.runChildrenFieldUpdate: childVia is empty")
	}
	var notes []string
	updated, after := 0, ""
	for {
		// Ask for one row beyond what the budget still allows: seeing it proves there are more.
		limit := min(childPageSize, MaxChildUpdatesPerRun-updated+1)
		ids, err := lister.ListChildIDs(ctx, evt.TenantID, evt.EntityName, childEntity, childVia, evt.RecordID, after, limit)
		if err != nil {
			return "", fmt.Errorf("workflow.runChildrenFieldUpdate: %w", err)
		}
		for _, id := range ids {
			after = id
			// A child already holding the target value is skipped and costs no budget, so a re-run
			// after a cut-off run walks past the finished rows and reaches the stragglers.
			if cur, _, rerr := mut.ReadForUpdate(ctx, childEntity, evt.TenantID, id); rerr == nil {
				if have, present := cur[field]; present && fmt.Sprint(have) == fmt.Sprint(value) {
					continue
				}
			}
			if updated >= MaxChildUpdatesPerRun {
				return "", fmt.Errorf("workflow.runChildrenFieldUpdate: WORKFLOW_CHILDREN_BUDGET_EXCEEDED: đã sửa %d bản ghi %s rồi dừng — còn bản ghi con chưa sửa (ngân sách %d bản ghi mỗi lượt)",
					updated, childEntity, MaxChildUpdatesPerRun)
			}
			n, err := writeRelatedRecord(ctx, evt, childEntity, id, field, value, mut, maxDepth)
			if err != nil {
				return "", fmt.Errorf("workflow.runChildrenFieldUpdate: %s %s (đã sửa %d bản ghi trước đó): %w", childEntity, id, updated, err)
			}
			if n != "" && len(notes) == 0 {
				notes = append(notes, n)
			}
			updated++
		}
		if len(ids) < limit {
			break
		}
	}
	msg := fmt.Sprintf("đã sửa %d bản ghi con %s qua %q", updated, childEntity, childVia)
	if updated == 0 {
		msg = fmt.Sprintf("không có bản ghi con %s nào trỏ tới bản ghi này qua %q — không có gì để cập nhật", childEntity, childVia)
	}
	return strings.Join(append([]string{msg}, notes...), "; "), nil
}

// depthCapWarning is the run-history note of a related write that landed WITHOUT firing the target's
// rules because the automation chain reached WORKFLOW_MAX_CHAIN_DEPTH.
func depthCapWarning(maxDepth int) string {
	return fmt.Sprintf("đã ghi bản ghi liên quan nhưng không kích hoạt luật của nó — chuỗi tự động đạt độ sâu tối đa (%d)", maxDepth)
}

// unlinkedWarning is the run-history note of a related field_update whose record links nothing.
func unlinkedWarning(via string) string {
	return fmt.Sprintf("bản ghi chưa liên kết %q — không có bản ghi liên quan để cập nhật", via)
}

// runSetDeadline COMPUTES a commitment instant (fire time + the action's offset) and writes it
// into the action's datetime field on the SAME record, through the SAME system-scope write path
// field_update uses. It is what turns "phiếu Khẩn phải xong trong 4 giờ" from a verbal convention
// into a value on the row.
//
// The arithmetic is ComputeFireAt — the identical function the date_offset trigger schedules with —
// so a deadline and a reminder can never disagree about what "+4 hours" means. For business_hours
// (DAILY-QUEUE #10) the tenant's working calendar is resolved through deps.sched, the SAME
// pinned-then-default lookup the timer producer uses (services.WorkflowScheduleResolver), so a
// deadline and a reminder cannot disagree about what a working hour is either.
//
// THREE states produce a wall-clock number and only two of them are legitimate — keep them apart:
//   - no resolver wired (deps.sched == nil): a WIRING bug (dispatcher built without the seam, or
//     NewScheduler snapshotted deps() before it was injected). Errors → a FAILED run says so.
//   - resolver answered "no calendar for this tenant" (nil schedule, nil error): the documented
//     CALENDAR-1 fallback (specs/023 §1) — stamp wall-clock and WARN, naming tenant and rule.
//   - resolver failed (store down): error. An infrastructure fault must not masquerade as the
//     fallback, because the stamped number is indistinguishable from a real one afterwards.
//
// The value is handed over as RFC3339 in UTC because that is one of the two shapes the record
// datetime validator accepts (services.datetimeLayouts); the write path then canonicalizes it to
// the MySQL literal exactly as a human's edit of the same field would be.
//
// Fail-open no-op: after_delete has no live record to stamp, matching runFieldUpdate. Everything
// else is a real error — a rule with no offset or no target field must surface as a FAILED run
// (W6) rather than quietly stamping "now", which would look like a deadline and be a lie.
//
// The returned string is the run-history note (ActionResult.Warning): wallClockFallbackWarning when
// a business_hours deadline was stamped on the no-calendar fallback, "" otherwise (DAILY-QUEUE #82).
func runSetDeadline(ctx context.Context, wf Workflow, evt Event, a Action, deps actionDeps) (string, error) {
	if deps.mut == nil {
		return "", fmt.Errorf("workflow.runSetDeadline: no record mutator wired")
	}
	if evt.Trigger == TriggerAfterDelete {
		return "", nil // nothing to stamp on a deleted record
	}
	field := strings.TrimSpace(a.Field)
	if field == "" {
		return "", fmt.Errorf("workflow.runSetDeadline: empty target field")
	}
	if a.Offset == nil {
		return "", fmt.Errorf("workflow.runSetDeadline: no offset configured")
	}
	if !ValidTimerUnit(a.Offset.Unit) {
		return "", fmt.Errorf("workflow.runSetDeadline: unit %q không dùng được cho set_deadline (minutes/hours/days/business_hours)", a.Offset.Unit)
	}
	sched, err := resolveDeadlineSchedule(ctx, wf, evt, a, deps)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if strings.TrimSpace(a.Mode) == DeadlineModeTightenOnly {
		wrote, err := tightenDeadline(ctx, evt, a, field, now, sched, deps.mut)
		if err != nil || !wrote {
			return "", err // !wrote: the deadline on the record was kept — policy, not a failure
		}
	} else {
		plan, err := PlanDeadline(a, evt, now, sched)
		if err != nil {
			return "", fmt.Errorf("workflow.runSetDeadline: %w", err)
		}
		value := plan.At.Format(time.RFC3339)
		if err := deps.mut.UpdateField(ctx, evt.EntityName, evt.TenantID, evt.RecordID, SystemActorID, field, value); err != nil {
			return "", fmt.Errorf("workflow.runSetDeadline: %w", err)
		}
		// The stamp goes SECOND and separately. If it fails after the deadline landed, the two fields
		// differ and every later tighten_only run treats the deadline as hand-typed — the failure mode
		// is "never shortened again", never "shortened a promise it did not make".
		if stamp := strings.TrimSpace(a.StampField); stamp != "" {
			if err := deps.mut.UpdateField(ctx, evt.EntityName, evt.TenantID, evt.RecordID, SystemActorID, stamp, value); err != nil {
				return "", fmt.Errorf("workflow.runSetDeadline: stamp %q: %w", stamp, err)
			}
		}
	}
	if a.Offset.Unit == TimerUnitBusinessHours && sched == nil {
		if strings.TrimSpace(a.CalendarID) != "" {
			return pinnedCalendarFallbackWarning, nil
		}
		return wallClockFallbackWarning, nil
	}
	return "", nil
}

// tightenAttempts bounds tightenDeadline's read-decide-write loop: each lost round means another
// write landed on the record in between, and a record still changing after three rounds fails the
// run (recorded, fail-open) rather than spinning.
const tightenAttempts = 3

// tightenDeadline is set_deadline's tighten_only write (DAILY-QUEUE #172). It decides from the
// RECORD, not from the event snapshot: the rule runs on the async dispatcher, so between "priority
// saved" and "rule runs" lies the dispatcher's queue delay — events of one record run in order on
// one worker (#444) but can wait seconds under a mass edit or an import —
// and a deadline typed by hand in that window is on the row, not in the snapshot. The same read
// covers the other order: a priority changed before the after_create event was handled has no
// stamp in its snapshot, but by the time this runs the creation tier's stamp is on the row.
//
// The write carries the deadline AND its stamp in ONE update, conditional on the version it read
// (RecordMutator.UpdateFieldsIfVersion); a save that lands between the read and the write makes it
// change nothing, and the loop reads and decides again — so a hand-typed deadline wins every race.
// wrote=false means the policy kept the deadline on the record.
func tightenDeadline(ctx context.Context, evt Event, a Action, field string, now time.Time, sched *calendar.Schedule, mut RecordMutator) (bool, error) {
	stamp := strings.TrimSpace(a.StampField)
	for attempt := 1; ; attempt++ {
		data, version, err := mut.ReadForUpdate(ctx, evt.EntityName, evt.TenantID, evt.RecordID)
		if err != nil {
			return false, fmt.Errorf("workflow.runSetDeadline: read record: %w", err)
		}
		cur := evt
		cur.Data = data
		plan, err := PlanDeadline(a, cur, now, sched)
		if err != nil {
			return false, fmt.Errorf("workflow.runSetDeadline: %w", err)
		}
		if !plan.Write {
			return false, nil
		}
		value := plan.At.Format(time.RFC3339)
		err = mut.UpdateFieldsIfVersion(ctx, evt.EntityName, evt.TenantID, evt.RecordID, SystemActorID,
			map[string]any{field: value, stamp: value}, version)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, ErrRecordChanged) || attempt == tightenAttempts {
			return false, fmt.Errorf("workflow.runSetDeadline: %w", err)
		}
	}
}

// DeadlinePlan is what one set_deadline action decides for one record: the instant it computed and
// whether its mode lets that instant be written. Keep says why a tighten_only action leaves the
// record alone (a sentence for the dry-run trace); it is "" whenever Write is true.
type DeadlinePlan struct {
	At    time.Time
	Write bool
	Keep  string
}

// PlanDeadline is the single decision behind a set_deadline action — the runner (runSetDeadline) and
// the dry run (services.previewAction) both call it, so "Chạy thử" cannot promise a write the run
// then refuses, or the reverse. It is pure: sched is already resolved by the caller (nil = wall-clock
// units, or the documented no-calendar fallback), now is the fire instant.
//
// tighten_only (queue #81) writes only when BOTH hold: the deadline on the record is the machine's
// own — Field parses to the same instant as StampField — and the recomputed instant is strictly
// earlier. A missing stamp is read as "not ours": a ticket created before stamps existed, or one
// whose deadline an agent typed, is never shortened, because the software cannot prove the promise
// on it was one it made.
//
// PlanDeadline reads evt.Data. The runner hands it the RECORD as read just before a conditional
// write (tightenDeadline, #172); the dry run hands it the record it previews. The event snapshot
// alone is not enough for tighten_only — see tightenDeadline for the window it misses.
func PlanDeadline(a Action, evt Event, now time.Time, sched *calendar.Schedule) (DeadlinePlan, error) {
	if a.Offset == nil {
		return DeadlinePlan{}, fmt.Errorf("workflow.PlanDeadline: no offset configured")
	}
	mode := strings.TrimSpace(a.Mode)
	switch mode {
	case "", DeadlineModeOverwrite, DeadlineModeTightenOnly:
	default:
		return DeadlinePlan{}, fmt.Errorf("workflow.PlanDeadline: mode %q không hợp lệ (overwrite/tighten_only)", a.Mode)
	}
	stampField := strings.TrimSpace(a.StampField)
	if mode == DeadlineModeTightenOnly && stampField == "" {
		return DeadlinePlan{}, fmt.Errorf("workflow.PlanDeadline: mode tighten_only cần stampField — không có dấu máy thì không phân biệt được hạn gõ tay")
	}
	from := now
	switch strings.TrimSpace(a.Anchor) {
	case "", DeadlineAnchorNow:
	case DeadlineAnchorCreatedAt:
		if evt.CreatedAt.IsZero() {
			return DeadlinePlan{}, fmt.Errorf("workflow.PlanDeadline: anchor created_at but the event carries no creation instant")
		}
		from = evt.CreatedAt
	default:
		return DeadlinePlan{}, fmt.Errorf("workflow.PlanDeadline: anchor %q không hợp lệ (now/created_at)", a.Anchor)
	}
	at, err := ComputeFireAt(TimerConfig{Offset: *a.Offset, Direction: TimerDirectionAfter}, from, sched)
	if err != nil {
		return DeadlinePlan{}, fmt.Errorf("workflow.PlanDeadline: %w", err)
	}
	plan := DeadlinePlan{At: at}
	if mode != DeadlineModeTightenOnly {
		plan.Write = true
		return plan, nil
	}
	stamp, _, stamped := ParseTimerDate(evt.Data[stampField])
	current, _, has := ParseTimerDate(evt.Data[strings.TrimSpace(a.Field)])
	switch {
	case !stamped:
		plan.Keep = "bản ghi chưa có dấu hạn máy (" + stampField + ") — hạn hiện có coi như gõ tay, không rút ngắn"
	case !has || !current.Equal(stamp):
		plan.Keep = "hạn trên bản ghi khác dấu hạn máy — là hạn gõ tay, không bao giờ bị đè"
	case !at.Before(current):
		plan.Keep = "hạn mới không sớm hơn hạn đang có — giữ hạn đã hứa"
	default:
		plan.Write = true
	}
	return plan, nil
}

// wallClockFallbackWarning is the run-history note on a business_hours deadline stamped without a
// calendar. It names the consequence and the fix in the admin's words, because this string is what
// the run-history screen prints under an otherwise green `success` row.
const wallClockFallbackWarning = "Tổ chức chưa có lịch làm việc mặc định dùng được — hạn này tính theo giờ treo tường " +
	"(không trừ đêm, cuối tuần, ngày nghỉ). Đặt một lịch làm mặc định ở Quản trị → Lịch làm việc."

// pinnedCalendarFallbackWarning is the same note for a deadline whose PINNED calendar (queue #79)
// no longer resolves — deleted after the rule was saved, or unreadable. Telling this admin to set a
// default calendar would change nothing: the default is not what this action reads. The way out
// names the control that exists: the "Lịch làm việc" dropdown of the "Đặt hạn xử lý" action in
// Studio → Tự động hoá, whose first entry clears the pin.
const pinnedCalendarFallbackWarning = "Lịch làm việc ghim cho hành động này không còn dùng được (đã bị xoá hoặc không đọc được) — " +
	"hạn này tính theo giờ treo tường (không trừ đêm, cuối tuần, ngày nghỉ). Mở luật ở Studio → Tự động hoá, " +
	"ô «Lịch làm việc» của hành động Đặt hạn xử lý: chọn lịch khác, hoặc «Lịch mặc định của tổ chức»."

// TimerWallClockWarning is the run-history note on a date_offset run whose wake-up was PARKED on
// the wall-clock fallback: the rule counts in business_hours but the tenant had no usable default
// working calendar when the record was saved, so the reminder fired at "date + N clock hours"
// (DAILY-QUEUE #105). It sits on every action of that run — the whole run happened at that instant.
const TimerWallClockWarning = "Lúc hẹn giờ cho lần chạy này, tổ chức chưa có lịch làm việc mặc định dùng được — giờ chạy " +
	"tính theo giờ treo tường (không trừ đêm, cuối tuần, ngày nghỉ). Đặt một lịch làm mặc định ở Quản trị → Lịch làm việc."

// TimerPinnedCalendarWarning is the same note for a date_offset rule whose PINNED calendar
// (timer.calendarId) no longer resolved when the wake-up was parked. The way out is the rule's own
// "Lịch làm việc" choice, not the tenant default.
const TimerPinnedCalendarWarning = "Lúc hẹn giờ cho lần chạy này, lịch làm việc ghim cho luật không còn dùng được (đã bị xoá " +
	"hoặc không đọc được) — giờ chạy tính theo giờ treo tường (không trừ đêm, cuối tuần, ngày nghỉ). Mở luật ở Studio → " +
	"Tự động hoá, ô «Lịch làm việc» của mốc thời gian: chọn lịch khác, hoặc lịch mặc định của tổ chức."

// TimerRepeatWallClockWarning is the note for occurrence 2..N of a business_hours timer rule: the
// repeat re-park recomputes the base instant without a calendar read (rearmTimerRepeat), so the
// instant is wall-clock BY DESIGN — even though the calendar was fine for occurrence 1 (#447).
const TimerRepeatWallClockWarning = "Đây là lần lặp lại của luật hẹn giờ theo giờ làm việc — các lần lặp được tính theo giờ treo tường " +
	"(không trừ đêm, cuối tuần, ngày nghỉ), dù lần đầu đã dùng lịch làm việc."

// runSetElapsed MEASURES how many working minutes the record took — from its creation
// (evt.CreatedAt) to the event's instant (evt.OccurredAt) — and writes that whole number into the action's number
// field on the SAME record, through the same system-scope write path field_update uses
// (DAILY-QUEUE #41). With an after_update rule on "status changedTo Resolved" it is "phiếu này
// mất bao nhiêu phút giờ làm việc", stored as data a report can group and average.
//
// Working time is calendar.BusinessDurationBetween over the tenant's DEFAULT calendar, resolved
// through the SAME three-state lookup set_deadline uses (resolveBusinessSchedule): no resolver or
// a failing store is an ERROR; "no usable default calendar" measures wall-clock and says so in
// the run history (setElapsedWallClockWarning), exactly like the deadline it is the other half of.
//
// Minutes are rounded DOWN and handed over as float64 — the only type the record validator's
// number arm accepts. A creation instant after the end instant (clock skew) is 0, never negative.
// A zero evt.CreatedAt is an error: measuring from year 1 would walk the calendar for ten years
// and stamp a huge number that looks like data.
func runSetElapsed(ctx context.Context, wf Workflow, evt Event, a Action, deps actionDeps) (string, error) {
	if deps.mut == nil {
		return "", fmt.Errorf("workflow.runSetElapsed: no record mutator wired")
	}
	if evt.Trigger == TriggerAfterDelete {
		return "", nil // nothing to stamp on a deleted record
	}
	field := strings.TrimSpace(a.Field)
	if field == "" {
		return "", fmt.Errorf("workflow.runSetElapsed: empty target field")
	}
	if evt.CreatedAt.IsZero() {
		return "", fmt.Errorf("workflow.runSetElapsed: the record's creation instant is unknown — refusing to measure from nothing")
	}
	sched, err := resolveBusinessSchedule(ctx, "runSetElapsed", wf, evt, a, deps)
	if err != nil {
		return "", err
	}
	// The clock stops at the EVENT's instant — the commit Emit stamped, or the scheduler's / timer's
	// fire instant — not when a worker drained the event: the dispatcher buffer can hold it for
	// seconds under a mass edit, and that wait is the automation's, not the ticket's. An unstamped
	// event (a direct caller) measures to now.
	end := evt.OccurredAt
	if end.IsZero() {
		end = time.Now().UTC()
	}
	var elapsed time.Duration
	if sched != nil {
		elapsed = sched.BusinessDurationBetween(evt.CreatedAt, end)
	} else if end.After(evt.CreatedAt) {
		elapsed = end.Sub(evt.CreatedAt)
	}
	minutes := float64(elapsed / time.Minute)
	if err := deps.mut.UpdateField(ctx, evt.EntityName, evt.TenantID, evt.RecordID, SystemActorID, field, minutes); err != nil {
		return "", fmt.Errorf("workflow.runSetElapsed: %w", err)
	}
	if sched == nil {
		return setElapsedWallClockWarning, nil
	}
	return "", nil
}

// setElapsedWallClockWarning is set_elapsed's run-history note on the no-calendar fallback — the
// wallClockFallbackWarning sentence, about a measured number instead of a deadline.
const setElapsedWallClockWarning = "Tổ chức chưa có lịch làm việc mặc định dùng được — số phút này tính theo giờ treo tường " +
	"(tính cả đêm, cuối tuần, ngày nghỉ). Đặt một lịch làm mặc định ở Quản trị → Lịch làm việc."

// resolveDeadlineSchedule returns the working calendar a business_hours deadline must be computed
// through, or nil for the wall-clock units (which never touch the store) and for the documented
// no-calendar fallback. See runSetDeadline for why the three nil-producing states are not one.
func resolveDeadlineSchedule(ctx context.Context, wf Workflow, evt Event, a Action, deps actionDeps) (*calendar.Schedule, error) {
	if a.Offset.Unit != TimerUnitBusinessHours {
		return nil, nil
	}
	return resolveBusinessSchedule(ctx, "runSetDeadline", wf, evt, a, deps)
}

const (
	wallClockFallbackLogMsg = "workflow: working-time action fell back to wall-clock — no usable default business calendar for this tenant"
	pinnedFallbackLogMsg    = "workflow: working-time action fell back to wall-clock — its pinned business calendar is gone or unreadable"
)

// resolveBusinessSchedule is the three-state calendar lookup every working-time action shares
// (set_deadline in business_hours, set_elapsed always): no resolver wired → error, resolver failed
// → error, resolver answered "no usable default calendar" → (nil, nil) + WARN. op names the caller
// in the error so a FAILED run says which action tripped.
func resolveBusinessSchedule(ctx context.Context, op string, wf Workflow, evt Event, a Action, deps actionDeps) (*calendar.Schedule, error) {
	if deps.sched == nil {
		return nil, fmt.Errorf("workflow.%s: working time needs a schedule resolver and none is wired — refusing to compute wall-clock under a working-hours label", op)
	}
	// "" = the tenant's DEFAULT calendar. A set_deadline may pin its own (queue #79) — the same
	// pinned-then-default lookup the date_offset trigger's TimerConfig.CalendarID goes through.
	// set_elapsed never carries a pin (its config does not store one), so it reads the default.
	calendarID := strings.TrimSpace(a.CalendarID)
	sched, err := deps.sched.ResolveSchedule(ctx, evt.TenantID, calendarID)
	if err != nil {
		return nil, fmt.Errorf("workflow.%s: resolve business calendar: %w", op, err)
	}
	if sched == nil {
		logger := deps.logger
		if logger == nil {
			logger = zap.NewNop()
		}
		// "no USABLE DEFAULT calendar", not "no calendar": the resolver answers nil both when the
		// tenant has none and when it has calendars but none marked default (or the default cannot
		// be parsed — logged separately by the resolver). The old wording was false in the second
		// case, which is the one an admin reaches by un-defaulting or deleting a calendar. A pinned
		// calendar that no longer resolves is a third fact, and the line says so.
		msg := wallClockFallbackLogMsg
		if calendarID != "" {
			msg = pinnedFallbackLogMsg
		}
		// #445: a scheduled firing shares one warnedOnce set, so 5000 matched records log the
		// fallback once (the run summary already carries warned/warnings). nil set = event path.
		// The key omits the action's field, so two set_deadline actions on different fields that
		// share a fallback log only the first field; the run summary still lists the warning once.
		key := msg + "\x00" + calendarID
		if _, seen := deps.warnedOnce[key]; !seen {
			if deps.warnedOnce != nil {
				deps.warnedOnce[key] = struct{}{}
			}
			logger.Warn(msg,
				zap.String("action", a.Type),
				zap.String("tenant_id", evt.TenantID),
				zap.String("workflow_id", wf.ID),
				zap.String("entity_name", evt.EntityName),
				zap.String("field", strings.TrimSpace(a.Field)),
				zap.String("calendar_id", calendarID),
			)
		}
	}
	return sched, nil
}

// runAssign applies an assign action: resolve the next team member through the resolver, then
// write that userId into the action's target field on the SAME record, via the SAME system-scope
// write path field_update uses (RLS never blocks the automation). The Strategy selects the picker
// (both advance the per-rule cursor under a row lock — the assignment_service holds that guarantee):
//   - round_robin (default when Strategy is blank, for back-compat): NEXT member in the ring.
//   - load_based (ASSIGN-2): the member with the FEWEST active assigned records in the trigger
//     entity at this field, tie-broken by the SAME round-robin cursor.
//
// Fail-open no-ops (never an error, so the run still records success):
//   - after_delete: no live record to assign (like field_update).
//   - empty/blank team, or resolver reports ok=false (no eligible member — empty team / every
//     member disabled): nothing to assign. The resolver logs the no-member case (it has the DB
//     handle); this layer has no logger, so it stays silent — matching field_update's after_delete.
//
// actionKey is the action's cursor slot within the rule ("a0"…). workflowID is taken from wf.
func runAssign(ctx context.Context, wf Workflow, evt Event, a Action, actionKey string, resolver AssignmentResolver, mut RecordMutator) error {
	_, err := runAssignPick(ctx, wf, evt, a, actionKey, resolver, mut)
	return err
}

// runAssignPick is runAssign that also reports WHO got the record ("" = nothing was assigned), so
// runActions can hand the new owner to the actions that run after it in the same firing (#280).
func runAssignPick(ctx context.Context, wf Workflow, evt Event, a Action, actionKey string, resolver AssignmentResolver, mut RecordMutator) (string, error) {
	if resolver == nil {
		return "", fmt.Errorf("workflow.runAssign: no assignment resolver wired")
	}
	if mut == nil {
		return "", fmt.Errorf("workflow.runAssign: no record mutator wired")
	}
	if evt.Trigger == TriggerAfterDelete {
		return "", nil // nothing to assign on a deleted record
	}
	field := strings.TrimSpace(a.Field)
	if field == "" {
		return "", fmt.Errorf("workflow.runAssign: empty target field")
	}
	teamID := strings.TrimSpace(a.Team)
	if teamID == "" {
		return "", nil // no team configured → nothing to assign (fail-open no-op)
	}
	var userID string
	var ok bool
	var err error
	switch strings.TrimSpace(a.Strategy) {
	case StrategyLoadBased:
		// load_based counts active records assigned in THIS entity at THIS field, so both are threaded.
		userID, ok, err = resolver.LeastLoadedAssignee(ctx, evt.TenantID, wf.ID, actionKey, evt.EntityName, field, teamID)
	default: // "" (back-compat) or round_robin
		userID, ok, err = resolver.NextAssignee(ctx, evt.TenantID, wf.ID, actionKey, teamID)
	}
	if err != nil {
		return "", fmt.Errorf("workflow.runAssign: %w", err)
	}
	if !ok {
		return "", nil // no eligible member (empty team / all disabled) → fail-open no-op
	}
	if err := mut.UpdateField(ctx, evt.EntityName, evt.TenantID, evt.RecordID, SystemActorID, field, userID); err != nil {
		return "", fmt.Errorf("workflow.runAssign: %w", err)
	}
	// ...and hand over the record itself. Routing used to write ONLY the target field, which is
	// plain data: the rep it named could SEE the lead and not edit it, because permissions read
	// owner_id. Under the default staff role (view all / edit own) that made automatic routing
	// useless. The field stays written because installed tenants display it as "Phân công cho".
	if err := mut.SetOwner(ctx, evt.EntityName, evt.TenantID, evt.RecordID, SystemActorID, userID); err != nil {
		return "", fmt.Errorf("workflow.runAssign: set owner: %w", err)
	}
	return userID, nil
}

// maxNotifyRecipients caps one in_app_notification's role/team fan-out.
const maxNotifyRecipients = 500

// runInAppNotification writes one sys_notifications row for the action's recipient (a.UserID),
// linking it back to the triggering record + the rule. It mirrors the audit/notify sinks: the
// write is parameterized and tenant-pinned by the Notification's TenantID.
//
// "$owner" resolution (M7 MED-1 fix, review 2026-07-08): a.UserID may carry the OwnerSentinel
// ("$owner") — a recipient shorthand the FE WorkflowBuilder and the AI Builder persona both
// offer meaning "notify whoever owns this record". Before this fix the literal string "$owner"
// was inserted as sys_notifications.user_id, which is not a real user — the notification was
// silently dead on EVERY rule using the sentinel, in both hand-built and AI-proposed rules. The
// fix resolves it HERE, at fire time, to evt.OwnerUserID (the record's created_by — see the
// field doc on Event) so the literal sentinel is never persisted. If the owner could not be
// resolved (evt.OwnerUserID empty — e.g. a best-effort before-image read failed upstream), this
// falls through to the SAME empty-recipient error as an empty a.UserID: no dead row is ever
// inserted for an unresolvable "$owner".
func runInAppNotification(ctx context.Context, wf Workflow, evt Event, a Action, notif NotificationWriter, templates TemplateResolver, recipients RecipientResolver) error {
	if notif == nil {
		return fmt.Errorf("workflow.runInAppNotification: no notification writer wired")
	}
	ids, err := ResolveRecipients(ctx, evt, a.UserID, recipients)
	if err != nil {
		return fmt.Errorf("workflow.runInAppNotification: %w", err)
	}
	if len(ids) == 0 {
		// Names the recipient CONFIG (never record data): this text lands in sys_workflow_runs.
		return fmt.Errorf("workflow.runInAppNotification: empty recipient userId — %q ra không có ai trên bản ghi này", strings.TrimSpace(a.UserID))
	}
	msg, err := RenderNotification(ctx, evt, a, templates)
	if err != nil {
		return fmt.Errorf("workflow.runInAppNotification: %w", err)
	}
	// ponytail: sequential inserts, capped so a huge role cannot hold one rule firing open (the BPM
	// routing slice caps at 50). Upgrade path: batch INSERT / async fan-out if a tenant needs more.
	// Not retried as a whole: a partial failure returns the first error after trying the rest.
	if len(ids) > maxNotifyRecipients {
		ids = ids[:maxNotifyRecipients]
	}
	var firstErr error
	for _, userID := range ids {
		// One failing recipient must not starve the rest of a role / team.
		if _, err := notif.InsertNotification(ctx, Notification{
			TenantID:   evt.TenantID,
			UserID:     userID,
			Title:      msg.Title,
			Message:    msg.Message,
			EntityName: evt.EntityName,
			RecordID:   evt.RecordID,
			WorkflowID: wf.ID,
			// A rule the tenant configured fired — the inbox shows it under "Hệ thống".
			Type: "system",
		}); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("workflow.runInAppNotification: %w", err)
		}
	}
	return firstErr
}
