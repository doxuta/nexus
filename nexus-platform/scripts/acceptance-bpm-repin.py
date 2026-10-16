#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S11 của specs/043 (`approval-process-edit-vs-pending`, DAILY-QUEUE #67, T180).

Sửa hoặc xoá một quy trình duyệt KHÔNG áp lên hồ sơ đang chờ; admin được báo "còn N hồ sơ vẫn chạy bản cũ"
và chuyển chúng sang bản mới (chạy thử rồi áp). Người dựng là admin; người dùng của khách là test@example.com
(người gửi) và duyet1/duyet2 (người duyệt, không-admin — `scripts/fixture-bpm.py` chạy trước trên cùng
server). Một pha, trên DB CLONE:

  1. admin tạo quy trình thủ công 1 bước (duyet1) ⇒ v1. test@ gửi báo giá A và B ⇒ cả hai chờ duyet1 (v1).
  2. admin LƯU quy trình thêm bước duyet2 ⇒ v2; câu trả lời của PUT mang `impact.pendingOnOldDefinition = 2`;
     GET /impact = 2 hồ sơ, cả hai ở bước s1; GET /instances?version=1 liệt kê đúng hai hồ sơ đó.
  3. repin KHÔNG nói dryRun ⇒ là chạy thử: 2 dòng ok, DB không đổi (SQL độc lập: process_version vẫn 1).
  4. repin "dryRun": false ⇒ 2 dòng ok; SQL: process_version = 2 và bản ghim v2 (2 bước); duyet1 vẫn thấy cả
     hai trên hộp duyệt; /impact còn 0.
  5. duyet1 duyệt A ⇒ sang duyet2 (bước mới có hiệu lực); duyet2 duyệt ⇒ xong. test@ (người gửi) đọc chi tiết:
     lịch sử có dòng "repinned" từ v1 sang v2.
  6. admin lưu v3 BỎ bước s1; B (đang ở s1, chạy v2) chuyển sang v3 ⇒ dòng bị từ chối
     APPROVAL_REPIN_NODE_MISSING, B ở lại v2 và duyet1 vẫn duyệt được.
  7. admin xoá quy trình: /impact trước khi xoá đếm B; DELETE ⇒ 204 (không chặn, không huỷ); duyet2 vẫn duyệt
     xong B.
  8. test@ và duyet1 (không-admin) gọi 3 route của lát ⇒ 403.

    python3 scripts/fixture-bpm.py --base http://localhost:8700/api/v1
    python3 scripts/acceptance-bpm-repin.py --base http://localhost:8700/api/v1 --db nexus_wt_approval_process_edit_vs_pending

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: dùng lại `Env`/`check` của scripts/acceptance-bpm-seats.py — cùng khuôn, một danh sách lỗi.
"""
import argparse
import importlib.util
import os
import sys

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check = seats_mod.Env, seats_mod.check

NAME = "S11 nghiệm thu — duyệt báo giá"
TAG = "S11-"


def body(steps):
    return {"name": NAME, "entityName": "crm_quotes", "trigger": "manual", "isActive": True,
            "criteria": {"match": "and", "rules": [{"field": "name", "op": "contains", "value": TAG}]}, "steps": steps}


def versions_of(env, ids):
    q = ",".join(f"'{i}'" for i in ids)
    rows = env.sql(f"SELECT id, IFNULL(process_version, 'NULL'), JSON_EXTRACT(process_snapshot, '$.version') "
                   f"FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id IN ({q})")
    return {r[0]: (r[1], r[2]) for r in rows}


def run(env):
    t = env.tokens()
    admin = t["admin"]
    procs = env.req("GET", "/admin/approval-processes", admin)[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]]
    assert not rivals, f"another active manual crm_quotes process would take the submit: {rivals}"
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", admin)[1]["data"]["users"]}
    d1, d2 = users["duyet1@example.com"], users["duyet2@example.com"]
    s1 = {"id": "s1", "name": "Trưởng phòng", "approverType": "user", "approverId": d1}
    s2 = {"id": "s2", "name": "Kế toán", "approverType": "user", "approverId": d2}

    # 1. v1 + two hồ sơ
    c, b = env.req("POST", "/admin/approval-processes", admin, body([s1]))
    assert c == 201, (c, b)
    pid = b["data"]["process"]["id"]
    base = f"/admin/approval-processes/{pid}"
    ia, ib = env.send(t["staff"], TAG + "A"), env.send(t["staff"], TAG + "B")
    pend = env.pending(t["duyet1"])
    check("1. A và B chờ duyet1 (v1)", ia in pend and ib in pend)

    # 2. save v2: the answer says how many hồ sơ still run the old one
    c, b = env.req("PUT", base, admin, body([s1, s2]))
    d = b.get("data", {})
    check("2. PUT ⇒ 200, process v2 VÀ impact.pendingOnOldDefinition = 2",
          c == 200 and d.get("process", {}).get("version") == 2 and (d.get("impact") or {}).get("pendingOnOldDefinition") == 2,
          f"{c} {b.get('code')} impact={d.get('impact')}")
    c, b = env.req("GET", base + "/impact", admin)
    im = b.get("data", {})
    by = {n["nodeId"]: n["count"] for n in im.get("byNode", [])}
    check("2a. GET /impact = 2 cũ / 2 đang chờ, cả hai ở s1 (tên bước kèm theo)",
          c == 200 and im.get("pendingOnOldDefinition") == 2 and im.get("pendingTotal") == 2 and by == {"s1": 2}
          and all(n.get("name") for n in im.get("byNode", [])), f"{c} {im}")
    c, b = env.req("GET", base + "/instances?version=1", admin)
    got = sorted(r["id"] for r in b.get("data", {}).get("instances", []))
    check("2b. GET /instances?version=1 liệt kê đúng A và B", c == 200 and got == sorted([ia, ib]), f"{c} {got}")

    # 3. no dryRun key = dry run
    c, b = env.req("POST", base + "/repin", admin, {})
    res = b.get("data", {}).get("results", [])
    check("3. repin không nói dryRun ⇒ chạy thử: 2 dòng ok", c == 200 and len(res) == 2 and all(r["ok"] for r in res), f"{c} {res}")
    v = versions_of(env, [ia, ib])
    check("3a. SQL độc lập: chạy thử KHÔNG ghi gì (process_version vẫn 1)", all(x[0] == "1" for x in v.values()), str(v))

    # 4. apply
    c, b = env.req("POST", base + "/repin", admin, {"dryRun": False})
    res = b.get("data", {}).get("results", [])
    check("4. repin dryRun:false ⇒ 2 dòng ok, s1→s1, v1→v2",
          c == 200 and len(res) == 2 and all(r["ok"] and r["fromNode"] == "s1" and r["toNode"] == "s1" and r["fromVersion"] == 1
                                              and r["toVersion"] == 2 for r in res), f"{c} {res}")
    v = versions_of(env, [ia, ib])
    check("4a. SQL độc lập: cột process_version = 2 và bản ghim v2", all(x == ("2", "2") for x in v.values()), str(v))
    pend = env.pending(t["duyet1"])
    check("4b. duyet1 (không-admin) vẫn thấy cả A và B trên hộp duyệt", ia in pend and ib in pend)
    c, b = env.req("GET", base + "/impact", admin)
    check("4c. /impact còn 0 hồ sơ chạy bản cũ", b.get("data", {}).get("pendingOnOldDefinition") == 0, str(b.get("data")))
    check("4d. ghế mở đúng 1 và ở s1", env.sql(f"SELECT COUNT(*), MIN(node_id) FROM sys_bpm_tasks WHERE tenant_id = '{env.tenant}' "
                                              f"AND instance_id = '{ia}' AND status = 'open'")[0] == ["1", "s1"])

    # 5. the new step is live; the requester sees the move
    c, _ = env.req("POST", f"/approvals/{ia}/approve", t["duyet1"], {"note": "ok A"})
    check("5. duyet1 duyệt A ⇒ sang duyet2 (bước mới của v2 có hiệu lực)", c == 200 and ia in env.pending(t["duyet2"]), str(c))
    c, _ = env.req("POST", f"/approvals/{ia}/approve", t["duyet2"], {"note": "ok A2"})
    c2, det = env.req("GET", f"/approvals/{ia}", t["staff"])
    appr = det.get("data", {}).get("approval", {})
    moves = [h for h in appr.get("history", []) if h.get("action") == "repinned"]
    check("5a. duyet2 duyệt ⇒ A xong; test@ (người gửi) đọc lịch sử có 'repinned' từ v1 sang v2",
          c == 200 and c2 == 200 and appr.get("status") == "approved" and len(moves) == 1
          and moves[0].get("fromVersion") == 1 and moves[0].get("version") == 2, f"{c}/{c2} {appr.get('status')} {moves}")

    # 6. v3 drops s1: B cannot follow
    c, b = env.req("PUT", base, admin, body([s2]))
    imp = b.get("data", {}).get("impact") or {}
    check("6. lưu v3 (bỏ s1) ⇒ impact báo đúng B (1 hồ sơ, ở s1) — hồ sơ A đã xong KHÔNG được đếm",
          c == 200 and imp.get("pendingOnOldDefinition") == 1 and [(n["nodeId"], n["count"]) for n in imp.get("byNode", [])] == [("s1", 1)], f"{c} {imp}")
    c, b = env.req("POST", base + "/repin", admin, {"dryRun": False})
    res = b.get("data", {}).get("results", [])
    check("6a. repin ⇒ B bị từ chối APPROVAL_REPIN_NODE_MISSING (ok=false)",
          c == 200 and len(res) == 1 and not res[0]["ok"] and res[0]["reason"] == "APPROVAL_REPIN_NODE_MISSING", f"{c} {res}")
    check("6b. B ở lại v2 (SQL)", versions_of(env, [ib])[ib] == ("2", "2"))
    check("6c. duyet1 vẫn thấy B và duyệt được", ib in env.pending(t["duyet1"]))
    c, _ = env.req("POST", f"/approvals/{ib}/approve", t["duyet1"], {"note": "ok B"})
    check("6d. duyet1 duyệt B ⇒ sang duyet2", c == 200 and ib in env.pending(t["duyet2"]), str(c))

    # 7. delete: the number first, then a plain 204; the running hồ sơ finishes
    c, b = env.req("GET", base + "/impact", admin)
    check("7. trước khi xoá: /impact đếm B (1 đang chờ)", b.get("data", {}).get("pendingTotal") == 1, str(b.get("data")))
    c, _ = env.req("DELETE", base, admin)
    check("7a. DELETE ⇒ 204 (không chặn, không huỷ hàng loạt)", c == 204, str(c))
    c, _ = env.req("GET", base + "/impact", admin)
    check("7b. impact của quy trình đã xoá ⇒ 404", c == 404, str(c))
    c, _ = env.req("POST", f"/approvals/{ib}/approve", t["duyet2"], {"note": "ok B2"})
    c2, det = env.req("GET", f"/approvals/{ib}", t["duyet2"])
    check("7c. duyet2 vẫn duyệt xong B sau khi quy trình bị xoá", c == 200 and det["data"]["approval"]["status"] == "approved", f"{c} {c2}")

    # 8. the customer's own people never reach the builder's routes
    for who in ("staff", "duyet1"):
        for method, path, payload in (("GET", base + "/impact", None), ("GET", base + "/instances", None), ("POST", base + "/repin", {"dryRun": False})):
            c, _ = env.req(method, path, t[who], payload)
            check(f"8. {who} (không-admin) {method} …{path.split(pid)[1]} ⇒ 403", c == 403, str(c))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--base", required=True, help="API base of the server running over the clone")
    ap.add_argument("--db", required=True, help="the CLONE database the server runs on")
    a = ap.parse_args()
    if a.db == "nexus_platform":
        sys.exit("refusing the shared dev DB — clone it first (nexus_wt_<slug>)")
    run(Env(a.base, a.db))
    print("KẾT QUẢ:", "ĐẠT" if not seats_mod.fails else f"CHƯA ĐẠT ({len(seats_mod.fails)}): {seats_mod.fails}")
    sys.exit(1 if seats_mod.fails else 0)


if __name__ == "__main__":
    main()
