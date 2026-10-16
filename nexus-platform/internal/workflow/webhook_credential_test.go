package workflow

// specs/050 S2b (tasks T037, T038) — an outbound_webhook action that names a key in the vault
// ({credentialId, path}) instead of carrying its own url/secret/headers. The engine reads the key
// AT SEND TIME under the rule's tenant, binds the joined URL to the key's origin, never follows a
// redirect with the key on board, and sends nothing at all when the key cannot be used.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeCredResolver answers ResolveAuth from a (tenant|id) map; err (when set) wins. calls records every
// (tenant|id) it was asked for.
type fakeCredResolver struct {
	mu    sync.Mutex
	auth  map[string]WebhookAuth
	err   error
	calls []string
}

func (f *fakeCredResolver) ResolveAuth(_ context.Context, tenantID, credentialID string) (WebhookAuth, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tenantID+"|"+credentialID)
	if f.err != nil {
		return WebhookAuth{}, f.err
	}
	a, ok := f.auth[tenantID+"|"+credentialID]
	if !ok {
		return WebhookAuth{}, &CodedError{Code: CodeCredentialNotFound, Msg: "không tìm thấy khoá kết nối"}
	}
	return a, nil
}

func (f *fakeCredResolver) set(key string, a WebhookAuth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth[key] = a
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("err = %v; want a CodedError %s", err, code)
	}
}

// TestJoinCredentialURL_HostBinding: the joined URL is the key's base + path, and nothing a path
// can hold moves it off the key's scheme/host/port or out of the base's path prefix.
func TestJoinCredentialURL_HostBinding(t *testing.T) {
	const base = "https://pay.example.vn/api"
	ok := []struct{ base, path, want string }{
		{base, "/orders", "https://pay.example.vn/api/orders"},
		{base, "", "https://pay.example.vn/api"},
		{base + "/", "/orders", "https://pay.example.vn/api/orders"},
		{base, "/orders?since=2026-09-30&kind=paid", "https://pay.example.vn/api/orders?since=2026-09-30&kind=paid"},
		{"http://127.0.0.1:8099", "/in", "http://127.0.0.1:8099/in"},
	}
	for _, tc := range ok {
		got, err := JoinCredentialURL(tc.base, tc.path)
		if err != nil || got != tc.want {
			t.Fatalf("JoinCredentialURL(%q, %q) = %q, %v; want %q", tc.base, tc.path, got, err, tc.want)
		}
	}
	bad := []string{
		"//evil.com",           // protocol-relative look-alike
		"/@evil.com",           // userinfo trick
		"@evil.com",            // no leading slash + userinfo
		"orders",               // relative
		"https://evil.com/x",   // absolute URL
		"/..",                  // dot segment
		"/a/../../admin",       // dot segments
		"/%2e%2e/admin",        // encoded dot segment
		"/%2E%2e/admin",        // mixed case
		"/.%2e/admin",          // half encoded
		"/a%2f..%2fadmin",      // encoded slash
		"\\evil.com",           // backslash
		"/x\\evil.com",         // backslash inside
		"/x#frag",              // fragment
		"/x\r\nHost: evil.com", // CR/LF
		"/x y",                 // whitespace
		"/x\ty",                // tab
		"/%zz",                 // broken escape: url.Parse refuses it at send time
		"/a%",                  // truncated escape
		"/100%",                // bare percent
		"/" + strings.Repeat("a", 500),
	}
	for _, p := range bad {
		// Config time (services.buildActionsJSON) runs ValidateCredentialPath alone: a path the
		// send-time join refuses but this rule accepts would save, then fail every delivery.
		if ValidateCredentialPath(p) == nil {
			t.Fatalf("ValidateCredentialPath(%q) = nil; the config-time rule must refuse what the send refuses", p)
		}
		got, err := JoinCredentialURL(base, p)
		if err == nil {
			t.Fatalf("JoinCredentialURL(%q) = %q; want WEBHOOK_PATH_INVALID", p, got)
		}
		wantCode(t, err, CodeWebhookPathInvalid)
		if strings.Contains(err.Error(), "pay.example.vn") {
			t.Fatalf("path error %q repeats the base URL (a url_token base carries the key)", err)
		}
	}
	// Against a base with no path, a naive concatenation would MOVE THE HOST. The path rule above
	// refuses every such path first, so the parse-back origin check behind it is exercised on its
	// own (joinWithinOrigin) — otherwise dropping it would leave every test green (spec §"Bằng
	// chứng" S2b).
	for _, p := range []string{"@evil.com", ".evil.com/x", ":8443/x", "evil.com"} {
		if got, err := JoinCredentialURL("https://pay.example.vn", p); err == nil {
			t.Fatalf("JoinCredentialURL(bare base, %q) = %q; want refused", p, got)
		}
		got, err := joinWithinOrigin("https://pay.example.vn", p)
		if err == nil {
			t.Fatalf("joinWithinOrigin(bare base, %q) = %q; want the origin wall to refuse it", p, got)
		}
		wantCode(t, err, CodeWebhookPathInvalid)
	}
	if got, err := joinWithinOrigin("https://pay.example.vn", "/orders"); err != nil || got != "https://pay.example.vn/orders" {
		t.Fatalf("joinWithinOrigin(bare base, /orders) = %q, %v; want the joined URL", got, err)
	}
}

