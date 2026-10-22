package notes

// Held out of the run; regrade.sh adds it to the finished workspace. Listing and counting a record's notes must
// filter on the caller's tenant: one top-level AND term `tenant_id = ?` whose placeholder binds the tenant, and no
// top-level OR (which would let other tenants' rows through). Where the term sits in the clause does not matter.

import (
	"context"
	"regexp"
	"testing"
)

var hiddenTenantTerm = regexp.MustCompile(`(?i)^\(?\s*(?:\w+\.)?` + "`?" + `tenant_id` + "`?" + `\s*=\s*\?\s*\)?$`)

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
	terms, topOr := hiddenConjuncts(c.query)
	if topOr {
		t.Errorf("%s: a top-level OR in the WHERE clause lets other tenants' rows through: %q", name, c.query)
	}
	for _, term := range terms {
		if hiddenTenantTerm.MatchString(term.text) && term.arg >= 0 && term.arg < len(c.args) && c.args[term.arg] == "tenant-a" {
			return
		}
	}
	t.Errorf("%s: no top-level `tenant_id = ?` term bound to the tenant in %q (args %v)", name, c.query, c.args)
}
