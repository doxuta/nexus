//go:build integration

package metaversion

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// specs/063 S7-6 T099: the history reads are pinned to the tenant BEFORE the id is used; a foreign id
// is ErrVersionNotFound; the list is newest first and paged; a snapshot row carries no bodies.
func TestVersionRepo_HistoryReads_TenantPinned(t *testing.T) {
	db := mvDB(t)
	repo := NewVersionRepo(db)
	ctx := context.Background()
	t1, t2 := newTenant(t, db), newTenant(t, db)
	v1 := insertVersion(t, db, repo, t1, uptr(1), KindBaseline, nil)
	v2 := insertVersion(t, db, repo, t1, uptr(2), KindPublish, nil)
	other := insertVersion(t, db, repo, t2, uptr(1), KindBaseline, nil)
	for _, d := range []VersionDoc{
		{TenantID: t1, VersionID: v1.ID, Seq: v1.Seq, DocType: "workflow", DocKey: "a", Op: "snapshot", AfterJSON: json.RawMessage(`{"name":"big"}`), CreatedByID: "u"},
		{TenantID: t1, VersionID: v2.ID, Seq: v2.Seq, DocType: "workflow", DocKey: "a", Op: "update", BeforeJSON: json.RawMessage(`{"name":"a"}`), AfterJSON: json.RawMessage(`{"name":"b"}`), CreatedByID: "u"},
	} {
		if err := repo.InsertDoc(ctx, db, d); err != nil {
			t.Fatal(err)
		}
	}

	list, total, err := repo.ListVersions(ctx, t1, 1, 0)
	if err != nil || total != 2 || len(list) != 1 || list[0].ID != v2.ID {
		t.Fatalf("page 1 = %+v total=%d err=%v, want newest (v2) of 2", list, total, err)
	}
	if list, _, _ = repo.ListVersions(ctx, t1, 1, 1); len(list) != 1 || list[0].ID != v1.ID {
		t.Fatalf("page 2 = %+v, want v1", list)
	}
	if _, err := repo.GetVersion(ctx, t1, other.ID); err != ErrVersionNotFound {
		t.Fatalf("another tenant's id: %v, want ErrVersionNotFound", err)
	}
	if docs, err := repo.ListDocs(ctx, t1, other.ID); err != nil || len(docs) != 0 {
		t.Fatalf("another tenant's docs: %v %v", docs, err)
	}
	snap, _ := repo.ListDocs(ctx, t1, v1.ID)
	if len(snap) != 1 || snap[0].AfterJSON != nil || snap[0].ApplyStatus != "applied" {
		t.Fatalf("snapshot docs = %+v, want no body and applied", snap)
	}
	upd, _ := repo.ListDocs(ctx, t1, v2.ID)
	if len(upd) != 1 || !strings.Contains(string(upd[0].AfterJSON), `"b"`) {
		t.Fatalf("update docs = %+v", upd)
	}
}
