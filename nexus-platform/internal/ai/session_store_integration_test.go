//go:build integration

// Integration tests for the E9 M3 AI-Builder session store. They prove what a fake cannot: the
// SQL itself scopes every read/write to (tenant_id, user_id) so a session is invisible across users
// AND across tenants (spec §11 §7 — a guessed id resolves to not-found, never a cross-owner leak),
// and that an AppendTurn (create+continue)→list→get→delete round-trip persists the raw conversation
// + draft_json atomically (post-review FIX 1: AppendTurn is the only write path into a session).
//
// Run with (throwaway DB migrated per the gate recipe — single-source TEST_DATABASE_URL, B7):
//
//	mysql -h127.0.0.1 -uroot -e "CREATE DATABASE IF NOT EXISTS nexus_it"
//	/opt/homebrew/bin/migrate -path migrations -database "mysql://root@tcp(127.0.0.1:3306)/nexus_it" up
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_it?parseTime=true&multiStatements=true&charset=utf8mb4" \
//	  go test -tags=integration ./internal/ai/ -run TestSessionStore -count=1
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"
)

// Fixed CHAR(36) UUID-shaped ids scoped to this test (cleaned up per run).
const (
	itTenantA = "a1a1a1a1-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	itTenantB = "b1b1b1b1-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	itUser1   = "11111111-1111-1111-1111-111111111111"
	itUser2   = "22222222-2222-2222-2222-222222222222"
)

func newSessionStoreForTest(t *testing.T) *SessionStore {
	t.Helper()
	db := connectAI(t) // reuses the helper + DSN from runs_integration_test.go
	// Isolate this test's rows across the fixed tenant ids (idempotent re-runs).
	for _, ten := range []string{itTenantA, itTenantB} {
		db.Exec(`DELETE FROM sys_ai_messages WHERE tenant_id = ?`, ten)
		db.Exec(`DELETE FROM sys_ai_sessions WHERE tenant_id = ?`, ten)
	}
	t.Cleanup(func() {
		for _, ten := range []string{itTenantA, itTenantB} {
			db.Exec(`DELETE FROM sys_ai_messages WHERE tenant_id = ?`, ten)
			db.Exec(`DELETE FROM sys_ai_sessions WHERE tenant_id = ?`, ten)
		}
	})
	return NewSessionStore(db)
}

