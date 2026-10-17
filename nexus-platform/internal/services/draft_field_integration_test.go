//go:build integration

package services

// draft_field_integration_test.go — specs/063-draft-publish S7-8 (T119–T121, T120a) against a real
// MySQL: a delivered workspace, a builder adding a required field, editing and reordering fields, the
// customer's view staying put until Publish, and Publish releasing everything in one version.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

type s78Env struct {
	dpEnv
	tenant, builder string
	bctx, cctx      context.Context
	fields          *FieldService
	layouts         *LayoutService
}

// s78Setup delivers a tenant (baseline) with the field adapter in the registry. Fields created next are
// strictly newer than the cutoff: the baseline names the second of the newest definition and waits it out (#19).
func s78Setup(t *testing.T) *s78Env {
	t.Helper()
	e := dpSetupWith(t, NewWorkflowDocAdapter(), NewLayoutDocAdapter(), NewFieldDocAdapter(), NewEntityDocAdapter())
	s := &s78Env{dpEnv: e}
	s.tenant = e.newTenant(t)
	s.builder = uuid.NewString()
	s.bctx, s.cctx = builderCtxOf(s.tenant, s.builder), legacyCtxOf(s.tenant)
	e.deliver(t, s.tenant, s.builder)
	s.fields = NewFieldService(e.dyn, e.meta).WithDraftFields(NewDraftFieldRepo(e.meta, e.drafts, e.gate))
	s.layouts = NewLayoutService(e.meta, NewDraftLayoutRepo(metadata.NewLayoutRepository(e.gdb), e.drafts, e.gate), nil).WithSchemaCutoff(e.gate)
	return s
}

func (s *s78Env) formHas(t *testing.T, ctx context.Context, field string) bool {
	t.Helper()
	res, err := s.layouts.GetLayout(ctx, s.tenant, "hk_deals", "edit")
	if err != nil {
		t.Fatalf("GetLayout: %v", err)
	}
	b, _ := json.Marshal(res.Views["edit"].LayoutData)
	return strings.Contains(string(b), `"`+field+`"`)
}

type liveField struct {
	Label    string `db:"label"`
	Required bool   `db:"is_required"`
	Order    int    `db:"field_order"`
	Options  string `db:"options"`
}

func (s *s78Env) liveField(t *testing.T, id string) liveField {
	t.Helper()
	var f liveField
	if err := s.sdb.Get(&f, `SELECT label, is_required, field_order, COALESCE(CAST(options AS CHAR), '') AS options FROM sys_fields WHERE tenant_id = ? AND id = ?`, s.tenant, id); err != nil {
		t.Fatalf("live field %s: %v", id, err)
	}
	return f
}

func (s *s78Env) createDeal(ctx context.Context, n string) error {
	_, err := s.rec.Create(ctx, engAdmin(hkUser), s.tenant, hkUser, "hk_deals", map[string]any{"name": "d" + n, "code": "C-" + n + s.tenant[:6], "stage": "Proposal"})
	return err
}

