# 0001. Metadata is the agent's action space

- **Status:** Accepted
- **Recorded:** 2026-10-17, after the fact. Citations are pinned to `d300dfc`, main after #20 (MCP) and #21 (access).
- **Paths:** `internal/`, `cmd/` and `scripts/` are under `nexus-platform/`. Every link is pinned to the commit.

## Context

An AI builder that writes application code hands each customer a new codebase. Someone has to review every generated
line, and every bug fix has to be made once per app. Nexus runs every customer app on one engine instead: an app is
rows in system tables that the engine reads at runtime ([`internal/metadata/metadata.go:9-10`]), and the
configuration document types are versioned per tenant ([`cmd/server/main.go:423`]).

The forces:

- A builder must be able to judge a change before users see it.
- A wrong model output needs a bounded blast radius.
- The model must never choose the tenant.
- The code that runs customer apps must stay small enough for humans to own.

## Decision

The builder agent writes no code, SQL or DDL. Its action space is a fixed list of typed tools over the metadata model,
returned by `ToolDefinitions` ([`internal/ai/tools.go:104-115`]). At `d300dfc` there are 26: 11 read tools and 15
`propose_*` tools that validate and buffer a change ([`internal/ai/tools.go:766-824`]). A test pins the count and fails
if any tool schema mentions a tenant ([`internal/ai/tools_test.go:188-212`]); the executor takes the tenant from the
verified request ([`internal/ai/agent.go:190`]).

A proposal goes through the validator the manual builder uses: `propose_entity` calls
`services.ValidateCreateEntity` ([`internal/ai/tools.go:1030-1079`]), `propose_field` calls `services.ValidateAddField`
([`internal/ai/tools.go:1081-1109`]). A refusal goes back to the model as a coded error it can correct
([`internal/ai/tools.go:757-764`]). A valid proposal is appended to an in-memory list, and nothing is applied
([`internal/ai/tools.go:47-52`], [`internal/ai/tools.go:738-755`]). Every kind except an app carries a before/after
preview keyed by the version registry's document type ([`internal/ai/tools.go:78-80`],
[`internal/ai/change.go:24-37`]).

A human applies the card through the same admin APIs as the manual builders ([`nexus-web/lib/aiApply.ts:1-5`]). From
there the change follows drafts, publish and rollback ([ADR 0002](0002-schema-cutoff-drafts-and-rollback.md)). Physical
tables come from the engine: `internal/ddl` builds the DDL from metadata, and an identifier must match
`^[a-z][a-z0-9_]{0,25}$` and not be a reserved word ([`internal/ddl/ddl.go:92-113`],
[`internal/ddl/create_table.go:164-173`]).

## Consequences

**Gained.**

- A change is reviewed as data: which document, which operation, which paths changed.
- The vocabulary bounds the blast radius, and the agent's vocabulary is narrower than the engine's. `propose_workflow`
  accepts only `after_*` triggers and refuses webhook, assign and create-record actions, so a webhook URL or a rule that
  creates records comes from a human ([`internal/ai/tools.go:644-668`], [`internal/ai/tools.go:1477-1508`]).
  `propose_access` grants only view, create and edit, at scope all or owner, never delete
  ([`internal/ai/tools_access.go:17-23`]).
- A fix to the engine fixes every app at once.

**Costs.**

- Expressiveness ends where the engine ends: 21 field types, pinned to the engine's allowlist by a test
  ([`internal/ai/tools.go:715-735`]); 7 workflow triggers and 11 action types
  ([`internal/workflow/workflow.go:34-60`], [`internal/workflow/workflow.go:89-162`]); formulas with arithmetic,
  comparisons, AND/OR and IF, and no string operations or other functions ([`internal/formula/formula.go:23-25`]).
  Custom logic, a custom screen, or an integration other than a webhook or an email is Go work for a human.
