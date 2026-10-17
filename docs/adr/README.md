# Architecture Decision Records

The decisions that hold the design up, one page each. They were written after the code
([#10](https://github.com/doxuta/nexus/issues/10)): every claim links to the lines it rests on, pinned to the commit
named in the record, and a claim that could not be checked was left out. "Alternatives considered" lists the options a
reviewer would raise and why the current design wins or loses against them.

| # | Decision |
|---|---|
| [0001](0001-metadata-is-the-agent-action-space.md) | Agents edit typed, versioned metadata through validated tools, never code, SQL or DDL. |
| [0002](0002-schema-cutoff-drafts-and-rollback.md) | Drafts until publish; new schema hidden by a cutoff; rollback re-hides it and never drops a table, field or record. |
| [0003](0003-agent-boundary-from-change-classes.md) | Agents only propose, access and MCP included; how an applied change reaches users, and whether it needs a confirm, follows its class. |
| [0004](0004-sqlx-for-dynamic-records-gorm-for-system-tables.md) | sqlx with allowlisted identifiers for the shared `obj_*` tables; GORM for fixed-shape system tables. |
| [0005](0005-tenant-isolation-and-row-level-access.md) | `tenant_id` on every query, ACL before the handler, row scope in the repository; a convention plus tests, not database security. |

## Template

```markdown
# NNNN. Title: the decision, stated as a fact

- **Status:** Proposed | Accepted | Superseded by NNNN
- **Recorded:** date, and the commit the citations point at
- **Paths:** how to read the paths in the citations

## Context
The forces at play: what is true of the system, and what pulls against what.

## Decision
What we do, stated so that the code can be checked against it. Each claim cites `path:line`.

## Consequences
**Gained.** What the decision buys.
**Costs.** What it costs, the known gaps, and the open issues that track them.
**Given up.** What we chose not to have.

## Alternatives considered
- **Option.** What it would give, and why it lost.
```
