package dynrepo

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// A5 team_set resolution (spec ../specs/001-auth-security/spec.md §A5; coverage map §B4/§G3). When a record
// is created, its `team_set_id` is derived from the CREATOR's team memberships so that
// access_level=team reads (rowFilter) can match the row to a team the viewer belongs to. A
// "team set" is the deduplicated set of the creator's teams: team_sets.team_md5 is the MD5 of
// the sorted team-id list, UNIQUE(tenant_id, team_md5), so two creators on the identical set of
// teams share one team_sets row (resolve-or-create, never duplicated).
//
// Edge cases (coverage map §B2/§B4):
//   - creator on NO team  → no team_set is created; team_set_id stays NULL. The row is then
//     visible only to its owner (and to all/admin scopes), which is correct.
//   - creator on MANY teams → one team_set containing all of them; any viewer who shares ANY of
//     those teams sees the row (the team rowFilter matches a set containing any member team).
//
// tenant isolation: team_sets carries tenant_id and the lookup/insert are pinned to it, and the
// creator's teams are read tenant-scoped (teams.tenant_id = ?), so a team set never mixes teams
// across tenants. All values travel as `?` placeholders.

// teamMD5 computes the dedupe key for a set of team ids: the MD5 (hex) of the team ids sorted
// ascending and joined with newlines. Sorting makes the key order-independent so the same set
// always hashes identically. teamIDs MUST be non-empty (callers skip the empty case).
func teamMD5(teamIDs []string) string {
	sorted := make([]string, len(teamIDs))
	copy(sorted, teamIDs)
	sort.Strings(sorted)
	sum := md5.Sum([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}

// listUserTeamIDs returns the team ids the user belongs to, within the tenant. It joins
// team_memberships (user↔team) to teams (for the tenant pin) so a membership pointing at a
// team in another tenant is never counted. Returns an empty slice for a user on no team.
// q is the active querier (the *sqlx.DB or a *sqlx.Tx) so this works inside InsertRecord's tx.
func listUserTeamIDs(ctx context.Context, q sqlx.QueryerContext, tenantID, userID string) ([]string, error) {
	const stmt = "SELECT t.id FROM team_memberships tm " +
		"JOIN teams t ON t.id = tm.team_id " +
		"WHERE tm.user_id = ? AND t.tenant_id = ?"
	var ids []string
	if err := sqlx.SelectContext(ctx, q, &ids, stmt, userID, tenantID); err != nil {
		return nil, fmt.Errorf("dynrepo.listUserTeamIDs: %w", err)
	}
	return ids, nil
}

// resolveOrCreateTeamSet returns the id of the team_sets row for the given team ids within the
// tenant, creating it (plus its team_sets_teams composition rows) if no set with the same
// team_md5 exists yet. teamIDs MUST be non-empty. It runs inside the caller's transaction (tx)
// so the set + composition + the record insert that follows are atomic.
//
// Concurrency: two creators on the identical team set may race to insert the same
// (tenant_id, team_md5). The UNIQUE(tenant_id, team_md5) index makes the loser's INSERT fail
// with a duplicate-entry error. The loser must then re-read the winner's row, but a PLAIN
// SELECT here would read from the tx's REPEATABLE READ snapshot (pinned at the first consistent
// read earlier in InsertRecord) and could miss a winner that committed AFTER that snapshot,
// returning "" and surfacing as a spurious 500. So the post-duplicate re-read uses a LOCKING
// read (SELECT ... FOR SHARE) which always sees the latest committed row and blocks until the
// racing winner commits — closing the race without an explicit lock on the success path.
func resolveOrCreateTeamSet(ctx context.Context, tx *sqlx.Tx, tenantID string, teamIDs []string) (string, error) {
	md5key := teamMD5(teamIDs)

	// Fast path: the set already exists for this tenant. A plain (non-locking) SELECT is fine
	// here — the success path holds no extra lock.
	id, err := lookupTeamSetID(ctx, tx, tenantID, md5key)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}

	// Create it: team_sets row + one team_sets_teams row per team, all in the tx.
	setID := uuid.NewString()
	const insSet = "INSERT INTO `team_sets` (`id`, `tenant_id`, `team_md5`) VALUES (?, ?, ?)"
	if _, err := tx.ExecContext(ctx, insSet, setID, tenantID, md5key); err != nil {
		if _, ok := duplicateIndexName(err); ok {
			// Lost the race: another tx created the identical set. Re-read with a LOCKING read so
			// we escape our own REPEATABLE READ snapshot and see the committed winner row.
			existing, lookErr := lockingLookupTeamSetID(ctx, tx, tenantID, md5key)
			if lookErr != nil {
				return "", lookErr
			}
			if existing == "" {
				return "", fmt.Errorf("dynrepo.resolveOrCreateTeamSet: duplicate team_set but re-read empty")
			}
			return existing, nil
		}
		return "", fmt.Errorf("dynrepo.resolveOrCreateTeamSet: insert team_set: %w", err)
	}

	const insLink = "INSERT INTO `team_sets_teams` (`team_set_id`, `team_id`) VALUES (?, ?)"
	for _, tid := range teamIDs {
		if _, err := tx.ExecContext(ctx, insLink, setID, tid); err != nil {
			return "", fmt.Errorf("dynrepo.resolveOrCreateTeamSet: link team %q: %w", tid, err)
		}
	}
	return setID, nil
}

// lookupTeamSetID returns the team_sets id for (tenant_id, team_md5), or "" if none exists.
func lookupTeamSetID(ctx context.Context, q sqlx.QueryerContext, tenantID, md5key string) (string, error) {
	const stmt = "SELECT `id` FROM `team_sets` WHERE `tenant_id` = ? AND `team_md5` = ?"
	var id string
	if err := sqlx.GetContext(ctx, q, &id, stmt, tenantID, md5key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("dynrepo.lookupTeamSetID: %w", err)
	}
	return id, nil
}

// lockingLookupTeamSetID is the post-duplicate variant of lookupTeamSetID: it adds FOR SHARE so
// the read escapes the transaction's REPEATABLE READ snapshot and observes the latest COMMITTED
// row. It must run inside a transaction (tx) — a locking read outside a tx is meaningless. When
// the racing winner has not committed yet, FOR SHARE blocks until it does (or rolls back), so the
// loser always reads the winner's id rather than a stale-snapshot empty result. Returns "" only if
// no row exists at all (e.g. the winner rolled back), which the caller treats as an error.
func lockingLookupTeamSetID(ctx context.Context, tx *sqlx.Tx, tenantID, md5key string) (string, error) {
	const stmt = "SELECT `id` FROM `team_sets` WHERE `tenant_id` = ? AND `team_md5` = ? FOR SHARE"
	var id string
	if err := tx.GetContext(ctx, &id, stmt, tenantID, md5key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("dynrepo.lockingLookupTeamSetID: %w", err)
	}
	return id, nil
}
