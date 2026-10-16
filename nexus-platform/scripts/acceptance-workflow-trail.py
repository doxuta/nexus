#!/usr/bin/env python3
"""workflow-trail-bundle (queue #85 + #86) third-person acceptance, on a REAL server.

#85 — an email_alert whose recipient is {{email_ca_nhan}} (a field the admin flagged sensitive)
fails to send (a dev server has no mail configured, so every send fails — the exact failure this
item is about). The run-history row an admin reads at /admin/workflows/:id/runs must NOT carry the
personal address, neither in `error` nor inside `actionsResult`.

⚠️ The server MUST run with JOBS_ENABLED=false. With the durable queue on (the default) the action
only ENQUEUES the mail, the run records `success`, and the send error lands in sys_jobs instead —
so every "no PII in the run row" check is green even on the unfixed code. The `status == failed`
check is the guard against that false green; it fails loudly in queue mode.

#86 — the same rule then runs two field_update actions on the record: one on a plain field
(title) and one on the sensitive field. Each machine write must leave an audit row whose actor is
the engine's system actor, with the plain value visible and the sensitive value "[redacted]" on
both sides — and the NON-ADMIN employee who owns the record must see that row in the record's own
history (GET /audit-log/:entity/:recordId, the History tab).

Setup is done by the admin (only an admin can add a field or a rule); the record is created by
test@example.com (crm_sales_rep) so the trigger is a real employee action. Everything the run creates
is removed in finally.

Usage: python3 scripts/acceptance-workflow-trail.py [--base URL] [--db NAME]
"""
import argparse, json, re, subprocess, sys, time, urllib.error, urllib.request

ap = argparse.ArgumentParser()
ap.add_argument("--base", default="http://localhost:8090/api/v1")
ap.add_argument("--db", default="nexus_platform", help="database the server runs on (cleanup only)")
args = ap.parse_args()
BASE = args.base

ENTITY = "crm_contacts"
FIELD = "email_ca_nhan"
PII = "nguyen.thi.kiemthu.canhan@gmail.com"
MASKED_TO = "an.danh@example.com"
TITLE = "Đã gửi thư chào mừng"
SYSTEM_ACTOR = "f1000000-0000-0000-0000-000000000000"


def call(method, path, token=None, body=None):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data) as r:
            payload = r.read()
            return r.status, (json.loads(payload) if payload else {})
    except urllib.error.HTTPError as e:
        payload = e.read()
        return e.code, (json.loads(payload) if payload else {})


def login(email, password):
    st, body = call("POST", "/auth/login", body={"tenant": "northwind", "email": email, "password": password})
    assert st == 200, f"login {email}: {st} {body}"
    return body["data"]["accessToken"]


fails = []


def check(label, cond, detail=""):
    print(("  PASS  " if cond else "  FAIL  ") + label + (f"   {detail}" if detail else ""))
    if not cond:
        fails.append(label)


def poll(fn, tries=40, delay=0.25):
    """The engine and the auditor are both async by contract — poll, never race."""
    for _ in range(tries):
        out = fn()
        if out:
            return out
        time.sleep(delay)
    return None


