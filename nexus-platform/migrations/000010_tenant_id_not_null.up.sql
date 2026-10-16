-- RM-18 tenant-leak hardening: enforce NOT NULL on sys_fields.tenant_id and
-- sys_relationships.tenant_id. Closes the structural isolation debt introduced by
-- migration 000004 (which added the columns as NULLABLE for safe backfill).
--
-- Safety contract:
--   1. DEFENSIVE BACKFILL runs first (idempotent — WHERE tenant_id IS NULL, so it is a
--      safe no-op if already clean).
--   2. MODIFY NOT NULL runs after. If any NULL survives the backfill (e.g. an orphan
--      sys_fields row whose entity_id has no matching sys_entities row), MySQL will refuse
--      the ALTER with ERROR 1138 (invalid use of NULL value) — this is intentional
--      fail-fast behaviour. Do NOT supply a default; a silent coercion would be worse.
--   3. sys_relationships assumption: the table was empty at migration 000004 time and
--      remains empty pre-M3 (no writer sets rows into it today; relations live in
--      sys_fields.options.relatedEntity). The backfill JOIN cannot resolve a tenant for
--      a relationship row whose parent entity does not exist, so we rely on the MODIFY
--      failing loudly in that case. If the table has rows from M3 work, apply the
--      backfill manually before running this migration (see RM-18 runbook).
--
-- Down (000010.down.sql): reverses both MODIFY back to CHAR(36) NULL.

-- Step 1a: backfill sys_fields from the entity that owns each field.
-- Uses LEFT JOIN + IS NULL guard so rows already populated are not touched.
-- sys_entities.deleted_at IS NOT NULL rows are included (the entity tenant_id is correct
-- even after a soft-delete, and we need to recover any field whose entity was deleted).
UPDATE sys_fields f
    JOIN sys_entities e ON e.id = f.entity_id
    SET f.tenant_id = e.tenant_id
    WHERE f.tenant_id IS NULL;

-- Step 1b: backfill sys_relationships from parent entity, if any NULL rows exist.
-- WARNING: the table is expected to be empty (0 rows pre-M3). If rows DO exist and have
-- NULL tenant_id, we join by parent_entity_name. Because entity names are NOT globally
-- unique (two tenants may share a name), a cross-tenant JOIN could match multiple rows and
-- MySQL would assign an ARBITRARY tenant_id — silently wrong. Manual pre-backfill is
-- required before running this migration when sys_relationships is non-empty. The MODIFY
-- below will fail loudly only for unresolved NULLs (no match), NOT for wrong-tenant
-- assignments. Safest approach if M3 rows exist: set tenant_id explicitly before running.
-- ASYMMETRY (deliberate): unlike Step 1a, this JOIN adds `e.deleted_at IS NULL` — for the
-- inert relationships table we resolve tenant ONLY from live parents to shrink the
-- name-collision surface above; any row whose parent is soft-deleted stays NULL and is
-- caught loudly by the MODIFY. Step 1a is lenient instead, to avoid orphaning real fields
-- whose entity was soft-deleted (their tenant_id is still correct).
UPDATE sys_relationships r
    JOIN sys_entities e ON e.name = r.parent_entity_name AND e.deleted_at IS NULL
    SET r.tenant_id = e.tenant_id
    WHERE r.tenant_id IS NULL;

-- Step 2a: enforce NOT NULL on sys_fields.tenant_id.
-- This WILL fail if any NULL rows remain after the backfill above (orphan field rows
-- with no matching entity). That failure is the correct behaviour — do not suppress it.
ALTER TABLE sys_fields
    MODIFY COLUMN tenant_id CHAR(36) NOT NULL;

-- Step 2b: enforce NOT NULL on sys_relationships.tenant_id.
ALTER TABLE sys_relationships
    MODIFY COLUMN tenant_id CHAR(36) NOT NULL;
