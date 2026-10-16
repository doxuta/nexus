//go:build integration

package audit_test

// specs/062-customer-workspace L3 T054 — the audit read path names a workspace-session builder
// ("Họ tên (Tên xưởng)") by looking the label up AT READ TIME, inside the same tenant. No
// audit_log column, no backfill: rows written before the session was opened carry the label too.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/audit"
)

const (
	alTenantX = "a1000000-0000-0000-0000-00000000000x"
	alTenantY = "a2000000-0000-0000-0000-00000000000y"
	alTenantH = "a3000000-0000-0000-0000-00000000000h"
)

type labelFixture struct {
	t    *testing.T
	db   *sqlx.DB
	repo *audit.SQLRepository
	base time.Time
	n    int
}

func newLabelFixture(t *testing.T) *labelFixture {
	t.Helper()
	// Production opens MySQL with loc=UTC (config.ApplyDBTimeouts); a test DSN with loc=Local would
	// shift every stored `at` by the machine's offset and the keyset cursor (always UTC) would walk
	// past the rows. Last loc wins in the driver, so pin UTC here.
	dsn := packageTestDSN
	if strings.Contains(dsn, "?") {
		dsn += "&loc=UTC"
	} else {
		dsn += "?loc=UTC"
	}
	db, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		t.Skipf("audit: MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &labelFixture{t: t, db: db, repo: audit.NewSQLRepository(db), base: time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)}
	for i, tid := range []string{alTenantX, alTenantY, alTenantH} {
		f.exec(`INSERT IGNORE INTO tenants (id, slug, name) VALUES (?, ?, ?)`, tid, "al-"+string(rune('a'+i))+uuid.NewString()[:8], "T"+tid[:2])
	}
	f.exec(`DELETE FROM audit_log WHERE tenant_id IN (?, ?, ?)`, alTenantX, alTenantY, alTenantH)
	f.exec(`DELETE FROM sys_workspace_sessions WHERE tenant_id IN (?, ?, ?)`, alTenantX, alTenantY, alTenantH)
	return f
}

func (f *labelFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatalf("exec %q: %v", q, err)
	}
}

func (f *labelFixture) user(tenant, id string) {
	f.exec(`INSERT INTO users (id, tenant_id, email, password_hash, full_name) VALUES (?, ?, ?, 'x', 'Nhan vien')`, id, tenant, id+"@x.test")
}

