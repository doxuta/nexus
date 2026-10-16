-- Queue #322. sys_field_acl rows are keyed by entity NAME + field NAME, and until now neither
-- SoftDeleteField nor SoftDeleteEntity removed them. A row for an entity/field that no longer
-- exists restricts nothing today, but an entity re-created or re-installed under the same name
-- adopted it silently (skipping the PutGrants / EditField dead-end guards), and the matrix listed
-- it as a phantom cell. The delete paths now drop the rows in their own transaction (metadata
-- repository); this one-off removes the rows already orphaned.
--
-- Tenant-pinned joins (a name collision across tenants never matches). Live rows are untouched:
-- a row is removed only when NO live entity of that tenant carries its entity_name, or the live
-- entity has NO live field of its field_name. Two statements, entity-level first.
DELETE a FROM sys_field_acl a
LEFT JOIN sys_entities e
       ON e.tenant_id = a.tenant_id AND e.name = a.entity_name AND e.deleted_at IS NULL
WHERE e.id IS NULL;

DELETE a FROM sys_field_acl a
JOIN sys_entities e
       ON e.tenant_id = a.tenant_id AND e.name = a.entity_name AND e.deleted_at IS NULL
LEFT JOIN sys_fields f
       ON f.entity_id = e.id AND f.tenant_id = a.tenant_id AND f.name = a.field_name AND f.deleted_at IS NULL
WHERE f.id IS NULL;
