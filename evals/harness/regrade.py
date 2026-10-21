#!/usr/bin/env python3
"""Re-grade a `claude plugin eval --keep-temp` run with real commands: python3 evals/harness/regrade.py <result.json>

`claude plugin eval` has no custom-code graders, so its own score reads files and the gates.txt the agent wrote.
This script checks the same runs the hard way. For every run in the result (both arms), it copies the workspace the
run left (kept by --keep-temp), adds the tests held out of the run (<case>/hidden/), and runs <case>/regrade.sh:
gofmt, go vet, go build, go test, the orphan-endpoint gate, the MCP eval's own tests. A run passes when every check
does. It prints WITH / W/OUT / Δ as pass rates and writes regrade.json next to the result.

--clean removes each kept run directory once it is graded. Standard library only.
"""
import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
CACHE = os.path.join(REPO, ".cache", "harness-eval")


def go_env():
    """Your Go and module cache, plus a build cache cloned once from the warm one, keyed without paths."""
    gocache = os.path.join(CACHE, "regrade-gocache")
    if not os.path.isdir(gocache) and os.path.isdir(os.path.join(CACHE, "gocache")):
        subprocess.run(["bash", "-c", 'cp -cR "$0" "$1" 2>/dev/null || cp -R "$0" "$1"', os.path.join(CACHE, "gocache"), gocache], check=True)
    env = dict(os.environ, GOFLAGS="-buildvcs=false -trimpath", GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off")
    if os.path.isdir(gocache):
        env["GOCACHE"] = gocache
    return env


def unseal(kept):
    """The kept directory is read-only and its home/ is sealed (mode 000): open both to read the workspace."""
    for d in (kept, os.path.join(kept, "sealed")):
        if os.path.isdir(d):
            os.chmod(d, 0o700)
    for ws in (os.path.join(kept, "sealed", "home", "cwd"), os.path.join(kept, "home", "cwd")):
        if os.path.isdir(ws):
            return ws
    return None


def regrade_run(case_dir, run, env):
    trace = run.get("tracePath") or ""
    kept = os.path.dirname(os.path.dirname(trace)) if trace else ""
    ws = unseal(kept) if kept and os.path.isdir(kept) else None
    if not ws:
        return {"pass": False, "kept": kept, "output": "workspace not found (was the run kept with --keep-temp?)"}
    tmp = tempfile.mkdtemp(prefix="regrade-", dir=CACHE if os.path.isdir(CACHE) else None)
    try:
        dst = os.path.join(tmp, "ws")
        # Never run anything inside the kept directory: it holds what the agent wrote (git hooks included).
        shutil.copytree(ws, dst, symlinks=True, ignore=shutil.ignore_patterns(".git"))
        p = subprocess.run(["bash", os.path.join(case_dir, "regrade.sh"), dst], env=env, capture_output=True, text=True, timeout=900)
        return {"pass": p.returncode == 0, "kept": kept, "output": (p.stdout + p.stderr).strip()}
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def remove(kept):
    subprocess.run(["chmod", "-R", "u+rwX", kept], check=False)
    shutil.rmtree(kept, ignore_errors=True)


def rate(runs):
    return sum(r["pass"] for r in runs) / len(runs) if runs else None


def fmt(x):
    return "—" if x is None else f"{x:.2f}"


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("result", help="aggregate-result.json (or the --json file) of a --keep-temp run")
    ap.add_argument("--clean", action="store_true", help="remove each kept run directory once graded")
    a = ap.parse_args()
    with open(a.result) as f:
        result = json.load(f)
    root = result["suite"]["root"]
    env = go_env()
    rows, out = [], []
    for case in result["cases"]:
        case_dir = os.path.join(root, case["dir"])
        arms = {}
        for arm in ("with", "without"):
            arms[arm] = []
            for i, run in enumerate(case["arms"].get(arm) or []):
                r = regrade_run(case_dir, run, env)
                r.update(case=case["name"], arm=arm, run=i + 1, evalScore=run.get("score"), evalError=run.get("error"))
                arms[arm].append(r)
                out.append(r)
                print(f'{case["name"]} {arm} run {i + 1}: {"pass" if r["pass"] else "FAIL"} (eval score {run.get("score")})', file=sys.stderr)
                for line in r["output"].splitlines():
                    print("   " + line, file=sys.stderr)
                if a.clean and r["kept"]:
                    remove(r["kept"])
        w, wo = rate(arms["with"]), rate(arms["without"])
        rows.append((case["name"], w, wo, None if w is None or wo is None else w - wo, len(arms["with"]) + len(arms["without"])))
    print("| case | WITH | W/OUT | Δ | runs |")
    print("|---|---|---|---|---|")
    for name, w, wo, d, n in rows:
        print(f"| {name} | {fmt(w)} | {fmt(wo)} | {'—' if d is None else f'{d:+.2f}'} | {n} |")
    allw = [r for r in out if r["arm"] == "with"]
    allwo = [r for r in out if r["arm"] == "without"]
    w, wo = rate(allw), rate(allwo)
    print(f"| **all** | {fmt(w)} | {fmt(wo)} | {'—' if w is None or wo is None else f'{w - wo:+.2f}'} | {len(out)} |")
    with open(os.path.join(os.path.dirname(os.path.abspath(a.result)), "regrade.json"), "w") as f:
        json.dump(out, f, indent=2, ensure_ascii=False)


if __name__ == "__main__":
    main()
