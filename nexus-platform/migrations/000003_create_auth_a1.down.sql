-- Reverse of 000003 A1 auth schema. Drop in FK-dependency order: child tables first
-- (refresh_tokens references users + tenants; users references tenants), then parents.
-- Dropping the tables removes the seeded tenant/admin rows too — no orphaned objects remain.

DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;
