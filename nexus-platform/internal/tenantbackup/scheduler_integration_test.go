//go:build integration

package tenantbackup

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/storage"
)

var schedCfg = ScheduleConfig{HourLocal: 2, KeepPreRestore: 5, FailedRetentionDays: 7}

// verStore bumps the schema version the service sees WITHOUT touching schema_migrations (a second
// test of the package reads that table).
type verStore struct {
	*GormStore
	bump int64
}

func (v *verStore) CurrentSchema(ctx context.Context) (int64, bool, error) {
	n, d, err := v.GormStore.CurrentSchema(ctx)
	return n + v.bump, d, err
}

func (e *env) schedSvc(store Store) *Service {
	e.t.Helper()
	runner := NewRunner(nil)
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = runner.Stop(ctx)
	})
	s := NewService(store, NewSqlxSnapshots(e.db), e.blobs, e.backups, runner, e.aud, Config{Dir: e.backupsDir, MinFreeBytes: 1}, nil).
		WithSchedule(e.store, schedCfg)
	s.freeBytes = func(string) (uint64, error) { return 1 << 40, nil }
	return s
}

func (e *env) liveOf(tenantID string) []Backup {
	e.t.Helper()
	rows, err := e.store.Live(context.Background(), tenantID)
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func (e *env) countScheduled(tenantID string) (n int) {
	for _, b := range e.liveOf(tenantID) {
		if b.Kind == KindScheduled && b.Status == StatusReady {
			n++
		}
	}
	return n
}

func TestSchedule_TwoReplicasSameTickMakeOneBackup(t *testing.T) {
	e := newEnv(t, nil)
	s1, s2 := e.schedSvc(e.store), e.schedSvc(e.store)
	now := time.Date(2026, 10, 14, 3, 0, 0, 0, ictZone)
	var wg sync.WaitGroup
	for _, s := range []*Service{s1, s2} {
		wg.Add(1)
		go func() { defer wg.Done(); s.sweepTenant(context.Background(), e.A.ID, now) }()
	}
	wg.Wait()
	rows := e.liveOf(e.A.ID)
	if len(rows) != 1 || rows[0].Kind != KindScheduled || rows[0].Status != StatusReady ||
		rows[0].ScheduleSlot == nil || *rows[0].ScheduleSlot != "d:2026-10-14" || rows[0].CreatedBy != nil {
		t.Fatalf("two replicas must leave exactly one ready scheduled d: backup by the schedule, got %+v", rows)
	}
	// A third pass the same day is a no-op.
	s1.sweepTenant(context.Background(), e.A.ID, now.Add(time.Hour))
	if got := len(e.liveOf(e.A.ID)); got != 1 {
		t.Fatalf("same-day pass must not add a backup, have %d", got)
	}
}

func TestSchedule_RetentionKeepsNewestNAndManualSurvives(t *testing.T) {
	e := newEnv(t, nil)
	s := e.schedSvc(e.store)
	ctx := context.Background()
	if err := e.store.SaveSchedule(ctx, e.A.ID, true, 2, e.opUser); err != nil {
		t.Fatal(err)
	}
	m, err := s.Create(ctx, CreateInput{TenantID: e.A.ID, Actor: Actor{TenantID: e.W.ID, UserID: e.opUser}})
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, e, e.A.ID, m.ID)

	var first Backup
	for day := 14; day <= 16; day++ {
		s.sweepTenant(ctx, e.A.ID, time.Date(2026, 10, day, 3, 0, 0, 0, ictZone))
		if day == 14 {
			for _, b := range e.liveOf(e.A.ID) {
				if b.Kind == KindScheduled {
					first = b
				}
			}
		}
		time.Sleep(5 * time.Millisecond) // distinct created_at
	}
	if got := e.countScheduled(e.A.ID); got != 2 {
		t.Fatalf("keep=2 after 3 days: want 2 scheduled backups, have %d", got)
	}
	if _, err := e.backups.Open(ctx, *first.StorageKey); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the oldest scheduled backup's file must be gone from the store, Open err = %v", err)
	}
	if _, err := e.store.Get(ctx, e.A.ID, first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the oldest row must be soft-deleted, Get err = %v", err)
	}
	if _, err := e.store.Get(ctx, e.A.ID, m.ID); err != nil {
		t.Errorf("the manual backup must survive: %v", err)
	}
	// The pruned day's slot stays taken: a pass on day 14 again must not retake it.
	s.sweepTenant(ctx, e.A.ID, time.Date(2026, 10, 14, 4, 0, 0, 0, ictZone))
	if got := e.countScheduled(e.A.ID); got != 2 {
		t.Errorf("a pruned day must not be re-taken, have %d scheduled", got)
	}
}

func TestSchedule_DisabledTakesNothingOthersStillDo(t *testing.T) {
	e := newEnv(t, nil)
	s := e.schedSvc(e.store)
	ctx := context.Background()
	if err := e.store.SaveSchedule(ctx, e.A.ID, false, 7, e.opUser); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 3, 0, 0, 0, ictZone)
	s.sweepTenant(ctx, e.A.ID, now)
	s.sweepTenant(ctx, e.B.ID, now)
	if n := len(e.liveOf(e.A.ID)); n != 0 {
		t.Errorf("disabled customer got %d backups", n)
	}
	if n := e.countScheduled(e.B.ID); n != 1 {
		t.Errorf("default (no row) customer must be backed up, have %d", n)
	}
}

