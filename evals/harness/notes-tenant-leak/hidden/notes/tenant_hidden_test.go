package notes

// Held out of the run; regrade.sh adds it to the finished workspace. Listing and counting a record's notes must
// open their WHERE clause with the caller's tenant, ANDed (an OR leaks), and bind it first.

import (
	"context"
	"regexp"
	"testing"
)

var (
	hiddenTenantFirst = regexp.MustCompile(`(?i)\bWHERE\s+tenant_id\s*=\s*\?\s+AND\s+entity_name\s*=\s*\?`)
	hiddenWhere       = regexp.MustCompile(`(?is)\bWHERE\b(.*?)(\bORDER\s+BY\b|\bLIMIT\b|$)`)
	hiddenOr          = regexp.MustCompile(`(?i)\bOR\b`)
)

func TestHidden_ListAndCountFilterTheTenant(t *testing.T) {
	db, s := newHiddenDB(t)
	repo := NewRepository(db)
	if _, err := repo.ListByRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1", 20, 0); err != nil {
		t.Fatalf("ListByRecord: %v", err)
	}
	hiddenFiltersTenant(t, "ListByRecord", s.lastCall(t))
	if _, err := repo.CountByRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1"); err != nil {
		t.Fatalf("CountByRecord: %v", err)
	}
	hiddenFiltersTenant(t, "CountByRecord", s.lastCall(t))
}

func hiddenFiltersTenant(t *testing.T, name string, c hiddenCall) {
	t.Helper()
	if !hiddenTenantFirst.MatchString(c.query) {
		t.Errorf("%s: want `WHERE tenant_id = ? AND entity_name = ?`, got %q", name, c.query)
	}
	if w := hiddenWhere.FindStringSubmatch(c.query); w != nil && hiddenOr.MatchString(w[1]) {
		t.Errorf("%s: an OR in the WHERE clause lets other tenants' rows through: %q", name, c.query)
	}
	if len(c.args) == 0 || c.args[0] != "tenant-a" {
		t.Errorf("%s: the tenant must be the first bound value, got %v", name, c.args)
	}
}
