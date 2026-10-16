package services

// Idempotency-Key on POST /:entity/bulk and /:entity/import (specs/047 FR-12, DAILY-QUEUE #174):
// the per-row outcome is stored at completion and replayed verbatim; nothing written ⇒ the key is
// released; written-then-failed ⇒ the key is kept (a resend must never write the rows twice).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/idempotency"
)

func bulkRows(titles ...string) []map[string]any {
	rows := make([]map[string]any, len(titles))
	for i, title := range titles {
		rows[i] = map[string]any{"title": title}
	}
	return rows
}

func TestCreateBulkIdempotent_ResendReplaysTheStoredResult(t *testing.T) {
	svc, store, idem := newIdemService()
	ctx := context.Background()
	first, replay, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", bulkRows("Go 101", "Go 102"), BulkModePartial, "b-1")
	if err != nil || replay != nil || first.Created != 2 {
		t.Fatalf("first = (created %d, replay %s, %v), want 2 created, no replay", first.Created, replay, err)
	}
	c, _ := idem.Get(ctx, "t1", "user-1", "b-1")
	if c == nil || c.Status != idempotency.StatusCompleted || len(c.Response) == 0 {
		t.Fatalf("claim after the batch = %+v, want completed with the stored response", c)
	}

	// The same batch decoded from differently ordered / spaced JSON is the same request.
	var resent struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal([]byte(`{"records":[ {"title" : "Go 101"}, {"title":"Go 102"} ]}`), &resent); err != nil {
		t.Fatal(err)
	}
	_, replay, err = svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", resent.Records, BulkModePartial, "b-1")
	if err != nil || replay == nil {
		t.Fatalf("resend = (replay %s, %v), want the stored response", replay, err)
	}
	var got BulkCreateResult
	if err := json.Unmarshal(replay, &got); err != nil {
		t.Fatalf("replay is not a bulk result: %v (%s)", err, replay)
	}
	if got.Created != 2 || len(got.Results) != 2 || got.Results[0].ID != first.Results[0].ID || got.Results[1].ID != first.Results[1].ID {
		t.Fatalf("replay = %+v, want the first result %+v", got, first)
	}
	if store.inserts() != 2 {
		t.Fatalf("inserts = %d after a resend, want 2", store.inserts())
	}

	for name, call := range map[string]func() error{
		"other rows": func() error {
			_, _, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", bulkRows("Go 101", "Go 103"), BulkModePartial, "b-1")
			return err
		},
		"other mode": func() error {
			_, _, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", bulkRows("Go 101", "Go 102"), BulkModeAtomic, "b-1")
			return err
		},
		"single create under the bulk key": func() error {
			_, _, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", map[string]any{"title": "Go 101"}, "b-1")
			return err
		},
	} {
		if err := call(); codeOf(err) != "IDEMPOTENCY_KEY_REUSED" {
			t.Errorf("%s: err = %v, want IDEMPOTENCY_KEY_REUSED", name, err)
		}
	}
	if store.inserts() != 2 {
		t.Fatalf("inserts = %d after refused resends, want 2", store.inserts())
	}
}

// Nothing written — an atomic abort, or a partial batch whose every row failed — releases the key,
// so the corrected batch runs under the same key.
func TestCreateBulkIdempotent_NothingWrittenReleasesTheKey(t *testing.T) {
	tests := []struct {
		name string
		mode BulkMode
		rows []map[string]any
	}{
		// No atomic inserter wired on the test service ⇒ BULK_ATOMIC_UNAVAILABLE abort, 0 created.
		{"atomic abort", BulkModeAtomic, bulkRows("Go 101")},
		{"partial, every row refused", BulkModePartial, []map[string]any{{"title": "x", "hacker": 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, store, idem := newIdemService()
			ctx := context.Background()
			res, replay, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", tt.rows, tt.mode, "b-1")
			if err != nil || replay != nil || res.Created != 0 {
				t.Fatalf("= (created %d, replay %s, %v), want a no-op result", res.Created, replay, err)
			}
			if _, gerr := idem.Get(ctx, "t1", "user-1", "b-1"); !errors.Is(gerr, idempotency.ErrNotFound) {
				t.Fatalf("claim kept after a batch that wrote nothing (%v) — the corrected batch would be refused", gerr)
			}
			if store.inserts() != 0 {
				t.Fatalf("inserts = %d, want 0", store.inserts())
			}
		})
	}
}

// A partial batch that breaks mid-way on an infrastructure error has ALREADY committed its earlier
// rows. Releasing the key would let the resend write them again: it is kept (409 until it expires).
func TestCreateBulkIdempotent_FailureAfterWritingKeepsTheKey(t *testing.T) {
	svc, store, idem := newIdemService()
	ctx := context.Background()
	calls := 0
	store.insertHook = func() {
		calls++
		if calls == 2 {
			store.insertErr = errors.New("connection reset")
		}
	}
	_, _, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", bulkRows("Go 101", "Go 102"), BulkModePartial, "b-1")
	if err == nil {
		t.Fatal("a batch whose second row hit a DB error succeeded")
	}
	c, gerr := idem.Get(ctx, "t1", "user-1", "b-1")
	if gerr != nil || c.Status != idempotency.StatusProcessing {
		t.Fatalf("claim after a half-written batch = (%+v, %v), want it kept processing", c, gerr)
	}
	store.insertHook, store.insertErr = nil, nil
	if _, _, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", bulkRows("Go 101", "Go 102"), BulkModePartial, "b-1"); codeOf(err) != "IDEMPOTENCY_IN_PROGRESS" {
		t.Fatalf("resend = %v, want IDEMPOTENCY_IN_PROGRESS", err)
	}
	if store.inserts() != 1 {
		t.Fatalf("inserts = %d, want only the first row", store.inserts())
	}
}