// TestSessionStore_RoundTrip walks the full lifecycle one builder sees: AppendTurn creates the
// session AND its first turn atomically → a second AppendTurn continues the SAME session (proving
// baseSeq is computed fresh inside the tx, no stale-seq race) → list → get (raw content + draft_json
// round-trip) → soft-delete → excluded from list.
func TestSessionStore_RoundTrip(t *testing.T) {
	store := newSessionStoreForTest(t)
	ctx := context.Background()

	draft := json.RawMessage(`[{"kind":"entity","entity":{"name":"patients","label":"Bệnh nhân","fields":[]}}]`)

	// Turn 1: sessionID == "" → AppendTurn creates the session AND persists both messages atomically.
	id, err := store.AppendTurn(ctx, itTenantA, itUser1, "",
		AutoTitle("Tôi làm phòng khám nha khoa cần quản lý bệnh nhân"),
		"Tôi cần quản lý bệnh nhân", "Tôi đề xuất entity patients.", draft)
	if err != nil {
		t.Fatalf("AppendTurn (create): %v", err)
	}

	// List: the session shows for its owner, newest first.
	list, err := store.ListSessions(ctx, itTenantA, itUser1)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("ListSessions = %+v; want exactly the created session", list)
	}

	// Get: raw content + draft_json round-trip, in seq order.
	sess, msgs, err := store.GetSession(ctx, itTenantA, itUser1, id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.UserID != itUser1 || sess.TenantID != itTenantA {
		t.Fatalf("session owner = (%s,%s); want (%s,%s)", sess.TenantID, sess.UserID, itTenantA, itUser1)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("messages = %+v; want [user, assistant] in seq order", msgs)
	}
	if msgs[0].Seq != 0 || msgs[1].Seq != 1 {
		t.Fatalf("seqs = (%d,%d); want (0,1) for the first turn", msgs[0].Seq, msgs[1].Seq)
	}
	if msgs[0].Content != "Tôi cần quản lý bệnh nhân" {
		t.Fatalf("user content = %q; raw conversation must round-trip", msgs[0].Content)
	}
	if len(msgs[0].DraftJSON) != 0 {
		t.Fatalf("user draft_json = %s; want NULL (no draft on a user turn)", msgs[0].DraftJSON)
	}
	var gotDraft []DraftItem
	if err := json.Unmarshal(msgs[1].DraftJSON, &gotDraft); err != nil {
		t.Fatalf("assistant draft_json did not round-trip as draft[]: %v (%s)", err, msgs[1].DraftJSON)
	}
	if len(gotDraft) != 1 || gotDraft[0].Kind != "entity" || gotDraft[0].Entity == nil || gotDraft[0].Entity.Name != "patients" {
		t.Fatalf("draft = %+v; want one entity draft 'patients'", gotDraft)
	}

	// Turn 2: sessionID != "" → AppendTurn computes baseSeq from MAX(seq)+1 INSIDE its own tx, so it
	// can never race a concurrent append into a stale seq. newTitle is ignored (session already named).
	if _, err := store.AppendTurn(ctx, itTenantA, itUser1, id, "ignored-title", "câu hỏi tiếp theo", "trả lời tiếp theo", nil); err != nil {
		t.Fatalf("AppendTurn (continue): %v", err)
	}
	_, msgs2, err := store.GetSession(ctx, itTenantA, itUser1, id)
	if err != nil {
		t.Fatalf("GetSession after 2nd turn: %v", err)
	}
	if len(msgs2) != 4 || msgs2[2].Seq != 2 || msgs2[3].Seq != 3 {
		t.Fatalf("messages after 2nd turn = %+v; want 4 with the new turn at seq 2,3", msgs2)
	}

	// Soft-delete → excluded from the list, and a subsequent get is not-found.
	if err := store.SoftDeleteSession(ctx, itTenantA, itUser1, id); err != nil {
		t.Fatalf("SoftDeleteSession: %v", err)
	}
	after, err := store.ListSessions(ctx, itTenantA, itUser1)
	if err != nil {
		t.Fatalf("ListSessions after delete: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("ListSessions after delete = %+v; want empty (soft-deleted excluded)", after)
	}
	if _, _, err := store.GetSession(ctx, itTenantA, itUser1, id); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetSession after delete = %v; want ErrSessionNotFound", err)
	}
}

// TestSessionStore_CrossUserIsolation proves user1's session is invisible to user2 in the SAME
// tenant. The PRIMARY proof is the RETURN VALUE of each call (a message-count readback is a weaker
// signal — a foreign write would be invisible to a scoped readback regardless of whether the guard
// ever ran); the readback at the end is kept as defense-in-depth confirmation.
func TestSessionStore_CrossUserIsolation(t *testing.T) {
	store := newSessionStoreForTest(t)
	ctx := context.Background()

	id, err := store.AppendTurn(ctx, itTenantA, itUser1, "", "user1 session", "hi", "hello", nil)
	if err != nil {
		t.Fatalf("AppendTurn (create): %v", err)
	}

	// user2 (same tenant) must not see, write to, or delete user1's session — proven by the return
	// value of each call.
	if list, err := store.ListSessions(ctx, itTenantA, itUser2); err != nil || len(list) != 0 {
		t.Fatalf("user2 ListSessions = (%+v, %v); want empty", list, err)
	}
	if _, _, err := store.GetSession(ctx, itTenantA, itUser2, id); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("user2 GetSession = %v; want ErrSessionNotFound (no cross-user read)", err)
	}
	if _, err := store.AppendTurn(ctx, itTenantA, itUser2, id, "ignored", "attempted hijack", "should not persist", nil); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("user2 AppendTurn = %v; want ErrSessionNotFound (no cross-user write — USER-scoped guard)", err)
	}
	if err := store.SoftDeleteSession(ctx, itTenantA, itUser2, id); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("user2 SoftDeleteSession = %v; want ErrSessionNotFound (no cross-user delete)", err)
	}

	// Defense-in-depth confirmation: the owner's session still has exactly the original 2 messages —
	// user2's rejected AppendTurn did not land.
	_, msgs, err := store.GetSession(ctx, itTenantA, itUser1, id)
	if err != nil {
		t.Fatalf("owner GetSession after cross-user attempts = %v; want intact", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("owner session has %d messages; want exactly the original 2 (user2's AppendTurn must not have written)", len(msgs))
	}
}

