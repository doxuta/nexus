// CONG-TIN-HIEU (../specs/015-inbound-hooks/spec.md): per-tenant HMAC-signed inbound webhooks. A hook is
// metadata — target entity + payload→field mapping (+ optional unique upsert key). The receive
// path verifies the signature, extracts the payload through the mapping and writes through the
// SAME RecordService as a hand-made request, so validation, tenant isolation, audit and the
// workflow dispatcher all fire exactly as if a user typed it in.
package services

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/workflow"
)

// ErrHookUnauthorized is the SINGLE receive-path rejection: unknown tenant, unknown hook,
// disabled hook and bad signature are deliberately indistinguishable (anti-probing).
var ErrHookUnauthorized = errors.New("services: hook unauthorized")

// hookTenantResolver resolves a public URL slug to a tenant (auth.UserRepository).
type hookTenantResolver interface {
	ResolveTenantBySlug(ctx context.Context, slug string) (*auth.Tenant, error)
}

// hookStore is the repository seam (satisfied by *inboundhook.Repository).
type hookStore interface {
	Create(ctx context.Context, h *inboundhook.Hook) error
	Update(ctx context.Context, tenantID string, h *inboundhook.Hook) error
	UpdateSecret(ctx context.Context, tenantID, id, secret string) error
	SoftDelete(ctx context.Context, tenantID, id string) error
	GetByID(ctx context.Context, tenantID, id string) (*inboundhook.Hook, error)
	GetByName(ctx context.Context, tenantID, name string) (*inboundhook.Hook, error)
	ListAll(ctx context.Context, tenantID string) ([]inboundhook.Hook, error)
	RecordDelivery(ctx context.Context, d *inboundhook.Delivery) error
	ListDeliveries(ctx context.Context, tenantID, hookID string, limit int) ([]inboundhook.Delivery, error)
}

// InboundHookService validates hook configs and processes signed inbound signals.
type InboundHookService struct {
	store   hookStore
	rec     *RecordService
	tenants hookTenantResolver
	logger  *zap.Logger
	// auditor receives one __inbound_hook__ row per committed create / update / rotate / delete
	// (specs/062 L4, FR-023). Never drafted — a hook is an entry door, written straight.
	auditor audit.Auditor
	// users answers "is this id a user OF the hook's tenant?" for D-22. nil = not wired: the caller's
	// id is stored as before (unit tests without a user store).
	users hookUserReader
	// freeze (specs/068 slice B): a correctly signed signal for a write-frozen tenant is refused with
	// ErrTenantMaintenance so the partner retries. nil = not enforced.
	freeze FreezeReader
}

// hookUserReader is the consumer-side slice of the user store D-22 needs (auth.UserRepository,
// tenant-scoped: an id that exists only in another tenant is ErrUserNotFound here).
type hookUserReader interface {
	GetUserByID(ctx context.Context, tenantID, id string) (*auth.User, error)
}

func NewInboundHookService(store hookStore, rec *RecordService, tenants hookTenantResolver, logger *zap.Logger) *InboundHookService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &InboundHookService{store: store, rec: rec, tenants: tenants, logger: logger, auditor: audit.NewNoopAuditor()}
}

// WithAuditor injects the audit sink for hook writes. Chainable; a nil auditor keeps the noop.
func (s *InboundHookService) WithAuditor(a audit.Auditor) *InboundHookService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// WithTenantFreeze wires the write-freeze gate for Receive. Chainable; nil keeps it off.
func (s *InboundHookService) WithTenantFreeze(f FreezeReader) *InboundHookService {
	s.freeze = f
	return s
}

// WithUserReader wires the tenant-scoped user lookup behind D-22. Chainable.
func (s *InboundHookService) WithUserReader(u hookUserReader) *InboundHookService {
	s.users = u
	return s
}

// hookCreatedBy decides whose id a new hook runs under (specs/062 D-22, FR-023a) — the ONE place, so
// the hook screen, "Cài mẫu" and a pack install (Pass 8e) all agree. A hook runs every signal as
// created_by with admin scope, and the customer's trail names that id on each record it writes; if
// the creator is a Nexus builder working inside the customer (no users row in THIS tenant), their
// name would be stamped on the partner system's writes for as long as the hook lives. Such a hook
// runs as inboundhook.SystemActorID instead. A creator who is a user of the tenant keeps today's
// behaviour; an unreadable users table refuses the create (never guess an identity).
func (s *InboundHookService) hookCreatedBy(ctx context.Context, tenantID, userID string) (string, error) {
	if s.users == nil {
		return userID, nil
	}
	if _, err := s.users.GetUserByID(ctx, tenantID, userID); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return inboundhook.SystemActorID, nil
		}
		return "", fmt.Errorf("services.InboundHookService.hookCreatedBy: %w", err)
	}
	return userID, nil
}

