package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"nexus/platform/internal/credential"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/workflow"
)

// specs/050 S3b (tasks T067): Doctor C10 moves the signing keys and auth headers that rules saved
// before the vault carry in plaintext into the vault — key first, rule after, one rule at a time,
// reusing the key a previous (failed or concurrent) run already stored, and never touching an action
// whose credential sits in its URL. What the partner receives must not change: the same URL, the same
// headers, the same V1 byte for byte.

// c10Rules is the rule store C10 reads and rewrites: tenant-pinned, compare-and-swap on the actions,
// with a per-rule write failure to inject.
type c10Rules struct {
	mu        sync.Mutex
	rows      map[string]*workflow.WorkflowRecord
	deleted   map[string]*workflow.WorkflowRecord // soft-deleted rows (deleted_at set)
	failWrite map[string]error
	writes    int
	// racer, when set, runs inside ReplaceActions before the compare — another writer saving the
	// rule between C10's read and its write.
	racer func(w *workflow.WorkflowRecord)
}

func newC10Rules() *c10Rules {
	return &c10Rules{rows: map[string]*workflow.WorkflowRecord{}, deleted: map[string]*workflow.WorkflowRecord{}, failWrite: map[string]error{}}
}

func (r *c10Rules) ListDeleted(_ context.Context, tenantID string) ([]workflow.WorkflowRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []workflow.WorkflowRecord{}
	for _, w := range r.deleted {
		if w.TenantID == tenantID {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *c10Rules) GetDeleted(_ context.Context, tenantID, id string) (*workflow.WorkflowRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.deleted[id]
	if !ok || w.TenantID != tenantID {
		return nil, workflow.ErrWorkflowNotFound
	}
	cp := *w
	return &cp, nil
}

func (r *c10Rules) ReplaceDeletedActions(_ context.Context, tenantID, id string, next []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.deleted[id]
	if !ok || w.TenantID != tenantID {
		return workflow.ErrWorkflowNotFound
	}
	w.Actions = append(json.RawMessage(nil), next...)
	return nil
}

// seedDeleted stores a rule and soft-deletes it (the admin's DELETE: the row and its actions stay).
func (r *c10Rules) seedDeleted(t *testing.T, tenantID, name string, acts ...workflow.Action) string {
	t.Helper()
	id := r.seed(t, tenantID, name, "orders", acts...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted[id] = r.rows[id]
	delete(r.rows, id)
	return id
}

func (r *c10Rules) deletedRaw(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.deleted[id].Actions)
}

func (r *c10Rules) List(_ context.Context, tenantID string) ([]workflow.WorkflowRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []workflow.WorkflowRecord{}
	for _, w := range r.rows {
		if w.TenantID == tenantID {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *c10Rules) Get(_ context.Context, tenantID, id string) (*workflow.WorkflowRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.rows[id]
	if !ok || w.TenantID != tenantID {
		return nil, workflow.ErrWorkflowNotFound
	}
	cp := *w
	return &cp, nil
}

func (r *c10Rules) ReplaceActions(_ context.Context, tenantID, id string, expect, next []byte, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failWrite[id]; err != nil {
		return err
	}
	w, ok := r.rows[id]
	if !ok || w.TenantID != tenantID {
		return workflow.ErrWorkflowNotFound
	}
	if r.racer != nil {
		r.racer(w)
		r.racer = nil
	}
	if !bytes.Equal(w.Actions, expect) {
		return workflow.ErrWorkflowChanged
	}
	w.Actions = append(json.RawMessage(nil), next...)
	r.writes++
	return nil
}

func (r *c10Rules) seed(t *testing.T, tenantID, name, entity string, acts ...workflow.Action) string {
	t.Helper()
	raw, err := json.Marshal(acts)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id := fmt.Sprintf("wf-%d", len(r.rows)+len(r.deleted)+1)
	r.rows[id] = &workflow.WorkflowRecord{ID: id, TenantID: tenantID, Name: name, EntityName: entity,
		Trigger: "after_create", Actions: raw, IsActive: true}
	return id
}

func (r *c10Rules) actions(t *testing.T, id string) []workflow.Action {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var acts []workflow.Action
	if err := json.Unmarshal(r.rows[id].Actions, &acts); err != nil {
		t.Fatalf("decode actions of %s: %v", id, err)
	}
	return acts
}

func (r *c10Rules) raw(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.rows[id].Actions)
}

// flakyCredStore fails Create for the rows failCreate picks (the database dropping mid-run).
type flakyCredStore struct {
	*fakeCredentialStore
	failCreate func(c *credential.Credential) bool
}

func (f *flakyCredStore) Create(ctx context.Context, c *credential.Credential) error {
	if f.failCreate != nil && f.failCreate(c) {
		return errors.New("db: connection lost")
	}
	return f.fakeCredentialStore.Create(ctx, c)
}

// c10Queue is the durable queue's probes, per rule: deliveries still waiting, and failed deliveries a
// "Gửi lại" could still send, counted by action slot.
type c10Queue struct {
	pending map[string]bool
	failed  map[string]map[int]int
}

func (q *c10Queue) HasPendingWebhooks(_ context.Context, _, workflowID string) (bool, error) {
	return q.pending[workflowID], nil
}

func (q *c10Queue) FailedWebhookSlots(_ context.Context, _, workflowID string) (map[int]int, error) {
	return q.failed[workflowID], nil
}

type c10Fixture struct {
	svc     *DoctorService
	creds   *CredentialService
	store   *flakyCredStore
	rules   *c10Rules
	queue   *c10Queue
	pending map[string]bool // queue.pending
}

func newC10Fixture(t *testing.T, withKeyring, allowInsecureHTTP bool) *c10Fixture {
	t.Helper()
	store := &flakyCredStore{fakeCredentialStore: newFakeCredentialStore()}
	keys := testKeyring(t)
	if !withKeyring {
		keys = nil
	}
	creds := NewCredentialService(store, keys, allowInsecureHTTP)
	rules := newC10Rules()
	queue := &c10Queue{pending: map[string]bool{}, failed: map[string]map[int]int{}}
	_, _, svc := newDoctorFixture()
	svc.WithInlineSecretMigration(creds, rules, queue)
	return &c10Fixture{svc: svc, creds: creds, store: store, rules: rules, queue: queue, pending: queue.pending}
}

func (f *c10Fixture) fix(t *testing.T) doctor.FixResult {
	t.Helper()
	res, err := f.svc.Fix(context.Background(), credTenantA, []string{DoctorCheckWebhookInlineSecret}, true)
	if err != nil || len(res) != 1 {
		t.Fatalf("Fix C10 = %+v, %v; want one result", res, err)
	}
	return res[0]
}

func (f *c10Fixture) scan(t *testing.T) doctor.CheckResult {
	t.Helper()
	got, ok := scanByID(t, f.svc, credTenantA)[DoctorCheckWebhookInlineSecret]
	if !ok {
		t.Fatalf("C10 not in the scan — not registered")
	}
	return got
}

// liveCreds lists the tenant's live keys by name.
func (f *c10Fixture) liveCreds(t *testing.T) []credential.Credential {
	t.Helper()
	rows, err := f.store.List(context.Background(), credTenantA)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// capture is a partner endpoint recording what it received.
type capture struct {
	mu   sync.Mutex
	reqs []*http.Request
	srv  *httptest.Server
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	c := &capture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.reqs = append(c.reqs, r.Clone(context.Background()))
		c.mu.Unlock()
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *capture) last(t *testing.T) *http.Request {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		t.Fatalf("the partner received nothing")
	}
	return c.reqs[len(c.reqs)-1]
}

const (
	c10Secret = "legacy-signing-key-0123456789abcdef"
	c10Bearer = "Bearer tok-9f3a-legacy"
	c10APIKey = "k-77-legacy"
)

func c10Legacy(url string) workflow.Action {
	return workflow.Action{Type: workflow.ActionWebhook, URL: url, Secret: c10Secret,
		Headers: map[string]string{"Authorization": c10Bearer, "X-Api-Key": c10APIKey}}
}

func TestDoctorC10_MigratesInlineSecretPreservingWireBytes(t *testing.T) {
	f := newC10Fixture(t, true, false)
	partner := newCapture(t)
	legacy := c10Legacy(partner.srv.URL + "/hook?src=nexus")
	notify := workflow.Action{Type: workflow.ActionInAppNotification, UserID: "u-1", Message: "đơn mới"}
	id := f.rules.seed(t, credTenantA, "Gửi đơn sang ERP", "orders", notify, legacy)

	got := f.scan(t)
	if got.Count != 1 || got.Items[0].Key != id || got.AutoFixSafe || got.ReportOnly {
		t.Fatalf("scan = %+v; want one needs-confirm finding for %s", got, id)
	}
	detail := got.Items[0].Detail
	for _, leak := range []string{c10Secret, c10Bearer, "tok-9f3a", c10APIKey, "/hook", "src=nexus"} {
		if strings.Contains(detail, leak) {
			t.Fatalf("finding detail leaks %q: %s", leak, detail)
		}
	}
	for _, must := range []string{"một chiều", "chuyển hướng", "V1"} {
		if !strings.Contains(detail, must) {
			t.Fatalf("finding detail must state %q (one-way step, no redirects, V1 kept): %s", must, detail)
		}
	}

	// What the partner receives today, for one body.
	body := []byte(`{"workflowId":"wf-1","recordId":"r-1"}`)
	sender := workflow.NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond)
	if err := sender.Post(context.Background(), workflow.WebhookRequestFromURLAction(legacy, body)); err != nil {
		t.Fatalf("legacy send: %v", err)
	}
	before := partner.last(t)

	res := f.fix(t)
	if res.Fixed != 1 || res.Failed != 0 {
		t.Fatalf("fix = %+v; want 1 fixed", res)
	}
	stored := f.rules.raw(id)
	for _, gone := range []string{`"secret"`, `"headers"`, `"url"`, c10Secret, "tok-9f3a", c10APIKey} {
		if strings.Contains(stored, gone) {
			t.Fatalf("rule still carries %s after C10: %s", gone, stored)
		}
	}
	acts := f.rules.actions(t, id)
	if len(acts) != 2 || acts[0].Type != workflow.ActionInAppNotification || acts[0].Message != "đơn mới" {
		t.Fatalf("the other action was touched: %+v", acts)
	}
	moved := acts[1]
	if moved.CredentialID == "" || moved.Path != "/hook?src=nexus" {
		t.Fatalf("migrated action = %+v; want credentialId + the url's path and query", moved)
	}
	keys := f.liveCreds(t)
	if len(keys) != 1 || keys[0].ID != moved.CredentialID || keys[0].AuthType != credential.AuthHMAC ||
		!strings.HasSuffix(keys[0].Label, CredentialLegacyLabelSuffix) || keys[0].SecretEnc == nil ||
		strings.Contains(*keys[0].SecretEnc, c10Secret) {
		t.Fatalf("vault = %+v; want one sealed hmac key labelled as moved from a rule", keys)
	}

	// What the partner receives after C10, for the same body, through the send path.
	req, err := workflow.WebhookRequestFor(context.Background(), f.creds, credTenantA, moved, body)
	if err != nil {
		t.Fatalf("resolve migrated action: %v", err)
	}
	if !req.NoRedirect {
		t.Fatalf("a vault request must not follow redirects")
	}
	if err := sender.Post(context.Background(), req); err != nil {
		t.Fatalf("migrated send: %v", err)
	}
	after := partner.last(t)
	if before.URL.RequestURI() != after.URL.RequestURI() || before.Host != after.Host {
		t.Fatalf("target moved: %s%s ⇒ %s%s", before.Host, before.URL.RequestURI(), after.Host, after.URL.RequestURI())
	}
	for _, h := range []string{"Authorization", "X-Api-Key", workflow.SignatureHeader, "Content-Type"} {
		if b, a := before.Header.Get(h), after.Header.Get(h); b == "" || b != a {
			t.Fatalf("%s: before %q, after %q; want the same non-empty bytes", h, b, a)
		}
	}
	if after.Header.Get(workflow.SignatureV2Header) == "" || after.Header.Get(workflow.TimestampHeader) == "" {
		t.Fatalf("after C10 the request must also carry V2 + timestamp")
	}
}

func TestDoctorC10_ConfirmGate(t *testing.T) {
	f := newC10Fixture(t, true, false)
	id := f.rules.seed(t, credTenantA, "r", "orders", c10Legacy("https://pay.example.vn/hook"))
	before := f.rules.raw(id)

	_, err := f.svc.Fix(context.Background(), credTenantA, []string{DoctorCheckWebhookInlineSecret}, false)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "DOCTOR_CONFIRM_REQUIRED" {
		t.Fatalf("fix without confirm = %v; want DOCTOR_CONFIRM_REQUIRED", err)
	}
	if f.rules.raw(id) != before || len(f.liveCreds(t)) != 0 {
		t.Fatalf("a refused fix changed something: rule %s, %d key(s)", f.rules.raw(id), len(f.liveCreds(t)))
	}
}

func TestDoctorC10_GroupsIdenticalCredentials(t *testing.T) {
	f := newC10Fixture(t, true, false)
	a := f.rules.seed(t, credTenantA, "a", "orders", c10Legacy("https://pay.example.vn/orders"))
	b := f.rules.seed(t, credTenantA, "b", "invoices", c10Legacy("https://pay.example.vn/invoices?v=2"))
	other := c10Legacy("https://pay.example.vn/refunds")
	other.Secret = "another-signing-key-0123456789abcd"
	c := f.rules.seed(t, credTenantA, "c", "refunds", other)

	if res := f.fix(t); res.Fixed != 3 || res.Failed != 0 {
		t.Fatalf("fix = %+v; want 3 fixed", res)
	}
	keys := f.liveCreds(t)
	names := []string{}
	for _, k := range keys {
		names = append(names, k.Name)
	}
	if len(keys) != 2 || names[0] != "webhook-pay-example-vn" || names[1] != "webhook-pay-example-vn-2" {
		t.Fatalf("vault = %v; want one key per (origin, secret, headers) group", names)
	}
	ida, idb, idc := f.rules.actions(t, a)[0].CredentialID, f.rules.actions(t, b)[0].CredentialID, f.rules.actions(t, c)[0].CredentialID
	if ida == "" || ida != idb || idc == ida || idc == "" {
		t.Fatalf("rules point at %q %q %q; want a and b sharing one key, c its own", ida, idb, idc)
	}
}

func TestDoctorC10_RefusesWithoutKeyring(t *testing.T) {
	f := newC10Fixture(t, false, false)
	id := f.rules.seed(t, credTenantA, "r", "orders", c10Legacy("https://pay.example.vn/hook"))
	before := f.rules.raw(id)

	if got := f.scan(t); got.Count != 1 {
		t.Fatalf("scan without a master key = %+v; want the finding (scanning needs no key)", got)
	}
	res := f.fix(t)
	if res.Fixed != 0 || res.Failed != 1 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "CREDENTIALS_NOT_CONFIGURED") {
		t.Fatalf("fix without a master key = %+v; want one CREDENTIALS_NOT_CONFIGURED failure", res)
	}
	if f.rules.raw(id) != before || f.rules.writes != 0 || len(f.store.rows) != 0 {
		t.Fatalf("a fix without a master key changed something: %d rule write(s), %d key row(s)", f.rules.writes, len(f.store.rows))
	}
}

// Rule 1 and rule 2 use different keys, so each run creates its own.
func seedTwoLegacyRules(t *testing.T, f *c10Fixture) (string, string) {
	t.Helper()
	one := f.rules.seed(t, credTenantA, "1 orders", "orders", c10Legacy("https://pay.example.vn/orders"))
	second := c10Legacy("https://crm.example.vn/leads")
	second.Secret = "second-signing-key-0123456789abcdef"
	two := f.rules.seed(t, credTenantA, "2 leads", "leads", second)
	return one, two
}

func TestDoctorC10_CredentialPersistedBeforeRewrite(t *testing.T) {
	t.Run("rule write fails ⇒ the key stays for the next run, the rule keeps its inline key", func(t *testing.T) {
		f := newC10Fixture(t, true, false)
		one, two := seedTwoLegacyRules(t, f)
		before := f.rules.raw(two)
		f.rules.failWrite[two] = errors.New("db: connection lost")

		res := f.fix(t)
		if res.Fixed != 1 || res.Failed != 1 || len(res.Errors) != 1 || !strings.HasPrefix(res.Errors[0], two+": ") {
			t.Fatalf("fix = %+v; want partial: rule %s fixed, rule %s failed", res, one, two)
		}
		if f.rules.actions(t, one)[0].CredentialID == "" {
			t.Fatalf("rule 1 was not moved")
		}
		if f.rules.raw(two) != before {
			t.Fatalf("rule 2 lost its inline key: %s", f.rules.raw(two))
		}
		if n := len(f.liveCreds(t)); n != 2 {
			t.Fatalf("%d key(s) stored; want both — rule 2's key is stored before its rule is rewritten", n)
		}
	})
	t.Run("key write fails ⇒ the rule is never pointed at a key that does not exist", func(t *testing.T) {
		f := newC10Fixture(t, true, false)
		one, two := seedTwoLegacyRules(t, f)
		before := f.rules.raw(two)
		f.store.failCreate = func(c *credential.Credential) bool { return strings.Contains(c.BaseURL, "crm.example.vn") }

		res := f.fix(t)
		if res.Fixed != 1 || res.Failed != 1 {
			t.Fatalf("fix = %+v; want partial", res)
		}
		if f.rules.actions(t, one)[0].CredentialID == "" {
			t.Fatalf("rule 1 was not moved")
		}
		if f.rules.raw(two) != before {
			t.Fatalf("rule 2 was rewritten although its key was never stored: %s", f.rules.raw(two))
		}
	})
}

func TestDoctorC10_RerunReusesCredential(t *testing.T) {
	f := newC10Fixture(t, true, false)
	_, two := seedTwoLegacyRules(t, f)
	f.rules.failWrite[two] = errors.New("db: connection lost")
	if res := f.fix(t); res.Fixed != 1 || res.Failed != 1 {
		t.Fatalf("first run = %+v; want partial", res)
	}
	stored := map[string]string{} // base URL → key id stored by the failed run
	for _, k := range f.liveCreds(t) {
		stored[k.BaseURL] = k.ID
	}

	delete(f.rules.failWrite, two)
	if res := f.fix(t); res.Fixed != 1 || res.Failed != 0 {
		t.Fatalf("re-run = %+v; want rule 2 fixed", res)
	}
	keys := f.liveCreds(t)
	if len(keys) != 2 {
		t.Fatalf("%d key(s) after the re-run; want 2 — the re-run must reuse the key it stored before", len(keys))
	}
	if got := f.rules.actions(t, two)[0].CredentialID; got != stored["https://crm.example.vn"] {
		t.Fatalf("rule 2 points at %q; want the key the failed run stored (%q)", got, stored["https://crm.example.vn"])
	}
	// A third run finds nothing to move and stores nothing.
	if res := f.fix(t); res.Fixed != 0 || res.Failed != 0 || len(f.liveCreds(t)) != 2 {
		t.Fatalf("third run = %+v with %d key(s); want a no-op", res, len(f.liveCreds(t)))
	}
}

func TestDoctorC10_ShortLegacySecretMigrates(t *testing.T) {
	f := newC10Fixture(t, true, false)
	partner := newCapture(t)
	legacy := workflow.Action{Type: workflow.ActionWebhook, URL: partner.srv.URL + "/in", Secret: "short-key-12"}
	id := f.rules.seed(t, credTenantA, "r", "orders", legacy)
	if d := f.scan(t).Items[0].Detail; !strings.Contains(d, "xoay") {
		t.Fatalf("detail must advise rotating a short key: %s", d)
	}
	if res := f.fix(t); res.Fixed != 1 {
		t.Fatalf("fix = %+v; want the 12-character key moved (no CREDENTIAL_SECRET_WEAK on import)", res)
	}
	body := []byte(`{"x":1}`)
	sender := workflow.NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond)
	if err := sender.Post(context.Background(), workflow.WebhookRequestFromURLAction(legacy, body)); err != nil {
		t.Fatal(err)
	}
	v1 := partner.last(t).Header.Get(workflow.SignatureHeader)
	req, err := workflow.WebhookRequestFor(context.Background(), f.creds, credTenantA, f.rules.actions(t, id)[0], body)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Post(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := partner.last(t).Header.Get(workflow.SignatureHeader); got != v1 || v1 == "" {
		t.Fatalf("V1 %q ⇒ %q; want the same bytes", v1, got)
	}
}

func TestDoctorC10_HostSlugName(t *testing.T) {
	long := strings.Repeat("sub.", 50) + "example.vn"
	for _, tc := range []struct {
		host string
		n    int
		want string
	}{
		{"pay.example.vn", 1, "webhook-pay-example-vn"},
		{"pay.example.vn:8443", 1, "webhook-pay-example-vn-8443"},
		{"Pay.Example.VN", 1, "webhook-pay-example-vn"},
		{"pay.example.vn", 2, "webhook-pay-example-vn-2"},
		{"127.0.0.1:9090", 1, "webhook-127-0-0-1-9090"},
		{"[::1]:8080", 1, "webhook-1-8080"},
		{"xn--bcher-kva.vn", 1, "webhook-xn-bcher-kva-vn"},
		{long, 1, ""},
		{long, 2, ""},
		{long, 20, ""},
	} {
		got := legacyCredentialName(tc.host, tc.n)
		if tc.want != "" && got != tc.want {
			t.Errorf("legacyCredentialName(%q, %d) = %q; want %q", tc.host, tc.n, got, tc.want)
		}
		if err := credential.ValidateName(got); err != nil || utf8.RuneCountInString(got) > 64 {
			t.Errorf("legacyCredentialName(%q, %d) = %q (%d chars): %v; want a valid name ≤ 64", tc.host, tc.n, got, len(got), err)
		}
		if tc.n > 1 && !strings.HasSuffix(got, fmt.Sprintf("-%d", tc.n)) {
			t.Errorf("legacyCredentialName(%q, %d) = %q; want the -%d suffix kept", tc.host, tc.n, got, tc.n)
		}
	}
	// The label names the host, keeps the reuse marker and fits the column.
	if l := legacyCredentialLabel(long); utf8.RuneCountInString(l) > credentialLabelMax || !strings.HasSuffix(l, CredentialLegacyLabelSuffix) {
		t.Fatalf("legacyCredentialLabel(long) = %q (%d runes)", l, utf8.RuneCountInString(l))
	}
}

func TestDoctorC10_SkipsInsecureHTTP(t *testing.T) {
	f := newC10Fixture(t, true, false)
	id := f.rules.seed(t, credTenantA, "r", "orders", c10Legacy("http://partner.example.vn/hook"))
	before := f.rules.raw(id)

	got := f.scan(t)
	if got.Count != 1 || !strings.Contains(got.Items[0].Detail, "https") {
		t.Fatalf("scan = %+v; want the action listed with 'switch to https'", got)
	}
	if res := f.fix(t); res.Fixed != 0 || res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("fix = %+v; want the plain-http action skipped", res)
	}
	if f.rules.raw(id) != before || len(f.liveCreds(t)) != 0 {
		t.Fatalf("a plain-http action was moved: %s", f.rules.raw(id))
	}

	// CREDENTIAL_ALLOW_INSECURE_HTTP (dev / internal network) lets the vault hold it, so C10 moves it.
	g := newC10Fixture(t, true, true)
	g.rules.seed(t, credTenantA, "r", "orders", c10Legacy("http://partner.example.vn/hook"))
	if res := g.fix(t); res.Fixed != 1 {
		t.Fatalf("fix with CREDENTIAL_ALLOW_INSECURE_HTTP = %+v; want it moved", res)
	}
}

