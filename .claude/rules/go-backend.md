---
description: Go backend conventions for nexus-platform
paths:
  - "nexus-platform/**/*.go"
  - "nexus-platform/migrations/**"
---

# Go backend conventions

## Layers
- `cmd/` wires dependencies only. Handlers parse and validate input, call a service, shape the response.
  Services hold business rules and check access before acting. Repositories run SQL and return domain types.
- Interfaces live next to their consumer; one implementation is fine, no factories for one product.

## Data
- System tables (`sys_*`, `users`, `roles`, `teams`…) use GORM; dynamic entity tables (`obj_*`) use sqlx.
- The standard columns and indexes of every `obj_*` table (UUID `id`, `tenant_id`, owner, team set, `version`,
  soft delete, timestamps) are defined once in `internal/ddl/create_table.go`; read it there rather than here.
  Charset `utf8mb4_unicode_ci`.
- Dynamic field values go in the `data` JSON column; anything joined, sorted or grouped often gets a real column
  or a generated column with an index.
- Timestamps have second precision. The metadata tables (`sys_entities`, `sys_fields`, `sys_relationships`)
  stamp `created_at` per row in the app (`internal/metadata/metadata.go`, `secondNow`), so rows written together
  can fall on two sides of a second: code that compares them with a cutoff must think in whole seconds.

## SQL
- Values are always parameters. Identifiers only through `internal/ddl.SanitizeIdentifier` / the metadata registry.
- The DSN sets `clientFoundRows`: `RowsAffected` counts MATCHED rows. A compare-and-set ("did THIS call win")
  must exclude the state its SET writes (`AND used_at IS NULL`, `AND status = 'running'`).

## Errors and logs
- `fmt.Errorf("pkg.Type.method: %w", err)` at every layer; map to HTTP status only in handlers.
- `zap` with fields (`tenant_id`, `user_id`, `entity`), never `fmt.Println`.

## API
- Base `/api/v1`; JSON is camelCase; timestamps ISO 8601 UTC.
- Success: `{"data": …}` (+ `"pagination": {page, limit, total, totalPages}` for lists).
  Error: `{"error": "<human message>", "code": "UPPER_SNAKE", "details": {}}`.
- Status codes: 200/201/204, 400 malformed, 401 unauthenticated, 403 forbidden (same body whether or not the route
  exists for that caller), 404, 409 conflict/state, 413, 422 validation, 429, 500.
- A new route must have a caller (web client or a documented consumer): `scripts/check-orphan-endpoints.py --strict`.

## Migrations
- golang-migrate, `NNNNNN_name.up.sql` + `.down.sql`, numbered after the highest existing one, idempotent where
  MySQL allows it. Never edit a migration that has been applied; add a new one.

## Tests
- Unit tests next to the code with fakes; table-driven where cases repeat.
- Integration tests: `//go:build integration`, real MySQL through `internal/ittest` (throwaway database per run);
  run with `make test-integration` (`scripts/run-integration.sh`, which fails an all-SKIP run).