// InboundHookInput is the create/update payload (handler-mapped).
type InboundHookInput struct {
	Name         string
	EntityName   string
	Mapping      []inboundhook.Mapping
	MatchField   string                    // "" = always create
	PaymentMatch *inboundhook.PaymentMatch // nil = no bank-credit matching step (p2)
	IsActive     *bool                     // nil = true
}

var hookNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// validate runs the full config-time validation (spec §4).
func (s *InboundHookService) validate(ctx context.Context, tenantID string, in *InboundHookInput) error {
	in.Name = strings.TrimSpace(in.Name)
	in.EntityName = strings.TrimSpace(in.EntityName)
	in.MatchField = strings.TrimSpace(in.MatchField)

	if !hookNameRe.MatchString(in.Name) {
		return invalidField("HOOK_INVALID", "name", "name phải là slug [a-z0-9_-], tối đa 64 ký tự")
	}
	if len(in.Mapping) == 0 {
		return invalidField("HOOK_INVALID", "mapping", "cần ít nhất một dòng mapping")
	}
	if len(in.Mapping) > 40 {
		return invalidField("HOOK_INVALID", "mapping", "tối đa 40 dòng mapping")
	}

	_, specs, err := s.rec.resolve(ctx, tenantID, in.EntityName)
	if err != nil {
		var ve *ValidationError
		if errors.Is(err, ErrEntityNotFound) || errors.As(err, &ve) {
			return invalidField("HOOK_INVALID", "entityName", "entity %q không tồn tại", in.EntityName)
		}
		return fmt.Errorf("services.InboundHookService.validate: resolve: %w", err)
	}
	byName := make(map[string]FieldSpec, len(specs))
	for _, f := range specs {
		byName[f.Name] = f
	}

	for i := range in.Mapping {
		m := &in.Mapping[i]
		m.Source = strings.TrimSpace(m.Source)
		m.Field = strings.TrimSpace(m.Field)
		if m.Source == "" || len(m.Source) > 200 {
			return invalidField("HOOK_INVALID", "mapping", "source path rỗng hoặc quá 200 ký tự")
		}
		if _, ok := byName[m.Field]; !ok {
			if _, isSub := polymorphicSubField(byName, m.Field); !isSub {
				return invalidField("HOOK_INVALID", m.Field, "field %q không có trong entity", m.Field)
			}
		}
	}

	if in.MatchField != "" {
		spec, ok := byName[in.MatchField]
		if !ok {
			return invalidField("HOOK_INVALID", "matchField", "field %q không có trong entity", in.MatchField)
		}
		// The upsert key must identify exactly one record — only unique fields qualify.
		if !spec.IsUnique {
			return invalidField("HOOK_INVALID", "matchField", "matchField phải là field unique (khoá upsert)")
		}
		// …and the mapping must actually FEED it (review W2): a match field no mapping row
		// targets can never carry a value → every signal silently creates, defeating upsert.
		feeds := false
		for _, m := range in.Mapping {
			if m.Field == in.MatchField {
				feeds = true
				break
			}
		}
		if !feeds {
			// The refusal is right, but on its own it sends people hunting for a mapping row to add
			// — when the usual correct answer is the opposite: DROP matchField. A reconciliation
			// hook (bank feed → paymentMatch) creates one row per signal on purpose and never
			// upserts, so it needs no match key at all. That took three tries to discover from a
			// message that only said what was missing, so the message now says what to do.
			return invalidField("HOOK_INVALID", "matchField",
				"matchField %q phải xuất hiện trong mapping (nếu không tín hiệu nào cũng tạo mới). "+
					"Hook đối soát (khớp bằng paymentMatch) thường KHÔNG cần matchField — bỏ trống là đúng; "+
					"chỉ dùng matchField khi muốn tín hiệu trùng khoá GHI ĐÈ bản ghi cũ",
				in.MatchField)
		}
	}

	if in.PaymentMatch != nil {
		if err := s.validatePaymentMatch(ctx, tenantID, in.PaymentMatch); err != nil {
			return err
		}
	}
	return nil
}

