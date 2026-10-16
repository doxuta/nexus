package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

func entItem(name string) DraftItem {
	return DraftItem{Kind: "entity", Entity: &DraftEntity{Name: name, Label: name}}
}

func appItem(nav ...string) DraftItem {
	return DraftItem{Kind: "app", App: &DraftApp{Name: "x", Label: "X", NavEntities: nav}}
}

func TestFinalizeDraft(t *testing.T) {
	inClinic := TurnContext{Present: true, App: &ContextApp{Name: "clinic"}}
	tests := []struct {
		name  string
		draft []DraftItem
		turn  TurnContext
		want  string // appName expected on entity "followups"
	}{
		{"app context stamps entity", []DraftItem{entItem("followups")}, inClinic, "clinic"},
		{"same-turn propose_app containing the entity => no stamp", []DraftItem{entItem("followups"), appItem("followups")}, inClinic, ""},
		{"propose_app NOT containing the entity => stamp", []DraftItem{entItem("followups"), appItem("other")}, inClinic, "clinic"},
		{"no app context => no stamp", []DraftItem{entItem("followups")}, TurnContext{Present: true, Panel: "bang"}, ""},
		{"no context at all", []DraftItem{entItem("followups")}, TurnContext{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := finalizeDraft(tc.draft, tc.turn)
			if g := got[0].Entity.AppName; g != tc.want {
				t.Fatalf("appName = %q; want %q", g, tc.want)
			}
		})
	}
}

func TestFinalizeDraft_AppNameSurvivesJSON(t *testing.T) {
	d := finalizeDraft([]DraftItem{entItem("followups")}, TurnContext{Present: true, App: &ContextApp{Name: "clinic"}})
	raw, _ := json.Marshal(d)
	if !strings.Contains(string(raw), `"appName":"clinic"`) {
		t.Fatalf("draft_json lacks appName: %s", raw)
	}
}

// Both terminal paths of the loop stamp the draft: final text and max_steps.
func TestAgent_StampsAppName_BothTerminalPaths(t *testing.T) {
	turn := TurnContext{Present: true, Project: "p", App: &ContextApp{Name: "clinic", Label: "C"}}
	args := `{"name":"followups","label":"Tái khám","fields":[{"name":"note","label":"Ghi chú","fieldType":"text"}]}`
	for name, steps := range map[string]int{"final text": 3, "max_steps": 1} {
		t.Run(name, func(t *testing.T) {
			resps := []ChatResponse{toolCallResp("c1", "propose_entity", args)}
			if steps > 1 {
				resps = append(resps, finalTextResp("xong"))
			}
			gw := &scriptedGateway{t: t, responses: resps}
			agent := NewAIBuilderAgent(enabledCfg(steps), gw, twoTenantReader(), nil, nil, nil)
			res, err := agent.ChatWithProgress(t.Context(), Scope{UserID: "u1"}, "tenant-A", "", turn,
				[]ChatMessage{{Role: "user", Content: "thêm bảng Tái khám"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Draft) != 1 || res.Draft[0].Entity == nil || res.Draft[0].Entity.AppName != "clinic" {
				t.Fatalf("outcome=%s draft=%+v; want appName clinic", res.Outcome, res.Draft)
			}
		})
	}
}
