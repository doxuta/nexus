package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/notify"
	"nexus/platform/internal/workflow"
)

// -----------------------------------------------------------------------------------------------
// JOBS-1 first real producer: durable workflow email_alert.
//
// The workflow engine's email_alert action used to hand the rendered mail straight to the SMTP
// transport inside the dispatcher goroutine — a crash or a down SMTP server lost the mail (one
// shot, zero retry). With the durable queue wired (JOBS_ENABLED, the default), the action instead
// ENQUEUES an "email_alert" sys_jobs row (EnqueuingWorkflowEmailSender below) and a queue worker
// delivers it through the SAME notify transport (NewEmailAlertJobHandler): an SMTP failure now
// retries with capped exponential backoff and survives a restart. Behavioural note: the workflow
// run history records the ENQUEUE outcome (delivery is async — its outcome lives on the sys_jobs
// row: attempts/last_error/status), mirroring how any queued send reports.
// -----------------------------------------------------------------------------------------------

// EmailAlertJobType is the sys_jobs job_type of one durable email_alert delivery.
const EmailAlertJobType = "email_alert"

// emailAlertJobPayload is the sys_jobs.payload shape of an EmailAlertJobType job: the ALREADY
// RENDERED mail ({{field}} interpolation happens at enqueue time in the workflow action, so the
// delivery retry never re-reads the record — it sends exactly what the triggering event saw).
// It carries personal data, so main.go registers the type with jobs.SQLStore.ScrubPayloadOnFinish:
// the payload lives exactly as long as a retry may still need it (queue #95).
type emailAlertJobPayload struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	// specs/059: the run-history row (+ the action's slot in it) the delivery outcome belongs to,
	// so a delivery the queue gives up on turns THAT row failed. Config ids, no personal data;
	// absent on a job enqueued outside a recorded run.
	WorkflowID  string `json:"workflowId,omitempty"`
	RunID       string `json:"runId,omitempty"`
	ActionIndex int    `json:"actionIndex,omitempty"`
	// specs/066: the sys_email_log row this delivery updates when it ends. An id (config, not
	// personal data); absent on a job parked before the log existed or when the row was not written.
	LogID string `json:"logId,omitempty"`
}

// JobEnqueuer is the consumer-side slice of the jobs store the enqueuing sender needs.
// Satisfied by *jobs.SQLStore.
type JobEnqueuer interface {
	Enqueue(ctx context.Context, j jobs.Job) (string, error)
}

// EnqueuingWorkflowEmailSender implements workflow.EmailSender by parking the rendered mail as a
// durable job instead of sending inline. Inside a recorded run (workflow.RunRefFrom — every
// dispatcher, timer and scheduler firing) the job is TENANT-PINNED and names its run row + action
// slot (specs/059): the tenant admin sees it in "Việc nền", and a delivery the queue gives up on
// turns that run failed. Outside a run, a mail whose ctx carries a workflow.EmailOrigin is pinned to
// the origin's tenant (specs/066 FR-013); with neither it stays a system job (tenant_id NULL).
type EnqueuingWorkflowEmailSender struct {
	queue  JobEnqueuer
	log    emailLogWriter
	logger *zap.Logger
}

// NewEnqueuingWorkflowEmailSender wires the sender over the jobs store.
func NewEnqueuingWorkflowEmailSender(queue JobEnqueuer) *EnqueuingWorkflowEmailSender {
	return &EnqueuingWorkflowEmailSender{queue: queue}
}

// WithEmailLog makes the sender write the tenant's email log (specs/066): a "queued" row BEFORE the
// enqueue (so the row exists before any worker can claim the job), the job id attached after it.
// A log failure never stops the mail (fail-open, D-6). Chainable; nil log = no logging.
func (s *EnqueuingWorkflowEmailSender) WithEmailLog(log emailLogWriter, logger *zap.Logger) *EnqueuingWorkflowEmailSender {
	if logger == nil {
		logger = zap.NewNop()
	}
	s.log, s.logger = log, logger
	return s
}

