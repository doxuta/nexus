package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/calendar"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/workflow"
)

// workflowRepo is the consumer-side slice of the workflow store the admin service needs. Declared
// here so the service is unit-testable with an in-memory fake and does not import a concrete DB
// type. EVERY method is tenant-scoped in the real implementation (sys_workflows.tenant_id), so no
// use case can read or mutate another tenant's workflow.
type workflowRepo interface {
	Create(ctx context.Context, in workflow.CreateInput) (string, error)
	Get(ctx context.Context, tenantID, id string) (*workflow.WorkflowRecord, error)
	List(ctx context.Context, tenantID string) ([]workflow.WorkflowRecord, error)
	Update(ctx context.Context, tenantID, id string, in workflow.UpdateInput) error
	Delete(ctx context.Context, tenantID, id string) error
	// SetActive flips only is_active (pause / resume) and leaves the actions alone.
	SetActive(ctx context.Context, tenantID, id string, active bool, updatedBy string) error
	// CountActive counts the active, non-deleted rules of (entity, trigger) other than excludeID —
	// the specs/048 cap on active before rules.
	CountActive(ctx context.Context, tenantID, entityName string, trigger workflow.Trigger, excludeID string) (int, error)
}

// workflowEntityChecker reports whether an entity name resolves to a real (non-deleted) entity in
// the tenant. Satisfied by *MetaCache / the metadata repository (GetEntityByName). The workflow
// service uses it to reject a rule pointing at an entity that does not exist, so a dead rule is
// never persisted. Optional: a nil checker skips the existence check (unit tests).
type workflowEntityChecker interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
}

// workflowFieldLister is the OPTIONAL capability the WF-01 field-target guard needs on top of
// workflowEntityChecker: list the entity's field definitions so a field_update action's target
// can be checked against sys_fields. The production checker (*MetaCache) implements it (it
// satisfies EntityCatalog); a checker without it (partial unit-test fakes) skips the guard.
type workflowFieldLister interface {
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}

// WorkflowService implements the EPIC 4 admin CRUD over /admin/workflows. Every method takes the
// verified tenant id and pins all repository calls to it, so no use case can touch another
// tenant's rule. It validates the trigger/condition/actions shape BEFORE any write so a stored
// workflow always carries a runnable rule. It is HTTP-agnostic (the handler maps its typed errors
// to status codes) and holds no per-request state. After a write commits it invalidates the
// engine's cached active-workflow list so a new/edited/removed rule takes effect promptly.
type WorkflowService struct {
	repo    workflowRepo
	entites workflowEntityChecker // optional; nil → skip entity-existence check
	cache   *workflow.CachedLoader
	// runs backs the W6 run-history reads (workflow_runs.go). Optional via WithRunReader.
	runs workflowRunReader
	// records backs the W6 dry-run record read (workflow_runs.go). Optional via WithRecordReader.
	records workflowRecordReader
	// calendars backs the TIMER-1 calendarId existence check (a date_offset rule pinning a
	// specific business calendar must point at a live calendar in the SAME tenant). Optional via
	// WithCalendarChecker; nil skips the check (unit tests without a calendar store).
	calendars workflowCalendarChecker
	// sched resolves the tenant working calendar the dry-run needs to rehearse a business_hours
	// set_deadline offset against the SAME schedule the live run walks. Optional via
	// WithScheduleResolver; nil makes the rehearsal REFUSE that action rather than draw the
	// wall-clock instant the live run will not produce.
	sched workflow.ScheduleResolver
	// templates renders a dry-run email's {{relation.field}} / lookup placeholders through the SAME
	// resolver the engine sends with (specs/059). Optional via WithTemplateResolver; nil previews
	// from the record's own data only.
	templates workflow.TemplateResolver
	// teams backs the ASSIGN-1 team existence check (an assign action's team must be a live team
	// in the SAME tenant). Optional via WithTeamChecker; nil skips the DB-dependent check (unit
	// tests without a team store — the config-time strategy/field guards still run).
	teams workflowTeamChecker
	// creds backs the specs/050 S2b vault webhook: the save-time key existence check
	// (checkWebhookCredentials — required, not optional) and the dry-run's key description.
	creds workflowCredentialChecker
	// liveRepo is the RAW store for the live-only paths (DisableNow; see WithLiveRepo). nil = not wired.
	liveRepo workflowLiveRepo
	// rec + recAdapter: DisableNow's direct version entry (S7-5); nil = none.
	rec        *VersionRecorder
	recAdapter metaversion.DocAdapter
	// auditor receives one __workflow_rule__ row per committed write (specs/062 L4); noop until
	// WithAuditor. draftMode skips the row for a write that only reached the builder's draft (FR-022).
	auditor   audit.Auditor
	draftMode docWriteModer
	// approvalProcs backs the start_approval save-time check (specs/043 S9b): the named process must
	// be a live manual process of the rule's entity. Optional via WithApprovalProcessReader; nil
	// skips the lookup (unit tests) — the shape and trigger guards still run.
	approvalProcs workflowApprovalProcessReader
}

// workflowApprovalProcessReader is the consumer-side slice of the approval store the start_approval
// check needs. Satisfied by *bpm.SQLRepository (tenant-scoped GetProcess).
type workflowApprovalProcessReader interface {
	GetProcess(ctx context.Context, tenantID, id string) (*bpm.ProcessRecord, error)
}

// WithApprovalProcessReader injects the approval store slice so create/update reject a
// start_approval action naming a process that cannot take the rule's records. Chainable.
func (s *WorkflowService) WithApprovalProcessReader(r workflowApprovalProcessReader) *WorkflowService {
	s.approvalProcs = r
	return s
}

// checkStartApprovalTargets is the DB-dependent half of start_approval validation (apply path, next
// to checkCreateRecordTargets). The trigger guard needs no store: an after_delete rule has no record
// left to approve. Then each named process must exist in the tenant, be switched on, be a MANUAL
// process (an automatic one starts itself — naming it here would double-start it) and belong to the
// rule's own entity (the engine only offers a record the processes of its entity). Every refusal is
// WF_START_APPROVAL_INVALID with the action's position.
func (s *WorkflowService) checkStartApprovalTargets(ctx context.Context, tenantID, entity string, trigger workflow.Trigger, actions []WorkflowActionInput) error {
	for i, a := range actions {
		if strings.TrimSpace(a.Type) != workflow.ActionStartApproval {
			continue
		}
		if trigger == workflow.TriggerAfterDelete {
			return invalidField("WF_START_APPROVAL_INVALID", actionPointer(i),
				"start_approval không dùng được với trigger after_delete — bản ghi đã xoá, không còn gì để duyệt")
		}
		if s.approvalProcs == nil {
			continue
		}
		id := strings.TrimSpace(a.ProcessID)
		p, err := s.approvalProcs.GetProcess(ctx, tenantID, id)
		switch {
		case errors.Is(err, bpm.ErrProcessNotFound):
			return invalidField("WF_START_APPROVAL_INVALID", actionPointer(i),
				"quy trình duyệt %q không tồn tại trong tổ chức này", id)
		case err != nil:
			return fmt.Errorf("services.WorkflowService.checkStartApprovalTargets: %w", err)
		case !p.IsActive:
			return invalidField("WF_START_APPROVAL_INVALID", actionPointer(i),
				"quy trình duyệt %q đang tắt — bật nó hoặc chọn quy trình khác", p.Name)
		case p.Trigger != string(bpm.TriggerManual):
			return invalidField("WF_START_APPROVAL_INVALID", actionPointer(i),
				"quy trình %q tự khởi động theo sự kiện (%s) — chỉ quy trình 'gửi tay' mới được luật khởi động", p.Name, p.Trigger)
		case p.EntityName != entity:
			return invalidField("WF_START_APPROVAL_INVALID", actionPointer(i),
				"quy trình %q duyệt đối tượng %q, không phải %q của luật này", p.Name, p.EntityName, entity)
		}
	}
	return nil
}

// actionPointer names action i the way the builder addresses it (details.field).
func actionPointer(i int) string { return "/actions/" + strconv.Itoa(i) }

// workflowCalendarChecker is the consumer-side slice of the CALENDAR-1 store the TIMER-1
// calendarId check needs. Satisfied by *calmeta.SQLRepository (tenant-scoped GetByID).
type workflowCalendarChecker interface {
	GetByID(ctx context.Context, tenantID, id string) (*calmeta.BusinessCalendar, error)
}

// workflowTeamChecker is the consumer-side slice of the team store the ASSIGN-1 team-existence
// check needs. Satisfied by *auth.TeamRepository (tenant-scoped GetTeamByID). Mirrors the
// calendar checker: an unknown/cross-tenant id folds to ASSIGN_TEAM_INVALID.
type workflowTeamChecker interface {
	GetTeamByID(ctx context.Context, tenantID, id string) (*auth.Team, error)
}

// NewWorkflowService wires the workflow admin service over a workflow repository.
func NewWorkflowService(repo workflowRepo) *WorkflowService {
	return &WorkflowService{repo: repo, auditor: audit.NewNoopAuditor()}
}

// WithAuditor injects the audit sink for rule writes (create / update / pause-resume / delete /
// "Tắt ngay cho khách") and the draft mode that suppresses the row for a draft-only write — main.go
// passes the DraftGate because "workflow" is a draft type. Chainable; a nil auditor keeps the noop.
func (s *WorkflowService) WithAuditor(a audit.Auditor, mode docWriteModer) *WorkflowService {
	if a != nil {
		s.auditor = a
	}
	s.draftMode = mode
	return s
}

// WithEntityChecker injects the metadata entity-existence checker so create/update reject a rule
// targeting an unknown entity. Chainable.
func (s *WorkflowService) WithEntityChecker(c workflowEntityChecker) *WorkflowService {
	s.entites = c
	return s
}

// WithCache attaches the engine's cached active-workflow loader so writes invalidate the affected
// entity's cached rules AFTER commit (the engine then reloads fresh). Chainable; nil-safe.
func (s *WorkflowService) WithCache(c *workflow.CachedLoader) *WorkflowService {
	s.cache = c
	return s
}

// WithCalendarChecker injects the CALENDAR-1 store slice so create/update reject a date_offset
// rule pinning an unknown/cross-tenant calendarId (TIMER-1). Chainable.
func (s *WorkflowService) WithCalendarChecker(c workflowCalendarChecker) *WorkflowService {
	s.calendars = c
	return s
}

// WithScheduleResolver injects the CALENDAR-1 lookup the dry-run rehearses a business_hours
// set_deadline with (DAILY-QUEUE #10) — the SAME services.WorkflowScheduleResolver the engine
// runs with, so the rehearsal and the firing read one calendar. Chainable.
func (s *WorkflowService) WithScheduleResolver(r workflow.ScheduleResolver) *WorkflowService {
	s.sched = r
	return s
}

// WithTemplateResolver injects the specs/059 email placeholder resolver the dry-run previews with —
// the SAME instance main.go hands the engine (AsyncDispatcher.WithTemplateResolver). Chainable.
func (s *WorkflowService) WithTemplateResolver(r workflow.TemplateResolver) *WorkflowService {
	s.templates = r
	return s
}

// WithTeamChecker injects the team store slice so create/update reject an assign action naming an
// unknown/cross-tenant team (ASSIGN-1 → ASSIGN_TEAM_INVALID). Chainable.
func (s *WorkflowService) WithTeamChecker(c workflowTeamChecker) *WorkflowService {
	s.teams = c
	return s
}

// checkAssignTeams verifies every assign action's team points at a live team in the tenant
// (ASSIGN_TEAM_INVALID, 422 — config-time reject, same family as WORKFLOW_ENTITY_NOT_FOUND /
// WORKFLOW_TIMER_CALENDAR_NOT_FOUND). It is the DB-dependent half of assign validation, so it
// lives on the apply path (CreateWorkflow/UpdateWorkflow) next to checkTimerCalendar — NOT in the
// pure buildWorkflowRule body. No-op when no checker is wired. An empty team is already rejected
// by buildActionsJSON (ASSIGN_TEAM_INVALID), so it is skipped here.
func (s *WorkflowService) checkAssignTeams(ctx context.Context, tenantID string, actions []WorkflowActionInput) error {
	if s.teams == nil {
		return nil
	}
	for _, a := range actions {
		teamID := strings.TrimSpace(a.Team)
		if strings.TrimSpace(a.Type) == workflow.ActionInAppNotification {
			// #281: "$team:<id>" recipient — same live-team check as an assign's team.
			if spec := strings.TrimSpace(a.UserID); strings.HasPrefix(spec, workflow.RecipientTeamPrefix) {
				teamID = strings.TrimSpace(strings.TrimPrefix(spec, workflow.RecipientTeamPrefix))
			}
		} else if strings.TrimSpace(a.Type) != workflow.ActionAssign {
			continue
		}
		if teamID == "" {
			continue // empty team already rejected at config time
		}
		if _, err := s.teams.GetTeamByID(ctx, tenantID, teamID); err != nil {
			if errors.Is(err, auth.ErrTeamNotFound) {
				return invalidField("ASSIGN_TEAM_INVALID", teamID,
					"team %q của action assign không tồn tại trong tenant", teamID)
			}
			return fmt.Errorf("services.WorkflowService.checkAssignTeams: %w", err)
		}
	}
	return nil
}

