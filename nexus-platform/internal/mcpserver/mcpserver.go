// Package mcpserver serves the builder agent's tools over MCP at /mcp (issue #3): agents propose,
// humans dispose. An outside agent (Claude Code, Claude Desktop) presents an nxm_ token a builder
// issued in the Studio and gets the same read tools and propose_* tools as the in-app builder agent,
// generated from ai.ToolDefinitions and run through the same validators, as that builder, in that
// builder's workspace. A proposal is appended to the token's assistant thread in the shape a chat
// turn writes, so the Studio shows it as a card a human applies to drafts and publishes. No tool
// applies, publishes, rolls back, writes access or records, or runs DDL.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
)

// Verifier resolves a presented nxm_ token (services.MCPTokenService).
type Verifier interface {
	Verify(ctx context.Context, raw string) (*services.MCPPrincipal, error)
}

// Runner runs one builder tool (ai.AIBuilderAgent.RunTool).
type Runner interface {
	RunTool(ctx context.Context, tenantID, tag string, prior []ai.DraftItem, name, argsJSON string) (string, []ai.DraftItem)
}

// Freezer is the per-tenant maintenance freeze (services.TenantFreeze): the same checker every
// /api/v1 write passes through (middleware.RequireTenantWritable).
type Freezer interface {
	FrozenUntil(ctx context.Context, tenantID string) (time.Time, bool, error)
}

// Threads reads and appends the token's assistant thread (ai.SessionStore).
type Threads interface {
	GetSession(ctx context.Context, tenantID, userID, id string) (ai.Session, []ai.Message, error)
	AppendTurn(ctx context.Context, tenantID, userID, sessionID, newTitle, userContent, assistantContent string, assistantDraft json.RawMessage) (string, error)
}

// instructions is what an MCP client hands its model about this server.
const instructions = `Nexus is a platform for business apps whose apps are metadata: tables (entities), fields, relations, layouts, automation rules, reports, apps and access. These tools read ONE workspace's model and PROPOSE changes to it. A proposal changes nothing: it is saved as a card in the builder's Studio thread, where a human reviews and applies it. No tool applies, publishes or writes data, so tell the user the change is waiting for review. Read before you propose (nexus_list_entities, nexus_list_fields). Names are snake_case; labels are what users see. A call refused by a validator answers {"ok":false,"error":"CODE: message"}: fix the arguments and call again. Tool descriptions are in Vietnamese.`

type server struct {
	tokens  Verifier
	runner  Runner
	threads Threads
	freeze  Freezer // nil = no freeze (test-only shape; production passes the API's own checker)
	logger  *zap.Logger
}

// New returns the /mcp handler: bearer auth for nxm_ tokens in front of the SDK's streamable HTTP
// handler in stateless mode. Every request verifies its token again and nothing is kept between
// requests, so a revoked or expired token stops at its next request.
func New(tokens Verifier, runner Runner, threads Threads, freeze Freezer, logger *zap.Logger) http.Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &server{tokens: tokens, runner: runner, threads: threads, freeze: freeze, logger: logger}
	srv := mcp.NewServer(&mcp.Implementation{Name: "nexus", Title: "Nexus", Version: "1"}, &mcp.ServerOptions{Instructions: instructions})
	for _, t := range tools(ai.ToolDefinitions()) {
		srv.AddTool(t, s.call)
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return s.authenticate(h)
}

type principalKey struct{}

// authenticate admits a live nxm_ token and nothing else: no credential, a JWT or an unknown,
// revoked or expired token is 401; an API key (nxk_), a valid credential of another kind, is 403
// and is never looked up. The verified principal rides the request context to the tool handlers.
func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r.Header.Get("Authorization"))
		switch {
		case strings.HasPrefix(tok, "nxk_"): // services.apiKeyPrefix
			writeErr(w, http.StatusForbidden, "FORBIDDEN", "Khoá API không dùng được ở /mcp — tạo token MCP trong Studio")
			return
		case !strings.HasPrefix(tok, services.MCPTokenPrefix):
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "Yêu cầu xác thực không hợp lệ")
			return
		}
		p, err := s.tokens.Verify(r.Context(), tok)
		if errors.Is(err, auth.ErrMCPTokenInvalid) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "Yêu cầu xác thực không hợp lệ")
			return
		}
		if err != nil {
			s.logger.Error("mcpserver: verify token failed (fail-closed)", zap.Error(err))
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "Lỗi hệ thống")
			return
		}
		// The tools then read what the in-app builder agent reads: DraftCaller sets the same Caller on a
		// builder's /api/v1/admin/* request, so on a delivered project the agent sees the builder's
		// drafts, not the published model. Verify has just checked that the user is a builder.
		ctx := metaversion.WithWarnings(metaversion.WithCaller(r.Context(), metaversion.Caller{
			UserID: p.UserID, TenantID: p.TenantID, HomeTenantID: p.TenantID, IsBuilder: true, ReadDraft: true,
		}))
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, principalKey{}, *p)))
	})
}

