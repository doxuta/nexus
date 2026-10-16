package vertical_pack

// specs/043 S0 bpm-node-ids T006: an approval process survives export → install WHOLE. Before
// this, PackApproval had no outcome at all, so a process that wrote "Chấp nhận" onto the quote
// exported as one that writes nothing — the customer's install approved quotes that never changed
// status. Step ids ride along too, so the installed process keeps the ids its own history and
// later versions refer to. A named-person approver stays non-portable (F6b — no email in a pack).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

func TestPackApprovals_RoundTripKeepsOutcomeAndIDs(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "ap_orders", Label: "Orders"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	steps := []bpm.Step{
		{ID: "truong-phong", Name: "Trưởng phòng", ApproverType: bpm.ApproverRole, ApproverID: "role-77", SLASeconds: 900},
		{ID: "s2", Name: "Giám đốc", ApproverType: bpm.ApproverRole, ApproverID: "role-77"},
	}
	outcome := bpm.Outcome{StatusField: "status", ApprovedValue: "Chấp nhận", RejectedValue: "Từ chối"}
	legacy := []bpm.Step{{ApproverType: bpm.ApproverRole, ApproverID: "role-77"}, {ApproverType: bpm.ApproverRole, ApproverID: "role-77"}}
	src := &fakeApprovalWriter{existing: []bpm.ProcessRecord{
		{ID: "p1", Name: "Duyệt báo giá", EntityName: "ap_orders", Trigger: "manual",
			Steps: mustJSON(t, steps), Outcome: mustJSON(t, outcome), IsActive: true},
		{ID: "p2", Name: "Quy trình cũ", EntityName: "ap_orders", Trigger: "after_create",
			Steps: mustJSON(t, legacy), IsActive: true},
	}}
	rd := fakeApRoleReader{roles: []struct{ id, name string }{{id: "role-77", name: "ap_manager"}}}
	exporter := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: rd, ApprovalWriter: src})

	pack, _, err := exporter.Export(ctx, "tenant-1", []string{"ap_orders"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Approvals) != 2 {
		t.Fatalf("exported approvals = %+v, want 2", pack.Approvals)
	}
	byName := map[string]PackApproval{}
	for _, a := range pack.Approvals {
		byName[a.Name] = a
	}
	got := byName["Duyệt báo giá"]
	if got.Outcome == nil || got.Outcome.StatusField != "status" || got.Outcome.ApprovedValue != "Chấp nhận" || got.Outcome.RejectedValue != "Từ chối" {
		t.Fatalf("exported outcome = %+v, want status/Chấp nhận/Từ chối (it was dropped on export)", got.Outcome)
	}
	if len(got.Steps) != 2 || got.Steps[0].ID != "truong-phong" || got.Steps[1].ID != "s2" {
		t.Fatalf("exported step ids = %+v, want [truong-phong s2]", got.Steps)
	}
	if old := byName["Quy trình cũ"]; old.Outcome != nil || len(old.Steps) != 2 || old.Steps[0].ID != "s1" || old.Steps[1].ID != "s2" {
		t.Fatalf("legacy process exports as %+v, want positional ids and no outcome", old)
	}

	// The pack as it travels (JSON) installs into a fresh tenant with outcome + ids intact.
	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}
	wire.Roles = []PackRole{{Name: "ap_manager", Label: "Manager"}}
	dst := &fakeApprovalWriter{}
	if _, err := newApprovalTestService(dst, nil).Install(ctx, "tenant-2", "user-2", &wire); err != nil {
		t.Fatalf("install: %v", err)
	}
	var in *services.CreateApprovalProcessInput
	for i := range dst.created {
		if dst.created[i].Name == "Duyệt báo giá" {
			in = &dst.created[i]
		}
	}
	if in == nil {
		t.Fatalf("installed = %+v, want Duyệt báo giá", dst.created)
	}
	if in.Outcome == nil || in.Outcome.StatusField != "status" || in.Outcome.ApprovedValue != "Chấp nhận" || in.Outcome.RejectedValue != "Từ chối" {
		t.Fatalf("installed outcome = %+v, want it carried into CreateProcess", in.Outcome)
	}
	if len(in.Steps) != 2 || in.Steps[0].ID != "truong-phong" || in.Steps[1].ID != "s2" || in.Steps[0].Name != "Trưởng phòng" {
		t.Fatalf("installed steps = %+v, want ids [truong-phong s2]", in.Steps)
	}
}

