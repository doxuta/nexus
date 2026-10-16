//go:build integration

package services

// draft_schema_delete_integration_test.go — specs/063-draft-publish S7-9 (T126, T128, T128a, T130)
// against a real MySQL: a builder's delete of a field / table on a delivered workspace is a draft that
// leaves the customer's app and data alone; Publish commits the documents first, records the sweep
// instant BEFORE sweeping, runs the existing delete services, and a failed delete is retried with the
// SAME instant and without a second effect.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// flakyEntityDeleter wraps the real EntityService and can be told to fail before it runs.
type flakyEntityDeleter struct {
	real schemaEntityDeleter
	fail atomic.Bool
	// failAfter runs the real delete, then reports a failure: the sweep (and here the tombstone) has
	// landed but the caller is told it did not - the shape of a lock timeout between the two steps.
	failAfter atomic.Bool
	calls     atomic.Int32
	// onCall runs at the start of every call (the test peeks at the DB there).
	onCall func()
}

func (f *flakyEntityDeleter) SoftDeleteEntity(ctx context.Context, tenantID, name string) error {
	f.calls.Add(1)
	if f.onCall != nil {
		f.onCall()
	}
	if f.fail.Load() {
		return errors.New("injected failure before the sweep")
	}
	err := f.real.SoftDeleteEntity(ctx, tenantID, name)
	if err == nil && f.failAfter.Load() {
		return errors.New("injected failure after the sweep")
	}
	return err
}

type s79Env struct {
	dpEnv
	tenant, builder string
	bctx, cctx      context.Context
	fields          *FieldService
	entities        *EntityService
	drafts4Fields   *DraftFieldRepo
	flaky           *flakyEntityDeleter
}

func s79Setup(t *testing.T) *s79Env {
	t.Helper()
	e := dpSetupWith(t, NewWorkflowDocAdapter(), NewFieldDocAdapter(), NewEntityDocAdapter())
	s := &s79Env{dpEnv: e}
	s.tenant, s.builder = e.newTenant(t), uuid.NewString()
	s.bctx, s.cctx = builderCtxOf(s.tenant, s.builder), legacyCtxOf(s.tenant)
	e.deliver(t, s.tenant, s.builder)
	s.drafts4Fields = NewDraftFieldRepo(e.meta, e.drafts, e.gate)
	s.fields = NewFieldService(e.dyn, e.meta).WithDraftFields(s.drafts4Fields)
	s.entities = NewEntityService(e.dyn, e.meta).WithDraftFields(s.drafts4Fields)
	s.flaky = &flakyEntityDeleter{real: s.entities}
	e.pub.WithSchemaOps(s.fields, s.flaky)
	return s
}

func (s *s79Env) seedRows(t *testing.T, tenant string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		s.seed(t, tenant, map[string]any{"name": "d", "code": "C-" + uuid.NewString()[:8], "stage": "Proposal"})
	}
}

func (s *s79Env) liveRows(t *testing.T, tenant string) int {
	return s.count(t, "SELECT COUNT(*) FROM `obj_hk_deals` WHERE tenant_id = ? AND deleted_at IS NULL", tenant)
}

