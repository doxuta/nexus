//go:build integration

package audit_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/ittest"

	_ "github.com/go-sql-driver/mysql"
)

var packageTestDSN = "root:@tcp(127.0.0.1:3306)/nexus_platform?parseTime=true&multiStatements=true"

// TestMain self-provisions ONE migrated throwaway database for the audit integration suite.
func TestMain(m *testing.M) {
	input := os.Getenv("TEST_DATABASE_URL")
	if input == "" {
		input = packageTestDSN
	}
	thr, cleanup, skip, err := ittest.Setup(input, "nexus_audit_it", "../../migrations")
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit integration setup failed: %v\n", err)
		os.Exit(1)
	}
	// Skip (MySQL unreachable at the DSN asked for) must not leave the package default (shared dev
	// nexus_platform on :3306) in place: a wrong TEST_DATABASE_URL would then run and WRITE there.
	packageTestDSN = input
	if !skip {
		packageTestDSN = thr
	}
	code := m.Run()
	if cleanup != nil {
		cleanup()
	}
	os.Exit(code)
}

// specs/048 T119a — the CSV import's history job asks which records already carry a `create` line.
// Pinned to tenant + entity + action; a replayed id list answers exactly the written ones.
func TestAuditRepo_ExistingCreateIDs_TenantScoped(t *testing.T) {
	db, err := sqlx.Connect("mysql", packageTestDSN)
	if err != nil {
		t.Skipf("audit: MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := audit.NewSQLRepository(db)
	ctx := context.Background()
	const ta, tb = "aaaaaaaa-0000-0000-0000-0000000000a1", "bbbbbbbb-0000-0000-0000-0000000000b1"
	ev := func(tenant, entity, rec, action string) {
		t.Helper()
		if err := repo.InsertEvent(ctx, audit.Event{TenantID: tenant, ActorUserID: "u", EntityName: entity, RecordID: rec, Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	ev(ta, "leads", "r1", audit.ActionCreate)
	ev(ta, "leads", "r2", audit.ActionUpdate)    // not a create
	ev(ta, "contacts", "r3", audit.ActionCreate) // other entity
	ev(tb, "leads", "r4", audit.ActionCreate)    // other tenant
	ev(ta, "leads", "r1", audit.ActionCreate)    // duplicate line still answers once

	got, err := repo.ExistingCreateIDs(ctx, ta, "leads", []string{"r1", "r2", "r3", "r4", "r5"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want only r1", got)
	}
	if _, ok := got["r1"]; !ok {
		t.Fatalf("r1 missing: %v", got)
	}
	if empty, err := repo.ExistingCreateIDs(ctx, ta, "leads", nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty ids: %v %v", empty, err)
	}
}
