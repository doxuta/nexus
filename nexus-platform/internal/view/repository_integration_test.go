//go:build integration

// Integration tests for the sys_views repository. Run with:
//
//	go test -tags=integration -race ./internal/view/
//
// They require a reachable MySQL 8 with migration 000035 applied (sys_views). Set
// TEST_DATABASE_URL to override the DSN; otherwise the local compose DSN is used.
package view

import (
	"context"
	"errors"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
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

func strptr(s string) *string { return &s }

// TestViewRepo_CRUDAndVisibility exercises the full repository surface against real MySQL: the
// personal/shared visibility union in ListForUser, cross-tenant isolation, update, and soft-delete.
func TestViewRepo_CRUDAndVisibility(t *testing.T) {
	db := testDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()

	const tenant = "aaaa1111-0000-0000-0000-00000000v001"
	const otherTenant = "bbbb2222-0000-0000-0000-00000000v002"
	const u1 = "user-0000-0000-0000-0000000000v1"
	const u2 = "user-0000-0000-0000-0000000000v2"

	// Clean slate + cleanup (sys_views is a shared table; scope teardown to the test tenants).
	clean := func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM sys_views WHERE tenant_id IN (?, ?)", tenant, otherTenant)
	}
	clean()
	t.Cleanup(clean)

	cfg := []byte(`{"columns":["name"]}`)

	// u1 personal, shared (nil owner), u2 personal — all on "accounts"; plus u1 personal on "contacts".
	u1AccID, err := repo.Create(ctx, CreateInput{TenantID: tenant, UserID: strptr(u1), EntityName: "accounts", Name: "u1 accounts", Config: cfg, CreatedBy: u1})
	if err != nil {
		t.Fatalf("create u1 accounts: %v", err)
	}
	sharedID, err := repo.Create(ctx, CreateInput{TenantID: tenant, UserID: nil, EntityName: "accounts", Name: "shared accounts", Config: cfg, CreatedBy: u1})
	if err != nil {
		t.Fatalf("create shared accounts: %v", err)
	}
	if _, err := repo.Create(ctx, CreateInput{TenantID: tenant, UserID: strptr(u2), EntityName: "accounts", Name: "u2 accounts", Config: cfg, CreatedBy: u2}); err != nil {
		t.Fatalf("create u2 accounts: %v", err)
	}
	if _, err := repo.Create(ctx, CreateInput{TenantID: tenant, UserID: strptr(u1), EntityName: "contacts", Name: "u1 contacts", Config: cfg, CreatedBy: u1}); err != nil {
		t.Fatalf("create u1 contacts: %v", err)
	}

	// u1 sees ITS OWN personal + the shared view for accounts — NOT u2's personal, NOT contacts.
	rows, err := repo.ListForUser(ctx, tenant, u1, "accounts")
	if err != nil {
		t.Fatalf("ListForUser u1 accounts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("u1 should see exactly its own + shared (2) views, got %d: %+v", len(rows), rows)
	}
	sawShared, sawOwn := false, false
	for _, r := range rows {
		switch r.ID {
		case sharedID:
			sawShared = true
			if !r.Shared || r.UserID != nil {
				t.Fatalf("shared view should have Shared=true, nil UserID; got %+v", r)
			}
		case u1AccID:
			sawOwn = true
			if r.Shared || r.UserID == nil || *r.UserID != u1 {
				t.Fatalf("own view should be personal to u1; got %+v", r)
			}
		default:
			t.Fatalf("unexpected view in u1's list: %+v", r)
		}
	}
	if !sawShared || !sawOwn {
		t.Fatalf("u1 list missing shared(%v) or own(%v)", sawShared, sawOwn)
	}

	// Cross-tenant isolation: the other tenant sees none of these, and Get across tenants is 404.
	if rows, err := repo.ListForUser(ctx, otherTenant, u1, "accounts"); err != nil || len(rows) != 0 {
		t.Fatalf("cross-tenant list must be empty: len=%d err=%v", len(rows), err)
	}
	if _, err := repo.Get(ctx, otherTenant, sharedID); !errors.Is(err, ErrViewNotFound) {
		t.Fatalf("cross-tenant Get must be ErrViewNotFound, got %v", err)
	}

	// Update the personal view's name + config; the change is visible on read-back.
	if err := repo.Update(ctx, tenant, u1AccID, UpdateInput{Name: "renamed", Config: []byte(`{"columns":["name","email"]}`), UpdatedBy: u1}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	rec, err := repo.Get(ctx, tenant, u1AccID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if rec.Name != "renamed" {
		t.Fatalf("Get after update: name=%q; want 'renamed'", rec.Name)
	}

	// Soft-delete removes it from the list and makes Get 404, without touching the shared view.
	if err := repo.Delete(ctx, tenant, u1AccID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, tenant, u1AccID); !errors.Is(err, ErrViewNotFound) {
		t.Fatalf("Get after delete must be ErrViewNotFound, got %v", err)
	}
	rows, err = repo.ListForUser(ctx, tenant, u1, "accounts")
	if err != nil || len(rows) != 1 || rows[0].ID != sharedID {
		t.Fatalf("after delete, u1 accounts list should be just the shared view: %+v (err=%v)", rows, err)
	}

	// Update/Delete of a non-existent id is ErrViewNotFound.
	if err := repo.Update(ctx, tenant, "no-such-id", UpdateInput{Name: "x", Config: cfg}); !errors.Is(err, ErrViewNotFound) {
		t.Fatalf("Update unknown id: want ErrViewNotFound, got %v", err)
	}
	if err := repo.Delete(ctx, tenant, "no-such-id"); !errors.Is(err, ErrViewNotFound) {
		t.Fatalf("Delete unknown id: want ErrViewNotFound, got %v", err)
	}
}
