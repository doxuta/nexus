package bpm

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/workflow"
)

// Routing (specs/043 lát S6a, FR-016…FR-019): a step names its holder by RULE, not only by name.
//
// A node's `assignment` is one of
//
//	user · role · team · queue   a named holder. team and queue are POOLS — every member claims from
//	                             them (strategy `pool`, the default). A TEAM may instead be dealt out
//	                             by a strategy (S6b): round_robin (the next member in a ring),
//	                             load_based (the member with the fewest open seats) — both seat ONE
//	                             person — or each_member (one seat per member, judged by the step's
//	                             completion policy, at most MaxApprovers).
//	field                        the person a user-type field of the record points at (`owner_id`
//	                             = the record's current owner).
//	submitter                    the person who sent / started the approval (the record's owner when
//	                             the submitter is unknown — an API key, a rule).
//
// A pool needs no lookup: it opens the seat straight away (stepSeats). field, submitter and a team
// strategy are DYNAMIC: they are resolved ONCE when the instance enters the step (Router.Resolve /
// Router.ResolveTeam) and the seat that comes out is frozen (FR-017) — moving the field later does not move a step already
// opened. When nobody can be found the process's fallback assignee holds the step (FR-018); with
// none the instance stays pending with LastError set and no seat — visible to the Nexus team, never
// lost.

// AssignmentType is what an assignment names.
type AssignmentType string

const (
	AssignUser      AssignmentType = "user"
	AssignRole      AssignmentType = "role"
	AssignTeam      AssignmentType = "team"
	AssignQueue     AssignmentType = "queue"
	AssignField     AssignmentType = "field"
	AssignSubmitter AssignmentType = "submitter"
)

// Assignment strategies. They apply to a TEAM only (a user is one person, a role / queue is a pool
// by nature, field / submitter name one person): StrategyPool (or none) is the shared pool.
const (
	StrategyPool       = "pool"
	StrategyEachMember = "each_member"
	StrategyRoundRobin = "round_robin"
	StrategyLoadBased  = "load_based"
)

// Assignment is the rule a step names its holder by (see the file comment).
type Assignment struct {
	Type     AssignmentType `json:"type"`
	ID       string         `json:"id,omitempty"`    // user / role / team / queue
	Field    string         `json:"field,omitempty"` // field: the record field (owner_id = the owner)
	Strategy string         `json:"strategy,omitempty"`
}

// AssignmentKeys is the closed JSON key set of an assignment object.
var AssignmentKeys = []string{"type", "id", "field", "strategy"}

// OwnerField is the pseudo-field a `field` assignment uses for the record's current owner
// (obj_*.owner_id, else created_by): "chủ hồ sơ" is not a user-type field of the entity.
const OwnerField = "owner_id"

// static is the holder an assignment names outright; ok=false for field / submitter.
func (a Assignment) static() (Approver, bool) {
	if a.TeamStrategy() {
		return Approver{}, false
	}
	switch a.Type {
	case AssignUser, AssignRole, AssignTeam, AssignQueue:
		return Approver{Type: ApproverType(a.Type), ID: a.ID}, true
	}
	return Approver{}, false
}

// TeamStrategy reports whether a team is dealt out by a strategy (round_robin, load_based,
// each_member) instead of being a pool.
func (a Assignment) TeamStrategy() bool {
	return a.Type == AssignTeam && (a.Strategy == StrategyRoundRobin || a.Strategy == StrategyLoadBased || a.Strategy == StrategyEachMember)
}

// EachMember reports whether every member of the team gets a seat (the step is then parallel).
func (a *Assignment) EachMember() bool {
	return a != nil && a.Type == AssignTeam && a.Strategy == StrategyEachMember
}

// Stateful reports whether asking for the holder CHANGES something (the round-robin cursor turns,
// and a load tie rotates through it): such a lookup must happen once per ENTRY into the step, not
// once per attempt.
func (a *Assignment) Stateful() bool {
	return a != nil && a.Type == AssignTeam && (a.Strategy == StrategyRoundRobin || a.Strategy == StrategyLoadBased)
}

// Dynamic reports whether the holder is found at entry (field, submitter, a team strategy) rather
// than named.
func (a Assignment) Dynamic() bool {
	return a.Type == AssignField || a.Type == AssignSubmitter || a.TeamStrategy()
}

var fieldNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)

