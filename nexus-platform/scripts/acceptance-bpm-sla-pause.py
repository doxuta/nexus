#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S13 của specs/043 (`bpm-sla-pause-calendar`, DAILY-QUEUE #127, T201).

Mẫu "SLA phiếu" trên `tk_tickets` (làn theo dõi): bước chờ tới khi phiếu `Resolved`, hạn theo ưu tiên
(Urgent 1 phút, còn lại 5 phút — đơn vị phút đồng hồ để quan sát kịp), `options.pauseWhen` = status `Pending`
("Chờ xử lý" — chờ khách). Nhân viên `hotro1` (vai tk_agent, không-admin) đọc SLA qua
`GET /tk_tickets/:id/approvals?lane=independent`. Chạy trên server dựng từ DB CLONE sau `scripts/fixture-bpm.py`.

  1. API: pauseWhen không quy tắc / calendarId có khoảng trắng ⇒ 422 APPROVAL_OPTIONS_INVALID; pauseWhen đọc
     trường lạ ⇒ 422 APPROVAL_FIELD_UNKNOWN; hạn business_hours quá lớn cho lịch ⇒ 422 APPROVAL_SLA_AMOUNT_TOO_LARGE.
  2. Phiếu A (Low, 5 phút): đổi sang Pending ⇒ mục SLA `paused=true`, `dueAt` rỗng, còn lại ≈ 5 phút; ghế/thời
     gian đóng băng qua ~70 giây; đổi lại Open ⇒ `paused=false`, hạn mới ≈ lúc tiếp tục + phần còn lại
     (lùi đúng bằng khoảng đã dừng, ±15 giây).
  3. Phiếu U (Urgent, 1 phút): Pending ngay, để qua 1 phút rưỡi ⇒ KHÔNG escalated, KHÔNG chuông; tiếp tục ⇒
     quá hạn ⇒ đúng MỘT chuông «Quá hạn» cho hotro1; người ngoài (duyet1) không có chuông.

    python3 scripts/acceptance-bpm-sla-pause.py --base http://localhost:8701/api/v1 --db nexus_wt_bpm_sla_pause_calendar

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng. Chạy ~4 phút (thời gian thật).
"""
import argparse
import importlib.util
import os
import sys
import time
from datetime import datetime

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check = seats_mod.Env, seats_mod.check

PROC = "S13 nghiệm thu — SLA phiếu có tạm dừng"
PAUSE = {"rules": [{"field": "status", "op": "eq", "value": "Pending"}]}


def graph(amount_base=5, unit="minutes"):
    return {"nodes": [
        {"id": "xu-ly", "name": "Giải quyết", "kind": "wait",
         "until": {"rules": [{"field": "status", "op": "eq", "value": "Resolved"}]},
         "sla": {"amount": amount_base, "unit": unit,
                 "matrix": [{"when": {"rules": [{"field": "priority", "op": "eq", "value": "Urgent"}]}, "amount": 1, "unit": "minutes"}],
                 "notify": {"type": "field", "field": "assigned_to"}}},
        {"id": "xong", "kind": "end", "result": "approved"}]}


def body(opts, g=None, name=PROC):
    return {"name": name, "entityName": "tk_tickets", "trigger": "after_create", "isActive": True,
            "options": opts, "graph": g or graph()}


def new_ticket(env, tok, subject, priority):
    c, b = env.req("POST", "/tk_tickets", tok, {"subject": subject, "priority": priority, "status": "Open"})
    assert c == 201, (c, b)
    return b["data"].get("id") or b["data"].get("record", {}).get("id")


def assign(env, adm, rid, agent_id):
    """Assign after the pack's own auto-assignment rule has run, and prove it stuck."""
    time.sleep(3)
    c, _ = env.req("PUT", f"/tk_tickets/{rid}", adm, {"assigned_to": agent_id})
    time.sleep(2)
    c2, rec = env.req("GET", f"/tk_tickets/{rid}", adm)
    data = rec["data"].get("record", rec["data"])
    got = data.get("assigned_to") or data.get("data", {}).get("assigned_to")
    check("phiếu được gán cho hotro1", c == 200 and got == agent_id, f"{c} {got}")


