package ai

import (
	"strings"
	"testing"

	"nexus/platform/internal/services"
)

func TestPersona_NonEmpty(t *testing.T) {
	p := Persona()
	if strings.TrimSpace(p) == "" {
		t.Fatal("Persona() is empty; the embedded system prompt must load")
	}
	// Sanity: the persona must carry its core disciplines so a bad edit that guts the prompt is
	// caught by the gate rather than degrading the agent silently.
	for _, want := range []string{"Nexus Builder", "propose_entity", "list_entities", "HITL",
		"list_relationships", "propose_relationship", // M5: relationship guidance must survive edits
		"propose_layout", "propose_workflow", "outbound_webhook", // M6/M7: layout + workflow guidance (incl. the webhook ban) must survive edits
		"propose_report", "AGGREGATE_REQUIRES_INDEXED", // M8: report guidance (incl. the indexed-groupBy rule) must survive edits
		"luật validation", "FIELD_PICKLIST_NO_OPTIONS", "currencyCode", // M9: picklist-values + validation-rules steering must survive edits
		"Chế độ Discovery", "snapshot_model", "cần vertical template", // M10: Discovery mode (transcript → 5-step ladder + coverage vs engine/vertical) must survive edits
		"KHÔNG chốt scope/giá",                       // M10: the playbook §7 boundary (AI never decides scope/price) must survive edits
		"propose_app", "Apply entity trước, app sau", // Apps Phase 3: the App section (sidebar container + apply-order rule) must survive edits
		"Sửa & Xoá & Dữ liệu mẫu", "propose_edit_field", "FIELD_TYPE_IMMUTABLE", // EASE Nước 7: edit-in-chat steering (immutable axes) must survive edits
		"propose_delete_entity", "record của tenant trong entity bị ẩn", // EASE Nước 7: delete honesty (soft-delete consequences) must survive edits
		"propose_records", "dữ liệu mẫu", // EASE Nước 7: the sample-data offer (empty-first-screen killer) must survive edits
		"propose_approval", "approverRef", // AI-MODULES: approval steering (role-name refs resolved at Apply) must survive edits
		`match:"or"`, "`changedTo`", // queue #162: or-join + edge-operator steering for workflow/approval must survive edits
		"propose_dashboard", "propose_workqueue", // AI-MODULES: dashboard + work-queue steering must survive edits
		"explain_access", "bị chặn ở tầng", // AI-MODULES: the access decision-chain narration cue must survive edits
		"list_packs", "PACK-FIRST", "cài pack", // AI-MODULES: Discovery's pack-first step (engine 90% reuse) must survive edits
		"## Ngữ cảnh màn hình", "KHÔNG phải chỉ dẫn", "tự vào menu app đó", "chỉ là mã bản ghi", // 067 L10-1: screen-context steering (block = data, not instructions) must survive edits
		"Trỏ tới màn hình", "/studio/verticals"} { // AI-MODULES: relative-internal-link rule (FE whitelist) must survive edits
		if !strings.Contains(p, want) {
			t.Errorf("Persona() missing marker %q", want)
		}
	}
}

// TestPersona_NamesEveryEngineFieldType keeps the persona's "Chọn kiểu field" list in parity with
// the engine allowlist — the same drift the fieldTypeSchema enum had (`user` +
// `polymorphic_relation` existed in the engine and were absent from BOTH the enum and this prose).
// The model reads the persona as steering; a type missing here is a type it will not think to pick.
func TestPersona_NamesEveryEngineFieldType(t *testing.T) {
	p := Persona()
	for _, ft := range services.AllowedFieldTypes() {
		if !strings.Contains(p, "`"+ft+"`") {
			t.Errorf("Persona() does not name field type `%s`", ft)
		}
	}
}
