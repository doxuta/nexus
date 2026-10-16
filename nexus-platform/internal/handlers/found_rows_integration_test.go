//go:build integration

// Queue #91 (update-noop-returns-not-found): pressing "Lưu" without changing anything must not
// answer 404. MySQL reports CHANGED rows by default, so an UPDATE that rewrites identical values
// affects 0 rows — and every repository that reads RowsAffected()==0 as "no such row" turned a
// second identical save into NOT_FOUND (measured on PUT /admin/workflows/:id and
// PUT /admin/dashboards/:id). The fix is one DSN flag, clientFoundRows, set by
// config.ApplyDBTimeouts for BOTH pools. These tests open their pools through that same
// function, so they judge the production DSN rather than whatever TEST_DATABASE_URL carries.
//
// The second half proves the flag did not loosen any compare-and-set: every RowsAffected reader
// that means "did THIS call win" encodes the old state in its WHERE, so matched == changed there.
package handlers

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/notification"
	"nexus/platform/internal/report"
	"nexus/platform/internal/view"
	"nexus/platform/internal/workflow"
)

// TestIntegrationDSN_CountsMatchedRowsLikeProduction: only this file opens its pools through
// config.ApplyDBTimeouts; every other integration suite (here and in services, jobs, janitor,
// packinstall, …) opens TEST_DATABASE_URL as given. If that DSN lacks clientFoundRows=true,
// those suites count CHANGED rows while production counts MATCHED rows, so a compare-and-set
// that forgot to exclude its target state in the WHERE stays green in CI yet lets a second
// caller win in production (specs/057-found-rows-dsn). Fail the run instead of judging code
// under semantics production never uses.
//
// It reads the RAW env var, not dsn(): ittest.Setup appends the flag to the throwaway DSN it hands
// back (#328), so dsn() would pass even with the flag gone from ci.yml. The env var is what the
// suites that build their own DSN (services, jobs, janitor, packinstall, vertical_pack) consume.
func TestIntegrationDSN_CountsMatchedRowsLikeProduction(t *testing.T) {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	harness, err := gomysql.ParseDSN(raw)
	if err != nil {
		t.Fatalf("parse harness DSN %q: %v", raw, err)
	}
	prod, err := gomysql.ParseDSN(config.ApplyDBTimeouts(raw, 0, 0))
	if err != nil {
		t.Fatalf("parse production-shaped DSN: %v", err)
	}
	if harness.ClientFoundRows != prod.ClientFoundRows {
		t.Fatalf("integration DSN clientFoundRows=%v, production DSN clientFoundRows=%v — append "+
			"&clientFoundRows=true to TEST_DATABASE_URL (.github/workflows/ci.yml; locally run "+
			"through scripts/run-integration.sh, which adds it)", harness.ClientFoundRows, prod.ClientFoundRows)
	}
}

// foundRowsPools opens a sqlx and a GORM pool on the DSN exactly as cmd/server shapes it.
func foundRowsPools(t *testing.T) (*sqlx.DB, *gorm.DB) {
	t.Helper()
	shaped := config.ApplyDBTimeouts(dsn(), 0, 0)
	sx, err := sqlx.Connect("mysql", shaped)
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping found-rows integration", err)
	}
	t.Cleanup(func() { _ = sx.Close() })
	gdb, err := gorm.Open(gormmysql.Open(shaped), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	return sx, gdb
}

