package vertical_pack

// specs/048 S1a (T011): a before_save veto rule crosses export → JSON → install intact. The pack DTOs
// are explicit structs, so a key they do not declare (condition.saveOn) vanishes in transit and the
// installed rule blocks creates as well as updates — the "đoạn nối" class, caught here instead of
// at the customer.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

func TestPackWorkflow_BeforeSaveRoundTrip(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "crm_opportunities", Label: "Cơ hội"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	src := &fakeWorkflowWriter{existing: []workflow.WorkflowRecord{{
		ID: "wf-1", TenantID: "tenant-1", Name: "Chốt Thắng cần Ngày chốt", EntityName: "crm_opportunities",
		Trigger:   "before_save",
		Condition: json.RawMessage(`{"match":"and","saveOn":"update","rules":[{"field":"stage","op":"changedTo","value":"Closed Won"},{"field":"close_date","op":"isnull"}]}`),
		Actions:   json.RawMessage(`[{"type":"veto","field":"close_date","message":"Chốt Thắng phải có Ngày chốt"}]`),
		IsActive:  false,
	}}}
	pack, warnings, err := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}, WorkflowWriter: src}).
		Export(ctx, "tenant-1", []string{"crm_opportunities"}, false, nil)
	if err != nil || len(pack.Workflows) != 1 {
		t.Fatalf("export: %v %+v", err, pack)
	}
	for _, w := range warnings {
		if strings.Contains(w, "veto") {
			t.Fatalf("veto is a known action — no 'unknown action' warning expected, got %q", w)
		}
	}

	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}
	dst := &fakeWorkflowWriter{}
	svc, _ := newWorkflowTestService(dst)
	if _, err := svc.Install(ctx, "tenant-2", "user-2", &wire); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(dst.created) != 1 {
		t.Fatalf("CreateWorkflow calls = %d, want 1", len(dst.created))
	}
	in := dst.created[0]
	if in.Trigger != "before_save" || in.SaveOn != "update" || len(in.Rules) != 2 {
		t.Fatalf("installed rule = trigger %q saveOn %q rules %+v — saveOn lost in transit?", in.Trigger, in.SaveOn, in.Rules)
	}
	if in.IsActive == nil || *in.IsActive {
		t.Fatalf("installed rule must stay switched off, got %v", in.IsActive)
	}
	if len(in.Actions) != 1 || in.Actions[0].Type != "veto" || in.Actions[0].Field != "close_date" ||
		in.Actions[0].Message != "Chốt Thắng phải có Ngày chốt" {
		t.Fatalf("installed actions = %+v", in.Actions)
	}
}

// S3: every before rule runs, so a pack may ship one switched ON — it arrives on, not refused. Until
// S3 the pack installer refused it before any write (the R13 gate, now deleted).
func TestPackInstall_ActiveBeforeRuleInstallsOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active *bool
	}{{"isActive true", boolPtr(true)}, {"isActive omitted (defaults to on)", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			pack := workflowTestPack()
			pack.Workflows = append(pack.Workflows, PackWorkflow{
				Name: "Không xoá phiếu đã đóng", EntityName: "wf_tickets", Trigger: "before_delete",
				Actions:  []PackWorkflowAction{{Type: "veto", Message: "Không xoá phiếu đã đóng"}},
				IsActive: tc.active,
			})
			dst := &fakeWorkflowWriter{}
			svc, _ := newWorkflowTestService(dst)
			if _, err := svc.Install(context.Background(), "tenant-1", "user-1", pack); err != nil {
				t.Fatalf("install: %v", err)
			}
			if len(dst.created) != 3 {
				t.Fatalf("CreateWorkflow calls = %d, want 3", len(dst.created))
			}
			in := dst.created[2]
			if in.Trigger != "before_delete" || (in.IsActive != nil && !*in.IsActive) {
				t.Fatalf("installed rule = trigger %q isActive %v, want before_delete switched on", in.Trigger, in.IsActive)
			}
		})
	}
}

