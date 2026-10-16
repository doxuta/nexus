package ai

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"nexus/platform/internal/config"
)

// scriptedGateway returns a pre-scripted sequence of responses/errors and records the messages of
// every call so a test can assert the tool_call_id echo (advisor #6).
type scriptedGateway struct {
	responses  []ChatResponse
	errs       []error
	calls      [][]ChatMessage
	t          *testing.T
	failIfUsed bool
}

func (s *scriptedGateway) Complete(_ context.Context, req ChatRequest) (ChatResponse, error) {
	if s.failIfUsed {
		s.t.Fatal("gateway.Complete called; a disabled agent must NOT contact the gateway")
	}
	i := len(s.calls)
	s.calls = append(s.calls, req.Messages)
	if i < len(s.errs) && s.errs[i] != nil {
		return ChatResponse{}, s.errs[i]
	}
	if i >= len(s.responses) {
		return ChatResponse{}, fmt.Errorf("scriptedGateway: no response scripted for call %d", i)
	}
	return s.responses[i], nil
}

// fakeRecorder captures the ledger rows the agent writes.
type fakeRecorder struct {
	runs []Run
	err  error
}

func (f *fakeRecorder) Record(_ context.Context, run Run) error {
	f.runs = append(f.runs, run)
	return f.err
}

func toolCallResp(id, name, args string) ChatResponse {
	return ChatResponse{
		Choices: []ChatChoice{{Message: ChatMessage{
			Role:      "assistant",
			ToolCalls: []ToolCall{{ID: id, Type: "function", Function: ToolCallFunc{Name: name, Arguments: args}}},
		}, FinishReason: "tool_calls"}},
		Usage: Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
	}
}

func finalTextResp(text string) ChatResponse {
	return ChatResponse{
		Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: text}, FinishReason: "stop"}},
		Usage:   Usage{PromptTokens: 12, CompletionTokens: 6, TotalTokens: 18},
	}
}

func enabledCfg(maxSteps int) config.AIConfig {
	return config.AIConfig{Enabled: true, Model: "cc/test", TimeoutSec: 5, MaxTokens: 1024, MaxSteps: maxSteps}
}

func TestAgent_Chat_ToolLoop_ProducesDraft(t *testing.T) {
	gw := &scriptedGateway{
		t: t,
		responses: []ChatResponse{
			toolCallResp("call_1", "list_entities", "{}"),
			toolCallResp("call_2", "propose_entity",
				`{"name":"appointments","label":"Lịch hẹn","fields":[{"name":"start_at","label":"Bắt đầu","fieldType":"datetime","required":true}]}`),
			finalTextResp("Tôi đã đề xuất entity appointments. Anh xem draft rồi bấm Apply."),
		},
	}
	rec := &fakeRecorder{}
	agent := NewAIBuilderAgent(enabledCfg(6), gw, twoTenantReader(), nil, rec, nil)

	msgs := []ChatMessage{{Role: "user", Content: "Tôi làm phòng khám, cần quản lý lịch hẹn"}}
	res, err := agent.Chat(context.Background(), Scope{UserID: "u1"}, "tenant-A", "", msgs)
	if err != nil {
		t.Fatalf("Chat() error = %v; want nil", err)
	}
	if res.Outcome != outcomeOK {
		t.Fatalf("outcome = %q; want ok", res.Outcome)
	}
	if len(res.Draft) != 1 || res.Draft[0].Kind != "entity" || res.Draft[0].Entity.Name != "appointments" {
		t.Fatalf("draft = %+v; want one entity draft 'appointments'", res.Draft)
	}
	if res.Reply == "" {
		t.Fatal("reply is empty; want the final assistant text")
	}
	// Tokens summed across the 3 billed calls: prompt 10+10+12=32, completion 4+4+6=14.
	if res.Usage.PromptTokens != 32 || res.Usage.CompletionTokens != 14 {
		t.Fatalf("usage = %+v; want summed prompt=32 completion=14", res.Usage)
	}

	// Ledger: exactly one row, outcome ok, tool_calls=2, prompt HASHED (never the raw message).
	if len(rec.runs) != 1 {
		t.Fatalf("recorder rows = %d; want 1", len(rec.runs))
	}
	run := rec.runs[0]
	if run.Outcome != outcomeOK || run.ToolCalls != 2 || run.Feature != FeatureBuilderChat {
		t.Fatalf("ledger row = %+v; want outcome=ok toolCalls=2 feature=ai_builder.chat", run)
	}
	if run.PromptHash != sha256Hex("Tôi làm phòng khám, cần quản lý lịch hẹn") {
		t.Fatalf("prompt_hash = %q; want sha256 of the last user message", run.PromptHash)
	}
	if len(run.PromptHash) != 64 {
		t.Fatalf("prompt_hash length = %d; want 64 hex chars", len(run.PromptHash))
	}

	// Advisor #6: the tool-result messages must echo the matching tool_call_id. The 2nd gateway
	// call must contain a tool message with ToolCallID "call_1"; the 3rd with "call_2".
	assertToolIDEcho(t, gw.calls[1], "call_1")
	assertToolIDEcho(t, gw.calls[2], "call_2")

	// W3 (review): the agent MUST prepend its own soul — the persona system prompt then the tenant
	// grounding — as the first two messages of the FIRST gateway call, followed by the client turn.
	// Without this assertion, deleting the Persona() prepend (the feature's entire "soul") would keep
	// every test green. This guards it: message[0] must be exactly Persona().
	first := gw.calls[0]
	if len(first) < 3 {
		t.Fatalf("first gateway call must carry persona + grounding + user turn; got %d messages", len(first))
	}
	if first[0].Role != "system" || first[0].Content != Persona() {
		t.Fatalf("message[0] must be the persona system prompt (the feature's soul); got role=%q", first[0].Role)
	}
	if first[1].Role != "system" || first[1].Content == "" || first[1].Content == Persona() {
		t.Fatalf("message[1] must be the distinct tenant-grounding system note; got role=%q", first[1].Role)
	}
	if first[2].Role != "user" {
		t.Fatalf("message[2] must be the client user turn; got role=%q", first[2].Role)
	}
}

