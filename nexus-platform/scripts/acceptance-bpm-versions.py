#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S10 của specs/043 (`bpm-process-versions`, DAILY-QUEUE #124, T170).

Sửa quy trình ở BẢN NHÁP, xuất bản thành phiên bản mới, xem lại lịch sử; mỗi hồ sơ ghi rõ nó chạy phiên bản
nào. Người dựng là admin; người dùng của khách là test@example.com (người gửi) và duyet1/duyet2 (người duyệt,
không-admin — `scripts/fixture-bpm.py` chạy trước trên cùng server). Một pha, trên DB CLONE:

  1. admin tạo quy trình thủ công 1 bước (duyet1) ⇒ phiên bản 1.
  2. admin LƯU NHÁP thêm bước duyet2 ⇒ quy trình vẫn v1, bản đang dùng vẫn 1 bước.
  3. test@ gửi báo giá A (nháp đang treo) ⇒ chi tiết (token duyet1) "Phiên bản 1", 1 bước — nháp không khởi động.
  4. admin XUẤT BẢN (ghi chú) ⇒ v2; lịch sử [v2, v1]; v1 còn nguyên 1 bước, v2 có 2.
  5. test@ gửi báo giá B ⇒ "Phiên bản 2", 2 bước. A vẫn v1.
  6. duyet1 thấy cả A, B; duyệt A ⇒ A xong ngay (v1 một bước); duyệt B ⇒ sang duyet2; duyet2 duyệt ⇒ xong.
     Dòng lịch sử mang đúng phiên bản (A = 1, B = 2) — kiểm bằng API VÀ bằng SQL độc lập (cột process_version).
  7. Nháp cũ: admin lưu nháp rồi LƯU TRỰC TIẾP (PUT cũ = lưu + xuất bản ⇒ v3) ⇒ xuất bản nháp cũ bị 409
     APPROVAL_DRAFT_STALE; bỏ nháp ⇒ 204; xuất bản khi không còn nháp ⇒ 409 APPROVAL_VERSION_NO_DRAFT.
  8. Lưu trực tiếp y nguyên ⇒ KHÔNG tăng phiên bản.
  9. Người không-admin (test@, duyet1) gọi 5 route của lát ⇒ 403, không đọc được lịch sử.
  Cuối lượt xoá quy trình nghiệm thu (hồ sơ đã ghim vẫn chạy tới khi xong).

    python3 scripts/fixture-bpm.py --base http://localhost:8711/api/v1
    python3 scripts/acceptance-bpm-versions.py --base http://localhost:8711/api/v1 --db nexus_wt_bpm_process_versions

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

NAME = "S10 nghiệm thu — duyệt báo giá"
TAG = "S10-"


def criteria():
    return {"match": "and", "rules": [{"field": "name", "op": "contains", "value": TAG}]}


def body(steps, name=NAME):
    return {"name": name, "entityName": "crm_quotes", "trigger": "manual", "isActive": True,
            "criteria": criteria(), "steps": steps}


def detail(env, tok, iid):
    c, b = env.req("GET", f"/approvals/{iid}", tok)
    assert c == 200, (c, b)
    return b["data"]["approval"]


