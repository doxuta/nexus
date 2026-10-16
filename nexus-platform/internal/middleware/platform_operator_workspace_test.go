package middleware

// platform_operator_workspace_test.go — specs/062-customer-workspace T009. Two gates around the
// builder workspace-session token:
//
//   - RequirePlatformOperator (/admin/tenants*, /admin/projects*) refuses a session token BEFORE
//     reading any repo: in a session the Subject's tenant is the customer, so the live-row read
//     would be pinned to the wrong tenant (self-suspend / provisioning traps) — never reachable.
//   - RequireBuilderTier (builderGated + purgeGroup) lets a session token through (the resolver
//     in this very request already confirmed the builder), and behaves exactly like
//     RequirePlatformOperator for a home token.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

const forbiddenBody = `{"code":"FORBIDDEN","error":"Yêu cầu quyền quản trị viên"}`

type countingOperatorRepo struct {
	user  *auth.User
	err   error
	calls int
}

func (f *countingOperatorRepo) GetUserByID(_ context.Context, _, _ string) (*auth.User, error) {
	f.calls++
	return f.user, f.err
}

func runTierGate(gate func(platformOperatorRepo) gin.HandlerFunc, repo platformOperatorRepo, sub auth.Subject) (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	c.Set(ctxKeyClaims, &auth.Claims{Subject: sub})
	gate(repo)(c)
	return w, c
}

func sessionSubject(operator bool) auth.Subject {
	return auth.Subject{
		UserID: "builder-1", TenantID: "customer-1", IsAdmin: true, SessionID: "sid",
		ActorUserID: "builder-1", ActorTenantID: "home-1", WorkspaceSessionID: "ws-1", IsPlatformOperator: operator,
	}
}

func homeSubject() auth.Subject { return auth.Subject{UserID: "u1", TenantID: "t1", IsAdmin: true} }

func operatorUser() *auth.User {
	return &auth.User{IsAdmin: true, IsPlatformOperator: true, Status: "active"}
}

func TestRequirePlatformOperator_RefusesWorkspaceSessionBeforeReadingRepo(t *testing.T) {
	repo := &countingOperatorRepo{user: operatorUser()} // an operator row, to prove it is NOT the repo that refuses
	gate := func(r platformOperatorRepo) gin.HandlerFunc { return RequirePlatformOperator(r, nil) }
	w, c := runTierGate(gate, repo, sessionSubject(true))
	if !c.IsAborted() || w.Code != http.StatusForbidden {
		t.Fatalf("aborted=%v status=%d, want 403", c.IsAborted(), w.Code)
	}
	if w.Body.String() != forbiddenBody {
		t.Errorf("body = %s, want byte-identical to RequireAdmin: %s", w.Body.String(), forbiddenBody)
	}
	if repo.calls != 0 {
		t.Errorf("repo read %d time(s) — the session refusal must come first", repo.calls)
	}
}

func TestRequireBuilderTier(t *testing.T) {
	cases := []struct {
		name      string
		sub       auth.Subject
		repo      *countingOperatorRepo
		wantPass  bool
		wantCode  int
		wantReads int
	}{
		{"workspace session with operator flag passes without a repo read", sessionSubject(true), &countingOperatorRepo{user: operatorUser()}, true, 0, 0},
		{"wsid but IsPlatformOperator false is refused byte-identical", sessionSubject(false), &countingOperatorRepo{user: operatorUser()}, false, http.StatusForbidden, 0},
		{"home operator passes (live row read)", homeSubject(), &countingOperatorRepo{user: operatorUser()}, true, 0, 1},
		{"home non-operator refused byte-identical", homeSubject(), &countingOperatorRepo{user: &auth.User{IsAdmin: true, Status: "active"}}, false, http.StatusForbidden, 1},
		{"home deleted operator refused", homeSubject(), &countingOperatorRepo{err: auth.ErrUserNotFound}, false, http.StatusForbidden, 1},
		{"repo error fails closed", homeSubject(), &countingOperatorRepo{err: errors.New("db down")}, false, http.StatusInternalServerError, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := func(r platformOperatorRepo) gin.HandlerFunc { return RequireBuilderTier(r, nil) }
			w, c := runTierGate(gate, tc.repo, tc.sub)
			if tc.repo.calls != tc.wantReads {
				t.Errorf("repo reads = %d, want %d", tc.repo.calls, tc.wantReads)
			}
			if tc.wantPass {
				if c.IsAborted() {
					t.Fatalf("refused (%d %s), want pass-through", w.Code, w.Body.String())
				}
				return
			}
			if !c.IsAborted() || w.Code != tc.wantCode {
				t.Fatalf("aborted=%v status=%d, want abort %d", c.IsAborted(), w.Code, tc.wantCode)
			}
			if tc.wantCode == http.StatusForbidden && w.Body.String() != forbiddenBody {
				t.Errorf("body = %s, want %s", w.Body.String(), forbiddenBody)
			}
		})
	}
}
