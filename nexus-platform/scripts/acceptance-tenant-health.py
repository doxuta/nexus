#!/usr/bin/env python3
"""'Sức khoẻ' (specs/066 L6) — third-person acceptance on a REAL server.

Story: the customer's own admin (is_admin, NOT a platform operator — proved by 403 on a builder route) reads
GET /admin/health while the employee test@example.com creates contacts that fire rules the script made.

Step 0: the DB clone carries the dev DB's failure history, so the script DELETES the tenant's failed rows of
the last 7 days (sys_jobs / sys_workflow_runs / sys_email_log / sys_hook_deliveries) and any queued job, and
REFUSES to run when --db is the shared nexus_platform. Then:
  * baseline status ok, Doctor unknown (checkedAt null);
  * a receiver answering 422 + test@example.com creating a contact ⇒ warn, jobs.failed7d has outbound_webhook x1
    (the rule is NOT expected in topFailing: with the queue on the run records "queued");
  * a contact with the email-rule's recipient field empty ⇒ the rule name is in workflowRuns.topFailing and
    emails.unresentFailed >= 1 (the resend → 0 path is pinned by TestFailedSummarySince_Integration; a real
    resend needs the SMTP rig of acceptance-email-log.py --resend);
  * the employee gets 403; an admin of ANOTHER tenant sees none of these; no key carries payload/raw errors;
  * STUCK (--stuck-base: a SECOND server on the same DB started with JOBS_POLL_INTERVAL_SEC=3600 — the main
    server's 1 s poller would pick the job up at once): wait 5 s, insert a queued job of the tenant with
    run_at = now - 20 min ⇒ critical ⇒ delete it ⇒ no longer critical.

Usage: python3 scripts/acceptance-tenant-health.py --base http://localhost:8447/api/v1 --db NAME
           --stuck-base http://localhost:8448/api/v1 [--other-tenant SLUG]
Exit: 0 = ĐẠT · 1 = CHƯA ĐẠT · 2 = lỗi môi trường.
"""
import argparse, http.server, json, re, subprocess, sys, threading, time, urllib.error, urllib.request, uuid

ap = argparse.ArgumentParser()
ap.add_argument("--base", default="http://localhost:8090/api/v1")
ap.add_argument("--db", required=True, help="DB clone the server runs on (NOT the shared nexus_platform)")
ap.add_argument("--skip-stuck", action="store_true", help="skip the stuck step (run it separately with --stuck-only)")
ap.add_argument("--stuck-only", action="store_true", help="run ONLY the stuck step, with the main server stopped (its 1 s poller would claim the job)")
ap.add_argument("--stuck-base", default="", help="second server on the same DB with JOBS_POLL_INTERVAL_SEC=3600")
ap.add_argument("--other-tenant", default="cong_ty_acme")
ap.add_argument("--receiver-port", type=int, default=9447)
ap.add_argument("--timeout", type=float, default=60)
args = ap.parse_args()
BASE = args.base
if not re.fullmatch(r"[A-Za-z0-9_]+", args.db):
    sys.exit(f"LỖI MÔI TRƯỜNG: tên DB không hợp lệ {args.db!r}")
if args.db == "nexus_platform":
    sys.exit("TỪ CHỐI: bước 0 xoá dòng lỗi — không chạy trên DB dev dùng chung nexus_platform")

ENTITY, FIELD = "crm_contacts", "email_ca_nhan"
fails = []


def check(label, cond, detail=""):
    print(("  PASS  " if cond else "  FAIL  ") + label + (f"   {detail}" if detail else ""))
    if not cond:
        fails.append(label)


def call(method, path, token=None, body=None, base=None):
    req = urllib.request.Request((base or BASE) + path, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data, timeout=30) as r:
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


def login(email, password, tenant="northwind", base=None):
    st, body = call("POST", "/auth/login", body={"tenant": tenant, "email": email, "password": password}, base=base)
    if st != 200:
        sys.exit(f"LỖI MÔI TRƯỜNG: đăng nhập {email}: {st} {body.get('code', '')}")
    return body["data"]["accessToken"]


def poll(label, fn, timeout=None):
    deadline = time.time() + (timeout or args.timeout)
    while time.time() < deadline:
        v = fn()
        if v:
            return v
        time.sleep(1)
    print(f"  (hết giờ chờ: {label})")
    return None