// validatePaymentMatch runs the config-time checks of the p2 matching step (../specs/016-payment-flow/spec.md §8).
func (s *InboundHookService) validatePaymentMatch(ctx context.Context, tenantID string, pm *inboundhook.PaymentMatch) error {
	pm.TargetEntity = strings.TrimSpace(pm.TargetEntity)
	pm.RefField = strings.TrimSpace(pm.RefField)
	pm.RefPattern = strings.TrimSpace(pm.RefPattern)
	pm.MemoSource = strings.TrimSpace(pm.MemoSource)
	pm.StatusField = strings.TrimSpace(pm.StatusField)
	pm.StatusValue = strings.TrimSpace(pm.StatusValue)
	pm.AmountSource = strings.TrimSpace(pm.AmountSource)
	pm.AmountField = strings.TrimSpace(pm.AmountField)

	if pm.MemoSource == "" || len(pm.MemoSource) > 200 {
		return invalidField("HOOK_INVALID", "paymentMatch.memoSource", "memoSource rỗng hoặc quá 200 ký tự")
	}
	if pm.RefPattern == "" {
		return invalidField("HOOK_INVALID", "paymentMatch.refPattern", "refPattern là bắt buộc (regex tìm mã trong memo)")
	}
	if _, err := regexp.Compile(pm.RefPattern); err != nil {
		return invalidField("HOOK_INVALID", "paymentMatch.refPattern", "refPattern không phải regex hợp lệ: %v", err)
	}
	if pm.StatusField == "" || pm.StatusValue == "" {
		return invalidField("HOOK_INVALID", "paymentMatch.statusField", "statusField và statusValue là bắt buộc")
	}

	_, specs, err := s.rec.resolve(ctx, tenantID, pm.TargetEntity)
	if err != nil {
		var ve *ValidationError
		if errors.Is(err, ErrEntityNotFound) || errors.As(err, &ve) {
			return invalidField("HOOK_INVALID", "paymentMatch.targetEntity", "entity %q không tồn tại", pm.TargetEntity)
		}
		return fmt.Errorf("services.InboundHookService.validatePaymentMatch: resolve: %w", err)
	}
	byName := make(map[string]FieldSpec, len(specs))
	for _, f := range specs {
		byName[f.Name] = f
	}
	ref, ok := byName[pm.RefField]
	if !ok || !ref.IsUnique {
		return invalidField("HOOK_INVALID", "paymentMatch.refField", "refField phải là field unique của %q", pm.TargetEntity)
	}
	sf, ok := byName[pm.StatusField]
	if !ok {
		return invalidField("HOOK_INVALID", "paymentMatch.statusField", "field %q không có trong %q", pm.StatusField, pm.TargetEntity)
	}
	// A computed/readonly status field would pass save and then dead-letter EVERY match at
	// runtime (review W1) — the same field class WF-01 blocks for workflow field_update.
	if _, readonly := computedReadonlyCode(sf.FieldType); readonly {
		return invalidField("HOOK_INVALID", "paymentMatch.statusField", "field %q là computed/readonly — không đổi trạng thái được", pm.StatusField)
	}
	// Amount check is opt-in but all-or-nothing: half a pair silently checks nothing.
	if (pm.AmountSource == "") != (pm.AmountField == "") {
		return invalidField("HOOK_INVALID", "paymentMatch.amountField", "amountSource và amountField phải đi cùng nhau (hoặc bỏ cả hai)")
	}
	if pm.AmountField != "" {
		af, ok := byName[pm.AmountField]
		if !ok || (af.FieldType != "number" && af.FieldType != "currency") {
			return invalidField("HOOK_INVALID", "paymentMatch.amountField", "amountField phải là field number/currency của %q", pm.TargetEntity)
		}
	}
	return nil
}

// mintHookSecret returns a fresh whk_<48 hex> shared secret.
func mintHookSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("services.mintHookSecret: %w", err)
	}
	return "whk_" + hex.EncodeToString(buf), nil
}

