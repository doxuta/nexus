package dynrepo

// Record purge — the bottom of the recycle bin (specs/056-record-hard-delete).
//
// Soft delete (SoftDeleteRecord) sets deleted_at and frees the record's unique slots; nothing ever
// removed the row, so the bin had no bottom: rows and their attachment catalog rows piled up in
// obj_* / sys_files forever. PurgeRecord is the ONE hard DELETE of a single obj_* row in this
// package, and it is deliberately narrow:
//
//   - only a record ALREADY in the bin (deleted_at IS NOT NULL) — a live one is ErrRecordNotInBin,
//     so nothing live is ever destroyed in one call (two steps, like retire → purge of a tenant);
//   - every statement pins tenant_id; values travel as `?`; table names come only from
//     ddl.SanitizeIdentifier (tableFor / entityNameForPivot);
//   - one transaction; a dry run runs the SAME transaction and rolls it back, so the preview an
//     operator reads is the exact count the real call removes — there is no second counting path
//     that could drift from the deleting one.
//
// Blobs are NOT touched here (no storage in a repository): the caller gets the sys_files ids and
// deletes the bytes after commit.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// ErrRecordNotInBin is returned by PurgeRecord for a record that exists in the tenant but is still
// live: only a record already in the recycle bin may be purged (specs/056 D2).
var ErrRecordNotInBin = errors.New("record is not in the recycle bin")

// PurgeParams selects the one binned record to purge.
type PurgeParams struct {
	Entity   string
	TenantID string
	ID       string
	// DryRun runs the whole transaction and rolls it back: the result reports what WOULD go.
	DryRun bool
	// ActorID stamps modified_by on a live record whose optional reference the purge clears.
	ActorID string
	// Graph is the dependents map the service resolved from metadata (sys_relationships +
	// sys_fields), keyed by entity: every entity a record may be purged FROM must have an entry
	// (the root and each required child's entity). A missing entry = no dependents.
	Graph map[string]PurgeEntityDeps
	// MaxRecords caps records purged + references cleared; beyond it the whole purge is refused
	// (*PurgeTooLargeError), never truncated. <= 0 ⇒ DefaultPurgeMaxRecords.
	MaxRecords int
	// SampleRoot (specs/064 S9-7) switches to the sample-data purge: the root may be live OR in the
	// bin but MUST carry a sample mark (else ErrRecordNotFound); a required child follows only when
	// it is marked too (live or binned); everything UNMARKED that depends on the cluster — a required
	// child, an optional/polymorphic reference, an N-N link — blocks it (*PurgeBlockedError) and is
	// never deleted or rewritten. Optional references held by MARKED rows are cleared as in 056.
	SampleRoot bool
	// Pinned (SampleRoot) are records a live relation field names as its default value, keyed by
	// PinKey(entity, id): the root or a followed child in it blocks the cluster (Kind field_default).
	Pinned map[string]PurgePin
}

// PurgePin names the relation field whose default value is a record: Entity.Field.
type PurgePin struct{ Entity, Field string }

// PinKey is the PurgeParams.Pinned key of one record.
func PinKey(entity, id string) string { return entity + "/" + id }

// BlockerFieldDefault is PurgeBlocker.Kind of a record pinned as a field's default value.
const BlockerFieldDefault = "field_default"

// DefaultPurgeMaxRecords bounds one purge transaction: records deleted + references cleared, and
// through probeLimit the rows each child probe loads (see children).
// ponytail: one transaction for the whole cascade (atomic), no batch path. A record with more than
// 500 optional references (a customer with 600 activities) cannot be purged — the refusal is 422
// RECORD_PURGE_TOO_LARGE, never a partial purge (specs/056 §7). Upgrade path when it bites: clear
// those references in batches of their own transactions first, then purge.
const DefaultPurgeMaxRecords = 500

