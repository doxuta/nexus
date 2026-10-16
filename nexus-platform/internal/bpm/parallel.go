package bpm

import (
	"fmt"
	"strings"
	"time"
)

// Parallel approval (specs/043 lát S4, FR-020…FR-023): one step held by SEVERAL people at once.
//
// A parallel step opens one seat per approver on the same visit (same node id, same entry_seq).
// Each approver decides their own seat; the step is over when Completion says so — `all` (everyone
// approves, a single rejection ends it) or `any` (the first decision, either way, ends it). The
// vote is counted in the transaction that records it (SQLRepository.VoteTaskTx), under the
// instance's row lock, so two last approvers pressing at once lose no vote and advance the step
// exactly once. S6b adds `quorum`: the step ends when percent% of the seats approved, or as soon
// as that many can no longer be reached.

// Approver names one holder of a seat of a parallel step: a user, or anyone holding a role.
type Approver struct {
	Type ApproverType `json:"type"`
	ID   string       `json:"id"`
}

// CompletionMode says when a parallel step is over.
type CompletionMode string

const (
	// CompletionAll: every seat approves; one rejection rejects the step at once.
	CompletionAll CompletionMode = "all"
	// CompletionAny: the first decision — approve or reject — decides the step.
	CompletionAny CompletionMode = "any"
	// CompletionQuorum: approved once ceil(percent x seats / 100) people approved (S6b); rejected as
	// soon as that many approvals can no longer be reached.
	CompletionQuorum CompletionMode = "quorum"
)

// ValidCompletionMode reports whether m is a mode this binary runs.
func ValidCompletionMode(m CompletionMode) bool {
	return m == CompletionAll || m == CompletionAny || m == CompletionQuorum
}

// Completion is the policy of a parallel step. A `completion` object states its mode (the key may
// be left out altogether — the step then means `all`). Percent (1..100) goes with `quorum` and
// only with it.
type Completion struct {
	Mode    CompletionMode `json:"mode"`
	Percent int            `json:"percent,omitempty"`
}

// checkCompletion says why a completion policy cannot run, as the key to blame (relative to the
// step) and a reason; ("", "") = runnable. Parity (B6): `percent` is read at vote time only under
// `quorum`, so a percent beside any other mode is refused here instead of being ignored.
func checkCompletion(c *Completion) (field, reason string) {
	if c == nil {
		return "", ""
	}
	if !ValidCompletionMode(c.Mode) {
		return "completion.mode", fmt.Sprintf("completion.mode không hợp lệ: %q (all = tất cả phải duyệt, any = người đầu tiên quyết, quorum = đủ phần trăm)", c.Mode)
	}
	if c.Mode == CompletionQuorum {
		if c.Percent < 1 || c.Percent > 100 {
			return "completion.percent", fmt.Sprintf("completion.percent của quorum phải từ 1 đến 100 (nhận: %d)", c.Percent)
		}
	} else if c.Percent != 0 {
		return "completion.percent", fmt.Sprintf("completion.percent chỉ dùng với mode %q (đang là %q)", CompletionQuorum, c.Mode)
	}
	return "", ""
}

// QuorumNeed is how many approvals carry a quorum of percent% over total seats: the ceiling, at
// least one, never more than the seats there are. 0 when there are no seats.
func QuorumNeed(percent, total int) int {
	if total <= 0 {
		return 0
	}
	need := (percent*total + 99) / 100
	if need < 1 {
		need = 1
	}
	if need > total {
		need = total
	}
	return need
}

// MaxApprovers bounds the seats of one step (a council of thousands is a role, not a list; the
// vote transaction and the breach fan-out stay small).
const MaxApprovers = 50

// Parallel reports whether the step is held by several approvers: the `approvers` form, or a team
// whose every member gets a seat (assignment strategy each_member, S6b) — the seats are known only
// when the step is entered, but the step is judged as a council from the start.
func (s Step) Parallel() bool { return len(s.Approvers) > 0 || s.Assignment.EachMember() }

// CompletionMode returns the step's policy: its own, else `all` — the safe reading of "everyone
// named must sign".
func (s Step) CompletionMode() CompletionMode {
	if s.Completion != nil {
		return s.Completion.Mode
	}
	return CompletionAll
}

// CompletionPolicy is the step's whole policy (mode + quorum percent) as the vote judges it.
func (s Step) CompletionPolicy() Completion {
	if s.Completion != nil {
		return *s.Completion
	}
	return Completion{Mode: CompletionAll}
}

