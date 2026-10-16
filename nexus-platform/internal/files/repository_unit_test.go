package files

// Default-build unit tests for the sys_files Repository over the fake driver (fakedb_test.go):
// tenant_id pin + deleted_at guard of every statement, named-exec arg ordering, and the
// ErrFileNotFound mapping (no rows / zero rows affected → 404, no cross-tenant oracle).

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

var testTime = time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC)

// fileCols matches the SELECT column list of GetByID/ListByRecordField (order matters).
func fileCols() []string {
	return []string{
		"id", "tenant_id", "entity_name", "record_id", "field_name", "name",
		"size", "mime", "path", "created_by", "created_at", "deleted_at",
	}
}

func sampleFileRow(id, name string) []driver.Value {
	return []driver.Value{
		id, "tenant-a", "orders", "rec-1", "attachments", name,
		int64(11), "image/png", "tenant-a/" + id, "user-1", testTime, nil,
	}
}

func TestFilesRepository_Insert(t *testing.T) {
	t.Run("named insert giữ nguyên thứ tự cột + tenant", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		err := repo.Insert(context.Background(), File{
			ID: "file-1", TenantID: "tenant-a", EntityName: "orders", RecordID: "rec-1",
			FieldName: "attachments", Name: "avatar.png", Size: 11, Mime: "image/png",
			Path: "tenant-a/file-1", CreatedBy: "user-1", Sha256: sumHelloWorld,
		})
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		call := rec.lastExec(t)
		// NULLIF: a row written without a digest stores NULL (the drill's "size-only"), never ''.
		mustContainSQL(t, call.query, "INSERT INTO sys_files", "NULLIF(?, '')")
		want := []driver.Value{
			"file-1", "tenant-a", "orders", "rec-1", "attachments",
			"avatar.png", int64(11), "image/png", "tenant-a/file-1", "user-1", sumHelloWorld,
		}
		if len(call.args) != len(want) {
			t.Fatalf("args = %v; want %v", call.args, want)
		}
		for i := range want {
			if call.args[i] != want[i] {
				t.Errorf("args[%d] = %v; want %v", i, call.args[i], want[i])
			}
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueExec(0, errors.New("duplicate entry"))
		err := repo.Insert(context.Background(), File{ID: "file-1", TenantID: "tenant-a"})
		if err == nil || !strings.Contains(err.Error(), "files.Insert") {
			t.Errorf("err = %v; want wrapped with files.Insert", err)
		}
	})
}

func TestFilesRepository_GetByID(t *testing.T) {
	t.Run("id lạ / khác tenant / đã xoá → ErrFileNotFound", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		_, err := repo.GetByID(context.Background(), "tenant-a", "file-x")
		if !errors.Is(err, ErrFileNotFound) {
			t.Fatalf("err = %v; want ErrFileNotFound", err)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query, "FROM sys_files", "tenant_id = ?", "deleted_at IS NULL")
		if len(call.args) != 2 || call.args[0] != "tenant-a" || call.args[1] != "file-x" {
			t.Errorf("args = %v; want [tenant-a file-x]", call.args)
		}
	})

	t.Run("row sống trả đủ catalog", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueQuery(fileCols(), [][]driver.Value{sampleFileRow("file-1", "avatar.png")}, nil)
		got, err := repo.GetByID(context.Background(), "tenant-a", "file-1")
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.ID != "file-1" || got.TenantID != "tenant-a" || got.Name != "avatar.png" {
			t.Errorf("file mismatched: %+v", got)
		}
		if got.Size != 11 || got.Mime != "image/png" || got.Path != "tenant-a/file-1" {
			t.Errorf("blob fields mismatched: %+v", got)
		}
		if got.DeletedAt.Valid {
			t.Errorf("DeletedAt = %v; want NULL", got.DeletedAt)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueQuery(nil, nil, errors.New("connection refused"))
		_, err := repo.GetByID(context.Background(), "tenant-a", "file-1")
		if err == nil || !strings.Contains(err.Error(), "files.GetByID") {
			t.Errorf("err = %v; want wrapped with files.GetByID", err)
		}
	})
}

