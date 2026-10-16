-- specs/063-draft-publish S7-1a: the draft -> publish layer for a delivered customer workspace.
-- Three tables only; no live table is altered, so a tenant with no numbered version behaves exactly
-- as before (draft mode stays OFF until the builder's first publish writes version 1).
--   sys_meta_drafts        one row per document the builder changed since the last publish
--   sys_meta_versions      append-only: baseline / publish / rollback / direct entries
--   sys_meta_version_docs  before/after of every document in a version
-- No foreign keys (like every sys_* table). deleted_at is kept for the transactional-table
-- convention but is NEVER set: drafts are hard-deleted on publish/discard (the UNIQUE key cannot
-- filter on deleted_at, same call as sys_layouts D4) and versions are append-only.
CREATE TABLE IF NOT EXISTS sys_meta_drafts (
  id          CHAR(36)     NOT NULL PRIMARY KEY,
  tenant_id   CHAR(36)     NOT NULL,
  doc_type    VARCHAR(40)  NOT NULL,
  doc_key     VARCHAR(191) NOT NULL,
  op          ENUM('upsert','delete') NOT NULL,
  draft_json  JSON         NULL,
  base_hash   CHAR(64)     NULL,
  entity_name VARCHAR(100) NULL,
  label       VARCHAR(255) NOT NULL DEFAULT '',
  created_by  CHAR(36)     NOT NULL,
  modified_by CHAR(36)     NULL,
  created_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at  TIMESTAMP(3) NULL DEFAULT NULL,
  UNIQUE KEY uq_meta_drafts_doc (tenant_id, doc_type, doc_key),
  KEY idx_meta_drafts_tenant (tenant_id, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sys_meta_versions (
  id               CHAR(36)     NOT NULL PRIMARY KEY,
  tenant_id        CHAR(36)     NOT NULL,
  seq              INT UNSIGNED NOT NULL,
  version_no       INT UNSIGNED NULL,
  kind             ENUM('baseline','publish','rollback','direct') NOT NULL,
  rollback_of      INT UNSIGNED NULL,
  note             VARCHAR(500) NOT NULL DEFAULT '',
  doc_count        INT UNSIGNED NOT NULL DEFAULT 0,
  covered_types    JSON         NULL,
  schema_cutoff_at TIMESTAMP(3) NULL,
  schema_status    ENUM('none','pending','done','failed') NOT NULL DEFAULT 'none',
  created_by       CHAR(36)     NOT NULL,
  created_by_label VARCHAR(191) NOT NULL DEFAULT '',
  created_at       TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at       TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at       TIMESTAMP(3) NULL DEFAULT NULL,
  UNIQUE KEY uq_meta_versions_seq (tenant_id, seq),
  UNIQUE KEY uq_meta_versions_no (tenant_id, version_no),
  KEY idx_meta_versions_created (tenant_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sys_meta_version_docs (
  id           CHAR(36)     NOT NULL PRIMARY KEY,
  tenant_id    CHAR(36)     NOT NULL,
  version_id   CHAR(36)     NOT NULL,
  seq          INT UNSIGNED NOT NULL,
  doc_type     VARCHAR(40)  NOT NULL,
  doc_key      VARCHAR(191) NOT NULL,
  label        VARCHAR(255) NOT NULL DEFAULT '',
  entity_name  VARCHAR(100) NULL,
  op           ENUM('create','update','delete','snapshot') NOT NULL,
  before_json  JSON NULL,
  after_json   JSON NULL,
  meta         JSON NULL,
  apply_status ENUM('applied','pending','failed') NOT NULL DEFAULT 'applied',
  apply_error  VARCHAR(500) NULL,
  created_by   CHAR(36)     NOT NULL,
  created_at   TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at   TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at   TIMESTAMP(3) NULL DEFAULT NULL,
  KEY idx_mvd_version (tenant_id, version_id),
  KEY idx_mvd_doc (tenant_id, doc_type, doc_key, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
