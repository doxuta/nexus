package services

// specs/066 L3: both rule-email senders implement workflow.EmailSkipRecorder, so a mail that never
// reached SendEmail (bad template, empty recipient) is a "failed" line with no address, resendable
// once the record is fixed.

import (
	"context"
	"testing"

	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/workflow"
)

func TestSendersRecordEmailSkipped(t *testing.T) {
	origin := workflow.EmailOrigin{TenantID: "t1", WorkflowID: "w1", ActionIndex: 3, EntityName: "crm_contacts",
		RecordID: "r1", SubjectTemplate: "Chào {{name}}", ActionHash: "h"}
	reason := `workflow.runEmailAlert: không có địa chỉ nhận — mẫu "{{email}}" ra rỗng trên bản ghi này`
	makers := map[string]func(log *spyEmailLog) workflow.EmailSkipRecorder{
		"enqueuing": func(log *spyEmailLog) workflow.EmailSkipRecorder {
			return NewEnqueuingWorkflowEmailSender(&fakeJobEnqueuer{}).WithEmailLog(log, nil)
		},
		"direct": func(log *spyEmailLog) workflow.EmailSkipRecorder {
			return NewWorkflowEmailSender(nil).WithEmailLog(log, nil)
		},
	}
	for name, mk := range makers {
		t.Run(name, func(t *testing.T) {
			log := &spyEmailLog{}
			mk(log).RecordEmailSkipped(originCtx(origin), reason)
			if len(log.inserted) != 1 {
				t.Fatalf("rows = %d, want 1", len(log.inserted))
			}
			e := log.inserted[0]
			if e.Status != emaillog.StatusFailed || e.Source != emaillog.SourceWorkflow || e.RecipientMasked != "" {
				t.Errorf("row = %+v; want a failed workflow row with no recipient", e)
			}
			if e.LastError != reason || e.Attempts != 0 || !e.Finished {
				t.Errorf("lastError=%q attempts=%d finished=%v; want the run's text, no attempt, terminal", e.LastError, e.Attempts, e.Finished)
			}
			// Everything Resend needs to rebuild the mail once the record is fixed.
			if e.TenantID != "t1" || e.WorkflowID != "w1" || e.ActionIndex == nil || *e.ActionIndex != 3 || e.RecordID != "r1" || e.EntityName != "crm_contacts" {
				t.Errorf("row lost its rule/record coordinates: %+v", e)
			}
			if e.SubjectTemplate != "Chào {{name}}" || e.ActionHash != "h" {
				t.Errorf("subject/hash = %q/%q", e.SubjectTemplate, e.ActionHash)
			}
		})
	}
}

func TestSendersRecordEmailSkipped_Quiet(t *testing.T) {
	t.Run("no tenant: nothing could ever read the row", func(t *testing.T) {
		log := &spyEmailLog{}
		NewWorkflowEmailSender(nil).WithEmailLog(log, nil).RecordEmailSkipped(context.Background(), "x")
		if len(log.inserted) != 0 {
			t.Errorf("rows = %d, want 0", len(log.inserted))
		}
	})
	t.Run("log not wired", func(t *testing.T) {
		NewWorkflowEmailSender(nil).RecordEmailSkipped(originCtx(workflow.EmailOrigin{TenantID: "t1"}), "x") // must not panic
		NewEnqueuingWorkflowEmailSender(&fakeJobEnqueuer{}).RecordEmailSkipped(originCtx(workflow.EmailOrigin{TenantID: "t1"}), "x")
	})
}
