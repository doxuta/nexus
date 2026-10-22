#!/usr/bin/env python3
"""Re-grade a `claude plugin eval --keep-temp` run with real commands:
python3 evals/harness/regrade.py <aggregate-result.json> [--archive DIR] [--clean]
python3 evals/harness/regrade.py --from-archive DIR

`claude plugin eval` has no custom-code graders, so its own score reads files and the gates.txt the agent wrote.
This script checks the same runs the hard way. For every run in the result (both arms), it copies the workspace the
run left (kept by --keep-temp) and runs <case>/regrade.sh on the copy: gofmt, go vet, go build, the gate scripts
taken from BASE, and tests the agent never saw (<case>/hidden/, lib/hidden/) that call the code it wrote. A run
passes when every check does. It prints WITH / W/OUT / Δ as pass rates and writes regrade.json next to the result.

It runs code the agent wrote (its Go code and tests, its Python) as you, outside any sandbox: grade only runs you
produced, or run it in a container.

--archive DIR writes what each run left, small and redacted, for committing: summary.json (score, graders, turns,
cost, tool calls, which plugin agents and skills were invoked, how often it ran each gate, the regrade checks),
<case>/<arm>-<n>.diff (the workspace against its scaffold) and regrade.txt. --clean removes each kept run directory
once it is graded.

--from-archive DIR re-grades an archive instead, with no model and no kept directory: each run's workspace is rebuilt
from its case's scaffold and its diff (patch -p1), graded again, and DIR's summary.json and regrade.txt are rewritten.
Standard library only.
"""
import argparse
import collections
import difflib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
CACHE = os.path.join(REPO, ".cache", "harness-eval")
HOME = os.path.expanduser("~")


