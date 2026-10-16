//go:build integration

package dynrepo

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/ddl"
)

// TestRowScope_UserFieldLevel is the real-MySQL proof of #283: a principal at access_level
// "field:bac_si" sees the rows whose user field `bac_si` points at them PLUS the rows they own,
// through every read (list, get, count/aggregate) and every write (update, delete) — and nothing
// else, in the same tenant or another.
//
// Records (tenant A, all created by the receptionist, so receptionist = owner):
//   - recDocA → bac_si = docA
//   - recDocB → bac_si = docB
//   - recNone → no bac_si
//   - recOwnByDocA → created BY docA for docB (docA owns it, docB is the doctor)
//
// Tenant B has its own row pointing at docA's id to prove the tenant filter still leads.
func TestRowScope_UserFieldLevel(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_rowscope_field"

	tenantA, tenantB := newID(), newID()
	reception, docA, docB, userB := newID(), newID(), newID(), newID()

	seedTenant(t, db, tenantA, "rsf-a")
	seedTenant(t, db, tenantB, "rsf-b")
	for _, u := range []string{reception, docA, docB} {
		seedUser(t, db, u, tenantA)
	}
	seedUser(t, db, userB, tenantB)
	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		cleanupA5(db, tenantA, tenantB, []string{reception, docA, docB, userB}, nil)
	})

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "bac_si", FieldType: "user"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	ins := func(tenant, by string, data map[string]any) string {
		t.Helper()
		id, err := repo.InsertRecord(ctx, entity, tenant, by, data, nil)
		if err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
		return id
	}
	recDocA := ins(tenantA, reception, map[string]any{"name": "a", "bac_si": docA})
	recDocB := ins(tenantA, reception, map[string]any{"name": "b", "bac_si": docB})
	recNone := ins(tenantA, reception, map[string]any{"name": "none"})
	recOwnByDocA := ins(tenantA, docA, map[string]any{"name": "own", "bac_si": docB})
	recTenantB := ins(tenantB, userB, map[string]any{"name": "tb", "bac_si": docA})

	docScope := RowScope{UserID: docA, Level: FieldAccessLevel("bac_si")}

	t.Run("list: assigned rows + own rows only", func(t *testing.T) {
		recs, total, err := repo.ListRecords(ctx, entity, tenantA, docScope, ListParams{Page: 1, Limit: 50})
		if err != nil {
			t.Fatalf("ListRecords: %v", err)
		}
		got := map[string]bool{}
		for _, r := range recs {
			got[r.ID] = true
		}
		if !got[recDocA] || !got[recOwnByDocA] || got[recDocB] || got[recNone] || got[recTenantB] || total != 2 {
			t.Fatalf("docA saw %v (total %d); want exactly {recDocA, recOwnByDocA}", got, total)
		}
	})

	t.Run("get: another doctor's row is not found", func(t *testing.T) {
		if _, err := repo.GetRecord(ctx, entity, tenantA, recDocA, docScope); err != nil {
			t.Fatalf("Get assigned: %v", err)
		}
		for _, id := range []string{recDocB, recNone, recTenantB} {
			if _, err := repo.GetRecord(ctx, entity, tenantA, id, docScope); !errors.Is(err, ErrRecordNotFound) {
				t.Fatalf("Get %s err = %v; want ErrRecordNotFound", id, err)
			}
		}
	})

	t.Run("receptionist at owner level still sees her own rows, not the doctor's", func(t *testing.T) {
		recs, _, err := repo.ListRecords(ctx, entity, tenantA, RowScope{UserID: reception, Level: AccessOwner}, ListParams{Page: 1, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 3 { // she created recDocA, recDocB, recNone; recOwnByDocA is docA's
			t.Fatalf("reception saw %d rows; want 3", len(recs))
		}
	})

	t.Run("update + delete are scoped by the same predicate", func(t *testing.T) {
		if err := repo.UpdateRecord(ctx, entity, tenantA, recDocB, docA, docScope, map[string]any{"name": "x"}, nil, nil); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("Update other doctor's row err = %v; want ErrRecordNotFound", err)
		}
		if err := repo.SoftDeleteRecord(ctx, entity, tenantA, recDocB, docScope); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("Delete other doctor's row err = %v; want ErrRecordNotFound", err)
		}
		if err := repo.UpdateRecord(ctx, entity, tenantA, recDocA, docA, docScope, map[string]any{"name": "ok"}, nil, nil); err != nil {
			t.Fatalf("Update assigned row: %v", err)
		}
	})

	t.Run("aggregate covers exactly the readable rows", func(t *testing.T) {
		res, err := repo.AggregateScalar(ctx, entity, tenantA, docScope, AggregateParams{Metric: MetricCount})
		if err != nil {
			t.Fatalf("AggregateScalar: %v", err)
		}
		if res.Count != 2 {
			t.Fatalf("count = %d; want 2", res.Count)
		}
	})

	t.Run("a doctor reassigned away loses the row", func(t *testing.T) {
		if err := repo.UpdateRecord(ctx, entity, tenantA, recDocA, reception, AdminScope(), map[string]any{"bac_si": docB}, nil, nil); err != nil {
			t.Fatalf("admin reassign: %v", err)
		}
		if _, err := repo.GetRecord(ctx, entity, tenantA, recDocA, docScope); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("after reassign err = %v; want ErrRecordNotFound", err)
		}
	})
}
