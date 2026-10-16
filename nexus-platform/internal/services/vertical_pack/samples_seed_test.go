package vertical_pack

// specs/064-preview-as-role-sample-data S9-6 (T101, pack side): re-seeding the samples of a
// built-in pack already installed in the tenant, with the sample mark, through the seeder the
// sample-data service calls (no new install).

import (
	"context"
	"slices"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

func installedCRM(t *testing.T) (*Service, *markWriter, *fakeSkipLinker, *Pack) {
	t.Helper()
	w, lk, led := &markWriter{}, &fakeSkipLinker{}, &ledgerFake{}
	svc := newMarkTestService(w, lk, led)
	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("crm pack missing")
	}
	led.recs = append(led.recs, InstallRecord{ID: "inst-1", TenantID: "t1", Slug: crm.Manifest.Slug, Version: crm.Manifest.Version,
		Result: map[string]any{"demoSamples": true}})
	return svc, w, lk, crm
}

func sampleEntities(p *Pack) []string {
	var out []string
	for _, s := range p.Samples {
		out = append(out, s.Entity)
	}
	return out
}

func TestPackSampleEntities_OnlyTablesOfAnInstalledBuiltinPack(t *testing.T) {
	svc, _, _, crm := installedCRM(t)
	got, err := svc.PackSampleEntities(context.Background(), "t1", []string{"crm_accounts", "not_in_pack", "crm_contacts"})
	if err != nil {
		t.Fatalf("PackSampleEntities: %v", err)
	}
	if !slices.Equal(got, []string{"crm_accounts", "crm_contacts"}) {
		t.Fatalf("covered = %v, want [crm_accounts crm_contacts]", got)
	}
	// Another tenant installed nothing: nothing is covered (tenant-scoped ledger read).
	other, err := svc.PackSampleEntities(context.Background(), "t2", sampleEntities(crm))
	if err != nil || len(other) != 0 {
		t.Fatalf("tenant t2: covered=%v err=%v, want none", other, err)
	}
}

func TestPackSampleEntities_NonBuiltinSlugIsNotASource(t *testing.T) {
	w, led := &markWriter{}, &ledgerFake{}
	svc := newMarkTestService(w, nil, led)
	led.recs = append(led.recs, InstallRecord{ID: "i", TenantID: "t1", Slug: "my-own-pack", Result: map[string]any{"demoSamples": true}})
	got, err := svc.PackSampleEntities(context.Background(), "t1", []string{"crm_accounts"})
	if err != nil || len(got) != 0 {
		t.Fatalf("covered=%v err=%v, want none for a non-builtin slug", got, err)
	}
}

// An uploaded pack that merely carries a built-in slug (a handover export of the CRM app is named
// "crm") is not the built-in: the ledger row has no demoSamples marker, so it is never a source.
func TestPackSampleEntities_UploadedPackWithBuiltinSlugIsNotASource(t *testing.T) {
	w, led := &markWriter{}, &ledgerFake{}
	svc := newMarkTestService(w, &fakeSkipLinker{}, led)
	crm, _ := BuiltinPack("crm")
	upload := &Pack{Manifest: &Manifest{Slug: crm.Manifest.Slug, Name: "Exported vertical", Version: "1.0.0"}, Entities: skipTestPack().Entities}
	if _, err := svc.Install(context.Background(), "t1", "u1", upload); err != nil {
		t.Fatalf("Install: %v", err)
	}
	got, err := svc.PackSampleEntities(context.Background(), "t1", []string{"crm_accounts"})
	if err != nil || len(got) != 0 {
		t.Fatalf("covered=%v err=%v, want none: the ledger row of an upload carries no demoSamples marker", got, err)
	}
}