func TestDoctorC10_ReportsURLEmbeddedCredentials(t *testing.T) {
	f := newC10Fixture(t, true, false)
	secretsInURLs := []string{"uname-5c1", "pw-5c1", "123456:AAE-tg", "XyZslack5c1", "gkey-5c1", "gtok-5c1", "qtok-5c1"}
	keyedQuery := c10Legacy("https://api.example.vn/hook?token=qtok-5c1")
	id := f.rules.seed(t, credTenantA, "r", "orders",
		workflow.Action{Type: workflow.ActionWebhook, URL: "https://uname-5c1:pw-5c1@erp.example.vn/in"},
		workflow.Action{Type: workflow.ActionWebhook, URL: "https://api.telegram.org/bot123456:AAE-tg/sendMessage"},
		workflow.Action{Type: workflow.ActionWebhook, URL: "https://hooks.slack.com/services/T0/B0/XyZslack5c1"},
		workflow.Action{Type: workflow.ActionWebhook, URL: "https://chat.googleapis.com/v1/spaces/S/messages?key=gkey-5c1&token=gtok-5c1"},
		keyedQuery,
		workflow.Action{Type: workflow.ActionWebhook, URL: "https://plain.example.vn/no-key"}, // nothing to move, nothing to report
	)
	before := f.rules.actions(t, id)

	got := f.scan(t)
	if got.Count != 1 {
		t.Fatalf("scan = %+v; want one finding for the rule", got)
	}
	detail := got.Items[0].Detail
	for _, leak := range append(secretsInURLs, c10Secret, "tok-9f3a", c10APIKey) {
		if strings.Contains(detail, leak) {
			t.Fatalf("finding detail leaks %q: %s", leak, detail)
		}
	}
	for _, must := range []string{"#1", "#2", "#3", "#4", "#5", "basic", "đường dẫn", "query"} {
		if !strings.Contains(detail, must) {
			t.Fatalf("finding detail must mention %q: %s", must, detail)
		}
	}
	if strings.Contains(detail, "#6") {
		t.Fatalf("an action with no key of its own was reported: %s", detail)
	}

	if res := f.fix(t); res.Fixed != 1 || res.Failed != 0 {
		t.Fatalf("fix = %+v; want only the keyed action with a query moved", res)
	}
	after := f.rules.actions(t, id)
	for i := 0; i < 4; i++ {
		if after[i].URL != before[i].URL || after[i].CredentialID != "" {
			t.Fatalf("action #%d (credential in its URL) was touched: %+v", i+1, after[i])
		}
	}
	if after[4].CredentialID == "" || after[4].Path != "/hook?token=qtok-5c1" || after[5].URL != before[5].URL {
		t.Fatalf("actions after C10 = %+v", after)
	}

	// Still listed for the manual move; a second run moves nothing and reports it skipped.
	if got := f.scan(t); got.Count != 1 || strings.Contains(got.Items[0].Detail, "#5") {
		t.Fatalf("re-scan = %+v; want only the manual actions left", got)
	}
	if res := f.fix(t); res.Fixed != 0 || res.Skipped != 1 || len(f.liveCreds(t)) != 1 {
		t.Fatalf("second run = %+v; want skipped, no new key", res)
	}
}

