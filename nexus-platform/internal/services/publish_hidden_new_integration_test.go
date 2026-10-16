//go:build integration

package services

// publish_hidden_new_integration_test.go — a table or field born after the last publish is hidden from
// the customer until the next publish (specs/063 FR-060). Until now that "next publish" had nothing to
// carry it: a non-required new field has no draft, so pendingCount stayed 0, the change list stayed
// empty and Publish answered NOTHING_TO_PUBLISH — the field was an invisible draft nobody could release.

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/metaversion"
)

func TestNewSchemaIsPendingAndPublishes_Integration(t *testing.T) {
	s := s78Setup(t)
	// the seeded table and fields predate delivery (raw INSERT defaults use the DB clock, which can differ from the Go clock)
	old := time.Now().Add(-48 * time.Hour)
	if _, err := s.sdb.Exec(`UPDATE sys_entities SET created_at = ? WHERE tenant_id = ?`, old, s.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sdb.Exec(`UPDATE sys_fields SET created_at = ? WHERE tenant_id = ?`, old, s.tenant); err != nil {
		t.Fatal(err)
	}
	st, err := s.pub.State(s.bctx, s.tenant)
	if err != nil || !st.Delivered || st.PendingCount != 0 {
		t.Fatalf("delivered tenant with nothing new: state=%+v err=%v, want delivered and pendingCount 0", st, err)
	}

	// 1. a plain (not required) field: no draft is ever written for it
	f, err := s.fields.AddField(s.bctx, s.tenant, AddFieldInput{EntityName: "hk_deals", Field: FieldInput{Name: "memo", Label: "Ghi chú nhanh", FieldType: "text"}})
	if err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if n := s.draftCount(t, s.tenant); n != 0 {
		t.Fatalf("setup: a plain new field wrote %d drafts, want 0", n)
	}
	// 2. a new table with one field of its own (the field must NOT be listed apart: the table carries it)
	entID := uuid.NewString()
	born := time.Now().Truncate(time.Second) // the repository's BeforeCreate hooks floor created_at; a raw insert must too, or MySQL rounds it up past the cutoff
	if _, err := s.sdb.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name, created_at, updated_at) VALUES (?, ?, 'hk_fresh', 'Bảng mới', ?, ?, ?)`,
		entID, s.tenant, "obj_"+strings.ReplaceAll(entID, "-", "")[:20], born, born); err != nil {
		t.Fatalf("insert entity: %v", err)
	}
	if _, err := s.sdb.Exec(`INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type, created_at, updated_at) VALUES (?, ?, ?, 'title', 'Tiêu đề', 'text', ?, ?)`,
		uuid.NewString(), entID, s.tenant, born, born); err != nil {
		t.Fatalf("insert field: %v", err)
	}

	st, err = s.pub.State(s.bctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingCount != 2 {
		t.Fatalf("pendingCount = %d, want 2 (the new field + the new table)", st.PendingCount)
	}
	items, hash, err := s.pub.Changes(s.bctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("changes = %+v, want the new field and the new table", items)
	}
	byType := map[string]ChangeItem{}
	for _, it := range items {
		byType[it.DocType] = it
		if it.Op != "create" || it.Conflict || it.Discardable {
			t.Errorf("hidden-new item %+v: want op create, no conflict, not discardable (there is no draft to drop)", it)
		}
	}
	if byType["field"].DocKey != f.ID || byType["field"].EntityName != "hk_deals" || !strings.Contains(byType["field"].Label, "Ghi chú nhanh") {
		t.Errorf("field item = %+v", byType["field"])
	}
	if byType["entity"].DocKey != entID || byType["entity"].Label != "Bảng mới" {
		t.Errorf("entity item = %+v", byType["entity"])
	}
	// the customer cannot see the field yet
	if s.formHas(t, s.cctx, "memo") {
		t.Fatal("customer form shows a field born after the last publish")
	}

	// a stale fingerprint is refused (a third thing was born after the builder looked)
	if _, err := s.fields.AddField(s.bctx, s.tenant, AddFieldInput{EntityName: "hk_deals", Field: FieldInput{Name: "memo2", Label: "Memo 2", FieldType: "text"}}); err != nil {
		t.Fatal(err)
	}
	_, err = s.pub.Publish(s.bctx, s.tenant, PublishRequest{ChangesetHash: hash})
	wantCode(t, err, "DRAFTS_CHANGED")

	// "Bỏ tất cả" drops drafts only: a hidden-new item is never silently deleted
	if _, hash, err = s.pub.Changes(s.bctx, s.tenant); err != nil {
		t.Fatal(err)
	}
	if err := s.pub.DiscardAll(s.bctx, s.tenant, hash); err != nil {
		t.Fatalf("DiscardAll: %v", err)
	}
	if st, _ = s.pub.State(s.bctx, s.tenant); st.PendingCount != 3 {
		t.Fatalf("pendingCount after DiscardAll = %d, want 3 (nothing to discard)", st.PendingCount)
	}

	// Publish releases them in one version
	res, err := s.publishWithCurrentHash(s.bctx, s.tenant, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.VersionNo != 2 || res.DocCount != 3 {
		t.Errorf("publish result = %+v, want version 2 with 3 released items", res)
	}
	if !s.formHas(t, s.cctx, "memo") {
		t.Error("customer form still hides the field after Publish")
	}
	if st, _ = s.pub.State(s.bctx, s.tenant); st.PendingCount != 0 {
		t.Errorf("pendingCount after Publish = %d, want 0", st.PendingCount)
	}
	_, err = s.publishWithCurrentHash(s.bctx, s.tenant, false)
	wantCode(t, err, "NOTHING_TO_PUBLISH")
}

// A new field that DOES have a draft (required, or edited) is listed once - as the draft - not twice.
func TestNewSchemaWithDraftIsListedOnce_Integration(t *testing.T) {
	s := s78Setup(t)
	old := time.Now().Add(-48 * time.Hour)
	_, _ = s.sdb.Exec(`UPDATE sys_entities SET created_at = ? WHERE tenant_id = ?`, old, s.tenant)
	_, _ = s.sdb.Exec(`UPDATE sys_fields SET created_at = ? WHERE tenant_id = ?`, old, s.tenant)
	f, err := s.fields.AddField(s.bctx, s.tenant, AddFieldInput{EntityName: "hk_deals", Field: FieldInput{Name: "prio", Label: "Prio", FieldType: "text", IsRequired: true}})
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := s.pub.Changes(s.bctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].DocKey != f.ID || !items[0].Discardable {
		t.Fatalf("changes = %+v, want the single field draft", items)
	}
	if st, _ := s.pub.State(s.bctx, s.tenant); st.PendingCount != 1 {
		t.Fatalf("pendingCount = %d, want 1", st.PendingCount)
	}
}

// ageSchema moves the seeded schema and the head version's cutoff back in time, so a test can place rows
// on either side of the cutoff at will (the Go and DB clocks may differ by a little).
func (s *s78Env) ageSchema(t *testing.T) time.Time {
	t.Helper()
	old, cut := time.Now().UTC().Add(-48*time.Hour).Truncate(time.Second), time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, q := range []string{`UPDATE sys_entities SET created_at = ?, updated_at = ? WHERE tenant_id = ?`, `UPDATE sys_fields SET created_at = ?, updated_at = ? WHERE tenant_id = ?`} {
		if _, err := s.sdb.Exec(q, old, old, s.tenant); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.sdb.Exec(`UPDATE sys_meta_versions SET schema_cutoff_at = ? WHERE tenant_id = ?`, cut, s.tenant); err != nil {
		t.Fatal(err)
	}
	s.gate.RefreshCutoff(s.tenant, cut)
	return cut
}

// oldTable inserts a second table that predates delivery.
func (s *s78Env) oldTable(t *testing.T, name string) string {
	t.Helper()
	id, old := uuid.NewString(), time.Now().UTC().Add(-48*time.Hour).Truncate(time.Second)
	if _, err := s.sdb.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, s.tenant, name, name, "obj_"+strings.ReplaceAll(id, "-", "")[:20], old, old); err != nil {
		t.Fatalf("insert table: %v", err)
	}
	return id
}

func (s *s78Env) rawField(t *testing.T, entityID, name string, born time.Time) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := s.sdb.Exec(`INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'text', ?, ?)`,
		id, entityID, s.tenant, name, name, born, born); err != nil {
		t.Fatalf("insert field: %v", err)
	}
	return id
}

func (s *s78Env) customerSeesRelationship(t *testing.T, name string) bool {
	t.Helper()
	rels, err := s.meta.ListAllRelationships(s.cctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if at, apply, err := s.gate.SchemaCutoff(s.cctx, s.tenant); err != nil {
		t.Fatal(err)
	} else if apply {
		rels = HideRelationshipsNewerThan(rels, at)
	}
	for _, r := range rels {
		if r.Name == name {
			return true
		}
	}
	return false
}

func (s *s78Env) versionByNo(t *testing.T, no uint) VersionSummary {
	t.Helper()
	vs, _, err := s.pub.ListVersions(s.bctx, s.tenant, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.VersionNo != nil && *v.VersionNo == no {
			return v
		}
	}
	t.Fatalf("version %d not found", no)
	return VersionSummary{}
}

// An N-N relation registered between two tables the customer already has writes no field and no draft, yet
// GET /metadata/relationships hides it by the schema cutoff: it must be a pending change the next publish releases.
func TestNewNNRelationshipIsPendingAndPublishes_Integration(t *testing.T) {
	s := s78Setup(t)
	s.ageSchema(t)
	s.oldTable(t, "hk_tags")
	born := time.Now().UTC().Truncate(time.Second)
	relID := uuid.NewString()
	if _, err := s.sdb.Exec(`INSERT INTO sys_relationships (id, tenant_id, name, parent_entity_name, child_entity_name, relationship_type, join_table_name, created_at, updated_at)
		VALUES (?, ?, 'deal_tags', 'hk_deals', 'hk_tags', 'N-N', ?, ?, ?)`, relID, s.tenant, "rel_"+relID[:8], born, born); err != nil {
		t.Fatalf("insert relationship: %v", err)
	}
	if s.customerSeesRelationship(t, "deal_tags") {
		t.Fatal("setup: the customer sees an N-N relation born after the last publish")
	}
	st, err := s.pub.State(s.bctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingCount != 1 {
		t.Fatalf("pendingCount = %d, want 1 (the new N-N relation)", st.PendingCount)
	}
	items, _, err := s.pub.Changes(s.bctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].DocType != "relationship" || items[0].DocKey != relID || items[0].Op != "create" || items[0].Discardable {
		t.Fatalf("changes = %+v, want one read-only create row for the relation", items)
	}
	res, err := s.publishWithCurrentHash(s.bctx, s.tenant, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !s.customerSeesRelationship(t, "deal_tags") {
		t.Fatal("the customer still does not see the relation after Publish")
	}
	v, err := s.pub.GetVersion(s.bctx, s.tenant, s.versionByNo(t, res.VersionNo).ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.DocCount != 1 || len(v.Docs) != 1 || v.Docs[0].DocType != "relationship" || v.Docs[0].Op != "create" {
		t.Fatalf("version = docCount %d docs %+v, want the one released relation recorded", v.DocCount, v.Docs)
	}
}

// The cutoff a publish writes releases what the builder was shown - nothing born after the read, even when it
// commits before the publish does (AddField / CreateEntity do not take the tenant lock).
func TestPublishReleasesOnlyWhatWasSeen_Integration(t *testing.T) {
	t.Run("born later, committed during the publish: stays pending", func(t *testing.T) {
		s := s78Setup(t)
		s.ageSchema(t)
		ent := s78EntityID(t, s)
		s.rawField(t, ent, "seen", time.Now().UTC().Add(-10*time.Second).Truncate(time.Second))
		_, hash, err := s.pub.Changes(s.bctx, s.tenant)
		if err != nil {
			t.Fatal(err)
		}
		s.pub.afterPendingRead = func() { s.rawField(t, ent, "unseen", time.Now().UTC().Add(-5*time.Second).Truncate(time.Second)) }
		if _, err := s.pub.Publish(s.bctx, s.tenant, PublishRequest{ChangesetHash: hash}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		s.pub.afterPendingRead = nil
		items, _, err := s.pub.Changes(s.bctx, s.tenant)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || !strings.Contains(items[0].Label, "unseen") {
			t.Fatalf("changes after publish = %+v, want the unseen field still pending (not released unseen)", items)
		}
		if s.formHas(t, s.cctx, "unseen") {
			t.Fatal("the customer sees a field the builder never saw in the change list")
		}
	})
	t.Run("same second as a seen row, committed during the publish: DRAFTS_CHANGED", func(t *testing.T) {
		s := s78Setup(t)
		s.ageSchema(t)
		ent := s78EntityID(t, s)
		at := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Second)
		s.rawField(t, ent, "seen", at)
		_, hash, err := s.pub.Changes(s.bctx, s.tenant)
		if err != nil {
			t.Fatal(err)
		}
		s.pub.afterPendingRead = func() { s.rawField(t, ent, "twin", at) }
		_, err = s.pub.Publish(s.bctx, s.tenant, PublishRequest{ChangesetHash: hash})
		s.pub.afterPendingRead = nil
		wantCode(t, err, "DRAFTS_CHANGED")
		if s.formHas(t, s.cctx, "twin") || s.formHas(t, s.cctx, "seen") {
			t.Fatal("a refused publish released something")
		}
	})
}

// A publish's cutoff uncovers more than the rows its change list names: the fields and N-N relations of a table
// that is itself new, and the 1-N row a relation field writes (a second after the field when a second boundary
// falls between the two INSERTs), are folded into a listed row. The cutoff must cover them too, or they stay
// hidden after the publish with nothing left to publish (a required one breaks the customer's create).
func TestPublishReleasesWhatTheListFoldsIn_Integration(t *testing.T) {
	s := s78Setup(t)
	s.ageSchema(t)
	s.oldTable(t, "hk_owners")
	base := time.Now().UTC().Add(-20 * time.Second).Truncate(time.Second)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	rel := func(name, typ, parent, child string, born time.Time) string {
		t.Helper()
		id := uuid.NewString()
		if _, err := s.sdb.Exec(`INSERT INTO sys_relationships (id, tenant_id, name, parent_entity_name, child_entity_name, relationship_type, join_table_name, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, s.tenant, name, parent, child, typ, "rel_"+id[:8], born, born); err != nil {
			t.Fatalf("insert relationship %s: %v", name, err)
		}
		return id
	}
	table := uuid.NewString()
	if _, err := s.sdb.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name, created_at, updated_at) VALUES (?, ?, 'pr_tags', 'pr_tags', ?, ?, ?)`,
		table, s.tenant, "obj_"+strings.ReplaceAll(table, "-", "")[:20], at(0), at(0)); err != nil {
		t.Fatalf("insert table: %v", err)
	}
	owner := s.rawField(t, s78EntityID(t, s), "owner_ref", at(1))    // a relation field on an old table (listed) ...
	oneN := rel("deal_owner", "1-N", "hk_owners", "hk_deals", at(2)) // ... and its 1-N row, a second later (folded)
	colour := s.rawField(t, table, "colour", at(4))                  // fields added to the new table after it (folded)
	prio := s.rawField(t, table, "prio", at(5))
	nn := rel("deal_prtags", "N-N", "hk_deals", "pr_tags", at(6)) // an N-N between an old table and the new one (folded)

	res, err := s.publishWithCurrentHash(s.bctx, s.tenant, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM (
			SELECT created_at FROM sys_entities WHERE tenant_id = ? AND id = ?
			UNION ALL SELECT created_at FROM sys_fields WHERE tenant_id = ? AND id IN (?, ?, ?)
			UNION ALL SELECT created_at FROM sys_relationships WHERE tenant_id = ? AND id IN (?, ?)) born
		WHERE born.created_at > (SELECT schema_cutoff_at FROM sys_meta_versions WHERE tenant_id = ? AND version_no = ?)`,
		s.tenant, table, s.tenant, owner, colour, prio, s.tenant, oneN, nn, s.tenant, res.VersionNo); n != 0 {
		t.Fatalf("%d definitions the publish read are still behind its cutoff (hidden from the customer)", n)
	}
	for _, name := range []string{"deal_owner", "deal_prtags"} {
		if !s.customerSeesRelationship(t, name) {
			t.Errorf("the customer does not see relation %s after the publish that released its parent", name)
		}
	}
	st, err := s.pub.State(s.bctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingCount != 0 {
		t.Fatalf("pendingCount after the publish = %d, want 0", st.PendingCount)
	}
}

