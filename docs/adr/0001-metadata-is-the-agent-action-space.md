# 0001. Metadata is the agent's action space

- **Status:** Accepted
- **Recorded:** 2026-10-17, after the fact, from the code at `b85bac4`
- **Paths:** `internal/`, `cmd/` and `scripts/` are under `nexus-platform/`. Every link is pinned to the commit.

## Context

An AI builder that writes application code hands each customer a new codebase. Someone has to review every generated
line, and every bug fix has to be made once per app. Nexus runs every customer app on one engine instead: an app is
rows in system tables that the engine reads at runtime ([`internal/metadata/metadata.go:9-10`]), and the
configuration document types are versioned per tenant ([`cmd/server/main.go:422`]).

The forces:

- A builder must be able to judge a change before users see it.
- A wrong model output needs a bounded blast radius.
- The model must never choose the tenant.
- The code that runs customer apps must stay small enough for humans to own.

## Decision

The builder agent writes no code, SQL or DDL. Its action space is a fixed list of typed tools over the metadata model,
returned by `ToolDefinitions` ([`internal/ai/tools.go:102-113`]). At `b85bac4` there are 25: 11 read tools and 14
`propose_*` tools that validate and buffer a change ([`internal/ai/tools.go:737-793`]). A test pins the count and fails
if any tool schema mentions a tenant ([`internal/ai/tools_test.go:188-211`]); the executor takes the tenant from the
verified request ([`internal/ai/agent.go:190`]).

A proposal goes through the validator the manual builder uses: `propose_entity` calls
`services.ValidateCreateEntity` ([`internal/ai/tools.go:999-1037`]), `propose_field` calls `services.ValidateAddField`
([`internal/ai/tools.go:1039-1067`]). A refusal goes back to the model as a coded error it can correct
([`internal/ai/tools.go:728-735`]). A valid proposal is appended to an in-memory list, and nothing is applied
([`internal/ai/tools.go:46-51`], [`internal/ai/tools.go:709-726`]). Every kind except an app carries a before/after
preview keyed by the version registry's document type ([`internal/ai/tools.go:76-78`],
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
  creates records comes from a human ([`internal/ai/tools.go:615-639`], [`internal/ai/tools.go:1435-1466`]).
- A fix to the engine fixes every app at once.

**Costs.**

- Expressiveness ends where the engine ends: 21 field types, pinned to the engine's allowlist by a test
  ([`internal/ai/tools.go:686-706`]); 7 workflow triggers and 11 action types
  ([`internal/workflow/workflow.go:34-60`], [`internal/workflow/workflow.go:89-162`]); formulas with arithmetic,
  comparisons, AND/OR and IF, and no string operations or other functions ([`internal/formula/formula.go:23-25`]).
  Custom logic, a custom screen, or an integration other than a webhook or an email is Go work for a human.
- Someone has to maintain that engine. A new kind of document touches the class table, an adapter, the registry, a
  propose tool and the web apply path, which already branches on 14 kinds ([`nexus-web/lib/aiApply.ts:95-225`]). A test
  ties the class table to a hand-kept copy of the registry; its comment says another test pins that copy to `main.go`,
  but none does ([`internal/services/draft_class_test.go:15-37`]). Nothing ties the propose tools or the web client to
  either.
- Propose-time checks are structural. Checks that need the database, such as a duplicate name or a missing relation
  target, run when the card is applied ([`internal/ai/tools.go:999-1005`]), so a valid card can still fail. One gap is
  open: `propose_field` judges a dependent picklist against the published model and never checks that the table exists
  ([`internal/ai/tools.go:1061`], [#22]).
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

<!-- Links below are generated; each one points at the commit named under Status. -->

[`internal/metadata/metadata.go:9-10`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/metadata/metadata.go#L9-L10
[`cmd/server/main.go:422`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/cmd/server/main.go#L422
[`internal/ai/tools.go:102-113`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L102-L113
[`internal/ai/tools.go:737-793`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L737-L793
[`internal/ai/tools_test.go:188-211`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools_test.go#L188-L211
[`internal/ai/agent.go:190`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/agent.go#L190
[`internal/ai/tools.go:999-1037`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L999-L1037
[`internal/ai/tools.go:1039-1067`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L1039-L1067
[`internal/ai/tools.go:728-735`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L728-L735
[`internal/ai/tools.go:46-51`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L46-L51
[`internal/ai/tools.go:709-726`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L709-L726
[`internal/ai/tools.go:76-78`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L76-L78
[`internal/ai/change.go:24-37`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/change.go#L24-L37
[`nexus-web/lib/aiApply.ts:1-5`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-web/lib/aiApply.ts#L1-L5
[`internal/ddl/ddl.go:92-113`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ddl/ddl.go#L92-L113
[`internal/ddl/create_table.go:164-173`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ddl/create_table.go#L164-L173
[`internal/ai/tools.go:615-639`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L615-L639
[`internal/ai/tools.go:1435-1466`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L1435-L1466
[`internal/ai/tools.go:686-706`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L686-L706
[`internal/workflow/workflow.go:34-60`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/workflow/workflow.go#L34-L60
[`internal/workflow/workflow.go:89-162`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/workflow/workflow.go#L89-L162
[`internal/formula/formula.go:23-25`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/formula/formula.go#L23-L25
[`nexus-web/lib/aiApply.ts:95-225`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-web/lib/aiApply.ts#L95-L225
[`internal/services/draft_class_test.go:15-37`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/services/draft_class_test.go#L15-L37
[`internal/ai/tools.go:999-1005`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L999-L1005
[`internal/ai/tools.go:1061`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L1061
[`internal/ddl/ddl.go:1-5`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ddl/ddl.go#L1-L5
[#22]: https://github.com/doxuta/nexus/issues/22
