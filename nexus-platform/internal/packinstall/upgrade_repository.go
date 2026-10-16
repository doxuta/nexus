package packinstall

// Run lock + history of pack upgrades (sys_pack_upgrades, migration 000094; specs/069 plan.md §8).
// One row per run. running_lock = 1 while running (NULL when finished) under UNIQUE
// (tenant_id, pack_slug, running_lock): any number of finished rows, at most one running row —
// the database, not the process, decides who holds the lock, so it works across server instances.
// The lease is the row's updated_at (bumped by every AppendResult): a run silent for more than
// leaseMinutes is dead, never blocks anyone and is shown as "interrupted".

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"nexus/platform/internal/services/vertical_pack"
)

const leaseMinutes = 5

// ClaimRun retires a dead run of the same (tenant, slug), then inserts run as the lock holder.
// A live holder yields *vertical_pack.ErrRunInProgress.
func (r *Repository) ClaimRun(ctx context.Context, run vertical_pack.UpgradeRun) error {
	decisions, err := json.Marshal(nonNilDecisions(run.Decisions))
	if err != nil {
		return fmt.Errorf("packinstall.ClaimRun: marshal decisions: %w", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := r.db.WithContext(ctx).Exec(
			"UPDATE sys_pack_upgrades SET status = 'interrupted', running_lock = NULL, finished_at = UTC_TIMESTAMP(3) "+
				"WHERE tenant_id = ? AND pack_slug = ? AND running_lock = 1 AND updated_at < NOW(3) - INTERVAL ? MINUTE",
			run.TenantID, run.Slug, leaseMinutes,
		).Error; err != nil {
			return fmt.Errorf("packinstall.ClaimRun: retire dead run: %w", err)
		}
		err = r.db.WithContext(ctx).Exec(
			"INSERT INTO sys_pack_upgrades (id, tenant_id, pack_slug, from_versions, to_version, source, mode, plan_hash, "+
				"status, running_lock, decisions, created_by, started_at) VALUES (?, ?, ?, '', ?, ?, ?, ?, 'running', 1, ?, ?, UTC_TIMESTAMP(3))",
			run.ID, run.TenantID, run.Slug, run.ToVersion, run.Source, run.Mode, run.PlanHash, decisions, run.CreatedBy,
		).Error
		if err == nil {
			return nil
		}
		var me *gomysql.MySQLError
		if !errors.As(err, &me) || me.Number != 1062 {
			return fmt.Errorf("packinstall.ClaimRun: insert: %w", err)
		}
		busy, qerr := r.RunningRun(ctx, run.TenantID, run.Slug)
		if qerr != nil {
			return qerr
		}
		if busy != nil {
			return busy
		}
		// The holder finished between our insert and our read: try once more.
	}
	return fmt.Errorf("packinstall.ClaimRun: lock contended")
}

// AbortRun hands the lock back for a run rejected before any write: row hidden from history.
func (r *Repository) AbortRun(ctx context.Context, tenantID, id string) error {
	if err := r.db.WithContext(ctx).Exec(
		"UPDATE sys_pack_upgrades SET status = 'rejected', running_lock = NULL, finished_at = UTC_TIMESTAMP(3), deleted_at = NOW() "+
			"WHERE id = ? AND tenant_id = ? AND running_lock = 1",
		id, tenantID,
	).Error; err != nil {
		return fmt.Errorf("packinstall.AbortRun: %w", err)
	}
	return nil
}

// BeginRun records the effective decisions and the baseline versions once the plan is accepted.
func (r *Repository) BeginRun(ctx context.Context, tenantID, id, fromVersions string, decisions []vertical_pack.UpgradeDecision) error {
	b, err := json.Marshal(nonNilDecisions(decisions))
	if err != nil {
		return fmt.Errorf("packinstall.BeginRun: marshal decisions: %w", err)
	}
	if err := r.db.WithContext(ctx).Exec(
		"UPDATE sys_pack_upgrades SET from_versions = ?, decisions = ? WHERE id = ? AND tenant_id = ? AND running_lock = 1",
		fromVersions, b, id, tenantID,
	).Error; err != nil {
		return fmt.Errorf("packinstall.BeginRun: %w", err)
	}
	return nil
}

// AppendResult adds one item outcome; the write bumps updated_at, which is the run's heartbeat.
func (r *Repository) AppendResult(ctx context.Context, tenantID, id string, res vertical_pack.UpgradeResult) error {
	b, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("packinstall.AppendResult: marshal: %w", err)
	}
	if err := r.db.WithContext(ctx).Exec(
		"UPDATE sys_pack_upgrades SET results = JSON_ARRAY_APPEND(COALESCE(results, JSON_ARRAY()), '$', CAST(? AS JSON)) "+
			"WHERE id = ? AND tenant_id = ?",
		string(b), id, tenantID,
	).Error; err != nil {
		return fmt.Errorf("packinstall.AppendResult: %w", err)
	}
	return nil
}