// CheckApprovers says why a step's approver configuration cannot run, as the key to blame
// (relative to the step) and a reason; ("", "") = runnable. THE one body: the engine uses it to
// refuse a stored step it cannot run (fail-closed, a nil chain), the API to answer the admin who
// sent it — so the two never disagree on what a runnable step is.
func CheckApprovers(s Step) (field, reason string) {
	if s.Assignment != nil { // S6a: a rule instead of a name — judged by its own body
		return CheckAssignment(s)
	}
	if !s.Parallel() {
		if s.Completion != nil {
			return "completion", "completion chỉ dùng cùng approvers — bước một người không có chính sách tất cả/người đầu tiên"
		}
		if !ValidApproverType(s.ApproverType) {
			return "approverType", fmt.Sprintf("approverType không hợp lệ: %q (user/role)", s.ApproverType)
		}
		if strings.TrimSpace(s.ApproverID) == "" {
			return "approverId", "approverId là bắt buộc"
		}
		return "", ""
	}
	if s.ApproverType != "" || s.ApproverID != "" {
		return "approverType", "bước nhiều người duyệt chỉ khai approvers — bỏ approverType/approverId (một bước chỉ có một nguồn người duyệt)"
	}
	if len(s.Approvers) < 2 {
		return "approvers", "bước nhiều người duyệt cần ít nhất 2 người (một người thì dùng approverType/approverId)"
	}
	if len(s.Approvers) > MaxApprovers {
		return "approvers", fmt.Sprintf("một bước có tối đa %d người duyệt", MaxApprovers)
	}
	seen := make(map[Approver]bool, len(s.Approvers))
	for _, a := range s.Approvers {
		if !ValidApproverType(a.Type) {
			return "approvers", fmt.Sprintf("approvers có loại không hợp lệ: %q (user/role)", a.Type)
		}
		if strings.TrimSpace(a.ID) == "" || a.ID != strings.TrimSpace(a.ID) {
			return "approvers", "approvers có người duyệt thiếu id"
		}
		if seen[a] {
			return "approvers", fmt.Sprintf("approvers có %s %q hai lần", a.Type, a.ID)
		}
		seen[a] = true
	}
	return checkCompletion(s.Completion)
}

// Verdict is what the votes of a visit add up to.
type Verdict string

const (
	VerdictPending  Verdict = "pending"
	VerdictApproved Verdict = "approved"
	VerdictRejected Verdict = "rejected"
)

// evaluateCompletion is the pure rule of a parallel step (T078). total counts the seats still in
// play — open ones plus the votes cast (a handed-on or withdrawn seat is not one).
//
//	all: any rejection rejects; approved == total approves; otherwise pending.
//	any: the first decision decides. Both kinds of vote at once cannot happen (the first one ends
//	     the step); if a bug ever produced it, the safe answer is the rejection.
func evaluateCompletion(mode CompletionMode, total, approved, rejected int) Verdict {
	switch {
	case rejected > 0:
		return VerdictRejected
	case mode == CompletionAny && approved > 0:
		return VerdictApproved
	case mode != CompletionAny && total > 0 && approved >= total:
		return VerdictApproved
	default:
		return VerdictPending
	}
}

// evaluateVotes is evaluateCompletion for a whole policy: `quorum` needs the percent too.
//
//	quorum: need = QuorumNeed(percent, total); approved >= need approves; rejected > total-need
//	        rejects (need approvals can no longer be reached); otherwise pending.
func evaluateVotes(c Completion, total, approved, rejected int) Verdict {
	if c.Mode != CompletionQuorum {
		return evaluateCompletion(c.Mode, total, approved, rejected)
	}
	need := QuorumNeed(c.Percent, total)
	switch {
	case need == 0:
		return VerdictPending
	case approved >= need:
		return VerdictApproved
	case rejected > total-need:
		return VerdictRejected
	default:
		return VerdictPending
	}
}

// GraphFromSteps lifts a linear step list into the graph form (every step an approval node, in
// order): the form a step carrying `approvers` must be stored in (D-2 — the linear column is
// what a pre-S4 binary reads, and it must not walk a parallel step as one approver).
func GraphFromSteps(steps []Step) *Graph {
	g := &Graph{Nodes: make([]Node, len(steps))}
	for i, s := range steps {
		g.Nodes[i] = Node{Step: s, Kind: NodeApproval}
	}
	return g
}

// VisitSeat is one seat of the visit an instance now stands on, in any state — what the detail
// screen shows ("2/3 — cần 2", who decided what) and what a vote is judged against.
type VisitSeat struct {
	ID           string
	NodeID       string
	AssigneeType ApproverType
	AssigneeID   string
	// Status is open | done | cancelled (sys_bpm_tasks.status).
	Status string
	// Outcome is how a closed seat ended ("" while open).
	Outcome       SeatOutcome
	DecidedBy     string
	Comment       string
	DelegatedFrom string
	// Kind is what the seat asked of its holder: approval | task | rework (SeatKind*).
	Kind        string
	EntrySeq    int64
	Version     int
	CompletedAt *time.Time
}

// voted reports whether the seat holds a vote: approve or reject. A reassigned (handed on),
// superseded or recalled seat is not one.
func (v VisitSeat) voted() bool { return v.Outcome == SeatApproved || v.Outcome == SeatRejected }

