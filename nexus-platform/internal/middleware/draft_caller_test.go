package middleware

// draft_caller_test.go — specs/063-draft-publish T009/T010: the builder flag comes from the live row
// only, and DraftCaller resolves a Caller (never refusing) from the Subject + matched route.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
)

func TestIsBuilder(t *testing.T) {
	tests := []struct {
		name string
		sub  auth.Subject
		want bool
	}{
		{"admin + operator", auth.Subject{IsAdmin: true, IsPlatformOperator: true}, true},
		{"admin without operator (legacy customer admin)", auth.Subject{IsAdmin: true}, false},
		{"operator without admin", auth.Subject{IsPlatformOperator: true}, false},
		{"workspace-session subject (admin + operator set by the resolver, wsid)",
			auth.Subject{IsAdmin: true, IsPlatformOperator: true, WorkspaceSessionID: "ws-1", ActorUserID: "b", ActorTenantID: "home"}, true},
		{"preview subject (IsAdmin pinned false)", auth.Subject{IsAdmin: false, IsPlatformOperator: true, ActorUserID: "b"}, false},
		{"zero subject", auth.Subject{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := metaversion.IsBuilder(tc.sub); got != tc.want {
				t.Fatalf("IsBuilder = %v, want %v", got, tc.want)
			}
		})
	}
}

// refreshSubject sets IsPlatformOperator from the LIVE row (and clears it when the row says no),
// never from the token.
func TestRefreshSubject_SetsIsPlatformOperatorFromLiveRow(t *testing.T) {
	stale := sampleClaims()
	stale.IsPlatformOperator = true // a stale/forged value on the claims must not survive
	for _, op := range []bool{true, false} {
		users := &fakeLiveUsers{user: &auth.User{Status: "active", IsAdmin: true, IsPlatformOperator: op}}
		live, err := refreshSubject(t.Context(), users, stale)
		if err != nil {
			t.Fatalf("refreshSubject: %v", err)
		}
		if live.IsPlatformOperator != op {
			t.Errorf("row operator=%v: Subject.IsPlatformOperator = %v", op, live.IsPlatformOperator)
		}
	}
}

func TestAuthMiddlewareLive_NonLiveNeverBuilder(t *testing.T) {
	v := &fakeVerifier{claims: sampleClaims()} // IsAdmin=true, IsPlatformOperator unset
	c, _ := newRequest("Bearer good.token.here")
	AuthMiddleware(v, nil)(c)
	sub, _ := SubjectFromContext(c)
	if metaversion.IsBuilder(sub) {
		t.Fatal("the non-live Auth stage produced a builder")
	}
}

