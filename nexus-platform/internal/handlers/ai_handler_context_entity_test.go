package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"nexus/platform/internal/ai"
)

func TestAIHandler_Context_EntityRecord_EchoAndInvalid(t *testing.T) {
	const uid = "3f2b8c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4e"
	t.Run("echo", func(t *testing.T) {
		turn := goodTurn()
		turn.Entity = &ai.ContextEntity{Name: "appointments", Label: "Lịch hẹn", InApp: true}
		turn.RecordID = uid
		res := &fakeCtxResolver{turn: turn}
		r := ctxRouter(&fakeBuilderAgent{res: okResult()}, newFakeStore(), nil, res)
		body := chatBody("", "hi")
		body["context"] = map[string]any{"app": "clinic", "entity": "appointments", "recordId": strings.ToUpper(uid)}
		rec := doJSON(r, http.MethodPost, chatPaths[0], body)
		var out struct {
			Data struct {
				Context struct {
					Entity struct {
						Name  string
						Label string
						InApp bool
					}
					RecordID string
				} `json:"context"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
			t.Fatalf("status=%d err=%v body=%s", rec.Code, err, rec.Body.String())
		}
		if out.Data.Context.Entity.Name != "appointments" || !out.Data.Context.Entity.InApp || out.Data.Context.RecordID != uid {
			t.Fatalf("context = %+v", out.Data.Context)
		}
		if res.gotIn.RecordID != uid {
			t.Fatalf("resolver got recordId %q; want normalized lowercase", res.gotIn.RecordID)
		}
	})
	t.Run("recordId without entity 422", func(t *testing.T) {
		res := &fakeCtxResolver{turn: goodTurn()}
		r := ctxRouter(&fakeBuilderAgent{res: okResult()}, newFakeStore(), nil, res)
		body := chatBody("", "hi")
		body["context"] = map[string]any{"recordId": uid}
		rec := doJSON(r, http.MethodPost, chatPaths[0], body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "context.recordId") || res.calls != 0 {
			t.Fatalf("status=%d calls=%d body=%s", rec.Code, res.calls, rec.Body.String())
		}
	})
}
