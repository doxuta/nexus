//go:build integration

package services

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// #331 on real MySQL, wired as cmd/server wires it (real dynrepo, real metadata, an authorizer
// resolving a NON-admin rep with view:own on the target): the rep may point a deal at their own
// customer and may NOT point it at a colleague's — the answer is the same 422 a missing id gets —
// across single create, update and batch create; admin and the tenant-wide system probe are
// unchanged.
func TestRelationFKViewScope_Integration(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entSvc := NewEntityService(dynRepo, metaRepo)
	tenant, rep, colleague := uuid.NewString(), uuid.NewString(), uuid.NewString()

	if _, _, err := entSvc.CreateEntity(ctx, tenant, CreateEntityInput{Name: "vs_khach", Label: "Khách", Fields: []FieldInput{
		{Name: "ten", Label: "Tên", FieldType: "text", IsRequired: true},
	}}); err != nil {
		t.Fatalf("CreateEntity vs_khach: %v", err)
	}
	if _, _, err := entSvc.CreateEntity(ctx, tenant, CreateEntityInput{Name: "vs_co_hoi", Label: "Cơ hội", Fields: []FieldInput{
		{Name: "ten", Label: "Tên", FieldType: "text", IsRequired: true},
		{Name: "khach", Label: "Khách", FieldType: "relation", Options: map[string]any{"relatedEntity": "vs_khach"}},
	}}); err != nil {
		t.Fatalf("CreateEntity vs_co_hoi: %v", err)
	}
	mine, err := dynRepo.InsertRecord(ctx, "vs_khach", tenant, rep, map[string]any{"ten": "Của tôi"}, nil)
	if err != nil {
		t.Fatalf("seed mine: %v", err)
	}
	theirs, err := dynRepo.InsertRecord(ctx, "vs_khach", tenant, colleague, map[string]any{"ten": "Của đồng nghiệp"}, nil)
	if err != nil {
		t.Fatalf("seed theirs: %v", err)
	}

	svc := NewRecordService(metaRepo, dynRepo).WithSequenceClaimer(dynRepo).WithBulkInserter(dynRepo).
		WithACLAuthorizer(&levelAuthz{level: "owner"})
	repScope := RecordScope{UserID: rep, AccessLevel: "owner", RoleID: "role-rep"}
	adminScope := RecordScope{UserID: colleague, AccessLevel: "all", IsAdmin: true}

	// Visible target: ok.
	oppID, err := svc.Create(ctx, repScope, tenant, rep, "vs_co_hoi", map[string]any{"ten": "A", "khach": mine})
	if err != nil {
		t.Fatalf("create with own customer: %v", err)
	}
	// Colleague's customer: 422 RECORD_RELATION_INVALID, same sentence as a missing id.
	_, hidden := svc.Create(ctx, repScope, tenant, rep, "vs_co_hoi", map[string]any{"ten": "B", "khach": theirs})
	_, ghost := svc.Create(ctx, repScope, tenant, rep, "vs_co_hoi", map[string]any{"ten": "B", "khach": uuid.NewString()})
	if codeOf(hidden) != "RECORD_RELATION_INVALID" || codeOf(ghost) != "RECORD_RELATION_INVALID" {
		t.Fatalf("hidden=%v ghost=%v; want both RECORD_RELATION_INVALID", hidden, ghost)
	}
	if hidden.Error() != ghost.Error() { // the message names the field + target entity, never the id
		t.Errorf("hidden and missing answers differ: %q vs %q", hidden, ghost)
	}
	// Update: re-pointing at the colleague's customer refused; resending the untouched one ok.
	if err := svc.Update(ctx, repScope, tenant, "vs_co_hoi", oppID, map[string]any{"ten": "A2", "khach": mine}, nil); err != nil {
		t.Fatalf("update resending own customer: %v", err)
	}
	if err := svc.Update(ctx, repScope, tenant, "vs_co_hoi", oppID, map[string]any{"khach": theirs}, nil); codeOf(err) != "RECORD_RELATION_INVALID" {
		t.Fatalf("update to colleague's customer: %v; want RECORD_RELATION_INVALID", err)
	}
	// Batch: only the offending row fails.
	res, err := svc.CreateBulk(ctx, repScope, tenant, rep, "vs_co_hoi", []map[string]any{
		{"ten": "R0", "khach": mine}, {"ten": "R1", "khach": theirs},
	}, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if res.Results[0].Status != "created" || res.Results[1].Status != "failed" || res.Results[1].Code != "RECORD_RELATION_INVALID" {
		t.Errorf("bulk results = %+v", res.Results)
	}
	// Admin: tenant-wide, unchanged.
	if _, err := svc.Create(ctx, adminScope, tenant, colleague, "vs_co_hoi", map[string]any{"ten": "C", "khach": mine}); err != nil {
		t.Fatalf("admin create: %v", err)
	}
}
