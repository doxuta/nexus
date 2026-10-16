// Package workflow is the EPIC 4 M1 Workflow/Automation foundation (spec:
// docs/ENGINE-COVERAGE-SPEC.md §EPIC 4; benchmark: Salesforce Workflow Rules / Process Builder
// + SugarCRM Logic Hooks). It turns the metadata-driven database into business software: an
// admin registers a rule ("khi <trigger> trên <entity>, NẾU <condition> THÌ chạy <actions>")
// and the Event Engine fires it AFTER a record mutation commits.
//
// It sits below the service layer exactly like internal/audit: RecordService depends only on the
// consumer-side Engine interface and calls Emit, which returns immediately. The concrete
// AsyncDispatcher drains a buffered channel from a pool of workers, loads the active workflows of
// the mutated entity (through a TTL cache), evaluates each rule's condition, and runs its actions.
//
// Invariants (rules + spec):
//   - ASYNC + FAIL-OPEN: a full buffer, a load error, a condition error, or an action error NEVER
//     blocks, slows, or breaks the originating mutation (it has already committed). Worst case is
//     a skipped/failed rule with a zap warning — same contract as the A7 audit dispatcher.
//   - Multi-tenant: every Event carries TenantID and every repository query filters tenant_id —
//     a workflow can never be loaded, or a notification written, across tenants.
//   - Metadata-driven: workflows are rows in sys_workflows, never hardcoded Go structs.
package workflow

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Trigger is the after-commit record event a workflow fires on. The values match the
// sys_workflows.trigger_event ENUM and the RecordService mutation that emits them.
type Trigger string

const (
	TriggerAfterCreate Trigger = "after_create"
	TriggerAfterUpdate Trigger = "after_update"
	TriggerAfterDelete Trigger = "after_delete"
	// TriggerScheduled is the M2 time-based trigger (spec §EPIC 4 "Background scheduler /
	// time-based events"). A scheduled rule fires from a background ticker (not a record
	// mutation): the scheduler scans due rules, evaluates the condition against each of the
	// entity's records, and runs the actions on every matching record. It is gated by the
	// rule's interval_seconds + last_run_at so it never double-fires (see scheduler.go).
	TriggerScheduled Trigger = "scheduled"
	// TriggerDateOffset is the TIMER-1 date-anchored trigger (engine-depth primitive #1b,
	// migration 000048): the rule fires at "amount×unit before/after the record's date field"
	// (config in Condition.Timer — see timer.go). Scheduling piggybacks on the SAME
	// post-mutation dispatch after_create/after_update use: the dispatcher parks a
	// 'workflow_timer' sys_jobs wake-up row (JOBS-1) at the computed instant, and the job
	// handler RE-EVALUATES everything at fire time (the handler is the source of truth; the
	// parked job is just a wake-up — see services/workflow_timer.go).
	TriggerDateOffset Trigger = "date_offset"
	// TriggerBeforeSave / TriggerBeforeDelete are the specs/048 SYNCHRONOUS rules: they run inside
	// the save (create/update — narrowed by Condition.SaveOn) or the delete, BEFORE the store call,
	// and may only veto or (later slices) change the values about to be written — never reach
	// outside the record. They are loaded by RecordService, not by AsyncDispatcher/scheduler (which
	// query by after_*/scheduled/date_offset and so never see them). The async contract at the top
	// of this file (fail-open, after commit) does NOT apply to them.
	TriggerBeforeSave   Trigger = "before_save"
	TriggerBeforeDelete Trigger = "before_delete"
)

// ValidTrigger reports whether t is one of the supported triggers (three after-commit events +
// the M2 scheduled trigger + the TIMER-1 date_offset trigger + the two specs/048 before triggers).
func ValidTrigger(t Trigger) bool {
	switch t {
	case TriggerAfterCreate, TriggerAfterUpdate, TriggerAfterDelete, TriggerScheduled, TriggerDateOffset,
		TriggerBeforeSave, TriggerBeforeDelete:
		return true
	default:
		return false
	}
}

// IsBeforeTrigger reports whether t is one of the synchronous specs/048 triggers.
func IsBeforeTrigger(t Trigger) bool {
	return t == TriggerBeforeSave || t == TriggerBeforeDelete
}

// Condition.SaveOn values: which half of a before_save rule applies. Absent = both.
const (
	SaveOnCreate = "create"
	SaveOnUpdate = "update"
)

