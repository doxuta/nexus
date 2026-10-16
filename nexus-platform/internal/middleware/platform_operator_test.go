package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

// fakeOperatorRepo is an in-memory platformOperatorRepo for the gate's unit tests.
type fakeOperatorRepo struct {
	user *auth.User
	err  error
}

func (f *fakeOperatorRepo) GetUserByID(_ context.Context, _, _ string) (*auth.User, error) {
	return f.user, f.err
}

// runGate invokes RequirePlatformOperator against repo, optionally with a verified subject in
// context, and reports whether the request was refused plus the recorder (status + body).
func runGate(repo platformOperatorRepo, withSubject bool) (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/tenants", nil)
	if withSubject {
		c.Set(ctxKeyClaims, &auth.Claims{Subject: auth.Subject{UserID: "u1", TenantID: "t1", IsAdmin: true}})
	}
	RequirePlatformOperator(repo, nil)(c)
	return w, c
}

func TestRequirePlatformOperator_ActiveOperatorPasses(t *testing.T) {
	repo := &fakeOperatorRepo{user: &auth.User{IsAdmin: true, IsPlatformOperator: true, Status: "active"}}
	w, c := runGate(repo, true)
	if c.IsAborted() {
		t.Fatalf("active operator was refused (status %d) — want pass-through", w.Code)
	}
}

func TestRequirePlatformOperator_Refusals(t *testing.T) {
	// A 403 body BYTE-IDENTICAL to RequireAdmin (anti-enumeration).
	const want403Body = `{"code":"FORBIDDEN","error":"Yêu cầu quyền quản trị viên"}`
	cases := []struct {
		name string
		repo *fakeOperatorRepo
		sub  bool
		code int
	}{
		{"not admin", &fakeOperatorRepo{user: &auth.User{IsAdmin: false, IsPlatformOperator: true, Status: "active"}}, true, http.StatusForbidden},
		{"flag off", &fakeOperatorRepo{user: &auth.User{IsAdmin: true, IsPlatformOperator: false, Status: "active"}}, true, http.StatusForbidden},
		{"disabled", &fakeOperatorRepo{user: &auth.User{IsAdmin: true, IsPlatformOperator: true, Status: "disabled"}}, true, http.StatusForbidden},
		{"no subject", &fakeOperatorRepo{user: &auth.User{IsAdmin: true, IsPlatformOperator: true, Status: "active"}}, false, http.StatusForbidden},
		{"deleted operator (ErrUserNotFound)", &fakeOperatorRepo{err: auth.ErrUserNotFound}, true, http.StatusForbidden},
		{"repo error fails closed", &fakeOperatorRepo{err: errors.New("db down")}, true, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, c := runGate(tc.repo, tc.sub)
			if !c.IsAborted() {
				t.Fatalf("request was NOT refused — want abort with %d", tc.code)
			}
			if w.Code != tc.code {
				t.Errorf("status = %d, want %d", w.Code, tc.code)
			}
			// The 403 (authorization refusal) must be byte-identical to RequireAdmin so a
			// tenant-admin cannot distinguish this route from any other admin route it lacks.
			if tc.code == http.StatusForbidden && w.Body.String() != want403Body {
				t.Errorf("403 body = %s, want %s (byte-identical to RequireAdmin)", w.Body.String(), want403Body)
			}
		})
	}
}
