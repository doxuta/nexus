#!/usr/bin/env python3
"""'Nhật ký email' (specs/066 L1a + L1b) — third-person acceptance on a REAL server.

Story: an admin adds a sensitive text field email_ca_nhan to crm_contacts and two rules that mail
{{email_ca_nhan}} after create (one with a normal subject template, one with a 400-character one).
The NON-ADMIN employee test@example.com creates a contact, so a real person's action fires both rules.

  L1a (--slice write, checked by SQL on --db): exactly one sys_email_log row per rule, tenant-pinned,
       recipient masked ("a***@gmail.com"), the subject TEMPLATE (not the rendered text, 400 chars cut
       to 255), workflow_id set, a terminal status; with the queue on: job_id set and the finished job's
       payload NULL; no row anywhere holds the address or the rendered subject. A hard-deleted record
       (bin, then purge as an operator) takes its rows with it.
  L1b (default = write + read): the customer's own admin — is_admin, NOT a platform operator (the script
       creates one and proves the persona by 403 on a builder route) — reads the rows through
       GET /admin/email-log: masked recipient, template, rule name, table, status; the status filter, the
       cursor page 2, a /admin/jobs cursor refused (400 INVALID_CURSOR), a bad status refused (400); the
       employee gets 403; an admin of ANOTHER tenant sees none of these rows.

Server modes: the default (JOBS_ENABLED on, start with JOBS_POLL_INTERVAL_SEC=1) or --queue off for a
server started with JOBS_ENABLED=false (no job_id / payload checks; rows are written final by the
direct sender). Without SMTP the mail ends 'failed' with the masked reason (ErrMailNotConfigured);
pass --expect-status sent|failed to pin the terminal state (sent needs a real/fake SMTP, see
scripts/acceptance-email-job-privacy.py).

Everything the run creates is removed in finally (rows, rules, field, users, record).

  L2 (--resend, specs/066 "Gửi lại"): needs a server that REALLY talks SMTP+STARTTLS to a fake-smtp this
       script starts itself (--smtp-cert/--smtp-key; same Linux-container recipe as
       scripts/acceptance-email-job-privacy.py, server env SMTP_HOST=host.docker.internal SMTP_PORT=<--smtp-port>,
       JOBS_MAX_ATTEMPTS=1, JOBS_POLL_INTERVAL_SEC=1). The fake SMTP starts in REJECT mode, the employee
       test@example.com creates three contacts ⇒ three failed rows; the customer admin (non-operator) sees
       canResend; the SMTP is restarted in ACCEPT mode; the PREVIEW sends nothing (0 mails at the SMTP) and
       writes nothing; the real send puts exactly ONE mail on the SMTP, the new row is sent with resend_of =
       the old row and the actor, the old row says resentAt; a second press is 409 EMAIL_NOT_RESENDABLE and
       the SMTP still has 1 mail; editing the rule's subject ⇒ the next preview has ruleChangedSinceSend and
       the NEW subject; a contact moved to the bin ⇒ 409 EMAIL_RESEND_RECORD_GONE; the employee gets 403;
       an admin of ANOTHER tenant gets a 404 byte-identical to an unknown id; the audit trail has one
       __email__ row naming who pressed it; with the queue on the new job's payload is NULL (#95); with
       --queue off (server JOBS_ENABLED=false) the answer carries the pre-minted entry and resend_of is set.
       --no-mail (server ALERTS_ENABLED=false) only checks the 503 MAIL_NOT_CONFIGURED refusal.

  L3 (--direct, specs/066 "thư xác thực và email lỗi trước khi gửi"): same server recipe as --resend (a Linux
       container that dials the fake SMTP the script starts, in ACCEPT mode; the server also needs
       RESET_URL_BASE and ALERTS_ENABLED=true). The forgot-password mail for test@example.com is ONE row
       source=password_reset with recipient t***@example.com and the fixed subject, no actor; an UNKNOWN email and
       an unknown tenant get the same status + byte-identical body as the known one and write NO row and send
       NO mail (no probe channel); the customer's own admin (non-operator) issuing a link for test@example.com is ONE
       row source=reset_link with the issuer's id and name (createdByName); no row holds the address or the raw
       token; ?source= filters, a bad source is 400 EMAIL_LOG_FILTER_INVALID; auth rows are never resendable.
       A contact created WITHOUT the recipient field fires the rule but cannot be mailed: a failed row with no
       recipient, "không có địa chỉ nhận" and zero attempts, canResend, preview 422 EMAIL_RESEND_NO_RECIPIENT
       until an admin fills the email, then Gửi lại sends ONE mail (resend_of = the skipped row); the employee
       gets 403.

Usage: python3 scripts/acceptance-email-log.py --base URL --db NAME [--slice write] [--queue on|off]
           [--expect-status sent|failed] [--other-tenant SLUG]
       python3 scripts/acceptance-email-log.py --resend --smtp-cert smtp.pem --smtp-key smtp.key [--smtp-port 2525]
           [--queue on|off] [--no-mail] --base URL --db NAME
       python3 scripts/acceptance-email-log.py --direct --smtp-cert smtp.pem --smtp-key smtp.key [--smtp-port 2525]
           [--queue on|off] --base URL --db NAME
Exit: 0 = ĐẠT · 1 = CHƯA ĐẠT · 2 = lỗi môi trường.
"""
import argparse, json, os, re, socket, subprocess, sys, tempfile, time, urllib.error, urllib.parse, urllib.request, uuid

ap = argparse.ArgumentParser()
ap.add_argument("--base", default="http://localhost:8090/api/v1")
ap.add_argument("--db", default="nexus_platform", help="database the server runs on (rows are read there)")
ap.add_argument("--slice", choices=["write", "all"], default="all")
ap.add_argument("--queue", choices=["on", "off"], default="on")
ap.add_argument("--expect-status", choices=["sent", "failed"], default=None)
ap.add_argument("--other-tenant", default="cong_ty_acme", help="slug of a second tenant (isolation check)")
ap.add_argument("--timeout", type=float, default=60)
ap.add_argument("--resend", action="store_true", help="L2 scenario (needs --smtp-cert/--smtp-key)")
ap.add_argument("--no-mail", action="store_true", help="with --resend: the server has ALERTS_ENABLED=false; check 503 only")
ap.add_argument("--direct", action="store_true", help="L3 scenario (needs --smtp-cert/--smtp-key)")
ap.add_argument("--smtp-cert", help="--resend/--direct: certificate for the fake SMTP the script starts")
ap.add_argument("--smtp-key", help="--resend/--direct: key for the fake SMTP the script starts")
ap.add_argument("--smtp-port", type=int, default=2525, help="--resend/--direct: port of the fake SMTP the server dials")
args = ap.parse_args()
BASE = args.base
if not re.fullmatch(r"[A-Za-z0-9_]+", args.db):
    sys.exit(f"LỖI MÔI TRƯỜNG: tên DB không hợp lệ {args.db!r}")

