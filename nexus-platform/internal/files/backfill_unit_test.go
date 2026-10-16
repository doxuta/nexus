package files

// Default-build unit tests for the sha256 backfill (#54, specs/036 §2.5): rows uploaded before
// migration 000068 carry sha256 NULL; BackfillHashes digests their blobs so the restore drill can
// compare them byte-for-byte. Covered: every row query tenant-pinned, keyset paging by id, digest of
// the blob bytes, never overwriting an existing digest, unreadable blobs reported not fatal, and a
// DB error surfaced wrapped.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func unhashedRow(id, path string) []driver.Value { return []driver.Value{id, path} }

var unhashedCols = []string{"id", "path"}

func TestFilesRepository_BackfillQueries(t *testing.T) {
	t.Run("TenantsWithUnhashed: chỉ trả tenant id, lọc dòng sống chưa có băm", func(t *testing.T) {
		db, rec := newFakeDB(t)
		rec.queueQuery([]string{"tenant_id"}, [][]driver.Value{{"tenant-a"}, {"tenant-b"}}, nil)
		got, err := NewRepository(db).TenantsWithUnhashed(context.Background())
		if err != nil {
			t.Fatalf("TenantsWithUnhashed: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"tenant-a", "tenant-b"}) {
			t.Errorf("tenants = %v; want [tenant-a tenant-b]", got)
		}
		mustContainSQL(t, rec.lastQuery(t).query, "SELECT DISTINCT tenant_id", "sha256 IS NULL", "deleted_at IS NULL")
	})

	t.Run("ListUnhashed: tenant-pinned, keyset theo id", func(t *testing.T) {
		db, rec := newFakeDB(t)
		rec.queueQuery(unhashedCols, [][]driver.Value{unhashedRow("f1", "tenant-a/f1")}, nil)
		got, err := NewRepository(db).ListUnhashed(context.Background(), "tenant-a", "f0", 50)
		if err != nil {
			t.Fatalf("ListUnhashed: %v", err)
		}
		if len(got) != 1 || got[0].ID != "f1" || got[0].Path != "tenant-a/f1" {
			t.Errorf("rows = %+v; want [f1 tenant-a/f1]", got)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query, "tenant_id = ?", "sha256 IS NULL", "deleted_at IS NULL", "id > ?", "ORDER BY id", "LIMIT ?")
		if want := []driver.Value{"tenant-a", "f0", int64(50)}; !reflect.DeepEqual(call.args, want) {
			t.Errorf("args = %v; want %v", call.args, want)
		}
	})

	t.Run("SetSha256: tenant-pinned, không bao giờ ghi đè mã băm đã có", func(t *testing.T) {
		db, rec := newFakeDB(t)
		if err := NewRepository(db).SetSha256(context.Background(), "tenant-a", "f1", sumHelloWorld); err != nil {
			t.Fatalf("SetSha256: %v", err)
		}
		call := rec.lastExec(t)
		mustContainSQL(t, call.query, "UPDATE sys_files SET sha256 = ?", "tenant_id = ?", "id = ?", "sha256 IS NULL")
		if want := []driver.Value{sumHelloWorld, "tenant-a", "f1"}; !reflect.DeepEqual(call.args, want) {
			t.Errorf("args = %v; want %v", call.args, want)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		db, rec := newFakeDB(t)
		rec.queueQuery(nil, nil, errors.New("server gone"))
		_, err := NewRepository(db).TenantsWithUnhashed(context.Background())
		if err == nil || !strings.Contains(err.Error(), "files.TenantsWithUnhashed") {
			t.Errorf("err = %v; want wrapped with files.TenantsWithUnhashed", err)
		}
	})
}

func TestFileService_BackfillHashes(t *testing.T) {
	t.Run("băm blob của từng tenant, blob thiếu thì báo chứ không dừng", func(t *testing.T) {
		backend := &fakeBackend{blobs: map[string]string{
			"tenant-a/f1": "hello world",
			"tenant-b/f3": "hello World",
		}}
		svc, rec := newTestService(t, &fakeGuard{}, backend, Limits{MimeOK: allowAllMime})
		rec.queueQuery([]string{"tenant_id"}, [][]driver.Value{{"tenant-a"}, {"tenant-b"}}, nil)
		rec.queueQuery(unhashedCols, [][]driver.Value{
			unhashedRow("f1", "tenant-a/f1"),
			unhashedRow("f2", "tenant-a/f2"), // blob gone
		}, nil)
		rec.queueQuery(unhashedCols, [][]driver.Value{unhashedRow("f3", "tenant-b/f3")}, nil)

		st, err := svc.BackfillHashes(context.Background())
		if err != nil {
			t.Fatalf("BackfillHashes: %v", err)
		}
		if st.Hashed != 2 {
			t.Errorf("Hashed = %d; want 2", st.Hashed)
		}
		if !reflect.DeepEqual(st.Unreadable, []string{"tenant-a/f2"}) {
			t.Errorf("Unreadable = %v; want [tenant-a/f2]", st.Unreadable)
		}
		// Each row's digest lands on ITS tenant + id — never another tenant's row.
		want := [][]driver.Value{
			{sumHelloWorld, "tenant-a", "f1"},
			{"db4067cec62c58bf8b2f8982071e77c082da9e00924bf3631f3b024fa54e7d7e", "tenant-b", "f3"},
		}
		if len(rec.execCalls) != len(want) {
			t.Fatalf("exec calls = %d; want %d (%v)", len(rec.execCalls), len(want), rec.execCalls)
		}
		for i, w := range want {
			if !reflect.DeepEqual(rec.execCalls[i].args, w) {
				t.Errorf("SetSha256[%d] args = %v; want %v", i, rec.execCalls[i].args, w)
			}
		}
	})

	t.Run("trang đầy → đọc tiếp từ id cuối (keyset), trang ngắn → dừng", func(t *testing.T) {
		blobs := map[string]string{}
		page := make([][]driver.Value, backfillBatch)
		for i := range page {
			id := fmt.Sprintf("f%04d", i)
			blobs["tenant-a/"+id] = id
			page[i] = unhashedRow(id, "tenant-a/"+id)
		}
		blobs["tenant-a/f9999"] = "last"
		svc, rec := newTestService(t, &fakeGuard{}, &fakeBackend{blobs: blobs}, Limits{MimeOK: allowAllMime})
		rec.queueQuery([]string{"tenant_id"}, [][]driver.Value{{"tenant-a"}}, nil)
		rec.queueQuery(unhashedCols, page, nil)
		rec.queueQuery(unhashedCols, [][]driver.Value{unhashedRow("f9999", "tenant-a/f9999")}, nil)

		st, err := svc.BackfillHashes(context.Background())
		if err != nil {
			t.Fatalf("BackfillHashes: %v", err)
		}
		if st.Hashed != backfillBatch+1 {
			t.Errorf("Hashed = %d; want %d", st.Hashed, backfillBatch+1)
		}
		// tenants + 2 pages, and NOT a third page after the short one.
		if len(rec.queryCalls) != 3 {
			t.Fatalf("query calls = %d; want 3", len(rec.queryCalls))
		}
		second := rec.queryCalls[2].args
		if second[0] != "tenant-a" || second[1] != fmt.Sprintf("f%04d", backfillBatch-1) {
			t.Errorf("2nd page args = %v; want [tenant-a f%04d …]", second, backfillBatch-1)
		}
	})

	t.Run("lỗi DB → trả lỗi đã wrap, không nuốt", func(t *testing.T) {
		svc, rec := newTestService(t, &fakeGuard{}, &fakeBackend{}, Limits{MimeOK: allowAllMime})
		rec.queueQuery(nil, nil, errors.New("server gone"))
		_, err := svc.BackfillHashes(context.Background())
		if err == nil || !strings.Contains(err.Error(), "files.BackfillHashes") {
			t.Errorf("err = %v; want wrapped with files.BackfillHashes", err)
		}
	})
}
