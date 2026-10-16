-- Reverse of 000020 sys_reports. The table has no FKs and nothing references it, so a single
-- DROP fully rolls back with no constraint-ordering needed. After this runs no sys_reports
-- table, index, or row remains; the preceding migrations are untouched. Idempotent (IF EXISTS).
DROP TABLE IF EXISTS sys_reports;
