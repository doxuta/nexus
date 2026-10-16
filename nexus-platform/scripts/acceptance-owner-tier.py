#!/usr/bin/env python3
"""Tầng chủ khách (specs/065-customer-owner-tier) — kịch bản nghiệm thu người-thứ-ba, trên server THẬT.

Mỗi lát của specs/065 có một nhóm kiểm (`--slice`); lát nào land thì thêm hàm `slice_<X>` của mình vào
bảng SLICES (tasks.md T003 + "Nghiệm thu --slice X" của lát đó). Hiện có:

Nhóm A (`user-admin-fixes`) — màn Người dùng của quản trị phải nói thật, kiểm bằng API như màn gọi:
  1. quản trị (admin@example.com) tạo một nhân viên MỚI (không-admin, mang một vai CRM) rồi đọc dòng đó;
  2. PUT ĐÚNG payload form cũ gửi — họ tên mới + isAdmin, roleId, status y như đang lưu ⇒ phiên gia hạn
     của nhân viên vẫn sống và KHÔNG có dòng cảnh báo `role_changed` mới (bản cũ: thu hồi + E2);
  3. đối chứng dương: đổi vai thật (bỏ vai `roleId:""`) ⇒ vai mất khi đọc lại, phiên gia hạn bị từ chối, CÓ
     dòng `role_changed` (không có đối chứng này, "không có cảnh báo" có thể chỉ là cảnh báo bị tắt);
  4. PUT status `xyz` ⇒ 422 `USER_STATUS_INVALID`, trạng thái không đổi;
  5. admin liệt kê phiên của nhân viên (2 lần đăng nhập) rồi kết thúc MỘT phiên ⇒ thiết bị đó 401 ở request
     kế tiếp, thiết bị kia vẫn dùng được.
Cần `--db nexus_wt_<slug>` (đọc `security_alerts`) và server chạy với `ALERTS_ENABLED=true SMTP_HOST=127.0.0.1
SMTP_PORT=<cổng đóng>` (thư gửi hỏng nhưng hàng kiểm toán vẫn được ghi — đó là thứ script đếm).

Nhóm B (`owner-tier-core`) — chủ khách (không-admin) quản người dùng trong app, kiểm bằng người thứ ba:
  1. người dựng (admin@example.com) tạo chủ khách + nhân viên (vai CRM có sẵn), bật `user-manager` cho chủ khách;
  2. ma trận 403: chủ khách gọi ≥ 1 route mỗi nhóm `/admin/*` (+ /audit-log) ⇒ 403 byte-identical;
  3. `/org-admin/users` 200 thấy cả ba tầng, `manageable` đúng; `/org-admin/roles` không lộ grant;
  4. nhân viên gọi `/org-admin/users` ⇒ 403; đổi vai ⇒ nhân viên mất quyền xem entity của vai cũ;
  5. khoá ⇒ nhân viên 401, mở ⇒ đăng nhập lại được; PUT lên quản trị ⇒ 403 USER_NOT_MANAGEABLE;
  6. PUT kèm isAdmin/canManageUsers/password ⇒ bị bỏ qua; người dựng tắt cờ ⇒ `/org-admin` 403 ngay.
Nhóm B2 (`operator-account-guard`) — quản trị không-vận-hành không đụng tài khoản vận hành. Cần `--db nexus_wt_<slug>`:
cờ vận hành chỉ cấp được ngoài API, nên script bật nó bằng SQL lên một tài khoản `@owner-acceptance.invalid`
trên DB bản sao (KHÔNG nhắm admin@example.com). Probe không phá trước (GET phiên, PUT họ tên giữ nguyên); chỉ khi
chúng đã 403 mới thử reset-link / kết thúc phiên / xoá — trên bản chưa sửa, dừng ở probe đầu tiên đỏ.

Nhóm D (`owner-tier-conversion`) và D2 (`owner-tier-provision-default`) — theo từng KHÔNG GIAN: người vận hành
(admin@example.com) tạo một không gian thử (POST /admin/tenants — không có đường xoá ⇒ bắt buộc `--db nexus_wt_*`),
cài gói CRM vào đó, rồi:
  D:  đưa về dạng cũ (admin không vai) ⇒ chuyển hai lần (lần hai 0) ⇒ chủ khách /admin/roles 403, /org-admin 200,
      thêm + mời nhân viên mang vai của gói, nhân viên đọc bảng ⇒ trả về hai lần ⇒ số đếm như trước; tự chuyển
      xưởng mình ⇒ 422; quản trị khách (không vận hành) ⇒ 403; phiên cũ bị thu hồi.
  D2: không gian mới có tài khoản đầu là chủ khách (phản hồi + ListTenants + /auth/me) và dùng được cả nhà;
      SC-011: acceptance-acl.py --admin-token-env chạy được (exit 0/1, không 2).

An toàn khi chạy:
  - `--base` phải là loopback (script GHI vào tổ chức nó đăng nhập); `--db` phải mang tiền tố `nexus_wt_`.
  - Tài khoản tạo ra mang đuôi `@owner-acceptance.invalid` (cố ý KHÁC `@acceptance.invalid` của
    acceptance-acl.py để bước quét của nó không chạm vào), bị xoá trong `finally`, kể cả khi nhận
    SIGTERM/SIGHUP (bài học audit #78). Đầu mỗi lần chạy quét tự lành mọi tài khoản còn sống mang đuôi đó.
  - Không in token / mật khẩu.
  - Dọn thất bại ⇒ exit 1.
Mã thoát: 0 = đạt · 1 = chưa đạt (hoặc dọn chưa sạch) · 2 = lỗi môi trường / nhóm chưa viết.

Mật khẩu: `NEXUS_ADMIN_PASS` (mặc định mật khẩu demo của tenant dev `northwind`).

Dùng (từ nexus-platform/):
    ALERTS_ENABLED=true SMTP_HOST=127.0.0.1 SMTP_PORT=1 PORT=8432 ./bin/server   # DATABASE_URL → bản sao
    python3 scripts/acceptance-owner-tier.py --slice A --base http://localhost:8432/api/v1 \
        --db nexus_wt_user_admin_fixes

ponytail: stdlib thuần — tiền lệ acceptance-acl.py / acceptance-named-credentials.py.
"""

import argparse
import base64
import ipaddress
import json
import os
import re
import secrets
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

DOMAIN = "@owner-acceptance.invalid"
STAFF_PASS = "Nv-Demo-Accept-2026!x"
EMAIL_RE = re.compile(r"^[a-z0-9.\-]+@owner-acceptance\.invalid$")
UUID_RE = re.compile(r"^[0-9a-f\-]{36}$")

