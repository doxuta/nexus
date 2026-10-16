package services

// specs/050 S2b (tasks T039) — an outbound_webhook action naming a key in the vault
// ({credentialId, path}) instead of carrying its own url/secret/headers. Save time: the key must be
// a live key of the rule's tenant, the action carries nothing of its own, the path cannot leave the
// key's host — in the ONE body every write path runs. Dry-run: names the key, never opens it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

// fakeWFCreds answers Describe from a (tenant|id) map, like CredentialService over the vault; err
// (when set) is a store outage.
type fakeWFCreds struct {
	views map[string]*CredentialView
	err   error
	asked []string
}

func (f *fakeWFCreds) Describe(_ context.Context, tenantID, id string) (*CredentialView, error) {
	f.asked = append(f.asked, tenantID+"|"+id)
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.views[tenantID+"|"+id]; ok {
		cp := *v
		return &cp, nil
	}
	return nil, credentialNotFound()
}

func vaultCreds() *fakeWFCreds {
	return &fakeWFCreds{views: map[string]*CredentialView{
		"t1|cred-1": {ID: "cred-1", Name: "cong-thanh-toan", BaseURL: "https://pay.example.vn/api", AuthType: "hmac", Status: "ready"},
		"t2|cred-b": {ID: "cred-b", Name: "khoa-to-chuc-khac", BaseURL: "https://b.example.vn", AuthType: "header", Status: "ready"},
	}}
}

func vaultHook(id, path string) WorkflowActionInput {
	return WorkflowActionInput{Type: "outbound_webhook", CredentialID: id, Path: path}
}

// TestValidateWorkflow_WebhookCredential: each violation is refused with its code and
// details.field pointing at the action's slot, and nothing is stored.
func TestValidateWorkflow_WebhookCredential(t *testing.T) {
	cases := []struct {
		name  string
		act   WorkflowActionInput
		code  string
		field string
	}{
		{"unknown key", vaultHook("cred-404", "/orders"), "WEBHOOK_CREDENTIAL_NOT_FOUND", "actions[1].credentialId"},
		{"another tenant's key", vaultHook("cred-b", "/orders"), "WEBHOOK_CREDENTIAL_NOT_FOUND", "actions[1].credentialId"},
		{"url alongside the key", WorkflowActionInput{Type: "outbound_webhook", CredentialID: "cred-1", URL: "https://evil.test"},
			"WEBHOOK_URL_WITH_CREDENTIAL", "actions[1].url"},
		{"inline secret alongside the key", WorkflowActionInput{Type: "outbound_webhook", CredentialID: "cred-1", Secret: "k"},
			"WEBHOOK_URL_WITH_CREDENTIAL", "actions[1].secret"},
		{"inline header alongside the key", WorkflowActionInput{Type: "outbound_webhook", CredentialID: "cred-1",
			Headers: map[string]string{"Authorization": "Bearer x"}}, "WEBHOOK_URL_WITH_CREDENTIAL", "actions[1].headers"},
		{"path leaves the host", vaultHook("cred-1", "//evil.test/x"), "WEBHOOK_PATH_INVALID", "actions[1].path"},
		{"path climbs out of the base", vaultHook("cred-1", "/../admin"), "WEBHOOK_PATH_INVALID", "actions[1].path"},
		{"path with a broken escape", vaultHook("cred-1", "/%zz"), "WEBHOOK_PATH_INVALID", "actions[1].path"},
		{"path with a truncated escape", vaultHook("cred-1", "/a%"), "WEBHOOK_PATH_INVALID", "actions[1].path"},
		{"path without a key", WorkflowActionInput{Type: "outbound_webhook", URL: legacyHook, Path: "/orders"},
			"WEBHOOK_PATH_INVALID", "actions[1].path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog()).WithCredentialChecker(vaultCreds())
			_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
				Name: "gọi đối tác", EntityName: "deals", Trigger: "after_create",
				Actions: []WorkflowActionInput{{Type: "field_update", Field: "probability", Value: 10.0}, tc.act},
			})
			ve := wantBeforeCode(t, err, tc.code)
			if ve.Details["field"] != tc.field {
				t.Fatalf("details.field = %v, want %s", ve.Details["field"], tc.field)
			}
			if len(repo.rows) != 0 {
				t.Fatal("a refused rule was stored")
			}
		})
	}

	t.Run("a live key of the tenant is stored as {credentialId, path} only", func(t *testing.T) {
		repo := newFakeWFRepo()
		creds := vaultCreds()
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog()).WithCredentialChecker(creds)
		rec, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "gọi đối tác", EntityName: "deals", Trigger: "after_create",
			Actions: []WorkflowActionInput{vaultHook(" cred-1 ", "/orders?kind=paid")},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if got := string(repo.rows[rec.ID].Actions); got != `[{"type":"outbound_webhook","credentialId":"cred-1","path":"/orders?kind=paid"}]` {
			t.Fatalf("stored actions = %s", got)
		}
		if len(creds.asked) != 1 || creds.asked[0] != "t1|cred-1" {
			t.Fatalf("checker asked %v; want the key looked up once in the rule's tenant", creds.asked)
		}
	})

	t.Run("no checker wired refuses instead of storing an unchecked key", func(t *testing.T) {
		repo := newFakeWFRepo()
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
		if _, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "x", EntityName: "deals", Trigger: "after_create", Actions: []WorkflowActionInput{vaultHook("cred-1", "")},
		}); err == nil || len(repo.rows) != 0 {
			t.Fatalf("err = %v, rows %d; want a refusal and nothing stored", err, len(repo.rows))
		}
	})

	t.Run("a vault outage is a server error, not a verdict on the key", func(t *testing.T) {
		svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(beforeCatalog()).
			WithCredentialChecker(&fakeWFCreds{err: errors.New("db down")})
		_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "x", EntityName: "deals", Trigger: "after_create", Actions: []WorkflowActionInput{vaultHook("cred-1", "")},
		})
		var ve *ValidationError
		if err == nil || errors.As(err, &ve) {
			t.Fatalf("err = %v; want a plain (500) error", err)
		}
	})
}

