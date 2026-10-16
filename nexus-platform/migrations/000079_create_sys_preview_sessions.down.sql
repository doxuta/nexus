-- Reverts 000079: the preview-session trail goes with its table (no other table references it).
DROP TABLE IF EXISTS sys_preview_sessions;