func TestDoctorC10_StaleFindingSkipped(t *testing.T) {
	f := newC10Fixture(t, true, false)
	edited := f.rules.seed(t, credTenantA, "edited", "orders", c10Legacy("https://pay.example.vn/a"))
	deleted := f.rules.seed(t, credTenantA, "deleted", "orders", c10Legacy("https://pay.example.vn/b"))
	if got := f.scan(t); got.Count != 2 {
		t.Fatalf("scan = %+v; want 2", got)
	}
	// Between the scan and the fix: one rule moved to a vault key by hand, the other deleted.
	f.rules.rows[edited].Actions = json.RawMessage(`[{"type":"outbound_webhook","credentialId":"cred-by-hand","path":"/a"}]`)
	delete(f.rules.rows, deleted)

	finding := doctor.Finding{Key: edited}
	for _, ck := range f.svc.checks() {
		if ck.ID != DoctorCheckWebhookInlineSecret {
			continue
		}
		for _, key := range []string{edited, deleted} {
			finding.Key = key
			if err := ck.Fix(context.Background(), credTenantA, finding); !errors.Is(err, doctor.ErrAlreadyClean) {
				t.Fatalf("Fix(stale %s) = %v; want ErrAlreadyClean", key, err)
			}
		}
	}
	if len(f.liveCreds(t)) != 0 || f.rules.writes != 0 {
		t.Fatalf("a stale finding stored %d key(s) / %d write(s)", len(f.liveCreds(t)), f.rules.writes)
	}
}

