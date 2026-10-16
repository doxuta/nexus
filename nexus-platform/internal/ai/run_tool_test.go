package ai

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/config"
)

// RunTool is the builder agent's tool loop without the loop (issue #3, the MCP surface): one tool,
// the same tenant-scoped executor and validators, and no gateway — so it works with AI_ENABLED=false.
func TestRunTool(t *testing.T) {
	agent := NewAIBuilderAgent(config.AIConfig{Enabled: false}, nil, twoTenantReader(), nil, nil, nil)
	ctx := context.Background()

	t.Run("a read tool is pinned to the tenant and proposes nothing", func(t *testing.T) {
		res, added := agent.RunTool(ctx, "tenant-A", "sess-1", nil, "list_entities", "{}")
		if !strings.Contains(res, "patients") || strings.Contains(res, "invoices") || len(added) != 0 {
			t.Fatalf("got %s, %d proposals; want tenant A's tables only and no proposal", res, len(added))
		}
	})

	t.Run("a proposal comes back with its change envelope", func(t *testing.T) {
		res, added := agent.RunTool(ctx, "tenant-A", "sess-1", nil, "propose_entity",
			`{"name":"suppliers","label":"Nhà cung cấp","fields":[{"name":"name","label":"Tên","fieldType":"text","required":true}]}`)
		if ok, _ := parseToolResult(t, res); !ok || len(added) != 1 {
			t.Fatalf("got %s, %d proposals; want one", res, len(added))
		}
		if it := added[0]; it.Kind != "entity" || it.Entity.Name != "suppliers" || it.Change == nil || it.Change.Op != "create" {
			t.Fatalf("proposal = %+v; want the entity with its create envelope", it)
		}
	})

	t.Run("an earlier proposal of the thread counts as proposed this turn", func(t *testing.T) {
		_, prior := agent.RunTool(ctx, "tenant-A", "sess-1", nil, "propose_entity",
			`{"name":"suppliers","label":"Nhà cung cấp","fields":[{"name":"name","label":"Tên","fieldType":"text"}]}`)
		rows := `{"entity":"suppliers","rows":[{"name":"Acme","$ref":"@acme"}]}`
		if res, added := agent.RunTool(ctx, "tenant-A", "sess-1", nil, "propose_records", rows); len(added) != 0 || !strings.Contains(res, "ENTITY_NOT_FOUND") {
			t.Fatalf("without the thread's proposals: %s; want ENTITY_NOT_FOUND", res)
		}
		res, added := agent.RunTool(ctx, "tenant-A", "sess-1", prior, "propose_records", rows)
		if len(added) != 1 || added[0].Kind != "records" {
			t.Fatalf("with them: %s, %+v; want only the new records proposal back", res, added)
		}
		if ref := added[0].Records.Rows[0]["$ref"]; ref != "@sess-1:acme" {
			t.Fatalf("$ref = %v; want it namespaced by the thread tag so a later call of the thread resolves it", ref)
		}
	})
}
