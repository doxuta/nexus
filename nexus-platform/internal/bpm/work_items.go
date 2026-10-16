package bpm

// Work items (specs/043 S5): the inbox a person works from, read off the work seats
// (sys_bpm_tasks), and the two verbs that act on a POOL seat — claim and release. Approving,
// rejecting, delegating and completing are not here: they are decisions, and every decision goes
// through DecideTaskTx (one guard, no verb writes around it).
//
// A work item is one open seat the person holds or could claim, or one they finished:
//
//	mine  open seats the person claimed from a pool, plus the seats that name them
//	pool  open, unclaimed seats of a role they hold or a queue visible to them ("Chờ nhận")
//	done  seats they decided or completed
//
// Every statement is pinned to the tenant (the first predicate of every branch); a seat of another
// tenant is never matched, listed, counted or claimed.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WorkItemView selects which of the three groups a list/count reads.
type WorkItemView string

const (
	ViewMine WorkItemView = "mine"
	ViewPool WorkItemView = "pool"
	ViewDone WorkItemView = "done"
)

// ValidWorkItemView reports whether v is one of the three views.
func ValidWorkItemView(v WorkItemView) bool { return v == ViewMine || v == ViewPool || v == ViewDone }

// Claim / release outcomes the service maps to coded errors.
var (
	// ErrSeatNotFound: no such seat in the tenant, or the caller is not a candidate of it — one
	// answer for both, so a stranger cannot tell a real item from a made-up id.
	ErrSeatNotFound = errors.New("bpm: work item not found")
	// ErrSeatNotOpen: the seat (or its instance) has already ended — decided, recalled, moved on.
	ErrSeatNotOpen = errors.New("bpm: work item is no longer open")
	// ErrSeatNotClaimable: a user seat already has its one holder; only role/queue seats are pools.
	ErrSeatNotClaimable = errors.New("bpm: work item is not a pool item")
	// ErrSeatNotClaimed: release of a seat nobody has claimed.
	ErrSeatNotClaimed = errors.New("bpm: work item is not claimed")
	// ErrNotClaimer: release by a candidate who is neither the claimer nor an admin.
	ErrNotClaimer = errors.New("bpm: only the claimer may release this work item")
)

// WorkItem is one seat as the inbox shows it.
type WorkItem struct {
	ID           string
	InstanceID   string
	ProcessID    string
	ProcessName  string
	EntityName   string
	RecordID     string
	NodeID       string
	NodeName     string // the step's name in the instance's pinned definition ("" if unpinned/unknown)
	Kind         string // SeatKind*
	AssigneeType ApproverType
	AssigneeID   string
	AssigneeName string // user name / role label / queue label; "" when it cannot be resolved
	ClaimedBy    string
	ClaimedAt    *time.Time
	Status       string // open | done | cancelled
	Outcome      string
	DueAt        *time.Time
	DueBasis     SLABasis
	Paused       bool // the instance's SLA clock is stopped
	CreatedAt    time.Time
	CompletedAt  *time.Time
	// RequiredFields are the fields a task must have filled before it completes (from the pinned
	// definition; nil for an approval seat).
	RequiredFields []string
	// ReturnReason (an OPEN rework item only, S8): why the approval came back to the submitter — the
	// rejector's note.
	ReturnReason string
	// StepIndex is the index of the seat's step in the instance's chain — the "step the client saw"
	// a decision on it is bound to (Decider.DecideAt expectedStep); -1 when it cannot be told.
	StepIndex int

	// Seat is the same seat as the engine judges authority over (candidate / claim checks).
	Seat Seat
}

// WorkItemCursor is the keyset position after the last row of a page: the sort timestamp and the
// seat id (the tie-break), in the direction of the sort (newest first).
type WorkItemCursor struct {
	At time.Time
	ID string
}

// EncodeCursor / DecodeWorkItemCursor turn the position into the opaque string clients hand back.
func (c WorkItemCursor) Encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.At.UTC().Format(time.RFC3339Nano) + "|" + c.ID))
}

