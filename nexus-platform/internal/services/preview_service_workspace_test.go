package services

// preview_service_workspace_test.go — specs/064-preview-as-role-sample-data T120 (S9-8): the preview
// token opened from INSIDE a customer's workspace. The builder's token is a workspace-session token
// (uid=act, tid=K the customer, att=home workshop, wsid, sid=home login); the preview row must name
// the REAL builder tenant (att, not K), carry wsid, and every later request re-checks the workspace
// session (GetLive) on top of the builder (ActorLive), uncached.

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

const (
	pvHomeTenant = "77777777-7777-7777-7777-777777777777" // the builder's own workshop (att)
	pvWsid       = "88888888-8888-8888-8888-888888888888"
)

type wsLiveCall struct{ tenant, id, act, att, sid string }

type fakeWorkspaceLive struct {
	calls []wsLiveCall
	err   error // nil ⇒ live
}

func (f *fakeWorkspaceLive) GetLive(_ context.Context, tenantID, id, act, att, sid string, _ time.Time) (*auth.WorkspaceSessionLive, error) {
	f.calls = append(f.calls, wsLiveCall{tenantID, id, act, att, sid})
	if f.err != nil {
		return nil, f.err
	}
	return &auth.WorkspaceSessionLive{ID: id, TenantID: tenantID}, nil
}

// workspaceBuilder is the Subject of a builder working inside customer pvTenant (K).
func workspaceBuilder() auth.Subject {
	return auth.Subject{
		UserID: pvBuilder, TenantID: pvTenant, IsAdmin: true, IsPlatformOperator: true, SessionID: pvLoginSID,
		ActorUserID: pvBuilder, ActorTenantID: pvHomeTenant, WorkspaceSessionID: pvWsid,
	}
}

func newWorkspacePreviewFixture(t *testing.T) (*previewFixture, *fakeWorkspaceLive) {
	t.Helper()
	f := newPreviewFixture(t)
	ws := &fakeWorkspaceLive{}
	f.svc.WithWorkspaceSessions(ws)
	return f, ws
}

func TestPreviewStart_InCustomerWorkspace(t *testing.T) {
	f, ws := newWorkspacePreviewFixture(t)
	res, err := f.svc.Start(context.Background(), workspaceBuilder(), PreviewStartInput{RoleID: pvRole})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	row := f.store.rows[pvTenant+"|"+res.SessionID]
	if row == nil {
		t.Fatal("no sys_preview_sessions row written")
	}
	if row.TenantID != pvTenant || row.ActorTenantID != pvHomeTenant || row.ActorUserID != pvBuilder ||
		row.ActorSessionID != pvLoginSID || row.CreatedBy != pvBuilder {
		t.Errorf("row = %+v; want tenant=K, actor_tenant=home (att, NOT K), actor_session=home login sid", row)
	}
	if row.WorkspaceSessionID == nil || *row.WorkspaceSessionID != pvWsid {
		t.Errorf("workspace_session_id = %v, want %s", row.WorkspaceSessionID, pvWsid)
	}
	if len(f.actors.calls) != 1 || f.actors.calls[0] != (actorLiveCall{pvHomeTenant, pvBuilder, pvLoginSID}) {
		t.Errorf("ActorLive calls = %+v; want (home tenant, builder, login sid)", f.actors.calls)
	}
	if len(ws.calls) != 1 || ws.calls[0] != (wsLiveCall{pvTenant, pvWsid, pvBuilder, pvHomeTenant, pvLoginSID}) {
		t.Errorf("GetLive calls = %+v; want (K, wsid, act, att, login sid)", ws.calls)
	}
	// The minted token never carries wsid, and reads as the preview of K.
	claims, err := f.tokens.VerifyPreviewToken(res.Token)
	if err != nil {
		t.Fatalf("VerifyPreviewToken: %v", err)
	}
	if claims.WorkspaceSessionID != "" || claims.TenantID != pvTenant || claims.ActorUserID != pvBuilder || claims.ActorTenantID != pvHomeTenant {
		t.Errorf("preview token claims = %+v", claims.Subject)
	}
	// Audit: written in K, by the real builder (the "(Nexus)" label is looked up from the workspace
	// session when the log is READ — no column, no stamp here).
	if len(f.auditor.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(f.auditor.events))
	}
	ev := f.auditor.events[0]
	if ev.TenantID != pvTenant || ev.ActorUserID != pvBuilder || ev.EntityName != audit.PreviewMeta || ev.Action != audit.ActionCreate {
		t.Errorf("audit event = %+v", ev)
	}
}

