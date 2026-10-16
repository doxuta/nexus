package services

// draft_repo_workflow_test.go — specs/063-draft-publish T024. The wrapped workflow repo against an
// in-memory live repo and an in-memory draft store: no DB.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/workflow"
)

// fakeDraftStore is an in-memory draftStore keyed by (tenant, type, key). base_hash is not modelled
// (the adapter + hash are covered by the integration tests).
type fakeDraftStore struct {
	rows  map[string]metaversion.DraftRow
	saves int
	now   time.Time
}

func newFakeDraftStore() *fakeDraftStore {
	return &fakeDraftStore{rows: map[string]metaversion.DraftRow{}, now: time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)}
}

func fdsKey(tenant, typ, key string) string { return tenant + "|" + typ + "|" + key }

func (f *fakeDraftStore) Upsert(_ context.Context, tenantID string, in metaversion.UpsertInput) error {
	f.saves++
	f.now = f.now.Add(time.Second)
	k := fdsKey(tenantID, in.DocType, in.DocKey)
	row, had := f.rows[k]
	if !had {
		row = metaversion.DraftRow{ID: "d-" + in.DocKey, TenantID: tenantID, DocType: in.DocType, DocKey: in.DocKey, CreatedBy: in.UserID, CreatedAt: f.now}
	}
	row.Op, row.DraftJSON, row.Label, row.UpdatedAt = in.Op, []byte(in.Doc), in.Label, f.now
	by := in.UserID
	row.ModifiedBy = &by
	if in.EntityName != "" {
		e := in.EntityName
		row.EntityName = &e
	}
	f.rows[k] = row
	return nil
}

func (f *fakeDraftStore) GetByKey(_ context.Context, tenantID, docType, docKey string) (*metaversion.DraftRow, error) {
	if r, ok := f.rows[fdsKey(tenantID, docType, docKey)]; ok {
		return &r, nil
	}
	return nil, nil
}

func (f *fakeDraftStore) ListByType(_ context.Context, tenantID, docType string) ([]metaversion.DraftRow, error) {
	var out []metaversion.DraftRow
	for _, r := range f.rows {
		if r.TenantID == tenantID && r.DocType == docType {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DocKey < out[j].DocKey })
	return out, nil
}

func (f *fakeDraftStore) DeleteByKey(_ context.Context, tenantID, docType, docKey string) (bool, error) {
	k := fdsKey(tenantID, docType, docKey)
	_, ok := f.rows[k]
	delete(f.rows, k)
	return ok, nil
}

type dwEnv struct {
	live   *fakeWFRepo
	drafts *fakeDraftStore
	deliv  *fakeDelivered
	repo   *DraftWorkflowRepo
}

func newDWEnv(delivered bool) *dwEnv {
	live := newFakeWFRepo()
	drafts := newFakeDraftStore()
	deliv := &fakeDelivered{delivered: delivered}
	return &dwEnv{live: live, drafts: drafts, deliv: deliv, repo: NewDraftWorkflowRepo(live, drafts, NewDraftGate(deliv))}
}

const dwTenant = "t1"

func builderCtx() context.Context {
	return metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "builder", TenantID: dwTenant, HomeTenantID: dwTenant, IsBuilder: true, ReadDraft: true})
}

func legacyAdminCtx() context.Context {
	return metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "legacy", TenantID: dwTenant, HomeTenantID: dwTenant})
}

func dwCreateInput(name string) workflow.CreateInput {
	return workflow.CreateInput{TenantID: dwTenant, Name: name, EntityName: "crm_x", Trigger: workflow.TriggerAfterCreate,
		Actions: []byte(`[{"type":"in_app_notification"}]`), IsActive: true, CreatedBy: "builder"}
}

func dwUpdateInput(name string, active bool) workflow.UpdateInput {
	return workflow.UpdateInput{Name: name, EntityName: "crm_x", Trigger: workflow.TriggerAfterCreate,
		Actions: []byte(`[{"type":"in_app_notification"}]`), IsActive: active, UpdatedBy: "builder"}
}

