package notes

// Default-build unit tests for the sys_record_notes Repository over the fake driver
// (fakedb_test.go): tenant_id pin + deleted_at guard of every statement, named-exec arg ordering,
// the cross-record scoping of GetForRecord, and the ErrNoteNotFound mapping (no rows / zero rows
// affected → 404, no cross-tenant/cross-record oracle).

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

var testTime = time.Date(2026, 7, 29, 9, 30, 0, 0, time.UTC)

// noteCols matches the SELECT column list of ListByRecord/GetForRecord (order matters).
func noteCols() []string {
	return []string{
		"id", "tenant_id", "entity_name", "record_id", "body",
		"created_by", "created_at", "updated_at", "deleted_at",
	}
}

func sampleNoteRow(id, body string) []driver.Value {
	return []driver.Value{
		id, "tenant-a", "crm_accounts", "rec-1", body,
		"user-1", testTime, testTime, nil,
	}
}

func TestNotesRepository_Insert(t *testing.T) {
	t.Run("named insert giữ nguyên thứ tự cột + tenant", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		err := repo.Insert(context.Background(), Note{
			ID: "note-1", TenantID: "tenant-a", EntityName: "crm_accounts", RecordID: "rec-1",
			Body: "đã gọi, khách hẹn tuần sau", CreatedBy: "user-1",
			CreatedAt: testTime, UpdatedAt: testTime,
		})
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		call := rec.lastExec(t)
		mustContainSQL(t, call.query, "INSERT INTO sys_record_notes")
		want := []driver.Value{
			"note-1", "tenant-a", "crm_accounts", "rec-1",
			"đã gọi, khách hẹn tuần sau", "user-1", testTime, testTime,
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
		err := repo.Insert(context.Background(), Note{ID: "note-1", TenantID: "tenant-a"})
		if err == nil || !strings.Contains(err.Error(), "notes.Insert") {
			t.Errorf("err = %v; want wrapped with notes.Insert", err)
		}
	})
}

func TestNotesRepository_ListByRecord(t *testing.T) {
	t.Run("filter đủ 3 chiều + mới nhất trước + limit/offset", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueQuery(noteCols(), [][]driver.Value{
			sampleNoteRow("note-2", "note mới"), sampleNoteRow("note-1", "note cũ"),
		}, nil)
		got, err := repo.ListByRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1", 20, 0)
		if err != nil {
			t.Fatalf("ListByRecord: %v", err)
		}
		if len(got) != 2 || got[0].ID != "note-2" || got[1].ID != "note-1" {
			t.Errorf("notes = %+v; want note-2, note-1 in driver order (newest first)", got)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query,
			"tenant_id = ?", "entity_name = ?", "record_id = ?",
			"deleted_at IS NULL", "ORDER BY created_at DESC, id DESC", "LIMIT ? OFFSET ?")
		wantArgs := []driver.Value{"tenant-a", "crm_accounts", "rec-1", int64(20), int64(0)}
		if len(call.args) != len(wantArgs) {
			t.Fatalf("args = %v; want %v", call.args, wantArgs)
		}
	})

	t.Run("không có ghi chú → slice rỗng, không lỗi", func(t *testing.T) {
		db, _ := newFakeDB(t)
		repo := NewRepository(db)
		got, err := repo.ListByRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1", 20, 0)
		if err != nil || len(got) != 0 {
			t.Errorf("ListByRecord = (%v, %v); want ([], nil)", got, err)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueQuery(nil, nil, errors.New("timeout"))
		_, err := repo.ListByRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1", 20, 0)
		if err == nil || !strings.Contains(err.Error(), "notes.ListByRecord") {
			t.Errorf("err = %v; want wrapped with notes.ListByRecord", err)
		}
	})
}

