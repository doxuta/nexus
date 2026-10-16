package vertical_pack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/credential"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// specs/050 S6: a pack carries a vault key as a NAME + slot definition, never an id or a secret.

type fakeVault struct {
	mu         sync.Mutex
	views      []services.CredentialView
	created    []services.CredentialInput
	unconfig   bool
	allowHTTP  bool
	createFail error
}

func (f *fakeVault) Configured() bool         { return !f.unconfig }
func (f *fakeVault) AllowsInsecureHTTP() bool { return f.allowHTTP }
func (f *fakeVault) List(_ context.Context, _ string) ([]services.CredentialView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]services.CredentialView(nil), f.views...), nil
}
func (f *fakeVault) Create(_ context.Context, _ string, in services.CredentialInput) (*services.CredentialView, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createFail != nil {
		return nil, "", f.createFail
	}
	f.created = append(f.created, in)
	v := services.CredentialView{ID: fmt.Sprintf("new-%d", len(f.created)), Name: in.Name, AuthType: credential.AuthType(in.AuthType), Status: credential.StatusNeedsSecret}
	f.views = append(f.views, v)
	return &v, "", nil
}

func vaultWorkflowRec(credID, path string) workflow.WorkflowRecord {
	acts, _ := json.Marshal([]workflow.Action{{Type: workflow.ActionWebhook, CredentialID: credID, Path: path}})
	return workflow.WorkflowRecord{ID: "wf-1", Name: "gọi cổng thanh toán", EntityName: "deals", Trigger: "after_create", Actions: acts, IsActive: true}
}

func srcVaultView() services.CredentialView {
	return services.CredentialView{
		ID: "SRC-ID-9f3", Name: "payments", Label: "Cổng thanh toán", BaseURL: "https://pay.example.com/v1",
		AuthType: credential.AuthHeader, HeaderName: "X-Api-Key", HeaderPrefix: "Key ",
		Status: credential.StatusReady, SecretHint: "ab12", ExtraHeaderNames: []string{"X-Partner"},
	}
}

func exportWith(t *testing.T, v *fakeVault, recs ...workflow.WorkflowRecord) (*Pack, []string) {
	t.Helper()
	svc := NewService(Deps{EntityReader: &fakeMintMeta{}, RoleReader: fakeMintRoles{}, EntityWriter: &fakeMintMeta{},
		WorkflowWriter: &fakeWorkflowWriter{existing: recs}, CredentialVault: v})
	pack := &Pack{}
	var warnings []string
	warn := func(f string, a ...any) { warnings = append(warnings, fmt.Sprintf(f, a...)) }
	if err := svc.exportWorkflows(context.Background(), "t1", map[string]bool{"deals": true}, nil, pack, warn, func(string) {}); err != nil {
		t.Fatalf("exportWorkflows: %v", err)
	}
	return pack, warnings
}

func TestExport_CredentialBecomesSlot(t *testing.T) {
	pack, _ := exportWith(t, &fakeVault{views: []services.CredentialView{srcVaultView()}}, vaultWorkflowRec("SRC-ID-9f3", "/orders"))
	if len(pack.Workflows) != 1 || len(pack.Credentials) != 1 {
		t.Fatalf("workflows=%d credentials=%d; want 1/1", len(pack.Workflows), len(pack.Credentials))
	}
	a := pack.Workflows[0].Actions[0]
	if a.CredentialName != "payments" || a.Path != "/orders" {
		t.Fatalf("action = %+v; want credentialName payments, path /orders", a)
	}
	c := pack.Credentials[0]
	if c.Name != "payments" || c.BaseURL != "https://pay.example.com/v1" || c.AuthType != "header" ||
		c.HeaderName != "X-Api-Key" || c.HeaderPrefix != "Key " || len(c.ExtraHeaderNames) != 1 {
		t.Fatalf("slot = %+v", c)
	}
	raw, _ := json.Marshal(pack)
	for _, leak := range []string{"SRC-ID-9f3", "ab12", "secretHint", "credentialId"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("pack JSON leaks %q: %s", leak, raw)
		}
	}
}

func TestExport_CredentialMissingOrSecretPathLeftOut(t *testing.T) {
	v := &fakeVault{views: []services.CredentialView{srcVaultView()}}
	pack, warnings := exportWith(t, v, vaultWorkflowRec("GONE", "/x"), vaultWorkflowRec("SRC-ID-9f3", "/in?token=abc"))
	if len(pack.Workflows) != 0 || len(pack.Credentials) != 0 {
		t.Fatalf("exported %+v / %+v; want both rules left out", pack.Workflows, pack.Credentials)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %q; want one per rule", warnings)
	}
}

func vaultPack() *Pack {
	return &Pack{
		Entities: []PackEntity{{Name: "deals", Label: "Deals", Fields: []PackField{{Name: "title", Label: "T", FieldType: "text"}}}},
		Credentials: []PackCredential{{Name: "payments", Label: "Cổng thanh toán", BaseURL: "https://pay.example.com/v1",
			AuthType: "header", HeaderName: "X-Api-Key", HeaderPrefix: "Key ", ExtraHeaderNames: []string{"X-Partner"}}},
		Workflows: []PackWorkflow{{Name: "gọi cổng thanh toán", EntityName: "deals", Trigger: "after_create",
			Actions: []PackWorkflowAction{{Type: "outbound_webhook", CredentialName: "payments", Path: "/orders"}}}},
	}
}

