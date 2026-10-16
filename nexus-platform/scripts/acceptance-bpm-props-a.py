#!/usr/bin/env python3
"""Nghiệm thu lát FE-BPM-PROPS-a của specs/043 (DAILY-QUEUE #132): đúng những BODY mà trình sửa Studio
(`ApprovalBuilder` + `lib/processModel.ts`) gửi, chạy trên server dựng từ DB CLONE, sau `scripts/fixture-bpm.py`.

Màn Studio là của người dựng (admin); khách/nhân viên KHÔNG bao giờ thấy nó — nên kiểm thêm: nhân viên không-admin
(test@example.com) bị chặn 403 khỏi `/admin/approval-processes`, và quy trình dựng từ Studio CHẠY ĐƯỢC cho người không-admin
(duyet1..3 + test@ gửi, đúng người thấy việc, quorum 60% xong sau 2 phiếu).

  A. Body của trình sửa được server nhận: bước vai trò + bước nhiều người quorum 60% + bước nhóm xoay vòng,
     «Khi từ chối → về người gửi» + outcome.returnedValue ⇒ 201; GET trả `graph` (steps rỗng) đủ 3 nút.
  B. Lưu lại đúng như Studio (steps: [] + graph.nodes[kind approval] + options.fallbackAssignee) ⇒ 200, mã bước giữ.
  C. Lưu tuyến tính đè lên đồ thị ⇒ 409 APPROVAL_PROCESS_V2_OVERWRITE; `?force=true` ⇒ 200 và đồ thị mất.
  D. 422 mang `details.field` trỏ `steps[i]` / `graph.nodes[i]` để Studio ghim đúng thẻ bước.
  E. Người không-admin: 403 ở route Studio; chạy trọn quy trình quorum.

    python3 scripts/acceptance-bpm-props-a.py --base http://localhost:8803/api/v1

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.  ponytail: stdlib thuần.
"""
import argparse
import json
import sys
import urllib.error
import urllib.request

FAILS = []


def check(label, ok, extra=""):
    print(("PASS " if ok else "FAIL ") + label + (f"  [{extra}]" if (extra and not ok) else ""))
    if not ok:
        FAILS.append(label)


