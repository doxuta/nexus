#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S3b của specs/043 (`bpm-seat-authority`, DAILY-QUEUE #114, T077).

Bấm duyệt hai lần / hai người bấm cùng lúc KHÔNG bao giờ làm hồ sơ nhảy bước, và quyền duyệt là GHẾ.
Chạy trên server dựng từ DB CLONE (không bao giờ DB dev dùng chung), sau `scripts/fixture-bpm.py`:

  1. Hai tab: duyet1 bấm Duyệt CÙNG LÚC hai lần trên cùng một hồ sơ (8 hồ sơ, mỗi hồ sơ hai luồng chờ
     nhau rồi bắn) ⇒ đúng một 200; luồng kia 409 APPROVAL_CONFLICT (thua cạnh tranh) hoặc 422
     APPROVAL_NOT_AUTHORIZED (đến sau khi bước đã sang duyet2); hồ sơ đứng ở bước 2, đúng MỘT dòng
     «đã duyệt» trong nhật ký, ghế s1 done bởi duyet1, ghế s2 mở cho duyet2.
  2. Uỷ quyền duyet1 → duyet3 giữ hạn (ghế mới cùng due_at); duyet1 bấm duyệt sau khi đã giao ⇒ 422
     APPROVAL_NOT_AUTHORIZED, không đổi gì; duyet3 duyệt được.
  3. Quyền là ghế: hồ sơ phép chiếu (current_approver_*) bị NULL — người ngoài (hotro1, duyet2) 422,
     người giữ ghế (duyet1) duyệt được.
  4. Hồ sơ không ghế (như do binary trước S3a khởi động — xoá ghế THẲNG trong clone): duyet1 duyệt được và
     để lại ghế s1 done/approved bởi duyet1 đúng nút ghim.
  5. Quét quá hạn theo tenant: ghim hạn của một hồ sơ về quá khứ (GHI THẲNG vào clone) ⇒ bộ quét leo thang
     đúng MỘT lần (ghế đóng dấu, nhật ký một mục escalated), người duyệt nhận đúng MỘT chuông «Quá hạn».
     Cần server chạy với WORKFLOW_SCHEDULER_INTERVAL_SEC nhỏ (vd 5); đặt --no-sweep để bỏ bước này.

    python3 scripts/fixture-bpm.py --base http://localhost:8701/api/v1
    python3 scripts/acceptance-bpm-seat-authority.py --base http://localhost:8701/api/v1 --db nexus_wt_bpm_seat_authority

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: stdlib + `docker exec nexus-mysql mysql` (cùng khuôn acceptance-bpm-seats.py, mà script này nạp lại).
"""
import argparse
import importlib.util
import json
import os
import sys
import threading
import time

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check = seats_mod.Env, seats_mod.check

PROC = "S3b nghiệm thu — báo giá 2 bước"


def open_seats(env, iid):
    return [r for r in env.seats(iid) if r[1] == "open"]


def history_actions(env, iid):
    raw = env.sql(f"SELECT history FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{iid}'")[0][0]
    return [h["action"] for h in json.loads(raw)]


def state(env, iid):
    r = env.sql(f"SELECT current_step, status, seq, escalated FROM sys_approval_instances "
                f"WHERE tenant_id = '{env.tenant}' AND id = '{iid}'")[0]
    return {"step": int(r[0]), "status": r[1], "seq": int(r[2]), "escalated": r[3] == "1"}


def two_tabs(env, tok, iid, barrier):
    """Both 'tabs' wait for each other, then fire the same Approve."""
    out = [None, None]

    def tab(i):
        barrier.wait()
        c, b = env.req("POST", f"/approvals/{iid}/approve", tok, {"note": f"tab {i}"})
        out[i] = (c, b.get("code"))

    ts = [threading.Thread(target=tab, args=(i,)) for i in (0, 1)]
    for t in ts:
        t.start()
    for t in ts:
        t.join()
    return out


def run(env, sweep):
    t = env.tokens()
    procs = env.req("GET", "/admin/approval-processes", t["admin"])[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]]
    assert not rivals, f"another active manual crm_quotes process would take the submit: {rivals}"
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", t["admin"])[1]["data"]["users"]}
    d1, d2, d3 = (users[f"duyet{i}@example.com"] for i in (1, 2, 3))
    steps = [{"id": "s1", "name": "Trưởng phòng", "approverType": "user", "approverId": d1, "slaSeconds": 14400},
             {"id": "s2", "name": "Kế toán", "approverType": "user", "approverId": d2}]
    c, b = env.req("POST", "/admin/approval-processes", t["admin"], {
        "name": PROC, "entityName": "crm_quotes", "trigger": "manual", "isActive": True, "steps": steps})
    assert c == 201, (c, b)
    proc = b["data"]["process"]["id"]
    try:
        # 1 — two tabs, one seat.
        outcomes = []
        for n in range(8):
            q = env.send(t["staff"], f"S3b-tabs-{n}")
            res = two_tabs(env, t["duyet1"], q, threading.Barrier(2))
            outcomes.append(res)
            codes = sorted(c for c, _ in res)
            s = state(env, q)
            seats = env.seats(q)
            ok = (codes.count(200) == 1
                  and all(c == 200 or (c, code) in {(409, "APPROVAL_CONFLICT"), (422, "APPROVAL_NOT_AUTHORIZED")} for c, code in res)
                  and s["step"] == 1 and s["status"] == "pending"
                  and history_actions(env, q).count("approved") == 1
                  and seats[0][:3] == ["s1", "done", "approved"] and seats[0][4] == d1
                  and [r for r in seats if r[1] == "open"][0][:1] == ["s2"] and s["seq"] == 3)  # seq: gửi = 2 lần ghi (tạo + «đã gửi»), + đúng MỘT quyết định thắng
            check(f"hai tab #{n}: một 200, tab kia bị từ chối đúng mã, hồ sơ ở bước 2 với một lượt duyệt", ok, f"{res} {s} {seats}")
        n409 = sum(1 for res in outcomes for c, _ in res if c == 409)
        print(f"  (ghi chú: {n409}/8 lượt thua cạnh tranh thật ra 409 APPROVAL_CONFLICT; lượt đến sau khi bước đã sang duyet2 là 422 — cả hai đều đạt)")

        # 2 — delegation keeps the deadline; the delegator can no longer decide.
        q2 = env.send(t["staff"], "S3b-uỷ quyền")
        due = open_seats(env, q2)[0]
        c, _ = env.req("POST", f"/approvals/{q2}/delegate", t["duyet1"], {"toUserId": d3, "note": "nghỉ phép"})
        o = open_seats(env, q2)
        due_now = env.sql(f"SELECT due_at FROM sys_bpm_tasks WHERE tenant_id = '{env.tenant}' AND instance_id = '{q2}' AND status = 'open'")[0][0]
        due_was = env.sql(f"SELECT sla_due_at FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{q2}'")[0][0]
        check("duyet1 uỷ quyền cho duyet3 ⇒ một ghế mở của duyet3 trên s1, GIỮ hạn",
              c == 200 and len(o) == 1 and o[0][3] == d3 and due_now == due_was, f"{c} {o} {due_now} vs {due_was}")
        before = env.seats(q2)
        c, b = env.req("POST", f"/approvals/{q2}/approve", t["duyet1"], {"note": "sau khi đã giao"})
        check("duyet1 duyệt sau khi đã giao ⇒ 422 APPROVAL_NOT_AUTHORIZED, ghế không đổi",
              c == 422 and b.get("code") == "APPROVAL_NOT_AUTHORIZED" and env.seats(q2) == before, f"{c} {b.get('code')}")
        c, b = env.req("POST", f"/approvals/{q2}/approve", t["duyet3"], {"note": "tôi nhận"})
        check("duyet3 (người giữ ghế) duyệt ⇒ sang duyet2", c == 200 and state(env, q2)["step"] == 1, f"{c} {b.get('code')}")

        # 3 — authority is the seat, not the projection.
        q3 = env.send(t["staff"], "S3b-phép chiếu NULL")
        env.sql(f"UPDATE sys_approval_instances SET current_approver_type = NULL, current_approver_id = NULL "
                f"WHERE tenant_id = '{env.tenant}' AND id = '{q3}'")
        for who in ("hotro1", "duyet2"):
            c, b = env.req("POST", f"/approvals/{q3}/approve", t[who], {"note": "x"})
            check(f"phép chiếu NULL: {who} (không giữ ghế) ⇒ 422 APPROVAL_NOT_AUTHORIZED, hồ sơ ở bước 1",
                  c == 422 and b.get("code") == "APPROVAL_NOT_AUTHORIZED" and state(env, q3)["step"] == 0, f"{c} {b.get('code')}")
        c, b = env.req("POST", f"/approvals/{q3}/approve", t["duyet1"], {"note": "ghế của tôi"})
        check("phép chiếu NULL: duyet1 (người giữ ghế) duyệt được ⇒ sang bước 2", c == 200 and state(env, q3)["step"] == 1, f"{c} {b.get('code')}")

        # 4 — seatless instance gets its seat on touch.
        q4 = env.send(t["staff"], "S3b-không ghế")
        env.sql(f"DELETE FROM sys_bpm_tasks WHERE tenant_id = '{env.tenant}' AND instance_id = '{q4}'")
        c, b = env.req("POST", f"/approvals/{q4}/approve", t["duyet1"], {"note": "hồ sơ không ghế"})
        s4 = env.seats(q4)
        node = env.sql(f"SELECT JSON_UNQUOTE(JSON_EXTRACT(process_snapshot, '$.steps[0].id')) FROM sys_approval_instances "
                       f"WHERE tenant_id = '{env.tenant}' AND id = '{q4}'")[0][0]
        check("hồ sơ không ghế: duyet1 duyệt được và ghế s1 được tạo khi chạm tới (done/approved bởi duyet1, đúng nút ghim)",
              c == 200 and s4[0][:3] == [node, "done", "approved"] and s4[0][4] == d1 and open_seats(env, q4)[0][0] == "s2", f"{c} {b.get('code')} {s4}")

        # 5 — tenant-by-tenant overdue sweep escalates once.
        if sweep:
            q5 = env.send(t["staff"], "S3b-quá hạn")
            env.sql(f"UPDATE sys_approval_instances SET sla_due_at = NOW() - INTERVAL 1 HOUR WHERE tenant_id = '{env.tenant}' AND id = '{q5}'")
            env.sql(f"UPDATE sys_bpm_tasks SET due_at = NOW() - INTERVAL 1 HOUR WHERE tenant_id = '{env.tenant}' AND instance_id = '{q5}' AND status = 'open'")
            base_n = int(env.sql(f"SELECT COUNT(*) FROM sys_notifications WHERE tenant_id = '{env.tenant}' AND user_id = '{d1}' AND type = 'due' "
                                 f"AND record_id = (SELECT record_id FROM sys_approval_instances WHERE id = '{q5}')")[0][0])
            deadline = time.time() + 90
            while time.time() < deadline and not state(env, q5)["escalated"]:
                time.sleep(2)
            time.sleep(12)  # a few more sweeps: it must stay at ONE
            s5 = state(env, q5)
            n_due = int(env.sql(f"SELECT COUNT(*) FROM sys_notifications WHERE tenant_id = '{env.tenant}' AND user_id = '{d1}' AND type = 'due' "
                                f"AND record_id = (SELECT record_id FROM sys_approval_instances WHERE id = '{q5}')")[0][0]) - base_n
            stamped = env.sql(f"SELECT COUNT(*) FROM sys_bpm_tasks WHERE tenant_id = '{env.tenant}' AND instance_id = '{q5}' "
                              f"AND status = 'open' AND breach_notified_at IS NOT NULL")[0][0]
            check("quá hạn: bộ quét leo thang đúng MỘT lần (nhật ký một mục, ghế đóng dấu, đúng một chuông «Quá hạn»)",
                  s5["escalated"] and history_actions(env, q5).count("escalated") == 1 and stamped == "1" and n_due == 1,
                  f"{s5} escalated-entries={history_actions(env, q5).count('escalated')} stamped={stamped} due-notices={n_due}")
    finally:
        c, _ = env.req("DELETE", f"/admin/approval-processes/{proc}", t["admin"])
        check("dọn: xoá quy trình nghiệm thu (hồ sơ đã ghim vẫn chạy)", c == 204, str(c))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--base", required=True, help="API base of the server running over the clone")
    ap.add_argument("--db", required=True, help="the CLONE database the server runs on")
    ap.add_argument("--no-sweep", action="store_true", help="skip step 5 (needs a short WORKFLOW_SCHEDULER_INTERVAL_SEC)")
    a = ap.parse_args()
    if a.db == "nexus_platform":
        sys.exit("refusing the shared dev DB — clone it first (nexus_wt_<slug>)")
    run(Env(a.base, a.db), not a.no_sweep)
    fails = seats_mod.fails
    print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)}): {fails}")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
