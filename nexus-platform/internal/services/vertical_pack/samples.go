package vertical_pack

import (
	"context"
	"errors"
	"fmt"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

// Pass 3 + Pass 4 of Install (the pack's sample rows and the N-N links between them), and the same
// walk re-run later for the sample-data surface (specs/064-preview-as-role-sample-data S9-6): the
// builder wipes a built-in pack's samples before handing the app over, and can bring them back.

// seedOpts tunes one walk over pack.Samples.
type seedOpts struct {
	mark    *dynrepo.SampleMark // non-nil: every row is created as SAMPLE data (no automation, countable, wipeable)
	only    map[string]bool     // non-nil: seed just these entities (rows pointing at any other entity's row cannot resolve)
	lenient bool                // a row the record layer refuses is reported and skipped; Install aborts and rolls back instead
}

// seedResult is what one walk did. entities is filled for every entity that had rows to seed.
type seedResult struct {
	inserted, skipped, linksCreated, linksSkipped int
	warnings                                      []string
	entities                                      map[string]*entitySeed
	failed                                        []services.SampleFailed
}

// entitySeed is one entity's tally. skipped counts every row left out; the other counters say why:
// duplicate (a unique value already present), vetoed (a tenant before_save rule refused it),
// cascaded (it refers to a row skipped earlier in the walk), unresolved (its "@ref" parent is not
// part of this walk), failed (the record layer refused it, reported in seedResult.failed).
type entitySeed struct{ created, skipped, duplicate, vetoed, cascaded, unresolved, failed int }

func (r *seedResult) of(entity string) *entitySeed {
	if r.entities == nil {
		r.entities = map[string]*entitySeed{}
	}
	e := r.entities[entity]
	if e == nil {
		e = &entitySeed{}
		r.entities[entity] = e
	}
	return e
}

// seedSamples inserts the pack's sample records in entity-dependency order (entityOrder) so FK
// values given as "@ref" resolve to the id of a row inserted earlier in this walk, then creates the
// pack's N-N links between the rows it inserted. Idempotency, skip and cascade rules are exactly
// those Install always had (a duplicate or a before_save veto skips the row and everything that
// refers to it); see Install's doc for the contract.
func (s *Service) seedSamples(ctx context.Context, tenantID, userID string, pack *Pack, entityOrder []string, o seedOpts) (*seedResult, error) {
	res := &seedResult{}
	// refMap tracks "@key" → {entity,id} for both resolution and Link.
	refMap := make(map[string]refEntry)
	// skippedRefs tracks the "@ref" of every sample row skipped (a unique value already present from
	// a previous install, a rule veto, an unresolvable parent). A later row or link referencing a
	// skipped ref is cascade-skipped too — its real id is unknown here — so a re-install never
	// hard-fails on it.
	skippedRefs := make(map[string]struct{})
	// vetoed counts, per entity (first-seen order), the sample rows a tenant before_save rule refused,
	// with the first refusal's words (queue #253), so the install can say so instead of staying silent.
	type vetoTally struct {
		entity, msg string
		n           int
	}
	var vetoed []*vetoTally

	if s.recordWr != nil {
		// Index samples by entity name for ordered access.
		sampleByEntity := make(map[string]*PackSample, len(pack.Samples))
		for i := range pack.Samples {
			sampleByEntity[pack.Samples[i].Entity] = &pack.Samples[i]
		}

		// Insert in entity-dependency order first, then any samples for entities not in
		// the ordered set (e.g. entities already present in the tenant).
		seen := make(map[string]struct{})
		insertOrder := make([]*PackSample, 0, len(pack.Samples))
		for _, name := range entityOrder {
			if ps, ok := sampleByEntity[name]; ok {
				insertOrder = append(insertOrder, ps)
				seen[name] = struct{}{}
			}
		}
		for i := range pack.Samples {
			if _, ok := seen[pack.Samples[i].Entity]; !ok {
				insertOrder = append(insertOrder, &pack.Samples[i])
			}
		}

		scope := services.RecordScope{
			UserID:      userID,
			AccessLevel: "all",
			IsAdmin:     true,
		}
		createCtx := ctx
		if o.mark != nil {
			createCtx = dynrepo.WithSampleMark(ctx, *o.mark)
		}
		for _, ps := range insertOrder {
			if o.only != nil && !o.only[ps.Entity] {
				continue
			}
			tally := res.of(ps.Entity)
			for idx, pr := range ps.Records {
				skip := func() {
					res.skipped++
					tally.skipped++
					if pr.Ref != "" {
						skippedRefs[pr.Ref] = struct{}{}
					}
				}
				// Cascade-skip: a row referencing a skipped duplicate cannot resolve that ref to
				// a real id, so it is skipped like its parent (and its own ref cascades further).
				if refersToSkipped(pr.Data, skippedRefs) {
					skip()
					tally.cascaded++
					continue
				}
				// Resolve "@ref" values in Data to real IDs.
				resolved, err := resolveRefs(pr.Data, refMap)
				if err != nil {
					if o.lenient {
						// The parent row is not part of this walk (its table was not picked, or it
						// was skipped): leave this row out too.
						skip()
						tally.unresolved++
						continue
					}
					return nil, fmt.Errorf("vertical_pack.Install: sample %q ref: %w", pr.Ref, err)
				}
				id, err := s.recordWr.Create(createCtx, scope, tenantID, userID, ps.Entity, resolved)
				if err != nil {
					// A unique value already present (typically a re-install into a populated
					// tenant) skips THIS row only — the install must complete and reach the
					// later passes (layouts/roles/dashboards/app-mint), not abort mid-seed.
					if isRecordDuplicate(err) {
						skip()
						tally.duplicate++
						continue
					}
					// A before_save rule the tenant already runs refuses this sample row (typically the
					// pack's own rule, switched on by an earlier install: a re-install seeds the samples
					// BEFORE the workflows pass, so the first install was never judged). FR-017 keeps the
					// rule binding every writer, so the row is not forced in; like a duplicate it is
					// skipped (and so are its dependents), and the warning below names the rule's words.
					if isHookVeto(err) {
						skip()
						tally.vetoed++
						var t *vetoTally
						for _, v := range vetoed {
							if v.entity == ps.Entity {
								t = v
							}
						}
						if t == nil {
							var ve *services.ValidationError
							errors.As(err, &ve)
							t = &vetoTally{entity: ps.Entity, msg: ve.Msg}
							vetoed = append(vetoed, t)
						}
						t.n++
						continue
					}
					var ve *services.ValidationError
					isVE := errors.As(err, &ve)
					// Lenient: only a refusal of the record layer (a ValidationError) is one row's problem.
					// Anything else (DB down or timed out, ctx cancelled, deadlock, schema drift) stops the
					// run with the wrapped error, like the bulk path does: its raw text never goes into
					// failed[], and a transient outage never reads as "created N, failed M".
					if o.lenient && isVE {
						res.failed = append(res.failed, services.SampleFailed{Entity: ps.Entity, Index: idx, Code: ve.Code, Error: ve.Msg})
						tally.failed++
						skip()
						continue
					}
					if isVE {
						return nil, err
					}
					return nil, fmt.Errorf("vertical_pack.Install: create sample %q in %q: %w", pr.Ref, ps.Entity, err)
				}
				res.inserted++
				tally.created++
				if pr.Ref != "" {
					refMap[pr.Ref] = refEntry{entity: ps.Entity, id: id}
				}
			}
		}
	}

	for _, v := range vetoed {
		res.warnings = append(res.warnings, fmt.Sprintf(
			"%d dòng dữ liệu mẫu của %q bị quy tắc \"trước khi lưu\" đang bật trong tổ chức từ chối nên không được thêm (kèm các dòng/liên kết tham chiếu tới chúng): %s",
			v.n, v.entity, v.msg))
	}

	// --- Pass 4: N-N links. ---------------------------------------------------------------
	// Create join-table rows between seed records. Both ends are resolved via refMap (which
	// was populated in Pass 3). Install calls Link with isAdmin=true and nil aclChecker so
	// ACL-on-target is bypassed during seeding.
	if s.lnk != nil && len(pack.Links) > 0 {
		adminScope := dynrepo.RowScope{UserID: userID, Level: dynrepo.AccessAll, IsAdmin: true}
		for _, lk := range pack.Links {
			// An endpoint whose sample row was skipped as a duplicate has no known id — skip the
			// link (it was created by the install that seeded the row; re-creating is impossible).
			if _, skipped := skippedRefs[lk.FromRef]; skipped {
				res.linksSkipped++
				continue
			}
			if _, skipped := skippedRefs[lk.ToRef]; skipped {
				res.linksSkipped++
				continue
			}
			fromEntry, ok := refMap[lk.FromRef]
			if !ok {
				if o.lenient { // an end in a table this walk did not seed
					res.linksSkipped++
					continue
				}
				return nil, fmt.Errorf("vertical_pack.Install: link fromRef %q not found in refMap", lk.FromRef)
			}
			toEntry, ok := refMap[lk.ToRef]
			if !ok {
				if o.lenient {
					res.linksSkipped++
					continue
				}
				return nil, fmt.Errorf("vertical_pack.Install: link toRef %q not found in refMap", lk.ToRef)
			}
			created, err := s.lnk.Link(
				ctx,
				tenantID, userID, "", // roleID empty — isAdmin bypasses role check
				true, // isAdmin
				fromEntry.entity, fromEntry.id,
				lk.Relation, toEntry.id,
				s.recExists, // TargetChecker
				nil,         // TargetACLChecker — nil: admin bypasses
				s.recFetch,  // RecordFetcher
				adminScope,
			)
			if err != nil {
				var ve *services.ValidationError
				switch {
				case o.lenient: // rows are already in: report fewer links rather than lose the batch's report
					res.linksSkipped++
					continue
				case errors.As(err, &ve):
					return nil, err
				}
				return nil, fmt.Errorf("vertical_pack.Install: link %q→%q via %q: %w", lk.FromRef, lk.ToRef, lk.Relation, err)
			}
			if created {
				res.linksCreated++
			}
		}
	}
	return res, nil
}

// --- Re-seeding the samples of a built-in pack (the sample-data surface) -------------------------

// installedBuiltinPacks are the built-in packs the tenant's install ledger names, newest first,
// one per slug. Only a ledger row that recordInstall stamped result.demoSamples=true counts (a
// built-in install): an upload reusing a built-in slug is not a source, and the samples used are
// always the ones in OUR pack, never an uploaded body. Installs older than the stamp are not
// sources either (their sample rows were never marked); reinstalling the pack stamps a new row.
func (s *Service) installedBuiltinPacks(ctx context.Context, tenantID string) ([]*Pack, error) {
	if s.installs == nil {
		return nil, nil
	}
	recs, err := s.installs.ListInstalls(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.installedBuiltinPacks: %w", err)
	}
	var out []*Pack
	seen := map[string]bool{}
	for _, rec := range recs {
		if rec.Slug == "" || seen[rec.Slug] || rec.Result["demoSamples"] != true {
			continue
		}
		seen[rec.Slug] = true
		if p, ok := BuiltinPack(rec.Slug); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// hasSamples reports whether pack ships at least one sample row for entity.
func hasSamples(pack *Pack, entity string) bool {
	for _, ps := range pack.Samples {
		if ps.Entity == entity && len(ps.Records) > 0 {
			return true
		}
	}
	return false
}

// PackSampleEntities answers which of entities have sample rows in a built-in pack installed in
// the tenant (request order kept). Satisfies services.packSampleSeeder.
func (s *Service) PackSampleEntities(ctx context.Context, tenantID string, entities []string) ([]string, error) {
	packs, err := s.installedBuiltinPacks(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	var covered []string
	for _, e := range entities {
		for _, p := range packs {
			if hasSamples(p, e) {
				covered = append(covered, e)
				break
			}
		}
	}
	return covered, nil
}

// SeedPackSamples creates the sample rows the installed built-in pack(s) ship for entities, all as
// sample data of source "pack" in batch. A row the record layer refuses is reported in Failed and
// skipped; a table short of rows is reported in Skipped with the cause (skipReasons). A non-validation
// error (DB down, ctx cancelled) aborts the run and is returned wrapped; rows already written stay.
// Satisfies services.packSampleSeeder.
func (s *Service) SeedPackSamples(ctx context.Context, tenantID, userID, batch string, entities []string) (services.PackSeedResult, error) {
	out := services.PackSeedResult{Created: []services.SampleCreated{}, Failed: []services.SampleFailed{}, Skipped: []services.SampleSkipped{}}
	packs, err := s.installedBuiltinPacks(ctx, tenantID)
	if err != nil {
		return out, err
	}
	claimed := map[string]bool{}
	for _, p := range packs {
		var mine []string
		only := map[string]bool{}
		for _, e := range entities {
			if !claimed[e] && hasSamples(p, e) {
				claimed[e] = true
				only[e] = true
				mine = append(mine, e)
			}
		}
		if len(mine) == 0 {
			continue
		}
		ordered, err := orderEntities(p.Entities)
		if err != nil {
			return out, fmt.Errorf("vertical_pack.SeedPackSamples: %w", err)
		}
		order := make([]string, 0, len(ordered))
		for _, pe := range ordered {
			order = append(order, pe.Name)
		}
		r, err := s.seedSamples(ctx, tenantID, userID, p, order, seedOpts{
			mark: &dynrepo.SampleMark{BatchID: batch, Source: dynrepo.SampleSourcePack}, only: only, lenient: true,
		})
		if err != nil {
			return out, fmt.Errorf("vertical_pack.SeedPackSamples: %w", err)
		}
		out.Failed = append(out.Failed, r.failed...)
		for _, e := range mine {
			t := r.entities[e]
			if t == nil { // no record writer wired, or the table never entered the walk: say so, never drop it
				out.Skipped = append(out.Skipped, services.SampleSkipped{Entity: e, Code: "SAMPLE_PACK_UNAVAILABLE"})
				continue
			}
			if t.created > 0 {
				out.Created = append(out.Created, services.SampleCreated{Entity: e, Count: t.created})
			}
			out.Skipped = append(out.Skipped, skipReasons(e, t)...)
		}
	}
	return out, nil
}

// skipReasons names why rows of entity e were left out. Rows the record layer refused are in
// Failed already (a table whose only cause is that gets no skip line). A table that got no row says its first cause (parent outside the run, a tenant
// rule's veto, a skipped parent row, an existing unique value); a table that got some rows still
// reports a veto or a skipped parent, so a short table is never silent.
func skipReasons(e string, t *entitySeed) []services.SampleSkipped {
	var out []services.SampleSkipped
	add := func(code string) { out = append(out, services.SampleSkipped{Entity: e, Code: code}) }
	if t.created == 0 {
		switch {
		case t.unresolved > 0:
			add("SAMPLE_PARENT_EMPTY")
		case t.vetoed > 0:
			add("SAMPLE_VETOED")
		case t.duplicate > 0:
			add("SAMPLE_ALREADY_PRESENT")
		case t.cascaded > 0:
			add("SAMPLE_PARENT_SKIPPED")
		}
		return out
	}
	if t.vetoed > 0 {
		add("SAMPLE_VETOED")
	}
	if t.cascaded > 0 {
		add("SAMPLE_PARENT_SKIPPED")
	}
	return out
}
