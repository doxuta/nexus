# Harness eval: the `.claude/` harness, with and without

Does the agent harness in `.claude/` (the `builder` and `verifier` agents, the `slice` and `verify-ui` skills) make
Claude Code better at small, real changes to this repo? Issue #5. `claude plugin eval` runs each case twice: with the
harness loaded as a plugin (WITH) and with no plugin (W/OUT); Δ is the difference. Code graders only, no model
grades another one.

## What is measured

`.claude/.claude-plugin/plugin.json` makes `.claude/` a plugin directory named `nexus-harness`, so its `agents/` and
`skills/` load as the plugin's components. (A skills-directory plugin in the documented sense lives at
`.claude/skills/<name>/`; moving the agents there would rename them for every session, so the plugin directory is
`.claude/` itself, which `claude plugin eval` takes as a path.) Each case points at it with `plugins:`.

Both arms get the same thing otherwise: a git checkout of the repo at a pinned commit (`BASE` in
[`lib/common.sh`](lib/common.sh)) **without** `.claude/` and `CLAUDE.md`, the same prompt, the same tools (Read, Glob,
Grep, Bash, Edit, Write, Skill, Agent, TodoWrite), and the same note appended to the system prompt about the sandbox.
So the comparison is the plugin and nothing else, with these limits:

- `AGENTS.md` is in the checkout of both arms as a file. Neither arm loads it on its own: eval runs never load a
  `CLAUDE.md` (which is `@AGENTS.md` here), so an agent reads it only if it goes looking.
- `.claude/rules/` (path-scoped rules) is not a plugin component, so it is in neither arm.
- The agent runs in Claude Code's sandbox: no network, no local ports (tests that start an `httptest` server fail),
  no MySQL, and on the Mac these runs used, no `git` (Apple's `/usr/bin/git` shim cannot find its tools there).

## Cases

Each case is a request a person would type, ending with "save the output of the checks you ran to gates.txt".

| Case | Request | Graders in the eval (free, read files) | Regrade (`regrade.py`, runs the code) |
|---|---|---|---|
| [`notes-tenant-leak`](notes-tenant-leak/) | Notes from another workspace show up on a record; find the cause and fix it. The scaffold plants the bug: the notes list and count lose their `tenant_id` filter and the unit tests that pinned it are loosened, so the package is green. | `ListByRecord` and `CountByRecord` filter `tenant_id` again; gates.txt shows `ok nexus/platform/internal/notes` | gofmt, go vet, go build; a hidden test calls both and wants a top-level `tenant_id = ?` term whose placeholder binds the tenant, anywhere in the clause, and no top-level `OR` |
| [`notes-pinned-field`](notes-pinned-field/) | Add a `pinned` flag to record notes: stored (off by default), returned by the API, pinned first. | migration `000100_*.up.sql` and `.down.sql` created; `db:"pinned"`; `ORDER BY pinned DESC`; `json:"pinned"`; notes tests ok | gofmt, vet, build; hidden tests: an older pinned note is listed before a newer unpinned one and comes back pinned, and `GET /:entity/:id/notes` returns `"pinned": true`; one migration pair, up adds the column with a default and down drops it, comments ignored |
| [`notes-newest-endpoint`](notes-newest-endpoint/) | Add `GET /api/v1/:entity/:id/notes/newest` and its client function in `nexus-web/lib/api.ts`. | route in `note_handler.go`; GET call in `lib/api.ts`; gates.txt shows the orphan-endpoint gate passing and handlers tests ok | gofmt, vet, build; the orphan-endpoint gate (script from BASE); an exported function in `lib/api.ts` that GETs `` `/${…}/${…}/notes/newest` ``, comments ignored; a hidden test over a table that also holds newer notes the query must leave out (another tenant on the same record id, another record of the same tenant, a soft-deleted note), with rows stored out of order and the right note holding the smallest id: `{"data": null}` with none of the caller's live notes, the caller's newest live note otherwise, and 404 on a record the parent-record gate hides |
| [`mcp-eval-delete-task`](mcp-eval-delete-task/) | Add task 6 to the MCP eval ([`evals/mcp`](../mcp/README.md)): "Remove the Warehouses table." passes only on a proposal to delete it. | task 6 in `run.py`; `run.py` and `test_run.py` read the draft key the engine really writes (`deleteEntity`); gates.txt shows the unittest `OK` | BASE's `test_run.py` against the agent's `run.py`; a hidden check that task 6 accepts the real deletion draft, rejects no proposal, another table and a creation, and that the report names the table |

The last case is the MCP one: what an outside agent may do over `/mcp` is propose, and the eval grades proposals, so
the case is about grading a deletion that is only ever proposed. Its trap is the draft's shape: `run.py` reads
`d[d["kind"]]`, but a deletion is stored under `deleteEntity`, which only reading `internal/ai/tools.go` tells you.

`claude plugin eval` has no custom-code graders, so its score reads the files the agent left and the gates.txt it
wrote: a comment can pass it. The regrade is the check that counts. It copies each kept workspace, overwrites the gate
scripts and test files it relies on with BASE's, and adds tests the agent never saw (`<case>/hidden/`,
[`lib/hidden/`](lib/hidden/)). Those tests carry their own SQL driver, which answers as the statement asks once its
SQL comments are removed (its top-level `col = ?`, `? = col`, `col IN (?)` and `col IS NULL` terms, parenthesised AND
groups included, its ORDER BY on plain or table-qualified columns, its LIMIT), and serve the notes routes through the
real
`RegisterAPIRoutes` over the real `notes.Service` and a parent-record gate, so they call whatever the agent wrote,
wherever it mounted it. They run with `-run '^TestHidden_' -v`, and each must print its own `--- PASS` line: a
`TestMain` that skips the tests does not get through. Other correct ways to write the query, such as a subquery
(`created_at = (SELECT MAX(created_at) …)`) or `tenant_id IN (?, ?)`, are not understood and grade as failures:
re-check a failing run by hand before counting it.

