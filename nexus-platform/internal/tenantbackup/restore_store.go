package tenantbackup

// restore_store.go — the SQL half of a restore (specs/068-tenant-backup-restore slice D): the
// catalogue bookkeeping of a pre_restore row and the ONE data transaction. Every statement is
// tenant-pinned (a child without tenant_id goes through its parent) and parameterised; table and
// column names are validated identifiers that come from the live schema + the policy, never from
// the backup file.

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// ApplyHooks are test seams of the data transaction; all nil in production.
type ApplyHooks struct {
	// AfterBatch runs inside the transaction after every INSERT batch (table, batch number).
	AfterBatch func(table string, batch int) error
	// BeforeAccess runs inside the transaction right before the access group (the D-27 gap).
	BeforeAccess func() error
	// AfterCommit runs right after COMMIT; an error leaves the row for the reaper (FR-071).
	AfterCommit func() error
}

// ApplyInput is everything the data transaction needs.
type ApplyInput struct {
	TenantID  string
	RestoreID string // the pre_restore catalogue row
	Reader    *Reader
	Manifest  *Manifest
	TxTimeout time.Duration
	Hooks     ApplyHooks
}

// ApplyResult is what the transaction counted. Committed is true once COMMIT returned, even when
// the function then returns an error (AfterCommit): the caller must not fail such a restore.
type ApplyResult struct {
	Committed          bool             `json:"-"`
	Deleted            map[string]int64 `json:"deleted"`
	Inserted           map[string]int64 `json:"inserted"`
	RolesLost          int              `json:"rolesLost"`
	MembershipsSkipped int              `json:"membershipsSkipped"`
	SharesSkipped      int              `json:"sharesSkipped"`
	WebhookKeysDropped int              `json:"webhookKeysDropped"`
	PendingBlobRemoval []string         `json:"-"`
}

// RestoreStore is the write side of a restore (GormStore in production, a fake in unit tests).
type RestoreStore interface {
	SetRestoreStep(ctx context.Context, tenantID, id, step string) error
	RestoreHeartbeat(ctx context.Context, tenantID, id string) (int64, error)
	// FinishRestore ends a RUNNING restore as done|failed; errCode "" leaves error_code alone.
	FinishRestore(ctx context.Context, tenantID, id, status string, report json.RawMessage, errCode, errDetail string) (int64, error)
	ListStaleRestores(ctx context.Context, olderThan time.Duration) ([]StaleRestore, error)
	// ExecDDL runs one CREATE TABLE IF NOT EXISTS built by ddl.BuildCreateTable/BuildCreateJoinTable.
	ExecDDL(ctx context.Context, stmt string) error
	ApplyRestore(ctx context.Context, in ApplyInput) (*ApplyResult, error)
}

// StaleRestore is a restore whose heartbeat stopped. Read across tenants by the reaper (ids only).
type StaleRestore struct {
	ID, TenantID, SourceID, StorageKey string
	ActorTenantID, CreatedBy           string
	CommittedAt                        *time.Time
	Report                             json.RawMessage
	BackupStatus                       string
}

var _ RestoreStore = (*GormStore)(nil)

// SetRestoreStep records the step of a running restore (the 4-step screen reads it).
func (r *GormStore) SetRestoreStep(ctx context.Context, tenantID, id, step string) error {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE sys_tenant_backups SET restore_step = ? WHERE id = ? AND tenant_id = ? AND restore_status = 'running'", step, id, tenantID)
	if res.Error != nil {
		return fmt.Errorf("tenantbackup.GormStore.SetRestoreStep: %w", res.Error)
	}
	return nil
}