func TestFilesRepository_ListByRecordField(t *testing.T) {
	t.Run("filter đủ 4 chiều + thứ tự cũ nhất trước", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueQuery(fileCols(), [][]driver.Value{
			sampleFileRow("file-1", "a.png"), sampleFileRow("file-2", "b.png"),
		}, nil)
		got, err := repo.ListByRecordField(context.Background(), "tenant-a", "orders", "rec-1", "attachments")
		if err != nil {
			t.Fatalf("ListByRecordField: %v", err)
		}
		if len(got) != 2 || got[0].ID != "file-1" || got[1].ID != "file-2" {
			t.Errorf("files = %+v; want file-1, file-2 in driver order", got)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query,
			"tenant_id = ?", "entity_name = ?", "record_id = ?", "field_name = ?",
			"deleted_at IS NULL", "ORDER BY created_at ASC, id ASC")
		wantArgs := []driver.Value{"tenant-a", "orders", "rec-1", "attachments"}
		if len(call.args) != len(wantArgs) {
			t.Fatalf("args = %v; want %v", call.args, wantArgs)
		}
		for i := range wantArgs {
			if call.args[i] != wantArgs[i] {
				t.Errorf("args[%d] = %v; want %v", i, call.args[i], wantArgs[i])
			}
		}
	})

	t.Run("không có file → slice rỗng, không lỗi", func(t *testing.T) {
		db, _ := newFakeDB(t)
		repo := NewRepository(db)
		got, err := repo.ListByRecordField(context.Background(), "tenant-a", "orders", "rec-1", "attachments")
		if err != nil || len(got) != 0 {
			t.Errorf("ListByRecordField = (%v, %v); want ([], nil)", got, err)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueQuery(nil, nil, errors.New("timeout"))
		_, err := repo.ListByRecordField(context.Background(), "tenant-a", "orders", "rec-1", "attachments")
		if err == nil || !strings.Contains(err.Error(), "files.ListByRecordField") {
			t.Errorf("err = %v; want wrapped with files.ListByRecordField", err)
		}
	})
}

func TestFilesRepository_SoftDelete(t *testing.T) {
	t.Run("tombstone tenant-pinned", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		if err := repo.SoftDelete(context.Background(), "tenant-a", "file-1"); err != nil {
			t.Fatalf("SoftDelete: %v", err)
		}
		call := rec.lastExec(t)
		mustContainSQL(t, call.query, "UPDATE sys_files SET deleted_at = ?", "tenant_id = ?", "deleted_at IS NULL")
		if len(call.args) != 3 {
			t.Fatalf("args = %v; want 3", call.args)
		}
		// The repo stamps time.Now (not injectable) — assert the type, then the tenant pin.
		if _, ok := call.args[0].(time.Time); !ok {
			t.Errorf("args[0] (deleted_at) = %T; want time.Time", call.args[0])
		}
		if call.args[1] != "tenant-a" || call.args[2] != "file-1" {
			t.Errorf("args[1:] = %v; want [tenant-a file-1]", call.args[1:])
		}
	})

	t.Run("double-delete / khác tenant → ErrFileNotFound", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueExec(0, nil)
		if err := repo.SoftDelete(context.Background(), "tenant-a", "file-1"); !errors.Is(err, ErrFileNotFound) {
			t.Errorf("err = %v; want ErrFileNotFound", err)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueExec(0, errors.New("server gone"))
		err := repo.SoftDelete(context.Background(), "tenant-a", "file-1")
		if err == nil || !strings.Contains(err.Error(), "files.SoftDelete") {
			t.Errorf("err = %v; want wrapped with files.SoftDelete", err)
		}
	})
}

func TestFilesRepository_Restore(t *testing.T) {
	t.Run("un-tombstone tenant-pinned", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		if err := repo.Restore(context.Background(), "tenant-a", "file-1"); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		call := rec.lastExec(t)
		mustContainSQL(t, call.query, "UPDATE sys_files SET deleted_at = NULL", "tenant_id = ?", "id = ?", "deleted_at IS NOT NULL")
		if len(call.args) != 2 || call.args[0] != "tenant-a" || call.args[1] != "file-1" {
			t.Errorf("args = %v; want [tenant-a file-1]", call.args)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueExec(0, errors.New("server gone"))
		err := repo.Restore(context.Background(), "tenant-a", "file-1")
		if err == nil || !strings.Contains(err.Error(), "files.Restore") {
			t.Errorf("err = %v; want wrapped with files.Restore", err)
		}
	})
}
