// Package bpm is the EPIC 5 M1 BPM / Approval / SLA engine (spec:
// docs/ENGINE-COVERAGE-SPEC.md §EPIC 5; benchmark: Salesforce Approval Processes + ServiceNow /
// SugarCRM approval + SLA engines). It adds a HUMAN gate on top of the metadata-driven database:
// an admin registers an approval PROCESS ("khi <trigger> trên <entity>, NẾU <criteria> THÌ chờ
// duyệt tuần tự theo <steps>"), and when a committed record matches the criteria the engine spins
// up an approval INSTANCE that blocks until every step is approved (sequential), or short-circuits
// on the first rejection.
//
// It sits below the service layer exactly like internal/workflow and internal/audit: RecordService
// depends only on the consumer-side Engine interface and calls Emit AFTER a record mutation
// commits. The concrete AsyncEngine drains a buffered channel from a pool of workers, loads the
// active processes of the mutated entity, evaluates the criteria, and arms an instance on a match.
//
// Invariants (rules + spec):
//   - ASYNC + FAIL-OPEN on the arming path: a full buffer, a load error, or an insert error NEVER
//     blocks, slows, or breaks the originating mutation (it has already committed) — same contract
//     as the EPIC 4 workflow dispatcher. The decision path (approve/reject) is SYNCHRONOUS (a user
//     action returning a status) and DOES surface errors to the caller.
//   - Multi-tenant: every Process/Instance carries TenantID and every repository query filters
//     tenant_id — a process can never be loaded, nor an instance decided, across tenants.
//   - Metadata-driven: processes are rows in sys_approval_processes, never hardcoded Go structs.
//   - Criteria reuse the EPIC 4 condition vocabulary (workflow.Condition + workflow.Evaluate) so an
//     admin reuses one familiar filter language across automation and approval.
package bpm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"nexus/platform/internal/workflow"
)

// Trigger is the after-commit record event that ARMS an approval process. Approvals only make
// sense on a record's creation or modification, so the BPM trigger set is a deliberate subset of
// the workflow triggers (no after_delete: a deleted record cannot be approved; no scheduled: an
// approval is record-driven, while the SLA scheduler is a separate breach scan).
type Trigger string

const (
	TriggerAfterCreate Trigger = "after_create"
	TriggerAfterUpdate Trigger = "after_update"
	// TriggerManual: the process is NEVER armed by a record event. It arms only when a person
	// explicitly asks for approval (POST /:entity/:id/approvals/submit).
	//
	// This is the difference between a rule and a request. after_create/after_update mean the
	// system decides on your behalf the moment you save — fine for "any discount over 30% needs
	// sign-off", useless for "I am finished with this quote, please review it", which is what most
	// real Vietnamese back-office work actually is. Without it the submitter has no verb at all:
	// they cannot ask, so they also cannot un-ask.
	TriggerManual Trigger = "manual"
)

// ValidTrigger reports whether t is a supported approval trigger.
func ValidTrigger(t Trigger) bool {
	switch t {
	case TriggerAfterCreate, TriggerAfterUpdate, TriggerManual:
		return true
	default:
		return false
	}
}

// ApproverType is how a step names its approver: a specific user, or anyone holding a role.
type ApproverType string

const (
	ApproverUser ApproverType = "user"
	ApproverRole ApproverType = "role"
	// ApproverQueue names a work queue (sys_work_queues.id, specs/012): the step belongs to whoever
	// the queue is visible to (its role, or everyone when it names none) and is claimed from a pool
	// (specs/043 S5). A graph node may name one; the linear v1 step and the instance projection
	// (ENUM user|role, D-5) never do.
	ApproverQueue ApproverType = "queue"
	// ApproverTeam names a team (teams.id): a SEAT holder only — the whole team is a pool, any member
	// may claim it (specs/043 S6a). Like a queue it never appears in the linear v1 step, in a
	// node's approverType or in the instance projection; a node reaches it through `assignment`.
	ApproverTeam ApproverType = "team"
)

// ValidApproverType reports whether at is a supported approver kind of the LINEAR step and of the
// current_approver_* projection (an ENUM of user|role).
func ValidApproverType(at ApproverType) bool {
	switch at {
	case ApproverUser, ApproverRole:
		return true
	default:
		return false
	}
}

// ValidAssigneeType reports whether at can own a graph node's seat: a user, a role, or a work queue.
func ValidAssigneeType(at ApproverType) bool {
	return ValidApproverType(at) || at == ApproverQueue
}

// InstanceStatus is the lifecycle state of an approval instance.
type InstanceStatus string

const (
	// StatusPending: the instance is awaiting a decision on its current step.
	StatusPending InstanceStatus = "pending"
	// StatusApproved: every step cleared — terminal.
	StatusApproved InstanceStatus = "approved"
	// StatusRejected: a step was rejected — terminal (no loop-back in M1).
	StatusRejected InstanceStatus = "rejected"
	// StatusCancelled: the process was removed / instance recalled — terminal.
	StatusCancelled InstanceStatus = "cancelled"
)

