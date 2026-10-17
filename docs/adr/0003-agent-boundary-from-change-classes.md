# 0003. The agent boundary follows the change classes

- **Status:** Accepted
- **Recorded:** 2026-10-17, after the fact. Citations are pinned to `d300dfc`, main after #20 (MCP) and #21 (access).
- **Paths:** `internal/` is under `nexus-platform/`. Every link is pinned to the commit.

## Context

Three kinds of agent act on a workspace: the builder agent in the Studio, the end-user assistant (⌘K), and outside
agents over MCP ([#20]). [ADR 0002](0002-schema-cutoff-drafts-and-rollback.md) sorts every change into classes by how
it reaches users: on publish, at once with a history entry, or at once with none. The forces:

- An agent's mistake must not reach users unreviewed.
- The list of changes that need an explicit confirm must not be a second table that drifts from the engine's.
- A credential handed to an outside agent will leak sooner or later.

## Decision

**Agents read and propose; they never apply.** The builder agent's executor holds read-only interfaces (the metadata
reader, [`internal/ai/tools.go:23-45`]; the document readers, [`internal/ai/doc_readers.go:22-41`]; the access probes,
[`internal/ai/tools.go:2943-2996`]) and an in-memory list of proposals ([`internal/ai/tools.go:738-755`]). The ⌘K
assistant has its own tool list: reads, plus two record drafts that the browser applies under the user's own token
([`internal/ai/assistant_tools.go:1-16`]).

**A human applies, and the class decides how.** `GET /admin/publish/state` returns the draftable and the direct types
from the class table ([`internal/services/publish_service.go:174-181`], [`internal/metaversion/class.go:53-56`]), and
the browser maps them in one place ([`nexus-web/lib/draftClass.ts:1-15`]). A card follows its class
([`nexus-web/lib/aiApply.ts:363-395`]):

- Draft class: no confirm, part of "Apply all". A card that wrote draft rows can be undone until the next publish
  ([`nexus-web/lib/aiApply.ts:397-514`]).
- Direct or untraced class, or a card that writes real records, seeds grants or creates an app: "Apply now", behind
  an in-place confirm, never part of "Apply all".
- Deletes ask on their own and are never part of "Apply all".

Moving a type between classes changes its cards without touching the agent or the card code.

**Access is DirectTraced, and agents may propose it.** On a delivered project a role change applies at once and leaves
a history entry ([`internal/metaversion/class.go:40-44`], [`internal/services/role_service.go:355`]) that Revert can
undo ([`internal/services/publish_revert.go:20-24`]). `propose_access` grants only on an existing role that is not
reserved and that no admin or platform operator holds ([`internal/ai/tools_access.go:135-144`]); only on a table
proposed in this turn or a live one born after the last publish, judged and named by the metadata registry, never by a
draft ([`internal/ai/tools_access.go:258-304`]); only view, create or edit, at scope all or owner
([`internal/ai/tools_access.go:151-161`]); and only additively, never lowering a grant
([`internal/ai/tools_access.go:46-74`]). Over MCP, a thread's earlier proposals count as "this turn"
([`internal/ai/agent.go:274-284`]).

Because the grant reaches users at once, its card always asks in place and is never part of "Apply all", published or
not ([`nexus-web/lib/aiApply.ts:366-389`]). Before writing, the apply re-reads the role and refuses when its grants on
the card's tables are no longer the card's before-image ([`nexus-web/lib/aiApply.ts:231-258`]). On a delivered project
the Studio's access matrix asks in place too ([`nexus-web/components/studio/AccessMatrix.tsx:89-90`],
[`nexus-web/components/studio/AccessMatrix.tsx:268-271`]).