// TestHTTPWebhookSender_CredentialRequestDoesNotFollowRedirect: a request that carries a vault key
// stops at a 3xx — a compromised partner cannot bounce the key (custom headers survive Go's
// cross-host redirect) to a host of its choosing. Not retried: the partner will answer the same.
func TestHTTPWebhookSender_CredentialRequestDoesNotFollowRedirect(t *testing.T) {
	var stolen int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stolen, 1)
	}))
	defer other.Close()
	var hops int32
	partner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hops, 1)
		http.Redirect(w, r, other.URL+"/collect", http.StatusFound)
	}))
	defer partner.Close()

	sender := NewHTTPWebhookSender(2*time.Second, 3, time.Millisecond)
	req := WebhookRequest{URL: partner.URL + "/orders", DisplayURL: partner.URL + "/orders", Body: []byte(`{}`),
		Headers: map[string]string{"X-Api-Key": "partner-key"}, NoRedirect: true}

	retryable, err := sender.Deliver(context.Background(), req)
	if err == nil || retryable {
		t.Fatalf("Deliver = (retryable %v, err %v); want a permanent error", retryable, err)
	}
	wantCode(t, err, CodeRedirectNotFollowed)
	if err := sender.Post(context.Background(), req); err == nil {
		t.Fatal("Post through a 302 must fail")
	} else {
		wantCode(t, err, CodeRedirectNotFollowed)
	}
	if n := atomic.LoadInt32(&stolen); n != 0 {
		t.Fatalf("the redirect target received %d requests — the key followed the 3xx", n)
	}
	if n := atomic.LoadInt32(&hops); n != 2 {
		t.Fatalf("partner saw %d requests; want 2 (one Deliver + one Post, no retry of a 3xx)", n)
	}

	// A url action (no vault key) keeps today's behaviour: the default client follows.
	legacy := WebhookRequest{URL: partner.URL + "/orders", Body: []byte(`{}`)}
	if _, err := sender.Deliver(context.Background(), legacy); err != nil {
		t.Fatalf("legacy request through a 302: %v", err)
	}
	if n := atomic.LoadInt32(&stolen); n != 1 {
		t.Fatalf("legacy request: redirect target saw %d requests, want 1 (unchanged behaviour)", n)
	}
}

