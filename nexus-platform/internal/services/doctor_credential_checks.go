// doctor_credential_checks.go — specs/050 S3b: C10, the confirm-gated move of the keys that
// outbound_webhook actions saved before the named-credential vault carry in plaintext (a signing
// key and/or auth headers inside sys_workflows.actions) into the vault.
//
// Why a Doctor step and not a migration: SQL cannot seal (the master key lives in Go), and the move
// is one-way — after it, a binary older than S2b does not know {credentialId, path} and the rule
// stops sending (spec Nợ 10). So an operator reads the finding, confirms, and it runs per tenant.
//
// Data safety (FR-024 a–f), in the order the fix runs:
//   - no master key ⇒ nothing is touched (CREDENTIALS_NOT_CONFIGURED per rule); the scan still works;
//   - every rule is re-read and re-classified before it is changed (a stale finding is skipped);
//   - key first, rule after: the key is stored AND opened back to exactly what the action sends
//     today before the rule is rewritten; one rule per step, so a failure leaves that rule with its
//     inline key untouched and the stored key waiting for the next run;
//   - reuse before create: a key C10 stored earlier for the same (origin, key, headers) is reused,
//     so a re-run after a failure (or a second operator) mints no duplicate;
//   - the rewrite is a compare-and-swap on the actions the fix read, and the new action goes through
//     the same buildActionsJSON body as every other rule write.
//
// Left where it is (reported, never touched): an action whose credential sits in its URL — userinfo
// (a basic key) or a token in the path of a known host (Telegram, Slack, Google Chat) — plain http to
// a non-loopback host, headers the vault cannot hold, a path the vault join cannot reproduce.
//
// Soft-deleted rules (deleted_at set) keep their actions with no purge and no restore, so their keys
// would outlive the move in the table and every backup. They never run again: C10 does not move their
// keys, it wipes them (secret, headers, the URL down to its origin) — no master key needed.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/credential"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/workflow"
)

// DoctorCheckWebhookInlineSecret is the C10 check id (C = soft metadata/config drift; C9 is taken
// by C9_LOCKED_REQUIRED_FIELDS, queue #143).
const DoctorCheckWebhookInlineSecret = "C10_WEBHOOK_INLINE_SECRET"

// legacyNameTries bounds the -2, -3 … suffixes tried when a key name is taken.
const legacyNameTries = 20

// doctorCredentialVault is the slice of *CredentialService C10 needs.
type doctorCredentialVault interface {
	Configured() bool
	AllowsInsecureHTTP() bool
	ImportLegacy(ctx context.Context, tenantID string, in CredentialInput) (*CredentialView, error)
	FindLegacyImport(ctx context.Context, tenantID, baseURL string, authType credential.AuthType, want workflow.WebhookAuth) (string, error)
	ResolveAuth(ctx context.Context, tenantID, credentialID string) (workflow.WebhookAuth, error)
}

// doctorLegacyRules is the slice of *workflow.SQLRepository C10 reads and rewrites (tenant-pinned).
type doctorLegacyRules interface {
	List(ctx context.Context, tenantID string) ([]workflow.WorkflowRecord, error)
	Get(ctx context.Context, tenantID, id string) (*workflow.WorkflowRecord, error)
	ReplaceActions(ctx context.Context, tenantID, id string, expect, next []byte, updatedBy string) error
	ListDeleted(ctx context.Context, tenantID string) ([]workflow.WorkflowRecord, error)
	GetDeleted(ctx context.Context, tenantID, id string) (*workflow.WorkflowRecord, error)
	ReplaceDeletedActions(ctx context.Context, tenantID, id string, next []byte) error
}

// deletedRuleKey prefixes the finding key of a soft-deleted rule (its keys are wiped, not moved).
const deletedRuleKey = "deleted:"

// doctorPendingWebhooks is the durable queue's probes (*EnqueuingWebhookQueue): are deliveries of a
// rule still queued or running, and how many failed ones per slot could "Gửi lại" still send?
type doctorPendingWebhooks interface {
	HasPendingWebhooks(ctx context.Context, tenantID, workflowID string) (bool, error)
	FailedWebhookSlots(ctx context.Context, tenantID, workflowID string) (map[int]int, error)
}

