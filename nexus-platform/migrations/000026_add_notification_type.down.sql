-- Reversal of 000026_add_notification_type.up.sql
-- Drops the `type` column added for FE notification categorisation.
-- The idx_notif_inbox index existed before v26 (added in 000017) so we do NOT drop it here.

ALTER TABLE sys_notifications
    DROP COLUMN IF EXISTS type;
