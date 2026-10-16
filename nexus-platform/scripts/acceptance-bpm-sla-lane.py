#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S12b của specs/043 (`bpm-sla-lane`, DAILY-QUEUE #126, T191 + T275).

Làn theo dõi SLA chạy CẠNH hồ sơ duyệt trên cùng phiếu `tk_tickets`; nhân viên `hotro1` (vai tk_agent,
không-admin) thấy SLA của phiếu qua `GET /tk_tickets/:id/approvals?lane=independent`. Chạy trên server dựng từ
DB CLONE, sau `scripts/fixture-bpm.py` — không bao giờ DB dev dùng chung.

  1. API: làn theo dõi có bước người ⇒ 422 APPROVAL_LANE_HUMAN_NODE; trigger manual ⇒ 422; lane lạ ⇒ 422.
  2. hotro1 tạo phiếu ⇒ có hồ sơ làn `independent`; `?lane=independent` (token hotro1) trả mục
     "Phản hồi đầu tiên" có hạn + căn cứ `business`; mặc định (không ?lane) KHÔNG có mục đó.
  3. cùng phiếu hotro1 bấm "Gửi duyệt" được (201) — tab Duyệt có đúng 1 hồ sơ, `?lane=independent` vẫn 1 mục.
  4. hotro1 thu hồi mục theo dõi ⇒ 422 APPROVAL_TRACKING_NOT_RECALLABLE; hồ sơ vẫn pending.
  5. hotro1 đổi phiếu khỏi Open ⇒ mục theo dõi `met = true`; phiếu không tồn tại ⇒ 404; ?lane=bogus ⇒ 400.

    python3 scripts/acceptance-bpm-sla-lane.py --base http://localhost:8700/api/v1 --db nexus_wt_bpm_sla_lane
