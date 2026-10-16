package services

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/workflow"
)

// specs/063 T105: the rollback planner is pure - history rows and a live reader in, changes and the
// warnings of the Modal out.

type rbLive map[string]json.RawMessage // "type|key" -> live doc

func (f rbLive) load(docType, key string) (json.RawMessage, bool, error) {
	d, ok := f[docType+"|"+key]
	return d, ok, nil
}

func rbInput(live rbLive, target, after []metaversion.HistoryDoc, covered ...string) rollbackInput {
	return rollbackInput{
		Types:    []string{DocTypeLayout, DocTypeWorkflow, DocTypeReport, DocTypeDashboard, DocTypeBranding, DocTypeApp},
		Covered:  covered,
		Target:   target,
		After:    after,
		CallerID: "builder-1",
		Live:     live.load,
		Redact:   func(_ string, d json.RawMessage) json.RawMessage { return d },
		TargetAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
	}
}

func hd(docType, key, op, kind string, seq uint, after string) metaversion.HistoryDoc {
	d := metaversion.HistoryDoc{DocType: docType, DocKey: key, Label: key, Op: op, Kind: kind, Seq: seq, CreatedBy: "builder-1", ByLabel: "Builder"}
	if after != "" {
		d.AfterJSON = json.RawMessage(after)
	}
	return d
}

func opsOf(p *rollbackPlan) map[string]string {
	m := map[string]string{}
	for _, c := range p.View.Changes {
		m[c.DocType+"|"+c.DocKey] = c.Op
	}
	return m
}

const (
	wfOn      = `{"name":"r","entityName":"e","trigger":"after_create","condition":null,"actions":[],"isActive":true,"intervalSeconds":0}`
	wfOff     = `{"name":"r","entityName":"e","trigger":"after_create","condition":null,"actions":[],"isActive":false,"intervalSeconds":0}`
	wfHook    = `{"name":"r","entityName":"e","trigger":"after_create","condition":null,"actions":[{"type":"outbound_webhook","url":"https://x.test","secret":"OLD-KEY"}],"isActive":true,"intervalSeconds":0}`
	wfHookNew = `{"name":"r","entityName":"e","trigger":"after_create","condition":null,"actions":[{"type":"outbound_webhook","url":"https://x.test","secret":"NEW-KEY"}],"isActive":false,"intervalSeconds":0}`
)