// workflowCredentialChecker is the slice of the named-credential vault a rule needs (specs/050
// S2b): the key's view — never its secret — for the save-time existence check and the dry-run.
// Satisfied by *CredentialService (Describe answers CREDENTIAL_NOT_FOUND for an unknown, deleted
// or cross-tenant id).
type workflowCredentialChecker interface {
	Describe(ctx context.Context, tenantID, id string) (*CredentialView, error)
}

// WithCredentialChecker injects the vault so create/update reject an outbound_webhook naming a
// key that is not a live key of the tenant (WEBHOOK_CREDENTIAL_NOT_FOUND) and the dry-run can name
// the key. Chainable.
func (s *WorkflowService) WithCredentialChecker(c workflowCredentialChecker) *WorkflowService {
	s.creds = c
	return s
}

// checkWebhookCredentials is the DB-dependent half of a vault webhook's validation (the shape half
// is in buildActionsJSON): the named key must be a live key of THIS tenant. Unlike the other
// apply-path checks it is NOT a no-op without a checker — an unchecked key id is a pointer into
// another tenant's vault waiting for a bug, so a server that forgot the wire refuses the save.
func (s *WorkflowService) checkWebhookCredentials(ctx context.Context, tenantID string, actions []WorkflowActionInput) error {
	for i, a := range actions {
		cid := strings.TrimSpace(a.CredentialID)
		if strings.TrimSpace(a.Type) != workflow.ActionWebhook || cid == "" {
			continue
		}
		if s.creds == nil {
			return fmt.Errorf("services.WorkflowService.checkWebhookCredentials: no credential checker wired")
		}
		if _, err := s.creds.Describe(ctx, tenantID, cid); err != nil {
			var ve *ValidationError
			if errors.As(err, &ve) && ve.Code == "CREDENTIAL_NOT_FOUND" {
				return invalidField("WEBHOOK_CREDENTIAL_NOT_FOUND", fmt.Sprintf("actions[%d].credentialId", i),
					"khoá kết nối %q không có trong kho của tổ chức (hoặc đã bị xoá)", cid)
			}
			return fmt.Errorf("services.WorkflowService.checkWebhookCredentials: %w", err)
		}
	}
	return nil
}

// checkCreateRecordTargets verifies every create_record action's target entity exists in the tenant
// and each Values key is a known, client-writable field of that TARGET entity whose value type is
// valid (CREATE-RECORD, config-time reject — same family as checkAssignTeams / checkTimerCalendar).
// It is the DB-dependent half of create_record validation (buildActionsJSON did the pure shape
// check): it needs the TARGET entity's fields, which buildActionsJSON — scoped to the TRIGGER entity
// — cannot see. No-op when no entity checker is wired (unit tests). Codes:
//   - unknown/cross-tenant target entity → CREATE_RECORD_ENTITY_INVALID.
//   - an unknown Values key → RECORD_UNKNOWN_FIELD; a computed/assigned target (formula/lookup/
//     rollup/auto_number) → the SAME computedReadonlyCode family field_update/assign use (WF-01), so
//     an automation can never seed a computed/auto_number field.
//   - a literal value of the wrong type/range/picklist for its field → the ValidatePartialRecord code
//     (RECORD_INVALID_TYPE / _RANGE / _PICKLIST …). The "$owner" sentinel is SKIPPED here (it
//     resolves at fire time to a user id). Required-field completeness is enforced at RUNTIME by
//     RecordService.Create (a create_record omitting a required target field surfaces as a FAILED run
//     — the fail-open contract), not at config time, so a rule relying on target-side defaults saves.
func (s *WorkflowService) checkCreateRecordTargets(ctx context.Context, tenantID string, actions []WorkflowActionInput) error {
	if s.entites == nil {
		return nil
	}
	lister, canList := s.entites.(workflowFieldLister)
	for _, a := range actions {
		if strings.TrimSpace(a.Type) != workflow.ActionCreateRecord {
			continue
		}
		entity := strings.TrimSpace(a.CreateEntity)
		if entity == "" {
			continue // empty already rejected by buildActionsJSON
		}
		def, err := s.entites.GetEntityByName(ctx, tenantID, entity)
		if err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				return invalidField("CREATE_RECORD_ENTITY_INVALID", entity,
					"entity %q của action create_record không tồn tại trong tenant", entity)
			}
			return fmt.Errorf("services.WorkflowService.checkCreateRecordTargets: %w", err)
		}
		if !canList {
			continue // cannot list the target's fields → skip the per-field guard (partial fakes)
		}
		fields, err := lister.ListFields(ctx, tenantID, def.ID)
		if err != nil {
			return fmt.Errorf("services.WorkflowService.checkCreateRecordTargets: %w", err)
		}
		specs, err := toFieldSpecs(fields)
		if err != nil {
			return fmt.Errorf("services.WorkflowService.checkCreateRecordTargets: %w", err)
		}
		byType := make(map[string]string, len(fields))
		for _, f := range fields {
			byType[f.Name] = f.FieldType
		}
		// Collect literal (non-$owner) values so the SAME validator the record write path uses runs
		// once per action; the key-existence + WF-01 readonly guards run inline per key.
		literals := make(map[string]any, len(a.Values))
		for k, v := range a.Values {
			field := strings.TrimSpace(k)
			ft, known := byType[field]
			if !known {
				return invalidField("RECORD_UNKNOWN_FIELD", field,
					"field %q không tồn tại trong entity %q của action create_record", field, entity)
			}
			if code, readonly := computedReadonlyCode(ft); readonly {
				return invalidField(code, field,
					"field %q là computed/assigned field (%s) — create_record không được gán", field, ft)
			}
			// Everything that RESOLVES at fire time is skipped here: it is not a literal, and
			// validating it as one produces a false rejection. $record.total is the string
			// "$record.total" at config time and a currency object at fire time; a template like
			// "{{name}} — hợp đồng" is text that is not yet filled in.
			if str, ok := v.(string); ok && isDeferredValue(str) {
				continue
			}
			literals[field] = v
		}
		if len(literals) > 0 {
			if _, err := ValidatePartialRecord(specs, literals); err != nil {
				return err // already a *ValidationError with a machine code (RECORD_INVALID_TYPE …)
			}
		}
	}
	return nil
}

