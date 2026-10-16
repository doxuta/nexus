//go:build integration

// specs/043 S9a — `bpm-open-key-durable-arm`: one record never has two approvals open at once, proven
// against real MySQL (tasks T144, T145, T146).
//
//   - Migration B adds lane / open_key / uq_ai_open and the idempotency columns, goes down/up
//     cleanly, and REFUSES (never deletes) when a record already holds two pending approvals;
//   - a burst of simultaneous starts for one record yields exactly one instance, and the rest read
//     as "already waiting" (bpm.ErrAlreadyPending), not as an error;
//   - a settled instance frees the record again;
//   - a tracking-lane (independent) instance neither blocks nor is seen by the approval checks.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run 'TestMigrationB_|TestOpenKey_|TestHasOpenExclusive_'
package services

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/bpm"
)

const migBSuffix = "_bpm_open_key"

func migBFile(t *testing.T, dir string) string {
	t.Helper()
	for _, f := range migFiles(t, "."+dir+".sql") {
		if strings.HasSuffix(f, migBSuffix+"."+dir+".sql") {
			return f
		}
	}
	t.Fatalf("Migration B (%s.%s.sql) not found in %s", migBSuffix, dir, o3aMigrationsDir)
	return ""
}

// preMigBDB is a throwaway database migrated to just BEFORE Migration B.
func preMigBDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db := throwawayDB(t)
	stop := migBFile(t, "up")
	for _, f := range migFiles(t, ".up.sql") {
		if f >= stop {
			break
		}
		if err := execMigFile(t, db, f); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return db
}

func bpmBSchema(t *testing.T, db *sqlx.DB) (cols, idx int) {
	t.Helper()
	if err := db.Get(&cols, `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME IN ('lane','open_key','idempotency_key','request_hash')`); err != nil {
		t.Fatalf("column probe: %v", err)
	}
	if err := db.Get(&idx, `SELECT COUNT(DISTINCT INDEX_NAME) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_NAME = 'sys_approval_instances' AND INDEX_NAME IN ('uq_ai_open','uq_ai_idem')`); err != nil {
		t.Fatalf("index probe: %v", err)
	}
	return cols, idx
}

