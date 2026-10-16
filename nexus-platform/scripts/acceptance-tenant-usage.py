#!/usr/bin/env python3
"""'Mức dùng' (specs/066 L4) — third-person acceptance on a REAL server.

Story: the NON-ADMIN employee test@example.com creates contacts, which fires two rules the script made
(one mails a fixed address, one calls a fake webhook receiver that answers 422). The customer's own admin
(is_admin, NOT a platform operator — proved by 403 on a builder route) reads GET /admin/usage.

Checks (numbers are compared with SQL the script runs itself, on --db):
  * today's row of the API == today's counts by hand SQL (runs, mail by status, webhook jobs by status,
    inbound hooks) AND, after the roller's tick, sys_usage_daily holds the same figures;
  * 365 days answers 365 rows, 366 answers 366, 367 is 400 USAGE_RANGE_INVALID, from > to is 400;
  * the employee gets 403, an admin of ANOTHER tenant sees none of these figures;
  * an old day: source rows are inserted for D-5 ⇒ restart the server ⇒ after the FIRST tick D-5 has the
    number; the source rows are deleted (what the janitor does) ⇒ restart ⇒ first tick ⇒ D-5 does NOT fall;
  * restart with USAGE_ROLLUP_INTERVAL_SEC=0 ⇒ rollupEnabled=false and today still has its numbers.

The server must be started with USAGE_ROLLUP_INTERVAL_SEC=60 (the roller's first tick is 60 s after boot) and
JOBS_ENABLED on. Restarts go through --restart-cmd, a shell command that stops the server and starts it again
with the env var USAGE_ROLLUP_INTERVAL_SEC set to the value the script exports, and returns once it is up.

Usage: python3 scripts/acceptance-tenant-usage.py --base http://localhost:8445/api/v1 --db NAME
           --restart-cmd "bash /path/restart-server.sh" [--other-tenant SLUG]
Exit: 0 = ĐẠT · 1 = CHƯA ĐẠT · 2 = lỗi môi trường.
"""
import argparse, datetime, http.server, json, os, re, subprocess, sys, threading, time, urllib.error, urllib.request, uuid

ap = argparse.ArgumentParser()
ap.add_argument("--base", default="http://localhost:8090/api/v1")
ap.add_argument("--db", default="nexus_platform", help="database the server runs on (rows are read there)")
ap.add_argument("--restart-cmd", required=True, help="shell command that restarts the server; reads env USAGE_ROLLUP_INTERVAL_SEC")
ap.add_argument("--other-tenant", default="cong_ty_acme", help="slug of a second tenant (isolation check)")
ap.add_argument("--receiver-port", type=int, default=9445)
ap.add_argument("--timeout", type=float, default=150, help="seconds to wait for a roller tick")
ap.add_argument("--gauges", action="store_true", help="L5: check the 'Hiện tại' block and usersSeen instead of the L4 counters")
args = ap.parse_args()
BASE = args.base
if not re.fullmatch(r"[A-Za-z0-9_]+", args.db):
    sys.exit(f"LỖI MÔI TRƯỜNG: tên DB không hợp lệ {args.db!r}")

ENTITY = "crm_contacts"
VN = datetime.timezone(datetime.timedelta(hours=7))  # APP_TIMEZONE default Asia/Ho_Chi_Minh, no DST
fails = []


def check(label, cond, detail=""):
    print(("  PASS  " if cond else "  FAIL  ") + label + (f"   {detail}" if detail else ""))
    if not cond:
        fails.append(label)


def call(method, path, token=None, body=None):
    req = urllib.request.Request(BASE + path, method=method)
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


def login(email, password, tenant="northwind"):
    st, body = call("POST", "/auth/login", body={"tenant": tenant, "email": email, "password": password})
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


