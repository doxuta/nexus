package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/idempotency"
)

// fakeIdemStore is an in-memory IdempotencyStore keyed like the real unique index
// (tenant, principal, key). Release/Complete refuse a cancelled context the way the MySQL driver
// does, so a test can prove the service detaches those writes from the request context.
type fakeIdemStore struct {
	mu          sync.Mutex
	rows        map[string]idempotency.Claim
	completeErr error
	planErr     error // SavePlan fails (the plan is saved before a leased batch writes)
	// vanishOnce makes the first Claim report ErrKeyExists while Get finds nothing — the
	// "released between our INSERT and our SELECT" interleaving.
	vanishOnce bool
	releases   int
	takeovers  int
}

func newFakeIdemStore() *fakeIdemStore {
	return &fakeIdemStore{rows: map[string]idempotency.Claim{}}
}

func idemRowKey(tenantID, principalID, key string) string {
	return tenantID + "|" + principalID + "|" + key
}

// Claim models the worst case of an autocommit INSERT on a cancelled context: the row lands, and
// the driver still reports the cancellation.
func (f *fakeIdemStore) Claim(ctx context.Context, c idempotency.Claim) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vanishOnce {
		f.vanishOnce = false
		return idempotency.ErrKeyExists
	}
	k := idemRowKey(c.TenantID, c.PrincipalID, c.Key)
	if _, ok := f.rows[k]; ok {
		return idempotency.ErrKeyExists
	}
	c.Status = idempotency.StatusProcessing
	f.rows[k] = c
	return ctx.Err()
}

func (f *fakeIdemStore) Get(_ context.Context, tenantID, principalID, key string) (*idempotency.Claim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[idemRowKey(tenantID, principalID, key)]
	if !ok {
		return nil, idempotency.ErrNotFound
	}
	return &c, nil
}

