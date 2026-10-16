package vertical_pack

// DAILY-QUEUE #720 (2/4 of #295): a report with a join / sort used to be DROPPED by export (warning
// only). It now travels: export -> pack JSON -> toReportInput, so Sao Mai keeps its two reports.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

func TestPackReport_JoinsAndSortRoundTrip(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "rp_orders", Label: "Orders"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	cases := []struct {
		name string
		cfg  string
	}{
		{"summary with optional join", `{"groupBy":"customer.region","metric":"count","joins":[{"relationship":"customer","optional":true}]}`},
		{"tabular with sort", `{"columns":["name"],"sort":{"field":"name","dir":"asc"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ := "summary"
			if tc.name == "tabular with sort" {
				typ = "tabular"
			}
			src := &fakeReportWriter{existing: []report.Record{{
				ID: "r1", TenantID: "tenant-1", Name: "R", EntityName: "rp_orders", Type: typ,
				Config: json.RawMessage(tc.cfg), IsActive: true, CreatedAt: time.Now(),
			}}}
			pack, warns, err := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}, ReportWriter: src}).
				Export(ctx, "tenant-1", []string{"rp_orders"}, false, nil)
			if err != nil || len(pack.Reports) != 1 {
				t.Fatalf("export: err=%v reports=%d warnings=%v", err, len(pack.Reports), warns)
			}
			var wire Pack
			if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
				t.Fatalf("pack JSON: %v", err)
			}
			in := toReportInput(wire.Reports[0], "u")
			var want services.ReportConfig
			if err := json.Unmarshal([]byte(tc.cfg), &want); err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(in.Config)
			exp, _ := json.Marshal(want)
			if string(got) != string(exp) {
				t.Fatalf("install config = %s, want %s", got, exp)
			}
		})
	}
}
