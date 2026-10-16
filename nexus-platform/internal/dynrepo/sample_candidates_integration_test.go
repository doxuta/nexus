//go:build integration

package dynrepo

import (
	"context"
	"testing"
)

// S9-5: LiveRecordIDs feeds the generator its parent candidates — live rows only, this tenant only,
// sample-marked rows first.
func TestLiveRecordIDs_MarkedFirstTenantScopedLiveOnly(t *testing.T) {
	repo := New(testDB(t))
	const entity = "it_sample_candidates"
	sampleTable(t, repo, entity)
	ctx := context.Background()
	mark := WithSampleMark(ctx, SampleMark{BatchID: "b1", Source: "generator"})

	insert := func(c context.Context, tenant, name string) string {
		t.Helper()
		id, err := repo.InsertRecord(c, entity, tenant, sampleUser, map[string]any{"name": name}, nil)
		if err != nil {
			t.Fatalf("InsertRecord %s: %v", name, err)
		}
		return id
	}
	real1, real2 := insert(ctx, sampleTenant, "real1"), insert(ctx, sampleTenant, "real2")
	m1, m2 := insert(mark, sampleTenant, "m1"), insert(mark, sampleTenant, "m2")
	binned := insert(mark, sampleTenant, "binned")
	other := insert(mark, sampleTenant2, "other-tenant")
	if _, err := repo.db.Exec("UPDATE `obj_"+entity+"` SET `deleted_at` = NOW() WHERE `id` = ?", binned); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	got, err := repo.LiveRecordIDs(ctx, sampleTenant, entity, 3)
	if err != nil {
		t.Fatalf("LiveRecordIDs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ids = %v, want 3", got)
	}
	marked := map[string]bool{m1: true, m2: true}
	if !marked[got[0]] || !marked[got[1]] {
		t.Fatalf("ids = %v, want the two marked rows (%s, %s) first", got, m1, m2)
	}
	if got[2] != real1 && got[2] != real2 {
		t.Fatalf("third id = %s, want a real row", got[2])
	}
	all, err := repo.LiveRecordIDs(ctx, sampleTenant, entity, 50)
	if err != nil || len(all) != 4 {
		t.Fatalf("all = %v err=%v, want the 4 live rows of the tenant", all, err)
	}
	for _, id := range all {
		if id == binned || id == other {
			t.Fatalf("ids %v contain %s: a recycle-bin row or another tenant's row", all, id)
		}
	}
}
