package workflow

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	neturl "net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// EmailSender is the engine's consumer-side view of the email transport needed by the email_alert
// action (M2). It is satisfied by an adapter over internal/notify's SMTPSender (declared in the
// services layer so the workflow package stays free of an SMTP dependency) and by a fake in unit
// tests. Send must be safe for concurrent use by multiple dispatcher workers.
type EmailSender interface {
	// SendEmail delivers one plain-text message. It returns an error on delivery failure; the
	// caller logs it fail-open (a down SMTP server never blocks the originating flow).
	SendEmail(ctx context.Context, to, subject, body string) error
}

// EmailSkipRecorder is an OPTIONAL capability of an EmailSender (specs/066 L3): a mail that fails
// BEFORE it reaches the sender — the template does not render, or the recipient renders empty —
// is still a line in the tenant's email log. reason is the same text the run history gets: it
// names the TEMPLATE, never record data. The EmailSender signature is unchanged; runEmailAlert
// finds the hook by type assertion, so a sender without it behaves exactly as before.
type EmailSkipRecorder interface {
	RecordEmailSkipped(ctx context.Context, reason string)
}

// recordEmailSkip tells the sender (when it listens) that a mail never got as far as SendEmail, and
// hands err back unchanged so the run history keeps the very same text.
func recordEmailSkip(ctx context.Context, email EmailSender, err error) error {
	if r, ok := email.(EmailSkipRecorder); ok {
		r.RecordEmailSkipped(ctx, err.Error())
	}
	return err
}

// WebhookSender is the engine's consumer-side view of the outbound HTTP transport needed by the
// outbound_webhook action (M2). The default implementation is *HTTPWebhookSender; a fake in unit
// tests records the call. Post must be safe for concurrent use.
type WebhookSender interface {
	// Post delivers one request, signing it when req.SigningSecret is set. It performs its own
	// retry/backoff and returns an error only after the final attempt fails (the caller logs it
	// fail-open — a down endpoint never blocks the originating flow).
	Post(ctx context.Context, req WebhookRequest) error
}

// WebhookRequest is one outbound_webhook delivery as the sender sees it (specs/050 S2a). It holds
// everything an attempt needs to compute EVERY signing header itself, so the inline retry loop
// (Post) and the durable job handler (Deliver, one attempt per claim) sign identically.
type WebhookRequest struct {
	// URL is the full target. It may carry a credential (user:pass@, ?token=, a token in the
	// path), so it is never written into an error or a log — DisplayURL is.
	URL string
	// DisplayURL is the only form of the target allowed into error text. Empty ⇒ the sender
	// derives the bare origin from URL (webhookDisplayURL), so a caller cannot leak by omission.
	DisplayURL string
	Body       []byte            // exactly the bytes POSTed and signed
	Headers    map[string]string // the action's own headers (legacy inline) — sent as given
	// SigningSecret "" ⇒ unsigned. Set ⇒ X-Nexus-Signature (V1) + X-Nexus-Timestamp +
	// X-Nexus-Signature-V2, computed inside each attempt.
	SigningSecret string
	// PrevSigningSecret adds X-Nexus-Signature-V2-Previous during a key-rotation grace (set from
	// specs/050 S4, by the credential resolver).
	PrevSigningSecret string
	// NoRedirect stops at a 3xx instead of following it (WEBHOOK_REDIRECT_NOT_FOLLOWED, not
	// retried). Set for every request carrying a vault key (specs/050 D-7): Go drops Authorization
	// and Cookie on a cross-host redirect but keeps custom headers, so a compromised partner could
	// otherwise bounce an X-Api-Key to a host of its choosing.
	NoRedirect bool
	// DeliveryID goes out as X-Nexus-Delivery-Id. Deliver: the job id. Post: minted once when
	// empty and repeated on its in-process retries.
	DeliveryID string
}

// CredentialResolver is the engine's consumer-side view of the named-credential vault (specs/050
// S2b): the auth a delivery must carry, read AT SEND TIME under the rule's tenant — never cached,
// so a rotated key is on the very next send (FR-014). Implemented by
// services.CredentialService.ResolveAuth; declared here so the engine does not import services.
// A key that cannot be used is a *CodedError (CREDENTIAL_NOT_FOUND / CREDENTIAL_NOT_READY /
// CREDENTIAL_KEY_UNAVAILABLE / CREDENTIALS_NOT_CONFIGURED); any other error is transient.
type CredentialResolver interface {
	ResolveAuth(ctx context.Context, tenantID, credentialID string) (WebhookAuth, error)
}

// WebhookAuth is one resolved key: where to send and what to send with it.
type WebhookAuth struct {
	BaseURL     string            // the key's base URL (the real one — S5 substitutes a url token into it)
	DisplayBase string            // the base as errors and the dry-run may show it
	Headers     map[string]string // the auth header + the key's extra headers, unsealed
	// SigningSecret / PrevSigningSecret: an hmac key (and its predecessor in a rotation grace).
	SigningSecret     string
	PrevSigningSecret string
}

