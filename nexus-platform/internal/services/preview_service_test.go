package services

// preview_service_test.go — specs/064-preview-as-role-sample-data T011 (unit half). The integration
// half (every real revocation path against MySQL) lives in
// internal/handlers/preview_session_integration_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

const (
	pvTenant     = "11111111-1111-1111-1111-111111111111"
	pvOtherTen   = "22222222-2222-2222-2222-222222222222"
	pvRole       = "33333333-3333-3333-3333-333333333333"
	pvOtherRole  = "44444444-4444-4444-4444-444444444444"
	pvBuilder    = "55555555-5555-5555-5555-555555555555"
	pvLoginSID   = "66666666-6666-6666-6666-666666666666"
	pvJWTSecret  = "preview-service-test-secret-0123456789"
	pvDefaultTTL = 30 * time.Minute
)

// ---- fakes -------------------------------------------------------------------------------------

type fakePreviewStore struct {
	rows    map[string]*auth.PreviewSession // key tenant|id
	getErr  error
	created int
}

func newFakePreviewStore() *fakePreviewStore {
	return &fakePreviewStore{rows: map[string]*auth.PreviewSession{}}
}

func (f *fakePreviewStore) Create(_ context.Context, s *auth.PreviewSession) error {
	cp := *s
	f.rows[s.TenantID+"|"+s.ID] = &cp
	f.created++
	return nil
}

