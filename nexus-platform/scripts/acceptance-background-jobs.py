#!/usr/bin/env python3
"""'Việc nền' (DAILY-QUEUE #31/#32, specs/021-durable-job-queue §3) — third-person acceptance on a
REAL server in the default durable-queue mode.

The persona is the CUSTOMER'S OWN tenant-admin: is_admin, NOT a platform operator (specs/034 puts
that person outside the builder tier, so a route mounted there would answer them 403). The script
creates such an account through the admin API and proves the persona first (403 on a builder route).

Story: an admin rule POSTs every new contact to the customer's system (a receiver this script runs
on --receiver-port). The receiver rejects it (422). The NON-ADMIN employee test@example.com creates a
contact, so a real person's action fires the rule; the delivery dead-letters on its first attempt.
  #31  the customer admin lists 'Việc nền' and sees that failed delivery with the receiver's error
       verbatim — and never another tenant's job nor a system (tenant-NULL) job, both seeded here;
       the employee gets 403.
  #32  the receiver is fixed (200); the customer admin presses "Gửi lại": the SAME row goes back to
       the queue and the receiver gets the body again under the SAME X-Nexus-Delivery-Id the failed
       attempt carried. A second press on the now-delivered job is refused (409); the other tenant's
       and the system job's ids are 404 to this admin; the employee gets 403; the audit trail names
       who pressed. #256: the DELIVERED job (the receiver answered 200 and lost the order) can be
       sent again - the receiver gets one more POST with the SAME delivery id and body, still one
       sys_jobs row; a delivered workflow_timer is refused (409) and another tenant's delivered job
       is 404. A failed workflow_timer of the admin's OWN tenant is refused (409) and stays
       failed — resending a timer can run its rule twice (review of #32).

Start the server with JOBS_POLL_INTERVAL_SEC=1 for a verdict in seconds. sys_jobs noise rows are
written straight into --db (test-owned ids) and removed in finally, like everything else the run
creates.

Usage: python3 scripts/acceptance-background-jobs.py --base URL --db NAME [--receiver-port 8198]
Exit: 0 = ĐẠT · 1 = CHƯA ĐẠT · 2 = lỗi môi trường.
"""
import argparse, http.server, json, re, subprocess, sys, threading, time, urllib.error, urllib.request, uuid

ap = argparse.ArgumentParser()
ap.add_argument("--base", default="http://localhost:8090/api/v1")
ap.add_argument("--db", default="nexus_platform", help="database the server runs on (noise rows are seeded there)")
ap.add_argument("--receiver-port", type=int, default=8198)
ap.add_argument("--timeout", type=float, default=60)
ap.add_argument("--only-list", action="store_true", help="#31 only (skip the resend checks of #32)")
args = ap.parse_args()
BASE = args.base
if not re.fullmatch(r"[A-Za-z0-9_]+", args.db):
    sys.exit(f"LỖI MÔI TRƯỜNG: tên DB không hợp lệ {args.db!r}")
ENTITY = "crm_contacts"

# ---- the customer's receiver -------------------------------------------------------------------
receiver = {"status": 422, "hits": []}


class Receiver(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        receiver["hits"].append({"status": receiver["status"], "delivery": self.headers.get("X-Nexus-Delivery-Id"),
                                 "body": body})
        self.send_response(receiver["status"])
        self.end_headers()

    def log_message(self, *_):
        pass


srv = http.server.ThreadingHTTPServer(("127.0.0.1", args.receiver_port), Receiver)
threading.Thread(target=srv.serve_forever, daemon=True).start()


def call(method, path, token=None, body=None):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data, timeout=15) as r:
            payload = r.read()
            return r.status, (json.loads(payload) if payload else {})
    except urllib.error.HTTPError as e:
        payload = e.read()
        return e.code, (json.loads(payload) if payload else {})


def sql(query):
    out = subprocess.run(["docker", "exec", "nexus-mysql", "mysql", "-uroot", "--default-character-set=utf8mb4",
                          "-N", "-B", args.db, "-e", query], capture_output=True, text=True)
    if out.returncode != 0:
        sys.exit(f"LỖI MÔI TRƯỜNG: mysql: {out.stderr.strip()}")
    return [line.split("\t") for line in out.stdout.splitlines()]


def login(email, password):
    st, body = call("POST", "/auth/login", body={"tenant": "northwind", "email": email, "password": password})
    if st != 200:
        sys.exit(f"LỖI MÔI TRƯỜNG: đăng nhập {email}: {st} {body.get('code', '')}")
    return body["data"]["accessToken"]


fails = []


def check(label, cond, detail=""):
    print(("  PASS  " if cond else "  FAIL  ") + label + (f"   {detail}" if detail else ""))
    if not cond:
        fails.append(label)


def list_jobs(token, query=""):
    return call("GET", "/admin/jobs" + query, token)