// Runtime codes of an outbound_webhook that could not be sent (specs/050 plan "Mã lỗi runtime").
// They land in sys_workflow_runs.error / actions_result and sys_jobs.last_error; none is fixed by
// retrying, so the durable handler fails the job at once (and #32 "Gửi lại" sends it after the
// admin fixes the key).
const (
	CodeCredentialNotFound       = "CREDENTIAL_NOT_FOUND"
	CodeCredentialNotReady       = "CREDENTIAL_NOT_READY"
	CodeCredentialKeyUnavailable = "CREDENTIAL_KEY_UNAVAILABLE"
	CodeCredentialsNotConfigured = "CREDENTIALS_NOT_CONFIGURED"
	CodeRedirectNotFollowed      = "WEBHOOK_REDIRECT_NOT_FOLLOWED"
	CodeWebhookPathInvalid       = "WEBHOOK_PATH_INVALID"
)

// CodedError is a delivery failure named by a stable runtime code. Error() is the code and a
// message written for the run history — never the cause, which may name a master-key id or quote
// an envelope; the cause stays reachable through Unwrap (errors.Is) and in the resolver's own log.
type CodedError struct {
	Code string
	Msg  string
	Err  error
}

func (e *CodedError) Error() string {
	if e.Msg == "" {
		return e.Code
	}
	return e.Code + ": " + e.Msg
}

func (e *CodedError) Unwrap() error { return e.Err }

// maxCredentialPathLen bounds a vault action's path (plan: WEBHOOK_PATH_INVALID ≤ 500).
const maxCredentialPathLen = 500

// ValidateCredentialPath checks the path a vault action appends to its key's base URL — the ONE
// rule for config time (services.buildActionsJSON) and send time (JoinCredentialURL). "" is the
// base itself. Otherwise it starts with a single "/", holds no "://", "@", "\", "#", whitespace or
// control character, and its path part (before "?") has no "%" outside a valid %XX escape, no dot
// segment and no encoded dot / slash / backslash — it can neither leave the key's host nor climb
// out of the base's path prefix, and it parses back at send time exactly as it did here.
func ValidateCredentialPath(p string) error {
	if p == "" {
		return nil
	}
	bad := func(why string) error {
		return &CodedError{Code: CodeWebhookPathInvalid, Msg: "path " + why}
	}
	if len(p) > maxCredentialPathLen {
		return bad(fmt.Sprintf("dài quá %d ký tự", maxCredentialPathLen))
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return bad("phải bắt đầu bằng đúng một dấu /")
	}
	if strings.Contains(p, "://") || strings.ContainsAny(p, `@\#`) {
		return bad(`không được chứa ://, @, \ hay #`)
	}
	if strings.IndexFunc(p, func(r rune) bool { return r <= ' ' || r == 0x7f || unicode.IsSpace(r) }) >= 0 {
		return bad("không được chứa khoảng trắng hay ký tự điều khiển")
	}
	// The send-time join parses base+path back; a broken escape in the path part ("/%zz", "/100%")
	// fails there, so it must fail here too — otherwise it saves and every delivery is refused.
	// (url.Parse leaves the query unchecked, on both sides alike.)
	if _, err := neturl.Parse("https://h" + p); err != nil {
		return bad("có dấu % không thuộc một mã %XX hợp lệ")
	}
	pathPart, _, _ := strings.Cut(strings.ToLower(p), "?")
	for _, enc := range []string{"%2e", "%2f", "%5c"} {
		if strings.Contains(pathPart, enc) {
			return bad("không được chứa dấu chấm hay gạch chéo đã mã hoá")
		}
	}
	for _, seg := range strings.Split(pathPart, "/") {
		if seg == "." || seg == ".." {
			return bad("không được chứa đoạn . hay ..")
		}
	}
	return nil
}

// JoinCredentialURL is a vault action's target: the key's base URL (trailing "/" dropped) + path,
// parsed back and required to keep the base's scheme, host and port with no userinfo or fragment
// (plan D-7). Errors never repeat the base — a url_token base (S5) carries the key.
func JoinCredentialURL(base, path string) (string, error) {
	if err := ValidateCredentialPath(path); err != nil {
		return "", err
	}
	return joinWithinOrigin(base, path)
}

// joinWithinOrigin is the second wall behind the path rule: concatenate, parse back, refuse any
// result that left the base's origin or grew userinfo / a fragment. Separate so a test can reach
// it with the path rule out of the way — for every path the rule accepts it cannot fire.
func joinWithinOrigin(base, path string) (string, error) {
	joined := strings.TrimRight(base, "/") + path
	b, berr := neturl.Parse(base)
	u, uerr := neturl.Parse(joined)
	if berr != nil || uerr != nil || u.User != nil || u.Fragment != "" || !sameOrigin(b, u) {
		return "", &CodedError{Code: CodeWebhookPathInvalid, Msg: "địa chỉ ghép không hợp lệ hoặc rời khỏi máy chủ của khoá kết nối"}
	}
	return joined, nil
}

// CredentialDisplayURL is how a vault action's target may be shown (errors, dry-run): the key's
// display base + the path without its query — the key lives in the vault, not in the path.
func CredentialDisplayURL(displayBase, path string) string {
	p, _, _ := strings.Cut(path, "?")
	return strings.TrimRight(displayBase, "/") + p
}

