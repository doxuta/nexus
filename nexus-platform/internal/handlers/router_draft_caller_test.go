package handlers

// router_draft_caller_test.go — specs/063-draft-publish T020a. The mount position of DraftCaller on
// the protected group is part of the contract (plan §2): gin snapshots a group's handler chain when
// the group is created, so a Use() placed after `adminGated := protected.Group("")` leaves every
// admin route without a Caller. The probes below are the real chain's own hooks: the data rate
// limiter (on `protected`, right after DraftCaller) and the operator tier (on builderGated, which
// derives from adminGated — it sees a Caller only if DraftCaller was in `protected`'s chain when
// the sub-groups were created).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
)

type seenCaller struct {
	hits int
	c    metaversion.Caller
	ok   bool
}

func (s *seenCaller) probe(c *gin.Context) {
	s.hits++
	s.c, s.ok = metaversion.CallerFrom(c.Request.Context())
	c.Next()
}

func draftCallerEngine(t *testing.T, limiter, operator *seenCaller) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	builder := &auth.Claims{Subject: auth.Subject{UserID: "builder", TenantID: "t1", IsAdmin: true}, TokenType: auth.TokenTypeAccess}
	preview := &auth.Claims{Subject: auth.Subject{UserID: "principal", TenantID: "t1", RoleID: "r1", SessionID: "ps",
		ActorUserID: "builder", ActorTenantID: "t1"}, TokenType: auth.TokenTypePreview}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"tok-builder": builder, "tok-preview": preview}}
	pass := func(c *gin.Context) { c.Next() }
	r := gin.New()
	r.Use(gin.Recovery())
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, allHandlers(t), zap.NewNop(),
		// adminReader returns the live row: admin + platform operator ⇒ refreshSubject must copy the
		// operator flag onto the Subject or the builder never appears here.
		WithLiveSubject(adminReader{}),
		WithSessionGuard(neverEnded{}),
		WithOperatorTier(operator.probe),
		WithDataRateLimiter(limiter.probe),
		WithAuthRateLimiter(pass), WithSSORateLimiter(pass), WithAssistantRateLimiter(pass),
	)
	return r
}

func TestDraftCallerRouteOrder(t *testing.T) {
	cases := []struct {
		name, method, path, token, header string
		wantBuilder, wantReadDraft        bool
	}{
		{"builder on a builderGated admin route reads the overlay", http.MethodGet, "/api/v1/admin/verticals/builtin", "tok-builder", "", true, true},
		{"builder on a shared route without header reads live", http.MethodGet, "/api/v1/crm_accounts", "tok-builder", "", true, false},
		{"builder on a shared route with header reads the overlay", http.MethodGet, "/api/v1/crm_accounts", "tok-builder", "1", true, true},
		{"header value other than 1 is ignored", http.MethodGet, "/api/v1/crm_accounts", "tok-builder", "true", true, false},
		// S7-4 (T080): the app switcher and the sidebar menu are shared with the customer; the overlay
		// is for the builder's header only.
		{"app list: builder with header reads the overlay", http.MethodGet, "/api/v1/apps", "tok-builder", "1", true, true},
		{"app list: builder without header reads live", http.MethodGet, "/api/v1/apps", "tok-builder", "", true, false},
		{"app menu: builder with header reads the overlay", http.MethodGet, "/api/v1/apps/crm/nav", "tok-builder", "1", true, true},
		{"app menu: builder without header reads live", http.MethodGet, "/api/v1/apps/crm/nav", "tok-builder", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limiter, operator := &seenCaller{}, &seenCaller{}
			r := draftCallerEngine(t, limiter, operator)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			if tc.header != "" {
				req.Header.Set("X-Nexus-Draft", tc.header)
			}
			r.ServeHTTP(httptest.NewRecorder(), req)
			if limiter.hits != 1 || !limiter.ok || limiter.c.IsBuilder != tc.wantBuilder || limiter.c.ReadDraft != tc.wantReadDraft {
				t.Fatalf("at the data limiter (protected group): hits=%d ok=%v caller=%+v, want builder=%v readDraft=%v",
					limiter.hits, limiter.ok, limiter.c, tc.wantBuilder, tc.wantReadDraft)
			}
			if strings.HasPrefix(tc.path, "/api/v1/admin/") {
				// The operator tier sits on builderGated (a child of adminGated): a Caller here proves
				// DraftCaller was in the chain BEFORE the sub-groups were created.
				if operator.hits != 1 || !operator.ok || !operator.c.IsBuilder {
					t.Fatalf("at the operator tier (builderGated): hits=%d ok=%v caller=%+v — DraftCaller mounted after the sub-groups?",
						operator.hits, operator.ok, operator.c)
				}
			}
		})
	}
}

// A preview-as-role token is refused by PreviewReadOnly BEFORE DraftCaller / the limiter run, and
// even on a read it is never the builder (IsAdmin=false pinned).
func TestDraftCallerRouteOrder_PreviewIsNeverBuilder(t *testing.T) {
	limiter, operator := &seenCaller{}, &seenCaller{}
	r := draftCallerEngine(t, limiter, operator)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/crm_accounts", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok-preview")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || limiter.hits != 0 {
		t.Fatalf("preview write: status %d limiter hits %d, want 403 before the limiter", w.Code, limiter.hits)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/crm_accounts", nil)
	req.Header.Set("Authorization", "Bearer tok-preview")
	req.Header.Set("X-Nexus-Draft", "1")
	r.ServeHTTP(httptest.NewRecorder(), req)
	if limiter.hits != 1 || !limiter.ok || limiter.c.IsBuilder || limiter.c.ReadDraft {
		t.Fatalf("preview read with the draft header: hits=%d ok=%v caller=%+v, want a non-builder Caller reading live", limiter.hits, limiter.ok, limiter.c)
	}
}
