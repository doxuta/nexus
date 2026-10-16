package bpm

import (
	"context"
	"fmt"
	"time"
)

// SLABasis is WHAT a deadline was computed on (specs/043 S1, #39 — D-8). It travels with the
// deadline so the screen can say it: a wall-clock number on a tenant with no calendar must never
// read like a working-hours one.
//
// Four values, deliberately not merged (the three wall-clock states of specs/037 §2.4):
//   - business:            the working calendar was walked.
//   - wall:                wall clock ON PURPOSE — the step chose slaClock "wall", or the instance
//     was pinned before this slice (its steps carry no clock) and keeps the clock it started on.
//   - wall_no_calendar:    the step wanted working hours, but the tenant has no live calendar (#82).
//   - wall_calendar_error: the step wanted working hours, the calendar could not be read (store
//     failure on the async arming path, or a calendar with no reachable working time) — a
//     temporary number, and it says so.
type SLABasis string

const (
	SLABasisBusiness          SLABasis = "business"
	SLABasisWall              SLABasis = "wall"
	SLABasisWallNoCalendar    SLABasis = "wall_no_calendar"
	SLABasisWallCalendarError SLABasis = "wall_calendar_error"
)

// Step clocks (Step.SLAClock). A LIVE step without a clock means business for every instance
// started from now on — and is pinned explicitly as such (pinDefinition). A PINNED step without a
// clock was pinned before this slice and means wall.
const (
	SLAClockBusiness = "business"
	SLAClockWall     = "wall"
)

// ValidSLAClock reports whether c is an acceptable Step.SLAClock ("" = the default).
func ValidSLAClock(c string) bool {
	return c == "" || c == SLAClockBusiness || c == SLAClockWall
}

// DeadlineCalculator is the engine's consumer-side view of the working calendar (D-8). The real
// implementation is services.BPMDeadlineCalculator, over the SAME pinned-then-default calendar
// lookup the workflow timer and set_deadline use — one answer to "when is the office open".
//
// DueAt returns the instant d of WORKING time after from, and the basis it was computed on. err is
// reserved for "the calendar store could not be read": the caller decides — a person's action
// fails with it, the async arming path degrades to wall clock under wall_calendar_error. "No
// calendar" and "no reachable working time" are answers, not errors (wall_no_calendar /
// wall_calendar_error with a wall-clock instant).
type DeadlineCalculator interface {
	DueAt(ctx context.Context, tenantID, calendarID string, from time.Time, d time.Duration) (time.Time, SLABasis, error)
}

// BusinessMeter is the optional second half of the calendar view (S13): how much WORKING time lies
// between two instants on the calendar a process pinned ("" = the tenant default). The pause needs
// it to freeze the remaining time of a deadline that was counted in working hours. nil schedule =
// the tenant has no calendar (the caller then counts wall time).
type BusinessMeter interface {
	BusinessBetween(ctx context.Context, tenantID, calendarID string, from, to time.Time) (time.Duration, bool, error)
}

// WithDeadlines wires the calendar-backed deadline arithmetic. Chainable. Unwired, every deadline
// is wall clock — which is only acceptable in a unit test; the server refuses to start without it
// (RequireDeadlines).
func (e *AsyncEngine) WithDeadlines(c DeadlineCalculator) *AsyncEngine {
	e.deadlines = c
	return e
}

// WithDeadlines — see AsyncEngine.WithDeadlines.
func (d *Decider) WithDeadlines(c DeadlineCalculator) *Decider {
	d.deadlines = c
	return d
}

// WithDeadlines — see AsyncEngine.WithDeadlines.
func (r *Requester) WithDeadlines(c DeadlineCalculator) *Requester {
	r.deadlines = c
	return r
}