// PurgeChildRef is one relation field on Entity that points at the purged entity (1-N or 1-1,
// FK-on-child: the value lives in data.<Field>, projected to the `<Field>_id` generated column).
type PurgeChildRef struct {
	Entity string
	Field  string
	// Required: the child cannot exist without its parent ⇒ it goes WITH the parent — but only if
	// it is itself in the bin; a live one blocks the purge. Optional ⇒ the reference is cleared.
	Required bool
	// OneToOne: the `<Field>_id` column is CASE-guarded (a binned row projects NULL), so binned
	// children are matched on the JSON value instead.
	OneToOne bool
	// Polymorphic: a polymorphic_relation field — the value is {type, id}, projected to
	// `<Field>_type` + `<Field>_id`; a child points at the purged record only when BOTH match.
	Polymorphic bool
}

// PurgeEntityDeps are the dependents of records of one entity.
type PurgeEntityDeps struct {
	Children []PurgeChildRef
	// JoinPeers are the N-N partner entities; link rows live in rel_<a>_<b> (ddl.JoinParts).
	JoinPeers []string
}

// ClearedRef is one optional reference to a purged record that the purge set to empty.
type ClearedRef struct {
	Entity string
	ID     string
	Field  string
	Was    string // the purged record's id the field held
}

// PurgeBlocker counts LIVE records that require a record being purged (Entity.Field).
type PurgeBlocker struct {
	Entity string `json:"entity"`
	Field  string `json:"field"`
	Count  int    `json:"count"`
	// Kind is empty for a dependent row; BlockerFieldDefault for a record a field defaults to.
	Kind string `json:"kind,omitempty"`
}

// PurgeBlockedError: live records require a record in the purge; nothing was deleted.
type PurgeBlockedError struct{ Blocked []PurgeBlocker }

func (e *PurgeBlockedError) Error() string {
	return fmt.Sprintf("purge blocked by %d live dependent group(s)", len(e.Blocked))
}

// PurgeTooLargeError: the cascade exceeds Max records; nothing was deleted.
type PurgeTooLargeError struct{ Max int }

func (e *PurgeTooLargeError) Error() string {
	return fmt.Sprintf("purge touches more than %d records", e.Max)
}

// PurgedRecord is one obj_* row a purge removed, with its data as it was (the audit before-image).
type PurgedRecord struct {
	Entity string
	ID     string
	Data   map[string]any
}

// PurgeTableCount is one table's contribution to a purge — reported per table because "12 rows"
// is not reviewable but "obj_crm_accounts 1, sys_files 2" is.
type PurgeTableCount struct {
	Table string `json:"table"`
	Rows  int64  `json:"rows"`
}

// PurgeResult is what a purge removed (or, on a dry run, would remove).
type PurgeResult struct {
	Records []PurgedRecord
	// Cleared are the optional references the purge emptied (records that stay).
	Cleared []ClearedRef
	// Blocked is filled on a dry run only (a real purge returns *PurgeBlockedError instead).
	Blocked []PurgeBlocker
	// FileIDs are the sys_files rows removed with the records; the caller deletes their blobs
	// (files.StorageKey(tenant, id)) after commit.
	FileIDs []string
	Tables  []PurgeTableCount
}

// count adds n rows to table in first-seen order (deterministic report, zero rows omitted).
func (res *PurgeResult) count(table string, n int64) {
	if n <= 0 {
		return
	}
	for i := range res.Tables {
		if res.Tables[i].Table == table {
			res.Tables[i].Rows += n
			return
		}
	}
	res.Tables = append(res.Tables, PurgeTableCount{Table: table, Rows: n})
}

