//go:build integration

package services

// draft_field_indexfix_integration_test.go — #56 against a real MySQL. "Bật index ngay" (IndexFixNotice)
// sends the options it read, whole, plus indexed:true, to PUT /admin/fields/:id (an /admin route: the
// builder's overlay is on). Outside /studio — the customer app's /reports or a dashboard — that read is
// LIVE (GET /metadata/fields without X-Nexus-Draft). Either way the index applies live and the builder's
// pending field draft survives untouched.

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// stageOf reads hk_deals.stage the way ctx's caller reads GET /metadata/fields/hk_deals.
func (s *s78Env) stageOf(t *testing.T, ctx context.Context) metadata.FieldDefinition {
	t.Helper()
	fs, err := NewDraftFieldRepo(s.meta, s.drafts, s.gate).ListFields(ctx, s.tenant, s78EntityID(t, s))
	if err != nil {
		t.Fatalf("ListFields: %v", err)
	}
	for _, f := range fs {
		if f.Name == "stage" {
			return f
		}
	}
	t.Fatal("hk_deals has no stage field")
	return metadata.FieldDefinition{}
}

func optsOf(t *testing.T, raw *string) map[string]any {
	t.Helper()
	m, err := optionsMap(raw)
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	return m
}

// indexFix is IndexFixNotice's call: the options of the field it read, whole, plus indexed:true.
func (s *s78Env) indexFix(t *testing.T, f metadata.FieldDefinition) {
	t.Helper()
	opts := optsOf(t, f.Options)
	opts["indexed"] = true
	if _, err := s.fields.EditField(s.bctx, s.tenant, EditFieldInput{FieldID: f.ID, Options: opts}); err != nil {
		t.Fatalf("index fix: %v", err)
	}
}

// oneFieldDraft asserts one pending change, the field draft, and returns its document.
func (s *s78Env) oneFieldDraft(t *testing.T) string {
	t.Helper()
	rows, err := s.drafts.ListByType(s.bctx, s.tenant, DocTypeField)
	if err != nil {
		t.Fatal(err)
	}
	if n := s.draftCount(t, s.tenant); n != 1 || len(rows) != 1 {
		t.Fatalf("pending = %d, field drafts = %d; want 1 and 1", n, len(rows))
	}
	return string(rows[0].DraftJSON)
}

// draftLostValue is the builder adding a stage value in Studio: one field draft, live keeps three values.
func (s *s78Env) draftLostValue(t *testing.T, stageID string) string {
	t.Helper()
	if n := s.draftCount(t, s.tenant); n != 0 {
		t.Fatalf("pending before the edit = %d, want 0", n)
	}
	opts := map[string]any{"values": []any{"Proposal", "Closed Won", "Locked", "Lost"}}
	if _, err := s.fields.EditField(s.bctx, s.tenant, EditFieldInput{FieldID: stageID, Options: opts}); err != nil {
		t.Fatalf("Studio edit: %v", err)
	}
	return s.oneFieldDraft(t)
}

func hasLost(t *testing.T, raw *string) bool {
	t.Helper()
	vals, _ := optsOf(t, raw)["values"].([]any)
	for _, v := range vals {
		if v == "Lost" {
			return true
		}
	}
	return false
}

func (s *s78Env) assertIndexedLive(t *testing.T, id string) {
	t.Helper()
	lf := s.liveField(t, id)
	opts := optsOf(t, &lf.Options)
	if opts["indexed"] != true || hasLost(t, &lf.Options) {
		t.Errorf("live options = %s, want indexed:true applied now and the draft's value not published", lf.Options)
	}
}

func TestIndexFixFromLiveRead_KeepsFieldDraft_Integration(t *testing.T) {
	s := s78Setup(t)
	stage := s.stageOf(t, s.bctx)
	draft := s.draftLostValue(t, stage.ID)

	// /reports or a dashboard: the builder's field list without X-Nexus-Draft is the live one.
	liveRead := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: s.builder, TenantID: s.tenant, HomeTenantID: s.tenant, IsBuilder: true})
	live := s.stageOf(t, liveRead)
	if hasLost(t, live.Options) || live.DraftState != "" {
		t.Fatalf("setup: the live read carries the draft: %+v", live)
	}
	s.indexFix(t, live)

	if got := s.oneFieldDraft(t); got != draft {
		t.Errorf("the index fix changed the field draft:\n got %s\nwant %s", got, draft)
	}
	s.assertIndexedLive(t, stage.ID)
	if b := s.stageOf(t, s.bctx); b.DraftState != draftStateEdited || !hasLost(t, b.Options) || optsOf(t, b.Options)["indexed"] != true {
		t.Errorf("builder overlay = %+v, want the pending value, indexed, marked edited", b)
	}
}

// /studio reads over the overlay: the draft's options go back with indexed:true. Unchanged by #56, and
// a hand revert (the live options back, indexed as it stands) still drops the draft.
func TestIndexFixInStudio_KeepsFieldDraft_HandRevertDropsIt_Integration(t *testing.T) {
	s := s78Setup(t)
	stage := s.stageOf(t, s.bctx)
	draft := s.draftLostValue(t, stage.ID)

	s.indexFix(t, s.stageOf(t, s.bctx))
	if got := s.oneFieldDraft(t); got != draft {
		t.Errorf("the index fix changed the field draft:\n got %s\nwant %s", got, draft)
	}
	s.assertIndexedLive(t, stage.ID)

	liveRead := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: s.builder, TenantID: s.tenant, HomeTenantID: s.tenant, IsBuilder: true})
	revert := optsOf(t, s.stageOf(t, liveRead).Options)
	if _, err := s.fields.EditField(s.bctx, s.tenant, EditFieldInput{FieldID: stage.ID, Options: revert}); err != nil {
		t.Fatalf("hand revert: %v", err)
	}
	if n := s.draftCount(t, s.tenant); n != 0 {
		t.Errorf("pending after a hand revert to the live options = %d, want 0", n)
	}
}
