package services

// schema_cutoff_test.go — specs/063-draft-publish S7-8 (T117, T118 gate half, T120, T121 class half):
// the cutoff cache, the customer-vs-builder schema view, the field draft layer and the field adapter's
// document form. No DB.

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// fakeVersions answers HasNumbered + HeadCutoff and counts the cutoff reads (SC-008).
type fakeVersions struct {
	delivered bool
	cutoff    *time.Time
	reads     int32
}

func (f *fakeVersions) HasNumbered(context.Context, string) (bool, error) { return f.delivered, nil }
func (f *fakeVersions) HeadCutoff(context.Context, string) (*time.Time, error) {
	atomic.AddInt32(&f.reads, 1)
	return f.cutoff, nil
}

var s78Cutoff = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestDraftGate_HeadCutoff_AtMostOneReadPerFiveSeconds(t *testing.T) {
	at := s78Cutoff.Add(300 * time.Millisecond) // the version row has milliseconds; the columns we compare with do not
	v := &fakeVersions{delivered: true, cutoff: &at}
	g := NewDraftGate(v)
	now := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	g.clock = func() time.Time { return now }

	for i := 0; i < 50; i++ {
		got, err := g.HeadCutoff(context.Background(), "t1")
		if err != nil || got == nil || !got.Equal(s78Cutoff) {
			t.Fatalf("HeadCutoff = %v, %v; want %v (cut to the second)", got, err, s78Cutoff)
		}
	}
	if v.reads != 1 {
		t.Fatalf("50 reads inside 5s hit the DB %d times, want 1 (SC-008)", v.reads)
	}
	now = now.Add(6 * time.Second)
	_, _ = g.HeadCutoff(context.Background(), "t1")
	if v.reads != 2 {
		t.Fatalf("after the TTL the DB was read %d times in total, want 2", v.reads)
	}
	// another tenant has its own entry
	_, _ = g.HeadCutoff(context.Background(), "t2")
	if v.reads != 3 {
		t.Fatalf("a second tenant did not get its own read: %d", v.reads)
	}
	// a publish on this replica moves the cutoff without a read
	g.RefreshCutoff("t1", s78Cutoff.Add(time.Hour))
	if got, _ := g.HeadCutoff(context.Background(), "t1"); got == nil || !got.Equal(s78Cutoff.Add(time.Hour)) || v.reads != 3 {
		t.Fatalf("RefreshCutoff: got %v after %d reads, want the new instant with no read", got, v.reads)
	}
}

func TestDraftGate_SchemaCutoff_WhoIsHidden(t *testing.T) {
	cutoff := s78Cutoff
	tests := []struct {
		name      string
		delivered bool
		cutoff    *time.Time
		ctx       context.Context
		wantApply bool
	}{
		{"customer (no overlay) of a delivered tenant", true, &cutoff, legacyAdminCtx(), true},
		{"request with no caller at all", true, &cutoff, context.Background(), true},
		{"builder reading the draft overlay sees everything", true, &cutoff, builderCtx(), false},
		{"builder without ReadDraft is a live reader", true, &cutoff, metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "b", TenantID: dwTenant, IsBuilder: true}), true},
		{"tenant never delivered: nothing hidden", false, nil, legacyAdminCtx(), false},
		{"delivered but the version has no cutoff: nothing hidden", true, nil, legacyAdminCtx(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewDraftGate(&fakeVersions{delivered: tt.delivered, cutoff: tt.cutoff})
			at, apply, err := g.SchemaCutoff(tt.ctx, dwTenant)
			if err != nil || apply != tt.wantApply {
				t.Fatalf("SchemaCutoff = %v, %v, %v; want apply=%v", at, apply, err, tt.wantApply)
			}
			if apply && !at.Equal(s78Cutoff) {
				t.Errorf("cutoff = %v, want %v", at, s78Cutoff)
			}
		})
	}
}

