//go:build integration

package services

// publish_rollback_integration_test.go — specs/063-draft-publish S7-7 (T106, T107, T107a) against a
// real MySQL: history v1..v3 with an applied-at-once entry in between, then "Quay lại" and a
// byte-for-byte comparison with the state captured when the target version was written.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/metaversion"
)

type rbEnv struct {
	dpEnv
	rec     *VersionRecorder
	tenant  string
	builder string
	bctx    context.Context
}

func rbSetup(t *testing.T) rbEnv {
	t.Helper()
	d := dpSetupWith(t, NewLayoutDocAdapter(), NewWorkflowDocAdapter(), NewReportDocAdapter(), NewDashboardDocAdapter(), NewBrandingDocAdapter(), NewAppDocAdapter())
	e := rbEnv{dpEnv: d, rec: NewVersionRecorder(d.sdb, d.vers, d.gate, nil), tenant: d.newTenant(t), builder: uuid.NewString()}
	e.bctx = builderCtxOf(e.tenant, e.builder)
	return e
}

func (e rbEnv) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.sdb.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (e rbEnv) report(t *testing.T, name string) string {
	t.Helper()
	id := uuid.NewString()
	e.exec(t, `INSERT INTO sys_reports (id, tenant_id, name, entity_name, type, config, is_active) VALUES (?, ?, ?, 'hk_deals', 'tabular', '{"columns":["name"]}', 1)`, id, e.tenant, name)
	return id
}

func (e rbEnv) app(t *testing.T, name, label string) string {
	t.Helper()
	id := uuid.NewString()
	e.exec(t, `INSERT INTO sys_apps (id, tenant_id, name, label, is_default, sort_order) VALUES (?, ?, ?, ?, 0, 0)`, id, e.tenant, name, label)
	return id
}

// live loads a document through its adapter ("" when absent).
func (e rbEnv) live(t *testing.T, docType, key string) string {
	t.Helper()
	ad, _ := e.reg.Get(docType)
	doc, ok, err := ad.LoadLive(context.Background(), e.sdb, e.tenant, key, false)
	if err != nil {
		t.Fatalf("LoadLive %s/%s: %v", docType, key, err)
	}
	if !ok {
		return ""
	}
	c, err := metaversion.Canonical(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(c)
}

// capture reads every document of the listed keys as the canonical bytes a rollback must reproduce.
func (e rbEnv) capture(t *testing.T, keys map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for k, docType := range keys {
		out[k] = e.live(t, docType, k)
	}
	return out
}

func (e rbEnv) draft(t *testing.T, docType, key, op string, mutate func(m map[string]any)) {
	t.Helper()
	in := metaversion.UpsertInput{DocType: docType, DocKey: key, Op: op, UserID: e.builder, Label: key}
	if op == metaversion.OpUpsert {
		cur := e.live(t, docType, key)
		var m map[string]any
		if cur != "" {
			if err := json.Unmarshal([]byte(cur), &m); err != nil {
				t.Fatal(err)
			}
		} else {
			m = map[string]any{}
		}
		mutate(m)
		b, _ := json.Marshal(m)
		in.Doc = b
	}
	if err := e.drafts.Upsert(context.Background(), e.tenant, in); err != nil {
		t.Fatalf("draft %s/%s: %v", docType, key, err)
	}
}

func (e rbEnv) publish(t *testing.T) *PublishResult {
	t.Helper()
	res, err := e.publishWithCurrentHash(e.bctx, e.tenant, false)
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			t.Fatalf("Publish: %v %v", err, ve.Details)
		}
		t.Fatalf("Publish: %v", err)
	}
	return res
}

func (e rbEnv) versionID(t *testing.T, no uint) string {
	t.Helper()
	var id string
	if err := e.sdb.Get(&id, `SELECT id FROM sys_meta_versions WHERE tenant_id = ? AND version_no = ?`, e.tenant, no); err != nil {
		t.Fatalf("version %d: %v", no, err)
	}
	return id
}

