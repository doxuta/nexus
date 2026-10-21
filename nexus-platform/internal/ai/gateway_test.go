package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newMockGateway returns a Gateway pointed at a test server whose handler is h, with a short
// per-call timeout so the hanging-server case fails fast.
func newMockGateway(t *testing.T, timeout time.Duration, h http.HandlerFunc) (*Gateway, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewGateway(srv.URL+"/v1", timeout), srv
}

func TestGateway_Complete_ToolCallsResponse(t *testing.T) {
	g, _ := newMockGateway(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q; want /v1/chat/completions", r.URL.Path)
		}
		// Assert the request never streams.
		var req ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			t.Error("request Stream = true; want false")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_entities","arguments":"{}"}}]},
				"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`))
	})

	resp, err := g.Complete(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete() error = %v; want nil", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d; want 1", len(resp.Choices))
	}
	tc := resp.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].ID != "call_1" || tc[0].Function.Name != "list_entities" {
		t.Fatalf("tool_calls = %+v; want one list_entities call with id call_1", tc)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v; want prompt=11 completion=3", resp.Usage)
	}
}

func TestGateway_Complete_FinalTextResponse(t *testing.T) {
	g, _ := newMockGateway(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"Tôi đề xuất entity patients."},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}`))
	})

	resp, err := g.Complete(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete() error = %v; want nil", err)
	}
	if got := resp.Choices[0].Message.Content; got != "Tôi đề xuất entity patients." {
		t.Fatalf("content = %q; want the final text", got)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Fatal("final text response should carry no tool_calls")
	}
}

func TestGateway_Complete_500_MapsGatewayError(t *testing.T) {
	g, _ := newMockGateway(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream boom"}`))
	})

	_, err := g.Complete(context.Background(), ChatRequest{Model: "m"})
	if !errors.Is(err, ErrGateway) {
		t.Fatalf("Complete() error = %v; want ErrGateway", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Fatal("a 500 must not be classified as a timeout")
	}
}

func TestGateway_Complete_HangingServer_MapsTimeout(t *testing.T) {
	g, _ := newMockGateway(t, 100*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // outlive the 100ms per-call timeout
		w.WriteHeader(http.StatusOK)
	})

	start := time.Now()
	_, err := g.Complete(context.Background(), ChatRequest{Model: "m"})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Complete() error = %v; want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Complete() took %v; want it to fail fast (~100ms), not wait for the server", elapsed)
	}
}

func TestGateway_Complete_BadJSONBody_MapsGatewayError(t *testing.T) {
	g, _ := newMockGateway(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json at all`))
	})

	_, err := g.Complete(context.Background(), ChatRequest{Model: "m"})
	if !errors.Is(err, ErrGateway) {
		t.Fatalf("Complete() error = %v; want ErrGateway for an undecodable body", err)
	}
}

// A gateway may echo the rejected credential in its error body ("Invalid API key sk-..."). That
// body is wrapped into the error and logged by the handler, so the key must be scrubbed first.
func TestGateway_Complete_ErrorBodyNeverEchoesKey(t *testing.T) {
	const key = "sk-secret-123456"
	g, _ := newMockGateway(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Invalid API key ` + key + `"}`))
	})

	_, err := g.WithAPIKey(key).Complete(context.Background(), ChatRequest{Model: "m"})
	if !errors.Is(err, ErrGateway) {
		t.Fatalf("Complete() error = %v; want ErrGateway", err)
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error leaks the API key: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "Invalid API key") {
		t.Errorf("error lost the diagnostic text: %q", err.Error())
	}
}

func TestGateway_Complete_SendsBearerOnlyWhenKeySet(t *testing.T) {
	for _, tc := range []struct{ name, key, want string }{
		{"with key", "sk-test", "Bearer sk-test"},
		{"without key", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			g, _ := newMockGateway(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			})
			if _, err := g.WithAPIKey(tc.key).Complete(context.Background(), ChatRequest{Model: "m"}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got != tc.want {
				t.Errorf("Authorization = %q; want %q", got, tc.want)
			}
		})
	}
}

