package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
)

// specs/050 S2b: the wire half of a vault webhook. A body field the handler does not map is dropped
// before the validator sees it — a dropped credentialId would leave an action with no url and no
// key, and a dropped path would silently post to the key's bare base URL.
func TestToServiceWorkflowActions_CredentialAndPathTravel(t *testing.T) {
	var body []workflowActionRequest
	if err := json.Unmarshal([]byte(`[{"type":"outbound_webhook","credentialId":"cred-1","path":"/orders?kind=paid"}]`), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := toServiceWorkflowActions(body)
	if len(got) != 1 || got[0].CredentialID != "cred-1" || got[0].Path != "/orders?kind=paid" || got[0].URL != "" {
		t.Fatalf("mapped = %+v; want credentialId + path, no url", got)
	}
}

// The save-time webhook codes (three from S2b, two from S3a) are 422 (well-formed request naming a
// bad reference/shape), the statusForCode default — pinned so a later grouping cannot quietly move them.
func TestStatusForCode_WebhookCredentialCodes(t *testing.T) {
	for _, code := range []string{"WEBHOOK_CREDENTIAL_NOT_FOUND", "WEBHOOK_URL_WITH_CREDENTIAL", "WEBHOOK_PATH_INVALID",
		"WEBHOOK_INLINE_SECRET_REMOVED", "WEBHOOK_REDACTED_VALUE"} {
		if got := statusForCode(code); got != http.StatusUnprocessableEntity {
			t.Fatalf("statusForCode(%s) = %d, want 422", code, got)
		}
	}
}