// bearerToken is the credential of an "Authorization: Bearer <token>" header (scheme in any case,
// RFC 7235), or "".
func bearerToken(h string) string {
	h = strings.TrimSpace(h)
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

// proposedNote follows the result of a proposal: where it went, that nothing changed yet, and what the
// human's apply does, by the change class of what it proposes (metaversion.ClassOf, issue #22): an edit
// of a draft type goes to drafts once the project is delivered; a NEW document of a NewHidden type (a
// table, a field, an N-N relation) is created at apply but the customer sees it only after a publish;
// anything else (access, approvals, work queues, records) takes effect when applied. The most immediate
// effect among the cards wins, and a card with no envelope counts as immediate (fail-safe).
func proposedNote(added []ai.DraftItem) string {
	const saved = "Saved as a card in the builder's Studio thread. Nothing has changed: a human reviews the card and applies it"
	now, hidden := false, false
	for _, it := range added {
		c := metaversion.ClassOf("")
		if it.Change != nil {
			c = metaversion.ClassOf(it.Change.DocType)
		}
		switch {
		case c.NewHidden && it.Change.Op != "update" && it.Change.Op != "delete": // a card that edits nothing is new
			hidden = true
		case c.Kind != metaversion.Draft:
			now = true
		}
	}
	switch {
	case now:
		return saved + ", and it takes effect as soon as it is applied: there is no draft or publish step for it."
	case hidden:
		return saved + ", which creates it; once the project is delivered the customer sees it only after it is published."
	}
	return saved + "; once the project is delivered it goes to drafts and reaches users when it is published."
}

// call runs one tool as the token's builder. A propose_* call first reads the thread's earlier
// proposals still pending (so it may build on them, as within one chat turn), and a proposal that
// passed the validators is appended to the thread; the agent is told it proposed only once that
// write landed.
func (s *server) call(ctx context.Context, req *mcp.CallToolRequest) (res *mcp.CallToolResult, err error) {
	p, ok := ctx.Value(principalKey{}).(services.MCPPrincipal)
	if !ok {
		return nil, errors.New("mcpserver: unauthenticated call") // unreachable behind authenticate
	}
	// go-sdk runs each tools/call in its own goroutine with no recover, outside gin's Recovery, so a
	// panic below would take the whole API down. It becomes a logged tool error instead.
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("mcpserver: tool call panicked", zap.String("tenant_id", p.TenantID),
				zap.String("mcp_token_id", p.TokenID), zap.String("tool", req.Params.Name), zap.Any("panic", r), zap.Stack("stack"))
			res, err = &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{
				Text: "INTERNAL: the tool failed on the server, so nothing was proposed. Try again; if it keeps failing, tell the builder.",
			}}}, nil
		}
	}()
	name := strings.TrimPrefix(req.Params.Name, toolPrefix)
	args := string(req.Params.Arguments)
	if strings.TrimSpace(args) == "" || args == "null" {
		args = "{}"
	}
	var prior []ai.DraftItem
	if isPropose(name) {
		if until, frozen := s.frozen(ctx, p); frozen {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
				"TENANT_MAINTENANCE: the workspace is frozen for maintenance or a restore until %s, so nothing was proposed. Reading still works; propose again after that time.",
				until.UTC().Format(time.RFC3339))}}}, nil
		}
		if prior, err = s.priorProposals(ctx, p); err != nil {
			return s.threadError(p, err), nil
		}
	}
	answer, added := s.runner.RunTool(ctx, p.TenantID, p.SessionID, prior, name, args)
	out := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: answer}}, IsError: !result(answer).OK}
	if len(added) == 0 {
		return out, nil
	}
	draft, err := json.Marshal(added)
	if err == nil && len(draft) > maxProposalBytes {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
			"PROPOSAL_TOO_LARGE: this proposal is %d KiB, more than the %d KiB one Studio card holds, so nothing was proposed. Split it into smaller proposals.",
			len(draft)>>10, maxProposalBytes>>10)}}}, nil
	}
	if err == nil {
		_, err = s.threads.AppendTurn(ctx, p.TenantID, p.UserID, p.SessionID, "",
			"[MCP] Agent gọi "+req.Params.Name, "Agent đề xuất qua MCP: "+result(answer).Result.Buffered+".", draft)
	}
	if err != nil {
		return s.threadError(p, err), nil
	}
	out.Content = append(out.Content, &mcp.TextContent{Text: proposedNote(added)})
	return out, nil
}

