//go:build integration

// specs/050 S3b (tasks T068) — Doctor C10 over a freshly migrated throwaway MySQL DB (o3aSetup), wired
// like cmd/server/main.go: a rule saved before the vault (signing key + Authorization inline, planted
// with the SQL a pre-S3a row holds), a record save that fires it to a receiver, the C10 fix through the
// REAL CredentialService + workflow.SQLRepository (compare-and-swap on sys_workflows.actions), then a
// second save: same path, same Authorization, V1 still the legacy key's HMAC of the body, plus V2.
// A second fix stores no second key. Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=true&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run TestDoctorC10_Integration
package services

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/credential"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

type c10Hit struct {
	uri  string
	hdr  http.Header
	body []byte
}

func TestDoctorC10_Integration(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000c10"
	const secret = "pre-vault-signing-key"
	const bearer = "Bearer partner-token-c10"

	var mu sync.Mutex
	var hits []c10Hit
	recv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits = append(hits, c10Hit{uri: r.URL.RequestURI(), hdr: r.Header.Clone(), body: b})
		mu.Unlock()
	}))
	t.Cleanup(recv.Close)
	waitHits := func(n int) []c10Hit {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			mu.Lock()
			got := append([]c10Hit(nil), hits...)
			mu.Unlock()
			if len(got) >= n {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("receiver saw %d requests, want %d", len(got), n)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	keys := testKeyring(t)
	credSvc := NewCredentialService(credential.NewRepository(gdb), keys, false) // loopback http is allowed
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, time.Hour) // only an invalidation reloads the rule
	sender := workflow.NewHTTPWebhookSender(2*time.Second, 1, 5*time.Millisecond)
	dispatcher := workflow.NewAsyncDispatcher(workflow.DispatcherConfig{Buffer: 16, Workers: 1},
		wfCache, NewWorkflowRecordMutator(dynRepo).WithCatalog(mc), wfRepo, nil).
		WithWebhookSender(sender).
		WithCredentialResolver(credSvc)
	st := &wfStack{
		sdb:         sdb,
		entitySvc:   NewEntityService(dynRepo, metaRepo).WithMetaCache(mc),
		recordSvc:   NewRecordService(mc, dynRepo).WithEngine(dispatcher),
		workflowSvc: NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache).WithCredentialChecker(credSvc),
		dispatcher:  dispatcher,
		cache:       wfCache,
	}
	t.Cleanup(func() { st.drain(t) })
	doc := NewDoctorService(doctor.NewSQLStore(sdb), dynRepo, metaRepo, metaRepo, metadata.NewLayoutRepository(gdb), nil, nil).
		WithOpsCaches(nil, wfCache).
		WithInlineSecretMigration(credSvc, wfRepo, NewEnqueuingWebhookQueue(jobs.NewSQLStore(sdb, 5)))

	st.createEntity(t, ctx, tenant, "orders", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	active := true
	rec, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name: "push-to-erp", EntityName: "orders", Trigger: "after_create", IsActive: &active,
		Actions: []WorkflowActionInput{{Type: "outbound_webhook", URL: recv.URL + "/erp/in?src=nexus"}}, ActorID: user,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	seedLegacyWebhookActions(t, sdb, tenant, rec.ID, []workflow.Action{{Type: workflow.ActionWebhook,
		URL: recv.URL + "/erp/in?src=nexus", Secret: secret, Headers: map[string]string{"Authorization": bearer}}})
	wfCache.InvalidateAll()

	v1 := func(body []byte) string {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(body)
		return hex.EncodeToString(m.Sum(nil))
	}
	if _, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "orders", map[string]any{"status": "new"}); err != nil {
		t.Fatalf("create order 1: %v", err)
	}
	before := waitHits(1)[0]
	if before.hdr.Get("Authorization") != bearer || before.hdr.Get(workflow.SignatureHeader) != v1(before.body) {
		t.Fatalf("legacy delivery not signed with the inline key — fixture broken")
	}

	res, err := doc.Fix(ctx, tenant, []string{DoctorCheckWebhookInlineSecret}, true)
	if err != nil || len(res) != 1 || res[0].Fixed != 1 || res[0].Failed != 0 {
		t.Fatalf("C10 fix = %+v (err %v); want 1 fixed", res, err)
	}
	var stored string
	if err := sdb.Get(&stored, `SELECT CAST(actions AS CHAR) FROM sys_workflows WHERE tenant_id = ? AND id = ?`, tenant, rec.ID); err != nil {
		t.Fatalf("read rule: %v", err)
	}
	for _, gone := range []string{`"secret"`, `"headers"`, secret, "partner-token-c10"} {
		if strings.Contains(stored, gone) {
			t.Fatalf("sys_workflows.actions still carries %s: %s", gone, stored)
		}
	}
	var sealed []struct {
		Name      string  `db:"name"`
		SecretEnc *string `db:"secret_enc"`
		Extra     *string `db:"extra_headers_enc"`
	}
	if err := sdb.Select(&sealed, `SELECT name, secret_enc, extra_headers_enc FROM sys_named_credentials WHERE tenant_id = ? AND deleted_at IS NULL`, tenant); err != nil {
		t.Fatalf("read vault: %v", err)
	}
	if len(sealed) != 1 || sealed[0].SecretEnc == nil || !strings.HasPrefix(*sealed[0].SecretEnc, "v1:") ||
		sealed[0].Extra == nil || strings.Contains(*sealed[0].SecretEnc+*sealed[0].Extra, "partner-token-c10") {
		t.Fatalf("vault rows = %+v; want one key sealed at rest", sealed)
	}

	if _, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "orders", map[string]any{"status": "new"}); err != nil {
		t.Fatalf("create order 2: %v", err)
	}
	after := waitHits(2)[1]
	if after.uri != before.uri || after.hdr.Get("Authorization") != bearer || after.hdr.Get(workflow.SignatureHeader) != v1(after.body) {
		t.Fatalf("after C10: %s Authorization %q V1 ok=%v; want %s, the same Authorization, V1 by the legacy key",
			after.uri, after.hdr.Get("Authorization"), after.hdr.Get(workflow.SignatureHeader) == v1(after.body), before.uri)
	}
	if after.hdr.Get(workflow.SignatureV2Header) == "" || after.hdr.Get(workflow.TimestampHeader) == "" {
		t.Fatalf("after C10 the delivery must carry V2 + timestamp")
	}

	res, err = doc.Fix(ctx, tenant, []string{DoctorCheckWebhookInlineSecret}, true)
	if err != nil || res[0].Fixed != 0 || res[0].Failed != 0 {
		t.Fatalf("second C10 fix = %+v (err %v); want a no-op", res, err)
	}
	var n int
	if err := sdb.Get(&n, `SELECT COUNT(*) FROM sys_named_credentials WHERE tenant_id = ?`, tenant); err != nil || n != 1 {
		t.Fatalf("sys_named_credentials rows = %d (err %v); want still 1", n, err)
	}
}