// WithInlineSecretMigration registers C10 over the vault, the rule store and the durable webhook
// queue (all three required). Chainable.
func (s *DoctorService) WithInlineSecretMigration(v doctorCredentialVault, rules doctorLegacyRules, pending doctorPendingWebhooks) *DoctorService {
	s.vault, s.legacyRules, s.pendingWebhooks = v, rules, pending
	s.runner = doctor.NewRunner(s.checks())
	return s
}

// legacyMove is one outbound_webhook action of a rule that carries a key of its own (or a
// credential in its URL, which C10 reports but leaves).
type legacyMove struct {
	index    int                  // position in the rule's actions
	display  string               // scheme://host[:port] — the only part of the URL a finding shows
	host     string               // host[:port] for the key's name and label
	base     string               // the URL up to the host, byte for byte — the key's base URL
	path     string               // the rest of the URL (path + query), appended unchanged
	authType credential.AuthType  // hmac when the action signs, header otherwise
	header   string               // header type: the header the key itself sets
	secret   string               // hmac: the signing key; header: that header's whole value
	extra    map[string]string    // every other header, sealed as the key's extra headers
	want     workflow.WebhookAuth // what the action sends today — the stored key must open to it
	notes    []string             // advice kept in the finding
	manual   string               // non-empty: left as it is — why, and what to do by hand
}

// knownKeyInURL reports the hosts whose integrations put the key in the URL itself.
func knownKeyInURL(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return host == "api.telegram.org" && strings.HasPrefix(u.Path, "/bot") ||
		host == "hooks.slack.com" && strings.HasPrefix(u.Path, "/services/") ||
		host == "chat.googleapis.com"
}

// classifyLegacyWebhook decides what C10 does with one action. ok=false: nothing to move or report.
func classifyLegacyWebhook(a workflow.Action, index int, allowInsecureHTTP bool) (m legacyMove, ok bool) {
	if a.Type != workflow.ActionWebhook || strings.TrimSpace(a.CredentialID) != "" {
		return m, false
	}
	m.index = index
	hasKey := a.Secret != "" || len(a.Headers) > 0
	raw := strings.TrimSpace(a.URL)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// An error here may quote the URL — never shown.
		m.display, m.manual = "(url không đọc được)", "sửa URL của action rồi chạy lại"
		return m, hasKey
	}
	m.display, m.host = u.Scheme+"://"+u.Host, u.Host
	switch {
	case u.User != nil:
		m.manual = "tài khoản/mật khẩu nằm ngay trong URL — tạo khoá kiểu basic ở Quản trị › Kho khoá kết nối rồi chọn cho action (Bác sĩ không tự tách)"
		return m, true
	case knownKeyInURL(u):
		m.manual = "token nằm trong đường dẫn của URL (Telegram / Slack / Google Chat) — giữ nguyên, chuyển tay sang khoá kiểu \"khoá trong đường dẫn\" (url_token) ở Quản trị › Kho khoá kết nối; khoá nằm ở query (Google Chat) thì kiểu này chưa nhận"
		return m, true
	case !hasKey:
		return m, false
	}
	prefix := len(u.Scheme) + len("://") + len(u.Host)
	if len(raw) < prefix || !strings.EqualFold(raw[:prefix], m.display) {
		m.manual = "không tách được URL thành địa chỉ gốc + đường dẫn — chuyển tay"
		return m, true
	}
	m.base, m.path = raw[:prefix], raw[prefix:]
	if err := credential.ValidateBaseURL(m.base, allowInsecureHTTP); err != nil {
		if strings.EqualFold(u.Scheme, "http") {
			m.manual = "http không mã hoá tới máy ngoài — đổi URL sang https rồi chạy lại (kho không giữ khoá cho địa chỉ không mã hoá)"
		} else {
			m.manual = "địa chỉ gốc không hợp lệ cho kho khoá — chuyển tay"
		}
		return m, true
	}
	if joined, err := workflow.JoinCredentialURL(m.base, m.path); err != nil || joined != raw {
		m.manual = "đường dẫn của URL có ký tự kho khoá không ghép lại y nguyên được — chuyển tay"
		return m, true
	}
	if len(a.Secret) > credentialMaxSecretLen || credential.ValidateExtraHeaders(a.Headers) != nil {
		m.manual = "khoá ký hoặc header không cất được vào kho (tối đa 10 header / 4KB, không trùng tên, không thuộc nhóm dành riêng như Host, Content-Type, X-Nexus-*) — chuyển tay"
		return m, true
	}

	sent := make(map[string]string, len(a.Headers))
	for k, v := range a.Headers {
		sent[k] = v
	}
	m.want = workflow.WebhookAuth{SigningSecret: a.Secret, Headers: sent}
	m.extra = make(map[string]string, len(sent))
	for k, v := range sent {
		m.extra[k] = v
	}
	if a.Secret != "" {
		m.authType, m.secret = credential.AuthHMAC, a.Secret
		if len(a.Secret) < credentialMinHMACLen {
			m.notes = append(m.notes, "khoá ký ngắn hơn 32 ký tự — sau khi dời nên xoay sang khoá Nexus tự sinh")
		}
	} else {
		// The first header by name that carries a value becomes the key's own header, with no
		// prefix: its whole value ("Bearer …" included) is the sealed secret, byte for byte.
		names := make([]string, 0, len(sent))
		for k := range sent {
			names = append(names, k)
		}
		sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
		for _, n := range names {
			if sent[n] != "" {
				m.authType, m.header, m.secret = credential.AuthHeader, n, sent[n]
				delete(m.extra, n)
				break
			}
		}
		if m.header == "" {
			m.manual = "header của action không có giá trị nào để cất — chuyển tay"
			return m, true
		}
	}
	if len(m.extra) == 0 {
		m.extra = nil
	}
	if u.RawQuery != "" || u.ForceQuery {
		m.notes = append(m.notes, "URL có query — vẫn nằm trong quy trình (bị che khi đọc): kiểm tay xem query có chứa token không")
	}
	return m, true
}