// sameOrigin compares scheme, host and port (default port made explicit), case-insensitively.
func sameOrigin(a, b *neturl.URL) bool {
	port := func(u *neturl.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "http") {
			return "80"
		}
		return "443"
	}
	return a.Hostname() != "" && strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// WebhookRequestFor builds the request of one outbound_webhook action — the ONE body the inline
// path (runWebhook) and the durable job handler share, so both send the same thing. A url action
// sends its own url/secret/headers; a vault action resolves its key NOW under tenantID and joins
// its path onto the key's base. A key that cannot be used returns its *CodedError and nothing is
// sent (FR-016 — never an unauthenticated call). The caller sets DeliveryID when it has one.
func WebhookRequestFor(ctx context.Context, creds CredentialResolver, tenantID string, a Action, body []byte) (WebhookRequest, error) {
	credentialID := strings.TrimSpace(a.CredentialID)
	if credentialID == "" {
		if strings.TrimSpace(a.URL) == "" {
			return WebhookRequest{}, fmt.Errorf("empty url")
		}
		return WebhookRequestFromURLAction(a, body), nil
	}
	if creds == nil {
		return WebhookRequest{}, &CodedError{Code: CodeCredentialsNotConfigured, Msg: "chưa nối kho khoá kết nối vào engine"}
	}
	auth, err := creds.ResolveAuth(ctx, tenantID, credentialID)
	if err != nil {
		return WebhookRequest{}, err
	}
	target, err := JoinCredentialURL(auth.BaseURL, a.Path)
	if err != nil {
		return WebhookRequest{}, err
	}
	return WebhookRequest{
		URL: target, DisplayURL: CredentialDisplayURL(auth.DisplayBase, a.Path), Body: body,
		Headers: auth.Headers, SigningSecret: auth.SigningSecret, PrevSigningSecret: auth.PrevSigningSecret,
		NoRedirect: true,
	}, nil
}

// WebhookRequestFromURLAction builds the request of an action that names its own target (url +
// the secret/headers stored on the action) — shared by the inline path (runWebhook) and the
// durable job handler, so both send the same thing. The caller checks for a blank URL first and
// sets DeliveryID when it has one.
func WebhookRequestFromURLAction(a Action, body []byte) WebhookRequest {
	target := strings.TrimSpace(a.URL)
	return WebhookRequest{
		URL: target, DisplayURL: webhookDisplayURL(target), Body: body,
		Headers: a.Headers, SigningSecret: a.Secret,
	}
}

// webhookDisplayURL is a url action's target reduced to scheme://host[:port]. Userinfo and query
// are credentials by convention, and so is the PATH of the most common Vietnamese integrations
// (Telegram /bot<TOKEN>/, Slack/Google Chat incoming webhooks) — nothing after the host can be
// told apart from a secret, so none of it reaches an error (specs/050 FR-017; the stricter #26
// rule TestHTTPWebhookSender_TransportErrorOmitsURL already pinned for the path).
func webhookDisplayURL(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "(url không hợp lệ)"
	}
	return u.Scheme + "://" + u.Host
}

// DisplayRedactedMarker replaces every secret-bearing value in RedactActionsForDisplay.
const DisplayRedactedMarker = "[đã che]"

// RedactActionsForDisplay is a rule's actions as the AI model may read them (specs/067 FR-081).
// Stricter than RedactActionsJSON (which keeps the query names for the builder): url is cut to
// scheme://host[:port], secret / every header value / path (a vault connection key) become the
// marker, header NAMES and credentialId (an id, never the key) stay. The input is not modified;
// a value that does not decode as a list of objects becomes [] rather than being echoed.
func RedactActionsForDisplay(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return raw
	}
	var acts []map[string]any
	if err := json.Unmarshal(raw, &acts); err != nil {
		return json.RawMessage("[]")
	}
	for _, a := range acts {
		for k, v := range a {
			switch {
			case strings.EqualFold(k, "secret"), strings.EqualFold(k, "path"):
				a[k] = DisplayRedactedMarker
			case strings.EqualFold(k, "headers"):
				if m, ok := v.(map[string]any); ok {
					for name := range m {
						m[name] = DisplayRedactedMarker
					}
				} else {
					a[k] = DisplayRedactedMarker
				}
			case strings.EqualFold(k, "url"):
				if s, ok := v.(string); ok && s != "" {
					a[k] = webhookDisplayURL(s)
				}
			}
		}
	}
	out, err := json.Marshal(acts)
	if err != nil {
		return json.RawMessage("[]")
	}
	return out
}

// WebhookDelivery is one outbound_webhook delivery handed to the durable queue (DAILY-QUEUE #26).
// It deliberately has NO URL, Secret or Headers: the queue row lives in a cross-tenant system
// table, so it carries only WHERE to re-read them at delivery time (the rule + the action's slot
// in it) plus the body frozen at fire time. A signing key cannot reach sys_jobs because this type
// has nowhere to put one.
type WebhookDelivery struct {
	TenantID    string // owning tenant — pins the job row (never NULL for a webhook)
	WorkflowID  string // the rule that fired
	ActionIndex int    // position of the outbound_webhook action in the rule's actions
	// CredentialID is the vault key the slot named when the event fired (specs/050 S2b) — an id,
	// never the key. The handler refuses to deliver when the slot names another key by then.
	CredentialID string
	Body         []byte // exactly the bytes to POST (same as the inline path and the dry-run preview)
}

