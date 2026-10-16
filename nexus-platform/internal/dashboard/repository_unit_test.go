package dashboard

// Default-build unit tests for the dashboard SQLRepository over the fake driver
// (fakedb_test.go): tenant_id pin + deleted_at guard of every statement, arg ordering, NULL
// handling of description/created_by, the widgets "[]" normalization, and the not-found error
// mapping. NOTE: widget CONFIG validation is service-level logic and already unit-covered by
// internal/services/dash_widget_schema_test.go — it is deliberately not duplicated here; this
// package's own logic is exactly the sys_dashboards storage layer.

import (
	"context"
	"database/sql/driver"
	"encoding/json"
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

// dashCols matches the SELECT column list of Get/List (order matters for row scanning).
func dashCols() []string {
	return []string{"id", "tenant_id", "name", "description", "widgets", "created_by", "created_at", "updated_at"}
}

func sampleDashRow() []driver.Value {
	return []driver.Value{
		"dash-1", "tenant-a", "Tổng quan bán hàng", "phễu theo stage",
		[]byte(`[{"type":"metric"}]`), "user-1", fixedNow, fixedNow,
	}
}

// TestRowToRecord covers the row→admin-view projection: an empty/NULL widgets column is
// normalized to the JSON literal "[]" (a dashboard is always an array of widgets), other fields
// forward verbatim.
func TestRowToRecord(t *testing.T) {
	t.Run("widgets rỗng chuẩn hoá thành []", func(t *testing.T) {
		got := rowToRecord(&dashboardRow{ID: "dash-2"})
		if string(got.Widgets) != "[]" {
			t.Errorf("Widgets = %q; want \"[]\"", got.Widgets)
		}
		if !json.Valid(got.Widgets) {
			t.Errorf("Widgets %q is not valid JSON", got.Widgets)
		}
	})

	t.Run("forward mọi field nguyên văn", func(t *testing.T) {
		desc := "phễu theo stage"
		row := dashboardRow{
			ID: "dash-1", TenantID: "tenant-a", Name: "Tổng quan",
			Description: &desc, Widgets: json.RawMessage(`[{"type":"metric"}]`),
			CreatedAt: fixedNow, UpdatedAt: fixedNow,
		}
		got := rowToRecord(&row)
		if got.ID != "dash-1" || got.TenantID != "tenant-a" || got.Name != "Tổng quan" {
			t.Errorf("identity fields mismatched: %+v", got)
		}
		if got.Description == nil || *got.Description != desc {
			t.Errorf("Description = %v; want %q", got.Description, desc)
		}
		if string(got.Widgets) != `[{"type":"metric"}]` {
			t.Errorf("Widgets = %s; want forwarded verbatim", got.Widgets)
		}
		if got.CreatedBy != nil {
			t.Errorf("CreatedBy = %v; want nil", got.CreatedBy)
		}
	})
}

func TestDashboardRepository_Create(t *testing.T) {
	t.Run("insert tenant-pinned với audit + clock cố định", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		id, err := repo.Create(context.Background(), CreateInput{
			TenantID:    "tenant-a",
			Name:        "Tổng quan bán hàng",
			Description: "phễu theo stage",
			Widgets:     []byte(`[{"type":"metric"}]`),
			CreatedBy:   "user-1",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if len(id) != 36 {
			t.Errorf("id = %q; want UUID (36 chars)", id)
		}
		call := rec.lastExec(t)
		mustContainSQL(t, call.query, "INSERT INTO sys_dashboards")
		if len(call.args) != 8 {
			t.Fatalf("args = %d; want 8 (%v)", len(call.args), call.args)
		}
		if call.args[0] != id {
			t.Errorf("args[0] = %v; want returned id %q", call.args[0], id)
		}
		if call.args[1] != "tenant-a" {
			t.Errorf("args[1] (tenant_id) = %v; want tenant-a", call.args[1])
		}
		if call.args[3] != "phễu theo stage" {
			t.Errorf("args[3] (description) = %v; want set", call.args[3])
		}
		if call.args[4] != `[{"type":"metric"}]` {
			t.Errorf("args[4] (widgets) = %v; want raw JSON string", call.args[4])
		}
		if call.args[5] != "user-1" {
			t.Errorf("args[5] (created_by) = %v; want user-1", call.args[5])
		}
		for _, i := range []int{6, 7} {
			ts, ok := call.args[i].(time.Time)
			if !ok || !ts.Equal(fixedNow) {
				t.Errorf("args[%d] (timestamp) = %v; want %v", i, call.args[i], fixedNow)
			}
		}
	})

	t.Run("description + createdBy trắng → NULL", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if _, err := repo.Create(context.Background(), CreateInput{
			TenantID: "tenant-a", Name: "d", Description: "  ", Widgets: []byte(`[]`), CreatedBy: "",
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		call := rec.lastExec(t)
		if call.args[3] != nil {
			t.Errorf("args[3] (description) = %v; want NULL", call.args[3])
		}
		if call.args[5] != nil {
			t.Errorf("args[5] (created_by) = %v; want NULL", call.args[5])
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, errors.New("duplicate entry"))
		_, err := repo.Create(context.Background(), CreateInput{
			TenantID: "tenant-a", Name: "d", Widgets: []byte(`[]`),
		})
		if err == nil || !strings.Contains(err.Error(), "dashboard.SQLRepository.Create") {
			t.Errorf("err = %v; want wrapped with dashboard.SQLRepository.Create", err)
		}
	})
}

func TestDashboardRepository_Get(t *testing.T) {
	t.Run("id lạ / khác tenant → ErrDashboardNotFound", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		_, err := repo.Get(context.Background(), "tenant-a", "dash-x")
		if !errors.Is(err, ErrDashboardNotFound) {
			t.Fatalf("err = %v; want ErrDashboardNotFound", err)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query, "FROM sys_dashboards", "tenant_id = ?", "deleted_at IS NULL")
		if len(call.args) != 2 || call.args[0] != "tenant-a" || call.args[1] != "dash-x" {
			t.Errorf("args = %v; want [tenant-a dash-x]", call.args)
		}
	})

	t.Run("row sống được chiếu về Record", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueQuery(dashCols(), [][]driver.Value{sampleDashRow()}, nil)
		got, err := repo.Get(context.Background(), "tenant-a", "dash-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ID != "dash-1" || got.TenantID != "tenant-a" || got.Name != "Tổng quan bán hàng" {
			t.Errorf("record mismatched: %+v", got)
		}
		if got.Description == nil || *got.Description != "phễu theo stage" {
			t.Errorf("Description = %v; want set", got.Description)
		}
		if string(got.Widgets) != `[{"type":"metric"}]` {
			t.Errorf("Widgets = %s; want forwarded verbatim", got.Widgets)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueQuery(nil, nil, errors.New("connection refused"))
		_, err := repo.Get(context.Background(), "tenant-a", "dash-1")
		if err == nil || !strings.Contains(err.Error(), "dashboard.SQLRepository.Get") {
			t.Errorf("err = %v; want wrapped with dashboard.SQLRepository.Get", err)
		}
	})
}

func TestDashboardRepository_List(t *testing.T) {
	t.Run("danh sách tenant-scoped, mới nhất trước", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		row2 := sampleDashRow()
		row2[0] = "dash-2"
		rec.queueQuery(dashCols(), [][]driver.Value{sampleDashRow(), row2}, nil)
		got, err := repo.List(context.Background(), "tenant-a")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 || got[0].ID != "dash-1" || got[1].ID != "dash-2" {
			t.Errorf("records = %+v; want dash-1, dash-2 in driver order", got)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query, "tenant_id = ?", "deleted_at IS NULL", "ORDER BY created_at DESC, id DESC")
		if len(call.args) != 1 || call.args[0] != "tenant-a" {
			t.Errorf("args = %v; want [tenant-a]", call.args)
		}
	})

	t.Run("tenant không có dashboard → slice rỗng, không lỗi", func(t *testing.T) {
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
		if err == nil || !strings.Contains(err.Error(), "dashboard.SQLRepository.List") {
			t.Errorf("err = %v; want wrapped with dashboard.SQLRepository.List", err)
		}
	})
}

func TestDashboardRepository_Update(t *testing.T) {
	in := UpdateInput{Name: "Đổi tên", Description: "mô tả mới", Widgets: []byte(`[]`)}

	t.Run("update tenant-pinned với thứ tự args đúng", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if err := repo.Update(context.Background(), "tenant-a", "dash-1", in); err != nil {
			t.Fatalf("Update: %v", err)
		}
		call := rec.lastExec(t)
		mustContainSQL(t, call.query, "UPDATE sys_dashboards", "tenant_id = ?", "deleted_at IS NULL")
		want := []driver.Value{"Đổi tên", "mô tả mới", "[]", "tenant-a", "dash-1"}
		if len(call.args) != len(want) {
			t.Fatalf("args = %v; want %v", call.args, want)
		}
		for i := range want {
			if call.args[i] != want[i] {
				t.Errorf("args[%d] = %v; want %v", i, call.args[i], want[i])
			}
		}
	})

	t.Run("description trắng → NULL", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		blank := in
		blank.Description = "   "
		if err := repo.Update(context.Background(), "tenant-a", "dash-1", blank); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if call := rec.lastExec(t); call.args[1] != nil {
			t.Errorf("args[1] (description) = %v; want NULL", call.args[1])
		}
	})

	t.Run("0 row khớp (khác tenant / đã xoá) → ErrDashboardNotFound", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, nil)
		if err := repo.Update(context.Background(), "tenant-b", "dash-1", in); !errors.Is(err, ErrDashboardNotFound) {
			t.Errorf("err = %v; want ErrDashboardNotFound", err)
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, errors.New("lock wait timeout"))
		err := repo.Update(context.Background(), "tenant-a", "dash-1", in)
		if err == nil || !strings.Contains(err.Error(), "dashboard.SQLRepository.Update") {
			t.Errorf("err = %v; want wrapped with dashboard.SQLRepository.Update", err)
		}
	})
}