func TestDoctorC10_ConcurrentSaveNeverOverwritten(t *testing.T) {
	t.Run("another run moved it meanwhile ⇒ skipped", func(t *testing.T) {
		f := newC10Fixture(t, true, false)
		id := f.rules.seed(t, credTenantA, "r", "orders", c10Legacy("https://pay.example.vn/hook"))
		moved := json.RawMessage(`[{"type":"outbound_webhook","credentialId":"by-the-other-run","path":"/hook"}]`)
		f.rules.racer = func(w *workflow.WorkflowRecord) { w.Actions = moved }
		if res := f.fix(t); res.Fixed != 0 || res.Skipped != 1 || res.Failed != 0 {
			t.Fatalf("fix = %+v; want skipped", res)
		}
		if f.rules.raw(id) != string(moved) {
			t.Fatalf("the other run's save was overwritten: %s", f.rules.raw(id))
		}
	})
	t.Run("the builder saved an unrelated change meanwhile ⇒ failed, run again", func(t *testing.T) {
		f := newC10Fixture(t, true, false)
		id := f.rules.seed(t, credTenantA, "r", "orders", c10Legacy("https://pay.example.vn/hook"),
			workflow.Action{Type: workflow.ActionInAppNotification, UserID: "u-1", Message: "cũ"})
		var edited string
		f.rules.racer = func(w *workflow.WorkflowRecord) {
			w.Actions = json.RawMessage(strings.Replace(string(w.Actions), `"cũ"`, `"mới"`, 1))
			edited = string(w.Actions)
		}
		res := f.fix(t)
		if res.Fixed != 0 || res.Failed != 1 || !strings.Contains(res.Errors[0], "chạy lại") {
			t.Fatalf("fix = %+v; want one failure asking to run again", res)
		}
		if f.rules.raw(id) != edited {
			t.Fatalf("the concurrent save was overwritten: %s", f.rules.raw(id))
		}
		// The re-run reuses the key the failed run stored and moves it.
		if res := f.fix(t); res.Fixed != 1 || len(f.liveCreds(t)) != 1 || f.rules.actions(t, id)[1].Message != "mới" {
			t.Fatalf("re-run = %+v, %d key(s), actions %s", res, len(f.liveCreds(t)), f.rules.raw(id))
		}
	})
}

