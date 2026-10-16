-- Reverse of 000060 sys_record_notes. No FKs reference it, so a single DROP fully rolls back with no
-- constraint ordering. Idempotent (IF EXISTS).
DROP TABLE IF EXISTS sys_record_notes;