// TestUpdateWorkflow_AllowsReplacingLegacyWithCredential: moving a rule off its inline key onto a
// vault key is exactly what the 409 guard asks for — it must save; and Update runs the same
// existence check as Create.
func TestUpdateWorkflow_AllowsReplacingLegacyWithCredential(t *testing.T) {
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog()).WithCredentialChecker(vaultCreds())
	id := seedLegacyRule(t, repo, []workflow.Action{signedLegacy()})
	if err := putActions(svc, id, []WorkflowActionInput{vaultHook("cred-1", "/hook")}); err != nil {
		t.Fatalf("replacing the inline key with a vault key must save, got %v", err)
	}
	if got := string(repo.rows[id].Actions); strings.Contains(got, "partner-signing-key") || !strings.Contains(got, `"credentialId":"cred-1"`) {
		t.Fatalf("stored actions = %s", got)
	}
	wantBeforeCode(t, putActions(svc, id, []WorkflowActionInput{vaultHook("cred-b", "/hook")}), "WEBHOOK_CREDENTIAL_NOT_FOUND")
}

// TestValidateWorkflow_ParityAcrossWritePaths: the shape rules live in the ONE body every write
// path runs (buildWorkflowRule — admin API, pack install via CreateWorkflow, the AI Builder's
// ValidateWorkflowConfig), so a verdict cannot differ between them.
func TestValidateWorkflow_ParityAcrossWritePaths(t *testing.T) {
	for _, act := range []WorkflowActionInput{
		{Type: "outbound_webhook", CredentialID: "cred-1", URL: "https://evil.test"},
		vaultHook("cred-1", "/%2e%2e/admin"),
		{Type: "outbound_webhook", URL: legacyHook, Path: "/x"},
	} {
		acts := []WorkflowActionInput{act}
		pure := ValidateWorkflowConfig("deals", "after_create", nil, acts, beforeFieldTypes())
		svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(beforeCatalog()).WithCredentialChecker(vaultCreds())
		_, apply := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "x", EntityName: "deals", Trigger: "after_create", Actions: acts,
		})
		var a, b *ValidationError
		if !errors.As(pure, &a) || !errors.As(apply, &b) || a.Code != b.Code || a.Details["field"] != b.Details["field"] {
			t.Fatalf("%+v: config-time %v vs apply %v — the verdicts differ", act, pure, apply)
		}
	}
}

