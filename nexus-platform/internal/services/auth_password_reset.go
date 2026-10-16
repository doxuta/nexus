package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"nexus/platform/internal/audit"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/notify"
	"nexus/platform/internal/workflow"
)

// resetTokenTTL is how long an issued password-reset token remains usable. Deliberately
// short — this is a bearer credential mailed in the clear — and unrelated to the JWT/refresh
// TTLs (this is a one-shot, single-use flow, not a session).
const resetTokenTTL = 30 * time.Minute

// resetTokenBytes is the amount of raw entropy (before encoding) in an issued reset token.
// 32 bytes = 256 bits, matching the JWT HS256 key-strength floor used elsewhere in this
// codebase (config.minSecretLength) — comfortably unguessable for a single-use, short-TTL,
// non-authenticated (no rate-limit key beyond IP) secret.
const resetTokenBytes = 32

// resetMailSendTimeout bounds the detached SMTP send spawned off the request path (MED-1) so a
// hung mail server cannot leak a goroutine indefinitely.
// A var only so a test can shrink it; nothing else writes it.
var resetMailSendTimeout = 15 * time.Second

// emailLogWriteTimeout bounds the email-log write that follows a reset-mail send. It gets its OWN
// context: sendCtx may have been spent by a slow transport, and the row is the evidence of it.
const emailLogWriteTimeout = 5 * time.Second

// emailLogInserter is the one email-log method the reset flow needs. Satisfied by *emaillog.Repository.
type emailLogInserter interface {
	Insert(ctx context.Context, e emaillog.Entry) error
}

// WithEmailLog makes the reset mail and the admin-issued reset link (and their failures) lines of the
// tenant's email log (specs/066 L3). The write happens in the same detached goroutine as the send,
// after it — never on the request — so it adds no signal to RequestPasswordReset's answer or latency
// (MED-1). A nil log leaves logging off; a failed write only warns.
func (s *AuthService) WithEmailLog(w emailLogInserter) *AuthService {
	s.emailLog = w
	return s
}

// logResetMail writes the outcome of one reset mail. Called only from the send goroutine, after
// Send. The row keeps nothing the mail carried: the recipient is masked, the subject is the fixed
// reset title, and the error text is stripped of the address, the mail body and the token.
func (s *AuthService) logResetMail(source, tenantID, userID, createdBy string, mail notify.Mail, rawToken, resetURL string, sendErr error) {
	if s.emailLog == nil {
		return
	}
	e := emaillog.Entry{
		ID: s.newID(), TenantID: tenantID, Source: source, Status: emaillog.StatusSent,
		RecipientMasked: workflow.MaskRecipients(mail.To), SubjectTemplate: mail.Subject,
		UserID: userID, CreatedBy: createdBy, Attempts: 1, Finished: true,
	}
	if sendErr != nil {
		e.Status = emaillog.StatusFailed
		// Redact the credential-bearing parts BEFORE masking the address: masking edits the text and
		// would stop the body from matching.
		cause := errors.New(redactResetMailError(sendErr.Error(), mail.Body, mail.HTMLBody, resetURL, rawToken))
		e.LastError = workflow.MaskRecipientError("services.AuthService.sendResetMail", mail.To, cause).Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), emailLogWriteTimeout)
	defer cancel()
	if err := s.emailLog.Insert(ctx, e); err != nil && s.logger != nil {
		s.logger.Warn("services.AuthService.logResetMail: email log insert failed (mail result unchanged)",
			zap.String("tenant_id", tenantID), zap.String("source", source), zap.Error(err))
	}
}

// redactResetMailError blanks the parts of a transport error that would be a bearer credential: the
// mail body (an SMTP server may echo it), the reset link and the raw token. Longest first, so the
// body goes before the link inside it; empty needles are skipped (they would match everywhere).
func redactResetMailError(msg string, secrets ...string) string {
	var pairs []string
	for _, sec := range secrets {
		if sec != "" {
			pairs = append(pairs, sec, "[redacted]")
		}
	}
	if len(pairs) == 0 {
		return msg
	}
	return strings.NewReplacer(pairs...).Replace(msg)
}

