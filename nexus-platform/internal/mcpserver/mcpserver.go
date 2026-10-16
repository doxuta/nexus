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

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/auth"
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

// Threads reads and appends the token's assistant thread (ai.SessionStore).
type Threads interface {
	GetSession(ctx context.Context, tenantID, userID, id string) (ai.Session, []ai.Message, error)
	AppendTurn(ctx context.Context, tenantID, userID, sessionID, newTitle, userContent, assistantContent string, assistantDraft json.RawMessage) (string, error)
}

// instructions is what an MCP client hands its model about this server.
const instructions = `Nexus is a platform for business apps whose apps are metadata: tables (entities), fields, relations, layouts, automation rules, reports, apps and access. These tools read ONE workspace's model and PROPOSE changes to it. A proposal changes nothing: it is saved as a card in the builder's Studio thread, where a human reviews it, applies it to drafts and publishes it. No tool applies, publishes or writes data, so tell the user the change is waiting for review. Read before you propose (nexus_list_entities, nexus_list_fields). Names are snake_case; labels are what users see. A call refused by a validator answers {"ok":false,"error":"CODE: message"}: fix the arguments and call again. Tool descriptions are in Vietnamese.`

type server struct {
	tokens  Verifier
	runner  Runner
	threads Threads
	logger  *zap.Logger
}

// New returns the /mcp handler: bearer auth for nxm_ tokens in front of the SDK's streamable HTTP
// handler in stateless mode. Every request verifies its token again and nothing is kept between
// requests, so a revoked or expired token stops at its next request.
func New(tokens Verifier, runner Runner, threads Threads, logger *zap.Logger) http.Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &server{tokens: tokens, runner: runner, threads: threads, logger: logger}
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
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, *p)))
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

// proposedNote follows the result of a proposal: where it went, and that nothing changed yet.
const proposedNote = "Saved as a card in the builder's Studio thread. Nothing has changed: a human reviews the card, applies it to drafts and publishes it."

// call runs one tool as the token's builder. A propose_* call first reads the thread's earlier
// proposals (so it may build on them, as within one chat turn), and a proposal that passed the
// validators is appended to the thread; the agent is told it proposed only once that write landed.
func (s *server) call(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p, ok := ctx.Value(principalKey{}).(services.MCPPrincipal)
	if !ok {
		return nil, errors.New("mcpserver: unauthenticated call") // unreachable behind authenticate
	}
	name := strings.TrimPrefix(req.Params.Name, toolPrefix)
	args := string(req.Params.Arguments)
	if strings.TrimSpace(args) == "" || args == "null" {
		args = "{}"
	}
	var prior []ai.DraftItem
	if isPropose(name) {
		var err error
		if prior, err = s.priorProposals(ctx, p); err != nil {
			return s.threadError(p, err), nil
		}
	}
	res, added := s.runner.RunTool(ctx, p.TenantID, p.SessionID, prior, name, args)
	out := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res}}, IsError: !result(res).OK}
	if len(added) == 0 {
		return out, nil
	}
	draft, err := json.Marshal(added)
	if err == nil {
		_, err = s.threads.AppendTurn(ctx, p.TenantID, p.UserID, p.SessionID, "",
			"[MCP] "+req.Params.Name, "Agent đề xuất qua MCP: "+result(res).Result.Buffered+".", draft)
	}
	if err != nil {
		return s.threadError(p, err), nil
	}
	out.Content = append(out.Content, &mcp.TextContent{Text: proposedNote})
	return out, nil
}

// priorProposals reads every proposal already in the token's thread.
func (s *server) priorProposals(ctx context.Context, p services.MCPPrincipal) ([]ai.DraftItem, error) {
	_, msgs, err := s.threads.GetSession(ctx, p.TenantID, p.UserID, p.SessionID)
	if err != nil {
		return nil, fmt.Errorf("mcpserver.priorProposals: %w", err)
	}
	var out []ai.DraftItem
	for _, m := range msgs {
		var items []ai.DraftItem
		if len(m.DraftJSON) > 0 && json.Unmarshal(m.DraftJSON, &items) == nil {
			out = append(out, items...)
		}
	}
	return out, nil
}

// threadError is the tool error of a proposal that cannot reach its thread.
func (s *server) threadError(p services.MCPPrincipal, err error) *mcp.CallToolResult {
	msg := "THREAD_UNAVAILABLE: the proposal could not be saved to the Studio thread, so nothing was proposed. Try again."
	if errors.Is(err, ai.ErrSessionNotFound) {
		msg = "THREAD_DELETED: the builder deleted the Studio thread of this token, so nothing was proposed. Ask them to issue a new MCP token."
	} else {
		s.logger.Error("mcpserver: thread write failed", zap.String("tenant_id", p.TenantID), zap.String("mcp_token_id", p.TokenID), zap.Error(err))
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
