-- Reverse 000050: drop the pivot table. Safe in Slice A — the OLD obj_* unique indexes still
-- enforce uniqueness on their own, so removing the pivot never leaves uniqueness unprotected.
DROP TABLE IF EXISTS sys_unique_index;
