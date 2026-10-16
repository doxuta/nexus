package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/calendar"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// approvalProcessRepo is the consumer-side slice of the BPM process store the admin service needs.
// Every method is tenant-scoped in the real implementation (*bpm.SQLRepository), so no use case can
// read or mutate another tenant's process.
type approvalProcessRepo interface {
	CreateProcess(ctx context.Context, in bpm.CreateProcessInput) (string, error)
	GetProcess(ctx context.Context, tenantID, id string) (*bpm.ProcessRecord, error)
	ListProcesses(ctx context.Context, tenantID string) ([]bpm.ProcessRecord, error)
	UpdateProcess(ctx context.Context, tenantID, id string, in bpm.UpdateProcessInput) error
	DeleteProcess(ctx context.Context, tenantID, id string) error
	// Draft / publish / history (specs/043 S10).
	SaveDraft(ctx context.Context, tenantID, id string, in bpm.SaveDraftInput) error
	DiscardDraft(ctx context.Context, tenantID, id string) error
	Publish(ctx context.Context, tenantID, id, note, by string) (int, error)
	ListVersions(ctx context.Context, tenantID, processID string) ([]bpm.VersionRecord, error)
	GetVersion(ctx context.Context, tenantID, processID string, version int) (*bpm.VersionRecord, error)
	PublishedNodeIDs(ctx context.Context, tenantID, processID string) (map[string]bool, error)
	// Impact + repin (specs/043 S11).
	ProcessImpact(ctx context.Context, tenantID, processID string) (*bpm.Impact, error)
	ListProcessInstances(ctx context.Context, tenantID, processID string, version *int, status string, stuck bool, limit int) ([]bpm.ProcessInstanceRow, error)
	Repin(ctx context.Context, tenantID, processID string, in bpm.RepinInput) (*bpm.RepinOutcome, error)
}

// approvalInstanceRepo is the consumer-side slice for the pending-list read + per-record read. tenant-pinned.
type approvalInstanceRepo interface {
	ListPendingForUser(ctx context.Context, tenantID, userID string, roleIDs []string, view bpm.EntityView) ([]bpm.Instance, error)
	GetInstance(ctx context.Context, tenantID, id string) (*bpm.Instance, error)
	ListByRecord(ctx context.Context, tenantID, entityName, recordID string) ([]bpm.Instance, error)
	// ListByRecordLane is ListByRecord for one lane — the SLA card reads the tracking lane (S12b).
	ListByRecordLane(ctx context.Context, tenantID, entityName, recordID, lane string) ([]bpm.Instance, error)
	// ListOpenSeats feeds the /approvals/:id visibility gate the seats the inbox judged the row by.
	ListOpenSeats(ctx context.Context, tenantID, instanceID string) ([]bpm.Seat, error)
	// ListVisitSeats feeds the detail's seats[] / completion: every seat, in any state, of the
	// latest visit of a step (specs/043 S4).
	ListVisitSeats(ctx context.Context, tenantID, instanceID, nodeID string) ([]bpm.VisitSeat, error)
}

// approvalDecider applies an approve/reject decision (the synchronous engine path).
type approvalDecider interface {
	DecideAt(ctx context.Context, tenantID, id string, kind bpm.DecisionKind, comment string, actor bpm.Actor, expectedStep *int) (*bpm.Instance, error)
	// Override is an admin deciding a many-seat step in place of its seats (specs/043 S4, D-13).
	Override(ctx context.Context, tenantID, id string, kind bpm.DecisionKind, note string, actor bpm.Actor, expectedStep *int) (*bpm.Instance, error)
}

// approvalRequester serves the submitter's and approver's human verbs: ask, take back, hand on.
// Satisfied by *bpm.Requester. Optional — nil leaves the automatic-only behaviour untouched.
type approvalRequester interface {
	SubmitWith(ctx context.Context, tenantID, entityName, recordID string, data map[string]any, actor bpm.Actor, opt bpm.SubmitOptions) (*bpm.Instance, error)
	Startable(ctx context.Context, tenantID, entityName string, data map[string]any) ([]bpm.StartableProcess, error)
	Recall(ctx context.Context, tenantID, id, comment string, actor bpm.Actor) (*bpm.Instance, error)
	Delegate(ctx context.Context, tenantID, id, toUserID, comment string, actor bpm.Actor) (*bpm.Instance, error)
}

// approvalRetrier restarts the walk of an instance stalled on a machine step (specs/043 S7).
// Satisfied by *bpm.Runner. Optional — nil leaves "Thử lại" unavailable (422 below).
type approvalRetrier interface {
	Retry(ctx context.Context, tenantID, id string, actor bpm.Actor, comment string) (*bpm.Instance, error)
}

// approvalRecordReader reads ONE record under the caller's row scope. Submit needs it twice over:
// to prove the caller may act on THIS row (entity-level "edit" is not row-level permission), and
// to get the field values the process criteria are evaluated against. Satisfied by
// *services.RecordService — the same Get the record screen uses, so the two answers cannot drift.
type approvalRecordReader interface {
	Get(ctx context.Context, scope RecordScope, tenantID, entity, id string, expand, rollup []string) (*dynrepo.Record, error)
	// AuthorizeView re-resolves the caller's row-scope level for an entity. Required because the
	// approval routes are FLAT (/approvals/:id — no :entity segment), so the route-level ACL stage
	// cannot run and scopeOf(c) hands the service AccessNone for every non-admin. Without this the
	// record probe below is dead code: it would ask "can you see this row" holding no access at all
	// and always answer no. Satisfied by *services.RecordService.
	AuthorizeView(ctx context.Context, tenantID string, scope RecordScope, entity string) (RecordScope, error)
}

// approvalUserExists reports whether userID is a real, non-deleted user in THIS tenant. Delegate
// parks the current step on a named person; without this, a typo or a user id copied from another
// tenant becomes an approval no one can ever act on (ghost-park) — it just sits pending forever.
// The bool/error split keeps a transient DB failure (surface it, 500) distinct from a genuine
// "no such user" (reject the delegate, 422). Optional — nil skips the check. A func port, not an
// interface, so services need not import auth just to know a user exists.
type approvalUserExists func(ctx context.Context, tenantID, userID string) (bool, error)

// approvalQueueExists reports whether queueID is a live, ACTIVE work queue of THIS tenant — what a
// graph node naming a queue (specs/043 S5) must point at, or its seat could never be claimed. A
// func port like approvalUserExists, wired in main.go over the work-queue repository.
type approvalQueueExists func(ctx context.Context, tenantID, queueID string) (bool, error)

// approvalHolderExists reports whether a user, role or team of THIS tenant exists — what an
// assignment (specs/043 S6a) or the fallback assignee must point at, or its seat could never be
// held. A user must be active; a role / team just present. A func port like approvalUserExists,
// wired in main.go over bpm.SQLRepository.HolderExists.
type approvalHolderExists func(ctx context.Context, tenantID string, kind bpm.ApproverType, id string) (bool, error)

// ApprovalService implements the EPIC 5 admin CRUD over /admin/approval-processes plus the user-
// facing approval surface (GET pending, POST approve/reject). Every method takes the verified
// tenant id and pins all repository calls to it. It validates the trigger/criteria/steps shape
// BEFORE any write so a stored process always carries a runnable definition. HTTP-agnostic.
type ApprovalService struct {
	procRepo approvalProcessRepo
	instRepo approvalInstanceRepo
	decider  approvalDecider
	entities workflowEntityChecker // optional; nil → skip entity-existence check

	// Request-path deps (WithRequestFlow). All-or-nothing: while unwired, submit/recall/delegate
	// answer APPROVAL_REQUEST_UNAVAILABLE rather than half-working.
	requester approvalRequester
	records   approvalRecordReader
	// retrier restarts a hồ sơ stalled on an automatic step (WithRetrier); nil = no "Thử lại".
	retrier approvalRetrier

	// userExists guards the delegate target. Optional; nil leaves delegation as-is (engine still
	// rejects empty target and self-delegate). Wired in main.go over auth.UserRepository.GetUserByID.
	userExists approvalUserExists
	// queueExists guards a graph node's work-queue assignee (WithQueueChecker). Optional; nil skips
	// the check (unit tests without a queue store).
	queueExists approvalQueueExists
	// holderExists guards a step's user / role / team assignee and the fallback (WithHolderChecker).
	// Optional; nil skips the check (unit tests without a directory).
	holderExists approvalHolderExists
	// teamsOf answers which teams a user belongs to (WithTeamDirectory), so a team seat is held by
	// its members. nil = nobody is a member of any team (fail closed).
	teamsOf teamDirectory
	// acl answers the entity-view ACL a QUEUE seat is judged by (WithQueueACL). nil fails closed:
	// no queue seat is held through this service.
	acl workItemACL

	// workingTime measures "how long did this approval take" on the SAME calendar the step
	// deadlines walk (specs/043 S2, #40). Optional; nil leaves durations off the detail rather than
	// guessing a clock. Wired in main.go with the deadline calculator itself.
	workingTime *BPMDeadlineCalculator
	// rules lists the tenant's automation rules (WithDueWriters): the "two writers of one deadline
	// field" warning reads them. Optional.
	rules ruleLister
	now   func() time.Time
}

// WithWorkingTime arms the elapsed-time readout of GET /approvals/:id. Chainable.
func (s *ApprovalService) WithWorkingTime(c *BPMDeadlineCalculator) *ApprovalService {
	s.workingTime = c
	return s
}

// WithUserChecker arms the delegate-target existence guard. Chainable.
func (s *ApprovalService) WithUserChecker(f approvalUserExists) *ApprovalService {
	s.userExists = f
	return s
}

// WithQueueChecker arms the existence guard on a graph node's work-queue assignee. Chainable.
func (s *ApprovalService) WithQueueChecker(f approvalQueueExists) *ApprovalService {
	s.queueExists = f
	return s
}

// WithHolderChecker arms the existence guard on a step's user / role / team assignee and on the
// fallback assignee. Chainable.
func (s *ApprovalService) WithHolderChecker(f approvalHolderExists) *ApprovalService {
	s.holderExists = f
	return s
}

// WithTeamDirectory arms team membership for the actor: a team seat is a pool its members hold.
// Chainable.
func (s *ApprovalService) WithTeamDirectory(d teamDirectory) *ApprovalService {
	s.teamsOf = d
	return s
}

// WithQueueACL arms the entity-view rule of queue seats: someone with the queue's role may see and
// decide its seats only if they may view the record's entity — the rule the work-item routes apply,
// here on the approval surface (list, detail, approve, reject, delegate). Chainable.
func (s *ApprovalService) WithQueueACL(a workItemACL) *ApprovalService {
	s.acl = a
	return s
}

// withQueueView returns the actor carrying what the engine judges authority with beyond their own
// name and role: the entity-view rule of queue seats (bpm.Actor.QueueView) and the teams they belong
// to (bpm.Actor.TeamIDs — a team seat is held by its members).
func (s *ApprovalService) withQueueView(ctx context.Context, tenantID string, actor bpm.Actor) bpm.Actor {
	actor.QueueView = entityViewFor(ctx, s.acl, nil, tenantID, actor.RoleIDs, actor.IsAdmin)
	actor.TeamIDs = teamIDsOf(ctx, s.teamsOf, nil, tenantID, actor.UserID)
	return actor
}

// nodesOf is the chain of a process body as nodes, whichever form it came in: the graph's nodes, or
// the steps (a lifted step list carries the same assignments a node does).
func nodesOf(steps []ApprovalStepInput, g *ApprovalGraphInput) (nodes []ApprovalNodeInput, at string) {
	if g != nil {
		return g.Nodes, "graph.nodes"
	}
	nodes = make([]ApprovalNodeInput, len(steps))
	for i, st := range steps {
		nodes[i] = ApprovalNodeInput{ApprovalStepInput: st, Kind: string(bpm.NodeApproval)}
	}
	return nodes, "steps"
}

// checkRefs judges what a chain's steps and the process options name against the tenant (specs/043
// S5, S6a): every queue assignee is a live, active work queue; every user / role / team an
// assignment (or the fallback assignee) names exists; every `field` assignment points at a
// user-type field of the process's entity (or the owner); every requiredFields entry is a field of
// the entity. Shape was already judged (buildApprovalGraphJSON); this is the half that needs the
// tenant. at is where the nodes live in the body ("graph.nodes" | "steps").
func (s *ApprovalService) checkRefs(ctx context.Context, tenantID, entity string, nodes []ApprovalNodeInput, at string, opts *ApprovalOptionsInput) error {
	var fieldTypes map[string]string // lazily read: most chains name no field
	fields := func() (map[string]string, error) {
		if fieldTypes == nil {
			var err error
			if fieldTypes, err = s.entityFieldTypes(ctx, tenantID, entity); err != nil {
				return nil, err
			}
		}
		return fieldTypes, nil
	}
	if err := s.checkRoutingRefs(ctx, tenantID, entity, nodes, at); err != nil {
		return err
	}
	for i, n := range nodes {
		if s.queueExists != nil && bpm.ApproverType(strings.TrimSpace(n.ApproverType)) == bpm.ApproverQueue {
			ok, err := s.queueExists(ctx, tenantID, strings.TrimSpace(n.ApproverID))
			if err != nil {
				return fmt.Errorf("services.ApprovalService.checkRefs: queue: %w", err)
			}
			if !ok {
				return invalidField("APPROVAL_ASSIGNEE_NOT_FOUND", fmt.Sprintf("%s[%d].approverId", at, i),
					"hàng đợi của bước %d không tồn tại hoặc đã tắt", i+1)
			}
		}
		if a := n.Assignment; a != nil {
			if err := s.checkAssignmentRef(ctx, tenantID, entity, fmt.Sprintf("%s[%d].assignment", at, i), i+1, a, fields); err != nil {
				return err
			}
		}
		if bpm.NodeKind(strings.TrimSpace(n.Kind)) == bpm.NodeWait {
			if err := s.checkWaitRefs(ctx, tenantID, entity, at, i, n, fields); err != nil {
				return err
			}
		}
		if len(n.RequiredFields) == 0 {
			continue
		}
		set, err := fields()
		if err != nil {
			return err
		}
		if set == nil { // no field lister wired (unit tests): shape was checked, existence cannot be
			continue
		}
		for _, f := range n.RequiredFields {
			if name := strings.TrimSpace(f); !fieldKnown(set, name) {
				return invalidField("APPROVAL_REQUIRED_FIELD_UNKNOWN", fmt.Sprintf("%s[%d].requiredFields", at, i),
					"trường bắt buộc %q của bước %d không có trong đối tượng %s", name, i+1, entity)
			}
		}
	}
	if err := s.checkSLAFitsCalendar(ctx, tenantID, nodes, at, opts); err != nil {
		return err
	}
	if opts != nil && opts.PauseWhen != nil {
		set, err := fields()
		if err != nil {
			return err
		}
		for k, r := range opts.PauseWhen.Rules {
			if f := strings.TrimSpace(r.Field); set != nil && !fieldKnown(set, f) {
				return invalidField("APPROVAL_FIELD_UNKNOWN", fmt.Sprintf("options.pauseWhen.rules[%d].field", k),
					"điều kiện tạm dừng đọc trường %q không có trong đối tượng %s", f, entity)
			}
		}
	}
	if opts != nil && opts.FallbackAssignee != nil {
		f := opts.FallbackAssignee
		if err := s.checkHolder(ctx, tenantID, bpm.ApproverType(strings.TrimSpace(f.Type)), strings.TrimSpace(f.ID),
			"options.fallbackAssignee.id", "người dự phòng"); err != nil {
			return err
		}
	}
	return nil
}

func fieldKnown(set map[string]string, name string) bool { _, ok := set[name]; return ok }

// checkAssignmentRef judges one assignment against the tenant. at is its location in the body, step
// the 1-based number the message shows.
func (s *ApprovalService) checkAssignmentRef(ctx context.Context, tenantID, entity, at string, step int, a *ApprovalAssignmentInput, fields func() (map[string]string, error)) error {
	switch t := bpm.AssignmentType(strings.TrimSpace(a.Type)); t {
	case bpm.AssignUser, bpm.AssignRole, bpm.AssignTeam:
		return s.checkHolder(ctx, tenantID, bpm.ApproverType(t), strings.TrimSpace(a.ID), at+".id", fmt.Sprintf("người nhận của bước %d", step))
	case bpm.AssignQueue:
		if s.queueExists == nil {
			return nil
		}
		ok, err := s.queueExists(ctx, tenantID, strings.TrimSpace(a.ID))
		if err != nil {
			return fmt.Errorf("services.ApprovalService.checkAssignmentRef: queue: %w", err)
		}
		if !ok {
			return invalidField("APPROVAL_ASSIGNEE_NOT_FOUND", at+".id", "hàng đợi của bước %d không tồn tại hoặc đã tắt", step)
		}
	case bpm.AssignField:
		name := strings.TrimSpace(a.Field)
		if name == bpm.OwnerField {
			return nil // the record's owner is not a field of the entity, and is always a user
		}
		set, err := fields()
		if err != nil {
			return err
		}
		if set == nil {
			return nil // no field lister wired (unit tests)
		}
		ft, ok := set[name]
		if !ok {
			return invalidField("APPROVAL_ASSIGNEE_FIELD_INVALID", at+".field",
				"trường %q của bước %d không có trong đối tượng %s", name, step, entity)
		}
		if ft != "user" {
			return invalidField("APPROVAL_ASSIGNEE_FIELD_INVALID", at+".field",
				"trường %q của bước %d không phải kiểu người dùng (user) nên không chỉ ra được ai nhận việc", name, step)
		}
	}
	return nil
}

