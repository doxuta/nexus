package services

// Unit tests for DOCTOR-OPS (../specs/008-nexus-doctor/spec.md §4 lát 3): C4 zombie-workflow
// detect → confirm-gate → disable (+ workflow-cache invalidation), the report-only contract
// (A3/B5/C8 scan flag + whole-request fix refusal), and the flush-caches action. They ride
// the same fakeDoctorWorld fixture as the CORE/SCHEMA tests (doctor_service_test.go).

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nexus/platform/internal/doctor"
	"nexus/platform/internal/workflow"
)

// countingWorkflowLoader counts DB hits behind a workflow.CachedLoader (invalidation proof).
// The ACL twin (countingACLLoader) is shared from acl_cache_test.go.
type countingWorkflowLoader struct{ calls int64 }

func (c *countingWorkflowLoader) ListActive(context.Context, string, string, workflow.Trigger) ([]workflow.Workflow, error) {
	atomic.AddInt64(&c.calls, 1)
	return nil, nil
}

func TestDoctor_C4_ZombieWorkflows_ConfirmGateThenDisable(t *testing.T) {
	w, _, svc := newDoctorFixture()
	w.zombies["wf-1"] = doctor.ZombieWorkflowRow{ID: "wf-1", Name: "auto-assign", EntityName: "gone_entity", Trigger: "after_create"}

	// Wire a real workflow cache over a counting inner so the fix's invalidation is provable.
	inner := &countingWorkflowLoader{}
	wfCache := workflow.NewCachedLoader(inner, time.Hour) // long TTL: only invalidation can force a reload
	svc.WithOpsCaches(nil, wfCache)
	if _, err := wfCache.ListActive(context.Background(), "t-a", "gone_entity", workflow.TriggerAfterCreate); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if n := atomic.LoadInt64(&inner.calls); n != 1 {
		t.Fatalf("loads = %d; want 1 after warm", n)
	}

	got := scanByID(t, svc, "t-a")[DoctorCheckZombieWorkflows]
	if got.Count != 1 || got.Items[0].Key != "wf-1" || got.AutoFixSafe || got.ReportOnly {
		t.Fatalf("detect = %+v; want 1 finding, autoFixSafe=false, reportOnly=false", got)
	}
	// The confirm copy must explain the resurrection risk + disable-not-delete.
	if !strings.Contains(got.Items[0].Detail, "HỒI SINH") || !strings.Contains(got.Items[0].Detail, "KHÔNG xóa") {
		t.Fatalf("detail phải nêu rủi ro hồi sinh + disable-not-delete: %s", got.Items[0].Detail)
	}

	// Needs-confirm contract: refuse the WHOLE request without confirm=true, zero mutation.
	_, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckZombieWorkflows}, false)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "DOCTOR_CONFIRM_REQUIRED" {
		t.Fatalf("err = %v; want ValidationError DOCTOR_CONFIRM_REQUIRED", err)
	}
	if w.wfDisabled["wf-1"] {
		t.Fatalf("workflow bị disable dù request bị reject — mutation trước validation")
	}

	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckZombieWorkflows}, true)
	if err != nil || results[0].Fixed != 1 {
		t.Fatalf("fix confirm=true = %+v (err %v); want 1 fixed", results[0], err)
	}
	if !w.wfDisabled["wf-1"] {
		t.Fatalf("workflow chưa được disable sau fix")
	}
	// Disable = is_active flip, NEVER delete — the row must still exist.
	if _, ok := w.zombies["wf-1"]; !ok {
		t.Fatalf("row workflow biến mất — C4 phải disable, không delete")
	}
	// Publish-after-commit: the (tenant, entity) cache entry was invalidated by the fix.
	if _, err := wfCache.ListActive(context.Background(), "t-a", "gone_entity", workflow.TriggerAfterCreate); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := atomic.LoadInt64(&inner.calls); n != 2 {
		t.Fatalf("loads = %d; want 2 — C4 fix phải invalidate workflow cache", n)
	}
	if got := scanByID(t, svc, "t-a")[DoctorCheckZombieWorkflows]; got.Count != 0 {
		t.Fatalf("re-scan = %+v; want clean", got)
	}
	// Idempotency: a second confirm run finds nothing to do.
	results, err = svc.Fix(context.Background(), "t-a", []string{DoctorCheckZombieWorkflows}, true)
	if err != nil || results[0].Fixed != 0 {
		t.Fatalf("second fix = %+v (err %v); want 0 fixed", results[0], err)
	}
}