// resetRepo is the subset of auth.PasswordResetTokenRepository the reset flow needs.
// Defined as a consumer-side interface (like userRepo/refreshRepo) so the service stays
// unit-testable with an in-memory fake and does not import a concrete GORM type.
type resetRepo interface {
	Store(ctx context.Context, prt *auth.PasswordResetToken) error
	// FindByHash is tenant-agnostic BY DESIGN (../specs/002-password-reset/spec.md §"#1" MUST-FIX 1):
	// the reset-password request body carries {token,newPassword} with no tenant. The
	// service derives tenant_id from the returned row and never accepts a client-supplied one.
	FindByHash(ctx context.Context, tokenHash string) (*auth.PasswordResetToken, error)
	// ConsumeByHash is tenant-pinned: the caller MUST pass the tenantID resolved from the row
	// FindByHash already returned, never from client input.
	ConsumeByHash(ctx context.Context, tenantID, tokenHash string) (bool, error)
}

// WithResetRepo injects the tenant-agnostic-lookup / tenant-pinned-write reset-token
// repository. A nil argument leaves the service without reset-password support (both
// RequestPasswordReset and ResetPassword return a wrapped "not configured" error), which
// keeps every existing 3-arg NewAuthService call site (main.go, unit tests) unchanged.
// Returns the service for fluent wiring, mirroring WithNotifier/WithLogger.
func (s *AuthService) WithResetRepo(r resetRepo) *AuthService {
	if r != nil {
		s.resetRepo = r
	}
	return s
}

// WithResetMailer injects the token-capable email sender + envelope From + reset-link base
// URL used by RequestPasswordReset (../specs/002-password-reset/spec.md §"#1" MUST-FIX 5). This is a
// SEPARATE sender/path from the notify.Notifier security-alert dispatcher: the alert
// dispatcher's renderAlert forbids interpolating secrets by invariant, so a token-bearing
// reset link cannot travel through it — notify.RenderResetMail is the one sanctioned
// token-bearing render path. The send itself is dispatched OFF the request path via
// runAsync (default `go fn()`, 15s bound — MED-1 anti-latency-oracle, see WithMailDispatcher);
// failures are logged, never surfaced to the requester. A nil sender leaves
// reset-mail sending disabled (RequestPasswordReset returns a wrapped error if ever reached
// with resetSender == nil, but see ResetPassword's confirmation path which is unaffected —
// it always uses the async notify.Notifier like ChangePassword).
func (s *AuthService) WithResetMailer(sender notify.EmailSender, from, resetURLBase string) *AuthService {
	if sender != nil {
		s.resetSender = sender
	}
	s.resetFrom = from
	s.resetURLBase = strings.TrimSpace(resetURLBase)
	return s
}

// WithMailDispatcher overrides how the token-bearing reset email is dispatched relative to the
// request. Defaults to `go fn()` (fire-and-forget) so RequestPasswordReset returns BEFORE the
// SMTP round-trip: a synchronous send would leak account existence via response LATENCY (a known
// address pays the SMTP round-trip, an unknown one returns immediately), and a send failure would
// flip the HTTP status (known→500, unknown→200) into a deterministic enumeration oracle (MED-1).
// Tests inject a synchronous runner for deterministic assertions; a production deployment could
// inject a bounded worker pool. A nil runner is ignored.
func (s *AuthService) WithMailDispatcher(run func(func())) *AuthService {
	if run != nil {
		s.runAsync = run
	}
	return s
}

