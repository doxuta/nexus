//go:build integration

package emaillog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
)

const (
	tenA = "aaaaaaaa-0000-0000-0000-00000000000a"
	tenB = "bbbbbbbb-0000-0000-0000-00000000000b"
)

func elogDB(t *testing.T) (*sqlx.DB, *Repository) {
	t.Helper()
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("emaillog: MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewRepository(db)
}

func seed(t *testing.T, r *Repository, tenant, id, status string) Entry {
	t.Helper()
	idx := 0
	e := Entry{ID: id, TenantID: tenant, Source: SourceWorkflow, Status: status, RecipientMasked: "a***@gmail.com",
		SubjectTemplate: "Chào {{name}}", WorkflowID: "wf-1", ActionIndex: &idx, EntityName: "crm_contacts", RecordID: "rec-1"}
	if err := r.Insert(context.Background(), e); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
	return e
}

func statusOf(t *testing.T, db *sqlx.DB, tenant, id string) string {
	t.Helper()
	var s string
	if err := db.Get(&s, "SELECT status FROM sys_email_log WHERE tenant_id = ? AND id = ?", tenant, id); err != nil {
		t.Fatalf("read status of %s: %v", id, err)
	}
	return s
}

// T015: a terminal state sticks — the late writer loses, in both directions — and a write for
// another tenant changes nothing.
func TestEmailLogCAS_Integration(t *testing.T) {
	db, r := elogDB(t)
	ctx := context.Background()

	seed(t, r, tenA, "cas-1", StatusQueued)
	if err := r.MarkSent(ctx, tenA, "cas-1", 2); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkFailed(ctx, tenA, "cas-1", 5, "late failure"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, db, tenA, "cas-1"); got != StatusSent {
		t.Errorf("sent then late failed = %q, want sent", got)
	}

	seed(t, r, tenA, "cas-2", StatusQueued)
	if err := r.MarkFailed(ctx, tenA, "cas-2", 5, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkSent(ctx, tenA, "cas-2", 6); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, db, tenA, "cas-2"); got != StatusFailed {
		t.Errorf("failed then late sent = %q, want failed", got)
	}

	seed(t, r, tenA, "cas-3", StatusQueued)
	if err := r.MarkSent(ctx, tenB, "cas-3", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkAttempt(ctx, tenB, "cas-3", 3, "x"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, db, tenA, "cas-3"); got != StatusQueued {
		t.Errorf("another tenant's UPDATE changed the row: %q", got)
	}
	var attempts int
	if err := db.Get(&attempts, "SELECT attempts FROM sys_email_log WHERE id = 'cas-3'"); err != nil || attempts != 0 {
		t.Errorf("attempts = %d err=%v, want 0 (tenant B must not touch it)", attempts, err)
	}
}

// T015: long strings are cut on a rune boundary INSIDE the repository — in strict mode an overlong
// value is "Data too long" and the row (the evidence a mail failed) would silently vanish.
func TestEmailLogClipsLongStrings_Integration(t *testing.T) {
	db, r := elogDB(t)
	ctx := context.Background()
	idx := 1
	e := Entry{ID: "clip-1", TenantID: tenA, Source: SourceWorkflow, Status: StatusQueued,
		RecipientMasked: strings.Repeat("Đ", 400), SubjectTemplate: strings.Repeat("Đ", 400), ActionIndex: &idx}
	if err := r.Insert(ctx, e); err != nil {
		t.Fatalf("insert with 400-rune strings: %v", err)
	}
	if err := r.MarkFailed(ctx, tenA, "clip-1", 1, strings.Repeat("Đ", 5000)); err != nil {
		t.Fatalf("MarkFailed with a 5000-rune error: %v", err)
	}
	var row struct {
		Subject string `db:"subject_template"`
		Rec     string `db:"recipient_masked"`
		Err     string `db:"last_error"`
		Status  string `db:"status"`
	}
	if err := db.Get(&row, "SELECT subject_template, recipient_masked, last_error, status FROM sys_email_log WHERE id = 'clip-1'"); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(row.Subject)); n > 255 || n < 200 {
		t.Errorf("subject_template runes = %d, want clipped to <=255", n)
	}
	if n := len([]rune(row.Rec)); n > 255 {
		t.Errorf("recipient_masked runes = %d", n)
	}
	if n := len([]rune(row.Err)); n > 2000 || n < 1500 {
		t.Errorf("last_error runes = %d, want clipped to <=2000", n)
	}
	if row.Status != StatusFailed {
		t.Errorf("status = %q", row.Status)
	}
}

func TestEmailLogAttachJobAndAttempt_Integration(t *testing.T) {
	db, r := elogDB(t)
	ctx := context.Background()
	seed(t, r, tenA, "att-1", StatusQueued)
	if err := r.AttachJob(ctx, tenA, "att-1", "job-9"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkAttempt(ctx, tenA, "att-1", 2, "smtp down"); err != nil {
		t.Fatal(err)
	}
	var row struct {
		JobID    string `db:"job_id"`
		Attempts int    `db:"attempts"`
		Err      string `db:"last_error"`
		Status   string `db:"status"`
	}
	if err := db.Get(&row, "SELECT job_id, attempts, last_error, status FROM sys_email_log WHERE id = 'att-1'"); err != nil {
		t.Fatal(err)
	}
	if row.JobID != "job-9" || row.Attempts != 2 || row.Err != "smtp down" || row.Status != StatusQueued {
		t.Errorf("row = %+v", row)
	}
}

// T015b: the read side — newest first, status filter, keyset pages, tenant isolation, workflow name.
func TestEmailLogListForTenant_Integration(t *testing.T) {
	db, r := elogDB(t)
	ctx := context.Background()
	if _, err := db.Exec("DELETE FROM sys_email_log WHERE tenant_id IN (?, ?)", tenA, tenB); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sys_workflows (id, tenant_id, name, entity_name, trigger_event, is_active, actions, created_by)
		VALUES ('wf-named', ?, 'Chào khách mới', 'crm_contacts', 'after_create', 1, '[]', 'u')`, tenA); err != nil {
		t.Skipf("sys_workflows seed shape differs (%v) — workflow-name assertion needs updating", err)
	}
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		id := "list-" + string(rune('a'+i))
		st := StatusSent
		if i%2 == 1 {
			st = StatusFailed
		}
		wf := "wf-named"
		if i == 4 {
			wf = "wf-deleted"
		}
		idx := 0
		e := Entry{ID: id, TenantID: tenA, Source: SourceWorkflow, Status: st, RecipientMasked: "a***@x.vn", WorkflowID: wf, ActionIndex: &idx}
		if err := r.Insert(ctx, e); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE sys_email_log SET created_at = ? WHERE id = ?", base.Add(time.Duration(i)*time.Minute), id); err != nil {
			t.Fatal(err)
		}
	}
	idx := 0
	if err := r.Insert(ctx, Entry{ID: "other-b", TenantID: tenB, Source: SourceWorkflow, Status: StatusSent, RecipientMasked: "b***@x.vn", ActionIndex: &idx}); err != nil {
		t.Fatal(err)
	}

	var all []Row
	f := Filter{Limit: 2}
	for page := 0; page < 10; page++ {
		rows, next, err := r.ListForTenant(ctx, tenA, f)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, rows...)
		if next == "" {
			break
		}
		f.After = next
	}
	if len(all) != 5 {
		t.Fatalf("walked %d rows, want 5", len(all))
	}
	for i, want := range []string{"list-e", "list-d", "list-c", "list-b", "list-a"} {
		if all[i].ID != want {
			t.Errorf("row %d = %s, want %s (newest first)", i, all[i].ID, want)
		}
	}
	if all[4].WorkflowName == nil || *all[4].WorkflowName != "Chào khách mới" {
		t.Errorf("workflowName = %v, want the rule's name", all[4].WorkflowName)
	}
	if all[0].WorkflowName != nil {
		t.Errorf("a deleted rule must read workflowName null, got %q", *all[0].WorkflowName)
	}

	failed, _, err := r.ListForTenant(ctx, tenA, Filter{Status: StatusFailed, Limit: 50})
	if err != nil || len(failed) != 2 {
		t.Errorf("failed filter = %d rows err=%v, want 2", len(failed), err)
	}
	other, _, err := r.ListForTenant(ctx, tenB, Filter{Limit: 50})
	if err != nil || len(other) != 1 || other[0].ID != "other-b" {
		t.Errorf("tenant B sees %+v err=%v, want only its own row", other, err)
	}
	if _, _, err := r.ListForTenant(ctx, tenA, Filter{After: dynrepo.EncodeCursor("sys_jobs.created_at", "DESC", false, "2026-10-01 08:00:00.000", "x")}); !errors.Is(err, dynrepo.ErrInvalidCursor) {
		t.Errorf("a /admin/jobs cursor must be ErrInvalidCursor, got %v", err)
	}
	if _, _, err := r.ListForTenant(ctx, "", Filter{}); err == nil {
		t.Errorf("empty tenant must be refused")
	}
}

// Several mails fired by one event share a millisecond: the (created_at, id) tie-break must carry the
// walk across a page boundary inside the tie without losing or repeating a row.
func TestEmailLogKeysetTieBreak_Integration(t *testing.T) {
	db, r := elogDB(t)
	ctx := context.Background()
	if _, err := db.Exec("DELETE FROM sys_email_log WHERE tenant_id = ?", tenA); err != nil {
		t.Fatal(err)
	}
	same := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	want := []string{"tie-e", "tie-d", "tie-c", "tie-b", "tie-a"}
	for _, id := range want {
		seed(t, r, tenA, id, StatusSent)
		if _, err := db.Exec("UPDATE sys_email_log SET created_at = ? WHERE id = ?", same, id); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	f := Filter{Limit: 2}
	for page := 0; page < 10; page++ {
		rows, next, err := r.ListForTenant(ctx, tenA, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			got = append(got, row.ID)
		}
		if next == "" {
			break
		}
		f.After = next
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("walk over equal created_at = %v, want %v", got, want)
	}
}

// T015b: the listing (first page, status filter, page 2) reads through idx_email_log_list without a
// filesort. One retry: a freshly filled table can plan differently before stats settle (#329).
func TestEmailLogListUsesIndex_Integration(t *testing.T) {
	db, _ := elogDB(t)
	cases := map[string]struct {
		where string
		args  []any
	}{
		"first page": {"l.tenant_id = ?", []any{tenA}},
		"status":     {"l.tenant_id = ? AND l.status = ?", []any{tenA, StatusFailed}},
		"page 2": {"l.tenant_id = ? AND (l.created_at < ? OR (l.created_at = ? AND l.id < ?))",
			[]any{tenA, "2026-10-02 09:00:00.000", "2026-10-02 09:00:00.000", "x"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			args := append(append([]any{}, c.args...), 51)
			var key, extra string
			for try := 0; try < 2; try++ {
				rows, err := db.Queryx("EXPLAIN "+listSQL(c.where), args...)
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
		})
	}
}
