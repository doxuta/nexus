package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/metaversion"
)

// Tables, fields and N-N relations born after the last publish (specs/063 FR-060) are live rows hidden from
// the customer by the schema cutoff; a plain one has NO draft (a draft only carries edits to a field that
// already exists, publish_adapters_field.go; an N-N relation is registered at once, relation_service.go).
// Without this file nothing named them: pendingCount stayed 0, the change list stayed empty and Publish
// answered NOTHING_TO_PUBLISH, so the builder had something the customer could not see and no way to release
// it. They are the "pending" set too — synthesised here as read-only rows, counted, listed, fingerprinted and
// released by the next publish (which moves the cutoff past them and records each as a "create").
// Discarding one means deleting it, not dropping a draft.

// hiddenIDPrefix marks a synthetic row's id; it is not a UUID, so the discard route 404s it.
const hiddenIDPrefix = "new:"

// DocTypeRelationship names an N-N relation in the change list and the version history. It has no adapter
// (it is never drafted): a version doc of it carries no body and a rollback leaves it to the cutoff.
const DocTypeRelationship = "relationship"

func isHiddenNew(d metaversion.DraftRow) bool { return strings.HasPrefix(d.ID, hiddenIDPrefix) }

// bornRow is one live table / field / N-N relation created inside a cutoff window.
type bornRow struct {
	DocType    string    `db:"doc_type"`
	ID         string    `db:"id"`
	Label      string    `db:"label"`
	EntityName string    `db:"entity_name"`
	EntityIDs  string    `db:"entity_ids"` // the table(s) it hangs from, comma-separated ("" for a table)
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}

// schemaBornBetween lists what a customer stops / starts seeing when the schema cutoff moves from `after` to
// `upTo` (nil = no upper bound): the live tables created in (after, upTo], the fields created in it on tables
// that are NOT themselves in it (the table carries its own fields), and the N-N relations created in it
// between two such tables. A relation FIELD writes its own 1-N / 1-1 sys_relationships row: the field carries
// it, so only N-N rows (registered alone, POST /admin/relationships) are listed. Every statement pins
// tenant_id; created_at is a TIMESTAMP compared in SQL, so no time-zone round trip.
func schemaBornBetween(ctx context.Context, q sqlx.QueryerContext, tenantID string, after time.Time, upTo *time.Time) ([]bornRow, error) {
	var hi any
	if upTo != nil {
		hi = *upTo
	}
	var out, rows []bornRow
	if err := sqlx.SelectContext(ctx, q, &rows, `SELECT 'entity' AS doc_type, id, label, name AS entity_name, '' AS entity_ids, created_at, updated_at
		FROM sys_entities WHERE tenant_id = ? AND deleted_at IS NULL AND created_at > ? AND (? IS NULL OR created_at <= ?)
		ORDER BY created_at, id`, tenantID, after, hi, hi); err != nil {
		return nil, fmt.Errorf("services.schemaBornBetween: tables: %w", err)
	}
	out = append(out, rows...)
	rows = nil
	if err := sqlx.SelectContext(ctx, q, &rows, `SELECT 'field' AS doc_type, f.id, CONCAT(f.label, ' · ', e.name) AS label, e.name AS entity_name,
			e.id AS entity_ids, f.created_at, f.updated_at
		FROM sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id AND e.deleted_at IS NULL
		WHERE f.tenant_id = ? AND f.deleted_at IS NULL AND f.created_at > ? AND (? IS NULL OR f.created_at <= ?) AND e.created_at <= ?
		ORDER BY f.created_at, f.id`, tenantID, after, hi, hi, after); err != nil {
		return nil, fmt.Errorf("services.schemaBornBetween: fields: %w", err)
	}
	out = append(out, rows...)
	rows = nil
	if err := sqlx.SelectContext(ctx, q, &rows, `SELECT 'relationship' AS doc_type, r.id, CONCAT(r.name, ' · ', pe.name, ' ↔ ', ce.name) AS label,
			pe.name AS entity_name, CONCAT(pe.id, ',', ce.id) AS entity_ids, r.created_at, r.updated_at
		FROM sys_relationships r
		JOIN sys_entities pe ON pe.tenant_id = r.tenant_id AND pe.name = r.parent_entity_name AND pe.deleted_at IS NULL
		JOIN sys_entities ce ON ce.tenant_id = r.tenant_id AND ce.name = r.child_entity_name AND ce.deleted_at IS NULL
		WHERE r.tenant_id = ? AND r.deleted_at IS NULL AND r.relationship_type = 'N-N' AND r.created_at > ? AND (? IS NULL OR r.created_at <= ?)
			AND pe.created_at <= ? AND ce.created_at <= ?
		ORDER BY r.created_at, r.id`, tenantID, after, hi, hi, after, after); err != nil {
		return nil, fmt.Errorf("services.schemaBornBetween: relationships: %w", err)
	}
	return append(out, rows...), nil
}

