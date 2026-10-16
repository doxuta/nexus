package bpm

import (
	"context"

	"nexus/platform/internal/jobs"
)

// continuerFunc adapts a function to Continuer.
type continuerFunc func(ctx context.Context, tenantID, instanceID string) error

func (f continuerFunc) Continue(ctx context.Context, tenantID, instanceID string) error {
	return f(ctx, tenantID, instanceID)
}

func jobsJob(tenant string, payload []byte) jobs.Job {
	return jobs.Job{ID: "j1", TenantID: tenant, Type: ContinueJobType, Payload: payload}
}
