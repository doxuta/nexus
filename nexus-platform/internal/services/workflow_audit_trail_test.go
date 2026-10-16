package services

// workflow-writes-leave-no-trail (queue #86, ../specs/029-audit-log/spec.md §4): the workflow
// engine writes records through WorkflowRecordMutator, which BYPASSES RecordService — and with it
// the RM-19 audit emit. A field_update / assign / set_deadline changed a value and the trail had no
// row, so "who changed this" had no answer for exactly the changes automation makes.
//
// The tests capture at the emit seam (captureAuditor), like audit_sensitive_redact_test.go: the
// event handed to the Auditor is exactly what the async writer persists.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

// trailStore is the dynrepo slice the mutator touches, with a canned before-image.
type trailStore struct {
	before    map[string]any
	updateErr error
	written   map[string]any
	owner     string
}

func (s *trailStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, data map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.written = data
	return nil
}

func (s *trailStore) SetOwner(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, newOwnerID string) error {
	s.owner = newOwnerID
	return nil
}

func (s *trailStore) GetRecord(_ context.Context, _, _, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return &dynrepo.Record{ID: id, TenantID: "tenant-1", Data: s.before}, nil
}

func trailMutator(t *testing.T, store *trailStore) (*WorkflowRecordMutator, *captureAuditor) {
	t.Helper()
	aud := &captureAuditor{}
	return NewWorkflowRecordMutator(store).WithCatalog(sensitiveFixture(t)).WithAuditor(aud), aud
}

func TestWorkflowFieldUpdate_LeavesAuditRow(t *testing.T) {
	tests := []struct {
		name       string
		field      string
		value      any
		wantBefore any
		wantAfter  any
	}{
		{name: "plain field carries real values", field: "name", value: "Công ty Mới",
			wantBefore: "Công ty Cũ", wantAfter: "Công ty Mới"},
		{name: "sensitive field is masked on both sides", field: "so_cccd", value: "079203004455",
			wantBefore: auditRedactedMarker, wantAfter: auditRedactedMarker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &trailStore{before: map[string]any{"name": "Công ty Cũ", "so_cccd": "001099001122"}}
			mut, aud := trailMutator(t, store)
			if err := mut.UpdateField(context.Background(), "customers", "tenant-1", "rec-9", workflow.SystemActorID, tt.field, tt.value); err != nil {
				t.Fatalf("UpdateField: %v", err)
			}
			evt := soleEvent(t, aud)
			if evt.ActorUserID != workflow.SystemActorID || evt.Action != audit.ActionUpdate ||
				evt.TenantID != "tenant-1" || evt.EntityName != "customers" || evt.RecordID != "rec-9" {
				t.Fatalf("event = %+v; want an update on customers/rec-9 by the engine's system actor", evt)
			}
			if evt.Diff == nil || evt.Diff.Before[tt.field] != tt.wantBefore || evt.Diff.After[tt.field] != tt.wantAfter {
				t.Fatalf("diff = %+v; want %s: %v → %v", evt.Diff, tt.field, tt.wantBefore, tt.wantAfter)
			}
			if len(evt.Diff.After) != 1 {
				t.Errorf("diff must carry only the field the rule wrote, got %+v", evt.Diff.After)
			}
		})
	}
}

// No write, no row: a refused value or a failed store write changed nothing.
func TestWorkflowFieldUpdate_NoAuditWhenNothingWritten(t *testing.T) {
	t.Run("validation refused", func(t *testing.T) {
		mut, aud := trailMutator(t, &trailStore{})
		if err := mut.UpdateField(context.Background(), "customers", "tenant-1", "rec-9", workflow.SystemActorID, "no_such_field", "x"); err == nil {
			t.Fatal("unknown field must be refused")
		}
		if len(aud.events) != 0 {
			t.Fatalf("refused write emitted %d audit rows", len(aud.events))
		}
	})
	t.Run("store write failed", func(t *testing.T) {
		mut, aud := trailMutator(t, &trailStore{updateErr: errors.New("deadlock")})
		if err := mut.UpdateField(context.Background(), "customers", "tenant-1", "rec-9", workflow.SystemActorID, "name", "x"); err == nil {
			t.Fatal("store error must surface")
		}
		if len(aud.events) != 0 {
			t.Fatalf("failed write emitted %d audit rows", len(aud.events))
		}
	})
	t.Run("value already in place", func(t *testing.T) {
		// A rule that re-stamps the value the record already holds (e.g. after_update set
		// stage=Won, fired by every later edit) changed nothing — a row per firing is noise.
		mut, aud := trailMutator(t, &trailStore{before: map[string]any{"name": "Giữ nguyên"}})
		if err := mut.UpdateField(context.Background(), "customers", "tenant-1", "rec-9", workflow.SystemActorID, "name", "Giữ nguyên"); err != nil {
			t.Fatalf("UpdateField: %v", err)
		}
		if len(aud.events) != 0 {
			t.Fatalf("no-change write emitted %d audit rows: %+v", len(aud.events), aud.events)
		}
	})
}

// The assign action hands the record over (owner_id is what ACL enforces on). That is the change
// "who gave this lead to her" asks about, so it gets the same row a human "Đổi chủ" writes
// (record_owner.go: diff {ownerId}).
func TestWorkflowSetOwner_LeavesAuditRow(t *testing.T) {
	store := &trailStore{}
	mut, aud := trailMutator(t, store)
	if err := mut.SetOwner(context.Background(), "customers", "tenant-1", "rec-9", workflow.SystemActorID, "user-lan"); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	evt := soleEvent(t, aud)
	if evt.ActorUserID != workflow.SystemActorID || evt.Action != audit.ActionUpdate || evt.RecordID != "rec-9" {
		t.Fatalf("event = %+v; want an update on rec-9 by the system actor", evt)
	}
	if evt.Diff == nil || evt.Diff.After["ownerId"] != "user-lan" {
		t.Fatalf("diff = %+v; want after.ownerId = user-lan", evt.Diff)
	}
}

// No auditor wired (legacy wiring / unit fakes) must stay a silent no-op, never a panic.
func TestWorkflowMutator_NilAuditorIsSafe(t *testing.T) {
	store := &trailStore{before: map[string]any{"name": "a"}}
	mut := NewWorkflowRecordMutator(store).WithCatalog(sensitiveFixture(t))
	if err := mut.UpdateField(context.Background(), "customers", "tenant-1", "rec-9", workflow.SystemActorID, "name", "b"); err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	if err := mut.SetOwner(context.Background(), "customers", "tenant-1", "rec-9", workflow.SystemActorID, "u"); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
}
