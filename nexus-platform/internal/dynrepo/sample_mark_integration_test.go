//go:build integration

package dynrepo

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/ddl"
)

const (
	sampleTenant  = "11111111-0000-0000-0000-0000000005a1"
	sampleTenant2 = "11111111-0000-0000-0000-0000000005a2"
	sampleUser    = "00000000-0000-0000-0000-0000000005aa"
)

// sampleTable provisions a throwaway obj_ table and wipes both it and its marks afterwards.
func sampleTable(t *testing.T, repo *Repository, entity string) {
	t.Helper()
	ctx := context.Background()
	clean := func() {
		_ = repo.DropTable(ctx, entity)
		_, _ = repo.db.Exec("DELETE FROM `sys_sample_records` WHERE `entity_name` = ?", entity)
		_, _ = repo.db.Exec("DELETE FROM `sys_unique_index` WHERE `entity_name` = ?", entity)
	}
	clean()
	t.Cleanup(clean)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
}

func markCount(t *testing.T, repo *Repository, tenant, entity string) int {
	t.Helper()
	var n int
	if err := repo.db.Get(&n, "SELECT COUNT(*) FROM `sys_sample_records` WHERE `tenant_id` = ? AND `entity_name` = ?", tenant, entity); err != nil {
		t.Fatalf("count marks: %v", err)
	}
	return n
}