func TestHideNewerThan(t *testing.T) {
	old, fresh := s78Cutoff.Add(-time.Hour), s78Cutoff.Add(time.Hour)
	fs := HideFieldsNewerThan([]metadata.FieldDefinition{{Name: "a", CreatedAt: old}, {Name: "edge", CreatedAt: s78Cutoff}, {Name: "b", CreatedAt: fresh}}, s78Cutoff)
	if len(fs) != 2 || fs[0].Name != "a" || fs[1].Name != "edge" {
		t.Errorf("fields = %+v, want a and edge (created_at == cutoff is the customer's)", fs)
	}
	es := HideEntitiesNewerThan([]metadata.EntityDefinition{{Name: "old", CreatedAt: old}, {Name: "new", CreatedAt: fresh}}, s78Cutoff)
	if len(es) != 1 || es[0].Name != "old" {
		t.Errorf("entities = %+v", es)
	}
	rs := HideRelationshipsNewerThan([]metadata.RelationshipDefinition{{Name: "old", CreatedAt: old}, {Name: "new", CreatedAt: fresh}}, s78Cutoff)
	if len(rs) != 1 || rs[0].Name != "old" {
		t.Errorf("relationships = %+v", rs)
	}
}

func TestGetLayout_HidesFieldsBornAfterCutoff(t *testing.T) {
	old, fresh := s78Cutoff.Add(-24*time.Hour), s78Cutoff.Add(time.Hour)
	fields := []metadata.FieldDefinition{
		{Name: "name", Label: "Name", FieldType: "text", CreatedAt: old},
		{Name: "fresh", Label: "Fresh", FieldType: "text", CreatedAt: fresh},
	}
	stored := metadata.LayoutDefinition{ID: "L1", ViewType: LayoutViewEdit, LayoutJSON: `{"version":1,"sections":[{"title":"A","columns":2,"fields":["name"]}]}`,
		CreatedAt: old, UpdatedAt: old.Add(time.Hour)} // saved before "fresh" was born: it would land in "Mới thêm"

	views := func(ctx context.Context, delivered bool, withStored bool) (def, edit string) {
		repo := &sortableFakeRepo{}
		if withStored {
			repo.stored = []metadata.LayoutDefinition{stored}
		}
		svc := NewLayoutService(&sortableFakeMeta{fields: fields}, repo, nil).WithSchemaCutoff(NewDraftGate(&fakeVersions{delivered: delivered, cutoff: &s78Cutoff}))
		res, err := svc.GetLayout(ctx, dwTenant, "deals", "edit")
		if err != nil {
			t.Fatalf("GetLayout: %v", err)
		}
		b, _ := json.Marshal(res.Views["edit"].LayoutData)
		return "", string(b)
	}
	tests := []struct {
		name       string
		ctx        context.Context
		delivered  bool
		withStored bool
		wantFresh  bool
	}{
		{"customer, generated default", legacyAdminCtx(), true, false, false},
		{"customer, saved layout (the Mới thêm section)", legacyAdminCtx(), true, true, false},
		{"builder overlay, generated default", builderCtx(), true, false, true},
		{"builder overlay, saved layout", builderCtx(), true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := views(tt.ctx, tt.delivered, tt.withStored)
			if has := strings.Contains(got, `"fresh"`); has != tt.wantFresh {
				t.Errorf("form has the new field = %v, want %v: %s", has, tt.wantFresh, got)
			}
			if !strings.Contains(got, `"name"`) {
				t.Errorf("the old field vanished: %s", got)
			}
		})
	}
	// control: with no cutoff wired the service behaves as before (the field is in "Mới thêm")
	svc := NewLayoutService(&sortableFakeMeta{fields: fields}, &sortableFakeRepo{stored: []metadata.LayoutDefinition{stored}}, nil)
	res, _ := svc.GetLayout(legacyAdminCtx(), dwTenant, "deals", "edit")
	if b, _ := json.Marshal(res.Views["edit"].LayoutData); !strings.Contains(string(b), `"fresh"`) {
		t.Errorf("control (no cutoff wired) lost the Mới thêm field: %s", b)
	}
}