// checkRoutingRefs judges the fields the machine nodes of a graph name against the entity
// (specs/043 S7, FR-003): a gateway's rules read fields that exist; an auto node writes fields that
// exist, are writable (not computed / assigned) and accept the value — the SAME checks a field_update
// of a rule gets, so a value the record would refuse is refused HERE, at save, instead of stalling
// an approval later. Nothing is read from the tenant when the graph has no machine node.
func (s *ApprovalService) checkRoutingRefs(ctx context.Context, tenantID, entity string, nodes []ApprovalNodeInput, at string) error {
	var specs []FieldSpec
	byName := map[string]FieldSpec{}
	loaded := false
	load := func() error {
		if loaded {
			return nil
		}
		loaded = true
		lister, ok := s.entities.(workflowFieldLister)
		if !ok || s.entities == nil {
			return nil // partial test fakes: shape was checked, existence cannot be
		}
		def, err := s.entities.GetEntityByName(ctx, tenantID, entity)
		if err != nil {
			return fmt.Errorf("services.ApprovalService.checkRoutingRefs: %w", err)
		}
		fields, err := lister.ListFields(ctx, tenantID, def.ID)
		if err != nil {
			return fmt.Errorf("services.ApprovalService.checkRoutingRefs: %w", err)
		}
		if specs, err = toFieldSpecs(fields); err != nil {
			return fmt.Errorf("services.ApprovalService.checkRoutingRefs: %w", err)
		}
		for _, sp := range specs {
			byName[sp.Name] = sp
		}
		return nil
	}
	for i, n := range nodes {
		kind := bpm.NodeKind(strings.TrimSpace(n.Kind))
		if kind != bpm.NodeGateway && kind != bpm.NodeAuto {
			continue
		}
		if err := load(); err != nil {
			return err
		}
		if specs == nil {
			return nil
		}
		for j, b := range n.Branches {
			for k, r := range b.Rules {
				if f := strings.TrimSpace(r.Field); byName[f].Name == "" {
					return invalidField("APPROVAL_FIELD_UNKNOWN", fmt.Sprintf("%s[%d].branches[%d].when.rules[%d].field", at, i, j, k),
						"điều kiện của nhánh %q (bước %d) đọc trường %q không có trong đối tượng %s", strings.TrimSpace(b.Name), i+1, f, entity)
				}
			}
		}
		for j, w := range n.Set {
			field := strings.TrimSpace(w.Field)
			where := fmt.Sprintf("%s[%d].set[%d]", at, i, j)
			sp, known := byName[field]
			if !known {
				return invalidField("RECORD_UNKNOWN_FIELD", where+".field", "bước tự động %d ghi trường %q không có trong đối tượng %s", i+1, field, entity)
			}
			if code, readonly := computedReadonlyCode(sp.FieldType); readonly {
				return invalidField(code, where+".field", "trường %q (%s) là trường tính/tự gán — bước tự động không ghi đè được", field, sp.FieldType)
			}
			if _, err := ValidatePartialRecord(specs, map[string]any{field: w.Value}); err != nil {
				var ve *ValidationError
				if !errors.As(err, &ve) {
					return fmt.Errorf("services.ApprovalService.checkRoutingRefs: %w", err)
				}
				return invalidField(ve.Code, where+".value", "giá trị bước tự động %d ghi vào %q không hợp lệ: %s", i+1, field, ve.Msg)
			}
		}
	}
	return nil
}

// checkHolder refuses a user / role / team / queue that does not exist in THIS tenant. what is the
// phrase for the message ("người dự phòng").
func (s *ApprovalService) checkHolder(ctx context.Context, tenantID string, kind bpm.ApproverType, id, at, what string) error {
	switch kind {
	case bpm.ApproverQueue:
		if s.queueExists == nil {
			return nil
		}
		ok, err := s.queueExists(ctx, tenantID, id)
		if err != nil {
			return fmt.Errorf("services.ApprovalService.checkHolder: queue: %w", err)
		}
		if !ok {
			return invalidField("APPROVAL_ASSIGNEE_NOT_FOUND", at, "hàng đợi của %s không tồn tại hoặc đã tắt", what)
		}
	case bpm.ApproverUser, bpm.ApproverRole, bpm.ApproverTeam:
		if s.holderExists == nil {
			return nil
		}
		ok, err := s.holderExists(ctx, tenantID, kind, id)
		if err != nil {
			return fmt.Errorf("services.ApprovalService.checkHolder: %s: %w", kind, err)
		}
		if !ok {
			return invalidField("APPROVAL_ASSIGNEE_NOT_FOUND", at, "%s (%s) không tồn tại trong tổ chức này", what, kind)
		}
	}
	return nil
}

// entityFieldTypes maps the field names of the entity to their types, or nil when the entity
// checker cannot list fields (partial test fakes — the same fall-through the workflow service takes).
func (s *ApprovalService) entityFieldTypes(ctx context.Context, tenantID, entity string) (map[string]string, error) {
	lister, ok := s.entities.(workflowFieldLister)
	if !ok || s.entities == nil {
		return nil, nil
	}
	def, err := s.entities.GetEntityByName(ctx, tenantID, entity)
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.entityFieldTypes: %w", err)
	}
	fields, err := lister.ListFields(ctx, tenantID, def.ID)
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.entityFieldTypes: %w", err)
	}
	set := make(map[string]string, len(fields))
	for _, f := range fields {
		set[f.Name] = f.FieldType
	}
	return set, nil
}

// WithRequestFlow arms submit/recall/delegate. requester runs the transitions; records answers
// "may this caller act on this row, and what does it contain". Chainable.
func (s *ApprovalService) WithRequestFlow(r approvalRequester, rec approvalRecordReader) *ApprovalService {
	s.requester = r
	s.records = rec
	return s
}

// SubmitForApproval is the verb a person presses. It resolves the record under the CALLER's scope
// first — so asking for approval on a row you cannot see fails exactly the way reading it does,
// and the criteria are evaluated against the values the caller is actually allowed to act on —
// then arms a manual process: the one the sender named (opt.ProcessID) or the first that fits.
func (s *ApprovalService) SubmitForApproval(ctx context.Context, scope RecordScope, tenantID, entityName, recordID string, actor bpm.Actor, opt bpm.SubmitOptions) (*bpm.Instance, error) {
	if s.requester == nil || s.records == nil {
		return nil, invalid("APPROVAL_REQUEST_UNAVAILABLE", "chức năng gửi duyệt chưa được cấu hình")
	}
	data, err := s.recordForSend(ctx, scope, tenantID, entityName, recordID)
	if err != nil {
		return nil, err
	}
	inst, err := s.requester.SubmitWith(ctx, tenantID, entityName, recordID, data, actor, opt)
	if err != nil {
		return nil, mapRequestErr(err)
	}
	return inst, nil
}

// StartableProcesses lists the manual processes the caller may send this record through — the same
// two reads as a send, so a row the caller cannot act on answers exactly as the send would.
func (s *ApprovalService) StartableProcesses(ctx context.Context, scope RecordScope, tenantID, entityName, recordID string) ([]bpm.StartableProcess, error) {
	if s.requester == nil || s.records == nil {
		return nil, invalid("APPROVAL_REQUEST_UNAVAILABLE", "chức năng gửi duyệt chưa được cấu hình")
	}
	data, err := s.recordForSend(ctx, scope, tenantID, entityName, recordID)
	if err != nil {
		return nil, err
	}
	out, err := s.requester.Startable(ctx, tenantID, entityName, data)
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.StartableProcesses: %w", err)
	}
	return out, nil
}

// recordForSend runs the two reads a send and the startable list share and returns the record's
// true data for criteria evaluation.
func (s *ApprovalService) recordForSend(ctx context.Context, scope RecordScope, tenantID, entityName, recordID string) (map[string]any, error) {
	// Two reads with two DIFFERENT purposes, deliberately not merged:
	//
	// (1) The caller-scoped Get is the ROW-VISIBILITY probe — may this person act on this record?
	// Its refusal (404 for an invisible row) is forwarded verbatim.
	if _, err := s.records.Get(ctx, scope, tenantID, entityName, recordID, nil, nil); err != nil {
		return nil, s.sharedReadOnlyOr(ctx, scope, tenantID, entityName, recordID, err)
	}
	// (2) A SYSTEM read supplies the criteria-evaluation data. The caller-scoped read strips
	// field-ACL-hidden fields, and approval ROUTING must not depend on which fields the submitter
	// happens to see: two people submitting the same record would otherwise route to different
	// processes ("over 50M needs the director" would silently misfire for anyone who cannot read
	// the amount). Routing is a system decision on the record's true state. Authorization already
	// happened in (1), so this system read grants no new access.
	full, err := s.records.Get(ctx, RecordScope{IsAdmin: true, AccessLevel: "all"}, tenantID, entityName, recordID, nil, nil)
	if err != nil {
		return nil, err
	}
	return full.Data, nil
}

// sharedReadOnlyOr maps a row-visibility miss of SubmitForApproval: this route is pinned to "edit", so
// the scope carries no manual-share flag and a caller who sees the record ONLY through a share (specs/
// 051) used to get the same 404 as a stranger. When the same level plus the share clause does reach
// the row they are a share-only viewer — they already read it, so RECORD_SHARED_READ_ONLY (403, the
// PUT/DELETE answer) leaks nothing (DAILY-QUEUE #225). The share clause only widens owner/team scopes,
// so admins and level "all" (a miss means the record is gone) skip the probe. Any other error, and a
// miss that survives the flag, is returned unchanged.
func (s *ApprovalService) sharedReadOnlyOr(ctx context.Context, scope RecordScope, tenantID, entityName, recordID string, err error) error {
	if !errors.Is(err, ErrRecordNotFound) || scope.IsAdmin || scope.IncludeShares || !dynrepo.AccessLevel(scope.AccessLevel).IsOwnerScoped() {
		return err
	}
	probe := scope
	probe.IncludeShares = true
	if _, perr := s.records.Get(ctx, probe, tenantID, entityName, recordID, nil, nil); perr == nil {
		return ErrRecordSharedReadOnly
	}
	return err
}

// RecallApproval withdraws a pending request the caller submitted.
func (s *ApprovalService) RecallApproval(ctx context.Context, tenantID, id, comment string, actor bpm.Actor) (*bpm.Instance, error) {
	if s.requester == nil {
		return nil, invalid("APPROVAL_REQUEST_UNAVAILABLE", "chức năng thu hồi chưa được cấu hình")
	}
	inst, err := s.requester.Recall(ctx, tenantID, id, comment, actor)
	if err != nil {
		return nil, mapRequestErr(err)
	}
	return inst, nil
}

// DelegateApproval hands the current step to another user.
func (s *ApprovalService) DelegateApproval(ctx context.Context, tenantID, id, toUserID, comment string, actor bpm.Actor) (*bpm.Instance, error) {
	if s.requester == nil {
		return nil, invalid("APPROVAL_REQUEST_UNAVAILABLE", "chức năng uỷ quyền chưa được cấu hình")
	}
	// Reject a ghost target BEFORE the transition writes: a non-existent or cross-tenant user id
	// would otherwise park the step on someone who can never act on it. Self-delegate and empty
	// target are still the engine's to reject (ErrDelegateToSelf / ErrNotAuthorized).
	if s.userExists != nil {
		if targetID := strings.TrimSpace(toUserID); targetID != "" {
			ok, err := s.userExists(ctx, tenantID, targetID)
			if err != nil {
				return nil, fmt.Errorf("ApprovalService.DelegateApproval: check user: %w", err)
			}
			if !ok {
				return nil, invalid("APPROVAL_DELEGATE_UNKNOWN_USER", "người nhận uỷ quyền không tồn tại trong tổ chức này")
			}
		}
	}
	inst, err := s.requester.Delegate(ctx, tenantID, id, toUserID, comment, s.withQueueView(ctx, tenantID, actor))
	if err != nil {
		return nil, mapRequestErr(err)
	}
	return inst, nil
}

// mapRequestErr turns the engine's sentinels into the API's coded validation errors. Each one is a
// DIFFERENT thing the person needs to do next, which is why they are not collapsed into one code:
// configure a process · look at the request already open · you are not the sender · pick someone else.
func mapRequestErr(err error) error {
	if mapped := mapInstanceErr(err); mapped != nil {
		return mapped
	}
	switch {
	case errors.Is(err, bpm.ErrNoApprovalProcess):
		return invalid("APPROVAL_NO_PROCESS", "chưa có quy trình duyệt nào nhận bản ghi này — cấu hình quy trình 'gửi tay' trước")
	case errors.Is(err, bpm.ErrProcessNotApplicable):
		return invalid("APPROVAL_PROCESS_NOT_APPLICABLE", "quy trình đã chọn không dùng được cho bản ghi này — chọn lại trong danh sách quy trình đang khớp")
	case errors.Is(err, bpm.ErrIdempotencyKeyReused):
		return invalid("APPROVAL_IDEMPOTENCY_KEY_REUSED", "Idempotency-Key này đã được dùng cho một yêu cầu khác — dùng khoá mới cho yêu cầu mới")
	case errors.Is(err, bpm.ErrAlreadyPending):
		return invalid("APPROVAL_ALREADY_PENDING", "bản ghi này đang chờ duyệt rồi")
	case errors.Is(err, bpm.ErrTrackingLane):
		return invalid("APPROVAL_TRACKING_NOT_RECALLABLE", "đây là đồng hồ SLA của bản ghi, không phải yêu cầu duyệt — không thu hồi được")
	case errors.Is(err, bpm.ErrNotSubmitter):
		return invalid("APPROVAL_NOT_SUBMITTER", "chỉ người gửi mới thu hồi được yêu cầu này")
	case errors.Is(err, bpm.ErrDelegateToSelf):
		return invalid("APPROVAL_DELEGATE_SELF", "không uỷ quyền cho chính mình")
	case errors.Is(err, bpm.ErrDelegateTargetSeated):
		return invalid("APPROVAL_DELEGATE_TARGET_SEATED", "người nhận đã có ghế hoặc đã bỏ phiếu ở bước này — một người chỉ bỏ một phiếu mỗi lượt, hãy chọn người khác")
	default:
		return err
	}
}

// NewApprovalService wires the approval service over the process/instance repositories + decider.
func NewApprovalService(procRepo approvalProcessRepo, instRepo approvalInstanceRepo, decider approvalDecider) *ApprovalService {
	return &ApprovalService{procRepo: procRepo, instRepo: instRepo, decider: decider, now: time.Now}
}

// WithEntityChecker injects the metadata entity-existence checker so create/update reject a process
// targeting an unknown entity. Chainable.
func (s *ApprovalService) WithEntityChecker(c workflowEntityChecker) *ApprovalService {
	s.entities = c
	return s
}

// ApprovalStepInput is one step as received from the API (camelCase mapped upstream).
type ApprovalStepInput struct {
	// ID is the step's stable id (specs/043 S0). Optional: a missing id is assigned by position
	// (bpm.NormalizeSteps) and written on this save, so it stays put from then on.
	ID           string
	Name         string
	ApproverType string
	ApproverID   string
	SLASeconds   int
	// SLAClock is what SLASeconds is counted in (specs/043 S1): "business" (working hours on the
	// tenant's calendar), "wall", or "" — stored as sent; "" means business for every instance
	// started from now on (bpm pins it explicitly at arm time).
	SLAClock string
	// Approvers + Completion make the step PARALLEL (specs/043 S4): several people each hold a
	// seat, and Completion ("all" | "any") says when the step is over. Instead of ApproverType/
	// ApproverID, never beside them. A request that carries them is stored in the graph form.
	Approvers  []ApprovalApproverInput
	Completion *ApprovalCompletionInput
	// OnReject is where a rejection of this step goes (specs/043 S8). A step that carries it is
	// stored in the graph form (a binary older than S8 would read it as a final reject).
	OnReject *ApprovalOnRejectInput
	// Assignment names the holder by rule instead of by name (specs/043 S6a): a team pool, a
	// queue, the person in a record field, the submitter. Instead of ApproverType/ApproverID and
	// Approvers. A request that carries it is stored in the graph form.
	Assignment *ApprovalAssignmentInput
	// Unsupported lists the keys the caller SENT on this step outside the linear v1 shape —
	// filled by whichever decoder saw the raw JSON (UnsupportedApprovalKeys). The validator
	// refuses them (D-7) rather than letting the typed decode drop them silently.
	Unsupported []string
}

// ApprovalAssignmentInput is the rule form of a step's holder as received from the API.
// Unsupported lists the keys sent outside bpm.AssignmentKeys (refused, never dropped silently).
type ApprovalAssignmentInput struct {
	Type, ID, Field, Strategy string
	Unsupported               []string
}

// ApprovalOptionsInput is the process-level settings block as received from the API (specs/043
// S6a). Unsupported lists the keys sent outside bpm.OptionsKeys.
type ApprovalOptionsInput struct {
	FallbackAssignee *ApprovalApproverInput
	Lane             string // "" | "exclusive" | "independent" (S12b)
	CalendarID       string // pinned working calendar ("" = tenant default, S13)
	PauseWhen        *workflow.Condition
	Unsupported      []string
}

// ApprovalApproverInput is one approver of a parallel step: type "user" | "role" and its id.
type ApprovalApproverInput struct {
	Type string
	ID   string
}

// ApprovalOnRejectInput is a step's answer to a rejection: To "@end" | "@submitter" | an earlier
// step's id; ResumeAt "rejector" | "start" (only with "@submitter").
type ApprovalOnRejectInput struct {
	To       string
	ResumeAt string
}

// ApprovalCompletionInput is the policy of a parallel step: Mode "all" | "any" | "quorum"; Percent
// (1..100) goes with "quorum" only.
type ApprovalCompletionInput struct {
	Mode    string
	Percent int
}

// completion is the engine form of the input.
func (c *ApprovalCompletionInput) completion() *bpm.Completion {
	if c == nil {
		return nil
	}
	return &bpm.Completion{Mode: bpm.CompletionMode(strings.TrimSpace(c.Mode)), Percent: c.Percent}
}

// ApprovalStepKeys is the closed key set of one step on the API wire since S4: the v1 keys plus
// approvers/completion (bpm.StepKeysParallel). A step that carries the latter is stored in the
// graph form — the linear column stays what a pre-S4 binary can read.
var ApprovalStepKeys = bpm.StepKeysWire