// schemaRowsBetween is EVERY schema definition created in (after, upTo]: every table, every field (those of a
// new table too) and every sys_relationships row of any type. The change list (schemaBornBetween) folds some of
// them into a parent row — a new table's fields and relations, the 1-N row a relation field writes — but the
// cutoff a publish writes releases all of them, so the cutoff is derived from this set and the re-read compares
// it. Deriving it from the listed rows left a field added to a new table a second later hidden after the table
// was published (a required one made the customer's create fail), with nothing left to publish.
func schemaRowsBetween(ctx context.Context, q sqlx.QueryerContext, tenantID string, after time.Time, upTo *time.Time) ([]bornRow, error) {
	var hi any
	if upTo != nil {
		hi = *upTo
	}
	var rows []bornRow
	if err := sqlx.SelectContext(ctx, q, &rows, `
		SELECT 'entity' AS doc_type, id, created_at FROM sys_entities
			WHERE tenant_id = ? AND deleted_at IS NULL AND created_at > ? AND (? IS NULL OR created_at <= ?)
		UNION ALL SELECT 'field', id, created_at FROM sys_fields
			WHERE tenant_id = ? AND deleted_at IS NULL AND created_at > ? AND (? IS NULL OR created_at <= ?)
		UNION ALL SELECT 'relationship', id, created_at FROM sys_relationships
			WHERE tenant_id = ? AND deleted_at IS NULL AND created_at > ? AND (? IS NULL OR created_at <= ?)`,
		tenantID, after, hi, hi, tenantID, after, hi, hi, tenantID, after, hi, hi); err != nil {
		return nil, fmt.Errorf("services.schemaRowsBetween: %w", err)
	}
	return rows, nil
}

// releasedBy keeps the hidden-new rows the cutoff actually uncovers. A row stamped after it (in the second still
// running under the lock, or ahead of the publisher's clock) stays hidden and pending, so it is not recorded as
// released.
func releasedBy(hidden []metaversion.DraftRow, cutoff time.Time) []metaversion.DraftRow {
	out := make([]metaversion.DraftRow, 0, len(hidden))
	for _, r := range hidden {
		if !r.CreatedAt.After(cutoff) {
			out = append(out, r)
		}
	}
	return out
}

// headCutoffOn is the newest numbered version's cutoff read on q (the publish tx sees its own snapshot);
// nil = no version, or a legacy version without a cutoff, which hides nothing.
func (s *PublishService) headCutoffOn(ctx context.Context, q sqlx.QueryerContext, tenantID string) (*time.Time, error) {
	head, err := s.versions.HeadVersion(ctx, q, tenantID)
	if err != nil {
		return nil, err
	}
	if head == nil {
		return nil, nil
	}
	return head.SchemaCutoffAt, nil
}

