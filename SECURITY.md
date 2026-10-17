# Security

Nexus lets AI agents change business apps. Most of what can go wrong is therefore an agent, or someone steering one,
doing more than the person it works for. This page is the threat model for that: who can do what, the trust
boundaries, prompt injection, the human gate, and the known gaps. It describes `main` at `d300dfc`. There are no
releases, so fixes land on `main`. Paths that start with `internal/`, `cmd/` or `scripts/` are under `nexus-platform/`,
and every code link is pinned to that commit.

## Reporting a vulnerability

Report it privately through GitHub: on this repository's **Security** tab, choose **Report a vulnerability**. That
opens a private security advisory. Please do not open a public issue or pull request. Say which credential you used,
what you reached, and how; a request and its response help most. The fix and the disclosure are coordinated in the
advisory.

## Who can do what

| Actor | Credential | Can | Bound by |
|---|---|---|---|
| Builder | JWT. "Builder" is admin plus the platform-operator flag, both read from the live user row on every request ([`internal/metaversion/context.go:55-59`], [`internal/middleware/auth.go:295-313`]) | everything in their tenant, including the builder-only routes: schema, layouts, the AI builder, API keys and MCP keys ([`internal/handlers/router.go:474-482`], [`internal/handlers/router.go:637-643`], [`internal/middleware/platform_operator.go:89-96`]) | the tenant filter; skips entity and row ACL ([`internal/middleware/acl.go:105-112`]) |
| Customer admin | JWT, admin without the operator flag | governance: users, roles, teams, audit, field ACL, rules, reports ([`internal/handlers/router.go:491-540`], [`internal/handlers/router.go:609-610`]) | the tenant filter; skips ACL; on a delivered project writes live, with a history entry ([`internal/services/draft_gate.go:53-60`], [`internal/services/version_recorder.go:137-139`]) |
| Rep (staff) | JWT; a user holds one role ([`internal/ai/tools_access.go:17-19`]) | records and the ⌘K assistant ([`internal/handlers/router.go:796-808`]) | entity ACL, row scope, field ACL |
| Builder in a customer workspace | workspace-session token, re-resolved on every request | acts as admin and builder in the customer tenant ([`internal/middleware/auth.go:205-213`], [`internal/middleware/auth.go:283-287`]) | refused on its home account's self-service routes ([`internal/middleware/workspace_session.go:13-18`]) and for MCP keys ([`internal/services/mcp_token_service.go:95-100`]) |
| Preview as a role | preview token | reads, as one role | no writes, no ⌘K ([`internal/middleware/preview_readonly.go:22-33`]) |
| In-app builder agent | none of its own: it runs inside the builder's chat request ([`internal/handlers/router.go:616-617`]) | the builder's read and `propose_*` tools | proposals only ([ADR 0003](docs/adr/0003-agent-boundary-from-change-classes.md)) |
| ⌘K assistant | none of its own: it runs inside the user's request | reads under that user's ACL; proposes record drafts ([`internal/ai/assistant_tools.go:1-16`]) | the user confirms every draft |
| Outside agent over MCP | `nxm_` key | the builder's read and `propose_*` tools, at `/mcp` only, as the builder who issued the key ([`internal/mcpserver/mcpserver.go:1-7`]) | at most 8 hours, re-checked on every request ([`internal/services/mcp_token_service.go:18-28`]) |
| API key | `nxk_` key | the record API as one role | never admin, never `/admin/*` ([`internal/services/api_key_service.go:21-40`]); 403 at `/mcp` ([`internal/mcpserver/mcpserver.go:84-88`]) |

## Trust boundaries

- **Tenant.** The tenant comes from the verified token, and a request without one is refused
  ([`internal/middleware/auth.go:331-349`]). By rule, every query on business data filters `tenant_id`
  ([`AGENTS.md:29`]). Admins skip ACL but not the tenant ([`internal/dynrepo/rowscope.go:182-185`]). This is a convention
  plus tests, not database-level security ([ADR 0005](docs/adr/0005-tenant-isolation-and-row-level-access.md)).