**The regrade runs code the agent wrote** (its Go code and tests, its `run.py`) as you, outside any sandbox. Grade only
runs you produced, or run it in a container.

[`selftest.py`](selftest.py) proves both kinds of grader without a model, on hand-written variants of each case:

| Variant | Eval graders | Regrade |
|---|---|---|
| nothing (the untouched workspace) | 0.00 in every case | fails in every case |
| gaming (comments that match the eval graders, a fake gates.txt) | 1.00 in every case | fails in every case |
| wrong but plausible: `leak-or` (`WHERE tenant_id = ? OR …`), `leak-sqlcomment` (`-- AND tenant_id = ?` in the SQL, its value passed), `leak-testmain` (the leak stays, a `TestMain` skips the tests), `pin-dto-never-set` (the response field is declared, never set), `pin-sqlcomment` (`ORDER BY /* pinned DESC, */ …`), `newest-wrong-handler` (the route serves the whole list), `newest-no-tenant`, `newest-no-record`, `newest-no-deleted` (a query without the tenant, the record or `deleted_at IS NULL`), `newest-no-guard` (a service method that skips the parent-record gate), `newest-raw-entity` (notes read under the entity name from the URL, not the one the gate resolved), `newest-oldest` (`ORDER BY created_at ASC`), `newest-order-id` (`ORDER BY id DESC`), `newest-fe-wrong-path` (the client GETs `/${entity}/notes/newest`) | 1.00 (0.33 for `leak-testmain`, 0.83 for `pin-sqlcomment`, 0.75 for `newest-fe-wrong-path`) | fails |
| reference (a correct change; also: for the leak the tenant last, `leak-reorder`, and a parenthesised AND group, `leak-parens`; for the flag `ORDER BY n.pinned DESC` through a table alias, `pin-alias`; for the endpoint a handler-only one, one with a new service method, one with its own correct query) | 1.00 | passes |

## Run it