// T083 (specs/048 S4): a before_save field_update keeps onlyIfEmpty and valueSource across export →
// JSON → install. Dropped in transit, "fill Website only when the source is empty" would install as
// "always overwrite the source", and a {{field}} template would be written literally.
func TestPackWorkflow_OnlyIfEmptyValueSourceRoundTrip(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "crm_leads", Label: "Lead"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	src := &fakeWorkflowWriter{existing: []workflow.WorkflowRecord{{
		ID: "wf-1", TenantID: "tenant-1", Name: "Lead không nguồn là Website", EntityName: "crm_leads",
		Trigger: "before_save",
		Actions: json.RawMessage(`[{"type":"field_update","field":"source","value":"Web","onlyIfEmpty":true},` +
			`{"type":"field_update","field":"company","value":"{{full_name}}","valueSource":"template"}]`),
		IsActive: true,
	}}}
	pack, _, err := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}, WorkflowWriter: src}).
		Export(ctx, "tenant-1", []string{"crm_leads"}, false, nil)
	if err != nil || len(pack.Workflows) != 1 {
		t.Fatalf("export: %v %+v", err, pack)
	}
	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}
	dst := &fakeWorkflowWriter{}
	svc, _ := newWorkflowTestService(dst)
	if _, err := svc.Install(ctx, "tenant-2", "user-2", &wire); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(dst.created) != 1 || len(dst.created[0].Actions) != 2 {
		t.Fatalf("installed = %+v", dst.created)
	}
	fill, tmpl := dst.created[0].Actions[0], dst.created[0].Actions[1]
	if !fill.OnlyIfEmpty || fill.ValueSource != "" || fill.Value != "Web" {
		t.Fatalf("source action = %+v — onlyIfEmpty lost in transit?", fill)
	}
	if tmpl.ValueSource != "template" || tmpl.OnlyIfEmpty || tmpl.Value != "{{full_name}}" {
		t.Fatalf("company action = %+v — valueSource lost in transit?", tmpl)
	}
}

// T096 (specs/048 S5): a normalize action keeps its operations, in order, across export → JSON →
// install. Dropped in transit, the installed rule would be refused (NORMALIZE_OPS_INVALID) and the
// pack install would die at Pass 8; reordered, "upper then lower" would install as its opposite.
func TestPackWorkflow_NormalizeOpsRoundTrip(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "crm_leads", Label: "Lead"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	src := &fakeWorkflowWriter{existing: []workflow.WorkflowRecord{{
		ID: "wf-1", TenantID: "tenant-1", Name: "Chuẩn hoá email và họ tên", EntityName: "crm_leads",
		Trigger: "before_save",
		Actions: json.RawMessage(`[{"type":"normalize","field":"email","ops":["trim","lower"]},` +
			`{"type":"normalize","field":"full_name","ops":["collapse_spaces","trim"]}]`),
		IsActive: true,
	}}}
	pack, warnings, err := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}, WorkflowWriter: src}).
		Export(ctx, "tenant-1", []string{"crm_leads"}, false, nil)
	if err != nil || len(pack.Workflows) != 1 {
		t.Fatalf("export: %v %+v", err, pack)
	}
	for _, w := range warnings {
		if strings.Contains(w, "normalize") {
			t.Fatalf("normalize is a known action — no 'unknown action' warning expected, got %q", w)
		}
	}
	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}
	dst := &fakeWorkflowWriter{}
	svc, _ := newWorkflowTestService(dst)
	if _, err := svc.Install(ctx, "tenant-2", "user-2", &wire); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(dst.created) != 1 || len(dst.created[0].Actions) != 2 {
		t.Fatalf("installed = %+v", dst.created)
	}
	email, name := dst.created[0].Actions[0], dst.created[0].Actions[1]
	if email.Type != "normalize" || email.Field != "email" || strings.Join(email.Ops, ",") != "trim,lower" {
		t.Fatalf("email action = %+v — ops lost in transit?", email)
	}
	if name.Field != "full_name" || strings.Join(name.Ops, ",") != "collapse_spaces,trim" {
		t.Fatalf("full_name action = %+v — ops reordered or lost?", name)
	}
}
