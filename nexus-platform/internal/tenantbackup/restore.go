package tenantbackup

// restore.go — "Khôi phục về bản này" (specs/068-tenant-backup-restore slice D, FR-040..FR-072).
//
// Order is the contract (plan D-6): write freeze + settle -> self backup (the undo net) -> DDL ->
// attachment bytes -> ONE READ COMMITTED data transaction -> after-commit cleanup, Doctor, caches,
// unlock. The data transaction itself lives in restore_store.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/ddl"
	"nexus/platform/internal/files"
	"nexus/platform/internal/storage"
)

const (
	restoreLease       = 15 * time.Minute
	defaultRestoreTx   = 10 * time.Minute
	defaultSettle      = 7 * time.Second // 2 x freeze-cache TTL (2 s) + 3 s, plan D-16
	restoreStepLock    = "lock"
	restoreStepBackup  = "prebackup"
	restoreStepApply   = "apply"
	restoreStepDoctor  = "doctor"
	restoreRunning     = "running"
	restoreDone        = "done"
	restoreFailed      = "failed"
	restoreOpPhase     = "phase"
	errPrebackupFailed = "TENANT_RESTORE_PREBACKUP_FAILED"
	errRestoreFailed   = "TENANT_RESTORE_FAILED"
)

// RestoreFreezer is the write freeze a restore holds (services.TenantFreeze).
type RestoreFreezer interface {
	Freezer
	Extend(ctx context.Context, tenantID string, until time.Time) error
}

// RestoreDoctor is what a restore asks of the Doctor once the data is back (services.DoctorService
// through a small adapter in main.go).
type RestoreDoctor interface {
	// Repair runs Fix(A1, A5, A6, B4) then Scan and returns how many items were fixed and how many
	// critical findings remain.
	Repair(ctx context.Context, tenantID string) (fixed, remainingCritical int, err error)
	// FlushCaches drops the in-process metadata / ACL / workflow / user caches of the customer.
	FlushCaches(ctx context.Context, tenantID string)
}

// RestoreDeps wires the restore. InvalidateHead (optional) drops the draft/publish HEAD-cutoff cache.
type RestoreDeps struct {
	Store          RestoreStore
	Freezer        RestoreFreezer
	Doctor         RestoreDoctor
	InvalidateHead func(tenantID string)
	Settle         time.Duration // 0 = 7 s
	TxTimeout      time.Duration // 0 = 10 min
	Hooks          ApplyHooks
}

// WithRestore wires slice D. Without it StartRestore fails with an error instead of pretending.
func (s *Service) WithRestore(d RestoreDeps) *Service {
	if d.Settle == 0 {
		d.Settle = defaultSettle
	}
	if d.TxTimeout == 0 {
		d.TxTimeout = defaultRestoreTx
	}
	s.rd = &d
	return s
}

// RestoreInput is one "Khôi phục về bản này".
type RestoreInput struct {
	TenantID    string
	BackupID    string
	ConfirmSlug string
	Actor       Actor
}

// RestoreStarted is the 202 body.
type RestoreStarted struct {
	RestoreID string `json:"restoreId"`
	Status    string `json:"status"`
}

// RestoreReport is restore_report once a restore ends.
type RestoreReport struct {
	SourceBackupID     string            `json:"sourceBackupId"`
	PreRestoreID       string            `json:"preRestoreBackupId"`
	Deleted            map[string]int64  `json:"deleted,omitempty"`
	Inserted           map[string]int64  `json:"inserted,omitempty"`
	RolesLost          int               `json:"rolesLost"`
	MembershipsSkipped int               `json:"membershipsSkipped"`
	SharesSkipped      int               `json:"sharesSkipped"`
	WebhookKeysDropped int               `json:"webhookKeysDropped"`
	BlobsWritten       int               `json:"blobsWritten"`
	BlobsRemoved       int               `json:"blobsRemoved"`
	Doctor             *RestoreDoctorRep `json:"doctor,omitempty"`
	AccessChanges      *AccessChanges    `json:"accessChanges,omitempty"`
	ResumedAfterCrash  bool              `json:"resumedAfterCrash,omitempty"`
}

