# 0002. Drafts, a schema cutoff, and a rollback that re-hides

- **Status:** Accepted
- **Recorded:** 2026-10-17, after the fact. Citations are pinned to `d300dfc`, main after #20 (MCP) and #21 (access).
- **Paths:** `internal/` and `cmd/` are under `nexus-platform/`. Every link is pinned to the commit.

## Context

Once a workspace is delivered to a customer, a builder's edit must not reach the customer's users before it is
published, and a bad publish must be reversible. But a table holds rows and a field holds values: creating a table, or
an indexed field, runs DDL, which MySQL commits on its own ([`internal/ddl/create_table.go:181-182`]), and deleting
either destroys data. The customer's requests must not pay for drafts, and metadata timestamps have second precision
([`internal/metadata/metadata.go:149-155`]).

## Decision

**Delivery turns drafts on, for good.** The first publish snapshots every registered document type as version 1
([`internal/services/publish_service.go:366-420`]). `DraftGate.WriteMode` alone decides where a write lands: live before
delivery or during a publish, a draft for the tenant's builder, live for anyone else, refused with no resolved caller
([`internal/services/draft_gate.go:53-84`]). The engine's loader and scheduler read the raw repositories, so a draft
never runs ([`internal/services/draft_repo_workflow.go:24-32`], [`internal/metaversion/structure_test.go:10-54`]).

**One class table** says how a change of each document type reaches users ([`internal/metaversion/class.go:5-68`]):

| Kind | Types | Reaches users |
|---|---|---|
| Draft | workflow, report, dashboard, layout, branding, app, field, entity | on publish |
| DirectTraced | role, field_acl | at once, with a history entry |
| DirectUntraced | relationship, and every type not listed, unknown ones included | at once, with no history |

A `NewHidden` flag marks field, entity and relationship: a new one is created live but hidden until the next publish.
Some operations on Draft types apply at once: creating or deleting an app
([`internal/services/draft_app_store.go:14-22`]) and a field's `indexed` option
([`internal/services/draft_repo_field.go:22-27`]). A table is drafted only as a delete
([`internal/services/publish_adapters_entity.go:15-17`]).

**Publish is one transaction** under a row lock on the tenant. It applies exactly the change set the builder reviewed,
checks for live edits made since each draft started, and checks consistency across documents
([`internal/services/publish_service.go:422-527`], [`internal/services/publish_service.go:588-606`]).

**The schema cutoff.** Each version stores `schema_cutoff_at` ([`internal/metaversion/versions.go:35`]). Outside the
builder's draft view, the schema readers drop tables, fields and relationships created after the head's cutoff
([`internal/services/schema_visibility.go:9-51`], [`internal/handlers/metadata_handler.go:449`],
[`internal/services/layout_service.go:105-114`]). Such a table is absent on the record path too: `RecordService.resolve`
answers `ENTITY_NOT_FOUND` to anyone but the builder, and the CSV import looks tables up through it
([`internal/services/record_orchestration.go:501-505`], [`internal/services/record_orchestration.go:517-532`],
[`internal/services/record_import.go:153-167`]). Hidden rows count as pending changes; a publish releases them by moving
the cutoff to the newest definition it read ([`internal/services/publish_hidden_new.go:15-22`]).

**Deletes are deferred and DML-only.** Deleting a field or table is a draft. Publish records the definition and a sweep
instant in the version and runs the delete after commit, never `DROP` or `ALTER`
([`internal/services/publish_schema_ops.go:3-13`]). A table delete also drops every role's grants on its name in the
same transaction, so a new table of that name inherits none. A published delete records them first as `roleGrants`; on
a workspace never delivered the drop is final ([`internal/metadata/repository.go:701-710`],
[`internal/services/publish_schema_ops.go:142-149`]).

**Rollback** is a new version written through the publish path ([`internal/services/publish_rollback.go:22-24`]).
Configuration returns to the target's state, with the exceptions below; a rule created later is soft-deleted
([`internal/services/publish_adapters_workflow.go:142-147`]). The target's cutoff returns, and fields and tables that a
later publish deleted come back: a table with exactly the rows its delete trashed and with its role grants, for roles
that still exist, raised where a role holds the grant again and never lowered
([`internal/services/publish_schema_restore.go:3-15`], [`internal/services/publish_schema_restore.go:106-133`]).
Rollback is refused while anything is pending, or when the head or the plan moved since the preview
([`internal/services/publish_rollback.go:666-693`]).

