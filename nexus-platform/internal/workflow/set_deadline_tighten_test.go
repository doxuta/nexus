package workflow

import (
	"context"
	"testing"
	"time"
)

// Queue #81 / specs/043 S14a: a set_deadline in `tighten_only` mode re-computes the deadline from the
// record's creation instant and applies it ONLY when it is earlier than the deadline on the record,
// and only when that deadline is provably the machine's own (the field still equals the machine
// stamp kept in `stampField`). A hand-typed deadline — or one the machine cannot prove it wrote,
// like every ticket created before the stamp existed — is never touched.

// tightenAction is the after_update tier the Ticketing pack ships: recompute from created_at into
// due_date, stamp due_date_auto, never lengthen.
func tightenAction(amount int, unit string) Action {
	return Action{
		Type: ActionSetDeadline, Field: "due_date", Offset: &TimerOffset{Amount: amount, Unit: unit},
		Mode: DeadlineModeTightenOnly, StampField: "due_date_auto", Anchor: DeadlineAnchorCreatedAt,
	}
}

// mysqlStamp renders an instant the way the record store hands a datetime back (MySQL literal, UTC).
func mysqlStamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

func TestSetDeadline_TightenOnly_StampField(t *testing.T) {
	created := time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC) // Mon 09:00 ICT
	promised := created.Add(40 * time.Hour)
	for _, tc := range []struct {
		name      string
		data      map[string]any
		createdAt time.Time
		action    Action
		wantWrite bool
		want      time.Time
		wantErr   bool
	}{
		{
			name:      "machine deadline, new tier earlier ⇒ applied to field and stamp",
			data:      map[string]any{"due_date": mysqlStamp(promised), "due_date_auto": mysqlStamp(promised)},
			createdAt: created, action: tightenAction(4, TimerUnitHours),
			wantWrite: true, want: created.Add(4 * time.Hour),
		},
		{
			name:      "machine deadline, new tier later ⇒ promised deadline kept",
			data:      map[string]any{"due_date": mysqlStamp(created.Add(4 * time.Hour)), "due_date_auto": mysqlStamp(created.Add(4 * time.Hour))},
			createdAt: created, action: tightenAction(40, TimerUnitHours),
		},
		{
			name:      "same instant ⇒ nothing to write",
			data:      map[string]any{"due_date": mysqlStamp(created.Add(4 * time.Hour)), "due_date_auto": mysqlStamp(created.Add(4 * time.Hour))},
			createdAt: created, action: tightenAction(4, TimerUnitHours),
		},
		{
			name:      "field differs from stamp (hand-typed) ⇒ untouched",
			data:      map[string]any{"due_date": mysqlStamp(created.Add(100 * time.Hour)), "due_date_auto": mysqlStamp(promised)},
			createdAt: created, action: tightenAction(4, TimerUnitHours),
		},
		{
			name:      "no stamp (ticket from before the stamp existed) ⇒ untouched",
			data:      map[string]any{"due_date": mysqlStamp(promised)},
			createdAt: created, action: tightenAction(4, TimerUnitHours),
		},
		{
			name:      "stamp present but deadline cleared by hand ⇒ untouched",
			data:      map[string]any{"due_date": nil, "due_date_auto": mysqlStamp(promised)},
			createdAt: created, action: tightenAction(4, TimerUnitHours),
		},
		{
			name:      "RFC3339 on one side, MySQL literal on the other ⇒ same instant, still the machine's",
			data:      map[string]any{"due_date": promised.Format(time.RFC3339), "due_date_auto": mysqlStamp(promised)},
			createdAt: created, action: tightenAction(4, TimerUnitHours),
			wantWrite: true, want: created.Add(4 * time.Hour),
		},
		{
			name:    "anchor created_at on an event that carries no creation instant ⇒ error, no write",
			data:    map[string]any{"due_date": mysqlStamp(promised), "due_date_auto": mysqlStamp(promised)},
			action:  tightenAction(4, TimerUnitHours),
			wantErr: true,
		},
		{
			name: "tighten_only without a stampField (rule stored by some other path) ⇒ error, no write",
			data: map[string]any{"due_date": mysqlStamp(promised)}, createdAt: created,
			action: Action{Type: ActionSetDeadline, Field: "due_date", Offset: &TimerOffset{Amount: 4, Unit: TimerUnitHours},
				Mode: DeadlineModeTightenOnly, Anchor: DeadlineAnchorCreatedAt},
			wantErr: true,
		},
		{
			name: "unknown mode ⇒ error, no write",
			data: map[string]any{}, createdAt: created,
			action:  Action{Type: ActionSetDeadline, Field: "due_date", Offset: &TimerOffset{Amount: 4, Unit: TimerUnitHours}, Mode: "extend_only"},
			wantErr: true,
		},
		{
			name: "unknown anchor ⇒ error, no write",
			data: map[string]any{}, createdAt: created,
			action:  Action{Type: ActionSetDeadline, Field: "due_date", Offset: &TimerOffset{Amount: 4, Unit: TimerUnitHours}, Anchor: "updated_at"},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mut := &fakeMutator{rows: map[string]map[string]any{"rec-1": tc.data}} // #172: tighten_only reads the row
			evt := Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "rec-1", Trigger: TriggerAfterUpdate,
				Data: tc.data, CreatedAt: tc.createdAt}
			wf := Workflow{ID: "wf-1", TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerAfterUpdate, Actions: []Action{tc.action}}
			_, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut})
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantWrite {
				if len(mut.calls) != 0 {
					t.Fatalf("wrote %+v, want no write", mut.calls)
				}
				return
			}
			if len(mut.calls) != 2 {
				t.Fatalf("got %d writes (%+v), want 2 (deadline, then stamp)", len(mut.calls), mut.calls)
			}
			if mut.calls[0].field != "due_date" || mut.calls[1].field != "due_date_auto" {
				t.Fatalf("wrote fields %q then %q, want due_date then due_date_auto", mut.calls[0].field, mut.calls[1].field)
			}
			for _, c := range mut.calls {
				if got := parseWritten(t, c.value); !got.Equal(tc.want) {
					t.Fatalf("%s = %s, want %s", c.field, got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
				}
			}
		})
	}
}