def wait_job(token, wf_id, status):
    deadline = time.time() + args.timeout
    while time.time() < deadline:
        st, body = list_jobs(token, "?type=outbound_webhook&limit=200")
        for row in (body.get("data") or []) if st == 200 else []:
            if row.get("workflowId") == wf_id and row.get("status") == status:
                return row
        time.sleep(0.5)
    return None


wf_id = record_id = user_id = None
noise = []
admin = login("admin@example.com", "Demo-Admin-2026!")
rep = login("test@example.com", "Demo-Rep-2026!")
tag = uuid.uuid4().hex[:8]
try:
    # The persona: a tenant-admin of northwind who is not a platform operator.
    cust_email, cust_pw = f"quantri-khach-{tag}@example.com", "Demo-Customer-2026!-" + tag
    st, body = call("POST", "/admin/users", admin, {"email": cust_email, "password": cust_pw,
                                                     "fullName": "Quản trị của khách", "isAdmin": True})
    assert st in (200, 201), f"create customer admin: {st} {body}"
    user_id = ((body.get("data") or {}).get("user") or body.get("data") or {}).get("id")
    cust = login(cust_email, cust_pw)
    st, _ = call("GET", "/admin/api-keys", cust)
    check("nhân vật đúng: quản trị của khách KHÔNG vào được bề mặt builder (403)", st == 403, f"GET /admin/api-keys = {st}")

    st, body = call("POST", "/admin/workflows", admin, {
        "name": f"zz nghiệm thu việc nền {tag}", "entityName": ENTITY, "trigger": "after_create",
        "actions": [{"type": "outbound_webhook", "url": f"http://127.0.0.1:{args.receiver_port}/don-hang"}],
    })
    assert st in (200, 201), f"create workflow: {st} {body}"
    wf_id = (body.get("data") or {}).get("workflow", {}).get("id")
    assert wf_id, f"workflow id missing: {body}"

    # Noise that must stay invisible: a job of another tenant, and a system (tenant-NULL) job —
    # both newer than anything this run creates, so they would top an unpinned page.
    other_tenant = sql("SELECT id FROM tenants WHERE slug <> 'northwind' ORDER BY slug LIMIT 1")[0][0]
    for tenant_sql in (f"'{other_tenant}'", "NULL"):
        nid = str(uuid.uuid4())
        sql("INSERT INTO sys_jobs (id, tenant_id, job_type, payload, status, run_at, attempts, max_attempts, "
            f"last_error, finished_at, created_at) VALUES ('{nid}', {tenant_sql}, 'outbound_webhook', "
            f"JSON_OBJECT('workflowId', 'wf-noise-{tag}', 'actionIndex', 0, 'body', '{{}}'), 'failed', NOW(3), 1, 10, "
            "'status 500', NOW(3), NOW(3) + INTERVAL 1 HOUR)")
        noise.append(nid)

    st, body = call("POST", f"/{ENTITY}", rep, {"first_name": "Khách", "last_name": f"Việc Nền {tag}"})
    assert st in (200, 201), f"rep create: {st} {body}"
    record_id = (body.get("data") or body).get("id")
    print(f"[setup] workflow={wf_id} record={record_id} (tạo bởi test@example.com) quản trị khách={cust_email}")

    failed = wait_job(cust, wf_id, "failed")
    check("#31 quản trị của khách thấy lần giao webhook LỖI của tổ chức mình", failed is not None,
          "" if failed else f"không thấy sau {args.timeout:.0f}s")
    if failed:
        print(f"         {json.dumps(failed, ensure_ascii=False)}")
        check("#31 lỗi bên nhận hiện nguyên văn (status 422)", "status 422" in (failed.get("lastError") or ""))
        check("#31 dòng không mang payload (dữ liệu bản ghi)", "payload" not in failed and "body" not in failed)
        check("#31 attempts=1/10 (4xx không thử lại)", failed.get("attempts") == 1 and failed.get("maxAttempts") == 10)

    st, body = list_jobs(cust, "?limit=200")
    ids = [r["id"] for r in body.get("data") or []]
    check("#31 GET /admin/jobs = 200 cho quản trị của khách", st == 200, f"status={st}")
    check("#31 KHÔNG thấy job của tổ chức khác", noise[0] not in ids)
    check("#31 KHÔNG thấy job hệ thống (tenant NULL)", noise[1] not in ids)
    st, body = list_jobs(cust, "?type=outbound_webhook&status=failed")
    check("#31 lọc loại+trạng thái giữ lại dòng lỗi", failed is not None and failed["id"] in [r["id"] for r in body.get("data") or []])
    st, body = list_jobs(cust, "?status=done&type=outbound_webhook&limit=200")
    check("#31 lọc done không lẫn dòng lỗi", failed is not None and failed["id"] not in [r["id"] for r in body.get("data") or []])
    st, first = list_jobs(cust, "?limit=1")
    nxt = (first.get("pagination") or {}).get("nextCursor")
    if nxt:
        st2, second = list_jobs(cust, "?limit=1&after=" + urllib.request.quote(nxt))
        check("#31 trang 2 theo cursor khác trang 1", st2 == 200 and second["data"] and second["data"][0]["id"] != first["data"][0]["id"])
    st, body = list_jobs(cust, "?status=chay-roi")
    check("#31 trạng thái lạ ⇒ 400 JOB_FILTER_INVALID", st == 400 and body.get("code") == "JOB_FILTER_INVALID", f"{st} {body.get('code')}")
    st, _ = list_jobs(rep)
    check("#31 nhân viên test@example.com (không admin) ⇒ 403", st == 403, f"status={st}")

    if failed and not args.only_list:
        first_delivery = receiver["hits"][0]["delivery"] if receiver["hits"] else None
        receiver["status"] = 200
        st, _ = call("POST", f"/admin/jobs/{failed['id']}/retry", rep)
        check("#32 nhân viên test@example.com bấm gửi lại ⇒ 403", st == 403, f"status={st}")
        st, body = call("POST", f"/admin/jobs/{failed['id']}/retry", cust)
        job = (body.get("data") or {}).get("job") or {}
        check("#32 quản trị của khách bấm Gửi lại ⇒ 200, chính dòng đó về hàng đợi",
              st == 200 and job.get("id") == failed["id"] and job.get("status") == "queued" and job.get("attempts") == 0,
              f"{st} {json.dumps(body, ensure_ascii=False)}")
        done = wait_job(cust, wf_id, "done")
        check("#32 lần giao lại thành công (done)", done is not None and done["id"] == failed["id"])
        ok_hits = [h for h in receiver["hits"] if h["status"] == 200]
        check("#32 bên nhận nhận ĐÚNG MỘT lần giao lại", len(ok_hits) == 1, f"{len(ok_hits)} lần 200")
        if ok_hits:
            check("#32 cùng X-Nexus-Delivery-Id với lần lỗi (= id job) ⇒ bên nhận khử trùng được",
                  ok_hits[0]["delivery"] == first_delivery == failed["id"],
                  f"lỗi={first_delivery} lại={ok_hits[0]['delivery']} job={failed['id']}")
            check("#32 cùng body với lần lỗi", ok_hits[0]["body"] == receiver["hits"][0]["body"])
        # #256: the delivered job (the receiver answered 200 and then "lost" the order) can be sent again.
        st, _ = call("POST", f"/admin/jobs/{failed['id']}/retry", rep)
        check("#256 nhân viên test@example.com bấm gửi lại bản đã giao ⇒ 403", st == 403, f"status={st}")
        st, body = call("POST", f"/admin/jobs/{failed['id']}/retry", cust)
        job = (body.get("data") or {}).get("job") or {}
        check("#256 quản trị của khách gửi lại lần giao ĐÃ XONG ⇒ 200, chính dòng đó về hàng đợi",
              st == 200 and job.get("id") == failed["id"] and job.get("status") == "queued" and job.get("attempts") == 0,
              f"{st} {json.dumps(body, ensure_ascii=False)}")
        deadline = time.time() + args.timeout
        while time.time() < deadline and len([h for h in receiver["hits"] if h["status"] == 200]) < 2:
            time.sleep(0.5)
        ok_hits = [h for h in receiver["hits"] if h["status"] == 200]
        check("#256 bên nhận nhận thêm ĐÚNG MỘT lần giao nữa (tổng 2 lần 200)", len(ok_hits) == 2, f"{len(ok_hits)} lần 200")
        if len(ok_hits) == 2:
            check("#256 cùng X-Nexus-Delivery-Id + cùng body với lần giao đầu ⇒ bên nhận khử trùng được",
                  ok_hits[1]["delivery"] == ok_hits[0]["delivery"] == failed["id"] and ok_hits[1]["body"] == ok_hits[0]["body"])
        again = wait_job(cust, wf_id, "done")
        check("#256 sau lần gửi lại dòng đó lại về done, vẫn MỘT dòng (không bản sao)",
              again is not None and again["id"] == failed["id"] and
              sql(f"SELECT COUNT(*) FROM sys_jobs WHERE JSON_UNQUOTE(JSON_EXTRACT(payload,'$.workflowId')) = '{wf_id}'")[0][0] == "1")
        for label, nid in (("tổ chức khác", noise[0]), ("hệ thống (tenant NULL)", noise[1])):
            st, body = call("POST", f"/admin/jobs/{nid}/retry", cust)
            check(f"#32 gửi lại job {label} theo id ⇒ 404 JOB_NOT_FOUND", st == 404 and body.get("code") == "JOB_NOT_FOUND", f"{st} {body.get('code')}")
            state = sql(f"SELECT status FROM sys_jobs WHERE id = '{nid}'")[0][0]
            check(f"#32 job {label} vẫn nguyên failed", state == "failed", f"status={state}")
        # Only a webhook is resendable: a failed timer of THIS tenant is refused and left untouched.
        own_tenant = sql("SELECT id FROM tenants WHERE slug = 'northwind'")[0][0]
        tid = str(uuid.uuid4())
        sql("INSERT INTO sys_jobs (id, tenant_id, job_type, payload, status, run_at, attempts, max_attempts, "
            f"last_error, finished_at) VALUES ('{tid}', '{own_tenant}', 'workflow_timer', "
            f"JSON_OBJECT('workflowId', 'wf-timer-{tag}', 'entityName', '{ENTITY}', 'recordId', 'r-{tag}', "
            "'dateValue', '2026-12-01'), 'failed', NOW(3), 5, 5, 'dynrepo: connection refused', NOW(3))")
        noise.append(tid)
        st, body = call("POST", f"/admin/jobs/{tid}/retry", cust)
        check("#32 gửi lại workflow_timer lỗi của chính tổ chức ⇒ 409 JOB_NOT_RETRYABLE (có thể chạy luật lần hai)",
              st == 409 and body.get("code") == "JOB_NOT_RETRYABLE", f"{st} {body.get('code')}")
        state = sql(f"SELECT status FROM sys_jobs WHERE id = '{tid}'")[0][0]
        check("#32 workflow_timer đó vẫn nguyên failed", state == "failed", f"status={state}")
        # #256: a DELIVERED timer would run every action of its rule again - refused like a failed one.
        did = str(uuid.uuid4())
        sql("INSERT INTO sys_jobs (id, tenant_id, job_type, payload, status, run_at, attempts, max_attempts, "
            f"finished_at) VALUES ('{did}', '{own_tenant}', 'workflow_timer', "
            f"JSON_OBJECT('workflowId', 'wf-timer-{tag}', 'entityName', '{ENTITY}', 'recordId', 'r2-{tag}', "
            "'dateValue', '2026-12-02'), 'done', NOW(3), 1, 5, NOW(3))")
        noise.append(did)
        st, body = call("POST", f"/admin/jobs/{did}/retry", cust)
        check("#256 gửi lại workflow_timer ĐÃ XONG ⇒ 409 JOB_NOT_RETRYABLE (bắn lại mọi action của luật)",
              st == 409 and body.get("code") == "JOB_NOT_RETRYABLE", f"{st} {body.get('code')}")
        state = sql(f"SELECT status FROM sys_jobs WHERE id = '{did}'")[0][0]
        check("#256 workflow_timer done đó vẫn nguyên done", state == "done", f"status={state}")
        # A delivered job of ANOTHER tenant is 404 and stays done.
        oid = str(uuid.uuid4())
        sql("INSERT INTO sys_jobs (id, tenant_id, job_type, payload, status, run_at, attempts, max_attempts, "
            f"finished_at) VALUES ('{oid}', '{other_tenant}', 'outbound_webhook', "
            f"JSON_OBJECT('workflowId', 'wf-noise-{tag}', 'actionIndex', 0, 'body', '{{}}'), 'done', NOW(3), 1, 10, NOW(3))")
        noise.append(oid)
        st, body = call("POST", f"/admin/jobs/{oid}/retry", cust)
        check("#256 gửi lại job ĐÃ XONG của tổ chức khác ⇒ 404 JOB_NOT_FOUND", st == 404 and body.get("code") == "JOB_NOT_FOUND", f"{st} {body.get('code')}")
        check("#256 job đã xong của tổ chức khác vẫn done", sql(f"SELECT status FROM sys_jobs WHERE id = '{oid}'")[0][0] == "done")
        st, body = call("GET", "/audit-log?entityName=__job__&limit=20", admin)
        mine = [r for r in body.get("data") or [] if r.get("recordId") == failed["id"]]
        check("#32/#256 audit ghi AI bấm gửi lại, MỖI lần một dòng (actor = quản trị của khách)",
              len(mine) == 2 and all(r.get("actorUserId") == user_id for r in mine),
              json.dumps(mine, ensure_ascii=False)[:300])
finally:
    if wf_id:
        call("DELETE", f"/admin/workflows/{wf_id}", admin)
    if record_id:
        call("DELETE", f"/{ENTITY}/{record_id}", admin)
    if user_id:
        call("DELETE", f"/admin/users/{user_id}", admin)
    for nid in noise:
        sql(f"DELETE FROM sys_jobs WHERE id = '{nid}'")
    srv.shutdown()

print()
print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)} điểm)")
sys.exit(1 if fails else 0)
