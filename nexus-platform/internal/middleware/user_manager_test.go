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

// countingRepo is a platformOperatorRepo that records how often the gate read the live row.
type countingRepo struct {
	user  *auth.User
	err   error
	calls int
}

func (f *countingRepo) GetUserByID(_ context.Context, _, _ string) (*auth.User, error) {
	f.calls++
	return f.user, f.err
}

func runUserManagerGate(repo platformOperatorRepo, claims *auth.Claims) (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/org-admin/users", nil)
	if claims != nil {
		c.Set(ctxKeyClaims, claims)
	}
	RequireUserManager(repo, nil)(c)
	return w, c
}

func homeClaims() *auth.Claims {
	return &auth.Claims{TokenType: auth.TokenTypeAccess, Subject: auth.Subject{UserID: "u1", TenantID: "t1"}}
}

func TestRequireUserManager_ActiveOwnerPasses(t *testing.T) {
	repo := &countingRepo{user: &auth.User{CanManageUsers: true, Status: "active"}}
	w, c := runUserManagerGate(repo, homeClaims())
	if c.IsAborted() {
		t.Fatalf("active owner refused (status %d)", w.Code)
	}
}

// Same fixed body as RequireAdmin: an owner who loses the flag cannot tell this route from any other.
func TestRequireUserManager_Refusals(t *testing.T) {
	const want403Body = `{"code":"FORBIDDEN","error":"Yêu cầu quyền quản trị viên"}`
	apiKey := homeClaims()
	apiKey.TokenType = auth.TokenTypeAPIKey
	preview := homeClaims()
	preview.TokenType = auth.TokenTypePreview
	wsSession := homeClaims()
	wsSession.WorkspaceSessionID = "ws-1"
	owner := &auth.User{CanManageUsers: true, Status: "active"}

	cases := []struct {
		name      string
		repo      *countingRepo
		claims    *auth.Claims
		code      int
		wantReads int
	}{
		{"no claims", &countingRepo{user: owner}, nil, 403, 0},
		{"api key token", &countingRepo{user: owner}, apiKey, 403, 0},
		{"preview token", &countingRepo{user: owner}, preview, 403, 0},
		{"workspace-session token", &countingRepo{user: owner}, wsSession, 403, 0},
		{"user gone", &countingRepo{err: auth.ErrUserNotFound}, homeClaims(), 403, 1},
		{"repo error fails closed", &countingRepo{err: errors.New("db down")}, homeClaims(), 500, 1},
		{"inactive owner", &countingRepo{user: &auth.User{CanManageUsers: true, Status: "inactive"}}, homeClaims(), 403, 1},
		{"admin without the flag", &countingRepo{user: &auth.User{IsAdmin: true, Status: "active"}}, homeClaims(), 403, 1},
		{"plain staff", &countingRepo{user: &auth.User{Status: "active"}}, homeClaims(), 403, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, c := runUserManagerGate(tc.repo, tc.claims)
			if !c.IsAborted() {
				t.Fatal("request not refused")
			}
			if w.Code != tc.code {
				t.Errorf("status = %d, want %d", w.Code, tc.code)
			}
			if tc.repo.calls != tc.wantReads {
				t.Errorf("repo reads = %d, want %d", tc.repo.calls, tc.wantReads)
			}
			if tc.code == 403 && w.Body.String() != want403Body {
				t.Errorf("403 body = %s, want %s", w.Body.String(), want403Body)
			}
		})
	}
}

// Read fresh: flipping the flag between two requests takes effect on the second one.
func TestRequireUserManager_FlagFlipBitesNextRequest(t *testing.T) {
	repo := &countingRepo{user: &auth.User{CanManageUsers: true, Status: "active"}}
	if _, c := runUserManagerGate(repo, homeClaims()); c.IsAborted() {
		t.Fatal("first request refused")
	}
	repo.user = &auth.User{CanManageUsers: false, Status: "active"}
	if w, c := runUserManagerGate(repo, homeClaims()); !c.IsAborted() || w.Code != 403 {
		t.Fatalf("second request after the flag was cleared: aborted=%v status=%d", c.IsAborted(), w.Code)
	}
}