// templatesOf lists the free-text templates of an action that render placeholders (#335): an
// email's To/Subject/Body, a notification's Title/Message, a create_record's string values.
func templatesOf(a WorkflowActionInput) []string {
	switch strings.TrimSpace(a.Type) {
	case workflow.ActionEmailAlert:
		return []string{a.To, a.Subject, a.Body}
	case workflow.ActionInAppNotification:
		return []string{a.Title, a.Message}
	case workflow.ActionCreateRecord:
		var out []string
		for _, v := range a.Values {
			if str, ok := v.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// checkTemplateRefs is config-time parity (B6) for the specs/059 linked-record placeholders of an
// email_alert / in_app_notification / create_record: every dotted {{a.b}} in To/Subject/Body must be <a to-one relation field of the rule's
// entity>.<a field of its related entity whose value renders (templateRendersEmpty)>, one hop only —
// the ONLY shape the engine
// (WorkflowTemplateResolver) can resolve. Anything else would render "" at fire time forever, so it
// is refused now with WORKFLOW_TEMPLATE_REF_INVALID (details.field = the placeholder). Plain
// {{field}} placeholders keep their pre-059 contract (unchecked) so no stored rule or pack breaks.
// A checker that cannot list fields skips the check (partial fakes), like checkCreateRecordTargets.
func (s *WorkflowService) checkTemplateRefs(ctx context.Context, tenantID string, entityDef *metadata.EntityDefinition, actions []WorkflowActionInput) error {
	lister, canList := s.entites.(workflowFieldLister)
	if s.entites == nil || entityDef == nil || !canList {
		return nil
	}
	var refs []string
	for _, a := range actions {
		for _, name := range workflow.TemplatePlaceholders(templatesOf(a)...) {
			if strings.Contains(name, ".") {
				refs = append(refs, name)
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}
	fieldsOf := func(def *metadata.EntityDefinition) (map[string]FieldSpec, error) {
		fields, err := lister.ListFields(ctx, tenantID, def.ID)
		if err != nil {
			return nil, err
		}
		specs, err := toFieldSpecs(fields)
		if err != nil {
			return nil, err
		}
		byName := make(map[string]FieldSpec, len(specs))
		for _, sp := range specs {
			byName[sp.Name] = sp
		}
		return byName, nil
	}
	own, err := fieldsOf(entityDef)
	if err != nil {
		return fmt.Errorf("services.WorkflowService.checkTemplateRefs: %w", err)
	}
	targets := make(map[string]map[string]FieldSpec) // related entity → its fields (read once)
	for _, ref := range refs {
		via, field, _ := strings.Cut(ref, ".")
		rel, ok := own[via]
		if ok && rel.FieldType == "user" {
			// #336: a user field → a row of `users`; only the columns the resolver reads.
			if !slices.Contains(userTemplateFields, field) {
				return invalidField("WORKFLOW_TEMPLATE_REF_INVALID", ref,
					"{{%s}}: người dùng chỉ có thể in %s (dạng {{%s.email}})", ref, strings.Join(userTemplateFields, ", "), via)
			}
			continue
		}
		target := relatedEntityOf(rel)
		if !ok || rel.FieldType != "relation" || target == "" {
			return invalidField("WORKFLOW_TEMPLATE_REF_INVALID", ref,
				"{{%s}}: %q không phải trường liên kết (quan hệ) của %q — chỉ lấy được trường của bản ghi liên kết qua một trường quan hệ", ref, via, entityDef.Name)
		}
		if strings.Contains(field, ".") {
			return invalidField("WORKFLOW_TEMPLATE_REF_INVALID", ref,
				"{{%s}}: chỉ đi được MỘT bậc liên kết (dạng {{%s.<trường>}})", ref, via)
		}
		fields, read := targets[target]
		if !read {
			def, err := s.entites.GetEntityByName(ctx, tenantID, target)
			if err != nil && !errors.Is(err, metadata.ErrEntityNotFound) {
				return fmt.Errorf("services.WorkflowService.checkTemplateRefs: %w", err)
			}
			if def != nil {
				if fields, err = fieldsOf(def); err != nil {
					return fmt.Errorf("services.WorkflowService.checkTemplateRefs: %w", err)
				}
			}
			targets[target] = fields
		}
		sp, ok := fields[field]
		if !ok {
			return invalidField("WORKFLOW_TEMPLATE_REF_INVALID", ref,
				"{{%s}}: bảng %q (liên kết qua %q) không có trường %q", ref, target, via, field)
		}
		if templateRendersEmpty(sp) {
			return invalidField("WORKFLOW_TEMPLATE_REF_INVALID", ref,
				"{{%s}}: trường %q (kiểu %s) không in được vào thư — lookup/rollup chỉ tính lúc xem bản ghi, tiền tệ/nhiều lựa chọn/tệp không có dạng chữ", ref, field, sp.FieldType)
		}
	}
	return nil
}

// isDeferredValue reports whether a create_record value is resolved at FIRE time rather than being
// a config-time literal: the $owner / $record sentinels, a $record.<field> copy, or any string
// carrying a {{field}} placeholder.
//
// These cannot be type-checked now — the point of them is that their value does not exist yet — so
// the guard that catches a mistyped one is the record write itself, at fire time, which reports a
// FAILED run rather than corrupting the row. That is the same fail-open contract required fields
// already follow here.
func isDeferredValue(s string) bool {
	t := strings.TrimSpace(s)
	return t == workflow.OwnerSentinel ||
		t == workflow.RecordSentinel ||
		strings.HasPrefix(t, workflow.RecordFieldPrefix) ||
		strings.Contains(t, "{{")
}

// checkTimerCalendar verifies a timer's optional calendarId points at a live calendar in the
// tenant (WORKFLOW_TIMER_CALENDAR_NOT_FOUND, 422 — config-time reject, same family as
// WORKFLOW_ENTITY_NOT_FOUND). No-op when the timer/calendarId is absent or no checker is wired.
// NOTE: this guards CONFIG time only — a calendar deleted AFTER the rule was saved degrades to
// the wall-clock fallback at schedule time (documented TIMER-1 contract), never a dead rule.
func (s *WorkflowService) checkTimerCalendar(ctx context.Context, tenantID string, timer *WorkflowTimerInput) error {
	if s.calendars == nil || timer == nil || strings.TrimSpace(timer.CalendarID) == "" {
		return nil
	}
	if _, err := s.calendars.GetByID(ctx, tenantID, strings.TrimSpace(timer.CalendarID)); err != nil {
		if errors.Is(err, calmeta.ErrCalendarNotFound) {
			return invalid("WORKFLOW_TIMER_CALENDAR_NOT_FOUND",
				"calendarId %q không tồn tại trong tenant", timer.CalendarID)
		}
		return fmt.Errorf("services.WorkflowService.checkTimerCalendar: %w", err)
	}
	return nil
}

// checkDeadlineCalendars is checkTimerCalendar for set_deadline's pin (queue #79): every pinned
// calendarId must be a live calendar of THIS tenant (SET_DEADLINE_CALENDAR_NOT_FOUND, 422 — unknown,
// deleted and cross-tenant are one answer). The error points at the action row
// (details.field = "actions[i].calendarId"): a rule may carry several set_deadline actions, and only
// the pointer says which pin to fix. Same NOTE as the timer: a calendar deleted AFTER the save
// degrades to wall-clock at run time — with a run-history warning naming the pin.
func (s *WorkflowService) checkDeadlineCalendars(ctx context.Context, tenantID string, actions []WorkflowActionInput) error {
	if s.calendars == nil {
		return nil
	}
	for i, a := range actions {
		id := strings.TrimSpace(a.CalendarID)
		if id == "" || strings.TrimSpace(a.Type) != workflow.ActionSetDeadline {
			continue
		}
		if _, err := s.calendars.GetByID(ctx, tenantID, id); err != nil {
			if errors.Is(err, calmeta.ErrCalendarNotFound) {
				return invalidField("SET_DEADLINE_CALENDAR_NOT_FOUND", actionCalendarPointer(i),
					"lịch làm việc %q ghim cho action set_deadline không tồn tại trong tổ chức", id)
			}
			return fmt.Errorf("services.WorkflowService.checkDeadlineCalendars: %w", err)
		}
	}
	return nil
}

// actionCalendarPointer is the details.field of a set_deadline calendar refusal — the action's row,
// in the "actions[i].<key>" shape WEBHOOK_LEGACY_SECRET_PENDING already uses.
func actionCalendarPointer(i int) string {
	return fmt.Sprintf("actions[%d].calendarId", i)
}

// checkBusinessHoursReach refuses a business_hours offset the tenant's working calendar cannot
// reach (DAILY-QUEUE #80) — for BOTH consumers of the unit: the date_offset timer and every
// set_deadline action. workflow.ValidTimerAmount's ceiling (100000) is in WALL-CLOCK hours; a
// business hour is scarcer, and calendar.AddBusinessDuration gives up after ~10 years of calendar
// days, so on a Mon-Fri 08:00-17:00 week anything past ~23500 business hours saved cleanly and then
// FAILED on every firing. The verdict is a trial walk through the SAME AddBusinessDuration the run
// makes: holidays, DST and the calendar's real week all count, and there is no second formula to
// drift from it.
//
// ponytail: the trial starts now, while a timer's run-time anchor is a record date. The reachable
// total differs by at most the holidays between the two starts, which only matters within days of
// a ten-year ceiling — not worth a second, more conservative formula.
//
// Skipped when no resolver is wired (unit tests) and when the tenant has no calendar: that is the
// documented wall-clock fallback (specs/023 §1), which never walks a calendar and stays under the
// wall-clock ceiling. A store failure is an error, never a pass — and never a 422 either, because
// it is not the admin's mistake.
func (s *WorkflowService) checkBusinessHoursReach(ctx context.Context, tenantID string, timer *WorkflowTimerInput, actions []WorkflowActionInput) error {
	if s.sched == nil {
		return nil
	}
	if timer != nil && strings.TrimSpace(timer.Offset.Unit) == workflow.TimerUnitBusinessHours {
		if err := s.businessHoursReachable(ctx, tenantID, strings.TrimSpace(timer.CalendarID), timer.Offset.Amount,
			"WORKFLOW_TIMER_OFFSET_INVALID", "timer.offset.amount"); err != nil {
			return err
		}
	}
	for _, a := range actions {
		if strings.TrimSpace(a.Type) != workflow.ActionSetDeadline || a.Offset == nil ||
			strings.TrimSpace(a.Offset.Unit) != workflow.TimerUnitBusinessHours {
			continue
		}
		// The calendar the run will walk: the action's pin (queue #79), else the tenant default ("").
		if err := s.businessHoursReachable(ctx, tenantID, strings.TrimSpace(a.CalendarID), a.Offset.Amount,
			"SET_DEADLINE_OFFSET_INVALID", "offset.amount của set_deadline"); err != nil {
			return err
		}
	}
	return nil
}

// businessHoursReachable is one trial walk of checkBusinessHoursReach: resolve the calendar the run
// will use, then ask it for `amount` business hours from now.
func (s *WorkflowService) businessHoursReachable(ctx context.Context, tenantID, calendarID string, amount int, code, what string) error {
	sched, err := s.sched.ResolveSchedule(ctx, tenantID, calendarID)
	if err != nil {
		return fmt.Errorf("services.WorkflowService.checkBusinessHoursReach: %w", err)
	}
	if sched == nil {
		return nil // no calendar → wall-clock fallback, bounded by ValidTimerAmount alone
	}
	_, err = sched.AddBusinessDuration(time.Now(), time.Duration(amount)*time.Hour)
	if errors.Is(err, calendar.ErrNoWorkingTime) {
		return invalid(code,
			"%s = %d giờ làm việc vượt quá sức chứa của lịch làm việc: lịch này chỉ có %s giờ làm việc mỗi tuần, nên hạn rơi ra ngoài giới hạn 10 năm mà máy tính hạn đi tới — luật sẽ lỗi ở mọi lần chạy. Hãy giảm số giờ",
			what, amount, strconv.FormatFloat(sched.WeeklyBusinessDuration().Hours(), 'f', -1, 64))
	}
	if err != nil {
		return fmt.Errorf("services.WorkflowService.checkBusinessHoursReach: %w", err)
	}
	return nil
}

// WorkflowActionInput is one action as received from the API (camelCase mapped upstream).
type WorkflowActionInput struct {
	Type  string
	Field string
	Value any
	// Via (field_update, specs/058): a to-one relation field of the rule's entity — the write lands
	// on the record it points at, and Field names a field of THAT entity. "" = the triggering record.
	Via string
	// ChildEntity + ChildVia (field_update, specs/084): write EVERY record of ChildEntity whose
	// relation field ChildVia points at the triggering record; Field names a field of ChildEntity.
	ChildEntity string
	ChildVia    string
	UserID      string
	Title       string
	Message     string
	// email_alert
	To      string
	Subject string
	Body    string
	// outbound_webhook
	URL     string
	Secret  string
	Headers map[string]string
	// outbound_webhook through the named-credential vault (specs/050 S2b): the key's id + the path
	// appended to its base URL. Mutually exclusive with URL/Secret/Headers.
	CredentialID string
	Path         string
	// assign (ASSIGN-1): Strategy (v1: "round_robin") + Team (id whose members are rotated).
	// Field (above) is the text field the picked userId is written into.
	Strategy string
	Team     string
	// create_record (CREATE-RECORD): CreateEntity is the target entity to insert into; Values is the
	// field→value map for the new record (literals + the "$owner" sentinel). Target-entity existence
	// and Values-key validity are checked on the apply path (checkCreateRecordTargets) because they
	// need the TARGET entity's fields, not the trigger entity's (which is all buildActionsJSON has).
	CreateEntity string
	Values       map[string]any
	// set_deadline: Offset is how far AFTER the fire instant the computed deadline lands, in the
	// SAME {amount, unit} vocabulary a date_offset timer carries (WorkflowTimerOffsetInput is
	// reused deliberately — one meaning of "+4 hours" across the engine). Field (above) is the
	// datetime field it is written into.
	Offset *WorkflowTimerOffsetInput
	// set_deadline policy (queue #81, specs/043 S14a) — see workflow.Action.Mode/StampField/Anchor.
	Mode       string
	StampField string
	Anchor     string
	// CalendarID pins set_deadline's business_hours calendar (queue #79); "" = tenant default.
	CalendarID string
	// field_update on a before_save rule (specs/048 S4) — see workflow.Action.OnlyIfEmpty/ValueSource.
	OnlyIfEmpty bool
	ValueSource string
	// Ops of a normalize action (specs/048 S5) — see workflow.Action.Ops.
	Ops []string
	// ProcessID of a start_approval action (specs/043 S9b) — see workflow.Action.ProcessID.
	ProcessID string
}

// WorkflowRuleInput is one condition predicate as received from the API.
type WorkflowRuleInput struct {
	Field string
	Op    string
	Value any
}

// WorkflowConditionInput is the condition block of a workflow rule as one value (M7). It exists so
// ValidateWorkflowConfig can take the condition the way POST /admin/workflows carries it
// (`condition:{match?,rules[],timer?}` — nil means match-all), while CreateWorkflowInput keeps its
// flat Match+Rules fields for handler compatibility. Timer is the TIMER-1 date_offset config,
// carried on the wire exactly where it is stored (inside the condition JSON).
type WorkflowConditionInput struct {
	Match string
	Rules []WorkflowRuleInput
	Timer *WorkflowTimerInput
	// SaveOn narrows a before_save rule to "create" or "update" (specs/048); "" = both. Rejected on
	// every other trigger.
	SaveOn string
}

// WorkflowTimerInput is the date_offset trigger config as received from the API (TIMER-1):
// `condition.timer = {dateField, offset:{amount,unit}, direction, calendarId?}`.
type WorkflowTimerInput struct {
	DateField  string
	Offset     WorkflowTimerOffsetInput
	Direction  string
	CalendarID string
	// Repeat (optional) makes the reminder recurring. nil = fire once — every rule written before
	// recurrence existed decodes to exactly that.
	Repeat *WorkflowTimerRepeatInput
}

// WorkflowTimerRepeatInput is the bounded recurrence: every N days/weeks, Count times in total.
type WorkflowTimerRepeatInput struct {
	Every int
	Unit  string
	Count int
}

// WorkflowTimerOffsetInput is the {amount, unit} pair of a timer config.
type WorkflowTimerOffsetInput struct {
	Amount int
	Unit   string
}

// CreateWorkflowInput is the payload to create a workflow rule.
type CreateWorkflowInput struct {
	Name       string
	EntityName string
	Trigger    string
	Match      string // condition join mode ("and"/""); reserved for "or"
	Rules      []WorkflowRuleInput
	// Timer is the TIMER-1 date_offset config (required for that trigger, rejected elsewhere).
	Timer *WorkflowTimerInput
	// SaveOn is condition.saveOn of a before_save rule (specs/048) — see WorkflowConditionInput.
	SaveOn   string
	Actions  []WorkflowActionInput
	IsActive *bool // defaults to true when nil
	// IntervalSeconds is the cadence (seconds) for a scheduled rule — fire at most once per
	// interval. Ignored (forced to 0) for the event triggers. 0 = fire on every scheduler tick.
	IntervalSeconds int
	ActorID         string
}

// UpdateWorkflowInput is the full-replace payload to update a workflow rule. Every field is
// supplied; the service re-validates and overwrites the row.
type UpdateWorkflowInput struct {
	Name            string
	EntityName      string
	Trigger         string
	Match           string
	Rules           []WorkflowRuleInput
	Timer           *WorkflowTimerInput
	SaveOn          string
	Actions         []WorkflowActionInput
	IsActive        *bool
	IntervalSeconds int
	ActorID         string
}

// ListWorkflows returns every workflow in the tenant (admin list page).
func (s *WorkflowService) ListWorkflows(ctx context.Context, tenantID string) ([]workflow.WorkflowRecord, error) {
	rows, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.WorkflowService.ListWorkflows: %w", err)
	}
	return rows, nil
}

// GetWorkflow returns one tenant-scoped workflow, or WORKFLOW_NOT_FOUND for an unknown id.
func (s *WorkflowService) GetWorkflow(ctx context.Context, tenantID, id string) (*workflow.WorkflowRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("WORKFLOW_ID_REQUIRED", "id workflow là bắt buộc")
	}
	rec, err := s.repo.Get(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, workflow.ErrWorkflowNotFound) {
			return nil, invalid("WORKFLOW_NOT_FOUND", "không tìm thấy workflow")
		}
		return nil, fmt.Errorf("services.WorkflowService.GetWorkflow: %w", err)
	}
	return rec, nil
}

// CreateWorkflow validates and inserts a workflow rule. name/entityName/trigger are required;
// the trigger must be one of the three after-commit events; the entity must exist; every
// condition rule's operator and every action's shape is validated. Returns the created row.
func (s *WorkflowService) CreateWorkflow(ctx context.Context, tenantID string, in CreateWorkflowInput) (*workflow.WorkflowRecord, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, invalid("WORKFLOW_NAME_REQUIRED", "name workflow là bắt buộc")
	}
	// AI-APPLY-IDEMPOTENT (3.1): sys_workflows has no UNIQUE(tenant,name), so a repeated
	// Apply/POST silently minted a SECOND identical rule that double-fires. App-level
	// list-then-check (the pack installer's own idempotency shape) → 409, parity with the
	// other draft kinds. "Same name = update" callers use PUT on the existing id instead.
	existing, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.WorkflowService.CreateWorkflow: dup check: %w", err)
	}
	for i := range existing {
		if strings.TrimSpace(existing[i].Name) == name {
			return nil, invalid("WORKFLOW_ALREADY_EXISTS", "workflow %q đã tồn tại — sửa rule cũ thay vì tạo trùng", name)
		}
	}
	entity, entityDef, err := s.validateEntity(ctx, tenantID, in.EntityName)
	if err != nil {
		return nil, err
	}
	fieldTypes, err := s.fieldTypesFor(ctx, tenantID, entityDef)
	if err != nil {
		return nil, err
	}
	// ONE validation body (buildWorkflowRule) shared with UpdateWorkflow AND the AI Builder's
	// ValidateWorkflowConfig (M7) — trigger/condition/actions rules can never drift between the
	// apply path and the config-time propose check.
	trigger, condJSON, actionsJSON, err := buildWorkflowRule(entity, in.Trigger,
		&WorkflowConditionInput{Match: in.Match, Rules: in.Rules, Timer: in.Timer, SaveOn: in.SaveOn}, in.Actions, fieldTypes)
	if err != nil {
		return nil, err
	}
	// TIMER-1: the calendarId existence check is DB-dependent (like the entity check), so it
	// lives here on the apply path, not in the pure buildWorkflowRule body.
	if err := s.checkTimerCalendar(ctx, tenantID, in.Timer); err != nil {
		return nil, err
	}
	// Queue #79: a set_deadline's pinned calendar must exist too — BEFORE the reach check walks it.
	if err := s.checkDeadlineCalendars(ctx, tenantID, in.Actions); err != nil {
		return nil, err
	}
	// DAILY-QUEUE #80: a business_hours amount must fit the tenant's calendar — DB-dependent too.
	if err := s.checkBusinessHoursReach(ctx, tenantID, in.Timer, in.Actions); err != nil {
		return nil, err
	}
	// ASSIGN-1: the team existence check is likewise DB-dependent (apply path only).
	if err := s.checkAssignTeams(ctx, tenantID, in.Actions); err != nil {
		return nil, err
	}
	// CREATE-RECORD: the target-entity + per-field check is DB-dependent (it reads the TARGET
	// entity's fields, not the trigger's) — apply path only, like checkAssignTeams.
	if err := s.checkCreateRecordTargets(ctx, tenantID, in.Actions); err != nil {
		return nil, err
	}
	// specs/043 S9b: a start_approval's process must be a live manual process of this entity.
	if err := s.checkStartApprovalTargets(ctx, tenantID, entity, trigger, in.Actions); err != nil {
		return nil, err
	}
	// specs/058: a field_update through a relation checks field + value against the RELATED entity.
	if err := s.checkRelatedFieldTargets(ctx, tenantID, entityDef, in.Actions); err != nil {
		return nil, err
	}
	// specs/059: an email's {{relation.field}} must name a real relation + a real field of its target.
	if err := s.checkTemplateRefs(ctx, tenantID, entityDef, in.Actions); err != nil {
		return nil, err
	}
	// specs/050 S2b: a vault webhook's key must be a live key of this tenant (apply path only).
	if err := s.checkWebhookCredentials(ctx, tenantID, in.Actions); err != nil {
		return nil, err
	}
	// specs/048 S4 + queue #282: a field_update constant must fit its field (needs the full definitions).
	if err := s.checkFieldUpdateValues(ctx, tenantID, entityDef, trigger, in.Actions); err != nil {
		return nil, err
	}
	isActive := true
	if in.IsActive != nil {
		isActive = *in.IsActive
	}
	// specs/048: an ACTIVE before rule — per-(entity, trigger) cap (DB-dependent).
	if err := s.checkBeforeActivation(ctx, tenantID, entity, trigger, isActive, ""); err != nil {
		return nil, err
	}

	id, err := s.repo.Create(ctx, workflow.CreateInput{
		TenantID:        tenantID,
		Name:            name,
		EntityName:      entity,
		Trigger:         trigger,
		Condition:       condJSON,
		Actions:         actionsJSON,
		IsActive:        isActive,
		IntervalSeconds: intervalFor(trigger, in.IntervalSeconds),
		CreatedBy:       strings.TrimSpace(in.ActorID),
	})
	if err != nil {
		return nil, fmt.Errorf("services.WorkflowService.CreateWorkflow: %w", err)
	}
	// Invalidate the engine's cached rules for this entity AFTER commit so the new rule fires.
	s.cache.Invalidate(tenantID, entity)
	rec, err := s.repo.Get(ctx, tenantID, id)
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.WorkflowRuleMeta, id, audit.ActionCreate,
		nil, workflowAuditSnapshot(rec))
	return rec, err
}