// legacyMovesOf decodes a rule's actions and classifies each. err: the actions do not decode (the
// engine sends nothing for such a rule either).
func legacyMovesOf(raw json.RawMessage, allowInsecureHTTP bool) ([]json.RawMessage, []legacyMove, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, nil, err
	}
	var moves []legacyMove
	for i, e := range elems {
		var a workflow.Action
		if err := json.Unmarshal(e, &a); err != nil {
			continue
		}
		if m, ok := classifyLegacyWebhook(a, i, allowInsecureHTTP); ok {
			moves = append(moves, m)
		}
	}
	return elems, moves, nil
}

// describeLegacyMoves is the finding: names, origins and advice — never a key, a header value, a
// path or a query (any of them may be the credential).
func describeLegacyMoves(ruleName string, moves []legacyMove) string {
	var b strings.Builder
	fmt.Fprintf(&b, "quy trình %q:", ruleName)
	moving := false
	for _, m := range moves {
		fmt.Fprintf(&b, " action #%d → %s: ", m.index+1, m.display)
		if m.manual != "" {
			b.WriteString("GIỮ NGUYÊN — " + m.manual + ".")
			continue
		}
		moving = true
		var what []string
		if m.authType == credential.AuthHMAC {
			what = append(what, "khoá ký HMAC")
		}
		if len(m.want.Headers) > 0 {
			names := make([]string, 0, len(m.want.Headers))
			for n := range m.want.Headers {
				names = append(names, n)
			}
			sort.Strings(names)
			what = append(what, "header "+strings.Join(names, ", "))
		}
		fmt.Fprintf(&b, "%s ⇒ dời vào Kho khoá kết nối (khoá %q, dùng lại nếu đã có).", strings.Join(what, " + "), legacyCredentialLabel(m.host))
		for _, n := range m.notes {
			b.WriteString(" " + n + ".")
		}
	}
	if moving {
		b.WriteString(" Sau khi dời: header và chữ ký V1 gửi đi giữ nguyên từng byte, thêm X-Nexus-Signature-V2 + X-Nexus-Timestamp;" +
			" request mang khoá KHÔNG theo chuyển hướng 3xx nữa. Bước một chiều: lùi bản phần mềm về trước bản có kho khoá" +
			" (specs/050 S2b) làm quy trình đã dời ngừng gửi, khoá trần không tự quay lại.")
	}
	return b.String()
}

var legacyNameJunk = regexp.MustCompile(`[^a-z0-9]+`)

