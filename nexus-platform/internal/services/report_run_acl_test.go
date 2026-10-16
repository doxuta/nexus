package services

// REPORT-RUN-ACL unit tests. The report RUN / CSV-export path is mounted OUTSIDE the /:entity
// RequireACL middleware (a report's entity is stored on the record, not the URL), so RecordService
// resolves the caller's "view" ACL itself via AuthorizeView. These tests prove that resolution in
// isolation (the four branches) and prove RunReport/ExportReportCSV enforce it BEFORE any data query
// (deny short-circuits at AuthorizeView; the store panics if the query is ever reached). The
// happy-path ROW-SCOPING (owner sees only theirs, not 0, not all) is proven end-to-end against a
// real DB in handlers/report_run_acl_integration_test.go — a unit test cannot observe SQL row-scope.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/report"
)

func TestRecordService_AuthorizeView(t *testing.T) {
	ctx := context.Background()
	// A non-admin scope that arrived fail-closed (AccessNone) from a no-ACL route — exactly the
	// scopeOf output the report handler hands in. AuthorizeView must OVERWRITE this with the grant.
	base := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "none"}

	t.Run("deny → FORBIDDEN, no scope leaked", func(t *testing.T) {
		s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"tickets": false}}, nil)
		got, err := s.AuthorizeView(ctx, "t1", base, "tickets")
		if codeOf(err) != "FORBIDDEN" {
			t.Fatalf("code = %q, want FORBIDDEN (err=%v)", codeOf(err), err)
		}
		if got != (RecordScope{}) {
			t.Fatalf("denied scope = %+v, want zero value", got)
		}
	})

	t.Run("allow → resolved level on a copy, identity VERBATIM", func(t *testing.T) {
		s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"tickets": true}, level: map[string]string{"tickets": "owner"}}, nil)
		got, err := s.AuthorizeView(ctx, "t1", base, "tickets")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.AccessLevel != "owner" {
			t.Fatalf("AccessLevel = %q, want owner (resolved from the grant, NOT the fail-closed none)", got.AccessLevel)
		}
		if got.UserID != "u1" || got.RoleID != "role1" || got.IsAdmin {
			t.Fatalf("identity fields mutated: %+v (only the level may change)", got)
		}
	})

	t.Run("admin → scope unchanged, authz NOT consulted", func(t *testing.T) {
		// authz would DENY tickets, but an admin must bypass and keep its scope verbatim (IsAdmin
		// short-circuits every downstream check to AccessAll).
		s := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"tickets": false}}, nil)
		admin := RecordScope{UserID: "root", RoleID: "r", IsAdmin: true, AccessLevel: "all"}
		got, err := s.AuthorizeView(ctx, "t1", admin, "tickets")
		if err != nil {
			t.Fatalf("admin err: %v", err)
		}
		if got != admin {
			t.Fatalf("admin scope = %+v, want unchanged %+v", got, admin)
		}
	})

	t.Run("nil authz → passthrough (unwired build / unit test)", func(t *testing.T) {
		s := buildRJService(&rjStore{}, nil, nil) // no WithACLAuthorizer
		got, err := s.AuthorizeView(ctx, "t1", base, "tickets")
		if err != nil {
			t.Fatalf("nil-authz err: %v", err)
		}
		if got != base {
			t.Fatalf("nil-authz scope = %+v, want passthrough %+v", got, base)
		}
	})
}

// fakeReportRepo returns a single canned report for Get; run/export only load-and-decode the report
// (loadRunnable), so the other methods are never reached in these tests.
type fakeReportRepo struct{ rec report.Record }

func (f *fakeReportRepo) Create(context.Context, report.CreateInput) (string, error) {
	panic("unexpected reportRepo.Create")
}
func (f *fakeReportRepo) Get(_ context.Context, _, _ string) (*report.Record, error) {
	r := f.rec // copy so a caller can't mutate the canned row
	return &r, nil
}
func (f *fakeReportRepo) List(context.Context, string) ([]report.Record, error) {
	panic("unexpected reportRepo.List")
}
func (f *fakeReportRepo) Update(context.Context, string, string, report.UpdateInput) error {
	panic("unexpected reportRepo.Update")
}
func (f *fakeReportRepo) Delete(context.Context, string, string) error {
	panic("unexpected reportRepo.Delete")
}

// TestReportRun_DeniesBeforeQuery proves the deny-before-query ordering for BOTH the run and the
// export path: a caller lacking "view" on the report entity is refused FORBIDDEN, and the record
// store is NEVER reached (rjStore's List/Aggregate methods panic if called). For export it also
// proves not one byte is written before the deny.
func TestReportRun_DeniesBeforeQuery(t *testing.T) {
	ctx := context.Background()
	scope := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "none"}
	rep := report.Record{
		ID: "rep1", TenantID: "t1", Name: "R", EntityName: "tickets", Type: "summary",
		Config: json.RawMessage(`{"groupBy":"stage","metric":"count"}`), IsActive: true,
	}
	// authz DENIES "tickets"; rjStore panics if the aggregate/list query is ever reached — so a clean
	// FORBIDDEN with no panic proves the ACL gate ran BEFORE any data query.
	records := buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{"tickets": false}}, nil)
	svc := NewReportService(&fakeReportRepo{rec: rep}, records)

	t.Run("RunReport", func(t *testing.T) {
		if _, err := svc.RunReport(ctx, scope, "t1", "rep1", nil); codeOf(err) != "FORBIDDEN" {
			t.Fatalf("RunReport code = %q, want FORBIDDEN (err=%v)", codeOf(err), err)
		}
	})

	t.Run("ExportReportCSV — denies before the first byte", func(t *testing.T) {
		var buf bytes.Buffer
		err := svc.ExportReportCSV(ctx, scope, "t1", "rep1", nil, &buf)
		if codeOf(err) != "FORBIDDEN" {
			t.Fatalf("ExportReportCSV code = %q, want FORBIDDEN (err=%v)", codeOf(err), err)
		}
		if buf.Len() != 0 {
			t.Fatalf("export wrote %d bytes before deny; want 0 (deny before write)", buf.Len())
		}
	})
}