func TestNotesRepository_CountByRecord(t *testing.T) {
	t.Run("đếm tenant-pinned", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueQuery([]string{"count"}, [][]driver.Value{{int64(3)}}, nil)
		got, err := repo.CountByRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1")
		if err != nil || got != 3 {
			t.Fatalf("CountByRecord = (%d, %v); want (3, nil)", got, err)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query, "COUNT(*)", "tenant_id = ?", "entity_name = ?", "record_id = ?", "deleted_at IS NULL")
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueQuery(nil, nil, errors.New("timeout"))
		_, err := repo.CountByRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1")
		if err == nil || !strings.Contains(err.Error(), "notes.CountByRecord") {
			t.Errorf("err = %v; want wrapped with notes.CountByRecord", err)
		}
	})
}

func TestNotesRepository_GetForRecord(t *testing.T) {
	t.Run("id lạ / khác tenant / khác record / đã xoá → ErrNoteNotFound", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		_, err := repo.GetForRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1", "note-x")
		if !errors.Is(err, ErrNoteNotFound) {
			t.Fatalf("err = %v; want ErrNoteNotFound", err)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query,
			"FROM sys_record_notes", "tenant_id = ?", "entity_name = ?", "record_id = ?", "id = ?", "deleted_at IS NULL")
		wantArgs := []driver.Value{"tenant-a", "crm_accounts", "rec-1", "note-x"}
		if len(call.args) != len(wantArgs) {
			t.Fatalf("args = %v; want %v", call.args, wantArgs)
		}
		for i := range wantArgs {
			if call.args[i] != wantArgs[i] {
				t.Errorf("args[%d] = %v; want %v", i, call.args[i], wantArgs[i])
			}
		}
	})

	t.Run("row sống trả đủ", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueQuery(noteCols(), [][]driver.Value{sampleNoteRow("note-1", "nội dung")}, nil)
		got, err := repo.GetForRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1", "note-1")
		if err != nil {
			t.Fatalf("GetForRecord: %v", err)
		}
		if got.ID != "note-1" || got.Body != "nội dung" || got.CreatedBy != "user-1" {
			t.Errorf("note mismatched: %+v", got)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueQuery(nil, nil, errors.New("connection refused"))
		_, err := repo.GetForRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1", "note-1")
		if err == nil || !strings.Contains(err.Error(), "notes.GetForRecord") {
			t.Errorf("err = %v; want wrapped with notes.GetForRecord", err)
		}
	})
}

func TestNotesRepository_SoftDelete(t *testing.T) {
	t.Run("tombstone tenant-pinned", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		if err := repo.SoftDelete(context.Background(), "tenant-a", "note-1"); err != nil {
			t.Fatalf("SoftDelete: %v", err)
		}
		call := rec.lastExec(t)
		mustContainSQL(t, call.query, "UPDATE sys_record_notes SET deleted_at = ?", "tenant_id = ?", "deleted_at IS NULL")
		if len(call.args) != 3 {
			t.Fatalf("args = %v; want 3", call.args)
		}
		if _, ok := call.args[0].(time.Time); !ok {
			t.Errorf("args[0] (deleted_at) = %T; want time.Time", call.args[0])
		}
		if call.args[1] != "tenant-a" || call.args[2] != "note-1" {
			t.Errorf("args[1:] = %v; want [tenant-a note-1]", call.args[1:])
		}
	})

	t.Run("double-delete / khác tenant → ErrNoteNotFound", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueExec(0, nil)
		if err := repo.SoftDelete(context.Background(), "tenant-a", "note-1"); !errors.Is(err, ErrNoteNotFound) {
			t.Errorf("err = %v; want ErrNoteNotFound", err)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		db, rec := newFakeDB(t)
		repo := NewRepository(db)
		rec.queueExec(0, errors.New("server gone"))
		err := repo.SoftDelete(context.Background(), "tenant-a", "note-1")
		if err == nil || !strings.Contains(err.Error(), "notes.SoftDelete") {
			t.Errorf("err = %v; want wrapped with notes.SoftDelete", err)
		}
	})
}
