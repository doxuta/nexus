---
name: slice
description: Write the contract for one vertical slice of Nexus before coding — the problem, which step of the demo loop it serves, acceptance criteria a script or a click can check (at least one as the non-admin rep), the middle-out build order and the evidence the PR will show. Use when an issue has no contract yet and the change touches more than two files or anything a user sees.
---

# Slice contract

1. Check the slice serves the product loop: prompt → diff → preview → publish → the rep uses the app → rollback,
   or the same loop through MCP. If it does not, say so instead of writing a contract.
2. Put the contract in the GitHub issue (or the PR description if there is no issue), using this template:

```markdown
## Problem
Two to four sentences: who is stuck, where, and what they see today.

## Loop step
Which step of the demo loop this serves.

## Acceptance
- [ ] Each line checkable by one command or one click, with the expected result.
- [ ] At least one line run as the non-admin rep.
- [ ] Failure path: what the user sees when it goes wrong.

## Out of scope
What this slice deliberately does not do.

## Build order (middle-out)
1. API contract (request/response, error codes) + a test.
2. UI against the contract.
3. Service and repository behind it.
4. Migration, if any (up + down).

## Evidence for the PR
Gate output lines, screenshots of the changed screens, the acceptance script run.
```

3. Keep it to one screen. A design decision that will outlive the slice gets an ADR in `docs/adr/` instead.
