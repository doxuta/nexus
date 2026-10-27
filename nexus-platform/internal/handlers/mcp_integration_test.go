//go:build integration

// Issue #3 — MCP end to end on real MySQL, through the REAL migration chain (000098 sys_mcp_tokens)
// and the production middleware order: a builder issues an nxm_ token (the rep may not), the token
// works on /mcp only, /mcp refuses a JWT (401), an API key (403), a revoked, expired or demoted
// builder's token (401), and a propose call lands as a card in the builder's thread, which the
// Studio reads through GET /admin/ai/builder/sessions/:id.
//
// Run: go test -tags=integration ./internal/handlers/ -run TestMCP_
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/mcpserver"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// mcpBearer adds the token to every request of the MCP client.
type mcpBearer string

func (b mcpBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// mcpITRouter is the production chain of cmd/server/main.go for MCP: /api/v1 behind the composite
// Bearer verifier, the live subject and the builder tier, the auth and token handlers, and /mcp with
// its own bearer. The auth service ends agent keys with the user's sessions, as in main.go.
type mcpITRouter struct {
	r    *gin.Engine
	keys *services.APIKeyService
}

func newMCPITRouter(t *testing.T, gdb *gorm.DB, sx *sqlx.DB) mcpITRouter {
	t.Helper()
	t.Setenv("JWT_SECRET", itJWTSecret)
	t.Setenv("JWT_ACCESS_TTL", "15m")
	t.Setenv("JWT_REFRESH_TTL", "168h")
	jwtCfg, err := config.LoadJWTConfig()
	if err != nil {
		t.Fatalf("jwt config: %v", err)
	}
	tokenSvc, err := auth.NewTokenService(jwtCfg.JWTSecret, jwtCfg.AccessTTL, jwtCfg.RefreshTTL)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	userRepo := auth.NewUserRepository(gdb)
	roleRepo := auth.NewRoleRepository(gdb)
	meta := metadata.NewRepository(gdb)
	store := ai.NewSessionStore(sx)
	apiKeySvc := services.NewAPIKeyService(auth.NewAPIKeyRepository(gdb), roleRepo)
	mcpSvc := services.NewMCPTokenService(auth.NewMCPTokenRepository(gdb), store, userRepo)
	authSvc := services.NewAuthService(userRepo, auth.NewRefreshTokenRepository(gdb), tokenSvc).WithAgentKeys(mcpSvc)
	// AI off: MCP needs no gateway. The column and ref probes are main.go's (issue #42): propose_field asks
	// AddField's column check, propose_records the ref registry of the SAME record service /:entity/bulk uses.
	dyn := dynrepo.New(sx)
	records := services.NewRecordService(meta, dyn)
	agent := ai.NewAIBuilderAgent(config.AIConfig{}, nil, ai.ReaderWithAccessProbes(meta, &ai.AccessProbes{Columns: dyn, Refs: records}),
		&ai.DocReaders{Layouts: services.NewLayoutService(meta, metadata.NewLayoutRepository(gdb), nil)}, nil, zap.NewNop())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), services.NewCompositeVerifier(tokenSvc, apiKeySvc), services.NewACLService(roleRepo), APIHandlers{
		Auth:     NewAuthHandler(authSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
		Admin:    NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		Record:   NewDynamicRecordHandler(records, zap.NewNop()),
		AI:       NewAIBuilderHandler(nil, store, nil, nil, zap.NewNop()),
		APIKey:   NewAdminAPIKeyHandler(apiKeySvc, zap.NewNop()),
		MCPToken: NewAdminMCPTokenHandler(mcpSvc, zap.NewNop()),
	}, zap.NewNop(),
		WithLiveSubject(services.NewCachedUserLoader(userRepo, services.DefaultUserClaimTTL)),
		WithOperatorTier(middleware.RequireBuilderTier(userRepo, zap.NewNop())))
	r.Any("/mcp", gin.WrapH(mcpserver.New(mcpSvc, agent, store, nil, zap.NewNop()))) // as cmd/server/main.go
	return mcpITRouter{r: r, keys: apiKeySvc}
}

// mcpStatus is the HTTP status of a tools/list on /mcp with credential (raw, no "Bearer " prefix).
func mcpStatus(t *testing.T, r *gin.Engine, credential string) int {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}
	return doAuthed(t, r, http.MethodPost, "/mcp", credential, body, map[string]string{"Accept": "application/json, text/event-stream"}).Code
}