// Create validates + persists a hook with a freshly minted secret.
func (s *InboundHookService) Create(ctx context.Context, tenantID, userID string, in InboundHookInput) (*inboundhook.Hook, error) {
	if err := s.validate(ctx, tenantID, &in); err != nil {
		return nil, err
	}
	createdBy, err := s.hookCreatedBy(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	secret, err := mintHookSecret()
	if err != nil {
		return nil, err
	}
	h := hookFromInput(tenantID, uuid.NewString(), createdBy, secret, in)
	if err := s.store.Create(ctx, h); err != nil {
		if errors.Is(err, inboundhook.ErrDuplicateName) {
			return nil, invalid("HOOK_ALREADY_EXISTS", "hook %q đã tồn tại", in.Name)
		}
		return nil, fmt.Errorf("services.InboundHookService.Create: %w", err)
	}
	// The row's actor is the request's (the builder), not createdBy: "who made this door" stays named.
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.InboundHookMeta, h.ID, audit.ActionCreate, nil, hookAuditSnapshot(h, true))
	return h, nil
}

// Update validates + full-replaces a hook's config (secret untouched).
func (s *InboundHookService) Update(ctx context.Context, tenantID, id string, in InboundHookInput) (*inboundhook.Hook, error) {
	if err := s.validate(ctx, tenantID, &in); err != nil {
		return nil, err
	}
	h := hookFromInput(tenantID, id, "", "", in)
	// Snapshot before the write (a store may hand back the very row it then mutates).
	beforeSnap := hookAuditSnapshot(priorDoc(func() (*inboundhook.Hook, error) { return s.store.GetByID(ctx, tenantID, id) }), false)
	if err := s.store.Update(ctx, tenantID, h); err != nil {
		switch {
		case errors.Is(err, inboundhook.ErrNotFound):
			return nil, invalid("HOOK_NOT_FOUND", "hook không tồn tại")
		case errors.Is(err, inboundhook.ErrDuplicateName):
			return nil, invalid("HOOK_ALREADY_EXISTS", "hook %q đã tồn tại", in.Name)
		}
		return nil, fmt.Errorf("services.InboundHookService.Update: %w", err)
	}
	updated, err := s.store.GetByID(ctx, tenantID, id)
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.InboundHookMeta, id, audit.ActionUpdate, beforeSnap, hookAuditSnapshot(updated, false))
	return updated, err
}

// RotateSecret mints + persists a new secret, returning the updated hook.
func (s *InboundHookService) RotateSecret(ctx context.Context, tenantID, id string) (*inboundhook.Hook, error) {
	secret, err := mintHookSecret()
	if err != nil {
		return nil, err
	}
	if err := s.store.UpdateSecret(ctx, tenantID, id, secret); err != nil {
		if errors.Is(err, inboundhook.ErrNotFound) {
			return nil, invalid("HOOK_NOT_FOUND", "hook không tồn tại")
		}
		return nil, fmt.Errorf("services.InboundHookService.RotateSecret: %w", err)
	}
	// One-sided on purpose: the old and the new key are both "[redacted]", so a before/after pair would
	// diff to nothing — the row says THAT the key was replaced, never what it was or is.
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.InboundHookMeta, id, audit.ActionUpdate, nil, map[string]any{"secret": auditRedacted})
	return s.store.GetByID(ctx, tenantID, id)
}

// Delete soft-deletes a hook.
func (s *InboundHookService) Delete(ctx context.Context, tenantID, id string) error {
	// Say what was deleted: the row is gone after the write.
	beforeSnap := hookAuditSnapshot(priorDoc(func() (*inboundhook.Hook, error) { return s.store.GetByID(ctx, tenantID, id) }), false)
	if err := s.store.SoftDelete(ctx, tenantID, id); err != nil {
		if errors.Is(err, inboundhook.ErrNotFound) {
			return invalid("HOOK_NOT_FOUND", "hook không tồn tại")
		}
		return fmt.Errorf("services.InboundHookService.Delete: %w", err)
	}
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.InboundHookMeta, id, audit.ActionDelete, beforeSnap, nil)
	return nil
}

// ListAdmin returns every hook of the tenant.
func (s *InboundHookService) ListAdmin(ctx context.Context, tenantID string) ([]inboundhook.Hook, error) {
	return s.store.ListAll(ctx, tenantID)
}

// ListDeliveries returns the most recent deliveries of one hook (existence-checked).
func (s *InboundHookService) ListDeliveries(ctx context.Context, tenantID, hookID string) ([]inboundhook.Delivery, error) {
	if _, err := s.store.GetByID(ctx, tenantID, hookID); err != nil {
		if errors.Is(err, inboundhook.ErrNotFound) {
			return nil, invalid("HOOK_NOT_FOUND", "hook không tồn tại")
		}
		return nil, fmt.Errorf("services.InboundHookService.ListDeliveries: %w", err)
	}
	return s.store.ListDeliveries(ctx, tenantID, hookID, 50)
}

