#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S3a2 của specs/043 (`bpm-seats-read-path`, DAILY-QUEUE #113, T066).

Hộp duyệt nay đọc từ GHẾ VIỆC (+ dự phòng phép chiếu) mà người duyệt không thấy gì khác; quy trình dạng
đồ thị lưu + chạy được, và không ai ghi đè được nó bằng dạng tuyến tính. Hai pha trên DB CLONE:

  pre   — binary TRƯỚC lát. Admin tạo quy trình thủ công tuyến tính 2 bước (duyet1 → duyet2, chỉ nhận báo
          giá tên có "S3a2-L"); test@example.com (không-admin) gửi Q1, Q2; duyet1 duyệt Q2 (sang duyet2). Chụp
          /approvals/pending của test@, duyet1/2/3, hotro1.
  post  — binary CỦA lát, cùng clone (không migration). (1) Năm hộp duyệt KHỚP đúng tập đã chụp. (2) Hồ sơ
          chèn tay KHÔNG ghế (bản cũ khởi động trong lúc triển khai cuốn) vẫn hiện cho duyet1 qua dự phòng.
          (3) Bản cũ uỷ quyền Q2 sang duyet3 mà không đụng ghế ⇒ duyet2 (ghế cũ) KHÔNG còn thấy, duyet3 thấy.
          (4) Hồ sơ phép chiếu NULL + ghế của duyet2 (bể/nhiều ghế — S4) ⇒ chỉ duyet2 thấy; quyết vẫn theo phép
          chiếu tới S3b (422). (5) Doctor D3 đếm đúng bằng một phép đếm SQL ĐỘC LẬP (biểu thức mã bước của
          backfill 000070), gọi tên đúng hồ sơ. (6) Duyệt các hồ sơ lệch ⇒ ghế lành, D3 giảm đúng. (7) Quy trình
          đồ thị: admin tạo qua API (lưu `steps: []` + `graph`); lưu dạng tuyến tính lên nó ⇒ 409
          APPROVAL_PROCESS_V2_OVERWRITE, đồ thị còn nguyên; test@ gửi Q5 ⇒ duyet1 rồi duyet2 duyệt theo nút,
          ghế mang mã nút, báo giá thành "Chấp nhận". (8) Lùi bản về binary này sau một lát mở khoá mới: quy trình
          mang `approvers`+`completion` (SQL) ⇒ gửi bị 422 APPROVAL_NO_PROCESS; hồ sơ ghim đồ thị có `onReject` ⇒ không
          quyết được, bản ghim giữ khoá. Cuối lượt dọn quy trình + hồ sơ nghiệm thu.

