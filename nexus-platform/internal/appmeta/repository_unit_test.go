package appmeta

// Default-build unit tests for the app SQLRepository over the fake driver (fakedb_test.go):
// tenant_id pin of every statement, the transactional default-clear (≤1 default/tenant), the
// replace-all nav sequence with slice-index sort_order, the soft-delete + nav-purge cascade,
// and the duplicate-slug / not-found error mapping. Real-MySQL round-trips (UNIQUE index,
// 2-tenant isolation, entity-delete cascade) live in repository_integration_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
)

var fixedNow = time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)

func newTestRepo(t *testing.T) (*SQLRepository, *fakeDB) {
	t.Helper()
	db, rec := newFakeDB(t)
	repo := NewSQLRepository(db)
	repo.now = func() time.Time { return fixedNow }
	return repo, rec
}

func TestAppRepository_Create(t *testing.T) {
	t.Run("insert tenant-pinned, KHÔNG clear default khi isDefault=false", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		id, err := repo.Create(context.Background(), CreateInput{
			TenantID: "tenant-a", Name: "sales", Label: "Bán hàng", CreatedBy: "user-1",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if len(id) != 36 {
			t.Errorf("id = %q; want UUID (36 chars)", id)
		}
		calls := rec.execs()
		if len(calls) != 1 {
			t.Fatalf("execs = %d; want 1 (INSERT only): %+v", len(calls), calls)
		}
		mustContainSQL(t, calls[0].query, "INSERT INTO sys_apps")
		if calls[0].args[1] != "tenant-a" || calls[0].args[2] != "sales" {
			t.Errorf("args[1:3] = %v; want [tenant-a sales]", calls[0].args[1:3])
		}
	})

	t.Run("isDefault=true → clear default cũ TRƯỚC insert, cùng tx", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if _, err := repo.Create(context.Background(), CreateInput{
			TenantID: "tenant-a", Name: "sales", Label: "Bán hàng", IsDefault: true,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		calls := rec.execs()
		if len(calls) != 2 {
			t.Fatalf("execs = %d; want 2 (clear UPDATE + INSERT): %+v", len(calls), calls)
		}
		mustContainSQL(t, calls[0].query, "UPDATE sys_apps SET is_default = 0", "tenant_id = ?", "deleted_at IS NULL")
		if calls[0].args[0] != "tenant-a" {
			t.Errorf("clear args[0] = %v; want tenant-a", calls[0].args[0])
		}
		mustContainSQL(t, calls[1].query, "INSERT INTO sys_apps")
	})

	t.Run("MySQL 1062 trên slug → ErrAppDuplicateName", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, &gomysql.MySQLError{Number: 1062, Message: "Duplicate entry 'tenant-a-sales' for key 'sys_apps.uq_sys_apps_tenant_name'"})
		_, err := repo.Create(context.Background(), CreateInput{TenantID: "tenant-a", Name: "sales", Label: "x"})
		if !errors.Is(err, ErrAppDuplicateName) {
			t.Errorf("err = %v; want ErrAppDuplicateName", err)
		}
	})

	t.Run("lỗi driver khác được wrap kèm method", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, errors.New("server gone"))
		_, err := repo.Create(context.Background(), CreateInput{TenantID: "tenant-a", Name: "sales", Label: "x"})
		if err == nil || !strings.Contains(err.Error(), "appmeta.SQLRepository.Create") {
			t.Errorf("err = %v; want wrapped with appmeta.SQLRepository.Create", err)
		}
	})
}

func TestAppRepository_GetList(t *testing.T) {
	t.Run("GetByID id lạ / khác tenant → ErrAppNotFound, query tenant-pinned", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		_, err := repo.GetByID(context.Background(), "tenant-a", "app-x")
		if !errors.Is(err, ErrAppNotFound) {
			t.Fatalf("err = %v; want ErrAppNotFound", err)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query, "FROM sys_apps", "a.tenant_id = ?", "a.deleted_at IS NULL")
	})

	t.Run("GetByName slug lạ → ErrAppNotFound", func(t *testing.T) {
		repo, _ := newTestRepo(t)
		if _, err := repo.GetByName(context.Background(), "tenant-a", "ghost"); !errors.Is(err, ErrAppNotFound) {
			t.Fatalf("err = %v; want ErrAppNotFound", err)
		}
	})

	t.Run("List tenant-pinned, thứ tự App Switcher", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if _, err := repo.List(context.Background(), "tenant-a"); err != nil {
			t.Fatalf("List: %v", err)
		}
		call := rec.lastQuery(t)
		mustContainSQL(t, call.query, "a.tenant_id = ?", "a.deleted_at IS NULL", "ORDER BY a.sort_order ASC")
		if len(call.args) != 1 || call.args[0] != "tenant-a" {
			t.Errorf("args = %v; want [tenant-a]", call.args)
		}
	})
}