func (f *fakeIdemStore) Complete(ctx context.Context, tenantID, principalID, key, recordID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.completeErr != nil {
		return f.completeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := idemRowKey(tenantID, principalID, key)
	c, ok := f.rows[k]
	if ok && c.Status == idempotency.StatusProcessing && c.RecordID == recordID {
		c.Status = idempotency.StatusCompleted
		f.rows[k] = c
	}
	return nil
}

func (f *fakeIdemStore) CompleteResponse(ctx context.Context, tenantID, principalID, key, recordID string, response []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.completeErr != nil {
		return f.completeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := idemRowKey(tenantID, principalID, key)
	if c, ok := f.rows[k]; ok && c.Status == idempotency.StatusProcessing && c.RecordID == recordID {
		c.Status, c.Response = idempotency.StatusCompleted, response
		f.rows[k] = c
	}
	return nil
}

func (f *fakeIdemStore) SavePlan(ctx context.Context, tenantID, principalID, key, recordID string, response []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.planErr != nil {
		return f.planErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := idemRowKey(tenantID, principalID, key)
	if c, ok := f.rows[k]; ok && c.Status == idempotency.StatusProcessing && c.RecordID == recordID {
		c.Response = response
		f.rows[k] = c
	}
	return nil
}

func (f *fakeIdemStore) Release(ctx context.Context, tenantID, principalID, key, recordID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	k := idemRowKey(tenantID, principalID, key)
	if c, ok := f.rows[k]; ok && c.Status == idempotency.StatusProcessing && c.RecordID == recordID {
		delete(f.rows, k)
	}
	return nil
}

func (f *fakeIdemStore) Takeover(ctx context.Context, tenantID, principalID, key, oldRecordID, newRecordID string, expiresAt time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.takeovers++
	k := idemRowKey(tenantID, principalID, key)
	c, ok := f.rows[k]
	if !ok || c.Status != idempotency.StatusProcessing || c.RecordID != oldRecordID {
		return false, nil
	}
	c.RecordID, c.ExpiresAt, c.Response = newRecordID, expiresAt, nil
	f.rows[k] = c
	return true, nil
}

func (f *fakeIdemStore) DeleteExpired(_ context.Context, tenantID, principalID, key string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := idemRowKey(tenantID, principalID, key)
	if c, ok := f.rows[k]; ok && c.ExpiresAt.Before(now) {
		delete(f.rows, k)
	}
	return nil
}

// idemRecordStore counts inserts and serves them back by id, so a replay's re-read is real. It
// keeps each record's tenant and owner and applies the row scope the way dynrepo.GetRecord does
// (all/admin: any row of the tenant; owner/team: only rows the caller owns; none: nothing), so a
// test can prove which scope the replay reads through.
type idemRecordStore struct {
	fakeRecordStore
	mu       sync.Mutex
	inserted map[string]bool
	tenant   map[string]string
	owner    map[string]string
	// insertHook, when set, runs inside InsertRecord (e.g. to cancel the request context).
	insertHook func()
	insertErr  error
}

func (s *idemRecordStore) InsertRecord(ctx context.Context, _, tenantID, userID string, _ map[string]any, _ []dynrepo.UniqueFieldSpec) (string, error) {
	// Like the driver: a cancelled context refuses the write before it starts.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.insertHook != nil {
		s.insertHook()
	}
	if s.insertErr != nil {
		return "", s.insertErr
	}
	// Like dynrepo: the id the idempotency claim chose, else a fresh one.
	id := dynrepo.RecordIDFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = fmt.Sprintf("rec-%d", len(s.inserted)+1)
	}
	s.put(id, tenantID, userID)
	return id, nil
}

// put stores a record as committed (callers hold s.mu).
func (s *idemRecordStore) put(id, tenantID, owner string) {
	if s.inserted == nil {
		s.inserted, s.tenant, s.owner = map[string]bool{}, map[string]string{}, map[string]string{}
	}
	s.inserted[id], s.tenant[id], s.owner[id] = true, tenantID, owner
}

func (s *idemRecordStore) RecordExists(_ context.Context, _, tenantID, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inserted[id] && s.tenant[id] == tenantID, nil
}

func (s *idemRecordStore) GetRecord(_ context.Context, _, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inserted[id] || s.tenant[id] != tenantID {
		return nil, dynrepo.ErrRecordNotFound
	}
	switch {
	case scope.IsAdmin || scope.Level == dynrepo.AccessAll:
	case (scope.Level == dynrepo.AccessOwner || scope.Level == dynrepo.AccessTeam) && s.owner[id] == scope.UserID:
	default:
		return nil, dynrepo.ErrRecordNotFound
	}
	return &dynrepo.Record{ID: id, TenantID: tenantID, Data: map[string]any{}}, nil
}

// reassign moves a record to another owner (a round-robin `assign` action, or a manual reassign).
func (s *idemRecordStore) reassign(id, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owner[id] = owner
}

func (s *idemRecordStore) inserts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inserted)
}

func newIdemService() (*RecordService, *idemRecordStore, *fakeIdemStore) {
	store := &idemRecordStore{}
	idem := newFakeIdemStore()
	cat := &fakeRecordCatalog{entity: courseEntityFixture(), fields: courseFieldDefs()}
	return NewRecordService(cat, store).WithIdempotencyStore(idem), store, idem
}

var idemScope = RecordScope{UserID: "user-1", AccessLevel: "all", RoleID: "role-1"}

func TestCreateIdempotent_FirstCallCreatesAndCompletesTheClaim(t *testing.T) {
	svc, store, idem := newIdemService()
	id, replayed, err := svc.CreateIdempotent(context.Background(), idemScope, "t1", "user-1", "courses",
		map[string]any{"title": "Go 101"}, "abc-1")
	if err != nil || replayed || id == "" {
		t.Fatalf("first call = (%q, replayed=%v, %v), want a fresh id, replayed=false, nil", id, replayed, err)
	}
	if store.inserts() != 1 {
		t.Fatalf("inserts = %d, want 1", store.inserts())
	}
	c, _ := idem.Get(context.Background(), "t1", "user-1", "abc-1")
	if c == nil || c.Status != idempotency.StatusCompleted || c.RecordID != id || c.EntityName != "courses" {
		t.Fatalf("claim after create = %+v, want completed with record %q", c, id)
	}
	if !c.ExpiresAt.After(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("claim expires at %s, want ~24h out", c.ExpiresAt)
	}
}

