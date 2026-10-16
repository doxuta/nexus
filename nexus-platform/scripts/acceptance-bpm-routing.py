#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S6a của specs/043 (`bpm-routing`, DAILY-QUEUE #118, T120).

Việc tự tìm đúng người — bằng token NGƯỜI THẬT không-admin (duyet1..3 vai crm_sales_manager, hotro1 vai tk_agent,
test@example.com nhân viên). Chạy trên server dựng từ DB CLONE (không bao giờ DB dev dùng chung), sau `scripts/fixture-bpm.py`.
Mỗi kịch bản dựng MỘT quy trình gửi-tay đang bật trên crm_quotes (chỉ một quy trình thủ công được bật mỗi lúc), gửi bằng
test@, rồi hỏi từng người xem được gì — hộp duyệt, bể việc, chuông. Trường kiểu người dùng `s6a_rev_<mã>` được thêm tạm
vào crm_quotes (crm_quotes chưa có trường nào kiểu đó) và gỡ lại trong `finally`; nhóm «Ban duyệt» bị bớt duyet3 ở
kịch bản nhóm (để chứng minh «cùng vai nhưng không thuộc nhóm thì không thấy») rồi trả lại.

  A. Theo trường: báo giá có `s6a_rev_*` = duyet2 ⇒ chỉ duyet2 nhận (hộp duyệt + «Của tôi» + chuông «Cần phê duyệt»);
     trường TRỐNG ⇒ người dự phòng (duyet3) giữ; đổi trường SAU khi gửi ⇒ ghế không dời (đóng băng lúc vào bước).
  B. Chủ hồ sơ (`owner_id`) ⇒ chính test@ (người tạo) giữ; người gửi (`submitter`) ⇒ người bấm Gửi duyệt.
  C. Kẹt chứ không mất: trường trống, KHÔNG người dự phòng ⇒ Gửi duyệt vẫn 201, hồ sơ chờ không ghế; không ai (kể cả
     duyet1..3) thấy; admin thấy `lastError` ở danh sách hồ sơ của quy trình và quyết tay được; lý do được xoá sau đó.
  D. Giao VAI crm_sales_manager ⇒ chuông của duyet1, duyet2, duyet3 đều có «Cần phê duyệt» đúng 1 lần (hotro1, test@ không).
  E. Giao NHÓM «Ban duyệt» dạng bể ⇒ thành viên (duyet1, duyet2) thấy ở view=pool + chuông; duyet3 (cùng VAI, không thuộc
     nhóm), hotro1, test@ không thấy, gọi thẳng claim ⇒ 404; duyet1 nhận ⇒ biến khỏi pool duyet2; duyet2 duyệt ⇒ 409
     WORK_ITEM_ALREADY_CLAIMED; duyet1 duyệt qua /approvals ⇒ approved.
  F. Cấu hình sai bị chặn khi lưu (qua API thật): nhóm/vai/người không có ⇒ 422 APPROVAL_ASSIGNEE_NOT_FOUND; trường text
     ⇒ 422 APPROVAL_ASSIGNEE_FIELD_INVALID; chiến lược lạ ⇒ 422 APPROVAL_STEP_INVALID (round_robin mở ở S6b); khoá lạ trong
     `options` ⇒ 422; PUT bỏ `options` của quy trình có sẵn ⇒ 409 APPROVAL_PROCESS_V2_OVERWRITE.

    python3 scripts/fixture-bpm.py --base http://localhost:8700/api/v1
    python3 scripts/acceptance-bpm-routing.py --base http://localhost:8700/api/v1 --db nexus_wt_bpm_routing

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: stdlib + `docker exec nexus-mysql mysql` (cùng khuôn acceptance-bpm-work-items.py, nạp lại Env/check của nó).
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

import uuid

# A field delete is a soft delete and the name stays taken, so each run adds its own (the clone keeps the husks).
FIELD = "s6a_rev_" + uuid.uuid4().hex[:6]
PROC_PREFIX = "S6a nghiệm thu — "


def code(resp):
    return resp[1].get("code")


