package services

// W6 — workflow run-history reads (GET /admin/workflows/:id/runs, GET /admin/workflows/runs)
// and the dry-run endpoint (POST /admin/workflows/:id/test). Both sit on the WorkflowService so
// they share the tenant-pinned repo + entity/field metadata the CRUD surface already uses.
//
// The dry-run evaluates a stored rule against ONE existing record WITHOUT any side effect: no
// record mutation, no email/webhook/notification send — email/webhook actions return a "would
// send" preview rendered by the SAME workflow.RenderTemplate / webhook-payload code the live
// engine uses, and field_update returns a before→after preview. It is the "rule sống" demo
// unlock (plan §3.4).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/credential"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

// workflowRunReader is the consumer-side slice of the workflow store the runs API needs.
// Satisfied by *workflow.SQLRepository. tenant_id pins every query.
type workflowRunReader interface {
	ListRuns(ctx context.Context, tenantID, workflowID string, limit, offset int) ([]workflow.RunRecord, int, error)
	ListRecentRuns(ctx context.Context, tenantID string, limit int, workflowIDs []string) ([]workflow.RunRecord, error)
}

// workflowRecordReader is the consumer-side slice of the dynamic record store the dry-run needs:
// read ONE record (admin scope — the rule evaluates tenant data, not one user's slice).
// Satisfied by *dynrepo.Repository.
type workflowRecordReader interface {
	GetRecord(ctx context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error)
}

// WithRunReader injects the run-history read store (production: the same workflow repo).
// Chainable; nil leaves the runs API answering WORKFLOW_RUNS_UNAVAILABLE.
func (s *WorkflowService) WithRunReader(r workflowRunReader) *WorkflowService {
	s.runs = r
	return s
}

// WithRecordReader injects the dynamic record reader backing the dry-run endpoint. Chainable;
// nil leaves POST /test answering WORKFLOW_TEST_UNAVAILABLE.
func (s *WorkflowService) WithRecordReader(r workflowRecordReader) *WorkflowService {
	s.records = r
	return s
}

// runsLimitDefault/-Max mirror the house list-pagination rules (default 20, hard cap 100).
const (
	runsLimitDefault = 20
	runsLimitMax     = 100
)