// PurgeRecord permanently removes ONE record that is already in the tenant's recycle bin, and
// everything keyed by it, in ONE transaction (specs/056 US1 + US2):
//
//   - its attachment catalog rows (sys_files, live and tombstoned — the caller deletes the bytes),
//     any pivot row still naming it (soft delete released them; expected 0), its notes,
//     notifications, manual shares, approval instances + BPM seats, workflow run history, and its
//     N-N link rows (p.Graph[entity].JoinPeers);
//   - its REQUIRED children (p.Graph[entity].Children, Required) go with it — recursively — but only
//     when they are in the bin themselves: a LIVE required child is never destroyed by emptying the
//     bin; it blocks the purge (dry run: Blocked; real: *PurgeBlockedError, nothing deleted);
//   - an OPTIONAL reference to it is cleared the way a partial PUT clears a field (key removed,
//     version+1, modified_by = p.ActorID), live or binned — the record that holds it stays;
//   - more than MaxRecords purged + cleared ⇒ *PurgeTooLargeError, nothing deleted.
//
// The audit log is NOT touched here (append-only; the service appends hard_delete rows after commit).
//
// Errors: ErrRecordNotFound (no such id in this tenant, incl. another tenant's id), ErrRecordNotInBin
// (the record is live), ErrWriteConflict (deadlock / lock-wait — MySQL rolled back, safe to retry).
func (r *Repository) PurgeRecord(ctx context.Context, p PurgeParams) (*PurgeResult, error) {
	table, err := tableFor(p.Entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.PurgeRecord: %w", err)
	}
	entityName, err := entityNameForPivot(p.Entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.PurgeRecord: %w", err)
	}
	if p.MaxRecords <= 0 {
		p.MaxRecords = DefaultPurgeMaxRecords
	}

	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.PurgeRecord: begin tx: %w", err)
	}
	// A dry run never reaches Commit, so this Rollback IS the dry run's undo.
	defer func() { _ = tx.Rollback() }()

	// Lock the root IN THE BIN (SampleRoot: live or binned, but marked). FOR UPDATE serializes two
	// concurrent purges (the loser finds 0 rows → not-found) and a concurrent restore (which then
	// waits, and finds the row gone).
	var raw []byte
	if p.SampleRoot {
		// The mark is checked HERE, in the deleting transaction (FR-032): an unmarked row is not-found.
		err = tx.GetContext(ctx, &raw, fmt.Sprintf(
			"SELECT `data` FROM `%s` WHERE `tenant_id` = ? AND `id` = ? AND %s FOR UPDATE", table, sampleMarkPredicate(table, true)),
			p.TenantID, p.ID, p.TenantID, entityName)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRecordNotFound
		}
	} else {
		err = tx.GetContext(ctx, &raw, fmt.Sprintf(
			"SELECT `data` FROM `%s` WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NOT NULL FOR UPDATE",
			table), p.TenantID, p.ID)
		if errors.Is(err, sql.ErrNoRows) {
			var live int
			if gerr := tx.GetContext(ctx, &live, fmt.Sprintf(
				"SELECT COUNT(*) FROM `%s` WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NULL",
				table), p.TenantID, p.ID); gerr != nil {
				return nil, purgeErr("probe live", gerr)
			}
			if live > 0 {
				return nil, ErrRecordNotInBin
			}
			return nil, ErrRecordNotFound
		}
	}
	if err != nil {
		return nil, purgeErr("lock record", err)
	}
	data, err := decodeData(raw)
	if err != nil {
		return nil, err
	}

	w := &purgeWalk{ctx: ctx, tx: tx, p: p, res: &PurgeResult{},
		purged: map[string]bool{entityName + "/" + p.ID: true}, blocked: map[[3]string]int{}}
	w.res.Records = []PurgedRecord{{Entity: entityName, ID: p.ID, Data: data}}
	// Breadth-first over the purge set: Records grows while we walk it (required children join the
	// queue), so index, don't range.
	for i := 0; i < len(w.res.Records); i++ {
		rec := w.res.Records[i]
		if pin, ok := p.Pinned[PinKey(rec.Entity, rec.ID)]; ok {
			w.blocked[[3]string{pin.Entity, pin.Field, BlockerFieldDefault}]++
		}
		if err := w.children(rec); err != nil {
			return nil, err
		}
		if err := w.purgeOne(rec); err != nil {
			return nil, err
		}
	}
	// A reference cleared on a record that the walk later purged is not "cleared" — it is gone.
	kept := w.res.Cleared[:0]
	for _, c := range w.res.Cleared {
		if !w.purged[c.Entity+"/"+c.ID] {
			kept = append(kept, c)
		}
	}
	w.res.Cleared = kept
	for k, n := range w.blocked {
		w.res.Blocked = append(w.res.Blocked, PurgeBlocker{Entity: k[0], Field: k[1], Kind: k[2], Count: n})
	}
	sort.Slice(w.res.Blocked, func(i, j int) bool {
		a, b := w.res.Blocked[i], w.res.Blocked[j]
		return a.Entity+"."+a.Field+"."+a.Kind < b.Entity+"."+b.Field+"."+b.Kind
	})

	if p.DryRun {
		return w.res, nil // deferred Rollback undoes every statement above
	}
	if len(w.res.Blocked) > 0 {
		return nil, &PurgeBlockedError{Blocked: w.res.Blocked}
	}
	if err := tx.Commit(); err != nil {
		return nil, purgeErr("commit", err)
	}
	return w.res, nil
}

