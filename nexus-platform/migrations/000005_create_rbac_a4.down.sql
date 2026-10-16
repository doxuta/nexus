-- Reverse of 000005 A4 RBAC schema. Undo in FK-dependency order:
--   1. drop the users.role_id FK + its index (users references acl_roles),
--   2. drop acl_role_actions (references acl_roles),
--   3. drop acl_roles.
-- This restores users.role_id to the plain nullable forward-reference column it was
-- after 000003 (the column itself stays; it was created in 000003, not here).
-- No orphaned constraints/indexes/objects remain after this runs.

ALTER TABLE users
    DROP FOREIGN KEY fk_users_role,
    DROP INDEX idx_users_role;

DROP TABLE IF EXISTS acl_role_actions;
DROP TABLE IF EXISTS acl_roles;
