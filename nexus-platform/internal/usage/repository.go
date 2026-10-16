package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
)

// Repository is the sqlx-backed store of sys_usage_daily and the counter queries over its sources.
// Every statement pins tenant_id = ?.
type Repository struct {
	db *sqlx.DB
}

// NewRepository wires the repository over the shared sqlx pool.
func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

// upsertChunk bounds rows per INSERT (5 placeholders per row stays far below the 65535 limit).
const upsertChunk = 200

// CountersForDay counts what the tenant did in [fromUTC, toUTC) (one calendar day of APP_TIMEZONE,
// converted to UTC by the caller). Every metric is present in the result, zero included, so the
// roller can overwrite a figure that fell back to 0. Email counters carry the mail source as dim.
func (r *Repository) CountersForDay(ctx context.Context, tenantID string, fromUTC, toUTC time.Time) (map[Key]int64, error) {
	out := map[Key]int64{}
	for _, m := range Metrics {
		if m == EmailsSent || m == EmailsFailed {
			// Mail is split by source: write a zero for each, so a source whose log rows were deleted
			// falls back to 0 instead of keeping the old figure.
			for _, src := range EmailSources {
				out[Key{m, src}] = 0
			}
			continue
		}
		out[Key{m, ""}] = 0
	}
	from, to := fromUTC.UTC(), toUTC.UTC()

	// Rule runs. A 'skipped' row is a condition that evaluated false — not a run. 'blocked'
	// (specs/048) is a run that was refused, so it counts. ponytail: reads the rows' status; the
	// index covers only (tenant_id, started_at) — past ~100k runs/day add status to it.
	var runs struct {
		Total  int64 `db:"total"`
		Failed int64 `db:"failed"`
	}
	if err := r.db.GetContext(ctx, &runs,
		`SELECT COUNT(*) AS total, CAST(COALESCE(SUM(status = 'failed'), 0) AS SIGNED) AS failed
		   FROM sys_workflow_runs
		  WHERE tenant_id = ? AND started_at >= ? AND started_at < ? AND status <> 'skipped'`,
		tenantID, from, to); err != nil {
		return nil, fmt.Errorf("usage.CountersForDay: workflow runs: %w", err)
	}
	out[Key{WorkflowRuns, ""}] = runs.Total
	out[Key{WorkflowRunsFailed, ""}] = runs.Failed

	// Mail written by rules, password reset and reset links (the email log has them all).
	type emailRow struct {
		Status string `db:"status"`
		Source string `db:"source"`
		N      int64  `db:"n"`
	}
	var mails []emailRow
	if err := r.db.SelectContext(ctx, &mails,
		`SELECT status, source, COUNT(*) AS n FROM sys_email_log
		  WHERE tenant_id = ? AND created_at >= ? AND created_at < ? GROUP BY status, source`,
		tenantID, from, to); err != nil {
		return nil, fmt.Errorf("usage.CountersForDay: email log: %w", err)
	}
	for _, m := range mails {
		switch m.Status {
		case "sent":
			out[Key{EmailsSent, m.Source}] += m.N
		case "failed":
			out[Key{EmailsFailed, m.Source}] += m.N
		}
	}

	// Outbound webhooks: the queue's own rows. The hint names the tenant listing index (000075);
	// on a schema without it MySQL ignores the hint (warning 3128) and still answers.
	type jobRow struct {
		Status string `db:"status"`
		N      int64  `db:"n"`
	}
	var jobs []jobRow
	if err := r.db.SelectContext(ctx, &jobs,
		`SELECT /*+ INDEX(sys_jobs idx_sys_jobs_tenant_list) */ status, COUNT(*) AS n FROM sys_jobs
		  WHERE tenant_id = ? AND created_at >= ? AND created_at < ? AND job_type = 'outbound_webhook'
		  GROUP BY status`,
		tenantID, from, to); err != nil {
		return nil, fmt.Errorf("usage.CountersForDay: outbound webhooks: %w", err)
	}
	for _, j := range jobs {
		switch j.Status {
		case "done":
			out[Key{WebhooksDelivered, ""}] += j.N
		case "failed":
			out[Key{WebhooksFailed, ""}] += j.N
		}
	}

	// Inbound hooks: processed signals (created/updated) vs refused ones (error). created_at here
	// is a TIMESTAMP in whole seconds (migration 000056) — the day edges are whole seconds anyway.
	var hooks []jobRow
	if err := r.db.SelectContext(ctx, &hooks,
		`SELECT /*+ INDEX(sys_hook_deliveries idx_hookdel_tenant_created) */ status, COUNT(*) AS n FROM sys_hook_deliveries
		  WHERE tenant_id = ? AND created_at >= ? AND created_at < ? GROUP BY status`,
		tenantID, from, to); err != nil {
		return nil, fmt.Errorf("usage.CountersForDay: inbound hooks: %w", err)
	}
	for _, h := range hooks {
		switch h.Status {
		case "created", "updated":
			out[Key{HooksReceived, ""}] += h.N
		case "error":
			out[Key{HooksFailed, ""}] += h.N
		}
	}

	// People who signed in that day (login and refresh both write a refresh_tokens row). The tenant
	// pin is what keeps a builder session out: it signs in under the builder's own tenant, so its
	// rows are never counted for the customer. Covered by idx_rt_tenant_created (000094).
	var seen int64
	if err := r.db.GetContext(ctx, &seen,
		`SELECT COUNT(DISTINCT user_id) FROM refresh_tokens WHERE tenant_id = ? AND created_at >= ? AND created_at < ?`,
		tenantID, from, to); err != nil {
		return nil, fmt.Errorf("usage.CountersForDay: users seen: %w", err)
	}
	out[Key{UsersSeen, ""}] = seen
	return out, nil
}

