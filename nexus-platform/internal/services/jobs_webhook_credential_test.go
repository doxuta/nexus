package services

// specs/050 S2b — the durable outbound_webhook job with a vault key (plan §"Hợp đồng hàng đợi"):
// the job carries WHICH key the slot named (an id, never the key); the handler resolves the key at
// delivery under the job's tenant, refuses when the slot names another key by then ("rule đã
// đổi"), and fails the job at once — nothing sent — when the key cannot be used.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"nexus/platform/internal/jobs"
	"nexus/platform/internal/workflow"
)

// fakeWebhookCreds answers ResolveAuth from a (tenant|id) map; err (when set) wins.
type fakeWebhookCreds struct {
	auth  map[string]workflow.WebhookAuth
	err   error
	calls []string
}

func (f *fakeWebhookCreds) ResolveAuth(_ context.Context, tenantID, id string) (workflow.WebhookAuth, error) {
	f.calls = append(f.calls, tenantID+"|"+id)
	if f.err != nil {
		return workflow.WebhookAuth{}, f.err
	}
	a, ok := f.auth[tenantID+"|"+id]
	if !ok {
		return workflow.WebhookAuth{}, &workflow.CodedError{Code: workflow.CodeCredentialNotFound}
	}
	return a, nil
}

// TestEnqueuingWebhookQueue_CarriesCredentialID: the job names the key by id so the handler can
// tell the slot changed under it — and still carries no url, secret or header.
func TestEnqueuingWebhookQueue_CarriesCredentialID(t *testing.T) {
	q := &fakeJobEnqueuer{}
	if err := NewEnqueuingWebhookQueue(q).EnqueueWebhook(context.Background(), workflow.WebhookDelivery{
		TenantID: "t1", WorkflowID: "wf1", ActionIndex: 0, CredentialID: "cred-1", Body: []byte(`{"a":1}`),
	}); err != nil {
		t.Fatalf("EnqueueWebhook: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(q.jobs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["credentialId"] != "cred-1" || len(p) != 4 {
		t.Fatalf("payload = %v; want workflowId, actionIndex, credentialId, body only", p)
	}
}

func TestOutboundWebhookJobHandler_VaultKey(t *testing.T) {
	vault := workflow.Action{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: "/orders?kind=paid"}
	rules := &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{
		"wf-v":      webhookRule(t, "t1", "wf-v", []workflow.Action{vault}),
		"wf-other":  webhookRule(t, "t1", "wf-other", []workflow.Action{{Type: workflow.ActionWebhook, CredentialID: "cred-2"}}),
		"wf-legacy": webhookRule(t, "t1", "wf-legacy", []workflow.Action{{Type: workflow.ActionWebhook, URL: "https://erp.test/in"}}),
	}}
	creds := func() *fakeWebhookCreds {
		return &fakeWebhookCreds{auth: map[string]workflow.WebhookAuth{
			"t1|cred-1": {BaseURL: "https://pay.example.vn/api", DisplayBase: "https://pay.example.vn/api",
				Headers: map[string]string{"Authorization": "Bearer tok"}, SigningSecret: "hmac-key"},
		}}
	}
	const body = `{"workflowId":"wf-v"}`
	payload := func(wf, cred string) string {
		p := `{"workflowId":"` + wf + `","actionIndex":0,"body":` + strconv.Quote(body)
		if cred != "" {
			p += `,"credentialId":"` + cred + `"`
		}
		return p + `}`
	}
	job := func(p string) jobs.Job {
		return jobs.Job{ID: "job-9", Type: OutboundWebhookJobType, TenantID: "t1", Payload: json.RawMessage(p)}
	}

	t.Run("resolved at delivery under the job's tenant", func(t *testing.T) {
		c, d := creds(), &fakeDeliverer{}
		if err := NewOutboundWebhookJobHandler(rules, c, d)(context.Background(), job(payload("wf-v", "cred-1"))); err != nil {
			t.Fatalf("handler: %v", err)
		}
		r := d.req
		if d.calls != 1 || r.URL != "https://pay.example.vn/api/orders?kind=paid" || r.Headers["Authorization"] != "Bearer tok" ||
			r.SigningSecret != "hmac-key" || !r.NoRedirect || r.DeliveryID != "job-9" || string(r.Body) != body {
			t.Fatalf("delivered %+v (calls %d); want the key's base+path, its header and hmac key, no redirects, the job id", r, d.calls)
		}
		if r.DisplayURL != "https://pay.example.vn/api/orders" {
			t.Fatalf("display = %q; want base + path without query", r.DisplayURL)
		}
		if len(c.calls) != 1 || c.calls[0] != "t1|cred-1" {
			t.Fatalf("resolver asked %v; want t1|cred-1", c.calls)
		}
	})

	cases := []struct {
		name      string
		payload   string
		creds     workflow.CredentialResolver
		code      string // "" = transient (queue retries)
		permanent bool
	}{
		{"slot without a secret", payload("wf-v", "cred-1"), &fakeWebhookCreds{err: &workflow.CodedError{Code: workflow.CodeCredentialNotReady}},
			workflow.CodeCredentialNotReady, true},
		{"master key missing", payload("wf-v", "cred-1"), &fakeWebhookCreds{err: &workflow.CodedError{Code: workflow.CodeCredentialKeyUnavailable}},
			workflow.CodeCredentialKeyUnavailable, true},
		{"key deleted", payload("wf-v", "cred-1"), &fakeWebhookCreds{auth: map[string]workflow.WebhookAuth{}},
			workflow.CodeCredentialNotFound, true},
		{"no resolver wired", payload("wf-v", "cred-1"), nil, workflow.CodeCredentialsNotConfigured, true},
		{"vault store down", payload("wf-v", "cred-1"), &fakeWebhookCreds{err: errors.New("db down")}, "", false},
		{"slot now names another key", payload("wf-other", "cred-1"), creds(), "rule đã đổi", true},
		{"slot turned into a url action", payload("wf-legacy", "cred-1"), creds(), "rule đã đổi", true},
		{"url job whose slot now names a key", payload("wf-v", ""), creds(), "rule đã đổi", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDeliverer{}
			var c workflow.CredentialResolver
			if tc.creds != nil {
				c = tc.creds
			}
			err := NewOutboundWebhookJobHandler(rules, c, d)(context.Background(), job(tc.payload))
			if err == nil || jobs.IsPermanent(err) != tc.permanent {
				t.Fatalf("err = %v (permanent %v); want permanent %v", err, jobs.IsPermanent(err), tc.permanent)
			}
			if tc.code != "" && !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("err = %v; want it to name %q (sys_jobs.last_error is what the admin reads)", err, tc.code)
			}
			if d.calls != 0 {
				t.Fatalf("delivered %d times; a key that cannot be used sends NOTHING", d.calls)
			}
		})
	}

	t.Run("a redirect is permanent", func(t *testing.T) {
		d := &fakeDeliverer{err: &workflow.CodedError{Code: workflow.CodeRedirectNotFollowed}}
		err := NewOutboundWebhookJobHandler(rules, creds(), d)(context.Background(), job(payload("wf-v", "cred-1")))
		if !jobs.IsPermanent(err) || !strings.Contains(err.Error(), workflow.CodeRedirectNotFollowed) {
			t.Fatalf("err = %v; want a permanent WEBHOOK_REDIRECT_NOT_FOLLOWED", err)
		}
	})
}