// legacyCredentialName is the key name for host: webhook- + the host in lowercase with every run of
// characters outside [a-z0-9] turned into one "-", cut so the -n suffix still fits 64 characters.
func legacyCredentialName(host string, n int) string {
	name := "webhook-" + strings.Trim(legacyNameJunk.ReplaceAllString(strings.ToLower(host), "-"), "-")
	const room = 64 - 3 // "-99"
	if len(name) > room {
		name = name[:room]
	}
	name = strings.TrimRight(name, "-")
	if n > 1 {
		name += "-" + strconv.Itoa(n)
	}
	return name
}

// legacyCredentialLabel is "Webhook <host> (dời từ quy trình)", the host shortened to fit the label
// column; the suffix is the marker FindLegacyImport reuses by.
func legacyCredentialLabel(host string) string {
	room := credentialLabelMax - utf8.RuneCountInString("Webhook "+CredentialLegacyLabelSuffix)
	if r := []rune(host); len(r) > room {
		host = string(r[:room-1]) + "…"
	}
	return "Webhook " + host + CredentialLegacyLabelSuffix
}

func (s *DoctorService) checkWebhookInlineSecret() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckWebhookInlineSecret,
		Severity:    doctor.SeverityWarn, // partner keys in plaintext in sys_workflows
		AutoFixSafe: false,               // one-way: a binary before S2b cannot send a moved rule
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			// ponytail: one read of every rule of the tenant (enabled or not — a disabled rule can be
			// re-enabled) per scan, hundreds in practice; findings are capped below. A JSON_TABLE
			// prefilter if a tenant grows past ~5k rules.
			rules, err := s.legacyRules.List(ctx, tenantID)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.C10 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0)
			for _, rec := range rules {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				_, moves, err := legacyMovesOf(rec.Actions, s.vault.AllowsInsecureHTTP())
				if err != nil || len(moves) == 0 {
					continue
				}
				if err := s.noteFailedDeliveries(ctx, tenantID, rec.ID, moves); err != nil {
					return nil, err
				}
				findings = append(findings, doctor.Finding{Key: rec.ID, Detail: describeLegacyMoves(rec.Name, moves)})
			}
			// ponytail: every deleted rule of the tenant, read per scan like the live ones above; a
			// JSON prefilter if deleted rules pile up past ~5k.
			deleted, err := s.legacyRules.ListDeleted(ctx, tenantID)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.C10 detect deleted: %w", err)
			}
			for _, rec := range deleted {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				if _, moves, err := legacyMovesOf(rec.Actions, s.vault.AllowsInsecureHTTP()); err == nil && len(moves) > 0 {
					findings = append(findings, doctor.Finding{Key: deletedRuleKey + rec.ID, Detail: describeDeletedRule(rec.Name, moves)})
				}
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			if id, ok := strings.CutPrefix(f.Key, deletedRuleKey); ok {
				if err := s.wipeDeletedRuleKeys(ctx, tenantID, id); err != nil {
					return err
				}
				s.recordFix(ctx, tenantID, DoctorCheckWebhookInlineSecret, f)
				return nil
			}
			if !s.vault.Configured() {
				return doctorPublicErrf("CREDENTIALS_NOT_CONFIGURED — máy chủ chưa có khoá mã hoá chủ (CREDENTIAL_ENC_KEY) nên không cất được khoá nào; quy trình giữ nguyên")
			}
			rec, err := s.legacyRules.Get(ctx, tenantID, f.Key)
			if errors.Is(err, workflow.ErrWorkflowNotFound) {
				return doctor.ErrAlreadyClean
			}
			if err != nil {
				return fmt.Errorf("services.Doctor.C10 re-detect: %w", err)
			}
			if err := s.moveInlineKeys(ctx, tenantID, rec); err != nil {
				return err
			}
			s.recordFix(ctx, tenantID, DoctorCheckWebhookInlineSecret, f)
			return nil
		},
	}
}

