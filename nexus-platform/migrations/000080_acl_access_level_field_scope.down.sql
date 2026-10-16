-- Reverts 000080. A "field:<name>" grant cannot exist in the old VARCHAR(20) world; fall back to the
-- NARROWER, safe level ('owner') rather than truncating the value into garbage (which would fail
-- closed to "none" and silently lock the role out).
UPDATE acl_role_actions SET access_level = 'owner' WHERE access_level LIKE 'field:%';
ALTER TABLE acl_role_actions
    MODIFY COLUMN access_level VARCHAR(20) NOT NULL DEFAULT 'all';
