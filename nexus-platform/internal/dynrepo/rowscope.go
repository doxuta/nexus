package dynrepo

import (
	"fmt"
	"strings"

	"nexus/platform/internal/ddl"
)

// A5 — Team-based row-level security (spec ../specs/001-auth-security/spec.md §A5; coverage map
// docs/_planner/A5-coverage-map.md §B2). This file holds the row-level access scope that
// every obj_* read (Get/List/Count) layers on top of the mandatory tenant_id filter, so a
// principal only ever sees rows their access_level permits. The four access levels come from
// acl_role_actions.access_level (A4, migration 000005) and are resolved per request by the
// ACL stage; the repository merely turns a resolved scope into a parameterized predicate.
//
// Visibility rules (spec §5 row-level, coverage map §B2):
//   - AccessAll   → no owner/team narrowing (tenant_id still applies). Admins use this too.
//   - AccessTeam  → owner = user OR the row's team_set_id resolves to a team the user belongs
//                   to OR any team below one (teams.parent_id descendants, closure computed in
//                   teamtree.go and carried as RowScope.TeamIDs; an unresolved scope falls back
//                   to direct membership only — never wider).
//   - AccessOwner → owner = user (only the principal's own rows), where owner is
//                   COALESCE(owner_id, created_by) — see rowFilter.
//   - "field:<name>" → owner = user OR the row's user-type field <name> points at the user
//                   (specs/071-rowscope-by-user-field). The ONE level that is per-entity by
//                   construction: <name> must be a user field of the entity the grant is for.
//   - Manual share (specs/051-record-manual-share) — on READS only, for Owner/Team, one more OR
//                   term: the rows shared with the user in sys_record_shares (same tenant, same
//                   entity). Never on a write, never under AccessNone. See RowScope.IncludeShares.
//   - AccessNone  → forbidden; no row is ever visible. NOTE: the ACL stage does NOT 403 an
//                   access_level=none grant — Authorize() returns allowed=true for any matching
//                   (entity, action) regardless of access_level (acl_service.go), so a none grant
//                   passes ACL and is stashed as access_level="none". The rowFilter "1=0" below is
//                   therefore the ONLY layer that enforces none, yielding zero rows (surfacing as
//                   ErrRecordNotFound/404). It also fails closed for unrecognized/empty levels.
//
// SECURITY: an UNRECOGNIZED / empty access level is treated as AccessNone (fail closed), so a
// mis-wired caller can never accidentally bypass the row filter. All values travel as `?`
// placeholders; no part of the scope is interpolated into SQL.

// AccessLevel is the row-level visibility scope for a (role, entity, action) grant.
type AccessLevel string

const (
	// AccessNone forbids every row (fail-closed default for unknown levels).
	AccessNone AccessLevel = "none"
	// AccessOwner limits visibility to rows the principal created.
	AccessOwner AccessLevel = "owner"
	// AccessTeam limits visibility to the principal's own rows plus rows whose team_set
	// includes a team the principal is a member of or any descendant of one (teamtree.go).
	AccessTeam AccessLevel = "team"
	// AccessAll allows every row in the tenant (no owner/team narrowing).
	AccessAll AccessLevel = "all"
)

// accessFieldPrefix starts the fifth access level: "field:<name>" = the principal sees the rows whose
// user-type field <name> points at them, plus the rows they own (DAILY-QUEUE #283, "bác sĩ chỉ thấy
// lịch hẹn của mình" without a receptionist re-assigning every record). The field name rides in the
// level string so the ~20 call sites that thread a resolved level through (acl_role_actions.access_level
// → ACL stage → RecordScope → RowScope) need no new parameter.
const accessFieldPrefix = "field:"

// FieldAccessLevel builds the level for "rows whose user field `field` is me". The name is validated
// where it is READ (UserField), not here, so a bad value can never produce a predicate.
func FieldAccessLevel(field string) AccessLevel { return AccessLevel(accessFieldPrefix + field) }

// UserField returns the user-field name of a "field:<name>" level. ok is false for every other level
// and for a name that is not a valid identifier (ddl.SanitizeIdentifier) — the name becomes a column
// identifier in SQL, so only a sanitized one may ever reach the predicate.
func (l AccessLevel) UserField() (name string, ok bool) {
	rest, found := strings.CutPrefix(string(l), accessFieldPrefix)
	if !found {
		return "", false
	}
	if _, err := ddl.SanitizeIdentifier(rest); err != nil {
		return "", false
	}
	return rest, true
}

