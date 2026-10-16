#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S9b của specs/043 (`bpm-start-approval-action`, DAILY-QUEUE #123, T159).

Người gửi chọn được quy trình, gọi API lặp lại không tạo trùng, và luật tự động khởi động được quy trình duyệt.
Chạy trên server dựng từ DB CLONE (không bao giờ DB dev dùng chung), sau `scripts/fixture-bpm.py`, trên bản cài
mặc định (KHÔNG đặt WORKFLOW_SCHEDULER_INTERVAL_SEC): luật `date_offset` chạy bằng job bền `workflow_timer`.

  1. Hai quy trình gửi tay cùng khớp `crm_contracts` (A → duyet1, B → duyet2). test@example.com (không-admin) tạo hợp
     đồng, `GET .../approvals/startable` thấy cả hai; gửi với `processId` của B + `Idempotency-Key` ⇒ 201 hồ sơ B,
     duyet2 thấy việc, duyet1 KHÔNG thấy.
  2. Gửi lại cùng khoá ⇒ cùng hồ sơ (CSDL đúng 1 dòng); cùng khoá + hợp đồng khác ⇒ 422 APPROVAL_IDEMPOTENCY_KEY_REUSED;
     không khoá ⇒ 422 APPROVAL_ALREADY_PENDING; `processId` lạ ⇒ 422 APPROVAL_PROCESS_NOT_APPLICABLE.
  3. Luật `after_update` «status vừa đổi thành Hiệu lực → start_approval A»: test@ đổi trạng thái ⇒ duyet1 thấy dòng,
     người gửi = test@. Lưu luật trỏ quy trình không tồn tại ⇒ 422 WF_START_APPROVAL_INVALID.
  4. Luật `date_offset` «1 phút trước end_date → start_approval A» (end_date = hôm nay ⇒ đã tới hạn): test@ tạo hợp đồng
     ⇒ job bền chạy ⇒ dòng mới ở /approvals/pending của duyet1 mà KHÔNG bật scheduler.

    python3 scripts/fixture-bpm.py --base http://localhost:8702/api/v1
    python3 scripts/acceptance-bpm-start-approval.py --base http://localhost:8702/api/v1 --db nexus_wt_bpm_start_approval_action

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: stdlib + `docker exec nexus-mysql mysql` (cùng khuôn acceptance-bpm-open-key.py, mà script này nạp lại).
"""
import argparse
import datetime
import importlib.util
import json
import os
import sys
import time
import urllib.error
import urllib.request

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
check = seats_mod.check


class Env(seats_mod.Env):
    """seats Env + extra request headers (Idempotency-Key)."""

    def req(self, method, path, token=None, body=None, headers=None):
        if not headers:
            return super().req(method, path, token, body)
        data = json.dumps(body).encode() if body is not None else None
        h = {"Content-Type": "application/json", **headers}
        if token:
            h["Authorization"] = "Bearer " + token
        r = urllib.request.Request(self.base + path, data=data, headers=h, method=method)
        try:
            with urllib.request.urlopen(r, timeout=15) as resp:
                raw = resp.read()
                return resp.status, (json.loads(raw) if raw else {})
        except urllib.error.HTTPError as e:
            try:
                return e.code, json.load(e)
            except Exception:
                return e.code, {}

PROC_A = "S9b nghiệm thu — duyệt A"
PROC_B = "S9b nghiệm thu — duyệt B"
KEY = "s9b-" + str(int(time.time()))


def pending_rows(env, rid):
    return int(env.sql(f"SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' "
                       f"AND record_id = '{rid}' AND status = 'pending'")[0][0])


def new_contract(env, tok, name, **extra):
    body = {"name": name, "status": "Nháp"}
    body.update(extra)
    c, b = env.req("POST", "/crm_contracts", tok, body)
    assert c == 201, (c, b)
    return b["data"].get("id") or b["data"].get("record", {}).get("id")


def pending_ids(env, tok):
    c, b = env.req("GET", "/approvals/pending", tok)
    assert c == 200, (c, b)
    return [r.get("recordId") or r.get("RecordID") for r in b["data"]["approvals"]]


def run(env):
    t = env.tokens()
    cached = env.tokens_cache
    procs = env.req("GET", "/admin/approval-processes", t["admin"])[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_contracts" and p["isActive"]]
    assert not rivals, f"another active crm_contracts process would take part: {rivals}"
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", t["admin"])[1]["data"]["users"]}
    d1, d2 = users["duyet1@example.com"], users["duyet2@example.com"]
    c, me = env.req("GET", "/auth/me", t["staff"])
    staff_id = me["data"].get("user", me["data"])["id"]

    made, wfs = [], []
    for name, approver in ((PROC_A, d1), (PROC_B, d2)):
        c, b = env.req("POST", "/admin/approval-processes", t["admin"], {
            "name": name, "entityName": "crm_contracts", "trigger": "manual", "isActive": True,
            "steps": [{"id": "s1", "name": "Duyệt", "approverType": "user", "approverId": approver}]})
        assert c == 201, (c, b)
        made.append(b["data"]["process"]["id"])
    proc_a, proc_b = made
    try:
        # 1 — the sender picks the process.
        c1 = new_contract(env, t["staff"], "S9b-chọn quy trình")
        c, b = env.req("GET", f"/crm_contracts/{c1}/approvals/startable", t["staff"])
        offered = sorted(p["processId"] for p in b.get("data", {}).get("processes", []))
        check("test@ (không-admin) GET startable thấy cả hai quy trình gửi tay", c == 200 and offered == sorted(made), f"{c} {offered}")
        c, b = env.req("POST", f"/crm_contracts/{c1}/approvals/submit", t["staff"], {"processId": proc_b},
                       headers={"Idempotency-Key": KEY})
        first_id = (b.get("data", {}).get("approval") or {}).get("id") or (b.get("data", {}).get("approval") or {}).get("ID")
        check("gửi với processId của B ⇒ 201", c == 201 and bool(first_id), f"{c} {b.get('code')} {b}")
        used = env.sql(f"SELECT process_id FROM sys_approval_instances WHERE id = '{first_id}'")[0][0]
        check("hồ sơ chạy quy trình B (không phải A, dù A khớp trước)", used == proc_b, used)
        check("duyet2 thấy việc, duyet1 KHÔNG thấy", c1 in pending_ids(env, cached["duyet2"]) and c1 not in pending_ids(env, cached["duyet1"]))

        # 2 — idempotency.
        c, b = env.req("POST", f"/crm_contracts/{c1}/approvals/submit", t["staff"], {"processId": proc_b},
                       headers={"Idempotency-Key": KEY})
        again = (b.get("data", {}).get("approval") or {}).get("id") or (b.get("data", {}).get("approval") or {}).get("ID")
        check("gửi lại cùng Idempotency-Key ⇒ 201 cùng hồ sơ", c == 201 and again == first_id, f"{c} {again} vs {first_id}")
        check("CSDL: vẫn đúng một hồ sơ cho hợp đồng", int(env.sql(
            f"SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND record_id = '{c1}'")[0][0]) == 1)
        c2 = new_contract(env, t["staff"], "S9b-hợp đồng khác")
        c, b = env.req("POST", f"/crm_contracts/{c2}/approvals/submit", t["staff"], {"processId": proc_b},
                       headers={"Idempotency-Key": KEY})
        check("cùng khoá, hợp đồng khác ⇒ 422 APPROVAL_IDEMPOTENCY_KEY_REUSED", c == 422 and b.get("code") == "APPROVAL_IDEMPOTENCY_KEY_REUSED", f"{c} {b.get('code')}")
        check("…và không tạo hồ sơ cho hợp đồng khác", pending_rows(env, c2) == 0)
        c, b = env.req("POST", f"/crm_contracts/{c1}/approvals/submit", t["staff"], {"processId": proc_b})
        check("không khoá, đang chờ ⇒ 422 APPROVAL_ALREADY_PENDING", c == 422 and b.get("code") == "APPROVAL_ALREADY_PENDING", f"{c} {b.get('code')}")
        c, b = env.req("POST", f"/crm_contracts/{c2}/approvals/submit", t["staff"], {"processId": "00000000-0000-4000-8000-000000000000"})
        check("processId lạ ⇒ 422 APPROVAL_PROCESS_NOT_APPLICABLE", c == 422 and b.get("code") == "APPROVAL_PROCESS_NOT_APPLICABLE", f"{c} {b.get('code')}")

        # 3 — an after_update rule starts the process.
        c, b = env.req("POST", "/admin/workflows", t["admin"], {
            "name": "S9b nghiệm thu — vừa Hiệu lực", "entityName": "crm_contracts", "trigger": "after_update", "isActive": True,
            "condition": {"rules": [{"field": "status", "op": "changedTo", "value": "Hiệu lực"}]},
            "actions": [{"type": "start_approval", "processId": proc_a}]})
        assert c == 201, (c, b)
        wfs.append(b["data"]["workflow"]["id"])
        c, b = env.req("POST", "/admin/workflows", t["admin"], {
            "name": "S9b nghiệm thu — quy trình ma", "entityName": "crm_contracts", "trigger": "after_update", "isActive": True,
            "actions": [{"type": "start_approval", "processId": "00000000-0000-4000-8000-000000000000"}]})
        check("lưu luật trỏ quy trình không tồn tại ⇒ 422 WF_START_APPROVAL_INVALID", c == 422 and b.get("code") == "WF_START_APPROVAL_INVALID", f"{c} {b.get('code')}")
        c3 = new_contract(env, t["staff"], "S9b-luật sự kiện")
        c, b = env.req("PUT", f"/crm_contracts/{c3}", t["staff"], {"status": "Hiệu lực"})
        assert c == 200, (c, b)
        ok = False
        for _ in range(20):
            if c3 in pending_ids(env, cached["duyet1"]):
                ok = True
                break
            time.sleep(1)
        check("luật after_update: duyet1 thấy dòng mới trên /approvals", ok)
        who = env.sql(f"SELECT created_by, process_id FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND record_id = '{c3}'")
        check("người gửi = test@ (chủ hồ sơ), quy trình = A", bool(who) and who[0] == [staff_id, proc_a], str(who))

        # 4 — a date_offset rule (durable job, scheduler NOT enabled).
        c, b = env.req("POST", "/admin/workflows", t["admin"], {
            "name": "S9b nghiệm thu — sắp hết hạn", "entityName": "crm_contracts", "trigger": "date_offset", "isActive": True,
            "condition": {"timer": {"dateField": "end_date", "offset": {"amount": 1, "unit": "minutes"}, "direction": "before"}},
            "actions": [{"type": "start_approval", "processId": proc_a}]})
        assert c == 201, (c, b)
        wfs.append(b["data"]["workflow"]["id"])
        today = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d")
        c4 = new_contract(env, t["staff"], "S9b-luật date_offset", end_date=today)
        ok = False
        for _ in range(60):
            if c4 in pending_ids(env, cached["duyet1"]):
                ok = True
                break
            time.sleep(1)
        check("luật date_offset: dòng mới trên /approvals của duyet1, không bật scheduler", ok)
        who = env.sql(f"SELECT created_by FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND record_id = '{c4}'")
        check("hồ sơ do luật date_offset mở mang người gửi = test@", bool(who) and who[0][0] == staff_id, str(who))
    finally:
        for wid in wfs:
            c, _ = env.req("DELETE", f"/admin/workflows/{wid}", t["admin"])
            check("dọn: xoá luật nghiệm thu", c in (200, 204), str(c))
        for pid in made:
            c, _ = env.req("DELETE", f"/admin/approval-processes/{pid}", t["admin"])
            check("dọn: xoá quy trình nghiệm thu", c == 204, str(c))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--base", required=True, help="API base of the server running over the clone")
    ap.add_argument("--db", required=True, help="the CLONE database the server runs on")
    a = ap.parse_args()
    if a.db == "nexus_platform":
        sys.exit("refusing the shared dev DB — clone it first (nexus_wt_<slug>)")
    env = Env(a.base, a.db)
    tokens = env.tokens

    def cached():
        t = tokens()
        env.tokens_cache = t
        return t

    env.tokens = cached
    run(env)
    fails = seats_mod.fails
    print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)}): {fails}")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