// RequireDeadlines is the boot-time wiring check (D-8, T021): each of the three places that open
// a step must have the calendar. Missing on any of them, a "4 working hours" step would quietly be
// timed on the wall clock on that path only — the exact silent fallback this slice removes — so
// the server fails fast instead (same posture as the MFA gate check in cmd/server).
func RequireDeadlines(e *AsyncEngine, d *Decider, r *Requester) error {
	var missing []string
	if e == nil || e.deadlines == nil {
		missing = append(missing, "engine")
	}
	if d == nil || d.deadlines == nil {
		missing = append(missing, "decider")
	}
	if r == nil || r.deadlines == nil {
		missing = append(missing, "requester")
	}
	if len(missing) > 0 {
		return fmt.Errorf("bpm.RequireDeadlines: approval deadline calculator not wired on %v", missing)
	}
	return nil
}

// pinDefinition is the definition a NEW instance pins: stable ids (S0) and, for every step whose
// live config names no clock, slaClock "business" written EXPLICITLY — so the instance keeps that
// answer however the live process is edited later, and a pin without the key can only ever mean
// "pinned before S1" (wall). Used by both arming paths; never on the read side.
//
// A graph-form process pins its graph — the same ids and explicit clocks on every node — beside an
// EMPTY steps array (D-2: a binary older than S3a2 refuses rather than misreads it).
func pinDefinition(p Process) *Definition {
	if p.Graph != nil {
		g := p.Graph.normalized()
		for i := range g.Nodes {
			if g.Nodes[i].SLAClock == "" {
				g.Nodes[i].SLAClock = SLAClockBusiness
			}
		}
		return &Definition{Steps: []Step{}, Graph: g, Outcome: p.Outcome, Options: p.Options, Version: p.Version}
	}
	steps := NormalizeSteps(p.Steps)
	for i := range steps {
		if steps[i].SLAClock == "" {
			steps[i].SLAClock = SLAClockBusiness
		}
	}
	return &Definition{Steps: steps, Outcome: p.Outcome, Options: p.Options, Version: p.Version}
}

// stepDue computes the deadline of steps[i] entered at now. pinned=false means the instance has no
// pin (armed before 000065) and reads the live process: always wall clock (D-9) — its first step
// was timed on the wall clock and it finishes on it, whatever the live steps say now.
//
// Returns (nil, "", nil) for a step without an SLA. err is the calculator's store error, returned
// untouched for the caller to decide (see DeadlineCalculator).
func stepDue(ctx context.Context, calc DeadlineCalculator, tenantID, calendarID string, steps []Step, i int, pinned bool, now time.Time) (*time.Time, SLABasis, error) {
	if i < 0 || i >= len(steps) || steps[i].SLASeconds <= 0 {
		return nil, "", nil
	}
	d := time.Duration(steps[i].SLASeconds) * time.Second
	if !pinned || steps[i].SLAClock != SLAClockBusiness || calc == nil {
		// calc == nil only in unit tests: the server refuses to start unwired (RequireDeadlines).
		t := now.Add(d)
		return &t, SLABasisWall, nil
	}
	due, basis, err := calc.DueAt(ctx, tenantID, calendarID, now, d)
	if err != nil {
		return nil, "", err
	}
	due = due.UTC()
	return &due, basis, nil
}

// CurrentSLABasis returns the basis of the instance's CURRENT deadline ("" when it has none). The
// first step's basis rides the pin (Definition.FirstDueBasis, written once at arm/submit); every
// later deadline's rides the history entry of the decision that set it — so the newest entry that
// carries one wins, and a delegation (which keeps the deadline) carries none. Nothing recorded at
// all = the deadline was computed before this slice, on the wall clock.
//
// ponytail: interim home until Migration A (S3a) gives the instance a context.sla.basis column —
// its backfill must read THIS, not stamp every open instance "wall".
func (inst *Instance) CurrentSLABasis() SLABasis {
	if inst == nil || inst.SLADueAt == nil {
		return ""
	}
	for i := len(inst.History) - 1; i >= 0; i-- {
		if b := inst.History[i].SLABasis; b != "" {
			return b
		}
	}
	if inst.Definition != nil && inst.Definition.FirstDueBasis != "" {
		return inst.Definition.FirstDueBasis
	}
	return SLABasisWall
}
