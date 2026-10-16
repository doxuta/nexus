//go:build integration

package services

// rollback_restore_schema_integration_test.go — specs/063-draft-publish S7-10a/S7-10b (T134, T134a,
// T135) against a real MySQL: "Quay lại" over a published version that deleted a unique field, a
// relation field and a table brings them back with their relationship rows, their unique pivot and
// EXACTLY the records that delete trashed; a name or a unique value taken meanwhile refuses the whole
// rollback and leaves everything as it was.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/metadata"
)

type rrEnv struct {
	*s79Env
	accts string // hk_accts entity name, parent side of the relation field
}

// rrSetup: a tenant with hk_deals (code is unique) + hk_accts and a relation field hk_deals.acct, three
// live deals, then delivered (v1 = baseline).
func rrSetup(t *testing.T) *rrEnv {
	t.Helper()
	e := dpSetupWith(t, NewWorkflowDocAdapter(), NewFieldDocAdapter(), NewEntityDocAdapter())
	s := &s79Env{dpEnv: e}
	s.tenant, s.builder = e.newTenant(t), uuid.NewString()
	s.bctx, s.cctx = builderCtxOf(s.tenant, s.builder), legacyCtxOf(s.tenant)
	s.drafts4Fields = NewDraftFieldRepo(e.meta, e.drafts, e.gate)
	s.fields = NewFieldService(e.dyn, e.meta).WithDraftFields(s.drafts4Fields)
	s.entities = NewEntityService(e.dyn, e.meta).WithDraftFields(s.drafts4Fields)
	s.flaky = &flakyEntityDeleter{real: s.entities}
	e.pub.WithSchemaOps(s.fields, s.flaky)
	if _, _, err := s.entities.CreateEntity(context.Background(), s.tenant, CreateEntityInput{
		Name: "hk_accts", Label: "Accounts", Fields: []FieldInput{{Name: "name", Label: "Name", FieldType: "text"}}}); err != nil {
		t.Fatalf("create hk_accts: %v", err)
	}
	if _, err := s.fields.AddField(context.Background(), s.tenant, AddFieldInput{EntityName: "hk_deals",
		Field: FieldInput{Name: "acct", Label: "Account", FieldType: "relation", Options: map[string]any{"relatedEntity": "hk_accts"}}}); err != nil {
		t.Fatalf("add relation field: %v", err)
	}
	s.seedRows(t, s.tenant, 3)
	e.deliver(t, s.tenant, s.builder)
	return &rrEnv{s79Env: s, accts: "hk_accts"}
}

func (r *rrEnv) fieldID(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := r.sdb.Get(&id, `SELECT f.id FROM sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
		WHERE f.tenant_id = ? AND e.name = 'hk_deals' AND f.name = ?`, r.tenant, name); err != nil {
		t.Fatalf("field id %s: %v", name, err)
	}
	return id
}

func (r *rrEnv) pivotRows(t *testing.T) int {
	return r.count(t, `SELECT COUNT(*) FROM sys_unique_index WHERE tenant_id = ? AND entity_name = 'hk_deals' AND field_name = 'code'`, r.tenant)
}

func (r *rrEnv) liveRels(t *testing.T) int {
	return r.count(t, `SELECT COUNT(*) FROM sys_relationships WHERE tenant_id = ? AND child_entity_name = 'hk_deals' AND deleted_at IS NULL`, r.tenant)
}