// CheckAssignment says why a step's assignment cannot run, as the key to blame (relative to the
// step) and a reason; ("", "") = runnable or no assignment. ONE body for the engine (a stored
// graph it cannot run is refused whole — fail-closed) and for the API that answers the admin.
func CheckAssignment(s Step) (field, reason string) {
	a := s.Assignment
	if a == nil {
		return "", ""
	}
	if s.ApproverType != "" || s.ApproverID != "" || len(s.Approvers) > 0 {
		return "assignment", "bước khai assignment thì bỏ approverType/approverId/approvers — một bước chỉ có một nguồn người nhận"
	}
	if s.Completion != nil && !a.EachMember() {
		return "assignment", fmt.Sprintf("bước khai assignment thì bỏ completion — chỉ nhóm giao theo %q (mỗi thành viên một ghế) mới có chính sách duyệt", StrategyEachMember)
	}
	switch a.Type {
	case AssignUser, AssignRole, AssignTeam, AssignQueue:
		if strings.TrimSpace(a.ID) == "" || a.ID != strings.TrimSpace(a.ID) {
			return "assignment.id", fmt.Sprintf("assignment.id là bắt buộc với loại %q", a.Type)
		}
		if a.Field != "" {
			return "assignment.field", fmt.Sprintf("assignment.field chỉ dùng với loại %q", AssignField)
		}
	case AssignField:
		if !fieldNamePattern.MatchString(a.Field) {
			return "assignment.field", fmt.Sprintf("assignment.field là tên trường của hồ sơ (hoặc %q — chủ hồ sơ), nhận: %q", OwnerField, a.Field)
		}
		if a.ID != "" {
			return "assignment.id", fmt.Sprintf("loại %q đọc người từ assignment.field, không dùng assignment.id", AssignField)
		}
	case AssignSubmitter:
		if a.ID != "" || a.Field != "" {
			return "assignment.id", fmt.Sprintf("loại %q là người gửi hồ sơ — không khai id/field", AssignSubmitter)
		}
	default:
		return "assignment.type", fmt.Sprintf("assignment.type không hợp lệ: %q (user/role/team/queue/field/submitter)", a.Type)
	}
	switch a.Strategy {
	case "", StrategyPool:
	case StrategyEachMember, StrategyRoundRobin, StrategyLoadBased:
		if a.Type != AssignTeam {
			return "assignment.strategy", fmt.Sprintf("chiến lược %q chỉ dùng được với nhóm (type %q) — %s đã là một người hoặc một bể", a.Strategy, AssignTeam, a.Type)
		}
	default:
		return "assignment.strategy", fmt.Sprintf("assignment.strategy không hợp lệ: %q (pool, each_member, round_robin, load_based)", a.Strategy)
	}
	if a.EachMember() { // the seats form a council: its policy is judged like a council's
		return checkCompletion(s.Completion)
	}
	return "", ""
}

// Options are the process-level settings (specs/043 plan "options", column sys_approval_processes
// .options). S6a opens one: the person or pool that holds a step nobody could be found for.
type Options struct {
	FallbackAssignee *Approver `json:"fallbackAssignee,omitempty"`
	// Lane is "" / "exclusive" (an approval) or "independent" (an SLA tracker, S12b).
	Lane string `json:"lane,omitempty"`
	// CalendarID pins the working calendar every deadline of this process counts on ("" = the
	// tenant default). S13.
	CalendarID string `json:"calendarId,omitempty"`
	// PauseWhen freezes the SLA clock while the record satisfies it (S13): the deadline's remaining
	// working time is kept and given back when the condition stops holding.
	PauseWhen *workflow.Condition `json:"pauseWhen,omitempty"`
}

// CalendarID is the pinned calendar of the definition ("" = default); nil-safe.
func (d *Definition) CalendarID() string {
	if d == nil || d.Options == nil {
		return ""
	}
	return d.Options.CalendarID
}

// OptionsKeys is the closed JSON key set of the process `options` object.
var OptionsKeys = []string{"fallbackAssignee", "lane", "calendarId", "pauseWhen"}

// ValidFallbackType reports whether t can hold a fallback seat (any seat holder but a rule).
func ValidFallbackType(t ApproverType) bool { return ValidAssigneeType(t) || t == ApproverTeam }

