# 0005. Tenant isolation and row-level access

- **Status:** Accepted
- **Recorded:** 2026-10-17, after the fact. Citations are pinned to `d300dfc`, main after #20 (MCP) and #21 (access).
- **Paths:** `internal/` and `scripts/` are under `nexus-platform/`. Every link is pinned to the commit.

## Context

All tenants share one database and, for each entity name, one physical table
([ADR 0004](0004-sqlx-for-dynamic-records-gorm-for-system-tables.md)). Inside that table, `tenant_id` is all that
separates one customer's rows from another's. Inside a tenant, roles grant actions per entity with a row scope. The
people who build and hand over an app never meet those rules: admins bypass them, and both the builder and the
customer's own admin are admins ([`internal/metaversion/context.go:55-59`], [`scripts/acceptance-acl.py:1-7`]). An
assistant that answers from data must not see more than the person asking.

## Decision

1. **The tenant comes from the verified token, and every statement filters it.** A request without a tenant claim is
   refused ([`internal/middleware/auth.go:331-349`]). The rule is that each query on business data writes
   `tenant_id = ?` itself ([`AGENTS.md:29`]): records ([`internal/dynrepo/record.go:686-689`],
   [`internal/dynrepo/record.go:1115-1123`]), the registry ([`internal/metadata/repository.go:121-130`]), every join
   hop ([`internal/dynrepo/report_join.go:104-123`]). The list indexes of `obj_*` lead with `tenant_id` because every
   read filters it ([`internal/ddl/create_table.go:141-152`]). A relation value must name a record of the caller's
   tenant ([`internal/dynrepo/record.go:1462-1480`]); the column is a generated column with an index, not a foreign key
   ([`internal/ddl/create_table.go:214-243`]).
2. **Entity access is checked before the handler.** `RequireACL` maps the HTTP method to view, create, edit or delete
   and asks `ACLService.Authorize` ([`internal/middleware/acl.go:11-20`], [`internal/middleware/acl.go:47-63`]). The
   decision denies by default, is pinned to the tenant, and prefers an exact grant to a wildcard
   ([`internal/services/acl_service.go:25-88`]). Every refusal is the same 403
   ([`internal/middleware/acl.go:189-197`]).
3. **Row scope is applied in the repository.** The grant's level becomes a parameterized predicate after the tenant
   predicate: all, owner, team with the team tree, a user field, and manual shares on reads only. An unknown level
   matches no row ([`internal/dynrepo/rowscope.go:10-40`], [`internal/dynrepo/rowscope.go:234-310`]). A grant of level
   "none" passes the ACL stage, and the row filter alone enforces it ([`internal/dynrepo/rowscope.go:31-36`]). A row
   outside the scope reads as not found ([`internal/dynrepo/record.go:666-670`]).
4. **Admins bypass ACL, not the tenant.** `is_admin` skips the ACL stage and reads every row of its own tenant
   ([`internal/middleware/acl.go:105-112`], [`internal/dynrepo/rowscope.go:182-194`]). So "done" means the non-admin
   rep can use it ([`AGENTS.md:33-34`]). `scripts/acceptance-acl.py` refuses to run when its staff account is an admin, and fails an expected
   entity the rep cannot read, a 200 with zero rows included when the admin sees rows
   ([`scripts/acceptance-acl.py:468-472`], [`scripts/acceptance-acl.py:134-152`]). In CI, the demo loop also checks
   writes as the rep, in Chrome ([`nexus-web/scripts/demo-loop.mjs:442-461`], [`.github/workflows/ci.yml:125-151`]).
5. **The assistant reads as the user.** ⌘K tools go through `RecordReadAdapter`, which runs `Authorize` for each tool
   call and entity, then the record service with the caller's scope unchanged. A forbidden entity reads as a missing
   one, and fields hidden by field ACL are not described ([`internal/ai/assistant_reader.go:12-20`],
   [`internal/ai/assistant_reader.go:81-109`], [`internal/ai/assistant_reader.go:214-259`]). No tool schema has a
   tenant parameter ([`internal/ai/assistant_tools_test.go:270`]).

## Consequences

**Gained.** One schema serves every tenant, and one migration reaches all of them. Access rules are data, enforced by
one engine for the UI, the API, reports and the assistant. The entity and row layers deny by default.

**Costs and limits.**

- **Isolation is a convention plus tests, not a database guarantee.** The database does not enforce it: the
  migrations create no views, policies or per-tenant users, and both pools connect with one DSN
  ([`cmd/server/main.go:117-139`]). A query that forgets `tenant_id` compiles and runs. The guards are review,
  integration tests built around a second tenant such as `TestReportJoin_CrossTenantIsolation`
  ([`internal/dynrepo/report_join_integration_test.go:64-69`]), and the tests that keep tenants out of tool schemas.
- One database credential reads every tenant, so an injection or a missing predicate can cross tenants.
- Field access allows by default: a field with no restriction row is visible
  ([`internal/services/publish_schema_restore.go:85-87`]).
- The acceptance gate checks reads only ([`scripts/acceptance-acl.py:493-503`]). Scoped writes are checked by
  integration tests ([`internal/dynrepo/rls_a5_integration_test.go:191-205`]) and the demo loop, not by the gate.