// Action types supported in M1 (spec §EPIC 4 "Action: field update", "Action: in-app
// notification") plus M2 (spec §EPIC 4 "Action: email_alert", "Action: outbound webhook").
// Later milestones add create_record / task.
const (
	ActionFieldUpdate       = "field_update"
	ActionInAppNotification = "in_app_notification"
	// ActionEmailAlert sends an email through the SMTP adapter (reusing internal/notify's
	// transport contract). Subject/Body support {{field}} interpolation from the record. M2.
	ActionEmailAlert = "email_alert"
	// ActionWebhook POSTs the record as JSON to an external URL with retry/backoff + an HMAC
	// signature header. Fail-open: a down endpoint never blocks the originating flow. M2.
	ActionWebhook = "outbound_webhook"
	// ActionAssign hands the triggering record to the NEXT member of a team via round-robin
	// (ASSIGN-1, engine-depth primitive #4): it resolves the next assignee through a per-rule
	// rotating cursor (sys_assignment_cursors) and writes that userId into a `user` or `text` field (assignTargetTypeOK) of the
	// record — the SAME write path field_update uses (system scope, RLS never blocks it). It does
	// NOT change ownership/ACL (see the spec §2 warning): assign = set a "who handles this" field.
	ActionAssign = "assign"
	// ActionCreateRecord creates a brand-new record of a target entity (CREATE-RECORD): e.g. an
	// after_create rule on a lead spawns a follow-up task. It runs the SAME RecordService.Create
	// the API uses (full validation / ACL / tenant scoping / pivot maintenance — no second write
	// path), acting as a SYSTEM principal, with the trigger record's owner as created_by. Because a
	// created record can re-trigger a create_record rule, recursion is bounded by Event.Depth and
	// the dispatcher's max chain depth (WORKFLOW_MAX_CHAIN_DEPTH) — see runCreateRecord.
	ActionCreateRecord = "create_record"
	// ActionSetDeadline COMPUTES a commitment date and writes it into a datetime field of the
	// triggering record — "phiếu Khẩn phải xong trong 4 giờ" stops being a verbal convention and
	// becomes software. It is the missing producer: field_update can only write a CONSTANT
	// (runFieldUpdate stores a.Value verbatim) and a formula field cannot yield a date (the
	// internal/formula package has no function calls and no date arithmetic), so before this
	// action nothing in Nexus could put a computed due date on a record — every deadline was typed
	// by hand, ticket by ticket.
	//
	// The offset is the SAME {amount, unit} vocabulary the date_offset trigger uses (TimerOffset),
	// and the arithmetic is the SAME ComputeFireAt — a deadline and a reminder must never disagree
	// about what "+4 hours" means. It anchors on the FIRE instant (now) and accepts the FULL unit
	// vocabulary — business_hours included since DAILY-QUEUE #10 threaded the tenant calendar into
	// the action deps (actionDeps.sched). Three states would otherwise produce the same wall-clock
	// number: no resolver wired and a failing calendar store are ERRORS, and only "this tenant has
	// no calendar" degrades — loudly. See runSetDeadline in actions.go.
	//
	// By default it OVERWRITES an existing value, exactly like field_update: an SLA is policy, and a
	// rule that quietly skipped records someone had already touched would be a deadline you cannot
	// trust. Scope it with the rule's condition, not with a hidden exception. The one sanctioned
	// exception is explicit and opt-in (queue #81): Mode tighten_only + StampField, which only ever
	// SHORTENS a deadline the machine can prove it wrote — see PlanDeadline in actions.go.
	ActionSetDeadline = "set_deadline"
	// ActionSetElapsed is set_deadline's other half: it MEASURES instead of promising. It writes
	// into a number field of the triggering record how many WORKING minutes passed between the
	// record's creation (Event.CreatedAt) and the event's instant (Event.OccurredAt — the commit of
	// the save that fired the rule, not the moment a worker drained it), walked through the tenant's default
	// business calendar with calendar.BusinessDurationBetween — the same schedule set_deadline and
	// the date_offset timer resolve (DAILY-QUEUE #41). "How long did this ticket take" had no
	// answer before it: field_update writes constants, formula has no date arithmetic, and
	// BusinessDurationBetween had no caller outside its own tests.
	//
	// The number is whole minutes, rounded DOWN, handed over as float64 (the only type the record
	// validator's number arm accepts). It OVERWRITES like every other writer — so an after_update
	// rule should use `changedTo` on the closing status, or every later save of a closed record
	// re-stamps a larger number. Reopen-then-close measures from creation again, by design.
	// No calendar → wall-clock minutes + a run-history warning, the same three-state contract as
	// set_deadline (runSetElapsed in actions.go).
	ActionSetElapsed = "set_elapsed"
	// ActionVeto (specs/048) refuses the save/delete a before rule matched: Message (1-500 chars) is
	// shown to the user, Field (optional) anchors it under that input. Only valid on before_* rules,
	// alone in its rule.
	ActionVeto = "veto"
	// ActionNormalize (specs/048 S5) rewrites a text Field in place with Ops (trim/lower/upper/
	// collapse_spaces) before it is written. Only valid on before_save rules.
	ActionNormalize = "normalize"
	// ActionStartApproval (specs/043 S9b) sends the record the rule fired for through a named MANUAL
	// approval process — "hợp đồng sắp hết hạn → khởi động quy trình duyệt gia hạn". It is the same
	// start a person's "Gửi duyệt" makes (criteria evaluated on the record, one open approval per
	// record, the record's owner recorded as the sender), so an event, scheduled or date_offset rule
	// can open a hồ sơ without anyone pressing a button. A record that already waits is a no-op with
	// a note, not a failure: a scheduled rule fires again every tick. Bounded by Event.Depth like
	// create_record. See runStartApproval in actions.go.
	ActionStartApproval = "start_approval"
)