// FinishRun closes the run and releases the lock. A run already retired by the lease is an error
// (the caller logs it): the row says "interrupted" although the work completed.
func (r *Repository) FinishRun(ctx context.Context, tenantID, id, status string, counts map[string]int) error {
	b, err := json.Marshal(counts)
	if err != nil {
		return fmt.Errorf("packinstall.FinishRun: marshal counts: %w", err)
	}
	res := r.db.WithContext(ctx).Exec(
		"UPDATE sys_pack_upgrades SET status = ?, running_lock = NULL, counts = ?, finished_at = UTC_TIMESTAMP(3) "+
			"WHERE id = ? AND tenant_id = ? AND running_lock = 1",
		status, b, id, tenantID,
	)
	if res.Error != nil {
		return fmt.Errorf("packinstall.FinishRun: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("packinstall.FinishRun: run %s no longer holds the lock", id)
	}
	return nil
}

// RunningRun returns the live lock holder for (tenant, slug), or nil. A run past its lease and a
// rejected (soft-deleted) run are ignored.
func (r *Repository) RunningRun(ctx context.Context, tenantID, slug string) (*vertical_pack.ErrRunInProgress, error) {
	var id string
	var started time.Time
	err := r.db.WithContext(ctx).Raw(
		"SELECT id, started_at FROM sys_pack_upgrades WHERE tenant_id = ? AND pack_slug = ? AND running_lock = 1 "+
			"AND updated_at >= NOW(3) - INTERVAL ? MINUTE AND deleted_at IS NULL LIMIT 1",
		tenantID, slug, leaseMinutes,
	).Row().Scan(&id, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("packinstall.RunningRun: %w", err)
	}
	return &vertical_pack.ErrRunInProgress{RunID: id, StartedAt: started}, nil
}

// ListRuns returns the newest-first history. A "running" row past its lease reads as
// "interrupted" — derived at read time, the GET never writes.
func (r *Repository) ListRuns(ctx context.Context, tenantID, slug string, limit int) ([]vertical_pack.UpgradeRun, error) {
	rows, err := r.db.WithContext(ctx).Raw(
		"SELECT id, pack_slug, from_versions, to_version, source, mode, plan_hash, "+
			"CASE WHEN status = 'running' AND updated_at < NOW(3) - INTERVAL ? MINUTE THEN 'interrupted' ELSE status END, "+
			"decisions, results, counts, created_by, started_at, finished_at "+
			"FROM sys_pack_upgrades WHERE tenant_id = ? AND pack_slug = ? AND deleted_at IS NULL "+
			"ORDER BY started_at DESC, id DESC LIMIT ?",
		leaseMinutes, tenantID, slug, limit,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("packinstall.ListRuns: query: %w", err)
	}
	defer rows.Close()
	var out []vertical_pack.UpgradeRun
	for rows.Next() {
		var (
			run                        vertical_pack.UpgradeRun
			decisions, results, counts []byte
			finished                   sql.NullTime
		)
		if err := rows.Scan(&run.ID, &run.Slug, &run.FromVersions, &run.ToVersion, &run.Source, &run.Mode, &run.PlanHash,
			&run.Status, &decisions, &results, &counts, &run.CreatedBy, &run.StartedAt, &finished); err != nil {
			return nil, fmt.Errorf("packinstall.ListRuns: scan: %w", err)
		}
		run.TenantID = tenantID
		run.Decisions, run.Results, run.Counts = []vertical_pack.UpgradeDecision{}, []vertical_pack.UpgradeResult{}, map[string]int{}
		if len(decisions) > 0 {
			if err := json.Unmarshal(decisions, &run.Decisions); err != nil {
				return nil, fmt.Errorf("packinstall.ListRuns: decode decisions of %s: %w", run.ID, err)
			}
		}
		if len(results) > 0 {
			if err := json.Unmarshal(results, &run.Results); err != nil {
				return nil, fmt.Errorf("packinstall.ListRuns: decode results of %s: %w", run.ID, err)
			}
		}
		if len(counts) > 0 {
			if err := json.Unmarshal(counts, &run.Counts); err != nil {
				return nil, fmt.Errorf("packinstall.ListRuns: decode counts of %s: %w", run.ID, err)
			}
		}
		if finished.Valid {
			t := finished.Time
			run.FinishedAt = &t
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("packinstall.ListRuns: rows: %w", err)
	}
	return out, nil
}

// DeleteItems drops baseline rows (the pack no longer carries the item and it is gone from the
// tenant). Pinned to tenant + slug + exact (type, key).
func (r *Repository) DeleteItems(ctx context.Context, tenantID, slug string, keys [][2]string) error {
	for _, k := range keys {
		if err := r.db.WithContext(ctx).Exec(
			"DELETE FROM sys_pack_items WHERE tenant_id = ? AND pack_slug = ? AND item_type = ? AND item_key = ?",
			tenantID, slug, k[0], k[1],
		).Error; err != nil {
			return fmt.Errorf("packinstall.DeleteItems: %s/%s: %w", k[0], k[1], err)
		}
	}
	return nil
}

// SetProtected flips the "Luôn giữ" flag of ONE baseline row. It never inserts: `field` and
// `role_grant` have no row of their own (a row would read as "missing" in the diff), and an absent
// row means the caller skipped writing the baseline first. Pinned to tenant + slug + type + key.
func (r *Repository) SetProtected(ctx context.Context, tenantID, slug, itemType, itemKey string, protected bool) error {
	if itemType == vertical_pack.ItemField || itemType == vertical_pack.ItemRoleGrant {
		return fmt.Errorf("packinstall.SetProtected: item type %q has no baseline row of its own", itemType)
	}
	res := r.db.WithContext(ctx).Exec(
		"UPDATE sys_pack_items SET protected = ? WHERE tenant_id = ? AND pack_slug = ? AND item_type = ? AND item_key = ?",
		protected, tenantID, slug, itemType, itemKey)
	if res.Error != nil {
		return fmt.Errorf("packinstall.SetProtected: %s/%s: %w", itemType, itemKey, res.Error)
	}
	if res.RowsAffected == 0 {
		// Without clientFoundRows MySQL counts changed rows: 0 may be a same-value no-op on an existing row.
		var n int64
		if err := r.db.WithContext(ctx).Raw(
			"SELECT COUNT(*) FROM sys_pack_items WHERE tenant_id = ? AND pack_slug = ? AND item_type = ? AND item_key = ?",
			tenantID, slug, itemType, itemKey).Scan(&n).Error; err != nil {
			return fmt.Errorf("packinstall.SetProtected: probe %s/%s: %w", itemType, itemKey, err)
		}
		if n == 0 {
			return fmt.Errorf("packinstall.SetProtected: no baseline row %s/%s", itemType, itemKey)
		}
	}
	return nil
}

func nonNilDecisions(d []vertical_pack.UpgradeDecision) []vertical_pack.UpgradeDecision {
	if d == nil {
		return []vertical_pack.UpgradeDecision{}
	}
	return d
}