// RestoreDoctorRep is the Doctor's verdict after a restore.
type RestoreDoctorRep struct {
	Fixed             int `json:"fixed"`
	RemainingCritical int `json:"remainingCritical"`
}

// StartRestore validates the backup again, checks the typed slug, takes the per-customer operation
// lock by inserting the pre_restore row, and hands the work to the runner. Everything that can be
// refused is refused here, before the customer is frozen.
func (s *Service) StartRestore(ctx context.Context, in RestoreInput) (*RestoreStarted, error) {
	if s.rd == nil || s.plan == nil {
		return nil, errors.New("tenantbackup.Service.StartRestore: restore is not wired")
	}
	l, err := s.Validate(ctx, in.Actor.TenantID, in.TenantID, in.BackupID)
	if err != nil {
		return nil, err
	}
	handedOver := false
	defer func() {
		if !handedOver {
			l.close()
		}
	}()
	if strings.TrimSpace(in.ConfirmSlug) != l.tenant.Slug {
		return nil, &Error{Code: "TENANT_RESTORE_CONFIRM_MISMATCH", Msg: "Gõ đúng tên khách để xác nhận khôi phục",
			Details: map[string]any{"expected": l.tenant.Slug}}
	}
	// The disk is checked BEFORE the freeze: the self backup is a full backup (FR-070).
	if err := s.checkDisk(ctx, l.tenant.ID); err != nil {
		return nil, err
	}
	version, _, err := s.store.CurrentSchema(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.StartRestore: schema version: %w", err)
	}
	id := s.newID()
	key := l.tenant.ID + "/" + id + ".zip"
	src, run, step := l.backup.ID, restoreRunning, restoreStepLock
	note := "Tự sao lưu trước khi khôi phục"
	pre := &Backup{
		ID: id, TenantID: l.tenant.ID, Kind: KindPreRestore, Status: StatusRunning, Note: &note, StorageKey: &key,
		FormatVersion: FormatVersion, SchemaVersion: version, RestoreSourceID: &src, RestoreStatus: &run, RestoreStep: &step,
		ActorTenantID: strPtr(in.Actor.TenantID), CreatedBy: strPtr(in.Actor.UserID),
	}
	if err := s.store.Insert(ctx, pre); err != nil {
		if errors.Is(err, ErrInProgress) {
			return nil, &Error{Code: "BACKUP_IN_PROGRESS", Msg: "Khách này đang có một tác vụ sao lưu hoặc khôi phục chạy — chờ xong rồi thử lại"}
		}
		return nil, fmt.Errorf("tenantbackup.Service.StartRestore: insert: %w", err)
	}
	row, err := s.store.Get(ctx, l.tenant.ID, id)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.StartRestore: reload: %w", err)
	}
	label := s.store.OperatorLabel(ctx, in.Actor.TenantID, in.Actor.UserID)
	// phase: start is recorded BEFORE the freeze is set, so a crash at any later point leaves a trail (D-25).
	s.auditRestore(ctx, in.Actor, l.tenant, row, "start", nil, label)
	beat := func(bctx context.Context) bool {
		n, err := s.rd.Store.RestoreHeartbeat(bctx, l.tenant.ID, id)
		return err != nil || n > 0
	}
	handedOver = true
	if err := s.runner.Go(in.Actor.UserID, beat, func(jctx context.Context) { s.runRestore(jctx, l, row, in.Actor, label) }); err != nil {
		l.close()
		s.finishRestoreFailed(ctx, row, in.Actor, l.tenant, label, "BACKUP_CANCELLED", "máy chủ đang tắt")
		return nil, fmt.Errorf("tenantbackup.Service.StartRestore: schedule: %w", err)
	}
	s.logger.Warn("tenantbackup.Service.StartRestore: restore started",
		zap.String("tenant_id", l.tenant.ID), zap.String("backup_id", src), zap.String("restore_id", id),
		zap.String("operator_tenant_id", in.Actor.TenantID), zap.String("operator_user_id", in.Actor.UserID))
	return &RestoreStarted{RestoreID: id, Status: restoreRunning}, nil
}

