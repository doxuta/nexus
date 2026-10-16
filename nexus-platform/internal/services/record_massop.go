package services

// ENG-MASSOP — mass update + mass delete from the list view (../specs/022-eng-massop/spec.md). Serves POST /:entity/bulk-update {ids, values} and POST /:entity/bulk-delete {ids}.
//
// Deliberately NO SQL shortcut: every id goes through the EXISTING single-record
// RecordService.Update / Delete, so A5 row-scope, field-ACL (RM-42), validation, workflow
// events, BPM arming and audit all fire per record exactly like a single-record call — the
// batch endpoints can never semantically drift from PUT/DELETE /:entity/:id (the same B6
// discipline as bulk-create's partial mode calling Create() per row).
//
// Semantics are ALWAYS partial, per row — including a values-level defect (unknown/readonly/
// computed field, forbidden field, bad type): each row fails with the SAME code a single PUT
// would have returned, and never blocks a sibling. There is NO fail-fast values pre-flight by
// design (per-row like bulk-create partial mode): the per-row Update() call IS the single
// source of truth, and a values defect short-circuits inside it before any store write, so the
// cost of N identical failures is N validation passes — no data touched. Only request-level
// problems (empty/oversized ids, empty values, unknown entity) or an infrastructure error
// (DB timeout — not a data problem) return a top-level error.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// defaultMassOpMaxIDs is the fallback ids cap when MAX_BULK_OPS is unset/invalid (mirrors
// Nexus's MassUpdate 500-row chunk). Applied to the RAW array, before dedupe.
const defaultMassOpMaxIDs = 500