## What rollback does not do

- It drops no table, field, relation or record. Schema born after the target is hidden again and listed as a
  "rehide"; rows written into it stay ([`internal/services/publish_rollback.go:247-252`],
  [`internal/services/publish_rollback.go:592-600`]). Records are not versioned at all ([`cmd/server/main.go:423`]).
- It does not revert attribute edits of a field or table ([`internal/services/publish_rollback.go:242-246`]).
- It does not undo applied-at-once changes; Revert does, one entry at a time
  ([`internal/services/publish_revert.go:20-24`]). A rule whose history holds only a redacted webhook key is left as it
  is ([`internal/services/publish_rollback.go:457-462`]).

## The second-precision edge

`created_at` and the cutoff are floored to the second ([`internal/metadata/metadata.go:149-155`],
[`internal/services/draft_gate_cutoff.go:31-55`]), and the cutoff's own second counts as released
([`internal/services/schema_visibility.go:18-27`]): a definition created later in that second would count as
published. A publish that releases something waits the second out and re-reads, refusing `DRAFTS_CHANGED` if the set
moved ([`internal/services/publish_hidden_new.go:196-237`]). Open: an insert still uncommitted at the re-read slips
through, and the wait holds the tenant lock ([#13]); the baseline takes the current second with no wait
([`internal/services/publish_service.go:400`]), and a rollback to it reuses that second
([`internal/services/publish_rollback.go:697-703`]). [#19] records a rollback test that passes only when its new table
is stamped in the cutoff's second, that is, when a publish that never saw the table counts it as released.

## Consequences

**Gained.** No draft-class change reaches customers before it is published. A published field or table delete can be
undone with its data and grants, and new schema withdrawn without losing rows. Publish and rollback share one write
path, and the history only grows.

**Costs.**

- About 8,500 lines of non-test Go in 31 files (`internal/metaversion`, `services/publish_*`, `services/draft_*`).
- Hiding is not complete. N-N `Link` and `ListLinked` find their target table without `resolve`, and so does a
  relation's `?expand` label, so a crafted call with an approved grant still reaches a hidden table
  ([`internal/services/relation_service.go:264-295`], [`internal/services/relation_service.go:481-505`],
  [`internal/services/record_expand_label.go:69-74`], [#23]). A field born after the publish is hidden by the schema
  readers only, since the record path checks the table, not its fields
  ([`internal/services/record_orchestration.go:517-532`]); so a new required field is created optional and its
  "required" flag rides a draft ([`internal/services/field_orchestration.go:357-367`]).
- A direct history entry is written after the change commits; if that write fails, the change stays and the response
  carries a warning ([`internal/services/version_recorder.go:73-84`]).
- Another replica can serve the old cutoff for 5 s and the old documents for up to 60 s
  ([`internal/services/draft_gate_cutoff.go:22-24`], [`internal/services/publish_service.go:23-26`]).
- The raise-only restore uses `VALUES()` in `ON DUPLICATE KEY UPDATE`, deprecated since MySQL 8.0.20
  ([`internal/services/publish_schema_restore.go:110-115`], [#23]).

**Given up.** Hard deletes: a deleted table's rows stay, soft-deleted, in the shared table
([`internal/services/entity_management.go:102-107`]).

## Alternatives considered

- **Draft new tables and fields too.** It needs a shadow schema, or DDL inside the publish transaction, which MySQL
  commits implicitly. It lost to "create live, hide by timestamp".
- **Immutable snapshots behind a version pointer.** Trivial rollback, but every engine read would resolve a version and
  rows cannot be snapshotted, while the customer's read path pays no query for drafts today
  ([`internal/services/draft_gate.go:86-96`]). It lost.
- **Rollback deletes what a later publish created.** It destroys rows customers wrote. It lost to re-hiding.
- **Sub-second timestamps.** They would shrink the edge, at the price of a migration and every comparison. Open until
  [#19] names the root cause.

<!-- Links below are generated; each code link points at the commit named under Recorded. -->

[`internal/ddl/create_table.go:181-182`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/create_table.go#L181-L182
[`internal/metadata/metadata.go:149-155`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metadata/metadata.go#L149-L155
[`internal/services/publish_service.go:366-420`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_service.go#L366-L420
[`internal/services/draft_gate.go:53-84`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/draft_gate.go#L53-L84
[`internal/services/draft_repo_workflow.go:24-32`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/draft_repo_workflow.go#L24-L32
[`internal/metaversion/structure_test.go:10-54`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metaversion/structure_test.go#L10-L54
[`internal/metaversion/class.go:5-68`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metaversion/class.go#L5-L68
[`internal/services/draft_app_store.go:14-22`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/draft_app_store.go#L14-L22
[`internal/services/draft_repo_field.go:22-27`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/draft_repo_field.go#L22-L27
[`internal/services/publish_adapters_entity.go:15-17`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_adapters_entity.go#L15-L17
[`internal/services/publish_service.go:422-527`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_service.go#L422-L527
[`internal/services/publish_service.go:588-606`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_service.go#L588-L606
[`internal/metaversion/versions.go:35`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metaversion/versions.go#L35
[`internal/services/schema_visibility.go:9-51`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/schema_visibility.go#L9-L51
[`internal/handlers/metadata_handler.go:449`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/handlers/metadata_handler.go#L449
[`internal/services/layout_service.go:105-114`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/layout_service.go#L105-L114
[`internal/services/record_orchestration.go:501-505`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/record_orchestration.go#L501-L505
[`internal/services/record_orchestration.go:517-532`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/record_orchestration.go#L517-L532
[`internal/services/record_import.go:153-167`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/record_import.go#L153-L167
[`internal/services/publish_hidden_new.go:15-22`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_hidden_new.go#L15-L22
[`internal/services/publish_schema_ops.go:3-13`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_schema_ops.go#L3-L13
[`internal/metadata/repository.go:701-710`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metadata/repository.go#L701-L710
[`internal/services/publish_schema_ops.go:142-149`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_schema_ops.go#L142-L149
[`internal/services/publish_rollback.go:22-24`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_rollback.go#L22-L24
[`internal/services/publish_adapters_workflow.go:142-147`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_adapters_workflow.go#L142-L147
[`internal/services/publish_schema_restore.go:3-15`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_schema_restore.go#L3-L15
[`internal/services/publish_schema_restore.go:106-133`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_schema_restore.go#L106-L133
[`internal/services/publish_rollback.go:666-693`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_rollback.go#L666-L693
[`internal/services/publish_rollback.go:247-252`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_rollback.go#L247-L252
[`internal/services/publish_rollback.go:592-600`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_rollback.go#L592-L600
[`cmd/server/main.go:423`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/cmd/server/main.go#L423
[`internal/services/publish_rollback.go:242-246`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_rollback.go#L242-L246
[`internal/services/publish_revert.go:20-24`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_revert.go#L20-L24
[`internal/services/publish_rollback.go:457-462`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_rollback.go#L457-L462
[`internal/services/draft_gate_cutoff.go:31-55`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/draft_gate_cutoff.go#L31-L55
[`internal/services/schema_visibility.go:18-27`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/schema_visibility.go#L18-L27
[`internal/services/publish_hidden_new.go:196-237`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_hidden_new.go#L196-L237
[`internal/services/publish_service.go:400`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_service.go#L400
[`internal/services/publish_rollback.go:697-703`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_rollback.go#L697-L703
[`internal/services/relation_service.go:264-295`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/relation_service.go#L264-L295
[`internal/services/relation_service.go:481-505`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/relation_service.go#L481-L505
[`internal/services/record_expand_label.go:69-74`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/record_expand_label.go#L69-L74
[`internal/services/field_orchestration.go:357-367`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/field_orchestration.go#L357-L367
[`internal/services/version_recorder.go:73-84`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/version_recorder.go#L73-L84
[`internal/services/draft_gate_cutoff.go:22-24`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/draft_gate_cutoff.go#L22-L24
[`internal/services/publish_service.go:23-26`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_service.go#L23-L26
[`internal/services/publish_schema_restore.go:110-115`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_schema_restore.go#L110-L115
[`internal/services/entity_management.go:102-107`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/entity_management.go#L102-L107
[`internal/services/draft_gate.go:86-96`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/draft_gate.go#L86-L96
[#13]: https://github.com/doxuta/nexus/issues/13
[#19]: https://github.com/doxuta/nexus/issues/19
[#23]: https://github.com/doxuta/nexus/issues/23
