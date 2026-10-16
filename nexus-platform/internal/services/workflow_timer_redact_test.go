package services

// specs/050 S3a (T057, D-11): the admin API masks a rule's inline key, but only at the HTTP
// boundary. The paths that SEND read the stored row: the timer job handler loads the rule through
// the repository (rules.Get) and must post the real header and sign with the real key — a mask
// applied in the repository or the service would send "••••" as a live Authorization header. The
// dry-run reads the stored row too (it reports wouldSign), but what it SHOWS is a response like any
// other read: the target is masked the same way GET /admin/workflows masks it.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

// capturingWebhookSender records every request the engine posts (inline path, no queue).
type capturingWebhookSender struct {
	mu   sync.Mutex
	reqs []workflow.WebhookRequest
}

func (c *capturingWebhookSender) Post(_ context.Context, req workflow.WebhookRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
	return nil
}

const (
	timerLegacyKey    = "timer-signing-key-51"
	timerLegacyBearer = "Bearer timer-tok-51"
	timerLegacyURL    = "https://shop:pa55-timer@erp.example.vn/due?token=q-51"
)

// TestTimerJob_ReadsUnredactedRule: a date_offset rule saved before S3a with an inline key fires
// from its parked wake-up with the real url, the real header and the real signing key.
func TestTimerJob_ReadsUnredactedRule(t *testing.T) {
	rec := timerRuleRecord(t, true, nil)
	acts, err := json.Marshal([]workflow.Action{{Type: workflow.ActionWebhook, URL: timerLegacyURL,
		Secret: timerLegacyKey, Headers: map[string]string{"Authorization": timerLegacyBearer}}})
	if err != nil {
		t.Fatal(err)
	}
	rec.Actions = acts
	rules := &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": rec}}
	records := &fakeTimerRecords{rows: map[string]*dynrepo.Record{
		"r1": {ID: "r1", TenantID: "t1", CreatedBy: "owner-1", Data: map[string]any{"close_date": "2026-08-01"}},
	}}
	sender := &capturingWebhookSender{}
	engine := workflow.NewAsyncDispatcher(workflow.DispatcherConfig{Buffer: 1, Workers: 1}, nil, &countingMutator{}, nil, nil).
		WithWebhookSender(sender)

	h := NewWorkflowTimerJobHandler(rules, records, engine, nil, nil)
	if err := h(context.Background(), timerJob(t, "2026-08-01")); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(sender.reqs) != 1 {
		t.Fatalf("posted %d requests, want 1", len(sender.reqs))
	}
	got := sender.reqs[0]
	if got.URL != timerLegacyURL || got.Headers["Authorization"] != timerLegacyBearer || got.SigningSecret != timerLegacyKey {
		t.Fatalf("timer fire sent url=%q Authorization=%q key=%q; want the stored (unmasked) values",
			got.URL, got.Headers["Authorization"], got.SigningSecret)
	}
}

// TestPreviewWebhook_LegacyURLMasked: "Chạy thử" of a legacy action still knows it would sign (it
// reads the stored key) but shows the target the way GET shows it — never the userinfo or the
// query token.
func TestPreviewWebhook_LegacyURLMasked(t *testing.T) {
	rec := &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"stage": "new"}}
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(w6Catalog()).WithRecordReader(&fakeRecordReader{rec: rec})
	id := seedLegacyRule(t, repo, []workflow.Action{{Type: workflow.ActionWebhook, URL: timerLegacyURL,
		Secret: timerLegacyKey, Headers: map[string]string{"Authorization": timerLegacyBearer}}})

	got, err := svc.TestWorkflow(context.Background(), "t1", id, "r1")
	if err != nil {
		t.Fatalf("TestWorkflow: %v", err)
	}
	b, _ := json.Marshal(got)
	for _, leak := range []string{"shop:", "pa55-timer", "q-51", timerLegacyKey, "timer-tok-51"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("dry-run leaks %q: %s", leak, b)
		}
	}
	detail := got.Trace[len(got.Trace)-1].Detail
	m := workflow.RedactedMarker
	if detail["url"] != "https://"+m+"@erp.example.vn/due?token="+m || detail["wouldSign"] != true {
		t.Fatalf("legacy preview = %v; want the masked target and wouldSign=true", detail)
	}
}
