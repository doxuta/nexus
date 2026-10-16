# AGENTS.md — working on Nexus

Nexus is an agent-native business-app platform. **Agents never write application code: they edit a typed,
versioned, permissioned model** (entities, fields, relations, layouts, workflows, access), and the engine — written
and maintained by humans — runs it. Every AI change is a reviewable diff, lands as a draft, is published by a human,
can be rolled back, and is enforced per tenant and per row for the people who use the app.

## Map

- `nexus-platform/` — Go 1.25 · Gin · sqlx for dynamic records, GORM for system tables · MySQL 8 · golang-migrate.
  Layers: `internal/handlers` → `internal/services` → repositories (`internal/metadata`, `internal/dynrepo`); domain
  types live in the package that owns them (there is no shared `entities` package).
  - `internal/ddl` — identifier allowlist and DDL for tables born from metadata.
  - `internal/dynrepo` — parameterized dynamic SQL, row-level scope (`rowscope.go`).
  - `internal/middleware/acl.go` + `internal/services/acl_service.go` — access control (admins bypass it).
  - `internal/metaversion`, `internal/services/draft_gate*.go`, `publish_*.go` — drafts, publish, rollback.
  - `internal/ai` — the builder agent loop and its tenant-safe tools (`tools.go`), the end-user assistant.
  - `internal/services/vertical_pack` — installable packs (CRM, ticketing, LMS…).
- `nexus-web/` — Next.js 16 App Router + Salesforce Lightning Design System CSS (BSD-3); Tailwind only for layout.
  New UI strings go through `lib/i18n` (Vietnamese first, English partial; older screens still hold literals).
  Read `nexus-web/AGENTS.md` first.
- `docker-compose.yml`, `deploy/Caddyfile.local`, `nexus-platform/scripts/seed-demo.py` — the one-command demo.
- `nexus-platform/scripts/acceptance-*.py` — end-to-end checks against a running server, judged as a non-admin.

## Invariants — never break these

1. **Metadata-first.** Custom entities and fields live in `sys_entities` / `sys_fields` and are read at runtime.
   Never hardcode one as a Go struct.
2. **Tenant isolation.** Every query on business data filters `tenant_id`. No exceptions, admin paths included.
3. **Parameterized SQL.** Values are always `?` parameters; dynamic table/column names only through `internal/ddl`.
4. **Layers and errors.** No SQL in handlers, no HTTP in services. Wrap every error `fmt.Errorf("pkg.method: %w", err)`;
   never discard one.
5. **Done means a third person can use it.** Admins bypass ACL, so "it works for me" proves nothing: anything that
   touches entities, access or apps is accepted as the non-admin sales rep (`scripts/acceptance-acl.py`).

Migrations: up and down, numbered sequentially, never edit one that has been applied.

## Gates — run before every commit

```bash
# nexus-platform/
gofmt -l . ; go vet ./... ; go test ./... ; go build ./cmd/server/
make test-integration                      # real MySQL; fails on an all-SKIP run
python3 scripts/check-orphan-endpoints.py --strict   # a new route nobody calls is wired or justified
python3 scripts/acceptance-acl.py --expect <entity>  # the rep can actually use what you built
# nexus-web/
npm run build && npx tsc --noEmit && npx vitest run && npx eslint .
```

## How work is done here — lights on

Agent-written code degrades a codebase when nobody steers it ("software factories fail"). So:

- **One vertical slice per PR.** Start from a contract: the problem, the acceptance criteria (at least one checked
  as the rep), and the evidence you will show. Build middle-out — API contract, then UI against it, then services,
  then the database — so the slice can be touched end to end early. Skill: `slice`.
- **Builder, then verifier.** The `builder` agent implements one slice with tests red first. The `verifier` agent,
  with a fresh context, tries to refute it and returns JSON per `.claude/findings.schema.json`; a confirmed HIGH or
  MEDIUM finding blocks the merge. A human reads the evidence before merging.
- **Prove the test.** Break the fix on purpose once, watch the test go red, restore it.
- **UI changes are clicked, not assumed.** Skill: `verify-ui` (screenshots + a scripted click-through as the rep).
- **A lesson becomes a check** — a test, a gate or an eval case — not another paragraph in this file.

## Don'ts

- Don't commit `.env`, keys or real data; don't force-push `main`.
- Don't point tests at a shared database: integration tests create throwaway databases (`internal/ittest`).
- Kill only processes you started; never `pkill -f` a pattern that can match someone else's server.
- Don't add a dependency, an abstraction or a config knob nobody asked for. The shortest correct diff wins.