// TestRunWebhook_ResolvesCredentialAtSendTime: nothing about the key is remembered between two
// fires — the second send carries whatever the vault holds at that moment (rotation takes effect
// on the next send, FR-014).
func TestRunWebhook_ResolvesCredentialAtSendTime(t *testing.T) {
	res := &fakeCredResolver{auth: map[string]WebhookAuth{}}
	wh := &fakeWebhookSender{}
	wf := Workflow{ID: "wf1", TenantID: "t1"}
	evt := Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterCreate}
	a := Action{Type: ActionWebhook, CredentialID: "cred-1", Path: "/orders?kind=paid"}
	deps := actionDeps{webhook: wh, creds: res}

	res.set("t1|cred-1", WebhookAuth{BaseURL: "https://pay.example.vn/api", DisplayBase: "https://pay.example.vn/api",
		Headers: map[string]string{"Authorization": "Bearer old"}, SigningSecret: "hmac-old"})
	if err := runWebhook(context.Background(), wf, evt, a, 0, deps); err != nil {
		t.Fatalf("first send: %v", err)
	}
	first := wh.req
	res.set("t1|cred-1", WebhookAuth{BaseURL: "https://pay.example.vn/api", DisplayBase: "https://pay.example.vn/api",
		Headers: map[string]string{"Authorization": "Bearer new"}, SigningSecret: "hmac-new"})
	if err := runWebhook(context.Background(), wf, evt, a, 0, deps); err != nil {
		t.Fatalf("second send: %v", err)
	}
	second := wh.req

	if first.Headers["Authorization"] != "Bearer old" || second.Headers["Authorization"] != "Bearer new" ||
		second.SigningSecret != "hmac-new" {
		t.Fatalf("second send carried %v / %q; want the key as the vault holds it NOW", second.Headers, second.SigningSecret)
	}
	if second.URL != "https://pay.example.vn/api/orders?kind=paid" || !second.NoRedirect {
		t.Fatalf("url=%q noRedirect=%v; want base+path and a no-redirect request", second.URL, second.NoRedirect)
	}
	// The display form keeps the path (the key is in the vault, not the path) and drops the query.
	if second.DisplayURL != "https://pay.example.vn/api/orders" {
		t.Fatalf("display = %q, want base + path without query", second.DisplayURL)
	}
	if len(res.calls) != 2 || res.calls[0] != "t1|cred-1" {
		t.Fatalf("resolver calls = %v; want one per send, under the rule's tenant", res.calls)
	}
}

