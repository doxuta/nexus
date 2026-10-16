#!/usr/bin/env python3
"""email_alert job privacy (queue #95 + #96) — third-person acceptance on a REAL server running the
DEFAULT durable-queue mode (JOBS_ENABLED on).

#95 — an email_alert job carries the RENDERED recipient in sys_jobs.payload (the worker needs it to
send). Once the job is finished — delivered, or out of attempts — no sys_jobs row may still hold
the address: it used to sit there for JOBS_RETENTION_DAYS in a table with no field-level ACL.

#96 — a real SMTP server that rejects RCPT TO echoes the address it was given
("550 5.1.1 <x@y>: Recipient address rejected"), often lowercased. sys_jobs.last_error must carry
"[redacted]" and never the address in any case. Checked with --expect-echo against an SMTP that
echoes (scripts/fake-smtp.py --reject-rcpt); a dev server's NoopSender never echoes, so without the
flag this check only proves the address is absent.

Setup (admin — only an admin adds fields and rules): a text field email_ca_nhan on crm_contacts
flagged sensitive, and a rule mailing {{email_ca_nhan}} after create. Trigger: the NON-ADMIN
employee test@example.com (crm_sales_rep) creates the contact, so a real person's action fires it.
sys_jobs has no API yet (the jobs viewer is queue #31), so the job row is read from the server's
database through `docker exec nexus-mysql` (--db). Everything the run creates is removed in finally;
the sys_jobs row is left as the evidence.

For a verdict in seconds start the server with JOBS_POLL_INTERVAL_SEC=1, and JOBS_MAX_ATTEMPTS=1 when
the send is expected to fail (the first failure is then terminal instead of a 7-minute backoff).

#96 needs a server that really talks SMTP+STARTTLS to scripts/fake-smtp.py and TRUSTS its cert.
Go on macOS ignores SSL_CERT_FILE, so run the server as a Linux binary in a container (certs as in
fake-smtp.py's docstring, CN/SAN host.docker.internal):

    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/server-linux ./cmd/server/
    python3 scripts/fake-smtp.py --cert smtp.pem --key smtp.key --reject-rcpt   # on the host
    docker run -d -p 127.0.0.1:8191:8191 --env-file srv.env -v "$PWD/bin:/app:ro" -v "$CERTS:/certs:ro" \\
        -w /tmp alpine:3 /app/server-linux
    # srv.env = .env + DATABASE_URL/REDIS_URL on host.docker.internal, PORT=8191, ALERTS_ENABLED=true,
    #   SMTP_HOST=host.docker.internal SMTP_PORT=2525 SMTP_FROM=… RESET_URL_BASE=…,
    #   SSL_CERT_FILE=/certs/ca.pem JOBS_MAX_ATTEMPTS=1 JOBS_POLL_INTERVAL_SEC=1
    python3 scripts/acceptance-email-job-privacy.py --base http://localhost:8191/api/v1 --db <db> \\
        --expect-status failed --expect-echo      # accept mode (no --reject-rcpt): --expect-status done

Usage: python3 scripts/acceptance-email-job-privacy.py --base URL --db NAME
           [--expect-status done|failed] [--expect-echo]
Exit: 0 = ĐẠT · 1 = CHƯA ĐẠT · 2 = lỗi môi trường.
"""
import argparse, json, re, subprocess, sys, time, urllib.error, urllib.request

ap = argparse.ArgumentParser()
ap.add_argument("--base", default="http://localhost:8090/api/v1")
ap.add_argument("--db", default="nexus_platform", help="database the server runs on (sys_jobs is read there)")
ap.add_argument("--expect-status", choices=["done", "failed"], default=None,
                help="terminal status the job must reach (done = the SMTP accepted it)")
ap.add_argument("--expect-echo", action="store_true",
                help="the SMTP echoes the recipient on reject: last_error MUST say [redacted]")
ap.add_argument("--timeout", type=float, default=90, help="seconds to wait for the job to finish")
args = ap.parse_args()
BASE = args.base
if not re.fullmatch(r"[A-Za-z0-9_]+", args.db):
    sys.exit(f"LỖI MÔI TRƯỜNG: tên DB không hợp lệ {args.db!r}")

ENTITY = "crm_contacts"
FIELD = "email_ca_nhan"
# Mixed case on purpose: SMTP servers echo the address lowercased, and the mask must catch both.
PII = "Nguyen.Thi.KiemThu.CaNhan@Gmail.com"


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
    """One tab-separated result set from the server's DB (test-owned literals only)."""
    out = subprocess.run(["docker", "exec", "nexus-mysql", "mysql", "-uroot", "--default-character-set=utf8mb4",
                          "-N", "-B", args.db, "-e", query],
                         capture_output=True, text=True)
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


