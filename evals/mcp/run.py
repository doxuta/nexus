#!/usr/bin/env python3
"""A small tool-use eval of the MCP endpoint (issue #3): can an outside agent turn a plain request into the right
proposal, and only a proposal?

Each task gets its own agent key, so its own Studio conversation: the runner issues the key as the builder, runs one
headless Claude Code session on it, revokes the key, then grades the proposals the conversation holds (their kinds
and names) with code, never with a model. A session that never reached the server fails, whatever it answered.
Nothing is applied: the workspace is left as it was. test_run.py checks the pass rule on recorded transcripts.

    python3 evals/mcp/run.py [--base http://localhost:8080] [--only 1,5] [--out DIR] [-- <extra claude flags>]

Needs a running one-command demo (the builder admin@example.com) and a logged-in Claude Code. Extra claude flags go
after `--`, e.g. `-- --model opus`. --out keeps each task's stream-json transcript there, every nxm_ token redacted.
The key never reaches the terminal or a file other than the MCP config in a private temp dir, removed after the task.
Exit 1 when a task fails. Standard library only.
"""
import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request

BUILDER = {"tenant": "northwind", "email": "admin@example.com", "password": os.environ.get("NEXUS_ADMIN_PASS", "Demo-Admin-2026!")}


def drafts_of(thread):
    return [d for m in thread["messages"] for d in (m.get("draft") or [])]


def has(drafts, kind, test):
    return any(d.get("kind") == kind and test(d) for d in drafts)


def label(d):
    """kind:name of one proposal, for the report (a layout is named by its table and view)."""
    p = d.get(d["kind"]) or {}
    return f'{d["kind"]}:{p.get("entity", "")}/{p.get("viewType", "")}' if d["kind"] == "layout" else f'{d["kind"]}:{p.get("name", "")}'


# (id, prompt, grader over the proposals of the task's conversation, what the grader wants)
TASKS = [
    (1, "Add a Warehouses table with a name, an address and a capacity (a number).",
     lambda ds: has(ds, "entity", lambda d: "warehouse" in d["entity"]["name"] and len(d["entity"]["fields"]) >= 3),
     "an entity proposal named *warehouse* with at least 3 fields"),
    (2, "Add a LinkedIn profile URL field to the CRM contacts table.",
     lambda ds: has(ds, "field", lambda d: d["field"].get("entity") == "crm_contacts" and "linkedin" in d["field"]["name"]),
     "a field proposal on crm_contacts named *linkedin*"),
    # The CRM pack already links contacts and opportunities N-N: asked for that, the agent rightly proposes nothing.
    (3, "Link CRM products and CRM contracts many-to-many, so a contract can list the products it covers.",
     lambda ds: has(ds, "relationship", lambda d: {d["relationship"]["entityA"], d["relationship"]["entityB"]} == {"crm_products", "crm_contracts"}),
     "an N-N relationship proposal between crm_products and crm_contracts"),
    (4, "Propose a list layout for CRM leads that shows the name, the company and the status, newest first.",
     lambda ds: has(ds, "layout", lambda d: d["layout"]["entity"] == "crm_leads" and d["layout"]["viewType"] == "list"),
     "a list layout proposal for crm_leads"),
    (5, "Who can edit contacts?",
     lambda ds: len(ds) == 0,
     "no proposal at all (a read-only question)"),
]


