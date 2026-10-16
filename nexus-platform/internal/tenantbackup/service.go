package tenantbackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/files"
	"nexus/platform/internal/storage"
)

const (
	maxNoteRunes = 200
	// staleAfter: a running backup with no heartbeat for this long is failed by the reaper.
	staleAfter = 30 * time.Minute
	// heartbeatEveryRows bounds how long a huge table goes without a heartbeat.
	heartbeatEveryRows = 20000
	maxSampleIDs       = 10
	maxManifestIDs     = 10000
	defaultListLimit   = 20
	maxListLimit       = 100
)

// Config is the part of the backup settings the service needs.
type Config struct {
	// Dir is the TENANT_BACKUP_DIR root (the disk the free-space check measures).
	Dir string
	// MinFreeBytes is the reserve a backup must leave free on top of its estimate (FR-070).
	MinFreeBytes int64
}

// Actor is the operator who triggered an action: the tenant they stand in (the workshop) and
// their user id.
type Actor struct{ TenantID, UserID string }

// Service creates, lists, reads and serves per-customer backups. It knows nothing of HTTP: the
// handler maps *Error to a status. It imports neither services nor net/http.
type Service struct {
	store          Store
	snaps          SnapshotSource
	blobs          storage.Backend // attachment bytes (FILE_STORAGE_DIR)
	backups        storage.Backend // backup zips (TENANT_BACKUP_DIR)
	runner         *Runner
	plan           PlanStore // read side of the restore preview (WithPlan)
	maxRestoreRows int
	audit          audit.Auditor
	freezer        Freezer      // write-freeze control for the manual maintenance routes (WithFreezer)
	rd             *RestoreDeps // slice D (WithRestore)
	sched          ScheduleStore
	schedCfg       ScheduleConfig
	cfg            Config
	logger         *zap.Logger

	// seams for tests
	freeBytes func(dir string) (uint64, error)
	now       func() time.Time
	newID     func() string
}

// NewService wires the service. aud and logger may be nil.
func NewService(st Store, snaps SnapshotSource, blobs, backups storage.Backend, runner *Runner,
	aud audit.Auditor, cfg Config, logger *zap.Logger) *Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	if aud == nil {
		aud = audit.NewNoopAuditor()
	}
	return &Service{
		store: st, snaps: snaps, blobs: blobs, backups: backups, runner: runner, audit: aud, cfg: cfg, logger: logger,
		freeBytes: diskFreeBytes, now: time.Now, newID: uuid.NewString,
	}
}

// CreateInput is one "Sao lưu ngay".
type CreateInput struct {
	TenantID string
	Note     string
	Actor    Actor
}

func tenantNotFound() *Error {
	return &Error{Code: "TENANT_NOT_FOUND", Msg: "không tìm thấy khách"}
}

func backupNotFound() *Error {
	return &Error{Code: "BACKUP_NOT_FOUND", Msg: "không tìm thấy bản sao lưu"}
}

// validateNote returns the trimmed note (nil when empty) or BACKUP_NOTE_INVALID.
func validateNote(note string) (*string, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(note) > maxNoteRunes {
		return nil, &Error{Code: "BACKUP_NOTE_INVALID", Msg: "ghi chú tối đa 200 ký tự"}
	}
	for _, r := range note {
		if unicode.IsControl(r) {
			return nil, &Error{Code: "BACKUP_NOTE_INVALID", Msg: "ghi chú không được chứa ký tự điều khiển"}
		}
	}
	return &note, nil
}

// Create starts a manual backup of one customer and returns the new catalogue row immediately
// (status running); the zip is written by the runner. The order is the contract:
// tenant exists (a retired one too) -> schema classified -> enough disk -> INSERT. The INSERT is
// where the database ENFORCES one running operation per customer (uq_tbk_oplock).
func (s *Service) Create(ctx context.Context, in CreateInput) (*BackupView, error) {
	v, _, err := s.start(ctx, in, KindManual, "")
	return v, err
}