func TestDoctor_C4_StaleFindingSkipped(t *testing.T) {
	w, au, svc := newDoctorFixture()
	w.zombies["wf-1"] = doctor.ZombieWorkflowRow{ID: "wf-1", Name: "auto", EntityName: "gone", Trigger: "after_update"}
	w.stale["wf-1"] = true // list still reports it; the per-item re-detect says clean

	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckZombieWorkflows}, true)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if results[0].Skipped != 1 || results[0].Fixed != 0 {
		t.Fatalf("fix = %+v; want 1 skipped (re-detect-first contract)", results[0])
	}
	if w.wfDisabled["wf-1"] || len(au.events) != 0 {
		t.Fatalf("stale item không được mutate/audit — disabled=%v events=%d", w.wfDisabled["wf-1"], len(au.events))
	}
}

func TestDoctor_ReportOnly_FixRefusedWholeRequest(t *testing.T) {
	w, _, svc := newDoctorFixture()
	// A live B1 finding rides along to prove validate-before-mutate: the report-only refusal
	// must reject the WHOLE request before the safe check's fix runs.
	w.phantoms["rel-1"] = doctor.RelationshipRow{ID: "rel-1", Name: "ghost", ParentEntityName: "x", ChildEntityName: "y", RelationshipType: "N-N"}
	w.deadTargets["f-1"] = doctor.DeadRelationTargetRow{FieldID: "f-1", EntityName: "accounts", FieldName: "owner", RelatedEntity: "gone"}

	for _, id := range []string{DoctorCheckOrphanUniqueIndex, DoctorCheckUnregisteredRelTables, DoctorCheckDeadRelationTarget} {
		_, err := svc.Fix(context.Background(), "t-a", []string{id, DoctorCheckPhantomRelationships}, true)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "DOCTOR_REPORT_ONLY" {
			t.Fatalf("%s: err = %v; want ValidationError DOCTOR_REPORT_ONLY (kể cả confirm=true)", id, err)
		}
		if _, ok := w.phantoms["rel-1"]; !ok {
			t.Fatalf("%s: B1 fix đã chạy dù request bị reject — mutation trước validation", id)
		}
	}
}