// UpdateWorkflow re-validates and replaces a tenant-scoped workflow. Unknown id → WORKFLOW_NOT_FOUND.
func (s *WorkflowService) UpdateWorkflow(ctx context.Context, tenantID, id string, in UpdateWorkflowInput) (*workflow.WorkflowRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("WORKFLOW_ID_REQUIRED", "id workflow là bắt buộc")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, invalid("WORKFLOW_NAME_REQUIRED", "name workflow là bắt buộc")
	}
	entity, entityDef, err := s.validateEntity(ctx, tenantID, in.EntityName)
	if err != nil {
		return nil, err
	}
	fieldTypes, err := s.fieldTypesFor(ctx, tenantID, entityDef)
	if err != nil {
		return nil, err
	}
	// Read the prior row: its (possibly different) entity bucket is invalidated too, its vault paths
	// are what a masked read-back is matched against, and its actions are what the silent-key-drop
	// guard compares against. A read error is reported below, after validation, as before.
	prior, gerr := s.repo.Get(ctx, tenantID, id)
	if gerr == nil && prior != nil {
		in.Actions = keepStoredVaultPaths(prior.Actions, in.Actions)
	}
	// Same ONE validation body as CreateWorkflow / ValidateWorkflowConfig (M7).
	trigger, condJSON, actionsJSON, err := buildWorkflowRule(entity, in.Trigger,
		&WorkflowConditionInput{Match: in.Match, Rules: in.Rules, Timer: in.Timer, SaveOn: in.SaveOn}, in.Actions, fieldTypes)
	if err != nil {
		return nil, err
	}
	// TIMER-1: DB-dependent calendarId existence check (apply path only — see CreateWorkflow).
	if err := s.checkTimerCalendar(ctx, tenantID, in.Timer); err != nil {
		return nil, err
	}
	// Queue #79: set_deadline's pinned calendar (apply path only — see CreateWorkflow).
	if err := s.checkDeadlineCalendars(ctx, tenantID, in.Actions); err != nil {
		return nil, err
	}
	// DAILY-QUEUE #80: calendar-aware business_hours ceiling (apply path only — see CreateWorkflow).
	if err := s.checkBusinessHoursReach(ctx, tenantID, in.Timer, in.Actions); err != nil {
		return nil, err
	}
	// ASSIGN-1: DB-dependent team existence check (apply path only — see CreateWorkflow).
	if err := s.checkAssignTeams(ctx, tenantID, in.Actions); err != nil {
		return nil, err
	}
	// CREATE-RECORD: DB-dependent target-entity + per-field check (apply path only — see CreateWorkflow).
	if err := s.checkCreateRecordTargets(ctx, tenantID, in.Actions); err != nil {
		return nil, err
	}
	// specs/043 S9b: start_approval's process (apply path only — see CreateWorkflow).
	if err := s.checkStartApprovalTargets(ctx, tenantID, entity, trigger, in.Actions); err != nil {
		return nil, err
	}
	// specs/058: related field_update target check (apply path only — see CreateWorkflow).
	if err := s.checkRelatedFieldTargets(ctx, tenantID, entityDef, in.Actions); err != nil {
		return nil, err
	}
	// specs/059: linked-record email placeholders (apply path only — see CreateWorkflow).
	if err := s.checkTemplateRefs(ctx, tenantID, entityDef, in.Actions); err != nil {
		return nil, err
	}
	// specs/050 S2b: vault key existence (apply path only — see CreateWorkflow).
	if err := s.checkWebhookCredentials(ctx, tenantID, in.Actions); err != nil {
		return nil, err
	}
	// specs/048 S4 + queue #282: field_update constants against the full field definitions (see CreateWorkflow).
	if err := s.checkFieldUpdateValues(ctx, tenantID, entityDef, trigger, in.Actions); err != nil {
		return nil, err
	}
	isActive := true
	if in.IsActive != nil {
		isActive = *in.IsActive
	}
	// specs/048: same cap as CreateWorkflow; the rule being edited never counts against itself.
	if err := s.checkBeforeActivation(ctx, tenantID, entity, trigger, isActive, id); err != nil {
		return nil, err
	}

	// An unreadable prior row fails the save instead of skipping the guard.
	if gerr != nil || prior == nil {
		if gerr == nil || errors.Is(gerr, workflow.ErrWorkflowNotFound) {
			return nil, invalid("WORKFLOW_NOT_FOUND", "không tìm thấy workflow")
		}
		return nil, fmt.Errorf("services.WorkflowService.UpdateWorkflow: load rule: %w", gerr)
	}
	if err := refuseSilentWebhookKeyDrop(prior.Actions, in.Actions); err != nil {
		return nil, err
	}
	priorEntity := prior.EntityName
	// Snapshot before the write: a repository may hand back the very row it then mutates.
	beforeSnap := workflowAuditSnapshot(prior)

	if err := s.repo.Update(ctx, tenantID, id, workflow.UpdateInput{
		Name:            name,
		EntityName:      entity,
		Trigger:         trigger,
		Condition:       condJSON,
		Actions:         actionsJSON,
		IsActive:        isActive,
		IntervalSeconds: intervalFor(trigger, in.IntervalSeconds),
		UpdatedBy:       strings.TrimSpace(in.ActorID),
	}); err != nil {
		if errors.Is(err, workflow.ErrWorkflowNotFound) {
			return nil, invalid("WORKFLOW_NOT_FOUND", "không tìm thấy workflow")
		}
		return nil, fmt.Errorf("services.WorkflowService.UpdateWorkflow: %w", err)
	}
	// Invalidate both the new and the (possibly different) prior entity bucket AFTER commit.
	s.cache.Invalidate(tenantID, entity)
	if priorEntity != "" && priorEntity != entity {
		s.cache.Invalidate(tenantID, priorEntity)
	}
	rec, err := s.repo.Get(ctx, tenantID, id)
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.WorkflowRuleMeta, id, audit.ActionUpdate,
		beforeSnap, workflowAuditSnapshot(rec))
	return rec, err
}