// Gauges takes the instant snapshot: active accounts, people seen since seenFromUTC, live rows per
// entity and attachment size. entities are the names the tenant's metadata lists; each table is
// counted through dynrepo.CountLive (name sanitised there). An entity whose table is missing
// (MySQL 1146 — schema drift the Doctor reports) is skipped with a warning so one gap cannot blank
// the whole block. Attachment bytes cover EVERY sys_files row of the tenant, binned or tombstoned:
// the bytes stay on disk until a hard delete.
func (r *Repository) Gauges(ctx context.Context, tenantID string, seenFromUTC time.Time, entities []string, logger *zap.Logger) (*Gauge, error) {
	g := &Gauge{Records: []RecordCount{}}
	if err := r.db.GetContext(ctx, &g.UsersActive,
		`SELECT COUNT(*) FROM users WHERE tenant_id = ? AND status = 'active' AND deleted_at IS NULL`, tenantID); err != nil {
		return nil, fmt.Errorf("usage.Gauges: active users: %w", err)
	}
	if err := r.db.GetContext(ctx, &g.UsersSeen30d,
		`SELECT COUNT(DISTINCT user_id) FROM refresh_tokens WHERE tenant_id = ? AND created_at >= ?`,
		tenantID, seenFromUTC.UTC()); err != nil {
		return nil, fmt.Errorf("usage.Gauges: users seen 30d: %w", err)
	}
	for _, e := range entities {
		n, err := dynrepo.CountLive(ctx, r.db, e, tenantID)
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1146 {
			if logger != nil {
				logger.Warn("usage.Gauges: entity has no table, skipped", zap.String("tenant_id", tenantID), zap.String("entity_name", e))
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("usage.Gauges: records of %s: %w", e, err)
		}
		g.Records = append(g.Records, RecordCount{Entity: e, Count: n})
	}
	var files struct {
		Bytes int64 `db:"bytes"`
		N     int64 `db:"n"`
	}
	if err := r.db.GetContext(ctx, &files,
		`SELECT CAST(COALESCE(SUM(size), 0) AS SIGNED) AS bytes, COUNT(*) AS n FROM sys_files WHERE tenant_id = ?`,
		tenantID); err != nil {
		return nil, fmt.Errorf("usage.Gauges: files: %w", err)
	}
	g.FileBytes, g.FileCount = files.Bytes, files.N
	return g, nil
}

// LatestGauge reads the snapshot the roller stored for day; nil when there is none yet.
func (r *Repository) LatestGauge(ctx context.Context, tenantID, day string) (*Gauge, *time.Time, error) {
	var rows []struct {
		Metric    string    `db:"metric"`
		Dim       string    `db:"dim"`
		Value     int64     `db:"value"`
		UpdatedAt time.Time `db:"updated_at"`
	}
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT metric, dim, value, updated_at FROM sys_usage_daily
		  WHERE tenant_id = ? AND day = ? AND metric IN (?, ?, ?, ?, ?)`,
		tenantID, day, UsersActive, UsersSeen30d, Records, FileBytes, FileCount); err != nil {
		return nil, nil, fmt.Errorf("usage.LatestGauge: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, nil
	}
	g := &Gauge{Records: []RecordCount{}}
	var at time.Time
	for _, row := range rows {
		if row.UpdatedAt.After(at) {
			at = row.UpdatedAt
		}
		switch row.Metric {
		case UsersActive:
			g.UsersActive = row.Value
		case UsersSeen30d:
			g.UsersSeen30d = row.Value
		case FileBytes:
			g.FileBytes = row.Value
		case FileCount:
			g.FileCount = row.Value
		case Records:
			g.Records = append(g.Records, RecordCount{Entity: row.Dim, Count: row.Value})
		}
	}
	return g, &at, nil
}

// Upsert writes rows in bounded multi-row statements. Overwrite replaces the stored value;
// FillMissing keeps an existing row (no INSERT IGNORE: it would turn a real error such as an
// over-long dim into a silent warning).
func (r *Repository) Upsert(ctx context.Context, rows []Row, mode Mode) error {
	for start := 0; start < len(rows); start += upsertChunk {
		end := start + upsertChunk
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*6)
		holders := make([]string, 0, len(chunk))
		for _, row := range chunk {
			holders = append(holders, "(?, ?, ?, ?, ?, ?)")
			args = append(args, uuid.NewString(), row.TenantID, row.Day, row.Metric, row.Dim, row.Value)
		}
		tail := "id = id"
		if mode == Overwrite {
			tail = "value = VALUES(value), updated_at = CURRENT_TIMESTAMP(3)"
		}
		q := "INSERT INTO sys_usage_daily (id, tenant_id, day, metric, dim, value) VALUES " +
			strings.Join(holders, ", ") + " ON DUPLICATE KEY UPDATE " + tail
		if _, err := r.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("usage.Upsert: %w", err)
		}
	}
	return nil
}

// Range returns the stored figures of the tenant for fromDay..toDay (YYYY-MM-DD, inclusive),
// summed over their dimensions.
func (r *Repository) Range(ctx context.Context, tenantID, fromDay, toDay string) ([]DayValue, error) {
	var out []DayValue
	if err := r.db.SelectContext(ctx, &out,
		`SELECT DATE_FORMAT(day, '%Y-%m-%d') AS day, metric, CAST(SUM(value) AS SIGNED) AS value
		   FROM sys_usage_daily WHERE tenant_id = ? AND day >= ? AND day <= ?
		  GROUP BY day, metric ORDER BY day`,
		tenantID, fromDay, toDay); err != nil {
		return nil, fmt.Errorf("usage.Range: %w", err)
	}
	return out, nil
}

// LastRollupAt is the newest write of the tenant's figures from sinceDay on; nil when none.
func (r *Repository) LastRollupAt(ctx context.Context, tenantID, sinceDay string) (*time.Time, error) {
	var t sql.NullTime
	if err := r.db.GetContext(ctx, &t,
		`SELECT MAX(updated_at) FROM sys_usage_daily WHERE tenant_id = ? AND day >= ?`,
		tenantID, sinceDay); err != nil {
		return nil, fmt.Errorf("usage.LastRollupAt: %w", err)
	}
	if !t.Valid {
		return nil, nil
	}
	return &t.Time, nil
}
