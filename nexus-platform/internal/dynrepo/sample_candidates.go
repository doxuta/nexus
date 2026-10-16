package dynrepo

import (
	"context"
	"fmt"
)

// LiveRecordIDs returns up to limit ids of the tenant's live (not in the recycle bin) records of
// entity, the ones carrying a live sample mark first. It feeds the sample-data generator the
// parent rows a new child row can point at (specs/064-preview-as-role-sample-data S9-5).
// Two primary-key-ordered queries (marked, then unmarked) instead of one ORDER BY over a join, so
// the cost stays O(limit) however large the table is.
func (r *Repository) LiveRecordIDs(ctx context.Context, tenantID, entity string, limit int) ([]string, error) {
	table, err := tableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.LiveRecordIDs: %w", err)
	}
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.LiveRecordIDs: %w", err)
	}
	var ids []string
	marked := fmt.Sprintf("SELECT o.`id` FROM `%s` o JOIN `sys_sample_records` s ON s.`tenant_id` = o.`tenant_id` AND s.`entity_name` = ? "+
		"AND s.`record_id` = o.`id` AND s.`deleted_at` IS NULL WHERE o.`tenant_id` = ? AND o.`deleted_at` IS NULL ORDER BY o.`id` LIMIT ?", table)
	if err := r.db.SelectContext(ctx, &ids, marked, entityName, tenantID, limit); err != nil {
		return nil, fmt.Errorf("dynrepo.LiveRecordIDs: %w", err)
	}
	if len(ids) >= limit {
		return ids, nil
	}
	var rest []string
	unmarked := fmt.Sprintf("SELECT o.`id` FROM `%s` o WHERE o.`tenant_id` = ? AND o.`deleted_at` IS NULL AND NOT EXISTS "+
		"(SELECT 1 FROM `sys_sample_records` s WHERE s.`tenant_id` = ? AND s.`entity_name` = ? AND s.`record_id` = o.`id` AND s.`deleted_at` IS NULL) "+
		"ORDER BY o.`id` LIMIT ?", table)
	if err := r.db.SelectContext(ctx, &rest, unmarked, tenantID, tenantID, entityName, limit-len(ids)); err != nil {
		return nil, fmt.Errorf("dynrepo.LiveRecordIDs: %w", err)
	}
	return append(ids, rest...), nil
}

// ActiveUserIDs returns up to limit ids of the tenant's active users (candidates for a required
// `user` field of a generated row).
func (r *Repository) ActiveUserIDs(ctx context.Context, tenantID string, limit int) ([]string, error) {
	var ids []string
	if err := r.db.SelectContext(ctx, &ids,
		"SELECT `id` FROM `users` WHERE `tenant_id` = ? AND `status` = 'active' AND `deleted_at` IS NULL ORDER BY `id` LIMIT ?",
		tenantID, limit); err != nil {
		return nil, fmt.Errorf("dynrepo.ActiveUserIDs: %w", err)
	}
	return ids, nil
}