// ApprovalStepV1Keys is the closed key set of one linear (v1) approval step on the API wire.
// Anything else — parallel approvers, routing, onReject, an sla object — is a capability a later
// slice of specs/043 unlocks together with the engine code that runs it (D-7). slaClock joined in
// S1, with the calendar-backed deadline that honours it. One list with the engine's (bpm.StepKeys).
var ApprovalStepV1Keys = bpm.StepKeys

// ApprovalProcessKeys is the closed key set of the POST/PUT /admin/approval-processes body.
// graph joined in S3a2 (stored in its own column, run by the engine as its node chain); options
// joined in S6a with its first sub-key, fallbackAssignee (bpm.OptionsKeys); lane S12b and
// calendarId/pauseWhen S13 join with the code that runs them (D-7).
var ApprovalProcessKeys = []string{"name", "entityName", "trigger", "criteria", "steps", "graph", "outcome", "options", "isActive"}

// ApprovalGraphKeys is the closed key set of the `graph` object (specs/043 S3a2): nodes only —
// start/next/branches arrive with the node kinds that need them (S7). The SAME list the engine
// refuses a stored graph by (bpm.GraphKeys): the API and the binary never disagree on what runs.
var ApprovalGraphKeys = bpm.GraphKeys

// ApprovalNodeKeys is the closed key set of one graph node at S3a2: a v1 step plus its kind
// (bpm.NodeKeys — the engine's list, as above).
var ApprovalNodeKeys = bpm.NodeKeys

// ApprovalGraphInput is the graph form of a process chain as received from the API (specs/043
// D-2). Unsupported lists the keys the caller sent on the graph object outside ApprovalGraphKeys.
type ApprovalGraphInput struct {
	Nodes []ApprovalNodeInput
	// Start is the id of the node the walk begins on (S7); "" = the first.
	Start       string
	Unsupported []string
}

// parallel reports whether the step input is in the S4 many-approver form.
func (s ApprovalStepInput) parallel() bool { return len(s.Approvers) > 0 || s.Completion != nil }

// needsGraph reports whether the step carries something the linear column cannot hold (S4 approvers,
// S6a rule-based assignment, S8 onReject): the whole chain is then stored in the graph form — the
// linear column is what a binary older than the slice reads.
func (s ApprovalStepInput) needsGraph() bool {
	return s.parallel() || s.Assignment != nil || s.OnReject != nil
}

// ApprovalNodeInput is one node: a step (validated by the SAME rules, its Unsupported keys judged
// against ApprovalNodeKeys) plus its kind.
type ApprovalNodeInput struct {
	ApprovalStepInput
	Kind string
	// RequiredFields are the record fields a TASK node needs filled before it completes (S5).
	RequiredFields []string
	// Routing (S7). Next is the node after this one (default: the next in the list); a gateway has
	// Branches + Default instead, an auto node Set, an end node Result.
	Next     string
	Branches []ApprovalBranchInput
	Default  string
	Set      []ApprovalSetInput
	Result   string
	// Until + SLA are a wait node's condition and deadline (S12a).
	Until *ApprovalConditionInput
	SLA   *ApprovalWaitSLAInput
}

// ApprovalBranchInput is one arm of a gateway: when its condition holds on the record's current
// values the walk goes To that node. Match "and" (default) or "or" joins Rules, the same
// vocabulary as the process's entry criteria.
type ApprovalBranchInput struct {
	Name  string
	Match string
	Rules []ApprovalCriteriaRuleInput
	To    string
}

// ApprovalSetInput is one field write of an auto node.
type ApprovalSetInput struct {
	Field string
	Value any
}

// UnsupportedApprovalKeys returns the top-level keys of the JSON object raw that are not in
// allowed, sorted, case-insensitive (bpm.UnknownKeys — the one body the engine decodes with too).
func UnsupportedApprovalKeys(raw []byte, allowed ...string) []string {
	return bpm.UnknownKeys(raw, allowed...)
}

// UnsupportedApprovalProcessKeys refuses process-level keys outside the v1 shape (D-7) with the
// same code the step check uses. nil/empty → nil. One body for every writer that decodes a raw
// process (the API handler, the AI tool).
func UnsupportedApprovalProcessKeys(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	return invalidField("APPROVAL_NODE_KIND_UNSUPPORTED", keys[0],
		"quy trình có khoá %s mà engine phê duyệt hiện tại chưa chạy được — bỏ khoá này (sẽ mở ở lát sau)", strings.Join(keys, ", "))
}

// UnsupportedApprovalStepKeys is the step-level twin: keys outside the v1 step shape on step i
// (0-based) → APPROVAL_NODE_KIND_UNSUPPORTED at steps[i].<key>. nil/empty → nil. The validator
// runs it per step; the pack install runs it before any write (vertical_pack.buildPlan).
// ApprovalStepShapeError is the coded refusal for a step whose approvers/completion
// bpm.CheckApprovers cannot run — ONE body for the API validator and the pack gate.
func ApprovalStepShapeError(i int, field, reason string) error {
	code := "APPROVAL_STEP_INVALID"
	if field == "completion.percent" {
		code = "APPROVAL_QUORUM_INVALID" // a quorum that cannot run has its own code (plan "Validator")
	}
	return invalidField(code, fmt.Sprintf("steps[%d].%s", i, field), "bước %d: %s", i+1, reason)
}

func UnsupportedApprovalStepKeys(i int, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	return invalidField("APPROVAL_NODE_KIND_UNSUPPORTED", fmt.Sprintf("steps[%d].%s", i, keys[0]),
		"bước %d có khoá %s mà engine phê duyệt hiện tại chưa chạy được — bỏ khoá này (sẽ mở ở lát sau)",
		i+1, strings.Join(keys, ", "))
}

// ApprovalCriteriaRuleInput is one criteria predicate (reuses the workflow condition vocabulary).
type ApprovalCriteriaRuleInput struct {
	Field string
	Op    string
	Value any
}

// CreateApprovalProcessInput is the payload to create an approval process.
type CreateApprovalProcessInput struct {
	Name       string
	EntityName string
	Trigger    string
	Match      string
	Rules      []ApprovalCriteriaRuleInput
	Steps      []ApprovalStepInput
	// Outcome (APPROVAL-OUTCOME): the field write-back a terminal decision performs. nil = the
	// decision changes nothing about the record, which is how every process behaved before.
	Outcome *ApprovalOutcomeInput
	// Options are the process-level settings (specs/043 S6a): the fallback assignee. nil = none.
	Options  *ApprovalOptionsInput
	IsActive *bool // defaults to true when nil
	ActorID  string
	// Graph is the chain in graph form (specs/043 S3a2) — instead of Steps, never beside them.
	Graph *ApprovalGraphInput
	// Unsupported: process-level keys the caller sent outside ApprovalProcessKeys (options…),
	// filled by the decoder that saw the raw body; refused with APPROVAL_NODE_KIND_UNSUPPORTED.
	Unsupported []string
}

// ApprovalOutcomeInput is the API shape of the terminal write-back.
type ApprovalOutcomeInput struct {
	StatusField   string
	ApprovedValue string
	RejectedValue string
	// ReturnedValue is written when a rejection returns the record to its submitter (S8).
	ReturnedValue string
}

// UpdateApprovalProcessInput is the full-replace payload to update an approval process.
type UpdateApprovalProcessInput struct {
	Name       string
	EntityName string
	Trigger    string
	Match      string
	Rules      []ApprovalCriteriaRuleInput
	Steps      []ApprovalStepInput
	IsActive   *bool
	ActorID    string
	Outcome    *ApprovalOutcomeInput
	// Options: see CreateApprovalProcessInput.Options.
	Options *ApprovalOptionsInput
	// Graph: see CreateApprovalProcessInput.Graph.
	Graph *ApprovalGraphInput
	// Force: a linear-only body (Graph nil) may replace a stored graph. Off, that save is refused
	// with 409 APPROVAL_PROCESS_V2_OVERWRITE (FR-004) — ?force=true on the API.
	Force bool
	// Unsupported: see CreateApprovalProcessInput.Unsupported.
	Unsupported []string
}

// ---- process CRUD ---------------------------------------------------------------------------

// ListProcesses returns every approval process in the tenant (admin list page).
func (s *ApprovalService) ListProcesses(ctx context.Context, tenantID string) ([]bpm.ProcessRecord, error) {
	rows, err := s.procRepo.ListProcesses(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.ListProcesses: %w", err)
	}
	return rows, nil
}

// GetProcess returns one tenant-scoped process, or APPROVAL_PROCESS_NOT_FOUND.
func (s *ApprovalService) GetProcess(ctx context.Context, tenantID, id string) (*bpm.ProcessRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("APPROVAL_PROCESS_ID_REQUIRED", "id quy trình phê duyệt là bắt buộc")
	}
	rec, err := s.procRepo.GetProcess(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, bpm.ErrProcessNotFound) {
			return nil, invalid("APPROVAL_PROCESS_NOT_FOUND", "không tìm thấy quy trình phê duyệt")
		}
		return nil, fmt.Errorf("services.ApprovalService.GetProcess: %w", err)
	}
	return rec, nil
}

// CreateProcess validates and inserts an approval process. name/entityName/trigger are required;
// the trigger must be after_create/after_update/manual; the entity must exist; criteria operators and
// every step's approver are validated. At least one step is required. Returns the created row.
// Config-shape validation runs through the exported ValidateApprovalProcess — the SAME single
// body the AI Builder's propose_approval tool runs (validator-parity: a draft that buffers is a

// approvalOptionsJSON validates and marshals the process options (specs/043 S6a). nil in, an empty block
// or one with no sub-key → nil out (no options).
func approvalOptionsJSON(in *ApprovalOptionsInput) ([]byte, error) {
	if in == nil {
		return nil, nil
	}
	if len(in.Unsupported) > 0 {
		// the same refusal as every key of a later slice (D-7): the engine does not run it yet
		return nil, invalidField("APPROVAL_NODE_KIND_UNSUPPORTED", "options."+in.Unsupported[0],
			"options có khoá %s mà engine phê duyệt hiện tại chưa chạy được — bỏ khoá này (sẽ mở ở lát sau)", strings.Join(in.Unsupported, ", "))
	}
	o := bpm.Options{Lane: strings.TrimSpace(in.Lane), CalendarID: in.CalendarID, PauseWhen: in.PauseWhen}
	if in.FallbackAssignee != nil {
		o.FallbackAssignee = &bpm.Approver{
			Type: bpm.ApproverType(strings.TrimSpace(in.FallbackAssignee.Type)), ID: strings.TrimSpace(in.FallbackAssignee.ID),
		}
	}
	if o.Lane == bpm.LaneExclusive {
		o.Lane = "" // the default is not stored
	}
	if o.Lane == "" && o.FallbackAssignee == nil && o.CalendarID == "" && o.PauseWhen == nil {
		return nil, nil
	}
	if f, reason := bpm.CheckOptions(&o); f != "" {
		return nil, invalidField("APPROVAL_OPTIONS_INVALID", "options."+f, "%s", reason)
	}
	b, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("services.approvalOptionsJSON: %w", err)
	}
	return b, nil
}

// checkLaneForm judges the tracking lane of a body (S12b): graph form, a record-event trigger and
// machine nodes only — the SAME bpm.CheckLane the engine uses, so save and run cannot disagree.
func checkLaneForm(optJSON []byte, trigger bpm.Trigger, g *ApprovalGraphInput) error {
	if len(optJSON) == 0 {
		return nil
	}
	var o bpm.Options
	if err := json.Unmarshal(optJSON, &o); err != nil {
		return fmt.Errorf("services.checkLaneForm: %w", err)
	}
	var kinds []bpm.NodeKind
	if g != nil {
		kinds = make([]bpm.NodeKind, len(g.Nodes))
		for i, n := range g.Nodes {
			kinds[i] = bpm.NodeKind(n.Kind)
		}
	}
	f, reason := bpm.CheckLane(&o, trigger, kinds)
	if f == "" {
		return nil
	}
	if strings.HasPrefix(f, "graph.nodes") {
		return invalidField("APPROVAL_LANE_HUMAN_NODE", f, "%s", reason)
	}
	return invalidField("APPROVAL_OPTIONS_INVALID", "options.lane", "%s", reason)
}

// outcomeJSON validates and marshals the terminal write-back. nil in → nil out (no write-back).
//
// The one hard rule: a value with no field to write it to is a configuration that silently does
// nothing, and silently-does-nothing is the failure mode this whole area keeps producing. Reject it
// at config time so the operator learns now instead of after the first approval changes nothing.
// The VALUE itself is not checked here — whether "Chấp nhận" is a valid option of that picklist is
// the record layer's question, and it is asked at write time by the same validator a human edit
// goes through, so there is exactly one answer rather than two that can drift.
func outcomeJSON(in *ApprovalOutcomeInput) ([]byte, error) {
	if in == nil {
		return nil, nil
	}
	field := strings.TrimSpace(in.StatusField)
	approved := strings.TrimSpace(in.ApprovedValue)
	rejected := strings.TrimSpace(in.RejectedValue)
	returned := strings.TrimSpace(in.ReturnedValue)
	if field == "" {
		if approved == "" && rejected == "" && returned == "" {
			return nil, nil // an entirely empty outcome block is simply "no write-back"
		}
		return nil, invalidField("APPROVAL_OUTCOME_FIELD_REQUIRED", "statusField",
			"đã đặt giá trị ghi-ngược nhưng thiếu statusField — cấu hình này sẽ không ghi gì cả")
	}
	if approved == "" && rejected == "" && returned == "" {
		return nil, invalidField("APPROVAL_OUTCOME_VALUE_REQUIRED", "approvedValue",
			"đã chọn field ghi-ngược nhưng không có giá trị nào để ghi")
	}
	b, err := json.Marshal(bpm.Outcome{StatusField: field, ApprovedValue: approved, RejectedValue: rejected, ReturnedValue: returned})
	if err != nil {
		return nil, fmt.Errorf("services.outcomeJSON: %w", err)
	}
	return b, nil
}

// draft this apply path accepts).
func (s *ApprovalService) CreateProcess(ctx context.Context, tenantID string, in CreateApprovalProcessInput) (*bpm.ProcessRecord, error) {
	if err := UnsupportedApprovalProcessKeys(in.Unsupported); err != nil {
		return nil, err
	}
	trigger, critJSON, stepsJSON, graphJSON, err := validateApprovalProcessForm(in.Name, in.Trigger, in.Match, in.Rules, in.Steps, in.Graph, nil)
	if err != nil {
		return nil, err
	}
	entity, err := s.validateEntity(ctx, tenantID, in.EntityName)
	if err != nil {
		return nil, err
	}
	nodes, at := nodesOf(in.Steps, in.Graph)
	if err := s.checkRefs(ctx, tenantID, entity, nodes, at, in.Options); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	isActive := true
	if in.IsActive != nil {
		isActive = *in.IsActive
	}
	outJSON, err := outcomeJSON(in.Outcome)
	if err != nil {
		return nil, err
	}
	if err := CheckReturnedValueUsed(in.Outcome, in.Steps, in.Graph); err != nil {
		return nil, err
	}
	optJSON, err := approvalOptionsJSON(in.Options)
	if err != nil {
		return nil, err
	}
	if err := checkLaneForm(optJSON, trigger, in.Graph); err != nil {
		return nil, err
	}
	if err := ApprovalOutcomeSelfTrigger(string(trigger), in.Rules, in.Outcome); err != nil {
		return nil, err
	}
	id, err := s.procRepo.CreateProcess(ctx, bpm.CreateProcessInput{
		TenantID:   tenantID,
		Name:       name,
		EntityName: entity,
		Trigger:    trigger,
		Criteria:   critJSON,
		Steps:      stepsJSON,
		Graph:      graphJSON,
		Outcome:    outJSON,
		Options:    optJSON,
		IsActive:   isActive,
		CreatedBy:  strings.TrimSpace(in.ActorID),
	})
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.CreateProcess: %w", err)
	}
	return s.procRepo.GetProcess(ctx, tenantID, id)
}

// preparedProcess is a process body that passed every config-time check, in storage form.
type preparedProcess struct {
	name, entity                             string
	trigger                                  bpm.Trigger
	criteria, steps, graph, outcome, options []byte
}

// prepareProcessBody validates an existing process's full-replace body — the ONE body behind the
// legacy save (UpdateProcess) and the draft save (SaveDraft), so a draft can never hold what a save
// would refuse. Ids it must assign are kept off the ids the process has already published (specs/043
// S10, "Nợ còn mở" 15).
func (s *ApprovalService) prepareProcessBody(ctx context.Context, tenantID, id string, in UpdateApprovalProcessInput) (*preparedProcess, error) {
	if err := UnsupportedApprovalProcessKeys(in.Unsupported); err != nil {
		return nil, err
	}
	reserved, err := s.procRepo.PublishedNodeIDs(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.prepareProcessBody: %w", err)
	}
	trigger, critJSON, stepsJSON, graphJSON, err := validateApprovalProcessForm(in.Name, in.Trigger, in.Match, in.Rules, in.Steps, in.Graph, reserved)
	if err != nil {
		return nil, err
	}
	entity, err := s.validateEntity(ctx, tenantID, in.EntityName)
	if err != nil {
		return nil, err
	}
	nodes, at := nodesOf(in.Steps, in.Graph)
	if err := s.checkRefs(ctx, tenantID, entity, nodes, at, in.Options); err != nil {
		return nil, err
	}
	outJSON, err := outcomeJSON(in.Outcome)
	if err != nil {
		return nil, err
	}
	if err := CheckReturnedValueUsed(in.Outcome, in.Steps, in.Graph); err != nil {
		return nil, err
	}
	optJSON, err := approvalOptionsJSON(in.Options)
	if err != nil {
		return nil, err
	}
	if err := checkLaneForm(optJSON, trigger, in.Graph); err != nil {
		return nil, err
	}
	if err := ApprovalOutcomeSelfTrigger(string(trigger), in.Rules, in.Outcome); err != nil {
		return nil, err
	}
	return &preparedProcess{
		name: strings.TrimSpace(in.Name), entity: entity, trigger: trigger,
		criteria: critJSON, steps: stepsJSON, graph: graphJSON, outcome: outJSON, options: optJSON,
	}, nil
}

