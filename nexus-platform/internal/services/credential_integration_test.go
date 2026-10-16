//go:build integration

// ../specs/050-named-credentials S1b (tasks T022) — CredentialService over REAL MySQL: a key a live
// rule still points at cannot be deleted (409 CREDENTIAL_IN_USE listing the rule); once the rule
// is gone the delete tombstones the row and shreds every envelope in the same statement. The rule
// is a raw SQL fixture — the workflow API only learns credentialId in S2b, but usages read the
// actions JSON directly (JSON_TABLE), so no Go struct is needed. Run with:
//
//	go test -tags=integration ./internal/services/ -run TestCredentialService -race
package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/credential"
)

func TestCredentialService_DeleteInUse409(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	svc := NewCredentialService(credential.NewRepository(gdb), testKeyring(t), false)
	ctx := context.Background()
	tenant, other := uuid.NewString(), uuid.NewString()

	v, _, err := svc.Create(ctx, tenant, CredentialInput{
		Name: "pay", Label: "Pay", BaseURL: "https://pay.example.vn", AuthType: "hmac", Secret: strPtr(strongKey),
		ExtraHeaders: map[string]string{"Authorization": "Bearer x"}, ActorID: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rule := func(tenantID, name string, active bool) string {
		id := uuid.NewString()
		asgExec(t, sdb, `INSERT INTO sys_workflows (id, tenant_id, name, entity_name, trigger_event, actions, is_active)
			VALUES (?, ?, ?, 'crm_contacts', 'after_create', ?, ?)`,
			id, tenantID, name, `[{"type":"email_alert","to":"a@b.vn"},{"type":"outbound_webhook","credentialId":"`+v.ID+`","path":"/orders"}]`, active)
		return id
	}
	wfID := rule(tenant, "Báo đơn cho đối tác", false) // disabled still counts: it can be re-enabled
	rule(other, "Rule of another tenant", true)        // same id in another tenant must not count

	err = svc.Delete(ctx, tenant, v.ID, "u-1")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "CREDENTIAL_IN_USE" {
		t.Fatalf("delete in-use = %v; want CREDENTIAL_IN_USE", err)
	}
	us, _ := ve.Details["usages"].([]credential.Usage)
	if len(us) != 1 || us[0].WorkflowID != wfID || us[0].ActionIndex != 1 {
		t.Fatalf("details.usages = %#v; want exactly this tenant's rule at action 1", ve.Details["usages"])
	}
	list, err := svc.List(ctx, tenant)
	if err != nil || len(list) != 1 || list[0].UsageCount != 1 {
		t.Fatalf("list usageCount = %+v err=%v; want 1", list, err)
	}
	// A label-only PUT keeps every other column: the repository rewrites all five metadata columns,
	// so the service must load-then-apply (S1a review LOW 4).
	up, err := svc.Update(ctx, tenant, v.ID, CredentialUpdate{Label: strPtr("Pay 2"), ActorID: "u-2"})
	if err != nil || up.Label != "Pay 2" || up.BaseURL != "https://pay.example.vn" || up.UsageCount != 1 || !up.UpdatedAt.After(v.CreatedAt.Add(-time.Second)) {
		t.Fatalf("label-only update = %+v err=%v", up, err)
	}

	asgExec(t, sdb, `UPDATE sys_workflows SET deleted_at = NOW(3) WHERE tenant_id = ? AND id = ?`, tenant, wfID)
	if err := svc.Delete(ctx, tenant, v.ID, "u-1"); err != nil {
		t.Fatalf("delete after the rule is gone: %v", err)
	}
	var row struct {
		DeletedAt       *string
		SecretEnc       *string
		ExtraHeadersEnc *string
		KeyID           *string
	}
	if err := sdb.Get(&row, `SELECT deleted_at AS deletedat, secret_enc AS secretenc, extra_headers_enc AS extraheadersenc, key_id AS keyid
		FROM sys_named_credentials WHERE tenant_id = ? AND id = ?`, tenant, v.ID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if row.DeletedAt == nil || row.SecretEnc != nil || row.ExtraHeadersEnc != nil || row.KeyID != nil {
		t.Fatalf("delete did not tombstone + shred: %+v", row)
	}
	if _, _, err := svc.Get(ctx, tenant, v.ID); !errors.As(err, &ve) || ve.Code != "CREDENTIAL_NOT_FOUND" {
		t.Fatalf("get after delete = %v; want CREDENTIAL_NOT_FOUND", err)
	}
}

// TestCredentialService_URLTokenRoundTrip (specs/050 S5): a url_token key survives the real store -
// ResolveAuth opens the sealed token and splices it (path-escaped) into the send form only, and
// another tenant cannot resolve it.
func TestCredentialService_URLTokenRoundTrip(t *testing.T) {
	gdb, _ := o3aSetup(t)
	svc := NewCredentialService(credential.NewRepository(gdb), testKeyring(t), false)
	ctx, tenant := context.Background(), uuid.NewString()

	v, _, err := svc.Create(ctx, tenant, CredentialInput{
		Name: "tele", BaseURL: "https://api.telegram.org/bot{secret}", AuthType: "url_token",
		Secret: strPtr("123456:ABC-def_GHIjklMNO"), ActorID: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a, err := svc.ResolveAuth(ctx, tenant, v.ID)
	if err != nil || a.BaseURL != "https://api.telegram.org/bot123456:ABC-def_GHIjklMNO" || a.DisplayBase != "https://api.telegram.org/bot{secret}" {
		t.Fatalf("resolve = %+v err=%v; want the token in the send base and {secret} in the display base", a, err)
	}
	if _, err := svc.ResolveAuth(ctx, uuid.NewString(), v.ID); err == nil {
		t.Fatal("another tenant resolved this tenant's url_token key")
	}
}