// TestPackReport_ColumnGroupRoundTrip (specs/049 S2b T033): a matrix report's column axis survives
// export → pack JSON → install. Without it the customer's install would get a one-axis summary
// under the name of a cross-tab — the pack key is added in the slice that introduces the config key.
func TestPackReport_ColumnGroupRoundTrip(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "rp_opps", Label: "Opps"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	src := &fakeReportWriter{existing: []report.Record{
		{ID: "r1", Name: "Giai đoạn × tháng", EntityName: "rp_opps", Type: "matrix", IsActive: true,
			Config: json.RawMessage(`{"groupBy":"stage","metric":"sum","field":"amount","columnGroup":{"field":"close_date","bucket":"month"}}`)},
		{ID: "r2", Name: "Ma trận cũ", EntityName: "rp_opps", Type: "matrix", IsActive: true,
			Config: json.RawMessage(`{"groupBy":"stage","metric":"count"}`)},
	}}
	exporter := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeApRoleReader{}, ReportWriter: src})
	pack, _, err := exporter.Export(ctx, "tenant-1", []string{"rp_opps"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}
	dst := &fakeReportWriter{}
	svc, _ := newReportTestService(dst)
	if _, err := svc.Install(ctx, "tenant-2", "user-2", &wire); err != nil {
		t.Fatalf("install: %v", err)
	}
	got := map[string]*services.ReportGroupInput{}
	for _, in := range dst.created {
		got[in.Name] = in.Config.ColumnGroup
	}
	if len(got) != 2 {
		t.Fatalf("installed reports = %+v, want both", dst.created)
	}
	if cg := got["Giai đoạn × tháng"]; cg == nil || *cg != (services.ReportGroupInput{Field: "close_date", Bucket: "month"}) {
		t.Fatalf("installed column axis = %+v, want close_date/month (dropped on export or install)", cg)
	}
	if cg := got["Ma trận cũ"]; cg != nil {
		t.Fatalf("legacy matrix installed with a column axis %+v, want none", cg)
	}
}

// TestPackReport_SubGroupsRoundTrip (specs/049 S3 T047): a summary report's sub-levels survive
// export → pack JSON → install, in order. Without it the customer's install would get a one-level
// summary under the name of a multi-level report.
func TestPackReport_SubGroupsRoundTrip(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "rp_opps", Label: "Opps"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	src := &fakeReportWriter{existing: []report.Record{
		{ID: "r1", Name: "Giai đoạn → tháng → người", EntityName: "rp_opps", Type: "summary", IsActive: true,
			Config: json.RawMessage(`{"groupBy":"stage","metric":"sum","field":"amount","subGroups":[{"field":"close_date","bucket":"month"},{"field":"ownerId"}]}`)},
		{ID: "r2", Name: "Một cấp", EntityName: "rp_opps", Type: "summary", IsActive: true,
			Config: json.RawMessage(`{"groupBy":"stage","metric":"count"}`)},
	}}
	exporter := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeApRoleReader{}, ReportWriter: src})
	pack, _, err := exporter.Export(ctx, "tenant-1", []string{"rp_opps"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}
	dst := &fakeReportWriter{}
	svc, _ := newReportTestService(dst)
	if _, err := svc.Install(ctx, "tenant-2", "user-2", &wire); err != nil {
		t.Fatalf("install: %v", err)
	}
	got := map[string][]services.ReportGroupInput{}
	for _, in := range dst.created {
		got[in.Name] = in.Config.SubGroups
	}
	if len(got) != 2 {
		t.Fatalf("installed reports = %+v, want both", dst.created)
	}
	want := []services.ReportGroupInput{{Field: "close_date", Bucket: "month"}, {Field: "ownerId"}}
	if sg := got["Giai đoạn → tháng → người"]; len(sg) != 2 || sg[0] != want[0] || sg[1] != want[1] {
		t.Fatalf("installed sub-levels = %+v, want %+v (dropped on export or install)", sg, want)
	}
	if sg := got["Một cấp"]; sg != nil {
		t.Fatalf("one-level summary installed with sub-levels %+v, want none", sg)
	}
}