// A delivery parked in the durable queue before the move names no key; after the move its slot names
// one, and the job handler drops it for good ("rule đã đổi" — and "Gửi lại" fails the same way). So a
// rule with deliveries still waiting is not moved yet.
func TestDoctorC10_WaitsForPendingDeliveries(t *testing.T) {
	f := newC10Fixture(t, true, false)
	id := f.rules.seed(t, credTenantA, "r", "orders", c10Legacy("https://pay.example.vn/hook"))
	before := f.rules.raw(id)
	f.pending[id] = true

	res := f.fix(t)
	if res.Fixed != 0 || res.Failed != 1 || !strings.Contains(res.Errors[0], "Việc nền") {
		t.Fatalf("fix with deliveries waiting = %+v; want refused, pointing at Việc nền", res)
	}
	// A rule that fires all the time almost always has a delivery waiting, so "run again when the
	// queue is done" may never come true: the refusal must also say how to get there (review #453).
	if !strings.Contains(res.Errors[0], "tạm tắt") || !strings.Contains(res.Errors[0], "không được gửi") {
		t.Fatalf("refusal = %q; want the way out: pause the rule, wait for the queue, run C10, re-enable (events while paused are not sent)", res.Errors[0])
	}
	if f.rules.raw(id) != before || len(f.liveCreds(t)) != 0 {
		t.Fatalf("a rule with waiting deliveries was touched: %s, %d key(s)", f.rules.raw(id), len(f.liveCreds(t)))
	}
	delete(f.pending, id)
	if res := f.fix(t); res.Fixed != 1 {
		t.Fatalf("fix once the queue drained = %+v; want moved", res)
	}
}