// mapProcessWriteErr turns the repository's process-write sentinels into coded API errors.
func mapProcessWriteErr(err error, op string) error {
	switch {
	case errors.Is(err, bpm.ErrProcessNotFound):
		return invalid("APPROVAL_PROCESS_NOT_FOUND", "không tìm thấy quy trình phê duyệt")
	case errors.Is(err, bpm.ErrProcessGraphOverwrite):
		return invalidField("APPROVAL_PROCESS_V2_OVERWRITE", "steps",
			"quy trình này đang lưu cấu hình mà bản gửi lên không mang — dạng đồ thị (graph) hoặc người dự phòng (options) — nên lưu lại sẽ XOÁ nó. Sửa bằng trình dựng đồ thị / gửi kèm đủ options, hoặc gửi lại kèm ?force=true nếu chủ ý bỏ")
	default:
		return fmt.Errorf("services.ApprovalService.%s: %w", op, err)
	}
}

// UpdateProcess re-validates and replaces a tenant-scoped process: the LEGACY save, which is "save
// and publish now" (specs/043 FR-043) — a changed definition becomes the next version, an unchanged
// one publishes nothing. The admin builder, the AI Builder and the pack install all come through
// here. Unknown id → NOT_FOUND. Config-shape validation shares ONE body with CreateProcess
// (ValidateApprovalProcess).
func (s *ApprovalService) UpdateProcess(ctx context.Context, tenantID, id string, in UpdateApprovalProcessInput) (*bpm.ProcessRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("APPROVAL_PROCESS_ID_REQUIRED", "id quy trình phê duyệt là bắt buộc")
	}
	p, err := s.prepareProcessBody(ctx, tenantID, id, in)
	if err != nil {
		return nil, err
	}
	isActive := true
	if in.IsActive != nil {
		isActive = *in.IsActive
	}
	if err := s.procRepo.UpdateProcess(ctx, tenantID, id, bpm.UpdateProcessInput{
		Name:       p.name,
		EntityName: p.entity,
		Trigger:    p.trigger,
		Criteria:   p.criteria,
		Steps:      p.steps,
		Graph:      p.graph,
		Outcome:    p.outcome,
		Options:    p.options,
		IsActive:   isActive,
		UpdatedBy:  strings.TrimSpace(in.ActorID),
		Force:      in.Force,
	}); err != nil {
		return nil, mapProcessWriteErr(err, "UpdateProcess")
	}
	return s.procRepo.GetProcess(ctx, tenantID, id)
}

// ---- draft / publish / history (specs/043 S10, FR-043) ----------------------------------------

// maxApprovalVersionNoteRunes is the width of sys_approval_process_versions.note.
const maxApprovalVersionNoteRunes = 500

// ApprovalPublishResult is the answer of a publish: the process now running the new version.
type ApprovalPublishResult struct {
	Process *bpm.ProcessRecord `json:"process"`
	Version int                `json:"version"`
	// Impact: how many hồ sơ still run an older version (specs/043 S11). nil when it could not be
	// read — the publish is committed either way, so a failed read is a missing hint, not an error.
	Impact *bpm.Impact `json:"impact"`
}

// SaveDraft stores an unpublished edit of a process. The body is the SAME full-replace body as a
// save and is validated by the same code, but nothing the engine reads changes: hồ sơ mới vẫn chạy
// bản đang dùng until PublishDraft. A second save replaces the first draft.
func (s *ApprovalService) SaveDraft(ctx context.Context, tenantID, id string, in UpdateApprovalProcessInput) (*bpm.ProcessRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("APPROVAL_PROCESS_ID_REQUIRED", "id quy trình phê duyệt là bắt buộc")
	}
	p, err := s.prepareProcessBody(ctx, tenantID, id, in)
	if err != nil {
		return nil, err
	}
	if err := s.procRepo.SaveDraft(ctx, tenantID, id, bpm.SaveDraftInput{
		Doc: bpm.DefinitionDoc{
			Name: p.name, EntityName: p.entity, Trigger: string(p.trigger),
			Criteria: p.criteria, Steps: p.steps, Graph: p.graph, Outcome: p.outcome, Options: p.options,
		},
		IsActive: in.IsActive,
		By:       strings.TrimSpace(in.ActorID),
		Force:    in.Force,
	}); err != nil {
		return nil, mapProcessWriteErr(err, "SaveDraft")
	}
	return s.procRepo.GetProcess(ctx, tenantID, id)
}

// DiscardDraft drops the unpublished edit; the active version is untouched.
func (s *ApprovalService) DiscardDraft(ctx context.Context, tenantID, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return invalid("APPROVAL_PROCESS_ID_REQUIRED", "id quy trình phê duyệt là bắt buộc")
	}
	if err := s.procRepo.DiscardDraft(ctx, tenantID, id); err != nil {
		if errors.Is(err, bpm.ErrDraftNotFound) {
			return invalid("APPROVAL_DRAFT_NOT_FOUND", "quy trình này không có bản nháp")
		}
		return mapProcessWriteErr(err, "DiscardDraft")
	}
	return nil
}

// PublishDraft makes the draft the active definition as the next version. New hồ sơ pin it from
// that moment; hồ sơ đang chờ keep the version they started under (S11 moves them on request).
func (s *ApprovalService) PublishDraft(ctx context.Context, tenantID, id, note, actorID string) (*ApprovalPublishResult, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("APPROVAL_PROCESS_ID_REQUIRED", "id quy trình phê duyệt là bắt buộc")
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxApprovalVersionNoteRunes {
		return nil, invalidField("APPROVAL_VERSION_NOTE_TOO_LONG", "note", "ghi chú phiên bản vượt %d ký tự", maxApprovalVersionNoteRunes)
	}
	rec, err := s.GetProcess(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if len(rec.Draft) == 0 {
		return nil, invalid("APPROVAL_VERSION_NO_DRAFT", "quy trình này không có bản nháp để xuất bản")
	}
	// The draft was validated when it was saved; what can have changed since is the world around
	// it — the entity it targets may be gone. Judged again here, not discovered by the engine.
	var d bpm.DraftDoc
	if err := json.Unmarshal(rec.Draft, &d); err != nil {
		return nil, fmt.Errorf("services.ApprovalService.PublishDraft: decode draft: %w", err)
	}
	if _, err := s.validateEntity(ctx, tenantID, d.EntityName); err != nil {
		return nil, err
	}
	version, err := s.procRepo.Publish(ctx, tenantID, id, note, strings.TrimSpace(actorID))
	if err != nil {
		switch {
		case errors.Is(err, bpm.ErrNoDraft):
			return nil, invalid("APPROVAL_VERSION_NO_DRAFT", "quy trình này không có bản nháp để xuất bản")
		case errors.Is(err, bpm.ErrDraftStale):
			return nil, invalid("APPROVAL_DRAFT_STALE",
				"quy trình đã có phiên bản mới hơn bản nháp này (một lần lưu hoặc áp dụng khác đã đổi nó) — lưu lại bản nháp để dựa trên bản hiện tại rồi xuất bản")
		}
		return nil, mapProcessWriteErr(err, "PublishDraft")
	}
	out, err := s.procRepo.GetProcess(ctx, tenantID, id)
	if err != nil {
		return nil, mapProcessWriteErr(err, "PublishDraft")
	}
	return &ApprovalPublishResult{Process: out, Version: version, Impact: s.impactHint(ctx, tenantID, id)}, nil
}

// ---- impact + repin (specs/043 S11, D7 phương án a) --------------------------------------------

// impactHint is the "N hồ sơ still run the old version" answer attached to a save or publish that
// is already committed: best-effort (nil when unreadable) — the admin loses a hint, not the save.
func (s *ApprovalService) impactHint(ctx context.Context, tenantID, id string) *bpm.Impact {
	im, err := s.procRepo.ProcessImpact(ctx, tenantID, strings.TrimSpace(id))
	if err != nil {
		return nil
	}
	return im
}

// SaveImpact is impactHint for the legacy save handler (PUT returns it beside the process).
func (s *ApprovalService) SaveImpact(ctx context.Context, tenantID, id string) *bpm.Impact {
	return s.impactHint(ctx, tenantID, id)
}

// ProcessImpact is GET /:id/impact: pending hồ sơ of the process, how many run an older version,
// and on which step they stand. Read it BEFORE deleting: a deleted process answers not-found.
func (s *ApprovalService) ProcessImpact(ctx context.Context, tenantID, id string) (*bpm.Impact, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("APPROVAL_PROCESS_ID_REQUIRED", "id quy trình phê duyệt là bắt buộc")
	}
	im, err := s.procRepo.ProcessImpact(ctx, tenantID, id)
	if err != nil {
		return nil, mapProcessWriteErr(err, "ProcessImpact")
	}
	return im, nil
}

var approvalInstanceStatuses = map[string]bool{"pending": true, "approved": true, "rejected": true, "cancelled": true}

// WithRetrier arms "Thử lại" on a hồ sơ stalled on an automatic step (specs/043 S7). Chainable.
func (s *ApprovalService) WithRetrier(r approvalRetrier) *ApprovalService {
	s.retrier = r
	return s
}

// RetryApproval is POST /admin/approval-instances/:id/retry: the Nexus team fixed what stalled an
// automatic step (the process, or the record's data) and presses "Thử lại". Admin only — the route
// group gates it and the check is repeated here, so no other caller can reach it. Not stalled ⇒
// 409 APPROVAL_NOT_STUCK; finished ⇒ 409 APPROVAL_NOT_PENDING.
func (s *ApprovalService) RetryApproval(ctx context.Context, tenantID, id, note string, actor bpm.Actor) (*bpm.Instance, error) {
	if !actor.IsAdmin {
		return nil, mapInstanceErr(bpm.ErrNotAuthorized)
	}
	if s.retrier == nil {
		return nil, invalid("APPROVAL_RETRY_UNAVAILABLE", "chức năng thử lại chưa được cấu hình")
	}
	inst, err := s.retrier.Retry(ctx, tenantID, strings.TrimSpace(id), actor, strings.TrimSpace(note))
	if err != nil {
		if mapped := mapInstanceErr(err); mapped != nil {
			return nil, mapped
		}
		return nil, fmt.Errorf("services.ApprovalService.RetryApproval: %w", err)
	}
	return inst, nil
}

// CancelApproval is POST /admin/approval-instances/:id/cancel: the Nexus team stops a hồ sơ for good
// (typically one stalled on an automatic step that is not worth fixing). It is the submitter's
// recall done by an admin — the same transition, history line and closed seats — so there is one
// way an approval is withdrawn, not two. Admin only (repeated here, as for retry).
func (s *ApprovalService) CancelApproval(ctx context.Context, tenantID, id, note string, actor bpm.Actor) (*bpm.Instance, error) {
	if !actor.IsAdmin {
		return nil, mapInstanceErr(bpm.ErrNotAuthorized)
	}
	return s.RecallApproval(ctx, tenantID, strings.TrimSpace(id), strings.TrimSpace(note), actor)
}

// ListProcessInstances is GET /:id/instances: the hồ sơ of a process, optionally those running one
// version (0 = unknown). status "" = pending. stuck=true keeps only those stalled on an automatic
// step with an error (specs/043 S7).
func (s *ApprovalService) ListProcessInstances(ctx context.Context, tenantID, id string, version *int, status string, stuck bool, limit int) ([]bpm.ProcessInstanceRow, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("APPROVAL_PROCESS_ID_REQUIRED", "id quy trình phê duyệt là bắt buộc")
	}
	if status != "" && !approvalInstanceStatuses[status] {
		return nil, invalidField("APPROVAL_INSTANCE_STATUS_INVALID", "status", "status phải là pending, approved, rejected hoặc cancelled")
	}
	if version != nil && *version < 0 {
		return nil, invalid("APPROVAL_VERSION_NOT_FOUND", "không tìm thấy phiên bản này của quy trình")
	}
	rows, err := s.procRepo.ListProcessInstances(ctx, tenantID, id, version, status, stuck, limit)
	if err != nil {
		return nil, mapProcessWriteErr(err, "ListProcessInstances")
	}
	if rows == nil {
		rows = []bpm.ProcessInstanceRow{}
	}
	return rows, nil
}

// RepinInstances is POST /:id/repin: move the named hồ sơ (or every older one, MaxRepinBatch at a
// time) onto a version of the process — a dry run first, then for real. A row that cannot move
// says why; the rest still move. Admin-only (the route group), never automatic.
func (s *ApprovalService) RepinInstances(ctx context.Context, tenantID, id string, in bpm.RepinInput) (*bpm.RepinOutcome, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("APPROVAL_PROCESS_ID_REQUIRED", "id quy trình phê duyệt là bắt buộc")
	}
	if in.ToVersion < 0 {
		return nil, invalid("APPROVAL_VERSION_NOT_FOUND", "không tìm thấy phiên bản này của quy trình")
	}
	out, err := s.procRepo.Repin(ctx, tenantID, id, in)
	if err != nil {
		switch {
		case errors.Is(err, bpm.ErrVersionNotFound):
			return nil, invalid("APPROVAL_VERSION_NOT_FOUND", "không tìm thấy phiên bản này của quy trình")
		case errors.Is(err, bpm.ErrRepinBatchTooLarge):
			return nil, invalidField("APPROVAL_REPIN_TOO_MANY", "instanceIds", "tối đa %d hồ sơ mỗi lần chuyển", bpm.MaxRepinBatch)
		}
		return nil, mapProcessWriteErr(err, "RepinInstances")
	}
	return out, nil
}

// ListVersions returns the published versions of a process, newest first (no definitions).
func (s *ApprovalService) ListVersions(ctx context.Context, tenantID, id string) ([]bpm.VersionRecord, error) {
	if _, err := s.GetProcess(ctx, tenantID, id); err != nil {
		return nil, err
	}
	rows, err := s.procRepo.ListVersions(ctx, tenantID, strings.TrimSpace(id))
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.ListVersions: %w", err)
	}
	if rows == nil {
		rows = []bpm.VersionRecord{}
	}
	return rows, nil
}

// GetVersion returns one published version with its definition.
func (s *ApprovalService) GetVersion(ctx context.Context, tenantID, id string, version int) (*bpm.VersionRecord, error) {
	if _, err := s.GetProcess(ctx, tenantID, id); err != nil {
		return nil, err
	}
	rec, err := s.procRepo.GetVersion(ctx, tenantID, strings.TrimSpace(id), version)
	if err != nil {
		if errors.Is(err, bpm.ErrVersionNotFound) {
			return nil, invalid("APPROVAL_VERSION_NOT_FOUND", "không tìm thấy phiên bản này của quy trình")
		}
		return nil, fmt.Errorf("services.ApprovalService.GetVersion: %w", err)
	}
	return rec, nil
}

// DeleteProcess soft-deletes a tenant-scoped process. Unknown id → NOT_FOUND.
func (s *ApprovalService) DeleteProcess(ctx context.Context, tenantID, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return invalid("APPROVAL_PROCESS_ID_REQUIRED", "id quy trình phê duyệt là bắt buộc")
	}
	if err := s.procRepo.DeleteProcess(ctx, tenantID, id); err != nil {
		if errors.Is(err, bpm.ErrProcessNotFound) {
			return invalid("APPROVAL_PROCESS_NOT_FOUND", "không tìm thấy quy trình phê duyệt")
		}
		return fmt.Errorf("services.ApprovalService.DeleteProcess: %w", err)
	}
	return nil
}

// ---- user approval surface ------------------------------------------------------------------

// ListPending returns the approval instances awaiting THIS user's decision: those targeting the
// user directly OR one of the roles they hold. roleIDs is the user's role set (the JWT subject's
// role id, plus any others the caller resolves). tenant-pinned.
func (s *ApprovalService) ListPending(ctx context.Context, tenantID string, actor bpm.Actor) ([]bpm.Instance, error) {
	userID := strings.TrimSpace(actor.UserID)
	if userID == "" {
		return nil, invalid("APPROVAL_USER_REQUIRED", "thiếu người dùng cho danh sách chờ duyệt")
	}
	actor = s.withQueueView(ctx, tenantID, actor)
	rows, err := s.instRepo.ListPendingForUser(ctx, tenantID, userID, actor.HolderIDs(), actor.QueueView)
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.ListPending: %w", err)
	}
	if !actor.IsAdmin {
		rows = publicHistoryRows(rows)
	}
	return rows, nil
}

// GetInstancesForRecord returns all approval instances for (entity, recordID) in the tenant,
// all statuses, newest first. Returns an empty slice (no error) when no process has ever been
// attached to the record. tenantID pins the query — no cross-tenant reads.
func (s *ApprovalService) GetInstancesForRecord(ctx context.Context, scope RecordScope, tenantID, entityName, recordID string) ([]bpm.Instance, error) {
	entityName, recordID, err := s.recordGate(ctx, scope, tenantID, entityName, recordID)
	if err != nil {
		return nil, err
	}
	rows, err := s.instRepo.ListByRecord(ctx, tenantID, entityName, recordID)
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.GetInstancesForRecord: %w", err)
	}
	if rows == nil {
		rows = []bpm.Instance{}
	}
	if !scope.IsAdmin {
		rows = publicHistoryRows(rows)
	}
	return rows, nil
}

