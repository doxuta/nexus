package services

// draft_schema_delete_test.go — specs/063-draft-publish S7-9 (T126, T129): a builder's delete of a field
// or table on a delivered workspace is a draft (op=delete) that changes nothing live; the publish and
// retry paths never issue DDL.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

func TestDraftFieldRepo_BuilderDeleteIsADraft_NothingLiveChanges(t *testing.T) {
	e := newDFEnv(true)
	f, _ := e.live.GetFieldByID(context.Background(), "", "f1")
	drafted, err := e.repo.SaveFieldDelete(builderCtx(), dwTenant, f)
	if err != nil || !drafted {
		t.Fatalf("SaveFieldDelete = (%v, %v), want a draft", drafted, err)
	}
	if len(e.live.rows) != 3 || e.live.updates != 0 {
		t.Fatalf("live rows=%d updates=%d, want 3/0: the customer keeps the field until Publish", len(e.live.rows), e.live.updates)
	}
	d, _ := e.drafts.GetByKey(context.Background(), dwTenant, DocTypeField, "f1")
	if d == nil || d.Op != metaversion.OpDelete || len(d.DraftJSON) != 0 {
		t.Fatalf("draft = %+v, want one op=delete without a body", d)
	}
	if d.EntityName == nil || *d.EntityName != "deals" {
		t.Errorf("draft entity = %v, want deals (the change list names the table)", d.EntityName)
	}

	// the builder's list marks it deleted; the customer's list is the live one, unmarked
	got, _ := e.repo.ListFields(builderCtx(), dwTenant, "e1")
	for _, g := range got {
		want := ""
		if g.ID == "f1" {
			want = draftStateDeleted
		}
		if g.ID != "f3" && g.DraftState != want {
			t.Errorf("builder list: field %s state = %q, want %q", g.ID, g.DraftState, want)
		}
	}
	for _, g := range mustList(t, e.repo, legacyAdminCtx()) {
		if g.DraftState != "" {
			t.Errorf("customer list: field %s state = %q, want none", g.ID, g.DraftState)
		}
	}
	if b, _ := e.repo.GetFieldByID(builderCtx(), dwTenant, "f1"); b.DraftState != draftStateDeleted {
		t.Errorf("builder GetFieldByID state = %q, want deleted", b.DraftState)
	}

	// an edit would silently replace the delete draft: refused until Hoàn tác
	edit := *f
	edit.Label = "Other"
	if _, err := e.repo.SaveEdit(builderCtx(), dwTenant, &edit); err == nil {
		t.Fatal("SaveEdit on a field marked for deletion succeeded; it would replace the delete draft")
	} else if ve := (*ValidationError)(nil); !errors.As(err, &ve) || ve.Code != "FIELD_PENDING_DELETE" {
		t.Errorf("SaveEdit err = %v, want FIELD_PENDING_DELETE", err)
	}
	// reorder leaves it where it is instead of failing the whole batch
	if err := e.repo.SaveReorder(builderCtx(), dwTenant, "e1", []string{"f2", "f1", "f3"}); err != nil {
		t.Errorf("SaveReorder with a field marked for deletion: %v", err)
	}
	if d, _ := e.drafts.GetByKey(context.Background(), dwTenant, DocTypeField, "f1"); d == nil || d.Op != metaversion.OpDelete {
		t.Errorf("reorder replaced the delete draft: %+v", d)
	}

	// Hoàn tác = discard the draft: the mark disappears
	delete(e.drafts.rows, fdsKey(dwTenant, DocTypeField, "f1"))
	if b, _ := e.repo.GetFieldByID(builderCtx(), dwTenant, "f1"); b.DraftState != "" {
		t.Errorf("after undo the field still reads %q", b.DraftState)
	}
}

func mustList(t *testing.T, r *DraftFieldRepo, ctx context.Context) []metadata.FieldDefinition {
	t.Helper()
	got, err := r.ListFields(ctx, dwTenant, "e1")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDraftFieldRepo_DeleteFallsThroughOutsideDraftMode(t *testing.T) {
	f := &metadata.FieldDefinition{ID: "f1", EntityID: "e1", Label: "Name"}
	cases := []struct {
		name      string
		delivered bool
		ctx       context.Context
	}{
		{"not delivered", false, builderCtx()},
		{"delivered, customer admin", true, legacyAdminCtx()},
		{"delivered, publish applying", true, metaversion.WithPublishApply(builderCtx())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newDFEnv(tc.delivered)
			drafted, err := e.repo.SaveFieldDelete(tc.ctx, dwTenant, f)
			if err != nil || drafted || len(e.drafts.rows) != 0 {
				t.Fatalf("SaveFieldDelete = (%v, %v), drafts=%d, want a live delete (false, nil, 0)", drafted, err, len(e.drafts.rows))
			}
			drafted, err = e.repo.SaveEntityDelete(tc.ctx, dwTenant, &metadata.EntityDefinition{ID: "e1", Name: "deals"})
			if err != nil || drafted || len(e.drafts.rows) != 0 {
				t.Fatalf("SaveEntityDelete = (%v, %v), drafts=%d, want a live delete", drafted, err, len(e.drafts.rows))
			}
		})
	}
	// fail closed: a delivered tenant and a ctx that never passed DraftCaller must not delete live
	e := newDFEnv(true)
	if _, err := e.repo.SaveFieldDelete(context.Background(), dwTenant, f); !errors.Is(err, metaversion.ErrNoCaller) {
		t.Errorf("no caller: err = %v, want ErrNoCaller", err)
	}
}