failures = []
created = []  # user ids this run made (cleanup list)


def check(ok, label, detail=""):
    print(f"  {'OK  ' if ok else 'ĐỎ  '} {label}" + (f" — {detail}" if detail and not ok else ""))
    if not ok:
        failures.append(label)
    return ok


def env_fail(msg):
    print(f"LỖI MÔI TRƯỜNG: {msg}", file=sys.stderr)
    sys.exit(2)


# --------------------------------------------------------------------------------------------
# HTTP + DB helpers
# --------------------------------------------------------------------------------------------

def req(method, url, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    r = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=15) as resp:
            raw = resp.read()
            try:
                return resp.status, (json.loads(raw) if raw else {})
            except ValueError:  # a non-JSON body (e.g. a CSV export): the status is what the caller reads
                return resp.status, {}
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.load(e)
        except Exception:
            return e.code, {}
    except Exception as e:
        env_fail(f"không gọi được {url} — {e}")


def login(base, tenant, email, password):
    code, body = req("POST", base + "/auth/login", body={"tenant": tenant, "email": email, "password": password})
    data = body.get("data") or {}
    if code != 200 or not data.get("accessToken"):
        env_fail(f"đăng nhập {email} thất bại ({code} {body.get('code', '')})")
    return data


def jwt_sid(token):
    seg = token.split(".")[1]
    seg += "=" * (-len(seg) % 4)
    return json.loads(base64.urlsafe_b64decode(seg)).get("sid", "")


def db_scalar(db, sql):
    out = subprocess.run(
        ["docker", "exec", "nexus-mysql", "mysql", "-uroot", "-N", "-B", db, "-e", sql],
        capture_output=True, text=True, timeout=30,
    )
    if out.returncode != 0:
        env_fail(f"đọc DB {db} thất bại: {out.stderr.strip()[:200]}")
    return out.stdout.strip()


def alert_count(db, user_id, alert_type):
    if not UUID_RE.match(user_id) or not re.match(r"^[a-z_]+$", alert_type):
        env_fail("id/loại cảnh báo sai dạng")
    # ids/types validated above; this is a one-shot script against a throwaway clone DB
    return int(db_scalar(db, f"SELECT COUNT(*) FROM security_alerts WHERE user_id='{user_id}' AND alert_type='{alert_type}'") or 0)


def wait_alert_count(db, user_id, alert_type, want_at_least, secs=5):
    """The dispatcher is async: poll a little for the row to land."""
    end = time.time() + secs
    n = alert_count(db, user_id, alert_type)
    while n < want_at_least and time.time() < end:
        time.sleep(0.3)
        n = alert_count(db, user_id, alert_type)
    return n


# --------------------------------------------------------------------------------------------
# Cleanup (finally + signals + self-healing sweep)
# --------------------------------------------------------------------------------------------

def sweep(base, admin_token):
    """Delete every live account carrying our domain (a previous run killed with -9)."""
    code, body = req("GET", base + "/admin/users", admin_token)
    if code != 200:
        return 0
    n = 0
    for u in (body.get("data") or {}).get("users") or []:
        if str(u.get("email", "")).endswith(DOMAIN):
            req("DELETE", f"{base}/admin/users/{u['id']}", admin_token)
            n += 1
    return n


def cleanup(base, admin_token):
    left = 0
    for uid in list(created):
        code, _ = req("DELETE", f"{base}/admin/users/{uid}", admin_token)
        if code not in (204, 404):
            left += 1
        else:
            created.remove(uid)
    return left


# --------------------------------------------------------------------------------------------
# Slice A
# --------------------------------------------------------------------------------------------

def slice_a(base, tenant, admin_pass, db):
    admin = login(base, tenant, "admin@example.com", admin_pass)
    at = admin["accessToken"]

    def guarded_cleanup(*_):
        cleanup(base, at)
        sys.exit(130)
    signal.signal(signal.SIGTERM, guarded_cleanup)
    signal.signal(signal.SIGHUP, guarded_cleanup)

    try:
        sweep(base, at)
        code, body = req("GET", base + "/admin/roles", at)
        roles = (body.get("data") or {}).get("roles") or []
        role = next((r for r in roles if r.get("name") == "crm_sales_rep"), None) or next((r for r in roles if r.get("name") != "*"), None)
        if code != 200 or not role:
            env_fail("không có vai nào để gán cho nhân viên thử")
        email = f"nv-{int(time.time())}-{secrets.token_hex(3)}{DOMAIN}"
        if not EMAIL_RE.match(email):
            env_fail("email thử sai dạng")
        code, body = req("POST", base + "/admin/users", at, {
            "email": email, "password": STAFF_PASS, "fullName": "Nhân viên nghiệm thu A",
            "status": "active", "isAdmin": False, "roleId": role["id"],
        })
        if code != 201:
            env_fail(f"tạo nhân viên thử thất bại ({code} {body.get('code', '')})")
        uid = body["data"]["user"]["id"]
        created.append(uid)

        def read_user():
            c, b = req("GET", f"{base}/admin/users/{uid}", at)
            return (b.get("data") or {}).get("user") or {}

        def refresh_ok(refresh_token):
            c, _ = req("POST", base + "/auth/refresh", body={"refreshToken": refresh_token})
            return c

        print("A1 — gửi lại payload form cũ (họ tên mới + isAdmin/roleId/status y như đang lưu)")
        stored = read_user()
        s1 = login(base, tenant, email, STAFF_PASS)
        e2_before = alert_count(db, uid, "role_changed")
        code, body = req("PUT", f"{base}/admin/users/{uid}", at, {
            "fullName": "Nhân viên đổi tên", "isAdmin": stored.get("isAdmin", False),
            "roleId": stored.get("roleId"), "status": stored.get("status"),
        })
        check(code == 200, "PUT form cũ ⇒ 200", f"{code} {body.get('code', '')}")
        time.sleep(1.5)  # async dispatcher: let a (wrongly) enqueued alert land before counting
        check(refresh_ok(s1["refreshToken"]) == 200, "phiên gia hạn của nhân viên vẫn sống sau khi chỉ đổi tên")
        check(alert_count(db, uid, "role_changed") == e2_before, "không có cảnh báo role_changed mới",
              f"{e2_before} → {alert_count(db, uid, 'role_changed')}")

        print("A2 — đối chứng dương: bỏ vai thật")
        s2 = login(base, tenant, email, STAFF_PASS)
        code, body = req("PUT", f"{base}/admin/users/{uid}", at, {"roleId": ""})
        check(code == 200, "PUT roleId:\"\" ⇒ 200", f"{code} {body.get('code', '')}")
        check(not read_user().get("roleId"), "đọc lại: nhân viên KHÔNG còn vai")
        check(refresh_ok(s2["refreshToken"]) == 401, "phiên gia hạn bị thu hồi sau khi đổi vai thật")
        n = wait_alert_count(db, uid, "role_changed", e2_before + 1)
        if n == e2_before:
            env_fail("đổi vai thật mà không có dòng security_alerts nào — server chưa chạy ALERTS_ENABLED=true? "
                     "(không có đối chứng này, kết luận 'không cảnh báo' ở A1 vô giá trị)")
        check(n > e2_before, "đổi vai thật CÓ cảnh báo role_changed (đối chứng)")
        code, _ = req("PUT", f"{base}/admin/users/{uid}", at, {"roleId": role["id"]})
        check(code == 200 and read_user().get("roleId") == role["id"], "gán lại vai ⇒ đọc lại thấy vai")

        print("A3 — trạng thái ngoài tập cho phép")
        code, body = req("PUT", f"{base}/admin/users/{uid}", at, {"status": "xyz"})
        check(code == 422 and body.get("code") == "USER_STATUS_INVALID", "PUT status xyz ⇒ 422 USER_STATUS_INVALID",
              f"{code} {body.get('code', '')}")
        check(read_user().get("status") == "active", "trạng thái không đổi")

        print("A4 — admin xem và đăng xuất MỘT phiên của nhân viên")
        a = login(base, tenant, email, STAFF_PASS)
        b = login(base, tenant, email, STAFF_PASS)
        sid_a = jwt_sid(a["accessToken"])
        code, body = req("GET", f"{base}/admin/users/{uid}/sessions", at)
        ids = [s.get("id") for s in (body.get("data") or [])]
        check(code == 200 and sid_a in ids and jwt_sid(b["accessToken"]) in ids, "liệt kê thấy cả hai phiên đang đăng nhập")
        code, _ = req("DELETE", f"{base}/admin/users/{uid}/sessions/{sid_a}", at)
        check(code == 204, "DELETE phiên A ⇒ 204", str(code))
        check(req("GET", base + "/auth/me", a["accessToken"])[0] == 401, "thiết bị A bị 401 ở request kế tiếp")
        check(req("GET", base + "/auth/me", b["accessToken"])[0] == 200, "thiết bị B vẫn dùng được")
    finally:
        left = cleanup(base, at)
        if left:
            failures.append(f"dọn chưa sạch ({left} tài khoản)")
            print(f"  ĐỎ   dọn chưa sạch: còn {left} tài khoản {DOMAIN}")