// recordGate is the read gate of the per-record approval reads: ids present, and — for a caller who
// is not an admin — the record itself readable under their row scope. Returns the trimmed ids.
func (s *ApprovalService) recordGate(ctx context.Context, scope RecordScope, tenantID, entityName, recordID string) (string, string, error) {
	entityName = strings.TrimSpace(entityName)
	recordID = strings.TrimSpace(recordID)
	if entityName == "" || recordID == "" {
		return "", "", invalid("APPROVAL_RECORD_REQUIRED", "entity và record id là bắt buộc")
	}
	// Entity-level "view" is NOT row-level permission. A user with view:own or view:team passed the
	// route ACL for the ENTITY, but the specific record may be a colleague's — and an approval
	// timeline is exactly the sensitive material row-scope hides: who signed off a deal, a
	// reject reason like "khách mất khả năng thanh toán", who a step was delegated to. The audit
	// history read (WAVE 12) already probes the row this way; this sibling read was missed.
	//
	// Fail closed when the probe is unwired for a non-admin — never serve an unscoped timeline.
	if !scope.IsAdmin {
		if s.records == nil {
			return "", "", invalid("APPROVAL_REQUEST_UNAVAILABLE", "chức năng phê duyệt chưa được cấu hình")
		}
		if _, err := s.records.Get(ctx, scope, tenantID, entityName, recordID, nil, nil); err != nil {
			return "", "", err // the record read's own refusal (404 for an invisible/absent row) — no oracle
		}
	}
	return entityName, recordID, nil
}

// RecordSLAItem is one tracking-lane (SLA) line of a record as the record page's SLA card reads it
// (specs/043 S12b): the step in words, its deadline and what became of it. No history, no ids of
// the engine's vocabulary beyond the instance id.
type RecordSLAItem struct {
	ID       string    `json:"id"`
	StepName string    `json:"stepName"`
	Status   string    `json:"status"`
	SLA      RecordSLA `json:"sla"`
}

// RecordSLA is the clock of a RecordSLAItem. Paused: the clock is frozen (S13) — DueAt is then null and RemainingSeconds holds the working time left.
type RecordSLA struct {
	DueAt  *time.Time `json:"dueAt"`
	Basis  string     `json:"basis,omitempty"`
	Paused bool       `json:"paused"`
	// RemainingSeconds: the time left on a frozen clock (absent while it runs).
	RemainingSeconds *int64 `json:"remainingSeconds,omitempty"`
	Met              bool   `json:"met"`
	Breached         bool   `json:"breached"`
}

// GetRecordSLA returns the tracking-lane instances of (entity, record), newest first, behind the
// SAME gate as GetInstancesForRecord (a record the caller cannot see is as absent as ever).
func (s *ApprovalService) GetRecordSLA(ctx context.Context, scope RecordScope, tenantID, entityName, recordID string) ([]RecordSLAItem, error) {
	entityName, recordID, err := s.recordGate(ctx, scope, tenantID, entityName, recordID)
	if err != nil {
		return nil, err
	}
	rows, err := s.instRepo.ListByRecordLane(ctx, tenantID, entityName, recordID, bpm.LaneIndependent)
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.GetRecordSLA: %w", err)
	}
	out := make([]RecordSLAItem, 0, len(rows))
	for _, in := range rows {
		out = append(out, slaItem(in))
	}
	return out, nil
}

// slaItem reads the clock off an instance's history: the newest waiting/wait_met line says whether
// the wait is still open or answered; any escalated / no-recipient line says the deadline passed.
func slaItem(in bpm.Instance) RecordSLAItem {
	it := RecordSLAItem{ID: in.ID, Status: string(in.Status), SLA: RecordSLA{DueAt: in.SLADueAt, Paused: in.SLAPausedAt != nil, RemainingSeconds: in.SLARemainingSeconds}}
	seen := false
	for i := len(in.History) - 1; i >= 0; i-- {
		h := in.History[i]
		switch h.Action {
		case bpm.HistoryEscalated, bpm.HistoryBreachNoRecipient:
			it.SLA.Breached = true
		case bpm.HistoryWaiting, bpm.HistoryWaitMet:
			if !seen {
				seen = true
				it.SLA.Met = h.Action == bpm.HistoryWaitMet
				it.StepName = h.StepName
			}
			if h.Action == bpm.HistoryWaiting && it.SLA.Basis == "" {
				it.SLA.Basis = string(h.SLABasis) // only the resting line carries the basis
			}
		}
	}
	if in.Escalated {
		it.SLA.Breached = true
	}
	if it.StepName == "" {
		_, it.StepName = in.Definition.StepRef(in.CurrentStep)
	}
	return it
}

// publicHistoryRows returns the instances with the machine's own history lines left out (what a
// person who is not an admin may read). A copy: the rows a store returned are never written to.
func publicHistoryRows(rows []bpm.Instance) []bpm.Instance {
	out := make([]bpm.Instance, len(rows))
	copy(out, rows)
	for i := range out {
		out[i].History = bpm.PublicHistory(out[i].History)
	}
	return out
}

// ApprovalProcessContext is the trimmed process view embedded in an approval detail (W6 inbox
// deep-link): enough for the FE to render the step timeline (names, approvers, SLAs) without a
// second admin-gated request. Steps is the raw steps JSON array forwarded verbatim.
type ApprovalProcessContext struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	EntityName string          `json:"entityName"`
	Trigger    string          `json:"trigger"`
	Steps      json.RawMessage `json:"steps"`
	IsActive   bool            `json:"isActive"`
	// Deleted: the live process row is gone but the instance still carries its pinned steps
	// (which the engine keeps walking) — name/trigger are unknown, steps are authoritative.
	Deleted bool `json:"deleted,omitempty"`
	// Pinned: `steps` is the copy this instance pinned at arm time, NOT the live process. Set
	// whenever a pin exists, including when the live row is alive and was edited since — without
	// it the screen presents a stale chain as if it were current, which is how a mis-set approval
	// chain stays mis-set (the admin builder shows the new chain, this shows the old one, and
	// neither says which one the engine will actually walk).
	Pinned bool `json:"pinned,omitempty"`
	// Version is the process version this instance runs under — the one it pinned when it started
	// (specs/043 S10). Absent for an instance armed before versions existed.
	Version int `json:"version,omitempty"`
}

// ApprovalDetail is the GET /approvals/:id payload: the instance (camelCase — NOTE: unlike the
// legacy /approvals/pending list, which serializes bpm.Instance with Go field names, this NEW
// endpoint follows the API camelCase convention) plus its process/step context and the per-step
// history timeline.
type ApprovalDetail struct {
	ID                  string     `json:"id"`
	ProcessID           string     `json:"processId"`
	EntityName          string     `json:"entityName"`
	RecordID            string     `json:"recordId"`
	CurrentStep         int        `json:"currentStep"`
	Status              string     `json:"status"`
	CurrentApproverType string     `json:"currentApproverType,omitempty"`
	CurrentApproverID   string     `json:"currentApproverId,omitempty"`
	SLADueAt            *time.Time `json:"slaDueAt"`
	// SLA says what slaDueAt was computed on (specs/043 S1): working hours, or one of the three
	// wall-clock reasons. Absent when the current step has no deadline. slaDueAt stays for every
	// reader that already parses it.
	SLA       *ApprovalSLA            `json:"sla,omitempty"`
	Escalated bool                    `json:"escalated"`
	History   []bpm.HistoryEntry      `json:"history"`
	CreatedBy string                  `json:"createdBy,omitempty"`
	CreatedAt time.Time               `json:"createdAt"`
	UpdatedAt time.Time               `json:"updatedAt"`
	Process   *ApprovalProcessContext `json:"process"`
	// Gateway is the arm each gateway the approval passed took (node id → branch name, specs/043
	// S7) — the path the screen draws. Absent when it passed none.
	Gateway map[string]string `json:"gateway,omitempty"`
	// Stuck: the approval stands still on an automatic step and an admin must act. LastError is why
	// — engine vocabulary (field names, refused values), so it is shown to admins only.
	Stuck     bool   `json:"stuck,omitempty"`
	LastError string `json:"lastError,omitempty"`
	// Durations answers "how long did this take" (specs/043 S2, #40). Absent only when the server
	// was built without the calendar (WithWorkingTime).
	Durations *ApprovalDurations `json:"durations,omitempty"`
	// Seats are the seats of the step the approval stands on (its latest visit), in any state:
	// who holds it, who decided what (specs/043 S4). Always an array.
	Seats []ApprovalSeat `json:"seats"`
	// Completion is "2/3 — cần 2": present only when the current step is held by several people.
	Completion *ApprovalCompletion `json:"completion,omitempty"`
	// AwaitsMe: the caller holds an open seat of this approval RIGHT NOW (bpm.AwaitsActor — the
	// inbox's own rule, no admin shortcut). The screen offers Duyệt/Từ chối from this, not from the
	// single-holder projection, which is empty on a step with several seats (specs/043 S4).
	AwaitsMe bool `json:"awaitsMe"`
	// AwaitingRework: the approval was sent back to its submitter and waits for "Gửi lại" (specs/043
	// S8) — nobody approves or rejects it until then. ReturnReason is the rejector's note.
	AwaitingRework bool   `json:"awaitingRework,omitempty"`
	ReturnReason   string `json:"returnReason,omitempty"`
}

// ApprovalSeat is one seat of the current visit, as the detail shows it.
type ApprovalSeat struct {
	ID            string     `json:"id"`
	AssigneeType  string     `json:"assigneeType"`
	AssigneeID    string     `json:"assigneeId"`
	Status        string     `json:"status"`            // open | done | cancelled
	Outcome       string     `json:"outcome,omitempty"` // approved | rejected | reassigned | superseded | recalled
	DecidedBy     string     `json:"decidedBy,omitempty"`
	Comment       string     `json:"comment,omitempty"`
	DecidedAt     *time.Time `json:"decidedAt,omitempty"`
	DelegatedFrom string     `json:"delegatedFrom,omitempty"`
	// Kind: approval | task | rework (a returned record waiting for its submitter, specs/043 S8).
	Kind string `json:"kind,omitempty"`
}

// ApprovalCompletion is the tally of a many-seat step: its policy and where the votes stand.
// Required is how many approvals end the step in favour: everyone for "all", one for "any",
// ceil(percent x seats / 100) for "quorum" ("2/3 — cần 2"). Percent echoes the quorum percentage.
type ApprovalCompletion struct {
	Mode     string `json:"mode"`
	Total    int    `json:"total"`
	Approved int    `json:"approved"`
	Rejected int    `json:"rejected"`
	Open     int    `json:"open"`
	Required int    `json:"required"`
	Percent  int    `json:"percent,omitempty"`
}

// ApprovalSLA is the deadline of the current step and its basis (bpm.SLABasis).
type ApprovalSLA struct {
	DueAt *time.Time   `json:"dueAt,omitempty"` // absent while the clock is paused (S13)
	Basis bpm.SLABasis `json:"basis"`
	// Paused + RemainingSeconds: the clock is frozen and this much working time is left.
	Paused           bool   `json:"paused,omitempty"`
	RemainingSeconds *int64 `json:"remainingSeconds,omitempty"`
}

// ApprovalDurations is the elapsed time of an approval: the whole of it (sent → last decision, or →
// now while it is still pending) and each step (opened → decided, or → now for the open one).
//
// Basis says what the business figures were measured on: business (the tenant's working calendar
// — the one the deadlines walk), wall_no_calendar (the tenant has none) or wall_calendar_error
// (the calendar could not be read, or has no working hours at all). Only under business are the
// *BusinessSeconds present: a wall-clock number is never dressed as working time.
type ApprovalDurations struct {
	Basis                bpm.SLABasis           `json:"basis"`
	TotalBusinessSeconds *int64                 `json:"totalBusinessSeconds,omitempty"`
	TotalWallSeconds     int64                  `json:"totalWallSeconds"`
	PerNode              []ApprovalStepDuration `json:"perNode"`
}

// ApprovalStepDuration is one step's stretch. EndedAt nil = the step is still open, measured up to
// now. Node/StepName come from the history entry that closed it (S0), else from the pinned chain.
type ApprovalStepDuration struct {
	Step            int        `json:"step"`
	Node            string     `json:"node,omitempty"`
	StepName        string     `json:"stepName,omitempty"`
	StartedAt       time.Time  `json:"startedAt"`
	EndedAt         *time.Time `json:"endedAt,omitempty"`
	BusinessSeconds *int64     `json:"businessSeconds,omitempty"`
	WallSeconds     int64      `json:"wallSeconds"`
}

// GetApprovalDetail returns one approval instance with its process context for the inbox
// deep-link. VISIBILITY mirrors /approvals/pending exactly: an admin sees any instance in the
// tenant; a non-admin sees it ONLY while they are the CURRENT step's approver (directly or via a
// role they hold). Anyone else gets APPROVAL_INSTANCE_NOT_FOUND — the same answer as a truly
// unknown id, so the endpoint never leaks whether a foreign instance exists. tenant_id pins the
// instance read (cross-tenant ids are not-found by construction).
func (s *ApprovalService) GetApprovalDetail(ctx context.Context, scope RecordScope, tenantID, id string, actor bpm.Actor) (*ApprovalDetail, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("APPROVAL_INSTANCE_ID_REQUIRED", "id phê duyệt là bắt buộc")
	}
	inst, err := s.instRepo.GetInstance(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, bpm.ErrInstanceNotFound) {
			return nil, invalid("APPROVAL_INSTANCE_NOT_FOUND", "không tìm thấy phiếu phê duyệt")
		}
		return nil, fmt.Errorf("services.ApprovalService.GetApprovalDetail: %w", err)
	}
	// Two ways to be allowed to see a detail, and the second was missing: (a) you are the current
	// approver of a pending instance (the inbox contract), OR (b) you can see the RECORD this
	// instance is about. The record's Approvals tab links to /approvals/:id, so without (b) the
	// SUBMITTER 404s on the detail of their OWN submission, and a past approver 404s on a decision
	// they made. Symmetric with GetInstancesForRecord: the record probe is the row-scope gate.
	awaitsMe, err := s.awaitsActor(ctx, tenantID, inst, actor)
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.GetApprovalDetail: %w", err)
	}
	canSee := actor.IsAdmin || awaitsMe
	// The seats of the step the approval stands on. Read once: they are also the proof that an
	// approver who has VOTED on a many-seat step (and so no longer awaits) may still open it (S4).
	visit, err := s.instRepo.ListVisitSeats(ctx, tenantID, inst.ID, inst.NodeID())
	if err != nil {
		return nil, fmt.Errorf("services.ApprovalService.GetApprovalDetail: read seats: %w", err)
	}
	if !canSee {
		canSee = seatedIn(visit, actor)
	}
	if !canSee {
		visibleViaRecord := false
		if s.records != nil && !actor.IsAdmin {
			// RESOLVE FIRST, then probe. /approvals/:id carries no :entity segment, so the ACL stage
			// never ran and the scope that arrived is AccessNone for every non-admin — probing with
			// it made this whole branch dead code, and the submitter kept 404ing on the detail of
			// their OWN submission, which is the exact bug the branch was added to fix. Same shape
			// as files.Service.scopeFor: when the route cannot carry the ACL stage, the SERVICE
			// re-runs it. A refusal here leaves visibleViaRecord false, so the answer stays the
			// no-oracle NOT_FOUND below.
			if sc, aerr := s.records.AuthorizeView(ctx, tenantID, scope, inst.EntityName); aerr == nil {
				if _, rerr := s.records.Get(ctx, sc, tenantID, inst.EntityName, inst.RecordID, nil, nil); rerr == nil {
					visibleViaRecord = true
				}
			}
		}
		if !visibleViaRecord {
			// Same code+message as unknown → no existence oracle for non-participants.
			return nil, invalid("APPROVAL_INSTANCE_NOT_FOUND", "không tìm thấy phiếu phê duyệt")
		}
	}

	detail := &ApprovalDetail{
		ID:                  inst.ID,
		ProcessID:           inst.ProcessID,
		EntityName:          inst.EntityName,
		RecordID:            inst.RecordID,
		CurrentStep:         inst.CurrentStep,
		Status:              string(inst.Status),
		CurrentApproverType: string(inst.CurrentApproverType),
		CurrentApproverID:   inst.CurrentApproverID,
		SLADueAt:            inst.SLADueAt,
		Escalated:           inst.Escalated,
		History:             inst.History,
		CreatedBy:           inst.CreatedBy,
		CreatedAt:           inst.CreatedAt,
		UpdatedAt:           inst.UpdatedAt,
		AwaitsMe:            awaitsMe,
	}
	if !actor.IsAdmin { // the machine's bookkeeping (branch names, written fields, failures) is for the Nexus team
		detail.History = bpm.PublicHistory(detail.History)
	}
	if detail.History == nil {
		detail.History = []bpm.HistoryEntry{} // JSON [] not null — FE renders a timeline
	}
	if g := inst.Context.Gateway(); len(g) > 0 {
		detail.Gateway = g
	}
	if inst.Stuck() {
		detail.Stuck = true
		if actor.IsAdmin {
			detail.LastError = inst.LastError
		}
	}
	detail.Seats = seatViews(visit)
	if inst.Status == bpm.StatusPending {
		for _, v := range visit {
			if v.Status == "open" && v.Kind == bpm.SeatKindRework {
				detail.AwaitingRework, detail.ReturnReason = true, v.Comment
			}
		}
	}
	if inst.SLADueAt != nil {
		detail.SLA = &ApprovalSLA{DueAt: inst.SLADueAt, Basis: inst.CurrentSLABasis()}
	} else if inst.SLAPausedAt != nil { // a frozen clock has no due date but is still an SLA (S13)
		detail.SLA = &ApprovalSLA{Basis: inst.CurrentSLABasis(), Paused: true, RemainingSeconds: inst.SLARemainingSeconds}
	}
	// Measured only now, past the visibility gate: nothing is computed for someone who gets the 404.
	if s.workingTime != nil {
		sched, basis := s.workingTime.ElapsedSchedule(ctx, tenantID)
		detail.Durations = approvalDurations(inst, sched, basis, s.now().UTC())
	}
	// Process context. The timeline must show the steps the ENGINE will walk — the definition
	// pinned on the instance at arm time (bpm.Definition) — not the live row, or the screen and the
	// decision path disagree the moment an admin edits the process. That holds even after the
	// process is soft-deleted: definitionOf never reads the live row for a pinned instance, so the
	// pinned steps are served with deleted=true rather than leaving the approver to decide blind.
	// Only a pre-000065 instance (no pin) whose process is gone yields process=null.
	proc, perr := s.procRepo.GetProcess(ctx, tenantID, inst.ProcessID)
	if perr != nil && !errors.Is(perr, bpm.ErrProcessNotFound) {
		return nil, fmt.Errorf("services.ApprovalService.GetApprovalDetail: %w", perr)
	}
	if proc == nil && inst.Definition == nil {
		return detail, nil
	}
	pc := &ApprovalProcessContext{ID: inst.ProcessID, EntityName: inst.EntityName, Deleted: proc == nil, Version: inst.ProcessVersion}
	if proc != nil {
		pc.Name, pc.EntityName, pc.Trigger, pc.Steps, pc.IsActive = proc.Name, proc.EntityName, proc.Trigger, proc.Steps, proc.IsActive
	}
	if inst.Definition != nil {
		var toMarshal any = inst.Definition.Chain() // a linear pin reads as its steps
		if inst.Definition.Graph != nil && len(inst.Definition.Chain()) > 0 {
			toMarshal = inst.Definition.Graph.Nodes // a graph pin reads as its nodes: kind and routing included (S7)
			if !actor.IsAdmin {
				toMarshal = redactNodes(inst.Definition.Graph.Nodes)
			}
		}
		b, merr := json.Marshal(toMarshal)
		if merr != nil {
			return nil, fmt.Errorf("services.ApprovalService.GetApprovalDetail: marshal pinned steps: %w", merr)
		}
		pc.Steps, pc.Pinned = b, true
	}
	detail.Process = pc
	if chain := inst.Definition.Chain(); inst.CurrentStep >= 0 && inst.CurrentStep < len(chain) && chain[inst.CurrentStep].Parallel() {
		open, approved, rejected := bpm.Tally(visit)
		required := open + approved + rejected // "all": everyone in play
		switch policy := chain[inst.CurrentStep].CompletionPolicy(); policy.Mode {
		case bpm.CompletionAny:
			required = 1
		case bpm.CompletionQuorum:
			required = bpm.QuorumNeed(policy.Percent, required)
		}
		detail.Completion = &ApprovalCompletion{
			Mode: string(chain[inst.CurrentStep].CompletionMode()), Total: open + approved + rejected,
			Approved: approved, Rejected: rejected, Open: open, Required: required,
			Percent: chain[inst.CurrentStep].CompletionPolicy().Percent,
		}
	}
	return detail, nil
}

