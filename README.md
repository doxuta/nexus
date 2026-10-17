# Nexus

**An agent-native platform for business apps. Agents don't write the app's code — they edit a typed, versioned,
permissioned model of it, and you review a diff instead of 2,000 lines.**

AI app builders that generate code leave you with a codebase nobody can maintain. Nexus takes the other road: the
engine is ordinary, human-maintained Go; an app is metadata (tables, fields, relations, layouts, workflows, access
rules), and that metadata is the only thing an AI is allowed to change.

| What you get | How Nexus does it |
|---|---|
| Review a change, not a code dump | Every AI proposal is a typed diff you can preview before it touches anyone |
| Safe to ship | Changes land as drafts; a human publishes; any version can be rolled back |
| Safe by construction | Tenant isolation and row-level access on every query; the app and its assistant only see what the user may see |
| Works with your agents | Claude Code or Claude Desktop get the builder's own read and propose tools over MCP: agents propose, humans dispose |
| Measured, not vibed | The whole loop runs in CI against a recorded model (record/replay gateway); an eval suite for the builder *(in progress)* |

> **Status:** work in progress, being finished in the open. The engine, the drafts → publish → rollback flow,
> access control, the CRM pack and the MCP endpoint run today; the eval numbers are tracked in
> [Issues](../../issues). The UI is Vietnamese-first; English coverage is partial.

## Quickstart

```bash
docker compose up --build
```

Open <http://localhost:8080> (set `NEXUS_PORT` to use another port).

| Who | Email | Password | Sees |
|---|---|---|---|
| Builder (Studio) | `admin@example.com` | `Demo-Admin-2026!` | everything, edits as drafts |
| Sales rep | `test@example.com` | `Demo-Rep-2026!` | only what the published version and their role allow |

Workspace code: `northwind`. These credentials are for your laptop only; the compose file documents every
override. AI features stay off until you point `AI_GATEWAY_BASE_URL` at an OpenAI-compatible gateway and set
`AI_ENABLED=true`.
Without a model or key, `AI_ENABLED=true AI_GATEWAY_MODE=replay AI_GATEWAY_CASSETTE=/cassettes/<name>.jsonl docker compose up` answers from a cassette in `./cassettes`, recorded once the same way with `AI_GATEWAY_MODE=record` and a live gateway.
The whole loop (the builder asks for a helpdesk app and says who will use it; the assistant proposes the table, the app and the rep's access, which the builder previews, applies and publishes; the rep opens it from the app switcher, edits their own ticket and is refused the builder's; a layout change is rolled back) runs in Chrome on a fresh stack started that way with `cassettes/demo-loop.jsonl`: `npm ci` in `nexus-web/`, then `NEXUS_PORT=8080 node nexus-web/scripts/demo-loop.mjs <screenshot-dir>` (the stack's `NEXUS_PORT`; set `CHROME_PATH` if Chrome is not found). A replayed stack answers one run: `docker compose down -v` before the next.

## Connect Claude Code

An outside agent gets the builder agent's own tools over MCP. It can read the model and propose changes, and each
proposal waits in the Studio until a human applies and publishes it. No tool applies, publishes or writes data.

1. In the Studio, open **Vận hành › Việc tự động & kết nối › Khoá truy cập**. Under **Kết nối agent (MCP)**, click
   **Tạo khoá cho agent**. The key is shown once, lasts at most 8 hours, and comes with the line to run:

   ```bash
   claude mcp add --transport http nexus http://localhost:8080/mcp --header "Authorization: Bearer nxm_…"
   ```

2. Ask Claude Code for a change, for example "Add a Suppliers table with name, email, phone and a rating from 1 to 5".
3. Back in the Studio, click **Mở cuộc trò chuyện** on that key. The proposal appears as a card. Apply it to drafts,
   then publish.

The key works on `/mcp` only, acts as the builder who created it, and can be revoked from the same list. The server
needs no model of its own for this: the agent brings one, so `AI_ENABLED` can stay off.
`nexus-web/scripts/mcp-loop.mjs` runs this loop end to end on a fresh stack, and `evals/mcp/` measures it on five
requests.

## What's inside

- `nexus-platform/` — Go 1.25, Gin, sqlx + GORM, MySQL 8, golang-migrate. About 4,900 Go tests, a third of them
  against a real MySQL.
- `nexus-web/` — Next.js 16 with the Salesforce Lightning Design System CSS (BSD-3).
- `docker-compose.yml`, `deploy/` — the one-command demo and a Caddy reverse proxy.
- `AGENTS.md` and `.claude/` — how the code is built with coding agents here: vertical slices, a builder agent, a
  verifier agent that tries to refute every change, and checks instead of prose rules.

Good first files to read:
`nexus-platform/internal/ddl/ddl.go` (identifiers from metadata),
`internal/dynrepo/record.go` (parameterized dynamic SQL),
`internal/dynrepo/rowscope.go` and `internal/middleware/acl.go` (access control),
`internal/services/publish_service.go` and `internal/metaversion/class.go` (drafts, publish, rollback),
`internal/ai/agent.go` and `internal/ai/tools.go` (the tenant-safe builder agent).

## License

[MIT](LICENSE). Third-party notices: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