// issueMCPKey issues an agent key through the Studio route as the given builder JWT.
func issueMCPKey(t *testing.T, r *gin.Engine, jwt string) (secret, id, session string) {
	t.Helper()
	w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/mcp-tokens", jwt, map[string]any{}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("issue: %d", w.Code) // never print a body that may hold a token
	}
	var out struct {
		Data struct {
			Token  auth.MCPToken `json:"token"`
			Secret string        `json:"secret"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	return out.Data.Secret, out.Data.Token.ID, out.Data.Token.SessionID
}

func TestMCP_TokenScopeAndProposalToThread(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newMCPITRouter(t, gdb, sx)
	r, apiKeySvc := rt.r, rt.keys

	const tenant = "ten-mcp-aaaa"
	a4SeedTenant(t, gdb, tenant, "mcp-ten", "MCP Tenant")
	builderID := a4SeedUser(t, gdb, tenant, "builder@mcp.test", true, nil)
	if err := gdb.Exec("UPDATE users SET is_platform_operator = 1 WHERE id = ?", builderID).Error; err != nil {
		t.Fatalf("builder flag: %v", err)
	}
	repRole := a4SeedRole(t, gdb, tenant, "sales-rep", []auth.RoleAction{grant("crm_contacts", "view")})
	a4SeedUser(t, gdb, tenant, "rep@mcp.test", false, &repRole)
	builderTok := itLogin(t, r, "mcp-ten", "builder@mcp.test", authPassword).Data.AccessToken
	repTok := itLogin(t, r, "mcp-ten", "rep@mcp.test", authPassword).Data.AccessToken

	issue := func() (secret, id, session string) { t.Helper(); return issueMCPKey(t, r, builderTok) }
	mcpPost := func(credential string) int { t.Helper(); return mcpStatus(t, r, credential) }

	// ---- issue: builders only ------------------------------------------------------------------
	if w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/mcp-tokens", repTok, map[string]any{}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("the rep issuing a token: %d; want 403", w.Code) // never print a body that may hold a token
	}
	secret, tokID, session := issue()
	if !strings.HasPrefix(secret, services.MCPTokenPrefix) || session == "" {
		t.Fatalf("issued %q (session %q); want an nxm_ token bound to a thread", secret[:4], session)
	}
	var stored string
	if err := sx.Get(&stored, "SELECT token_hash FROM sys_mcp_tokens WHERE id = ?", tokID); err != nil || stored != auth.HashRefreshToken(secret) {
		t.Fatalf("stored hash: %v; want the SHA-256 only", err)
	}
	if w := doAuthed(t, r, http.MethodGet, "/api/v1/admin/mcp-tokens", builderTok, nil, nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), tokID) || strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), stored) {
		t.Fatalf("list: %d; want the token listed without its secret or hash", w.Code)
	}

	// ---- the scope matrix ------------------------------------------------------------------------
	_, keySecret, err := apiKeySvc.IssueAPIKey(context.Background(), tenant, services.IssueAPIKeyInput{Name: "it", RoleID: repRole})
	if err != nil {
		t.Fatalf("issue api key: %v", err)
	}
	for _, tc := range []struct {
		name, method, path, credential string
		want                           int
	}{
		{"nxm_ on an admin route", http.MethodGet, "/api/v1/admin/mcp-tokens", secret, http.StatusUnauthorized},
		{"nxm_ on a metadata route", http.MethodGet, "/api/v1/metadata/entities", secret, http.StatusUnauthorized},
		{"nxm_ on /auth/me", http.MethodGet, "/api/v1/auth/me", secret, http.StatusUnauthorized},
	} {
		if w := doAuthed(t, r, tc.method, tc.path, tc.credential, nil, nil); w.Code != tc.want {
			t.Errorf("%s: %d; want %d", tc.name, w.Code, tc.want)
		}
	}
	for _, tc := range []struct {
		name, credential string
		want             int
	}{
		{"no credential", "", http.StatusUnauthorized},
		{"a JWT access token", builderTok, http.StatusUnauthorized},
		{"an API key", keySecret, http.StatusForbidden},
		{"a live MCP token", secret, http.StatusOK},
	} {
		if got := mcpPost(tc.credential); got != tc.want {
			t.Errorf("/mcp with %s: %d; want %d", tc.name, got, tc.want)
		}
	}

	// ---- a proposal over MCP becomes a card in the builder's thread ---------------------------
	ts := httptest.NewServer(r)
	defer ts.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "it", Version: "v0"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: mcpBearer(secret)}}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "nexus_propose_entity", Arguments: map[string]any{
		"name": "suppliers", "label": "Suppliers",
		"fields": []map[string]any{{"name": "name", "label": "Name", "fieldType": "text", "required": true}, {"name": "email", "label": "Email", "fieldType": "email"}},
	}})
	if err != nil || res.IsError {
		t.Fatalf("propose over MCP: %v %+v", err, res)
	}
	w := doAuthed(t, r, http.MethodGet, "/api/v1/admin/ai/builder/sessions/"+session, builderTok, nil, nil)
	var thread struct {
		Data struct {
			Messages []struct {
				Role  string         `json:"role"`
				Draft []ai.DraftItem `json:"draft"`
			} `json:"messages"`
		} `json:"data"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &thread) != nil {
		t.Fatalf("read thread: %d %s", w.Code, w.Body.String())
	}
	var cards []ai.DraftItem
	for _, m := range thread.Data.Messages {
		cards = append(cards, m.Draft...)
	}
	if len(cards) != 1 || cards[0].Kind != "entity" || cards[0].Entity.Name != "suppliers" || cards[0].Change == nil {
		t.Fatalf("thread cards = %+v; want the suppliers entity with its change envelope", cards)
	}
	var used int
	if err := sx.Get(&used, "SELECT COUNT(*) FROM sys_mcp_tokens WHERE id = ? AND last_used_at IS NOT NULL", tokID); err != nil || used != 1 {
		t.Fatalf("last_used_at not recorded: %v", err)
	}
	var live int
	if err := sx.Get(&live, "SELECT COUNT(*) FROM sys_entities WHERE tenant_id = ? AND name = 'suppliers'", tenant); err != nil || live != 0 {
		t.Fatalf("the proposal must not create the table (%d rows, %v)", live, err)
	}

	// ---- the token dies: revoked, expired, its builder demoted --------------------------------
	if w := doAuthed(t, r, http.MethodDelete, "/api/v1/admin/mcp-tokens/"+tokID, repTok, nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("the rep revoking: %d; want 403", w.Code)
	}
	if w := doAuthed(t, r, http.MethodDelete, "/api/v1/admin/mcp-tokens/"+tokID, builderTok, nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	if got := mcpPost(secret); got != http.StatusUnauthorized {
		t.Errorf("/mcp with a revoked token: %d; want 401", got)
	}
	expired, expiredID, _ := issue()
	if _, err := sx.Exec("UPDATE sys_mcp_tokens SET expires_at = NOW(3) - INTERVAL 1 SECOND WHERE id = ?", expiredID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if got := mcpPost(expired); got != http.StatusUnauthorized {
		t.Errorf("/mcp with an expired token: %d; want 401", got)
	}
	demoted, _, _ := issue()
	if got := mcpPost(demoted); got != http.StatusOK {
		t.Fatalf("control: a fresh token: %d", got)
	}
	if err := gdb.Exec("UPDATE users SET is_platform_operator = 0 WHERE id = ?", builderID).Error; err != nil {
		t.Fatalf("demote: %v", err)
	}
	if got := mcpPost(demoted); got != http.StatusUnauthorized {
		t.Errorf("/mcp after the builder lost the builder flag: %d; want 401", got)
	}
}

func mcpText(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func mcpConnect(t *testing.T, url, secret string) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "it", Version: "v0"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: mcpBearer(secret)}}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// mcpBuilder seeds a tenant with an active builder (admin + builder flag) and returns its ids.
func mcpBuilder(t *testing.T, gdb *gorm.DB, tenant, slug string) string {
	t.Helper()
	a4SeedTenant(t, gdb, tenant, slug, slug)
	id := a4SeedUser(t, gdb, tenant, "builder@"+slug+".test", true, nil)
	if err := gdb.Exec("UPDATE users SET is_platform_operator = 1 WHERE id = ?", id).Error; err != nil {
		t.Fatalf("builder flag: %v", err)
	}
	return id
}

// On a delivered project the builder's edits are drafts, and the in-app builder agent reads them
// (DraftCaller on /api/v1/admin/*). An agent over /mcp must read the same drafts: a proposal built on
// the published document would hide the builder's pending edit and, once applied, revert it.
func TestMCP_ReadsTheBuildersDrafts(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	ctx := context.Background()
	const tenant = "ten-mcp-draft"
	builderID := mcpBuilder(t, gdb, tenant, "mcp-draft")
	entID := uuid.NewString()
	if err := gdb.Create(&metadata.EntityDefinition{ID: entID, TenantID: tenant, Name: "it_tickets", Label: "Tickets", DataTable: "obj_it_tickets"}).Error; err != nil {
		t.Fatalf("entity: %v", err)
	}
	status := `{"values":["open","closed"]}`
	if err := gdb.Create(&metadata.FieldDefinition{ID: uuid.NewString(), EntityID: entID, TenantID: tenant, Name: "status", Label: "Status", FieldType: "picklist", Options: &status, FieldOrder: 10}).Error; err != nil {
		t.Fatalf("field: %v", err)
	}
	meta := metadata.NewRepository(gdb)
	reg := metaversion.NewRegistry(services.NewWorkflowDocAdapter())
	drafts := metaversion.NewDraftRepo(sx, reg)
	vers := metaversion.NewVersionRepo(sx)
	gate := services.NewDraftGate(vers)
	raw := workflow.NewSQLRepository(sx)
	wf := services.NewWorkflowService(services.NewDraftWorkflowRepo(raw, drafts, gate)).WithLiveRepo(raw).WithEntityChecker(meta)
	rule := func(value string) services.CreateWorkflowInput {
		on := true
		return services.CreateWorkflowInput{Name: "Close ticket", EntityName: "it_tickets", Trigger: "after_update", IsActive: &on,
			Rules:   []services.WorkflowRuleInput{{Field: "status", Op: workflow.OpEq, Value: value}},
			Actions: []services.WorkflowActionInput{{Type: "field_update", Field: "status", Value: "closed"}}}
	}
	// The Caller DraftCaller puts on a builder's /api/v1/admin/* request.
	builderCtx := metaversion.WithCaller(ctx, metaversion.Caller{UserID: builderID, TenantID: tenant, HomeTenantID: tenant, IsBuilder: true, ReadDraft: true})
	live, err := wf.CreateWorkflow(ctx, tenant, rule("open"))
	if err != nil {
		t.Fatalf("create the live rule: %v", err)
	}
	if _, err := services.NewPublishService(sx, drafts, vers, reg, gate, nil, nil).Baseline(builderCtx, tenant, "first"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	edit := rule("closed")
	if _, err := wf.UpdateWorkflow(builderCtx, tenant, live.ID, services.UpdateWorkflowInput{Name: edit.Name, EntityName: edit.EntityName,
		Trigger: edit.Trigger, Rules: edit.Rules, Actions: edit.Actions, IsActive: edit.IsActive, ActorID: builderID}); err != nil {
		t.Fatalf("the builder's draft edit: %v", err)
	}

	agent := ai.NewAIBuilderAgent(config.AIConfig{}, nil, meta, &ai.DocReaders{Workflows: wf}, nil, zap.NewNop())
	store := ai.NewSessionStore(sx)
	mcpSvc := services.NewMCPTokenService(auth.NewMCPTokenRepository(gdb), store, auth.NewUserRepository(gdb))
	_, secret, err := mcpSvc.Issue(ctx, auth.Subject{UserID: builderID, TenantID: tenant, IsAdmin: true}, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	ts := httptest.NewServer(mcpserver.New(mcpSvc, agent, store, nil, zap.NewNop()))
	defer ts.Close()
	cs := mcpConnect(t, ts.URL, secret)

	cond := func(v string) string { return `"op":"eq","field":"status","value":"` + v + `"` }
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "nexus_list_workflows", Arguments: map[string]any{"entity": "it_tickets"}})
	if err != nil {
		t.Fatalf("list_workflows: %v", err)
	}
	if got := mcpText(res); !strings.Contains(got, cond("closed")) || strings.Contains(got, cond("open")) {
		t.Fatalf("over /mcp the rule reads %s; want the builder's draft (value closed), not the published rule", got)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "nexus_propose_workflow", Arguments: map[string]any{
		"name": "Close ticket", "entity": "it_tickets", "trigger": "after_update",
		"condition": map[string]any{"match": "and", "rules": []map[string]any{{"field": "status", "op": "eq", "value": "closed"}}},
		"actions":   []map[string]any{{"type": "field_update", "field": "status", "value": "closed"}}}})
	if err != nil {
		t.Fatalf("propose_workflow: %v", err)
	}
	if got := mcpText(res); !strings.Contains(got, `"op":"update"`) || strings.Contains(got, "changedPaths") {
		t.Fatalf("proposing the draft as it stands: %s; want an update with nothing changed against the draft", got)
	}
}