// DefaultMaxChainDepth bounds create_record automation recursion when WORKFLOW_MAX_CHAIN_DEPTH is
// unset or invalid, and is the clamp WithRecordCreator applies to a non-positive maxDepth so a
// mis-set 0 never disables create_record entirely (the depth guard would otherwise skip at depth 0).
const DefaultMaxChainDepth = 5

// Assignment strategies for the assign action:
//   - round_robin: a per-rule cursor rotates through a team's active members (ASSIGN-1).
//   - load_based:  pick the active member with the FEWEST active assigned records in the trigger
//     entity, tie-broken by the SAME round-robin cursor (ASSIGN-2). queue-claim is a later slice.
const (
	StrategyRoundRobin = "round_robin"
	StrategyLoadBased  = "load_based"
)

// ValidStrategy reports whether s is a supported assign strategy. An empty strategy is NOT accepted
// here — the config layer normalizes "" to round_robin (back-compat) before this check.
func ValidStrategy(s string) bool {
	switch strings.TrimSpace(s) {
	case StrategyRoundRobin, StrategyLoadBased:
		return true
	}
	return false
}

// Condition operators (mirror the record-list FilterOperator vocabulary so admins reuse a
// familiar set). M1 keeps it deliberately small: equality/inequality, ordered comparisons on
// numbers, substring contains, and presence checks. All rules in a condition are AND-joined.
const (
	OpEq       = "eq"
	OpNeq      = "neq"
	OpGt       = "gt"
	OpGte      = "gte"
	OpLt       = "lt"
	OpLte      = "lte"
	OpContains = "contains"
	OpIsNull   = "isnull"
	OpNotNull  = "notnull"
	// OpChanged / OpChangedTo are EDGE predicates: they compare the record against its own
	// before-image instead of against a constant. Every other operator asks "is it in this state";
	// these ask "did it just enter it".
	//
	// The difference is the difference between a rule that fires once and a rule that fires on
	// every subsequent save. "Send the contract when the quote becomes Accepted" written with eq
	// re-sends on each later edit of an already-accepted quote; written with changedTo it fires on
	// the one save that made it true.
	OpChanged   = "changed"
	OpChangedTo = "changedTo"
)

// validOperators is the closed set a condition rule may use. A rule naming anything outside it
// is rejected at config time (service validation) so a stored rule never carries a dead operator.
var validOperators = map[string]struct{}{
	OpEq: {}, OpNeq: {}, OpGt: {}, OpGte: {}, OpLt: {}, OpLte: {},
	OpContains: {}, OpIsNull: {}, OpNotNull: {},
	OpChanged: {}, OpChangedTo: {},
}

// ValidOperator reports whether op is a supported condition operator.
func ValidOperator(op string) bool {
	_, ok := validOperators[strings.TrimSpace(op)]
	return ok
}

// Operators returns the supported condition operators, sorted — for callers that must advertise
// the exact set (the AI Builder's condition schema pins its enum to this).
func Operators() []string {
	return slices.Sorted(maps.Keys(validOperators))
}

