package services

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
)

func (f *fakeSeedRoles) ListActionsForRole(_ context.Context, _, roleID string) ([]auth.RoleAction, error) {
	return f.grants[roleID], nil
}

// S7-5 / FR-032: on a delivered workspace the seed leaves one direct entry per default role it
// creates or extends, with before (nil on create) and after grants.
func TestAppACLSeedService_RecordsDirectEntries(t *testing.T) {
	const tenant = "tenant-1"
	app := &appmeta.App{ID: "app-1", TenantID: tenant, Name: "myapp", Label: "App"}
	store := &fakeDirectStore{delivered: true}
	rec := &VersionRecorder{store: store, logger: zap.NewNop()}
	svc := NewAppACLSeedService(newFakeSeedApps(app), newFakeSeedRoles()).WithRecorder(rec)
	svc.newID = seedIDs()
	ctx := callerCtx()

	if _, _, err := svc.SeedForEntity(ctx, tenant, app.ID, "widgets"); err != nil {
		t.Fatal(err)
	}
	if len(store.versions) != 2 {
		t.Fatalf("direct entries = %d, want 2 (staff + manager)", len(store.versions))
	}
	if d := store.docs[0][0]; d.Op != "create" || d.BeforeJSON != nil || d.AfterJSON == nil {
		t.Errorf("first seed: op=%s before=%s", d.Op, d.BeforeJSON)
	}
	if _, _, err := svc.SeedForEntity(ctx, tenant, app.ID, "gadgets"); err != nil {
		t.Fatal(err)
	}
	if len(store.versions) != 4 {
		t.Fatalf("direct entries = %d, want 4", len(store.versions))
	}
	if d := store.docs[2][0]; d.Op != "update" || d.BeforeJSON == nil {
		t.Errorf("second seed: op=%s want update with before image", d.Op)
	}

	store.delivered = false
	if _, _, err := svc.SeedForEntity(ctx, tenant, app.ID, "things"); err != nil {
		t.Fatal(err)
	}
	if len(store.versions) != 4 {
		t.Errorf("undelivered tenant must record nothing, got %d", len(store.versions))
	}
}