// The client hanging up mid-batch is the case the key exists for. Each partial row commits on its
// own, so a batch cut at row 2 by the cancelled request context would keep the key processing for
// 24 h (409 on every resend, no way to learn which rows exist). The batch runs detached from the
// request instead: it finishes, its response is stored, and the resend replays it.
func TestCreateBulkIdempotent_ClientHangingUpMidBatchStillCompletes(t *testing.T) {
	svc, store, idem := newIdemService()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	store.insertHook = func() {
		calls++
		if calls == 1 {
			cancel() // the connection drops while row 1 is being written
		}
	}
	rows := bulkRows("Go 101", "Go 102", "Go 103")
	first, _, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", rows, BulkModePartial, "b-1")
	if err != nil || first.Created != 3 {
		t.Fatalf("first = (created %d, %v), want the whole batch written although the client hung up", first.Created, err)
	}
	store.insertHook = nil
	_, replay, err := svc.CreateBulkIdempotent(context.Background(), idemScope, "t1", "user-1", "courses", bulkRows("Go 101", "Go 102", "Go 103"), BulkModePartial, "b-1")
	if err != nil || replay == nil {
		t.Fatalf("resend = (replay %s, %v), want the stored response", replay, err)
	}
	var got BulkCreateResult
	if err := json.Unmarshal(replay, &got); err != nil || got.Created != 3 {
		t.Fatalf("replay = %s (%v), want created 3", replay, err)
	}
	if c, _ := idem.Get(context.Background(), "t1", "user-1", "b-1"); c == nil || c.Status != idempotency.StatusCompleted {
		t.Fatalf("claim = %+v, want completed", c)
	}
	if store.inserts() != 3 {
		t.Fatalf("inserts = %d, want 3", store.inserts())
	}
}

func TestCreateBulkIdempotent_FirstStillRunningIs409(t *testing.T) {
	svc, store, idem := newIdemService()
	rows := bulkRows("Go 101")
	_ = idem.Claim(context.Background(), idempotency.Claim{TenantID: "t1", PrincipalID: "user-1", Key: "b-1", EntityName: "courses",
		RequestHash: idempotencyRequestHash("bulk\x00"+string(BulkModePartial)+"\x00courses", map[string]any{"records": rows}),
		ExpiresAt:   time.Now().Add(time.Hour)})
	if _, _, err := svc.CreateBulkIdempotent(context.Background(), idemScope, "t1", "user-1", "courses", rows, BulkModePartial, "b-1"); codeOf(err) != "IDEMPOTENCY_IN_PROGRESS" {
		t.Fatalf("err = %v, want IDEMPOTENCY_IN_PROGRESS", err)
	}
	if store.inserts() != 0 {
		t.Fatalf("inserts = %d, want 0", store.inserts())
	}
}

func newIdemImportService() (*ImportService, *fakeImportStore, *fakeIdemStore) {
	svc, store := newImportSvcForTest()
	idem := newFakeIdemStore()
	svc.WithIdempotencyStore(idem)
	return svc, store, idem
}

func importIn(csv string) ImportInput {
	return ImportInput{TenantID: "t1", UserID: "u1", RoleID: "r1", EntityName: "contacts",
		Mapping: map[string]string{"col_name": "name"}, Reader: strings.NewReader(csv), FileSize: int64(len(csv))}
}