// TestSessionStore_CrossTenantIsolation proves the same guard holds across tenants: even the SAME
// user id in tenant B cannot read/append/delete tenant A's session. The PRIMARY proof is again the
// RETURN VALUE of each call; the readback is defense-in-depth confirmation.
func TestSessionStore_CrossTenantIsolation(t *testing.T) {
	store := newSessionStoreForTest(t)
	ctx := context.Background()

	id, err := store.AppendTurn(ctx, itTenantA, itUser1, "", "tenantA session", "hi", "hello", nil)
	if err != nil {
		t.Fatalf("AppendTurn (create): %v", err)
	}

	// Same user id, DIFFERENT tenant → invisible.
	if _, _, err := store.GetSession(ctx, itTenantB, itUser1, id); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-tenant GetSession = %v; want ErrSessionNotFound", err)
	}
	if _, err := store.AppendTurn(ctx, itTenantB, itUser1, id, "ignored", "x", "y", nil); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-tenant AppendTurn = %v; want ErrSessionNotFound (guard blocks the write)", err)
	}
	if err := store.SoftDeleteSession(ctx, itTenantB, itUser1, id); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-tenant SoftDeleteSession = %v; want ErrSessionNotFound", err)
	}

	// Defense-in-depth confirmation: the foreign AppendTurn must NOT have persisted anything beyond
	// the original turn.
	_, msgs, err := store.GetSession(ctx, itTenantA, itUser1, id)
	if err != nil {
		t.Fatalf("owner GetSession = %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("session has %d messages; a cross-tenant AppendTurn must write nothing beyond the original turn", len(msgs))
	}
}

// TestSessionStore_FeatureSplit (mig 000059 / F9-5) proves the SQL itself keeps the two AI
// surfaces apart: an assistant thread never shows in the Builder rail (the historical leak), the
// two surfaces cannot read/append/delete each other's threads (cross-feature id ≡ not-found, same
// no-leak contract as cross-user), and the assistant surface keeps the (tenant, user) ownership
// guard plus its newest-first LIMIT-20 list cap.
func TestSessionStore_FeatureSplit(t *testing.T) {
	store := newSessionStoreForTest(t)
	ctx := context.Background()

	bid, err := store.AppendTurn(ctx, itTenantA, itUser1, "", "builder thread", "thiết kế entity", "đề xuất entity", nil)
	if err != nil {
		t.Fatalf("AppendTurn (builder): %v", err)
	}
	draft := json.RawMessage(`[{"kind":"record_create","recordCreate":{"entity":"tickets","data":{"title":"x"}}}]`)
	aid, err := store.AppendAssistantTurn(ctx, itTenantA, itUser1, "", "⌘K thread", "tạo ticket cho khách A", "Đã soạn bản nháp.", draft)
	if err != nil {
		t.Fatalf("AppendAssistantTurn (create): %v", err)
	}

	// Rails are disjoint: the Builder list shows ONLY the builder thread (the F9-5 leak fix), the
	// assistant list ONLY the ⌘K thread.
	blist, err := store.ListSessions(ctx, itTenantA, itUser1)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(blist) != 1 || blist[0].ID != bid {
		t.Fatalf("builder list = %+v; want exactly the builder thread (no ⌘K leak)", blist)
	}
	alist, err := store.ListAssistantSessions(ctx, itTenantA, itUser1)
	if err != nil {
		t.Fatalf("ListAssistantSessions: %v", err)
	}
	if len(alist) != 1 || alist[0].ID != aid {
		t.Fatalf("assistant list = %+v; want exactly the ⌘K thread", alist)
	}

	// Cross-feature access is not-found BY RETURN VALUE — read, append and delete alike.
	if _, _, err := store.GetSession(ctx, itTenantA, itUser1, aid); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("builder GetSession(assistant id) = %v; want ErrSessionNotFound", err)
	}
	if _, _, err := store.GetAssistantSession(ctx, itTenantA, itUser1, bid); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetAssistantSession(builder id) = %v; want ErrSessionNotFound", err)
	}
	if _, err := store.AppendTurn(ctx, itTenantA, itUser1, aid, "ignored", "x", "y", nil); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("builder AppendTurn onto assistant thread = %v; want ErrSessionNotFound", err)
	}
	if _, err := store.AppendAssistantTurn(ctx, itTenantA, itUser1, bid, "ignored", "x", "y", nil); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("AppendAssistantTurn onto builder thread = %v; want ErrSessionNotFound", err)
	}
	if err := store.SoftDeleteSession(ctx, itTenantA, itUser1, aid); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("builder delete of assistant thread = %v; want ErrSessionNotFound", err)
	}

	// The assistant surface keeps the ownership guard: another user in the same tenant sees and
	// touches nothing.
	if list, err := store.ListAssistantSessions(ctx, itTenantA, itUser2); err != nil || len(list) != 0 {
		t.Fatalf("user2 assistant list = (%+v, %v); want empty", list, err)
	}
	if _, _, err := store.GetAssistantSession(ctx, itTenantA, itUser2, aid); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("user2 GetAssistantSession = %v; want ErrSessionNotFound", err)
	}
	if err := store.SoftDeleteAssistantSession(ctx, itTenantA, itUser2, aid); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("user2 SoftDeleteAssistantSession = %v; want ErrSessionNotFound", err)
	}

	// Resume round-trip on the assistant surface: raw content + draft_json intact.
	_, msgs, err := store.GetAssistantSession(ctx, itTenantA, itUser1, aid)
	if err != nil {
		t.Fatalf("GetAssistantSession: %v", err)
	}
	if len(msgs) != 2 || msgs[1].Role != "assistant" || len(msgs[1].DraftJSON) == 0 {
		t.Fatalf("assistant thread messages = %+v; want the turn with its draft_json", msgs)
	}

	// Owner delete works on the right surface; the builder thread is untouched.
	if err := store.SoftDeleteAssistantSession(ctx, itTenantA, itUser1, aid); err != nil {
		t.Fatalf("SoftDeleteAssistantSession: %v", err)
	}
	if after, _ := store.ListAssistantSessions(ctx, itTenantA, itUser1); len(after) != 0 {
		t.Fatalf("assistant list after delete = %+v; want empty", after)
	}
	if blist2, _ := store.ListSessions(ctx, itTenantA, itUser1); len(blist2) != 1 {
		t.Fatalf("builder list after assistant delete = %+v; must be untouched", blist2)
	}
}

