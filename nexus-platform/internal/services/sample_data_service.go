package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// SampleDataService is the builder's sample-data surface (specs/064-preview-as-role-sample-data
// S9-4): it seeds rows the AI Builder proposed as SAMPLE data (marked in the insert transaction,
// no automation) and reports how much sample data each table holds. The mark itself is written by
// dynrepo; this service only puts it on ctx, so a client can never forge one.
type SampleDataService struct {
	store   sampleCounter
	catalog sampleEntityReader
	records sampleRecordWriter
	auditor audit.Auditor
	packs   packSampleSeeder // optional (WithPackSeeder): the "pack" / "auto" sources
	locks   sync.Map         // tenant id -> *sync.Mutex
	// Wipe (S9-7), wired by WithWipe / WithLogger.
	purger samplePurger
	lister sampleEntityLister
	logger *zap.Logger
}

// sampleCounter is the repository surface Summary and Generate read (satisfied by *dynrepo.Repository).
type sampleCounter interface {
	CountSampleRecords(ctx context.Context, tenantID, entity string) (dynrepo.SampleCounts, error)
	SampleEntityNames(ctx context.Context, tenantID string) ([]string, error)
	// LiveRecordIDs / ActiveUserIDs feed Generate the parent rows a new row may point at (S9-5).
	LiveRecordIDs(ctx context.Context, tenantID, entity string, limit int) ([]string, error)
	ActiveUserIDs(ctx context.Context, tenantID string, limit int) ([]string, error)
	// SampleRoots / ResetSequenceIfEmpty feed Wipe (S9-7): the marked rows to delete, and the counter reset.
	SampleRoots(ctx context.Context, tenantID, entity string) ([]dynrepo.SampleRootRow, error)
	ResetSequenceIfEmpty(ctx context.Context, tenantID, entity, field string) (bool, error)
}

// sampleEntityReader resolves a LIVE entity (soft-deleted ones are not found) — satisfied by the MetaCache.
type sampleEntityReader interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	// ListFields is the entity's live field definitions (Generate reads them at run time).
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}

// sampleRecordWriter is RecordService.CreateBulk: every validation, relation, auto_number and
// before_save check of an ordinary bulk create still runs on sample rows.
type sampleRecordWriter interface {
	CreateBulk(ctx context.Context, scope RecordScope, tenantID, userID, entityName string, payloads []map[string]any, mode BulkMode) (BulkCreateResult, error)
}

// NewSampleDataService wires the service; the auditor defaults to a noop (WithAuditor sets the real one).
func NewSampleDataService(store sampleCounter, catalog sampleEntityReader, records sampleRecordWriter) *SampleDataService {
	return &SampleDataService{store: store, catalog: catalog, records: records, auditor: audit.NewNoopAuditor()}
}