// WebhookQueue parks a delivery on the durable job queue instead of posting from the dispatcher
// goroutine, so a receiver that is down for a while — or a Nexus restart mid-backoff — no longer
// loses the event. Implemented by services.EnqueuingWebhookQueue over sys_jobs; nil (queue
// disabled, JOBS_ENABLED=false) keeps the inline WebhookSender path.
type WebhookQueue interface {
	EnqueueWebhook(ctx context.Context, d WebhookDelivery) error
	// HasPendingWebhooks reports whether deliveries parked by this rule are still queued or
	// running — the scheduler holds a rule while they are (Scheduler.webhooksPending).
	HasPendingWebhooks(ctx context.Context, tenantID, workflowID string) (bool, error)
}

// templateVarRe matches a {{field}} placeholder. The captured group is the field name (trimmed of
// surrounding spaces by renderTemplate). Compiled ONCE at package init — never inside a per-record
// loop (B5 hot-loop discipline: the scheduler renders one template per matching record, so a
// per-call Compile would re-parse the same pattern for every row).
var templateVarRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.-]+)\s*\}\}`)

// renderTemplate substitutes every {{field}} placeholder in tmpl with the record's value for that
// field (rendered via the same canonical toString used by the condition evaluator, so 5 and 5.0
// render identically). An unknown field renders as the empty string (a template referencing a
// dropped field degrades gracefully rather than leaking the literal "{{field}}"). renderTemplate
// is pure and allocation-light: it returns tmpl unchanged when it carries no placeholder.
func renderTemplate(tmpl string, data map[string]any) string {
	if !strings.Contains(tmpl, "{{") {
		return tmpl
	}
	return templateVarRe.ReplaceAllStringFunc(tmpl, func(match string) string {
		// Extract the field name from the matched "{{ name }}" via the same regex.
		sub := templateVarRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return ""
		}
		field := strings.TrimSpace(sub[1])
		if v, ok := data[field]; ok {
			return toString(v)
		}
		return ""
	})
}

// TemplateResolver answers the placeholders of an email template that the triggering record's own
// data cannot (specs/059): {{<relation>.<field>}} — a field of the record a to-one relation points
// at — and a lookup field, which is computed at READ time and so is never in evt.Data. Without it
// "Gửi tới {{benh_nhan.email}}" on an appointment always rendered "" and every such rule failed.
//
// names are the placeholders absent from data (deduplicated); the answer maps each name it could
// resolve to its value — an unresolvable name (empty FK, deleted target, unknown field) is simply
// left out and renders "" like any unknown field. An error means the READ failed: the caller must
// fail the action rather than send with holes in it. Implemented by services.WorkflowTemplateResolver
// (tenant-pinned system read); nil keeps the pre-059 behaviour.
type TemplateResolver interface {
	ResolveTemplateValues(ctx context.Context, tenantID, entity string, data map[string]any, names []string) (map[string]any, error)
}

// TemplatePlaceholders returns the distinct placeholder names of the templates, in first-seen order.
func TemplatePlaceholders(tmpls ...string) []string {
	var names []string
	for _, tmpl := range tmpls {
		if !strings.Contains(tmpl, "{{") {
			continue
		}
		for _, m := range templateVarRe.FindAllStringSubmatch(tmpl, -1) {
			if name := strings.TrimSpace(m[1]); !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// RenderedEmail is one email_alert rendered for one event.
type RenderedEmail struct {
	To, Subject, Body string
}

// RenderEmail renders an email_alert's To (trimmed), Subject and Body for evt — the ONE render path
// of the live action (runEmailAlert) and the dry-run preview (POST /admin/workflows/:id/test), so the
// preview can never show an address the real send would not use. Placeholders the record data
// answers render straight from it; the rest go to the resolver in ONE call. evt.Data is shared
// read-only across every action of every rule, so resolved values land in a copy, never in it.
func RenderEmail(ctx context.Context, evt Event, a Action, resolver TemplateResolver) (RenderedEmail, error) {
	data, err := RenderData(ctx, evt, resolver, a.To, a.Subject, a.Body)
	if err != nil {
		return RenderedEmail{}, fmt.Errorf("workflow.RenderEmail: %w", err)
	}
	return RenderedEmail{
		To:      strings.TrimSpace(renderTemplate(a.To, data)),
		Subject: renderTemplate(a.Subject, data),
		Body:    renderTemplate(a.Body, data),
	}, nil
}

// RenderData is the data a set of templates render from: evt.Data plus, in ONE resolver call, the
// placeholders the record's own data cannot answer ({{relation.field}}, {{user.email}}, a lookup).
// The shared seam of every action that renders placeholders — email_alert, in_app_notification,
// create_record (DAILY-QUEUE #335) — so they all mean the same thing by "{{benh_nhan.ten}}".
// evt.Data is shared read-only across every action of every rule, so resolved values land in a
// copy, never in it. A nil resolver (or nothing missing) returns evt.Data itself.
func RenderData(ctx context.Context, evt Event, resolver TemplateResolver, tmpls ...string) (map[string]any, error) {
	if resolver == nil {
		return evt.Data, nil
	}
	var missing []string
	for _, name := range TemplatePlaceholders(tmpls...) {
		// A null value renders "" exactly like an absent key, so it is not an answer either:
		// a lookup key that is present but null in the row's JSON must not mask the value the
		// record page shows.
		if v, ok := evt.Data[name]; !ok || v == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return evt.Data, nil
	}
	extra, err := resolver.ResolveTemplateValues(ctx, evt.TenantID, evt.EntityName, evt.Data, missing)
	if err != nil {
		return nil, fmt.Errorf("không đọc được bản ghi liên kết: %w", err)
	}
	if len(extra) == 0 {
		return evt.Data, nil
	}
	data := make(map[string]any, len(evt.Data)+len(extra))
	maps.Copy(data, evt.Data)
	maps.Copy(data, extra)
	return data, nil
}

// RenderCreateValues is a create_record's value map for evt (the "$owner"/"$record" sentinels and
// the {{…}} placeholders resolved) — the one path of the live action and the dry-run preview (#335).
// Only the free-text values are asked of the resolver; sentinels never are.
func RenderCreateValues(ctx context.Context, evt Event, a Action, resolver TemplateResolver) (map[string]any, error) {
	var tmpls []string
	for _, v := range a.Values {
		if s, ok := v.(string); ok && strings.Contains(s, "{{") {
			tmpls = append(tmpls, s)
		}
	}
	data, err := RenderData(ctx, evt, resolver, tmpls...)
	if err != nil {
		return nil, fmt.Errorf("workflow.RenderCreateValues: %w", err)
	}
	evt.Data = data
	return resolveCreateValues(a.Values, strings.TrimSpace(evt.OwnerUserID), evt), nil
}

// runEmailAlert sends the action's email through the EmailSender. To/Subject/Body are rendered by
// RenderEmail (the record's fields, plus — through templates — the linked record's). A missing
// sender makes the action error (logged fail-open). A blank recipient (after render) is an error
// naming the recipient TEMPLATE, so a rule whose patient has no email reads as a failed run that
// says why, instead of silently sending to "".
func runEmailAlert(ctx context.Context, evt Event, a Action, email EmailSender, templates TemplateResolver) error {
	if email == nil {
		return fmt.Errorf("workflow.runEmailAlert: no email sender wired")
	}
	mail, err := RenderEmail(ctx, evt, a, templates)
	if err != nil {
		return recordEmailSkip(ctx, email, fmt.Errorf("workflow.runEmailAlert: %w", err))
	}
	to, subject, body := mail.To, mail.Subject, mail.Body
	if to == "" {
		// Names the template (config), never record data — this text lands in sys_workflow_runs
		// and, through EmailSkipRecorder, in the email log.
		return recordEmailSkip(ctx, email, fmt.Errorf("workflow.runEmailAlert: không có địa chỉ nhận — mẫu %q ra rỗng trên bản ghi này (trường trống, hoặc bản ghi chưa liên kết)", strings.TrimSpace(a.To)))
	}
	if err := email.SendEmail(ctx, to, subject, body); err != nil {
		// A server with no mail configured discards the message; that is a legitimate state, but
		// it must READ as "not sent" in the run history rather than as a success. Saying which of
		// the two happened is the whole point — "sent" and "silently dropped" look identical from
		// the outside otherwise.
		//
		// The recipient is named by its TEMPLATE (a.To — the rule's config), never the rendered
		// address (record data): this text is copied verbatim into sys_workflow_runs.error and
		// actions_result (queue #85).
		return MaskRecipientError(fmt.Sprintf("workflow.runEmailAlert: gửi tới %q thất bại", strings.TrimSpace(a.To)), to, err)
	}
	return nil
}

// recipientMaskedError is a send failure whose TEXT has the rendered recipient masked while
// Unwrap still returns the original cause, so errors.Is/As keep working (fmt.Errorf's %w can
// only append the cause's text verbatim, which is exactly what must not happen here).
type recipientMaskedError struct {
	msg   string
	cause error
}

func (e *recipientMaskedError) Error() string { return e.msg }
func (e *recipientMaskedError) Unwrap() error { return e.cause }

// minMaskLen: a recipient part shorter than this is not an address, and replacing it everywhere
// would shred the error text ("a" matches half the message) without protecting anything.
const minMaskLen = 3

// MaskRecipientError wraps a mail-send failure as "<prefix>: <cause>" with every occurrence of the
// rendered recipient — whole, and each comma/semicolon-separated address in it — replaced by
// "[redacted]" in the cause text. An SMTP server rejecting RCPT TO echoes the address it was
// given ("550 5.1.1 <x@y>: Recipient address rejected"), so masking the prefix alone would still
// leak through the wrapped cause.
//
// Why this exists (queue #85, workflow-run-error-pii): an email_alert whose To is
// {{email_ca_nhan}} renders a personal address — possibly a field the admin flagged sensitive —
// and the dispatcher copies the action error into sys_workflow_runs.error, actions_result[].error
// and the worker log. Those are logs, not the record; they have no field-level ACL and a 90-day
// retention, so a value that lands there escapes every control the record has. Shared by the
// inline action and the durable email_alert job handler (sys_jobs.last_error), one rule for both.
func MaskRecipientError(prefix, recipient string, cause error) error {
	text := cause.Error()
	parts := append([]string{recipient}, strings.FieldsFunc(recipient, func(r rune) bool { return r == ',' || r == ';' })...)
	for _, p := range parts {
		if p = strings.TrimSpace(p); len(p) >= minMaskLen {
			// Case-insensitive: servers normalise case when they echo ("Lan@Gmail.com" comes back
			// as "lan@gmail.com"). ponytail: compiles per FAILED send only — never on the send path.
			text = regexp.MustCompile("(?i)"+regexp.QuoteMeta(p)).ReplaceAllLiteralString(text, "[redacted]")
		}
	}
	return &recipientMaskedError{msg: prefix + ": " + text, cause: cause}
}

// RenderTemplate is the exported form of renderTemplate, consumed by the W6 dry-run endpoint
// (POST /admin/workflows/:id/test) so the "would send" email/notification preview is rendered by
// the EXACT code path the live email_alert action uses — the preview can never drift from the
// real send.
func RenderTemplate(tmpl string, data map[string]any) string {
	return renderTemplate(tmpl, data)
}

// WebhookPayloadPreview marshals the SAME payload runWebhook would POST for (workflowID, evt) —
// consumed by the W6 dry-run endpoint so the webhook preview is byte-identical to a live send.
func WebhookPayloadPreview(workflowID string, evt Event) (json.RawMessage, error) {
	b, err := marshalWebhookBody(workflowID, evt)
	if err != nil {
		return nil, fmt.Errorf("workflow.WebhookPayloadPreview: %w", err)
	}
	return b, nil
}

// webhookPayload is the JSON body POSTed by the outbound_webhook action. It carries the triggering
// context (tenant/entity/record/trigger/workflow) plus the record's committed field map so the
// receiving system has everything it needs. It deliberately mirrors the audit/notification link
// shape. The map is the same READ-ONLY Data the dispatcher passes around (never mutated here).
type webhookPayload struct {
	WorkflowID string         `json:"workflowId"`
	TenantID   string         `json:"tenantId"`
	EntityName string         `json:"entityName"`
	RecordID   string         `json:"recordId"`
	Trigger    string         `json:"trigger"`
	OccurredAt string         `json:"occurredAt"`
	Data       map[string]any `json:"data"`
}

// OccurredAtLayout renders a webhook body's occurredAt: UTC, always six fractional digits. Fixed
// width on purpose — time.RFC3339Nano trims trailing zeros, so "…:03.1Z" would sort AFTER
// "…:03.12Z" for a receiver comparing the strings.
const OccurredAtLayout = "2006-01-02T15:04:05.000000Z07:00"

// marshalWebhookBody is the ONE place the webhook body is built — the live action (inline and
// durable) and the dry-run preview share it. occurredAt is the event's instant (Emit, the
// scheduler and the timer handler stamp it); an event nobody stamped (dry-run) takes the marshal
// time — never the zero time, which a receiver dropping "older than what I have" would drop
// forever.
func marshalWebhookBody(workflowID string, evt Event) ([]byte, error) {
	at := evt.OccurredAt
	if at.IsZero() {
		at = time.Now()
	}
	return json.Marshal(webhookPayload{
		WorkflowID: workflowID,
		TenantID:   evt.TenantID,
		EntityName: evt.EntityName,
		RecordID:   evt.RecordID,
		Trigger:    string(evt.Trigger),
		OccurredAt: at.UTC().Format(OccurredAtLayout),
		Data:       evt.Data,
	})
}

// runWebhook marshals the event into the webhook payload and hands it on. With a WebhookQueue
// wired (the default) the delivery is PARKED as a durable job and the run history records the
// enqueue — the delivery outcome lives on the sys_jobs row, exactly like email_alert. Without one
// it POSTs inline through the WebhookSender, which owns retry + HMAC. actionIndex is the action's
// slot in wf.Actions: the job handler re-reads url/secret/headers (or the vault key) from there at
// delivery time. A missing transport / blank URL is an error (logged fail-open).
func runWebhook(ctx context.Context, wf Workflow, evt Event, a Action, actionIndex int, deps actionDeps) error {
	if deps.webhookQueue == nil && deps.webhook == nil {
		return fmt.Errorf("workflow.runWebhook: no webhook sender wired")
	}
	credentialID := strings.TrimSpace(a.CredentialID)
	if credentialID != "" {
		// The key is read under the RULE's tenant; an event of another tenant reaching this rule is
		// a bug upstream — refuse before anything is resolved or parked.
		if wf.TenantID == "" || wf.TenantID != evt.TenantID {
			return fmt.Errorf("workflow.runWebhook: rule and event belong to different tenants — not sent")
		}
	} else if strings.TrimSpace(a.URL) == "" {
		return fmt.Errorf("workflow.runWebhook: empty url")
	}
	body, err := marshalWebhookBody(wf.ID, evt)
	if err != nil {
		return fmt.Errorf("workflow.runWebhook: marshal payload: %w", err)
	}
	if deps.webhookQueue != nil {
		if err := deps.webhookQueue.EnqueueWebhook(ctx, WebhookDelivery{
			TenantID: evt.TenantID, WorkflowID: wf.ID, ActionIndex: actionIndex, CredentialID: credentialID, Body: body,
		}); err != nil {
			return fmt.Errorf("workflow.runWebhook: enqueue: %w", err)
		}
		return nil
	}
	req, err := WebhookRequestFor(ctx, deps.creds, wf.TenantID, a, body)
	if err != nil {
		return fmt.Errorf("workflow.runWebhook: %w", err)
	}
	if err := deps.webhook.Post(ctx, req); err != nil {
		return fmt.Errorf("workflow.runWebhook: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// HTTPWebhookSender — the production WebhookSender (retry + backoff + HMAC).
// ---------------------------------------------------------------------------------------------

// HTTPWebhookSender POSTs a JSON payload over HTTP with bounded retry + exponential backoff and
// optional HMAC-SHA256 signing (V1 + timestamp + V2). It is the production WebhookSender: a
// transient failure (network error, 5xx, 429) is retried; a permanent 4xx is NOT retried (a bad
// request will never succeed on retry). It is concurrency-safe (the underlying http.Client is).
type HTTPWebhookSender struct {
	client     *http.Client
	maxRetries int           // total ATTEMPTS = maxRetries (>=1); e.g. 3 → up to 3 POSTs
	baseDelay  time.Duration // first backoff sleep; doubles each retry
	// now is the clock the timestamp header is read from (time.Now; a test steps it to prove
	// each attempt reads its own).
	now func() time.Time
}

// SignatureHeader (V1) is the lowercase-hex HMAC-SHA256 of the body. The receiver recomputes
// HMAC(secret, body) and compares to authenticate the webhook came from Nexus. Unchanged byte
// for byte since M2 — receivers in production verify it; it has no replay protection (V2 does).
const SignatureHeader = "X-Nexus-Signature"

// Replay-protected signing (specs/050 S2a, FR-020/021). The receiver checks the timestamp is
// within its window (5 minutes recommended) and recomputes V2 = hex(HMAC-SHA256(key, timestamp +
// "." + body)) — a captured request cannot be replayed once the window passes. V2-Previous is V2
// under the previous key during a rotation grace. docs/api-conventions.md "Kiểm chữ ký webhook V2".
const (
	TimestampHeader           = "X-Nexus-Timestamp"
	SignatureV2Header         = "X-Nexus-Signature-V2"
	SignatureV2PreviousHeader = "X-Nexus-Signature-V2-Previous"
)

// DeliveryIDHeader names the delivery a POST belongs to (DAILY-QUEUE #27). Delivery is
// at-least-once — a retry after a timeout the receiver actually processed, a claim the reaper
// handed to a second worker, a resend of the same job — so the receiver must be able to tell a
// repeat from a new event: the id is stable across every attempt of one delivery and differs
// between deliveries. Durable path: the sys_jobs id. Inline path (JOBS_ENABLED=false): one UUID
// per Post call, repeated on its in-process retries.
const DeliveryIDHeader = "X-Nexus-Delivery-Id"

// NewHTTPWebhookSender builds a sender with a per-request timeout and a retry policy. maxRetries
// and baseDelay are clamped to sane minimums so a misconfigured zero never produces a no-attempt
// or busy-loop sender. timeout caps each individual POST (not the whole retry sequence).
func NewHTTPWebhookSender(timeout time.Duration, maxRetries int, baseDelay time.Duration) *HTTPWebhookSender {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if maxRetries < 1 {
		maxRetries = 3
	}
	if baseDelay <= 0 {
		baseDelay = 200 * time.Millisecond
	}
	return &HTTPWebhookSender{
		client:     &http.Client{Timeout: timeout},
		maxRetries: maxRetries,
		baseDelay:  baseDelay,
		now:        time.Now,
	}
}

// Post delivers req, retrying transient failures with exponential backoff. Every attempt signs
// itself (fresh timestamp) and carries the same X-Nexus-Delivery-Id, so a receiver can drop a
// retry it already processed. It returns nil on the first 2xx and the last error after exhausting
// retries. The context cancels the wait between attempts AND each in-flight request, so a
// shutdown drain never hangs on a slow endpoint.
func (s *HTTPWebhookSender) Post(ctx context.Context, req WebhookRequest) error {
	if req.DeliveryID == "" {
		req.DeliveryID = uuid.NewString()
	}

	var lastErr error
	delay := s.baseDelay
	for attempt := 0; attempt < s.maxRetries; attempt++ {
		if attempt > 0 {
			// Backoff between attempts; abort early if the context is cancelled (shutdown).
			select {
			case <-ctx.Done():
				return fmt.Errorf("workflow.HTTPWebhookSender.Post: context cancelled after %d attempts: %w", attempt, ctx.Err())
			case <-time.After(delay):
			}
			delay *= 2
		}

		retryable, err := s.attempt(ctx, req)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			// A permanent failure (4xx other than 429) will never succeed on retry — stop now.
			return fmt.Errorf("workflow.HTTPWebhookSender.Post: non-retryable: %w", err)
		}
	}
	return fmt.Errorf("workflow.HTTPWebhookSender.Post: exhausted %d attempts: %w", s.maxRetries, lastErr)
}

// Deliver makes exactly ONE signed POST and reports whether a retry could succeed — the durable
// job handler's entry point (DAILY-QUEUE #26), where the QUEUE owns retry and backoff. The caller
// sets req.DeliveryID to the job id (#27), so every attempt of one job names the same delivery.
// It must never loop itself: Post's in-process retries under the queue's would multiply into 30
// POSTs per event and pin one of the (default two) queue workers in time.After. Same signing
// headers and status classification as Post, so a receiver cannot tell the two paths apart.
func (s *HTTPWebhookSender) Deliver(ctx context.Context, req WebhookRequest) (retryable bool, err error) {
	retryable, err = s.attempt(ctx, req)
	if err != nil {
		return retryable, fmt.Errorf("workflow.HTTPWebhookSender.Deliver: %w", err)
	}
	return false, nil
}

// signBody is the lowercase-hex HMAC-SHA256 of body under secret, or "" when there is no secret
// (no signature header is sent then).
func signBody(secret string, body []byte) string {
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// nexusOwnedHeaders are the headers attempt computes itself; a rule's own header of the same name is
// dropped. Only these names — a rule's other X-Nexus-* header is the rule's business.
var nexusOwnedHeaders = map[string]bool{
	http.CanonicalHeaderKey(SignatureHeader):           true,
	http.CanonicalHeaderKey(TimestampHeader):           true,
	http.CanonicalHeaderKey(SignatureV2Header):         true,
	http.CanonicalHeaderKey(SignatureV2PreviousHeader): true,
	http.CanonicalHeaderKey(DeliveryIDHeader):          true,
}

// signV2 is the lowercase-hex HMAC-SHA256 of ts + "." + body under secret.
func signV2(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// attempt performs one POST, computing every signing header from req right here (FR-020): the
// durable job handler calls it once per claim through Deliver, so nothing it needs may be computed
// outside. It returns (retryable, err): a network/timeout error or a 5xx/429 status is retryable;
// a 2xx is success (nil err); any other 4xx is a permanent (non-retryable) failure. Errors name
// the target by DisplayURL only. The response body is always drained+closed so the connection can
// be reused, and never read into an error.
func (s *HTTPWebhookSender) attempt(ctx context.Context, req WebhookRequest) (retryable bool, err error) {
	display := req.DisplayURL
	if display == "" {
		display = webhookDisplayURL(req.URL)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		// A malformed URL is permanent — never retryable. url.Parse's error quotes the whole URL.
		return false, fmt.Errorf("build request %s: %w", display, stripURLError(err))
	}
	hr.Header.Set("Content-Type", "application/json")
	for k, v := range req.Headers {
		// The headers Nexus computes are not the rule's to set (specs/050 S2a review): a copy of
		// X-Nexus-Signature-V2-Previous would reach the receiver next to a real signature, and on an
		// unsigned rule any of them would pose as ours (api-conventions §32).
		if nexusOwnedHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		hr.Header.Set(k, v)
	}
	// Signing and the delivery id are set after the rule's own headers, which never carry them.
	if req.SigningSecret != "" {
		ts := strconv.FormatInt(s.now().Unix(), 10)
		hr.Header.Set(SignatureHeader, signBody(req.SigningSecret, req.Body))
		hr.Header.Set(TimestampHeader, ts)
		hr.Header.Set(SignatureV2Header, signV2(req.SigningSecret, ts, req.Body))
		if req.PrevSigningSecret != "" {
			hr.Header.Set(SignatureV2PreviousHeader, signV2(req.PrevSigningSecret, ts, req.Body))
		}
	}
	if req.DeliveryID != "" {
		// A fixed X-Nexus-Delivery-Id copied into a rule would make the receiver drop every
		// delivery after the first as a duplicate.
		hr.Header.Set(DeliveryIDHeader, req.DeliveryID)
	}

	client := s.client
	if req.NoRedirect {
		// A copy sharing the transport (and its connection pool) that hands a 3xx back as is.
		c := *s.client
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &c
	}
	resp, err := client.Do(hr)
	if err != nil {
		// Transport error (DNS, connection refused, timeout) — transient, retry. The *url.Error
		// wrapper repeats the full URL, and this text lands in sys_jobs.last_error,
		// sys_workflow_runs.error and the retry log line.
		return true, fmt.Errorf("post %s: %w", display, stripURLError(err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil // success
	case req.NoRedirect && resp.StatusCode >= 300 && resp.StatusCode < 400:
		// The key stays with its host: a partner answering 3xx will answer it again (permanent).
		return false, &CodedError{Code: CodeRedirectNotFollowed,
			Msg: fmt.Sprintf("post %s: status %d — không theo chuyển hướng khi request mang khoá kết nối", display, resp.StatusCode)}
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return true, fmt.Errorf("post %s: status %d", display, resp.StatusCode) // transient
	default:
		return false, fmt.Errorf("post %s: status %d", display, resp.StatusCode) // permanent 4xx
	}
}

// stripURLError drops a *url.Error wrapper (whose text quotes the full target, credentials and
// all) and keeps its cause; any other error is returned as is.
func stripURLError(err error) error {
	var uerr *neturl.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}
