-- Reverse of 000007: drop the soft-delete column added to tenants and its index.
-- Drop the index before the column it covers (symmetric with 000004's down). After this
-- runs the tenants table is back to its 000003 shape (no deleted_at), which re-introduces
-- the model/schema drift — so this rollback is only safe paired with reverting the
-- auth.Tenant soft-delete usage. No data loss for live tenants (rows where deleted_at IS
-- NULL are unaffected; any soft-deleted tenants become visible again, matching pre-000007
-- behaviour).

ALTER TABLE tenants
    DROP INDEX idx_tenants_deleted,
    DROP COLUMN deleted_at;