// RequestPasswordReset issues a single-use password-reset token for the given tenant slug +
// email and emails a reset link to the account's address, OR does nothing at all when the
// tenant or user is unknown.
//
// Anti-enumeration contract (../specs/002-password-reset/spec.md §"#1" MUST-FIX 7): the caller (handler)
// ALWAYS returns the identical 200 {"data":{"ok":true}} regardless of what this method does
// internally. For an unknown tenant or unknown/inactive user this method creates ZERO token
// rows and sends ZERO email — it returns nil exactly like the success path, so the handler has
// no signal to leak. Any OTHER error (DB failure, mailer failure) is returned wrapped so the
// caller can log/500 rather than silently pretending success on infrastructure failure — this
// does not weaken anti-enumeration because the not-found branches never reach these calls.
func (s *AuthService) RequestPasswordReset(ctx context.Context, tenantSlug, email string) error {
	if s.resetRepo == nil {
		return fmt.Errorf("services.AuthService.RequestPasswordReset: reset repository not configured")
	}

	tenant := strings.TrimSpace(tenantSlug)
	addr := strings.TrimSpace(email)
	if tenant == "" || addr == "" {
		// Malformed request, not a credential/enumeration question — mirror Login's
		// missing-field validation error so the handler can 422 it.
		return invalid("RESET_FIELD_REQUIRED", "tenant và email là bắt buộc")
	}

	t, err := s.users.ResolveTenantBySlug(ctx, tenant)
	if err != nil {
		if errors.Is(err, auth.ErrTenantNotFound) {
			// Unknown tenant: silently do nothing (anti-enumeration).
			return nil
		}
		return fmt.Errorf("services.AuthService.RequestPasswordReset: resolve tenant: %w", err)
	}

	u, err := s.users.GetUserByEmail(ctx, t.ID, addr)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			// Unknown user within a known tenant: silently do nothing (anti-enumeration).
			return nil
		}
		return fmt.Errorf("services.AuthService.RequestPasswordReset: load user: %w", err)
	}
	if u.Status != statusActive {
		// A disabled/invited account gets no reset link, same as Login rejecting it — but
		// still folds into the silent "do nothing" branch so the response cannot distinguish
		// "unknown email" from "known-but-disabled email".
		return nil
	}

	raw, err := newResetToken()
	if err != nil {
		return fmt.Errorf("services.AuthService.RequestPasswordReset: generate token: %w", err)
	}

	prt := &auth.PasswordResetToken{
		ID:        s.newID(),
		TenantID:  t.ID,
		UserID:    u.ID,
		TokenHash: auth.HashRefreshToken(raw), // SHA-256 — same hashing helper as refresh tokens
		ExpiresAt: s.now().Add(resetTokenTTL),
		CreatedAt: s.now(),
	}
	if err := s.resetRepo.Store(ctx, prt); err != nil {
		return fmt.Errorf("services.AuthService.RequestPasswordReset: store token: %w", err)
	}

	if s.resetSender == nil {
		// Config bug (WithResetMailer never called): the token is stored but cannot be mailed.
		// Log and return nil — do NOT surface an error, or a misconfigured deploy would turn
		// "known account" (reaches here → 500) vs "unknown account" (returned nil above → 200)
		// into a status oracle. The boot-time config fail-fast (reset_config.go) prevents this in
		// production; this is defense-in-depth.
		if s.logger != nil {
			s.logger.Warn("services.AuthService.RequestPasswordReset: reset mailer not configured; token stored but not sent",
				zap.String("tenant_id", t.ID))
		}
		return nil
	}

	resetURL := s.resetURLBase + "?token=" + raw
	subject, text, htmlBody := notify.RenderResetMail(resetURL)
	mail := notify.Mail{To: u.Email, From: s.resetFrom, Subject: subject, Body: text, HTMLBody: htmlBody}

	// Send the reset link OFF the request path (MED-1, flagged by security-auditor + reviewer):
	//  - a synchronous SMTP round-trip leaks account existence via response LATENCY even though the
	//    response bodies are byte-identical (a known address pays ~SMTP, an unknown one returns now);
	//  - a Send() error on that path would flip the HTTP status (known→500 vs unknown→200) into a
	//    deterministic oracle under any mail-transport hiccup.
	// Fire-and-forget with a detached, time-bounded context; a failure is logged, never surfaced.
	run := s.runAsync
	if run == nil {
		run = func(fn func()) { go fn() }
	}
	sender, logger, tenantID := s.resetSender, s.logger, t.ID
	run(func() {
		sendCtx, cancel := context.WithTimeout(context.Background(), resetMailSendTimeout)
		defer cancel()
		err := sender.Send(sendCtx, mail)
		if err != nil && logger != nil {
			logger.Warn("services.AuthService.RequestPasswordReset: reset mail send failed (dropped, fail-open)",
				zap.String("tenant_id", tenantID), zap.Error(err))
		}
		// specs/066 L3: the email log is written HERE, in the goroutine, after the send — never on the
		// request (a DB write only a known account pays for would be a latency oracle, MED-1). The
		// unknown-account branches returned above, so they write nothing.
		s.logResetMail(emaillog.SourcePasswordReset, tenantID, u.ID, "", mail, raw, resetURL, err)
	})
	return nil
}

