package ai

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/config"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// TestAssistantAgent_Ask_ToolLoop_ScopeThreaded is the end-to-end trap-#1 pin at the agent layer:
// the recordScope given to Ask is the ONE scope every tool read receives (grounding included),
// the loop echoes tool_call_ids, and the ledger row is tagged assistant.ask with the prompt
// HASHED. A pure READ turn buffers no draft (drafts exist only when a propose_record_* tool ran —
// see TestAssistantAgent_Ask_RecordDraftThreaded).
func TestAssistantAgent_Ask_ToolLoop_ScopeThreaded(t *testing.T) {
	gw := &scriptedGateway{
		t: t,
		responses: []ChatResponse{
			toolCallResp("call_1", "aggregate_records", `{"entity":"opportunities","metric":"count"}`),
			finalTextResp("Bạn có 12 cơ hội đang mở."),
		},
	}
	rd := &fakeRecordReader{
		ents:   []metadata.EntityDefinition{{Name: "opportunities", Label: "Cơ hội"}},
		aggRes: services.AggregateResult{Scalar: services.AggregateScalarResult{Count: 12}},
	}
	rec := &fakeRecorder{}
	agent := NewAssistantAgent(enabledCfg(6), gw, rd, rec, nil)

	scope := callerScope()
	res, err := agent.Ask(context.Background(), scope, "tenant-A", "",
		[]ChatMessage{{Role: "user", Content: "có bao nhiêu cơ hội đang mở?"}})
	if err != nil {
		t.Fatalf("Ask() error = %v; want nil", err)
	}
	if res.Outcome != outcomeOK || res.Reply == "" {
		t.Fatalf("result = %+v; want ok outcome with a reply", res)
	}
	if len(res.Draft) != 0 {
		t.Fatalf("a pure read turn returned a draft %+v; drafts come only from propose_record_*", res.Draft)
	}

	// EVERY reader call (grounding list_entities + the aggregate tool) received the caller scope
	// VERBATIM — never a rebuilt/blank scope.
	if len(rd.gotScopes) < 2 {
		t.Fatalf("reader calls = %d; want grounding + tool call", len(rd.gotScopes))
	}
	for i, got := range rd.gotScopes {
		if got != scope {
			t.Fatalf("reader call %d scope = %+v; want the Ask recordScope VERBATIM %+v", i, got, scope)
		}
	}

	// Soul: message[0] of the first gateway call is the ASSISTANT persona (not the builder's),
	// message[1] the ACL-filtered grounding, message[2] the user turn.
	first := gw.calls[0]
	if len(first) < 3 || first[0].Role != "system" || first[0].Content != AssistantPersona() {
		t.Fatalf("message[0] must be the assistant persona; got %+v", first[0])
	}
	if first[0].Content == Persona() {
		t.Fatal("assistant persona must be DISTINCT from the builder persona")
	}
	if first[1].Role != "system" || first[1].Content == "" {
		t.Fatalf("message[1] must be the grounding note; got %+v", first[1])
	}
	assertToolIDEcho(t, gw.calls[1], "call_1")

	// Ledger: one row, feature assistant.ask, prompt hashed (never raw), user attributed from
	// the recordScope identity.
	if len(rec.runs) != 1 {
		t.Fatalf("ledger rows = %d; want 1", len(rec.runs))
	}
	run := rec.runs[0]
	if run.Feature != FeatureAssistantAsk || run.TenantID != "tenant-A" || run.UserID != scope.UserID {
		t.Fatalf("ledger row = %+v; want feature=assistant.ask tenant-A user %s", run, scope.UserID)
	}
	if run.PromptHash != sha256Hex("có bao nhiêu cơ hội đang mở?") {
		t.Fatalf("prompt_hash = %q; want the sha256 of the user message", run.PromptHash)
	}
}

// TestAssistantAgent_Ask_Disabled: honest-disabled — no gateway, no reader, no ledger row.
func TestAssistantAgent_Ask_Disabled(t *testing.T) {
	gw := &scriptedGateway{t: t, failIfUsed: true}
	rec := &fakeRecorder{}
	agent := NewAssistantAgent(config.AIConfig{Enabled: false}, gw, &fakeRecordReader{}, rec, nil)

	_, err := agent.Ask(context.Background(), callerScope(), "t1", "", []ChatMessage{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v; want ErrDisabled", err)
	}
	if len(rec.runs) != 0 {
		t.Fatalf("a disabled agent must write no ledger rows; got %d", len(rec.runs))
	}
}

// TestAssistantAgent_Ask_RecordDraftThreaded (⌘K-can-act): a propose_record_create tool call
// lands in Result.Draft — the executor's buffer is threaded out exactly like the builder's, so
// the handler can return drafts[] for the palette's confirm cards. Still ZERO writes anywhere.
func TestAssistantAgent_Ask_RecordDraftThreaded(t *testing.T) {
	gw := &scriptedGateway{
		t: t,
		responses: []ChatResponse{
			toolCallResp("call_1", "propose_record_create", `{"entity":"tickets","data":{"title":"Khách A lỗi đăng nhập"}}`),
			finalTextResp("Đã soạn bản nháp — bấm Xác nhận để lưu."),
		},
	}
	rd := &fakeRecordReader{fields: ticketFields()}
	agent := NewAssistantAgent(enabledCfg(6), gw, rd, nil, nil)

	res, err := agent.Ask(context.Background(), callerScope(), "tenant-A", "",
		[]ChatMessage{{Role: "user", Content: "tạo ticket cho khách A: lỗi đăng nhập"}})
	if err != nil {
		t.Fatalf("Ask() error = %v; want nil", err)
	}
	if len(res.Draft) != 1 || res.Draft[0].Kind != "record_create" || res.Draft[0].RecordCreate == nil {
		t.Fatalf("res.Draft = %+v; want the one record_create draft threaded out", res.Draft)
	}
	if res.Draft[0].RecordCreate.Entity != "tickets" || res.Draft[0].RecordCreate.Data["title"] != "Khách A lỗi đăng nhập" {
		t.Fatalf("draft payload = %+v; want the proposed record", res.Draft[0].RecordCreate)
	}
}

// TestAssistantAgent_Ask_MaxSteps: a model that never stops tool-calling terminates bounded with
// outcome max_steps and a nil error (HTTP 200 + warning at the handler).
func TestAssistantAgent_Ask_MaxSteps(t *testing.T) {
	gw := &scriptedGateway{
		t: t,
		responses: []ChatResponse{
			toolCallResp("c1", "list_entities", "{}"),
			toolCallResp("c2", "list_entities", "{}"),
		},
	}
	agent := NewAssistantAgent(enabledCfg(2), gw, &fakeRecordReader{}, nil, nil)

	res, err := agent.Ask(context.Background(), callerScope(), "t1", "", []ChatMessage{{Role: "user", Content: "?"}})
	if err != nil {
		t.Fatalf("Ask() error = %v; want nil (bounded loop)", err)
	}
	if res.Outcome != outcomeMaxSteps {
		t.Fatalf("outcome = %q; want max_steps", res.Outcome)
	}
}
