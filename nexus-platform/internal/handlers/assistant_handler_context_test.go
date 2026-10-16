package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"nexus/platform/internal/ai"
)

// TestAssistantHandler_IgnoresBuilderContext pins specs/067 FR-004/SC-004: the ⌘K assistant keeps
// the plain chatRequest, so a `context` block (even an invalid one) neither changes the messages
// the agent receives nor the response — it cannot be smuggled into the assistant.
func TestAssistantHandler_IgnoresBuilderContext(t *testing.T) {
	run := func(body map[string]any) (string, []ai.ChatMessage, int) {
		agent := &fakeAssistantAgent{res: assistantOKResult()}
		r := assistantTestRouter(agent, newFakeAssistantStore(), false)
		rec := doJSON(r, http.MethodPost, "/api/v1/assistant/ask", body)
		var resp struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		delete(resp.Data, "sessionId") // minted per run
		out, err := json.Marshal(resp.Data)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return string(out), agent.lastMsgs, rec.Code
	}
	plainBody, plainMsgs, plainCode := run(map[string]any{"message": "doanh số?"})
	ctxBody, ctxMsgs, ctxCode := run(map[string]any{"message": "doanh số?", "context": map[string]any{"app": "Bad App!", "panel": "zzz"}})
	if plainCode != http.StatusOK || ctxCode != plainCode || ctxBody != plainBody {
		t.Fatalf("codes %d/%d, bodies differ:\n%s\n%s", plainCode, ctxCode, plainBody, ctxBody)
	}
	if len(plainMsgs) != len(ctxMsgs) || plainMsgs[len(plainMsgs)-1].Content != ctxMsgs[len(ctxMsgs)-1].Content {
		t.Fatalf("agent messages differ: %+v vs %+v", plainMsgs, ctxMsgs)
	}
}
