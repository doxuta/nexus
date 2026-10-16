package dynrepo

import (
	"context"
	"fmt"
	"sort"

	"github.com/jmoiron/sqlx"
)

// Team-hierarchy row scope (queue #20 team-hierarchy-rowscope-a; spec
// ../specs/001-auth-security/spec.md §A5). teams.parent_id (migration 000006) declares a tree —
// "Vùng Bắc > Chi nhánh Hà Nội > PGD Cầu Giấy" — and an access_level=team principal must see the
// rows of every team BELOW the teams they are a member of, not only their direct memberships.
//
// The closure is expanded in Go, not in SQL: ONE tenant-pinned query returns every team of the
// tenant with its parent and whether the caller is a direct member; a breadth-first walk with a
// visited set collects the member teams plus all their descendants. A visited set — rather than a
// recursive CTE with depth accounting — is what makes a cycle already present in the data
// (parent_id has no hard FK, so out-of-order seeds and pre-guard loops exist) terminate instead of
// hanging. The result is emitted by rowFilterFor as a plain `st.team_id IN (?,…)`.
//
// ponytail: no cache, by decision. Membership MUST stay live (removing a user from a team revokes
// visibility immediately today, and that must not regress), so a cached tree would still need a
// live membership read per request — one round-trip either way. Teams number in the tens per
// tenant; the query is index-backed (idx_teams_tenant, uq_tm_user_team). Bulk edit (mass-op)
// calls UpdateRecord per row and so resolves per row; import is INSERT-only and never resolves a
// scope. Ceiling: if a tenant grows past
// ~2k teams, cache the (id, parent_id) map per tenant in cache.TTLCache and invalidate it from
// TeamService parent/delete writes — membership still read live.

// teamTreeRow is one team of the tenant as seen from the caller: its parent link and whether the
// caller is a direct member.
type teamTreeRow struct {
	ID       string  `db:"id"`
	ParentID *string `db:"parent_id"`
	Member   bool    `db:"member"`
}

// descendantClosure returns, sorted for deterministic SQL, every team the caller is a member of
// plus every team reachable downward from those. Never nil: an empty (non-nil) slice means
// "resolved, no team" — distinct from nil, which rowFilterFor treats as unresolved.
func descendantClosure(rows []teamTreeRow) []string {
	children := make(map[string][]string, len(rows))
	var queue []string
	for _, r := range rows {
		if r.ParentID != nil && *r.ParentID != "" {
			children[*r.ParentID] = append(children[*r.ParentID], r.ID)
		}
		if r.Member {
			queue = append(queue, r.ID)
		}
	}
	seen := make(map[string]bool, len(rows))
	out := make([]string, 0, len(queue))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		queue = append(queue, children[id]...)
	}
	sort.Strings(out)
	return out
}

// visibleTeamIDs runs the one tenant-pinned tree+membership query for the caller and returns the
// closure. team_memberships has no tenant_id column; the tenant pin lives on teams (the JOIN
// target), so a membership pointing at another tenant's team can never count.
func visibleTeamIDs(ctx context.Context, q sqlx.QueryerContext, tenantID, userID string) ([]string, error) {
	const stmt = "SELECT t.id, t.parent_id, (tm.user_id IS NOT NULL) AS member " +
		"FROM teams t LEFT JOIN team_memberships tm ON tm.team_id = t.id AND tm.user_id = ? " +
		"WHERE t.tenant_id = ?"
	var rows []teamTreeRow
	if err := sqlx.SelectContext(ctx, q, &rows, stmt, userID, tenantID); err != nil {
		return nil, fmt.Errorf("dynrepo.visibleTeamIDs: %w", err)
	}
	return descendantClosure(rows), nil
}

