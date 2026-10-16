package services

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

// AssignmentService implements the ASSIGN-1 round-robin resolver (workflow.AssignmentResolver) for
// the `assign` workflow action (engine-depth primitive #4, spec ../specs/033-assignment-rule/spec.md). Each
// fire hands the record to the NEXT active member of the action's team, remembering the last pick
// in a per-rule cursor (sys_assignment_cursors, migration 000049). It is tenant-scoped and uses
// only parameterized queries.
//
// The advance is the risk center (spec §4): NextAssignee runs the whole read-compute-write inside
// ONE transaction with a `SELECT last_user_id ... FOR UPDATE` row lock, so two records firing the
// SAME rule concurrently serialize and receive DIFFERENT members. The FOR UPDATE is load-bearing —
// under InnoDB REPEATABLE READ a plain SELECT reads the transaction snapshot and would miss a
// concurrent fire's just-committed advance (both would pick the same member). A fake in-memory
// resolver cannot catch that; only a real-MySQL 2-goroutine test does (the lead owns that test).
type AssignmentService struct {
	db     *sqlx.DB
	logger *zap.Logger
	newID  func() string // injectable id generator; defaults to uuid
}

// NewAssignmentService wires the resolver over an existing sqlx connection.
func NewAssignmentService(db *sqlx.DB, logger *zap.Logger) *AssignmentService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AssignmentService{db: db, logger: logger, newID: uuid.NewString}
}

// assignMemberQuery lists a team's ACTIVE members in a stable order (by user_id → deterministic
// ring). users has no disabled_at column (verified against migration 000003) so eligibility is
// status='active' + deleted_at IS NULL. The tenant guard rides on the users row (u.tenant_id) —
// team_memberships has no tenant_id column but a team is tenant-owned, so a cross-tenant user can
// never appear here.
const assignMemberQuery = `SELECT tm.user_id
	FROM team_memberships tm
	JOIN users u ON u.id = tm.user_id
	JOIN teams t ON t.id = tm.team_id AND t.tenant_id = u.tenant_id
	WHERE tm.team_id = ? AND u.tenant_id = ? AND u.status = 'active' AND u.deleted_at IS NULL
	ORDER BY tm.user_id`

// assignCursorEnsure inserts the cursor row if absent (ON DUPLICATE KEY UPDATE id = id is a no-op
// touch) so the FOR UPDATE below always has a row to lock — even on the very first fire, where two
// concurrent first-fires must not both see "no row" and race.
const assignCursorEnsure = `INSERT INTO sys_assignment_cursors
		(id, tenant_id, workflow_id, action_key, team_id, last_user_id)
	VALUES (?, ?, ?, ?, ?, NULL)
	ON DUPLICATE KEY UPDATE id = id`

// assignCursorLock reads the last-picked member UNDER a row lock (FOR UPDATE) — the concurrency
// hinge (see the type doc). Never downgrade this to a plain SELECT.
const assignCursorLock = `SELECT last_user_id FROM sys_assignment_cursors
	WHERE tenant_id = ? AND workflow_id = ? AND action_key = ? FOR UPDATE`

// assignCursorAdvance writes the newly-picked member back as last_user_id (team_id is refreshed
// too — cosmetic, keeps the stored team current after a rule's team change).
const assignCursorAdvance = `UPDATE sys_assignment_cursors
	SET last_user_id = ?, team_id = ?
	WHERE tenant_id = ? AND workflow_id = ? AND action_key = ?`