// TestDoctor_A3_OrphanUniqueIndexReported pins the RM-28 Slice B repurpose (spec §6): a plain
// `is_unique` field's index is now non-unique (idx_), so it NEVER legitimately owns a uq_ name
// any more — ANY surviving uq_ behind one (residual/half-run-demote) is now itself flagged,
// alongside the pre-existing "truly orphaned, no live field at all" class. A 1-1 relation's own
// uq_ index stays expected/unflagged (never pivot-backed, never demoted).
func TestDoctor_A3_OrphanUniqueIndexReported(t *testing.T) {
	w, _, svc := newDoctorFixture()
	seedSchemaEntity(w, "accounts")
	tbl := w.tables["obj_accounts"]
	// email: live unique field whose index is STILL physically uq_ (e.g. migration 000051's
	// coverage guard declined to demote it) → RM-28 Slice B: now flagged (no longer owned).
	w.physFields["f-email"] = doctor.PhysicalFieldRow{FieldID: "f-email", EntityName: "accounts", FieldName: "email", FieldType: "text", IsUnique: 1}
	tbl.columns["email"] = fakeGuardedExpr
	tbl.indexes["uq_obj_accounts_email"] = []string{"tenant_id", "email"}
	// code: live unique field CORRECTLY demoted (idx_, non-unique) → the healthy steady state,
	// never flagged (A3 only ever scans uq_-prefixed indexes to begin with).
	w.physFields["f-code"] = doctor.PhysicalFieldRow{FieldID: "f-code", EntityName: "accounts", FieldName: "code", FieldType: "text", IsUnique: 1}
	tbl.columns["code"] = fakeGuardedExpr
	tbl.indexes["idx_obj_accounts_code"] = []string{"tenant_id", "code"}
	// partner: live 1-1 relation field + its _id index → still owned/expected, unaffected.
	w.physFields["f-partner"] = doctor.PhysicalFieldRow{FieldID: "f-partner", EntityName: "accounts", FieldName: "partner", FieldType: "relation", Options: `{"relatedEntity":"accounts","cardinality":"1-1"}`}
	tbl.columns["partner_id"] = fakeGuardedExpr
	tbl.indexes["uq_obj_accounts_partner_id"] = []string{"tenant_id", "partner_id"}
	// sku: index still on the shared table but NO live field for THIS tenant at all → A3 (pre-existing class).
	tbl.columns["sku"] = fakeGuardedExpr
	tbl.indexes["uq_obj_accounts_sku"] = []string{"tenant_id", "sku"}

	got := scanByID(t, svc, "t-a")[DoctorCheckOrphanUniqueIndex]
	wantKeys := map[string]bool{"accounts:uq_obj_accounts_email": true, "accounts:uq_obj_accounts_sku": true}
	if got.Count != len(wantKeys) {
		t.Fatalf("detect = %+v; want %d findings (email residual-uq_ + sku orphan)", got, len(wantKeys))
	}
	for _, it := range got.Items {
		if !wantKeys[it.Key] {
			t.Fatalf("unexpected finding key %q; want one of %v", it.Key, wantKeys)
		}
		delete(wantKeys, it.Key)
	}
	if len(wantKeys) != 0 {
		t.Fatalf("missing findings: %v", wantKeys)
	}
	if !got.ReportOnly || got.AutoFixSafe {
		t.Fatalf("A3 = %+v; want reportOnly=true, autoFixSafe=false", got)
	}
	// The guidance must warn against dropping on the shared table.
	for _, it := range got.Items {
		if !strings.Contains(it.Detail, "KHÔNG tự DROP") {
			t.Fatalf("detail thiếu cảnh báo co-tenant: %s", it.Detail)
		}
	}
}

func TestDoctor_B5_OrphanRelTablesReportedWithAge(t *testing.T) {
	w, _, svc := newDoctorFixture()
	created := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	w.orphanRelTbl = []doctor.OrphanRelTable{
		{TableName: "rel_accounts_tags", CreatedAt: sql.NullTime{Time: created, Valid: true}},
		{TableName: "rel_old_pair", CreatedAt: sql.NullTime{}},
	}

	got := scanByID(t, svc, "t-a")[DoctorCheckUnregisteredRelTables]
	if got.Count != 2 || !got.ReportOnly {
		t.Fatalf("detect = %+v; want 2 findings reportOnly", got)
	}
	if !strings.Contains(got.Items[0].Detail, "2026-06-01T00:00:00Z") {
		t.Fatalf("finding phải kèm tuổi bảng (CREATE_TIME): %s", got.Items[0].Detail)
	}
	if !strings.Contains(got.Items[1].Detail, "không rõ") {
		t.Fatalf("CREATE_TIME NULL phải báo trung thực: %s", got.Items[1].Detail)
	}
	// The grace-window is a feature — the copy must say so, not prescribe a drop.
	if !strings.Contains(got.Items[0].Detail, "FEATURE") || !strings.Contains(got.Items[0].Detail, "KHÔNG tự DROP") {
		t.Fatalf("detail thiếu giải thích grace-window: %s", got.Items[0].Detail)
	}
}

func TestDoctor_C8_DeadRelationTargetGuidesToDeleteField(t *testing.T) {
	w, _, svc := newDoctorFixture()
	w.deadTargets["f-owner"] = doctor.DeadRelationTargetRow{FieldID: "f-owner", EntityName: "tickets", FieldName: "owner", RelatedEntity: "gone_users"}

	got := scanByID(t, svc, "t-a")[DoctorCheckDeadRelationTarget]
	if got.Count != 1 || got.Items[0].Key != "f-owner" || !got.ReportOnly {
		t.Fatalf("detect = %+v; want 1 reportOnly finding key f-owner", got)
	}
	d := got.Items[0].Detail
	if !strings.Contains(d, "tickets.owner") || !strings.Contains(d, "gone_users") || !strings.Contains(d, "DeleteField") || !strings.Contains(d, "f-owner") {
		t.Fatalf("detail phải nêu field + target + hướng dẫn DeleteField flow: %s", d)
	}
}