```bash
bash evals/harness/prepare.sh   # once: Go modules and a warm build cache in .cache/harness-eval (gitignored)
python3 evals/harness/selftest.py   # no model: the 31 variants above, about 11 minutes
claude plugin eval . --eval-dir evals/harness --ablation with-without --scaffold \
  --allow-tools Bash Edit Write --trust-plugin --no-publish --keep-temp \
  --runs 2 --max-cost-usd 30 --model <id> --output-dir evals/harness/results/<name>
python3 evals/harness/regrade.py evals/harness/results/<name>/aggregate-result.json --archive evals/harness/runs/<date> --clean
python3 evals/harness/regrade.py --from-archive evals/harness/runs/<date>   # later: re-grade from the diffs, no model
```

- **Validate without spending:** the eval command with `--max-cost-usd 0` loads every case and the plugin, prints
  `Ablation: 2 arms × 4 cases (16 runs)`, and stops before the first run (exit 2, `cost ceiling $0 hit`).
- **Go:** `prepare.sh` takes the modules from your module cache, else from proxy.golang.org. The scaffold clones your
  Go root, the modules and the warm cache into each run's home and sets them with `go env -w`, because the sandbox
  can run `go` but cannot read your home directory or `/tmp`. Tested on macOS (APFS clones make it a second); on
  Linux it copies.
- **Credentials:** runs need a Claude login (`/login`) or `ANTHROPIC_API_KEY`. A gateway set through
  `ANTHROPIC_AUTH_TOKEN` (for example in `~/.claude/settings.json`) cannot be passed to the runs, which then fail with
  "Not logged in"; the runs below used `env -i … claude --setting-sources project,local plugin eval …` to skip it.
- `--keep-temp` keeps every run's directory under `/tmp` for `regrade.py`. `--archive` writes a small, redacted record
  of every run for committing (summary, one diff per run, the regrade log); `--clean` removes the kept directories.

## Results (2026-10-21)

The second run of the suite, at `e84f541` (the hardened regrade), cases at `BASE` `59b2f94`, Claude Code 2.1.305,
agent `claude-opus-5-5` (the default model the pilot ran on, pinned), no judge model. Two runs per case per arm, 16
runs, two at a time (`-j 2`). Cost is Claude Code's list-price estimate. Every run is archived in
[`runs/2026-10-21/`](runs/2026-10-21/): `summary.json`, one diff per run, `regrade.txt`. The regrade columns are the
current regrade (after verifier rounds 2 to 4: a driver that honours the query, traps for every term it needs,
the parent-record gate and its canonical entity name, `--- PASS` lines) applied to the archive with `regrade.py --from-archive`, which rebuilds each
run from its scaffold and its diff; they match what the round-1 regrade gave on the live runs.

| Case | Eval WITH | Eval W/OUT | Eval Δ | Regrade WITH | Regrade W/OUT | Cost WITH | Cost W/OUT | Turns WITH | Turns W/OUT | Minutes WITH | Minutes W/OUT |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `mcp-eval-delete-task` | 1.00 | 1.00 | 0.00 | 2/2 | 2/2 | $1.00 | $0.92 | 18, 16 | 16, 15 | 2.0, 1.6 | 1.2, 1.1 |
| `notes-newest-endpoint` | 1.00 | 0.88 | +0.13 | 2/2 | 2/2 | $1.83 | $1.92 | 24, 24 | 30, 30 | 3.8, 11.2 | 5.7, 5.6 |
| `notes-pinned-field` | 1.00 | 1.00 | 0.00 | 2/2 | 1/2 | $1.97 | $2.19 | 37, 21 | 32, 34 | 11.5, 3.6 | 4.5, 3.5 |
| `notes-tenant-leak` | 1.00 | 1.00 | 0.00 | 2/2 | 2/2 | $1.55 | $1.43 | 28, 22 | 22, 24 | 7.2, 2.3 | 5.2, 2.8 |
| **All** | **1.00** | **0.97** | **+0.03** | **8/8 (1.00)** | **7/8 (0.88)** | **$6.35** | **$6.45** | 23.8 mean | 25.4 mean | 5.4 mean | 3.7 mean |

