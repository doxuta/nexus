package handlers

// import_route_wiring_test.go — unit tests (no DB, no build-tag) that verify:
//   1. RegisterAPIRoutes with both Import and Record handlers does NOT panic (runtime
//      route registration is gin-validated at startup, not compile-time).
//   2. POST /:entity/import is routed to the ImportHandler, NOT the DynamicRecordHandler
//      Create path — static "/import" segment wins over the /:id param.
//
// These tests exist specifically because the route-conflict risk cannot be caught by
// the compiler, and the integration tests (build-tag: integration) require MySQL so
// they do not run in the unit CI job.

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// stubImportSvc is a minimal importServiceI that records whether it was called.
type stubImportSvc struct {
	called bool
}

func (s *stubImportSvc) Import(_ context.Context, _ services.ImportInput) (*services.ImportResult, error) {
	s.called = true
	return &services.ImportResult{Total: 0, Inserted: 0, Failed: 0, Errors: nil}, nil
}

func (s *stubImportSvc) ImportIdempotent(ctx context.Context, in services.ImportInput, _ string) (*services.ImportResult, json.RawMessage, error) {
	r, err := s.Import(ctx, in)
	return r, nil, err
}

// buildImportWiringEngine calls RegisterAPIRoutes with BOTH Import and Record handlers
// wired, using stub services so no DB is needed. Returns the engine and the stub import svc.
func buildImportWiringEngine(t *testing.T) (*gin.Engine, *stubImportSvc) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	// A verifier that accepts exactly "tok" as a valid bearer token for tenant "t1".
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok": claimsFor("t1", "role-1", true), // admin so ACL is bypassed
	}}

	importSvc := &stubImportSvc{}
	importH := NewImportHandler(importSvc, zap.NewNop())

	// RecordService requires real catalog/store — pass nil; the test only cares that
	// POST /:entity/import is NOT dispatched to the record Create handler. If it were,
	// DynamicRecordHandler would panic trying to dereference a nil service and the test
	// would fail with a panic rather than a wrong-status, which is fine either way.
	recordH := NewDynamicRecordHandler(nil, zap.NewNop())

	r := gin.New()
	api := r.Group("/api/v1")
	// No aclChecker (nil) so RequireACL is skipped — admin flag in stubVerifier bypasses it anyway.
	RegisterAPIRoutes(api, ver, nil, APIHandlers{
		Import: importH,
		Record: recordH,
	}, zap.NewNop())

	return r, importSvc
}

// TestImportRoutWiring_NoRegistrationPanic verifies that wiring both Import and Record
// handlers via RegisterAPIRoutes does not panic (gin validates the route tree at
// registration time for obvious conflicts).
func TestImportRoutWiring_NoRegistrationPanic(t *testing.T) {
	// buildImportWiringEngine calls RegisterAPIRoutes; a panic here would fail the test.
	buildImportWiringEngine(t)
}

// TestImportRoutWiring_PostImportRouteToImportHandler verifies that
// POST /api/v1/leads/import is dispatched to the ImportHandler (not the DynamicRecord
// Create handler) even when both are mounted on the same router group.
//
// The request body is a valid multipart form with a file + mapping so the handler can
// at least reach the service call. The stub service sets called=true on entry, which
// is the assertion. A 404 or panic would mean the route was misrouted.
func TestImportRoutWiring_PostImportRouteToImportHandler(t *testing.T) {
	r, importSvc := buildImportWiringEngine(t)

	// Build a minimal multipart/form-data body.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	// file field
	fw, err := mw.CreateFormFile("file", "data.csv")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	_, _ = fw.Write([]byte("name\nAlice\n"))
	// mapping field
	_ = mw.WriteField("mapping", `{"name":"name"}`)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/leads/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer tok")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// The stub import service returns a zero ImportResult (200).
	// If the request had been routed to DynamicRecordHandler.Create with a nil service
	// it would panic (recovered by gin → 500) or return a different status.
	// Either way, importSvc.called must be true — that is the decisive assertion.
	if !importSvc.called {
		t.Fatalf("ImportHandler was NOT called — route dispatched elsewhere (status=%d, body=%s)",
			rec.Code, rec.Body.String())
	}
}

// TestImportRoutWiring_PostEntityRouteNotImport verifies that a plain
// POST /api/v1/leads (without /import) does NOT hit the ImportHandler — i.e. the two
// routes are genuinely distinct and the import registration doesn't shadow the create route.
//
// The DynamicRecordHandler is wired with a nil service; gin.Recovery() catches the resulting
// nil-dereference and returns 500. That is expected — what matters is importSvc.called==false.
func TestImportRoutWiring_PostEntityRouteNotImport(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok": claimsFor("t1", "role-1", true),
	}}
	importSvc := &stubImportSvc{}
	importH := NewImportHandler(importSvc, zap.NewNop())

	// Nil RecordService; gin.Recovery() will catch the nil-deref panic and return 500.
	recordH := NewDynamicRecordHandler(nil, zap.NewNop())

	r := gin.New()
	r.Use(gin.Recovery()) // prevent nil-deref from crashing the test process
	api := r.Group("/api/v1")
	RegisterAPIRoutes(api, ver, nil, APIHandlers{
		Import: importH,
		Record: recordH,
	}, zap.NewNop())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/leads", bytes.NewBufferString(`{"name":"X"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// ImportHandler must NOT have been called — the route must go to DynamicRecordHandler.
	if importSvc.called {
		t.Fatalf("ImportHandler was called for POST /:entity — route conflict detected (status=%d)", rec.Code)
	}
	t.Logf("POST /:entity status=%d (expected ~500 from nil service, import not called — OK)", rec.Code)
}
