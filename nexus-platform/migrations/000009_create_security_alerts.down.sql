-- Reverse of 000009 A7 security_alerts audit table. The table has no FKs and nothing
-- references it, so a single DROP fully rolls back with no constraint ordering needed.
-- After this runs no security_alerts table, index, or row remains; migrations
-- 000001-000008 are untouched.
DROP TABLE IF EXISTS security_alerts;