// DecodeWorkItemCursor parses a cursor produced by Encode; ok=false for anything else.
func DecodeWorkItemCursor(s string) (WorkItemCursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return WorkItemCursor{}, false
	}
	at, id, found := strings.Cut(string(raw), "|")
	if !found || id == "" {
		return WorkItemCursor{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return WorkItemCursor{}, false
	}
	return WorkItemCursor{At: t, ID: id}, true
}

// WorkItemQuery describes one inbox read. UserID/RoleIDs say who is asking: RoleIDs is the caller's
// HOLDER ids — the one role the JWT carries plus their teams (Actor.HolderIDs), so a role pool and
// a team pool are found by the same list. QueueEntities, when RestrictQueues, limits queue seats to those record entities — the
// ones the caller may view (the service resolves ACL; this layer only filters).
type WorkItemQuery struct {
	View           WorkItemView
	UserID         string
	RoleIDs        []string
	Entity         string
	RestrictQueues bool
	QueueEntities  []string
	Limit          int
	After          *WorkItemCursor
}

// seatBranch is one disjoint slice of a view: a WHERE tail (tenant first) and its arguments.
type seatBranch struct {
	where string
	args  []any
}

// branches builds the disjoint index-friendly slices of the view. Not one WHERE with OR: MySQL
// cannot range-scan an OR across idx_bt_claimed and idx_bt_inbox (the lesson of ListPendingForUser).
func (q WorkItemQuery) branches(tenantID string) []seatBranch {
	var out []seatBranch
	switch q.View {
	case ViewMine:
		// A seat the user claimed stays theirs only while they are still a candidate of it — the
		// holder rule /approvals/pending applies (a claimer who lost the role, or whose queue was
		// switched off or whose entity they may no longer view, does not carry a ghost item).
		holder, hargs := holderPredicate("t.assignee_type", "t.assignee_id", q.UserID, q.RoleIDs, tenantID)
		claimed := seatBranch{"t.tenant_id = ? AND t.status = 'open' AND t.claimed_by = ? AND (" + holder + ")",
			append([]any{tenantID, q.UserID}, hargs...)}
		if q.RestrictQueues {
			if len(q.QueueEntities) == 0 {
				claimed.where += " AND t.assignee_type <> 'queue'"
			} else {
				claimed.where += " AND (t.assignee_type <> 'queue' OR t.entity_name IN (" + placeholders(len(q.QueueEntities)) + "))"
				for _, e := range q.QueueEntities {
					claimed.args = append(claimed.args, e)
				}
			}
		}
		out = append(out, claimed,
			seatBranch{"t.tenant_id = ? AND t.status = 'open' AND t.assignee_type = 'user' AND t.assignee_id = ? AND t.claimed_by IS NULL", []any{tenantID, q.UserID}},
		)
	case ViewPool:
		if len(q.RoleIDs) > 0 {
			in := placeholders(len(q.RoleIDs))
			args := []any{tenantID}
			for _, r := range q.RoleIDs {
				args = append(args, r)
			}
			out = append(out, seatBranch{
				"t.tenant_id = ? AND t.status = 'open' AND t.assignee_type IN ('role', 'team') AND t.assignee_id IN (" + in + ") AND t.claimed_by IS NULL", args})
		}
		if !q.RestrictQueues || len(q.QueueEntities) > 0 {
			in := placeholders(len(q.RoleIDs))
			where := "t.tenant_id = ? AND t.status = 'open' AND t.assignee_type = 'queue' AND t.claimed_by IS NULL AND t.assignee_id IN (" + visibleQueuesSQL(in) + ")"
			args := []any{tenantID, tenantID}
			for _, r := range q.RoleIDs {
				args = append(args, r)
			}
			if q.RestrictQueues {
				where += " AND t.entity_name IN (" + placeholders(len(q.QueueEntities)) + ")"
				for _, e := range q.QueueEntities {
					args = append(args, e)
				}
			}
			out = append(out, seatBranch{where, args})
		}
	case ViewDone:
		out = append(out, seatBranch{"t.tenant_id = ? AND t.status = 'done' AND t.decided_by = ?", []any{tenantID, q.UserID}})
	}
	for i := range out {
		if q.Entity != "" {
			out[i].where += " AND t.entity_name = ?"
			out[i].args = append(out[i].args, q.Entity)
		}
	}
	return out
}