// A failed delivery keeps its payload for JOBS_RETENTION_DAYS and "Gửi lại" (POST /admin/jobs/:id/retry)
// can still send it — until its slot names a key: then the job handler fails it for good ("rule đã
// đổi"). Refusing like a pending delivery would hold C10 for up to 30 days, so the finding (which the
// confirm strip sits next to, and which the fix audits) says how many of each moved action's failed
// deliveries lose their resend — and only those: another slot's failures keep theirs.
func TestDoctorC10_WarnsFailedDeliveriesLoseResend(t *testing.T) {
	f := newC10Fixture(t, true, false)
	notify := workflow.Action{Type: workflow.ActionInAppNotification, UserID: "u-1", Message: "x"}
	failing := f.rules.seed(t, credTenantA, "a-failing", "orders", notify, c10Legacy("https://pay.example.vn/hook"))
	clean := f.rules.seed(t, credTenantA, "b-clean", "orders", c10Legacy("https://pay.example.vn/other"))
	f.queue.failed[failing] = map[int]int{0: 5, 1: 3} // slot 0 is not moved: its failures are not C10's

	got := f.scan(t)
	details := map[string]string{}
	for _, it := range got.Items {
		details[it.Key] = it.Detail
	}
	if d := details[failing]; !strings.Contains(d, "3 lần gửi đã lỗi") || !strings.Contains(d, "Gửi lại") ||
		strings.Contains(d, "5 lần") || strings.Contains(d, "8 lần") {
		t.Fatalf("finding of the rule with failed deliveries = %q; want the 3 failures of the moved slot, told they lose their resend", d)
	}
	if d := details[clean]; strings.Contains(d, "lần gửi đã lỗi") {
		t.Fatalf("a rule without failed deliveries must not warn: %q", d)
	}
	// A warning, not a refusal: waiting for failures to expire would hold the move for a month.
	if res := f.fix(t); res.Fixed != 2 || res.Failed != 0 {
		t.Fatalf("fix = %+v; want both moved", res)
	}
}