func (s *s79Env) fieldLive(t *testing.T, id string) bool {
	return s.count(t, `SELECT COUNT(*) FROM sys_fields WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, s.tenant, id) == 1
}

func (s *s79Env) entityLive(t *testing.T, tenant string) bool {
	return s.count(t, `SELECT COUNT(*) FROM sys_entities WHERE tenant_id = ? AND name = 'hk_deals' AND deleted_at IS NULL`, tenant) == 1
}

type schemaDocRow struct {
	ID     string  `db:"id"`
	Status string  `db:"apply_status"`
	Err    *string `db:"apply_error"`
	Meta   *string `db:"meta"`
}

func (s *s79Env) schemaDoc(t *testing.T, versionNo uint, docType string) schemaDocRow {
	t.Helper()
	var r schemaDocRow
	err := s.sdb.Get(&r, `SELECT d.id, d.apply_status, d.apply_error, CAST(d.meta AS CHAR) AS meta FROM sys_meta_version_docs d
		JOIN sys_meta_versions v ON v.id = d.version_id AND v.tenant_id = d.tenant_id
		WHERE d.tenant_id = ? AND v.version_no = ? AND d.doc_type = ? AND d.op = 'delete'`, s.tenant, versionNo, docType)
	if err != nil {
		t.Fatalf("schema doc %s of v%d: %v", docType, versionNo, err)
	}
	return r
}

func (s *s79Env) schemaStatus(t *testing.T, versionNo uint) string {
	var st string
	if err := s.sdb.Get(&st, `SELECT schema_status FROM sys_meta_versions WHERE tenant_id = ? AND version_no = ?`, s.tenant, versionNo); err != nil {
		t.Fatal(err)
	}
	return st
}

func metaOf(t *testing.T, raw *string) map[string]any {
	t.Helper()
	m := map[string]any{}
	if raw == nil {
		return m
	}
	if err := json.Unmarshal([]byte(*raw), &m); err != nil {
		t.Fatalf("meta %q: %v", *raw, err)
	}
	return m
}

// T126: deleting a field / table is a draft - nothing live moves, the builder's lists say "deleted",
// and Hoàn tác (discard) makes the draft disappear.
func TestDeferredDelete_DraftOnly_Integration(t *testing.T) {
	s := s79Setup(t)
	s.seedRows(t, s.tenant, 3)
	entID := s78EntityID(t, &s78Env{dpEnv: s.dpEnv, tenant: s.tenant})
	flds, _ := s.meta.ListFields(s.bctx, s.tenant, entID)
	var stage metadata.FieldDefinition
	for _, f := range flds {
		if f.Name == "close_date" {
			stage = f
		}
	}
	if err := s.fields.DeleteField(s.bctx, s.tenant, DeleteFieldInput{FieldID: stage.ID}); err != nil {
		t.Fatalf("DeleteField (builder): %v", err)
	}
	if err := s.entities.SoftDeleteEntity(s.bctx, s.tenant, "hk_deals"); err != nil {
		t.Fatalf("SoftDeleteEntity (builder): %v", err)
	}
	if !s.fieldLive(t, stage.ID) || !s.entityLive(t, s.tenant) || s.liveRows(t, s.tenant) != 3 {
		t.Fatal("a builder delete changed the live field / table / rows: the customer must keep using them until Publish")
	}
	if n := s.draftCount(t, s.tenant); n != 2 {
		t.Fatalf("drafts = %d, want 2 (field delete + entity delete)", n)
	}
	items, _, err := s.pub.Changes(s.bctx, s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]string{}
	for _, it := range items {
		ops[it.DocType] = it.Op
	}
	if ops["field"] != "delete" || ops["entity"] != "delete" {
		t.Fatalf("change list ops = %v, want field:delete and entity:delete", ops)
	}
	// the builder's lists carry the mark, the customer's do not
	got, _ := s.drafts4Fields.ListFields(s.bctx, s.tenant, entID)
	marked := 0
	for _, f := range got {
		if f.DraftState == draftStateDeleted {
			marked++
		}
	}
	if marked != 1 {
		t.Errorf("builder field list marks %d fields deleted, want 1", marked)
	}
	ents, _ := s.meta.ListEntities(s.tenant)
	if b, _ := s.drafts4Fields.MarkDeletedEntities(s.bctx, s.tenant, ents); !hasDeleted(b) {
		t.Error("builder entity list does not mark the table deleted")
	}
	if c, _ := s.drafts4Fields.MarkDeletedEntities(s.cctx, s.tenant, ents); hasDeleted(c) {
		t.Error("customer entity list carries the builder's mark")
	}
	// Hoàn tác: discarding both drafts leaves nothing pending and nothing marked
	for _, it := range items {
		if err := s.pub.Discard(s.bctx, s.tenant, it.ID); err != nil {
			t.Fatalf("Discard: %v", err)
		}
	}
	if s.draftCount(t, s.tenant) != 0 {
		t.Error("drafts remain after Hoàn tác")
	}
	if b, _ := s.drafts4Fields.MarkDeletedEntities(s.bctx, s.tenant, ents); hasDeleted(b) {
		t.Error("table still marked after Hoàn tác")
	}
	// a customer-admin (non-builder) delete is NOT a draft: unchanged legacy behaviour
	if err := s.fields.DeleteField(s.cctx, s.tenant, DeleteFieldInput{FieldID: stage.ID}); err != nil {
		t.Fatalf("DeleteField (customer admin): %v", err)
	}
	if s.fieldLive(t, stage.ID) || s.draftCount(t, s.tenant) != 0 {
		t.Error("a customer admin's field delete did not apply live")
	}
}

func hasDeleted(es []metadata.EntityDefinition) bool {
	for _, e := range es {
		if e.DraftState == draftStateDeleted {
			return true
		}
	}
	return false
}

// T128: Publish with deletes. Documents commit first; sweptAt is in the version BEFORE the sweep; a
// failure leaves schema_status failed; retry reuses sweptAt, finishes, and does not act twice.
func TestPublishWithDeletes_RetryReusesTheRecordedInstant_Integration(t *testing.T) {
	s := s79Setup(t)
	other := s.newTenant(t) // shares the physical obj_hk_deals table
	s.seedRows(t, s.tenant, 4)
	s.seedRows(t, other, 2)
	entID := s78EntityID(t, &s78Env{dpEnv: s.dpEnv, tenant: s.tenant})
	flds, _ := s.meta.ListFields(s.bctx, s.tenant, entID)
	var closeDate metadata.FieldDefinition
	for _, f := range flds {
		if f.Name == "close_date" {
			closeDate = f
		}
	}
	if err := s.fields.DeleteField(s.bctx, s.tenant, DeleteFieldInput{FieldID: closeDate.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.entities.SoftDeleteEntity(s.bctx, s.tenant, "hk_deals"); err != nil {
		t.Fatal(err)
	}

	// the table survives the whole thing: record how it looks now
	cols := func() int {
		return s.count(t, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'obj_hk_deals'`)
	}
	colsBefore := cols()

	// first attempt: the table delete fails before it sweeps. Peek at the DB at that moment: the
	// version must already carry the sweep instant (written before any sweep).
	var seenAtCall map[string]any
	s.flaky.fail.Store(true)
	s.flaky.onCall = func() {
		var raw *string
		if err := s.sdb.Get(&raw, `SELECT CAST(d.meta AS CHAR) FROM sys_meta_version_docs d WHERE d.tenant_id = ? AND d.doc_type = 'entity' AND d.op = 'delete'`, s.tenant); err == nil {
			seenAtCall = metaOf(t, raw)
		}
	}
	res, err := s.publishWithCurrentHash(s.bctx, s.tenant, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.SchemaStatus != "failed" || len(res.SchemaOps) != 2 {
		t.Fatalf("publish result schemaStatus=%q ops=%+v, want failed with 2 ops", res.SchemaStatus, res.SchemaOps)
	}
	if seenAtCall["sweptAt"] == nil || seenAtCall["entityDef"] == nil {
		t.Fatalf("meta at the moment of the table delete = %v, want sweptAt and entityDef recorded before the sweep", seenAtCall)
	}
	if s.fieldLive(t, closeDate.ID) {
		t.Error("the field delete (first in order) did not run")
	}
	if !s.entityLive(t, s.tenant) || s.liveRows(t, s.tenant) != 4 {
		t.Error("the failed table delete changed the live table or rows")
	}
	if got := s.schemaStatus(t, res.VersionNo); got != "failed" {
		t.Errorf("schema_status = %q, want failed", got)
	}
	if st, serr := s.pub.State(s.bctx, s.tenant); serr != nil || st.SchemaRetry == nil || st.SchemaRetry.VersionNo != res.VersionNo ||
		len(st.SchemaRetry.Ops) != 1 || st.SchemaRetry.Ops[0].DocType != "entity" || st.SchemaRetry.Ops[0].Error == "" {
		t.Errorf("state.schemaRetry = %+v (%v), want the failed table delete of v%d with its reason", st.SchemaRetry, serr, res.VersionNo)
	}
	ent := s.schemaDoc(t, res.VersionNo, "entity")
	if ent.Status != "failed" || ent.Err == nil || !strings.Contains(*ent.Err, "injected") {
		t.Errorf("entity doc = %+v, want failed with the reason", ent)
	}
	fld := s.schemaDoc(t, res.VersionNo, "field")
	if fld.Status != "applied" {
		t.Errorf("field doc status = %q, want applied", fld.Status)
	}
	if fm := metaOf(t, fld.Meta); fm["fieldDef"] == nil || fm["sweptAt"] == nil {
		t.Errorf("field doc meta = %v, want fieldDef and sweptAt", fm)
	}
	// the documents committed first: the draft rows are gone and the version holds both deletes
	if s.draftCount(t, s.tenant) != 0 {
		t.Error("drafts remain after Publish")
	}
	sweptAt := metaOf(t, ent.Meta)["sweptAt"].(string)

	// retry with the table delete working again, after the clock has moved on
	time.Sleep(1200 * time.Millisecond)
	s.flaky.fail.Store(false)
	s.flaky.onCall = nil
	before := s.flaky.calls.Load()
	rr, err := s.pub.RetrySchema(s.bctx, s.tenant, s.versionID(t, res.VersionNo))
	if err != nil {
		t.Fatalf("RetrySchema: %v", err)
	}
	if rr.SchemaStatus != "done" || s.schemaStatus(t, res.VersionNo) != "done" {
		t.Fatalf("after retry: result=%+v stored=%s, want done", rr, s.schemaStatus(t, res.VersionNo))
	}
	if st, serr := s.pub.State(s.bctx, s.tenant); serr != nil || st.SchemaRetry != nil {
		t.Errorf("state.schemaRetry after a finished retry = %+v (%v), want null", st.SchemaRetry, serr)
	}
	if s.flaky.calls.Load() != before+1 {
		t.Errorf("table delete ran %d times in the retry, want exactly 1 (the field was not repeated)", s.flaky.calls.Load()-before)
	}
	if s.entityLive(t, s.tenant) {
		t.Error("the table is still live after the retry")
	}
	// every row of this tenant carries the RECORDED instant, not the retry's
	at, err := time.Parse(time.RFC3339, sweptAt)
	if err != nil {
		t.Fatal(err)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM `obj_hk_deals` WHERE tenant_id = ? AND deleted_at = ?", s.tenant, at.UTC()); n != 4 {
		t.Errorf("rows with deleted_at = recorded sweptAt (%s) = %d, want 4", sweptAt, n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM `obj_hk_deals` WHERE tenant_id = ? AND deleted_at IS NULL", s.tenant); n != 0 {
		t.Errorf("%d live rows remain", n)
	}
	if s.liveRows(t, other) != 2 {
		t.Error("another tenant's rows in the shared table were touched")
	}
	if got := metaOf(t, s.schemaDoc(t, res.VersionNo, "entity").Meta); got["sweptCount"] != float64(4) {
		t.Errorf("entity doc sweptCount = %v, want 4", got["sweptCount"])
	}
	if got := metaOf(t, s.schemaDoc(t, res.VersionNo, "entity").Meta); got["sweptAt"] != sweptAt {
		t.Errorf("sweptAt changed on retry: %v -> %v", sweptAt, got["sweptAt"])
	}
	if colsBefore != cols() || s.count(t, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'obj_hk_deals'`) != 1 {
		t.Error("the physical table or its columns changed: Publish must never run DDL (FR-068)")
	}

	// a second retry has nothing to do and acts on nothing
	calls := s.flaky.calls.Load()
	if _, err := s.pub.RetrySchema(s.bctx, s.tenant, s.versionID(t, res.VersionNo)); err == nil {
		t.Fatal("retry of a done version succeeded")
	} else {
		wantCode(t, err, "SCHEMA_RETRY_NOT_NEEDED")
	}
	if s.flaky.calls.Load() != calls {
		t.Error("a refused retry still ran a delete")
	}
	// the version's audit-visible summary stays free of bodies: no new publish needed to see it
	if s.versionCount(t, s.tenant) != 2 {
		t.Errorf("versions = %d, want 2 (baseline + publish): a retry writes no version", s.versionCount(t, s.tenant))
	}
}

func (s *s79Env) versionID(t *testing.T, no uint) string {
	t.Helper()
	var id string
	if err := s.sdb.Get(&id, `SELECT id FROM sys_meta_versions WHERE tenant_id = ? AND version_no = ?`, s.tenant, no); err != nil {
		t.Fatal(err)
	}
	return id
}

// T128 happy path: a publish whose deletes all work finishes in one call, status done.
func TestPublishWithDeletes_HappyPath_Integration(t *testing.T) {
	s := s79Setup(t)
	s.seedRows(t, s.tenant, 3)
	if err := s.entities.SoftDeleteEntity(s.bctx, s.tenant, "hk_deals"); err != nil {
		t.Fatal(err)
	}
	res, err := s.publishWithCurrentHash(s.bctx, s.tenant, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.SchemaStatus != "done" || len(res.SchemaOps) != 1 || res.SchemaOps[0].ApplyStatus != "applied" {
		t.Fatalf("result = %+v, want done with one applied op", res)
	}
	if s.entityLive(t, s.tenant) || s.liveRows(t, s.tenant) != 0 {
		t.Error("table or rows still live after a successful publish")
	}
	m := metaOf(t, s.schemaDoc(t, res.VersionNo, "entity").Meta)
	if m["sweptCount"] != float64(3) || m["sweptAt"] == nil {
		t.Errorf("meta = %v, want sweptCount 3 and sweptAt", m)
	}
}

// T128a (FR-081): a version id of another tenant is VERSION_NOT_FOUND and runs nothing in either tenant.
func TestRetrySchema_CrossTenantIsNotFound_Integration(t *testing.T) {
	a := s79Setup(t)
	a.seedRows(t, a.tenant, 2)
	if err := a.entities.SoftDeleteEntity(a.bctx, a.tenant, "hk_deals"); err != nil {
		t.Fatal(err)
	}
	a.flaky.fail.Store(true)
	res, err := a.publishWithCurrentHash(a.bctx, a.tenant, false)
	if err != nil || res.SchemaStatus != "failed" {
		t.Fatalf("setup publish = %+v, %v", res, err)
	}
	verA := a.versionID(t, res.VersionNo)

	// tenant B: delivered, builder of B
	b := a.newTenant(t)
	bBuilder := uuid.NewString()
	a.deliver(t, b, bBuilder)
	a.flaky.fail.Store(false)
	calls := a.flaky.calls.Load()
	_, err = a.pub.RetrySchema(builderCtxOf(b, bBuilder), b, verA)
	if err == nil {
		t.Fatal("tenant B retried tenant A's version")
	}
	wantCode(t, err, "VERSION_NOT_FOUND")
	if _, err2 := a.pub.RetrySchema(builderCtxOf(b, bBuilder), b, uuid.NewString()); err2 == nil {
		t.Fatal("unknown version id succeeded")
	} else if err.Error() != err2.Error() {
		t.Errorf("another tenant's id (%q) is distinguishable from an unknown one (%q)", err, err2)
	}
	if a.flaky.calls.Load() != calls {
		t.Error("a cross-tenant retry ran a delete")
	}
	if a.schemaStatus(t, res.VersionNo) != "failed" {
		t.Error("a cross-tenant retry changed tenant A's schema_status")
	}
	if a.count(t, "SELECT COUNT(*) FROM `obj_hk_deals` WHERE tenant_id = ? AND deleted_at IS NULL", a.tenant) != 2 {
		t.Error("tenant A's rows moved")
	}
	// a non-builder cannot retry at all
	if _, err := a.pub.RetrySchema(legacyCtxOf(a.tenant), a.tenant, verA); err == nil {
		t.Error("a non-builder retried")
	} else {
		wantCode(t, err, "FORBIDDEN")
	}
	_ = metaversion.StatusFailed
}

// T130: a pending delete warns about the reports that use it - a warning, never a block.
func TestChanges_WarnsAboutReportsUsingADeletedField_Integration(t *testing.T) {
	s := s79Setup(t)
	if _, err := s.sdb.Exec(`INSERT INTO sys_reports (id, tenant_id, name, entity_name, type, config) VALUES (?, ?, 'Pipeline', 'hk_deals', 'tabular', ?)`,
		uuid.NewString(), s.tenant, `{"columns":["name","stage"]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sdb.Exec(`INSERT INTO sys_reports (id, tenant_id, name, entity_name, type, config) VALUES (?, ?, 'Names only', 'hk_deals', 'tabular', ?)`,
		uuid.NewString(), s.tenant, `{"columns":["name"]}`); err != nil {
		t.Fatal(err)
	}
	entID := s78EntityID(t, &s78Env{dpEnv: s.dpEnv, tenant: s.tenant})
	flds, _ := s.meta.ListFields(s.bctx, s.tenant, entID)
	for _, f := range flds {
		if f.Name == "stage" {
			if err := s.fields.DeleteField(s.bctx, s.tenant, DeleteFieldInput{FieldID: f.ID}); err != nil {
				t.Fatal(err)
			}
		}
	}
	items, _, err := s.pub.Changes(s.bctx, s.tenant)
	if err != nil || len(items) != 1 {
		t.Fatalf("Changes = %+v, %v", items, err)
	}
	if w := items[0].Warnings; len(w) != 1 || !strings.Contains(w[0], `"Pipeline"`) || !strings.Contains(w[0], `"stage"`) {
		t.Fatalf("warnings = %v, want exactly the Pipeline report naming the field", w)
	}
	if _, err := s.publishWithCurrentHash(s.bctx, s.tenant, false); err != nil {
		t.Fatalf("a warning blocked the publish: %v", err)
	}
}

// A table delete that fails AFTER its sweep committed must still record how many rows it moved: the retry
// finds nothing left to sweep and would otherwise write 0 (FR-066, US11 #4).
func TestPublishWithDeletes_SweptCountSurvivesAFailureAfterTheSweep_Integration(t *testing.T) {
	s := s79Setup(t)
	s.seedRows(t, s.tenant, 4)
	if err := s.entities.SoftDeleteEntity(s.bctx, s.tenant, "hk_deals"); err != nil {
		t.Fatal(err)
	}
	s.flaky.failAfter.Store(true)
	res, err := s.publishWithCurrentHash(s.bctx, s.tenant, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.SchemaStatus != "failed" {
		t.Fatalf("schemaStatus = %q, want failed", res.SchemaStatus)
	}
	if got := metaOf(t, s.schemaDoc(t, res.VersionNo, "entity").Meta)["sweptCount"]; got != float64(4) {
		t.Fatalf("sweptCount after the failed attempt = %v, want 4 (rows were already moved)", got)
	}
	s.flaky.failAfter.Store(false)
	rr, err := s.pub.RetrySchema(s.bctx, s.tenant, s.versionID(t, res.VersionNo))
	if err != nil {
		t.Fatalf("RetrySchema: %v", err)
	}
	if rr.SchemaStatus != "done" {
		t.Fatalf("retry schemaStatus = %q, want done", rr.SchemaStatus)
	}
	if got := metaOf(t, s.schemaDoc(t, res.VersionNo, "entity").Meta)["sweptCount"]; got != float64(4) {
		t.Errorf("sweptCount after the retry = %v, want 4", got)
	}
}