- A record the rep can read but not edit answers PUT with 404, not 403 ([`internal/dynrepo/record.go:1169-1204`],
  [#18]).
- The platform operator has deliberate cross-tenant paths, such as installing a pack into another tenant
  ([`internal/handlers/admin_vertical_handler.go:209-218`]).
- The ACL bounds what the assistant reads, not what that data says. The threat model for prompt injection through
  record data is still to be written ([#11]).

**Stronger guarantees would need** one of: a database or schema per tenant, each with its own database user;
enforcement below the application, which MySQL lacks natively and would take views keyed on a per-connection tenant
variable, or a SQL proxy; or a static check that fails the build when an `obj_*` statement lacks `tenant_id`, plus a
two-tenant test for every repository method.

## Alternatives considered

- **A database per tenant.** The strongest isolation. It lost on cost: one migration run and one pool per tenant, and
  the operator's cross-tenant features become cross-database.
- **A table per tenant and entity.** Isolation by name, but the identifier budget is already spent
  ([`internal/ddl/ddl.go:14-29`]) and the table count grows with tenants times entities. It lost.
- **An ORM scope that adds the tenant.** It would cover GORM only; the record path is hand-written sqlx
  ([ADR 0004](0004-sqlx-for-dynamic-records-gorm-for-system-tables.md)). It lost.
- **Access checks in the web client.** It lost: the API is the boundary.

<!-- Links below are generated; each one points at the commit named under Status. -->

[`internal/metaversion/context.go:55-59`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metaversion/context.go#L55-L59
[`scripts/acceptance-acl.py:1-7`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/scripts/acceptance-acl.py#L1-L7
[`internal/middleware/auth.go:331-349`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/auth.go#L331-L349
[`AGENTS.md:29`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/AGENTS.md#L29
[`internal/dynrepo/record.go:686-689`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/record.go#L686-L689
[`internal/dynrepo/record.go:1115-1123`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/record.go#L1115-L1123
[`internal/metadata/repository.go:121-130`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metadata/repository.go#L121-L130
[`internal/dynrepo/report_join.go:104-123`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/report_join.go#L104-L123
[`internal/ddl/create_table.go:141-152`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/create_table.go#L141-L152
[`internal/dynrepo/record.go:1462-1480`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/record.go#L1462-L1480
[`internal/ddl/create_table.go:214-243`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/create_table.go#L214-L243
[`internal/middleware/acl.go:11-20`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/acl.go#L11-L20
[`internal/middleware/acl.go:47-63`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/acl.go#L47-L63
[`internal/services/acl_service.go:25-88`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/acl_service.go#L25-L88
[`internal/middleware/acl.go:189-197`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/acl.go#L189-L197
[`internal/dynrepo/rowscope.go:10-40`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/rowscope.go#L10-L40
[`internal/dynrepo/rowscope.go:234-310`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/rowscope.go#L234-L310
[`internal/dynrepo/rowscope.go:31-36`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/rowscope.go#L31-L36
[`internal/dynrepo/record.go:666-670`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/record.go#L666-L670
[`internal/middleware/acl.go:105-112`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/acl.go#L105-L112
[`internal/dynrepo/rowscope.go:182-194`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/rowscope.go#L182-L194
[`AGENTS.md:33-34`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/AGENTS.md#L33-L34
[`scripts/acceptance-acl.py:468-472`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/scripts/acceptance-acl.py#L468-L472
[`scripts/acceptance-acl.py:134-152`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/scripts/acceptance-acl.py#L134-L152
[`nexus-web/scripts/demo-loop.mjs:442-461`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/scripts/demo-loop.mjs#L442-L461
[`.github/workflows/ci.yml:125-151`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/.github/workflows/ci.yml#L125-L151
[`internal/ai/assistant_reader.go:12-20`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_reader.go#L12-L20
[`internal/ai/assistant_reader.go:81-109`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_reader.go#L81-L109
[`internal/ai/assistant_reader.go:214-259`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_reader.go#L214-L259
[`internal/ai/assistant_tools_test.go:270`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_tools_test.go#L270
[`cmd/server/main.go:117-139`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/cmd/server/main.go#L117-L139
[`internal/dynrepo/report_join_integration_test.go:64-69`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/report_join_integration_test.go#L64-L69
[`internal/services/publish_schema_restore.go:85-87`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_schema_restore.go#L85-L87
[`scripts/acceptance-acl.py:493-503`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/scripts/acceptance-acl.py#L493-L503
[`internal/dynrepo/rls_a5_integration_test.go:191-205`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/rls_a5_integration_test.go#L191-L205
[`internal/dynrepo/record.go:1169-1204`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/record.go#L1169-L1204
[`internal/handlers/admin_vertical_handler.go:209-218`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/handlers/admin_vertical_handler.go#L209-L218
[`internal/ddl/ddl.go:14-29`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/ddl.go#L14-L29
[#11]: https://github.com/doxuta/nexus/issues/11
[#18]: https://github.com/doxuta/nexus/issues/18
