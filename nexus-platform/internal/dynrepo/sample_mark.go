package dynrepo

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Sample-data mark (specs/064-preview-as-role-sample-data S9-4). A record the builder seeds as
// SAMPLE data gets one row in the side table sys_sample_records, written in the SAME transaction
// as the obj_<entity> row by InsertRecord / InsertRecordsAtomic. The mark rides on ctx (the
// precedent is workflow.WithChainDepth): only the sample-data service attaches it, never a client
// parameter, so an ordinary create cannot forge one.

// Sample sources stored in sys_sample_records.source (VARCHAR(16)).
const (
	SampleSourceAI        = "ai"
	SampleSourceGenerator = "generator"
	SampleSourcePack      = "pack"
)

// SampleMark is what WithSampleMark puts on ctx: the batch (one AI apply / generator run / pack
// install) and where the rows came from.
type SampleMark struct {
	BatchID string
	Source  string
}

type sampleMarkKey struct{}

// WithSampleMark returns ctx carrying mark: every record inserted with it is marked as sample.
func WithSampleMark(ctx context.Context, mark SampleMark) context.Context {
	return context.WithValue(ctx, sampleMarkKey{}, mark)
}

// SampleMarkFrom returns the mark WithSampleMark put on ctx, or ok=false for an ordinary create.
func SampleMarkFrom(ctx context.Context) (SampleMark, bool) {
	m, ok := ctx.Value(sampleMarkKey{}).(SampleMark)
	return m, ok
}

// insertSampleMark writes the mark of record recordID in the caller's transaction. A failure (for
// example a source longer than VARCHAR(16), MySQL strict 1406) rolls the whole row back: there is
// never a sample row without its mark. A ctx without a mark is a no-op.
func insertSampleMark(ctx context.Context, ex pivotExecer, tenantID, entityName, recordID, userID string) error {
	mark, ok := SampleMarkFrom(ctx)
	if !ok {
		return nil
	}
	if _, err := ex.ExecContext(ctx,
		"INSERT INTO `sys_sample_records` (`id`, `tenant_id`, `entity_name`, `record_id`, `batch_id`, `source`, `created_by`) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		uuid.NewString(), tenantID, entityName, recordID, mark.BatchID, mark.Source, userID); err != nil {
		return fmt.Errorf("dynrepo.insertSampleMark: %w", err)
	}
	return nil
}

// excludeSamplesClause is the predicate ListParams.ExcludeSamples appends: the row has no live
// mark. Parameterised; args are (tenant, entity) and go AFTER the other predicates' args.
func excludeSamplesClause(table, tenantID, entityName string) (string, []any) {
	return fmt.Sprintf(" AND NOT EXISTS (SELECT 1 FROM `sys_sample_records` s WHERE s.`tenant_id` = ? AND s.`entity_name` = ? "+
		"AND s.`record_id` = `%s`.`id` AND s.`deleted_at` IS NULL)", table), []any{tenantID, entityName}
}

// SampleCounts is the per-entity tally behind the sample-data card.
type SampleCounts struct {
	Live     int            // marked rows still in use
	InBin    int            // marked rows in the recycle bin
	BySource map[string]int // marked rows per source (live + recycle bin)
}

// CountSampleRecords counts the tenant's marked rows of one entity. It JOINs the live obj_ table,
// so an orphan mark (its row hard-deleted) is never counted. Tenant-scoped on both sides.
func (r *Repository) CountSampleRecords(ctx context.Context, tenantID, entity string) (SampleCounts, error) {
	table, err := tableFor(entity)
	if err != nil {
		return SampleCounts{}, fmt.Errorf("dynrepo.CountSampleRecords: %w", err)
	}
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return SampleCounts{}, fmt.Errorf("dynrepo.CountSampleRecords: %w", err)
	}
	var rows []struct {
		Source string `db:"source"`
		InBin  bool   `db:"in_bin"`
		N      int    `db:"n"`
	}
	stmt := fmt.Sprintf("SELECT s.`source` AS source, (o.`deleted_at` IS NOT NULL) AS in_bin, COUNT(*) AS n "+
		"FROM `sys_sample_records` s JOIN `%s` o ON o.`id` = s.`record_id` AND o.`tenant_id` = s.`tenant_id` "+
		"WHERE s.`tenant_id` = ? AND s.`entity_name` = ? AND s.`deleted_at` IS NULL GROUP BY s.`source`, in_bin", table)
	if err := r.db.SelectContext(ctx, &rows, stmt, tenantID, entityName); err != nil {
		return SampleCounts{}, fmt.Errorf("dynrepo.CountSampleRecords: %w", err)
	}
	out := SampleCounts{BySource: map[string]int{}}
	for _, row := range rows {
		if row.InBin {
			out.InBin += row.N
		} else {
			out.Live += row.N
		}
		out.BySource[row.Source] += row.N
	}
	return out, nil
}

// SampleRootRow is one marked row a wipe works through: its id and the batch that seeded it.
type SampleRootRow struct {
	ID      string `db:"record_id"`
	BatchID string `db:"batch_id"`
}

// SampleRoots lists the tenant's marked rows of one entity, live and recycle bin both — the list a
// wipe works through (S9-7). It JOINs the live obj_ table, so an orphan mark is not a root. Ordered by
// id so a re-read after an interrupted wipe walks the same order.
func (r *Repository) SampleRoots(ctx context.Context, tenantID, entity string) ([]SampleRootRow, error) {
	table, err := tableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.SampleRoots: %w", err)
	}
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.SampleRoots: %w", err)
	}
	ids := []SampleRootRow{}
	if err := r.db.SelectContext(ctx, &ids, fmt.Sprintf("SELECT s.`record_id`, s.`batch_id` FROM `sys_sample_records` s "+
		"JOIN `%s` o ON o.`id` = s.`record_id` AND o.`tenant_id` = s.`tenant_id` "+
		"WHERE s.`tenant_id` = ? AND s.`entity_name` = ? AND s.`deleted_at` IS NULL ORDER BY s.`record_id`", table),
		tenantID, entityName); err != nil {
		return nil, fmt.Errorf("dynrepo.SampleRoots: %w", err)
	}
	return ids, nil
}

// SampleEntityNames lists the entity names the tenant has marks for (the card's default scope).
func (r *Repository) SampleEntityNames(ctx context.Context, tenantID string) ([]string, error) {
	var names []string
	if err := r.db.SelectContext(ctx, &names,
		"SELECT DISTINCT `entity_name` FROM `sys_sample_records` WHERE `tenant_id` = ? AND `deleted_at` IS NULL ORDER BY `entity_name`",
		tenantID); err != nil {
		return nil, fmt.Errorf("dynrepo.SampleEntityNames: %w", err)
	}
	return names, nil
}