// IsOwnerScoped reports whether the level narrows by owner (owner, team, field:<name>): the levels the
// manual-share clause can widen, so a write that misses a row may be hitting a share-only one.
func (l AccessLevel) IsOwnerScoped() bool {
	if _, ok := l.UserField(); ok {
		return true
	}
	return l == AccessOwner || l == AccessTeam
}

// normalizeAccessLevel maps a raw access_level string onto a known level, failing closed to
// AccessNone for anything unrecognized (empty, typo, future value). This is the single place
// the repo decides how to read the stored level, so the fail-closed policy is centralized.
func normalizeAccessLevel(raw string) AccessLevel {
	if _, ok := AccessLevel(raw).UserField(); ok {
		return AccessLevel(raw)
	}
	switch AccessLevel(raw) {
	case AccessAll:
		return AccessAll
	case AccessTeam:
		return AccessTeam
	case AccessOwner:
		return AccessOwner
	case AccessNone:
		return AccessNone
	default:
		return AccessNone
	}
}

// RowScope is the resolved row-level access context for a single read. UserID is the verified
// principal (auth.Subject.UserID, NEVER a client-supplied value); Level is the resolved
// access_level for the action. The repository turns this into a WHERE fragment via
// rowFilter; tenant scoping is applied separately and always.
//
// IsAdmin short-circuits to AccessAll (the ACL stage already bypasses for admins, but
// carrying the flag lets a repo caller pass an admin scope explicitly without re-deriving it).
type RowScope struct {
	UserID  string
	Level   AccessLevel
	IsAdmin bool
	// TeamIDs is the resolved set of teams an AccessTeam principal sees through: their direct
	// memberships PLUS every descendant team (teams.parent_id — queue #20, teamtree.go). It is
	// filled by Repository.resolveScope at each read/write entry point, never by callers. nil =
	// unresolved → rowFilterFor falls back to the direct-membership subquery (never wider); an
	// empty non-nil slice = resolved, member of no team → owner rows only.
	TeamIDs []string
	// IncludeShares opts this scope into the manual-share clause (specs/051-record-manual-share):
	// an owner/team principal also sees the rows shared with them in sys_record_shares. Callers set
	// it ONLY on a scope resolved for the VIEW action — shares are read-only (Q4 2026-09-05). It is
	// the first of two keys: the clause is emitted only after a dynrepo READ method has pinned it to
	// its own tenant + entity (forRead below); write methods never do, whatever this flag says.
	IncludeShares bool
	// shareTenantID / shareEntity pin the share clause to the tenant + entity of the query being
	// built. Unexported and set ONLY by forRead, so no caller outside dynrepo can aim a share at a
	// different tenant or table, and a scope that never went through a read method emits no clause.
	shareTenantID string
	shareEntity   string
	// shareTeamIDs is the viewer's team closure used ONLY for the team half of the share clause
	// (rows shared with a team, DAILY-QUEUE #224). Filled by resolveReadScope for any owner-scoped
	// level — a team share reaches an owner-level viewer who sits on that team too — and cleared by
	// the write-side resolver. nil/empty = no team term.
	shareTeamIDs []string
}

// forRead returns the scope pinned for a READ of entity in tenantID: when the caller opted in
// (IncludeShares) the manual-share clause is armed with exactly this query's tenant + sanitized
// entity name. A scope that did not opt in is returned unchanged.
func (s RowScope) forRead(tenantID, entity string) (RowScope, error) {
	if !s.IncludeShares {
		return s, nil
	}
	name, err := ddl.SanitizeIdentifier(entity) // the same name tableFor turns into obj_<name>
	if err != nil {
		return s, fmt.Errorf("dynrepo.RowScope.forRead: %w", err)
	}
	s.shareTenantID, s.shareEntity = tenantID, name
	return s, nil
}