def mk_quote(env, tok, name, **extra):
    c, b = env.req("POST", "/crm_quotes", tok, {"name": name, "status": "Nháp", **extra})
    assert c == 201, (c, b)
    return b["data"].get("id") or b["data"].get("record", {}).get("id")


def submit(env, tok, rid):
    c, b = env.req("POST", f"/crm_quotes/{rid}/approvals/submit", tok)
    inst = (b.get("data") or {}).get("approval") or (b.get("data") or {})
    return c, (inst.get("ID") or inst.get("id")), b


def bells(env, uid):
    return int(env.sql(f"SELECT COUNT(*) FROM sys_notifications WHERE tenant_id = '{env.tenant}' AND user_id = '{uid}' "
                       f"AND title = 'Cần phê duyệt'")[0][0])


def seat_rows(env, iid):
    return env.sql(f"SELECT assignee_type, assignee_id, status FROM sys_bpm_tasks WHERE tenant_id = '{env.tenant}' "
                   f"AND instance_id = '{iid}' ORDER BY created_at, id")


def last_error(env, iid):
    r = env.sql(f"SELECT IFNULL(last_error, '') FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{iid}'")
    return r[0][0] if r else ""


def pool_ids(env, tok):
    c, b = env.req("GET", "/work-items?view=pool&limit=200", tok)
    assert c == 200, (c, b)
    return {i["instanceId"]: i for i in b["data"]["items"]}


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
    roles = env.req("GET", "/admin/roles", admin)[1]["data"]["roles"]
    mgr_role = next(r["id"] for r in roles if r["name"] == "crm_sales_manager")

    created_procs, field_id, removed_member = [], None, False

    def make_proc(name, graph, options=None, active=True):
        body = {"name": PROC_PREFIX + name, "entityName": "crm_quotes", "trigger": "manual", "isActive": active, "graph": graph}
        if options:
            body["options"] = options
        c, b = env.req("POST", "/admin/approval-processes", admin, body)
        assert c == 201, (c, b)
        pid = b["data"]["process"]["id"]
        created_procs.append(pid)
        return pid

    def drop_proc(pid):
        env.req("DELETE", f"/admin/approval-processes/{pid}", admin)

    def pending_of(who):
        return env.pending(t[who])

    try:
        c, b = env.req("POST", "/admin/fields", admin, {"entityName": "crm_quotes", "name": FIELD, "label": "Người xem xét (nghiệm thu S6a)", "fieldType": "user"})
        assert c in (200, 201), (c, b)
        field_id = (b["data"].get("field") or b["data"]).get("id")

        # ---- F (first: config guards through the real API; nothing is stored) --------------------------------
        node = lambda a: {"nodes": [{"id": "a", "kind": "approval", "assignment": a}]}  # noqa: E731
        ghost = "00000000-0000-0000-0000-000000000000"
        for label, a in (("nhóm không có", {"type": "team", "id": ghost}), ("vai không có", {"type": "role", "id": ghost}),
                         ("người không có", {"type": "user", "id": ghost})):
            r = env.req("POST", "/admin/approval-processes", admin, {"name": PROC_PREFIX + "lỗi", "entityName": "crm_quotes", "trigger": "manual", "isActive": False, "graph": node(a)})
            check(f"F: {label} ⇒ 422 APPROVAL_ASSIGNEE_NOT_FOUND", r[0] == 422 and code(r) == "APPROVAL_ASSIGNEE_NOT_FOUND", f"{r[0]} {r[1]}")
        r = env.req("POST", "/admin/approval-processes", admin, {"name": PROC_PREFIX + "lỗi", "entityName": "crm_quotes", "trigger": "manual", "isActive": False, "graph": node({"type": "field", "field": "name"})})
        check("F: trường text (không phải kiểu người dùng) ⇒ 422 APPROVAL_ASSIGNEE_FIELD_INVALID", r[0] == 422 and code(r) == "APPROVAL_ASSIGNEE_FIELD_INVALID", f"{r[0]} {r[1]}")
        r = env.req("POST", "/admin/approval-processes", admin, {"name": PROC_PREFIX + "lỗi", "entityName": "crm_quotes", "trigger": "manual", "isActive": False, "graph": node({"type": "team", "id": team["id"], "strategy": "lottery"})})
        check("F: chiến lược lạ ⇒ 422 APPROVAL_STEP_INVALID (round_robin đã mở ở S6b)", r[0] == 422 and code(r) == "APPROVAL_STEP_INVALID", f"{r[0]} {r[1]}")
        r = env.req("POST", "/admin/approval-processes", admin, {"name": PROC_PREFIX + "lỗi", "entityName": "crm_quotes", "trigger": "manual", "isActive": False, "graph": node({"type": "submitter"}), "options": {"calendarId": "x"}})
        check("F: khoá lạ trong options ⇒ 422 APPROVAL_NODE_KIND_UNSUPPORTED", r[0] == 422 and code(r) == "APPROVAL_NODE_KIND_UNSUPPORTED", f"{r[0]} {r[1]}")
        r = env.req("POST", "/admin/approval-processes", admin, {"name": PROC_PREFIX + "lỗi", "entityName": "crm_quotes", "trigger": "manual", "isActive": False, "graph": node({"type": "submitter"}), "options": {"fallbackAssignee": {"type": "user", "id": ghost}}})
        check("F: người dự phòng không có ⇒ 422 APPROVAL_ASSIGNEE_NOT_FOUND", r[0] == 422 and code(r) == "APPROVAL_ASSIGNEE_NOT_FOUND", f"{r[0]} {r[1]}")

        # ---- A: by field, with a fallback ------------------------------------------------------------------
        fb = {"fallbackAssignee": {"type": "user", "id": uid["duyet3"]}}
        pid = make_proc("theo trường + dự phòng", {"nodes": [{"id": "xem-xet", "name": "Xem xét", "kind": "approval", "assignment": {"type": "field", "field": FIELD}}]}, fb)
        r = env.req("GET", f"/admin/approval-processes/{pid}", admin)
        check("A: GET trả lại assignment và options đã lưu",
              r[0] == 200 and r[1]["data"]["process"].get("options", {}).get("fallbackAssignee", {}).get("id") == uid["duyet3"]
              and r[1]["data"]["process"]["graph"]["nodes"][0]["assignment"]["field"] == FIELD, f"{r[0]}")
        bell0 = {n: bells(env, uid[n]) for n in ("duyet1", "duyet2", "duyet3", "hotro1", "staff")}
        q1 = mk_quote(env, t["staff"], "S6a-A1", **{FIELD: uid["duyet2"]})
        c, i1, b = submit(env, t["staff"], q1)
        check("A: test@ Gửi duyệt báo giá có người xem xét = duyet2 ⇒ 201", c == 201 and i1, f"{c} {b}")
        check("A: chỉ duyet2 thấy ở hộp duyệt", i1 in pending_of("duyet2") and all(i1 not in pending_of(n) for n in ("duyet1", "duyet3", "hotro1")))
        c, mine = env.req("GET", "/work-items?view=mine&limit=200", t["duyet2"])
        check("A: nằm ở «Của tôi» của duyet2 (giao đích danh, không qua bể)", c == 200 and any(i["instanceId"] == i1 for i in mine["data"]["items"]), f"{c}")
        check("A: chuông «Cần phê duyệt» của duyet2 +1, người khác +0",
              bells(env, uid["duyet2"]) == bell0["duyet2"] + 1 and all(bells(env, uid[n]) == bell0[n] for n in ("duyet1", "duyet3", "hotro1", "staff")))
        check("A: ghế là một ghế người dùng của duyet2", seat_rows(env, i1) == [["user", uid["duyet2"], "open"]], f"{seat_rows(env, i1)}")
        # frozen on entry: moving the field afterwards does not move the open step
        r = env.req("PUT", f"/crm_quotes/{q1}", admin, {FIELD: uid["duyet1"]})
        check("A: đổi người xem xét sang duyet1 sau khi gửi (PUT hồ sơ)", r[0] == 200, f"{r[0]} {r[1]}")
        check("A: ghế KHÔNG dời — vẫn duyet2 (đóng băng lúc vào bước)", seat_rows(env, i1) == [["user", uid["duyet2"], "open"]] and i1 not in pending_of("duyet1"), f"{seat_rows(env, i1)}")
        r = env.req("POST", f"/approvals/{i1}/approve", t["duyet1"], {"note": "tôi duyệt thay"})
        check("A: duyet1 (không giữ ghế) duyệt ⇒ APPROVAL_NOT_AUTHORIZED", r[0] != 200 and code(r) == "APPROVAL_NOT_AUTHORIZED", f"{r[0]} {r[1]}")
        r = env.req("POST", f"/approvals/{i1}/approve", t["duyet2"], {"note": "ok"})
        check("A: duyet2 duyệt ⇒ 200", r[0] == 200, f"{r[0]} {r[1]}")
        # empty field → fallback
        q2 = mk_quote(env, t["staff"], "S6a-A2")
        c, i2, b = submit(env, t["staff"], q2)
        check("A: trường TRỐNG ⇒ người dự phòng duyet3 giữ (ghế duyet3, duyet2 không thấy)",
              c == 201 and seat_rows(env, i2) == [["user", uid["duyet3"], "open"]] and i2 in pending_of("duyet3") and i2 not in pending_of("duyet2"), f"{c} {seat_rows(env, i2) if i2 else b}")
        check("A: lý do kẹt trống (đã có người dự phòng)", last_error(env, i2) == "")
        drop_proc(pid)

        # ---- B: owner_id and submitter --------------------------------------------------------------------
        pid = make_proc("chủ hồ sơ", {"nodes": [{"id": "chu", "name": "Chủ hồ sơ xem", "kind": "approval", "assignment": {"type": "field", "field": "owner_id"}}]})
        q3 = mk_quote(env, t["staff"], "S6a-B1")
        c, i3, b = submit(env, t["staff"], q3)
        check("B: owner_id ⇒ chính test@ (người tạo) giữ ghế", c == 201 and seat_rows(env, i3) == [["user", uid["staff"], "open"]], f"{c} {seat_rows(env, i3) if i3 else b}")
        check("B: test@ thấy ở hộp duyệt của mình, duyet1 không", i3 in pending_of("staff") and i3 not in pending_of("duyet1"))
        drop_proc(pid)
        pid = make_proc("người gửi", {"nodes": [{"id": "gui", "name": "Người gửi", "kind": "approval", "assignment": {"type": "submitter"}}]})
        q4 = mk_quote(env, t["staff"], "S6a-B2")
        c, i4, b = submit(env, t["staff"], q4)
        check("B: submitter ⇒ người bấm Gửi duyệt giữ ghế", c == 201 and seat_rows(env, i4) == [["user", uid["staff"], "open"]], f"{c} {seat_rows(env, i4) if i4 else b}")
        drop_proc(pid)

        # ---- C: stuck, not lost ----------------------------------------------------------------------------
        pid = make_proc("kẹt (không dự phòng)", {"nodes": [{"id": "xem-xet", "name": "Xem xét", "kind": "approval", "assignment": {"type": "field", "field": FIELD}}]})
        bellc = {n: bells(env, uid[n]) for n in ("duyet1", "duyet2", "duyet3")}
        q5 = mk_quote(env, t["staff"], "S6a-C")
        c, i5, b = submit(env, t["staff"], q5)
        check("C: trường trống + KHÔNG người dự phòng ⇒ Gửi duyệt vẫn 201 (không mất)", c == 201 and i5, f"{c} {b}")
        check("C: hồ sơ chờ, KHÔNG ghế, có lý do", seat_rows(env, i5) == [] and last_error(env, i5) != "", f"{seat_rows(env, i5)} {last_error(env, i5)!r}")
        check("C: không ai (duyet1..3, hotro1, test@) thấy ở hộp duyệt",
              all(i5 not in pending_of(n) for n in ("duyet1", "duyet2", "duyet3", "hotro1", "staff")))
        check("C: không chuông nào rung", all(bells(env, uid[n]) == bellc[n] for n in bellc))
        r = env.req("GET", f"/admin/approval-processes/{pid}/instances?status=pending", admin)
        row = next((x for x in (r[1].get("data") or {}).get("instances", []) if x["id"] == i5), None)
        check("C: admin thấy `lastError` ở danh sách hồ sơ của quy trình", r[0] == 200 and row is not None and row.get("lastError"), f"{r[0]} {row}")
        r = env.req("GET", f"/approvals/{i5}", t["hotro1"])
        check("C: hotro1 (không xem được báo giá) mở thẳng /approvals/:id ⇒ 404 (không lộ)", r[0] == 404, f"{r[0]}")
        r = env.req("POST", f"/approvals/{i5}/approve", admin, {"note": "xử lý tay"})
        check("C: admin quyết tay ⇒ 200 approved", r[0] == 200 and (r[1]["data"]["approval"].get("status") or r[1]["data"]["approval"].get("Status")) == "approved", f"{r[0]} {r[1]}")
        drop_proc(pid)

        # ---- D: role → everyone holding it gets the bell ----------------------------------------------------
        pid = make_proc("giao vai", {"nodes": [{"id": "vai", "name": "Quản lý duyệt", "kind": "approval", "assignment": {"type": "role", "id": mgr_role}}]})
        bell_d = {n: bells(env, uid[n]) for n in ("duyet1", "duyet2", "duyet3", "hotro1", "staff")}
        q6 = mk_quote(env, t["staff"], "S6a-D")
        c, i6, b = submit(env, t["staff"], q6)
        check("D: giao vai crm_sales_manager ⇒ 201", c == 201 and i6, f"{c} {b}")
        check("D: chuông «Cần phê duyệt» của duyet1, duyet2, duyet3 đều +1 đúng một lần",
              all(bells(env, uid[n]) == bell_d[n] + 1 for n in ("duyet1", "duyet2", "duyet3")),
              str({n: bells(env, uid[n]) - bell_d[n] for n in bell_d}))
        check("D: hotro1 (khác vai) và test@ (người gửi) không có chuông", all(bells(env, uid[n]) == bell_d[n] for n in ("hotro1", "staff")))
        check("D: cả ba thấy ở hộp duyệt, hotro1 không", all(i6 in pending_of(n) for n in ("duyet1", "duyet2", "duyet3")) and i6 not in pending_of("hotro1"))
        env.req("POST", f"/approvals/{i6}/reject", t["duyet1"], {"note": "dọn nghiệm thu"})
        drop_proc(pid)

        # ---- E: team pool ------------------------------------------------------------------------------------
        r = env.req("DELETE", f"/admin/teams/{team['id']}/members/{uid['duyet3']}", admin)
        removed_member = r[0] in (200, 204)
        check("E: bớt duyet3 khỏi nhóm «Ban duyệt» (để thử «cùng vai, không thuộc nhóm»)", removed_member, f"{r[0]}")
        pid = make_proc("giao nhóm (bể)", {"nodes": [{"id": "ban", "name": "Ban duyệt", "kind": "approval", "assignment": {"type": "team", "id": team["id"], "strategy": "pool"}}]})
        bell_e = {n: bells(env, uid[n]) for n in ("duyet1", "duyet2", "duyet3", "hotro1", "staff")}
        q7 = mk_quote(env, t["staff"], "S6a-E")
        c, i7, b = submit(env, t["staff"], q7)
        check("E: giao nhóm dạng bể ⇒ 201, ghế loại team", c == 201 and seat_rows(env, i7) == [["team", team["id"], "open"]], f"{c} {seat_rows(env, i7) if i7 else b}")
        p1, p2, p3 = (pool_ids(env, t[n]) for n in ("duyet1", "duyet2", "duyet3"))
        check("E: duyet1 và duyet2 (thành viên) thấy ở view=pool, tên nhóm hiện đúng",
              i7 in p1 and i7 in p2 and p1[i7]["assigneeName"] == "Ban duyệt" and p1[i7]["assigneeType"] == "team", f"{p1.get(i7)}")
        check("E: duyet3 (cùng VAI, không thuộc nhóm), hotro1, test@ KHÔNG thấy",
              i7 not in p3 and i7 not in pool_ids(env, t["hotro1"]) and i7 not in pool_ids(env, t["staff"]))
        check("E: chuông của duyet1, duyet2 +1; duyet3, hotro1, test@ +0",
              all(bells(env, uid[n]) == bell_e[n] + 1 for n in ("duyet1", "duyet2")) and all(bells(env, uid[n]) == bell_e[n] for n in ("duyet3", "hotro1", "staff")))
        check("E: hộp duyệt: duyet1, duyet2 thấy; duyet3 không", i7 in pending_of("duyet1") and i7 in pending_of("duyet2") and i7 not in pending_of("duyet3"))
        wid = p1[i7]["id"]
        for who in ("duyet3", "hotro1", "staff"):
            r = env.req("POST", f"/work-items/{wid}/claim", t[who])
            check(f"E: {who} gọi thẳng claim ⇒ 404 WORK_ITEM_NOT_FOUND", r[0] == 404 and code(r) == "WORK_ITEM_NOT_FOUND", f"{r[0]} {r[1]}")
        r = env.req("POST", f"/work-items/{wid}/claim", t["duyet1"])
        check("E: duyet1 nhận ⇒ 200", r[0] == 200, f"{r[0]} {r[1]}")
        check("E: việc biến khỏi pool của duyet2, nằm ở «Của tôi» của duyet1",
              i7 not in pool_ids(env, t["duyet2"]) and any(i["instanceId"] == i7 for i in env.req("GET", "/work-items?view=mine&limit=200", t["duyet1"])[1]["data"]["items"]))
        r = env.req("POST", f"/approvals/{i7}/approve", t["duyet2"], {"note": "tôi duyệt"})
        check("E: duyet2 duyệt việc duyet1 đã nhận ⇒ 409 WORK_ITEM_ALREADY_CLAIMED", r[0] == 409 and code(r) == "WORK_ITEM_ALREADY_CLAIMED", f"{r[0]} {r[1]}")
        r = env.req("POST", f"/approvals/{i7}/approve", t["duyet3"], {"note": "tôi duyệt"})
        check("E: duyet3 (không thuộc nhóm) duyệt ⇒ không được (APPROVAL_NOT_AUTHORIZED)", r[0] != 200 and code(r) == "APPROVAL_NOT_AUTHORIZED", f"{r[0]} {r[1]}")
        r = env.req("POST", f"/approvals/{i7}/approve", t["duyet1"], {"note": "đồng ý"})
        check("E: duyet1 duyệt qua /approvals/:id/approve ⇒ 200 approved",
              r[0] == 200 and (r[1]["data"]["approval"].get("status") or r[1]["data"]["approval"].get("Status")) == "approved", f"{r[0]} {r[1]}")
        drop_proc(pid)

        # ---- F2: options guard on an existing process ---------------------------------------------------------
        pid = make_proc("giữ options", {"nodes": [{"id": "a", "kind": "approval", "assignment": {"type": "submitter"}}]}, fb, active=False)
        r = env.req("PUT", f"/admin/approval-processes/{pid}", admin, {"name": PROC_PREFIX + "giữ options", "entityName": "crm_quotes", "trigger": "manual", "isActive": False,
                                                                    "graph": {"nodes": [{"id": "a", "kind": "approval", "assignment": {"type": "submitter"}}]}})
        check("F: PUT bỏ `options` của quy trình có người dự phòng ⇒ 409 APPROVAL_PROCESS_V2_OVERWRITE", r[0] == 409 and code(r) == "APPROVAL_PROCESS_V2_OVERWRITE", f"{r[0]} {r[1]}")
        drop_proc(pid)
    finally:
        for pid in created_procs:
            env.req("DELETE", f"/admin/approval-processes/{pid}", admin)
        if removed_member:
            env.req("POST", f"/admin/teams/{team['id']}/members", admin, {"userId": uid["duyet3"]})
        if field_id:
            env.req("DELETE", f"/admin/fields/{field_id}", admin)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--db", required=True, help="tên DB clone của server (đọc bảng ghế, thông báo)")
    a = ap.parse_args()
    run(Env(a.base, a.db))
    print(f"\n{len(fails)} kiểm tra hỏng" if fails else "\n✅ Nghiệm thu S6a đạt (non-admin).")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