// TestPackReport_HavingRoundTrip (specs/049 S5 T064): a report's threshold survives export → pack
// JSON → install. Without it the customer's install would total every group under the name of a
// report that keeps only some.
func TestPackReport_HavingRoundTrip(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "rp_opps", Label: "Opps"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	src := &fakeReportWriter{existing: []report.Record{
		{ID: "r1", Name: "Giai đoạn trên 10.000", EntityName: "rp_opps", Type: "summary", IsActive: true,
			Config: json.RawMessage(`{"groupBy":"stage","metric":"sum","field":"amount","having":{"on":"value","op":"between","values":["10000","50000.5"]}}`)},
		{ID: "r2", Name: "Không ngưỡng", EntityName: "rp_opps", Type: "summary", IsActive: true,
			Config: json.RawMessage(`{"groupBy":"stage","metric":"count"}`)},
	}}
	exporter := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeApRoleReader{}, ReportWriter: src})
	pack, _, err := exporter.Export(ctx, "tenant-1", []string{"rp_opps"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}
	dst := &fakeReportWriter{}
	svc, _ := newReportTestService(dst)
	if _, err := svc.Install(ctx, "tenant-2", "user-2", &wire); err != nil {
		t.Fatalf("install: %v", err)
	}
	got := map[string]*services.ReportHavingInput{}
	for _, in := range dst.created {
		got[in.Name] = in.Config.Having
	}
	if len(got) != 2 {
		t.Fatalf("installed reports = %+v, want both", dst.created)
	}
	if h := got["Giai đoạn trên 10.000"]; h == nil || h.On != "value" || h.Op != "between" ||
		len(h.Values) != 2 || h.Values[0] != "10000" || h.Values[1] != "50000.5" {
		t.Fatalf("installed threshold = %+v, want value between 10000 and 50000.5 (dropped on export or install)", h)
	}
	if h := got["Không ngưỡng"]; h != nil {
		t.Fatalf("report without a threshold installed with %+v, want none", h)
	}
}

// TestPackApprovals_UnsupportedKeysRefusedBeforeAnyWrite: a hand-authored (or newer-engine) pack
// whose approval carries a process/step key this engine does not run is refused (D-7) with the
// shared validator's code — BEFORE any write, by the same gate the dry-run runs. Refusing it only
// at Pass 8d (the approval writer) left entities, roles, samples and workflows committed and no
// app minted (late passes never roll back), while the dry-run still listed the process under
// approvalsToCreate. The keys are known once the pack is decoded, so the plan refuses them.
func TestPackApprovals_UnsupportedKeysRefusedBeforeAnyWrite(t *testing.T) {
	const tmpl = `{"entities":[{"name":"ap_orders","label":"Orders","fields":[{"name":"code","label":"Code","fieldType":"text"}]}],
		"roles":[{"name":"ap_manager","label":"Manager"}],
		"approvals":[{"name":"Ok","entityName":"ap_orders","trigger":"manual","steps":[{"approverType":"role","approverRef":"ap_manager"}]},
			{"name":"P","entityName":"ap_orders","trigger":"manual"%s,
			"steps":[{"approverType":"role","approverRef":"ap_manager"},{"approverType":"role","approverRef":"ap_manager"%s}]}]}`
	cases := []struct {
		name, procExtra, stepExtra string
		wantErr                    bool
		wantField                  string
	}{
		// Control: the same pack without the extra key installs — so "zero writes" below is not
		// a harness that could never write.
		{"supported shape installs", "", "", false, ""},
		{"process key", `,"graph":{"start":"a"}`, "", true, "approvals[1].graph"},
		{"step key", "", `,"next":"x"`, true, "approvals[1].steps[1].next"}, // approvers opened in S4, assignment in S6b; graph routing (next…) is not a pack shape
		{"both — the process key is reported", `,"options":{}`, `,"onReject":{}`, true, "approvals[1].options"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var p Pack
			if err := json.Unmarshal([]byte(fmt.Sprintf(tmpl, tc.procExtra, tc.stepExtra)), &p); err != nil {
				t.Fatalf("pack JSON: %v", err)
			}
			meta, roles, ap := &fakeMintMeta{}, &fakeApprovalRoleWriter{}, &fakeApprovalWriter{}
			svc := NewService(Deps{EntityReader: meta, RoleReader: fakeMintRoles{}, EntityWriter: meta, RoleWriter: roles, ApprovalWriter: ap})

			_, dryErr := svc.DryRunInstall(ctx, "tenant-1", &p)
			_, err := svc.Install(ctx, "tenant-1", "user-1", &p)
			ents, _ := meta.ListEntities("tenant-1")
			if !tc.wantErr {
				if dryErr != nil || err != nil || len(ents) != 1 || len(ap.created) != 2 {
					t.Fatalf("control: dry=%v install=%v entities=%d approvals=%d", dryErr, err, len(ents), len(ap.created))
				}
				return
			}
			for label, e := range map[string]error{"dry-run": dryErr, "install": err} {
				var ve *services.ValidationError
				if !errors.As(e, &ve) || ve.Code != "APPROVAL_NODE_KIND_UNSUPPORTED" || ve.Field != tc.wantField {
					t.Errorf("%s: err = %v (%+v); want APPROVAL_NODE_KIND_UNSUPPORTED at %s", label, e, ve, tc.wantField)
				}
			}
			if len(ents) != 0 || len(roles.existing) != 0 || len(ap.created) != 0 {
				t.Fatalf("refused pack still wrote: entities=%d roles=%d approvals=%d — half-installed",
					len(ents), len(roles.existing), len(ap.created))
			}
		})
	}
}

