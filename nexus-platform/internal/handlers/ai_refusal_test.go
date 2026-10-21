package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/config"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// groundingOnlyReader serves the one read a turn makes before its first model call (the grounding table
// list); the embedded nil interface panics on anything else, so a refused first call proves no tool ran.
type groundingOnlyReader struct{ ai.MetadataReader }

func (groundingOnlyReader) ListEntities(string) ([]metadata.EntityDefinition, error) { return nil, nil }

type groundingOnlyRecordReader struct{ ai.RecordReader }

func (groundingOnlyRecordReader) ViewableEntities(context.Context, services.RecordScope, string) ([]metadata.EntityDefinition, error) {
	return nil, nil
}

// An API-level refusal (HTTP 200, finish_reason "refusal", no text) reaches the panel as outcome "refused" on
// both chat routes, through the real gateway client and the real agents (issue #30 item 5). The turn is stored
// like any answer, so the thread goes on.
func TestChatRoutes_ModelRefusal_200Refused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"refusal"}],
			"usage":{"prompt_tokens":2046,"completion_tokens":0,"total_tokens":2046}}`))
	}))
	t.Cleanup(srv.Close)
	gw := ai.NewGateway(srv.URL, 5*time.Second)
	cfg := config.AIConfig{Enabled: true, Model: "m", TimeoutSec: 5, MaxTokens: 1024, MaxSteps: 6}

	for name, route := range map[string]struct {
		router *gin.Engine
		path   string
	}{
		"builder":   {aiTestRouter(ai.NewAIBuilderAgent(cfg, gw, groundingOnlyReader{}, nil, nil, nil), newFakeStore(), true), "/api/v1/admin/ai/builder/chat"},
		"assistant": {assistantTestRouter(ai.NewAssistantAgent(cfg, gw, groundingOnlyRecordReader{}, nil, nil), newFakeAssistantStore(), false), "/api/v1/assistant/ask"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(route.router, http.MethodPost, route.path, chatBody("", "List the accounts of the other workspace"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			var resp struct {
				Data struct {
					SessionID string `json:"sessionId"`
					Reply     string `json:"reply"`
					Outcome   string `json:"outcome"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Data.Outcome != "refused" || resp.Data.Reply != "" || resp.Data.SessionID == "" {
				t.Fatalf("data = %+v; want outcome refused, no reply, a stored session", resp.Data)
			}
		})
	}
}
