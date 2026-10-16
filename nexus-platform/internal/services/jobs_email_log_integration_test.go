//go:build integration

// specs/066 L1a over a real queue and a real MySQL: every rule email leaves exactly one tenant-pinned
// sys_email_log row — queued → sent, or → failed with a masked error — and the log never holds the
// address or the rendered subject, while the finished job's payload stays scrubbed (#95).
//
//	TEST_DATABASE_URL=… go test -tags=integration ./internal/services/ -run TestEmailLog
package services

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
)

type logRow struct {
	ID       string  `db:"id"`
	Tenant   string  `db:"tenant_id"`
	Source   string  `db:"source"`
	Status   string  `db:"status"`
	Masked   string  `db:"recipient_masked"`
	Subject  *string `db:"subject_template"`
	Workflow *string `db:"workflow_id"`
	JobID    *string `db:"job_id"`
	Attempts int     `db:"attempts"`
	LastErr  *string `db:"last_error"`
}

func readLog(t *testing.T, sdb *sqlx.DB, want string) []logRow {
	t.Helper()
	var rows []logRow
	waitUntil(t, "the email log row to reach "+want, func() bool {
		rows = nil
		if err := sdb.Select(&rows, `SELECT id, tenant_id, source, status, recipient_masked, subject_template, workflow_id, job_id, attempts, last_error
			FROM sys_email_log WHERE tenant_id = ?`, emlDeliveryTenant); err != nil {
			return false
		}
		return len(rows) == 1 && rows[0].Status == want
	})
	return rows
}

func TestEmailLog_SentRowIsMaskedAndPayloadStaysScrubbed(t *testing.T) {
	tr := &fakeMailTransport{}
	sdb, recordSvc, mgr := deliveryStack(t, tr, 1)
	mgr.Start()
	defer stopManager(t, mgr)

	_ = createLinkedAppointment(t, recordSvc, "a.canhan@gmail.com")
	rows := readLog(t, sdb, "sent")
	r := rows[0]
	if r.Tenant != emlDeliveryTenant || r.Source != "workflow" || r.Masked != "a***@gmail.com" || r.Attempts != 1 ||
		r.Subject == nil || *r.Subject != "Nhắc hẹn" || r.Workflow == nil || r.JobID == nil || r.LastErr != nil {
		t.Fatalf("row = %+v", r)
	}
	var payload *string
	waitUntil(t, "the job payload to be scrubbed", func() bool {
		return sdb.Get(&payload, "SELECT CAST(payload AS CHAR) FROM sys_jobs WHERE id = ? AND status = 'done'", *r.JobID) == nil && payload == nil
	})
	var hits int
	if err := sdb.Get(&hits, "SELECT COUNT(*) FROM sys_email_log WHERE CONCAT_WS('|', recipient_masked, subject_template, last_error) LIKE '%canhan%'"); err != nil || hits != 0 {
		t.Fatalf("an address reached the log: hits=%d err=%v", hits, err)
	}
}

func TestEmailLog_FailedRowCarriesTheMaskedError(t *testing.T) {
	const personal = "lan.canhan@gmail.com"
	echo := fmt.Errorf("notify.SMTPSender.Send: rcpt to: 550 5.1.1 <%s>: Recipient address rejected", personal)
	sdb, recordSvc, mgr := deliveryStack(t, &fakeMailTransport{err: echo}, 1)
	mgr.Start()
	defer stopManager(t, mgr)

	_ = createLinkedAppointment(t, recordSvc, personal)
	r := readLog(t, sdb, "failed")[0]
	if r.LastErr == nil || !strings.Contains(*r.LastErr, "[redacted]") || strings.Contains(strings.ToLower(*r.LastErr), "canhan") {
		t.Fatalf("last_error = %v; want the masked SMTP reason", r.LastErr)
	}
	if r.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", r.Attempts)
	}
}