The suite took 2,203 s and $12.80. Both nonzero deltas come from one run each, and neither is the harness:

- **Eval, `notes-newest-endpoint` W/OUT run 1 (0.75):** a grader false negative. The run wrote
  `` `/${encodeURIComponent(entity)}/${encodeURIComponent(recordId)}/notes/newest` ``, which the `web-caller` pattern
  (`\$\{\w+\}`) did not match; the regrade passed it. The pattern now takes any `${…}`; the scores above are the
  run's, under the old one.
- **Regrade, `notes-pinned-field` W/OUT run 2:** a real failure. The change works, every hidden test passes, but it
  added an integration test that `gofmt -l .` flags (`internal/handlers/note_integration_test.go`).
- **The harness never ran.** No with-arm run dispatched `nexus-harness:builder` or `:verifier` or invoked a skill
  (`agentsInvoked` and `skillsInvoked` are empty in all 16 rows of `summary.json`), and it did not run the gates
  more. (Not archived: over the 8 runs of each arm, `gofmt` 9 times against 12, `go vet` 13 against 14, `go test`
  36 against 36, the orphan-endpoint gate 7 against 13, counted from the transcripts before `--clean` removed them;
  this archive cannot re-check those counts, later ones carry them as `gateCommands`.) So the with-arm differs only
  by the plugin's descriptions in its context, and one failed run in eight is inside the noise of two runs per case.

**What it means.** On these four tasks `claude-opus-5-5` does the work without the harness, and with the harness
loaded it does not use it. Δ measures the harness's presence in the tool list, which is zero within the noise of 16
runs. It does not show the harness is useless: the harness is written for the lights-on loop (a slice contract, the
builder, a verifier with a fresh context, a human reading the evidence), which a single headless prompt never
exercises. Cases that could move Δ, not tried here: a review case for the verifier (a diff with a planted bug to
refute), a slice whose acceptance needs the non-admin rep, a run on a smaller model.

**Earlier runs.** The pilot (`notes-tenant-leak`, one run per arm, `--max-cost-usd 3`) scored 1.00 in both arms for
$0.53 and $0.58. The first full run (at `8d6614b`, $12.33) scored 1.00 in both arms on every case, and its regrade,
the text-matching one that the verifier then broke with two comment lines, passed all 16. Its runs were not archived.
Spent on this issue in all: the pilot $1.11, the two full runs $12.33 and $12.80, and a few one-command
`claude-haiku-5-5` probes of the sandbox (under $0.02), about **$26.26**.

Commands, verbatim (`env -i … --setting-sources project,local` for the credential reason above):

```bash
# pilot
env -i HOME="$HOME" USER="$USER" LOGNAME="$USER" PATH="$PATH" TERM=xterm-256color LANG=en_US.UTF-8 claude --setting-sources project,local plugin eval . --eval-dir evals/harness --case notes-tenant-leak --runs 1 --ablation with-without --scaffold --allow-tools Bash Edit Write --trust-plugin --no-publish --max-cost-usd 3 --keep-temp --output-dir evals/harness/results/pilot
# suite (both full runs; the second wrote to results/full2)
env -i HOME="$HOME" USER="$USER" LOGNAME="$USER" PATH="$PATH" TERM=xterm-256color LANG=en_US.UTF-8 claude --setting-sources project,local plugin eval . --eval-dir evals/harness --ablation with-without --scaffold --allow-tools Bash Edit Write --trust-plugin --no-publish --keep-temp --runs 2 --max-cost-usd 30 --model claude-opus-5-5 -j 2 --output-dir evals/harness/results/full2
# regrade of the 16 kept runs, archived
python3 evals/harness/regrade.py evals/harness/results/full2/aggregate-result.json --archive evals/harness/runs/2026-10-21 --clean
# re-grade after verifier rounds 2 to 4, from the archive (no model)
python3 evals/harness/regrade.py --from-archive evals/harness/runs/2026-10-21
```

The minutes come from two runs sharing the machine (`-j 2`), so they are rough.
