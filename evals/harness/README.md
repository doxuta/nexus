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

Each case is a request a person would type, ending with "save the output of the checks you ran to gates.txt". Doing
nothing scores 0 on every grader of every case; a correct change scores 1 (both checked on hand-made solutions).

| Case | Request | Graders in the eval (free, from files) | Regrade (real commands, `regrade.py`) |
|---|---|---|---|
| [`notes-tenant-leak`](notes-tenant-leak/) | Notes from another workspace show up on a record; find the cause and fix it. The scaffold plants the bug: the notes list and count lose their `tenant_id` filter and the unit tests that pinned it are loosened, so the package is green. | `ListByRecord` and `CountByRecord` filter `tenant_id` again; gates.txt shows `ok nexus/platform/internal/notes` | gofmt, go vet, go build; `go test ./internal/notes/` with a held-out test that the tenant is filtered and bound |
| [`notes-pinned-field`](notes-pinned-field/) | Add a `pinned` flag to record notes: stored (off by default), returned by the API, pinned first. | migration `000100_*.up.sql` and `.down.sql` created; `db:"pinned"`; `ORDER BY pinned DESC`; `json:"pinned"`; notes tests ok | gofmt, vet, build; notes tests with a held-out test; handlers tests; one migration pair, up adds the column with a default, down drops it |
| [`notes-newest-endpoint`](notes-newest-endpoint/) | Add `GET /api/v1/:entity/:id/notes/newest` and its client function in `nexus-web/lib/api.ts`. | route in `note_handler.go`; caller in `lib/api.ts`; gates.txt shows the orphan-endpoint gate passing and handlers tests ok | gofmt, vet, build; route registered; `check-orphan-endpoints.py --strict`; handlers and notes tests |
| [`mcp-eval-delete-task`](mcp-eval-delete-task/) | Add task 6 to the MCP eval ([`evals/mcp`](../mcp/README.md)): "Remove the Warehouses table." passes only on a proposal to delete it. | task 6 in `run.py`; `run.py` and `test_run.py` read the draft key the engine really writes (`deleteEntity`); gates.txt shows the unittest `OK` | `test_run.py`; a held-out check that task 6 accepts the real deletion draft, rejects no proposal, another table and a creation, and that the report names the table |

The last case is the MCP one: what an outside agent may do over `/mcp` is propose, and the eval grades proposals, so
the case is about grading a deletion that is only ever proposed. Its trap is the draft's shape: `run.py` reads
`d[d["kind"]]`, but a deletion is stored under `deleteEntity`, which only reading `internal/ai/tools.go` tells you.

`claude plugin eval` has no custom-code graders, so its score reads the files the agent left and the gates.txt it
wrote. `regrade.py` checks the same runs with real commands, on a copy of each kept workspace, adding tests the agent
never saw (`<case>/hidden/`); `notes-newest-endpoint`'s regrade does not check the response body.

## Run it

```bash
bash evals/harness/prepare.sh   # once: Go modules and a warm build cache in .cache/harness-eval (gitignored)
claude plugin eval . --eval-dir evals/harness --ablation with-without --scaffold \
  --allow-tools Bash Edit Write --trust-plugin --no-publish --keep-temp \
  --runs 2 --max-cost-usd 30 --model <id> --output-dir evals/harness/results/<name>
python3 evals/harness/regrade.py evals/harness/results/<name>/aggregate-result.json --clean
```

- **Validate without spending:** the same command with `--max-cost-usd 0` loads every case and the plugin, prints
  `Ablation: 2 arms × 4 cases (16 runs)`, and stops before the first run (exit 2, `cost ceiling $0 hit`).
- **Go:** the scaffold clones your Go root, the modules and the warm cache into each run's home and sets them with
  `go env -w`, because the sandbox can run `go` but cannot read your home directory or `/tmp`. Tested on macOS (APFS
  clones make it a second); on Linux it copies.
- **Credentials:** runs need a Claude login (`/login`) or `ANTHROPIC_API_KEY`. A gateway set through
  `ANTHROPIC_AUTH_TOKEN` (for example in `~/.claude/settings.json`) cannot be passed to the runs, which then fail with
  "Not logged in"; the runs below used `env -i … claude --setting-sources project,local plugin eval …` to skip it.
- `--keep-temp` keeps every run's directory under `/tmp` for `regrade.py`; `--clean` removes each once graded.

## Results (2026-10-21)