// TestUnchangedSave_IsNotNotFound: for each repository with the `RowsAffected()==0 ⇒ not found`
// shape, a save that re-sends the stored content succeeds, while an unknown id and another
// tenant's id still answer not-found.
func TestUnchangedSave_IsNotNotFound(t *testing.T) {
	sx, gdb := foundRowsPools(t)
	ctx := context.Background()
	tenant, other := uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		for _, tbl := range []string{"sys_workflows", "sys_dashboards", "sys_reports", "sys_views", "sys_apps", "sys_business_calendars"} {
			_, _ = sx.Exec("DELETE FROM "+tbl+" WHERE tenant_id = ?", tenant)
		}
	})

	wf := workflow.NewSQLRepository(sx)
	dash := dashboard.NewSQLRepository(sx)
	rep := report.NewSQLRepository(sx)
	vw := view.NewSQLRepository(sx)
	app := appmeta.NewSQLRepository(sx)
	cal := calmeta.NewSQLRepository(sx)

	cases := []struct {
		name     string
		notFound error
		create   func() (string, error)
		save     func(tenantID, id string) error // the SAME payload on every call
	}{
		{"workflow", workflow.ErrWorkflowNotFound,
			func() (string, error) {
				return wf.Create(ctx, workflow.CreateInput{TenantID: tenant, Name: "draft", EntityName: "lead",
					Trigger: workflow.TriggerAfterCreate, Actions: []byte(`[]`), IsActive: true})
			},
			func(tn, id string) error {
				return wf.Update(ctx, tn, id, workflow.UpdateInput{Name: "rule", EntityName: "lead",
					Trigger: workflow.TriggerAfterCreate, Actions: []byte(`[]`), IsActive: true})
			}},
		{"dashboard", dashboard.ErrDashboardNotFound,
			func() (string, error) {
				return dash.Create(ctx, dashboard.CreateInput{TenantID: tenant, Name: "draft", Widgets: []byte(`[]`)})
			},
			func(tn, id string) error {
				return dash.Update(ctx, tn, id, dashboard.UpdateInput{Name: "board", Widgets: []byte(`[]`)})
			}},
		{"report", report.ErrReportNotFound,
			func() (string, error) {
				return rep.Create(ctx, report.CreateInput{TenantID: tenant, Name: "draft", EntityName: "lead",
					Type: report.TypeTabular, Config: []byte(`{}`), IsActive: true})
			},
			func(tn, id string) error {
				return rep.Update(ctx, tn, id, report.UpdateInput{Name: "report", EntityName: "lead",
					Type: report.TypeTabular, Config: []byte(`{}`), IsActive: true})
			}},
		{"view", view.ErrViewNotFound,
			func() (string, error) {
				return vw.Create(ctx, view.CreateInput{TenantID: tenant, EntityName: "lead", Name: "draft", Config: []byte(`{}`)})
			},
			func(tn, id string) error {
				return vw.Update(ctx, tn, id, view.UpdateInput{Name: "view", Config: []byte(`{}`)})
			}},
		{"app", appmeta.ErrAppNotFound,
			func() (string, error) {
				return app.Create(ctx, appmeta.CreateInput{TenantID: tenant, Name: "sales", Label: "draft"})
			},
			func(tn, id string) error {
				return app.Update(ctx, tn, id, appmeta.UpdateInput{Label: "Sales", SortOrder: 1})
			}},
		{"business calendar", calmeta.ErrCalendarNotFound,
			func() (string, error) {
				return cal.Create(ctx, calmeta.CreateInput{TenantID: tenant, Name: "draft", Timezone: "Asia/Ho_Chi_Minh", Hours: []byte(`{}`)})
			},
			func(tn, id string) error {
				return cal.Update(ctx, tn, id, calmeta.UpdateInput{Name: "office", Timezone: "Asia/Ho_Chi_Minh", Hours: []byte(`{}`)})
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := tc.create()
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := tc.save(tenant, id); err != nil {
				t.Fatalf("first save (content changes): %v", err)
			}
			if err := tc.save(tenant, id); err != nil {
				t.Fatalf("second save with identical content = %v; want nil (the row exists, nothing to change)", err)
			}
			if err := tc.save(tenant, uuid.NewString()); !errors.Is(err, tc.notFound) {
				t.Fatalf("save of an unknown id = %v; want %v", err, tc.notFound)
			}
			if err := tc.save(other, id); !errors.Is(err, tc.notFound) {
				t.Fatalf("save through another tenant = %v; want %v", err, tc.notFound)
			}
		})
	}

	// The GORM pool (sys_* repositories on *gorm.DB) is opened from the same DSN; prove it
	// counts matched rows too, with a statement that changes nothing.
	t.Run("gorm pool counts matched rows", func(t *testing.T) {
		id, err := wf.Create(ctx, workflow.CreateInput{TenantID: tenant, Name: "gorm", EntityName: "lead",
			Trigger: workflow.TriggerAfterCreate, Actions: []byte(`[]`)})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		res := gdb.Exec("UPDATE sys_workflows SET name = name WHERE tenant_id = ? AND id = ?", tenant, id)
		if res.Error != nil {
			t.Fatalf("exec: %v", res.Error)
		}
		if res.RowsAffected != 1 {
			t.Fatalf("RowsAffected = %d for a matched-but-unchanged row; want 1", res.RowsAffected)
		}
	})
}