// sharedOnlyExpr returns a SELECT expression (plus its placeholder args) that is 1 when a row the
// share-armed read returned is visible ONLY through the share clause, 0 when the caller's own
// owner/team predicate matches it too (DAILY-QUEUE #223 — the per-record read-only marker). It is the
// same predicate rowFilter emits, minus the share term, wrapped in IF(…, 0, 1): IF treats a NULL own
// predicate (team_set_id NULL, owner not the caller) as "not own", so a share-only row can never read
// as editable. Empty when the share clause is not armed (write paths, no opt-in) or the level does not
// narrow (AccessAll / admin): such a row is never share-only.
func (s RowScope) sharedOnlyExpr() (string, []any) {
	if s.shareEntity == "" {
		return "", nil
	}
	own := s
	own.shareTenantID, own.shareEntity, own.shareTeamIDs = "", "", nil
	frag, args := own.rowFilter()
	if frag == "" {
		return "", nil
	}
	return "IF(" + strings.TrimPrefix(frag, " AND ") + ", 0, 1)", args
}

// AdminScope returns a scope that sees every row in the tenant — used for admin principals
// and for internal/system reads (e.g. relation FK existence probes) that must not be narrowed
// by row-level security but still respect tenant_id.
func AdminScope() RowScope { return RowScope{Level: AccessAll, IsAdmin: true} }

// effectiveLevel resolves the level actually enforced: admins always see all; otherwise the
// normalized (fail-closed) stored level.
func (s RowScope) effectiveLevel() AccessLevel {
	if s.IsAdmin {
		return AccessAll
	}
	return normalizeAccessLevel(string(s.Level))
}

// rowFilter returns a SQL fragment (already prefixed with " AND ") plus its placeholder args
// that narrows an obj_* query to the rows this scope may see. The fragment is appended AFTER
// the mandatory `tenant_id = ?` / `deleted_at IS NULL` predicates, so tenant isolation is
// never weakened by row-level scoping. The `?team_sets_teams`/`?team_memberships` subquery is
// fully parameterized (only the user id is a value); table/column names are constants.
//
// Returns:
//   - AccessAll   → ("", nil)             no extra narrowing.
//   - AccessOwner → owner predicate       COALESCE(owner_id, created_by) = ?.
//   - AccessTeam  → owner OR team predicate.
//   - AccessNone  → ("AND 1=0", nil)      match no row (fail closed). The team predicate matches a
//     row whose team_set_id is any set containing a visible team. With a resolved
//     scope (TeamIDs != nil) the visible teams are the closure from teamtree.go
//     (memberships + descendants) emitted as an IN list; unresolved, the subquery
//     joins team_memberships (user↔team) to team_sets_teams (set↔team) — direct
//     membership only. A user on no team matches zero team rows → sees only their
//     owner rows; a user on many teams matches every set containing any of those
//     teams (multi-team union), exactly per coverage map §B2 edge cases.
//
// TRỤ 2 mục 2 "Giao việc & đổi chủ": the owner predicate reads `owner_id` (the CURRENT,
// reassignable owner PUT /:entity/:id/owner writes — services.RecordService.SetOwner) with a
// COALESCE fallback to `created_by` (the immutable creator). The fallback is defense-in-depth,
// not the common case: every INSERT path (dynrepo.InsertRecord/InsertRecordsAtomic/
// InsertRecords) writes owner_id = created_by at create time, and migration 000061 backfills
// every pre-existing row the same way — so owner_id is NULL only in a narrow transient window
// (a table the Doctor's generic ADD-COLUMN repair reached before the migration's own backfill,
// or a future insert path that forgets to set it). Because the fallback degrades to EXACTLY the
// pre-owner_id behavior (visible to the creator), this can never make a row LESS visible than
// before owner_id existed, nor MORE visible than the resolved owner_id/created_by allows — so
// this column's introduction cannot itself leak or hide data.
func (s RowScope) rowFilter() (string, []any) { return s.rowFilterFor("") }

