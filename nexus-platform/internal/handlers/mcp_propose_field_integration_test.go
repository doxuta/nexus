//go:build integration

// Issue #22 item 1 over a real /mcp on real MySQL: propose_field judges a dependent picklist against the
// builder's DRAFT of its parent (a delivered project, read the way cmd/server/main.go's aiDraftFieldReader
// reads fields), and files no card for a table that is not in the token's tenant.
//
// Run: go test -tags=integration ./internal/handlers/ -run TestMCP_ProposeField
package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/mcpserver"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
)

// draftFieldReader is cmd/server/main.go's aiDraftFieldReader: the registry, with field lists read
// through the draft layer.
type draftFieldReader struct {
	*metadata.Repository
	fields *services.DraftFieldRepo
}

func (r draftFieldReader) ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	return r.fields.ListFields(ctx, tenantID, entityID)
}

func TestMCP_ProposeFieldJudgesTheBuildersDraftAndItsOwnTenant(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	ctx := context.Background()
	const tenant, other = "ten-mcp-pfield", "ten-mcp-pfother"
	builderID := mcpBuilder(t, gdb, tenant, "mcp-pfield")
	a4SeedTenant(t, gdb, other, "mcp-pfother", "Other")
	entID, statusID := uuid.NewString(), uuid.NewString()
	status := `{"values":["open","closed"]}`
	for _, row := range []any{
		&metadata.EntityDefinition{ID: entID, TenantID: tenant, Name: "pf_tickets", Label: "Tickets", DataTable: "obj_pf_tickets"},
		&metadata.FieldDefinition{ID: statusID, EntityID: entID, TenantID: tenant, Name: "status", Label: "Status", FieldType: "picklist", Options: &status, FieldOrder: 10},
		// A table of ANOTHER tenant: its name must read as missing to this token.
		&metadata.EntityDefinition{ID: uuid.NewString(), TenantID: other, Name: "pf_invoices", Label: "Invoices", DataTable: "obj_pf_invoices"},
	} {
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Deliver the project, then the builder adds "hold" to status: a draft, not yet the customer's.
	meta := metadata.NewRepository(gdb)
	reg := metaversion.NewRegistry(services.NewFieldDocAdapter())
	drafts := metaversion.NewDraftRepo(sx, reg)
	vers := metaversion.NewVersionRepo(sx)
	gate := services.NewDraftGate(vers)
	fieldDrafts := services.NewDraftFieldRepo(meta, drafts, gate)
	builderCtx := metaversion.WithCaller(ctx, metaversion.Caller{UserID: builderID, TenantID: tenant, HomeTenantID: tenant, IsBuilder: true, ReadDraft: true})
	if _, err := services.NewPublishService(sx, drafts, vers, reg, gate, nil, nil).Baseline(builderCtx, tenant, "first"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	fieldSvc := services.NewFieldService(dynrepo.New(sx), meta).WithDraftFields(fieldDrafts)
	if _, err := fieldSvc.EditField(builderCtx, tenant, services.EditFieldInput{FieldID: statusID,
		Options: map[string]any{"values": []any{"open", "closed", "hold"}}}); err != nil {
		t.Fatalf("the builder's draft edit: %v", err)
	}
	var live string
	if err := sx.Get(&live, "SELECT CAST(options AS CHAR) FROM sys_fields WHERE id = ?", statusID); err != nil || strings.Contains(live, "hold") {
		t.Fatalf("published status = %s (%v); the edit must be a draft", live, err)
	}

	agent := ai.NewAIBuilderAgent(config.AIConfig{}, nil, draftFieldReader{meta, fieldDrafts}, nil, nil, zap.NewNop())
	store := ai.NewSessionStore(sx)
	mcpSvc := services.NewMCPTokenService(auth.NewMCPTokenRepository(gdb), store, auth.NewUserRepository(gdb))
	_, secret, err := mcpSvc.Issue(ctx, auth.Subject{UserID: builderID, TenantID: tenant, IsAdmin: true}, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	ts := httptest.NewServer(mcpserver.New(mcpSvc, agent, store, nil, zap.NewNop()))
	defer ts.Close()
	cs := mcpConnect(t, ts.URL, secret)

	// propose answers "" when the card landed, else the validator's "CODE: message".
	propose := func(t *testing.T, args map[string]any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "nexus_propose_field", Arguments: args})
		if err != nil {
			t.Fatalf("propose_field: %v", err)
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

	t.Run("a dependent value keyed on the builder's draft value is accepted", func(t *testing.T) {
		if got := propose(t, map[string]any{"entity": "pf_tickets", "name": "hold_reason", "label": "Hold reason", "fieldType": "picklist",
			"options": map[string]any{"values": []string{"parts", "other"}, "controllingField": "status", "dependentValues": map[string]any{"hold": []string{"parts"}}}}); got != "" {
			t.Fatalf("keyed on the draft value hold: %q; want the card (the builder's model has hold)", got)
		}
	})

	t.Run("a table not in the token's tenant gets no card", func(t *testing.T) {
		for _, entity := range []string{"pf_invoices", "pf_ghosts"} {
			if got := propose(t, map[string]any{"entity": entity, "name": "note", "label": "Note", "fieldType": "text"}); !strings.HasPrefix(got, "ENTITY_NOT_FOUND") {
				t.Errorf("propose_field on %s: %q; want ENTITY_NOT_FOUND", entity, got)
			}
		}
		var cards int
		if err := sx.Get(&cards, `SELECT COUNT(*) FROM sys_ai_messages WHERE tenant_id = ? AND draft_json IS NOT NULL`, tenant); err != nil || cards != 1 {
			t.Fatalf("cards in the thread = %d (%v); want only the accepted one", cards, err)
		}
	})
}