class Api:
    def __init__(self, base, tenant):
        self.base, self.tenant = base, tenant

    def req(self, method, path, tok=None, body=None):
        data = json.dumps(body).encode() if body is not None else None
        h = {"Content-Type": "application/json"} if data else {}
        if tok:
            h["Authorization"] = "Bearer " + tok
        r = urllib.request.Request(self.base + path, data=data, headers=h, method=method)
        try:
            with urllib.request.urlopen(r, timeout=20) as resp:
                raw = resp.read()
                return resp.status, (json.loads(raw) if raw else {})
        except urllib.error.HTTPError as e:
            try:
                return e.code, json.load(e)
            except Exception:
                return e.code, {}

    def login(self, email, pw):
        c, b = self.req("POST", "/auth/login", None, {"tenant": self.tenant, "email": email, "password": pw})
        if c != 200:
            sys.exit(f"LỖI MÔI TRƯỜNG: đăng nhập {email} {c}")
        return b["data"]["accessToken"], b["data"].get("user") or {}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://localhost:8803/api/v1")
    ap.add_argument("--tenant", default="northwind")
    a = ap.parse_args()
    api = Api(a.base, a.tenant)
    adm, _ = api.login("admin@example.com", "Demo-Admin-2026!")
    staff, _ = api.login("test@example.com", "Demo-Rep-2026!")
    d1, d1u = api.login("duyet1@example.com", "Duyet#Bpm-2026")
    d2, _ = api.login("duyet2@example.com", "Duyet#Bpm-2026")
    d3, _ = api.login("duyet3@example.com", "Duyet#Bpm-2026")

    c, b = api.req("GET", "/admin/teams", adm)
    team = next(t for t in b["data"]["teams"] if t["name"] == "Ban duyệt")["id"]
    c, b = api.req("GET", "/admin/users", adm)
    users = {u["email"]: u["id"] for u in (b["data"].get("users") or b["data"])}
    c, b = api.req("GET", "/admin/roles", adm)
    role = next(r for r in b["data"]["roles"] if r["name"] == "crm_sales_manager")["id"]

    # deactivate any manual process on crm_quotes so only ours is armed
    c, b = api.req("GET", "/admin/approval-processes", adm)
    for p in b["data"]["processes"]:
        if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]:
            api.req("PUT", f"/admin/approval-processes/{p['id']}", adm, {
                "name": p["name"], "entityName": p["entityName"], "trigger": "manual", "isActive": False,
                "steps": p["steps"] or [], **({"graph": p["graph"]} if p.get("graph") else {}),
                **({"options": p["options"]} if p.get("options") else {}),
                **({"outcome": p["outcome"]} if p.get("outcome") else {})})

    # A — the editor's create body
    body = {
        "name": "PROPS-a nghiệm thu", "entityName": "crm_quotes", "trigger": "manual", "isActive": False,
        "steps": [
            {"approverType": "role", "approverId": role, "slaSeconds": 3600},
            {"approvers": [{"type": "user", "id": users["duyet1@example.com"]}, {"type": "user", "id": users["duyet2@example.com"]},
                           {"type": "user", "id": users["duyet3@example.com"]}],
             "completion": {"mode": "quorum", "percent": 60}, "slaSeconds": 7200, "slaClock": "wall",
             "onReject": {"to": "@submitter", "resumeAt": "rejector"}},
            {"assignment": {"type": "team", "id": team, "strategy": "round_robin"}, "slaSeconds": 3600, "onReject": {"to": "@end"}},
        ],
        "outcome": {"statusField": "status", "approvedValue": "Đã duyệt", "rejectedValue": "Từ chối", "returnedValue": "Cần bổ sung"},
    }
    body["steps"][2].pop("onReject")  # the editor drops "@end"
    c, b = api.req("POST", "/admin/approval-processes", adm, body)
    check("A POST body của Studio ⇒ 201", c == 201, f"{c} {b}")
    proc = (b.get("data") or {}).get("process") or {}
    pid = proc.get("id")
    c, b = api.req("GET", f"/admin/approval-processes/{pid}", adm)
    g = b["data"]["process"].get("graph") or {}
    nodes = g.get("nodes") or []
    check("A GET trả graph 3 nút, steps rỗng", len(nodes) == 3 and not b["data"]["process"].get("steps"), str(nodes)[:200])
    check("A nút 2 giữ quorum 60% + onReject submitter", nodes and nodes[1].get("completion") == {"mode": "quorum", "percent": 60}
          and nodes[1].get("onReject", {}).get("to") == "@submitter")
    check("A nút 3 là nhóm xoay vòng", nodes and nodes[2].get("assignment", {}).get("strategy") == "round_robin")
    check("A outcome.returnedValue giữ", b["data"]["process"].get("outcome", {}).get("returnedValue") == "Cần bổ sung")
    ids = [n.get("id") for n in nodes]

    # B — save back as the editor does for a graph-stored process
    save = {k: b["data"]["process"][k] for k in ("name", "entityName", "trigger", "isActive", "outcome")}
    save["steps"] = []
    save["graph"] = {"nodes": [{kk: vv for kk, vv in n.items()} for n in nodes]}
    save["options"] = {"fallbackAssignee": {"type": "role", "id": role}}
    c, b2 = api.req("PUT", f"/admin/approval-processes/{pid}", adm, save)
    check("B PUT graph + options ⇒ 200", c == 200, f"{c} {b2}")
    c, b = api.req("GET", f"/admin/approval-processes/{pid}", adm)
    check("B mã bước giữ nguyên", [n.get("id") for n in b["data"]["process"]["graph"]["nodes"]] == ids)
    check("B người dự phòng đọc lại được", (b["data"]["process"].get("options") or {}).get("fallbackAssignee", {}).get("id") == role)

    # C — linear over graph
    lin = {"name": save["name"], "entityName": "crm_quotes", "trigger": "manual", "isActive": False,
           "steps": [{"approverType": "role", "approverId": role, "slaSeconds": 3600}], "options": save["options"]}
    c, b3 = api.req("PUT", f"/admin/approval-processes/{pid}", adm, lin)
    check("C linear đè graph ⇒ 409 APPROVAL_PROCESS_V2_OVERWRITE", c == 409 and b3.get("code") == "APPROVAL_PROCESS_V2_OVERWRITE", f"{c} {b3}")

    # D — 422 field path (before force, on a fresh body)
    bad = json.loads(json.dumps(save))
    bad["graph"]["nodes"][1]["completion"] = {"mode": "quorum", "percent": 0}
    c, b4 = api.req("PUT", f"/admin/approval-processes/{pid}", adm, bad)
    f = (b4.get("details") or {}).get("field", "")
    check("D 422 quorum 0 trỏ nút 2 (details.field hoặc «bước 2:»)", c == 422 and ("[1]" in f or b4.get("error", "").startswith("bước 2")), f"{c} {b4}")
    bad2 = {"name": "x", "entityName": "crm_quotes", "trigger": "manual", "isActive": False,
            "steps": [{"approverType": "role", "approverId": role}, {"assignment": {"type": "team", "id": "", "strategy": "pool"}}]}
    c, b5 = api.req("POST", "/admin/approval-processes", adm, bad2)
    f = (b5.get("details") or {}).get("field", "")
    check("D 422 nhóm thiếu id trỏ bước 2 (details.field hoặc «bước 2:»)", c == 422 and ("[1]" in f or b5.get("error", "").startswith("bước 2")), f"{c} {b5}")

    c, b6 = api.req("PUT", f"/admin/approval-processes/{pid}?force=true", adm, lin)
    check("C ?force=true ⇒ 200", c == 200, f"{c} {b6}")
    c, b = api.req("GET", f"/admin/approval-processes/{pid}", adm)
    check("C đồ thị mất, còn 1 bước tuyến tính", not b["data"]["process"].get("graph") and len(b["data"]["process"]["steps"]) == 1)

    # E — non-admin
    c, _ = api.req("GET", "/admin/approval-processes", staff)
    check("E test@example.com bị chặn khỏi route Studio (403)", c == 403, str(c))
    save["isActive"] = True
    c, _ = api.req("PUT", f"/admin/approval-processes/{pid}", adm, save | {"steps": [], "graph": {"nodes": [nodes[1]]}})
    check("E dựng lại quy trình 1 bước quorum đang bật ⇒ 200", c == 200, str(c))
    c, bq = api.req("POST", "/crm_quotes", staff, {"name": "PROPS-a báo giá", "status": "Nháp"})
    rid = bq["data"].get("id") or bq["data"].get("record", {}).get("id")
    c, bs = api.req("POST", f"/crm_quotes/{rid}/approvals/submit", staff)
    inst = (bs.get("data") or {}).get("approval") or bs.get("data") or {}
    iid = inst.get("ID") or inst.get("id")
    check("E test@ gửi duyệt ⇒ 201/200", c in (200, 201) and iid, f"{c} {bs}")
    c, p1 = api.req("GET", "/approvals/pending", d1)
    check("E duyet1 thấy việc", any((x.get("id") or x.get("ID")) == iid for x in (p1.get("data") or {}).get("approvals", p1.get("data") or [])), str(p1)[:200])
    c, p0 = api.req("GET", "/approvals/pending", staff)
    check("E test@ không thấy việc của người khác", not any((x.get("id") or x.get("ID")) == iid for x in (p0.get("data") or {}).get("approvals", p0.get("data") or [])))
    api.req("POST", f"/approvals/{iid}/approve", d1, {"note": "ok"})
    c, bd = api.req("GET", f"/approvals/{iid}", d1)
    st = (bd["data"].get("approval") or bd["data"]).get("status") or (bd["data"].get("approval") or bd["data"]).get("Status")
    check("E sau 1/3 phiếu vẫn chờ", st == "pending", str(st))
    api.req("POST", f"/approvals/{iid}/approve", d2, {"note": "ok"})
    c, bd = api.req("GET", f"/approvals/{iid}", d3)
    st = (bd["data"].get("approval") or bd["data"]).get("status") or (bd["data"].get("approval") or bd["data"]).get("Status")
    check("E sau 2/3 phiếu (60%) ⇒ approved", st == "approved", str(st))

    api.req("PUT", f"/admin/approval-processes/{pid}", adm, lin | {"isActive": False})
    print("\nĐẠT" if not FAILS else f"\nHỎNG {len(FAILS)}: {FAILS}")
    sys.exit(1 if FAILS else 0)


main()
