-- auto_number field type — per-entity per-tenant atomic sequence (ENG #6, docs/eng-buildout-plan.md
-- §"#6 auto_number").
--
-- sys_sequences holds exactly one counter row per (tenant_id, entity_name, field_name) triple.
-- next_val is the value that will be handed out to the NEXT claim (Salesforce autonumber
-- semantics: the stored value is always one AHEAD of the last issued number). A fresh triple has
-- no row at all — the claim query (dynrepo.ClaimSequence) INSERTs it lazily on first use rather
-- than requiring a pre-seeded row, so adding an auto_number field never needs a migration-time
-- backfill.
--
-- PRIMARY KEY choice: PRIMARY KEY(tenant_id, entity_name, field_name) — no surrogate CHAR(36) id
-- column. Two reasons this is the right call here (unlike every obj_*/sys_* transactional table,
-- which DOES carry a UUID id per rules/database.md):
--   1. The triple IS the row's whole identity — a sequence counter has no other attribute an id
--      would ever address independently of (tenant, entity, field). There is nothing to look up
--      BY id; every read/write is always keyed by the triple.
--   2. The atomic claim (INSERT ... ON DUPLICATE KEY UPDATE) needs a UNIQUE key to detect "this
--      triple already has a row" — the spec's requested UNIQUE(tenant_id, entity_name, field_name)
--      constraint. Making that unique key the row's OWN primary key (rather than a second index
--      alongside a surrogate id) avoids maintaining two indexes for one fact and lets InnoDB use
--      the clustered index directly for both the lookup and the update.
--
-- Atomic claim pattern (dynrepo.ClaimSequence): a single parameterized statement claims the next
-- number AND advances the counter, using MySQL's LAST_INSERT_ID(expr) side-effect so the caller
-- reads the claimed value off the driver's LastInsertId() result — no follow-up SELECT, no extra
-- round trip, safe under concurrent claims for the SAME triple (row-level lock on the UPSERT).
-- See internal/dynrepo/sequence.go for the exact statement and why the literal
-- "VALUES (?,?,?,2) ON DUPLICATE KEY UPDATE next_val = LAST_INSERT_ID(next_val) + 1" shape would
-- return 0 (not 1) on a brand-new triple's first claim, and how LAST_INSERT_ID(1) fixes it.
--
-- Gaps-allowed by design (DECISION, eng-buildout-plan.md §"#6"): a claim happens in
-- services.RecordService.Create OUTSIDE the InsertRecord transaction. If InsertRecord then fails
-- (e.g. a different unique field collides), the claimed number is burned — no record ever uses it,
-- and the next successful create skips straight over it. This mirrors Salesforce's own Auto Number
-- field behaviour and is documented, not accidental; a future `options.gapFree` could add a
-- single-transaction claim+insert path if a tenant needs legally gap-free numbering, but that is
-- explicitly NOT built now (RM-41-style discipline: land the common case, defer the narrow one).
--
-- MySQL 8 / InnoDB / utf8mb4. Idempotent (CREATE TABLE IF NOT EXISTS) per rules/database.md.
CREATE TABLE IF NOT EXISTS sys_sequences (
    tenant_id   CHAR(36)        NOT NULL,
    entity_name VARCHAR(100)    NOT NULL,
    field_name  VARCHAR(100)    NOT NULL,
    next_val    BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at  TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, entity_name, field_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