// History action kinds appended to an instance's audit log.
const (
	HistoryApproved  = "approved"
	HistoryRejected  = "rejected"
	HistoryEscalated = "escalated"
	// HistorySubmitted: a person asked for this approval (TriggerManual). Recorded as step 0 so
	// the timeline opens with "who sent it", not with the first decision.
	HistorySubmitted = "submitted"
	// HistoryRecalled: the submitter withdrew the request while it was still pending.
	HistoryRecalled = "recalled"
	// HistoryDelegated: the current approver handed THIS step to someone else. The step does not
	// advance — the same decision is still owed, by a different person.
	HistoryDelegated = "delegated"
	// HistoryCompleted: the holder of a task step (not an approval) finished it (specs/043 S5).
	HistoryCompleted = "completed"
	// HistoryResubmitted: the submitter fixed a returned record and sent it again (specs/043 S8);
	// the approval resumes at the rejector's step, or at the start (onReject.resumeAt).
	HistoryResubmitted = "resubmitted"
	// HistoryRepinned: an admin moved this running approval onto another version of its process
	// (specs/043 S11). The step is the same one (mapped by its stable id); the version changed.
	HistoryRepinned = "repinned"
	// HistoryPaused / HistoryResumed: the SLA clock was frozen because the record met the process's
	// pauseWhen (e.g. the ticket waits for the customer), and set going again when it stopped
	// meeting it (specs/043 S13). Seats stay actionable throughout.
	HistoryPaused  = "paused"
	HistoryResumed = "resumed"
	// The machine steps (specs/043 S7). Comment says what happened: the branch a gateway took, the
	// fields an auto node wrote, how an end node settled the instance, or why a step failed.
	HistoryGateway = "gateway"
	HistoryAuto    = "auto"
	HistoryEnded   = "ended"
	// HistoryEffectFailed: something that should have happened after a step did not (an auto node's
	// write, the record write-back) — recorded here, not only in the log, so the Nexus team sees it
	// (FR-010). On an auto/gateway step it is also why the instance stands still (last_error).
	HistoryEffectFailed = "effect_failed"
	// HistoryRetried: an admin pressed "Thử lại" on a stuck instance.
	HistoryRetried = "retried"
	// The wait node (specs/043 S12a). HistoryWaiting marks the instance resting on the node (the
	// deadline it was stamped with is on the instance); HistoryWaitMet says the record answered —
	// the customer face shows it ("Phản hồi đầu tiên — đạt"), and it closes the node's stretch in the
	// durations; HistoryBreachNoRecipient says a missed deadline found nobody to tell.
	HistoryWaiting = "waiting"
	// HistoryWaitTightened: a tighten_only wait deadline moved earlier (S14b).
	HistoryWaitTightened     = "wait_tightened"
	HistoryWaitMet           = "wait_met"
	HistoryBreachNoRecipient = "breach_no_recipient"
)

// InternalHistoryAction reports whether a history line is the machine's own bookkeeping (specs/043
// S7): a gateway's branch, an automatic write, an end node's settling, a failed effect, an admin's
// retry. Their comments name fields, branches and refused values — engine vocabulary the customer
// face never shows (the outcome is the hồ sơ's status, which it does show).
func InternalHistoryAction(a string) bool {
	switch a {
	case HistoryGateway, HistoryAuto, HistoryEnded, HistoryEffectFailed, HistoryRetried, HistoryWaiting, HistoryBreachNoRecipient:
		return true
	}
	return false
}

// PublicHistory is the history a person who is not an admin may read: the lines without the machine
// bookkeeping above. The slice is returned as it is when it holds none (the common case); durations
// are measured on the FULL history before this is applied.
func PublicHistory(h []HistoryEntry) []HistoryEntry {
	n := 0
	for _, e := range h {
		if InternalHistoryAction(e.Action) {
			n++
		}
	}
	if n == 0 {
		return h
	}
	out := make([]HistoryEntry, 0, len(h)-n)
	for _, e := range h {
		if !InternalHistoryAction(e.Action) {
			out = append(out, e)
		}
	}
	return out
}

// InstanceContext is the run-time variable bag of an instance (sys_approval_instances.context).
// Today it holds the branches taken: {"gateway": {"<nodeId>": "<branch name>"}}; later slices add
// sla/rework/vars to the same object, so unknown keys are carried through untouched.
type InstanceContext map[string]json.RawMessage

// Gateway returns the branch each gateway the instance has passed took (node id → branch name).
func (c InstanceContext) Gateway() map[string]string {
	out := map[string]string{}
	if raw, ok := c["gateway"]; ok {
		_ = json.Unmarshal(raw, &out) // a malformed value reads as "no branches yet"; never blocks a read
	}
	return out
}

// WithGateway returns a copy of the context that records branch as the arm taken at node.
func (c InstanceContext) WithGateway(node, branch string) (InstanceContext, error) {
	out := make(InstanceContext, len(c)+1)
	for k, v := range c {
		out[k] = v
	}
	taken := c.Gateway()
	taken[node] = branch
	raw, err := json.Marshal(taken)
	if err != nil {
		return nil, fmt.Errorf("bpm.InstanceContext.WithGateway: %w", err)
	}
	out["gateway"] = raw
	return out, nil
}

// writtenDue is the deadline the engine last wrote into the record's due field ({"sla":{"writtenDue"}}),
// nil when it never wrote one.
func (c InstanceContext) writtenDue() *time.Time {
	var v struct {
		WrittenDue time.Time `json:"writtenDue"`
	}
	if raw, ok := c["sla"]; ok && json.Unmarshal(raw, &v) == nil && !v.WrittenDue.IsZero() {
		return &v.WrittenDue
	}
	return nil
}

// withWrittenDue returns the context (marshalled) recording t as the deadline written to the record.
func (c InstanceContext) withWrittenDue(t time.Time) ([]byte, error) {
	out := make(InstanceContext, len(c)+1)
	for k, v := range c {
		out[k] = v
	}
	raw, err := json.Marshal(map[string]any{"writtenDue": t.UTC()})
	if err != nil {
		return nil, fmt.Errorf("bpm.InstanceContext.withWrittenDue: %w", err)
	}
	out["sla"] = raw
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("bpm.InstanceContext.withWrittenDue: %w", err)
	}
	return b, nil
}

