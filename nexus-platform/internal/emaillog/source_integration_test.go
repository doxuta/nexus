//go:build integration

package emaillog

import (
	"context"
	"strings"
	"testing"
	"time"
)

// specs/066 L3 (T073): the source filter, the issuer's name and the shape of the auth rows.

func seedSources(t *testing.T) (*Repository, func()) {
	t.Helper()
	db, r := elogDB(t)
	ctx := context.Background()
	clean := func() {
		_, _ = db.Exec("DELETE FROM sys_email_log WHERE tenant_id IN (?, ?)", tenA, tenB)
		_, _ = db.Exec("DELETE FROM users WHERE id IN ('src-u-in', 'src-u-out')")
		_, _ = db.Exec("DELETE FROM tenants WHERE id IN (?, ?)", tenA, tenB)
	}
	clean()
	t.Cleanup(clean)
	for _, row := range [][]string{{tenA, "src-a"}, {tenB, "src-b"}} {
		if _, err := db.Exec("INSERT INTO tenants (id, slug, name) VALUES (?, ?, ?)", row[0], row[1], row[1]); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	for _, u := range [][]string{{"src-u-in", tenA, "in@a.test", "Nguyễn Văn An"}, {"src-u-out", tenB, "out@b.test", "Người Tenant Khác"}} {
		if _, err := db.Exec("INSERT INTO users (id, tenant_id, email, password_hash, full_name) VALUES (?, ?, ?, 'x', ?)", u[0], u[1], u[2], u[3]); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	idx := 0
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	rows := []Entry{
		{ID: "s-wf", Source: SourceWorkflow, Status: StatusFailed, RecipientMasked: "a***@x.vn", WorkflowID: "wf-1", ActionIndex: &idx},
		{ID: "s-reset", Source: SourcePasswordReset, Status: StatusSent, RecipientMasked: "t***@example.com", SubjectTemplate: "Đặt lại mật khẩu", UserID: "src-u-in"},
		{ID: "s-link-in", Source: SourceResetLink, Status: StatusSent, RecipientMasked: "t***@example.com", UserID: "src-u-in", CreatedBy: "src-u-in"},
		{ID: "s-link-out", Source: SourceResetLink, Status: StatusFailed, RecipientMasked: "t***@example.com", UserID: "src-u-in", CreatedBy: "src-u-out"},
		{ID: "s-link-none", Source: SourceResetLink, Status: StatusSent, RecipientMasked: "t***@example.com", UserID: "src-u-in"},
	}
	for i, e := range rows {
		e.TenantID = tenA
		if err := r.Insert(ctx, e); err != nil {
			t.Fatalf("insert %s: %v", e.ID, err)
		}
		if _, err := db.Exec("UPDATE sys_email_log SET created_at = ? WHERE id = ?", base.Add(time.Duration(i)*time.Minute), e.ID); err != nil {
			t.Fatal(err)
		}
	}
	return r, clean
}

func TestEmailLogSourceFilter_Integration(t *testing.T) {
	r, _ := seedSources(t)
	ctx := context.Background()
	cases := []struct {
		name string
		f    Filter
		want []string
	}{
		{"reset_link only, newest first", Filter{Source: SourceResetLink, Limit: 50}, []string{"s-link-none", "s-link-out", "s-link-in"}},
		{"password_reset only", Filter{Source: SourcePasswordReset, Limit: 50}, []string{"s-reset"}},
		{"workflow only", Filter{Source: SourceWorkflow, Limit: 50}, []string{"s-wf"}},
		{"source and status together", Filter{Source: SourceResetLink, Status: StatusFailed, Limit: 50}, []string{"s-link-out"}},
		{"no source = every source", Filter{Limit: 50}, []string{"s-link-none", "s-link-out", "s-link-in", "s-reset", "s-wf"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, _, err := r.ListForTenant(ctx, tenA, c.f)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, row := range rows {
				got = append(got, row.ID)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("ids = %v, want %v", got, c.want)
			}
		})
	}
	// The cursor walk must hold with the filter on: 3 reset_link rows, pages of 2.
	f := Filter{Source: SourceResetLink, Limit: 2}
	var walked []string
	for page := 0; page < 5; page++ {
		rows, next, err := r.ListForTenant(ctx, tenA, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			walked = append(walked, row.ID)
		}
		if next == "" {
			break
		}
		f.After = next
	}
	if strings.Join(walked, ",") != "s-link-none,s-link-out,s-link-in" {
		t.Errorf("filtered keyset walk = %v", walked)
	}
	// Another tenant sees none of tenant A's rows whatever it asks for.
	other, _, err := r.ListForTenant(ctx, tenB, Filter{Source: SourceResetLink, Limit: 50})
	if err != nil || len(other) != 0 {
		t.Errorf("tenant B sees %d reset_link rows err=%v, want 0", len(other), err)
	}
}

func TestEmailLogCreatedByName_Integration(t *testing.T) {
	r, _ := seedSources(t)
	ctx := context.Background()
	rows, _, err := r.ListForTenant(ctx, tenA, Filter{Source: SourceResetLink, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Row{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if n := byID["s-link-in"].CreatedByName; n == nil || *n != "Nguyễn Văn An" {
		t.Errorf("issuer of the same tenant: createdByName = %v, want the user's name", n)
	}
	// An id that is no user of THIS tenant (a builder operator, or another tenant's user) is null: the
	// join pins tenant_id, so a name of another tenant can never leak onto this tenant's screen.
	if n := byID["s-link-out"].CreatedByName; n != nil {
		t.Errorf("a user of another tenant must read createdByName null, got %q", *n)
	}
	if byID["s-link-out"].CreatedBy == nil || *byID["s-link-out"].CreatedBy != "src-u-out" {
		t.Errorf("createdBy must still carry the id, got %v", byID["s-link-out"].CreatedBy)
	}
	if byID["s-link-none"].CreatedByName != nil || byID["s-link-none"].CreatedBy != nil {
		t.Errorf("no issuer: createdBy/createdByName = %v/%v, want null/null", byID["s-link-none"].CreatedBy, byID["s-link-none"].CreatedByName)
	}
	// Get (the resend's echo) names the issuer the same way.
	got, err := r.Get(ctx, tenA, "s-link-in")
	if err != nil || got.CreatedByName == nil || *got.CreatedByName != "Nguyễn Văn An" {
		t.Errorf("Get createdByName = %v err=%v", got, err)
	}
}

// The listing with a source filter still reads through idx_email_log_list without a filesort.
func TestEmailLogListWithSourceUsesIndex_Integration(t *testing.T) {
	db, _ := elogDB(t)
	where := "l.tenant_id = ? AND l.source = ?"
	var key, extra string
	for try := 0; try < 2; try++ {
		rows, err := db.Queryx("EXPLAIN "+listSQL(where), tenA, SourceResetLink, 51)
		if err != nil {
			t.Fatal(err)
		}
		key, extra = "", ""
		for rows.Next() {
			m := map[string]any{}
			if err := rows.MapScan(m); err != nil {
				t.Fatal(err)
			}
			if s, _ := m["table"].([]byte); string(s) == "l" {
				k, _ := m["key"].([]byte)
				e, _ := m["Extra"].([]byte)
				key, extra = string(k), string(e)
			}
		}
		_ = rows.Close()
		if key == "idx_email_log_list" && !strings.Contains(extra, "filesort") {
			return
		}
	}
	t.Errorf("EXPLAIN key=%q extra=%q, want idx_email_log_list without filesort", key, extra)
}