// auditRestore writes one ActionRestore row under the operator's tenant AND the customer's. The
// customer-side diff carries a human label (after.by), never the workshop's ids (D-25).
func (s *Service) auditRestore(ctx context.Context, actor Actor, t *TenantInfo, pre *Backup, phase string, extra map[string]any, label string) {
	at := s.now().UTC()
	src := deref(pre.RestoreSourceID)
	after := map[string]any{restoreOpPhase: phase, "backupId": src, "preRestoreBackupId": pre.ID}
	for k, v := range extra {
		after[k] = v
	}
	if actor.TenantID != "" && actor.TenantID != t.ID {
		a := map[string]any{"tenantId": t.ID, "slug": t.Slug}
		for k, v := range after {
			a[k] = v
		}
		s.audit.Record(ctx, audit.Event{TenantID: actor.TenantID, ActorUserID: actor.UserID, EntityName: audit.TenantBackupMeta,
			RecordID: pre.ID, Action: audit.ActionRestore, Diff: &audit.Diff{After: a}, At: at})
	}
	c := map[string]any{"by": label}
	for k, v := range after {
		c[k] = v
	}
	s.audit.Record(ctx, audit.Event{TenantID: t.ID, ActorUserID: actorOrSystem(actor.UserID), EntityName: audit.TenantBackupMeta,
		RecordID: pre.ID, Action: audit.ActionRestore, Diff: &audit.Diff{After: c}, At: at})
}

// runRestore is the runner job. It never panics out: a panic ends as a failed restore.
func (s *Service) runRestore(ctx context.Context, l *loaded, pre *Backup, actor Actor, label string) {
	defer l.close()
	t := l.tenant
	rd := s.rd
	fail := func(err error) {
		code, detail := errRestoreFailed, ""
		var be *Error
		switch {
		case errors.As(err, &be):
			code = be.Code
		case ctx.Err() != nil:
			code = "BACKUP_CANCELLED"
		}
		var se *stepError
		if errors.As(err, &se) {
			detail = se.detail()
		}
		s.logger.Error("tenantbackup.Service.runRestore: restore failed", zap.String("tenant_id", t.ID),
			zap.String("restore_id", pre.ID), zap.String("code", code), zap.Error(err))
		s.finishRestoreFailed(ctx, pre, actor, t, label, code, detail)
	}
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Error("tenantbackup.Service.runRestore: panic", zap.String("tenant_id", t.ID), zap.Any("panic", rec))
			fail(fmt.Errorf("tenantbackup.Service.runRestore: panic"))
		}
	}()

	// 1. freeze + settle.
	prev, err := rd.Freezer.Set(ctx, t.ID, s.now().Add(restoreLease), "restore")
	if err != nil {
		fail(fmt.Errorf("tenantbackup.Service.runRestore: freeze: %w", err))
		return
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if cerr := rd.Freezer.Clear(cctx, t.ID, prev); cerr != nil {
			s.logger.Error("tenantbackup.Service.runRestore: could not lift the write freeze (it expires by itself)",
				zap.String("tenant_id", t.ID), zap.Error(cerr))
		}
	}()
	if err := sleepCtx(ctx, rd.Settle); err != nil {
		fail(err)
		return
	}
	extend := func() error {
		return rd.Freezer.Extend(ctx, t.ID, s.now().Add(restoreLease))
	}

	// 2. self backup into the pre_restore row itself (no second semaphore: we already hold it).
	if err := s.setStep(ctx, pre, restoreStepBackup); err != nil {
		fail(err)
		return
	}
	if err := s.safeWrite(ctx, pre, label); err != nil {
		s.logger.Error("tenantbackup.Service.runRestore: self backup failed", zap.String("tenant_id", t.ID), zap.Error(err))
		s.failRow(ctx, pre, "BACKUP_WRITE_FAILED", "")
		s.finishRestoreFailed(ctx, pre, actor, t, label, errPrebackupFailed, "")
		return
	}
	if err := extend(); err != nil {
		fail(fmt.Errorf("tenantbackup.Service.runRestore: extend freeze: %w", err))
		return
	}

	// 3-5. DDL, blobs, data transaction.
	if err := s.setStep(ctx, pre, restoreStepApply); err != nil {
		fail(err)
		return
	}
	if err := s.ensureTables(ctx, l); err != nil {
		fail(err)
		return
	}
	written, err := s.restoreBlobs(ctx, l)
	if err != nil {
		s.removeBlobs(ctx, t.ID, written)
		fail(err)
		return
	}
	// The lease covers the whole transaction; it is the last write to `tenants` before COMMIT (D-17).
	if err := rd.Freezer.Extend(ctx, t.ID, s.now().Add(rd.TxTimeout+5*time.Minute)); err != nil {
		s.removeBlobs(ctx, t.ID, written)
		fail(fmt.Errorf("tenantbackup.Service.runRestore: extend freeze: %w", err))
		return
	}
	res, err := rd.Store.ApplyRestore(ctx, ApplyInput{TenantID: t.ID, RestoreID: pre.ID, Reader: l.reader, Manifest: l.manifest,
		TxTimeout: rd.TxTimeout, Hooks: rd.Hooks})
	if res != nil && res.Committed {
		if err != nil {
			// Data is already restored: leave the row `running` with its commit marker; the reaper finishes it (FR-071).
			s.logger.Error("tenantbackup.Service.runRestore: stopped after commit; the reaper will finish it",
				zap.String("tenant_id", t.ID), zap.String("restore_id", pre.ID), zap.Error(err))
			return
		}
	} else if err != nil {
		s.removeBlobs(ctx, t.ID, written)
		fail(err)
		return
	}

	// 6. after commit.
	rep := &RestoreReport{SourceBackupID: l.backup.ID, PreRestoreID: pre.ID, Deleted: res.Deleted, Inserted: res.Inserted,
		RolesLost: res.RolesLost, MembershipsSkipped: res.MembershipsSkipped, SharesSkipped: res.SharesSkipped,
		WebhookKeysDropped: res.WebhookKeysDropped, BlobsWritten: len(written)}
	rep.AccessChanges = s.accessAfter(ctx, l, pre)
	s.finishAfterCommit(ctx, pre, actor, t, label, res.PendingBlobRemoval, rep, "done")
}

