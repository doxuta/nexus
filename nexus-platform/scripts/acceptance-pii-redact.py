#!/usr/bin/env python3
"""pii-sensitive-audit-redact (#19) third-person acceptance.

An ADMIN flags a field sensitive (what Studio's "Trường nhạy cảm" toggle writes).
A NON-ADMIN employee (test@example.com, crm_sales_rep) creates a record and then CHANGES the
sensitive value — an update, so the audit diff has BOTH sides. The employee's own detail view
must still show the real value. The admin then reads that audit row on the API the /admin/audit
screen uses, and in the CSV export, and must see [redacted] on both sides.

Everything it creates is cleaned up in finally: the field is soft-deleted, the record deleted.
"""
import json, re, sys, time, subprocess, urllib.request, urllib.error

BASE = "http://localhost:8090/api/v1"
ENTITY = "crm_contacts"
FIELD = "so_cccd"
OLD, NEW = "001099001122", "079203004455"

def call(method, path, token=None, body=None, raw=False):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data) as r:
            payload = r.read()
            return r.status, (payload.decode() if raw else (json.loads(payload) if payload else {}))
    except urllib.error.HTTPError as e:
        payload = e.read()
        return e.code, (payload.decode() if raw else (json.loads(payload) if payload else {}))

def login(email, password):
    st, body = call("POST", "/auth/login", body={"tenant": "northwind", "email": email, "password": password})
    assert st == 200, f"login {email}: {st} {body}"
    return body["data"]["accessToken"]

fails = []
def check(label, cond, detail=""):
    print(("  PASS  " if cond else "  FAIL  ") + label + (f"   {detail}" if detail else ""))
    if not cond:
        fails.append(label)