- Someone has to maintain that engine. A new kind of document touches the class table, an adapter, the registry, a
  propose tool and the web apply path, which already branches on 15 kinds ([`nexus-web/lib/aiApply.ts:97-229`]). A test
  ties the class table to the registry `main.go` wires, through one constructor, `services.NewMetaRegistry` ([#26]).
  Nothing ties the propose tools or the web client to either.
- Most propose-time checks are structural. `propose_entity` now refuses a name that is taken
  ([`internal/ai/tools.go:1060-1068`]), but other checks that need the database, such as a missing relation target, run
  when the card is applied ([`internal/ai/tools.go:1030-1038`]), so a valid card can still fail. One gap is open:
  `propose_field` judges a dependent picklist against the published model and never checks that the table exists
  ([`internal/ai/tools.go:1103`], [#22]).
- The tool list costs about 52 KB per model call ([#22]).

**Given up.** The model's ability to write code. An app that needs behaviour the engine lacks waits until the engine
has it.

## Alternatives considered

- **Generate application code.** The most expressive option. It lost because each app becomes a codebase to review, test
  and patch, and tenant isolation would have to be proven again for each one.
- **Tools that write metadata live.** One click fewer, but a wrong proposal would reach users with nobody in between. It
  lost on blast radius ([ADR 0003](0003-agent-boundary-from-change-classes.md)).
- **Let the model emit SQL or DDL.** It lost on safety: identifiers cannot be bound as parameters, and the allowlist in
  `internal/ddl` is the main defence on the dynamic-schema path ([`internal/ddl/ddl.go:1-5`]).
- **A scripting language inside metadata.** It would close the expressiveness gap, but it brings code review back and
  needs a sandbox. Not chosen: gaps are closed by extending the engine.

<!-- Links below are generated; each code link points at the commit named under Recorded. -->

[`internal/metadata/metadata.go:9-10`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metadata/metadata.go#L9-L10
[`cmd/server/main.go:423`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/cmd/server/main.go#L423
[`internal/ai/tools.go:104-115`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L104-L115
[`internal/ai/tools.go:766-824`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L766-L824
[`internal/ai/tools_test.go:188-212`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools_test.go#L188-L212
[`internal/ai/agent.go:190`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/agent.go#L190
[`internal/ai/tools.go:1030-1079`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L1030-L1079
[`internal/ai/tools.go:1081-1109`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L1081-L1109
[`internal/ai/tools.go:757-764`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L757-L764
[`internal/ai/tools.go:47-52`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L47-L52
[`internal/ai/tools.go:738-755`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L738-L755
[`internal/ai/tools.go:78-80`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L78-L80
[`internal/ai/change.go:24-37`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/change.go#L24-L37
[`nexus-web/lib/aiApply.ts:1-5`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/aiApply.ts#L1-L5
[`internal/ddl/ddl.go:92-113`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/ddl.go#L92-L113
[`internal/ddl/create_table.go:164-173`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/create_table.go#L164-L173
[`internal/ai/tools.go:644-668`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L644-L668
[`internal/ai/tools.go:1477-1508`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L1477-L1508
[`internal/ai/tools_access.go:17-23`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools_access.go#L17-L23
[`internal/ai/tools.go:715-735`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L715-L735
[`internal/workflow/workflow.go:34-60`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/workflow/workflow.go#L34-L60
[`internal/workflow/workflow.go:89-162`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/workflow/workflow.go#L89-L162
[`internal/formula/formula.go:23-25`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/formula/formula.go#L23-L25
[`nexus-web/lib/aiApply.ts:97-229`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/aiApply.ts#L97-L229
[`internal/ai/tools.go:1060-1068`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L1060-L1068
[`internal/ai/tools.go:1030-1038`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L1030-L1038
[`internal/ai/tools.go:1103`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L1103
[`internal/ddl/ddl.go:1-5`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ddl/ddl.go#L1-L5
[#22]: https://github.com/doxuta/nexus/issues/22
[#26]: https://github.com/doxuta/nexus/pull/26
