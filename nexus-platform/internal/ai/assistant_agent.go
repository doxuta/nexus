// assistant_agent.go — AssistantAgent, the ⌘K end-user ask-data agent. Option (b) of the slice
// design: a THIN sibling of AIBuilderAgent that reuses the shared plumbing UNCHANGED — the same
// Completer (gateway), config.AIConfig knobs, RunRecorder ledger, BudgetGuard and SessionStore
// wire around it in main.go — while owning its OWN persona, tool registry and executor. The
// builder agent is deliberately not parameterized/forked: its loop keeps accumulating drafts and
// grounding on the admin-wide metadata reader, none of which the assistant may do.
package ai

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/config"
	"nexus/platform/internal/services"
)

// FeatureAssistantAsk is the sys_ai_runs.feature tag for every ⌘K assistant turn. It shares the
// ledger (and therefore the per-tenant daily token budget — TokensSince sums ALL features) with
// the builder, but stays distinguishable for cost/audit queries.
const FeatureAssistantAsk = "assistant.ask"

// AssistantAgent runs the ask-data loop: persona + ACL-filtered grounding → gateway with the
// read + HITL-draft tools → execute caller-scoped reads (and buffer record proposals) → repeat
// until a final text answer or MaxSteps. Its Draft can carry ONLY the two record kinds
// (record_create/record_update) — nothing is ever applied here; the user confirms in the palette
// and the FE writes through POST /:entity / PUT /:entity/:id under the caller's own token.
type AssistantAgent struct {
	cfg      config.AIConfig
	gateway  Completer
	reader   RecordReader
	recorder RunRecorder // nil → runs are not persisted (best-effort ledger, like the builder)
	logger   *zap.Logger
}

// NewAssistantAgent wires the assistant. A nil logger is replaced with a no-op; a nil recorder
// disables ledger persistence. gateway and reader must be non-nil in production.
func NewAssistantAgent(cfg config.AIConfig, gateway Completer, reader RecordReader, recorder RunRecorder, logger *zap.Logger) *AssistantAgent {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AssistantAgent{cfg: cfg, gateway: gateway, reader: reader, recorder: recorder, logger: logger}
}

