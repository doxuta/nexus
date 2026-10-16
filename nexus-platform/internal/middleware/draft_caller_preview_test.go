package middleware

// draft_caller_preview_test.go — specs/064-preview-as-role-sample-data S9-9 (T130): a preview token
// reads the draft overlay only when it carries pdr, on any route, never via X-Nexus-Draft, and it
// never becomes the builder.

import (
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
)

func TestDraftCaller_PreviewDraftFlag(t *testing.T) {
	preview := func(pdr bool) *auth.Claims {
		return &auth.Claims{Subject: auth.Subject{UserID: "principal", TenantID: "customer", RoleID: "r1",
			ActorUserID: "b", ActorTenantID: "home", PreviewDraft: pdr}, TokenType: auth.TokenTypePreview}
	}
	// IsAdmin/IsPlatformOperator on a preview Subject would be a forged or leaked value: still never a builder.
	forged := preview(true)
	forged.IsPlatformOperator, forged.IsAdmin = true, true
	base := metaversion.Caller{UserID: "principal", TenantID: "customer", HomeTenantID: "home"}
	withDraft := base
	withDraft.ReadDraft = true

	tests := []struct {
		name       string
		claims     *auth.Claims
		route, url string
		header     string
		want       metaversion.Caller
	}{
		{"pdr=true on a shared route reads the overlay", preview(true), "/api/v1/reports", "/api/v1/reports", "", withDraft},
		{"pdr=true on a record route", preview(true), "/api/v1/:entity", "/api/v1/crm_accounts", "", withDraft},
		{"pdr=false reads live", preview(false), "/api/v1/reports", "/api/v1/reports", "", base},
		{"pdr=false + X-Nexus-Draft header still reads live", preview(false), "/api/v1/reports", "/api/v1/reports", "1", base},
		{"pdr=false on an /admin route still reads live", preview(false), "/api/v1/admin/workflows", "/api/v1/admin/workflows", "1", base},
		{"forged admin+operator on a preview token is never the builder", forged, "/api/v1/reports", "/api/v1/reports", "", withDraft},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := draftCallerRun(t, tc.claims, tc.route, tc.url, tc.header)
			if !ok || got != tc.want {
				t.Fatalf("Caller = %+v (ok=%v), want %+v", got, ok, tc.want)
			}
		})
	}
	// A non-preview Subject with a stale PreviewDraft must not turn the overlay on.
	odd := &auth.Claims{Subject: auth.Subject{UserID: "s", TenantID: "t1", PreviewDraft: true}, TokenType: auth.TokenTypeAccess}
	if got, _ := draftCallerRun(t, odd, "/api/v1/reports", "/api/v1/reports", ""); got.ReadDraft {
		t.Error("PreviewDraft on a non-preview token turned the overlay on")
	}
}