// noteFailedDeliveries adds to each action C10 would move how many of its failed deliveries lose their
// resend. A failed job keeps its payload for JOBS_RETENTION_DAYS and "Gửi lại" can send it — until its
// slot names a key: the job handler then fails it for good ("rule đã đổi"). Refusing the move as for a
// pending delivery would hold it for up to a month, so the operator is told before confirming (the
// finding sits under the confirm strip, and the fix audits it).
// ponytail: one grouped count per rule with something to move — rare rows, never a hot path.
func (s *DoctorService) noteFailedDeliveries(ctx context.Context, tenantID, workflowID string, moves []legacyMove) error {
	if len(movableOf(moves)) == 0 {
		return nil
	}
	failed, err := s.pendingWebhooks.FailedWebhookSlots(ctx, tenantID, workflowID)
	if err != nil {
		return fmt.Errorf("services.Doctor.C10 detect: failed deliveries: %w", err)
	}
	for i := range moves {
		if n := failed[moves[i].index]; n > 0 && moves[i].manual == "" {
			moves[i].notes = append(moves[i].notes, fmt.Sprintf("%d lần gửi đã lỗi của action này (Quản trị › Việc nền) sẽ KHÔNG Gửi lại được sau khi dời — "+
				"bấm Gửi lại trước (đợi hàng đợi xong) nếu còn cần, rồi mới dời", n))
		}
	}
	return nil
}

// describeDeletedRule is a deleted rule's finding: origins only, never a key, a path or a query.
func describeDeletedRule(ruleName string, moves []legacyMove) string {
	seen := map[string]bool{}
	var origins []string
	for _, m := range moves {
		if !seen[m.display] {
			seen[m.display] = true
			origins = append(origins, m.display)
		}
	}
	sort.Strings(origins)
	return fmt.Sprintf("quy trình ĐÃ XOÁ %q: %d action webhook còn khoá trần (%s) ⇒ xoá khỏi bản ghi: bỏ khoá ký và header, URL chỉ còn địa chỉ gốc. "+
		"Không dời vào kho — quy trình đã xoá không chạy lại (không cần khoá chủ). Khoá này từng nằm trần trong DB và các bản sao lưu — nên xoay ở phía đối tác.",
		ruleName, len(moves), strings.Join(origins, ", "))
}

// wipeDeletedRuleKeys wipes, from one soft-deleted rule, every key C10 would move or list: secret and
// headers dropped, the URL cut to scheme://host[:port] (userinfo and path tokens go with the rest; an
// unreadable URL is dropped). The other fields and actions stay as stored. ErrAlreadyClean: nothing
// left to wipe (a stale finding, or a concurrent run).
func (s *DoctorService) wipeDeletedRuleKeys(ctx context.Context, tenantID, id string) error {
	rec, err := s.legacyRules.GetDeleted(ctx, tenantID, id)
	if errors.Is(err, workflow.ErrWorkflowNotFound) {
		return doctor.ErrAlreadyClean
	}
	if err != nil {
		return fmt.Errorf("services.Doctor.C10 re-detect deleted: %w", err)
	}
	elems, moves, err := legacyMovesOf(rec.Actions, s.vault.AllowsInsecureHTTP())
	if err != nil || len(moves) == 0 {
		return doctor.ErrAlreadyClean
	}
	for _, m := range moves {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(elems[m.index], &fields); err != nil {
			return fmt.Errorf("services.Doctor.C10 wipe deleted: decode action #%d: %w", m.index+1, err)
		}
		var rawURL string
		if v, ok := fields["url"]; ok {
			if err := json.Unmarshal(v, &rawURL); err != nil {
				return fmt.Errorf("services.Doctor.C10 wipe deleted: decode url #%d: %w", m.index+1, err)
			}
		}
		delete(fields, "secret")
		delete(fields, "headers")
		delete(fields, "url")
		if u, perr := url.Parse(strings.TrimSpace(rawURL)); perr == nil && u.Host != "" {
			origin, err := json.Marshal(u.Scheme + "://" + u.Host)
			if err != nil {
				return fmt.Errorf("services.Doctor.C10 wipe deleted: encode url #%d: %w", m.index+1, err)
			}
			fields["url"] = origin
		}
		one, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("services.Doctor.C10 wipe deleted: encode action #%d: %w", m.index+1, err)
		}
		elems[m.index] = one
	}
	body, err := json.Marshal(elems)
	if err != nil {
		return fmt.Errorf("services.Doctor.C10 wipe deleted: encode actions: %w", err)
	}
	if err := s.legacyRules.ReplaceDeletedActions(ctx, tenantID, id, body); errors.Is(err, workflow.ErrWorkflowNotFound) {
		return doctor.ErrAlreadyClean
	} else if err != nil {
		return fmt.Errorf("services.Doctor.C10 wipe deleted: %w", err)
	}
	s.logger.Info("services.Doctor.C10: inline webhook keys wiped from a deleted rule",
		zap.String("tenant_id", tenantID), zap.String("workflow_id", id), zap.Int("actions", len(moves)),
		zap.String("user_id", audit.ActorFromContext(ctx)))
	return nil
}

