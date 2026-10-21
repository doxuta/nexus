//go:build integration

// Real-MySQL half of tools_name_parity_test.go. The registry's collation (utf8mb4_unicode_ci:
// case-insensitive, trailing spaces ignored, a LEADING space not) is what let " name" miss the live
// table while the draft lookup found an older proposal of the same name (PR #39 verifier LOW). And
// propose_field's new refusal (#30 item 3) is the one the apply path gives: AddField refuses the same
// name with the same code.
//
// Run with: bash scripts/run-integration.sh -run TestToolsNameParity_IT ./internal/ai/
package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

func TestToolsNameParity_IT_LiveTableWinsAndADuplicateFieldIsRefusedLikeApply(t *testing.T) {
	repo, gdb := newRelITReader(t)
	sx := connectAI(t)
	ctx := context.Background()
	const tenant = "aiaiaiai-np30-4444-8888-tenant000303"
	const name = "it_ai_np30_cands"
	clean := func() {
		gdb.Exec(`DELETE FROM sys_fields WHERE tenant_id = ?`, tenant)
		gdb.Exec(`DELETE FROM sys_entities WHERE tenant_id = ?`, tenant)
		gdb.Exec(`DROP TABLE IF EXISTS obj_` + name)
	}
	clean()
	t.Cleanup(clean)

	if _, _, err := services.NewEntityService(dynrepo.New(sx), repo).CreateEntity(ctx, tenant, services.CreateEntityInput{
		Name: name, Label: "Ứng viên", Fields: []services.FieldInput{
			{Name: "full_name", Label: "Họ tên", FieldType: "text", IsRequired: true},
			{Name: "email", Label: "Email", FieldType: "email"},
		}}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	// An MCP thread replays an older proposal of the same name, shaped differently (nothing required).
	e := &toolExecutor{reader: repo, tenantID: tenant}
	e.draft = []DraftItem{{Kind: "entity", Entity: &DraftEntity{Name: name, Label: "Cũ",
		Fields: []DraftField{{Name: "email", Label: "Email", FieldType: "email"}}}}}
	for _, ref := range []string{name, " " + name, " " + name, strings.ToUpper(name) + " "} {
		args, _ := json.Marshal(map[string]any{"entity": ref, "rows": []any{map[string]any{"email": "a@b.vn"}}})
		mustReject(t, e, "propose_records", string(args), "RECORD_REQUIRED_MISSING")
	}

	// #30 item 3 on the real registry: a live name, and the same name twice in one draft.
	mustReject(t, e, "propose_field", `{"entity":"`+name+`","name":"email","label":"Email","fieldType":"email"}`, "FIELD_ALREADY_EXISTS")
	phone := `{"entity":"` + name + `","name":"phone","label":"SĐT","fieldType":"text"}`
	mustBuffer(t, e, "propose_field", phone)
	mustReject(t, e, "propose_field", phone, "FIELD_ALREADY_EXISTS")

	// Apply agrees: the first phone lands, a second one is refused with the code propose_field gave.
	f := e.draft[len(e.draft)-1].Field
	in := services.AddFieldInput{EntityName: f.Entity, Field: services.FieldInput{Name: f.Name, Label: f.Label, FieldType: f.FieldType}}
	fields := services.NewFieldService(dynrepo.New(sx), repo)
	if _, err := fields.AddField(ctx, tenant, in); err != nil {
		t.Fatalf("apply the first phone: %v", err)
	}
	_, err := fields.AddField(ctx, tenant, in)
	if code, _, _ := validationOf(err); code != "FIELD_ALREADY_EXISTS" {
		t.Fatalf("apply of a second phone = %v; want FIELD_ALREADY_EXISTS, the code propose_field gave", err)
	}
}
