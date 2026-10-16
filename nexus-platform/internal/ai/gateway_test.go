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
