// Package ai implements the E9 conversational AI Builder (../specs/020-e9-ai-builder/spec.md):
// a tenant-safe, in-process agentic loop that talks to a local OpenAI-compatible LLM gateway
// to help a builder model an app from natural language. The agent PROPOSES
// metadata (entities/fields) as a draft; a human applies it through the existing gated
// /admin/entities + /admin/fields APIs (HITL). The AI never writes schema directly.
//
// Model A (see spec §3): Nexus owns the loop, the tools, and the tenant/ACL guards; the model brings
// only reasoning, under the PERSONA (persona_nexus_builder.md). Every tool read is pinned to the tenant taken from
// the request's verified JWT — never from anything the model says.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors the agent/handler map to HTTP status codes (spec §6). Callers use errors.Is.
//
//	ErrDisabled     → 503 AI_DISABLED       (AI_ENABLED=false; gateway never contacted)
//	ErrGateway      → 502 AI_GATEWAY_ERROR   (gateway non-2xx / transport failure / bad body)
//	ErrTimeout      → 504 AI_TIMEOUT         (per-turn deadline elapsed)
//	ErrInvalidDraft → 422 AI_INVALID_DRAFT   (a proposal was off-spec and unrecoverable; the
//	                                          normal loop instead feeds the validation error back
//	                                          to the model as a tool result so it self-corrects —
//	                                          this sentinel is a defensive fallback, wired but not
//	                                          raised by the happy path)
var (
	ErrDisabled     = errors.New("ai: builder disabled")
	ErrGateway      = errors.New("ai: gateway error")
	ErrTimeout      = errors.New("ai: gateway timeout")
	ErrInvalidDraft = errors.New("ai: invalid draft proposal")
)

// ChatMessage is one OpenAI-compatible chat message. Role is "system" | "user" | "assistant" |
// "tool". For an assistant turn that calls tools, ToolCalls is populated (and Content may be
// empty). For a tool RESULT message, Role is "tool", ToolCallID echoes the id of the call being
// answered, and Content carries the tool's JSON output. Name is optional (tool name on a tool
// message). Empty fields are omitted so the wire shape matches what the gateway expects.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is a single function call the model requested. Arguments is a RAW JSON string (the
// model serializes the arguments object as a string per the OpenAI function-calling contract);
// the executor unmarshals it.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function ToolCallFunc `json:"function"`
}

// ToolCallFunc is the {name, arguments} inside a tool call.
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a function tool advertised to the model. Type is always "function"; Function.Parameters
// is a JSON-Schema object describing the accepted arguments. CRITICAL: no tool schema declares a
// tenant parameter — the tenant is injected server-side from the JWT, so the model can never name
// a tenant (spec §7 tenant isolation, by construction).
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the {name, description, parameters} of a function tool.
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ChatRequest is the POST /chat/completions body. Stream is always false (the loop needs the whole
// response before deciding whether to run tools). MaxTokens bounds a single completion.
type ChatRequest struct {
	Model     string        `json:"model"`
	Messages  []ChatMessage `json:"messages"`
	Tools     []Tool        `json:"tools,omitempty"`
	Stream    bool          `json:"stream"`
	MaxTokens int           `json:"max_tokens,omitempty"`
}

// ChatResponse is the subset of the OpenAI-compatible response the loop consumes.
type ChatResponse struct {
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

// ChatChoice wraps one assistant message (the loop only ever reads choices[0]).
type ChatChoice struct {
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// Usage carries the token counts the ledger records (spec §5). PromptTokens is ALL the input the model
// read, cache reads and cache writes included; CacheReadTokens and CacheWriteTokens say how much of it
// was read from and written to the provider's prompt cache. Decoding normalizes every shape the gateway
// can send (UnmarshalJSON); encoding writes this one back, which decodes to the same values.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// UnmarshalJSON reads the usage shapes a gateway can send and counts every input token once:
//
//   - OpenAI: prompt_tokens INCLUDES prompt_tokens_details.cached_tokens (cache reads). A router that
//     reports cache writes there too (prompt_tokens_details.cache_creation_tokens) includes them as well.
//   - Anthropic: input_tokens EXCLUDES cache_read_input_tokens and cache_creation_input_tokens, so the
//     input is their sum, and output_tokens is the completion.
//   - Both at once (an OpenAI-compatible proxy in front of Anthropic): prompt_tokens is taken to include
//     the cache fields, as OpenAI's does, unless it is smaller than them and so cannot.
//
// A field that is absent counts 0. A gateway that sends no cache figures leaves both at 0 and its
// prompt_tokens a floor: 9router's non-streaming answer for a Claude model (the dev gateway) drops
// them. ponytail: DeepSeek's prompt_cache_hit_tokens and other vendor
// spellings are not read; add one here when a gateway sends it.
func (u *Usage) UnmarshalJSON(b []byte) error {
	var w struct {
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens int  `json:"completion_tokens"`
		InputTokens      int  `json:"input_tokens"`
		OutputTokens     int  `json:"output_tokens"`
		CacheRead        int  `json:"cache_read_input_tokens"`
		CacheWrite       int  `json:"cache_creation_input_tokens"`
		Details          struct {
			Cached        int `json:"cached_tokens"`
			CacheCreation int `json:"cache_creation_tokens"`
		} `json:"prompt_tokens_details"`
	}
	// Lenient: accounting never fails a model answer. encoding/json skips a field of the wrong type (1.5, a
	// string, [] for an object), reports it as *UnmarshalTypeError and still fills every other field, so the
	// skipped one reads 0, not reported. b is a value the outer decoder already parsed, so a type error is
	// the only error expected here; any other is still returned.
	if err := json.Unmarshal(b, &w); err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return fmt.Errorf("ai.Usage.UnmarshalJSON: %w", err)
		}
	}
	read, write := w.CacheRead, w.CacheWrite
	if read == 0 {
		read = w.Details.Cached
	}
	if write == 0 {
		write = w.Details.CacheCreation
	}
	prompt := w.InputTokens + read + write // Anthropic: input_tokens is the uncached remainder
	if w.PromptTokens != nil {
		prompt = *w.PromptTokens
		if prompt < read+write {
			prompt += read + write // a prompt_tokens smaller than its cached part reports the remainder only
		}
	}
	completion := w.CompletionTokens
	if completion == 0 {
		completion = w.OutputTokens
	}
	*u = Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion,
		CacheReadTokens: read, CacheWriteTokens: write}
	return nil
}