# --------------------------------------------------------------------------------------------
# Slice B / B2 helpers
# --------------------------------------------------------------------------------------------

FORBIDDEN_BODY = {"code": "FORBIDDEN", "error": "Yêu cầu quyền quản trị viên"}
# One REAL route per /admin group of specs/034 §2 (builder AND tenant-admin) plus the two audit surfaces that
# live outside the prefix. Each must be a registered route: a path nobody registered falls into the
# /:entity dispatcher and answers a *different* 403 (the ACL one) - exactly the trap this list must not hide
# behind, so every probe is first proven live with the builder's own token (anything but 404).
ADMIN_PROBES = [
    ("POST", "/admin/entities"), ("GET", "/admin/verticals/builtin"), ("GET", "/admin/apps"),
    ("GET", "/admin/api-keys"), ("GET", "/admin/ai/builder/sessions"), ("GET", "/admin/publish/state"),
    ("GET", "/admin/users"), ("GET", "/admin/roles"), ("GET", "/admin/teams"), ("GET", "/admin/team-sets"),
    ("GET", "/admin/field-acl"), ("GET", "/admin/sso/providers"), ("GET", "/admin/business-calendars"),
    ("GET", "/admin/jobs"), ("GET", "/admin/email-log"), ("GET", "/admin/usage"), ("GET", "/admin/health"),
    ("GET", "/admin/credentials"), ("GET", "/admin/work-queues"), ("GET", "/admin/inbound-hooks"),
    ("GET", "/admin/workflows"), ("GET", "/admin/approval-processes"), ("GET", "/admin/reports"),
    ("GET", "/admin/dashboards"), ("GET", "/admin/tenant/branding"), ("GET", "/admin/tenant/payment"),
    ("GET", "/audit-log"), ("GET", "/audit-log/export.csv"),
]


def make_user(base, at, label, is_admin=False, role_id=None):
    """Create a fixture account (cleanup list entry) and return (id, email)."""
    email = f"{label}-{int(time.time())}-{secrets.token_hex(3)}{DOMAIN}"
    if not EMAIL_RE.match(email):
        env_fail("email thử sai dạng")
    body = {"email": email, "password": STAFF_PASS, "fullName": f"Nghiệm thu {label}", "status": "active", "isAdmin": is_admin}
    if role_id:
        body["roleId"] = role_id
    code, resp = req("POST", base + "/admin/users", at, body)
    if code != 201:
        env_fail(f"tạo {label} thất bại ({code} {resp.get('code', '')})")
    uid = resp["data"]["user"]["id"]
    created.append(uid)
    return uid, email


def pick_role(base, at):
    code, body = req("GET", base + "/admin/roles", at)
    roles = (body.get("data") or {}).get("roles") or []
    role = next((r for r in roles if r.get("name") == "crm_sales_rep"), None) or next((r for r in roles if r.get("name") != "*"), None)
    if code != 200 or not role:
        env_fail("không có vai nào để gán cho nhân viên thử")
    return role


def guarded(base, at):
    def handler(*_):
        cleanup(base, at)
        sys.exit(130)
    signal.signal(signal.SIGTERM, handler)
    signal.signal(signal.SIGHUP, handler)


# --------------------------------------------------------------------------------------------
# Slice B
# --------------------------------------------------------------------------------------------