// TestSessionStore_AssistantListCap: the ⌘K rail is newest-first and hard-capped at 20 — the
// palette shows recent threads, not an archive.
func TestSessionStore_AssistantListCap(t *testing.T) {
	store := newSessionStoreForTest(t)
	ctx := context.Background()

	for i := 0; i < assistantSessionListCap+3; i++ {
		if _, err := store.AppendAssistantTurn(ctx, itTenantA, itUser2, "", "thread", "q", "a", nil); err != nil {
			t.Fatalf("AppendAssistantTurn #%d: %v", i, err)
		}
	}
	list, err := store.ListAssistantSessions(ctx, itTenantA, itUser2)
	if err != nil {
		t.Fatalf("ListAssistantSessions: %v", err)
	}
	if len(list) != assistantSessionListCap {
		t.Fatalf("assistant list length = %d; want the %d cap", len(list), assistantSessionListCap)
	}
}

// Issue #42: SettleProposal marks one card of the caller's own builder thread, idempotently and
// without losing a concurrent mark on the same turn; any other target is ErrProposalNotFound with
// nothing written, and GetSession hands the marks back so PendingProposals can skip them.
func TestSessionStore_SettleProposal(t *testing.T) {
	store := newSessionStoreForTest(t)
	ctx := context.Background()
	draft := json.RawMessage(`[{"kind":"field","field":{"entity":"a","name":"f0"}},{"kind":"field","field":{"entity":"a","name":"f1"}},` +
		`{"kind":"field","field":{"entity":"a","name":"f2"}},{"kind":"field","field":{"entity":"a","name":"f3"}},{"kind":"field","field":{"entity":"a","name":"f4"}}]`)
	id, err := store.AppendTurn(ctx, itTenantA, itUser1, "", "t", "hỏi", "đề xuất", draft)
	if err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	settled := func() []int {
		t.Helper()
		_, msgs, err := store.GetSession(ctx, itTenantA, itUser1, id)
		if err != nil || len(msgs) != 2 {
			t.Fatalf("GetSession: %v (%d messages)", err, len(msgs))
		}
		if msgs[0].Settled != nil {
			t.Fatalf("the user turn carries settled marks %v", msgs[0].Settled)
		}
		return msgs[1].Settled
	}
	if got := settled(); got != nil {
		t.Fatalf("a fresh turn: settled = %v; want nil", got)
	}

	for _, tc := range []struct {
		name, tenant, user, session string
		seq, index                  int
	}{
		{"another builder's thread", itTenantA, itUser2, id, 1, 0},
		{"another tenant", itTenantB, itUser1, id, 1, 0},
		{"the user turn", itTenantA, itUser1, id, 0, 0},
		{"past the last card", itTenantA, itUser1, id, 1, 5},
		{"a negative index", itTenantA, itUser1, id, 1, -1},
		{"no such session", itTenantA, itUser1, "00000000-0000-0000-0000-000000000000", 1, 0},
	} {
		if err := store.SettleProposal(ctx, tc.tenant, tc.user, tc.session, tc.seq, tc.index); !errors.Is(err, ErrProposalNotFound) {
			t.Errorf("%s: %v; want ErrProposalNotFound", tc.name, err)
		}
	}
	if got := settled(); got != nil {
		t.Fatalf("refused settles wrote %v", got)
	}

	if err := store.SettleProposal(ctx, itTenantA, itUser1, id, 1, 3); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := store.SettleProposal(ctx, itTenantA, itUser1, id, 1, 3); err != nil {
		t.Fatalf("settle again: %v", err)
	}
	if got := settled(); !slices.Equal(got, []int{3}) {
		t.Fatalf("settled = %v; want [3] once", got)
	}

	// Apply-all settles the cards of one turn back to back; two tabs may race. Every mark lands once.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for _, i := range []int{0, 1, 2, 4, 0, 1, 2, 4} {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- store.SettleProposal(ctx, itTenantA, itUser1, id, 1, i)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent settle: %v", err)
		}
	}
	got := settled()
	slices.Sort(got)
	if !slices.Equal(got, []int{0, 1, 2, 3, 4}) {
		t.Fatalf("settled after concurrent marks = %v; want each card once", got)
	}
	_, msgs, _ := store.GetSession(ctx, itTenantA, itUser1, id)
	if p := PendingProposals(msgs); len(p) != 0 {
		t.Fatalf("pending after settling every card = %+v; want none", p)
	}

	if err := store.SoftDeleteSession(ctx, itTenantA, itUser1, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.SettleProposal(ctx, itTenantA, itUser1, id, 1, 0); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("settle on a deleted thread: %v; want ErrProposalNotFound", err)
	}
}