func TestPlanRollback_TargetStateCases(t *testing.T) {
	all := []string{DocTypeLayout, DocTypeWorkflow, DocTypeReport, DocTypeDashboard, DocTypeBranding, DocTypeApp}
	tests := []struct {
		name   string
		live   rbLive
		target []metaversion.HistoryDoc
		after  []metaversion.HistoryDoc
		want   map[string]string
	}{
		{"edited after N is put back", rbLive{"workflow|w1": json.RawMessage(wfOff)},
			[]metaversion.HistoryDoc{hd("workflow", "w1", "snapshot", "baseline", 1, wfOn)},
			[]metaversion.HistoryDoc{hd("workflow", "w1", "update", "publish", 3, wfOff)},
			map[string]string{"workflow|w1": "update"}},
		{"created after N is deleted", rbLive{"dashboard|d9": json.RawMessage(`{"name":"d9"}`)},
			nil,
			[]metaversion.HistoryDoc{hd("dashboard", "d9", "create", "publish", 3, "")},
			map[string]string{"dashboard|d9": "delete"}},
		{"deleted after N comes back", rbLive{},
			[]metaversion.HistoryDoc{hd("report", "r1", "snapshot", "baseline", 1, `{"name":"r1"}`)},
			[]metaversion.HistoryDoc{hd("report", "r1", "delete", "publish", 3, "")},
			map[string]string{"report|r1": "create"}},
		{"unchanged since N is no change", rbLive{"report|r1": json.RawMessage(`{"name":"r1"}`)},
			[]metaversion.HistoryDoc{hd("report", "r1", "snapshot", "baseline", 1, `{"name":"r1"}`)},
			nil,
			map[string]string{}},
		{"target deleted and still deleted", rbLive{},
			[]metaversion.HistoryDoc{hd("report", "r1", "delete", "publish", 2, "")},
			[]metaversion.HistoryDoc{hd("report", "r1", "create", "publish", 3, "")},
			map[string]string{}},
		{"live changed outside history is compared with LIVE, not with any after_json", rbLive{"report|r1": json.RawMessage(`{"name":"edited-by-sql"}`)},
			[]metaversion.HistoryDoc{hd("report", "r1", "snapshot", "baseline", 1, `{"name":"r1"}`)},
			nil,
			map[string]string{"report|r1": "update"}},
		{"branding is never deleted: absent target resets to the empty document", rbLive{"branding|t": json.RawMessage(`{"primaryColor":"#fff"}`)},
			nil,
			[]metaversion.HistoryDoc{hd("branding", "t", "create", "publish", 3, "")},
			map[string]string{"branding|t": "update"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := rbInput(tc.live, tc.target, tc.after, all...)
			p, err := planRollback(in)
			if err != nil {
				t.Fatalf("planRollback: %v", err)
			}
			got := opsOf(p)
			if len(got) != len(tc.want) {
				t.Fatalf("changes = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("changes = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestPlanRollback_DirectEntryBeforeNIsPartOfTheTarget(t *testing.T) {
	// A customer admin edited the report directly (seq 2, before N=3); a later publish changed it again.
	live := rbLive{"report|r1": json.RawMessage(`{"name":"after-publish"}`)}
	target := []metaversion.HistoryDoc{hd("report", "r1", "update", "direct", 2, `{"name":"admin-edit"}`)}
	after := []metaversion.HistoryDoc{hd("report", "r1", "update", "publish", 4, `{"name":"after-publish"}`)}
	p, err := planRollback(rbInput(live, target, after, DocTypeReport))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 1 || string(p.Changes[0].After) != `{"name":"admin-edit"}` {
		t.Fatalf("the admin's direct edit must be the target state, got %+v", p.Changes)
	}
}

func TestPlanRollback_UntouchedTypesComeFromCoveredTypes(t *testing.T) {
	// dashboard was covered by the baseline although the tenant had 0 dashboards then (no snapshot row):
	// a dashboard created after N is DELETED and the type is NOT untouched.
	live := rbLive{"dashboard|d1": json.RawMessage(`{"name":"d1"}`)}
	after := []metaversion.HistoryDoc{hd("dashboard", "d1", "create", "publish", 3, "")}
	p, err := planRollback(rbInput(live, nil, after, DocTypeDashboard, DocTypeReport))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.View.UntouchedTypes) != 0 {
		t.Fatalf("untouchedTypes = %v, want none", p.View.UntouchedTypes)
	}
	if opsOf(p)["dashboard|d1"] != "delete" {
		t.Fatalf("dashboard created after N must be deleted, got %v", opsOf(p))
	}

	// a type the baseline never covered is untouched: left alone, named for the Modal.
	live = rbLive{"layout|e/form": json.RawMessage(`{"entityName":"e","viewType":"form"}`)}
	after = []metaversion.HistoryDoc{hd("layout", "e/form", "create", "publish", 3, "")}
	p, err = planRollback(rbInput(live, nil, after, DocTypeReport))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.View.UntouchedTypes) != 1 || p.View.UntouchedTypes[0] != DocTypeLayout || len(p.Changes) != 0 {
		t.Fatalf("untouched = %v changes = %d, want [layout] and 0", p.View.UntouchedTypes, len(p.Changes))
	}
}

func TestPlanRollback_Warnings(t *testing.T) {
	live := rbLive{"workflow|w1": json.RawMessage(wfHookNew), "workflow|w2": json.RawMessage(wfOff)}
	target := []metaversion.HistoryDoc{
		hd("workflow", "w1", "snapshot", "baseline", 1, wfHook),
		hd("workflow", "w2", "snapshot", "baseline", 1, wfOn),
	}
	after := []metaversion.HistoryDoc{
		hd("workflow", "w1", "update", "publish", 3, wfHookNew),
		func() metaversion.HistoryDoc {
			d := hd("workflow", "w2", "update", "direct", 4, wfOff)
			d.CreatedBy, d.ByLabel, d.At = "customer-admin", "Chủ khách", time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
			return d
		}(),
	}
	p, err := planRollback(rbInput(live, target, after, DocTypeWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.View.SecretRestores) != 1 || p.View.SecretRestores[0].DocKey != "w1" {
		t.Fatalf("secretRestores = %+v, want w1 only", p.View.SecretRestores)
	}
	re := map[string]bool{}
	for _, r := range p.View.Reactivates {
		re[r.DocKey] = r.Outbound
	}
	if len(re) != 2 || !re["w1"] || re["w2"] {
		t.Fatalf("reactivates = %v, want w1 (outbound) and w2 (not outbound)", re)
	}
	if len(p.View.OverridesDirect) != 1 || p.View.OverridesDirect[0].DocKey != "w2" || p.View.OverridesDirect[0].ByLabel != "Chủ khách" {
		t.Fatalf("overridesDirect = %+v, want w2 by Chủ khách", p.View.OverridesDirect)
	}
	// the builder's own direct entry is not an override
	after[1].CreatedBy = "builder-1"
	p, _ = planRollback(rbInput(live, target, after, DocTypeWorkflow))
	if len(p.View.OverridesDirect) != 0 {
		t.Fatalf("own direct entry listed as override: %+v", p.View.OverridesDirect)
	}
}

func TestPlanRollback_DeterministicOrder(t *testing.T) {
	live := rbLive{}
	target := []metaversion.HistoryDoc{
		hd("report", "b", "snapshot", "baseline", 1, `{"name":"b"}`),
		hd("report", "a", "snapshot", "baseline", 1, `{"name":"a"}`),
	}
	p, err := planRollback(rbInput(live, target, nil, DocTypeReport))
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(p.Changes))
	for _, c := range p.Changes {
		keys = append(keys, c.DocKey)
	}
	if !sort.StringsAreSorted(keys) || len(keys) != 2 {
		t.Fatalf("keys = %v", keys)
	}
}

// Builder only, and a malformed id is VERSION_NOT_FOUND before any SQL.
func TestPublishService_RollbackBoundary(t *testing.T) {
	s := nilDepsPublishService()
	const someID = "00000000-0000-0000-0000-000000000001"
	for name, ctx := range map[string]context.Context{
		"no caller": context.Background(), "legacy customer admin": legacyAdminCtx(),
		"builder of another tenant": metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "b", TenantID: "other", IsBuilder: true}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.RollbackPlan(ctx, dwTenant, someID)
			wantCode(t, err, "FORBIDDEN")
			_, err = s.Rollback(ctx, dwTenant, someID, RollbackRequest{ExpectedHeadVersion: 1})
			wantCode(t, err, "FORBIDDEN")
		})
	}
	_, err := s.RollbackPlan(builderCtx(), dwTenant, "not-a-uuid")
	wantCode(t, err, "VERSION_NOT_FOUND")
	_, err = s.Rollback(builderCtx(), dwTenant, "not-a-uuid", RollbackRequest{ExpectedHeadVersion: 1})
	wantCode(t, err, "VERSION_NOT_FOUND")
	_, err = s.Rollback(builderCtx(), dwTenant, someID, RollbackRequest{})
	wantCode(t, err, "INVALID_BODY")
}

// A "direct" workflow entry stores the REDACTED body; rolling back to it must never write it over the
// live rule (signing key, headers, url token) nor rebuild a deleted rule from it.
func TestPlanRollback_DirectWorkflowTargetIsNeverWritten(t *testing.T) {
	const redacted = `{"name":"r","entityName":"e","trigger":"after_create","condition":null,"actions":[{"type":"outbound_webhook","url":"https://erp.test/in?token=••••","headers":{"X-Key":"••••"}}],"isActive":true,"intervalSeconds":0}`
	live := `{"name":"renamed","entityName":"e","trigger":"after_create","condition":null,"actions":[{"type":"outbound_webhook","url":"https://erp.test/in?token=abc","secret":"KEY","headers":{"X-Key":"k"}}],"isActive":true,"intervalSeconds":0}`
	target := []metaversion.HistoryDoc{hd("workflow", "w1", "update", "direct", 2, redacted)}
	for name, l := range map[string]rbLive{"live differs": {"workflow|w1": json.RawMessage(live)}, "live deleted": {}} {
		t.Run(name, func(t *testing.T) {
			in := rbInput(l, target, nil, DocTypeWorkflow)
			in.Redact = func(_ string, d json.RawMessage) json.RawMessage { return workflow.RedactActionsJSON(d) }
			p, err := planRollback(in)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Changes) != 0 {
				t.Fatalf("redacted direct body must not be written: %+v", p.Changes)
			}
			if len(p.View.KeptAsIs) != 1 || p.View.KeptAsIs[0].DocKey != "w1" {
				t.Fatalf("kept-as-is not reported: %+v", p.View.KeptAsIs)
			}
		})
	}
}

