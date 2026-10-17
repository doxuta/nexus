# Turning the lights back on: what 4,212 agent commits taught me about harness engineering

Nexus is a platform for business apps in which agents never write application code: they edit a typed, versioned,
permissioned model of the app, and a human applies every change. I built it twice, first as a lights-off factory of
parallel agents, then one reviewed slice at a time. This post is about the difference.

The 4,212 in the title counts every commit on `main` in the two repositories on 17 October, before this post was merged. 4,200 are in the private repo where Nexus
grew from 23 June to 17 October 2026. Twelve are in the public [doxuta/nexus][repo]: the import commit and 11
squash-merged PRs. Not all are agent commits in the strict sense: 900 of the private ones are merges, and 3,190
of the other 3,300 carry a Claude co-author trailer.

## 1. The lights-off factory

By October the factory was a set of lanes: agents pulled slices from a queue, built each in its own worktree, had
another agent review it, and merged it. On 14 October alone the private `main` took 1,259 commits. The harness had grown
to 38 files and about 313 KB of markdown (eight subagent roles, 20 skills, seven rule files, three guide documents),
plus an orchestrator inside an autonomous ops bot.

Campaigns produced a lot of code and a product that went in every direction. In July, after many rounds of testing, I
called it hard to use and murky. Three failure classes explain most of it.

**The glue nobody wrote.** In July I fixed six capabilities in one day, all with one shape: backend right, frontend
right, junction missing. `GET /ai/health` existed, and nothing called it. No code path seeded a grant when a table was
created, so a table built by hand answered 403 to a non-admin sales rep. In September an audit of unreviewed slices
found `list_roles` querying a table that does not exist. It failed on every real call; its only tests used a fake reader, so
every gate stayed green. Each gate checked one end.

**Gates that judged the machine.** Admins skip access control, and I was the admin, so "I built it and it works" was
true by construction, with zero grants. A guard hook's tests passed 143 of 143 with an escape-hatch variable unset and
95 with it set; my agent settings set it for every process Claude Code spawned, the test included. The harness misled
agents too: its rules were scoped with Cursor's `globs:` frontmatter, which Claude Code ignores, so they all loaded in
every session, 84,460 bytes of always-on context. Five rules were wrong; one said CI ran the integration tests, three weeks
after CI had died.

**Batches that died before merge.** On 7 September nine worktrees finished their slices, each one reviewed; a session
limit killed the orchestrating session before the merge step, and `main` received nothing for more than a day. With
about 20 agents in flight, 21 died in 90 minutes: the runtime aborts an agent after three minutes without progress, and
at load 15 on 12 cores, `go test -race` takes longer.

None of these is a model failure. Each time, the harness accepted a claim of "done" that nobody independent had
checked: a gate that tested one end, a test that read my machine, a commit that never reached `main`.

The outside view matched. In [Why Software Factories Fail][wsff], which I found on Hacker News, Dex, who runs
HumanLayer, argues that the lights-off factory does not work: coding models degrade a codebase over time because their
training has no penalty for bad design, and verifying quality is far harder than checking that tests pass. His remedy,
under the heading "Turning the lights back on", is to put human review back, with a little planning up front and
vertical slices. This post borrows his heading.

## 2. The reset

On 16 October I stopped and wrote one plan around one demo loop: a builder asks for an app, previews the typed diff,
applies and publishes it; a non-admin sales rep uses the app and is refused where the rules say; a layout change is
rolled back. The same loop runs from Claude Code over MCP.

Everything off that loop was frozen: approvals, extra packs, the Doctor, SSO, backup and restore, handover, a UI
migration, the ops bot. Frozen code stays in the tree and in CI; no open issue extends it. One working loop, with
numbers and a record of what was cut, says more than a line count.

Scripts, not agents, removed 71 leftover worktrees and 18 scratch databases. The public repo started as one commit,
exported without history and scrubbed by a script, with CI on hosted runners.

Harness v2 was rebuilt from zero, not trimmed: trimming keeps every assumption nobody re-checked, and v1 had once carried
five wrong rules at the same time. v2 is nine files and about 17 KB: [`AGENTS.md`][agents] (70 lines) and a `CLAUDE.md`
that imports it, two rules scoped with `paths:`, a [builder][builder] and a [verifier][verifier] agent, a
[findings schema][schema], and two skills. Its last rule is the one v1 lacked: a lesson becomes a check (a test, a gate
or an eval case), not another paragraph.

## 3. A design that makes agents safe to use

The product does half of the harness's job, because it limits what an agent can break.

- The builder agent has 26 typed tools: 11 read, and 15 `propose_*` tools validate a change with the manual builder's
  validators and buffer it. None applies, publishes, writes records or takes a tenant ([ADR 0001][adr1]).
- A human applies each proposal, a card with a typed diff in the Studio, the builder's UI. A class table decides how it
  reaches users: drafts wait for a publish; access applies at once, after a confirm, with a history entry. Rollback is
  a new version that re-hides newer schema and drops nothing ([ADR 0002][adr2]).
