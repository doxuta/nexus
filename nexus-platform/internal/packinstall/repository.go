// Package packinstall persists the vertical-pack install ledger (sys_pack_installs): one row per
// successful install of a pack into a tenant. It is a system table (fixed schema) so it uses GORM
// per the stack rules, and every read is tenant-scoped. The vertical-pack service writes a row
// fail-open on each successful install and the admin API lists them for the Studio "installed
// versions" view.
package packinstall

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nexus/platform/internal/services/vertical_pack"
)

// newID mints a UUID for one provenance row.
func newID() string { return uuid.NewString() }

// Repository reads/writes the pack-install ledger. Tenant-scoped on every query.
type Repository struct {
	db *gorm.DB
}

// New builds a ledger repository over a GORM handle.
func New(db *gorm.DB) *Repository { return &Repository{db: db} }

// RecordInstall inserts one ledger row. It marshals the (small) InstallResult map into the
// result_json JSON column. Called fail-open by the installer — a returned error is logged, never
// surfaced to the caller as an install failure. installed_at is stored in UTC.
func (r *Repository) RecordInstall(ctx context.Context, rec vertical_pack.InstallRecord) error {
	var resultJSON []byte
	if rec.Result != nil {
		b, err := json.Marshal(rec.Result)
		if err != nil {
			return fmt.Errorf("packinstall.RecordInstall: marshal result: %w", err)
		}
		resultJSON = b
	}
	if err := r.db.WithContext(ctx).Exec(
		"INSERT INTO sys_pack_installs (id, tenant_id, slug, version, installed_at, result_json) VALUES (?, ?, ?, ?, ?, ?)",
		rec.ID, rec.TenantID, rec.Slug, rec.Version, rec.InstalledAt.UTC(), resultJSON,
	).Error; err != nil {
		return fmt.Errorf("packinstall.RecordInstall: insert: %w", err)
	}
	return nil
}

// ListInstalls returns the tenant's install ledger, newest first. Tenant-scoped.
func (r *Repository) ListInstalls(ctx context.Context, tenantID string) ([]vertical_pack.InstallRecord, error) {
	rows, err := r.db.WithContext(ctx).Raw(
		"SELECT id, slug, version, installed_at, result_json FROM sys_pack_installs "+
			"WHERE tenant_id = ? ORDER BY installed_at DESC, id DESC",
		tenantID,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("packinstall.ListInstalls: query: %w", err)
	}
	defer rows.Close()

	var out []vertical_pack.InstallRecord
	for rows.Next() {
		var (
			id, slug, version string
			installedAt       time.Time
			resultJSON        []byte
		)
		if err := rows.Scan(&id, &slug, &version, &installedAt, &resultJSON); err != nil {
			return nil, fmt.Errorf("packinstall.ListInstalls: scan: %w", err)
		}
		rec := vertical_pack.InstallRecord{
			ID:          id,
			TenantID:    tenantID,
			Slug:        slug,
			Version:     version,
			InstalledAt: installedAt,
		}
		if len(resultJSON) > 0 {
			var m map[string]any
			if err := json.Unmarshal(resultJSON, &m); err == nil {
				rec.Result = m
			}
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("packinstall.ListInstalls: rows: %w", err)
	}
	return out, nil
}

// ── KHUON-SONG-P1: per-item pack provenance (sys_pack_items) ────────────────────────────────

// SaveItems persists one install's provenance batch (../specs/011-pack-provenance/spec.md §4.1). Write
// semantics per item (vet BLOCKER 3 — protect the 3-way-merge ancestor):
//   - Wrote  → upsert: this install actually wrote the live item, so the row must reflect the
//     content that produced it (new ancestor).
//   - !Wrote → insert-if-absent: the item pre-existed (skip pass); a row recorded by the install
//     that DID write it must never be overwritten with a later pack's expectation.
//
// Tenant-scoped by the unique key (tenant_id, pack_slug, item_type, item_key). live_id is stored
// NULL when the create path returned no id. protected is reserved (always 0 here).
func (r *Repository) SaveItems(ctx context.Context, tenantID, slug, version string, items []vertical_pack.PackItem) error {
	for _, it := range items {
		liveID := any(nil)
		if it.LiveID != "" {
			liveID = it.LiveID
		}
		var err error
		if it.Wrote {
			err = r.db.WithContext(ctx).Exec(
				"INSERT INTO sys_pack_items (id, tenant_id, pack_slug, pack_version, item_type, item_key, live_id, content) "+
					"VALUES (?, ?, ?, ?, ?, ?, ?, ?) "+
					"ON DUPLICATE KEY UPDATE pack_version = VALUES(pack_version), live_id = COALESCE(VALUES(live_id), live_id), content = VALUES(content)",
				newID(), tenantID, slug, version, it.Type, it.Key, liveID, []byte(it.Content),
			).Error
		} else {
			err = r.db.WithContext(ctx).Exec(
				"INSERT IGNORE INTO sys_pack_items (id, tenant_id, pack_slug, pack_version, item_type, item_key, live_id, content) "+
					"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
				newID(), tenantID, slug, version, it.Type, it.Key, liveID, []byte(it.Content),
			).Error
		}
		if err != nil {
			return fmt.Errorf("packinstall.SaveItems: %s/%s: %w", it.Type, it.Key, err)
		}
	}
	return nil
}

// ListItems returns the tenant's provenance rows for one pack slug (diff report input).
func (r *Repository) ListItems(ctx context.Context, tenantID, slug string) ([]vertical_pack.StoredPackItem, error) {
	rows, err := r.db.WithContext(ctx).Raw(
		"SELECT item_type, item_key, pack_version, content, live_id, protected FROM sys_pack_items "+
			"WHERE tenant_id = ? AND pack_slug = ? ORDER BY item_type, item_key",
		tenantID, slug,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("packinstall.ListItems: query: %w", err)
	}
	defer rows.Close()
	var out []vertical_pack.StoredPackItem
	for rows.Next() {
		var it vertical_pack.StoredPackItem
		var content []byte
		var liveID sql.NullString
		if err := rows.Scan(&it.Type, &it.Key, &it.PackVersion, &content, &liveID, &it.Protected); err != nil {
			return nil, fmt.Errorf("packinstall.ListItems: scan: %w", err)
		}
		it.Content = content
		it.LiveID = liveID.String
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("packinstall.ListItems: rows: %w", err)
	}
	return out, nil
}

// ListItemKeysOtherSlugs returns the (slug, type, key) of every provenance row the tenant holds
// for packs OTHER than slug — the upgrade plan's "dùng chung với gói <mã>" lookup (FR-015).
// Tenant-scoped: another tenant's rows are never visible.
func (r *Repository) ListItemKeysOtherSlugs(ctx context.Context, tenantID, slug string) ([]vertical_pack.OtherPackItem, error) {
	rows, err := r.db.WithContext(ctx).Raw(
		"SELECT pack_slug, item_type, item_key FROM sys_pack_items WHERE tenant_id = ? AND pack_slug <> ?",
		tenantID, slug,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("packinstall.ListItemKeysOtherSlugs: query: %w", err)
	}
	defer rows.Close()
	var out []vertical_pack.OtherPackItem
	for rows.Next() {
		var it vertical_pack.OtherPackItem
		if err := rows.Scan(&it.Slug, &it.Type, &it.Key); err != nil {
			return nil, fmt.Errorf("packinstall.ListItemKeysOtherSlugs: scan: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("packinstall.ListItemKeysOtherSlugs: rows: %w", err)
	}
	return out, nil
}
