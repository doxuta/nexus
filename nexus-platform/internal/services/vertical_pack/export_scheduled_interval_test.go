package vertical_pack

// DAILY-QUEUE #295: a scheduled rule's cadence (interval_seconds) travels through export -> install.
// Dropped in transit, the installed rule fired on EVERY scheduler tick (a daily reminder became spam).

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

func TestPackWorkflow_ScheduledIntervalRoundTrip(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "sc_jobs", Label: "Jobs"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	src := &fakeWorkflowWriter{existing: []workflow.WorkflowRecord{
		{ID: "wf-1", TenantID: "tenant-1", Name: "Nhắc hằng ngày", EntityName: "sc_jobs", Trigger: "scheduled", IsActive: true,
			IntervalSeconds: 86400, Actions: json.RawMessage(`[{"type":"notify","userId":"u1","title":"t","message":"m"}]`)},
	}}
	pack, _, err := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}, WorkflowWriter: src}).
		Export(ctx, "tenant-1", []string{"sc_jobs"}, false, nil)
	if err != nil || len(pack.Workflows) != 1 {
		t.Fatalf("export: %v %+v", err, pack)
	}
	if got := pack.Workflows[0].IntervalSeconds; got != 86400 {
		t.Fatalf("exported intervalSeconds = %d, want 86400", got)
	}
	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}
	in, err := toWorkflowInput(wire.Workflows[0], "u", nil, nil, nil)
	if err != nil {
		t.Fatalf("toWorkflowInput: %v", err)
	}
	if in.IntervalSeconds != 86400 {
		t.Fatalf("install input intervalSeconds = %d, want 86400", in.IntervalSeconds)
	}
}