// A deleted rule is soft-deleted (sys_workflows.deleted_at) and keeps its actions — keys included — with
// no purge and no restore. It never runs again, so C10 does not move its keys: it wipes them from the
// row (secret, headers, and the URL down to its origin — userinfo and path tokens with it), and the scan
// counts such rows so C10 = 0 means the table is clean. No master key is needed for that.
func TestDoctorC10_ScrubsSoftDeletedRules(t *testing.T) {
	const tgToken = "123456:AAF-telegram-bot-token"
	notify := workflow.Action{Type: workflow.ActionInAppNotification, UserID: "u-1", Message: "đơn mới"}
	seedAll := func(t *testing.T, f *c10Fixture) (live, delA, delB, delClean string) {
		live = f.rules.seed(t, credTenantA, "a-live", "orders", c10Legacy("https://pay.example.vn/hook"))
		delA = f.rules.seedDeleted(t, credTenantA, "b-deleted", notify, c10Legacy("https://pay.example.vn/hook?src=nexus"))
		delB = f.rules.seedDeleted(t, credTenantA, "c-deleted-url-keys",
			workflow.Action{Type: workflow.ActionWebhook, URL: "https://api.telegram.org/bot" + tgToken + "/sendMessage"},
			workflow.Action{Type: workflow.ActionWebhook, URL: "https://shop:pa55word@erp.example.vn/x"})
		delClean = f.rules.seedDeleted(t, credTenantA, "d-deleted-clean",
			workflow.Action{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: "/x"})
		f.rules.seedDeleted(t, credTenantB, "e-other-tenant", c10Legacy("https://pay.example.vn/hook"))
		return
	}
	leaks := []string{c10Secret, "tok-9f3a", c10APIKey, tgToken, "pa55word", "shop:", "/hook", "src=nexus", "/sendMessage"}

	t.Run("scan counts them, fix wipes them, the live rule still moves", func(t *testing.T) {
		f := newC10Fixture(t, true, false)
		live, delA, delB, delClean := seedAll(t, f)
		otherBefore := ""
		for id, w := range f.rules.deleted {
			if w.TenantID == credTenantB {
				otherBefore = id
			}
		}
		otherRaw := f.rules.deletedRaw(otherBefore)

		got := f.scan(t)
		keys := map[string]string{}
		for _, it := range got.Items {
			keys[it.Key] = it.Detail
		}
		if got.Count != 3 || keys[live] == "" || keys["deleted:"+delA] == "" || keys["deleted:"+delB] == "" {
			t.Fatalf("scan = %+v; want the live rule + the two deleted rules holding keys", got)
		}
		if _, listed := keys["deleted:"+delClean]; listed {
			t.Fatalf("a deleted rule with nothing to wipe was listed")
		}
		for k, d := range keys {
			for _, leak := range leaks {
				if strings.Contains(d, leak) {
					t.Fatalf("finding %s leaks %q: %s", k, leak, d)
				}
			}
		}
		if d := keys["deleted:"+delA]; !strings.Contains(d, "ĐÃ XOÁ") || !strings.Contains(d, "xoay") {
			t.Fatalf("deleted finding = %q; want it to say the rule is deleted and the key should be rotated", d)
		}

		if res := f.fix(t); res.Fixed != 3 || res.Failed != 0 {
			t.Fatalf("fix = %+v; want 3 fixed", res)
		}
		for _, id := range []string{delA, delB} {
			raw := f.rules.deletedRaw(id)
			for _, leak := range append(leaks, `"secret"`, `"headers"`) {
				if strings.Contains(raw, leak) {
					t.Fatalf("deleted rule %s still holds %s: %s", id, leak, raw)
				}
			}
		}
		var a []workflow.Action
		if err := json.Unmarshal([]byte(f.rules.deletedRaw(delA)), &a); err != nil {
			t.Fatal(err)
		}
		if len(a) != 2 || a[0].Message != "đơn mới" || a[1].Type != workflow.ActionWebhook || a[1].URL != "https://pay.example.vn" || a[1].CredentialID != "" {
			t.Fatalf("deleted rule A = %+v; want the notification untouched and the webhook down to its origin, no key", a)
		}
		var b []workflow.Action
		if err := json.Unmarshal([]byte(f.rules.deletedRaw(delB)), &b); err != nil {
			t.Fatal(err)
		}
		if len(b) != 2 || b[0].URL != "https://api.telegram.org" || b[1].URL != "https://erp.example.vn" {
			t.Fatalf("deleted rule B = %+v; want the URLs down to their origins", b)
		}
		if f.rules.deletedRaw(otherBefore) != otherRaw {
			t.Fatalf("another tenant's deleted rule was touched")
		}
		if k := f.liveCreds(t); len(k) != 1 {
			t.Fatalf("vault = %d key(s); want only the live rule's — a deleted rule's key is wiped, not stored", len(k))
		}
		if got := f.scan(t); got.Count != 0 {
			t.Fatalf("scan after the fix = %+v; want clean", got)
		}
		if res := f.fix(t); res.Fixed != 0 || res.Failed != 0 {
			t.Fatalf("second fix = %+v; want a no-op", res)
		}
	})

	t.Run("no master key: deleted rules are still wiped, live ones wait", func(t *testing.T) {
		f := newC10Fixture(t, false, false)
		live, delA, _, _ := seedAll(t, f)
		liveBefore := f.rules.raw(live)
		res := f.fix(t)
		if res.Fixed != 2 || res.Failed != 1 || !strings.Contains(strings.Join(res.Errors, " "), "CREDENTIALS_NOT_CONFIGURED") {
			t.Fatalf("fix without a master key = %+v; want the 2 deleted rules wiped, the live one refused", res)
		}
		if f.rules.raw(live) != liveBefore || strings.Contains(f.rules.deletedRaw(delA), c10Secret) {
			t.Fatalf("live rule %s / deleted rule %s", f.rules.raw(live), f.rules.deletedRaw(delA))
		}
	})
}

