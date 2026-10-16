//go:build integration

// W6 HTTP-contract integration tests — the wire-level counterpart of the services suite
// (internal/services/w6_workflow_runs_integration_test.go): exact envelopes, error codes and
// status codes of the W6 surface, driven through the REAL router (AuthMiddleware →
// RequireTenant → RequireAdmin where applicable), against the shared MySQL from dsn().
//
//	POST /admin/workflows                    → WF-01 422 rejects (computed/assigned/unknown target)
//	GET  /admin/workflows/:id/runs           → paged run history envelope, 404 cross-tenant
//	GET  /admin/workflows/runs               → recent-across-rules envelope
//	POST /admin/workflows/:id/test           → dry-run trace, 404/422 errors
//	GET  /approvals/:id                      → approval detail, pending-mirror ACL, 404 no-leak
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_platform?..." \
//	  go test -tags=integration ./internal/handlers/ -run TestW6HTTP -race -v
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

const w6HTTPEntity = "it_w6_deals"
const w6HTTPOtherTenant = "abcdef00-0000-0000-0000-0000000000f6"

// newW6Router wires admin entity + dynamic record + workflow (with W6 run reader / record
// reader) + approval handlers behind the production route registration.
func newW6Router(t *testing.T) (*gin.Engine, *sqlx.DB, *workflow.SQLRepository, *bpm.SQLRepository) {
	t.Helper()
	sx, err := sqlx.Connect("mysql", dsn())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	gdb, err := gorm.Open(gormmysql.Open(dsn()), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	wfRepo := workflow.NewSQLRepository(sx)
	bpmRepo := bpm.NewSQLRepository(sx, nil)

	entitySvc := services.NewEntityService(dyn, meta)
	fieldSvc := services.NewFieldService(dyn, meta)
	recordSvc := services.NewRecordService(meta, dyn).WithSequenceClaimer(dyn)
	workflowSvc := services.NewWorkflowService(wfRepo).WithEntityChecker(meta).
		WithRunReader(wfRepo).WithRecordReader(dyn)
	approvalSvc := services.NewApprovalService(bpmRepo, bpmRepo, nil).WithEntityChecker(meta)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:    NewAdminEntityHandler(entitySvc, fieldSvc, zap.NewNop()),
		Record:   NewDynamicRecordHandler(recordSvc, zap.NewNop()),
		Workflow: NewAdminWorkflowHandler(workflowSvc, zap.NewNop()),
		Approval: NewApprovalHandler(approvalSvc, zap.NewNop()),
	}, zap.NewNop())
	return r, sx, wfRepo, bpmRepo
}