// clampRunsPage normalizes limit/offset: limit defaults to 20 and is hard-capped at 100; a
// negative offset becomes 0. Silent clamping mirrors the record-list atoiDefault behaviour.
func clampRunsPage(limit, offset int) (int, int) {
	if limit < 1 {
		limit = runsLimitDefault
	}
	if limit > runsLimitMax {
		limit = runsLimitMax
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// ListWorkflowRuns returns one workflow's run history newest-first (limit/offset paged) plus the
// total run count. Unknown / cross-tenant workflow → WORKFLOW_NOT_FOUND (404) — the existence
// check runs FIRST so the runs read can never probe another tenant's rule.
func (s *WorkflowService) ListWorkflowRuns(ctx context.Context, tenantID, workflowID string, limit, offset int) ([]workflow.RunRecord, int, error) {
	if s.runs == nil {
		return nil, 0, invalid("WORKFLOW_RUNS_UNAVAILABLE", "run history chưa được cấu hình trên server")
	}
	if _, err := s.GetWorkflow(ctx, tenantID, workflowID); err != nil {
		return nil, 0, err
	}
	limit, offset = clampRunsPage(limit, offset)
	rows, total, err := s.runs.ListRuns(ctx, tenantID, workflowID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("services.WorkflowService.ListWorkflowRuns: %w", err)
	}
	if rows == nil {
		rows = []workflow.RunRecord{}
	}
	return rows, total, nil
}

// runsFilterMaxIDs caps the rule list of a recent-runs read (?workflowIds=): the automation panel
// of one app sends the ids of the rules it lists. Over the cap is refused, never truncated — a
// truncated list would answer for fewer rules than the panel shows.
const runsFilterMaxIDs = 100

// ListRecentWorkflowRuns returns the tenant's most recent runs (newest first, capped). The cheap
// cross-rule "recent activity" read — no COUNT. workflowIDs nil = every rule; non-nil = only those
// rules (queue #297 — one app's panel), so an empty non-nil list is "no rule" and returns no run
// without a query (never widened to "every rule").
func (s *WorkflowService) ListRecentWorkflowRuns(ctx context.Context, tenantID string, limit int, workflowIDs []string) ([]workflow.RunRecord, error) {
	if s.runs == nil {
		return nil, invalid("WORKFLOW_RUNS_UNAVAILABLE", "run history chưa được cấu hình trên server")
	}
	if len(workflowIDs) > runsFilterMaxIDs {
		return nil, &ValidationError{Code: "WORKFLOW_RUNS_FILTER_INVALID",
			Msg:     fmt.Sprintf("lọc lịch sử chạy theo tối đa %d luật mỗi lần (đang có %d)", runsFilterMaxIDs, len(workflowIDs)),
			Details: map[string]any{"max": runsFilterMaxIDs}}
	}
	if workflowIDs != nil && len(workflowIDs) == 0 {
		return []workflow.RunRecord{}, nil
	}
	limit, _ = clampRunsPage(limit, 0)
	rows, err := s.runs.ListRecentRuns(ctx, tenantID, limit, workflowIDs)
	if err != nil {
		return nil, fmt.Errorf("services.WorkflowService.ListRecentWorkflowRuns: %w", err)
	}
	if rows == nil {
		rows = []workflow.RunRecord{}
	}
	return rows, nil
}

// ---------------------------------------------------------------------------------------------
// Dry-run (POST /admin/workflows/:id/test)
// ---------------------------------------------------------------------------------------------

// WorkflowTestStep is one row of the dry-run trace. Kind is trigger|condition|action. Matched is
// set on trigger/condition steps (did this predicate hold for the record); Evaluated is set on
// action steps (would this action run, i.e. every condition matched and — for field_update —
// the WF-01 target guard passed). Detail carries the kind-specific payload (rendered previews).
type WorkflowTestStep struct {
	Step      int            `json:"step"`
	Kind      string         `json:"kind"`
	Matched   *bool          `json:"matched,omitempty"`
	Evaluated *bool          `json:"evaluated,omitempty"`
	Detail    map[string]any `json:"detail"`
}

// WorkflowTestResult is the dry-run response: overall match verdict + per-step trace.
type WorkflowTestResult struct {
	WorkflowID string             `json:"workflowId"`
	EntityName string             `json:"entityName"`
	RecordID   string             `json:"recordId"`
	Trigger    string             `json:"trigger"`
	Op         string             `json:"op,omitempty"`
	Matched    bool               `json:"matched"`
	Trace      []WorkflowTestStep `json:"trace"`
}

// WorkflowTestInput is the write a dry-run simulates (specs/048 S6). RecordID is the stored record the
// write starts from — required, except on a create, whose image may be the changes alone. Op is the
// write: "create" | "update"; empty = the rule's own (after_create ⇒ create, after_update and
// before_save ⇒ update, after_delete and before_delete ⇒ delete; scheduled and date_offset judge the
// record as it is). Changes are the fields the write sets, validated like a save of those fields; a
// delete sets none.
type WorkflowTestInput struct {
	RecordID string
	Op       string
	Changes  map[string]any
	// WithEntityRules (#442, spec 048 Nợ 18): judge the rule inside the entity's whole before_save
	// chain — the other ACTIVE rules run in save order (created_at, id): older ones before it, newer
	// after, and a veto reads the final image. false = the rule alone, as before.
	WithEntityRules bool
}

// TestWorkflow dry-runs a stored rule against one EXISTING record as it is — DryRun with no simulated
// change, the contract before specs/048 S6.
func (s *WorkflowService) TestWorkflow(ctx context.Context, tenantID, workflowID, recordID string) (*WorkflowTestResult, error) {
	return s.DryRun(ctx, tenantID, workflowID, WorkflowTestInput{RecordID: recordID})
}

// DryRun rehearses a stored rule on a simulated write with ZERO side effects: the only I/O is
// metadata + at most one record read. The image the rule reads is the stored record merged with the
// changes (formulas recomputed, as a save recomputes them); on an update the edge operators
// (changed/changedTo) compare it with the record as stored — so an after_update or before_save rule
// written with changedTo can be rehearsed at all. A before rule previews what the save would do: a
// veto {message, wouldBlock}, a fill or a normalize {before, after, wouldApply, error?} through the
// code the save runs (workflow.PreviewMutations + the save's value check). The rule is judged alone:
// the other before rules of the entity are not applied first.
//
// Errors: unknown / cross-tenant workflow ⇒ WORKFLOW_NOT_FOUND (404); op other than create/update ⇒
// WORKFLOW_TEST_OP_INVALID; a change a save would refuse, or any change on a delete ⇒
// WORKFLOW_TEST_CHANGES_INVALID (details.field + details.reason = the record validator's code); no
// recordId outside a create ⇒ WORKFLOW_TEST_RECORD_REQUIRED; a recordId that does not resolve inside
// the rule's entity (unknown, soft-deleted, other entity, other tenant) ⇒ RECORD_NOT_FOUND (404).
func (s *WorkflowService) DryRun(ctx context.Context, tenantID, workflowID string, in WorkflowTestInput) (*WorkflowTestResult, error) {
	if s.records == nil {
		return nil, invalid("WORKFLOW_TEST_UNAVAILABLE", "dry-run chưa được cấu hình trên server")
	}
	rec, err := s.GetWorkflow(ctx, tenantID, workflowID)
	if err != nil {
		return nil, err
	}
	cond, actions, err := decodeStoredRule(rec)
	if err != nil {
		return nil, fmt.Errorf("services.WorkflowService.DryRun: %w", err)
	}
	trigger := workflow.Trigger(rec.Trigger)
	op, err := dryRunOp(trigger, cond, in.Op)
	if err != nil {
		return nil, err
	}
	if op == workflow.SaveOpDelete && len(in.Changes) > 0 {
		return nil, invalid("WORKFLOW_TEST_CHANGES_INVALID", "quy tắc chạy khi xoá — lần xoá không đổi trường nào, bỏ changes")
	}
	recordID := strings.TrimSpace(in.RecordID)
	if recordID == "" && op != workflow.SaveOpCreate {
		return nil, invalid("WORKFLOW_TEST_RECORD_REQUIRED", "recordId là bắt buộc để chạy thử workflow")
	}
	// WF-01 parity in the preview: the field types turn a PRE-EXISTING bad target into an error row
	// instead of a bogus "would apply"; the specs validate the change and run the save's checks.
	fieldTypes, specs, err := s.dryRunFields(ctx, tenantID, rec.EntityName)
	if err != nil {
		return nil, err
	}
	changes, err := dryRunChanges(specs, in.Changes)
	if err != nil {
		return nil, err
	}
	row := &dynrepo.Record{}
	if recordID != "" {
		if row, err = s.records.GetRecord(ctx, rec.EntityName, tenantID, recordID, dynrepo.AdminScope()); err != nil {
			if errors.Is(err, dynrepo.ErrRecordNotFound) {
				return nil, invalid("RECORD_NOT_FOUND", "không tìm thấy bản ghi %q trong entity %q", recordID, rec.EntityName)
			}
			return nil, fmt.Errorf("services.WorkflowService.DryRun: %w", err)
		}
	}
	// A new map whenever something changes: row.Data is the store's, and the dry-run writes nothing. A
	// create judges what its validator hands the rules: the payload plus every default it fills in.
	image := row.Data
	if len(changes) > 0 || op == workflow.SaveOpCreate {
		image = mergeAfterImage(row.Data, changes)
		if op == workflow.SaveOpCreate {
			if err := fillCreateDefaults(specs, image); err != nil {
				return nil, err
			}
		}
		applyComputedFields(specs, image)
	}
	var before map[string]any // only an update has a stored record to have changed from
	if op == workflow.SaveOpUpdate {
		before = row.Data
	}
	wf := workflow.Workflow{ID: rec.ID, TenantID: tenantID, EntityName: rec.EntityName, Trigger: trigger, Condition: cond, Actions: actions}

	// Chain mode: the older rules' fills land in the image first, so this rule's trigger/conditions read
	// what a save would hand it; a veto reads the image after every rule (newer ones included).
	var newerRules []workflow.Workflow
	chain := in.WithEntityRules && trigger == workflow.TriggerBeforeSave
	var check workflow.ValueCheck
	if specs != nil {
		check = beforeValueCheck(specs)
	}
	if chain {
		var older []workflow.Workflow
		if older, newerRules, err = s.chainAround(ctx, tenantID, rec); err != nil {
			return nil, err
		}
		image = applyChainPatch(specs, image, older, op, before, check)
	}

	result := &WorkflowTestResult{
		WorkflowID: rec.ID,
		EntityName: rec.EntityName,
		RecordID:   row.ID,
		Trigger:    rec.Trigger,
		Op:         string(op),
	}

	// Step 1 — trigger: does the rule run on this write at all (an update-only rule on a create, a
	// delete rule on a save…). Without an op the dry-run simulates the rule's own write, so it does.
	step := 1
	fires := dryRunFires(wf, op)
	triggerDetail := map[string]any{"trigger": rec.Trigger}
	if op != "" {
		triggerDetail["op"] = string(op)
	}
	if cond != nil && cond.SaveOn != "" {
		triggerDetail["saveOn"] = cond.SaveOn
	}
	result.Trace = append(result.Trace, WorkflowTestStep{Step: step, Kind: "trigger", Matched: &fires, Detail: triggerDetail})

	// Steps 2..n — each condition rule evaluated INDIVIDUALLY (single-rule EvaluateWithBefore reuses
	// the engine's exact operator semantics), so the admin sees which predicate failed.
	condMatched := true
	if cond == nil || len(cond.Rules) == 0 {
		step++
		tTrue := true
		result.Trace = append(result.Trace, WorkflowTestStep{
			Step: step, Kind: "condition", Matched: &tTrue,
			Detail: map[string]any{"matchAll": true},
		})
	} else {
		// Aggregate per the condition's join mode so the preview verdict matches real dispatch
		// (workflow.EvaluateWithBefore): OR fires on ANY rule, AND (default) requires ALL. Each rule
		// is still traced individually so the admin sees which predicate matched.
		isOr := strings.EqualFold(strings.TrimSpace(cond.Match), "or")
		anyMatched, allMatched := false, true
		for _, r := range cond.Rules {
			step++
			ok := workflow.EvaluateWithBefore(&workflow.Condition{Match: "and", Rules: []workflow.Rule{r}}, image, before)
			if ok {
				anyMatched = true
			} else {
				allMatched = false
			}
			m := ok
			detail := map[string]any{
				"field": r.Field, "op": r.Op, "value": r.Value, "actual": image[r.Field],
				"match": condJoinMode(cond.Match),
			}
			if edge := strings.TrimSpace(r.Op); before != nil && (edge == workflow.OpChanged || edge == workflow.OpChangedTo) {
				detail["before"] = before[r.Field] // what the edge operator compares against
			}
			result.Trace = append(result.Trace, WorkflowTestStep{Step: step, Kind: "condition", Matched: &m, Detail: detail})
		}
		if isOr {
			condMatched = anyMatched
		} else {
			condMatched = allMatched
		}
	}
	matched := fires && condMatched
	result.Matched = matched

	// Action steps — pure previews, never executed.
	if workflow.IsBeforeTrigger(trigger) {
		steps := map[int]workflow.MutationStep{}
		mutSteps := workflow.PreviewMutations(wf, image, liveFields(specs), check)
		for _, st := range mutSteps {
			steps[st.Action] = st
		}
		vetoMatched := matched
		if chain {
			// Final image = this rule's own fills (if it runs), then the newer rules'; vetoes judge it.
			final := image
			if matched {
				final = copyImage(image)
				for _, st := range mutSteps {
					if st.Changed {
						setOrDelete(final, st.Field, st.After)
					}
				}
			}
			final = applyChainPatch(specs, final, newerRules, op, before, check)
			vetoMatched = fires && workflow.EvaluateWithBefore(cond, final, before)
		}
		for i, a := range actions {
			step++
			detail, wouldRun := previewBeforeAction(a, steps[i], pickMatched(a, matched, vetoMatched))
			result.Trace = append(result.Trace, WorkflowTestStep{Step: step, Kind: "action", Evaluated: &wouldRun, Detail: detail})
		}
		return result, nil
	}
	// A simulated create rehearses the record the create would make, and emitCreateEvents stamps
	// that record's creation as "now" — not the sample record's created_at (or none, record-less).
	createdAt := row.CreatedAt
	if op == workflow.SaveOpCreate {
		createdAt = time.Now().UTC()
	}
	evt := recordEvent(tenantID, rec.EntityName, trigger, row, image, before, createdAt)
	for _, a := range actions {
		step++
		trace, wouldRun := previewAction(ctx, rec.ID, a, evt, matched, fieldTypes, s.sched, s.templates)
		if a.Type == workflow.ActionWebhook && strings.TrimSpace(a.CredentialID) != "" {
			wouldRun, err = s.previewVaultWebhook(ctx, tenantID, a, trace, wouldRun)
			if err != nil {
				return nil, err
			}
		}
		e := wouldRun
		result.Trace = append(result.Trace, WorkflowTestStep{
			Step: step, Kind: "action", Evaluated: &e, Detail: trace,
		})
	}
	return result, nil
}

// chainAround loads the entity's ACTIVE before_save rules other than rec and splits them around it in
// save order (created_at, id — the order the loader serves a real save): older run before it, newer after.
func (s *WorkflowService) chainAround(ctx context.Context, tenantID string, rec *workflow.WorkflowRecord) (older, newer []workflow.Workflow, err error) {
	all, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("services.WorkflowService.chainAround: %w", err)
	}
	sort.SliceStable(all, func(a, b int) bool {
		if !all[a].CreatedAt.Equal(all[b].CreatedAt) {
			return all[a].CreatedAt.Before(all[b].CreatedAt)
		}
		return all[a].ID < all[b].ID
	})
	seenSelf := false
	for i := range all {
		o := &all[i]
		if o.ID == rec.ID {
			seenSelf = true
			continue
		}
		if !o.IsActive || o.EntityName != rec.EntityName || o.Trigger != string(workflow.TriggerBeforeSave) {
			continue
		}
		wf, err := o.ToWorkflow()
		if err != nil {
			return nil, nil, fmt.Errorf("services.WorkflowService.chainAround: %w", err)
		}
		if seenSelf {
			newer = append(newer, wf)
		} else {
			older = append(older, wf)
		}
	}
	return older, newer, nil
}

// pickMatched: a veto is judged on the final image, a fill on the image its turn sees.
func pickMatched(a workflow.Action, fill, veto bool) bool {
	if a.Type == workflow.ActionVeto {
		return veto
	}
	return fill
}

func copyImage(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func setOrDelete(m map[string]any, k string, v any) {
	if v == nil {
		delete(m, k)
		return
	}
	m[k] = v
}

// applyChainPatch runs rules' value-changing actions on a COPY of image (the stored row is never
// touched) through the very ApplyMutations a save runs, and recomputes formulas if anything changed.
func applyChainPatch(specs []FieldSpec, image map[string]any, rules []workflow.Workflow, op workflow.SaveOp, before map[string]any, check workflow.ValueCheck) map[string]any {
	if len(rules) == 0 {
		return image
	}
	patch, _ := workflow.ApplyMutations(rules, op, image, before, liveFields(specs), check)
	if len(patch) == 0 {
		return image
	}
	out := copyImage(image)
	for f, v := range patch {
		setOrDelete(out, f, v)
	}
	applyComputedFields(specs, out)
	return out
}

// recordEvent is the workflow.Event a rule sees for one stored record: the ONE place the dry-run
// (TestWorkflow/DryRun) and the email resend (RenderEmailForRecord) build it, so what "Chạy thử" shows
// and what "Gửi lại" sends cannot drift apart.
func recordEvent(tenantID, entity string, trigger workflow.Trigger, row *dynrepo.Record, image, before map[string]any, createdAt time.Time) workflow.Event {
	return workflow.Event{
		TenantID:    tenantID,
		EntityName:  entity,
		RecordID:    row.ID,
		Trigger:     trigger,
		Data:        image,
		Before:      before,
		OwnerUserID: row.OwnerID, // parity with the live paths: EFFECTIVE owner, not the creator
		CreatedAt:   createdAt,
	}
}

// Refusals of RenderEmailForRecord. The email-log service turns them into the 409s of the resend
// contract (EMAIL_RESEND_RULE_GONE / EMAIL_RESEND_RULE_DISABLED / EMAIL_RESEND_RECORD_GONE); ErrEmailResendUnavailable is a wiring
// fault (500), never a silent fallback to another repository.
var (
	ErrEmailRuleGone          = errors.New("services: the rule no longer has this email action")
	ErrEmailRuleDisabled      = errors.New("services: the rule is switched off")
	ErrEmailRecordGone        = errors.New("services: the record is gone or in the bin")
	ErrEmailResendUnavailable = errors.New("services: email resend is not wired on this server")
)

// RenderEmailForRecord builds the email a resend would send — the email_alert at actionIndex of the
// LIVE rule, rendered over the record's CURRENT data — with no side effect at all (specs/066 L2).
//
// The rule is read through liveRepo, the raw store the engine itself runs, NEVER through s.repo: in
// production s.repo is the draft-aware wrapper, and resending through it would mail a builder's
// unpublished wording to a real customer (and make two admins see two different mails). A nil liveRepo
// is ErrEmailResendUnavailable. The record is read under the system scope, like the engine and the
// dry-run; a missing or binned record is ErrEmailRecordGone. A rule that is deleted, an index outside
// its actions, or a slot that is no longer an email_alert is ErrEmailRuleGone. A rule that is switched
// off (is_active=false — also the operator's "Tắt ngay cho khách" emergency stop, specs/063 FR-017) is
// ErrEmailRuleDisabled: a stop must stop this channel too, and the resend would mail the CURRENT
// wording of that very rule to a real customer. The rule's trigger and condition are NOT re-evaluated:
// the mail was due when the event happened.
func (s *WorkflowService) RenderEmailForRecord(ctx context.Context, tenantID, workflowID string, actionIndex int, recordID string) (workflow.RenderedEmail, workflow.Action, error) {
	if s.liveRepo == nil || s.records == nil {
		return workflow.RenderedEmail{}, workflow.Action{}, ErrEmailResendUnavailable
	}
	rec, err := s.liveRepo.Get(ctx, tenantID, workflowID)
	if err != nil {
		if errors.Is(err, workflow.ErrWorkflowNotFound) {
			return workflow.RenderedEmail{}, workflow.Action{}, ErrEmailRuleGone
		}
		return workflow.RenderedEmail{}, workflow.Action{}, fmt.Errorf("services.WorkflowService.RenderEmailForRecord: %w", err)
	}
	_, actions, err := decodeStoredRule(rec)
	if err != nil {
		return workflow.RenderedEmail{}, workflow.Action{}, fmt.Errorf("services.WorkflowService.RenderEmailForRecord: %w", err)
	}
	if actionIndex < 0 || actionIndex >= len(actions) || actions[actionIndex].Type != workflow.ActionEmailAlert {
		return workflow.RenderedEmail{}, workflow.Action{}, ErrEmailRuleGone
	}
	if !rec.IsActive {
		return workflow.RenderedEmail{}, workflow.Action{}, ErrEmailRuleDisabled
	}
	row, err := s.records.GetRecord(ctx, rec.EntityName, tenantID, recordID, dynrepo.AdminScope())
	if err != nil {
		if errors.Is(err, dynrepo.ErrRecordNotFound) {
			return workflow.RenderedEmail{}, workflow.Action{}, ErrEmailRecordGone
		}
		return workflow.RenderedEmail{}, workflow.Action{}, fmt.Errorf("services.WorkflowService.RenderEmailForRecord: %w", err)
	}
	evt := recordEvent(tenantID, rec.EntityName, workflow.Trigger(rec.Trigger), row, row.Data, nil, row.CreatedAt)
	mail, err := workflow.RenderEmail(ctx, evt, actions[actionIndex], s.templates)
	if err != nil {
		return workflow.RenderedEmail{}, workflow.Action{}, fmt.Errorf("services.WorkflowService.RenderEmailForRecord: %w", err)
	}
	return mail, actions[actionIndex], nil
}

// dryRunOp is the write a dry-run simulates: the requested create/update, or the rule's own write — a
// before_save rule narrowed to creates (saveOn=create) is rehearsed on a create, every other one on an
// update.
func dryRunOp(t workflow.Trigger, cond *workflow.Condition, requested string) (workflow.SaveOp, error) {
	switch op := workflow.SaveOp(strings.TrimSpace(requested)); op {
	case workflow.SaveOpCreate, workflow.SaveOpUpdate:
		return op, nil
	case "":
	default:
		return "", invalidField("WORKFLOW_TEST_OP_INVALID", "op", "op %q không hợp lệ — chạy thử nhận create hoặc update", requested)
	}
	switch t {
	case workflow.TriggerAfterCreate:
		return workflow.SaveOpCreate, nil
	case workflow.TriggerAfterDelete, workflow.TriggerBeforeDelete:
		return workflow.SaveOpDelete, nil
	case workflow.TriggerScheduled, workflow.TriggerDateOffset:
		return "", nil // fired by the clock: the record is judged as it is
	case workflow.TriggerBeforeSave:
		if cond != nil && strings.TrimSpace(cond.SaveOn) == workflow.SaveOnCreate {
			return workflow.SaveOpCreate, nil
		}
	}
	return workflow.SaveOpUpdate, nil
}

// fillCreateDefaults gives image what a create's ValidateRecord gives the record: the default of every
// writable field left absent or empty. A default the create would refuse (a legacy row past the
// field-edit check) refuses the rehearsal the same way.
func fillCreateDefaults(specs []FieldSpec, image map[string]any) error {
	for _, f := range specs {
		if _, computed := computedReadonlyCode(f.FieldType); computed {
			continue
		}
		if v, present := image[f.Name]; present && !isEmpty(v) {
			continue
		}
		dv, ok, err := defaultValue(f)
		if err != nil {
			return refusedValue("services.fillCreateDefaults", f.Name, fmt.Sprintf("giá trị mặc định của field %q không hợp lệ — lần tạo thật cũng bị từ chối", f.Name), err)
		}
		if ok {
			image[f.Name] = dv
		}
	}
	return nil
}

// refusedValue is the 422 of a simulated value a save would refuse: details.field names the field,
// details.reason is the record validator's code. Anything but a validation error is a fault (fn wraps it).
func refusedValue(fn, field, msg string, err error) error {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return fmt.Errorf("%s: %w", fn, err)
	}
	return &ValidationError{Code: "WORKFLOW_TEST_CHANGES_INVALID", Field: field, Msg: msg + ": " + ve.Msg,
		Details: map[string]any{"field": field, "reason": ve.Code}}
}

// dryRunFires reports whether the rule runs on op: a before rule per workflow.AppliesTo (saveOn
// included), an after rule on its own write only, a clock rule always.
func dryRunFires(wf workflow.Workflow, op workflow.SaveOp) bool {
	switch wf.Trigger {
	case workflow.TriggerBeforeSave, workflow.TriggerBeforeDelete:
		return workflow.AppliesTo(wf, op)
	case workflow.TriggerAfterCreate:
		return op == workflow.SaveOpCreate
	case workflow.TriggerAfterUpdate:
		return op == workflow.SaveOpUpdate
	case workflow.TriggerAfterDelete:
		return op == workflow.SaveOpDelete
	}
	return true
}

// dryRunFields reads the rule entity's live fields once: their types (the after previews' target
// guards) and specs (the change's validator, formulas, the before previews). Both nil when no field
// lister is wired or the entity is gone — the previews then skip what needs them, as before S6.
func (s *WorkflowService) dryRunFields(ctx context.Context, tenantID, entity string) (map[string]string, []FieldSpec, error) {
	_, def, err := s.validateEntity(ctx, tenantID, entity)
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	lister, ok := s.entites.(workflowFieldLister)
	if !ok || def == nil {
		return nil, nil, nil
	}
	fields, err := lister.ListFields(ctx, tenantID, def.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("services.WorkflowService.dryRunFields: %w", err)
	}
	specs, err := toFieldSpecs(fields)
	if err != nil {
		return nil, nil, fmt.Errorf("services.WorkflowService.dryRunFields: %w", err)
	}
	types := make(map[string]string, len(fields))
	for _, f := range fields {
		types[f.Name] = f.FieldType
	}
	return types, specs, nil
}

// dryRunChanges validates a simulated change the way a save of those fields is validated
// (ValidatePartialRecord: known, writable, typed, never blanking a required field), one field at a
// time in name order so a refusal names its field. An optional field sent empty is dropped, as a save
// drops it; an explicit null clears.
func dryRunChanges(specs []FieldSpec, changes map[string]any) (map[string]any, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	if specs == nil {
		return nil, invalid("WORKFLOW_TEST_CHANGES_INVALID", "không đọc được danh sách trường của entity — không chạy thử được với changes")
	}
	names := make([]string, 0, len(changes))
	for name := range changes {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string]any, len(changes))
	for _, name := range names {
		one, err := ValidatePartialRecord(specs, map[string]any{name: changes[name]})
		if err != nil {
			return nil, refusedValue("services.dryRunChanges", name, fmt.Sprintf("thay đổi giả lập của field %q không hợp lệ", name), err)
		}
		for k, v := range one {
			out[k] = v
		}
	}
	return out, nil
}

