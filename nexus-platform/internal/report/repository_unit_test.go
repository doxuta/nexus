package report

// Default-build unit tests for SQLRepository over the fake driver (fakedb_test.go): they pin the
// tenant_id filter + deleted_at guard of every generated statement, the exact arg ordering, the
// NULL handling of the audit columns, and the error mapping (no rows / zero rows affected →
// ErrReportNotFound; driver errors → wrapped with the repo method name). Real-MySQL behavior
// (ENUM coercion, JSON column round-trip) remains integration-only.

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC)

func newTestRepo(t *testing.T) (*SQLRepository, *fakeDB) {
	t.Helper()
	db, rec := newFakeDB(t)
	repo := NewSQLRepository(db)
	repo.now = func() time.Time { return fixedNow }
	return repo, rec
}

// reportCols matches the SELECT column list of Get/List (order matters for row scanning).
func reportCols() []string {
	return []string{
		"id", "tenant_id", "name", "entity_name", "type", "config", "is_active",
		"created_by", "updated_by", "created_at", "updated_at",
	}
}

func sampleReportRow() []driver.Value {
	return []driver.Value{
		"rep-1", "tenant-a", "Doanh thu theo stage", "orders", "summary",
		[]byte(`{"groupBy":"stage"}`), true, "user-1", nil, fixedNow, fixedNow,
	}
}

func TestReportRepository_Create(t *testing.T) {
	t.Run("insert tenant-pinned với audit + clock cố định", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		id, err := repo.Create(context.Background(), CreateInput{
			TenantID:   "tenant-a",
			Name:       "Doanh thu theo stage",
			EntityName: "orders",
			Type:       TypeSummary,
			Config:     []byte(`{"groupBy":"stage"}`),
			IsActive:   true,
			CreatedBy:  "user-1",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if len(id) != 36 {
			t.Errorf("id = %q; want UUID (36 chars)", id)
		}
		call := rec.lastExec(t)
		mustContainSQL(t, call.query, "INSERT INTO sys_reports")
		if len(call.args) != 11 {
			t.Fatalf("args = %d; want 11 (%v)", len(call.args), call.args)
		}
		if call.args[0] != id {
			t.Errorf("args[0] = %v; want returned id %q", call.args[0], id)
		}
		if call.args[1] != "tenant-a" {
			t.Errorf("args[1] (tenant_id) = %v; want tenant-a", call.args[1])
		}
		if call.args[4] != "summary" {
			t.Errorf("args[4] (type) = %v; want summary", call.args[4])
		}
		if call.args[5] != `{"groupBy":"stage"}` {
			t.Errorf("args[5] (config) = %v; want raw JSON string", call.args[5])
		}
		if call.args[6] != true {
			t.Errorf("args[6] (is_active) = %v; want true", call.args[6])
		}
		if call.args[7] != "user-1" || call.args[8] != "user-1" {
			t.Errorf("created_by/updated_by = %v/%v; want user-1/user-1", call.args[7], call.args[8])
		}
		for _, i := range []int{9, 10} {
			ts, ok := call.args[i].(time.Time)
			if !ok || !ts.Equal(fixedNow) {
				t.Errorf("args[%d] (timestamp) = %v; want %v", i, call.args[i], fixedNow)
			}
		}
	})

	t.Run("CreatedBy trắng → NULL audit", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if _, err := repo.Create(context.Background(), CreateInput{
			TenantID: "tenant-a", Name: "r", EntityName: "orders",
			Type: TypeTabular, Config: []byte(`{}`), CreatedBy: "   ",
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		call := rec.lastExec(t)
		if call.args[7] != nil || call.args[8] != nil {
			t.Errorf("created_by/updated_by = %v/%v; want NULL/NULL", call.args[7], call.args[8])
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, errors.New("duplicate entry"))
		_, err := repo.Create(context.Background(), CreateInput{
			TenantID: "tenant-a", Name: "r", EntityName: "orders",
			Type: TypeTabular, Config: []byte(`{}`),
		})
		if err == nil || !strings.Contains(err.Error(), "report.SQLRepository.Create") {
			t.Errorf("err = %v; want wrapped with report.SQLRepository.Create", err)
		}
	})
}

func TestReportRepository_Get(t *testing.T) {
	t.Run("id lạ / khác tenant → ErrReportNotFound", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		// Default reply: zero rows — exactly what a cross-tenant or unknown id produces.
		_, err := repo.Get(context.Background(), "tenant-a", "rep-x")
		if !errors.Is(err, ErrReportNotFound) {
			t.Fatalf("err = %v; want ErrReportNotFound", err)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query, "FROM sys_reports", "tenant_id = ?", "deleted_at IS NULL")
		if len(call.args) != 2 || call.args[0] != "tenant-a" || call.args[1] != "rep-x" {
			t.Errorf("args = %v; want [tenant-a rep-x]", call.args)
		}
	})

	t.Run("row sống được chiếu về Record", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueQuery(reportCols(), [][]driver.Value{sampleReportRow()}, nil)
		got, err := repo.Get(context.Background(), "tenant-a", "rep-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ID != "rep-1" || got.TenantID != "tenant-a" || got.Type != "summary" || !got.IsActive {
			t.Errorf("record mismatched: %+v", got)
		}
		if string(got.Config) != `{"groupBy":"stage"}` {
			t.Errorf("Config = %s; want forwarded verbatim", got.Config)
		}
		if got.CreatedBy == nil || *got.CreatedBy != "user-1" || got.UpdatedBy != nil {
			t.Errorf("audit = %v/%v; want user-1/nil", got.CreatedBy, got.UpdatedBy)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueQuery(nil, nil, errors.New("connection refused"))
		_, err := repo.Get(context.Background(), "tenant-a", "rep-1")
		if err == nil || !strings.Contains(err.Error(), "report.SQLRepository.Get") {
			t.Errorf("err = %v; want wrapped with report.SQLRepository.Get", err)
		}
	})
}

func TestReportRepository_List(t *testing.T) {
	t.Run("danh sách tenant-scoped, mới nhất trước", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		row2 := sampleReportRow()
		row2[0] = "rep-2"
		rec.queueQuery(reportCols(), [][]driver.Value{sampleReportRow(), row2}, nil)
		got, err := repo.List(context.Background(), "tenant-a")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 || got[0].ID != "rep-1" || got[1].ID != "rep-2" {
			t.Errorf("records = %+v; want rep-1, rep-2 in driver order", got)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query, "tenant_id = ?", "deleted_at IS NULL", "ORDER BY created_at DESC, id DESC")
		if len(call.args) != 1 || call.args[0] != "tenant-a" {
			t.Errorf("args = %v; want [tenant-a]", call.args)
		}
	})

	t.Run("tenant không có report → slice rỗng, không lỗi", func(t *testing.T) {
		repo, _ := newTestRepo(t)
		got, err := repo.List(context.Background(), "tenant-empty")
		if err != nil || len(got) != 0 {
			t.Errorf("List = (%v, %v); want ([], nil)", got, err)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueQuery(nil, nil, errors.New("timeout"))
		_, err := repo.List(context.Background(), "tenant-a")
		if err == nil || !strings.Contains(err.Error(), "report.SQLRepository.List") {
			t.Errorf("err = %v; want wrapped with report.SQLRepository.List", err)
		}
	})
}

func TestReportRepository_Update(t *testing.T) {
	in := UpdateInput{
		Name: "Đổi tên", EntityName: "orders", Type: TypeMatrix,
		Config: []byte(`{"groupBy":"owner"}`), IsActive: false, UpdatedBy: "user-2",
	}

	t.Run("update tenant-pinned với thứ tự args đúng", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if err := repo.Update(context.Background(), "tenant-a", "rep-1", in); err != nil {
			t.Fatalf("Update: %v", err)
		}
		call := rec.lastExec(t)
		mustContainSQL(t, call.query, "UPDATE sys_reports", "tenant_id = ?", "deleted_at IS NULL")
		want := []driver.Value{"Đổi tên", "orders", "matrix", `{"groupBy":"owner"}`, false, "user-2", "tenant-a", "rep-1"}
		if len(call.args) != len(want) {
			t.Fatalf("args = %v; want %v", call.args, want)
		}
		for i := range want {
			if call.args[i] != want[i] {
				t.Errorf("args[%d] = %v; want %v", i, call.args[i], want[i])
			}
		}
	})

	t.Run("UpdatedBy trắng → NULL", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		blank := in
		blank.UpdatedBy = ""
		if err := repo.Update(context.Background(), "tenant-a", "rep-1", blank); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if call := rec.lastExec(t); call.args[5] != nil {
			t.Errorf("args[5] (updated_by) = %v; want NULL", call.args[5])
		}
	})

	t.Run("0 row khớp (khác tenant / đã xoá) → ErrReportNotFound", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, nil)
		if err := repo.Update(context.Background(), "tenant-b", "rep-1", in); !errors.Is(err, ErrReportNotFound) {
			t.Errorf("err = %v; want ErrReportNotFound", err)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, errors.New("lock wait timeout"))
		err := repo.Update(context.Background(), "tenant-a", "rep-1", in)
		if err == nil || !strings.Contains(err.Error(), "report.SQLRepository.Update") {
			t.Errorf("err = %v; want wrapped with report.SQLRepository.Update", err)
		}
	})
}

