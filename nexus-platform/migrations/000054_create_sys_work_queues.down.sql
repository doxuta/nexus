-- VIEC-HOM-NAY rollback: queues are pure metadata (no customer records) — standard drop.
DROP TABLE IF EXISTS sys_work_queues;
