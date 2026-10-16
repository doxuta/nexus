//go:build integration

// Round-trip gate of the change preview (specs/067 FR-056): save a document through the REAL apply
// service, propose the equivalent again, and the Before the tool reads (the SERVED layout, the
// stored rule) must diff EMPTY against the proposal. Diff(x, x) would not prove this — the noise
// (served-time `sortable`, intervalSeconds, match "and") only shows when the two sides come from
// different code paths.
package ai

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

func TestChange_IT_RoundTrip_ListLayout(t *testing.T) {
	repo, gdb := newRelITReader(t)
	seedLayoutIT(t, gdb)
	ctx := context.Background()
	svc := services.NewLayoutService(repo, metadata.NewLayoutRepository(gdb), nil)

	list := map[string]any{"version": float64(1), "columns": []any{map[string]any{"field": "full_name"}, map[string]any{"field": "note"}},
		"defaultSort": map[string]any{"field": "created_at", "order": "desc"}}
	if _, err := svc.SaveLayout(ctx, layoutITTenantA, services.SaveLayoutInput{EntityName: layoutITEntity, ViewType: "list", LayoutJSON: list}); err != nil {
		t.Fatalf("SaveLayout: %v", err)
	}

	e := &toolExecutor{reader: repo, tenantID: layoutITTenantA, docs: &DocReaders{Layouts: svc}}
	ok, res := parseToolResult(t, e.execute(ctx, "propose_layout",
		`{"entity":"`+layoutITEntity+`","viewType":"list","layoutJson":{"version":1,"columns":[{"field":"full_name"},{"field":"note"}],"defaultSort":{"field":"created_at","order":"desc"}}}`))
	if !ok {
		t.Fatalf("refused: %v", res)
	}
	c := lastChange(t, e)
	if c.Op != "update" || len(c.ChangedPaths) != 0 || c.MoreChanged != 0 {
		t.Fatalf("saving the same layout must diff empty, got op=%s paths=%v", c.Op, c.ChangedPaths)
	}
}

func TestChange_IT_RoundTrip_Workflow(t *testing.T) {
	repo, gdb := newRelITReader(t)
	sqlxDB := connectAI(t)
	seedWorkflowIT(t, gdb)
	ctx := context.Background()
	svc := services.NewWorkflowService(workflow.NewSQLRepository(sqlxDB)).WithEntityChecker(repo)
	e := &toolExecutor{reader: repo, tenantID: wfITTenantA, docs: &DocReaders{Workflows: svc}}

	args := `{"name":"Đóng ticket","entity":"` + wfITEntity + `","trigger":"after_update",` +
		`"condition":{"match":"and","rules":[{"field":"status","op":"eq","value":"closed"}]},` +
		`"actions":[{"type":"field_update","field":"status","value":"closed"}]}`
	if ok, res := parseToolResult(t, e.execute(ctx, "propose_workflow", args)); !ok {
		t.Fatalf("refused: %v", res)
	}
	if c := lastChange(t, e); c.Op != "create" {
		t.Fatalf("first proposal must be create, got %s", c.Op)
	}
	if _, err := svc.CreateWorkflow(ctx, wfITTenantA, draftToCreateInput(e.draft[0].Workflow)); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	e2 := &toolExecutor{reader: repo, tenantID: wfITTenantA, docs: &DocReaders{Workflows: svc}}
	if ok, res := parseToolResult(t, e2.execute(ctx, "propose_workflow", args)); !ok {
		t.Fatalf("refused: %v", res)
	}
	c := lastChange(t, e2)
	if c.Op != "update" || len(c.ChangedPaths) != 0 {
		t.Fatalf("same rule again must be update with empty diff, got op=%s paths=%v", c.Op, c.ChangedPaths)
	}
}
