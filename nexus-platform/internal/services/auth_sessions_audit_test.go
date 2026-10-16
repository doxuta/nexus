package services

import (
	"context"
	"testing"

	"nexus/platform/internal/audit"
)

// #311 / specs/065 T058: ending SOMEONE ELSE's session leaves a __user__ audit row.

func TestEndSession_AuditsAnotherActor(t *testing.T) {
	cases := []struct {
		name      string
		actor     string // "" = no actor in ctx
		unknown   bool
		wantEvent bool
	}{
		{"admin ends user's session", "admin-9", false, true},
		{"self end", "u-1", false, false},
		{"no actor", "", false, false},
		{"unknown session", "admin-9", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, password := newTestService(t)
			aud := &capAuditor{}
			svc = svc.WithAuditor(aud)
			b := loginAlice(t, svc, password)
			sid := sidOfAccess(t, svc, b.AccessToken)
			if tc.unknown {
				sid = "nope"
			}
			ctx := context.Background()
			if tc.actor != "" {
				ctx = audit.WithActor(ctx, tc.actor)
			}
			_ = svc.EndSession(ctx, "t-1", "u-1", sid)
			if !tc.wantEvent {
				if len(aud.events) != 0 {
					t.Fatalf("events = %+v, want none", aud.events)
				}
				return
			}
			if len(aud.events) != 1 {
				t.Fatalf("events = %d, want 1", len(aud.events))
			}
			e := aud.events[0]
			if e.EntityName != audit.UserMeta || e.ActorUserID != tc.actor || e.RecordID != "u-1" || e.TenantID != "t-1" || e.Action != audit.ActionUpdate {
				t.Fatalf("event = %+v", e)
			}
			if e.Diff == nil || e.Diff.After["sessionEnded"] != sid {
				t.Fatalf("diff = %+v", e.Diff)
			}
		})
	}
}
