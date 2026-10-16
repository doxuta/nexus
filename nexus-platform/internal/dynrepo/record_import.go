package dynrepo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"nexus/platform/internal/ddl"
)

// dedupChunkSize is the maximum number of values per IN() clause in BatchExistingValuesForField.
// MySQL handles large IN() lists fine, but chunking avoids protocol/stack issues and keeps
// query plans predictable. At 5000-row imports with unique values this is ≤10 round-trips
// instead of ≤5000. Exported for integration tests to verify chunking behaviour.
const DedupChunkSize = 500

// importRow is an internal struct holding a fully-prepared record row for batch insert.
type importRow struct {
	id        string
	teamSetID string // empty string means NULL
	createdBy string
	data      []byte
}

// InsertRecords batch-inserts pre-validated row payloads into the entity's obj_* table in
// a SINGLE transaction. team_set_id is resolved ONCE for the creator (not per row) so we
// avoid N round-trips. Returns the ids of the inserted rows, in input order (len == len(rows) on success) — the CSV
// import's audit job (specs/048 S7a) names records by them.
//
// The caller must send only validated, clean payloads: this function does NOT re-validate.
// If the batch violates a unique constraint, it returns ErrDuplicateUnique.
func (r *Repository) InsertRecords(ctx context.Context, entity, tenantID, userID string, rows []map[string]any, uniqueFields []UniqueFieldSpec) ([]string, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	table, err := tableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.InsertRecords: %w", err)
	}
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.InsertRecords: %w", err)
	}

	rowIDs, err := batchRowIDs(ctx, len(rows))
	if err != nil {
		return nil, fmt.Errorf("dynrepo.InsertRecords: %w", err)
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.InsertRecords: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Resolve team_set once for the creator (A5 §B4).
	var teamSetID sql.NullString
	if userID != "" {
		teamIDs, terr := listUserTeamIDs(ctx, tx, tenantID, userID)
		if terr != nil {
			return nil, fmt.Errorf("dynrepo.InsertRecords: %w", terr)
		}
		if len(teamIDs) > 0 {
			setID, serr := resolveOrCreateTeamSet(ctx, tx, tenantID, teamIDs)
			if serr != nil {
				return nil, fmt.Errorf("dynrepo.InsertRecords: %w", serr)
			}
			teamSetID = sql.NullString{String: setID, Valid: true}
		}
	}

	// Prepare import rows (marshal data to JSON, generate UUIDs).
	irows := make([]importRow, 0, len(rows))
	for i, row := range rows {
		payload, merr := json.Marshal(row)
		if merr != nil {
			return nil, fmt.Errorf("dynrepo.InsertRecords: marshal: %w", merr)
		}
		ts := ""
		if teamSetID.Valid {
			ts = teamSetID.String
		}
		irows = append(irows, importRow{
			id:        rowIDs[i],
			teamSetID: ts,
			createdBy: userID,
			data:      payload,
		})
	}

	// Build and execute a multi-row INSERT in one statement.
	stmt, err := buildInsertSQL(table, len(irows))
	if err != nil {
		return nil, fmt.Errorf("dynrepo.InsertRecords: %w", err)
	}
	args := buildBatchArgs(tenantID, irows)

	if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
		if _, ok := duplicateIndexName(err); ok {
			return nil, fmt.Errorf("dynrepo.InsertRecords: %w", ErrDuplicateUnique)
		}
		if isWriteConflict(err) {
			return nil, fmt.Errorf("dynrepo.InsertRecords: %w", ErrWriteConflict)
		}
		return nil, fmt.Errorf("dynrepo.InsertRecords: exec: %w", err)
	}
	// RM-28: set-based pivot dual-write for the whole batch in the SAME tx (read-back gencol per
	// field over the batch ids). A duplicate the still-active obj_ index did not catch trips
	// uq_pivot 1062 → the batch rolls back (all-or-nothing import semantics preserved).
	if len(uniqueFields) > 0 {
		batchIDs := make([]string, len(irows))
		for i := range irows {
			batchIDs[i] = irows[i].id
		}
		if perr := insertPivotForBatch(ctx, tx, table, tenantID, entityName, batchIDs, uniqueFields); perr != nil {
			return nil, fmt.Errorf("dynrepo.InsertRecords: %w", perr)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("dynrepo.InsertRecords: commit: %w: %w", ErrCommitOutcomeUnknown, err)
	}
	ids := make([]string, len(irows))
	for i := range irows {
		ids[i] = irows[i].id
	}
	return ids, nil
}