// TestPackApprovals_SelfTriggerRefusedBeforeAnyWrite (specs/043 S2b): a pack approval whose
// write-back lands on the value it starts on is refused by the SAME check the admin API runs —
// before entities or roles are written, for dry-run and install alike (the Pass 8d refusal alone
// would leave the tenant half-installed, the lesson of S0's shape gate).
func TestPackApprovals_SelfTriggerRefusedBeforeAnyWrite(t *testing.T) {
	const tmpl = `{"entities":[{"name":"ap_orders","label":"Orders","fields":[{"name":"status","label":"Status","fieldType":"text"}]}],
		"roles":[{"name":"ap_manager","label":"Manager"}],
		"approvals":[{"name":"P","entityName":"ap_orders","trigger":"after_update",
			"criteria":{"match":"or","rules":[{"field":"status","op":%q,"value":"Chờ duyệt"}]},
			"outcome":{"statusField":"status","approvedValue":%q},
			"steps":[{"approverType":"role","approverRef":"ap_manager"}]}]}`
	for _, tc := range []struct {
		name, op, approved string
		wantErr            bool
	}{
		{"distinct outcome installs", "changedTo", "Đã duyệt", false},
		{"outcome restarts the process", "changedTo", "Chờ duyệt", true},
		{"outcome satisfies a state rule", "eq", "Chờ duyệt", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var p Pack
			if err := json.Unmarshal([]byte(fmt.Sprintf(tmpl, tc.op, tc.approved)), &p); err != nil {
				t.Fatalf("pack JSON: %v", err)
			}
			meta, roles, ap := &fakeMintMeta{}, &fakeApprovalRoleWriter{}, &fakeApprovalWriter{}
			svc := NewService(Deps{EntityReader: meta, RoleReader: fakeMintRoles{}, EntityWriter: meta, RoleWriter: roles, ApprovalWriter: ap})
			_, dryErr := svc.DryRunInstall(ctx, "tenant-1", &p)
			_, err := svc.Install(ctx, "tenant-1", "user-1", &p)
			ents, _ := meta.ListEntities("tenant-1")
			if !tc.wantErr {
				if dryErr != nil || err != nil || len(ap.created) != 1 {
					t.Fatalf("control: dry=%v install=%v approvals=%d", dryErr, err, len(ap.created))
				}
				return
			}
			for label, e := range map[string]error{"dry-run": dryErr, "install": err} {
				var ve *services.ValidationError
				if !errors.As(e, &ve) || ve.Code != "APPROVAL_OUTCOME_SELF_TRIGGER" || ve.Field != "approvals[0].outcome.approvedValue" {
					t.Errorf("%s: err = %v (%+v); want APPROVAL_OUTCOME_SELF_TRIGGER at approvals[0].outcome.approvedValue", label, e, ve)
				}
			}
			if len(ents) != 0 || len(roles.existing) != 0 || len(ap.created) != 0 {
				t.Fatalf("refused pack still wrote: entities=%d roles=%d approvals=%d", len(ents), len(roles.existing), len(ap.created))
			}
		})
	}
}

// TestComparePackItems_ApprovalStepIDsCanonicalized: a pack installed before step ids existed
// (provenance has none) must not read as "modified" against the live export, which now carries
// the positional ids the engine assigned — but a real id change still surfaces.
func TestComparePackItems_ApprovalStepIDsCanonicalized(t *testing.T) {
	packSide := map[string]any{"name": "P", "entityName": "e", "trigger": "manual", "isActive": true,
		"steps": []any{map[string]any{"approverType": "role", "approverRef": "m"}, map[string]any{"approverType": "role", "approverRef": "n"}}}
	live := map[string]any{"name": "P", "entityName": "e", "trigger": "manual", "isActive": true,
		"steps": []any{map[string]any{"id": "s1", "approverType": "role", "approverRef": "m"}, map[string]any{"id": "s2", "approverType": "role", "approverRef": "n"}}}
	paths, err := comparePackItems(ItemApproval, mustRaw(t, packSide), mustRaw(t, live))
	if err != nil || len(paths) != 0 {
		t.Fatalf("positional ids must canonicalize equal, got %v err %v", paths, err)
	}
	live["steps"].([]any)[1].(map[string]any)["id"] = "giam-doc"
	if paths, err = comparePackItems(ItemApproval, mustRaw(t, packSide), mustRaw(t, live)); err != nil || len(paths) == 0 {
		t.Fatalf("a real id change must surface, got %v err %v", paths, err)
	}
}