// Step is one approval step in a process definition. The engine walks the steps array in order;
// step i+1 only opens once step i is approved (sequential). SLASeconds is the per-step deadline in
// seconds (0 = no SLA on this step); when set, an instance arriving at this step gets sla_due_at =
// now + SLASeconds and the SLA scheduler escalates if it is still pending past that deadline.
//
// ID is the step's STABLE id (specs/043 S0): assigned once — positionally for a legacy step, see
// NormalizeSteps — and kept for good, so an instance, a history entry or a later version map can
// name "the director's step" without that meaning "whatever step now sits at index 1".
type Step struct {
	ID           string       `json:"id,omitempty"`
	Name         string       `json:"name,omitempty"`
	ApproverType ApproverType `json:"approverType,omitempty"`
	ApproverID   string       `json:"approverId,omitempty"`
	SLASeconds   int          `json:"slaSeconds,omitempty"`
	SLAClock     string       `json:"slaClock,omitempty"`
	// NodeKind is what the chain walks at this step: "" / NodeApproval = a decision, NodeTask = a
	// piece of work to complete. Set by Graph.approvalChain from the node; a linear step is always
	// an approval. Never serialized (the graph node's own "kind" is the stored one). A string, so a
	// Step stays comparable.
	NodeKind NodeKind `json:"-"`
	// Approvers + Completion make the step PARALLEL (specs/043 S4): several people each hold a
	// seat of the same visit and Completion says when the step is over. Graph form only — the
	// API converts a linear step that carries them (GraphFromSteps), and a step names its approver
	// EITHER by approverType/approverId OR by Approvers, never both (CheckApprovers).
	Approvers  []Approver  `json:"approvers,omitempty"`
	Completion *Completion `json:"completion,omitempty"`
	// Assignment names the holder by RULE instead of by name (specs/043 S6a): a team pool, a
	// work queue, the person in a record field, the submitter. Graph form only, and instead of
	// approverType/approverId and approvers (CheckApprovers). The rule is resolved once, when the
	// instance enters the step, and the seat it produced is frozen (FR-017).
	Assignment *Assignment `json:"assignment,omitempty"`
}

// IsTask reports whether the step is a task (completed, not approved or rejected).
func (s Step) IsTask() bool { return s.NodeKind == NodeTask }

// IsMachine reports whether the step is a node the engine runs by itself (gateway, auto, end —
// specs/043 S7): no seat, no deadline, nobody to notify.
func (s Step) IsMachine() bool { return MachineKind(s.NodeKind) }

// Process is a loaded approval definition (one sys_approval_processes row). Criteria is the
// entry-criteria (nil/empty matches every event, reusing workflow.Condition + workflow.Evaluate).
// Steps run sequentially. Only IsActive, non-deleted rows are armed by the engine.
type Process struct {
	ID         string
	TenantID   string
	Name       string
	EntityName string
	Trigger    Trigger
	Criteria   *workflow.Condition
	Steps      []Step
	// Graph is the graph form of the definition (specs/043 D-2; steps is then empty). The engine
	// walks Chain(), which reads the graph when there is one.
	Graph *Graph
	// Outcome is what the process WRITES when it reaches a terminal state. nil = the historical
	// behaviour: the instance ends and nothing about the record changes.
	Outcome *Outcome
	// Options are the process-level settings (specs/043 S6a: the fallback assignee). nil = none.
	Options  *Options
	IsActive bool
	// Version is the published version this row stands for (specs/043 S10, 1-based; the row is the
	// denormalised copy of the active version). 0 only in a value built by hand.
	Version int
}

// Outcome is the field write-back a terminal decision performs on the record being approved.
//
// Without it an approval was a note in an inbox: the quote everyone signed off stayed in whatever
// status it was in, and nothing downstream could react, because StatusApproved had no consumer
// anywhere outside this package. "Approved" has to mean something to the record, or the workflow
// is theatre.
//
// Either value may be empty — plenty of processes only care about one direction ("approved →
// Chấp nhận", rejection handled by a human). An empty value for the direction that occurred simply
// writes nothing.
type Outcome struct {
	// StatusField is the field on the TARGET record to write.
	StatusField string `json:"statusField"`
	// ApprovedValue is written when the last step is approved.
	ApprovedValue string `json:"approvedValue,omitempty"`
	// RejectedValue is written when any step rejects.
	RejectedValue string `json:"rejectedValue,omitempty"`
	// ReturnedValue is written when a rejection sends the record back to its submitter to fix
	// (onReject.to = @submitter, specs/043 S8) — "Cần bổ sung". The approval is still pending then,
	// so this is not a terminal write: ValueFor never answers it.
	ReturnedValue string `json:"returnedValue,omitempty"`
}

// ReturnedValueFor returns the value to write when a rejection hands the record back to its
// submitter, and whether there is one to write.
func (o *Outcome) ReturnedValueFor() (string, bool) {
	if o == nil || strings.TrimSpace(o.StatusField) == "" {
		return "", false
	}
	return o.ReturnedValue, strings.TrimSpace(o.ReturnedValue) != ""
}

