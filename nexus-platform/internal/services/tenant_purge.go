package services

// Tenant purge — the missing second half of the tenant lifecycle.
//
// RetireTenant soft-deletes ONE row (`tenants`) and its own doc comment says so: "the data stays".
// Login and inbound webhooks stop, but every business row the customer ever created still sits in
// the 80-odd shared tables keyed by tenant_id, forever. That is a hole with three faces:
//
//   - a customer who leaves cannot actually be removed — Nexus keeps their real business data;
//   - the debris of throwaway test tenants accumulates in the same tables as live customers;
//   - VISION-AND-DELIVERY §6b's hosting leg rests on per-tenant data ops that did not exist.
//
// WHY THE NAIVE VERSION IS WRONG. "DELETE FROM every table WHERE tenant_id = ?" misses three
// tables that hold tenant data but carry NO tenant_id column of their own — acl_role_actions,
// team_memberships, team_sets_teams — reachable only through their parent. Every one of those
// FKs is ON DELETE CASCADE, so the correct move is the opposite of the usual purge idiom: keep
// FOREIGN_KEY_CHECKS **ON** and let the database cascade the children (native platform rung —
// the DB already models this, app code does not need to). All the non-cascading FKs point at
// `tenants` itself, which is why the tenants row goes LAST.
//
// That correctness depends on a schema fact, so it is CHECKED at runtime rather than assumed:
// purgeSchemaDrift re-derives the "child table with no tenant_id" set from information_schema and
// refuses the purge if any such FK is not CASCADE. A future migration that adds a child table with
// ON DELETE NO ACTION gets a loud refusal instead of a silent orphan.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
)

// PurgeTableCount is one table's contribution to a purge (rows counted in dry-run, rows deleted
// otherwise). Reported per table because "we removed 12,000 rows" is not reviewable but
// "obj_crm_accounts 431, audit_logs 8,102" is. One type for both purges (tenant and single
// record, specs/056) — the record purge's repository already produces it.
type PurgeTableCount = dynrepo.PurgeTableCount

// PurgeReport is what the operator reads before (dry-run) and after a purge.
type PurgeReport struct {
	TenantID  string            `json:"tenantId"`
	Slug      string            `json:"slug"`
	DryRun    bool              `json:"dryRun"`
	Tables    []PurgeTableCount `json:"tables"`
	TotalRows int64             `json:"totalRows"`
	// Blobs* describe the FILES on disk/object storage, which no SQL DELETE can reach. Reported
	// separately and explicitly because the first version of this purge deleted the sys_files
	// catalog rows and left every byte behind — while the report, counting only rows, read as
	// though the customer's documents were gone. Telling someone their data was deleted when it
	// was not is worse than failing to delete it.
	BlobsRemoved int    `json:"blobsRemoved"`
	BlobsNote    string `json:"blobsNote,omitempty"`
	// Backups* are the same honesty for the customer's own backup zips (specs/068, FR-060): a
	// purge that deleted the catalogue rows but left the files would keep a "deleted" customer's
	// real data on the backup disk.
	BackupsRemoved int    `json:"backupsRemoved"`
	BackupsNote    string `json:"backupsNote,omitempty"`
}

// backupRemover deletes one purged tenant's backup files: the given keys, then the whole
// `<tenant>/` directory. A func port (like blobRemover) so this package imports nothing of
// tenantbackup; cmd/server wires tenantbackup.NewRemover.
type backupRemover func(ctx context.Context, tenantID string, keys []string) (int, error)

// WithBackupRemover wires the backup-file side of a purge. Chainable.
func (s *TenantProvisionService) WithBackupRemover(f backupRemover) *TenantProvisionService {
	s.backups = f
	return s
}

// blobRemover deletes one retired tenant's stored files. A func port, not an interface, so this
// package needs no dependency on files/storage. Optional: when unwired the purge still runs but
// SAYS the blobs remain (BlobsNote) instead of implying they went with the rows.
type blobRemover func(ctx context.Context, tenantID string, fileIDs []string) (int, error)