// placeholders returns n comma-joined "?" ("" for n <= 0).
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// sortColumn is the timestamp a view sorts (newest first) and pages by.
func (q WorkItemQuery) sortColumn() string {
	if q.View == ViewDone {
		return "t.completed_at"
	}
	return "t.created_at"
}

// workItemRow is one row of the work-item select.
type workItemRow struct {
	ID              string           `db:"id"`
	InstanceID      string           `db:"instance_id"`
	ProcessID       string           `db:"process_id"`
	ProcessName     *string          `db:"process_name"`
	EntityName      string           `db:"entity_name"`
	RecordID        string           `db:"record_id"`
	NodeID          string           `db:"node_id"`
	Kind            string           `db:"kind"`
	AssigneeType    string           `db:"assignee_type"`
	AssigneeID      string           `db:"assignee_id"`
	AssigneeName    *string          `db:"assignee_name"`
	ClaimedBy       *string          `db:"claimed_by"`
	ClaimedAt       *time.Time       `db:"claimed_at"`
	Status          string           `db:"status"`
	Outcome         *string          `db:"outcome"`
	DueAt           *time.Time       `db:"due_at"`
	DueBasis        *string          `db:"due_basis"`
	Paused          bool             `db:"paused"`
	CreatedAt       time.Time        `db:"created_at"`
	CompletedAt     *time.Time       `db:"completed_at"`
	ProcessSnapshot *json.RawMessage `db:"process_snapshot"`
	QueueLive       bool             `db:"queue_live"`
	QueueRoleID     *string          `db:"queue_role_id"`
	Note            *string          `db:"note"`
}

// workItemSelect is the column list + joins shared by the list and the single read. Names are
// resolved in the same statement (user full name or email, role label, queue label), every join
// pinned to the seat's tenant. The pinned definition rides along to name the step and list the
// task's required fields — decoded in Go for the page only.
const workItemSelect = `SELECT t.id, t.instance_id, t.process_id, p.name AS process_name, t.entity_name, t.record_id,
		t.node_id, t.kind, t.assignee_type, t.assignee_id,
		COALESCE(u.full_name, u.email, r.label, r.name, tm.name, q.label) AS assignee_name,
		t.claimed_by, t.claimed_at, t.status, t.outcome, t.due_at, t.due_basis, t.comment AS note,
		(i.sla_paused_at IS NOT NULL) AS paused, t.created_at, t.completed_at, i.process_snapshot,
		` + queueSeatCols + `
	FROM sys_bpm_tasks t
	JOIN sys_approval_instances i ON i.tenant_id = t.tenant_id AND i.id = t.instance_id
	LEFT JOIN sys_approval_processes p ON p.tenant_id = t.tenant_id AND p.id = t.process_id
	LEFT JOIN users u ON t.assignee_type = 'user' AND u.tenant_id = t.tenant_id AND u.id = t.assignee_id
	LEFT JOIN acl_roles r ON t.assignee_type = 'role' AND r.tenant_id = t.tenant_id AND r.id = t.assignee_id
	LEFT JOIN teams tm ON t.assignee_type = 'team' AND tm.tenant_id = t.tenant_id AND tm.id = t.assignee_id
	` + queueSeatJoin