// movableOf keeps the moves C10 performs (the others stay for the operator).
func movableOf(moves []legacyMove) []legacyMove {
	out := moves[:0:0]
	for _, m := range moves {
		if m.manual == "" {
			out = append(out, m)
		}
	}
	return out
}

// moveInlineKeys moves every movable action of one rule: store (or reuse) each key and open it back,
// then rewrite the rule once. ErrAlreadyClean: nothing left to move (stale finding, or only actions
// to move by hand).
func (s *DoctorService) moveInlineKeys(ctx context.Context, tenantID string, rec *workflow.WorkflowRecord) error {
	elems, moves, err := legacyMovesOf(rec.Actions, s.vault.AllowsInsecureHTTP())
	if err != nil {
		return doctor.ErrAlreadyClean // undecodable actions send nothing — no key to lose
	}
	movable := movableOf(moves)
	if len(movable) == 0 {
		return doctor.ErrAlreadyClean
	}
	// A delivery parked before the move names no key; once its slot names one the job handler drops
	// it for good ("rule đã đổi", and "Gửi lại" fails the same way). Wait for the queue to drain.
	// ponytail: an event firing between this probe and the write below still parks one such job. On
	// ONE server that window is milliseconds; with several, wfCache.Invalidate below only clears the
	// cache of THIS process — another keeps the old rule for up to DefaultWorkflowTTL (30 s) and parks
	// jobs with credentialId "" that the handler then cancels for good ("rule đã đổi"). A stable action
	// key (#228) closes both; until then run C10 on a rule that is paused or quiet.
	pending, err := s.pendingWebhooks.HasPendingWebhooks(ctx, tenantID, rec.ID)
	if err != nil {
		return fmt.Errorf("services.Doctor.C10 fix: pending deliveries: %w", err)
	}
	if pending {
		// A rule that fires all the time almost always has a delivery waiting, so "run again when the
		// queue is done" may never come true — say how to get there.
		return doctorPublicErrf("quy trình còn lần gửi webhook đang chờ trong Việc nền — dời lúc này làm chúng bị huỷ. " +
			"Quy trình bắn liên tục thì hàng đợi khó trống: tạm tắt quy trình (Studio › Quy trình, nút nguồn cạnh tên quy trình), đợi Việc nền gửi xong, chạy lại bước này rồi bật lại " +
			"(sự kiện xảy ra lúc đang tắt không được gửi). Quy trình giữ nguyên")
	}
	next := append([]json.RawMessage(nil), elems...)
	created := map[string]bool{} // key id → stored by this run (false = reused)
	for _, m := range movable {
		id, isNew, err := s.storeLegacyKey(ctx, tenantID, m)
		if err != nil {
			return err // the rule keeps every inline key
		}
		created[id] = created[id] || isNew
		// The same body every rule write goes through (API, pack, AI) — it re-checks the path.
		one, err := buildActionsJSON([]WorkflowActionInput{{Type: workflow.ActionWebhook, CredentialID: id, Path: m.path}}, nil)
		if err != nil {
			return fmt.Errorf("services.Doctor.C10 fix: build action #%d: %w", m.index+1, err)
		}
		var built []json.RawMessage
		if err := json.Unmarshal(one, &built); err != nil || len(built) != 1 {
			return fmt.Errorf("services.Doctor.C10 fix: decode built action #%d: %v", m.index+1, err)
		}
		next[m.index] = built[0]
	}
	body, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("services.Doctor.C10 fix: encode actions: %w", err)
	}
	actor := audit.ActorFromContext(ctx)
	switch err := s.legacyRules.ReplaceActions(ctx, tenantID, rec.ID, rec.Actions, body, actor); {
	case errors.Is(err, workflow.ErrWorkflowNotFound):
		return doctor.ErrAlreadyClean
	case errors.Is(err, workflow.ErrWorkflowChanged):
		// Saved in between (a second operator's C10, or the builder): moved already ⇒ clean.
		cur, gerr := s.legacyRules.Get(ctx, tenantID, rec.ID)
		if gerr == nil {
			if _, left, derr := legacyMovesOf(cur.Actions, s.vault.AllowsInsecureHTTP()); derr != nil || len(movableOf(left)) == 0 {
				return doctor.ErrAlreadyClean
			}
		}
		return doctorPublicErrf("quy trình vừa được lưu trong lúc dời — chạy lại (khoá đã cất sẽ được dùng lại)")
	case err != nil:
		return fmt.Errorf("services.Doctor.C10 fix: rewrite rule: %w", err)
	}
	s.wfCache.Invalidate(tenantID, rec.EntityName) // the next save sends through the vault key
	for id, isNew := range created {
		s.auditor.Record(ctx, audit.Event{
			TenantID:    tenantID,
			ActorUserID: actor,
			EntityName:  audit.NamedCredentialMeta,
			RecordID:    id,
			Action:      audit.ActionUpdate,
			Diff: audit.ComputeDiff(nil, map[string]any{"doctorCheck": DoctorCheckWebhookInlineSecret,
				"workflowIds": []string{rec.ID}, "reused": !isNew}),
			At: time.Now().UTC(),
		})
	}
	s.logger.Info("services.Doctor.C10: inline webhook keys moved into the vault",
		zap.String("tenant_id", tenantID), zap.String("workflow_id", rec.ID),
		zap.Int("actions", len(movable)), zap.Int("keys", len(created)), zap.String("user_id", actor))
	return nil
}