// Issue #42's migration: the down file drops settled_json and the up file brings it back, a second
// up being a no-op. Found by name, not number (the number is assigned at merge). Runs on its own
// multiStatements connection and leaves the shared throwaway DB migrated.
func TestMigration_SettledJSON_DownThenUpAgain(t *testing.T) {
	db := connectAI(t)
	read := func(suffix string) string {
		t.Helper()
		m, err := filepath.Glob("../../migrations/*_sys_ai_messages_settled." + suffix + ".sql")
		if err != nil || len(m) != 1 {
			t.Fatalf("migration %s: glob = %v, %v", suffix, m, err)
		}
		b, err := os.ReadFile(m[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	exec := func(script string) {
		t.Helper()
		conn, err := sqlx.Connect("mysql", aiTestDSN()+"&multiStatements=true")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Exec(script); err != nil {
			t.Fatalf("exec migration: %v", err)
		}
	}
	has := func() bool {
		t.Helper()
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = 'sys_ai_messages' AND column_name = 'settled_json'`); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if !has() {
		t.Fatal("after up: sys_ai_messages lacks settled_json")
	}
	exec(read("down"))
	if has() {
		t.Fatal("after down: settled_json is still there")
	}
	exec(read("down")) // a second down is a no-op
	exec(read("up"))
	exec(read("up")) // a second up is a no-op
	if !has() {
		t.Fatal("after up again: settled_json is missing")
	}
}