// purgeWalk is the state of one purge transaction's walk over the dependents graph.
type purgeWalk struct {
	ctx     context.Context
	tx      *sqlx.Tx
	p       PurgeParams
	res     *PurgeResult
	purged  map[string]bool   // "entity/id" in the purge set
	blocked map[[3]string]int // {entity, field, kind} → dependents that block the purge
}

// probeLimit bounds one child probe — the rows it locks and loads. The only rows a probe returns
// that do not count toward the cap are ones already in the purge set (at most len(Records)), so a
// probe that comes back FULL carries more new rows than the cap has room for (the walk refuses it)
// and one that comes back short saw every row.
// ponytail: loose by len(Records) (exact = remaining+1+already-purged rows of that entity); ≤ cap+1
// rows either way.
func (w *purgeWalk) probeLimit() int {
	return w.p.MaxRecords - len(w.res.Cleared) + 1
}

// purgeChildRow is one record whose relation field points at a record being purged (Data only for a
// row the purge deletes).
type purgeChildRow struct {
	ID   string `db:"id"`
	Data []byte `db:"data"`
}

// probe locks (FOR UPDATE) and returns at most probeLimit rows of table matching cond, tenant-pinned.
// cond and cols are built from sanitized identifiers only; every value travels in args.
func (w *purgeWalk) probe(step, table, cols, cond string, args ...any) ([]purgeChildRow, error) {
	var rows []purgeChildRow
	args = append(append([]any{w.p.TenantID}, args...), w.probeLimit())
	if err := w.tx.SelectContext(w.ctx, &rows, fmt.Sprintf(
		"SELECT %s FROM `%s` WHERE `tenant_id` = ? AND %s LIMIT ? FOR UPDATE", cols, table, cond), args...); err != nil {
		return nil, purgeErr(step, err)
	}
	return rows, nil
}

