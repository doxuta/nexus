#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S9a của specs/043 (`bpm-open-key-durable-arm`, DAILY-QUEUE #122, T145/T155).

Một bản ghi không bao giờ có hai hồ sơ duyệt mở cùng lúc, và sự kiện khởi động không rơi khi hệ thống bận.
Chạy trên server dựng từ DB CLONE đã migrate 000091 (không bao giờ DB dev dùng chung), sau `scripts/fixture-bpm.py`:

  1. Tám lượt «Gửi duyệt» CÙNG LÚC của test@example.com (không-admin) trên một báo giá ⇒ đúng MỘT 201, bảy lượt còn
     lại 422 APPROVAL_ALREADY_PENDING (không 500); CSDL đúng một hồ sơ pending cho bản ghi.
  2. Gửi lại khi đang chờ ⇒ 422 APPROVAL_ALREADY_PENDING; hai người duyệt xong ⇒ báo giá gửi lại được (201).
  3. Làn theo dõi: một hồ sơ làn `independent` chèn thẳng vào clone KHÔNG chặn việc gửi duyệt và KHÔNG hiện ở tab
     GET /crm_quotes/:id/approvals của test@.
  4. Job bền `bpm_arm`: quy trình `after_create` đang tắt ⇒ báo giá tạo ra không có hồ sơ; bật quy trình rồi chèn
     job `bpm_arm` (như khi buffer đầy) ⇒ hàng đợi khởi động đúng MỘT hồ sơ, duyet1 nhận việc; chèn job thứ hai
     cho cùng bản ghi ⇒ vẫn một hồ sơ, job `done`.

    python3 scripts/fixture-bpm.py --base http://localhost:8701/api/v1
    python3 scripts/acceptance-bpm-open-key.py --base http://localhost:8701/api/v1 --db nexus_wt_bpm_open_key_v91

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: stdlib + `docker exec nexus-mysql mysql` (cùng khuôn acceptance-bpm-seat-authority.py, mà script này nạp lại).
"""
import argparse
import importlib.util
import json
import os
import sys
import threading
import time
import uuid

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check = seats_mod.Env, seats_mod.check

SUBMIT_PROC = "S9a nghiệm thu — gửi tay"
ARM_PROC = "S9a nghiệm thu — tự khởi động"


def pending_count(env, rid):
    return int(env.sql(f"SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' "
                       f"AND record_id = '{rid}' AND status = 'pending'")[0][0])


def new_quote(env, tok, name):
    c, b = env.req("POST", "/crm_quotes", tok, {"name": name, "status": "Nháp"})
    assert c == 201, (c, b)
    return b["data"].get("id") or b["data"].get("record", {}).get("id")


def run(env):
    t = env.tokens()
    procs = env.req("GET", "/admin/approval-processes", t["admin"])[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["isActive"]]
    assert not rivals, f"another active crm_quotes process would take part: {rivals}"
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", t["admin"])[1]["data"]["users"]}
    d1, d2 = users["duyet1@example.com"], users["duyet2@example.com"]
    c, me = env.req("GET", "/auth/me", t["staff"])
    staff_id = me["data"].get("user", me["data"])["id"]
    steps = [{"id": "s1", "name": "Trưởng phòng", "approverType": "user", "approverId": d1},
             {"id": "s2", "name": "Kế toán", "approverType": "user", "approverId": d2}]
    created = []
    for name, trigger, active in ((SUBMIT_PROC, "manual", True), (ARM_PROC, "after_create", False)):
        c, b = env.req("POST", "/admin/approval-processes", t["admin"], {
            "name": name, "entityName": "crm_quotes", "trigger": trigger, "isActive": active, "steps": steps})
        assert c == 201, (c, b)
        created.append(b["data"]["process"]["id"])
    submit_proc, arm_proc = created
    try:
        # 1 — eight simultaneous submits of one quote.
        q1 = new_quote(env, t["staff"], "S9a-đua")
        barrier, out = threading.Barrier(8), [None] * 8

        def fire(i):
            barrier.wait()
            c, b = env.req("POST", f"/crm_quotes/{q1}/approvals/submit", t["staff"])
            out[i] = (c, b.get("code"))

        ths = [threading.Thread(target=fire, args=(i,)) for i in range(8)]
        for th in ths:
            th.start()
        for th in ths:
            th.join()
        won = [o for o in out if o[0] == 201]
        lost = [o for o in out if o == (422, "APPROVAL_ALREADY_PENDING")]
        check("8 lượt gửi cùng lúc: đúng một 201, bảy lượt còn lại 422 APPROVAL_ALREADY_PENDING (không 500)",
              len(won) == 1 and len(lost) == 7, str(out))
        check("CSDL: đúng một hồ sơ pending cho bản ghi", pending_count(env, q1) == 1, str(pending_count(env, q1)))

        # 2 — sequential re-submit, then free once approved.
        c, b = env.req("POST", f"/crm_quotes/{q1}/approvals/submit", t["staff"])
        check("gửi lại khi đang chờ ⇒ 422 APPROVAL_ALREADY_PENDING", c == 422 and b.get("code") == "APPROVAL_ALREADY_PENDING", f"{c} {b.get('code')}")
        iid = env.sql(f"SELECT id FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND record_id = '{q1}' AND status = 'pending'")[0][0]
        env.req("POST", f"/approvals/{iid}/approve", env.tokens_cache["duyet1"], {"note": "ok"})
        c, _ = env.req("POST", f"/approvals/{iid}/approve", env.tokens_cache["duyet2"], {"note": "ok"})
        check("hai người duyệt xong ⇒ hồ sơ approved, bản ghi hết bị giữ chỗ", c == 200 and pending_count(env, q1) == 0, str(c))
        c, b = env.req("POST", f"/crm_quotes/{q1}/approvals/submit", t["staff"])
        check("báo giá đã duyệt gửi lại được ⇒ 201 (hồ sơ mới)", c == 201 and pending_count(env, q1) == 1, f"{c} {b.get('code')}")

        # 3 — an independent-lane instance neither blocks nor shows in the approvals tab.
        q3 = new_quote(env, t["staff"], "S9a-làn theo dõi")
        env.sql(f"INSERT INTO sys_approval_instances (id, tenant_id, process_id, entity_name, record_id, current_step, status, history, lane) "
                f"VALUES ('{uuid.uuid4()}', '{env.tenant}', '{uuid.uuid4()}', 'crm_quotes', '{q3}', 0, 'pending', '[]', 'independent')")
        c, tab = env.req("GET", f"/crm_quotes/{q3}/approvals", t["staff"])
        listed = tab.get("data", {}).get("instances", None)
        check("tab Duyệt của test@ không thấy hồ sơ làn theo dõi", c == 200 and listed == [], f"{c} {tab}")
        c, b = env.req("POST", f"/crm_quotes/{q3}/approvals/submit", t["staff"])
        check("hồ sơ làn theo dõi KHÔNG chặn gửi duyệt ⇒ 201", c == 201, f"{c} {b.get('code')}")
        c, tab = env.req("GET", f"/crm_quotes/{q3}/approvals", t["staff"])
        listed = tab.get("data", {}).get("instances", [])
        check("sau khi gửi, tab chỉ hiện đúng hồ sơ duyệt (1 dòng), vẫn không có hồ sơ làn theo dõi", c == 200 and len(listed) == 1, f"{c} {len(listed)}")

        # 4 — the durable bpm_arm job arms a start event that met a full buffer.
        q4 = new_quote(env, t["staff"], "S9a-job bền")
        check("quy trình tự khởi động đang tắt ⇒ báo giá mới chưa có hồ sơ", pending_count(env, q4) == 0)
        c, _ = env.req("PUT", f"/admin/approval-processes/{arm_proc}", t["admin"], {
            "name": ARM_PROC, "entityName": "crm_quotes", "trigger": "after_create", "isActive": True, "steps": steps})
        assert c == 200, c
        payload = json.dumps({"actorUserId": staff_id, "entityName": "crm_quotes", "recordId": q4,
                              "trigger": "after_create", "data": {"name": "S9a-job bền", "status": "Nháp"}})
        job_ids = []
        for _ in range(2):  # the second one is the duplicate: the record is already waiting
            jid = str(uuid.uuid4())
            job_ids.append(jid)
            env.sql(f"INSERT INTO sys_jobs (id, tenant_id, job_type, payload, status, run_at, attempts, max_attempts) "
                    f"VALUES ('{jid}', '{env.tenant}', 'bpm_arm', '{payload}', 'queued', UTC_TIMESTAMP(3), 0, 3)")
            deadline = time.time() + 40
            while time.time() < deadline and env.sql(f"SELECT status FROM sys_jobs WHERE id = '{jid}'")[0][0] not in ("done", "failed"):
                time.sleep(1)
        st = [env.sql(f"SELECT status FROM sys_jobs WHERE id = '{j}'")[0][0] for j in job_ids]
        check("hai job bpm_arm cho cùng bản ghi: cả hai done (job thứ hai = «đã đang chờ», không lỗi, không retry)", st == ["done", "done"], str(st))
        check("job bền khởi động đúng MỘT hồ sơ pending cho báo giá", pending_count(env, q4) == 1, str(pending_count(env, q4)))
        who = env.sql(f"SELECT created_by, current_approver_id FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND record_id = '{q4}'")[0]
        check("hồ sơ do job tạo mang người gửi = test@ và đang chờ duyet1", who == [staff_id, d1], str(who))
        c, pend = env.req("GET", "/approvals/pending", env.tokens_cache["duyet1"])
        ids = [r.get("recordId") or r.get("RecordID") for r in pend["data"]["approvals"]]
        check("duyet1 (không-admin) thấy việc do job tạo trong hộp duyệt", c == 200 and q4 in ids, f"{c} {ids}")
    finally:
        for pid in created:
            c, _ = env.req("DELETE", f"/admin/approval-processes/{pid}", t["admin"])
            check("dọn: xoá quy trình nghiệm thu", c == 204, str(c))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--base", required=True, help="API base of the server running over the clone")
    ap.add_argument("--db", required=True, help="the CLONE database the server runs on")
    a = ap.parse_args()
    if a.db == "nexus_platform":
        sys.exit("refusing the shared dev DB — clone it first (nexus_wt_<slug>)")
    env = Env(a.base, a.db)
    tokens = env.tokens

    def cached():
        t = tokens()
        env.tokens_cache = t
        return t

    env.tokens = cached
    run(env)
    fails = seats_mod.fails
    print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)}): {fails}")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
