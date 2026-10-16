package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// recordingAuditor keeps every audit event so a test can pin WHO the row is attributed to.
type recordingAuditor struct{ events []audit.Event }

func (r *recordingAuditor) Record(_ context.Context, e audit.Event) { r.events = append(r.events, e) }

// orgRoles is an in-memory role store satisfying the service's role validator + lister.
type orgRoles struct{ roles map[string]*auth.Role }

func (o *orgRoles) GetRoleByID(_ context.Context, tenantID, id string) (*auth.Role, error) {
	if r, ok := o.roles[id]; ok && r.TenantID == tenantID {
		return r, nil
	}
	return nil, auth.ErrRoleNotFound
}

func (o *orgRoles) ListRoles(_ context.Context, tenantID string) ([]auth.Role, error) {
	var out []auth.Role
	for _, r := range o.roles {
		if r.TenantID == tenantID {
			out = append(out, *r)
		}
	}
	return out, nil
}

type orgFixture struct {
	repo *fakeUserCRUDRepo
	aud  *recordingAuditor
	org  *OrgAdminHandler
	usr  *AdminUserHandler
}

func newOrgFixture() *orgFixture {
	repo := newFakeUserCRUDRepo()
	roles := &orgRoles{roles: map[string]*auth.Role{
		"r-sales": {ID: "r-sales", TenantID: "tenant-1", Name: "sales", Label: "Bán hàng"},
		"r-owner": {ID: "r-owner", TenantID: "tenant-1", Name: auth.OwnerRoleName},
	}}
	aud := &recordingAuditor{}
	svc := services.NewUserService(repo).WithRoleValidator(roles).WithRoleLister(roles).WithAuditor(aud)
	put := func(id string, mut func(*auth.User)) {
		u := &auth.User{ID: id, TenantID: "tenant-1", Email: id + "@acme.test", FullName: "N " + id, Status: "active"}
		if mut != nil {
			mut(u)
		}
		repo.rows[repo.key("tenant-1", id)] = u
	}
	put("owner", func(u *auth.User) { u.CanManageUsers = true })
	put("staff", nil)
	put("admin", func(u *auth.User) { u.IsAdmin = true })
	put("op", func(u *auth.User) { u.IsAdmin = true; u.IsPlatformOperator = true })
	put("boss", func(u *auth.User) { u.IsAdmin = true }) // admin, NOT operator
	return &orgFixture{repo: repo, aud: aud, org: NewOrgAdminHandler(svc, zap.NewNop()), usr: NewAdminUserHandler(svc, zap.NewNop())}
}

func (f *orgFixture) do(actor, method, path string, body any) *httptest.ResponseRecorder {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{UserID: actor, TenantID: "tenant-1", IsAdmin: actor == "op" || actor == "boss" || actor == "admin"}, TokenType: auth.TokenTypeAccess})
		c.Set("tenantID", "tenant-1")
		c.Next()
	})
	g := r.Group("")
	f.org.RegisterRoutes(g)
	f.usr.RegisterRoutes(g)
	f.usr.RegisterBuilderRoutes(g)
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, rec.Body.String())
	}
	return m
}