// TestRunWebhook_CredentialFailureNeverSendsUnauthenticated: a key that cannot be used stops the
// send — the partner never receives an unauthenticated call (FR-016) — and the error carries the
// runtime code the run history shows.
func TestRunWebhook_CredentialFailureNeverSendsUnauthenticated(t *testing.T) {
	wf := Workflow{ID: "wf1", TenantID: "t1"}
	evt := Event{TenantID: "t1"}
	a := Action{Type: ActionWebhook, CredentialID: "cred-1", Path: "/orders"}
	cases := []struct {
		name  string
		creds CredentialResolver
		code  string
	}{
		{"slot without a secret", &fakeCredResolver{err: &CodedError{Code: CodeCredentialNotReady}}, CodeCredentialNotReady},
		{"sealed under a missing master key", &fakeCredResolver{err: &CodedError{Code: CodeCredentialKeyUnavailable}}, CodeCredentialKeyUnavailable},
		{"deleted / unknown", &fakeCredResolver{auth: map[string]WebhookAuth{}}, CodeCredentialNotFound},
		{"vault not configured", &fakeCredResolver{err: &CodedError{Code: CodeCredentialsNotConfigured}}, CodeCredentialsNotConfigured},
		{"no resolver wired", nil, CodeCredentialsNotConfigured},
		{"base the path cannot join", &fakeCredResolver{auth: map[string]WebhookAuth{"t1|cred-1": {BaseURL: "https://pay.example.vn"}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wh := &fakeWebhookSender{}
			act := a
			if tc.code == "" {
				act.Path = "//evil.com" // stored before the config check existed
				tc.code = CodeWebhookPathInvalid
			}
			var deps actionDeps
			deps.webhook = wh
			if tc.creds != nil {
				deps.creds = tc.creds
			}
			err := runWebhook(context.Background(), wf, evt, act, 0, deps)
			wantCode(t, err, tc.code)
			if wh.calls != 0 {
				t.Fatalf("sender called %d times; a key that cannot be used must send NOTHING", wh.calls)
			}
		})
	}
	// A transient store failure is not a code: it propagates as is (the durable queue retries it).
	wh := &fakeWebhookSender{}
	err := runWebhook(context.Background(), wf, evt, a, 0, actionDeps{webhook: wh, creds: &fakeCredResolver{err: errors.New("db down")}})
	var ce *CodedError
	if err == nil || errors.As(err, &ce) || wh.calls != 0 {
		t.Fatalf("store failure = %v (calls %d); want a plain error and no send", err, wh.calls)
	}
}

// TestRunWebhook_CrossTenantCredentialNotFound: the key is looked up under the RULE's tenant, so a
// rule of tenant A pointing at tenant B's key id finds nothing and sends nothing.
func TestRunWebhook_CrossTenantCredentialNotFound(t *testing.T) {
	res := &fakeCredResolver{auth: map[string]WebhookAuth{
		"tB|cred-b": {BaseURL: "https://b.example.vn", DisplayBase: "https://b.example.vn", Headers: map[string]string{"Authorization": "Bearer b"}},
	}}
	wh := &fakeWebhookSender{}
	err := runWebhook(context.Background(), Workflow{ID: "wf-a", TenantID: "tA"}, Event{TenantID: "tA"},
		Action{Type: ActionWebhook, CredentialID: "cred-b"}, 0, actionDeps{webhook: wh, creds: res})
	wantCode(t, err, CodeCredentialNotFound)
	if wh.calls != 0 || len(res.calls) != 1 || res.calls[0] != "tA|cred-b" {
		t.Fatalf("calls = %d, resolver asked %v; want no send and a lookup pinned to tenant tA", wh.calls, res.calls)
	}
}

// TestRunWebhook_TenantMismatchRefuses: an event of another tenant reaching a rule is a bug
// upstream; the engine refuses before resolving anything — on both the inline and the queue path.
func TestRunWebhook_TenantMismatchRefuses(t *testing.T) {
	res := &fakeCredResolver{auth: map[string]WebhookAuth{"tA|cred-a": {BaseURL: "https://a.example.vn"}}}
	wh, q := &fakeWebhookSender{}, &fakeWebhookQueue{}
	a := Action{Type: ActionWebhook, CredentialID: "cred-a"}
	for _, deps := range []actionDeps{{webhook: wh, creds: res}, {webhookQueue: q, creds: res}} {
		if err := runWebhook(context.Background(), Workflow{ID: "wf-a", TenantID: "tA"}, Event{TenantID: "tB"}, a, 0, deps); err == nil {
			t.Fatal("rule tenant ≠ event tenant must refuse")
		}
	}
	if wh.calls != 0 || len(q.parked) != 0 || len(res.calls) != 0 {
		t.Fatalf("send %d, parked %d, resolved %v; want nothing at all", wh.calls, len(q.parked), res.calls)
	}
}

// TestRunWebhook_QueueParksCredentialDelivery: the durable path parks WHICH key the slot named (so
// the job handler can tell the rule changed under it) and resolves nothing at enqueue — the key is
// read when the job is delivered, not when the event fired.
func TestRunWebhook_QueueParksCredentialDelivery(t *testing.T) {
	res := &fakeCredResolver{auth: map[string]WebhookAuth{}}
	q := &fakeWebhookQueue{}
	err := runWebhook(context.Background(), Workflow{ID: "wf1", TenantID: "t1"}, Event{TenantID: "t1"},
		Action{Type: ActionWebhook, CredentialID: "cred-1", Path: "/orders"}, 2, actionDeps{webhookQueue: q, creds: res})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if len(q.parked) != 1 || q.parked[0].CredentialID != "cred-1" || q.parked[0].ActionIndex != 2 {
		t.Fatalf("parked = %+v; want one delivery naming cred-1 at slot 2", q.parked)
	}
	if len(res.calls) != 0 {
		t.Fatalf("resolver asked %v at enqueue; the key is read at delivery", res.calls)
	}
}

// TestDispatcher_InlineVaultWebhookGoesThroughWiring: with the durable queue off (JOBS_ENABLED=false)
// a vault action is resolved inline, through the resolver WithCredentialResolver stored and deps()
// hands to runWebhook — and to the scheduler, which snapshots deps() in NewScheduler. The runWebhook
// tests build actionDeps by hand and the integration test resolves at delivery, so without this a
// dropped `creds: d.creds` would leave every test green and every inline vault send failing
// CREDENTIALS_NOT_CONFIGURED.
func TestDispatcher_InlineVaultWebhookGoesThroughWiring(t *testing.T) {
	auth := WebhookAuth{BaseURL: "https://pay.example.vn/api", DisplayBase: "https://pay.example.vn/api",
		Headers: map[string]string{"X-Api-Key": "k-live"}, SigningSecret: "hmac-live"}
	act := Action{Type: ActionWebhook, CredentialID: "cred-1", Path: "/orders"}
	check := func(t *testing.T, wh *fakeWebhookSender, res *fakeCredResolver) {
		t.Helper()
		if wh.calls != 1 {
			t.Fatalf("sender called %d times; want one inline send", wh.calls)
		}
		if wh.req.URL != "https://pay.example.vn/api/orders" || !wh.req.NoRedirect ||
			wh.req.Headers["X-Api-Key"] != "k-live" || wh.req.SigningSecret != "hmac-live" {
			t.Fatalf("request = url %q noRedirect %v headers %v secret %q; want the key's base+path, headers and secret, no redirect",
				wh.req.URL, wh.req.NoRedirect, wh.req.Headers, wh.req.SigningSecret)
		}
		if len(res.calls) != 1 || res.calls[0] != "t1|cred-1" {
			t.Fatalf("resolver asked %v; want one lookup under the rule's tenant", res.calls)
		}
	}

	t.Run("event engine", func(t *testing.T) {
		res := &fakeCredResolver{auth: map[string]WebhookAuth{"t1|cred-1": auth}}
		wh := &fakeWebhookSender{}
		loader := &fakeLoader{workflows: []Workflow{{
			ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterCreate, IsActive: true, Actions: []Action{act},
		}}}
		d := NewAsyncDispatcher(DispatcherConfig{Buffer: 4, Workers: 1}, loader, nil, nil, zap.NewNop()).
			WithWebhookSender(wh).WithCredentialResolver(res)
		d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterCreate})
		drainDispatcher(t, d)
		check(t, wh, res)
	})

	t.Run("scheduler snapshot", func(t *testing.T) {
		res := &fakeCredResolver{auth: map[string]WebhookAuth{"t1|cred-1": auth}}
		wh := &fakeWebhookSender{}
		engine := NewAsyncDispatcher(DispatcherConfig{Buffer: 1, Workers: 1}, nil, nil, nil, zap.NewNop()).
			WithWebhookSender(wh).WithCredentialResolver(res)
		defer drainDispatcher(t, engine)
		loader := newFakeScheduledLoader(ScheduledWorkflow{Workflow: Workflow{
			ID: "wf-s", TenantID: "t1", EntityName: "deals", Trigger: TriggerScheduled, IsActive: true, Actions: []Action{act},
		}})
		lister := &fakeRecordLister{records: map[string][]ScheduledRecord{"deals": {{ID: "r1", Data: map[string]any{}}}}}
		NewScheduler(SchedulerConfig{Interval: time.Minute}, loader, lister, engine, zap.NewNop()).
			runDue(context.Background(), time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC))
		check(t, wh, res)
	})
}