func TestRollback_RestoresStateOfTheTargetVersion_Integration(t *testing.T) {
	e := rbSetup(t)
	// --- live state before delivery
	wf, err := e.wf.CreateWorkflow(e.bctx, e.tenant, lockedVeto("rule one"))
	if err != nil {
		t.Fatal(err)
	}
	r1, r3 := e.report(t, "r1"), e.report(t, "r3")
	a1 := e.app(t, "sales", "Sales")
	e.exec(t, `INSERT INTO sys_app_nav_items (id, tenant_id, app_id, item_type, ref_id, label, sort_order) VALUES (?, ?, ?, 'entity', (SELECT id FROM sys_entities WHERE tenant_id = ? AND name = 'hk_deals' LIMIT 1), 'Deals', 0)`,
		uuid.NewString(), e.tenant, a1, e.tenant)
	e.exec(t, `INSERT INTO sys_layouts (id, tenant_id, entity_name, view_type, layout_json) VALUES (?, ?, 'hk_deals', 'form', '{"sections":[]}')`, uuid.NewString(), e.tenant)
	layoutKey := "hk_deals:form"
	e.exec(t, `UPDATE tenants SET branding = '{"appName":"V1"}' WHERE id = ?`, e.tenant)

	// v1 = baseline
	e.deliver(t, e.tenant, e.builder)

	// a customer admin edits a report AT ONCE (a direct entry between v1 and v2) - must survive a rollback to v2
	before := e.live(t, DocTypeReport, r3)
	e.exec(t, `UPDATE sys_reports SET name = 'r3-by-customer-admin' WHERE tenant_id = ? AND id = ?`, e.tenant, r3)
	e.rec.RecordDirect(legacyCtxOf(e.tenant), e.tenant, "", DirectDoc{DocType: DocTypeReport, DocKey: r3, Label: "r3", Op: "update",
		Before: json.RawMessage(before), After: json.RawMessage(e.live(t, DocTypeReport, r3))})

	// v2 = publish: rule off, a new report, branding
	r2 := uuid.NewString()
	e.draft(t, DocTypeWorkflow, wf.ID, metaversion.OpUpsert, func(m map[string]any) { m["isActive"] = false })
	e.draft(t, DocTypeReport, r2, metaversion.OpUpsert, func(m map[string]any) {
		m["name"], m["entityName"], m["type"], m["config"], m["isActive"] = "r2", "hk_deals", "tabular", map[string]any{"columns": []string{"name"}}, true
	})
	e.draft(t, DocTypeBranding, e.tenant, metaversion.OpUpsert, func(m map[string]any) { m["appName"] = "V2" })
	if res := e.publish(t); res.VersionNo != 2 {
		t.Fatalf("second version = %d", res.VersionNo)
	}
	keys := map[string]string{wf.ID: DocTypeWorkflow, r1: DocTypeReport, r2: DocTypeReport, r3: DocTypeReport, a1: DocTypeApp, layoutKey: DocTypeLayout, e.tenant: DocTypeBranding}
	atV2 := e.capture(t, keys)

	// after v2: an app made at once, then v3 changes everything
	a2 := e.app(t, "extra", "Extra")
	e.rec.RecordDirect(legacyCtxOf(e.tenant), e.tenant, "", DirectDoc{DocType: DocTypeApp, DocKey: a2, Label: "Extra", Op: "create", After: map[string]any{"name": "extra", "label": "Extra"}})
	r4 := uuid.NewString()
	e.draft(t, DocTypeWorkflow, wf.ID, metaversion.OpUpsert, func(m map[string]any) { m["isActive"] = true; m["name"] = "rule one renamed" })
	e.draft(t, DocTypeReport, r1, metaversion.OpDelete, nil)
	e.draft(t, DocTypeReport, r3, metaversion.OpUpsert, func(m map[string]any) { m["name"] = "r3-v3" })
	e.draft(t, DocTypeReport, r4, metaversion.OpUpsert, func(m map[string]any) {
		m["name"], m["entityName"], m["type"], m["config"], m["isActive"] = "r4", "hk_deals", "tabular", map[string]any{"columns": []string{"name"}}, true
	})
	e.draft(t, DocTypeApp, a1, metaversion.OpUpsert, func(m map[string]any) { m["label"] = "Sales v3"; m["nav"] = []any{} })
	e.draft(t, DocTypeLayout, layoutKey, metaversion.OpUpsert, func(m map[string]any) { m["layout"] = map[string]any{"sections": []any{"x"}} })
	e.draft(t, DocTypeBranding, e.tenant, metaversion.OpUpsert, func(m map[string]any) { m["appName"] = "V3" })
	if res := e.publish(t); res.VersionNo != 3 {
		t.Fatalf("third version = %d", res.VersionNo)
	}
	// the customer admin deleted the app A1 at once after v3, the way DeleteApp does (tombstone name)
	keys[r4], keys[a2] = DocTypeReport, DocTypeApp
	atV3 := e.capture(t, keys)
	if atV3[r3] == atV2[r3] || atV3[r1] != "" {
		t.Fatal("setup: v3 must differ from v2")
	}

	// --- plan: names the changes, no body
	v2 := e.versionID(t, 2)
	plan, err := e.pub.RollbackPlan(e.bctx, e.tenant, v2)
	if err != nil {
		t.Fatalf("RollbackPlan: %v", err)
	}
	if plan.TargetVersionNo != 2 || plan.HeadVersionNo != 3 || len(plan.UntouchedTypes) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	got := map[string]string{}
	for _, c := range plan.Changes {
		got[c.DocType+"|"+c.DocKey] = c.Op
	}
	for k, want := range map[string]string{"report|" + r1: "create", "report|" + r4: "delete", "report|" + r3: "update", "workflow|" + wf.ID: "update",
		"app|" + a1: "update", "app|" + a2: "delete", "layout|" + layoutKey: "update", "branding|" + e.tenant: "update"} {
		if got[k] != want {
			t.Fatalf("plan change %s = %q, want %q (all: %v)", k, got[k], want, got)
		}
	}
	if _, ok := got["report|"+r2]; ok {
		t.Fatalf("r2 is unchanged since v2 yet listed: %v", got)
	}

	// --- rollback to v2
	res, err := e.pub.Rollback(e.bctx, e.tenant, v2, RollbackRequest{ExpectedHeadVersion: 3, Note: "back to 2"})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if res.VersionNo != 4 || res.Kind != metaversion.KindRollback {
		t.Fatalf("rollback result = %+v", res)
	}
	for k, docType := range keys {
		if now := e.live(t, docType, k); now != atV2[k] {
			t.Fatalf("%s %s after rollback:\n got %s\nwant %s", docType, k, now, atV2[k])
		}
	}
	// the customer admin's direct edit made BEFORE v2 is part of v2 and is kept (US8 #6)
	if e.live(t, DocTypeReport, r3) == "" || !strings.Contains(e.live(t, DocTypeReport, r3), "r3-by-customer-admin") {
		t.Fatalf("direct edit before the target was lost: %s", e.live(t, DocTypeReport, r3))
	}
	// same ids come back; the tombstone suffix is gone from the restored report row and the app created after v2 is tombstoned
	if n := e.count(t, `SELECT COUNT(*) FROM sys_reports WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, e.tenant, r1); n != 1 {
		t.Fatal("r1 did not come back under its own id")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_apps WHERE tenant_id = ? AND id = ? AND deleted_at IS NOT NULL AND name LIKE 'extra\_\_deleted\_%'`, e.tenant, a2); n != 1 {
		t.Fatal("the app made after v2 is not soft-deleted with the tombstone name")
	}
	var kind string
	var of *uint
	if err := e.sdb.QueryRow(`SELECT kind, rollback_of FROM sys_meta_versions WHERE tenant_id = ? AND version_no = 4`, e.tenant).Scan(&kind, &of); err != nil || kind != "rollback" || of == nil || *of != 2 {
		t.Fatalf("version 4 = kind %q rollback_of %v (%v)", kind, of, err)
	}
	// the new version records live as "before": rolling forward is a rollback to v3 and reproduces it
	if _, err := e.pub.Rollback(e.bctx, e.tenant, e.versionID(t, 3), RollbackRequest{ExpectedHeadVersion: 4}); err != nil {
		t.Fatalf("roll forward: %v", err)
	}
	for _, k := range []string{r1, r3, r4, wf.ID, layoutKey, e.tenant} {
		if now := e.live(t, keys[k], k); now != atV3[k] {
			t.Fatalf("%s after rolling forward:\n got %s\nwant %s", keys[k], now, atV3[k])
		}
	}
}

