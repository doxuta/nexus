package handlers

// Held out of the eval run: regrade.sh copies this file next to a case's hidden handler test. It serves the
// notes routes through the REAL RegisterAPIRoutes, as a non-admin user granted view on every table, over the
// REAL notes.Service and Repository on the hidden SQL driver: wherever the agent mounted a route, and whatever
// it added to the service, the request goes through the code the agent wrote.

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/notes"
	"nexus/platform/internal/services"
)

type hiddenVerifier struct{}

func (hiddenVerifier) VerifyAccessToken(tok string) (*auth.Claims, error) {
	if tok != "rep" {
		return nil, auth.ErrTokenInvalid
	}
	return &auth.Claims{Subject: auth.Subject{UserID: "u1", TenantID: "t1", RoleID: "rep"}, TokenType: auth.TokenTypeAccess}, nil
}

type hiddenGrantAll struct{}

func (hiddenGrantAll) Authorize(context.Context, string, string, string, string) (bool, string, error) {
	return true, "all", nil
}

type hiddenGuard struct{}

func (hiddenGuard) ResolveNoteTarget(_ context.Context, scope services.RecordScope, _, entity, _ string) (string, services.RecordScope, error) {
	return entity, scope, nil
}

// hiddenGet returns a GET helper over a router whose notes table holds rows (newest first).
func hiddenGet(t *testing.T, rows ...map[string]driver.Value) func(path string) (int, map[string]any, string) {
	t.Helper()
	db, _ := newHiddenDB(t, rows...)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), hiddenVerifier{}, hiddenGrantAll{}, APIHandlers{
		Record: NewDynamicRecordHandler(nil, zap.NewNop()),
		Note:   NewNoteHandler(notes.NewService(notes.NewRepository(db), hiddenGuard{}, nil), zap.NewNop()),
	}, zap.NewNop())
	return func(path string) (int, map[string]any, string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer rep")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body, rec.Body.String()
	}
}