// finishAfterCommit is steps 6 of the plan — shared by the runner and the reaper (FR-071).
func (s *Service) finishAfterCommit(ctx context.Context, pre *Backup, actor Actor, t *TenantInfo, label string,
	pending []string, rep *RestoreReport, phase string) {
	rd := s.rd
	if err := s.setStep(ctx, pre, restoreStepDoctor); err != nil {
		s.logger.Warn("tenantbackup.Service.finishAfterCommit: step", zap.String("tenant_id", t.ID), zap.Error(err))
	}
	rep.BlobsRemoved = s.removeBlobs(ctx, t.ID, pending)
	if rd.Doctor != nil {
		fixed, remaining, err := rd.Doctor.Repair(ctx, t.ID)
		if err != nil {
			s.logger.Error("tenantbackup.Service.finishAfterCommit: doctor", zap.String("tenant_id", t.ID), zap.Error(err))
		} else {
			rep.Doctor = &RestoreDoctorRep{Fixed: fixed, RemainingCritical: remaining}
		}
		rd.Doctor.FlushCaches(ctx, t.ID)
	}
	if rd.InvalidateHead != nil {
		rd.InvalidateHead(t.ID)
	}
	body, err := json.Marshal(rep)
	if err != nil {
		s.logger.Error("tenantbackup.Service.finishAfterCommit: report", zap.Error(err))
		body = nil
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := rd.Store.FinishRestore(fctx, t.ID, pre.ID, restoreDone, body, "", ""); err != nil {
		s.logger.Error("tenantbackup.Service.finishAfterCommit: finish", zap.String("tenant_id", t.ID), zap.Error(err))
	}
	extra := map[string]any{"rows": rep.Inserted}
	if rep.AccessChanges != nil {
		extra["accessChanges"] = rep.AccessChanges
	}
	s.auditRestore(fctx, actor, t, pre, phase, extra, label)
	s.logger.Warn("tenantbackup.Service.finishAfterCommit: restore done", zap.String("tenant_id", t.ID), zap.String("restore_id", pre.ID))
}

func (s *Service) setStep(ctx context.Context, pre *Backup, step string) error {
	if err := s.rd.Store.SetRestoreStep(ctx, pre.TenantID, pre.ID, step); err != nil {
		return fmt.Errorf("tenantbackup.Service.setStep %s: %w", step, err)
	}
	return nil
}

// finishRestoreFailed ends a restore that changed nothing (or whose transaction rolled back).
func (s *Service) finishRestoreFailed(ctx context.Context, pre *Backup, actor Actor, t *TenantInfo, label, code, detail string) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	// op_lock keys on status='running' too: a failure before the self backup finished must free the row.
	if _, err := s.store.MarkFailed(fctx, t.ID, pre.ID, code, detail); err != nil {
		s.logger.Error("tenantbackup.Service.finishRestoreFailed: mark failed", zap.String("tenant_id", t.ID), zap.Error(err))
	}
	if _, err := s.rd.Store.FinishRestore(fctx, t.ID, pre.ID, restoreFailed, nil, code, detail); err != nil {
		s.logger.Error("tenantbackup.Service.finishRestoreFailed: finish", zap.String("tenant_id", t.ID), zap.Error(err))
	}
	s.auditRestore(fctx, actor, t, pre, "failed", map[string]any{"code": code}, label)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("tenantbackup.sleepCtx: %w", ctx.Err())
	}
}

