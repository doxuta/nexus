-- RECORD-NOTES (docs/NEXUS-MOT-XUONG-2026-07-29.md §3d mục 1 "Trao đổi trên hồ sơ") — sys_record_notes
-- is the ENGINE-level notes/comments capability for EVERY entity (not a CRM-only feature): a free-text
-- note a user attaches to any record so colleagues have somewhere to write "đã gọi, khách hẹn tuần
-- sau" without polluting the technical audit-log ("Lịch sử" tab tracks field changes, not conversation).
--
-- Shape mirrors sys_files (migration 000023): one row per note, tenant_id + entity_name + record_id
-- link it back to the parent record whose tenant + A5 row-scope governs who may read/write it — the
-- API layer (services.RecordService.ResolveNoteTarget) always re-authorizes against the parent record
-- before touching this table, so there is deliberately NO extra ACL column here. created_by stamps the
-- real authenticated author (never a placeholder) so the delete rule "author OR entity delete-grant"
-- has something to compare against. updated_at is included for the standard transactional-table
-- contract (rules/database.md) even though no endpoint edits a note today (only create/delete) — a
-- future edit endpoint costs zero extra migration work.
--
-- Multi-tenant invariant: every query against sys_record_notes MUST filter tenant_id (rules: đa tổ
-- chức). No hard FK on tenant_id/record_id by design, same rationale as sys_files: notes are managed
-- entirely by the application layer, which always pins tenant_id, and a note's lifecycle is decoupled
-- from the obj_* row (a soft-deleted record keeps its notes until an explicit cleanup). MySQL 8 /
-- InnoDB / utf8mb4.
CREATE TABLE IF NOT EXISTS sys_record_notes (
    id          CHAR(36)     NOT NULL,
    tenant_id   CHAR(36)     NOT NULL,
    entity_name VARCHAR(100) NOT NULL,                    -- parent entity (e.g. "crm_accounts")
    record_id   CHAR(36)     NOT NULL,                    -- parent record the note is attached to
    body        TEXT         NOT NULL,                    -- free-text note content
    created_by  CHAR(36)     NOT NULL,                    -- real authenticated author (audit + delete rule)
    created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP    NULL DEFAULT NULL,            -- soft-delete tombstone
    PRIMARY KEY (id),
    -- Hot path: "notes on this record, newest first" — tenant + entity + record lead, sorted tail.
    KEY idx_record_notes_lookup (tenant_id, entity_name, record_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
