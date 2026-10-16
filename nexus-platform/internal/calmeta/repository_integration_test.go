//go:build integration

// Integration tests for the sys_business_calendars repository (CALENDAR-1). Run with:
//
//	go test -tags=integration -race ./internal/calmeta/
//
// They require a reachable MySQL 8 with migration 000047 applied. Set TEST_DATABASE_URL to
// override the DSN; otherwise the local compose DSN is used (same convention as
// internal/appmeta). Covered here (real MySQL): 2-tenant isolation, UNIQUE(tenant_id, name)
// + the tombstone-rename freeing the name slot, the default-clear transaction (create AND
// update paths), GetDefault, and the hours_json/holidays_json round-trip through the REAL
// JSON columns — the bytes read back must parse in internal/calendar (the run-time path).
package calmeta_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/calmeta"
)

// testDSN returns the DSN of the throwaway migrated database provisioned by TestMain (see
// main_test.go). Before #84 this read TEST_DATABASE_URL directly, which broke against a db-less
// DSN ("No database selected"); TestMain now creates+migrates a throwaway DB and points
// packageTestDSN at it.
func testDSN() string {
	return packageTestDSN
}

func testDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("mysql", testDSN())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

const (
	tenantA = "aaaa1111-0000-0000-0000-00000000cl01"
	tenantB = "bbbb2222-0000-0000-0000-00000000cl02"
)

func cleanTenants(t *testing.T, db *sqlx.DB) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		"DELETE FROM sys_business_calendars WHERE tenant_id IN (?, ?)", tenantA, tenantB); err != nil {
		t.Fatalf("clean: %v", err)
	}
}

const (
	hoursVN    = `{"mon":[{"start":"08:00","end":"12:00"},{"start":"13:00","end":"17:00"}],"tue":[{"start":"08:00","end":"17:00"}]}`
	holidaysVN = `["2026-09-02","2027-01-01"]`
)

func createInput(tenantID, name string, isDefault bool) calmeta.CreateInput {
	return calmeta.CreateInput{
		TenantID:  tenantID,
		Name:      name,
		Timezone:  "Asia/Ho_Chi_Minh",
		Hours:     []byte(hoursVN),
		Holidays:  []byte(holidaysVN),
		IsDefault: isDefault,
		CreatedBy: "it-actor",
	}
}

func TestCalendarRepo_UniqueNameAndTenantIsolation(t *testing.T) {
	db := testDB(t)
	cleanTenants(t, db)
	t.Cleanup(func() { cleanTenants(t, db) })
	repo := calmeta.NewSQLRepository(db)
	ctx := context.Background()

	idA, err := repo.Create(ctx, createInput(tenantA, "hanh_chinh", false))
	if err != nil {
		t.Fatalf("create tenant-a: %v", err)
	}
	// Same name in the SAME tenant → duplicate.
	if _, err := repo.Create(ctx, createInput(tenantA, "hanh_chinh", false)); !errors.Is(err, calmeta.ErrCalendarDuplicateName) {
		t.Fatalf("same-tenant dup err = %v; want ErrCalendarDuplicateName", err)
	}
	// Same name in ANOTHER tenant → fine (unique is per tenant).
	if _, err := repo.Create(ctx, createInput(tenantB, "hanh_chinh", false)); err != nil {
		t.Fatalf("cross-tenant same name: %v", err)
	}

	// Isolation: tenant B lists only its own; cross-tenant GetByID/Update/Delete are not-found.
	calsB, err := repo.List(ctx, tenantB)
	if err != nil || len(calsB) != 1 || calsB[0].TenantID != tenantB {
		t.Fatalf("tenant-b list = %+v (%v); want exactly its own calendar", calsB, err)
	}
	if _, err := repo.GetByID(ctx, tenantB, idA); !errors.Is(err, calmeta.ErrCalendarNotFound) {
		t.Fatalf("cross-tenant GetByID err = %v; want ErrCalendarNotFound", err)
	}
	if err := repo.Update(ctx, tenantB, idA, calmeta.UpdateInput{
		Name: "x", Timezone: "UTC", Hours: []byte(hoursVN),
	}); !errors.Is(err, calmeta.ErrCalendarNotFound) {
		t.Fatalf("cross-tenant Update err = %v; want ErrCalendarNotFound", err)
	}
	if err := repo.SoftDelete(ctx, tenantB, idA); !errors.Is(err, calmeta.ErrCalendarNotFound) {
		t.Fatalf("cross-tenant SoftDelete err = %v; want ErrCalendarNotFound", err)
	}

	// Soft-delete tombstones the name slot → the SAME name can be recreated.
	if err := repo.SoftDelete(ctx, tenantA, idA); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := repo.Create(ctx, createInput(tenantA, "hanh_chinh", false)); err != nil {
		t.Fatalf("recreate after soft-delete: %v (tombstone should free the name slot)", err)
	}
	if _, err := repo.GetByID(ctx, tenantA, idA); !errors.Is(err, calmeta.ErrCalendarNotFound) {
		t.Fatalf("deleted row still readable: err = %v; want ErrCalendarNotFound", err)
	}
}