// ResetPassword consumes a single-use reset token and sets a new password.
//
// Flow (../specs/002-password-reset/spec.md §"#1" MUST-FIX 1/3/4/7, order is load-bearing):
//  1. Validate the new password policy FIRST — a weak attempt must NOT burn the token (MUST-FIX
//     3), so the caller can retry the same link with a stronger password.
//  2. FindByHash (tenant-agnostic) to locate the row and derive tenant_id FROM it — the request
//     body carries no tenant and none is ever trusted from the client (MUST-FIX 1).
//  3. Load the user (tenant-pinned) and verify user.TenantID == token row's TenantID as a
//     defense-in-depth cross-check (the load is already tenant-pinned so this can only fail on
//     a corrupt row, but the spec explicitly calls for the explicit check).
//  4. ConsumeByHash (tenant-pinned, atomic conditional UPDATE) — single winner under concurrent
//     double-submit (MUST-FIX 4); any 0-rows-affected outcome (already used / expired / vanished
//     between steps) collapses to ErrResetTokenInvalid.
//  5. Hash + persist the new password, revoke all sessions, and fire the SAME
//     AlertPasswordChanged confirmation ChangePassword uses (async, token-free).
func (s *AuthService) ResetPassword(ctx context.Context, rawToken, newPassword string) error {
	if s.resetRepo == nil {
		return fmt.Errorf("services.AuthService.ResetPassword: reset repository not configured")
	}

	raw := strings.TrimSpace(rawToken)
	if raw == "" {
		return auth.ErrResetTokenInvalid
	}

	// 1. Validate password policy BEFORE touching the token (MUST-FIX 3) — shared body
	//    (RM-17, password_policy.go), same rules as CreateUser/UpdateUser/ChangePassword.
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}

	// 2. Tenant-agnostic lookup; tenant_id is derived from the row, never from the client.
	tokenHash := auth.HashRefreshToken(raw)
	prt, err := s.resetRepo.FindByHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, auth.ErrResetTokenInvalid) {
			return auth.ErrResetTokenInvalid
		}
		return fmt.Errorf("services.AuthService.ResetPassword: find token: %w", err)
	}

	// 3. Load the user tenant-pinned to the row's OWN tenant, then cross-check (MUST-FIX 1).
	u, err := s.users.GetUserByID(ctx, prt.TenantID, prt.UserID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return auth.ErrResetTokenInvalid
		}
		return fmt.Errorf("services.AuthService.ResetPassword: load user: %w", err)
	}
	if u.TenantID != prt.TenantID {
		// Defense-in-depth: cannot happen via the tenant-pinned load above, but guards
		// against a future refactor that loosens GetUserByID's WHERE clause.
		return auth.ErrResetTokenInvalid
	}
	if u.Status != statusActive {
		// LOW-2 defense-in-depth: RequestPasswordReset only gates issuance on active status; an
		// account disabled AFTER the token was issued must not be able to complete a reset. Reject
		// BEFORE ConsumeByHash so the token stays unconsumed, and collapse to the generic invalid
		// error so nothing about account state leaks.
		return auth.ErrResetTokenInvalid
	}

	// 4. Atomic single-use claim (tenant-pinned, derived tenant — never client input).
	consumed, err := s.resetRepo.ConsumeByHash(ctx, prt.TenantID, tokenHash)
	if err != nil {
		return fmt.Errorf("services.AuthService.ResetPassword: consume token: %w", err)
	}
	if !consumed {
		// Already used, expired, or lost a concurrent race — no signal which (anti-probe).
		return auth.ErrResetTokenInvalid
	}

	// 5. Hash + persist the new password (bcrypt cost 12 — bcryptCost constant, user_service.go).
	hashed, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcryptCost)
	if err != nil {
		return fmt.Errorf("services.AuthService.ResetPassword: hash: %w", err)
	}
	if err := s.users.UpdatePasswordHash(ctx, prt.TenantID, prt.UserID, string(hashed)); err != nil {
		return fmt.Errorf("services.AuthService.ResetPassword: update hash: %w", err)
	}

	// 5b. Clear any account lockout (security MED-1): a valid reset token proves account control,
	//     so a locked-out victim's forgot-password flow MUST restore password login. Without this
	//     the durable lock (failed_login_count/locked_until) never decays and a lockout-DoS is
	//     permanent — the victim resets their password but still gets 401. Fail-open (the hash
	//     already changed; a stale lock counter is far less bad than blocking the reset).
	if err := s.users.ResetLoginFailures(ctx, prt.TenantID, prt.UserID); err != nil && s.logger != nil {
		s.logger.Error("services.AuthService.ResetPassword: clear lockout",
			zap.String("tenant_id", prt.TenantID), zap.Error(err))
	}

	// 6. End EVERY session (the requester is not logged in, so nothing to keep) — refresh rows
	//    'ended' + guard invalidated, so access tokens die on their next request (#310), same as
	//    ChangePassword. A failure here is real: the password already changed, so we must not
	//    silently leave live sessions carrying the old credential.
	if err := s.endAllSessions(ctx, prt.TenantID, prt.UserID, ""); err != nil {
		return fmt.Errorf("services.AuthService.ResetPassword: end sessions: %w", err)
	}

	// 7. Confirmation alert — REUSES the existing token-free AlertPasswordChanged path
	//    (async, fail-open, audited), exactly like ChangePassword.
	s.enqueueAlert(ctx, notify.SecurityEvent{
		Type:     notify.AlertPasswordChanged,
		TenantID: prt.TenantID,
		UserID:   prt.UserID,
		Email:    u.Email,
		At:       time.Now().UTC(),
	})

	return nil
}