ENTITY, FIELD = "crm_contacts", "email_ca_nhan"
PII = "a.canhan@gmail.com"
MASKED = "a***@gmail.com"
LONG_SUBJECT = "Chào {{first_name}} " + "x" * 380  # 400 chars


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


def login(email, password, tenant="northwind"):
    st, body = call("POST", "/auth/login", body={"tenant": tenant, "email": email, "password": password})
    if st != 200:
        sys.exit(f"LỖI MÔI TRƯỜNG: đăng nhập {email}: {st} {body.get('code', '')}")
    return body["data"]["accessToken"]


fails = []


def check(label, cond, detail=""):
    print(("  PASS  " if cond else "  FAIL  ") + label + (f"   {detail}" if detail else ""))
    if not cond:
        fails.append(label)


class FakeSMTP:
    """scripts/fake-smtp.py as a child process: restartable between REJECT and ACCEPT, with a DATA counter
    (every client command is printed, so one 'C: DATA' line = one mail the SMTP was handed)."""
    SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "fake-smtp.py")

    def __init__(self, cert, key, port):
        self.cert, self.key, self.port, self.proc = cert, key, port, None
        self.log = tempfile.NamedTemporaryFile("w+", prefix="fake-smtp-", suffix=".log", delete=False)

    def start(self, reject):
        self.stop()
        cmd = [sys.executable, "-u", self.SCRIPT, "--cert", self.cert, "--key", self.key, "--port", str(self.port)]
        if reject:
            cmd.append("--reject-rcpt")
        self.proc = subprocess.Popen(cmd, stdout=self.log, stderr=subprocess.STDOUT)
        deadline = time.time() + 10
        while time.time() < deadline:
            try:
                socket.create_connection(("127.0.0.1", self.port), timeout=1).close()
                return
            except OSError:
                time.sleep(0.2)
        sys.exit(f"LỖI MÔI TRƯỜNG: fake-smtp không lên ở cổng {self.port} (xem {self.log.name})")

    def stop(self):
        if self.proc:
            self.proc.terminate()
            self.proc.wait(timeout=10)
            self.proc = None

    def mails(self):
        self.log.flush()
        with open(self.log.name, encoding="utf-8", errors="replace") as f:
            return sum(1 for line in f if line.rstrip().endswith("C: DATA"))


def poll(label, fn, timeout=None):
    """Wait until fn() is truthy; returns its value (or None on timeout)."""
    deadline = time.time() + (timeout or args.timeout)
    while time.time() < deadline:
        v = fn()
        if v:
            return v
        time.sleep(0.5)
    print(f"  (hết giờ chờ: {label})")
    return None


