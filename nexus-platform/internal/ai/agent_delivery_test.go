package ai

// agent_delivery_test.go — specs/067 T084 (L10-5). Once a project is delivered, edits to rules/reports/
// screens/menus go into the DRAFT, new tables/fields stay hidden from the customer until Publish, and
// the AI must stop volunteering sample data. The agent says so in ONE grounding line.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeDelivery struct {
	delivered bool
	err       error
	gotTenant string
}

func (f *fakeDelivery) Delivered(_ context.Context, tenantID string) (bool, error) {
	f.gotTenant = tenantID
	return f.delivered, f.err
}

func groundingFor(t *testing.T, d DeliveryChecker) string {
	t.Helper()
	gw := &scriptedGateway{t: t, responses: []ChatResponse{finalTextResp("ok")}}
	agent := NewAIBuilderAgent(enabledCfg(3), gw, twoTenantReader(), nil, nil, nil)
	if d != nil {
		agent = agent.WithDelivery(d)
	}
	if _, err := agent.Chat(context.Background(), Scope{UserID: "u1"}, "tenant-A", "", []ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	return gw.calls[0][1].Content
}

func TestAgent_DeliveredGroundingLine(t *testing.T) {
	cases := []struct {
		name string
		d    DeliveryChecker
		want int
	}{
		{"delivered => exactly one line", &fakeDelivery{delivered: true}, 1},
		{"not delivered => none", &fakeDelivery{delivered: false}, 0},
		{"no checker wired => none", nil, 0},
		{"lookup error => treated as delivered (cautious)", &fakeDelivery{err: errors.New("db down")}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := groundingFor(t, tc.d)
			if n := strings.Count(g, "Dự án ĐÃ GIAO"); n != tc.want {
				t.Fatalf("grounding has %d delivered lines, want %d: %.200s", n, tc.want, g)
			}
			if f, ok := tc.d.(*fakeDelivery); ok && f.gotTenant != "tenant-A" {
				t.Fatalf("checker asked about tenant %q, want tenant-A", f.gotTenant)
			}
		})
	}
}

func TestPersona_SampleDataOnlyBeforeDelivery(t *testing.T) {
	if !strings.Contains(Persona(), "chỉ khi dự án chưa giao") {
		t.Fatal("persona must condition the proactive sample-data offer on 'chỉ khi dự án chưa giao'")
	}
}