func TestImportIdempotent_ResendReplaysTheStoredSummary(t *testing.T) {
	svc, store, idem := newIdemImportService()
	ctx := context.Background()
	first, replay, err := svc.ImportIdempotent(ctx, importIn("col_name\nAlice\nBob\n"), "i-1")
	if err != nil || replay != nil || first == nil || first.Inserted != 2 {
		t.Fatalf("first = (%+v, replay %s, %v), want 2 inserted", first, replay, err)
	}
	store.insertCalled = false
	_, replay, err = svc.ImportIdempotent(ctx, importIn("col_name\nAlice\nBob\n"), "i-1")
	if err != nil || replay == nil {
		t.Fatalf("resend = (replay %s, %v), want the stored summary", replay, err)
	}
	var got ImportResult
	if err := json.Unmarshal(replay, &got); err != nil || got.Inserted != 2 || got.Total != 2 {
		t.Fatalf("replay = %s (%v), want the first summary", replay, err)
	}
	if store.insertCalled {
		t.Fatal("the resend inserted the rows again")
	}
	// Another file under the same key is another request.
	if _, _, err := svc.ImportIdempotent(ctx, importIn("col_name\nAlice\nCarol\n"), "i-1"); codeOf(err) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("other file = %v, want IDEMPOTENCY_KEY_REUSED", err)
	}
	if c, _ := idem.Get(ctx, "t1", "u1", "i-1"); c == nil || c.Status != idempotency.StatusCompleted {
		t.Fatalf("claim = %+v, want completed", c)
	}
}

func TestImportIdempotent_NothingInsertedReleasesTheKey(t *testing.T) {
	svc, _, idem := newIdemImportService()
	ctx := context.Background()
	res, replay, err := svc.ImportIdempotent(ctx, importIn("col_name\n\"\"\n"), "i-2") // required name empty
	if err != nil || replay != nil || res == nil || res.Inserted != 0 {
		t.Fatalf("= (%+v, replay %s, %v), want an import that inserted nothing", res, replay, err)
	}
	if _, gerr := idem.Get(ctx, "t1", "u1", "i-2"); !errors.Is(gerr, idempotency.ErrNotFound) {
		t.Fatalf("claim kept after an import that inserted nothing (%v)", gerr)
	}
}

func TestImportIdempotent_UnknownCommitOutcomeKeepsTheKey(t *testing.T) {
	svc, store, idem := newIdemImportService()
	ctx := context.Background()
	store.insertErr = fmt.Errorf("dynrepo.InsertRecords: commit: %w: %w", dynrepo.ErrCommitOutcomeUnknown, errors.New("invalid connection"))
	if _, _, err := svc.ImportIdempotent(ctx, importIn("col_name\nAlice\n"), "i-3"); !errors.Is(err, dynrepo.ErrCommitOutcomeUnknown) {
		t.Fatalf("err = %v, want the unknown-commit error", err)
	}
	if c, gerr := idem.Get(ctx, "t1", "u1", "i-3"); gerr != nil || c.Status != idempotency.StatusProcessing {
		t.Fatalf("claim after an unknown commit = (%+v, %v), want it kept processing", c, gerr)
	}
}

// specs/061 FR-010 × specs/047 FR-12: a preview writes nothing, so its Idempotency-Key protects
// nothing — it neither replays the import the key already names (that would show "inserted" for a
// preview) nor holds the key.
func TestImportIdempotent_PreviewNeverReplaysNorClaims(t *testing.T) {
	svc, store, idem := newIdemImportService()
	ctx := context.Background()
	if _, _, err := svc.ImportIdempotent(ctx, importIn("col_name\nAlice\nBob\n"), "i-4"); err != nil {
		t.Fatalf("first import: %v", err)
	}
	store.insertCalled = false
	preview := importIn("col_name\nAlice\nBob\n")
	preview.DryRun = true
	res, replay, err := svc.ImportIdempotent(ctx, preview, "i-4")
	if err != nil || replay != nil || res == nil || !res.DryRun || res.Valid != 2 || res.Inserted != 0 {
		t.Fatalf("preview = (%+v, replay %s, %v); want a fresh dry run, not the stored summary", res, replay, err)
	}
	if store.insertCalled {
		t.Fatal("the preview inserted rows")
	}
	fresh := importIn("col_name\nCarol\n")
	fresh.DryRun = true
	if _, _, err := svc.ImportIdempotent(ctx, fresh, "i-5"); err != nil {
		t.Fatalf("preview under a fresh key: %v", err)
	}
	if _, gerr := idem.Get(ctx, "t1", "u1", "i-5"); !errors.Is(gerr, idempotency.ErrNotFound) {
		t.Fatalf("a preview claimed its key (%v)", gerr)
	}
}

// specs/061 FR-012: "Khớp theo" changes which records a file links to, so a resend under the same key
// with another matchBy is another request (422), never a replay of the first.
func TestImportIdempotent_MatchByIsPartOfTheRequest(t *testing.T) {
	svc, _, _ := newIdemImportService()
	ctx := context.Background()
	if _, _, err := svc.ImportIdempotent(ctx, importIn("col_name\nAlice\n"), "i-6"); err != nil {
		t.Fatalf("first import: %v", err)
	}
	other := importIn("col_name\nAlice\n")
	other.MatchBy = map[string]string{"account": "code"}
	if _, replay, err := svc.ImportIdempotent(ctx, other, "i-6"); codeOf(err) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("other matchBy = (replay %s, %v); want IDEMPOTENCY_KEY_REUSED", replay, err)
	}
}
