package middleware

// workspace_session_test.go — specs/062-customer-workspace T010. RefuseActingSession closes the
// self-management surface (password, sessions, MFA) and the nested-open route to a builder
// workspace-session token: those acts belong to the builder's HOME account, not to a session
// that is only a visit inside a customer.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

func TestRefuseActingSession(t *testing.T) {
	cases := []struct {
		name     string
		sub      auth.Subject
		wantPass bool
	}{
		{"workspace session token is refused", sessionSubject(true), false},
		{"home token passes", homeSubject(), true},
		{"preview-shaped subject without wsid passes (PreviewReadOnly owns it)", auth.Subject{UserID: "p", TenantID: "t", ActorUserID: "b", ActorTenantID: "t"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/auth/me/password", nil)
			c.Set(ctxKeyClaims, &auth.Claims{Subject: tc.sub})
			RefuseActingSession()(c)
			if tc.wantPass {
				if c.IsAborted() {
					t.Fatalf("refused (%d %s), want pass-through", w.Code, w.Body.String())
				}
				return
			}
			if !c.IsAborted() || w.Code != http.StatusForbidden {
				t.Fatalf("aborted=%v status=%d, want 403", c.IsAborted(), w.Code)
			}
			if want := `{"code":"WORKSPACE_SESSION_NOT_ALLOWED","error":"Không thực hiện được khi đang dựng trong không gian của khách — hãy về Nexus để làm việc này"}`; w.Body.String() != want {
				t.Errorf("body = %s, want %s", w.Body.String(), want)
			}
		})
	}
}
