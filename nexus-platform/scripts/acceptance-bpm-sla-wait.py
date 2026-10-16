#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S12a của specs/043 (`bpm-sla-wait-node`, DAILY-QUEUE #125, T271).

Bước chờ "phản hồi đầu tiên" trên phiếu hỗ trợ (`tk_tickets`): chờ tới khi trạng thái KHÁC `Open`, hạn theo
mức ưu tiên (ma trận), ghi hạn vào trường `first_response_due`, quá hạn thì báo người ở trường `assigned_to`
(tk_agent) — không ghế, không ai "giữ" bước. Chạy trên server dựng từ DB CLONE, sau `scripts/fixture-bpm.py`
(hotro1 vai tk_agent) — không bao giờ DB dev dùng chung.

  1. admin lưu quy trình (ma trận: Khẩn 1 phút, Cao 30 phút, mặc định 4 giờ; notify = assigned_to).
     Thiếu `notify` ⇒ 422 APPROVAL_SLA_NOTIFY_REQUIRED; notify trỏ trường không phải người ⇒ 422;
     `writeDueTo` = `due_date` (mà luật set_deadline của gói đang ghi) ⇒ lưu được + `warnings` có cảnh báo.
  2. hotro1 (tk_agent, không-admin — test@example.com là vai CRM, không có quyền phiếu) tạo phiếu Khẩn A và phiếu Thấp B; admin gán A,B cho hotro1.
     Hồ sơ bước chờ mở cho cả hai, KHÔNG có ghế mở; A có hạn ≈ +1 phút và `first_response_due` nhận đúng hạn đó.
  3. hotro1 (tk_agent, không-admin) đổi B khỏi `Open` trước hạn ⇒ hồ sơ B xong; GET /approvals/:id bằng
     token hotro1 có dòng "wait_met" (Phản hồi đầu tiên — đạt) và durations.perNode[phan-hoi] có giờ làm việc.
  4. A để quá hạn ⇒ hotro1 có đúng MỘT chuông "Quá hạn" (của A); B không; đợi thêm một chu kỳ ⇒ vẫn một;
     hồ sơ A escalated đúng một mục; job bpm_sla_due `done`. Một người ngoài (duyet1) không có chuông.

    python3 scripts/acceptance-bpm-sla-wait.py --base http://localhost:8703/api/v1 --db nexus_wt_bpm_sla_wait_node

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng. Đợi ~2 phút (một phút-lẻ thật).
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

PROC = "S12a nghiệm thu — phản hồi đầu tiên"
DUE_FIELD = "first_response_due"


def graph(sla):
    return {"nodes": [
        {"id": "phan-hoi", "name": "Phản hồi đầu tiên", "kind": "wait",
         "until": {"rules": [{"field": "status", "op": "neq", "value": "Open"}]}, "sla": sla},
        {"id": "xong", "kind": "end", "result": "approved"}]}


def sla(**over):
    base = {"amount": 4, "unit": "hours",
            "matrix": [{"when": {"rules": [{"field": "priority", "op": "eq", "value": "Urgent"}]}, "amount": 1, "unit": "minutes"},
                       {"when": {"rules": [{"field": "priority", "op": "eq", "value": "High"}]}, "amount": 30, "unit": "minutes"}],
            "writeDueTo": DUE_FIELD, "notify": {"type": "field", "field": "assigned_to"}}
    base.update(over)
    return {k: v for k, v in base.items() if v is not None}


def body(g, active=True, name=PROC):
    return {"name": name, "entityName": "tk_tickets", "trigger": "after_create", "isActive": active, "graph": g}


def notices(env, tok):
    c, b = env.req("GET", "/notifications?limit=100", tok)
    assert c == 200, (c, b)
    d = b["data"]
    items = d if isinstance(d, list) else (d.get("notifications") or d.get("items") or [])
    return [n for n in items if (n.get("title") or n.get("Title") or "") == "Quá hạn"]


def new_ticket(env, tok, subject, priority):
    c, b = env.req("POST", "/tk_tickets", tok, {"subject": subject, "priority": priority, "status": "Open"})
    assert c == 201, (c, b)
    return b["data"].get("id") or b["data"].get("record", {}).get("id")