// start is Create for any kind. slot is the schedule slot of a scheduled backup ("" = none): the
// INSERT is where uq_tbk_slot makes two replicas produce one row (ErrSlotTaken). The returned
// channel closes when the runner is finished with the backup (ready, failed or abandoned); the
// schedule's sweep waits on it, Create ignores it.
func (s *Service) start(ctx context.Context, in CreateInput, kind, slot string) (*BackupView, <-chan struct{}, error) {
	note, err := validateNote(in.Note)
	if err != nil {
		return nil, nil, err
	}
	t, err := s.store.TenantInfo(ctx, in.TenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			return nil, nil, tenantNotFound()
		}
		return nil, nil, fmt.Errorf("tenantbackup.Service.Create: tenant: %w", err)
	}
	if err := s.store.CheckSchema(ctx); err != nil {
		return nil, nil, fmt.Errorf("tenantbackup.Service.Create: schema: %w", err)
	}
	if err := s.checkDisk(ctx, t.ID); err != nil {
		return nil, nil, err
	}
	version, _, err := s.store.CurrentSchema(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("tenantbackup.Service.Create: schema version: %w", err)
	}

	id := s.newID()
	key := t.ID + "/" + id + ".zip"
	actorTenant, actorUser := in.Actor.TenantID, in.Actor.UserID
	b := &Backup{
		ID: id, TenantID: t.ID, Kind: kind, Status: StatusRunning, Note: note, StorageKey: &key,
		FormatVersion: FormatVersion, SchemaVersion: version, ActorTenantID: strPtr(actorTenant), CreatedBy: strPtr(actorUser),
		ScheduleSlot: strPtr(slot),
	}
	if err := s.store.Insert(ctx, b); err != nil {
		if errors.Is(err, ErrInProgress) {
			return nil, nil, &Error{Code: "BACKUP_IN_PROGRESS", Msg: "Khách này đang có một tác vụ sao lưu hoặc khôi phục chạy — chờ xong rồi thử lại"}
		}
		if errors.Is(err, ErrSlotTaken) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("tenantbackup.Service.Create: insert: %w", err)
	}
	row, err := s.store.Get(ctx, t.ID, id)
	if err != nil {
		return nil, nil, fmt.Errorf("tenantbackup.Service.Create: reload: %w", err)
	}

	label := SystemActorLabel // the schedule has no operator
	if actorUser != "" {
		label = s.store.OperatorLabel(ctx, actorTenant, actorUser)
	}
	done := make(chan struct{})
	finish := sync.OnceFunc(func() { close(done) })
	beat := func(bctx context.Context) bool {
		n, err := s.store.Heartbeat(bctx, t.ID, id)
		alive := err != nil || n > 0 // a failing beat is not proof the row is gone
		if !alive {
			finish()
		}
		return alive
	}
	if err := s.runner.Go(actorOrSystem(actorUser), beat, func(jctx context.Context) { defer finish(); s.run(jctx, row, label) }); err != nil {
		s.failRow(ctx, row, "BACKUP_CANCELLED", "máy chủ đang tắt")
		return nil, nil, fmt.Errorf("tenantbackup.Service.Create: schedule: %w", err)
	}
	s.auditBoth(ctx, in.Actor, t, row, audit.ActionCreate, label)
	s.logger.Info("tenantbackup.Service.Create: backup started",
		zap.String("tenant_id", t.ID), zap.String("backup_id", id), zap.String("kind", kind), zap.String("slot", slot),
		zap.String("operator_tenant_id", actorTenant), zap.String("operator_user_id", actorUser))
	notReady := reasonNotReady
	v := s.view(row, &notReady, false, map[[2]string]string{{actorTenant, actorUser}: label})
	return &v, done, nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// checkDisk refuses a backup that cannot fit (FR-070): the backup directory shares a disk with
// MySQL, and filling it breaks writes for EVERY customer, not just this one.
func (s *Service) checkDisk(ctx context.Context, tenantID string) error {
	est, err := s.store.Estimate(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("tenantbackup.Service.checkDisk: estimate: %w", err)
	}
	free, err := s.freeBytes(s.cfg.Dir)
	if err != nil {
		// Unknown free space is not a reason to refuse every backup; say so in the log.
		s.logger.Warn("tenantbackup.Service.checkDisk: free space unknown, check skipped", zap.Error(err))
		return nil
	}
	need := est + s.cfg.MinFreeBytes
	if need < 0 || free < uint64(need) {
		return &Error{
			Code:    "BACKUP_DISK_LOW",
			Msg:     "Không đủ dung lượng trống để sao lưu an toàn",
			Details: map[string]any{"needBytes": need, "freeBytes": free},
		}
	}
	return nil
}

// ---- Reading ---------------------------------------------------------------------------------

// ListInput is one page request.
type ListInput struct {
	TenantID       string
	CallerTenantID string
	Limit          int
	After          string
}

// ListMeta is the envelope's meta.
type ListMeta struct {
	CurrentSchemaVersion int64      `json:"currentSchemaVersion"`
	MaintenanceUntil     *time.Time `json:"maintenanceUntil"`
	NextCursor           *string    `json:"nextCursor"`
}