// ReceiveResult is the outcome of one processed signal.
type ReceiveResult struct {
	Status   string `json:"status"` // created | updated
	RecordID string `json:"recordId"`
}

// Receive is the public receive path: resolve slug → load hook → verify HMAC → extract →
// upsert. Every rejection before the write is the SAME ErrHookUnauthorized (spec §5).
// Record-validation failures surface as the real *ValidationError so the sender learns why,
// and land in the delivery log either way.
func (s *InboundHookService) Receive(ctx context.Context, tenantSlug, hookName, signature string, rawBody []byte) (*ReceiveResult, error) {
	tenant, err := s.tenants.ResolveTenantBySlug(ctx, strings.ToLower(strings.TrimSpace(tenantSlug)))
	if err != nil {
		// Uniform 401 stays; a REAL infrastructure error (not not-found) is still logged so a
		// flapping DB doesn't masquerade as auth noise (security-audit note).
		if !errors.Is(err, auth.ErrTenantNotFound) {
			s.logger.Warn("services.InboundHookService.Receive: tenant resolve failed", zap.Error(err))
		}
		return nil, ErrHookUnauthorized // unknown/suspended tenant — uniform
	}
	hook, err := s.store.GetByName(ctx, tenant.ID, strings.TrimSpace(hookName))
	if err != nil {
		if !errors.Is(err, inboundhook.ErrNotFound) {
			s.logger.Warn("services.InboundHookService.Receive: hook lookup failed", zap.Error(err))
		}
		return nil, ErrHookUnauthorized // unknown hook — uniform
	}
	if !hook.IsActive || !verifyHookSignature(hook.Secret, signature, rawBody) {
		return nil, ErrHookUnauthorized // disabled or bad signature — uniform
	}
	// specs/068 D-13: the freeze is judged only AFTER the signature, so an unsigned prober learns
	// nothing about which slugs exist or are under maintenance. A read error is logged by the checker.
	if s.freeze != nil {
		if until, frozen, _ := s.freeze.FrozenUntil(ctx, tenant.ID); frozen {
			return nil, &MaintenanceError{Until: until}
		}
	}

	var payload any
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		s.logDelivery(ctx, hook, "error", "", "payload không phải JSON hợp lệ", rawBody)
		return nil, invalid("HOOK_PAYLOAD_INVALID", "payload không phải JSON hợp lệ")
	}

	fields := map[string]any{}
	for _, m := range hook.Mapping {
		if v, ok := extractPath(payload, m.Source); ok {
			fields[m.Field] = v
		}
	}
	// Coerce foreign scalars into the engine's own shapes before the write. An inbound hook is by
	// definition fed by a SYSTEM WE DO NOT CONTROL — a bank feed sends {"amount": 150000000}, not
	// Nexus's internal {amount, currencyCode} object. Without this step the very first money field
	// in any integration dies on 422 RECORD_INVALID_TYPE, which is exactly what happened the first
	// time the reconciliation loop was run end-to-end over HTTP (2026-08-02). Requiring a partner
	// to speak our internal representation is not an integration.
	if specs, serr := s.fieldSpecsFor(ctx, hook.TenantID, hook.EntityName); serr == nil {
		coerceInboundFields(fields, specs)
	} else {
		// Non-fatal: fall through with the raw values and let record validation speak. A metadata
		// read failure must not turn a deliverable signal into a hard rejection.
		s.logDeliveryDebug(ctx, hook, serr)
	}
	if len(fields) == 0 {
		s.logDelivery(ctx, hook, "error", "", "mapping không khớp trường nào trong payload", rawBody)
		return nil, invalid("HOOK_PAYLOAD_EMPTY", "mapping không khớp trường nào trong payload")
	}

	res, err := s.upsert(ctx, hook, fields)
	if err != nil {
		s.logDelivery(ctx, hook, "error", "", err.Error(), rawBody)
		return nil, scrubVetoForPartner(err)
	}

	// p2 matching (../specs/016-payment-flow/spec.md §8): strictly AFTER the successful write, strictly fail-open
	// — the transaction is recorded either way; a missed match is hộp-chờ work, not a sender
	// error. The unmatched reason rides the delivery row's error column.
	matchStatus, matchID, matchReason := s.runPaymentMatch(ctx, hook, payload)
	s.logDeliveryMatch(ctx, hook, res.Status, res.RecordID, matchReason, matchStatus, matchID, rawBody)
	return res, nil
}