func TestAppRepository_Update(t *testing.T) {
	in := UpdateInput{Label: "Đổi tên", IsDefault: true, SortOrder: 3, UpdatedBy: "user-2"}

	t.Run("isDefault=true → clear default app KHÁC (except id) rồi update", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if err := repo.Update(context.Background(), "tenant-a", "app-1", in); err != nil {
			t.Fatalf("Update: %v", err)
		}
		calls := rec.execs()
		if len(calls) != 2 {
			t.Fatalf("execs = %d; want 2 (clear + UPDATE): %+v", len(calls), calls)
		}
		mustContainSQL(t, calls[0].query, "UPDATE sys_apps SET is_default = 0", "id <> ?")
		if calls[0].args[0] != "tenant-a" || calls[0].args[1] != "app-1" {
			t.Errorf("clear args = %v; want [tenant-a app-1]", calls[0].args)
		}
		mustContainSQL(t, calls[1].query, "UPDATE sys_apps", "tenant_id = ?", "deleted_at IS NULL")
	})

	t.Run("0 row khớp → ErrAppNotFound", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(1, nil) // clear-default succeeds
		rec.queueExec(0, nil) // update matches nothing
		if err := repo.Update(context.Background(), "tenant-b", "app-1", in); !errors.Is(err, ErrAppNotFound) {
			t.Errorf("err = %v; want ErrAppNotFound", err)
		}
	})
}

func TestAppRepository_SoftDelete(t *testing.T) {
	t.Run("tombstone-rename + stamp + purge nav cùng tx", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if err := repo.SoftDelete(context.Background(), "tenant-a", "app-1"); err != nil {
			t.Fatalf("SoftDelete: %v", err)
		}
		calls := rec.execs()
		if len(calls) != 2 {
			t.Fatalf("execs = %d; want 2 (UPDATE + nav DELETE): %+v", len(calls), calls)
		}
		mustContainSQL(t, calls[0].query,
			"UPDATE sys_apps", "deleted_at = ?", "__deleted_", "tenant_id = ?", "deleted_at IS NULL")
		if ts, ok := calls[0].args[0].(time.Time); !ok || !ts.Equal(fixedNow) {
			t.Errorf("args[0] (deleted_at) = %v; want %v", calls[0].args[0], fixedNow)
		}
		mustContainSQL(t, calls[1].query, "DELETE FROM sys_app_nav_items", "tenant_id = ?", "app_id = ?")
		if calls[1].args[0] != "tenant-a" || calls[1].args[1] != "app-1" {
			t.Errorf("purge args = %v; want [tenant-a app-1]", calls[1].args)
		}
	})

	t.Run("double-delete / khác tenant → ErrAppNotFound, KHÔNG purge", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		rec.queueExec(0, nil)
		if err := repo.SoftDelete(context.Background(), "tenant-a", "app-1"); !errors.Is(err, ErrAppNotFound) {
			t.Errorf("err = %v; want ErrAppNotFound", err)
		}
		if calls := rec.execs(); len(calls) != 1 {
			t.Errorf("execs = %d; want 1 (no purge after not-found)", len(calls))
		}
	})
}