func TestSchedule_CatchUpAfterSchemaChange(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 3, 0, 0, 0, ictZone)
	e.schedSvc(e.store).sweepTenant(ctx, e.A.ID, now) // the night's backup at the old version
	deployed := e.schedSvc(&verStore{GormStore: e.store, bump: 1})
	deployed.sweepTenant(ctx, e.A.ID, now.Add(30*time.Minute))
	slots := map[string]int64{}
	for _, b := range e.liveOf(e.A.ID) {
		slots[*b.ScheduleSlot] = b.SchemaVersion
	}
	if len(slots) != 2 {
		t.Fatalf("want the daily + one v: catch-up backup, got %v", slots)
	}
	deployed.sweepTenant(ctx, e.A.ID, now.Add(time.Hour))
	if n := len(e.liveOf(e.A.ID)); n != 2 {
		t.Errorf("the catch-up must happen once, have %d backups", n)
	}
}

func TestSchedule_LowDiskSkipsWithoutAFailedRow(t *testing.T) {
	e := newEnv(t, nil)
	s := e.schedSvc(e.store)
	s.freeBytes = func(string) (uint64, error) { return 0, nil }
	s.sweepTenant(context.Background(), e.A.ID, time.Date(2026, 10, 14, 3, 0, 0, 0, ictZone))
	if n := len(e.liveOf(e.A.ID)); n != 0 {
		t.Errorf("a backup that could not start must leave no row, have %d", n)
	}
}

func TestDelete_ManualBackupAndGuards(t *testing.T) {
	e := newEnv(t, nil)
	s := e.schedSvc(e.store)
	ctx := context.Background()
	op := Actor{TenantID: e.W.ID, UserID: e.opUser}
	m, err := s.Create(ctx, CreateInput{TenantID: e.A.ID, Actor: op})
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, e, e.A.ID, m.ID)
	// Another customer's id under this path is "not found", the file is untouched.
	if err := s.Delete(ctx, e.B.ID, op, m.ID); err == nil || err.(*Error).Code != "BACKUP_NOT_FOUND" {
		t.Fatalf("cross-tenant delete: want BACKUP_NOT_FOUND, got %v", err)
	}
	row, _ := e.store.Get(ctx, e.A.ID, m.ID)
	// In use as a restore source ⇒ 409.
	e.exec("INSERT INTO sys_tenant_backups (id, tenant_id, kind, status, format_version, schema_version, restore_source_id, restore_status) VALUES (UUID(), ?, 'pre_restore', 'ready', 1, 1, ?, 'running')", e.A.ID, m.ID)
	if err := s.Delete(ctx, e.A.ID, op, m.ID); err == nil || err.(*Error).Code != "BACKUP_DELETE_IN_USE" {
		t.Fatalf("restore source: want BACKUP_DELETE_IN_USE, got %v", err)
	}
	if _, err := e.backups.Open(ctx, *row.StorageKey); err != nil {
		t.Fatalf("a refused delete must keep the file: %v", err)
	}
	e.exec("UPDATE sys_tenant_backups SET restore_status = 'done' WHERE tenant_id = ?", e.A.ID)
	if err := s.Delete(ctx, e.A.ID, op, m.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := e.backups.Open(ctx, *row.StorageKey); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("file must be gone: %v", err)
	}
	if _, err := e.store.Get(ctx, e.A.ID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("row must be hidden: %v", err)
	}
}

func TestSchedule_PutGetRoundTripAndValidation(t *testing.T) {
	e := newEnv(t, nil)
	s := e.schedSvc(e.store)
	ctx := context.Background()
	op := Actor{TenantID: e.W.ID, UserID: e.opUser}
	v, err := s.GetSchedule(ctx, e.A.ID)
	if err != nil || !v.Enabled || v.KeepScheduled != 7 || v.HourLocal != 2 {
		t.Fatalf("default schedule = %+v, %v", v, err)
	}
	tr, f := true, false
	for _, keep := range []int{0, 31} {
		k := keep
		if _, err := s.PutSchedule(ctx, e.A.ID, op, &tr, &k); err == nil || err.(*Error).Code != "BACKUP_SCHEDULE_INVALID" {
			t.Errorf("keep=%d: want BACKUP_SCHEDULE_INVALID, got %v", keep, err)
		}
	}
	if _, err := s.PutSchedule(ctx, e.A.ID, op, nil, new(int)); err == nil {
		t.Error("missing enabled must be refused")
	}
	k := 3
	if v, err = s.PutSchedule(ctx, e.A.ID, op, &f, &k); err != nil || v.Enabled || v.KeepScheduled != 3 || v.NextDueAt != nil {
		t.Fatalf("saved schedule = %+v, %v", v, err)
	}
	k = 5
	if v, err = s.PutSchedule(ctx, e.A.ID, op, &tr, &k); err != nil || !v.Enabled || v.KeepScheduled != 5 || v.NextDueAt == nil {
		t.Fatalf("second PUT must upsert, got %+v, %v", v, err)
	}
}

func waitReady(t *testing.T, e *env, tenantID, id string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := e.store.Get(context.Background(), tenantID, id); err == nil && b.Status != StatusRunning {
			if b.Status != StatusReady {
				t.Fatalf("backup %s ended %s", id, b.Status)
			}
			return
		}
	}
	t.Fatalf("backup %s never finished", id)
}