// children locks every record whose relation field (per the graph) points at rec, then queues the
// required ones for purging (binned) or counts them as blockers (live), and clears the optional ones.
//
// The cap bounds what this READS, not only what it deletes: a live required child is counted and
// never loaded (the purge cannot take it), every other probe is LIMITed by probeLimit, `data` is
// fetched only for rows the purge deletes, and the cap is checked BEFORE the first write.
// Locks: InnoDB (REPEATABLE READ) X-locks every row a locking range scan reads, and deleted_at is not
// in the (tenant_id, <f>_id) index — so the live rows in the reference's range are locked for the
// rest of this (cap-bounded) transaction. That lock is also what keeps a new live child out of the
// range while a purge that found none commits.
func (w *purgeWalk) children(rec PurgedRecord) error {
	for _, c := range w.p.Graph[rec.Entity].Children {
		table, err := tableFor(c.Entity)
		if err != nil {
			return fmt.Errorf("dynrepo.PurgeRecord: child entity: %w", err)
		}
		field, err := ddl.SanitizeIdentifier(c.Field)
		if err != nil {
			return fmt.Errorf("dynrepo.PurgeRecord: child field: %w", err)
		}
		childEntity := table[len("obj_"):]
		// The FK generated column names rec on every 1-N row (live and binned). A polymorphic value
		// is {type, id} → `<f>_type`/`<f>_id` (plain STORED, index (tenant_id, type, id)): the id alone
		// is not enough — the same value under another type names another record. A 1-1 column is
		// CASE-guarded against deleted_at (a binned row projects NULL so it does not squat the unique
		// slot): binned 1-1 rows are matched on the JSON value, over the tenant's binned rows only.
		match, args := fmt.Sprintf("`%s_id` = ?", field), []any{rec.ID}
		if c.Polymorphic {
			match, args = fmt.Sprintf("`%s_type` = ? AND `%s_id` = ?", field, field), []any{rec.Entity, rec.ID}
		}
		binnedMatch := "`deleted_at` IS NOT NULL AND " + match
		if c.OneToOne {
			binnedMatch = fmt.Sprintf("`deleted_at` IS NOT NULL AND JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s')) = ?", field)
		}
		var rows []purgeChildRow
		if w.p.SampleRoot {
			if rows, err = w.sampleChildRows(c, table, childEntity, field, match, binnedMatch, args); err != nil {
				return err
			}
		} else if c.Required {
			// A LIVE required child is never destroyed by emptying the bin — it blocks. Counted, not
			// loaded: 100k live children cost one integer, not their JSON.
			var live int
			if err := w.tx.GetContext(w.ctx, &live, fmt.Sprintf(
				"SELECT COUNT(*) FROM `%s` WHERE `tenant_id` = ? AND `deleted_at` IS NULL AND %s FOR UPDATE", table, match),
				append([]any{w.p.TenantID}, args...)...); err != nil {
				return purgeErr("lock live children", err)
			}
			if live > 0 {
				w.blocked[[3]string{childEntity, field, ""}] += live
			}
			// Binned required children go WITH rec: their data is the audit before-image.
			if rows, err = w.probe("lock binned children", table, "`id`, `data`", binnedMatch, args...); err != nil {
				return err
			}
		} else {
			// Optional: cleared whether live or binned — ids only. A 1-1 gencol names live rows only.
			if rows, err = w.probe("lock references", table, "`id`", match, args...); err != nil {
				return err
			}
			if c.OneToOne {
				binned, err := w.probe("lock binned 1-1 references", table, "`id`", binnedMatch, args...)
				if err != nil {
					return err
				}
				rows = append(rows, binned...)
			}
		}
		fresh := rows[:0]
		for _, row := range rows {
			if !w.purged[childEntity+"/"+row.ID] { // already going: a self-reference, or another path
				fresh = append(fresh, row)
			}
		}
		// Refuse before the first write (a full probe always lands here — see probeLimit).
		if len(w.res.Records)+len(w.res.Cleared)+len(fresh) > w.p.MaxRecords {
			return &PurgeTooLargeError{Max: w.p.MaxRecords}
		}
		for _, row := range fresh {
			if !c.Required {
				if err := w.clear(table, childEntity, field, row.ID, rec.ID); err != nil {
					return err
				}
				continue
			}
			data, err := decodeData(row.Data)
			if err != nil {
				return err
			}
			w.purged[childEntity+"/"+row.ID] = true
			w.res.Records = append(w.res.Records, PurgedRecord{Entity: childEntity, ID: row.ID, Data: data})
		}
	}
	return nil
}

// clear empties one optional reference exactly like a partial PUT with {field: null} would (the
// JSON key is removed; the generated column projects NULL), bumping version and stamping the actor.
func (w *purgeWalk) clear(table, entity, field, id, was string) error {
	patch, err := json.Marshal(map[string]any{field: nil})
	if err != nil {
		return fmt.Errorf("dynrepo.PurgeRecord: clear patch: %w", err)
	}
	if _, err := w.tx.ExecContext(w.ctx, fmt.Sprintf(
		"UPDATE `%s` SET `data` = JSON_MERGE_PATCH(COALESCE(`data`, JSON_OBJECT()), ?), `version` = `version` + 1, `modified_by` = ? "+
			"WHERE `tenant_id` = ? AND `id` = ?", table), patch, w.p.ActorID, w.p.TenantID, id); err != nil {
		return purgeErr("clear reference", err)
	}
	w.res.Cleared = append(w.res.Cleared, ClearedRef{Entity: entity, ID: id, Field: field, Was: was})
	return nil
}