func (f *fakePreviewStore) GetByID(_ context.Context, tenantID, id string) (*auth.PreviewSession, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	s, ok := f.rows[tenantID+"|"+id]
	if !ok {
		return nil, auth.ErrPreviewSessionNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *fakePreviewStore) End(_ context.Context, tenantID, id, reason string, now time.Time) (bool, error) {
	s, ok := f.rows[tenantID+"|"+id]
	if !ok || s.EndedAt != nil {
		return false, nil
	}
	s.EndedAt, s.EndReason = &now, &reason
	return true, nil
}

type actorLiveCall struct{ att, act, sid string }

type fakeActorLive struct {
	calls []actorLiveCall
	err   error // nil ⇒ live
}

func (f *fakeActorLive) ActorLive(_ context.Context, att, act, sid string, _ time.Time) (*auth.LiveActor, error) {
	f.calls = append(f.calls, actorLiveCall{att, act, sid})
	if f.err != nil {
		return nil, f.err
	}
	return &auth.LiveActor{ID: act, FullName: "Người dựng"}, nil
}

type fakePreviewRoles struct {
	byTenant map[string]map[string]auth.Role
}

func (f *fakePreviewRoles) GetRoleByID(_ context.Context, tenantID, id string) (*auth.Role, error) {
	if r, ok := f.byTenant[tenantID][id]; ok {
		return &r, nil
	}
	return nil, auth.ErrRoleNotFound
}

type previewFixture struct {
	svc     *PreviewService
	store   *fakePreviewStore
	actors  *fakeActorLive
	auditor *captureAuditor
	tokens  *auth.TokenService
	now     time.Time
}

func newPreviewFixture(t *testing.T) *previewFixture {
	t.Helper()
	tokens, err := auth.NewTokenService(pvJWTSecret, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	f := &previewFixture{
		store:   newFakePreviewStore(),
		actors:  &fakeActorLive{},
		auditor: &captureAuditor{},
		tokens:  tokens,
		now:     time.Now().UTC().Truncate(time.Second),
	}
	roles := &fakePreviewRoles{byTenant: map[string]map[string]auth.Role{
		pvTenant:   {pvRole: {ID: pvRole, TenantID: pvTenant, Name: "crm_sales_rep", Label: "Nhân viên kinh doanh"}},
		pvOtherTen: {pvOtherRole: {ID: pvOtherRole, TenantID: pvOtherTen, Name: "other"}},
	}}
	f.svc = NewPreviewService(f.store, f.actors, roles, tokens, pvDefaultTTL).WithAuditor(f.auditor)
	f.svc.now = func() time.Time { return f.now }
	return f
}

func builderSubject() auth.Subject {
	return auth.Subject{UserID: pvBuilder, TenantID: pvTenant, IsAdmin: true, SessionID: pvLoginSID}
}

// start opens a session for pvRole and returns the result; fails the test on error.
func (f *previewFixture) start(t *testing.T) *PreviewStartResult {
	t.Helper()
	res, err := f.svc.Start(context.Background(), builderSubject(), PreviewStartInput{RoleID: pvRole})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return res
}

// ---- Start -------------------------------------------------------------------------------------

func TestPreviewStart_HappyPath(t *testing.T) {
	f := newPreviewFixture(t)
	appID := uuid.NewString()
	res, err := f.svc.Start(context.Background(), builderSubject(), PreviewStartInput{RoleID: pvRole, AppID: appID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Token == "" || res.SessionID == "" {
		t.Fatalf("Start: empty token/session: %+v", res)
	}
	if want := f.now.Add(pvDefaultTTL); !res.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %s, want now+TTL %s", res.ExpiresAt, want)
	}
	if res.Role.ID != pvRole || res.Role.Name != "crm_sales_rep" || res.Role.Label != "Nhân viên kinh doanh" {
		t.Errorf("role = %+v", res.Role)
	}
	if res.AppID == nil || *res.AppID != appID || res.Draft {
		t.Errorf("appId/draft = %v/%v", res.AppID, res.Draft)
	}

	row := f.store.rows[pvTenant+"|"+res.SessionID]
	if row == nil {
		t.Fatal("no sys_preview_sessions row written")
	}
	if row.PrincipalID != PreviewPrincipal(pvTenant, pvRole) || row.ActorUserID != pvBuilder ||
		row.ActorTenantID != pvTenant || row.ActorSessionID != pvLoginSID || row.CreatedBy != pvBuilder ||
		row.RoleID != pvRole || !row.ExpiresAt.Equal(res.ExpiresAt) {
		t.Errorf("row = %+v", row)
	}
	// The builder was checked live BEFORE the row: (home tenant, builder, login sid).
	if len(f.actors.calls) != 1 || f.actors.calls[0] != (actorLiveCall{pvTenant, pvBuilder, pvLoginSID}) {
		t.Errorf("ActorLive calls = %+v", f.actors.calls)
	}
	if len(f.auditor.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(f.auditor.events))
	}
	ev := f.auditor.events[0]
	if ev.EntityName != audit.PreviewMeta || ev.Action != audit.ActionCreate || ev.ActorUserID != pvBuilder ||
		ev.TenantID != pvTenant || ev.RecordID != res.SessionID || ev.Diff == nil || ev.Diff.After["roleId"] != pvRole {
		t.Errorf("audit event = %+v", ev)
	}
}

func TestPreviewPrincipal_IsUUIDv5PerTenantAndRole(t *testing.T) {
	p := PreviewPrincipal(pvTenant, pvRole)
	u, err := uuid.Parse(p)
	if err != nil || u.Version() != 5 {
		t.Fatalf("principal %q: version %v err %v; want a UUIDv5 (never collides with a v4 user id)", p, u.Version(), err)
	}
	if p != PreviewPrincipal(pvTenant, pvRole) {
		t.Error("principal is not deterministic for the same (tenant, role)")
	}
	if p == PreviewPrincipal(pvOtherTen, pvRole) || p == PreviewPrincipal(pvTenant, pvOtherRole) {
		t.Error("principal collides across tenants or roles")
	}
}

func TestPreviewStart_Refusals(t *testing.T) {
	cases := []struct {
		name     string
		sub      func() auth.Subject
		in       PreviewStartInput
		actorErr error
		wantCode string // ValidationError code, or "" for ErrPreviewForbidden
	}{
		{"role of another tenant", builderSubject, PreviewStartInput{RoleID: pvOtherRole}, nil, "PREVIEW_ROLE_NOT_FOUND"},
		{"unknown role", builderSubject, PreviewStartInput{RoleID: uuid.NewString()}, nil, "PREVIEW_ROLE_NOT_FOUND"},
		{"draft asked, no draft layer wired", builderSubject, PreviewStartInput{RoleID: pvRole, Draft: boolPtr(true)}, nil, "PREVIEW_DRAFT_UNAVAILABLE"},
		{"appId not a uuid", builderSubject, PreviewStartInput{RoleID: pvRole, AppID: "not-a-uuid"}, nil, "INVALID_BODY"},
		{"token without sid", func() auth.Subject { s := builderSubject(); s.SessionID = ""; return s },
			PreviewStartInput{RoleID: pvRole}, nil, ""},
		{"builder login session dead", builderSubject, PreviewStartInput{RoleID: pvRole}, auth.ErrActorNotLive, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPreviewFixture(t)
			f.actors.err = tc.actorErr
			_, err := f.svc.Start(context.Background(), tc.sub(), tc.in)
			if tc.wantCode == "" {
				if !errors.Is(err, ErrPreviewForbidden) {
					t.Fatalf("err = %v, want ErrPreviewForbidden", err)
				}
			} else {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tc.wantCode {
					t.Fatalf("err = %v, want ValidationError %s", err, tc.wantCode)
				}
			}
			if f.store.created != 0 {
				t.Errorf("a refused Start wrote %d sys_preview_sessions rows", f.store.created)
			}
			if len(f.auditor.events) != 0 {
				t.Errorf("a refused Start wrote %d audit events", len(f.auditor.events))
			}
		})
	}
}

func TestPreviewStart_ActorDBErrorIsNotForbidden(t *testing.T) {
	f := newPreviewFixture(t)
	f.actors.err = errors.New("db down")
	_, err := f.svc.Start(context.Background(), builderSubject(), PreviewStartInput{RoleID: pvRole})
	if err == nil || errors.Is(err, ErrPreviewForbidden) {
		t.Fatalf("err = %v, want a wrapped infrastructure error (500), not 403", err)
	}
	if f.store.created != 0 {
		t.Error("Start wrote a row after the builder check failed")
	}
}

// ---- Verify ------------------------------------------------------------------------------------

func TestPreviewVerify_Success(t *testing.T) {
	f := newPreviewFixture(t)
	res := f.start(t)
	f.actors.calls = nil

	claims, err := f.svc.Verify(context.Background(), res.Token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.IsAdmin || claims.IsPlatformOperator || claims.WorkspaceSessionID != "" {
		t.Errorf("preview claims carry privilege: admin=%v operator=%v wsid=%q", claims.IsAdmin, claims.IsPlatformOperator, claims.WorkspaceSessionID)
	}
	if claims.TokenType != auth.TokenTypePreview || claims.RoleID != pvRole || claims.TenantID != pvTenant ||
		claims.UserID != PreviewPrincipal(pvTenant, pvRole) || claims.SessionID != res.SessionID ||
		claims.ActorUserID != pvBuilder || claims.ActorTenantID != pvTenant {
		t.Errorf("claims = %+v", claims)
	}
	// ActorLive gets the builder's LOGIN sid from the row — never the preview token's own sid claim.
	if len(f.actors.calls) != 1 {
		t.Fatalf("ActorLive calls = %d, want 1 (every request, uncached)", len(f.actors.calls))
	}
	if got := f.actors.calls[0]; got != (actorLiveCall{pvTenant, pvBuilder, pvLoginSID}) || got.sid == res.SessionID {
		t.Errorf("ActorLive(%+v); want (home tenant, builder, login sid %s) — not the preview sid %s", got, pvLoginSID, res.SessionID)
	}
}

func TestPreviewVerify_NoCache(t *testing.T) {
	f := newPreviewFixture(t)
	res := f.start(t)
	f.actors.calls = nil
	for i := 0; i < 3; i++ {
		if _, err := f.svc.Verify(context.Background(), res.Token); err != nil {
			t.Fatalf("Verify #%d: %v", i, err)
		}
	}
	if len(f.actors.calls) != 3 {
		t.Fatalf("ActorLive calls = %d over 3 requests; the builder check must never be cached", len(f.actors.calls))
	}
}

func TestPreviewVerify_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *previewFixture, res *PreviewStartResult) string // returns the token to verify
	}{
		{"session row missing", func(f *previewFixture, res *PreviewStartResult) string {
			delete(f.store.rows, pvTenant+"|"+res.SessionID)
			return res.Token
		}},
		{"session ended", func(f *previewFixture, res *PreviewStartResult) string {
			now := f.now
			f.store.rows[pvTenant+"|"+res.SessionID].EndedAt = &now
			return res.Token
		}},
		{"row expired while the JWT is not", func(f *previewFixture, res *PreviewStartResult) string {
			f.store.rows[pvTenant+"|"+res.SessionID].ExpiresAt = f.now.Add(-time.Second)
			return res.Token
		}},
		{"role mismatch", func(f *previewFixture, res *PreviewStartResult) string {
			f.store.rows[pvTenant+"|"+res.SessionID].RoleID = pvOtherRole
			return res.Token
		}},
		{"principal mismatch", func(f *previewFixture, res *PreviewStartResult) string {
			f.store.rows[pvTenant+"|"+res.SessionID].PrincipalID = uuid.NewString()
			return res.Token
		}},
		{"actor mismatch", func(f *previewFixture, res *PreviewStartResult) string {
			f.store.rows[pvTenant+"|"+res.SessionID].ActorUserID = uuid.NewString()
			return res.Token
		}},
		{"builder no longer live (inactive / demoted / operator removed / session ended / RevokeAllForUser / refresh expired / home tenant suspended)",
			func(f *previewFixture, res *PreviewStartResult) string {
				f.actors.err = auth.ErrActorNotLive
				return res.Token
			}},
		{"builder check DB error fails closed", func(f *previewFixture, res *PreviewStartResult) string {
			f.actors.err = errors.New("db down")
			return res.Token
		}},
		{"session lookup DB error fails closed", func(f *previewFixture, res *PreviewStartResult) string {
			f.store.getErr = errors.New("db down")
			return res.Token
		}},
		{"row lives in another tenant", func(f *previewFixture, res *PreviewStartResult) string {
			row := f.store.rows[pvTenant+"|"+res.SessionID]
			delete(f.store.rows, pvTenant+"|"+res.SessionID)
			row.TenantID = pvOtherTen
			f.store.rows[pvOtherTen+"|"+res.SessionID] = row
			return res.Token
		}},
		{"access token is not a preview token", func(f *previewFixture, _ *PreviewStartResult) string {
			tok, err := f.tokens.IssueAccessToken(builderSubject())
			if err != nil {
				panic(err)
			}
			return tok
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPreviewFixture(t)
			res := f.start(t)
			tok := tc.mutate(f, res)
			claims, err := f.svc.Verify(context.Background(), tok)
			if err == nil {
				t.Fatalf("Verify passed (claims %+v); want a refusal", claims)
			}
		})
	}
}

