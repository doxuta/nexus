#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S7 của specs/043 (`bpm-gateways-auto-end`, DAILY-QUEUE #120, T133).

Quy trình RẼ NHÁNH theo giá trị hồ sơ, bước TỰ ĐỘNG ghi trường, bước KẾT THÚC; hồ sơ kẹt có nút Thử lại
(hàng r3 + r11 của ENGINE-COVERAGE-SPEC §E5). Chạy trên server dựng từ DB CLONE (không bao giờ DB dev dùng
chung), sau `scripts/fixture-bpm.py`. Mọi việc người dùng làm bằng TOKEN KHÔNG-ADMIN (`test@example.com` gửi,
`duyet1/2/3` duyệt); admin chỉ cấu hình quy trình, bấm Thử lại / Huỷ và soi danh sách hồ sơ kẹt.

  1. Cấu hình sai bị chặn lúc LƯU, đúng chỗ: rẽ nhánh thiếu mặc định, vòng chỉ gồm bước tự động, hai người
     duyệt nhau mãi (không tới kết thúc), nhánh trỏ bước không có, bước tự động ghi giá trị picklist không có.
  2. «Dưới 5 triệu tự duyệt · dưới 50 triệu duyet1 · từ 50 triệu duyet1 rồi duyet3» (trường tiền tệ
     `amount_due`): test@ gửi 3 / 30 / 80 triệu.
       3tr  ⇒ duyệt ngay, báo giá thành «Chấp nhận» trên màn của test@ (bước tự động ghi), không ai có việc;
       30tr ⇒ chỉ duyet1 có việc; duyet1 duyệt ⇒ xong;
       80tr ⇒ duyet1 rồi duyet3 (duyet3 chưa thấy việc khi duyet1 chưa duyệt);
     `GET /approvals/:id` bằng token test@ trả nhánh đã đi (`gateway`) và `process.steps` dạng nút.
  3. Đọc dữ liệu MỚI: gửi 80tr, test@ sửa xuống 3tr khi đang chờ duyet1; duyet1 duyệt ⇒ cổng thứ hai đọc 3tr
     ⇒ KHÔNG thêm bước duyet3.
  4. Hồ sơ kẹt (thực thể tạm có trường mã duy nhất; bước tự động ghi mã đã có người dùng): hồ sơ đứng lại ở
     bước tự động, `pending`, lý do đọc được; danh sách `?stuck=true` của admin có nó (kèm lastError), người
     gửi/người không-admin KHÔNG thấy lý do; test@ (không-admin) bấm Thử lại ⇒ 403; admin Thử lại khi chưa sửa
     ⇒ vẫn kẹt; giải phóng mã rồi Thử lại ⇒ xong, mã đã ghi; Huỷ một hồ sơ kẹt khác ⇒ cancelled.
  5. Sau sự cố (mô phỏng: kéo hồ sơ về lại nút rẽ nhánh + xếp job `bpm_continue` như giao dịch của lần ghi
     đã làm) job đưa hồ sơ tới đúng ghế duyet1 trong vài giây, không tạo hồ sơ hay ghế thừa.

    python3 scripts/fixture-bpm.py --base http://localhost:8702/api/v1
    python3 scripts/acceptance-bpm-gateways.py --base http://localhost:8702/api/v1 --db nexus_wt_bpm_gateways_auto_end

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: stdlib + `docker exec nexus-mysql mysql` (cùng khuôn acceptance-bpm-parallel.py, mà script này nạp lại).
"""
import argparse
import importlib.util
import json
import os
import sys
import time
import uuid

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check = seats_mod.Env, seats_mod.check
fails = seats_mod.fails

PROC = "S7 nghiệm thu — báo giá rẽ nhánh"
OUTCOME = {"statusField": "status", "approvedValue": "Chấp nhận", "rejectedValue": "Từ chối"}


def money(n):
    return {"amount": n, "currencyCode": "VND"}


def when(*rules, match="and"):
    return {"match": match, "rules": [{"field": f, "op": o, "value": v} for f, o, v in rules]}


def quote_graph(d1, d3):
    return {"start": "route", "nodes": [
        {"id": "route", "name": "Phân luồng theo giá trị", "kind": "gateway", "default": "duyet-1", "branches": [
            {"name": "tu-duyet", "when": when(("amount_due", "lt", 5000000)), "to": "tu-duyet"},
            {"name": "cap-1", "when": when(("amount_due", "lt", 50000000)), "to": "duyet-1"}]},
        {"id": "tu-duyet", "name": "Tự duyệt", "kind": "auto", "set": [{"field": "status", "value": "Chấp nhận"}], "next": "xong"},
        {"id": "duyet-1", "name": "Trưởng phòng duyệt", "kind": "approval", "approverType": "user", "approverId": d1, "next": "tren-50tr"},
        {"id": "tren-50tr", "name": "Trên 50 triệu?", "kind": "gateway", "default": "xong", "branches": [
            {"name": "them-giam-doc", "when": when(("amount_due", "gte", 50000000)), "to": "duyet-3"}]},
        {"id": "duyet-3", "name": "Giám đốc duyệt", "kind": "approval", "approverType": "user", "approverId": d3, "next": "xong"},
        {"id": "xong", "name": "Xong", "kind": "end", "result": "approved"},
    ]}


def process_body(name, graph, trigger="manual", outcome=OUTCOME, entity="crm_quotes"):
    body = {"name": name, "entityName": entity, "trigger": trigger, "isActive": True, "graph": graph}
    if outcome:
        body["outcome"] = outcome
    return body


def create_quote(env, tok, name, amount):
    c, b = env.req("POST", "/crm_quotes", tok, {"name": name, "status": "Nháp", "amount_due": money(amount)})
    assert c == 201, (c, b)
    return b["data"].get("id") or b["data"].get("record", {}).get("id")


def get_quote(env, tok, rid):
    c, rec = env.req("GET", f"/crm_quotes/{rid}", tok)
    return (rec.get("data") or {}).get("data") or {}


def send(env, tok, name, amount):
    rid = create_quote(env, tok, name, amount)
    c, b = env.req("POST", f"/crm_quotes/{rid}/approvals/submit", tok)
    assert c == 201, (c, b)
    inst = b["data"].get("approval") or b["data"]
    return inst.get("id") or inst.get("ID"), rid, inst


def detail(env, tok, iid):
    c, b = env.req("GET", f"/approvals/{iid}", tok)
    return c, (b.get("data") or {}).get("approval") or b.get("data") or {}


def state(env, iid):
    # '-' stands for NULL: the mysql client output is stripped, which would drop a trailing empty column.
    r = env.sql(f"SELECT current_step, status, IFNULL(last_error, '-') FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{iid}'")[0]
    return int(r[0]), r[1], ("" if r[2] == "-" else r[2])


def approve(env, tok, iid, note="ok"):
    return env.req("POST", f"/approvals/{iid}/approve", tok, {"note": note})


def run(env):
    t = env.tokens()
    admin, staff = t["admin"], t["staff"]
    procs = env.req("GET", "/admin/approval-processes", admin)[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]]
    assert not rivals, f"another active manual crm_quotes process would take the submit: {rivals}"
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", admin)[1]["data"]["users"]}
    d1, d3 = users["duyet1@example.com"], users["duyet3@example.com"]

    # ---- 1. config errors are refused at save, at the right place ------------------------------
    good = quote_graph(d1, d3)

    def variant(mutate):
        g = json.loads(json.dumps(good))
        mutate(g)
        return g

    bad = [
        ("rẽ nhánh thiếu mặc định", variant(lambda g: g["nodes"][0].pop("default")), "APPROVAL_GATEWAY_DEFAULT_REQUIRED", "bước 1"),
        ("nhánh trỏ bước không có", variant(lambda g: g["nodes"][0]["branches"][0].update(to="khong-co")), "APPROVAL_TRANSITION_TARGET_UNKNOWN", "bước 1"),
        ("vòng chỉ gồm bước tự động", {"nodes": [
            {"id": "a", "kind": "auto", "set": [{"field": "name", "value": "x"}], "next": "b"},
            {"id": "b", "kind": "auto", "set": [{"field": "name", "value": "y"}], "next": "a"},
            {"id": "p", "kind": "approval", "approverType": "user", "approverId": d1}]}, "APPROVAL_AUTO_CYCLE", "bước 1"),
        ("hai người duyệt nhau mãi", {"nodes": [
            {"id": "a", "kind": "approval", "approverType": "user", "approverId": d1, "next": "b"},
            {"id": "b", "kind": "approval", "approverType": "user", "approverId": d3, "next": "a"}]}, "APPROVAL_END_UNREACHABLE", "bước 1"),
        ("bước tự động ghi giá trị picklist không có", variant(lambda g: g["nodes"][1]["set"][0].update(value="Không có thật")), "RECORD_INVALID_PICKLIST", "bước tự động 2"),
        ("bước tự động ghi trường không có", variant(lambda g: g["nodes"][1]["set"][0].update(field="khong_co")), "RECORD_UNKNOWN_FIELD", "bước tự động 2"),
        ("điều kiện đọc trường không có", variant(lambda g: g["nodes"][0]["branches"][0]["when"]["rules"][0].update(field="khong_co")), "APPROVAL_FIELD_UNKNOWN", "bước 1"),
    ]
    for label, g, code, where in bad:
        c, b = env.req("POST", "/admin/approval-processes", admin, process_body(f"{PROC} — sai", g))
        ok = c == 422 and b.get("code") == code and where in (b.get("error") or "")  # the message names the node
        check(f"cấu hình sai ({label}) ⇒ 422 {code}, thông báo chỉ đúng «{where}»", ok, f"{c} {b.get('code')} {b.get('error')}")
    check("không quy trình nào được lưu từ các ca sai", not [p for p in env.req("GET", "/admin/approval-processes", admin)[1]["data"]["processes"] if p["name"].startswith(PROC)])

    # ---- 2. the tiers ----------------------------------------------------------------------------
    c, b = env.req("POST", "/admin/approval-processes", admin, process_body(PROC, good))
    assert c == 201, (c, b)
    pid = b["data"]["process"]["id"]
    stored = b["data"]["process"].get("graph") or {}
    check("quy trình lưu đủ tuyến: start + 6 nút, steps []", stored.get("start") == "route" and len(stored.get("nodes", [])) == 6 and b["data"]["process"].get("steps") in ([], None), json.dumps(stored)[:200])
    try:
        q3, r3, i3 = send(env, staff, "S7 3 triệu", 3000000)
        check("3 triệu: gửi xong là «đã duyệt» ngay (không ai phải bấm)", i3.get("status") == "approved", str(i3.get("status")))
        check("3 triệu: báo giá thành «Chấp nhận» trên màn của test@ (bước tự động ghi)", get_quote(env, staff, r3).get("status") == "Chấp nhận", str(get_quote(env, staff, r3).get("status")))
        check("3 triệu: không ai có việc", all(q3 not in env.pending(t[n]) for n in ("duyet1", "duyet2", "duyet3")))
        c, d = detail(env, staff, q3)
        check("3 triệu: GET /approvals/:id bằng token test@ trả nhánh đã đi + nút", c == 200 and d.get("gateway") == {"route": "tu-duyet"}
              and [n.get("kind") for n in d.get("process", {}).get("steps", [])] == ["gateway", "auto", "approval", "gateway", "approval", "end"], f"{c} {d.get('gateway')}")
        acts = [h["action"] for h in d.get("history", [])]
        check("3 triệu: test@ (không-admin) thấy lịch sử SẠCH — chỉ dòng gửi, không tên nhánh/trường ghi/bước máy", acts == ["submitted"], str(acts))
        c, da = detail(env, admin, q3)
        acts_admin = [h["action"] for h in da.get("history", [])]
        check("3 triệu: admin thấy đủ gateway → auto → ended (đội Nexus đọc được đường đi)", acts_admin == ["submitted", "gateway", "auto", "ended"], str(acts_admin))

        q30, r30, i30 = send(env, staff, "S7 30 triệu", 30000000)
        check("30 triệu: chỉ duyet1 có việc (duyet2, duyet3 không)", q30 in env.pending(t["duyet1"]) and q30 not in env.pending(t["duyet2"]) and q30 not in env.pending(t["duyet3"]))
        check("30 triệu: báo giá chưa đổi khi chưa ai duyệt", get_quote(env, staff, r30).get("status") == "Nháp")
        c, b = approve(env, t["duyet3"], q30)
        check("30 triệu: duyet3 duyệt thử ⇒ 422 APPROVAL_NOT_AUTHORIZED", c == 422 and b.get("code") == "APPROVAL_NOT_AUTHORIZED", f"{c} {b.get('code')}")
        c, b = approve(env, t["duyet1"], q30)
        check("30 triệu: duyet1 duyệt ⇒ xong, báo giá «Chấp nhận», không sang duyet3", c == 200 and state(env, q30)[1] == "approved"
              and get_quote(env, staff, r30).get("status") == "Chấp nhận" and q30 not in env.pending(t["duyet3"]), f"{c} {state(env, q30)}")
        c, d = detail(env, staff, q30)
        check("30 triệu: nhánh đã đi = cap-1 rồi mặc định", d.get("gateway") == {"route": "cap-1", "tren-50tr": "default"}, str(d.get("gateway")))

        q80, r80, i80 = send(env, staff, "S7 80 triệu", 80000000)
        check("80 triệu: duyet1 có việc, duyet3 CHƯA", q80 in env.pending(t["duyet1"]) and q80 not in env.pending(t["duyet3"]))
        c, b = approve(env, t["duyet1"], q80)
        check("80 triệu: duyet1 duyệt ⇒ vẫn chờ, việc sang duyet3 (khỏi hộp duyet1)", c == 200 and state(env, q80)[1] == "pending"
              and q80 in env.pending(t["duyet3"]) and q80 not in env.pending(t["duyet1"]), f"{c} {state(env, q80)}")
        check("80 triệu: báo giá chưa đổi khi mới qua duyet1", get_quote(env, staff, r80).get("status") == "Nháp")
        c, b = approve(env, t["duyet3"], q80)
        check("80 triệu: duyet3 duyệt ⇒ xong, báo giá «Chấp nhận»", c == 200 and state(env, q80)[1] == "approved" and get_quote(env, staff, r80).get("status") == "Chấp nhận", f"{c} {state(env, q80)}")
        c, d = detail(env, staff, q80)
        check("80 triệu: nhánh đã đi = mặc định rồi them-giam-doc", d.get("gateway") == {"route": "default", "tren-50tr": "them-giam-doc"}, str(d.get("gateway")))

        # ---- 3. fresh data ---------------------------------------------------------------------
        qf, rf, _ = send(env, staff, "S7 đổi giá khi đang chờ", 80000000)
        c, b = env.req("PUT", f"/crm_quotes/{rf}", staff, {"amount_due": money(3000000)})
        check("test@ sửa giá 80tr → 3tr khi đang chờ duyet1", c == 200, f"{c} {b.get('code')}")
        c, b = approve(env, t["duyet1"], qf)
        check("giá mới 3tr: cổng thứ hai đọc dữ liệu MỚI ⇒ xong sau duyet1, không thêm duyet3",
              c == 200 and state(env, qf)[1] == "approved" and qf not in env.pending(t["duyet3"]), f"{c} {state(env, qf)}")

        # ---- 5. crash recovery (simulated state) -------------------------------------------------
        qc, rc, _ = send(env, staff, "S7 sau sự cố", 30000000)
        seq = int(env.sql(f"SELECT seq FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{qc}'")[0][0]) + 1
        env.sql(f"UPDATE sys_approval_instances SET current_step = 0, current_approver_type = NULL, current_approver_id = NULL, "
                f"sla_due_at = NULL, seq = {seq} WHERE tenant_id = '{env.tenant}' AND id = '{qc}'")
        env.sql(f"UPDATE sys_bpm_tasks SET status = 'cancelled', outcome = 'superseded' WHERE tenant_id = '{env.tenant}' AND instance_id = '{qc}' AND status = 'open'")
        env.sql(f"INSERT INTO sys_jobs (id, tenant_id, job_type, payload, status, run_at, attempts, max_attempts) VALUES "
                f"('{uuid.uuid4()}', '{env.tenant}', 'bpm_continue', JSON_OBJECT('instanceId', '{qc}', 'seq', {seq}), 'queued', NOW(3), 0, 5)")
        check("mô phỏng sau sự cố: hồ sơ đứng trên nút rẽ nhánh, không ghế, không ai thấy việc", state(env, qc)[:2] == (0, "pending") and qc not in env.pending(t["duyet1"]))
        moved = False
        for _ in range(40):
            time.sleep(1)
            if state(env, qc)[0] == 2:
                moved = True
                break
        seats = env.sql(f"SELECT node_id, assignee_id FROM sys_bpm_tasks WHERE tenant_id = '{env.tenant}' AND instance_id = '{qc}' AND status = 'open'")
        check("job bpm_continue đưa hồ sơ tới đúng ghế duyet1 (không ghế thừa), duyet1 thấy việc", moved and seats == [["duyet-1", d1]] and qc in env.pending(t["duyet1"]), f"{state(env, qc)} {seats}")
        n = env.sql(f"SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND record_id = '{rc}'")[0][0]
        check("vẫn đúng một hồ sơ cho báo giá đó", n == "1", n)
        c, b = approve(env, t["duyet1"], qc)
        check("duyet1 duyệt hồ sơ vừa được job đưa tới ⇒ xong", c == 200 and state(env, qc)[1] == "approved", f"{c} {state(env, qc)}")
    finally:
        c, _ = env.req("DELETE", f"/admin/approval-processes/{pid}", admin)
        check("dọn: xoá quy trình nghiệm thu (hồ sơ đã ghim vẫn chạy)", c == 204, str(c))

    # ---- 4. stuck hồ sơ -----------------------------------------------------------------------------
    tag = uuid.uuid4().hex[:6]
    ent = f"zz_s7_{tag}"
    c, b = env.req("POST", "/admin/entities", admin, {"name": ent, "label": "S7 nghiệm thu (tạm)", "fields": [
        {"name": "ten", "label": "Tên", "fieldType": "text"},
        {"name": "ma", "label": "Mã", "fieldType": "text", "isUnique": True}]})
    assert c in (200, 201), (c, b)
    pid2 = None
    try:
        c, b = env.req("POST", f"/{ent}", admin, {"ten": "giữ mã", "ma": "DUP"})
        assert c == 201, (c, b)
        holder = b["data"].get("id") or b["data"].get("record", {}).get("id")
        g2 = {"nodes": [
            {"id": "ghi-ma", "name": "Ghi mã", "kind": "auto", "set": [{"field": "ma", "value": "DUP"}], "next": "xong"},
            {"id": "xong", "kind": "end", "result": "approved"}]}
        c, b = env.req("POST", "/admin/approval-processes", admin, process_body(f"{PROC} — kẹt", g2, trigger="after_create", outcome=None, entity=ent))
        assert c == 201, (c, b)
        pid2 = b["data"]["process"]["id"]
        victims = []
        for name in ("kẹt 1", "kẹt 2"):
            c, b = env.req("POST", f"/{ent}", admin, {"ten": name, "ma": f"RIENG-{name[-1]}"})
            assert c == 201, (c, b)
            victims.append(b["data"].get("id") or b["data"].get("record", {}).get("id"))
        time.sleep(2)  # the arming engine is asynchronous
        rows = env.sql(f"SELECT id, record_id, status, current_step, IFNULL(last_error, '') FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND process_id = '{pid2}' ORDER BY created_at")
        check("2 hồ sơ cùng dừng ở bước tự động: pending, có lý do đọc được", len(rows) == 2 and all(r[2] == "pending" and r[3] == "0" and r[4] for r in rows), str(rows))
        iids = [r[0] for r in rows]
        c, b = env.req("GET", f"/admin/approval-processes/{pid2}/instances?stuck=true", admin)
        lst = (b.get("data") or {}).get("instances") or []
        check("danh sách ?stuck=true của admin có cả hai, kèm lastError + effectFailed", c == 200 and len(lst) == 2 and all(x["stuck"] and x["lastError"] and x["effectFailed"] and x["nodeId"] == "ghi-ma" for x in lst), f"{c} {lst}")
        c, d = detail(env, admin, iids[0])
        check("admin mở chi tiết: thấy stuck + lý do + dòng effect_failed", c == 200 and d.get("stuck") is True and d.get("lastError")
              and any(h["action"] == "effect_failed" for h in d.get("history", [])), f"{c} {d.get('stuck')} {d.get('lastError')}")
        c, b = env.req("POST", f"/admin/approval-instances/{iids[0]}/retry", staff, {"note": "tôi thử"})
        check("test@ (không-admin) bấm Thử lại ⇒ 403, hồ sơ nguyên", c == 403 and state(env, iids[0])[2] != "", f"{c}")
        c, b = env.req("GET", f"/admin/approval-processes/{pid2}/instances?stuck=true", staff)
        check("test@ (không-admin) không đọc được danh sách kẹt ⇒ 403", c == 403, f"{c}")
        c, b = env.req("POST", f"/admin/approval-instances/{iids[0]}/retry", admin, {"note": "thử khi chưa sửa"})
        after = (b.get("data") or {}).get("approval") or {}
        check("Thử lại khi CHƯA sửa: vẫn kẹt (pending, còn lỗi), trên timeline có dòng 'retried'", c == 200 and after.get("stuck") is True and state(env, iids[0])[1] == "pending" and state(env, iids[0])[2] != ""
              and any(h["action"] == "retried" for h in after.get("history", [])), f"{c} {after.get('status')}")
        c, b = env.req("PUT", f"/{ent}/{holder}", admin, {"ma": "DA-GIAI-PHONG"})
        check("giải phóng mã (sửa bản ghi đang giữ)", c == 200, f"{c} {b.get('code')}")
        c, b = env.req("POST", f"/admin/approval-instances/{iids[0]}/retry", admin, {"note": "đã giải phóng mã"})
        got = env.req("GET", f"/{ent}/{victims[0]}", admin)[1]["data"]["data"]
        check("Thử lại sau khi sửa ⇒ xong (approved, hết lỗi), mã DUP đã ghi lên bản ghi", c == 200 and state(env, iids[0])[1:] == ("approved", "") and got.get("ma") == "DUP", f"{c} {state(env, iids[0])} {got.get('ma')}")
        c, b = env.req("POST", f"/admin/approval-instances/{iids[0]}/retry", admin, {})
        check("Thử lại hồ sơ đã xong ⇒ 422 APPROVAL_NOT_PENDING", c == 422 and b.get("code") == "APPROVAL_NOT_PENDING", f"{c} {b.get('code')}")
        c, b = env.req("POST", f"/admin/approval-instances/{iids[1]}/cancel", admin, {"note": "không cần nữa"})
        check("Huỷ hồ sơ kẹt còn lại ⇒ cancelled, hết ở danh sách kẹt", c == 200 and state(env, iids[1])[1] == "cancelled"
              and not (env.req("GET", f"/admin/approval-processes/{pid2}/instances?stuck=true", admin)[1].get("data") or {}).get("instances"), f"{c} {state(env, iids[1])}")
    finally:
        if pid2:
            env.req("DELETE", f"/admin/approval-processes/{pid2}", admin)
        c, _ = env.req("DELETE", f"/admin/entities/{ent}", admin)
        check("dọn: xoá thực thể tạm", c in (200, 204), str(c))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--base", required=True, help="API base of the server running over the clone")
    ap.add_argument("--db", required=True, help="the CLONE database the server runs on")
    a = ap.parse_args()
    if a.db == "nexus_platform":
        sys.exit("refusing the shared dev DB — clone it first (nexus_wt_<slug>)")
    run(Env(a.base, a.db))
    print(f"\n{'❌ ' + str(len(fails)) + ' kiểm tra hỏng' if fails else '✅ Đạt'}")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
