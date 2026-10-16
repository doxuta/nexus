package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
)

// fakeCtxResolver records every call so a test can prove a refusal happened BEFORE it / after it.
type fakeCtxResolver struct {
	calls  int
	gotTen string
	gotIn  ai.TurnContextInput
	turn   ai.TurnContext
	err    error
}

func (f *fakeCtxResolver) Resolve(_ context.Context, tenantID string, in ai.TurnContextInput) (ai.TurnContext, error) {
	f.calls++
	f.gotTen, f.gotIn = tenantID, in
	return f.turn, f.err
}

func ctxRouter(agent BuilderAgent, store SessionStore, guard TurnGuard, res ContextResolver) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "t1")
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{UserID: "u1", TenantID: "t1", RoleID: "r1", IsAdmin: true}})
		c.Next()
	})
	grp := r.Group("/api/v1")
	grp.Use(middleware.RequireAdmin(nil))
	NewAIBuilderHandler(agent, store, guard, res, nil).RegisterRoutes(grp)
	return r
}

var chatPaths = []string{"/api/v1/admin/ai/builder/chat", "/api/v1/admin/ai/builder/chat/stream"}

func goodTurn() ai.TurnContext {
	return ai.TurnContext{Present: true, Project: "Dự án", Panel: "bang",
		App: &ai.ContextApp{Name: "clinic", Label: "Phòng khám", Nav: []ai.ContextNavItem{{Type: "entity", Name: "x", Label: "X"}}}}
}

