//go:build integration

package auth

// branding_exec_integration_test.go — specs/063-draft-publish T064: SetBrandingExec (what publish runs
// on its transaction) writes the same column the repository writes, only for an ACTIVE tenant of the
// given id.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func brandingConn(t *testing.T) (*sqlx.DB, *UserRepository) {
	t.Helper()
	sdb, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	gdb, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sdb.DB}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm: %v", err)
	}
	return sdb, NewUserRepository(gdb)
}

func seedBrandingTenant(t *testing.T, db *sqlx.DB, status string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO tenants (id, slug, name, status) VALUES (?, ?, 'T', ?)`, id, "b-"+id[:8], status); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func readBranding(t *testing.T, db *sqlx.DB, id string) string {
	t.Helper()
	var s *string
	if err := db.Get(&s, `SELECT CAST(branding AS CHAR) FROM tenants WHERE id = ?`, id); err != nil {
		t.Fatalf("read branding: %v", err)
	}
	if s == nil {
		return "<NULL>"
	}
	return *s
}

func TestSetBrandingExec_WritesWhatTheRepositoryWrites(t *testing.T) {
	sdb, repo := brandingConn(t)
	ctx := context.Background()
	viaRepo, viaTx := seedBrandingTenant(t, sdb, "active"), seedBrandingTenant(t, sdb, "active")
	doc := json.RawMessage(`{"appName":"Phòng khám A","accentColor":"#112233"}`)

	if err := repo.UpdateTenantBranding(ctx, viaRepo, doc); err != nil {
		t.Fatalf("repo: %v", err)
	}
	tx, err := sdb.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := SetBrandingExec(ctx, tx, viaTx, doc)
	if err != nil || n != 1 {
		t.Fatalf("SetBrandingExec: n=%d err=%v", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if a, b := readBranding(t, sdb, viaRepo), readBranding(t, sdb, viaTx); a != b {
		t.Errorf("repository wrote %s, SetBrandingExec wrote %s", a, b)
	}
	// empty document ⇒ SQL NULL (back to default branding), both paths
	if _, err := SetBrandingExec(ctx, sdb, viaTx, nil); err != nil {
		t.Fatal(err)
	}
	if got := readBranding(t, sdb, viaTx); got != "<NULL>" {
		t.Errorf("empty document must store NULL, got %s", got)
	}
}

func TestSetBrandingExec_OnlyTheGivenActiveTenant(t *testing.T) {
	sdb, _ := brandingConn(t)
	ctx := context.Background()
	mine, other, suspended := seedBrandingTenant(t, sdb, "active"), seedBrandingTenant(t, sdb, "active"), seedBrandingTenant(t, sdb, "suspended")
	if n, err := SetBrandingExec(ctx, sdb, mine, json.RawMessage(`{"appName":"X"}`)); err != nil || n != 1 {
		t.Fatalf("mine: n=%d err=%v", n, err)
	}
	if got := readBranding(t, sdb, other); got != "<NULL>" {
		t.Errorf("another tenant's branding changed: %s", got)
	}
	if n, err := SetBrandingExec(ctx, sdb, suspended, json.RawMessage(`{"appName":"X"}`)); err != nil || n != 0 {
		t.Errorf("suspended tenant: n=%d err=%v, want 0 rows", n, err)
	}
}
