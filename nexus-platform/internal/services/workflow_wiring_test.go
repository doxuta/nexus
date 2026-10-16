package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/workflow"
)

// specs/066 L1a: the direct sender (JOBS_ENABLED=false) writes ONE final row too.
func TestWorkflowEmailSenderWritesFinalRow(t *testing.T) {
	ctxOf := func(o workflow.EmailOrigin) context.Context { return workflow.WithEmailOrigin(context.Background(), o) }

	t.Run("sent", func(t *testing.T) {
		log := &spyEmailLog{}
		s := NewWorkflowEmailSender(&fakeMailTransport{}).WithEmailLog(log, nil)
		if err := s.SendEmail(ctxOf(workflow.EmailOrigin{TenantID: "t1", WorkflowID: "w1", ActionIndex: 1, SubjectTemplate: "Hi {{n}}"}),
			"lan.nguyen@gmail.com", "Hi Lan", "b"); err != nil {
			t.Fatal(err)
		}
		if len(log.inserted) != 1 {
			t.Fatalf("rows = %d, want exactly 1", len(log.inserted))
		}
		e := log.inserted[0]
		if e.Status != emaillog.StatusSent || e.Attempts != 1 || !e.Finished || e.RecipientMasked != "l***@gmail.com" ||
			e.SubjectTemplate != "Hi {{n}}" || e.TenantID != "t1" || e.ID == "" {
			t.Errorf("row = %+v", e)
		}
	})
	t.Run("failed row carries a masked error", func(t *testing.T) {
		log := &spyEmailLog{}
		tr := &fakeMailTransport{err: errors.New("550 lan.nguyen@gmail.com: Recipient address rejected")}
		s := NewWorkflowEmailSender(tr).WithEmailLog(log, nil)
		err := s.SendEmail(ctxOf(workflow.EmailOrigin{TenantID: "t1"}), "lan.nguyen@gmail.com", "s", "b")
		if err == nil {
			t.Fatal("the failure must still be returned")
		}
		e := log.inserted[0]
		if e.Status != emaillog.StatusFailed || strings.Contains(e.LastError, "lan.nguyen") || !strings.Contains(e.LastError, "[redacted]") {
			t.Errorf("row = %+v", e)
		}
	})
	t.Run("resend identity is honoured", func(t *testing.T) {
		log := &spyEmailLog{}
		s := NewWorkflowEmailSender(&fakeMailTransport{}).WithEmailLog(log, nil)
		_ = s.SendEmail(ctxOf(workflow.EmailOrigin{TenantID: "t1", LogID: "pre", ResendOf: "old", CreatedBy: "u9"}), "a@b.vn", "s", "b")
		e := log.inserted[0]
		if e.ID != "pre" || e.ResendOf != "old" || e.CreatedBy != "u9" {
			t.Errorf("row = %+v", e)
		}
	})
	t.Run("no tenant, no row", func(t *testing.T) {
		log := &spyEmailLog{}
		s := NewWorkflowEmailSender(&fakeMailTransport{}).WithEmailLog(log, nil)
		if err := s.SendEmail(context.Background(), "a@b.vn", "s", "b"); err != nil {
			t.Fatal(err)
		}
		if len(log.inserted) != 0 {
			t.Errorf("rows = %v, want none", log.inserted)
		}
	})
	t.Run("a log failure never changes the send result", func(t *testing.T) {
		log := &spyEmailLog{insertErr: errors.New("gone")}
		s := NewWorkflowEmailSender(&fakeMailTransport{}).WithEmailLog(log, nil)
		if err := s.SendEmail(ctxOf(workflow.EmailOrigin{TenantID: "t1"}), "a@b.vn", "s", "b"); err != nil {
			t.Fatal(err)
		}
	})
}

// specs/064 S9-4 (T085): the scheduler's lister must ask the store to leave sample rows out —
// otherwise a scheduled rule emails / webhooks demo data. Only this list sets the flag.
func TestWorkflowRecordLister_ExcludesSamples(t *testing.T) {
	store := &fakeRecordStore{}
	if _, err := NewWorkflowRecordLister(store).ListRecordsForScheduler(context.Background(), "crm_accounts", "t1", 10); err != nil {
		t.Fatalf("ListRecordsForScheduler: %v", err)
	}
	if !store.gotParams.ExcludeSamples {
		t.Fatal("ListRecordsForScheduler did not set ListParams.ExcludeSamples")
	}
	if store.gotTenantID != "t1" {
		t.Fatalf("tenant = %q, want t1", store.gotTenantID)
	}
}