func TestAIHandler_Context_InvalidShape_422_BeforeEverything(t *testing.T) {
	for _, path := range chatPaths {
		for _, tc := range []struct{ name, key, val, field string }{
			{"bad app", "app", "Bad App!", "context.app"},
			{"bad panel", "panel", "ignore-rules", "context.panel"},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				agent := &fakeBuilderAgent{res: okResult()}
				store := newFakeStore()
				guard := &fakeTurnGuard{}
				res := &fakeCtxResolver{turn: goodTurn()}
				r := ctxRouter(agent, store, guard, res)
				body := chatBody("", "hi")
				body["context"] = map[string]any{tc.key: tc.val}
				rec := doJSON(r, http.MethodPost, path, body)
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
				}
				var out struct {
					Code    string         `json:"code"`
					Details map[string]any `json:"details"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &out)
				if out.Code != "AI_CONTEXT_INVALID" || out.Details["field"] != tc.field {
					t.Fatalf("body = %s", rec.Body.String())
				}
				if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Fatalf("Content-Type = %q; refusals are plain JSON on the stream route too", ct)
				}
				if agent.called || guard.acquired || res.calls != 0 || len(store.sessions) != 0 {
					t.Fatalf("agent=%v guard=%v resolver=%d sessions=%d; all must be untouched", agent.called, guard.acquired, res.calls, len(store.sessions))
				}
			})
		}
	}
}

func TestAIHandler_Context_NotFound_422_NoGuardNoAgent(t *testing.T) {
	for _, path := range chatPaths {
		agent := &fakeBuilderAgent{res: okResult()}
		store := newFakeStore()
		guard := &fakeTurnGuard{}
		res := &fakeCtxResolver{err: &ai.ContextError{Code: "AI_CONTEXT_NOT_FOUND", Field: "context.app"}}
		r := ctxRouter(agent, store, guard, res)
		body := chatBody("", "hi")
		body["context"] = map[string]any{"app": "clinic"}
		rec := doJSON(r, http.MethodPost, path, body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "AI_CONTEXT_NOT_FOUND") {
			t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		if agent.called || guard.acquired || len(store.sessions) != 0 {
			t.Fatalf("%s: refusal must precede guard/agent/session", path)
		}
	}
}

func TestAIHandler_Context_ResolverFailure_500(t *testing.T) {
	r := ctxRouter(&fakeBuilderAgent{res: okResult()}, newFakeStore(), nil, &fakeCtxResolver{err: errors.New("db down")})
	body := chatBody("", "hi")
	body["context"] = map[string]any{"app": "clinic"}
	rec := doJSON(r, http.MethodPost, chatPaths[0], body)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "AI_CHAT_FAILED") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAIHandler_Context_NilResolverWithContext_500(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult()}
	r := ctxRouter(agent, newFakeStore(), nil, nil)
	body := chatBody("", "hi")
	body["context"] = map[string]any{"app": "clinic"}
	rec := doJSON(r, http.MethodPost, chatPaths[0], body)
	if rec.Code != http.StatusInternalServerError || agent.called {
		t.Fatalf("status=%d agent.called=%v; a context must never be silently dropped", rec.Code, agent.called)
	}
}

func TestAIHandler_NoContext_ResolverUntouched_NullContext(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult()}
	res := &fakeCtxResolver{turn: goodTurn()}
	r := ctxRouter(agent, newFakeStore(), nil, res)
	rec := doJSON(r, http.MethodPost, chatPaths[0], chatBody("", "hi"))
	if rec.Code != http.StatusOK || res.calls != 0 || agent.lastTurn.Present {
		t.Fatalf("status=%d resolver=%d turn=%+v", rec.Code, res.calls, agent.lastTurn)
	}
	var out struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if string(out.Data["context"]) != "null" {
		t.Fatalf("data.context = %s; want null", out.Data["context"])
	}
}

func TestAIHandler_Context_Valid_ForeignTenantKeyDropped_BothRoutesSameBody(t *testing.T) {
	var bodies []string
	for _, path := range chatPaths {
		agent := &fakeBuilderAgent{res: okResult()}
		res := &fakeCtxResolver{turn: goodTurn()}
		r := ctxRouter(agent, newFakeStore(), nil, res)
		body := chatBody("", "hi")
		body["context"] = map[string]any{"app": " clinic ", "panel": "bang", "tenantId": "other-tenant"}
		rec := doJSON(r, http.MethodPost, path, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		if res.gotTen != "t1" || res.gotIn.App != "clinic" || res.gotIn.Panel != "bang" {
			t.Fatalf("resolver got tenant=%q in=%+v; want JWT tenant t1 and trimmed input", res.gotTen, res.gotIn)
		}
		if !agent.lastTurn.Present || agent.lastTurn.App == nil || agent.lastTurn.App.Name != "clinic" {
			t.Fatalf("agent turn = %+v", agent.lastTurn)
		}
		raw := rec.Body.String()
		if strings.Contains(raw, "\"nav\"") {
			t.Fatalf("data.context must omit nav: %s", raw)
		}
		if strings.Contains(path, "stream") {
			i := strings.Index(raw, "event:result")
			if i < 0 {
				i = strings.Index(raw, "event: result")
			}
			if i < 0 {
				t.Fatalf("no result event: %s", raw)
			}
			line := raw[i:]
			line = line[strings.Index(line, "data:")+5:]
			line = strings.TrimSpace(line[:strings.Index(line, "\n")])
			bodies = append(bodies, line)
		} else {
			bodies = append(bodies, strings.TrimSpace(raw))
		}
		var out struct {
			Data struct {
				Context struct {
					Project struct{ Name string } `json:"project"`
					App     struct{ Name, Label string }
					Panel   string
				} `json:"context"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &out); err != nil || out.Data.Context.App.Label != "Phòng khám" || out.Data.Context.Project.Name != "Dự án" || out.Data.Context.Panel != "bang" {
			t.Fatalf("data.context = %+v err=%v", out.Data.Context, err)
		}
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("the two routes must answer with the same bytes:\n%s\n%s", bodies[0], bodies[1])
	}
}

func TestAIHandler_Context_UnknownSession_404_ResolverUntouched(t *testing.T) {
	res := &fakeCtxResolver{turn: goodTurn()}
	agent := &fakeBuilderAgent{res: okResult()}
	r := ctxRouter(agent, newFakeStore(), nil, res)
	body := chatBody("no-such-session", "hi")
	body["context"] = map[string]any{"app": "clinic"}
	rec := doJSON(r, http.MethodPost, chatPaths[0], body)
	if rec.Code != http.StatusNotFound || res.calls != 0 || agent.called {
		t.Fatalf("status=%d resolver=%d", rec.Code, res.calls)
	}
}
