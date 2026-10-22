package ai

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
		"propose_access", "ai sẽ dùng", // issue #2: "the user says who will use it" → propose_access must survive edits
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

// TestPersonas_FollowTheUsersLanguage (issue #6): the builder and the end-user assistant answer, and the builder
// labels what it proposes, in the language the person writes in; neither prompt pins Vietnamese any more. Both
// prompts stay in Vietnamese; only the rule changed. The cassette fingerprint leaves system messages out, so this
// edit does not make a recorded demo stale (TestFingerprint_ToolsPromptsAndShapeNotFreshIDs). What a model did with
// the rule is checked on a recording: TestEnglishDemoCassette_ProposesInEnglish.
func TestPersonas_FollowTheUsersLanguage(t *testing.T) {
	// The rule itself, as the builder reads it: which words follow the user's language, both languages, the
	// default, and what is never translated.
	builder := Persona()
	rule := ""
	for _, line := range strings.Split(builder, "\n") {
		if strings.HasPrefix(line, "- **Theo ngôn ngữ người dùng.**") {
			rule = line
		}
	}
	if rule == "" {
		t.Fatal("builder persona has no \"Theo ngôn ngữ người dùng\" rule")
	}
	for _, want := range []string{"Trả lời", "`label`", "`valueLabels`", "tên app", "`title` section", "dữ liệu mẫu",
		"tiếng Việt có dấu nếu họ viết tiếng Việt", "tiếng Anh nếu họ viết tiếng Anh", "không rõ thì tiếng Việt",
		"`name` là snake_case không dấu", "`options.values`"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the builder's language rule does not say %q:\n%s", want, rule)
		}
	}
	// Its own picklist example follows it: codes in values, the words in valueLabels.
	if !strings.Contains(builder, `options:{values:["new","in_progress","done","cancelled"], valueLabels:{`) {
		t.Error("the builder's picklist example does not keep codes in values and words in valueLabels")
	}
	if !strings.Contains(AssistantPersona(), "Trả lời bằng **ngôn ngữ người dùng** (tiếng Việt nếu họ hỏi tiếng Việt, tiếng Anh nếu họ hỏi tiếng Anh)") {
		t.Error("assistant persona has no rule to answer in the user's language")
	}
	for name, p := range map[string]string{"builder": builder, "assistant": AssistantPersona()} {
		for _, pinned := range []string{"nói tiếng Việt", "Trả lời tiếng Việt", "Trả lời **tiếng Việt**", "nhãn tiếng Việt", "`title` tiếng Việt", "record tiếng Việt"} {
			if strings.Contains(p, pinned) {
				t.Errorf("%s persona still pins Vietnamese: %q", name, pinned)
			}
		}
	}
	// The tool schemas steer labels too: none may ask for a Vietnamese label or Vietnamese sample data.
	raw, err := json.Marshal(ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	for _, pinned := range []string{"hiển thị tiếng Việt", "nhãn tiếng Việt", "Dữ liệu tiếng Việt", "(tiếng Việt có dấu)"} {
		if strings.Contains(string(raw), pinned) {
			t.Errorf("a tool description still pins Vietnamese: %q", pinned)
		}
	}
}

// TestEnglishDemoCassette_ProposesInEnglish (issue #6): what a model did with the language rule. The English demo
// cassette (cassettes/demo-loop-en.jsonl, recorded once against a live model with this persona and replayed by CI's
// demo-loop-en job) holds an English prompt; every proposal it recorded names things in English (picklist codes in
// values, English words in valueLabels) and the reply is English. A Vietnamese letter in any of them fails.
func TestEnglishDemoCassette_ProposesInEnglish(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "cassettes", "demo-loop-en.jsonl"))
	if err != nil {
		t.Fatalf("read the English demo cassette: %v", err)
	}
	vietnamese := regexp.MustCompile(`(?i)[àáạảãâầấậẩẫăằắặẳẵèéẹẻẽêềếệểễìíịỉĩòóọỏõôồốộổỗơờớợởỡùúụủũưừứựửữỳýỵỷỹđ]`)
	var proposals, replies int
	var labels []string
	for i, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var e cassetteEntry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("cassette line %d: %v", i+1, err)
		}
		for _, m := range e.Request.Messages {
			if m.Role == "user" && vietnamese.MatchString(m.Content) {
				t.Errorf("line %d: the prompt is not English: %q", i+1, m.Content)
			}
		}
		for _, c := range e.Response.Choices {
			for _, tc := range c.Message.ToolCalls {
				if !strings.HasPrefix(tc.Function.Name, "propose_") {
					continue
				}
				proposals++
				if vietnamese.MatchString(tc.Function.Arguments) {
					t.Errorf("line %d: %s proposes Vietnamese text: %s", i+1, tc.Function.Name, tc.Function.Arguments)
				}
				var args struct {
					Label  string `json:"label"`
					Fields []struct {
						Label string `json:"label"`
					} `json:"fields"`
				}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				labels = append(labels, args.Label)
				for _, f := range args.Fields {
					labels = append(labels, f.Label)
				}
			}
			if strings.TrimSpace(c.Message.Content) != "" {
				replies++
				if vietnamese.MatchString(c.Message.Content) {
					t.Errorf("line %d: the reply is not English: %s", i+1, c.Message.Content)
				}
			}
		}
	}
	if proposals < 3 || replies == 0 {
		t.Fatalf("the cassette holds %d proposals and %d replies; want the table, the access and the app, and a reply", proposals, replies)
	}
	for _, want := range []string{"Tickets", "Title", "Priority", "Status", "Helpdesk"} {
		if !slices.Contains(labels, want) {
			t.Errorf("no proposal is labelled %q in English: %q", want, labels)
		}
	}
}
