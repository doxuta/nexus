package ai

import (
	"context"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"

	"nexus/platform/internal/config"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// Issue #22 item 1: propose_field judges a dependent picklist against the model the CALLER reads (on a
// delivered project, the builder's drafts), and files no card for a table its tenant does not have.

// draftOverlayReader shows the builder's unpublished edit of tickets.status (a value "hold") only to a
// ctx that carries a ReadDraft Caller, as services.DraftFieldRepo does on a delivered project.
type draftOverlayReader struct{ *fakeMetaReader }

func (r draftOverlayReader) ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	fields, err := r.fakeMetaReader.ListFields(ctx, tenantID, entityID)
	if c, ok := metaversion.CallerFrom(ctx); err != nil || !ok || !c.ReadDraft {
		return fields, err
	}
	out := slices.Clone(fields)
	for i := range out {
		if out[i].Name == "status" {
			draft := `{"values":["open","closed","hold"]}`
			out[i].Options = &draft
		}
	}
	return out, nil
}

const holdReason = `{"entity":"tickets","name":"hold_reason","label":"Lý do chờ","fieldType":"picklist",
	"options":{"values":["parts","other"],"controllingField":"status","dependentValues":{"hold":["parts"]}}}`

func TestTools_ProposeField_SiblingsAreTheCallersDraft(t *testing.T) {
	builder := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "u-b", TenantID: "tenant-A", HomeTenantID: "tenant-A", IsBuilder: true, ReadDraft: true})
	reader := draftOverlayReader{editFixtureReader()}

	t.Run("in-app turn", func(t *testing.T) {
		e := &toolExecutor{reader: reader, tenantID: "tenant-A"}
		if ok, _ := parseToolResult(t, e.execute(builder, "propose_field", holdReason)); !ok || len(e.draft) != 1 {
			t.Fatalf("keyed on the builder's draft value: want buffered, got %v (draft %d)", ok, len(e.draft))
		}
		// The control: without the builder's Caller the published model has no "hold".
		e = &toolExecutor{reader: reader, tenantID: "tenant-A"}
		if res := e.execute(context.Background(), "propose_field", holdReason); !strings.Contains(res, "FIELD_DEPENDENT_PICKLIST_INVALID") {
			t.Fatalf("the published model must refuse it: %s", res)
		}
	})

	t.Run("one MCP call (RunTool)", func(t *testing.T) {
		agent := NewAIBuilderAgent(config.AIConfig{}, nil, reader, nil, nil, zap.NewNop())
		res, added := agent.RunTool(builder, "tenant-A", "sess-1", nil, "propose_field", holdReason)
		if ok, _ := parseToolResult(t, res); !ok || len(added) != 1 {
			t.Fatalf("keyed on the builder's draft value: want one proposal, got %s", res)
		}
	})
}

func TestTools_ProposeField_RefusesATableNotInTheTenant(t *testing.T) {
	note := func(entity string) string {
		return `{"entity":"` + entity + `","name":"note","label":"Ghi chú","fieldType":"text"}`
	}
	agent := NewAIBuilderAgent(config.AIConfig{}, nil, editFixtureReader(), nil, nil, zap.NewNop())
	// "invoices" is tenant-B's table: it must read as missing to tenant-A, as any unknown name does.
	for _, entity := range []string{"ghosts", "invoices"} {
		e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
		if res := e.execute(context.Background(), "propose_field", note(entity)); !strings.Contains(res, "ENTITY_NOT_FOUND") || len(e.draft) != 0 {
			t.Errorf("in-app propose_field on %q: %s (draft %d); want ENTITY_NOT_FOUND and no card", entity, res, len(e.draft))
		}
		if res, added := agent.RunTool(context.Background(), "tenant-A", "sess-1", nil, "propose_field", note(entity)); !strings.Contains(res, "ENTITY_NOT_FOUND") || len(added) != 0 {
			t.Errorf("MCP propose_field on %q: %s (%d added); want ENTITY_NOT_FOUND and no card", entity, res, len(added))
		}
	}

	// A table proposed earlier — this turn, or earlier in the MCP thread (prior) — is not missing.
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	e.execute(context.Background(), "propose_entity", `{"name":"ghosts","label":"Ma","fields":[{"name":"title","label":"Tên","fieldType":"text"}]}`)
	if ok, _ := parseToolResult(t, e.execute(context.Background(), "propose_field", note("ghosts"))); !ok {
		t.Fatal("a table drafted this turn must take a field")
	}
	prior := []DraftItem{{Kind: "entity", Entity: &DraftEntity{Name: "ghosts", Label: "Ma"}}}
	if res, added := agent.RunTool(context.Background(), "tenant-A", "sess-1", prior, "propose_field", note("ghosts")); len(added) != 1 {
		t.Fatalf("a table the thread proposed earlier must take a field: %s", res)
	}
}