Cần `scripts/fixture-bpm.py` chạy trước trên cùng server. Trạng thái giữa hai pha nằm ở `--state`.

    python3 scripts/acceptance-bpm-seats-read.py pre  --base http://localhost:8137/api/v1 --db nexus_wt_bpm_seats_read_path
    (dừng server; chạy binary của lát trên cùng clone)
    python3 scripts/acceptance-bpm-seats-read.py post --base http://localhost:8137/api/v1 --db nexus_wt_bpm_seats_read_path

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: dùng lại `Env`/`check` của scripts/acceptance-bpm-seats.py (S3a) — cùng khuôn, một danh sách lỗi.
"""
import argparse
import importlib.util
import json
import os
import sys
import tempfile
import uuid

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check = seats_mod.Env, seats_mod.check

LINEAR = "S3a2 nghiệm thu — tuyến tính"
GRAPH = "S3a2 nghiệm thu — đồ thị"
PEOPLE = ("staff", "duyet1", "duyet2", "duyet3", "hotro1")

# The node id of the step an instance stands on, computed the way Migration A's backfill does (steps
# first, then — graph pins, S3a2 — the graph's nodes, then positional). An oracle INDEPENDENT of the Go
# code under test (bpm.seatNodeID / trustedSeats).
NODE_SQL = ("COALESCE(JSON_UNQUOTE(JSON_EXTRACT(i.process_snapshot, CONCAT('$.steps[', i.current_step, '].id'))), "
            "JSON_UNQUOTE(JSON_EXTRACT(i.process_snapshot, CONCAT('$.graph.nodes[', i.current_step, '].id'))), "
            "CONCAT('s', i.current_step + 1))")


def criteria(tag):
    return {"match": "and", "rules": [{"field": "name", "op": "contains", "value": tag}]}


def create_quote(env, tok, name):
    c, b = env.req("POST", "/crm_quotes", tok, {"name": name, "status": "Nháp"})
    assert c == 201, (c, b)
    return b["data"].get("id") or b["data"].get("record", {}).get("id")


def submit(env, tok, rid):
    c, b = env.req("POST", f"/crm_quotes/{rid}/approvals/submit", tok)
    assert c == 201, (c, b)
    inst = b["data"].get("approval") or b["data"]
    return inst.get("ID") or inst.get("id")


def drift_oracle(env):
    """Pending instances of northwind with no TRUSTED open seat, by SQL alone: (id, kind) sorted."""
    rows = env.sql(
        f"SELECT i.id, IF(NOT EXISTS (SELECT 1 FROM sys_bpm_tasks t WHERE t.tenant_id = i.tenant_id AND t.instance_id = i.id "
        f"AND t.status = 'open'), 'missing', 'mismatch') FROM sys_approval_instances i "
        f"WHERE i.tenant_id = '{env.tenant}' AND i.status = 'pending' AND NOT EXISTS (SELECT 1 FROM sys_bpm_tasks t "
        f"WHERE t.tenant_id = i.tenant_id AND t.instance_id = i.id AND t.status = 'open' AND t.node_id = {NODE_SQL} "
        f"AND (i.current_approver_type IS NULL OR (t.assignee_type = i.current_approver_type AND t.assignee_id = i.current_approver_id)))")
    return sorted((r[0], r[1]) for r in rows)


def doctor_d3(env, admin):
    c, b = env.req("POST", "/admin/doctor/scan", admin)
    assert c == 200, (c, b)
    for r in b["data"]["findings"]:
        if r["checkId"] == "D3_APPROVAL_SEAT_DRIFT":
            return r
    return None


def pre(env, state):
    t = env.tokens()
    procs = env.req("GET", "/admin/approval-processes", t["admin"])[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]]
    assert not rivals, f"another active manual crm_quotes process would take the submit: {rivals}"
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", t["admin"])[1]["data"]["users"]}
    d1, d2, d3 = (users[f"duyet{i}@example.com"] for i in (1, 2, 3))
    c, b = env.req("POST", "/admin/approval-processes", t["admin"], {
        "name": LINEAR, "entityName": "crm_quotes", "trigger": "manual", "isActive": True, "criteria": criteria("S3a2-L"),
        "steps": [{"id": "s1", "name": "Trưởng phòng", "approverType": "user", "approverId": d1, "slaSeconds": 14400},
                  {"id": "s2", "name": "Kế toán", "approverType": "user", "approverId": d2}]})
    assert c == 201, (c, b)
    proc = b["data"]["process"]["id"]
    q = {}
    for n in ("Q1", "Q2"):
        rid = create_quote(env, t["staff"], f"S3a2-L {n}")
        q[n] = {"rid": rid, "iid": submit(env, t["staff"], rid)}
    c, _ = env.req("POST", f"/approvals/{q['Q2']['iid']}/approve", t["duyet1"], {"note": "trước lát"})
    check("pre: duyet1 duyệt Q2 (sang duyet2)", c == 200, str(c))
    snap = {n: env.pending(t[n]) for n in PEOPLE}
    check("pre: duyet1 thấy Q1, duyet2 thấy Q2", q["Q1"]["iid"] in snap["duyet1"] and q["Q2"]["iid"] in snap["duyet2"])
    json.dump({"proc": proc, "q": q, "snap": snap, "d1": d1, "d2": d2, "d3": d3}, open(state, "w"), indent=1)


def post(env, state):
    st = json.load(open(state))
    q, snap, d1, d2, d3 = st["q"], st["snap"], st["d1"], st["d2"], st["d3"]
    t = env.tokens()
    made, graph_proc = [], None
    try:
        # (1) Parity: the seat read answers exactly what the projection read answered.
        for n in PEOPLE:
            now = env.pending(t[n])
            check(f"(1) /approvals/pending của {n} KHỚP đúng tập đã chụp trên binary trước lát", now == snap[n], f"{len(now)} dòng")

        # (2) An instance a pre-S3a binary armed during the rolling deploy: no seat at all.
        q3 = create_quote(env, t["staff"], "S3a2-L Q3 (bản cũ khởi động, không ghế)")
        seatless = str(uuid.uuid4())
        env.sql(f"INSERT INTO sys_approval_instances (id, tenant_id, process_id, process_snapshot, entity_name, record_id, "
                f"current_step, status, seq, current_approver_type, current_approver_id, escalated, history, created_by, created_at, updated_at) "
                f"SELECT '{seatless}', tenant_id, process_id, process_snapshot, entity_name, '{q3}', 0, 'pending', 0, 'user', '{d1}', 0, '[]', "
                f"created_by, NOW(3), NOW(3) FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{q['Q1']['iid']}'")
        made.append(seatless)
        check("(2) hồ sơ chèn tay KHÔNG ghế vẫn hiện trong hộp của duyet1 (dự phòng)", seatless in env.pending(t["duyet1"]))
        c, _ = env.req("GET", f"/approvals/{seatless}", t["duyet1"])
        check("(2) duyet1 mở được chi tiết của nó", c == 200, str(c))
        c, b = env.req("GET", f"/approvals/{seatless}", t["hotro1"])
        check("(2) hotro1 (người ngoài) ⇒ 404 APPROVAL_INSTANCE_NOT_FOUND", c == 404 and b.get("code") == "APPROVAL_INSTANCE_NOT_FOUND", f"{c} {b.get('code')}")

        # (3) A pre-S3a binary delegates Q2 to duyet3 without touching the seat duyet2 still holds.
        env.sql(f"UPDATE sys_approval_instances SET current_approver_id = '{d3}' WHERE tenant_id = '{env.tenant}' AND id = '{q['Q2']['iid']}'")
        check("(3) ghế cũ (duyet2) không được tin: Q2 rời hộp của duyet2", q["Q2"]["iid"] not in env.pending(t["duyet2"]))
        check("(3) người phép chiếu gọi tên (duyet3) thấy Q2", q["Q2"]["iid"] in env.pending(t["duyet3"]))

        # (4) NULL projection + an open seat of duyet2 on the current node (a pool / several seats — S4).
        q4 = create_quote(env, t["staff"], "S3a2-L Q4 (phép chiếu NULL)")
        pooled = str(uuid.uuid4())
        env.sql(f"INSERT INTO sys_approval_instances (id, tenant_id, process_id, process_snapshot, entity_name, record_id, "
                f"current_step, status, seq, escalated, history, created_by, created_at, updated_at) "
                f"SELECT '{pooled}', tenant_id, process_id, process_snapshot, entity_name, '{q4}', 0, 'pending', 1, 0, '[]', "
                f"created_by, NOW(3), NOW(3) FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{q['Q1']['iid']}'")
        env.sql(f"INSERT INTO sys_bpm_tasks (id, tenant_id, instance_id, process_id, entity_name, record_id, node_id, entry_seq, "
                f"kind, assignee_type, assignee_id, status, version, created_at, updated_at) "
                f"SELECT '{uuid.uuid4()}', tenant_id, id, process_id, entity_name, record_id, 's1', 1, 'approval', 'user', '{d2}', "
                f"'open', 0, NOW(3), NOW(3) FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{pooled}'")
        made.append(pooled)
        check("(4) phép chiếu NULL: duyet2 (giữ ghế) thấy hồ sơ", pooled in env.pending(t["duyet2"]))
        check("(4) phép chiếu NULL: duyet1 (không ghế) không thấy", pooled not in env.pending(t["duyet1"]))
        c, _ = env.req("GET", f"/approvals/{pooled}", t["duyet2"])
        check("(4) duyet2 mở được chi tiết", c == 200, str(c))
        c, b = env.req("POST", f"/approvals/{pooled}/approve", t["duyet2"], {"note": "thử"})
        check("(4) quyết vẫn theo phép chiếu tới S3b ⇒ 422 APPROVAL_NOT_AUTHORIZED, hồ sơ vẫn chờ",
              c == 422 and b.get("code") == "APPROVAL_NOT_AUTHORIZED", f"{c} {b.get('code')}")

        # (5) Doctor D3 = an independent SQL count, naming the same instances and kinds.
        want = drift_oracle(env)
        d3r = doctor_d3(env, t["admin"])
        got = sorted(it["key"] for it in (d3r or {}).get("items", []))
        check("(5) Doctor có D3, report-only", d3r is not None and d3r.get("reportOnly") is True, str(d3r and d3r.get("reportOnly")))
        check("(5) D3 đếm đúng bằng phép đếm SQL độc lập", d3r is not None and d3r["count"] == len(want) and got == sorted(i for i, _ in want),
              f"D3={d3r and d3r['count']} SQL={len(want)}")
        kinds = dict(want)
        check("(5) hồ sơ không ghế = missing, Q2 (ghế lệch người) = mismatch, hồ sơ phép chiếu NULL có ghế KHÔNG bị đếm",
              kinds.get(seatless) == "missing" and kinds.get(q["Q2"]["iid"]) == "mismatch" and pooled not in kinds, str(kinds))

        # (6) The next decision heals each drifted instance: its seat is right again, D3 drops by two.
        c, _ = env.req("POST", f"/approvals/{seatless}/approve", t["duyet1"], {"note": "sau lát"})
        check("(6) duyet1 duyệt hồ sơ không ghế ⇒ sang duyet2, có ghế", c == 200 and seatless in env.pending(t["duyet2"]), str(c))
        c, _ = env.req("POST", f"/approvals/{q['Q2']['iid']}/approve", t["duyet3"], {"note": "sau lát"})
        check("(6) duyet3 duyệt Q2 (phép chiếu) ⇒ kết thúc", c == 200 and q["Q2"]["iid"] not in env.pending(t["duyet3"]), str(c))
        after = doctor_d3(env, t["admin"])
        check("(6) D3 giảm đúng 2 và vẫn bằng phép đếm SQL", after is not None and after["count"] == len(want) - 2 == len(drift_oracle(env)),
              f"{after and after['count']} / {len(want) - 2}")

        # (7) Graph form: stored beside steps [], guarded against a linear overwrite, run node by node.
        c, b = env.req("POST", "/admin/approval-processes", t["admin"], {
            "name": GRAPH, "entityName": "crm_quotes", "trigger": "manual", "isActive": True, "criteria": criteria("S3a2-G"),
            "outcome": {"statusField": "status", "approvedValue": "Chấp nhận"},
            "graph": {"nodes": [
                {"id": "truong-phong", "name": "Trưởng phòng", "kind": "approval", "approverType": "user", "approverId": d1, "slaSeconds": 14400},
                {"id": "giam-doc", "name": "Giám đốc", "kind": "approval", "approverType": "user", "approverId": d2}]}})
        check("(7) admin tạo quy trình đồ thị ⇒ 201, steps [] + graph 2 nút",
              c == 201 and b["data"]["process"]["steps"] == [] and len(b["data"]["process"]["graph"]["nodes"]) == 2, f"{c} {b.get('code')}")
        graph_proc = b.get("data", {}).get("process", {}).get("id")
        # What the old Studio (which reads only `steps`) would send after adding a step to it.
        c, b = env.req("PUT", f"/admin/approval-processes/{graph_proc}", t["admin"], {
            "name": GRAPH, "entityName": "crm_quotes", "trigger": "manual", "isActive": True, "criteria": criteria("S3a2-G"),
            "outcome": {"statusField": "status", "approvedValue": "Chấp nhận"},
            "steps": [{"approverType": "user", "approverId": d1}]})
        c2, g = env.req("GET", f"/admin/approval-processes/{graph_proc}", t["admin"])
        check("(7) lưu dạng tuyến tính lên quy trình đồ thị ⇒ 409 APPROVAL_PROCESS_V2_OVERWRITE, đồ thị còn nguyên",
              c == 409 and b.get("code") == "APPROVAL_PROCESS_V2_OVERWRITE" and c2 == 200 and len(g["data"]["process"].get("graph", {}).get("nodes", [])) == 2,
              f"{c} {b.get('code')}")
        rid = create_quote(env, t["staff"], "S3a2-G Q5 (quy trình đồ thị)")
        q5 = submit(env, t["staff"], rid)
        made.append(q5)
        pin = env.sql(f"SELECT JSON_LENGTH(process_snapshot, '$.steps'), JSON_LENGTH(process_snapshot, '$.graph.nodes') "
                      f"FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{q5}'")[0]
        check("(7) test@ gửi Q5 ⇒ hồ sơ ghim {steps: [], graph 2 nút} (bản trước S3a2 sẽ từ chối, không đi nhầm)", pin == ["0", "2"], str(pin))
        s = env.seats(q5)
        check("(7) ghế mở trên mã nút truong-phong cho duyet1", len(s) == 1 and s[0][:2] == ["truong-phong", "open"] and s[0][3] == d1, str(s))
        check("(7) duyet1 thấy Q5", q5 in env.pending(t["duyet1"]))
        c, det = env.req("GET", f"/approvals/{q5}", t["duyet1"])
        names = [x.get("name") for x in (det.get("data", {}).get("approval", det.get("data", {})).get("process") or {}).get("steps", [])]
        check("(7) chi tiết hiện chuỗi nút đã ghim", c == 200 and names == ["Trưởng phòng", "Giám đốc"], f"{c} {names}")
        c, _ = env.req("POST", f"/approvals/{q5}/approve", t["duyet1"], {"note": "ok"})
        s = env.seats(q5)
        check("(7) duyet1 duyệt ⇒ ghế giam-doc mở cho duyet2", c == 200 and s[-1][:2] == ["giam-doc", "open"] and s[-1][3] == d2
              and q5 in env.pending(t["duyet2"]), f"{c} {s}")
        c, _ = env.req("POST", f"/approvals/{q5}/approve", t["duyet2"], {"note": "ok"})
        c2, rec = env.req("GET", f"/crm_quotes/{rid}", t["staff"])
        status = (rec.get("data") or {}).get("status") or (rec.get("data") or {}).get("data", {}).get("status")
        check("(7) duyet2 duyệt ⇒ xong, báo giá của test@ thành «Chấp nhận»", c == 200 and c2 == 200 and status == "Chấp nhận", f"{c} {c2} {status}")

        # (8) Rollback onto THIS binary: a later slice (S4/S8) stored keys this binary does not run —
        # `approvers`+`completion: all` on the process, `onReject` on an instance's pin. encoding/json
        # drops unknown keys, so without the guard node 1 walks as ONE approver. Fail closed: no new
        # instance, no decision, and the pin keeps the key (never re-written stripped).
        q6 = submit(env, t["staff"], create_quote(env, t["staff"], "S3a2-G Q6 (ghim trước khi lùi bản)"))
        made.append(q6)
        env.sql(f"UPDATE sys_approval_processes SET graph = JSON_SET(graph, '$.nodes[0].completion', JSON_OBJECT('mode', 'all'), "
                f"'$.nodes[0].approvers', JSON_ARRAY(JSON_OBJECT('type', 'user', 'id', '{d1}'), JSON_OBJECT('type', 'user', 'id', '{d3}'))) "
                f"WHERE tenant_id = '{env.tenant}' AND id = '{graph_proc}'")
        env.sql(f"UPDATE sys_approval_instances SET process_snapshot = JSON_SET(process_snapshot, '$.graph.nodes[0].onReject', 'giam-doc') "
                f"WHERE tenant_id = '{env.tenant}' AND id = '{q6}'")
        rid7 = create_quote(env, t["staff"], "S3a2-G Q7 (sau khi lùi bản)")
        c, b = env.req("POST", f"/crm_quotes/{rid7}/approvals/submit", t["staff"])
        armed = env.sql(f"SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND record_id = '{rid7}'")[0][0]
        check("(8) quy trình mang khoá lát sau (approvers + completion: all) ⇒ test@ gửi Q7 bị 422 APPROVAL_NO_PROCESS, không hồ sơ nào",
              c == 422 and b.get("code") == "APPROVAL_NO_PROCESS" and armed == "0", f"{c} {b.get('code')} armed={armed}")
        if c == 201:
            made.append((b["data"].get("approval") or b["data"]).get("id"))
        c, b = env.req("POST", f"/approvals/{q6}/approve", t["duyet1"], {"note": "sau lùi bản"})
        row = env.sql(f"SELECT status, current_step, JSON_UNQUOTE(JSON_EXTRACT(process_snapshot, '$.graph.nodes[0].onReject')) "
                      f"FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{q6}'")[0]
        check("(8) hồ sơ ghim đồ thị có onReject ⇒ duyet1 KHÔNG quyết được, hồ sơ vẫn chờ ở nút 1, bản ghim giữ nguyên khoá",
              c != 200 and row == ["pending", "0", "giam-doc"], f"{c} {b.get('code')} {row}")
    finally:
        ours = [q["Q1"]["iid"], q["Q2"]["iid"]] + made
        for iid in ours:
            env.req("POST", f"/approvals/{iid}/recall", t["staff"], {"note": "dọn nghiệm thu"})
        for pid in (st["proc"], graph_proc):
            if pid:
                env.req("DELETE", f"/admin/approval-processes/{pid}", t["admin"])
        left = env.sql(f"SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND status = 'pending' "
                       f"AND id IN ('" + "', '".join(ours) + "')")[0][0]
        print(f"dọn: {left} hồ sơ nghiệm thu còn chờ; quy trình nghiệm thu đã xoá")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("phase", choices=("pre", "post"))
    ap.add_argument("--base", required=True, help="API base of the server running over the clone")
    ap.add_argument("--db", required=True, help="the CLONE database the server runs on")
    ap.add_argument("--state", help="file carrying pre's snapshot to post (default: temp dir, per db)")
    a = ap.parse_args()
    if a.db == "nexus_platform":
        sys.exit("refusing the shared dev DB — clone it first (nexus_wt_<slug>)")
    state = a.state or os.path.join(tempfile.gettempdir(), f"acceptance-bpm-seats-read-{a.db}.json")
    {"pre": pre, "post": post}[a.phase](Env(a.base, a.db), state)
    fails = seats_mod.fails
    print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)}): {fails}")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
