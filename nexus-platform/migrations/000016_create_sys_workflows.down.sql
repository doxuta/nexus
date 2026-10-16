-- Reverse of 000016 sys_workflows. The table has no FKs and nothing references it, so a single
-- DROP fully rolls back with no constraint-ordering needed. After this runs no sys_workflows
-- table, index, or row remains; migrations 000001-000015 are untouched. Idempotent (IF EXISTS).
DROP TABLE IF EXISTS sys_workflows;