// buildInsertSQL constructs a parameterized multi-row INSERT statement for n rows.
// table must be the already-sanitized obj_* table name. Returns an error for n==0.
func buildInsertSQL(table string, n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("buildInsertSQL: n must be > 0, got %d", n)
	}
	// Each tuple: (id, tenant_id, team_set_id, created_by, owner_id, data). owner_id starts
	// equal to created_by, mirroring InsertRecord/InsertRecordsAtomic (TRỤ 2 mục 2).
	tuple := "(?, ?, ?, ?, ?, ?)"
	tuples := make([]string, n)
	for i := range tuples {
		tuples[i] = tuple
	}
	stmt := fmt.Sprintf(
		"INSERT INTO `%s` (`id`, `tenant_id`, `team_set_id`, `created_by`, `owner_id`, `data`) VALUES %s",
		table, strings.Join(tuples, ", "))
	return stmt, nil
}

// buildBatchArgs flattens importRows into a flat args slice for ExecContext.
// Each row contributes 6 args: id, tenant_id, team_set_id (NULL when empty), created_by,
// owner_id (== createdBy), data.
func buildBatchArgs(tenantID string, rows []importRow) []any {
	args := make([]any, 0, len(rows)*6)
	for _, row := range rows {
		var ts any
		if row.teamSetID == "" {
			ts = nil
		} else {
			ts = row.teamSetID
		}
		args = append(args, row.id, tenantID, ts, row.createdBy, row.createdBy, row.data)
	}
	return args
}

// DedupFieldSpec carries the minimal generated-column metadata BatchExistingValuesForField
// needs to choose between a physical indexed column and the JSON fallback expression. It is
// intentionally narrow (not the full services.FieldSpec) so dynrepo stays decoupled from the
// services package.
type DedupFieldSpec struct {
	// IsUnique signals the field has a unique generated column named after the field
	// (utf8mb4_bin collation). BatchExistingValuesForField queries the physical column so the
	// unique index is used and collation matches the constraint (byte-exact comparison).
	IsUnique bool
	// IsIndexed signals the field has a non-unique generated column named after the field
	// (D1 design). BatchExistingValuesForField queries the physical column so the index is used.
	IsIndexed bool
}

// BatchExistingValuesForField returns the subset of values that already exist in the entity's
// obj_* table for the given tenant. It replaces the old per-value ValueExistsForField loop:
// instead of one COUNT(*) query per value (O(N) round-trips), it issues a single
// SELECT <value-expr> ... WHERE <value-expr> IN (?, ...) per chunk of up to DedupChunkSize
// values, returning the set of matched stored values in one (or a few) round-trips.
//
// Column selection (mirrors the RM-13 filter-rewrite path in record_orchestration.go):
//   - IsUnique or IsIndexed → physical generated column named after the field (index-used).
//   - Otherwise → JSON_UNQUOTE(JSON_EXTRACT(data, '$.field')) — same expression the old path
//     used, preserving identical semantics for non-indexed fields.
//
// All identifiers are sanitized through ddl.SanitizeIdentifier; values travel as `?` placeholders.
// Tenant-scoping (tenant_id = ?) and soft-delete filter (deleted_at IS NULL) are always applied.
func (r *Repository) BatchExistingValuesForField(
	ctx context.Context,
	entity, tenantID, fieldName string,
	spec DedupFieldSpec,
	values []string,
) (map[string]struct{}, error) {
	if len(values) == 0 {
		return map[string]struct{}{}, nil
	}

	table, err := tableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchExistingValuesForField: %w", err)
	}
	col, err := ddl.SanitizeIdentifier(fieldName)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchExistingValuesForField: field %q: %w", fieldName, err)
	}

	// Build the value expression: physical column for indexed/unique (uses the B-tree index),
	// JSON path expression for plain fields (same expression as the old per-value path).
	var valueExpr string
	if spec.IsUnique || spec.IsIndexed {
		// Physical generated column: name == field name (from ddl.BuildAddUniqueColumn /
		// BuildAddIndexedColumn). Quoting with backticks; col is already sanitized.
		valueExpr = fmt.Sprintf("`%s`", col)
	} else {
		valueExpr = fmt.Sprintf("JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s'))", col)
	}

	existing := make(map[string]struct{}, len(values))

	// Process in chunks so the IN() list stays within a safe parameter count.
	for start := 0; start < len(values); start += DedupChunkSize {
		end := start + DedupChunkSize
		if end > len(values) {
			end = len(values)
		}
		chunk := values[start:end]

		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1] // trim trailing comma

		stmt := fmt.Sprintf(
			"SELECT %s FROM `%s` WHERE `tenant_id` = ? AND `deleted_at` IS NULL AND %s IN (%s)",
			valueExpr, table, valueExpr, placeholders,
		)

		// Build args: tenant_id first, then one arg per value in chunk.
		args := make([]any, 0, 1+len(chunk))
		args = append(args, tenantID)
		for _, v := range chunk {
			args = append(args, v)
		}

		var found []string
		if err := r.db.SelectContext(ctx, &found, stmt, args...); err != nil {
			return nil, fmt.Errorf("dynrepo.BatchExistingValuesForField: query chunk [%d,%d): %w", start, end, err)
		}
		for _, v := range found {
			existing[v] = struct{}{}
		}
	}
	return existing, nil
}

