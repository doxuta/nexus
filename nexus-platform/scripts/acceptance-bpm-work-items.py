#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S5 của specs/043 (`bpm-work-items-claim`, DAILY-QUEUE #117, T106).

Hộp «Việc của tôi»: nhận việc từ hàng đợi của phòng, trả lại, làm việc thường có trường bắt buộc —
bằng token NGƯỜI THẬT không-admin (duyet1..3 vai crm_sales_manager, hotro1 vai tk_agent, test@ nhân viên).
Chạy trên server dựng từ DB CLONE (không bao giờ DB dev dùng chung), sau `scripts/fixture-bpm.py`:

  Quy trình: «Kiểm tra chứng từ» (việc thường, hàng đợi «Kế toán», bắt buộc trường `amount_due`)
  → «Kế toán duyệt» (duyệt, cùng hàng đợi).

  1. test@ gửi báo giá ⇒ duyet1, duyet2 thấy ở view=pool (tên bước, tên hàng đợi, nhãn hồ sơ — không mã);
     test@ và hotro1 (khác vai) không thấy; hotro1 gọi thẳng claim ⇒ 404 WORK_ITEM_NOT_FOUND.
  2. duyet1 nhận ⇒ biến khỏi pool duyet2; duyet2 nhận ⇒ 409; duyet2 bấm Duyệt ở /approvals ⇒ 409
     WORK_ITEM_ALREADY_CLAIMED; duyet1 trả lại ⇒ về pool; duyet2 trả hộ ⇒ 403 (không phải người nhận).
  3. Hoàn thành khi trường trống ⇒ 422 WORK_ITEM_REQUIRED_FIELDS, details.fields = [amount_due], hồ sơ vẫn ở
     bước 1; bấm Duyệt trên việc ⇒ 422 APPROVAL_STEP_KIND_MISMATCH; test@ điền trường ⇒ hoàn thành ⇒ 200,
     hồ sơ sang bước duyệt, việc nằm ở view=done của duyet1.
  4. Bước duyệt cùng hàng đợi: duyet1, duyet2, duyet3 cùng thấy; duyet2 nhận; duyet1 và duyet3 duyệt ⇒ 409;
     duyet2 duyệt qua /approvals/:id/approve ⇒ 200, hồ sơ «approved».
  5. Hai người nhận CÙNG LÚC (8 lượt, mỗi lượt 2 luồng chờ nhau rồi bắn) ⇒ đúng một 200, luồng kia 409.
  6. counts của duyet1; DB: ghế đã nhận ghi claimed_by, ghế việc outcome=completed/kind=task.

    python3 scripts/fixture-bpm.py --base http://localhost:8702/api/v1
    python3 scripts/acceptance-bpm-work-items.py --base http://localhost:8702/api/v1 --db nexus_wt_bpm_work_items_claim

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: stdlib + `docker exec nexus-mysql mysql` (cùng khuôn acceptance-bpm-seat-authority.py, mà script này nạp lại).
"""
import argparse
import importlib.util
import os
import sys
import threading

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check, fails = seats_mod.Env, seats_mod.check, seats_mod.fails

PROC = "S5 nghiệm thu — kiểm tra chứng từ"
FIELD = "amount_due"


def items(env, tok, view):
    c, b = env.req("GET", f"/work-items?view={view}&limit=200", tok)
    assert c == 200, (view, c, b)
    return b["data"]["items"]


def item_of(env, tok, view, instance_id):
    return next((i for i in items(env, tok, view) if i["instanceId"] == instance_id), None)


def code(resp):
    return resp[1].get("code")


def submit(env, tok, name):
    return env.send(tok, name)


def run(env):
    t = env.tokens()
    procs = env.req("GET", "/admin/approval-processes", t["admin"])[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]]
    assert not rivals, f"another active manual crm_quotes process would take the submit: {rivals}"
    queues = env.req("GET", "/admin/work-queues", t["admin"])[1]["data"]["queues"]
    queue = next(q for q in queues if q["name"] == "ke_toan")
    graph = {"nodes": [
        {"id": "kiem-tra", "name": "Kiểm tra chứng từ", "kind": "task", "approverType": "queue", "approverId": queue["id"],
         "requiredFields": [FIELD]},
        {"id": "ke-toan", "name": "Kế toán duyệt", "kind": "approval", "approverType": "queue", "approverId": queue["id"]},
    ]}
    c, b = env.req("POST", "/admin/approval-processes", t["admin"], {
        "name": PROC, "entityName": "crm_quotes", "trigger": "manual", "isActive": True, "graph": graph})
    assert c == 201, (c, b)
    proc = b["data"]["process"]["id"]
    try:
        # 0 — config-time guards through the real API.
        bad = env.req("POST", "/admin/approval-processes", t["admin"], {
            "name": PROC + " lỗi", "entityName": "crm_quotes", "trigger": "manual", "isActive": False,
            "graph": {"nodes": [{"id": "a", "kind": "task", "approverType": "queue", "approverId": queue["id"], "requiredFields": ["khong_co"]}]}})
        check("trường bắt buộc không có trên đối tượng ⇒ 422 APPROVAL_REQUIRED_FIELD_UNKNOWN",
              bad[0] == 422 and code(bad) == "APPROVAL_REQUIRED_FIELD_UNKNOWN", f"{bad[0]} {bad[1]}")
        bad = env.req("POST", "/admin/approval-processes", t["admin"], {
            "name": PROC + " lỗi 2", "entityName": "crm_quotes", "trigger": "manual", "isActive": False,
            "graph": {"nodes": [{"id": "a", "kind": "task", "approverType": "queue", "approverId": "00000000-0000-0000-0000-000000000000"}]}})
        check("hàng đợi không tồn tại ⇒ 422 APPROVAL_ASSIGNEE_NOT_FOUND",
              bad[0] == 422 and code(bad) == "APPROVAL_ASSIGNEE_NOT_FOUND", f"{bad[0]} {bad[1]}")

        base1 = env.req("GET", "/work-items/counts", t["duyet1"])[1]["data"]

        # 1 — who sees the pool.
        q = env.send(t["staff"], "S5-hộp việc")
        rid = env.sql(f"SELECT record_id FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{q}'")[0][0]
        it1, it2 = item_of(env, t["duyet1"], "pool", q), item_of(env, t["duyet2"], "pool", q)
        check("duyet1, duyet2 thấy việc ở «Chờ nhận» với tên bước, tên hàng đợi, loại việc",
              bool(it1 and it2) and it1["nodeName"] == "Kiểm tra chứng từ" and it1["assigneeName"] == "Kế toán"
              and it1["kind"] == "task" and it1["requiredFields"] == [FIELD], f"{it1}")
        check("nhãn hồ sơ là tên báo giá, không phải mã", bool(it1) and it1["recordLabel"] == "S5-hộp việc", f"{it1 and it1['recordLabel']}")
        check("test@ (nhân viên) không thấy ở «Chờ nhận» lẫn «Của tôi»",
              item_of(env, t["staff"], "pool", q) is None and item_of(env, t["staff"], "mine", q) is None)
        check("hotro1 (vai tk_agent) không thấy", item_of(env, t["hotro1"], "pool", q) is None)
        wid = it1["id"]
        r = env.req("POST", f"/work-items/{wid}/claim", t["hotro1"])
        check("hotro1 gọi thẳng claim ⇒ 404 WORK_ITEM_NOT_FOUND (không lộ tồn tại)", r[0] == 404 and code(r) == "WORK_ITEM_NOT_FOUND", f"{r[0]} {r[1]}")
        r = env.req("POST", f"/work-items/{wid}/claim", t["staff"])
        check("test@ gọi thẳng claim ⇒ 404", r[0] == 404, f"{r[0]}")

        # 2 — claim, taken, release.
        r = env.req("POST", f"/work-items/{wid}/claim", t["duyet1"])
        check("duyet1 nhận ⇒ 200 và claimedByMe", r[0] == 200 and r[1]["data"]["workItem"]["claimedByMe"] is True, f"{r[0]} {r[1]}")
        check("việc biến khỏi «Chờ nhận» của duyet2, nằm ở «Của tôi» của duyet1",
              item_of(env, t["duyet2"], "pool", q) is None and item_of(env, t["duyet1"], "mine", q) is not None)
        r = env.req("POST", f"/work-items/{wid}/claim", t["duyet2"])
        check("duyet2 nhận sau ⇒ 409 WORK_ITEM_ALREADY_CLAIMED", r[0] == 409 and code(r) == "WORK_ITEM_ALREADY_CLAIMED", f"{r[0]} {r[1]}")
        r = env.req("POST", f"/approvals/{q}/approve", t["duyet2"], {"note": "tôi duyệt thay"})
        check("duyet2 bấm Duyệt ở /approvals trên việc đã bị nhận ⇒ 409 WORK_ITEM_ALREADY_CLAIMED",
              r[0] == 409 and code(r) == "WORK_ITEM_ALREADY_CLAIMED", f"{r[0]} {r[1]}")
        r = env.req("POST", f"/work-items/{wid}/release", t["duyet2"])
        check("duyet2 trả hộ ⇒ 403 WORK_ITEM_RELEASE_FORBIDDEN", r[0] == 403 and code(r) == "WORK_ITEM_RELEASE_FORBIDDEN", f"{r[0]} {r[1]}")
        r = env.req("POST", f"/work-items/{wid}/release", t["hotro1"])
        check("người ngoài trả ⇒ 404", r[0] == 404, f"{r[0]}")
        r = env.req("POST", f"/work-items/{wid}/release", t["duyet1"])
        check("duyet1 trả lại ⇒ 200, việc về «Chờ nhận» của duyet2",
              r[0] == 200 and item_of(env, t["duyet2"], "pool", q) is not None, f"{r[0]}")
        r = env.req("POST", f"/work-items/{wid}/release", t["duyet1"])
        check("trả lại việc chưa ai nhận ⇒ 422 WORK_ITEM_NOT_CLAIMED", r[0] == 422 and code(r) == "WORK_ITEM_NOT_CLAIMED", f"{r[0]} {r[1]}")
        r = env.req("POST", f"/work-items/{wid}/claim", t["duyet1"])
        check("duyet1 nhận lại", r[0] == 200, f"{r[0]}")

        # 3 — complete a task with a required field.
        r = env.req("POST", f"/work-items/{wid}/complete", t["duyet1"], {"note": "đủ"})
        fields = (r[1].get("details") or {}).get("fields")
        check("hoàn thành khi trường trống ⇒ 422 WORK_ITEM_REQUIRED_FIELDS, details.fields = [amount_due]",
              r[0] == 422 and code(r) == "WORK_ITEM_REQUIRED_FIELDS" and fields == [FIELD], f"{r[0]} {r[1]}")
        step = env.sql(f"SELECT current_step FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{q}'")[0][0]
        check("hồ sơ vẫn ở bước 1 sau lần hoàn thành bị từ chối", step == "0", step)
        r = env.req("POST", f"/approvals/{q}/approve", t["duyet1"], {"note": "duyệt"})
        check("bấm Duyệt trên một việc ⇒ 422 APPROVAL_STEP_KIND_MISMATCH", r[0] == 422 and code(r) == "APPROVAL_STEP_KIND_MISMATCH", f"{r[0]} {r[1]}")
        r = env.req("PUT", f"/crm_quotes/{rid}", t["staff"], {FIELD: {"amount": 1500000, "currencyCode": "VND"}})
        check("test@ (chủ hồ sơ) điền trường", r[0] == 200, f"{r[0]} {r[1]}")
        r = env.req("POST", f"/work-items/{wid}/complete", t["duyet1"], {"note": "đủ chứng từ"})
        check("điền đủ ⇒ hoàn thành 200, hồ sơ sang bước 2 còn chờ",
              r[0] == 200 and r[1]["data"]["approval"]["Status"] == "pending" and r[1]["data"]["approval"]["CurrentStep"] == 1, f"{r[0]} {r[1]}")
        done = item_of(env, t["duyet1"], "done", q)
        check("việc nằm ở «Đã xử lý» của duyet1", bool(done) and done["status"] == "done", f"{done}")
        r = env.req("POST", f"/work-items/{wid}/complete", t["duyet1"], {})
        check("hoàn thành lần hai ⇒ 409 WORK_ITEM_NOT_OPEN", r[0] == 409 and code(r) == "WORK_ITEM_NOT_OPEN", f"{r[0]} {r[1]}")
        st = env.sql(f"SELECT kind, status, outcome, claimed_by, decided_by FROM sys_bpm_tasks WHERE tenant_id = '{env.tenant}' AND id = '{wid}'")[0]
        users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", t["admin"])[1]["data"]["users"]}
        d1, d2, d3 = (users[f"duyet{i}@example.com"] for i in (1, 2, 3))
        check("DB: ghế việc kind=task, done, outcome=completed, do duyet1 nhận và hoàn thành",
              st[:3] == ["task", "done", "completed"] and st[3] == d1 and st[4] == d1, st)

        # 4 — the approval step on the same queue: pool, claim, the one decision path.
        it = [item_of(env, t[u], "pool", q) for u in ("duyet1", "duyet2", "duyet3")]
        check("bước duyệt cùng hàng đợi: duyet1, duyet2, duyet3 cùng thấy, loại approval",
              all(it) and all(i["kind"] == "approval" for i in it), f"{it}")
        wid2 = it[1]["id"]
        r = env.req("POST", f"/work-items/{wid2}/claim", t["duyet2"])
        check("duyet2 nhận bước duyệt", r[0] == 200, f"{r[0]} {r[1]}")
        for u in ("duyet1", "duyet3"):
            r = env.req("POST", f"/approvals/{q}/approve", t[u], {"note": "x"})
            check(f"{u} duyệt việc duyet2 đã nhận ⇒ 409", r[0] == 409 and code(r) == "WORK_ITEM_ALREADY_CLAIMED", f"{r[0]} {r[1]}")
        pend1 = env.pending(t["duyet1"])
        pend2 = env.pending(t["duyet2"])
        check("/approvals/pending: chỉ duyet2 còn thấy hồ sơ", q not in pend1 and q in pend2, f"{pend1} {pend2}")
        r = env.req("POST", f"/work-items/{wid2}/complete", t["duyet2"], {})
        check("«Hoàn thành» trên bước duyệt ⇒ 422 WORK_ITEM_NOT_COMPLETABLE", r[0] == 422 and code(r) == "WORK_ITEM_NOT_COMPLETABLE", f"{r[0]} {r[1]}")
        r = env.req("POST", f"/approvals/{q}/approve", t["duyet2"], {"note": "đồng ý"})
        check("duyet2 duyệt qua /approvals/:id/approve ⇒ 200 approved",
              r[0] == 200 and r[1]["data"]["approval"].get("status", r[1]["data"]["approval"].get("Status")) == "approved", f"{r[0]} {r[1]}")

        # 5 — a real race over HTTP.
        outcomes = []
        for n in range(8):
            q5 = env.send(t["staff"], f"S5-đua-{n}")
            w5 = item_of(env, t["duyet1"], "pool", q5)["id"]
            barrier, out = threading.Barrier(2), [None, None]

            def racer(i, who):
                barrier.wait()
                rr = env.req("POST", f"/work-items/{w5}/claim", t[who])
                out[i] = (rr[0], code(rr))

            ts = [threading.Thread(target=racer, args=(i, who)) for i, who in enumerate(("duyet1", "duyet2"))]
            [x.start() for x in ts]
            [x.join() for x in ts]
            winners = env.sql(f"SELECT claimed_by FROM sys_bpm_tasks WHERE tenant_id = '{env.tenant}' AND id = '{w5}'")[0][0]
            ok = sorted(c for c, _ in out) == [200, 409] and winners in (d1, d2)
            outcomes.append(out)
            check(f"đua nhận #{n}: đúng một 200 và một 409, ghế ghi đúng một người", ok, f"{out} {winners}")

        # 6 — the home-card numbers.
        counts = env.req("GET", "/work-items/counts", t["duyet1"])[1]["data"]
        mine_db = int(env.sql(
            f"SELECT COUNT(*) FROM sys_bpm_tasks s JOIN sys_approval_instances i ON i.id = s.instance_id AND i.tenant_id = s.tenant_id "
            f"AND i.status = 'pending' WHERE s.tenant_id = '{env.tenant}' AND s.status = 'open' AND "
            f"(s.claimed_by = '{d1}' OR (s.assignee_type = 'user' AND s.assignee_id = '{d1}' AND s.claimed_by IS NULL))")[0][0])
        check("counts của duyet1: mine khớp bảng ghế, pool và overdue là số", counts["mine"] == mine_db
              and isinstance(counts["pool"], int) and isinstance(counts["overdue"], int), f"{counts} db={mine_db} base={base1}")
        c, b = env.req("GET", "/work-items?view=bogus", t["duyet1"])
        check("view lạ ⇒ 422 WORK_ITEM_VIEW_INVALID", c == 422 and b.get("code") == "WORK_ITEM_VIEW_INVALID", f"{c} {b}")
        c, b = env.req("GET", "/work-items?cursor=%21%21", t["duyet1"])
        check("cursor hỏng ⇒ 400 WORK_ITEM_CURSOR_INVALID", c == 400 and b.get("code") == "WORK_ITEM_CURSOR_INVALID", f"{c} {b}")
        # paging: walk «Của tôi» of duyet1 in pages of 3 - no gap, no repeat.
        total = len(items(env, t["duyet1"], "mine"))
        seen, cursor = [], ""
        for _ in range(20):
            c, b = env.req("GET", f"/work-items?view=mine&limit=3&cursor={cursor}", t["duyet1"])
            seen += [i["id"] for i in b["data"]["items"]]
            cursor = b["data"]["nextCursor"] or ""
            if not cursor:
                break
        check("phân trang con trỏ duyệt hết «Của tôi» (trang 3, >1 trang), không lặp không sót", len(seen) == len(set(seen)) == total, f"{len(seen)} vs {total}")
    finally:
        env.req("DELETE", f"/admin/approval-processes/{proc}", t["admin"])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--db", required=True, help="tên DB clone của server (đọc bảng ghế)")
    a = ap.parse_args()
    run(Env(a.base, a.db))
    print(f"\n{len(fails)} kiểm tra hỏng" if fails else "\n✅ Nghiệm thu S5 đạt (non-admin).")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
