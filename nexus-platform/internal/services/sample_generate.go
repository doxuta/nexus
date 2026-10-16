package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/sampledata"
)

// Generate (specs/064-preview-as-role-sample-data S9-5) seeds sample rows for the chosen tables
// from their field definitions, no AI involved. The metadata is read at run time; the pure
// internal/sampledata package decides the rows and the table order; every row still goes through
// RecordService.CreateBulk (partial mode), so validation, relation checks and auto_number run as
// for any create, and the rows carry a generator-sourced sample mark (no workflow, no approval).

const (
	maxSamplePerEntity   = 20
	maxSampleEntities    = 30 // tables per request: the run holds the tenant lock for N x perEntity synchronous rows
	sampleCandidateLimit = 50 // parent ids a new row may point at, taken from a table outside this run
)

// GenerateRequest is POST /admin/sample-data/generate.
type GenerateRequest struct {
	Entities  []string
	PerEntity int
	Source    string // "" / "fields" (field metadata), "pack" (built-in pack samples), "auto" (pack where it has the table, fields elsewhere)
}

// SampleCreated is how many rows one table got.
type SampleCreated struct {
	Entity string `json:"entity"`
	Count  int    `json:"count"`
}

// SampleFailed is one row the record layer refused, with its bulk-create code.
type SampleFailed struct {
	Entity string `json:"entity"`
	Index  int    `json:"index"`
	Code   string `json:"code"`
	Error  string `json:"error"`
}

// SampleSkipped is a table left out, with the reason code.
type SampleSkipped struct {
	Entity string `json:"entity"`
	Code   string `json:"code"`
}

// GenerateResult is the report of one run; the arrays are never null.
type GenerateResult struct {
	BatchID      string          `json:"batchId"`
	Created      []SampleCreated `json:"created"`
	Failed       []SampleFailed  `json:"failed"`
	Skipped      []SampleSkipped `json:"skipped"`
	AddedParents []string        `json:"addedParents"`
}