func TestCreateIdempotent_ResendIsAReplayNotASecondRecord(t *testing.T) {
	svc, store, _ := newIdemService()
	ctx := context.Background()
	first, _, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses",
		map[string]any{"title": "Go 101", "seats": float64(30)}, "abc-1")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// The same body decoded from differently ordered / spaced JSON must match.
	var resent map[string]any
	if err := json.Unmarshal([]byte(`{ "seats": 30.0,   "title": "Go 101" }`), &resent); err != nil {
		t.Fatal(err)
	}
	second, replayed, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", resent, "abc-1")
	if err != nil || !replayed || second != first {
		t.Fatalf("resend = (%q, replayed=%v, %v), want (%q, true, nil)", second, replayed, err, first)
	}
	if store.inserts() != 1 {
		t.Fatalf("inserts = %d after a resend, want 1", store.inserts())
	}
}

func TestCreateIdempotent_Refusals(t *testing.T) {
	tests := []struct {
		name     string
		seed     func(svc *RecordService, idem *fakeIdemStore)
		entity   string
		payload  map[string]any
		wantCode string
	}{
		{
			name: "same key, different body",
			seed: func(svc *RecordService, _ *fakeIdemStore) {
				_, _, _ = svc.CreateIdempotent(context.Background(), idemScope, "t1", "user-1", "courses", map[string]any{"title": "Go 101"}, "k")
			},
			entity: "courses", payload: map[string]any{"title": "Go 102"}, wantCode: "IDEMPOTENCY_KEY_REUSED",
		},
		{
			name: "same key, same body, first still processing",
			seed: func(_ *RecordService, idem *fakeIdemStore) {
				_ = idem.Claim(context.Background(), idempotency.Claim{TenantID: "t1", PrincipalID: "user-1", Key: "k",
					EntityName: "courses", RequestHash: idempotencyRequestHash("courses", map[string]any{"title": "Go 101"}),
					ExpiresAt: time.Now().Add(time.Hour)})
			},
			entity: "courses", payload: map[string]any{"title": "Go 101"}, wantCode: "IDEMPOTENCY_IN_PROGRESS",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, store, idem := newIdemService()
			tt.seed(svc, idem)
			before := store.inserts()
			_, _, err := svc.CreateIdempotent(context.Background(), idemScope, "t1", "user-1", tt.entity, tt.payload, "k")
			if got := codeOf(err); got != tt.wantCode {
				t.Fatalf("code = %q (err=%v), want %q", got, err, tt.wantCode)
			}
			if store.inserts() != before {
				t.Fatalf("a refused call inserted a record (%d -> %d)", before, store.inserts())
			}
		})
	}
}

func TestCreateIdempotent_SameKeyOtherEntityIsReuse(t *testing.T) {
	svc, _, idem := newIdemService()
	body := map[string]any{"title": "Go 101"}
	if _, _, err := svc.CreateIdempotent(context.Background(), idemScope, "t1", "user-1", "courses", body, "k"); err != nil {
		t.Fatal(err)
	}
	// Point the stored claim at another entity: the fingerprint covers the entity, not just the body.
	c, _ := idem.Get(context.Background(), "t1", "user-1", "k")
	c.RequestHash = idempotencyRequestHash("lessons", body)
	idem.rows[idemRowKey("t1", "user-1", "k")] = *c
	_, _, err := svc.CreateIdempotent(context.Background(), idemScope, "t1", "user-1", "courses", body, "k")
	if codeOf(err) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("err = %v, want IDEMPOTENCY_KEY_REUSED", err)
	}
}

func TestCreateIdempotent_KeyIsScopedToTenantAndPrincipal(t *testing.T) {
	svc, store, _ := newIdemService()
	ctx := context.Background()
	body := map[string]any{"title": "Go 101"}
	a, _, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", body, "k")
	if err != nil {
		t.Fatal(err)
	}
	b, replayed, err := svc.CreateIdempotent(ctx, RecordScope{UserID: "user-2", AccessLevel: "all"}, "t1", "user-2", "courses", body, "k")
	if err != nil || replayed || b == a {
		t.Fatalf("other principal = (%q, %v, %v), want its own record", b, replayed, err)
	}
	c, replayed, err := svc.CreateIdempotent(ctx, idemScope, "t2", "user-1", "courses", body, "k")
	if err != nil || replayed || c == a || c == b {
		t.Fatalf("other tenant = (%q, %v, %v), want its own record", c, replayed, err)
	}
	if store.inserts() != 3 {
		t.Fatalf("inserts = %d, want 3", store.inserts())
	}
}

