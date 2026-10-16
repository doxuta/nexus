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
	"slices"
	"strings"
	"sync"
	"time"

	"nexus/platform/internal/config"
)

// cassetteEntry is one line of a cassette: one exchange with the gateway.
type cassetteEntry struct {
	Fingerprint fingerprint  `json:"fingerprint"`
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
	live    Completer
	path    string
	mu      sync.Mutex // the builder and the assistant can call at once; one Write per line, never interleaved
	file    *os.File
	started bool // the first exchange written empties the file first
}

// newRecorder opens the cassette WITHOUT truncating it: an unwritable path still stops the boot, but
// a boot that records nothing (AI off, the gateway down, a restart before the first turn) leaves the
// previous recording as it was. The first exchange that succeeds replaces it, so a re-record starts
// afresh. The file stays open for the life of the process; each line is a single write, so a crash
// keeps every finished exchange.
func newRecorder(live Completer, path string) (Completer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
	line, err := json.Marshal(cassetteEntry{Fingerprint: fingerprintOf(req), Request: req, Response: resp})
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ai.recorder.Complete: encode exchange: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		if err := r.file.Truncate(0); err != nil {
			return ChatResponse{}, fmt.Errorf("ai.recorder.Complete: empty cassette %s: %w", r.path, err)
		}
		r.started = true
	}
	if _, err := r.file.Write(append(line, '\n')); err != nil {
		return ChatResponse{}, fmt.Errorf("ai.recorder.Complete: write cassette %s: %w", r.path, err)
	}
	return resp, nil
}

// replayer is replay mode: it answers each call with the next exchange of the cassette, strictly in
// order, without network. A call whose fingerprint differs from the recorded one — another agent,
// another prompt, another number of steps — or one past the end fails as ErrGateway (502) with the
// way to re-record, instead of answering with another turn's exchange.
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
	got := fingerprintOf(req)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next >= len(r.entries) {
		return ChatResponse{}, r.stale("no exchange left for call %d (the cassette holds %d)", r.next+1, len(r.entries))
	}
	e := r.entries[r.next]
	if why := got.mismatch(e.Fingerprint); why != "" {
		return ChatResponse{}, r.stale("call %d %s", r.next+1, why)
	}
	r.next++
	// No model ran, so the call costs nothing: the recorded usage would go into sys_ai_runs and eat
	// the tenant's daily token budget on every rehearsal. The cassette keeps the real figures.
	resp := e.Response
	resp.Usage = Usage{}
	return resp, nil
}

// replays reports whether c answers from a cassette. Its zero usage is by design, not a gateway that
// stopped reporting usage, so the agents' zero-usage canary stays quiet for it.
func replays(c Completer) bool {
	_, ok := c.(*replayer)
	return ok
}

// stale is the error for a cassette that no longer matches the calls, with the way to re-record it.
func (r *replayer) stale(format string, args ...any) error {
	return fmt.Errorf("ai.replayer.Complete: %w: cassette %s is stale: %s — re-record it: run the same steps against a live gateway with AI_GATEWAY_MODE=record AI_GATEWAY_CASSETTE=%s",
		ErrGateway, r.path, fmt.Sprintf(format, args...), r.path)
}

// fingerprint is what replay checks each call against, so it never hands one turn another's answer:
// the names of the tools offered (they tell the builder from the assistant; schemas stay out, so a
// reworded description does not go stale), a short hash of the user messages (the scripted prompts)
// and the conversation's shape (roles in order, each assistant turn with the tools it called).
// System grounding and tool results stay out: they carry fresh ids and change with the database.
type fingerprint struct {
	Tools []string `json:"tools"`
	User  string   `json:"user"`
	Shape string   `json:"shape"`
}

func fingerprintOf(req ChatRequest) fingerprint {
	f := fingerprint{Tools: make([]string, len(req.Tools))}
	for i, t := range req.Tools {
		f.Tools[i] = t.Function.Name
	}
	var users []string
	shape := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		shape[i] = m.Role
		if m.Role == "user" {
			users = append(users, m.Content)
		}
		if len(m.ToolCalls) > 0 {
			names := make([]string, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				names[j] = tc.Function.Name
			}
			shape[i] += "(" + strings.Join(names, ",") + ")"
		}
	}
	f.User = sha256Hex(strings.Join(users, "\x00"))[:8]
	f.Shape = strings.Join(shape, " ")
	return f
}

// mismatch says how a call with fingerprint f differs from the exchange recorded as rec; "" = none.
func (f fingerprint) mismatch(rec fingerprint) string {
	switch {
	case !slices.Equal(f.Tools, rec.Tools):
		return fmt.Sprintf("is offered the tools [%s] where the cassette recorded [%s] (another agent, or tools added or renamed)",
			strings.Join(f.Tools, ","), strings.Join(rec.Tools, ","))
	case f.User != rec.User:
		return fmt.Sprintf("carries user messages hashed %s where the cassette recorded %s (another prompt)", f.User, rec.User)
	case f.Shape != rec.Shape:
		return fmt.Sprintf("sends [%s] where the cassette recorded [%s]", f.Shape, rec.Shape)
	}
	return ""
}
