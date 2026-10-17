# 0004. sqlx for dynamic records, GORM for system tables

- **Status:** Accepted
- **Recorded:** 2026-10-17, after the fact. Citations are pinned to `d300dfc`, main after #20 (MCP) and #21 (access).
- **Paths:** `internal/` and `cmd/` are under `nexus-platform/`. Every link is pinned to the commit.

## Context

Business records live in tables born from metadata. There is one physical `obj_<entity>` table per entity name, shared
by every tenant that registers that name ([`internal/services/entity_management.go:102-107`],
[`internal/services/entity_orchestration.go:219-230`]). It has fixed system columns, keeps field values in a `data`
JSON column, and adds generated columns and indexes only for unique, relation, user, polymorphic and indexed fields
([`internal/ddl/create_table.go:56-86`], [`internal/ddl/create_table.go:164-292`]).

So table and column names are runtime values, and SQL cannot bind an identifier as a parameter
([`internal/ddl/ddl.go:1-5`]). The queries also lean on MySQL itself: a version-guarded `JSON_MERGE_PATCH` update
([`internal/dynrepo/record.go:1104-1124`]), row-scope fragments that carry their own arguments
([`internal/dynrepo/rowscope.go:196-310`]), keyset cursors ([`internal/dynrepo/record.go:1482-1489`]), and report
joins that filter the tenant at every hop ([`internal/dynrepo/report_join.go:104-123`]). The fixed system tables, such
as users, roles and the metadata registry, have static shapes.

## Decision

- **Dynamic tables: sqlx and hand-written SQL** ([`internal/dynrepo/dynrepo.go:1-7`]). Values always travel as `?`
  parameters. An identifier enters a statement only after `ddl.SanitizeIdentifier` ([`internal/ddl/ddl.go:92-113`]),
  or `SanitizeComposedIdentifier` for a name the engine built from sanitized parts ([`internal/ddl/ddl.go:45-70`]).
  Every public method in `record.go` resolves its table through `tableFor` ([`internal/dynrepo/record.go:508-515`]), as in
  `InsertRecord` ([`internal/dynrepo/record.go:592-594`]) and `GetRecord` ([`internal/dynrepo/record.go:686-689`]).
- **Static shapes: GORM models**, such as the metadata registry ([`internal/metadata/metadata.go:9-129`],
  [`internal/metadata/repository.go:121-130`]) and the auth tables.
- Both pools are opened on the same DSN ([`cmd/server/main.go:117-139`]).

## Consequences

**Gained.**

- Identifier safety has one place to look, `internal/ddl`: a lowercase allowlist whose 26-character cap also keeps
  every index name the engine derives within MySQL's 64 ([`internal/ddl/ddl.go:14-29`]).
- The tenant and row-scope predicates are visible in each statement
  ([ADR 0005](0005-tenant-isolation-and-row-level-access.md)), and MySQL features are used with no translation layer.

**Costs.**

- Two data-access styles, and two pools sharing one connection budget ([`cmd/server/main.go:132-139`]).
- No transaction spans both handles. Where one is needed, a GORM statement is written again as raw SQL:
  `UpdateFieldMutableExec` repeats `UpdateFieldMutable` for the publish transaction
  ([`internal/metadata/field_exec.go:8-12`]). Creating an entity runs the DDL first and then writes the registry; on
  failure it drops the table only if no tenant claims the name ([`internal/services/entity_orchestration.go:216-242`]).
- The split has drifted. sqlx also serves newer system tables: workflows, reports, apps, the audit log, versions
  ([`internal/workflow/repository.go:21`], [`internal/report/repository.go:21`], [`internal/appmeta/repository.go:24`],
  [`internal/audit/repository.go:21`], [`internal/metaversion/versions.go:82`]). In non-test code under `internal/`, 25
  packages import sqlx and 11 import GORM. Only the `obj_*` half of the rule holds without exception.
- The compiler does not check the discipline: an identifier formatted into SQL without the sanitizer still builds.
  Review and the integration tests against real MySQL are the guard.
- Shared tables couple tenants physically. A unique index would bind every tenant that uses the entity name, so
  uniqueness goes through a pivot table instead ([`internal/ddl/create_table.go:203-209`]). Deleting an
  entity soft-deletes that tenant's rows and never drops the table
  ([`internal/services/entity_management.go:102-107`]).

**Given up.** ORM conveniences on the record path, and a single way of doing data access.

## Alternatives considered

- **EAV**, one row per record and field. No DDL at runtime, but every read pivots or joins once per field, values lose
  their types, and indexing a field needs one value column per type. It lost on read cost. The uniqueness pivot uses
  that shape for one narrow job.
- **JSON only**, one table for every entity. No DDL at all, but generated columns and indexes belong to a table, so
  every indexed field of every entity would become a column of that one table. It lost on indexing.
- **GORM for everything.** GORM can address a table by name, but the record path needs SQL that GORM does not model
  (generated columns, `JSON_MERGE_PATCH`, predicate fragments with their own arguments), so most of it would be raw SQL
  through GORM anyway. It lost on control.
- **sqlx for everything.** One style, and the code is drifting there already. Not done: it means rewriting the auth and
  registry repositories (18 files in `internal/auth` import GORM) for consistency alone.

<!-- Links below are generated; each code link points at the commit named under Recorded. -->

[`internal/services/entity_management.go:102-107`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/entity_management.go#L102-L107
[`internal/services/entity_orchestration.go:219-230`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/entity_orchestration.go#L219-L230
[`internal/ddl/create_table.go:56-86`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/create_table.go#L56-L86
[`internal/ddl/create_table.go:164-292`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/create_table.go#L164-L292
[`internal/ddl/ddl.go:1-5`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/ddl.go#L1-L5
[`internal/dynrepo/record.go:1104-1124`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/record.go#L1104-L1124
[`internal/dynrepo/rowscope.go:196-310`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/rowscope.go#L196-L310
[`internal/dynrepo/record.go:1482-1489`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/record.go#L1482-L1489
[`internal/dynrepo/report_join.go:104-123`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/report_join.go#L104-L123
[`internal/dynrepo/dynrepo.go:1-7`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/dynrepo.go#L1-L7
[`internal/ddl/ddl.go:92-113`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/ddl.go#L92-L113
[`internal/ddl/ddl.go:45-70`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/ddl.go#L45-L70
[`internal/dynrepo/record.go:508-515`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/record.go#L508-L515
[`internal/dynrepo/record.go:592-594`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/record.go#L592-L594
[`internal/dynrepo/record.go:686-689`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/record.go#L686-L689
[`internal/metadata/metadata.go:9-129`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metadata/metadata.go#L9-L129
[`internal/metadata/repository.go:121-130`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metadata/repository.go#L121-L130
[`cmd/server/main.go:117-139`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/cmd/server/main.go#L117-L139
[`internal/ddl/ddl.go:14-29`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/ddl.go#L14-L29
[`cmd/server/main.go:132-139`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/cmd/server/main.go#L132-L139
[`internal/metadata/field_exec.go:8-12`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metadata/field_exec.go#L8-L12
[`internal/services/entity_orchestration.go:216-242`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/entity_orchestration.go#L216-L242
[`internal/workflow/repository.go:21`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/workflow/repository.go#L21
[`internal/report/repository.go:21`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/report/repository.go#L21
[`internal/appmeta/repository.go:24`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/appmeta/repository.go#L24
[`internal/audit/repository.go:21`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/audit/repository.go#L21
[`internal/metaversion/versions.go:82`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metaversion/versions.go#L82
[`internal/ddl/create_table.go:203-209`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/create_table.go#L203-L209