// SendEmail enqueues one email_alert job carrying the rendered mail. An enqueue failure returns
// an error so the workflow action records the failure (fail-open in the dispatcher, as before).
func (s *EnqueuingWorkflowEmailSender) SendEmail(ctx context.Context, to, subject, body string) error {
	if s == nil || s.queue == nil {
		return fmt.Errorf("services.EnqueuingWorkflowEmailSender.SendEmail: no queue wired")
	}
	p := emailAlertJobPayload{To: to, Subject: subject, Body: body}
	job := jobs.Job{Type: EmailAlertJobType, TenantID: emailTenant(ctx)}
	if origin, ok := workflow.EmailOriginFrom(ctx); ok {
		p.WorkflowID, p.ActionIndex = origin.WorkflowID, origin.ActionIndex
	}
	if ref, ok := workflow.RunRefFrom(ctx); ok && ref.TenantID != "" {
		p.WorkflowID, p.RunID, p.ActionIndex = ref.WorkflowID, ref.RunID, ref.ActionIndex
	}
	var entry emaillog.Entry
	logged := false
	if s.log != nil {
		if e, ok := emailLogEntry(ctx, to, emaillog.StatusQueued); ok {
			if err := s.log.Insert(ctx, e); err != nil {
				// Losing a log row is better than losing the customer's mail.
				s.logger.Warn("email log: insert failed (mail still sent)", zap.String("tenant_id", e.TenantID),
					zap.String("workflow_id", e.WorkflowID), zap.Error(err))
			} else {
				entry, logged = e, true
				p.LogID = e.ID
			}
		}
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("services.EnqueuingWorkflowEmailSender.SendEmail: marshal payload: %w", err)
	}
	job.Payload = payload
	jobID, err := s.queue.Enqueue(ctx, job)
	if err != nil {
		if logged {
			masked := workflow.MaskRecipientError("services.EnqueuingWorkflowEmailSender.SendEmail", to, err)
			if lerr := s.log.MarkFailed(ctx, entry.TenantID, entry.ID, 0, masked.Error()); lerr != nil {
				s.logger.Warn("email log: mark failed errored", zap.String("tenant_id", entry.TenantID), zap.Error(lerr))
			}
		}
		return fmt.Errorf("services.EnqueuingWorkflowEmailSender.SendEmail: %w", err)
	}
	if logged {
		if lerr := s.log.AttachJob(ctx, entry.TenantID, entry.ID, jobID); lerr != nil {
			s.logger.Warn("email log: attach job errored", zap.String("tenant_id", entry.TenantID), zap.Error(lerr))
		}
	}
	if p.RunID != "" {
		workflow.MarkQueued(ctx) // #339: the run's slot reads "queued" until the handler settles it
	}
	return nil
}

// Ensure the sender satisfies the workflow engine's interface at compile time.
var _ workflow.EmailSender = (*EnqueuingWorkflowEmailSender)(nil)

// workflowRunAnnotator is the slice of the workflow store a delivery that gave up is written back
// through (specs/059). Satisfied by *workflow.SQLRepository.
type workflowRunAnnotator interface {
	MarkRunActionFailed(ctx context.Context, tenantID, runID string, actionIndex int, key, msg string) (bool, error)
	// MarkRunActionSent turns the run's slot for a delivered mail "sent" (#339).
	MarkRunActionSent(ctx context.Context, tenantID, runID string, actionIndex int) (bool, error)
}

