package handlers

import (
	"context"
	"net/http"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// specs/065 lát C (T057/T058): owner create / invite link / sessions. Fakes pin the audit actor.

type fakeInviteIssuer struct {
	actor, tenant, user string
}

func (f *fakeInviteIssuer) IssueResetLinkForUser(ctx context.Context, tenantID, userID string) (*services.ResetLinkIssue, error) {
	f.actor, f.tenant, f.user = audit.ActorFromContext(ctx), tenantID, userID
	if userID == "inactive" {
		return nil, &services.ValidationError{Code: "USER_NOT_ACTIVE", Msg: "không active"}
	}
	return &services.ResetLinkIssue{ResetURL: "http://x/reset?token=t", Token: "t", ExpiresAt: time.Now(), TTLMinutes: 30}, nil
}

type fakeSessions struct {
	endActor, endUser, endSID string
	listUser                  string
}

func (f *fakeSessions) ListSessions(_ context.Context, _, userID, _ string) ([]services.SessionView, error) {
	f.listUser = userID
	return []services.SessionView{{ID: "s1"}}, nil
}

func (f *fakeSessions) EndSession(ctx context.Context, _, userID, sid string) error {
	f.endActor, f.endUser, f.endSID = audit.ActorFromContext(ctx), userID, sid
	if sid != "s1" {
		return &services.ValidationError{Code: "SESSION_NOT_FOUND", Msg: "không có phiên"}
	}
	return nil
}

func newInviteFixture() (*orgFixture, *fakeInviteIssuer, *fakeSessions) {
	f := newOrgFixture()
	iss, ses := &fakeInviteIssuer{}, &fakeSessions{}
	f.org.WithResetLinkIssuer(iss).WithSessionManager(ses)
	f.usr.WithSessionManager(ses)
	return f, iss, ses
}

func TestOrgAdminCreate_IgnoresPrivilegedKeysAndAttributesOwner(t *testing.T) {
	f, _, _ := newInviteFixture()
	rec := f.do("owner", http.MethodPost, "/org-admin/users", map[string]any{
		"email": "new@acme.test", "fullName": "Mới", "roleId": "r-sales",
		"password": "Hacked-Pass-1!", "isAdmin": true, "canManageUsers": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var created *authUserRow
	for _, u := range f.repo.rows {
		if u.Email == "new@acme.test" {
			created = &authUserRow{u.IsAdmin, u.CanManageUsers, u.PasswordHash}
		}
	}
	if created == nil || created.admin || created.owner || created.hash == "" || created.hash == "Hacked-Pass-1!" {
		t.Fatalf("created = %+v", created)
	}
	if n := len(f.aud.events); n != 1 || f.aud.events[0].ActorUserID != "owner" || f.aud.events[0].Action != audit.ActionCreate {
		t.Fatalf("audit = %+v", f.aud.events)
	}
	if body := decodeBody(t, rec)["data"].(map[string]any)["user"].(map[string]any); body["tier"] != "staff" {
		t.Errorf("user = %v", body)
	}
}

func authUser(id, status string) *auth.User {
	return &auth.User{ID: id, TenantID: "tenant-1", Email: id + "@acme.test", Status: status}
}

type authUserRow struct {
	admin, owner bool
	hash         string
}

func TestOrgAdminInviteAndSessions_Codes(t *testing.T) {
	cases := []struct {
		name         string
		actor        string
		method, path string
		body         any
		code         int
		errCode      string
	}{
		{"create duplicate 409", "owner", http.MethodPost, "/org-admin/users", map[string]any{"email": "staff@acme.test"}, 409, "USER_ALREADY_EXISTS"},
		{"create empty email 422", "owner", http.MethodPost, "/org-admin/users", map[string]any{"email": ""}, 422, "USER_FIELD_REQUIRED"},
		{"create by staff 403", "staff", http.MethodPost, "/org-admin/users", map[string]any{"email": "z@acme.test"}, 403, "FORBIDDEN"},
		{"link staff 201", "owner", http.MethodPost, "/org-admin/users/staff/reset-password-link", nil, 201, ""},
		{"link admin 403", "owner", http.MethodPost, "/org-admin/users/admin/reset-password-link", nil, 403, "USER_NOT_MANAGEABLE"},
		{"link unknown 404", "owner", http.MethodPost, "/org-admin/users/nope/reset-password-link", nil, 404, "USER_NOT_FOUND"},
		{"link inactive 422", "owner", http.MethodPost, "/org-admin/users/inactive/reset-password-link", nil, 422, "USER_NOT_ACTIVE"},
		{"link by staff 403", "staff", http.MethodPost, "/org-admin/users/staff/reset-password-link", nil, 403, "FORBIDDEN"},
		{"sessions staff 200", "owner", http.MethodGet, "/org-admin/users/staff/sessions", nil, 200, ""},
		{"sessions admin 403", "owner", http.MethodGet, "/org-admin/users/admin/sessions", nil, 403, "USER_NOT_MANAGEABLE"},
		{"sessions by staff 403", "staff", http.MethodGet, "/org-admin/users/staff/sessions", nil, 403, "FORBIDDEN"},
		{"end ok 204", "owner", http.MethodDelete, "/org-admin/users/staff/sessions/s1", nil, 204, ""},
		{"end other's session 404", "owner", http.MethodDelete, "/org-admin/users/staff/sessions/zz", nil, 404, "SESSION_NOT_FOUND"},
		{"end admin's session 403", "owner", http.MethodDelete, "/org-admin/users/admin/sessions/s1", nil, 403, "USER_NOT_MANAGEABLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _, _ := newInviteFixture()
			// an inactive (locked, still manageable) account for the USER_NOT_ACTIVE row
			f.repo.rows[f.repo.key("tenant-1", "inactive")] = authUser("inactive", "inactive")
			rec := f.do(tc.actor, tc.method, tc.path, tc.body)
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.code, rec.Body.String())
			}
			if tc.errCode != "" && decodeBody(t, rec)["code"] != tc.errCode {
				t.Fatalf("code = %v, want %s", decodeBody(t, rec)["code"], tc.errCode)
			}
		})
	}
}

