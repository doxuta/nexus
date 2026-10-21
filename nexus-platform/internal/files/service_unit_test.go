package files

// Default-build unit tests for the file Service business logic. The RecordGuard and
// storage.Backend seams are faked in-memory; the sys_files Repository runs over the fake sqlx
// driver (fakedb_test.go). Covered: upload policy (mime whitelist, declared + actual size cap,
// empty upload), server-generated storage key (never the client filename), catalog rollback on
// insert failure, ref-array sync, and the Open/Delete not-found remapping that prevents a
// cross-tenant / out-of-scope existence oracle.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/services"
	"nexus/platform/internal/storage"
)

// ---- fakes -------------------------------------------------------------------------------------

type fakeGuard struct {
	mu           sync.Mutex
	entity       string // canonical entity name to return (defaults to the input name)
	resolveErr   error
	explainErr   error // what the shared write-miss decision answers (nil = the miss unchanged)
	setRefsErr   error
	onSetRefs    func() // runs inside SetFileRefs, before it answers (a test cancels the request here)
	resolveCalls int
	setRefsCalls int
	gotRefs      []map[string]any
}

func (g *fakeGuard) ResolveFileField(_ context.Context, _ services.RecordScope, _, entityName, _, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resolveCalls++
	if g.resolveErr != nil {
		return "", g.resolveErr
	}
	if g.entity != "" {
		return g.entity, nil
	}
	return entityName, nil
}

func (g *fakeGuard) ExplainWriteMiss(_ context.Context, _ services.RecordScope, _, _, _ string, err error) error {
	if g.explainErr != nil {
		return g.explainErr
	}
	return err
}

func (g *fakeGuard) SetFileRefs(_ context.Context, _ services.RecordScope, _, _, _, _ string, refs []map[string]any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.setRefsCalls++
	g.gotRefs = refs
	if g.onSetRefs != nil {
		g.onSetRefs()
	}
	return g.setRefsErr
}

type fakeBackend struct {
	mu       sync.Mutex
	putCalls int
	putKey   string
	putData  []byte
	putErr   error
	openData string
	openErr  error
	deleted  []string
	// deleteCtxErr is the ctx.Err() the last Delete saw: a rollback must not run on a dead request.
	deleteCtxErr error
	// blobs, when set, makes Open key-aware (key → bytes; absent key → storage.ErrNotFound).
	blobs map[string]string
}

func (b *fakeBackend) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	data, err := io.ReadAll(r) // consume fully, like a real copy — the LimitReader cap applies here
	if err != nil {
		return 0, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.putCalls++
	b.putKey = key
	b.putData = data
	if b.putErr != nil {
		return 0, b.putErr
	}
	return int64(len(data)), nil
}

func (b *fakeBackend) Open(_ context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.blobs != nil {
		data, ok := b.blobs[key]
		if !ok {
			return nil, storage.ErrNotFound
		}
		return io.NopCloser(strings.NewReader(data)), nil
	}
	if b.openErr != nil {
		return nil, b.openErr
	}
	return io.NopCloser(strings.NewReader(b.openData)), nil
}

func (b *fakeBackend) Delete(ctx context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleteCtxErr = ctx.Err()
	b.deleted = append(b.deleted, key)
	return nil
}

func (b *fakeBackend) deletedKeys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.deleted...)
}

var testScope = services.RecordScope{UserID: "user-1", AccessLevel: "all"}

func allowAllMime(string) bool { return true }

// newTestService wires a Service over the fakes + fake-driver repository, with a fixed id.
func newTestService(t *testing.T, guard *fakeGuard, backend *fakeBackend, limits Limits) (*Service, *fakeDB) {
	t.Helper()
	db, rec := newFakeDB(t)
	svc := NewService(NewRepository(db), backend, guard, limits, func() string { return "file-1" })
	return svc, rec
}

func pngUpload(content string) UploadInput {
	return UploadInput{
		TenantID:   "tenant-a",
		EntityName: "Orders", // non-canonical on purpose: the service must use the guard's resolved name
		RecordID:   "rec-1",
		FieldName:  "attachments",
		Filename:   `C:\Users\x\avatar.png`, // windows path on purpose: display name must be sanitized
		Mime:       "image/png",
		Size:       int64(len(content)),
		Content:    strings.NewReader(content),
	}
}