// NewEmailAlertJobHandler returns the queue-side consumer of EmailAlertJobType: unmarshal the
// rendered mail and deliver it through the notify transport (SMTP when ALERTS_ENABLED, noop
// otherwise — the same transport the direct path used). A transport error is returned masked so
// the queue retries with backoff; a malformed payload also errors (it will burn its attempts and
// land in status=failed with the reason in last_error — a producer bug made visible, never
// silently dropped).
//
// specs/059: when the queue will NOT try again — the attempt budget is spent, or the server has no
// mail configured (notify.ErrMailNotConfigured, now Permanent: retrying cannot configure it) — the
// run the mail belongs to is turned failed with the masked reason (runs.MarkRunActionFailed), so the
// builder sees it in the rule's history instead of a "Thành công" that never became true. runs and
// logger may be nil (no write-back / no log).
//
// specs/066: emailLog (nil = off) gets the outcome of the row the payload's logId names — sent, an
// attempt that will be retried, or the final failure; every update is a CAS on status='queued', and a
// log error only warns (the delivery result is unchanged).
func NewEmailAlertJobHandler(transport workflowEmailTransport, runs workflowRunAnnotator, emailLog emailLogWriter, logger *zap.Logger) jobs.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(ctx context.Context, job jobs.Job) error {
		if transport == nil {
			return fmt.Errorf("services.EmailAlertJobHandler: no transport wired")
		}
		var p emailAlertJobPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return fmt.Errorf("services.EmailAlertJobHandler: unmarshal payload: %w", err)
		}
		if p.To == "" {
			return fmt.Errorf("services.EmailAlertJobHandler: empty recipient")
		}
		logOutcome := func(f func(tenantID, id string) error) {
			if emailLog == nil || p.LogID == "" || job.TenantID == "" {
				return
			}
			if lerr := f(job.TenantID, p.LogID); lerr != nil {
				logger.Warn("email log: update failed (delivery outcome unchanged)",
					zap.String("job_id", job.ID), zap.String("tenant_id", job.TenantID), zap.String("log_id", p.LogID), zap.Error(lerr))
			}
		}
		if err := transport.Send(ctx, notify.Mail{To: p.To, Subject: p.Subject, Body: p.Body}); err != nil {
			// Masked: this text becomes sys_jobs.last_error, a worker log line and — when the queue
			// gives up — the run's error (queue #85).
			masked := workflow.MaskRecipientError("services.EmailAlertJobHandler", p.To, err)
			final := job.Attempts >= job.MaxAttempts
			if errors.Is(err, notify.ErrMailNotConfigured) {
				masked, final = jobs.Permanent(masked), true
			}
			if final {
				markEmailRunFailed(ctx, runs, logger, job, p, masked)
				logOutcome(func(t, id string) error { return emailLog.MarkFailed(ctx, t, id, job.Attempts, masked.Error()) })
			} else {
				logOutcome(func(t, id string) error { return emailLog.MarkAttempt(ctx, t, id, job.Attempts, masked.Error()) })
			}
			return masked
		}
		logOutcome(func(t, id string) error { return emailLog.MarkSent(ctx, t, id, job.Attempts) })
		markEmailRunSent(ctx, runs, logger, job, p)
		return nil
	}
}

// markEmailRunSent turns the run's slot for a delivered mail "sent" (#339). Like markEmailRunFailed
// it never changes the delivery outcome: a failure here is logged, not returned (the mail is out).
func markEmailRunSent(ctx context.Context, runs workflowRunAnnotator, logger *zap.Logger, job jobs.Job, p emailAlertJobPayload) {
	if runs == nil || p.RunID == "" || job.TenantID == "" {
		return
	}
	if found, err := runs.MarkRunActionSent(ctx, job.TenantID, p.RunID, p.ActionIndex); err != nil || !found {
		logger.Warn("email_alert: could not mark the workflow run slot sent (mail already delivered)",
			zap.String("job_id", job.ID), zap.String("tenant_id", job.TenantID),
			zap.String("workflow_id", p.WorkflowID), zap.String("run_id", p.RunID),
			zap.Bool("run_row_found", found), zap.Error(err))
	}
}

// markEmailRunFailed writes a delivery the queue gave up on onto its run row (specs/059). Never
// changes the delivery outcome: every failure here is logged, not returned.
func markEmailRunFailed(ctx context.Context, runs workflowRunAnnotator, logger *zap.Logger, job jobs.Job, p emailAlertJobPayload, cause error) {
	if runs == nil || p.RunID == "" || job.TenantID == "" {
		return // no run to mark: a system job, or enqueued outside a recorded run
	}
	msg := fmt.Sprintf("Gửi email không thành — hàng đợi đã dừng thử (lần %d/%d): %s", job.Attempts, job.MaxAttempts, cause.Error())
	if jobs.IsPermanent(cause) {
		msg = fmt.Sprintf("Gửi email không thành — lỗi không thể thử lại (lần %d/%d): %s", job.Attempts, job.MaxAttempts, cause.Error())
	}
	// #338: the dispatcher/scheduler wrote the run row BEFORE the action enqueued this job, so a
	// delivery that gives up on its first attempt finds it — no waiting. A miss now means the row's
	// start was lost (fail-open) or the run was purged: logged, never changing the delivery outcome.
	found, err := runs.MarkRunActionFailed(ctx, job.TenantID, p.RunID, p.ActionIndex, job.ID, msg)
	if err != nil || !found {
		logger.Warn("email_alert: could not mark the workflow run failed (delivery outcome unchanged)",
			zap.String("job_id", job.ID), zap.String("tenant_id", job.TenantID),
			zap.String("workflow_id", p.WorkflowID), zap.String("run_id", p.RunID),
			zap.Bool("run_row_found", found), zap.Error(err))
	}
}