// ---- End ---------------------------------------------------------------------------------------

func TestPreviewEnd_IdempotentAndAudited(t *testing.T) {
	f := newPreviewFixture(t)
	res := f.start(t)
	claims, err := f.svc.Verify(context.Background(), res.Token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	f.auditor.events = nil

	if err := f.svc.End(context.Background(), claims.Subject, "exit"); err != nil {
		t.Fatalf("End: %v", err)
	}
	row := f.store.rows[pvTenant+"|"+res.SessionID]
	if row.EndedAt == nil || row.EndReason == nil || *row.EndReason != "exit" {
		t.Fatalf("row after End = %+v", row)
	}
	if len(f.auditor.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(f.auditor.events))
	}
	ev := f.auditor.events[0]
	if ev.EntityName != audit.PreviewMeta || ev.Action != audit.ActionDelete || ev.ActorUserID != pvBuilder ||
		ev.RecordID != res.SessionID || ev.TenantID != pvTenant {
		t.Errorf("end audit = %+v (actor must be the REAL builder, not the principal)", ev)
	}

	// Second End: no error, no second audit row.
	if err := f.svc.End(context.Background(), claims.Subject, "exit"); err != nil {
		t.Fatalf("End #2: %v", err)
	}
	if len(f.auditor.events) != 1 {
		t.Errorf("idempotent End wrote another audit row")
	}
	// And the token is dead.
	if _, err := f.svc.Verify(context.Background(), res.Token); err == nil {
		t.Error("Verify after End passed")
	}
}