func TestCalendarRepo_JSONRoundTripParsesAtRuntime(t *testing.T) {
	db := testDB(t)
	cleanTenants(t, db)
	t.Cleanup(func() { cleanTenants(t, db) })
	repo := calmeta.NewSQLRepository(db)
	ctx := context.Background()

	id, err := repo.Create(ctx, createInput(tenantA, "round_trip", false))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cal, err := repo.GetByID(ctx, tenantA, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Semantic JSON equality (MySQL normalizes key order/whitespace in JSON columns).
	var wantHours, gotHours map[string][]calendar.Interval
	if err := json.Unmarshal([]byte(hoursVN), &wantHours); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if err := json.Unmarshal(cal.Hours, &gotHours); err != nil {
		t.Fatalf("unmarshal stored hours: %v", err)
	}
	if len(gotHours) != len(wantHours) || len(gotHours["mon"]) != 2 || gotHours["mon"][1].End != "17:00" {
		t.Fatalf("stored hours = %+v; want %+v", gotHours, wantHours)
	}
	var gotHolidays []string
	if err := json.Unmarshal(cal.Holidays, &gotHolidays); err != nil {
		t.Fatalf("unmarshal stored holidays: %v", err)
	}
	if len(gotHolidays) != 2 || gotHolidays[0] != "2026-09-02" {
		t.Fatalf("stored holidays = %v", gotHolidays)
	}

	// The stored bytes must parse through the run-time path and give working math.
	sched, err := calendar.FromJSON(cal.Timezone, cal.Hours, cal.Holidays)
	if err != nil {
		t.Fatalf("stored row does not parse at run time: %v", err)
	}
	vn := sched.Location()
	deadline, err := sched.AddBusinessDuration(time.Date(2026, 7, 13, 9, 0, 0, 0, vn), 2*time.Hour) // Mon
	if err != nil {
		t.Fatalf("AddBusinessDuration on stored row: %v", err)
	}
	if want := time.Date(2026, 7, 13, 11, 0, 0, 0, vn); !deadline.Equal(want) {
		t.Fatalf("deadline = %v; want %v", deadline, want)
	}

	// NULL holidays: create without holidays → read back normalizes to [] and still parses.
	in := createInput(tenantA, "no_holidays", false)
	in.Holidays = nil
	id2, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatalf("create no-holidays: %v", err)
	}
	cal2, err := repo.GetByID(ctx, tenantA, id2)
	if err != nil {
		t.Fatalf("get no-holidays: %v", err)
	}
	if string(cal2.Holidays) != "[]" {
		t.Fatalf("NULL holidays read back as %s; want []", cal2.Holidays)
	}
	if _, err := calendar.FromJSON(cal2.Timezone, cal2.Hours, cal2.Holidays); err != nil {
		t.Fatalf("no-holidays row does not parse: %v", err)
	}
}

func TestCalendarRepo_DefaultClearAndGetDefault(t *testing.T) {
	db := testDB(t)
	cleanTenants(t, db)
	t.Cleanup(func() { cleanTenants(t, db) })
	repo := calmeta.NewSQLRepository(db)
	ctx := context.Background()

	// No calendar flagged → GetDefault is not-found (the SLA wall-clock fallback signal).
	if _, err := repo.GetDefault(ctx, tenantA); !errors.Is(err, calmeta.ErrCalendarNotFound) {
		t.Fatalf("GetDefault on empty tenant err = %v; want ErrCalendarNotFound", err)
	}

	id1, err := repo.Create(ctx, createInput(tenantA, "cal_one", true))
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	id2, err := repo.Create(ctx, createInput(tenantA, "cal_two", true))
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	// ≤1 default per tenant: the second default create cleared the first (same tx).
	c1, err := repo.GetByID(ctx, tenantA, id1)
	if err != nil || c1.IsDefault {
		t.Fatalf("cal 1 after second default = %+v (%v); want IsDefault=false", c1, err)
	}
	def, err := repo.GetDefault(ctx, tenantA)
	if err != nil || def.ID != id2 {
		t.Fatalf("GetDefault = %+v (%v); want cal_two", def, err)
	}

	// Update path clears too: flip cal_one back to default via Update.
	if err := repo.Update(ctx, tenantA, id1, calmeta.UpdateInput{
		Name: "cal_one", Timezone: "Asia/Ho_Chi_Minh", Hours: []byte(hoursVN), IsDefault: true, UpdatedBy: "it-actor",
	}); err != nil {
		t.Fatalf("update to default: %v", err)
	}
	def, err = repo.GetDefault(ctx, tenantA)
	if err != nil || def.ID != id1 {
		t.Fatalf("GetDefault after update = %+v (%v); want cal_one", def, err)
	}
	c2, err := repo.GetByID(ctx, tenantA, id2)
	if err != nil || c2.IsDefault {
		t.Fatalf("cal 2 = %+v (%v); want IsDefault=false", c2, err)
	}

	// Another tenant's default is unaffected and isolated.
	if _, err := repo.Create(ctx, createInput(tenantB, "cal_b", true)); err != nil {
		t.Fatalf("create tenant-b default: %v", err)
	}
	defA, err := repo.GetDefault(ctx, tenantA)
	if err != nil || defA.ID != id1 {
		t.Fatalf("tenant-a default drifted after tenant-b create: %+v (%v)", defA, err)
	}

	// Deleting the default leaves the tenant with NO default (wall-clock fallback).
	if err := repo.SoftDelete(ctx, tenantA, id1); err != nil {
		t.Fatalf("delete default: %v", err)
	}
	if _, err := repo.GetDefault(ctx, tenantA); !errors.Is(err, calmeta.ErrCalendarNotFound) {
		t.Fatalf("GetDefault after deleting default err = %v; want ErrCalendarNotFound", err)
	}
}