// -----------------------------------------------------------------------------------------------
// DAILY-QUEUE #26: durable outbound_webhook.
//
// The action used to POST from the workflow dispatcher goroutine with its retries sleeping in RAM:
// a receiver down for ten minutes, or a Nexus restart mid-backoff, lost the event with no trace and
// no way to resend. With the queue ON it now parks an "outbound_webhook" sys_jobs row
// (EnqueuingWebhookQueue) and a queue worker delivers it (NewOutboundWebhookJobHandler) — ONE POST
// per claim, the queue owning retry, backoff and the dead-letter (status=failed).
// -----------------------------------------------------------------------------------------------

// OutboundWebhookJobType is the sys_jobs job_type of one durable outbound_webhook delivery.
const OutboundWebhookJobType = "outbound_webhook"

// OutboundWebhookMaxAttempts is the webhook's own attempt budget. The queue default (5 →
// 30s,1m,2m,4m ≈ 7.5 min) gives up before a ten-minute receiver outage ends; 10 attempts walk
// 30s,1m,2m,4m,8m,16m,32m,1h,1h ≈ 3h before the job lands in status=failed.
// ponytail: a constant — make it a WEBHOOK_* knob when a customer needs a longer window.
const OutboundWebhookMaxAttempts = 10

// outboundWebhookJobPayload is the sys_jobs.payload of an OutboundWebhookJobType job. It names
// WHERE the delivery config lives (rule + action slot) and never copies it: sys_jobs is a
// cross-tenant system table, and url/secret/headers are credentials. Body is the exact bytes to
// POST, stored as a JSON STRING — as a nested object MySQL would re-serialise it (key order,
// spacing) and the receiver would get different bytes than the inline path and the dry-run.
//
// CredentialID (specs/050 S2b) is the vault key the slot named when the event fired — an id,
// never the key; absent on a url action and on every job parked before S2b.
type outboundWebhookJobPayload struct {
	WorkflowID   string `json:"workflowId"`
	ActionIndex  int    `json:"actionIndex"`
	CredentialID string `json:"credentialId,omitempty"`
	Body         string `json:"body"`
}

// webhookJobStore is the slice of the jobs store the webhook producer needs: park a delivery, and
// ask whether a rule's deliveries are still pending. Satisfied by *jobs.SQLStore.
type webhookJobStore interface {
	JobEnqueuer
	HasPendingForWorkflow(ctx context.Context, jobType, tenantID, workflowID string) (bool, error)
	FailedBySlot(ctx context.Context, jobType, tenantID, workflowID string) (map[int]int, error)
}

// EnqueuingWebhookQueue implements workflow.WebhookQueue over the jobs store. Unlike email_alert,
// a webhook job is TENANT-PINNED: the tenant travels on the delivery, so the row is attributable
// and the handler re-reads the rule inside that tenant only.
type EnqueuingWebhookQueue struct {
	queue webhookJobStore
}

// NewEnqueuingWebhookQueue wires the producer over the jobs store.
func NewEnqueuingWebhookQueue(queue webhookJobStore) *EnqueuingWebhookQueue {
	return &EnqueuingWebhookQueue{queue: queue}
}

// Ensure the producer satisfies the workflow engine's seam at compile time.
var _ workflow.WebhookQueue = (*EnqueuingWebhookQueue)(nil)