// ensureTables recreates the obj_/rel_ tables the backup holds and the database lost (step 3). The
// statement comes from the metadata INSIDE the backup through the same builders the engine uses;
// the name they produce must equal the table the file names, else SCHEMA_DRIFT.
func (s *Service) ensureTables(ctx context.Context, l *loaded) error {
	var dyn []string
	for _, mt := range l.manifest.Tables {
		if IsDynamic(mt.Name) {
			dyn = append(dyn, mt.Name)
		}
	}
	if len(dyn) == 0 {
		return nil
	}
	have, err := s.plan.TableColumns(ctx, dyn)
	if err != nil {
		return fmt.Errorf("tenantbackup.Service.ensureTables: %w", err)
	}
	var missing []string
	for _, n := range dyn {
		if _, ok := have[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	entities := map[string]string{} // table_name -> entity name
	if err := l.reader.eachRow("sys_entities", func(m map[string]json.RawMessage) {
		entities[rawString(m, "table_name")] = rawString(m, "name")
	}); err != nil {
		return fmt.Errorf("tenantbackup.Service.ensureTables: entities: %w", err)
	}
	type pair struct{ a, b string }
	joins := map[string]pair{}
	if err := l.reader.eachRow("sys_relationships", func(m map[string]json.RawMessage) {
		if j := rawString(m, "join_table_name"); j != "" {
			joins[j] = pair{rawString(m, "parent_entity_name"), rawString(m, "child_entity_name")}
		}
	}); err != nil {
		return fmt.Errorf("tenantbackup.Service.ensureTables: relationships: %w", err)
	}
	for _, name := range missing {
		var stmt string
		if strings.HasPrefix(name, "obj_") {
			ent, ok := entities[name]
			if !ok {
				return driftErr("Không có mô tả để dựng lại bảng "+name, map[string]any{"table": name})
			}
			if stmt, err = ddl.BuildCreateTable(ent, nil); err != nil {
				return driftErr("Không dựng lại được bảng "+name, map[string]any{"table": name})
			}
		} else {
			j, ok := joins[name]
			if !ok {
				return driftErr("Không có mô tả để dựng lại bảng "+name, map[string]any{"table": name})
			}
			if stmt, err = ddl.BuildCreateJoinTable(j.a, j.b); err != nil {
				return driftErr("Không dựng lại được bảng "+name, map[string]any{"table": name})
			}
		}
		if !strings.Contains(stmt, "`"+name+"`") {
			return driftErr("Tên bảng dựng lại khác tên trong bản sao lưu: "+name, map[string]any{"table": name})
		}
		if err := s.rd.Store.ExecDDL(ctx, stmt); err != nil {
			return fmt.Errorf("tenantbackup.Service.ensureTables: %w", err)
		}
	}
	return nil
}

// restoreBlobs puts back the attachment bytes the backup holds and the disk lacks (step 4). Only
// ids that the backup's own sys_files lists for THIS customer are written, and only under
// files.StorageKey(tenant, id) (plan D-26). Returns the ids written that no live row owns, so a
// failed restore can remove them again.
func (s *Service) restoreBlobs(ctx context.Context, l *loaded) ([]string, error) {
	tid := l.tenant.ID
	listed := map[string]bool{}
	if err := l.reader.eachRow("sys_files", func(m map[string]json.RawMessage) {
		if rawString(m, "tenant_id") == tid {
			listed[rawString(m, "id")] = true
		}
	}); err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.restoreBlobs: %w", err)
	}
	live, err := s.plan.LiveFiles(ctx, tid)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.restoreBlobs: live files: %w", err)
	}
	var written []string
	for _, id := range l.reader.BlobIDs() {
		if !listed[id] || !uuidRe.MatchString(id) {
			continue
		}
		key := files.StorageKey(tid, id)
		if rc, err := s.blobs.Open(ctx, key); err == nil {
			_ = rc.Close() // ids are immutable: the bytes already there are the bytes of the backup
			continue
		} else if !errors.Is(err, storage.ErrNotFound) {
			return written, &stepError{step: "blobs", err: err}
		}
		src, err := l.reader.Blob(id)
		if err != nil {
			return written, err
		}
		_, perr := s.blobs.Put(ctx, key, src)
		_ = src.Close()
		if perr != nil {
			return written, &stepError{step: "blobs", err: perr}
		}
		if _, isLive := live[id]; !isLive {
			written = append(written, id)
		}
	}
	return written, nil
}

