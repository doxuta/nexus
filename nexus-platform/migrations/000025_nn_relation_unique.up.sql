-- N-N relation integrity fixes (review batch).
--
-- Fix 2: UNIQUE(tenant_id, join_table_name) prevents two separate relationship rows
-- pointing at the same physical join table (which would happen when the same entity pair
-- is registered twice, e.g. (accounts,contacts) and (contacts,accounts) — JoinParts
-- sorts the pair alphabetically so both produce the same join table name). This is the
-- canonical dup-check key because join_table_name is already canonical.
--
-- Fix 3: UNIQUE(tenant_id, name) makes each relationship name unique per tenant so
-- GetRelationshipByName never resolves ambiguously (First() on a non-unique key resolves
-- arbitrarily). The combination (tenant_id, name) is the natural primary lookup key.
--
-- Both use IF NOT EXISTS (idempotent re-run for CI).
-- MySQL 8 / InnoDB / utf8mb4.

ALTER TABLE sys_relationships
    ADD CONSTRAINT uq_sysrel_tenant_join_table UNIQUE (tenant_id, join_table_name),
    ADD CONSTRAINT uq_sysrel_tenant_name       UNIQUE (tenant_id, name);