- **Entity ACL and row scope.** Access is denied by default and checked before the handler, then narrowed to rows by a
  predicate in the repository ([`internal/services/acl_service.go:25-88`], [`internal/dynrepo/rowscope.go:10-40`]).
  Admins skip both, and every field-level restriction too ([`internal/ai/assistant_reader.go:88-90`]).
- **Drafts and publish.** On a delivered project the customer sees only what is published. A table born after the last
  publish is hidden from everyone but the builder, on the record path as well, with residual paths open
  ([ADR 0002](docs/adr/0002-schema-cutoff-drafts-and-rollback.md), [#23]).
- **Credentials.**
  - A JWT is HS256 with an algorithm check ([`internal/auth/token.go:90-95`]). Role, admin and status come from the
    database on every request, and a disabled user or an ended session is refused at once
    ([`internal/middleware/auth.go:132-156`]). The builder flag is never a claim ([`internal/auth/token.go:76-79`]).
  - An `nxk_` key acts as one role and is never admin ([`internal/services/api_key_service.go:21-40`]). Failed key
    lookups are throttled per IP ([`internal/middleware/auth.go:167-172`]).
  - An `nxm_` key is stored only as a hash ([`internal/auth/mcp_token.go:12-28`]). It is refused when revoked or
    expired, when the workspace is retired or suspended, or when its user is no longer an active builder
    ([`internal/services/mcp_token_service.go:175-233`]). Logout-all and a password change revoke it
    ([`internal/services/auth_service_extra.go:202-222`]).
- **`/mcp` and `/api/v1` are separate.** `/mcp` is mounted outside `/api/v1`, with its own bearer
  ([`cmd/server/main.go:1516-1518`]). `/api/v1` refuses an `nxm_` key ([`internal/services/api_key_service.go:496-500`]).
  `/mcp` answers an `nxk_` key with 403 and any other non-`nxm_` credential with 401
  ([`internal/mcpserver/mcpserver.go:79-111`]).

## Prompt injection

Text an agent reads can carry instructions. Two sources matter:

- **Configuration, read by the builder agent and by MCP agents.** Their read tools return names, labels, layouts, rules
  with webhook secrets masked, report definitions, role names and access decisions
  ([`internal/ai/tools.go:826-836`], [`internal/ai/doc_readers.go:59-175`], [`internal/ai/tools.go:1265-1274`]). Rules, reports and roles are written on
  admin routes, so a customer admin can write text that the builder's agent later reads
  ([`internal/handlers/router.go:503-540`]). No builder read tool returns record field values. `explain_access` can
  confirm that a record id exists and give its owner's id ([`internal/ai/tools.go:3183-3254`]).
- **Records, read by the ⌘K assistant.** `query_records` returns up to 50 rows with their field values;
  `run_report` and `aggregate_records` return results computed from records. All three run under the asking user's
  ACL ([`internal/ai/assistant_tools.go:32-37`], [`internal/ai/assistant_tools.go:290-314`],
  [`internal/ai/assistant_tools.go:562-601`], [`internal/ai/assistant_reader.go:12-20`]). Anyone who can write a record
  the user can read can plant text in it.

An injected model **can**:

- say anything in its reply. Replies render through `MarkdownLite`, which emits no HTML and no images and links only to
  app paths, so a reply cannot make the browser fetch an outside URL
  ([`nexus-web/components/ui/MarkdownLite.tsx:3-12`], [`nexus-web/components/ui/MarkdownLite.tsx:26-31`]);
- file cards, some of them harmful if applied unread. For example, `propose_workflow` accepts an `email_alert` to any
  address with record fields in its body ([`internal/ai/tools.go:644-668`],
  [`internal/services/workflow_service.go:1597-1614`]). It runs only after a human applies it and publishes. A ⌘K record
  draft is written only when the user confirms it, under their own token
  ([`nexus-web/components/ui/CommandPalette.tsx:20-24`]);
- over MCP, steer the outside agent. Tool results enter that agent's context, and it may hold tools outside Nexus (a
  shell, files). Nexus cannot bound those, so an MCP client should treat `nexus_` tool output as untrusted data.

It **cannot**:

- apply, publish, roll back, write records or change access, because no tool does
  ([`internal/ai/tools.go:766-824`], [`internal/mcpserver/tools.go:14-28`]);
- name a tenant: no tool schema has a tenant parameter ([`internal/ai/tools_test.go:188-212`],
  [`internal/ai/assistant_tools_test.go:270`]);
- propose an outbound webhook, a round-robin assign, a rule that creates records, or a before-save rule
  ([`internal/ai/tools.go:1477-1508`]);
- propose admin rights: `propose_access` never grants delete or a team or field scope, and never touches a reserved role
  or one that an admin or a platform operator holds ([`internal/ai/tools_access.go:17-23`]).

## The human gate and its limits

Every agent change is a card. A human applies it through the same admin APIs as the manual builders, with their own
session ([`nexus-web/lib/aiApply.ts:1-5`]), and a draft-class change then needs a publish. The limits:

- The confirm is a client rule. The server cannot tell a card from a click, and anyone holding the builder's session can
  skip it.
- "Apply all" applies every draft-class card in one click ([`nexus-web/lib/aiApply.ts:363-395`]). A card's text comes
  partly from the model; an access card names its tables from the registry, never from the model
  ([`internal/ai/tools_access.go:258-282`]).
- `PUT /admin/roles/:id` has no version, so an edit made between an access card's read and its write is lost
  ([`nexus-web/lib/aiApply.ts:251-253`], [#23]).
- Admins skip ACL, so testing as the builder proves nothing about the rep ([`AGENTS.md:33-34`]).

## Known gaps

- **Access follow-ups ([#23]).** A hidden-new table is still reachable through N-N `Link` and `ListLinked` and through
  `?expand`, with a crafted call and an approved grant. `PUT /admin/roles/:id` is unversioned.
- **MCP follow-ups ([#22]).** `/mcp` has no rate limit, no brake on failed key lookups, and no cap on turns per thread.
  The MCP tool filter is a naming rule, `propose_field` judges a dependent picklist against the published model, and
  the MCP note misdescribes an access card.
- **404 instead of 403 ([#18]).** A record the rep can read but not edit answers `PUT` with 404. Existence is not leaked,
  but the reason is misstated.
- **The cutoff's second ([#19]).** Metadata timestamps have second precision, so a table born in the same second as a
  publish can land on the wrong side of the cutoff.

<!-- Links below are generated; each code link points at d300dfc, named in the first paragraph. -->

[`internal/metaversion/context.go:55-59`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/metaversion/context.go#L55-L59
[`internal/middleware/auth.go:295-313`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/auth.go#L295-L313
[`internal/handlers/router.go:474-482`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/handlers/router.go#L474-L482
[`internal/handlers/router.go:637-643`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/handlers/router.go#L637-L643
[`internal/middleware/platform_operator.go:89-96`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/platform_operator.go#L89-L96
[`internal/middleware/acl.go:105-112`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/acl.go#L105-L112
[`internal/handlers/router.go:491-540`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/handlers/router.go#L491-L540
[`internal/handlers/router.go:609-610`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/handlers/router.go#L609-L610
[`internal/services/draft_gate.go:53-60`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/draft_gate.go#L53-L60
[`internal/services/version_recorder.go:137-139`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/version_recorder.go#L137-L139
[`internal/ai/tools_access.go:17-19`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools_access.go#L17-L19
[`internal/handlers/router.go:796-808`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/handlers/router.go#L796-L808
[`internal/middleware/auth.go:205-213`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/auth.go#L205-L213
[`internal/middleware/auth.go:283-287`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/auth.go#L283-L287
[`internal/middleware/workspace_session.go:13-18`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/workspace_session.go#L13-L18
[`internal/services/mcp_token_service.go:95-100`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/mcp_token_service.go#L95-L100
[`internal/middleware/preview_readonly.go:22-33`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/preview_readonly.go#L22-L33
[`internal/handlers/router.go:616-617`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/handlers/router.go#L616-L617
[`internal/ai/assistant_tools.go:1-16`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_tools.go#L1-L16
[`internal/mcpserver/mcpserver.go:1-7`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/mcpserver/mcpserver.go#L1-L7
[`internal/services/mcp_token_service.go:18-28`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/mcp_token_service.go#L18-L28
[`internal/services/api_key_service.go:21-40`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/api_key_service.go#L21-L40
[`internal/mcpserver/mcpserver.go:84-88`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/mcpserver/mcpserver.go#L84-L88
[`internal/middleware/auth.go:331-349`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/auth.go#L331-L349
[`AGENTS.md:29`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/AGENTS.md#L29
[`internal/dynrepo/rowscope.go:182-185`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/rowscope.go#L182-L185
[`internal/services/acl_service.go:25-88`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/acl_service.go#L25-L88
[`internal/dynrepo/rowscope.go:10-40`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/dynrepo/rowscope.go#L10-L40
[`internal/ai/assistant_reader.go:88-90`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_reader.go#L88-L90
[`internal/auth/token.go:90-95`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/auth/token.go#L90-L95
[`internal/middleware/auth.go:132-156`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/auth.go#L132-L156
[`internal/auth/token.go:76-79`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/auth/token.go#L76-L79
[`internal/middleware/auth.go:167-172`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/middleware/auth.go#L167-L172
[`internal/auth/mcp_token.go:12-28`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/auth/mcp_token.go#L12-L28
[`internal/services/mcp_token_service.go:175-233`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/mcp_token_service.go#L175-L233
[`internal/services/auth_service_extra.go:202-222`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/auth_service_extra.go#L202-L222
[`cmd/server/main.go:1516-1518`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/cmd/server/main.go#L1516-L1518
[`internal/services/api_key_service.go:496-500`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/api_key_service.go#L496-L500
[`internal/mcpserver/mcpserver.go:79-111`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/mcpserver/mcpserver.go#L79-L111
[`internal/ai/tools.go:826-836`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L826-L836
[`internal/ai/doc_readers.go:59-175`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/doc_readers.go#L59-L175
[`internal/ai/tools.go:1265-1274`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L1265-L1274
[`internal/handlers/router.go:503-540`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/handlers/router.go#L503-L540
[`internal/ai/tools.go:3183-3254`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L3183-L3254
[`internal/ai/assistant_tools.go:32-37`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_tools.go#L32-L37
[`internal/ai/assistant_tools.go:290-314`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_tools.go#L290-L314
[`internal/ai/assistant_tools.go:562-601`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_tools.go#L562-L601
[`internal/ai/assistant_reader.go:12-20`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_reader.go#L12-L20
[`nexus-web/components/ui/MarkdownLite.tsx:3-12`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/components/ui/MarkdownLite.tsx#L3-L12
[`nexus-web/components/ui/MarkdownLite.tsx:26-31`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/components/ui/MarkdownLite.tsx#L26-L31
[`internal/ai/tools.go:644-668`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L644-L668
[`internal/services/workflow_service.go:1597-1614`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/services/workflow_service.go#L1597-L1614
[`nexus-web/components/ui/CommandPalette.tsx:20-24`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/components/ui/CommandPalette.tsx#L20-L24
[`internal/ai/tools.go:766-824`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L766-L824
[`internal/mcpserver/tools.go:14-28`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/mcpserver/tools.go#L14-L28
[`internal/ai/tools_test.go:188-212`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools_test.go#L188-L212
[`internal/ai/assistant_tools_test.go:270`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/assistant_tools_test.go#L270
[`internal/ai/tools.go:1477-1508`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools.go#L1477-L1508
[`internal/ai/tools_access.go:17-23`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools_access.go#L17-L23
[`nexus-web/lib/aiApply.ts:1-5`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/aiApply.ts#L1-L5
[`nexus-web/lib/aiApply.ts:363-395`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/aiApply.ts#L363-L395
[`internal/ai/tools_access.go:258-282`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-platform/internal/ai/tools_access.go#L258-L282
[`nexus-web/lib/aiApply.ts:251-253`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/nexus-web/lib/aiApply.ts#L251-L253
[`AGENTS.md:33-34`]: https://github.com/doxuta/nexus/blob/d300dfc617ac330dd06b6c4d41f37ae7079e2dc4/AGENTS.md#L33-L34
[#18]: https://github.com/doxuta/nexus/issues/18
[#19]: https://github.com/doxuta/nexus/issues/19
[#22]: https://github.com/doxuta/nexus/issues/22
[#23]: https://github.com/doxuta/nexus/issues/23
