//go:build integration

// specs/066 L2 (T052) over a real queue and a real MySQL, wired as main.go wires it: a rule mail that
// failed is rebuilt from the LIVE rule and the record's CURRENT data and sent again — once per failed
// row even under two simultaneous presses, never to another tenant's id, with the finished job's
// payload scrubbed (#95) and the new log row pointing back at the old one.
//
//	TEST_DATABASE_URL=… go test -tags=integration ./internal/services/ -run TestEmailResend
package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/emaillog"
)

const emlOtherTenant = "e4e4e4e4-0000-4000-8000-0000000000bb"

func falseP() *bool { f := false; return &f }

type resendRow struct {
	ID      string  `db:"id"`
	Status  string  `db:"status"`
	Masked  string  `db:"recipient_masked"`
	Resend  *string `db:"resend_of"`
	Created *string `db:"created_by"`
	JobID   *string `db:"job_id"`
}

func resendRows(t *testing.T, sdb *sqlx.DB, of string) []resendRow {
	t.Helper()
	var rows []resendRow
	if err := sdb.Select(&rows, `SELECT id, status, recipient_masked, resend_of, created_by, job_id FROM sys_email_log
		WHERE tenant_id = ? AND resend_of = ?`, emlDeliveryTenant, of); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestEmailResend_RebuildsFromTheLiveRuleAndSendsOncePerFailedRow(t *testing.T) {
	const addr = "lan@nhakhoa.vn"
	tr := &fakeMailTransport{err: errors.New("smtp down")}
	env := deliveryStackFull(t, tr, 1)
	env.mgr.Start()
	defer stopManager(t, env.mgr)
	ctx := context.Background()

	appt := createLinkedAppointment(t, env.recordSvc, addr)
	var failed logRow
	waitUntil(t, "the original mail to fail", func() bool {
		var rows []logRow
		if err := env.sdb.Select(&rows, `SELECT id, tenant_id, source, status, recipient_masked, subject_template, workflow_id, job_id, attempts, last_error
			FROM sys_email_log WHERE tenant_id = ? AND status = 'failed'`, emlDeliveryTenant); err != nil || len(rows) != 1 {
			return false
		}
		failed = rows[0]
		return true
	})

	// The cause is fixed (SMTP is back). A preview sends nothing and writes nothing.
	tr.mu.Lock()
	tr.err = nil
	tr.mu.Unlock()
	res, err := env.resend.Resend(ctx, emlDeliveryTenant, "admin-1", failed.ID, nil)
	if err != nil || !res.DryRun || res.RecipientMasked != "l***@nhakhoa.vn" || res.Subject != "Nhắc hẹn" || res.RuleChangedSinceSend {
		t.Fatalf("preview = %+v, %v", res, err)
	}
	if len(tr.mails()) != 0 || len(resendRows(t, env.sdb, failed.ID)) != 0 {
		t.Fatal("a preview sent a mail or wrote a row")
	}

	// Tenant B on tenant A's id: the byte-identical 404 of an id that never existed.
	_, errOther := env.resend.Resend(ctx, emlOtherTenant, "admin-b", failed.ID, falseP())
	_, errGhost := env.resend.Resend(ctx, emlOtherTenant, "admin-b", "no-such-row", falseP())
	wantCode(t, errOther, "EMAIL_LOG_NOT_FOUND")
	wantCode(t, errGhost, "EMAIL_LOG_NOT_FOUND")
	var a, b *ValidationError
	if !errors.As(errOther, &a) || !errors.As(errGhost, &b) || a.Msg != b.Msg {
		t.Fatalf("foreign vs ghost bodies differ: %v / %v", errOther, errGhost)
	}

	// Real send.
	res, err = env.resend.Resend(ctx, emlDeliveryTenant, "admin-1", failed.ID, falseP())
	if err != nil || res.DryRun || res.Entry == nil {
		t.Fatalf("resend = %+v, %v", res, err)
	}
	waitUntil(t, "the resent mail to go out", func() bool { return len(tr.mails()) == 1 })
	if m := tr.mails()[0]; m.To != addr || m.Subject != "Nhắc hẹn" {
		t.Fatalf("mail = %+v; want the rule's mail to the linked patient", m)
	}
	waitUntil(t, "the new row to become sent", func() bool {
		rows := resendRows(t, env.sdb, failed.ID)
		return len(rows) == 1 && rows[0].Status == "sent"
	})
	nr := resendRows(t, env.sdb, failed.ID)[0]
	if nr.ID != res.Entry.ID || nr.Masked != "l***@nhakhoa.vn" || nr.Created == nil || *nr.Created != "admin-1" || nr.JobID == nil {
		t.Fatalf("new row = %+v (entry %s)", nr, res.Entry.ID)
	}
	var payload *string
	waitUntil(t, "the resend job's payload to be scrubbed", func() bool {
		return env.sdb.Get(&payload, "SELECT CAST(payload AS CHAR) FROM sys_jobs WHERE id = ? AND status = 'done'", *nr.JobID) == nil && payload == nil
	})
	var hits int
	if err := env.sdb.Get(&hits, `SELECT COUNT(*) FROM sys_email_log WHERE CONCAT_WS('|', recipient_masked, subject_template, last_error) LIKE '%lan%'`); err != nil || hits != 0 {
		t.Fatalf("an address reached the log: hits=%d err=%v", hits, err)
	}

	// The old row is spent: a second press is the 409.
	_, err = env.resend.Resend(ctx, emlDeliveryTenant, "admin-1", failed.ID, falseP())
	wantCode(t, err, "EMAIL_NOT_RESENDABLE")
	if _, err = env.resend.Resend(ctx, emlDeliveryTenant, "admin-1", failed.ID, nil); err == nil {
		t.Fatal("even the preview of a spent row must be refused")
	}

	// 20 rounds, two simultaneous presses each: exactly one wins and exactly one mail goes out.
	wf, _ := failedWorkflowFields(t, env.sdb, failed.ID)
	idx := 1
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("race-%02d", i)
		if err := emaillog.NewRepository(env.sdb).Insert(ctx, emaillog.Entry{ID: id, TenantID: emlDeliveryTenant, Source: emaillog.SourceWorkflow,
			Status: emaillog.StatusFailed, RecipientMasked: "l***@nhakhoa.vn", SubjectTemplate: "Nhắc hẹn", WorkflowID: wf, ActionIndex: &idx,
			EntityName: "lich_hen", RecordID: appt}); err != nil {
			t.Fatal(err)
		}
		before := len(tr.mails())
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for j := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[j] = env.resend.Resend(ctx, emlDeliveryTenant, "admin-1", id, falseP())
			}()
		}
		wg.Wait()
		wins := 0
		for _, e := range errs {
			if e == nil {
				wins++
			} else {
				wantCode(t, e, "EMAIL_NOT_RESENDABLE")
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d winners, errs %v", i, wins, errs)
		}
		waitUntil(t, "exactly one more mail", func() bool { return len(tr.mails()) == before+1 })
	}
	if got := len(tr.mails()); got != 21 {
		t.Fatalf("%d mails in total; want 1 + 20", got)
	}
}

func failedWorkflowFields(t *testing.T, sdb *sqlx.DB, logID string) (string, string) {
	t.Helper()
	var r struct {
		WF  string `db:"workflow_id"`
		Rec string `db:"record_id"`
	}
	if err := sdb.Get(&r, "SELECT workflow_id, record_id FROM sys_email_log WHERE id = ?", logID); err != nil {
		t.Fatal(err)
	}
	return r.WF, r.Rec
}
