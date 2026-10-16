package dynrepo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/google/uuid"
)

// batchBaseKey carries the base id of a leased batch (specs/047 FR-13) on a context.
type batchBaseKey struct{}

// WithBatchBaseID returns ctx telling InsertRecordsAtomic / InsertRecords to give the batch's rows the
// ids BatchRowID(base, i) instead of minting random ones. A leased Idempotency-Key claim stores base
// BEFORE the write, so a claim stuck in processing can later be reconciled against obj_* by probing
// one id (row 0 = base): the batch is ONE transaction, so that row exists iff the whole batch
// committed — and the ids of every row are derivable, so an atomic bulk's response can be rebuilt.
// Kept apart from WithRecordID (single-record inserts): an id preset for ONE record must never be
// reused by the several rows of a batch.
func WithBatchBaseID(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, batchBaseKey{}, base)
}

// BatchBaseIDFrom returns the base id WithBatchBaseID put on ctx, or "" (the batch then mints random ids).
func BatchBaseIDFrom(ctx context.Context) string {
	base, _ := ctx.Value(batchBaseKey{}).(string)
	return base
}

// BatchRowID is the id of row i of a batch whose base id is base: row 0 is base itself; row i > 0 is
// the name-based UUID of i in base's namespace. "" when base is not a UUID.
func BatchRowID(base string, i int) string {
	ns, err := uuid.Parse(base)
	if err != nil {
		return ""
	}
	if i == 0 {
		return ns.String()
	}
	return uuid.NewSHA1(ns, []byte(strconv.Itoa(i))).String()
}

// batchRowIDs returns the n row ids of a batch insert: derived from the base id on ctx
// (WithBatchBaseID), else n fresh random UUIDs.
func batchRowIDs(ctx context.Context, n int) ([]string, error) {
	ids := make([]string, n)
	base := BatchBaseIDFrom(ctx)
	for i := range ids {
		if base == "" {
			ids[i] = uuid.NewString()
			continue
		}
		if ids[i] = BatchRowID(base, i); ids[i] == "" {
			return nil, fmt.Errorf("dynrepo.batchRowIDs: batch base id %q is not a UUID", base)
		}
	}
	return ids, nil
}

