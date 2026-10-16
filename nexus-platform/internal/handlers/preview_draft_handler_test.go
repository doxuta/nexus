package handlers

// preview_draft_handler_test.go — specs/064-preview-as-role-sample-data S9-9 (T130): the HTTP face of
// the draft flag. A workspace with no draft layer answers draft:true with 422 PREVIEW_DRAFT_UNAVAILABLE
// (no session row, no token); an absent or false flag opens a live preview as before.

import (
	"net/http"
	"testing"
)

func TestPreviewHandler_StartDraftFlag(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{"draft true, nothing to preview => 422", `{"roleId":"` + phRole + `","draft":true}`, http.StatusUnprocessableEntity, "PREVIEW_DRAFT_UNAVAILABLE"},
		{"draft false => 201 live", `{"roleId":"` + phRole + `","draft":false}`, http.StatusCreated, ""},
		{"draft absent, nothing to preview => 201 live", `{"roleId":"` + phRole + `"}`, http.StatusCreated, ""},
		{"draft not a boolean => 400", `{"roleId":"` + phRole + `","draft":"yes"}`, http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPreviewHarness(t)
			before := len(h.store.rows)
			code, out := h.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", "tok-op", tc.body)
			if code != tc.wantCode {
				t.Fatalf("status %d (%+v), want %d", code, out, tc.wantCode)
			}
			if tc.wantErr != "" && out.Code != tc.wantErr {
				t.Errorf("code %q, want %q", out.Code, tc.wantErr)
			}
			if code == http.StatusCreated {
				if out.Data.Draft == nil || *out.Data.Draft {
					t.Errorf("draft = %v, want false", out.Data.Draft)
				}
				return
			}
			if len(h.store.rows) != before {
				t.Error("a refused Start wrote a sys_preview_sessions row")
			}
		})
	}
}