// List returns one customer's backups, newest first.
func (s *Service) List(ctx context.Context, in ListInput) ([]BackupView, ListMeta, error) {
	t, err := s.store.TenantInfo(ctx, in.TenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			return nil, ListMeta{}, tenantNotFound()
		}
		return nil, ListMeta{}, fmt.Errorf("tenantbackup.Service.List: tenant: %w", err)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	var after *Cursor
	if in.After != "" {
		if after, err = DecodeCursor(in.After); err != nil {
			return nil, ListMeta{}, err
		}
	}
	rows, err := s.store.List(ctx, t.ID, limit+1, after)
	if err != nil {
		return nil, ListMeta{}, fmt.Errorf("tenantbackup.Service.List: %w", err)
	}
	meta := ListMeta{}
	if len(rows) > limit {
		rows = rows[:limit]
		c := Cursor{CreatedAt: rows[limit-1].CreatedAt, ID: rows[limit-1].ID}.Encode()
		meta.NextCursor = &c
	}
	version, _, err := s.store.CurrentSchema(ctx)
	if err != nil {
		return nil, ListMeta{}, fmt.Errorf("tenantbackup.Service.List: schema version: %w", err)
	}
	meta.CurrentSchemaVersion = version
	if s.freezer != nil {
		// A read error is logged by the checker and leaves the field empty: the list itself must not fail.
		if until, frozen, _ := s.freezer.FrozenUntil(ctx, t.ID); frozen {
			meta.MaintenanceUntil = &until
		}
	}

	labels := s.labelsFor(ctx, rows)
	out := make([]BackupView, 0, len(rows))
	for i := range rows {
		b := &rows[i]
		ok, reason := s.restorable(ctx, b, t, in.CallerTenantID, version)
		out = append(out, s.view(b, reason, ok, labels))
	}
	return out, meta, nil
}

// Get returns one backup of one customer (the poll target).
func (s *Service) Get(ctx context.Context, tenantID, callerTenantID, id string) (*BackupView, error) {
	t, err := s.store.TenantInfo(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			return nil, tenantNotFound()
		}
		return nil, fmt.Errorf("tenantbackup.Service.Get: tenant: %w", err)
	}
	b, err := s.getBackup(ctx, t.ID, id)
	if err != nil {
		return nil, err
	}
	version, _, err := s.store.CurrentSchema(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.Get: schema version: %w", err)
	}
	ok, reason := s.restorable(ctx, b, t, callerTenantID, version)
	v := s.view(b, reason, ok, s.labelsFor(ctx, []Backup{*b}))
	return &v, nil
}

// getBackup pins (id, tenant_id): another customer's backup id is "not found", never "forbidden".
func (s *Service) getBackup(ctx context.Context, tenantID, id string) (*Backup, error) {
	if !uuidRe.MatchString(id) {
		return nil, backupNotFound()
	}
	b, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, backupNotFound()
		}
		return nil, fmt.Errorf("tenantbackup.Service.getBackup: %w", err)
	}
	return b, nil
}

func (s *Service) labelsFor(ctx context.Context, rows []Backup) map[[2]string]string {
	var pairs [][2]string
	for _, b := range rows {
		if b.CreatedBy != nil && b.ActorTenantID != nil {
			pairs = append(pairs, [2]string{*b.ActorTenantID, *b.CreatedBy})
		}
	}
	return s.store.PersonLabels(ctx, pairs)
}

const (
	reasonSelf     = "SELF"
	reasonRetired  = "TENANT_RETIRED"
	reasonNotReady = "NOT_READY"
	reasonSchema   = "SCHEMA_MISMATCH"
	reasonFile     = "FILE_MISSING"
)

// restorable is the cheap "could a restore start from this?" answer for the list (no hashing).
func (s *Service) restorable(ctx context.Context, b *Backup, t *TenantInfo, callerTenantID string, currentVersion int64) (bool, *string) {
	reason := func(r string) (bool, *string) { return false, &r }
	switch {
	case t.ID == callerTenantID:
		return reason(reasonSelf)
	case t.Retired:
		return reason(reasonRetired)
	case b.Status != StatusReady:
		return reason(reasonNotReady)
	case b.SchemaVersion != currentVersion:
		return reason(reasonSchema)
	}
	if b.StorageKey == nil {
		return reason(reasonFile)
	}
	rc, err := s.backups.Open(ctx, *b.StorageKey)
	if err != nil {
		return reason(reasonFile)
	}
	_ = rc.Close()
	return true, nil
}

func (s *Service) view(b *Backup, reason *string, restorable bool, labels map[[2]string]string) BackupView {
	v := BackupView{
		ID: b.ID, TenantID: b.TenantID, Kind: b.Kind, Status: b.Status, Note: b.Note, SizeBytes: b.SizeBytes,
		SHA256: b.SHA256, SchemaVersion: b.SchemaVersion, FormatVersion: b.FormatVersion, RowCount: b.RowCount,
		TableCount: b.TableCount, FileCount: b.FileCount, BlobBytes: b.BlobBytes, Warnings: []Warning{},
		Error: errorView(b.ErrorCode), CreatedAt: b.CreatedAt, FinishedAt: b.FinishedAt,
		Restorable: restorable, NotRestorableReason: reason,
	}
	if len(b.Warnings) > 0 {
		_ = json.Unmarshal(b.Warnings, &v.Warnings) // a malformed column degrades to "no warnings"
	}
	if b.CreatedBy != nil {
		label := ""
		if b.ActorTenantID != nil {
			label = labels[[2]string{*b.ActorTenantID, *b.CreatedBy}]
		}
		if label == "" {
			label = genericOperatorLabel
		}
		v.CreatedBy = &PersonView{ID: *b.CreatedBy, Label: label}
	}
	if b.RestoreStatus != nil {
		rv := &RestoreView{Status: *b.RestoreStatus, Step: b.RestoreStep, CommittedAt: b.RestoreCommittedAt, FinishedAt: b.RestoreFinishedAt}
		if b.RestoreSourceID != nil {
			rv.SourceBackupID = *b.RestoreSourceID
		}
		if len(b.RestoreReport) > 0 {
			r := b.RestoreReport
			rv.Report = &r
		}
		v.Restore = rv
	}
	return v
}