// previewBeforeAction is the trace detail of one before action. A veto says whether the save would be
// refused and with which sentence; a fill or a normalize shows the value before → after it (st, from
// workflow.PreviewMutations) and whether it would be written — or the code a save would record for it.
func previewBeforeAction(a workflow.Action, st workflow.MutationStep, matched bool) (map[string]any, bool) {
	switch a.Type {
	case workflow.ActionVeto:
		return map[string]any{"type": a.Type, "field": strings.TrimSpace(a.Field), "message": a.Message, "wouldBlock": matched}, matched
	case workflow.ActionFieldUpdate, workflow.ActionNormalize:
		would := matched && st.Changed
		detail := map[string]any{"type": a.Type, "field": st.Field, "before": st.Before, "after": st.After, "wouldApply": would}
		if st.Error != "" {
			detail["error"] = st.Error
		}
		return detail, would
	}
	return map[string]any{"type": a.Type, "error": "action type không được hỗ trợ"}, false
}

// previewVaultWebhook completes the dry-run detail of an outbound_webhook naming a vault key
// (specs/050 S2b, FR-018): the key's id/name/authType/status, the display URL (base + path, no
// query) and whether it would sign — read through Describe, which never opens a secret. A key the
// live send would refuse (gone, not ready, unopenable) previews as wouldSend=false with the SAME
// runtime code the run would record. Only a vault outage is an error.
func (s *WorkflowService) previewVaultWebhook(ctx context.Context, tenantID string, a workflow.Action, detail map[string]any, wouldRun bool) (bool, error) {
	delete(detail, "url")
	refuse := func(code, msg string) (bool, error) {
		detail["wouldSend"] = false
		detail["error"] = code + ": " + msg
		return false, nil
	}
	if s.creds == nil {
		return refuse(workflow.CodeCredentialsNotConfigured, "chưa nối kho khoá kết nối")
	}
	v, err := s.creds.Describe(ctx, tenantID, a.CredentialID)
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) && ve.Code == "CREDENTIAL_NOT_FOUND" {
			detail["credential"] = map[string]any{"id": a.CredentialID}
			return refuse(workflow.CodeCredentialNotFound, "khoá kết nối không còn trong kho của tổ chức")
		}
		return false, fmt.Errorf("services.WorkflowService.previewVaultWebhook: %w", err)
	}
	detail["credential"] = map[string]any{"id": v.ID, "name": v.Name, "authType": string(v.AuthType), "status": string(v.Status)}
	detail["url"] = workflow.CredentialDisplayURL(v.BaseURL, a.Path)
	detail["wouldSign"] = v.AuthType == credential.AuthHMAC
	switch v.Status {
	case credential.StatusNeedsSecret:
		return refuse(workflow.CodeCredentialNotReady, "khoá kết nối chưa được nhập bí mật")
	case credential.StatusKeyUnavailable:
		return refuse(workflow.CodeCredentialKeyUnavailable, "máy chủ không mở được khoá kết nối này")
	}
	return wouldRun, nil
}