// Generate runs the generator for the tenant. 409 SAMPLE_BUSY while another sample batch runs.
func (s *SampleDataService) Generate(ctx context.Context, scope RecordScope, tenantID, userID string, req GenerateRequest) (GenerateResult, error) {
	source := req.Source
	switch source {
	case "":
		source = "fields"
	case "fields", "pack", "auto":
	default:
		return GenerateResult{}, invalid("SAMPLE_SOURCE_INVALID", "Nguồn dữ liệu mẫu phải là fields, pack hoặc auto")
	}
	// source=pack replays the pack's own rows: there is no per-table count to validate.
	if source != "pack" && (req.PerEntity < 1 || req.PerEntity > maxSamplePerEntity) {
		return GenerateResult{}, invalid("SAMPLE_PER_ENTITY_INVALID", "Số dòng mỗi bảng phải từ 1 đến %d", maxSamplePerEntity)
	}
	selected := dedupe(req.Entities)
	if len(selected) == 0 {
		return GenerateResult{}, invalid("SAMPLE_ENTITY_NOT_FOUND", "Không tìm thấy bảng dữ liệu đã chọn")
	}
	if len(selected) > maxSampleEntities {
		return GenerateResult{}, invalid("SAMPLE_TOO_MANY_ENTITIES", "Chọn tối đa %d bảng mỗi lần", maxSampleEntities)
	}
	for _, name := range selected {
		if _, err := s.liveEntity(ctx, tenantID, name); err != nil {
			return GenerateResult{}, err
		}
	}
	release, ok := s.tryLock(tenantID)
	if !ok {
		return GenerateResult{}, invalid("SAMPLE_BUSY", "Đang sinh hoặc xoá dữ liệu mẫu, thử lại sau")
	}
	defer release()

	run := &sampleRun{
		s: s, tenantID: tenantID, batch: uuid.NewString(),
		defs: map[string]sampledata.Entity{}, db: map[string][]string{}, made: map[string][]string{},
	}
	res := GenerateResult{BatchID: run.batch, Created: []SampleCreated{}, Failed: []SampleFailed{}, Skipped: []SampleSkipped{}, AddedParents: []string{}}
	defer func() {
		if len(res.Created) == 0 {
			return
		}
		counts := map[string]int{}
		for _, c := range res.Created {
			counts[c.Entity] = c.Count
		}
		s.auditor.Record(ctx, audit.Event{
			TenantID: tenantID, ActorUserID: userID, EntityName: audit.SampleDataMeta, RecordID: run.batch,
			Action: audit.ActionCreate,
			Diff:   &audit.Diff{After: map[string]any{"source": source, "counts": counts, "failed": len(res.Failed)}},
			At:     time.Now().UTC(),
		})
	}()

	// pack / auto: the tables a built-in pack ships samples for come from the pack; "auto" generates the
	// rest from field metadata, "pack" reports them.
	if source != "fields" {
		var err error
		if selected, err = s.seedFromPack(ctx, tenantID, userID, run.batch, source, selected, &res); err != nil {
			return GenerateResult{}, err
		}
		if len(selected) == 0 {
			return res, nil
		}
	}

	plan, users, err := run.plan(ctx, selected)
	if err != nil {
		return GenerateResult{}, err
	}
	for _, sk := range plan.Skipped {
		res.Skipped = append(res.Skipped, SampleSkipped{Entity: sk.Entity, Code: sk.Code})
	}
	now := time.Now().UTC()
	added := toSet(plan.AddedParents)
	for _, name := range plan.Order {
		def := run.defs[name]
		rows, skip := sampledata.Rows(def, req.PerEntity, run.batch, now, run.candidates(ctx, def, users))
		if skip != "" {
			res.Skipped = append(res.Skipped, SampleSkipped{Entity: name, Code: skip})
			delete(added, name)
			continue
		}
		mctx := dynrepo.WithSampleMark(ctx, dynrepo.SampleMark{BatchID: run.batch, Source: dynrepo.SampleSourceGenerator})
		out, err := s.records.CreateBulk(mctx, scope, tenantID, userID, name, rows, BulkModePartial)
		if err != nil {
			if errors.Is(err, ErrEntityNotFound) {
				return res, invalid("SAMPLE_ENTITY_NOT_FOUND", "Không tìm thấy bảng dữ liệu đã chọn")
			}
			return res, fmt.Errorf("services.SampleDataService.Generate: entity %q: %w", name, err)
		}
		if out.Created == 0 {
			delete(added, name) // every row was rejected: nothing was added to the parent table either
		} else {
			res.Created = append(res.Created, SampleCreated{Entity: name, Count: out.Created})
		}
		for _, r := range out.Results {
			switch r.Status {
			case "created":
				run.made[name] = append(run.made[name], r.ID)
			case "failed":
				res.Failed = append(res.Failed, SampleFailed{Entity: name, Index: r.Index, Code: r.Code, Error: r.Error})
			}
		}
	}
	for _, name := range plan.AddedParents {
		if added[name] {
			res.AddedParents = append(res.AddedParents, name)
		}
	}
	return res, nil
}

// sampleRun is the state of one Generate call.
type sampleRun struct {
	s        *SampleDataService
	tenantID string
	batch    string
	defs     map[string]sampledata.Entity
	db       map[string][]string // table -> live ids already in the database (nil = none or not a live table)
	made     map[string][]string // table -> ids created by this run
	probed   map[string]bool
}

// plan loads the chosen tables plus every empty table their required relations need, probes which
// referenced tables already have rows, and asks sampledata.Plan for the order.
func (r *sampleRun) plan(ctx context.Context, selected []string) (sampledata.PlanResult, []string, error) {
	r.probed = map[string]bool{}
	for _, name := range selected {
		if err := r.load(ctx, name); err != nil {
			return sampledata.PlanResult{}, nil, err
		}
	}
	for queue := append([]string(nil), selected...); len(queue) > 0; queue = queue[1:] {
		for _, t := range sampledata.RequiredTargets(r.defs[queue[0]]) {
			if _, loaded := r.defs[t]; loaded {
				continue
			}
			ids, err := r.liveIDs(ctx, t)
			if err != nil {
				return sampledata.PlanResult{}, nil, err
			}
			if len(ids) > 0 {
				continue
			}
			if err := r.load(ctx, t); err != nil {
				return sampledata.PlanResult{}, nil, err
			}
			if _, ok := r.defs[t]; ok {
				queue = append(queue, t)
			}
		}
	}
	existing := map[string]bool{}
	entities := make([]sampledata.Entity, 0, len(r.defs))
	for _, def := range r.defs {
		entities = append(entities, def)
		for _, t := range referencedTables(def) {
			ids, err := r.liveIDs(ctx, t)
			if err != nil {
				return sampledata.PlanResult{}, nil, err
			}
			existing[t] = len(ids) > 0
		}
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i].Name < entities[j].Name })
	users, err := r.s.store.ActiveUserIDs(ctx, r.tenantID, sampleCandidateLimit)
	if err != nil {
		return sampledata.PlanResult{}, nil, fmt.Errorf("services.SampleDataService.Generate: users: %w", err)
	}
	return sampledata.Plan(sampledata.PlanInput{Entities: entities, Selected: selected, Existing: existing, HasUsers: len(users) > 0}), users, nil
}