// ---- Download --------------------------------------------------------------------------------

// FileDownload is an open backup file ready to be streamed.
type FileDownload struct {
	Name string
	Size int64
	Body io.ReadCloser
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9-]`)

// FileName is nexus-<slug>-<YYYYMMDD-HHMMSS>-<kind>.zip, the slug reduced to [a-z0-9-] so the name
// is safe in a Content-Disposition header.
func FileName(slug string, createdAt time.Time, kind string) string {
	slug = slugUnsafe.ReplaceAllString(strings.ToLower(slug), "-")
	return "nexus-" + slug + "-" + createdAt.UTC().Format("20060102-150405") + "-" + slugUnsafe.ReplaceAllString(kind, "-") + ".zip"
}

// OpenFile opens a READY backup of this customer for download and audits the download BEFORE any
// byte is streamed. A running / failed one is 409 BACKUP_NOT_READY, a file gone from disk 409
// BACKUP_FILE_MISSING.
func (s *Service) OpenFile(ctx context.Context, tenantID string, actor Actor, id string) (*FileDownload, error) {
	t, err := s.store.TenantInfo(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			return nil, tenantNotFound()
		}
		return nil, fmt.Errorf("tenantbackup.Service.OpenFile: tenant: %w", err)
	}
	b, err := s.getBackup(ctx, t.ID, id)
	if err != nil {
		return nil, err
	}
	if b.Status != StatusReady || b.StorageKey == nil {
		return nil, &Error{Code: "BACKUP_NOT_READY", Msg: "Bản sao lưu chưa sẵn sàng để tải"}
	}
	rc, err := s.backups.Open(ctx, *b.StorageKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, &Error{Code: "BACKUP_FILE_MISSING", Msg: "Tệp sao lưu không còn trên máy chủ"}
		}
		return nil, fmt.Errorf("tenantbackup.Service.OpenFile: open: %w", err)
	}
	size := int64(-1)
	if b.SizeBytes != nil {
		size = *b.SizeBytes
	}
	label := s.store.OperatorLabel(ctx, actor.TenantID, actor.UserID)
	s.auditBoth(ctx, actor, t, b, audit.ActionDownload, label)
	s.logger.Warn("tenantbackup.Service.OpenFile: backup downloaded",
		zap.String("tenant_id", t.ID), zap.String("backup_id", b.ID),
		zap.String("operator_tenant_id", actor.TenantID), zap.String("operator_user_id", actor.UserID))
	return &FileDownload{Name: FileName(t.Slug, b.CreatedAt, b.Kind), Size: size, Body: rc}, nil
}

// auditBoth writes the event under the operator's own tenant AND under the customer's tenant: the
// customer's trail must show that someone created / took its data. The customer-side diff carries
// only a human label (after.by) — never the operator's email or the workshop tenant id (spec D-25);
// the row's actor_user_id column DOES carry the operator's user id (an opaque UUID that the audit
// reader resolves to a name), so the customer can see that id in GET /admin/audit. When the operator backs up its OWN tenant one row is enough.
func (s *Service) auditBoth(ctx context.Context, actor Actor, t *TenantInfo, b *Backup, action, label string) {
	at := s.now().UTC()
	by := label
	if actor.UserID == "" {
		by = SystemActorLabel
	}
	if actor.TenantID != "" && actor.TenantID != t.ID {
		s.audit.Record(ctx, audit.Event{
			TenantID: actor.TenantID, ActorUserID: actor.UserID, EntityName: audit.TenantBackupMeta, RecordID: b.ID, Action: action,
			Diff: &audit.Diff{After: map[string]any{"backupId": b.ID, "tenantId": t.ID, "slug": t.Slug, "kind": b.Kind}}, At: at,
		})
	}
	s.audit.Record(ctx, audit.Event{
		TenantID: t.ID, ActorUserID: actorOrSystem(actor.UserID), EntityName: audit.TenantBackupMeta, RecordID: b.ID, Action: action,
		Diff: &audit.Diff{After: map[string]any{"backupId": b.ID, "kind": b.Kind, "by": by}}, At: at,
	})
}

func actorOrSystem(id string) string {
	if id == "" {
		return SystemActorID
	}
	return id
}

// ---- Writing ---------------------------------------------------------------------------------

// stepError records WHERE a backup failed (a table or a step) without any data value, so the
// message stored in error_detail can never carry a customer's row. The wrapped err is only for logs.
type stepError struct {
	step  string // "table" | "blobs" | "zip" | "snapshot"
	table string
	err   error
}

func (e *stepError) Error() string {
	return "tenantbackup." + e.step + " " + e.table + ": " + e.err.Error()
}
func (e *stepError) Unwrap() error { return e.err }

// detail is what is stored: step + table name only.
func (e *stepError) detail() string {
	if e.table != "" {
		return "bước " + e.step + ", bảng " + e.table
	}
	return "bước " + e.step
}

var errRowGone = errors.New("tenantbackup: catalogue row vanished (customer purged or backup reaped)")

// run is the runner job: write the zip, and on ANY failure leave a failed row and no file.
func (s *Service) run(ctx context.Context, b *Backup, label string) {
	err := s.safeWrite(ctx, b, label)
	if err == nil {
		return
	}
	s.logger.Error("tenantbackup.Service.run: backup failed",
		zap.String("tenant_id", b.TenantID), zap.String("backup_id", b.ID), zap.Error(err))
	code, detail := "BACKUP_WRITE_FAILED", ""
	var se *stepError
	if errors.As(err, &se) {
		detail = se.detail()
	}
	switch {
	case ctx.Err() != nil:
		code = "BACKUP_CANCELLED"
	case errors.Is(err, syscall.ENOSPC):
		code = "BACKUP_DISK_FULL"
	case errors.Is(err, errRowGone):
		// The row is gone (customer purged / backup reaped): nothing to mark, but the partial file
		// must not outlive it.
		s.removeFile(ctx, b)
		return
	}
	s.failRow(ctx, b, code, detail)
}

// safeWrite turns a panic in write into an error so run still records a failed row and removes the
// file; without it the row would stay running (holding op_lock) until the reaper's staleAfter.
// The panic value goes to the log only — never to error_detail.
// ponytail: a Put goroutine blocked on the pipe at panic time lingers until process exit.
func (s *Service) safeWrite(ctx context.Context, b *Backup, label string) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Error("tenantbackup.Service.safeWrite: backup job panicked",
				zap.String("tenant_id", b.TenantID), zap.String("backup_id", b.ID), zap.Any("panic", rec))
			err = &stepError{step: "write", err: fmt.Errorf("tenantbackup.Service.write: panic")}
		}
	}()
	return s.write(ctx, b, label)
}

// failRow marks a running row failed and removes its file. It runs on a context of its own
// (detached from cancellation, 10 s budget) because the usual reason to be here is that the job's
// context is already cancelled.
func (s *Service) failRow(ctx context.Context, b *Backup, code, detail string) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	s.removeFile(ctx, b)
	if _, err := s.store.MarkFailed(fctx, b.TenantID, b.ID, code, detail); err != nil {
		s.logger.Error("tenantbackup.Service.failRow: could not mark the backup failed",
			zap.String("tenant_id", b.TenantID), zap.String("backup_id", b.ID), zap.Error(err))
	}
}

// removeFile deletes the backup's file on a context detached from cancellation.
func (s *Service) removeFile(ctx context.Context, b *Backup) {
	if b.StorageKey == nil {
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.backups.Delete(dctx, *b.StorageKey); err != nil {
		s.logger.Warn("tenantbackup.Service.removeFile: could not remove the file",
			zap.String("tenant_id", b.TenantID), zap.String("backup_id", b.ID), zap.Error(err))
	}
}

type fileRef struct {
	id      string
	path    string
	sha     string
	deleted bool
}

// warnAgg aggregates warnings: a code, a count and up to ten sample ids.
type warnAgg map[string]*Warning

func (w warnAgg) add(code, id string) {
	x := w[code]
	if x == nil {
		x = &Warning{Code: code}
		w[code] = x
	}
	x.Count++
	if len(x.Sample) < maxSampleIDs {
		x.Sample = append(x.Sample, id)
	}
}

func (w warnAgg) list() []Warning {
	out := make([]Warning, 0, len(w))
	for _, x := range w {
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// countingWriter counts the bytes of the finished zip.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// write produces the zip for one catalogue row and flips it to ready.
//
// Order matters: (1) a REPEATABLE READ READ ONLY snapshot on its own connection pinned to UTC;
// (2) every exported table of ONE customer streamed through the codec into the zip (no table is
// held in memory); (3) the snapshot is COMMITTED before the attachment bytes are copied (blobs
// are not transactional, and holding MVCC history through a multi-GB copy is wasted pressure);
// (4) manifest + README last (they carry the counts); (5) only then is the row flipped to ready —
// with the WHERE excluding the state it writes — and if that matches nothing the customer was
// purged while we ran, so the file is removed rather than left orphaned (FR-060).
func (s *Service) write(ctx context.Context, b *Backup, label string) (err error) {
	if b.StorageKey == nil {
		return fmt.Errorf("tenantbackup.Service.write: row %s has no storage key", b.ID)
	}
	key := *b.StorageKey
	t, err := s.store.TenantInfo(ctx, b.TenantID)
	if err != nil {
		return &stepError{step: "snapshot", err: fmt.Errorf("tenant: %w", err)}
	}

	snap, err := s.snaps.Open(ctx)
	if err != nil {
		return &stepError{step: "snapshot", err: err}
	}
	defer snap.Close(ctx)
	version, dirty := snap.SchemaVersion()
	plans, err := snap.Plan(ctx)
	if err != nil {
		return &stepError{step: "snapshot", err: err}
	}
	branding, err := snap.Branding(ctx, b.TenantID)
	if err != nil {
		return &stepError{step: "snapshot", table: "tenants", err: err}
	}

	pr, pw := io.Pipe()
	putDone := make(chan error, 1)
	go func() {
		_, perr := s.backups.Put(ctx, key, pr)
		pr.CloseWithError(perr)
		putDone <- perr
	}()
	abort := func(cause error) error {
		pw.CloseWithError(cause)
		<-putDone
		return cause
	}

	sum := sha256.New()
	cw := &countingWriter{}
	zw := NewWriter(io.MultiWriter(pw, sum, cw))

	manifest := Manifest{
		FormatVersion: FormatVersion, BackupID: b.ID,
		Tenant:        ManifestTenant{ID: t.ID, Slug: t.Slug, Name: t.Name},
		Kind:          b.Kind,
		CreatedAt:     b.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		CreatedBy:     label,
		SchemaVersion: version, SchemaDirty: dirty, EngineBuild: engineBuild(),
		Tables: []ManifestTable{}, ExcludedTables: []ExcludedTable{}, ExcludedColumns: []ExcludedColumn{}, ExcludedJSON: []ExcludedJSONPath{},
		Files: ManifestFiles{Missing: []string{}, ShaMismatch: []string{}},
	}
	var (
		totalRows int64
		refs      []fileRef
		redacted  = map[[2]string]int{}
	)
	for _, tp := range plans {
		if err := ctx.Err(); err != nil {
			return abort(&stepError{step: "table", table: tp.Table, err: err})
		}
		rows, tableFiles, err := s.dumpTable(ctx, snap, zw, tp, b, redacted)
		if err != nil {
			return abort(err)
		}
		refs = append(refs, tableFiles...)
		totalRows += rows
		manifest.Tables = append(manifest.Tables, ManifestTable{
			Name: tp.Table, Class: tp.Rule.Class.String(), Rows: rows, Columns: tp.Cols, OwnerFK: tp.Rule.Owner,
		})
		manifest.ExcludedColumns = append(manifest.ExcludedColumns, tp.Dropped...)
		if alive, herr := s.heartbeat(ctx, b); herr == nil && !alive {
			return abort(errRowGone)
		}
	}
	snap.Close(ctx) // COMMIT the snapshot before the (non-transactional) attachment copy

	if err := zw.JSON(entryTenant, map[string]any{"branding": nullableJSON(branding)}); err != nil {
		return abort(&stepError{step: "zip", err: err})
	}

	warns := warnAgg{}
	blobBytes, blobCount, err := s.copyBlobs(ctx, zw, b, refs, warns, &manifest.Files)
	if err != nil {
		return abort(err)
	}
	manifest.Files.Count, manifest.Files.Bytes = blobCount, blobBytes

	for _, ex := range excludedTables() {
		manifest.ExcludedTables = append(manifest.ExcludedTables, ex)
	}
	for k, n := range redacted {
		manifest.ExcludedJSON = append(manifest.ExcludedJSON, ExcludedJSONPath{
			Table: k[0], Column: k[1], Path: "$..[type=outbound_webhook].secret|headers", Count: n,
			Reason: "khoá ký và header tuỳ chỉnh của hành động webhook ra không bao giờ vào tệp sao lưu",
		})
	}
	sort.Slice(manifest.ExcludedJSON, func(i, j int) bool {
		a, c := manifest.ExcludedJSON[i], manifest.ExcludedJSON[j]
		return a.Table+"."+a.Column < c.Table+"."+c.Column
	})
	if err := zw.Finish(Readme(manifest), manifest); err != nil {
		return abort(&stepError{step: "zip", err: err})
	}
	if err := pw.Close(); err != nil {
		return abort(&stepError{step: "zip", err: err})
	}
	if perr := <-putDone; perr != nil {
		return &stepError{step: "zip", err: fmt.Errorf("put: %w", perr)}
	}

	rows, err := s.store.MarkReady(ctx, b.TenantID, b.ID, ReadyFields{
		SizeBytes: cw.n, SHA256: hex.EncodeToString(sum.Sum(nil)), SchemaVersion: version, RowCount: totalRows,
		TableCount: len(manifest.Tables), FileCount: blobCount, BlobBytes: blobBytes, Warnings: warns.list(),
	})
	if err != nil {
		_ = s.backups.Delete(context.WithoutCancel(ctx), key)
		return &stepError{step: "finish", err: err}
	}
	if rows == 0 {
		// The row is gone (customer purged mid-run) or was reaped: a file whose catalogue row is
		// missing would sit on disk forever holding a customer's data (FR-060).
		if derr := s.backups.Delete(context.WithoutCancel(ctx), key); derr != nil {
			s.logger.Warn("tenantbackup.Service.write: could not remove the orphaned file", zap.String("backup_id", b.ID), zap.Error(derr))
		}
		s.logger.Warn("tenantbackup.Service.write: catalogue row vanished; file removed",
			zap.String("tenant_id", b.TenantID), zap.String("backup_id", b.ID))
		return errRowGone
	}
	s.logger.Info("tenantbackup.Service.write: backup ready",
		zap.String("tenant_id", b.TenantID), zap.String("backup_id", b.ID), zap.Int64("rows", totalRows),
		zap.Int("files", blobCount), zap.Int64("size_bytes", cw.n))
	return nil
}

// heartbeat reports whether the row is still ours to write.
func (s *Service) heartbeat(ctx context.Context, b *Backup) (alive bool, err error) {
	n, err := s.store.Heartbeat(ctx, b.TenantID, b.ID)
	if err != nil {
		s.logger.Warn("tenantbackup.Service.heartbeat: failed", zap.String("backup_id", b.ID), zap.Error(err))
		return true, err
	}
	return n > 0, nil
}

func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// dumpTable streams one table into tables/<name>.jsonl, redacting webhook credentials out of every
// JSON column of a system table, and collects the attachment catalogue when the table is sys_files.
func (s *Service) dumpTable(ctx context.Context, snap Snapshot, zw *Writer, tp TablePlan, b *Backup,
	redacted map[[2]string]int) (int64, []fileRef, error) {
	tw, err := zw.Table(tp.Table)
	if err != nil {
		return 0, nil, &stepError{step: "table", table: tp.Table, err: err}
	}
	var jsonCols []int // JSON columns of a system table: the only cells that can hold a webhook credential
	if !IsDynamic(tp.Table) {
		for i, c := range tp.Cols {
			if c.DataType == "json" {
				jsonCols = append(jsonCols, i)
			}
		}
	}
	var fid, fpath, fsha, fdel = -1, -1, -1, -1
	var refs []fileRef
	if tp.Table == "sys_files" {
		for i, c := range tp.Cols {
			switch c.Name {
			case "id":
				fid = i
			case "path":
				fpath = i
			case "sha256":
				fsha = i
			case "deleted_at":
				fdel = i
			}
		}
	}
	var rows int64
	err = snap.Dump(ctx, tp, b.TenantID, func(vals []sql.NullString) error {
		for _, i := range jsonCols {
			if err := redactCell(tp, i, vals, redacted); err != nil {
				return err
			}
		}
		line, err := EncodeRow(tp.Cols, vals)
		if err != nil {
			return err
		}
		if _, err := tw.Write(line); err != nil {
			return err
		}
		rows++
		if fid >= 0 && vals[fid].Valid {
			ref := fileRef{id: vals[fid].String}
			if fpath >= 0 {
				ref.path = vals[fpath].String
			}
			if fsha >= 0 && vals[fsha].Valid {
				ref.sha = strings.ToLower(vals[fsha].String)
			}
			ref.deleted = fdel >= 0 && vals[fdel].Valid
			refs = append(refs, ref)
		}
		if rows%heartbeatEveryRows == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			if alive, _ := s.heartbeat(ctx, b); !alive {
				return errRowGone
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errRowGone) {
			return 0, nil, err
		}
		return 0, nil, &stepError{step: "table", table: tp.Table, err: err}
	}
	return rows, refs, nil
}

// redactCell removes the credentials of outbound-webhook elements from one JSON cell. The
// substring test is a cheap pre-filter, not the rule: MySQL serialises JSON in canonical form
// (CAST AS CHAR decodes \uXXXX escapes), so a type of "outbound_webhook" always appears literally;
// a cell without the word cannot hold one and is not parsed (this runs once per row of every
// system table).
func redactCell(tp TablePlan, col int, vals []sql.NullString, redacted map[[2]string]int) error {
	if !vals[col].Valid || !strings.Contains(vals[col].String, "outbound_webhook") {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(vals[col].String))
	dec.UseNumber() // keep 18446744073709551615 exact: float64 would not
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("column %s: invalid JSON: %w", tp.Cols[col].Name, err)
	}
	out, n := RedactSecrets(v)
	if n == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("column %s: %w", tp.Cols[col].Name, err)
	}
	vals[col] = sql.NullString{String: strings.TrimRight(buf.String(), "\n"), Valid: true}
	redacted[[2]string{tp.Table, tp.Cols[col].Name}] += n
	return nil
}

// copyBlobs copies each attachment's bytes into blobs/<id>. Bytes are read ONLY from keys under
// the customer's own prefix `<tenant_id>/` (plan D-26): the zip is the one way bytes leave the
// system for a customer, so a catalogue `path` that points at another customer's directory — by
// a hand edit, or a `..` that normalises into one — is not opened (BLOB_PATH_FOREIGN), however
// unlikely the row is.
func (s *Service) copyBlobs(ctx context.Context, zw *Writer, b *Backup, refs []fileRef, warns warnAgg, mf *ManifestFiles) (int64, int, error) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].id < refs[j].id })
	prefix := b.TenantID + "/"
	var total int64
	var count int
	for i, f := range refs {
		if err := ctx.Err(); err != nil {
			return 0, 0, &stepError{step: "blobs", err: err}
		}
		if i%200 == 0 {
			if alive, _ := s.heartbeat(ctx, b); !alive {
				return 0, 0, errRowGone
			}
		}
		if f.path == "" || path.Clean(f.path) != f.path || !strings.HasPrefix(f.path, prefix) {
			warns.add(WarnBlobForeign, f.id)
			continue
		}
		if f.path != files.StorageKey(b.TenantID, f.id) {
			warns.add(WarnBlobNonstandard, f.id)
		}
		if !uuidRe.MatchString(f.id) {
			warns.add(WarnBlobMissing, f.id)
			continue
		}
		rc, err := s.blobs.Open(ctx, f.path)
		if errors.Is(err, storage.ErrNotFound) {
			// A file whose catalogue row is deleted has had its bytes removed on purpose (files.Delete):
			// that is not a missing blob. A LIVE row without bytes is.
			if !f.deleted {
				warns.add(WarnBlobMissing, f.id)
				if len(mf.Missing) < maxManifestIDs {
					mf.Missing = append(mf.Missing, f.id)
				}
			}
			continue
		}
		if err != nil {
			return 0, 0, &stepError{step: "blobs", err: err}
		}
		n, got, err := zw.Blob(f.id, rc)
		_ = rc.Close()
		if err != nil {
			return 0, 0, &stepError{step: "blobs", err: err}
		}
		total += n
		count++
		if f.sha != "" && got != f.sha {
			warns.add(WarnBlobShaMismatch, f.id)
			if len(mf.ShaMismatch) < maxManifestIDs {
				mf.ShaMismatch = append(mf.ShaMismatch, f.id)
			}
		}
	}
	return total, count, nil
}

// excludedTables lists, for the manifest, every table the policy never writes into the file.
func excludedTables() []ExcludedTable {
	var out []ExcludedTable
	for t, r := range rules {
		switch r.Class {
		case Skip:
			out = append(out, ExcludedTable{Table: t, Reason: "phiên, khoá, bí mật hoặc việc nền — không đưa vào tệp sao lưu"})
		case Clear:
			out = append(out, ExcludedTable{Table: t, Reason: "bộ nhớ đệm tạm thời — không cần sao lưu"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	return out
}

func engineBuild() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return bi.Main.Version
		}
	}
	return "dev"
}

// RunReaper reaps once at startup and then every interval until ctx is cancelled. It is the
// "at startup and periodically" of FR-021: a backup whose process died has no goroutine left to
// fail it, so something else has to.
func (s *Service) RunReaper(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if n, err := s.ReapStale(ctx); err != nil {
			s.logger.Error("tenantbackup.Service.RunReaper: reap failed", zap.Error(err))
		} else if n > 0 {
			s.logger.Warn("tenantbackup.Service.RunReaper: reaped abandoned backups", zap.Int("count", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ReapStale fails the backups that were running when the process died (no heartbeat for 30
// minutes) and removes their partial files. Run at startup and on a timer. Returns how many it failed.
func (s *Service) ReapStale(ctx context.Context) (int, error) {
	stale, err := s.store.ListStale(ctx, staleAfter)
	if err != nil {
		return 0, fmt.Errorf("tenantbackup.Service.ReapStale: %w", err)
	}
	n := 0
	for _, r := range stale {
		if r.StorageKey != "" {
			if derr := s.backups.Delete(ctx, r.StorageKey); derr != nil {
				s.logger.Warn("tenantbackup.Service.ReapStale: could not remove a partial file",
					zap.String("tenant_id", r.TenantID), zap.String("backup_id", r.ID), zap.Error(derr))
			}
		}
		rows, merr := s.store.MarkFailed(ctx, r.TenantID, r.ID, "BACKUP_ABANDONED", "bước dọn dẹp: không có nhịp tim quá 30 phút")
		if merr != nil {
			s.logger.Error("tenantbackup.Service.ReapStale: could not fail a stale backup",
				zap.String("tenant_id", r.TenantID), zap.String("backup_id", r.ID), zap.Error(merr))
			continue
		}
		if rows > 0 {
			n++
			s.logger.Warn("tenantbackup.Service.ReapStale: abandoned backup failed",
				zap.String("tenant_id", r.TenantID), zap.String("backup_id", r.ID))
		}
	}
	rn, err := s.reapRestores(ctx)
	if err != nil {
		return n, err
	}
	return n + rn, nil
}