// pending is what the builder is asked to publish: the drafts plus the hidden-new rows, and head the head's
// cutoff (nil: nothing is hidden).
func (s *PublishService) pending(ctx context.Context, q sqlx.QueryerContext, tenantID string, drafts []metaversion.DraftRow) (rows []metaversion.DraftRow, head *time.Time, err error) {
	rows = append([]metaversion.DraftRow(nil), drafts...)
	if head, err = s.headCutoffOn(ctx, q, tenantID); err != nil || head == nil {
		return rows, head, err
	}
	born, err := schemaBornBetween(ctx, q, tenantID, *head, nil)
	if err != nil {
		return nil, nil, err
	}
	have, going := map[string]bool{}, map[string]bool{}
	for _, d := range drafts {
		have[d.DocType+"/"+d.DocKey] = true
		if d.DocType == DocTypeEntity && d.Op == metaversion.OpDelete {
			going[d.DocKey] = true
		}
	}
	for _, r := range born {
		if have[r.DocType+"/"+r.ID] || hangsFromAny(r.EntityIDs, going) {
			continue // a draft of the same document is already listed / its table is being deleted
		}
		row := metaversion.DraftRow{ID: hiddenIDPrefix + r.DocType + ":" + r.ID, TenantID: tenantID, DocType: r.DocType, DocKey: r.ID, Op: metaversion.OpUpsert,
			Label: r.Label, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
		if r.EntityName != "" {
			name := r.EntityName
			row.EntityName = &name
		}
		rows = append(rows, row)
	}
	return rows, head, nil
}

func hangsFromAny(ids string, set map[string]bool) bool {
	for _, id := range strings.Split(ids, ",") {
		if set[id] {
			return true
		}
	}
	return false
}

// pendingRows is pending without the cutoff bookkeeping (state, change list, discard, rollback guard).
func (s *PublishService) pendingRows(ctx context.Context, q sqlx.QueryerContext, tenantID string, drafts []metaversion.DraftRow) ([]metaversion.DraftRow, error) {
	rows, _, err := s.pending(ctx, q, tenantID, drafts)
	return rows, err
}

// splitHidden separates the drafts from the synthetic rows of a pendingRows result.
func splitHidden(rows []metaversion.DraftRow) (drafts, hidden []metaversion.DraftRow) {
	for _, r := range rows {
		if isHiddenNew(r) {
			hidden = append(hidden, r)
		} else {
			drafts = append(drafts, r)
		}
	}
	return drafts, hidden
}

// settleSecond runs before a version takes the tenant lock (#13). When a definition was born in the second still
// running, or is stamped ahead of this clock, it waits for that second to end, so that the cutoff the version then
// takes under the lock (never past the last second that is over) releases a table created just before the click.
// Never longer than a second, and never holding a lock: schema writes and draft saves do not wait for it.
func (s *PublishService) settleSecond(ctx context.Context, tenantID string) error {
	last := time.Now().UTC().Truncate(time.Second).Add(-time.Second)
	born, err := schemaRowsBetween(ctx, s.db, tenantID, last, nil)
	if err == nil && len(born) > 0 {
		time.Sleep(time.Until(last.Add(2 * time.Second))) // to the end of the running second
	}
	return err
}

// releaseCutoff is the schema cutoff a version writes, read on its tx q under the tenant lock: the second of the
// newest of ALL the definitions it read as born after the head (schemaRowsBetween — listed or folded into a listed
// row, so it releases what the builder was shown and everything that hangs from it), but never past the last
// second that is over; the head's own when it read none. Never the clock (#19): the cutoff's own second counts as
// released, so a cutoff naming the running second released a table born later in it that no version read.
// Exact because every schema writer takes the tenant row FOR SHARE before it stamps (#13,
// metadata.lockTenantForSchemaWrite): an INSERT committed before the lock is in the version's snapshot (the
// snapshot is taken at the first plain read, after the lock), and one that waited for the lock is stamped after
// the commit, in a second later than the cutoff. A row stamped in the running second (the wait is settleSecond's,
// before the lock) or ahead of this clock (another process's) stays hidden and pending. The re-read on the pool
// still refuses DRAFTS_CHANGED when the window moved: a delete committed meanwhile, or a writer that bypasses the
// repository. head nil (the baseline, or a head written before cutoffs) hid nothing: every live definition is
// released, and a tenant with none gets the last second that is over.
// ponytail: one clock. A writer on another replica whose clock runs behind the publisher's can still stamp at or
// before the cutoff after the publish committed; flooring the stamp at the tenant's newest cutoff + 1 s, read
// under the writer's lock, would close it.
func (s *PublishService) releaseCutoff(ctx context.Context, q sqlx.QueryerContext, tenantID string, head *time.Time) (time.Time, error) {
	after := time.Unix(0, 0).UTC() // before every TIMESTAMP: with no head cutoff, all is born after it
	if head != nil {
		after = head.UTC().Truncate(time.Second)
	}
	born, err := schemaRowsBetween(ctx, q, tenantID, after, nil)
	if err != nil {
		return time.Time{}, err
	}
	last := time.Now().UTC().Truncate(time.Second).Add(-time.Second) // the newest second that is over
	cut := newestBorn(born, after, &last)
	if !cut.After(after) {
		if head == nil {
			return last, nil
		}
		return after, nil
	}
	fresh, err := schemaRowsBetween(ctx, s.db, tenantID, after, &cut)
	if err != nil {
		return time.Time{}, err
	}
	if bornKeys(born, cut) != bornKeys(fresh, cut) {
		return time.Time{}, &ValidationError{Code: "DRAFTS_CHANGED", Msg: "Bản nháp đã đổi, đã tải lại danh sách"}
	}
	return cut, nil
}

// newestBorn is the second of the newest row stamped after `after` (and not after upTo, when set); `after` if none is.
func newestBorn(rows []bornRow, after time.Time, upTo *time.Time) time.Time {
	cut := after
	for _, r := range rows {
		if c := r.CreatedAt.UTC().Truncate(time.Second); c.After(cut) && (upTo == nil || !c.After(*upTo)) {
			cut = c
		}
	}
	return cut
}

// bornKeys is the sorted "type/id" set of the rows born at or before cut.
func bornKeys(rows []bornRow, cut time.Time) string {
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		if !r.CreatedAt.After(cut) {
			keys = append(keys, r.DocType+"/"+r.ID)
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
