-- TIMER-1 (engine-depth primitive #1b — docs/business-capability-map.md §3.1 "parked row +
-- wake-up", docs/legacy-upgrade-notes.md #1): sys_workflows.trigger_event gains 'date_offset',
-- the date-anchored timer trigger ("X phút/giờ/ngày/giờ-làm-việc TRƯỚC/SAU field ngày F").
--
-- This is the ONLY schema change the timer needs: the timer config itself
-- ({dateField, offset:{amount,unit}, direction, calendarId?}) lives INSIDE the existing
-- `condition` JSON column (key "timer" next to "match"/"rules") — no new column, no new table.
-- The parked wake-up rows reuse sys_jobs (JOBS-1, mig 000046, job_type 'workflow_timer') and the
-- business-hours math reuses sys_business_calendars (CALENDAR-1, mig 000047).
--
-- ENUM discipline (same as 000019): the new value is APPENDED at the END so the integer ordinals
-- of the existing values (after_create=1 … scheduled=4) are unchanged — appending is the only
-- safe ENUM mutation. Re-applying the same MODIFY is a no-op, so this is naturally idempotent.

ALTER TABLE sys_workflows
    MODIFY COLUMN trigger_event
        ENUM('after_create','after_update','after_delete','scheduled','date_offset') NOT NULL;