func TestClassOf_FieldAndEntityAreNewHidden(t *testing.T) {
	if c := metaversion.ClassOf("field"); c.Kind != metaversion.Draft || !c.NewHidden {
		t.Errorf("ClassOf(field) = %+v, want {draft, NewHidden}", c)
	}
	if c := metaversion.ClassOf("entity"); !c.NewHidden || c.Kind != metaversion.Draft {
		t.Errorf("ClassOf(entity) = %+v, want {draft, NewHidden}: a new table hides, and deleting one is a draft (S7-9)", c)
	}
	if c := metaversion.ClassOf("workflow"); c.NewHidden {
		t.Errorf("ClassOf(workflow).NewHidden = true; only field and entity hide when new")
	}
	// GET /admin/publish/state builds draftableTypes from this list (specs/067-ai-context FR-062 relies on it).
	found := false
	for _, typ := range metaversion.TypesOfKind(metaversion.Draft) {
		found = found || typ == "field"
	}
	if !found {
		t.Error(`"field" missing from the draftable types`)
	}
	// Publish drops the MetaCache entry of the entity a field draft belongs to.
	if got := NewFieldDocAdapter().Entities(json.RawMessage(`{"entityId":"e-9","label":"x"}`)); len(got) != 1 || got[0] != "e-9" {
		t.Errorf("FieldDocAdapter.Entities = %v, want [e-9]", got)
	}
	if got := NewFieldDocAdapter().Entities(nil); got != nil {
		t.Errorf("Entities(nil) = %v, want nil", got)
	}
}

// ---------------------------------------------------------------------------------------------
// DraftFieldRepo (in-memory live repo + the shared fake draft store)
// ---------------------------------------------------------------------------------------------

type memFields struct {
	rows    []metadata.FieldDefinition
	updates int
}

func (m *memFields) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	var out []metadata.FieldDefinition
	for _, r := range m.rows {
		if r.EntityID == entityID {
			out = append(out, r)
		}
	}
	return out, nil
}
func (m *memFields) GetFieldByID(_ context.Context, _, id string) (*metadata.FieldDefinition, error) {
	for i := range m.rows {
		if m.rows[i].ID == id {
			r := m.rows[i]
			return &r, nil
		}
	}
	return nil, metadata.ErrFieldNotFound
}
func (m *memFields) UpdateFieldMutable(_ context.Context, _ string, f *metadata.FieldDefinition) error {
	for i := range m.rows {
		if m.rows[i].ID == f.ID {
			m.rows[i].Label, m.rows[i].IsRequired, m.rows[i].DefaultVal = f.Label, f.IsRequired, f.DefaultVal
			m.rows[i].Options, m.rows[i].FieldOrder, m.rows[i].Description = f.Options, f.FieldOrder, f.Description
			m.updates++
			return nil
		}
	}
	return metadata.ErrFieldNotFound
}
func (m *memFields) GetEntityNameByID(context.Context, string, string) (string, error) {
	return "deals", nil
}

type dfEnv struct {
	live   *memFields
	drafts *fakeDraftStore
	repo   *DraftFieldRepo
}

func newDFEnv(delivered bool) *dfEnv {
	cut := s78Cutoff
	live := &memFields{rows: []metadata.FieldDefinition{
		{ID: "f1", EntityID: "e1", Name: "name", Label: "Name", FieldType: "text", FieldOrder: 10, CreatedAt: s78Cutoff.Add(-time.Hour)},
		{ID: "f2", EntityID: "e1", Name: "stage", Label: "Stage", FieldType: "text", FieldOrder: 20, CreatedAt: s78Cutoff.Add(-time.Hour), Options: sp(`{"indexed":false,"hint":"x"}`)},
		{ID: "f3", EntityID: "e1", Name: "fresh", Label: "Fresh", FieldType: "text", FieldOrder: 30, CreatedAt: s78Cutoff.Add(time.Hour)},
	}}
	drafts := newFakeDraftStore()
	return &dfEnv{live: live, drafts: drafts, repo: NewDraftFieldRepo(live, drafts, NewDraftGate(&fakeVersions{delivered: delivered, cutoff: &cut}))}
}

func TestDraftFieldRepo_NotDeliveredWritesLive(t *testing.T) {
	e := newDFEnv(false)
	f, _ := e.live.GetFieldByID(context.Background(), "", "f1")
	f.Label = "Renamed"
	draftOnly, err := e.repo.SaveEdit(builderCtx(), dwTenant, f)
	if err != nil || draftOnly || e.live.rows[0].Label != "Renamed" || len(e.drafts.rows) != 0 {
		t.Fatalf("not delivered: draftOnly=%v err=%v live=%q drafts=%d, want a live write", draftOnly, err, e.live.rows[0].Label, len(e.drafts.rows))
	}
}