// RestoreHeartbeat bumps updated_at of a RUNNING restore row.
func (r *GormStore) RestoreHeartbeat(ctx context.Context, tenantID, id string) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE sys_tenant_backups SET updated_at = CURRENT_TIMESTAMP(3) WHERE id = ? AND tenant_id = ? AND restore_status = 'running'", id, tenantID)
	if res.Error != nil {
		return 0, fmt.Errorf("tenantbackup.GormStore.RestoreHeartbeat: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// FinishRestore ends a running restore. The WHERE excludes the state it writes.
func (r *GormStore) FinishRestore(ctx context.Context, tenantID, id, status string, report json.RawMessage, errCode, errDetail string) (int64, error) {
	if len(errDetail) > 500 {
		errDetail = errDetail[:500]
	}
	var rep any
	if len(report) > 0 {
		rep = string(report)
	}
	res := r.db.WithContext(ctx).Exec(`UPDATE sys_tenant_backups SET restore_status = ?, restore_report = ?,
		restore_finished_at = CURRENT_TIMESTAMP(3), error_code = IF(? = '', error_code, ?), error_detail = IF(? = '', error_detail, ?)
		WHERE id = ? AND tenant_id = ? AND restore_status = 'running'`,
		status, rep, errCode, errCode, errDetail, errDetail, id, tenantID)
	if res.Error != nil {
		return 0, fmt.Errorf("tenantbackup.GormStore.FinishRestore: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// ListStaleRestores lists running restores with no heartbeat for olderThan. Like ListStale it is a
// cross-tenant read of ids and the file key only.
func (r *GormStore) ListStaleRestores(ctx context.Context, olderThan time.Duration) ([]StaleRestore, error) {
	var rows []struct {
		ID, TenantID, Status string
		SourceID, StorageKey *string
		ActorTenantID        *string
		CreatedBy            *string
		CommittedAt          *time.Time
		Report               []byte
	}
	err := r.sdb.SelectContext(ctx, &rows, `SELECT id AS id, tenant_id AS tenantid, status AS status,
		restore_source_id AS sourceid, storage_key AS storagekey, actor_tenant_id AS actortenantid, created_by AS createdby,
		restore_committed_at AS committedat, restore_report AS report
		FROM sys_tenant_backups WHERE restore_status = 'running' AND deleted_at IS NULL
		  AND updated_at < NOW(3) - INTERVAL ? SECOND`, int64(olderThan.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ListStaleRestores: %w", err)
	}
	out := make([]StaleRestore, 0, len(rows))
	for _, x := range rows {
		s := StaleRestore{ID: x.ID, TenantID: x.TenantID, BackupStatus: x.Status, CommittedAt: x.CommittedAt, Report: x.Report}
		s.SourceID, s.StorageKey = deref(x.SourceID), deref(x.StorageKey)
		s.ActorTenantID, s.CreatedBy = deref(x.ActorTenantID), deref(x.CreatedBy)
		out = append(out, s)
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ExecDDL runs one statement produced by the ddl builders (DDL commits implicitly: that is why a
// restore does it BEFORE its data transaction, plan D-6).
func (r *GormStore) ExecDDL(ctx context.Context, stmt string) error {
	if _, err := r.sdb.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("tenantbackup.GormStore.ExecDDL: %w", err)
	}
	return nil
}

// ---- the data transaction ---------------------------------------------------------------------

// accessSeed is the permission group: written LAST so the row locks its CASCADE ... SET NULL takes
// on users are held for milliseconds (plan D-17).
var accessSeed = []string{"acl_roles", "acl_role_actions", "sys_field_acl"}

func conflictErr(table string, err error) error {
	var me *gomysql.MySQLError
	if errors.As(err, &me) && me.Number == mysqlDuplicateEntry {
		return &Error{Code: "TENANT_RESTORE_CONFLICT", Msg: "Bản sao lưu có dòng trùng id với dữ liệu của khách khác",
			Details: map[string]any{"table": table}}
	}
	return fmt.Errorf("tenantbackup.ApplyRestore %s: %w", table, err)
}

func mismatch(msg string, table string) *Error {
	return &Error{Code: "TENANT_RESTORE_TENANT_MISMATCH", Msg: msg, Details: map[string]any{"table": table}}
}

// tablePlan is one table of the restore with its file columns and the indexes the row guards use.
type restoreTable struct {
	ref              TableRef
	cols             []Column
	inFile           bool
	tenantIdx, idIdx int
	ownerIdx         int
	userIdx          int // column checked against the customer's own users (SkipOrphan / shares)
	actionsIdx       int
	parentOf         bool
}

// ApplyRestore runs the single data transaction of a restore. See plan.md §Khôi phục bước 5.
func (r *GormStore) ApplyRestore(ctx context.Context, in ApplyInput) (res *ApplyResult, err error) {
	res = &ApplyResult{Deleted: map[string]int64{}, Inserted: map[string]int64{}}
	tid := in.TenantID

	// Tables of the CURRENT schema decide what is written (never the manifest's class column).
	refs, err := exportTables(ctx, r.sdb)
	if err != nil {
		return nil, err
	}
	tenantTables, fks, err := LoadSchema(ctx, r.sdb)
	if err != nil {
		return nil, err
	}
	mt := map[string]ManifestTable{}
	for _, t := range in.Manifest.Tables {
		mt[t.Name] = t
	}
	tables := map[string]*restoreTable{}
	var seq *restoreTable
	set := map[string]bool{}
	owners := map[string]bool{}
	for _, ref := range refs {
		switch ref.Rule.Class {
		case Restore, MergeMax:
		default:
			continue
		}
		rt := &restoreTable{ref: ref, tenantIdx: -1, idIdx: -1, ownerIdx: -1, userIdx: -1, actionsIdx: -1}
		if m, ok := mt[ref.Table]; ok && in.Reader.HasTable(ref.Table) {
			rt.inFile, rt.cols = true, m.Columns
			for i, c := range m.Columns {
				switch {
				case c.Name == "tenant_id":
					rt.tenantIdx = i
				case c.Name == "id":
					rt.idIdx = i
				case ref.Rule.Owner != nil && c.Name == ref.Rule.Owner.Column:
					rt.ownerIdx = i
				case ref.Table == "sys_workflows" && c.Name == "actions":
					rt.actionsIdx = i
				}
				for col := range ref.Rule.SkipOrphan {
					if c.Name == col {
						rt.userIdx = i
					}
				}
				if ref.Table == "sys_record_shares" && c.Name == "user_id" {
					rt.userIdx = i
				}
			}
		}
		if ref.Rule.Owner != nil {
			owners[ref.Rule.Owner.Parent] = true
		}
		if ref.Rule.Class == MergeMax {
			seq = rt
			continue
		}
		tables[ref.Table] = rt
		set[ref.Table] = true
	}
	for t := range owners {
		if tables[t] != nil {
			tables[t].parentOf = true
		}
	}
	var clears []TableRef
	for _, t := range tenantTables {
		if rule, ok := RuleFor(t); ok && rule.Class == Clear {
			clears = append(clears, TableRef{Table: t, Rule: rule})
		}
	}
	// access group = seed + everything that has an FK into it (so no row is inserted before its parent).
	access := map[string]bool{}
	for _, t := range accessSeed {
		if set[t] {
			access[t] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for _, fk := range fks {
			if set[fk.Table] && !access[fk.Table] && access[fk.RefTable] {
				access[fk.Table], grew = true, true
			}
		}
	}
	order := topoOrder(set, fks)
	var mainOrder, accessOrder []string
	for _, t := range order {
		if access[t] {
			accessOrder = append(accessOrder, t)
		} else {
			mainOrder = append(mainOrder, t)
		}
	}

	conn, err := r.sdb.Connx(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: conn: %w", err)
	}
	defer conn.Close()
	var prevTZ string
	if err := conn.GetContext(ctx, &prevTZ, "SELECT @@session.time_zone"); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: read time zone: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: set time zone: %w", err)
	}
	defer func() {
		// The pooled connection goes back exactly as found.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, rerr := conn.ExecContext(rctx, "SET time_zone = ?", prevTZ); rerr != nil {
			// A connection we could not reset must not go back to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	txCtx, cancel := context.WithTimeout(ctx, in.TxTimeout)
	defer cancel()
	tx, err := conn.BeginTxx(txCtx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// --- snapshots (before anything is deleted) -------------------------------------------------
	type wfSnap struct {
		ID      string         `db:"id"`
		LastRun sql.NullString `db:"last_run"`
		Actions []byte         `db:"actions"`
	}
	var wfs []wfSnap
	if err := tx.SelectContext(txCtx, &wfs, `SELECT id, DATE_FORMAT(last_run_at,'%Y-%m-%d %H:%i:%s.%f') AS last_run, actions
		FROM sys_workflows WHERE tenant_id = ?`, tid); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: snapshot workflows: %w", err)
	}
	var filesBefore []string
	if err := tx.SelectContext(txCtx, &filesBefore, "SELECT id FROM sys_files WHERE tenant_id = ?", tid); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: snapshot files: %w", err)
	}
	var userIDs []string
	if err := tx.SelectContext(txCtx, &userIDs, "SELECT id FROM users WHERE tenant_id = ?", tid); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: snapshot users: %w", err)
	}
	ownUsers := make(map[string]bool, len(userIDs))
	for _, u := range userIDs {
		ownUsers[u] = true
	}

	inserted := map[string]map[string]bool{} // parent table -> ids just written
	restoredFiles := map[string]bool{}
	newActions := map[string]string{} // workflow id -> actions as restored
	batchNo := 0

	deleteGroup := func(names []string) error {
		for i := len(names) - 1; i >= 0; i-- {
			rt := tables[names[i]]
			q, err := rt.ref.deleteQuery()
			if err != nil {
				return err
			}
			rs, err := tx.ExecContext(txCtx, q, tid)
			if err != nil {
				return fmt.Errorf("tenantbackup.GormStore.ApplyRestore: delete %s: %w", rt.ref.Table, err)
			}
			n, _ := rs.RowsAffected()
			res.Deleted[rt.ref.Table] = n
		}
		return nil
	}
	insertGroup := func(names []string) error {
		for _, name := range names {
			rt := tables[name]
			if !rt.inFile {
				continue
			}
			if err := r.insertTable(txCtx, tx, in, rt, tid, ownUsers, inserted, restoredFiles, newActions, res, &batchNo); err != nil {
				return err
			}
			if _, err := tx.ExecContext(txCtx, "UPDATE sys_tenant_backups SET updated_at = CURRENT_TIMESTAMP(3) WHERE id = ? AND tenant_id = ?", in.RestoreID, tid); err != nil {
				return fmt.Errorf("tenantbackup.GormStore.ApplyRestore: heartbeat: %w", err)
			}
		}
		return nil
	}

	// --- data + configuration group ------------------------------------------------------------
	for _, c := range clears {
		q, err := c.deleteQuery()
		if err != nil {
			return nil, err
		}
		rs, err := tx.ExecContext(txCtx, q, tid)
		if err != nil {
			return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: clear %s: %w", c.Table, err)
		}
		n, _ := rs.RowsAffected()
		res.Deleted[c.Table] = n
	}
	if err := deleteGroup(mainOrder); err != nil {
		return nil, err
	}
	if err := insertGroup(mainOrder); err != nil {
		return nil, err
	}
	if seq != nil && seq.inFile {
		if err := r.insertTable(txCtx, tx, in, seq, tid, ownUsers, inserted, restoredFiles, newActions, res, &batchNo); err != nil {
			return nil, err
		}
	}
	oldActions := map[string][]byte{}
	lastRun := map[string]sql.NullString{}
	for _, w := range wfs {
		oldActions[w.ID] = w.Actions
		lastRun[w.ID] = w.LastRun
	}
	// last_run_at keeps the CURRENT value (a restored rule must not run catch-up); a rule that did
	// not exist before starts "now".
	for id := range newActions {
		var last any = nil
		if lr, ok := lastRun[id]; ok {
			if lr.Valid {
				last = lr.String
			}
			if _, err := tx.ExecContext(txCtx, "UPDATE sys_workflows SET last_run_at = ?, updated_at = updated_at WHERE id = ? AND tenant_id = ?", last, id, tid); err != nil {
				return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: keep last_run_at: %w", err)
			}
			continue
		}
		if _, err := tx.ExecContext(txCtx, "UPDATE sys_workflows SET last_run_at = NOW(3), updated_at = updated_at WHERE id = ? AND tenant_id = ?", id, tid); err != nil {
			return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: start last_run_at: %w", err)
		}
	}
	for id, actions := range newActions {
		if !strings.Contains(actions, "outbound_webhook") {
			continue
		}
		merged, dropped, err := ReattachWebhookKeys(actions, oldActions[id])
		if err != nil {
			return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: webhook keys of %s: %w", id, err)
		}
		res.WebhookKeysDropped += dropped
		if merged == actions {
			continue
		}
		if _, err := tx.ExecContext(txCtx, "UPDATE sys_workflows SET actions = ?, updated_at = updated_at WHERE id = ? AND tenant_id = ?", merged, id, tid); err != nil {
			return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: re-attach webhook keys: %w", err)
		}
	}

	// --- permission group, LAST ----------------------------------------------------------------
	if in.Hooks.BeforeAccess != nil {
		if err := in.Hooks.BeforeAccess(); err != nil {
			return nil, err
		}
	}
	var curRoles []struct {
		ID   string         `db:"id"`
		Role sql.NullString `db:"role_id"`
	}
	// FOR UPDATE: an admin who drops someone's role mid-restore waits for COMMIT and then wins (D-27).
	if err := tx.SelectContext(txCtx, &curRoles, "SELECT id, role_id FROM users WHERE tenant_id = ? FOR UPDATE", tid); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: snapshot user roles: %w", err)
	}
	// Customer owners (specs/065 FR-026): the reserved role is recreated below, never counted as lost.
	var ownerIDs []string
	if err := tx.SelectContext(txCtx, &ownerIDs,
		`SELECT u.id FROM users u JOIN acl_roles r ON r.id = u.role_id AND r.tenant_id = u.tenant_id
		 WHERE u.tenant_id = ? AND r.name = ?`, tid, auth.OwnerRoleName); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: read owners: %w", err)
	}
	isOwner := make(map[string]bool, len(ownerIDs))
	for _, id := range ownerIDs {
		isOwner[id] = true
	}
	if err := deleteGroup(accessOrder); err != nil {
		return nil, err
	}
	if err := insertGroup(accessOrder); err != nil {
		return nil, err
	}
	if len(ownerIDs) > 0 {
		// Same tx: wrap the sqlx tx so services.SeedOwnerRole writes into it.
		gtx, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: tx.Tx, SkipInitializeWithVersion: true}), &gorm.Config{})
		if err != nil {
			return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: wrap tx: %w", err)
		}
		roleID, _, err := services.SeedOwnerRole(txCtx, gtx, tid)
		if err != nil {
			return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: owner role: %w", err)
		}
		q, args, err := sqlx.In("UPDATE users SET role_id = ? WHERE tenant_id = ? AND role_id IS NULL AND id IN (?)", roleID, tid, ownerIDs)
		if err != nil {
			return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: owner re-attach query: %w", err)
		}
		if _, err := tx.ExecContext(txCtx, q, args...); err != nil {
			return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: owner re-attach: %w", err)
		}
	}
	for _, u := range curRoles {
		if !u.Role.Valid || isOwner[u.ID] {
			continue
		}
		if !inserted["acl_roles"][u.Role.String] {
			res.RolesLost++
			continue
		}
		if _, err := tx.ExecContext(txCtx, "UPDATE users SET role_id = ? WHERE id = ? AND tenant_id = ? AND role_id IS NULL", u.Role.String, u.ID, tid); err != nil {
			return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: re-attach role: %w", err)
		}
	}

	// --- branding, commit marker, COMMIT -------------------------------------------------------
	tj, err := in.Reader.TenantJSON()
	if err != nil {
		return nil, err
	}
	var tenantDoc struct {
		Branding json.RawMessage `json:"branding"`
	}
	if err := json.Unmarshal(tj, &tenantDoc); err != nil {
		return nil, corrupt("tenant.json không đọc được")
	}
	var branding any
	if len(tenantDoc.Branding) > 0 && !bytes.Equal(tenantDoc.Branding, []byte("null")) {
		branding = string(tenantDoc.Branding)
	}
	if _, err := tx.ExecContext(txCtx, "UPDATE tenants SET branding = ? WHERE id = ?", branding, tid); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: branding: %w", err)
	}
	for _, id := range filesBefore {
		if !restoredFiles[id] {
			res.PendingBlobRemoval = append(res.PendingBlobRemoval, id)
		}
	}
	pending, err := json.Marshal(append([]string{}, res.PendingBlobRemoval...))
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: pending blobs: %w", err)
	}
	// In the SAME transaction: the marker exists exactly when the data changed (D-23).
	if _, err := tx.ExecContext(txCtx, `UPDATE sys_tenant_backups SET restore_committed_at = NOW(3),
		restore_report = JSON_OBJECT('pendingBlobRemovals', CAST(? AS JSON)) WHERE id = ? AND tenant_id = ?`, string(pending), in.RestoreID, tid); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: commit marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ApplyRestore: commit: %w", err)
	}
	committed = true
	res.Committed = true
	if in.Hooks.AfterCommit != nil {
		if err := in.Hooks.AfterCommit(); err != nil {
			return res, err
		}
	}
	return res, nil
}