// Keys the contract does not list are dropped by construction: an owner cannot smuggle isAdmin,
// canManageUsers or a password through the PUT.
func TestOrgAdminUpdate_ExtraKeysAreIgnored(t *testing.T) {
	f := newOrgFixture()
	rec := f.do("owner", http.MethodPut, "/org-admin/users/staff", map[string]any{
		"fullName": "Tên mới", "isAdmin": true, "canManageUsers": true, "password": "Hacked-Pass-1!", "roleId": "r-sales",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	row := f.repo.rows[f.repo.key("tenant-1", "staff")]
	if row.IsAdmin || row.CanManageUsers || row.PasswordHash != "" {
		t.Fatalf("a forbidden key was applied: %+v", row)
	}
	if row.FullName != "Tên mới" || row.RoleID == nil || *row.RoleID != "r-sales" {
		t.Fatalf("allowed keys not applied: %+v", row)
	}
	user := decodeBody(t, rec)["data"].(map[string]any)["user"].(map[string]any)
	for _, k := range []string{"id", "email", "fullName", "status", "roleId", "tier", "manageable"} {
		if _, ok := user[k]; !ok {
			t.Errorf("camelCase key %q missing: %v", k, user)
		}
	}
	if user["tier"] != "staff" || user["manageable"] != true {
		t.Errorf("tier/manageable = %v/%v", user["tier"], user["manageable"])
	}
}

func TestOrgAdmin_StatusCodes(t *testing.T) {
	cases := []struct {
		name         string
		actor        string
		method, path string
		body         any
		code         int
		errCode      string
	}{
		{"list ok", "owner", http.MethodGet, "/org-admin/users", nil, 200, ""},
		{"get ok", "owner", http.MethodGet, "/org-admin/users/staff", nil, 200, ""},
		{"roles ok", "owner", http.MethodGet, "/org-admin/roles", nil, 200, ""},
		{"get unknown 404", "owner", http.MethodGet, "/org-admin/users/nope", nil, 404, "USER_NOT_FOUND"},
		{"put unknown 404", "owner", http.MethodPut, "/org-admin/users/nope", map[string]any{"fullName": "x"}, 404, "USER_NOT_FOUND"},
		{"put admin target 403", "owner", http.MethodPut, "/org-admin/users/admin", map[string]any{"fullName": "x"}, 403, "USER_NOT_MANAGEABLE"},
		{"put self 403", "owner", http.MethodPut, "/org-admin/users/owner", map[string]any{"fullName": "x"}, 403, "USER_NOT_MANAGEABLE"},
		{"owner role 422", "owner", http.MethodPut, "/org-admin/users/staff", map[string]any{"roleId": "r-owner"}, 422, "ROLE_NOT_ASSIGNABLE"},
		{"unknown role 422", "owner", http.MethodPut, "/org-admin/users/staff", map[string]any{"roleId": "zzz"}, 422, "ROLE_INVALID"},
		{"disabled status 422", "owner", http.MethodPut, "/org-admin/users/staff", map[string]any{"status": "disabled"}, 422, "USER_STATUS_INVALID"},
		{"non-owner actor 403", "staff", http.MethodGet, "/org-admin/users", nil, 403, "FORBIDDEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrgFixture()
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

// FR-036 / SC-012: the audit row of an owner's write names the OWNER, not an empty actor.
func TestOrgAdminUpdate_AuditActorIsTheCaller(t *testing.T) {
	f := newOrgFixture()
	if rec := f.do("owner", http.MethodPut, "/org-admin/users/staff", map[string]any{"fullName": "A"}); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if len(f.aud.events) != 1 || f.aud.events[0].ActorUserID != "owner" || f.aud.events[0].RecordID != "staff" {
		t.Fatalf("audit events = %+v", f.aud.events)
	}
}

func TestSetUserManager_Handler(t *testing.T) {
	t.Run("operator switches it on; audit actor is the operator", func(t *testing.T) {
		f := newOrgFixture()
		rec := f.do("op", http.MethodPut, "/admin/users/staff/user-manager", map[string]any{"enabled": true})
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		user := decodeBody(t, rec)["data"].(map[string]any)["user"].(map[string]any)
		if user["canManageUsers"] != true {
			t.Fatalf("user = %v", user)
		}
		if len(f.aud.events) != 1 || f.aud.events[0].ActorUserID != "op" {
			t.Fatalf("audit events = %+v", f.aud.events)
		}
	})
	t.Run("admin who is not an operator is refused with the FORBIDDEN body", func(t *testing.T) {
		f := newOrgFixture()
		rec := f.do("boss", http.MethodPut, "/admin/users/staff/user-manager", map[string]any{"enabled": true})
		const want = `{"code":"FORBIDDEN","error":"Yêu cầu quyền quản trị viên"}`
		if rec.Code != 403 || rec.Body.String() != want {
			t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
		}
		if f.repo.rows[f.repo.key("tenant-1", "staff")].CanManageUsers {
			t.Fatal("flag written by a non-operator")
		}
	})
	t.Run("admin target is 422 USER_TIER_CONFLICT", func(t *testing.T) {
		f := newOrgFixture()
		rec := f.do("op", http.MethodPut, "/admin/users/admin/user-manager", map[string]any{"enabled": true})
		if rec.Code != 422 || decodeBody(t, rec)["code"] != "USER_TIER_CONFLICT" {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("missing enabled is 400", func(t *testing.T) {
		f := newOrgFixture()
		if rec := f.do("op", http.MethodPut, "/admin/users/staff/user-manager", map[string]any{}); rec.Code != 400 {
			t.Fatalf("status %d", rec.Code)
		}
	})
}

// D-14: ONE helper answers "is the caller an operator" for every caller shape.
func TestCallerIsOperator(t *testing.T) {
	f := newOrgFixture()
	cases := []struct {
		name   string
		claims *auth.Claims
		want   bool
	}{
		{"home token of an operator (read fresh)", &auth.Claims{TokenType: auth.TokenTypeAccess, Subject: auth.Subject{UserID: "op", TenantID: "tenant-1"}}, true},
		{"home token of an admin without the flag", &auth.Claims{TokenType: auth.TokenTypeAccess, Subject: auth.Subject{UserID: "boss", TenantID: "tenant-1"}}, false},
		{"workspace session of a confirmed builder (no row read)", &auth.Claims{TokenType: auth.TokenTypeAccess, Subject: auth.Subject{UserID: "ghost", TenantID: "tenant-1", WorkspaceSessionID: "ws", IsPlatformOperator: true}}, true},
		{"workspace session without the confirmed flag", &auth.Claims{TokenType: auth.TokenTypeAccess, Subject: auth.Subject{UserID: "op", TenantID: "tenant-1", WorkspaceSessionID: "ws"}}, false},
		{"preview token", &auth.Claims{TokenType: auth.TokenTypePreview, Subject: auth.Subject{UserID: "op", TenantID: "tenant-1"}}, false},
		{"api key", &auth.Claims{TokenType: auth.TokenTypeAPIKey, Subject: auth.Subject{UserID: "op", TenantID: "tenant-1"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			c.Set("authClaims", tc.claims)
			got, err := f.usr.callerIsOperator(c)
			if err != nil || got != tc.want {
				t.Fatalf("got %v err %v, want %v", got, err, tc.want)
			}
		})
	}
}

// B2 on the routes that reach into another account: reset link, session list, session end.
func TestAdminUserRoutes_OperatorGuard(t *testing.T) {
	f := newOrgFixture()
	f.usr.WithResetLinkIssuer(&orgStubIssuer{}).WithSessionManager(&orgStubSessions{})
	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/admin/users/op/reset-password-link"},
		{http.MethodGet, "/admin/users/op/sessions"},
		{http.MethodDelete, "/admin/users/op/sessions/s1"},
		{http.MethodPut, "/admin/users/op"},
		{http.MethodDelete, "/admin/users/op"},
	} {
		rec := f.do("boss", rt.method, rt.path, map[string]any{"fullName": "x"})
		if rec.Code != 403 || decodeBody(t, rec)["code"] != "USER_NOT_MANAGEABLE" {
			t.Errorf("%s %s by a non-operator admin: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
	if orgStubIssuerCalls+stubSessionCalls != 0 {
		t.Fatalf("the reset issuer / session manager ran for a refused call (%d/%d)", orgStubIssuerCalls, stubSessionCalls)
	}
	// an operator, and any admin on a plain account, still pass
	if rec := f.do("op", http.MethodGet, "/admin/users/op/sessions", nil); rec.Code != 200 {
		t.Errorf("operator on operator: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do("boss", http.MethodGet, "/admin/users/staff/sessions", nil); rec.Code != 200 {
		t.Errorf("admin on staff: %d %s", rec.Code, rec.Body.String())
	}
}

var orgStubIssuerCalls, stubSessionCalls int

type orgStubIssuer struct{}

func (*orgStubIssuer) IssueResetLinkForUser(context.Context, string, string) (*services.ResetLinkIssue, error) {
	orgStubIssuerCalls++
	return &services.ResetLinkIssue{}, nil
}

type orgStubSessions struct{}

func (*orgStubSessions) ListSessions(context.Context, string, string, string) ([]services.SessionView, error) {
	return []services.SessionView{}, nil
}

func (*orgStubSessions) EndSession(context.Context, string, string, string) error {
	stubSessionCalls++
	return nil
}
