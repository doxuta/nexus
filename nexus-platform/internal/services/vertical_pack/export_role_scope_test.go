package vertical_pack

import (
	"testing"

	"nexus/platform/internal/auth"
)

// MỘT XƯỞNG RỔ-4 P0 (2026-07-29): Export used to ship EVERY role in the tenant with its FULL grant
// list — the old comment said so out loud ("export every role in the tenant"). Nexus builds many
// customers' apps inside ONE tenant, so that tenant accumulates a role per engagement and every
// handover recreated ALL of them inside the new customer's workspace. The wizard scopes by ENTITY,
// so the operator reasonably believes the whole bundle is scoped. Pin the scoping rule.
func TestScopeRoleActions(t *testing.T) {
	selected := map[string]bool{"crm_accounts": true, "crm_contacts": true}

	t.Run("bỏ grant trỏ tới entity ngoài phạm vi", func(t *testing.T) {
		kept, skipped := scopeRoleActions([]auth.RoleAction{
			{EntityName: "crm_accounts", Action: "view", AccessLevel: "all"},
			{EntityName: "lms_students", Action: "view", AccessLevel: "all"}, // khách KHÁC
			{EntityName: "crm_contacts", Action: "edit", AccessLevel: "owner"},
		}, selected)
		if len(kept) != 2 || skipped != 1 {
			t.Fatalf("kept=%d skipped=%d; want kept=2 skipped=1 (%v)", len(kept), skipped, kept)
		}
		for _, k := range kept {
			if k.EntityName == "lms_students" {
				t.Fatal("grant của entity ngoài phạm vi đã lọt sang workspace khách")
			}
		}
	})

	t.Run("giữ wildcard", func(t *testing.T) {
		kept, skipped := scopeRoleActions([]auth.RoleAction{
			{EntityName: "*", Action: "view", AccessLevel: "all"},
		}, selected)
		if len(kept) != 1 || skipped != 0 {
			t.Fatalf("wildcard phải được giữ; kept=%d skipped=%d", len(kept), skipped)
		}
	})

	t.Run("vai toàn grant ngoài phạm vi trả rỗng để caller bỏ hẳn vai", func(t *testing.T) {
		kept, skipped := scopeRoleActions([]auth.RoleAction{
			{EntityName: "tk_tickets", Action: "view", AccessLevel: "all"},
			{EntityName: "clinic_patients", Action: "edit", AccessLevel: "all"},
		}, selected)
		if len(kept) != 0 || skipped != 2 {
			t.Fatalf("kept=%d skipped=%d; want kept=0 skipped=2", len(kept), skipped)
		}
	})

	t.Run("selected rỗng thì chỉ wildcard sống sót", func(t *testing.T) {
		kept, _ := scopeRoleActions([]auth.RoleAction{
			{EntityName: "*", Action: "view", AccessLevel: "all"},
			{EntityName: "crm_accounts", Action: "view", AccessLevel: "all"},
		}, map[string]bool{})
		if len(kept) != 1 || kept[0].EntityName != "*" {
			t.Fatalf("kept=%v; want chỉ wildcard", kept)
		}
	})
}