// CheckOptions says why the options cannot run (key to blame, reason); ("", "") = fine.
func CheckOptions(o *Options) (field, reason string) {
	if o == nil {
		return "", ""
	}
	if o.Lane != "" && o.Lane != LaneExclusive && o.Lane != LaneIndependent {
		return "lane", fmt.Sprintf("lane không hợp lệ: %q (exclusive/independent)", o.Lane)
	}
	if o.CalendarID != strings.TrimSpace(o.CalendarID) || len(o.CalendarID) > 36 {
		return "calendarId", "calendarId không hợp lệ (tối đa 36 ký tự, không có khoảng trắng đầu/cuối)"
	}
	if p := o.PauseWhen; p != nil {
		if len(p.Rules) == 0 {
			return "pauseWhen", "pauseWhen cần ít nhất một quy tắc"
		}
		if p.Timer != nil || p.SaveOn != "" {
			return "pauseWhen", "pauseWhen chỉ nhận match + rules"
		}
		for _, r := range p.Rules {
			if r.Op == workflow.OpChanged || r.Op == workflow.OpChangedTo {
				return "pauseWhen", "«vừa đổi»/«vừa đổi thành» không dùng được ở pauseWhen — nó xét giá trị hiện tại của hồ sơ, không có bản trước"
			}
		}
	}
	if o.FallbackAssignee == nil {
		return "", ""
	}
	f := o.FallbackAssignee
	if !ValidFallbackType(f.Type) {
		return "fallbackAssignee.type", fmt.Sprintf("fallbackAssignee.type không hợp lệ: %q (user/role/team/queue)", f.Type)
	}
	if strings.TrimSpace(f.ID) == "" || f.ID != strings.TrimSpace(f.ID) {
		return "fallbackAssignee.id", "fallbackAssignee.id là bắt buộc"
	}
	return "", ""
}

// RouteInput is what a Router needs to find the holder of a dynamic assignment.
type RouteInput struct {
	TenantID   string
	EntityName string
	RecordID   string
	// ProcessID + NodeID name the step being entered: the key a team strategy keeps its rotation
	// under (sys_assignment_cursors: workflow_id = process, action_key = "bpm:"+node).
	ProcessID string
	NodeID    string
	// Submitter is the user who sent / started the approval ("" = unknown: an API key or a rule).
	Submitter string
}

// Router resolves a DYNAMIC assignment (field, submitter) to the holder it points at right now.
// found=false means "nobody": the field is empty, the person is gone or inactive, the record is
// gone — not an error. err is reserved for a store that could not be read. Satisfied by
// services.BPMRouter. Optional: unwired, every dynamic step has no holder (fallback, else stuck).
//
// ResolveTeam deals a TEAM out by its strategy (S6b): the member the ring turns to (round_robin),
// the member with the fewest open seats (load_based), or every active member (each_member). Empty =
// nobody (an empty team); err only for a store that could not be read. Every holder is a user.
type Router interface {
	Resolve(ctx context.Context, in RouteInput, a Assignment) (holder Approver, found bool, err error)
	ResolveTeam(ctx context.Context, in RouteInput, a Assignment) (holders []Approver, err error)
}

// routeSeats returns the seats `step` opens for an instance entering it: the named holder, or the
// one a dynamic assignment resolves to, or the process's fallback assignee. stuck != "" (with no
// seats) when nobody holds it. err only for a store failure of the Router — the caller decides
// (a person waiting is told; the async arming path leaves the instance stuck).
func routeSeats(ctx context.Context, rt Router, in RouteInput, opts *Options, step Step, due *time.Time, basis SLABasis) (seats []Seat, stuck string, err error) {
	if seats = stepSeats(step, due, basis); len(seats) > 0 || step.Assignment == nil || !step.Assignment.Dynamic() {
		return seats, "", nil
	}
	in.NodeID = step.ID
	var holders []Approver
	if rt != nil {
		if step.Assignment.TeamStrategy() {
			if holders, err = rt.ResolveTeam(ctx, in, *step.Assignment); err != nil {
				return nil, "", fmt.Errorf("bpm.routeSeats: %w", err)
			}
			if len(holders) > MaxApprovers { // a council of thousands is a role, not a list (the snapshot is capped)
				holders = holders[:MaxApprovers]
			}
		} else {
			holder, found, rerr := rt.Resolve(ctx, in, *step.Assignment)
			if rerr != nil {
				return nil, "", fmt.Errorf("bpm.routeSeats: %w", rerr)
			}
			if found {
				holders = []Approver{holder}
			}
		}
	}
	if len(holders) == 0 && opts != nil && opts.FallbackAssignee != nil {
		holders = []Approver{*opts.FallbackAssignee}
	}
	if len(holders) == 0 {
		return nil, stuckMessage(step), nil
	}
	kind := SeatKindApproval
	if step.IsTask() {
		kind = SeatKindTask
	}
	seats = make([]Seat, len(holders))
	for i, h := range holders {
		seats[i] = Seat{NodeID: step.ID, AssigneeType: h.Type, AssigneeID: h.ID, Kind: kind, DueAt: due, DueBasis: basis}
	}
	return seats, "", nil
}