// BatchIDsByFieldValue resolves CSV relation cells that name their target by a field value
// (specs/061 FR-014): for each value it returns the ids of the entity's LIVE records in the tenant
// whose field, trimmed and lower-cased, equals it. values must already be trimmed + lower-cased by
// the caller; the result is keyed by MySQL's LOWER(TRIM(...)) of the stored value, so a value whose
// case mapping MySQL and Go disagree on simply finds nothing (the cell is refused, never mismatched).
// A value naming two records maps to both ids — the caller refuses it as ambiguous.
//
// scope is the caller's VIEW row scope on the entity (A5): a record they may not see is not found,
// exactly as GetRecord answers — a name or code is guessable where a UUID is not, so the lookup must
// never reach further than the caller's own reads (the class relation_service Link closed).
//
// One SELECT per chunk of DedupChunkSize values; identifiers sanitized, values as `?` only,
// tenant_id + deleted_at + the row filter always applied.
// ponytail: the JSON expression scans the target table (no index on LOWER(TRIM(json))) — import is
// a one-off batch over a customer's own master data; a generated column if targets reach ~1e5 rows.
func (r *Repository) BatchIDsByFieldValue(ctx context.Context, entity, tenantID, fieldName string, values []string, scope RowScope) (map[string][]string, error) {
	out := make(map[string][]string, len(values))
	if len(values) == 0 {
		return out, nil
	}
	table, err := tableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchIDsByFieldValue: %w", err)
	}
	col, err := ddl.SanitizeIdentifier(fieldName)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchIDsByFieldValue: field %q: %w", fieldName, err)
	}
	if scope, err = r.resolveReadScope(ctx, tenantID, entity, scope); err != nil {
		return nil, fmt.Errorf("dynrepo.BatchIDsByFieldValue: %w", err)
	}
	scopeSQL, scopeArgs := scope.rowFilter()
	valueExpr := fmt.Sprintf("LOWER(TRIM(JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s'))))", col)
	for start := 0; start < len(values); start += DedupChunkSize {
		end := min(start+DedupChunkSize, len(values))
		chunk := values[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		stmt := fmt.Sprintf(
			"SELECT `id`, %s AS `v` FROM `%s` WHERE `tenant_id` = ? AND `deleted_at` IS NULL AND %s IN (%s)%s",
			valueExpr, table, valueExpr, placeholders, scopeSQL,
		)
		args := make([]any, 0, 1+len(chunk)+len(scopeArgs))
		args = append(args, tenantID)
		for _, v := range chunk {
			args = append(args, v)
		}
		args = append(args, scopeArgs...)
		var rows []struct {
			ID string `db:"id"`
			V  string `db:"v"`
		}
		if err := r.db.SelectContext(ctx, &rows, stmt, args...); err != nil {
			return nil, fmt.Errorf("dynrepo.BatchIDsByFieldValue: query chunk [%d,%d): %w", start, end, err)
		}
		for _, row := range rows {
			out[row.V] = append(out[row.V], row.ID)
		}
	}
	return out, nil
}

// BatchRecordData returns the stored data of the live records among ids, keyed by id, pinned to
// the tenant (one parameterized IN query). Ids that are soft-deleted or belong to another tenant
// are simply absent. System read for the CSV import's history job (specs/048 S7a) — no row scope.
func (r *Repository) BatchRecordData(ctx context.Context, entity, tenantID string, ids []string) (map[string]map[string]any, error) {
	out := make(map[string]map[string]any, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	table, err := tableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchRecordData: %w", err)
	}
	stmt := fmt.Sprintf("SELECT `id`, `data` FROM `%s` WHERE `tenant_id` = ? AND `deleted_at` IS NULL AND `id` IN (%s)",
		table, strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","))
	args := make([]any, 0, 1+len(ids))
	args = append(args, tenantID)
	for _, id := range ids {
		args = append(args, id)
	}
	var rows []struct {
		ID   string          `db:"id"`
		Data json.RawMessage `db:"data"`
	}
	if err := r.db.SelectContext(ctx, &rows, stmt, args...); err != nil {
		return nil, fmt.Errorf("dynrepo.BatchRecordData: %w", err)
	}
	for _, row := range rows {
		d := map[string]any{}
		if len(row.Data) > 0 {
			if err := json.Unmarshal(row.Data, &d); err != nil {
				return nil, fmt.Errorf("dynrepo.BatchRecordData: record %s: %w", row.ID, err)
			}
		}
		out[row.ID] = d
	}
	return out, nil
}