func draftCallerRun(t *testing.T, claims *auth.Claims, route, url, header string) (metaversion.Caller, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var got metaversion.Caller
	var ok bool
	r.Use(func(c *gin.Context) {
		if claims != nil {
			c.Set(ctxKeyClaims, claims)
		}
		c.Next()
	})
	r.Use(DraftCaller("/api/v1"))
	r.GET(route, func(c *gin.Context) { got, ok = metaversion.CallerFrom(c.Request.Context()); c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if header != "" {
		req.Header.Set(DraftHeader, header)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DraftCaller must never refuse: %d", w.Code)
	}
	return got, ok
}

func TestDraftCaller(t *testing.T) {
	builder := func() *auth.Claims {
		return &auth.Claims{Subject: auth.Subject{UserID: "b", TenantID: "t1", IsAdmin: true, IsPlatformOperator: true}, TokenType: auth.TokenTypeAccess}
	}
	staff := &auth.Claims{Subject: auth.Subject{UserID: "s", TenantID: "t1"}, TokenType: auth.TokenTypeAccess}
	session := builder()
	session.TenantID, session.ActorTenantID, session.ActorUserID = "customer", "home", "b"
	apiKey := builder()
	apiKey.TokenType = auth.TokenTypeAPIKey

	tests := []struct {
		name       string
		claims     *auth.Claims
		route, url string
		header     string
		want       metaversion.Caller
	}{
		{"builder on /admin reads the overlay", builder(), "/api/v1/admin/workflows", "/api/v1/admin/workflows", "",
			metaversion.Caller{UserID: "b", TenantID: "t1", HomeTenantID: "t1", IsBuilder: true, ReadDraft: true}},
		{"builder on a shared route without header reads live", builder(), "/api/v1/reports", "/api/v1/reports", "",
			metaversion.Caller{UserID: "b", TenantID: "t1", HomeTenantID: "t1", IsBuilder: true}},
		{"builder with the header reads the overlay", builder(), "/api/v1/reports", "/api/v1/reports", "1",
			metaversion.Caller{UserID: "b", TenantID: "t1", HomeTenantID: "t1", IsBuilder: true, ReadDraft: true}},
		{"staff with the header is ignored silently", staff, "/api/v1/reports", "/api/v1/reports", "1",
			metaversion.Caller{UserID: "s", TenantID: "t1", HomeTenantID: "t1"}},
		{"workspace session: home tenant is the actor's tenant", session, "/api/v1/admin/workflows", "/api/v1/admin/workflows", "",
			metaversion.Caller{UserID: "b", TenantID: "customer", HomeTenantID: "home", IsBuilder: true, ReadDraft: true}},
		{"API key is never the builder", apiKey, "/api/v1/admin/workflows", "/api/v1/admin/workflows", "1",
			metaversion.Caller{UserID: "b", TenantID: "t1", HomeTenantID: "t1"}},
		// The raw URL starts with /admin/ but the MATCHED route is the record dispatcher /:entity/:id.
		{"raw URL cannot fake the admin prefix", builder(), "/api/v1/:entity/:id", "/api/v1/admin/x", "",
			metaversion.Caller{UserID: "b", TenantID: "t1", HomeTenantID: "t1", IsBuilder: true}},
		{"no Subject: an empty Caller still present", nil, "/api/v1/reports", "/api/v1/reports", "1", metaversion.Caller{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := draftCallerRun(t, tc.claims, tc.route, tc.url, tc.header)
			if !ok {
				t.Fatal("no Caller on the request context (c.Request.Context())")
			}
			if got != tc.want {
				t.Fatalf("Caller = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDraftCaller_WithLiveReadOverrides(t *testing.T) {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(ctxKeyClaims, &auth.Claims{Subject: auth.Subject{UserID: "b", TenantID: "t1", IsAdmin: true, IsPlatformOperator: true}, TokenType: auth.TokenTypeAccess})
		c.Next()
	})
	r.Use(DraftCaller("/api/v1"))
	var readDraft, liveRead bool
	r.GET("/api/v1/admin/x", func(c *gin.Context) {
		ctx := metaversion.WithLiveRead(c.Request.Context())
		caller, _ := metaversion.CallerFrom(ctx)
		readDraft, liveRead = caller.ReadDraft, metaversion.IsLiveRead(ctx)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/admin/x", nil))
	if !readDraft || !liveRead {
		t.Fatalf("Caller.ReadDraft=%v IsLiveRead=%v: WithLiveRead must be a separate override the gate honours", readDraft, liveRead)
	}
}

// SC-010 (the chain, not the pieces): a builder holding a workspace-session token reaches the draft
// layer AS the builder — real AuthMiddleware (delegation branch) → DraftCaller. If the Auth stage
// ever stopped setting IsPlatformOperator on the rebuilt Subject, this Caller would be a plain admin
// and a delivered customer's rule edits would land live.
func TestDraftCaller_WorkspaceTokenThroughAuthIsTheBuilder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	claims := wsClaims()
	claims.IsAdmin = false // the token's own claim must not decide privilege
	r := gin.New()
	r.Use(AuthMiddlewareLive(&fakeVerifier{claims: claims}, &adminForEveryone{}, &fakeSessions{}, nil,
		WithWorkspaceResolver(&fakeResolver{res: liveResolution()})))
	r.Use(DraftCaller("/api/v1"))
	var got metaversion.Caller
	r.PUT("/api/v1/admin/workflows/:id", func(c *gin.Context) {
		got, _ = metaversion.CallerFrom(c.Request.Context())
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/workflows/w1", nil)
	req.Header.Set("Authorization", "Bearer ws.token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	want := metaversion.Caller{UserID: "builder-1", TenantID: "customer-1", HomeTenantID: "home-1", IsBuilder: true, ReadDraft: true}
	if w.Code != http.StatusOK || got != want {
		t.Fatalf("status=%d Caller=%+v, want %+v", w.Code, got, want)
	}
}