"""
import argparse
import importlib.util
import os
import sys
import time

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check = seats_mod.Env, seats_mod.check

NAME_SLA = "S12b nghiệm thu — SLA phiếu (làn theo dõi)"
NAME_AP = "S12b nghiệm thu — duyệt phiếu"
OPT = {"lane": "independent"}


def wait_graph():
    return {"nodes": [
        {"id": "phan-hoi", "name": "Phản hồi đầu tiên", "kind": "wait",
         "until": {"rules": [{"field": "status", "op": "neq", "value": "Open"}]},
         "sla": {"amount": 4, "unit": "business_hours", "notify": {"type": "field", "field": "assigned_to"}}},
        {"id": "xong", "kind": "end", "result": "approved"}]}


def main(env):
    t = env.tokens()
    adm, agent = t["admin"], t["hotro1"]
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", adm)[1]["data"]["users"]}
    approver = users["duyet1@example.com"]

    human = {"nodes": [{"id": "a", "name": "Duyệt", "kind": "approval", "approverType": "user", "approverId": approver}]}
    base = {"entityName": "tk_tickets", "trigger": "after_create", "options": OPT}
    c, b = env.req("POST", "/admin/approval-processes", adm, {**base, "name": NAME_SLA + " (người)", "graph": human})
    check("làn theo dõi có bước người ⇒ 422 APPROVAL_LANE_HUMAN_NODE", c == 422 and b.get("code") == "APPROVAL_LANE_HUMAN_NODE", f"{c} {b.get('code')}")
    c, b = env.req("POST", "/admin/approval-processes", adm, {**base, "name": NAME_SLA + " (tay)", "trigger": "manual", "graph": wait_graph()})
    check("làn theo dõi trigger manual ⇒ 422 APPROVAL_OPTIONS_INVALID", c == 422 and b.get("code") == "APPROVAL_OPTIONS_INVALID", f"{c} {b.get('code')}")
    c, b = env.req("POST", "/admin/approval-processes", adm, {**base, "name": NAME_SLA + " (lạ)", "options": {"lane": "sideways"}, "graph": wait_graph()})
    check("lane lạ ⇒ 422 APPROVAL_OPTIONS_INVALID", c == 422 and b.get("code") == "APPROVAL_OPTIONS_INVALID", f"{c} {b.get('code')}")

    procs = []
    try:
        c, b = env.req("POST", "/admin/approval-processes", adm, {**base, "name": NAME_SLA, "graph": wait_graph()})
        check("quy trình làn theo dõi hợp lệ ⇒ 201", c == 201, f"{c} {b.get('code')} {b.get('error')}")
        assert c == 201, b
        procs.append(b["data"]["process"]["id"])
        c, b = env.req("POST", "/admin/approval-processes", adm, {
            "name": NAME_AP, "entityName": "tk_tickets", "trigger": "manual",
            "steps": [{"approverType": "user", "approverId": approver}]})
        check("quy trình duyệt tay trên cùng thực thể ⇒ 201", c == 201, f"{c} {b.get('code')} {b.get('error')}")
        assert c == 201, b
        procs.append(b["data"]["process"]["id"])

        c, b = env.req("POST", "/tk_tickets", agent, {"subject": "S12b nghiệm thu", "priority": "High", "status": "Open"})
        assert c == 201, (c, b)
        rid = b["data"].get("id") or b["data"].get("record", {}).get("id")
        time.sleep(2)
        row = env.sql(f"SELECT lane, status FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND record_id = '{rid}'")
        check("phiếu mới có đúng một hồ sơ làn independent, đang chờ", row == [["independent", "pending"]], str(row))

        c, b = env.req("GET", f"/tk_tickets/{rid}/approvals?lane=independent", agent)
        items = b.get("data", {}).get("instances", []) if c == 200 else []
        it = items[0] if items else {}
        sla = it.get("sla", {})
        check("hotro1 (không-admin) đọc ?lane=independent ⇒ 200, 1 mục «Phản hồi đầu tiên»", c == 200 and len(items) == 1 and it.get("stepName") == "Phản hồi đầu tiên", f"{c} {items}")
        check("mục có hạn + căn cứ giờ làm việc, chưa đạt/quá hạn/dừng",
              bool(sla.get("dueAt")) and sla.get("basis") == "business" and not sla.get("met") and not sla.get("breached") and sla.get("paused") is False, str(sla))
        c, b = env.req("GET", f"/tk_tickets/{rid}/approvals", agent)
        check("mặc định (không ?lane) KHÔNG có mục theo dõi", c == 200 and b["data"]["instances"] == [], f"{c} {b.get('data')}")

        c, b = env.req("POST", f"/tk_tickets/{rid}/approvals/submit", agent, {"processId": procs[1]})
        check("cùng phiếu hotro1 bấm «Gửi duyệt» ⇒ 201", c == 201, f"{c} {b.get('code')} {b.get('error')}")
        c, b = env.req("GET", f"/tk_tickets/{rid}/approvals", agent)
        check("tab Duyệt có đúng 1 hồ sơ", c == 200 and len(b["data"]["instances"]) == 1, f"{c} {len(b.get('data', {}).get('instances', []))}")
        c, b = env.req("GET", f"/tk_tickets/{rid}/approvals?lane=independent", agent)
        check("?lane=independent vẫn đúng 1 mục (hồ sơ duyệt không lẫn vào)", c == 200 and len(b["data"]["instances"]) == 1, f"{c}")

        sla_id = it.get("id")
        c, b = env.req("POST", f"/approvals/{sla_id}/recall", agent, {})
        check("thu hồi mục theo dõi ⇒ 422 APPROVAL_TRACKING_NOT_RECALLABLE", c == 422 and b.get("code") == "APPROVAL_TRACKING_NOT_RECALLABLE", f"{c} {b.get('code')}")
        st = env.sql(f"SELECT status FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{sla_id}'")
        check("hồ sơ theo dõi vẫn pending", st == [["pending"]], str(st))

        c, _ = env.req("PUT", f"/tk_tickets/{rid}", agent, {"status": "Pending"})
        check("hotro1 đổi phiếu khỏi Open ⇒ 200", c == 200, str(c))
        met = False
        for _ in range(15):
            c, b = env.req("GET", f"/tk_tickets/{rid}/approvals?lane=independent", agent)
            m = (b.get("data", {}).get("instances") or [{}])[0]
            if m.get("sla", {}).get("met"):
                met = True
                break
            time.sleep(1)
        check("mục theo dõi chuyển met=true, status approved", met and m.get("status") == "approved", str(m))

        c, _ = env.req("GET", "/tk_tickets/00000000-0000-0000-0000-000000000000/approvals?lane=independent", agent)
        check("phiếu không tồn tại ⇒ 404", c == 404, str(c))
        c, b = env.req("GET", f"/tk_tickets/{rid}/approvals?lane=bogus", agent)
        check("?lane=bogus ⇒ 400 APPROVAL_LANE_INVALID", c == 400 and b.get("code") == "APPROVAL_LANE_INVALID", f"{c} {b.get('code')}")
    finally:
        for pid in procs:
            env.req("DELETE", f"/admin/approval-processes/{pid}", adm)


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