// Rule is a single field-op-value predicate evaluated against the record's data.
type Rule struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	// Value is the comparison operand. It is decoded from JSON as any (string/float64/bool);
	// the evaluator coerces per operator. Absent for isnull/notnull.
	Value any `json:"value,omitempty"`
}

// Condition is the entry-criteria of a workflow: a set of AND-joined rules. Match is reserved
// for a future OR mode; M1 always treats the rules as AND ("and" or empty). An empty/nil Rules
// list matches every event (a rule with no entry-criteria — like a Salesforce "every time" rule).
// Timer is the TIMER-1 date_offset config, stored in the SAME condition JSON (no new column) —
// present exactly when Trigger is date_offset, nil otherwise. Evaluate ignores it (it is
// scheduling config, not a predicate).
type Condition struct {
	Match string       `json:"match,omitempty"` // "and" (default); reserved for "or"
	Rules []Rule       `json:"rules,omitempty"`
	Timer *TimerConfig `json:"timer,omitempty"` // date_offset trigger config (timer.go)
	// SaveOn narrows a before_save rule to creates or updates (SaveOnCreate/SaveOnUpdate); absent =
	// both. Stored here like Timer (no new column); rejected on every other trigger at config time.
	SaveOn string `json:"saveOn,omitempty"`
}