// SetWorkflowActive pauses or resumes a rule without rewriting it. It exists because PUT cannot do
// it for a rule that still holds a pre-vault key: the builder resends {type, url}, refuseSilentWebhookKeyDrop
// answers 409, and the Doctor C10 refusal ("pause the rule, wait for the queue") became a loop.
// Resuming goes through the same active-before-rule cap as a save. Unknown / cross-tenant id →
// WORKFLOW_NOT_FOUND.
func (s *WorkflowService) SetWorkflowActive(ctx context.Context, tenantID, id string, active bool, actorID string) (*workflow.WorkflowRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("WORKFLOW_ID_REQUIRED", "id workflow là bắt buộc")
	}
	prior, err := s.repo.Get(ctx, tenantID, id)
	if err != nil || prior == nil {
		if err == nil || errors.Is(err, workflow.ErrWorkflowNotFound) {
			return nil, invalid("WORKFLOW_NOT_FOUND", "không tìm thấy workflow")
		}
		return nil, fmt.Errorf("services.WorkflowService.SetWorkflowActive: load rule: %w", err)
	}
	if err := s.checkBeforeActivation(ctx, tenantID, prior.EntityName, workflow.Trigger(prior.Trigger), active, id); err != nil {
		return nil, err
	}
	beforeSnap := workflowAuditSnapshot(prior) // before the write: see UpdateWorkflow
	if err := s.repo.SetActive(ctx, tenantID, id, active, strings.TrimSpace(actorID)); err != nil {
		if errors.Is(err, workflow.ErrWorkflowNotFound) {
			return nil, invalid("WORKFLOW_NOT_FOUND", "không tìm thấy workflow")
		}
		return nil, fmt.Errorf("services.WorkflowService.SetWorkflowActive: %w", err)
	}
	// AFTER commit, so the engine stops (or starts) firing the rule at once.
	s.cache.Invalidate(tenantID, prior.EntityName)
	rec, err := s.repo.Get(ctx, tenantID, id)
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.WorkflowRuleMeta, id, audit.ActionUpdate,
		beforeSnap, workflowAuditSnapshot(rec))
	return rec, err
}

// refuseSilentWebhookKeyDrop is the specs/050 S2a guard (D-11, FR-023). An outbound_webhook
// action saved before S3a can carry a signing key / auth headers of its own, but the old Studio
// builder only knows {type, url} and an update replaces the actions wholesale — one "Lưu" there used
// to strip the key and the partner started receiving unauthenticated calls. The save is refused
// with 409 when a bare action still points at a URL that had a keyed webhook action (exactly the
// builder's shape). Changing the URL, removing the action or moving it onto a vault key all save.
//
// Since S3a no write path accepts an inline key (buildActionsJSON → WEBHOOK_INLINE_SECRET_REMOVED,
// which runs before this), so the key can no longer be "sent again": the way out is the vault, or
// the Doctor step that moves the stored key there (S3b). Pausing the rule is not a save: it goes
// through SetWorkflowActive, so a rule C10 refused because its queue never drains can be switched off.
func refuseSilentWebhookKeyDrop(stored json.RawMessage, next []WorkflowActionInput) error {
	var prev []workflow.Action
	// ponytail: a stored row the engine cannot decode sends nothing, so it has no key to lose —
	// let the PUT repair it rather than lock the rule.
	if len(stored) == 0 || json.Unmarshal(stored, &prev) != nil {
		return nil
	}
	keyed := map[string]bool{} // URLs a keyed webhook action of the stored rule posts to
	for _, a := range prev {
		if a.Type == workflow.ActionWebhook && (a.Secret != "" || len(a.Headers) > 0) {
			keyed[strings.TrimSpace(a.URL)] = true
		}
	}
	for i, a := range next {
		if strings.TrimSpace(a.Type) == workflow.ActionWebhook && keyed[strings.TrimSpace(a.URL)] {
			return &ValidationError{
				Code: "WEBHOOK_LEGACY_SECRET_PENDING",
				Msg: "rule này còn khoá ký/header trần cho URL này — lưu kiểu này sẽ xoá khoá. " +
					"Chọn một khoá trong Kho khoá kết nối (Quản trị › Kho khoá kết nối) cho action này, " +
					"hoặc chạy Bác sĩ hệ thống để dời khoá trần vào kho; đổi URL hay xoá action cũng được",
				Field:   fmt.Sprintf("actions[%d].url", i),
				Details: map[string]any{"field": fmt.Sprintf("actions[%d].url", i)},
			}
		}
	}
	return nil
}

// keepStoredVaultPaths is the save half of the read-side mask for vault actions (specs/050 S3b
// review). GET masks the query values of a vault action's path (workflow.RedactActionsJSON) and the
// builder shows that path read-only and sends it back as it read it — the shape Doctor C10 gives a
// legacy URL with a query. An incoming vault action whose path is exactly the mask of a stored path
// under the SAME key gets the stored path back: the key fixes the host, so nothing reaches a new
// target. The stored action at the same position wins; elsewhere the match must be unique (two
// different stored paths masking alike are ambiguous). Anything else keeps the mask and meets the
// WEBHOOK_REDACTED_VALUE refusal in buildActionsJSON. next is copied, never changed in place.
func keepStoredVaultPaths(stored json.RawMessage, next []WorkflowActionInput) []WorkflowActionInput {
	var prev []workflow.Action
	if len(stored) == 0 || json.Unmarshal(stored, &prev) != nil {
		return next
	}
	masks := func(p workflow.Action, cid, path string) bool {
		return p.Type == workflow.ActionWebhook && strings.TrimSpace(p.CredentialID) == cid &&
			p.Path != path && workflow.RedactTarget(p.Path) == path
	}
	var out []WorkflowActionInput
	for i, a := range next {
		cid, path := strings.TrimSpace(a.CredentialID), strings.TrimSpace(a.Path)
		if cid == "" || !workflow.HasRedactedMarker(path) {
			continue
		}
		path = workflow.NormalizeRedactedMarker(path) // a client may have percent-encoded the mask
		keep := ""
		if i < len(prev) && masks(prev[i], cid, path) {
			keep = prev[i].Path
		} else {
			for _, p := range prev {
				if !masks(p, cid, path) {
					continue
				}
				if keep != "" && keep != p.Path {
					keep = "" // ambiguous — left to the refusal
					break
				}
				keep = p.Path
			}
		}
		if keep == "" {
			continue
		}
		if out == nil {
			out = append([]WorkflowActionInput(nil), next...)
		}
		out[i].Path = keep
	}
	if out == nil {
		return next
	}
	return out
}

// DeleteWorkflow soft-deletes a tenant-scoped workflow. Unknown id → WORKFLOW_NOT_FOUND.
func (s *WorkflowService) DeleteWorkflow(ctx context.Context, tenantID, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return invalid("WORKFLOW_ID_REQUIRED", "id workflow là bắt buộc")
	}
	// Read the row first so we can invalidate its entity bucket after delete (and say what was deleted).
	prior := priorDoc(func() (*workflow.WorkflowRecord, error) { return s.repo.Get(ctx, tenantID, id) })
	var entity string
	if prior != nil {
		entity = prior.EntityName
	}
	if err := s.repo.Delete(ctx, tenantID, id); err != nil {
		if errors.Is(err, workflow.ErrWorkflowNotFound) {
			return invalid("WORKFLOW_NOT_FOUND", "không tìm thấy workflow")
		}
		return fmt.Errorf("services.WorkflowService.DeleteWorkflow: %w", err)
	}
	s.cache.Invalidate(tenantID, entity)
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.WorkflowRuleMeta, id, audit.ActionDelete,
		workflowAuditSnapshot(prior), nil)
	return nil
}

// redactedValue is the S3a refusal of a url/path that still carries the read-side mask
// (workflow.RedactedMarker): the rule is left as it was.
func redactedValue(i int, field string) error {
	return invalidField("WEBHOOK_REDACTED_VALUE", fmt.Sprintf("actions[%d].%s", i, field),
		"%s còn phần đã che (%s) của lần đọc trước — nhập lại giá trị thật, hoặc chuyển sang khoá trong Kho khoá kết nối; quy trình chưa bị đổi",
		field, workflow.RedactedMarker)
}

// validateEntity trims + checks the entity name is non-empty and (when a checker is wired) refers
// to a real entity in the tenant. Returns the trimmed entity name plus the resolved definition
// (nil when no checker is wired) so callers can list the entity's fields without a second lookup.
func (s *WorkflowService) validateEntity(ctx context.Context, tenantID, raw string) (string, *metadata.EntityDefinition, error) {
	entity := strings.TrimSpace(raw)
	if entity == "" {
		return "", nil, invalid("WORKFLOW_ENTITY_REQUIRED", "entityName của workflow là bắt buộc")
	}
	var def *metadata.EntityDefinition
	if s.entites != nil {
		d, err := s.entites.GetEntityByName(ctx, tenantID, entity)
		if err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				return "", nil, invalid("WORKFLOW_ENTITY_NOT_FOUND", "entity %q không tồn tại", entity)
			}
			return "", nil, fmt.Errorf("services.WorkflowService.validateEntity: %w", err)
		}
		def = d
	}
	return entity, def, nil
}

// fieldTypesFor resolves the entity's field name → fieldType map for the WF-01 field-target
// guard. It returns nil (guard skipped) when the entity definition is unknown (no checker wired)
// or the checker cannot list fields — production wires *MetaCache which can, so the guard is
// always armed there. The map is small (one entity's fields) and read through the TTL'd metadata
// cache, so config-time validation adds no hot-path cost.
func (s *WorkflowService) fieldTypesFor(ctx context.Context, tenantID string, def *metadata.EntityDefinition) (map[string]string, error) {
	if def == nil {
		return nil, nil
	}
	lister, ok := s.entites.(workflowFieldLister)
	if !ok {
		return nil, nil
	}
	fields, err := lister.ListFields(ctx, tenantID, def.ID)
	if err != nil {
		return nil, fmt.Errorf("services.WorkflowService.fieldTypesFor: %w", err)
	}
	types := make(map[string]string, len(fields))
	for _, f := range fields {
		types[f.Name] = f.FieldType
	}
	return types, nil
}

// ValidateWorkflowConfig is the EXPORTED config-time workflow validator — a thin wrapper around
// buildWorkflowRule, the SAME single body CreateWorkflow and UpdateWorkflow run (M7, spec
// ../specs/020-e9-ai-builder/spec.md §14). Callers outside this package (the AI Builder's
// propose_workflow tool, internal/ai) therefore validate with EXACT parity to the apply path:
// one function body, no hand-mirrored rule logic that could drift (the M6 ValidateLayoutConfig
// pattern).
//
// Split of responsibilities (mirrors how M6 handled liveFields):
//   - entityName here is checked for NON-EMPTINESS only (WORKFLOW_ENTITY_REQUIRED). The
//     EXISTENCE check is DB-dependent and stays with the caller: the service resolves it via
//     validateEntity (→ WORKFLOW_ENTITY_NOT_FOUND), the AI tool via its tenant-scoped reader
//     (→ ENTITY_NOT_FOUND) BEFORE calling this.
//   - fieldTypes is the target entity's field name → fieldType map, ALREADY resolved by the
//     caller (service: fieldTypesFor through MetaCache; tool: reader.ListFields). Non-nil arms
//     the WF-01 field_update target guard; nil skips it (checkers that cannot list fields).
//
// cond is the condition block as the POST /admin/workflows body carries it; nil → match-all.
// Returns a *ValidationError (WORKFLOW_TRIGGER_INVALID / WORKFLOW_CONDITION_INVALID /
// WORKFLOW_TIMER_* (TIMER-1 — all but the DB-dependent calendarId existence check, which stays
// with the service apply path) / WORKFLOW_ACTIONS_REQUIRED / WORKFLOW_ACTION_INVALID /
// RECORD_UNKNOWN_FIELD / RECORD_*_READONLY) on any violation; nil when the rule is valid.
func ValidateWorkflowConfig(entityName, trigger string, cond *WorkflowConditionInput, actions []WorkflowActionInput, fieldTypes map[string]string) error {
	_, _, _, err := buildWorkflowRule(entityName, trigger, cond, actions, fieldTypes)
	return err
}

