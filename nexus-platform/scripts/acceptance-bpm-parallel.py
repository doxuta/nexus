#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S4 của specs/043 (`bpm-parallel-approval`, DAILY-QUEUE #116, T090).

Một bước duyệt có NHIỀU người (hàng r10 của ENGINE-COVERAGE-SPEC §E5). Chạy trên server dựng từ DB CLONE
(không bao giờ DB dev dùng chung), sau `scripts/fixture-bpm.py`. Mọi thao tác duyệt bằng TOKEN KHÔNG-ADMIN
(`duyet1/2/3`, `hotro1`, `test@example.com` gửi); admin chỉ cấu hình quy trình và dùng «quyết thay».

  1. Cấu hình qua API: `steps[i].approvers` + `completion` lưu thành dạng đồ thị (steps `[]`); cấu hình sai
     (1 người, trùng người, hai nguồn người duyệt, chế độ lạ (quorum mở ở S6b: acceptance-bpm-rr-quorum.py)) ⇒ 422 APPROVAL_STEP_INVALID đúng trường.
  2. «Tất cả» (duyet1, duyet2): cả hai thấy việc, hotro1 không; duyet1 duyệt ⇒ vẫn chờ, `GET /approvals/:id`
     bằng token duyet1 trả «1/2 — cần 2», việc biến khỏi hộp duyet1 nhưng còn ở hộp duyet2, báo giá chưa đổi;
     duyet1 bấm lại ⇒ 409 APPROVAL_ALREADY_DECIDED_BY_ACTOR; duyet2 duyệt ⇒ xong, báo giá của test@ thành
     «Chấp nhận».
  3. «Người đầu tiên»: duyet2 từ chối thiếu lý do ⇒ 422; có lý do ⇒ hồ sơ bị từ chối, việc biến khỏi hộp
     duyet1, báo giá «Từ chối», duyet1 bấm sau ⇒ 422 APPROVAL_NOT_PENDING.
  4. Một người hai ghế (tên + vai manager): duyet1 duyệt rồi bấm lại ⇒ 409; duyet3 (cùng vai) lấy ghế vai.
  5. Admin không có ghế: duyệt thẳng ⇒ 409 APPROVAL_ADMIN_OVERRIDE_REQUIRED; «quyết thay» thiếu lý do ⇒ 422;
     duyet1 (không-admin) gọi route quyết thay ⇒ 403; admin quyết thay có lý do ⇒ xong, nhật ký đánh dấu
     via=override, các ghế còn lại superseded.
  6. Hai người cuối bấm CÙNG LÚC (8 hồ sơ «tất cả» 3 người): 16/16 yêu cầu 200 (không mất phiếu, không 409),
     mỗi hồ sơ đúng MỘT lần chuyển «đã duyệt», đúng 3 mục nhật ký (2 partial), 3 ghế done.

    python3 scripts/fixture-bpm.py --base http://localhost:8701/api/v1
    python3 scripts/acceptance-bpm-parallel.py --base http://localhost:8701/api/v1 --db nexus_wt_bpm_parallel_approval

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: stdlib + `docker exec nexus-mysql mysql` để đọc bảng ghế (cùng khuôn acceptance-bpm-seats.py, mà script này nạp lại).
"""
import argparse
import importlib.util
import json
import os
import sys
import threading

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check = seats_mod.Env, seats_mod.check

PROC = "S4 nghiệm thu — hội đồng"
OUTCOME = {"statusField": "status", "approvedValue": "Chấp nhận", "rejectedValue": "Từ chối"}


def quote_status(env, tok, rid):
    c, rec = env.req("GET", f"/crm_quotes/{rid}", tok)
    d = rec.get("data") or {}
    return d.get("status") or (d.get("data") or {}).get("status")


def record_of(env, iid):
    return env.sql(f"SELECT record_id FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{iid}'")[0][0]


def create_quote(env, tok, name):
    c, b = env.req("POST", "/crm_quotes", tok, {"name": name, "status": "Nháp"})
    assert c == 201, (c, b)
    return b["data"].get("id") or b["data"].get("record", {}).get("id")


def send(env, tok, name):
    rid = create_quote(env, tok, name)
    c, b = env.req("POST", f"/crm_quotes/{rid}/approvals/submit", tok)
    assert c == 201, (c, b)
    inst = b["data"].get("approval") or b["data"]
    return inst.get("ID") or inst.get("id"), rid


def history(env, iid):
    raw = env.sql(f"SELECT history FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{iid}'")[0][0]
    return json.loads(raw)


def state(env, iid):
    r = env.sql(f"SELECT current_step, status FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{iid}'")[0]
    return int(r[0]), r[1]


def seat_counts(env, iid):
    """(open, approved, rejected, superseded) of the council step (node s1)."""
    rows = env.seats(iid)
    o = sum(1 for r in rows if r[0] == "s1" and r[1] == "open")
    a = sum(1 for r in rows if r[0] == "s1" and r[2] == "approved")
    j = sum(1 for r in rows if r[0] == "s1" and r[2] == "rejected")
    s = sum(1 for r in rows if r[0] == "s1" and r[2] == "superseded")
    return o, a, j, s


def make_process(env, admin, suffix, approvers, mode, outcome=True):
    body = {"name": f"{PROC} — {suffix}", "entityName": "crm_quotes", "trigger": "manual", "isActive": True,
            "steps": [{"name": "Hội đồng", "approvers": approvers, "completion": {"mode": mode}}]}
    if outcome:
        body["outcome"] = OUTCOME
    c, b = env.req("POST", "/admin/approval-processes", admin, body)
    assert c == 201, (c, b)
    return b["data"]["process"]


def drop_process(env, admin, pid):
    c, _ = env.req("DELETE", f"/admin/approval-processes/{pid}", admin)
    check("dọn: xoá quy trình nghiệm thu (hồ sơ đã ghim vẫn chạy)", c == 204, str(c))


def run(env):
    t = env.tokens()
    admin = t["admin"]
    procs = env.req("GET", "/admin/approval-processes", admin)[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]]
    assert not rivals, f"another active manual crm_quotes process would take the submit: {rivals}"
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", admin)[1]["data"]["users"]}
    d1, d2, d3 = (users[f"duyet{i}@example.com"] for i in (1, 2, 3))
    u = lambda x: {"type": "user", "id": x}

    # ---- 1. config over the API ---------------------------------------------------------------
    p = make_process(env, admin, "cấu hình", [u(d1), u(d2)], "all")
    node = (p.get("graph") or {}).get("nodes", [{}])[0]
    check("cấu hình: steps lưu thành đồ thị (steps [] + 1 nút có approvers + completion all)",
          p.get("steps") in ([], None) and len(node.get("approvers", [])) == 2 and node.get("completion", {}).get("mode") == "all"
          and "approverType" not in node, f"steps={p.get('steps')} node={node}")
    drop_process(env, admin, p["id"])
    base = {"name": f"{PROC} — sai", "entityName": "crm_quotes", "trigger": "manual", "isActive": True}
    for label, step in (
        ("1 người", {"approvers": [u(d1)]}),
        ("trùng người", {"approvers": [u(d1), u(d1)]}),
        ("hai nguồn người duyệt", {"approverType": "user", "approverId": d1, "approvers": [u(d2), u(d3)]}),
        ("chế độ lạ (quorum đã mở ở S6b — majority thì chưa)", {"approvers": [u(d1), u(d2)], "completion": {"mode": "majority"}}),
    ):
        c, b = env.req("POST", "/admin/approval-processes", admin, {**base, "steps": [step]})
        check(f"cấu hình sai ({label}) ⇒ 422 APPROVAL_STEP_INVALID, không lưu gì",
              c == 422 and b.get("code") == "APPROVAL_STEP_INVALID", f"{c} {b.get('code')} {b.get('error')}")

    # ---- 2. all ---------------------------------------------------------------------------------
    p = make_process(env, admin, "tất cả", [u(d1), u(d2)], "all")
    try:
        q, rid = send(env, t["staff"], "S4 tất cả")
        check("cả duyet1 và duyet2 thấy việc, hotro1 không", q in env.pending(t["duyet1"]) and q in env.pending(t["duyet2"]) and q not in env.pending(t["hotro1"]))
        c, b = env.req("POST", f"/approvals/{q}/approve", t["duyet1"], {"note": "ok"})
        check("duyet1 duyệt ⇒ 200, hồ sơ vẫn chờ ở bước hội đồng", c == 200 and state(env, q) == (0, "pending"), f"{c} {b.get('code')} {state(env, q)}")
        c, det = env.req("GET", f"/approvals/{q}", t["duyet1"])
        a = (det.get("data") or {}).get("approval") or {}
        comp = a.get("completion") or {}
        check("duyet1 mở chi tiết (đã bỏ phiếu vẫn xem được): 1/2 — cần 2, 2 ghế (1 done)",
              c == 200 and comp.get("approved") == 1 and comp.get("total") == 2 and comp.get("required") == 2 and comp.get("mode") == "all"
              and len(a.get("seats", [])) == 2 and sum(1 for s in a["seats"] if s["status"] == "done") == 1, f"{c} {comp} {a.get('seats')}")
        check("việc biến khỏi hộp duyet1, còn ở hộp duyet2", q not in env.pending(t["duyet1"]) and q in env.pending(t["duyet2"]))
        check("báo giá chưa đổi trạng thái khi mới đủ 1/2", quote_status(env, t["staff"], rid) == "Nháp", str(quote_status(env, t["staff"], rid)))
        c, b = env.req("POST", f"/approvals/{q}/approve", t["duyet1"], {"note": "bấm lại"})
        check("duyet1 bấm lại ⇒ 409 APPROVAL_ALREADY_DECIDED_BY_ACTOR", c == 409 and b.get("code") == "APPROVAL_ALREADY_DECIDED_BY_ACTOR", f"{c} {b.get('code')}")
        c, b = env.req("POST", f"/approvals/{q}/approve", t["hotro1"], {"note": "x"})
        check("hotro1 (không ngồi ghế) ⇒ 422 APPROVAL_NOT_AUTHORIZED", c == 422 and b.get("code") == "APPROVAL_NOT_AUTHORIZED", f"{c} {b.get('code')}")
        c, b = env.req("POST", f"/approvals/{q}/approve", t["duyet2"], {"note": "ok"})
        st = quote_status(env, t["staff"], rid)
        check("duyet2 duyệt ⇒ xong, test@ thấy báo giá «Chấp nhận»", c == 200 and state(env, q) == (0, "approved") and st == "Chấp nhận", f"{c} {state(env, q)} {st}")
        check("nhật ký: 2 mục «đã duyệt» (1 partial) · 2 ghế done/approved", [h.get("partial", False) for h in history(env, q) if h["action"] == "approved"] == [True, False]
              and seat_counts(env, q) == (0, 2, 0, 0), f"{history(env, q)} {seat_counts(env, q)}")
    finally:
        drop_process(env, admin, p["id"])

    # ---- 3. any ---------------------------------------------------------------------------------
    p = make_process(env, admin, "người đầu tiên", [u(d1), u(d2), u(d3)], "any")
    try:
        q, rid = send(env, t["staff"], "S4 người đầu tiên")
        c, b = env.req("POST", f"/approvals/{q}/reject", t["duyet2"], {"note": "  "})
        check("duyet2 từ chối thiếu lý do ⇒ 422 APPROVAL_REJECT_REASON_REQUIRED, hồ sơ nguyên", c == 422 and b.get("code") == "APPROVAL_REJECT_REASON_REQUIRED" and state(env, q) == (0, "pending"), f"{c} {b.get('code')}")
        c, b = env.req("POST", f"/approvals/{q}/reject", t["duyet2"], {"note": "thiếu hồ sơ pháp lý"})
        check("duyet2 từ chối có lý do ⇒ hồ sơ bị từ chối ngay", c == 200 and state(env, q) == (0, "rejected"), f"{c} {b.get('code')} {state(env, q)}")
        check("việc biến khỏi hộp duyet1 và duyet3", q not in env.pending(t["duyet1"]) and q not in env.pending(t["duyet3"]))
        check("báo giá thành «Từ chối»; ghế còn lại superseded", quote_status(env, t["staff"], rid) == "Từ chối" and seat_counts(env, q) == (0, 0, 1, 2),
              f"{quote_status(env, t['staff'], rid)} {seat_counts(env, q)}")
        c, b = env.req("POST", f"/approvals/{q}/approve", t["duyet1"], {"note": "đến muộn"})
        check("duyet1 bấm sau khi đã xong ⇒ 422 APPROVAL_NOT_PENDING", c == 422 and b.get("code") == "APPROVAL_NOT_PENDING", f"{c} {b.get('code')}")
    finally:
        drop_process(env, admin, p["id"])

    # ---- 4. one person, two seats ----------------------------------------------------------------
    mgr_role = env.sql(f"SELECT role_id FROM users WHERE tenant_id = '{env.tenant}' AND email = 'duyet1@example.com'")[0][0]
    p = make_process(env, admin, "hai ghế", [u(d1), {"type": "role", "id": mgr_role}, u(d2)], "all")
    try:
        q, rid = send(env, t["staff"], "S4 hai ghế")
        c1, _ = env.req("POST", f"/approvals/{q}/approve", t["duyet1"], {"note": "ghế tên"})
        c2, b2 = env.req("POST", f"/approvals/{q}/approve", t["duyet1"], {"note": "ghế vai"})
        check("duyet1 (tên + vai manager) duyệt một lần, lần hai ⇒ 409 APPROVAL_ALREADY_DECIDED_BY_ACTOR, chỉ 1 ghế done",
              c1 == 200 and c2 == 409 and b2.get("code") == "APPROVAL_ALREADY_DECIDED_BY_ACTOR" and seat_counts(env, q)[1] == 1, f"{c1} {c2} {b2.get('code')} {seat_counts(env, q)}")
        c3, _ = env.req("POST", f"/approvals/{q}/approve", t["duyet3"], {"note": "ghế vai"})
        check("duyet3 (cùng vai manager) lấy ghế vai ⇒ 200, hồ sơ chờ duyet2", c3 == 200 and state(env, q) == (0, "pending") and seat_counts(env, q)[1] == 2, f"{c3} {state(env, q)} {seat_counts(env, q)}")
        c4, _ = env.req("POST", f"/approvals/{q}/approve", t["duyet2"], {"note": "ok"})
        check("duyet2 duyệt ⇒ xong, báo giá «Chấp nhận»", c4 == 200 and state(env, q) == (0, "approved") and quote_status(env, t["staff"], rid) == "Chấp nhận", f"{c4} {state(env, q)}")
    finally:
        drop_process(env, admin, p["id"])

    # ---- 5. admin without a seat -----------------------------------------------------------------
    p = make_process(env, admin, "quyết thay", [u(d1), u(d2), u(d3)], "all")
    try:
        q, rid = send(env, t["staff"], "S4 quyết thay")
        c, b = env.req("POST", f"/approvals/{q}/approve", admin, {"note": "tôi là admin"})
        check("admin duyệt thẳng bước hội đồng ⇒ 409 APPROVAL_ADMIN_OVERRIDE_REQUIRED, hồ sơ nguyên", c == 409 and b.get("code") == "APPROVAL_ADMIN_OVERRIDE_REQUIRED" and seat_counts(env, q)[0] == 3, f"{c} {b.get('code')}")
        c, b = env.req("POST", f"/admin/approval-instances/{q}/override", admin, {"decision": "approve"})
        check("quyết thay thiếu lý do ⇒ 422 APPROVAL_OVERRIDE_NOTE_REQUIRED", c == 422 and b.get("code") == "APPROVAL_OVERRIDE_NOTE_REQUIRED", f"{c} {b.get('code')}")
        c, b = env.req("POST", f"/admin/approval-instances/{q}/override", t["duyet1"], {"decision": "approve", "note": "thử"})
        check("duyet1 (không-admin) gọi quyết thay ⇒ 403, hồ sơ nguyên", c == 403 and state(env, q) == (0, "pending"), f"{c} {b.get('code')}")
        c, b = env.req("POST", f"/approvals/{q}/delegate", t["duyet1"], {"toUserId": d2})
        check("duyet1 uỷ quyền ghế cho duyet2 (đã có ghế) ⇒ 409 APPROVAL_DELEGATE_TARGET_SEATED, vẫn 3 ghế mở",
              c == 409 and b.get("code") == "APPROVAL_DELEGATE_TARGET_SEATED" and seat_counts(env, q)[0] == 3, f"{c} {b.get('code')} {seat_counts(env, q)}")
        c, b = env.req("POST", f"/admin/approval-instances/{q}/override", admin, {"decision": "approve", "note": "tab cũ", "expectedStep": 5})
        check("quyết thay từ màn hình đã cũ (expectedStep sai) ⇒ 409 APPROVAL_CONFLICT, hồ sơ nguyên",
              c == 409 and b.get("code") == "APPROVAL_CONFLICT" and state(env, q) == (0, "pending"), f"{c} {b.get('code')} {state(env, q)}")
        c, b = env.req("POST", f"/admin/approval-instances/{q}/override", admin, {"decision": "approve", "note": "hội đồng họp bất thường"})
        last = history(env, q)[-1]
        check("admin quyết thay có lý do ⇒ xong, báo giá «Chấp nhận», nhật ký via=override, 3 ghế superseded",
              c == 200 and state(env, q) == (0, "approved") and quote_status(env, t["staff"], rid) == "Chấp nhận"
              and last.get("via") == "override" and last.get("comment") == "hội đồng họp bất thường" and seat_counts(env, q) == (0, 0, 0, 3),
              f"{c} {b.get('code')} {last} {seat_counts(env, q)}")
        check("việc biến khỏi hộp cả ba người duyệt", all(q not in env.pending(t[n]) for n in ("duyet1", "duyet2", "duyet3")))
    finally:
        drop_process(env, admin, p["id"])

    # ---- 6. the last two press together ----------------------------------------------------------
    p = make_process(env, admin, "đua", [u(d1), u(d2), u(d3)], "all")
    try:
        made = [send(env, t["staff"], f"S4 đua {n}") for n in range(8)]
        for q, _ in made:
            c, _ = env.req("POST", f"/approvals/{q}/approve", t["duyet1"], {"note": "đầu tiên"})
            assert c == 200, c
        results = {}
        barrier = threading.Barrier(2)

        def press(q, who):
            barrier.wait()
            results[(q, who)] = env.req("POST", f"/approvals/{q}/approve", t[who], {"note": who})[0]

        for q, _ in made:
            barrier.reset()
            ths = [threading.Thread(target=press, args=(q, w)) for w in ("duyet2", "duyet3")]
            for th in ths:
                th.start()
            for th in ths:
                th.join()
        codes = sorted(results.values())
        check("16 lần bấm của 2 người cuối trên 8 hồ sơ: 16/16 đều 200 (không mất phiếu, không 409)", codes == [200] * 16, str(results))
        good = 0
        for q, rid in made:
            hist = [h for h in history(env, q) if h["action"] == "approved"]
            if (state(env, q) == (0, "approved") and len(hist) == 3 and sum(1 for h in hist if h.get("partial")) == 2
                    and seat_counts(env, q) == (0, 3, 0, 0) and quote_status(env, t["staff"], rid) == "Chấp nhận"):
                good += 1
        check("mỗi hồ sơ: chuyển «đã duyệt» đúng MỘT lần, 3 mục nhật ký (2 partial), 3 ghế done, báo giá «Chấp nhận»", good == 8, f"{good}/8")
    finally:
        drop_process(env, admin, p["id"])


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--base", required=True, help="API base of the server running over the clone")
    ap.add_argument("--db", required=True, help="the CLONE database the server runs on")
    a = ap.parse_args()
    if a.db == "nexus_platform":
        sys.exit("refusing the shared dev DB — clone it first (nexus_wt_<slug>)")
    run(Env(a.base, a.db))
    fails = seats_mod.fails
    print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)}): {fails}")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