func assertToolIDEcho(t *testing.T, msgs []ChatMessage, wantID string) {
	t.Helper()
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == wantID {
			return
		}
	}
	t.Fatalf("no tool message echoing tool_call_id %q found in %d messages", wantID, len(msgs))
}

func TestAgent_Chat_Disabled_NoGatewayNoLedger(t *testing.T) {
	gw := &scriptedGateway{t: t, failIfUsed: true}
	rec := &fakeRecorder{}
	agent := NewAIBuilderAgent(config.AIConfig{Enabled: false, Model: "cc/test", MaxSteps: 6}, gw, twoTenantReader(), nil, rec, nil)

	_, err := agent.Chat(context.Background(), Scope{UserID: "u1"}, "tenant-A", "", []ChatMessage{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("Chat() error = %v; want ErrDisabled", err)
	}
	if len(gw.calls) != 0 {
		t.Fatalf("gateway was called %d times; a disabled agent must not contact it", len(gw.calls))
	}
	if len(rec.runs) != 0 {
		t.Fatalf("ledger rows = %d; a disabled turn must not write the ledger", len(rec.runs))
	}
}

func TestAgent_Chat_MaxSteps_PartialDraft(t *testing.T) {
	// Every response returns a tool call → the loop never sees a final text and hits MaxSteps=2.
	gw := &scriptedGateway{
		t: t,
		responses: []ChatResponse{
			toolCallResp("c1", "propose_field", `{"entity":"patients","name":"phone","label":"SĐT","fieldType":"text"}`),
			toolCallResp("c2", "list_entities", "{}"),
			toolCallResp("c3", "list_entities", "{}"), // never reached (MaxSteps=2)
		},
	}
	rec := &fakeRecorder{}
	agent := NewAIBuilderAgent(enabledCfg(2), gw, twoTenantReader(), nil, rec, nil)

	res, err := agent.Chat(context.Background(), Scope{UserID: "u1"}, "tenant-A", "", []ChatMessage{{Role: "user", Content: "thêm field"}})
	if err != nil {
		t.Fatalf("Chat() error = %v; want nil (max_steps is a 200, not an error)", err)
	}
	if res.Outcome != outcomeMaxSteps {
		t.Fatalf("outcome = %q; want max_steps", res.Outcome)
	}
	if len(gw.calls) != 2 {
		t.Fatalf("gateway calls = %d; want exactly MaxSteps=2", len(gw.calls))
	}
	// The one valid propose_field from step 1 is still returned as a partial draft.
	if len(res.Draft) != 1 || res.Draft[0].Kind != "field" {
		t.Fatalf("partial draft = %+v; want the one buffered field", res.Draft)
	}
	if len(rec.runs) != 1 || rec.runs[0].Outcome != outcomeMaxSteps {
		t.Fatalf("ledger = %+v; want one row outcome=max_steps", rec.runs)
	}
}

// TestAgent_ChatWithProgress_EmitsStepEvents pins the step-event wire contract: "thinking" before
// every gateway round-trip, "calling_tool"/"tool_done" around every tool execution, "final" when
// the model lands its final text — with 1-based step numbers and the configured MaxSteps. The
// result must be identical to a plain Chat turn (progress is pure observability).
func TestAgent_ChatWithProgress_EmitsStepEvents(t *testing.T) {
	gw := &scriptedGateway{
		t: t,
		responses: []ChatResponse{
			toolCallResp("call_1", "list_entities", "{}"),
			finalTextResp("Xong."),
		},
	}
	agent := NewAIBuilderAgent(enabledCfg(6), gw, twoTenantReader(), nil, nil, nil)

	var got []StepEvent
	res, err := agent.ChatWithProgress(context.Background(), Scope{UserID: "u1"}, "tenant-A", "", TurnContext{},
		[]ChatMessage{{Role: "user", Content: "hi"}},
		func(ev StepEvent) { got = append(got, ev) })
	if err != nil {
		t.Fatalf("ChatWithProgress() error = %v; want nil", err)
	}
	if res.Outcome != outcomeOK || res.Reply == "" {
		t.Fatalf("result = %+v; progress must not change the turn's outcome", res)
	}
	want := []StepEvent{
		{Step: 1, MaxSteps: 6, Phase: StepPhaseThinking},
		{Step: 1, MaxSteps: 6, Phase: StepPhaseCallingTool, Tool: "list_entities"},
		{Step: 1, MaxSteps: 6, Phase: StepPhaseToolDone, Tool: "list_entities"},
		{Step: 2, MaxSteps: 6, Phase: StepPhaseThinking},
		{Step: 2, MaxSteps: 6, Phase: StepPhaseFinal},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("step events =\n%+v\nwant\n%+v", got, want)
	}
}

// TestAgent_ChatWithProgress_PanickingCallback_TurnSurvives: a broken/panicking callback (e.g. a
// write to a dead SSE connection surfacing as a panic in a future refactor) must NEVER break the
// turn — the events are dropped, the result is intact.
func TestAgent_ChatWithProgress_PanickingCallback_TurnSurvives(t *testing.T) {
	gw := &scriptedGateway{
		t: t,
		responses: []ChatResponse{
			toolCallResp("call_1", "list_entities", "{}"),
			finalTextResp("Vẫn xong."),
		},
	}
	agent := NewAIBuilderAgent(enabledCfg(6), gw, twoTenantReader(), nil, nil, nil)

	res, err := agent.ChatWithProgress(context.Background(), Scope{UserID: "u1"}, "tenant-A", "", TurnContext{},
		[]ChatMessage{{Role: "user", Content: "hi"}},
		func(StepEvent) { panic("callback boom") })
	if err != nil {
		t.Fatalf("ChatWithProgress() error = %v; a panicking callback must not fail the turn", err)
	}
	if res.Outcome != outcomeOK || res.Reply != "Vẫn xong." {
		t.Fatalf("result = %+v; want the normal ok outcome despite the panicking callback", res)
	}
	if len(gw.calls) != 2 {
		t.Fatalf("gateway calls = %d; want 2 (the loop must keep running through callback panics)", len(gw.calls))
	}
}

func TestAgent_Chat_GatewayTimeout_MapsTimeout(t *testing.T) {
	gw := &scriptedGateway{t: t, errs: []error{fmt.Errorf("boom: %w", ErrTimeout)}}
	rec := &fakeRecorder{}
	agent := NewAIBuilderAgent(enabledCfg(6), gw, twoTenantReader(), nil, rec, nil)

	_, err := agent.Chat(context.Background(), Scope{UserID: "u1"}, "tenant-A", "", []ChatMessage{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Chat() error = %v; want ErrTimeout", err)
	}
	if len(rec.runs) != 1 || rec.runs[0].Outcome != outcomeTimeout {
		t.Fatalf("ledger = %+v; want one row outcome=timeout (failed turns MUST be recorded)", rec.runs)
	}
}

func TestAgent_Chat_GatewayError_MapsGatewayError(t *testing.T) {
	gw := &scriptedGateway{t: t, errs: []error{fmt.Errorf("boom: %w", ErrGateway)}}
	rec := &fakeRecorder{}
	agent := NewAIBuilderAgent(enabledCfg(6), gw, twoTenantReader(), nil, rec, nil)

	_, err := agent.Chat(context.Background(), Scope{UserID: "u1"}, "tenant-A", "", []ChatMessage{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrGateway) {
		t.Fatalf("Chat() error = %v; want ErrGateway", err)
	}
	if len(rec.runs) != 1 || rec.runs[0].Outcome != outcomeGatewayError {
		t.Fatalf("ledger = %+v; want one row outcome=gateway_error", rec.runs)
	}
}