def slice_b(base, tenant, admin_pass, db):
    admin = login(base, tenant, "admin@example.com", admin_pass)
    at = admin["accessToken"]
    guarded(base, at)
    try:
        sweep(base, at)
        role = pick_role(base, at)
        # An entity the role can view, to prove a role change really changes what the staff member reads.
        code, body = req("GET", f"{base}/admin/roles/{role['id']}", at)
        grants = ((body.get("data") or {}).get("role") or {}).get("actions") or []
        entity = next((g["entityName"] for g in grants if g.get("action") == "view" and g.get("entityName") != "*"), None)

        owner_id, owner_email = make_user(base, at, "chukhach")
        staff_id, staff_email = make_user(base, at, "nhanvien", role_id=role["id"])
        # A plain admin (NOT an operator): admin@example.com is an operator, which would be refused for that reason
        # alone and so could not prove the is_admin clause of the manageable rule.
        boss_id, _ = make_user(base, at, "quantri", is_admin=True)

        print("B1 — người dựng bật 'Chủ khách' (PUT /admin/users/:id/user-manager)")
        code, body = req("PUT", f"{base}/admin/users/{owner_id}/user-manager", at, {"enabled": True})
        if not check(code == 200 and (body.get("data") or {}).get("user", {}).get("canManageUsers") is True,
                     "PUT user-manager {enabled:true} ⇒ 200 canManageUsers", f"{code} {body.get('code', '')}"):
            return
        o = login(base, tenant, owner_email, STAFF_PASS)
        ot = o["accessToken"]
        code, me = req("GET", base + "/auth/me", ot)
        check(code == 200 and (me.get("data") or me).get("canManageUsers") is True and (me.get("data") or me).get("isAdmin") is False,
              "chủ khách: /auth/me canManageUsers=true, isAdmin=false")

        print("B2 — chủ khách KHÔNG vào được /admin/* (mỗi nhóm một route)")
        for method, path in ADMIN_PROBES:
            live, _ = req(method, base + path, at, {} if method != "GET" else None)
            if live in (404, 405):
                check(False, f"{method} {path} là route thật (builder gọi được)", f"builder nhận {live}")
                continue
            code, body = req(method, base + path, ot, {} if method != "GET" else None)
            check(code == 403 and body == FORBIDDEN_BODY, f"owner {method} {path} ⇒ 403 byte-identical", f"{code} {body}")

        print("B3 — /org-admin/users, /org-admin/roles")
        code, body = req("GET", base + "/org-admin/users", ot)
        users = (body.get("data") or {}).get("users") or []
        by = {u["id"]: u for u in users}
        check(code == 200 and staff_id in by and owner_id in by, "GET /org-admin/users ⇒ 200, thấy mọi tài khoản", f"{code}")
        check(by.get(staff_id, {}).get("manageable") is True and by.get(staff_id, {}).get("tier") == "staff", "nhân viên: tier staff, manageable")
        check(by.get(owner_id, {}).get("manageable") is False and by.get(owner_id, {}).get("tier") == "owner", "chính chủ khách: tier owner, chỉ đọc")
        admin_row = next((u for u in users if u.get("tier") == "admin"), None)
        check(admin_row is not None and admin_row.get("manageable") is False, "có tài khoản tier admin và nó chỉ đọc")
        check(not any(k in json.dumps(users) for k in ("tenantId", "isPlatformOperator", "passwordHash")), "danh sách không lộ tenantId/cờ vận hành/mật khẩu")
        code, body = req("GET", base + "/org-admin/roles", ot)
        roles = (body.get("data") or {}).get("roles") or []
        check(code == 200 and any(r["id"] == role["id"] and r["assignable"] for r in roles), "GET /org-admin/roles ⇒ 200, vai CRM gán được")
        check("actions" not in json.dumps(roles), "danh sách vai không lộ grant")

        print("B4 — nhân viên không vào được /org-admin; đổi vai đổi quyền thật")
        s = login(base, tenant, staff_email, STAFF_PASS)
        st = s["accessToken"]
        code, body = req("GET", base + "/org-admin/users", st)
        check(code == 403 and body == FORBIDDEN_BODY, "nhân viên GET /org-admin/users ⇒ 403 byte-identical", f"{code}")
        if entity:
            check(req("GET", f"{base}/{entity}?limit=1", st)[0] == 200, f"nhân viên có vai: đọc được {entity}")
        code, body = req("PUT", f"{base}/org-admin/users/{staff_id}", ot, {"roleId": ""})
        check(code == 200 and (body.get("data") or {}).get("user", {}).get("roleId") in (None, ""), "chủ khách bỏ vai nhân viên ⇒ 200", f"{code} {body.get('code', '')}")
        if entity:
            check(req("GET", f"{base}/{entity}?limit=1", st)[0] == 403, f"nhân viên mất vai: {entity} ⇒ 403 ở request kế tiếp")
        code, body = req("PUT", f"{base}/org-admin/users/{staff_id}", ot, {"roleId": role["id"]})
        check(code == 200, "gán lại vai CRM ⇒ 200", f"{code} {body.get('code', '')}")

        print("B5 — khoá / mở; không động vào quản trị")
        code, _ = req("PUT", f"{base}/org-admin/users/{staff_id}", ot, {"status": "inactive"})
        check(code == 200, "khoá nhân viên ⇒ 200")
        # /auth/me self-verifies and does not look at status; the data plane (Auth stage) does.
        check(req("GET", base + "/metadata/entities", st)[0] == 401, "nhân viên bị khoá ⇒ 401 ở request kế tiếp")
        code, _ = req("POST", base + "/auth/login", body={"tenant": tenant, "email": staff_email, "password": STAFF_PASS})
        check(code in (401, 403), "nhân viên bị khoá không đăng nhập lại được", str(code))
        code, _ = req("PUT", f"{base}/org-admin/users/{staff_id}", ot, {"status": "active"})
        check(code == 200, "mở khoá ⇒ 200")
        check(req("POST", base + "/auth/login", body={"tenant": tenant, "email": staff_email, "password": STAFF_PASS})[0] == 200,
              "mở khoá xong đăng nhập lại được")
        for label, uid in (("quản trị thường", boss_id), ("quản trị viên vận hành", (admin_row or {}).get("id", ""))):
            if not uid:
                continue
            code, body = req("PUT", f"{base}/org-admin/users/{uid}", ot, {"fullName": "HACK"})
            check(code == 403 and body.get("code") == "USER_NOT_MANAGEABLE", f"PUT lên {label} ⇒ 403 USER_NOT_MANAGEABLE", f"{code} {body.get('code', '')}")
        code, body = req("GET", f"{base}/admin/users/{boss_id}", at)
        check((body.get("data") or {}).get("user", {}).get("fullName") != "HACK", "tài khoản quản trị không bị ghi")
        code, body = req("PUT", f"{base}/org-admin/users/{owner_id}", ot, {"fullName": "HACK"})
        check(code == 403 and body.get("code") == "USER_NOT_MANAGEABLE", "chủ khách tự PUT mình ⇒ 403 USER_NOT_MANAGEABLE", f"{code}")

        print("B6 — khoá không có trong hợp đồng bị bỏ qua; tắt cờ có hiệu lực ngay")
        code, _ = req("PUT", f"{base}/org-admin/users/{staff_id}", ot, {"fullName": "Tên mới", "isAdmin": True, "canManageUsers": True, "password": "Hacked-Pass-1!"})
        check(code == 200, "PUT kèm isAdmin/canManageUsers/password ⇒ 200 (bị bỏ qua)")
        code, body = req("GET", f"{base}/admin/users/{staff_id}", at)
        u = (body.get("data") or {}).get("user") or {}
        check(u.get("isAdmin") is False and u.get("canManageUsers") is False and u.get("fullName") == "Tên mới", "đọc lại bằng người dựng: vẫn là nhân viên, tên đã đổi")
        check(req("POST", base + "/auth/login", body={"tenant": tenant, "email": staff_email, "password": "Hacked-Pass-1!"})[0] != 200, "mật khẩu không bị đổi")
        code, _ = req("PUT", f"{base}/admin/users/{owner_id}/user-manager", ot, {"enabled": False})
        check(code == 403, "chủ khách tự gọi user-manager ⇒ 403")
        code, _ = req("PUT", f"{base}/admin/users/{owner_id}/user-manager", at, {"enabled": False})
        check(code == 200, "người dựng tắt cờ ⇒ 200")
        code, body = req("GET", base + "/org-admin/users", ot)
        check(code == 403 and body == FORBIDDEN_BODY, "cờ tắt: /org-admin ⇒ 403 ngay ở request kế tiếp", f"{code}")
    finally:
        left = cleanup(base, at)
        if left:
            failures.append(f"dọn chưa sạch ({left} tài khoản)")
            print(f"  ĐỎ   dọn chưa sạch: còn {left} tài khoản {DOMAIN}")