field_id = record_id = doomed_id = None
admin = login("admin@example.com", "Demo-Admin-2026!")
rep = login("test@example.com", "Demo-Rep-2026!")
try:
    # 1. Admin flags the field sensitive — the exact payload Studio's toggle sends.
    st, body = call("POST", "/admin/fields", admin, {
        "entityName": ENTITY, "name": FIELD, "label": "Số CCCD",
        "fieldType": "text", "options": {"sensitive": True},
    })
    if st == 409:
        # The name already exists on this entity. It is NOT ours, so refuse rather than reuse:
        # the cleanup below hard-deletes the row it created, and reusing someone else's field
        # here would destroy a configuration this run did not make.
        sys.exit(f"HỦY: {ENTITY}.{FIELD} đã tồn tại (có thể do lần chạy trước xoá mềm). "
                 f"Kiểm tra sys_fields rồi chạy lại — script không đụng field nó không tạo ra.")
    assert st in (200, 201), f"add field: {st} {body}"
    payload = body.get("data", body)
    if isinstance(payload, list):
        field_id = next(f["id"] for f in payload if f["name"] == FIELD)
    else:
        field_id = payload.get("id")
    if not field_id:
        st, listed = call("GET", f"/metadata/fields/{ENTITY}", admin)
        field_id = next(f["id"] for f in listed.get("data", listed) if f["name"] == FIELD)
    print(f"[setup] field {FIELD} created sensitive=true (id={field_id})")

    # 2. NON-ADMIN employee creates a record (create:all ⇒ they own it) ...
    st, body = call("POST", f"/{ENTITY}", rep, {"first_name": "Nguyễn Thị", "last_name": "Kiểm Thử", FIELD: OLD})
    assert st in (200, 201), f"rep create: {st} {body}"
    record_id = (body.get("data") or body).get("id")

    # ... then CHANGES the sensitive value. An update is what produces a two-sided diff.
    st, body = call("PUT", f"/{ENTITY}/{record_id}", rep, {FIELD: NEW})
    check("nhân viên (không admin) sửa được số CCCD của bản ghi mình tạo", st in (200, 204), f"HTTP {st}")

    # 3. The employee's own detail view still shows the REAL value.
    st, body = call("GET", f"/{ENTITY}/{record_id}", rep)
    seen = ((body.get("data") or body).get("data") or {}).get(FIELD)
    check("màn chi tiết của chính nhân viên vẫn hiện giá trị thật", seen == NEW, f"đọc được {seen!r}")

    # 4. Admin reads that audit row on the API /admin/audit uses.
    # The auditor is async by contract (fail-open), so poll briefly rather than racing it.
    row, rows = None, []
    for _ in range(20):
        st, body = call("GET", f"/audit-log?entityName={ENTITY}&recordId={record_id}&action=update", admin)
        rows = body.get("data") if isinstance(body, dict) else body
        if isinstance(rows, dict):
            rows = rows.get("items", [])
        rows = rows or []
        row = next((r for r in rows if r.get("action") == "update"), None)
        if row:
            break
        time.sleep(0.25)
    check("có đúng hàng nhật ký 'update' cho bản ghi đó", row is not None, f"HTTP {st}, {len(rows)} hàng")
    if row:
        diff = row.get("diff") or {}
        if isinstance(diff, str):
            diff = json.loads(diff)
        before = (diff.get("before") or {}).get(FIELD)
        after = (diff.get("after") or {}).get(FIELD)
        check("nhật ký che giá trị CŨ", before == "[redacted]", f"before={before!r}")
        check("nhật ký che giá trị MỚI", after == "[redacted]", f"after={after!r}")
        check("khoá so_cccd VẪN CÒN trong diff (còn dấu vết ai đổi, lúc nào)",
              FIELD in (diff.get("before") or {}) and FIELD in (diff.get("after") or {}))
        blob = json.dumps(diff, ensure_ascii=False)
        check("không có giá trị thật nào lọt vào hàng nhật ký", OLD not in blob and NEW not in blob)

    # 5. Same row through the CSV export an admin downloads.
    st, text = call("GET", f"/audit-log/export.csv?entityName={ENTITY}&recordId={record_id}", admin, raw=True)
    check("CSV xuất ra không chứa số CCCD thật", st == 200 and OLD not in text and NEW not in text, f"HTTP {st}")
    check("CSV xuất ra có chứa dấu [redacted]", "[redacted]" in text)

    # 6. The Studio inspector's wire contract. `options` is a FULL REPLACEMENT on PUT, so the
    #    exact objects withSensitive() builds must (a) turn the flag on without dropping a
    #    sibling option, and (b) turn it OFF again — an omitted key inside a present payload.
    def stored_options():
        _, listed = call("GET", f"/metadata/fields/{ENTITY}", admin)
        rows = listed.get("data", listed)
        return next(f for f in rows if f["name"] == FIELD).get("options") or {}

    call("PUT", f"/admin/fields/{field_id}", admin, {"options": {"maxLength": 12, "sensitive": True}})
    o = stored_options()
    check("bật cờ nhạy cảm không làm mất tuỳ chọn khác của field",
          o.get("sensitive") is True and o.get("maxLength") == 12, f"options={o}")
    call("PUT", f"/admin/fields/{field_id}", admin, {"options": {"maxLength": 12}})
    o = stored_options()
    check("tắt cờ nhạy cảm thực sự xoá được cờ (không dính vĩnh viễn)",
          "sensitive" not in o and o.get("maxLength") == 12, f"options={o}")

    # 7. The hole the pre-commit security audit found, proven against the REAL server — which is
    #    the only thing that shows production takes the complete path. Deleting a FIELD is a SOFT
    #    delete: the definition goes, the values stay in obj_*.data. A record delete then diffs
    #    that raw blob, and the live field list no longer mentions so_cccd — so the flag has to be
    #    read from the including-deleted list, through MetaCache, or every real CCCD in the table
    #    lands in the append-only trail on a bulk delete.
    call("PUT", f"/admin/fields/{field_id}", admin, {"options": {"sensitive": True}})
    st, body = call("POST", f"/{ENTITY}", rep,
                    {"first_name": "Lê Văn", "last_name": "Xoá Field", FIELD: OLD})
    assert st in (200, 201), f"rep create #2: {st} {body}"
    doomed_id = (body.get("data") or body).get("id")

    st, _ = call("DELETE", f"/admin/fields/{field_id}", admin)   # field soft-deleted
    check("admin xoá được field khỏi app (xoá mềm, dữ liệu cũ vẫn nằm trong bản ghi)", st in (200, 204), f"HTTP {st}")

    # Deleted by the ADMIN on purpose: crm_sales_rep has no `delete` grant on this entity, and
    # the routine forbids editing a shared role to make a test pass. That costs nothing here —
    # redaction is not an access-control check, so WHO deletes cannot change which bytes get
    # written. The access-control half of this slice is already proven above, by the non-admin's
    # own update. (The same reasoning the queue item gives for #19: an admin cannot prove or
    # disprove ACL, but nothing an admin does changes what the writer wrote.)
    st, _ = call("DELETE", f"/{ENTITY}/{doomed_id}", admin)
    assert st in (200, 204), f"admin delete: {st}"

    drow = None
    for _ in range(20):
        st, body = call("GET", f"/audit-log?entityName={ENTITY}&recordId={doomed_id}&action=delete", admin)
        rws = body.get("data") if isinstance(body, dict) else body
        if isinstance(rws, dict):
            rws = rws.get("items", [])
        drow = next((r for r in (rws or []) if r.get("action") == "delete"), None)
        if drow:
            break
        time.sleep(0.25)
    check("có hàng nhật ký 'delete' cho bản ghi đó", drow is not None)
    if drow:
        ddiff = drow.get("diff") or {}
        if isinstance(ddiff, str):
            ddiff = json.loads(ddiff)
        dbefore = (ddiff.get("before") or {}).get(FIELD)
        check("field ĐÃ BỊ XOÁ MỀM vẫn được che khi xoá bản ghi", dbefore == "[redacted]",
              f"before={dbefore!r}")
        check("không có số CCCD thật nào trong hàng nhật ký xoá",
              OLD not in json.dumps(ddiff, ensure_ascii=False))

    # 8. The security flag must not be settable in a shape that silently fails open.
    st, _ = call("POST", "/admin/fields", admin, {
        "entityName": ENTITY, "name": "zz_sensitive_typecheck", "label": "probe",
        "fieldType": "text", "options": {"sensitive": "true"},
    })
    check("cờ nhạy cảm sai kiểu bị từ chối ngay lúc khai báo (không fail-open im lặng)",
          st == 422, f"HTTP {st}")