// FR-036: the owner is the audit actor of the mint and of the session end; refused calls reach nothing.
func TestOrgAdmin_PinsActorAndDoesNotCallOnRefusal(t *testing.T) {
	f, iss, ses := newInviteFixture()
	if rec := f.do("owner", http.MethodPost, "/org-admin/users/staff/reset-password-link", nil); rec.Code != 201 {
		t.Fatalf("link: %d", rec.Code)
	}
	if iss.actor != "owner" || iss.user != "staff" || iss.tenant != "tenant-1" {
		t.Errorf("issuer saw %+v", iss)
	}
	if rec := f.do("owner", http.MethodDelete, "/org-admin/users/staff/sessions/s1", nil); rec.Code != 204 {
		t.Fatalf("end: %d", rec.Code)
	}
	if ses.endActor != "owner" || ses.endUser != "staff" || ses.endSID != "s1" {
		t.Errorf("sessions saw %+v", ses)
	}
	*iss, *ses = fakeInviteIssuer{}, fakeSessions{}
	f.do("owner", http.MethodPost, "/org-admin/users/admin/reset-password-link", nil)
	f.do("owner", http.MethodGet, "/org-admin/users/admin/sessions", nil)
	f.do("owner", http.MethodDelete, "/org-admin/users/admin/sessions/s1", nil)
	if iss.user != "" || ses.listUser != "" || ses.endUser != "" {
		t.Errorf("a refused call reached the service: %+v %+v", iss, ses)
	}
}

// #311: the admin route now hands the end-session service a context carrying the admin as actor.
func TestAdminEndSession_PassesActor(t *testing.T) {
	f, _, ses := newInviteFixture()
	if rec := f.do("admin", http.MethodDelete, "/admin/users/staff/sessions/s1", nil); rec.Code != 204 {
		t.Fatalf("status %d", rec.Code)
	}
	if ses.endActor != "admin" {
		t.Errorf("actor = %q, want admin", ses.endActor)
	}
}