field_id = wf_id = record_id = None
admin = login("admin@example.com", "Demo-Admin-2026!")
rep = login("test@example.com", "Demo-Rep-2026!")
try:
    # 1. Admin adds a personal-email field and flags it sensitive (Studio "Trường nhạy cảm").
    st, body = call("POST", "/admin/fields", admin, {
        "entityName": ENTITY, "name": FIELD, "label": "Email cá nhân",
        "fieldType": "text", "options": {"sensitive": True},
    })
    if st == 409:
        sys.exit(f"HỦY: {ENTITY}.{FIELD} đã tồn tại — script không đụng field nó không tạo ra.")
    assert st in (200, 201), f"add field: {st} {body}"
    st, listed = call("GET", f"/metadata/fields/{ENTITY}", admin)
    field_id = next(f["id"] for f in listed.get("data", listed) if f["name"] == FIELD)

    # 2. Admin writes the rule: mail the contact's personal address, then two machine writes.
    st, body = call("POST", "/admin/workflows", admin, {
        "name": "zz nghiệm thu dấu vết workflow", "entityName": ENTITY, "trigger": "after_create",
        "actions": [
            {"type": "email_alert", "to": "{{" + FIELD + "}}", "subject": "Chào {{first_name}}", "body": "Xin chào"},
            {"type": "field_update", "field": "title", "value": TITLE},
            {"type": "field_update", "field": FIELD, "value": MASKED_TO},
        ],
    })
    assert st in (200, 201), f"create workflow: {st} {body}"
    wf_id = (body.get("data") or {}).get("workflow", {}).get("id")
    assert wf_id, f"workflow id missing: {body}"

    # 3. The EMPLOYEE (not admin) creates the contact — a real person's action fires the rule.
    st, body = call("POST", f"/{ENTITY}", rep, {"first_name": "Nguyễn Thị", "last_name": "Kiểm Thử", FIELD: PII})
    assert st in (200, 201), f"rep create: {st} {body}"
    record_id = (body.get("data") or body).get("id")
    print(f"[setup] field={field_id} workflow={wf_id} record={record_id}")

    # 4. #85 — the run row the admin reads.
    def the_run():
        _, b = call("GET", f"/admin/workflows/{wf_id}/runs", admin)
        runs = ((b.get("data") or {}).get("runs")) or []
        return next((r for r in runs if r.get("recordId") == record_id), None)

    run = poll(the_run)
    check("#85 có đúng một lượt chạy cho bản ghi nhân viên vừa tạo", run is not None)
    if run:
        err = run.get("error") or ""
        blob = json.dumps(run, ensure_ascii=False)
        print(f"         status={run.get('status')} error={err!r}")
        check("#85 lượt chạy ghi FAILED (dev không cấu hình mail — gửi thất bại thật; server phải chạy JOBS_ENABLED=false)",
              run.get("status") == "failed", f"status={run.get('status')}")
        check("#85 cột error KHÔNG chứa email cá nhân", PII not in err)
        check("#85 actionsResult KHÔNG chứa email cá nhân", PII not in json.dumps(run.get("actionsResult"), ensure_ascii=False))
        check("#85 không chỗ nào của hàng run chứa email cá nhân", PII not in blob)
        if run.get("status") == "failed":
            check("#85 lỗi vẫn nói được gửi theo mẫu nào (admin còn sửa được rule)", "{{" + FIELD + "}}" in err, err)

    # 5. #86 — the machine writes leave audit rows, attributed to the engine.
    def machine_rows():
        _, b = call("GET", f"/audit-log?entityName={ENTITY}&recordId={record_id}&action=update", admin)
        rows = b.get("data") if isinstance(b, dict) else b
        if isinstance(rows, dict):
            rows = rows.get("items", [])
        rows = [r for r in (rows or []) if r.get("actorUserId") == SYSTEM_ACTOR]
        return rows if len(rows) >= 2 else None

    rows = poll(machine_rows) or []
    check("#86 mỗi field_update do máy chạy để lại một hàng nhật ký 'update'", len(rows) == 2, f"{len(rows)} hàng")
    diffs = []
    for r in rows:
        d = r.get("diff") or {}
        diffs.append(json.loads(d) if isinstance(d, str) else d)
    title_diff = next((d for d in diffs if "title" in (d.get("after") or {})), None)
    pii_diff = next((d for d in diffs if FIELD in (d.get("after") or {})), None)
    check("#86 hàng của field thường mang giá trị mới thật", title_diff is not None and title_diff["after"]["title"] == TITLE,
          json.dumps(title_diff, ensure_ascii=False))
    check("#86 hàng của field nhạy cảm che CẢ HAI phía",
          pii_diff is not None and (pii_diff.get("before") or {}).get(FIELD) == "[redacted]"
          and pii_diff["after"][FIELD] == "[redacted]", json.dumps(pii_diff, ensure_ascii=False))
    check("#86 không hàng nhật ký nào chứa email cá nhân", PII not in json.dumps(diffs, ensure_ascii=False))

    # 6. #86 — the non-admin owner sees who changed his record, on the History tab endpoint.
    def rep_history():
        st, b = call("GET", f"/audit-log/{ENTITY}/{record_id}", rep)
        rows = b.get("data") if isinstance(b, dict) else b
        if isinstance(rows, dict):
            rows = rows.get("items", [])
        return (st, rows or [])

    st, hist = rep_history()
    sys_rows = [r for r in hist if r.get("actorUserId") == SYSTEM_ACTOR]
    check("#86 nhân viên (không admin) mở Lịch sử bản ghi của mình thấy thay đổi do quy tắc tự động",
          st == 200 and len(sys_rows) == 2, f"HTTP {st}, {len(sys_rows)} hàng của hệ thống")
finally:
    if wf_id:
        call("DELETE", f"/admin/workflows/{wf_id}", admin)
    if record_id:
        call("DELETE", f"/{ENTITY}/{record_id}", admin)
    if field_id:
        call("DELETE", f"/admin/fields/{field_id}", admin)
        if re.fullmatch(r"[0-9a-fA-F-]{36}", field_id) and re.fullmatch(r"[A-Za-z0-9_]+", args.db):
            # The API delete is soft and burns the name; this field did not exist before the run,
            # so drop exactly that one soft-deleted row (pure metadata, non-unique text field).
            subprocess.run(["docker", "exec", "nexus-mysql", "mysql", "-uroot", args.db, "-e",
                            f"DELETE FROM sys_fields WHERE id='{field_id}' AND deleted_at IS NOT NULL"],
                           check=False)

print()
print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)} điểm)")
sys.exit(1 if fails else 0)