// redactedNode is what a non-admin may read of a graph node: where it sits and where it leads —
// never a gateway's conditions (field names, thresholds), an auto node's writes or an end's result.
type redactedNode struct {
	ID       string           `json:"id,omitempty"`
	Name     string           `json:"name,omitempty"`
	Kind     bpm.NodeKind     `json:"kind"`
	Next     string           `json:"next,omitempty"`
	Branches []redactedBranch `json:"branches,omitempty"`
	Default  string           `json:"default,omitempty"`
}

type redactedBranch struct {
	Name string `json:"name"`
	To   string `json:"to"`
}

func redactNodes(nodes []bpm.Node) []redactedNode {
	out := make([]redactedNode, len(nodes))
	for i, n := range nodes {
		r := redactedNode{ID: n.ID, Name: n.Name, Kind: n.Kind, Next: n.Next, Default: n.Default}
		for _, b := range n.Branches {
			r.Branches = append(r.Branches, redactedBranch{Name: b.Name, To: b.To})
		}
		out[i] = r
	}
	return out
}

// seatViews maps a visit to the API shape (always a non-nil slice: JSON [] not null).
func seatViews(visit []bpm.VisitSeat) []ApprovalSeat {
	out := make([]ApprovalSeat, len(visit))
	for i, v := range visit {
		out[i] = ApprovalSeat{
			ID: v.ID, AssigneeType: string(v.AssigneeType), AssigneeID: v.AssigneeID, Status: v.Status,
			Outcome: string(v.Outcome), DecidedBy: v.DecidedBy, Comment: v.Comment, DecidedAt: v.CompletedAt,
			DelegatedFrom: v.DelegatedFrom, Kind: v.Kind,
		}
	}
	return out
}

// seatedIn reports whether the actor VOTED on the visit — so an approver who has cast their vote on
// a step with several seats keeps seeing the approval they voted on (specs/043 S4), though they no
// longer await it. A seat merely NAMING them is not enough: one the instance disagrees with (left by
// a pre-S3a binary) is not believed — the holders of open seats get in through awaitsMe, which
// applies that trust (AwaitsActor), not through here.
func seatedIn(visit []bpm.VisitSeat, actor bpm.Actor) bool {
	if actor.UserID == "" {
		return false
	}
	for _, v := range visit {
		if v.DecidedBy == actor.UserID && (v.Outcome == bpm.SeatApproved || v.Outcome == bpm.SeatRejected) {
			return true
		}
	}
	return false
}

// approvalDurations measures an instance's elapsed time from what is already persisted: created_at
// and the `at` of each history entry (specs/043 S2, #40 — no column, no migration). A step opens
// when the instance is created (the first) or when the step before it is approved, and closes at
// the approve / reject / recall entry on it; delegation, escalation and the "submitted" mark neither
// open nor close one. While the instance is pending, its open step and the total run to now.
//
// sched nil = nothing to measure working time on: wall figures only, under basis.
//
// ponytail: measured on today's calendar — an edited calendar re-reads old approvals too (there is
// no calendar history to replay). Revisit if a customer audits SLA figures across a calendar change.
func approvalDurations(inst *bpm.Instance, sched *calendar.Schedule, basis bpm.SLABasis, now time.Time) *ApprovalDurations {
	measure := func(from, to time.Time) (*int64, int64) {
		wall := max(int64(to.Sub(from)/time.Second), 0)
		if sched == nil {
			return nil, wall
		}
		b := int64(sched.BusinessDurationBetween(from, to) / time.Second)
		return &b, wall
	}
	d := &ApprovalDurations{Basis: basis, PerNode: []ApprovalStepDuration{}}
	stretch := func(step int, node, name string, from, to time.Time, closed bool) {
		b, w := measure(from, to)
		sd := ApprovalStepDuration{Step: step, Node: node, StepName: name, StartedAt: from, BusinessSeconds: b, WallSeconds: w}
		if closed {
			sd.EndedAt = &to
		}
		d.PerNode = append(d.PerNode, sd)
	}
	opened := inst.CreatedAt
	end := opened
	for _, h := range inst.History {
		if h.Action != bpm.HistoryApproved && h.Action != bpm.HistoryRejected && h.Action != bpm.HistoryRecalled && h.Action != bpm.HistoryCompleted && h.Action != bpm.HistoryResubmitted && h.Action != bpm.HistoryWaitMet {
			continue
		}
		if h.Partial {
			continue // one vote of a parallel step that is still open: the stretch has not ended
		}
		at := h.At
		if at.Before(opened) {
			at = opened // a mark out of order (skew, a zero legacy stamp) must not run the window backwards
		}
		node, name := h.Node, h.StepName
		if node == "" { // entry written before S0: name it from the pinned chain
			node, name = inst.Definition.StepRef(h.Step)
		}
		stretch(h.Step, node, name, opened, at, true)
		opened, end = at, at
	}
	if inst.Status == bpm.StatusPending {
		node, name := inst.Definition.StepRef(inst.CurrentStep) // unpinned (pre-000065): unnamed, index only
		stretch(inst.CurrentStep, node, name, opened, now, false)
		end = now
	}
	d.TotalBusinessSeconds, d.TotalWallSeconds = measure(inst.CreatedAt, end)
	return d
}

// awaitsActor is the /approvals/pending visibility contract for a single instance: ONLY while the
// instance is PENDING and awaits the actor by the inbox's own rule (bpm.AwaitsActor over its open
// seats, the projection as fallback — specs/043 S3a2). One body with the list, so a row in
// someone's inbox never opens onto a 404. No admin shortcut here: GetApprovalDetail adds it.
func (s *ApprovalService) awaitsActor(ctx context.Context, tenantID string, inst *bpm.Instance, actor bpm.Actor) (bool, error) {
	if inst.Status != bpm.StatusPending {
		return false, nil
	}
	seats, err := s.instRepo.ListOpenSeats(ctx, tenantID, inst.ID)
	if err != nil {
		return false, err
	}
	actor = s.withQueueView(ctx, tenantID, actor)
	return bpm.AwaitsActor(inst, seats, actor.UserID, actor.HolderIDs(), actor.QueueView), nil
}

// Decide applies an approve/reject decision to a pending instance on behalf of the actor. It maps
// the engine's typed errors to validation codes: not-found → 404, not-pending/not-authorized →
// 422/403-shaped. Returns the updated instance.
func (s *ApprovalService) Decide(ctx context.Context, tenantID, id string, kind bpm.DecisionKind, comment string, actor bpm.Actor) (*bpm.Instance, error) {
	return s.DecideAt(ctx, tenantID, id, kind, comment, actor, nil)
}

// DecideAt is Decide for a client that says which step it SAW (expectedStep): a click that
// arrives after another one already moved the instance is refused as APPROVAL_CONFLICT instead
// of approving the step the clicker never looked at. nil = the client sent none.
func (s *ApprovalService) DecideAt(ctx context.Context, tenantID, id string, kind bpm.DecisionKind, comment string, actor bpm.Actor, expectedStep *int) (*bpm.Instance, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("APPROVAL_INSTANCE_ID_REQUIRED", "id phê duyệt là bắt buộc")
	}
	if kind != bpm.DecisionApprove && kind != bpm.DecisionReject {
		return nil, invalid("APPROVAL_DECISION_INVALID", "quyết định không hợp lệ (approve/reject)")
	}
	// A rejection states its reason (specs/043 FR-027). Required on the SERVER: the screens asked for
	// it, but any holder of a token could reject with nothing and leave the submitter a "no" with no
	// why. Checked before any read — the same answer for every id, so it is no existence oracle.
	if kind == bpm.DecisionReject && strings.TrimSpace(comment) == "" {
		return nil, invalidField("APPROVAL_REJECT_REASON_REQUIRED", "note", "cần ghi lý do khi từ chối")
	}
	inst, err := s.decider.DecideAt(ctx, tenantID, id, kind, comment, s.withQueueView(ctx, tenantID, actor), expectedStep)
	if err != nil {
		if mapped := mapInstanceErr(err); mapped != nil {
			return nil, mapped
		}
		return nil, fmt.Errorf("services.ApprovalService.DecideAt: %w", err)
	}
	return inst, nil
}

// OverrideApproval lets an admin decide the current step in place of its seats (specs/043 FR-023,
// D-13). The note is the reason, REQUIRED for both directions (it is the only record of why the
// seats were bypassed) and checked before any read — the same answer for every id.
func (s *ApprovalService) OverrideApproval(ctx context.Context, tenantID, id string, kind bpm.DecisionKind, note string, actor bpm.Actor, expectedStep *int) (*bpm.Instance, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("APPROVAL_INSTANCE_ID_REQUIRED", "id phê duyệt là bắt buộc")
	}
	if kind != bpm.DecisionApprove && kind != bpm.DecisionReject {
		return nil, invalidField("APPROVAL_DECISION_INVALID", "decision", "decision không hợp lệ (approve/reject)")
	}
	if strings.TrimSpace(note) == "" {
		return nil, invalidField("APPROVAL_OVERRIDE_NOTE_REQUIRED", "note", "quyết thay cần ghi lý do")
	}
	inst, err := s.decider.Override(ctx, tenantID, id, kind, strings.TrimSpace(note), actor, expectedStep)
	if err != nil {
		if mapped := mapInstanceErr(err); mapped != nil {
			return nil, mapped
		}
		return nil, fmt.Errorf("services.ApprovalService.OverrideApproval: %w", err)
	}
	return inst, nil
}

// mapInstanceErr turns the engine's instance-lifecycle sentinels into coded validation errors, or
// nil when err is none of them (the caller then wraps it as a real failure).
//
// ONE body for every path that touches an instance — decide, recall, delegate. A second copy would
// be a place for "not pending" to become a 404 on one route and a 422 on another for the same
// underlying state, which is the sort of drift nobody notices until a client branches on it.
func mapInstanceErr(err error) error {
	switch {
	case errors.Is(err, bpm.ErrInstanceNotFound):
		return invalid("APPROVAL_INSTANCE_NOT_FOUND", "không tìm thấy phiếu phê duyệt")
	case errors.Is(err, bpm.ErrNotPending):
		return invalid("APPROVAL_NOT_PENDING", "phiếu phê duyệt đã kết thúc, không thể xử lý")
	case errors.Is(err, bpm.ErrNotStuck):
		return invalid("APPROVAL_NOT_STUCK", "phiếu này không kẹt ở bước tự động — đang chờ người xử lý nên không có gì để thử lại")
	case errors.Is(err, bpm.ErrNotAuthorized):
		return invalid("APPROVAL_NOT_AUTHORIZED", "bạn không phải người duyệt bước hiện tại")
	case errors.Is(err, bpm.ErrConcurrent):
		return invalid("APPROVAL_CONFLICT", "bước phê duyệt vừa thay đổi, vui lòng thử lại")
	case errors.Is(err, bpm.ErrSeatClaimed):
		return invalid("WORK_ITEM_ALREADY_CLAIMED", "đã có người nhận việc này")
	case errors.Is(err, bpm.ErrWrongStepKind):
		return invalid("APPROVAL_STEP_KIND_MISMATCH", "thao tác này không áp dụng cho bước hiện tại (việc thì bấm Hoàn thành, bước duyệt thì Duyệt hoặc Từ chối)")
	case errors.Is(err, bpm.ErrNoSubmitter):
		return invalid("APPROVAL_NO_SUBMITTER", "không tìm được người gửi để trả hồ sơ về (người tạo không còn hoạt động và bản ghi không có chủ sở hữu) — nhờ quản trị viên xử lý")
	case errors.Is(err, bpm.ErrAlreadyDecidedByActor):
		return invalid("APPROVAL_ALREADY_DECIDED_BY_ACTOR", "bạn đã quyết ở bước này rồi")
	case errors.Is(err, bpm.ErrAdminOverrideRequired):
		return invalid("APPROVAL_ADMIN_OVERRIDE_REQUIRED", "bước này có nhiều người duyệt — quản trị viên không có ghế dùng lệnh \"quyết thay\" (có ghi lý do)")
	default:
		return nil
	}
}

// ---- validation helpers ---------------------------------------------------------------------

// maxApprovalProcessNameRunes mirrors bpm_processes.name VARCHAR(150) (migration 000021) — a
// CHARACTER budget. Without this guard a long name passes the non-empty check and dies as a
// MySQL 1406 → 500 at insert; rejecting it config-time keeps both the admin apply path and an
// AI-buffered draft applyable on the name axis (the M5 maxRelationshipNameBytes lesson).
const maxApprovalProcessNameRunes = 150

// ValidateApprovalProcess is the exported config-time validator for an approval process:
// name (required, ≤150 chars), trigger (after_create/after_update/manual), criteria (workflow
// condition vocabulary, match and|or; changed/changedTo only on after_update — specs/043 S2b) and
// steps (≥1, approverType user/role, non-empty approver ref,
// slaSeconds ≥ 0, optional stable id — unique, ^[a-z0-9_-]{1,60}$, missing ones assigned —, and
// no key outside the linear v1 shape: APPROVAL_NODE_KIND_UNSUPPORTED, specs/043 D-7).
// It is ONE body with the apply path — CreateProcess and UpdateProcess run THIS function
// (validator-parity), and the AI Builder's propose_approval tool runs it too, so a draft that
// buffers is exactly what POST /admin/approval-processes accepts. Entity existence is
// deliberately NOT here: it is tenant-scoped + DB-dependent, so each caller resolves it against
// its own tenant-pinned reader (CreateProcess via validateEntity; the AI tool via its
// tenant-scoped metadata reader with draft-buffer awareness).
//
// Returns the normalized trigger plus the marshaled criteria/steps JSON the repository persists
// (nil criteria = match-all).
func ValidateApprovalProcess(name, trigger, match string, rules []ApprovalCriteriaRuleInput, steps []ApprovalStepInput) (bpm.Trigger, []byte, []byte, error) {
	return validateApprovalLinear(name, trigger, match, rules, steps, nil)
}

// ValidateApprovalChain is ValidateApprovalProcess for a caller whose steps may be in the
// many-approver form (specs/043 S4 — the AI tool, the pack installer): the SAME body CreateProcess
// runs, so what it accepts is exactly what Apply will store. Returns trigger, criteria JSON, steps
// JSON and graph JSON: a chain with a parallel step comes back as steps '[]' beside a graph.
func ValidateApprovalChain(name, trigger, match string, rules []ApprovalCriteriaRuleInput, steps []ApprovalStepInput) (bpm.Trigger, []byte, []byte, []byte, error) {
	return validateApprovalProcessForm(name, trigger, match, rules, steps, nil, nil)
}

// validateApprovalLinear is ValidateApprovalProcess that also keeps the ids it assigns off reserved
// (the ids the process has published — specs/043 S10): nil for a process with no history.
func validateApprovalLinear(name, trigger, match string, rules []ApprovalCriteriaRuleInput, steps []ApprovalStepInput, reserved map[string]bool) (bpm.Trigger, []byte, []byte, error) {
	t, critJSON, err := validateApprovalHeader(name, trigger, match, rules)
	if err != nil {
		return "", nil, nil, err
	}
	stepsJSON, err := buildApprovalStepsJSON(steps, reserved)
	if err != nil {
		return "", nil, nil, err
	}
	return t, critJSON, stepsJSON, nil
}