// removeBlobs deletes attachment bytes by id — always files.StorageKey(tenant, id), never a stored
// path (D-26). Best effort; returns how many were removed.
func (s *Service) removeBlobs(ctx context.Context, tenantID string, ids []string) int {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	n := 0
	for _, id := range ids {
		if !uuidRe.MatchString(id) {
			continue
		}
		if err := s.blobs.Delete(dctx, files.StorageKey(tenantID, id)); err != nil {
			s.logger.Warn("tenantbackup.Service.removeBlobs: could not remove a blob",
				zap.String("tenant_id", tenantID), zap.String("file_id", id), zap.Error(err))
			continue
		}
		n++
	}
	return n
}

// accessAfter = DiffAccess(source backup, the self backup just taken): what the restore widened
// (D-24). Best effort — a read failure leaves the field out and is logged.
func (s *Service) accessAfter(ctx context.Context, l *loaded, pre *Backup) *AccessChanges {
	if pre.StorageKey == nil {
		return nil
	}
	ra, size, _, release, err := s.openZip(ctx, *pre.StorageKey)
	if err != nil {
		s.logger.Warn("tenantbackup.Service.accessAfter: open self backup", zap.String("tenant_id", pre.TenantID), zap.Error(err))
		return nil
	}
	defer release()
	r, err := OpenReader(ra, size)
	if err != nil {
		s.logger.Warn("tenantbackup.Service.accessAfter: read self backup", zap.String("tenant_id", pre.TenantID), zap.Error(err))
		return nil
	}
	src, _, err := readBackupFacts(l.reader)
	if err != nil {
		s.logger.Warn("tenantbackup.Service.accessAfter: read source", zap.String("tenant_id", pre.TenantID), zap.Error(err))
		return nil
	}
	cur, _, err := readBackupFacts(r)
	if err != nil {
		s.logger.Warn("tenantbackup.Service.accessAfter: read self backup facts", zap.String("tenant_id", pre.TenantID), zap.Error(err))
		return nil
	}
	ac := DiffAccess(src, cur)
	return &ac
}