func TestAppRepository_ReplaceNav(t *testing.T) {
	t.Run("delete-all rồi insert theo thứ tự slice, sort_order = index", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		err := repo.ReplaceNav(context.Background(), "tenant-a", "app-1", []NavItemInput{
			{ItemType: ItemTypeEntity, RefID: "ent-1"},
			{ItemType: ItemTypeDashboard, RefID: "dash-1", Label: "KPI"},
		})
		if err != nil {
			t.Fatalf("ReplaceNav: %v", err)
		}
		calls := rec.execs()
		if len(calls) != 3 {
			t.Fatalf("execs = %d; want 3 (DELETE + 2 INSERT): %+v", len(calls), calls)
		}
		mustContainSQL(t, calls[0].query, "DELETE FROM sys_app_nav_items", "tenant_id = ?", "app_id = ?")
		for i, want := range []struct {
			itemType, refID string
			label           any
			sort            int64
		}{
			{ItemTypeEntity, "ent-1", nil, 0},
			{ItemTypeDashboard, "dash-1", "KPI", 1},
		} {
			call := calls[i+1]
			mustContainSQL(t, call.query, "INSERT INTO sys_app_nav_items")
			// args: id, tenant_id, app_id, item_type, ref_id, label, sort_order, created_at
			if call.args[1] != "tenant-a" || call.args[2] != "app-1" {
				t.Errorf("insert %d tenant/app args = %v", i, call.args[1:3])
			}
			if call.args[3] != want.itemType || call.args[4] != want.refID {
				t.Errorf("insert %d = %v/%v; want %s/%s", i, call.args[3], call.args[4], want.itemType, want.refID)
			}
			if call.args[5] != want.label {
				t.Errorf("insert %d label = %v; want %v", i, call.args[5], want.label)
			}
			if call.args[6] != want.sort {
				t.Errorf("insert %d sort_order = %v; want %d", i, call.args[6], want.sort)
			}
		}
	})

	t.Run("mảng rỗng = clear nav (chỉ DELETE)", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if err := repo.ReplaceNav(context.Background(), "tenant-a", "app-1", nil); err != nil {
			t.Fatalf("ReplaceNav: %v", err)
		}
		if calls := rec.execs(); len(calls) != 1 {
			t.Errorf("execs = %d; want 1 (DELETE only)", len(calls))
		}
	})
}

func TestPurgeNavForRef(t *testing.T) {
	t.Run("statement tenant-pinned với đủ 3 arg", func(t *testing.T) {
		db, rec := newFakeDB(t)
		if err := PurgeNavForRef(context.Background(), db, "tenant-a", ItemTypeEntity, "ent-9"); err != nil {
			t.Fatalf("PurgeNavForRef: %v", err)
		}
		calls := rec.execs()
		if len(calls) != 1 {
			t.Fatalf("execs = %d; want 1", len(calls))
		}
		mustContainSQL(t, calls[0].query, "DELETE FROM sys_app_nav_items", "tenant_id = ?", "item_type = ?", "ref_id = ?")
		if calls[0].args[0] != "tenant-a" || calls[0].args[1] != "entity" || calls[0].args[2] != "ent-9" {
			t.Errorf("args = %v; want [tenant-a entity ent-9]", calls[0].args)
		}
	})

	t.Run("lỗi driver được wrap", func(t *testing.T) {
		db, rec := newFakeDB(t)
		rec.queueExec(0, errors.New("gone"))
		err := PurgeNavForRef(context.Background(), db, "tenant-a", ItemTypeEntity, "ent-9")
		if err == nil || !strings.Contains(err.Error(), "appmeta.PurgeNavForRef") {
			t.Errorf("err = %v; want wrapped with appmeta.PurgeNavForRef", err)
		}
	})
}

func TestAppRepository_UpdateHomeDashboard(t *testing.T) {
	t.Run("HomeDashboardID != nil → thêm một UPDATE options tham số hoá, tenant-pinned", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		id := "dash-7"
		if err := repo.Update(context.Background(), "tenant-a", "app-1", UpdateInput{Label: "X", HomeDashboardID: &id}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		calls := rec.execs()
		if len(calls) != 2 {
			t.Fatalf("execs = %d; want 2 (UPDATE + options)", len(calls))
		}
		mustContainSQL(t, calls[1].query, "UPDATE sys_apps SET options", "homeDashboardId", "tenant_id = ?")
		if strings.Contains(calls[1].query, "dash-7") {
			t.Errorf("giá trị bị nối vào SQL: %s", calls[1].query)
		}
	})
	t.Run("HomeDashboardID == nil → không đụng options", func(t *testing.T) {
		repo, rec := newTestRepo(t)
		if err := repo.Update(context.Background(), "tenant-a", "app-1", UpdateInput{Label: "X"}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if n := len(rec.execs()); n != 1 {
			t.Fatalf("execs = %d; want 1", n)
		}
	})
}

func TestPurgeNavForRef_ClearsHomeDashboard(t *testing.T) {
	db, rec := newFakeDB(t)
	if err := PurgeNavForRef(context.Background(), db, "tenant-a", ItemTypeDashboard, "dash-9"); err != nil {
		t.Fatalf("PurgeNavForRef: %v", err)
	}
	calls := rec.execs()
	if len(calls) != 2 {
		t.Fatalf("execs = %d; want 2 (nav purge + gỡ homeDashboardId)", len(calls))
	}
	mustContainSQL(t, calls[1].query, "UPDATE sys_apps", "homeDashboardId", "tenant_id = ?")
}