def instance_of(env, rid, timeout=20):
    end = time.time() + timeout
    while time.time() < end:
        rows = env.sql(f"SELECT id, status, IFNULL(sla_due_at, ''), escalated, seq FROM sys_approval_instances "
                       f"WHERE tenant_id = '{env.tenant}' AND record_id = '{rid}' ORDER BY created_at DESC LIMIT 1")
        if rows:
            return rows[0]
        time.sleep(1)
    return None


def main(env):
    t = env.tokens()
    adm, staff, agent, outsider = t["admin"], t["staff"], t["hotro1"], t["duyet1"]
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", adm)[1]["data"]["users"]}
    agent_id = users["hotro1@example.com"]

    # the deadline field: a datetime of its own, so the ticketing pack's set_deadline rules do not fight it
    c, b = env.req("GET", "/metadata/fields/tk_tickets", adm)
    have = {f["name"] for f in (b["data"] if isinstance(b["data"], list) else b["data"].get("fields", []))}
    if DUE_FIELD not in have:
        c, b = env.req("POST", "/admin/fields", adm, {"entityName": "tk_tickets", "name": DUE_FIELD,
                                                       "label": "Hạn phản hồi đầu tiên", "fieldType": "datetime"})
        check("thêm trường hạn first_response_due ⇒ 201", c in (200, 201), f"{c} {b.get('code', '')}")

    # (1) the API
    c, b = env.req("POST", "/admin/approval-processes", adm, body(graph(sla(notify=None))))
    check("sla thiếu notify ⇒ 422 APPROVAL_SLA_NOTIFY_REQUIRED", c == 422 and b.get("code") == "APPROVAL_SLA_NOTIFY_REQUIRED", f"{c} {b.get('code')}")
    c, b = env.req("POST", "/admin/approval-processes", adm, body(graph(sla(notify={"type": "field", "field": "subject"}))))
    check("notify trỏ trường không phải người ⇒ 422 APPROVAL_ASSIGNEE_FIELD_INVALID", c == 422 and b.get("code") == "APPROVAL_ASSIGNEE_FIELD_INVALID", f"{c} {b.get('code')}")
    c, b = env.req("POST", "/admin/approval-processes", adm, body(graph(sla(writeDueTo="subject"))))
    check("writeDueTo không phải ngày giờ ⇒ 422 APPROVAL_SLA_TARGET_FIELD_INVALID", c == 422 and b.get("code") == "APPROVAL_SLA_TARGET_FIELD_INVALID", f"{c} {b.get('code')}")
    c, b = env.req("POST", "/admin/approval-processes", adm, body(graph(sla(writeDueTo="due_date")), active=False, name=PROC + " (xung đột)"))
    check("writeDueTo = due_date (luật set_deadline cũng ghi) ⇒ 201 kèm cảnh báo", c == 201 and len(b.get("data", {}).get("warnings", [])) >= 1,
          f"{c} {b.get('data', {}).get('warnings')}")
    if c == 201:
        env.req("DELETE", f"/admin/approval-processes/{b['data']['process']['id']}", adm)

    c, b = env.req("POST", "/admin/approval-processes", adm, body(graph(sla())))
    check("quy trình hợp lệ ⇒ 201, không cảnh báo", c == 201 and b["data"]["warnings"] == [], f"{c} {b.get('code')} {b.get('data', {}).get('warnings')}")
    assert c == 201, (c, b)
    proc = b["data"]["process"]["id"]
    try:
        # (2) two tickets by a non-admin, assigned to the agent
        a_id = new_ticket(env, agent, "S12a-A khẩn", "Urgent")
        b_id = new_ticket(env, agent, "S12a-B thấp", "Low")
        ia, ib = instance_of(env, a_id), instance_of(env, b_id)
        check("mỗi phiếu mở một hồ sơ bước chờ", ia is not None and ib is not None, f"{ia} {ib}")
        for rid in (a_id, b_id):
            c, _ = env.req("PUT", f"/tk_tickets/{rid}", adm, {"assigned_to": agent_id})
            check("admin gán phiếu cho hotro1 ⇒ 200", c == 200, str(c))
        time.sleep(2)
        ia, ib = instance_of(env, a_id), instance_of(env, b_id)
        opened = env.sql(f"SELECT COUNT(*) FROM sys_bpm_tasks WHERE tenant_id = '{env.tenant}' AND status = 'open' AND instance_id IN ('{ia[0]}','{ib[0]}')")[0][0]
        check("bước chờ không có ghế mở", opened == "0", opened)
        check("hồ sơ A/B đang chờ, A có hạn, chưa quá hạn", ia[1] == "pending" and ib[1] == "pending" and ia[2] != "" and ia[3] == "0", f"{ia} {ib}")
        c, rec = env.req("GET", f"/tk_tickets/{a_id}", adm)
        data = rec["data"].get("record", rec["data"])
        got = (data.get(DUE_FIELD) or data.get("data", {}).get(DUE_FIELD) or "")
        due_sql = env.sql(f"SELECT DATE_FORMAT(sla_due_at, '%Y-%m-%d %H:%i:%s') FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{ia[0]}'")[0][0]
        check("trường first_response_due của A nhận đúng hạn của hồ sơ", got != "" and got[:16].replace("T", " ") == due_sql[:16], f"{got} vs {due_sql}")

        # (3) the agent answers B in time
        c, _ = env.req("PUT", f"/tk_tickets/{b_id}", agent, {"status": "Pending"})
        check("hotro1 (tk_agent) đổi B khỏi Open ⇒ 200", c == 200, str(c))
        end = time.time() + 20
        while time.time() < end:
            ib = instance_of(env, b_id)
            if ib[1] == "approved":
                break
            time.sleep(1)
        check("hồ sơ B xong ngay khi phiếu có phản hồi", ib[1] == "approved", str(ib))
        c, det = env.req("GET", f"/approvals/{ib[0]}", agent)
        d = det.get("data", {})
        d = d.get("approval", d)
        hist = [h.get("action") for h in d.get("history", [])]
        node = next((n for n in (d.get("durations") or {}).get("perNode", []) if n.get("node") == "phan-hoi"), None)
        check("hotro1 (không-admin) đọc được chi tiết: có dòng wait_met, không lộ dấu nghỉ nội bộ",
              c == 200 and "wait_met" in hist and "waiting" not in hist, f"{c} {hist}")
        check("durations có đoạn 'Phản hồi đầu tiên' đã đóng, đo trên lịch làm việc",
              node is not None and node.get("endedAt") and node.get("wallSeconds") is not None, json.dumps(node)[:200])

        # (4) A is left to breach
        end = time.time() + 150
        while time.time() < end:
            ia = instance_of(env, a_id)
            if ia[3] == "1":
                break
            time.sleep(3)
        check("hồ sơ A quá hạn ⇒ escalated", ia[3] == "1", str(ia))
        time.sleep(3)
        n = notices(env, agent)
        mine = [x for x in n if (x.get("recordId") or x.get("record_id") or x.get("RecordID")) == a_id]
        check("hotro1 có đúng MỘT chuông «Quá hạn» của phiếu A", len(mine) == 1, f"{len(mine)} / tổng {len(n)}")
        check("không chuông quá hạn nào cho phiếu B (đã phản hồi)", not [x for x in n if (x.get("recordId") or x.get("record_id") or x.get("RecordID")) == b_id])
        check("duyet1 (ngoài cuộc) không có chuông quá hạn", len(notices(env, outsider)) == 0)
        hist = json.loads(env.sql(f"SELECT history FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{ia[0]}'")[0][0])
        check("A: đúng một mục lịch sử «escalated»", sum(1 for h in hist if h["action"] == "escalated") == 1, str([h["action"] for h in hist]))
        jobs = env.sql(f"SELECT status FROM sys_jobs WHERE tenant_id = '{env.tenant}' AND job_type = 'bpm_sla_due' AND payload->>'$.instanceId' = '{ia[0]}'")
        check("mọi job bpm_sla_due của A đã done", jobs and all(j == ["done"] for j in jobs), str(jobs))
        time.sleep(20)
        check("đợi thêm một chu kỳ ⇒ vẫn đúng một chuông", len([x for x in notices(env, agent) if (x.get("recordId") or x.get("record_id") or x.get("RecordID")) == a_id]) == 1)
        # a late answer still closes the wait
        env.req("PUT", f"/tk_tickets/{a_id}", agent, {"status": "Pending"})
        time.sleep(4)
        check("trả lời muộn vẫn đóng bước chờ của A", instance_of(env, a_id)[1] == "approved", str(instance_of(env, a_id)))
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