func TestDraftFieldRepo_BuilderEditIsADraft_IndexedAppliesNow(t *testing.T) {
	e := newDFEnv(true)
	f, _ := e.live.GetFieldByID(context.Background(), "", "f2")
	f.Label = "Giai đoạn"
	f.IsRequired = true
	f.Options = sp(`{"indexed":true,"hint":"x"}`)
	draftOnly, err := e.repo.SaveEdit(builderCtx(), dwTenant, f)
	if err != nil {
		t.Fatal(err)
	}
	if draftOnly {
		t.Error("draftOnly = true although options.indexed went live")
	}
	live := e.live.rows[1]
	if live.Label != "Stage" || live.IsRequired || live.Options == nil || !strings.Contains(*live.Options, `"indexed":true`) || !strings.Contains(*live.Options, `"hint"`) {
		t.Fatalf("live = %+v opts=%v, want label/required untouched and indexed:true with the other options kept", live, live.Options)
	}
	d, _ := e.drafts.GetByKey(context.Background(), dwTenant, DocTypeField, "f2")
	if d == nil || !strings.Contains(string(d.DraftJSON), "Giai đoạn") || strings.Contains(string(d.DraftJSON), "indexed") {
		t.Fatalf("draft = %v, want the new label and no indexed key", d)
	}

	// the builder reads draft + live indexed; the customer reads live
	b, _ := e.repo.GetFieldByID(builderCtx(), dwTenant, "f2")
	if b.Label != "Giai đoạn" || !b.IsRequired || b.DraftState != draftStateEdited || !b.PendingRequired || !strings.Contains(*b.Options, `"indexed":true`) {
		t.Errorf("builder read = %+v", b)
	}
	c, _ := e.repo.GetFieldByID(legacyAdminCtx(), dwTenant, "f2")
	if c.Label != "Stage" || c.DraftState != "" {
		t.Errorf("customer read = %+v, want the live row", c)
	}
	if got, _ := e.repo.ListFields(legacyAdminCtx(), dwTenant, "e1"); got[1].Label != "Stage" || len(e.drafts.rows) != 1 {
		t.Errorf("customer list = %+v", got)
	}

	// editing it back by hand drops the draft: nothing pending, nothing to publish
	back := *b
	back.Label, back.IsRequired = "Stage", false
	if _, err := e.repo.SaveEdit(builderCtx(), dwTenant, &back); err != nil {
		t.Fatal(err)
	}
	if len(e.drafts.rows) != 0 {
		t.Errorf("a field edited back to its live state still has %d drafts", len(e.drafts.rows))
	}
}