// rowFilterFor is rowFilter with every OUTER column qualified by the given table alias, so the
// same A5 row-level predicate can gate a JOINed obj_* table (REPORT-JOIN). The team-subquery's
// own aliases (st, tm) are internal and stay unchanged. alias="" reproduces rowFilter exactly
// (unqualified columns) for the single-table path. The alias is a SERVER-generated identifier
// (e.g. "base", "j0"), never client input, but it is still emitted through the backtick-quoted
// qualifier below — callers must not pass raw user text.
func (s RowScope) rowFilterFor(alias string) (string, []any) {
	// col qualifies a bare column with the alias when one is given: `alias`.`col`.
	col := func(name string) string {
		if alias == "" {
			return "`" + name + "`"
		}
		return "`" + alias + "`.`" + name + "`"
	}
	// ownerExpr is the "who owns this row" expression the Owner/Team levels narrow by: the
	// current owner, falling back to the creator when owner_id is not yet populated (see the
	// rowFilter doc comment above for why the fallback is safe).
	ownerExpr := "COALESCE(" + col("owner_id") + ", " + col("created_by") + ")"
	// terms are OR-ed: the owner predicate, the team predicate (team level), the share predicate.
	terms, args := []string{ownerExpr + " = ?"}, []any{s.UserID}
	level := s.effectiveLevel()
	if field, ok := level.UserField(); ok {
		// #283: the owner term plus "the user field points at me". A user field is materialised as
		// the STORED generated column `<field>_id` (ddl.BuildCreateTable) indexed (tenant_id,
		// <field>_id), so this is an indexed equality, never a JSON scan. `field` passed
		// ddl.SanitizeIdentifier in UserField, so it is safe as a quoted identifier.
		terms = append(terms, col(field+"_id")+" = ?")
		args = append(args, s.UserID)
		level = AccessOwner // the owner branch below adds nothing more; the extra term is already in
	}
	switch level {
	case AccessAll:
		return "", nil
	case AccessOwner:
	case AccessTeam:
		switch {
		case s.TeamIDs == nil:
			// Unresolved: team_set_id IN (sets that contain a team the user is a DIRECT member
			// of) — the pre-hierarchy predicate, kept as the never-wider fallback.
			terms = append(terms, col("team_set_id")+" IN ("+
				"SELECT st.team_set_id FROM team_sets_teams st "+
				"JOIN team_memberships tm ON tm.team_id = st.team_id "+
				"WHERE tm.user_id = ?)")
			args = append(args, s.UserID)
		case len(s.TeamIDs) > 0:
			// Resolved (teamtree.go): the caller's teams + every descendant, already read from
			// team_memberships/teams in Go — so the predicate is a plain IN list over the set
			// composition. Membership of NO team (empty non-nil slice) adds no team term and
			// collapses to the owner predicate.
			terms = append(terms, col("team_set_id")+" IN ("+
				"SELECT st.team_set_id FROM team_sets_teams st "+
				"WHERE st.team_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(s.TeamIDs)), ",")+"))")
			for _, id := range s.TeamIDs {
				args = append(args, id)
			}
		}
	default: // AccessNone (and any unrecognized level) — see nothing, shared rows included.
		return " AND 1=0", nil
	}
	// specs/051-record-manual-share: the rows shared with this user, as one more OR term. Armed only
	// when a READ method pinned the scope to its own tenant + entity (forRead), so a write path never
	// emits it. entity_name stays in the predicate on purpose: a mis-attributed share fails closed
	// instead of relying on UUID uniqueness across tables.
	if s.shareEntity != "" {
		terms = append(terms, col("id")+" IN (SELECT rs.record_id FROM sys_record_shares rs "+
			"WHERE rs.tenant_id = ? AND rs.entity_name = ? AND rs.user_id = ? AND rs.deleted_at IS NULL)")
		args = append(args, s.shareTenantID, s.shareEntity, s.UserID)
		// DAILY-QUEUE #224: rows shared with a TEAM the viewer sits on (or above, via the closure).
		if len(s.shareTeamIDs) > 0 {
			terms = append(terms, col("id")+" IN (SELECT rs.record_id FROM sys_record_shares rs "+
				"WHERE rs.tenant_id = ? AND rs.entity_name = ? AND rs.team_id IN ("+
				strings.TrimSuffix(strings.Repeat("?,", len(s.shareTeamIDs)), ",")+") AND rs.deleted_at IS NULL)")
			args = append(args, s.shareTenantID, s.shareEntity)
			for _, id := range s.shareTeamIDs {
				args = append(args, id)
			}
		}
	}
	if len(terms) == 1 {
		return " AND " + terms[0], args
	}
	return " AND (" + strings.Join(terms, " OR ") + ")", args
}