// NextAssignee atomically advances the per-rule cursor and returns the next active member of the
// team. ok=false means no eligible member (empty team / all disabled) — the assign action then
// no-ops (fail-open). Everything happens in ONE transaction under a FOR UPDATE row lock so
// concurrent fires of the same rule pick different members (spec §4).
func (s *AssignmentService) NextAssignee(ctx context.Context, tenantID, workflowID, actionKey, teamID string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("services.AssignmentService.NextAssignee: no db wired")
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.NextAssignee: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// 1. Ensure the cursor row exists (so FOR UPDATE has a row to lock, first fire included).
	if _, err := tx.ExecContext(ctx, assignCursorEnsure, s.newID(), tenantID, workflowID, actionKey, teamID); err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.NextAssignee: ensure cursor: %w", err)
	}
	// 2. Lock the cursor row + read the last pick (FOR UPDATE — the serialization point).
	var lastUserID sql.NullString
	if err := tx.GetContext(ctx, &lastUserID, assignCursorLock, tenantID, workflowID, actionKey); err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.NextAssignee: lock cursor: %w", err)
	}
	// 3. Resolve the team's active members INSIDE the tx (ordered ring).
	var members []string
	if err := tx.SelectContext(ctx, &members, assignMemberQuery, teamID, tenantID); err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.NextAssignee: list members: %w", err)
	}
	if len(members) == 0 {
		// No one to assign — commit the harmless ensure and report ok=false (fail-open no-op).
		if err := tx.Commit(); err != nil {
			return "", false, fmt.Errorf("services.AssignmentService.NextAssignee: commit: %w", err)
		}
		s.logger.Info("assignment: no eligible member for team; assign no-op",
			zap.String("tenant_id", tenantID),
			zap.String("workflow_id", workflowID),
			zap.String("action_key", actionKey),
			zap.String("team_id", teamID))
		return "", false, nil
	}
	// 4. Compute the next member (after last_user_id; wrap; restart if last left the team).
	picked := nextInRing(members, lastUserID.String)
	// 5. Persist the advance + commit (releases the lock so the next fire proceeds).
	if _, err := tx.ExecContext(ctx, assignCursorAdvance, picked, teamID, tenantID, workflowID, actionKey); err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.NextAssignee: advance cursor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.NextAssignee: commit: %w", err)
	}
	return picked, true, nil
}

// LeastLoadedAssignee resolves the load_based assign strategy (ASSIGN-2): pick the team's active
// member with the FEWEST active assigned records in `entity` at `field`, tie-broken by the SAME
// per-rule round-robin cursor (nextInRing among the tied members) so repeated equal-load fires still
// rotate. It reuses ASSIGN-1's whole transaction shape: ensure + FOR UPDATE lock + advance the
// cursor in ONE tx, so concurrent fires of the same rule serialize on the cursor and the tie-break
// is deterministic under load. ok=false (empty team / every member disabled) is the SAME fail-open
// no-op as NextAssignee — runAssign then writes nothing.
//
// Load-snapshot semantics: the count is read INSIDE the cursor tx but the assignment write lands
// AFTER this returns (runAssign → field_update), exactly like round-robin's model. So the FOR UPDATE
// lock serializes the CURSOR (deterministic tie rotation), not the load count — two truly-concurrent
// fires may read the same load snapshot and both pick the current minimum. That is a benign
// approximation of "least loaded" (both land on a genuinely-least-loaded member), not a correctness
// bug; strict per-fire load accounting is a later slice.
func (s *AssignmentService) LeastLoadedAssignee(ctx context.Context, tenantID, workflowID, actionKey, entity, field, teamID string) (string, bool, error) {
	return s.leastLoadedPick(ctx, "LeastLoadedAssignee", tenantID, workflowID, actionKey, teamID,
		func(tx *sqlx.Tx, members []string) (map[string]int, error) {
			return s.countActiveLoads(ctx, tx, tenantID, entity, field, members)
		})
}

// LeastLoadedSeatAssignee is load_based for a BPM step (specs/043 S6b): the team's active member
// with the FEWEST open work seats, ties broken by the same per-node round-robin cursor
// (workflowID = the process, actionKey = "bpm:"+nodeId). A member's load is the seats they hold
// (open user seats) plus the pool seats they have claimed — what is on their desk right now.
func (s *AssignmentService) LeastLoadedSeatAssignee(ctx context.Context, tenantID, workflowID, actionKey, teamID string) (string, bool, error) {
	return s.leastLoadedPick(ctx, "LeastLoadedSeatAssignee", tenantID, workflowID, actionKey, teamID,
		func(tx *sqlx.Tx, members []string) (map[string]int, error) {
			return s.countOpenSeats(ctx, tx, tenantID, members)
		})
}

