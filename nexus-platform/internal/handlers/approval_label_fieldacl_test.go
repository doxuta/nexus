package handlers

// The pending-approval list decorates each row with a display label read off ANOTHER entity's rows.
// Field ACL applies to a value regardless of which surface reads it — but this one skipped the
// check that every record read performs, and the label preference list includes `email`. So a role
// with `email` explicitly hidden in sys_field_acl still saw it here, in a list they open daily.
//
// Skipping the label leaves it empty, which is the path this loop already takes for every other
// reason a label cannot be resolved — the FE falls back to the mono slug+id, an honest answer.

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

type labelMeta struct{}

func (labelMeta) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	return &metadata.EntityDefinition{ID: "e1", Name: name, Label: "Khách hàng"}, nil
}
func (labelMeta) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	// No `name`-like field, so pickApprovalLabelField falls through to `email` — the exact case
	// where a hidden field would otherwise be surfaced.
	return []metadata.FieldDefinition{{Name: "email", FieldType: "text"}}, nil
}

type spyLabels struct{ called bool }

func (s *spyLabels) BatchFetchLabels(_ context.Context, _, _, _ string, ids []string) ([]dynrepo.LabelResult, error) {
	s.called = true
	out := make([]dynrepo.LabelResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, dynrepo.LabelResult{ID: id, Label: "an@khach.vn"})
	}
	return out, nil
}

type fixedHidden struct{ hidden map[string]struct{} }

func (f fixedHidden) HiddenFields(_ context.Context, _, _, _ string, isAdmin bool) (map[string]struct{}, error) {
	if isAdmin {
		return nil, nil // admin bypasses field ACL, same as everywhere else
	}
	return f.hidden, nil
}

func pendingRows() []bpm.Instance {
	return []bpm.Instance{{ID: "ai-1", EntityName: "crm_accounts", RecordID: "rec-1"}}
}

func TestHumanizePending_HonoursFieldACL(t *testing.T) {
	t.Run("label field hidden for this role → NOT fetched, label stays empty", func(t *testing.T) {
		labels := &spyLabels{}
		h := NewApprovalHandler(nil, zap.NewNop()).
			WithRecordLabels(labelMeta{}, labels).
			WithFieldACL(fixedHidden{hidden: map[string]struct{}{"email": {}}})

		rows := h.humanizePending(context.Background(), "t1", "role-staff", false, pendingRows())
		if labels.called {
			t.Error("the hidden field was fetched anyway — field ACL is not a per-surface opinion")
		}
		if len(rows) != 1 || rows[0].RecordLabel != "" {
			t.Errorf("row = %+v; want an empty label so the FE falls back to the id", rows[0])
		}
	})

	t.Run("field visible → decorated as before", func(t *testing.T) {
		labels := &spyLabels{}
		h := NewApprovalHandler(nil, zap.NewNop()).
			WithRecordLabels(labelMeta{}, labels).
			WithFieldACL(fixedHidden{hidden: map[string]struct{}{}})

		rows := h.humanizePending(context.Background(), "t1", "role-staff", false, pendingRows())
		if !labels.called || rows[0].RecordLabel == "" {
			t.Errorf("a visible field was not decorated: called=%v row=%+v", labels.called, rows[0])
		}
	})

	t.Run("admin is not gated", func(t *testing.T) {
		labels := &spyLabels{}
		h := NewApprovalHandler(nil, zap.NewNop()).
			WithRecordLabels(labelMeta{}, labels).
			WithFieldACL(fixedHidden{hidden: map[string]struct{}{"email": {}}})

		rows := h.humanizePending(context.Background(), "t1", "", true, pendingRows())
		if !labels.called || rows[0].RecordLabel == "" {
			t.Errorf("admin lost the label: called=%v row=%+v", labels.called, rows[0])
		}
	})
}

// #272: the table's explicit label field beats the approval preference list.
type explicitLabelMeta struct{}

func (explicitLabelMeta) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	o := `{"labelField":"ho_ten"}`
	return &metadata.EntityDefinition{ID: "e1", Name: name, Label: "Khách hàng", Options: &o}, nil
}
func (explicitLabelMeta) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	// `email` would win the preference list; the builder chose ho_ten.
	return []metadata.FieldDefinition{{Name: "email", FieldType: "email"}, {Name: "ho_ten", FieldType: "text"}}, nil
}

type fieldSpyLabels struct{ got string }

func (s *fieldSpyLabels) BatchFetchLabels(_ context.Context, _, _, lf string, ids []string) ([]dynrepo.LabelResult, error) {
	s.got = lf
	return []dynrepo.LabelResult{{ID: ids[0], Label: "Nguyễn An"}}, nil
}

func TestHumanizePending_ExplicitLabelFieldWins(t *testing.T) {
	labels := &fieldSpyLabels{}
	h := NewApprovalHandler(nil, zap.NewNop()).WithRecordLabels(explicitLabelMeta{}, labels)
	rows := h.humanizePending(context.Background(), "t1", "", true, pendingRows())
	if labels.got != "ho_ten" || rows[0].RecordLabel != "Nguyễn An" {
		t.Errorf("label read from %q, row=%+v; want the table's chosen field ho_ten", labels.got, rows[0])
	}
}
