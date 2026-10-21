package services

// specs/048 — runtime of the synchronous "before" rules inside RecordService. The rules come from
// the SAME CachedLoader the after-commit dispatcher reads (invalidated by WorkflowService on every
// rule edit); the judgement itself is the pure workflow.CollectVetoes, the value changes (S4) the pure
// workflow.ApplyMutations. Nothing here writes the record: a veto is an error returned before the
// store call, so a refused save writes no row by construction, and a filled value only goes into the
// payload of the caller's ONE store call. The only write is the run history (S3), handed to a RunSink
// off the save's path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/workflow"
)

// ErrRecordHooksUnavailable means the before rules of the entity (or the record they judge) could
// not be read, so the write was refused rather than let through unjudged (FR-021 fail-closed). The
// handler answers 503 RECORD_HOOKS_UNAVAILABLE + Retry-After: 1.
var ErrRecordHooksUnavailable = errors.New("record hooks unavailable")

// beforeRulesLoadTimeout bounds the rule load on a cache miss. The load runs detached from the
// request's cancellation: it is single-flighted in the cache, so one caller hanging up must not
// fail the other saves waiting on the same load.
const beforeRulesLoadTimeout = 2 * time.Second

// WithBeforeHooks arms the before rules (specs/048). Production passes the workflow CachedLoader
// the dispatcher uses; nil (the default) leaves every write unjudged, as before the feature. runs
// (S3) receives the run each matched rule leaves in the history; nil records none.
func (s *RecordService) WithBeforeHooks(loader workflow.WorkflowLoader, runs *workflow.RunSink) *RecordService {
	s.beforeRules = loader
	s.beforeRuns = runs
	return s
}

// systemWriteOriginKey marks a write the engine makes on a decision already taken elsewhere. The
// key type is unexported, so no caller outside this package — least of all a request — can set it.
type systemWriteOriginKey struct{}

// The two writes exempt from vetoes (FR-019): an approval's decided outcome and a matched bank
// payment. Refusing them would leave an approval "done" and a payment "received" with the record
// unchanged — a silent failure worse than no rule.
const (
	originApprovalOutcome = "approval_outcome"
	originPaymentMatch    = "payment_match"
)

func withSystemWriteOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, systemWriteOriginKey{}, origin)
}

func systemWriteOrigin(ctx context.Context) string {
	o, _ := ctx.Value(systemWriteOriginKey{}).(string)
	return o
}

// loadBeforeRules returns the active rules of trigger on entity that apply to op. A load error is
// ErrRecordHooksUnavailable: without the rules nobody knows whether this write is allowed.
func (s *RecordService) loadBeforeRules(ctx context.Context, tenantID, entity string, t workflow.Trigger, op workflow.SaveOp) ([]workflow.Workflow, error) {
	if s.beforeRules == nil {
		return nil, nil
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), beforeRulesLoadTimeout)
	defer cancel()
	all, err := s.beforeRules.ListActive(lctx, tenantID, entity, t)
	if err != nil {
		return nil, fmt.Errorf("services.RecordService.loadBeforeRules: %w: %w", ErrRecordHooksUnavailable, err)
	}
	var out []workflow.Workflow // a new slice: the cached one is shared and read-only
	for _, wf := range all {
		if workflow.AppliesTo(wf, op) {
			out = append(out, wf)
		}
	}
	return out, nil
}

// checkBeforeUpdate runs the entity's before_save rules on an update. image is the stored record
// merged with the change (computed fields recomputed); before is the stored record, read by Update
// itself (found/readErr describe that read); clean is the payload the store call will persist. With
// no rule this is a cache lookup and nothing else — the read stays best-effort exactly as before.
// With rules, an unreadable record is refused (503) and a missing one is 404, never judged against
// the payload alone. The value-changing rules write into clean and image (S4) before the vetoes
// judge image; commit — called once the update is stored — leaves their runs.
func (s *RecordService) checkBeforeUpdate(ctx context.Context, scope RecordScope, tenantID, entity, id string, specs []FieldSpec, clean, image, before map[string]any, found bool, readErr error) (commit func(), err error) {
	rules, err := s.loadBeforeRules(ctx, tenantID, entity, workflow.TriggerBeforeSave, workflow.SaveOpUpdate)
	if err != nil || len(rules) == 0 {
		return func() {}, err
	}
	if !found {
		if readErr == nil || errors.Is(readErr, ErrRecordNotFound) {
			// A record the caller reaches only through a share is 403, not 404 (#314) — the same
			// answer the entity gives with no rule at all.
			return nil, s.ExplainWriteMiss(ctx, scope, tenantID, entity, id, ErrRecordNotFound)
		}
		return nil, fmt.Errorf("services.RecordService.checkBeforeUpdate: %w: %w", ErrRecordHooksUnavailable, readErr)
	}
	rep := s.newVetoReporter(scope, tenantID, entity, workflow.TriggerBeforeSave)
	outs := applyBeforeMutations(rules, workflow.SaveOpUpdate, specs, image, before, clean)
	if err := s.enforceVetoes(ctx, rep, id, workflow.CollectVetoes(rules, workflow.SaveOpUpdate, image, before)); err != nil {
		return nil, err
	}
	return func() { rep.recordMutations(id, outs) }, nil
}

