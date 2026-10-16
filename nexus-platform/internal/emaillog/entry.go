// Package emaillog is the tenant's email log (specs/066): one sys_email_log row per mail a workflow
// rule sent or failed to send. It stores NO rendered data — the recipient is masked
// ("l***@gmail.com"), the subject is the rule's TEMPLATE and the error is masked at its source —
// so the log can outlive the job payload that queue #95 scrubs.
package emaillog

import "time"

// Source says what produced a mail: a rule, a forgot-password request, or a link a tenant admin issued.
const (
	SourceWorkflow      = "workflow"
	SourcePasswordReset = "password_reset"
	SourceResetLink     = "reset_link"
)

// Status is the delivery state of one mail.
const (
	StatusQueued = "queued"
	StatusSent   = "sent"
	StatusFailed = "failed"
)

// Entry is the WRITE shape of one log row. Empty strings (and nil ActionIndex) are stored as NULL.
type Entry struct {
	ID              string `db:"id" gorm:"primaryKey"`
	TenantID        string `db:"tenant_id"`
	Source          string `db:"source"`
	Status          string `db:"status"`
	RecipientMasked string `db:"recipient_masked"`
	SubjectTemplate string `db:"subject_template"`
	WorkflowID      string `db:"workflow_id"`
	RunID           string `db:"run_id"`
	ActionIndex     *int   `db:"action_index"`
	ActionHash      string `db:"action_hash"`
	EntityName      string `db:"entity_name"`
	RecordID        string `db:"record_id"`
	UserID          string `db:"user_id"`
	JobID           string `db:"job_id"`
	Attempts        int    `db:"attempts"`
	LastError       string `db:"last_error"`
	ResendOf        string `db:"resend_of"`
	CreatedBy       string `db:"created_by"`
	// Finished stamps finished_at = now (the sender that writes a terminal row directly).
	Finished bool `db:"-" gorm:"-"`
}

// Row is the READ shape the admin screen lists (GET /admin/email-log). WorkflowName is null when the
// rule has been deleted.
type Row struct {
	ID              string     `db:"id" json:"id"`
	CreatedAt       time.Time  `db:"created_at" json:"createdAt"`
	FinishedAt      *time.Time `db:"finished_at" json:"finishedAt"`
	Source          string     `db:"source" json:"source"`
	Status          string     `db:"status" json:"status"`
	RecipientMasked string     `db:"recipient_masked" json:"recipientMasked"`
	SubjectTemplate *string    `db:"subject_template" json:"subjectTemplate"`
	WorkflowID      *string    `db:"workflow_id" json:"workflowId"`
	WorkflowName    *string    `db:"workflow_name" json:"workflowName"`
	ActionIndex     *int       `db:"action_index" json:"actionIndex"`
	EntityName      *string    `db:"entity_name" json:"entityName"`
	RecordID        *string    `db:"record_id" json:"recordId"`
	JobID           *string    `db:"job_id" json:"jobId"`
	Attempts        int        `db:"attempts" json:"attempts"`
	LastError       *string    `db:"last_error" json:"lastError"`
	ResendOf        *string    `db:"resend_of" json:"resendOf"`
	ResentAt        *time.Time `db:"resent_at" json:"resentAt"`
	CreatedBy       *string    `db:"created_by" json:"createdBy"`
	// CreatedByName is the issuer's full name from users of the SAME tenant; null when there is no
	// issuer, or the id is no user of this tenant (a builder operator) — the screen reads null as
	// "Người dựng".
	CreatedByName *string `db:"created_by_name" json:"createdByName"`
	// ActionHash fingerprints the action's templates at send time (Get only). Internal: the resend
	// compares it with the live action to say "the rule changed since"; it is never serialized.
	ActionHash *string `db:"action_hash" json:"-"`
	// CanResend is set by the service (the resend policy), not read from a column.
	CanResend bool `db:"-" json:"canResend"`
}

// Filter narrows ListForTenant. Empty Status / Source = any; Limit is clamped by ClampLimit; After is
// the opaque cursor of the previous page ("" = first page).
type Filter struct {
	Status string
	Source string
	Limit  int
	After  string
}