// A live document changed by SQL outside the history (a whole-customer restore): the plan and the
// version's "before" come from the LIVE document, not from any version's after_json.
func TestRollback_OutsideHistoryEditIsComparedWithLive_Integration(t *testing.T) {
	e := rbSetup(t)
	r1 := e.report(t, "r1")
	e.deliver(t, e.tenant, e.builder)
	e.draft(t, DocTypeReport, r1, metaversion.OpUpsert, func(m map[string]any) { m["name"] = "r1-v2" })
	e.publish(t)
	e.exec(t, `UPDATE sys_reports SET name = 'r1-restored-from-backup' WHERE tenant_id = ? AND id = ?`, e.tenant, r1)
	liveNow := e.live(t, DocTypeReport, r1)

	if _, err := e.pub.Rollback(e.bctx, e.tenant, e.versionID(t, 1), RollbackRequest{ExpectedHeadVersion: 2}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	var before string
	if err := e.sdb.Get(&before, `SELECT CAST(d.before_json AS CHAR) FROM sys_meta_version_docs d JOIN sys_meta_versions v ON v.id = d.version_id
		WHERE d.tenant_id = ? AND v.version_no = 3 AND d.doc_key = ?`, e.tenant, r1); err != nil {
		t.Fatal(err)
	}
	if c, _ := metaversion.Canonical([]byte(before)); string(c) != liveNow {
		t.Fatalf("before_json of the rollback version = %s, want the live document %s", c, liveNow)
	}
}

func TestRollback_Errors_Integration(t *testing.T) {
	e := rbSetup(t)
	r1 := e.report(t, "r1")
	e.deliver(t, e.tenant, e.builder)
	e.draft(t, DocTypeReport, r1, metaversion.OpUpsert, func(m map[string]any) { m["name"] = "r1-v2" })
	e.publish(t)
	v1 := e.versionID(t, 1)
	snap := e.dump(t, e.tenant)

	t.Run("drafts pending", func(t *testing.T) {
		e.draft(t, DocTypeReport, r1, metaversion.OpUpsert, func(m map[string]any) { m["name"] = "pending" })
		_, err := e.pub.Rollback(e.bctx, e.tenant, v1, RollbackRequest{ExpectedHeadVersion: 2})
		wantCode(t, err, "DRAFTS_PENDING")
		if _, err := e.drafts.DeleteByKey(context.Background(), e.tenant, DocTypeReport, r1); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("head moved", func(t *testing.T) {
		_, err := e.pub.Rollback(e.bctx, e.tenant, v1, RollbackRequest{ExpectedHeadVersion: 1})
		wantCode(t, err, "HEAD_MOVED")
	})
	t.Run("direct entry is a revert", func(t *testing.T) {
		e.rec.RecordDirect(legacyCtxOf(e.tenant), e.tenant, "", DirectDoc{DocType: DocTypeReport, DocKey: r1, Label: "r1", Op: "update", After: map[string]any{"name": "x"}})
		var id string
		if err := e.sdb.Get(&id, `SELECT id FROM sys_meta_versions WHERE tenant_id = ? AND kind = 'direct'`, e.tenant); err != nil {
			t.Fatal(err)
		}
		_, err := e.pub.Rollback(e.bctx, e.tenant, id, RollbackRequest{ExpectedHeadVersion: 2})
		wantCode(t, err, "ROLLBACK_USE_REVERT")
		_, err = e.pub.RollbackPlan(e.bctx, e.tenant, id)
		wantCode(t, err, "ROLLBACK_USE_REVERT")
	})
	t.Run("no snapshot of a type", func(t *testing.T) {
		// pretend the baseline predates the report type: the plan cannot know how reports looked at v1
		e.exec(t, `UPDATE sys_meta_versions SET covered_types = '["workflow"]' WHERE tenant_id = ? AND version_no = 1`, e.tenant)
		_, err := e.pub.Rollback(e.bctx, e.tenant, v1, RollbackRequest{ExpectedHeadVersion: 2})
		wantCode(t, err, "ROLLBACK_NO_SNAPSHOT")
		e.exec(t, `UPDATE sys_meta_versions SET covered_types = '["layout","workflow","report","dashboard","branding","app"]' WHERE tenant_id = ? AND version_no = 1`, e.tenant)
	})
	if e.dump(t, e.tenant) != snap {
		t.Fatal("a refused rollback changed the live documents")
	}
	if n := e.versionCount(t, e.tenant); n != 3 { // baseline, publish, the direct entry
		t.Fatalf("versions = %d, a refused rollback must write none", n)
	}
}

func TestRollback_NameTaken_Integration(t *testing.T) {
	e := rbSetup(t)
	a1 := e.app(t, "crm", "CRM")
	e.deliver(t, e.tenant, e.builder)
	// the customer deleted the app at once (tombstone name) and made another one under the same name
	e.exec(t, `UPDATE sys_apps SET deleted_at = NOW(3), name = CONCAT(name, '__deleted_', LEFT(id, 8)) WHERE tenant_id = ? AND id = ?`, e.tenant, a1)
	e.exec(t, `DELETE FROM sys_app_nav_items WHERE tenant_id = ? AND app_id = ?`, e.tenant, a1)
	e.rec.RecordDirect(legacyCtxOf(e.tenant), e.tenant, "", DirectDoc{DocType: DocTypeApp, DocKey: a1, Label: "CRM", Op: "delete", Before: map[string]any{"name": "crm", "label": "CRM"}})
	e.app(t, "crm", "CRM two")
	e.draft(t, DocTypeBranding, e.tenant, metaversion.OpUpsert, func(m map[string]any) { m["appName"] = "later" })
	e.publish(t) // v2, so the head is a numbered version after the deletion

	before := e.dump(t, e.tenant)
	versions := e.versionCount(t, e.tenant)
	_, err := e.pub.Rollback(e.bctx, e.tenant, e.versionID(t, 1), RollbackRequest{ExpectedHeadVersion: 2})
	wantCode(t, err, "ROLLBACK_NAME_TAKEN")
	if e.dump(t, e.tenant) != before || e.versionCount(t, e.tenant) != versions {
		t.Fatal("ROLLBACK_NAME_TAKEN must change nothing")
	}
	// free the name: the app comes back under its own id with the plain name
	e.exec(t, `UPDATE sys_apps SET deleted_at = NOW(3) WHERE tenant_id = ? AND name = 'crm'`, e.tenant)
	e.exec(t, `UPDATE sys_apps SET name = 'crm-two-gone' WHERE tenant_id = ? AND label = 'CRM two'`, e.tenant)
	if _, err := e.pub.Rollback(e.bctx, e.tenant, e.versionID(t, 1), RollbackRequest{ExpectedHeadVersion: 2}); err != nil {
		t.Fatalf("Rollback after freeing the name: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_apps WHERE tenant_id = ? AND id = ? AND name = 'crm' AND deleted_at IS NULL`, e.tenant, a1); n != 1 {
		t.Fatal("deleted app did not come back as 'crm' under its own id")
	}
}

// FR-081: a version id of another customer is the same 404 as an unknown one; nothing is written at either.
func TestRollback_CrossTenant_Integration(t *testing.T) {
	a, b := rbSetup(t), rbSetup(t)
	rb := b.report(t, "b-report")
	b.deliver(t, b.tenant, b.builder)
	b.draft(t, DocTypeReport, rb, metaversion.OpUpsert, func(m map[string]any) { m["name"] = "b-v2" })
	b.publish(t)
	ra := a.report(t, "a-report")
	a.deliver(t, a.tenant, a.builder)
	a.draft(t, DocTypeReport, ra, metaversion.OpUpsert, func(m map[string]any) { m["name"] = "a-v2" })
	a.publish(t)

	foreign := b.versionID(t, 1)
	unknown := uuid.NewString()
	snapA, snapB := a.dump(t, a.tenant), b.dump(t, b.tenant)
	versA, versB := a.versionCount(t, a.tenant), b.versionCount(t, b.tenant)
	for _, id := range []string{foreign, unknown, "not-a-uuid"} {
		_, perr := a.pub.RollbackPlan(a.bctx, a.tenant, id)
		_, rerr := a.pub.Rollback(a.bctx, a.tenant, id, RollbackRequest{ExpectedHeadVersion: 2})
		wantCode(t, perr, "VERSION_NOT_FOUND")
		wantCode(t, rerr, "VERSION_NOT_FOUND")
		if perr.Error() != rerr.Error() {
			t.Fatalf("plan and rollback answer differently: %q vs %q", perr, rerr)
		}
	}
	if a.dump(t, a.tenant) != snapA || b.dump(t, b.tenant) != snapB || a.versionCount(t, a.tenant) != versA || b.versionCount(t, b.tenant) != versB {
		t.Fatal("a cross-tenant rollback attempt wrote something")
	}
}

// dump is every report, app and the branding of the tenant as plain text: "nothing changed" is a string compare.
func (e rbEnv) dump(t *testing.T, tenant string) string {
	t.Helper()
	var rows []string
	for _, q := range []string{
		`SELECT CONCAT_WS('|', 'report', id, name, entity_name, type, CAST(config AS CHAR), is_active, IFNULL(deleted_at, '')) FROM sys_reports WHERE tenant_id = ? ORDER BY id`,
		`SELECT CONCAT_WS('|', 'app', id, name, label, IFNULL(deleted_at, '')) FROM sys_apps WHERE tenant_id = ? ORDER BY id`,
		`SELECT CONCAT_WS('|', 'branding', IFNULL(CAST(branding AS CHAR), '')) FROM tenants WHERE id = ?`,
	} {
		var part []string
		if err := e.sdb.Select(&part, q, tenant); err != nil {
			t.Fatalf("dump: %v", err)
		}
		rows = append(rows, part...)
	}
	return strings.Join(rows, "\n")
}

// FR-051: what the builder saw is what gets written. An applied-at-once change that lands between the
// preview and the confirmation moves the plan token, and Rollback refuses with PLAN_CHANGED.
func TestRollback_PlanTokenGuardsTheWarnings_Integration(t *testing.T) {
	e := rbSetup(t)
	wf, err := e.wf.CreateWorkflow(e.bctx, e.tenant, lockedVeto("rule one"))
	if err != nil {
		t.Fatal(err)
	}
	e.deliver(t, e.tenant, e.builder)
	e.draft(t, DocTypeWorkflow, wf.ID, metaversion.OpUpsert, func(m map[string]any) { m["isActive"] = false })
	e.publish(t) // v2: rule off, so rolling back to v1 turns it on
	v1 := e.versionID(t, 1)
	plan, err := e.pub.RollbackPlan(e.bctx, e.tenant, v1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PlanToken == "" || len(plan.Reactivates) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	// a customer admin touches the rule at once after the builder opened the Modal
	e.rec.RecordDirect(legacyCtxOf(e.tenant), e.tenant, "", DirectDoc{DocType: DocTypeWorkflow, DocKey: wf.ID, Label: "rule one", Op: "update",
		Before: json.RawMessage(e.live(t, DocTypeWorkflow, wf.ID)), After: json.RawMessage(e.live(t, DocTypeWorkflow, wf.ID))})
	before := e.dump(t, e.tenant)
	_, err = e.pub.Rollback(e.bctx, e.tenant, v1, RollbackRequest{ExpectedHeadVersion: 2, ExpectedPlanToken: plan.PlanToken})
	wantCode(t, err, "PLAN_CHANGED")
	if e.dump(t, e.tenant) != before {
		t.Fatal("PLAN_CHANGED changed the live documents")
	}
	fresh, err := e.pub.RollbackPlan(e.bctx, e.tenant, v1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pub.Rollback(e.bctx, e.tenant, v1, RollbackRequest{ExpectedHeadVersion: 2, ExpectedPlanToken: fresh.PlanToken}); err != nil {
		t.Fatalf("Rollback with the fresh token: %v", err)
	}
}