// Ask runs one turn of the assistant conversation. recordScope is the caller's OWN row-level
// access scope, built by the HANDLER via scopeOf from the verified JWT (trap #1) — it carries
// UserID/RoleID/IsAdmin/AccessLevel and is threaded to the tool executor VERBATIM; the agent
// never widens, narrows or rebuilds it. tenantID likewise comes from the verified claim, one-way.
//
// Contract (mirrors AIBuilderAgent.Chat):
//   - Disabled config → ErrDisabled immediately, no gateway/read/ledger touch.
//   - Gateway timeout → ErrTimeout (504); transport/decode failure → ErrGateway (502).
//   - MaxSteps without a final text → outcome max_steps, nil error (bounded loop, HTTP 200).
//   - Every non-disabled terminal path writes ONE sys_ai_runs row (feature assistant.ask).
func (a *AssistantAgent) Ask(ctx context.Context, recordScope services.RecordScope, tenantID, model string, messages []ChatMessage) (Result, error) {
	if !a.cfg.Enabled {
		return Result{Outcome: "disabled"}, ErrDisabled
	}

	if model == "" {
		model = a.cfg.Model
	}

	start := time.Now()
	runID := uuid.NewString()
	promptHash := sha256Hex(lastUserMessage(messages))

	var (
		usage     Usage // summed over every billed call of the turn
		toolCalls int
		outcome   = outcomeGatewayError // pessimistic default until a terminal path sets it
	)
	defer func() {
		a.recordRun(ctx, Run{
			ID:               runID,
			TenantID:         tenantID,
			UserID:           recordScope.UserID,
			Feature:          FeatureAssistantAsk,
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

	exec := &assistantExecutor{reader: a.reader, tenantID: tenantID, scope: recordScope}

	// Persona + grounding. The grounding reuses the SAME ACL-filtered executor tool as the loop,
	// so it can only ever name entities THIS caller may view (a read error degrades to embedding
	// the {"ok":false} envelope — harmless, the model can retry list_entities itself).
	convo := make([]ChatMessage, 0, len(messages)+2)
	convo = append(convo, ChatMessage{Role: "system", Content: AssistantPersona()})
	convo = append(convo, ChatMessage{Role: "system", Content: "Bối cảnh — các entity người dùng này được xem (JSON, chỉ để tham khảo): " + exec.listEntities(ctx)})
	convo = append(convo, messages...)

	tools := AssistantToolDefinitions()

	for step := 0; step < a.cfg.MaxSteps; step++ {
		resp, err := a.gateway.Complete(ctx, ChatRequest{
			Model:     model,
			Messages:  convo,
			Tools:     tools,
			MaxTokens: a.cfg.MaxTokens,
		})
		if err != nil {
			if errors.Is(err, ErrTimeout) {
				outcome = outcomeTimeout
				return Result{RunID: runID, Draft: exec.draft, Outcome: outcome}, fmt.Errorf("ai.Ask: %w", err)
			}
			outcome = outcomeGatewayError
			return Result{RunID: runID, Draft: exec.draft, Outcome: outcome}, fmt.Errorf("ai.Ask: %w", err)
		}

		// Sum real token spend across every billed call of the turn (same accounting as Chat).
		usage.add(resp.Usage)

		// A refusal ends the turn unused, as in the builder's loop (read before the choices, as there).
		if resp.Refused() {
			outcome = outcomeRefused
			return Result{Draft: exec.draft, Usage: usage, RunID: runID, Outcome: outcome}, nil
		}

		if len(resp.Choices) == 0 {
			outcome = outcomeGatewayError
			return Result{RunID: runID, Draft: exec.draft, Outcome: outcome},
				fmt.Errorf("ai.Ask: %w: gateway returned no choices", ErrGateway)
		}
		msg := resp.Choices[0].Message

		if len(msg.ToolCalls) == 0 {
			outcome = outcomeOK
			return Result{
				Reply:   msg.Content,
				Draft:   exec.draft,
				Usage:   usage,
				RunID:   runID,
				Outcome: outcome,
			}, nil
		}

		// Tool calls: append the assistant message CARRYING the tool_calls, then one tool-result
		// message per call echoing the matching tool_call_id (the builder's advisor-#6 lesson).
		convo = append(convo, msg)
		for _, tc := range msg.ToolCalls {
			toolCalls++
			content := exec.execute(ctx, tc.Function.Name, tc.Function.Arguments)
			convo = append(convo, ChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    content,
			})
		}
	}

	outcome = outcomeMaxSteps
	return Result{
		Draft:   exec.draft,
		Usage:   usage,
		RunID:   runID,
		Outcome: outcome,
	}, nil
}

// recordRun mirrors AIBuilderAgent.recordRun (fail-open, detached from the request ctx, with the
// zero-usage canary) — duplicated rather than extracted so this slice touches none of the
// builder's code paths. ponytail: fold both into one shared helper if a third agent ever appears.
func (a *AssistantAgent) recordRun(ctx context.Context, run Run) {
	if run.Outcome == outcomeOK && run.PromptTokens == 0 && run.OutputTokens == 0 && !replays(a.gateway) {
		a.logger.Warn("ai: gateway returned no usage on a successful assistant turn — daily budget not accruing for this run",
			zap.String("tenant_id", run.TenantID),
			zap.String("run_id", run.ID))
	}
	if a.recorder == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.recorder.Record(writeCtx, run); err != nil {
		a.logger.Warn("ai: failed to record assistant ai_run ledger row",
			zap.String("tenant_id", run.TenantID),
			zap.String("run_id", run.ID),
			zap.String("outcome", run.Outcome),
			zap.Error(err))
	}
}