// w6Cleanup removes every W6 fixture row for both tenants.
func w6Cleanup(sx *sqlx.DB) {
	for _, tenant := range []string{testTenant, w6HTTPOtherTenant} {
		sx.Exec(`DELETE FROM sys_workflow_runs WHERE tenant_id = ?`, tenant)
		sx.Exec(`DELETE FROM sys_workflows WHERE tenant_id = ?`, tenant)
		sx.Exec(`DELETE FROM sys_approval_instances WHERE tenant_id = ?`, tenant)
		sx.Exec(`DELETE FROM sys_approval_processes WHERE tenant_id = ?`, tenant)
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name = ?`, tenant, w6HTTPEntity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, tenant, w6HTTPEntity)
	}
	sx.Exec("DROP TABLE IF EXISTS `obj_" + w6HTTPEntity + "`")
}

// w6DoAs performs a request as an arbitrary principal (tenant, user, role, admin flag) — the
// approval-detail ACL cases need NON-admin identities, which do()/doAs() cannot mint.
func w6DoAs(t *testing.T, r *gin.Engine, method, path string, sub auth.Subject, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		reader = bytes.NewReader(buf)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	tok, err := accessVerifier(t).IssueAccessToken(sub)
	if err != nil {
		t.Fatalf("w6DoAs: issue token: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// w6Entity creates the fixture entity (text status/stage + one auto_number) via the admin API.
func w6Entity(t *testing.T, r *gin.Engine) {
	t.Helper()
	body := map[string]any{
		"name":  w6HTTPEntity,
		"label": "W6 Deals",
		"fields": []map[string]any{
			{"name": "status", "label": "Status", "fieldType": "text"},
			{"name": "stage", "label": "Stage", "fieldType": "text"},
			{"name": "deal_no", "label": "Deal No", "fieldType": "auto_number",
				"options": map[string]any{"prefix": "DL-", "padding": 3}},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", body); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}
}

// w6Record creates one record and returns its id.
func w6Record(t *testing.T, r *gin.Engine, status string) string {
	t.Helper()
	w := do(t, r, http.MethodPost, "/api/v1/"+w6HTTPEntity, map[string]any{"status": status, "stage": "new"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create record: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	return created.Data.ID
}

// w6Rule creates a workflow rule via the admin API and returns its id.
func w6Rule(t *testing.T, r *gin.Engine) string {
	t.Helper()
	w := do(t, r, http.MethodPost, "/api/v1/admin/workflows", map[string]any{
		"name": "won-closes", "entityName": w6HTTPEntity, "trigger": "after_update",
		"condition": map[string]any{"rules": []map[string]any{{"field": "status", "op": "eq", "value": "won"}}},
		"actions":   []map[string]any{{"type": "field_update", "field": "stage", "value": "closed"}},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create workflow: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			Workflow struct {
				ID string `json:"id"`
			} `json:"workflow"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	return created.Data.Workflow.ID
}

func TestW6HTTP_WF01_CreateRejectsBadTargets(t *testing.T) {
	r, sx, _, _ := newW6Router(t)
	w6Cleanup(sx)
	t.Cleanup(func() { w6Cleanup(sx) })
	w6Entity(t, r)

	for _, tc := range []struct {
		field    string
		wantCode string
	}{
		{field: "deal_no", wantCode: "RECORD_AUTONUMBER_READONLY"},
		{field: "ghost", wantCode: "RECORD_UNKNOWN_FIELD"},
	} {
		w := do(t, r, http.MethodPost, "/api/v1/admin/workflows", map[string]any{
			"name": "bad", "entityName": w6HTTPEntity, "trigger": "after_update",
			"actions": []map[string]any{{"type": "field_update", "field": tc.field, "value": "x"}},
		})
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("field %q: status = %d body=%s; want 422", tc.field, w.Code, w.Body.String())
		}
		var resp struct {
			Code string `json:"code"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Code != tc.wantCode {
			t.Errorf("field %q: code = %s; want %s", tc.field, resp.Code, tc.wantCode)
		}
	}
}

func TestW6HTTP_RunsEnvelopeAndIsolation(t *testing.T) {
	r, sx, wfRepo, _ := newW6Router(t)
	w6Cleanup(sx)
	t.Cleanup(func() { w6Cleanup(sx) })
	w6Entity(t, r)
	wfID := w6Rule(t, r)

	// Seed 3 runs directly through the recorder (the async-engine recording itself is proven in
	// the services suite; this test pins the WIRE contract).
	base := time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)
	for i, st := range []workflow.RunStatus{workflow.RunSuccess, workflow.RunSkipped, workflow.RunFailed} {
		in := workflow.RunInput{
			TenantID: testTenant, WorkflowID: wfID, TriggerType: "after_update",
			RecordID: "rec-1", Status: st,
			StartedAt: base.Add(time.Duration(i) * time.Minute), FinishedAt: base.Add(time.Duration(i)*time.Minute + time.Second),
		}
		if st == workflow.RunFailed {
			in.Error = "boom"
		}
		if st == workflow.RunSuccess {
			in.ActionsResult = []byte(`[{"type":"field_update","field":"stage","ok":true}]`)
		}
		if err := wfRepo.InsertRun(t.Context(), in); err != nil {
			t.Fatalf("seed run: %v", err)
		}
	}

	// Paged list: newest first, camelCase, pagination echo.
	w := do(t, r, http.MethodGet, "/api/v1/admin/workflows/"+wfID+"/runs?limit=2&offset=0", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("runs: %d %s", w.Code, w.Body.String())
	}
	var runsResp struct {
		Data struct {
			Runs []struct {
				ID            string          `json:"id"`
				WorkflowID    string          `json:"workflowId"`
				TriggerType   string          `json:"triggerType"`
				RecordID      *string         `json:"recordId"`
				Status        string          `json:"status"`
				Error         *string         `json:"error"`
				ActionsResult json.RawMessage `json:"actionsResult"`
				StartedAt     time.Time       `json:"startedAt"`
			} `json:"runs"`
			Pagination struct {
				Limit  int `json:"limit"`
				Offset int `json:"offset"`
				Total  int `json:"total"`
			} `json:"pagination"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &runsResp)
	if len(runsResp.Data.Runs) != 2 || runsResp.Data.Pagination.Total != 3 ||
		runsResp.Data.Pagination.Limit != 2 || runsResp.Data.Pagination.Offset != 0 {
		t.Fatalf("envelope = %s", w.Body.String())
	}
	// Newest first: the failed run (latest started_at) leads.
	if runsResp.Data.Runs[0].Status != "failed" || runsResp.Data.Runs[0].Error == nil {
		t.Errorf("runs[0] = %+v; want the newest (failed) run", runsResp.Data.Runs[0])
	}
	if runsResp.Data.Runs[0].WorkflowID != wfID || runsResp.Data.Runs[0].RecordID == nil {
		t.Errorf("runs[0] missing workflowId/recordId: %+v", runsResp.Data.Runs[0])
	}

	// Offset page 2.
	w = do(t, r, http.MethodGet, "/api/v1/admin/workflows/"+wfID+"/runs?limit=2&offset=2", nil)
	json.Unmarshal(w.Body.Bytes(), &runsResp)
	if len(runsResp.Data.Runs) != 1 || runsResp.Data.Runs[0].Status != "success" {
		t.Errorf("offset page = %s", w.Body.String())
	}
	if runsResp.Data.Runs[0].ActionsResult == nil {
		t.Errorf("success run must carry actionsResult")
	}

	// Recent across workflows.
	w = do(t, r, http.MethodGet, "/api/v1/admin/workflows/runs?limit=10", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("recent runs: %d %s", w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &runsResp)
	if len(runsResp.Data.Runs) != 3 {
		t.Errorf("recent runs = %d; want 3", len(runsResp.Data.Runs))
	}

	// Cross-tenant probe: another tenant asking for THIS rule's runs → 404, zero leak.
	w = doAs(t, r, http.MethodGet, "/api/v1/admin/workflows/"+wfID+"/runs", w6HTTPOtherTenant, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant runs: %d; want 404", w.Code)
	}
	// Unknown workflow → 404.
	w = do(t, r, http.MethodGet, "/api/v1/admin/workflows/00000000-0000-0000-0000-000000000000/runs", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown workflow runs: %d; want 404", w.Code)
	}
}

// Queue #297: the automation panel of ONE app shows "Chạy gần đây" of that app's rules only —
// ?workflowIds= narrows the tenant-wide recent read to the rules the panel lists. Pinned: only the
// named rules' runs come back (newest first), present-but-empty means none (never "all"), the
// tenant stays pinned (another tenant naming this rule's id gets its OWN runs, i.e. none), and an
// oversized list is refused at the boundary.
func TestW6HTTP_RecentRunsScopedToRules(t *testing.T) {
	r, sx, wfRepo, _ := newW6Router(t)
	w6Cleanup(sx)
	t.Cleanup(func() { w6Cleanup(sx) })
	w6Entity(t, r)
	ruleA := w6Rule(t, r)
	const ruleB = "0b0b0b0b-0000-0000-0000-00000000297b" // another rule of the tenant (runs carry no FK)

	base := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	seed := func(tenant, wfID string, minute int) {
		t.Helper()
		in := workflow.RunInput{
			TenantID: tenant, WorkflowID: wfID, TriggerType: "after_update", RecordID: "rec-1",
			Status:    workflow.RunSuccess,
			StartedAt: base.Add(time.Duration(minute) * time.Minute), FinishedAt: base.Add(time.Duration(minute)*time.Minute + time.Second),
		}
		if err := wfRepo.InsertRun(t.Context(), in); err != nil {
			t.Fatalf("seed run: %v", err)
		}
	}
	seed(testTenant, ruleA, 0)
	seed(testTenant, ruleB, 1)
	seed(testTenant, ruleA, 2)
	seed(w6HTTPOtherTenant, ruleA, 3) // same rule id, other tenant: must never surface

	type recent struct {
		Data struct {
			Runs []struct {
				WorkflowID string `json:"workflowId"`
				TenantID   string `json:"tenantId"`
			} `json:"runs"`
		} `json:"data"`
	}
	get := func(path, tenant string) (int, recent, string) {
		t.Helper()
		w := doAs(t, r, http.MethodGet, path, tenant, nil)
		var out recent
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out, w.Body.String()
	}
	wfIDs := func(rc recent) []string {
		ids := make([]string, 0, len(rc.Data.Runs))
		for _, run := range rc.Data.Runs {
			ids = append(ids, run.WorkflowID)
		}
		return ids
	}

	cases := []struct {
		name   string
		path   string
		tenant string
		want   []string
	}{
		{"no param = every rule of the tenant", "/api/v1/admin/workflows/runs?limit=10", testTenant, []string{ruleA, ruleB, ruleA}},
		{"one rule", "/api/v1/admin/workflows/runs?limit=10&workflowIds=" + ruleA, testTenant, []string{ruleA, ruleA}},
		{"two rules, spaces and a trailing comma tolerated", "/api/v1/admin/workflows/runs?limit=10&workflowIds=" + ruleB + ",%20" + ruleA + ",", testTenant, []string{ruleA, ruleB, ruleA}},
		{"limit still applies", "/api/v1/admin/workflows/runs?limit=1&workflowIds=" + ruleA, testTenant, []string{ruleA}},
		{"present but empty = no rule = no run", "/api/v1/admin/workflows/runs?limit=10&workflowIds=", testTenant, []string{}},
		{"other tenant naming this rule sees only its own run", "/api/v1/admin/workflows/runs?limit=10&workflowIds=" + ruleA, w6HTTPOtherTenant, []string{ruleA}},
		{"unknown rule = nothing", "/api/v1/admin/workflows/runs?limit=10&workflowIds=00000000-0000-0000-0000-000000000000", testTenant, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, body := get(tc.path, tc.tenant)
			if code != http.StatusOK {
				t.Fatalf("status = %d %s", code, body)
			}
			if got := wfIDs(out); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("workflowIds = %v; want %v (%s)", got, tc.want, body)
			}
			for _, run := range out.Data.Runs {
				if run.TenantID != tc.tenant {
					t.Fatalf("run of tenant %s leaked to %s", run.TenantID, tc.tenant)
				}
			}
		})
	}

	// Boundary: more ids than the cap is refused, not truncated into a wrong answer.
	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
	}
	w := do(t, r, http.MethodGet, "/api/v1/admin/workflows/runs?workflowIds="+strings.Join(many, ","), nil)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"WORKFLOW_RUNS_FILTER_INVALID"`) {
		t.Fatalf("101 ids: %d %s; want 422 WORKFLOW_RUNS_FILTER_INVALID", w.Code, w.Body.String())
	}
}

func TestW6HTTP_DryRunTrace(t *testing.T) {
	r, sx, _, _ := newW6Router(t)
	w6Cleanup(sx)
	t.Cleanup(func() { w6Cleanup(sx) })
	w6Entity(t, r)
	wfID := w6Rule(t, r)
	recID := w6Record(t, r, "won")

	w := do(t, r, http.MethodPost, "/api/v1/admin/workflows/"+wfID+"/test", map[string]any{"recordId": recID})
	if w.Code != http.StatusOK {
		t.Fatalf("test: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Test struct {
				WorkflowID string `json:"workflowId"`
				EntityName string `json:"entityName"`
				RecordID   string `json:"recordId"`
				Trigger    string `json:"trigger"`
				Matched    bool   `json:"matched"`
				Trace      []struct {
					Step      int            `json:"step"`
					Kind      string         `json:"kind"`
					Matched   *bool          `json:"matched"`
					Evaluated *bool          `json:"evaluated"`
					Detail    map[string]any `json:"detail"`
				} `json:"trace"`
			} `json:"test"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	tst := resp.Data.Test
	if !tst.Matched || tst.WorkflowID != wfID || tst.RecordID != recID || tst.EntityName != w6HTTPEntity {
		t.Fatalf("test payload = %s", w.Body.String())
	}
	if len(tst.Trace) != 3 { // trigger + condition + action
		t.Fatalf("trace = %s", w.Body.String())
	}
	if tst.Trace[0].Kind != "trigger" || tst.Trace[1].Kind != "condition" || tst.Trace[2].Kind != "action" {
		t.Errorf("trace kinds = %+v", tst.Trace)
	}
	if tst.Trace[1].Matched == nil || !*tst.Trace[1].Matched || tst.Trace[1].Detail["actual"] != "won" {
		t.Errorf("condition step = %+v", tst.Trace[1])
	}
	act := tst.Trace[2]
	if act.Evaluated == nil || !*act.Evaluated ||
		act.Detail["before"] != "new" || act.Detail["after"] != "closed" {
		t.Errorf("action preview = %+v; want before=new after=closed", act)
	}

	// The dry-run must NOT have mutated the record.
	w = do(t, r, http.MethodGet, "/api/v1/"+w6HTTPEntity+"/"+recID, nil)
	var got struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Data.Data["stage"] != "new" {
		t.Errorf("dry-run mutated the record: stage = %v", got.Data.Data["stage"])
	}

	// Errors: unknown workflow 404 · unknown record 404 · blank recordId 422.
	if w := do(t, r, http.MethodPost, "/api/v1/admin/workflows/00000000-0000-0000-0000-000000000000/test", map[string]any{"recordId": recID}); w.Code != http.StatusNotFound {
		t.Errorf("unknown workflow test: %d; want 404", w.Code)
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/workflows/"+wfID+"/test", map[string]any{"recordId": "00000000-0000-0000-0000-000000000000"}); w.Code != http.StatusNotFound {
		t.Errorf("unknown record test: %d; want 404", w.Code)
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/workflows/"+wfID+"/test", map[string]any{}); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank recordId test: %d; want 422", w.Code)
	}
}