// session stamps a workspace session of `actor` in `tenant`; created is the session's age.
func (f *labelFixture) session(tenant, actor, label string, created time.Time, deleted bool) {
	var del any
	if deleted {
		del = created
	}
	f.exec(`INSERT INTO sys_workspace_sessions
		(id, tenant_id, actor_user_id, actor_tenant_id, actor_session_id, actor_label, expires_at, created_by, created_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), tenant, actor, alTenantH, uuid.NewString(), label, created.Add(8*time.Hour), actor, created, del)
}

// row writes one audit_log row, strictly later than the previous one.
func (f *labelFixture) row(tenant, actor, entity, record, action, diff string) string {
	f.t.Helper()
	f.n++
	id := uuid.NewString()
	var d, r any
	if diff != "" {
		d = diff
	}
	if record != "" {
		r = record
	}
	f.exec(`INSERT INTO audit_log (id, tenant_id, actor_user_id, entity_name, record_id, action, diff, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, tenant, actor, entity, r, action, d, f.base.Add(time.Duration(f.n)*time.Second))
	return id
}

func labelOf(rows []audit.AuditRow, id string) *string {
	for i := range rows {
		if rows[i].ID == id {
			return rows[i].ActorLabel
		}
	}
	panic("row " + id + " not in page")
}

func TestAuditRepo_ActorLabel(t *testing.T) {
	f := newLabelFixture(t)
	ctx := context.Background()
	const builder, builder2, staff = "b0000000-0000-0000-0000-000000000001", "b0000000-0000-0000-0000-000000000002", "c0000000-0000-0000-0000-000000000001"
	f.user(alTenantX, staff)

	// Rows written BEFORE any session exists must pick the label up once a session is opened.
	before := f.row(alTenantX, builder, "__layout__", "", audit.ActionUpdate, `{"after":{"x":1}}`)
	f.session(alTenantX, builder, "An (Nexus)", f.base.Add(-time.Minute), false)
	f.session(alTenantX, builder, "An Nguyễn (Nexus)", f.base.Add(-time.Second), false) // newest wins
	f.session(alTenantX, builder, "Deleted (Nexus)", f.base.Add(time.Hour), true)       // soft-deleted never counts

	// Another tenant's session of the same actor id must NOT label X's rows.
	f.session(alTenantY, builder2, "Leak (Nexus)", f.base.Add(-time.Minute), false)
	crossTenant := f.row(alTenantX, builder2, "leads", "r1", audit.ActionUpdate, `{"after":{"name":"a"}}`)

	staffRow := f.row(alTenantX, staff, "leads", "r1", audit.ActionUpdate, `{"after":{"name":"b"}}`)
	// diff.after.by branch: meta key, actor without a users row in X.
	byMeta := f.row(alTenantX, "ghost-op", "__tenant_backup__", "bk1", audit.ActionCreate, `{"after":{"by":"Bình (Xưởng)"}}`)
	// a session label beats diff.after.by
	sessionWins := f.row(alTenantX, builder, "__tenant_backup__", "bk2", audit.ActionCreate, `{"after":{"by":"Khác (Xưởng)"}}`)
	// a dynamic table row whose data happens to hold a `by` key must not forge the label
	dynBy := f.row(alTenantX, "ghost-op", "leads", "r2", audit.ActionUpdate, `{"after":{"by":"Giả (Nexus)"}}`)
	// a meta row of a real user in X carrying after.by must not forge it either
	staffMetaBy := f.row(alTenantX, staff, "__user__", "u1", audit.ActionUpdate, `{"after":{"by":"Giả (Nexus)"}}`)
	// LIKE '\_\_%' must be literal underscores, not "any two chars"
	wildcard := f.row(alTenantX, "ghost-op", "ab_leads", "r3", audit.ActionUpdate, `{"after":{"by":"Giả (Nexus)"}}`)

	rows, next, err := f.repo.Query(ctx, alTenantX, audit.QueryFilter{Limit: 200})
	if err != nil || next != "" {
		t.Fatalf("Query: next=%q err=%v", next, err)
	}
	want := map[string]string{
		"before-session row":   "An Nguyễn (Nexus)",
		"session wins over by": "An Nguyễn (Nexus)",
		"by on a meta key":     "Bình (Xưởng)",
	}
	for name, got := range map[string]*string{
		"before-session row":   labelOf(rows, before),
		"session wins over by": labelOf(rows, sessionWins),
		"by on a meta key":     labelOf(rows, byMeta),
	} {
		if got == nil || *got != want[name] {
			t.Errorf("%s: actorLabel = %v; want %q", name, deref(got), want[name])
		}
	}
	for name, got := range map[string]*string{
		"cross-tenant session leak": labelOf(rows, crossTenant),
		"staff user row":            labelOf(rows, staffRow),
		"dynamic-table by":          labelOf(rows, dynBy),
		"meta row of a tenant user": labelOf(rows, staffMetaBy),
		"underscore wildcard":       labelOf(rows, wildcard),
	} {
		if got != nil {
			t.Errorf("%s: actorLabel = %q; want null", name, *got)
		}
	}

	// RecordHistory shape: same Query, filtered by record — label rides along.
	hist, _, err := f.repo.Query(ctx, alTenantX, audit.QueryFilter{EntityName: "__tenant_backup__", RecordID: "bk1"})
	if err != nil || len(hist) != 1 || hist[0].ActorLabel == nil || *hist[0].ActorLabel != "Bình (Xưởng)" {
		t.Fatalf("record history label: rows=%+v err=%v", hist, err)
	}

	// Keyset pagination is unchanged by the extra column: 2 per page walks all 8 rows once.
	seen := map[string]bool{}
	after := ""
	for i := 0; i < 10; i++ {
		page, nx, err := f.repo.Query(ctx, alTenantX, audit.QueryFilter{Limit: 2, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			if seen[r.ID] {
				t.Fatalf("duplicate row %s across pages", r.ID)
			}
			seen[r.ID] = true
		}
		if nx == "" {
			break
		}
		after = nx
	}
	if len(seen) != 8 {
		t.Fatalf("paged %d rows; want 8", len(seen))
	}
}

// T071 — QueryFilter.BuilderOnly keeps exactly the rows that carry an actorLabel: it reuses the one
// label expression, so the filter and the Badge can never disagree (session label, diff.after.by on
// a reserved meta key; never a forged `by`, a cross-tenant session or a customer user).
func TestAuditRepo_BuilderOnly(t *testing.T) {
	f := newLabelFixture(t)
	ctx := context.Background()
	const builder, builderY, staff = "b0000000-0000-0000-0000-000000000011", "b0000000-0000-0000-0000-000000000012", "c0000000-0000-0000-0000-000000000011"
	f.user(alTenantX, staff)
	f.session(alTenantX, builder, "An (Nexus)", f.base.Add(-time.Minute), false)
	f.session(alTenantY, builderY, "Leak (Nexus)", f.base.Add(-time.Minute), false)

	viaSession := f.row(alTenantX, builder, "__layout__", "", audit.ActionUpdate, `{"after":{"x":1}}`)
	viaBy := f.row(alTenantX, "ghost-op", "__tenant_backup__", "bk1", audit.ActionCreate, `{"after":{"by":"Bình (Xưởng)"}}`)
	f.row(alTenantX, staff, "leads", "r1", audit.ActionUpdate, `{"after":{"name":"a"}}`)
	f.row(alTenantX, "ghost-op", "leads", "r2", audit.ActionUpdate, `{"after":{"by":"Giả (Nexus)"}}`)
	f.row(alTenantX, builderY, "leads", "r3", audit.ActionUpdate, `{"after":{"a":1}}`) // session only in Y
	f.row(alTenantY, builderY, "__layout__", "", audit.ActionUpdate, `{"after":{"y":1}}`)

	ids := func(fl audit.QueryFilter) map[string]bool {
		rows, _, err := f.repo.Query(ctx, alTenantX, fl)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			if r.ActorLabel == nil {
				t.Errorf("BuilderOnly returned row %s without an actorLabel", r.ID)
			}
			out[r.ID] = true
		}
		return out
	}
	got := ids(audit.QueryFilter{BuilderOnly: true, Limit: 200})
	if len(got) != 2 || !got[viaSession] || !got[viaBy] {
		t.Fatalf("BuilderOnly rows = %v; want exactly the session row and the meta-`by` row", got)
	}
	// Combines with the other filters (AND) and with keyset paging.
	if g := ids(audit.QueryFilter{BuilderOnly: true, EntityName: "__layout__", Limit: 200}); len(g) != 1 || !g[viaSession] {
		t.Fatalf("BuilderOnly+entity = %v; want only the layout row", g)
	}
	p1, next, err := f.repo.Query(ctx, alTenantX, audit.QueryFilter{BuilderOnly: true, Limit: 1})
	if err != nil || len(p1) != 1 || next == "" {
		t.Fatalf("page 1: rows=%d next=%q err=%v", len(p1), next, err)
	}
	p2, next2, err := f.repo.Query(ctx, alTenantX, audit.QueryFilter{BuilderOnly: true, Limit: 1, After: next})
	if err != nil || len(p2) != 1 || next2 != "" || p2[0].ID == p1[0].ID {
		t.Fatalf("page 2: rows=%v next=%q err=%v", p2, next2, err)
	}
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
