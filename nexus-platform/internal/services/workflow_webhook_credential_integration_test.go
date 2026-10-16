//go:build integration

// specs/050 S2b (tasks T042) — a webhook that names a vault key, END-TO-END over a freshly migrated
// throwaway MySQL DB (o3aSetup), wired like cmd/server/main.go:
//
//	CredentialService.Create (header token, sealed in sys_named_credentials)
//	WorkflowService.CreateWorkflow {credentialId, path}           (save-time key check)
//	RecordService.Create → AsyncDispatcher → sys_jobs              (job names the key by id only)
//	jobs.Manager → NewOutboundWebhookJobHandler(creds = the SAME CredentialService) → receiver
//
// The key's secret is then replaced straight in the table (rotation's API is S4) and a second
// record saved AT ONCE — the rule is still in the engine's cache — and must go out with the new
// token: the key is read at send time, never cached (FR-014). A slot without a secret sends
// nothing and fails its job CREDENTIAL_NOT_READY at once (FR-016). Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=true&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run WorkflowWebhookCredential
package services

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/credential"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// credHit is one request the vault receiver saw: where it went and what it authenticated with.
type credHit struct{ path, auth string }

type credReceiver struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []credHit
}

func newCredReceiver(t *testing.T) *credReceiver {
	t.Helper()
	r := &credReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits = append(r.hits, credHit{path: req.URL.RequestURI(), auth: req.Header.Get("Authorization")})
		r.mu.Unlock()
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *credReceiver) wait(t *testing.T, n int) []credHit {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		r.mu.Lock()
		hits := append([]credHit(nil), r.hits...)
		r.mu.Unlock()
		if len(hits) >= n {
			return hits
		}
		if time.Now().After(deadline) {
			t.Fatalf("receiver saw %d requests, want %d", len(hits), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWorkflowWebhookCredential_EndToEnd(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000203"

	keys := testKeyring(t)
	credSvc := NewCredentialService(credential.NewRepository(gdb), keys, false) // loopback http is allowed
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, time.Minute) // the rule stays cached across both saves
	jobsStore := jobs.NewSQLStore(sdb, 5)
	sender := workflow.NewHTTPWebhookSender(2*time.Second, 1, 5*time.Millisecond)
	dispatcher := workflow.NewAsyncDispatcher(workflow.DispatcherConfig{Buffer: 16, Workers: 1},
		wfCache, NewWorkflowRecordMutator(dynRepo).WithCatalog(mc), wfRepo, nil).
		WithWebhookSender(sender).
		WithWebhookQueue(NewEnqueuingWebhookQueue(jobsStore)).
		WithCredentialResolver(credSvc).
		WithRunRecorder(wfRepo)
	reg := jobs.NewRegistry()
	if err := reg.Register(OutboundWebhookJobType, NewOutboundWebhookJobHandler(wfRepo, credSvc, sender)); err != nil {
		t.Fatalf("register handler: %v", err)
	}
	st := &wfStack{
		sdb:         sdb,
		entitySvc:   NewEntityService(dynRepo, metaRepo).WithMetaCache(mc),
		recordSvc:   NewRecordService(mc, dynRepo).WithEngine(dispatcher),
		workflowSvc: NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache).WithCredentialChecker(credSvc),
		dispatcher:  dispatcher,
		cache:       wfCache,
	}
	t.Cleanup(func() { st.drain(t) })

	recv := newCredReceiver(t)
	st.createEntity(t, ctx, tenant, "orders", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	key, _, err := credSvc.Create(ctx, tenant, CredentialInput{Name: "erp", BaseURL: recv.srv.URL + "/api",
		AuthType: "header", Secret: strPtr("tok-old")})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	slot, _, err := credSvc.Create(ctx, tenant, CredentialInput{Name: "slot", BaseURL: recv.srv.URL, AuthType: "hmac"})
	if err != nil {
		t.Fatalf("create slot: %v", err)
	}
	active := true
	for name, act := range map[string]WorkflowActionInput{
		"push-to-erp":  {Type: "outbound_webhook", CredentialID: key.ID, Path: "/orders?src=nexus"},
		"push-to-slot": {Type: "outbound_webhook", CredentialID: slot.ID, Path: "/never"},
	} {
		if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
			Name: name, EntityName: "orders", Trigger: "after_create", IsActive: &active,
			Actions: []WorkflowActionInput{act}, ActorID: user,
		}); err != nil {
			t.Fatalf("CreateWorkflow %s: %v", name, err)
		}
	}
	// Another tenant's rule cannot name this tenant's key (same entity there, so only the key differs).
	st.createEntity(t, ctx, o3aTenantB, "orders", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	_, err = st.workflowSvc.CreateWorkflow(ctx, o3aTenantB, CreateWorkflowInput{
		Name: "steal", EntityName: "orders", Trigger: "after_create", IsActive: &active,
		Actions: []WorkflowActionInput{{Type: "outbound_webhook", CredentialID: key.ID}}, ActorID: user,
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "WEBHOOK_CREDENTIAL_NOT_FOUND" {
		t.Fatalf("tenant B naming tenant A's key = %v; want WEBHOOK_CREDENTIAL_NOT_FOUND", err)
	}

	m := newWebhookManager(jobsStore, reg)
	m.Start()
	defer stopManager(t, m)

	if _, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "orders", map[string]any{"status": "new"}); err != nil {
		t.Fatalf("create order 1: %v", err)
	}
	first := recv.wait(t, 1)[0]
	if first.path != "/api/orders?src=nexus" || first.auth != "Bearer tok-old" {
		t.Fatalf("first delivery = %+v; want /api/orders?src=nexus with Bearer tok-old", first)
	}

	// The partner's token changes in the vault (S4 will do this through /rotate).
	env, err := keys.SealAAD([]byte("tok-new"), credential.AAD(tenant, key.ID, credential.SlotSecret))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := sdb.Exec(`UPDATE sys_named_credentials SET secret_enc = ? WHERE tenant_id = ? AND id = ?`, env, tenant, key.ID); err != nil {
		t.Fatalf("replace secret: %v", err)
	}
	if _, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "orders", map[string]any{"status": "new"}); err != nil {
		t.Fatalf("create order 2: %v", err)
	}
	hits := recv.wait(t, 2)
	if hits[1].auth != "Bearer tok-new" {
		t.Fatalf("second delivery auth = %q; want the token the vault holds NOW (Bearer tok-new)", hits[1].auth)
	}

	// The slot's two deliveries failed at once, named, and nothing reached the receiver for them.
	rows := waitWebhookJobs(t, st, func(rs []webhookJobRow) bool {
		failed, done := 0, 0
		for _, r := range rs {
			switch r.Status {
			case jobs.StatusFailed:
				failed++
			case jobs.StatusDone:
				done++
			}
		}
		return failed == 2 && done == 2
	})
	for _, r := range rows {
		if strings.Contains(r.Payload, "tok-") || !strings.Contains(r.Payload, `"credentialId"`) {
			t.Fatalf("job payload = %s; want the key id and never the key", r.Payload)
		}
		if r.Status == jobs.StatusFailed && (r.Attempts != 1 || !strings.Contains(r.LastError.String, workflow.CodeCredentialNotReady)) {
			t.Fatalf("slot job = attempts %d, last_error %q; want one attempt failed CREDENTIAL_NOT_READY", r.Attempts, r.LastError.String)
		}
	}
	for _, h := range recv.wait(t, 2) {
		if strings.HasPrefix(h.path, "/never") {
			t.Fatalf("the slot without a secret sent a request: %+v", h)
		}
	}
}