// resolveScope fills RowScope.TeamIDs for an access_level=team scope (a no-op for every other
// level and for an already-resolved scope), so the row filter can emit the hierarchy-aware
// predicate. Every obj_* read/write entry point calls this ONCE before building SQL; a scope that
// somehow skips it falls back to direct-membership visibility (rowFilterFor's nil branch) — never
// wider.
//
// It is also the WRITE-side resolver: it disarms the manual-share clause (specs/051), so an
// Update/SoftDelete/Restore/SetOwner can never be widened by a share, whatever the caller passed.
// Read methods use resolveReadScope instead.
func (r *Repository) resolveScope(ctx context.Context, tenantID string, s RowScope) (RowScope, error) {
	s.shareTenantID, s.shareEntity, s.shareTeamIDs = "", "", nil
	if s.effectiveLevel() != AccessTeam || s.TeamIDs != nil {
		return s, nil
	}
	ids, err := visibleTeamIDs(ctx, r.db, tenantID, s.UserID)
	if err != nil {
		return s, fmt.Errorf("dynrepo.resolveScope: %w", err)
	}
	s.TeamIDs = ids
	return s, nil
}

// resolveReadScope is resolveScope for a READ of entity (GetRecord, ListRecords, ListRecordsCursor,
// the aggregate base table, ListLinked's target): it additionally arms the manual-share clause for a
// scope that opted in (RowScope.IncludeShares), pinned to THIS query's tenant + entity. The read-only
// guarantee of shares rests on this being the only place the clause is armed.
func (r *Repository) resolveReadScope(ctx context.Context, tenantID, entity string, s RowScope) (RowScope, error) {
	s, err := r.resolveScope(ctx, tenantID, s)
	if err != nil {
		return s, err
	}
	if s, err = s.forRead(tenantID, entity); err != nil || s.shareEntity == "" || !s.effectiveLevel().IsOwnerScoped() {
		return s, err
	}
	// Rows shared with a team (DAILY-QUEUE #224) reach the viewer through their team closure whatever
	// their own level is, so an owner-level viewer needs it resolved too. A team-level scope already
	// carries it. ponytail: one extra tenant-pinned query per share-armed read for owner/field levels;
	// cache with the closure (see the teamtree note above) if it ever shows in a profile.
	if s.TeamIDs != nil {
		s.shareTeamIDs = s.TeamIDs
		return s, nil
	}
	if s.shareTeamIDs, err = visibleTeamIDs(ctx, r.db, tenantID, s.UserID); err != nil {
		return s, fmt.Errorf("dynrepo.resolveReadScope: %w", err)
	}
	return s, nil
}

// aggregateFromWhereScoped is aggregateFromWhere with the base scope AND every JOIN hop's scope
// resolved first (REPORT-JOIN hops carry their own row-level scope for the related entity, so a
// regional manager's report over a related entity sees the same descendant rows a list would).
// p.Joins is copied before mutation so the caller's slice is never touched. The base table (entity)
// is a read and so honors manual shares; a JOIN hop does too when its JoinSpec names its Entity (the name
// the share clause is pinned to — DAILY-QUEUE #225), and does not otherwise.
func (r *Repository) aggregateFromWhereScoped(ctx context.Context, entity, table, tenantID string, scope RowScope, p AggregateParams) (fromJoin, where string, args []any, err error) {
	if scope, err = r.resolveReadScope(ctx, tenantID, entity, scope); err != nil {
		return "", "", nil, err
	}
	if len(p.Joins) > 0 {
		joins := make([]JoinSpec, len(p.Joins))
		copy(joins, p.Joins)
		for i := range joins {
			if joins[i].NoRowScope {
				continue
			}
			// A hop that names its entity is a READ of that table and honours manual shares (#225); one
			// that does not (or opted out) is resolved as before — shares disarmed.
			if joins[i].Entity != "" {
				joins[i].Scope, err = r.resolveReadScope(ctx, tenantID, joins[i].Entity, joins[i].Scope)
			} else {
				joins[i].Scope, err = r.resolveScope(ctx, tenantID, joins[i].Scope)
			}
			if err != nil {
				return "", "", nil, err
			}
		}
		p.Joins = joins
	}
	return aggregateFromWhere(table, tenantID, scope, p)
}
