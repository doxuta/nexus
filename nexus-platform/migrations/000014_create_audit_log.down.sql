-- Reverse of 000014 RM-19 audit_log table. The table has no FKs and nothing references
-- it, so a single DROP fully rolls back with no constraint ordering needed. After this
-- runs no audit_log table, index, or row remains; migrations 000001-000013 are untouched.
DROP TABLE IF EXISTS audit_log;