// The gateway can receive usage in more than one shape. Whatever the shape, Usage.PromptTokens is ALL
// the input the model read — cache reads and cache writes included, counted once — and the two cache
// fields say how much of it was read from and written to the provider's prompt cache (issue #30
// item 8: a ~70 KB call was recorded as ~2,000 prompt tokens). The figures are the dev gateway's own
// ledger for the three calls of cassettes/demo-loop.jsonl.
func TestUsage_ParsesCacheTokensFromEveryShape(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		want        Usage
	}{
		{"OpenAI: prompt_tokens already includes prompt_tokens_details.cached_tokens",
			`{"prompt_tokens":34915,"completion_tokens":1076,"total_tokens":35991,"prompt_tokens_details":{"cached_tokens":33906}}`,
			Usage{PromptTokens: 34915, CompletionTokens: 1076, TotalTokens: 35991, CacheReadTokens: 33906}},
		{"OpenAI-compatible router: cache writes in prompt_tokens_details.cache_creation_tokens, also included",
			`{"prompt_tokens":34915,"completion_tokens":1076,"total_tokens":35991,"prompt_tokens_details":{"cached_tokens":33906,"cache_creation_tokens":1007}}`,
			Usage{PromptTokens: 34915, CompletionTokens: 1076, TotalTokens: 35991, CacheReadTokens: 33906, CacheWriteTokens: 1007}},
		{"Anthropic: input_tokens EXCLUDES cache_read_input_tokens and cache_creation_input_tokens",
			`{"input_tokens":2,"output_tokens":1076,"cache_read_input_tokens":33906,"cache_creation_input_tokens":1007}`,
			Usage{PromptTokens: 34915, CompletionTokens: 1076, TotalTokens: 35991, CacheReadTokens: 33906, CacheWriteTokens: 1007}},
		{"Anthropic fields beside an OpenAI prompt_tokens that already includes them: no double count",
			`{"prompt_tokens":34915,"completion_tokens":1076,"total_tokens":35991,"cache_read_input_tokens":33906,"cache_creation_input_tokens":1007}`,
			Usage{PromptTokens: 34915, CompletionTokens: 1076, TotalTokens: 35991, CacheReadTokens: 33906, CacheWriteTokens: 1007}},
		{"prompt_tokens smaller than its cached part cannot include it: the cache is added",
			`{"prompt_tokens":2002,"completion_tokens":1064,"total_tokens":3066,"cache_read_input_tokens":32378,"cache_creation_input_tokens":1528}`,
			Usage{PromptTokens: 35908, CompletionTokens: 1064, TotalTokens: 36972, CacheReadTokens: 32378, CacheWriteTokens: 1528}},
		{"no cache fields (the dev gateway's non-streaming answer): cache 0, prompt as reported",
			`{"prompt_tokens":2002,"completion_tokens":1064,"total_tokens":3066}`,
			Usage{PromptTokens: 2002, CompletionTokens: 1064, TotalTokens: 3066}},
		{"empty prompt_tokens_details", `{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14,"prompt_tokens_details":{}}`,
			Usage{PromptTokens: 11, CompletionTokens: 3, TotalTokens: 14}},
		{"empty usage", `{}`, Usage{}},
		{"null usage", `null`, Usage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp ChatResponse
			if err := json.Unmarshal([]byte(`{"choices":[],"usage":`+tc.usage+`}`), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Usage != tc.want {
				t.Fatalf("usage = %+v; want %+v", resp.Usage, tc.want)
			}
		})
	}
}

// The recorder writes Usage back out into the cassette and the replayer reads it again: a usage that
// carries cache tokens must come back unchanged, not with its cache counted a second time.
func TestUsage_RoundTripsThroughACassetteLine(t *testing.T) {
	u := Usage{PromptTokens: 34915, CompletionTokens: 1076, TotalTokens: 35991, CacheReadTokens: 33906, CacheWriteTokens: 1007}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back Usage
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	if back != u {
		t.Fatalf("round trip %s = %+v; want %+v", b, back, u)
	}
}

// The live gateway hands the decoded cache figures to the caller.
func TestGateway_Complete_ReportsCacheTokens(t *testing.T) {
	g, _ := newMockGateway(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":20000,"completion_tokens":300,"total_tokens":20300,"prompt_tokens_details":{"cached_tokens":18000}}}`))
	})
	resp, err := g.Complete(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := (Usage{PromptTokens: 20000, CompletionTokens: 300, TotalTokens: 20300, CacheReadTokens: 18000}); resp.Usage != want {
		t.Fatalf("usage = %+v; want %+v", resp.Usage, want)
	}
}

// Accounting never fails a model answer: a usage field of the wrong type is read as not reported, and the
// answer and every well-typed figure still come through (a decode error here used to turn the whole call
// into a 502).
func TestGateway_Complete_BadUsageNeverFailsTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		want        Usage
	}{
		{"fractional cached_tokens", `{"prompt_tokens":20000,"completion_tokens":300,"prompt_tokens_details":{"cached_tokens":1.5}}`,
			Usage{PromptTokens: 20000, CompletionTokens: 300, TotalTokens: 20300}},
		{"prompt_tokens_details is an array", `{"prompt_tokens":20000,"completion_tokens":300,"prompt_tokens_details":[]}`,
			Usage{PromptTokens: 20000, CompletionTokens: 300, TotalTokens: 20300}},
		{"cache_read_input_tokens is a string", `{"input_tokens":2,"output_tokens":7,"cache_read_input_tokens":"lots","cache_creation_input_tokens":40}`,
			Usage{PromptTokens: 42, CompletionTokens: 7, TotalTokens: 49, CacheWriteTokens: 40}},
		{"usage is a string", `"n/a"`, Usage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newMockGateway(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":` + tc.usage + `}`))
			})
			resp, err := g.Complete(context.Background(), ChatRequest{Model: "m"})
			if err != nil {
				t.Fatalf("Complete: %v; want the answer despite the bad usage", err)
			}
			if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "ok" {
				t.Fatalf("choices = %+v; want the answer", resp.Choices)
			}
			if resp.Usage != tc.want {
				t.Fatalf("usage = %+v; want %+v", resp.Usage, tc.want)
			}
		})
	}
}
