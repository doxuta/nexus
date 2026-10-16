# MCP tool-use eval

Five plain requests to an outside agent connected over MCP (issue #3). Each must end in the right proposal, and only
a proposal. Code graders read the proposals that the task's Studio conversation holds (their kinds and names). No
model grades another one. This is a smoke-sized eval; the real bench is #4.

```bash
# a running one-command demo, a logged-in Claude Code
python3 evals/mcp/run.py --base http://localhost:8080 --out /tmp/mcp-eval -- --model opus
```

For each task, the runner:

1. issues an agent key as the builder, which opens a conversation of its own;
2. runs one headless `claude -p` session with only the `nexus` server (`--strict-mcp-config --allowedTools mcp__nexus`);
3. revokes the key;
4. grades the conversation's proposals.

A session that never reached the server fails, whatever it answered: it needs at least one `nexus_` tool call, or
the `nexus` server connected in its init event. A failed connection still exits 0, and task 5 wants no proposal, so
without this rule it would pass. `python3 evals/mcp/test_run.py` (in CI) checks the pass rule on a recorded
transcript and on a synthetic failed connection.

Nothing is applied, so the workspace is unchanged. `--out` keeps each stream-json transcript with every `nxm_` token
redacted, plus `results.json`. The exit code is 1 when a task fails.

| # | Request | Passes when the conversation holds |
|---|---|---|
| 1 | Add a Warehouses table with a name, an address and a capacity (a number). | an `entity` named `*warehouse*` with at least 3 fields |
| 2 | Add a LinkedIn profile URL field to the CRM contacts table. | a `field` on `crm_contacts` named `*linkedin*` |
| 3 | Link CRM products and CRM contracts many-to-many, so a contract can list the products it covers. | a `relationship` between `crm_products` and `crm_contracts` |
| 4 | Propose a list layout for CRM leads that shows the name, the company and the status, newest first. | a `layout` of `crm_leads`, view `list` |
| 5 | Who can edit contacts? | no proposal at all |

## Results (2026-10-17)

Each task ran once, headless, against the `docker compose` demo the end-to-end run had just used (its Suppliers
table included). The model was `claude-opus-5-5` (Claude Code 2.1.292, `--model opus`). Input tokens include the
cached part, shown in brackets. Claude Code reports about $0.07–0.10 per task.

| # | Proposals in the conversation | Tool calls | Input tokens (cached) | Output tokens | Result |
|---|---|---|---|---|---|
| 1 | `entity:warehouses` | list_entities, propose_entity | 155,583 (149,117) | 561 | pass |
| 2 | `field:linkedin_url` | list_entities, list_fields, propose_field | 208,296 (201,405) | 840 | pass |
| 3 | `relationship:crm_contracts_products` | list_entities, list_relationships, propose_relationship | 157,223 (149,989) | 471 | pass |
| 4 | `layout:crm_leads/list` | list_entities, list_fields, get_layout, propose_layout | 208,661 (201,623) | 772 | pass |
| 5 | none | list_entities, list_roles, explain_access | 155,599 (149,237) | 636 | pass |

**5/5 pass.** The rule that a session must reach the server came after this run; the five recorded sessions all
connected and called `nexus_` tools, so the result stands (checked on the transcripts, not re-run). Notes:

- **Task 3 was rewritten.** Its first version asked to link contacts and opportunities. The CRM pack already
  links those two N-N. The agent read the relationships, proposed nothing, and said why. That first run is the
  only failure on record (4/5), and it was the task's fault, not the agent's. The new task links a pair the pack
  does not relate.
- **Task 5 gets an honest answer, but not a full one.** No read tool lists what a role may do on a table, so the
  agent names the two CRM roles and explains the three layers of access, but cannot say which role edits
  contacts. A read tool for role grants belongs with #2.
- **Input cost.** Most input tokens are the tool list, read again on every model call and almost all served from
  cache. The 25 tools come to 52 KB of JSON, mostly Vietnamese descriptions.
