-- CALENDAR-1 (engine-depth primitive #1a, docs/business-capability-map.md §3.1):
-- sys_business_calendars — per-tenant business-hours calendars for the SLA/timer engine.
--
-- One row = one calendar: an IANA timezone + per-weekday working intervals (hours_json) +
-- non-working dates (holidays_json). The math over these rows lives in internal/calendar
-- (pure, no DB): AddBusinessDuration / BusinessDurationBetween / IsWorkingTime.
--
-- hours_json shape (times are LOCAL to `timezone`, "HH:MM" 24h, end may be "24:00"):
--   {"mon":[{"start":"08:00","end":"12:00"},{"start":"13:00","end":"17:00"}], ..., "sun":[]}
--   A weekday absent or [] = non-working. holidays_json: ["2026-09-02", ...] or NULL.
--
-- INV: DDL-ONLY — zero seed rows. A tenant with no calendar keeps today's behaviour
-- untouched: the SLA/timer engine (next slice, 1b) falls back to WALL-CLOCK arithmetic.
-- is_default: ≤1 per tenant, enforced by a clear-old UPDATE in the repository transaction
-- (same convention as sys_apps.is_default).
CREATE TABLE IF NOT EXISTS sys_business_calendars (
    id            CHAR(36)     NOT NULL,
    tenant_id     CHAR(36)     NOT NULL,
    name          VARCHAR(100) NOT NULL,            -- display name, unique per tenant (service caps at 80 so the soft-delete tombstone suffix fits)
    timezone      VARCHAR(64)  NOT NULL,            -- IANA tz, e.g. "Asia/Ho_Chi_Minh" (validated via time.LoadLocation)
    hours_json    JSON         NOT NULL,            -- per-weekday working intervals (shape above)
    holidays_json JSON         DEFAULT NULL,        -- ["YYYY-MM-DD", ...] non-working dates, NULL = none
    is_default    TINYINT(1)   NOT NULL DEFAULT 0,  -- tenant's default calendar (≤1/tenant, repo tx)
    created_by    CHAR(36)     DEFAULT NULL,
    updated_by    CHAR(36)     DEFAULT NULL,
    created_at    TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at    TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at    TIMESTAMP(3) DEFAULT NULL,        -- soft delete (tombstone-renames name to free the unique slot)
    PRIMARY KEY (id),
    UNIQUE KEY uq_sys_business_calendars_tenant_name (tenant_id, name),
    KEY idx_sys_business_calendars_lookup (tenant_id, deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
