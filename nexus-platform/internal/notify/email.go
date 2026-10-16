package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Mail is a single rendered message handed to an EmailSender. Body is plain text;
// HTMLBody is the optional HTML alternative (empty = text-only). None of these fields
// ever contain a token, hash or password (the templates guarantee it) EXCEPT when the
// mail was built by notify.RenderResetMail (the one sanctioned token-bearing path — a
// password-reset link is, by definition, the token; see reset_mail.go).
//
// From optionally overrides the envelope/header sender for this one message (e.g. the
// password-reset flow's RESET_MAIL_FROM). Empty means "use the sender's configured
// default" (SMTPConfig.From) — this keeps every existing caller (security alerts,
// workflow email_alert) byte-identical.
type Mail struct {
	To       string
	From     string
	Subject  string
	Body     string
	HTMLBody string
}

// EmailSender abstracts the email transport so the dispatcher never knows about SMTP.
// Implementations: SMTPSender (real, provider-agnostic) and NoopSender (disabled/test).
// Send must be safe for concurrent use by multiple dispatcher workers.
type EmailSender interface {
	// Send delivers one message. It returns an error on delivery failure; the caller
	// (dispatcher worker) records the failure in the audit row but does not propagate
	// it to the business flow (fail-open).
	Send(ctx context.Context, m Mail) error
}

// NoopSender is the EmailSender wired when ALERTS_ENABLED=false or in tests. It records
// nothing externally and never returns an error — it simply does not send mail.
type NoopSender struct{}

// NewNoopSender returns an EmailSender that discards every message and SAYS SO.
func NewNoopSender() *NoopSender { return &NoopSender{} }

// ErrMailNotConfigured is returned by NoopSender: the message was NOT sent because this server has
// no mail configured (ALERTS_ENABLED is false, the default).
//
// It used to return nil — "discards the message and reports success". That made a workflow run
// show a green "email sent" step for a mail that never left the process, on the one screen a
// builder opens to check whether the automation works. Nothing customer-facing in any vertical
// could be trusted, and nobody could tell, which is why no pack ever shipped an email action.
//
// A typed sentinel rather than a generic error so callers can say WHY without string-matching, and
// so this reads as a configuration state rather than a fault.
var ErrMailNotConfigured = errors.New("notify: mail not configured on this server (ALERTS_ENABLED=false) — message discarded")

// Send discards the message and reports that it did.
func (*NoopSender) Send(context.Context, Mail) error { return ErrMailNotConfigured }

// SMTPConfig holds the provider-agnostic SMTP parameters (spec §9: works with Gmail /
// SendGrid / SES / Mailgun by changing env only). It mirrors config.AlertsConfig so the
// caller can build it without importing config into this package.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

// SMTPSender delivers mail over SMTP with STARTTLS, authenticating with PLAIN when a
// username is configured. It is provider-agnostic: any standards-compliant submission
// server works by changing the host/port/credentials (spec §9, no provider lock-in).
type SMTPSender struct {
	cfg     SMTPConfig
	timeout time.Duration
	// dialer is injectable so unit tests can drive an in-memory SMTP server; production
	// uses net.Dialer. It returns a raw connection to host:port.
	dial func(ctx context.Context, addr string) (net.Conn, error)
}

// NewSMTPSender builds an SMTPSender from the resolved SMTP configuration. From defaults
// to Username when SMTP_FROM is unset so a misconfigured From never produces an empty
// envelope sender.
func NewSMTPSender(cfg SMTPConfig) *SMTPSender {
	if strings.TrimSpace(cfg.From) == "" {
		cfg.From = cfg.Username
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &SMTPSender{
		cfg:     cfg,
		timeout: 15 * time.Second,
		dial: func(ctx context.Context, addr string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", addr)
		},
	}
}

// Send delivers one message over SMTP+STARTTLS. The flow is: dial → EHLO → STARTTLS →
// (AUTH PLAIN if credentials) → MAIL FROM / RCPT TO / DATA. Any step's error is wrapped
// and returned so the worker can audit status=failed; it is never bubbled to the caller
// of EnqueueSecurityAlert (fail-open is enforced one layer up, in the dispatcher).
func (s *SMTPSender) Send(ctx context.Context, m Mail) error {
	// Per-message From override (e.g. RESET_MAIL_FROM): falls back to the sender's
	// configured default so every existing caller that leaves Mail.From empty is unaffected.
	from := s.cfg.From
	if strings.TrimSpace(m.From) != "" {
		from = m.From
	}

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))

	conn, err := s.dial(ctx, addr)
	if err != nil {
		return fmt.Errorf("notify.SMTPSender.Send: dial %s: %w", addr, err)
	}

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("notify.SMTPSender.Send: new client: %w", err)
	}
	defer func() { _ = c.Close() }()

	// STARTTLS: upgrade the plaintext submission connection to TLS before AUTH so the
	// credentials never travel in the clear. ServerName must match the SMTP host cert.
	if ok, _ := c.Extension("STARTTLS"); ok {
		tlsCfg := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("notify.SMTPSender.Send: starttls: %w", err)
		}
	} else {
		// Refuse to fall back to plaintext for a submission that may carry AUTH. A
		// server without STARTTLS on the submission port is a misconfiguration.
		return fmt.Errorf("notify.SMTPSender.Send: server %s does not advertise STARTTLS", s.cfg.Host)
	}

	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("notify.SMTPSender.Send: auth: %w", err)
		}
	}

	if err := c.Mail(from); err != nil {
		return fmt.Errorf("notify.SMTPSender.Send: mail from: %w", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return fmt.Errorf("notify.SMTPSender.Send: rcpt to: %w", err)
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("notify.SMTPSender.Send: data: %w", err)
	}
	if _, err := w.Write([]byte(buildMIME(from, m))); err != nil {
		_ = w.Close()
		return fmt.Errorf("notify.SMTPSender.Send: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("notify.SMTPSender.Send: close data: %w", err)
	}

	if err := c.Quit(); err != nil {
		return fmt.Errorf("notify.SMTPSender.Send: quit: %w", err)
	}
	return nil
}

// buildMIME assembles a minimal multipart/alternative message (text + optional HTML).
// Headers and bodies are UTF-8; the Subject is Q-encoded so non-ASCII (Vietnamese)
// subjects survive transport.
func buildMIME(from string, m Mail) string {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + m.To + "\r\n")
	b.WriteString("Subject: " + encodeHeader(m.Subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")

	if m.HTMLBody == "" {
		b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
		b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
		b.WriteString(m.Body)
		return b.String()
	}

	const boundary = "nexus-alert-boundary-7f3a"
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(m.Body + "\r\n\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(m.HTMLBody + "\r\n\r\n")

	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

// encodeHeader Q-encodes a header value (RFC 2047) so UTF-8 subjects are transport-safe.
func encodeHeader(s string) string {
	return mime.QEncoding.Encode("utf-8", s)
}