// PurgeTenant permanently removes every row belonging to one retired tenant, then the tenant row.
//
// It is deliberately hard to fire by accident, because it is the one operation in this codebase
// with no undo whatsoever:
//
//  1. never the caller's own tenant;
//  2. the tenant must ALREADY be retired — purge is the second step of a two-step lifecycle, so
//     nobody deletes a live customer's data in one call;
//  3. confirmSlug must equal the tenant's slug exactly (the same type-the-name confirmation the
//     destructive AI cards use);
//  4. dryRun (the handler's default) counts and deletes nothing.
//
// The deletes run in ONE transaction: a half-purged tenant — some tables emptied, others not — is
// strictly worse than a failed purge, because nothing downstream can tell the difference between
// that and real data.
//
// ponytail: whole-tenant delete in a single tx. Fine at the scale this engine ships at (a large
// tenant is tens of thousands of rows); if a tenant ever grows past what one tx should hold, batch
// per table with a LIMIT loop and drop atomicity deliberately, not by accident.
// WithBlobRemover wires the file-storage side of a purge. Chainable.
func (s *TenantProvisionService) WithBlobRemover(f blobRemover) *TenantProvisionService {
	s.blobs = f
	return s
}

func (s *TenantProvisionService) PurgeTenant(ctx context.Context, callerTenantID, actorUserID, tenantID, confirmSlug string, dryRun bool) (*PurgeReport, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil, &ValidationError{Code: "TENANT_NOT_FOUND", Msg: "không tìm thấy workspace"}
	}
	if tenantID == strings.TrimSpace(callerTenantID) {
		return nil, &ValidationError{
			Code: "TENANT_SELF_PURGE",
			Msg:  "không thể xoá vĩnh viễn chính workspace bạn đang đứng",
		}
	}

	// Unscoped: a retired tenant is soft-deleted, so the normal scope would report it missing —
	// and a retired tenant is exactly the one we are allowed to purge.
	var t auth.Tenant
	if err := s.db.WithContext(ctx).Unscoped().Where("id = ?", tenantID).First(&t).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, &ValidationError{Code: "TENANT_NOT_FOUND", Msg: "không tìm thấy workspace"}
		}
		return nil, fmt.Errorf("services.TenantProvisionService.PurgeTenant: load tenant: %w", err)
	}
	if !t.DeletedAt.Valid {
		return nil, &ValidationError{
			Code: "TENANT_PURGE_NOT_RETIRED",
			Msg:  "workspace này còn đang hoạt động — phải ngừng hoạt động (retire) trước rồi mới xoá vĩnh viễn được",
		}
	}
	if strings.TrimSpace(confirmSlug) != t.Slug {
		return nil, &ValidationError{
			Code:    "TENANT_PURGE_CONFIRM_MISMATCH",
			Msg:     fmt.Sprintf("gõ đúng slug %q để xác nhận xoá vĩnh viễn", t.Slug),
			Details: map[string]any{"expected": t.Slug},
		}
	}

	if err := s.purgeSchemaDrift(ctx); err != nil {
		return nil, err
	}
	tables, err := s.tenantScopedTables(ctx)
	if err != nil {
		return nil, err
	}

	report := &PurgeReport{TenantID: t.ID, Slug: t.Slug, DryRun: dryRun}

	// Read the blob ids BEFORE the transaction removes their catalog rows — afterwards there is no
	// record of what to delete, and an object store has no "everything under this tenant" sweep on
	// the Backend interface.
	var fileIDs []string
	if err := s.db.WithContext(ctx).Raw(
		"SELECT id FROM sys_files WHERE tenant_id = ?", t.ID,
	).Scan(&fileIDs).Error; err != nil {
		return nil, fmt.Errorf("services.TenantProvisionService.PurgeTenant: list files: %w", err)
	}

	// Backups (specs/068): a backup or restore is running for this customer => refuse BEFORE any
	// delete (a half-purged customer is worse than a refused purge). The keys of EVERY catalogue
	// row — hidden ones included — are read now; after the transaction they are gone with their rows.
	var backupKeys []string
	if err := s.db.WithContext(ctx).Raw(
		"SELECT storage_key FROM sys_tenant_backups WHERE tenant_id = ? AND storage_key IS NOT NULL", t.ID,
	).Scan(&backupKeys).Error; err != nil {
		return nil, fmt.Errorf("services.TenantProvisionService.PurgeTenant: list backups: %w", err)
	}
	if !dryRun {
		var running int64
		if err := s.db.WithContext(ctx).Raw(
			"SELECT COUNT(*) FROM sys_tenant_backups WHERE tenant_id = ? AND op_lock IS NOT NULL", t.ID,
		).Scan(&running).Error; err != nil {
			return nil, fmt.Errorf("services.TenantProvisionService.PurgeTenant: running backups: %w", err)
		}
		if running > 0 {
			return nil, &ValidationError{
				Code: "BACKUP_IN_PROGRESS",
				Msg:  "khách này đang có một tác vụ sao lưu hoặc khôi phục chạy — chờ xong rồi mới xoá vĩnh viễn",
			}
		}
	}

	if dryRun {
		if len(fileIDs) > 0 {
			report.BlobsNote = fmt.Sprintf("%d tệp đính kèm sẽ bị xoá khỏi kho lưu trữ", len(fileIDs))
		}
		if len(backupKeys) > 0 {
			report.BackupsNote = fmt.Sprintf("%d bản sao lưu của khách sẽ bị xoá cùng tệp của chúng", len(backupKeys))
		}
		for _, tbl := range tables {
			var n int64
			// #nosec G202 -- tbl comes from information_schema for THIS schema, never from input.
			if err := s.db.WithContext(ctx).Raw(
				"SELECT COUNT(*) FROM `"+tbl+"` WHERE tenant_id = ?", t.ID,
			).Scan(&n).Error; err != nil {
				return nil, fmt.Errorf("services.TenantProvisionService.PurgeTenant: count %s: %w", tbl, err)
			}
			if n > 0 {
				report.Tables = append(report.Tables, PurgeTableCount{Table: tbl, Rows: n})
				report.TotalRows += n
			}
		}
		return report, nil
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, tbl := range tables {
			// #nosec G202 -- tbl comes from information_schema for THIS schema, never from input.
			res := tx.Exec("DELETE FROM `"+tbl+"` WHERE tenant_id = ?", t.ID)
			if res.Error != nil {
				return fmt.Errorf("delete %s: %w", tbl, res.Error)
			}
			if res.RowsAffected > 0 {
				report.Tables = append(report.Tables, PurgeTableCount{Table: tbl, Rows: res.RowsAffected})
				report.TotalRows += res.RowsAffected
			}
		}
		// LAST, and hard: every non-cascading FK in the schema points here, so the tenants row can
		// only go once its dependents are gone. Unscoped because the row is already soft-deleted.
		if err := tx.Unscoped().Where("id = ?", t.ID).Delete(&auth.Tenant{}).Error; err != nil {
			return fmt.Errorf("delete tenant row: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("services.TenantProvisionService.PurgeTenant: %w", err)
	}

	// Blobs go AFTER the commit: a rolled-back purge must not have destroyed files, and a blob the
	// catalog no longer references is at worst dead weight, whereas a deleted blob with a live row
	// is a broken download. Best-effort by design — a storage fault must not undo a committed
	// purge — but never silent: whatever happens is stated in the report the operator reads.
	switch {
	case len(fileIDs) == 0:
		// nothing to say
	case s.blobs == nil:
		report.BlobsNote = fmt.Sprintf(
			"CHƯA xoá %d tệp trong kho lưu trữ (chức năng xoá tệp chưa được nối) — dữ liệu file của khách VẪN CÒN trên đĩa",
			len(fileIDs))
		s.logger.Warn("services.TenantProvisionService.PurgeTenant: blob remover not wired; files remain",
			zap.String("tenant_id", t.ID), zap.Int("files", len(fileIDs)))
	default:
		removed, berr := s.blobs(ctx, t.ID, fileIDs)
		report.BlobsRemoved = removed
		if berr != nil || removed < len(fileIDs) {
			report.BlobsNote = fmt.Sprintf("xoá được %d/%d tệp; số còn lại vẫn nằm trong kho lưu trữ",
				removed, len(fileIDs))
			s.logger.Warn("services.TenantProvisionService.PurgeTenant: some blobs survived",
				zap.String("tenant_id", t.ID), zap.Int("removed", removed),
				zap.Int("total", len(fileIDs)), zap.Error(berr))
		}
	}

	// The customer's backup zips, same rule as the blobs (after the commit, best-effort, never
	// silent). The remover also sweeps the whole `<tenant>/` directory, so a backup that was
	// mid-write when the purge ran leaves nothing behind. It is called even with zero known keys.
	switch {
	case s.backups == nil && len(backupKeys) > 0:
		report.BackupsNote = fmt.Sprintf(
			"CHƯA xoá %d tệp sao lưu của khách (chức năng xoá tệp sao lưu chưa được nối) — dữ liệu khách VẪN CÒN trên đĩa",
			len(backupKeys))
		s.logger.Warn("services.TenantProvisionService.PurgeTenant: backup remover not wired; backup files remain",
			zap.String("tenant_id", t.ID), zap.Int("backups", len(backupKeys)))
	case s.backups != nil:
		removed, berr := s.backups(ctx, t.ID, backupKeys)
		report.BackupsRemoved = removed
		if berr != nil || removed < len(backupKeys) {
			report.BackupsNote = fmt.Sprintf("gỡ được %d/%d tệp sao lưu; số còn lại vẫn nằm trên đĩa", removed, len(backupKeys))
			s.logger.Warn("services.TenantProvisionService.PurgeTenant: some backup files survived",
				zap.String("tenant_id", t.ID), zap.Int("removed", removed), zap.Int("total", len(backupKeys)), zap.Error(berr))
		}
	}

	s.logger.Warn("services.TenantProvisionService.PurgeTenant: workspace data purged",
		zap.String("tenant_id", t.ID), zap.String("slug", t.Slug),
		zap.String("by_tenant", callerTenantID), zap.Int64("rows", report.TotalRows))
	if s.auditor != nil {
		// Recorded in the OPERATOR's tenant on purpose: the purged tenant's own audit_logs rows
		// are part of what this call just deleted, so a trail written there would erase itself.
		s.auditor.Record(ctx, audit.Event{
			TenantID:    callerTenantID,
			ActorUserID: actorUserID,
			EntityName:  audit.TenantMeta,
			RecordID:    t.ID,
			Action:      audit.ActionDelete,
			Diff: audit.ComputeDiff(nil, map[string]any{
				"slug": t.Slug, "rows": report.TotalRows, "tables": len(report.Tables),
			}),
			At: time.Now().UTC(),
		})
	}
	return report, nil
}

// tenantScopedTables lists every table in the CURRENT schema carrying a tenant_id column, sorted
// for a deterministic report. Read from information_schema rather than a hand-kept list precisely
// because obj_* tables are created at runtime from metadata — a hand-kept list would be wrong the
// moment someone builds an entity, which is every day.
func (s *TenantProvisionService) tenantScopedTables(ctx context.Context) ([]string, error) {
	var tables []string
	err := s.db.WithContext(ctx).Raw(`
		SELECT TABLE_NAME FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND COLUMN_NAME = 'tenant_id'
		ORDER BY TABLE_NAME`).Scan(&tables).Error
	if err != nil {
		return nil, fmt.Errorf("services.TenantProvisionService.tenantScopedTables: %w", err)
	}
	if len(tables) == 0 {
		// Zero is never right on a migrated schema; treating it as "nothing to do" would report a
		// successful purge that removed nothing.
		return nil, &ValidationError{
			Code: "TENANT_PURGE_SCHEMA_DRIFT",
			Msg:  "không đọc được danh sách bảng theo tenant — dừng để tránh xoá nửa vời",
		}
	}
	return tables, nil
}

// CheckSchemaShape is the tenant-backup (specs/068) view of the purge's schema guard: the shape
// facts "every child of a tenant table cascades, every base table is one of the three shapes we
// understand" are the same facts a backup relies on, so it asks the SAME function rather than
// carrying a second copy of the SQL. main.go hands it to the tenantbackup service as a func port
// (tenantbackup does not import services).
func (s *TenantProvisionService) CheckSchemaShape(ctx context.Context) error {
	return s.purgeSchemaDrift(ctx)
}

// purgeSchemaDrift re-derives the tables that hold tenant data WITHOUT a tenant_id column (they
// hang off a tenant-scoped parent) and insists every such FK is ON DELETE CASCADE, which is what
// makes the tenant_id-only delete complete. Today that set is acl_role_actions, team_memberships
// and team_sets_teams, all CASCADE. This does not hardcode those names: it asks the schema, so a
// future migration that adds a child with NO ACTION gets a refusal here instead of quietly
// leaving rows behind that belong to a customer who was told their data was deleted.
func (s *TenantProvisionService) purgeSchemaDrift(ctx context.Context) error {
	var bad []string
	err := s.db.WithContext(ctx).Raw(`
		SELECT DISTINCT CONCAT(k.TABLE_NAME, ' -> ', k.REFERENCED_TABLE_NAME, ' (', rc.DELETE_RULE, ')')
		FROM information_schema.KEY_COLUMN_USAGE k
		JOIN information_schema.REFERENTIAL_CONSTRAINTS rc
		  ON rc.CONSTRAINT_SCHEMA = k.TABLE_SCHEMA AND rc.CONSTRAINT_NAME = k.CONSTRAINT_NAME
		WHERE k.TABLE_SCHEMA = DATABASE()
		  AND k.REFERENCED_TABLE_NAME IS NOT NULL
		  AND rc.DELETE_RULE <> 'CASCADE'
		  AND k.TABLE_NAME NOT IN (
		        SELECT TABLE_NAME FROM information_schema.COLUMNS
		        WHERE TABLE_SCHEMA = DATABASE() AND COLUMN_NAME = 'tenant_id')
		  AND k.REFERENCED_TABLE_NAME IN (
		        SELECT TABLE_NAME FROM information_schema.COLUMNS
		        WHERE TABLE_SCHEMA = DATABASE() AND COLUMN_NAME = 'tenant_id')`).Scan(&bad).Error
	if err != nil {
		return fmt.Errorf("services.TenantProvisionService.purgeSchemaDrift: %w", err)
	}
	if len(bad) > 0 {
		return &ValidationError{
			Code: "TENANT_PURGE_SCHEMA_DRIFT",
			Msg: "schema có bảng con mang dữ liệu tenant nhưng KHÔNG cascade khi xoá cha — " +
				"xoá bây giờ sẽ để lại dữ liệu mồ côi: " + strings.Join(bad, ", "),
			Details: map[string]any{"constraints": bad},
		}
	}

	// The check above is NEGATIVE — it can only see a table the schema connects to a tenant-scoped
	// parent by an FK. A table with NEITHER a tenant_id column NOR any FK is invisible to it, and
	// such a table could still hold per-customer rows (keyed by, say, a user id or an entity name).
	// So assert positively: every base table must be one of the three shapes we understand.
	var unknown []string
	if err := s.db.WithContext(ctx).Raw(`
		SELECT t.TABLE_NAME FROM information_schema.TABLES t
		WHERE t.TABLE_SCHEMA = DATABASE() AND t.TABLE_TYPE = 'BASE TABLE'
		  AND t.TABLE_NAME NOT IN (
		        SELECT TABLE_NAME FROM information_schema.COLUMNS
		        WHERE TABLE_SCHEMA = DATABASE() AND COLUMN_NAME = 'tenant_id')
		  AND t.TABLE_NAME NOT IN (
		        SELECT TABLE_NAME FROM information_schema.KEY_COLUMN_USAGE
		        WHERE TABLE_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME IS NOT NULL)
		  AND t.TABLE_NAME NOT IN (?)`, purgeUnscopedTables).Scan(&unknown).Error; err != nil {
		return fmt.Errorf("services.TenantProvisionService.purgeSchemaDrift: unscoped scan: %w", err)
	}
	if len(unknown) > 0 {
		return &ValidationError{
			Code: "TENANT_PURGE_SCHEMA_DRIFT",
			Msg: "schema có bảng không mang tenant_id và cũng không có khoá ngoại nào — không xác định " +
				"được nó có giữ dữ liệu khách hay không, nên không xoá: " + strings.Join(unknown, ", ") +
				". Thêm tên bảng vào purgeUnscopedTables sau khi đã kiểm tra, hoặc cho nó cột tenant_id.",
			Details: map[string]any{"tables": unknown},
		}
	}
	return nil
}

// purgeUnscopedTables is the REVIEWED allowlist of base tables that legitimately carry neither a
// tenant_id column nor an FK, and therefore hold nothing belonging to a customer:
//
//   - schema_migrations — golang-migrate's own version row;
//   - tenants           — the registry itself; the purge deletes its row by primary key, last;
//   - sys_unique_demote_log — an append-only record of migration 000051's index demotions,
//     keyed by entity+field, with no customer data in it.
//
// Adding a name here is a decision to be made ON PURPOSE, with the table opened first. The guard
// exists so that decision cannot be made by accident, which is what "the purge just didn't happen
// to touch it" would have been.
var purgeUnscopedTables = []string{"schema_migrations", "tenants", "sys_unique_demote_log"}