def run(env):
    t = env.tokens()
    admin = t["admin"]
    procs = env.req("GET", "/admin/approval-processes", admin)[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]]
    assert not rivals, f"another active manual crm_quotes process would take the submit: {rivals}"
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", admin)[1]["data"]["users"]}
    d1, d2 = users["duyet1@example.com"], users["duyet2@example.com"]
    step1 = {"id": "s1", "name": "Trưởng phòng", "approverType": "user", "approverId": d1}
    step2 = {"id": "s2", "name": "Kế toán", "approverType": "user", "approverId": d2}

    # 1. version 1
    c, b = env.req("POST", "/admin/approval-processes", admin, body([step1]))
    assert c == 201, (c, b)
    proc = b["data"]["process"]
    pid = proc["id"]
    check("1. quy trình mới là phiên bản 1, chưa có nháp", proc["version"] == 1 and not proc.get("draft"), str(proc.get("version")))
    base = f"/admin/approval-processes/{pid}"

    # 2. draft: the active definition does not move
    c, b = env.req("PUT", base + "/draft", admin, body([step1, step2]))
    p = b.get("data", {}).get("process", {})
    check("2. lưu nháp ⇒ 200, có nháp (dựa trên v1), quy trình vẫn v1 với 1 bước",
          c == 200 and p.get("version") == 1 and p.get("draft", {}).get("baseVersion") == 1 and len(p.get("steps", [])) == 1,
          f"{c} {b.get('code')}")
    c, b = env.req("PUT", base + "/draft", admin, {**body([step1]), "steps": [{**step1, "assignment": {"type": "field"}}]})
    check("2b. nháp bị chặn bằng ĐÚNG luật của lưu (khoá lạ ⇒ 422 APPROVAL_NODE_KIND_UNSUPPORTED)",
          c == 422 and b.get("code") == "APPROVAL_NODE_KIND_UNSUPPORTED", f"{c} {b.get('code')}")
    c, b = env.req("PUT", base + "/draft", admin, body([step1, step2]))
    check("2c. lưu nháp lần hai thay nháp cũ", c == 200, str(c))

    # 3. a hồ sơ started with a draft pending runs the ACTIVE definition
    ra = env.req("POST", "/crm_quotes", t["staff"], {"name": TAG + "A", "status": "Nháp"})[1]["data"]
    ra = ra.get("id") or ra.get("record", {}).get("id")
    c, b = env.req("POST", f"/crm_quotes/{ra}/approvals/submit", t["staff"])
    assert c == 201, (c, b)
    ia = (b["data"].get("approval") or b["data"]).get("ID") or (b["data"].get("approval") or b["data"]).get("id")
    da = detail(env, t["duyet1"], ia)
    check("3. A gửi khi nháp đang treo ⇒ duyet1 (không-admin) thấy 'Phiên bản 1', 1 bước",
          da["process"].get("version") == 1 and len(json_steps(da)) == 1, f"v={da['process'].get('version')} steps={len(json_steps(da))}")

    # 4. publish
    c, b = env.req("POST", base + "/publish", admin, {"note": "thêm bước Kế toán"})
    pub = b.get("data", {})
    check("4. xuất bản ⇒ 200, phiên bản 2, không còn nháp, bước mới đang dùng",
          c == 200 and pub.get("version") == 2 and pub["process"]["version"] == 2 and not pub["process"].get("draft")
          and len(pub["process"]["steps"]) == 2, f"{c} {b.get('code')}")
    c, b = env.req("GET", base + "/versions", admin)
    vs = b.get("data", {}).get("versions", [])
    check("4b. lịch sử [v2, v1], v2 mang ghi chú, danh sách không kèm định nghĩa",
          c == 200 and [v["version"] for v in vs] == [2, 1] and vs[0].get("note") == "thêm bước Kế toán"
          and all("definition" not in v for v in vs), str(vs))
    d_v1 = env.req("GET", base + "/versions/1", admin)[1]["data"]["version"]["definition"]
    d_v2 = env.req("GET", base + "/versions/2", admin)[1]["data"]["version"]["definition"]
    check("4c. v1 còn NGUYÊN 1 bước, v2 có 2 bước", len(d_v1["steps"]) == 1 and len(d_v2["steps"]) == 2,
          f"{len(d_v1['steps'])}/{len(d_v2['steps'])}")
    c, b = env.req("GET", base + "/versions/99", admin)
    check("4d. phiên bản không có ⇒ 404 APPROVAL_VERSION_NOT_FOUND", c == 404 and b.get("code") == "APPROVAL_VERSION_NOT_FOUND", f"{c} {b.get('code')}")

    # 5. a hồ sơ started after publish runs v2; A stays on v1
    rb = env.req("POST", "/crm_quotes", t["staff"], {"name": TAG + "B", "status": "Nháp"})[1]["data"]
    rb = rb.get("id") or rb.get("record", {}).get("id")
    c, b = env.req("POST", f"/crm_quotes/{rb}/approvals/submit", t["staff"])
    assert c == 201, (c, b)
    ib = (b["data"].get("approval") or b["data"]).get("ID") or (b["data"].get("approval") or b["data"]).get("id")
    db_, da = detail(env, t["duyet1"], ib), detail(env, t["duyet1"], ia)
    check("5. B gửi sau xuất bản ⇒ 'Phiên bản 2', 2 bước; A vẫn 'Phiên bản 1'",
          db_["process"].get("version") == 2 and len(json_steps(db_)) == 2 and da["process"].get("version") == 1,
          f"B v{db_['process'].get('version')} A v{da['process'].get('version')}")

    # 6. both generations decide; history carries the right version
    pend = env.pending(t["duyet1"])
    check("6. duyet1 thấy cả A (v1) và B (v2) trên hộp duyệt", ia in pend and ib in pend)
    c, _ = env.req("POST", f"/approvals/{ia}/approve", t["duyet1"], {"note": "ok A"})
    da = detail(env, t["duyet1"], ia)
    check("6a. duyet1 duyệt A ⇒ xong ngay (v1 chỉ một bước)", c == 200 and da["status"] == "approved", f"{c} {da['status']}")
    c, _ = env.req("POST", f"/approvals/{ib}/approve", t["duyet1"], {"note": "ok B"})
    check("6b. duyet1 duyệt B ⇒ sang duyet2", c == 200 and ib in env.pending(t["duyet2"]), str(c))
    c, _ = env.req("POST", f"/approvals/{ib}/approve", t["duyet2"], {"note": "ok B2"})
    db_ = detail(env, t["duyet2"], ib)
    check("6c. duyet2 duyệt B ⇒ xong", c == 200 and db_["status"] == "approved", f"{c} {db_['status']}")
    ha = {h.get("version") for h in da["history"]}
    hb = {h.get("version") for h in db_["history"]}
    check("6d. mọi dòng lịch sử của A mang phiên bản 1, của B mang 2 (API)", ha == {1} and hb == {2}, f"A={ha} B={hb}")
    rows = env.sql(f"SELECT id, process_version, JSON_EXTRACT(process_snapshot, '$.version') FROM sys_approval_instances "
                   f"WHERE tenant_id = '{env.tenant}' AND id IN ('{ia}', '{ib}')")
    got = {r[0]: (r[1], r[2]) for r in rows}
    check("6e. SQL độc lập: cột process_version + bản ghim = A:1, B:2",
          got.get(ia) == ("1", "1") and got.get(ib) == ("2", "2"), str(got))
    n_ver = env.sql(f"SELECT COUNT(*) FROM sys_approval_process_versions WHERE tenant_id = '{env.tenant}' AND process_id = '{pid}'")[0][0]
    check("6f. SQL độc lập: đúng 2 hàng phiên bản của quy trình", n_ver == "2", n_ver)

    # 7. a stale draft cannot revert a later change
    c, _ = env.req("PUT", base + "/draft", admin, body([step1, step2], name=NAME + " (nháp cũ)"))
    check("7. lưu nháp trên v2", c == 200, str(c))
    c, b = env.req("PUT", base, admin, body([step1, {**step2, "name": "Kế toán trưởng"}]))
    check("7a. LƯU TRỰC TIẾP (PUT cũ) = lưu + xuất bản ngay ⇒ v3", c == 200 and b["data"]["process"]["version"] == 3, f"{c} {b.get('code')}")
    c, b = env.req("POST", base + "/publish", admin, {})
    check("7b. xuất bản nháp lập trên v2 khi bản đang dùng là v3 ⇒ 409 APPROVAL_DRAFT_STALE",
          c == 409 and b.get("code") == "APPROVAL_DRAFT_STALE", f"{c} {b.get('code')}")
    cur = env.req("GET", base, admin)[1]["data"]["process"]
    check("7c. lần xuất bản bị từ chối không đổi gì: vẫn v3, tên Kế toán trưởng",
          cur["version"] == 3 and cur["steps"][1]["name"] == "Kế toán trưởng", f"v{cur['version']}")
    c, _ = env.req("DELETE", base + "/draft", admin)
    check("7d. bỏ nháp ⇒ 204", c == 204, str(c))
    c, b = env.req("DELETE", base + "/draft", admin)
    check("7e. bỏ lần hai ⇒ 404 APPROVAL_DRAFT_NOT_FOUND", c == 404 and b.get("code") == "APPROVAL_DRAFT_NOT_FOUND", f"{c} {b.get('code')}")
    c, b = env.req("POST", base + "/publish", admin, {})
    check("7f. xuất bản khi không còn nháp ⇒ 409 APPROVAL_VERSION_NO_DRAFT",
          c == 409 and b.get("code") == "APPROVAL_VERSION_NO_DRAFT", f"{c} {b.get('code')}")

    # 8. a save that changes nothing publishes nothing
    same = {**body([cur["steps"][0], cur["steps"][1]])}
    for i in (1, 2):
        c, b = env.req("PUT", base, admin, same)
        check(f"8.{i} lưu trực tiếp y nguyên ⇒ vẫn v3", c == 200 and b["data"]["process"]["version"] == 3, f"{c} v{b.get('data', {}).get('process', {}).get('version')}")

    # 9. the customer's own people never reach the builder's routes
    for who in ("staff", "duyet1"):
        for method, path, payload in (("GET", base + "/versions", None), ("GET", base + "/versions/1", None),
                                      ("PUT", base + "/draft", body([step1])), ("DELETE", base + "/draft", None),
                                      ("POST", base + "/publish", {})):
            c, _ = env.req(method, path, t[who], payload)
            check(f"9. {who} (không-admin) {method} …{path.split(pid)[1]} ⇒ 403", c == 403, str(c))

    c, _ = env.req("DELETE", base, admin)
    check("dọn: xoá quy trình nghiệm thu (hồ sơ đã ghim đã xong)", c == 204, str(c))


def json_steps(d):
    import json
    s = d["process"].get("steps")
    return json.loads(s) if isinstance(s, str) else (s or [])


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
