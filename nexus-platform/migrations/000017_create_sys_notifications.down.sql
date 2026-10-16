-- Reverse of 000017 sys_notifications. No FKs reference it, so a single DROP fully rolls back
-- with no constraint ordering. After this runs no sys_notifications table/index/row remains;
-- migrations 000001-000016 are untouched. Idempotent (IF EXISTS).
DROP TABLE IF EXISTS sys_notifications;