// An MCP client may run tool calls in parallel: every proposal still gets its own place in the
// thread, because the Studio keys a card (and what was applied of it) by that place.
func TestMCP_ParallelProposalsGetOnePlaceEach(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	ctx := context.Background()
	const tenant = "ten-mcp-par"
	builderID := mcpBuilder(t, gdb, tenant, "mcp-par")
	store := ai.NewSessionStore(sx)
	mcpSvc := services.NewMCPTokenService(auth.NewMCPTokenRepository(gdb), store, auth.NewUserRepository(gdb))
	tok, secret, err := mcpSvc.Issue(ctx, auth.Subject{UserID: builderID, TenantID: tenant, IsAdmin: true}, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	agent := ai.NewAIBuilderAgent(config.AIConfig{}, nil, metadata.NewRepository(gdb), nil, nil, zap.NewNop())
	ts := httptest.NewServer(mcpserver.New(mcpSvc, agent, store, nil, zap.NewNop()))
	defer ts.Close()

	const n = 12
	clients := make([]*mcp.ClientSession, n)
	for i := range clients {
		clients[i] = mcpConnect(t, ts.URL, secret)
	}
	var wg sync.WaitGroup
	errs := make([]string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := clients[i].CallTool(ctx, &mcp.CallToolParams{Name: "nexus_propose_entity", Arguments: map[string]any{
				"name": fmt.Sprintf("par_tbl_%d", i), "label": fmt.Sprintf("Par %d", i),
				"fields": []map[string]any{{"name": "name", "label": "Name", "fieldType": "text"}}}})
			switch {
			case err != nil:
				errs[i] = err.Error()
			case res.IsError:
				errs[i] = mcpText(res)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		if e != "" {
			t.Errorf("proposal %d: %s", i, e)
		}
	}
	var rows, places int
	if err := sx.Get(&rows, "SELECT COUNT(*) FROM sys_ai_messages WHERE session_id = ? AND tenant_id = ?", tok.SessionID, tenant); err != nil {
		t.Fatal(err)
	}
	if err := sx.Get(&places, "SELECT COUNT(DISTINCT seq) FROM sys_ai_messages WHERE session_id = ? AND tenant_id = ?", tok.SessionID, tenant); err != nil {
		t.Fatal(err)
	}
	if want := 2 + 2*n; rows != want || places != want {
		t.Fatalf("%d parallel proposals left %d messages in %d places; want %d in %d", n, rows, places, want, want)
	}
}

// The production freeze checker (the one every /api/v1 write passes): while a tenant is frozen for
// maintenance or a restore, an agent reads but cannot append a proposal to its thread.
func TestMCP_FrozenTenantReadsButDoesNotPropose(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	ctx := context.Background()
	const tenant = "ten-mcp-frozen"
	builderID := mcpBuilder(t, gdb, tenant, "mcp-frozen")
	userRepo := auth.NewUserRepository(gdb)
	store := ai.NewSessionStore(sx)
	mcpSvc := services.NewMCPTokenService(auth.NewMCPTokenRepository(gdb), store, userRepo)
	tok, secret, err := mcpSvc.Issue(ctx, auth.Subject{UserID: builderID, TenantID: tenant, IsAdmin: true}, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := gdb.Exec("UPDATE tenants SET maintenance_until = NOW(3) + INTERVAL 10 MINUTE WHERE id = ?", tenant).Error; err != nil {
		t.Fatalf("freeze: %v", err)
	}
	agent := ai.NewAIBuilderAgent(config.AIConfig{}, nil, metadata.NewRepository(gdb), nil, nil, zap.NewNop())
	ts := httptest.NewServer(mcpserver.New(mcpSvc, agent, store, services.NewTenantFreeze(userRepo, zap.NewNop()), zap.NewNop()))
	defer ts.Close()
	cs := mcpConnect(t, ts.URL, secret)

	if res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "nexus_list_entities", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("a read while frozen: %v %s; want it served", err, mcpText(res))
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "nexus_propose_entity", Arguments: map[string]any{
		"name": "frozen_tbl", "label": "Frozen", "fields": []map[string]any{{"name": "name", "label": "Name", "fieldType": "text"}}}})
	if err != nil || !res.IsError || !strings.HasPrefix(mcpText(res), "TENANT_MAINTENANCE:") {
		t.Fatalf("a proposal while frozen: %v %s; want a TENANT_MAINTENANCE tool error", err, mcpText(res))
	}
	var rows int
	if err := sx.Get(&rows, "SELECT COUNT(*) FROM sys_ai_messages WHERE session_id = ? AND tenant_id = ?", tok.SessionID, tenant); err != nil || rows != 2 {
		t.Fatalf("thread rows = %d (%v); want only the 2 of the key's opening turn", rows, err)
	}
}

// Retiring a workspace (DELETE /admin/tenants/:id soft-deletes its row) closes every door: its
// agent keys stop at their next request, like its logins.
func TestMCP_RetiredTenantKeysStop(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	ctx := context.Background()
	const tenant = "ten-mcp-retired"
	builderID := mcpBuilder(t, gdb, tenant, "mcp-retired")
	store := ai.NewSessionStore(sx)
	mcpSvc := services.NewMCPTokenService(auth.NewMCPTokenRepository(gdb), store, auth.NewUserRepository(gdb))
	_, secret, err := mcpSvc.Issue(ctx, auth.Subject{UserID: builderID, TenantID: tenant, IsAdmin: true}, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	agent := ai.NewAIBuilderAgent(config.AIConfig{}, nil, metadata.NewRepository(gdb), nil, nil, zap.NewNop())
	ts := httptest.NewServer(mcpserver.New(mcpSvc, agent, store, nil, zap.NewNop()))
	defer ts.Close()
	list := func() int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nexus_list_entities","arguments":{}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := list(); got != http.StatusOK {
		t.Fatalf("control: a live workspace's key: %d", got)
	}
	// What services.TenantProvisionService.RetireTenant does: a GORM soft delete of the tenant row.
	if err := gdb.Where("id = ?", tenant).Delete(&auth.Tenant{}).Error; err != nil {
		t.Fatalf("retire: %v", err)
	}
	if got := list(); got != http.StatusUnauthorized {
		t.Fatalf("a retired workspace's key: %d; want 401", got)
	}
}

// A stolen session can mint an agent key; the victim's incident response must end it. Logout-all and
// a password change go through the credential-level endAllSessions, which now ends agent keys too.
func TestMCP_LogoutAllAndPasswordChangeEndAgentKeys(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	r := newMCPITRouter(t, gdb, sx).r
	const tenant = "ten-mcp-incident"
	mcpBuilder(t, gdb, tenant, "mcp-incident")
	login := func() string {
		t.Helper()
		return itLogin(t, r, "mcp-incident", "builder@mcp-incident.test", authPassword).Data.AccessToken
	}

	jwt := login()
	secret, _, _ := issueMCPKey(t, r, jwt)
	if got := mcpStatus(t, r, secret); got != http.StatusOK {
		t.Fatalf("control: a fresh key: %d", got)
	}
	if w := doAuthed(t, r, http.MethodPost, "/api/v1/auth/logout-all", jwt, map[string]any{}, nil); w.Code >= 300 {
		t.Fatalf("logout-all: %d %s", w.Code, w.Body.String())
	}
	if got := mcpStatus(t, r, secret); got != http.StatusUnauthorized {
		t.Errorf("the key after logout-all: %d; want 401", got)
	}

	jwt = login()
	secret, _, _ = issueMCPKey(t, r, jwt)
	if got := mcpStatus(t, r, secret); got != http.StatusOK {
		t.Fatalf("control: a fresh key: %d", got)
	}
	w := doAuthed(t, r, http.MethodPut, "/api/v1/auth/me/password", jwt, map[string]any{"currentPassword": authPassword, "newPassword": "Br4nd-New-Pw-2026!"}, nil)
	if w.Code >= 300 {
		t.Fatalf("password change: %d %s", w.Code, w.Body.String())
	}
	if got := mcpStatus(t, r, secret); got != http.StatusUnauthorized {
		t.Errorf("the key after a password change: %d; want 401", got)
	}
}

// Issue #42: an MCP thread replays only the proposals still pending. The Studio settles a card it
// applied, discarded or undid through POST /admin/ai/builder/sessions/:id/settled, and from then on the
// draft-aware validators judge against the live schema alone. Each case first shows the pending card
// still counting (the replay works), then the settled one not counting. Last, a relation or user field
// whose <name>_id column is taken is refused at propose time with Apply's own code.
func TestMCP_SettledProposalsAreNotReplayed(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	r := newMCPITRouter(t, gdb, sx).r
	ctx := context.Background()
	const tenant = "ten-mcp-settle"
	mcpBuilder(t, gdb, tenant, "mcp-settle")
	repRole := a4SeedRole(t, gdb, tenant, "sales-rep", nil)
	a4SeedUser(t, gdb, tenant, "rep@mcp-settle.test", false, &repRole)
	jwt := itLogin(t, r, "mcp-settle", "builder@mcp-settle.test", authPassword).Data.AccessToken
	repJWT := itLogin(t, r, "mcp-settle", "rep@mcp-settle.test", authPassword).Data.AccessToken
	secret, _, session := issueMCPKey(t, r, jwt)
	ts := httptest.NewServer(r)
	defer ts.Close()
	cs := mcpConnect(t, ts.URL+"/mcp", secret)

	admin := func(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return doAuthed(t, r, method, "/api/v1"+path, jwt, body, nil)
	}
	// The live table, applied the way the Studio applies: through the admin API.
	if w := admin(t, http.MethodPost, "/admin/entities", map[string]any{"name": "st_patients", "label": "Patients", "fields": []map[string]any{
		{"name": "full_name", "label": "Name", "fieldType": "text", "isRequired": true},
		{"name": "code", "label": "Code", "fieldType": "text"},
		{"name": "ref_no", "label": "Ref", "fieldType": "text"},
		{"name": "status", "label": "Status", "fieldType": "picklist", "options": map[string]any{"values": []string{"new", "done"}}},
	}}); w.Code != http.StatusCreated {
		t.Fatalf("create table: %d %s", w.Code, w.Body.String())
	}

	// propose calls a tool; "" means the proposal landed, else the validator's "CODE: message".
	propose := func(t *testing.T, tool string, args map[string]any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "nexus_" + tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if !res.IsError {
			return ""
		}
		var env struct{ Error string }
		if json.Unmarshal([]byte(mcpText(res)), &env) != nil || env.Error == "" {
			return mcpText(res)
		}
		return env.Error
	}
	mustPropose := func(t *testing.T, tool string, args map[string]any) {
		t.Helper()
		if got := propose(t, tool, args); got != "" {
			t.Fatalf("%s refused: %s", tool, got)
		}
	}
	// card is the newest card of the thread: its turn, and the proposal the Studio shows.
	type card struct {
		seq  int
		item ai.DraftItem
	}
	last := func(t *testing.T) card {
		t.Helper()
		w := admin(t, http.MethodGet, "/admin/ai/builder/sessions/"+session, nil)
		var th struct {
			Data struct {
				Messages []struct {
					Seq   int            `json:"seq"`
					Draft []ai.DraftItem `json:"draft"`
				} `json:"messages"`
			} `json:"data"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &th) != nil {
			t.Fatalf("read thread: %d %s", w.Code, w.Body.String())
		}
		for i := len(th.Data.Messages) - 1; i >= 0; i-- {
			if m := th.Data.Messages[i]; len(m.Draft) == 1 {
				return card{m.Seq, m.Draft[0]}
			}
		}
		t.Fatal("no card in the thread")
		return card{}
	}
	settle := func(t *testing.T, c card) {
		t.Helper()
		if w := admin(t, http.MethodPost, "/admin/ai/builder/sessions/"+session+"/settled", map[string]any{"seq": c.seq, "index": 0}); w.Code != http.StatusNoContent {
			t.Fatalf("settle seq %d: %d %s", c.seq, w.Code, w.Body.String())
		}
	}
	sortBy := func(field string) map[string]any {
		return map[string]any{"entity": "st_patients", "viewType": "list", "layoutJson": map[string]any{
			"version": 1, "columns": []map[string]any{{"field": "full_name"}, {"field": field}}, "defaultSort": map[string]any{"field": field, "order": "asc"}}}
	}
	indexed := func(field string) map[string]any {
		return map[string]any{"entity": "st_patients", "field": field, "changes": map[string]any{"options": map[string]any{"indexed": true}}}
	}

	// The rep cannot settle anything: the route is the builder's.
	if w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/ai/builder/sessions/"+session+"/settled", repJWT, map[string]any{"seq": 1, "index": 0}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("the rep settling a card: %d; want 403", w.Code)
	}

	t.Run("a discarded index no longer vouches for a sort", func(t *testing.T) {
		mustPropose(t, "propose_edit_field", indexed("code"))
		edit := last(t)
		mustPropose(t, "propose_layout", sortBy("code")) // pending: the proposed index counts
		settle(t, edit)
		if got := propose(t, "propose_layout", sortBy("code")); !strings.HasPrefix(got, "LAYOUT_INVALID") {
			t.Fatalf("a sort on code after its index was discarded: %q; want LAYOUT_INVALID (code has no index live)", got)
		}
	})

	t.Run("an applied then reverted index no longer vouches for a sort", func(t *testing.T) {
		mustPropose(t, "propose_edit_field", indexed("ref_no"))
		edit := last(t)
		if edit.item.EditField == nil || edit.item.EditField.FieldID == "" {
			t.Fatalf("edit card = %+v; want the field id the Studio applies by", edit.item)
		}
		field := "/admin/fields/" + edit.item.EditField.FieldID
		if w := admin(t, http.MethodPut, field, map[string]any{"options": map[string]any{"indexed": true}}); w.Code != http.StatusOK {
			t.Fatalf("apply: %d %s", w.Code, w.Body.String())
		}
		settle(t, edit)
		mustPropose(t, "propose_layout", sortBy("ref_no")) // applied: the live index counts
		if w := admin(t, http.MethodPut, field, map[string]any{"options": map[string]any{"indexed": false}}); w.Code != http.StatusOK {
			t.Fatalf("revert: %d %s", w.Code, w.Body.String())
		}
		if got := propose(t, "propose_layout", sortBy("ref_no")); !strings.HasPrefix(got, "LAYOUT_INVALID") {
			t.Fatalf("a sort on ref_no after its index was reverted: %q; want LAYOUT_INVALID", got)
		}
	})

	t.Run("a discarded field can be proposed again", func(t *testing.T) {
		website := map[string]any{"entity": "st_patients", "name": "website", "label": "Website", "fieldType": "text"}
		mustPropose(t, "propose_field", website)
		field := last(t)
		if got := propose(t, "propose_field", website); !strings.HasPrefix(got, "FIELD_ALREADY_EXISTS") {
			t.Fatalf("the same field while its card is pending: %q; want FIELD_ALREADY_EXISTS", got)
		}
		settle(t, field)
		mustPropose(t, "propose_field", website)
	})

	t.Run("a field applied then deleted keeps its name, as at Apply", func(t *testing.T) {
		fax := map[string]any{"entity": "st_patients", "name": "fax", "label": "Fax", "fieldType": "text"}
		mustPropose(t, "propose_field", fax)
		card := last(t)
		w := admin(t, http.MethodPost, "/admin/fields", map[string]any{"entityName": "st_patients", "name": "fax", "label": "Fax", "fieldType": "text"})
		var added struct {
			Data struct {
				Field struct{ ID string } `json:"field"`
			} `json:"data"`
		}
		if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &added) != nil || added.Data.Field.ID == "" {
			t.Fatalf("apply: %d %s", w.Code, w.Body.String())
		}
		settle(t, card)
		if got := propose(t, "propose_field", fax); !strings.HasPrefix(got, "FIELD_ALREADY_EXISTS") {
			t.Fatalf("the applied field again: %q; want FIELD_ALREADY_EXISTS (it is live)", got)
		}
		if w := admin(t, http.MethodDelete, "/admin/fields/"+added.Data.Field.ID, nil); w.Code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", w.Code, w.Body.String())
		}
		got := propose(t, "propose_field", fax)
		w = admin(t, http.MethodPost, "/admin/fields", map[string]any{"entityName": "st_patients", "name": "fax", "label": "Fax", "fieldType": "text"})
		var env struct{ Code string }
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if !strings.HasPrefix(got, "FIELD_ALREADY_EXISTS") || env.Code != "FIELD_ALREADY_EXISTS" {
			t.Fatalf("the deleted field's name: propose %q, Apply %d %s; want FIELD_ALREADY_EXISTS from both", got, w.Code, w.Body.String())
		}
	})

	t.Run("an applied then reverted picklist value no longer vouches for a dependent value", func(t *testing.T) {
		values := func(v ...string) map[string]any { return map[string]any{"options": map[string]any{"values": v}} }
		mustPropose(t, "propose_edit_field", map[string]any{"entity": "st_patients", "field": "status", "changes": values("new", "done", "lost")})
		edit := last(t)
		field := "/admin/fields/" + edit.item.EditField.FieldID
		if w := admin(t, http.MethodPut, field, values("new", "done", "lost")); w.Code != http.StatusOK {
			t.Fatalf("apply: %d %s", w.Code, w.Body.String())
		}
		settle(t, edit)
		reason := func(name string) map[string]any {
			return map[string]any{"entity": "st_patients", "name": name, "label": "Why", "fieldType": "picklist",
				"options": map[string]any{"values": []string{"price", "other"}, "controllingField": "status", "dependentValues": map[string]any{"lost": []string{"price"}}}}
		}
		mustPropose(t, "propose_field", reason("lost_why")) // applied: the live value counts
		if w := admin(t, http.MethodPut, field, values("new", "done")); w.Code != http.StatusOK {
			t.Fatalf("revert: %d %s", w.Code, w.Body.String())
		}
		if got := propose(t, "propose_field", reason("lost_cause")); !strings.HasPrefix(got, "FIELD_DEPENDENT_PICKLIST_INVALID") {
			t.Fatalf("a dependent value keyed on a reverted status value: %q; want FIELD_DEPENDENT_PICKLIST_INVALID", got)
		}
	})

	t.Run("an applied parent card's @ref still resolves for its child; a discarded one does not", func(t *testing.T) {
		for _, ent := range []map[string]any{
			{"name": "st_companies", "label": "Companies", "fields": []map[string]any{{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true}}},
			{"name": "st_contacts", "label": "Contacts", "fields": []map[string]any{{"name": "full_name", "label": "Name", "fieldType": "text"},
				{"name": "company", "label": "Company", "fieldType": "relation", "options": map[string]any{"relatedEntity": "st_companies"}}}},
		} {
			if w := admin(t, http.MethodPost, "/admin/entities", ent); w.Code != http.StatusCreated {
				t.Fatalf("create %s: %d %s", ent["name"], w.Code, w.Body.String())
			}
		}
		bulk := func(t *testing.T, c card) int {
			t.Helper()
			w := admin(t, http.MethodPost, "/"+c.item.Records.Entity+"/bulk?mode=partial", map[string]any{"records": c.item.Records.Rows})
			var res struct {
				Data struct{ Created int } `json:"data"`
			}
			if w.Code >= 300 || json.Unmarshal(w.Body.Bytes(), &res) != nil {
				t.Fatalf("apply %s: %d %s", c.item.Records.Entity, w.Code, w.Body.String())
			}
			return res.Data.Created
		}
		mustPropose(t, "propose_records", map[string]any{"entity": "st_companies", "rows": []map[string]any{{"title": "Acme", "$ref": "acme"}}})
		parent := last(t)
		if n := bulk(t, parent); n != 1 {
			t.Fatalf("parent created %d; want 1", n)
		}
		settle(t, parent)
		mustPropose(t, "propose_records", map[string]any{"entity": "st_contacts", "rows": []map[string]any{{"full_name": "Bình", "company": "@acme"}}})
		if n := bulk(t, last(t)); n != 1 {
			t.Fatalf("child created %d at Apply; want 1 (the ref resolves there too)", n)
		}

		mustPropose(t, "propose_records", map[string]any{"entity": "st_companies", "rows": []map[string]any{{"title": "Beta", "$ref": "beta"}}})
		settle(t, last(t)) // discarded: never applied, nothing registered
		if got := propose(t, "propose_records", map[string]any{"entity": "st_contacts", "rows": []map[string]any{{"full_name": "Chi", "company": "@beta"}}}); !strings.HasPrefix(got, "RECORD_REF_UNRESOLVED") {
			t.Fatalf("a child of a discarded parent card: %q; want RECORD_REF_UNRESOLVED", got)
		}
	})

	t.Run("a discarded picklist edit no longer vouches for a dependent value", func(t *testing.T) {
		mustPropose(t, "propose_edit_field", map[string]any{"entity": "st_patients", "field": "status",
			"changes": map[string]any{"options": map[string]any{"values": []string{"new", "done", "hold"}}}})
		edit := last(t)
		reason := func(name string) map[string]any {
			return map[string]any{"entity": "st_patients", "name": name, "label": "Reason", "fieldType": "picklist",
				"options": map[string]any{"values": []string{"parts", "other"}, "controllingField": "status", "dependentValues": map[string]any{"hold": []string{"parts"}}}}
		}
		mustPropose(t, "propose_field", reason("hold_reason")) // pending: "hold" is a value of the proposed status
		settle(t, edit)
		if got := propose(t, "propose_field", reason("hold_cause")); !strings.HasPrefix(got, "FIELD_DEPENDENT_PICKLIST_INVALID") {
			t.Fatalf("a dependent value keyed on a discarded status value: %q; want FIELD_DEPENDENT_PICKLIST_INVALID", got)
		}
	})

	t.Run("a taken <name>_id column is refused at propose with Apply's code", func(t *testing.T) {
		if w := admin(t, http.MethodPost, "/admin/entities", map[string]any{"name": "st_visits", "label": "Visits", "fields": []map[string]any{
			{"name": "note", "label": "Note", "fieldType": "text"},
			{"name": "patient_id", "label": "Patient code", "fieldType": "text", "options": map[string]any{"indexed": true}},
		}}); w.Code != http.StatusCreated {
			t.Fatalf("create visits: %d %s", w.Code, w.Body.String())
		}
		for _, f := range []map[string]any{
			{"entityName": "st_visits", "name": "owner", "label": "Owner", "fieldType": "user"},
			{"entityName": "st_visits", "name": "patient", "label": "Patient", "fieldType": "relation", "options": map[string]any{"relatedEntity": "st_patients"}},
		} {
			args := map[string]any{"entity": f["entityName"], "name": f["name"], "label": f["label"], "fieldType": f["fieldType"], "options": f["options"]}
			if got := propose(t, "propose_field", args); !strings.HasPrefix(got, "FIELD_RELATION_COLUMN_CONFLICT") {
				t.Errorf("propose %s %s: %q; want FIELD_RELATION_COLUMN_CONFLICT", f["fieldType"], f["name"], got)
			}
			w := admin(t, http.MethodPost, "/admin/fields", f)
			var env struct{ Code string }
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if w.Code != http.StatusUnprocessableEntity || env.Code != "FIELD_RELATION_COLUMN_CONFLICT" {
				t.Errorf("Apply %s %s: %d %s; want the same refusal", f["fieldType"], f["name"], w.Code, w.Body.String())
			}
		}
		mustPropose(t, "propose_field", map[string]any{"entity": "st_visits", "name": "doctor", "label": "Doctor", "fieldType": "user"}) // doctor_id is free
	})
}

// Issue #23: an outside agent answers "who can edit contacts?" whole. nexus_list_roles {grants: true}
// lists this workspace's roles with their grants (the role reader main.go wires). A token of tenant A
// never sees tenant B's roles or grants (nor B's token A's), and as a read it writes nothing: no
// role or grant changes, and the token's thread keeps only its opening turn.
func TestMCP_ListRolesWithGrants(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	ctx := context.Background()
	const tenantA, tenantB = "ten-mcp-grants", "ten-mcp-grants-b"
	builderA := mcpBuilder(t, gdb, tenantA, "mcp-grants")
	builderB := mcpBuilder(t, gdb, tenantB, "mcp-grants-b")
	roleRepo := auth.NewRoleRepository(gdb)
	roles := services.NewRoleService(roleRepo)
	if _, err := roles.CreateRole(ctx, tenantA, services.CreateRoleInput{Name: "it_contacts_editor", Label: "Contacts editor", Actions: []services.RoleActionInput{
		{EntityName: "it_contacts", Action: "view", AccessLevel: "all"}, {EntityName: "it_contacts", Action: "edit", AccessLevel: "owner"}}}); err != nil {
		t.Fatalf("role A: %v", err)
	}
	// B's role grants on a table of the same name: a leak would show up as a role or as a grant.
	if _, err := roles.CreateRole(ctx, tenantB, services.CreateRoleInput{Name: "it_b_contacts_admin", Label: "B", Actions: []services.RoleActionInput{
		{EntityName: "it_contacts", Action: "delete", AccessLevel: "all"}, {EntityName: "*", Action: "edit", AccessLevel: "all"}}}); err != nil {
		t.Fatalf("role B: %v", err)
	}
	store := ai.NewSessionStore(sx)
	mcpSvc := services.NewMCPTokenService(auth.NewMCPTokenRepository(gdb), store, auth.NewUserRepository(gdb))
	agent := ai.NewAIBuilderAgent(config.AIConfig{}, nil, ai.ReaderWithAccessProbes(metadata.NewRepository(gdb), &ai.AccessProbes{Roles: roleRepo}), nil, nil, zap.NewNop())
	ts := httptest.NewServer(mcpserver.New(mcpSvc, agent, store, nil, zap.NewNop()))
	defer ts.Close()

	type role struct {
		Name   string                    `json:"name"`
		Grants []services.RoleActionView `json:"grants"`
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := sx.Get(&n, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	grantsBefore := count("SELECT COUNT(*) FROM acl_role_actions")
	rolesBefore := count("SELECT COUNT(*) FROM acl_roles")
	listAs := func(tenant, builderID string) []role {
		t.Helper()
		tok, secret, err := mcpSvc.Issue(ctx, auth.Subject{UserID: builderID, TenantID: tenant, IsAdmin: true}, nil)
		if err != nil {
			t.Fatalf("issue %s: %v", tenant, err)
		}
		res, err := mcpConnect(t, ts.URL, secret).CallTool(ctx, &mcp.CallToolParams{Name: "nexus_list_roles", Arguments: map[string]any{"grants": true}})
		if err != nil || res.IsError {
			t.Fatalf("list_roles grants as %s: %v %s", tenant, err, mcpText(res))
		}
		var out struct {
			Result struct {
				Roles []role `json:"roles"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(mcpText(res)), &out); err != nil {
			t.Fatalf("decode %s: %v", mcpText(res), err)
		}
		if n := count("SELECT COUNT(*) FROM sys_ai_messages WHERE session_id = ? AND tenant_id = ?", tok.SessionID, tenant); n != 2 {
			t.Fatalf("%s thread rows = %d; want only the 2 of the key's opening turn", tenant, n)
		}
		return out.Result.Roles
	}

	gotA := listAs(tenantA, builderA)
	wantA := []role{{Name: "it_contacts_editor", Grants: []services.RoleActionView{{EntityName: "it_contacts", Action: "edit", AccessLevel: "owner"}, {EntityName: "it_contacts", Action: "view", AccessLevel: "all"}}}}
	if !reflect.DeepEqual(gotA, wantA) {
		t.Fatalf("tenant A's token sees %+v; want only its own role and grants %+v", gotA, wantA)
	}
	gotB := listAs(tenantB, builderB)
	if len(gotB) != 1 || gotB[0].Name != "it_b_contacts_admin" || len(gotB[0].Grants) != 2 {
		t.Fatalf("tenant B's token sees %+v; want only its own role with its 2 grants", gotB)
	}
	if g, r := count("SELECT COUNT(*) FROM acl_role_actions"), count("SELECT COUNT(*) FROM acl_roles"); g != grantsBefore || r != rolesBefore {
		t.Fatalf("after two reads: %d grants, %d roles; want %d and %d (a read writes nothing)", g, r, grantsBefore, rolesBefore)
	}
}