func TestReportRepository_Delete(t *testing.T) {
	t.Run("soft-delete stamp clock cố định, tenant-pinned", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if err := repo.Delete(context.Background(), "tenant-a", "rep-1"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		call := rec.lastExec(t)
		mustContainSQL(t, call.query, "UPDATE sys_reports SET deleted_at = ?", "tenant_id = ?", "deleted_at IS NULL")
		if len(call.args) != 3 {
			t.Fatalf("args = %v; want 3", call.args)
		}
		if ts, ok := call.args[0].(time.Time); !ok || !ts.Equal(fixedNow) {
			t.Errorf("args[0] (deleted_at) = %v; want %v", call.args[0], fixedNow)
		}
		if call.args[1] != "tenant-a" || call.args[2] != "rep-1" {
			t.Errorf("args[1:] = %v; want [tenant-a rep-1]", call.args[1:])
		}
	})

	t.Run("double-delete / khác tenant → ErrReportNotFound", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, nil)
		if err := repo.Delete(context.Background(), "tenant-a", "rep-1"); !errors.Is(err, ErrReportNotFound) {
			t.Errorf("err = %v; want ErrReportNotFound", err)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, errors.New("server gone"))
		err := repo.Delete(context.Background(), "tenant-a", "rep-1")
		if err == nil || !strings.Contains(err.Error(), "report.SQLRepository.Delete") {
			t.Errorf("err = %v; want wrapped with report.SQLRepository.Delete", err)
		}
	})
}