finally:
    if record_id:
        call("DELETE", f"/{ENTITY}/{record_id}", rep)
    if doomed_id:
        call("DELETE", f"/{ENTITY}/{doomed_id}", admin)
    if field_id:
        # UUID-shaped or we do not interpolate it into SQL at all.
        if not re.fullmatch(r"[0-9a-fA-F-]{36}", field_id):
            sys.exit(f"HỦY dọn dẹp: field id không đúng dạng UUID ({field_id!r})")
        st, _ = call("DELETE", f"/admin/fields/{field_id}", admin)  # already soft-deleted by §7 → 404 is fine
        # The API soft-deletes, which permanently burns the name for this tenant (a second
        # create returns FIELD_ALREADY_EXISTS). This field did not exist before the run, so the
        # only way to leave the dev tenant exactly as found is to drop that one row by id.
        # Non-unique text field ⇒ pure metadata, no obj_* column was ever created.
        subprocess.run(["docker", "exec", "nexus-mysql", "mysql", "-uroot", "nexus_platform",
                        # `deleted_at IS NOT NULL` makes the statement structurally incapable of
                        # removing a LIVE field row — it can only ever drop what the line above
                        # just soft-deleted, on whatever database someone later points this at.
                        "-e", f"DELETE FROM sys_fields WHERE id = '{field_id}' AND name = '{FIELD}'"
                              " AND deleted_at IS NOT NULL;"],
                       # NOT check=True: this runs in `finally`, and a docker hiccup here must not
                       # replace the acceptance verdict with a CalledProcessError traceback.
                       check=False)
        print(f"[cleanup] field removed (soft-delete HTTP {st} + row dropped), record deleted")

print()
print("KẾT QUẢ:", "ĐẠT" if not fails else f"KHÔNG ĐẠT — {fails}")
sys.exit(0 if not fails else 1)