// reapRestores finishes the restores whose process died (FR-071): one that already committed runs
// its after-commit steps again; one that did not is failed (MySQL rolled its transaction back).
func (s *Service) reapRestores(ctx context.Context) (int, error) {
	if s.rd == nil {
		return 0, nil
	}
	stale, err := s.rd.Store.ListStaleRestores(ctx, staleAfter)
	if err != nil {
		return 0, fmt.Errorf("tenantbackup.Service.reapRestores: %w", err)
	}
	n := 0
	for _, r := range stale {
		t, terr := s.store.TenantInfo(ctx, r.TenantID)
		if terr != nil {
			s.logger.Error("tenantbackup.Service.reapRestores: tenant", zap.String("tenant_id", r.TenantID), zap.Error(terr))
			continue
		}
		actor := Actor{TenantID: r.ActorTenantID, UserID: r.CreatedBy}
		label := s.store.OperatorLabel(ctx, actor.TenantID, actor.UserID)
		pre := &Backup{ID: r.ID, TenantID: r.TenantID, StorageKey: strPtr(r.StorageKey), RestoreSourceID: strPtr(r.SourceID)}
		if err := s.rd.Freezer.Clear(ctx, r.TenantID, nil); err != nil {
			s.logger.Warn("tenantbackup.Service.reapRestores: unfreeze", zap.String("tenant_id", r.TenantID), zap.Error(err))
		}
		if r.CommittedAt != nil {
			var marker struct {
				Pending []string `json:"pendingBlobRemovals"`
			}
			_ = json.Unmarshal(r.Report, &marker) // a missing marker only means nothing to remove
			rep := &RestoreReport{SourceBackupID: r.SourceID, PreRestoreID: r.ID, ResumedAfterCrash: true}
			s.finishAfterCommit(ctx, pre, actor, t, label, marker.Pending, rep, "resumed")
		} else {
			if r.BackupStatus == StatusRunning {
				s.removeFile(ctx, pre)
				if _, err := s.store.MarkFailed(ctx, r.TenantID, r.ID, "BACKUP_ABANDONED", "bước dọn dẹp: không có nhịp tim quá 30 phút"); err != nil {
					s.logger.Error("tenantbackup.Service.reapRestores: mark backup failed", zap.String("tenant_id", r.TenantID), zap.Error(err))
				}
			}
			s.finishRestoreFailed(ctx, pre, actor, t, label, "BACKUP_ABANDONED", "bước dọn dẹp: không có nhịp tim quá 30 phút")
		}
		n++
	}
	return n, nil
}

// ReattachWebhookKeys copies `secret` and `headers` of the outbound_webhook actions of the CURRENT
// rule onto the restored actions, matched by trimmed URL in order (D-21). A restored webhook with
// no counterpart keeps no key and is counted: a revoked or rotated key is never brought back.
func ReattachWebhookKeys(restored string, current []byte) (string, int, error) {
	var neu []any
	if err := decodeJSONNumber([]byte(restored), &neu); err != nil {
		return "", 0, fmt.Errorf("tenantbackup.ReattachWebhookKeys: restored actions: %w", err)
	}
	var cur []any
	if len(bytes.TrimSpace(current)) > 0 {
		if err := decodeJSONNumber(current, &cur); err != nil {
			cur = nil // an unreadable live rule has no key to give
		}
	}
	used := make([]bool, len(cur))
	dropped, changed := 0, false
	for _, e := range neu {
		m, ok := e.(map[string]any)
		if !ok || m["type"] != "outbound_webhook" {
			continue
		}
		url, _ := m["url"].(string)
		url = strings.TrimSpace(url)
		found := false
		for i, c := range cur {
			cm, ok := c.(map[string]any)
			if !ok || used[i] || cm["type"] != "outbound_webhook" {
				continue
			}
			if cu, _ := cm["url"].(string); strings.TrimSpace(cu) != url {
				continue
			}
			used[i], found = true, true
			for _, k := range []string{"secret", "headers"} {
				if v, has := cm[k]; has {
					m[k], changed = v, true
				}
			}
			break
		}
		if !found {
			dropped++
		}
	}
	if !changed {
		return restored, dropped, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(neu); err != nil {
		return "", 0, fmt.Errorf("tenantbackup.ReattachWebhookKeys: encode: %w", err)
	}
	return strings.TrimRight(buf.String(), "\n"), dropped, nil
}

func decodeJSONNumber(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // keep 18446744073709551615 exact
	return dec.Decode(v)
}
