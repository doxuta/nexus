package bpm

import (
	"encoding/json"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// DAILY-QUEUE #68: a pending instance whose row will not decode used to vanish from the
// approver's inbox (and from the record's approvals tab, and from the SLA sweep) with no log line
// anywhere — the three list reads `continue`d past it. Skipping is still right (one bad row must
// not blind the reader to every other row), but it has to be loud: an operator must be able to
// find the instance that nobody can see any more.
func TestDecodeInstances_SkipsUndecodableRowAndLogsIt(t *testing.T) {
	raw := func(s string) *json.RawMessage { m := json.RawMessage(s); return &m }
	good := instanceRow{
		ID: "inst-good", TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "rec-good",
		Status: "pending", History: json.RawMessage(`[]`),
		ProcessSnapshot: raw(`{"steps":[{"approverType":"user","approverId":"u1"}]}`),
	}
	cases := []struct {
		name string
		bad  instanceRow
	}{
		{"history is not an array", instanceRow{
			ID: "inst-bad-history", TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "rec-bad",
			Status: "pending", History: json.RawMessage(`{"step":0}`),
		}},
		{"snapshot steps is not an array", instanceRow{
			ID: "inst-bad-snapshot", TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "rec-bad",
			Status: "pending", History: json.RawMessage(`[]`), ProcessSnapshot: raw(`{"steps":"x"}`),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			r := &SQLRepository{logger: zap.New(core)}

			got := r.decodeInstances("ListPendingForUser", []instanceRow{good, tc.bad})

			if len(got) != 1 || got[0].ID != "inst-good" {
				t.Fatalf("decoded = %+v; want only inst-good (the corrupt row is skipped, the rest still listed)", got)
			}
			// A corrupt snapshot must never come back as a nil Definition: nil means "armed before
			// 000065, read the live process", which would re-open the #8 step-skip.
			if got[0].Definition == nil {
				t.Fatalf("good row lost its pinned definition")
			}
			entries := logs.FilterMessageSnippet("undecodable").AllUntimed()
			if len(entries) != 1 {
				t.Fatalf("log entries = %d (%v); want exactly 1 line naming the skipped row", len(entries), logs.AllUntimed())
			}
			e := entries[0]
			if e.Level != zapcore.ErrorLevel {
				t.Errorf("level = %s; want error — a record silently missing from an inbox is not a warning", e.Level)
			}
			f := e.ContextMap()
			want := map[string]string{
				"op": "ListPendingForUser", "tenant_id": "t1", "instance_id": tc.bad.ID,
				"process_id": "p1", "entity_name": "crm_quotes", "record_id": "rec-bad", "status": "pending",
			}
			for k, v := range want {
				if f[k] != v {
					t.Errorf("field %s = %v; want %q", k, f[k], v)
				}
			}
			if _, ok := f["error"]; !ok {
				t.Errorf("log line has no error field: %v", f)
			}
		})
	}
}