func TestDraftFieldRepo_EntityDeleteMarksOnlyTheBuildersList(t *testing.T) {
	e := newDFEnv(true)
	ents := []metadata.EntityDefinition{{ID: "e1", Name: "deals"}, {ID: "e2", Name: "notes"}}
	drafted, err := e.repo.SaveEntityDelete(builderCtx(), dwTenant, &ents[0])
	if err != nil || !drafted {
		t.Fatalf("SaveEntityDelete = (%v, %v)", drafted, err)
	}
	d, _ := e.drafts.GetByKey(context.Background(), dwTenant, DocTypeEntity, "e1")
	if d == nil || d.Op != metaversion.OpDelete || d.EntityName == nil || *d.EntityName != "deals" {
		t.Fatalf("entity draft = %+v, want op=delete named deals", d)
	}
	b, err := e.repo.MarkDeletedEntities(builderCtx(), dwTenant, ents)
	if err != nil || b[0].DraftState != draftStateDeleted || b[1].DraftState != "" {
		t.Errorf("builder list = %+v (%v), want only deals marked", b, err)
	}
	c, err := e.repo.MarkDeletedEntities(legacyAdminCtx(), dwTenant, ents)
	if err != nil || c[0].DraftState != "" {
		t.Errorf("customer list = %+v (%v), want unmarked", c, err)
	}
	if ents[0].DraftState != "" {
		t.Error("MarkDeletedEntities mutated its input")
	}
}

// FR-068 (T129): the publish and retry paths never create, alter or drop a physical table or column.
// The two services they call back are DML-only; this pins that nobody wires a DDL call into the
// publish side. (The integration test also checks the physical table survives.)
func TestPublishPathNeverIssuesDDL(t *testing.T) {
	ddlCall := regexp.MustCompile(`\b(DropTable|CreateTable|AddIndexedColumn|DropIndexedColumn|AddPolymorphicColumns|DropPolymorphicColumns|AddUniqueColumn)\b|(?i)\b(ALTER|DROP|CREATE)\s+TABLE\b`)
	files, err := filepath.Glob("publish_*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob publish_*.go: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		if filepath.Ext(f) != ".go" || regexp.MustCompile(`_test\.go$`).MatchString(f) {
			continue
		}
		if hit := codeDDLCall(t, f, ddlCall); hit != "" {
			t.Errorf("%s calls %q: Publish must never change a table's structure (FR-068)", f, hit)
		}
	}
	// the entity delete the publish calls back is DML-only too
	if hit := codeDDLCall(t, "entity_management.go", ddlCall); hit != "" {
		t.Errorf("entity_management.go calls %q", hit)
	}
}

// codeDDLCall returns the first DDL name in the CODE of a file (comment lines are skipped: the
// comments here say, in words, what the code must not do).
func codeDDLCall(t *testing.T, file string, re *regexp.Regexp) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if m := re.FindString(line); m != "" {
			return m
		}
	}
	return ""
}

// sweepSpy records the instant of the sweep and reports 7 rows moved.
type sweepSpy struct {
	fakeSchema
	at time.Time
}

func (s *sweepSpy) SoftDeleteRecordsAt(_ context.Context, _, _ string, at time.Time) (int64, error) {
	s.at = at
	return 7, nil
}

// deleteMeta adds the tombstone capability SoftDeleteEntity asserts on the metadata writer.
type deleteMeta struct{ *fakeMeta }

func (deleteMeta) SoftDeleteEntity(context.Context, string, string) error { return nil }

// SoftDeleteEntity sweeps with the instant WithSweepAt carries and adds the moved rows to the counter.
func TestSoftDeleteEntity_UsesTheRecordedSweepInstant(t *testing.T) {
	spy := &sweepSpy{}
	svc := NewEntityService(spy, deleteMeta{&fakeMeta{knownEntities: map[string]bool{"deals": true}}})
	at := time.Date(2026, 10, 14, 3, 2, 1, 0, time.UTC)
	ctx, n := metaversion.WithSweepCount(metaversion.WithSweepAt(context.Background(), at))
	if err := svc.SoftDeleteEntity(ctx, "t1", "deals"); err != nil {
		t.Fatalf("SoftDeleteEntity: %v", err)
	}
	if !spy.at.Equal(at) {
		t.Errorf("swept at %v, want the recorded instant %v", spy.at, at)
	}
	if n.N.Load() != 7 {
		t.Errorf("sweep count = %d, want 7", n.N.Load())
	}
	// no instant on ctx (a delete outside a publish): now
	before := time.Now().UTC().Add(-time.Second)
	if err := svc.SoftDeleteEntity(context.Background(), "t1", "deals"); err != nil {
		t.Fatal(err)
	}
	if spy.at.Before(before) {
		t.Errorf("without WithSweepAt the sweep used %v, want now", spy.at)
	}
}
