// DONG-TIEN-KHEP-KIN p1 (../specs/016-payment-flow/spec.md): per-tenant VietQR bank config + the record
// payment-QR resolver. Mirrors the branding service pattern (small JSON document on tenants,
// admin-only write scoped to the JWT-claim tenant); the QR endpoint itself reads the record
// under the CALLER's scope so ACL/row-scope decide who can mint a QR for what.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// PaymentConfig is the parsed bank document. Zero value = not configured.
type PaymentConfig struct {
	BankBin       string `json:"bankBin,omitempty"`
	AccountNumber string `json:"accountNumber,omitempty"`
	AccountName   string `json:"accountName,omitempty"`
}

// paymentTenantStore is the narrow repository slice (auth.UserRepository).
type paymentTenantStore interface {
	GetTenantByID(ctx context.Context, id string) (*auth.Tenant, error)
	UpdateTenantPayment(ctx context.Context, tenantID string, raw json.RawMessage) error
}

// TenantPaymentService validates/persists the bank config and resolves record payment QRs.
type TenantPaymentService struct {
	store paymentTenantStore
	rec   *RecordService
	// auditor receives one __payment_config__ row per committed Update (specs/062 L4): where the
	// customer's money flows must be answerable. Never drafted — written straight (FR-023).
	auditor audit.Auditor
}

func NewTenantPaymentService(store paymentTenantStore, rec *RecordService) *TenantPaymentService {
	return &TenantPaymentService{store: store, rec: rec, auditor: audit.NewNoopAuditor()}
}

// WithAuditor injects the audit sink for bank-config writes. Chainable; a nil auditor keeps the noop.
func (s *TenantPaymentService) WithAuditor(a audit.Auditor) *TenantPaymentService {
	if a != nil {
		s.auditor = a
	}
	return s
}

var (
	paymentBinRe     = regexp.MustCompile(`^[0-9]{6}$`)
	paymentAccountRe = regexp.MustCompile(`^[0-9A-Za-z]{1,19}$`)
	memoTokenRe      = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)
)

// ValidatePayment trims + validates the document in place (422 PAYMENT_INVALID).
func ValidatePayment(p *PaymentConfig) error {
	p.BankBin = strings.TrimSpace(p.BankBin)
	p.AccountNumber = strings.TrimSpace(p.AccountNumber)
	p.AccountName = strings.TrimSpace(p.AccountName)

	empty := p.BankBin == "" && p.AccountNumber == "" && p.AccountName == ""
	if empty {
		return nil // {} = clear config
	}
	if !paymentBinRe.MatchString(p.BankBin) {
		return invalidField("PAYMENT_INVALID", "bankBin", "bankBin phải là 6 chữ số (mã BIN NAPAS của ngân hàng)")
	}
	if !paymentAccountRe.MatchString(p.AccountNumber) {
		return invalidField("PAYMENT_INVALID", "accountNumber", "accountNumber 1-19 ký tự chữ/số")
	}
	if utf8.RuneCountInString(p.AccountName) > 100 {
		return invalidField("PAYMENT_INVALID", "accountName", "accountName tối đa 100 ký tự")
	}
	return nil
}

// Get returns the tenant's bank config (empty when unset).
func (s *TenantPaymentService) Get(ctx context.Context, tenantID string) (PaymentConfig, error) {
	t, err := s.store.GetTenantByID(ctx, tenantID)
	if err != nil {
		return PaymentConfig{}, fmt.Errorf("services.TenantPaymentService.Get: %w", err)
	}
	var p PaymentConfig
	if len(t.Payment) > 0 {
		_ = json.Unmarshal(t.Payment, &p) // malformed row reads as unconfigured (fail-safe)
	}
	return p, nil
}

// Update validates + full-replaces the config ({} clears it).
func (s *TenantPaymentService) Update(ctx context.Context, tenantID string, p PaymentConfig) (PaymentConfig, error) {
	if err := ValidatePayment(&p); err != nil {
		return PaymentConfig{}, err
	}
	var raw json.RawMessage
	if p != (PaymentConfig{}) {
		enc, err := json.Marshal(p)
		if err != nil {
			return PaymentConfig{}, fmt.Errorf("services.TenantPaymentService.Update: marshal: %w", err)
		}
		raw = enc
	}
	// The prior config, read before the write so the row carries both sides (best-effort: an
	// unreadable tenant makes the write below fail on its own terms).
	var before any
	if t := priorDoc(func() (*auth.Tenant, error) { return s.store.GetTenantByID(ctx, tenantID) }); t != nil {
		var prev PaymentConfig
		if len(t.Payment) > 0 {
			if err := json.Unmarshal(t.Payment, &prev); err != nil {
				prev = PaymentConfig{} // a malformed row reads as unconfigured, like Get
			}
		}
		before = prev
	}
	if err := s.store.UpdateTenantPayment(ctx, tenantID, raw); err != nil {
		if errors.Is(err, auth.ErrTenantNotFound) {
			return PaymentConfig{}, invalid("TENANT_NOT_FOUND", "tenant không tồn tại")
		}
		return PaymentConfig{}, fmt.Errorf("services.TenantPaymentService.Update: %w", err)
	}
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.PaymentConfigMeta, tenantID, audit.ActionUpdate, before, p)
	return p, nil
}