// MassOpMaxIDs returns the ids cap for POST /:entity/bulk-update|bulk-delete (env MAX_BULK_OPS).
// Mirrors BulkMaxRows's env-config pattern (record_bulk.go): a non-positive/unparseable value
// degrades to the default rather than disabling the cap.
func MassOpMaxIDs() int {
	if v := os.Getenv("MAX_BULK_OPS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return defaultMassOpMaxIDs
}

// BulkOpRowResult is one per-id outcome of a bulk-update/bulk-delete call, in the SAME order
// as the (deduped) submitted ids. ok=true rows carry only {id, ok}; failed rows add the SAME
// code/error/details a single-record Update/Delete would have returned for that id.
type BulkOpRowResult struct {
	ID      string         `json:"id"`
	OK      bool           `json:"ok"`
	Code    string         `json:"code,omitempty"`
	Error   string         `json:"error,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// BulkUpdateResult is the response envelope for POST /:entity/bulk-update.
// updated+failed == len(results) always; HTTP status is 200 whenever the batch executed
// (per-row errors live inside), mirroring bulk-create's mode=partial contract.
type BulkUpdateResult struct {
	Results []BulkOpRowResult `json:"results"`
	Updated int               `json:"updated"`
	Failed  int               `json:"failed"`
}

// BulkDeleteResult is the response envelope for POST /:entity/bulk-delete
// (deleted+failed == len(results) always).
type BulkDeleteResult struct {
	Results []BulkOpRowResult `json:"results"`
	Deleted int               `json:"deleted"`
	Failed  int               `json:"failed"`
}

// UpdateBulk applies ONE shared values patch to every id via the single-record Update path.
// values is validated PER ROW by Update() itself (field-ACL write guard, ValidatePartialRecord
// incl. computed/readonly rejection, relation-FK probe) — a values-level defect therefore fails
// each row with the identical code a single PUT returns (RECORD_UNKNOWN_FIELD,
// RECORD_*_READONLY, FIELD_FORBIDDEN, …) instead of pre-rejecting the whole batch.
// expectedVersion is never pinned: mass update is unconditional last-writer-wins by design (a
// 500-row batch cannot meaningfully pin per-row versions); a `values` key literally named
// "version" is rejected per-row as RECORD_UNKNOWN_FIELD like any other undeclared field.
func (s *RecordService) UpdateBulk(ctx context.Context, scope RecordScope, tenantID, entityName string, ids []string, values map[string]any) (BulkUpdateResult, error) {
	uids, err := normalizeMassOpIDs(ids)
	if err != nil {
		return BulkUpdateResult{}, err
	}
	if len(values) == 0 {
		return BulkUpdateResult{}, invalid("BULK_VALUES_EMPTY", "values rỗng — cần ít nhất 1 field để cập nhật")
	}
	// Resolve once up front so an unknown/invalid entity is a request-level 404
	// (ErrEntityNotFound), not N identical per-row failures. Update() re-resolves per row —
	// the same cost as N separate PUTs, which is this endpoint's documented cost model.
	e, _, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return BulkUpdateResult{}, err
	}
	results, ok, failed, err := s.massOp(ctx, uids, "UpdateBulk", func(ctx context.Context, id string) error {
		// Update never mutates `values` (ValidatePartialRecord builds its own clean map), so
		// sharing one payload map across rows is safe.
		return s.Update(ctx, scope, tenantID, e.Name, id, values, nil)
	})
	if err != nil {
		return BulkUpdateResult{}, err
	}
	return BulkUpdateResult{Results: results, Updated: ok, Failed: failed}, nil
}

// DeleteBulk soft-deletes every id via the single-record Delete path (A5 row-scope, audit,
// after_delete workflows per record). A record the caller may not see fails its row as
// RECORD_NOT_FOUND — identical to DELETE /:entity/:id.
func (s *RecordService) DeleteBulk(ctx context.Context, scope RecordScope, tenantID, entityName string, ids []string) (BulkDeleteResult, error) {
	uids, err := normalizeMassOpIDs(ids)
	if err != nil {
		return BulkDeleteResult{}, err
	}
	e, _, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return BulkDeleteResult{}, err
	}
	results, ok, failed, err := s.massOp(ctx, uids, "DeleteBulk", func(ctx context.Context, id string) error {
		return s.Delete(ctx, scope, tenantID, e.Name, id)
	})
	if err != nil {
		return BulkDeleteResult{}, err
	}
	return BulkDeleteResult{Results: results, Deleted: ok, Failed: failed}, nil
}

// normalizeMassOpIDs enforces the ids contract shared by both mass ops: at most MassOpMaxIDs()
// RAW entries (capped before dedupe — the documented "ids ≤ cap" reads literally), then trim,
// drop blank entries, and dedupe preserving first-seen order (spec: "Dedupe ids. Order of
// results = order of ids" — a duplicated id must not be updated/audited twice, and its second
// soft-delete would confusingly report "not found"). Nothing left → BULK_EMPTY.
func normalizeMassOpIDs(ids []string) ([]string, error) {
	maxIDs := MassOpMaxIDs()
	if len(ids) > maxIDs {
		ve, _ := invalid("BULK_TOO_MANY",
			"batch có %d id, vượt quá giới hạn %d id mỗi lần gọi", len(ids), maxIDs).(*ValidationError)
		ve.Details["max"] = maxIDs
		return nil, ve
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, invalid("BULK_EMPTY", "ids rỗng — cần ít nhất 1 id bản ghi")
	}
	return out, nil
}

// massOp runs op once per id, collecting per-row outcomes without ever aborting the batch on a
// DATA failure. Infra-vs-validation classification mirrors createBulkPartial exactly: a
// *ValidationError or ErrRecordNotFound is that row's own outcome (reported with its real code,
// loop continues); anything else (DB timeout, dropped connection, vanished entity) is NOT a data
// problem and returns as a top-level error so respondError classifies it (503 DB_TIMEOUT / 500).
// Rows already processed before an infra abort are each an independently committed single-record
// op — not lost, just absent from THIS response (same contract as bulk-create partial).
func (s *RecordService) massOp(ctx context.Context, ids []string, op string, do func(ctx context.Context, id string) error) (results []BulkOpRowResult, ok, failed int, err error) {
	results = make([]BulkOpRowResult, 0, len(ids))
	for _, id := range ids {
		derr := do(ctx, id)
		if derr == nil {
			results = append(results, BulkOpRowResult{ID: id, OK: true})
			ok++
			continue
		}
		var ve *ValidationError
		switch {
		case errors.Is(derr, ErrRecordNotFound):
			// Non-existent / cross-tenant / out-of-A5-scope — same collapsed 404 semantics
			// (no existence leak) as the single-record call, scoped to this row.
			results = append(results, BulkOpRowResult{ID: id, OK: false, Code: "RECORD_NOT_FOUND", Error: "Bản ghi không tồn tại"})
			failed++
		case errors.As(derr, &ve):
			results = append(results, BulkOpRowResult{ID: id, OK: false, Code: ve.Code, Error: ve.Msg, Details: ve.Details})
			failed++
		default:
			return nil, 0, 0, fmt.Errorf("services.%s: id %s: %w", op, id, derr)
		}
	}
	return results, ok, failed, nil
}
