package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/config"
)

// FeatureBuilderChat is the sys_ai_runs.feature tag for every AI-Builder chat turn (spec §5).
const FeatureBuilderChat = "ai_builder.chat"

// Outcome values recorded in the ledger (spec §5). They double as the response's outcome field so
// the FE can badge a max_steps/partial turn.
const (
	outcomeOK           = "ok"
	outcomeInvalidDraft = "invalid_draft"
	outcomeGatewayError = "gateway_error"
	outcomeTimeout      = "timeout"
	outcomeMaxSteps     = "max_steps"
	outcomeRefused      = "refused" // the model declined (finish_reason "refusal"); the reply is empty
)

// Completer is the gateway surface the agent needs (satisfied by *Gateway). Declared consumer-side
// so unit tests inject a scripted mock instead of a live HTTP client.
type Completer interface {
	Complete(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// Scope is the verified principal handed to a Chat call, derived from the request JWT by the
// handler (mirrors services.RecordScope). Only UserID is used by M1 (ledger attribution); RoleID/
// IsAdmin are carried for a future ACL gate on the read tools.
type Scope struct {
	UserID  string
	RoleID  string
	IsAdmin bool
}

// Result bundles everything a Chat turn returns to the handler: the assistant's final reply, the
// accumulated (unapplied) draft, the token usage, the ledger run id, and the terminal outcome.
type Result struct {
	Reply   string
	Draft   []DraftItem
	Usage   Usage
	RunID   string
	Outcome string
}

// StepEvent phases — the observable moments of one agentic loop iteration, in emit order:
// thinking (a gateway round-trip is about to start) → calling_tool/tool_done per tool call →
// final (the model produced its final text). Values are part of the SSE wire contract.
const (
	StepPhaseThinking    = "thinking"
	StepPhaseCallingTool = "calling_tool"
	StepPhaseToolDone    = "tool_done"
	StepPhaseFinal       = "final"
)

// StepEvent is one progress beat of the agentic loop, delivered to ChatWithProgress's optional
// callback so a streaming handler can render live step progress instead of a blind multi-minute
// wait. JSON tags are the camelCase wire shape the SSE endpoint forwards verbatim. It carries NO
// tenant/user data — only loop mechanics plus the tool name (already shown to the same admin in
// the draft anyway).
type StepEvent struct {
	Step     int    `json:"step"`     // 1-based loop iteration
	MaxSteps int    `json:"maxSteps"` // configured bound, so the FE can show "step 2/6"
	Phase    string `json:"phase"`    // one of the StepPhase* constants
	Tool     string `json:"tool,omitempty"`
}

// AIBuilderAgent runs the tenant-safe agentic loop (spec §4): persona + grounding → gateway with
// tools → execute tenant-scoped tools → repeat until a final text answer or MaxSteps. It owns the
// tools and the tenant guard; the gateway only relays messages. gateway/reader/recorder are
// injected so production wires the real HTTP gateway + metadata repo + sqlx ledger while tests wire
// fakes.
type AIBuilderAgent struct {
	cfg      config.AIConfig
	gateway  Completer
	reader   MetadataReader
	docs     *DocReaders // get_layout / list_workflows / list_reports / get_app (specs/067)
	recorder RunRecorder // nil → runs are not persisted (best-effort ledger)
	logger   *zap.Logger
	delivery DeliveryChecker // nil → no "already delivered" grounding line (specs/067 L10-5)
}

// DeliveryChecker says whether the tenant's project has been handed to the customer
// (= *services.DraftGate). Once delivered, edits to rules/reports/screens/menus land in the draft.
type DeliveryChecker interface {
	Delivered(ctx context.Context, tenantID string) (bool, error)
}

// WithDelivery wires the delivered-state source; returns the agent for chaining at construction.
func (a *AIBuilderAgent) WithDelivery(d DeliveryChecker) *AIBuilderAgent {
	a.delivery = d
	return a
}

// deliveredLine is the single grounding line for a delivered project (plan §9).
const deliveredLine = "\nDự án ĐÃ GIAO cho khách: luật/báo cáo/màn/menu Áp dụng vào bản nháp (khách thấy khi Xuất bản); bảng/trường mới ẩn với khách tới Xuất bản; vai/quyền áp ngay; không tự đề nghị dữ liệu mẫu."

// NewAIBuilderAgent wires the agent. A nil logger is replaced with a no-op; a nil recorder disables
// ledger persistence (the agent still runs). gateway and reader must be non-nil in production.
func NewAIBuilderAgent(cfg config.AIConfig, gateway Completer, reader MetadataReader, docs *DocReaders, recorder RunRecorder, logger *zap.Logger) *AIBuilderAgent {
	if logger == nil {
		logger = zap.NewNop()
	}
	// One warning per construction (the ai_handler nil-guard precedent): explain_access degrades
	// honestly without probes, but silently — this shipped unwired for a month before the
	// 2026-09-01 security audit caught it, so an unwired production build must be loud at startup.
	if c, ok := reader.(AccessProbeCarrier); !ok || !c.AccessProbes().complete() {
		logger.Warn("ai: builder agent constructed WITHOUT access probes — explain_access will answer ACCESS_EXPLAIN_UNAVAILABLE (production wires ReaderWithAccessProbes; test fakes may ignore this)")
	}
	if docs == nil || docs.Layouts == nil || docs.Workflows == nil || docs.Reports == nil || docs.Apps == nil {
		logger.Warn("ai: builder agent constructed WITHOUT doc readers — get_layout/list_workflows/list_reports/get_app will answer DOC_READ_UNAVAILABLE")
	}
	return &AIBuilderAgent{cfg: cfg, gateway: gateway, reader: reader, docs: docs, recorder: recorder, logger: logger}
}

// Chat runs one turn of the conversation. messages is the history the HANDLER rebuilds from the
// server-held session (spec §6/§11 — the M2 client-held messages[] MVP is retired). model overrides cfg.Model when non-empty (honors the API's optional
// `model`). It returns the final reply + the draft the model proposed this turn (nothing applied).
//
// Contract:
//   - Disabled config → ErrDisabled immediately, WITHOUT contacting the gateway or reading metadata
//     (honest-disabled; a disabled agent touches nothing) and WITHOUT writing a ledger row.
//   - Gateway timeout → ErrTimeout (504); gateway/transport/decode failure → ErrGateway (502).
//   - MaxSteps reached without a final text → outcome max_steps, partial draft, nil error (HTTP 200
//     with a warning — the tool-loop is bounded, never infinite).
//
// Every terminal path except Disabled writes ONE sys_ai_runs row (deferred) with the final outcome
// and measured latency — a failed/timed-out turn is exactly the cost/audit data the factory needs.
func (a *AIBuilderAgent) Chat(ctx context.Context, scope Scope, tenantID, model string, messages []ChatMessage) (Result, error) {
	return a.ChatWithProgress(ctx, scope, tenantID, model, TurnContext{}, messages, nil)
}

// ChatWithProgress is Chat plus an OPTIONAL step-event callback (nil = identical to Chat). onStep
// is invoked synchronously, in-loop: "thinking" before every gateway round-trip, "calling_tool"/
// "tool_done" around every tool execution, "final" when the model lands its final text. Progress
// is best-effort observability — a panicking callback is recovered and logged, never breaking the
// turn — and the callback must be fast (it blocks the loop; the SSE handler just writes+flushes).
//
// turn is the server-RESOLVED screen context (zero value = none, the pre-067 shape): when Present
// it adds one extra system message after the grounding — see contextBlock.
func (a *AIBuilderAgent) ChatWithProgress(ctx context.Context, scope Scope, tenantID, model string, turn TurnContext, messages []ChatMessage, onStep func(StepEvent)) (Result, error) {
	// Advisor #4: the disabled check is the FIRST statement — no grounding read, no gateway call,
	// no ledger row. A disabled agent is completely inert.
	if !a.cfg.Enabled {
		return Result{Outcome: "disabled"}, ErrDisabled
	}

	if model == "" {
		model = a.cfg.Model
	}

	start := time.Now()
	runID := uuid.NewString()
	promptHash := sha256Hex(lastUserMessage(messages))

	// Ledger accounting — mutated as the loop progresses, read by the deferred writer at return.
	var (
		usage     Usage // summed over every billed call of the turn
		toolCalls int
		outcome   = outcomeGatewayError // pessimistic default until a terminal path sets it
	)
	// Advisor #1: record the run on EVERY non-disabled terminal path (success, error, timeout,
	// max_steps) with measured latency — write-on-success-only would silently drop failed turns.
	defer func() {
		a.recordRun(ctx, Run{
			ID:               runID,
			TenantID:         tenantID,
			UserID:           scope.UserID,
			Feature:          FeatureBuilderChat,
			Model:            model,
			PromptHash:       promptHash,
			PromptTokens:     usage.PromptTokens,
			OutputTokens:     usage.CompletionTokens,
			CacheReadTokens:  usage.CacheReadTokens,
			CacheWriteTokens: usage.CacheWriteTokens,
			ToolCalls:        toolCalls,
			Outcome:          outcome,
			LatencyMS:        int(time.Since(start).Milliseconds()),
		})
	}()

	exec := &toolExecutor{reader: a.reader, docs: a.docs, tenantID: tenantID, turnTag: runID, logger: a.logger}

	// Assemble the conversation: persona system prompt + tenant grounding + the client history.
	convo := make([]ChatMessage, 0, len(messages)+2)
	convo = append(convo, ChatMessage{Role: "system", Content: Persona()})
	convo = append(convo, ChatMessage{Role: "system", Content: a.grounding(ctx, exec, tenantID)})
	if turn.Present {
		convo = append(convo, ChatMessage{Role: "system", Content: contextBlock(turn)})
	}
	convo = append(convo, messages...)

	tools := ToolDefinitions()

	for step := 0; step < a.cfg.MaxSteps; step++ {
		a.emitStep(onStep, StepEvent{Step: step + 1, MaxSteps: a.cfg.MaxSteps, Phase: StepPhaseThinking})
		resp, err := a.gateway.Complete(ctx, ChatRequest{
			Model:     model,
			Messages:  convo,
			Tools:     tools,
			MaxTokens: a.cfg.MaxTokens,
		})
		if err != nil {
			if errors.Is(err, ErrTimeout) {
				outcome = outcomeTimeout
				return Result{RunID: runID, Draft: exec.draft, Outcome: outcome}, fmt.Errorf("ai.Chat: %w", err)
			}
			outcome = outcomeGatewayError
			return Result{RunID: runID, Draft: exec.draft, Outcome: outcome}, fmt.Errorf("ai.Chat: %w", err)
		}

		// Accumulate real token spend across every billed call in the turn (each call is a
		// separate gateway round-trip → its tokens are a real cost, so we SUM, not overwrite).
		usage.add(resp.Usage)

		// A refusal ends the turn before its partial text or tool calls are used; proposals from earlier calls
		// stay, as on max_steps. The outcome is what tells the panel the model declined (issue #30 item 5). It is
		// read before the choices: Claude's top-level stop_reason may come with none.
		if resp.Refused() {
			a.emitStep(onStep, StepEvent{Step: step + 1, MaxSteps: a.cfg.MaxSteps, Phase: StepPhaseFinal})
			outcome = outcomeRefused
			return Result{Draft: finalizeDraft(exec.draft, turn), Usage: usage, RunID: runID, Outcome: outcome}, nil
		}

		if len(resp.Choices) == 0 {
			outcome = outcomeGatewayError
			return Result{RunID: runID, Draft: exec.draft, Outcome: outcome},
				fmt.Errorf("ai.Chat: %w: gateway returned no choices", ErrGateway)
		}
		msg := resp.Choices[0].Message

		// Final text (no tool calls) → the turn is done.
		if len(msg.ToolCalls) == 0 {
			a.emitStep(onStep, StepEvent{Step: step + 1, MaxSteps: a.cfg.MaxSteps, Phase: StepPhaseFinal})
			outcome = outcomeOK
			return Result{
				Reply:   msg.Content,
				Draft:   finalizeDraft(exec.draft, turn),
				Usage:   usage,
				RunID:   runID,
				Outcome: outcome,
			}, nil
		}

		// Tool calls → append the assistant message CARRYING the tool_calls, then one tool-result
		// message per call echoing the matching tool_call_id (advisor #6 — a mismatched/omitted id
		// silently breaks the multi-step loop). Execute each tool tenant-scoped.
		convo = append(convo, msg)
		for _, tc := range msg.ToolCalls {
			toolCalls++
			a.emitStep(onStep, StepEvent{Step: step + 1, MaxSteps: a.cfg.MaxSteps, Phase: StepPhaseCallingTool, Tool: tc.Function.Name})
			content := exec.execute(ctx, tc.Function.Name, tc.Function.Arguments)
			a.emitStep(onStep, StepEvent{Step: step + 1, MaxSteps: a.cfg.MaxSteps, Phase: StepPhaseToolDone, Tool: tc.Function.Name})
			convo = append(convo, ChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    content,
			})
		}
	}

	// MaxSteps exhausted without a final text answer → return the partial draft with a warning
	// (HTTP 200). The loop is bounded so a runaway model can never spin cost/latency unbounded.
	outcome = outcomeMaxSteps
	return Result{
		Draft:   finalizeDraft(exec.draft, turn),
		Usage:   usage,
		RunID:   runID,
		Outcome: outcome,
	}, nil
}

// RunTool runs ONE builder tool outside a chat turn, for the MCP surface (internal/mcpserver): the
// same tenant-scoped executor and validators as a Chat turn, with no gateway call and no ledger
// row, so it also runs with AI disabled. prior holds the thread's earlier proposals, so a tool that
// accepts "an entity proposed this turn" accepts one an earlier call proposed; tag namespaces the
// @refs of propose_records per thread, as the run id does per turn. It returns the tool's JSON
// result and the proposals this call added, each with its change envelope.
func (a *AIBuilderAgent) RunTool(ctx context.Context, tenantID, tag string, prior []DraftItem, name, argsJSON string) (string, []DraftItem) {
	exec := &toolExecutor{reader: a.reader, docs: a.docs, tenantID: tenantID, turnTag: tag, draft: append([]DraftItem(nil), prior...), logger: a.logger}
	res := exec.execute(ctx, name, argsJSON)
	return res, exec.draft[len(prior):]
}

// finalizeDraft stamps entity.appName with the app the turn ran in (D6) at the ONE place every
// result-with-draft passes through. A table that a propose_app of the SAME turn already packs
// into its menu is left unstamped (that card attaches it; stamping too would double-add).
func finalizeDraft(draft []DraftItem, turn TurnContext) []DraftItem {
	if turn.App == nil {
		return draft
	}
	packed := map[string]bool{}
	for _, it := range draft {
		if it.Kind == "app" && it.App != nil {
			for _, n := range it.App.NavEntities {
				packed[n] = true
			}
		}
	}
	for i := range draft {
		if draft[i].Kind == "entity" && draft[i].Entity != nil && !packed[draft[i].Entity.Name] {
			draft[i].Entity.AppName = turn.App.Name
		}
	}
	return draft
}

// emitStep delivers one progress event to the optional callback. Progress is best-effort
// observability — the turn's Result is the contract — so a panicking callback is recovered and
// logged here and can never break the agent loop. nil callback = no-op (the plain Chat path).
func (a *AIBuilderAgent) emitStep(onStep func(StepEvent), ev StepEvent) {
	if onStep == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			a.logger.Warn("ai: step-progress callback panicked — event dropped, turn continues",
				zap.Any("panic", r),
				zap.String("phase", ev.Phase))
		}
	}()
	onStep(ev)
}