// ActiveMembers lists the active members of a team in the stable ring order, at most limit (the
// each_member snapshot of a BPM step). Tenant-pinned through the users row, like the ring.
func (s *AssignmentService) ActiveMembers(ctx context.Context, tenantID, teamID string, limit int) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("services.AssignmentService.ActiveMembers: no db wired")
	}
	var members []string
	if err := s.db.SelectContext(ctx, &members, assignMemberQuery+" LIMIT ?", teamID, tenantID, limit); err != nil {
		return nil, fmt.Errorf("services.AssignmentService.ActiveMembers: %w", err)
	}
	return members, nil
}

// leastLoadedPick is the shared transaction of the load_based strategies: ensure + lock the cursor
// row, list the team's members, let count say each member's load, pick the least loaded (cursor
// tie-break), advance the cursor, commit. op names the public caller in errors.
func (s *AssignmentService) leastLoadedPick(ctx context.Context, op, tenantID, workflowID, actionKey, teamID string, count func(tx *sqlx.Tx, members []string) (map[string]int, error)) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("services.AssignmentService.%s: no db wired", op)
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// 1-3. Same as NextAssignee: ensure the cursor row, lock it FOR UPDATE (serialization point +
	// tie-break state), resolve the team's active members inside the tx (ordered ring by user_id).
	if _, err := tx.ExecContext(ctx, assignCursorEnsure, s.newID(), tenantID, workflowID, actionKey, teamID); err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.%s: ensure cursor: %w", op, err)
	}
	var lastUserID sql.NullString
	if err := tx.GetContext(ctx, &lastUserID, assignCursorLock, tenantID, workflowID, actionKey); err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.%s: lock cursor: %w", op, err)
	}
	var members []string
	if err := tx.SelectContext(ctx, &members, assignMemberQuery, teamID, tenantID); err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.%s: list members: %w", op, err)
	}
	if len(members) == 0 {
		if err := tx.Commit(); err != nil {
			return "", false, fmt.Errorf("services.AssignmentService.%s: commit: %w", op, err)
		}
		s.logger.Info("assignment: no eligible member for team; load_based assign no-op",
			zap.String("tenant_id", tenantID),
			zap.String("workflow_id", workflowID),
			zap.String("action_key", actionKey),
			zap.String("team_id", teamID))
		return "", false, nil
	}
	// 4. Count each member's load (tenant-scoped).
	counts, err := count(tx, members)
	if err != nil {
		return "", false, err
	}
	// 5. Pick the least-loaded member (cursor tie-break among equals), advance the cursor, commit.
	picked := leastLoaded(members, counts, lastUserID.String)
	if _, err := tx.ExecContext(ctx, assignCursorAdvance, picked, teamID, tenantID, workflowID, actionKey); err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.%s: advance cursor: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("services.AssignmentService.%s: commit: %w", op, err)
	}
	return picked, true, nil
}

// countOpenSeats returns, per member id, how many OPEN work seats (sys_bpm_tasks) they hold: seats
// naming them as the user (idx_bt_inbox) plus pool seats they claimed (idx_bt_claimed). Tenant-
// pinned, values bound through sqlx.In; a member with none is absent (= load 0).
func (s *AssignmentService) countOpenSeats(ctx context.Context, tx *sqlx.Tx, tenantID string, members []string) (map[string]int, error) {
	counts := make(map[string]int, len(members))
	for _, q := range []string{
		"SELECT assignee_id AS member, COUNT(*) AS cnt FROM sys_bpm_tasks " +
			"WHERE tenant_id = ? AND status = 'open' AND deleted_at IS NULL AND assignee_type = 'user' AND assignee_id IN (?) GROUP BY assignee_id",
		"SELECT claimed_by AS member, COUNT(*) AS cnt FROM sys_bpm_tasks " +
			"WHERE tenant_id = ? AND status = 'open' AND deleted_at IS NULL AND claimed_by IN (?) GROUP BY claimed_by",
	} {
		query, args, err := sqlx.In(q, tenantID, members)
		if err != nil {
			return nil, fmt.Errorf("services.AssignmentService.countOpenSeats: %w", err)
		}
		var rows []struct {
			Member string `db:"member"`
			Count  int    `db:"cnt"`
		}
		if err := tx.SelectContext(ctx, &rows, query, args...); err != nil {
			return nil, fmt.Errorf("services.AssignmentService.countOpenSeats: %w", err)
		}
		for _, r := range rows {
			counts[r.Member] += r.Count
		}
	}
	return counts, nil
}