// EnqueueWebhook parks one delivery, runnable now. A delivery without a tenant or a rule is
// refused: the first would land tenant-NULL, the second could never find its config.
func (q *EnqueuingWebhookQueue) EnqueueWebhook(ctx context.Context, d workflow.WebhookDelivery) error {
	if q == nil || q.queue == nil {
		return fmt.Errorf("services.EnqueuingWebhookQueue.EnqueueWebhook: no queue wired")
	}
	if d.TenantID == "" || d.WorkflowID == "" {
		return fmt.Errorf("services.EnqueuingWebhookQueue.EnqueueWebhook: tenant and workflow are required")
	}
	payload, err := json.Marshal(outboundWebhookJobPayload{
		WorkflowID: d.WorkflowID, ActionIndex: d.ActionIndex, CredentialID: d.CredentialID, Body: string(d.Body),
	})
	if err != nil {
		return fmt.Errorf("services.EnqueuingWebhookQueue.EnqueueWebhook: marshal payload: %w", err)
	}
	if _, err := q.queue.Enqueue(ctx, jobs.Job{
		Type:        OutboundWebhookJobType,
		TenantID:    d.TenantID,
		Payload:     payload,
		MaxAttempts: OutboundWebhookMaxAttempts,
	}); err != nil {
		return fmt.Errorf("services.EnqueuingWebhookQueue.EnqueueWebhook: %w", err)
	}
	return nil
}

// HasPendingWebhooks reports whether outbound_webhook jobs of this rule are still queued or
// running in the tenant — the scheduler's back-pressure probe (workflow.WebhookQueue).
func (q *EnqueuingWebhookQueue) HasPendingWebhooks(ctx context.Context, tenantID, workflowID string) (bool, error) {
	if q == nil || q.queue == nil {
		return false, fmt.Errorf("services.EnqueuingWebhookQueue.HasPendingWebhooks: no queue wired")
	}
	pending, err := q.queue.HasPendingForWorkflow(ctx, OutboundWebhookJobType, tenantID, workflowID)
	if err != nil {
		return false, fmt.Errorf("services.EnqueuingWebhookQueue.HasPendingWebhooks: %w", err)
	}
	return pending, nil
}

// FailedWebhookSlots counts, per action slot, the failed outbound_webhook jobs of this rule that name
// no vault key and could still be resent ("Gửi lại") — Doctor C10's warning (specs/050 S3b).
func (q *EnqueuingWebhookQueue) FailedWebhookSlots(ctx context.Context, tenantID, workflowID string) (map[int]int, error) {
	if q == nil || q.queue == nil {
		return nil, fmt.Errorf("services.EnqueuingWebhookQueue.FailedWebhookSlots: no queue wired")
	}
	slots, err := q.queue.FailedBySlot(ctx, OutboundWebhookJobType, tenantID, workflowID)
	if err != nil {
		return nil, fmt.Errorf("services.EnqueuingWebhookQueue.FailedWebhookSlots: %w", err)
	}
	return slots, nil
}

// webhookDeliverer makes ONE signed POST and says whether a retry could help. Satisfied by
// *workflow.HTTPWebhookSender (Deliver — never Post, whose own retries would stack on the queue's).
type webhookDeliverer interface {
	Deliver(ctx context.Context, req workflow.WebhookRequest) (retryable bool, err error)
}

