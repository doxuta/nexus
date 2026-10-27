package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/config"
)

// Issue #30 item 8: the dev gateway (9router) reports a call's whole input only on its streaming path, so the
// gateway asks for a stream with usage and folds it back into the one ChatResponse the agents read. These tests
// pin that fold: text, tool calls, usage, every refusal shape, an error, a cut and a timeout in mid-stream.

// sse is a streamed answer as a gateway sends it: one "data:" event per chunk, then the end marker.
func sse(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return b.String() + "data: [DONE]\n\n"
}

// streamingGateway is a Gateway whose server streams body as an event stream.
func streamingGateway(t *testing.T, timeout time.Duration, body string) *Gateway {
	t.Helper()
	g, _ := newMockGateway(t, timeout, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	})
	return g
}

// The request asks for a stream that ends with the usage: stream true, stream_options.include_usage true.
func TestGateway_Complete_RequestsAStreamWithUsage(t *testing.T) {
	var got map[string]json.RawMessage
	g, _ := newMockGateway(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)))
	})
	if _, err := g.Complete(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if string(got["stream"]) != "true" || strings.ReplaceAll(string(got["stream_options"]), " ", "") != `{"include_usage":true}` {
		t.Fatalf("stream = %s, stream_options = %s; want true and {\"include_usage\":true}", got["stream"], got["stream_options"])
	}
}

