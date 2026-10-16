//go:build integration

package services

// publish_integration_test.go — specs/063-draft-publish T027–T030 against a real MySQL: draft mode
// end to end with the REAL record service + the REAL workflow loader (so "the engine never runs a
// draft" is observed, not assumed), publish atomicity under injected faults, 20-way race, conflict,
// first publish, cross-tenant ids and the audit trail.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/workflow"
)

type dpEnv struct {
	hkEnv
	reg    *metaversion.Registry
	drafts *metaversion.DraftRepo
	vers   *metaversion.VersionRepo
	gate   *DraftGate
	wf     *WorkflowService // the admin service over the draft-aware repo
	pub    *PublishService
	// invalidations counts post-commit cache drops by the publish service.
	invalidations *int32
}

func dpSetup(t *testing.T) dpEnv { return dpSetupWith(t, NewWorkflowDocAdapter()) }

func dpSetupWith(t *testing.T, adapters ...metaversion.DocAdapter) dpEnv {
	t.Helper()
	e := hkSetup(t)
	reg := metaversion.NewRegistry(adapters...)
	drafts := metaversion.NewDraftRepo(e.sdb, reg)
	vers := metaversion.NewVersionRepo(e.sdb)
	gate := NewDraftGate(vers)
	var inv int32
	d := dpEnv{hkEnv: e, reg: reg, drafts: drafts, vers: vers, gate: gate, invalidations: &inv}
	rawRepo := workflow.NewSQLRepository(e.sdb)
	d.wf = NewWorkflowService(NewDraftWorkflowRepo(rawRepo, drafts, gate)).WithLiveRepo(rawRepo).WithEntityChecker(e.meta).WithCache(e.cache)
	d.pub = NewPublishService(e.sdb, drafts, vers, reg, gate, e.audit, nil).
		WithInvalidator(DocTypeWorkflow, func(tenantID, entity string) { atomic.AddInt32(&inv, 1); e.cache.Invalidate(tenantID, entity) })
	return d
}

// newTenant inserts a tenants row (the draft layer locks it) plus the hk_deals entity.
func (e dpEnv) newTenant(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := e.sdb.Exec(`INSERT INTO tenants (id, slug, name) VALUES (?, ?, 'dp test')`, id, "dp"+id[:8]); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	e.createDeals(t, id)
	return id
}

func builderCtxOf(tenant, userID string) context.Context {
	return metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: userID, TenantID: tenant, HomeTenantID: tenant, IsBuilder: true, ReadDraft: true})
}

func legacyCtxOf(tenant string) context.Context {
	return metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: uuid.NewString(), TenantID: tenant, HomeTenantID: tenant})
}

const dpVetoMsg = "khong duoc khoa"

func lockedVeto(name string) CreateWorkflowInput {
	on := true
	return CreateWorkflowInput{Name: name, EntityName: "hk_deals", Trigger: string(workflow.TriggerBeforeSave),
		Rules:   []WorkflowRuleInput{{Field: "stage", Op: workflow.OpEq, Value: "Locked"}},
		Actions: []WorkflowActionInput{{Type: workflow.ActionVeto, Field: "stage", Message: dpVetoMsg}}, IsActive: &on}
}

func closedWonVeto(name string) CreateWorkflowInput {
	on := true
	return CreateWorkflowInput{Name: name, EntityName: "hk_deals", Trigger: string(workflow.TriggerBeforeSave),
		Rules:   []WorkflowRuleInput{{Field: "stage", Op: workflow.OpEq, Value: "Closed Won"}, {Field: "close_date", Op: workflow.OpIsNull}},
		Actions: []WorkflowActionInput{{Type: workflow.ActionVeto, Field: "close_date", Message: "can ngay chot"}}, IsActive: &on}
}

func asUpdate(in CreateWorkflowInput, name string, active bool) UpdateWorkflowInput {
	return UpdateWorkflowInput{Name: name, EntityName: in.EntityName, Trigger: in.Trigger, Rules: in.Rules, Actions: in.Actions, IsActive: &active}
}

var dealSeq int32

// createDeal saves a deal through the REAL record service (before rules run via the CachedLoader).
func (e dpEnv) createDeal(tenant, stage string) error {
	n := atomic.AddInt32(&dealSeq, 1)
	_, err := e.rec.Create(context.Background(), engAdmin(hkUser), tenant, hkUser, "hk_deals",
		map[string]any{"name": fmt.Sprintf("deal %d", n), "code": fmt.Sprintf("D-%d-%s", n, tenant[:6]), "stage": stage})
	return err
}

type liveRow struct {
	Name    string `db:"name"`
	Active  bool   `db:"is_active"`
	Deleted bool   `db:"del"`
}

func (e dpEnv) liveRule(t *testing.T, tenant, id string) (liveRow, bool) {
	t.Helper()
	var r liveRow
	err := e.sdb.Get(&r, `SELECT name, is_active, deleted_at IS NOT NULL AS del FROM sys_workflows WHERE tenant_id = ? AND id = ?`, tenant, id)
	if err != nil {
		return liveRow{}, false
	}
	return r, true
}

func (e dpEnv) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.sdb.Get(&n, query, args...); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (e dpEnv) draftCount(t *testing.T, tenant string) int {
	return e.count(t, `SELECT COUNT(*) FROM sys_meta_drafts WHERE tenant_id = ?`, tenant)
}

func (e dpEnv) versionCount(t *testing.T, tenant string) int {
	return e.count(t, `SELECT COUNT(*) FROM sys_meta_versions WHERE tenant_id = ?`, tenant)
}

