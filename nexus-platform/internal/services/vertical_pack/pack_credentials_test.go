package vertical_pack

import (
	"context"
	"testing"

	"nexus/platform/internal/services"
)

// A pack is handed to a DIFFERENT company. exportHooks has always said so for inbound hooks
// ("Dropping the secret is the whole point, not an omission") and enforced it by not having a
// secret field at all; the OUTBOUND direction shipped the live HMAC key and every custom header
// verbatim, install wrote them into the target tenant's sys_workflows.config in plaintext, and
// provenance persisted a SECOND copy into that tenant's sys_pack_items — readable by that
// company's admin through GET /admin/workflows and GET /admin/verticals/installs/:slug/diff.
// Pin the rule at the one function all three paths call.
func TestStripWorkflowCredentials(t *testing.T) {
	t.Run("gỡ secret + headers của outbound_webhook, giữ URL", func(t *testing.T) {
		in := PackWorkflow{Name: "CRM → ERP", Actions: []PackWorkflowAction{{
			Type:    "outbound_webhook",
			URL:     "https://partner.example.com/hooks/crm",
			Secret:  "hmac-prod-8f3a",
			Headers: map[string]string{"Authorization": "Bearer sk_live_xxx", "X-Source": "nexus"},
		}}}
		out, n := stripWorkflowCredentials(in)
		if n != 1 {
			t.Fatalf("stripped = %d; want 1", n)
		}
		got := out.Actions[0]
		if got.Secret != "" {
			t.Errorf("Secret = %q; want empty — a live signing key must not cross the tenant boundary", got.Secret)
		}
		if len(got.Headers) != 0 {
			// Whole map, not a name allowlist: "X-Source" is harmless but the pack cannot tell it
			// apart from a bearer token under a company-specific name.
			t.Errorf("Headers = %v; want none", got.Headers)
		}
		if got.URL != "https://partner.example.com/hooks/crm" {
			t.Errorf("URL = %q; want it preserved so the rule's shape still installs", got.URL)
		}
	})

	t.Run("không đụng action khác loại", func(t *testing.T) {
		in := PackWorkflow{Actions: []PackWorkflowAction{
			{Type: "email_alert", To: "ops@x.vn", Subject: "hi", Body: "b"},
			{Type: "field_update", Field: "status", Value: "won"},
		}}
		out, n := stripWorkflowCredentials(in)
		if n != 0 {
			t.Fatalf("stripped = %d; want 0", n)
		}
		if out.Actions[0].To != "ops@x.vn" || out.Actions[1].Value != "won" {
			t.Errorf("non-webhook actions were modified: %+v", out.Actions)
		}
	})

	t.Run("không mutate bản gốc", func(t *testing.T) {
		// provenanceItems iterates the very slice install is about to consume; stripping in place
		// would silently rewrite the caller's pack.
		orig := PackWorkflow{Actions: []PackWorkflowAction{{
			Type: "outbound_webhook", Secret: "keep-me", Headers: map[string]string{"A": "b"},
		}}}
		if _, n := stripWorkflowCredentials(orig); n != 1 {
			t.Fatalf("stripped = %d; want 1", n)
		}
		if orig.Actions[0].Secret != "keep-me" || len(orig.Actions[0].Headers) != 1 {
			t.Fatalf("caller's pack was mutated: %+v", orig.Actions[0])
		}
	})

	t.Run("pack sạch đi qua nguyên vẹn", func(t *testing.T) {
		in := PackWorkflow{Actions: []PackWorkflowAction{{Type: "outbound_webhook", URL: "https://x/y"}}}
		out, n := stripWorkflowCredentials(in)
		if n != 0 || out.Actions[0].URL != "https://x/y" {
			t.Fatalf("n=%d actions=%+v; want an untouched pass-through", n, out.Actions)
		}
	})
}