def restart(interval):
    env = dict(os.environ, USAGE_ROLLUP_INTERVAL_SEC=str(interval))
    r = subprocess.run(args.restart_cmd, shell=True, env=env, capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit(f"LỖI MÔI TRƯỜNG: restart-cmd: {r.stderr.strip() or r.stdout.strip()}")
    ok = poll("máy chủ lên lại", lambda: call("GET", "/auth/me")[0] in (200, 401), 60)
    if not ok:
        sys.exit("LỖI MÔI TRƯỜNG: máy chủ không lên lại")


class Receiver(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.send_response(422)
        self.end_headers()

    def log_message(self, *a):
        pass


def day_bounds(d):
    """UTC 'YYYY-MM-DD HH:MM:SS' edges of the Vietnam calendar day d (a date)."""
    start = datetime.datetime(d.year, d.month, d.day, tzinfo=VN).astimezone(datetime.timezone.utc)
    end = start + datetime.timedelta(days=1)
    f = "%Y-%m-%d %H:%M:%S"
    return start.strftime(f), end.strftime(f)


def hand_counts(tenant_id, d):
    lo, hi = day_bounds(d)
    t = f"tenant_id = '{tenant_id}'"
    runs = sql(f"SELECT COUNT(*), IFNULL(SUM(status='failed'),0) FROM sys_workflow_runs WHERE {t} "
               f"AND started_at >= '{lo}' AND started_at < '{hi}' AND status <> 'skipped'")[0]
    mail = {r[0]: int(r[1]) for r in sql(f"SELECT status, COUNT(*) FROM sys_email_log WHERE {t} "
                                         f"AND created_at >= '{lo}' AND created_at < '{hi}' GROUP BY status")}
    jobs = {r[0]: int(r[1]) for r in sql(f"SELECT status, COUNT(*) FROM sys_jobs WHERE {t} AND job_type = 'outbound_webhook' "
                                         f"AND created_at >= '{lo}' AND created_at < '{hi}' GROUP BY status")}
    hooks = {r[0]: int(r[1]) for r in sql(f"SELECT status, COUNT(*) FROM sys_hook_deliveries WHERE {t} "
                                          f"AND created_at >= '{lo}' AND created_at < '{hi}' GROUP BY status")}
    return {
        "workflowRuns": int(runs[0]), "workflowRunsFailed": int(runs[1]),
        "emailsSent": mail.get("sent", 0), "emailsFailed": mail.get("failed", 0),
        "webhooksDelivered": jobs.get("done", 0), "webhooksFailed": jobs.get("failed", 0),
        "hooksReceived": hooks.get("created", 0) + hooks.get("updated", 0), "hooksFailed": hooks.get("error", 0),
    }


STORED = {"workflow_runs": "workflowRuns", "workflow_runs_failed": "workflowRunsFailed", "emails_sent": "emailsSent",
          "emails_failed": "emailsFailed", "webhooks_delivered": "webhooksDelivered", "webhooks_failed": "webhooksFailed",
          "hooks_received": "hooksReceived", "hooks_failed": "hooksFailed"}


def stored_counts(tenant_id, d):
    rows = sql(f"SELECT metric, SUM(value) FROM sys_usage_daily WHERE tenant_id = '{tenant_id}' AND day = '{d}' GROUP BY metric")
    out = {STORED[m]: int(v) for m, v in rows if m in STORED}
    return out


def usage(token, q=""):
    return call("GET", "/admin/usage" + q, token)


def main():
    restart(60)  # known start state: roller on, first tick 60 s after boot
    admin = login("admin@example.com", "Demo-Admin-2026!")
    rep = login("test@example.com", "Demo-Rep-2026!")
    tag = uuid.uuid4().hex[:8]
    tenant_id = sql("SELECT id FROM tenants WHERE slug = 'northwind'")[0][0]
    today = datetime.datetime.now(VN).date()
    old_day = today - datetime.timedelta(days=5)
    wf_ids, contacts, cust_id, other_id, old_rows = [], [], None, None, []
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", args.receiver_port), Receiver)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        for name, action in (
            ("thư", {"type": "email_alert", "to": "khach@example.com", "subject": "Chào {{first_name}}", "body": "Xin chào"}),
            ("webhook", {"type": "outbound_webhook", "url": f"http://127.0.0.1:{args.receiver_port}/422"}),
        ):
            st, body = call("POST", "/admin/workflows", admin, {
                "name": f"zz nghiệm thu mức dùng {name} {tag}", "entityName": ENTITY, "trigger": "after_create", "actions": [action]})
            assert st in (200, 201), f"create workflow {name}: {st} {body}"
            wf_ids.append((body.get("data") or {}).get("workflow", {}).get("id"))

        cust_email, cust_pw = f"quantri-khach-{tag}@example.com", "Demo-Customer-2026!-" + tag
        st, body = call("POST", "/admin/users", admin, {"email": cust_email, "password": cust_pw, "fullName": "Quản trị của khách", "isAdmin": True})
        assert st in (200, 201), f"create customer admin: {st} {body}"
        cust_id = ((body.get("data") or {}).get("user") or body.get("data") or {}).get("id")
        cust = login(cust_email, cust_pw)
        st, _ = call("GET", "/admin/api-keys", cust)
        check("nhân vật đúng: quản trị của khách KHÔNG vào được bề mặt builder (403)", st == 403, f"GET /admin/api-keys = {st}")

        for i in range(3):
            st, body = call("POST", f"/{ENTITY}", rep, {"first_name": f"Lan{i}", "last_name": f"Mức dùng {tag}"})
            assert st in (200, 201), f"rep create: {st} {body}"
            contacts.append((body.get("data") or body).get("id"))
        print(f"[setup] rules={wf_ids} contacts={contacts} (tạo bởi test@example.com)")

        def settled():
            c = hand_counts(tenant_id, today)
            pending = sql(f"SELECT COUNT(*) FROM sys_jobs WHERE tenant_id = '{tenant_id}' AND job_type = 'outbound_webhook' "
                          "AND status IN ('queued','running')")[0][0]
            return c if c["workflowRuns"] >= 6 and c["emailsSent"] + c["emailsFailed"] >= 3 and pending == "0" else None
        want = poll("luật chạy xong, mail có dòng, webhook hết hàng đợi", settled)
        check("setup: 6 lần chạy luật, 3 email, webhook ra đã có kết quả", want is not None)
        if want is None:
            return

        # ── staff and the admin
        st, _ = usage(rep)
        check("nhân viên test@example.com ⇒ 403", st == 403, f"{st}")
        st, body = usage(cust)
        data = body.get("data") or {}
        row = next((d for d in data.get("days") or [] if d.get("day") == str(today)), {})
        want = hand_counts(tenant_id, today)
        check("quản trị khách: 200, 30 ngày, hôm nay khớp câu SQL đếm tay (đếm trực tiếp)",
              st == 200 and len(data.get("days") or []) == 30 and all(row.get(k) == v for k, v in want.items()),
              f"api={ {k: row.get(k) for k in want} } sql={want}")
        check("hôm nay có đủ 3 webhook ra (bên nhận trả 422 ⇒ lỗi)", want["webhooksDelivered"] + want["webhooksFailed"] >= 3, str(want))

        # ── ranges
        for n, code in ((365, 200), (366, 200), (367, 400)):
            frm = (today - datetime.timedelta(days=n - 1)).isoformat()
            st, body = usage(cust, f"?from={frm}")
            ok = st == code and (code != 200 or len((body.get("data") or {}).get("days") or []) == n)
            check(f"khoảng {n} ngày ⇒ {code}" + (f" và {n} hàng" if code == 200 else " USAGE_RANGE_INVALID"), ok and (code == 200 or body.get("code") == "USAGE_RANGE_INVALID"), f"{st}")
        st, body = usage(cust, f"?from={today}&to={today - datetime.timedelta(days=1)}")
        check("from sau to ⇒ 400 USAGE_RANGE_INVALID", st == 400 and body.get("code") == "USAGE_RANGE_INVALID", f"{st}")

        # ── another tenant
        other_t = sql(f"SELECT id FROM tenants WHERE slug = '{args.other_tenant}'")
        if not other_t:
            check("có tenant thứ hai để kiểm cô lập", False, f"không thấy tenant {args.other_tenant}")
        else:
            other_id = str(uuid.uuid4())
            other_email = f"zz-b-{tag}@isolation.test"
            sql("INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin) "
                f"SELECT '{other_id}', '{other_t[0][0]}', '{other_email}', password_hash, 'Quản trị B', 'active', 1 "
                f"FROM users WHERE email = 'admin@example.com' AND tenant_id = '{tenant_id}'")
            other = login(other_email, "Demo-Admin-2026!", args.other_tenant)
            st, body = usage(other)
            other_today = next((d for d in (body.get("data") or {}).get("days") or [] if d.get("day") == str(today)), {})
            want_b = hand_counts(other_t[0][0], today)
            check("admin tenant B: 200 và chỉ thấy số của tenant B (khớp SQL của B)",
                  st == 200 and all(other_today.get(k) == v for k, v in want_b.items()),
                  f"B api={other_today.get('workflowRuns')} B sql={want_b['workflowRuns']} A={want['workflowRuns']}")

        # ── the roller wrote today's figures (first tick is 60 s after boot)
        got = poll("tick của UsageRoller ghi hôm nay", lambda: stored_counts(tenant_id, today).get("workflowRuns") == want["workflowRuns"] and stored_counts(tenant_id, today))
        want = hand_counts(tenant_id, today)
        check("sys_usage_daily hôm nay khớp câu SQL đếm tay", bool(got) and all(got.get(k) == v for k, v in want.items()), f"stored={got} sql={want}")

        # ── an old day survives the janitor: insert D-5 source rows, restart, first tick fills
        noon = datetime.datetime(old_day.year, old_day.month, old_day.day, 12, tzinfo=VN).astimezone(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S")
        for _ in range(3):
            rid = str(uuid.uuid4())
            old_rows.append(rid)
            sql("INSERT INTO sys_email_log (id, tenant_id, source, status, recipient_masked, created_at) "
                f"VALUES ('{rid}', '{tenant_id}', 'workflow', 'sent', 'a***@x.vn', '{noon}')")
        sql(f"DELETE FROM sys_usage_daily WHERE tenant_id = '{tenant_id}' AND day = '{old_day}'")
        restart(60)
        got = poll("tick đầu bù ngày cũ", lambda: stored_counts(tenant_id, old_day).get("emailsSent") == 3)
        check(f"khởi động lại ⇒ sau tick ĐẦU ngày {old_day} có 3 email đã gửi", bool(got))
        st, body = usage(cust, f"?from={old_day}&to={old_day}")
        check("API trả ngày cũ = 3", st == 200 and ((body.get("data") or {}).get("days") or [{}])[0].get("emailsSent") == 3, str(body)[:160])

        sql(f"DELETE FROM sys_email_log WHERE id IN ({','.join(repr(r) for r in old_rows)})")  # what the janitor does
        old_rows.clear()
        before = datetime.datetime.now(datetime.timezone.utc)
        restart(60)

        def ticked():  # the figures were written AFTER this restart: lastRollupAt moved past `before`
            at = (usage(cust)[1].get("data") or {}).get("lastRollupAt")
            return at if at and datetime.datetime.fromisoformat(at.replace("Z", "+00:00")) > before else None
        last = poll("tick đầu sau khi nguồn bị xoá", ticked)
        check(f"nguồn đã xoá + khởi động lại + tick đầu ⇒ ngày {old_day} KHÔNG giảm (vẫn 3)",
              last is not None and stored_counts(tenant_id, old_day).get("emailsSent") == 3, str(stored_counts(tenant_id, old_day)))

        # ── roller off
        restart(0)
        st, body = usage(cust)
        data = body.get("data") or {}
        row = next((d for d in data.get("days") or [] if d.get("day") == str(today)), {})
        want = hand_counts(tenant_id, today)
        check("USAGE_ROLLUP_INTERVAL_SEC=0 ⇒ rollupEnabled=false và hôm nay vẫn có số",
              st == 200 and data.get("rollupEnabled") is False and all(row.get(k) == v for k, v in want.items()), f"{st} {data.get('rollupEnabled')}")
    finally:
        srv.shutdown()
        for wf in wf_ids:
            if wf:
                call("DELETE", f"/admin/workflows/{wf}", admin)
        for rid in contacts:
            call("DELETE", f"/{ENTITY}/{rid}", admin)
        if old_rows:
            sql(f"DELETE FROM sys_email_log WHERE id IN ({','.join(repr(r) for r in old_rows)})")
        sql(f"DELETE FROM sys_usage_daily WHERE tenant_id = '{tenant_id}' AND day = '{old_day}'")
        if other_id:
            sql(f"DELETE FROM users WHERE id = '{other_id}'")
        if cust_id:
            sql(f"DELETE FROM users WHERE id = '{cust_id}'")


def multipart_file(token, rid, field, filename, content):
    boundary = "nexus" + uuid.uuid4().hex
    body = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{filename}"\r\n'
            "Content-Type: text/plain\r\n\r\n").encode() + content + f"\r\n--{boundary}--\r\n".encode()
    req = urllib.request.Request(BASE + f"/{ENTITY}/{rid}/files?field={field}", data=body, method="POST")
    req.add_header("Authorization", "Bearer " + token)
    req.add_header("Content-Type", f"multipart/form-data; boundary={boundary}")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


def main_gauges():
    """L5 (--gauges): the 'Hiện tại' block and the 'Người đăng nhập' column, against SQL the script runs itself."""
    restart(60)  # roller on, ticks every 60 s
    admin = login("admin@example.com", "Demo-Admin-2026!")
    rep = login("test@example.com", "Demo-Rep-2026!")
    tag = uuid.uuid4().hex[:8]
    tenant_id = sql("SELECT id FROM tenants WHERE slug = 'northwind'")[0][0]
    rep_id = sql(f"SELECT id FROM users WHERE tenant_id = '{tenant_id}' AND email = 'test@example.com'")[0][0]
    today = datetime.datetime.now(VN).date()
    field, cust_id, contact, field_id = f"zz_tep_{tag}", None, None, None

    def current(token):
        st, body = usage(token)
        return (body.get("data") or {}).get("current") or {}, body.get("data") or {}

    def sql_contacts():
        return int(sql(f"SELECT COUNT(*) FROM obj_{ENTITY} WHERE tenant_id = '{tenant_id}' AND deleted_at IS NULL")[0][0])

    def sql_files():
        r = sql(f"SELECT IFNULL(SUM(size),0), COUNT(*) FROM sys_files WHERE tenant_id = '{tenant_id}'")[0]
        return int(r[0]), int(r[1])

    def snapshot_is(pred):
        """Wait for a roller tick that makes pred(current) true (the block is read from the stored snapshot)."""
        def f():
            c, _ = current(cust)
            return c if c and pred(c) else None
        return poll("tick ghi chỉ số tức thời", f)

    def rows_of(c, entity=ENTITY):
        c = c or {}
        return next((r for r in c.get("records") or [] if r["entity"] == entity), {})

    try:
        cust_email, cust_pw = f"quantri-khach-{tag}@example.com", "Demo-Customer-2026!-" + tag
        st, body = call("POST", "/admin/users", admin, {"email": cust_email, "password": cust_pw, "fullName": "Quản trị của khách", "isAdmin": True})
        assert st in (200, 201), f"create customer admin: {st} {body}"
        cust_id = ((body.get("data") or {}).get("user") or body.get("data") or {}).get("id")
        cust = login(cust_email, cust_pw)
        st, _ = usage(rep)
        check("nhân viên test@example.com ⇒ 403", st == 403, f"{st}")

        st, body = call("POST", "/admin/fields", admin, {"entityName": ENTITY, "name": field, "label": "Tệp nghiệm thu", "fieldType": "file"})
        assert st in (200, 201), f"add file field: {st} {body}"
        st, listed = call("GET", f"/metadata/fields/{ENTITY}", admin)
        field_id = next(f["id"] for f in listed.get("data", listed) if f["name"] == field)

        # ── sign-ins: start from a day where test@example.com has not signed in, then sign in once
        sql(f"DELETE FROM refresh_tokens WHERE user_id = '{rep_id}'")
        before = int(next((d["usersSeen"] for d in current(cust)[1].get("days") or [] if d["day"] == str(today)), 0))
        login("test@example.com", "Demo-Rep-2026!")
        st, body = usage(cust)
        row = next((d for d in (body.get("data") or {}).get("days") or [] if d["day"] == str(today)), {})
        want_seen = int(sql(f"SELECT COUNT(DISTINCT user_id) FROM refresh_tokens WHERE tenant_id = '{tenant_id}' "
                            f"AND created_at >= '{day_bounds(today)[0]}' AND created_at < '{day_bounds(today)[1]}'")[0][0])
        check("test@example.com đăng nhập ⇒ usersSeen hôm nay +1 và khớp COUNT(DISTINCT) SQL",
              row.get("usersSeen") == before + 1 == want_seen, f"api={row.get('usersSeen')} trước={before} sql={want_seen}")

        # ── one contact with one file; first snapshot
        st, body = call("POST", f"/{ENTITY}", rep, {"first_name": "Lan", "last_name": f"Chỉ số {tag}"})
        assert st in (200, 201), f"create contact: {st} {body}"
        contact = (body.get("data") or body).get("id")
        content = b"x" * 5000
        check("test@example.com tải tệp 5000 byte lên bản ghi", multipart_file(rep, contact, field, "tep.txt", content) in (200, 201))
        n0, (b0, f0) = sql_contacts(), sql_files()
        c = snapshot_is(lambda c: rows_of(c).get("count") == n0 and c.get("fileBytes") == b0)
        want_active = int(sql(f"SELECT COUNT(*) FROM users WHERE tenant_id = '{tenant_id}' AND status = 'active' AND deleted_at IS NULL")[0][0])
        check("Hiện tại: số Liên hệ = COUNT(*) deleted_at IS NULL (nhãn nghiệp vụ, không phải tên bảng)",
              bool(c) and rows_of(c).get("count") == n0 and rows_of(c).get("label") not in (None, "", ENTITY), str(rows_of(c)))
        check("Hiện tại: usersActive = SQL; dung lượng và số tệp = SUM(size)/COUNT(*) trên sys_files",
              bool(c) and c.get("usersActive") == want_active and c.get("fileBytes") == b0 and c.get("fileCount") == f0,
              f"api={ {k: c.get(k) for k in ('usersActive', 'fileBytes', 'fileCount')} } sql=({want_active}, {b0}, {f0})")
        check("usersSeen30d ≥ 1 và recordsTotal = tổng các bảng", bool(c) and c.get("usersSeen30d", 0) >= 1
              and c.get("recordsTotal") == sum(r["count"] for r in c.get("records") or []))

        # ── bin the contact: the count drops, the bytes do not
        st, _ = call("DELETE", f"/{ENTITY}/{contact}", admin)
        check("đưa liên hệ vào thùng rác", st in (200, 204), f"{st}")
        n1 = sql_contacts()
        c = snapshot_is(lambda c: rows_of(c).get("count") == n1)
        check("vào thùng rác ⇒ số Liên hệ giảm 1 sau tick", n1 == n0 - 1 and bool(c) and rows_of(c).get("count") == n1, f"sql {n0}→{n1}")
        check("vào thùng rác ⇒ dung lượng KHÔNG giảm", bool(c) and c.get("fileBytes") == b0 and sql_files()[0] == b0, f"bytes={c.get('fileBytes')} trước={b0}")

        # ── hard delete: the bytes drop
        st, body = call("POST", f"/{ENTITY}/{contact}/purge", admin, {"dryRun": False})
        check("người vận hành xoá hẳn bản ghi", st == 200, f"{st} {body.get('code')}")
        contact = None
        b2, f2 = sql_files()
        c = snapshot_is(lambda c: c.get("fileBytes") == b2 and c.get("fileCount") == f2)
        check("xoá hẳn ⇒ dung lượng giảm đúng 5000 byte sau tick", bool(c) and b2 == b0 - 5000 and c.get("fileBytes") == b2, f"{b0}→{b2}")
    finally:
        if contact:
            call("DELETE", f"/{ENTITY}/{contact}", admin)
            call("POST", f"/{ENTITY}/{contact}/purge", admin, {"dryRun": False})
        if field_id:
            call("DELETE", f"/admin/fields/{field_id}", admin)
            if re.fullmatch(r"[0-9a-fA-F-]{36}", field_id):
                sql(f"DELETE FROM sys_fields WHERE id='{field_id}' AND deleted_at IS NOT NULL")
        if cust_id:
            sql(f"DELETE FROM users WHERE id = '{cust_id}'")


if __name__ == "__main__":
    if args.gauges:
        main_gauges()
    else:
        main()
    print()
    if fails:
        print(f"CHƯA ĐẠT — {len(fails)} mục: " + "; ".join(fails))
        sys.exit(1)
    print("ĐẠT")