func TestDoctor_FlushCaches(t *testing.T) {
	t.Run("flush cả 4 cache khi wired đủ + audit row", func(t *testing.T) {
		w, au, _ := newDoctorFixture()
		catalog := &countingCatalog{}
		metaCache := NewMetaCache(catalog, time.Hour)
		aclInner := &countingACLLoader{}
		aclCache := NewCachedACLLoader(aclInner, time.Hour)
		wfInner := &countingWorkflowLoader{}
		wfCache := workflow.NewCachedLoader(wfInner, time.Hour)
		// #14 claim-freshness: the live user-row cache the Auth stage reads is an in-proc TTL
		// cache too — the flush escape hatch must clear it like the other three.
		userInner := newCountingUserReader()
		userCache := NewCachedUserLoader(userInner, time.Hour)
		svc := NewDoctorService(w, w, w, w, w, au, nil).WithMetaCache(metaCache).WithOpsCaches(aclCache, wfCache).WithUserClaimCache(userCache)

		// Warm all four.
		if _, err := metaCache.GetEntityByName(context.Background(), "t-a", "accounts"); err != nil {
			t.Fatalf("warm meta: %v", err)
		}
		if _, err := aclCache.LoadActionsForACL(context.Background(), "t-a", "role-1"); err != nil {
			t.Fatalf("warm acl: %v", err)
		}
		if _, err := wfCache.ListActive(context.Background(), "t-a", "accounts", workflow.TriggerAfterCreate); err != nil {
			t.Fatalf("warm wf: %v", err)
		}
		if _, err := userCache.GetUserByID(context.Background(), "t1", "u1"); err != nil {
			t.Fatalf("warm users: %v", err)
		}

		flushed := svc.FlushCaches(context.Background(), "t-a")
		if len(flushed) != 4 || flushed[0] != "meta" || flushed[1] != "acl" || flushed[2] != "workflow" || flushed[3] != "users" {
			t.Fatalf("flushed = %v; want [meta acl workflow users]", flushed)
		}
		// Every cache reloads from its inner source after the flush.
		if _, err := metaCache.GetEntityByName(context.Background(), "t-a", "accounts"); err != nil {
			t.Fatalf("reload meta: %v", err)
		}
		if _, err := aclCache.LoadActionsForACL(context.Background(), "t-a", "role-1"); err != nil {
			t.Fatalf("reload acl: %v", err)
		}
		if _, err := wfCache.ListActive(context.Background(), "t-a", "accounts", workflow.TriggerAfterCreate); err != nil {
			t.Fatalf("reload wf: %v", err)
		}
		if _, err := userCache.GetUserByID(context.Background(), "t1", "u1"); err != nil {
			t.Fatalf("reload users: %v", err)
		}
		if n := atomic.LoadInt64(&catalog.entityCalls); n != 2 {
			t.Fatalf("meta loads = %d; want 2 (flush phải xóa entry)", n)
		}
		if n := atomic.LoadInt64(&aclInner.calls); n != 2 {
			t.Fatalf("acl loads = %d; want 2", n)
		}
		if n := atomic.LoadInt64(&wfInner.calls); n != 2 {
			t.Fatalf("wf loads = %d; want 2", n)
		}
		if userInner.loads != 2 {
			t.Fatalf("user row loads = %d; want 2 (flush phải xóa hàng user cache — claim-freshness #14)", userInner.loads)
		}
		// The action is audited (admin-attributable operational action).
		if len(au.events) != 1 || au.events[0].Diff == nil || au.events[0].Diff.After["action"] != "flush-caches" {
			t.Fatalf("audit events = %+v; want 1 row action=flush-caches", au.events)
		}
	})

	t.Run("cache không wired → không nằm trong flushed (trung thực)", func(t *testing.T) {
		w, _, _ := newDoctorFixture()
		svc := NewDoctorService(w, w, w, w, w, nil, nil) // no caches at all
		if flushed := svc.FlushCaches(context.Background(), "t-a"); len(flushed) != 0 {
			t.Fatalf("flushed = %v; want [] khi không cache nào wired", flushed)
		}
	})
}