# --------------------------------------------------------------------------------------------
# Slice B2
# --------------------------------------------------------------------------------------------

def slice_b2(base, tenant, admin_pass, db):
    admin = login(base, tenant, "admin@example.com", admin_pass)
    at = admin["accessToken"]
    guarded(base, at)
    try:
        sweep(base, at)
        op_id, op_email = make_user(base, at, "opguard", is_admin=True)
        boss_id, boss_email = make_user(base, at, "quantri", is_admin=True)
        staff_id, _ = make_user(base, at, "nhanvien")
        if not (UUID_RE.match(op_id) and op_email.endswith(DOMAIN)):
            env_fail("id/email vận hành thử sai dạng")
        # The operator flag is grantable only out of band (never via the API): set it by SQL on the CLONE db.
        db_scalar(db, f"UPDATE users SET is_platform_operator = 1 WHERE id = '{op_id}' AND email LIKE '%{DOMAIN}'")
        check(db_scalar(db, f"SELECT is_platform_operator FROM users WHERE id = '{op_id}'") == "1", "tài khoản thử đã mang cờ vận hành")

        b = login(base, tenant, boss_email, STAFF_PASS)["accessToken"]
        print("B2.1 — probe không phá: quản trị không-vận-hành lên tài khoản vận hành")
        code, body = req("GET", f"{base}/admin/users/{op_id}/sessions", b)
        ok1 = check(code == 403 and body.get("code") == "USER_NOT_MANAGEABLE", "GET …/sessions ⇒ 403 USER_NOT_MANAGEABLE", f"{code} {body.get('code', '')}")
        code, body = req("PUT", f"{base}/admin/users/{op_id}", b, {"fullName": f"Nghiệm thu opguard"})
        ok2 = check(code == 403 and body.get("code") == "USER_NOT_MANAGEABLE", "PUT (họ tên giữ nguyên) ⇒ 403 USER_NOT_MANAGEABLE", f"{code} {body.get('code', '')}")
        if not (ok1 and ok2):
            print("  DỪNG — hai probe không phá chưa 403; không thử reset-link / kết thúc phiên / xoá (tránh phá tài khoản vận hành)")
            return
        print("B2.2 — probe có thể phá (chỉ chạy khi hai probe trên đã 403)")
        code, body = req("POST", f"{base}/admin/users/{op_id}/reset-password-link", b)
        check(code == 403 and body.get("code") == "USER_NOT_MANAGEABLE", "POST reset-password-link ⇒ 403", f"{code} {body.get('code', '')}")
        code, body = req("DELETE", f"{base}/admin/users/{op_id}/sessions/{secrets.token_hex(8)}", b)
        check(code == 403 and body.get("code") == "USER_NOT_MANAGEABLE", "DELETE …/sessions/:sid ⇒ 403", f"{code} {body.get('code', '')}")
        code, body = req("DELETE", f"{base}/admin/users/{op_id}", b)
        check(code == 403 and body.get("code") == "USER_NOT_MANAGEABLE", "DELETE tài khoản vận hành ⇒ 403", f"{code} {body.get('code', '')}")
        code, body = req("GET", f"{base}/admin/users/{op_id}", at)
        check(code == 200 and (body.get("data") or {}).get("user", {}).get("status") == "active", "tài khoản vận hành còn nguyên, còn hoạt động")
        print("B2.3 — không đổi gì với người khác")
        code, _ = req("PUT", f"{base}/admin/users/{staff_id}", b, {"fullName": "Tên mới"})
        check(code == 200, "quản trị không-vận-hành sửa nhân viên thường ⇒ 200 như cũ")
        code, _ = req("GET", f"{base}/admin/users/{staff_id}/sessions", b)
        check(code == 200, "…và xem phiên nhân viên thường ⇒ 200")
        code, _ = req("PUT", f"{base}/admin/users/{op_id}", at, {"fullName": "Nghiệm thu opguard 2"})
        check(code == 200, "người vận hành (admin@example.com) sửa tài khoản vận hành ⇒ 200")
    finally:
        left = cleanup(base, at)
        if left:
            failures.append(f"dọn chưa sạch ({left} tài khoản)")
            print(f"  ĐỎ   dọn chưa sạch: còn {left} tài khoản {DOMAIN}")


# --------------------------------------------------------------------------------------------
# Slice C
# --------------------------------------------------------------------------------------------

