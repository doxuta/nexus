#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S3c của specs/043 (`bpm-sla-due-durable`, DAILY-QUEUE #115, T255).

Báo quá hạn duyệt chạy bằng JOB BỀN (`bpm_sla_due`), không cần bộ quét. Chạy trên server dựng từ DB
CLONE với `WORKFLOW_SCHEDULER_INTERVAL_SEC=0` (bộ quét TẮT — chỉ job báo được), sau `scripts/fixture-bpm.py`.
Hai pha, để người chạy khởi động lại server GIỮA hai pha:

  --phase arm     tạo quy trình bước 1 SLA ngắn giao VAI crm_sales_manager; test@example.com gửi hồ sơ;
                  in id hồ sơ ra file trạng thái; kiểm job `bpm_sla_due` đã xếp hàng đúng một.
  (khởi động lại server khi job chưa tới hạn)
  --phase verify  đợi tới quá hạn; duyet1, duyet2, duyet3 (không-admin) mỗi người đúng MỘT chuông
                  «Quá hạn phê duyệt» qua GET /notifications; hotro1 (ngoài vai) không có; hồ sơ
                  escalated đúng một mục lịch sử; job `done`; đợi thêm một chu kỳ hàng đợi ⇒ vẫn một.

    python3 scripts/acceptance-bpm-sla-due.py --base http://localhost:8702/api/v1 --db nexus_wt_bpm_sla_due_durable --phase arm

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.
"""
import argparse
import importlib.util
import json
import os
import sys
import time

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check = seats_mod.Env, seats_mod.check

PROC = "S3c nghiệm thu — báo giá quá hạn vai"
STATE = "/tmp/acceptance-bpm-sla-due.state.json"
SLA_SECONDS = 45


def breach_notices(env, tok):
    c, b = env.req("GET", "/notifications?limit=100", tok)
    assert c == 200, (c, b)
    d = b["data"]
    items = d if isinstance(d, list) else (d.get("notifications") or d.get("items") or [])
    return [n for n in items if "Quá hạn" in (n.get("title") or n.get("Title") or "")]


def arm(env):
    t = env.tokens()
    roles = {r["name"]: r["id"] for r in env.req("GET", "/admin/roles", t["admin"])[1]["data"]["roles"]}
    c, b = env.req("POST", "/admin/approval-processes", t["admin"], {
        "name": PROC, "entityName": "crm_quotes", "trigger": "manual", "isActive": True,
        "steps": [{"id": "s1", "name": "Quản lý", "approverType": "role", "approverId": roles["crm_sales_manager"],
                   "slaSeconds": SLA_SECONDS, "slaClock": "wall"}]})
    assert c == 201, (c, b)
    proc = b["data"]["process"]["id"]
    iid = env.send(t["staff"], "S3c-quá hạn")
    seq = env.sql(f"SELECT seq FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{iid}'")[0][0]
    jobs = env.sql(f"SELECT status, payload->>'$.seq' FROM sys_jobs WHERE tenant_id = '{env.tenant}' AND job_type = 'bpm_sla_due' "
                   f"AND payload->>'$.instanceId' = '{iid}'")
    cur = [j for j in jobs if j[1] == seq]
    # gửi = hai lần ghi (tạo seq 1 + «đã gửi» seq 2): job seq cũ vô hại (handler bỏ qua), job seq hiện tại đúng một
    check("gửi hồ sơ ⇒ đúng MỘT job bpm_sla_due xếp hàng cho seq hiện tại", len(cur) == 1 and cur[0][0] == "queued", f"seq={seq} {jobs}")
    json.dump({"iid": iid, "proc": proc}, open(STATE, "w"))
    print(f"  hồ sơ {iid}; khởi động lại server rồi chạy --phase verify sau ≥ {SLA_SECONDS}s")


def verify(env):
    st = json.load(open(STATE))
    iid = st["iid"]
    t = env.tokens()
    deadline = time.time() + SLA_SECONDS + 120
    while time.time() < deadline:
        esc = env.sql(f"SELECT escalated FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{iid}'")[0][0]
        if esc == "1":
            break
        time.sleep(3)
    try:
        for who in ("duyet1", "duyet2", "duyet3"):
            n = breach_notices(env, t[who])
            check(f"{who} (không-admin) nhận đúng MỘT chuông «Quá hạn phê duyệt»", len(n) == 1, f"{len(n)}")
        check("hotro1 (ngoài vai) không nhận chuông quá hạn", len(breach_notices(env, t["hotro1"])) == 0)
        hist = json.loads(env.sql(f"SELECT history FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{iid}'")[0][0])
        check("hồ sơ escalated, đúng một mục lịch sử «escalated»", sum(1 for h in hist if h["action"] == "escalated") == 1, str(hist))
        jobs = env.sql(f"SELECT status FROM sys_jobs WHERE tenant_id = '{env.tenant}' AND job_type = 'bpm_sla_due' "
                       f"AND payload->>'$.instanceId' = '{iid}'")
        check("mọi job bpm_sla_due của hồ sơ đã done", jobs and all(j == ["done"] for j in jobs), str(jobs))
        time.sleep(15)
        again = [len(breach_notices(env, t[w])) for w in ("duyet1", "duyet2", "duyet3")]
        check("đợi thêm một chu kỳ ⇒ vẫn đúng một chuông mỗi người", again == [1, 1, 1], str(again))
    finally:
        pid = st.get("proc")
        t2 = env.tokens()
        env.req("DELETE", f"/admin/approval-processes/{pid}", t2["admin"])


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--db", required=True)
    ap.add_argument("--phase", choices=["arm", "verify"], required=True)
    a = ap.parse_args()
    if a.db == "nexus_platform":
        sys.exit("refusing the shared dev DB — clone it first (nexus_wt_<slug>)")
    env = Env(a.base, a.db)
    arm(env) if a.phase == "arm" else verify(env)
    print("KẾT QUẢ:", "ĐẠT" if not seats_mod.fails else f"CHƯA ĐẠT ({len(seats_mod.fails)}): {seats_mod.fails}")
    sys.exit(1 if seats_mod.fails else 0)