// recordKeyedTables hold rows keyed by (tenant_id, entity_name, record_id) that exist only for their
// record — each is served by an index leading with those three columns.
var recordKeyedTables = []string{
	"sys_files",
	// Soft delete released these (SoftDeleteRecord → deletePivotByRecord); a leftover row would hold
	// a unique value hostage forever once the record is gone, so sweep it anyway.
	"sys_unique_index",
	"sys_record_notes",
	"sys_notifications",
	"sys_record_shares",
	"sys_approval_instances",
	"sys_bpm_tasks",
	// specs/066: the email log (idx_email_log_record) — a hard-deleted record leaves no log row naming it.
	"sys_email_log",
	// specs/064 S9-7: the sample mark goes with its row, so a hard delete leaves no orphan mark.
	"sys_sample_records",
}

// purgeOne deletes one already-locked record and the rows keyed by it, inside the walk's tx. Order:
// dependent rows first, the obj_* row last, so the "exactly one row" check on the obj_ DELETE is the
// final word on whether the record was really there.
func (w *purgeWalk) purgeOne(rec PurgedRecord) error {
	ctx, tx, tenantID := w.ctx, w.tx, w.p.TenantID
	var fileIDs []string
	if err := tx.SelectContext(ctx, &fileIDs,
		"SELECT `id` FROM `sys_files` WHERE `tenant_id` = ? AND `entity_name` = ? AND `record_id` = ? FOR UPDATE",
		tenantID, rec.Entity, rec.ID); err != nil {
		return purgeErr("list files", err)
	}
	for _, t := range recordKeyedTables {
		// t is one of the constant table names above, never input.
		n, err := execCount(ctx, tx, "DELETE FROM `"+t+"` WHERE `tenant_id` = ? AND `entity_name` = ? AND `record_id` = ?",
			tenantID, rec.Entity, rec.ID)
		if err != nil {
			return purgeErr("delete "+t, err)
		}
		w.res.count(t, n)
	}
	// Run history carries no entity_name; record ids are UUIDs (idx_wfr_tenant_record, mig 000077).
	n, err := execCount(ctx, tx, "DELETE FROM `sys_workflow_runs` WHERE `tenant_id` = ? AND `record_id` = ?", tenantID, rec.ID)
	if err != nil {
		return purgeErr("delete sys_workflow_runs", err)
	}
	w.res.count("sys_workflow_runs", n)
	for _, peer := range w.p.Graph[rec.Entity].JoinPeers {
		joinTable, colA, colB, err := ddl.JoinParts(rec.Entity, peer)
		if err != nil {
			return fmt.Errorf("dynrepo.PurgeRecord: join table: %w", err)
		}
		col, other := colB, colA
		if colA == rec.Entity+"_id" {
			col, other = colA, colB
		}
		if w.p.SampleRoot {
			// A link to an UNMARKED record is real data: it blocks and stays (the delete below is skipped).
			held, err := w.unmarkedLinks(joinTable, col, other, peer, rec.ID)
			if err != nil {
				return err
			}
			if held {
				continue
			}
		}
		n, err := execCount(ctx, tx, fmt.Sprintf("DELETE FROM `%s` WHERE `tenant_id` = ? AND `%s` = ?", joinTable, col), tenantID, rec.ID)
		if err != nil {
			return purgeErr("delete links "+joinTable, err)
		}
		w.res.count(joinTable, n)
	}
	table, err := tableFor(rec.Entity)
	if err != nil {
		return fmt.Errorf("dynrepo.PurgeRecord: %w", err)
	}
	n, err = execCount(ctx, tx, fmt.Sprintf("DELETE FROM `%s` WHERE `tenant_id` = ? AND `id` = ?", table), tenantID, rec.ID)
	if err != nil {
		return purgeErr("delete record", err)
	}
	if n != 1 {
		return ErrRecordNotFound // unreachable after the FOR UPDATE lock; never report a phantom purge
	}
	w.res.count(table, n)
	w.res.FileIDs = append(w.res.FileIDs, fileIDs...)
	return nil
}

// decodeData unmarshals an obj_* data column (NULL/empty ⇒ empty map).
func decodeData(raw []byte) (map[string]any, error) {
	data := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("dynrepo.PurgeRecord: unmarshal data: %w", err)
		}
	}
	return data, nil
}

