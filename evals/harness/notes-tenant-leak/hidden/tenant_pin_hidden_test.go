package notes

// Held out of the run (evals/harness/notes-tenant-leak/hidden); regrade.py adds it to the finished workspace.
// Listing and counting a record's notes must filter on the caller's tenant and bind it.

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
)

func TestHidden_ListAndCountFilterTheTenant(t *testing.T) {
	db, rec := newFakeDB(t)
	repo := NewRepository(db)
	if _, err := repo.ListByRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1", 20, 0); err != nil {
		t.Fatalf("ListByRecord: %v", err)
	}
	filtersTenant(t, "ListByRecord", rec.lastQuery(t))
	rec.queueQuery([]string{"count"}, [][]driver.Value{{int64(1)}}, nil)
	if _, err := repo.CountByRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1"); err != nil {
		t.Fatalf("CountByRecord: %v", err)
	}
	filtersTenant(t, "CountByRecord", rec.lastQuery(t))
}

func filtersTenant(t *testing.T, name string, call loggedCall) {
	t.Helper()
	if !regexp.MustCompile(`tenant_id\s*=\s*\?`).MatchString(call.query) {
		t.Errorf("%s: no tenant_id filter in %q", name, call.query)
	}
	for _, a := range call.args {
		if a == "tenant-a" {
			return
		}
	}
	t.Errorf("%s: args %v do not bind the tenant", name, call.args)
}