// decodeStoredRule unmarshals a persisted rule's condition/actions JSON into domain types.
// A NULL condition is match-all (nil).
func decodeStoredRule(rec *workflow.WorkflowRecord) (*workflow.Condition, []workflow.Action, error) {
	var cond *workflow.Condition
	if len(rec.Condition) > 0 && string(rec.Condition) != "null" {
		var c workflow.Condition
		if err := json.Unmarshal(rec.Condition, &c); err != nil {
			return nil, nil, fmt.Errorf("decode condition: %w", err)
		}
		cond = &c
	}
	var actions []workflow.Action
	if len(rec.Actions) > 0 && string(rec.Actions) != "null" {
		if err := json.Unmarshal(rec.Actions, &actions); err != nil {
			return nil, nil, fmt.Errorf("decode actions: %w", err)
		}
	}
	return cond, actions, nil
}

// previewDeadlineSchedule resolves the working calendar the REHEARSAL must compute a business_hours
// deadline through. It mirrors runSetDeadline's three-state split exactly — that symmetry is the
// whole point: "Chạy thử" showing a different instant from the live run is the trap #9 already fell
// into once, and ComputeFireAt's business_hours arm degrades to wall-clock on a nil schedule
// instead of complaining, so an un-mirrored preview would look perfectly healthy.
func previewDeadlineSchedule(ctx context.Context, tenantID string, off workflow.TimerOffset, calendarID string, sched workflow.ScheduleResolver) (*calendar.Schedule, error) {
	if off.Unit != workflow.TimerUnitBusinessHours {
		return nil, nil
	}
	if sched == nil {
		return nil, errors.New("chưa nối được lịch làm việc vào bản chạy thử — không tính được hạn theo giờ làm việc")
	}
	return sched.ResolveSchedule(ctx, tenantID, strings.TrimSpace(calendarID)) // "" = the tenant default (queue #79)
}

