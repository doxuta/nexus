package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/workflow"
)

// emailLogStore is the log the service needs. Satisfied by *emaillog.Repository; every method pins
// tenant_id.
type emailLogStore interface {
	ListForTenant(ctx context.Context, tenantID string, f emaillog.Filter) ([]emaillog.Row, string, error)
	Get(ctx context.Context, tenantID, id string) (*emaillog.Row, error)
	LockResend(ctx context.Context, tenantID, id string, at time.Time) (bool, error)
	ReleaseResend(ctx context.Context, tenantID, id string) error
}

// emailRenderer builds the email a resend would send from the LIVE rule and the record's current data
// (specs/066 FR-032). Satisfied by *WorkflowService.
type emailRenderer interface {
	RenderEmailForRecord(ctx context.Context, tenantID, workflowID string, actionIndex int, recordID string) (workflow.RenderedEmail, workflow.Action, error)
}

// EmailLogService serves the customer admin's email log (specs/066, "Nhật ký email") and its "Gửi lại".
type EmailLogService struct {
	store          emailLogStore
	renderer       emailRenderer
	sender         workflow.EmailSender
	auditor        audit.Auditor
	mailConfigured bool
	queueEnabled   bool
	now            func() time.Time
}

// NewEmailLogService wires the service over the email-log store (no resend until WithRenderer and
// WithSender are called; no audit until WithAuditor).
func NewEmailLogService(store emailLogStore) *EmailLogService {
	return &EmailLogService{store: store, auditor: audit.NewNoopAuditor(), now: time.Now}
}

// WithMailConfigured says whether the server can deliver mail at all (ALERTS_ENABLED); the screen
// warns when it cannot, because then every row is a mail that was never sent. Chainable.
func (s *EmailLogService) WithMailConfigured(v bool) *EmailLogService { s.mailConfigured = v; return s }

// WithQueueEnabled says whether the durable queue runs (JOBS_ENABLED). Chainable.
func (s *EmailLogService) WithQueueEnabled(v bool) *EmailLogService { s.queueEnabled = v; return s }

// WithRenderer injects the live-rule renderer of the resend (production: the WorkflowService). Chainable.
func (s *EmailLogService) WithRenderer(r emailRenderer) *EmailLogService { s.renderer = r; return s }

// WithSender injects the configured rule-email sender (production: the SAME instance the engine uses,
// queue or direct, so a resend writes its log row the way every rule email does). Chainable.
func (s *EmailLogService) WithSender(sender workflow.EmailSender) *EmailLogService {
	s.sender = sender
	return s
}

// WithAuditor injects the RM-19 audit dispatcher: every resend writes an __email__ row naming who
// pressed it. Chainable; nil is ignored.
func (s *EmailLogService) WithAuditor(a audit.Auditor) *EmailLogService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// MailConfigured reports ALERTS_ENABLED.
func (s *EmailLogService) MailConfigured() bool { return s.mailConfigured }

// QueueEnabled reports JOBS_ENABLED.
func (s *EmailLogService) QueueEnabled() bool { return s.queueEnabled }

var emailLogStatuses = map[string]bool{emaillog.StatusQueued: true, emaillog.StatusSent: true, emaillog.StatusFailed: true}

var emailLogSources = map[string]bool{emaillog.SourceWorkflow: true, emaillog.SourcePasswordReset: true, emaillog.SourceResetLink: true}

// resendable is the resend policy over a row: a rule's mail (never an auth mail — its token is single
// use; issue a new link instead), failed, not resent yet, and still pointing at a rule slot and a
// record to rebuild it from.
func resendable(r *emaillog.Row) bool {
	return r.Source == emaillog.SourceWorkflow && r.Status == emaillog.StatusFailed && r.ResentAt == nil &&
		r.WorkflowID != nil && r.ActionIndex != nil && r.RecordID != nil
}