def slice_c(base, tenant, admin_pass, db):
    admin = login(base, tenant, "admin@example.com", admin_pass)
    at = admin["accessToken"]
    guarded(base, at)
    try:
        sweep(base, at)
        role = pick_role(base, at)
        owner_id, owner_email = make_user(base, at, "chukhach")
        code, body = req("PUT", f"{base}/admin/users/{owner_id}/user-manager", at, {"enabled": True})
        if not check(code == 200, "người dựng bật 'Chủ khách' cho tài khoản thử", f"{code} {body.get('code', '')}"):
            return
        ot = login(base, tenant, owner_email, STAFF_PASS)["accessToken"]

        print("C1 — chủ khách thêm nhân viên (không mật khẩu), lấy link, nhân viên tự đặt mật khẩu")
        new_email = f"nv-{int(time.strftime('%H%M%S'))}-{secrets.token_hex(3)}{DOMAIN}"
        if not EMAIL_RE.match(new_email):
            env_fail("email thử sai dạng")
        code, body = req("POST", base + "/org-admin/users", ot, {
            "email": new_email, "fullName": "Nhân viên mới", "roleId": role["id"],
            "password": "Hacked-Pass-1!", "isAdmin": True, "canManageUsers": True,
        })
        user = (body.get("data") or {}).get("user") or {}
        if not check(code == 201 and user.get("tier") == "staff" and user.get("manageable") is True,
                     "POST /org-admin/users ⇒ 201, tier staff", f"{code} {body.get('code', '')}"):
            return
        staff_id = user["id"]
        created.append(staff_id)
        check("password" not in json.dumps(body).lower(), "phản hồi không chứa mật khẩu")
        code, body = req("GET", f"{base}/admin/users/{staff_id}", at)
        u = (body.get("data") or {}).get("user") or {}
        check(u.get("isAdmin") is False and u.get("canManageUsers") is False, "isAdmin/canManageUsers trong thân bị bỏ qua")
        check(req("POST", base + "/auth/login", body={"tenant": tenant, "email": new_email, "password": "Hacked-Pass-1!"})[0] != 200,
              "mật khẩu trong thân không có hiệu lực")
        code, body = req("POST", base + "/org-admin/users", ot, {"email": new_email})
        check(code == 409 and body.get("code") == "USER_ALREADY_EXISTS", "thêm lại cùng email ⇒ 409", f"{code} {body.get('code', '')}")

        code, body = req("POST", f"{base}/org-admin/users/{staff_id}/reset-password-link", ot)
        link = (body.get("data") or {})
        check(code == 201 and link.get("token"), "POST …/reset-password-link ⇒ 201 có token", f"{code} {body.get('code', '')}")
        code, body = req("POST", base + "/auth/reset-password", body={"token": link.get("token", ""), "newPassword": STAFF_PASS})
        check(code in (200, 204), "nhân viên đặt mật khẩu bằng link ⇒ thành công", f"{code} {body.get('code', '')}")
        s = login(base, tenant, new_email, STAFF_PASS)
        st = s["accessToken"]
        check(req("GET", base + "/admin/users", st)[0] == 403, "nhân viên mới: /admin/users ⇒ 403")
        check(req("GET", base + "/metadata/entities", st)[0] == 200, "nhân viên mới đọc được metadata của vai")

        print("C2 — phiên của nhân viên: xem và kết thúc một phiên")
        s2 = login(base, tenant, new_email, STAFF_PASS)
        code, body = req("GET", f"{base}/org-admin/users/{staff_id}/sessions", ot)
        sessions = body.get("data") or []
        check(code == 200 and len(sessions) >= 2, "GET …/sessions ⇒ 200, thấy ≥ 2 phiên", f"{code} {len(sessions)}")
        sid = jwt_sid(st)
        code, _ = req("DELETE", f"{base}/org-admin/users/{staff_id}/sessions/{sid}", ot)
        check(code == 204, "DELETE …/sessions/:sid ⇒ 204", str(code))
        check(req("GET", base + "/metadata/entities", st)[0] == 401, "phiên bị kết thúc ⇒ 401 ở request kế tiếp")
        check(req("GET", base + "/metadata/entities", s2["accessToken"])[0] == 200, "phiên còn lại vẫn dùng được")
        code, body = req("DELETE", f"{base}/org-admin/users/{staff_id}/sessions/{sid}", ot)
        check(code == 404 and body.get("code") == "SESSION_NOT_FOUND", "kết thúc lại phiên đó ⇒ 404 SESSION_NOT_FOUND", f"{code}")

        print("C3 — không động vào quản trị / chủ khách khác; nhân viên không được gọi")
        admin_row = next((x for x in (req("GET", base + "/org-admin/users", ot)[1].get("data") or {}).get("users", []) if x.get("tier") == "admin"), None)
        if admin_row:
            aid = admin_row["id"]
            for method, path in (("POST", f"/org-admin/users/{aid}/reset-password-link"), ("GET", f"/org-admin/users/{aid}/sessions")):
                code, body = req(method, base + path, ot)
                check(code == 403 and body.get("code") == "USER_NOT_MANAGEABLE", f"{method} {path} (admin) ⇒ 403 USER_NOT_MANAGEABLE", f"{code} {body.get('code', '')}")
        code, body = req("POST", base + "/org-admin/users", s2["accessToken"], {"email": "x" + new_email})
        check(code == 403 and body == FORBIDDEN_BODY, "nhân viên POST /org-admin/users ⇒ 403 byte-identical", f"{code}")

        print("C4 — nhật ký __user__ có actor = chủ khách")
        code, body = req("GET", f"{base}/audit-log?entityName=__user__&recordId={staff_id}&limit=50", at)
        rows = (body.get("data") or [])
        rows = rows if isinstance(rows, list) else (rows.get("entries") or rows.get("items") or [])
        mine = [r for r in rows if r.get("actorUserId") == owner_id]
        acts = {r.get("action") for r in mine}
        check(code == 200 and "create" in acts, "có dòng create với actor = chủ khách", f"{code} {sorted(a for a in acts if a)}")
        check(any("sessionEnded" in json.dumps(r) for r in mine), "có dòng kết thúc phiên với actor = chủ khách")
        check(any("resetLinkIssued" in json.dumps(r) for r in mine), "có dòng cấp link mời với actor = chủ khách")
    finally:
        left = cleanup(base, at)
        if left:
            failures.append(f"dọn chưa sạch ({left} tài khoản)")
            print(f"  ĐỎ   dọn chưa sạch: còn {left} tài khoản {DOMAIN}")


# --------------------------------------------------------------------------------------------
# Slices D / D2 — owner tier per WORKSPACE (operator = admin@example.com; the customer side is a throwaway workspace)
# --------------------------------------------------------------------------------------------

