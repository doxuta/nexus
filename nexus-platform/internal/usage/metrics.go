// Package usage is the repository of sys_usage_daily (specs/066 L4): per-day counters of what a
// tenant's automation did (rule runs, mail, webhooks out and in). Every statement pins tenant_id.
package usage

// Metric names stored in sys_usage_daily.metric.
const (
	WorkflowRuns       = "workflow_runs"
	WorkflowRunsFailed = "workflow_runs_failed"
	EmailsSent         = "emails_sent"
	EmailsFailed       = "emails_failed"
	WebhooksDelivered  = "webhooks_delivered"
	WebhooksFailed     = "webhooks_failed"
	HooksReceived      = "hooks_received"
	HooksFailed        = "hooks_failed"
	// UsersSeen is the count of DISTINCT people who signed in (or refreshed) that day, from refresh_tokens (L5).
	UsersSeen = "users_seen"
)

// Instant gauges (L5): a snapshot of "now", stored under the day the roller took it.
const (
	UsersActive  = "users_active"
	UsersSeen30d = "users_seen_30d"
	Records      = "records" // dim = entity name
	FileBytes    = "file_bytes"
	FileCount    = "file_count"
)

// Metrics lists every counter metric, in display order.
var Metrics = []string{WorkflowRuns, WorkflowRunsFailed, EmailsSent, EmailsFailed,
	WebhooksDelivered, WebhooksFailed, HooksReceived, HooksFailed, UsersSeen}

// EmailSources are the sources of sys_email_log rows (the dim of the mail counters).
var EmailSources = []string{"workflow", "password_reset", "reset_link"}

// Key identifies one counter of one day: the metric and its dimension (” or the mail source).
type Key struct{ Metric, Dim string }

// Mode says how Upsert treats an existing row.
type Mode int

const (
	// Overwrite replaces the stored value (today and yesterday: the source is still complete).
	Overwrite Mode = iota
	// FillMissing keeps an existing row untouched (older days: the janitor may already have
	// deleted part of the source, so a recount could only lower a figure that was right).
	FillMissing
)

// Row is one sys_usage_daily row to write. Day is YYYY-MM-DD in APP_TIMEZONE.
type Row struct {
	TenantID string
	Day      string
	Metric   string
	Dim      string
	Value    int64
}

// DayValue is one metric of one day, summed over its dimensions.
type DayValue struct {
	Day    string `db:"day"`
	Metric string `db:"metric"`
	Value  int64  `db:"value"`
}

// RecordCount is the live row count of one entity.
type RecordCount struct {
	Entity string
	Count  int64
}

// Gauge is one snapshot of what the tenant holds right now.
type Gauge struct {
	UsersActive  int64
	UsersSeen30d int64
	Records      []RecordCount
	FileBytes    int64
	FileCount    int64
}

// Rows turns the snapshot into sys_usage_daily rows for day.
func (g *Gauge) Rows(tenantID, day string) []Row {
	rows := []Row{
		{tenantID, day, UsersActive, "", g.UsersActive},
		{tenantID, day, UsersSeen30d, "", g.UsersSeen30d},
		{tenantID, day, FileBytes, "", g.FileBytes},
		{tenantID, day, FileCount, "", g.FileCount},
	}
	for _, r := range g.Records {
		rows = append(rows, Row{tenantID, day, Records, r.Entity, r.Count})
	}
	return rows
}
