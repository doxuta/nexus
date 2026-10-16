package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// fakeWorkspaceStore is an in-memory auth.WorkspaceSessionRepository stand-in.
type fakeWorkspaceStore struct {
	live      *auth.WorkspaceSessionLive
	liveErr   error
	actor     *auth.LiveActor
	actorErr  error
	open      *auth.WorkspaceSession // what FindOpen returns (nil ⇒ not live)
	resumable *auth.WorkspaceSession // what FindResumable returns (nil ⇒ not live)
	closeOK   bool
	closeErr  error
	findErr   error

	created   []*auth.WorkspaceSession
	liveCalls int
	actCalls  int
	gotLive   [5]string // tenant, id, act, att, sid
	gotActor  [3]string // att, act, sid
	gotClose  [4]string // tenant, id, act, reason
	gotFind   [4]string // tenant, act, att, sid (FindOpen) | tenant, id, act, sid (FindResumable)
}

func (f *fakeWorkspaceStore) GetLive(_ context.Context, tenantID, id, act, att, sid string, _ time.Time) (*auth.WorkspaceSessionLive, error) {
	f.liveCalls++
	f.gotLive = [5]string{tenantID, id, act, att, sid}
	return f.live, f.liveErr
}

func (f *fakeWorkspaceStore) ActorLive(_ context.Context, att, act, sid string, _ time.Time) (*auth.LiveActor, error) {
	f.actCalls++
	f.gotActor = [3]string{att, act, sid}
	return f.actor, f.actorErr
}

func (f *fakeWorkspaceStore) Create(_ context.Context, s *auth.WorkspaceSession) error {
	f.created = append(f.created, s)
	return nil
}

func (f *fakeWorkspaceStore) FindOpen(_ context.Context, tenantID, act, _, sid string, _ time.Time) (*auth.WorkspaceSession, error) {
	f.gotFind = [4]string{tenantID, act, "", sid}
	if f.findErr != nil {
		return nil, f.findErr
	}
	if f.open == nil {
		return nil, auth.ErrWorkspaceSessionNotLive
	}
	return f.open, nil
}

func (f *fakeWorkspaceStore) FindResumable(_ context.Context, tenantID, id, act, _, sid string, _ time.Time) (*auth.WorkspaceSession, error) {
	f.gotFind = [4]string{tenantID, id, act, sid}
	if f.findErr != nil {
		return nil, f.findErr
	}
	if f.resumable == nil {
		return nil, auth.ErrWorkspaceSessionNotLive
	}
	return f.resumable, nil
}

func (f *fakeWorkspaceStore) Close(_ context.Context, tenantID, id, act, reason string, _ time.Time) (bool, error) {
	f.gotClose = [4]string{tenantID, id, act, reason}
	return f.closeOK, f.closeErr
}

type fakeWorkspaceTenants struct {
	bySlug map[string]*auth.Tenant
	calls  int
}

func (f *fakeWorkspaceTenants) ResolveTenantBySlug(_ context.Context, slug string) (*auth.Tenant, error) {
	f.calls++
	if t, ok := f.bySlug[slug]; ok {
		return t, nil
	}
	return nil, auth.ErrTenantNotFound
}

type fakeWorkspaceTokens struct {
	until time.Time
	sub   auth.Subject
	err   error
}

func (f *fakeWorkspaceTokens) IssueAccessTokenUntil(sub auth.Subject, until time.Time) (string, error) {
	f.sub, f.until = sub, until
	return "ws.token", f.err
}
func (f *fakeWorkspaceTokens) AccessTTL() time.Duration { return 15 * time.Minute }

