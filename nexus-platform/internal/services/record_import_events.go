package services

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/workflow"
)

// specs/048 S7a — CSV import writes one "Tạo" audit line per stored row.
//
// Import commits its rows in one statement; the history lines are written afterwards by a durable
// job (sys_jobs) per chunk of ≤ImportEventsChunkSize ids, SYNCHRONOUSLY through audit.Writer — the
// async auditor drops events when its buffer is full, and a 10k-row file must not lose lines.
// sys_jobs is at-least-once and has no progress column, so the per-record "already handled" mark is
// the record's own audit `create` line: each run first asks which ids already have one and skips
// them. A crash between two rows costs at most the rows still pending, never a duplicate line (two workers racing after a lease expiry can both write one: rare, bounded by one chunk).

// ImportEventsJobType is the sys_jobs job_type of one chunk of imported records.
const ImportEventsJobType = "record_import_events"

// ImportEventsChunkSize is the most record ids one job carries.
const ImportEventsChunkSize = 500

// ImportEventsPayload is the sys_jobs.payload: ids and names only, never a field value (the job
// re-reads the stored rows, tenant-pinned). The tenant lives on the sys_jobs row itself.
type ImportEventsPayload struct {
	ImportID   string   `json:"importId"`
	Chunk      int      `json:"chunk"`
	EntityName string   `json:"entityName"`
	UserID     string   `json:"userId"`
	RecordIDs  []string `json:"recordIds"`
	// RunAutomations (specs/048 S7b): fire the after_create workflow rules and BPM approvals of each
	// record, as a hand-made create does, before its history line is written.
	RunAutomations bool `json:"runAutomations,omitempty"`
}

// importEmitTimeout bounds how long one record waits for room in a dispatcher buffer; past it the job
// fails (retried by the queue) rather than dropping the record's automations.
const importEmitTimeout = 30 * time.Second

// ImportAutomation is the two after_create consumers the job feeds when the import asked for
// automations. Both are the blocking EmitWait of the real dispatcher / engine.
type ImportAutomation struct {
	Workflow interface {
		EmitWait(ctx context.Context, evt workflow.Event) error
	}
	BPM interface {
		EmitWait(ctx context.Context, evt bpm.Event) error
	}
}

// WithEventJobs arms the post-commit history job on the import path. nil (the default) writes no
// history for imported rows — the pre-S7a behaviour.
func (s *ImportService) WithEventJobs(q JobEnqueuer) *ImportService {
	s.eventJobs = q
	return s
}

// enqueueImportEvents parks one job per chunk of ids after the rows are committed. A failed
// enqueue never fails the committed import: it is logged and counted (returned) so the result can
// say that many history jobs are missing.
func (s *ImportService) enqueueImportEvents(ctx context.Context, in ImportInput, entity string, ids []string) (failed int) {
	if s.eventJobs == nil || len(ids) == 0 {
		return 0
	}
	importID := uuid.NewString()
	for chunk := 0; chunk*ImportEventsChunkSize < len(ids); chunk++ {
		end := min((chunk+1)*ImportEventsChunkSize, len(ids))
		payload, err := json.Marshal(ImportEventsPayload{
			ImportID: importID, Chunk: chunk, EntityName: entity, UserID: in.UserID, RecordIDs: ids[chunk*ImportEventsChunkSize : end],
			RunAutomations: in.RunAutomations,
		})
		if err == nil {
			_, err = s.eventJobs.Enqueue(context.WithoutCancel(ctx), jobs.Job{TenantID: in.TenantID, Type: ImportEventsJobType, Payload: payload})
		}
		if err != nil {
			failed++
			s.logger.Error("services.ImportService.enqueueImportEvents: history job not queued",
				zap.Error(err), zap.String("tenant_id", in.TenantID), zap.String("entity_name", entity),
				zap.String("import_id", importID), zap.Int("chunk", chunk))
		}
	}
	return failed
}

