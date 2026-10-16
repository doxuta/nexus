package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
)

// DAILY-QUEUE #70: the audit row of an approval decision stored {approvalInstanceId, step,
// comment} — a step index with nothing to resolve it against once the instance's pinned snapshot
// is gone. The row must say which process and which chain that index belongs to, in the row itself.
//
// The row records the STEPS only, never the outcome. A non-admin reads this row through the
// record's History tab (GET /audit-log/:entity/:recordId), whose field-ACL filter is a deny-list
// over top-level keys: it would strip a hidden `status` key but not `definition.outcome`, which
// names that field and the value the approval writes into it. The write-back itself is already a
// separate record-update audit row, redacted like any other edit — so leaving it out loses nothing.
func TestApprovalAuditAdapter_RecordsProcessAndDefinition(t *testing.T) {
	def := &bpm.Definition{
		Steps: []bpm.Step{
			{Name: "Trưởng phòng", ApproverType: bpm.ApproverUser, ApproverID: "u-mgr"},
			{Name: "CFO", ApproverType: bpm.ApproverRole, ApproverID: "role-cfo", SLASeconds: 3600},
		},
		Outcome: &bpm.Outcome{StatusField: "status", ApprovedValue: "Chấp nhận"},
	}
	cases := []struct {
		name    string
		def     *bpm.Definition
		wantDef string // "" → the key must be absent, not null
	}{
		{"decision with its executed steps (outcome left out — field-ACL)", def,
			`{"steps":[{"name":"Trưởng phòng","approverType":"user","approverId":"u-mgr"},{"name":"CFO","approverType":"role","approverId":"role-cfo","slaSeconds":3600}]}`},
		{"escalation of a pre-000065 instance has no definition to record", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &captureAuditor{} // shared test double (field_acl_admin_test.go)
			NewApprovalAuditAdapter(cap).RecordApprovalDecision(context.Background(), bpm.ApprovalAuditEvent{
				TenantID: "t1", ActorUserID: "u-mgr", EntityName: "crm_quotes", RecordID: "q1",
				InstanceID: "i1", ProcessID: "p1", Definition: tc.def, Step: 1, Action: bpm.HistoryApproved,
				Comment: "ok", At: time.Now(),
			})
			if len(cap.events) != 1 || cap.events[0].Diff == nil {
				t.Fatalf("audit events = %+v; want one with a diff", cap.events)
			}
			// Round-trip through JSON exactly as audit.SQLRepository.InsertEvent stores it.
			b, err := json.Marshal(cap.events[0].Diff)
			if err != nil {
				t.Fatalf("marshal diff: %v", err)
			}
			var stored struct {
				After map[string]json.RawMessage `json:"after"`
			}
			if err := json.Unmarshal(b, &stored); err != nil {
				t.Fatalf("unmarshal diff: %v", err)
			}
			if got := string(stored.After["processId"]); got != `"p1"` {
				t.Errorf("after.processId = %s; want \"p1\"", got)
			}
			if got := string(stored.After["step"]); got != `1` {
				t.Errorf("after.step = %s; want 1 (unchanged)", got)
			}
			got, present := stored.After["definition"]
			if tc.wantDef == "" {
				if present {
					t.Errorf("after.definition = %s; want the key absent when nothing is known", got)
				}
				return
			}
			if string(got) != tc.wantDef {
				t.Errorf("after.definition = %s\nwant %s", got, tc.wantDef)
			}
		})
	}
}