func TestCreateIdempotent_ExpiredClaimIsReclaimed(t *testing.T) {
	svc, store, idem := newIdemService()
	body := map[string]any{"title": "Go 101"}
	idem.rows[idemRowKey("t1", "user-1", "k")] = idempotency.Claim{TenantID: "t1", PrincipalID: "user-1", Key: "k",
		EntityName: "courses", RequestHash: idempotencyRequestHash("courses", body), Status: idempotency.StatusCompleted,
		RecordID: "old-record", ExpiresAt: time.Now().Add(-time.Minute)}
	id, replayed, err := svc.CreateIdempotent(context.Background(), idemScope, "t1", "user-1", "courses", body, "k")
	if err != nil || replayed || id == "old-record" {
		t.Fatalf("expired claim = (%q, %v, %v), want a fresh create", id, replayed, err)
	}
	if store.inserts() != 1 {
		t.Fatalf("inserts = %d, want 1", store.inserts())
	}
}

func TestCreateIdempotent_FailedCreateReleasesTheKey(t *testing.T) {
	svc, store, idem := newIdemService()
	ctx := context.Background()
	_, _, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", map[string]any{"title": "x", "hacker": 1}, "k")
	if codeOf(err) != "RECORD_UNKNOWN_FIELD" {
		t.Fatalf("err = %v, want the create's own RECORD_UNKNOWN_FIELD", err)
	}
	if _, gerr := idem.Get(ctx, "t1", "user-1", "k"); !errors.Is(gerr, idempotency.ErrNotFound) {
		t.Fatalf("claim still present after a failed create (%v) — a corrected resend would be refused", gerr)
	}
	// The corrected resend runs.
	if _, replayed, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", map[string]any{"title": "x"}, "k"); err != nil || replayed {
		t.Fatalf("corrected resend = (replayed=%v, %v), want a fresh create", replayed, err)
	}
	if store.inserts() != 1 {
		t.Fatalf("inserts = %d, want 1", store.inserts())
	}
}

// The client that dropped the connection is exactly the case idempotency exists for: its request
// context is cancelled mid-create. Releasing with that context would leave the key stuck.
func TestCreateIdempotent_ReleaseSurvivesACancelledRequest(t *testing.T) {
	svc, store, idem := newIdemService()
	ctx, cancel := context.WithCancel(context.Background())
	store.insertHook = cancel
	store.insertErr = context.Canceled
	_, _, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", map[string]any{"title": "x"}, "k")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled from the insert", err)
	}
	if _, gerr := idem.Get(context.Background(), "t1", "user-1", "k"); !errors.Is(gerr, idempotency.ErrNotFound) {
		t.Fatalf("claim stuck after the client hung up (%v) — the release ran on the cancelled request context", gerr)
	}
}

func TestCreateIdempotent_CompleteFailureNeverDuplicates(t *testing.T) {
	svc, store, idem := newIdemService()
	ctx := context.Background()
	idem.completeErr = errors.New("db blip")
	id, replayed, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", map[string]any{"title": "x"}, "k")
	if err != nil || replayed || id == "" {
		t.Fatalf("create with a failing Complete = (%q, %v, %v), want the created id and no error", id, replayed, err)
	}
	idem.completeErr = nil
	_, _, err = svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", map[string]any{"title": "x"}, "k")
	if codeOf(err) != "IDEMPOTENCY_IN_PROGRESS" {
		t.Fatalf("resend = %v, want IDEMPOTENCY_IN_PROGRESS (never a second record)", err)
	}
	if store.inserts() != 1 {
		t.Fatalf("inserts = %d, want 1", store.inserts())
	}
}

