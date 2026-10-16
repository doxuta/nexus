package services

// ENG-BATCH #1 — bulk (multi-row) record create. See ../specs/009-bulk-create/spec.md for the full
// design (two-phase VALIDATE-ALL → ASSIGN+INSERT, atomic vs partial mode, error envelope).
//
// This file deliberately does NOT re-implement field validation / relation-FK checking /
// auto_number claiming / formula computation / insert error mapping: every one of those steps is
// the SAME primitive the single-record Create() path already uses (ValidateRecord,
// validateRelations-equivalent batched probe, assignSequences, applyComputedFields,
// asDuplicate/asDataConstraint, emitCreateEvents). mode=partial goes one step further and calls
// Create() ITSELF per surviving row, so the two paths can never semantically drift (B6).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// bulkDeadlockRetries bounds the InsertRecordsAtomic re-runs after a MySQL 1213 deadlock
// rollback. One retry usually suffices (the competing transaction has committed by then);
// the bound keeps a pathological lock storm from looping forever.
const bulkDeadlockRetries = 3

// isMySQLDeadlock reports a transient write-contention that InsertRecordsAtomic already fully rolled
// back, so the one-shot insert is safe to re-run. It matches BOTH a raw MySQL 1213 (ER_LOCK_DEADLOCK
// — InnoDB picked this tx as the deadlock victim) AND the RM-28 ErrWriteConflict sentinel, into
// which InsertRecordsAtomic now classifies a 1213/1205 on either the obj_ INSERT or a pivot
// statement. Without the ErrWriteConflict arm, that classification would silently disable this
// retry. Mirrors the MySQLError-number pattern of relation_service.go's duplicate check.
func isMySQLDeadlock(err error) bool {
	if errors.Is(err, ErrWriteConflict) {
		return true
	}
	var me *gomysql.MySQLError
	return errors.As(err, &me) && me.Number == 1213
}

// defaultBulkMaxRows is the fallback row cap when BULK_MAX_ROWS is unset/invalid.
const defaultBulkMaxRows = 100

