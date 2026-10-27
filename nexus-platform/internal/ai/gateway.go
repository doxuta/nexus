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
// message). Refusal is OpenAI's reason text on an answer the model declined (read only, never sent: a
// declined message never enters the conversation). Empty fields are omitted so the wire shape matches
// what the gateway expects.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	Refusal    string     `json:"refusal,omitempty"`
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

// ChatRequest is the POST /chat/completions body. MaxTokens bounds a single completion. Callers leave Stream
// and StreamOptions unset: Gateway.Complete sets them on the wire (it streams to get the whole usage, then
// folds the stream back into one ChatResponse), so a cassette records the request as the agent built it.
type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []ChatMessage  `json:"messages"`
	Tools         []Tool         `json:"tools,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
}

// StreamOptions asks a streamed completion to end with the call's usage (OpenAI's include_usage).
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatResponse is the subset of the OpenAI-compatible response the loop consumes. StopReason is Claude's
// top-level field, which a proxy may pass through as it is (decoded as any, like ChatChoice.StopReason).
type ChatResponse struct {
	Choices    []ChatChoice `json:"choices"`
	Usage      Usage        `json:"usage"`
	StopReason any          `json:"stop_reason,omitempty"`
}

// Refused reports a response whose model declined the request, wherever the gateway put the signal: Claude's
// top-level stop_reason "refusal" (a response that may carry no choice at all) or the first choice's.
func (r ChatResponse) Refused() bool {
	return r.StopReason == "refusal" || (len(r.Choices) > 0 && r.Choices[0].Refused())
}

// ChatChoice wraps one assistant message (the loop only ever reads choices[0]).
type ChatChoice struct {
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
	// StopReason is Claude's name for the finish reason, which a proxy may copy onto the choice as it is. It is
	// decoded as any because vLLM sends a token id (a number) under the same name.
	StopReason any `json:"stop_reason,omitempty"`
}

// Refused reports a model that declined the request (HTTP 200): Claude's stop_reason "refusal", relayed as
// finish_reason "refusal" or copied onto the choice as it is, and OpenAI's finish_reason "content_filter" or
// message.refusal text. Its text and tool calls may be cut short and are not an answer.
func (c ChatChoice) Refused() bool {
	return c.FinishReason == "refusal" || c.FinishReason == "content_filter" || c.StopReason == "refusal" || c.Message.Refusal != ""
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
// A field that is absent counts 0, so a gateway that sends no cache figures leaves both at 0. The dev
// gateway (9router 0.5.99, a Claude model) is one: its OpenAI-shape usage keeps only prompt_tokens,
// completion_tokens and total_tokens, and it adds 2,000 to prompt_tokens. Streamed (what Gateway.Complete
// asks for), prompt_tokens is the whole input plus that 2,000; not streamed, it was the uncached
// remainder plus 2,000, a floor. ponytail: DeepSeek's prompt_cache_hit_tokens and other vendor
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

// Complete performs one POST {baseURL}/chat/completions and returns the whole answer as one ChatResponse.
// The request asks for a stream that ends with the usage (stream, stream_options.include_usage): the dev
// gateway reports a call's whole input only on that path (issue #30 item 8). The stream is read to its end
// and folded back (foldStream), so the agents, the recorder and replay see what a non-streamed answer
// gives; a gateway that ignores stream and answers one JSON body is read as before. It maps failure modes
// to typed sentinels:
//   - deadline/timeout (context or transport), mid-stream too → ErrTimeout (504)
//   - non-2xx status, transport error, a body that is neither an answer nor a finished stream,
//     an error event mid-stream                                → ErrGateway (502)
//
// The caller's ctx is honored; Complete additionally bounds the call by g.timeout so a hung gateway
// cannot outlive the deadline even if the caller's ctx has none.
func (g *Gateway) Complete(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	req.Stream = true
	req.StreamOptions = &StreamOptions{IncludeUsage: true}

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
		return ChatResponse{}, fmt.Errorf("ai.Gateway.Complete: %w: status %d: %s", ErrGateway, resp.StatusCode, g.snippet(string(respBody)))
	}

	// A gateway that ignores stream answers one JSON object; an event stream never starts with "{".
	var out ChatResponse
	if b := bytes.TrimSpace(respBody); len(b) > 0 && b[0] == '{' {
		err = json.Unmarshal(b, &out)
	} else {
		out, err = foldStream(respBody)
	}
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ai.Gateway.Complete: %w: %s", ErrGateway, g.snippet(err.Error()))
	}
	return out, nil
}

// snippet is upstream text fit for an error and a log line. A gateway may echo the rejected credential;
// it is scrubbed BEFORE truncating (a cut could otherwise leave a key prefix behind), then cut to 512 bytes
// so a large error page never bloats the wrapped error.
func (g *Gateway) snippet(s string) string {
	if g.apiKey != "" {
		s = strings.ReplaceAll(s, g.apiKey, "[redacted]")
	}
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

// streamChunk is one data event of a streamed completion (OpenAI's chat.completion.chunk). Each choice
// carries a delta: the next piece of the message, and of each tool call by its index.
type streamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			Refusal   string `json:"refusal"`
			ToolCalls []struct {
				Index    int          `json:"index"`
				ID       string       `json:"id"`
				Type     string       `json:"type"`
				Function ToolCallFunc `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
		StopReason   any    `json:"stop_reason"`
	} `json:"choices"`
	Usage      *Usage          `json:"usage"`
	StopReason any             `json:"stop_reason"`
	Error      json.RawMessage `json:"error"`
}