class Receiver(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.send_response(422)
        self.end_headers()

    def log_message(self, *a):
        pass


def health(token, base=None):
    st, body = call("GET", "/admin/health", token, base=base)
    return st, body.get("data") or {}, body


def stuck(tenant_id, cust_email, cust_pw):
    # The second server's worker sleeps an hour after the first empty claim: wait, then insert.
    cust2 = login(cust_email, cust_pw, base=args.stuck_base)
    time.sleep(5)
    job = str(uuid.uuid4())
    sql("INSERT INTO sys_jobs (id, tenant_id, job_type, status, attempts, max_attempts, run_at) "
        f"VALUES ('{job}', '{tenant_id}', 'outbound_webhook', 'queued', 0, 5, NOW(3) - INTERVAL 20 MINUTE)")
    try:
        st, h2, _ = health(cust2, args.stuck_base)
        check("việc ứ 20 phút ⇒ critical, jobs.stuck >= 1", st == 200 and h2.get("status") == "critical" and h2["jobs"]["stuck"] >= 1, json.dumps(h2.get("jobs"))[:200])
    finally:
        sql(f"DELETE FROM sys_jobs WHERE id = '{job}'")
    st, h2, _ = health(cust2, args.stuck_base)
    check("xoá việc ứ ⇒ không còn critical", st == 200 and h2.get("status") != "critical" and h2["jobs"]["stuck"] == 0, h2.get("status", ""))


def main():
    admin = login("admin@example.com", "Demo-Admin-2026!")
    rep = login("test@example.com", "Demo-Rep-2026!")
    tag = uuid.uuid4().hex[:8]
    tenant_id = sql("SELECT id FROM tenants WHERE slug = 'northwind'")[0][0]
    wf_ids, contacts, cust_id, other_id, field_id, stuck_id = [], [], None, None, None, None
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", args.receiver_port), Receiver)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        t = f"tenant_id = '{tenant_id}'"
        sql(f"DELETE FROM sys_jobs WHERE {t} AND (status = 'failed' OR status = 'queued') AND created_at >= NOW() - INTERVAL 8 DAY")
        sql(f"DELETE FROM sys_workflow_runs WHERE {t} AND status = 'failed' AND started_at >= NOW() - INTERVAL 8 DAY")
        sql(f"DELETE FROM sys_email_log WHERE {t} AND status = 'failed' AND created_at >= NOW() - INTERVAL 8 DAY")
        sql(f"DELETE FROM sys_hook_deliveries WHERE {t} AND status = 'error' AND created_at >= NOW() - INTERVAL 8 DAY")

        cust_email, cust_pw = f"quantri-khach-{tag}@example.com", "Demo-Customer-2026!-" + tag
        st, body = call("POST", "/admin/users", admin, {"email": cust_email, "password": cust_pw, "fullName": "Quản trị của khách", "isAdmin": True})
        assert st in (200, 201), f"create customer admin: {st} {body}"
        cust_id = ((body.get("data") or {}).get("user") or body.get("data") or {}).get("id")
        cust = login(cust_email, cust_pw)
        st, _ = call("GET", "/admin/api-keys", cust)
        check("nhân vật đúng: quản trị của khách KHÔNG vào được bề mặt builder (403)", st == 403, f"GET /admin/api-keys = {st}")

        if args.stuck_only:
            return stuck(tenant_id, cust_email, cust_pw)
        st, h, _ = health(cust)
        check("ban đầu: 200, status ok, Doctor unknown (checkedAt null), không việc ứ",
              st == 200 and h.get("status") == "ok" and h["doctor"]["status"] == "unknown" and h["doctor"]["checkedAt"] is None
              and h["jobs"]["stuck"] == 0, json.dumps(h)[:300])
        st, _, _ = health(rep)
        check("nhân viên test@example.com ⇒ 403", st == 403, str(st))

        # ── webhook out answered 422
        st, body = call("POST", "/admin/workflows", admin, {
            "name": f"zz nghiệm thu sức khoẻ webhook {tag}", "entityName": ENTITY, "trigger": "after_create",
            "actions": [{"type": "outbound_webhook", "url": f"http://127.0.0.1:{args.receiver_port}/422"}]})
        assert st in (200, 201), f"create webhook rule: {st} {body}"
        wf_ids.append((body.get("data") or {}).get("workflow", {}).get("id"))
        st, body = call("POST", f"/{ENTITY}", rep, {"first_name": "Lan", "last_name": f"Sức khoẻ {tag}"})
        assert st in (200, 201), f"rep create: {st} {body}"
        contacts.append((body.get("data") or body).get("id"))

        def webhook_failed():
            s, d, _ = health(cust)
            return d if s == 200 and any(j["jobType"] == "outbound_webhook" and j["count"] == 1 for j in d["jobs"]["failed7d"]) else None
        h = poll("webhook lỗi hiện ở jobs.failed7d", webhook_failed)
        check("webhook 422 ⇒ status warn và jobs.failed7d có outbound_webhook lỗi 1", bool(h) and h["status"] == "warn", json.dumps(h)[:300] if h else "")

        # ── email rule with an empty recipient field
        st, body = call("POST", "/admin/fields", admin, {"entityName": ENTITY, "name": FIELD, "label": "Email cá nhân",
                                                         "fieldType": "text", "options": {"sensitive": True}})
        if st == 409:
            sys.exit(f"HỦY: {ENTITY}.{FIELD} đã tồn tại — script không đụng field nó không tạo ra.")
        assert st in (200, 201), f"add field: {st} {body}"
        st, listed = call("GET", f"/metadata/fields/{ENTITY}", admin)
        field_id = next(f["id"] for f in listed.get("data", listed) if f["name"] == FIELD)
        rule_name = f"zz nghiệm thu sức khoẻ thư {tag}"
        st, body = call("POST", "/admin/workflows", admin, {
            "name": rule_name, "entityName": ENTITY, "trigger": "after_create",
            "actions": [{"type": "email_alert", "to": "{{" + FIELD + "}}", "subject": "Chào {{first_name}}", "body": "Xin chào"}]})
        assert st in (200, 201), f"create email rule: {st} {body}"
        wf_ids.append((body.get("data") or {}).get("workflow", {}).get("id"))
        st, body = call("POST", f"/{ENTITY}", rep, {"first_name": "Mai", "last_name": f"Sức khoẻ {tag}"})
        assert st in (200, 201), f"rep create: {st} {body}"
        contacts.append((body.get("data") or body).get("id"))

        def rule_failing():
            s, d, _ = health(cust)
            return d if s == 200 and any(r["name"] == rule_name for r in d["workflowRuns"]["topFailing"]) else None
        h = poll("tên luật email hiện ở topFailing", rule_failing)
        check("contact trống trường email ⇒ tên luật trong workflowRuns.topFailing", bool(h), json.dumps((h or {}).get("workflowRuns"), ensure_ascii=False)[:300])
        if h:
            check("emails.unresentFailed >= 1 và failed24h >= 1; status warn",
                  h["emails"]["unresentFailed"] >= 1 and h["emails"]["failed24h"] >= 1 and h["status"] == "warn", json.dumps(h["emails"]))
            sql_unresent = int(sql(f"SELECT COUNT(*) FROM sys_email_log WHERE {t} AND status='failed' AND source='workflow' AND resent_at IS NULL "
                                   "AND created_at >= NOW() - INTERVAL 7 DAY")[0][0])
            check("unresentFailed khớp câu SQL đếm tay", h["emails"]["unresentFailed"] == sql_unresent, f"api={h['emails']['unresentFailed']} sql={sql_unresent}")
            raw = json.dumps(h)
            check("không trường nào mang payload / payload_head", "payload" not in raw.lower())

        # ── tenant B sees none of it
        other_t = sql(f"SELECT id FROM tenants WHERE slug = '{args.other_tenant}'")
        if not other_t:
            check("có tenant thứ hai để kiểm cô lập", False, args.other_tenant)
        else:
            other_id = str(uuid.uuid4())
            other_email = f"zz-b-{tag}@isolation.test"
            sql("INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin) "
                f"SELECT '{other_id}', '{other_t[0][0]}', '{other_email}', password_hash, 'Quản trị B', 'active', 1 "
                f"FROM users WHERE email = 'admin@example.com' AND tenant_id = '{tenant_id}'")
            other = login(other_email, "Demo-Admin-2026!", args.other_tenant)
            st, hb, _ = health(other)
            check("admin tenant B: 200 và không thấy lỗi của tenant A",
                  st == 200 and not any(r["name"] == rule_name for r in hb["workflowRuns"]["topFailing"])
                  and all(j["jobType"] != "outbound_webhook" or j["count"] != 1 for j in hb["jobs"]["failed7d"]
                          ) and hb["emails"]["unresentFailed"] == int(sql(
                              f"SELECT COUNT(*) FROM sys_email_log WHERE tenant_id='{other_t[0][0]}' AND status='failed' AND source='workflow' "
                              "AND resent_at IS NULL AND created_at >= NOW() - INTERVAL 7 DAY")[0][0]), json.dumps(hb)[:200])

        if not args.skip_stuck:
            stuck(tenant_id, cust_email, cust_pw)
    finally:
        srv.shutdown()
        for w in wf_ids:
            if w:
                call("DELETE", f"/admin/workflows/{w}", admin)
        if wf_ids:
            sql("DELETE FROM sys_email_log WHERE workflow_id IN (" + ",".join(f"'{w}'" for w in wf_ids if w) + ")") if any(wf_ids) else None
        for rid in contacts:
            call("DELETE", f"/{ENTITY}/{rid}", admin)
        if stuck_id:
            sql(f"DELETE FROM sys_jobs WHERE id = '{stuck_id}'")
        if cust_id:
            call("DELETE", f"/admin/users/{cust_id}", admin)
        if other_id:
            sql(f"DELETE FROM users WHERE id = '{other_id}'")
        if field_id:
            call("DELETE", f"/admin/fields/{field_id}", admin)
            sql(f"DELETE FROM sys_fields WHERE id='{field_id}' AND deleted_at IS NOT NULL")


try:
    main()
except AssertionError as e:
    print(f"LỖI MÔI TRƯỜNG: {e}")
    sys.exit(2)
print("\nKẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)}): " + "; ".join(fails))
sys.exit(1 if fails else 0)
