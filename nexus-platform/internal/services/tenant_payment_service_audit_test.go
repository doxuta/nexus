package services

// specs/062-customer-workspace L4 T064b — changing the bank account the payment QR pays into leaves a
// __payment_config__ audit row with the full before/after of bankBin / accountNumber / accountName,
// including the first configuration and clearing it ({}). A refused change leaves nothing.

import (
	"context"
	"testing"

	"nexus/platform/internal/audit"
)

func TestTenantPaymentService_UpdateEmitsPaymentConfigAudit(t *testing.T) {
	store := &paymentStoreFake{}
	aud := &captureAuditor{}
	svc := NewTenantPaymentService(store, nil).WithAuditor(aud)
	ctx := audit.WithActor(context.Background(), "builder-1")

	first := PaymentConfig{BankBin: "970436", AccountNumber: "0011001234567", AccountName: "CONG TY ACME"}
	second := PaymentConfig{BankBin: "970422", AccountNumber: "9999888877", AccountName: "CONG TY ACME"}
	for _, p := range []PaymentConfig{first, second, {}} { // configure, redirect the money, clear
		if _, err := svc.Update(ctx, "tenant-1", p); err != nil {
			t.Fatalf("Update(%+v): %v", p, err)
		}
		// paymentStoreFake keeps what was written, so the next Update reads it back as the prior config
	}

	if len(aud.events) != 3 {
		t.Fatalf("events = %d; want 3 (configure, change, clear): %+v", len(aud.events), aud.events)
	}
	for i, e := range aud.events {
		if e.EntityName != audit.PaymentConfigMeta || e.Action != audit.ActionUpdate || e.TenantID != "tenant-1" || e.ActorUserID != "builder-1" || e.RecordID != "tenant-1" {
			t.Errorf("event %d = %+v; want %s/update by builder-1 on tenant-1", i, e, audit.PaymentConfigMeta)
		}
		if e.Diff == nil {
			t.Fatalf("event %d has no diff", i)
		}
	}
	if d := aud.events[0].Diff; d.Before != nil || d.After["accountNumber"] != "0011001234567" || d.After["bankBin"] != "970436" {
		t.Errorf("first-config diff = %+v; want after-only with the account", d)
	}
	if d := aud.events[1].Diff; d.Before["accountNumber"] != "0011001234567" || d.After["accountNumber"] != "9999888877" || d.After["bankBin"] != "970422" {
		t.Errorf("redirect diff = %+v; want old and new account", d)
	}
	if d := aud.events[2].Diff; d.After != nil || d.Before["accountNumber"] != "9999888877" {
		t.Errorf("clear diff = %+v; want before-only with the account that was removed", d)
	}
}

func TestTenantPaymentService_RefusedUpdateEmitsNothing(t *testing.T) {
	aud := &captureAuditor{}
	svc := NewTenantPaymentService(&paymentStoreFake{}, nil).WithAuditor(aud)
	if _, err := svc.Update(context.Background(), "tenant-1", PaymentConfig{BankBin: "97043", AccountNumber: "0011001234567"}); err == nil {
		t.Fatal("want PAYMENT_INVALID")
	}
	if len(aud.events) != 0 {
		t.Fatalf("events after a refused update = %d; want 0", len(aud.events))
	}
}
