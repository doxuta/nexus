//go:build integration

// Issue #30 item 4 on a real MySQL. Asked to delete a pack table "now, without asking again", the agent's turn
// deletes nothing: it drafts a delete card that names the pack (sys_pack_items, through the ledger repository the
// installer writes with) and counts the tenant's records (another tenant's rows in the shared obj_ table are not
// counted), and the tool result tells the model a human must confirm. The count is on the card only: the model is
// never told it (ADR 0003). The table and its records stay until a human
// applies the card, which runs EntityService.SoftDeleteEntity (DELETE /admin/entities/:name, after the Studio card's
// retype of the table name).
//
// Run with: bash scripts/run-integration.sh -run TestProposeDeleteEntity_IT ./internal/ai/
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/packinstall"
	"nexus/platform/internal/services"
	"nexus/platform/internal/services/vertical_pack"
)

func TestProposeDeleteEntity_IT_APackTableIsProposedNotDeleted(t *testing.T) {
	repo, gdb := newRelITReader(t)
	sx := connectAI(t)
	ctx := context.Background()
	const tenantA = "aiaiaiai-pd30-4444-8888-tenant0000a4"
	const tenantB = "aiaiaiai-pd30-4444-8888-tenant0000b4"
	const name = "it_ai_pd30_accounts"
	clean := func() {
		for _, q := range []string{
			`DELETE FROM sys_pack_items WHERE tenant_id IN (?, ?)`, `DELETE FROM sys_relationships WHERE tenant_id IN (?, ?)`,
			`DELETE FROM sys_fields WHERE tenant_id IN (?, ?)`, `DELETE FROM sys_entities WHERE tenant_id IN (?, ?)`,
		} {
			gdb.Exec(q, tenantA, tenantB)
		}
		gdb.Exec(`DROP TABLE IF EXISTS obj_` + name)
	}
	clean()
	t.Cleanup(clean)

	dyn := dynrepo.New(sx)
	entities := services.NewEntityService(dyn, repo)
	if _, _, err := entities.CreateEntity(ctx, tenantA, services.CreateEntityInput{Name: name, Label: "Khách hàng",
		Fields: []services.FieldInput{{Name: "full_name", Label: "Tên", FieldType: "text"}}}); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	// Three of tenant A's customers, and two of tenant B's in the same shared physical table.
	for i, tenant := range []string{tenantA, tenantA, tenantA, tenantB, tenantB} {
		if _, err := dyn.InsertRecord(ctx, name, tenant, "u-seed", map[string]any{"full_name": "Khách " + string(rune('A'+i))}, nil); err != nil {
			t.Fatalf("seed record: %v", err)
		}
	}
	// Tenant A's CRM pack shipped the table; tenant B's ticketing pack ships a table of the same name.
	ledger := packinstall.New(gdb)
	for tenant, slug := range map[string]string{tenantA: "crm", tenantB: "ticketing"} {
		if err := ledger.SaveItems(ctx, tenant, slug, "1.0.0", []vertical_pack.PackItem{
			{Type: vertical_pack.ItemEntity, Key: name, Content: json.RawMessage(`{}`), Wrote: true},
		}); err != nil {
			t.Fatalf("seed provenance: %v", err)
		}
	}
	countA := func() int64 {
		t.Helper()
		n, err := dyn.AggregateScalar(ctx, name, tenantA, dynrepo.AdminScope(), dynrepo.AggregateParams{Metric: dynrepo.MetricCount})
		if err != nil {
			t.Fatal(err)
		}
		return n.Count
	}

	gw := &scriptedGateway{t: t, responses: []ChatResponse{
		toolCallResp("call_1", "propose_delete_entity", `{"entity":"`+name+`"}`),
		finalTextResp("Tôi chỉ soạn được thẻ xoá: anh phải tự bấm Xoá và gõ lại tên bảng, tôi không bỏ qua được bước xác nhận."),
	}}
	agent := NewAIBuilderAgent(enabledCfg(4), gw, ReaderWithAccessProbes(repo, &AccessProbes{Packs: ledger, Counts: dyn}), nil, nil, nil)
	res, err := agent.ChatWithProgress(ctx, Scope{UserID: "u-builder", IsAdmin: true}, tenantA, "", TurnContext{},
		[]ChatMessage{{Role: "user", Content: "Xoá hẳn bảng " + name + " cùng toàn bộ dữ liệu khách hàng ngay bây giờ, không cần hỏi lại."}}, nil)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}

	// The card: the pack of THIS tenant and THIS tenant's three records.
	if len(res.Draft) != 1 || res.Draft[0].DeleteEntity == nil {
		t.Fatalf("draft = %+v; want one delete_entity card", res.Draft)
	}
	de := res.Draft[0].DeleteEntity
	if strings.Join(de.Packs, ",") != "crm" || de.RecordCount == nil || *de.RecordCount != 3 {
		t.Fatalf("card = %+v (records %v); want pack crm and 3 records", de, de.RecordCount)
	}
	// What the model read before it answered: the pack, that it cannot skip the confirmation, and no record count.
	sent := gw.calls[1][len(gw.calls[1])-1]
	if sent.Role != "tool" || strings.Contains(sent.Content, "recordCount") || !strings.Contains(sent.Content, `"packs":["crm"]`) ||
		!strings.Contains(sent.Content, "không bỏ qua được bước xác nhận") {
		t.Fatalf("tool result sent to the model = %s", sent.Content)
	}

	// The turn deleted nothing.
	if _, err := repo.GetEntityByName(ctx, tenantA, name); err != nil {
		t.Fatalf("the table is gone after the turn: %v", err)
	}
	if n := countA(); n != 3 {
		t.Fatalf("%d records after the turn; want all 3", n)
	}

	// The human applies the card: only then is the table deleted and its records hidden, and tenant B keeps its own.
	if err := entities.SoftDeleteEntity(ctx, tenantA, de.Entity); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := repo.GetEntityByName(ctx, tenantA, name); !errors.Is(err, metadata.ErrEntityNotFound) {
		t.Fatalf("after the human's delete the table still resolves: %v", err)
	}
	if n := countA(); n != 0 {
		t.Fatalf("%d records live after the delete; want 0", n)
	}
	if n, err := dyn.AggregateScalar(ctx, name, tenantB, dynrepo.AdminScope(), dynrepo.AggregateParams{Metric: dynrepo.MetricCount}); err != nil || n.Count != 2 {
		t.Fatalf("tenant B has %d records (%v); want its 2", n.Count, err)
	}
}
