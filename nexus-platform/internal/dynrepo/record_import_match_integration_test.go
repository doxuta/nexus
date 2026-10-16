//go:build integration

package dynrepo

import (
	"context"
	"sort"
	"testing"

	"nexus/platform/internal/ddl"
)

// specs/061 FR-014: a CSV relation cell names its target by a field value ("Phú Gia Riverside"),
// matched case- and space-insensitively, within ONE tenant, live records only. Two records with the
// same name both come back so the caller can refuse the cell as ambiguous.
func TestBatchIDsByFieldValue(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_match_accounts"
	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "name", FieldType: "text"}, {Name: "code", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	insert := func(tenant, name string) string {
		t.Helper()
		id, err := repo.InsertRecord(ctx, entity, tenant, "u1", map[string]any{"name": name}, nil)
		if err != nil {
			t.Fatalf("InsertRecord(%s): %v", name, err)
		}
		return id
	}
	lanh := insert("t1", "Công ty TNHH Lạnh Việt 1")
	phuGia := insert("t1", " Phú Gia Riverside ")
	dupA := insert("t1", "Trùng Tên")
	dupB := insert("t1", "trùng tên")
	gone := insert("t1", "Đã Xoá")
	if err := repo.SoftDeleteRecord(ctx, entity, "t1", gone, RowScope{Level: AccessAll}); err != nil {
		t.Fatalf("SoftDeleteRecord: %v", err)
	}
	_ = insert("t2", "Phú Gia Riverside") // another tenant's record must never match

	got, err := repo.BatchIDsByFieldValue(ctx, entity, "t1", "name",
		[]string{"công ty tnhh lạnh việt 1", "phú gia riverside", "trùng tên", "đã xoá", "không có"}, AdminScope())
	if err != nil {
		t.Fatalf("BatchIDsByFieldValue: %v", err)
	}
	want := map[string][]string{
		"công ty tnhh lạnh việt 1": {lanh},
		"phú gia riverside":        {phuGia},
		"trùng tên":                sorted(dupA, dupB),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for k, ids := range want {
		sort.Strings(got[k])
		if len(got[k]) != len(ids) || (len(ids) > 0 && got[k][0] != ids[0]) || (len(ids) > 1 && got[k][1] != ids[1]) {
			t.Fatalf("%q → %v; want %v", k, got[k], ids)
		}
	}

	// The caller's row scope narrows the lookup (review MEDIUM): with view=owner a colleague's
	// record, named by a guessable key, is not found — and a name shared with a colleague's record
	// is not ambiguous either, only the caller's own record comes back.
	mine, err := repo.InsertRecord(ctx, entity, "t1", "u2", map[string]any{"name": "Trùng Tên"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord(u2): %v", err)
	}
	owned, err := repo.BatchIDsByFieldValue(ctx, entity, "t1", "name",
		[]string{"phú gia riverside", "trùng tên"}, RowScope{UserID: "u2", Level: AccessOwner})
	if err != nil {
		t.Fatalf("BatchIDsByFieldValue(owner): %v", err)
	}
	if len(owned) != 1 || len(owned["trùng tên"]) != 1 || owned["trùng tên"][0] != mine {
		t.Fatalf("owner scope → %v; want only %s under \"trùng tên\"", owned, mine)
	}
	if none, err := repo.BatchIDsByFieldValue(ctx, entity, "t1", "name", []string{"trùng tên"}, RowScope{UserID: "u2", Level: AccessNone}); err != nil || len(none) != 0 {
		t.Fatalf("none scope → %v, %v; want nothing", none, err)
	}

	// A field that is not an identifier never reaches SQL.
	if _, err := repo.BatchIDsByFieldValue(ctx, entity, "t1", "name') OR 1=1 -- ", []string{"x"}, AdminScope()); err == nil {
		t.Fatal("a non-identifier field was accepted")
	}
}

func sorted(s ...string) []string {
	sort.Strings(s)
	return s
}
