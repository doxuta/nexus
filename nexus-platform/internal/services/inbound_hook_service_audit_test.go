package services

// specs/062-customer-workspace L4 T064a — an inbound webhook's create / update / secret rotation /
// delete leave an __inbound_hook__ audit row whose diff NEVER carries the hook's secret (SENTINEL),
// and D-22: a hook created by someone who is not a user of the hook's own tenant runs as the fixed
// system actor, so the customer's trail never names a Nexus builder on the partner system's writes.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/inboundhook"
)

const sentinelHookSecret = "SENTINEL-HOOK-SECRET"

// fakeHookStore is an in-memory hookStore. Create stores the hook as given except that its Secret is
// replaced by sentinelHookSecret, so a leak of ANY stored secret into a diff is caught by the sentinel.
type fakeHookStore struct {
	rows map[string]*inboundhook.Hook
}

func newFakeHookStore() *fakeHookStore { return &fakeHookStore{rows: map[string]*inboundhook.Hook{}} }

func (f *fakeHookStore) Create(_ context.Context, h *inboundhook.Hook) error {
	cp := *h
	cp.Secret = sentinelHookSecret
	f.rows[h.ID] = &cp
	return nil
}

func (f *fakeHookStore) Update(_ context.Context, tenantID string, h *inboundhook.Hook) error {
	r, ok := f.rows[h.ID]
	if !ok || r.TenantID != tenantID {
		return inboundhook.ErrNotFound
	}
	r.Name, r.EntityName, r.Mapping, r.MatchField, r.PaymentMatch, r.IsActive = h.Name, h.EntityName, h.Mapping, h.MatchField, h.PaymentMatch, h.IsActive
	return nil
}

func (f *fakeHookStore) UpdateSecret(_ context.Context, tenantID, id, secret string) error {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return inboundhook.ErrNotFound
	}
	r.Secret = secret + sentinelHookSecret
	return nil
}

func (f *fakeHookStore) SoftDelete(_ context.Context, tenantID, id string) error {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return inboundhook.ErrNotFound
	}
	delete(f.rows, id)
	return nil
}

func (f *fakeHookStore) GetByID(_ context.Context, tenantID, id string) (*inboundhook.Hook, error) {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return nil, inboundhook.ErrNotFound
	}
	return r, nil
}

func (f *fakeHookStore) GetByName(context.Context, string, string) (*inboundhook.Hook, error) {
	return nil, inboundhook.ErrNotFound
}
func (f *fakeHookStore) ListAll(context.Context, string) ([]inboundhook.Hook, error) { return nil, nil }
func (f *fakeHookStore) RecordDelivery(context.Context, *inboundhook.Delivery) error { return nil }
func (f *fakeHookStore) ListDeliveries(context.Context, string, string, int) ([]inboundhook.Delivery, error) {
	return nil, nil
}

// fakeHookUsers knows the user ids in `known` (of ANY tenant — the tenant check is the caller's
// argument, which the test pins) and fails with err when set.
type fakeHookUsers struct {
	tenant string
	known  map[string]bool
	err    error
}

func (f *fakeHookUsers) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	if tenantID == f.tenant && f.known[id] {
		return &auth.User{ID: id, TenantID: tenantID}, nil
	}
	return nil, auth.ErrUserNotFound
}

func hookAuditSvc(t *testing.T) (*InboundHookService, *fakeHookStore, *captureAuditor) {
	t.Helper()
	store, aud := newFakeHookStore(), &captureAuditor{}
	rec := NewRecordService(sensitiveFixture(t), &fakeRecordStore{})
	svc := NewInboundHookService(store, rec, nil, nil).WithAuditor(aud).
		WithUserReader(&fakeHookUsers{tenant: "tenant-1", known: map[string]bool{"staff-1": true}})
	return svc, store, aud
}

func hookInput(name string) InboundHookInput {
	return InboundHookInput{Name: name, EntityName: "customers", Mapping: []inboundhook.Mapping{{Source: "data.name", Field: "name"}}}
}

func hookCtx() context.Context { return audit.WithActor(context.Background(), "builder-1") }

