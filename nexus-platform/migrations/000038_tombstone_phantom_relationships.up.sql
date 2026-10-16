-- REL-RM21-NN — data cleanup: tombstone + soft-delete PHANTOM sys_relationships rows.
--
-- Before this slice, metadata.Repository.SoftDeleteEntity swept ONLY the 1-N/1-1 registry
-- rows where the deleted entity was the CHILD. Rows where it was the PARENT and ALL N-N rows
-- (either side) were left ACTIVE — so every past entity delete left "phantom" rows whose
-- parent or child entity no longer exists. Those phantoms keep surfacing through
-- ListAllRelationships / ListRelationshipsForEntity → GET /metadata/relationships → ghost
-- edges in the Studio ERD + Related tab, AND they squat their UNIQUE slots
-- (uq_sysrel_tenant_name / uq_sysrel_tenant_join_table, migration 000025 — both span
-- soft-deleted rows), bricking a recreate of the same relationship with 409.
--
-- The Go code path is fixed go-forward (SoftDeleteEntity now sweeps EITHER side, ANY type);
-- this migration repairs the rows already damaged by past deletes.
--
-- A row is a phantom when its parent_entity_name OR child_entity_name has NO ACTIVE
-- sys_entities row under the SAME tenant (entity tombstones rename sys_entities.name, so a
-- deleted entity can never match by name). Tombstone format mirrors the Go helpers
-- (tombstoneName/tombstoneSuffix): "<name>__deleted_<first-8-of-id>". Budgets: name is
-- VARCHAR(150) → keep LEFT(name, 132) (150 − 18 for "__deleted_" + 8 id chars);
-- join_table_name is VARCHAR(100) → keep LEFT(join_table_name, 82) (100 − 18). VARCHAR
-- lengths and LEFT() both count CHARACTERS in utf8mb4, so multibyte names cannot overflow.
-- Distinct row ids give distinct suffixes, so the renames cannot collide with each other or
-- with previously tombstoned rows (same guarantee the Go path relies on).
--
-- IDEMPOTENT: the WHERE clause only matches ACTIVE rows (deleted_at IS NULL); every row this
-- statement touches gets deleted_at set, so a re-run matches 0 rows. Physical rel_* join
-- tables are deliberately NOT touched (shared across tenants; grace-window reuse — see
-- metadata.Repository.SoftDeleteRelationshipByID). MySQL 8 / InnoDB / utf8mb4.

UPDATE sys_relationships r
SET r.name = CONCAT(LEFT(r.name, 132), '__deleted_', LEFT(r.id, 8)),
    r.join_table_name = CASE WHEN r.join_table_name IS NULL OR r.join_table_name = '' THEN r.join_table_name
                             ELSE CONCAT(LEFT(r.join_table_name, 82), '__deleted_', LEFT(r.id, 8)) END,
    r.deleted_at = CURRENT_TIMESTAMP
WHERE r.deleted_at IS NULL
  AND ( NOT EXISTS (SELECT 1 FROM sys_entities e
                    WHERE e.tenant_id = r.tenant_id AND e.name = r.parent_entity_name AND e.deleted_at IS NULL)
     OR NOT EXISTS (SELECT 1 FROM sys_entities e
                    WHERE e.tenant_id = r.tenant_id AND e.name = r.child_entity_name AND e.deleted_at IS NULL) );