// foldStream folds a streamed completion into the ChatResponse a non-streamed one gives: the text and the
// refusal text joined; each tool call's arguments joined by its index, "{}" when none came, its id, type and
// name taken as sent (9router repeats the id on every piece; a proxy that leaves the index out gets a new
// call for each new id); the last finish reason and stop reasons; and the last usage the stream sent (OpenAI
// sends it once, after the finish). A finish reason or a top-level stop reason finishes the answer, and
// [DONE] or the usage closes the stream (9router sends no [DONE], its usage rides on the finish chunk). A
// stream that stops before the finish, or after it with neither (the usage lost), or that sends an error
// event, is an error: a partial tool call is not an answer, and an answer without its usage escapes the
// budget.
//
// ponytail: one "data:" line is one event; a gateway that splits an event over several data lines (the
// SSE spec allows it, no OpenAI-compatible gateway does) is read as broken JSON, a 502.
func foldStream(body []byte) (ChatResponse, error) {
	var (
		out    ChatResponse
		choice *ChatChoice
		slot   = map[int]int{} // a tool call's stream index → its place in the message
		done   bool            // [DONE] came
		usage  bool            // a usage came
	)
	for _, line := range strings.Split(string(body), "\n") {
		data, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data:")
		if !ok {
			continue // a blank line, a comment, an event name or id
		}
		if data = strings.TrimSpace(data); data == "[DONE]" {
			done = true
			break
		}
		var c streamChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return ChatResponse{}, fmt.Errorf("ai.foldStream: decode stream chunk: %w", err)
		}
		if len(c.Error) > 0 && string(c.Error) != "null" {
			return ChatResponse{}, fmt.Errorf("ai.foldStream: error event mid-stream: %s", c.Error)
		}
		if c.Usage != nil {
			out.Usage, usage = *c.Usage, true
		}
		if c.StopReason != nil {
			out.StopReason = c.StopReason
		}
		for _, ch := range c.Choices {
			if ch.Index != 0 {
				continue // the loop only ever reads choices[0]
			}
			if choice == nil {
				choice = &ChatChoice{Message: ChatMessage{Role: "assistant"}}
			}
			if ch.Delta.Role != "" {
				choice.Message.Role = ch.Delta.Role
			}
			choice.Message.Content += ch.Delta.Content
			choice.Message.Refusal += ch.Delta.Refusal
			for _, tc := range ch.Delta.ToolCalls {
				i, seen := slot[tc.Index]
				if !seen || tc.ID != "" && choice.Message.ToolCalls[i].ID != "" && tc.ID != choice.Message.ToolCalls[i].ID {
					i = len(choice.Message.ToolCalls)
					slot[tc.Index] = i
					choice.Message.ToolCalls = append(choice.Message.ToolCalls, ToolCall{Type: "function"})
				}
				call := &choice.Message.ToolCalls[i]
				if tc.ID != "" {
					call.ID = tc.ID
				}
				if tc.Type != "" {
					call.Type = tc.Type
				}
				if tc.Function.Name != "" {
					call.Function.Name = tc.Function.Name
				}
				call.Function.Arguments += tc.Function.Arguments
			}
			if ch.FinishReason != "" {
				choice.FinishReason = ch.FinishReason
			}
			if ch.StopReason != nil {
				choice.StopReason = ch.StopReason
			}
		}
	}
	if out.StopReason == nil && (choice == nil || choice.FinishReason == "") {
		return ChatResponse{}, errors.New("ai.foldStream: the stream ended before the answer finished")
	}
	if !done && !usage {
		return ChatResponse{}, errors.New("ai.foldStream: the stream ended after the answer, before its usage or [DONE]")
	}
	if choice != nil {
		// A call without arguments streams no piece of them; a whole answer sends "{}", and the gateway
		// refuses "" when the call is sent back (9router: "tool_use.input: Input should be an object").
		for i := range choice.Message.ToolCalls {
			if strings.TrimSpace(choice.Message.ToolCalls[i].Function.Arguments) == "" {
				choice.Message.ToolCalls[i].Function.Arguments = "{}"
			}
		}
		out.Choices = []ChatChoice{*choice}
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