// List returns one page of the tenant's email log, newest first, and the next page's cursor ("" on
// the last page). An unknown status is EMAIL_LOG_FILTER_INVALID (a filter that would silently match
// nothing is a client bug); a malformed cursor comes back wrapping dynrepo.ErrInvalidCursor. Each row
// carries canResend: the policy AND a rule that still exists (a button that can only 409 is noise).
func (s *EmailLogService) List(ctx context.Context, tenantID string, f emaillog.Filter) ([]emaillog.Row, string, error) {
	if f.Status != "" && !emailLogStatuses[f.Status] {
		return nil, "", invalid("EMAIL_LOG_FILTER_INVALID", "bộ lọc không hợp lệ: status ∈ queued|sent|failed")
	}
	if f.Source != "" && !emailLogSources[f.Source] {
		return nil, "", invalid("EMAIL_LOG_FILTER_INVALID", "bộ lọc không hợp lệ: source ∈ workflow|password_reset|reset_link")
	}
	rows, next, err := s.store.ListForTenant(ctx, tenantID, f)
	if err != nil {
		return nil, "", fmt.Errorf("services.EmailLogService.List: %w", err)
	}
	for i := range rows {
		rows[i].CanResend = resendable(&rows[i]) && rows[i].WorkflowName != nil
	}
	return rows, next, nil
}

// resendDeadline bounds the post-lock phase of a resend (one enqueue or one SMTP exchange + log writes).
const resendDeadline = 30 * time.Second

// ErrEmailResendFailed: the resend took the one-shot lock but the mail could not be handed to the
// sender. The result still carries the new failed row (when one exists) so it can be resent again.
var ErrEmailResendFailed = errors.New("services: email resend failed after the lock")

// ResendResult is the answer of Resend. DryRun=true is the preview (no Entry); DryRun=false carries the
// new log row (nil only when the sender's fail-open log write was lost).
type ResendResult struct {
	DryRun               bool
	RecipientMasked      string
	Subject              string
	RuleChangedSinceSend bool
	Entry                *emaillog.Row
}