// previewAction builds the side-effect-free trace detail of one action and reports whether it
// WOULD run (condition matched + — for field_update — the WF-01 target guard passes). email and
// notification templates are rendered through workflow.RenderTemplate (the live send path's
// renderer) and the webhook payload through workflow.WebhookPayloadPreview, so the previews can
// never drift from what the engine would actually send.
//
// templates is the SAME resolver the live engine renders email placeholders with (specs/059), so a
// "{{benh_nhan.email}}" preview shows the linked patient's address exactly as the send would.
func previewAction(ctx context.Context, workflowID string, a workflow.Action, evt workflow.Event, condMatched bool, fieldTypes map[string]string, sched workflow.ScheduleResolver, templates workflow.TemplateResolver) (map[string]any, bool) {
	switch a.Type {
	case workflow.ActionFieldUpdate:
		if a.Via != "" {
			return previewRelatedFieldUpdate(a, evt, condMatched, fieldTypes)
		}
		if a.ChildEntity != "" {
			return previewChildrenFieldUpdate(a, evt, condMatched)
		}
		value, resolved := workflow.ResolveFieldUpdateValue(a, evt) // what the live run writes (#333)
		detail := map[string]any{
			"type":   a.Type,
			"field":  a.Field,
			"before": evt.Data[a.Field],
			"after":  value,
		}
		wouldRun := condMatched
		if !resolved {
			detail["after"] = nil
			detail["note"] = fmt.Sprintf("giá trị %v rỗng trên bản ghi kích hoạt — không cập nhật", a.Value)
			wouldRun = false
		}
		if fieldTypes != nil {
			if ft, known := fieldTypes[a.Field]; !known {
				detail["error"] = fmt.Sprintf("field %q không tồn tại trong metadata (RECORD_UNKNOWN_FIELD)", a.Field)
				wouldRun = false
			} else if code, readonly := computedReadonlyCode(ft); readonly {
				detail["error"] = fmt.Sprintf("field %q là computed/assigned (%s) — workflow không được ghi đè (%s)", a.Field, ft, code)
				wouldRun = false
			}
		}
		detail["wouldApply"] = wouldRun
		return detail, wouldRun
	case workflow.ActionInAppNotification:
		// Same renderer as the live send: the rehearsal shows the text with the record's values in it.
		msg, err := workflow.RenderNotification(ctx, evt, a, templates)
		detail := map[string]any{
			"type":      a.Type,
			"userId":    a.UserID,
			"title":     msg.Title,
			"message":   msg.Message,
			"wouldSend": condMatched && err == nil,
		}
		if err != nil {
			detail["error"] = err.Error()
		}
		return detail, condMatched
	case workflow.ActionEmailAlert:
		// Mirror runEmailAlert's live behavior: an empty recipient after render is a send
		// failure, so the preview must not claim wouldSend (reviewer LOW — preview drift). A failed
		// read of the linked record is one too (the live action fails the run on it).
		mail, err := workflow.RenderEmail(ctx, evt, a, templates)
		detail := map[string]any{
			"type":      a.Type,
			"to":        mail.To,
			"subject":   mail.Subject,
			"body":      mail.Body,
			"wouldSend": condMatched && err == nil && mail.To != "",
		}
		switch {
		case err != nil:
			detail["error"] = err.Error()
		case condMatched && mail.To == "":
			detail["error"] = fmt.Sprintf("không có địa chỉ nhận — mẫu %q ra rỗng trên bản ghi này (trường trống, hoặc bản ghi chưa liên kết) — gửi thật sẽ lỗi", strings.TrimSpace(a.To))
		}
		return detail, condMatched
	case workflow.ActionWebhook:
		// A vault action (credentialId) gets its url/credential/wouldSign from previewVaultWebhook.
		// A url action shows its target masked like GET /admin/workflows shows it (specs/050 S3a):
		// the rehearsal reads the stored key (wouldSign), but its response is a read like any other.
		detail := map[string]any{
			"type":      a.Type,
			"url":       workflow.RedactTarget(a.URL),
			"wouldSend": condMatched,
			"wouldSign": a.Secret != "",
		}
		if payload, err := workflow.WebhookPayloadPreview(workflowID, evt); err == nil {
			detail["payload"] = payload
		}
		return detail, condMatched
	case workflow.ActionAssign:
		// Preview only (ASSIGN-1): the live action resolves the NEXT team member through the
		// round-robin cursor AT fire time. The dry-run is contractually zero-side-effect, so it
		// must NOT advance that cursor — it therefore reports the strategy/team/target field and
		// the wouldAssign verdict WITHOUT naming a concrete assignee (which would require an
		// advancing read). Concrete rotation is only visible on a real fire (run history).
		detail := map[string]any{
			"type":     a.Type,
			"strategy": a.Strategy,
			"team":     a.Team,
			"field":    a.Field,
		}
		wouldRun := condMatched
		// Parity with the field_update arm (reviewer suggestion): a target field deleted / retyped
		// AFTER the rule was saved would fail at fire time, so the preview surfaces the error row
		// instead of a misleading wouldAssign:true. assign targets a `user` (or legacy `text`) field.
		if fieldTypes != nil {
			if ft, known := fieldTypes[a.Field]; !known {
				detail["error"] = fmt.Sprintf("field %q không tồn tại trong metadata (RECORD_UNKNOWN_FIELD)", a.Field)
				wouldRun = false
			} else if code, readonly := computedReadonlyCode(ft); readonly {
				detail["error"] = fmt.Sprintf("field %q là computed/assigned (%s) — assign không được ghi (%s)", a.Field, ft, code)
				wouldRun = false
			} else if !assignTargetTypeOK(ft) {
				detail["error"] = fmt.Sprintf("field %q kiểu %q — assign chỉ ghi field user hoặc text (ASSIGN_FIELD_TYPE_INVALID)", a.Field, ft)
				wouldRun = false
			}
		}
		detail["wouldAssign"] = wouldRun
		return detail, wouldRun
	case workflow.ActionCreateRecord:
		// create_record is a first-class engine action (workflow/actions.go runCreateRecord) with
		// config-time validation and a full editor in the builder — but the dry-run's default arm
		// answered "action type không được hỗ trợ", which the FE renders as a red BLOCKED row.
		// So the highest-blast-radius rule Nexus has (one record creating another) was the one you
		// could not rehearse: the Test button, whose entire purpose is confidence before going
		// live, told you it would not work. The only way to learn otherwise was to fire it on real
		// data — exactly what the dry run exists to avoid.
		//
		// The TARGET entity's fields are deliberately not validated here: previewAction is pure
		// (no ctx, no metadata service) and fieldTypes carries the TRIGGER entity's fields only,
		// which are a different entity entirely. Checking a create_record's values against the
		// trigger's field list would invent errors that do not exist. The real guard already runs
		// at config time (checkCreateRecordTargets in workflow_service.go), so a bad target cannot
		// be saved in the first place — nothing is lost by reporting the plan honestly here.
		// Values show what the live run writes: placeholders resolved through the same path (#335).
		values, err := workflow.RenderCreateValues(ctx, evt, a, templates)
		detail := map[string]any{
			"type":        a.Type,
			"entityName":  a.CreateEntity,
			"values":      values,
			"wouldCreate": condMatched && err == nil,
		}
		if err != nil {
			detail["values"] = a.Values
			detail["error"] = err.Error()
		}
		return detail, condMatched
	case workflow.ActionSetDeadline:
		// The dry run must name the CONCRETE instant. "It would set a deadline" is not something an
		// admin can check against their own expectation; "it would set 2026-09-11T15:09:00Z" is —
		// and a +4-hours rule an admin reads back as tomorrow is a rule they fix BEFORE it stamps a
		// thousand tickets. The arithmetic below is workflow.ComputeFireAt, the same call the live
		// runner makes, so the rehearsal cannot drift from the firing.
		detail := map[string]any{
			"type":   a.Type,
			"field":  a.Field,
			"before": evt.Data[a.Field],
		}
		wouldRun := condMatched
		// after_delete has no live record to stamp: runSetDeadline returns a silent no-op. Saying
		// "sẽ chạy" here would be the rehearsal promising something the run cannot do — and the run
		// then records OK for it, so nothing downstream corrects the impression either.
		if evt.Trigger == workflow.TriggerAfterDelete {
			detail["error"] = "after_delete: không còn bản ghi để đóng dấu — action này sẽ không làm gì"
			wouldRun = false
		}
		if a.Offset == nil {
			detail["error"] = "action set_deadline thiếu 'offset' {amount, unit}"
			wouldRun = false
		} else if !workflow.ValidTimerUnit(a.Offset.Unit) {
			// The unit backstop the RUNNER has (runSetDeadline). Without it here, a rule stored by
			// some other path previews a clean instant and then fails on every real run.
			detail["offset"] = map[string]any{"amount": a.Offset.Amount, "unit": a.Offset.Unit}
			detail["error"] = fmt.Sprintf("offset.unit %q không dùng được cho set_deadline (minutes/hours/days/business_hours) — SET_DEADLINE_UNIT_INVALID", a.Offset.Unit)
			wouldRun = false
		} else {
			detail["offset"] = map[string]any{"amount": a.Offset.Amount, "unit": a.Offset.Unit}
			// The rehearsal walks the SAME calendar the live run will (DAILY-QUEUE #10). Previewing
			// business_hours against a nil schedule would quietly draw the wall-clock instant —
			// ComputeFireAt does not error on business_hours, it falls back — so "Chạy thử" would
			// show Friday 19:00 for a rule that really stamps Monday 10:00.
			resolved, serr := previewDeadlineSchedule(ctx, evt.TenantID, *a.Offset, a.CalendarID, sched)
			if serr != nil {
				// A FIXED sentence, never serr.Error(): a calendar-store failure wraps the driver
				// error, and this string is returned verbatim in the dry-run trace. Every other
				// error in this arm comes from ComputeFireAt (pure, no I/O), so this is the only
				// place a DB-layer message could reach an API response body.
				detail["error"] = "không đọc được lịch làm việc của tổ chức — thử lại, hoặc kiểm tra Quản trị → Lịch làm việc"
				wouldRun = false
			} else {
				if a.Offset.Unit == workflow.TimerUnitBusinessHours && resolved == nil {
					detail["calendar"] = "Tổ chức chưa khai lịch làm việc — hạn sẽ tính theo giờ thường"
					if a.CalendarID != "" {
						detail["calendar"] = "Lịch làm việc ghim cho hành động này không còn dùng được — hạn sẽ tính theo giờ thường"
					}
				}
				// The runner's own planner (queue #81): same anchor, same tighten_only verdict. A
				// kept deadline is policy, so it is reported as `keep`, never as `error`.
				plan, err := workflow.PlanDeadline(a, evt, time.Now().UTC(), resolved)
				if err != nil {
					detail["error"] = err.Error()
					wouldRun = false
				} else {
					detail["after"] = plan.At.Format(time.RFC3339)
					if !plan.Write {
						detail["keep"] = plan.Keep
						wouldRun = false
					}
				}
			}
		}
		for k, v := range map[string]string{"mode": a.Mode, "stampField": a.StampField, "anchor": a.Anchor, "calendarId": a.CalendarID} {
			if v != "" {
				detail[k] = v
			}
		}
		// Same field-target guards field_update previews, plus the datetime requirement — a `date`
		// or `text` target would be a FAILED run at fire time, and the rehearsal exists to say so.
		if fieldTypes != nil {
			if ft, known := fieldTypes[a.Field]; !known {
				detail["error"] = fmt.Sprintf("field %q không tồn tại trong metadata (RECORD_UNKNOWN_FIELD)", a.Field)
				wouldRun = false
			} else if code, readonly := computedReadonlyCode(ft); readonly {
				detail["error"] = fmt.Sprintf("field %q là computed/assigned (%s) — set_deadline không được ghi đè (%s)", a.Field, ft, code)
				wouldRun = false
			} else if ft != "datetime" {
				detail["error"] = fmt.Sprintf("field %q phải là kiểu datetime (đang là %s) — SET_DEADLINE_FIELD_TYPE_INVALID", a.Field, ft)
				wouldRun = false
			}
			// The stamp is written by the same system path: a stamp field deleted or retyped after
			// the rule was saved fails the run's second write, so the rehearsal says it first.
			if stamp := a.StampField; stamp != "" {
				if ft, known := fieldTypes[stamp]; !known {
					detail["error"] = fmt.Sprintf("stampField %q không tồn tại trong metadata — SET_DEADLINE_STAMP_FIELD_INVALID", stamp)
					wouldRun = false
				} else if _, readonly := computedReadonlyCode(ft); readonly || ft != "datetime" {
					detail["error"] = fmt.Sprintf("stampField %q phải là trường datetime ghi được (đang là %s) — SET_DEADLINE_STAMP_FIELD_INVALID", stamp, ft)
					wouldRun = false
				}
			}
		}
		detail["wouldApply"] = wouldRun
		return detail, wouldRun
	case workflow.ActionSetElapsed:
		return previewSetElapsed(ctx, a, evt, condMatched, fieldTypes, sched)
	case workflow.ActionStartApproval:
		// Zero side effect: the dry run NEVER arms a hồ sơ (no starter is even reachable from here).
		// The process's existence and entity were judged when the rule was saved
		// (checkStartApprovalTargets); whether the record already waits is a fire-time fact.
		return map[string]any{
			"type":       a.Type,
			"processId":  a.ProcessID,
			"wouldStart": condMatched,
		}, condMatched
	default:
		return map[string]any{"type": a.Type, "error": "action type không được hỗ trợ"}, false
	}
}