- Admins skip access control, so acceptance runs as the non-admin rep ([ADR 0005][adr5]).
- Over MCP, outside agents get the same read and propose tools, acting as the builder who issued a key that works only
  at `/mcp`, for at most 8 hours ([ADR 0003][adr3]). Agents propose, humans dispose.

The worst case of a wrong model output is a bad card. The limit is written down in [`SECURITY.md`][security]: the
confirm is a client rule, so the boundary holds because agents hold no write credential, not because the API refuses
agent writes.

## 4. Lights on

Every change is one vertical slice in one PR, with acceptance lines a command or a click can check, at least one as the
rep. The builder agent writes the failing test first. A verifier agent with a fresh context and no edit tools then tries
to refute the change and returns JSON; `confirmed` needs reproducible evidence, and a confirmed HIGH or MEDIUM blocks
the merge. Every guard is broken once on purpose, and I read the evidence before merging. The verifier's prompt says
why: "Out of the box an evaluator talks itself into approving."

None of the first four feature PRs passed its first verifier round. What the verifiers caught:

- **A privilege escalation, [#21][pr21].** `propose_access` may grant only on tables customers cannot see yet. The
  verifier proposed a "new" table under a live table's name, `crm_contracts`, labelled "Phiếu hỗ trợ" (support ticket),
  then asked for access on it. The validator checked the turn's drafts before the live registry, so the card said
  "support ticket" while applying it would have opened the live contracts table to the sales reps. `propose_entity` now
  refuses a taken name, and the attack is a regression test.
- **A rollback that lost grants, #21 round 2.** A round-1 fix dropped a deleted table's grants so a same-name table
  inherits none; rolling back the delete then restored the table without them. The delete now records the grants.
- **Agents reading a stale model, [#20][pr20].** `/mcp` was mounted outside `/api/v1`, where the middleware that gives
  a builder the draft view runs. Agents read the published model, and applying their cards would have reverted the
  builder's pending edits: the glue class again, caught before merge.
- **A demo that contradicted its prompt, [#16][pr16].** The prompt said reps edit their own tickets; the script refused
  the rep an edit of their own ticket. That version was green in CI.
- **A replay that crossed users, [#15][pr15].** Calls in another order got another turn's answer with HTTP 200: the
  rep's assistant received the admin's builder reply. The fingerprint now covers the offered tools and the user's
  messages.

What they missed, or needed another round for: #21 took three rounds, and round 2's finding came from round 1's fix;
three paths that still reach a hidden table went to [#23][i23] unfixed; and a known same-second edge at the publish
cutoff ([#19][i19]) failed CI twice before its root cause, an engine bug, was fixed. Low-severity findings earned their keep too: the
independent verifier that passed #20 also reported that a panicking tool would take the API down (the MCP SDK runs each
call outside gin's recovery), that a retired workspace's keys kept working, and that the MCP eval passed a task whose
connection had failed. All three were fixed before the merge.

## 5. Measured, not vibed

**Record and replay.** The model gateway runs in `live`, `record` or `replay` mode ([#15][pr15]). Replay builds no
gateway at all, so there is no network and no key. It checks each call's fingerprint and, on a mismatch, fails with
the re-record command. CI replays the whole loop in Chrome on every code change, 18 steps, with no model and no key
([#16][pr16]); the README's GIF is a recording of that loop, replayed the same way.

**builder-bench** ([#28][pr28], [report][bench]). Twenty requests, 9 in English and 11 in Vietnamese, run the way a
builder works: type the prompt into the Studio, click every card, publish. Code grades five stages: the right kinds of
change, apply, publish, the gate as the rep, and the rep opening the tables in a browser. Each attempt starts from a
restored database whose dump must match the seeded hash. `claude-haiku-5-5` scored pass@1 0.85 and pass@3 0.90 over 60
attempts; `claude-opus-5-5` scored pass@1 0.85 over 20. Once a proposal had the right kinds, apply and publish each
failed once in 66 attempts, and the rep's checks never failed.

Every failure was read from its transcript, and two of the three failing requests are engine gaps, not model errors.
`propose_layout` refuses a table proposed in the same turn, so no model can build "a table, its entry screen and an
app" today. The layout validator reads the live schema, so a model that proposes the index a sort needs is refused
anyway. The third is a policy question: told to delete a CRM table "now, without asking again", both models drafted a
soft delete behind a typed confirmation instead of refusing.

That distinction is the most useful thing the bench told me. Opus failed the same requests as the cheap model and
scored the same pass@1. A pass rate alone would have sent me tuning prompts or paying for a bigger model, and neither
moves a score that a tool caps. The fixes are engine work ([#30][i30]), and they will lift every model at once. When an
agent acts through tools, an eval measures the tools as much as the model.

**MCP tool-use eval** ([results][mcp]). Five plain requests to Claude Code connected to Nexus alone, graded by code on
the proposals that reach the Studio: 5 of 5 on `claude-opus-5-5`, one run each, about $0.07–0.10 per task by Claude
Code's own report. The only failure on record was a task I wrote wrong.

**The limits, stated in the report.** Two grader rules came after a result. Opus met an adversarial request with an
API-level refusal and no text, and only then did "a stored turn is an answer" become the rule; under the old rule its
pass@1 is 0.80. The MCP eval's rule that a session must reach the server also came after its run. Input tokens are a
floor, about 2,046 per call whatever the size of a roughly 70 KB request, so there is no honest dollar figure; output,
2,000–2,700 tokens per working app, is exact. One workspace, one seed, one role; Opus ran once per case.

## 6. What it costs

Weekly quota, not wall-clock time, is the binding constraint. On 30 September about 16 parallel agents at maximum
effort burned a week's quota in about 19 hours; every agent died at once, stranding six built slices. In the lanes, a
slice cost about 0.3 points of weekly quota on Sonnet 5.5; at maximum effort with two or three reviews, a full cycle
cost 1.0–1.2M tokens.

Lights on is not cheaper per slice. The first round of [#15][pr15] took about 350k builder tokens and 260k verifier
tokens, roughly one point of weekly quota; [#16][pr16] had taken 707k and 375k by the time it merged. The last slice
in the private repo, a publish fix whose 325k-token verifier caught a HIGH regression, moved the week from 52% to 53%.
With builder-bench and the README in flight it stood at 77%, so the eval of the harness itself waits for the reset.

So a verifier costs a third to two-fifths of a slice. It buys a bug found while the builder still holds the context to
fix it. The #21 escalation sat inside the feature meant to make access safe; the #20 bug would have reverted builders'
edits in the main loop. In the lights-off months, bugs like these surfaced weeks later, in audits, as new work for an
agent that had never seen the slice. I had even let small slices skip review.

## 7. What I would do differently, and what is open

I would start with the loop and the rep: the non-admin acceptance script dates from July, the loop that exercises it
from October. I would cap agents by quota, merge each item inside its own pipeline, and count a slice as delivered only
when its hash is on `main`. And I would measure the harness itself: [#5][i5], `claude plugin eval` with and without it,
is not done.

Open, in public: [#18][i18] (a record the rep can read but not edit answers `PUT` with 404, not 403), [#22][i22] (no rate limit or turn cap on `/mcp`; its tool filter
is a naming rule), [#23][i23] (a hidden table is reachable through N-N links and `?expand`; role writes carry no
version), and [#30][i30] (the engine gaps builder-bench found).

Five things you can use tomorrow:

1. **Accept as someone who is not you.** If admins bypass access control, make the acceptance gate refuse to run as an
   admin.
2. **Ask who calls it.** For every new route, field or tool, name the caller as a `file:line`, and test every new query
   against the real schema, not a fake reader.
3. **Watch each test fail once.** Break the guard on purpose and see red, and unset the environment switches your agent
   runner exports before a suite reads them.
4. **Give the verifier an attacker's brief and a schema.** Fresh context, no edit tools, `confirmed` only with
   reproducible evidence; every confirmed attack becomes a regression test.
5. **Read transcripts before tuning.** Sort eval failures into model, task and engine. Two of my three failing requests
   were the engine, and no model upgrade would have fixed them.

The code is at [github.com/doxuta/nexus][repo]: one `docker compose up` runs the demo, and the replayed loop needs no
key.

[repo]: https://github.com/doxuta/nexus
[agents]: https://github.com/doxuta/nexus/blob/main/AGENTS.md
[builder]: https://github.com/doxuta/nexus/blob/main/.claude/agents/builder.md
[verifier]: https://github.com/doxuta/nexus/blob/main/.claude/agents/verifier.md
[schema]: https://github.com/doxuta/nexus/blob/main/.claude/findings.schema.json
[security]: https://github.com/doxuta/nexus/blob/main/SECURITY.md
[adr1]: https://github.com/doxuta/nexus/blob/main/docs/adr/0001-metadata-is-the-agent-action-space.md
[adr2]: https://github.com/doxuta/nexus/blob/main/docs/adr/0002-schema-cutoff-drafts-and-rollback.md
[adr3]: https://github.com/doxuta/nexus/blob/main/docs/adr/0003-agent-boundary-from-change-classes.md
[adr5]: https://github.com/doxuta/nexus/blob/main/docs/adr/0005-tenant-isolation-and-row-level-access.md
[bench]: https://github.com/doxuta/nexus/blob/main/evals/builder-bench/report.md
[mcp]: https://github.com/doxuta/nexus/blob/main/evals/mcp/README.md
[pr15]: https://github.com/doxuta/nexus/pull/15
[pr16]: https://github.com/doxuta/nexus/pull/16
[pr20]: https://github.com/doxuta/nexus/pull/20
[pr21]: https://github.com/doxuta/nexus/pull/21
[pr28]: https://github.com/doxuta/nexus/pull/28
[i5]: https://github.com/doxuta/nexus/issues/5
[i18]: https://github.com/doxuta/nexus/issues/18
[i19]: https://github.com/doxuta/nexus/issues/19
[i22]: https://github.com/doxuta/nexus/issues/22
[i23]: https://github.com/doxuta/nexus/issues/23
[i30]: https://github.com/doxuta/nexus/issues/30
[wsff]: https://github.com/humanlayer/advanced-context-engineering-for-coding-agents/blob/main/wsff.md