// checkBeforeDelete judges a delete against the entity's before_delete rules (S3), on the record as
// stored — read by Delete itself (found/readErr describe that read). With no rule this is a cache
// lookup and nothing else: the read stays best-effort exactly as before. With rules, an unreadable
// record is refused (503), never deleted unjudged. A record the read did not find is left to
// SoftDeleteRecord, which looks under the SAME row scope: it answers 404 — or 403 for a record the
// caller only sees through a share (ExplainWriteMiss) — exactly as it did before the feature.
func (s *RecordService) checkBeforeDelete(ctx context.Context, scope RecordScope, tenantID, entity, id string, stored map[string]any, found bool, readErr error) error {
	rules, err := s.loadBeforeRules(ctx, tenantID, entity, workflow.TriggerBeforeDelete, workflow.SaveOpDelete)
	if err != nil || len(rules) == 0 {
		return err
	}
	if !found {
		if readErr == nil || errors.Is(readErr, ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("services.RecordService.checkBeforeDelete: %w: %w", ErrRecordHooksUnavailable, readErr)
	}
	return s.enforceVetoes(ctx, s.newVetoReporter(scope, tenantID, entity, workflow.TriggerBeforeDelete), id, workflow.CollectVetoes(rules, workflow.SaveOpDelete, stored, nil))
}

// createBatchHook is the before_save hook of a batch create that does not go through Create — bulk
// create atomic, CSV import. It loads, once, the rules of entity that apply to a create and returns
// judge: nil when there is none (the caller pays one in-process cache lookup), otherwise the hook of
// one new record, with at most one hidden-field lookup for the whole batch. judge runs FR-014 on
// clean — the validated payload before its auto_number claim (every caller judges first and claims
// after, so a refused row burns no number): the value-changing rules write into clean (Phase 1, S5),
// then the vetoes judge the record as it will be stored (nil = allowed). It returns what the
// value-changing rules did to that row. flush — called once, when the batch is decided — writes ONE
// summary run per rule (FR-031): the rows a veto refused, and the rows a value-changing rule changed or
// could not change among stored, the outcomes of the rows that were actually written (nil when the
// batch wrote nothing: a value on a row never stored left no mark).
func (s *RecordService) createBatchHook(ctx context.Context, scope RecordScope, tenantID, entity string, specs []FieldSpec) (judge func(clean map[string]any) ([]workflow.MutationOutcome, error), flush func(stored [][]workflow.MutationOutcome), err error) {
	rules, err := s.loadBeforeRules(ctx, tenantID, entity, workflow.TriggerBeforeSave, workflow.SaveOpCreate)
	if err != nil || len(rules) == 0 {
		return nil, nil, err
	}
	rep := s.newVetoReporter(scope, tenantID, entity, workflow.TriggerBeforeSave)
	rep.batch = map[string]*batchTally{}
	return func(clean map[string]any) ([]workflow.MutationOutcome, error) {
		// The rules read the record as it will be stored — formulas computed — on a copy (image).
		image := cloneData(clean)
		applyComputedFields(specs, image)
		outs := applyBeforeMutations(rules, workflow.SaveOpCreate, specs, image, nil, clean)
		if len(outs) > 0 {
			// A rewritten value may feed a formula: an import stores clean as it is (its formulas
			// were computed before the rules ran); the atomic batch recomputes after its claims.
			applyComputedFields(specs, clean)
		}
		if err := s.enforceVetoes(ctx, rep, "", workflow.CollectVetoes(rules, workflow.SaveOpCreate, image, nil)); err != nil {
			return nil, err
		}
		return outs, nil
	}, rep.flush, nil
}

// checkBeforeCreate is Create's hook, after every field check and before the auto_number claim (a
// refused create burns no number, FR-020): the value-changing rules write into clean (S4), then the
// vetoes judge the record as it will be stored — formulas computed, on a copy (image). commit —
// called with the new id once the record is stored — leaves the value-changing rules' runs.
func (s *RecordService) checkBeforeCreate(ctx context.Context, scope RecordScope, tenantID, entity string, specs []FieldSpec, clean map[string]any) (commit func(id string), err error) {
	rules, err := s.loadBeforeRules(ctx, tenantID, entity, workflow.TriggerBeforeSave, workflow.SaveOpCreate)
	if err != nil || len(rules) == 0 {
		return func(string) {}, err
	}
	rep := s.newVetoReporter(scope, tenantID, entity, workflow.TriggerBeforeSave)
	image := cloneData(clean)
	applyComputedFields(specs, image)
	outs := applyBeforeMutations(rules, workflow.SaveOpCreate, specs, image, nil, clean)
	if err := s.enforceVetoes(ctx, rep, "", workflow.CollectVetoes(rules, workflow.SaveOpCreate, image, nil)); err != nil {
		return nil, err
	}
	return func(id string) { rep.recordMutations(id, outs) }, nil
}

// applyBeforeMutations is Phase 1 of a save (FR-014): the value-changing before_save rules run on
// image — the record as it will be stored — and what they changed is written into clean (the payload
// the ONE store call persists, so the value travels in the same INSERT/UPDATE and the same audit row)
// and into image, whose formulas are then recomputed so Phase 2 (the vetoes) judges the final record.
// A cleared field leaves a create's payload and is sent as null on an update (JSON_MERGE_PATCH drops
// it). The rules run after the field-ACL write guard, which only ever judged what the caller sent: a
// rule may set a field the caller's role may not edit — it is Nexus's data law (R9).
func applyBeforeMutations(rules []workflow.Workflow, op workflow.SaveOp, specs []FieldSpec, image, before, clean map[string]any) []workflow.MutationOutcome {
	patch, outs := workflow.ApplyMutations(rules, op, image, before, liveFields(specs), beforeValueCheck(specs))
	for field, v := range patch {
		if v != nil {
			clean[field], image[field] = v, v
			continue
		}
		delete(image, field)
		if op == workflow.SaveOpCreate {
			delete(clean, field)
		} else {
			clean[field] = nil
		}
	}
	if len(patch) > 0 {
		applyComputedFields(specs, image)
	}
	return outs
}

// liveFields is the set a before rule may read a value from (valueSource field, {{placeholders}}):
// the entity's live fields — specs never carry a deleted one.
func liveFields(specs []FieldSpec) map[string]bool {
	live := make(map[string]bool, len(specs))
	for _, sp := range specs {
		live[sp.Name] = true
	}
	return live
}

// beforeValueCheck judges one value a rule is about to write with the SAME checks a user's save of
// that field goes through: the partial-save validator (type, range, picklist option, never blank a
// required field) and the dependent picklist pair it forms (specs/052), judged against image — the
// record as the rules before it left it, so a rule can neither store a child outside its parent's list
// nor move a parent away from the child it leaves behind. It returns the value in its stored form. A
// refused value drops that action alone — the save goes on (FR-022). A target the hook may not write
// (its field changed type after the rule was saved) is refused as at config time; a non-null empty
// value on an optional field, which the validator skips, is a clear.
func beforeValueCheck(specs []FieldSpec) workflow.ValueCheck {
	types := make(map[string]string, len(specs))
	for _, sp := range specs {
		types[sp.Name] = sp.FieldType
	}
	return func(field string, v any, image map[string]any) (any, string) {
		if beforeTargetUnsupported[types[field]] {
			return nil, "WORKFLOW_BEFORE_TARGET_UNSUPPORTED"
		}
		out, err := ValidatePartialRecord(specs, map[string]any{field: v})
		if err == nil {
			err = checkDependentPicklists(specs, map[string]any{field: out[field]}, image)
		}
		if err != nil {
			var ve *ValidationError
			if errors.As(err, &ve) {
				return nil, ve.Code
			}
			return nil, "RECORD_INVALID" // ValidatePartialRecord only returns *ValidationError today
		}
		return out[field], ""
	}
}

// enforceVetoes turns matched vetoes into the caller's 422 — except for the exempt system writes,
// which go through (value-changing rules still apply to them from S4). Either way every matched rule
// leaves its run: blocked, or skipped with the origin as the reason (FR-019, FR-030). id is empty
// on a create.
func (s *RecordService) enforceVetoes(ctx context.Context, rep *vetoReporter, id string, vs []workflow.Violation) error {
	if len(vs) == 0 {
		return nil
	}
	if origin := systemWriteOrigin(ctx); origin != "" {
		rep.record(id, vs, workflow.RunSkipped, origin)
		return nil
	}
	rep.record(id, vs, workflow.RunBlocked, "")
	return rep.refuse(ctx, vs)
}

// vetoReporter turns matched vetoes into what the caller may read (R8). A violation anchored to a
// field the caller's role may not see loses its anchor (the form shows it in the banner and the
// response never names the field); rule ids are for admins only. If the hidden set cannot be read,
// every anchor is dropped — the user still learns why the save failed, and no field name leaks.
// The hidden set is read at most once per reporter and only once a violation is anchored, so a
// batch refusing many rows costs one lookup and a save with no violation costs none.
//
// The same reporter leaves the run history of the write (record, flush): one run per matched rule,
// or — for a batch (batch non-nil) — one summary run per rule once the batch is decided.
type vetoReporter struct {
	fieldACL    fieldACLEnforcer
	log         *zap.Logger
	runs        *workflow.RunSink
	scope       RecordScope
	tenantID    string
	entity      string
	trigger     workflow.Trigger
	loaded      bool
	hidden      map[string]struct{}
	unanchorAll bool
	// batch counts, per rule, what it did to the rows of a batch; order is the rules in the order
	// they first did something.
	batch map[string]*batchTally
	order []string
}

// batchTally is one rule's summary over a batch (FR-031): rows a veto refused; rows a value-changing
// rule changed (applied) or could not change (failed, err = the first validator code).
type batchTally struct {
	blocked, applied, failed int
	err                      string
}

// tallyOf returns ruleID's batch tally, opening it (in first-seen order) if needed.
func (r *vetoReporter) tallyOf(ruleID string) *batchTally {
	t := r.batch[ruleID]
	if t == nil {
		t = &batchTally{}
		r.batch[ruleID] = t
		r.order = append(r.order, ruleID)
	}
	return t
}

func (s *RecordService) newVetoReporter(scope RecordScope, tenantID, entity string, t workflow.Trigger) *vetoReporter {
	return &vetoReporter{fieldACL: s.fieldACL, log: s.log(), runs: s.beforeRuns, scope: scope, tenantID: tenantID, entity: entity, trigger: t}
}

// record leaves each matched rule's run (FR-030): its status, the veto's anchor field and — for an
// exempt write — why it did not refuse. Field NAMES and the origin only, never a value (FR-031):
// the history is read by people the record's own field ACL never filtered. A batch tallies instead.
func (r *vetoReporter) record(recordID string, vs []workflow.Violation, status workflow.RunStatus, origin string) {
	if r.runs == nil {
		return
	}
	for _, v := range vs {
		if r.batch != nil {
			r.tallyOf(v.RuleID).blocked++
			continue
		}
		r.emit(v.RuleID, recordID, status, "", []workflow.ActionResult{{Type: workflow.ActionVeto, Field: v.Field, OK: true, Skipped: origin}})
	}
}

// recordMutations leaves the run of every value-changing rule that did something on a write that was
// stored (FR-030): success when each of its changes landed, failed — with the validator's code, also
// in the run's error — when one was refused. A rule whose condition was false, or that left the value
// as it was, leaves none. Field names and codes only, never a value (FR-031).
func (r *vetoReporter) recordMutations(recordID string, outs []workflow.MutationOutcome) {
	if r.runs == nil {
		return
	}
	var order []string
	results := map[string][]workflow.ActionResult{}
	firstErr := map[string]string{}
	for _, o := range outs {
		if _, seen := results[o.RuleID]; !seen {
			order = append(order, o.RuleID)
		}
		results[o.RuleID] = append(results[o.RuleID], workflow.ActionResult{Type: o.Type, Field: o.Field, OK: o.Error == "", Error: o.Error})
		if o.Error != "" && firstErr[o.RuleID] == "" {
			firstErr[o.RuleID] = o.Error
		}
	}
	for _, ruleID := range order {
		status := workflow.RunSuccess
		if firstErr[ruleID] != "" {
			status = workflow.RunFailed
		}
		r.emit(ruleID, recordID, status, firstErr[ruleID], results[ruleID])
	}
}

// flush writes the batch's runs, one per rule that did something (FR-031): blocked with how many rows
// it refused; or — counting only stored, the outcomes of the rows the batch actually wrote — success
// with the rows it changed, failed (the first validator code as the run's error) once it could not
// change one. Per row a rule is applied or failed, as recordMutations reads a single save. A rule
// whose changes all sat on rows never written leaves no run, like a refused single save.
func (r *vetoReporter) flush(stored [][]workflow.MutationOutcome) {
	if r.runs == nil {
		return
	}
	for _, outs := range stored {
		failed := map[string]string{}
		var ruleIDs []string
		for _, o := range outs {
			if _, seen := failed[o.RuleID]; !seen {
				ruleIDs = append(ruleIDs, o.RuleID)
				failed[o.RuleID] = ""
			}
			if o.Error != "" && failed[o.RuleID] == "" {
				failed[o.RuleID] = o.Error
			}
		}
		for _, ruleID := range ruleIDs {
			t := r.tallyOf(ruleID)
			if code := failed[ruleID]; code != "" {
				t.failed++
				if t.err == "" {
					t.err = code
				}
				continue
			}
			t.applied++
		}
	}
	for _, ruleID := range r.order {
		t := r.batch[ruleID]
		status := workflow.RunSuccess
		switch {
		case t.blocked > 0:
			status = workflow.RunBlocked
		case t.failed > 0:
			status = workflow.RunFailed
		}
		r.emit(ruleID, "", status, t.err, map[string]int{"blocked": t.blocked, "applied": t.applied, "failed": t.failed})
	}
}

// emit hands one run to the sink; errCode (a machine code, never a value) is the run's error column.
func (r *vetoReporter) emit(ruleID, recordID string, status workflow.RunStatus, errCode string, result any) {
	body, err := json.Marshal(result)
	if err != nil { // fixed shapes of strings, bools and ints — the run is still worth recording
		r.log.Warn("services.vetoReporter.emit: actions_result not encodable, run recorded without it",
			zap.String("tenant_id", r.tenantID), zap.String("workflow_id", ruleID), zap.Error(err))
		body = nil
	}
	now := time.Now().UTC()
	r.runs.Record(workflow.RunInput{TenantID: r.tenantID, WorkflowID: ruleID, TriggerType: string(r.trigger),
		RecordID: recordID, Status: status, Error: errCode, ActionsResult: body, StartedAt: now, FinishedAt: now})
}

// refuse builds 422 RECORD_HOOK_VETO from a non-empty vs: the first violation's message is the
// error, every violation rides details.violations, details.field is the first anchor still visible.
func (r *vetoReporter) refuse(ctx context.Context, vs []workflow.Violation) error {
	if !r.loaded && r.fieldACL != nil && anyAnchored(vs) {
		r.loaded = true
		h, err := r.fieldACL.HiddenFields(ctx, r.tenantID, r.scope.RoleID, r.entity, r.scope.IsAdmin)
		if err != nil {
			r.log.Warn("services.vetoReporter.refuse: hidden fields unavailable, veto anchors dropped",
				zap.String("tenant_id", r.tenantID), zap.String("user_id", r.scope.UserID), zap.String("entity_name", r.entity), zap.Error(err))
			r.unanchorAll = true
		}
		r.hidden = h
	}
	out := make([]workflow.Violation, len(vs))
	for i, v := range vs {
		if _, isHidden := r.hidden[v.Field]; isHidden || r.unanchorAll {
			v.Field = ""
		}
		if !r.scope.IsAdmin {
			v.RuleID = ""
		}
		out[i] = v
	}
	ve := &ValidationError{Code: "RECORD_HOOK_VETO", Msg: out[0].Message, Field: out[0].Field,
		Details: map[string]any{"violations": out}}
	if out[0].Field != "" {
		ve.Details["field"] = out[0].Field
	}
	return ve
}

func anyAnchored(vs []workflow.Violation) bool {
	for _, v := range vs {
		if v.Field != "" {
			return true
		}
	}
	return false
}