SALES_LABEL = "Nhân viên kinh doanh"  # label of the CRM pack's crm_sales_rep role


def new_workspace(base, at, label):
    """Provision a throwaway customer workspace (no delete route exists: needs a nexus_wt_ clone DB)."""
    slug = f"ownacc-{label}-{int(time.time())}-{secrets.token_hex(2)}"
    email = f"ow-{int(time.time())}-{secrets.token_hex(3)}{DOMAIN}"
    if not EMAIL_RE.match(email):
        env_fail("email thử sai dạng")
    code, body = req("POST", base + "/admin/tenants", at, {"slug": slug, "name": f"Nghiệm thu {label}", "adminEmail": email, "generatePassword": True})
    data = body.get("data") or {}
    if code != 201 or not (data.get("oneTimeCredentials") or {}).get("password"):
        env_fail(f"tạo không gian thử thất bại ({code} {body.get('code', '')})")
    return data["tenant"]["id"], slug, email, data["oneTimeCredentials"]["password"], data["admin"]


def tenant_row(base, at, tid):
    code, body = req("GET", base + "/admin/tenants", at)
    return next((t for t in (body.get("data") or []) if t.get("id") == tid), {}) if code == 200 else {}


def install_crm(base, at, slug):
    code, body = req("POST", f"{base}/admin/verticals/install?targetTenant={slug}", at, {"builtin": "crm"})
    return check(code == 200, "cài gói CRM vào không gian thử (?targetTenant)", f"{code} {body.get('code', '')}")


def owner_can_run_the_house(base, slug, owner_email, owner_pass, ot, tag):
    """What a customer owner must be able to do, and must NOT be able to do. Returns the staff token or None."""
    check(req("GET", base + "/admin/roles", ot)[0] == 403, f"{tag}: chủ khách /admin/roles ⇒ 403")
    code, body = req("GET", base + "/org-admin/users", ot)
    check(code == 200, f"{tag}: chủ khách /org-admin/users ⇒ 200", str(code))
    check(req("GET", base + "/crm_accounts?limit=1", ot)[0] == 200, f"{tag}: chủ khách đọc được bảng của gói (vai tenant_owner)")
    code, body = req("GET", base + "/org-admin/roles", ot)
    roles = (body.get("data") or {}).get("roles") or []
    check(not any(r.get("label") == "Chủ khách — toàn quyền dữ liệu" and r.get("assignable") for r in roles),
          f"{tag}: vai chủ khách không gán được cho người khác")
    role = next((r for r in roles if r.get("label") == SALES_LABEL and r.get("assignable")), None)
    if not check(role is not None, f"{tag}: vai {SALES_LABEL!r} của gói gán được", str([r.get('label') for r in roles])):
        return None
    st_email = f"nv-{int(time.time())}-{secrets.token_hex(3)}{DOMAIN}"
    code, body = req("POST", base + "/org-admin/users", ot, {"email": st_email, "fullName": "Nhân viên thử", "roleId": role["id"]})
    user = (body.get("data") or {}).get("user") or {}
    if not check(code == 201 and user.get("tier") == "staff", f"{tag}: chủ khách thêm nhân viên mang vai của gói", f"{code} {body.get('code', '')}"):
        return None
    code, body = req("POST", f"{base}/org-admin/users/{user['id']}/reset-password-link", ot)
    token = (body.get("data") or {}).get("token", "")
    check(code == 201 and token, f"{tag}: chủ khách cấp link mời", str(code))
    code, _ = req("POST", base + "/auth/reset-password", body={"token": token, "newPassword": STAFF_PASS})
    check(code in (200, 204), f"{tag}: nhân viên đặt mật khẩu bằng link", str(code))
    s = login(base, slug, st_email, STAFF_PASS)["accessToken"]
    check(req("GET", base + "/crm_accounts?limit=1", s)[0] == 200, f"{tag}: nhân viên đọc được bảng theo vai của gói")
    check(req("GET", base + "/org-admin/users", s)[0] == 403, f"{tag}: nhân viên /org-admin/users ⇒ 403")
    return s