// TestPreviewWebhook_ShowsCredentialNameNeverSecret: "Chạy thử" names the key, its type and
// status, the display URL and whether it would sign — and never opens the key (FR-018).
func TestPreviewWebhook_ShowsCredentialNameNeverSecret(t *testing.T) {
	rec := &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"stage": "new"}}
	creds := vaultCreds()
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(w6Catalog()).WithRecordReader(&fakeRecordReader{rec: rec}).
		WithCredentialChecker(creds)
	// The inline-key action is a row saved before specs/050 S3a — no write path accepts one now.
	id := seedLegacyRule(t, repo, []workflow.Action{
		{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: "/orders?token=abc"},
		{Type: workflow.ActionWebhook, URL: legacyHook, Secret: "inline-key"},
	})
	preview := func() (vault, legacy map[string]any) {
		t.Helper()
		got, err := svc.TestWorkflow(context.Background(), "t1", id, "r1")
		if err != nil {
			t.Fatalf("TestWorkflow: %v", err)
		}
		if b, _ := json.Marshal(got); strings.Contains(string(b), "abc") {
			t.Fatalf("dry-run leaks the path's query value: %s", b)
		}
		n := len(got.Trace)
		return got.Trace[n-2].Detail, got.Trace[n-1].Detail
	}

	vault, legacy := preview()
	cred, _ := vault["credential"].(map[string]any)
	if cred["id"] != "cred-1" || cred["name"] != "cong-thanh-toan" || fmt.Sprint(cred["authType"]) != "hmac" || fmt.Sprint(cred["status"]) != "ready" {
		t.Fatalf("credential = %v; want id/name/authType/status of the key", vault["credential"])
	}
	if vault["url"] != "https://pay.example.vn/api/orders" || vault["wouldSign"] != true || vault["wouldSend"] != true {
		t.Fatalf("vault preview = %v; want the display url (no query), wouldSign and wouldSend", vault)
	}
	if legacy["wouldSign"] != true {
		t.Fatalf("legacy preview = %v; an inline secret signs too", legacy)
	}

	// A key that cannot send previews as "would not send", naming the runtime code.
	creds.views["t1|cred-1"].Status = "needs_secret"
	if vault, _ = preview(); vault["wouldSend"] != false || !strings.Contains(fmt.Sprint(vault["error"]), "CREDENTIAL_NOT_READY") {
		t.Fatalf("slot preview = %v; want wouldSend=false naming CREDENTIAL_NOT_READY", vault)
	}
	delete(creds.views, "t1|cred-1")
	if vault, _ = preview(); vault["wouldSend"] != false || !strings.Contains(fmt.Sprint(vault["error"]), "CREDENTIAL_NOT_FOUND") {
		t.Fatalf("deleted-key preview = %v; want wouldSend=false naming CREDENTIAL_NOT_FOUND", vault)
	}
}

// TestPreviewWebhook_URLTokenShowsPlaceholder (specs/050 S5, T086): the dry-run of a rule using a
// url_token key shows the base with {secret}, never a token, and says it would not sign.
func TestPreviewWebhook_URLTokenShowsPlaceholder(t *testing.T) {
	rec := &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"stage": "new"}}
	creds := vaultCreds()
	creds.views["t1|cred-tg"] = &CredentialView{ID: "cred-tg", Name: "telegram", BaseURL: "https://api.telegram.org/bot{secret}", AuthType: "url_token", Status: "ready"}
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(w6Catalog()).WithRecordReader(&fakeRecordReader{rec: rec}).WithCredentialChecker(creds)
	id := seedLegacyRule(t, repo, []workflow.Action{{Type: workflow.ActionWebhook, CredentialID: "cred-tg", Path: "/sendMessage?chat_id=1"}})
	got, err := svc.TestWorkflow(context.Background(), "t1", id, "r1")
	if err != nil {
		t.Fatalf("TestWorkflow: %v", err)
	}
	d := got.Trace[len(got.Trace)-1].Detail
	if d["url"] != "https://api.telegram.org/bot{secret}/sendMessage" || d["wouldSign"] != false || d["wouldSend"] != true {
		t.Fatalf("preview = %v; want the placeholder URL (no query), wouldSign=false, wouldSend=true", d)
	}
}