// specs/047 FR-11: a claim still processing past its lease belongs to a create whose process died
// (or whose commit outcome was unknown). It is reconciled against obj_* by the record id it chose
// before its insert, instead of answering 409 until the 24 h window ends.
func TestCreateIdempotent_StaleProcessingClaim(t *testing.T) {
	body := map[string]any{"title": "Go 101"}
	tests := []struct {
		name string
		// claimedAgo is how long before now the claim was written; recordID the id it holds.
		claimedAgo time.Duration
		recordID   string
		committed  bool // the guarded create's record exists
		wantCode   string
		wantReplay bool
		wantFresh  bool // a new record is created under the taken-over claim
	}{
		{name: "past the lease, the create committed but its Complete was lost", claimedAgo: 3 * time.Minute,
			recordID: "11111111-1111-1111-1111-111111111111", committed: true, wantReplay: true},
		{name: "past the lease, nothing committed", claimedAgo: 3 * time.Minute,
			recordID: "22222222-2222-2222-2222-222222222222", wantFresh: true},
		{name: "inside the lease", claimedAgo: 30 * time.Second,
			recordID: "33333333-3333-3333-3333-333333333333", wantCode: "IDEMPOTENCY_IN_PROGRESS"},
		{name: "a claim written before FR-11 names no record", claimedAgo: 3 * time.Minute,
			wantCode: "IDEMPOTENCY_IN_PROGRESS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, store, idem := newIdemService()
			ctx := context.Background()
			if tt.committed {
				store.mu.Lock()
				store.put(tt.recordID, "t1", "user-1")
				store.mu.Unlock()
			}
			before := store.inserts()
			idem.rows[idemRowKey("t1", "user-1", "k")] = idempotency.Claim{TenantID: "t1", PrincipalID: "user-1", Key: "k",
				EntityName: "courses", RequestHash: idempotencyRequestHash("courses", body), Status: idempotency.StatusProcessing,
				RecordID: tt.recordID, ExpiresAt: time.Now().Add(-tt.claimedAgo).Add(idempotencyWindow)}

			id, replayed, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", body, "k")
			if tt.wantCode != "" {
				if codeOf(err) != tt.wantCode || store.inserts() != before {
					t.Fatalf("= (%q, %v, %v, inserts %d->%d), want %s and no insert", id, replayed, err, before, store.inserts(), tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("resend on a stale claim: %v", err)
			}
			c, _ := idem.Get(ctx, "t1", "user-1", "k")
			if c == nil || c.Status != idempotency.StatusCompleted || c.RecordID != id {
				t.Fatalf("claim after reconciliation = %+v, want completed with %q", c, id)
			}
			if tt.wantReplay && (!replayed || id != tt.recordID || store.inserts() != before) {
				t.Fatalf("= (%q, replayed=%v, inserts %d->%d), want a replay of %q and no second record", id, replayed, before, store.inserts(), tt.recordID)
			}
			if tt.wantFresh && (replayed || id == tt.recordID || store.inserts() != before+1) {
				t.Fatalf("= (%q, replayed=%v, inserts %d->%d), want one fresh record", id, replayed, before, store.inserts())
			}
		})
	}
}

// Several resends racing for the same stale claim: exactly one takes it over and creates; the others
// see a live claim (409) or, once it completed, replay the same record.
func TestCreateIdempotent_StaleClaimTakeoverRaceCreatesOnce(t *testing.T) {
	svc, store, idem := newIdemService()
	body := map[string]any{"title": "Go 101"}
	idem.rows[idemRowKey("t1", "user-1", "k")] = idempotency.Claim{TenantID: "t1", PrincipalID: "user-1", Key: "k",
		EntityName: "courses", RequestHash: idempotencyRequestHash("courses", body), Status: idempotency.StatusProcessing,
		RecordID: "44444444-4444-4444-4444-444444444444", ExpiresAt: time.Now().Add(-3 * time.Minute).Add(idempotencyWindow)}
	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], _, errs[i] = svc.CreateIdempotent(context.Background(), idemScope, "t1", "user-1", "courses", body, "k")
		}(i)
	}
	wg.Wait()
	if store.inserts() != 1 {
		t.Fatalf("inserts = %d after %d racing resends, want 1", store.inserts(), n)
	}
	var winner string
	for i := 0; i < n; i++ {
		switch {
		case errs[i] == nil && (winner == "" || winner == ids[i]):
			winner = ids[i]
		case codeOf(errs[i]) == "IDEMPOTENCY_IN_PROGRESS":
		default:
			t.Fatalf("resend %d = (%q, %v), want the one record or 409", i, ids[i], errs[i])
		}
	}
}

