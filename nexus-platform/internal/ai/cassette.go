// cassette.go — record/replay for the LLM gateway (AI_GATEWAY_MODE), so demos, CI and evals can run
// the builder and the assistant without a live model or key. A cassette is a JSON-lines file with
// one exchange per line: the request's fingerprint, the request as the agent sent it and the
// gateway's response.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"nexus/platform/internal/config"
)

// cassetteEntry is one line of a cassette: one exchange with the gateway.
type cassetteEntry struct {
	Fingerprint string       `json:"fingerprint"`
	Request     ChatRequest  `json:"request"`
	Response    ChatResponse `json:"response"`
}

// NewCompleter returns the Completer the agents talk to for cfg.GatewayMode: the live gateway (live,
// the default), the live gateway with every exchange appended to cfg.Cassette (record), or the
// cassette alone (replay — no gateway is built, so there is no network, base URL or key). A cassette
// that cannot be opened is an error here, at startup, rather than on the first chat turn.
func NewCompleter(cfg config.AIConfig) (Completer, error) {
	if cfg.GatewayMode == config.AIGatewayReplay {
		return newReplayer(cfg.Cassette)
	}
	live := NewGateway(cfg.BaseURL, time.Duration(cfg.TimeoutSec)*time.Second).WithAPIKey(cfg.APIKey)
	switch cfg.GatewayMode {
	case "", config.AIGatewayLive:
		return live, nil
	case config.AIGatewayRecord:
		return newRecorder(live, cfg.Cassette)
	}
	return nil, fmt.Errorf("ai.NewCompleter: AI_GATEWAY_MODE=%q: want live, record or replay", cfg.GatewayMode)
}

// recorder is record mode: it calls the live gateway and appends each exchange to the cassette as one
// JSON line. The API key exists only in the gateway's Authorization header, never in a ChatRequest or
// ChatResponse, so a line cannot carry it and nothing needs redacting. A failed call is returned
// and not written: it has no response to replay. Lines land in the order calls finish, which is call
// order for one conversation at a time (a demo, CI, an eval).
//
// ponytail: a line keeps the whole request — persona and tool schemas too, ~70 KB for a builder
// call — so a stale cassette can be diagnosed from the file; drop Tools from the line if cassettes
// kept in git grow too big.
type recorder struct {
	live Completer
	path string
	mu   sync.Mutex // the builder and the assistant can call at once; one Write per line, never interleaved
	file *os.File
}

// newRecorder starts the cassette afresh, so a re-record replaces the stale one. The file stays open
// for the life of the process; each line is a single write, so a crash keeps every finished exchange.
func newRecorder(live Completer, path string) (Completer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("ai.newRecorder: open cassette: %w", err)
	}
	return &recorder{live: live, path: path, file: f}, nil
}

func (r *recorder) Complete(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	resp, err := r.live.Complete(ctx, req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ai.recorder.Complete: %w", err)
	}
	line, err := json.Marshal(cassetteEntry{Fingerprint: fingerprint(req), Request: req, Response: resp})
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ai.recorder.Complete: encode exchange: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.file.Write(append(line, '\n')); err != nil {
		return ChatResponse{}, fmt.Errorf("ai.recorder.Complete: write cassette %s: %w", r.path, err)
	}
	return resp, nil
}

// replayer is replay mode: it answers each call with the next exchange of the cassette, strictly in
// order, without network. A call whose fingerprint differs from the recorded one, or one past the
// end, means the code no longer makes the calls the cassette holds: it fails as ErrGateway (502) with
// the way to re-record, instead of answering with the wrong exchange.
type replayer struct {
	path    string
	mu      sync.Mutex
	entries []cassetteEntry
	next    int
}

func newReplayer(path string) (Completer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ai.newReplayer: read cassette: %w", err)
	}
	r := &replayer{path: path}
	for i, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e cassetteEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("ai.newReplayer: cassette %s line %d: %w", path, i+1, err)
		}
		r.entries = append(r.entries, e)
	}
	return r, nil
}

func (r *replayer) Complete(_ context.Context, req ChatRequest) (ChatResponse, error) {
	got := fingerprint(req)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next >= len(r.entries) {
		return ChatResponse{}, r.stale("it holds %d exchanges and call %d asks for another [%s]", len(r.entries), r.next+1, got)
	}
	e := r.entries[r.next]
	if e.Fingerprint != got {
		return ChatResponse{}, r.stale("call %d sends [%s] where it recorded [%s]", r.next+1, got, e.Fingerprint)
	}
	r.next++
	return e.Response, nil
}

// stale is the error for a cassette that no longer matches the calls, with the way to re-record it.
func (r *replayer) stale(format string, args ...any) error {
	return fmt.Errorf("ai.replayer.Complete: %w: cassette %s is stale: %s — re-record it: run the same steps against a live gateway with AI_GATEWAY_MODE=record AI_GATEWAY_CASSETTE=%s",
		ErrGateway, r.path, fmt.Sprintf(format, args...), r.path)
}

// fingerprint is a request's shape: its message roles in order, each assistant turn followed by the
// names of the tools it called. Contents stay out — they carry fresh UUIDs, so they differ every run.
func fingerprint(req ChatRequest) string {
	parts := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		parts[i] = m.Role
		if len(m.ToolCalls) > 0 {
			names := make([]string, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				names[j] = tc.Function.Name
			}
			parts[i] += "(" + strings.Join(names, ",") + ")"
		}
	}
	return strings.Join(parts, " ")
}