// TestFoundRows_CompareAndSetStillSingleWinner: the RowsAffected readers that mean "did THIS
// call win" keep their meaning under clientFoundRows, because each WHERE excludes the state
// its SET writes.
func TestFoundRows_CompareAndSetStillSingleWinner(t *testing.T) {
	sx, _ := foundRowsPools(t)
	ctx := context.Background()
	tenant := uuid.NewString()
	jobType := "found_rows_it_" + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = sx.Exec(`DELETE FROM sys_workflows WHERE tenant_id = ?`, tenant)
		_, _ = sx.Exec(`DELETE FROM sys_notifications WHERE tenant_id = ?`, tenant)
		_, _ = sx.Exec(`DELETE FROM sys_jobs WHERE job_type = ?`, jobType)
	})

	t.Run("scheduled workflow claim fires once", func(t *testing.T) {
		wf := workflow.NewSQLRepository(sx)
		id, err := wf.Create(ctx, workflow.CreateInput{TenantID: tenant, Name: "tick", EntityName: "lead",
			Trigger: workflow.TriggerScheduled, Actions: []byte(`[]`), IsActive: true, IntervalSeconds: 3600})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		now := time.Now()
		const racers = 8
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				won, err := wf.ClaimScheduledRun(ctx, tenant, id, now)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if won {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("%d of %d concurrent ticks claimed the run; want exactly 1", wins, racers)
		}
		if won, err := wf.ClaimScheduledRun(ctx, tenant, id, now); err != nil || won {
			t.Fatalf("re-claim in the same interval = (%v, %v); want (false, nil)", won, err)
		}
	})

	t.Run("mark-all-read counts only rows it flipped", func(t *testing.T) {
		user := uuid.NewString()
		for i := 0; i < 2; i++ {
			if _, err := sx.Exec(`INSERT INTO sys_notifications (id, tenant_id, user_id, message) VALUES (?, ?, ?, 'x')`,
				uuid.NewString(), tenant, user); err != nil {
				t.Fatalf("seed notification: %v", err)
			}
		}
		repo := notification.New(sx)
		if n, err := repo.MarkAllRead(ctx, tenant, user); err != nil || n != 2 {
			t.Fatalf("first MarkAllRead = (%d, %v); want (2, nil)", n, err)
		}
		if n, err := repo.MarkAllRead(ctx, tenant, user); err != nil || n != 0 {
			t.Fatalf("second MarkAllRead = (%d, %v); want (0, nil) — nothing left unread", n, err)
		}
	})

	t.Run("job claim is exclusive and a stale finish is refused", func(t *testing.T) {
		store := jobs.NewSQLStore(sx, 5)
		const jobCount = 12
		for i := 0; i < jobCount; i++ {
			if _, err := store.Enqueue(ctx, jobs.Job{Type: jobType}); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}
		lane := jobs.Lane{Only: jobType}
		var mu sync.Mutex
		owner := map[string]string{}
		claims := 0
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(worker string) {
				defer wg.Done()
				for {
					job, err := store.ClaimNext(ctx, worker, time.Now(), lane)
					if err != nil {
						t.Errorf("claim: %v", err)
						return
					}
					if job == nil {
						return
					}
					mu.Lock()
					owner[job.ID] = worker
					claims++
					mu.Unlock()
				}
			}("racer-" + uuid.NewString()[:4])
		}
		wg.Wait()
		if claims != jobCount || len(owner) != jobCount {
			t.Fatalf("%d claims over %d distinct jobs; want %d each (no double claim)", claims, len(owner), jobCount)
		}
		for id, worker := range owner {
			if ok, err := store.MarkDone(ctx, id, worker, time.Now()); err != nil || !ok {
				t.Fatalf("MarkDone by owner = (%v, %v); want applied", ok, err)
			}
			if ok, err := store.MarkDone(ctx, id, worker, time.Now()); err != nil || ok {
				t.Fatalf("second MarkDone = (%v, %v); want refused — the job is no longer running", ok, err)
			}
		}
	})
}