// ImportEventsReader reads the stored data of records by id, pinned to the tenant.
type ImportEventsReader interface {
	BatchRecordData(ctx context.Context, entity, tenantID string, ids []string) (map[string]map[string]any, error)
}

// ImportEventsAudit is the audit side of the job: the already-written probe and the sync writer.
type ImportEventsAudit interface {
	audit.Writer
	ExistingCreateIDs(ctx context.Context, tenantID, entity string, ids []string) (map[string]struct{}, error)
}

// NewImportEventsJobHandler builds the ImportEventsJobType handler. Every error is returned (the
// queue retries with backoff); ids already audited are skipped, so a retry resumes where it died.
func NewImportEventsJobHandler(catalog EntityCatalog, reader ImportEventsReader, aud ImportEventsAudit, logger *zap.Logger, auto ...ImportAutomation) jobs.HandlerFunc {
	var automation ImportAutomation
	if len(auto) > 0 {
		automation = auto[0]
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(ctx context.Context, job jobs.Job) error {
		var p ImportEventsPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return jobs.Permanent(fmt.Errorf("services.ImportEventsJobHandler: payload: %w", err))
		}
		if job.TenantID == "" || p.EntityName == "" {
			return jobs.Permanent(fmt.Errorf("services.ImportEventsJobHandler: job %s has no tenant or entity", job.ID))
		}
		e, err := catalog.GetEntityByName(ctx, job.TenantID, p.EntityName)
		if err != nil {
			return fmt.Errorf("services.ImportEventsJobHandler: entity: %w", err)
		}
		fields, err := catalog.ListFields(ctx, job.TenantID, e.ID)
		if err != nil {
			return fmt.Errorf("services.ImportEventsJobHandler: fields: %w", err)
		}
		live, err := toFieldSpecs(fields)
		if err != nil {
			return fmt.Errorf("services.ImportEventsJobHandler: %w", err)
		}
		specs := redactionSpecsFor(ctx, catalog, logger, job.TenantID, e.ID, live)

		done, err := aud.ExistingCreateIDs(ctx, job.TenantID, p.EntityName, p.RecordIDs)
		if err != nil {
			return fmt.Errorf("services.ImportEventsJobHandler: probe: %w", err)
		}
		todo := make([]string, 0, len(p.RecordIDs))
		for _, id := range p.RecordIDs {
			if _, ok := done[id]; !ok {
				todo = append(todo, id)
			}
		}
		if len(todo) == 0 {
			return nil
		}
		data, err := reader.BatchRecordData(ctx, p.EntityName, job.TenantID, todo)
		if err != nil {
			return fmt.Errorf("services.ImportEventsJobHandler: read: %w", err)
		}
		for _, id := range todo {
			row, ok := data[id] // absent: deleted since, or not this tenant's — nothing to record
			if !ok {
				continue
			}
			// Fire BEFORE the audit line: that line is the "handled" mark, so a failed emit leaves the
			// record unmarked and the retry fires it again. At-least-once (spec FR-035): a record can
			// fire twice when the second consumer (BPM) or the audit write fails after the first
			// (workflow) already fired, or when the process dies in between.
			if p.RunAutomations {
				if err := fireImportAutomations(ctx, automation, job.TenantID, p, id, row, logger); err != nil {
					return fmt.Errorf("services.ImportEventsJobHandler: automations %s: %w", id, err)
				}
			}
			if err := aud.InsertEvent(ctx, audit.Event{
				TenantID: job.TenantID, ActorUserID: p.UserID, EntityName: p.EntityName, RecordID: id,
				Action: audit.ActionCreate, Diff: redactSensitiveDiff(specs, audit.ComputeDiff(nil, row)),
			}); err != nil {
				return fmt.Errorf("services.ImportEventsJobHandler: audit %s: %w", id, err)
			}
		}
		return nil
	}
}
