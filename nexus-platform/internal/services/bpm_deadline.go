package services

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/calendar"
	"nexus/platform/internal/workflow"
)

// BPMDeadlineCalculator times approval steps on the tenant's working calendar (specs/043 S1, #39).
// It resolves the calendar through the SAME pinned-then-default lookup the workflow timer and the
// set_deadline action use (WorkflowScheduleResolver), so an approval deadline, a rule's deadline
// and the reminder about either can never disagree about when the office is open.
//
// Answers (bpm.DeadlineCalculator):
//   - calendar found:                 AddBusinessDuration → basis business.
//   - no live calendar (#82):         from + d, basis wall_no_calendar (the documented CALENDAR-1
//     fallback — and now the screen says so).
//   - calendar cannot hold d (#80):   from + d, basis wall_calendar_error, WARN — a temporary number,
//     labelled as one; never a hang and never "no calendar".
//   - store unreadable:               error, untouched — the engine decides (a person's action
//     fails; the async arming path stamps wall_calendar_error).
type BPMDeadlineCalculator struct {
	resolver workflow.ScheduleResolver
	logger   *zap.Logger
}

var _ bpm.DeadlineCalculator = (*BPMDeadlineCalculator)(nil)

// NewBPMDeadlineCalculator refuses a resolver that cannot reach a calendar store (nil, typed nil,
// or a WorkflowScheduleResolver built without one): unwired, it would answer "no calendar" for
// every tenant and every deadline would quietly be wall clock. logger may be nil.
func NewBPMDeadlineCalculator(resolver workflow.ScheduleResolver, logger *zap.Logger) (*BPMDeadlineCalculator, error) {
	if resolver == nil {
		return nil, fmt.Errorf("services.NewBPMDeadlineCalculator: no schedule resolver")
	}
	if w, ok := resolver.(*WorkflowScheduleResolver); ok && (w == nil || w.calendars == nil) {
		return nil, fmt.Errorf("services.NewBPMDeadlineCalculator: schedule resolver has no business-calendar store")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &BPMDeadlineCalculator{resolver: resolver, logger: logger}, nil
}

// schedule is THE calendar approval time runs on — deadlines (DueAt) and elapsed time
// (ElapsedSchedule) alike, so "due 11:00 Monday" and "took 3 working hours" cannot disagree about
// when the office is open. nil = the tenant has no live calendar.
//
// calendarID is the calendar a process pinned (options.calendarId, S13); "" = the tenant default.
// A pinned id that no longer exists resolves to "no calendar" (wall_no_calendar), like a rule's.
func (c *BPMDeadlineCalculator) schedule(ctx context.Context, tenantID, calendarID string) (*calendar.Schedule, error) {
	return c.resolver.ResolveSchedule(ctx, tenantID, calendarID)
}

var _ bpm.BusinessMeter = (*BPMDeadlineCalculator)(nil)

// BusinessBetween implements bpm.BusinessMeter: the working time between two instants on the
// calendar (ok=false when the tenant has none — the caller counts wall time).
func (c *BPMDeadlineCalculator) BusinessBetween(ctx context.Context, tenantID, calendarID string, from, to time.Time) (time.Duration, bool, error) {
	sched, err := c.schedule(ctx, tenantID, calendarID)
	if err != nil {
		return 0, false, fmt.Errorf("services.BPMDeadlineCalculator.BusinessBetween: %w", err)
	}
	if sched == nil {
		return 0, false, nil
	}
	return sched.BusinessDurationBetween(from, to), true, nil
}

// DueAt implements bpm.DeadlineCalculator on the tenant's approval calendar (schedule).
func (c *BPMDeadlineCalculator) DueAt(ctx context.Context, tenantID, calendarID string, from time.Time, d time.Duration) (time.Time, bpm.SLABasis, error) {
	sched, err := c.schedule(ctx, tenantID, calendarID)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("services.BPMDeadlineCalculator.DueAt: %w", err)
	}
	if sched == nil {
		return from.Add(d), bpm.SLABasisWallNoCalendar, nil
	}
	due, err := sched.AddBusinessDuration(from, d)
	if err != nil {
		c.logger.Warn("bpm: approval deadline does not fit the working calendar — stamped on the wall clock",
			zap.String("tenant_id", tenantID), zap.Duration("sla", d), zap.Error(err))
		return from.Add(d), bpm.SLABasisWallCalendarError, nil
	}
	return due, bpm.SLABasisBusiness, nil
}

// ElapsedSchedule answers "what is elapsed approval time measured on" for a READ (specs/043 S2,
// #40): the calendar, basis business; nil + wall_no_calendar when the tenant has none; nil +
// wall_calendar_error when the store cannot be read (WARN) or the calendar has no working hours at
// all (every stretch would read "0 working hours" — true of that calendar, and useless).
//
// Never an error, unlike DueAt: the elapsed readout rides GET /approvals/:id, where the approver
// decides — a secondary figure must not take that page down. It degrades under its own label.
func (c *BPMDeadlineCalculator) ElapsedSchedule(ctx context.Context, tenantID string) (*calendar.Schedule, bpm.SLABasis) {
	sched, err := c.schedule(ctx, tenantID, "")
	switch {
	case err != nil:
		c.logger.Warn("bpm: working calendar unreadable — approval elapsed time shown on the wall clock",
			zap.String("tenant_id", tenantID), zap.Error(err))
		return nil, bpm.SLABasisWallCalendarError
	case sched == nil:
		return nil, bpm.SLABasisWallNoCalendar
	case !sched.HasWorkingHours():
		return nil, bpm.SLABasisWallCalendarError
	}
	return sched, bpm.SLABasisBusiness
}