// seedLive puts a live rule (id "wf-1") in the fake live repo.
func (e *dwEnv) seedLive(t *testing.T, name string) string {
	t.Helper()
	id, err := e.live.Create(context.Background(), dwCreateInput(name))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDraftWorkflowRepo_NotDeliveredGoesStraightLive(t *testing.T) {
	e := newDWEnv(false)
	id, err := e.repo.Create(builderCtx(), dwCreateInput("R1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.live.Get(context.Background(), dwTenant, id); err != nil {
		t.Fatalf("not delivered: the rule must be live at once: %v", err)
	}
	if e.drafts.saves != 0 {
		t.Fatalf("a draft was written for a tenant that is not delivered")
	}
	if err := e.repo.Update(builderCtx(), dwTenant, id, dwUpdateInput("R1b", true)); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.live.Get(context.Background(), dwTenant, id); got.Name != "R1b" {
		t.Fatalf("live name = %q", got.Name)
	}
	if err := e.repo.Delete(builderCtx(), dwTenant, id); err != nil {
		t.Fatal(err)
	}
}

func TestDraftWorkflowRepo_DeliveredBuilderWritesDrafts(t *testing.T) {
	e := newDWEnv(true)
	liveID := e.seedLive(t, "live rule")
	ctx := builderCtx()

	// Update: the draft gets the new content, the live row does not move.
	if err := e.repo.Update(ctx, dwTenant, liveID, dwUpdateInput("edited", false)); err != nil {
		t.Fatal(err)
	}
	if live, _ := e.live.Get(context.Background(), dwTenant, liveID); live.Name != "live rule" || !live.IsActive {
		t.Fatalf("live row changed by a draft save: %+v", live)
	}
	got, err := e.repo.Get(ctx, dwTenant, liveID)
	if err != nil || got.Name != "edited" || got.IsActive || got.DraftState != "edited" {
		t.Fatalf("overlay Get = %+v, %v; want the draft, state edited", got, err)
	}

	// Create: a fresh id, nothing live, readable as "new".
	newID, err := e.repo.Create(ctx, dwCreateInput("brand new"))
	if err != nil || newID == "" {
		t.Fatalf("Create: %q %v", newID, err)
	}
	if _, err := e.live.Get(context.Background(), dwTenant, newID); !errors.Is(err, workflow.ErrWorkflowNotFound) {
		t.Fatalf("a new draft rule exists live: %v", err)
	}
	got, err = e.repo.Get(ctx, dwTenant, newID)
	if err != nil || got.DraftState != "new" || got.Name != "brand new" || got.CreatedAt.IsZero() {
		t.Fatalf("new draft Get = %+v, %v", got, err)
	}

	// Delete a live rule: becomes a delete draft shown as "deleted", still listed.
	otherLive := e.seedLive(t, "to delete")
	if err := e.repo.Delete(ctx, dwTenant, otherLive); err != nil {
		t.Fatal(err)
	}
	if _, err := e.live.Get(context.Background(), dwTenant, otherLive); err != nil {
		t.Fatalf("live rule deleted by a draft delete: %v", err)
	}
	got, _ = e.repo.Get(ctx, dwTenant, otherLive)
	if got == nil || got.DraftState != "deleted" {
		t.Fatalf("deleted rule = %+v", got)
	}

	// Saving a rule marked "deleted" turns it back into an edit (FR-008).
	if err := e.repo.Update(ctx, dwTenant, otherLive, dwUpdateInput("kept", true)); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.repo.Get(ctx, dwTenant, otherLive); got.DraftState != "edited" || got.Name != "kept" {
		t.Fatalf("after re-save: %+v", got)
	}

	// Delete a draft-only rule: the draft row disappears, no delete draft is left behind.
	if err := e.repo.Delete(ctx, dwTenant, newID); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.drafts.GetByKey(ctx, dwTenant, DocTypeWorkflow, newID); d != nil {
		t.Fatalf("draft of a never-live rule survived its deletion: %+v", d)
	}
	if _, err := e.repo.Get(ctx, dwTenant, newID); !errors.Is(err, workflow.ErrWorkflowNotFound) {
		t.Fatalf("Get after deleting a draft-only rule: %v", err)
	}

	// Unknown id (neither live nor draft).
	if err := e.repo.Update(ctx, dwTenant, "ghost", dwUpdateInput("x", true)); !errors.Is(err, workflow.ErrWorkflowNotFound) {
		t.Fatalf("Update of a ghost: %v", err)
	}
	if err := e.repo.Delete(ctx, dwTenant, "ghost"); !errors.Is(err, workflow.ErrWorkflowNotFound) {
		t.Fatalf("Delete of a ghost: %v", err)
	}
}

func TestDraftWorkflowRepo_ListOverlayAndPlainReaders(t *testing.T) {
	e := newDWEnv(true)
	editedID := e.seedLive(t, "a")
	e.seedLive(t, "b")
	deletedID := e.seedLive(t, "c")
	ctx := builderCtx()
	_ = e.repo.Update(ctx, dwTenant, editedID, dwUpdateInput("a-edited", true))
	_ = e.repo.Delete(ctx, dwTenant, deletedID)
	newID, _ := e.repo.Create(ctx, dwCreateInput("d-new"))

	rows, err := e.repo.List(ctx, dwTenant)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{}
	for _, r := range rows {
		state[r.Name] = r.DraftState
	}
	want := map[string]string{"a-edited": "edited", "b": "live", "c": "deleted", "d-new": "new"}
	if len(state) != len(want) {
		t.Fatalf("List = %v, want %v", state, want)
	}
	for k, v := range want {
		if state[k] != v {
			t.Fatalf("List state of %q = %q, want %q (all: %v)", k, state[k], v, state)
		}
	}

	// Everyone else — a legacy admin, no caller, and the builder under WithLiveRead — reads live.
	for name, c := range map[string]context.Context{
		"legacy admin": legacyAdminCtx(), "no caller": context.Background(), "live read": metaversion.WithLiveRead(builderCtx()),
	} {
		live, err := e.repo.List(c, dwTenant)
		if err != nil || len(live) != 3 {
			t.Fatalf("%s: List = %d rows, %v; want the 3 live rules", name, len(live), err)
		}
		for _, r := range live {
			if r.DraftState != "" || r.Name == "a-edited" {
				t.Fatalf("%s sees the overlay: %+v", name, r)
			}
		}
		if _, err := e.repo.Get(c, dwTenant, newID); !errors.Is(err, workflow.ErrWorkflowNotFound) {
			t.Fatalf("%s: Get of a draft-only rule: %v", name, err)
		}
	}
}

// FR-006: the cap on active before rules counts the state AFTER publish.
func TestDraftWorkflowRepo_CountActiveOnOverlay(t *testing.T) {
	e := newDWEnv(true)
	mk := func(name string) workflow.CreateInput {
		in := dwCreateInput(name)
		in.Trigger = workflow.TriggerBeforeSave
		in.Actions = []byte(`[{"type":"veto"}]`)
		return in
	}
	idA, _ := e.live.Create(context.Background(), mk("A"))
	idB, _ := e.live.Create(context.Background(), mk("B"))
	ctx := builderCtx()
	count := func(c context.Context, exclude string) int {
		n, err := e.repo.CountActive(c, dwTenant, "crm_x", workflow.TriggerBeforeSave, exclude)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(ctx, ""); n != 2 {
		t.Fatalf("baseline count = %d, want 2", n)
	}
	// Delete A (draft), switch B off (draft), add C (draft, active): after publish only C is active.
	_ = e.repo.Delete(ctx, dwTenant, idA)
	upd := workflow.UpdateInput{Name: "B", EntityName: "crm_x", Trigger: workflow.TriggerBeforeSave, Actions: []byte(`[{"type":"veto"}]`), IsActive: false}
	_ = e.repo.Update(ctx, dwTenant, idB, upd)
	cID, _ := e.repo.Create(ctx, mk("C"))
	if n := count(ctx, ""); n != 1 {
		t.Fatalf("overlay count = %d, want 1 (only C survives)", n)
	}
	if n := count(ctx, cID); n != 0 {
		t.Fatalf("overlay count excluding C = %d, want 0", n)
	}
	if n := count(legacyAdminCtx(), ""); n != 2 {
		t.Fatalf("a non-builder must count live rules: %d", n)
	}
}

func TestDraftWorkflowRepo_LegacyAdminWritesLive_NoCallerFailsClosed(t *testing.T) {
	e := newDWEnv(true)
	id := e.seedLive(t, "r")
	if err := e.repo.Update(legacyAdminCtx(), dwTenant, id, dwUpdateInput("by legacy admin", true)); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.live.Get(context.Background(), dwTenant, id); got.Name != "by legacy admin" {
		t.Fatalf("legacy admin write did not land live (as today): %q", got.Name)
	}
	if e.drafts.saves != 0 {
		t.Fatal("a draft was written for a non-builder")
	}
	// A route that never passed DraftCaller must not write the customer's running app.
	if err := e.repo.Update(context.Background(), dwTenant, id, dwUpdateInput("no caller", true)); !errors.Is(err, metaversion.ErrNoCaller) {
		t.Fatalf("no caller on a delivered tenant: %v, want ErrNoCaller", err)
	}
	if _, err := e.repo.Create(context.Background(), dwCreateInput("x")); !errors.Is(err, metaversion.ErrNoCaller) {
		t.Fatalf("Create without a caller: %v", err)
	}
	if err := e.repo.Delete(context.Background(), dwTenant, id); !errors.Is(err, metaversion.ErrNoCaller) {
		t.Fatalf("Delete without a caller: %v", err)
	}
	// Another tenant than the token's.
	if err := e.repo.Update(builderCtx(), "t-other", id, dwUpdateInput("x", true)); !errors.Is(err, metaversion.ErrCrossTenantDraftWrite) {
		t.Fatalf("cross-tenant write: %v", err)
	}
	if got, _ := e.live.Get(context.Background(), dwTenant, id); got.Name != "by legacy admin" {
		t.Fatalf("a refused write changed the live rule: %q", got.Name)
	}
}

func TestWorkflowDocCanonical_RoundTrip(t *testing.T) {
	cond := []byte(`{"match":"and","rules":[{"field":"a","op":"eq","value":1}]}`)
	in := workflow.UpdateInput{Name: "n", EntityName: "e", Trigger: workflow.TriggerAfterCreate, Condition: cond, Actions: []byte(`[{"type":"x"}]`), IsActive: true, IntervalSeconds: 0}
	raw, err := workflowDocOfUpdate(in).marshal()
	if err != nil {
		t.Fatal(err)
	}
	var d workflowDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	back := d.updateInput("u")
	if string(back.Condition) != string(cond) || back.Name != "n" || !back.IsActive {
		t.Fatalf("round trip lost data: %+v", back)
	}
	// No condition ⇒ JSON null in the doc ⇒ SQL NULL (nil) again.
	raw, _ = workflowDocOfUpdate(workflow.UpdateInput{Name: "n", EntityName: "e", Trigger: workflow.TriggerAfterCreate, Actions: []byte(`[]`)}).marshal()
	_ = json.Unmarshal(raw, &d)
	if back := d.updateInput(""); back.Condition != nil {
		t.Fatalf("a null condition came back as %q, want nil", back.Condition)
	}
	// Same document, different key order / whitespace ⇒ same hash (a draft saved identical to live is no change).
	other := []byte("{ \"isActive\": true, \"intervalSeconds\": 0, \"actions\": [{\"type\":\"x\"}], \"condition\": null,\n \"trigger\": \"after_create\", \"entityName\": \"e\", \"name\": \"n\" }")
	h1, _ := metaversion.DocHash(other)
	d2 := workflowDoc{Name: "n", EntityName: "e", Trigger: "after_create", Condition: json.RawMessage("null"), Actions: json.RawMessage(`[{"type":"x"}]`), IsActive: true}
	raw2, _ := d2.marshal()
	h2, _ := metaversion.DocHash(raw2)
	if h1 != h2 {
		t.Fatal("canonical hash is not stable across key order / whitespace")
	}
}

// SetActive (pause / resume, main's PATCH /admin/workflows/:id/active): not delivered it flips the
// live row; delivered + builder it is a DRAFT edit that carries the rest of the rule verbatim and
// leaves the live row alone; a rule marked "to delete" cannot be switched (a resume must not un-delete).
func TestDraftWorkflowRepo_SetActive(t *testing.T) {
	t.Run("not delivered flips the live row", func(t *testing.T) {
		e := newDWEnv(false)
		id := e.seedLive(t, "R")
		if err := e.repo.SetActive(builderCtx(), dwTenant, id, false, "builder"); err != nil {
			t.Fatal(err)
		}
		if got, _ := e.live.Get(context.Background(), dwTenant, id); got.IsActive || e.drafts.saves != 0 {
			t.Fatalf("live=%+v saves=%d, want live OFF and no draft", got, e.drafts.saves)
		}
	})
	t.Run("delivered builder pauses as a draft, live untouched", func(t *testing.T) {
		e := newDWEnv(true)
		id := e.seedLive(t, "R")
		if err := e.repo.SetActive(builderCtx(), dwTenant, id, false, "builder"); err != nil {
			t.Fatal(err)
		}
		if live, _ := e.live.Get(context.Background(), dwTenant, id); !live.IsActive {
			t.Fatal("the customer's live rule was switched by a draft pause")
		}
		got, err := e.repo.Get(builderCtx(), dwTenant, id)
		if err != nil || got.IsActive || got.DraftState != "edited" || got.Name != "R" || string(got.Actions) != `[{"type":"in_app_notification"}]` {
			t.Fatalf("overlay = %+v, %v; want OFF, edited, rest carried over", got, err)
		}
		// Resuming on top of the draft edits the DRAFT (still one draft row).
		if err := e.repo.SetActive(builderCtx(), dwTenant, id, true, "builder"); err != nil {
			t.Fatal(err)
		}
		if got, _ := e.repo.Get(builderCtx(), dwTenant, id); !got.IsActive || len(e.drafts.rows) != 1 {
			t.Fatalf("resume = %+v, drafts=%d", got, len(e.drafts.rows))
		}
	})
	t.Run("delivered legacy admin writes live", func(t *testing.T) {
		e := newDWEnv(true)
		id := e.seedLive(t, "R")
		if err := e.repo.SetActive(legacyAdminCtx(), dwTenant, id, false, "legacy"); err != nil {
			t.Fatal(err)
		}
		if got, _ := e.live.Get(context.Background(), dwTenant, id); got.IsActive || e.drafts.saves != 0 {
			t.Fatalf("live=%+v saves=%d", got, e.drafts.saves)
		}
	})
	t.Run("errors", func(t *testing.T) {
		e := newDWEnv(true)
		id := e.seedLive(t, "R")
		if err := e.repo.SetActive(builderCtx(), dwTenant, "nope", true, "b"); !errors.Is(err, workflow.ErrWorkflowNotFound) {
			t.Fatalf("unknown id: %v", err)
		}
		if err := e.repo.Delete(builderCtx(), dwTenant, id); err != nil {
			t.Fatal(err)
		}
		if err := e.repo.SetActive(builderCtx(), dwTenant, id, true, "b"); !errors.Is(err, workflow.ErrWorkflowNotFound) {
			t.Fatalf("a rule marked to delete must not be switchable: %v", err)
		}
	})
}