// grounding builds the minimal context system message: the tenant's current entities, so the model
// knows what already exists before proposing (spec §4). It reuses the SAME tenant-scoped executor
// as the loop, so grounding can never read another tenant. A read error degrades to embedding the
// {"ok":false,"error":...} tool-error envelope in the grounding note (harmless — the model can still
// call list_entities itself) rather than failing the turn.
func (a *AIBuilderAgent) grounding(ctx context.Context, exec *toolExecutor, tenantID string) string {
	list := exec.listEntities()
	g := "Bối cảnh tenant hiện tại — entity đang có (JSON, chỉ để tham khảo, KHÔNG phải dữ liệu để bịa thêm): " + list
	if a.delivery == nil {
		return g
	}
	delivered, err := a.delivery.Delivered(ctx, tenantID)
	if err != nil {
		// Cautious: a false "not delivered" would let the AI offer live-looking sample data on a customer project.
		a.logger.Warn("ai: delivered-state lookup failed — treating the project as delivered",
			zap.String("tenant_id", tenantID), zap.Error(err))
		delivered = true
	}
	if delivered {
		g += deliveredLine
	}
	return g
}

// recordRun persists one ledger row best-effort (fail-open): a ledger write problem logs a warning
// but never affects the chat result. A nil recorder skips persistence entirely.
//
// The write is DETACHED from the request context (context.WithoutCancel + a short own deadline):
// the ledger must survive a cancelled/timed-out/disconnected request, because those are exactly the
// failed turns worth auditing (a gateway_error/timeout row would otherwise be silently lost when the
// request ctx that triggered it is already Done).
func (a *AIBuilderAgent) recordRun(ctx context.Context, run Run) {
	// Usage canary (post-review security LOW-1): a SUCCESSFUL turn (outcome "ok" — the gateway did
	// respond with a final answer) that reports ZERO tokens on both sides is suspicious, not
	// impossible — a gateway that stops returning a `usage` block would silently turn M4's daily
	// token budget (BudgetGuard, spec §12) into a no-op: every turn would look free and the tenant
	// could never be capped. This never blocks the turn (the reply already succeeded) — it only
	// makes the condition observable in ops logs. A replayed turn (AI_GATEWAY_MODE=replay) is free by
	// design, so it stays quiet.
	if run.Outcome == outcomeOK && run.PromptTokens == 0 && run.OutputTokens == 0 && !replays(a.gateway) {
		a.logger.Warn("ai: gateway returned no usage on a successful turn — daily budget not accruing for this run",
			zap.String("tenant_id", run.TenantID),
			zap.String("run_id", run.ID))
	}
	if a.recorder == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.recorder.Record(writeCtx, run); err != nil {
		a.logger.Warn("ai: failed to record ai_run ledger row",
			zap.String("tenant_id", run.TenantID),
			zap.String("run_id", run.ID),
			zap.String("outcome", run.Outcome),
			zap.Error(err))
	}
}

// lastUserMessage returns the content of the last role:"user" message, or "" when there is none.
// It is what prompt_hash is computed over (spec §5: hash the last user message, never store raw).
func lastUserMessage(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// sha256Hex returns the lowercase hex sha256 of s (64 chars), matching the CHAR(64) prompt_hash
// column. Hashing (not storing) the prompt keeps customer business data out of the ledger.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
