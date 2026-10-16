package ai

// list_packs tests: the tool serves the SAME in-code registry GET /admin/verticals/builtin
// serves (vertical_pack.BuiltinInfos/BuiltinPack) — a static, tenant-free catalog for the
// Discovery PACK-FIRST step.

import (
	"context"
	"strings"
	"testing"
)

func TestListPacks_ServesBuiltinCatalogWithEntities(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	res := e.execute(context.Background(), "list_packs", `{}`)
	ok, out := parseToolResult(t, res)
	if !ok {
		t.Fatalf("list_packs expected ok, got %s", res)
	}
	result, _ := out["result"].(map[string]any)
	packs, _ := result["packs"].([]any)
	if len(packs) != 5 {
		t.Fatalf("expected the 5 shipped built-in packs, got %d: %s", len(packs), res)
	}

	slugs := make([]string, 0, len(packs))
	for _, p := range packs {
		row, _ := p.(map[string]any)
		slug, _ := row["slug"].(string)
		slugs = append(slugs, slug)
		if name, _ := row["name"].(string); name == "" {
			t.Errorf("pack %q missing manifest name", slug)
		}
		if version, _ := row["version"].(string); version == "" {
			t.Errorf("pack %q missing manifest version", slug)
		}
		ents, _ := row["entities"].([]any)
		if len(ents) == 0 {
			t.Errorf("pack %q must list its entities for blueprint matching", slug)
		}
		if cnt, _ := row["entityCount"].(float64); int(cnt) != len(ents) {
			t.Errorf("pack %q entityCount %v != len(entities) %d", slug, row["entityCount"], len(ents))
		}
	}
	// BuiltinInfos sorts by slug — the tool must preserve that stable order.
	if got := strings.Join(slugs, ","); got != "clinic,crm,lms,pm,ticketing" {
		t.Fatalf("expected sorted builtin slugs, got %s", got)
	}

	// The CRM pack must expose its entity names (what PACK-FIRST matches a blueprint against).
	if !strings.Contains(res, `"crm_accounts"`) {
		t.Fatalf("crm pack entities missing crm_accounts: %s", res)
	}
}
