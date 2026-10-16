-- Reverse of 000068: drop the per-blob digest. The blobs themselves are untouched; the restore
-- drill falls back to size-only for a schema without the column.
ALTER TABLE sys_files DROP COLUMN sha256;