// buildWorkflowRule is the ONE validation + marshal body for a workflow rule's config surface:
// entityName non-empty → trigger → timer (TIMER-1) → condition → actions (incl. the WF-01
// field_update target guard). CreateWorkflow, UpdateWorkflow AND ValidateWorkflowConfig all call
// it, so the apply path and the AI Builder's config-time check can never drift. Returns the
// normalized trigger plus the persisted-form JSON blobs (condition nil → NULL column, match-all;
// a date_offset rule always persists a condition JSON — it carries the timer config).
func buildWorkflowRule(entityName, trigger string, cond *WorkflowConditionInput, actions []WorkflowActionInput, fieldTypes map[string]string) (workflow.Trigger, []byte, []byte, error) {
	if strings.TrimSpace(entityName) == "" {
		return "", nil, nil, invalid("WORKFLOW_ENTITY_REQUIRED", "entityName của workflow là bắt buộc")
	}
	t, err := validateTrigger(trigger)
	if err != nil {
		return "", nil, nil, err
	}
	var match, saveOn string
	var rules []WorkflowRuleInput
	var timerIn *WorkflowTimerInput
	if cond != nil {
		match, rules, timerIn, saveOn = cond.Match, cond.Rules, cond.Timer, strings.TrimSpace(cond.SaveOn)
	}
	// specs/058 first: a save-time rule writing a RELATED record is refused for that reason, not
	// for "field_update is not supported here yet".
	if err := checkRelatedTrigger(t, actions); err != nil {
		return "", nil, nil, err
	}
	// specs/048: before_* shape (saveOn, allowed actions, never-true edges, caps) — and the refusal
	// of the before-only keys on every other trigger. Runs before the timer check so a timer on a
	// before rule reports WORKFLOW_BEFORE_TIMER_INVALID, not the generic date_offset message.
	if err := checkBeforeRule(t, saveOn, rules, timerIn, actions, fieldTypes); err != nil {
		return "", nil, nil, err
	}
	timer, err := buildTimerConfig(t, timerIn, fieldTypes)
	if err != nil {
		return "", nil, nil, err
	}
	condJSON, err := buildConditionJSON(match, rules, timer, saveOn)
	if err != nil {
		return "", nil, nil, err
	}
	actionsJSON, err := buildActionsJSON(actions, fieldTypes)
	if err != nil {
		return "", nil, nil, err
	}
	return t, condJSON, actionsJSON, nil
}

// validateTrigger normalizes + validates the trigger against the supported events.
func validateTrigger(raw string) (workflow.Trigger, error) {
	t := workflow.Trigger(strings.TrimSpace(raw))
	if !workflow.ValidTrigger(t) {
		return "", invalid("WORKFLOW_TRIGGER_INVALID",
			"trigger không hợp lệ: %q (chỉ after_create/after_update/after_delete/scheduled/date_offset/before_save/before_delete)", raw)
	}
	return t, nil
}

// buildTimerConfig validates the TIMER-1 date_offset config and returns the persisted-form
// TimerConfig (nil for non-timer triggers). It is part of the ONE buildWorkflowRule body, so
// validate == apply by construction. Rules (codes are the WORKFLOW_TIMER_* family, all 422):
//
//   - a date_offset rule REQUIRES a timer; any other trigger must NOT carry one (a timer on an
//     after_create rule is dead config that would silently never fire time-based — reject loudly,
//     the B6 validation-parity discipline).
//   - dateField must be a date/datetime field of the entity (fieldTypes — the same resolved map
//     the WF-01 action guard uses) or the system column "created_at" (immutable creation stamp —
//     the SLA anchor) or "updated_at" (#287 inactivity: the producer reads the stamp back from the
//     store, so each edit parks a replacement and the stale wake-up skips itself). A nil fieldTypes map (checker that cannot list fields) skips the type check,
//     mirroring the WF-01 guard.
//   - offset.amount positive and bounded; offset.unit in the closed set; direction before/after.
//   - business_hours supports direction=after only (CALENDAR-1 has no backward walk — v1).
//   - calendarId is only meaningful with unit=business_hours; existence in the tenant is
//     DB-dependent and checked by the service apply path (checkTimerCalendar), not here.
func buildTimerConfig(trigger workflow.Trigger, in *WorkflowTimerInput, fieldTypes map[string]string) (*workflow.TimerConfig, error) {
	if trigger != workflow.TriggerDateOffset {
		if in != nil {
			return nil, invalid("WORKFLOW_TIMER_INVALID",
				"condition.timer chỉ dùng cho trigger date_offset (trigger hiện tại: %q)", trigger)
		}
		return nil, nil
	}
	if in == nil {
		return nil, invalid("WORKFLOW_TIMER_REQUIRED",
			"trigger date_offset cần condition.timer ({dateField, offset:{amount,unit}, direction})")
	}
	dateField := strings.TrimSpace(in.DateField)
	if dateField == "" {
		return nil, invalid("WORKFLOW_TIMER_FIELD_INVALID", "timer.dateField là bắt buộc")
	}
	if fieldTypes != nil && !workflow.IsSystemTimerAnchor(dateField) {
		ft, known := fieldTypes[dateField]
		if !known {
			return nil, invalidField("WORKFLOW_TIMER_FIELD_INVALID", dateField,
				"timer.dateField %q không tồn tại trong metadata (dùng field date/datetime, created_at hoặc updated_at)", dateField)
		}
		if ft != "date" && ft != "datetime" {
			return nil, invalidField("WORKFLOW_TIMER_FIELD_INVALID", dateField,
				"timer.dateField %q phải là field date/datetime (đang là %s)", dateField, ft)
		}
	}
	if !workflow.ValidTimerAmount(in.Offset.Amount) {
		return nil, invalid("WORKFLOW_TIMER_OFFSET_INVALID",
			"timer.offset.amount phải là số nguyên dương (nhận %d)", in.Offset.Amount)
	}
	unit := strings.TrimSpace(in.Offset.Unit)
	if !workflow.ValidTimerUnit(unit) {
		return nil, invalid("WORKFLOW_TIMER_UNIT_INVALID",
			"timer.offset.unit không hợp lệ: %q (minutes/hours/days/business_hours)", in.Offset.Unit)
	}
	direction := strings.TrimSpace(in.Direction)
	if !workflow.ValidTimerDirection(direction) {
		return nil, invalid("WORKFLOW_TIMER_DIRECTION_INVALID",
			"timer.direction không hợp lệ: %q (before/after)", in.Direction)
	}
	// updated_at moves on every save: "before" would land in the past and fire on every edit — an
	// inactivity reminder only makes sense as "N after the last edit".
	if dateField == workflow.TimerAnchorUpdatedAt && direction == workflow.TimerDirectionBefore {
		return nil, invalid("WORKFLOW_TIMER_DIRECTION_INVALID",
			"mốc updated_at (lần sửa gần nhất) chỉ hỗ trợ direction=after")
	}
	if unit == workflow.TimerUnitBusinessHours && direction == workflow.TimerDirectionBefore {
		return nil, invalid("WORKFLOW_TIMER_DIRECTION_INVALID",
			"business_hours chỉ hỗ trợ direction=after (v1 — lịch làm việc không đi lùi)")
	}
	calendarID := strings.TrimSpace(in.CalendarID)
	if calendarID != "" && unit != workflow.TimerUnitBusinessHours {
		return nil, invalid("WORKFLOW_TIMER_CALENDAR_INVALID",
			"timer.calendarId chỉ dùng với unit business_hours")
	}
	// Recurrence is validated at CONFIG time, through the same body the engine reads — a repeat
	// that cannot run must be refused when it is written, not discovered when the reminder does not
	// arrive. Bounded on purpose (no "forever", no `until`): see workflow.TimerRepeat.
	var repeat *workflow.TimerRepeat
	if in.Repeat != nil {
		repeat = &workflow.TimerRepeat{
			Every: in.Repeat.Every,
			Unit:  strings.TrimSpace(in.Repeat.Unit),
			Count: in.Repeat.Count,
		}
		if err := workflow.ValidTimerRepeat(repeat); err != nil {
			return nil, invalid("WORKFLOW_TIMER_REPEAT_INVALID", "timer.repeat không hợp lệ: %v", err)
		}
	}
	return &workflow.TimerConfig{
		DateField:  dateField,
		Offset:     workflow.TimerOffset{Amount: in.Offset.Amount, Unit: unit},
		Direction:  direction,
		CalendarID: calendarID,
		Repeat:     repeat,
	}, nil
}

// intervalFor resolves the persisted interval_seconds. It is meaningful ONLY for the scheduled
// trigger; for the event triggers it is forced to 0 so a stray interval on an after_* rule never
// confuses the scheduler scan. A negative input is clamped to 0 (fire-every-tick).
func intervalFor(trigger workflow.Trigger, interval int) int {
	if trigger != workflow.TriggerScheduled {
		return 0
	}
	if interval < 0 {
		return 0
	}
	return interval
}

// buildConditionJSON validates the condition rules and marshals the condition to JSON. An empty
// rule list with no timer yields nil (NULL column → match-all); a TIMER-1 timer config is
// persisted in the SAME condition JSON, so a date_offset rule always yields a non-nil blob even
// with zero entry-criteria rules. Each rule must name a non-empty field and a supported operator;
// value-bearing operators require a value, null operators must NOT carry one. saveOn (a before_save
// rule's create/update narrowing, already validated by checkBeforeRule) rides in the same JSON, so
// a saveOn-only condition is persisted too — dropping it would widen the rule to both halves.
func buildConditionJSON(match string, rules []WorkflowRuleInput, timer *workflow.TimerConfig, saveOn string) ([]byte, error) {
	if len(rules) == 0 && timer == nil && saveOn == "" {
		return nil, nil // match-all (no entry-criteria)
	}
	// joinMode is the persisted condition join: "or" fires on ANY rule, "and" (default) on ALL.
	// An empty/missing match defaults to "and" (backward compatible). Reject anything else rather
	// than silently coercing (validation-parity: a mistyped match must not pass quietly) — validated
	// unconditionally, even on a timer-only condition with zero entry-rules, so a typo can't slip in.
	joinMode := "and"
	switch strings.TrimSpace(strings.ToLower(match)) {
	case "", "and":
		joinMode = "and"
	case "or":
		joinMode = "or"
	default:
		return nil, invalid("WORKFLOW_CONDITION_INVALID", "match không hợp lệ: %q (chỉ 'and' hoặc 'or')", match)
	}
	cond := workflow.Condition{Match: joinMode, Rules: make([]workflow.Rule, 0, len(rules)), Timer: timer, SaveOn: saveOn}
	for _, r := range rules {
		field := strings.TrimSpace(r.Field)
		if field == "" {
			return nil, invalid("WORKFLOW_CONDITION_INVALID", "field của điều kiện là bắt buộc")
		}
		op := strings.TrimSpace(r.Op)
		if !workflow.ValidOperator(op) {
			return nil, invalid("WORKFLOW_CONDITION_INVALID",
				"operator không hợp lệ: %q (eq/neq/gt/gte/lt/lte/contains/isnull/notnull/changed/changedTo)", r.Op)
		}
		// `changed` asks only "did this field move", so like the null-operators it takes NO value.
		// `changedTo` pins the destination and therefore does need one.
		isNullOp := op == workflow.OpIsNull || op == workflow.OpNotNull || op == workflow.OpChanged
		if isNullOp {
			if r.Value != nil {
				return nil, invalid("WORKFLOW_CONDITION_INVALID",
					"operator %q không nhận value", op)
			}
		} else if r.Value == nil {
			return nil, invalid("WORKFLOW_CONDITION_INVALID",
				"operator %q cần value cho field %q", op, field)
		}
		// queue #286: an object value is a field reference and must be exactly {"field": "<name>"};
		// anything else would never match at record time.
		if m, isObj := r.Value.(map[string]any); isObj {
			if _, ok := workflow.RuleFieldRef(m); !ok {
				return nil, invalid("WORKFLOW_CONDITION_INVALID",
					`value của field %q phải là hằng số hoặc {"field": "<tên trường>"}`, field)
			}
		}
		cond.Rules = append(cond.Rules, workflow.Rule{Field: field, Op: op, Value: r.Value})
	}
	b, err := json.Marshal(cond)
	if err != nil {
		return nil, fmt.Errorf("services.buildConditionJSON: %w", err)
	}
	return b, nil
}

