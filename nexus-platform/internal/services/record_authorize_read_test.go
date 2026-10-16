package services

import (
	"context"
	"testing"
)

// TestRecordService_AuthorizeRead pins DAILY-QUEUE #225: the read-side twin of AuthorizeView arms the
// manual-share flag AFTER the grant resolves (so a denied caller gets nothing, not even a flagged
// scope), while AuthorizeView itself keeps leaving the flag as the route set it — the notes and
// approval paths rely on that (a share must not let someone attach a note).
func TestRecordService_AuthorizeRead(t *testing.T) {
	ctx := context.Background()
	base := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "none"}
	allow := &rjAuthz{allow: map[string]bool{"tickets": true}, level: map[string]string{"tickets": "owner"}}

	t.Run("allow: resolved level, identity verbatim, shares ON", func(t *testing.T) {
		got, err := buildRJService(&rjStore{}, allow, nil).AuthorizeRead(ctx, "t1", base, "tickets")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "owner", IncludeShares: true}
		if got != want {
			t.Fatalf("scope = %+v; want %+v", got, want)
		}
	})

	t.Run("deny: FORBIDDEN and a zero scope", func(t *testing.T) {
		deny := &rjAuthz{allow: map[string]bool{"tickets": false}}
		got, err := buildRJService(&rjStore{}, deny, nil).AuthorizeRead(ctx, "t1", base, "tickets")
		if codeOf(err) != "FORBIDDEN" || got != (RecordScope{}) {
			t.Fatalf("got (%+v, %v); want zero scope + FORBIDDEN", got, err)
		}
	})

	t.Run("AuthorizeView does NOT arm shares (notes/approvals stay write-honest)", func(t *testing.T) {
		got, err := buildRJService(&rjStore{}, allow, nil).AuthorizeView(ctx, "t1", base, "tickets")
		if err != nil || got.IncludeShares {
			t.Fatalf("AuthorizeView = (%+v, %v); want IncludeShares false", got, err)
		}
	})

	t.Run("a write action never carries the flag, whatever the caller sent", func(t *testing.T) {
		in := base
		in.IncludeShares = true
		allowEdit := &rjAuthz{allow: map[string]bool{"tickets": true}, level: map[string]string{"tickets": "owner"}}
		got, err := buildRJService(&rjStore{}, allowEdit, nil).authorizeAction(ctx, "t1", in, "tickets", "edit")
		if err != nil || got.IncludeShares {
			t.Fatalf("edit scope = (%+v, %v); want IncludeShares false", got, err)
		}
	})
}