// publishDeletes drafts the given deletes as the builder, publishes and requires every delete to have run.
func (r *rrEnv) publishDeletes(t *testing.T, deletes func()) *PublishResult {
	t.Helper()
	deletes()
	res, err := r.publishWithCurrentHash(r.bctx, r.tenant, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.SchemaStatus != "done" {
		t.Fatalf("schemaStatus = %q ops=%+v, want done", res.SchemaStatus, res.SchemaOps)
	}
	return res
}

func (r *rrEnv) rollbackTo(t *testing.T, targetNo, headNo uint) (*PublishResult, *RollbackPlanView, error) {
	t.Helper()
	id := r.versionID(t, targetNo)
	plan, err := r.pub.RollbackPlan(r.bctx, r.tenant, id)
	if err != nil {
		t.Fatalf("RollbackPlan: %v", err)
	}
	res, err := r.pub.Rollback(r.bctx, r.tenant, id, RollbackRequest{ExpectedHeadVersion: headNo, ExpectedPlanToken: plan.PlanToken, AcceptUntouched: true})
	return res, plan, err
}

func wantRollbackCode(t *testing.T, err error, code string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

// T134a: a unique field and a relation field deleted by a published version come back: the rows, the
// relationship registry row, the unique pivot (a duplicate is refused again) and the values.
func TestRollbackRestoresDeletedFields_Integration(t *testing.T) {
	r := rrSetup(t)
	code, acct := r.fieldID(t, "code"), r.fieldID(t, "acct")
	relName := ""
	if err := r.sdb.Get(&relName, `SELECT name FROM sys_relationships WHERE tenant_id = ? AND child_entity_name = 'hk_deals' AND deleted_at IS NULL`, r.tenant); err != nil {
		t.Fatalf("relationship row of the relation field: %v", err)
	}
	if r.pivotRows(t) != 3 {
		t.Fatalf("pivot rows before = %d, want 3", r.pivotRows(t))
	}
	res := r.publishDeletes(t, func() {
		for _, id := range []string{code, acct} {
			if err := r.fields.DeleteField(r.bctx, r.tenant, DeleteFieldInput{FieldID: id}); err != nil {
				t.Fatalf("DeleteField: %v", err)
			}
		}
	})
	if r.fieldLive(t, code) || r.fieldLive(t, acct) || r.pivotRows(t) != 0 || r.liveRels(t) != 0 {
		t.Fatal("setup: the published delete did not remove the fields, the pivot and the relationship")
	}

	got, plan, err := r.rollbackTo(t, 1, res.VersionNo)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(plan.SchemaRestores) != 2 {
		t.Errorf("schemaRestores = %+v, want the 2 deleted fields", plan.SchemaRestores)
	}
	if got.Kind != "rollback" || got.VersionNo != res.VersionNo+1 {
		t.Errorf("result = %+v, want a rollback version %d", got, res.VersionNo+1)
	}
	if !r.fieldLive(t, code) || !r.fieldLive(t, acct) {
		t.Fatal("a deleted field did not come back")
	}
	if r.liveRels(t) != 1 {
		t.Errorf("live relationships = %d, want the relation field's row back", r.liveRels(t))
	}
	var back string
	if err := r.sdb.Get(&back, `SELECT name FROM sys_relationships WHERE tenant_id = ? AND child_entity_name = 'hk_deals' AND deleted_at IS NULL`, r.tenant); err != nil || back != relName {
		t.Errorf("relationship name = %q (%v), want the original %q (tombstone removed)", back, err, relName)
	}
	if n := r.pivotRows(t); n != 3 {
		t.Errorf("pivot rows after = %d, want 3 rebuilt", n)
	}
	if n := r.count(t, "SELECT COUNT(*) FROM `obj_hk_deals` WHERE tenant_id = ? AND deleted_at IS NULL AND `code` IS NOT NULL", r.tenant); n != 3 {
		t.Errorf("rows with a code value = %d, want 3: the values must still be there", n)
	}
	// the restored unique field is enforced again
	var existing string
	if err := r.sdb.Get(&existing, "SELECT `code` FROM `obj_hk_deals` WHERE tenant_id = ? AND deleted_at IS NULL LIMIT 1", r.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := r.rec.Create(context.Background(), engAdmin(hkUser), r.tenant, hkUser, "hk_deals", map[string]any{"name": "dup", "code": existing, "stage": "Proposal"}); err == nil {
		t.Error("a record with an existing code was accepted: the unique pivot was not rebuilt")
	}
	// the customer sees the field again
	flds, err := r.drafts4Fields.ListFields(r.cctx, r.tenant, entityIDOf(t, r.dpEnv, r.tenant))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range flds {
		seen[f.Name] = true
	}
	if !seen["code"] || !seen["acct"] {
		t.Errorf("customer field list lacks a restored field: %v", seen)
	}
}

// T134a: a unique value two live records now share refuses the rollback and changes nothing (FR-067).
func TestRollbackRestoreFieldUniqueConflict_Integration(t *testing.T) {
	r := rrSetup(t)
	code := r.fieldID(t, "code")
	res := r.publishDeletes(t, func() {
		if err := r.fields.DeleteField(r.bctx, r.tenant, DeleteFieldInput{FieldID: code}); err != nil {
			t.Fatal(err)
		}
	})
	// a record created while the field was gone carries a value an older record has
	r.exec2(t, "INSERT INTO `obj_hk_deals` (id, tenant_id, created_by, data) SELECT UUID(), tenant_id, created_by, data FROM `obj_hk_deals` WHERE tenant_id = ? AND deleted_at IS NULL LIMIT 1", r.tenant)
	versions := r.versionCount(t, r.tenant)

	_, _, err := r.rollbackTo(t, 1, res.VersionNo)
	wantRollbackCode(t, err, "ROLLBACK_UNIQUE_CONFLICT")
	if r.fieldLive(t, code) || r.pivotRows(t) != 0 || r.versionCount(t, r.tenant) != versions {
		t.Errorf("a refused rollback changed something: field live=%v pivot=%d versions=%d (was %d)",
			r.fieldLive(t, code), r.pivotRows(t), r.versionCount(t, r.tenant), versions)
	}
}

func (r *rrEnv) exec2(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := r.sdb.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// T134 + T135: a deleted table comes back with EXACTLY the records its delete trashed: a record the
// staff binned earlier stays binned, another tenant sharing obj_hk_deals is untouched, relationships and
// the unique pivot of the whole table are rebuilt.
func TestRollbackRestoresDeletedTable_Integration(t *testing.T) {
	r := rrSetup(t)
	other := r.newTenant(t)
	r.seedRows(t, other, 2)
	// the staff binned one deal long before the table was deleted
	r.exec2(t, "UPDATE `obj_hk_deals` SET deleted_at = '2026-01-02 03:04:05' WHERE tenant_id = ? AND deleted_at IS NULL LIMIT 1", r.tenant)
	if r.liveRows(t, r.tenant) != 2 {
		t.Fatalf("setup: live rows = %d, want 2", r.liveRows(t, r.tenant))
	}
	res := r.publishDeletes(t, func() {
		if err := r.entities.SoftDeleteEntity(r.bctx, r.tenant, "hk_deals"); err != nil {
			t.Fatal(err)
		}
	})
	if r.entityLive(t, r.tenant) || r.liveRows(t, r.tenant) != 0 || r.pivotRows(t) != 0 || r.liveRels(t) != 0 {
		t.Fatal("setup: the published delete left the table, its rows, pivot or relationships alive")
	}
	otherBefore := r.liveRows(t, other)

	_, plan, err := r.rollbackTo(t, 1, res.VersionNo)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(plan.SchemaRestores) != 1 || plan.SchemaRestores[0].SweptCount == nil || *plan.SchemaRestores[0].SweptCount != 2 {
		t.Errorf("schemaRestores = %+v, want 1 table with sweptCount 2", plan.SchemaRestores)
	}
	if !r.entityLive(t, r.tenant) {
		t.Fatal("the table did not come back under its original name")
	}
	if n := r.liveRows(t, r.tenant); n != 2 {
		t.Errorf("live rows after = %d, want exactly the 2 the delete trashed", n)
	}
	if n := r.count(t, "SELECT COUNT(*) FROM `obj_hk_deals` WHERE tenant_id = ? AND deleted_at = '2026-01-02 03:04:05'", r.tenant); n != 1 {
		t.Errorf("rows still in the bin from before = %d, want the 1 the staff binned", n)
	}
	if r.liveRows(t, other) != otherBefore {
		t.Errorf("another tenant's live rows changed: %d -> %d", otherBefore, r.liveRows(t, other))
	}
	if r.liveRels(t) != 1 {
		t.Errorf("live relationships = %d, want the relation field's row back", r.liveRels(t))
	}
	if n := r.pivotRows(t); n != 2 {
		t.Errorf("pivot rows = %d, want 2 (one per live record)", n)
	}
	// reading through the engine works again, the cache was dropped
	if _, err := r.meta.GetEntityByName(context.Background(), r.tenant, "hk_deals"); err != nil {
		t.Errorf("entity lookup after the rollback: %v", err)
	}
	var meta metadata.EntityDefinition
	if err := r.gdb.Where("tenant_id = ? AND name = ?", r.tenant, "hk_deals").First(&meta).Error; err != nil {
		t.Errorf("registry row: %v", err)
	}
}

// FR-066: a record the staff binned in the very second a publish picked as its sweep instant would be
// brought back by the rollback with the swept ones; the instant moves to a second nobody used.
func TestFreeSweepInstant_SkipsASecondTheBinAlreadyUses_Integration(t *testing.T) {
	r := rrSetup(t)
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	got, err := freeSweepInstant(context.Background(), r.sdb, r.tenant, "hk_deals", at)
	if err != nil || !got.Equal(at) {
		t.Fatalf("empty bin: got %v err %v, want %v", got, err, at)
	}
	r.exec2(t, "UPDATE `obj_hk_deals` SET deleted_at = ? WHERE tenant_id = ? AND deleted_at IS NULL LIMIT 1", at, r.tenant)
	got, err = freeSweepInstant(context.Background(), r.sdb, r.tenant, "hk_deals", at)
	if err != nil || !got.Equal(at.Add(time.Second)) {
		t.Fatalf("a binned record at %v: got %v err %v, want the next second", at, got, err)
	}
	other := r.newTenant(t)
	if got, _ = freeSweepInstant(context.Background(), r.sdb, other, "hk_deals", at); !got.Equal(at) {
		t.Errorf("another tenant's bin must not move the instant: %v", got)
	}
}

// T135: a live table that took the name meanwhile refuses the rollback, and so does a unique value two
// live records now share; in both cases nothing is half-restored.
func TestRollbackRestoreTableRefusals_Integration(t *testing.T) {
	t.Run("name taken", func(t *testing.T) {
		r := rrSetup(t)
		res := r.publishDeletes(t, func() {
			if err := r.entities.SoftDeleteEntity(r.bctx, r.tenant, "hk_deals"); err != nil {
				t.Fatal(err)
			}
		})
		r.createDeals(t, r.tenant) // a new, live hk_deals
		versions := r.versionCount(t, r.tenant)
		_, _, err := r.rollbackTo(t, 1, res.VersionNo)
		wantRollbackCode(t, err, "ROLLBACK_NAME_TAKEN")
		if r.liveRows(t, r.tenant) != 0 || r.versionCount(t, r.tenant) != versions {
			t.Errorf("a refused rollback changed something: live rows=%d versions=%d (was %d)", r.liveRows(t, r.tenant), r.versionCount(t, r.tenant), versions)
		}
	})
	t.Run("unique conflict", func(t *testing.T) {
		r := rrSetup(t)
		res := r.publishDeletes(t, func() {
			if err := r.entities.SoftDeleteEntity(r.bctx, r.tenant, "hk_deals"); err != nil {
				t.Fatal(err)
			}
		})
		// a live record that appeared meanwhile has the code of a trashed one
		r.exec2(t, "INSERT INTO `obj_hk_deals` (id, tenant_id, created_by, data, deleted_at) SELECT UUID(), tenant_id, created_by, data, NULL FROM `obj_hk_deals` WHERE tenant_id = ? AND deleted_at IS NOT NULL LIMIT 1", r.tenant)
		live := r.liveRows(t, r.tenant)
		versions := r.versionCount(t, r.tenant)
		_, _, err := r.rollbackTo(t, 1, res.VersionNo)
		wantRollbackCode(t, err, "ROLLBACK_UNIQUE_CONFLICT")
		if r.entityLive(t, r.tenant) || r.liveRows(t, r.tenant) != live || r.versionCount(t, r.tenant) != versions {
			t.Errorf("a refused rollback changed something: entity live=%v live rows=%d (was %d) versions=%d (was %d)",
				r.entityLive(t, r.tenant), r.liveRows(t, r.tenant), live, r.versionCount(t, r.tenant), versions)
		}
	})
}

// A field hidden from a role stays hidden after a rollback brings the deleted field back: sys_field_acl is
// default-allow, so a restore that forgot the dropped row would expose the field (fail-open).
func TestRollbackRestoresFieldACL_Integration(t *testing.T) {
	r := rrSetup(t)
	code := r.fieldID(t, "code")
	role := uuid.NewString()
	if _, err := r.sdb.Exec(`INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, 'sales', 'Sales')`, role, r.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := r.sdb.Exec(`INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit) VALUES (?, ?, ?, 'hk_deals', 'code', 0, 0)`,
		uuid.NewString(), r.tenant, role); err != nil {
		t.Fatal(err)
	}
	const q = `SELECT COUNT(*) FROM sys_field_acl WHERE tenant_id = ? AND role_id = ? AND entity_name = 'hk_deals' AND field_name = 'code' AND can_view = 0 AND can_edit = 0`
	res := r.publishDeletes(t, func() {
		if err := r.fields.DeleteField(r.bctx, r.tenant, DeleteFieldInput{FieldID: code}); err != nil {
			t.Fatalf("DeleteField: %v", err)
		}
	})
	if n := r.count(t, q, r.tenant, role); n != 0 {
		t.Fatalf("setup: the delete left %d restriction rows", n)
	}
	if _, _, err := r.rollbackTo(t, 1, res.VersionNo); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if n := r.count(t, q, r.tenant, role); n != 1 {
		t.Errorf("restriction rows after rollback = %d, want 1: the field would be visible to the role again", n)
	}
}