// NewOutboundWebhookJobHandler returns the queue-side consumer of OutboundWebhookJobType. Per claim:
// re-read the rule (tenant-pinned through the row's tenant_id), take url/secret/headers from the
// action at the parked slot, and POST the frozen body once with the job id as X-Nexus-Delivery-Id
// (#27) — the same id on every attempt of this job, so the receiver can drop a repeat.
//
//	receiver 2xx                         → nil (done)
//	network error / 5xx / 429            → error (the queue retries with backoff)
//	other 4xx / a 3xx with a vault key   → jobs.Permanent (failed now — a bad request stays bad)
//	rule gone / slot moved / url blanked → jobs.Permanent (nothing left to deliver to)
//	slot names another vault key         → jobs.Permanent ("rule đã đổi" — specs/050 S2b)
//	vault key unusable (CREDENTIAL_*)    → jobs.Permanent, NOTHING sent (fixed by the admin, then "Gửi lại")
//	rule / vault store error             → error (transient — retried)
//
// Config is re-read on every attempt, so an admin who fixes a wrong URL or rotates a secret while
// a delivery is backing off gets the fix on the next attempt. The rule's is_active flag is NOT
// consulted: the event happened while the rule was live, and pausing a rule stops NEW events, not
// the delivery of ones already owed. creds resolves a vault action's key under the job's tenant
// (nil ⇒ such an action fails CREDENTIALS_NOT_CONFIGURED; url actions do not need it).
func NewOutboundWebhookJobHandler(rules timerRuleReader, creds workflow.CredentialResolver, deliverer webhookDeliverer) jobs.HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		if rules == nil || deliverer == nil {
			return fmt.Errorf("services.OutboundWebhookJobHandler: not fully wired")
		}
		var p outboundWebhookJobPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return jobs.Permanent(fmt.Errorf("services.OutboundWebhookJobHandler: unmarshal payload: %w", err))
		}
		// job.ID is the delivery id (#27) — without it the receiver could not deduplicate.
		if job.ID == "" || job.TenantID == "" || p.WorkflowID == "" {
			return jobs.Permanent(fmt.Errorf("services.OutboundWebhookJobHandler: job has no id, tenant or workflow"))
		}
		rec, err := rules.Get(ctx, job.TenantID, p.WorkflowID)
		if err != nil {
			if errors.Is(err, workflow.ErrWorkflowNotFound) {
				return jobs.Permanent(fmt.Errorf("services.OutboundWebhookJobHandler: rule %s no longer exists", p.WorkflowID))
			}
			return fmt.Errorf("services.OutboundWebhookJobHandler: load rule: %w", err)
		}
		wf, err := rec.ToWorkflow()
		if err != nil {
			return jobs.Permanent(fmt.Errorf("services.OutboundWebhookJobHandler: decode rule: %w", err))
		}
		if p.ActionIndex < 0 || p.ActionIndex >= len(wf.Actions) || wf.Actions[p.ActionIndex].Type != workflow.ActionWebhook {
			return jobs.Permanent(fmt.Errorf("services.OutboundWebhookJobHandler: rule %s has no outbound_webhook at action %d any more", p.WorkflowID, p.ActionIndex))
		}
		a := wf.Actions[p.ActionIndex]
		// The body was built for the key the slot named when the event fired: delivering it with
		// another partner's key (or to a url that replaced it) would hand one partner's data to
		// another. ponytail: url actions are still matched by slot only — #228 (stable action key).
		if strings.TrimSpace(a.CredentialID) != p.CredentialID {
			return jobs.Permanent(fmt.Errorf("services.OutboundWebhookJobHandler: rule %s action %d đã đổi khoá kết nối từ lúc xếp hàng (rule đã đổi) — không gửi", p.WorkflowID, p.ActionIndex))
		}
		if p.CredentialID == "" && strings.TrimSpace(a.URL) == "" {
			return jobs.Permanent(fmt.Errorf("services.OutboundWebhookJobHandler: rule %s action %d has no url", p.WorkflowID, p.ActionIndex))
		}
		// The SAME request the inline path builds (runWebhook): the vault key is resolved now,
		// signing headers are computed inside the one attempt Deliver makes, errors name the
		// display URL only (specs/050 S2a/S2b).
		req, err := workflow.WebhookRequestFor(ctx, creds, job.TenantID, a, []byte(p.Body))
		if err != nil {
			err = fmt.Errorf("services.OutboundWebhookJobHandler: %w", err)
			var coded *workflow.CodedError
			if errors.As(err, &coded) {
				return jobs.Permanent(err) // retrying cannot fix the key — and nothing was sent
			}
			return err
		}
		req.DeliveryID = job.ID
		retryable, err := deliverer.Deliver(ctx, req)
		if err == nil {
			return nil
		}
		err = fmt.Errorf("services.OutboundWebhookJobHandler: %w", err)
		if !retryable {
			return jobs.Permanent(err)
		}
		return err
	}
}
