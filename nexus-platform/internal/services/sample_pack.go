package services

import (
	"context"
	"fmt"

	"nexus/platform/internal/dynrepo"
)

// Generate sources "pack" and "auto" (specs/064-preview-as-role-sample-data S9-6): the sample rows
// a built-in vertical pack ships (@refs resolved, N-N links created) come back as marked sample
// data, so a builder who wiped them — or built the tables before the demo rows existed — can bring
// them back without reinstalling the pack.

// packSampleSeeder is the vertical-pack service seen from here (services cannot import
// vertical_pack, which imports services — main.go wires *vertical_pack.Service in).
type packSampleSeeder interface {
	// PackSampleEntities: of entities, the tables that have sample rows in a built-in pack installed
	// in the tenant (request order).
	PackSampleEntities(ctx context.Context, tenantID string, entities []string) ([]string, error)
	// SeedPackSamples creates those rows as sample data of source "pack" in batch.
	SeedPackSamples(ctx context.Context, tenantID, userID, batch string, entities []string) (PackSeedResult, error)
}

// PackSeedResult is the report of one SeedPackSamples call; the arrays are never null.
type PackSeedResult struct {
	Created []SampleCreated
	Failed  []SampleFailed
	Skipped []SampleSkipped
}

// WithPackSeeder wires the pack source. Without it "pack" answers SAMPLE_PACK_UNAVAILABLE and
// "auto" generates everything from field metadata.
func (s *SampleDataService) WithPackSeeder(p packSampleSeeder) *SampleDataService {
	s.packs = p
	return s
}

// seedFromPack handles the pack part of a "pack"/"auto" run and returns the tables still to be
// generated from field metadata ("auto" only). Called with the tenant lock held.
//   - "pack" with no selected table in an installed pack: 422 SAMPLE_PACK_UNAVAILABLE.
//   - "pack" with some of them in the pack: the others are reported as skipped SAMPLE_PACK_UNAVAILABLE.
//   - a table that already holds pack samples is skipped SAMPLE_ALREADY_PRESENT (never seeded twice).
func (s *SampleDataService) seedFromPack(ctx context.Context, tenantID, userID, batch, source string, selected []string, res *GenerateResult) ([]string, error) {
	var covered []string
	if s.packs != nil {
		var err error
		if covered, err = s.packs.PackSampleEntities(ctx, tenantID, selected); err != nil {
			return nil, fmt.Errorf("services.SampleDataService.Generate: pack tables: %w", err)
		}
	}
	if source == "pack" && len(covered) == 0 {
		return nil, invalid("SAMPLE_PACK_UNAVAILABLE", "Bảng này không có dữ liệu mẫu trong gói đã cài")
	}
	inPack := toSet(covered)
	var seed, rest []string
	for _, name := range selected {
		switch {
		case !inPack[name] && source == "pack":
			res.Skipped = append(res.Skipped, SampleSkipped{Entity: name, Code: "SAMPLE_PACK_UNAVAILABLE"})
		case !inPack[name]:
			rest = append(rest, name)
		default:
			c, err := s.store.CountSampleRecords(ctx, tenantID, name)
			if err != nil {
				return nil, fmt.Errorf("services.SampleDataService.Generate: count %q: %w", name, err)
			}
			if c.BySource[dynrepo.SampleSourcePack] > 0 {
				res.Skipped = append(res.Skipped, SampleSkipped{Entity: name, Code: "SAMPLE_ALREADY_PRESENT"})
				continue
			}
			seed = append(seed, name)
		}
	}
	if len(seed) == 0 {
		return rest, nil
	}
	out, err := s.packs.SeedPackSamples(ctx, tenantID, userID, batch, seed)
	if err != nil {
		return nil, fmt.Errorf("services.SampleDataService.Generate: seed from pack: %w", err)
	}
	res.Created = append(res.Created, out.Created...)
	res.Failed = append(res.Failed, out.Failed...)
	res.Skipped = append(res.Skipped, out.Skipped...)
	return rest, nil
}