// ValueFor returns the value to write for a terminal status, and whether there is one to write.
func (o *Outcome) ValueFor(st InstanceStatus) (string, bool) {
	if o == nil || strings.TrimSpace(o.StatusField) == "" {
		return "", false
	}
	switch st {
	case StatusApproved:
		return o.ApprovedValue, strings.TrimSpace(o.ApprovedValue) != ""
	case StatusRejected:
		return o.RejectedValue, strings.TrimSpace(o.RejectedValue) != ""
	default:
		return "", false
	}
}

// Definition is the slice of a process an instance EXECUTES — its steps and its terminal
// write-back — pinned onto the instance when it is armed (migration 000065,
// sys_approval_instances.process_snapshot). An instance finishes the process it started under;
// an edit to the process applies to instances armed afterwards.
//
// Why pin: the decision path used to index the LIVE steps array by the instance's own
// current_step, so an admin shortening a 3-step process to 2 while a record waited at step 2
// made that approval satisfy `next >= len(steps)` and terminate as 'approved' — one step skipped,
// the outcome written onto the record, no warning anywhere. During a handover, when processes
// are still being tuned, that is the normal case, not the edge case.
//
// What is pinned: steps + outcome (what the engine executes). Name/entity/trigger/isActive stay
// live — display context, not behaviour. The pin also freezes references that can later vanish:
// a step's approver role may be deleted, the outcome's statusField renamed. Neither corrupts
// anything: CanDecide simply never matches a deleted role (an admin can still decide or delegate
// that step — the same as on a live process), and applyOutcome already degrades fail-open on a
// refused write (logged loudly, the decision stands).
//
// nil = the instance was armed before the snapshot column existed; the Decider then reads the
// live process, exactly as before.
type Definition struct {
	// Steps is the linear form; [] (never omitted) beside a Graph, so a binary older than S3a2
	// finds no step and refuses the instance instead of misreading the graph (D-2).
	Steps []Step `json:"steps"`
	// Graph is the graph form (specs/043 S3a2); the engine walks Chain(), graph first.
	Graph   *Graph   `json:"graph,omitempty"`
	Outcome *Outcome `json:"outcome,omitempty"`
	// Options are the process options pinned with the steps (S6a): an instance keeps the fallback
	// assignee it started under, however the live process is edited later.
	Options       *Options `json:"options,omitempty"`
	FirstDueBasis SLABasis `json:"firstDueBasis,omitempty"`
	// Version is the published version of the process this definition was pinned from (specs/043
	// S10). 0 = pinned before versions existed (or backfilled): unknown, never guessed.
	Version int `json:"version,omitempty"`
}

// hasStatefulRouting reports whether some step deals its holder out with a cursor (round_robin /
// load_based, S6b): the one kind of routing whose lookup must not be repeated "just in case".
func (d *Definition) hasStatefulRouting() bool {
	for _, s := range d.Chain() {
		if s.Assignment.Stateful() {
			return true
		}
	}
	return false
}

// version is the pinned process version, 0 for a nil (pre-000065) or pre-S10 definition.
func (d *Definition) version() int {
	if d == nil {
		return 0
	}
	return d.Version
}

// HistoryEntry is one decision appended to an instance's history log.
type HistoryEntry struct {
	Step    int    `json:"step"`
	Action  string `json:"action"` // one of the History* constants
	Actor   string `json:"actor"`  // user id who made the decision ("" for system escalation)
	Comment string `json:"comment,omitempty"`
	// Target is the user id a delegation handed the step TO. Structured rather than glued into
	// Comment: the first cut wrote "→ 094da8b4-…" into the human note, and the timeline then
	// showed a raw uuid to the reader — the engine's vocabulary leaking onto the customer face,
	// in the exact place someone goes to ask who has this now. A separate field lets the UI
	// resolve a name and lets Comment stay what a person actually typed.
	Target string `json:"target,omitempty"`
	// Node + StepName name the step this entry happened on by its stable id and its display
	// name at the time (specs/043 S0). Step (the index) stays for every reader that has only
	// ever known it; an entry written before S0 has neither key, and the timeline falls back to
	// the index exactly as before.
	Node     string   `json:"node,omitempty"`
	StepName string   `json:"stepName,omitempty"`
	SLABasis SLABasis `json:"slaBasis,omitempty"`
	// Partial marks one vote of a parallel step that did NOT finish the step (specs/043 S4): the
	// step is still open, so it neither closes a stretch of the timeline nor sets a deadline.
	Partial bool `json:"partial,omitempty"`
	// Via says the decision was not an ordinary seat holder's: "override" = an admin decided the
	// step in place of its seats, with the note as the reason (FR-023).
	Via string `json:"via,omitempty"`
	// Version is the process version the instance runs under when this entry was written (specs/043
	// S10, #70). Absent on an entry written before S10 or on an instance pinned before versions.
	Version int `json:"version,omitempty"`
	// ReturnedTo (a rejected entry, S8) says the rejection did not end the approval: the node id it
	// went back to, or "@submitter" (the record is with its submitter, whose user id is in Target).
	// On a resubmitted entry it is the node id the approval resumed at.
	ReturnedTo string `json:"returnedTo,omitempty"`
	// FromVersion is the version a repin moved the instance OFF (HistoryRepinned only).
	FromVersion int       `json:"fromVersion,omitempty"`
	At          time.Time `json:"at"`
}