// A real built-in install writes the marker the seeder trusts.
func TestInstall_BuiltinInstallLeavesTheDemoSamplesMarkerInTheLedger(t *testing.T) {
	w, led := &markWriter{}, &ledgerFake{}
	svc := newMarkTestService(w, &fakeSkipLinker{}, led)
	pack := skipTestPack()
	pack.DemoSamples = true
	pack.Manifest = &Manifest{Slug: "crm", Name: "CRM", Version: "1.0.0"}
	if _, err := svc.Install(context.Background(), "t1", "u1", pack); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(led.recs) != 1 || led.recs[0].Result["demoSamples"] != true {
		t.Fatalf("ledger = %+v, want one row with result.demoSamples=true", led.recs)
	}
}

func TestSeedPackSamples_AllTablesResolveRefsMarkAndLink(t *testing.T) {
	svc, w, lk, crm := installedCRM(t)
	res, err := svc.SeedPackSamples(context.Background(), "t1", "u1", "batch-1", sampleEntities(crm))
	if err != nil {
		t.Fatalf("SeedPackSamples: %v", err)
	}
	want := 0
	for _, s := range crm.Samples {
		want += len(s.Records)
	}
	if len(w.rows) != want {
		t.Fatalf("created %d rows, want %d (every sample row of the pack)", len(w.rows), want)
	}
	created := 0
	for _, c := range res.Created {
		created += c.Count
	}
	if created != want || len(res.Failed) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("result created=%d failed=%v skipped=%v, want %d/none/none", created, res.Failed, res.Skipped, want)
	}
	ids := map[string]string{} // account name -> minted id
	for _, r := range w.rows {
		if !r.marked || r.mark.Source != dynrepo.SampleSourcePack || r.mark.BatchID != "batch-1" {
			t.Fatalf("row %s/%s mark = %+v marked=%v, want pack/batch-1", r.entity, r.id, r.mark, r.marked)
		}
		if r.entity == "crm_accounts" {
			ids[r.data["name"].(string)] = r.id
		}
	}
	for _, r := range w.rows {
		if r.entity == "crm_contacts" && r.data["first_name"] == "Alice" && r.data["account"] != ids["Acme Corp"] {
			t.Errorf("Alice.account = %v, want the minted id of Acme %q (@ref resolved)", r.data["account"], ids["Acme Corp"])
		}
	}
	if len(lk.calls) != len(crm.Links) {
		t.Errorf("links created = %d, want %d (N-N rows between the re-seeded samples)", len(lk.calls), len(crm.Links))
	}
}

