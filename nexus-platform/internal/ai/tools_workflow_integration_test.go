//go:build integration

// Integration tests for the M7 workflow tool against the REAL *metadata.Repository and the REAL
// services.WorkflowService. They prove what the in-memory fake cannot: (a) propose_workflow
// writes NOTHING to sys_workflows, and (b) VALIDATOR PARITY — the exact payload a propose
// buffered is accepted by the real CreateWorkflow apply path (accept parity), and an invalid
// payload is rejected by BOTH sides with the SAME ValidationError code (reject parity) — one
// function body: services.buildWorkflowRule via ValidateWorkflowConfig.
//
// Run with (throwaway DB migrated per the gate recipe — single-source TEST_DATABASE_URL, B7):
//
//	mysql -h127.0.0.1 -uroot -e "CREATE DATABASE IF NOT EXISTS nexus_it"
//	/opt/homebrew/bin/migrate -path migrations -database "mysql://root@tcp(127.0.0.1:3306)/nexus_it" up
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_it?parseTime=true&multiStatements=true&charset=utf8mb4" \
//	  go test -tags=integration ./internal/ai/ -run TestToolsWorkflow_IT -count=1
package ai

import (
	"context"
	"errors"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

const (
	wfITTenantA = "aiaiaiai-m7m7-4444-8888-tenanta00001"
	wfITTenantB = "aiaiaiai-m7m7-4444-8888-tenantb00002"
	wfITEntity  = "it_ai_m7_tickets" // 16 chars — within SanitizeIdentifier's 26-char cap
)

// cleanupWorkflowIT hard-deletes every row the suite seeds, keyed by its throwaway tenant IDs,
// so reruns are idempotent and nothing leaks into other suites sharing the DB.
func cleanupWorkflowIT(gdb *gorm.DB) {
	gdb.Exec(`DELETE FROM sys_workflows WHERE tenant_id IN (?, ?)`, wfITTenantA, wfITTenantB)
	gdb.Exec(`DELETE FROM sys_fields WHERE tenant_id IN (?, ?)`, wfITTenantA, wfITTenantB)
	gdb.Exec(`DELETE FROM sys_entities WHERE tenant_id IN (?, ?)`, wfITTenantA, wfITTenantB)
}

// seedWorkflowIT registers the entity under both tenants; tenant A's copy carries a writable
// picklist field (status — the natural field_update target) plus an auto_number field
// (ticket_no — the WF-01 readonly-target probe) so both parity directions are real.
func seedWorkflowIT(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	cleanupWorkflowIT(gdb)
	t.Cleanup(func() { cleanupWorkflowIT(gdb) })

	seed := []struct {
		entID, tenant string
	}{
		{"itai-m7e1-0000-0000-000000000001", wfITTenantA},
		{"itai-m7e2-0000-0000-000000000002", wfITTenantB},
	}
	for _, s := range seed {
		ent := metadata.EntityDefinition{
			ID:        s.entID,
			TenantID:  s.tenant,
			Name:      wfITEntity,
			Label:     "M7 IT tickets",
			DataTable: "obj_" + wfITEntity,
		}
		if err := gdb.Create(&ent).Error; err != nil {
			t.Fatalf("seed entity for %s: %v", s.tenant, err)
		}
	}

	status := `{"values":["open","closed"]}`
	fields := []metadata.FieldDefinition{
		{ID: "itai-m7f1-0000-0000-000000000001", EntityID: seed[0].entID, TenantID: wfITTenantA,
			Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: &status, FieldOrder: 10},
		{ID: "itai-m7f2-0000-0000-000000000002", EntityID: seed[0].entID, TenantID: wfITTenantA,
			Name: "ticket_no", Label: "Số ticket", FieldType: "auto_number", FieldOrder: 20},
	}
	for i := range fields {
		if err := gdb.Create(&fields[i]).Error; err != nil {
			t.Fatalf("seed field %s: %v", fields[i].Name, err)
		}
	}
}

// draftToCreateInput maps a buffered DraftWorkflow onto the apply path's CreateWorkflowInput
// EXACTLY the way the FE Apply button maps it onto the POST /admin/workflows body (1:1 fields) —
// so the parity assertion exercises the same payload on both sides.
func draftToCreateInput(d *DraftWorkflow) services.CreateWorkflowInput {
	in := services.CreateWorkflowInput{
		Name:       d.Name,
		EntityName: d.EntityName,
		Trigger:    d.Trigger,
		IsActive:   &d.IsActive,
	}
	if d.Condition != nil {
		in.Match = d.Condition.Match
		for _, r := range d.Condition.Rules {
			in.Rules = append(in.Rules, services.WorkflowRuleInput{Field: r.Field, Op: r.Op, Value: r.Value})
		}
	}
	for _, a := range d.Actions {
		in.Actions = append(in.Actions, services.WorkflowActionInput{
			Type: a.Type, Field: a.Field, Value: a.Value,
			UserID: a.UserID, Title: a.Title, Message: a.Message,
			To: a.To, Subject: a.Subject, Body: a.Body,
		})
	}
	return in
}

func TestToolsWorkflow_IT_ProposeParity_CreateWorkflowRoundTrip(t *testing.T) {
	repo, gdb := newRelITReader(t) // same single-source DSN helper as the M5/M6 suites
	sqlxDB := connectAI(t)         // sqlx over the SAME DSN for the real workflow repository
	seedWorkflowIT(t, gdb)
	ctx := context.Background()

	execA := &toolExecutor{reader: repo, tenantID: wfITTenantA,
		docs: &DocReaders{Workflows: services.NewWorkflowService(workflow.NewSQLRepository(sqlxDB)).WithEntityChecker(repo)}}

	// Happy path: condition + safe actions (field_update on a writable field, in_app to $owner).
	goodArgs := `{
		"name": "Đóng ticket",
		"entity": "` + wfITEntity + `",
		"trigger": "after_update",
		"condition": {"match": "and", "rules": [{"field": "status", "op": "eq", "value": "closed"}]},
		"actions": [
			{"type": "field_update", "field": "status", "value": "closed"},
			{"type": "in_app_notification", "userId": "$owner", "title": "Đóng", "message": "Ticket đã đóng"}
		]
	}`
	res := execA.execute(ctx, "propose_workflow", goodArgs)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("propose_workflow should succeed against real metadata: %s", res)
	}
	if len(execA.draft) != 1 || execA.draft[0].Kind != "workflow" || execA.draft[0].Workflow == nil {
		t.Fatalf("expected 1 buffered workflow draft, got %+v", execA.draft)
	}

	// HITL: nothing was written — the draft is a proposal, not a sys_workflows row.
	var n int64
	if err := gdb.Raw(`SELECT COUNT(*) FROM sys_workflows WHERE tenant_id = ?`, wfITTenantA).
		Scan(&n).Error; err != nil {
		t.Fatalf("count check: %v", err)
	}
	if n != 0 {
		t.Fatalf("propose_workflow must NOT write sys_workflows; found %d rows", n)
	}

	// PARITY (accept side): applying the buffered draft VERBATIM through the real CreateWorkflow —
	// the apply path behind POST /admin/workflows, with the WF-01 guard armed via the metadata
	// repo as entity checker — must succeed, because the tool validated with the SAME function
	// body (buildWorkflowRule via ValidateWorkflowConfig) CreateWorkflow runs.
	svc := services.NewWorkflowService(workflow.NewSQLRepository(sqlxDB)).WithEntityChecker(repo)
	rec, err := svc.CreateWorkflow(ctx, wfITTenantA, draftToCreateInput(execA.draft[0].Workflow))
	if err != nil {
		t.Fatalf("parity broken: CreateWorkflow rejected a rule propose_workflow accepted: %v", err)
	}
	if rec == nil || rec.ID == "" {
		t.Fatalf("CreateWorkflow returned no persisted row: %+v", rec)
	}

	// PARITY (reject side): a field_update targeting the auto_number field is rejected by the
	// TOOL with RECORD_AUTONUMBER_READONLY (WF-01)...
	badArgs := `{"name":"Ghi đè mã","entity":"` + wfITEntity + `","trigger":"after_update",
		"actions":[{"type":"field_update","field":"ticket_no","value":"T-999"}]}`
	resBad := execA.execute(ctx, "propose_workflow", badArgs)
	okBad, mBad := parseToolResult(t, resBad)
	if okBad {
		t.Fatalf("propose_workflow must reject an auto_number field_update target: %s", resBad)
	}
	if errStr, _ := mBad["error"].(string); !strings.Contains(errStr, "RECORD_AUTONUMBER_READONLY") {
		t.Fatalf("expected RECORD_AUTONUMBER_READONLY from the tool, got %q", errStr)
	}
	if len(execA.draft) != 1 { // still only the good draft
		t.Fatalf("rejected proposal must not buffer, got %+v", execA.draft)
	}
	// ...and by the REAL CreateWorkflow with the SAME code — both sides run one validator body.
	_, errCreate := svc.CreateWorkflow(ctx, wfITTenantA, services.CreateWorkflowInput{
		Name: "Ghi đè mã", EntityName: wfITEntity, Trigger: "after_update",
		Actions: []services.WorkflowActionInput{{Type: "field_update", Field: "ticket_no", Value: "T-999"}},
	})
	var ve *services.ValidationError
	if !errors.As(errCreate, &ve) || ve.Code != "RECORD_AUTONUMBER_READONLY" {
		t.Fatalf("parity broken: CreateWorkflow should reject with RECORD_AUTONUMBER_READONLY, got %v", errCreate)
	}

	// Final ledger: exactly the ONE applied rule exists (the propose calls added nothing).
	if err := gdb.Raw(`SELECT COUNT(*) FROM sys_workflows WHERE tenant_id = ?`, wfITTenantA).
		Scan(&n).Error; err != nil {
		t.Fatalf("final count check: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 sys_workflows row (the applied one), found %d", n)
	}
}

