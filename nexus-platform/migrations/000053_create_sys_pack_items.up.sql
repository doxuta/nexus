-- KHUON-SONG-P1 (docs/pack-provenance.md §2): per-item pack provenance.
-- One row per (tenant, pack, item): the item's content EXACTLY as the pack shipped it at the
-- install that last WROTE the item. This is the non-backfillable ancestor for the future
-- pack-upgrade 3-way merge; the read-only diff report derives "modified" by comparing this
-- content against a live re-serialization (never a maintained flag).
--   live_id  : anchor to the live record when the create path returned an id (rename-stable
--              resolution for the future apply step; phase 1 never reads it). NULL when the
--              item was skipped (pre-existing) or the pass returns no id.
--   protected: reserved noupdate-style user-intent flag for the apply step (always 0 in P1).
-- No content_hash column (all comparisons happen on canonicalized forms computed at read time)
-- and no install_id column (ledger writes are fail-open so the FK could dangle by design;
-- pack_slug+pack_version identify the writing install).
CREATE TABLE IF NOT EXISTS sys_pack_items (
    id            CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id     CHAR(36)     NOT NULL,
    pack_slug     VARCHAR(150) NOT NULL,
    pack_version  VARCHAR(50)  NOT NULL,
    item_type     VARCHAR(24)  NOT NULL,
    item_key      VARCHAR(191) NOT NULL,
    live_id       CHAR(36)     NULL,
    protected     TINYINT(1)   NOT NULL DEFAULT 0,
    content       JSON         NOT NULL,
    created_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_packitem (tenant_id, pack_slug, item_type, item_key),
    INDEX idx_packitem_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
