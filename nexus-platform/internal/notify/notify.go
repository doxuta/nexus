// Package notify is the security-alert notification adapter (../specs/014-security-alerts/spec.md
// §3). It lives below the service layer: services depend only on the consumer-side
// Notifier interface and call EnqueueSecurityAlert, which returns immediately. The
// concrete AsyncDispatcher renders a locale template, sends an email through an
// EmailSender adapter (SMTP / noop), and records an audit row in security_alerts.
//
// Invariants (rules + spec §1):
//   - Fail-open: a full buffer or a send error never blocks or breaks the originating
//     business flow (password change, role change, account disable/delete, refresh
//     reuse). Worst case is a dropped/failed alert with a zap log line.
//   - Multi-tenant: every SecurityEvent carries TenantID + UserID; the audit repository
//     filters tenant_id on every query.
//   - No secret leakage: templates contain only the event + time + advice — never a
//     token, hash or password.
package notify

import (
	"context"
	"time"
)

// SecurityEvent is the metadata-driven payload a service emits when a sensitive account
// event happens. It is provider- and channel-agnostic; the dispatcher decides how to
// render and deliver it. Meta carries optional non-sensitive context (e.g. the actor or
// a human label) and MUST NOT contain tokens, hashes or passwords.
type SecurityEvent struct {
	Type     string         // one of the AlertType* constants below
	TenantID string         // owning tenant — pins every audit query
	UserID   string         // account the alert is about
	Email    string         // recipient address snapshot at emit time
	At       time.Time      // when the originating event occurred
	Meta     map[string]any // optional non-sensitive context (never secrets)
}

// Canonical alert types (spec §2 / migration 000009 alert_type values). Services emit
// these strings; templates and the audit row key off them.
const (
	AlertPasswordChanged = "password_changed"
	AlertRoleChanged     = "role_changed"
	AlertAccountDisabled = "account_disabled"
	AlertAccountDeleted  = "account_deleted"
	AlertRefreshReuse    = "refresh_reuse"
	// AlertMFAEnabled / AlertMFADisabled (ENG-AUTH2 #7) fire on POST /auth/mfa/activate and
	// POST /auth/mfa/disable respectively — a change to the account's second-factor posture
	// is exactly the kind of sensitive event this dispatcher exists for (spec §1). A failed
	// verify attempt (wrong TOTP/backup code, on /auth/mfa/verify, /activate, AND /disable) is
	// deliberately NOT alerted here: TOTP typos are routine, so emailing on every miss would
	// train the user to ignore these alerts — a failed attempt is logged instead at Warn level
	// in handlers.AuthHandler.writeMFAError (reviewer LOW-1), never via this Notifier.
	AlertMFAEnabled  = "mfa_enabled"
	AlertMFADisabled = "mfa_disabled"
)

// Notifier is the consumer-side interface the service layer depends on. The single
// method is non-blocking by contract: it enqueues the event and returns immediately,
// so a slow or down SMTP server can never stall a business flow. Implementations must
// be safe for concurrent use.
type Notifier interface {
	// EnqueueSecurityAlert schedules an alert for asynchronous delivery and returns
	// at once. It never blocks on I/O and never returns an error: delivery problems
	// (including a full buffer) are handled out-of-band (logged) by the implementation.
	EnqueueSecurityAlert(ctx context.Context, evt SecurityEvent)
}

// noopNotifier is the default Notifier wired into services so that legacy flows and
// tests run unchanged when alerts are disabled. It discards every event.
type noopNotifier struct{}

// NewNoopNotifier returns a Notifier that drops every event. It is the safe default for
// services (preserving prior behaviour) and for tests that do not exercise delivery.
func NewNoopNotifier() Notifier { return noopNotifier{} }

// EnqueueSecurityAlert discards the event. It deliberately does nothing.
func (noopNotifier) EnqueueSecurityAlert(context.Context, SecurityEvent) {}
