package workflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHTTPWebhookSender_RuleCannotForgeNexusHeaders (specs/050 S2a review, queue #309): the headers
// a rule carries never stand in for the ones Nexus computes — with a key (signing headers are set
// AFTER the rule's, so the computed value wins) AND without one (api-conventions §32: "luật chưa có
// khoá ký thì không có header nào trong số này" — a copied X-Nexus-Signature-V2 must not reach the
// receiver as if it were ours). Unrelated custom headers go out untouched.
func TestHTTPWebhookSender_RuleCannotForgeNexusHeaders(t *testing.T) {
	forged := map[string]string{
		SignatureHeader: "forged-v1", TimestampHeader: "1", SignatureV2Header: "forged-v2",
		SignatureV2PreviousHeader: "forged-prev", DeliveryIDHeader: "forged-delivery", "x-doi-tac": "northwind",
	}
	for _, tc := range []struct {
		name   string
		secret string
		signed bool // a real signature must be present
	}{{"with a key", goldenSecret, true}, {"without a key", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			var got signedHit
			var delivery, partner string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, delivery, partner = signingHeaders(r), r.Header.Get(DeliveryIDHeader), r.Header.Get("X-Doi-Tac")
			}))
			defer srv.Close()
			sender := NewHTTPWebhookSender(2*time.Second, 1, time.Millisecond)
			if _, err := sender.attempt(context.Background(), WebhookRequest{URL: srv.URL, Body: []byte(goldenBody),
				Headers: forged, SigningSecret: tc.secret, DeliveryID: "d-real"}); err != nil {
				t.Fatalf("attempt: %v", err)
			}
			if partner != "northwind" {
				t.Fatalf("X-Doi-Tac = %q; a custom header must go out unchanged", partner)
			}
			if delivery != "d-real" {
				t.Fatalf("%s = %q, want the delivery's own id", DeliveryIDHeader, delivery)
			}
			if tc.signed {
				if got.v1 != goldenV1 || got.v2 != wantV2(goldenSecret, got.ts, []byte(goldenBody)) || got.ts == "1" || got.v2prev != "" {
					t.Fatalf("signing headers = %+v; want the computed ones, the rule's copies gone", got)
				}
				return
			}
			if got != (signedHit{}) {
				t.Fatalf("unsigned request carried %+v; the rule's X-Nexus-* copies must not go out", got)
			}
		})
	}
}
