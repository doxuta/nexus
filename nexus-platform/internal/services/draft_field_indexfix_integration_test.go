//go:build integration

package services

// draft_field_indexfix_integration_test.go — #56 against a real MySQL. "Bật index ngay" (IndexFixNotice)
// runs from /studio and from the customer app's /reports or a dashboard, whose field read is LIVE (GET
// /metadata/fields without X-Nexus-Draft). It sends PUT /admin/fields/:id {"indexed": true} — an /admin
// route, so the builder's overlay is on — and the server lays the flag onto the options as they stand:
// the index applies live, the builder's pending field draft survives untouched. Whole options keep
// their meaning: what is sent is what the builder wants, a revert to live included.

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

// liveRead is the builder on a customer-app screen: no X-Nexus-Draft, so reads are live.
func (s *s78Env) liveRead() context.Context {
	return metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: s.builder, TenantID: s.tenant, HomeTenantID: s.tenant, IsBuilder: true})
}

func optsOf(t *testing.T, raw *string) map[string]any {
	t.Helper()
	m, err := optionsMap(raw)
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	return m
}

// withIndexedOn is what the Studio inspector sends on ticking "Đánh index": the options it holds, whole.
func withIndexedOn(t *testing.T, raw *string) map[string]any {
	t.Helper()
	m := optsOf(t, raw)
	m["indexed"] = true
	return m
}

func (s *s78Env) edit(t *testing.T, in EditFieldInput) {
	t.Helper()
	if _, err := s.fields.EditField(s.bctx, s.tenant, in); err != nil {
		t.Fatalf("EditField %+v: %v", in, err)
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
	s.edit(t, EditFieldInput{FieldID: stageID, Options: map[string]any{"values": []any{"Proposal", "Closed Won", "Locked", "Lost"}}})
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

func (s *s78Env) assertLiveStage(t *testing.T, id string, lost bool) {
	t.Helper()
	lf := s.liveField(t, id)
	if optsOf(t, &lf.Options)["indexed"] != true || hasLost(t, &lf.Options) != lost {
		t.Errorf("live stage options = %s, want indexed:true and Lost=%v", lf.Options, lost)
	}
}

func TestIndexFixFromLiveRead_KeepsFieldDraft_Integration(t *testing.T) {
	s := s78Setup(t)
	stage := s.stageOf(t, s.bctx)
	draft := s.draftLostValue(t, stage.ID)

	live := s.stageOf(t, s.liveRead()) // /reports or a dashboard
	if hasLost(t, live.Options) || live.DraftState != "" {
		t.Fatalf("setup: the live read carries the draft: %+v", live)
	}
	on := true
	s.edit(t, EditFieldInput{FieldID: live.ID, Indexed: &on})

	if got := s.oneFieldDraft(t); got != draft {
		t.Errorf("the index fix changed the field draft:\n got %s\nwant %s", got, draft)
	}
	s.assertLiveStage(t, stage.ID, false)
	if b := s.stageOf(t, s.bctx); b.DraftState != draftStateEdited || !hasLost(t, b.Options) || optsOf(t, b.Options)["indexed"] != true {
		t.Errorf("builder overlay = %+v, want the pending value, indexed, marked edited", b)
	}
}

// The Studio inspector reads over the overlay: ticking "Đánh index" sends the draft's options whole with
// indexed:true. As on main: the draft is written back as it was, the index applies live.
func TestStudioInspectorIndex_KeepsFieldDraft_Integration(t *testing.T) {
	s := s78Setup(t)
	stage := s.stageOf(t, s.bctx)
	draft := s.draftLostValue(t, stage.ID)

	s.edit(t, EditFieldInput{FieldID: stage.ID, Options: withIndexedOn(t, s.stageOf(t, s.bctx).Options)})
	if got := s.oneFieldDraft(t); got != draft {
		t.Errorf("ticking index changed the field draft:\n got %s\nwant %s", got, draft)
	}
	s.assertLiveStage(t, stage.ID, false)
}

// Whole options mean what they say, as on main: options equal to live (plus indexed) drop the pending
// options draft. The two ways a builder sends them: deleting the drafted row and ticking "Đánh index" in
// one save, or ticking it in an inspector opened before the draft (its options are the live ones).
func TestWholeOptionsBackToLive_DropTheDraft_Integration(t *testing.T) {
	t.Run("revert the drafted row and index in one save; publish ships no Lost", func(t *testing.T) {
		s := s78Setup(t)
		stage := s.stageOf(t, s.bctx)
		s.draftLostValue(t, stage.ID)

		s.edit(t, EditFieldInput{FieldID: stage.ID, Options: map[string]any{"values": []any{"Proposal", "Closed Won", "Locked"}, "indexed": true}})
		if n := s.draftCount(t, s.tenant); n != 0 {
			t.Fatalf("pending after reverting the drafted row = %d, want 0", n)
		}
		s.assertLiveStage(t, stage.ID, false)

		// something to publish, so the publish itself is judged
		live, _ := s.meta.ListFields(s.bctx, s.tenant, s78EntityID(t, s))
		label := "Tên"
		for _, f := range live {
			if f.Name == "name" {
				s.edit(t, EditFieldInput{FieldID: f.ID, Label: &label})
			}
		}
		if _, err := s.publishWithCurrentHash(s.bctx, s.tenant, false); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		s.assertLiveStage(t, stage.ID, false)
	})
	t.Run("a stale inspector ticks index", func(t *testing.T) {
		s := s78Setup(t)
		stage := s.stageOf(t, s.bctx)
		opened := stage.Options // the inspector opened before the draft
		s.draftLostValue(t, stage.ID)

		s.edit(t, EditFieldInput{FieldID: stage.ID, Options: withIndexedOn(t, opened)})
		if n := s.draftCount(t, s.tenant); n != 0 {
			t.Fatalf("pending after the stale inspector's save = %d, want 0 (as on main)", n)
		}
		s.assertLiveStage(t, stage.ID, false)
	})
}
