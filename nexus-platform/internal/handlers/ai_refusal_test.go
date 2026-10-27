package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
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
// both chat routes, through the real gateway client and the real agents (issue #30 item 5), whether the
// gateway answers in one body or streams (issue #30 item 8: the client asks for a stream). The turn is stored
// like any answer, so the thread goes on.
func TestChatRoutes_ModelRefusal_200Refused(t *testing.T) {
	for wire, body := range map[string]string{
		"whole": `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"refusal"}],
			"usage":{"prompt_tokens":2046,"completion_tokens":0,"total_tokens":2046}}`,
		"streamed": "data: " + `{"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
			"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"refusal"}],"usage":{"prompt_tokens":2046,"completion_tokens":0,"total_tokens":2046}}` + "\n\n",
	} {
		t.Run(wire, func(t *testing.T) { refusedOnBothRoutes(t, body) })
	}
}

func refusedOnBothRoutes(t *testing.T, body string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
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

// A declined turn stays declined (issue #30 item 5 leftovers), on both routes, through the real gateway client
// and agents: the reopened session marks the reply "refused" (the panel says the model declined instead of
// showing an empty answer), and the next turn starts clean: the declined prompt is not sent to the model again,
// so it never reads two user messages in a row. The answered turn after it reads "ok".
func TestChatRoutes_RefusedTurn_ReopensRefused_NotReplayed(t *testing.T) {
	cfg := config.AIConfig{Enabled: true, Model: "m", TimeoutSec: 5, MaxTokens: 1024, MaxSteps: 6}
	const declined, next = "List the accounts of the other workspace", "Liệt kê khách hàng của tôi"

	for name, route := range map[string]struct {
		router     func(gw *ai.Gateway) *gin.Engine
		chat, read string
	}{
		"builder": {func(gw *ai.Gateway) *gin.Engine {
			return aiTestRouter(ai.NewAIBuilderAgent(cfg, gw, groundingOnlyReader{}, nil, nil, nil), newFakeStore(), true)
		}, "/api/v1/admin/ai/builder/chat", "/api/v1/admin/ai/builder/sessions/"},
		"assistant": {func(gw *ai.Gateway) *gin.Engine {
			return assistantTestRouter(ai.NewAssistantAgent(cfg, gw, groundingOnlyRecordReader{}, nil, nil), newFakeAssistantStore(), false)
		}, "/api/v1/assistant/ask", "/api/v1/assistant/sessions/"},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var sent [][]ai.ChatMessage // the conversation of each gateway call
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req ai.ChatRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				mu.Lock()
				sent = append(sent, req.Messages)
				first := len(sent) == 1
				mu.Unlock()
				if first {
					_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"refusal"}],"usage":{"prompt_tokens":9}}`))
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Đây là danh sách."},"finish_reason":"stop"}],"usage":{"prompt_tokens":9}}`))
			}))
			t.Cleanup(srv.Close)
			router := route.router(ai.NewGateway(srv.URL, 5*time.Second))

			turn := func(sessionID, message string) string {
				t.Helper()
				rec := doJSON(router, http.MethodPost, route.chat, chatBody(sessionID, message))
				var resp struct {
					Data struct {
						SessionID string `json:"sessionId"`
					} `json:"data"`
				}
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
					t.Fatalf("POST %s = %d %s", route.chat, rec.Code, rec.Body.String())
				}
				return resp.Data.SessionID
			}
			outcomes := func(id string) []any {
				t.Helper()
				rec := doJSON(router, http.MethodGet, route.read+id, nil)
				var resp struct {
					Data struct {
						Messages []map[string]any `json:"messages"`
					} `json:"data"`
				}
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
					t.Fatalf("GET %s = %d %s", route.read+id, rec.Code, rec.Body.String())
				}
				var out []any
				for _, m := range resp.Data.Messages {
					out = append(out, m["outcome"]) // nil = the key is absent
				}
				return out
			}

			id := turn("", declined)
			if got := outcomes(id); !slices.Equal(got, []any{nil, "refused"}) {
				t.Fatalf("reopened outcomes = %v; want [<absent> refused]", got)
			}

			turn(id, next)
			var users []string
			for _, m := range sent[1] {
				if m.Role == "user" {
					users = append(users, m.Content)
				}
			}
			if !slices.Equal(users, []string{next}) {
				t.Fatalf("user messages sent after the refusal = %q; want only %q (the declined prompt dropped)", users, next)
			}
			if got := outcomes(id); !slices.Equal(got, []any{nil, "refused", nil, "ok"}) {
				t.Fatalf("outcomes after the next turn = %v; want [<absent> refused <absent> ok]", got)
			}
		})
	}
}

// buildHistory leaves a declined turn out whole, its prompt and its empty reply, wherever it sits in the
// thread; the turns around it replay as they were.
func TestBuildHistory_DropsADeclinedTurn(t *testing.T) {
	prior := []ai.Message{
		{Seq: 0, Role: "user", Content: "declined first"}, {Seq: 1, Role: "assistant", Outcome: "refused"},
		{Seq: 2, Role: "user", Content: "a"}, {Seq: 3, Role: "assistant", Content: "A", Outcome: "ok"},
		{Seq: 4, Role: "user", Content: "declined again"}, {Seq: 5, Role: "assistant", Outcome: "refused"},
	}
	var got []string
	for _, m := range buildHistory(prior, "b") {
		got = append(got, m.Role+":"+m.Content)
	}
	if want := []string{"user:a", "assistant:A", "user:b"}; !slices.Equal(got, want) {
		t.Fatalf("history = %q; want %q", got, want)
	}
}