func TestDraftFieldRepo_OverlayMarksNewFields(t *testing.T) {
	e := newDFEnv(true)
	got, err := e.repo.ListFields(builderCtx(), dwTenant, "e1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"f1": "", "f2": "", "f3": draftStateNew}
	for _, f := range got {
		if f.DraftState != want[f.ID] {
			t.Errorf("field %s state = %q, want %q", f.ID, f.DraftState, want[f.ID])
		}
	}
}

func TestDraftFieldRepo_ReorderIsDraftsOnly(t *testing.T) {
	e := newDFEnv(true)
	if err := e.repo.SaveReorder(builderCtx(), dwTenant, "e1", []string{"f3", "f2", "f1"}); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{10, 20, 30} {
		if e.live.rows[i].FieldOrder != want {
			t.Errorf("live field_order of %s = %d, want %d untouched", e.live.rows[i].ID, e.live.rows[i].FieldOrder, want)
		}
	}
	// f2 keeps position 20; f3 -> 10 and f1 -> 30 are drafts
	if len(e.drafts.rows) != 2 {
		t.Fatalf("reorder made %d drafts, want 2 (f2 did not move)", len(e.drafts.rows))
	}
	b, _ := e.repo.ListFields(builderCtx(), dwTenant, "e1")
	if b[0].ID != "f3" || b[2].ID != "f1" {
		t.Errorf("builder order = %s,%s,%s, want f3,f2,f1", b[0].ID, b[1].ID, b[2].ID)
	}
	// a reorder merges into the pending edit of the same field
	f, _ := e.repo.GetFieldByID(builderCtx(), dwTenant, "f1")
	f.Label = "Tên"
	if _, err := e.repo.SaveEdit(builderCtx(), dwTenant, f); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.SaveReorder(builderCtx(), dwTenant, "e1", []string{"f1", "f2", "f3"}); err != nil {
		t.Fatal(err)
	}
	g, _ := e.repo.GetFieldByID(builderCtx(), dwTenant, "f1")
	if g.Label != "Tên" || g.FieldOrder != 10 {
		t.Errorf("after putting f1 back first: %+v, want the edit kept and order 10", g)
	}
	// putting everything back leaves only the label edit pending
	if len(e.drafts.rows) != 1 {
		t.Errorf("%d drafts, want 1 (the label)", len(e.drafts.rows))
	}
}

func TestFieldDoc_IndexedNeverInTheDocument(t *testing.T) {
	a, b := `{"indexed":true,"values":["x"]}`, `{"values":["x"]}`
	da, _ := fieldDocOf(&metadata.FieldDefinition{ID: "f", Label: "L", Options: &a})
	db, _ := fieldDocOf(&metadata.FieldDefinition{ID: "f", Label: "L", Options: &b})
	ja, _ := da.marshal()
	jb, _ := db.marshal()
	ha, _ := metaversion.DocHash(ja)
	hb, _ := metaversion.DocHash(jb)
	if ha != hb {
		t.Errorf("a field with and without options.indexed hashes differently: the toggle would read as an unpublished change")
	}
	// {} and absent options are the same document
	empty := `{}`
	de, _ := fieldDocOf(&metadata.FieldDefinition{ID: "f", Label: "L", Options: &empty})
	dn, _ := fieldDocOf(&metadata.FieldDefinition{ID: "f", Label: "L"})
	if string(de.Options) != string(dn.Options) {
		t.Errorf("empty options %s != no options %s", de.Options, dn.Options)
	}
	// applying a draft keeps the LIVE indexed value
	merged, err := applyFieldDoc(metadata.FieldDefinition{ID: "f", Options: &a}, fieldDoc{Label: "N", Options: json.RawMessage(`{"values":["y"]}`)})
	if err != nil || merged.Label != "N" || !strings.Contains(*merged.Options, `"indexed":true`) || !strings.Contains(*merged.Options, `"y"`) {
		t.Errorf("applyFieldDoc = %+v %v", merged, err)
	}
}

func TestEditField_TypeChangeCarriesAHint(t *testing.T) {
	_, _, err := ValidateEditFieldChanges(metadata.FieldDefinition{ID: "f", FieldType: "text"}, EditFieldInput{FieldID: "f", FieldType: "number"}, nil, nil)
	wantCode(t, err, "FIELD_TYPE_IMMUTABLE")
	ve := err.(*ValidationError)
	if h, _ := ve.Details["hint"].(string); h == "" {
		t.Errorf("details = %v, want a hint naming the way out (FR-061)", ve.Details)
	}
}

func TestFieldService_AddRequired_DeferredOnlyWhenDeliveredAndBuilder(t *testing.T) {
	for _, tt := range []struct {
		name      string
		ctx       context.Context
		delivered bool
		wantReq   bool
		wantDraft int
	}{
		{"not delivered: required stays live", builderCtx(), false, true, 0},
		{"delivered + builder: optional live + a draft", builderCtx(), true, false, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			meta := &fakeFieldMeta{entity: entityFixture()}
			live := &memFields{}
			cut := s78Cutoff
			drafts := newFakeDraftStore()
			repo := NewDraftFieldRepo(live, drafts, NewDraftGate(&fakeVersions{delivered: tt.delivered, cutoff: &cut}))
			svc := NewFieldService(&fakeFieldSchema{}, &addToMem{fakeFieldMeta: meta, mem: live}).WithDraftFields(repo)
			in := ordinaryField()
			in.Field.IsRequired = true
			f, err := svc.AddField(tt.ctx, dwTenant, in)
			if err != nil {
				t.Fatal(err)
			}
			if f.IsRequired != tt.wantReq || len(drafts.rows) != tt.wantDraft {
				t.Errorf("IsRequired=%v drafts=%d, want %v / %d", f.IsRequired, len(drafts.rows), tt.wantReq, tt.wantDraft)
			}
		})
	}
}

// addToMem mirrors a created field into the in-memory live repo the draft layer reads.
type addToMem struct {
	*fakeFieldMeta
	mem *memFields
}

func (a *addToMem) CreateField(ctx context.Context, fd *metadata.FieldDefinition) error {
	if err := a.fakeFieldMeta.CreateField(ctx, fd); err != nil {
		return err
	}
	a.mem.rows = append(a.mem.rows, *fd)
	return nil
}
