package workflow

// specs/050 S4 (tasks T076) — the send path of a rotated hmac key: a request built from a resolved
// key in its grace window reaches the partner signed twice (V2 under the new key, V2-Previous under
// the old one), and the first send after the grace — the resolver having dropped the previous key —
// carries no V2-Previous at all. Nothing is cached between the two sends.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHTTPWebhookSender_V2PreviousDuringGrace(t *testing.T) {
	var mu sync.Mutex
	var hits []signedHit
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits = append(hits, signingHeaders(r))
	}))
	defer srv.Close()

	const key = "new-signing-key-0123456789abcdef-new"
	const prev = "old-signing-key-0123456789abcdef-old"
	creds := &fakeCredResolver{auth: map[string]WebhookAuth{
		"t1|k1": {BaseURL: srv.URL, DisplayBase: srv.URL, SigningSecret: key, PrevSigningSecret: prev},
	}}
	sender := NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond)
	send := func() {
		t.Helper()
		req, err := WebhookRequestFor(context.Background(), creds, "t1", Action{Type: "outbound_webhook", CredentialID: "k1", Path: "/orders"}, []byte(goldenBody))
		if err != nil {
			t.Fatalf("WebhookRequestFor: %v", err)
		}
		if err := sender.Post(context.Background(), req); err != nil {
			t.Fatalf("Post: %v", err)
		}
	}

	send()                                                                                      // inside the grace
	creds.set("t1|k1", WebhookAuth{BaseURL: srv.URL, DisplayBase: srv.URL, SigningSecret: key}) // the grace is over
	send()

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 2 {
		t.Fatalf("receiver got %d requests, want 2", len(hits))
	}
	in := hits[0]
	if in.v2 != wantV2(key, in.ts, []byte(goldenBody)) || in.v2prev != wantV2(prev, in.ts, []byte(goldenBody)) {
		t.Fatalf("grace send: V2 under new key ok=%v, V2-Previous under old key ok=%v",
			in.v2 == wantV2(key, in.ts, []byte(goldenBody)), in.v2prev == wantV2(prev, in.ts, []byte(goldenBody)))
	}
	if after := hits[1]; after.v2prev != "" || after.v2 != wantV2(key, after.ts, []byte(goldenBody)) {
		t.Fatalf("send after the grace: V2-Previous = %q (want none), V2 ok=%v", after.v2prev, after.v2 == wantV2(key, after.ts, []byte(goldenBody)))
	}
}
