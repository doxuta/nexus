//go:build integration

// DAILY-QUEUE #171 (`pack-upgrade-workflow-reconcile`) against real MySQL. A tenant that installed
// Ticketing 1.7.0 and then re-installs 1.8.0 gets due_date_auto and the four "Tính lại hạn SLA"
// rules, but the four "Đặt hạn SLA — phiếu …" rules are skipped by name — they keep the 1.7.0
// config without stampField, so no ticket is ever stamped and a priority change never recomputes.
// Before this slice nothing said so. Now the dry-run plan and the install result name exactly those
// rules and where they differ; following the sentence (delete the stale rules, re-install) brings
// the #81 behaviour to the tenant.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=true&multiStatements=true&loc=Local&clientFoundRows=true" \
//	  go test -tags=integration -count=1 -run 'TicketingPack_Reinstall' ./internal/services/vertical_pack/
package vertical_pack

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/services"
)

// ticketing170 rebuilds the 1.7.0 shape of the built-in pack from 1.8.0: no due_date_auto, no
// "Tính lại hạn SLA" rules, and the creation tiers without stampField.
func ticketing170(t *testing.T) *Pack {
	t.Helper()
	pack, ok := BuiltinPack("ticketing")
	if !ok {
		t.Fatal("builtin ticketing pack not found")
	}
	m := *pack.Manifest
	m.Version = "1.7.0"
	pack.Manifest = &m
	for i := range pack.Entities {
		if pack.Entities[i].Name != "tk_tickets" {
			continue
		}
		var kept []PackField
		for _, f := range pack.Entities[i].Fields {
			if f.Name != "due_date_auto" {
				kept = append(kept, f)
			}
		}
		pack.Entities[i].Fields = kept
	}
	var rules []PackWorkflow
	for _, w := range pack.Workflows {
		if strings.HasPrefix(w.Name, "Tính lại hạn SLA") {
			continue
		}
		acts := append([]PackWorkflowAction(nil), w.Actions...)
		for j := range acts {
			acts[j].StampField = ""
		}
		w.Actions = acts
		rules = append(rules, w)
	}
	pack.Workflows = rules
	return pack
}

func TestTicketingPack_ReinstallNamesRulesTheUpgradeSkipped(t *testing.T) {
	gdb, sdb := vpSetup(t)
	st := vpBuildSLA(gdb, sdb)
	defer st.drain(t)
	slaSeedInstaller(t, gdb)
	ctx := context.Background()
	if _, err := services.SeedDefaultBusinessCalendar(ctx, gdb, vpTenantA); err != nil {
		t.Fatalf("seed default business calendar: %v", err)
	}
	if _, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, ticketing170(t)); err != nil {
		t.Fatalf("install ticketing 1.7.0: %v", err)
	}
	v180, _ := BuiltinPack("ticketing")
	sla := []string{"Đặt hạn SLA — phiếu Khẩn", "Đặt hạn SLA — phiếu Cao", "Đặt hạn SLA — phiếu Trung bình", "Đặt hạn SLA — phiếu thường"}
	stale := func(t *testing.T, warnings []string) string {
		t.Helper()
		var out []string
		for _, w := range warnings {
			if strings.Contains(w, "KHÔNG được cập nhật") {
				out = append(out, w)
			}
		}
		if len(out) > 1 {
			t.Fatalf("warnings = %q; want at most one stale-rule sentence", warnings)
		}
		return strings.Join(out, "")
	}

	plan, err := st.pack.DryRunInstall(ctx, vpTenantA, v180)
	if err != nil {
		t.Fatalf("dry run 1.8.0: %v", err)
	}
	got := stale(t, plan.Warnings)
	if !strings.HasPrefix(got, "4 luật tự động đã có sẵn nên KHÔNG được cập nhật, dù khác bản gói 1.8.0") {
		t.Fatalf("plan warning = %q; want the four skipped SLA rules counted against 1.8.0", got)
	}
	for _, name := range sla {
		if !strings.Contains(got, name+" (khác ở actions[0].stampField)") {
			t.Errorf("plan warning %q does not name %q with the path that differs", got, name)
		}
	}
	// Rules the tenant has exactly as 1.8.0 ships them are not noise in that sentence.
	for _, same := range []string{"Định tuyến ticket mới", "Nhắc hạn SLA", "Leo thang ticket quá hạn SLA"} {
		if strings.Contains(got, same) {
			t.Errorf("plan warning %q names %q, which is identical to the pack", got, same)
		}
	}

	res, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, v180)
	if err != nil {
		t.Fatalf("re-install 1.8.0: %v", err)
	}
	if inst := stale(t, res.Warnings); inst != got {
		t.Fatalf("install warned %q, the dry run promised %q", inst, got)
	}

	// Follow the sentence: delete the stale rules, re-install — the pack's own rules land.
	wf := st.pack.workflowWr.(*services.WorkflowService)
	live, err := wf.ListWorkflows(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	for _, r := range live {
		if strings.HasPrefix(r.Name, "Đặt hạn SLA") {
			if err := wf.DeleteWorkflow(ctx, vpTenantA, r.ID); err != nil {
				t.Fatalf("delete %q: %v", r.Name, err)
			}
		}
	}
	res, err = st.pack.Install(ctx, vpTenantA, vpSeedUserID, v180)
	if err != nil {
		t.Fatalf("re-install 1.8.0 after deleting the stale rules: %v", err)
	}
	if w := stale(t, res.Warnings); w != "" {
		t.Fatalf("install still warns %q after the stale rules were replaced", w)
	}

	// The #81 behaviour is live: a new ticket is stamped, and raising its priority tightens it.
	st.restart(t)
	admin := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}
	id := st.raise(t, admin, "Low") // asserts due_date == due_date_auto (the machine's signature)
	before, _, _ := st.ticketDeadline(t, admin, id)
	st.edit(t, admin, id, map[string]any{"priority": "Urgent"})
	after, stamp, _ := st.ticketDeadline(t, admin, id)
	if !after.Before(before) || !stamp.Equal(after) {
		t.Fatalf("Low→Urgent: due %s → %s (stamp %s); want an earlier deadline carried by the stamp", before, after, stamp)
	}
}