// scrubVetoForPartner removes what an outside sender must not learn from a before_save veto
// (specs/048 FR-025): the write ran under the hook owner's admin scope, so refuse() kept the internal
// rule UUID and the field anchors unfiltered. The partner keeps the message, the part they can act
// on. Any other error passes through untouched; the ValidationError is copied, never mutated.
func scrubVetoForPartner(err error) error {
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "RECORD_HOOK_VETO" {
		return err
	}
	out := *ve
	out.Field = ""
	out.Details = map[string]any{}
	if vs, ok := ve.Details["violations"].([]workflow.Violation); ok {
		clean := make([]workflow.Violation, len(vs))
		for i, v := range vs {
			clean[i] = workflow.Violation{Message: v.Message}
		}
		out.Details["violations"] = clean
	}
	return &out
}

// runPaymentMatch executes the configured matching step. Returns ("", "", "") when the hook
// has none; else ("matched", targetID, "") or ("unmatched", "", reason).
func (s *InboundHookService) runPaymentMatch(ctx context.Context, hook *inboundhook.Hook, payload any) (string, string, string) {
	pm := hook.PaymentMatch
	if pm == nil {
		return "", "", ""
	}
	memoV, ok := extractPath(payload, pm.MemoSource)
	memo, isStr := memoV.(string)
	if !ok || !isStr || strings.TrimSpace(memo) == "" {
		return "unmatched", "", fmt.Sprintf("không thấy nội dung CK tại %q", pm.MemoSource)
	}
	re, err := regexp.Compile(pm.RefPattern)
	if err != nil {
		return "unmatched", "", "refPattern không compile được" // config drifted post-validate
	}
	ref := re.FindString(memo)
	if ref == "" {
		return "unmatched", "", fmt.Sprintf("memo %q không chứa mã khớp %q", truncateUTF8(memo, 80), pm.RefPattern)
	}

	scope := RecordScope{UserID: hook.CreatedBy, AccessLevel: "all", IsAdmin: true}
	lr, err := s.rec.List(ctx, scope, hook.TenantID, pm.TargetEntity, ListQuery{
		Limit:   1,
		Filters: []FilterInput{{Field: pm.RefField, Operator: dynrepo.FilterEq, Values: []string{ref}}},
	})
	if err != nil {
		// Fail-open stays, but an infrastructure error must not masquerade silently as a
		// business miss (review W3 — mirror the Receive-path warn posture).
		s.logger.Warn("services.InboundHookService.runPaymentMatch: target lookup failed",
			zap.String("tenant_id", hook.TenantID), zap.String("hook_id", hook.ID), zap.Error(err))
		return "unmatched", "", fmt.Sprintf("không tra được bản ghi %s = %q (lỗi hệ thống)", pm.RefField, ref)
	}
	if len(lr.Records) != 1 {
		return "unmatched", "", fmt.Sprintf("không có bản ghi %s = %q", pm.RefField, ref)
	}
	target := lr.Records[0]

	// Double-payment guard (review S1): a second credit for an ALREADY-settled document is a
	// human problem (refund/overpay), not a re-match — hộp chờ, and no duplicate workflow fire.
	if cur, ok := target.Data[pm.StatusField]; ok && fmt.Sprintf("%v", cur) == pm.StatusValue {
		return "unmatched", "", fmt.Sprintf("bản ghi %s = %q đã ở trạng thái %q từ trước (khả năng thu trùng)", pm.RefField, ref, pm.StatusValue)
	}

	if pm.AmountField != "" {
		paidV, ok := extractPath(payload, pm.AmountSource)
		paid, pOK := toFloat(paidV)
		if !ok || !pOK {
			return "unmatched", "", fmt.Sprintf("không đọc được số tiền CK tại %q", pm.AmountSource)
		}
		wantV := target.Data[pm.AmountField]
		if m, isMap := wantV.(map[string]any); isMap {
			wantV = m["amount"]
		}
		want, wOK := toFloat(wantV)
		if !wOK || paid != want {
			return "unmatched", "", fmt.Sprintf("số tiền lệch: CK %v ≠ chứng từ %v", paid, wantV)
		}
	}

	// specs/048 FR-019: the money has arrived — a before_save veto must not leave a paid document
	// marked unpaid. Only this status write is exempt; the hook's own upsert is judged like any write.
	if err := s.rec.Update(withSystemWriteOrigin(ctx, originPaymentMatch), scope, hook.TenantID, pm.TargetEntity, target.ID,
		map[string]any{pm.StatusField: pm.StatusValue}, nil); err != nil {
		return "unmatched", "", truncateUTF8("cập nhật trạng thái thất bại: "+err.Error(), 480)
	}
	return "matched", target.ID, ""
}

