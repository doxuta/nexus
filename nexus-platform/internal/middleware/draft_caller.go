package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
)

// DraftHeader asks the server to overlay the builder's drafts on a read route shared with the
// customer's app (specs/063-draft-publish). Anyone but the builder is silently ignored.
const DraftHeader = "X-Nexus-Draft"

// WantsDraft is THE one rule for "this request reads the draft overlay" (specs/063 S7-3 + specs/064
// S9-9): the builder on /admin/* or with X-Nexus-Draft: 1, OR a preview-as-role session that carries
// pdr (any route — it is the whole point of the flag). A preview token without pdr never reads
// drafts, header or not, and the header of a non-builder is ignored. rel is the matched route
// relative to the API base.
func WantsDraft(claims *auth.Claims, isBuilder bool, rel, draftHeader string) bool {
	if claims.TokenType == auth.TokenTypePreview {
		return claims.PreviewDraft
	}
	return isBuilder && (strings.HasPrefix(rel, "/admin/") || draftHeader == "1")
}

// DraftCaller resolves WHO is acting, once, for the draft layer, and puts it on the request
// context (c.Request.Context(), where every handler already takes its ctx from). It never refuses
// a request: the decision to write a draft, a live row or to fail closed belongs to
// services.DraftGate, which needs the tenant's delivered state.
//
// Mount position is part of the contract (plan §2): after Auth, RequireTenant, PreviewReadOnly and
// RequireTenantWritable, and BEFORE the data rate limiter and every sub-group — gin snapshots a
// group's handler chain when the group is created, so a later Use() would leave a whole group
// without a Caller (and its writes on a delivered tenant failing closed with ErrNoCaller).
// apiBase is the protected group's base path ("/api/v1"); the admin test compares the MATCHED route
// (c.FullPath()), never the raw URL, so /api/v1/admin/../x cannot turn the overlay on.
func DraftCaller(apiBase string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var caller metaversion.Caller
		if claims, ok := ClaimsFromContext(c); ok {
			sub := claims.Subject
			// Neither an API key nor a preview token is ever the builder (a preview Subject has
			// IsAdmin pinned false; the type check keeps that true even if a field leaked).
			isBuilder := metaversion.IsBuilder(sub) && claims.TokenType != auth.TokenTypeAPIKey && claims.TokenType != auth.TokenTypePreview
			home := sub.TenantID
			if sub.ActorTenantID != "" {
				home = sub.ActorTenantID
			}
			rel := strings.TrimPrefix(c.FullPath(), apiBase)
			caller = metaversion.Caller{
				UserID:       sub.UserID,
				TenantID:     sub.TenantID,
				HomeTenantID: home,
				IsBuilder:    isBuilder,
				ReadDraft:    WantsDraft(claims, isBuilder, rel, c.GetHeader(DraftHeader)),
			}
		}
		// A request with no Subject still gets an (empty) Caller: "passed DraftCaller" is a
		// different fact from "no Caller at all" (CallerFrom ok=false).
		c.Request = c.Request.WithContext(metaversion.WithWarnings(metaversion.WithCaller(c.Request.Context(), caller)))
		c.Next()
	}
}