// buildActionsJSON validates the action list and marshals it to JSON. At least one action is
// required (a rule with no action is dead config). Each action's type-specific fields are checked:
//   - field_update:       field required; value may be any JSON scalar (including null to clear).
//     WF-01: when fieldTypes is non-nil the target is checked against sys_fields — an unknown
//     field is rejected (RECORD_UNKNOWN_FIELD) and a computed/assigned target (formula/lookup/
//     rollup/auto_number) is rejected with the SAME computedReadonlyCode family every other
//     write path (POST/PUT/CSV import) uses. auto_number is the critical one: it has NO read
//     gate and is never re-assigned on Update, so a workflow overwrite would corrupt it
//     PERMANENTLY (breaks the immutable/monotonic per-tenant promise).
//   - in_app_notification: userId + message required.
//
// fieldTypes is the target entity's field name → fieldType map (nil → skip the WF-01 target
// check, for checkers that cannot list fields).
//
// maxCreateRecordActionsPerRule caps how many create_record actions ONE rule may carry.
// ponytail: bounds the fan-out branching factor B in the geometric B^depth create_record cascade
// (partial mitigation of #79 — the depth exponent is capped separately by workflow.DefaultMaxChainDepth;
// a per-root total-records budget stays out of scope). Raise if a legitimate vertical needs more.
const maxCreateRecordActionsPerRule = 3

func buildActionsJSON(actions []WorkflowActionInput, fieldTypes map[string]string) ([]byte, error) {
	if len(actions) == 0 {
		return nil, invalid("WORKFLOW_ACTIONS_REQUIRED", "workflow cần ít nhất 1 action")
	}
	out := make([]workflow.Action, 0, len(actions))
	createRecordCount := 0
	for i, a := range actions {
		typ := strings.TrimSpace(a.Type)
		// specs/058: 'via' means something only to field_update. Stored on any other action it
		// would be accepted and then ignored — refuse it instead.
		if typ != workflow.ActionFieldUpdate && strings.TrimSpace(a.Via) != "" {
			return nil, invalid("WORKFLOW_ACTION_INVALID",
				"'via' chỉ dùng cho action field_update (action này là %q)", typ)
		}
		// specs/084: same for the children pair.
		if typ != workflow.ActionFieldUpdate && (strings.TrimSpace(a.ChildEntity) != "" || strings.TrimSpace(a.ChildVia) != "") {
			return nil, invalid("WORKFLOW_ACTION_INVALID",
				"'childEntity'/'childVia' chỉ dùng cho action field_update (action này là %q)", typ)
		}
		// specs/048 S5: same for 'ops' — only normalize reads it.
		if typ != workflow.ActionNormalize && len(a.Ops) > 0 {
			return nil, invalid("WORKFLOW_ACTION_INVALID",
				"'ops' chỉ dùng cho action normalize (action này là %q)", typ)
		}
		// specs/043 S9b: same for 'processId' — only start_approval reads it.
		if typ != workflow.ActionStartApproval && strings.TrimSpace(a.ProcessID) != "" {
			return nil, invalid("WORKFLOW_ACTION_INVALID",
				"'processId' chỉ dùng cho action start_approval (action này là %q)", typ)
		}
		switch typ {
		case workflow.ActionFieldUpdate:
			field := strings.TrimSpace(a.Field)
			if field == "" {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "action field_update cần 'field'")
			}
			via := strings.TrimSpace(a.Via)
			childEntity, childVia := strings.TrimSpace(a.ChildEntity), strings.TrimSpace(a.ChildVia)
			if (childEntity == "") != (childVia == "") {
				return nil, invalidField(childViaInvalidCode, "childVia", "'childEntity' và 'childVia' phải đi cùng nhau")
			}
			if childEntity != "" && via != "" {
				return nil, invalidField(childViaInvalidCode, "childVia", "không dùng 'via' cùng 'childEntity' — một action sửa một loại bản ghi")
			}
			if childEntity != "" {
				// specs/084: Field belongs to the CHILD entity; the relation is checked against the
				// child's metadata on the apply path (checkRelatedFieldTargets).
			} else if via != "" {
				// specs/058: Field belongs to the RELATED entity, so the WF-01 guard below (which reads
				// the rule's entity) does not apply; it runs against the target on the apply path
				// (checkRelatedFieldTargets). Here: via must be a relation of the rule's entity.
				if fieldTypes != nil && fieldTypes[via] != "relation" {
					return nil, invalidField(relatedViaInvalidCode, via,
						"'via' %q không phải trường quan hệ của entity của luật", via)
				}
			} else if fieldTypes != nil {
				// WF-01 config-time guard: the field_update target must be a real, client-writable
				// field. Reuses computedReadonlyCode (record_service.go) so the workflow path can
				// never drift from the record write paths on which types are readonly.
				ft, known := fieldTypes[field]
				if !known {
					return nil, invalidField("RECORD_UNKNOWN_FIELD", field,
						"field %q không tồn tại trong metadata", field)
				}
				if code, readonly := computedReadonlyCode(ft); readonly {
					return nil, invalidField(code, field,
						"field %q là computed/assigned field (%s) — workflow không được ghi đè", field, ft)
				}
			}
			// onlyIfEmpty/valueSource: before_save only — checkBeforeRule refused them elsewhere.
			out = append(out, workflow.Action{Type: typ, Field: field, Value: a.Value, Via: via, ChildEntity: childEntity, ChildVia: childVia,
				OnlyIfEmpty: a.OnlyIfEmpty, ValueSource: strings.TrimSpace(a.ValueSource)})
		case workflow.ActionInAppNotification:
			userID := strings.TrimSpace(a.UserID)
			if userID == "" {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "action in_app_notification cần 'userId'")
			}
			// #281: the recipient is a person, a role, a team or a user field of the record.
			if err := workflow.ValidateRecipientSpec(userID); err != nil {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "người nhận của action in_app_notification không hợp lệ: %q", userID)
			}
			if name, isField := strings.CutPrefix(userID, workflow.RecipientFieldPrefix); isField && fieldTypes != nil {
				if ft := fieldTypes[strings.TrimSpace(name)]; ft != "user" {
					return nil, invalidField("WORKFLOW_ACTION_INVALID", strings.TrimSpace(name),
						"người nhận theo trường: %q phải là trường kiểu người dùng của entity của luật", strings.TrimSpace(name))
				}
			}
			if strings.TrimSpace(a.Message) == "" {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "action in_app_notification cần 'message'")
			}
			out = append(out, workflow.Action{
				Type: typ, UserID: userID, Title: strings.TrimSpace(a.Title), Message: a.Message,
			})
		case workflow.ActionEmailAlert:
			// email_alert: recipient (to) + subject + body are required. All three may carry
			// {{field}} placeholders (rendered at fire time) — so we DON'T reject a "to" without
			// an '@' (it might be "{{ownerEmail}}"), but we do require the raw fields be present
			// so a misconfigured rule is caught at config time (validation-parity B6).
			to := strings.TrimSpace(a.To)
			if to == "" {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "action email_alert cần 'to'")
			}
			if strings.TrimSpace(a.Subject) == "" {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "action email_alert cần 'subject'")
			}
			if strings.TrimSpace(a.Body) == "" {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "action email_alert cần 'body'")
			}
			out = append(out, workflow.Action{
				Type: typ, To: to, Subject: a.Subject, Body: a.Body,
			})
		case workflow.ActionWebhook:
			// specs/050 S2b: an action naming a vault key carries nothing of its own — no url, no
			// secret, no header (all come from the key at send time) — and its path must keep to
			// the key's host (the SAME rule JoinCredentialURL applies when sending). The key's
			// existence in the tenant is the DB-dependent half: checkWebhookCredentials.
			if cid := strings.TrimSpace(a.CredentialID); cid != "" {
				for _, own := range []struct {
					field string
					set   bool
				}{{"url", strings.TrimSpace(a.URL) != ""}, {"secret", a.Secret != ""}, {"headers", len(a.Headers) > 0}} {
					if own.set {
						return nil, invalidField("WEBHOOK_URL_WITH_CREDENTIAL", fmt.Sprintf("actions[%d].%s", i, own.field),
							"action dùng khoá kết nối không kèm %s riêng — địa chỉ, bí mật và header đều lấy từ khoá", own.field)
					}
				}
				path := strings.TrimSpace(a.Path)
				// S3a: the mask of a redacted read sent back verbatim (old builder) — refuse it
				// rather than store "••••" as the real query value (ValidateCredentialPath lets
				// U+2022 through, and it is a valid path).
				if workflow.HasRedactedMarker(path) {
					return nil, redactedValue(i, "path")
				}
				if err := workflow.ValidateCredentialPath(path); err != nil {
					return nil, invalidField("WEBHOOK_PATH_INVALID", fmt.Sprintf("actions[%d].path", i),
						"path phải bắt đầu bằng /, tối đa 500 ký tự, không chứa ://, @, \\, #, khoảng trắng, đoạn .. (kể cả dạng mã hoá) hay dấu %% lẻ ngoài mã %%XX")
				}
				out = append(out, workflow.Action{Type: typ, CredentialID: cid, Path: path})
				continue
			}
			if strings.TrimSpace(a.Path) != "" {
				return nil, invalidField("WEBHOOK_PATH_INVALID", fmt.Sprintf("actions[%d].path", i),
					"path chỉ dùng cùng credentialId (khoá kết nối) — action có url thì ghi đủ đường dẫn vào url")
			}
			// specs/050 S3a (FR-025): a key lives in the vault, never on the action — refused on
			// every write path (admin API, pack install, AI Builder) rather than stored in plaintext
			// where GET /admin/workflows reads it back. Rows saved before S3a keep sending theirs.
			for _, own := range []struct {
				field string
				set   bool
			}{{"secret", a.Secret != ""}, {"headers", len(a.Headers) > 0}} {
				if own.set {
					return nil, invalidField("WEBHOOK_INLINE_SECRET_REMOVED", fmt.Sprintf("actions[%d].%s", i, own.field),
						"quy trình không còn giữ khoá ký/header riêng (%s) — cất khoá ở Quản trị › Kho khoá kết nối rồi chọn khoá đó cho action (credentialId + path)", own.field)
				}
			}
			// outbound_webhook: an absolute http/https URL is required. A relative/blank/non-http
			// URL is rejected at config time so a stored rule never carries a dead target.
			rawURL := strings.TrimSpace(a.URL)
			// S3a: checked before the parse — a masked password does not even parse, and a masked
			// query value does; either way the real value must not be replaced by the mask.
			if workflow.HasRedactedMarker(rawURL) {
				return nil, redactedValue(i, "url")
			}
			if rawURL == "" {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "action outbound_webhook cần 'url'")
			}
			u, perr := url.Parse(rawURL)
			if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, invalid("WORKFLOW_ACTION_INVALID",
					"action outbound_webhook cần 'url' http/https hợp lệ: %q", a.URL)
			}
			out = append(out, workflow.Action{Type: typ, URL: rawURL}) // no key of its own since S3a (above)
		case workflow.ActionAssign:
			// assign (ASSIGN-1): round-robin over a team into a text target field. This is the
			// pure, DB-free half of validation (strategy allowlist + field-target guard, shared by
			// every path via buildWorkflowRule). The team EXISTENCE check is DB-dependent and runs
			// on the apply path (checkAssignTeams); an empty team is still rejected HERE so a bad
			// rule can never persist without a target team.
			strategy := strings.TrimSpace(a.Strategy)
			if strategy == "" {
				strategy = workflow.StrategyRoundRobin // back-compat: omitted strategy defaults to round_robin
			}
			if !workflow.ValidStrategy(strategy) {
				return nil, invalid("ASSIGN_STRATEGY_INVALID",
					"strategy assign không hợp lệ: %q (round_robin/load_based)", a.Strategy)
			}
			team := strings.TrimSpace(a.Team)
			if team == "" {
				return nil, invalid("ASSIGN_TEAM_INVALID", "action assign cần 'team'")
			}
			field := strings.TrimSpace(a.Field)
			if field == "" {
				return nil, invalid("ASSIGN_FIELD_REQUIRED", "action assign cần 'field' đích")
			}
			// Field-target guard (when fieldTypes is armed): must exist, must NOT be a
			// computed/assigned field (RECORD_*_READONLY — SAME computedReadonlyCode family as
			// field_update, checked BEFORE the type check so a computed target reports readonly,
			// matching spec test #5), and must be a field that HOLDS a userId.
			//
			// `user` is the right type; `text` is the legacy one. When this guard was written the
			// `user` field type did not exist, so this comment read "a relation-to-users is
			// impossible" and text was the only option — which is why the CRM/Ticketing packs
			// model "Phân công cho" as free text and every screen printed a raw UUID (found
			// 2026-07-29 walking the customer face). The type exists now: it validates the target
			// is a live ACTIVE user of the tenant, and ?expand= resolves it to a name. `text` stays
			// accepted — every already-installed tenant has a text target and must keep routing.
			if fieldTypes != nil {
				ft, known := fieldTypes[field]
				if !known {
					return nil, invalidField("RECORD_UNKNOWN_FIELD", field,
						"field %q không tồn tại trong metadata", field)
				}
				if code, readonly := computedReadonlyCode(ft); readonly {
					return nil, invalidField(code, field,
						"field %q là computed/assigned field (%s) — assign không được ghi đè", field, ft)
				}
				if !assignTargetTypeOK(ft) {
					return nil, invalidField("ASSIGN_FIELD_TYPE_INVALID", field,
						"field %q phải là kiểu user (hoặc text) để ghi userId (đang là %s)", field, ft)
				}
			}
			out = append(out, workflow.Action{Type: typ, Strategy: strategy, Team: team, Field: field})
		case workflow.ActionCreateRecord:
			// create_record (CREATE-RECORD): this is the pure, DB-free half of validation — the
			// target entity name is non-empty and at least one value is supplied. The TARGET entity's
			// existence and its per-field key/type validity are DB-dependent (they need the target
			// entity's fields, which this trigger-scoped fieldTypes map does not carry) and run on the
			// apply path (checkCreateRecordTargets), exactly like assign's team-existence check.
			entity := strings.TrimSpace(a.CreateEntity)
			if entity == "" {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "action create_record cần 'entityName'")
			}
			if len(a.Values) == 0 {
				return nil, invalid("CREATE_RECORD_VALUES_REQUIRED", "action create_record cần ít nhất 1 giá trị trong 'values'")
			}
			createRecordCount++
			if createRecordCount > maxCreateRecordActionsPerRule {
				return nil, invalid("WORKFLOW_TOO_MANY_CREATE_RECORD",
					"một workflow rule chỉ được tối đa %d action create_record (chặn fan-out tự-động-hoá)", maxCreateRecordActionsPerRule)
			}
			out = append(out, workflow.Action{Type: typ, CreateEntity: entity, Values: a.Values})
		case workflow.ActionSetDeadline:
			// set_deadline (DAILY-QUEUE #9): compute "now + amount×unit" into a datetime field.
			// Everything here is DB-free, so it runs on every path via buildWorkflowRule.
			field := strings.TrimSpace(a.Field)
			if field == "" {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "action set_deadline cần 'field' đích")
			}
			if a.Offset == nil {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "action set_deadline cần 'offset' {amount, unit}")
			}
			if !workflow.ValidTimerAmount(a.Offset.Amount) {
				return nil, invalid("SET_DEADLINE_OFFSET_INVALID",
					"offset.amount của set_deadline phải là số dương: %d", a.Offset.Amount)
			}
			// business_hours is accepted since DAILY-QUEUE #10: the action resolves the tenant's
			// working calendar through the SAME lookup the date_offset timer uses. A tenant with
			// no calendar still gets the documented wall-clock fallback, logged — and a deployment
			// that forgot to wire the resolver gets a FAILED run, not a plausible wrong number.
			if !workflow.ValidTimerUnit(a.Offset.Unit) {
				return nil, invalid("SET_DEADLINE_UNIT_INVALID",
					"offset.unit của set_deadline phải là minutes/hours/days/business_hours (đang là %q)", a.Offset.Unit)
			}
			// Field-target guard (when fieldTypes is armed), SAME order as assign: must exist,
			// must not be computed/assigned (WF-01 family), and must be able to HOLD an instant.
			// The type check is not cosmetic — the action writes RFC3339, which a `date` field's
			// validator rejects at fire time, so without it the rule saves and every single
			// firing is a FAILED run in a history nobody is watching.
			if fieldTypes != nil {
				ft, known := fieldTypes[field]
				if !known {
					return nil, invalidField("RECORD_UNKNOWN_FIELD", field,
						"field %q không tồn tại trong metadata", field)
				}
				if code, readonly := computedReadonlyCode(ft); readonly {
					return nil, invalidField(code, field,
						"field %q là computed/assigned field (%s) — set_deadline không được ghi đè", field, ft)
				}
				if ft != "datetime" {
					return nil, invalidField("SET_DEADLINE_FIELD_TYPE_INVALID", field,
						"field %q phải là kiểu datetime để giữ được hạn có giờ (đang là %s)", field, ft)
				}
			}
			mode, stamp, anchor, err := setDeadlinePolicy(a, field, fieldTypes)
			if err != nil {
				return nil, err
			}
			// calendarId (queue #79): the timer's pin rule, same reason — a wall-clock unit walks
			// no calendar, so a pin on it is a setting that does nothing. Existence in the tenant is
			// DB-dependent and checked on the apply path (checkDeadlineCalendars).
			calendarID := strings.TrimSpace(a.CalendarID)
			if calendarID != "" && a.Offset.Unit != workflow.TimerUnitBusinessHours {
				return nil, invalidField("SET_DEADLINE_CALENDAR_INVALID", actionCalendarPointer(i),
					"calendarId của set_deadline chỉ dùng với offset.unit business_hours (đang là %q — đơn vị này không đi theo lịch làm việc nào)", a.Offset.Unit)
			}
			out = append(out, workflow.Action{
				Type: typ, Field: field,
				Offset: &workflow.TimerOffset{Amount: a.Offset.Amount, Unit: a.Offset.Unit},
				Mode:   mode, StampField: stamp, Anchor: anchor, CalendarID: calendarID,
			})
		case workflow.ActionSetElapsed:
			// set_elapsed (DAILY-QUEUE #41): write the WORKING minutes since the record was created
			// into a number field. DB-free, runs on every path via buildWorkflowRule. Same guard order
			// as set_deadline; the type must be a plain `number` because the action writes a float64
			// count — currency wants an object and rating is 1-5, so either would save cleanly and
			// fail on every firing.
			field := strings.TrimSpace(a.Field)
			if field == "" {
				return nil, invalid("WORKFLOW_ACTION_INVALID", "action set_elapsed cần 'field' đích")
			}
			if fieldTypes != nil {
				ft, known := fieldTypes[field]
				if !known {
					return nil, invalidField("RECORD_UNKNOWN_FIELD", field,
						"field %q không tồn tại trong metadata", field)
				}
				if code, readonly := computedReadonlyCode(ft); readonly {
					return nil, invalidField(code, field,
						"field %q là computed/assigned field (%s) — set_elapsed không được ghi đè", field, ft)
				}
				if ft != "number" {
					return nil, invalidField("SET_ELAPSED_FIELD_TYPE_INVALID", field,
						"field %q phải là kiểu number để giữ số phút (đang là %s)", field, ft)
				}
			}
			out = append(out, workflow.Action{Type: typ, Field: field})
		case workflow.ActionVeto:
			// veto (specs/048): the trigger/rule-shape half lives in checkBeforeRule (only on before_*,
			// alone in its rule); this is the per-action half. The message is what the user reads under
			// the input, so it must say something and fit (1-500 characters, trimmed). The anchor field
			// is optional; when given it must exist so the form can place the error.
			msg := strings.TrimSpace(a.Message)
			if n := utf8.RuneCountInString(msg); n == 0 || n > maxVetoMessageRunes {
				return nil, &ValidationError{Code: "WORKFLOW_VETO_MESSAGE_INVALID",
					Msg:     fmt.Sprintf("thông điệp chặn phải có 1–%d ký tự (đang có %d)", maxVetoMessageRunes, n),
					Details: map[string]any{"max": maxVetoMessageRunes}}
			}
			field := strings.TrimSpace(a.Field)
			if field != "" && fieldTypes != nil {
				if _, known := fieldTypes[field]; !known {
					return nil, invalidField("RECORD_UNKNOWN_FIELD", field,
						"field %q không tồn tại trong metadata", field)
				}
			}
			out = append(out, workflow.Action{Type: typ, Field: field, Message: msg})
		case workflow.ActionStartApproval:
			// start_approval (specs/043 S9b): the pure half — a process id must be named. Its
			// existence, kind and entity are DB-dependent (checkStartApprovalTargets).
			processID := strings.TrimSpace(a.ProcessID)
			if processID == "" {
				return nil, invalidField("WF_START_APPROVAL_INVALID", actionPointer(i), "action start_approval cần 'processId' (quy trình duyệt gửi tay)")
			}
			out = append(out, workflow.Action{Type: typ, ProcessID: processID})
		case workflow.ActionNormalize:
			// normalize (specs/048 S5): trigger, field type and ops were judged by checkBeforeRule
			// (before_save only); stored trimmed, in the order given — the order is the rewrite.
			ops := make([]string, len(a.Ops))
			for i, op := range a.Ops {
				ops[i] = strings.TrimSpace(op)
			}
			out = append(out, workflow.Action{Type: typ, Field: strings.TrimSpace(a.Field), Ops: ops})
		default:
			return nil, invalid("WORKFLOW_ACTION_INVALID",
				"action type không hợp lệ: %q (field_update/in_app_notification/email_alert/outbound_webhook/assign/create_record/set_deadline/set_elapsed/start_approval/veto/normalize)", a.Type)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("services.buildActionsJSON: %w", err)
	}
	return b, nil
}

