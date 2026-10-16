-- Reverse of 000022 RM-42 sys_field_acl table. The only inbound dependency is its own FK to
-- acl_roles (fk_sfa_role); nothing references sys_field_acl, so a single DROP fully rolls back
-- with no constraint-ordering needed. After this runs no sys_field_acl table/index/row remains;
-- field-level security reverts to default-allow (every field visible+editable per the A4 module
-- ACL). Migrations 000001-000021 are untouched.
DROP TABLE IF EXISTS sys_field_acl;