// lyingVault stores keys like the real vault but opens them to something else — a sealing bug.
type lyingVault struct{ *CredentialService }

func (l lyingVault) ResolveAuth(ctx context.Context, tenantID, id string) (workflow.WebhookAuth, error) {
	got, err := l.CredentialService.ResolveAuth(ctx, tenantID, id)
	got.SigningSecret += "-corrupted"
	return got, err
}

func TestDoctorC10_RefusesKeyThatDoesNotOpenBack(t *testing.T) {
	f := newC10Fixture(t, true, false)
	id := f.rules.seed(t, credTenantA, "r", "orders", c10Legacy("https://pay.example.vn/hook"))
	before := f.rules.raw(id)
	f.svc.WithInlineSecretMigration(lyingVault{f.creds}, f.rules, f.queue)
	res := f.fix(t)
	if res.Fixed != 0 || res.Failed != 1 || !strings.Contains(res.Errors[0], "không mở lại đúng") {
		t.Fatalf("fix = %+v; want the rule refused", res)
	}
	if f.rules.raw(id) != before || f.rules.writes != 0 {
		t.Fatalf("the rule was pointed at a key that does not open to its key: %s", f.rules.raw(id))
	}
}

func TestDoctorC10_InvalidatesCache(t *testing.T) {
	f := newC10Fixture(t, true, false)
	f.rules.seed(t, credTenantA, "r", "orders", c10Legacy("https://pay.example.vn/hook"))
	inner := &countingWorkflowLoader{}
	wfCache := workflow.NewCachedLoader(inner, time.Hour) // only an invalidation can force a reload
	f.svc.WithOpsCaches(nil, wfCache)
	if _, err := wfCache.ListActive(context.Background(), credTenantA, "orders", workflow.TriggerAfterCreate); err != nil {
		t.Fatal(err)
	}
	if res := f.fix(t); res.Fixed != 1 {
		t.Fatalf("fix = %+v", res)
	}
	if _, err := wfCache.ListActive(context.Background(), credTenantA, "orders", workflow.TriggerAfterCreate); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&inner.calls); n != 2 {
		t.Fatalf("loads = %d; want 2 — the next save must send with the rewritten rule, not the cached one", n)
	}
}

func TestDoctorC10_HeaderOnlyKeyKeepsEveryByte(t *testing.T) {
	// Headers only, no signing key: the first header by name (with a value) becomes the key's own
	// header with NO prefix — "Bearer " stays inside the sealed value, byte for byte.
	f := newC10Fixture(t, true, false)
	partner := newCapture(t)
	legacy := workflow.Action{Type: workflow.ActionWebhook, URL: partner.srv.URL + "/in",
		Headers: map[string]string{"authorization": "Bearer abc", "X-Empty": "", "X-Tenant": "northwind"}}
	id := f.rules.seed(t, credTenantA, "r", "orders", legacy)
	if res := f.fix(t); res.Fixed != 1 {
		t.Fatalf("fix = %+v", res)
	}
	k := f.liveCreds(t)
	if len(k) != 1 || k[0].AuthType != credential.AuthHeader || k[0].HeaderPrefix != "" {
		t.Fatalf("vault = %+v; want one header key with no prefix", k)
	}
	body := []byte(`{"x":1}`)
	sender := workflow.NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond)
	if err := sender.Post(context.Background(), workflow.WebhookRequestFromURLAction(legacy, body)); err != nil {
		t.Fatal(err)
	}
	before := partner.last(t).Header.Clone()
	req, err := workflow.WebhookRequestFor(context.Background(), f.creds, credTenantA, f.rules.actions(t, id)[0], body)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Post(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	after := partner.last(t).Header
	for _, h := range []string{"Authorization", "X-Empty", "X-Tenant"} {
		if len(before.Values(h)) != 1 || before.Get(h) != after.Get(h) || len(after.Values(h)) != 1 {
			t.Fatalf("%s: before %q, after %q", h, before.Values(h), after.Values(h))
		}
	}
	if after.Get(workflow.SignatureHeader) != "" {
		t.Fatalf("a header key must not start signing")
	}
}
