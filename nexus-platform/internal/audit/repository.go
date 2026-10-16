package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
)

// SQLRepository persists and reads audit_log rows over sqlx with parameterized queries. It
// mirrors the dynrepo/notify style: identifiers are static (a fixed table), values always
// travel as `?` placeholders, and tenant_id pins every statement (multi-tenant invariant).
type SQLRepository struct {
	db  *sqlx.DB
	now func() time.Time // injectable clock; defaults to time.Now
}

// NewSQLRepository wires the audit repository over an existing sqlx connection.
func NewSQLRepository(db *sqlx.DB) *SQLRepository {
	return &SQLRepository{db: db, now: time.Now}
}

// InsertEvent appends one row to audit_log (append-only). It is the Writer the
// AsyncDispatcher drains to. diff is JSON-marshalled to the JSON column (NULL when nil), and
// record_id is stored NULL for admin-meta mutations (empty RecordID). `at` uses the
// repository clock with millisecond precision so the TIMESTAMP(3) column orders mutations
// deterministically alongside the id tie-break.
func (r *SQLRepository) InsertEvent(ctx context.Context, evt Event) error {
	id := uuid.NewString()

	var diffJSON sql.NullString
	if evt.Diff != nil {
		b, err := json.Marshal(evt.Diff)
		if err != nil {
			return fmt.Errorf("audit.SQLRepository.InsertEvent: marshal diff: %w", err)
		}
		diffJSON = sql.NullString{String: string(b), Valid: true}
	}

	var recordID sql.NullString
	if strings.TrimSpace(evt.RecordID) != "" {
		recordID = sql.NullString{String: evt.RecordID, Valid: true}
	}

	const q = `INSERT INTO audit_log
		(id, tenant_id, actor_user_id, entity_name, record_id, action, diff, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := r.db.ExecContext(ctx, q,
		id, evt.TenantID, evt.ActorUserID, evt.EntityName, recordID, evt.Action,
		diffJSON, r.now().UTC(),
	); err != nil {
		return fmt.Errorf("audit.SQLRepository.InsertEvent: %w", err)
	}
	return nil
}

// ExistingCreateIDs returns which of ids already have an audit `create` line for the entity — the
// per-record "already handled" mark of the CSV import's history job (specs/048 S7a), so a retried
// job resumes instead of writing a second line. Pinned to tenant_id and entity_name (served by
// idx_al_record); ids travel as `?` placeholders.
func (r *SQLRepository) ExistingCreateIDs(ctx context.Context, tenantID, entity string, ids []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	q := "SELECT DISTINCT record_id FROM audit_log WHERE tenant_id = ? AND entity_name = ? AND action = ? AND record_id IN (" +
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
	args := make([]any, 0, 3+len(ids))
	args = append(args, tenantID, entity, ActionCreate)
	for _, id := range ids {
		args = append(args, id)
	}
	var found []string
	if err := r.db.SelectContext(ctx, &found, q, args...); err != nil {
		return nil, fmt.Errorf("audit.ExistingCreateIDs: %w", err)
	}
	for _, id := range found {
		out[id] = struct{}{}
	}
	return out, nil
}

// AuditRow is one persisted audit_log row as returned by the read API. Diff is RawJSON — raw
// JSON forwarded verbatim without re-marshalling, AND NULL-scannable (W7 fix: a NULL diff row
// previously failed the whole page's scan because json.RawMessage cannot receive SQL NULL —
// the live "limit>N → 500 AUDIT_QUERY_FAILED" bug). RecordID is a pointer so an admin-meta
// row (NULL record_id) serializes as null.
type AuditRow struct {
	ID          string    `db:"id" json:"id"`
	TenantID    string    `db:"tenant_id" json:"-"`
	ActorUserID string    `db:"actor_user_id" json:"actorUserId"`
	EntityName  string    `db:"entity_name" json:"entityName"`
	RecordID    *string   `db:"record_id" json:"recordId"`
	Action      string    `db:"action" json:"action"`
	Diff        RawJSON   `db:"diff" json:"diff"`
	At          time.Time `db:"at" json:"at"`
	// ActorLabel is the builder's display name ("Họ tên (Tên xưởng)") when the row's actor is a Nexus
	// builder who worked inside this tenant — computed at READ time by Query (specs/062 L3, plan §6),
	// never stored. nil for ordinary customer users and engine actors.
	ActorLabel *string `db:"actor_label" json:"actorLabel,omitempty"`
}

// QueryFilter is the validated filter set for an audit-log listing (spec §3). Empty string
// fields are ignored. From/To bound the `at` timestamp inclusively when non-nil; ToExclusive
// is a strict `at < ?` upper bound (W7: the whole-day `?dateTo=` semantics — dateTo+1d
// 00:00:00 — without the 23:59:59-to-midnight sub-second gap; mirrors run_overlay.go). Limit
// is the page size; After is the opaque cursor from a prior page ("" → first page).
type QueryFilter struct {
	EntityName  string
	RecordID    string
	ActorUserID string
	Action      string
	From        *time.Time
	To          *time.Time
	ToExclusive *time.Time
	// BuilderOnly keeps only rows that carry an actorLabel (a Nexus builder who worked in this tenant) —
	// the "Người dựng Nexus" filter (specs/062 T071). It tests the SAME actorLabelSQL the Badge shows,
	// so the filter and the label can never disagree.
	BuilderOnly bool
	Limit       int
	After       string
}

// maxAuditLimit / defaultAuditLimit bound the page size so a caller cannot request an
// unbounded scan. Mirrors the record-list clamps.
const (
	defaultAuditLimit = 50
	maxAuditLimit     = 200
)

// auditCursorSort / auditCursorOrder bind audit cursors to the one fixed keyset (at DESC, id DESC),
// so a record-list cursor replayed here (or the reverse) is 400 INVALID_CURSOR, not a wrong page.
const (
	auditCursorSort  = "at"
	auditCursorOrder = "DESC"
)

// ClampLimit normalizes a requested page size to [1, maxAuditLimit], defaulting non-positive
// values to defaultAuditLimit. Exported so the HTTP handler can echo the EFFECTIVE limit in the
// pagination envelope (the repository applies the identical clamp internally — one rule).
func ClampLimit(n int) int {
	if n <= 0 {
		return defaultAuditLimit
	}
	if n > maxAuditLimit {
		return maxAuditLimit
	}
	return n
}

// actorLabelSQL is THE single expression that names a Nexus builder on an audit row (specs/062 L3,
// plan §6). List, CSV and RecordHistory all go through Query, so the Badge, the CSV column and the
// "Người dựng Nexus" filter can never disagree. No audit_log column and no backfill: the label is
// looked up when the row is READ, so rows written before the session opened carry it too.
//
//  1. the newest live workspace session of this actor IN THIS TENANT (tenant_id pinned — a session
//     the same builder holds in another customer never labels this customer's rows);
//  2. else diff.after.by, the one key cross-workspace operations that bypass sessions (backup /
//     restore, owner-tier flip) write as "Họ tên (Tên xưởng)". Guarded so a customer cannot forge
//     the Badge: only reserved meta keys (`__…__`; a dynamic table's data key `by` never counts),
//     only a string value, and only when the actor has NO users row in the tenant.
const actorLabelSQL = `COALESCE(
		(SELECT ws.actor_label FROM sys_workspace_sessions ws
		  WHERE ws.tenant_id = a.tenant_id AND ws.actor_user_id = a.actor_user_id AND ws.deleted_at IS NULL
		  ORDER BY ws.created_at DESC LIMIT 1),
		CASE WHEN a.entity_name LIKE '\_\_%'
		      AND JSON_TYPE(JSON_EXTRACT(a.diff, '$.after.by')) = 'STRING'
		      AND NOT EXISTS (SELECT 1 FROM users u WHERE u.tenant_id = a.tenant_id AND u.id = a.actor_user_id)
		THEN LEFT(JSON_UNQUOTE(JSON_EXTRACT(a.diff, '$.after.by')), 191) END)`

// Query returns a tenant-scoped page of audit rows newest-first, plus an opaque nextCursor
// ("" on the last page). It uses keyset (cursor) pagination on (at DESC, id DESC) so every
// page is O(limit) regardless of depth — no OFFSET, no COUNT. `at` is NOT NULL (column
// default), so the cursor never carries a NULL-sort flag; the keyset predicate is the
// plain DESC tie-broken comparison `(at < ?) OR (at = ? AND id < ?)`.
//
// Multi-tenant: tenant_id pins the WHERE clause unconditionally; an audit row of another
// tenant can never appear. Every dynamic filter travels as a `?` placeholder.
func (r *SQLRepository) Query(ctx context.Context, tenantID string, f QueryFilter) ([]AuditRow, string, error) {
	limit := ClampLimit(f.Limit)

	var where []string
	var args []any
	where = append(where, "tenant_id = ?")
	args = append(args, tenantID)

	if f.EntityName != "" {
		where = append(where, "entity_name = ?")
		args = append(args, f.EntityName)
	}
	if f.RecordID != "" {
		where = append(where, "record_id = ?")
		args = append(args, f.RecordID)
	}
	if f.ActorUserID != "" {
		where = append(where, "actor_user_id = ?")
		args = append(args, f.ActorUserID)
	}
	if f.Action != "" {
		where = append(where, "action = ?")
		args = append(args, f.Action)
	}
	if f.BuilderOnly {
		where = append(where, actorLabelSQL+" IS NOT NULL")
	}
	if f.From != nil {
		where = append(where, "at >= ?")
		args = append(args, f.From.UTC())
	}
	if f.To != nil {
		where = append(where, "at <= ?")
		args = append(args, f.To.UTC())
	}
	if f.ToExclusive != nil {
		where = append(where, "at < ?")
		args = append(args, f.ToExclusive.UTC())
	}

	// Keyset predicate from the cursor: rows strictly after the boundary in (at DESC, id DESC).
	if strings.TrimSpace(f.After) != "" {
		isNull, sortVal, id, err := dynrepo.DecodeCursor(f.After, auditCursorSort, auditCursorOrder)
		if err != nil {
			return nil, "", fmt.Errorf("audit.SQLRepository.Query: %w", err)
		}
		if isNull {
			// `at` is NOT NULL so a NULL-sort cursor is malformed for this table.
			return nil, "", fmt.Errorf("audit.SQLRepository.Query: %w", dynrepo.ErrInvalidCursor)
		}
		// DESC tie-break: next page is everything below the boundary row.
		where = append(where, "(at < ? OR (at = ? AND id < ?))")
		args = append(args, sortVal, sortVal, id)
	}

	// Fetch limit+1 to detect whether a further page exists without a COUNT.
	q := fmt.Sprintf(`SELECT a.id, a.tenant_id, a.actor_user_id, a.entity_name, a.record_id, a.action, a.diff, a.at,
		%s AS actor_label
		FROM audit_log a
		WHERE %s
		ORDER BY at DESC, id DESC
		LIMIT ?`, actorLabelSQL, strings.Join(where, " AND "))
	args = append(args, limit+1)

	var rows []AuditRow
	if err := r.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, "", fmt.Errorf("audit.SQLRepository.Query: %w", err)
	}

	nextCursor := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		// `at` is rendered as the MySQL-native text the keyset comparison uses. Match the
		// CAST the dynrepo cursor path uses: TIMESTAMP(3) text "2006-01-02 15:04:05.000".
		nextCursor = dynrepo.EncodeCursor(auditCursorSort, auditCursorOrder, false, last.At.UTC().Format("2006-01-02 15:04:05.000"), last.ID)
	}
	return rows, nextCursor, nil
}