// The US11 Independent Test in engine terms: a Low ticket raised 09:00 Monday is recomputed as
// Urgent (4 business hours) FROM ITS CREATION, not from the moment someone changed the priority —
// so the answer is 13:00 Monday no matter when the rule fires.
func TestSetDeadline_TightenOnly_AnchorsOnCreationThroughTheCalendar(t *testing.T) {
	sched := mustSchedule(t)
	created := time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC) // Mon 09:00 ICT
	promised, err := sched.AddBusinessDuration(created, 40*time.Hour)
	if err != nil {
		t.Fatalf("AddBusinessDuration: %v", err)
	}
	row := map[string]any{"due_date": mysqlStamp(promised), "due_date_auto": mysqlStamp(promised)}
	mut := &fakeMutator{rows: map[string]map[string]any{"rec-1": row}}
	evt := Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "rec-1", Trigger: TriggerAfterUpdate, CreatedAt: created,
		Data: row}
	wf := Workflow{ID: "wf-1", TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerAfterUpdate,
		Actions: []Action{tightenAction(4, TimerUnitBusinessHours)}}
	if _, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut, sched: &fakeScheduleResolver{sched: sched}}); err != nil {
		t.Fatalf("runActions: %v", err)
	}
	want := time.Date(2026, 9, 28, 6, 0, 0, 0, time.UTC) // Mon 13:00 ICT
	if len(mut.calls) != 2 {
		t.Fatalf("got %d writes, want 2", len(mut.calls))
	}
	if got := parseWritten(t, mut.calls[0].value); !got.Equal(want) {
		t.Fatalf("due_date = %s, want %s (09:00 + 4 business hours)", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// DAILY-QUEUE #172: tighten_only decides against the RECORD, not the event snapshot. The rule runs
// on the async dispatcher, so the window between "priority saved" and "rule runs" is the queue's
// delay (ALERTS_WORKERS workers; seconds under a mass edit or import) — not milliseconds — and
// anything saved in it is invisible to the snapshot.
func TestSetDeadline_TightenOnly_ReadsTheRowNotTheSnapshot(t *testing.T) {
	created := time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)
	promised := created.Add(40 * time.Hour)
	handTyped := created.Add(100 * time.Hour)
	machine := map[string]any{"due_date": mysqlStamp(promised), "due_date_auto": mysqlStamp(promised)}
	wf := Workflow{ID: "wf-1", TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerAfterUpdate,
		Actions: []Action{tightenAction(4, TimerUnitHours)}}
	evt := func(data map[string]any) Event {
		return Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "rec-1", Trigger: TriggerAfterUpdate, Data: data, CreatedAt: created}
	}

	t.Run("a deadline typed after the priority change is kept, though the snapshot shows the machine's", func(t *testing.T) {
		mut := &fakeMutator{rows: map[string]map[string]any{"rec-1": {"due_date": mysqlStamp(handTyped), "due_date_auto": mysqlStamp(promised)}}}
		if _, err := runActions(context.Background(), wf, evt(machine), actionDeps{mut: mut}); err != nil {
			t.Fatalf("runActions: %v", err)
		}
		if len(mut.calls) != 0 {
			t.Fatalf("wrote %+v over a hand-typed deadline", mut.calls)
		}
	})

	// The reverse order (US11 AC1): the priority changed before the after_create event was handled,
	// so the snapshot has no stamp yet — but by the time this rule runs the row has it.
	t.Run("a stamp that landed after the snapshot counts", func(t *testing.T) {
		mut := &fakeMutator{rows: map[string]map[string]any{"rec-1": machine}}
		if _, err := runActions(context.Background(), wf, evt(map[string]any{"priority": "Urgent"}), actionDeps{mut: mut}); err != nil {
			t.Fatalf("runActions: %v", err)
		}
		if len(mut.calls) != 2 || !parseWritten(t, mut.calls[0].value).Equal(created.Add(4*time.Hour)) {
			t.Fatalf("writes = %+v; want the Urgent tier from creation on due_date and its stamp", mut.calls)
		}
	})

	// The acceptance of #172: a hand-typed save lands between this rule's read and its write. The
	// write is conditional on the version it read, so it changes nothing; the rule reads again,
	// sees the hand-typed deadline, and keeps it.
	t.Run("a save between read and write: the conditional write loses, the re-read keeps it", func(t *testing.T) {
		mut := &fakeMutator{rows: map[string]map[string]any{"rec-1": machine}, conflicts: 1,
			raceRow: map[string]any{"due_date": mysqlStamp(handTyped), "due_date_auto": mysqlStamp(promised)}}
		if _, err := runActions(context.Background(), wf, evt(machine), actionDeps{mut: mut}); err != nil {
			t.Fatalf("runActions: %v", err)
		}
		if len(mut.calls) != 0 || mut.casCalls != 1 {
			t.Fatalf("writes = %+v after %d conditional attempts; want the one attempt refused and nothing written", mut.calls, mut.casCalls)
		}
		if got := mut.rows["rec-1"]["due_date"]; got != mysqlStamp(handTyped) {
			t.Fatalf("due_date = %v; want the hand-typed %s", got, mysqlStamp(handTyped))
		}
	})

	t.Run("a record that keeps changing fails the run instead of looping", func(t *testing.T) {
		mut := &fakeMutator{rows: map[string]map[string]any{"rec-1": machine}, conflicts: 99, raceRow: machine}
		if _, err := runActions(context.Background(), wf, evt(machine), actionDeps{mut: mut}); err == nil {
			t.Fatal("runActions: want an error after the bounded retries")
		}
		if len(mut.calls) != 0 || mut.casCalls != 3 {
			t.Fatalf("writes = %+v after %d attempts; want 3 refused attempts and nothing written", mut.calls, mut.casCalls)
		}
	})
}

// overwrite (the default, today's behaviour) with a stampField writes the stamp next to the value —
// this is how the pack's creation tiers mark a deadline as the machine's.
func TestSetDeadline_OverwriteWithStampFieldStampsBoth(t *testing.T) {
	mut := &fakeMutator{}
	wf := deadlineWF(4, TimerUnitHours)
	wf.Actions[0].StampField = "due_date_auto"
	if _, err := runActions(context.Background(), wf, deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut}); err != nil {
		t.Fatalf("runActions: %v", err)
	}
	if len(mut.calls) != 2 || mut.calls[0].field != "due_date" || mut.calls[1].field != "due_date_auto" {
		t.Fatalf("writes = %+v, want due_date then due_date_auto", mut.calls)
	}
	if mut.calls[0].value != mut.calls[1].value {
		t.Fatalf("stamp %v differs from the deadline %v — the next tighten_only run would treat the machine's own deadline as hand-typed", mut.calls[1].value, mut.calls[0].value)
	}
}