// Contacts alone reference accounts that are not part of this run: nothing can be resolved, so the
// table is reported as skipped for its missing parents and no half-linked row is written.
func TestSeedPackSamples_ChildOnlyIsSkippedForItsParents(t *testing.T) {
	svc, w, _, _ := installedCRM(t)
	res, err := svc.SeedPackSamples(context.Background(), "t1", "u1", "b", []string{"crm_contacts"})
	if err != nil {
		t.Fatalf("SeedPackSamples: %v", err)
	}
	if len(w.rows) != 0 {
		t.Fatalf("wrote %d rows, want 0", len(w.rows))
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != (services.SampleSkipped{Entity: "crm_contacts", Code: "SAMPLE_PARENT_EMPTY"}) {
		t.Fatalf("skipped = %+v, want crm_contacts SAMPLE_PARENT_EMPTY", res.Skipped)
	}
}

// A row the record layer refuses is reported, not fatal: the rest of the batch still lands (Install
// aborts instead, because it has a schema to roll back).
func TestSeedPackSamples_RefusedRowIsReportedAndBatchContinues(t *testing.T) {
	svc, w, _, _ := installedCRM(t)
	w.failName = "Globex Inc"
	res, err := svc.SeedPackSamples(context.Background(), "t1", "u1", "b", []string{"crm_accounts"})
	if err != nil {
		t.Fatalf("SeedPackSamples: %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Entity != "crm_accounts" || res.Failed[0].Code != "RECORD_REQUIRED_MISSING" || res.Failed[0].Index != 1 {
		t.Fatalf("failed = %+v, want one crm_accounts row 1 RECORD_REQUIRED_MISSING", res.Failed)
	}
	if len(res.Created) != 1 || res.Created[0].Count != 3 {
		t.Fatalf("created = %+v, want crm_accounts x3", res.Created)
	}
}

// Why a table came back empty must be named truthfully: a duplicate, a tenant rule's veto, a parent
// the record layer refused and a parent outside the run are four different stories.
func TestSeedPackSamples_SkipReasonIsClassified(t *testing.T) {
	cases := []struct {
		name       string
		failEntity string
		failKind   string
		entities   []string
		table      string
		want       []string // skip codes reported for table
		wantFailed bool
	}{
		{"every row a duplicate", "crm_accounts", "dup", []string{"crm_accounts"}, "crm_accounts", []string{"SAMPLE_ALREADY_PRESENT"}, false},
		{"every row vetoed by a tenant rule", "crm_accounts", "veto", []string{"crm_accounts"}, "crm_accounts", []string{"SAMPLE_VETOED"}, false},
		{"contacts of vetoed accounts", "crm_accounts", "veto", []string{"crm_accounts", "crm_contacts"}, "crm_contacts", []string{"SAMPLE_PARENT_SKIPPED"}, false},
		{"contacts of refused accounts", "crm_accounts", "", []string{"crm_accounts", "crm_contacts"}, "crm_contacts", []string{"SAMPLE_PARENT_SKIPPED"}, true},
		{"parent outside the run", "", "", []string{"crm_contacts"}, "crm_contacts", []string{"SAMPLE_PARENT_EMPTY"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, w, _, _ := installedCRM(t)
			w.failEntity, w.failKind = tc.failEntity, tc.failKind
			res, err := svc.SeedPackSamples(context.Background(), "t1", "u1", "b", tc.entities)
			if err != nil {
				t.Fatalf("SeedPackSamples: %v", err)
			}
			var got []string
			for _, s := range res.Skipped {
				if s.Entity == tc.table {
					got = append(got, s.Code)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("skip codes of %s = %v, want %v (skipped %+v, failed %+v)", tc.table, got, tc.want, res.Skipped, res.Failed)
			}
			if tc.wantFailed != (len(res.Failed) > 0) {
				t.Fatalf("failed = %+v, want failed lines: %v", res.Failed, tc.wantFailed)
			}
		})
	}
}

// One account vetoed, the rest created: the table is not silent about the veto.
func TestSeedPackSamples_PartialVetoIsReported(t *testing.T) {
	svc, w, _, _ := installedCRM(t)
	w.failName, w.failKind = "Globex Inc", "veto"
	res, err := svc.SeedPackSamples(context.Background(), "t1", "u1", "b", []string{"crm_accounts"})
	if err != nil {
		t.Fatalf("SeedPackSamples: %v", err)
	}
	if len(res.Created) != 1 || res.Created[0].Count != 3 {
		t.Fatalf("created = %+v, want crm_accounts x3", res.Created)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != (services.SampleSkipped{Entity: "crm_accounts", Code: "SAMPLE_VETOED"}) {
		t.Fatalf("skipped = %+v, want crm_accounts SAMPLE_VETOED beside the 3 created rows", res.Skipped)
	}
}

// An infrastructure error (not a ValidationError) aborts the run with the wrapped error: it is not
// a per-row "failed" line (raw driver text must not reach the client, and a DB outage must not
// look like a 201 "created N, failed M").
func TestSeedPackSamples_InfrastructureErrorAbortsAndDoesNotLeak(t *testing.T) {
	svc, w, _, _ := installedCRM(t)
	w.failName, w.failKind = "Globex Inc", "infra"
	res, err := svc.SeedPackSamples(context.Background(), "t1", "u1", "b", []string{"crm_accounts"})
	if err == nil {
		t.Fatalf("err = nil, want the infrastructure error returned (res=%+v)", res)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("failed = %+v, want no per-row line carrying raw error text", res.Failed)
	}
	if len(w.rows) != 1 {
		t.Fatalf("wrote %d rows, want 1: the run stops at the failing row instead of hammering the DB", len(w.rows))
	}
}