// frozen asks the API's freeze checker, as RequireTenantWritable does for every /api/v1 write: a
// proposal is a thread write, so it waits out a maintenance or restore freeze; reads do not. The
// checker already decided on a read error (a known live freeze stays frozen); here it is only logged.
func (s *server) frozen(ctx context.Context, p services.MCPPrincipal) (time.Time, bool) {
	if s.freeze == nil {
		return time.Time{}, false
	}
	until, frozen, err := s.freeze.FrozenUntil(ctx, p.TenantID)
	if err != nil {
		s.logger.Warn("mcpserver: freeze read failed", zap.String("tenant_id", p.TenantID), zap.Bool("frozen", frozen), zap.Error(err))
	}
	return until, frozen
}

// Issue #22: a key holder could append to its thread without end, and every proposal re-reads the
// whole thread. maxThreadTurns caps the turns one thread takes (the opening turn included); a key that
// needs more is replaced by a new one, which opens a new thread. maxProposalBytes caps one turn's
// draft row; the SDK's own 4 MiB request cap was the only bound before.
const (
	maxThreadTurns   = 200
	maxProposalBytes = 256 << 10
)

// errThreadFull is a thread at maxThreadTurns.
var errThreadFull = errors.New("mcpserver: thread full")

// priorProposals reads the proposals of the token's thread that are still pending: a card the builder
// applied, discarded or undid in the Studio is not replayed (issue #42), so the validators judge it
// by the live schema and the builder's drafts only. A full thread is errThreadFull.
func (s *server) priorProposals(ctx context.Context, p services.MCPPrincipal) ([]ai.DraftItem, error) {
	_, msgs, err := s.threads.GetSession(ctx, p.TenantID, p.UserID, p.SessionID)
	if err != nil {
		return nil, fmt.Errorf("mcpserver.priorProposals: %w", err)
	}
	if len(msgs) >= 2*maxThreadTurns { // a turn is two messages: the call and the proposal
		return nil, errThreadFull
	}
	return ai.PendingProposals(msgs), nil
}

// threadError is the tool error of a proposal that cannot reach its thread.
func (s *server) threadError(p services.MCPPrincipal, err error) *mcp.CallToolResult {
	msg := "THREAD_UNAVAILABLE: the proposal could not be saved to the Studio thread, so nothing was proposed. Try again."
	switch {
	case errors.Is(err, errThreadFull):
		msg = fmt.Sprintf("THREAD_FULL: the Studio thread of this token holds %d turns, the most one thread takes, so nothing was proposed. Ask the builder to issue a new MCP token: it opens a new thread.", maxThreadTurns)
	case errors.Is(err, ai.ErrSessionNotFound):
		msg = "THREAD_DELETED: the builder deleted the Studio thread of this token, so nothing was proposed. Ask them to issue a new MCP token."
	default:
		s.logger.Error("mcpserver: token thread unavailable", zap.String("tenant_id", p.TenantID), zap.String("mcp_token_id", p.TokenID), zap.Error(err))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}, IsError: true}
}

// envelope is the {ok, result|error} answer of every builder tool; Buffered is the human-readable
// summary a propose tool returns ("entity \"suppliers\" (4 field)").
type envelope struct {
	OK     bool `json:"ok"`
	Result struct {
		Buffered string `json:"buffered"`
	} `json:"result"`
}

func result(res string) envelope {
	var env envelope
	_ = json.Unmarshal([]byte(res), &env) // not JSON = not ok
	return env
}