// upsert writes the extracted fields through RecordService under the hook owner's identity
// (admin-configured integration → admin scope; audit trail points at hook.CreatedBy).
func (s *InboundHookService) upsert(ctx context.Context, hook *inboundhook.Hook, fields map[string]any) (*ReceiveResult, error) {
	scope := RecordScope{UserID: hook.CreatedBy, AccessLevel: "all", IsAdmin: true}

	matched, res, err := s.tryMatchUpdate(ctx, scope, hook, fields)
	if err != nil {
		return nil, err
	}
	if matched {
		return res, nil
	}

	id, err := s.rec.Create(ctx, scope, hook.TenantID, hook.CreatedBy, hook.EntityName, fields)
	if err != nil {
		// Two concurrent signals with the same fresh match value can both miss the lookup;
		// the loser hits the unique index. Re-match once so the sender sees "updated" instead
		// of a spurious duplicate error (review S1).
		var ve *ValidationError
		if errors.As(err, &ve) && ve.Code == "RECORD_DUPLICATE" {
			if matched, res, merr := s.tryMatchUpdate(ctx, scope, hook, fields); merr == nil && matched {
				return res, nil
			}
		}
		return nil, err
	}
	return &ReceiveResult{Status: "created", RecordID: id}, nil
}

// tryMatchUpdate looks the match value up and updates in place when found. matched=false means
// "no usable match value / no existing record" → the caller creates.
func (s *InboundHookService) tryMatchUpdate(ctx context.Context, scope RecordScope, hook *inboundhook.Hook, fields map[string]any) (bool, *ReceiveResult, error) {
	if hook.MatchField == nil || *hook.MatchField == "" {
		return false, nil, nil
	}
	v, has := fields[*hook.MatchField]
	if !has {
		return false, nil, nil
	}
	val, ok := matchValueString(v)
	if !ok {
		return false, nil, nil // bool/null/object make no unique key — fall through to create
	}
	lr, err := s.rec.List(ctx, scope, hook.TenantID, hook.EntityName, ListQuery{
		Limit:   1,
		Filters: []FilterInput{{Field: *hook.MatchField, Operator: dynrepo.FilterEq, Values: []string{val}}},
	})
	if err != nil {
		return false, nil, fmt.Errorf("services.InboundHookService.upsert: match lookup: %w", err)
	}
	if len(lr.Records) != 1 || lr.Records[0].ID == "" {
		return false, nil, nil
	}
	id := lr.Records[0].ID
	if err := s.rec.Update(ctx, scope, hook.TenantID, hook.EntityName, id, fields, nil); err != nil {
		return false, nil, err
	}
	return true, &ReceiveResult{Status: "updated", RecordID: id}, nil
}

// matchValueString renders a JSON-decoded match value as the filter operand. Explicit per type
// (review W1): %v on a float64 gives scientific notation ("1.5e+06") whose eq-match only
// survived by MySQL coercion — format decimals plainly instead. Non-scalar values carry no
// usable unique key.
func matchValueString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, t != ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	default:
		return "", false
	}
}

// logDelivery appends a delivery row — fail-open (a log failure never fails the request),
// never silent (warned).
func (s *InboundHookService) logDelivery(ctx context.Context, hook *inboundhook.Hook, status, recordID, errMsg string, rawBody []byte) {
	s.logDeliveryMatch(ctx, hook, status, recordID, errMsg, "", "", rawBody)
}

