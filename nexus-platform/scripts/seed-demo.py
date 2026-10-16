#!/usr/bin/env python3
"""Seed the one-command demo (docker-compose.yml at the repo root): CRM pack, a sales rep, the first version.

bootstrap-admin (run by the api service before the server starts) already created the tenant and its operator
admin. Once the API is healthy, this script logs in as that admin and turns the workspace into a delivered
project a third person can use:
  1. installs the CRM pack with its sample records (skipped once the project is delivered);
  2. creates the sales rep — a NON-admin user on the pack's crm_sales_rep role, so access control is visible
     (an admin bypasses every ACL, so "it works for me" proves nothing);
  3. puts the rep in the pack's sales team, so lead routing assigns leads to someone who can see them;
  4. publishes the first version (baseline): from then on the builder's changes are drafts that the rep does
     not see until Publish.
Idempotent: a second run finds everything in place and changes nothing. Standard library only.

    NEXUS_ADMIN_PASS=… NEXUS_STAFF_PASS=… python3 scripts/seed-demo.py --base http://localhost:8080/api/v1
"""
import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

PACK_ROLE = "crm_sales_rep"
PACK_TEAM = "Đội Sales"


def req(base, method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data is not None else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    r = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=60) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.load(e)
        except ValueError:
            return e.code, {}


def fail(msg):
    print("seed-demo: " + msg, file=sys.stderr)
    sys.exit(1)


def wait_healthy(base, seconds=180):
    health = base.split("/api/")[0] + "/health"
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(health, timeout=5) as r:
                if r.status == 200:
                    return
        except (urllib.error.URLError, OSError):
            pass
        time.sleep(2)
    fail(f"API not healthy after {seconds}s ({health})")


def login(base, tenant, email, password):
    code, body = req(base, "POST", "/auth/login", body={"tenant": tenant, "email": email, "password": password})
    token = (body.get("data") or {}).get("accessToken")
    if code != 200 or not token:
        fail(f"login {email} failed ({code} {body.get('code', '')})")
    return token


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default=os.environ.get("NEXUS_API", "http://localhost:8080/api/v1"))
    a = ap.parse_args()
    tenant = os.environ.get("NEXUS_TENANT", "northwind")
    admin_email = os.environ.get("NEXUS_ADMIN_EMAIL", "admin@example.com")
    rep_email = os.environ.get("NEXUS_STAFF_EMAIL", "test@example.com")
    admin_pass, rep_pass = os.environ.get("NEXUS_ADMIN_PASS"), os.environ.get("NEXUS_STAFF_PASS")
    if not admin_pass or not rep_pass:
        fail("NEXUS_ADMIN_PASS and NEXUS_STAFF_PASS are required")

    wait_healthy(a.base)
    adm = login(a.base, tenant, admin_email, admin_pass)

    code, body = req(a.base, "GET", "/admin/publish/state", adm)
    if code != 200:
        fail(f"publish state: {code} {body.get('code', '')}")
    delivered = bool((body.get("data") or {}).get("delivered"))

    if not delivered:
        code, body = req(a.base, "POST", "/admin/verticals/install", adm, {"builtin": "crm"})
        if code >= 300 and "INSTALLED" not in str(body.get("code", "")) and "EXISTS" not in str(body.get("code", "")):
            fail(f"install CRM pack: {code} {body.get('code', '')} {body.get('error', '')}")
        print(f"seed-demo: CRM pack {'installed' if code < 300 else 'already installed'}")

    code, body = req(a.base, "GET", "/admin/roles", adm)
    roles = (body.get("data") or {}).get("roles") or []
    role = next((r for r in roles if r.get("name") == PACK_ROLE), None)
    if not role:
        fail(f"role {PACK_ROLE} not found — is the CRM pack installed?")

    code, body = req(a.base, "GET", "/admin/users", adm)
    users = (body.get("data") or {}).get("users") or []
    rep = next((u for u in users if (u.get("email") or "").lower() == rep_email.lower()), None)
    if not rep:
        code, body = req(a.base, "POST", "/admin/users", adm, {
            "email": rep_email, "password": rep_pass, "fullName": "Sam Rivera (sales rep)",
            "status": "active", "isAdmin": False, "roleId": role["id"]})
        rep = (body.get("data") or {}).get("user") or {}
        if code != 201 or not rep.get("id"):
            fail(f"create rep: {code} {body.get('code', '')} {body.get('error', '')}")
        print(f"seed-demo: created {rep_email} on role {PACK_ROLE}")

    code, body = req(a.base, "GET", "/admin/teams", adm)
    team = next((t for t in (body.get("data") or {}).get("teams") or [] if t.get("name") == PACK_TEAM), None)
    if team:
        code, body = req(a.base, "POST", f"/admin/teams/{team['id']}/members", adm, {"userId": rep["id"]})
        if code >= 300:
            fail(f"add rep to {PACK_TEAM}: {code} {body.get('code', '')}")

    if not delivered:
        code, body = req(a.base, "POST", "/admin/publish/baseline", adm, {"note": "Demo seed: first published version"})
        if code >= 300:
            fail(f"baseline: {code} {body.get('code', '')} {body.get('error', '')}")
        print("seed-demo: first version published (baseline) — builder changes are drafts from now on")

    login(a.base, tenant, rep_email, rep_pass)  # the rep can sign in
    print(f"seed-demo: ready · workspace '{tenant}' · builder {admin_email} · sales rep {rep_email}")


if __name__ == "__main__":
    main()