// Resend sends a failed rule email again, REBUILT from the live rule and the record's current data —
// not replayed (the original payload is scrubbed the moment a job ends, queue #95). Order:
//
//	row of this tenant (404 EMAIL_LOG_NOT_FOUND, byte-identical for a foreign or unknown id) →
//	policy (409 EMAIL_NOT_RESENDABLE) → mail configured (503 MAIL_NOT_CONFIGURED) → wiring (500
//	EMAIL_RESEND_UNAVAILABLE) → render (409 …_RULE_GONE / …_RULE_DISABLED / …_RECORD_GONE, 422 …_NO_RECIPIENT) →
//	dryRun nil|true: preview, nothing written → CAS resent_at (the loser gets 409) → send →
//	__email__ audit.
//
// The body never chooses the recipient or the content (FR-038): both come out of the rule's templates.
func (s *EmailLogService) Resend(ctx context.Context, tenantID, actorID, id string, dryRun *bool) (*ResendResult, error) {
	row, err := s.store.Get(ctx, tenantID, id)
	if errors.Is(err, emaillog.ErrNotFound) {
		return nil, invalid("EMAIL_LOG_NOT_FOUND", "Không tìm thấy dòng nhật ký email")
	}
	if err != nil {
		return nil, fmt.Errorf("services.EmailLogService.Resend: %w", err)
	}
	if !resendable(row) {
		return nil, invalid("EMAIL_NOT_RESENDABLE", "Chỉ gửi lại được email của quy tắc đã lỗi và chưa gửi lại")
	}
	if !s.mailConfigured {
		return nil, invalid("MAIL_NOT_CONFIGURED", "Máy chủ chưa cấu hình gửi mail — gửi lại sẽ không đi đâu")
	}
	if s.renderer == nil || s.sender == nil {
		// A wiring fault must be loud: no fallback to another repository or a no-op sender.
		return nil, fmt.Errorf("services.EmailLogService.Resend: %w", ErrEmailResendUnavailable)
	}
	mail, action, err := s.renderer.RenderEmailForRecord(ctx, tenantID, *row.WorkflowID, *row.ActionIndex, *row.RecordID)
	switch {
	case errors.Is(err, ErrEmailRuleGone):
		return nil, invalid("EMAIL_RESEND_RULE_GONE", "Quy tắc không còn email này — không dựng lại được thư")
	case errors.Is(err, ErrEmailRuleDisabled):
		return nil, invalid("EMAIL_RESEND_RULE_DISABLED", "Quy tắc đang tắt — bật lại quy tắc rồi mới gửi lại được")
	case errors.Is(err, ErrEmailRecordGone):
		return nil, invalid("EMAIL_RESEND_RECORD_GONE", "Bản ghi không còn (đã xoá hoặc nằm trong thùng rác) — không gửi lại được")
	case err != nil:
		return nil, fmt.Errorf("services.EmailLogService.Resend: render: %w", err)
	}
	if strings.TrimSpace(mail.To) == "" {
		return nil, invalid("EMAIL_RESEND_NO_RECIPIENT", "Bản ghi không có địa chỉ nhận — sửa bản ghi rồi gửi lại")
	}
	hash := workflow.EmailActionHash(action)
	if dryRun == nil || *dryRun {
		return &ResendResult{DryRun: true, RecipientMasked: workflow.MaskRecipients(mail.To), Subject: mail.Subject,
			RuleChangedSinceSend: row.ActionHash != nil && *row.ActionHash != "" && *row.ActionHash != hash}, nil
	}

	at := s.now()
	won, err := s.store.LockResend(ctx, tenantID, id, at)
	if err != nil {
		return nil, fmt.Errorf("services.EmailLogService.Resend: %w", err)
	}
	if !won {
		return nil, invalid("EMAIL_NOT_RESENDABLE", "Chỉ gửi lại được email của quy tắc đã lỗi và chưa gửi lại")
	}
	// The lock is taken: from here the hand-over, its log row and the audit must finish even if the
	// admin's browser goes away (a cancelled ctx would fail the enqueue AND the "mark failed" that
	// follows it, leaving a locked old row and a row stuck queued). Detached, but bounded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resendDeadline)
	defer cancel()
	newID := uuid.NewString()
	sendCtx := workflow.WithEmailOrigin(ctx, workflow.EmailOrigin{
		TenantID: tenantID, WorkflowID: *row.WorkflowID, ActionIndex: *row.ActionIndex,
		EntityName: derefStr(row.EntityName), RecordID: *row.RecordID,
		SubjectTemplate: action.Subject, ActionHash: hash,
		LogID: newID, ResendOf: id, CreatedBy: actorID,
	})
	sendErr := s.sender.SendEmail(sendCtx, mail.To, mail.Subject, mail.Body)
	// Even a failed hand-over was a resend attempt by this person on this row.
	s.auditor.Record(ctx, audit.Event{
		TenantID: tenantID, ActorUserID: actorID, EntityName: audit.EmailMeta, RecordID: id,
		Action: audit.ActionCreate, At: at,
		Diff: &audit.Diff{After: map[string]any{"resendOf": id, "newLogId": newID}},
	})
	// The new row is read back for the answer. Not found = the senders' fail-open log write was lost;
	// any other read error means "unknown" (the row may or may not exist): it only costs the echo, and
	// the lock is deliberately kept — releasing it could double-send if the row does exist.
	entry, getErr := s.store.Get(ctx, tenantID, newID)
	rowLost := errors.Is(getErr, emaillog.ErrNotFound)
	if entry != nil {
		entry.CanResend = resendable(entry) && entry.WorkflowName != nil
	}
	if sendErr != nil {
		if rowLost {
			// Nothing carries the retry (the sender's fail-open log write was lost too): give the
			// right back, or the mail would be lost for good.
			if rerr := s.store.ReleaseResend(ctx, tenantID, id); rerr != nil {
				return nil, fmt.Errorf("services.EmailLogService.Resend: release after failed send: %w (send: %v)", rerr, sendErr)
			}
		}
		return &ResendResult{Entry: entry}, fmt.Errorf("services.EmailLogService.Resend: %w: %w", ErrEmailResendFailed, sendErr)
	}
	return &ResendResult{Entry: entry}, nil
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