func TestGateway_Complete_FoldsTheStream(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       ChatResponse
	}{
		{"text in pieces, then a usage-only chunk (OpenAI)",
			sse(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`,
				`{"choices":[{"index":0,"delta":{"content":"Tôi đề xuất "},"finish_reason":null}],"usage":null}`,
				`{"choices":[{"index":0,"delta":{"content":"entity patients."},"finish_reason":null}],"usage":null}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`,
				`{"choices":[],"usage":{"prompt_tokens":20000,"completion_tokens":300,"total_tokens":20300,"prompt_tokens_details":{"cached_tokens":18000,"cache_creation_tokens":1500}}}`),
			ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: "Tôi đề xuất entity patients."}, FinishReason: "stop"}},
				Usage: Usage{PromptTokens: 20000, CompletionTokens: 300, TotalTokens: 20300, CacheReadTokens: 18000, CacheWriteTokens: 1500}}},
		{"two tool calls, arguments in pieces, the id repeated on every piece (9router)",
			sse(`{"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"propose_entity","arguments":""}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_1","function":{"arguments":"{\"name\":"}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_1","function":{"arguments":"\"tickets\"}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"toolu_2","type":"function","function":{"name":"list_entities","arguments":"{}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":36836,"completion_tokens":120,"total_tokens":36956}}`),
			ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "toolu_1", Type: "function", Function: ToolCallFunc{Name: "propose_entity", Arguments: `{"name":"tickets"}`}},
				{ID: "toolu_2", Type: "function", Function: ToolCallFunc{Name: "list_entities", Arguments: "{}"}},
			}}, FinishReason: "tool_calls"}},
				Usage: Usage{PromptTokens: 36836, CompletionTokens: 120, TotalTokens: 36956}}},
		{"a tool call without arguments streams no piece of them: read as {}, as a whole answer sends it",
			// 9router, 2026-10-27: list_packs came back with arguments "" and the next call, which sent it back,
			// failed with 400 "tool_use.input: Input should be an object".
			sse(`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"list_roles","arguments":""}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_1","function":{"arguments":"{\"grants\": true}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"toolu_2","type":"function","function":{"name":"list_packs","arguments":""}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
			ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "toolu_1", Type: "function", Function: ToolCallFunc{Name: "list_roles", Arguments: `{"grants": true}`}},
				{ID: "toolu_2", Type: "function", Function: ToolCallFunc{Name: "list_packs", Arguments: "{}"}},
			}}, FinishReason: "tool_calls"}}}},
		{"no [DONE], usage on the finish chunk: 9router's stream as recorded on 2026-10-27",
			"data: " + `{"id":"chatcmpl-msg_011","object":"chat.completion.chunk","created":1793086291,"model":"claude-opus-5-5","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
				"data: " + `{"id":"chatcmpl-msg_011","object":"chat.completion.chunk","created":1793086291,"model":"claude-opus-5-5","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n" +
				"data: " + `{"id":"chatcmpl-msg_011","object":"chat.completion.chunk","created":1793086291,"model":"claude-opus-5-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":18836,"completion_tokens":4,"total_tokens":18840}}` + "\n\n",
			ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
				Usage: Usage{PromptTokens: 18836, CompletionTokens: 4, TotalTokens: 18840}}},
		{"parallel tool calls streamed without an index: a new id opens a new call",
			// Some OpenAI-compatible proxies leave the index out; every piece then reads index 0.
			sse(`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{\"x\":"}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"1}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c2","type":"function","function":{"name":"b","arguments":"{\"y\":2}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
			ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "a", Arguments: `{"x":1}`}},
				{ID: "c2", Type: "function", Function: ToolCallFunc{Name: "b", Arguments: `{"y":2}`}},
			}}, FinishReason: "tool_calls"}}}},
		{"comments, event names and ids between the chunks are not data",
			": keep-alive\n\nevent: message\nid: 1\ndata: " + `{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}` + "\n\n: bye\n\ndata: [DONE]\n\n",
			ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}}}},
		{"data: without a space, CRLF line ends",
			"data:" + `{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":30000}}` + "\r\n\r\ndata:[DONE]\r\n\r\n",
			ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
				Usage: Usage{PromptTokens: 30002, CompletionTokens: 1, TotalTokens: 30003, CacheReadTokens: 30000}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := streamingGateway(t, 5*time.Second, tc.body).Complete(context.Background(), ChatRequest{Model: "m"})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("response =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// A streamed answer reads exactly as the same answer sent whole: the agents see one ChatResponse, so a
// refusal, a finish reason or a tool call means the same thing on both paths (#69's Refused() mappings).
func TestGateway_Complete_StreamReadsAsTheWholeAnswer(t *testing.T) {
	const usage = `"usage":{"prompt_tokens":2046,"completion_tokens":0}`
	for _, tc := range []struct {
		name, whole string
		stream      []string
		refused     bool
	}{
		{"finish_reason refusal, no text",
			`{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"refusal"}],` + usage + `}`,
			[]string{`{"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"refusal"}],` + usage + `}`}, true},
		{"stop_reason refusal on the choice",
			`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop","stop_reason":"refusal"}],` + usage + `}`,
			[]string{`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop","stop_reason":"refusal"}],` + usage + `}`}, true},
		{"finish_reason content_filter",
			`{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"content_filter"}],` + usage + `}`,
			[]string{`{"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"content_filter"}],` + usage + `}`}, true},
		{"OpenAI refusal text, streamed in pieces",
			`{"choices":[{"message":{"role":"assistant","content":null,"refusal":"I can't help with that."},"finish_reason":"stop"}],` + usage + `}`,
			[]string{`{"choices":[{"index":0,"delta":{"role":"assistant","refusal":"I can't "},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"refusal":"help with that."},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, `{"choices":[],` + usage + `}`}, true},
		{"top-level stop_reason refusal",
			`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"stop_reason":"refusal",` + usage + `}`,
			[]string{`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"stop"}],"stop_reason":"refusal",` + usage + `}`}, true},
		{"top-level stop_reason refusal, no choices",
			`{"choices":[],"stop_reason":"refusal",` + usage + `}`,
			[]string{`{"choices":[],"stop_reason":"refusal",` + usage + `}`}, true},
		{"stop_reason is a token id (vLLM)",
			`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","stop_reason":128001}],` + usage + `}`,
			[]string{`{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop","stop_reason":128001}],` + usage + `}`}, false},
		{"a tool call",
			`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_entities","arguments":"{}"}}]},"finish_reason":"tool_calls"}],` + usage + `}`,
			[]string{`{"choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"list_entities","arguments":"{"}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}],` + usage + `}`}, false},
		{"max_tokens cut", `{"choices":[{"message":{"role":"assistant","content":"Bảng"},"finish_reason":"length"}],` + usage + `}`,
			[]string{`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Bảng"},"finish_reason":"length"}],` + usage + `}`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whole, err := streamingGatewayJSON(t, tc.whole).Complete(context.Background(), ChatRequest{Model: "m"})
			if err != nil {
				t.Fatalf("whole: %v", err)
			}
			streamed, err := streamingGateway(t, 5*time.Second, sse(tc.stream...)).Complete(context.Background(), ChatRequest{Model: "m"})
			if err != nil {
				t.Fatalf("streamed: %v", err)
			}
			if len(whole.Choices) == 0 {
				whole.Choices = nil // "choices":[] and a stream with no choice say the same
			}
			if !reflect.DeepEqual(streamed, whole) {
				t.Fatalf("streamed =\n%+v\nwhole =\n%+v", streamed, whole)
			}
			if streamed.Refused() != tc.refused {
				t.Fatalf("Refused() = %v; want %v", streamed.Refused(), tc.refused)
			}
		})
	}
}