// WithAuditor sets the audit sink for the __sample_data__ rows (async, fail-open).
func (s *SampleDataService) WithAuditor(a audit.Auditor) *SampleDataService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// tryLock takes the tenant's sample-data lock without waiting. ponytail: the lock lives in this
// process (one server instance); with several instances the worst case is two concurrent batches
// for one tenant, which only seeds extra sample rows. Upgrade path: SELECT GET_LOCK('sample:<tenant>', 0).
func (s *SampleDataService) tryLock(tenantID string) (release func(), ok bool) {
	v, _ := s.locks.LoadOrStore(tenantID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// CreateRecords seeds rows of one entity as AI-sourced sample data: the same shape and result as
// POST /:entity/bulk, with every created row marked and silent (no workflow, no approval).
func (s *SampleDataService) CreateRecords(ctx context.Context, scope RecordScope, tenantID, userID, entityName string, rows []map[string]any, mode BulkMode) (BulkCreateResult, error) {
	if _, err := s.liveEntity(ctx, tenantID, entityName); err != nil {
		return BulkCreateResult{}, err
	}
	release, ok := s.tryLock(tenantID)
	if !ok {
		return BulkCreateResult{}, invalid("SAMPLE_BUSY", "Đang sinh hoặc xoá dữ liệu mẫu, thử lại sau")
	}
	defer release()

	batch := uuid.NewString()
	mctx := dynrepo.WithSampleMark(ctx, dynrepo.SampleMark{BatchID: batch, Source: dynrepo.SampleSourceAI})
	res, err := s.records.CreateBulk(mctx, scope, tenantID, userID, entityName, rows, mode)
	if err != nil {
		if errors.Is(err, ErrEntityNotFound) {
			return BulkCreateResult{}, invalid("SAMPLE_ENTITY_NOT_FOUND", "Không tìm thấy bảng dữ liệu đã chọn")
		}
		return BulkCreateResult{}, fmt.Errorf("services.SampleDataService.CreateRecords: %w", err)
	}
	s.auditor.Record(ctx, audit.Event{
		TenantID: tenantID, ActorUserID: userID, EntityName: audit.SampleDataMeta, RecordID: batch,
		Action: audit.ActionCreate,
		Diff: &audit.Diff{After: map[string]any{
			"entity": entityName, "source": dynrepo.SampleSourceAI, "mode": string(mode),
			"created": res.Created, "failed": res.Failed,
		}},
		At: time.Now().UTC(),
	})
	return res, nil
}

// SampleBySource is the per-source split of one table's sample rows.
type SampleBySource struct {
	Generator int `json:"generator"`
	AI        int `json:"ai"`
	Pack      int `json:"pack"`
}

// SampleEntitySummary is one table's line of the sample-data card. Count = Live + InBin.
type SampleEntitySummary struct {
	Entity   string         `json:"entity"`
	Label    string         `json:"label"`
	Count    int            `json:"count"`
	Live     int            `json:"live"`
	InBin    int            `json:"inBin"`
	BySource SampleBySource `json:"bySource"`
}

// SampleSummary is GET /admin/sample-data.
type SampleSummary struct {
	Total    int                   `json:"total"`
	Entities []SampleEntitySummary `json:"entities"`
}

// Summary counts the tenant's sample rows per table. entities empty = every table the tenant has
// marks for. Only tables still live in metadata count: a mark whose table was deleted is skipped
// without error (ponytail: such orphan marks stay in sys_sample_records, harmless, tenant purge
// removes them); an explicitly requested table that is not live is SAMPLE_ENTITY_NOT_FOUND.
// Marks whose row was hard-deleted are not counted (the repository JOINs the live table).
func (s *SampleDataService) Summary(ctx context.Context, tenantID string, entities []string) (SampleSummary, error) {
	explicit := len(entities) > 0
	if !explicit {
		names, err := s.store.SampleEntityNames(ctx, tenantID)
		if err != nil {
			return SampleSummary{}, fmt.Errorf("services.SampleDataService.Summary: %w", err)
		}
		entities = names
	}
	out := SampleSummary{Entities: []SampleEntitySummary{}}
	for _, name := range entities {
		e, err := s.liveEntity(ctx, tenantID, name)
		if err != nil {
			var ve *ValidationError
			if !explicit && errors.As(err, &ve) && ve.Code == "SAMPLE_ENTITY_NOT_FOUND" {
				continue
			}
			return SampleSummary{}, err
		}
		c, err := s.store.CountSampleRecords(ctx, tenantID, name)
		if err != nil {
			return SampleSummary{}, fmt.Errorf("services.SampleDataService.Summary: %w", err)
		}
		row := SampleEntitySummary{
			Entity: name, Label: e.Label, Count: c.Live + c.InBin, Live: c.Live, InBin: c.InBin,
			BySource: SampleBySource{
				Generator: c.BySource[dynrepo.SampleSourceGenerator],
				AI:        c.BySource[dynrepo.SampleSourceAI],
				Pack:      c.BySource[dynrepo.SampleSourcePack],
			},
		}
		out.Total += row.Count
		out.Entities = append(out.Entities, row)
	}
	return out, nil
}

// liveEntity resolves a live entity of the tenant or answers SAMPLE_ENTITY_NOT_FOUND.
func (s *SampleDataService) liveEntity(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	e, err := s.catalog.GetEntityByName(ctx, tenantID, name)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) || errors.Is(err, ErrEntityNotFound) {
			return nil, invalid("SAMPLE_ENTITY_NOT_FOUND", "Không tìm thấy bảng dữ liệu đã chọn")
		}
		return nil, fmt.Errorf("services.SampleDataService.liveEntity: %w", err)
	}
	if e == nil {
		return nil, invalid("SAMPLE_ENTITY_NOT_FOUND", "Không tìm thấy bảng dữ liệu đã chọn")
	}
	return e, nil
}