def run_resend():
    if not args.no_mail and not (args.smtp_cert and args.smtp_key):
        sys.exit("LỖI MÔI TRƯỜNG: --resend cần --smtp-cert và --smtp-key (xem docstring)")
    smtp = None if args.no_mail else FakeSMTP(args.smtp_cert, args.smtp_key, args.smtp_port)
    field_id = wf_id = cust_id = other_id = None
    contacts = []
    admin = login("admin@example.com", "Demo-Admin-2026!")
    rep = login("test@example.com", "Demo-Rep-2026!")
    tag = uuid.uuid4().hex[:8]
    tenant_id = sql("SELECT id FROM tenants WHERE slug = 'northwind'")[0][0]
    new_subject = "Kính chào {{first_name}}"
    try:
        st, body = call("POST", "/admin/fields", admin, {
            "entityName": ENTITY, "name": FIELD, "label": "Email cá nhân", "fieldType": "text", "options": {"sensitive": True}})
        if st == 409:
            sys.exit(f"HỦY: {ENTITY}.{FIELD} đã tồn tại — script không đụng field nó không tạo ra.")
        assert st in (200, 201), f"add field: {st} {body}"
        st, listed = call("GET", f"/metadata/fields/{ENTITY}", admin)
        field_id = next(f["id"] for f in listed.get("data", listed) if f["name"] == FIELD)
        rule = {"name": f"zz nghiệm thu gửi lại {tag}", "entityName": ENTITY, "trigger": "after_create",
                "actions": [{"type": "email_alert", "to": "{{" + FIELD + "}}", "subject": "Chào {{first_name}}", "body": "Xin chào"}]}
        st, body = call("POST", "/admin/workflows", admin, rule)
        assert st in (200, 201), f"create workflow: {st} {body}"
        created = (body.get("data") or {}).get("workflow", {})
        wf_id = created.get("id")
        assert wf_id, f"workflow id missing: {body}"

        # The persona: the customer's own admin — is_admin, NOT a platform operator.
        cust_email, cust_pw = f"quantri-khach-{tag}@example.com", "Demo-Customer-2026!-" + tag
        st, body = call("POST", "/admin/users", admin, {"email": cust_email, "password": cust_pw,
                                                         "fullName": "Quản trị của khách", "isAdmin": True})
        assert st in (200, 201), f"create customer admin: {st} {body}"
        cust_id = ((body.get("data") or {}).get("user") or body.get("data") or {}).get("id")
        cust = login(cust_email, cust_pw)
        st, _ = call("GET", "/admin/api-keys", cust)
        check("nhân vật đúng: quản trị của khách KHÔNG vào được bề mặt builder (403)", st == 403, f"GET /admin/api-keys = {st}")

        if smtp:
            smtp.start(reject=True)
        people = [("Lan", "a.canhan@gmail.com"), ("Mai", "b.canhan@gmail.com"), ("Cúc", "c.canhan@gmail.com")]
        if args.no_mail:
            people = people[:1]
        for first, addr in people:
            st, body = call("POST", f"/{ENTITY}", rep, {"first_name": first, "last_name": f"Gửi lại {tag}", FIELD: addr})
            assert st in (200, 201), f"rep create {first}: {st} {body}"
            contacts.append((first, (body.get("data") or body).get("id")))
        print(f"[setup] rule={wf_id} contacts={[c[1] for c in contacts]} (tạo bởi test@example.com)")

        def failed_rows():
            rows = sql("SELECT id, record_id, status FROM sys_email_log "
                       f"WHERE tenant_id = '{tenant_id}' AND workflow_id = '{wf_id}' AND resend_of IS NULL")
            return rows if len(rows) == len(people) and all(r[2] == "failed" for r in rows) else None
        rows = poll("các email đầu thành Lỗi", failed_rows)
        check(f"setup: {len(people)} email của luật đều Lỗi (SMTP đang từ chối)", rows is not None)
        if rows is None:
            return
        row_of = {r[1]: r[0] for r in rows}  # record id -> log id

        def resend(token, log_id, body=None):
            return call("POST", f"/admin/email-log/{log_id}/resend", token, body)

        if args.no_mail:
            log_id = row_of[contacts[0][1]]
            st, body = resend(cust, log_id)
            check("L2: máy chủ chưa cấu hình gửi mail ⇒ 503 MAIL_NOT_CONFIGURED", st == 503 and body.get("code") == "MAIL_NOT_CONFIGURED", f"{st} {body.get('code')}")
            st, body = resend(cust, log_id, {"dryRun": False})
            check("L2: gửi thật cũng 503 và dòng chưa bị khoá", st == 503 and sql(f"SELECT resent_at FROM sys_email_log WHERE id = '{log_id}'")[0][0] == "NULL", f"{st}")
            return

        # ── the customer admin sees which rows can be resent
        st, body = call("GET", "/admin/email-log?limit=200", cust)
        by_id = {r["id"]: r for r in body.get("data") or []}
        check("L2: danh sách có canResend=true cho mọi dòng Lỗi của luật", st == 200 and all(by_id.get(i, {}).get("canResend") is True for i in row_of.values()),
              json.dumps([by_id.get(i, {}).get("canResend") for i in row_of.values()]))

        # ── the cause is fixed: the SMTP now accepts
        smtp.start(reject=False)
        (first_a, rec_a), (first_b, rec_b), (first_c, rec_c) = contacts
        a, b, c = row_of[rec_a], row_of[rec_b], row_of[rec_c]
        n0 = smtp.mails()

        st, body = resend(cust, a)
        pv = body.get("data") or {}
        check("L2: không body ⇒ XEM TRƯỚC: người nhận đã che, tiêu đề đã dựng, ruleChangedSinceSend=false",
              st == 200 and pv.get("dryRun") is True and pv.get("recipientMasked") == "a***@gmail.com"
              and pv.get("subject") == f"Chào {first_a}" and pv.get("ruleChangedSinceSend") is False and "entry" not in pv, f"{st} {pv}")
        st, body = resend(cust, a, {"dryRun": True})
        check("L2: dryRun=true cũng chỉ xem trước", st == 200 and (body.get("data") or {}).get("dryRun") is True)
        time.sleep(2)
        check("L2: xem trước KHÔNG gửi gì — SMTP nhận 0 thư", smtp.mails() == n0, f"{smtp.mails() - n0} thư")
        check("L2: xem trước KHÔNG khoá dòng", sql(f"SELECT resent_at FROM sys_email_log WHERE id = '{a}'")[0][0] == "NULL")
        check("L2: xem trước KHÔNG sinh dòng nhật ký / việc nền mới",
              int(sql(f"SELECT COUNT(*) FROM sys_email_log WHERE resend_of = '{a}'")[0][0]) == 0)

        st, body = resend(cust, a, {"dryRun": False, "to": "evil@example.com"})
        check("L2: body có khoá lạ (to) ⇒ 400 INVALID_BODY, không gửi gì", st == 400 and body.get("code") == "INVALID_BODY", f"{st} {body.get('code')}")

        # ── the real send
        st, body = resend(cust, a, {"dryRun": False})
        data = body.get("data") or {}
        entry = data.get("entry") or {}
        check("L2: {dryRun:false} ⇒ 200 và trả dòng mới (id đúc sẵn) mang resendOf", st == 200 and data.get("dryRun") is False
              and entry.get("resendOf") == a and entry.get("id"), f"{st} {json.dumps(body, ensure_ascii=False)[:240]}")
        sent = poll("dòng gửi lại thành Đã gửi", lambda: sql(f"SELECT id, status, resend_of, created_by, job_id, recipient_masked, subject_template "
                                                              f"FROM sys_email_log WHERE tenant_id = '{tenant_id}' AND resend_of = '{a}' AND status = 'sent'"))
        check("L2: dòng gửi lại thành Đã gửi, trỏ về dòng cũ, mang người bấm, người nhận đã che, MẪU tiêu đề",
              bool(sent) and sent[0][0] == entry.get("id") and sent[0][3] == cust_id and sent[0][5] == "a***@gmail.com" and sent[0][6] == "Chào {{first_name}}", str(sent))
        check("L2: SMTP nhận ĐÚNG 1 thư", poll("SMTP nhận thư", lambda: smtp.mails() == n0 + 1, 15) is True, f"{smtp.mails() - n0} thư")
        if args.queue == "on" and sent:
            check("L2: hàng đợi bật ⇒ dòng mới có job_id", bool(sent[0][4]) and sent[0][4] != "NULL", sent[0][4])
            pl = sql(f"SELECT IFNULL(CAST(payload AS CHAR),'NULL'), status FROM sys_jobs WHERE id = '{sent[0][4]}'")
            check("L2: payload job gửi lại đã kết thúc là NULL (#95 giữ nguyên)", bool(pl) and pl[0][0] == "NULL" and pl[0][1] == "done", str(pl))
        elif args.queue == "off" and sent:
            check("L2: hàng đợi tắt (sender gửi thẳng) ⇒ dòng mới có resend_of, attempts=1, không job",
                  sent[0][2] == a and sent[0][4] == "NULL", str(sent))
        st, body = call("GET", "/admin/email-log?limit=200", cust)
        by_id = {r["id"]: r for r in body.get("data") or []}
        old, new = by_id.get(a, {}), by_id.get(entry.get("id"), {})
        check("L2: dòng cũ có resentAt, hết canResend; dòng mới có resendOf",
              bool(old.get("resentAt")) and old.get("canResend") is False and new.get("resendOf") == a, f"{old.get('resentAt')} {old.get('canResend')} {new.get('resendOf')}")

        st, body = resend(cust, a, {"dryRun": False})
        check("L2: bấm lần hai ⇒ 409 EMAIL_NOT_RESENDABLE", st == 409 and body.get("code") == "EMAIL_NOT_RESENDABLE", f"{st} {body.get('code')}")
        time.sleep(2)
        check("L2: bấm lần hai KHÔNG gửi thêm — SMTP vẫn 1 thư", smtp.mails() == n0 + 1, f"{smtp.mails() - n0} thư")

        # ── the rule changes: the next preview says so and shows the NEW subject
        st, body = call("PUT", f"/admin/workflows/{wf_id}", admin, {
            "name": rule["name"], "entityName": ENTITY, "trigger": "after_create", "isActive": True,
            "actions": [{"type": "email_alert", "to": "{{" + FIELD + "}}", "subject": new_subject, "body": "Xin chào"}]})
        check("setup: sửa mẫu tiêu đề của luật", st == 200, f"{st} {body.get('code')}")
        st, body = resend(cust, b)
        pv = body.get("data") or {}
        check("L2: luật đã đổi ⇒ ruleChangedSinceSend=true và tiêu đề là bản MỚI",
              st == 200 and pv.get("ruleChangedSinceSend") is True and pv.get("subject") == f"Kính chào {first_b}", f"{st} {pv}")

        # ── the record goes to the bin: nothing to rebuild the mail from
        st, _ = call("DELETE", f"/{ENTITY}/{rec_c}", admin)
        check("setup: đưa contact thứ ba vào thùng rác", st in (200, 204), f"{st}")
        st, body = resend(cust, c)
        check("L2: bản ghi trong thùng rác ⇒ 409 EMAIL_RESEND_RECORD_GONE", st == 409 and body.get("code") == "EMAIL_RESEND_RECORD_GONE", f"{st} {body.get('code')}")
        st, body = resend(cust, c, {"dryRun": False})
        check("L2: gửi thật cũng 409 và dòng không bị khoá", st == 409 and sql(f"SELECT resent_at FROM sys_email_log WHERE id = '{c}'")[0][0] == "NULL", f"{st}")

        # ── who may press
        st, body = resend(rep, b, {"dryRun": False})
        check("L2: nhân viên test@example.com (không admin) ⇒ 403", st == 403, f"{st}")
        other_t = sql(f"SELECT id FROM tenants WHERE slug = '{args.other_tenant}'")
        if not other_t:
            check("L2: có tenant thứ hai để kiểm cô lập", False, f"không thấy tenant {args.other_tenant}")
        else:
            other_id = str(uuid.uuid4())
            other_email = f"zz-b-{tag}@isolation.test"
            sql("INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin) "
                f"SELECT '{other_id}', '{other_t[0][0]}', '{other_email}', password_hash, 'Quản trị B', 'active', 1 "
                f"FROM users WHERE email = 'admin@example.com' AND tenant_id = '{tenant_id}'")
            other = login(other_email, "Demo-Admin-2026!", args.other_tenant)
            st1, foreign = resend(other, b, {"dryRun": False})
            st2, ghost = resend(other, str(uuid.uuid4()), {"dryRun": False})
            check("L2: admin tenant B bấm id của tenant A ⇒ 404 EMAIL_LOG_NOT_FOUND, thân giống HỆT id không tồn tại",
                  st1 == 404 and st2 == 404 and foreign == ghost and foreign.get("code") == "EMAIL_LOG_NOT_FOUND", f"{st1} {foreign} / {st2} {ghost}")
            check("L2: dòng của tenant A không bị tenant B khoá",
                  sql(f"SELECT resent_at FROM sys_email_log WHERE id = '{b}'")[0][0] == "NULL")

        # ── send B too (the rule is NEW now) and audit
        n1 = smtp.mails()
        st, body = resend(cust, b, {"dryRun": False})
        check("L2: gửi dòng thứ hai (luật đã đổi) ⇒ 200", st == 200, f"{st} {body.get('code')}")
        check("L2: SMTP nhận thêm đúng 1 thư (tổng 2)", poll("SMTP nhận thư thứ hai", lambda: smtp.mails() == n1 + 1, 15) is True, f"{smtp.mails() - n0} thư")

        def audited():
            s, bd = call("GET", "/audit-log?entityName=__email__&limit=50", admin)
            return [r for r in bd.get("data") or [] if r.get("recordId") in (a, b)] if s == 200 else None
        trail = poll("audit __email__", lambda: len(audited() or []) >= 2 and audited())
        check("L2: audit __email__ có MỘT dòng cho mỗi lần gửi lại, actor = quản trị của khách",
              bool(trail) and len(trail) == 2 and all(r.get("actorUserId") == cust_id for r in trail), json.dumps(trail, ensure_ascii=False)[:300])

        # ── the log still holds nothing rendered
        hits = int(sql(f"SELECT COUNT(*) FROM sys_email_log WHERE workflow_id = '{wf_id}' AND LOWER(CONCAT_WS('|', recipient_masked, "
                       "subject_template, last_error)) LIKE '%canhan%'")[0][0])
        check("SC-002: quét sys_email_log của lần chạy: KHÔNG dòng nào chứa 'canhan'", hits == 0, f"COUNT = {hits}")
        hits = int(sql(f"SELECT COUNT(*) FROM sys_email_log WHERE workflow_id = '{wf_id}' AND CONCAT_WS('|', subject_template, last_error) "
                       "REGEXP 'Chào (Lan|Mai|Cúc)|Kính chào (Lan|Mai|Cúc)'")[0][0])
        check("SC-002: KHÔNG dòng nào chứa tiêu đề đã điền", hits == 0, f"COUNT = {hits}")
        if args.queue == "on":
            n = int(sql("SELECT COUNT(*) FROM sys_jobs WHERE job_type = 'email_alert' AND status IN ('done','failed') "
                        f"AND payload IS NOT NULL AND tenant_id = '{tenant_id}' AND created_at >= NOW() - INTERVAL 1 HOUR")[0][0])
            check("SC-002: 0 job email_alert đã kết thúc còn payload", n == 0, f"COUNT = {n}")
    finally:
        if smtp:
            smtp.stop()
        if wf_id:
            call("DELETE", f"/admin/workflows/{wf_id}", admin)
            sql(f"DELETE FROM sys_email_log WHERE workflow_id = '{wf_id}'")
        for _, rid in contacts:
            call("DELETE", f"/{ENTITY}/{rid}", admin)
        if cust_id:
            call("DELETE", f"/admin/users/{cust_id}", admin)
        if other_id:
            sql(f"DELETE FROM users WHERE id = '{other_id}'")
        if field_id:
            call("DELETE", f"/admin/fields/{field_id}", admin)
            if re.fullmatch(r"[0-9a-fA-F-]{36}", field_id):
                sql(f"DELETE FROM sys_fields WHERE id='{field_id}' AND deleted_at IS NOT NULL")