**The boundary is tested adversarially.** The verifier of [#21] found that `propose_entity` with a live table's name and
a harmless label let `propose_access` treat that published table as built in the same turn, and name it on the card by
the model's label. `propose_entity` now refuses a taken name, and `propose_access` judges and names a live table by the
registry. A test replays the exact attack, also with the shadowing draft injected into the buffer
([`internal/ai/tools.go:1030-1034`], [`internal/ai/tools_access_test.go:236-276`]).

**MCP keeps the same boundary.** `/mcp` serves the builder agent's own tools, generated from `ai.ToolDefinitions` and
filtered by name to `list_`, `get_`, `snapshot_`, `explain_` and `propose_`, so `propose_access` appears as
`nexus_propose_access` ([`internal/mcpserver/tools.go:11-28`], [`internal/mcpserver/tools.go:37-61`]); tests pin that
set ([`internal/mcpserver/mcpserver_test.go:32-88`]). A call runs as the issuing builder over that builder's drafts, and
a valid proposal lands in their Studio thread as an ordinary card ([`internal/mcpserver/mcpserver.go:103-108`],
[`internal/mcpserver/mcpserver.go:135-185`]). The `nxm_` token is hashed at rest, lives at most 8 hours and is
re-checked on every request ([`internal/auth/mcp_token.go:12-28`], [`internal/services/mcp_token_service.go:18-28`],
[`internal/services/mcp_token_service.go:175-233`]); every `/api/v1` route refuses it
([`internal/services/api_key_service.go:496-500`]), `/mcp` refuses an API key with 403
([`internal/mcpserver/mcpserver.go:79-111`]), and logout-all or a password change revokes it
([`internal/services/auth_service_extra.go:202-222`]).

## Consequences

**Gained.**

- An agent's worst case is a bad card. A leaked MCP token reads one workspace's configuration and access decisions and
  files cards in one thread, for at most 8 hours. It cannot apply, publish or call `/api/v1`, and its read tools
  return no record values (`explain_access` can confirm that a record id exists and who owns it,
  [`internal/ai/tools.go:3183-3227`]).
- One classification drives the publish screen, the AI cards and pack upgrades
  ([`internal/metaversion/class.go:53-56`]).

**Costs.**

- A human is in the loop for every change, trivial ones included.
- The confirm is a client rule. A card goes through the ordinary admin APIs ([`nexus-web/lib/aiApply.ts:1-5`]), so the
  server cannot tell a card from a click, and anyone with the builder's session can skip the confirm. The boundary
  holds because agents hold no write credential, not because the API refuses agent writes.
- `PUT /admin/roles/:id` carries no version. The stale check catches a change made after the proposal, but an edit
  made between the apply's own read and its write is lost ([`nexus-web/lib/aiApply.ts:251-253`], [#23]).
- Some of it is still kept by hand: the agent's sentence about where changes land is literal text
  ([`internal/ai/agent.go:104-105`]); the access matrix asks on delivery state, not on the class
  ([`nexus-web/components/shell/useDirectApply.tsx:60-67`]); the MCP filter is a naming rule; and every MCP propose tool's
  note says the human "applies it to drafts and publishes it", wrong for an access card
  ([`internal/mcpserver/tools.go:34-35`], [#22]).
- Open: no rate limit and no cap on turns per thread on `/mcp` ([#22]); no read tool lists a role's grants ([#23]).

**Given up.** Agents that apply their own changes.

## Alternatives considered

- **Agents write drafts, humans only publish.** Publish ships every pending draft of the tenant as one change set
  ([`internal/services/publish_service.go:450-468`]), so an agent's draft would ride along with the builder's work, and
  direct types have no draft to stop at. It lost.
- **A confirm table in the agent layer.** A second list of types that drifts from the engine's. It lost to the class
  lists the API serves.
- **Give outside agents an API key.** Keys are role-bound machine principals that never reach `/admin/*`
  ([`internal/services/api_key_service.go:21-40`]); widening one to the builder's tools would put a builder credential
  on the whole API. It lost to a token that works only at `/mcp`.
- **Let agents act, roll back afterwards.** Rollback does not undo applied-at-once changes as one step and does not
  touch records ([ADR 0002](0002-schema-cutoff-drafts-and-rollback.md)). It lost: review before is cheaper than repair
  after.

<!-- Links below are generated; each code link points at the commit named under Recorded. -->

[`internal/ai/tools.go:23-45`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L23-L45
[`internal/ai/doc_readers.go:22-41`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/doc_readers.go#L22-L41
[`internal/ai/tools.go:2943-2996`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L2943-L2996
[`internal/ai/tools.go:738-755`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L738-L755
[`internal/ai/assistant_tools.go:1-16`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_tools.go#L1-L16
[`internal/services/publish_service.go:174-181`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_service.go#L174-L181
[`internal/metaversion/class.go:53-56`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metaversion/class.go#L53-L56
[`nexus-web/lib/draftClass.ts:1-15`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/draftClass.ts#L1-L15
[`nexus-web/lib/aiApply.ts:363-395`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/aiApply.ts#L363-L395
[`nexus-web/lib/aiApply.ts:397-514`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/aiApply.ts#L397-L514
[`internal/metaversion/class.go:40-44`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metaversion/class.go#L40-L44
[`internal/services/role_service.go:355`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/role_service.go#L355
[`internal/services/publish_revert.go:20-24`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_revert.go#L20-L24
[`internal/ai/tools_access.go:135-144`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools_access.go#L135-L144
[`internal/ai/tools_access.go:258-304`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools_access.go#L258-L304
[`internal/ai/tools_access.go:151-161`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools_access.go#L151-L161
[`internal/ai/tools_access.go:46-74`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools_access.go#L46-L74
[`internal/ai/agent.go:274-284`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/agent.go#L274-L284
[`nexus-web/lib/aiApply.ts:366-389`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/aiApply.ts#L366-L389
[`nexus-web/lib/aiApply.ts:231-258`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/aiApply.ts#L231-L258
[`nexus-web/components/studio/AccessMatrix.tsx:89-90`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/components/studio/AccessMatrix.tsx#L89-L90
[`nexus-web/components/studio/AccessMatrix.tsx:268-271`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/components/studio/AccessMatrix.tsx#L268-L271
[`internal/ai/tools.go:1030-1034`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L1030-L1034
[`internal/ai/tools_access_test.go:236-276`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools_access_test.go#L236-L276
[`internal/mcpserver/tools.go:11-28`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/mcpserver/tools.go#L11-L28
[`internal/mcpserver/tools.go:37-61`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/mcpserver/tools.go#L37-L61
[`internal/mcpserver/mcpserver_test.go:32-88`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/mcpserver/mcpserver_test.go#L32-L88
[`internal/mcpserver/mcpserver.go:103-108`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/mcpserver/mcpserver.go#L103-L108
[`internal/mcpserver/mcpserver.go:135-185`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/mcpserver/mcpserver.go#L135-L185
[`internal/auth/mcp_token.go:12-28`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/auth/mcp_token.go#L12-L28
[`internal/services/mcp_token_service.go:18-28`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/mcp_token_service.go#L18-L28
[`internal/services/mcp_token_service.go:175-233`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/mcp_token_service.go#L175-L233
[`internal/services/api_key_service.go:496-500`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/api_key_service.go#L496-L500
[`internal/mcpserver/mcpserver.go:79-111`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/mcpserver/mcpserver.go#L79-L111
[`internal/services/auth_service_extra.go:202-222`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/auth_service_extra.go#L202-L222
[`internal/ai/tools.go:3183-3227`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L3183-L3227
[`nexus-web/lib/aiApply.ts:1-5`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/aiApply.ts#L1-L5
[`nexus-web/lib/aiApply.ts:251-253`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/aiApply.ts#L251-L253
[`internal/ai/agent.go:104-105`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/agent.go#L104-L105
[`nexus-web/components/shell/useDirectApply.tsx:60-67`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/components/shell/useDirectApply.tsx#L60-L67
[`internal/mcpserver/tools.go:34-35`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/mcpserver/tools.go#L34-L35
[`internal/services/publish_service.go:450-468`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/publish_service.go#L450-L468
[`internal/services/api_key_service.go:21-40`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/api_key_service.go#L21-L40
[#20]: https://github.com/doxuta/nexus/pull/20
[#21]: https://github.com/doxuta/nexus/pull/21
[#22]: https://github.com/doxuta/nexus/issues/22
[#23]: https://github.com/doxuta/nexus/issues/23