// validateApprovalProcessForm is ValidateApprovalProcess for a body that may carry its chain in
// either form (the admin API): steps → exactly ValidateApprovalProcess; graph → the same header
// checks + buildApprovalGraphJSON, stored beside steps '[]'. Both at once is refused.
func validateApprovalProcessForm(name, trigger, match string, rules []ApprovalCriteriaRuleInput, steps []ApprovalStepInput, graph *ApprovalGraphInput, reserved map[string]bool) (t bpm.Trigger, critJSON, stepsJSON, graphJSON []byte, err error) {
	if graph == nil {
		// A step in the many-approver form (S4) cannot live in the linear column — a binary older
		// than S4 would walk it as no approver at all. The whole chain moves to the graph form,
		// every step a node, in order (bpm.GraphFromSteps); errors keep pointing at steps[i].
		for _, st := range steps {
			if st.needsGraph() {
				nodes := make([]ApprovalNodeInput, len(steps))
				for i, s := range steps {
					nodes[i] = ApprovalNodeInput{ApprovalStepInput: s, Kind: string(bpm.NodeApproval)}
				}
				if t, critJSON, err = validateApprovalHeader(name, trigger, match, rules); err != nil {
					return "", nil, nil, nil, err
				}
				if graphJSON, err = buildApprovalGraphJSON(&ApprovalGraphInput{Nodes: nodes}, reserved, false); err != nil {
					return "", nil, nil, nil, err
				}
				return t, critJSON, []byte("[]"), graphJSON, nil
			}
		}
		t, critJSON, stepsJSON, err = validateApprovalLinear(name, trigger, match, rules, steps, reserved)
		return t, critJSON, stepsJSON, nil, err
	}
	// Unknown keys first, like UnsupportedApprovalProcessKeys runs before everything else: the
	// answer to "graph.start" is "not yet", whatever else the body gets wrong.
	if len(graph.Unsupported) > 0 {
		return "", nil, nil, nil, invalidField("APPROVAL_NODE_KIND_UNSUPPORTED", "graph."+graph.Unsupported[0],
			"graph có khoá %s mà engine phê duyệt hiện tại chưa chạy được — bỏ khoá này (sẽ mở ở lát sau)", strings.Join(graph.Unsupported, ", "))
	}
	if t, critJSON, err = validateApprovalHeader(name, trigger, match, rules); err != nil {
		return "", nil, nil, nil, err
	}
	if len(steps) > 0 {
		return "", nil, nil, nil, invalidField("APPROVAL_STEP_INVALID", "graph",
			"gửi các bước ở `steps` HOẶC ở `graph`, không cả hai — một quy trình chỉ có một dạng")
	}
	if graphJSON, err = buildApprovalGraphJSON(graph, reserved, true); err != nil {
		return "", nil, nil, nil, err
	}
	return t, critJSON, []byte("[]"), graphJSON, nil
}

// validateApprovalHeader checks what every process form shares: name, trigger, criteria.
func validateApprovalHeader(name, trigger, match string, rules []ApprovalCriteriaRuleInput) (bpm.Trigger, []byte, error) {
	if strings.TrimSpace(name) == "" {
		return "", nil, invalid("APPROVAL_PROCESS_NAME_REQUIRED", "name quy trình là bắt buộc")
	}
	if utf8.RuneCountInString(name) > maxApprovalProcessNameRunes {
		return "", nil, invalid("APPROVAL_PROCESS_NAME_TOO_LONG",
			"name quy trình vượt %d ký tự (cột bpm_processes.name)", maxApprovalProcessNameRunes)
	}
	t, err := validateApprovalTrigger(trigger)
	if err != nil {
		return "", nil, err
	}
	critJSON, err := buildApprovalCriteriaJSON(t, match, rules)
	if err != nil {
		return "", nil, err
	}
	return t, critJSON, nil
}

// buildApprovalGraphJSON validates the graph form and marshals it (specs/043 S3a2). Every node's
// kind is judged FIRST: required, and only the kind the engine runs today (approval) is accepted —
// D-7. A later slice's node (gateway/task/end/wait) carries no approver, so judging it as a step
// first would answer "add an approver" instead of "that kind is not open yet". Then every node is
// judged by the SAME rules as a step (validateApprovalSteps — id pattern and uniqueness, approver,
// SLA, clock, unknown keys), located at graph.nodes[i]. A graph-level key other than nodes is
// refused the same way — by validateApprovalProcessForm, before anything else.
func buildApprovalGraphJSON(g *ApprovalGraphInput, reserved map[string]bool, fromGraph bool) ([]byte, error) {
	if len(g.Nodes) == 0 {
		return nil, invalidField("APPROVAL_STEPS_REQUIRED", "graph.nodes", "quy trình cần ít nhất 1 bước phê duyệt")
	}
	stepInputs := make([]ApprovalStepInput, len(g.Nodes))
	kinds := make([]bpm.NodeKind, len(g.Nodes))
	for i, n := range g.Nodes {
		kind := bpm.NodeKind(strings.TrimSpace(n.Kind))
		switch {
		case kind == "":
			return nil, invalidField("APPROVAL_STEP_INVALID", fmt.Sprintf("graph.nodes[%d].kind", i),
				"bước %d thiếu kind (%q, %q, %q, %q, %q hoặc %q)", i+1, bpm.NodeApproval, bpm.NodeTask, bpm.NodeGateway, bpm.NodeAuto, bpm.NodeWait, bpm.NodeEnd)
		case !bpm.RunnableNodeKind(kind):
			return nil, invalidField("APPROVAL_NODE_KIND_UNSUPPORTED", fmt.Sprintf("graph.nodes[%d].kind", i),
				"bước %d có loại %q mà engine phê duyệt hiện tại chưa chạy được — hiện có %q, %q, %q, %q, %q và %q (các loại khác mở ở lát sau)", i+1, n.Kind, bpm.NodeApproval, bpm.NodeTask, bpm.NodeGateway, bpm.NodeAuto, bpm.NodeWait, bpm.NodeEnd)
		}
		kinds[i] = kind
		si := n.ApprovalStepInput
		if bpm.MachineKind(kind) {
			if err := machineNodeShape(i, n); err != nil {
				return nil, err
			}
			// A machine node names nobody. The shared step validator below judges ids, names and
			// unknown keys for EVERY node and insists on an approver, so it gets a stand-in that is
			// dropped again once the ids are settled.
			si.ApproverType, si.ApproverID = string(bpm.ApproverUser), "-"
		}
		stepInputs[i] = si
	}
	// A graph node may also name a work queue (S5) — the linear v1 step may not.
	steps, err := validateApprovalStepsFor(stepInputs, reserved, bpm.ValidAssigneeType, "user/role/queue")
	if err != nil {
		if fromGraph { // the caller sent `graph`: point at graph.nodes[i]; a lifted `steps` list keeps steps[i]
			return nil, atGraphNodes(err)
		}
		return nil, err
	}
	out := bpm.Graph{Nodes: make([]bpm.Node, len(g.Nodes)), Start: strings.TrimSpace(g.Start)}
	for i := range g.Nodes {
		fields, err := normalizeRequiredFields(i, kinds[i], g.Nodes[i].RequiredFields)
		if err != nil {
			return nil, err
		}
		step := steps[i]
		if bpm.MachineKind(kinds[i]) {
			step.ApproverType, step.ApproverID = "", "" // drop the stand-in the shared validator needed
		}
		onReject, err := normalizeOnReject(i, kinds[i], g.Nodes[i].OnReject, steps, fromGraph)
		if err != nil {
			return nil, err
		}
		node, err := buildRoutedNode(i, kinds[i], g.Nodes[i], step, fields)
		if err != nil {
			return nil, err
		}
		node.OnReject = onReject
		out.Nodes[i] = node
	}
	// How the nodes lead to each other — the half no single node can judge. ONE body with the
	// engine (bpm.CheckGraph): what it refuses to walk, the API refuses to store.
	if p := bpm.CheckGraph(&out); p != nil {
		return nil, graphProblemError(&out, p)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("services.buildApprovalGraphJSON: %w", err)
	}
	return b, nil
}

// Bounds on the routing config of one node: a gateway reads every branch per walk and an auto node
// writes its fields in one update, so neither is allowed to grow without limit.
const (
	maxGatewayBranches = 20
	maxAutoSetFields   = 20
	maxFieldNameLen    = 64
	maxBranchNameLen   = 60
)

// machineNodeShape refuses what a gateway / auto / end node cannot carry: it is held by nobody, so
// no approver, deadline or required field (B6: a config the engine would ignore is refused, not
// dropped).
func machineNodeShape(i int, n ApprovalNodeInput) error {
	at := func(f string) string { return fmt.Sprintf("graph.nodes[%d].%s", i, f) }
	switch {
	case strings.TrimSpace(n.ApproverType) != "" || strings.TrimSpace(n.ApproverID) != "" || len(n.Approvers) > 0 || n.Completion != nil:
		return invalidField("APPROVAL_STEP_INVALID", at("approverType"),
			"bước %d là bước tự động (%s) nên không có người duyệt", i+1, n.Kind)
	case n.SLASeconds != 0 || strings.TrimSpace(n.SLAClock) != "":
		return invalidField("APPROVAL_STEP_INVALID", at("slaSeconds"),
			"bước %d là bước tự động (%s) nên không có hạn xử lý", i+1, n.Kind)
	case len(n.RequiredFields) > 0:
		return invalidField("APPROVAL_STEP_INVALID", at("requiredFields"),
			"bước %d là bước tự động (%s) nên không có trường bắt buộc", i+1, n.Kind)
	}
	return nil
}

// buildRoutedNode assembles the node with its routing and refuses a key that does not belong to its
// kind: only a gateway has branches/default, only an auto node set, only an end node result, and a
// gateway / end node has no next.
func buildRoutedNode(i int, kind bpm.NodeKind, in ApprovalNodeInput, step bpm.Step, required []string) (bpm.Node, error) {
	at := func(f string) string { return fmt.Sprintf("graph.nodes[%d].%s", i, f) }
	node := bpm.Node{Step: step, Kind: kind, RequiredFields: required, Next: strings.TrimSpace(in.Next)}
	if kind != bpm.NodeGateway && (len(in.Branches) > 0 || strings.TrimSpace(in.Default) != "") {
		return node, invalidField("APPROVAL_STEP_INVALID", at("branches"), "bước %d không phải bước rẽ nhánh nên không có branches/default", i+1)
	}
	if kind != bpm.NodeAuto && len(in.Set) > 0 {
		return node, invalidField("APPROVAL_STEP_INVALID", at("set"), "bước %d không phải bước tự động ghi trường nên không có set", i+1)
	}
	if kind != bpm.NodeEnd && strings.TrimSpace(in.Result) != "" {
		return node, invalidField("APPROVAL_STEP_INVALID", at("result"), "bước %d không phải bước kết thúc nên không có result", i+1)
	}
	if kind != bpm.NodeWait && (in.Until != nil || in.SLA != nil) {
		return node, invalidField("APPROVAL_STEP_INVALID", at("until"), "bước %d không phải bước chờ nên không có until/sla", i+1)
	}
	switch kind {
	case bpm.NodeWait:
		return buildWaitNode(i, node, in)
	case bpm.NodeGateway:
		if node.Next != "" {
			return node, invalidField("APPROVAL_STEP_INVALID", at("next"), "bước rẽ nhánh %d đi theo branches/default, không có next", i+1)
		}
		node.Default = strings.TrimSpace(in.Default)
		if len(in.Branches) == 0 {
			return node, invalidField("APPROVAL_STEP_INVALID", at("branches"), "bước rẽ nhánh %d cần ít nhất 1 nhánh có điều kiện", i+1)
		}
		if len(in.Branches) > maxGatewayBranches {
			return node, invalidField("APPROVAL_STEP_INVALID", at("branches"), "bước rẽ nhánh %d có quá %d nhánh", i+1, maxGatewayBranches)
		}
		for j, b := range in.Branches {
			name := strings.TrimSpace(b.Name)
			if utf8.RuneCountInString(name) > maxBranchNameLen {
				return node, invalidField("APPROVAL_STEP_INVALID", at(fmt.Sprintf("branches[%d].name", j)), "tên nhánh %d của bước %d quá dài (tối đa %d ký tự)", j+1, i+1, maxBranchNameLen)
			}
			// The record the gateway reads is a snapshot of NOW: an edge operator (changed /
			// changedTo) needs a before-image it does not have, so it could never hold.
			cond, err := buildApprovalCondition(b.Match, b.Rules, func(op string) error {
				return invalidField("APPROVAL_CRITERIA_EDGE_OP_UNSUPPORTED", at(fmt.Sprintf("branches[%d].when", j)),
					"«vừa đổi»/«vừa đổi thành» (%q) không dùng được ở bước rẽ nhánh — nó chỉ xét giá trị hiện tại của hồ sơ; dùng «=»", op)
			})
			if err != nil {
				return node, withField(err, at(fmt.Sprintf("branches[%d].when", j)))
			}
			node.Branches = append(node.Branches, bpm.Branch{Name: name, When: cond, To: strings.TrimSpace(b.To)})
		}
	case bpm.NodeAuto:
		if len(in.Set) == 0 {
			return node, invalidField("APPROVAL_STEP_INVALID", at("set"), "bước tự động %d cần ít nhất 1 trường để ghi (set)", i+1)
		}
		if len(in.Set) > maxAutoSetFields {
			return node, invalidField("APPROVAL_STEP_INVALID", at("set"), "bước tự động %d ghi quá %d trường", i+1, maxAutoSetFields)
		}
		seen := make(map[string]bool, len(in.Set))
		for j, w := range in.Set {
			field := strings.TrimSpace(w.Field)
			switch {
			case field == "":
				return node, invalidField("APPROVAL_STEP_INVALID", at(fmt.Sprintf("set[%d].field", j)), "trường ghi %d của bước %d để trống", j+1, i+1)
			case utf8.RuneCountInString(field) > maxFieldNameLen:
				return node, invalidField("APPROVAL_STEP_INVALID", at(fmt.Sprintf("set[%d].field", j)), "tên trường %q của bước %d quá dài (tối đa %d ký tự)", field, i+1, maxFieldNameLen)
			case seen[field]:
				return node, invalidField("APPROVAL_STEP_INVALID", at(fmt.Sprintf("set[%d].field", j)), "trường %q của bước %d bị ghi hai lần", field, i+1)
			case w.Value == nil:
				return node, invalidField("APPROVAL_STEP_INVALID", at(fmt.Sprintf("set[%d].value", j)), "trường %q của bước %d thiếu giá trị (dùng chuỗi rỗng để xoá chữ)", field, i+1)
			}
			seen[field] = true
			node.Set = append(node.Set, bpm.SetField{Field: field, Value: w.Value})
		}
	case bpm.NodeEnd:
		node.Result = strings.TrimSpace(in.Result)
		if node.Result != bpm.ResultApproved && node.Result != bpm.ResultRejected {
			return node, invalidField("APPROVAL_STEP_INVALID", at("result"), "bước kết thúc %d cần result %q hoặc %q (đang là %q)", i+1, bpm.ResultApproved, bpm.ResultRejected, in.Result)
		}
		if node.Next != "" {
			return node, invalidField("APPROVAL_STEP_INVALID", at("next"), "bước kết thúc %d không có bước nào sau nó", i+1)
		}
	}
	return node, nil
}

// graphProblemError turns what bpm.CheckGraph found into the coded 422, pointing at the node and
// key at fault (the node's name when it has one — never only its machine id).
func graphProblemError(g *bpm.Graph, p *bpm.GraphProblem) error {
	if p.Node < 0 {
		return invalidField(p.Code, "graph."+p.Field, "%s", p.Reason)
	}
	field := fmt.Sprintf("graph.nodes[%d]", p.Node)
	if p.Field != "" {
		field += "." + p.Field
	}
	label := g.Nodes[p.Node].ID
	if name := g.Nodes[p.Node].Name; name != "" {
		label = name
	}
	return invalidField(p.Code, field, "bước %d (%s): %s", p.Node+1, label, p.Reason)
}

// normalizeOnReject validates node i's onReject (S8) by the engine's own rule (bpm.CheckOnReject — what
// it refuses to run, the API refuses to store) against the chain's ids, and writes the default
// resume point down so a pinned node never depends on a default. nil in → nil out. fromGraph says
// whether the caller sent `graph` (error located at graph.nodes[i]) or a lifted `steps` list.
func normalizeOnReject(i int, kind bpm.NodeKind, in *ApprovalOnRejectInput, chain []bpm.Step, fromGraph bool) (*bpm.OnReject, error) {
	if in == nil {
		return nil, nil
	}
	out := &bpm.OnReject{To: strings.TrimSpace(in.To), ResumeAt: strings.TrimSpace(in.ResumeAt)}
	if f, reason := bpm.CheckOnReject(out, kind, i, chain); f != "" {
		where := "steps"
		if fromGraph {
			where = "graph.nodes"
		}
		return nil, invalidField("APPROVAL_STEP_INVALID", fmt.Sprintf("%s[%d].onReject.%s", where, i, f), "bước %d: %s", i+1, reason)
	}
	if out.ToSubmitter() && out.ResumeAt == "" {
		out.ResumeAt = bpm.ResumeRejector
	}
	return out, nil
}

// CheckReturnedValueUsed (exported for the pack pre-write gate) refuses an outcome.returnedValue no step can ever produce (B6 parity: a
// value with nothing to write it is a configuration that silently does nothing). The steps/graph
// are the ones the same request carries.
func CheckReturnedValueUsed(out *ApprovalOutcomeInput, steps []ApprovalStepInput, graph *ApprovalGraphInput) error {
	if out == nil || strings.TrimSpace(out.ReturnedValue) == "" {
		return nil
	}
	submitter := func(o *ApprovalOnRejectInput) bool {
		return o != nil && strings.TrimSpace(o.To) == bpm.RejectToSubmitter
	}
	for _, s := range steps {
		if submitter(s.OnReject) {
			return nil
		}
	}
	if graph != nil {
		for _, n := range graph.Nodes {
			if submitter(n.OnReject) {
				return nil
			}
		}
	}
	return invalidField("APPROVAL_OUTCOME_VALUE_UNUSED", "outcome.returnedValue",
		"đã đặt returnedValue nhưng không bước nào có onReject trả về người gửi (@submitter) — giá trị này sẽ không bao giờ được ghi")
}