def run_direct():
    if not (args.smtp_cert and args.smtp_key):
        sys.exit("LỖI MÔI TRƯỜNG: --direct cần --smtp-cert và --smtp-key (xem docstring)")
    smtp = FakeSMTP(args.smtp_cert, args.smtp_key, args.smtp_port)
    field_id = wf_id = cust_id = None
    contacts = []
    admin = login("admin@example.com", "Demo-Admin-2026!")
    rep = login("test@example.com", "Demo-Rep-2026!")
    tag = uuid.uuid4().hex[:8]
    tenant_id = sql("SELECT id FROM tenants WHERE slug = 'northwind'")[0][0]
    rep_id = sql(f"SELECT id FROM users WHERE tenant_id = '{tenant_id}' AND email = 'test@example.com'")[0][0]
    RESET_SUBJECT = "Đặt lại mật khẩu tài khoản Nexus"
    t0 = sql("SELECT NOW(3)")[0][0]

    def auth_rows(source=None):
        q = ("SELECT id, source, status, recipient_masked, IFNULL(subject_template,''), IFNULL(user_id,''), IFNULL(created_by,''), "
             f"IFNULL(last_error,''), attempts FROM sys_email_log WHERE tenant_id = '{tenant_id}' AND created_at >= '{t0}' "
             f"AND source {'= ' + repr(source) if source else '<> ' + repr('workflow')}")
        return sql(q)

    try:
        smtp.start(reject=False)
        m0 = smtp.mails()

        # ── a forgot-password request for a KNOWN employee
        st, known = call("POST", "/auth/forgot-password", None, {"tenant": "northwind", "email": "test@example.com"})
        rows = poll("dòng password_reset", lambda: auth_rows("password_reset"))
        check("L3: quên mật khẩu cho test@example.com ⇒ 200", st == 200, f"{st} {known}")
        check("L3: đúng MỘT dòng password_reset, đã gửi, người nhận t***@example.com, tiêu đề cố định, đúng user, không người bấm",
              bool(rows) and len(rows) == 1 and rows[0][2] == "sent" and rows[0][3] == "t***@example.com" and rows[0][4] == RESET_SUBJECT
              and rows[0][5] == rep_id and rows[0][6] == "" and rows[0][8] == "1", str(rows))
        check("L3: SMTP nhận đúng 1 thư đặt lại", poll("SMTP nhận thư", lambda: smtp.mails() == m0 + 1, 15) is True, f"{smtp.mails() - m0} thư")

        # ── UNKNOWN account / tenant: same answer, no row, no mail
        m1 = smtp.mails()
        t_known = time.time()
        call("POST", "/auth/forgot-password", None, {"tenant": "northwind", "email": "test@example.com"})
        d_known = time.time() - t_known
        t_unk = time.time()
        st2, unknown = call("POST", "/auth/forgot-password", None, {"tenant": "northwind", "email": f"khong-co-{tag}@example.com"})
        d_unk = time.time() - t_unk
        st3, no_tenant = call("POST", "/auth/forgot-password", None, {"tenant": f"khong-co-{tag}", "email": "test@example.com"})
        check("L3: email lạ / tenant lạ ⇒ cùng mã trạng thái và cùng thân với email có thật",
              st2 == st == st3 and unknown == known and no_tenant == known, f"{st}/{st2}/{st3} {known} | {unknown} | {no_tenant}")
        print(f"         (đo: có thật {d_known * 1000:.0f} ms · lạ {d_unk * 1000:.0f} ms — chỉ để xem, không phải điều kiện)")
        time.sleep(3)
        rows = auth_rows("password_reset")
        check("L3: email lạ / tenant lạ KHÔNG sinh dòng nhật ký (chỉ 2 dòng của test@example.com)", len(rows) == 2 and all(r[5] == rep_id for r in rows), str(rows))
        check("L3: email lạ / tenant lạ KHÔNG gửi thư (chỉ thêm 1 thư của lần có thật)", poll("SMTP", lambda: smtp.mails() == m1 + 1, 15) is True, f"{smtp.mails() - m1} thư")

        # ── the customer's own admin issues a link for the employee
        cust_email, cust_pw = f"quantri-khach-{tag}@example.com", "Demo-Customer-2026!-" + tag
        st, body = call("POST", "/admin/users", admin, {"email": cust_email, "password": cust_pw, "fullName": "Quản trị của khách", "isAdmin": True})
        assert st in (200, 201), f"create customer admin: {st} {body}"
        cust_id = ((body.get("data") or {}).get("user") or body.get("data") or {}).get("id")
        cust = login(cust_email, cust_pw)
        st, _ = call("GET", "/admin/api-keys", cust)
        check("nhân vật đúng: quản trị của khách KHÔNG vào được bề mặt builder (403)", st == 403, f"GET /admin/api-keys = {st}")
        st, body = call("POST", f"/admin/users/{rep_id}/reset-password-link", cust)
        issued = body.get("data") or {}
        check("L3: quản trị khách cấp link cho test@example.com ⇒ 201, có link, emailQueued", st == 201 and issued.get("token") and issued.get("emailQueued") is True, f"{st} {body.get('code')}")
        link_rows = poll("dòng reset_link", lambda: auth_rows("reset_link"))
        check("L3: đúng MỘT dòng reset_link, đã gửi, người bấm = quản trị khách, đúng user, người nhận đã che",
              bool(link_rows) and len(link_rows) == 1 and link_rows[0][2] == "sent" and link_rows[0][6] == cust_id and link_rows[0][5] == rep_id
              and link_rows[0][3] == "t***@example.com" and link_rows[0][4] == RESET_SUBJECT, str(link_rows))
        token = issued.get("token") or "?"
        hits = int(sql(f"SELECT COUNT(*) FROM sys_email_log WHERE tenant_id = '{tenant_id}' AND created_at >= '{t0}' AND "
                       f"CONCAT_WS('|', recipient_masked, subject_template, last_error, IFNULL(user_id,''), IFNULL(created_by,'')) LIKE '%{token}%'")[0][0])
        check("L3: KHÔNG dòng nào chứa token thô", hits == 0, f"COUNT = {hits}")
        hits = int(sql(f"SELECT COUNT(*) FROM sys_email_log WHERE tenant_id = '{tenant_id}' AND created_at >= '{t0}' AND source <> 'workflow' AND "
                       "LOWER(CONCAT_WS('|', recipient_masked, subject_template, last_error)) LIKE '%test@example.com%'")[0][0])
        check("L3: KHÔNG dòng thư xác thực nào chứa địa chỉ đầy đủ test@example.com", hits == 0, f"COUNT = {hits}")

        # ── read side: filter, issuer name, never resendable
        st, body = call("GET", "/admin/email-log?source=reset_link&limit=200", cust)
        data = body.get("data") or []
        check("L3: ?source=reset_link ⇒ chỉ dòng reset_link, kèm tên người cấp, canResend=false",
              st == 200 and len(data) >= 1 and all(r["source"] == "reset_link" for r in data)
              and any(r.get("createdBy") == cust_id and r.get("createdByName") == "Quản trị của khách" for r in data)
              and all(r["canResend"] is False for r in data), f"{st} {json.dumps(data[:1], ensure_ascii=False)[:200]}")
        st, body = call("GET", "/admin/email-log?source=password_reset&limit=200", cust)
        data = body.get("data") or []
        check("L3: ?source=password_reset ⇒ chỉ dòng đặt lại, createdBy rỗng, canResend=false",
              st == 200 and len(data) >= 2 and all(r["source"] == "password_reset" and r["createdBy"] is None and r["canResend"] is False for r in data), f"{st}")
        st, body = call("GET", "/admin/email-log?source=chay-roi", cust)
        check("L3: source lạ ⇒ 400 EMAIL_LOG_FILTER_INVALID", st == 400 and body.get("code") == "EMAIL_LOG_FILTER_INVALID", f"{st} {body.get('code')}")
        st, _ = call("GET", "/admin/email-log?source=reset_link", rep)
        check("L3: nhân viên test@example.com (không admin) ⇒ 403 trên cùng đường dẫn", st == 403, f"{st}")
        st, body = call("GET", "/admin/email-log?limit=200", cust)
        raw = json.dumps(body, ensure_ascii=False)
        check("L3: phản hồi danh sách KHÔNG chứa token thô hay địa chỉ đầy đủ", token not in raw and "test@example.com" not in raw)
        log_id = next((r["id"] for r in body.get("data") or [] if r["source"] == "reset_link"), None)
        st, body = call("POST", f"/admin/email-log/{log_id}/resend", cust, {"dryRun": False})
        check("L3: Gửi lại một thư xác thực ⇒ 409 EMAIL_NOT_RESENDABLE (token dùng một lần)", st == 409 and body.get("code") == "EMAIL_NOT_RESENDABLE", f"{st} {body.get('code')}")

        # ── a rule mail that cannot be built: the contact has no email
        st, body = call("POST", "/admin/fields", admin, {
            "entityName": ENTITY, "name": FIELD, "label": "Email cá nhân", "fieldType": "text", "options": {"sensitive": True}})
        if st == 409:
            sys.exit(f"HỦY: {ENTITY}.{FIELD} đã tồn tại — script không đụng field nó không tạo ra.")
        assert st in (200, 201), f"add field: {st} {body}"
        st, listed = call("GET", f"/metadata/fields/{ENTITY}", admin)
        field_id = next(f["id"] for f in listed.get("data", listed) if f["name"] == FIELD)
        rule_name = f"zz nghiệm thu thư lỗi trước khi gửi {tag}"
        st, body = call("POST", "/admin/workflows", admin, {"name": rule_name, "entityName": ENTITY, "trigger": "after_create",
                        "actions": [{"type": "email_alert", "to": "{{" + FIELD + "}}", "subject": "Chào {{first_name}}", "body": "Xin chào"}]})
        assert st in (200, 201), f"create workflow: {st} {body}"
        wf_id = ((body.get("data") or {}).get("workflow") or {}).get("id")
        st, body = call("POST", f"/{ENTITY}", rep, {"first_name": "Lan", "last_name": f"Không email {tag}"})
        assert st in (200, 201), f"rep create: {st} {body}"
        rec_id = (body.get("data") or body).get("id")
        contacts.append(rec_id)
        skipped = poll("dòng bị bỏ qua", lambda: sql(
            "SELECT id, status, recipient_masked, IFNULL(last_error,''), attempts, IFNULL(workflow_id,''), IFNULL(record_id,''), IFNULL(subject_template,'') "
            f"FROM sys_email_log WHERE tenant_id = '{tenant_id}' AND workflow_id = '{wf_id}' AND resend_of IS NULL"))
        sk = skipped[0] if skipped else None
        check("L3: contact không có email ⇒ MỘT dòng Lỗi, không người nhận, 'không có địa chỉ nhận', 0 lần thử, gắn luật + bản ghi",
              bool(sk) and len(skipped) == 1 and sk[1] == "failed" and sk[2] == "" and "không có địa chỉ nhận" in sk[3] and sk[4] == "0"
              and sk[5] == wf_id and sk[6] == rec_id and sk[7] == "Chào {{first_name}}", str(skipped))
        if sk:
            check("L3: lỗi của dòng nêu MẪU {{email_ca_nhan}}, không nêu dữ liệu", "{{" + FIELD + "}}" in sk[3] and "Lan" not in sk[3], sk[3][:160])
        st, body = call("GET", "/admin/email-log?status=failed&limit=200", cust)
        row = next((r for r in body.get("data") or [] if sk and r["id"] == sk[0]), {})
        check("L3: màn danh sách thấy dòng đó với canResend=true, người nhận rỗng", row.get("canResend") is True and row.get("recipientMasked") == "", json.dumps(row, ensure_ascii=False)[:240])
        st, body = call("POST", f"/admin/email-log/{sk[0]}/resend", cust) if sk else (0, {})
        check("L3: xem trước khi bản ghi vẫn trống ⇒ 422 EMAIL_RESEND_NO_RECIPIENT", st == 422 and body.get("code") == "EMAIL_RESEND_NO_RECIPIENT", f"{st} {body.get('code')}")

        # ── the admin fills the email, then "Gửi lại" goes out
        st, body = call("PUT", f"/{ENTITY}/{rec_id}", admin, {FIELD: "a.canhan@gmail.com"})
        check("setup: quản trị điền email cho contact", st in (200, 204), f"{st} {body.get('code')}")
        m2 = smtp.mails()
        st, body = call("POST", f"/admin/email-log/{sk[0]}/resend", cust) if sk else (0, {})
        pv = body.get("data") or {}
        check("L3: điền email xong ⇒ xem trước 200, người nhận a***@gmail.com, tiêu đề đã dựng", st == 200 and pv.get("recipientMasked") == "a***@gmail.com" and pv.get("subject") == "Chào Lan", f"{st} {pv}")
        st, body = call("POST", f"/admin/email-log/{sk[0]}/resend", cust, {"dryRun": False}) if sk else (0, {})
        check("L3: Gửi lại thành công ⇒ 200", st == 200 and ((body.get("data") or {}).get("entry") or {}).get("resendOf") == sk[0], f"{st} {body.get('code')}")
        sent = poll("dòng gửi lại thành Đã gửi", lambda: sql(f"SELECT id FROM sys_email_log WHERE tenant_id = '{tenant_id}' AND resend_of = '{sk[0]}' AND status = 'sent'")) if sk else None
        check("L3: dòng gửi lại thành Đã gửi, SMTP nhận đúng 1 thư", bool(sent) and poll("SMTP", lambda: smtp.mails() == m2 + 1, 15) is True, f"{smtp.mails() - m2} thư")
        st, _ = call("POST", f"/admin/email-log/{sk[0]}/resend", rep, {"dryRun": False}) if sk else (0, {})
        check("L3: nhân viên test@example.com bấm Gửi lại ⇒ 403", st == 403, f"{st}")
        hits = int(sql(f"SELECT COUNT(*) FROM sys_email_log WHERE workflow_id = '{wf_id}' AND LOWER(CONCAT_WS('|', recipient_masked, subject_template, last_error)) LIKE '%canhan%'")[0][0])
        check("SC-002: dòng của luật KHÔNG chứa 'canhan'", hits == 0, f"COUNT = {hits}")
    finally:
        smtp.stop()
        if wf_id:
            call("DELETE", f"/admin/workflows/{wf_id}", admin)
            sql(f"DELETE FROM sys_email_log WHERE workflow_id = '{wf_id}'")
        sql(f"DELETE FROM sys_email_log WHERE tenant_id = '{tenant_id}' AND created_at >= '{t0}' AND source IN ('password_reset', 'reset_link') AND user_id = '{rep_id}'")
        for rid in contacts:
            call("DELETE", f"/{ENTITY}/{rid}", admin)
        if cust_id:
            call("DELETE", f"/admin/users/{cust_id}", admin)
        if field_id:
            call("DELETE", f"/admin/fields/{field_id}", admin)
            if re.fullmatch(r"[0-9a-fA-F-]{36}", field_id):
                sql(f"DELETE FROM sys_fields WHERE id='{field_id}' AND deleted_at IS NOT NULL")


