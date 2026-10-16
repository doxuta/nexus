-- Down for 000021_create_bpm_approval: drop the BPM approval tables, reverting head to v20.
-- Child table first is unnecessary (no physical FK between them by design) but instances is
-- dropped before processes for readability (instance references a process logically).
DROP TABLE IF EXISTS sys_approval_instances;
DROP TABLE IF EXISTS sys_approval_processes;
