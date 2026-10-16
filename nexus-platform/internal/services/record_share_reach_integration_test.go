//go:build integration

// DAILY-QUEUE #225 record-manual-share-reach, on real MySQL: a record that B can see ONLY because it
// was shared with B (person or team) must also be counted/found wherever B's role-scoped numbers are
// computed — dashboard widget, saved report (tabular + summary), global search, work-queue card —
// otherwise a card says 0 while the list it opens shows the record. The ACL stage is a fake that
// resolves view=owner for every non-admin, which is what makes the shared record share-only for B.
package services

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
	"nexus/platform/internal/workqueue"
)

type ownerACL struct{}

func (ownerACL) Authorize(_ context.Context, _, _, _, _ string) (bool, string, error) {
	return true, "owner", nil
}

type oneQueueStore struct {
	workQueueStore
	q workqueue.Queue
}

func (s oneQueueStore) ListActive(context.Context, string) ([]workqueue.Queue, error) {
	return []workqueue.Queue{s.q}, nil
}

func TestManualShareReach_NumbersFollowTheList(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	repo := dynrepo.New(sdb)
	recordSvc := NewRecordService(metaRepo, repo).WithACLAuthorizer(ownerACL{})
	entitySvc := NewEntityService(repo, metaRepo)
	ctx := context.Background()

	const tenant = "5ea1e000-0000-0000-0000-00000000aa01"
	const entity = "sr_deals"
	owner, viewer, mate, stranger := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, u := range []string{owner, viewer, mate, stranger} {
		if _, err := sdb.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, status, is_admin, created_at, updated_at)
			VALUES (?, ?, ?, 'x', 'active', 0, NOW(3), NOW(3))`, u, tenant, u+"@x.test"); err != nil {
			// tenants FK may be enforced: seed the tenant first, then retry once.
			if _, terr := sdb.Exec(`INSERT INTO tenants (id, slug, name, status, created_at, updated_at) VALUES (?, 'sr-reach', 'sr-reach', 'active', NOW(3), NOW(3))`, tenant); terr != nil {
				t.Fatalf("seed tenant: %v (user insert: %v)", terr, err)
			}
			if _, err := sdb.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, status, is_admin, created_at, updated_at)
				VALUES (?, ?, ?, 'x', 'active', 0, NOW(3), NOW(3))`, u, tenant, u+"@x.test"); err != nil {
				t.Fatalf("seed user: %v", err)
			}
		}
	}
	team := uuid.NewString()
	if _, err := sdb.Exec(`INSERT INTO teams (id, tenant_id, name) VALUES (?, ?, 'reach-team')`, team, tenant); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if _, err := sdb.Exec(`INSERT INTO team_memberships (id, user_id, team_id) VALUES (?, ?, ?)`, uuid.NewString(), mate, team); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: entity, Label: "Deals", Fields: []FieldInput{{Name: "name", Label: "Name", FieldType: "text"}},
	}); err != nil {
		t.Fatalf("create entity: %v", err)
	}
	adminScope := RecordScope{UserID: owner, AccessLevel: "all", IsAdmin: true}
	mk := func(name string) string {
		t.Helper()
		rec, err := recordSvc.Create(ctx, adminScope, tenant, owner, entity, map[string]any{"name": name})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return rec
	}
	recPerson, recTeam := mk("Acme shared with a person"), mk("Acme shared with a team")
	mk("Acme private")
	share := func(rec, userID, teamID string) {
		t.Helper()
		var u, tm any
		if userID != "" {
			u = userID
		}
		if teamID != "" {
			tm = teamID
		}
		if _, err := sdb.Exec(`INSERT INTO sys_record_shares (id, tenant_id, entity_name, record_id, user_id, team_id, shared_by, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, uuid.NewString(), tenant, entity, rec, u, tm, owner, owner); err != nil {
			t.Fatalf("share: %v", err)
		}
	}
	share(recPerson, viewer, "")
	share(recTeam, "", team)

	// A flat route (no :entity segment) hands the service a scope that never saw the ACL stage:
	// AccessNone, no share flag. Every path below must resolve view itself.
	flat := func(user string) RecordScope { return RecordScope{UserID: user, RoleID: "role-x", AccessLevel: "none"} }

	t.Run("dashboard widget counts the shared record", func(t *testing.T) {
		count := func(user string) int64 {
			t.Helper()
			res := runWidget(ctx, recordSvc, nil, flat(user), tenant, WidgetConfig{ID: "w", Type: "kpi", Entity: entity, Metric: "count"}, nil)
			if res.Result == nil {
				t.Fatalf("widget error: %s (%s)", res.Error, res.Code)
			}
			return res.Result.Scalar.Count
		}
		if got := count(viewer); got != 1 {
			t.Fatalf("viewer's widget count = %d; want 1 (the record shared with them)", got)
		}
		if got := count(mate); got != 1 {
			t.Fatalf("team mate's widget count = %d; want 1 (the record shared with their team)", got)
		}
		if got := count(stranger); got != 0 {
			t.Fatalf("stranger's widget count = %d; want 0", got)
		}
	})

	t.Run("saved report (tabular and summary) counts the shared record", func(t *testing.T) {
		tab := report.Record{ID: "r-tab", TenantID: tenant, Name: "T", EntityName: entity, Type: "tabular",
			Config: json.RawMessage(`{"columns":["name"]}`), IsActive: true}
		sum := report.Record{ID: "r-sum", TenantID: tenant, Name: "S", EntityName: entity, Type: "summary",
			Config: json.RawMessage(`{"metric":"count"}`), IsActive: true}
		run := func(rec report.Record, user string) ReportRunResult {
			t.Helper()
			res, err := NewReportService(&fakeReportRepo{rec: rec}, recordSvc).RunReport(ctx, flat(user), tenant, rec.ID, nil)
			if err != nil {
				t.Fatalf("RunReport %s: %v", rec.ID, err)
			}
			return res
		}
		if got := run(tab, viewer).Tabular.Total; got != 1 {
			t.Fatalf("tabular total for the viewer = %d; want 1", got)
		}
		if got := run(tab, stranger).Tabular.Total; got != 0 {
			t.Fatalf("tabular total for a stranger = %d; want 0", got)
		}
		if res := run(sum, mate); res.Aggregate == nil || res.Aggregate.Scalar.Count != 1 {
			t.Fatalf("summary for the team mate = %+v; want count 1", res.Aggregate)
		}
	})

	t.Run("global search finds the shared record", func(t *testing.T) {
		hits := func(user string) int {
			t.Helper()
			groups, err := NewSearchService(metaRepo, ownerACL{}, recordSvc).Search(ctx, flat(user), tenant, "Acme")
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			n := 0
			for _, g := range groups {
				if g.Entity == entity {
					n += len(g.Hits)
				}
			}
			return n
		}
		if got := hits(viewer); got != 1 {
			t.Fatalf("viewer search hits = %d; want 1", got)
		}
		if got := hits(mate); got != 1 {
			t.Fatalf("team mate search hits = %d; want 1", got)
		}
		if got := hits(stranger); got != 0 {
			t.Fatalf("stranger search hits = %d; want 0", got)
		}
	})

	t.Run("work-queue card counts what the list it opens shows", func(t *testing.T) {
		svc := NewWorkQueueService(oneQueueStore{q: workqueue.Queue{ID: "q1", Name: "all", Label: "All", EntityName: entity, ActionVerb: "open"}}, recordSvc, ownerACL{}, nil, nil)
		card := func(user string) int64 {
			t.Helper()
			views, err := svc.ListForUser(ctx, flat(user), tenant)
			if err != nil || len(views) != 1 || views[0].Count == nil {
				t.Fatalf("ListForUser: %v %+v", err, views)
			}
			return *views[0].Count
		}
		if got := card(viewer); got != 1 {
			t.Fatalf("viewer card = %d; want 1", got)
		}
		if got := card(stranger); got != 0 {
			t.Fatalf("stranger card = %d; want 0", got)
		}
	})
}