// maxLastErrorRunes is the width of sys_approval_instances.last_error (VARCHAR(500)).
const maxLastErrorRunes = 500

// stuckMessage is the customer-readable reason a step has no holder (FR-018).
func stuckMessage(step Step) string {
	name := strings.TrimSpace(step.Name)
	if name == "" {
		name = step.ID
	}
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200]) + "…"
	}
	return fmt.Sprintf("Không tìm được người nhận cho bước «%s» và quy trình chưa khai người dự phòng — cần người quản trị xử lý.", name)
}

// routeErrMessage is the last_error of an instance left stuck by a store failure (the async arming
// path, where nobody is waiting to be told).
const routeErrMessage = "Không tìm được người nhận lúc này vì lỗi hệ thống — cần người quản trị xử lý."

// maxNotifyHolders bounds how many people one step entry pings (FR-019): a role of thousands is a
// pool people look into, not a mailing list.
const maxNotifyHolders = 50

// notifyHolders pings every person who holds one of the seats a step just opened — the named
// user, and for a role / team / queue seat the people behind it (store.ResolveSeatUsers, tenant
// pinned, at most maxNotifyHolders) — that a record awaits them. Best-effort: a failure is a log
// line, never a failed decision. inbox == nil disables it.
func notifyHolders(ctx context.Context, store InstanceStore, inbox InboxWriter, logger *zap.Logger, tenantID, entity, recordID string, step Step, tail string, seats []Seat) {
	if inbox == nil || len(seats) == 0 {
		return
	}
	var users []string
	if store != nil {
		var err error
		if users, err = store.ResolveSeatUsers(ctx, tenantID, seats, maxNotifyHolders); err != nil {
			logger.Warn("bpm: resolving the holders of a step failed — some will not be pinged (fail-open)",
				zap.String("tenant_id", tenantID), zap.String("record_id", recordID), zap.Error(err))
		}
	} else { // no directory wired (a unit test): the users the seats name outright
		for _, st := range seats {
			if st.AssigneeType == ApproverUser && st.AssigneeID != "" {
				users = append(users, st.AssigneeID)
			}
		}
	}
	title, msg := stepNotice(step, entity, tail)
	for _, uid := range users {
		if _, err := inbox.InsertNotification(ctx, Notification{
			TenantID:   tenantID,
			UserID:     uid,
			Title:      title,
			Type:       "approval",
			Message:    msg,
			EntityName: entity,
			RecordID:   recordID,
		}); err != nil {
			logger.Warn("bpm: approval notice failed (fail-open)",
				zap.String("tenant_id", tenantID), zap.String("record_id", recordID), zap.Error(err))
		}
	}
}

// WithRouter wires the resolver of field / submitter assignments. Chainable. Unwired, such a step
// has no holder: the process's fallback assignee, else the instance is stuck (routeSeats).
func (e *AsyncEngine) WithRouter(rt Router) *AsyncEngine {
	e.router = rt
	return e
}

// WithRouter — see AsyncEngine.WithRouter.
func (d *Decider) WithRouter(rt Router) *Decider {
	d.router = rt
	return d
}

// WithRouter — see AsyncEngine.WithRouter.
func (r *Requester) WithRouter(rt Router) *Requester {
	r.router = rt
	return r
}

// RequireRouting is the boot-time wiring check, like RequireDeadlines: each of the three places that
// open a step must have the router. Missing on one, a "the person in this field" step would be
// stuck there only — a silent difference between how a record is sent and how it is decided on.
func RequireRouting(e *AsyncEngine, d *Decider, r *Requester) error {
	var missing []string
	if e == nil || e.router == nil {
		missing = append(missing, "engine")
	}
	if d == nil || d.router == nil {
		missing = append(missing, "decider")
	}
	if r == nil || r.router == nil {
		missing = append(missing, "requester")
	}
	if len(missing) > 0 {
		return fmt.Errorf("bpm.RequireRouting: approval router not wired on %v", missing)
	}
	return nil
}
