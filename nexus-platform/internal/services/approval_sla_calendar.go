package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/workflow"
)

// checkSLAFitsCalendar refuses, at save, a working-hours deadline the calendar cannot hold (#80,
// FR-036 neighbour): AddBusinessDuration walks a bounded number of days, so a huge amount would
// only fail at run time and be stamped wall_calendar_error. Probed on the calendar the process
// would run on (options.calendarId, else the tenant default) with the REAL arithmetic, so save and
// run cannot disagree. A tenant with no calendar passes (wall clock is honest about itself); so does
// a server built without the calendar. 422 APPROVAL_SLA_AMOUNT_TOO_LARGE.
func (s *ApprovalService) checkSLAFitsCalendar(ctx context.Context, tenantID string, nodes []ApprovalNodeInput, at string, opts *ApprovalOptionsInput) error {
	if s.workingTime == nil {
		return nil
	}
	calID := ""
	if opts != nil {
		calID = strings.TrimSpace(opts.CalendarID)
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	fits := func(where string, d time.Duration) error {
		_, basis, err := s.workingTime.DueAt(ctx, tenantID, calID, now.UTC(), d)
		if err != nil {
			return fmt.Errorf("services.ApprovalService.checkSLAFitsCalendar: %w", err)
		}
		if basis == bpm.SLABasisWallCalendarError {
			return invalidField("APPROVAL_SLA_AMOUNT_TOO_LARGE", where,
				"hạn quá lớn so với lịch làm việc — lịch không chứa nổi khoảng giờ làm việc này, hãy giảm hạn")
		}
		return nil
	}
	for i, n := range nodes {
		if n.SLASeconds > 0 && strings.TrimSpace(n.SLAClock) != bpm.SLAClockWall {
			if err := fits(fmt.Sprintf("%s[%d].slaSeconds", at, i), time.Duration(n.SLASeconds)*time.Second); err != nil {
				return err
			}
		}
		if sla := n.SLA; sla != nil {
			if sla.Unit == workflow.TimerUnitBusinessHours && sla.Amount > 0 {
				if err := fits(fmt.Sprintf("%s[%d].sla.amount", at, i), time.Duration(sla.Amount)*time.Hour); err != nil {
					return err
				}
			}
			for j, row := range sla.Matrix {
				if row.Unit == workflow.TimerUnitBusinessHours && row.Amount > 0 {
					if err := fits(fmt.Sprintf("%s[%d].sla.matrix[%d].amount", at, i, j), time.Duration(row.Amount)*time.Hour); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
