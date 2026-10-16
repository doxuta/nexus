package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/notify"
	"nexus/platform/internal/workflow"
)

// spyEmailLog records every write in order (shared trace with orderedEnqueuer) and can fail on demand.
type spyEmailLog struct {
	trace     *[]string
	inserted  []emaillog.Entry
	attached  []string // "tenant|id|job"
	attempts  []string // "tenant|id|n|err"
	sent      []string // "tenant|id|n"
	failed    []string // "tenant|id|n|err"
	insertErr error
}

func (s *spyEmailLog) note(x string) {
	if s.trace != nil {
		*s.trace = append(*s.trace, x)
	}
}
func (s *spyEmailLog) Insert(_ context.Context, e emaillog.Entry) error {
	s.note("insert")
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted = append(s.inserted, e)
	return nil
}
func (s *spyEmailLog) AttachJob(_ context.Context, tenantID, id, jobID string) error {
	s.attached = append(s.attached, tenantID+"|"+id+"|"+jobID)
	return nil
}
func (s *spyEmailLog) MarkAttempt(_ context.Context, tenantID, id string, n int, e string) error {
	s.attempts = append(s.attempts, tenantID+"|"+id+"|"+itoa(n)+"|"+e)
	return nil
}
func (s *spyEmailLog) MarkSent(_ context.Context, tenantID, id string, n int) error {
	s.sent = append(s.sent, tenantID+"|"+id+"|"+itoa(n))
	return nil
}
func (s *spyEmailLog) MarkFailed(_ context.Context, tenantID, id string, n int, e string) error {
	s.failed = append(s.failed, tenantID+"|"+id+"|"+itoa(n)+"|"+e)
	return nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// orderedEnqueuer notes "enqueue" on the shared trace.
type orderedEnqueuer struct {
	fakeJobEnqueuer
	trace *[]string
}

func (o *orderedEnqueuer) Enqueue(ctx context.Context, j jobs.Job) (string, error) {
	*o.trace = append(*o.trace, "enqueue")
	return o.fakeJobEnqueuer.Enqueue(ctx, j)
}

func originCtx(o workflow.EmailOrigin) context.Context {
	return workflow.WithEmailOrigin(context.Background(), o)
}

func TestEnqueuingSenderWritesLogBeforeEnqueue(t *testing.T) {
	var trace []string
	q := &orderedEnqueuer{trace: &trace}
	log := &spyEmailLog{trace: &trace}
	s := NewEnqueuingWorkflowEmailSender(q).WithEmailLog(log, nil)
	idx := 2
	_ = idx
	ctx := originCtx(workflow.EmailOrigin{TenantID: "t1", WorkflowID: "w1", ActionIndex: 2, EntityName: "crm_contacts",
		RecordID: "r1", SubjectTemplate: "Chào {{name}}", ActionHash: "h"})

	if err := s.SendEmail(ctx, "lan.nguyen@gmail.com", "Chào Lan", "body"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(trace, ",") != "insert,enqueue" {
		t.Fatalf("order = %v, want insert before enqueue", trace)
	}
	e := log.inserted[0]
	if e.TenantID != "t1" || e.Status != emaillog.StatusQueued || e.Source != emaillog.SourceWorkflow ||
		e.RecipientMasked != "l***@gmail.com" || e.SubjectTemplate != "Chào {{name}}" || e.WorkflowID != "w1" ||
		e.EntityName != "crm_contacts" || e.RecordID != "r1" || e.ActionIndex == nil || *e.ActionIndex != 2 || e.ID == "" {
		t.Errorf("entry = %+v", e)
	}
	for _, f := range []string{e.RecipientMasked, e.SubjectTemplate} {
		if strings.Contains(f, "Lan") && f != e.SubjectTemplate {
			t.Errorf("a rendered value reached the log: %q", f)
		}
	}
	job := q.jobs[0]
	if job.TenantID != "t1" {
		t.Errorf("job.TenantID = %q, want the origin's tenant even without a RunRef (FR-013)", job.TenantID)
	}
	var p emailAlertJobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.LogID != e.ID {
		t.Errorf("payload logId = %q, want %q", p.LogID, e.ID)
	}
	if len(log.attached) != 1 || log.attached[0] != "t1|"+e.ID+"|id-1" {
		t.Errorf("AttachJob = %v", log.attached)
	}
}

func TestEnqueuingSenderHonoursPreMintedOrigin(t *testing.T) {
	var trace []string
	log := &spyEmailLog{trace: &trace}
	s := NewEnqueuingWorkflowEmailSender(&orderedEnqueuer{trace: &trace}).WithEmailLog(log, nil)
	ctx := originCtx(workflow.EmailOrigin{TenantID: "t1", WorkflowID: "w1", LogID: "pre", ResendOf: "old", CreatedBy: "u9"})
	if err := s.SendEmail(ctx, "a@b.vn", "s", "b"); err != nil {
		t.Fatal(err)
	}
	e := log.inserted[0]
	if e.ID != "pre" || e.ResendOf != "old" || e.CreatedBy != "u9" {
		t.Errorf("entry = %+v, want the pre-minted id / resendOf / createdBy", e)
	}
}

func TestEnqueuingSenderEnqueueFailureMarksFailedMasked(t *testing.T) {
	var trace []string
	q := &orderedEnqueuer{trace: &trace, fakeJobEnqueuer: fakeJobEnqueuer{err: errors.New("db down for lan.nguyen@gmail.com")}}
	log := &spyEmailLog{trace: &trace}
	s := NewEnqueuingWorkflowEmailSender(q).WithEmailLog(log, nil)
	err := s.SendEmail(originCtx(workflow.EmailOrigin{TenantID: "t1", WorkflowID: "w1"}), "lan.nguyen@gmail.com", "s", "b")
	if err == nil {
		t.Fatal("enqueue failure must still be returned")
	}
	if len(log.failed) != 1 {
		t.Fatalf("MarkFailed calls = %v, want 1", log.failed)
	}
	if strings.Contains(log.failed[0], "lan.nguyen") || !strings.Contains(log.failed[0], "[redacted]") {
		t.Errorf("failed row must carry the MASKED error, got %q", log.failed[0])
	}
}

func TestEnqueuingSenderLogFailureIsFailOpen(t *testing.T) {
	var trace []string
	q := &orderedEnqueuer{trace: &trace}
	log := &spyEmailLog{trace: &trace, insertErr: errors.New("log table gone")}
	s := NewEnqueuingWorkflowEmailSender(q).WithEmailLog(log, nil)
	if err := s.SendEmail(originCtx(workflow.EmailOrigin{TenantID: "t1"}), "a@b.vn", "s", "b"); err != nil {
		t.Fatalf("a log failure must not stop the mail: %v", err)
	}
	var p emailAlertJobPayload
	if err := json.Unmarshal(q.jobs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.LogID != "" {
		t.Errorf("payload must carry no logId when the row was not written, got %q", p.LogID)
	}
	if len(log.attached) != 0 {
		t.Errorf("nothing to attach: %v", log.attached)
	}
}

func TestEnqueuingSenderTenantFromRunRefStillWins(t *testing.T) {
	q := &fakeJobEnqueuer{}
	log := &spyEmailLog{}
	s := NewEnqueuingWorkflowEmailSender(q).WithEmailLog(log, nil)
	ctx := workflow.WithRunRef(originCtx(workflow.EmailOrigin{TenantID: "t-origin", WorkflowID: "w1"}),
		workflow.RunRef{TenantID: "t-run", WorkflowID: "w1", RunID: "run1", ActionIndex: 1})
	if err := s.SendEmail(ctx, "a@b.vn", "s", "b"); err != nil {
		t.Fatal(err)
	}
	if q.jobs[0].TenantID != "t-run" || log.inserted[0].TenantID != "t-run" || log.inserted[0].RunID != "run1" {
		t.Errorf("job tenant %q, log %+v: the run's tenant must win and run_id be set", q.jobs[0].TenantID, log.inserted[0])
	}
}

// --- handler ---------------------------------------------------------------------------------

func mkEmailJob(logID string, attempt, max int) jobs.Job {
	p := `{"to":"lan.nguyen@gmail.com","subject":"s","body":"b"`
	if logID != "" {
		p += `,"logId":"` + logID + `"`
	}
	return jobs.Job{ID: "j1", TenantID: "t1", Type: EmailAlertJobType, Payload: json.RawMessage(p + `}`), Attempts: attempt, MaxAttempts: max}
}

func TestEmailAlertHandlerUpdatesLog(t *testing.T) {
	t.Run("sent", func(t *testing.T) {
		log := &spyEmailLog{}
		if err := NewEmailAlertJobHandler(&fakeMailTransport{}, nil, log, nil)(context.Background(), mkEmailJob("L1", 2, 5)); err != nil {
			t.Fatal(err)
		}
		if len(log.sent) != 1 || log.sent[0] != "t1|L1|2" {
			t.Errorf("sent = %v", log.sent)
		}
	})
	t.Run("transient failure, attempts left", func(t *testing.T) {
		log := &spyEmailLog{}
		tr := &fakeMailTransport{err: errors.New("550 rcpt lan.nguyen@gmail.com rejected")}
		if err := NewEmailAlertJobHandler(tr, nil, log, nil)(context.Background(), mkEmailJob("L1", 1, 5)); err == nil {
			t.Fatal("must still return the error so the queue retries")
		}
		if len(log.attempts) != 1 || len(log.failed) != 0 || strings.Contains(log.attempts[0], "lan.nguyen") || !strings.Contains(log.attempts[0], "[redacted]") {
			t.Errorf("attempts = %v failed = %v (masked error expected)", log.attempts, log.failed)
		}
	})
	t.Run("last attempt fails", func(t *testing.T) {
		log := &spyEmailLog{}
		tr := &fakeMailTransport{err: errors.New("down")}
		_ = NewEmailAlertJobHandler(tr, nil, log, nil)(context.Background(), mkEmailJob("L1", 5, 5))
		if len(log.failed) != 1 || log.failed[0] != "t1|L1|5|services.EmailAlertJobHandler: down" {
			t.Errorf("failed = %v", log.failed)
		}
	})
	t.Run("mail not configured fails at once", func(t *testing.T) {
		log := &spyEmailLog{}
		tr := &fakeMailTransport{err: notify.ErrMailNotConfigured}
		_ = NewEmailAlertJobHandler(tr, nil, log, nil)(context.Background(), mkEmailJob("L1", 1, 5))
		if len(log.failed) != 1 || len(log.attempts) != 0 {
			t.Errorf("failed = %v attempts = %v, want an immediate failure", log.failed, log.attempts)
		}
	})
	t.Run("no logId in the payload touches nothing", func(t *testing.T) {
		log := &spyEmailLog{}
		_ = NewEmailAlertJobHandler(&fakeMailTransport{}, nil, log, nil)(context.Background(), mkEmailJob("", 1, 5))
		_ = NewEmailAlertJobHandler(&fakeMailTransport{err: errors.New("x")}, nil, log, nil)(context.Background(), mkEmailJob("", 5, 5))
		if len(log.sent)+len(log.attempts)+len(log.failed) != 0 {
			t.Errorf("log touched without a logId: %+v", log)
		}
	})
	t.Run("a nil log is fine", func(t *testing.T) {
		if err := NewEmailAlertJobHandler(&fakeMailTransport{}, nil, nil, nil)(context.Background(), mkEmailJob("L1", 1, 5)); err != nil {
			t.Fatal(err)
		}
	})
}