field_id = wf_id = record_id = None
admin = login("admin@example.com", "Demo-Admin-2026!")
rep = login("test@example.com", "Demo-Rep-2026!")
try:
    st, body = call("POST", "/admin/fields", admin, {
        "entityName": ENTITY, "name": FIELD, "label": "Email cá nhân",
        "fieldType": "text", "options": {"sensitive": True},
    })
    if st == 409:
        sys.exit(f"HỦY: {ENTITY}.{FIELD} đã tồn tại — script không đụng field nó không tạo ra.")
    assert st in (200, 201), f"add field: {st} {body}"
    st, listed = call("GET", f"/metadata/fields/{ENTITY}", admin)
    field_id = next(f["id"] for f in listed.get("data", listed) if f["name"] == FIELD)

    st, body = call("POST", "/admin/workflows", admin, {
        "name": "zz nghiệm thu riêng tư job email", "entityName": ENTITY, "trigger": "after_create",
        "actions": [{"type": "email_alert", "to": "{{" + FIELD + "}}", "subject": "Chào {{first_name}}",
                     "body": "Xin chào"}],
    })
    assert st in (200, 201), f"create workflow: {st} {body}"
    wf_id = (body.get("data") or {}).get("workflow", {}).get("id")
    assert wf_id, f"workflow id missing: {body}"

    t0 = sql("SELECT NOW(3)")[0][0]  # DB clock: created_at is written by MySQL, not by us
    st, body = call("POST", f"/{ENTITY}", rep, {"first_name": "Nguyễn Thị", "last_name": "Kiểm Thử", FIELD: PII})
    assert st in (200, 201), f"rep create: {st} {body}"
    record_id = (body.get("data") or body).get("id")
    print(f"[setup] field={field_id} workflow={wf_id} record={record_id} (tạo bởi test@example.com) t0={t0}")

    job = None
    deadline = time.time() + args.timeout
    while time.time() < deadline:
        rows = sql("SELECT id, status, attempts, IFNULL(CAST(payload AS CHAR), 'NULL'), IFNULL(last_error, '') "
                   f"FROM sys_jobs WHERE job_type = 'email_alert' AND created_at >= '{t0}' ORDER BY created_at")
        if rows and rows[-1][1] in ("done", "failed"):
            job = rows[-1]
            break
        time.sleep(0.5)
    check("có job email_alert của bản ghi vừa tạo và nó đã KẾT THÚC (done/failed)", job is not None,
          "" if job else f"chưa xong sau {args.timeout:.0f}s")
    if job:
        jid, status, attempts, payload, last_error = job
        print(f"         job={jid} status={status} attempts={attempts}")
        print(f"         payload={payload}")
        print(f"         last_error={last_error!r}")
        if args.expect_status:
            check(f"job kết thúc ở trạng thái {args.expect_status}", status == args.expect_status, f"status={status}")
        check("#95 payload của job đã kết thúc là NULL", payload == "NULL")
        # Scoped to this run: a row a pre-fix binary finished keeps its payload until the janitor
        # sweeps it (JOBS_RETENTION_DAYS) — that is history, not this build's behaviour.
        n = int(sql("SELECT COUNT(*) FROM sys_jobs WHERE LOWER(CAST(payload AS CHAR)) LIKE "
                    f"'%{PII.lower()}%' AND created_at >= '{t0}'")[0][0])
        check("#95 KHÔNG hàng sys_jobs nào của lần chạy còn giữ địa chỉ (COUNT payload LIKE địa chỉ = 0)",
              n == 0, f"COUNT = {n}")
        check("#96 last_error KHÔNG chứa địa chỉ (không phân biệt hoa-thường)", PII.lower() not in last_error.lower())
        if args.expect_echo:
            check("#96 máy chủ SMTP echo địa chỉ ⇒ last_error ghi [redacted]", "[redacted]" in last_error)
finally:
    if wf_id:
        call("DELETE", f"/admin/workflows/{wf_id}", admin)
    if record_id:
        call("DELETE", f"/{ENTITY}/{record_id}", admin)
    if field_id:
        call("DELETE", f"/admin/fields/{field_id}", admin)
        if re.fullmatch(r"[0-9a-fA-F-]{36}", field_id):
            # The API delete is soft and burns the name; this field did not exist before the run,
            # so drop exactly that one soft-deleted row (pure metadata, non-unique text field).
            sql(f"DELETE FROM sys_fields WHERE id='{field_id}' AND deleted_at IS NOT NULL")

print()
print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)} điểm)")
sys.exit(1 if fails else 0)