def req(base, method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    r = urllib.request.Request(base + "/api/v1" + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        return e.code, {}


def redact(s):
    return re.sub(r"nxm_[A-Za-z0-9_-]{40,}", "nxm_[REDACTED]", s)


def parse_events(stdout):
    """The stream-json events of one `claude -p` session."""
    return [json.loads(line) for line in stdout.splitlines() if line.strip().startswith("{")]


def tool_calls(events):
    """Names of the nexus tools the agent called, in order (mcp__nexus__ prefix dropped)."""
    return [c["name"].removeprefix("mcp__nexus__") for e in events if e.get("type") == "assistant"
            for c in e["message"]["content"] if c.get("type") == "tool_use"]


def reached_server(events):
    """Whether the session reached Nexus: a nexus_ tool call, or the nexus server connected in the init event."""
    init = next((e for e in events if e.get("type") == "system" and e.get("subtype") == "init"), {})
    connected = any(s.get("name") == "nexus" and s.get("status") == "connected" for s in init.get("mcp_servers") or [])
    return connected or any(c.startswith("nexus_") for c in tool_calls(events))


def passes(task, events, drafts, returncode):
    """Whether a task passed: the session reached the server, ended cleanly, and the grader accepts the thread's
    proposals. A session whose MCP connection failed still exits 0, and with no proposal it would pass task 5."""
    return returncode == 0 and reached_server(events) and task[2](drafts)


def run_task(base, jwt, task, claude_args, out):
    tid, prompt, grade, want = task
    code, body = req(base, "POST", "/admin/mcp-tokens", jwt, {})
    if code != 201:
        sys.exit(f"issue an agent key: HTTP {code}")
    tok, secret = body["data"]["token"], body["data"]["secret"]
    tmp = tempfile.mkdtemp(prefix="nexus-mcp-eval-")
    try:
        cfg = os.path.join(tmp, "mcp.json")
        with open(os.open(cfg, os.O_WRONLY | os.O_CREAT, 0o600), "w") as f:
            json.dump({"mcpServers": {"nexus": {"type": "http", "url": base + "/mcp", "headers": {"Authorization": "Bearer " + secret}}}}, f)
        p = subprocess.run(["claude", "-p", prompt, "--mcp-config", cfg, "--strict-mcp-config", "--allowedTools", "mcp__nexus",
                            "--output-format", "stream-json", "--verbose", "--no-session-persistence", *claude_args],
                           cwd=tmp, capture_output=True, text=True, timeout=15 * 60, stdin=subprocess.DEVNULL)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
        req(base, "DELETE", "/admin/mcp-tokens/" + tok["id"], jwt)
    if out:
        with open(os.path.join(out, f"task-{tid}.jsonl"), "w") as f:
            f.write(redact(p.stdout))
    events = parse_events(p.stdout)
    calls = tool_calls(events)
    result = next((e for e in events if e.get("type") == "result"), {})
    usage = result.get("usage") or {}
    model = next((e.get("model") for e in events if e.get("type") == "system" and e.get("subtype") == "init"), "?")
    code, body = req(base, "GET", "/admin/ai/builder/sessions/" + tok["sessionId"], jwt)
    ds = drafts_of(body["data"]) if code == 200 else []
    ok = code == 200 and passes(task, events, ds, p.returncode)
    return {
        "task": tid, "prompt": prompt, "want": want, "pass": ok, "model": model,
        "proposals": [label(d) for d in ds],
        "tools": calls, "reached_server": reached_server(events), "turns": result.get("num_turns"),
        "input_tokens": usage.get("input_tokens", 0) + usage.get("cache_creation_input_tokens", 0) + usage.get("cache_read_input_tokens", 0),
        "cached_input_tokens": usage.get("cache_read_input_tokens", 0),
        "output_tokens": usage.get("output_tokens", 0),
        "cost_usd": result.get("total_cost_usd"),
        "error": None if p.returncode == 0 else redact(p.stderr)[-300:],
    }


def main():
    argv = sys.argv[1:]
    claude_args = argv[argv.index("--") + 1:] if "--" in argv else []
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--base", default=os.environ.get("NEXUS_URL", "http://localhost:8080"))
    ap.add_argument("--only", default="", help="comma-separated task ids")
    ap.add_argument("--out", default="", help="directory for the redacted transcripts")
    a = ap.parse_args(argv[: argv.index("--")] if "--" in argv else argv)
    base = a.base.rstrip("/")
    if a.out:
        os.makedirs(a.out, exist_ok=True)
    code, body = req(base, "POST", "/auth/login", body=BUILDER)
    jwt = (body.get("data") or {}).get("accessToken")
    if code != 200 or not jwt:
        sys.exit(f"builder login failed: HTTP {code}")
    only = {int(x) for x in a.only.split(",") if x.strip()}
    rows = [run_task(base, jwt, t, claude_args, a.out) for t in TASKS if not only or t[0] in only]
    print("| # | task | wants | proposals | tool calls | input tokens (cached) | output tokens | result |")
    print("|---|---|---|---|---|---|---|---|")
    for r in rows:
        print(f'| {r["task"]} | {r["prompt"]} | {r["want"]} | {", ".join(r["proposals"]) or "none"} | {len(r["tools"])} | '
              f'{r["input_tokens"]:,} ({r["cached_input_tokens"]:,}) | {r["output_tokens"]:,} | {"pass" if r["pass"] else "FAIL"} |')
    print(f'\n{sum(r["pass"] for r in rows)}/{len(rows)} passed · model {rows[0]["model"] if rows else "?"}')
    if a.out:
        with open(os.path.join(a.out, "results.json"), "w") as f:
            json.dump(rows, f, indent=2)
    sys.exit(0 if all(r["pass"] for r in rows) else 1)


if __name__ == "__main__":
    main()