// countActiveLoads returns, per member id, the count of ACTIVE records of `entity` whose target
// `field` holds that member's id — the member's "load". One GROUP BY query, tenant-scoped, only `?`
// placeholders for values. Members absent from the result have zero load (the caller treats a
// missing key as 0). The table name comes from dynrepo.TableFor (the ONE obj_* name rule) and the
// field goes through ddl.SanitizeIdentifier (the only client-derived identifier in the SQL — every
// value binds through `?`), so the statement is injection-safe.
//
// The `deleted_at IS NULL` filter is load-bearing: the indexed TEXT field's VIRTUAL generated column
// (ddl.indexedColExpr) carries NO deleted_at CASE guard, so a soft-deleted row's projected value
// still equals a member id — this explicit filter is what excludes it from the load.
//
// The assignee expression JSON_UNQUOTE(JSON_EXTRACT(`data`,'$.<field>')) matches that generated
// column's expression EXACTLY, so MySQL 8 uses the generated-column index when the assignee field is
// indexed.
//
// ponytail: full-table JSON scan when the assignee field is NOT indexed. Fine for typical tenants;
// if a tenant grows past ~10k active records set the assignee field's options.indexed=true and MySQL
// picks up the generated-column index on this same expression — no query change needed.
func (s *AssignmentService) countActiveLoads(ctx context.Context, tx *sqlx.Tx, tenantID, entity, field string, members []string) (map[string]int, error) {
	table, err := dynrepo.TableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("services.AssignmentService.countActiveLoads: table: %w", err)
	}
	col, err := ddl.SanitizeIdentifier(field)
	if err != nil {
		return nil, fmt.Errorf("services.AssignmentService.countActiveLoads: field %q: %w", field, err)
	}
	assigneeExpr := fmt.Sprintf("JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s'))", col)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(members)), ",")
	query := fmt.Sprintf(
		"SELECT %s AS `assignee`, COUNT(*) AS `cnt` FROM `%s` "+
			"WHERE `tenant_id` = ? AND `deleted_at` IS NULL AND %s IN (%s) "+
			"GROUP BY `assignee`",
		assigneeExpr, table, assigneeExpr, placeholders)
	args := make([]any, 0, len(members)+1)
	args = append(args, tenantID)
	for _, m := range members {
		args = append(args, m)
	}
	var rows []struct {
		Assignee string `db:"assignee"`
		Count    int    `db:"cnt"`
	}
	if err := tx.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("services.AssignmentService.countActiveLoads: %w", err)
	}
	counts := make(map[string]int, len(rows))
	for _, r := range rows {
		counts[r.Assignee] = r.Count
	}
	return counts, nil
}

// leastLoaded returns the member with the FEWEST active assigned records. counts maps member id →
// load; a member ABSENT from the map has load 0 (so an unassigned member correctly wins). Ties (>1
// member sharing the minimum load) are broken by the round-robin cursor: among the tied members —
// kept in the ring order of `members` (ordered by user_id) — the one immediately AFTER `last`
// (nextInRing) wins, so two equal-load fires of the same rule rotate through the tied set instead of
// always picking the same member. Deterministic: with a fixed load snapshot the result depends only
// on (counts, last, ring order). members MUST be non-empty (the caller guarantees it).
func leastLoaded(members []string, counts map[string]int, last string) string {
	minLoad := counts[members[0]]
	for _, m := range members[1:] {
		if c := counts[m]; c < minLoad {
			minLoad = c
		}
	}
	tied := make([]string, 0, len(members))
	for _, m := range members {
		if counts[m] == minLoad {
			tied = append(tied, m)
		}
	}
	return nextInRing(tied, last)
}

// nextInRing returns the member immediately AFTER `last` in the ordered ring, wrapping to the first
// at the end. An empty `last` (cursor never rotated) or a `last` no longer present (member left the
// team) both restart from the first member. members MUST be non-empty (the caller guarantees it).
func nextInRing(members []string, last string) string {
	if last == "" {
		return members[0]
	}
	for i, m := range members {
		if m == last {
			return members[(i+1)%len(members)]
		}
	}
	return members[0] // last no longer in the team → restart from first
}

// Ensure the service satisfies the engine's resolver interface at compile time.
var _ workflow.AssignmentResolver = (*AssignmentService)(nil)
