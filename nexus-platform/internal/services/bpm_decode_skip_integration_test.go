//go:build integration

package services

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"nexus/platform/internal/bpm"
)

// TestBPM_UndecodableRowSkippedLoudly is DAILY-QUEUE #68 against a real MySQL. Both JSON columns
// (history, process_snapshot) are MySQL JSON, so the corruption that reaches the decoder in
// practice is VALID JSON of the WRONG SHAPE — an object where an array belongs. Before the fix,
// every list read `continue`d past such a row with no log: the approval vanished from the
// approver's inbox, from the record's approvals tab and from the SLA sweep, and nothing anywhere
// said so. The row is still skipped (one bad row must not blind the reader to the rest), but each
// read now names it in one structured error line.
func TestBPM_UndecodableRowSkippedLoudly(t *testing.T) {
	_, sdb := o3aSetup(t)
	ctx := context.Background()
	core, logs := observer.New(zapcore.InfoLevel)
	repo := bpm.NewSQLRepository(sdb, zap.New(core))

	const tenant = o3aTenantA
	const approver = "22222222-0000-0000-0000-000000006801"
	const record = "33333333-0000-0000-0000-000000006801"
	// The corrupt row sits on its OWN record: since Migration B (uq_ai_open) a record cannot hold two
	// pending instances, so the two can no longer be planted side by side on one.
	const recordBad = "33333333-0000-0000-0000-000000006802"
	const badInstance = "44444444-0000-0000-0000-000000006801"
	const badProcess = "55555555-0000-0000-0000-000000006801"
	past := time.Now().UTC().Add(-time.Hour)

	goodID, err := repo.CreateInstance(ctx, bpm.CreateInstanceInput{
		TenantID: tenant, ProcessID: "66666666-0000-0000-0000-000000006801", EntityName: "crm_quotes", RecordID: record,
		CurrentApproverType: bpm.ApproverUser, CurrentApproverID: approver, SLADueAt: &past,
		Definition: &bpm.Definition{Steps: []bpm.Step{{ApproverType: bpm.ApproverUser, ApproverID: approver}}},
	})
	if err != nil {
		t.Fatalf("CreateInstance (good): %v", err)
	}
	if _, err := sdb.Exec(`INSERT INTO sys_approval_instances
		(id, tenant_id, process_id, process_snapshot, entity_name, record_id, current_step, status,
		 current_approver_type, current_approver_id, sla_due_at, escalated, history)
		VALUES (?, ?, ?, NULL, 'crm_quotes', ?, 0, 'pending', 'user', ?, ?, 0, '{"step":0}')`,
		badInstance, tenant, badProcess, recordBad, approver, past); err != nil {
		t.Fatalf("insert wrong-shape instance: %v", err)
	}
	if _, err := repo.CreateProcess(ctx, bpm.CreateProcessInput{
		TenantID: tenant, Name: "good", EntityName: "crm_quotes", Trigger: bpm.TriggerAfterCreate,
		Steps: []byte(`[{"approverType":"user","approverId":"` + approver + `"}]`), IsActive: true,
	}); err != nil {
		t.Fatalf("CreateProcess (good): %v", err)
	}
	if _, err := sdb.Exec(`INSERT INTO sys_approval_processes
		(id, tenant_id, name, entity_name, trigger_event, criteria, steps, outcome, is_active)
		VALUES (?, ?, 'bad', 'crm_quotes', 'after_create', NULL, '{"approverType":"user"}', NULL, 1)`,
		badProcess, tenant); err != nil {
		t.Fatalf("insert wrong-shape process: %v", err)
	}

	// skipped asserts exactly one structured line for op naming the skipped row, then resets.
	skipped := func(op, idKey, id string) {
		t.Helper()
		var hits int
		for _, e := range logs.TakeAll() {
			f := e.ContextMap()
			if f["op"] != op {
				continue
			}
			hits++
			if e.Level != zapcore.ErrorLevel || f[idKey] != id || f["tenant_id"] != tenant || f["error"] == nil {
				t.Errorf("%s: log line = %s %v; want error level naming %s=%s tenant_id=%s with an error", op, e.Level, f, idKey, id, tenant)
			}
		}
		if hits != 1 {
			t.Errorf("%s: %d log lines for the skipped row; want exactly 1 — skipping is only acceptable if it is loud", op, hits)
		}
	}

	pending, err := repo.ListPendingForUser(ctx, tenant, approver, nil, nil)
	if err != nil {
		t.Fatalf("ListPendingForUser: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != goodID {
		t.Fatalf("inbox = %d rows; want only the good instance (one corrupt row must not blind the rest)", len(pending))
	}
	skipped("ListPendingForUser", "instance_id", badInstance)

	byRecord, err := repo.ListByRecord(ctx, tenant, "crm_quotes", record)
	if err != nil {
		t.Fatalf("ListByRecord: %v", err)
	}
	if len(byRecord) != 1 || byRecord[0].ID != goodID {
		t.Fatalf("record approvals = %d rows; want only the good instance", len(byRecord))
	}
	byBadRecord, err := repo.ListByRecord(ctx, tenant, "crm_quotes", recordBad)
	if err != nil {
		t.Fatalf("ListByRecord (corrupt record): %v", err)
	}
	if len(byBadRecord) != 0 {
		t.Fatalf("corrupt record approvals = %d rows; want the undecodable one skipped", len(byBadRecord))
	}
	skipped("ListByRecord", "instance_id", badInstance)

	breached, err := repo.ListBreachedForTenant(ctx, tenant, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListBreachedForTenant: %v", err)
	}
	if len(breached) != 1 || breached[0].ID != goodID {
		t.Fatalf("SLA sweep = %d rows; want only the good instance", len(breached))
	}
	skipped("ListBreachedForTenant", "instance_id", badInstance)

	procs, err := repo.ListActiveProcesses(ctx, tenant, "crm_quotes", bpm.TriggerAfterCreate)
	if err != nil {
		t.Fatalf("ListActiveProcesses: %v", err)
	}
	if len(procs) != 1 || procs[0].Name != "good" {
		t.Fatalf("active processes = %d; want only the good one", len(procs))
	}
	// A corrupt process row means that approval never arms again — the same silent vanishing,
	// one step earlier.
	skipped("ListActiveProcesses", "process_id", badProcess)
}
