//go:build integration

// Integration tests for the notification repository. Run with:
//
//	TEST_DATABASE_URL='root:@tcp(127.0.0.1:3306)/nexus_platform?parseTime=true&multiStatements=true' \
//	  go test -tags=integration ./internal/notification/ -race
//
// Requires a running MySQL 8 with the nexus_platform schema migrated (migrations 000001–000026).
package notification

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	_ "github.com/go-sql-driver/mysql"
)

// testDB connects to the throwaway migrated database provisioned by TestMain (see
// main_test.go). Before #84 this read TEST_DATABASE_URL directly, which broke against a db-less
// DSN ("No database selected"); TestMain now creates+migrates a throwaway DB and points testDSN
// at it.
func testDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	return db
}

// seedNotification inserts a minimal sys_notifications row and returns its id.
// tenant_id + user_id ownership is baked in; caller controls is_read and type.
func seedNotification(t *testing.T, db *sqlx.DB, tenantID, userID, notifType string, isRead bool) string {
	t.Helper()
	id := uuid.NewString()
	var readAt interface{}
	if isRead {
		readAt = time.Now().UTC()
	}
	_, err := db.Exec(
		`INSERT INTO sys_notifications (id, tenant_id, user_id, type, title, message, is_read, read_at, created_at)
		 VALUES (?, ?, ?, ?, 'test title', 'test message', ?, ?, NOW())`,
		id, tenantID, userID, notifType, isRead, readAt,
	)
	if err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM sys_notifications WHERE id = ?", id)
	})
	return id
}

func TestNotificationRepo_ListAndCount_Integration(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()

	tenant := "notif-integ-tenant-" + uuid.NewString()[:8]
	user := "notif-integ-user-" + uuid.NewString()[:8]

	// Seed 3 notifications: 2 unread, 1 read.
	id1 := seedNotification(t, db, tenant, user, "workflow", false)
	_ = seedNotification(t, db, tenant, user, "bpm", false)
	_ = seedNotification(t, db, tenant, user, "system", true)

	t.Run("list all returns 3", func(t *testing.T) {
		rows, total, err := repo.List(ctx, tenant, user, false, 1, 20)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 3 {
			t.Errorf("total = %d, want 3", total)
		}
		if len(rows) != 3 {
			t.Errorf("len(rows) = %d, want 3", len(rows))
		}
	})

	t.Run("list unread-only returns 2", func(t *testing.T) {
		rows, total, err := repo.List(ctx, tenant, user, true, 1, 20)
		if err != nil {
			t.Fatalf("List unread: %v", err)
		}
		if total != 2 {
			t.Errorf("total = %d, want 2", total)
		}
		if len(rows) != 2 {
			t.Errorf("len(rows) = %d, want 2", len(rows))
		}
	})

	t.Run("count unread = 2", func(t *testing.T) {
		n, err := repo.CountUnread(ctx, tenant, user)
		if err != nil {
			t.Fatalf("CountUnread: %v", err)
		}
		if n != 2 {
			t.Errorf("unread count = %d, want 2", n)
		}
	})

	t.Run("tenant isolation — different tenant sees 0", func(t *testing.T) {
		n, err := repo.CountUnread(ctx, "other-tenant", user)
		if err != nil {
			t.Fatalf("CountUnread other-tenant: %v", err)
		}
		if n != 0 {
			t.Errorf("other-tenant unread count = %d, want 0", n)
		}
	})

	t.Run("user isolation — different user in same tenant sees 0", func(t *testing.T) {
		n, err := repo.CountUnread(ctx, tenant, "other-user")
		if err != nil {
			t.Fatalf("CountUnread other-user: %v", err)
		}
		if n != 0 {
			t.Errorf("other-user unread count = %d, want 0", n)
		}
	})

	t.Run("ExistsForUser — owner sees true", func(t *testing.T) {
		ok, err := repo.ExistsForUser(ctx, tenant, user, id1)
		if err != nil {
			t.Fatalf("ExistsForUser: %v", err)
		}
		if !ok {
			t.Error("expected true for own notification, got false")
		}
	})

	t.Run("ExistsForUser — wrong user sees false", func(t *testing.T) {
		ok, err := repo.ExistsForUser(ctx, tenant, "wrong-user", id1)
		if err != nil {
			t.Fatalf("ExistsForUser wrong-user: %v", err)
		}
		if ok {
			t.Error("expected false for notification belonging to another user, got true")
		}
	})

	t.Run("MarkRead — idempotent, decrements unread count", func(t *testing.T) {
		if err := repo.MarkRead(ctx, tenant, user, id1); err != nil {
			t.Fatalf("MarkRead: %v", err)
		}
		// Idempotent second call.
		if err := repo.MarkRead(ctx, tenant, user, id1); err != nil {
			t.Fatalf("MarkRead idempotent: %v", err)
		}
		n, err := repo.CountUnread(ctx, tenant, user)
		if err != nil {
			t.Fatalf("CountUnread after MarkRead: %v", err)
		}
		if n != 1 {
			t.Errorf("unread after MarkRead = %d, want 1", n)
		}
	})

	t.Run("MarkAllRead — marks remaining unread", func(t *testing.T) {
		// Re-count before.
		before, _ := repo.CountUnread(ctx, tenant, user)
		updated, err := repo.MarkAllRead(ctx, tenant, user)
		if err != nil {
			t.Fatalf("MarkAllRead: %v", err)
		}
		if updated != before {
			t.Errorf("MarkAllRead updated %d rows, want %d", updated, before)
		}
		after, _ := repo.CountUnread(ctx, tenant, user)
		if after != 0 {
			t.Errorf("unread after MarkAllRead = %d, want 0", after)
		}
	})
}