// previewSetElapsed rehearses a set_elapsed action (DAILY-QUEUE #41): the CONCRETE number of
// minutes the run would write (`after`), measured from the record's creation (`from`) to now over
// the SAME resolved calendar runSetElapsed walks — and the SAME verdict in every branch the runner
// has (after_delete no-op, unknown creation instant, no resolver, store failure, no calendar).
func previewSetElapsed(ctx context.Context, a workflow.Action, evt workflow.Event, condMatched bool, fieldTypes map[string]string, sched workflow.ScheduleResolver) (map[string]any, bool) {
	detail := map[string]any{
		"type":   a.Type,
		"field":  a.Field,
		"before": evt.Data[a.Field],
	}
	wouldRun := condMatched
	switch {
	case evt.Trigger == workflow.TriggerAfterDelete:
		detail["error"] = "after_delete: không còn bản ghi để ghi số phút — action này sẽ không làm gì"
		wouldRun = false
	case evt.CreatedAt.IsZero():
		detail["error"] = "không đọc được lúc tạo của bản ghi — lần chạy thật sẽ báo lỗi thay vì ghi một con số"
		wouldRun = false
	case sched == nil:
		detail["error"] = "chưa nối được lịch làm việc vào bản chạy thử — không tính được số phút giờ làm việc"
		wouldRun = false
	default:
		detail["from"] = evt.CreatedAt.UTC().Format(time.RFC3339)
		resolved, err := sched.ResolveSchedule(ctx, evt.TenantID, "")
		if err != nil {
			// A FIXED sentence, never err.Error(): the store error wraps the driver's message and
			// this detail is returned verbatim in the dry-run response body.
			detail["error"] = "không đọc được lịch làm việc của tổ chức — thử lại, hoặc kiểm tra Quản trị → Lịch làm việc"
			wouldRun = false
			break
		}
		now := time.Now().UTC()
		var elapsed time.Duration
		if resolved != nil {
			elapsed = resolved.BusinessDurationBetween(evt.CreatedAt, now)
		} else {
			detail["calendar"] = "Tổ chức chưa khai lịch làm việc — số phút sẽ tính theo giờ thường"
			if now.After(evt.CreatedAt) {
				elapsed = now.Sub(evt.CreatedAt)
			}
		}
		detail["after"] = float64(elapsed / time.Minute)
	}
	// Same target guards the config-time validator runs: a field deleted or retyped after the rule
	// was saved would fail at fire time, and the rehearsal exists to say so first.
	if fieldTypes != nil {
		if ft, known := fieldTypes[a.Field]; !known {
			detail["error"] = fmt.Sprintf("field %q không tồn tại trong metadata (RECORD_UNKNOWN_FIELD)", a.Field)
			wouldRun = false
		} else if code, readonly := computedReadonlyCode(ft); readonly {
			detail["error"] = fmt.Sprintf("field %q là computed/assigned (%s) — set_elapsed không được ghi đè (%s)", a.Field, ft, code)
			wouldRun = false
		} else if ft != "number" {
			detail["error"] = fmt.Sprintf("field %q phải là kiểu number (đang là %s) — SET_ELAPSED_FIELD_TYPE_INVALID", a.Field, ft)
			wouldRun = false
		}
	}
	detail["wouldApply"] = wouldRun
	return detail, wouldRun
}

// condJoinMode normalizes a stored condition Match for display in the dry-run trace: "or" stays
// "or", everything else (empty / "and") reads "and".
func condJoinMode(match string) string {
	if strings.EqualFold(strings.TrimSpace(match), "or") {
		return "or"
	}
	return "and"
}