// SENTINEL (L4 mutation-prove: pass the real hook to StructToMap and every row below goes red).
func TestInboundHookService_WritesEmitAuditWithoutTheSecret(t *testing.T) {
	svc, _, aud := hookAuditSvc(t)
	ctx := hookCtx()

	h, err := svc.Create(ctx, "tenant-1", "staff-1", hookInput("bank-credit"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	upd := hookInput("bank-credit")
	upd.Mapping = []inboundhook.Mapping{{Source: "data.full_name", Field: "name"}}
	if _, err := svc.Update(ctx, "tenant-1", h.ID, upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := svc.RotateSecret(ctx, "tenant-1", h.ID); err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	if err := svc.Delete(ctx, "tenant-1", h.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want := []string{audit.ActionCreate, audit.ActionUpdate, audit.ActionUpdate, audit.ActionDelete}
	if len(aud.events) != len(want) {
		t.Fatalf("events = %d; want %d: %+v", len(aud.events), len(want), aud.events)
	}
	for i, e := range aud.events {
		if e.EntityName != audit.InboundHookMeta || e.Action != want[i] || e.TenantID != "tenant-1" || e.ActorUserID != "builder-1" || e.RecordID != h.ID {
			t.Errorf("event %d = %+v; want %s/%s by builder-1 on %s", i, e, audit.InboundHookMeta, want[i], h.ID)
		}
		if e.Diff == nil {
			t.Fatalf("event %d has no diff", i)
		}
	}
	// h.Secret is the real minted key (the store keeps its own marker): neither may be in any diff.
	assertNoSentinel(t, aud.events, sentinelHookSecret, h.Secret)
	if aud.events[0].Diff.After["secret"] != auditRedacted {
		t.Errorf("create diff = %+v; want secret:%q", aud.events[0].Diff, auditRedacted)
	}
	if aud.events[2].Diff.Before != nil || aud.events[2].Diff.After["secret"] != auditRedacted {
		t.Errorf("rotate diff = %+v; want after-only {secret:%q}", aud.events[2].Diff, auditRedacted)
	}
	if aud.events[1].Diff.Before["mapping"] == nil || aud.events[1].Diff.After["mapping"] == nil {
		t.Errorf("update diff = %+v; want both sides of mapping", aud.events[1].Diff)
	}
	if aud.events[3].Diff.After != nil || aud.events[3].Diff.Before["name"] != "bank-credit" {
		t.Errorf("delete diff = %+v; want before-only", aud.events[3].Diff)
	}
}

// D-22: created_by is the creator when they are a user of THIS tenant, else the system actor; the
// audit row still names the request's actor (the builder) either way.
func TestInboundHookService_Create_D22_ActorOutsideTheTenantRunsAsSystem(t *testing.T) {
	tests := []struct {
		name      string
		tenant    string
		userID    string
		wantBy    string
		wantUsers error
	}{
		{name: "creator is a user of the tenant: unchanged behaviour", tenant: "tenant-1", userID: "staff-1", wantBy: "staff-1"},
		{name: "creator has no row in the tenant (a builder in the session)", tenant: "tenant-1", userID: "builder-1", wantBy: inboundhook.SystemActorID},
		{name: "creator exists only in ANOTHER tenant", tenant: "tenant-2", userID: "staff-1", wantBy: inboundhook.SystemActorID},
		{name: "empty creator id", tenant: "tenant-1", userID: "", wantBy: inboundhook.SystemActorID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, aud := hookAuditSvc(t)
			h, err := svc.Create(hookCtx(), tc.tenant, tc.userID, hookInput("h1"))
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got := store.rows[h.ID].CreatedBy; got != tc.wantBy {
				t.Fatalf("created_by = %q; want %q", got, tc.wantBy)
			}
			if len(aud.events) != 1 || aud.events[0].ActorUserID != "builder-1" {
				t.Fatalf("audit row = %+v; want one row naming the request's actor builder-1", aud.events)
			}
		})
	}

	t.Run("an unreadable users table refuses the create and writes nothing", func(t *testing.T) {
		boom := errors.New("db down")
		svc, store, aud := hookAuditSvc(t)
		svc.WithUserReader(&fakeHookUsers{err: boom})
		if _, err := svc.Create(hookCtx(), "tenant-1", "staff-1", hookInput("h1")); !errors.Is(err, boom) {
			t.Fatalf("err = %v; want the wrapped read error", err)
		}
		if len(store.rows) != 0 || len(aud.events) != 0 {
			t.Fatalf("rows=%d events=%d; want nothing created and nothing audited", len(store.rows), len(aud.events))
		}
	})
}

func TestInboundHookService_RejectedWriteEmitsNothing(t *testing.T) {
	svc, _, aud := hookAuditSvc(t)
	bad := hookInput("Not A Slug")
	if _, err := svc.Create(hookCtx(), "tenant-1", "staff-1", bad); err == nil {
		t.Fatal("want HOOK_INVALID")
	}
	if _, err := svc.RotateSecret(hookCtx(), "tenant-1", "ghost"); err == nil {
		t.Fatal("want HOOK_NOT_FOUND")
	}
	if err := svc.Delete(hookCtx(), "tenant-1", "ghost"); err == nil {
		t.Fatal("want HOOK_NOT_FOUND")
	}
	if len(aud.events) != 0 {
		t.Fatalf("events after rejected writes = %d; want 0", len(aud.events))
	}
}
