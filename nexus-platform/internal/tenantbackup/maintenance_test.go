package tenantbackup

// specs/068-tenant-backup-restore T040 (service half) — the operator's manual write freeze: minutes
// 1..120, never the operator's own workspace, never while a restore holds the freeze, audited in BOTH
// the workshop's and the customer's trail.

import (
	"context"
	"testing"
	"time"

	"nexus/platform/internal/audit"
)

type fakeFreezer struct {
	setTenant string
	setUntil  time.Time
	setReason string
	cleared   []string
	prev      *time.Time
}

func (f *fakeFreezer) Set(_ context.Context, tenantID string, until time.Time, reason string) (*time.Time, error) {
	f.setTenant, f.setUntil, f.setReason = tenantID, until, reason
	return f.prev, nil
}
func (f *fakeFreezer) FrozenUntil(_ context.Context, tenantID string) (time.Time, bool, error) {
	if f.setTenant == tenantID {
		return f.setUntil, true, nil
	}
	return time.Time{}, false, nil
}
func (f *fakeFreezer) Clear(_ context.Context, tenantID string, _ *time.Time) error {
	f.cleared = append(f.cleared, tenantID)
	return nil
}

func TestMaintenance_SetAndClear(t *testing.T) {
	actor := Actor{TenantID: opTen, UserID: opUser}
	tests := []struct {
		name      string
		tenant    string
		minutes   int
		restoring bool
		wantCode  string // "" = success
	}{
		{"5 minutes", tenantA, 5, false, ""},
		{"upper bound 120", tenantA, 120, false, ""},
		{"zero minutes", tenantA, 0, false, "MAINTENANCE_MINUTES_INVALID"},
		{"121 minutes", tenantA, 121, false, "MAINTENANCE_MINUTES_INVALID"},
		{"own workspace", opTen, 5, false, "MAINTENANCE_SELF"},
		{"unknown tenant", "ffffffff-0000-4000-8000-0000000000ff", 5, false, "TENANT_NOT_FOUND"},
		{"restore running", tenantA, 5, true, "BACKUP_IN_PROGRESS"},
	}
	for _, tc := range tests {
		t.Run("set/"+tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.store.tenants[opTen] = &TenantInfo{ID: opTen, Slug: "northwind", Name: "Nexus"}
			f.store.restoring = map[string]bool{tenantA: tc.restoring}
			fr := &fakeFreezer{}
			f.svc.WithFreezer(fr)

			v, err := f.svc.SetMaintenance(context.Background(), MaintenanceInput{TenantID: tc.tenant, Minutes: tc.minutes, Actor: actor})
			if tc.wantCode != "" {
				asErr(t, err, tc.wantCode)
				if fr.setTenant != "" || len(f.audit.events) != 0 {
					t.Fatalf("a refused request must change and audit nothing: %+v %+v", fr, f.audit.events)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := f.store.now.Add(time.Duration(tc.minutes) * time.Minute)
			if !v.MaintenanceUntil.Equal(want) || !fr.setUntil.Equal(want) || fr.setTenant != tc.tenant || fr.setReason != "manual" {
				t.Fatalf("view=%v freezer=%+v want until %v", v, fr, want)
			}
			// Audited in the workshop's trail AND the customer's.
			var seenOp, seenCustomer bool
			for _, e := range f.audit.events {
				if e.EntityName != audit.TenantBackupMeta {
					t.Fatalf("event on %q", e.EntityName)
				}
				seenOp = seenOp || e.TenantID == opTen
				seenCustomer = seenCustomer || e.TenantID == tc.tenant
			}
			if !seenOp || !seenCustomer {
				t.Fatalf("audit must land in both trails: %+v", f.audit.events)
			}
		})
	}

	t.Run("clear", func(t *testing.T) {
		f := newFixture(t)
		f.store.tenants[opTen] = &TenantInfo{ID: opTen, Slug: "northwind", Name: "Nexus"}
		fr := &fakeFreezer{}
		f.svc.WithFreezer(fr)
		if err := f.svc.ClearMaintenance(context.Background(), MaintenanceInput{TenantID: tenantA, Actor: actor}); err != nil {
			t.Fatal(err)
		}
		if len(fr.cleared) != 1 || fr.cleared[0] != tenantA || len(f.audit.events) != 2 {
			t.Fatalf("cleared=%v audit=%d", fr.cleared, len(f.audit.events))
		}
		asErr(t, f.svc.ClearMaintenance(context.Background(), MaintenanceInput{TenantID: opTen, Actor: actor}), "MAINTENANCE_SELF")
		f.store.restoring = map[string]bool{tenantA: true}
		asErr(t, f.svc.ClearMaintenance(context.Background(), MaintenanceInput{TenantID: tenantA, Actor: actor}), "BACKUP_IN_PROGRESS")
	})

	// The backup list carries the live freeze deadline so the panel can show "Đang bảo trì đến HH:MM".
	t.Run("list meta carries the freeze", func(t *testing.T) {
		f := newFixture(t)
		f.store.tenants[opTen] = &TenantInfo{ID: opTen, Slug: "northwind", Name: "Nexus"}
		f.svc.WithFreezer(&fakeFreezer{})
		_, meta, err := f.svc.List(context.Background(), ListInput{TenantID: tenantA, CallerTenantID: opTen})
		if err != nil || meta.MaintenanceUntil != nil {
			t.Fatalf("not frozen: meta=%+v err=%v", meta, err)
		}
		v, err := f.svc.SetMaintenance(context.Background(), MaintenanceInput{TenantID: tenantA, Minutes: 5, Actor: actor})
		if err != nil {
			t.Fatal(err)
		}
		_, meta, err = f.svc.List(context.Background(), ListInput{TenantID: tenantA, CallerTenantID: opTen})
		if err != nil || meta.MaintenanceUntil == nil || !meta.MaintenanceUntil.Equal(v.MaintenanceUntil) {
			t.Fatalf("frozen: meta=%+v err=%v want %v", meta, err, v.MaintenanceUntil)
		}
	})

	t.Run("unwired freezer fails closed", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.svc.SetMaintenance(context.Background(), MaintenanceInput{TenantID: tenantA, Minutes: 5, Actor: actor}); err == nil {
			t.Fatal("without a freezer the freeze would silently not happen — must be an error")
		}
	})
}