// Action is one automation step. Type is one of the Action* constants. The remaining fields are
// type-specific and validated at config time:
//   - field_update:       Field + Value (set Field = Value on the SAME record).
//   - in_app_notification: UserID (recipient) + Message (+ optional Title).
//   - email_alert:         To (recipient address) + Subject + Body. Subject/Body support
//     {{field}} interpolation from the record data (rendered at fire time).
//   - outbound_webhook:    URL (POST target). Optional Secret (HMAC signing key) + Headers.
//     OR (specs/050 S2b) CredentialID + optional Path: the target, auth and signing key come from
//     the tenant's named-credential vault, read at send time — never stored on the rule.
//   - assign:              Strategy ("round_robin") + Team (id) + Field (the user/text field the
//     next-assignee userId is written into).
type Action struct {
	Type string `json:"type"`
	// field_update
	Field string `json:"field,omitempty"`
	// Value is a constant, or (after_* triggers, queue #333) resolved at fire time exactly like a
	// create_record value: "$owner", "$record", "$record.<field>", "{{field}}" — ResolveFieldUpdateValue.
	Value any `json:"value,omitempty"`
	// Via (field_update, specs/058) names a to-one `relation` field of the rule's entity. Empty =
	// write Field on the triggering record (every rule stored before Via existed). Set = read the
	// related record's id from the triggering record's Via value and write Field — a field of the
	// RELATED entity (options.relatedEntity, resolved from metadata at fire time) — on THAT record.
	Via string `json:"via,omitempty"`
	// ChildEntity + ChildVia (field_update, specs/084) fan the write out to EVERY child record: the
	// rows of ChildEntity whose to-one relation field ChildVia points at the triggering record
	// ("lớp Đã đóng ⇒ mọi ghi danh đổi trạng thái"). Both or neither; exclusive with Via. Field is a
	// field of ChildEntity. Bounded by MaxChildUpdatesPerRun.
	ChildEntity string `json:"childEntity,omitempty"`
	ChildVia    string `json:"childVia,omitempty"`
	// in_app_notification
	UserID  string `json:"userId,omitempty"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
	// email_alert
	To      string `json:"to,omitempty"`      // recipient address (may contain {{field}})
	Subject string `json:"subject,omitempty"` // subject template ({{field}} interpolation)
	Body    string `json:"body,omitempty"`    // body template ({{field}} interpolation)
	// outbound_webhook
	URL     string            `json:"url,omitempty"`     // POST target (absolute http/https URL)
	Secret  string            `json:"secret,omitempty"`  // HMAC-SHA256 signing key (optional)
	Headers map[string]string `json:"headers,omitempty"` // extra static headers (optional)
	// outbound_webhook through the named-credential vault (specs/050 S2b): the key's id and the
	// path appended to its base URL. Mutually exclusive with URL/Secret/Headers (config-time).
	CredentialID string `json:"credentialId,omitempty"`
	Path         string `json:"path,omitempty"`
	// assign (ASSIGN-1): Strategy is the rotation strategy (v1: "round_robin"); Team is the
	// team id whose active members are rotated through. Field (reused from field_update above)
	// is the user/text field the picked userId is written into.
	Strategy string `json:"strategy,omitempty"`
	Team     string `json:"team,omitempty"`
	// create_record (CREATE-RECORD): CreateEntity is the target entity to insert a new record into;
	// Values is the field→value map for that record. Values are literals plus the "$owner" sentinel
	// (resolved at fire time to the trigger record's owner, e.g. to own the follow-up task),
	// "$record" (the trigger record's id), "$record.<field>" (typed copy) and "{{field}}" templates —
	// see resolveCreateValues. Config-time validation (buildActionsJSON + checkCreateRecordTargets)
	// asserts CreateEntity exists in the tenant and every Values key is a known, non-readonly field.
	CreateEntity string         `json:"entityName,omitempty"`
	Values       map[string]any `json:"values,omitempty"`
	// set_deadline: Offset is how far AFTER the anchor the deadline lands, in the SAME
	// {amount, unit} vocabulary a date_offset timer uses. Field (reused from field_update above)
	// is the datetime field the computed instant is written into.
	Offset *TimerOffset `json:"offset,omitempty"`
	// set_deadline (queue #81, specs/043 S14a). All three are optional and their zero value is
	// today's behaviour, so every rule stored before them reads back unchanged:
	//   - Mode: "" / overwrite = write the computed instant; tighten_only = write it only when it
	//     is EARLIER than the deadline on the record AND that deadline is provably the machine's
	//     own (Field still equals StampField). A promised deadline is never lengthened and a
	//     hand-typed one is never touched.
	//   - StampField: a datetime field that receives a copy of every instant this action writes —
	//     the machine's signature. Required for tighten_only.
	//   - Anchor: "" / now = the fire instant; created_at = the record's creation instant, so a
	//     recomputation after a priority change measures from when the promise was made.
	Mode       string `json:"mode,omitempty"`
	StampField string `json:"stampField,omitempty"`
	Anchor     string `json:"anchor,omitempty"`
	// CalendarID (set_deadline, queue #79) pins the sys_business_calendars row a business_hours
	// offset walks; "" = the tenant's default calendar, which is every rule saved before it. Same
	// meaning as the date_offset trigger's TimerConfig.CalendarID, resolved through the same
	// ScheduleResolver — a deadline and a reminder pinned to one calendar agree on the hours.
	CalendarID string `json:"calendarId,omitempty"`
	// specs/048 veto reuses Field (optional anchor) + Message. The before-rule value-change keys are
	// added by the slices whose runtime reads them: a key declared here must travel through the pack
	// and the service input (TestPackWorkflowAction_CarriesEveryEngineActionField), and one accepted
	// before anything reads it is the "accepted then swallowed" class.
	//
	// field_update on a before_save rule (S4, read by ApplyMutations):
	//   - OnlyIfEmpty: write only when the field is empty (IsBlank) at that point of the save.
	//   - ValueSource: how Value is read — "" / constant = Value as is (a string starting with "$" is
	//     a literal, never a reference), field = Value names a field of the record whose typed value is
	//     copied, template = Value is text with {{field}} placeholders (text targets only).
	// Both are refused on every other trigger at config time.
	OnlyIfEmpty bool   `json:"onlyIfEmpty,omitempty"`
	ValueSource string `json:"valueSource,omitempty"`
	// Ops (normalize, S5) are the rewrites applied to Field's text, in this order: 1-4 distinct
	// Normalize* values, checked when the rule is saved.
	Ops []string `json:"ops,omitempty"`
	// ProcessID (start_approval, specs/043 S9b) is the manual approval process to start. Checked when
	// the rule is saved (exists, manual, active, same entity); a pack carries it as a process NAME.
	ProcessID string `json:"processId,omitempty"`
}

// The normalize operations (Action.Ops, specs/048 S5). Whitespace is unicode.IsSpace — the same set
// strings.TrimSpace and the record validator's "empty" read — so a no-break space counts too.
const (
	NormalizeTrim           = "trim"            // drop leading and trailing whitespace
	NormalizeLower          = "lower"           // lower-case every letter (Vietnamese included)
	NormalizeUpper          = "upper"           // upper-case every letter
	NormalizeCollapseSpaces = "collapse_spaces" // every run of whitespace becomes one space
)

// MaxNormalizeOps caps the operations of one normalize action: there are four, each at most once.
const MaxNormalizeOps = 4

// ValidNormalizeOp reports whether op is one of the Normalize* operations.
func ValidNormalizeOp(op string) bool {
	switch op {
	case NormalizeTrim, NormalizeLower, NormalizeUpper, NormalizeCollapseSpaces:
		return true
	}
	return false
}

// Action.ValueSource values (specs/048 S4). The empty string means constant.
const (
	ValueSourceConstant = "constant"
	ValueSourceField    = "field"
	ValueSourceTemplate = "template"
)

// set_deadline modes and anchors (Action.Mode / Action.Anchor). The empty string means the first of
// each pair; the config layer stores the empty string for it so a rule written before these
// existed and one written after with the default are byte-identical.
const (
	DeadlineModeOverwrite   = "overwrite"
	DeadlineModeTightenOnly = "tighten_only"
	DeadlineAnchorNow       = "now"
	DeadlineAnchorCreatedAt = "created_at"
)

// Workflow is a loaded rule (one sys_workflows row). Condition is the entry-criteria (nil/empty
// matches all). Actions run in order. Only IsActive, non-deleted rows are loaded by the engine.
type Workflow struct {
	ID         string
	TenantID   string
	Name       string
	EntityName string
	Trigger    Trigger
	Condition  *Condition
	Actions    []Action
	IsActive   bool
}

// Event is the payload RecordService emits AFTER a record mutation commits. Data is the record's
// committed field map used for condition evaluation AND as the field_update target (the engine
// re-reads/patches the same record by RecordID). ActorUserID is the verified mutator (carried for
// audit context; the engine itself acts as a system principal when applying field_update so RLS
// never blocks an automation) — it is NOT the record's owner (see OwnerUserID). Delete events
// carry the before-image in Data.
type Event struct {
	TenantID    string
	ActorUserID string
	EntityName  string
	RecordID    string
	Trigger     Trigger
	Data        map[string]any
	// OwnerUserID is the record's immutable creator (obj_*.created_by), NOT the actor who
	// triggered this event. It is the SOLE source the engine resolves the in_app_notification
	// "$owner" sentinel from (runInAppNotification, actions.go) — MED-1 fix (M7 review
	// 2026-07-08): the FE WorkflowBuilder and the AI Builder persona both offer "$owner" as a
	// recipient shorthand, but prior to this field the engine inserted the literal string
	// "$owner" as a user_id, producing a silently-dead notification to a nonexistent user on
	// EVERY rule using the sentinel. Every caller that builds an Event MUST populate this from
	// the record's created_by (never from ActorUserID, which is the mutator/deleter, not the
	// owner) — see record_orchestration.go (create/update/delete), scheduler.go (scheduled),
	// workflow_runs.go (dry-run preview). Empty when the owner could not be resolved (e.g. a
	// best-effort before-image read failed) — the resolver then falls through to the existing
	// empty-recipient error rather than inserting a dead row.
	OwnerUserID string
	// Depth is the automation chain depth of this event (CREATE-RECORD recursion guard): 0 for an
	// event emitted by a user-driven (API / CSV import) mutation, and parent+1 for an event emitted
	// by a create_record action's nested RecordService.Create. The dispatcher SKIPS a create_record
	// action once Depth >= the configured max chain depth (WORKFLOW_MAX_CHAIN_DEPTH), so a rule that
	// creates a record which re-triggers the same rule can never loop forever. It rides the Event
	// (NOT ctx) because immediate-trigger dispatch is async: the worker processes each event under a
	// fresh context.Background(), so ctx cannot carry depth across the channel. The ONE hop where
	// depth travels via ctx is action → nested Create → emit: runCreateRecord stashes depth+1 with
	// WithChainDepth, and services.emitCreateEvents reads it back with ChainDepthFromContext to stamp
	// this field on the created record's after_create event. A user-driven mutation leaves it 0.
	Depth int

	// Before is the record's committed field map as it stood BEFORE an update — nil on create, and
	// nil on delete (where Data already IS the before-image).
	//
	// Without it the condition language could only ask "is the record currently in this state",
	// never "did it just enter it". A builder wanting "when a quote becomes Accepted" had to write
	// "whenever a quote is saved AND status = Accepted", which re-fires on every later edit of that
	// quote — so the rule that should send one contract sends one per keystroke-save. The `changed`
	// / `changedTo` operators read this map; every other operator ignores it.
	Before map[string]any

	// CreatedAt is the record's creation instant (obj_*.created_at). It lives on the row, not in
	// Data, so every emit site must copy it from the Record it already holds — the set_elapsed
	// action and a set_deadline with anchor created_at (queue #81) measure from it. Zero means "not
	// known" (a best-effort read failed, or the delete path) and makes either action ERROR rather
	// than measure from year 1 or silently from now.
	CreatedAt time.Time

	// OccurredAt is when the event happened: stamped by AsyncDispatcher.Emit the moment the
	// committed mutation hands it over (a caller-set value is kept). The outbound_webhook body
	// carries it because queue retries back off per attempt, so an event parked during an outage
	// can reach the receiver AFTER a newer event of the same record — the receiver drops the older
	// one by this instant. The scheduler and the timer handler stamp it BEFORE reading the record
	// the event carries (never after: a later edit's Emit stamp must win over that snapshot). Zero
	// (dry-run preview) means "now" at marshal time.
	OccurredAt time.Time
}

// chainDepthKey is the private context key under which a create_record action carries the depth of
// the NEXT event into the nested RecordService.Create it invokes. It is read back by the emitting
// layer (services.emitCreateEvents) to stamp Event.Depth — the async worker discards ctx at the
// channel boundary, so this is the ONLY hop where depth travels via ctx.
type chainDepthKey struct{}

// WithChainDepth returns a ctx carrying the automation chain depth to stamp on the next emitted
// event. Called by runCreateRecord before invoking the nested create so the created record's
// after_create event inherits depth+1 (the recursion guard's counter).
func WithChainDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, chainDepthKey{}, depth)
}

// ChainDepthFromContext reads the automation chain depth a create_record action stashed for the
// event about to be emitted. Absent (a user-driven mutation, or any non-create_record path) → 0.
func ChainDepthFromContext(ctx context.Context) int {
	if v, ok := ctx.Value(chainDepthKey{}).(int); ok {
		return v
	}
	return 0
}

// OwnerSentinel is the recipient shorthand an in_app_notification action's userId may carry to
// mean "the record's owner" — offered by the FE WorkflowBuilder and the AI Builder persona.
// runInAppNotification resolves it to Event.OwnerUserID at fire time (never stored literally).
const OwnerSentinel = "$owner"

// RecordSentinel resolves to the id of the record that TRIGGERED the rule, so a created child can
// carry a relation back to its source. Without it every created record was an orphan.
const RecordSentinel = "$record"

// RecordFieldPrefix copies a TYPED value out of the triggering record: "$record.total" carries the
// currency object across intact, where the text renderer would flatten it to a string the write
// would then reject.
const RecordFieldPrefix = "$record."

// Evaluate reports whether the event's record data satisfies the condition. A nil or empty
// condition matches everything. Rules are joined by Match: "or" fires when ANY rule matches;
// "and" (the default — empty/"and") requires EVERY rule. An unknown operator or an uncomparable
// operand makes that rule false (fail-safe: a malformed rule never spuriously fires an
// automation). Evaluate is pure and side-effect free.
func Evaluate(cond *Condition, data map[string]any) bool {
	return EvaluateWithBefore(cond, data, nil)
}

// EvaluateWithBefore is Evaluate with the record's before-image available, so the edge operators
// (changed / changedTo) can compare against it.
//
// A nil before means "there is nothing to have changed from" — a create, a delete, or any caller
// that has no prior image — and every edge rule then evaluates FALSE. That is the conservative
// reading and the explainable one: a rule that says "when it becomes Accepted" should not fire on
// a record that was born Accepted, because nothing became anything. A builder who wants that case
// writes eq, which is what eq is for.
//
// Evaluate stays for the callers with no before-image, so this addition breaks nothing. bpm's
// approval criteria call this too since specs/043 S2b (bpm.Event carries Before on after_update).
func EvaluateWithBefore(cond *Condition, data, before map[string]any) bool {
	if cond == nil || len(cond.Rules) == 0 {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(cond.Match), "or") {
		for _, r := range cond.Rules {
			if evalRuleWithBefore(r, data, before) {
				return true
			}
		}
		return false
	}
	// AND (default): a single false rule fails the whole condition.
	for _, r := range cond.Rules {
		if !evalRuleWithBefore(r, data, before) {
			return false
		}
	}
	return true
}

// evalRule evaluates one predicate against the record data. The record value is looked up by the
// rule's field name; a missing field is treated as absent (nil) for null checks and as a failed
// comparison for the value operators.
func evalRule(r Rule, data map[string]any) bool {
	return evalRuleWithBefore(r, data, nil)
}

// evalRuleWithBefore is evalRule with access to the before-image for the edge operators.
func evalRuleWithBefore(r Rule, data, before map[string]any) bool {
	field := strings.TrimSpace(r.Field)
	recVal, present := data[field]
	// queue #286: a value of {"field": "<name>"} compares against another field of the same record.
	if ref, ok := RuleFieldRef(r.Value); ok {
		r.Value = data[ref]
		if r.Value == nil {
			return false // nothing to compare with; null ops carry no value so are unaffected
		}
	}

	switch strings.TrimSpace(r.Op) {
	case OpChanged, OpChangedTo:
		if before == nil {
			return false // nothing to have changed from
		}
		if looseEqual(before[field], recVal) {
			return false // this write did not touch the field
		}
		if strings.TrimSpace(r.Op) == OpChanged {
			return true
		}
		// changedTo additionally pins WHERE it landed.
		return looseEqual(recVal, r.Value)
	case OpIsNull:
		return !present || recVal == nil
	case OpNotNull:
		return present && recVal != nil
	case OpEq:
		return looseEqual(recVal, r.Value)
	case OpNeq:
		return !looseEqual(recVal, r.Value)
	case OpGt, OpGte, OpLt, OpLte:
		return compareNumeric(r.Op, recVal, r.Value)
	case OpContains:
		return strings.Contains(toString(recVal), toString(r.Value))
	default:
		// Unknown operator → fail-safe false (should be unreachable after config validation).
		return false
	}
}

// RuleFieldRef reports whether a rule value is a reference to another field of the record —
// exactly {"field": "<name>"} — and returns that field name. Anything else is a constant.
func RuleFieldRef(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return "", false
	}
	name, ok := m["field"].(string)
	name = strings.TrimSpace(name)
	return name, ok && name != ""
}

// looseEqual compares two operands with type-tolerant equality. JSON decoding yields float64 for
// numbers and string for text; an admin may store the condition value as the "wrong" JSON type
// (e.g. "5" vs 5), so equality first tries a numeric comparison (both parse as numbers), then
// falls back to a string comparison. Booleans compare by their string form too. This mirrors the
// record-list eq/neq symmetric-cast intent (B6 validation-parity: don't silently drop a mistyped
// operand — compare it sensibly).
func looseEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af == bf
		}
	}
	return toString(a) == toString(b)
}

// compareNumeric applies an ordered operator to two operands coerced to float64. If either side
// is not numeric the comparison is false (ordered comparison is only meaningful on numbers).
func compareNumeric(op string, a, b any) bool {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if !aok || !bok {
		return false
	}
	switch op {
	case OpGt:
		return af > bf
	case OpGte:
		return af >= bf
	case OpLt:
		return af < bf
	case OpLte:
		return af <= bf
	default:
		return false
	}
}

// toFloat coerces a JSON-decoded scalar (float64, int, json.Number-as-string, or numeric string)
// to a float64, reporting ok=false for non-numeric values.
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case map[string]any:
		// A currency field is stored as {amount, currencyCode}. "Over 50 million" compares the AMOUNT:
		// without this every ordered comparison on a money field was silently false, so a rule or an
		// approval branch on a price could never hold (found with specs/043 S7's gateway).
		if amt, ok := t["amount"]; ok {
			return toFloat(amt)
		}
		return 0, false
	default:
		return 0, false
	}
}

// toString renders a scalar to its canonical string form for equality/contains comparisons.
// Numbers use the shortest round-trippable form so 5 and 5.0 render identically ("5").
func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
}

// Engine is the consumer-side interface the service layer depends on. Emit is non-blocking by
// contract: it enqueues the event and returns immediately, so a slow/down database or a slow
// action can never stall a record mutation. Implementations must be safe for concurrent use.
type Engine interface {
	// Emit schedules a record event for asynchronous workflow processing and returns at once.
	// It never blocks on I/O and never returns an error: load/condition/action problems are
	// handled out-of-band (logged) by the implementation (fail-open).
	Emit(ctx context.Context, evt Event)
}

// noopEngine is the default Engine wired into services so legacy flows and tests run unchanged
// when automation is disabled. It discards every event.
type noopEngine struct{}

// NewNoopEngine returns an Engine that drops every event. Safe default for services/tests.
func NewNoopEngine() Engine { return noopEngine{} }

// Emit discards the event. It deliberately does nothing.
func (noopEngine) Emit(context.Context, Event) {}