def go_env():
    """Your Go and module cache, plus a build cache cloned once from the warm one, keyed without paths."""
    gocache = os.path.join(CACHE, "regrade-gocache")
    if not os.path.isdir(gocache) and os.path.isdir(os.path.join(CACHE, "gocache")):
        subprocess.run(["bash", "-c", 'cp -cR "$0" "$1" 2>/dev/null || cp -R "$0" "$1"', os.path.join(CACHE, "gocache"), gocache], check=True)
    env = dict(os.environ, GOFLAGS="-buildvcs=false -trimpath", GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off")
    if os.path.isdir(gocache):
        env["GOCACHE"] = gocache
    return env


def scaffold(case_dir):
    """Run a case's scaffold.sh the way `claude plugin eval --scaffold` does; returns (temp root, workspace)."""
    root = tempfile.mkdtemp(prefix="scaffold-")
    ws = os.path.join(root, "home", "cwd")
    os.makedirs(ws)
    os.makedirs(os.path.join(root, "tmp"))
    env = {"PATH": os.environ["PATH"], "HOME": os.path.join(root, "home"), "TMPDIR": os.path.join(root, "tmp"),
           "TERM": "dumb", "GIT_CONFIG_NOSYSTEM": "1"}
    subprocess.run(["bash", os.path.join(case_dir, "scaffold.sh")], cwd=ws, env=env, check=True)
    return root, ws


def remove(path):
    subprocess.run(["chmod", "-R", "u+rwX", path], check=False)
    shutil.rmtree(path, ignore_errors=True)


def regrade_ws(case_dir, ws, env):
    """Run the case's regrade.sh on a copy of ws (never inside it: it holds what the agent wrote, git hooks included)."""
    tmp = tempfile.mkdtemp(prefix="regrade-")
    try:
        dst = os.path.join(tmp, "ws")
        shutil.copytree(ws, dst, symlinks=True, ignore=shutil.ignore_patterns(".git"))
        p = subprocess.run(["bash", os.path.join(case_dir, "regrade.sh"), dst], env=env, capture_output=True, text=True, timeout=900)
        return p.returncode == 0, (p.stdout + p.stderr).strip()
    finally:
        remove(tmp)


def unseal(kept):
    """The kept directory is read-only and its home/ is sealed (mode 000): open both to read the workspace."""
    for d in (kept, os.path.join(kept, "sealed")):
        if os.path.isdir(d):
            os.chmod(d, 0o700)
    for ws in (os.path.join(kept, "sealed", "home", "cwd"), os.path.join(kept, "home", "cwd")):
        if os.path.isdir(ws):
            return ws
    return None


def redact(s, *prefixes):
    for p in prefixes:
        if p:
            s = s.replace(p, "<run>")
    s = s.replace(HOME, "~")
    s = re.sub(r"sk-[A-Za-z0-9_-]{16,}", "sk-[REDACTED]", s)
    s = re.sub(r"nxm_[A-Za-z0-9_-]{20,}", "nxm_[REDACTED]", s)
    return re.sub(r"eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+", "[JWT REDACTED]", s)


def text_files(root):
    out = {}
    for dp, dn, fn in os.walk(root):
        dn[:] = [d for d in dn if d not in (".git", "node_modules")]
        for f in fn:
            p = os.path.join(dp, f)
            out[os.path.relpath(p, root)] = p
    return out


def read(p):
    try:
        with open(p, encoding="utf-8") as f:
            return f.read().splitlines(keepends=True)
    except (UnicodeDecodeError, OSError):
        return None


def diff(base, ws):
    """The workspace against its scaffold, as a unified diff (binary files named only)."""
    a, b, out = text_files(base), text_files(ws), []
    for rel in sorted(set(a) | set(b)):
        x = read(a[rel]) if rel in a else []
        y = read(b[rel]) if rel in b else []
        if x == y:
            continue
        if x is None or y is None:
            out.append(f"Binary file {rel} differs\n")
            continue
        out.extend(difflib.unified_diff(x, y, "a/" + rel if rel in a else "/dev/null", "b/" + rel if rel in b else "/dev/null"))
    return "".join(out)


def trace_stats(path):
    """Tool calls of a run, the plugin agents and skills it invoked, and whether it read AGENTS.md."""
    tools, agents, skills, read_agents = collections.Counter(), [], [], False
    gates = collections.Counter()
    with open(path) as f:
        for line in f:
            e = json.loads(line)
            if e.get("type") != "assistant":
                continue
            for c in e["message"]["content"]:
                if c.get("type") != "tool_use":
                    continue
                tools[c["name"]] += 1
                inp = c.get("input") or {}
                if c["name"] in ("Agent", "Task"):
                    agents.append(inp.get("subagent_type", "?"))
                if c["name"] == "Skill":
                    skills.append(inp.get("skill", "?"))
                if c["name"] == "Read" and str(inp.get("file_path", "")).endswith("AGENTS.md"):
                    read_agents = True
                if c["name"] == "Bash":
                    for gate in ("gofmt", "go vet", "go test", "check-orphan-endpoints"):
                        gates[gate] += gate in str(inp.get("command", ""))
    return {"toolCalls": dict(tools), "agentsInvoked": agents, "skillsInvoked": skills, "readAgentsMd": read_agents,
            "gateCommands": dict(gates)}


def regrade_archive(archive, env):
    """Rebuild every archived run from its scaffold and its diff, grade it again, rewrite the archive's records."""
    path = os.path.join(archive, "summary.json")
    with open(path) as f:
        summary = json.load(f)
    log, bases = [], {}
    try:
        for r in summary["runs"]:
            case_dir = os.path.join(HERE, r["case"])
            if r["case"] not in bases:
                bases[r["case"]] = scaffold(case_dir)
            root, base = bases[r["case"]]
            ws = os.path.join(root, "rebuilt")
            shutil.copytree(base, ws, symlinks=True)
            try:
                p = subprocess.run(["patch", "-p1", "-s", "-i", os.path.abspath(os.path.join(archive, r["case"], f'{r["arm"]}-{r["run"]}.diff'))],
                                   cwd=ws, capture_output=True, text=True)
                ok, output = regrade_ws(case_dir, ws, env) if p.returncode == 0 else (False, "patch failed: " + p.stdout + p.stderr)
            finally:
                remove(ws)
            r["regradePass"], r["regrade"] = ok, redact(output, root).splitlines()
            head = f'{r["case"]} {r["arm"]} run {r["run"]}: regrade {"pass" if ok else "FAIL"} (eval score {r["evalScore"]})'
            log += [head] + ["   " + line for line in r["regrade"]]
            print("\n".join(log[-1 - len(r["regrade"]):]), file=sys.stderr)
    finally:
        for root, _ in bases.values():
            remove(root)
    with open(path, "w") as f:
        json.dump(summary, f, indent=2, ensure_ascii=False)
    with open(os.path.join(archive, "regrade.txt"), "w") as f:
        f.write("\n".join(log) + "\n")
    return summary["runs"]


def table(out):
    print("| case | WITH | W/OUT | Δ | runs |")
    print("|---|---|---|---|---|")
    for name in dict.fromkeys(r["case"] for r in out):
        w = rate([r for r in out if r["case"] == name and r["arm"] == "with"])
        wo = rate([r for r in out if r["case"] == name and r["arm"] == "without"])
        n = sum(r["case"] == name for r in out)
        print(f"| {name} | {fmt(w)} | {fmt(wo)} | {'—' if w is None or wo is None else f'{w - wo:+.2f}'} | {n} |")
    w, wo = rate([r for r in out if r["arm"] == "with"]), rate([r for r in out if r["arm"] == "without"])
    print(f"| **all** | {fmt(w)} | {fmt(wo)} | {'—' if w is None or wo is None else f'{w - wo:+.2f}'} | {len(out)} |")


def rate(runs):
    return sum(r["regradePass"] for r in runs) / len(runs) if runs else None


def fmt(x):
    return "—" if x is None else f"{x:.2f}"


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("result", nargs="?", help="aggregate-result.json (or the --json file) of a --keep-temp run")
    ap.add_argument("--from-archive", help="re-grade an archive written by --archive, rebuilding each run from its diff")
    ap.add_argument("--archive", help="write summary.json, one diff per run and regrade.txt here")
    ap.add_argument("--clean", action="store_true", help="remove each kept run directory once graded")
    a = ap.parse_args()
    if a.from_archive:
        table(regrade_archive(a.from_archive, go_env()))
        return
    if not a.result:
        ap.error("give a result file or --from-archive")
    with open(a.result) as f:
        result = json.load(f)
    root = result["suite"]["root"]
    env = go_env()
    out, log = [], []
    for case in result["cases"]:
        case_dir = os.path.join(root, case["dir"])
        base = scaffold(case_dir) if a.archive else None
        arms = {}
        for arm in ("with", "without"):
            arms[arm] = []
            for i, run in enumerate(case["arms"].get(arm) or []):
                trace = run.get("tracePath") or ""
                kept = os.path.dirname(os.path.dirname(trace)) if trace else ""
                ws = unseal(kept) if kept and os.path.isdir(kept) else None
                ok, output = regrade_ws(case_dir, ws, env) if ws else (False, "workspace not found (kept with --keep-temp?)")
                r = {"case": case["name"], "arm": arm, "run": i + 1, "evalScore": run.get("score"), "evalError": run.get("error"),
                     "graders": {g["name"]: g["passed"] for g in run.get("graders") or []},
                     "turns": run.get("turns"), "costUsd": run.get("costUsd"), "durationSeconds": run.get("durationSeconds"),
                     "regradePass": ok, "regrade": redact(output, kept).splitlines()}
                if trace and os.path.isfile(trace):
                    r.update(trace_stats(trace))
                if a.archive and ws:
                    d = os.path.join(a.archive, case["name"])
                    os.makedirs(d, exist_ok=True)
                    with open(os.path.join(d, f"{arm}-{i + 1}.diff"), "w") as f:
                        f.write(redact(diff(base[1], ws), kept, base[0]))
                arms[arm].append(r)
                out.append(r)
                head = f'{case["name"]} {arm} run {i + 1}: regrade {"pass" if ok else "FAIL"} (eval score {run.get("score")})'
                log += [head] + ["   " + line for line in r["regrade"]]
                print("\n".join([head] + ["   " + line for line in r["regrade"]]), file=sys.stderr)
                if a.clean and kept:
                    remove(kept)
        if base:
            remove(base[0])
    table(out)
    with open(os.path.join(os.path.dirname(os.path.abspath(a.result)), "regrade.json"), "w") as f:
        json.dump(out, f, indent=2, ensure_ascii=False)
    if a.archive:
        summary = {"claudeVersion": result.get("claudeVersion"), "startedAt": result.get("startedAt"),
                   "durationSeconds": result.get("durationSeconds"), "costUsd": result.get("costUsd"),
                   "model": result["suite"].get("modelOverride"), "runs": out}
        with open(os.path.join(a.archive, "summary.json"), "w") as f:
            json.dump(summary, f, indent=2, ensure_ascii=False)
        with open(os.path.join(a.archive, "regrade.txt"), "w") as f:
            f.write("\n".join(log) + "\n")


if __name__ == "__main__":
    main()