// execCount runs one DELETE/UPDATE and returns its affected-row count.
func execCount(ctx context.Context, tx *sqlx.Tx, stmt string, args ...any) (int64, error) {
	r, err := tx.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// purgeErr wraps a purge statement error, classifying a deadlock / lock-wait as ErrWriteConflict
// (MySQL already rolled the transaction back — the handler answers 503 + Retry-After).
func purgeErr(step string, err error) error {
	if isWriteConflict(err) {
		return fmt.Errorf("dynrepo.PurgeRecord: %s: %w", step, ErrWriteConflict)
	}
	return fmt.Errorf("dynrepo.PurgeRecord: %s: %w", step, err)
}

// sampleMarkPredicate is the "row has / has no live sample mark" test on `table`. Its two
// placeholders are (tenant_id, entity_name) and come AFTER the caller's other arguments.
func sampleMarkPredicate(table string, marked bool) string {
	not := ""
	if !marked {
		not = "NOT "
	}
	return fmt.Sprintf(not+"EXISTS (SELECT 1 FROM `sys_sample_records` s WHERE s.`tenant_id` = ? AND s.`entity_name` = ? "+
		"AND s.`record_id` = `%s`.`id` AND s.`deleted_at` IS NULL)", table)
}

// sampleChildRows is children() for SampleRoot: it counts the UNMARKED rows that name rec (live or
// binned — they block, are locked and never touched) and returns the MARKED ones (ids, plus data
// for a required child that goes with the cluster). A 1-1 binned row projects NULL in its
// CASE-guarded column, so it takes a second probe on the JSON value.
func (w *purgeWalk) sampleChildRows(c PurgeChildRef, table, childEntity, field, match, binnedMatch string, args []any) ([]purgeChildRow, error) {
	conds := []string{match}
	if c.OneToOne {
		conds = append(conds, binnedMatch)
	}
	cols := "`id`"
	if c.Required {
		cols = "`id`, `data`"
	}
	markArgs := []any{w.p.TenantID, childEntity}
	var rows []purgeChildRow
	for _, cond := range conds {
		var unmarked int
		if err := w.tx.GetContext(w.ctx, &unmarked, fmt.Sprintf(
			"SELECT COUNT(*) FROM `%s` WHERE `tenant_id` = ? AND (%s) AND %s FOR UPDATE", table, cond, sampleMarkPredicate(table, false)),
			append(append([]any{w.p.TenantID}, args...), markArgs...)...); err != nil {
			return nil, purgeErr("lock unmarked dependents", err)
		}
		if unmarked > 0 {
			w.blocked[[3]string{childEntity, field, ""}] += unmarked
		}
		got, err := w.probe("lock sample dependents", table, cols, "("+cond+") AND "+sampleMarkPredicate(table, true),
			append(append([]any{}, args...), markArgs...)...)
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}
	return rows, nil
}

// unmarkedLinks counts (FOR UPDATE) the N-N link rows of recID whose other end is an UNMARKED
// record; any ⇒ a blocker {peer, join table} and true (the caller keeps those rows).
func (w *purgeWalk) unmarkedLinks(joinTable, col, other, peer, recID string) (bool, error) {
	peerName, err := entityNameForPivot(peer)
	if err != nil {
		return false, fmt.Errorf("dynrepo.PurgeRecord: join peer: %w", err)
	}
	var n int
	if err := w.tx.GetContext(w.ctx, &n, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%[1]s` j WHERE j.`tenant_id` = ? AND j.`%[2]s` = ? AND NOT EXISTS ("+
			"SELECT 1 FROM `sys_sample_records` s WHERE s.`tenant_id` = ? AND s.`entity_name` = ? "+
			"AND s.`record_id` = j.`%[3]s` AND s.`deleted_at` IS NULL) FOR UPDATE", joinTable, col, other),
		w.p.TenantID, recID, w.p.TenantID, peerName); err != nil {
		return false, purgeErr("lock unmarked links "+joinTable, err)
	}
	if n > 0 {
		w.blocked[[3]string{peerName, joinTable, ""}] += n
	}
	return n > 0, nil
}