// Instance is a running (or terminal) approval (one sys_approval_instances row). CurrentStep is
// the 0-based index of the step now awaiting a decision (meaningless once status is terminal).
// CurrentApproverType/ID are denormalised from the active step so the "pending for me" query is a
// cheap indexed lookup. SLADueAt is the absolute UTC deadline of the current step (nil = no SLA).
type Instance struct {
	ID                  string
	TenantID            string
	ProcessID           string
	EntityName          string
	RecordID            string
	CurrentStep         int
	Status              InstanceStatus
	CurrentApproverType ApproverType
	CurrentApproverID   string
	SLADueAt            *time.Time
	// SLAPausedAt / SLARemainingSeconds: the clock is frozen (S13) — SLADueAt is nil meanwhile and
	// the remaining working time waits here. Both nil = running (or no SLA).
	SLAPausedAt         *time.Time
	SLARemainingSeconds *int64
	Escalated           bool
	History             []HistoryEntry
	// Context holds the branches the instance has taken (S7); nil on an instance that never passed
	// a gateway. Read through the detail (`gateway`), not serialized here.
	Context InstanceContext `json:"-"`
	// Seq counts the state writes of the instance (arm = 1, every decision / delegation / recall /
	// escalation / history append +1). It is the CAS key of every write (specs/043 D-4): a caller
	// presents the Seq it READ, so a write that "looks the same" — same step, same escalated flag,
	// a different holder — still loses. Internal: not serialized.
	Seq int64 `json:"-"`
	// LastError is why the instance is stuck on its step ("" = not stuck): a step whose holder could
	// not be found (specs/043 S6a, FR-018). The instance stays pending — nobody holds the step —
	// and the Nexus team reads this from the admin instance list. It is also why the instance stands
	// still on a MACHINE step (specs/043 S7): an auto node's write the record refused, a branch it
	// could not read; an admin fixes the cause and presses "Thử lại".
	// Not serialized: the lists serialize Instance verbatim to everyone who can see the record, and
	// the reason is engine vocabulary (field names, refused values). It reaches an admin through the
	// detail and the per-process list, never through here.
	LastError string `json:"-"`
	// Definition is the pinned steps + outcome this instance runs under (nil = armed before
	// migration 000065 → the live process is read instead). Not serialized: the list endpoints
	// serialize Instance verbatim (PascalCase, documented deviation) and the timeline's step
	// context has one documented home, GET /approvals/:id `process.steps`.
	Definition *Definition `json:"-"`
	// InRework: set by ListPendingForUser when the caller's row stands on a rework seat (the record was
	// returned to its submitter, S8), so the inbox can hide Duyệt / Từ chối. Not stored, not serialized here.
	InRework bool `json:"-"`
	// ProcessVersion is the version of the process this instance runs (specs/043 S10) — read off its
	// pin; 0 = unknown (armed before versions existed).
	ProcessVersion int
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// matchesCriteria reports whether a record's data satisfies a process's entry-criteria. A nil/empty
// criteria matches every event (reuses the EPIC 4 evaluator so the filter language is identical).
// before is the record's before-image (nil on create / manual send): the edge operators
// changed/changedTo read it, and with nil they are false — the validator only lets them onto an
// after_update process, whose event carries it (specs/043 S2b).
func matchesCriteria(p Process, data, before map[string]any) bool {
	return workflow.EvaluateWithBefore(p.Criteria, data, before)
}

// nodeIDPattern is the shape of a step's stable id. Lowercase ASCII only: the id is a machine key,
// never a label (the step's Name is what people read). 60 characters keeps the round-robin cursor
// key "bpm:<id>" within sys_assignment_cursors.action_key VARCHAR(64).
var nodeIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,60}$`)

// ValidNodeID reports whether id is an acceptable step id (^[a-z0-9_-]{1,60}$).
func ValidNodeID(id string) bool { return nodeIDPattern.MatchString(id) }

// NormalizeSteps returns a copy of steps in which every step has an ID. An ID that is already
// there is NEVER changed — that is the whole point: reorder, insert or delete steps and each
// surviving step keeps the id a pinned instance, a history entry or a version map already refers
// to. A missing ID gets its positional id s{i+1}; when that one is taken by another step (a step
// inserted in front of an id'd one), the next free s{n} instead, so ids never collide.
//
// For a legacy array (no ids at all) this is exactly s1..sN — the same answer Migration A's
// backfill computes in SQL (COALESCE(steps[i].id, CONCAT('s', i+1))), so every reader agrees on
// which step a pre-S0 instance is standing on. The input is not mutated (it may be a pinned
// definition); nil stays nil.
func NormalizeSteps(steps []Step) []Step { return NormalizeStepsAvoiding(steps, nil) }

// NormalizeStepsAvoiding is NormalizeSteps that also never hands out an id in reserved (specs/043
// S10, "Nợ còn mở" 15): the ids a process has published in some version. A step id means "the same
// step" across versions (S11 maps a pending instance by it), so an id released by a deleted step
// must not come back as a DIFFERENT step. An id the caller sends explicitly is never touched —
// reserved only steers the ones this function assigns.
func NormalizeStepsAvoiding(steps []Step, reserved map[string]bool) []Step {
	if steps == nil {
		return nil
	}
	out := make([]Step, len(steps))
	copy(out, steps)
	used := make(map[string]bool, len(out)+len(reserved))
	for id := range reserved {
		used[id] = true
	}
	for _, s := range out {
		if s.ID != "" {
			used[s.ID] = true
		}
	}
	for i := range out {
		if out[i].ID != "" {
			continue
		}
		n := i + 1
		for used["s"+strconv.Itoa(n)] {
			n++
		}
		out[i].ID = "s" + strconv.Itoa(n)
		used[out[i].ID] = true
	}
	return out
}

// StepRef returns the stable id and display name of step i of the definition — the pair every
// new HistoryEntry carries. A nil definition (armed before migration 000065) or an index outside
// the steps yields "", "": the timeline then falls back to the step index, as it always has.
func (d *Definition) StepRef(i int) (node, name string) {
	chain := d.Chain()
	if i < 0 || i >= len(chain) {
		return "", ""
	}
	s := NormalizeSteps(chain)[i]
	return s.ID, s.Name
}

// ----------------------------------------------------------------------------------------------
// Work seats (sys_bpm_tasks, migration bpm_runtime_v2 — specs/043 S3a).
// ----------------------------------------------------------------------------------------------

// SeatOutcome is how a seat ended. It also decides the seat's status: a seat its holder acted on
// (approved / rejected / reassigned) is "done" and keeps who acted and their note; a seat nobody
// acted on (recalled) is "cancelled".
type SeatOutcome string

const (
	SeatApproved   SeatOutcome = "approved"
	SeatRejected   SeatOutcome = "rejected"
	SeatRecalled   SeatOutcome = "recalled"
	SeatReassigned SeatOutcome = "reassigned"
	// SeatCompleted: the holder finished a task seat (specs/043 S5).
	SeatCompleted SeatOutcome = "completed"
	// SeatSuperseded: a seat left open on a step the instance is no longer on — a pre-S3a binary
	// (rolling deploy, or a binary rollback on this schema) moved the instance without touching
	// seats. The next write of this binary closes it, cancelled, decided by nobody.
	SeatSuperseded SeatOutcome = "superseded"
	// SeatRepinned: the instance moved to another version of its process (S11); its seat is
	// rebuilt from the new definition. Nobody acted on the old one — it ends cancelled.
	SeatRepinned SeatOutcome = "repinned"
)

// Seat is one open work seat: WHO holds the step an instance stands on, on which visit, until when.
//
// S3a writes seats beside the instance (DUAL WRITE) and reads none: authority is still the
// instance's CAS on current_step + escalated, and the inbox still reads current_approver_*. The
// point of writing them now is that the day the reads move over (S3a2/S3b), every instance a
// binary of this slice touched already has the right seat.
type Seat struct {
	NodeID       string
	AssigneeType ApproverType
	AssigneeID   string
	// Kind is what the seat asks of its holder (SeatKindApproval when empty).
	Kind string
	// ClaimedBy is the user who took this pool seat (role / queue) out of the pool (specs/043 S5);
	// "" = unclaimed. A claimed pool seat is held by its claimer ALONE until released — see heldBy.
	ClaimedBy string
	// QueueLive / QueueRoleID describe the work queue a queue seat names, read with the seat: the
	// queue still exists and is active, and the role it is visible to ("" = every user). Zero on a
	// seat that is not a queue seat.
	QueueLive   bool
	QueueRoleID string
	DueAt       *time.Time
	DueBasis    SLABasis
	// DelegatedFrom is the user who handed this seat on (a delegation); "" otherwise.
	DelegatedFrom string
	// Note rides on the seat when it OPENS: the reason a rejection returned the record (a rework
	// seat, S8). Stored in the seat's comment column until its holder acts on it.
	Note string
	// BreachNotifiedAt mirrors the instance's escalated flag onto the seat: non-nil iff the
	// breach of this visit has already been actioned.
	BreachNotifiedAt *time.Time
}

// SeatWrite is the seat side of one instance transition, applied in the SAME transaction as the
// guarded instance UPDATE and only when that UPDATE wins. The zero value touches no seat.
type SeatWrite struct {
	// Close ends the OPEN seats of the instance with this outcome ("" = close none).
	Close SeatOutcome
	// CloseSeat narrows Close to the one seat with this id (a delegation on a step with several
	// seats hands on the delegator's own and no other). "" = every open seat takes Close.
	CloseSeat string
	// Node is the step the transition acts on. Open seats on any OTHER node are stale (see
	// SeatSuperseded) and close as superseded instead of taking the actor's outcome. "" = every
	// open seat takes Close (a recall withdraws them all alike).
	Node string
	// By is the actor of the transition: decided_by of a seat it acted on, created_by of the
	// seats it opens.
	By string
	// Comment is the actor's note, kept on the seat they acted on.
	Comment string
	// Open are the seats the transition opens: the step the instance now stands on.
	Open []Seat
	// SameVisit: the opened seats CONTINUE the visit of the seats being closed (a delegation hands
	// a seat on; it does not enter the step again) and take their entry_seq. Otherwise they enter
	// a new visit, numbered by the seq this write produced. A visit is what votes are counted
	// over (S3b/S4), so a handed-on seat must never land in a visit of its own.
	SameVisit bool
}

// acted reports whether the write closes seats because someone ACTED on them (decided, handed on)
// rather than withdrew them (recall, superseded) — the writes that need a seat to close.
func (w SeatWrite) acted() bool {
	switch w.Close {
	case SeatApproved, SeatRejected, SeatReassigned, SeatCompleted:
		return true
	}
	return false
}

// stepSeats returns the seats a step opens on a visit that is due at due (computed on basis):
// one for a plain step — its approver, on the step's stable id — one per approver for a parallel
// one (S4).
// A step with no approver opens none: like Migration A's backfill, the instance is then left to
// the read-side fallback (D-6) rather than given a seat nobody holds.
func stepSeats(s Step, due *time.Time, basis SLABasis) []Seat {
	kind := SeatKindApproval
	if s.IsTask() {
		kind = SeatKindTask
	}
	if s.Assignment != nil { // a rule naming its holder outright; a dynamic one opens none here (routeSeats resolves it)
		ap, ok := s.Assignment.static()
		if !ok {
			return nil
		}
		return []Seat{{NodeID: s.ID, AssigneeType: ap.Type, AssigneeID: ap.ID, Kind: kind, DueAt: due, DueBasis: basis}}
	}
	if s.Parallel() { // S4: one seat per approver, all on the same visit and the same deadline
		out := make([]Seat, len(s.Approvers))
		for i, a := range s.Approvers {
			out[i] = Seat{NodeID: s.ID, AssigneeType: a.Type, AssigneeID: a.ID, DueAt: due, DueBasis: basis}
		}
		return out
	}
	if s.ApproverType == "" || s.ApproverID == "" {
		return nil
	}
	return []Seat{{NodeID: s.ID, AssigneeType: s.ApproverType, AssigneeID: s.ApproverID, Kind: kind, DueAt: due, DueBasis: basis}}
}

// Seat kinds (sys_bpm_tasks.kind): what the seat asks of its holder. Approval seats are decided
// through /approvals/:id/approve|reject; task and rework seats are completed through
// /work-items/:id/complete.
const (
	SeatKindApproval = "approval"
	SeatKindTask     = "task"
	// SeatKindRework: the submitter's "fix and send again" seat of a returned approval (S8 opens it;
	// completing it is the same verb as completing a task).
	SeatKindRework = "rework"
)

// projection is what the instance's current_approver_* columns say about a set of open seats
// (D-5). They are ENUM('user','role') and the inbox of today reads them, so they name the holder
// only when there is exactly ONE seat and it is a user or a role; anything else — a team pool, a
// queue, a parallel step — is NULL. Writing 'team' into that ENUM is error 1265, and on the
// fail-open arming path that error is a log line and a record that never enters approval.
func projection(seats []Seat) (ApproverType, string) {
	if len(seats) != 1 || !ValidApproverType(seats[0].AssigneeType) {
		return "", ""
	}
	return seats[0].AssigneeType, seats[0].AssigneeID
}

// heldBy reports whether the actor holds this seat: a user seat naming them, or a pool seat
// (role / queue) they are a candidate of that nobody else has claimed. A claimed pool seat is its
// claimer's alone (specs/043 S5) — for as long as the claimer is still a candidate: a claimer who
// has lost the role stops holding it (an admin releases it), the same fail-closed reading the
// inbox query makes. Blank ids never match (a seat always names someone).
func (s Seat) heldBy(userID string, roleIDs []string) bool {
	if s.ClaimedBy != "" && s.Claimable() && s.ClaimedBy != userID {
		return false
	}
	return s.candidate(userID, roleIDs)
}

// candidate reports whether the actor would hold this seat were nobody to have claimed it: the user
// it names, a holder of the role it names, a member of the team it names, or — for a queue seat — a
// user the live queue is visible to. Claiming is only open to a candidate, and a candidate is who
// learns "someone else has it". roleIDs is the actor's HOLDER ids (Actor.HolderIDs): their roles and
// their teams. The two never collide (distinct uuids, and the seat's type picks the meaning).
func (s Seat) candidate(userID string, roleIDs []string) bool {
	if s.AssigneeID == "" {
		return false
	}
	switch s.AssigneeType {
	case ApproverUser:
		return s.AssigneeID == userID
	case ApproverRole:
		return holdsRole(roleIDs, s.AssigneeID)
	case ApproverTeam:
		return holdsRole(roleIDs, s.AssigneeID) // roleIDs carries the teams too — see Actor.HolderIDs
	case ApproverQueue:
		return s.QueueLive && (s.QueueRoleID == "" || holdsRole(roleIDs, s.QueueRoleID))
	}
	return false
}

// CandidateFor is candidate for callers outside the package (the work-item service).
func (s Seat) CandidateFor(userID string, roleIDs []string) bool { return s.candidate(userID, roleIDs) }

// Claimable reports whether the seat is a pool seat (role, team or queue) — the only kind that can
// be taken out of a pool; a user seat already has its one holder.
func (s Seat) Claimable() bool {
	return s.AssigneeType == ApproverRole || s.AssigneeType == ApproverTeam || s.AssigneeType == ApproverQueue
}

// EntityView answers whether the caller may VIEW a record entity (the entity-view ACL). It is asked
// only about QUEUE seats: a queue is shown to — and decided by — someone with the queue's role AND
// view access on the entity of the record (specs/043 plan "Quyền"). nil = not judged: system
// callers and unit tests; every person-driven service call sets it.
type EntityView func(entity string) bool

// viewable reports whether the seat passes the entity-view rule: only a queue seat is subject to
// it, and nil view judges nothing.
func (s Seat) viewable(entity string, view EntityView) bool {
	return s.AssigneeType != ApproverQueue || view == nil || view(entity)
}

func holdsRole(roleIDs []string, role string) bool {
	for _, rid := range roleIDs {
		if rid == role {
			return true
		}
	}
	return false
}

// claimedAgainst reports whether a trusted seat of the instance's current step is one the actor is
// a candidate of but someone ELSE has claimed — the one refusal that is a 409 ("đã có người nhận")
// rather than a 404: a candidate has already seen the item in the pool, a stranger learns nothing.
func claimedAgainst(inst *Instance, open []Seat, userID string, roleIDs []string, view EntityView) bool {
	for _, s := range trustedSeats(inst, open) {
		if s.ClaimedBy != "" && s.ClaimedBy != userID && s.Claimable() && s.candidate(userID, roleIDs) && s.viewable(inst.EntityName, view) {
			return true
		}
	}
	return false
}

// trustedSeats returns the open seats of a pending instance that agree with it: on the node its
// pin says it stands on and — while it projects a holder (current_approver_*) — held by that
// holder. A seat that disagrees was left open by a binary older than S3a (a rolling deploy, or a
// rollback on this schema), which moves instances without touching seats; the next write of a
// binary from S3a on heals it (SeatSuperseded), until then the table says the wrong thing and is
// not believed (specs/043 "Nợ còn mở" 21).
func trustedSeats(inst *Instance, open []Seat) []Seat {
	node := seatNodeID(inst)
	var out []Seat
	for _, s := range open {
		if s.NodeID != node {
			continue
		}
		if inst.CurrentApproverType != "" && (s.AssigneeType != inst.CurrentApproverType || s.AssigneeID != inst.CurrentApproverID) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// AwaitsActor is THE inbox rule (GET /approvals/pending and the /approvals/:id visibility gate —
// one body, so a row someone can click always opens): a pending instance awaits the actor when
// one of its trusted open seats is theirs. An instance with no seat it can trust — armed or moved
// by a pre-S3a binary — answers through its current_approver_* projection instead (D-6).
//
// While the projection names a holder, a trusted seat IS that holder, so the answer equals the
// projection read of before S3a2 — the parity DAILY-QUEUE #113 promises. The seats alone decide
// only where the projection is NULL: a pool or several seats on one step (D-5).
//
// ponytail: the projection fallback stays until Doctor D3 (seatDriftOf) counts 0 on every tenant;
// then this is `trusted seats only` (specs/043 "Nợ còn mở" 9).
//
// view is the caller's entity-view ACL for queue seats (EntityView); every surface that answers
// "does this await me" passes the same one, so the list, the detail and the decision cannot differ.
func AwaitsActor(inst *Instance, open []Seat, userID string, roleIDs []string, view EntityView) bool {
	if trusted := trustedSeats(inst, open); len(trusted) > 0 {
		for _, s := range trusted {
			if s.heldBy(userID, roleIDs) && s.viewable(inst.EntityName, view) {
				return true
			}
		}
		return false
	}
	if inst.CurrentApproverType == "" {
		return false
	}
	return Seat{AssigneeType: inst.CurrentApproverType, AssigneeID: inst.CurrentApproverID}.heldBy(userID, roleIDs)
}

// SeatDrift is how the seats of a PENDING instance fail to carry its inbox — the Doctor's reading
// of what the projection fallback of AwaitsActor still answers for (specs/043 S3a2, T063).
type SeatDrift string

const (
	// SeatDriftMissing: no open seat at all — armed or moved by a pre-S3a binary, or a step with
	// no approver.
	SeatDriftMissing SeatDrift = "missing"
	// SeatDriftMismatch: open seats, none of which agrees with the instance (another node, or a
	// holder the projection no longer names).
	SeatDriftMismatch SeatDrift = "mismatch"
)

// seatDriftOf classifies a pending instance's open seats: "" when AwaitsActor can answer from a
// trusted seat, else why it falls back.
func seatDriftOf(inst *Instance, open []Seat) SeatDrift {
	switch {
	case len(open) == 0 && inst.LastError != "":
		return "" // stuck on purpose (S6a, FR-018): nobody holds the step and last_error says so — not drift
	case onMachineNode(inst):
		return "" // a machine node (S7) is held by nobody: no seat is the right answer
	case len(open) == 0:
		return SeatDriftMissing
	case len(trustedSeats(inst, open)) == 0:
		return SeatDriftMismatch
	default:
		return ""
	}
}

// onMachineNode reports whether the instance stands on a gateway / auto / end node (specs/043 S7):
// a step nobody holds, which the engine walks through by itself.
func onMachineNode(inst *Instance) bool {
	n := inst.Definition.NodeAt(inst.CurrentStep)
	return n != nil && MachineKind(n.Kind)
}

// OnMachineNode is onMachineNode for callers outside the package (the admin reads).
func (i *Instance) OnMachineNode() bool { return onMachineNode(i) }

// Stuck reports whether a pending instance stands still for a reason an admin should look at: a step
// nobody holds (last_error) or a machine node the walk has not passed. A wait node that RESTS is
// neither — waiting for the record is its job (specs/043 S12a).
func (i *Instance) Stuck() bool {
	if i.Status != StatusPending {
		return false
	}
	if i.LastError != "" {
		return true
	}
	if n := i.Definition.NodeAt(i.CurrentStep); n != nil && n.Kind == NodeWait {
		return !resting(i)
	}
	return onMachineNode(i)
}

// seatNodeID is the stable id of the step an instance stands on: the id its pin gives the current
// step, else s{current_step+1} — the same answer Migration A's backfill computes in SQL for the
// same row (a pin written before S0 has no ids; one written after has all of them).
func seatNodeID(inst *Instance) string {
	if node, _ := inst.Definition.StepRef(inst.CurrentStep); node != "" {
		return node
	}
	return "s" + strconv.Itoa(inst.CurrentStep+1)
}

// NodeID is the stable id of the step the instance stands on (see seatNodeID) — the key its
// seats are read by.
func (i *Instance) NodeID() string { return seatNodeID(i) }

// escalatedAt is when the breach of the instance's current visit was actioned: the newest
// escalation in its history (escalated resets on every step change, so the newest one is this
// visit's). fallback covers a history that lost the entry.
func escalatedAt(inst *Instance, fallback time.Time) *time.Time {
	for i := len(inst.History) - 1; i >= 0; i-- {
		if inst.History[i].Action == HistoryEscalated {
			at := inst.History[i].At
			return &at
		}
	}
	return &fallback
}