// A row stamped after the cutoff a publish writes (its inserting process's clock runs ahead of the publisher's,
// so releaseCutoff clamps to now) stays hidden: it must not be recorded as released, and it stays pending.
func TestPublishDoesNotRecordRowsAfterItsCutoff_Integration(t *testing.T) {
	s := s78Setup(t)
	s.ageSchema(t)
	ent := s78EntityID(t, s)
	seen := s.rawField(t, ent, "seen", time.Now().UTC().Add(-10*time.Second).Truncate(time.Second))
	s.rawField(t, ent, "ahead", time.Now().UTC().Add(30*time.Second).Truncate(time.Second))
	res, err := s.publishWithCurrentHash(s.bctx, s.tenant, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	v, err := s.pub.GetVersion(s.bctx, s.tenant, s.versionByNo(t, res.VersionNo).ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.DocCount != 1 || len(v.Docs) != 1 || v.Docs[0].DocKey != seen {
		t.Fatalf("version = docCount %d docs %+v, want only the field its cutoff released", v.DocCount, v.Docs)
	}
	items, _, err := s.pub.Changes(s.bctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Label, "ahead") {
		t.Fatalf("changes after publish = %+v, want the field stamped after the cutoff still pending", items)
	}
}

// Publish of hidden-new rows only records them (docCount = docs), and Quay lại to the version before hides them
// again through that version's cutoff - without deleting the definition - instead of answering NOTHING_TO_PUBLISH.
func TestHiddenNewReleaseIsRecordedAndRolledBack_Integration(t *testing.T) {
	s := s78Setup(t)
	s.ageSchema(t)
	f, err := s.fields.AddField(s.bctx, s.tenant, AddFieldInput{EntityName: "hk_deals", Field: FieldInput{Name: "memo", Label: "Ghi chú nhanh", FieldType: "text"}})
	if err != nil {
		t.Fatalf("AddField: %v", err)
	}
	res, err := s.publishWithCurrentHash(s.bctx, s.tenant, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	v2, err := s.pub.GetVersion(s.bctx, s.tenant, s.versionByNo(t, res.VersionNo).ID)
	if err != nil {
		t.Fatal(err)
	}
	if v2.DocCount != 1 || len(v2.Docs) != 1 || v2.Docs[0].Op != "create" || v2.Docs[0].DocKey != f.ID || len(v2.Docs[0].After) == 0 {
		t.Fatalf("version %d = docCount %d docs %+v, want the released field recorded as create with its body", res.VersionNo, v2.DocCount, v2.Docs)
	}
	if !s.formHas(t, s.cctx, "memo") {
		t.Fatal("setup: the customer does not see the published field")
	}

	v1 := s.versionByNo(t, 1)
	plan, err := s.pub.RollbackPlan(s.bctx, s.tenant, v1.ID)
	if err != nil {
		t.Fatalf("RollbackPlan: %v", err)
	}
	if len(plan.Changes) != 0 || len(plan.Rehides) != 1 || plan.Rehides[0].DocKey != f.ID {
		t.Fatalf("plan = changes %+v rehides %+v, want no change and the field hidden again", plan.Changes, plan.Rehides)
	}
	if _, err := s.pub.Rollback(s.bctx, s.tenant, v1.ID, RollbackRequest{ExpectedHeadVersion: res.VersionNo, ExpectedPlanToken: plan.PlanToken}); err != nil {
		t.Fatalf("Rollback to v1: %v", err)
	}
	if s.formHas(t, s.cctx, "memo") {
		t.Error("the customer still sees a field the target version did not have")
	}
	if n := s.count(t, `SELECT COUNT(*) FROM sys_fields WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, s.tenant, f.ID); n != 1 {
		t.Fatalf("the rollback deleted the field definition (live rows = %d)", n)
	}
	st, err := s.pub.State(s.bctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingCount != 1 {
		t.Fatalf("pendingCount after the rollback = %d, want 1 (the hidden field waits for the next publish)", st.PendingCount)
	}
	// a hidden-new row is a pending change too: Quay lại refuses while it waits, as it does for a draft
	_, err = s.pub.Rollback(s.bctx, s.tenant, s.versionByNo(t, res.VersionNo).ID, RollbackRequest{ExpectedHeadVersion: res.VersionNo + 1})
	wantCode(t, err, "DRAFTS_PENDING")
}

// A field born on a table the builder is deleting is not a "create" to publish, and the fingerprint of a hidden
// row moves when the row is edited (updated_at), not only when it is born.
func TestHiddenNewSkipsDeletedTableAndTracksEdits_Integration(t *testing.T) {
	s := s78Setup(t)
	s.ageSchema(t)
	tags := s.oldTable(t, "hk_tags")
	born := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Second)
	s.rawField(t, tags, "colour", born)
	keep := s.rawField(t, s78EntityID(t, s), "memo", born)
	if err := s.drafts.Upsert(s.bctx, s.tenant, metaversion.UpsertInput{DocType: DocTypeEntity, DocKey: tags, Op: metaversion.OpDelete, EntityName: "hk_tags", Label: "hk_tags", UserID: s.builder}); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	items, hash, err := s.pub.Changes(s.bctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.DocType == DocTypeField && it.EntityName == "hk_tags" {
			t.Fatalf("changes = %+v: a field of a table pending delete is listed as a create", items)
		}
	}
	if len(items) != 2 {
		t.Fatalf("changes = %+v, want the table delete and the memo field", items)
	}
	if _, err := s.sdb.Exec(`UPDATE sys_fields SET label = 'Memo 2', updated_at = ? WHERE tenant_id = ? AND id = ?`, born.Add(5*time.Second), s.tenant, keep); err != nil {
		t.Fatal(err)
	}
	if _, again, _ := s.pub.Changes(s.bctx, s.tenant); again == hash {
		t.Fatal("changesetHash did not move when a hidden-new field was edited")
	}
}