func TestW6HTTP_ApprovalDetail(t *testing.T) {
	r, sx, _, bpmRepo := newW6Router(t)
	w6Cleanup(sx)
	t.Cleanup(func() { w6Cleanup(sx) })
	w6Entity(t, r)

	const approver = "77777777-aaaa-0000-0000-0000000000aa"
	const stranger = "77777777-bbbb-0000-0000-0000000000bb"

	// Process via the admin API (context payload), instance seeded directly (deterministic —
	// the async arming engine is proven in the bpm suite).
	w := do(t, r, http.MethodPost, "/api/v1/admin/approval-processes", map[string]any{
		"name": "Duyệt deal", "entityName": w6HTTPEntity, "trigger": "after_create",
		"steps": []map[string]any{{"approverType": "user", "approverId": approver, "slaSeconds": 3600}},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create process: %d %s", w.Code, w.Body.String())
	}
	var procResp struct {
		Data struct {
			Process struct {
				ID string `json:"id"`
			} `json:"process"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &procResp)
	procID := procResp.Data.Process.ID

	recID := w6Record(t, r, "open")
	due := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	instID, err := bpmRepo.CreateInstance(t.Context(), bpm.CreateInstanceInput{
		TenantID: testTenant, ProcessID: procID, EntityName: w6HTTPEntity, RecordID: recID,
		CurrentApproverType: bpm.ApproverUser, CurrentApproverID: approver,
		SLADueAt: &due, CreatedBy: stranger,
		Definition: &bpm.Definition{Steps: []bpm.Step{{ApproverType: bpm.ApproverUser, ApproverID: approver, SLASeconds: 3600}}},
	})
	if err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	// 1) The assigned approver (NON-admin) sees the full camelCase detail + process context.
	w = w6DoAs(t, r, http.MethodGet, "/api/v1/approvals/"+instID,
		auth.Subject{UserID: approver, TenantID: testTenant}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("approver detail: %d %s", w.Code, w.Body.String())
	}
	var detResp struct {
		Data struct {
			Approval struct {
				ID                  string           `json:"id"`
				ProcessID           string           `json:"processId"`
				EntityName          string           `json:"entityName"`
				RecordID            string           `json:"recordId"`
				CurrentStep         int              `json:"currentStep"`
				Status              string           `json:"status"`
				CurrentApproverType string           `json:"currentApproverType"`
				CurrentApproverID   string           `json:"currentApproverId"`
				SLADueAt            *time.Time       `json:"slaDueAt"`
				History             []map[string]any `json:"history"`
				Process             *struct {
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Steps json.RawMessage `json:"steps"`
				} `json:"process"`
			} `json:"approval"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &detResp)
	det := detResp.Data.Approval
	if det.ID != instID || det.RecordID != recID || det.EntityName != w6HTTPEntity ||
		det.Status != "pending" || det.CurrentApproverID != approver || det.CurrentApproverType != "user" {
		t.Fatalf("detail payload = %s", w.Body.String())
	}
	if det.SLADueAt == nil {
		t.Errorf("slaDueAt missing")
	}
	if det.History == nil {
		t.Errorf("history must be [] not null: %s", w.Body.String())
	}
	if det.Process == nil || det.Process.ID != procID || det.Process.Name != "Duyệt deal" || len(det.Process.Steps) == 0 {
		t.Errorf("process context = %+v", det.Process)
	}

	// 2) A non-participant (NON-admin) gets 404 — same shape as unknown (no existence oracle).
	w = w6DoAs(t, r, http.MethodGet, "/api/v1/approvals/"+instID,
		auth.Subject{UserID: stranger, TenantID: testTenant}, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("stranger detail: %d; want 404", w.Code)
	}

	// 3) An admin of the SAME tenant sees it.
	if w := do(t, r, http.MethodGet, "/api/v1/approvals/"+instID, nil); w.Code != http.StatusOK {
		t.Errorf("admin detail: %d %s", w.Code, w.Body.String())
	}

	// 4) An admin of ANOTHER tenant gets 404 (tenant pin, not role).
	w = doAs(t, r, http.MethodGet, "/api/v1/approvals/"+instID, w6HTTPOtherTenant, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant detail: %d; want 404", w.Code)
	}

	// 5) Unknown id → 404; the static sibling /approvals/pending still resolves.
	if w := do(t, r, http.MethodGet, "/api/v1/approvals/00000000-0000-0000-0000-000000000000", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown detail: %d; want 404", w.Code)
	}
	if w := do(t, r, http.MethodGet, "/api/v1/approvals/pending", nil); w.Code != http.StatusOK {
		t.Errorf("pending after :id registration: %d; want 200", w.Code)
	}
}