// ---- Upload ------------------------------------------------------------------------------------

func TestFileService_Upload_HappyPath(t *testing.T) {
	guard := &fakeGuard{entity: "orders"}
	backend := &fakeBackend{}
	svc, rec := newTestService(t, guard, backend, Limits{MaxBytes: 1024, MimeOK: allowAllMime})
	// Insert uses the default exec reply (OK); the post-insert ref sync reads the catalog back.
	rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)

	got, err := svc.Upload(context.Background(), testScope, pngUpload("hello world"))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got.ID != "file-1" || got.Name != "avatar.png" || got.Size != 11 {
		t.Errorf("result = %+v; want id file-1, sanitized name avatar.png, size 11", got)
	}
	if got.URL != "/api/v1/files/file-1" {
		t.Errorf("URL = %q; want /api/v1/files/file-1", got.URL)
	}
	// Storage key is server-generated "<tenant>/<id>" — never derived from the client filename.
	if backend.putKey != "tenant-a/file-1" {
		t.Errorf("storage key = %q; want tenant-a/file-1", backend.putKey)
	}
	if strings.Contains(backend.putKey, "avatar") || strings.Contains(backend.putKey, `\`) {
		t.Errorf("storage key %q leaked client filename", backend.putKey)
	}
	if string(backend.putData) != "hello world" {
		t.Errorf("stored bytes = %q; want full content", backend.putData)
	}
	// Catalog row uses the guard's CANONICAL entity name, the sanitized display name, and the key.
	call := rec.lastExec(t)
	mustContainSQL(t, call.query, "INSERT INTO sys_files")
	if call.args[1] != "tenant-a" || call.args[2] != "orders" {
		t.Errorf("insert tenant/entity = %v/%v; want tenant-a/orders (canonical)", call.args[1], call.args[2])
	}
	if call.args[5] != "avatar.png" || call.args[8] != "tenant-a/file-1" {
		t.Errorf("insert name/path = %v/%v; want avatar.png / tenant-a/file-1", call.args[5], call.args[8])
	}
	// The record's file field was re-synced from the catalog.
	if guard.setRefsCalls != 1 || len(guard.gotRefs) != 1 {
		t.Fatalf("SetFileRefs calls/refs = %d/%v; want 1 call with 1 ref", guard.setRefsCalls, guard.gotRefs)
	}
	if guard.gotRefs[0]["id"] != "file-1" || guard.gotRefs[0]["name"] != "avatar.png" {
		t.Errorf("refs = %v; want [{id:file-1 name:avatar.png}]", guard.gotRefs)
	}
	if len(backend.deletedKeys()) != 0 {
		t.Errorf("blob deleted on success: %v", backend.deletedKeys())
	}
}

// sumHelloWorld = sha256("hello world"), lowercase hex — the form the drill's sha256sum prints.
const sumHelloWorld = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"

// The digest is taken from the bytes the backend actually consumed (tee on the stream), so the
// restore drill can later prove a restored blob is byte-for-byte the uploaded one (#54).
func TestFileService_Upload_RecordsSha256OfStoredBytes(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "hello world", content: "hello world", want: sumHelloWorld},
		{name: "một byte khác → mã băm khác", content: "hello World",
			want: "db4067cec62c58bf8b2f8982071e77c082da9e00924bf3631f3b024fa54e7d7e"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeBackend{}
			svc, rec := newTestService(t, &fakeGuard{}, backend, Limits{MaxBytes: 1024, MimeOK: allowAllMime})
			rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)

			if _, err := svc.Upload(context.Background(), testScope, pngUpload(tc.content)); err != nil {
				t.Fatalf("Upload: %v", err)
			}
			call := rec.lastExec(t)
			mustContainSQL(t, call.query, "INSERT INTO sys_files", "sha256")
			if got := call.args[len(call.args)-1]; got != tc.want {
				t.Errorf("stored sha256 = %v; want %s (digest of %q)", got, tc.want, backend.putData)
			}
		})
	}
}

func TestFileService_Upload_PolicyRejections(t *testing.T) {
	tests := []struct {
		name        string
		guardErr    error
		explainErr  error
		limits      Limits
		mutate      func(*UploadInput)
		content     string
		wantErr     error
		wantPut     bool // backend.Put reached?
		wantCleanup bool // stored blob rolled back?
	}{
		{
			name:     "guard giấu record (khác tenant / ngoài scope) → lỗi propagate, không đụng storage",
			guardErr: services.ErrRecordNotFound,
			limits:   Limits{MaxBytes: 1024, MimeOK: allowAllMime},
			content:  "hello",
			wantErr:  services.ErrRecordNotFound,
		},
		{
			// #43: the record is outside the edit scope but readable — the shared decision's 403 wins.
			name:       "guard trượt, người gọi vẫn xem được hồ sơ → 403 của quyết định chung, không đụng storage",
			guardErr:   services.ErrRecordNotFound,
			explainErr: services.ErrRecordEditForbidden,
			limits:     Limits{MaxBytes: 1024, MimeOK: allowAllMime},
			content:    "hello",
			wantErr:    services.ErrRecordEditForbidden,
		},
		{
			name:    "mime ngoài whitelist → ErrMimeNotAllowed trước khi ghi byte nào",
			limits:  Limits{MaxBytes: 1024, MimeOK: func(m string) bool { return m == "application/pdf" }},
			content: "hello",
			wantErr: ErrMimeNotAllowed,
		},
		{
			name:    "Content-Length khai báo vượt cap → ErrFileTooLarge trước khi stream",
			limits:  Limits{MaxBytes: 4, MimeOK: allowAllMime},
			content: "hello",
			wantErr: ErrFileTooLarge,
		},
		{
			name:        "upload rỗng → ErrEmptyFile + dọn blob",
			limits:      Limits{MaxBytes: 1024, MimeOK: allowAllMime},
			content:     "",
			wantErr:     ErrEmptyFile,
			wantPut:     true,
			wantCleanup: true,
		},
		{
			name:   "Content-Length nói dối (khai 0, gửi vượt cap) → cap giữa dòng copy + dọn blob",
			limits: Limits{MaxBytes: 4, MimeOK: allowAllMime},
			mutate: func(in *UploadInput) {
				in.Size = 0 // unknown declared size — the mid-copy cap must still trip
			},
			content:     "hello world",
			wantErr:     ErrFileTooLarge,
			wantPut:     true,
			wantCleanup: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guard := &fakeGuard{entity: "orders", resolveErr: tt.guardErr, explainErr: tt.explainErr}
			backend := &fakeBackend{}
			svc, _ := newTestService(t, guard, backend, tt.limits)
			in := pngUpload(tt.content)
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			_, err := svc.Upload(context.Background(), testScope, in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v; want %v", err, tt.wantErr)
			}
			if !tt.wantPut && backend.putCalls != 0 {
				t.Errorf("backend.Put called %d times; want 0 (reject before storing)", backend.putCalls)
			}
			deleted := backend.deletedKeys()
			if tt.wantCleanup && len(deleted) == 0 {
				t.Errorf("stored blob not rolled back")
			}
			if !tt.wantCleanup && len(deleted) != 0 {
				t.Errorf("unexpected blob delete: %v", deleted)
			}
			if guard.setRefsCalls != 0 {
				t.Errorf("SetFileRefs called on a rejected upload")
			}
		})
	}
}

func TestFileService_Upload_CapBoundary(t *testing.T) {
	// Exactly at the cap passes (the LimitReader is cap+1 so the boundary is inclusive).
	guard := &fakeGuard{entity: "orders"}
	backend := &fakeBackend{}
	svc, rec := newTestService(t, guard, backend, Limits{MaxBytes: 10, MimeOK: allowAllMime})
	rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)

	got, err := svc.Upload(context.Background(), testScope, pngUpload("0123456789")) // 10 bytes
	if err != nil {
		t.Fatalf("Upload at cap: %v", err)
	}
	if got.Size != 10 {
		t.Errorf("Size = %d; want 10", got.Size)
	}

	// One byte over (with a lying declared size) trips the cap and only cap+1 bytes were read.
	backend2 := &fakeBackend{}
	svc2, _ := newTestService(t, &fakeGuard{entity: "orders"}, backend2, Limits{MaxBytes: 10, MimeOK: allowAllMime})
	in := pngUpload("0123456789A") // 11 bytes
	in.Size = 0
	if _, err := svc2.Upload(context.Background(), testScope, in); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v; want ErrFileTooLarge", err)
	}
	if len(backend2.putData) != 11 { // MaxBytes+1: the copy stops right past the cap
		t.Errorf("bytes read = %d; want capped at MaxBytes+1 = 11", len(backend2.putData))
	}
}

func TestFileService_Upload_InsertFailureRollsBackBlob(t *testing.T) {
	guard := &fakeGuard{entity: "orders"}
	backend := &fakeBackend{}
	svc, rec := newTestService(t, guard, backend, Limits{MaxBytes: 1024, MimeOK: allowAllMime})
	rec.queueExec(0, errors.New("db down")) // sys_files INSERT fails

	_, err := svc.Upload(context.Background(), testScope, pngUpload("hello"))
	if err == nil || !strings.Contains(err.Error(), "files.Upload") {
		t.Fatalf("err = %v; want wrapped files.Upload", err)
	}
	if deleted := backend.deletedKeys(); len(deleted) != 1 || deleted[0] != "tenant-a/file-1" {
		t.Errorf("blob rollback = %v; want [tenant-a/file-1]", deleted)
	}
	if guard.setRefsCalls != 0 {
		t.Errorf("SetFileRefs called after failed insert")
	}
}

func TestFileService_Upload_SyncFailureKeepsBlob(t *testing.T) {
	guard := &fakeGuard{entity: "orders", setRefsErr: errors.New("record update failed")}
	backend := &fakeBackend{}
	svc, rec := newTestService(t, guard, backend, Limits{MaxBytes: 1024, MimeOK: allowAllMime})
	rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)

	_, err := svc.Upload(context.Background(), testScope, pngUpload("hello"))
	if err == nil || !strings.Contains(err.Error(), "sync record field") {
		t.Fatalf("err = %v; want wrapped sync failure", err)
	}
	// The blob + catalog row are committed — a sync failure must NOT delete the stored file.
	if deleted := backend.deletedKeys(); len(deleted) != 0 {
		t.Errorf("blob deleted after sync failure: %v", deleted)
	}
}

// refusedWrites are record-write errors that mean the record service REFUSED the ref-array write
// before touching the row: a specs/048 before_save veto, a required file field emptied, and the
// fail-closed "rules unreadable" answer. A file mutation that preceded one of them must be undone.
var refusedWrites = []struct {
	name string
	err  error
}{
	{"before_save veto", &services.ValidationError{Code: "RECORD_HOOK_VETO", Msg: "Hồ sơ đã khoá, không sửa được"}},
	{"required file field emptied", &services.ValidationError{Code: "RECORD_REQUIRED_MISSING", Msg: "field attachments is required"}},
	{"before rules unreadable (fail-closed)", fmt.Errorf("services.RecordService.loadBeforeRules: %w: %w", services.ErrRecordHooksUnavailable, errors.New("db down"))},
}

// specs/048 S1b: a before_save rule refusing the record write is a decision, not a glitch. The
// catalog row + blob are rolled back so the refused file does not ride into the record on the next
// upload's re-sync (which rebuilds the field from every live sys_files row).
func TestFileService_Upload_VetoRollsBackFile(t *testing.T) {
	for _, tt := range refusedWrites {
		t.Run(tt.name, func(t *testing.T) {
			guard := &fakeGuard{entity: "orders", setRefsErr: fmt.Errorf("services.SetFileRefs: %w", tt.err)}
			backend := &fakeBackend{}
			svc, rec := newTestService(t, guard, backend, Limits{MaxBytes: 1024, MimeOK: allowAllMime})
			rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)

			_, err := svc.Upload(context.Background(), testScope, pngUpload("hello"))
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v; want the refusal passed through", err)
			}
			if deleted := backend.deletedKeys(); len(deleted) != 1 || deleted[0] != "tenant-a/file-1" {
				t.Errorf("blob rollback = %v; want [tenant-a/file-1]", deleted)
			}
			last := rec.lastExec(t)
			if !strings.Contains(last.query, "UPDATE sys_files SET deleted_at") || last.args[1] != "tenant-a" || last.args[2] != "file-1" {
				t.Errorf("catalog rollback = %q %v; want the tenant-pinned soft delete of file-1", last.query, last.args)
			}
		})
	}
}

// #227: the rollback of a refused upload and the restore of a refused delete are decisions already
// taken, so a client that hangs up at that exact moment must not strand the file row: the refused
// attachment left live (the next upload's re-sync would attach it) or the kept attachment deleted.
func TestFileService_RefusedWriteRollback_SurvivesRequestCancel(t *testing.T) {
	veto := refusedWrites[0].err
	t.Run("upload: refused file is removed though the request was cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		guard := &fakeGuard{entity: "orders", setRefsErr: fmt.Errorf("services.SetFileRefs: %w", veto), onSetRefs: cancel}
		backend := &fakeBackend{}
		svc, rec := newTestService(t, guard, backend, Limits{MaxBytes: 1024, MimeOK: allowAllMime})
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)

		_, err := svc.Upload(ctx, testScope, pngUpload("hello"))
		if !errors.Is(err, veto) {
			t.Fatalf("err = %v; want the veto passed through (not a roll-back failure)", err)
		}
		if got := rec.lastExec(t).query; !strings.Contains(got, "UPDATE sys_files SET deleted_at") {
			t.Errorf("last exec = %q; want the soft delete of the refused file", got)
		}
		if backend.deleteCtxErr != nil {
			t.Errorf("blob rollback ran on a dead ctx: %v", backend.deleteCtxErr)
		}
	})
	t.Run("delete: kept file is restored though the request was cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		guard := &fakeGuard{entity: "orders", setRefsErr: fmt.Errorf("services.SetFileRefs: %w", veto), onSetRefs: cancel}
		svc, rec := newTestService(t, guard, &fakeBackend{}, Limits{MimeOK: allowAllMime})
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)

		err := svc.Delete(ctx, testScope, "tenant-a", "file-1")
		if !errors.Is(err, veto) {
			t.Fatalf("err = %v; want the veto passed through (not a restore failure)", err)
		}
		if got := rec.lastExec(t).query; !strings.Contains(got, "SET deleted_at = NULL") {
			t.Errorf("last exec = %q; want the restore of the kept file", got)
		}
	})
}

// ---- Open --------------------------------------------------------------------------------------

func TestFileService_Open(t *testing.T) {
	t.Run("stream blob + catalog row", func(t *testing.T) {
		guard := &fakeGuard{entity: "orders"}
		backend := &fakeBackend{openData: "blob-bytes"}
		svc, rec := newTestService(t, guard, backend, Limits{MimeOK: allowAllMime})
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)

		rc, f, err := svc.Open(context.Background(), testScope, "tenant-a", "file-1")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer rc.Close()
		data, _ := io.ReadAll(rc)
		if string(data) != "blob-bytes" {
			t.Errorf("stream = %q; want blob-bytes", data)
		}
		if f.Name != "avatar.png" || f.Mime != "image/png" || f.Path != "tenant-a/file-1" {
			t.Errorf("catalog row mismatched: %+v", f)
		}
	})

	t.Run("catalog miss → ErrFileNotFound", func(t *testing.T) {
		svc, _ := newTestService(t, &fakeGuard{}, &fakeBackend{}, Limits{MimeOK: allowAllMime})
		if _, _, err := svc.Open(context.Background(), testScope, "tenant-a", "file-x"); !errors.Is(err, ErrFileNotFound) {
			t.Errorf("err = %v; want ErrFileNotFound", err)
		}
	})

	t.Run("record ngoài scope → remap ErrFileNotFound (không lộ record tồn tại)", func(t *testing.T) {
		guard := &fakeGuard{resolveErr: services.ErrRecordNotFound}
		svc, rec := newTestService(t, guard, &fakeBackend{}, Limits{MimeOK: allowAllMime})
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)
		if _, _, err := svc.Open(context.Background(), testScope, "tenant-a", "file-1"); !errors.Is(err, ErrFileNotFound) {
			t.Errorf("err = %v; want ErrFileNotFound", err)
		}
	})

	t.Run("lỗi guard khác record-not-found → propagate nguyên văn", func(t *testing.T) {
		guard := &fakeGuard{resolveErr: services.ErrNotFileField}
		svc, rec := newTestService(t, guard, &fakeBackend{}, Limits{MimeOK: allowAllMime})
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)
		_, _, err := svc.Open(context.Background(), testScope, "tenant-a", "file-1")
		if !errors.Is(err, services.ErrNotFileField) {
			t.Errorf("err = %v; want services.ErrNotFileField passthrough", err)
		}
	})

	t.Run("blob mất trên backend → ErrFileNotFound", func(t *testing.T) {
		backend := &fakeBackend{openErr: storage.ErrNotFound}
		svc, rec := newTestService(t, &fakeGuard{entity: "orders"}, backend, Limits{MimeOK: allowAllMime})
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)
		if _, _, err := svc.Open(context.Background(), testScope, "tenant-a", "file-1"); !errors.Is(err, ErrFileNotFound) {
			t.Errorf("err = %v; want ErrFileNotFound", err)
		}
	})
}

// ---- Delete ------------------------------------------------------------------------------------

func TestFileService_Delete(t *testing.T) {
	t.Run("soft-delete + gỡ blob + re-sync refs rỗng", func(t *testing.T) {
		guard := &fakeGuard{entity: "orders"}
		backend := &fakeBackend{}
		svc, rec := newTestService(t, guard, backend, Limits{MimeOK: allowAllMime})
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)
		// SoftDelete uses the default exec reply (1 row); the ref re-sync then reads an empty catalog.

		if err := svc.Delete(context.Background(), testScope, "tenant-a", "file-1"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if deleted := backend.deletedKeys(); len(deleted) != 1 || deleted[0] != "tenant-a/file-1" {
			t.Errorf("blob delete = %v; want [tenant-a/file-1]", deleted)
		}
		if guard.setRefsCalls != 1 || len(guard.gotRefs) != 0 {
			t.Errorf("SetFileRefs calls/refs = %d/%v; want 1 call with empty refs", guard.setRefsCalls, guard.gotRefs)
		}
	})

	t.Run("record ngoài scope → remap ErrFileNotFound", func(t *testing.T) {
		guard := &fakeGuard{resolveErr: services.ErrRecordNotFound}
		backend := &fakeBackend{}
		svc, rec := newTestService(t, guard, backend, Limits{MimeOK: allowAllMime})
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)
		if err := svc.Delete(context.Background(), testScope, "tenant-a", "file-1"); !errors.Is(err, ErrFileNotFound) {
			t.Errorf("err = %v; want ErrFileNotFound", err)
		}
		if len(backend.deletedKeys()) != 0 {
			t.Errorf("blob deleted for an unauthorized caller")
		}
	})

	t.Run("double-delete → ErrFileNotFound, blob không bị gỡ lại", func(t *testing.T) {
		guard := &fakeGuard{entity: "orders"}
		backend := &fakeBackend{}
		svc, rec := newTestService(t, guard, backend, Limits{MimeOK: allowAllMime})
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)
		rec.queueExec(0, nil) // SoftDelete matches no live row (already tombstoned)
		if err := svc.Delete(context.Background(), testScope, "tenant-a", "file-1"); !errors.Is(err, ErrFileNotFound) {
			t.Errorf("err = %v; want ErrFileNotFound", err)
		}
		if len(backend.deletedKeys()) != 0 {
			t.Errorf("blob deleted despite failed soft-delete")
		}
		if guard.setRefsCalls != 0 {
			t.Errorf("SetFileRefs called despite failed soft-delete")
		}
	})
}

// specs/048 Nợ 16: removing an attachment from a record a before_save rule has locked used to half
// happen — the 422 came back with the rule's words, but the sys_files row was already tombstoned,
// so the download 404'd and the next re-sync dropped the file for good. A refused record write
// must leave the attachment exactly as it was: row live again, blob untouched, refusal passed on.
func TestFileService_Delete_RefusedRecordWriteKeepsTheFile(t *testing.T) {
	for _, tt := range refusedWrites {
		t.Run(tt.name, func(t *testing.T) {
			guard := &fakeGuard{entity: "orders", setRefsErr: fmt.Errorf("services.SetFileRefs: %w", tt.err)}
			backend := &fakeBackend{}
			svc, rec := newTestService(t, guard, backend, Limits{MimeOK: allowAllMime})
			rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)

			err := svc.Delete(context.Background(), testScope, "tenant-a", "file-1")
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v; want the refusal passed through", err)
			}
			if deleted := backend.deletedKeys(); len(deleted) != 0 {
				t.Errorf("blob deleted on a refused delete: %v", deleted)
			}
			if len(rec.execCalls) != 2 {
				t.Fatalf("exec calls = %d; want soft delete then restore", len(rec.execCalls))
			}
			last := rec.lastExec(t)
			mustContainSQL(t, last.query, "UPDATE sys_files SET deleted_at = NULL", "tenant_id = ?", "id = ?")
			if len(last.args) != 2 || last.args[0] != "tenant-a" || last.args[1] != "file-1" {
				t.Errorf("restore args = %v; want [tenant-a file-1]", last.args)
			}
		})
	}

	t.Run("restore itself fails ⇒ both errors surface", func(t *testing.T) {
		veto := refusedWrites[0].err
		guard := &fakeGuard{entity: "orders", setRefsErr: fmt.Errorf("services.SetFileRefs: %w", veto)}
		svc, rec := newTestService(t, guard, &fakeBackend{}, Limits{MimeOK: allowAllMime})
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)
		rec.queueExec(1, nil)                       // SoftDelete
		rec.queueExec(0, errors.New("server gone")) // Restore
		err := svc.Delete(context.Background(), testScope, "tenant-a", "file-1")
		if !errors.Is(err, veto) || !strings.Contains(err.Error(), "server gone") {
			t.Errorf("err = %v; want the veto AND the restore failure", err)
		}
	})

	t.Run("an ambiguous sync failure keeps the old handling (no restore, blob kept)", func(t *testing.T) {
		guard := &fakeGuard{entity: "orders", setRefsErr: errors.New("record update failed")}
		backend := &fakeBackend{}
		svc, rec := newTestService(t, guard, backend, Limits{MimeOK: allowAllMime})
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)
		err := svc.Delete(context.Background(), testScope, "tenant-a", "file-1")
		if err == nil || !strings.Contains(err.Error(), "sync record field") {
			t.Fatalf("err = %v; want wrapped sync failure", err)
		}
		if len(rec.execCalls) != 1 {
			t.Errorf("exec calls = %d; want only the soft delete", len(rec.execCalls))
		}
		if deleted := backend.deletedKeys(); len(deleted) != 0 {
			t.Errorf("blob deleted after a failed sync: %v", deleted)
		}
	})
}

// ---- constructor defaults + pure helpers --------------------------------------------------------

func TestNewService_Defaults(t *testing.T) {
	t.Run("không có whitelist → fail closed (từ chối mọi mime)", func(t *testing.T) {
		svc := NewService(nil, &fakeBackend{}, &fakeGuard{}, Limits{}, func() string { return "x" })
		if svc.limits.MimeOK("image/png") || svc.limits.MimeOK("") {
			t.Error("nil MimeOK must reject everything, not accept everything")
		}
	})

	t.Run("newID nil → UUID mặc định", func(t *testing.T) {
		svc := NewService(nil, &fakeBackend{}, &fakeGuard{}, Limits{}, nil)
		a, b := svc.newID(), svc.newID()
		if len(a) != 36 || a == b {
			t.Errorf("default id gen = %q/%q; want distinct UUIDs", a, b)
		}
	})
}

func TestStorageKey(t *testing.T) {
	if got := storageKey("tenant-a", "file-1"); got != "tenant-a/file-1" {
		t.Errorf("storageKey = %q; want tenant-a/file-1", got)
	}
}

func TestSanitizeDisplayName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "tên thường giữ nguyên", in: "report.pdf", want: "report.pdf"},
		{name: "trim khoảng trắng", in: "  report.pdf  ", want: "report.pdf"},
		{name: "strip đường dẫn unix", in: "/etc/passwd", want: "passwd"},
		{name: "strip đường dẫn windows", in: `C:\Users\x\avatar.png`, want: "avatar.png"},
		{name: "strip đường dẫn trộn", in: `a/b\c.txt`, want: "c.txt"},
		{name: "rỗng → fallback", in: "", want: "file"},
		{name: "toàn khoảng trắng → fallback", in: "   ", want: "file"},
		{name: "kết thúc bằng separator → fallback", in: "uploads/", want: "file"},
		{name: "unicode giữ nguyên", in: "báo cáo quý.xlsx", want: "báo cáo quý.xlsx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeDisplayName(tt.in); got != tt.want {
				t.Errorf("sanitizeDisplayName(%q) = %q; want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPublicURL(t *testing.T) {
	if got := PublicURL("file-1"); got != "/api/v1/files/file-1" {
		t.Errorf("PublicURL = %q; want /api/v1/files/file-1", got)
	}
}