func (row workItemRow) item() WorkItem {
	it := WorkItem{
		ID: row.ID, InstanceID: row.InstanceID, ProcessID: row.ProcessID,
		EntityName: row.EntityName, RecordID: row.RecordID, NodeID: row.NodeID, Kind: row.Kind,
		AssigneeType: ApproverType(row.AssigneeType), AssigneeID: row.AssigneeID,
		ClaimedAt: row.ClaimedAt, Status: row.Status, DueAt: row.DueAt, Paused: row.Paused,
		CreatedAt: row.CreatedAt, CompletedAt: row.CompletedAt, StepIndex: -1,
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	it.ProcessName, it.AssigneeName, it.ClaimedBy = str(row.ProcessName), str(row.AssigneeName), str(row.ClaimedBy)
	it.Outcome, it.DueBasis = str(row.Outcome), SLABasis(str(row.DueBasis))
	if row.Kind == SeatKindRework && row.Status == "open" {
		it.ReturnReason = str(row.Note)
	}
	it.Seat = seatReadRow{
		NodeID: row.NodeID, AssigneeType: row.AssigneeType, AssigneeID: row.AssigneeID, Kind: row.Kind,
		ClaimedBy: row.ClaimedBy, QueueLive: row.QueueLive, QueueRoleID: row.QueueRoleID,
	}.seat()
	if row.ProcessSnapshot != nil && len(*row.ProcessSnapshot) > 0 && string(*row.ProcessSnapshot) != "null" {
		var def Definition
		if json.Unmarshal(*row.ProcessSnapshot, &def) == nil {
			def.Steps = NormalizeSteps(def.Steps)
			def.Graph = def.Graph.normalized()
			for i, s := range def.Chain() {
				if s.ID == row.NodeID {
					it.NodeName, it.StepIndex = s.Name, i
					it.RequiredFields = def.RequiredFieldsAt(i)
					break
				}
			}
		}
	}
	return it
}

// ListWorkItems returns up to q.Limit+1 work items of the view, newest first, keyset-paged: the
// caller treats an extra row as "there is a next page" and builds the cursor from the last row it
// keeps. Each branch of the view is an index range scan limited and ordered on its own, merged and
// re-limited — never an OR (see branches) — then the page's rows are joined for names and the
// pinned definition.
func (r *SQLRepository) ListWorkItems(ctx context.Context, tenantID string, q WorkItemQuery) ([]WorkItem, error) {
	if !ValidWorkItemView(q.View) {
		return nil, fmt.Errorf("bpm.SQLRepository.ListWorkItems: unknown view %q", q.View)
	}
	limit := q.Limit
	if limit < 1 {
		limit = 50
	}
	limit++ // one extra row tells the caller there is a next page
	branches := q.branches(tenantID)
	if len(branches) == 0 {
		return []WorkItem{}, nil
	}
	col := q.sortColumn()
	var parts []string
	var args []any
	for _, b := range branches {
		where := b.where
		bargs := append([]any{}, b.args...)
		if q.After != nil {
			where += " AND (" + col + " < ? OR (" + col + " = ? AND t.id < ?))"
			bargs = append(bargs, q.After.At.UTC(), q.After.At.UTC(), q.After.ID)
		}
		// The instance must still be pending INSIDE each limited branch: filtering after the LIMIT
		// would drop a stale open seat (a leftover of a pre-S3a binary) from a full page and hide
		// that a next page exists.
		join := ""
		if q.View != ViewDone {
			join = " JOIN sys_approval_instances pi ON pi.tenant_id = t.tenant_id AND pi.id = t.instance_id AND pi.status = 'pending'"
		}
		parts = append(parts, "(SELECT t.id, "+col+" AS sort_at FROM sys_bpm_tasks t"+join+" WHERE "+where+
			" ORDER BY "+col+" DESC, t.id DESC LIMIT ?)")
		args = append(args, bargs...)
		args = append(args, limit)
	}
	candidates := strings.Join(parts, " UNION ALL ")
	stmt := workItemSelect + `
		JOIN (SELECT id, sort_at FROM (` + candidates + `) m ORDER BY sort_at DESC, id DESC LIMIT ?) c ON c.id = t.id
		WHERE t.tenant_id = ?`
	stmt += " ORDER BY c.sort_at DESC, t.id DESC"
	args = append(args, limit, tenantID)
	var rows []workItemRow
	if err := r.db.SelectContext(ctx, &rows, stmt, args...); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListWorkItems: %w", err)
	}
	out := make([]WorkItem, len(rows))
	for i := range rows {
		out[i] = rows[i].item()
	}
	return out, nil
}

