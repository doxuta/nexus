package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/audit"
)

// SSO-ADMIN-AUDIT — provider create / policy-flip / delete must land one RM-19 audit row each,
// attributed to the acting admin, and the client secret must NEVER appear in any diff.

func ssoAuditCtx() context.Context {
	return audit.WithActor(context.Background(), "u-admin")
}

func ptrBool(b bool) *bool { return &b }

func TestSSOCreateProvider_EmitsAudit(t *testing.T) {
	f := newSSOFixture(t)
	aud := &captureAuditor{}
	f.svc.WithAuditor(aud)

	p, err := f.svc.CreateProvider(ssoAuditCtx(), f.tenantID, CreateSSOProviderInput{
		Issuer: "https://idp.example.com", ClientID: "client-1", ClientSecret: "pw", DisplayName: "Okta",
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if len(aud.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(aud.events))
	}
	e := aud.events[0]
	if e.Action != audit.ActionCreate || e.EntityName != audit.SSOProviderMeta {
		t.Fatalf("wrong event action/entity: %q/%q", e.Action, e.EntityName)
	}
	if e.TenantID != f.tenantID || e.ActorUserID != "u-admin" {
		t.Fatalf("wrong tenant/actor: %q/%q", e.TenantID, e.ActorUserID)
	}
	if e.Diff == nil || e.Diff.After["issuer"] != p.Issuer || e.Diff.After["clientId"] != "client-1" {
		t.Fatalf("create diff missing safe fields: %+v", e.Diff)
	}
	if e.Diff.Before != nil && len(e.Diff.Before) != 0 {
		t.Fatalf("create diff should have no before: %+v", e.Diff.Before)
	}
}

func TestSSOUpdateProviderPolicy_EmitsAudit(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false) // enabled=true, trustIdpMfa=false
	aud := &captureAuditor{}
	f.svc.WithAuditor(aud)

	if _, err := f.svc.UpdateProviderPolicy(ssoAuditCtx(), f.tenantID, p.ID, ptrBool(false), ptrBool(true)); err != nil {
		t.Fatalf("UpdateProviderPolicy: %v", err)
	}
	if len(aud.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(aud.events))
	}
	e := aud.events[0]
	if e.Action != audit.ActionUpdate || e.EntityName != audit.SSOProviderMeta {
		t.Fatalf("wrong action/entity: %q/%q", e.Action, e.EntityName)
	}
	// Only the two flipped flags (plus never-secret) should show in the diff.
	if e.Diff == nil || e.Diff.Before["enabled"] != true || e.Diff.After["enabled"] != false {
		t.Fatalf("enabled flip not in diff: %+v", e.Diff)
	}
	if e.Diff.Before["trustIdpMfa"] != false || e.Diff.After["trustIdpMfa"] != true {
		t.Fatalf("trustIdpMfa flip not in diff: %+v", e.Diff)
	}
}

func TestSSOUpdateProviderPolicy_BothNil_NoAudit(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	aud := &captureAuditor{}
	f.svc.WithAuditor(aud)

	if _, err := f.svc.UpdateProviderPolicy(ssoAuditCtx(), f.tenantID, p.ID, nil, nil); err != nil {
		t.Fatalf("UpdateProviderPolicy(both-nil): %v", err)
	}
	if len(aud.events) != 0 {
		t.Fatalf("both-nil update is a read — want 0 audit events, got %d", len(aud.events))
	}
}

func TestSSODeleteProvider_EmitsAudit(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	aud := &captureAuditor{}
	f.svc.WithAuditor(aud)

	if err := f.svc.DeleteProvider(ssoAuditCtx(), f.tenantID, p.ID); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if len(aud.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(aud.events))
	}
	e := aud.events[0]
	if e.Action != audit.ActionDelete || e.EntityName != audit.SSOProviderMeta {
		t.Fatalf("wrong action/entity: %q/%q", e.Action, e.EntityName)
	}
	if e.Diff == nil || e.Diff.Before["issuer"] != p.Issuer {
		t.Fatalf("delete before-diff missing issuer: %+v", e.Diff)
	}
	if len(e.Diff.After) != 0 {
		t.Fatalf("delete diff should have no after: %+v", e.Diff.After)
	}
}

// The one that matters: the raw client secret must never surface in an emitted audit diff.
// fakeSealer embeds the plaintext inside the ciphertext, so a single sentinel assertion covers
// BOTH the plaintext and the sealed ClientSecretEnc — if json:"-" were ever removed, this fails.
func TestSSOAudit_SecretNeverInDiff(t *testing.T) {
	const sentinel = "SUPER-SEKRET-SENTINEL-9f3a"
	f := newSSOFixture(t)
	aud := &captureAuditor{}
	f.svc.WithAuditor(aud)
	ctx := ssoAuditCtx()

	p, err := f.svc.CreateProvider(ctx, f.tenantID, CreateSSOProviderInput{
		Issuer: "https://idp.example.com", ClientID: "client-1", ClientSecret: sentinel,
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := f.svc.UpdateProviderPolicy(ctx, f.tenantID, p.ID, ptrBool(false), nil); err != nil {
		t.Fatalf("UpdateProviderPolicy: %v", err)
	}
	if err := f.svc.DeleteProvider(ctx, f.tenantID, p.ID); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}

	if len(aud.events) != 3 {
		t.Fatalf("want 3 audit events (create/update/delete), got %d", len(aud.events))
	}
	for i, e := range aud.events {
		blob, err := json.Marshal(e.Diff)
		if err != nil {
			t.Fatalf("marshal diff[%d]: %v", i, err)
		}
		if strings.Contains(string(blob), sentinel) {
			t.Fatalf("SECRET LEAK: event %d diff contains client secret: %s", i, blob)
		}
	}
}

// Without WithAuditor the service records to the noop default — no panic, no events lost track.
func TestSSOAudit_NoopDefaultSafe(t *testing.T) {
	f := newSSOFixture(t) // fixture does NOT wire a capturing auditor
	if _, err := f.svc.CreateProvider(ssoAuditCtx(), f.tenantID, CreateSSOProviderInput{
		Issuer: "https://idp.example.com", ClientID: "client-1", ClientSecret: "pw",
	}); err != nil {
		t.Fatalf("CreateProvider with noop auditor: %v", err)
	}
}