func TestToolsWorkflow_IT_TenantIsolation(t *testing.T) {
	repo, gdb := newRelITReader(t)
	seedWorkflowIT(t, gdb)
	ctx := context.Background()

	// Tenant B owns an entity with the SAME name but NONE of tenant A's fields — a rule valid
	// for tenant A must fail for tenant B on the WF-01 unknown-field guard, proving the tool
	// resolved fieldTypes through tenant B's rows only (never tenant A's).
	execB := &toolExecutor{reader: repo, tenantID: wfITTenantB}
	args := `{"name":"r","entity":"` + wfITEntity + `","trigger":"after_update",
		"actions":[{"type":"field_update","field":"status","value":"closed"}]}`
	ok, m := parseToolResult(t, execB.execute(ctx, "propose_workflow", args))
	if ok {
		t.Fatal("tenant B must not validate against tenant A's fields")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "RECORD_UNKNOWN_FIELD") {
		t.Fatalf("expected RECORD_UNKNOWN_FIELD (tenant B has no 'status'), got %q", errStr)
	}

	// And an entity that exists ONLY under other tenants resolves as missing.
	execA := &toolExecutor{reader: repo, tenantID: wfITTenantA}
	resMissing := execA.execute(ctx, "propose_workflow",
		`{"name":"r","entity":"never_seeded_entity","trigger":"after_create",
		"actions":[{"type":"in_app_notification","userId":"$owner","message":"x"}]}`)
	if okM, mM := parseToolResult(t, resMissing); okM {
		t.Fatal("propose_workflow on a missing entity must fail")
	} else if errStr, _ := mM["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
		t.Fatalf("expected ENTITY_NOT_FOUND, got %q", errStr)
	}
}