// maxTaskRequiredFields / maxRequiredFieldNameLen bound a task's requiredFields at config time: the
// list is read on every completion, and a field name is at most the metadata identifier length.
const (
	maxTaskRequiredFields   = 50
	maxRequiredFieldNameLen = 64
)

// normalizeRequiredFields validates a node's requiredFields shape (B6 parity: a mistyped list is
// refused HERE, not left to turn a task into one that can never — or always — complete): only a task
// carries them, each a non-blank name of ≤ 64 characters, no duplicates, at most 50. Returns the
// trimmed list (nil when empty). Whether the fields exist on the process's entity is judged by the
// service, which has the tenant and the entity (checkGraphRefs).
func normalizeRequiredFields(i int, kind bpm.NodeKind, in []string) ([]string, error) {
	at := fmt.Sprintf("graph.nodes[%d].requiredFields", i)
	if len(in) == 0 {
		return nil, nil
	}
	if kind != bpm.NodeTask {
		return nil, invalidField("APPROVAL_STEP_INVALID", at, "bước %d không phải việc (task) nên không có requiredFields", i+1)
	}
	if len(in) > maxTaskRequiredFields {
		return nil, invalidField("APPROVAL_STEP_INVALID", at, "bước %d có quá %d trường bắt buộc", i+1, maxTaskRequiredFields)
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		name := strings.TrimSpace(raw)
		switch {
		case name == "":
			return nil, invalidField("APPROVAL_STEP_INVALID", at, "bước %d có tên trường bắt buộc để trống", i+1)
		case utf8.RuneCountInString(name) > maxRequiredFieldNameLen:
			return nil, invalidField("APPROVAL_STEP_INVALID", at, "tên trường bắt buộc %q của bước %d quá dài (tối đa %d ký tự)", name, i+1, maxRequiredFieldNameLen)
		case seen[name]:
			return nil, invalidField("APPROVAL_STEP_INVALID", at, "trường bắt buộc %q của bước %d bị lặp", name, i+1)
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// atGraphNodes relocates a step validation error to the graph node it came from
// (steps[i].x → graph.nodes[i].x) so the caller is pointed at the key they actually sent.
func atGraphNodes(err error) error {
	var ve *ValidationError
	if errors.As(err, &ve) && strings.HasPrefix(ve.Field, "steps[") {
		ve.Field = "graph.nodes[" + strings.TrimPrefix(ve.Field, "steps[")
		ve.Details["field"] = ve.Field
	}
	return err
}

// ApprovalOutcomeSelfTrigger refuses a process whose own write-back would start it again
// (APPROVAL_OUTCOME_SELF_TRIGGER, specs/043 US7 #5): an after_update process with a criteria rule
// on outcome.statusField that the value it writes on finishing satisfies — "vừa đổi (thành X)"
// and writes X, but equally "= X", "≠ Nháp" or "không rỗng". The outcome write goes through
// RecordService.Update, so it IS an edit; the instance it closes is no longer pending, so the
// engine arms a fresh one — every approval would end by opening the next.
//
// Every rule on the outcome's field is judged by the engine's own evaluator against the one change
// the write-back makes (field → value, from nothing), so "does this value start it" means exactly
// what it means at arm time, for edge and state operators alike. Shared by
// CreateProcess/UpdateProcess and the pack's pre-write gate; the AI tool has no outcome to check.
//
// ponytail: judged per rule, ignoring match — under AND a sibling rule might keep the restart
// from happening on a given record, but a process that re-opens itself whenever it can is still
// a mistake worth refusing at save time rather than discovering in the inbox. A process with NO
// rule on the outcome field (match-all, or rules on other fields only) also re-arms after its
// write-back; that stays open in specs/043 "Nợ còn mở" 19.
func ApprovalOutcomeSelfTrigger(trigger string, rules []ApprovalCriteriaRuleInput, out *ApprovalOutcomeInput) error {
	if bpm.Trigger(strings.TrimSpace(trigger)) != bpm.TriggerAfterUpdate || out == nil {
		return nil
	}
	field := strings.TrimSpace(out.StatusField)
	if field == "" {
		return nil
	}
	for _, r := range rules {
		if strings.TrimSpace(r.Field) != field {
			continue
		}
		op := strings.TrimSpace(r.Op)
		cond := &workflow.Condition{Rules: []workflow.Rule{{Field: field, Op: op, Value: r.Value}}}
		for _, w := range []struct{ key, value string }{
			{"approvedValue", out.ApprovedValue}, {"rejectedValue", out.RejectedValue},
		} {
			v := strings.TrimSpace(w.value)
			if v == "" {
				continue
			}
			if workflow.EvaluateWithBefore(cond, map[string]any{field: v}, map[string]any{}) {
				rule := op
				if r.Value != nil {
					rule = fmt.Sprintf("%s %v", op, r.Value)
				}
				return invalidField("APPROVAL_OUTCOME_SELF_TRIGGER", "outcome."+w.key,
					"kết quả duyệt ghi %q vào %q — giá trị đó thoả chính điều kiện khởi động của quy trình này («%s %s»), mà ghi-ngược là một lần sửa bản ghi: mỗi lần kết thúc nó sẽ tự mở lại. Đổi giá trị ghi-ngược hoặc điều kiện khởi động",
					v, field, field, rule)
			}
		}
	}
	return nil
}

// validateEntity trims + checks the entity name is non-empty and (when a checker is wired) refers
// to a real entity in the tenant.
func (s *ApprovalService) validateEntity(ctx context.Context, tenantID, raw string) (string, error) {
	entity := strings.TrimSpace(raw)
	if entity == "" {
		return "", invalid("APPROVAL_PROCESS_ENTITY_REQUIRED", "entityName của quy trình là bắt buộc")
	}
	if s.entities != nil {
		if _, err := s.entities.GetEntityByName(ctx, tenantID, entity); err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				return "", invalid("APPROVAL_PROCESS_ENTITY_NOT_FOUND", "entity %q không tồn tại", entity)
			}
			return "", fmt.Errorf("services.ApprovalService.validateEntity: %w", err)
		}
	}
	return entity, nil
}

// validateApprovalTrigger normalizes + validates the trigger against the supported events.
func validateApprovalTrigger(raw string) (bpm.Trigger, error) {
	t := bpm.Trigger(strings.TrimSpace(raw))
	if !bpm.ValidTrigger(t) {
		return "", invalid("APPROVAL_TRIGGER_INVALID",
			"trigger không hợp lệ: %q (chỉ after_create/after_update/manual)", raw)
	}
	return t, nil
}

// buildApprovalCriteriaJSON validates the criteria rules and marshals them to JSON. An empty rule
// list yields nil (NULL column → match-all). Reuses the workflow condition vocabulary/validators so
// the filter language is identical across automation and approval (validation-parity B6): match
// "and" (default) or "or", and the value rules of workflow.buildConditionJSON.
func buildApprovalCriteriaJSON(trigger bpm.Trigger, match string, rules []ApprovalCriteriaRuleInput) ([]byte, error) {
	if len(rules) == 0 {
		return nil, nil // match-all
	}
	cond, err := buildApprovalCondition(match, rules, func(op string) error {
		// changed / changedTo compare against the record's BEFORE-image, and only an edit has one:
		// the after_update event carries it (bpm.Event.Before), a create or a manual send does not,
		// so there the rule could never be true — a silent, always-false rule. Refuse it on those
		// triggers instead of saving a process that can never start (specs/043 FR-025).
		if trigger == bpm.TriggerAfterUpdate {
			return nil
		}
		return invalid("APPROVAL_CRITERIA_EDGE_OP_UNSUPPORTED",
			"«vừa đổi»/«vừa đổi thành» (%q) chỉ dùng được khi quy trình khởi động lúc SỬA bản ghi (after_update) — lúc tạo hoặc gửi tay chưa có giá trị cũ để so; dùng «=» để so trạng thái hiện tại", op)
	})
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(cond)
	if err != nil {
		return nil, fmt.Errorf("services.buildApprovalCriteriaJSON: %w", err)
	}
	return b, nil
}

// buildApprovalCondition validates match + rules and builds the condition — the ONE body for the
// process's entry criteria and a gateway's branches, so "a rule that means X at start means X at a
// branch" (B6 parity). edgeOK is asked about each edge operator (changed / changedTo): nil error =
// allowed here, else the refusal to return.
func buildApprovalCondition(match string, rules []ApprovalCriteriaRuleInput, edgeOK func(op string) error) (*workflow.Condition, error) {
	joinMode := "and"
	switch strings.TrimSpace(strings.ToLower(match)) {
	case "", "and":
	case "or":
		joinMode = "or"
	default:
		return nil, invalid("APPROVAL_CRITERIA_INVALID", "match không hợp lệ: %q (chỉ 'and' hoặc 'or')", match)
	}
	cond := workflow.Condition{Match: joinMode, Rules: make([]workflow.Rule, 0, len(rules))}
	for _, r := range rules {
		field := strings.TrimSpace(r.Field)
		if field == "" {
			return nil, invalid("APPROVAL_CRITERIA_INVALID", "field của điều kiện là bắt buộc")
		}
		op := strings.TrimSpace(r.Op)
		if !workflow.ValidOperator(op) {
			return nil, invalid("APPROVAL_CRITERIA_INVALID",
				"operator không hợp lệ: %q (eq/neq/gt/gte/lt/lte/contains/isnull/notnull/changed/changedTo)", r.Op)
		}
		if op == workflow.OpChanged || op == workflow.OpChangedTo {
			if err := edgeOK(op); err != nil {
				return nil, err
			}
		}
		// changed asks only "did this field move", so like the null operators it takes NO value.
		isNullOp := op == workflow.OpIsNull || op == workflow.OpNotNull || op == workflow.OpChanged
		if isNullOp {
			if r.Value != nil {
				return nil, invalid("APPROVAL_CRITERIA_INVALID", "operator %q không nhận value", op)
			}
		} else if r.Value == nil {
			return nil, invalid("APPROVAL_CRITERIA_INVALID", "operator %q cần value cho field %q", op, field)
		}
		cond.Rules = append(cond.Rules, workflow.Rule{Field: field, Op: op, Value: r.Value})
	}
	return &cond, nil
}

// buildApprovalStepsJSON validates the step list and marshals it to JSON. At least one step is
// required (a process with no steps is dead config — nothing to approve). Each step needs a valid
// approverType (user/role) + a non-empty approverId; slaSeconds must be >= 0 (validation-parity B6:
// a negative SLA must not pass quietly — a misconfigured deadline is rejected at config time).
func buildApprovalStepsJSON(steps []ApprovalStepInput, reserved map[string]bool) ([]byte, error) {
	if len(steps) == 0 {
		return nil, invalid("APPROVAL_STEPS_REQUIRED", "quy trình cần ít nhất 1 bước phê duyệt")
	}
	out, err := validateApprovalSteps(steps, reserved)
	if err != nil {
		return nil, err
	}
	for i, st := range out {
		if st.Parallel() { // the form-level validator lifts these to a graph first; a caller that skipped it must not store one linear
			return nil, invalidField("APPROVAL_NODE_KIND_UNSUPPORTED", fmt.Sprintf("steps[%d].approvers", i),
				"bước %d có nhiều người duyệt — chỉ lưu được ở dạng đồ thị (graph)", i+1)
		}
		if st.Assignment != nil { // same for a rule-based holder (S6a)
			return nil, invalidField("APPROVAL_NODE_KIND_UNSUPPORTED", fmt.Sprintf("steps[%d].assignment", i),
				"bước %d giao việc theo quy tắc (assignment) — chỉ lưu được ở dạng đồ thị (graph)", i+1)
		}
	}
	// Persist real ids: the first save of a legacy process writes the positional ids it has
	// been read with all along, so they stay with their steps from here on (FR-002).
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("services.buildApprovalStepsJSON: %w", err)
	}
	return b, nil
}

// validateApprovalSteps checks every step (or graph node, which shares the rules) and returns
// them normalized — trimmed, with stable ids (bpm.NormalizeStepsAvoiding: an id the process has
// already published under another step is never handed out again). Errors are located at
// steps[i]; the graph path relocates them (atGraphNodes).
func validateApprovalSteps(steps []ApprovalStepInput, reserved map[string]bool) ([]bpm.Step, error) {
	return validateApprovalStepsFor(steps, reserved, bpm.ValidApproverType, "user/role")
}

// validateApprovalStepsFor is validateApprovalSteps for a caller whose steps may name more
// assignee types than the linear v1 step (a graph node may name a work queue): valid says which,
// allowed spells them in the refusal.
func validateApprovalStepsFor(steps []ApprovalStepInput, reserved map[string]bool, valid func(bpm.ApproverType) bool, allowed string) ([]bpm.Step, error) {
	out := make([]bpm.Step, 0, len(steps))
	seen := make(map[string]int, len(steps))
	for i, st := range steps {
		if err := UnsupportedApprovalStepKeys(i, st.Unsupported); err != nil {
			return nil, err
		}
		// An explicit id is checked; a missing one is assigned below (bpm.NormalizeSteps) — a
		// client that sends no ids is never refused for it.
		id := strings.TrimSpace(st.ID)
		if id != "" {
			if !bpm.ValidNodeID(id) {
				return nil, invalidField("APPROVAL_NODE_ID_INVALID", fmt.Sprintf("steps[%d].id", i),
					"mã bước %d không hợp lệ: %q (chỉ a-z, 0-9, '_' hoặc '-', tối đa 60 ký tự)", i+1, st.ID)
			}
			if first, dup := seen[id]; dup {
				return nil, invalidField("APPROVAL_NODE_ID_DUPLICATE", fmt.Sprintf("steps[%d].id", i),
					"mã bước %q của bước %d trùng với bước %d", id, i+1, first+1)
			}
			seen[id] = i
		}
		at := bpm.ApproverType(strings.TrimSpace(st.ApproverType))
		approverID := strings.TrimSpace(st.ApproverID)
		var approvers []bpm.Approver
		var completion *bpm.Completion
		var assignment *bpm.Assignment
		if st.Assignment != nil {
			// A rule instead of a name (S6a): ONE body with the engine (bpm.CheckAssignment, reached through
			// CheckApprovers), so what it refuses to run, the API refuses to store. Anything else the step
			// names a holder by (approverType/Id, approvers, completion) is judged by the same call.
			a := st.Assignment
			if len(a.Unsupported) > 0 {
				return nil, invalidField("APPROVAL_NODE_KIND_UNSUPPORTED", fmt.Sprintf("steps[%d].assignment.%s", i, a.Unsupported[0]),
					"bước %d: assignment có khoá %s mà engine phê duyệt hiện tại chưa chạy được — bỏ khoá này", i+1, strings.Join(a.Unsupported, ", "))
			}
			assignment = &bpm.Assignment{
				Type: bpm.AssignmentType(strings.TrimSpace(a.Type)), ID: strings.TrimSpace(a.ID),
				Field: strings.TrimSpace(a.Field), Strategy: strings.TrimSpace(a.Strategy),
			}
			cand := bpm.Step{ApproverType: at, ApproverID: approverID, Assignment: assignment}
			for _, ap := range st.Approvers {
				cand.Approvers = append(cand.Approvers, bpm.Approver{Type: bpm.ApproverType(strings.TrimSpace(ap.Type)), ID: strings.TrimSpace(ap.ID)})
			}
			cand.Completion = st.Completion.completion()
			if f, reason := bpm.CheckApprovers(cand); f != "" {
				return nil, ApprovalStepShapeError(i, f, reason)
			}
			completion = cand.Completion
			if completion == nil && assignment.EachMember() { // write the default down, like a council's
				completion = &bpm.Completion{Mode: bpm.CompletionAll}
			}
		} else if st.parallel() {
			// ONE body with the engine (bpm.CheckApprovers): what it refuses to run, the API refuses to store.
			for _, a := range st.Approvers {
				approvers = append(approvers, bpm.Approver{Type: bpm.ApproverType(strings.TrimSpace(a.Type)), ID: strings.TrimSpace(a.ID)})
			}
			completion = st.Completion.completion()
			cand := bpm.Step{ApproverType: at, ApproverID: approverID, Approvers: approvers, Completion: completion}
			if f, reason := bpm.CheckApprovers(cand); f != "" {
				return nil, ApprovalStepShapeError(i, f, reason)
			}
			if completion == nil { // write the default down: a pinned step never depends on a default
				completion = &bpm.Completion{Mode: bpm.CompletionAll}
			}
		} else {
			if !valid(at) {
				return nil, invalid("APPROVAL_STEP_INVALID",
					"approverType bước %d không hợp lệ: %q (%s)", i+1, st.ApproverType, allowed)
			}
			if approverID == "" {
				return nil, invalid("APPROVAL_STEP_INVALID", "approverId bước %d là bắt buộc", i+1)
			}
		}
		if st.SLASeconds < 0 {
			return nil, invalid("APPROVAL_STEP_INVALID", "slaSeconds bước %d không được âm", i+1)
		}
		// B6 parity: the engine reads slaClock at run time, so a mistyped one must be refused
		// HERE — otherwise "giờ làm việc" silently becomes whatever an unknown value falls to.
		clock := strings.TrimSpace(st.SLAClock)
		if !bpm.ValidSLAClock(clock) {
			return nil, invalidField("APPROVAL_STEP_INVALID", fmt.Sprintf("steps[%d].slaClock", i),
				"slaClock bước %d không hợp lệ: %q (business = giờ làm việc, wall = giờ đồng hồ)", i+1, st.SLAClock)
		}
		out = append(out, bpm.Step{
			ID:           id,
			Name:         strings.TrimSpace(st.Name),
			ApproverType: at,
			ApproverID:   approverID,
			SLASeconds:   st.SLASeconds,
			SLAClock:     clock,
			Approvers:    approvers,
			Completion:   completion,
			Assignment:   assignment,
		})
	}
	return bpm.NormalizeStepsAvoiding(out, reserved), nil
}