func TestDashboardRepository_Delete(t *testing.T) {
	t.Run("soft-delete stamp clock cố định, tenant-pinned + purge nav cùng tx (INV-4)", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if err := repo.Delete(context.Background(), "tenant-a", "dash-1"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		calls := rec.execs()
		if len(calls) != 3 {
			t.Fatalf("execs = %d; want 3 (soft-delete UPDATE + nav-purge DELETE + app home-dashboard clear): %+v", len(calls), calls)
		}
		call := calls[0]
		mustContainSQL(t, call.query, "UPDATE sys_dashboards SET deleted_at = ?", "tenant_id = ?", "deleted_at IS NULL")
		if len(call.args) != 3 {
			t.Fatalf("args = %v; want 3", call.args)
		}
		if ts, ok := call.args[0].(time.Time); !ok || !ts.Equal(fixedNow) {
			t.Errorf("args[0] (deleted_at) = %v; want %v", call.args[0], fixedNow)
		}
		if call.args[1] != "tenant-a" || call.args[2] != "dash-1" {
			t.Errorf("args[1:] = %v; want [tenant-a dash-1]", call.args[1:])
		}
		purge := calls[1]
		mustContainSQL(t, purge.query, "DELETE FROM sys_app_nav_items", "tenant_id = ?", "item_type = ?", "ref_id = ?")
		if len(purge.args) != 3 || purge.args[0] != "tenant-a" || purge.args[1] != "dashboard" || purge.args[2] != "dash-1" {
			t.Errorf("purge args = %v; want [tenant-a dashboard dash-1]", purge.args)
		}
		mustContainSQL(t, calls[2].query, "UPDATE sys_apps", "homeDashboardId", "tenant_id = ?")
	})

	t.Run("double-delete / khác tenant → ErrDashboardNotFound, KHÔNG purge", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, nil)
		if err := repo.Delete(context.Background(), "tenant-a", "dash-1"); !errors.Is(err, ErrDashboardNotFound) {
			t.Errorf("err = %v; want ErrDashboardNotFound", err)
		}
		if calls := rec.execs(); len(calls) != 1 {
			t.Errorf("execs = %d; want 1 (no purge after not-found)", len(calls))
		}
	})

	t.Run("lỗi driver được wrap kèm method", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, errors.New("server gone"))
		err := repo.Delete(context.Background(), "tenant-a", "dash-1")
		if err == nil || !strings.Contains(err.Error(), "dashboard.SQLRepository.Delete") {
			t.Errorf("err = %v; want wrapped with dashboard.SQLRepository.Delete", err)
		}
	})
}
