package handlers

// router_ai_builder_gate_test.go — specs/067-ai-context FR-080: the builder chat routes (with their
// new `context` block) sit behind the REAL builderGated chain: non-admin and customer-admin
// (is_admin, not operator) are refused 403 before the handler runs; only the builder-tier operator
// reaches it. Handlers are real, the agent is a counting fake.
//
// The session-token case (wsid + IsPlatformOperator) of specs/062-customer-workspace L1a
// (RequireBuilderTier) is NOT here yet: that middleware is not on main, and a skipped test would be
// a fake green. The contract line lives in that spec's tasks (L1a).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
)

func TestRouter_AIBuilderChat_BuilderGated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens := map[string]*auth.Claims{
		"tok-user":  {Subject: auth.Subject{UserID: "u-user", TenantID: "t1", RoleID: "r1", IsAdmin: false}, TokenType: auth.TokenTypeAccess},
		"tok-admin": {Subject: auth.Subject{UserID: "u-adm", TenantID: "t1", RoleID: "r1", IsAdmin: true}, TokenType: auth.TokenTypeAccess},
		"tok-op":    {Subject: auth.Subject{UserID: "u-op", TenantID: "t1", RoleID: "r1", IsAdmin: true}, TokenType: auth.TokenTypeAccess},
	}
	agent := &fakeBuilderAgent{res: okResult()}
	res := &fakeCtxResolver{turn: goodTurn()}
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), &stubVerifier{byToken: tokens}, nil, APIHandlers{
		AI: NewAIBuilderHandler(agent, newFakeStore(), nil, res, zap.NewNop()),
	}, zap.NewNop(),
		WithOperatorTier(middleware.RequirePlatformOperator(&operatorRepoFake{operatorID: "u-op"}, zap.NewNop())),
	)

	post := func(path, token string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"message":"hi","context":{"app":"clinic","panel":"bang"}}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	for _, path := range chatPaths {
		for _, tok := range []string{"tok-user", "tok-admin"} {
			agent.called, res.calls = false, 0
			if code := post(path, tok); code != http.StatusForbidden || agent.called || res.calls != 0 {
				t.Fatalf("%s as %s: code=%d agent=%v resolver=%d; want 403 and an untouched handler", path, tok, code, agent.called, res.calls)
			}
		}
		agent.called, res.calls = false, 0
		if code := post(path, "tok-op"); code != http.StatusOK || !agent.called || res.calls != 1 {
			t.Fatalf("%s as operator: code=%d agent=%v resolver=%d; want 200 through the gate", path, code, agent.called, res.calls)
		}
	}
}
