-- Reverts 000080: the workspace-session trail goes with its table (no other table references it).
DROP TABLE IF EXISTS sys_workspace_sessions;