// streamingGatewayJSON is a Gateway whose server ignores stream and answers one JSON body, as a gateway
// without streaming does.
func streamingGatewayJSON(t *testing.T, body string) *Gateway {
	t.Helper()
	g, _ := newMockGateway(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	return g
}

// A stream that fails part-way is a failed call, never a half answer: an error event, a stream cut before
// the answer finished, a chunk that is not JSON and a cut connection are gateway errors (502); a gateway that
// stops sending is a timeout (504). The key never appears in the error.
func TestGateway_Complete_StreamFailures(t *testing.T) {
	const key = "sk-stream-secret-77"
	partial := `{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"propose_entity","arguments":"{\"na"}}]},"finish_reason":null}]}`
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    error
		says    string
	}{
		{"an error event mid-stream", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + partial + "\n\ndata: {\"error\":{\"message\":\"upstream overloaded for " + key + "\",\"type\":\"overloaded_error\"}}\n\n"))
		}, ErrGateway, "upstream overloaded"},
		{"the stream ends before the answer finished", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + partial + "\n\n"))
		}, ErrGateway, "ended before"},
		{"[DONE] before the answer finished", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + partial + "\n\ndata: [DONE]\n\n"))
		}, ErrGateway, "ended before"},
		{"the stream closes after the finish, before the usage and [DONE]", func(w http.ResponseWriter, r *http.Request) {
			// OpenAI sends the usage after the finish, then [DONE]; a stream that stops in between lost the usage.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + `{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
		}, ErrGateway, "before its usage"},
		{"a chunk that is not JSON", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + partial + "\n\ndata: {not json\n\n"))
		}, ErrGateway, "decode"},
		{"the connection is cut mid-stream", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Content-Length", "100000") // promise more than is sent: the client reads an unexpected EOF
			_, _ = w.Write([]byte("data: " + partial + "\n\n"))
		}, ErrGateway, "read"},
		{"the gateway stops sending mid-stream", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + partial + "\n\n"))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}, ErrTimeout, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newMockGateway(t, 200*time.Millisecond, tc.handler)
			start := time.Now()
			_, err := g.WithAPIKey(key).Complete(context.Background(), ChatRequest{Model: "m"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Complete() error = %v; want %v", err, tc.want)
			}
			if tc.want == ErrGateway && errors.Is(err, ErrTimeout) {
				t.Fatalf("error = %v; a failed stream is not a timeout", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("error = %q; want it to say %q", err, tc.says)
			}
			if strings.Contains(err.Error(), key) {
				t.Fatalf("error leaks the API key: %q", err)
			}
			if time.Since(start) > time.Second {
				t.Fatalf("Complete() took %v; want it to fail at the 200ms deadline at the latest", time.Since(start))
			}
		})
	}
}

// Recording a streamed call writes the folded answer, the shape every cassette has always had, so replay
// reads it like any older line: the same choices back, usage zeroed.
func TestRecord_StreamedCallReplays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(
			`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"list_entities","arguments":""}}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_1","function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":36836,"completion_tokens":120,"prompt_tokens_details":{"cached_tokens":34960,"cache_creation_tokens":472}}}`)))
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "streamed.jsonl")
	rec, err := NewCompleter(config.AIConfig{GatewayMode: config.AIGatewayRecord, Cassette: path, BaseURL: srv.URL, TimeoutSec: 5})
	if err != nil {
		t.Fatalf("NewCompleter(record): %v", err)
	}
	live, err := rec.Complete(context.Background(), ask("q1"))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if want := (Usage{PromptTokens: 36836, CompletionTokens: 120, TotalTokens: 36956, CacheReadTokens: 34960, CacheWriteTokens: 472}); live.Usage != want {
		t.Fatalf("recorded usage = %+v; want %+v", live.Usage, want)
	}
	lines := readCassette(t, path)
	if len(lines) != 1 || !reflect.DeepEqual(lines[0].Response, live) || lines[0].Request.Stream {
		t.Fatalf("cassette = %+v; want one line holding the folded answer and the request as the agent sent it", lines)
	}

	rep, err := NewCompleter(config.AIConfig{GatewayMode: config.AIGatewayReplay, Cassette: path})
	if err != nil {
		t.Fatalf("NewCompleter(replay): %v", err)
	}
	got, err := rep.Complete(context.Background(), ask("q1"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	want := live
	want.Usage = Usage{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed = %+v; want %+v", got, want)
	}
}

// The cassettes recorded before the stream (non-streaming answers, kept in git and replayed by CI's demo-loop
// jobs) load and answer every recorded call with its recorded choices, unchanged.
func TestReplay_TheNonStreamingCassettesStillAnswer(t *testing.T) {
	for _, name := range []string{"demo-loop.jsonl", "demo-loop-en.jsonl"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "..", "cassettes", name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var entries []cassetteEntry
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				var e cassetteEntry
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					t.Fatalf("line: %v", err)
				}
				entries = append(entries, e)
			}
			rep, err := NewCompleter(config.AIConfig{GatewayMode: config.AIGatewayReplay, Cassette: path})
			if err != nil {
				t.Fatalf("NewCompleter(replay): %v", err)
			}
			for i, e := range entries {
				got, err := rep.Complete(context.Background(), e.Request)
				if err != nil {
					t.Fatalf("call %d: %v", i+1, err)
				}
				want := e.Response
				want.Usage = Usage{}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("call %d = %+v; want the recorded answer %+v", i+1, got, want)
				}
			}
		})
	}
}