func TestWorkspaceSessionService_Resolve(t *testing.T) {
	claims := &auth.Claims{Subject: auth.Subject{
		UserID: "builder-1", TenantID: "customer-1", SessionID: "login-sid",
		ActorUserID: "builder-1", ActorTenantID: "home-1", WorkspaceSessionID: "ws-1",
	}}
	okLive := &auth.WorkspaceSessionLive{ID: "ws-1", TenantID: "customer-1", TenantSlug: "khach"}
	okActor := &auth.LiveActor{ID: "builder-1", FullName: "Người Dựng"}
	dbErr := errors.New("db down")

	cases := []struct {
		name      string
		store     *fakeWorkspaceStore
		wantErr   error // errors.Is target; nil ⇒ success
		wantDB    bool  // any non-sentinel error
		wantActor int   // ActorLive calls
	}{
		{"live session and builder", &fakeWorkspaceStore{live: okLive, actor: okActor}, nil, false, 1},
		{"session not live: the builder is not even asked", &fakeWorkspaceStore{liveErr: auth.ErrWorkspaceSessionNotLive}, auth.ErrWorkspaceSessionNotLive, false, 0},
		{"builder not live", &fakeWorkspaceStore{live: okLive, actorErr: auth.ErrActorNotLive}, auth.ErrActorNotLive, false, 1},
		{"GetLive DB error is wrapped, not a sentinel", &fakeWorkspaceStore{liveErr: dbErr}, dbErr, true, 0},
		{"ActorLive DB error is wrapped, not a sentinel", &fakeWorkspaceStore{live: okLive, actorErr: dbErr}, dbErr, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewWorkspaceSessionService(tc.store, nil, nil)
			res, err := svc.Resolve(context.Background(), claims)
			if tc.wantErr == nil {
				if err != nil || res == nil || res.Session.ID != "ws-1" || res.Actor.ID != "builder-1" {
					t.Fatalf("Resolve = %+v, %v; want the live session + builder", res, err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantDB && (errors.Is(err, auth.ErrWorkspaceSessionNotLive) || errors.Is(err, auth.ErrActorNotLive)) {
				t.Error("a DB error must not look like a revocation sentinel (it would be answered 401 instead of failing closed with 500)")
			}
			if tc.store.actCalls != tc.wantActor {
				t.Errorf("ActorLive calls = %d, want %d", tc.store.actCalls, tc.wantActor)
			}
			// Every claim of the token is passed through, pinned: tenant = the customer, att = the workshop.
			if tc.store.gotLive != [5]string{"customer-1", "ws-1", "builder-1", "home-1", "login-sid"} {
				t.Errorf("GetLive args = %v", tc.store.gotLive)
			}
			if tc.store.actCalls > 0 && tc.store.gotActor != [3]string{"home-1", "builder-1", "login-sid"} {
				t.Errorf("ActorLive args = %v, want (att, act, sid)", tc.store.gotActor)
			}
		})
	}
}

// ---- Open / Close (L1b) ------------------------------------------------------------------

var wsNow = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

type wsOpenFixture struct {
	svc     *WorkspaceSessionService
	store   *fakeWorkspaceStore
	tenants *fakeWorkspaceTenants
	tokens  *fakeWorkspaceTokens
	aud     *spyAuditor
	home    auth.Subject
}

func newWSOpenFixture() *wsOpenFixture {
	store := &fakeWorkspaceStore{
		actor:   &auth.LiveActor{ID: "builder-1", Email: "builder@example.com", FullName: "Nguyễn Văn Dựng", TenantName: "Xưởng Nexus"},
		closeOK: true,
	}
	tenants := &fakeWorkspaceTenants{bySlug: map[string]*auth.Tenant{
		"khach": {ID: "customer-1", Slug: "khach", Name: "Khách Một"},
		"home":  {ID: "home-1", Slug: "home", Name: "Xưởng Nexus"},
	}}
	tokens := &fakeWorkspaceTokens{}
	aud := &spyAuditor{}
	svc := NewWorkspaceSessionService(store, tenants, tokens).WithAuditor(aud)
	svc.now = func() time.Time { return wsNow }
	svc.newID = func() string { return "ws-new" }
	return &wsOpenFixture{
		svc: svc, store: store, tenants: tenants, tokens: tokens, aud: aud,
		home: auth.Subject{UserID: "builder-1", TenantID: "home-1", IsAdmin: true, SessionID: "login-sid", IsPlatformOperator: true},
	}
}

func TestWorkspaceSessionService_Open_New(t *testing.T) {
	f := newWSOpenFixture()
	res, err := f.svc.Open(context.Background(), f.home, WorkspaceOpenInput{TenantSlug: "khach"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.Created || res.Session.Resumed || res.AccessToken != "ws.token" {
		t.Errorf("result = %+v, want a created (not resumed) session with a token", res)
	}
	if len(f.store.created) != 1 {
		t.Fatalf("created %d rows, want 1", len(f.store.created))
	}
	row := f.store.created[0]
	if row.ID != "ws-new" || row.TenantID != "customer-1" || row.ActorUserID != "builder-1" || row.ActorTenantID != "home-1" ||
		row.ActorSessionID != "login-sid" || row.CreatedBy != "builder-1" {
		t.Errorf("row = %+v", row)
	}
	if !row.ExpiresAt.Equal(wsNow.Add(8 * time.Hour)) {
		t.Errorf("expires_at = %s, want now+8h", row.ExpiresAt)
	}
	if row.ActorLabel != "Nguyễn Văn Dựng (Xưởng Nexus)" || strings.Contains(row.ActorLabel, "@") {
		t.Errorf("label = %q, want the full name + workshop and NEVER the e-mail", row.ActorLabel)
	}
	// Token: customer tenant, admin, the three delegation claims, home login sid; exp = min(accessTTL, hard deadline).
	want := auth.Subject{UserID: "builder-1", TenantID: "customer-1", IsAdmin: true, SessionID: "login-sid",
		ActorUserID: "builder-1", ActorTenantID: "home-1", WorkspaceSessionID: "ws-new"}
	if f.tokens.sub != want {
		t.Errorf("token subject = %+v, want %+v", f.tokens.sub, want)
	}
	if !f.tokens.until.Equal(row.ExpiresAt) {
		t.Errorf("token until = %s, want the session deadline (the token helper caps it at accessTTL)", f.tokens.until)
	}
	if !res.ExpiresAt.Equal(wsNow.Add(15*time.Minute)) || !res.Session.ExpiresAt.Equal(row.ExpiresAt) {
		t.Errorf("expiresAt = %s / session.expiresAt = %s", res.ExpiresAt, res.Session.ExpiresAt)
	}
	// Audit: create at BOTH tenants, actor = the builder, record = the session id.
	if len(f.aud.events) != 2 {
		t.Fatalf("audit events = %d, want 2 (customer + home)", len(f.aud.events))
	}
	byTenant := map[string]audit.Event{}
	for _, e := range f.aud.events {
		byTenant[e.TenantID] = e
		if e.EntityName != audit.WorkspaceSessionMeta || e.Action != audit.ActionCreate || e.RecordID != "ws-new" || e.ActorUserID != "builder-1" {
			t.Errorf("event = %+v", e)
		}
	}
	if _, ok := byTenant["customer-1"]; !ok {
		t.Error("no audit row under the customer tenant")
	}
	home, ok := byTenant["home-1"]
	if !ok {
		t.Fatal("no audit row under the home tenant")
	}
	if home.Diff.After["tenantSlug"] != "khach" || home.Diff.After["tenantName"] != "Khách Một" {
		t.Errorf("home diff = %+v, want the customer's slug and name", home.Diff.After)
	}
}

func TestWorkspaceSessionService_Open_EmptyFullNameUsesFallbackLabel(t *testing.T) {
	f := newWSOpenFixture()
	f.store.actor = &auth.LiveActor{ID: "builder-1", Email: "builder@example.com", FullName: "  ", TenantName: "Xưởng Nexus"}
	if _, err := f.svc.Open(context.Background(), f.home, WorkspaceOpenInput{TenantSlug: "khach"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := f.store.created[0].ActorLabel; got != "Người dựng (Xưởng Nexus)" {
		t.Errorf("label = %q, want the fallback without any e-mail", got)
	}
}

func TestWorkspaceSessionService_Open_ReusesAnOpenSession(t *testing.T) {
	f := newWSOpenFixture()
	f.store.open = &auth.WorkspaceSession{ID: "ws-old", TenantID: "customer-1", ExpiresAt: wsNow.Add(2 * time.Hour), CreatedAt: wsNow.Add(-time.Hour)}
	res, err := f.svc.Open(context.Background(), f.home, WorkspaceOpenInput{TenantSlug: "khach"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.Created || !res.Session.Resumed || res.Session.ID != "ws-old" {
		t.Errorf("result = %+v, want the existing session reused (resumed:true)", res)
	}
	if len(f.store.created) != 0 || len(f.aud.events) != 0 {
		t.Errorf("created=%d audit=%d, want 0 and 0 on a reuse", len(f.store.created), len(f.aud.events))
	}
	if f.tokens.sub.WorkspaceSessionID != "ws-old" {
		t.Errorf("token wsid = %q, want ws-old", f.tokens.sub.WorkspaceSessionID)
	}
	if !f.tokens.until.Equal(wsNow.Add(2*time.Hour)) || !res.ExpiresAt.Equal(wsNow.Add(15*time.Minute)) {
		t.Errorf("until=%s expiresAt=%s", f.tokens.until, res.ExpiresAt)
	}
}

func TestWorkspaceSessionService_Open_ResumeNeverCreates(t *testing.T) {
	f := newWSOpenFixture()
	f.store.resumable = &auth.WorkspaceSession{ID: "ws-old", TenantID: "customer-1", ExpiresAt: wsNow.Add(10 * time.Minute)}
	res, err := f.svc.Open(context.Background(), f.home, WorkspaceOpenInput{TenantSlug: "khach", ResumeSessionID: "ws-old"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Created || res.Session.ID != "ws-old" || len(f.store.created) != 0 || len(f.aud.events) != 0 {
		t.Errorf("resume = %+v created=%d audit=%d", res, len(f.store.created), len(f.aud.events))
	}
	// Token exp is capped by the session's own deadline (10 min < accessTTL 15 min).
	if !res.ExpiresAt.Equal(wsNow.Add(10 * time.Minute)) {
		t.Errorf("token exp = %s, want min(accessTTL, deadline) = now+10m", res.ExpiresAt)
	}

	// A closed/expired/foreign id is ENDED — and does NOT fall back to opening a fresh session.
	f2 := newWSOpenFixture()
	f2.store.open = &auth.WorkspaceSession{ID: "ws-other", ExpiresAt: wsNow.Add(time.Hour)} // would be reused if resume fell through
	_, err = f2.svc.Open(context.Background(), f2.home, WorkspaceOpenInput{TenantSlug: "khach", ResumeSessionID: "ws-closed"})
	if !errors.Is(err, ErrWorkspaceSessionEnded) {
		t.Fatalf("resume of a dead id: err = %v, want ErrWorkspaceSessionEnded", err)
	}
	if len(f2.store.created) != 0 || len(f2.aud.events) != 0 {
		t.Error("a failed resume created a row or wrote audit")
	}
}

func TestWorkspaceSessionService_Open_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(f *wsOpenFixture, in *WorkspaceOpenInput)
		wantErr error
	}{
		{"called with a workspace session token", func(f *wsOpenFixture, _ *WorkspaceOpenInput) { f.home.WorkspaceSessionID = "ws-x" }, ErrWorkspaceNotAllowed},
		{"empty slug", func(_ *wsOpenFixture, in *WorkspaceOpenInput) { in.TenantSlug = "  " }, ErrWorkspaceSlugRequired},
		{"token without sid (pre-055)", func(f *wsOpenFixture, _ *WorkspaceOpenInput) { f.home.SessionID = "" }, ErrWorkspaceUnauthorized},
		{"home login no longer live", func(f *wsOpenFixture, _ *WorkspaceOpenInput) { f.store.actorErr = auth.ErrActorNotLive }, ErrWorkspaceUnauthorized},
		{"unknown / inactive slug", func(_ *wsOpenFixture, in *WorkspaceOpenInput) { in.TenantSlug = "khong-co" }, ErrWorkspaceTenantNotFound},
		{"the target is the home tenant", func(_ *wsOpenFixture, in *WorkspaceOpenInput) { in.TenantSlug = "home" }, ErrWorkspaceIsHome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWSOpenFixture()
			in := WorkspaceOpenInput{TenantSlug: "khach"}
			tc.mutate(f, &in)
			_, err := f.svc.Open(context.Background(), f.home, in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if len(f.store.created) != 0 || len(f.aud.events) != 0 {
				t.Errorf("a refusal created %d rows / %d audit events", len(f.store.created), len(f.aud.events))
			}
		})
	}
}

func TestWorkspaceSessionService_Open_DBErrorsAreNotSentinels(t *testing.T) {
	f := newWSOpenFixture()
	f.store.actorErr = errors.New("db down")
	_, err := f.svc.Open(context.Background(), f.home, WorkspaceOpenInput{TenantSlug: "khach"})
	if err == nil || errors.Is(err, ErrWorkspaceUnauthorized) {
		t.Fatalf("err = %v, want a wrapped DB error (500), not the 401 sentinel", err)
	}
}

func TestWorkspaceSessionService_Close(t *testing.T) {
	sess := auth.Subject{
		UserID: "builder-1", TenantID: "customer-1", IsAdmin: true, SessionID: "login-sid",
		ActorUserID: "builder-1", ActorTenantID: "home-1", WorkspaceSessionID: "ws-1", IsPlatformOperator: true,
	}
	t.Run("closes and audits both sides", func(t *testing.T) {
		f := newWSOpenFixture()
		if err := f.svc.Close(context.Background(), sess); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if f.store.gotClose != [4]string{"customer-1", "ws-1", "builder-1", auth.WorkspaceClosedByBuilder} {
			t.Errorf("Close args = %v", f.store.gotClose)
		}
		if len(f.aud.events) != 2 {
			t.Fatalf("audit events = %d, want 2", len(f.aud.events))
		}
		tenants := map[string]bool{}
		for _, e := range f.aud.events {
			tenants[e.TenantID] = true
			if e.EntityName != audit.WorkspaceSessionMeta || e.Action != audit.ActionDelete || e.RecordID != "ws-1" || e.ActorUserID != "builder-1" {
				t.Errorf("event = %+v", e)
			}
		}
		if !tenants["customer-1"] || !tenants["home-1"] {
			t.Errorf("audit tenants = %v, want customer-1 and home-1", tenants)
		}
	})
	t.Run("duplicate close (RowsAffected 0) writes no audit and still succeeds", func(t *testing.T) {
		f := newWSOpenFixture()
		f.store.closeOK = false
		if err := f.svc.Close(context.Background(), sess); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if len(f.aud.events) != 0 {
			t.Errorf("audit events = %d, want 0 when this call did not close the row", len(f.aud.events))
		}
	})
	t.Run("a home token has no session to close", func(t *testing.T) {
		f := newWSOpenFixture()
		home := sess
		home.WorkspaceSessionID, home.ActorUserID, home.ActorTenantID = "", "", ""
		if err := f.svc.Close(context.Background(), home); !errors.Is(err, ErrWorkspaceSessionNotFound) {
			t.Fatalf("err = %v, want ErrWorkspaceSessionNotFound", err)
		}
	})
	t.Run("DB error is wrapped", func(t *testing.T) {
		f := newWSOpenFixture()
		f.store.closeErr = errors.New("db down")
		if err := f.svc.Close(context.Background(), sess); err == nil {
			t.Fatal("want error")
		}
	})
}