func rowCount(t *testing.T, repo *Repository, entity string) int {
	t.Helper()
	var n int
	if err := repo.db.Get(&n, "SELECT COUNT(*) FROM `obj_"+entity+"`"); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// T070: the mark is written in the SAME transaction as the row, by both insert paths.
func TestSampleMark_InsertRecord_SameTx(t *testing.T) {
	repo := New(testDB(t))
	const entity = "it_sample_single"
	sampleTable(t, repo, entity)

	mctx := WithSampleMark(context.Background(), SampleMark{BatchID: "b1", Source: "ai"})
	id, err := repo.InsertRecord(mctx, entity, sampleTenant, sampleUser, map[string]any{"name": "A"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}
	var src, batch, rec, by string
	if err := repo.db.QueryRow("SELECT source, batch_id, record_id, created_by FROM sys_sample_records WHERE tenant_id = ? AND entity_name = ?", sampleTenant, entity).Scan(&src, &batch, &rec, &by); err != nil {
		t.Fatalf("mark row: %v", err)
	}
	if src != "ai" || batch != "b1" || rec != id || by != sampleUser {
		t.Fatalf("mark = %s/%s/%s/%s, want ai/b1/%s/%s", src, batch, rec, by, id, sampleUser)
	}

	// No ctx mark -> no mark row.
	if _, err := repo.InsertRecord(context.Background(), entity, sampleTenant, sampleUser, map[string]any{"name": "real"}, nil); err != nil {
		t.Fatalf("InsertRecord plain: %v", err)
	}
	if n := markCount(t, repo, sampleTenant, entity); n != 1 {
		t.Fatalf("marks = %d, want 1 (the unmarked insert must add none)", n)
	}

	// A failing mark insert rolls the obj_ row back too (VARCHAR(16) overflow -> 1406 strict).
	bad := WithSampleMark(context.Background(), SampleMark{BatchID: "b2", Source: strings.Repeat("x", 17)})
	if _, err := repo.InsertRecord(bad, entity, sampleTenant, sampleUser, map[string]any{"name": "ghost"}, nil); err == nil {
		t.Fatal("InsertRecord with an over-long mark source succeeded; want an error")
	}
	if n := rowCount(t, repo, entity); n != 2 {
		t.Fatalf("obj rows = %d, want 2 (the failed mark must roll the row back)", n)
	}
}

func TestSampleMark_InsertRecordsAtomic_SameTx(t *testing.T) {
	repo := New(testDB(t))
	const entity = "it_sample_bulk"
	sampleTable(t, repo, entity)

	rows := []map[string]any{{"name": "A"}, {"name": "B"}, {"name": "C"}}
	mctx := WithSampleMark(context.Background(), SampleMark{BatchID: "b1", Source: "generator"})
	ids, failed, err := repo.InsertRecordsAtomic(mctx, entity, sampleTenant, sampleUser, rows, nil)
	if err != nil || failed != -1 || len(ids) != 3 {
		t.Fatalf("InsertRecordsAtomic = %v,%d,%v", ids, failed, err)
	}
	if n := markCount(t, repo, sampleTenant, entity); n != 3 {
		t.Fatalf("marks = %d, want 3", n)
	}

	// No ctx mark -> none.
	if _, _, err := repo.InsertRecordsAtomic(context.Background(), entity, sampleTenant, sampleUser, rows, nil); err != nil {
		t.Fatalf("plain bulk: %v", err)
	}
	if n := markCount(t, repo, sampleTenant, entity); n != 3 {
		t.Fatalf("marks = %d after plain bulk, want still 3", n)
	}

	// Failing mark on row 0 -> whole batch rolled back.
	bad := WithSampleMark(context.Background(), SampleMark{BatchID: "b2", Source: strings.Repeat("x", 17)})
	if _, failed, err := repo.InsertRecordsAtomic(bad, entity, sampleTenant, sampleUser, rows, nil); err == nil || failed != 0 {
		t.Fatalf("bad mark: failed=%d err=%v; want row 0 + error", failed, err)
	}
	if n := rowCount(t, repo, entity); n != 6 {
		t.Fatalf("obj rows = %d, want 6", n)
	}
}

// T085: the scheduler's list leaves marked rows out of BOTH the page and the total.
func TestSampleMark_ListExcludeSamples(t *testing.T) {
	repo := New(testDB(t))
	const entity = "it_sample_list"
	sampleTable(t, repo, entity)
	ctx := context.Background()

	mctx := WithSampleMark(ctx, SampleMark{BatchID: "b1", Source: "ai"})
	for _, n := range []string{"s1", "s2"} {
		if _, err := repo.InsertRecord(mctx, entity, sampleTenant, sampleUser, map[string]any{"name": n}, nil); err != nil {
			t.Fatal(err)
		}
	}
	realID, err := repo.InsertRecord(ctx, entity, sampleTenant, sampleUser, map[string]any{"name": "real"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	recs, total, err := repo.ListRecords(ctx, entity, sampleTenant, AdminScope(), ListParams{ExcludeSamples: true})
	if err != nil || total != 1 || len(recs) != 1 || recs[0].ID != realID {
		t.Fatalf("ExcludeSamples: total=%d len=%d err=%v; want only the real row", total, len(recs), err)
	}
	if _, total, err = repo.ListRecords(ctx, entity, sampleTenant, AdminScope(), ListParams{}); err != nil || total != 3 {
		t.Fatalf("default list: total=%d err=%v; want 3 (samples still listed)", total, err)
	}
	// A mark owned by ANOTHER tenant for the same entity/record id must not hide this tenant's row.
	if _, err := repo.db.Exec("INSERT INTO sys_sample_records (id, tenant_id, entity_name, record_id, batch_id, source, created_by) VALUES (UUID(), ?, ?, ?, 'x', 'ai', ?)",
		sampleTenant2, entity, realID, sampleUser); err != nil {
		t.Fatal(err)
	}
	if _, total, err = repo.ListRecords(ctx, entity, sampleTenant, AdminScope(), ListParams{ExcludeSamples: true}); err != nil || total != 1 {
		t.Fatalf("cross-tenant mark leaked: total=%d err=%v; want 1", total, err)
	}
}

// Summary counts: JOIN on the live table, live vs bin, grouped by source; orphan marks ignored.
func TestSampleMark_CountSamples(t *testing.T) {
	repo := New(testDB(t))
	const entity = "it_sample_count"
	sampleTable(t, repo, entity)
	ctx := context.Background()

	ai := WithSampleMark(ctx, SampleMark{BatchID: "b1", Source: "ai"})
	gen := WithSampleMark(ctx, SampleMark{BatchID: "b2", Source: "generator"})
	var aiIDs []string
	for _, n := range []string{"a", "b", "c"} {
		id, err := repo.InsertRecord(ai, entity, sampleTenant, sampleUser, map[string]any{"name": n}, nil)
		if err != nil {
			t.Fatal(err)
		}
		aiIDs = append(aiIDs, id)
	}
	if _, err := repo.InsertRecord(gen, entity, sampleTenant, sampleUser, map[string]any{"name": "g"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.SoftDeleteRecord(ctx, entity, sampleTenant, aiIDs[0], AdminScope()); err != nil {
		t.Fatal(err)
	}
	// Orphan mark (row hard-deleted elsewhere): record id exists nowhere.
	if _, err := repo.db.Exec("INSERT INTO sys_sample_records (id, tenant_id, entity_name, record_id, batch_id, source, created_by) VALUES (UUID(), ?, ?, UUID(), 'z', 'ai', ?)",
		sampleTenant, entity, sampleUser); err != nil {
		t.Fatal(err)
	}

	got, err := repo.CountSampleRecords(ctx, sampleTenant, entity)
	if err != nil {
		t.Fatalf("CountSampleRecords: %v", err)
	}
	if got.Live != 3 || got.InBin != 1 || got.BySource["ai"] != 3 || got.BySource["generator"] != 1 {
		t.Fatalf("counts = %+v; want live 3, inBin 1, ai 3 (live+bin), generator 1 (orphan not counted)", got)
	}
	if other, err := repo.CountSampleRecords(ctx, sampleTenant2, entity); err != nil || other.Live != 0 || other.InBin != 0 {
		t.Fatalf("tenant2 counts = %+v err=%v; want zero", other, err)
	}
	names, err := repo.SampleEntityNames(ctx, sampleTenant)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		found = found || n == entity
	}
	if !found {
		t.Fatalf("SampleEntityNames = %v; want it to include %s", names, entity)
	}
}