// logDeliveryMatch is logDelivery + the p2 match outcome columns (matchStatus/matchRecordID
// empty → NULL). The unmatched reason travels in errMsg alongside a successful write status.
func (s *InboundHookService) logDeliveryMatch(ctx context.Context, hook *inboundhook.Hook, status, recordID, errMsg, matchStatus, matchRecordID string, rawBody []byte) {
	head := truncateUTF8(string(rawBody), 2048)
	d := &inboundhook.Delivery{
		ID: uuid.NewString(), TenantID: hook.TenantID, HookID: hook.ID, Status: status,
	}
	if recordID != "" {
		d.RecordID = &recordID
	}
	if errMsg != "" {
		errMsg = truncateUTF8(errMsg, 500)
		d.Error = &errMsg
	}
	if head != "" {
		d.PayloadHead = &head
	}
	if matchStatus != "" {
		d.MatchStatus = &matchStatus
	}
	if matchRecordID != "" {
		d.MatchRecordID = &matchRecordID
	}
	if err := s.store.RecordDelivery(ctx, d); err != nil {
		s.logger.Warn("services.InboundHookService: delivery log write failed",
			zap.String("tenant_id", hook.TenantID), zap.String("hook_id", hook.ID), zap.Error(err))
	}
}

// truncateUTF8 cuts s to at most max BYTES without splitting a rune (review S2 — a mid-rune
// cut makes invalid UTF-8 that strict MySQL rejects, silently losing the log row).
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// verifyHookSignature checks sig == hex(hmac-sha256(secret, body)) in constant time.
func verifyHookSignature(secret, signature string, body []byte) bool {
	sig, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil || len(sig) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}

// extractPath walks a decoded JSON value by dot-path ("data.amount", "items.0.sku"; a numeric
// segment indexes an array). Returns (value, true) only when the full path resolves.
func extractPath(v any, path string) (any, bool) {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			nv, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = nv
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// hookFromInput maps the input onto a repository row.
func hookFromInput(tenantID, id, createdBy, secret string, in InboundHookInput) *inboundhook.Hook {
	h := &inboundhook.Hook{
		ID: id, TenantID: tenantID, Name: in.Name, EntityName: in.EntityName,
		Secret: secret, Mapping: in.Mapping, PaymentMatch: in.PaymentMatch,
		CreatedBy: createdBy, IsActive: true,
	}
	if in.MatchField != "" {
		mf := in.MatchField
		h.MatchField = &mf
	}
	if in.IsActive != nil {
		h.IsActive = *in.IsActive
	}
	return h
}

// fieldSpecsFor resolves an entity's field specs for the inbound coercion step.
func (s *InboundHookService) fieldSpecsFor(ctx context.Context, tenantID, entityName string) ([]FieldSpec, error) {
	_, specs, err := s.rec.resolve(ctx, tenantID, entityName)
	if err != nil {
		return nil, fmt.Errorf("services.InboundHookService.fieldSpecsFor: %w", err)
	}
	return specs, nil
}

// logDeliveryDebug records a non-fatal coercion-path problem without failing the delivery.
func (s *InboundHookService) logDeliveryDebug(_ context.Context, hook *inboundhook.Hook, err error) {
	if s.logger == nil {
		return
	}
	s.logger.Debug("services.InboundHookService: field specs unavailable, skipping coercion",
		zap.String("tenant_id", hook.TenantID), zap.String("hook", hook.Name), zap.Error(err))
}

// coerceInboundFields rewrites mapped values into the shapes the record engine expects, IN PLACE.
//
// Only currency is handled, and only from a scalar: that is the one place where the engine's
// internal representation ({amount, currencyCode}) differs from what every external system sends
// (a plain number). Everything else the engine already coerces itself (numbers from strings,
// dates from ISO strings), so widening this would duplicate validation logic in a second place —
// the drift risk this codebase keeps paying for.
//
// A value that is ALREADY an object is left untouched: a partner sophisticated enough to send the
// full shape is not second-guessed. An unparseable scalar is also left alone, so the record
// validator produces the honest per-field error instead of this layer inventing a zero.
func coerceInboundFields(fields map[string]any, specs []FieldSpec) {
	byName := make(map[string]FieldSpec, len(specs))
	for _, f := range specs {
		byName[f.Name] = f
	}
	for name, raw := range fields {
		spec, ok := byName[name]
		if !ok || spec.FieldType != "currency" {
			continue
		}
		if _, isObj := raw.(map[string]any); isObj {
			continue
		}
		// toFloat is the one already used by the payment service (tenant_payment_service.go) —
		// same money-parsing rules, including refusing partial parses like "123abc".
		amount, ok := toFloat(raw)
		if !ok {
			continue
		}
		code := "VND"
		if spec.Options != nil {
			if c, ok := spec.Options["currencyCode"].(string); ok && strings.TrimSpace(c) != "" {
				code = c
			}
		}
		fields[name] = map[string]any{"amount": amount, "currencyCode": code}
	}
}
