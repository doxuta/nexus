package handlers

// specs/062-customer-workspace L3 T055a — the vertical install path emits __entity__/__field__/__role__
// audit rows from inside the service, which read the actor from the request context
// (audit.ActorFromContext). The handler used to hand the service the bare c.Request.Context(), so
// every row of an install (including "Cài mẫu" inside a workspace session and ?targetTenant)
// was written with actor_user_id = ''. These tests pin that the context reaching Install and
// DryRunInstall carries the verified caller.

import (
	"context"
	"net/http"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/services/vertical_pack"
)

// actorSpyVerticalSvc records the audit actor found on the ctx each service call receives.
type actorSpyVerticalSvc struct {
	fakeVerticalSvc
	installActor string
	dryRunActor  string
}

func (s *actorSpyVerticalSvc) Install(ctx context.Context, tenantID, userID string, p *vertical_pack.Pack) (*vertical_pack.InstallResult, error) {
	s.installActor = audit.ActorFromContext(ctx)
	return s.fakeVerticalSvc.Install(ctx, tenantID, userID, p)
}

func (s *actorSpyVerticalSvc) DryRunInstall(ctx context.Context, tenantID string, p *vertical_pack.Pack) (*vertical_pack.InstallPlan, error) {
	s.dryRunActor = audit.ActorFromContext(ctx)
	return s.fakeVerticalSvc.DryRunInstall(ctx, tenantID, p)
}

func TestInstall_CarriesActorOnContext(t *testing.T) {
	tests := []struct {
		name   string
		query  string
		dryRun bool
	}{
		{"install in own workspace", "", false},
		{"install with targetTenant", "?targetTenant=khach-b", false},
		{"dry run in own workspace", "?dryRun=true", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &actorSpyVerticalSvc{}
			gate := &fakeOperatorGate{t: t, user: operatorUser()}
			resolver := &fakeTenantResolver{t: t, bySlug: map[string]*auth.Tenant{"khach-b": {ID: mscTargetTenant}}}
			r := mscRouter(svc, gate, resolver, &fakeAuditor{})

			w := postInstall(t, r, tc.query, map[string]any{"builtin": "crm"})
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
			}
			got := svc.installActor
			if tc.dryRun {
				got = svc.dryRunActor
			}
			if got != mscCallerUser {
				t.Fatalf("actor on the service ctx = %q; want %q", got, mscCallerUser)
			}
		})
	}
}
