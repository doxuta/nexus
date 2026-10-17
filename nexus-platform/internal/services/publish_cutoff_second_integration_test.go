//go:build integration

package services

// publish_cutoff_second_integration_test.go — issue #19. Metadata created_at has second precision and the
// cutoff's own second counts as released (schema_visibility.go), so a version may only name a second whose
// definitions it read. A cutoff taken from the clock released a table born later in that second, which no
// version saw: the customer had it at once, it was never pending, never recorded, and a rollback to that
// version did not hide it again. Each case stamps that table explicitly in the second the version began,
// after the version returned, while every definition the version reads was born an hour earlier: no case
// depends on where a second boundary falls.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type csEnv struct {
	dpEnv
	tenant string
	bctx   context.Context
	old    time.Time // when every definition of the tenant was born
}

// csSetup: a tenant with hk_deals, not delivered yet, whose definitions were all born an hour ago.
func csSetup(t *testing.T) *csEnv {
	t.Helper()
	e := dpSetupWith(t, NewWorkflowDocAdapter(), NewFieldDocAdapter(), NewEntityDocAdapter())
	s := &csEnv{dpEnv: e, old: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	s.tenant = e.newTenant(t)
	s.bctx = builderCtxOf(s.tenant, uuid.NewString())
	for _, q := range []string{`UPDATE sys_entities SET created_at = ?, updated_at = ? WHERE tenant_id = ?`,
		`UPDATE sys_fields SET created_at = ?, updated_at = ? WHERE tenant_id = ?`,
		`UPDATE sys_relationships SET created_at = ?, updated_at = ? WHERE tenant_id = ?`} {
		if _, err := e.sdb.Exec(q, s.old, s.old, s.tenant); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// table registers a live table stamped in the second of at, as a writer whose clock read at would (a raw
// INSERT: the repository's hooks would stamp now).
func (s *csEnv) table(t *testing.T, at time.Time) string {
	t.Helper()
	id, at := uuid.NewString(), at.UTC().Truncate(time.Second)
	if _, err := s.sdb.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, s.tenant, "cs_"+id[:8], "cs_"+id[:8], "obj_"+strings.ReplaceAll(id, "-", "")[:20], at, at); err != nil {
		t.Fatalf("insert table: %v", err)
	}
	return id
}

// customerSees: the table is in what GET /metadata/entities answers the customer (the same cutoff and filter).
func (s *csEnv) customerSees(t *testing.T, id string) bool {
	t.Helper()
	es, err := s.meta.ListEntities(s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if at, apply, err := s.gate.SchemaCutoff(legacyCtxOf(s.tenant), s.tenant); err != nil {
		t.Fatal(err)
	} else if apply {
		es = HideEntitiesNewerThan(es, at)
	}
	for _, e := range es {
		if e.ID == id {
			return true
		}
	}
	return false
}

// pending: the builder's change list offers the table to the next publish.
func (s *csEnv) pending(t *testing.T, id string) bool {
	t.Helper()
	items, _, err := s.pub.Changes(s.bctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.DocType == DocTypeEntity && it.DocKey == id {
			return true
		}
	}
	return false
}

func (s *csEnv) wantUnreleased(t *testing.T, id, after string) {
	t.Helper()
	if s.customerSees(t, id) {
		t.Errorf("after %s the customer sees a table born in its second that no version read", after)
	}
	if !s.pending(t, id) {
		t.Errorf("after %s that table is not pending: no publish would ever list or record it", after)
	}
}

func TestCutoffSameSecond_Integration(t *testing.T) {
	t.Run("baseline", func(t *testing.T) {
		s := csSetup(t)
		start := time.Now()
		if _, err := s.pub.Baseline(s.bctx, s.tenant, ""); err != nil {
			t.Fatalf("Baseline: %v", err)
		}
		s.wantUnreleased(t, s.table(t, start), "the baseline")
	})

	// A table stamped ahead of this process's clock (another replica's) holds the publish for a second; the
	// cutoff must still name the newest second the publish read, not the one running when it re-reads.
	t.Run("publish", func(t *testing.T) {
		s := csSetup(t)
		if _, err := s.pub.Baseline(s.bctx, s.tenant, ""); err != nil {
			t.Fatalf("Baseline: %v", err)
		}
		// whatever cutoff the baseline wrote, the head names the definitions' second: only this publish is judged
		if _, err := s.sdb.Exec(`UPDATE sys_meta_versions SET schema_cutoff_at = ? WHERE tenant_id = ?`, s.old, s.tenant); err != nil {
			t.Fatal(err)
		}
		s.gate.RefreshCutoff(s.tenant, s.old)
		seen, ahead := s.table(t, s.old.Add(time.Second)), s.table(t, time.Now().Add(30*time.Second))
		start := time.Now()
		if _, err := s.publishWithCurrentHash(s.bctx, s.tenant, false); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if !s.customerSees(t, seen) {
			t.Error("the publish did not release the table it read")
		}
		if s.customerSees(t, ahead) || !s.pending(t, ahead) {
			t.Error("the publish released a table stamped ahead of its clock")
		}
		s.wantUnreleased(t, s.table(t, start), "the publish")
	})

	// A rollback writes its target's cutoff: back to the baseline, a table born in the baseline's second that a
	// later publish released is hidden again, like anything else born after the target.
	t.Run("rollback", func(t *testing.T) {
		s := csSetup(t)
		start := time.Now()
		if _, err := s.pub.Baseline(s.bctx, s.tenant, ""); err != nil {
			t.Fatalf("Baseline: %v", err)
		}
		late := s.table(t, start)
		if _, err := s.wf.CreateWorkflow(s.bctx, s.tenant, lockedVeto("cs rule")); err != nil { // a draft: v2 publishes something either way
			t.Fatalf("CreateWorkflow: %v", err)
		}
		res, err := s.publishWithCurrentHash(s.bctx, s.tenant, false)
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
		var v1 string
		if err := s.sdb.Get(&v1, `SELECT id FROM sys_meta_versions WHERE tenant_id = ? AND version_no = 1`, s.tenant); err != nil {
			t.Fatal(err)
		}
		plan, err := s.pub.RollbackPlan(s.bctx, s.tenant, v1)
		if err != nil {
			t.Fatalf("RollbackPlan: %v", err)
		}
		if _, err := s.pub.Rollback(s.bctx, s.tenant, v1, RollbackRequest{ExpectedHeadVersion: res.VersionNo, ExpectedPlanToken: plan.PlanToken}); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		listed := false
		for _, r := range plan.Rehides {
			listed = listed || r.DocKey == late
		}
		if !listed {
			t.Errorf("rehides = %+v: the plan does not say the table is hidden again", plan.Rehides)
		}
		s.wantUnreleased(t, late, "the rollback to the baseline")
	})
}