// A commit whose outcome is unknown (connection lost at COMMIT) may have written the record, so the
// claim is kept: a resend gets 409 now and the lease reconciliation later — never a second record.
// Only a failure certain to be before commit releases the key (TestCreateIdempotent_FailedCreateReleasesTheKey).
func TestCreateIdempotent_UnknownCommitOutcomeKeepsTheClaim(t *testing.T) {
	svc, store, idem := newIdemService()
	ctx := context.Background()
	store.insertErr = fmt.Errorf("dynrepo.InsertRecord: commit: %w: %w", dynrepo.ErrCommitOutcomeUnknown, errors.New("invalid connection"))
	if _, _, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", map[string]any{"title": "x"}, "k"); !errors.Is(err, dynrepo.ErrCommitOutcomeUnknown) {
		t.Fatalf("err = %v, want the unknown-commit error", err)
	}
	c, err := idem.Get(ctx, "t1", "user-1", "k")
	if err != nil || c.Status != idempotency.StatusProcessing || c.RecordID == "" {
		t.Fatalf("claim after an unknown commit = (%+v, %v), want it kept processing with its record id", c, err)
	}
	store.insertErr = nil
	if _, _, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", map[string]any{"title": "x"}, "k"); codeOf(err) != "IDEMPOTENCY_IN_PROGRESS" {
		t.Fatalf("immediate resend = %v, want IDEMPOTENCY_IN_PROGRESS", err)
	}
}

// The claim INSERT must not ride the request context: a client hanging up at that instant can see
// its autocommit INSERT land while the driver reports the cancellation — the claim would sit
// processing with no request behind it.
func TestCreateIdempotent_ClaimIsNotCancelledWithTheRequest(t *testing.T) {
	svc, _, idem := newIdemService()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _ = svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", map[string]any{"title": "x"}, "k")
	if c, err := idem.Get(context.Background(), "t1", "user-1", "k"); err == nil && c.Status == idempotency.StatusProcessing {
		t.Fatalf("claim left processing after the client hung up during the claim: %+v", c)
	}
}

func TestCreateIdempotent_ReplayOfADeletedRecordIsNotFound(t *testing.T) {
	svc, store, _ := newIdemService()
	ctx := context.Background()
	id, _, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", map[string]any{"title": "x"}, "k")
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	delete(store.inserted, id)
	store.mu.Unlock()
	_, _, err = svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", map[string]any{"title": "x"}, "k")
	if !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("replay of a deleted record = %v, want ErrRecordNotFound", err)
	}
}

// POST's RecordScope carries the row level of the CREATE grant (RequireACL maps POST to create), not
// of view. The replay only hands back the id this principal already received, so it must not be
// filtered by that level: an owner/team create grant whose record was reassigned since (the CRM
// pack's round-robin `assign` on crm_leads), or a create grant at row level none, would otherwise
// answer 404 for a record that exists — and a client reading 404 as "not created" POSTs again with a
// fresh key, the very duplicate the key exists to prevent.
func TestCreateIdempotent_ReplayIgnoresTheCreateGrantRowScope(t *testing.T) {
	tests := []struct {
		name     string
		scope    RecordScope
		reassign bool
	}{
		{name: "owner-level create grant, record reassigned since", scope: RecordScope{UserID: "user-1", AccessLevel: "owner", RoleID: "role-1"}, reassign: true},
		{name: "team-level create grant, record reassigned since", scope: RecordScope{UserID: "user-1", AccessLevel: "team", RoleID: "role-1"}, reassign: true},
		{name: "create grant at row level none", scope: RecordScope{UserID: "user-1", AccessLevel: "none", RoleID: "role-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, store, _ := newIdemService()
			ctx := context.Background()
			body := map[string]any{"title": "Go 101"}
			first, _, err := svc.CreateIdempotent(ctx, tt.scope, "t1", "user-1", "courses", body, "k")
			if err != nil {
				t.Fatalf("first: %v", err)
			}
			if tt.reassign {
				store.reassign(first, "user-9")
			}
			second, replayed, err := svc.CreateIdempotent(ctx, tt.scope, "t1", "user-1", "courses", body, "k")
			if err != nil || !replayed || second != first {
				t.Fatalf("resend = (%q, replayed=%v, %v), want (%q, true, nil) — a 404 here makes the client create a duplicate", second, replayed, err, first)
			}
			if store.inserts() != 1 {
				t.Fatalf("inserts = %d, want 1", store.inserts())
			}
		})
	}
}

