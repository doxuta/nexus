package handlers

// specs/043 S10 bpm-process-versions at the HTTP seam: the five routes exist, the draft body goes
// through the SAME decoder + validator as a save (so a draft can never hold what a save refuses),
// and every refusal carries its own status and code (409 for state, 404 for "no such", 422 for bad
// input). The version/draft STORAGE is proven against real MySQL in
// services/bpm_versions_integration_test.go; this proves the join between the two.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"nexus/platform/internal/bpm"
)

func codeOf(t *testing.T, body []byte) string {
	t.Helper()
	var e struct{ Code string }
	_ = json.Unmarshal(body, &e)
	return e.Code
}

func TestApprovalVersionsAPI_StatusAndCodes(t *testing.T) {
	const body = `{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[{"approverType":"user","approverId":"u-1"}]}`
	repo := &memProcessRepo{rec: &bpm.ProcessRecord{ID: "p-1", TenantID: "t-1", Version: 1}}
	r := newApprovalAdminRouter(repo)

	cases := []struct {
		name         string
		method, path string
		body         string
		wantStatus   int
		wantCode     string
	}{
		{"draft saved", http.MethodPut, "/admin/approval-processes/p-1/draft", body, http.StatusOK, ""},
		{"draft refuses a key a save refuses", http.MethodPut, "/admin/approval-processes/p-1/draft",
			`{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[{"approverType":"user","approverId":"u-1","next":"s2"}]}`,
			http.StatusUnprocessableEntity, "APPROVAL_NODE_KIND_UNSUPPORTED"},
		{"draft with no steps", http.MethodPut, "/admin/approval-processes/p-1/draft",
			`{"name":"P","entityName":"crm_quotes","trigger":"manual","steps":[]}`, http.StatusUnprocessableEntity, "APPROVAL_STEPS_REQUIRED"},
		{"discard with no draft", http.MethodDelete, "/admin/approval-processes/p-1/draft", "", http.StatusNotFound, "APPROVAL_DRAFT_NOT_FOUND"},
		{"publish with no draft", http.MethodPost, "/admin/approval-processes/p-1/publish", `{"note":"x"}`, http.StatusConflict, "APPROVAL_VERSION_NO_DRAFT"},
		{"publish with no body at all", http.MethodPost, "/admin/approval-processes/p-1/publish", "", http.StatusConflict, "APPROVAL_VERSION_NO_DRAFT"},
		{"publish note too long", http.MethodPost, "/admin/approval-processes/p-1/publish", `{"note":"` + strings.Repeat("x", 501) + `"}`, http.StatusUnprocessableEntity, "APPROVAL_VERSION_NOTE_TOO_LONG"},
		{"versions list", http.MethodGet, "/admin/approval-processes/p-1/versions", "", http.StatusOK, ""},
		{"version is not a number", http.MethodGet, "/admin/approval-processes/p-1/versions/abc", "", http.StatusNotFound, "APPROVAL_VERSION_NOT_FOUND"},
		{"version zero", http.MethodGet, "/admin/approval-processes/p-1/versions/0", "", http.StatusNotFound, "APPROVAL_VERSION_NOT_FOUND"},
		{"version unknown", http.MethodGet, "/admin/approval-processes/p-1/versions/7", "", http.StatusNotFound, "APPROVAL_VERSION_NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := sendJSON(r, tc.method, tc.path, tc.body)
			if w.Code != tc.wantStatus || codeOf(t, w.Body.Bytes()) != tc.wantCode {
				t.Fatalf("%s %s = %d %s; want %d %q", tc.method, tc.path, w.Code, w.Body.String(), tc.wantStatus, tc.wantCode)
			}
		})
	}

	// An empty history is `[]`, never null — the screen maps over it.
	w := sendJSON(r, http.MethodGet, "/admin/approval-processes/p-1/versions", "")
	if !strings.Contains(w.Body.String(), `"versions":[]`) {
		t.Fatalf("empty history serialised as %s; want \"versions\":[]", w.Body.String())
	}
}