def slice_d(base, tenant, admin_pass, db):
    admin = login(base, tenant, "admin@example.com", admin_pass)
    at = admin["accessToken"]
    code, me = req("GET", base + "/auth/me", at)
    my_tid = ((me.get("data") or me).get("tenantId")) or tenant_row_by_slug(base, at, tenant).get("id", "")
    tid, slug, email, pw, adm = new_workspace(base, at, "d")
    print(f"D0 — không gian thử {slug} (cuối chạy vẫn còn trên DB bản sao)")
    # Make it look like an OLD workspace: first account is an admin without a role.
    code, body = req("DELETE", f"{base}/admin/tenants/{tid}/owner-tier", at)
    d = body.get("data") or {}
    check(code == 200 and d.get("reverted") == 1, "đưa về dạng cũ: DELETE owner-tier ⇒ reverted 1", f"{code} {body}")
    check("@" not in json.dumps(body), "phản hồi không có email")
    check(install_crm(base, at, slug), "gói CRM đã cài")
    old_admin = login(base, slug, email, pw)["accessToken"]
    check(req("GET", base + "/admin/roles", old_admin)[0] == 200, "dạng cũ: khách là quản trị viên (/admin/roles ⇒ 200)")
    row = tenant_row(base, at, tid)
    check((row.get("usableAdminCount"), row.get("usableOwnerCount"), row.get("convertibleAdminCount")) == (1, 0, 1),
          "ListTenants: 1 quản trị viên · 0 chủ khách · 1 chuyển được", str(row))

    print("D1 — từ chối: chính xưởng mình; quản trị không-vận-hành")
    if my_tid:
        code, body = req("POST", f"{base}/admin/tenants/{my_tid}/owner-tier", at)
        check(code == 422 and body.get("code") == "TENANT_SELF_OWNER_TIER", "tự chuyển xưởng của mình ⇒ 422 TENANT_SELF_OWNER_TIER", f"{code} {body.get('code', '')}")
    code, body = req("POST", f"{base}/admin/tenants/{tid}/owner-tier", old_admin)
    check(code == 403 and body == FORBIDDEN_BODY, "quản trị viên khách (không vận hành) POST owner-tier ⇒ 403 byte-identical", f"{code}")
    code, body = req("POST", f"{base}/admin/tenants/00000000-0000-0000-0000-000000000000/owner-tier", at)
    check(code == 404 and body.get("code") == "TENANT_NOT_FOUND", "không gian không có ⇒ 404 TENANT_NOT_FOUND", f"{code}")

    print("D2 — chuyển sang chủ khách (hai lần)")
    code, body = req("POST", f"{base}/admin/tenants/{tid}/owner-tier", at)
    d = body.get("data") or {}
    check(code == 200 and d.get("converted") == 1 and d.get("ownerRoleId") and d.get("revokeFailed") == 0, "POST ⇒ converted 1", f"{code} {body}")
    check("@" not in json.dumps(body), "phản hồi không có email")
    code, body = req("POST", f"{base}/admin/tenants/{tid}/owner-tier", at)
    check(code == 200 and (body.get("data") or {}).get("converted") == 0, "POST lần hai ⇒ converted 0 (idempotent)", f"{code} {body}")
    # The access token is a JWT that outlives the switch; what must change at once is what it is ALLOWED to do
    # (claim freshness reads the live row), not whether it parses.
    check(req("GET", base + "/admin/roles", old_admin)[0] == 403, "token cũ của quản trị viên: /admin/roles ⇒ 403 ngay ở request kế tiếp")
    row = tenant_row(base, at, tid)
    check((row.get("usableAdminCount"), row.get("usableOwnerCount"), row.get("convertibleAdminCount")) == (0, 1, 0),
          "ListTenants: 0 quản trị viên · 1 chủ khách · 0 chuyển được", str(row))
    ot = login(base, slug, email, pw)["accessToken"]
    owner_can_run_the_house(base, slug, email, pw, ot, "D2")

    print("D3 — trả về quản trị viên (hai lần)")
    code, body = req("DELETE", f"{base}/admin/tenants/{tid}/owner-tier", at)
    check(code == 200 and (body.get("data") or {}).get("reverted") == 1, "DELETE ⇒ reverted 1", f"{code} {body}")
    code, body = req("DELETE", f"{base}/admin/tenants/{tid}/owner-tier", at)
    check(code == 200 and (body.get("data") or {}).get("reverted") == 0, "DELETE lần hai ⇒ reverted 0", f"{code} {body}")
    check(req("GET", base + "/org-admin/users", ot)[0] in (401, 403), "phiên chủ khách cũ không còn dùng được")
    back = login(base, slug, email, pw)["accessToken"]
    check(req("GET", base + "/admin/roles", back)[0] == 200, "về lại quản trị viên: /admin/roles ⇒ 200")
    row = tenant_row(base, at, tid)
    check((row.get("usableAdminCount"), row.get("usableOwnerCount"), row.get("convertibleAdminCount")) == (1, 0, 1),
          "ListTenants trở lại như trước chuyển", str(row))


def tenant_row_by_slug(base, at, slug):
    code, body = req("GET", base + "/admin/tenants", at)
    return next((t for t in (body.get("data") or []) if t.get("slug") == slug), {}) if code == 200 else {}


def slice_d2(base, tenant, admin_pass, db):
    admin = login(base, tenant, "admin@example.com", admin_pass)
    at = admin["accessToken"]
    print("D2.1 — không gian mới: tài khoản đầu là chủ khách")
    tid, slug, email, pw, adm = new_workspace(base, at, "d2")
    check(adm.get("isAdmin") is False and adm.get("canManageUsers") is True and adm.get("roleId"),
          "phản hồi POST /admin/tenants: isAdmin=false canManageUsers=true roleId", str(adm))
    row = tenant_row(base, at, tid)
    check((row.get("usableAdminCount"), row.get("usableOwnerCount")) == (0, 1), "ListTenants: 0 quản trị viên · 1 chủ khách", str(row))
    check(install_crm(base, at, slug), "gói CRM đã cài")
    ot = login(base, slug, email, pw)["accessToken"]
    code, me = req("GET", base + "/auth/me", ot)
    check(code == 200 and (me.get("data") or me).get("isAdmin") is False and (me.get("data") or me).get("canManageUsers") is True,
          "chủ khách: /auth/me isAdmin=false canManageUsers=true")
    print("D2.2 — chủ khách dùng được cả nhà")
    owner_can_run_the_house(base, slug, email, pw, ot, "D2")
    print("D2.3 — SC-011: cổng người-thứ-ba chạy được với token của phiên khác (không đăng nhập quản trị)")
    here = os.path.dirname(os.path.abspath(__file__))
    env = dict(os.environ, NEXUS_ADMIN_TOKEN=at)  # the staff password stays acceptance-acl's own default / NEXUS_STAFF_PASS
    out = subprocess.run([sys.executable, os.path.join(here, "acceptance-acl.py"), "--base", base, "--tenant", tenant,
                          "--admin-token-env", "NEXUS_ADMIN_TOKEN", "--expect", "crm_accounts"],
                         capture_output=True, text=True, timeout=300, env=env)
    check(out.returncode in (0, 1), "acceptance-acl --admin-token-env chạy được (exit 0/1, không 2)", f"exit {out.returncode}: {out.stderr.strip()[-200:]}")


SLICES = {"A": slice_a, "B": slice_b, "B2": slice_b2, "C": slice_c, "D": slice_d, "D2": slice_d2}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--slice", default="A")
    ap.add_argument("--base", default="http://localhost:8090/api/v1")
    ap.add_argument("--tenant", default="northwind")
    ap.add_argument("--db", default="")
    a = ap.parse_args()

    host = urllib.parse.urlparse(a.base).hostname or ""
    try:
        loop = host == "localhost" or ipaddress.ip_address(host).is_loopback
    except ValueError:
        loop = False
    if not loop:
        env_fail(f"--base phải là loopback (script GHI dữ liệu); nhận {host!r}")
    if a.slice not in SLICES:
        env_fail(f"nhóm {a.slice!r} chưa viết (có: {', '.join(SLICES)})")
    if a.slice in ("A", "B2", "D", "D2") and not a.db.startswith("nexus_wt_"):
        env_fail(f"nhóm {a.slice} cần --db nexus_wt_<slug> (đọc security_alerts / bật cờ vận hành / tạo không gian không xoá được trên DB bản sao)")

    admin_pass = os.environ.get("NEXUS_ADMIN_PASS", "Demo-Admin-2026!")
    print(f"acceptance-owner-tier --slice {a.slice} · {a.base} · tenant {a.tenant}")
    SLICES[a.slice](a.base, a.tenant, admin_pass, a.db)
    if failures:
        print(f"\nCHƯA ĐẠT — {len(failures)} kiểm đỏ: " + "; ".join(failures))
        return 1
    print("\nĐẠT — mọi kiểm xanh")
    return 0


if __name__ == "__main__":
    sys.exit(main())