// The existence probe is still pinned to the tenant: a claim whose record id lives in ANOTHER
// tenant (impossible through the API — claims are tenant-pinned — but the probe must not rely on
// that) answers not found, never a cross-tenant hit.
func TestCreateIdempotent_ReplayProbeIsTenantPinned(t *testing.T) {
	svc, store, idem := newIdemService()
	ctx := context.Background()
	body := map[string]any{"title": "Go 101"}
	other, _, err := svc.CreateIdempotent(ctx, idemScope, "t2", "user-1", "courses", body, "other-k")
	if err != nil {
		t.Fatal(err)
	}
	idem.rows[idemRowKey("t1", "user-1", "k")] = idempotency.Claim{TenantID: "t1", PrincipalID: "user-1", Key: "k",
		EntityName: "courses", RequestHash: idempotencyRequestHash("courses", body), Status: idempotency.StatusCompleted,
		RecordID: other, ExpiresAt: time.Now().Add(time.Hour)}
	if _, _, err := svc.CreateIdempotent(ctx, idemScope, "t1", "user-1", "courses", body, "k"); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("replay of a record in another tenant = %v, want ErrRecordNotFound", err)
	}
	if store.inserts() != 1 {
		t.Fatalf("inserts = %d, want 1", store.inserts())
	}
}

func TestCreateIdempotent_ClaimVanishingBetweenInsertAndReadIsRetried(t *testing.T) {
	svc, store, idem := newIdemService()
	idem.vanishOnce = true
	id, replayed, err := svc.CreateIdempotent(context.Background(), idemScope, "t1", "user-1", "courses", map[string]any{"title": "x"}, "k")
	if err != nil || replayed || id == "" || store.inserts() != 1 {
		t.Fatalf("= (%q, %v, %v, inserts=%d), want one fresh create after re-claiming", id, replayed, err, store.inserts())
	}
}

func TestCreateIdempotent_FailsClosedWithoutAStoreOrPrincipal(t *testing.T) {
	cat := &fakeRecordCatalog{entity: courseEntityFixture(), fields: courseFieldDefs()}
	store := &idemRecordStore{}
	bare := NewRecordService(cat, store)
	if _, _, err := bare.CreateIdempotent(context.Background(), idemScope, "t1", "user-1", "courses", map[string]any{"title": "x"}, "k"); err == nil {
		t.Fatal("no store wired: want an error, not an unguarded create")
	}
	svc, _, _ := newIdemService()
	if _, _, err := svc.CreateIdempotent(context.Background(), RecordScope{}, "t1", "", "courses", map[string]any{"title": "x"}, "k"); err == nil {
		t.Fatal("empty principal: want an error — every anonymous caller would share one key space")
	}
	if store.inserts() != 0 {
		t.Fatalf("inserts = %d, want 0", store.inserts())
	}
}

func TestIdempotencyRequestHash(t *testing.T) {
	decode := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	a := idempotencyRequestHash("courses", decode(`{"title":"x","meta":{"a":1,"b":[1,2]}}`))
	b := idempotencyRequestHash("courses", decode("{ \"meta\" : { \"b\":[1, 2.0], \"a\":1 },\n \"title\":\"x\" }"))
	if a != b {
		t.Fatalf("key order / whitespace / 2 vs 2.0 changed the fingerprint: %s vs %s", a, b)
	}
	if a == idempotencyRequestHash("lessons", decode(`{"title":"x","meta":{"a":1,"b":[1,2]}}`)) {
		t.Fatal("the entity is not part of the fingerprint")
	}
	if a == idempotencyRequestHash("courses", decode(`{"title":"y","meta":{"a":1,"b":[1,2]}}`)) {
		t.Fatal("a different body has the same fingerprint")
	}
	if len(a) != 64 {
		t.Fatalf("fingerprint %q is not a hex SHA-256", a)
	}
}
