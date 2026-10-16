package tenantbackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/storage"
)

const (
	tenantA = "aaaaaaaa-0000-4000-8000-000000000001"
	tenantB = "bbbbbbbb-0000-4000-8000-000000000002"
	opTen   = "dddddddd-0000-4000-8000-0000000000d1"
	opUser  = "eeeeeeee-0000-4000-8000-0000000000e1"
)

// ---- fakes ------------------------------------------------------------------------------------

// memBackend is an in-memory storage.Backend.
type memBackend struct {
	mu      sync.Mutex
	objs    map[string][]byte
	deleted []string
	putErr  error
}

func newMem() *memBackend { return &memBackend{objs: map[string][]byte{}} }

func (m *memBackend) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return int64(len(b)), err
	}
	if m.putErr != nil {
		return 0, m.putErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = b
	return int64(len(b)), nil
}

func (m *memBackend) Open(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memBackend) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	m.deleted = append(m.deleted, key)
	return nil
}

func (m *memBackend) deletedKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.deleted...)
}

func (m *memBackend) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objs[key]
	return ok
}

type fakeStore struct {
	mu          sync.Mutex
	tenants     map[string]*TenantInfo
	rows        map[string]*Backup
	insertErr   error
	schemaErr   error
	estimate    int64
	version     int64
	heartbeatN  int64 // rows a heartbeat reports (default 1)
	readyN      int64 // rows MarkReady reports (default 1)
	insertCalls int
	failed      []string // "<code>|<detail>"
	stale       []StaleRow
	now         time.Time
	restoring   map[string]bool // tenants with a restore_status='running' row
}

func newStore() *fakeStore {
	return &fakeStore{
		tenants: map[string]*TenantInfo{
			tenantA: {ID: tenantA, Slug: "qa-x", Name: "QA X"},
			tenantB: {ID: tenantB, Slug: "qa-y", Name: "QA Y"},
		},
		rows: map[string]*Backup{}, version: 84, heartbeatN: 1, readyN: 1, now: time.Date(2026, 10, 7, 1, 2, 3, 0, time.UTC),
	}
}

func (f *fakeStore) TenantInfo(_ context.Context, id string) (*TenantInfo, error) {
	if t, ok := f.tenants[id]; ok {
		cp := *t
		return &cp, nil
	}
	return nil, ErrTenantNotFound
}
func (f *fakeStore) CheckSchema(context.Context) error { return f.schemaErr }
func (f *fakeStore) CurrentSchema(context.Context) (int64, bool, error) {
	return f.version, false, nil
}
func (f *fakeStore) Estimate(context.Context, string) (int64, error) { return f.estimate, nil }
func (f *fakeStore) Insert(_ context.Context, b *Backup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.insertCalls++
	if f.insertErr != nil {
		return f.insertErr
	}
	cp := *b
	cp.CreatedAt = f.now
	f.rows[b.ID] = &cp
	return nil
}
func (f *fakeStore) Get(_ context.Context, tenantID, id string) (*Backup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.rows[id]; ok && b.TenantID == tenantID {
		cp := *b
		return &cp, nil
	}
	return nil, ErrNotFound
}
func (f *fakeStore) List(_ context.Context, tenantID string, limit int, _ *Cursor) ([]Backup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Backup
	for _, b := range f.rows {
		if b.TenantID == tenantID {
			out = append(out, *b)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeStore) Heartbeat(context.Context, string, string) (int64, error) {
	return f.heartbeatN, nil
}
func (f *fakeStore) MarkReady(_ context.Context, tenantID, id string, r ReadyFields) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readyN == 0 {
		return 0, nil
	}
	b := f.rows[id]
	b.Status = StatusReady
	b.SizeBytes, b.SHA256, b.RowCount = &r.SizeBytes, &r.SHA256, &r.RowCount
	b.TableCount, b.FileCount, b.BlobBytes = &r.TableCount, &r.FileCount, &r.BlobBytes
	if len(r.Warnings) > 0 {
		b.Warnings, _ = json.Marshal(r.Warnings)
	}
	return 1, nil
}
func (f *fakeStore) MarkFailed(_ context.Context, tenantID, id, code, detail string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, code+"|"+detail)
	if b, ok := f.rows[id]; ok && b.Status == StatusRunning {
		b.Status = StatusFailed
		b.ErrorCode, b.ErrorDetail = &code, &detail
		return 1, nil
	}
	return 0, nil
}
func (f *fakeStore) ListStale(context.Context, time.Duration) ([]StaleRow, error) {
	return f.stale, nil
}
func (f *fakeStore) RestoreRunning(_ context.Context, tenantID string) (bool, error) {
	return f.restoring[tenantID], nil
}
func (f *fakeStore) OperatorLabel(context.Context, string, string) string {
	return "Nguyễn Văn A (Nexus)"
}
func (f *fakeStore) PersonLabels(_ context.Context, pairs [][2]string) map[[2]string]string {
	out := map[[2]string]string{}
	for _, p := range pairs {
		out[p] = "Nguyễn Văn A (Nexus)"
	}
	return out
}

