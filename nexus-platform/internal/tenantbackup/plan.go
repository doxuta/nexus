package tenantbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"go.uber.org/zap"

	"nexus/platform/internal/storage"
)

// defaultRestoreMaxRows mirrors config.defaultTenantRestoreMaxRows for a Service wired without one.
const defaultRestoreMaxRows = 300000

// PlanStore is the read-only half of the live database that "Kiểm tra khôi phục" needs. Every
// method is tenant-pinned and none writes (FR-032).
type PlanStore interface {
	// OperationRunning: a backup or restore of this customer is running (the op_lock column).
	OperationRunning(ctx context.Context, tenantID string) (bool, error)
	// ExportTables: the tables a backup of the live schema would hold, and every foreign key.
	ExportTables(ctx context.Context) ([]TableRef, []FK, error)
	// TableColumns: native columns of the named tables that exist; a missing table is absent.
	TableColumns(ctx context.Context, names []string) (map[string][]Column, error)
	CountRows(ctx context.Context, ref TableRef, tenantID string) (int64, error)
	AccessNow(ctx context.Context, tenantID string) (*AccessSnapshot, error)
	// EntityLabels: obj_ table name -> the entity's current label.
	EntityLabels(ctx context.Context, tenantID string) (map[string]string, error)
	// LiveFiles: sys_files id -> size.
	LiveFiles(ctx context.Context, tenantID string) (map[string]int64, error)
	PendingJobs(ctx context.Context, tenantID string) (int64, error)
}

// WithPlan wires the read side of the restore preview. maxRows <= 0 means the default (300 000).
func (s *Service) WithPlan(p PlanStore, maxRows int) *Service {
	if maxRows <= 0 {
		maxRows = defaultRestoreMaxRows
	}
	s.plan, s.maxRestoreRows = p, maxRows
	return s
}

// ---- API shapes (plan.md §Hợp đồng API) -------------------------------------------------------

// RestorePlan is what a restore would do, computed without doing it.
type RestorePlan struct {
	BackupID           string        `json:"backupId"`
	BackupCreatedAt    string        `json:"backupCreatedAt"`
	Integrity          PlanOK        `json:"integrity"`
	Schema             PlanSchema    `json:"schema"`
	Groups             []PlanGroup   `json:"groups"`
	Blobs              PlanBlobs     `json:"blobs"`
	Warnings           []PlanWarn    `json:"warnings"`
	AccessChanges      AccessChanges `json:"accessChanges"`
	Kept               []string      `json:"kept"`
	PreRestoreEstimate PlanEstimate  `json:"preRestoreEstimate"`
}

type PlanOK struct {
	OK bool `json:"ok"`
}

type PlanSchema struct {
	Backup  int64 `json:"backup"`
	Current int64 `json:"current"`
	OK      bool  `json:"ok"`
}

type PlanGroup struct {
	Group  string      `json:"group"`
	Label  string      `json:"label"`
	Tables []PlanTable `json:"tables"`
}

type PlanTable struct {
	Table        string `json:"table"`
	Label        string `json:"label"`
	RowsInBackup int64  `json:"rowsInBackup"`
	RowsNow      int64  `json:"rowsNow"`
}

type PlanBlobs struct {
	ToWrite      int   `json:"toWrite"`
	BytesToWrite int64 `json:"bytesToWrite"`
	ToRemove     int   `json:"toRemove"`
}

type PlanWarn struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

