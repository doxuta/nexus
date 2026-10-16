package handlers

// router_ai_draft_caller_test.go — specs/067 T083 (Rủi ro 4). The AI Builder reads the builder's
// DRAFT overlay only because its chat routes live under /api/v1/admin/, where DraftCaller sets
// ReadDraft=true for the builder with no header. Move the route out of /admin/ and the overlay turns
// off silently; the ⌘K assistant (shared, non-admin path) must never read it without the header.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAIChatRoutes_ReadDraftOverlay(t *testing.T) {
	cases := []struct {
		name, path    string
		wantReadDraft bool
	}{
		{"builder chat reads the overlay", "/api/v1/admin/ai/builder/chat", true},
		{"builder chat stream reads the overlay", "/api/v1/admin/ai/builder/chat/stream", true},
		{"assistant ask without header reads live", "/api/v1/assistant/ask", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limiter, operator := &seenCaller{}, &seenCaller{}
			r := draftCallerEngine(t, limiter, operator)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer tok-builder")
			r.ServeHTTP(httptest.NewRecorder(), req)
			if limiter.hits != 1 || !limiter.ok || !limiter.c.IsBuilder || limiter.c.ReadDraft != tc.wantReadDraft {
				t.Fatalf("%s: hits=%d ok=%v caller=%+v, want builder with readDraft=%v", tc.path, limiter.hits, limiter.ok, limiter.c, tc.wantReadDraft)
			}
		})
	}
}