// PaymentQR is the resolved QR answer for one record.
type PaymentQR struct {
	Payload string        `json:"payload"`
	Amount  int64         `json:"amount"`
	Memo    string        `json:"memo"`
	Bank    PaymentConfig `json:"bank"`
}

// RecordQR resolves a record's payment QR under the CALLER's scope: read the record (ACL +
// row-scope enforced by RecordService.Get), pull the amount from amountField, expand the memo
// template's {{field}} tokens, and build the NAPAS payload.
func (s *TenantPaymentService) RecordQR(ctx context.Context, scope RecordScope, tenantID, entityName, recordID, amountField, memoTemplate string) (*PaymentQR, error) {
	cfg, err := s.Get(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if cfg.BankBin == "" || cfg.AccountNumber == "" {
		return nil, invalid("PAYMENT_NOT_CONFIGURED", "tenant chưa cấu hình tài khoản nhận (Studio → Thương hiệu → Thanh toán)")
	}

	rec, err := s.rec.Get(ctx, scope, tenantID, entityName, recordID, nil, nil)
	if err != nil {
		return nil, err // real 404/403 semantics from the record engine
	}

	amount, err := paymentAmountOf(rec.Data, strings.TrimSpace(amountField))
	if err != nil {
		return nil, err
	}

	memo := sanitizeVietQRMemo(expandMemoTemplate(strings.TrimSpace(memoTemplate), rec.Data, recordID))
	return &PaymentQR{
		Payload: BuildVietQRPayload(cfg.BankBin, cfg.AccountNumber, amount, memo),
		Amount:  amount,
		Memo:    memo,
		Bank:    cfg,
	}, nil
}

// paymentAmountOf extracts a whole-VND amount from a record field: plain number, or a currency
// composite's "amount" member. VND has no minor unit — a fractional value is a config error,
// not something to round silently.
func paymentAmountOf(data map[string]any, field string) (int64, error) {
	if field == "" {
		return 0, invalidField("PAYMENT_AMOUNT_INVALID", "amountField", "amountField là bắt buộc")
	}
	v, ok := data[field]
	if !ok || v == nil {
		return 0, invalidField("PAYMENT_AMOUNT_INVALID", field, "record không có giá trị ở field %q", field)
	}
	if m, isMap := v.(map[string]any); isMap {
		v = m["amount"] // currency composite {amount, currencyCode}
	}
	f, isNum := toFloat(v)
	if !isNum {
		return 0, invalidField("PAYMENT_AMOUNT_INVALID", field, "field %q không phải số", field)
	}
	if f <= 0 || f != math.Trunc(f) || f > 1e13 {
		return 0, invalidField("PAYMENT_AMOUNT_INVALID", field, "số tiền phải là số nguyên VND dương (nhận %v)", f)
	}
	return int64(f), nil
}

// toFloat coerces the JSON/database scalar shapes a numeric field can arrive in.
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		// strconv, not Sscanf: partial parses ("123abc" → 123) must fail, silently-wrong
		// money being the one bug class this endpoint cannot afford (review S1).
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// expandMemoTemplate replaces {{field}} tokens with the record's stringified values ("" for
// absent); "{{id}}" resolves to the record id's first 8 chars (human-checkable reference).
func expandMemoTemplate(tpl string, data map[string]any, recordID string) string {
	if tpl == "" {
		return ""
	}
	return memoTokenRe.ReplaceAllStringFunc(tpl, func(tok string) string {
		name := memoTokenRe.FindStringSubmatch(tok)[1]
		if name == "id" {
			if len(recordID) > 8 {
				return recordID[:8]
			}
			return recordID
		}
		v, ok := data[name]
		if !ok || v == nil {
			return ""
		}
		if f, isNum := toFloat(v); isNum && f == math.Trunc(f) {
			return fmt.Sprintf("%d", int64(f))
		}
		return fmt.Sprintf("%v", v)
	})
}
