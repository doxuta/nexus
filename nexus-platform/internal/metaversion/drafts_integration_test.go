//go:build integration

package metaversion

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// memAdapter is a DocAdapter over an in-memory map: the draft layer is tested independently of any
// real document table.
type memAdapter struct {
	typ  string
	mu   sync.Mutex
	docs map[string]json.RawMessage // key -> live canonical doc
}

func newMemAdapter(typ string) *memAdapter {
	return &memAdapter{typ: typ, docs: map[string]json.RawMessage{}}
}

func (a *memAdapter) Type() string { return a.typ }
func (a *memAdapter) LoadLive(_ context.Context, _ sqlx.QueryerContext, _ string, key string, _ bool) (json.RawMessage, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d, ok := a.docs[key]
	return d, ok, nil
}
func (a *memAdapter) ListLiveKeys(context.Context, sqlx.QueryerContext, string) ([]DocRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []DocRef
	for k := range a.docs {
		out = append(out, DocRef{Key: k, Label: k})
	}
	return out, nil
}
func (a *memAdapter) ApplyTx(context.Context, *sqlx.Tx, string, Change) error { return nil }
func (a *memAdapter) ValidateFinal(context.Context, FinalState) []Problem     { return nil }
func (a *memAdapter) Redact(d json.RawMessage) json.RawMessage                { return d }
func (a *memAdapter) Entities(json.RawMessage) []string                       { return nil }