// TestWorkflowRepository_ReplaceActionsCompareAndSwap pins the write C10 goes through: it swaps the
// actions only while they still equal what the caller read (JSON equality on MySQL's own rendering),
// and tells a changed rule from a missing one.
func TestWorkflowRepository_ReplaceActionsCompareAndSwap(t *testing.T) {
	_, sdb := o3aSetup(t)
	ctx := context.Background()
	repo := workflow.NewSQLRepository(sdb)
	id, err := repo.Create(ctx, workflow.CreateInput{TenantID: o3aTenantA, Name: "r", EntityName: "orders",
		Trigger: workflow.TriggerAfterCreate, Actions: []byte(`[{"type":"outbound_webhook","url":"https://a.example.vn/x"}]`), IsActive: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec, err := repo.Get(ctx, o3aTenantA, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	next := []byte(`[{"type":"outbound_webhook","credentialId":"c-1","path":"/x"}]`)

	if err := repo.ReplaceActions(ctx, o3aTenantA, id, []byte(`[{"type":"outbound_webhook","url":"https://other"}]`), next, ""); !errors.Is(err, workflow.ErrWorkflowChanged) {
		t.Fatalf("stale expect = %v; want ErrWorkflowChanged", err)
	}
	if err := repo.ReplaceActions(ctx, o3aTenantB, id, rec.Actions, next, ""); !errors.Is(err, workflow.ErrWorkflowNotFound) {
		t.Fatalf("other tenant = %v; want ErrWorkflowNotFound", err)
	}
	if err := repo.ReplaceActions(ctx, o3aTenantA, id, rec.Actions, next, "11111111-0000-0000-0000-000000000c10"); err != nil {
		t.Fatalf("ReplaceActions: %v", err)
	}
	got, err := repo.Get(ctx, o3aTenantA, id)
	if err != nil || !strings.Contains(string(got.Actions), `"credentialId": "c-1"`) || got.UpdatedBy == nil {
		t.Fatalf("after swap = %s (updatedBy %v, err %v)", got.Actions, got.UpdatedBy, err)
	}
	// The same expect again no longer matches: a second writer cannot swap twice.
	if err := repo.ReplaceActions(ctx, o3aTenantA, id, rec.Actions, next, ""); !errors.Is(err, workflow.ErrWorkflowChanged) {
		t.Fatalf("second swap on the old expect = %v; want ErrWorkflowChanged", err)
	}
	if err := repo.Delete(ctx, o3aTenantA, id); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceActions(ctx, o3aTenantA, id, got.Actions, next, ""); !errors.Is(err, workflow.ErrWorkflowNotFound) {
		t.Fatalf("deleted rule = %v; want ErrWorkflowNotFound", err)
	}
}

// TestDoctorC10_WipesDeletedRules_Integration (specs/050 S3b review): a soft-deleted rule keeps its
// actions in sys_workflows — a pre-vault key included — with no purge and no restore. C10 must count it
// and wipe the key from the row (tenant-pinned, no master key needed), and the repository's deleted-row
// reads/writes must never reach a live row or another tenant's.
func TestDoctorC10_WipesDeletedRules_Integration(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	const secret = "pre-vault-signing-key-deleted"
	const bearer = "Bearer partner-token-deleted"
	repo := workflow.NewSQLRepository(sdb)
	mk := func(tenant, name string) string {
		t.Helper()
		id, err := repo.Create(ctx, workflow.CreateInput{TenantID: tenant, Name: name, EntityName: "orders",
			Trigger: workflow.TriggerAfterCreate, Actions: []byte(`[{"type":"outbound_webhook","url":"https://x"}]`), IsActive: true})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		seedLegacyWebhookActions(t, sdb, tenant, id, []workflow.Action{{Type: workflow.ActionWebhook,
			URL: "https://erp.example.vn/in?token=q-token-deleted", Secret: secret, Headers: map[string]string{"Authorization": bearer}}})
		return id
	}
	deletedA, deletedB, liveA := mk(o3aTenantA, "gone-a"), mk(o3aTenantB, "gone-b"), mk(o3aTenantA, "live-a")
	for _, d := range []struct{ tenant, id string }{{o3aTenantA, deletedA}, {o3aTenantB, deletedB}} {
		if err := repo.Delete(ctx, d.tenant, d.id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	actionsOf := func(id string) string {
		t.Helper()
		var s string
		if err := sdb.Get(&s, `SELECT CAST(actions AS CHAR) FROM sys_workflows WHERE id = ?`, id); err != nil {
			t.Fatalf("read actions: %v", err)
		}
		return s
	}
	liveBefore, otherBefore := actionsOf(liveA), actionsOf(deletedB)

	// The repository's deleted-row seam: tenant-pinned, never a live row.
	if _, err := repo.GetDeleted(ctx, o3aTenantA, liveA); !errors.Is(err, workflow.ErrWorkflowNotFound) {
		t.Fatalf("GetDeleted(live) = %v; want ErrWorkflowNotFound", err)
	}
	if _, err := repo.GetDeleted(ctx, o3aTenantA, deletedB); !errors.Is(err, workflow.ErrWorkflowNotFound) {
		t.Fatalf("GetDeleted(other tenant) = %v; want ErrWorkflowNotFound", err)
	}
	for _, id := range []string{liveA, deletedB} {
		if err := repo.ReplaceDeletedActions(ctx, o3aTenantA, id, []byte(`[]`)); !errors.Is(err, workflow.ErrWorkflowNotFound) {
			t.Fatalf("ReplaceDeletedActions(%s) = %v; want ErrWorkflowNotFound", id, err)
		}
	}
	if got, err := repo.ListDeleted(ctx, o3aTenantA); err != nil || len(got) != 1 || got[0].ID != deletedA {
		t.Fatalf("ListDeleted(A) = %+v, %v; want only %s", got, err, deletedA)
	}

	// No master key: the wipe needs none; the live rule is refused and left as is.
	doc := NewDoctorService(doctor.NewSQLStore(sdb), dynrepo.New(sdb), metadata.NewRepository(gdb), metadata.NewRepository(gdb),
		metadata.NewLayoutRepository(gdb), nil, nil).
		WithInlineSecretMigration(NewCredentialService(credential.NewRepository(gdb), nil, false), repo, NewEnqueuingWebhookQueue(jobs.NewSQLStore(sdb, 5)))
	res, err := doc.Fix(ctx, o3aTenantA, []string{DoctorCheckWebhookInlineSecret}, true)
	if err != nil || len(res) != 1 || res[0].Fixed != 1 || res[0].Failed != 1 {
		t.Fatalf("C10 fix = %+v (err %v); want the deleted rule wiped, the live one refused (no master key)", res, err)
	}
	wiped := actionsOf(deletedA)
	for _, gone := range []string{`"secret"`, `"headers"`, secret, "partner-token-deleted", "q-token-deleted", "/in"} {
		if strings.Contains(wiped, gone) {
			t.Fatalf("deleted rule still holds %s: %s", gone, wiped)
		}
	}
	if !strings.Contains(wiped, `"url": "https://erp.example.vn"`) {
		t.Fatalf("deleted rule = %s; want the URL down to its origin", wiped)
	}
	var stillDeleted int
	if err := sdb.Get(&stillDeleted, `SELECT COUNT(*) FROM sys_workflows WHERE id = ? AND deleted_at IS NOT NULL`, deletedA); err != nil || stillDeleted != 1 {
		t.Fatalf("the wiped rule must stay deleted (%d, %v)", stillDeleted, err)
	}
	if actionsOf(liveA) != liveBefore || actionsOf(deletedB) != otherBefore {
		t.Fatalf("a live rule or another tenant's deleted rule was touched")
	}
	res, err = doc.Fix(ctx, o3aTenantA, []string{DoctorCheckWebhookInlineSecret}, true)
	if err != nil || res[0].Fixed != 0 {
		t.Fatalf("second fix = %+v (err %v); want nothing left to wipe", res, err)
	}
}