// ResetLinkIssue is what POST /admin/users/:id/reset-password-link returns to the ADMIN (W7
// invite flow): the raw one-shot token plus the composed link, so the admin can hand it to the
// user out-of-band. EmailQueued reports whether a copy was ALSO dispatched to the user's
// address (true only when the reset mailer is wired; the send is async fire-and-forget — the
// same MED-1 dispatch RequestPasswordReset uses — so "queued", not "delivered").
type ResetLinkIssue struct {
	ResetURL    string    `json:"resetLink"`
	Token       string    `json:"token"`
	ExpiresAt   time.Time `json:"expiresAt"`
	TTLMinutes  int       `json:"ttlMinutes"`
	EmailQueued bool      `json:"emailQueued"`
}

// IssueResetLinkForUser mints a password-reset token for a SPECIFIC user on behalf of an admin
// (W7 §3.13 "invite flow", the minimal honest version): the admin creates the user (any
// password), calls this, and hands the returned link to the user — who then sets their own
// password through the EXACT forgot-password consume path (POST /auth/reset-password: same
// token shape, same SHA-256 at-rest hashing, same single-use CAS, same 30-minute
// resetTokenTTL, same session revocation). Nothing is duplicated from that machinery — this
// is RequestPasswordReset with (a) the target addressed by verified tenant + user id instead
// of slug + email, (b) the raw token RETURNED to the (already-authenticated admin) caller
// instead of being email-only, and (c) no anti-enumeration silence: the caller is an admin,
// so an unknown user is an honest 404 (USER_NOT_FOUND) and a non-active target an honest 422
// (USER_NOT_ACTIVE — a disabled account must not receive a live credential-reset link;
// ResetPassword would refuse the consume anyway, LOW-2).
//
// The handler mounts this admin-gated (RequireAdmin); tenantID is ALWAYS the verified claim.
func (s *AuthService) IssueResetLinkForUser(ctx context.Context, tenantID, userID string) (*ResetLinkIssue, error) {
	if s.resetRepo == nil {
		return nil, fmt.Errorf("services.AuthService.IssueResetLinkForUser: reset repository not configured")
	}
	tenantID = strings.TrimSpace(tenantID)
	userID = strings.TrimSpace(userID)
	if tenantID == "" || userID == "" {
		return nil, invalid("USER_NOT_FOUND", "user không tồn tại")
	}

	// Tenant-pinned load: a cross-tenant / unknown id is a 404, honestly (admin caller).
	u, err := s.users.GetUserByID(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, invalid("USER_NOT_FOUND", "user không tồn tại")
		}
		return nil, fmt.Errorf("services.AuthService.IssueResetLinkForUser: load user: %w", err)
	}
	if u.Status != statusActive {
		return nil, invalid("USER_NOT_ACTIVE", "user không ở trạng thái active — kích hoạt trước khi cấp link đặt mật khẩu")
	}

	raw, err := newResetToken()
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.IssueResetLinkForUser: generate token: %w", err)
	}
	expiresAt := s.now().Add(resetTokenTTL)
	prt := &auth.PasswordResetToken{
		ID:        s.newID(),
		TenantID:  tenantID,
		UserID:    u.ID,
		TokenHash: auth.HashRefreshToken(raw), // same at-rest hashing as forgot-password
		ExpiresAt: expiresAt,
		CreatedAt: s.now(),
	}
	if err := s.resetRepo.Store(ctx, prt); err != nil {
		return nil, fmt.Errorf("services.AuthService.IssueResetLinkForUser: store token: %w", err)
	}

	// Governance trail (security W-1): minting a reset link is a credential-granting act —
	// record WHO minted for WHOM. Never the token or its hash.
	s.auditor.Record(ctx, audit.Event{
		TenantID:    tenantID,
		ActorUserID: audit.ActorFromContext(ctx),
		EntityName:  audit.UserMeta,
		RecordID:    u.ID,
		Action:      audit.ActionUpdate,
		Diff:        &audit.Diff{After: map[string]any{"resetLinkIssued": true, "expiresAt": expiresAt.UTC().Format(time.RFC3339)}},
		At:          s.now(),
	})

	resetURL := ""
	if s.resetURLBase != "" {
		resetURL = s.resetURLBase + "?token=" + raw
	}

	issue := &ResetLinkIssue{
		ResetURL:   resetURL,
		Token:      raw,
		ExpiresAt:  expiresAt.UTC(),
		TTLMinutes: int(resetTokenTTL / time.Minute),
	}

	// Best-effort courtesy copy to the user when SMTP is wired — same async, time-bounded,
	// fail-open dispatch as RequestPasswordReset (a mail hiccup must not fail the admin call;
	// the admin already holds the link). No mailer → link-only, still fully functional.
	if s.resetSender != nil && resetURL != "" {
		subject, text, htmlBody := notify.RenderResetMail(resetURL)
		mail := notify.Mail{To: u.Email, From: s.resetFrom, Subject: subject, Body: text, HTMLBody: htmlBody}
		run := s.runAsync
		if run == nil {
			run = func(fn func()) { go fn() }
		}
		sender, logger := s.resetSender, s.logger
		// The issuing admin, read here: the goroutine must not touch the request's ctx.
		actor := audit.ActorFromContext(ctx)
		run(func() {
			sendCtx, cancel := context.WithTimeout(context.Background(), resetMailSendTimeout)
			defer cancel()
			err := sender.Send(sendCtx, mail)
			if err != nil && logger != nil {
				logger.Warn("services.AuthService.IssueResetLinkForUser: reset mail send failed (dropped, fail-open)",
					zap.String("tenant_id", tenantID), zap.Error(err))
			}
			s.logResetMail(emaillog.SourceResetLink, tenantID, u.ID, actor, mail, raw, resetURL, err)
		})
		issue.EmailQueued = true
	}
	return issue, nil
}

// newResetToken returns a URL-safe, base64-encoded 256-bit random token. Errors from
// crypto/rand.Read are checked (never ignored) — a failure here means the platform's CSPRNG is
// broken, which must abort issuance rather than hand out a weak/predictable token.
func newResetToken() (string, error) {
	buf := make([]byte, resetTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("crypto/rand read: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