// BulkMaxRows returns the row cap for POST /:entity/bulk (env BULK_MAX_ROWS). Mirrors
// ImportMaxRows's env-config pattern (record_import.go) — a non-positive/unparseable value
// falls back to defaultBulkMaxRows rather than disabling the cap.
func BulkMaxRows() int {
	if v := os.Getenv("BULK_MAX_ROWS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return defaultBulkMaxRows
}

// BulkMode selects CreateBulk's all-or-nothing (atomic) vs best-effort (partial) semantics
// (../specs/009-bulk-create/spec.md §2, query param ?mode=).
type BulkMode string

const (
	// BulkModeAtomic is the DEFAULT mode: any row failing Phase 1 (or a DB-level race in Phase 2)
	// aborts the WHOLE batch — zero rows written, zero auto_number values consumed for rows that
	// never reached Phase 2.
	BulkModeAtomic BulkMode = "atomic"
	// BulkModePartial inserts every row that passes validation independently; a failing row is
	// reported per-row without blocking its siblings.
	BulkModePartial BulkMode = "partial"
)

// BulkInserter is the atomic multi-row insert primitive CreateBulk's mode=atomic Phase 2 needs.
// Declared consumer-side (mirrors SequenceClaimer / RollupLinkedLister) so the orchestration
// layer never imports the concrete dynrepo type and unit tests can inject a fake. Satisfied by
// *dynrepo.Repository.
type BulkInserter interface {
	// InsertRecordsAtomic inserts len(rows) pre-validated, pre-computed row payloads for the
	// SAME (entity, tenant, creator) in ONE transaction. On success it returns one generated id
	// per row (index-aligned with rows) and failedIndex == -1. On the FIRST row whose INSERT
	// fails, the ENTIRE transaction is rolled back (no partial commit ever occurs) and the method
	// returns (nil, failedIndex, err): failedIndex is the zero-based index into rows that failed,
	// err is the (possibly typed — DuplicateUniqueError/DataConstraintError) underlying error the
	// caller maps with asDuplicate/asDataConstraint exactly like a single InsertRecord failure.
	// uniqueFields (RM-28) is the entity's active unique fields so the repo dual-writes the pivot
	// per row in the same tx; resolved by the service via uniqueFieldSpecs(specs).
	InsertRecordsAtomic(ctx context.Context, entity, tenantID, userID string, rows []map[string]any, uniqueFields []dynrepo.UniqueFieldSpec) (ids []string, failedIndex int, err error)
}

// BulkRowResult mirrors one element of the ../specs/009-bulk-create/spec.md §2 `results[]` array. The
// zero-value-friendly `omitempty` tags mean a "created" row emits only {index,status,id} and a
// "failed"/"skipped" row emits only {index,status,code,error,details} — never a mix of both.
type BulkRowResult struct {
	Index   int            `json:"index"`
	Status  string         `json:"status"` // "created" | "failed" | "skipped"
	ID      string         `json:"id,omitempty"`
	Code    string         `json:"code,omitempty"`
	Error   string         `json:"error,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// BulkCreateResult is the full response envelope for POST /:entity/bulk.
type BulkCreateResult struct {
	Mode    string `json:"mode"`
	Created int    `json:"created"`
	// Failed counts ONLY rows that actually errored. In an atomic abort the valid rows are
	// reported as status:"skipped" (blocked by another row's failure, not their own fault) and
	// counted in Skipped, not Failed — so created+failed+skipped == len(records) always
	// (review LOW-4: previously Failed==len(rows) over-reported a single bad row as N failures).
	Failed  int             `json:"failed"`
	Skipped int             `json:"skipped,omitempty"`
	Results []BulkRowResult `json:"results"`
}

// bulkRowState is the Phase-1 working state for a single row of the batch, carried into Phase 2.
// A non-nil err at any point means this row is OUT — it never reaches insert, and (for atomic
// mode) its presence is what triggers the whole-batch abort.
type bulkRowState struct {
	index   int            // position in the client's `records` array (echoed as results[i].index)
	payload map[string]any // ORIGINAL raw payload — partial mode re-validates it via Create()
	clean   map[string]any // populated once Phase-1 structural + relation validation passes
	err     error          // non-nil ⇒ this row is excluded from Phase 2
	// declaredRef (ENG-SAMPLE-REF) is this row's OWN reserved "$ref" value (stripped from payload
	// BEFORE ValidateRecord — record_ref_registry.go), if any. Non-empty ⇒ once this row is
	// actually persisted, its real id is registered under this token so a LATER, separate
	// CreateBulk call's relation field can resolve "@<token>" to it.
	declaredRef string
}

// CreateBulk validates every row of payloads (Phase 1), then — depending on mode — either
// inserts all of them in one transaction (atomic) or inserts each surviving row independently
// (partial). scope/tenantID/userID/entityName mirror Create()'s parameters exactly (this IS the
// batch form of Create()). See ../specs/009-bulk-create/spec.md for the full contract.
//
// The returned error is non-nil ONLY for a request-level failure that never reached per-row
// evaluation: empty/oversized batch, an unrecognized mode, an unknown entity, or an unexpected
// infrastructure error. Every per-row outcome (including a whole-batch atomic abort) is reported
// inside BulkCreateResult with a nil error — the caller (handler) derives the HTTP status from
// {mode, Failed}. An infrastructure error mid-way through mode=partial comes back WITH the result
// tallying the rows committed before it (Created > 0 ⇒ the batch was not a no-op).
func (s *RecordService) CreateBulk(ctx context.Context, scope RecordScope, tenantID, userID, entityName string, payloads []map[string]any, mode BulkMode) (BulkCreateResult, error) {
	if len(payloads) == 0 {
		return BulkCreateResult{}, invalid("BULK_EMPTY", "records rỗng — cần ít nhất 1 bản ghi")
	}
	maxRows := BulkMaxRows()
	if len(payloads) > maxRows {
		// NIT (dual-review 2026-09-01): construct directly — no blind type-assert on invalid().
		ve := &ValidationError{Code: "BULK_TOO_LARGE",
			Msg:     fmt.Sprintf("batch có %d bản ghi, vượt quá giới hạn %d bản ghi mỗi lần gọi", len(payloads), maxRows),
			Details: map[string]any{"max": maxRows}}
		return BulkCreateResult{}, ve
	}
	if mode != BulkModeAtomic && mode != BulkModePartial {
		return BulkCreateResult{}, invalid("BULK_INVALID_MODE",
			"mode %q không hợp lệ (chấp nhận atomic hoặc partial)", string(mode))
	}

	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return BulkCreateResult{}, err
	}

	rows := make([]*bulkRowState, len(payloads))
	// queue #62: one explainer per batch, so the read-only set loads once however many rows fail.
	explainLocked := s.requiredLockExplainer(ctx, scope, tenantID, e.Name, specs)
	// specs/054: same create-path visibleWhen rule as Create(); one layout read per batch at most.
	rejudge := s.hiddenRequiredRejudge(ctx, tenantID, e.Name, specs)
	for i, payload := range payloads {
		rows[i] = &bulkRowState{index: i, payload: payload}
		// ENG-SAMPLE-REF (step 0, before ANYTHING else): strip the reserved "$ref" marker key from
		// the payload, in place, so neither the field-ACL check nor ValidateRecord ever sees it (it
		// is not a real field — record_ref_registry.go). Mutating `payload` here (the same map
		// object as `payloads[i]`) means partial mode's later re-submission via Create() (which uses
		// rows[i].payload, not .clean) is ALSO already clean.
		if raw, has := payload["$ref"]; has {
			if refStr, ok := raw.(string); ok {
				rows[i].declaredRef = strings.TrimSpace(refStr)
			} else {
				// Dual-review 2026-09-01 LOW: a non-string "$ref" used to be swallowed silently —
				// the row LOOKED registered, and the failure only surfaced in a LATER batch as
				// RECORD_REF_UNRESOLVED, far from the cause. Fail the offending row here instead.
				rows[i].err = invalidField("RECORD_REF_INVALID", "$ref",
					`"$ref" phải là một chuỗi token (vd "@kh1")`)
			}
			delete(payload, "$ref")
		}
		// Step 1 (spec §3.1): RM-42 field-level write guard, same call/order as Create().
		if s.fieldACL != nil {
			if aerr := s.fieldACL.CheckWriteFields(ctx, tenantID, scope.RoleID, e.Name, scope.IsAdmin, payload); aerr != nil {
				// A hidden key reads exactly like an undeclared one (queue #317): same finish as Create().
				rows[i].err = finishHiddenKeyRefusal(aerr, func(h map[string]struct{}) error {
					_, verr := validateRecord(specs, payload, h)
					return verr
				})
				continue
			}
		}
		// Step 2+3 (spec §3.2/3.3): ValidateRecord ALSO rejects a client-supplied
		// formula/lookup/rollup/auto_number value (computedReadonlyCode) — same single source of
		// truth Create() uses, so this can never drift from the single-record readonly contract.
		clean, verr := ValidateRecord(specs, payload)
		if verr != nil {
			clean, verr = rejudge(payload, verr)
		}
		if verr != nil {
			rows[i].err = explainLocked(verr)
			continue
		}
		rows[i].clean = clean
	}

	// ENG-SAMPLE-REF: resolve every "@ref" relation-field value to the real id it names BEFORE any
	// DB-existence probe runs, so Step 4 below sees an ordinary id — no special-casing downstream,
	// and a stale/wrong resolution is still caught by that SAME tenant-scoped existence check.
	s.resolveBulkRefs(tenantID, specs, rows)

	// Step 4 (spec §3.4): relation-FK validation, batched ONE probe per unique target entity
	// across the WHOLE batch (RM-11 discipline) instead of once per relation-field PER ROW. A
	// store-level error while probing a target is an infrastructure failure (not a per-row data
	// problem) and aborts the WHOLE CreateBulk call — mirrors validateRelations' own bubble-up
	// behaviour for a single Create().
	if err := s.applyBulkRelationValidation(ctx, scope, tenantID, specs, rows); err != nil {
		return BulkCreateResult{}, err
	}

	// ACTIVITY-CORE #3a: same batched tenant-scoped existence probe for polymorphic_relation
	// values across the whole batch (parity with the single-record Create path — a bulk write
	// must not accept a dangling/cross-tenant {type,id} the single POST would reject).
	if err := s.applyBulkPolymorphicValidation(ctx, scope, tenantID, specs, rows); err != nil {
		return BulkCreateResult{}, err
	}

	// USER-FIELD: same batched tenant-scoped ACTIVE-user probe as single-create's
	// validateUserFields. Found by the 2026-09-01 dual-review (HIGH): mode=atomic never ran
	// checkUserTargets, so a dangling/disabled/cross-tenant userId that single POST and
	// mode=partial both reject with 422 RECORD_USER_INVALID would persist — the joint-nobody-
	// wired class, this time between two branches of the SAME function.
	if err := s.applyBulkUserValidation(ctx, tenantID, specs, rows); err != nil {
		return BulkCreateResult{}, err
	}

	// Step 5 (spec §3.5): intra-batch unique / 1-1 pre-check — among rows STILL ok, reject the
	// SECOND (and later) row sharing a unique/1-1 value with an earlier surviving row.
	applyIntraBatchUniqueCheck(specs, rows)

	if mode == BulkModePartial {
		return s.createBulkPartial(ctx, scope, tenantID, userID, e.Name, rows)
	}
	return s.createBulkAtomic(ctx, scope, tenantID, userID, e, specs, rows)
}

// resolveBulkRefs (ENG-SAMPLE-REF) substitutes every "@ref"-token relation-field value with the
// real id it names, for every row still `err == nil` after Phase 1. Mutates BOTH r.clean (what
// mode=atomic persists) and r.payload (what mode=partial re-submits via Create()), so both insert
// paths see an ordinary, already-real value from here on — nothing downstream needs to know "@ref"
// tokens exist. A token that does not resolve (registered by no earlier, separate CreateBulk call)
// sets the row's err to RECORD_REF_UNRESOLVED naming the field and the raw token, and the row is
// excluded from every later phase exactly like any other Phase-1 failure. Only `relation` values
// and the `id` inside a `polymorphic_relation` {type, id} value are inspected (see
// record_ref_registry.go for why that scope is what makes this safe to run on the shared
// bulk-create path unconditionally).
func (s *RecordService) resolveBulkRefs(tenantID string, specs []FieldSpec, rows []*bulkRowState) {
	for _, r := range rows {
		if r.err != nil || r.clean == nil {
			continue
		}
		for _, f := range specs {
			if f.FieldType == "polymorphic_relation" {
				if r.err = s.resolvePolymorphicRef(tenantID, f, r); r.err != nil {
					break
				}
				continue
			}
			if f.FieldType != "relation" {
				continue
			}
			raw, ok := r.clean[f.Name].(string)
			if !ok || !strings.HasPrefix(raw, "@") {
				continue
			}
			entry, found := s.refRegistry.get(tenantID, raw)
			if !found {
				r.err = invalidField("RECORD_REF_UNRESOLVED", f.Name,
					"field %q tham chiếu %q — không tìm thấy record nào đã tạo trước đó với ref này (áp dụng dữ liệu mẫu của entity cha TRƯỚC, rồi mới áp dụng entity con tham chiếu nó)", f.Name, raw)
				break
			}
			// Defense in depth: the token resolved, but to a record of the WRONG entity (e.g. a ref
			// declared by a "Dự án" row referenced from a field that expects "Nhân viên"). Rejecting
			// here with the ACTUAL entity name is clearer than letting it fall through to Step 4's
			// generic "record does not exist" — that record DOES exist, just not as this field's
			// target — and it can never leak cross-entity data (both entity and id came from THIS
			// tenant's own registry entries).
			if target, ok := relatedEntityName(f.Options); ok && target != "" && entry.entity != target {
				r.err = invalidField("RECORD_REF_UNRESOLVED", f.Name,
					"field %q tham chiếu %q — ref này là record của entity %q, không phải %q", f.Name, raw, entry.entity, target)
				break
			}
			r.clean[f.Name] = entry.id
			r.payload[f.Name] = entry.id
		}
	}
}

// resolvePolymorphicRef is resolveBulkRefs for one polymorphic_relation field (queue #140): an
// "@ref" id inside the {type, id} value is swapped for the real id it names, provided the ref was
// registered by a record of that {type} — the polymorphic twin of the relation branch's
// wrong-entity guard (type is the value's own target here, already checked against
// targetEntities by ValidateRecord). No "@" id ⇒ nothing to do. The existence probe
// (applyBulkPolymorphicValidation) still runs afterwards on the resolved id.
func (s *RecordService) resolvePolymorphicRef(tenantID string, f FieldSpec, r *bulkRowState) error {
	m, ok := r.clean[f.Name].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := m["id"].(string)
	if !ok || !strings.HasPrefix(raw, "@") {
		return nil
	}
	typ, _ := m["type"].(string)
	entry, found := s.refRegistry.get(tenantID, raw)
	if !found {
		return invalidField("RECORD_REF_UNRESOLVED", f.Name,
			"field %q tham chiếu %q — không tìm thấy record nào đã tạo trước đó với ref này (áp dụng dữ liệu mẫu của entity được tham chiếu TRƯỚC, rồi mới áp dụng entity tham chiếu nó)", f.Name, raw)
	}
	if entry.entity != typ {
		return invalidField("RECORD_REF_UNRESOLVED", f.Name,
			"field %q tham chiếu %q — ref này là record của entity %q, không phải %q", f.Name, raw, entry.entity, typ)
	}
	resolved := map[string]any{"type": typ, "id": entry.id}
	r.clean[f.Name] = resolved
	r.payload[f.Name] = resolved
	return nil
}

// bulkRelCheck is one (row, field, target-entity, id) relation-FK probe collected while scanning
// every structurally-OK row of the batch.
type bulkRelCheck struct {
	rowIndex   int
	fieldName  string
	targetName string
	id         string
}

// applyBulkRelationValidation enforces the SAME 1-N relation FK rule as validateRelations
// (record_orchestration.go), batched across every row that is still `err == nil` after the
// structural checks: ids are grouped by TARGET ENTITY across the WHOLE batch and
// store.BatchRecordExists is called ONCE per unique target (RM-11), instead of once per
// relation-field PER ROW. A relation value that points nowhere sets that row's err to the
// IDENTICAL RECORD_RELATION_INVALID invalid() call validateRelations uses (same code, same
// message shape — no intra-batch forward-reference: a target must already exist BEFORE the
// batch, per spec §9). Intra-batch relation-value collisions (two rows referencing the SAME live
// target) are NOT an error — only intra-batch UNIQUE/1-1 collisions are (applyIntraBatchUniqueCheck).
func (s *RecordService) applyBulkRelationValidation(ctx context.Context, scope RecordScope, tenantID string, specs []FieldSpec, rows []*bulkRowState) error {
	var checks []bulkRelCheck
	byTarget := make(map[string][]string)

	for _, r := range rows {
		if r.err != nil || r.clean == nil {
			continue
		}
		for _, f := range specs {
			if f.FieldType != "relation" {
				continue
			}
			raw, present := r.clean[f.Name]
			if !present {
				continue
			}
			id, ok := raw.(string)
			if !ok || strings.TrimSpace(id) == "" {
				continue
			}
			target, ok := relatedEntityName(f.Options)
			if !ok {
				r.err = invalid("RECORD_RELATION_INVALID",
					"field relation %q thiếu entity đích để kiểm tra FK", f.Name)
				continue
			}
			targetName, terr := ddl.SanitizeIdentifier(target)
			if terr != nil {
				r.err = invalid("RECORD_RELATION_INVALID",
					"field relation %q có entity đích không hợp lệ", f.Name)
				continue
			}
			checks = append(checks, bulkRelCheck{rowIndex: r.index, fieldName: f.Name, targetName: targetName, id: id})
			byTarget[targetName] = append(byTarget[targetName], id)
		}
	}
	if len(checks) == 0 {
		return nil
	}

	exists, err := probeRelationTargets(ctx, s.store, tenantID, byTarget, s.relationScopeFor(scope))
	if err != nil {
		return fmt.Errorf("services.applyBulkRelationValidation: %w", err)
	}

	byIndex := make(map[int]*bulkRowState, len(rows))
	for _, r := range rows {
		byIndex[r.index] = r
	}
	for _, c := range checks {
		r := byIndex[c.rowIndex]
		if r.err != nil {
			continue // already failed by an earlier relation field on this same row
		}
		if !exists[c.targetName][c.id] {
			r.err = invalid("RECORD_RELATION_INVALID",
				"field relation %q trỏ tới record không tồn tại trong entity %q", c.fieldName, c.targetName)
		}
	}
	return nil
}

// applyBulkPolymorphicValidation is the ACTIVITY-CORE #3a analogue of applyBulkRelationValidation:
// it enforces the SAME tenant-scoped existence probe checkPolymorphicTargets runs on a single
// Create, batched across every still-OK row (ids grouped by target entity across the WHOLE batch,
// ONE store.BatchRecordExists per unique target — RM-11). A {type,id} pointing nowhere (or at
// another tenant, which BatchRecordExists reports absent) sets that row's err to the IDENTICAL
// RECORD_POLYMORPHIC_TARGET_INVALID invalid() call the single-record path uses. `type` membership
// in options.targetEntities was already enforced per-row by ValidateRecord → checkPolymorphic.
func (s *RecordService) applyBulkPolymorphicValidation(ctx context.Context, scope RecordScope, tenantID string, specs []FieldSpec, rows []*bulkRowState) error {
	type bulkPolyCheck struct {
		rowIndex   int
		fieldName  string
		targetName string
		id         string
	}
	var checks []bulkPolyCheck
	byTarget := make(map[string][]string)

	for _, r := range rows {
		if r.err != nil || r.clean == nil {
			continue
		}
		for _, f := range specs {
			if f.FieldType != "polymorphic_relation" {
				continue
			}
			raw, present := r.clean[f.Name]
			if !present {
				continue
			}
			m, ok := raw.(map[string]any)
			if !ok {
				continue // shape validated upstream by checkPolymorphic
			}
			typ, _ := m["type"].(string)
			id, _ := m["id"].(string)
			if strings.TrimSpace(typ) == "" || strings.TrimSpace(id) == "" {
				continue
			}
			targetName, terr := ddl.SanitizeIdentifier(typ)
			if terr != nil {
				r.err = invalidField("RECORD_POLYMORPHIC_TARGET_INVALID", f.Name+".type",
					"field %q có type %q không hợp lệ", f.Name, typ)
				continue
			}
			checks = append(checks, bulkPolyCheck{rowIndex: r.index, fieldName: f.Name, targetName: targetName, id: id})
			byTarget[targetName] = append(byTarget[targetName], id)
		}
	}
	if len(checks) == 0 {
		return nil
	}

	exists, err := probeRelationTargets(ctx, s.store, tenantID, byTarget, s.relationScopeFor(scope))
	if err != nil {
		return fmt.Errorf("services.applyBulkPolymorphicValidation: %w", err)
	}

	byIndex := make(map[int]*bulkRowState, len(rows))
	for _, r := range rows {
		byIndex[r.index] = r
	}
	for _, c := range checks {
		r := byIndex[c.rowIndex]
		if r.err != nil {
			continue // already failed by an earlier field on this same row
		}
		if !exists[c.targetName][c.id] {
			r.err = invalidField("RECORD_POLYMORPHIC_TARGET_INVALID", c.fieldName+".id",
				"field %q trỏ tới record không tồn tại trong entity %q (cùng tenant)", c.fieldName, c.targetName)
		}
	}
	return nil
}

// applyBulkUserValidation is checkUserTargets' bulk twin: ONE tenant-scoped
// BatchActiveUserExists probe for every `user`-field value across all still-ok rows, per-row
// RECORD_USER_INVALID on a miss (parity with validateUserFields on the single-create path —
// added 2026-09-01, dual-review HIGH). A nil userStore fails CLOSED per offending row
// (RECORD_USER_UNAVAILABLE), mirroring checkUserTargets' stance: never accept an
// unverifiable user id. A store error is infrastructure and aborts the whole call.
func (s *RecordService) applyBulkUserValidation(ctx context.Context, tenantID string, specs []FieldSpec, rows []*bulkRowState) error {
	type bulkUserCheck struct {
		rowIndex  int
		fieldName string
		id        string
	}
	var checks []bulkUserCheck
	var ids []string
	for _, r := range rows {
		if r.err != nil || r.clean == nil {
			continue
		}
		for _, f := range specs {
			if f.FieldType != "user" {
				continue
			}
			raw, present := r.clean[f.Name]
			if !present {
				continue
			}
			id, ok := raw.(string)
			if !ok || strings.TrimSpace(id) == "" {
				continue // shape validated upstream by ValidateRecord
			}
			checks = append(checks, bulkUserCheck{rowIndex: r.index, fieldName: f.Name, id: id})
			ids = append(ids, id)
		}
	}
	if len(checks) == 0 {
		return nil
	}

	byIndex := make(map[int]*bulkRowState, len(rows))
	for _, r := range rows {
		byIndex[r.index] = r
	}
	if s.userStore == nil {
		for _, c := range checks {
			if r := byIndex[c.rowIndex]; r.err == nil {
				r.err = invalid("RECORD_USER_UNAVAILABLE",
					"kiểm tra field kiểu user hiện không khả dụng — vui lòng thử lại sau")
			}
		}
		return nil
	}
	found, err := s.userStore.BatchActiveUserExists(ctx, tenantID, ids)
	if err != nil {
		return fmt.Errorf("services.applyBulkUserValidation: batch probe: %w", err)
	}
	for _, c := range checks {
		r := byIndex[c.rowIndex]
		if r.err != nil {
			continue // already failed by an earlier field on this same row
		}
		if !found[c.id] {
			r.err = invalidField("RECORD_USER_INVALID", c.fieldName,
				"field %q phải là id của một người dùng đang hoạt động trong tổ chức", c.fieldName)
		}
	}
	return nil
}

// applyIntraBatchUniqueCheck rejects the SECOND (and later) row within the batch that shares a
// unique-field or 1-1-relation value with an EARLIER, still-OK row (spec §3.5) — caught BEFORE
// any DB round-trip, so the client gets a clean RECORD_DUPLICATE / RECORD_RELATION_ALREADY_BOUND
// instead of a confusing mid-batch constraint failure. Only rows still `err == nil` participate:
// a row already marked failed never "claims" a value (it will never be inserted), and a row that
// LOSES this check is likewise excluded from claiming for a THIRD row behind it — only genuinely
// surviving rows build the `seen` set. The DB's own tenant-scoped unique index (utf8mb4_bin,
// byte-exact — see record_service.go comment on caseInsensitiveTypes) remains the authoritative
// backstop for anything this pre-check cannot see (a value already in the table, or a race with a
// concurrent request); the comparison here is therefore BYTE-EXACT (no case-folding) so it never
// disagrees with what the DB would actually accept/reject.
func applyIntraBatchUniqueCheck(specs []FieldSpec, rows []*bulkRowState) {
	type candidate struct {
		field string
		code  string
	}
	var candidates []candidate
	for _, f := range specs {
		if f.IsUnique {
			candidates = append(candidates, candidate{field: f.Name, code: "RECORD_DUPLICATE"})
		} else if f.FieldType == "relation" && relationCardinality(f.Options) == "1-1" {
			candidates = append(candidates, candidate{field: f.Name, code: "RECORD_RELATION_ALREADY_BOUND"})
		}
	}
	if len(candidates) == 0 {
		return
	}

	seen := make(map[string]map[string]struct{}, len(candidates))
	for _, c := range candidates {
		seen[c.field] = make(map[string]struct{})
	}

	for _, r := range rows {
		if r.err != nil || r.clean == nil {
			continue
		}
		var dupErr error
		for _, c := range candidates {
			val, present := r.clean[c.field]
			if !present || isEmpty(val) {
				continue // NULL-ish values never collide — mirrors MySQL unique-index NULL semantics
			}
			key, kerr := dedupKey(val)
			if kerr != nil {
				continue // unencodable value — let normal validation/DB surface any real issue
			}
			if _, dup := seen[c.field][key]; dup {
				if c.code == "RECORD_RELATION_ALREADY_BOUND" {
					dupErr = invalidField(c.code, c.field,
						"quan hệ 1-1 %q trùng với một dòng khác trong cùng batch", c.field)
				} else {
					dupErr = invalidField(c.code, c.field,
						"field %q trùng giá trị với một dòng khác trong cùng batch", c.field)
				}
				break
			}
		}
		if dupErr != nil {
			r.err = dupErr
			continue
		}
		// This row survives — claim its candidate values so a LATER row cannot reuse them.
		for _, c := range candidates {
			val, present := r.clean[c.field]
			if !present || isEmpty(val) {
				continue
			}
			if key, kerr := dedupKey(val); kerr == nil {
				seen[c.field][key] = struct{}{}
			}
		}
	}
}

// dedupKey renders a clean field value into a stable, comparable string key for the intra-batch
// dedup map. Scalars (string/float64/bool) use fmt.Sprint directly — string values pass through
// UNCHANGED (byte-exact, no case-folding). Nested JSON (e.g. a currency {amount,currencyCode}
// map, in case a currency field is ever marked unique) uses json.Marshal, whose Go encoder sorts
// object keys deterministically, so two structurally-equal values always produce the same key
// even though Go maps are not directly comparable.
func dedupKey(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case float64, bool:
		return fmt.Sprint(t), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// createBulkAtomic runs Phase 2 (assign+insert) for mode=atomic: if ANY row failed Phase 1, the
// whole batch is aborted with ZERO writes and ZERO auto_number claims (spec §3, §6). Only when
// every row is Phase-1-clean does it claim sequences / compute formulas / call the one-shot
// InsertRecordsAtomic transaction.
//
// Infra-vs-validation error classification (matches single Create()'s contract): a *ValidationError
// (duplicate/data-constraint/config-unavailable) is a per-row business outcome — it aborts the
// batch through the normal abortAtomicBulk envelope (422). Anything else — a DB timeout, a
// dropped connection, an unexpected driver error — is NOT a data problem; it is returned as a
// TOP-LEVEL Go error so the handler's respondError can classify it exactly like a single
// Create() would (503 DB_TIMEOUT via db.IsTimeout, or a generic 500) INSTEAD OF the misleading
// 422 "your data is invalid" an embedded per-row code would imply. Neither assignSequences nor
// InsertRecordsAtomic can have written a partial row by the time either fails (the claim runs
// before any insert; the insert is one all-or-nothing transaction), so there is nothing lost by
// bubbling — no row from THIS call was ever persisted.
func (s *RecordService) createBulkAtomic(ctx context.Context, scope RecordScope, tenantID, userID string, e *metadata.EntityDefinition, specs []FieldSpec, rows []*bulkRowState) (BulkCreateResult, error) {
	for _, r := range rows {
		if r.err != nil {
			return abortAtomicBulk(rows), nil
		}
	}

	// Fail-closed BEFORE the claim loop (review LOW-3): if the atomic inserter was never wired
	// (server misconfig — main.go always calls WithBulkInserter, so this is defensive), abort
	// now so a misconfigured server never burns auto_number sequence values it can't insert.
	if s.bulkStore == nil {
		rows[0].err = invalid("BULK_ATOMIC_UNAVAILABLE",
			"atomic bulk-insert chưa được cấu hình trên server")
		return abortAtomicBulk(rows), nil
	}

	// specs/048 S2/S5: the before_save rules run on every row — rules loaded once for the batch —
	// BEFORE any auto_number claim: the value-changing rules rewrite r.clean, then one refused row
	// refuses the batch. Hooked here, not in the shared Phase 1: mode=partial re-enters Create(),
	// which runs the rules on each row itself. Unreadable rules fail the call (503) with nothing
	// claimed or written. S3: one summary run per rule for the batch (flush, when the call returns),
	// the value-changing ones only when the batch was written (stored).
	judge, flush, err := s.createBatchHook(ctx, scope, tenantID, e.Name, specs)
	if err != nil {
		return BulkCreateResult{}, fmt.Errorf("services.createBulkAtomic: %w", err)
	}
	var outs, stored [][]workflow.MutationOutcome
	if judge != nil {
		defer func() { flush(stored) }()
		outs = make([][]workflow.MutationOutcome, len(rows))
		refused := false
		for i, r := range rows {
			if outs[i], r.err = judge(r.clean); r.err != nil {
				refused = true
			}
		}
		// A rewritten value may now equal another row's on a unique field ("An@x.vn" and "an@x.vn"
		// once lower-cased): judge the batch's own duplicates again, on what will be stored.
		applyIntraBatchUniqueCheck(specs, rows)
		for _, r := range rows {
			refused = refused || r.err != nil
		}
		if refused {
			return abortAtomicBulk(rows), nil
		}
	}

	// Phase 2 (spec §3, Pha 2): assign auto_number + compute formulas for every OK row. The claim
	// happens OUTSIDE any DB transaction (matches assignSequences' documented gaps-allowed
	// contract) — if InsertRecordsAtomic subsequently rolls back, a claimed number is burned, but
	// NO number is claimed at all unless every row already passed Phase 1.
	cleanRows := make([]map[string]any, len(rows))
	for i, r := range rows {
		if err := s.assignSequences(ctx, tenantID, e.Name, specs, r.clean); err != nil {
			var ve *ValidationError
			if errors.As(err, &ve) {
				r.err = err
				return abortAtomicBulk(rows), nil
			}
			return BulkCreateResult{}, fmt.Errorf("services.createBulkAtomic: assignSequences row %d: %w", i, err)
		}
		applyComputedFields(specs, r.clean)
		seedStoredCounts(specs, r.clean) // specs/083: a new parent starts at 0
		cleanRows[i] = r.clean
	}

	// Two concurrent atomic batches inserting the same unique key can make InnoDB pick one as a
	// deadlock victim (MySQL 1213) instead of a clean 1062 duplicate. 1213 means MySQL ALREADY
	// rolled the whole transaction back and its own guidance is "try restarting transaction" —
	// so re-run the one-shot insert (rows are pre-validated; sequences were claimed before this
	// loop, so a retry burns nothing). After the competing batch commits, the retry resolves
	// deterministically: either it inserts fine or it hits 1062, which maps to the normal
	// per-row RECORD_DUPLICATE abort below. Without this, the caller would get a raw 500 for a
	// transient serialization artifact.
	var (
		ids       []string
		failedIdx int
		ierr      error
	)
	for attempt := 0; ; attempt++ {
		ids, failedIdx, ierr = s.bulkStore.InsertRecordsAtomic(ctx, e.Name, tenantID, userID, cleanRows, uniqueFieldSpecs(specs))
		if ierr == nil || !isMySQLDeadlock(ierr) || attempt >= bulkDeadlockRetries {
			break
		}
		select {
		case <-ctx.Done():
			return BulkCreateResult{}, fmt.Errorf("services.createBulkAtomic: insert retry: %w", ctx.Err())
		case <-time.After(time.Duration(attempt+1) * 25 * time.Millisecond):
		}
	}
	if ierr != nil {
		mapped := asDataConstraint(asDuplicate(ierr, e.Name, specs))
		var ve *ValidationError
		if !errors.As(mapped, &ve) {
			return BulkCreateResult{}, fmt.Errorf("services.createBulkAtomic: insert: %w", mapped)
		}
		if failedIdx >= 0 && failedIdx < len(rows) {
			rows[failedIdx].err = mapped
		} else {
			rows[0].err = mapped
		}
		return abortAtomicBulk(rows), nil
	}

	stored = outs // the batch is written: the value-changing rules' summary runs count its rows
	result := BulkCreateResult{Mode: string(BulkModeAtomic), Created: len(rows), Results: make([]BulkRowResult, 0, len(rows))}
	images := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		images = append(images, r.clean)
	}
	s.refreshStoredRollups(ctx, tenantID, e.Name, images...) // specs/083: once per batch, before the automations fire
	for i, r := range rows {
		id := ids[i]
		result.Results = append(result.Results, BulkRowResult{Index: r.index, Status: "created", ID: id})
		s.emitCreateEvents(ctx, tenantID, userID, e.Name, id, r.clean, specs)
		// ENG-SAMPLE-REF: only NOW — after the row is actually, atomically committed — register its
		// declared ref (if any) so a LATER, separate CreateBulk call can resolve "@<ref>" to this id.
		if r.declaredRef != "" {
			s.refRegistry.put(tenantID, r.declaredRef, e.Name, id)
		}
	}
	return result, nil
}

// abortAtomicBulk builds the atomic-abort envelope (spec §3): every row that actually failed
// carries its real code/error/details; every OTHER row (one that itself passed every check) is
// marked "skipped"/BULK_ATOMIC_ABORTED so the client can tell "not your row's fault" apart from
// a genuine validation error. created is always 0 (nothing was ever written).
func abortAtomicBulk(rows []*bulkRowState) BulkCreateResult {
	result := BulkCreateResult{Mode: string(BulkModeAtomic), Created: 0, Results: make([]BulkRowResult, 0, len(rows))}
	for _, r := range rows {
		if r.err != nil {
			code, msg, details := errParts(r.err)
			result.Results = append(result.Results, BulkRowResult{Index: r.index, Status: "failed", Code: code, Error: msg, Details: details})
			result.Failed++
			continue
		}
		result.Results = append(result.Results, BulkRowResult{
			Index:  r.index,
			Status: "skipped",
			Code:   "BULK_ATOMIC_ABORTED",
			Error:  "batch bị huỷ vì một dòng khác trong cùng batch lỗi (atomic mode)",
		})
		result.Skipped++
	}
	return result
}

// createBulkPartial runs Phase 2 for mode=partial: every row that survived Phase 1 is inserted
// via Create() ITSELF — the full single-record path (write-guard, validate, relation-check,
// auto_number claim, formula compute, insert, audit/workflow/approval emit) — so a partial-mode
// row is byte-for-byte what a standalone POST would have produced. A row is independent: one
// failing does not affect its siblings.
//
// Cost note (RM-11): Create() independently re-runs its OWN single-row relation-FK probe for
// every surviving row, on top of applyBulkRelationValidation's one batched Phase-1 pre-check.
// This is bounded and intentional, not a regression — mode=partial's Phase 2 cost is, by
// definition, IDENTICAL to the caller sending N separate POST /:entity requests (that is what
// "partial" means), PLUS Phase-1's cheap early-reject that filters bad rows out BEFORE any of
// those N calls run. mode=atomic does NOT pay this cost: its Phase 2 goes straight from
// Phase-1's already-validated `clean` data to insert, so atomic mode gets the full RM-11
// single-batched-probe benefit with zero re-validation (see createBulkAtomic).
//
// Infra-vs-validation error classification (mirrors createBulkAtomic / matches single Create()'s
// contract): when Create() fails with a *ValidationError (duplicate/relation/forbidden/…), that
// row is reported "failed" with its real code and the loop continues to the next row. When
// Create() fails with anything else (a DB timeout, a dropped connection), the error is NOT a
// data problem — it is returned as a TOP-LEVEL Go error so respondError classifies it (503
// DB_TIMEOUT / 500) instead of masking it behind a misleading per-row 422-shaped code. Rows
// already appended to `result` before the infra failure are NOT lost data — each was its own
// independently committed Create() call — the client simply does not see them in THIS response
// and can discover them via a normal GET/List, exactly as if those had been separate requests
// that succeeded before a later one timed out.
func (s *RecordService) createBulkPartial(ctx context.Context, scope RecordScope, tenantID, userID, entityName string, rows []*bulkRowState) (BulkCreateResult, error) {
	result := BulkCreateResult{Mode: string(BulkModePartial), Results: make([]BulkRowResult, 0, len(rows))}
	for _, r := range rows {
		if r.err != nil {
			code, msg, details := errParts(r.err)
			result.Results = append(result.Results, BulkRowResult{Index: r.index, Status: "failed", Code: code, Error: msg, Details: details})
			result.Failed++
			continue
		}
		id, err := s.Create(ctx, scope, tenantID, userID, entityName, r.payload)
		if err != nil {
			var ve *ValidationError
			if !errors.As(err, &ve) {
				// The rows before this one ARE committed: hand back their tally with the error, so an
				// Idempotency-Key guard (CreateBulkIdempotent) knows the batch was not a no-op.
				return result, fmt.Errorf("services.createBulkPartial: row %d: %w", r.index, err)
			}
			code, msg, details := errParts(err)
			result.Results = append(result.Results, BulkRowResult{Index: r.index, Status: "failed", Code: code, Error: msg, Details: details})
			result.Failed++
			continue
		}
		result.Results = append(result.Results, BulkRowResult{Index: r.index, Status: "created", ID: id})
		result.Created++
		// ENG-SAMPLE-REF: only NOW — after Create() actually committed the row — register its
		// declared ref (if any) so a LATER, separate CreateBulk call can resolve "@<ref>" to this id.
		if r.declaredRef != "" {
			s.refRegistry.put(tenantID, r.declaredRef, entityName, id)
		}
	}
	return result, nil
}

// errParts extracts a per-row (code, message, details) triple from an error for the bulk-create
// envelope. A *ValidationError (every error every step in this file can produce) surfaces its
// real code/message/details verbatim. Anything else (an infrastructure error that slipped past
// the checks above) is NEVER echoed verbatim (B12: never leak a raw DB/driver error to the
// client) — it collapses to the SAME generic fallback respondError uses for an unmapped error.
func errParts(err error) (code, msg string, details map[string]any) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Code, ve.Msg, ve.Details
	}
	return "RECORD_OP_FAILED", "Lỗi xử lý bản ghi", nil
}
