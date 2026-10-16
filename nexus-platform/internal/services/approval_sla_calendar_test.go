package services

// specs/043 S13 (#80) — a working-hours deadline the calendar cannot hold is refused at save, on the
// calendar the process would actually run on.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"nexus/platform/internal/calmeta"
)

func TestValidate_SLAAmountTooLargeForCalendar(t *testing.T) {
	tiny := &calmeta.BusinessCalendar{ // one working minute a week: 10 working hours is ~600 weeks, past the walk bound
		ID: "cal-tiny", TenantID: "t1", Timezone: "UTC", IsDefault: true,
		Hours: json.RawMessage(`{"mon":[{"start":"08:00","end":"08:01"}]}`),
	}
	cases := []struct {
		name    string
		store   timerCalendarReader
		node    ApprovalNodeInput
		opts    *ApprovalOptionsInput
		wantErr bool
	}{
		{"fits the default calendar", &fakeTimerCalendars{def: vnDefaultCalendar()}, ApprovalNodeInput{ApprovalStepInput: ApprovalStepInput{SLASeconds: 100000}}, nil, false},
		{"too large for a tiny calendar", &fakeTimerCalendars{def: tiny}, ApprovalNodeInput{ApprovalStepInput: ApprovalStepInput{SLASeconds: 36000}}, nil, true},
		{"wall clock is not the calendar's business", &fakeTimerCalendars{def: tiny}, ApprovalNodeInput{ApprovalStepInput: ApprovalStepInput{SLASeconds: 36000, SLAClock: "wall"}}, nil, false},
		{"no calendar at all passes (wall clock is honest)", &fakeTimerCalendars{}, ApprovalNodeInput{ApprovalStepInput: ApprovalStepInput{SLASeconds: 36000}}, nil, false},
		{"judged on the PINNED calendar, not the default", pinnedCals{def: vnDefaultCalendar(), byID: tiny},
			ApprovalNodeInput{ApprovalStepInput: ApprovalStepInput{SLASeconds: 36000}}, &ApprovalOptionsInput{CalendarID: "cal-tiny"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &ApprovalService{workingTime: mustCalc(t, tc.store)}
			err := s.checkSLAFitsCalendar(context.Background(), "t1", []ApprovalNodeInput{tc.node}, "steps", tc.opts)
			var ve *ValidationError
			if tc.wantErr != (errors.As(err, &ve) && ve.Code == "APPROVAL_SLA_AMOUNT_TOO_LARGE") {
				t.Fatalf("err = %v; wantErr %v (APPROVAL_SLA_AMOUNT_TOO_LARGE)", err, tc.wantErr)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error %v", err)
			}
		})
	}
}

// pinnedCals: the default calendar is roomy, the one a process pins by id ("cal-tiny") is not.
type pinnedCals struct{ def, byID *calmeta.BusinessCalendar }

func (p pinnedCals) GetByID(_ context.Context, _, id string) (*calmeta.BusinessCalendar, error) {
	if id == p.byID.ID {
		return p.byID, nil
	}
	return nil, calmeta.ErrCalendarNotFound
}

func (p pinnedCals) GetDefault(context.Context, string) (*calmeta.BusinessCalendar, error) {
	return p.def, nil
}
