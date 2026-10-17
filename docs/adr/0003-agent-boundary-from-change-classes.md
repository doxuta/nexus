# 0003. The agent boundary follows the change classes

- **Status:** Accepted. The MCP part lands with [#20].
- **Recorded:** 2026-10-17, after the fact, from the code at `b85bac4`; MCP from `slice/mcp` at `daa7a3e`
- **Paths:** `internal/` is under `nexus-platform/`. Every link is pinned to its commit; "on #20" marks the MCP branch.

## Context

Three kinds of agent act on a workspace: the builder agent in the Studio, the end-user assistant (⌘K) and, with [#20],
outside agents over MCP. [ADR 0002](0002-schema-cutoff-drafts-and-rollback.md) sorts every change into classes by how
it reaches users: on publish, at once with a history entry, or at once with none. The forces:

- An agent's mistake must not reach users unreviewed.
- The list of changes that need an explicit confirm must not be a second table that drifts from the engine's.
- A credential handed to an outside agent will leak sooner or later.

## Decision

**Agents read and propose; they never apply.** The builder agent's executor holds read-only interfaces (the metadata
reader, [`internal/ai/tools.go:22-44`]; the document readers, [`internal/ai/doc_readers.go:22-41`]; the access probes,
[`internal/ai/tools.go:2901-2940`]) and an in-memory list of proposals ([`internal/ai/tools.go:709-726`]). The ⌘K
assistant has its own tool list: reads, plus two record drafts that the browser applies under the user's own token
([`internal/ai/assistant_tools.go:1-16`]).

**A human applies, and the class decides how.** `GET /admin/publish/state` returns the draftable and the direct types
from the class table ([`internal/services/publish_service.go:165-172`], [`internal/metaversion/class.go:53-56`]), and
the browser maps them in one place ([`nexus-web/lib/draftClass.ts:1-15`]). A card follows its class
([`nexus-web/lib/aiApply.ts:330-359`]):

- Draft class: no confirm, part of "Apply all". A card that wrote draft rows can be undone until the next publish
  ([`nexus-web/lib/aiApply.ts:361-478`]).
- Direct or untraced class, or a card that writes real records, seeds grants or creates an app: "Apply now", behind
  an in-place confirm, never part of "Apply all".
- Deletes ask on their own and are never part of "Apply all".

Moving a type between classes changes its cards without touching the agent or the card code.

**Access is DirectTraced** ([`internal/metaversion/class.go:40-44`]). On a delivered project a role change applies at
once and leaves a history entry ([`internal/services/role_service.go:355`]) that Revert can undo
([`internal/services/publish_revert.go:20-24`]). Nothing stands between that save and the users, so the Studio's access
matrix asks in place before it saves ([`nexus-web/components/studio/AccessMatrix.tsx:67-68`],
[`nexus-web/components/studio/AccessMatrix.tsx:230-233`]). A tool for agents to propose access is in progress ([#2]).

**MCP keeps the same boundary (lands with [#20]).**

- `/mcp` serves the builder agent's tools, generated from `ai.ToolDefinitions`; only names that start with `list_`,
  `get_`, `snapshot_`, `explain_` or `propose_` are exposed ([`internal/mcpserver/tools.go:11-28` on #20],
  [`internal/mcpserver/tools.go:37-61` on #20]). Tests feed the generator write-like names and fail if the builder
  agent gains a tool that neither reads nor proposes ([`internal/mcpserver/mcpserver_test.go:32-88` on #20]).
- A call runs the in-app executor as the issuing builder, over that builder's drafts, and a valid proposal lands in
  the builder's Studio thread as an ordinary card ([`internal/ai/agent.go:274-284` on #20],
  [`internal/mcpserver/mcpserver.go:103-108` on #20], [`internal/mcpserver/mcpserver.go:135-185` on #20]).
- The `nxm_` token is stored only as a SHA-256 hash ([`internal/auth/mcp_token.go:12-28` on #20]) and lives at most 8
  hours ([`internal/services/mcp_token_service.go:18-28` on #20]). Each request re-checks it: revoked, expired, a
  retired or suspended workspace, or a user who is no longer an active builder means 401
  ([`internal/services/mcp_token_service.go:175-233` on #20]). Every `/api/v1` route refuses it
  ([`internal/services/api_key_service.go:496-500` on #20]); `/mcp` answers an API key with 403
  ([`internal/mcpserver/mcpserver.go:79-111` on #20]); logout-all or a password change revokes it
  ([`internal/services/auth_service_extra.go:202-222` on #20]).

## Consequences

**Gained.**

- An agent's worst case is a bad card. A leaked MCP token reads one workspace's configuration and access decisions and
  files cards in one thread, for at most 8 hours. It cannot apply, publish or call `/api/v1`, and its read tools
  return no record values (`explain_access` can confirm that a record id exists and who owns it,
  [`internal/ai/tools.go:3127-3171`]).
- One classification drives the publish screen, the AI cards and pack upgrades
  ([`internal/metaversion/class.go:53-56`]).

**Costs.**

- A human is in the loop for every change, trivial ones included. An agent cannot finish a task alone.
- The confirm is a client rule. A card goes through the ordinary admin APIs ([`nexus-web/lib/aiApply.ts:1-5`]), so the
  server cannot tell a card from a click, and anyone with the builder's session can skip the confirm. The boundary
  holds because agents hold no write credential, not because the API refuses agent writes.
- Some of it is still kept by hand: the agent's sentence about where changes land is literal text
  ([`internal/ai/agent.go:104-105`]); the access matrix asks on delivery state, not on the class
  ([`nexus-web/components/shell/useDirectApply.tsx:60-67`]); the MCP filter is a naming rule ([#22]).
- Open on MCP: no rate limit and no cap on turns per thread ([#22]).

**Given up.** Agents that apply their own changes.

## Alternatives considered

- **Agents write drafts, humans only publish.** It saves a click, but Publish ships every pending draft of the tenant
  as one change set ([`internal/services/publish_service.go:441-459`]), so an agent's draft would ride along with the
  builder's work, and direct types have no draft to stop at. It lost.
- **A confirm table in the agent layer.** Simple, but a second list of types that drifts from the engine's. It lost to
  the class lists the API serves.
- **Give outside agents an API key.** Keys are role-bound machine principals that never reach `/admin/*`
  ([`internal/services/api_key_service.go:21-40`]); widening one to the builder's tools would put a builder credential
  on the whole API. It lost to a token that works only at `/mcp`.
- **Let agents act, roll back afterwards.** Rollback does not undo applied-at-once changes as one step and does not
  touch records ([ADR 0002](0002-schema-cutoff-drafts-and-rollback.md)). Review before a change is cheaper than repair
  after it. It lost.

<!-- Links below are generated; each one points at the commit named under Status. -->

[`internal/ai/tools.go:22-44`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L22-L44
[`internal/ai/doc_readers.go:22-41`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/doc_readers.go#L22-L41
[`internal/ai/tools.go:2901-2940`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L2901-L2940
[`internal/ai/tools.go:709-726`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L709-L726
[`internal/ai/assistant_tools.go:1-16`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/assistant_tools.go#L1-L16
[`internal/services/publish_service.go:165-172`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/services/publish_service.go#L165-L172
[`internal/metaversion/class.go:53-56`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/metaversion/class.go#L53-L56
[`nexus-web/lib/draftClass.ts:1-15`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-web/lib/draftClass.ts#L1-L15
[`nexus-web/lib/aiApply.ts:330-359`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-web/lib/aiApply.ts#L330-L359
[`nexus-web/lib/aiApply.ts:361-478`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-web/lib/aiApply.ts#L361-L478
[`internal/metaversion/class.go:40-44`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/metaversion/class.go#L40-L44
[`internal/services/role_service.go:355`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/services/role_service.go#L355
[`internal/services/publish_revert.go:20-24`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/services/publish_revert.go#L20-L24
[`nexus-web/components/studio/AccessMatrix.tsx:67-68`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-web/components/studio/AccessMatrix.tsx#L67-L68
[`nexus-web/components/studio/AccessMatrix.tsx:230-233`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-web/components/studio/AccessMatrix.tsx#L230-L233
[`internal/mcpserver/tools.go:11-28` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/mcpserver/tools.go#L11-L28
[`internal/mcpserver/tools.go:37-61` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/mcpserver/tools.go#L37-L61
[`internal/mcpserver/mcpserver_test.go:32-88` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/mcpserver/mcpserver_test.go#L32-L88
[`internal/ai/agent.go:274-284` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/ai/agent.go#L274-L284
[`internal/mcpserver/mcpserver.go:103-108` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/mcpserver/mcpserver.go#L103-L108
[`internal/mcpserver/mcpserver.go:135-185` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/mcpserver/mcpserver.go#L135-L185
[`internal/auth/mcp_token.go:12-28` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/auth/mcp_token.go#L12-L28
[`internal/services/mcp_token_service.go:18-28` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/services/mcp_token_service.go#L18-L28
[`internal/services/mcp_token_service.go:175-233` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/services/mcp_token_service.go#L175-L233
[`internal/services/api_key_service.go:496-500` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/services/api_key_service.go#L496-L500
[`internal/mcpserver/mcpserver.go:79-111` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/mcpserver/mcpserver.go#L79-L111
[`internal/services/auth_service_extra.go:202-222` on #20]: https://github.com/doxuta/nexus/blob/daa7a3e7a94e9774aa63ad9bc85080a09f2842b3/nexus-platform/internal/services/auth_service_extra.go#L202-L222
[`internal/ai/tools.go:3127-3171`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/tools.go#L3127-L3171
[`nexus-web/lib/aiApply.ts:1-5`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-web/lib/aiApply.ts#L1-L5
[`internal/ai/agent.go:104-105`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/ai/agent.go#L104-L105
[`nexus-web/components/shell/useDirectApply.tsx:60-67`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-web/components/shell/useDirectApply.tsx#L60-L67
[`internal/services/publish_service.go:441-459`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/services/publish_service.go#L441-L459
[`internal/services/api_key_service.go:21-40`]: https://github.com/doxuta/nexus/blob/b85bac4c1255146cfdabdd681c992b428c53c665/nexus-platform/internal/services/api_key_service.go#L21-L40
[#2]: https://github.com/doxuta/nexus/issues/2
[#20]: https://github.com/doxuta/nexus/pull/20
[#22]: https://github.com/doxuta/nexus/issues/22