def sla_item(env, tok, rid, timeout=20):
    end = time.time() + timeout
    while time.time() < end:
        c, b = env.req("GET", f"/tk_tickets/{rid}/approvals?lane=independent", tok)
        items = b.get("data", {}).get("instances", []) if c == 200 else []
        sla = (items[0].get("sla") or {}) if items else {}
        if sla.get("dueAt") or sla.get("paused"):  # the walk has rested and stamped the clock
            return sla
        time.sleep(1)
    return None


def ts(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


def bells(env, tok, rid):
    c, b = env.req("GET", "/notifications?limit=100", tok)
    d = b["data"]
    items = d if isinstance(d, list) else (d.get("notifications") or d.get("items") or [])
    return [n for n in items if (n.get("title") or n.get("Title") or "") == "Quá hạn"
            and (n.get("recordId") or n.get("record_id") or n.get("RecordID")) == rid]


def wait_for(fn, timeout):
    end = time.time() + timeout
    while time.time() < end:
        v = fn()
        if v:
            return v
        time.sleep(2)
    return None


def main(env):
    t = env.tokens()
    adm, agent, outsider = t["admin"], t["hotro1"], t["duyet1"]
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", adm)[1]["data"]["users"]}
    agent_id = users["hotro1@example.com"]
    base = {"lane": "independent"}

    # (1) the API
    c, b = env.req("POST", "/admin/approval-processes", adm, body({**base, "pauseWhen": {"rules": []}}, name=PROC + " (rỗng)"))
    check("pauseWhen không quy tắc ⇒ 422 APPROVAL_OPTIONS_INVALID", c == 422 and b.get("code") == "APPROVAL_OPTIONS_INVALID", f"{c} {b.get('code')}")
    c, b = env.req("POST", "/admin/approval-processes", adm, body({**base, "calendarId": " cal"}, name=PROC + " (lịch)"))
    check("calendarId có khoảng trắng ⇒ 422 APPROVAL_OPTIONS_INVALID", c == 422 and b.get("code") == "APPROVAL_OPTIONS_INVALID", f"{c} {b.get('code')}")
    c, b = env.req("POST", "/admin/approval-processes", adm,
                   body({**base, "pauseWhen": {"rules": [{"field": "khong_co", "op": "eq", "value": "x"}]}}, name=PROC + " (trường)"))
    check("pauseWhen đọc trường lạ ⇒ 422 APPROVAL_FIELD_UNKNOWN", c == 422 and b.get("code") == "APPROVAL_FIELD_UNKNOWN", f"{c} {b.get('code')}")
    c, b = env.req("POST", "/admin/approval-processes", adm, body(base, g=graph(100000, "business_hours"), name=PROC + " (lớn)"))
    check("hạn business_hours 100000 giờ ⇒ 422 APPROVAL_SLA_AMOUNT_TOO_LARGE hoặc 422 vượt trần",
          c == 422, f"{c} {b.get('code')}")

    c, b = env.req("POST", "/admin/approval-processes", adm, body({**base, "pauseWhen": PAUSE}))
    check("quy trình hợp lệ có pauseWhen ⇒ 201", c == 201, f"{c} {b.get('code')} {b.get('error')}")
    assert c == 201, (c, b)
    proc = b["data"]["process"]["id"]
    try:
        # (2) ticket A: pause, wait, resume
        a_id = new_ticket(env, agent, "S13-A thấp", "Low")
        assign(env, adm, a_id, agent_id)
        s0 = sla_item(env, agent, a_id)
        check("A: có mục SLA đang chạy (hạn, chưa tạm dừng)", s0 and s0.get("dueAt") and not s0.get("paused"), str(s0))
        due0 = ts(s0["dueAt"])
        c, _ = env.req("PUT", f"/tk_tickets/{a_id}", agent, {"status": "Pending"})
        check("hotro1 đổi A sang Pending ⇒ 200", c == 200, str(c))
        paused = wait_for(lambda: (lambda s: s if s and s.get("paused") else None)(sla_item(env, agent, a_id, 3)), 20)
        check("A: mục SLA paused=true, không còn dueAt, còn lại ≈ 5 phút",
              paused is not None and not paused.get("dueAt") and 200 <= (paused.get("remainingSeconds") or 0) <= 300, str(paused))
        t_pause = time.time()
        time.sleep(70)
        still = sla_item(env, agent, a_id, 3)
        check("A: qua 70 giây vẫn đóng băng (cùng số giây còn lại)", still and still.get("paused") and still.get("remainingSeconds") == paused.get("remainingSeconds"), str(still))
        c, _ = env.req("PUT", f"/tk_tickets/{a_id}", agent, {"status": "Open"})
        check("hotro1 đổi A lại Open ⇒ 200", c == 200, str(c))
        t_resume = time.time()
        resumed = wait_for(lambda: (lambda s: s if s and not s.get("paused") and s.get("dueAt") else None)(sla_item(env, agent, a_id, 3)), 20)
        check("A: tiếp tục ⇒ paused=false, có hạn mới", resumed is not None, str(resumed))
        if resumed:
            shift = ts(resumed["dueAt"]) - due0
            gap = t_resume - t_pause
            check(f"hạn lùi đúng khoảng đã dừng (lùi {shift:.0f}s, dừng {gap:.0f}s, ±15s)", abs(shift - gap) <= 15, f"{shift} vs {gap}")
        # the history carries both lines for the people who may read it? (internal: only the admin sees them)
        rows = env.sql(f"SELECT history FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND record_id = '{a_id}' ORDER BY created_at DESC LIMIT 1")
        hist = rows[0][0] if rows else ""
        check("lịch sử có dòng paused và resumed", '"paused"' in hist and '"resumed"' in hist, hist[:200])

        # (3) ticket U: breach is silenced while paused, rings once after resume
        u_id = new_ticket(env, agent, "S13-U khẩn", "Urgent")
        assign(env, adm, u_id, agent_id)
        sla_item(env, agent, u_id)
        env.req("PUT", f"/tk_tickets/{u_id}", agent, {"status": "Pending"})
        wait_for(lambda: (lambda s: s if s and s.get("paused") else None)(sla_item(env, agent, u_id, 3)), 20)
        time.sleep(95)  # past the original one minute
        rows = env.sql(f"SELECT escalated FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND record_id = '{u_id}' ORDER BY created_at DESC LIMIT 1")
        check("U: qua hạn gốc khi đang dừng ⇒ KHÔNG escalated", rows and rows[0][0] == "0", str(rows))
        check("U: đang dừng ⇒ KHÔNG chuông «Quá hạn»", len(bells(env, agent, u_id)) == 0)
        env.req("PUT", f"/tk_tickets/{u_id}", agent, {"status": "Open"})
        fired = wait_for(lambda: len(bells(env, agent, u_id)) >= 1, 120)
        check("U: tiếp tục rồi quá hạn ⇒ có chuông «Quá hạn»", bool(fired))
        time.sleep(15)
        check("U: đúng MỘT chuông", len(bells(env, agent, u_id)) == 1, str(len(bells(env, agent, u_id))))
        check("duyet1 (ngoài cuộc) không có chuông", len(bells(env, outsider, u_id)) == 0)
    finally:
        env.req("DELETE", f"/admin/approval-processes/{proc}", adm)


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--db", required=True)
    a = ap.parse_args()
    if a.db == "nexus_platform":
        sys.exit("refusing the shared dev DB — clone it first (nexus_wt_<slug>)")
    main(Env(a.base, a.db))
    print("KẾT QUẢ:", "ĐẠT" if not seats_mod.fails else f"CHƯA ĐẠT ({len(seats_mod.fails)}): {seats_mod.fails}")
    sys.exit(1 if seats_mod.fails else 0)
