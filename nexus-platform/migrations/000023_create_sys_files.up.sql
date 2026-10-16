-- File-upload M1 — sys_files. Backs the `file` / `image` dynamic field types (ENGINE-COVERAGE-SPEC
-- §EPIC CROSS "File / media / document attachment", P0). One row per uploaded blob: the binary
-- itself lives on the configured StorageBackend (local dir today, S3 later) at `path`, while this
-- table is the tenant-scoped metadata catalog the API reads to list/serve/delete attachments. A
-- `file`/`image` field on a record stores only the lightweight reference [{id,name}] inside the
-- row's JSON `data`; the heavy metadata (size, mime, storage path) is normalized here.
--
-- Multi-tenant invariant: every query against sys_files MUST filter tenant_id (rules: đa tổ chức).
-- entity_name / record_id link the file back to the record it is attached to (the parent whose
-- tenant + A5 row-scope governs who may read/delete it); field_name is the `file`/`image` field the
-- attachment belongs to. name is the ORIGINAL client filename (display only — never used to build a
-- storage path, which is server-generated from the id to defeat path traversal). size is the byte
-- count, mime the detected/declared content type. created_by stamps the uploader (audit). No hard FK
-- on tenant_id / record_id by design — file rows are managed by the application layer (which always
-- pins tenant_id) and an attachment's lifecycle is decoupled from the obj_* row (a soft-deleted
-- record keeps its blobs until an explicit cleanup). MySQL 8 / InnoDB / utf8mb4.
CREATE TABLE IF NOT EXISTS sys_files (
    id          CHAR(36)      NOT NULL,
    tenant_id   CHAR(36)      NOT NULL,
    entity_name VARCHAR(100)  NOT NULL,                    -- parent entity (e.g. "patients")
    record_id   CHAR(36)      NOT NULL,                    -- parent record the file is attached to
    field_name  VARCHAR(100)  NOT NULL,                    -- the file/image field the attachment belongs to
    name        VARCHAR(255)  NOT NULL,                    -- original client filename (display only)
    size        BIGINT        NOT NULL DEFAULT 0,          -- byte count
    mime        VARCHAR(150)  NOT NULL DEFAULT '',         -- content type
    path        VARCHAR(500)  NOT NULL,                    -- storage backend key/relative path
    created_by  CHAR(36)      NOT NULL,                    -- uploader (audit)
    created_at  TIMESTAMP(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at  TIMESTAMP(3)  DEFAULT NULL,                -- soft-delete tombstone
    PRIMARY KEY (id),
    -- Hot path: "files attached to this record's field" — tenant + entity + record + field, live first.
    KEY idx_files_record (tenant_id, entity_name, record_id, field_name, deleted_at),
    -- Tenant-scoped single-file lookup (GET /files/:id always re-pins tenant_id in the WHERE).
    KEY idx_files_tenant (tenant_id, deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