if args.resend or args.direct:
    (run_resend if args.resend else run_direct)()
    print()
    print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)} điểm)")
    sys.exit(1 if fails else 0)

field_id = record_id = cust_id = other_id = None
wf_ids = []
admin = login("admin@example.com", "Demo-Admin-2026!")
rep = login("test@example.com", "Demo-Rep-2026!")
tag = uuid.uuid4().hex[:8]
try:
    st, body = call("POST", "/admin/fields", admin, {
        "entityName": ENTITY, "name": FIELD, "label": "Email cá nhân", "fieldType": "text", "options": {"sensitive": True},
    })
    if st == 409:
        sys.exit(f"HỦY: {ENTITY}.{FIELD} đã tồn tại — script không đụng field nó không tạo ra.")
    assert st in (200, 201), f"add field: {st} {body}"
    st, listed = call("GET", f"/metadata/fields/{ENTITY}", admin)
    field_id = next(f["id"] for f in listed.get("data", listed) if f["name"] == FIELD)

    rules = {}
    for key, subject in (("normal", "Chào {{first_name}}"), ("long", LONG_SUBJECT)):
        name = f"zz nghiệm thu nhật ký email {key} {tag}"
        st, body = call("POST", "/admin/workflows", admin, {
            "name": name, "entityName": ENTITY, "trigger": "after_create",
            "actions": [{"type": "email_alert", "to": "{{" + FIELD + "}}", "subject": subject, "body": "Xin chào"}],
        })
        assert st in (200, 201), f"create workflow {key}: {st} {body}"
        wid = (body.get("data") or {}).get("workflow", {}).get("id")
        assert wid, f"workflow id missing: {body}"
        wf_ids.append(wid)
        rules[key] = {"id": wid, "name": name, "subject": subject}

    tenant_id = sql("SELECT id FROM tenants WHERE slug = 'northwind'")[0][0]
    t0 = sql("SELECT NOW(3)")[0][0]
    st, body = call("POST", f"/{ENTITY}", rep, {"first_name": "Nguyễn Thị", "last_name": f"Nhật Ký {tag}", FIELD: PII})
    assert st in (200, 201), f"rep create: {st} {body}"
    record_id = (body.get("data") or body).get("id")
    print(f"[setup] rules={wf_ids} record={record_id} (tạo bởi test@example.com) t0={t0}")

    ids_sql = ",".join(f"'{w}'" for w in wf_ids)
    rows = []
    deadline = time.time() + args.timeout
    while time.time() < deadline:
        rows = sql("SELECT id, tenant_id, source, status, recipient_masked, IFNULL(subject_template,''), workflow_id, "
                   "IFNULL(job_id,''), attempts, IFNULL(last_error,''), IFNULL(entity_name,''), IFNULL(record_id,'') "
                   f"FROM sys_email_log WHERE workflow_id IN ({ids_sql}) AND created_at >= '{t0}'")
        if len(rows) >= 2 and all(r[3] in ("sent", "failed") for r in rows):
            break
        time.sleep(0.5)
    by_wf = {r[6]: r for r in rows}
    check("L1a: đúng MỘT dòng nhật ký cho mỗi luật (2 luật ⇒ 2 dòng) và cả hai đã kết thúc", len(rows) == 2 and len(by_wf) == 2 and all(r[3] in ("sent", "failed") for r in rows), f"{len(rows)} dòng")
    for key, rule in rules.items():
        r = by_wf.get(rule["id"])
        if not r:
            check(f"L1a [{key}] có dòng", False)
            continue
        print(f"         [{key}] status={r[3]} attempts={r[8]} job={r[7] or '-'} masked={r[4]} subject_len={len(r[5])} err={r[9][:90]!r}")
        check(f"L1a [{key}] gắn đúng tenant, nguồn workflow", r[1] == tenant_id and r[2] == "workflow")
        check(f"L1a [{key}] người nhận đã che {MASKED}", r[4] == MASKED, r[4])
        want_subject = rule["subject"] if len(rule["subject"]) <= 255 else rule["subject"][:254] + "…"
        check(f"L1a [{key}] mẫu tiêu đề (không bản đã điền)" + (", cắt 255 ký tự" if key == "long" else ""), r[5] == want_subject, f"len={len(r[5])}")
        check(f"L1a [{key}] bản ghi nguồn", r[10] == ENTITY and r[11] == record_id)
        if args.expect_status:
            check(f"L1a [{key}] trạng thái {args.expect_status}", r[3] == args.expect_status, r[3])
        if r[3] == "failed":
            check(f"L1a [{key}] lỗi không chứa địa chỉ", PII.lower() not in r[9].lower() and "canhan" not in r[9].lower(), r[9][:120])
        if args.queue == "on":
            check(f"L1a [{key}] hàng đợi bật ⇒ có job_id", r[7] != "")
            if r[7]:
                pl = sql(f"SELECT IFNULL(CAST(payload AS CHAR),'NULL'), status FROM sys_jobs WHERE id = '{r[7]}'")
                check(f"L1a [{key}] payload của job đã kết thúc là NULL (#95 giữ nguyên)", bool(pl) and pl[0][0] == "NULL" and pl[0][1] in ("done", "failed"), str(pl))
        else:
            check(f"L1a [{key}] hàng đợi tắt ⇒ dòng cuối do sender gửi thẳng ghi (attempts=1, không job)", r[7] == "" and r[8] == "1")
    hits = int(sql("SELECT COUNT(*) FROM sys_email_log WHERE created_at >= '" + t0 + "' AND LOWER(CONCAT_WS('|', recipient_masked, "
                   "subject_template, last_error)) LIKE '%canhan%'")[0][0])
    check("L1a: quét sys_email_log của lần chạy: KHÔNG dòng nào chứa 'canhan' (địa chỉ)", hits == 0, f"COUNT = {hits}")
    hits = int(sql("SELECT COUNT(*) FROM sys_email_log WHERE created_at >= '" + t0 + "' AND CONCAT_WS('|', subject_template, last_error) "
                   "LIKE '%Nguyễn Thị%'")[0][0])
    check("L1a: KHÔNG dòng nào chứa tiêu đề đã điền ('Nguyễn Thị')", hits == 0, f"COUNT = {hits}")

    if args.slice == "all":
        # The persona: a tenant-admin of northwind who is not a platform operator.
        cust_email, cust_pw = f"quantri-khach-{tag}@example.com", "Demo-Customer-2026!-" + tag
        st, body = call("POST", "/admin/users", admin, {"email": cust_email, "password": cust_pw,
                                                         "fullName": "Quản trị của khách", "isAdmin": True})
        assert st in (200, 201), f"create customer admin: {st} {body}"
        cust_id = ((body.get("data") or {}).get("user") or body.get("data") or {}).get("id")
        cust = login(cust_email, cust_pw)
        st, _ = call("GET", "/admin/api-keys", cust)
        check("nhân vật đúng: quản trị của khách KHÔNG vào được bề mặt builder (403)", st == 403, f"GET /admin/api-keys = {st}")

        st, body = call("GET", "/admin/email-log?limit=200", cust)
        check("L1b: GET /admin/email-log = 200 cho quản trị của khách", st == 200, f"status={st}")
        data = {r["workflowId"]: r for r in body.get("data") or [] if r.get("workflowId") in wf_ids}
        check("L1b: thấy cả hai dòng của lần chạy", len(data) == 2)
        raw = json.dumps(body, ensure_ascii=False)
        check("L1b: phản hồi KHÔNG chứa địa chỉ đầy đủ", PII.lower() not in raw.lower() and "canhan" not in raw.lower())
        check("L1b: phản hồi có meta.mailConfigured / queueEnabled", "mailConfigured" in (body.get("meta") or {}) and "queueEnabled" in (body.get("meta") or {}), str(body.get("meta")))
        for key, rule in rules.items():
            r = data.get(rule["id"])
            if not r:
                continue
            check(f"L1b [{key}] người nhận đã che, tên luật, bảng, bản ghi, trạng thái",
                  r.get("recipientMasked") == MASKED and r.get("workflowName") == rule["name"]
                  and r.get("entityName") == ENTITY and r.get("recordId") == record_id and r.get("status") in ("sent", "failed"),
                  json.dumps(r, ensure_ascii=False)[:260])
        st, body = call("GET", "/admin/email-log?status=failed&limit=200", cust)
        failed_ids = {r["workflowId"] for r in body.get("data") or []}
        expect_failed = {r[6] for r in rows if r[3] == "failed"}
        check("L1b: lọc status=failed trả đúng các dòng Lỗi", expect_failed <= failed_ids and all(r["status"] == "failed" for r in body.get("data") or []))
        st, body = call("GET", "/admin/email-log?status=chay-roi", cust)
        check("L1b: status lạ ⇒ 400 EMAIL_LOG_FILTER_INVALID", st == 400 and body.get("code") == "EMAIL_LOG_FILTER_INVALID", f"{st} {body.get('code')}")
        st, first = call("GET", "/admin/email-log?limit=1", cust)
        nxt = (first.get("pagination") or {}).get("nextCursor")
        check("L1b: limit=1 ⇒ có nextCursor", bool(nxt))
        if nxt:
            st2, second = call("GET", "/admin/email-log?limit=1&after=" + urllib.parse.quote(nxt), cust)
            check("L1b: trang 2 theo cursor khác trang 1", st2 == 200 and second["data"] and second["data"][0]["id"] != first["data"][0]["id"])
            st3, b3 = call("GET", "/admin/jobs?after=" + urllib.parse.quote(nxt), cust)
            check("L1b: cursor của nhật ký email đưa sang /admin/jobs ⇒ 400 INVALID_CURSOR", st3 == 400 and b3.get("code") == "INVALID_CURSOR", f"{st3} {b3.get('code')}")
        st, _ = call("GET", "/admin/email-log", rep)
        check("L1b: nhân viên test@example.com (không admin) ⇒ 403", st == 403, f"status={st}")
        # Another tenant's admin: a user cloned into the second tenant (same password hash as admin@example.com).
        other_t = sql(f"SELECT id FROM tenants WHERE slug = '{args.other_tenant}'")
        if not other_t:
            check("L1b: có tenant thứ hai để kiểm cô lập", False, f"không thấy tenant {args.other_tenant}")
        else:
            other_id = str(uuid.uuid4())
            other_email = f"zz-b-{tag}@isolation.test"
            sql("INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin) "
                f"SELECT '{other_id}', '{other_t[0][0]}', '{other_email}', password_hash, 'Quản trị B', 'active', 1 "
                f"FROM users WHERE email = 'admin@example.com' AND tenant_id = '{tenant_id}'")
            other = login(other_email, "Demo-Admin-2026!", args.other_tenant)
            st, body = call(f"GET", "/admin/email-log?limit=200&tenantId=" + tenant_id, other)
            mine = [r for r in body.get("data") or [] if r.get("workflowId") in wf_ids or r.get("recordId") == record_id]
            check("L1b: admin tenant B (kể cả ?tenantId=northwind) thấy 0 dòng của tenant northwind", st == 200 and not mine, f"status={st}, {len(mine)} dòng")

    # Hard delete takes the rows with it (bin first — purge refuses a live record — then purge as operator).
    st, _ = call("DELETE", f"/{ENTITY}/{record_id}", admin)
    check("xoá mềm bản ghi", st in (200, 204), f"status={st}")
    st, body = call("POST", f"/{ENTITY}/{record_id}/purge", admin, {"dryRun": False})
    check("xoá hẳn bản ghi (operator)", st == 200, f"{st} {body.get('code')}")
    n = int(sql(f"SELECT COUNT(*) FROM sys_email_log WHERE tenant_id = '{tenant_id}' AND record_id = '{record_id}'")[0][0])
    check("L1a: xoá hẳn bản ghi ⇒ dòng nhật ký của nó biến mất", n == 0, f"COUNT = {n}")
finally:
    if wf_ids:
        for w in wf_ids:
            call("DELETE", f"/admin/workflows/{w}", admin)
        sql("DELETE FROM sys_email_log WHERE workflow_id IN (" + ",".join(f"'{w}'" for w in wf_ids) + ")")
    if record_id:
        call("DELETE", f"/{ENTITY}/{record_id}", admin)
    if cust_id:
        call("DELETE", f"/admin/users/{cust_id}", admin)
    if other_id:
        sql(f"DELETE FROM users WHERE id = '{other_id}'")
    if field_id:
        call("DELETE", f"/admin/fields/{field_id}", admin)
        if re.fullmatch(r"[0-9a-fA-F-]{36}", field_id):
            sql(f"DELETE FROM sys_fields WHERE id='{field_id}' AND deleted_at IS NOT NULL")

print()
print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)} điểm)")
sys.exit(1 if fails else 0)
