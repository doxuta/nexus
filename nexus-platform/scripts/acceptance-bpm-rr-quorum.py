#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S6b của specs/043 (`bpm-routing-rr-quorum`, DAILY-QUEUE #119, T261).

Chia việc cho NHÓM theo chiến lược và duyệt theo quorum % — bằng token NGƯỜI THẬT không-admin (duyet1..3 vai
crm_sales_manager, hotro1 vai tk_agent, test@example.com nhân viên). Chạy trên server dựng từ DB CLONE (không bao giờ DB
dev dùng chung), sau `scripts/fixture-bpm.py`. Mỗi kịch bản dựng MỘT quy trình gửi-tay đang bật trên crm_quotes (chỉ một
quy trình thủ công được bật mỗi lúc), gửi bằng test@, rồi hỏi từng người xem được gì — hộp duyệt, chuông. Nhóm «Ban duyệt»
bị bớt duyet3 ở kịch bản A/B (nhóm hai người) rồi trả lại cho kịch bản C (nhóm ba người).

  A. Xoay vòng (round_robin): test@ gửi 4 báo giá cho nhóm «Ban duyệt» (duyet1, duyet2) ⇒ mỗi người đúng 2, luân phiên;
     duyet3 (không thuộc nhóm), hotro1, test@ không thấy gì; mỗi người chuông «Cần phê duyệt» +2.
  B. Theo tải (load_based): duyet1 giữ 3 việc mở, duyet2 giữ 1 ⇒ việc mới vào duyet2; việc duyet2 NHẬN từ bể cũng tính
     là của duyet2 (2 việc riêng + 2 nhận = 4 > 3 ⇒ việc kế vào duyet1).
  C. Quorum 60% + mỗi thành viên: nhóm ba người ⇒ ba ghế, ba chuông; chi tiết «1/3 đã duyệt — cần 2»; duyet1 + duyet2
     duyệt ⇒ xong (approved) và ghế duyet3 đóng — duyet3 không còn thấy, duyệt ⇒ APPROVAL_NOT_AUTHORIZED; hai người từ
     chối ⇒ từ chối NGAY (không còn đạt được 60%), duyet3 chưa kịp bỏ phiếu.
  D. Cấu hình sai bị chặn khi lưu (qua API thật): quorum thiếu/ngoài 1–100 ⇒ 422 APPROVAL_QUORUM_INVALID; percent cạnh
     mode khác ⇒ cũng thế; chiến lược trên vai/hàng đợi ⇒ 422 APPROVAL_STEP_INVALID; completion cạnh nhóm bể ⇒ 422;
     mỗi-thành-viên trên việc (task) ⇒ 422.

    python3 scripts/fixture-bpm.py --base http://localhost:8700/api/v1
    python3 scripts/acceptance-bpm-rr-quorum.py --base http://localhost:8700/api/v1 --db nexus_wt_bpm_routing_rr_quorum

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: stdlib + `docker exec nexus-mysql mysql` (cùng khuôn acceptance-bpm-routing.py, nạp lại Env/check của nó).
"""
import argparse
import importlib.util
import os
import sys

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check, fails = seats_mod.Env, seats_mod.check, seats_mod.fails

PROC_PREFIX = "S6b nghiệm thu — "


def code(resp):
    return resp[1].get("code")


def mk_quote(env, tok, name):
    c, b = env.req("POST", "/crm_quotes", tok, {"name": name, "status": "Nháp"})
    assert c == 201, (c, b)
    return b["data"].get("id") or b["data"].get("record", {}).get("id")


def submit(env, tok, rid):
    c, b = env.req("POST", f"/crm_quotes/{rid}/approvals/submit", tok)
    inst = (b.get("data") or {}).get("approval") or (b.get("data") or {})
    return c, (inst.get("ID") or inst.get("id")), b


def bells(env, uid):
    return int(env.sql(f"SELECT COUNT(*) FROM sys_notifications WHERE tenant_id = '{env.tenant}' AND user_id = '{uid}' "
                       f"AND title = 'Cần phê duyệt'")[0][0])


def open_seats(env, iid):
    """[(assignee_type, assignee_id)] of the OPEN seats, stable order."""
    return [tuple(r) for r in env.sql(f"SELECT assignee_type, assignee_id FROM sys_bpm_tasks WHERE tenant_id = '{env.tenant}' "
                                      f"AND instance_id = '{iid}' AND status = 'open' ORDER BY assignee_id")]


def seat_outcomes(env, iid):
    return sorted(r[0] for r in env.sql(f"SELECT CONCAT(assignee_id, ':', IFNULL(outcome, 'open')) FROM sys_bpm_tasks "
                                        f"WHERE tenant_id = '{env.tenant}' AND instance_id = '{iid}'"))


def status_of(r):
    a = r[1]["data"]["approval"]
    return a.get("status") or a.get("Status")


def run(env):
    t = env.tokens()
    admin = t["admin"]
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", admin)[1]["data"]["users"]}
    uid = {n: users[f"{n}@example.com"] for n in ("duyet1", "duyet2", "duyet3", "hotro1")}
    uid["staff"] = users["test@example.com"]
    procs = env.req("GET", "/admin/approval-processes", admin)[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]]
    assert not rivals, f"another active manual crm_quotes process would take the submit: {rivals}"
    teams = env.req("GET", "/admin/teams", admin)[1]["data"]["teams"]
    team = next(x for x in teams if x["name"] == "Ban duyệt")

    created_procs, pending_instances, removed_member = [], [], False

    def make_proc(name, graph, active=True):
        body = {"name": PROC_PREFIX + name, "entityName": "crm_quotes", "trigger": "manual", "isActive": active, "graph": graph}
        c, b = env.req("POST", "/admin/approval-processes", admin, body)
        assert c == 201, (c, b)
        pid = b["data"]["process"]["id"]
        created_procs.append(pid)
        return pid

    def drop_proc(pid):
        env.req("DELETE", f"/admin/approval-processes/{pid}", admin)

    def send(name):
        c, i, b = submit(env, t["staff"], mk_quote(env, t["staff"], name))
        assert c == 201 and i, (c, b)
        pending_instances.append(i)
        return i

    def pending_of(who):
        return env.pending(t[who])

    def team_node(strategy, completion=None, nid="ban"):
        n = {"id": nid, "name": "Ban duyệt", "kind": "approval", "assignment": {"type": "team", "id": team["id"], "strategy": strategy}}
        if completion:
            n["completion"] = completion
        return {"nodes": [n]}

    def user_node(who):
        return {"nodes": [{"id": "ca-nhan", "name": "Cá nhân", "kind": "approval", "assignment": {"type": "user", "id": uid[who]}}]}

    try:
        # ---- D first (config guards through the real API; nothing is stored) -------------------------------
        def bad(label, graph, want):
            r = env.req("POST", "/admin/approval-processes", admin, {"name": PROC_PREFIX + "lỗi", "entityName": "crm_quotes", "trigger": "manual", "isActive": False, "graph": graph})
            check(f"D: {label} ⇒ 422 {want}", r[0] == 422 and code(r) == want, f"{r[0]} {r[1]}")

        em = lambda c: team_node("each_member", c)  # noqa: E731
        bad("quorum không có percent", em({"mode": "quorum"}), "APPROVAL_QUORUM_INVALID")
        bad("quorum 101", em({"mode": "quorum", "percent": 101}), "APPROVAL_QUORUM_INVALID")
        bad("quorum 0", em({"mode": "quorum", "percent": 0}), "APPROVAL_QUORUM_INVALID")
        bad("percent cạnh mode all", em({"mode": "all", "percent": 60}), "APPROVAL_QUORUM_INVALID")
        bad("chiến lược round_robin trên vai", {"nodes": [{"id": "a", "kind": "approval", "assignment": {"type": "role", "id": "00000000-0000-0000-0000-000000000000", "strategy": "round_robin"}}]}, "APPROVAL_STEP_INVALID")
        bad("completion cạnh nhóm bể", team_node("pool", {"mode": "all"}), "APPROVAL_STEP_INVALID")
        bad("completion cạnh round_robin (một người không có chính sách)", team_node("round_robin", {"mode": "all"}), "APPROVAL_STEP_INVALID")
        bad("mỗi-thành-viên trên việc (task)", {"nodes": [{"id": "a", "kind": "task", "assignment": {"type": "team", "id": team["id"], "strategy": "each_member"}}]}, "APPROVAL_STEP_INVALID")
        bad("chiến lược lạ", team_node("lottery"), "APPROVAL_STEP_INVALID")

        # ---- A: round robin on a team of two ----------------------------------------------------------------
        r = env.req("DELETE", f"/admin/teams/{team['id']}/members/{uid['duyet3']}", admin)
        removed_member = r[0] in (200, 204)
        check("A: bớt duyet3 khỏi nhóm «Ban duyệt» (nhóm hai người)", removed_member, f"{r[0]}")
        bell0 = {n: bells(env, uid[n]) for n in ("duyet1", "duyet2", "duyet3", "hotro1", "staff")}
        pid = make_proc("xoay vòng", team_node("round_robin"))
        insts = [send(f"S6b-A{i}") for i in range(1, 5)]
        holders = []
        for i in insts:
            seats = open_seats(env, i)
            holders.append(seats[0][1] if len(seats) == 1 and seats[0][0] == "user" else None)
        got = {n: holders.count(uid[n]) for n in ("duyet1", "duyet2")}
        check("A: 4 báo giá ⇒ duyet1 đúng 2, duyet2 đúng 2, không ai khác", got == {"duyet1": 2, "duyet2": 2} and None not in holders, f"{holders}")
        check("A: luân phiên (hai việc liền nhau không cùng một người)", all(holders[k] != holders[k + 1] for k in range(3)), f"{[h[:6] for h in holders if h]}")
        for n in ("duyet1", "duyet2"):
            mine = [i for i, h in zip(insts, holders) if h == uid[n]]
            check(f"A: {n} thấy đúng 2 việc của mình ở hộp duyệt, không thấy việc người kia",
                  sorted(mine) == sorted(i for i in pending_of(n) if i in insts), f"{pending_of(n)}")
        check("A: duyet3 (không thuộc nhóm), hotro1, test@ không thấy việc nào", all(not (set(insts) & set(pending_of(n))) for n in ("duyet3", "hotro1", "staff")))
        check("A: chuông «Cần phê duyệt» duyet1 +2, duyet2 +2; người khác +0",
              bells(env, uid["duyet1"]) == bell0["duyet1"] + 2 and bells(env, uid["duyet2"]) == bell0["duyet2"] + 2
              and all(bells(env, uid[n]) == bell0[n] for n in ("duyet3", "hotro1", "staff")))
        r = env.req("GET", f"/approvals/{insts[0]}", t["duyet3"])
        check("A: duyet3 (không thuộc nhóm) mở thẳng việc của người khác ⇒ không có nút Duyệt/Từ chối (awaitsMe=false)",
              r[0] != 200 or not (r[1]["data"].get("approval") or {}).get("awaitsMe"), f"{r[0]} {(r[1].get('data') or {}).get('approval', {}).get('awaitsMe')}")
        drop_proc(pid)

        # ---- B: load based ------------------------------------------------------------------------------------
        pid = make_proc("việc của duyet1", user_node("duyet1"))
        for k in range(3):
            send(f"S6b-B1-{k}")
        drop_proc(pid)
        pid = make_proc("việc của duyet2", user_node("duyet2"))
        send("S6b-B2")
        drop_proc(pid)
        pid = make_proc("theo tải", team_node("load_based"))
        i_b1 = send("S6b-B3")
        check("B: duyet1 giữ 3 việc mở, duyet2 giữ 1 ⇒ việc mới vào duyet2", open_seats(env, i_b1) == [("user", uid["duyet2"])], f"{open_seats(env, i_b1)}")
        check("B: duyet2 thấy ở hộp duyệt, duyet1 không", i_b1 in pending_of("duyet2") and i_b1 not in pending_of("duyet1"))
        drop_proc(pid)
        # duyet2 now holds 2; two POOL seats duyet2 CLAIMS count as theirs (4 > 3)
        pid = make_proc("bể nhóm", team_node("pool"))
        pool_insts = [send(f"S6b-B4-{k}") for k in range(2)]
        for i in pool_insts:
            c, b = env.req("GET", "/work-items?view=pool&limit=200", t["duyet2"])
            item = next(x for x in b["data"]["items"] if x["instanceId"] == i)
            r = env.req("POST", f"/work-items/{item['id']}/claim", t["duyet2"])
            check("B: duyet2 nhận việc từ bể ⇒ 200", r[0] == 200, f"{r[0]} {r[1]}")
        drop_proc(pid)
        pid = make_proc("theo tải 2", team_node("load_based"))
        i_b2 = send("S6b-B5")
        check("B: duyet2 giữ 2 việc riêng + 2 việc nhận từ bể = 4 > duyet1 3 ⇒ việc kế vào duyet1", open_seats(env, i_b2) == [("user", uid["duyet1"])], f"{open_seats(env, i_b2)}")
        drop_proc(pid)

        # ---- C: each member + quorum 60% on the team of three ------------------------------------------------
        r = env.req("POST", f"/admin/teams/{team['id']}/members", admin, {"userId": uid["duyet3"]})
        check("C: trả duyet3 về nhóm (nhóm ba người)", r[0] in (200, 201, 204), f"{r[0]} {r[1]}")
        removed_member = False
        # the council is followed by a one-person step (hotro1, nobody else holds it), so the approval stays pending after the quorum
        graph_c = team_node("each_member", {"mode": "quorum", "percent": 60})
        graph_c["nodes"].append({"id": "giam-doc", "name": "Giám đốc", "kind": "approval", "assignment": {"type": "user", "id": uid["hotro1"]}})
        pid = make_proc("quorum 60%", graph_c)
        bell_c = {n: bells(env, uid[n]) for n in ("duyet1", "duyet2", "duyet3", "hotro1", "staff")}
        i_c = send("S6b-C1")
        check("C: ba ghế người dùng — duyet1, duyet2, duyet3",
              sorted(open_seats(env, i_c)) == sorted(("user", uid[n]) for n in ("duyet1", "duyet2", "duyet3")), f"{open_seats(env, i_c)}")
        check("C: chuông của cả ba +1; hotro1, test@ +0",
              all(bells(env, uid[n]) == bell_c[n] + 1 for n in ("duyet1", "duyet2", "duyet3")) and all(bells(env, uid[n]) == bell_c[n] for n in ("hotro1", "staff")))
        check("C: cả ba thấy ở hộp duyệt, hotro1 không", all(i_c in pending_of(n) for n in ("duyet1", "duyet2", "duyet3")) and i_c not in pending_of("hotro1"))
        r = env.req("POST", f"/approvals/{i_c}/approve", t["duyet1"], {"note": "đồng ý"})
        check("C: duyet1 duyệt ⇒ 200, hồ sơ còn chờ", r[0] == 200 and status_of(r) == "pending", f"{r[0]} {r[1]}")
        r = env.req("GET", f"/approvals/{i_c}", t["duyet2"])
        comp = ((r[1].get("data") or {}).get("approval") or {}).get("completion") or {}
        check("C: chi tiết «1/3 — cần 2» (mode quorum, 60%)",
              r[0] == 200 and (comp.get("mode"), comp.get("approved"), comp.get("total"), comp.get("required"), comp.get("percent")) == ("quorum", 1, 3, 2, 60), f"{comp}")
        r = env.req("POST", f"/approvals/{i_c}/approve", t["duyet2"], {"note": "đồng ý"})
        check("C: duyet2 duyệt ⇒ đủ 2/3 ⇒ bước ban duyệt xong, hồ sơ sang bước kế (giám đốc)",
              r[0] == 200 and status_of(r) == "pending" and r[1]["data"]["approval"].get("currentStep") == 1, f"{r[0]} {r[1]}")
        check("C: ghế duyet3 đóng (superseded), 2 ghế đã duyệt; chỉ còn ghế của giám đốc (hotro1)",
              {f"{uid['duyet1']}:approved", f"{uid['duyet2']}:approved", f"{uid['duyet3']}:superseded"} <= set(seat_outcomes(env, i_c))
              and open_seats(env, i_c) == [("user", uid["hotro1"])], f"{seat_outcomes(env, i_c)} {open_seats(env, i_c)}")
        check("C: duyet3 không còn thấy ở hộp duyệt", i_c not in pending_of("duyet3"))
        r = env.req("POST", f"/approvals/{i_c}/approve", t["duyet3"], {"note": "muộn"})
        check("C: duyet3 duyệt muộn ⇒ không được (APPROVAL_NOT_AUTHORIZED)", r[0] != 200 and code(r) == "APPROVAL_NOT_AUTHORIZED", f"{r[0]} {r[1]}")
        # two rejections make 60% unreachable: rejected at once
        i_c2 = send("S6b-C2")
        r = env.req("POST", f"/approvals/{i_c2}/reject", t["duyet1"], {"note": "giá cao"})
        check("C: duyet1 từ chối ⇒ vẫn chờ (còn đạt được 2/3)", r[0] == 200 and status_of(r) == "pending", f"{r[0]} {r[1]}")
        r = env.req("POST", f"/approvals/{i_c2}/reject", t["duyet2"], {"note": "giá cao"})
        check("C: duyet2 từ chối ⇒ từ chối NGAY (không còn đạt được 60%)", r[0] == 200 and status_of(r) == "rejected", f"{r[0]} {r[1]}")
        check("C: duyet3 chưa bỏ phiếu và không còn việc nào", open_seats(env, i_c2) == [] and i_c2 not in pending_of("duyet3"))
        drop_proc(pid)
    finally:
        # settle what is still open so the clone is left as found (an admin may decide a single-seat step; a
        # several-seat one needs the explicit override)
        for i in pending_instances:
            r = env.req("POST", f"/admin/approval-instances/{i}/override", admin, {"decision": "reject", "note": "dọn nghiệm thu"})
            if r[0] not in (200, 404, 409):
                env.req("POST", f"/approvals/{i}/reject", admin, {"note": "dọn nghiệm thu"})
        for pid in created_procs:
            env.req("DELETE", f"/admin/approval-processes/{pid}", admin)
        if removed_member:
            env.req("POST", f"/admin/teams/{team['id']}/members", admin, {"userId": uid["duyet3"]})


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--db", required=True, help="tên DB clone của server (đọc bảng ghế, thông báo)")
    a = ap.parse_args()
    run(Env(a.base, a.db))
    print(f"\n{len(fails)} kiểm tra hỏng" if fails else "\n✅ Nghiệm thu S6b đạt (non-admin).")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
