-- Reverse 000069: drop the Idempotency-Key claims. Only open replay windows (24 h) are lost; no
-- record data lives here. A binary that still expects the table fails (500) on a POST /:entity that
-- carries Idempotency-Key — roll the binary back together with the schema.
DROP TABLE IF EXISTS sys_idempotency_keys;