// setDeadlinePolicy validates and canonicalizes set_deadline's mode / stampField / anchor (queue
// #81, specs/043 S14a) — the config-time twin of workflow.PlanDeadline's run-time backstop. The
// defaults (overwrite, now) come back as "" so they are stored ABSENT: a rule saved before these
// options existed and one saved after with the defaults are byte-identical, and the pack diff never
// reports a phantom change on either.
//
// The stamp must be a separate, writable datetime field: tighten_only tells the machine's deadline
// from a hand-typed one by comparing the two, so a stamp that is the deadline field itself (always
// equal) or one the write path rejects (date/text/computed) would make every deadline look like the
// machine's — or none.
func setDeadlinePolicy(a WorkflowActionInput, field string, fieldTypes map[string]string) (mode, stamp, anchor string, err error) {
	mode = strings.TrimSpace(a.Mode)
	switch mode {
	case "", workflow.DeadlineModeOverwrite:
		mode = ""
	case workflow.DeadlineModeTightenOnly:
	default:
		return "", "", "", invalid("WORKFLOW_ACTION_INVALID",
			"mode của set_deadline phải là overwrite/tighten_only (đang là %q)", a.Mode)
	}
	anchor = strings.TrimSpace(a.Anchor)
	switch anchor {
	case "", workflow.DeadlineAnchorNow:
		anchor = ""
	case workflow.DeadlineAnchorCreatedAt:
	default:
		return "", "", "", invalid("WORKFLOW_ACTION_INVALID",
			"anchor của set_deadline phải là now/created_at (đang là %q)", a.Anchor)
	}
	stamp = strings.TrimSpace(a.StampField)
	if stamp == "" {
		if mode == workflow.DeadlineModeTightenOnly {
			return "", "", "", invalid("SET_DEADLINE_STAMP_FIELD_INVALID",
				"mode tighten_only cần 'stampField' — một trường datetime giữ dấu hạn máy, để không bao giờ đè hạn người dùng tự gõ")
		}
		return mode, "", anchor, nil
	}
	if stamp == field {
		return "", "", "", invalidField("SET_DEADLINE_STAMP_FIELD_INVALID", stamp,
			"stampField phải khác field đích %q — dấu hạn máy trùng chính trường hạn thì không phân biệt được hạn gõ tay", field)
	}
	if fieldTypes != nil {
		ft, known := fieldTypes[stamp]
		if !known {
			return "", "", "", invalidField("SET_DEADLINE_STAMP_FIELD_INVALID", stamp,
				"stampField %q không tồn tại trong metadata", stamp)
		}
		if _, readonly := computedReadonlyCode(ft); readonly || ft != "datetime" {
			return "", "", "", invalidField("SET_DEADLINE_STAMP_FIELD_INVALID", stamp,
				"stampField %q phải là trường datetime ghi được (đang là %s)", stamp, ft)
		}
	}
	return mode, stamp, anchor, nil
}
