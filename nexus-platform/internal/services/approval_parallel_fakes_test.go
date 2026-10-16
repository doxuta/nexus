package services

import (
	"context"
	"errors"

	"nexus/platform/internal/bpm"
)

// specs/043 S4 widened the consumer-side ports (approvalInstanceRepo.ListVisitSeats,
// approvalDecider.Override); the existing test doubles gain the methods here and nothing else —
// none of their scenarios touches many-seat steps.

func (f *fakeApprovalInstanceRepo) ListVisitSeats(context.Context, string, string, string) ([]bpm.VisitSeat, error) {
	return nil, nil
}

func (f *fakeApprovalInstRepo) ListVisitSeats(context.Context, string, string, string) ([]bpm.VisitSeat, error) {
	return nil, nil
}

func (f *fakeApprovalDecider) Override(context.Context, string, string, bpm.DecisionKind, string, bpm.Actor, *int) (*bpm.Instance, error) {
	return nil, errors.New("not implemented")
}

func (d *countingDecider) Override(context.Context, string, string, bpm.DecisionKind, string, bpm.Actor, *int) (*bpm.Instance, error) {
	return nil, errors.New("not implemented")
}
