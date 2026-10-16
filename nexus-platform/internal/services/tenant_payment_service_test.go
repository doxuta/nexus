// Bank-config validator tests (DONG-TIEN-KHEP-KIN p1, ../specs/016-payment-flow/spec.md §2/§7). A
// wrong BIN or account number here does not fail loudly later — it mints a QR that sends the
// customer's money to the wrong account — so every reject asserts the CODE and the offending FIELD,
// not just err != nil (bankBin is checked first and would otherwise mask a broken later check).
package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
)

func TestValidatePayment(t *testing.T) {
	name100 := strings.Repeat("Đ", 100) // 100 runes, 200 bytes: the limit counts runes
	cases := []struct {
		name      string
		in        PaymentConfig
		wantField string        // "" = accepted
		want      PaymentConfig // checked only when accepted (trim happens in place)
	}{
		{name: "all_empty_clears_config", in: PaymentConfig{}},
		{name: "whitespace_only_clears_config", in: PaymentConfig{BankBin: "  ", AccountNumber: "\t", AccountName: " "}},
		{
			name: "valid_padded_is_trimmed_in_place",
			in:   PaymentConfig{BankBin: " 970436 ", AccountNumber: " 0011001234567\n", AccountName: "  CONG TY ACME  "},
			want: PaymentConfig{BankBin: "970436", AccountNumber: "0011001234567", AccountName: "CONG TY ACME"},
		},
		{
			// PINS CURRENT BEHAVIOUR (queue #53 correction B): the validator only caps the name
			// at 100 runes, so an empty recipient name is accepted. Tightening that is a product
			// decision, not something a test-only slice may smuggle in.
			name: "empty_account_name_accepted_current_behaviour",
			in:   PaymentConfig{BankBin: "970436", AccountNumber: "0011001234567"},
			want: PaymentConfig{BankBin: "970436", AccountNumber: "0011001234567"},
		},
		{name: "bin_five_digits", in: PaymentConfig{BankBin: "97043", AccountNumber: "0011001234567"}, wantField: "bankBin"},
		{name: "bin_seven_digits", in: PaymentConfig{BankBin: "9704361", AccountNumber: "0011001234567"}, wantField: "bankBin"},
		{name: "bin_non_digit", in: PaymentConfig{BankBin: "97043A", AccountNumber: "0011001234567"}, wantField: "bankBin"},
		{name: "bin_fullwidth_digits", in: PaymentConfig{BankBin: "９７０４３６", AccountNumber: "0011001234567"}, wantField: "bankBin"},
		{name: "name_only_is_not_a_clear", in: PaymentConfig{AccountName: "CONG TY ACME"}, wantField: "bankBin"},
		{name: "account_empty", in: PaymentConfig{BankBin: "970436"}, wantField: "accountNumber"},
		{
			name: "account_19_chars_accepted",
			in:   PaymentConfig{BankBin: "970436", AccountNumber: "ABC0123456789012345"},
			want: PaymentConfig{BankBin: "970436", AccountNumber: "ABC0123456789012345"},
		},
		{name: "account_20_chars", in: PaymentConfig{BankBin: "970436", AccountNumber: "ABC01234567890123456"}, wantField: "accountNumber"},
		{name: "account_with_hyphen", in: PaymentConfig{BankBin: "970436", AccountNumber: "0011-001"}, wantField: "accountNumber"},
		{name: "account_inner_space", in: PaymentConfig{BankBin: "970436", AccountNumber: "0011 001"}, wantField: "accountNumber"},
		{
			name: "name_100_runes_accepted",
			in:   PaymentConfig{BankBin: "970436", AccountNumber: "0011001234567", AccountName: name100},
			want: PaymentConfig{BankBin: "970436", AccountNumber: "0011001234567", AccountName: name100},
		},
		{name: "name_101_runes", in: PaymentConfig{BankBin: "970436", AccountNumber: "0011001234567", AccountName: name100 + "A"}, wantField: "accountName"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.in
			err := ValidatePayment(&p)
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("ValidatePayment(%+v) = %v, want nil", tc.in, err)
				}
				if p != tc.want {
					t.Fatalf("after ValidatePayment got %+v, want %+v (trim in place)", p, tc.want)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("ValidatePayment(%+v) = %v, want *ValidationError on %s", tc.in, err, tc.wantField)
			}
			if ve.Code != "PAYMENT_INVALID" || ve.Details["field"] != tc.wantField {
				t.Fatalf("got code=%q field=%v, want PAYMENT_INVALID on %s", ve.Code, ve.Details["field"], tc.wantField)
			}
		})
	}
}

// paymentStoreFake records whether Update reached the persistence layer.
type paymentStoreFake struct {
	writes int
	raw    json.RawMessage
}

func (f *paymentStoreFake) GetTenantByID(context.Context, string) (*auth.Tenant, error) {
	return &auth.Tenant{Payment: f.raw}, nil
}

func (f *paymentStoreFake) UpdateTenantPayment(_ context.Context, _ string, raw json.RawMessage) error {
	f.writes++
	f.raw = raw
	return nil
}

// An invalid document must be refused BEFORE the store is touched: a half-validated bank account
// that reached the tenants row would be served on every QR minted afterwards.
func TestTenantPaymentService_UpdateRejectsInvalidWithoutWriting(t *testing.T) {
	store := &paymentStoreFake{}
	svc := NewTenantPaymentService(store, nil)

	if _, err := svc.Update(context.Background(), "t1", PaymentConfig{BankBin: "97043", AccountNumber: "0011001234567"}); err == nil {
		t.Fatal("Update with a 5-digit BIN succeeded, want PAYMENT_INVALID")
	}
	if store.writes != 0 {
		t.Fatalf("invalid config reached the store (%d writes)", store.writes)
	}

	got, err := svc.Update(context.Background(), "t1", PaymentConfig{BankBin: " 970436 ", AccountNumber: "0011001234567"})
	if err != nil {
		t.Fatalf("valid Update: %v", err)
	}
	if store.writes != 1 || !strings.Contains(string(store.raw), `"bankBin":"970436"`) {
		t.Fatalf("valid config not persisted trimmed: writes=%d raw=%s", store.writes, store.raw)
	}
	if got.BankBin != "970436" {
		t.Fatalf("Update returned %+v, want trimmed bankBin", got)
	}

	// {} clears: persisted as SQL NULL (nil raw), not the string "{}".
	if _, err := svc.Update(context.Background(), "t1", PaymentConfig{}); err != nil {
		t.Fatalf("clear Update: %v", err)
	}
	if store.writes != 2 || store.raw != nil {
		t.Fatalf("clear must write NULL: writes=%d raw=%q", store.writes, store.raw)
	}
}