// ---- CompositeVerifier.WithPreview (T020) ------------------------------------------------------

func TestCompositeVerifier_PreviewDoorOpensOnlyWhenWired(t *testing.T) {
	f := newPreviewFixture(t)
	res := f.start(t)
	access, err := f.tokens.IssueAccessToken(builderSubject())
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	refresh, err := f.tokens.IssueRefreshToken(builderSubject())
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}

	closed := NewCompositeVerifier(f.tokens, nil)
	if _, err := closed.VerifyAccessTokenContext(context.Background(), res.Token); !errors.Is(err, auth.ErrWrongTokenType) {
		t.Errorf("unwired composite accepted a preview token: %v", err)
	}

	open := NewCompositeVerifier(f.tokens, nil).WithPreview(f.svc)
	claims, err := open.VerifyAccessTokenContext(context.Background(), res.Token)
	if err != nil || claims.TokenType != auth.TokenTypePreview || claims.IsAdmin {
		t.Fatalf("preview through composite: claims=%+v err=%v", claims, err)
	}
	if claims, err := open.VerifyAccessTokenContext(context.Background(), access); err != nil || claims.TokenType != auth.TokenTypeAccess {
		t.Errorf("access token through composite: claims=%+v err=%v", claims, err)
	}
	if _, err := open.VerifyAccessTokenContext(context.Background(), refresh); err == nil {
		t.Error("a refresh token passed the Bearer verifier")
	}
}
