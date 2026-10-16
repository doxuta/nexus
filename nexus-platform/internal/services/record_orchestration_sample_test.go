package services

// specs/064 S9-4 (T071): a record created under a sample mark (dynrepo.WithSampleMark) is audited
// like any create but fires NO workflow event and arms NO approval — on every create path
// (Create, CreateBulk partial via Create, CreateBulk atomic). An unmarked create emits as before,
// and the before_save veto still judges a sample row.

import (
	"context"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

func sampleCtx() context.Context {
	return dynrepo.WithSampleMark(context.Background(), dynrepo.SampleMark{BatchID: "b1", Source: dynrepo.SampleSourceAI})
}

func newSampleSvc(rules ...workflow.Workflow) (*RecordService, *captureEngine, *captureBPM, *spyAuditor, *fakeBulkInserter) {
	eng, appr, aud, bulk := &captureEngine{}, &captureBPM{}, &spyAuditor{}, &fakeBulkInserter{}
	store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}}
	svc := NewRecordService(oppsCatalog(), store).WithSequenceClaimer(&fakeSequenceClaimer{}).
		WithEngine(eng).WithApprovalEngine(appr).WithAuditor(aud).WithBulkInserter(bulk)
	if len(rules) > 0 {
		svc = svc.WithBeforeHooks(&vetoLoader{rules: rules}, nil)
	}
	return svc, eng, appr, aud, bulk
}

func TestEmitCreateEvents_SampleMark(t *testing.T) {
	tests := []struct {
		name       string
		ctx        context.Context
		wantEngine int
		wantAppr   int
	}{
		{"marked create emits nothing", sampleCtx(), 0, 0},
		{"unmarked create emits as before", context.Background(), 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, eng, appr, aud, _ := newSampleSvc()
			if _, err := svc.Create(tc.ctx, rep, "t1", rep.UserID, "opps", map[string]any{"name": "Deal"}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if len(eng.evts) != tc.wantEngine || len(appr.events) != tc.wantAppr {
				t.Fatalf("engine=%d approvals=%d, want %d/%d", len(eng.evts), len(appr.events), tc.wantEngine, tc.wantAppr)
			}
			if len(aud.events) != 1 || aud.events[0].Action != audit.ActionCreate {
				t.Fatalf("audit events = %+v, want exactly one create (a sample create is still audited)", aud.events)
			}
		})
	}
}

func TestEmitCreateEvents_SampleMark_BulkPaths(t *testing.T) {
	rows := func() []map[string]any { return []map[string]any{{"name": "A"}, {"name": "B"}} }
	for _, mode := range []BulkMode{BulkModePartial, BulkModeAtomic} {
		t.Run(string(mode)+" marked", func(t *testing.T) {
			svc, eng, appr, aud, _ := newSampleSvc()
			res, err := svc.CreateBulk(sampleCtx(), rep, "t1", rep.UserID, "opps", rows(), mode)
			if err != nil || res.Created != 2 {
				t.Fatalf("CreateBulk: created=%d err=%v", res.Created, err)
			}
			if len(eng.evts) != 0 || len(appr.events) != 0 {
				t.Fatalf("engine=%d approvals=%d, want 0/0", len(eng.evts), len(appr.events))
			}
			if len(aud.events) != 2 {
				t.Fatalf("audit = %d, want 2 creates", len(aud.events))
			}
		})
		t.Run(string(mode)+" unmarked", func(t *testing.T) {
			svc, eng, appr, _, _ := newSampleSvc()
			if _, err := svc.CreateBulk(context.Background(), rep, "t1", rep.UserID, "opps", rows(), mode); err != nil {
				t.Fatalf("CreateBulk: %v", err)
			}
			if len(eng.evts) != 2 || len(appr.events) != 2 {
				t.Fatalf("engine=%d approvals=%d, want 2/2", len(eng.evts), len(appr.events))
			}
		})
	}
}

// before_save still judges a sample row: only the AFTER side effects are skipped.
func TestSampleMark_BeforeSaveVetoStillBlocks(t *testing.T) {
	svc, eng, _, _, _ := newSampleSvc(wonNeedsDateOn("opps", ""))
	_, err := svc.Create(sampleCtx(), rep, "t1", rep.UserID, "opps", map[string]any{"name": "Deal", "stage": "Closed Won"})
	wantVeto(t, err)
	if len(eng.evts) != 0 {
		t.Fatalf("engine got %d events after a veto", len(eng.evts))
	}
}
