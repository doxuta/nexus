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
  no MySQL, and on macOS no `git`.

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
  `ANTHROPIC_AUTH_TOKEN` (here, in `~/.claude/settings.json`) cannot be passed to the runs, which then fail with
  "Not logged in"; the runs below used `env -i … claude --setting-sources project,local plugin eval …` to skip it.
- `--keep-temp` keeps every run's directory under `/tmp` for `regrade.py`; `--clean` removes each once graded.