// held reports whether the actor may use this seat: it is open and names them, or a role they hold.
func (v VisitSeat) held(userID string, roleIDs []string) bool {
	return v.Status == "open" && Seat{AssigneeType: v.AssigneeType, AssigneeID: v.AssigneeID}.heldBy(userID, roleIDs)
}

// ActorSeat picks the open seat the actor decides with: a seat naming them first (the named seat
// is the more specific claim), else a role seat of theirs. nil = they hold none.
func ActorSeat(visit []VisitSeat, actor Actor) *VisitSeat {
	var byRole *VisitSeat
	for i := range visit {
		v := &visit[i]
		if !v.held(actor.UserID, actor.HolderIDs()) {
			continue
		}
		if v.AssigneeType == ApproverUser {
			return v
		}
		if byRole == nil {
			byRole = v
		}
	}
	return byRole
}

// DecidedIn reports whether the user already cast a vote in this visit (FR-022).
func DecidedIn(visit []VisitSeat, userID string) bool {
	if userID == "" {
		return false
	}
	for _, v := range visit {
		if v.voted() && v.DecidedBy == userID {
			return true
		}
	}
	return false
}

// Tally counts a visit: seats still open, approvals, rejections. total = open + approved + rejected.
func Tally(visit []VisitSeat) (open, approved, rejected int) {
	for _, v := range visit {
		switch {
		case v.Status == "open":
			open++
		case v.Outcome == SeatApproved:
			approved++
		case v.Outcome == SeatRejected:
			rejected++
		}
	}
	return open, approved, rejected
}

// castVote is the whole decision of one vote, pure: given the seats of the visit as the vote
// transaction read them (under the instance's row lock), the seat being used and who uses it, it
// returns the visit as it stands after the vote and what that adds up to. The SQL transaction and
// the in-memory test double both call it, so they cannot disagree on the rule.
//
// ErrConcurrent: the seat is not open any more (the step moved on, the seat was handed on or the
// request withdrawn) — the click is stale. ErrAlreadyDecidedByActor: the user has voted in this
// visit already, on another seat of theirs (FR-022).
func castVote(visit []VisitSeat, seatID, userID string, kind SeatOutcome, policy Completion, comment string, at time.Time) ([]VisitSeat, Verdict, error) {
	idx := -1
	for i, v := range visit {
		if v.ID == seatID && v.Status == "open" {
			idx = i
		}
	}
	if idx < 0 {
		return nil, "", ErrConcurrent
	}
	if DecidedIn(visit, userID) {
		return nil, "", ErrAlreadyDecidedByActor
	}
	after := append([]VisitSeat(nil), visit...)
	after[idx].Status, after[idx].Outcome, after[idx].DecidedBy, after[idx].Comment = "done", kind, userID, comment
	after[idx].CompletedAt, after[idx].Version = &at, visit[idx].Version+1
	open, approved, rejected := Tally(after)
	return after, evaluateVotes(policy, open+approved+rejected, approved, rejected), nil
}

// VoteGuard is what a vote on a parallel step proves under the instance's row lock. There is NO
// seq to match, unlike TaskGuard: a vote on one seat must not be lost because another seat voted
// a moment before (that is exactly the concurrent-last-two case) — what makes a vote stale is the
// step having moved on (ExpectStep) or the seat not being open any more (the seat's own CAS).
type VoteGuard struct {
	Actor Actor
	// ExpectStep is the current_step the caller read.
	ExpectStep int
	// Node is the stable id of that step; SeatID the open seat the actor decides with.
	Node, SeatID string
	// Kind is SeatApproved or SeatRejected; Mode (+ Percent for a quorum) the step's policy.
	Kind    SeatOutcome
	Mode    CompletionMode
	Percent int
	// Comment is the actor's note, kept on their seat.
	Comment string
	// Entry is the history entry of this vote. The transaction marks it Partial when the vote did
	// not finish the step, and stamps CompleteBasis on it when it did (the basis of the deadline
	// the NEXT step got).
	Entry         HistoryEntry
	CompleteBasis SLABasis
	// Unrouted: the transition handed to VoteTaskTx was prepared WITHOUT routing the step after this
	// one (the caller expected the vote to leave the step open). If the vote finishes it after all,
	// the transaction writes nothing and returns errRouteNeeded.
	Unrouted bool
}

// policy is the step's policy as the guard carries it.
func (g VoteGuard) policy() Completion { return Completion{Mode: g.Mode, Percent: g.Percent} }

// VoteResult is what one vote did.
type VoteResult struct {
	// Completed: the vote finished the step and the prepared transition was applied.
	Completed bool
	Verdict   Verdict
	// Open / Approved / Rejected count the visit after the vote.
	Open, Approved, Rejected int
	// History is the instance's history after the vote.
	History []HistoryEntry
}
