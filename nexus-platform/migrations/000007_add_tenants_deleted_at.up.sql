-- GĐ1 bug #1 / #4 fix (spec: docs/_planner/GD1-review-bugs.md §1, §4).
-- DRIFT chí mạng: the GORM model auth.Tenant declares `DeletedAt gorm.DeletedAt`
-- (internal/auth/user.go:19), which makes GORM append `AND tenants.deleted_at IS NULL`
-- to EVERY query on the tenants table — including the login-path ResolveTenantBySlug
-- (internal/auth/repository.go:32-44). But migration 000003 created `tenants` WITHOUT a
-- deleted_at column (only `users` has one), so production login deterministically fails
-- with MySQL 1054 "Unknown column tenants.deleted_at" -> 500 for every tenant.
--
-- Soft-delete on tenant is INTENTIONAL (users already has deleted_at and
-- repository.go:30 documents behaviour that relies on it), so the fix is to ADD the
-- column to match the model — not to drop the struct field. We add a new migration
-- (never edit an already-applied one). The integration harness previously masked this
-- drift with `gdb.AutoMigrate(&auth.Tenant{})`; that reconcile line is removed by the
-- backend/qa tasks now that the column exists in the real schema.
--
-- Index name idx_tenants_deleted mirrors idx_users_deleted (000003:34) for consistency.
-- MySQL 8 / InnoDB / utf8mb4.

ALTER TABLE tenants
    ADD COLUMN deleted_at TIMESTAMP NULL DEFAULT NULL,
    ADD INDEX idx_tenants_deleted (deleted_at);