func TestRollbackPlanView_TokenMovesWithTheWarnings(t *testing.T) {
	a := RollbackPlanView{Reactivates: []RollbackReactivate{}}
	b := RollbackPlanView{Reactivates: []RollbackReactivate{{DocKey: "w1", Outbound: true}}}
	if a.token() == b.token() || a.token() != a.token() {
		t.Fatal("token must be stable and change when a reactivation appears")
	}
}

// A field / table first recorded by the publish that released it ("create", hidden-new) did not exist for the
// customer at an earlier target. The rollback hides it again through the target's cutoff; it never deletes the
// definition (or sweeps its records) - a field / table delete only ever comes from a delete the history recorded.
func TestPlanRollback_SchemaBornAfterTargetIsNeverDeleted(t *testing.T) {
	in := rbInput(rbLive{"field|f9": json.RawMessage(`{"label":"F9"}`), "entity|e9": json.RawMessage(`{"name":"e9"}`)},
		nil,
		[]metaversion.HistoryDoc{hd("field", "f9", "create", "publish", 3, ""), hd("entity", "e9", "create", "publish", 3, "")},
		DocTypeField, DocTypeEntity)
	in.Types = append(in.Types, DocTypeField, DocTypeEntity)
	p, err := planRollback(in)
	if err != nil {
		t.Fatalf("planRollback: %v", err)
	}
	if len(p.Changes) != 0 || len(p.View.Changes) != 0 {
		t.Fatalf("changes = %v, want none (the target's cutoff hides them; nothing is deleted)", opsOf(p))
	}
}