func TestSchemaNewHiddenUntilPublish_Integration(t *testing.T) {
	s := s78Setup(t)

	// T119: a REQUIRED field on a delivered workspace is born optional + a draft that turns it on.
	f, err := s.fields.AddField(s.bctx, s.tenant, AddFieldInput{EntityName: "hk_deals", Field: FieldInput{Name: "priority", Label: "Priority", FieldType: "text", IsRequired: true}})
	if err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if lf := s.liveField(t, f.ID); lf.Required {
		t.Fatal("live is_required = 1 right after AddField: staff would be blocked by a field they cannot see")
	}
	rows, _ := s.drafts.ListByType(s.bctx, s.tenant, DocTypeField)
	if len(rows) != 1 || !s78Doc(t, rows[0]).IsRequired {
		t.Fatalf("want one field draft with isRequired:true, got %d rows", len(rows))
	}
	if err := s.createDeal(s.cctx, "1"); err != nil {
		t.Fatalf("a staff member could not create a record while the required field was unpublished: %v", err)
	}

	// T117 end to end: the customer's form does not carry the new field, the builder's does.
	if s.formHas(t, s.cctx, "priority") {
		t.Error("customer form shows a field born after the last publish")
	}
	if !s.formHas(t, s.bctx, "priority") {
		t.Error("builder (draft overlay) form does not show the new field")
	}
	// the builder's field list carries the new/pending markers
	got, err := NewDraftFieldRepo(s.meta, s.drafts, s.gate).ListFields(s.bctx, s.tenant, s78EntityID(t, s))
	if err != nil {
		t.Fatal(err)
	}
	var mark *metadata.FieldDefinition
	for i := range got {
		if got[i].ID == f.ID {
			mark = &got[i]
		}
	}
	if mark == nil || mark.DraftState != draftStateNew || !mark.PendingRequired || !mark.IsRequired {
		t.Fatalf("builder overlay of the new field = %+v, want state new, required, pendingRequired", mark)
	}

	// T120: label goes to the draft, options.indexed applies at once.
	idxOn := map[string]any{"indexed": true}
	label := "Mức ưu tiên"
	if _, err := s.fields.EditField(s.bctx, s.tenant, EditFieldInput{FieldID: f.ID, Label: &label, Options: idxOn}); err != nil {
		t.Fatalf("EditField: %v", err)
	}
	lf := s.liveField(t, f.ID)
	if lf.Label != "Priority" || !strings.Contains(lf.Options, `"indexed"`) {
		t.Fatalf("live after EditField = %+v, want the old label and indexed:true applied now", lf)
	}
	rows, _ = s.drafts.ListByType(s.bctx, s.tenant, DocTypeField)
	if len(rows) != 1 || s78Doc(t, rows[0]).Label != "Mức ưu tiên" || strings.Contains(string(rows[0].DraftJSON), "indexed") {
		t.Fatalf("field draft = %s, want the new label and no indexed key", rows[0].DraftJSON)
	}
	// a type change stays refused and says how to get there
	_, err = s.fields.EditField(s.bctx, s.tenant, EditFieldInput{FieldID: f.ID, FieldType: "number"})
	wantCode(t, err, "FIELD_TYPE_IMMUTABLE")

	// T120a: reorder keeps sys_fields.field_order; every moved field gets a draft.
	live, _ := s.meta.ListFields(s.bctx, s.tenant, s78EntityID(t, s))
	before := map[string]int{}
	order := make([]string, 0, len(live))
	for i := len(live) - 1; i >= 0; i-- {
		order = append(order, live[i].ID)
	}
	for _, lfd := range live {
		before[lfd.ID] = lfd.FieldOrder
	}
	if _, err := s.fields.ReorderFields(s.bctx, s.tenant, ReorderFieldsInput{EntityName: "hk_deals", Order: order}); err != nil {
		t.Fatalf("ReorderFields: %v", err)
	}
	after, _ := s.meta.ListFields(s.bctx, s.tenant, s78EntityID(t, s))
	for _, lfd := range after {
		if before[lfd.ID] != lfd.FieldOrder {
			t.Errorf("field %s: live field_order changed %d -> %d before Publish", lfd.Name, before[lfd.ID], lfd.FieldOrder)
		}
	}
	if rows, _ = s.drafts.ListByType(s.bctx, s.tenant, DocTypeField); len(rows) < 2 {
		t.Errorf("reorder left %d field drafts, want one per moved field", len(rows))
	}

	// Publish releases the lot in ONE version.
	res, err := s.publishWithCurrentHash(s.bctx, s.tenant, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	lf = s.liveField(t, f.ID)
	if !lf.Required || lf.Label != label || !strings.Contains(lf.Options, `"indexed"`) {
		t.Fatalf("live after Publish = %+v, want required, new label and indexed kept", lf)
	}
	if !s.formHas(t, s.cctx, "priority") {
		t.Error("customer form still hides the field after Publish")
	}
	if s.draftCount(t, s.tenant) != 0 {
		t.Error("drafts left after Publish")
	}
	if err := s.createDeal(s.cctx, "2"); err == nil {
		t.Error("record created without the now-required field")
	}
	// T121: the baseline already covered "field"; a later publish does not snapshot it again.
	var covered []string
	_ = s.sdb.Select(&covered, `SELECT COALESCE(CAST(covered_types AS CHAR), '') FROM sys_meta_versions WHERE tenant_id = ? ORDER BY seq`, s.tenant)
	if len(covered) != 2 || !strings.Contains(covered[0], `"field"`) || strings.Contains(covered[1], "field") {
		t.Errorf("covered_types by version = %v (publish %d), want field on the baseline only", covered, res.VersionNo)
	}
}

// A LIVE delete of a field (or its whole entity) - the customer's own admin, not the builder: S7-9
// turns the builder's delete into a draft - must take the field's pending draft with it; an orphan
// draft would 409 every Publish and 500 a forced one.
func TestDeleteFieldAndEntityDropFieldDrafts_Integration(t *testing.T) {
	s := s78Setup(t)
	add := func(name string) *metadata.FieldDefinition {
		f, err := s.fields.AddField(s.bctx, s.tenant, AddFieldInput{EntityName: "hk_deals", Field: FieldInput{Name: name, Label: name, FieldType: "text", IsRequired: true}})
		if err != nil {
			t.Fatalf("AddField %s: %v", name, err)
		}
		return f
	}
	f := add("doomed")
	if s.draftCount(t, s.tenant) != 1 {
		t.Fatal("setup: want one field draft")
	}
	if err := s.fields.DeleteField(s.cctx, s.tenant, DeleteFieldInput{FieldID: f.ID}); err != nil {
		t.Fatalf("DeleteField: %v", err)
	}
	if n := s.draftCount(t, s.tenant); n != 0 {
		t.Fatalf("drafts after DeleteField = %d, want 0 (orphan draft)", n)
	}

	add("doomed2")
	entities := NewEntityService(s.dyn, s.meta).WithDraftFields(NewDraftFieldRepo(s.meta, s.drafts, s.gate))
	if err := entities.SoftDeleteEntity(s.cctx, s.tenant, "hk_deals"); err != nil {
		t.Fatalf("SoftDeleteEntity: %v", err)
	}
	if n := s.draftCount(t, s.tenant); n != 0 {
		t.Fatalf("drafts after SoftDeleteEntity = %d, want 0", n)
	}
}

// T121: a delivery made BEFORE the field type existed snapshots it at its first publish after, and
// writes "field" into that version's covered_types.
func TestFieldTypeAddedAfterDelivery_SnapshotsOnFirstPublish_Integration(t *testing.T) {
	e := dpSetupWith(t, NewWorkflowDocAdapter())
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx := builderCtxOf(tenant, builder)
	e.deliver(t, tenant, builder) // registry without "field"
	// the same database, a registry that now knows the type
	reg := metaversion.NewRegistry(NewWorkflowDocAdapter(), NewFieldDocAdapter())
	drafts := metaversion.NewDraftRepo(e.sdb, reg)
	pub := NewPublishService(e.sdb, drafts, e.vers, reg, e.gate, e.audit, nil)
	fieldRepo := NewDraftFieldRepo(e.meta, drafts, e.gate)
	fs := NewFieldService(e.dyn, e.meta).WithDraftFields(fieldRepo)

	fields, _ := e.meta.ListFields(bctx, tenant, entityIDOf(t, e, tenant))
	label := "Tên mới"
	if _, err := fs.EditField(bctx, tenant, EditFieldInput{FieldID: fields[0].ID, Label: &label}); err != nil {
		t.Fatalf("EditField: %v", err)
	}
	_, hash, err := pub.Changes(bctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Publish(bctx, tenant, PublishRequest{ChangesetHash: hash}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	var covered string
	if err := e.sdb.Get(&covered, `SELECT CAST(covered_types AS CHAR) FROM sys_meta_versions WHERE tenant_id = ? AND version_no = 2`, tenant); err != nil || !strings.Contains(covered, `"field"`) {
		t.Fatalf("v2 covered_types = %q (%v), want it to list the late type", covered, err)
	}
	var snaps int
	_ = e.sdb.Get(&snaps, `SELECT COUNT(*) FROM sys_meta_version_docs WHERE tenant_id = ? AND doc_type = 'field' AND op = 'snapshot'`, tenant)
	if snaps == 0 {
		t.Error("no field snapshot rows: a rollback could not restore the pre-publish fields")
	}
}

func s78Doc(t *testing.T, r metaversion.DraftRow) fieldDoc {
	t.Helper()
	var d fieldDoc
	if err := json.Unmarshal(r.DraftJSON, &d); err != nil {
		t.Fatalf("decode draft: %v", err)
	}
	return d
}

func s78EntityID(t *testing.T, s *s78Env) string { return entityIDOf(t, s.dpEnv, s.tenant) }

func entityIDOf(t *testing.T, e dpEnv, tenant string) string {
	t.Helper()
	var id string
	if err := e.sdb.Get(&id, `SELECT id FROM sys_entities WHERE tenant_id = ? AND name = 'hk_deals' AND deleted_at IS NULL`, tenant); err != nil {
		t.Fatalf("entity id: %v", err)
	}
	return id
}

// f2-draft-labels-data-menu: the builder's table list marks a table born after the last publish "new"
// (the customer does not see it yet), next to the existing "deleted" mark.
func TestMarkEntities_NewAfterDelivery_Integration(t *testing.T) {
	s := s78Setup(t)
	// the seeded table predates delivery (raw INSERT defaults use the DB clock, which can differ from the Go clock)
	if _, err := s.sdb.Exec(`UPDATE sys_entities SET created_at = ? WHERE tenant_id = ?`, time.Now().Add(-48*time.Hour), s.tenant); err != nil {
		t.Fatalf("age seeded entity: %v", err)
	}
	id := uuid.NewString()
	if _, err := s.sdb.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name, created_at, updated_at) VALUES (?, ?, 'hk_fresh', 'Fresh', ?, ?, ?)`,
		id, s.tenant, "obj_"+strings.ReplaceAll(id, "-", "")[:20], time.Now(), time.Now()); err != nil {
		t.Fatalf("insert entity: %v", err)
	}
	list, err := s.meta.ListEntities(s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewDraftFieldRepo(s.meta, s.drafts, s.gate)
	marked, err := repo.MarkDeletedEntities(s.bctx, s.tenant, list)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range marked {
		want := ""
		if e.Name == "hk_fresh" {
			want = draftStateNew
		}
		if e.DraftState != want {
			t.Errorf("builder overlay: %s draftState = %q, want %q", e.Name, e.DraftState, want)
		}
	}
	plain, err := repo.MarkDeletedEntities(s.cctx, s.tenant, list)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range plain {
		if e.DraftState != "" {
			t.Errorf("customer reader: %s carries draftState %q", e.Name, e.DraftState)
		}
	}
}