func TestPreviewStart_InCustomerWorkspace_Refusals(t *testing.T) {
	cases := []struct {
		name      string
		sub       func() auth.Subject
		wsErr     error
		actorErr  error
		in        PreviewStartInput
		wantCode  string // ValidationError code; "" ⇒ ErrPreviewForbidden
		wantInfra bool   // a wrapped infrastructure error, neither 403 nor validation
	}{
		{"role of another customer (K2)", workspaceBuilder, nil, nil, PreviewStartInput{RoleID: pvOtherRole}, "PREVIEW_ROLE_NOT_FOUND", false},
		{"workspace session closed / expired / customer suspended", workspaceBuilder, auth.ErrWorkspaceSessionNotLive, nil, PreviewStartInput{RoleID: pvRole}, "", false},
		{"builder no longer live", workspaceBuilder, nil, auth.ErrActorNotLive, PreviewStartInput{RoleID: pvRole}, "", false},
		{"workspace check DB error", workspaceBuilder, errors.New("db down"), nil, PreviewStartInput{RoleID: pvRole}, "", true},
		{"partial delegation claims (no att)", func() auth.Subject { s := workspaceBuilder(); s.ActorTenantID = ""; return s },
			nil, nil, PreviewStartInput{RoleID: pvRole}, "", false},
		{"partial delegation claims (att == tid)", func() auth.Subject { s := workspaceBuilder(); s.ActorTenantID = pvTenant; return s },
			nil, nil, PreviewStartInput{RoleID: pvRole}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ws := newWorkspacePreviewFixture(t)
			ws.err, f.actors.err = tc.wsErr, tc.actorErr
			_, err := f.svc.Start(context.Background(), tc.sub(), tc.in)
			var ve *ValidationError
			switch {
			case tc.wantInfra:
				if err == nil || errors.Is(err, ErrPreviewForbidden) {
					t.Fatalf("err = %v, want a wrapped infrastructure error", err)
				}
			case tc.wantCode == "":
				if !errors.Is(err, ErrPreviewForbidden) {
					t.Fatalf("err = %v, want ErrPreviewForbidden", err)
				}
			default:
				if !errors.As(err, &ve) || ve.Code != tc.wantCode {
					t.Fatalf("err = %v, want ValidationError %s", err, tc.wantCode)
				}
			}
			if f.store.created != 0 || len(f.auditor.events) != 0 {
				t.Errorf("a refused Start wrote %d rows / %d audit events", f.store.created, len(f.auditor.events))
			}
		})
	}
}

// A workspace builder subject but no GetLive wired: fail closed rather than preview without the check.
func TestPreviewStart_InCustomerWorkspace_UnwiredFailsClosed(t *testing.T) {
	f := newPreviewFixture(t)
	if _, err := f.svc.Start(context.Background(), workspaceBuilder(), PreviewStartInput{RoleID: pvRole}); !errors.Is(err, ErrPreviewForbidden) {
		t.Fatalf("err = %v, want ErrPreviewForbidden", err)
	}
}

func TestPreviewVerify_InCustomerWorkspace(t *testing.T) {
	f, ws := newWorkspacePreviewFixture(t)
	res, err := f.svc.Start(context.Background(), workspaceBuilder(), PreviewStartInput{RoleID: pvRole})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.actors.calls, ws.calls = nil, nil

	claims, err := f.svc.Verify(context.Background(), res.Token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.IsAdmin || claims.IsPlatformOperator || claims.WorkspaceSessionID != "" {
		t.Errorf("preview claims carry privilege: %+v", claims.Subject)
	}
	if claims.TenantID != pvTenant || claims.ActorTenantID != pvHomeTenant || claims.RoleID != pvRole {
		t.Errorf("claims = %+v", claims.Subject)
	}
	// Both uncached checks, both with the HOME login sid from the row (never the preview sid).
	if len(f.actors.calls) != 1 || f.actors.calls[0] != (actorLiveCall{pvHomeTenant, pvBuilder, pvLoginSID}) {
		t.Errorf("ActorLive calls = %+v", f.actors.calls)
	}
	if len(ws.calls) != 1 || ws.calls[0] != (wsLiveCall{pvTenant, pvWsid, pvBuilder, pvHomeTenant, pvLoginSID}) {
		t.Errorf("GetLive calls = %+v", ws.calls)
	}
	// No cache: a second request asks again.
	if _, err := f.svc.Verify(context.Background(), res.Token); err != nil {
		t.Fatalf("Verify #2: %v", err)
	}
	if len(ws.calls) != 2 {
		t.Errorf("GetLive calls over 2 requests = %d, want 2", len(ws.calls))
	}
}

func TestPreviewVerify_InCustomerWorkspace_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *previewFixture, ws *fakeWorkspaceLive)
	}{
		{"workspace session closed (Về Nexus) / past its hard deadline / customer suspended", func(_ *previewFixture, ws *fakeWorkspaceLive) {
			ws.err = auth.ErrWorkspaceSessionNotLive
		}},
		{"workspace check DB error fails closed", func(_ *previewFixture, ws *fakeWorkspaceLive) { ws.err = errors.New("db down") }},
		{"builder no longer live", func(f *previewFixture, _ *fakeWorkspaceLive) { f.actors.err = auth.ErrActorNotLive }},
		{"checker unwired but the row names a workspace session", func(f *previewFixture, _ *fakeWorkspaceLive) { f.svc.workspaces = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ws := newWorkspacePreviewFixture(t)
			res, err := f.svc.Start(context.Background(), workspaceBuilder(), PreviewStartInput{RoleID: pvRole})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			tc.mutate(f, ws)
			if claims, err := f.svc.Verify(context.Background(), res.Token); err == nil {
				t.Fatalf("Verify passed (%+v); want a refusal", claims.Subject)
			}
		})
	}
}

// A preview opened from the builder's HOME workshop (no wsid on the row) never asks the workspace table.
func TestPreviewVerify_HomePreviewSkipsWorkspaceCheck(t *testing.T) {
	f, ws := newWorkspacePreviewFixture(t)
	res := f.start(t)
	if _, err := f.svc.Verify(context.Background(), res.Token); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(ws.calls) != 0 {
		t.Errorf("GetLive called %d times for a home preview", len(ws.calls))
	}
}