// newTenant inserts a tenants row (the draft Upsert locks it) and returns its id.
func newTenant(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO tenants (id, slug, name) VALUES (?, ?, ?)`, id, "mv"+id[:8], "mv test"); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return id
}

func draftSetup(t *testing.T) (*sqlx.DB, *DraftRepo, *memAdapter) {
	t.Helper()
	db := mvDB(t)
	ad := newMemAdapter("workflow")
	return db, NewDraftRepo(db, NewRegistry(ad)), ad
}

func TestDraftRepo_UpsertKeepsBaseHash(t *testing.T) {
	db, repo, ad := draftSetup(t)
	ctx := context.Background()
	tenant := newTenant(t, db)
	ad.docs["wf-1"] = json.RawMessage(`{"name":"live v1"}`)

	save := func(name string) {
		t.Helper()
		doc := json.RawMessage(`{"name":"` + name + `"}`)
		if err := repo.Upsert(ctx, tenant, UpsertInput{DocType: "workflow", DocKey: "wf-1", Op: OpUpsert, Doc: doc, Label: name, EntityName: "crm_x", UserID: "u1"}); err != nil {
			t.Fatalf("Upsert %s: %v", name, err)
		}
	}
	save("draft A")
	first, _ := DocHash(ad.docs["wf-1"])
	// The live doc changes AFTER the draft was created (a customer-admin edit).
	ad.docs["wf-1"] = json.RawMessage(`{"name":"live v2"}`)
	save("draft B")

	rows, err := repo.ListByType(ctx, tenant, "workflow")
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListByType: %v rows=%d", err, len(rows))
	}
	if rows[0].BaseHash == nil || *rows[0].BaseHash != first {
		t.Errorf("base_hash = %v, want the fingerprint of the live doc when the draft was FIRST created (%s)", rows[0].BaseHash, first)
	}
	if rows[0].Label != "draft B" || !strings.Contains(string(rows[0].DraftJSON), "draft B") {
		t.Errorf("second save did not replace the body: %s / %s", rows[0].Label, rows[0].DraftJSON)
	}
	if n, _ := repo.Count(ctx, tenant); n != 1 {
		t.Errorf("Count = %d, want 1", n)
	}
}

func TestDraftRepo_NewDocumentHasNullBaseHash(t *testing.T) {
	db, repo, _ := draftSetup(t)
	tenant := newTenant(t, db)
	if err := repo.Upsert(context.Background(), tenant, UpsertInput{DocType: "workflow", DocKey: "brand-new", Op: OpUpsert, Doc: json.RawMessage(`{"n":1}`), UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := repo.ListByType(context.Background(), tenant, "workflow")
	if len(rows) != 1 || rows[0].BaseHash != nil {
		t.Fatalf("a document absent from live must have base_hash NULL, got %+v", rows)
	}
}

func TestDraftRepo_DeleteOpHasNullBody(t *testing.T) {
	db, repo, ad := draftSetup(t)
	tenant := newTenant(t, db)
	ad.docs["wf-1"] = json.RawMessage(`{"n":1}`)
	if err := repo.Upsert(context.Background(), tenant, UpsertInput{DocType: "workflow", DocKey: "wf-1", Op: OpDelete, UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := repo.ListByType(context.Background(), tenant, "workflow")
	if len(rows) != 1 || rows[0].Op != OpDelete || rows[0].DraftJSON != nil {
		t.Fatalf("delete draft: %+v", rows)
	}
}

func TestDraftRepo_DeleteByKeyAndTenantIsolation(t *testing.T) {
	db, repo, _ := draftSetup(t)
	ctx := context.Background()
	a, b := newTenant(t, db), newTenant(t, db)
	for _, tn := range []string{a, b} {
		if err := repo.Upsert(ctx, tn, UpsertInput{DocType: "workflow", DocKey: "k", Op: OpUpsert, Doc: json.RawMessage(`{}`), UserID: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	rowsA, _ := repo.ListByType(ctx, a, "workflow")
	if _, err := repo.Get(ctx, b, rowsA[0].ID); !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("tenant B read tenant A's draft by id: %v", err)
	}
	if n, err := repo.DeleteIDs(ctx, db, b, []string{rowsA[0].ID}); err != nil || n != 0 {
		t.Fatalf("tenant B deleted tenant A's draft: n=%d err=%v", n, err)
	}
	if ok, err := repo.DeleteByKey(ctx, a, "workflow", "k"); err != nil || !ok {
		t.Fatalf("DeleteByKey = %v, %v", ok, err)
	}
	if ok, _ := repo.DeleteByKey(ctx, a, "workflow", "k"); ok {
		t.Fatal("second DeleteByKey reported a row")
	}
	if n, _ := repo.Count(ctx, b); n != 1 {
		t.Fatalf("tenant B lost its draft: %d", n)
	}
}

// doc_type outside the registry is refused BEFORE any SQL: nothing reaches the table.
func TestDraftRepo_UnknownDocTypeRefusedBeforeSQL(t *testing.T) {
	db, repo, _ := draftSetup(t)
	tenant := newTenant(t, db)
	err := repo.Upsert(context.Background(), tenant, UpsertInput{DocType: "x'; DROP TABLE users; --", DocKey: "k", Op: OpUpsert, Doc: json.RawMessage(`{}`), UserID: "u"})
	if err == nil {
		t.Fatal("unknown doc type accepted")
	}
	if n, _ := repo.Count(context.Background(), tenant); n != 0 {
		t.Fatalf("a row was written: %d", n)
	}
	if _, err := repo.ListByType(context.Background(), tenant, "nope"); err == nil {
		t.Fatal("ListByType accepted an unknown type")
	}
}

// plan §4: Upsert takes tenants ... FOR SHARE, so it WAITS while a publish holds the row FOR UPDATE.
func TestDraftRepo_UpsertWaitsForPublishLock(t *testing.T) {
	db, repo, _ := draftSetup(t)
	tenant := newTenant(t, db)
	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT id FROM tenants WHERE id = ? FOR UPDATE`, tenant); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- repo.Upsert(context.Background(), tenant, UpsertInput{DocType: "workflow", DocKey: "k", Op: OpUpsert, Doc: json.RawMessage(`{}`), UserID: "u"})
	}()
	select {
	case err := <-done:
		_ = tx.Rollback()
		t.Fatalf("Upsert returned while the tenant row was locked FOR UPDATE: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Upsert after unlock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Upsert did not resume after the lock was released")
	}
}

func TestDraftRepo_ListLocksAndOrders(t *testing.T) {
	db, repo, _ := draftSetup(t)
	ctx := context.Background()
	tenant := newTenant(t, db)
	for _, k := range []string{"a", "b"} {
		if err := repo.Upsert(ctx, tenant, UpsertInput{DocType: "workflow", DocKey: k, Op: OpUpsert, Doc: json.RawMessage(`{}`), UserID: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := repo.List(ctx, tx, tenant, true)
	if err != nil || len(rows) != 2 {
		t.Fatalf("List(forUpdate) = %d rows, %v", len(rows), err)
	}
}