// fakeRoleWr is the smallest roleWriter that exercises the reconcile path: one existing role whose
// grant set the test inspects after the call.
type fakeRoleWr struct {
	role    services.RoleView
	written []services.RoleActionInput
}

func (f *fakeRoleWr) ListRoles(context.Context, string) ([]services.RoleView, error) {
	return []services.RoleView{f.role}, nil
}

func (f *fakeRoleWr) CreateRole(context.Context, string, services.CreateRoleInput) (*services.RoleView, error) {
	return nil, nil
}

func (f *fakeRoleWr) GetRole(context.Context, string, string) (*services.RoleView, error) {
	r := f.role
	return &r, nil
}

func (f *fakeRoleWr) UpdateRole(_ context.Context, _, _ string, in services.UpdateRoleInput) (*services.RoleView, error) {
	f.written = in.Actions
	return &f.role, nil
}

// Install merges a pack's grants into a same-named role the customer already has, keyed by
// entity|action. A ("*","view") grant is therefore "missing" from ANY role that has only
// per-entity grants, so it was always appended — and ACLService.Authorize resolves '*' as every
// entity, so one handover could grant a customer's existing "sales" role read access to every
// table in their workspace, including entities from unrelated later deliveries. A pack may extend
// a live tenant only within its own footprint; a wildcard has no footprint.
func TestReconcileRoleGrantsRefusesWildcard(t *testing.T) {
	fake := &fakeRoleWr{role: services.RoleView{
		ID: "r1", Name: "sales",
		Actions: []services.RoleActionView{{EntityName: "crm_leads", Action: "view", AccessLevel: "owner"}},
	}}
	s := &Service{roleWr: fake}

	added, refused, err := s.reconcileRoleGrants(context.Background(), "t-acme", "sales", PackRole{
		Name: "sales",
		Actions: []PackRoleAction{
			{EntityName: "*", Action: "view", AccessLevel: "all"},
			{EntityName: "crm_accounts", Action: "view", AccessLevel: "all"},
		},
	})
	if err != nil {
		t.Fatalf("reconcileRoleGrants: %v", err)
	}
	if refused != 1 {
		t.Errorf("refused = %d; want 1 wildcard refused", refused)
	}
	if added != 1 {
		t.Errorf("added = %d; want 1 (the in-footprint grant still lands)", added)
	}
	for _, a := range fake.written {
		if a.EntityName == "*" {
			t.Fatalf("a wildcard grant was written to the customer's live role: %+v", fake.written)
		}
	}
	// The customer's own grant must survive untouched — reconcile is additive.
	var sawLeads bool
	for _, a := range fake.written {
		if a.EntityName == "crm_leads" && a.AccessLevel == "owner" {
			sawLeads = true
		}
	}
	if !sawLeads {
		t.Errorf("the tenant's own crm_leads grant was lost: %+v", fake.written)
	}
}

// A pack whose role carries ONLY wildcards must write nothing at all — not an empty UpdateRole
// (which REPLACES the grant set and would wipe what the customer has).
func TestReconcileRoleGrantsWildcardOnlyWritesNothing(t *testing.T) {
	fake := &fakeRoleWr{role: services.RoleView{
		ID: "r1", Name: "sales",
		Actions: []services.RoleActionView{{EntityName: "crm_leads", Action: "view", AccessLevel: "owner"}},
	}}
	s := &Service{roleWr: fake}

	added, refused, err := s.reconcileRoleGrants(context.Background(), "t-acme", "sales", PackRole{
		Name:    "sales",
		Actions: []PackRoleAction{{EntityName: "*", Action: "view", AccessLevel: "all"}},
	})
	if err != nil {
		t.Fatalf("reconcileRoleGrants: %v", err)
	}
	if added != 0 || refused != 1 {
		t.Fatalf("added=%d refused=%d; want 0/1", added, refused)
	}
	if fake.written != nil {
		t.Fatalf("UpdateRole was called with %+v; want no write at all", fake.written)
	}
}