func (e dpEnv) publishWithCurrentHash(ctx context.Context, tenant string, force bool) (*PublishResult, error) {
	_, hash, err := e.pub.Changes(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return e.pub.Publish(ctx, tenant, PublishRequest{ChangesetHash: hash, Force: force})
}

// deliver runs the first publish for a tenant that already has the given live rules.
func (e dpEnv) deliver(t *testing.T, tenant string, builder string) {
	t.Helper()
	if _, err := e.pub.Baseline(builderCtxOf(tenant, builder), tenant, "first"); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------

// US2 + FR-001/003/027: before delivery everything is live, the first publish snapshots and turns
// draft mode on, and it cannot be done twice.
func TestPublish_FirstPublishTurnsDraftModeOn_Integration(t *testing.T) {
	e := dpSetup(t)
	ctx := context.Background()

	// A tenant with ZERO rules: version 1 covers the type anyway.
	empty := e.newTenant(t)
	bEmpty := uuid.NewString()
	if _, err := e.pub.Publish(builderCtxOf(empty, bEmpty), empty, PublishRequest{ChangesetHash: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("publish on a tenant that is not delivered succeeded")
	} else {
		wantCode(t, err, "NOT_DELIVERED")
	}
	res, err := e.pub.Baseline(builderCtxOf(empty, bEmpty), empty, "")
	if err != nil || res.VersionNo != 1 || res.DocCount != 0 {
		t.Fatalf("baseline of an empty tenant = %+v, %v", res, err)
	}
	var covered string
	if err := e.sdb.Get(&covered, `SELECT covered_types FROM sys_meta_versions WHERE tenant_id = ?`, empty); err != nil || covered != `["workflow"]` {
		t.Fatalf("covered_types = %q (%v), want [\"workflow\"] even with no rule", covered, err)
	}

	tenant := e.newTenant(t)
	builder := uuid.NewString()
	if _, err := e.sdb.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, full_name, status) VALUES (?, ?, ?, 'x', 'Nguyen Builder', 'active')`,
		builder, tenant, builder[:8]+"@t.vn"); err != nil {
		t.Fatal(err)
	}
	bctx := builderCtxOf(tenant, builder)

	// Not delivered: a builder's save is LIVE, no draft, and the engine runs it at once.
	r1, err := e.wf.CreateWorkflow(bctx, tenant, lockedVeto("no locked"))
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := e.liveRule(t, tenant, r1.ID); !ok || !row.Active || e.draftCount(t, tenant) != 0 {
		t.Fatalf("before delivery the rule must be live with 0 drafts: %+v ok=%v drafts=%d", row, ok, e.draftCount(t, tenant))
	}
	hkWantVeto(t, e.createDeal(tenant, "Locked"), dpVetoMsg)
	st, err := e.pub.State(bctx, tenant)
	if err != nil || st.Delivered || st.HeadVersion != nil || st.PendingCount != 0 {
		t.Fatalf("State before delivery = %+v, %v", st, err)
	}

	res, err = e.pub.Baseline(bctx, tenant, "giao cho khach")
	if err != nil || res.VersionNo != 1 || res.Kind != "baseline" || res.DocCount != 1 || res.PropagationSeconds != 60 {
		t.Fatalf("Baseline = %+v, %v", res, err)
	}
	// FR-027: snapshot count = number of live rules; label captured at write time.
	if n := e.count(t, `SELECT COUNT(*) FROM sys_meta_version_docs WHERE tenant_id = ? AND op = 'snapshot' AND doc_type = 'workflow'`, tenant); n != 1 {
		t.Fatalf("snapshot docs = %d, want 1", n)
	}
	var label string
	if err := e.sdb.Get(&label, `SELECT created_by_label FROM sys_meta_versions WHERE tenant_id = ?`, tenant); err != nil || label != "Nguyen Builder" {
		t.Fatalf("created_by_label = %q (%v)", label, err)
	}
	if ok, _ := e.vers.HasNumbered(ctx, tenant); !ok {
		t.Fatal("HasNumbered false after the baseline")
	}
	// One-way: a second baseline is refused and writes nothing.
	n := e.versionCount(t, tenant)
	if _, err := e.pub.Baseline(bctx, tenant, ""); err == nil {
		t.Fatal("second baseline accepted")
	} else {
		wantCode(t, err, "ALREADY_DELIVERED")
	}
	if e.versionCount(t, tenant) != n {
		t.Fatal("a refused baseline wrote a version")
	}
	// After delivery: a builder's save goes to a draft (the live rule is untouched).
	if _, err := e.wf.UpdateWorkflow(bctx, tenant, r1.ID, asUpdate(lockedVeto("no locked"), "no locked v2", true)); err != nil {
		t.Fatal(err)
	}
	if row, _ := e.liveRule(t, tenant, r1.ID); row.Name != "no locked" || e.draftCount(t, tenant) != 1 {
		t.Fatalf("after delivery the edit must be a draft: live %+v drafts %d", row, e.draftCount(t, tenant))
	}
	// A tenant that was never delivered is unaffected by this one (FR-003 per tenant).
	other := e.newTenant(t)
	if _, err := e.wf.CreateWorkflow(builderCtxOf(other, uuid.NewString()), other, lockedVeto("other tenant live")); err != nil {
		t.Fatal(err)
	}
	if e.draftCount(t, other) != 0 {
		t.Fatal("a tenant with no numbered version got a draft")
	}
}

// US1 end to end: the customer keeps the OLD rules until publish; publish is one version with
// before/after; the engine switches at once (cache invalidated); the audit trail carries no body.
func TestPublish_DraftThenPublish_Integration(t *testing.T) {
	e := dpSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx, sctx := builderCtxOf(tenant, builder), legacyCtxOf(tenant)

	x, err := e.wf.CreateWorkflow(bctx, tenant, lockedVeto("rule X"))
	if err != nil {
		t.Fatal(err)
	}
	e.deliver(t, tenant, builder)

	// Edit X off (draft), add Y (draft). Neither reaches the engine.
	edited, err := e.wf.UpdateWorkflow(bctx, tenant, x.ID, asUpdate(lockedVeto("rule X"), "rule X", false))
	if err != nil || edited.DraftState != "edited" || edited.IsActive {
		t.Fatalf("edit = %+v, %v", edited, err)
	}
	y, err := e.wf.CreateWorkflow(bctx, tenant, closedWonVeto("rule Y"))
	if err != nil || y.DraftState != "new" {
		t.Fatalf("create Y = %+v, %v", y, err)
	}
	if row, _ := e.liveRule(t, tenant, x.ID); !row.Active {
		t.Fatal("a draft save switched the live rule off")
	}
	if _, ok := e.liveRule(t, tenant, y.ID); ok {
		t.Fatal("a draft-only rule exists in sys_workflows")
	}
	// SC-005: the engine (real record service + real CachedLoader) still runs OLD X and has never heard of Y.
	hkWantVeto(t, e.createDeal(tenant, "Locked"), dpVetoMsg)
	for i := 0; i < 50; i++ {
		if err := e.createDeal(tenant, "Closed Won"); err != nil { // Y would veto a Closed Won without a date
			t.Fatalf("deal %d refused by a rule that is only a draft: %v", i, err)
		}
	}
	// What the customer's admin sees is live only; the builder sees the overlay.
	staffList, _ := e.wf.ListWorkflows(sctx, tenant)
	if len(staffList) != 1 || staffList[0].Name != "rule X" || !staffList[0].IsActive || staffList[0].DraftState != "" {
		t.Fatalf("legacy admin list = %+v, want only the live rule X untouched", staffList)
	}
	builderList, _ := e.wf.ListWorkflows(bctx, tenant)
	if len(builderList) != 2 {
		t.Fatalf("builder list = %d rules, want 2", len(builderList))
	}

	items, hash, err := e.pub.Changes(bctx, tenant)
	if err != nil || len(items) != 2 {
		t.Fatalf("Changes = %+v, %v", items, err)
	}
	ops := map[string]string{}
	for _, it := range items {
		if it.Conflict {
			t.Fatalf("unexpected conflict on %+v", it)
		}
		ops[it.DocKey] = it.Op
	}
	if ops[x.ID] != "update" || ops[y.ID] != "create" {
		t.Fatalf("ops = %v", ops)
	}
	st, _ := e.pub.State(bctx, tenant)
	if !st.Delivered || st.PendingCount != 2 || st.ChangesetHash != hash || st.HeadVersion == nil || st.HeadVersion.VersionNo != 1 ||
		strings.Join(st.DraftableTypes, ",") != "app,branding,dashboard,entity,field,layout,report,workflow" { // S7-9 added "entity"
		t.Fatalf("State = %+v", st)
	}

	// A stale list is refused with the CURRENT fingerprint; a spent set is NOTHING_TO_PUBLISH later.
	_, err = e.pub.Publish(bctx, tenant, PublishRequest{ChangesetHash: strings.Repeat("0", 64)})
	wantCode(t, err, "DRAFTS_CHANGED")
	var ve *ValidationError
	if errors.As(err, &ve); ve.Details["changesetHash"] != hash {
		t.Fatalf("DRAFTS_CHANGED details = %v, want the current hash %s", ve.Details, hash)
	}
	if e.draftCount(t, tenant) != 2 {
		t.Fatal("a refused publish consumed drafts")
	}

	before := atomic.LoadInt32(e.invalidations)
	res, err := e.pub.Publish(bctx, tenant, PublishRequest{ChangesetHash: hash, Note: "Sua luat nhac hen"})
	if err != nil || res.VersionNo != 2 || res.Kind != "publish" || res.DocCount != 2 || res.PropagationSeconds != 60 {
		t.Fatalf("Publish = %+v, %v", res, err)
	}
	if e.draftCount(t, tenant) != 0 {
		t.Fatal("drafts survived a publish")
	}
	if row, _ := e.liveRule(t, tenant, x.ID); row.Active {
		t.Fatal("X still active after publish")
	}
	if row, ok := e.liveRule(t, tenant, y.ID); !ok || !row.Active || row.Name != "rule Y" {
		t.Fatalf("Y after publish = %+v ok=%v — a new rule keeps the id it got as a draft (FR-007)", row, ok)
	}
	if atomic.LoadInt32(e.invalidations) == before {
		t.Fatal("publish did not drop the engine cache")
	}
	// The engine switches AT ONCE (the loader TTL is an hour: only invalidation explains this).
	if err := e.createDeal(tenant, "Locked"); err != nil {
		t.Fatalf("X was switched off by the publish but still vetoes: %v", err)
	}
	hkWantVeto2 := e.createDeal(tenant, "Closed Won")
	var veto *ValidationError
	if !errors.As(hkWantVeto2, &veto) || veto.Code != "RECORD_HOOK_VETO" {
		t.Fatalf("Y does not run after publish: %v", hkWantVeto2)
	}

	// The version: before/after of each document, full content, ordered after the baseline.
	type vdoc struct {
		Op     string          `db:"op"`
		DocKey string          `db:"doc_key"`
		Before *string         `db:"before_json"`
		After  json.RawMessage `db:"after_json"`
	}
	var docs []vdoc
	if err := e.sdb.Select(&docs, `SELECT d.op, d.doc_key, d.before_json, d.after_json FROM sys_meta_version_docs d
		JOIN sys_meta_versions v ON v.id = d.version_id AND v.tenant_id = d.tenant_id
		WHERE d.tenant_id = ? AND v.version_no = 2 ORDER BY d.op`, tenant); err != nil || len(docs) != 2 {
		t.Fatalf("version docs = %+v, %v", docs, err)
	}
	for _, d := range docs {
		switch d.DocKey {
		case y.ID:
			if d.Op != "create" || d.Before != nil || !strings.Contains(string(d.After), "rule Y") {
				t.Fatalf("Y doc = %+v", d)
			}
		case x.ID:
			if d.Op != "update" || d.Before == nil || !strings.Contains(*d.Before, `"isActive": true`) || !strings.Contains(string(d.After), `"isActive": false`) {
				t.Fatalf("X doc = %+v (before %v)", d, d.Before)
			}
		default:
			t.Fatalf("unexpected doc %s", d.DocKey)
		}
	}
	// Audit: the actor is the builder, the diff holds ONLY the summary keys (the customer admin reads /audit-log).
	var found bool
	for _, ev := range e.audit.events {
		if ev.EntityName != "__meta_version__" || ev.Action != "publish" {
			continue
		}
		found = true
		if ev.ActorUserID != builder {
			t.Errorf("audit actor = %q, want the builder %q even though the handler passed a plain ctx", ev.ActorUserID, builder)
		}
		for _, side := range []map[string]any{ev.Diff.Before, ev.Diff.After} {
			for k := range side {
				if k != "versionNo" && k != "kind" && k != "docCount" {
					t.Errorf("audit diff carries %q — only {versionNo, kind, docCount} may", k)
				}
			}
		}
		if raw, _ := json.Marshal(ev); strings.Contains(string(raw), "rule Y") || strings.Contains(string(raw), "Locked") {
			t.Errorf("audit event carries document content: %s", raw)
		}
	}
	if !found {
		t.Fatal("no __meta_version__ publish audit event")
	}

	// Nothing left: the same publish again is NOTHING_TO_PUBLISH and writes no version.
	n := e.versionCount(t, tenant)
	_, err = e.pub.Publish(bctx, tenant, PublishRequest{ChangesetHash: hash})
	wantCode(t, err, "NOTHING_TO_PUBLISH")
	if e.versionCount(t, tenant) != n {
		t.Fatal("NOTHING_TO_PUBLISH wrote a version")
	}
}

// FR-023: a live document changed (by the legacy customer admin) under a draft ⇒ PUBLISH_CONFLICT,
// "force" overrides explicitly, and forcing over a rule the customer admin DELETED restores it.
func TestPublish_ConflictAndForce_Integration(t *testing.T) {
	e := dpSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx, sctx := builderCtxOf(tenant, builder), legacyCtxOf(tenant)
	x, _ := e.wf.CreateWorkflow(bctx, tenant, lockedVeto("rule X"))
	z, _ := e.wf.CreateWorkflow(bctx, tenant, lockedVeto("rule Z"))
	e.deliver(t, tenant, builder)

	_, _ = e.wf.UpdateWorkflow(bctx, tenant, x.ID, asUpdate(lockedVeto("rule X"), "X by builder", true))
	_, _ = e.wf.UpdateWorkflow(bctx, tenant, z.ID, asUpdate(lockedVeto("rule Z"), "Z by builder", true))
	// The legacy admin (not the builder) edits X and deletes Z — straight to live, as today.
	if _, err := e.wf.UpdateWorkflow(sctx, tenant, x.ID, asUpdate(lockedVeto("rule X"), "X by customer admin", true)); err != nil {
		t.Fatal(err)
	}
	if err := e.wf.DeleteWorkflow(sctx, tenant, z.ID); err != nil {
		t.Fatal(err)
	}
	if row, _ := e.liveRule(t, tenant, x.ID); row.Name != "X by customer admin" {
		t.Fatalf("the legacy admin's write did not land live: %+v", row)
	}
	items, _, _ := e.pub.Changes(bctx, tenant)
	for _, it := range items {
		if !it.Conflict {
			t.Fatalf("change %+v should be flagged as a conflict", it)
		}
	}
	before := e.versionCount(t, tenant)
	_, err := e.publishWithCurrentHash(bctx, tenant, false)
	wantCode(t, err, "PUBLISH_CONFLICT")
	var ve *ValidationError
	errors.As(err, &ve)
	if cs, _ := ve.Details["conflicts"].([]map[string]any); len(cs) != 2 {
		t.Fatalf("conflicts = %v, want 2 documents", ve.Details["conflicts"])
	}
	if e.versionCount(t, tenant) != before || e.draftCount(t, tenant) != 2 {
		t.Fatal("a conflicting publish changed something")
	}
	if row, _ := e.liveRule(t, tenant, x.ID); row.Name != "X by customer admin" {
		t.Fatal("a refused publish overwrote the customer admin's edit")
	}

	if _, err := e.publishWithCurrentHash(bctx, tenant, true); err != nil {
		t.Fatalf("forced publish: %v", err)
	}
	if row, _ := e.liveRule(t, tenant, x.ID); row.Name != "X by builder" {
		t.Fatalf("force did not apply the draft: %+v", row)
	}
	if row, ok := e.liveRule(t, tenant, z.ID); !ok || row.Deleted || row.Name != "Z by builder" {
		t.Fatalf("force over a soft-deleted rule must restore it under its own id: %+v ok=%v", row, ok)
	}
}

// Discard: one change or all; a stale fingerprint is refused; another tenant's draft id is a 404 and untouched.
func TestPublish_DiscardAndCrossTenant_Integration(t *testing.T) {
	e := dpSetup(t)
	a, b := e.newTenant(t), e.newTenant(t)
	ba, bb := uuid.NewString(), uuid.NewString()
	actx, bctx := builderCtxOf(a, ba), builderCtxOf(b, bb)
	ra, _ := e.wf.CreateWorkflow(actx, a, lockedVeto("A rule"))
	rb, _ := e.wf.CreateWorkflow(bctx, b, lockedVeto("B rule"))
	e.deliver(t, a, ba)
	e.deliver(t, b, bb)
	_, _ = e.wf.UpdateWorkflow(actx, a, ra.ID, asUpdate(lockedVeto("A rule"), "A edited", true))
	_, _ = e.wf.CreateWorkflow(actx, a, closedWonVeto("A new"))
	_, _ = e.wf.UpdateWorkflow(bctx, b, rb.ID, asUpdate(lockedVeto("B rule"), "B edited", true))

	itemsB, hashB, _ := e.pub.Changes(bctx, b)
	itemsA, hashA, _ := e.pub.Changes(actx, a)
	if len(itemsA) != 2 || len(itemsB) != 1 {
		t.Fatalf("changes A=%d B=%d, want 2 and 1 (no cross-tenant bleed)", len(itemsA), len(itemsB))
	}
	// A's builder cannot touch B's draft through any route: same 404 as an unknown id, B's draft intact.
	for name, call := range map[string]func() error{
		"Discard of B's draft id":  func() error { return e.pub.Discard(actx, a, itemsB[0].ID) },
		"Discard of an unknown id": func() error { return e.pub.Discard(actx, a, uuid.NewString()) },
	} {
		wantCode(t, call(), "DRAFT_NOT_FOUND")
		_ = name
	}
	if e.draftCount(t, b) != 1 {
		t.Fatal("tenant B's draft was removed by tenant A")
	}
	if _, err := e.pub.Publish(actx, b, PublishRequest{ChangesetHash: hashB}); err == nil {
		t.Fatal("tenant A's builder published tenant B")
	} else {
		wantCode(t, err, "FORBIDDEN")
	}

	// Discard one: the rule is back to exactly what is live.
	var editedID string
	for _, it := range itemsA {
		if it.Op == "update" {
			editedID = it.ID
		}
	}
	if err := e.pub.Discard(actx, a, editedID); err != nil {
		t.Fatal(err)
	}
	got, err := e.wf.GetWorkflow(actx, a, ra.ID)
	if err != nil || got.Name != "A rule" || got.DraftState != "live" {
		t.Fatalf("after discarding the draft: %+v, %v", got, err)
	}
	// The same id again finds nothing: a discard that removed 0 rows is never a quiet success.
	wantCode(t, e.pub.Discard(actx, a, editedID), "DRAFT_NOT_FOUND")
	// A single discard leaves the same summary-only trace as discard-all (no label, no body).
	single := 0
	for _, ev := range e.audit.events {
		if ev.EntityName == "__meta_version__" && ev.Action == "discard" && ev.TenantID == a {
			single++
			if fmt.Sprint(ev.Diff.After["discardedCount"]) != "1" || len(ev.Diff.After) != 1 {
				t.Errorf("single discard audit diff = %+v, want only discardedCount=1", ev.Diff)
			}
		}
	}
	if single != 1 {
		t.Fatalf("%d discard audit events after one discard, want 1", single)
	}
	// Discard all needs the fingerprint the builder looked at.
	err = e.pub.DiscardAll(actx, a, hashA)
	wantCode(t, err, "DRAFTS_CHANGED")
	_, hashNow, _ := e.pub.Changes(actx, a)
	if err := e.pub.DiscardAll(actx, a, hashNow); err != nil {
		t.Fatal(err)
	}
	if e.draftCount(t, a) != 0 || e.draftCount(t, b) != 1 {
		t.Fatalf("drafts A=%d B=%d after discard-all, want 0 and 1", e.draftCount(t, a), e.draftCount(t, b))
	}
	var discard bool
	for _, ev := range e.audit.events {
		if ev.EntityName == "__meta_version__" && ev.Action == "discard" && ev.TenantID == a {
			discard = true
			if ev.Diff.After["discardedCount"] == nil || len(ev.Diff.After) != 1 {
				t.Errorf("discard audit diff = %+v, want only discardedCount", ev.Diff)
			}
		}
	}
	if !discard {
		t.Fatal("no discard audit event")
	}
}

// ---------------------------------------------------------------------------------------------

// failingAdapter wraps the workflow adapter with injectable failures.
type failingAdapter struct {
	*WorkflowDocAdapter
	applyCalls  int32
	failApplyAt int32 // fail on the Nth ApplyTx call (1-based); 0 = never
	problem     *metaversion.Problem
}

func (f *failingAdapter) ApplyTx(ctx context.Context, tx *sqlx.Tx, tenantID string, c metaversion.Change) error {
	n := atomic.AddInt32(&f.applyCalls, 1)
	if err := f.WorkflowDocAdapter.ApplyTx(ctx, tx, tenantID, c); err != nil {
		return err
	}
	if f.failApplyAt != 0 && n == f.failApplyAt {
		return errors.New("injected apply failure (after the write)")
	}
	return nil
}

func (f *failingAdapter) ValidateFinal(ctx context.Context, fs metaversion.FinalState) []metaversion.Problem {
	if f.problem != nil {
		return []metaversion.Problem{*f.problem}
	}
	return f.WorkflowDocAdapter.ValidateFinal(ctx, fs)
}

func (e dpEnv) snapshotLive(t *testing.T, tenant string) string {
	t.Helper()
	var rows []struct {
		ID     string `db:"id"`
		Name   string `db:"name"`
		Active bool   `db:"is_active"`
		Del    bool   `db:"del"`
	}
	if err := e.sdb.Select(&rows, `SELECT id, name, is_active, deleted_at IS NOT NULL AS del FROM sys_workflows WHERE tenant_id = ? ORDER BY id`, tenant); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

// SC-002 / FR-021: a failure at ANY step of publish leaves the live rules, the drafts and the
// history exactly as they were, and drops no cache. Faults: validation, the 2nd apply (after the 1st
// wrote), the version-doc insert (after an apply), the version row insert, and the final draft cleanup.
func TestPublish_FaultInjectionIsAtomic_Integration(t *testing.T) {
	fa := &failingAdapter{WorkflowDocAdapter: NewWorkflowDocAdapter()}
	e := dpSetupWith(t, fa)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx := builderCtxOf(tenant, builder)
	a, _ := e.wf.CreateWorkflow(bctx, tenant, lockedVeto("rule A"))
	b, _ := e.wf.CreateWorkflow(bctx, tenant, lockedVeto("rule B"))
	e.deliver(t, tenant, builder)
	_, _ = e.wf.UpdateWorkflow(bctx, tenant, a.ID, asUpdate(lockedVeto("rule A"), "A edited", true))
	_, _ = e.wf.UpdateWorkflow(bctx, tenant, b.ID, asUpdate(lockedVeto("rule B"), "B edited", false))
	_, _ = e.wf.CreateWorkflow(bctx, tenant, closedWonVeto("rule C"))

	liveBefore := e.snapshotLive(t, tenant)
	draftsBefore := e.draftCount(t, tenant)
	versionsBefore := e.versionCount(t, tenant)
	docsBefore := e.count(t, `SELECT COUNT(*) FROM sys_meta_version_docs WHERE tenant_id = ?`, tenant)
	invBefore := atomic.LoadInt32(e.invalidations)
	assertUntouched := func(step string) {
		t.Helper()
		if got := e.snapshotLive(t, tenant); got != liveBefore {
			t.Fatalf("%s: live rules changed\n before %s\n after  %s", step, liveBefore, got)
		}
		if e.draftCount(t, tenant) != draftsBefore || e.versionCount(t, tenant) != versionsBefore ||
			e.count(t, `SELECT COUNT(*) FROM sys_meta_version_docs WHERE tenant_id = ?`, tenant) != docsBefore {
			t.Fatalf("%s: drafts/versions/docs changed", step)
		}
		if atomic.LoadInt32(e.invalidations) != invBefore {
			t.Fatalf("%s: the engine cache was dropped by a rolled-back publish", step)
		}
	}
	run := func(step string, wantErr string) {
		t.Helper()
		_, err := e.publishWithCurrentHash(bctx, tenant, false)
		if err == nil {
			t.Fatalf("%s: publish succeeded", step)
		}
		if wantErr != "" {
			wantCode(t, err, wantErr)
		}
		assertUntouched(step)
	}
	trigger := func(name, ddl string) {
		t.Helper()
		if _, err := e.sdb.Exec(ddl); err != nil {
			t.Fatalf("create trigger %s: %v", name, err)
		}
	}
	dropTrigger := func(name string) { _, _ = e.sdb.Exec("DROP TRIGGER IF EXISTS " + name) }

	// 1. cross-document validation fails
	fa.problem = &metaversion.Problem{DocType: "workflow", DocKey: a.ID, Code: "INJECTED", Message: "no"}
	run("validate", "PUBLISH_INVALID")
	fa.problem = nil

	// 2. the 2nd of 3 applies fails AFTER its own write
	fa.failApplyAt = atomic.LoadInt32(&fa.applyCalls) + 2
	run("apply 2", "")
	fa.failApplyAt = 0

	// 3. a version-doc insert fails after the first change was applied
	trigger("trg_dp_docs", `CREATE TRIGGER trg_dp_docs BEFORE INSERT ON sys_meta_version_docs FOR EACH ROW
		BEGIN IF NEW.op <> 'snapshot' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected doc failure'; END IF; END`)
	run("version doc insert", "")
	dropTrigger("trg_dp_docs")

	// 4. the version row itself cannot be written
	trigger("trg_dp_ver", `CREATE TRIGGER trg_dp_ver BEFORE INSERT ON sys_meta_versions FOR EACH ROW
		BEGIN IF NEW.kind = 'publish' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected version failure'; END IF; END`)
	run("version insert", "")
	dropTrigger("trg_dp_ver")

	// 5. everything applied and recorded, the draft cleanup fails last
	trigger("trg_dp_drafts", `CREATE TRIGGER trg_dp_drafts BEFORE DELETE ON sys_meta_drafts FOR EACH ROW
		BEGIN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected cleanup failure'; END`)
	run("draft cleanup", "")
	dropTrigger("trg_dp_drafts")

	// The drafts were never lost: with every fault removed the very same set publishes.
	res, err := e.publishWithCurrentHash(bctx, tenant, false)
	if err != nil || res.DocCount != 3 {
		t.Fatalf("publish after the faults were removed: %+v, %v", res, err)
	}
	if atomic.LoadInt32(e.invalidations) == invBefore {
		t.Fatal("the successful publish did not drop the cache")
	}
}

// SC-003: 20 builders publish the same drafts at once ⇒ exactly one version; nothing is applied twice.
func TestPublish_ConcurrentPublishMakesOneVersion_Integration(t *testing.T) {
	e := dpSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx := builderCtxOf(tenant, builder)
	x, _ := e.wf.CreateWorkflow(bctx, tenant, lockedVeto("rule X"))
	e.deliver(t, tenant, builder)
	_, _ = e.wf.UpdateWorkflow(bctx, tenant, x.ID, asUpdate(lockedVeto("rule X"), "X v2", true))
	_, _ = e.wf.CreateWorkflow(bctx, tenant, closedWonVeto("rule Y"))
	_, hash, _ := e.pub.Changes(bctx, tenant)

	const n = 20
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = e.pub.Publish(bctx, tenant, PublishRequest{ChangesetHash: hash})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
			continue
		}
		var ve *ValidationError
		if !errors.As(err, &ve) || (ve.Code != "NOTHING_TO_PUBLISH" && ve.Code != "DRAFTS_CHANGED" && ve.Code != "PUBLISH_BUSY") {
			t.Fatalf("a losing publisher got %v, want NOTHING_TO_PUBLISH / DRAFTS_CHANGED / PUBLISH_BUSY", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d publishes succeeded, want exactly 1", ok)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_meta_versions WHERE tenant_id = ? AND kind = 'publish'`, tenant); n != 1 {
		t.Fatalf("%d publish versions, want 1", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_workflows WHERE tenant_id = ? AND deleted_at IS NULL`, tenant); n != 2 {
		t.Fatalf("%d live rules, want 2 (X + Y once)", n)
	}
	var maxNo, cnt int
	_ = e.sdb.Get(&maxNo, `SELECT MAX(version_no) FROM sys_meta_versions WHERE tenant_id = ?`, tenant)
	_ = e.sdb.Get(&cnt, `SELECT COUNT(DISTINCT version_no) FROM sys_meta_versions WHERE tenant_id = ? AND version_no IS NOT NULL`, tenant)
	if maxNo != 2 || cnt != 2 {
		t.Fatalf("version numbers: max %d distinct %d, want 2 and 2", maxNo, cnt)
	}
}

// stubDocAdapter is a second document type added to the registry AFTER the baseline.
type stubDocAdapter struct{ keys []string }

func (s *stubDocAdapter) Type() string { return "widget" }
func (s *stubDocAdapter) LoadLive(_ context.Context, _ sqlx.QueryerContext, _, key string, _ bool) (json.RawMessage, bool, error) {
	return json.RawMessage(fmt.Sprintf(`{"key":%q}`, key)), true, nil
}
func (s *stubDocAdapter) ListLiveKeys(context.Context, sqlx.QueryerContext, string) ([]metaversion.DocRef, error) {
	out := make([]metaversion.DocRef, len(s.keys))
	for i, k := range s.keys {
		out[i] = metaversion.DocRef{Key: k, Label: k}
	}
	return out, nil
}
func (s *stubDocAdapter) ApplyTx(context.Context, *sqlx.Tx, string, metaversion.Change) error {
	return nil
}
func (s *stubDocAdapter) ValidateFinal(context.Context, metaversion.FinalState) []metaversion.Problem {
	return nil
}
func (s *stubDocAdapter) Redact(d json.RawMessage) json.RawMessage { return d }
func (s *stubDocAdapter) Entities(json.RawMessage) []string        { return nil }

// A type added to the registry after delivery is snapshotted by the next publish, BEFORE it changes,
// and recorded in covered_types (so a later rollback does not mistake it for "never captured").
func TestPublish_TypeAddedAfterDeliveryIsSnapshotted_Integration(t *testing.T) {
	e := dpSetup(t) // registry = workflow only
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx := builderCtxOf(tenant, builder)
	x, _ := e.wf.CreateWorkflow(bctx, tenant, lockedVeto("rule X"))
	e.deliver(t, tenant, builder)
	_, _ = e.wf.UpdateWorkflow(bctx, tenant, x.ID, asUpdate(lockedVeto("rule X"), "X v2", true))

	// "Deploy" a build whose registry also knows a second type.
	reg2 := metaversion.NewRegistry(NewWorkflowDocAdapter(), &stubDocAdapter{keys: []string{"w1", "w2"}})
	drafts2 := metaversion.NewDraftRepo(e.sdb, reg2)
	pub2 := NewPublishService(e.sdb, drafts2, e.vers, reg2, e.gate, nil, nil)
	_, hash, _ := pub2.Changes(bctx, tenant)
	res, err := pub2.Publish(bctx, tenant, PublishRequest{ChangesetHash: hash})
	if err != nil || res.VersionNo != 2 {
		t.Fatalf("publish with a wider registry: %+v, %v", res, err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_meta_version_docs WHERE tenant_id = ? AND doc_type = 'widget' AND op = 'snapshot'`, tenant); n != 2 {
		t.Fatalf("widget snapshots = %d, want 2", n)
	}
	head, _ := e.vers.HeadVersionRead(context.Background(), tenant)
	got, err := e.vers.CoveredTypesUpTo(context.Background(), e.sdb, tenant, head.Seq)
	if err != nil || strings.Join(got, ",") != "widget,workflow" {
		t.Fatalf("covered types = %v, %v; want widget + workflow", got, err)
	}
	// The next publish snapshots nothing again.
	_, _ = e.wf.UpdateWorkflow(bctx, tenant, x.ID, asUpdate(lockedVeto("rule X"), "X v3", true))
	_, hash, _ = pub2.Changes(bctx, tenant)
	if _, err := pub2.Publish(bctx, tenant, PublishRequest{ChangesetHash: hash}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_meta_version_docs WHERE tenant_id = ? AND doc_type = 'widget'`, tenant); n != 2 {
		t.Fatalf("widget snapshots after a second publish = %d, want still 2", n)
	}
}

// FR-024: the final state is validated under the lock — a draft rule pointing at a table that no
// longer exists blocks the publish with PUBLISH_INVALID and nothing is written.
func TestPublish_InvalidFinalState_Integration(t *testing.T) {
	e := dpSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx := builderCtxOf(tenant, builder)
	e.deliver(t, tenant, builder)
	y, err := e.wf.CreateWorkflow(bctx, tenant, closedWonVeto("rule Y"))
	if err != nil {
		t.Fatal(err)
	}
	// The table disappears from the customer's workspace after the draft was saved.
	if _, err := e.sdb.Exec(`UPDATE sys_entities SET deleted_at = NOW() WHERE tenant_id = ? AND name = 'hk_deals'`, tenant); err != nil {
		t.Fatal(err)
	}
	before := e.versionCount(t, tenant)
	_, err = e.publishWithCurrentHash(bctx, tenant, false)
	wantCode(t, err, "PUBLISH_INVALID")
	var ve *ValidationError
	errors.As(err, &ve)
	errs, _ := ve.Details["errors"].([]map[string]any)
	if len(errs) != 1 || errs[0]["docKey"] != y.ID || errs[0]["code"] != "ENTITY_NOT_FOUND" {
		t.Fatalf("details = %v, want one ENTITY_NOT_FOUND on the draft rule", ve.Details)
	}
	if e.versionCount(t, tenant) != before || e.draftCount(t, tenant) != 1 {
		t.Fatal("an invalid publish changed something")
	}
}

// US1 scenario 10 / FR-017: "Tắt ngay cho khách" stops a misbehaving LIVE rule at once (engine cache
// dropped), leaves every draft alone, and the next publish reports a conflict on exactly that rule.
func TestDisableNow_StopsLiveRuleAtOnce_NextPublishConflicts_Integration(t *testing.T) {
	e := dpSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx := builderCtxOf(tenant, builder)
	x, err := e.wf.CreateWorkflow(bctx, tenant, lockedVeto("rule X"))
	if err != nil {
		t.Fatal(err)
	}
	e.deliver(t, tenant, builder)
	hkWantVeto(t, e.createDeal(tenant, "Locked"), dpVetoMsg) // X runs

	// Pending drafts: an edit of X and an unrelated new rule.
	if _, err := e.wf.UpdateWorkflow(bctx, tenant, x.ID, asUpdate(lockedVeto("rule X"), "rule X v2", true)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.wf.CreateWorkflow(bctx, tenant, closedWonVeto("rule Y")); err != nil {
		t.Fatal(err)
	}
	draftsBefore := e.draftCount(t, tenant)
	// Warm the engine cache AFTER the draft saves (they drop it too): only DisableNow's own
	// invalidation may explain the deal saving below.
	hkWantVeto(t, e.createDeal(tenant, "Locked"), dpVetoMsg)

	rec, err := e.wf.DisableNow(bctx, tenant, x.ID, builder)
	if err != nil || rec.IsActive || rec.Name != "rule X" {
		t.Fatalf("DisableNow = %+v, %v; want the live rule X, off, content untouched", rec, err)
	}
	// At once: the loader TTL is an hour, so only the invalidation can explain a deal that now saves.
	if err := e.createDeal(tenant, "Locked"); err != nil {
		t.Fatalf("X was disabled but still vetoes: %v", err)
	}
	if e.draftCount(t, tenant) != draftsBefore {
		t.Fatal("DisableNow changed the drafts")
	}
	// A draft-only rule is not live: 404, nothing written.
	items, _, _ := e.pub.Changes(bctx, tenant)
	var yID string
	for _, it := range items {
		if it.Op == "create" {
			yID = it.DocKey
		}
	}
	_, err = e.wf.DisableNow(bctx, tenant, yID, builder)
	wantCode(t, err, "WORKFLOW_NOT_FOUND")

	// The next publish stops on X (its live document moved under the draft); Y alone is fine.
	_, err = e.publishWithCurrentHash(bctx, tenant, false)
	wantCode(t, err, "PUBLISH_CONFLICT")
	var ve *ValidationError
	errors.As(err, &ve)
	cs, _ := ve.Details["conflicts"].([]map[string]any)
	if len(cs) != 1 || cs[0]["docKey"] != x.ID {
		t.Fatalf("conflicts = %v, want exactly rule X", ve.Details["conflicts"])
	}
	// Another tenant's builder cannot switch it off.
	other := e.newTenant(t)
	_, err = e.wf.DisableNow(builderCtxOf(other, uuid.NewString()), other, x.ID, "x")
	wantCode(t, err, "WORKFLOW_NOT_FOUND")
}