// CountWorkItems returns the home-card numbers: open items in mine, open unclaimed items in the
// pool, and how many of mine are past their deadline now. now is injected for tests.
func (r *SQLRepository) CountWorkItems(ctx context.Context, tenantID string, q WorkItemQuery, now time.Time) (mine, pool, overdue int64, err error) {
	count := func(view WorkItemView, overdueOnly bool) (int64, error) {
		qq := q
		qq.View, qq.Entity = view, ""
		var total int64
		for _, b := range qq.branches(tenantID) {
			stmt := `SELECT COUNT(*) FROM sys_bpm_tasks t JOIN sys_approval_instances i ON i.tenant_id = t.tenant_id AND i.id = t.instance_id
				AND i.status = 'pending' WHERE ` + b.where
			args := append([]any{}, b.args...)
			if overdueOnly {
				// A paused clock is not running late — the list marks Overdue the same way (!Paused).
				stmt += " AND t.due_at IS NOT NULL AND t.due_at < ? AND i.sla_paused_at IS NULL"
				args = append(args, now.UTC())
			}
			var n int64
			if err := r.db.GetContext(ctx, &n, stmt, args...); err != nil {
				return 0, err
			}
			total += n
		}
		return total, nil
	}
	if mine, err = count(ViewMine, false); err != nil {
		return 0, 0, 0, fmt.Errorf("bpm.SQLRepository.CountWorkItems: mine: %w", err)
	}
	if pool, err = count(ViewPool, false); err != nil {
		return 0, 0, 0, fmt.Errorf("bpm.SQLRepository.CountWorkItems: pool: %w", err)
	}
	if overdue, err = count(ViewMine, true); err != nil {
		return 0, 0, 0, fmt.Errorf("bpm.SQLRepository.CountWorkItems: overdue: %w", err)
	}
	return mine, pool, overdue, nil
}

// PoolSeatEntities lists the record entities that open queue seats of the tenant are about — the
// short list the service runs entity ACL over before listing the pool.
func (r *SQLRepository) PoolSeatEntities(ctx context.Context, tenantID string) ([]string, error) {
	const q = `SELECT DISTINCT entity_name FROM sys_bpm_tasks
		WHERE tenant_id = ? AND status = 'open' AND assignee_type = 'queue' ORDER BY entity_name`
	var out []string
	if err := r.db.SelectContext(ctx, &out, q, tenantID); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.PoolSeatEntities: %w", err)
	}
	return out, nil
}

// CountPoolSeats counts the open, unclaimed seats of one work queue on one record entity — what a
// queue with the "claim" verb shows on its home card (specs/012).
func (r *SQLRepository) CountPoolSeats(ctx context.Context, tenantID, queueID, entity string) (int64, error) {
	const q = `SELECT COUNT(*) FROM sys_bpm_tasks t
		JOIN sys_approval_instances i ON i.tenant_id = t.tenant_id AND i.id = t.instance_id AND i.status = 'pending'
		WHERE t.tenant_id = ? AND t.status = 'open' AND t.assignee_type = 'queue' AND t.assignee_id = ?
		  AND t.entity_name = ? AND t.claimed_by IS NULL`
	var n int64
	if err := r.db.GetContext(ctx, &n, q, tenantID, queueID, entity); err != nil {
		return 0, fmt.Errorf("bpm.SQLRepository.CountPoolSeats: %w", err)
	}
	return n, nil
}

// GetWorkItem reads one seat (any status) of the tenant, or ErrSeatNotFound.
func (r *SQLRepository) GetWorkItem(ctx context.Context, tenantID, seatID string) (*WorkItem, error) {
	var row workItemRow
	if err := r.db.GetContext(ctx, &row, workItemSelect+` WHERE t.tenant_id = ? AND t.id = ?`, tenantID, seatID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSeatNotFound
		}
		return nil, fmt.Errorf("bpm.SQLRepository.GetWorkItem: %w", err)
	}
	it := row.item()
	return &it, nil
}

