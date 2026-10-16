package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
)

// userLiveness answers "is this a real, active user of the tenant" (satisfied by *bpm.SQLRepository).
type userLiveness interface {
	LiveUser(ctx context.Context, tenantID, userID string) (bool, error)
}

// ApprovalSubmitterResolver names the user a rejected record goes back to (specs/043 S8): the
// instance's creator when they are a live user, else the record's current owner when they are, else
// nobody (bpm.ErrNoSubmitter). An API-key principal or a user who has left is not a person to hand
// "fix and send again" to. Satisfies bpm.SubmitterResolver.
type ApprovalSubmitterResolver struct {
	users   userLiveness
	records approvalRecordReader
}

// NewApprovalSubmitterResolver wires the resolver over the user table and the record service.
func NewApprovalSubmitterResolver(users userLiveness, records approvalRecordReader) *ApprovalSubmitterResolver {
	return &ApprovalSubmitterResolver{users: users, records: records}
}

// ResolveSubmitter returns the user to hand the record to, or "" when there is none.
func (r *ApprovalSubmitterResolver) ResolveSubmitter(ctx context.Context, tenantID, entityName, recordID, createdBy string) (string, error) {
	if r == nil || r.users == nil {
		return "", fmt.Errorf("services.ApprovalSubmitterResolver: not wired")
	}
	if ok, err := r.users.LiveUser(ctx, tenantID, createdBy); err != nil {
		return "", fmt.Errorf("services.ApprovalSubmitterResolver: creator: %w", err)
	} else if ok {
		return createdBy, nil
	}
	if r.records == nil {
		return "", nil
	}
	// A system read: who owns the record is a fact about it, not something the rejector may or may
	// not see (the same posture as the outcome write-back).
	rec, err := r.records.Get(ctx, RecordScope{IsAdmin: true, AccessLevel: string(dynrepo.AccessAll)}, tenantID, entityName, recordID, nil, nil)
	if errors.Is(err, ErrRecordNotFound) { // record deleted and creator gone: nobody to hand it to (422, not 500)
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("services.ApprovalSubmitterResolver: record: %w", err)
	}
	owner := strings.TrimSpace(rec.OwnerID)
	if owner == "" {
		return "", nil
	}
	if ok, err := r.users.LiveUser(ctx, tenantID, owner); err != nil {
		return "", fmt.Errorf("services.ApprovalSubmitterResolver: owner: %w", err)
	} else if ok {
		return owner, nil
	}
	return "", nil
}

var _ bpm.SubmitterResolver = (*ApprovalSubmitterResolver)(nil)