// storeLegacyKey returns the vault key for one action — reused when C10 stored one for the same
// material, stored otherwise — after opening it back to exactly what the action sends today.
func (s *DoctorService) storeLegacyKey(ctx context.Context, tenantID string, m legacyMove) (id string, isNew bool, err error) {
	for n := 1; n <= legacyNameTries; n++ {
		// Looked up on every try: a name taken meanwhile may be a twin a concurrent run just stored.
		found, err := s.vault.FindLegacyImport(ctx, tenantID, m.base, m.authType, m.want)
		if err != nil {
			return "", false, fmt.Errorf("services.Doctor.C10 fix: find stored key: %w", err)
		}
		if found != "" {
			id = found
			break
		}
		in := CredentialInput{Name: legacyCredentialName(m.host, n), Label: legacyCredentialLabel(m.host),
			BaseURL: m.base, AuthType: string(m.authType), Secret: &m.secret, ExtraHeaders: m.extra,
			ActorID: audit.ActorFromContext(ctx)}
		if m.authType == credential.AuthHeader {
			header, prefix := m.header, ""
			in.HeaderName, in.HeaderPrefix = &header, &prefix
		}
		v, err := s.vault.ImportLegacy(ctx, tenantID, in)
		var ve *ValidationError
		switch {
		case errors.As(err, &ve) && ve.Code == "CREDENTIAL_NAME_TAKEN":
			continue
		case errors.As(err, &ve):
			return "", false, doctorPublicErrf("%s — %s; quy trình giữ nguyên", ve.Code, ve.Msg)
		case err != nil:
			return "", false, fmt.Errorf("services.Doctor.C10 fix: store key: %w", err)
		}
		id, isNew = v.ID, true
		break
	}
	if id == "" {
		return "", false, doctorPublicErrf("đã thử %d tên khoá %q… đều bị dùng — đổi tên một khoá cũ rồi chạy lại; quy trình giữ nguyên",
			legacyNameTries, legacyCredentialName(m.host, 1))
	}
	// Key first, rule after: the stored key must open to exactly what the action sends today.
	got, err := s.vault.ResolveAuth(ctx, tenantID, id)
	if err != nil || got.BaseURL != m.base || !sameWireAuth(got, m.want) {
		s.logger.Warn("services.Doctor.C10: stored key does not open back to the inline key — rule left as is",
			zap.String("tenant_id", tenantID), zap.String("credential_id", id), zap.Error(err))
		return "", false, doctorPublicErrf("khoá vừa cất không mở lại đúng khoá của action #%d — quy trình giữ nguyên", m.index+1)
	}
	return id, isNew, nil
}