type PlanEstimate struct {
	Rows      int64  `json:"rows"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	FreeBytes uint64 `json:"freeBytes"`
}

var groupOrder = []struct{ key, label string }{
	{"data", "Dữ liệu"}, {"config", "Cấu hình"}, {"access", "Quyền"}, {"files", "Tệp"},
}

// keptList is what a restore never touches (FR-036), already in the operator's language.
var keptList = []string{
	"Người dùng, mật khẩu và đăng nhập (SSO, xác thực hai bước)",
	"Phiên đăng nhập và mọi token",
	"Khoá API và khoá kết nối ngoài",
	"Webhook vào và khoá bí mật của chúng",
	"Nhật ký hoạt động và cảnh báo bảo mật",
	"Việc nền đang chờ",
	"Thông tin thanh toán",
	"Lịch sử trò chuyện với trợ lý AI",
	"Chủ sở hữu hồ sơ quay về như lúc sao lưu",
}

// ---- Validate (shared with the restore itself) ------------------------------------------------

// loaded is a backup that passed every check of FR-033, still open.
type loaded struct {
	tenant   *TenantInfo
	backup   *Backup
	reader   *Reader
	manifest *Manifest
	current  int64
	exports  []TableRef
	fks      []FK
	nowRows  map[string]int64 // restore / merge_max tables of the live schema
	release  func()
}

func (l *loaded) close() {
	if l.release != nil {
		l.release()
	}
}

func driftErr(msg string, details map[string]any) *Error {
	return &Error{Code: "TENANT_RESTORE_SCHEMA_DRIFT", Msg: msg, Details: details}
}

// openZip returns the backup file as an io.ReaderAt with its size and sha256. A LocalBackend hands
// back an *os.File, which is read in place; any other backend is spooled to a 0600 temp file that
// release removes.
func (s *Service) openZip(ctx context.Context, key string) (ra io.ReaderAt, size int64, sum string, release func(), err error) {
	rc, err := s.backups.Open(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, 0, "", nil, &Error{Code: "BACKUP_FILE_MISSING", Msg: "Tệp sao lưu không còn trên máy chủ"}
		}
		return nil, 0, "", nil, fmt.Errorf("tenantbackup.Service.openZip: open: %w", err)
	}
	h := sha256.New()
	if f, ok := rc.(*os.File); ok {
		st, serr := f.Stat()
		if serr != nil {
			_ = rc.Close()
			return nil, 0, "", nil, fmt.Errorf("tenantbackup.Service.openZip: stat: %w", serr)
		}
		if _, cerr := io.Copy(h, rc); cerr != nil {
			_ = rc.Close()
			return nil, 0, "", nil, fmt.Errorf("tenantbackup.Service.openZip: hash: %w", cerr)
		}
		return f, st.Size(), hex.EncodeToString(h.Sum(nil)), func() { _ = f.Close() }, nil
	}
	defer rc.Close()
	tmp, err := os.CreateTemp("", "nexus-restore-*.zip")
	if err != nil {
		return nil, 0, "", nil, fmt.Errorf("tenantbackup.Service.openZip: spool: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, 0, "", nil, fmt.Errorf("tenantbackup.Service.openZip: chmod: %w", err)
	}
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }
	n, err := io.Copy(io.MultiWriter(tmp, h), rc)
	if err != nil {
		cleanup()
		return nil, 0, "", nil, fmt.Errorf("tenantbackup.Service.openZip: spool copy: %w", err)
	}
	return tmp, n, hex.EncodeToString(h.Sum(nil)), cleanup, nil
}

// Validate runs the checks of FR-033 in their fixed order and stops at the first failure. On
// success the caller owns the returned *loaded and must close it. It writes nothing.
func (s *Service) Validate(ctx context.Context, callerTenantID, tenantID, backupID string) (*loaded, error) {
	if s.plan == nil {
		return nil, errors.New("tenantbackup.Service.Validate: plan store not wired")
	}
	t, err := s.store.TenantInfo(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			return nil, tenantNotFound()
		}
		return nil, fmt.Errorf("tenantbackup.Service.Validate: tenant: %w", err)
	}
	if t.Retired {
		return nil, &Error{Code: "TENANT_RESTORE_RETIRED", Msg: "Khách đã ngưng, không khôi phục được"}
	}
	if t.ID == callerTenantID {
		return nil, &Error{Code: "TENANT_RESTORE_SELF", Msg: "Không khôi phục chính không gian bạn đang đứng"}
	}
	b, err := s.getBackup(ctx, t.ID, backupID)
	if err != nil {
		return nil, err
	}
	if b.Status != StatusReady || b.StorageKey == nil {
		return nil, &Error{Code: "BACKUP_NOT_READY", Msg: "Bản sao lưu chưa sẵn sàng"}
	}
	running, err := s.plan.OperationRunning(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.Validate: operation: %w", err)
	}
	if running {
		return nil, &Error{Code: "BACKUP_IN_PROGRESS", Msg: "Khách này đang có một tác vụ sao lưu hoặc khôi phục chạy"}
	}

	ra, size, sum, release, err := s.openZip(ctx, *b.StorageKey)
	if err != nil {
		return nil, err
	}
	l := &loaded{tenant: t, backup: b, release: release}
	ok := false
	defer func() {
		if !ok {
			l.close()
		}
	}()
	if b.SHA256 == nil || sum != *b.SHA256 {
		return nil, corrupt("mã băm của tệp không khớp với lúc sao lưu")
	}
	if l.reader, err = OpenReader(ra, size); err != nil {
		return nil, err
	}
	if l.manifest, err = l.reader.Manifest(); err != nil {
		return nil, err
	}
	m := l.manifest
	if m.Tenant.ID != t.ID {
		return nil, &Error{Code: "TENANT_RESTORE_TENANT_MISMATCH", Msg: "Bản sao lưu này không phải của khách đang chọn"}
	}
	if m.FormatVersion != FormatVersion {
		return nil, corrupt("định dạng %d không được hỗ trợ", m.FormatVersion)
	}
	cur, dirty, err := s.store.CurrentSchema(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.Validate: schema version: %w", err)
	}
	l.current = cur
	// Both the file and its catalogue row must name the current version (the row is what the list
	// shows; a disagreement is never silently trusted either way).
	if m.SchemaVersion != cur || b.SchemaVersion != cur || m.SchemaDirty || dirty {
		return nil, &Error{Code: "TENANT_RESTORE_SCHEMA_MISMATCH",
			Msg:     "Bản sao lưu được tạo ở phiên bản cấu trúc khác với hiện tại",
			Details: map[string]any{"backup": b.SchemaVersion, "current": cur}}
	}
	if err := s.store.CheckSchema(ctx); err != nil {
		return nil, err
	}
	if err := s.checkManifestAgainstSchema(ctx, l); err != nil {
		return nil, err
	}
	var inFile int64
	for _, mt := range m.Tables {
		if mt.Class == Restore.String() {
			inFile += mt.Rows
		}
	}
	if inFile > int64(s.maxRestoreRows) {
		return nil, tooLarge(inFile, s.maxRestoreRows)
	}
	if l.exports, l.fks, err = s.plan.ExportTables(ctx); err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.Validate: tables: %w", err)
	}
	l.nowRows = map[string]int64{}
	var live int64
	for _, ref := range l.exports {
		if ref.Rule.Class != Restore && ref.Rule.Class != MergeMax {
			continue
		}
		n, err := s.plan.CountRows(ctx, ref, t.ID)
		if err != nil {
			return nil, fmt.Errorf("tenantbackup.Service.Validate: count %s: %w", ref.Table, err)
		}
		l.nowRows[ref.Table] = n
		if ref.Rule.Class == Restore {
			live += n
		}
	}
	if live > int64(s.maxRestoreRows) {
		return nil, tooLarge(live, s.maxRestoreRows)
	}
	ok = true
	return l, nil
}

func tooLarge(rows int64, limit int) *Error {
	return &Error{Code: "TENANT_RESTORE_TOO_LARGE", Msg: "Khách có quá nhiều dòng để khôi phục trong một lần",
		Details: map[string]any{"rows": rows, "limit": limit}}
}

// checkManifestAgainstSchema: every table the manifest lists is classified the same way NOW, and
// every column it carries still exists. The manifest is untrusted input: names are validated before
// they reach a query, and the set of tables a restore writes is always taken from the live policy.
func (s *Service) checkManifestAgainstSchema(ctx context.Context, l *loaded) error {
	names := make([]string, 0, len(l.manifest.Tables))
	seen := map[string]bool{}
	for _, mt := range l.manifest.Tables {
		if err := checkIdent(mt.Name); err != nil || seen[mt.Name] || !l.reader.HasTable(mt.Name) {
			return corrupt("danh sách bảng trong bản không hợp lệ (%q)", mt.Name)
		}
		for _, c := range mt.Columns {
			if err := checkIdent(c.Name); err != nil {
				return corrupt("tên cột không hợp lệ trong bảng %q", mt.Name)
			}
		}
		seen[mt.Name] = true
		names = append(names, mt.Name)
		r, ok := RuleFor(mt.Name)
		if !ok || r.Class.String() != mt.Class {
			cur := "unclassified"
			if ok {
				cur = r.Class.String()
			}
			return driftErr("Lớp sao lưu của bảng "+mt.Name+" đã đổi từ lúc sao lưu",
				map[string]any{"table": mt.Name, "backupClass": mt.Class, "currentClass": cur})
		}
	}
	have, err := s.plan.TableColumns(ctx, names)
	if err != nil {
		return fmt.Errorf("tenantbackup.Service.checkManifestAgainstSchema: %w", err)
	}
	hint := "Chạy Khám sức khoẻ (A5) rồi sao lưu lại"
	for _, mt := range l.manifest.Tables {
		cols, exists := have[mt.Name]
		if !exists {
			if IsDynamic(mt.Name) {
				continue // a restore recreates a dropped obj_/rel_ table
			}
			return driftErr("Bảng "+mt.Name+" không còn trong cấu trúc hiện tại",
				map[string]any{"table": mt.Name, "columns": []string{}, "hint": hint})
		}
		haveCol := make(map[string]bool, len(cols))
		for _, c := range cols {
			haveCol[c.Name] = true
		}
		var lost []string
		for _, c := range mt.Columns {
			if !haveCol[c.Name] {
				lost = append(lost, c.Name)
			}
		}
		if len(lost) > 0 {
			return driftErr("Bảng "+mt.Name+" không còn một số cột có trong bản sao lưu",
				map[string]any{"table": mt.Name, "columns": lost, "hint": hint})
		}
	}
	return nil
}

// ---- Plan -------------------------------------------------------------------------------------

// PreviewInput is one "Kiểm tra khôi phục".
type PreviewInput struct {
	TenantID       string
	BackupID       string
	CallerTenantID string
}

// Preview validates the backup and describes what restoring it would change. It writes nothing:
// not a row, not a file.
func (s *Service) Preview(ctx context.Context, in PreviewInput) (*RestorePlan, error) {
	l, err := s.Validate(ctx, in.CallerTenantID, in.TenantID, in.BackupID)
	if err != nil {
		return nil, err
	}
	defer l.close()
	tid := l.tenant.ID

	bAccess, facts, err := readBackupFacts(l.reader)
	if err != nil {
		return nil, err
	}
	cur, err := s.plan.AccessNow(ctx, tid)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.Preview: access now: %w", err)
	}
	labels, err := s.plan.EntityLabels(ctx, tid)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.Preview: labels: %w", err)
	}
	live, err := s.plan.LiveFiles(ctx, tid)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.Preview: files: %w", err)
	}
	jobs, err := s.plan.PendingJobs(ctx, tid)
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.Service.Preview: jobs: %w", err)
	}

	p := &RestorePlan{
		BackupID: l.backup.ID, BackupCreatedAt: l.backup.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Integrity: PlanOK{OK: true}, Schema: PlanSchema{Backup: l.manifest.SchemaVersion, Current: l.current, OK: true},
		Kept: keptList, AccessChanges: DiffAccess(bAccess, cur),
	}

	// tables: manifest ∪ live, parents before children, bucketed by group.
	inFile := map[string]ManifestTable{}
	for _, mt := range l.manifest.Tables {
		inFile[mt.Name] = mt
	}
	set := map[string]bool{}
	for n, mt := range inFile {
		if mt.Class == Restore.String() || mt.Class == MergeMax.String() {
			set[n] = true
		}
	}
	for n := range l.nowRows {
		set[n] = true
	}
	byGroup := map[string][]PlanTable{}
	var rowsNow int64
	for _, name := range topoOrder(set, l.fks) {
		r, ok := RuleFor(name)
		if !ok || r.Group == "" {
			continue
		}
		pt := PlanTable{Table: name, RowsInBackup: inFile[name].Rows, RowsNow: l.nowRows[name]}
		rowsNow += pt.RowsNow
		if pt.RowsInBackup == 0 && pt.RowsNow == 0 {
			continue
		}
		pt.Label = labels[name]
		if pt.Label == "" {
			pt.Label = facts.labels[name]
		}
		if pt.Label == "" {
			pt.Label = name
		}
		byGroup[r.Group] = append(byGroup[r.Group], pt)
	}
	for _, g := range groupOrder {
		if len(byGroup[g.key]) > 0 {
			p.Groups = append(p.Groups, PlanGroup{Group: g.key, Label: g.label, Tables: byGroup[g.key]})
		}
	}

	// attachments
	inZip := map[string]bool{}
	for _, id := range l.reader.BlobIDs() {
		inZip[id] = true
	}
	for id, size := range facts.files {
		if _, ok := live[id]; !ok && inZip[id] {
			p.Blobs.ToWrite++
			p.Blobs.BytesToWrite += size
		}
	}
	var liveBytes int64
	for id, size := range live {
		liveBytes += size
		if _, ok := facts.files[id]; !ok {
			p.Blobs.ToRemove++
		}
	}

	// warnings
	lost := 0
	for _, u := range cur.Users {
		if u.RoleID != "" && !bAccess.Roles[u.RoleID] {
			lost++
		}
	}
	skipped := 0
	for k := range bAccess.Members {
		if _, ok := cur.Users[k[indexAfterSep(k):]]; !ok {
			skipped++
		}
	}
	for _, w := range []PlanWarn{
		{"ROLE_LOST", lost}, {"MEMBERSHIP_SKIPPED", skipped}, {"PENDING_JOBS_KEPT", int(jobs)},
		{"BLOB_MISSING_IN_BACKUP", len(l.manifest.Files.Missing)}, {"ACCESS_WIDENED", p.AccessChanges.Total},
	} {
		if w.Count > 0 {
			p.Warnings = append(p.Warnings, w)
		}
	}

	var free uint64
	if s.freeBytes != nil {
		if f, ferr := s.freeBytes(s.cfg.Dir); ferr == nil {
			free = f
		} else {
			s.logger.Warn("tenantbackup.Service.Preview: free space unreadable", zap.String("tenant_id", tid), zap.Error(ferr))
		}
	}
	p.PreRestoreEstimate = PlanEstimate{Rows: rowsNow, Files: len(live), Bytes: liveBytes, FreeBytes: free}
	s.logger.Info("tenantbackup.Service.Preview: restore preview computed",
		zap.String("tenant_id", tid), zap.String("backup_id", l.backup.ID), zap.Int("access_widened", p.AccessChanges.Total))
	return p, nil
}

func indexAfterSep(k string) int {
	for i := 0; i < len(k); i++ {
		if k[i] == keySep[0] {
			return i + 1
		}
	}
	return 0
}

// topoOrder sorts the tables so a parent comes before its children (Kahn, ties by name). A cycle —
// not expected — falls back to name order for what is left.
func topoOrder(set map[string]bool, fks []FK) []string {
	parents := map[string]map[string]bool{}
	for _, fk := range fks {
		if fk.Table == fk.RefTable || !set[fk.Table] || !set[fk.RefTable] {
			continue
		}
		if parents[fk.Table] == nil {
			parents[fk.Table] = map[string]bool{}
		}
		parents[fk.Table][fk.RefTable] = true
	}
	var out []string
	done := map[string]bool{}
	for len(out) < len(set) {
		var ready []string
		for n := range set {
			if done[n] {
				continue
			}
			ok := true
			for p := range parents[n] {
				if !done[p] {
					ok = false
					break
				}
			}
			if ok {
				ready = append(ready, n)
			}
		}
		if len(ready) == 0 { // cycle: take the rest in name order
			for n := range set {
				if !done[n] {
					ready = append(ready, n)
				}
			}
		}
		sort.Strings(ready)
		for _, n := range ready {
			done[n] = true
			out = append(out, n)
		}
	}
	return out
}

// ---- reading the file -------------------------------------------------------------------------

type backupFacts struct {
	files  map[string]int64  // sys_files id -> size
	labels map[string]string // obj table -> label, from the backup's own sys_entities
}

func rawString(m map[string]json.RawMessage, k string) string {
	var s string
	if v, ok := m[k]; ok && string(v) != "null" {
		_ = json.Unmarshal(v, &s)
	}
	return s
}

func rawNull(m map[string]json.RawMessage, k string) bool {
	v, ok := m[k]
	return !ok || string(v) == "null"
}

func (r *Reader) eachRow(table string, fn func(map[string]json.RawMessage)) error {
	if !r.HasTable(table) {
		return nil
	}
	rr, err := r.Table(table)
	if err != nil {
		return err
	}
	defer rr.Close()
	for {
		line, err := rr.ReadLine()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return corrupt("dòng không đọc được trong bảng %s", table)
		}
		fn(m)
	}
}

// readBackupFacts reads the few small tables the preview needs: the access configuration (as an
// AccessSnapshot), the attachment list and the entity labels. Other tables are only counted from
// the manifest.
func readBackupFacts(r *Reader) (*AccessSnapshot, *backupFacts, error) {
	a := NewAccessSnapshot()
	f := &backupFacts{files: map[string]int64{}, labels: map[string]string{}}
	steps := []struct {
		table string
		fn    func(m map[string]json.RawMessage)
	}{
		{"acl_roles", func(m map[string]json.RawMessage) { a.AddRole(rawString(m, "id")) }},
		{"acl_role_actions", func(m map[string]json.RawMessage) {
			a.AddGrant(rawString(m, "role_id"), rawString(m, "entity_name"), rawString(m, "action"), rawString(m, "access_level"))
		}},
		{"sys_field_acl", func(m map[string]json.RawMessage) {
			a.AddFieldACL(rawString(m, "role_id"), rawString(m, "entity_name"), rawString(m, "field_name"),
				rawString(m, "can_view") != "0", rawString(m, "can_edit") != "0")
		}},
		{"sys_record_shares", func(m map[string]json.RawMessage) {
			a.AddShare(rawString(m, "id"), rawString(m, "user_id"), !rawNull(m, "deleted_at"))
		}},
		{"team_memberships", func(m map[string]json.RawMessage) { a.AddMember(rawString(m, "team_id"), rawString(m, "user_id")) }},
		{"sys_fields", func(m map[string]json.RawMessage) {
			a.AddField(rawString(m, "id"), m["options"], !rawNull(m, "deleted_at"))
		}},
		{"sys_workflows", func(m map[string]json.RawMessage) {
			a.AddWorkflow(rawString(m, "id"), rawString(m, "is_active") == "1", !rawNull(m, "deleted_at"), m["actions"])
		}},
		{"sys_files", func(m map[string]json.RawMessage) {
			var size int64
			_, _ = fmt.Sscan(rawString(m, "size"), &size)
			f.files[rawString(m, "id")] = size
		}},
		{"sys_entities", func(m map[string]json.RawMessage) {
			if t, l := rawString(m, "table_name"), rawString(m, "label"); t != "" && l != "" {
				f.labels[t] = l
			}
		}},
	}
	for _, st := range steps {
		if err := r.eachRow(st.table, st.fn); err != nil {
			return nil, nil, fmt.Errorf("tenantbackup.readBackupFacts %s: %w", st.table, err)
		}
	}
	return a, f, nil
}
