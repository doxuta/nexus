-- Reverse of 000023 sys_files. No FKs reference it, so a single DROP fully rolls back with no
-- constraint ordering. After this runs no sys_files table/index/row remains; migrations
-- 000001-000022 are untouched. The on-disk blobs under FILE_STORAGE_DIR are NOT removed by this
-- migration (they are filesystem artifacts outside the DB transaction) — clean them out of band.
-- Idempotent (IF EXISTS).
DROP TABLE IF EXISTS sys_files;