// InsertRecordsAtomic inserts N pre-validated, pre-computed row payloads for the SAME
// (entity, tenant, creator) in ONE transaction (ENG-BATCH #1 atomic bulk-create). Unlike
// InsertRecords (a single multi-row `VALUES (...),(...)` statement used by CSV import, which
// cannot attribute a constraint violation to a specific row), rows here are inserted ONE AT A
// TIME inside the SAME shared transaction — so a duplicate/data-constraint violation is
// attributable to an EXACT zero-based row index, which the per-row bulk-create envelope
// (results[i]) needs. team_set_id is resolved ONCE for the creator (mirrors InsertRecord /
// InsertRecords — A5 §B4), not once per row.
//
// On success: returns (ids, -1, nil) — one generated UUID per row (BatchRowID(base, i) when ctx carries a
// WithBatchBaseID), index-aligned with rows.
// On the FIRST row whose INSERT fails: the transaction is rolled back in FULL (deferred
// tx.Rollback(); Commit is never reached, so no partial commit is ever possible) and the method
// returns (nil, i, err) where i is the failing row's zero-based index and err is
// *DuplicateUniqueError / *DataConstraintError (the SAME typed sentinels InsertRecord returns)
// or a wrapped driver error for anything else. The caller maps err with the SAME
// asDuplicate/asDataConstraint helpers it already uses for a single InsertRecord failure.
func (r *Repository) InsertRecordsAtomic(ctx context.Context, entity, tenantID, userID string, rows []map[string]any, uniqueFields []UniqueFieldSpec) ([]string, int, error) {
	if len(rows) == 0 {
		return nil, -1, nil
	}
	table, err := tableFor(entity)
	if err != nil {
		return nil, -1, fmt.Errorf("dynrepo.InsertRecordsAtomic: %w", err)
	}
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return nil, -1, fmt.Errorf("dynrepo.InsertRecordsAtomic: %w", err)
	}

	rowIDs, err := batchRowIDs(ctx, len(rows))
	if err != nil {
		return nil, -1, fmt.Errorf("dynrepo.InsertRecordsAtomic: %w", err)
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, -1, fmt.Errorf("dynrepo.InsertRecordsAtomic: begin tx: %w", err)
	}
	// Roll back on any error path; a committed tx makes the rollback a no-op.
	defer func() { _ = tx.Rollback() }()

	// Resolve the creator's team_set ONCE (A5 §B4), exactly like InsertRecord/InsertRecords.
	var teamSetID sql.NullString
	if userID != "" {
		teamIDs, terr := listUserTeamIDs(ctx, tx, tenantID, userID)
		if terr != nil {
			return nil, -1, fmt.Errorf("dynrepo.InsertRecordsAtomic: %w", terr)
		}
		if len(teamIDs) > 0 {
			setID, serr := resolveOrCreateTeamSet(ctx, tx, tenantID, teamIDs)
			if serr != nil {
				return nil, -1, fmt.Errorf("dynrepo.InsertRecordsAtomic: %w", serr)
			}
			teamSetID = sql.NullString{String: setID, Valid: true}
		}
	}

	// owner_id starts equal to created_by, mirroring InsertRecord (TRỤ 2 mục 2).
	stmt := fmt.Sprintf(
		"INSERT INTO `%s` (`id`, `tenant_id`, `team_set_id`, `created_by`, `owner_id`, `data`) VALUES (?, ?, ?, ?, ?, ?)", table)

	ids := make([]string, 0, len(rows))
	for i, row := range rows {
		payload, merr := json.Marshal(row)
		if merr != nil {
			return nil, i, fmt.Errorf("dynrepo.InsertRecordsAtomic: marshal row %d: %w", i, merr)
		}
		id := rowIDs[i]
		if _, eerr := tx.ExecContext(ctx, stmt, id, tenantID, teamSetID, userID, userID, payload); eerr != nil {
			if idxName, ok := duplicateIndexName(eerr); ok {
				return nil, i, fmt.Errorf("dynrepo.InsertRecordsAtomic: row %d: %w", i, &DuplicateUniqueError{IndexName: idxName})
			}
			// W5-B4: a value that overflows / mistypes a generated column (1264/1406/1292/1366)
			// is a 422-class input error, not a 500 — classify it for the service layer, exactly
			// like InsertRecord.
			if dce, ok := asDataConstraintError(eerr); ok {
				return nil, i, fmt.Errorf("dynrepo.InsertRecordsAtomic: row %d: %w", i, dce)
			}
			if isWriteConflict(eerr) {
				return nil, i, fmt.Errorf("dynrepo.InsertRecordsAtomic: row %d: %w", i, ErrWriteConflict)
			}
			return nil, i, fmt.Errorf("dynrepo.InsertRecordsAtomic: row %d exec: %w", i, eerr)
		}
		// RM-28: dual-write this row's pivot in the SAME tx (read-back gencol). An intra-batch
		// duplicate that the still-active obj_ index did not already catch trips uq_pivot 1062 →
		// attribute to THIS row index and roll the whole batch back.
		if perr := insertPivotForRecord(ctx, tx, table, tenantID, entityName, id, uniqueFields); perr != nil {
			return nil, i, fmt.Errorf("dynrepo.InsertRecordsAtomic: row %d: %w", i, perr)
		}
		// specs/064 S9-4: mark each row in the same tx; a failing mark aborts the whole batch.
		if merr := insertSampleMark(ctx, tx, tenantID, entityName, id, userID); merr != nil {
			return nil, i, fmt.Errorf("dynrepo.InsertRecordsAtomic: row %d: %w", i, merr)
		}
		ids = append(ids, id)
	}

	if err := tx.Commit(); err != nil {
		return nil, -1, fmt.Errorf("dynrepo.InsertRecordsAtomic: commit: %w: %w", ErrCommitOutcomeUnknown, err)
	}
	return ids, -1, nil
}