// insertTable streams one table of the file into the transaction in batches of BatchSize rows.
func (r *GormStore) insertTable(ctx context.Context, tx *sqlx.Tx, in ApplyInput, rt *restoreTable, tid string,
	ownUsers map[string]bool, inserted map[string]map[string]bool, restoredFiles map[string]bool,
	newActions map[string]string, res *ApplyResult, batchNo *int) error {
	table := rt.ref.Table
	if rt.ref.Rule.Owner == nil && rt.tenantIdx < 0 {
		return corrupt("bảng %s thiếu cột tenant_id", table)
	}
	if rt.ref.Rule.Owner != nil && rt.ownerIdx < 0 {
		return corrupt("bảng %s thiếu cột %s", table, rt.ref.Rule.Owner.Column)
	}
	rr, err := in.Reader.Table(table)
	if err != nil {
		return err
	}
	defer rr.Close()
	size := BatchSize(len(rt.cols))
	batch := make([][]any, 0, size)
	suffix := ""
	if rt.ref.Rule.Class == MergeMax {
		suffix = " ON DUPLICATE KEY UPDATE next_val = GREATEST(next_val, VALUES(next_val))"
	}
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		q, args, err := BuildInsert(table, rt.cols, batch)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, q+suffix, args...); err != nil {
			return conflictErr(table, err)
		}
		res.Inserted[table] += int64(len(batch))
		batch = batch[:0]
		*batchNo++
		if in.Hooks.AfterBatch != nil {
			return in.Hooks.AfterBatch(table, *batchNo)
		}
		return nil
	}
	for {
		line, err := rr.ReadLine()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		args, err := DecodeRow(rt.cols, line)
		if err != nil {
			return corrupt("dòng không đọc được trong bảng %s", table)
		}
		if rt.ref.Rule.Owner == nil {
			if v, _ := args[rt.tenantIdx].(string); v != tid {
				return mismatch("Bản sao lưu có dòng của khách khác", table)
			}
		} else {
			p, _ := args[rt.ownerIdx].(string)
			if !inserted[rt.ref.Rule.Owner.Parent][p] {
				return mismatch("Bản sao lưu có dòng con trỏ tới cha không thuộc khách", table)
			}
		}
		if rt.userIdx >= 0 {
			if u, ok := args[rt.userIdx].(string); ok && !ownUsers[u] {
				switch table {
				case "team_memberships":
					res.MembershipsSkipped++
				case "sys_record_shares":
					res.SharesSkipped++
				}
				continue
			}
		}
		if rt.parentOf && rt.idIdx >= 0 {
			if inserted[table] == nil {
				inserted[table] = map[string]bool{}
			}
			if id, _ := args[rt.idIdx].(string); id != "" {
				inserted[table][id] = true
			}
		}
		if table == "sys_files" && rt.idIdx >= 0 {
			if id, _ := args[rt.idIdx].(string); id != "" {
				restoredFiles[id] = true
			}
		}
		if table == "sys_workflows" && rt.idIdx >= 0 && rt.actionsIdx >= 0 {
			if id, _ := args[rt.idIdx].(string); id != "" {
				a, _ := args[rt.actionsIdx].(string)
				newActions[id] = a
			}
		}
		batch = append(batch, args)
		if len(batch) >= size {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}
