//go:build integration

// specs/067-ai-context L10-1 (T012): the context refusal path over a REAL migrated MySQL — the
// real ContextResolver on the real AppService / metadata repo / user repo, the real handler and the
// real SessionStore; only the agent and the budget guard are fakes WITH counters. (With a fake
// agent, counting sys_ai_runs would always be 0 and prove nothing — the counters are the proof.)
//
// Run: go test -tags=integration ./internal/handlers/ -run TestAIContext_Integration
package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

const (
	aiCtxTenantA = "aic00000-0000-0000-0000-00000000000a"
	aiCtxTenantB = "aic00000-0000-0000-0000-00000000000b"
)

func TestAIContext_Integration_CrossTenantAppIs422ByteIdenticalAndFree(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	for _, tn := range []struct{ id, slug, name string }{
		{aiCtxTenantA, "aic-a", "Dự án A"}, {aiCtxTenantB, "aic-b", "Dự án B"},
	} {
		if _, err := sx.Exec(`INSERT INTO tenants (id, slug, name, status) VALUES (?, ?, ?, 'active')`, tn.id, tn.slug, tn.name); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	meta := metadata.NewRepository(gdb)
	appSvc := services.NewAppService(appmeta.NewSQLRepository(sx), meta, dashboard.NewSQLRepository(sx), nil, zap.NewNop())
	ctx := context.Background()
	appA, err := appSvc.CreateApp(ctx, aiCtxTenantA, services.CreateAppInput{Name: "clinic_a", Label: "Phòng khám A"})
	if err != nil {
		t.Fatalf("create app A: %v", err)
	}
	// One table in app A's menu: the resolver must hand the AI its real name via the real GetNav.
	if _, err := sx.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name) VALUES (?, ?, 'patients', 'Bệnh nhân', 'obj_patients')`,
		"aic-ent-patients", aiCtxTenantA); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := appSvc.ReplaceNav(ctx, aiCtxTenantA, appA.ID, []services.AppNavItemInput{{ItemType: "entity", RefID: "aic-ent-patients"}}); err != nil {
		t.Fatalf("replace nav: %v", err)
	}
	if _, err := appSvc.CreateApp(ctx, aiCtxTenantB, services.CreateAppInput{Name: "secret_b", Label: "Bí mật B"}); err != nil {
		t.Fatalf("create app B: %v", err)
	}

	agent := &fakeBuilderAgent{res: okResult()}
	guard := &fakeTurnGuard{}
	resolver := ai.NewContextResolver(meta, appSvc, auth.NewUserRepository(gdb))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", aiCtxTenantA)
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{UserID: "u-a", TenantID: aiCtxTenantA, RoleID: "r1", IsAdmin: true}})
		c.Next()
	})
	grp := r.Group("/api/v1")
	grp.Use(middleware.RequireAdmin(nil))
	NewAIBuilderHandler(agent, ai.NewSessionStore(sx), guard, resolver, zap.NewNop()).RegisterRoutes(grp)

	sessions := func() int {
		var n int
		if err := sx.Get(&n, `SELECT COUNT(*) FROM sys_ai_sessions WHERE tenant_id = ?`, aiCtxTenantA); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		return n
	}
	post := func(path, app string) (int, string) {
		body := chatBody("", "hi")
		body["context"] = map[string]any{"app": app}
		rec := doJSON(r, http.MethodPost, path, body)
		return rec.Code, rec.Body.String()
	}

	for _, path := range chatPaths {
		cCode, cBody := post(path, "secret_b") // an app that exists — in the OTHER tenant
		mCode, mBody := post(path, "no_such_app")
		if cCode != http.StatusUnprocessableEntity || cCode != mCode || cBody != mBody {
			t.Fatalf("%s: cross-tenant=%d %s / missing=%d %s; want identical 422 bodies", path, cCode, cBody, mCode, mBody)
		}
	}
	if agent.called || guard.acquired || sessions() != 0 {
		t.Fatalf("agent=%v guard=%v sessions=%d; a refused context must reach neither the guard nor the agent nor the store",
			agent.called, guard.acquired, sessions())
	}

	// Positive control in the same test: a valid context of tenant A's own app reaches agent+store.
	if code, body := post(chatPaths[0], "clinic_a"); code != http.StatusOK || !agent.called || sessions() != 1 {
		t.Fatalf("valid context: code=%d agent=%v sessions=%d body=%s", code, agent.called, sessions(), body)
	}
	if agent.lastTurn.App == nil || agent.lastTurn.App.Label != "Phòng khám A" || agent.lastTurn.Project != "Dự án A" {
		t.Fatalf("agent turn = %+v; want tenant A's resolved app + project name", agent.lastTurn)
	}
	if nav := agent.lastTurn.App.Nav; len(nav) != 1 || nav[0].Name != "patients" || nav[0].Type != "entity" {
		t.Fatalf("app.nav = %+v; want one entity entry named 'patients' (RefName from real GetNav)", nav)
	}
}