// load reads one live table's fields into the generator's shape; a table that is not live is left out.
func (r *sampleRun) load(ctx context.Context, name string) error {
	e, err := r.s.liveEntity(ctx, r.tenantID, name)
	if err != nil {
		if codeIs(err, "SAMPLE_ENTITY_NOT_FOUND") {
			return nil
		}
		return err
	}
	fields, err := r.s.catalog.ListFields(ctx, r.tenantID, e.ID)
	if err != nil {
		return fmt.Errorf("services.SampleDataService.Generate: fields of %q: %w", name, err)
	}
	def := sampledata.Entity{Name: e.Name, Label: e.Label}
	for _, f := range fields {
		var opts map[string]any
		if f.Options != nil && *f.Options != "" {
			if err := json.Unmarshal([]byte(*f.Options), &opts); err != nil {
				return fmt.Errorf("services.SampleDataService.Generate: field %q.%q options: %w", name, f.Name, err)
			}
		}
		def.Fields = append(def.Fields, sampledata.Field{
			Name: f.Name, Label: f.Label, Type: f.FieldType, Required: f.IsRequired, Unique: f.IsUnique, Options: opts,
		})
	}
	r.defs[name] = def
	return nil
}

// liveIDs is the table's live ids in the database, probed once per run; a table that is not live
// (a relation left pointing at a deleted table) has none.
func (r *sampleRun) liveIDs(ctx context.Context, name string) ([]string, error) {
	if r.probed[name] {
		return r.db[name], nil
	}
	r.probed[name] = true
	if _, err := r.s.liveEntity(ctx, r.tenantID, name); err != nil {
		if codeIs(err, "SAMPLE_ENTITY_NOT_FOUND") {
			return nil, nil
		}
		return nil, err
	}
	ids, err := r.s.store.LiveRecordIDs(ctx, r.tenantID, name, sampleCandidateLimit)
	if err != nil {
		return nil, fmt.Errorf("services.SampleDataService.Generate: candidates of %q: %w", name, err)
	}
	r.db[name] = ids
	return ids, nil
}

// candidates are the ids new rows of def may point at: rows this run just made, else rows already there.
func (r *sampleRun) candidates(ctx context.Context, def sampledata.Entity, users []string) sampledata.Candidates {
	c := sampledata.Candidates{Relation: map[string][]string{}, Users: users}
	for _, t := range referencedTables(def) {
		if ids := r.made[t]; len(ids) > 0 {
			c.Relation[t] = ids
		} else if ids, err := r.liveIDs(ctx, t); err == nil {
			c.Relation[t] = ids
		}
	}
	return c
}

// referencedTables lists every table def's relation and polymorphic fields point at (self included).
func referencedTables(def sampledata.Entity) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, f := range def.Fields {
		switch f.Type {
		case "relation":
			t, _ := f.Options["relatedEntity"].(string)
			add(t)
		case "polymorphic_relation":
			list, _ := f.Options["targetEntities"].([]any)
			for _, x := range list {
				t, _ := x.(string)
				add(t)
			}
		}
	}
	return out
}

func codeIs(err error, code string) bool {
	var ve *ValidationError
	return errors.As(err, &ve) && ve.Code == code
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func toSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, s := range in {
		m[s] = true
	}
	return m
}