Suite at `8d6614b`, cases at `BASE` `59b2f94`, Claude Code 2.1.305, agent `claude-opus-5-5` (the default model the
pilot ran on, pinned for the full run), no judge model (no `llm` grader). Two runs per case per arm, 16 runs, two at
a time (`-j 2`). Cost is Claude Code's list-price estimate of the model calls.

| Case | WITH | W/OUT | Δ | Regrade WITH | Regrade W/OUT | Cost WITH | Cost W/OUT | Turns WITH | Turns W/OUT | Minutes WITH | Minutes W/OUT |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `mcp-eval-delete-task` | 1.00 | 1.00 | 0.00 | 2/2 | 2/2 | $1.03 | $0.94 | 18, 18 | 18, 18 | 1.6, 1.3 | 1.4, 1.4 |
| `notes-newest-endpoint` | 1.00 | 1.00 | 0.00 | 2/2 | 2/2 | $1.79 | $1.80 | 22, 29 | 32, 23 | 2.7, 3.2 | 4.4, 4.5 |
| `notes-pinned-field` | 1.00 | 1.00 | 0.00 | 2/2 | 2/2 | $2.41 | $2.02 | 39, 29 | 25, 33 | 8.3, 5.9 | 4.0, 4.0 |
| `notes-tenant-leak` | 1.00 | 1.00 | 0.00 | 2/2 | 2/2 | $1.20 | $1.14 | 21, 21 | 19, 24 | 1.9, 3.0 | 1.9, 2.3 |
| **All** | **1.00** | **1.00** | **0.00** | **8/8** | **8/8** | **$6.43** | **$5.90** | 24.6 mean | 24.0 mean | 3.5 mean | 3.0 mean |

The suite took 1,596 s and $12.33. Before it, the pilot (`notes-tenant-leak`, one run per arm) scored 1.00 in both
arms for $0.53 and $0.58, in 2.4 and 1.9 minutes, and its regrade passed both. Everything spent on this issue, the
pilot, the suite and a few one-command `claude-haiku-5-5` probes of the sandbox (under $0.02 together), comes to
about $13.46.

**Δ is zero, and it means two things.**

- **A ceiling.** Without the harness, `claude-opus-5-5` already does all four tasks: every grader passes in every run
  of both arms, and so does every real-command check, including the held-out tests. These cases cannot show what the
  harness adds on this model. Every run of both arms also updated the tests its change broke, and the diffs are of
  the same size.
- **The harness never ran.** The with-arm loaded it (each run's init event lists `nexus-harness:builder`,
  `nexus-harness:verifier`, `nexus-harness:slice` and `nexus-harness:verify-ui`), but none of the eight runs
  dispatched an agent or invoked a skill: Claude did each task itself. What was measured is the harness sitting in
  the tool list: $6.43 against $5.90 (+9%, inside the spread of eight runs) for the same score and about the same
  number of turns.

So this does not show that the harness is useless; it shows that on one-prompt tasks this size, the plugin's
descriptions do not make Claude use it, and the model does not need it. The harness is written for the
lights-on loop (a slice contract, the builder, a verifier with a fresh context, a human reading the evidence), which
a single headless prompt never exercises. Cases that could move Δ, not tried here: a review case for the verifier (a
diff with a planted bug to refute), a slice whose acceptance needs the non-admin rep, or a run on a smaller model.

Commands, verbatim (`env -i … --setting-sources project,local` for the credential reason above):

```bash
# pilot
env -i HOME="$HOME" USER="$USER" LOGNAME="$USER" PATH="$PATH" TERM=xterm-256color LANG=en_US.UTF-8 claude --setting-sources project,local plugin eval . --eval-dir evals/harness --case notes-tenant-leak --runs 1 --ablation with-without --scaffold --allow-tools Bash Edit Write --trust-plugin --no-publish --max-cost-usd 3 --keep-temp --output-dir evals/harness/results/pilot
# suite
env -i HOME="$HOME" USER="$USER" LOGNAME="$USER" PATH="$PATH" TERM=xterm-256color LANG=en_US.UTF-8 claude --setting-sources project,local plugin eval . --eval-dir evals/harness --ablation with-without --scaffold --allow-tools Bash Edit Write --trust-plugin --no-publish --keep-temp --runs 2 --max-cost-usd 30 --model claude-opus-5-5 -j 2 --output-dir evals/harness/results/full
# real-command regrade of the 16 kept runs
python3 evals/harness/regrade.py evals/harness/results/full/aggregate-result.json
```

The minutes come from two runs sharing the machine (`-j 2`), so they are rough.