// TestMigrationB_PrecheckFailsOnDuplicatePending (T144): two pending approvals of one record — the
// old start-up race — make the migration refuse, change NOTHING, and delete nothing; once a person
// withdraws one it goes through.
func TestMigrationB_PrecheckFailsOnDuplicatePending(t *testing.T) {
	db := preMigBDB(t)
	const tenant = o3aTenantA
	proc := uuid.NewString()
	rec := uuid.NewString()
	first := plantInstance(t, db, legacyInstance{tenant: tenant, process: proc, entity: "quotes", record: rec})
	second := plantInstance(t, db, legacyInstance{tenant: tenant, process: proc, entity: "quotes", record: rec})
	// Not duplicates: a settled copy of the same record, the same record id in another tenant.
	plantInstance(t, db, legacyInstance{tenant: tenant, process: proc, entity: "quotes", record: rec, status: "approved"})
	plantInstance(t, db, legacyInstance{tenant: o3aTenantB, process: proc, entity: "quotes", record: rec})

	err := execMigFile(t, db, migBFile(t, "up"))
	if err == nil || !strings.Contains(err.Error(), "BPM-B ABORT") {
		t.Fatalf("up over a duplicate pair: err = %v; want BPM-B ABORT", err)
	}
	if cols, idx := bpmBSchema(t, db); cols != 0 || idx != 0 {
		t.Fatalf("a refused up changed the schema: cols=%d idx=%d; want 0/0", cols, idx)
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM sys_approval_instances`); err != nil || n != 4 {
		t.Fatalf("rows after a refused up = %d (%v); the migration must never delete data (want 4)", n, err)
	}

	// Nexus withdraws one of the pair by hand; the migration then goes through.
	if _, err := db.Exec(`UPDATE sys_approval_instances SET status = 'cancelled' WHERE id = ?`, second); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if err := execMigFile(t, db, migBFile(t, "up")); err != nil {
		t.Fatalf("up after withdrawing one: %v", err)
	}
	var openKey *string
	if err := db.Get(&openKey, `SELECT open_key FROM sys_approval_instances WHERE id = ?`, first); err != nil || openKey == nil || *openKey != "quotes/"+rec+"/x" {
		t.Fatalf("open_key of the surviving pending = %v (%v); want quotes/<record>/x", openKey, err)
	}
	if err := db.Get(&openKey, `SELECT open_key FROM sys_approval_instances WHERE id = ?`, second); err != nil || openKey != nil {
		t.Fatalf("open_key of the settled one = %v (%v); want NULL so it never blocks", openKey, err)
	}
}

// TestMigrationB_UpDownUp (T144): up twice and down twice are harmless; down refuses while an
// independent-lane row exists (dropping `lane` would turn it into a second approval).
func TestMigrationB_UpDownUp(t *testing.T) {
	db := preMigBDB(t)
	const tenant = o3aTenantA
	up, down := migBFile(t, "up"), migBFile(t, "down")
	for i := 1; i <= 2; i++ {
		if err := execMigFile(t, db, up); err != nil {
			t.Fatalf("up #%d: %v", i, err)
		}
		if cols, idx := bpmBSchema(t, db); cols != 4 || idx != 2 {
			t.Fatalf("after up #%d: cols=%d idx=%d; want 4/2", i, cols, idx)
		}
	}
	var notTenantFirst int
	if err := db.Get(&notTenantFirst, `SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_NAME = 'sys_approval_instances' AND INDEX_NAME IN ('uq_ai_open','uq_ai_idem') AND SEQ_IN_INDEX = 1 AND COLUMN_NAME <> 'tenant_id'`); err != nil || notTenantFirst != 0 {
		t.Fatalf("%d unique key(s) do not lead with tenant_id (%v)", notTenantFirst, err)
	}

	id := plantInstance(t, db, legacyInstance{tenant: tenant, process: uuid.NewString(), entity: "quotes"})
	if _, err := db.Exec(`UPDATE sys_approval_instances SET lane = 'independent' WHERE id = ?`, id); err != nil {
		t.Fatalf("plant independent: %v", err)
	}
	err := execMigFile(t, db, down)
	if err == nil || !strings.Contains(err.Error(), "BPM-B ABORT") {
		t.Fatalf("down with an independent row: err = %v; want BPM-B ABORT", err)
	}
	if cols, idx := bpmBSchema(t, db); cols != 4 || idx != 2 {
		t.Fatalf("a refused down changed the schema: cols=%d idx=%d", cols, idx)
	}
	if _, err := db.Exec(`DELETE FROM sys_approval_instances WHERE id = ?`, id); err != nil {
		t.Fatalf("clear: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if err := execMigFile(t, db, down); err != nil {
			t.Fatalf("down #%d: %v", i, err)
		}
		if cols, idx := bpmBSchema(t, db); cols != 0 || idx != 0 {
			t.Fatalf("after down #%d: cols=%d idx=%d; want 0/0", i, cols, idx)
		}
	}
	if err := execMigFile(t, db, up); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

func openKeyInput(tenant, entity, record string) bpm.CreateInstanceInput {
	return bpm.CreateInstanceInput{
		TenantID: tenant, ProcessID: uuid.NewString(), EntityName: entity, RecordID: record,
		CurrentApproverType: bpm.ApproverUser, CurrentApproverID: uuid.NewString(),
		Definition: &bpm.Definition{Steps: []bpm.Step{{ApproverType: bpm.ApproverUser, ApproverID: uuid.NewString()}}},
	}
}

// TestOpenKey_DuplicateArmIsIdempotent (T145): N starts of ONE record at the same instant — the
// event twice, the bpm_arm job beside the in-memory worker, a double-click — leave exactly one
// pending instance; the losers see ErrAlreadyPending. Settling it frees the record.
func TestOpenKey_DuplicateArmIsIdempotent(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	rec := uuid.NewString()

	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	ids := make([]string, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], errs[i] = st.repo.CreateInstance(ctx, openKeyInput(tenant, "quotes", rec))
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, bpm.ErrAlreadyPending):
		default:
			t.Fatalf("racer %d: %v; want success or ErrAlreadyPending", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d racers armed the record; want exactly 1", won)
	}
	var pending int
	if err := st.sdb.Get(&pending, `SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = ? AND record_id = ? AND status = 'pending'`, tenant, rec); err != nil || pending != 1 {
		t.Fatalf("pending rows = %d (%v); want 1", pending, err)
	}
	if open, err := st.repo.HasOpenExclusive(ctx, tenant, "quotes", rec); err != nil || !open {
		t.Fatalf("HasOpenExclusive = %v (%v); want true", open, err)
	}

	// Another tenant's same record id is a different record.
	if _, err := st.repo.CreateInstance(ctx, openKeyInput(o3aTenantB, "quotes", rec)); err != nil {
		t.Fatalf("same record id in another tenant: %v; want success", err)
	}

	// Settling frees the record: the next start is accepted.
	for i, id := range ids {
		if errs[i] == nil {
			if _, err := st.sdb.Exec(`UPDATE sys_approval_instances SET status = 'approved' WHERE tenant_id = ? AND id = ?`, tenant, id); err != nil {
				t.Fatalf("settle: %v", err)
			}
		}
	}
	if _, err := st.repo.CreateInstance(ctx, openKeyInput(tenant, "quotes", rec)); err != nil {
		t.Fatalf("start after the first settled: %v; want success", err)
	}
}

// TestHasOpenExclusive_IgnoresIndependentLane (T146, prepares S12b): a tracking-lane instance is
// invisible to the approval checks — HasOpenExclusive, the tab read — and does not block an approval
// of the same record; two of the SAME process still collide, two different processes do not.
func TestHasOpenExclusive_IgnoresIndependentLane(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	rec := uuid.NewString()
	procA, procB := uuid.NewString(), uuid.NewString()

	plantTracking := func(proc string) error {
		_, err := st.sdb.Exec(`INSERT INTO sys_approval_instances
			(id, tenant_id, process_id, entity_name, record_id, current_step, status, history, lane)
			VALUES (?, ?, ?, 'quotes', ?, 0, 'pending', '[]', 'independent')`, uuid.NewString(), tenant, proc, rec)
		return err
	}
	if err := plantTracking(procA); err != nil {
		t.Fatalf("plant tracking A: %v", err)
	}
	if open, err := st.repo.HasOpenExclusive(ctx, tenant, "quotes", rec); err != nil || open {
		t.Fatalf("HasOpenExclusive with only a tracking instance = %v (%v); want false", open, err)
	}
	if list, err := st.repo.ListByRecord(ctx, tenant, "quotes", rec); err != nil || len(list) != 0 {
		t.Fatalf("ListByRecord (the Duyệt tab) = %d rows (%v); want 0 — tracking never shows there", len(list), err)
	}
	if err := plantTracking(procB); err != nil {
		t.Fatalf("a second PROCESS on the tracking lane: %v; want allowed", err)
	}
	if err := plantTracking(procA); err == nil {
		t.Fatalf("the same process twice on the tracking lane was accepted; want the unique key to refuse")
	}
	if _, err := st.repo.CreateInstance(ctx, openKeyInput(tenant, "quotes", rec)); err != nil {
		t.Fatalf("an approval beside tracking instances: %v; want allowed", err)
	}
	if open, err := st.repo.HasOpenExclusive(ctx, tenant, "quotes", rec); err != nil || !open {
		t.Fatalf("HasOpenExclusive after the approval = %v (%v); want true", open, err)
	}
	if list, err := st.repo.ListByRecord(ctx, tenant, "quotes", rec); err != nil || len(list) != 1 {
		t.Fatalf("ListByRecord = %d rows (%v); want only the approval", len(list), err)
	}
}