// add sums one call's usage into a turn's.
func (u *Usage) add(v Usage) {
	u.PromptTokens += v.PromptTokens
	u.CompletionTokens += v.CompletionTokens
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	u.CacheReadTokens += v.CacheReadTokens
	u.CacheWriteTokens += v.CacheWriteTokens
}

// Gateway is the HTTP client for the OpenAI-compatible LLM gateway. baseURL is the localhost-pinned
// value from config (never a request), so there is no SSRF surface. timeout bounds a single
// completion call end-to-end.
type Gateway struct {
	baseURL string
	apiKey  string // optional bearer token; a gateway in API-key mode rejects keyless calls with 401
	client  *http.Client
	timeout time.Duration
}

// NewGateway builds a Gateway over the config-pinned base URL and per-call timeout. The underlying
// http.Client carries the same timeout as a backstop; Complete additionally derives a per-call
// context deadline so a slow gateway fails fast with ErrTimeout rather than hanging the request.
func NewGateway(baseURL string, timeout time.Duration) *Gateway {
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &Gateway{
		baseURL: baseURL,
		client:  &http.Client{Timeout: timeout},
		timeout: timeout,
	}
}

// WithAPIKey sets the bearer token sent on every call. An empty key leaves requests unauthenticated.
func (g *Gateway) WithAPIKey(key string) *Gateway {
	g.apiKey = key
	return g
}

// Complete performs one non-streaming POST {baseURL}/chat/completions and returns the parsed
// response. It maps failure modes to typed sentinels:
//   - deadline/timeout (context or transport)      → ErrTimeout (504)
//   - non-2xx status, transport error, bad JSON body → ErrGateway (502)
//
// The request always carries stream:false. The caller's ctx is honored; Complete additionally
// bounds the call by g.timeout so a hung gateway cannot outlive the deadline even if the caller's
// ctx has none.
func (g *Gateway) Complete(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	req.Stream = false

	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ai.Gateway.Complete: marshal request: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	url := g.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ai.Gateway.Complete: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
	}

	resp, err := g.client.Do(httpReq)
	if err != nil {
		if isTimeout(callCtx, err) {
			return ChatResponse{}, fmt.Errorf("ai.Gateway.Complete: %w: %v", ErrTimeout, err)
		}
		return ChatResponse{}, fmt.Errorf("ai.Gateway.Complete: %w: %v", ErrGateway, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		if isTimeout(callCtx, err) {
			return ChatResponse{}, fmt.Errorf("ai.Gateway.Complete: read body: %w: %v", ErrTimeout, err)
		}
		return ChatResponse{}, fmt.Errorf("ai.Gateway.Complete: read body: %w: %v", ErrGateway, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Truncate the upstream body so a large error page never bloats the wrapped error/log.
		snippet := string(respBody)
		// A gateway may echo the rejected credential; scrub it BEFORE truncating (a cut could
		// otherwise leave a key prefix behind) so the error/log never carries the key.
		if g.apiKey != "" {
			snippet = strings.ReplaceAll(snippet, g.apiKey, "[redacted]")
		}
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return ChatResponse{}, fmt.Errorf("ai.Gateway.Complete: %w: status %d: %s", ErrGateway, resp.StatusCode, snippet)
	}

	var out ChatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return ChatResponse{}, fmt.Errorf("ai.Gateway.Complete: %w: decode response: %v", ErrGateway, err)
	}
	return out, nil
}

// isTimeout reports whether err represents a deadline/timeout rather than a plain transport error.
// It checks the call context's deadline first (the most reliable signal once WithTimeout fires),
// then falls back to the net.Error Timeout() probe for driver-level i/o timeouts.
func isTimeout(ctx context.Context, err error) bool {
	if ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}
