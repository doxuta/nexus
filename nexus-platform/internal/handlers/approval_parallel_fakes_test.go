package handlers

import (
	"context"
	"errors"

	"nexus/platform/internal/bpm"
)

// specs/043 S4 widened the consumer-side ports the approval service reads (ListVisitSeats,
// Override); the existing test doubles gain the methods here and nothing else.

func (f *detailRepo) ListVisitSeats(context.Context, string, string, string) ([]bpm.VisitSeat, error) {
	return nil, nil
}

func (f *fakePendingRepo) ListVisitSeats(context.Context, string, string, string) ([]bpm.VisitSeat, error) {
	return nil, nil
}

func (s *spyDecider) Override(context.Context, string, string, bpm.DecisionKind, string, bpm.Actor, *int) (*bpm.Instance, error) {
	return nil, errors.New("not implemented")
}

func (s *spyWIDecider) Override(context.Context, string, string, bpm.DecisionKind, string, bpm.Actor, *int) (*bpm.Instance, error) {
	return nil, nil
}
