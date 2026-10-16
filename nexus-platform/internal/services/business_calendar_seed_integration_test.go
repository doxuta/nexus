//go:build integration

// Integration cover for the default-working-calendar seed (DAILY-QUEUE #10). The unit test next to
// it proves the JSON parses; this proves the two behaviours that only exist against a real MySQL:
// the single-statement insert is idempotent, and it does NOT resurrect a calendar the tenant
// deliberately deleted.
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true" \
//	  go test -tags=integration ./internal/services/ -run SeedDefaultBusinessCalendar
package services

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"nexus/platform/internal/calmeta"
)

func TestSeedDefaultBusinessCalendar_Integration(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	repo := calmeta.NewSQLRepository(sdb)
	const tenant = "11111111-2222-3333-4444-555555555555"

	// 1. First call seeds; it reports that it did.
	seeded, err := SeedDefaultBusinessCalendar(ctx, gdb, tenant)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !seeded {
		t.Fatal("first seed reported no insert; the caller uses this to tell the operator what changed")
	}
	cal, err := repo.GetDefault(ctx, tenant)
	if err != nil {
		t.Fatalf("GetDefault after seed: %v", err)
	}
	if cal.Name != DefaultCalendarName || !cal.IsDefault {
		t.Fatalf("seeded calendar = {name:%q default:%v}, want {%q true}", cal.Name, cal.IsDefault, DefaultCalendarName)
	}

	// 2. Re-running must be a no-op. bootstrap-admin calls this on EVERY run.
	seeded, err = SeedDefaultBusinessCalendar(ctx, gdb, tenant)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if seeded {
		t.Error("second seed reported an insert — a re-run would grow a calendar every time")
	}
	assertLiveCalendarCount(t, gdb, tenant, 1)

	// 3. A tenant that DELETED its only calendar chose the wall-clock fallback. Re-seeding would
	//    silently move every SLA instant in that tenant, with nothing on their side saying why —
	//    so the existence probe deliberately counts soft-deleted rows too.
	if err := repo.SoftDelete(ctx, tenant, cal.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	seeded, err = SeedDefaultBusinessCalendar(ctx, gdb, tenant)
	if err != nil {
		t.Fatalf("seed after delete: %v", err)
	}
	if seeded {
		t.Fatal("the seed resurrected a calendar the tenant deliberately deleted — every business_hours deadline in that tenant just moved, silently")
	}
	if _, err := repo.GetDefault(ctx, tenant); err == nil {
		t.Fatal("GetDefault still returns a calendar after the tenant deleted it")
	}

	// 4. Isolation: seeding tenant A never touches tenant B.
	const other = "99999999-8888-7777-6666-555555555555"
	if _, err := SeedDefaultBusinessCalendar(ctx, gdb, other); err != nil {
		t.Fatalf("seed other tenant: %v", err)
	}
	assertLiveCalendarCount(t, gdb, other, 1)
	assertLiveCalendarCount(t, gdb, tenant, 0)
}

// assertLiveCalendarCount counts the tenant's NON-deleted calendars straight from the table, so the
// assertion does not depend on the repository's own filtering.
func assertLiveCalendarCount(t *testing.T, gdb *gorm.DB, tenantID string, want int64) {
	t.Helper()
	var got int64
	if err := gdb.Raw(`SELECT COUNT(*) FROM sys_business_calendars WHERE tenant_id = ? AND deleted_at IS NULL`, tenantID).
		Scan(&got).Error; err != nil {
		t.Fatalf("count calendars: %v", err)
	}
	if got != want {
		t.Errorf("tenant %s has %d live calendar(s), want %d", tenantID, got, want)
	}
}
