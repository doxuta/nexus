//go:build integration

// specs/067 T082 (L10-5, SC-009): once a project is delivered, the builder's edit of a rule is a DRAFT;
// the AI read tools and the change preview must see that draft when the ctx carries a ReadDraft Caller
// (what DraftCaller sets on /api/v1/admin/ai/builder/chat) and must see the live rule otherwise.
package ai

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

func TestDraftOverlay_IT_AIReadsTheBuildersDraft(t *testing.T) {
	repo, gdb := newRelITReader(t)
	sqlxDB := connectAI(t)
	seedWorkflowIT(t, gdb)
	const tenant, builder = wfITTenantA, "aiaiaiai-m7m7-4444-8888-builder00001"

	cleanup := func() {
		for _, q := range []string{
			`DELETE FROM sys_meta_version_docs WHERE tenant_id = ?`,
			`DELETE FROM sys_meta_versions WHERE tenant_id = ?`,
			`DELETE FROM sys_meta_drafts WHERE tenant_id = ?`,
			`DELETE FROM tenants WHERE id = ?`,
		} {
			gdb.Exec(q, tenant)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	// The draft layer locks the tenant row.
	if err := gdb.Exec(`INSERT INTO tenants (id, slug, name) VALUES (?, 'aidraftov', 'ai draft overlay')`, tenant).Error; err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	reg := metaversion.NewRegistry(services.NewWorkflowDocAdapter())
	drafts := metaversion.NewDraftRepo(sqlxDB, reg)
	vers := metaversion.NewVersionRepo(sqlxDB)
	gate := services.NewDraftGate(vers)
	raw := workflow.NewSQLRepository(sqlxDB)
	svc := services.NewWorkflowService(services.NewDraftWorkflowRepo(raw, drafts, gate)).WithLiveRepo(raw).WithEntityChecker(repo)
	pub := services.NewPublishService(sqlxDB, drafts, vers, reg, gate, nil, nil)

	rule := func(value string) services.CreateWorkflowInput {
		on := true
		return services.CreateWorkflowInput{Name: "Đóng ticket", EntityName: wfITEntity, Trigger: "after_update", IsActive: &on,
			Rules:   []services.WorkflowRuleInput{{Field: "status", Op: workflow.OpEq, Value: value}},
			Actions: []services.WorkflowActionInput{{Type: "field_update", Field: "status", Value: "closed"}}}
	}
	builderCtx := metaversion.WithCaller(context.Background(),
		metaversion.Caller{UserID: builder, TenantID: tenant, HomeTenantID: tenant, IsBuilder: true, ReadDraft: true})

	created, err := svc.CreateWorkflow(context.Background(), tenant, rule("open"))
	if err != nil {
		t.Fatalf("CreateWorkflow (pre-delivery, live): %v", err)
	}
	if _, err := pub.Baseline(builderCtx, tenant, "first"); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	upd := rule("closed")
	if _, err := svc.UpdateWorkflow(builderCtx, tenant, created.ID, services.UpdateWorkflowInput{Name: upd.Name, EntityName: upd.EntityName,
		Trigger: upd.Trigger, Rules: upd.Rules, Actions: upd.Actions, IsActive: upd.IsActive, ActorID: builder}); err != nil {
		t.Fatalf("UpdateWorkflow (delivered => draft): %v", err)
	}

	cond := func(v string) string { return `"op":"eq","field":"status","value":"` + v + `"` } // the rule, not the action's value
	run := func(ctx context.Context) (list string, e *toolExecutor) {
		e = &toolExecutor{reader: repo, tenantID: tenant, docs: &DocReaders{Workflows: svc}}
		list = e.execute(ctx, "list_workflows", `{"entity":"`+wfITEntity+`"}`)
		return list, e
	}

	// The builder's chat ctx (ReadDraft=true) sees the DRAFT condition...
	list, e := run(builderCtx)
	if !strings.Contains(list, cond("closed")) || strings.Contains(list, cond("open")) {
		t.Fatalf("builder ctx must list the draft condition (closed), got %s", list)
	}
	// ...and propose_workflow for the same rule previews against the draft: before = closed.
	args := `{"name":"Đóng ticket","entity":"` + wfITEntity + `","trigger":"after_update",` +
		`"condition":{"match":"and","rules":[{"field":"status","op":"eq","value":"closed"}]},` +
		`"actions":[{"type":"field_update","field":"status","value":"closed"}]}`
	if ok, res := parseToolResult(t, e.execute(builderCtx, "propose_workflow", args)); !ok {
		t.Fatalf("propose_workflow refused: %v", res)
	}
	c := lastChange(t, e)
	if c.Op != "update" || !strings.Contains(string(c.Before), "closed") || len(c.ChangedPaths) != 0 {
		t.Fatalf("change.before must be the draft (closed, no diff): op=%s before=%s paths=%v", c.Op, c.Before, c.ChangedPaths)
	}

	// A ctx without a Caller (⌘K assistant, staff) reads the LIVE rule: open.
	list, e = run(context.Background())
	if !strings.Contains(list, cond("open")) || strings.Contains(list, cond("closed")) {
		t.Fatalf("a ctx without Caller must list the live condition (open), got %s", list)
	}
	e.execute(context.Background(), "propose_workflow", args)
	if c := lastChange(t, e); !strings.Contains(string(c.Before), "open") {
		t.Fatalf("no-caller change.before must be the live rule, got %s", c.Before)
	}
}