func installWith(t *testing.T, v *fakeVault, wf *fakeWorkflowWriter, pack *Pack) (*InstallResult, error) {
	t.Helper()
	svc := NewService(Deps{EntityReader: &fakeMintMeta{}, RoleReader: fakeMintRoles{}, EntityWriter: &fakeMintMeta{},
		WorkflowWriter: wf, CredentialVault: v})
	return svc.Install(context.Background(), "t2", "u1", pack)
}

func TestInstall_CreatesNeedsSecretSlot(t *testing.T) {
	v, wf := &fakeVault{}, &fakeWorkflowWriter{}
	res, err := installWith(t, v, wf, vaultPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(v.created) != 1 || v.created[0].Name != "payments" || v.created[0].Secret != nil || v.created[0].GenerateSecret ||
		v.created[0].HeaderName == nil || *v.created[0].HeaderName != "X-Api-Key" {
		t.Fatalf("created = %+v; want one secretless slot with the pack's header", v.created)
	}
	if len(wf.created) != 1 || wf.created[0].Actions[0].CredentialID != "new-1" || wf.created[0].Actions[0].Path != "/orders" {
		t.Fatalf("rule input = %+v; want credentialId new-1 + path", wf.created)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "Nhập bí mật ở Quản trị › Kho khoá kết nối") {
		t.Fatalf("warnings = %q; want the enter-secret hint", res.Warnings)
	}
}

func TestInstall_ReusesCredentialByName(t *testing.T) {
	existing := services.CredentialView{ID: "dst-1", Name: "payments", AuthType: credential.AuthHeader, Status: credential.StatusReady}
	v, wf := &fakeVault{views: []services.CredentialView{existing}}, &fakeWorkflowWriter{}
	res, err := installWith(t, v, wf, vaultPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(v.created) != 0 || wf.created[0].Actions[0].CredentialID != "dst-1" {
		t.Fatalf("created=%d id=%q; want reuse of dst-1", len(v.created), wf.created[0].Actions[0].CredentialID)
	}
	if strings.Contains(strings.Join(res.Warnings, "\n"), "Nhập bí mật") {
		t.Fatalf("a ready key must not ask for a secret: %q", res.Warnings)
	}
}

func TestInstall_UnknownCredentialNameRejected(t *testing.T) {
	p := vaultPack()
	p.Workflows[0].Actions[0].CredentialName = "ghost"
	v, wf := &fakeVault{}, &fakeWorkflowWriter{}
	_, err := installWith(t, v, wf, p)
	var ve *services.ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Msg, "ghost") {
		t.Fatalf("err = %v; want a ValidationError naming ghost", err)
	}
	if len(v.created) != 0 || len(wf.created) != 0 {
		t.Fatal("nothing may be written on a rejected pack")
	}
}

func TestInstall_VaultNotConfiguredRejectedBeforeWrites(t *testing.T) {
	v, wf := &fakeVault{unconfig: true}, &fakeWorkflowWriter{}
	_, err := installWith(t, v, wf, vaultPack())
	var ve *services.ValidationError
	if !errors.As(err, &ve) || ve.Code != "CREDENTIALS_NOT_CONFIGURED" {
		t.Fatalf("err = %v; want CREDENTIALS_NOT_CONFIGURED", err)
	}
}

func TestProvenance_StoresCredentialName(t *testing.T) {
	pw := vaultPack().Workflows[0]
	raw, err := marshalItem(pw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"credentialName":"payments"`) || strings.Contains(string(raw), "credentialId") {
		t.Fatalf("provenance item = %s; want the name, no id", raw)
	}
}

func TestInstall_BadSlotMetadataRejectedBeforeWrites(t *testing.T) {
	cases := map[string]struct {
		mut  func(*PackCredential)
		code string
	}{
		"http base":     {func(c *PackCredential) { c.BaseURL = "http://evil.com" }, "CREDENTIAL_BASE_URL_INVALID"},
		"userinfo base": {func(c *PackCredential) { c.BaseURL = "https://u:p@pay.example.com" }, "CREDENTIAL_BASE_URL_INVALID"},
		"auth type":     {func(c *PackCredential) { c.AuthType = "foo" }, "CREDENTIAL_AUTH_TYPE_INVALID"},
		"reserved hdr":  {func(c *PackCredential) { c.AuthType = "header"; c.HeaderName = "Host" }, "CREDENTIAL_HEADER_INVALID"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := vaultPack()
			tc.mut(&p.Credentials[0])
			v, wf := &fakeVault{}, &fakeWorkflowWriter{}
			_, err := installWith(t, v, wf, p)
			var ve *services.ValidationError
			if !errors.As(err, &ve) || ve.Code != tc.code {
				t.Fatalf("err = %v; want %s", err, tc.code)
			}
			if len(v.created) != 0 || len(wf.created) != 0 {
				t.Fatal("nothing may be written on a rejected pack")
			}
		})
	}
}

func TestInstall_ReuseWithDifferentBaseURLWarns(t *testing.T) {
	existing := services.CredentialView{ID: "dst-1", Name: "payments", BaseURL: "https://other.example.org", AuthType: credential.AuthHeader, Status: credential.StatusReady}
	res, err := installWith(t, &fakeVault{views: []services.CredentialView{existing}}, &fakeWorkflowWriter{}, vaultPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "địa chỉ gốc khác") {
		t.Fatalf("warnings = %q; want base-url mismatch", res.Warnings)
	}
}