func (f *fakeStore) status(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id].Status
}

// fakeSnaps serves canned tables.
type fakeSnaps struct {
	plans   []TablePlan
	rows    map[string][][]sql.NullString
	dumpErr map[string]error
	openErr error
	version int64
	panicOn string // table whose Dump panics
}

func (f *fakeSnaps) Open(context.Context) (Snapshot, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f, nil
}
func (f *fakeSnaps) SchemaVersion() (int64, bool) { return f.version, false }
func (f *fakeSnaps) Branding(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"appName":"X"}`), nil
}
func (f *fakeSnaps) Plan(context.Context) ([]TablePlan, error) { return f.plans, nil }
func (f *fakeSnaps) Dump(_ context.Context, tp TablePlan, _ string, row func([]sql.NullString) error) error {
	if f.panicOn == tp.Table {
		panic("boom")
	}
	for _, r := range f.rows[tp.Table] {
		cp := make([]sql.NullString, len(r))
		copy(cp, r)
		if err := row(cp); err != nil {
			return err
		}
	}
	return f.dumpErr[tp.Table]
}
func (f *fakeSnaps) Close(context.Context) {}

type recAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recAudit) Record(_ context.Context, e audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

type fixture struct {
	svc     *Service
	store   *fakeStore
	snaps   *fakeSnaps
	blobs   *memBackend
	backups *memBackend
	audit   *recAudit
	runner  *Runner
}

func plan(table string, class Class, cols ...Column) TablePlan {
	return TablePlan{TableRef: TableRef{Table: table, Rule: Rule{Class: class}}, Cols: cols}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: newStore(), snaps: &fakeSnaps{rows: map[string][][]sql.NullString{}, dumpErr: map[string]error{}, version: 84},
		blobs: newMem(), backups: newMem(), audit: &recAudit{}, runner: NewRunner(nil)}
	f.svc = NewService(f.store, f.snaps, f.blobs, f.backups, f.runner, f.audit, Config{Dir: t.TempDir(), MinFreeBytes: 100}, nil)
	f.svc.freeBytes = func(string) (uint64, error) { return 1 << 40, nil }
	f.svc.now = func() time.Time { return f.store.now }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.runner.Stop(ctx)
	})
	return f
}

func (f *fixture) waitDone(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.store.status(id) != StatusRunning {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("backup %s still running after 5s", id)
}

func asErr(t *testing.T, err error, code string) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want *Error code %s, got %v", code, err)
	}
	return e
}

var op = Actor{TenantID: opTen, UserID: opUser}

// ---- Create -----------------------------------------------------------------------------------

func TestCreate_HappyPathWritesAReadyZip(t *testing.T) {
	f := newFixture(t)
	f.snaps.plans = []TablePlan{plan("sys_entities", Restore, Column{"id", "char"}, Column{"name", "varchar"})}
	f.snaps.rows["sys_entities"] = [][]sql.NullString{{ns("e1"), ns("Khách hàng")}, {ns("e2"), ns("Cơ hội")}}

	v, err := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Note: "  trước khi nâng cấp ", Actor: op})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if v.Status != StatusRunning || v.Kind != KindManual || v.Note == nil || *v.Note != "trước khi nâng cấp" || v.Restorable {
		t.Errorf("a freshly created backup must be running/manual/trimmed-note/not restorable: %+v", v)
	}
	if v.CreatedBy == nil || v.CreatedBy.Label != "Nguyễn Văn A (Nexus)" {
		t.Errorf("createdBy label = %+v", v.CreatedBy)
	}
	f.waitDone(t, v.ID)

	row, _ := f.store.Get(context.Background(), tenantA, v.ID)
	if row.Status != StatusReady || row.RowCount == nil || *row.RowCount != 2 || row.SHA256 == nil || len(*row.SHA256) != 64 {
		t.Fatalf("row after write = %+v", row)
	}
	body, ok := f.backups.objs[tenantA+"/"+v.ID+".zip"]
	if !ok {
		t.Fatal("the zip must be at <tenant>/<id>.zip")
	}
	if int64(len(body)) != *row.SizeBytes {
		t.Errorf("size_bytes %d != stored %d", *row.SizeBytes, len(body))
	}
	r, err := OpenReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Tenant.Slug != "qa-x" || m.CreatedBy != "Nguyễn Văn A (Nexus)" || len(m.Tables) != 1 || m.Tables[0].Rows != 2 || m.SchemaVersion != 84 {
		t.Errorf("manifest = %+v", m)
	}
	if strings.Contains(toJSON(m), opUser) || strings.Contains(toJSON(m), opTen) {
		t.Errorf("manifest must not carry the operator's user id or the workshop tenant id: %s", toJSON(m))
	}
	rr, _ := r.Table("sys_entities")
	line, _ := rr.ReadLine()
	if !strings.Contains(string(line), `"name":"Khách hàng"`) {
		t.Errorf("first row = %s", line)
	}

	// audit: one row under the workshop tenant and one under the customer, the latter with a human label.
	if len(f.audit.events) != 2 {
		t.Fatalf("want 2 audit events, got %d", len(f.audit.events))
	}
	byTenant := map[string]audit.Event{}
	for _, e := range f.audit.events {
		byTenant[e.TenantID] = e
	}
	cust, work := byTenant[tenantA], byTenant[opTen]
	if cust.EntityName != audit.TenantBackupMeta || cust.Action != audit.ActionCreate || cust.Diff.After["by"] != "Nguyễn Văn A (Nexus)" {
		t.Errorf("customer-side audit = %+v", cust)
	}
	if strings.Contains(toJSON(cust.Diff), opTen) {
		t.Errorf("the customer-side diff must not carry the workshop tenant id: %s", toJSON(cust.Diff))
	}
	if work.RecordID != v.ID || work.ActorUserID != opUser {
		t.Errorf("workshop-side audit = %+v", work)
	}
}

func TestCreate_RefusalsBeforeAnyRow(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture)
		in    CreateInput
		code  string
	}{
		{"unknown customer", func(*fixture) {}, CreateInput{TenantID: "nope", Actor: op}, "TENANT_NOT_FOUND"},
		{"customer id is not a uuid-shaped existing tenant", func(*fixture) {}, CreateInput{TenantID: "cccccccc-0000-4000-8000-00000000000c", Actor: op}, "TENANT_NOT_FOUND"},
		{"note longer than 200 runes", func(*fixture) {}, CreateInput{TenantID: tenantA, Note: strings.Repeat("ạ", 201), Actor: op}, "BACKUP_NOTE_INVALID"},
		{"note with a control character", func(*fixture) {}, CreateInput{TenantID: tenantA, Note: "a\nb", Actor: op}, "BACKUP_NOTE_INVALID"},
		{"another operation is running (database says so)", func(f *fixture) { f.store.insertErr = ErrInProgress }, CreateInput{TenantID: tenantA, Actor: op}, "BACKUP_IN_PROGRESS"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f)
			_, err := f.svc.Create(context.Background(), tc.in)
			asErr(t, err, tc.code)
			if len(f.backups.objs) != 0 {
				t.Errorf("a refused backup must leave no file")
			}
		})
	}
}

func TestCreate_NoteBoundary(t *testing.T) {
	f := newFixture(t)
	v, err := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Note: strings.Repeat("ạ", 200), Actor: op})
	if err != nil {
		t.Fatalf("a 200-rune note must be accepted: %v", err)
	}
	f.waitDone(t, v.ID)
}

// FR-070: a backup that cannot fit is refused BEFORE a catalogue row exists.
func TestCreate_DiskLowInsertsNothing(t *testing.T) {
	f := newFixture(t)
	f.store.estimate = 5000
	f.svc.freeBytes = func(string) (uint64, error) { return 5099, nil } // need 5000 + 100 reserve
	_, err := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	e := asErr(t, err, "BACKUP_DISK_LOW")
	if e.Details["needBytes"] != int64(5100) || e.Details["freeBytes"] != uint64(5099) {
		t.Errorf("details = %v", e.Details)
	}
	if f.store.insertCalls != 0 {
		t.Errorf("no catalogue row may be inserted when the disk is low (insert calls = %d)", f.store.insertCalls)
	}

	f.svc.freeBytes = func(string) (uint64, error) { return 5100, nil } // exactly enough
	v, err := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	if err != nil {
		t.Fatalf("exactly enough space must be accepted: %v", err)
	}
	f.waitDone(t, v.ID)
}

func TestCreate_UnknownFreeSpaceDoesNotRefuse(t *testing.T) {
	f := newFixture(t)
	f.svc.freeBytes = func(string) (uint64, error) { return 0, errors.New("statfs unsupported") }
	v, err := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	if err != nil {
		t.Fatalf("an unreadable free-space figure must not block every backup: %v", err)
	}
	f.waitDone(t, v.ID)
}

// ---- failure paths ---------------------------------------------------------------------------

// A failure mid-way ends in a failed row, no file, and an error_detail that names the table and
// NEVER a value from a row.
func TestRun_FailureLeavesNoFileAndNoValue(t *testing.T) {
	f := newFixture(t)
	f.snaps.plans = []TablePlan{plan("sys_entities", Restore, Column{"id", "char"})}
	f.snaps.rows["sys_entities"] = [][]sql.NullString{{ns("e1")}}
	f.snaps.dumpErr["sys_entities"] = errors.New("Duplicate entry 'SENTINEL-CUSTOMER-VALUE' for key x")

	v, err := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	if err != nil {
		t.Fatal(err)
	}
	f.waitDone(t, v.ID)

	row, _ := f.store.Get(context.Background(), tenantA, v.ID)
	if row.Status != StatusFailed || row.ErrorCode == nil || *row.ErrorCode != "BACKUP_WRITE_FAILED" {
		t.Fatalf("row = %+v", row)
	}
	if row.ErrorDetail == nil || !strings.Contains(*row.ErrorDetail, "sys_entities") || strings.Contains(*row.ErrorDetail, "SENTINEL") {
		t.Errorf("error_detail must name the table and carry no value: %v", row.ErrorDetail)
	}
	key := tenantA + "/" + v.ID + ".zip"
	if f.backups.has(key) {
		t.Error("a failed backup must leave no file")
	}
	found := false
	for _, k := range f.backups.deleted {
		found = found || k == key
	}
	if !found {
		t.Error("the partial file must be deleted through the backend")
	}
	got, _ := f.svc.Get(context.Background(), tenantA, opTen, v.ID)
	if got.Error == nil || got.Error.Code != "BACKUP_WRITE_FAILED" || strings.Contains(got.Error.Message, "SENTINEL") {
		t.Errorf("the API error must be a sentence built from the code: %+v", got.Error)
	}
}

// A panic inside the job must not strand a running row (and its op_lock) for the reaper's 30 minutes:
// it ends as a failed row with no file and no panic value in the stored detail.
func TestRun_PanicEndsAsFailedRow(t *testing.T) {
	f := newFixture(t)
	f.snaps.plans = []TablePlan{plan("sys_entities", Restore, Column{"id", "char"})}
	f.snaps.panicOn = "sys_entities"

	v, err := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	if err != nil {
		t.Fatal(err)
	}
	f.waitDone(t, v.ID)

	row, _ := f.store.Get(context.Background(), tenantA, v.ID)
	if row.Status != StatusFailed || row.ErrorCode == nil || *row.ErrorCode != "BACKUP_WRITE_FAILED" {
		t.Fatalf("row = %+v", row)
	}
	if row.ErrorDetail != nil && strings.Contains(*row.ErrorDetail, "boom") {
		t.Errorf("panic value leaked into error_detail: %v", row.ErrorDetail)
	}
}

// A cancelled context (graceful stop) still records the failure — with a context of its own.
func TestRun_CancelledContextStillRecordsFailed(t *testing.T) {
	f := newFixture(t)
	f.snaps.plans = []TablePlan{plan("sys_entities", Restore, Column{"id", "char"})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	row := &Backup{ID: "11111111-1111-4111-8111-111111111111", TenantID: tenantA, Kind: KindManual, Status: StatusRunning,
		StorageKey: strPtr(tenantA + "/x.zip")}
	f.store.rows[row.ID] = row
	f.svc.run(ctx, row, "x")
	if f.store.status(row.ID) != StatusFailed {
		t.Fatalf("status = %s", f.store.status(row.ID))
	}
	if len(f.store.failed) != 1 || !strings.HasPrefix(f.store.failed[0], "BACKUP_CANCELLED|") {
		t.Errorf("failed = %v", f.store.failed)
	}
}

// FR-060 / FR-021: the row vanished (customer purged) while the backup ran: the file goes too.
func TestWrite_RowVanishedDeletesTheFile(t *testing.T) {
	f := newFixture(t)
	f.snaps.plans = []TablePlan{plan("sys_entities", Restore, Column{"id", "char"})}
	f.snaps.rows["sys_entities"] = [][]sql.NullString{{ns("e1")}}
	f.store.readyN = 0 // MarkReady matches no row

	v, err := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	if err != nil {
		t.Fatal(err)
	}
	key := tenantA + "/" + v.ID + ".zip"
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(f.backups.deletedKeys()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if f.backups.has(key) {
		t.Fatal("a file whose catalogue row is gone must be removed, not left holding a customer's data")
	}
}

func TestWrite_HeartbeatReportsGoneStopsTheBackup(t *testing.T) {
	f := newFixture(t)
	f.store.heartbeatN = 0
	f.snaps.plans = []TablePlan{plan("sys_entities", Restore, Column{"id", "char"})}
	f.snaps.rows["sys_entities"] = [][]sql.NullString{{ns("e1")}}
	v, err := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	if err != nil {
		t.Fatal(err)
	}
	key := tenantA + "/" + v.ID + ".zip"
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(f.backups.deletedKeys()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if f.backups.has(key) || f.store.status(v.ID) == StatusReady {
		t.Fatal("a backup whose row is gone must stop and leave no file")
	}
}

// ---- content rules ---------------------------------------------------------------------------

// The webhook credentials never reach the file, in sys_workflows.actions or in a provenance copy
// (sys_pack_items.content), while the URL and unknown keys survive; the manifest says so.
func TestWrite_RedactsWebhookCredentialsInEveryJSONColumn(t *testing.T) {
	f := newFixture(t)
	f.snaps.plans = []TablePlan{
		plan("sys_pack_items", Restore, Column{"id", "char"}, Column{"content", "json"}),
		plan("sys_workflows", Restore, Column{"id", "char"}, Column{"actions", "json"}),
		plan("obj_crm_contacts", Restore, Column{"id", "char"}, Column{"data", "json"}),
	}
	hook := `[{"type":"outbound_webhook","url":"https://partner.example/h","secret":"SENTINEL-SECRET","headers":{"Authorization":"Bearer SENTINEL-TOKEN"},"keep":1}]`
	f.snaps.rows["sys_workflows"] = [][]sql.NullString{{ns("w1"), ns(hook)}}
	f.snaps.rows["sys_pack_items"] = [][]sql.NullString{{ns("p1"), ns(`{"actions":` + hook + `,"name":"n"}`)}}
	// obj_* data is the customer's own business data: a field that merely LOOKS like a webhook is kept.
	f.snaps.rows["obj_crm_contacts"] = [][]sql.NullString{{ns("c1"), ns(`{"note":{"type":"outbound_webhook","secret":"mine"}}`)}}

	v, err := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	if err != nil {
		t.Fatal(err)
	}
	f.waitDone(t, v.ID)
	body := f.backups.objs[tenantA+"/"+v.ID+".zip"]
	r, _ := OpenReader(bytes.NewReader(body), int64(len(body)))
	var all strings.Builder
	for _, tbl := range []string{"sys_pack_items", "sys_workflows"} {
		rr, err := r.Table(tbl)
		if err != nil {
			t.Fatal(err)
		}
		line, _ := rr.ReadLine()
		all.Write(line)
	}
	text := all.String()
	if strings.Contains(text, "SENTINEL") {
		t.Errorf("a webhook credential reached the file: %s", text)
	}
	if !strings.Contains(text, "https://partner.example/h") || !strings.Contains(text, `"keep":1`) {
		t.Errorf("the URL and unknown keys must survive the redaction: %s", text)
	}
	rr, _ := r.Table("obj_crm_contacts")
	line, _ := rr.ReadLine()
	if !strings.Contains(string(line), `"secret":"mine"`) {
		t.Errorf("obj_* data must never be rewritten: %s", line)
	}
	m, _ := r.Manifest()
	seen := map[string]int{}
	for _, x := range m.ExcludedJSON {
		seen[x.Table+"."+x.Column] = x.Count
	}
	if seen["sys_workflows.actions"] != 1 || seen["sys_pack_items.content"] != 1 || len(seen) != 2 {
		t.Errorf("excludedJsonPaths must list exactly the (table,column) pairs that lost something: %v", seen)
	}
}

func TestWrite_DropColumnsAreNotInTheFile(t *testing.T) {
	f := newFixture(t)
	tp := plan("users", KeepExport, Column{"id", "char"}, Column{"email", "varchar"})
	tp.Dropped = []ExcludedColumn{{Table: "users", Column: "password_hash", Reason: "mật khẩu"}}
	f.snaps.plans = []TablePlan{tp}
	f.snaps.rows["users"] = [][]sql.NullString{{ns("u1"), ns("a@b.vn")}}
	v, _ := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	f.waitDone(t, v.ID)
	body := f.backups.objs[tenantA+"/"+v.ID+".zip"]
	r, _ := OpenReader(bytes.NewReader(body), int64(len(body)))
	m, _ := r.Manifest()
	if len(m.ExcludedColumns) != 1 || m.ExcludedColumns[0].Column != "password_hash" {
		t.Errorf("excludedColumns = %+v", m.ExcludedColumns)
	}
	// Skip-class tables are named in the manifest as excluded.
	names := map[string]bool{}
	for _, x := range m.ExcludedTables {
		names[x.Table] = true
	}
	for _, must := range []string{"refresh_tokens", "sys_api_keys", "sys_named_credentials", "sys_inbound_hooks", "user_mfa", "sys_jobs"} {
		if !names[must] {
			t.Errorf("manifest.excludedTables must list %s", must)
		}
	}
}

// ---- attachments -----------------------------------------------------------------------------

func filesPlan() TablePlan {
	return plan("sys_files", Restore, Column{"id", "char"}, Column{"path", "varchar"}, Column{"sha256", "char"}, Column{"deleted_at", "timestamp"})
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestWrite_BlobsAreReadOnlyUnderTheCustomersPrefix(t *testing.T) {
	const (
		idOK       = "10000000-0000-4000-8000-000000000001"
		idMissing  = "10000000-0000-4000-8000-000000000002"
		idMismatch = "10000000-0000-4000-8000-000000000003"
		idForeign  = "10000000-0000-4000-8000-000000000004"
		idTraverse = "10000000-0000-4000-8000-000000000005"
		idTomb     = "10000000-0000-4000-8000-000000000006"
		idTombGone = "10000000-0000-4000-8000-000000000007"
		idOdd      = "10000000-0000-4000-8000-000000000008"
	)
	f := newFixture(t)
	f.snaps.plans = []TablePlan{filesPlan()}
	own := func(id string) string { return tenantA + "/" + id }
	f.blobs.objs[own(idOK)] = []byte("hello")
	f.blobs.objs[own(idMismatch)] = []byte("changed")
	f.blobs.objs[tenantB+"/"+idForeign] = []byte("B-SECRET-BYTES")
	f.blobs.objs[tenantB+"/x"] = []byte("B-SECRET-BYTES")
	f.blobs.objs[own(idTomb)] = []byte("tomb")
	f.blobs.objs[tenantA+"/odd/"+idOdd] = []byte("odd")
	f.snaps.rows["sys_files"] = [][]sql.NullString{
		{ns(idOK), ns(own(idOK)), ns(sha("hello")), null},
		{ns(idMissing), ns(own(idMissing)), null, null},
		{ns(idMismatch), ns(own(idMismatch)), ns(sha("original")), null},
		{ns(idForeign), ns(tenantB + "/" + idForeign), null, null},                    // points into customer B
		{ns(idTraverse), ns(tenantA + "/../" + tenantB + "/x"), null, null},           // normalises into B
		{ns(idTomb), ns(own(idTomb)), null, ns("2026-01-01 00:00:00.000000")},         // deleted, bytes still there
		{ns(idTombGone), ns(own(idTombGone)), null, ns("2026-01-01 00:00:00.000000")}, // deleted, bytes gone (normal)
		{ns(idOdd), ns(tenantA + "/odd/" + idOdd), null, null},                        // under the prefix, not the standard key
	}
	v, _ := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	f.waitDone(t, v.ID)

	body := f.backups.objs[tenantA+"/"+v.ID+".zip"]
	if bytes.Contains(body, []byte("B-SECRET-BYTES")) {
		t.Fatal("another customer's bytes reached this customer's file")
	}
	r, err := OpenReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, id := range r.BlobIDs() {
		have[id] = true
	}
	for _, id := range []string{idOK, idMismatch, idTomb, idOdd} {
		if !have[id] {
			t.Errorf("blob %s must be in the file", id)
		}
	}
	for _, id := range []string{idMissing, idForeign, idTraverse, idTombGone} {
		if have[id] {
			t.Errorf("blob %s must NOT be in the file", id)
		}
	}
	row, _ := f.store.Get(context.Background(), tenantA, v.ID)
	var warns []Warning
	_ = json.Unmarshal(row.Warnings, &warns)
	count := map[string]int{}
	for _, w := range warns {
		count[w.Code] = w.Count
	}
	want := map[string]int{WarnBlobMissing: 1, WarnBlobShaMismatch: 1, WarnBlobForeign: 2, WarnBlobNonstandard: 1}
	for code, n := range want {
		if count[code] != n {
			t.Errorf("warning %s = %d, want %d (all: %v)", code, count[code], n, count)
		}
	}
	if row.Status != StatusReady {
		t.Errorf("warnings must not fail the backup (status %s)", row.Status)
	}
	m, _ := r.Manifest()
	if len(m.Files.Missing) != 1 || m.Files.Missing[0] != idMissing || len(m.Files.ShaMismatch) != 1 || m.Files.Count != 4 {
		t.Errorf("manifest.files = %+v", m.Files)
	}
}

// ---- download / list --------------------------------------------------------------------------

func TestOpenFile(t *testing.T) {
	f := newFixture(t)
	f.snaps.plans = []TablePlan{plan("sys_entities", Restore, Column{"id", "char"})}
	v, _ := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	f.waitDone(t, v.ID)
	f.audit.events = nil

	dl, err := f.svc.OpenFile(context.Background(), tenantA, op, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	if dl.Name != "nexus-qa-x-20261007-010203-manual.zip" {
		t.Errorf("name = %s", dl.Name)
	}
	if dl.Size != int64(len(f.backups.objs[tenantA+"/"+v.ID+".zip"])) {
		t.Errorf("size = %d", dl.Size)
	}
	if len(f.audit.events) != 2 || f.audit.events[0].Action != audit.ActionDownload {
		t.Errorf("a download must be audited in both places before streaming: %+v", f.audit.events)
	}

	// another customer's id under this customer's path => not found
	_, err = f.svc.OpenFile(context.Background(), tenantB, op, v.ID)
	asErr(t, err, "BACKUP_NOT_FOUND")
	_, err = f.svc.OpenFile(context.Background(), tenantA, op, "not-a-uuid")
	asErr(t, err, "BACKUP_NOT_FOUND")

	// file gone from disk => 409 file missing
	delete(f.backups.objs, tenantA+"/"+v.ID+".zip")
	_, err = f.svc.OpenFile(context.Background(), tenantA, op, v.ID)
	asErr(t, err, "BACKUP_FILE_MISSING")

	// not ready
	f.store.rows[v.ID].Status = StatusRunning
	_, err = f.svc.OpenFile(context.Background(), tenantA, op, v.ID)
	asErr(t, err, "BACKUP_NOT_READY")
}

func TestFileName(t *testing.T) {
	at := time.Date(2026, 10, 7, 1, 2, 3, 0, time.FixedZone("x", 7*3600))
	if got := FileName("QA X/../y", at, KindPreRestore); got != "nexus-qa-x----y-20261006-180203-pre-restore.zip" {
		t.Errorf("FileName = %q", got)
	}
}

func TestListAndGet_RestorableReasons(t *testing.T) {
	f := newFixture(t)
	f.snaps.plans = []TablePlan{plan("sys_entities", Restore, Column{"id", "char"})}
	v, _ := f.svc.Create(context.Background(), CreateInput{TenantID: tenantA, Actor: op})
	f.waitDone(t, v.ID)

	check := func(name string, caller string, wantOK bool, wantReason string) {
		t.Helper()
		got, err := f.svc.Get(context.Background(), tenantA, caller, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		reason := ""
		if got.NotRestorableReason != nil {
			reason = *got.NotRestorableReason
		}
		if got.Restorable != wantOK || reason != wantReason {
			t.Errorf("%s: restorable=%v reason=%q, want %v %q", name, got.Restorable, reason, wantOK, wantReason)
		}
	}
	check("ready, other tenant stands", opTen, true, "")
	check("the customer the caller stands in", tenantA, false, reasonSelf)
	f.store.version = 85
	check("schema moved on", opTen, false, reasonSchema)
	f.store.version = 84
	f.store.tenants[tenantA].Retired = true
	check("retired customer", opTen, false, reasonRetired)
	f.store.tenants[tenantA].Retired = false
	delete(f.backups.objs, tenantA+"/"+v.ID+".zip")
	check("file gone", opTen, false, reasonFile)
	f.store.rows[v.ID].Status = StatusFailed
	check("not ready", opTen, false, reasonNotReady)

	list, meta, err := f.svc.List(context.Background(), ListInput{TenantID: tenantA, CallerTenantID: opTen, Limit: 1000})
	if err != nil || len(list) != 1 || meta.CurrentSchemaVersion != 84 || meta.NextCursor != nil {
		t.Fatalf("List = %v %+v %v", list, meta, err)
	}
	_, _, err = f.svc.List(context.Background(), ListInput{TenantID: tenantA, After: "garbage!"})
	asErr(t, err, "BACKUP_CURSOR_INVALID")
	_, _, err = f.svc.List(context.Background(), ListInput{TenantID: "missing"})
	asErr(t, err, "TENANT_NOT_FOUND")
}

// ---- reaper ----------------------------------------------------------------------------------

func TestReapStale_FailsAbandonedAndRemovesTheirFiles(t *testing.T) {
	f := newFixture(t)
	key := tenantA + "/dead.zip"
	f.backups.objs[key] = []byte("partial")
	f.store.rows["dead"] = &Backup{ID: "dead", TenantID: tenantA, Status: StatusRunning, StorageKey: &key}
	f.store.stale = []StaleRow{{ID: "dead", TenantID: tenantA, StorageKey: key}}

	n, err := f.svc.ReapStale(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("ReapStale = %d, %v", n, err)
	}
	if f.store.status("dead") != StatusFailed || f.backups.has(key) {
		t.Error("the abandoned backup must be failed and its file removed")
	}
	if len(f.store.failed) != 1 || !strings.HasPrefix(f.store.failed[0], "BACKUP_ABANDONED|") {
		t.Errorf("failed = %v", f.store.failed)
	}
}

// ---- helpers ---------------------------------------------------------------------------------

func TestValidateNote(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false}, {"   ", "", false}, {" ok ", "ok", false},
		{strings.Repeat("a", 200), strings.Repeat("a", 200), false},
		{strings.Repeat("a", 201), "", true}, {"tab\there", "", true}, {"nul\x00", "", true},
	}
	for _, tc := range tests {
		got, err := validateNote(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateNote(%q) err = %v", tc.in, err)
			continue
		}
		if !tc.wantErr && ((got == nil && tc.want != "") || (got != nil && *got != tc.want)) {
			t.Errorf("validateNote(%q) = %v, want %q", tc.in, got, tc.want)
		}
	}
}
