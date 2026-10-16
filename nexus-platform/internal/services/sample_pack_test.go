package services

// sample_pack_test.go — specs/064-preview-as-role-sample-data S9-6 (T101, service side):
// Generate with source "pack" / "auto" hands the tables a built-in pack covers to the pack
// seeder, refuses a table that already holds pack samples, and (auto) generates the rest from
// field metadata.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// fakePackSeeder covers the tables in covered and records what it was asked to seed.
type fakePackSeeder struct {
	covered   []string
	seeded    []string
	batch     string
	result    PackSeedResult
	seedErr   error
	seedCalls int
}

func (f *fakePackSeeder) PackSampleEntities(_ context.Context, _ string, entities []string) ([]string, error) {
	var out []string
	for _, e := range entities {
		if slices.Contains(f.covered, e) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakePackSeeder) SeedPackSamples(_ context.Context, _, _, batch string, entities []string) (PackSeedResult, error) {
	f.seedCalls++
	f.seeded, f.batch = entities, batch
	return f.result, f.seedErr
}

func newPackGenSvc(store *genStore, w *genWriter, seeder *fakePackSeeder) (*SampleDataService, *spyAuditor) {
	svc, aud := newGenSvc(threeTableApp(), store, w)
	return svc.WithPackSeeder(seeder), aud
}

func TestGenerate_PackSource_SeedsCoveredTablesThroughThePackSeeder(t *testing.T) {
	seeder := &fakePackSeeder{covered: []string{"customers", "appointments"}, result: PackSeedResult{
		Created: []SampleCreated{{Entity: "customers", Count: 4}, {Entity: "appointments", Count: 6}},
	}}
	w := &genWriter{}
	svc, aud := newPackGenSvc(&genStore{}, w, seeder)

	res, err := svc.Generate(context.Background(), genScope, "t1", "u1",
		GenerateRequest{Entities: []string{"customers", "appointments"}, PerEntity: 5, Source: "pack"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !slices.Equal(seeder.seeded, []string{"customers", "appointments"}) || seeder.batch == "" || seeder.batch != res.BatchID {
		t.Fatalf("seeder got entities=%v batch=%q, want both tables and the result's batch %q", seeder.seeded, seeder.batch, res.BatchID)
	}
	if len(w.calls) != 0 {
		t.Fatalf("field generator ran %d times for source=pack, want 0", len(w.calls))
	}
	if len(res.Created) != 2 || len(res.Skipped) != 0 || len(res.Failed) != 0 || res.AddedParents == nil {
		t.Fatalf("result = %+v, want the seeder's two created lines and non-nil arrays", res)
	}
	if len(aud.events) != 1 || aud.events[0].Diff.After["source"] != "pack" {
		t.Fatalf("audit = %+v, want one __sample_data__ line with source pack", aud.events)
	}
}

func TestGenerate_PackSource_TableAlreadyHoldingPackSamplesIsSkipped(t *testing.T) {
	seeder := &fakePackSeeder{covered: []string{"customers", "appointments"}, result: PackSeedResult{
		Created: []SampleCreated{{Entity: "appointments", Count: 3}},
	}}
	store := &genStore{fakeSampleStore: fakeSampleStore{counts: map[string]dynrepo.SampleCounts{
		"t1|customers": {Live: 4, BySource: map[string]int{"pack": 4}},
	}}}
	svc, _ := newPackGenSvc(store, &genWriter{}, seeder)

	res, err := svc.Generate(context.Background(), genScope, "t1", "u1",
		GenerateRequest{Entities: []string{"customers", "appointments"}, PerEntity: 5, Source: "pack"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !slices.Equal(seeder.seeded, []string{"appointments"}) {
		t.Fatalf("seeded %v, want only appointments", seeder.seeded)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Entity != "customers" || res.Skipped[0].Code != "SAMPLE_ALREADY_PRESENT" {
		t.Fatalf("skipped = %+v, want customers SAMPLE_ALREADY_PRESENT", res.Skipped)
	}
}

func TestGenerate_PackSource_NothingCoveredIsPackUnavailable(t *testing.T) {
	tests := []struct {
		name   string
		seeder *fakePackSeeder
	}{
		{"no table of the pack", &fakePackSeeder{covered: []string{"somewhere_else"}}},
		{"no seeder wired", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &genWriter{}
			svc, _ := newGenSvc(threeTableApp(), &genStore{}, w)
			if tc.seeder != nil {
				svc.WithPackSeeder(tc.seeder)
			}
			_, err := svc.Generate(context.Background(), genScope, "t1", "u1",
				GenerateRequest{Entities: []string{"customers"}, PerEntity: 5, Source: "pack"})
			if !codeIs(err, "SAMPLE_PACK_UNAVAILABLE") || len(w.calls) != 0 {
				t.Fatalf("err = %v calls=%d, want SAMPLE_PACK_UNAVAILABLE and no write", err, len(w.calls))
			}
			if tc.seeder != nil && tc.seeder.seedCalls != 0 {
				t.Fatalf("seeder ran %d times, want 0", tc.seeder.seedCalls)
			}
		})
	}
}

// Some tables are in the pack and some are not: the ones the pack does not have are reported, the
// rest are seeded (one missing table must not block the pack's own).
func TestGenerate_PackSource_UncoveredTableIsReportedNotFatal(t *testing.T) {
	seeder := &fakePackSeeder{covered: []string{"customers"}, result: PackSeedResult{
		Created: []SampleCreated{{Entity: "customers", Count: 4}},
	}}
	svc, _ := newPackGenSvc(&genStore{}, &genWriter{}, seeder)

	res, err := svc.Generate(context.Background(), genScope, "t1", "u1",
		GenerateRequest{Entities: []string{"customers", "services"}, PerEntity: 5, Source: "pack"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Entity != "services" || res.Skipped[0].Code != "SAMPLE_PACK_UNAVAILABLE" {
		t.Fatalf("skipped = %+v, want services SAMPLE_PACK_UNAVAILABLE", res.Skipped)
	}
}

func TestGenerate_AutoSource_PackForCoveredTablesFieldsForTheRest(t *testing.T) {
	seeder := &fakePackSeeder{covered: []string{"customers"}, result: PackSeedResult{
		Created: []SampleCreated{{Entity: "customers", Count: 4}},
	}}
	w := &genWriter{}
	svc, aud := newPackGenSvc(&genStore{}, w, seeder)

	res, err := svc.Generate(context.Background(), genScope, "t1", "u1",
		GenerateRequest{Entities: []string{"customers", "services"}, PerEntity: 3, Source: "auto"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !slices.Equal(seeder.seeded, []string{"customers"}) {
		t.Fatalf("pack seeded %v, want customers", seeder.seeded)
	}
	if len(w.calls) != 1 || w.calls[0].entity != "services" || len(w.calls[0].rows) != 3 ||
		!w.calls[0].marked || w.calls[0].mark.Source != dynrepo.SampleSourceGenerator || w.calls[0].mark.BatchID != res.BatchID {
		t.Fatalf("field generator calls = %+v, want services x3 under a generator mark of batch %q", w.calls, res.BatchID)
	}
	if len(res.Created) != 2 || len(res.Skipped) != 0 {
		t.Fatalf("result = %+v, want customers (pack) + services (fields) created", res)
	}
	if len(aud.events) != 1 || aud.events[0].Diff.After["source"] != "auto" {
		t.Fatalf("audit = %+v, want one line with source auto", aud.events)
	}
}

func TestGenerate_AutoSource_WithoutAPackEverythingFallsBackToFields(t *testing.T) {
	w := &genWriter{}
	svc, _ := newGenSvc(threeTableApp(), &genStore{}, w)

	res, err := svc.Generate(context.Background(), genScope, "t1", "u1",
		GenerateRequest{Entities: []string{"services"}, PerEntity: 2, Source: "auto"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(w.calls) != 1 || len(res.Created) != 1 {
		t.Fatalf("calls=%d created=%+v, want the field generator to run for services", len(w.calls), res.Created)
	}
}

func TestGenerate_PackSeederErrorIsWrapped(t *testing.T) {
	boom := errors.New("boom")
	seeder := &fakePackSeeder{covered: []string{"customers"}, seedErr: boom}
	svc, _ := newPackGenSvc(&genStore{}, &genWriter{}, seeder)

	_, err := svc.Generate(context.Background(), genScope, "t1", "u1",
		GenerateRequest{Entities: []string{"customers"}, PerEntity: 5, Source: "pack"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the seeder's error", err)
	}
}

func TestGenerate_UnknownSourceIsRefused(t *testing.T) {
	svc, _ := newGenSvc(threeTableApp(), &genStore{}, &genWriter{})
	_, err := svc.Generate(context.Background(), genScope, "t1", "u1",
		GenerateRequest{Entities: []string{"customers"}, PerEntity: 5, Source: "wat"})
	if !codeIs(err, "SAMPLE_SOURCE_INVALID") {
		t.Fatalf("err = %v, want SAMPLE_SOURCE_INVALID", err)
	}
}

// One lock for every sample writer: a pack run while another batch runs answers SAMPLE_BUSY.
func TestGenerate_PackSource_SharesTheTenantLock(t *testing.T) {
	seeder := &fakePackSeeder{covered: []string{"customers"}}
	svc, _ := newPackGenSvc(&genStore{}, &genWriter{}, seeder)
	release, ok := svc.tryLock("t1")
	if !ok {
		t.Fatal("could not take the lock")
	}
	defer release()
	_, err := svc.Generate(context.Background(), genScope, "t1", "u1",
		GenerateRequest{Entities: []string{"customers"}, PerEntity: 5, Source: "pack"})
	if !codeIs(err, "SAMPLE_BUSY") || seeder.seedCalls != 0 {
		t.Fatalf("err = %v seedCalls=%d, want SAMPLE_BUSY and no seeding", err, seeder.seedCalls)
	}
}

// source=pack replays the pack's own rows, so it needs no perEntity; fields and auto still do.
func TestGenerate_PerEntityIsRequiredExceptForPack(t *testing.T) {
	cases := []struct {
		source  string
		wantErr bool
	}{{"pack", false}, {"auto", true}, {"fields", true}, {"", true}}
	for _, tc := range cases {
		t.Run("source="+tc.source, func(t *testing.T) {
			seeder := &fakePackSeeder{covered: []string{"customers"}, result: PackSeedResult{Created: []SampleCreated{{Entity: "customers", Count: 2}}}}
			svc, _ := newPackGenSvc(&genStore{}, &genWriter{}, seeder)
			_, err := svc.Generate(context.Background(), genScope, "t1", "u1",
				GenerateRequest{Entities: []string{"customers"}, Source: tc.source})
			if tc.wantErr != codeIs(err, "SAMPLE_PER_ENTITY_INVALID") || (!tc.wantErr && err != nil) {
				t.Fatalf("source %q without perEntity: err = %v, wantErr %v", tc.source, err, tc.wantErr)
			}
		})
	}
}