// ClaimSeat takes a pool seat out of the pool for userID. Under the instance row lock — the lock
// every decision takes, so a claim and a decision on the same instance are serialized — it
// re-reads the seat and refuses unless it is open, a pool seat the caller is a candidate of, and
// unclaimed; then sets claimed_by with a compare-and-set that excludes the state it writes
// (claimed_by IS NULL), so of two simultaneous claims exactly one affects a row. Claiming a seat
// the caller already holds is a no-op success.
func (r *SQLRepository) ClaimSeat(ctx context.Context, tenantID, seatID, userID string, roleIDs []string) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.ClaimSeat: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	it, err := r.lockSeat(ctx, tx, tenantID, seatID)
	if err != nil {
		return err
	}
	if !it.Seat.candidate(userID, roleIDs) {
		return ErrSeatNotFound
	}
	if !it.Seat.Claimable() {
		return ErrSeatNotClaimable
	}
	if it.ClaimedBy == userID {
		return nil
	}
	if it.ClaimedBy != "" {
		return ErrSeatClaimed
	}
	now := r.now().UTC()
	const q = `UPDATE sys_bpm_tasks SET claimed_by = ?, claimed_at = ?, updated_at = ?, version = version + 1
		WHERE tenant_id = ? AND id = ? AND status = 'open' AND claimed_by IS NULL`
	res, err := tx.ExecContext(ctx, q, userID, now, now, tenantID, seatID)
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.ClaimSeat: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("bpm.SQLRepository.ClaimSeat: rows affected: %w", err)
	} else if n != 1 {
		return ErrSeatClaimed // unreachable under the row lock; a lost CAS must never look like a win
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("bpm.SQLRepository.ClaimSeat: commit: %w", err)
	}
	return nil
}

// ReleaseSeat puts a claimed pool seat back in the pool. Only the claimer — or an admin — may
// (ErrNotClaimer for any other candidate; a non-candidate is ErrSeatNotFound). The CAS names the
// claimer it saw (claimed_by = ?), so a release racing another release or a re-claim affects a row
// only once.
func (r *SQLRepository) ReleaseSeat(ctx context.Context, tenantID, seatID, userID string, roleIDs []string, isAdmin bool) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.ReleaseSeat: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	it, err := r.lockSeat(ctx, tx, tenantID, seatID)
	if err != nil {
		return err
	}
	if !isAdmin && !it.Seat.candidate(userID, roleIDs) && it.ClaimedBy != userID {
		return ErrSeatNotFound
	}
	if !it.Seat.Claimable() {
		return ErrSeatNotClaimable
	}
	if it.ClaimedBy == "" {
		return ErrSeatNotClaimed
	}
	if !isAdmin && it.ClaimedBy != userID {
		return ErrNotClaimer
	}
	now := r.now().UTC()
	const q = `UPDATE sys_bpm_tasks SET claimed_by = NULL, claimed_at = NULL, updated_at = ?, version = version + 1
		WHERE tenant_id = ? AND id = ? AND status = 'open' AND claimed_by = ?`
	res, err := tx.ExecContext(ctx, q, now, tenantID, seatID, it.ClaimedBy)
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.ReleaseSeat: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("bpm.SQLRepository.ReleaseSeat: rows affected: %w", err)
	} else if n != 1 {
		return ErrSeatNotClaimed
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("bpm.SQLRepository.ReleaseSeat: commit: %w", err)
	}
	return nil
}

// lockSeat locks the seat's instance row (the lock every decision queues on), then reads the seat
// and requires it — and its instance — to still be open. ErrSeatNotFound when the seat is not in
// the tenant; ErrSeatNotOpen when it or its instance has ended.
func (r *SQLRepository) lockSeat(ctx context.Context, tx interface {
	GetContext(ctx context.Context, dest any, query string, args ...any) error
}, tenantID, seatID string) (*WorkItem, error) {
	var instanceID string
	if err := tx.GetContext(ctx, &instanceID, `SELECT instance_id FROM sys_bpm_tasks WHERE tenant_id = ? AND id = ?`, tenantID, seatID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSeatNotFound
		}
		return nil, fmt.Errorf("bpm.SQLRepository.lockSeat: %w", err)
	}
	var status string
	if err := tx.GetContext(ctx, &status, `SELECT status FROM sys_approval_instances WHERE tenant_id = ? AND id = ? FOR UPDATE`, tenantID, instanceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSeatNotFound
		}
		return nil, fmt.Errorf("bpm.SQLRepository.lockSeat: lock instance: %w", err)
	}
	var row workItemRow
	if err := tx.GetContext(ctx, &row, workItemSelect+` WHERE t.tenant_id = ? AND t.id = ?`, tenantID, seatID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSeatNotFound
		}
		return nil, fmt.Errorf("bpm.SQLRepository.lockSeat: read seat: %w", err)
	}
	it := row.item()
	if status != string(StatusPending) || it.Status != "open" {
		return nil, ErrSeatNotOpen
	}
	return &it, nil
}
