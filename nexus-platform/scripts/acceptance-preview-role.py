#!/usr/bin/env python3
"""Nghiệm thu người-thứ-ba của token "xem như vai" (specs/064-preview-as-role-sample-data S9-1, #393).

Vì sao cần: người dựng là admin, và admin bỏ qua toàn bộ ACL — "tôi thử thấy chạy" không chứng minh gì.
Token xem thử phải cho người dựng thấy ĐÚNG như một nhân viên của vai đó, không hơn, không ghi được, và
chết ngay khi phiên đăng nhập của người dựng chết. Script so trùng với một đăng nhập THẬT của nhân viên
(`test@example.com`, vai `crm_sales_rep`) trên server đang chạy:

  1. So trùng: `/metadata/permissions/me`, `/apps`, và TẬP ID dòng của mọi bảng vai xem ở mức "tất cả" —
     token xem thử phải bằng đúng nhân viên thật. Bảng mức "của tôi"/"đội" phải RỖNG với token xem thử
     (chủ thể tổng hợp không sở hữu gì). Ép `IsAdmin=true` trong bộ xác thực ⇒ phần này đỏ.
  2. Chặn: `POST /<bảng>` ⇒ 403 `PREVIEW_READ_ONLY`; `GET /auth/sessions` ⇒ 403 `PREVIEW_FORBIDDEN`;
     `GET /admin/users` ⇒ 403 `FORBIDDEN`; dùng token xem thử làm refresh token ⇒ 401.
  3. Ai được mở: nhân viên thật ⇒ 403; quản trị viên khách di sản (admin, KHÔNG phải người vận hành — tài
     khoản tạm `nghiemthu-preview-legacy-admin@acceptance.invalid`, cất lại trong `finally`) ⇒ 403.
  4. Thoát: `DELETE /preview-sessions/current` ⇒ 204, rồi token ⇒ 401; Nhật ký có `__preview__`
     create + delete mang TÊN NGƯỜI DỰNG THẬT (không phải chủ thể tổng hợp).
  5. Thu hồi: người dựng đăng xuất phiên đã mở xem thử ⇒ 401 ngay request kế tiếp; người dựng ĐỔI MẬT
     KHẨU ở phiên khác ⇒ 401 (đường `RevokeAllForUser` không đánh dấu 'ended' — kiểm "phiên còn sống",
     không phải "phiên chưa kết thúc"). Mật khẩu được đổi lại trong `finally`.

  6b. `--workspace <slug>` (S9-8): CHẠY THAY cho 1-6 — người dựng mở phiên dựng trong không gian của khách <slug>
     rồi xem như vai TRONG không gian đó (token phiên dựng mở xem thử). Cần một nhân viên thật của khách
     (`--workspace-staff`, mật khẩu `NEXUS_WS_STAFF_PASS`, vai V): so trùng quyền/ứng dụng/tập id dòng với đăng
     nhập thật của họ; chỉ đọc; vai của tenant KHÁC (vai xưởng) ⇒ 422 `PREVIEW_ROLE_NOT_FOUND`; Nhật ký của
     khách có dòng `__preview__` mang TÊN người dựng kèm "(Tên xưởng)"; đóng phiên dựng ("Về Nexus") ⇒ token xem
     thử 401 ngay; người dựng đăng xuất khỏi xưởng ⇒ 401.
  7. `--scan-get` (S9-3): GET mọi route của `RegisterAPIRoutes` bằng token xem thử (danh sách do
     `go test ./internal/handlers -run TestPreviewRouteScan_GetRoutes -args -routes-out=<f>` ghi ra; `--routes-file`
     để dùng danh sách có sẵn) — bất kỳ 5xx ⇒ exit 1; rồi, với `NEXUS_ACCEPT_MYSQL_DSN` (DB bản sao,
     `user:pass@tcp(host:port)/nexus_wt_<lát>`; thiếu ⇒ CHƯA ĐẠT), đếm principal của phiên trong MỌI cột chuỗi/JSON
     của MỌI bảng có `tenant_id` (liệt kê qua information_schema, không danh sách tay; `audit_log` không được miễn,
     chỉ `sys_preview_sessions.principal_id`) — khác 0 ở đâu ⇒ exit 1 kèm bảng.cột. Cần client `mysql` hoặc container
     `nexus-mysql` (docker exec). Lưu ý giới hạn đăng nhập 10 lần/phút: chờ ~1 phút giữa hai lần chạy.

Dùng (server riêng + DB bản sao, KHÔNG dùng :8090 chung — script từ chối cổng đó; script GHI: đổi mật khẩu người dựng rồi đổi lại,
bật/cất một tài khoản admin tạm):
    python3 scripts/acceptance-preview-role.py --base http://localhost:8163/api/v1

Mật khẩu: `NEXUS_ADMIN_PASS` / `NEXUS_STAFF_PASS`, thiếu thì rơi về tài khoản seed của tenant dev. `--base` bắt buộc. Chỉ ghi
khi `--base` là loopback (hoặc có `--allow-remote-write`).

Mã thoát: 0 = đạt · 1 = lệch/không chặn/không thu hồi · 2 = lỗi môi trường (kể cả không đổi lại được mật
khẩu hoặc không cất được tài khoản tạm — in rõ để người chạy tự xử).

ponytail: stdlib thuần (urllib/argparse), khuôn `acceptance-acl.py`.
"""

import argparse
import ipaddress
import json
import os
import secrets
import signal
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

DEFAULTS = {
    "tenant": "northwind",
    "admin": "admin@example.com",
    "admin_pass": os.environ.get("NEXUS_ADMIN_PASS") or "Demo-Admin-2026!",
    "staff": "test@example.com",
    "staff_pass": os.environ.get("NEXUS_STAFF_PASS") or "Demo-Rep-2026!",
}

LEGACY_ADMIN_EMAIL = "nghiemthu-preview-legacy-admin@acceptance.invalid"


class EnvError(Exception):
    """Environment problem (server down, login refused, cleanup failed) — exit 2."""


def _req(method, url, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data is not None else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.load(e)
        except Exception:
            return e.code, {}
    except Exception as e:  # connection refused, DNS, timeout
        raise EnvError(f"không gọi được {method} {url} — {e}") from e


def is_loopback(base):
    """True when --base points at this machine: `localhost` or a literal loopback IP."""
    host = (urllib.parse.urlparse(base).hostname or "").lower()
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


SHARED_DEV_PORT = 8090  # the shared dev/demo server — this script writes, so it never targets it


def is_shared_dev(base):
    """True when --base is the shared dev server's port (:8090)."""
    try:
        return urllib.parse.urlparse(base).port == SHARED_DEV_PORT
    except ValueError:
        return False


def login(base, tenant, email, password):
    """(access, refresh, user) of a fresh login session."""
    code, body = _req("POST", base + "/auth/login", body={"tenant": tenant, "email": email, "password": password})
    data = body.get("data") or {}
    if code != 200 or not data.get("accessToken"):
        if data.get("mfaPending"):
            raise EnvError(f"{email} đang bật MFA — script không tự vượt bước 2")
        raise EnvError(f"đăng nhập {email} thất bại ({code} {body.get('code', '')})")
    return data["accessToken"], data.get("refreshToken", ""), data.get("user") or {}


def perm_set(body):
    """The comparable set of (entity, action, level) grants from GET /metadata/permissions/me."""
    rows = body.get("data")
    if not isinstance(rows, list):
        return None
    return {(r.get("entityName"), r.get("action"), r.get("accessLevel")) for r in rows}


def app_names(body):
    apps = ((body.get("data") or {}).get("apps")) or []
    return sorted(a.get("name") for a in apps)


def view_levels(perms):
    """entity → access level of its `view` grant (the level the record list is scoped by)."""
    return {e: lvl for (e, act, lvl) in perms if act == "view"}


def record_ids(base, token, entity, cap=5000):
    """(status, ids) — every id of the entity the token can list, walking the keyset cursor."""
    ids, after = set(), None
    while True:
        q = {"paginate": "cursor", "limit": "100"}
        if after:
            q["after"] = after
        code, body = _req("GET", f"{base}/{entity}?{urllib.parse.urlencode(q)}", token)
        if code != 200:
            return code, ids
        for row in body.get("data") or []:
            ids.add(row.get("id"))
        after = (body.get("pagination") or {}).get("nextCursor")
        if not after or len(ids) >= cap:
            return 200, ids


def audit_ok(rows, session_id, builder_id):
    """(ok, detail): the __preview__ trail of one session has a create AND a delete row, every row names
    the REAL builder (never the synthetic principal) and carries the session id."""
    mine = [r for r in rows if r.get("recordId") == session_id]
    actions = {r.get("action") for r in mine}
    actors = {r.get("actorUserId") for r in mine}
    if not {"create", "delete"} <= actions:
        return False, f"thiếu dòng: có {sorted(actions)} (cần create + delete)"
    if actors != {builder_id}:
        return False, f"người thao tác {sorted(a or '' for a in actors)} ≠ người dựng {builder_id}"
    return True, f"{len(mine)} dòng, người thao tác = người dựng"


class Gate:
    def __init__(self):
        self.failures = []

    def check(self, name, ok, detail=""):
        print(("  ✅ " if ok else "  ❌ ") + name + (f" — {detail}" if detail else ""))
        if not ok:
            self.failures.append(name)


def start_preview(base, token, role_id):
    return _req("POST", base + "/admin/preview-sessions", token, {"roleId": role_id})


def alive(base, preview_token):
    code, _ = _req("GET", base + "/metadata/entities", preview_token)
    return code


def ensure_legacy_admin(base, adm):
    """Enable (create once, then reuse) the temporary legacy customer admin: is_admin=1,
    is_platform_operator=0 (no API sets the operator flag). Returns (user_id, password)."""
    password = "Pv-" + secrets.token_urlsafe(18)
    code, body = _req("POST", base + "/admin/users", adm, {
        "email": LEGACY_ADMIN_EMAIL, "password": password, "fullName": "Nghiệm thu xem như vai (tạm)",
        "status": "active", "isAdmin": True})
    if code == 201:
        return ((body.get("data") or {}).get("user") or body.get("data") or {}).get("id"), password
    if code != 409:
        raise EnvError(f"tạo admin tạm thất bại ({code} {body.get('code', '')})")
    code, body = _req("GET", base + "/admin/users", adm)
    users = ((body.get("data") or {}).get("users")) or []
    uid = next((u.get("id") for u in users if u.get("email") == LEGACY_ADMIN_EMAIL), None)
    if not uid:
        raise EnvError("admin tạm đã tồn tại (409) nhưng không thấy trong danh sách — có thể đã bị xoá mềm")
    code, body = _req("PUT", f"{base}/admin/users/{uid}", adm, {"status": "active", "isAdmin": True, "password": password})
    if code != 200:
        raise EnvError(f"bật lại admin tạm thất bại ({code} {body.get('code', '')})")
    return uid, password


def stow_legacy_admin(base, adm, uid):
    code, body = _req("PUT", f"{base}/admin/users/{uid}", adm, {"status": "disabled", "isAdmin": False})
    if code != 200:
        raise EnvError(f"KHÔNG cất được admin tạm {uid} ({code} {body.get('code', '')}) — tắt tay trong Admin › Người dùng")


def run(a, gate):
    base = a.base.rstrip("/")
    print(f"Server: {base} · tenant {a.tenant} · người dựng {a.admin} · nhân viên {a.staff}")

    adm, _, adm_user = login(base, a.tenant, a.admin, a.admin_pass)
    staff, _, staff_user = login(base, a.tenant, a.staff, a.staff_pass)
    a.staff_session = (staff, staff_user)
    role_id = staff_user.get("roleId")
    if not role_id:
        raise EnvError(f"{a.staff} không có vai — không có gì để so trùng")
    builder_id = adm_user.get("id")

    print("\n1. Mở phiên xem như vai của nhân viên")
    code, body = start_preview(base, adm, role_id)
    data = body.get("data") or {}
    gate.check("POST /admin/preview-sessions ⇒ 201", code == 201 and bool(data.get("token")), f"{code} {body.get('code', '')}")
    if code != 201:
        return
    pv, session_id = data["token"], data.get("sessionId")
    print(f"     vai {data.get('role', {}).get('name')} · hết hạn {data.get('expiresAt')}")

    print("\n2. So trùng với đăng nhập thật của nhân viên")
    c1, b1 = _req("GET", base + "/metadata/permissions/me", staff)
    c2, b2 = _req("GET", base + "/metadata/permissions/me", pv)
    staff_perms, pv_perms = perm_set(b1), perm_set(b2)
    gate.check("/metadata/permissions/me trùng", c1 == c2 == 200 and staff_perms is not None and staff_perms == pv_perms,
               f"nhân viên {c1}/{len(staff_perms or [])} quyền · xem thử {c2}/{len(pv_perms or [])} quyền")
    c1, b1 = _req("GET", base + "/apps", staff)
    c2, b2 = _req("GET", base + "/apps", pv)
    gate.check("/apps trùng", c1 == c2 == 200 and app_names(b1) == app_names(b2), f"{app_names(b1)} vs {app_names(b2)}")

    # S9-2: the app boots on GET /auth/me — it must answer the synthetic profile, never the builder's.
    c, b = _req("GET", base + "/auth/me", pv)
    me = (b or {}).get("data") or {}
    pinfo = me.get("preview") or {}
    gate.check("/auth/me = hồ sơ xem thử (không admin, có khối preview, không phải người dựng)",
               c == 200 and me.get("isAdmin") is False and me.get("roleId") == role_id and pinfo.get("sessionId") == session_id
               and me.get("id") != builder_id and me.get("isPlatformOperator") is False,
               f"{c} isAdmin={me.get('isAdmin')} fullName={me.get('fullName')!r} preview.sessionId={pinfo.get('sessionId')}")

    levels = view_levels(staff_perms or set())
    for entity in sorted(levels):
        lvl = levels[entity]
        cs, staff_ids = record_ids(base, staff, entity)
        cp, pv_ids = record_ids(base, pv, entity)
        if lvl == "all":
            gate.check(f"{entity} (xem: tất cả) — cùng tập id", cs == cp == 200 and staff_ids == pv_ids,
                       f"nhân viên {cs}/{len(staff_ids)} dòng · xem thử {cp}/{len(pv_ids)} dòng")
        elif lvl in ("owner", "team"):
            gate.check(f"{entity} (xem: {lvl}) — xem thử rỗng", cp == 200 and not pv_ids,
                       f"xem thử {cp}/{len(pv_ids)} dòng (nhân viên thật {len(staff_ids)} dòng của mình)")

    print("\n3. Chỉ đọc, không leo quyền")
    target = a.entity if a.entity in levels else (sorted(levels)[0] if levels else a.entity)
    code, body = _req("POST", f"{base}/{target}", pv, {})
    gate.check(f"POST /{target} ⇒ 403 PREVIEW_READ_ONLY", code == 403 and body.get("code") == "PREVIEW_READ_ONLY", f"{code} {body.get('code', '')}")
    code, body = _req("GET", base + "/auth/sessions", pv)
    gate.check("GET /auth/sessions ⇒ 403 PREVIEW_FORBIDDEN", code == 403 and body.get("code") == "PREVIEW_FORBIDDEN", f"{code} {body.get('code', '')}")
    code, body = _req("GET", base + "/admin/users", pv)
    gate.check("GET /admin/users ⇒ 403 FORBIDDEN", code == 403 and body.get("code") == "FORBIDDEN", f"{code} {body.get('code', '')}")
    code, body = _req("POST", base + "/auth/refresh", None, {"refreshToken": pv})
    gate.check("token xem thử làm refresh token ⇒ 401", code == 401, f"{code} {body.get('code', '')}")
    code, body = start_preview(base, pv, role_id)
    gate.check("token xem thử mở phiên khác ⇒ 403", code == 403, f"{code} {body.get('code', '')}")

    print("\n4. Ai được mở phiên")
    code, body = start_preview(base, staff, role_id)
    gate.check(f"{a.staff} (không admin) ⇒ 403", code == 403, f"{code} {body.get('code', '')}")
    uid = None
    try:
        uid, pw = ensure_legacy_admin(base, adm)
        legacy, _, _ = login(base, a.tenant, LEGACY_ADMIN_EMAIL, pw)
        code, body = start_preview(base, legacy, role_id)
        gate.check("quản trị viên khách di sản (admin, không operator) ⇒ 403", code == 403, f"{code} {body.get('code', '')}")
    finally:
        if uid:
            stow_legacy_admin(base, adm, uid)

    print("\n5. Thoát phiên + Nhật ký")
    code, _ = _req("DELETE", base + "/preview-sessions/current?reason=exit", pv)
    gate.check("DELETE /preview-sessions/current ⇒ 204", code == 204, str(code))
    gate.check("token sau khi thoát ⇒ 401", alive(base, pv) == 401)
    ok, detail = False, "không đọc được Nhật ký"
    for _ in range(10):  # audit is written asynchronously
        code, body = _req("GET", base + "/audit-log?" + urllib.parse.urlencode(
            {"entityName": "__preview__", "recordId": session_id, "limit": "50"}), adm)
        if code == 200:
            ok, detail = audit_ok(body.get("data") or [], session_id, builder_id)
            if ok:
                break
        time.sleep(0.5)
    gate.check("Nhật ký __preview__ create + delete mang tên người dựng thật", ok, detail)

    print("\n6. Thu hồi theo phiên đăng nhập của người dựng")
    adm2, refresh2, _ = login(base, a.tenant, a.admin, a.admin_pass)
    code, body = start_preview(base, adm2, role_id)
    pv2 = (body.get("data") or {}).get("token")
    gate.check("mở phiên thứ hai", code == 201 and bool(pv2), str(code))
    if pv2:
        _req("POST", base + "/auth/logout", None, {"refreshToken": refresh2})
        gate.check("người dựng đăng xuất ⇒ request xem thử kế tiếp 401", alive(base, pv2) == 401)

    adm3, _, _ = login(base, a.tenant, a.admin, a.admin_pass)
    code, body = start_preview(base, adm3, role_id)
    pv3 = (body.get("data") or {}).get("token")
    gate.check("mở phiên thứ ba", code == 201 and bool(pv3), str(code))
    if not pv3:
        return
    gate.check("phiên thứ ba đang sống", alive(base, pv3) == 200)
    if not (is_loopback(base) or a.allow_remote_write):
        print("  ⚠️  bỏ qua bước đổi mật khẩu (base không phải loopback, không có --allow-remote-write)")
        gate.failures.append("đổi mật khẩu (bỏ qua)")
        return
    new_pw = "Pv-" + secrets.token_urlsafe(18)
    changed = False
    try:
        other, _, _ = login(base, a.tenant, a.admin, a.admin_pass)
        code, body = _req("PUT", base + "/auth/me/password", other, {"currentPassword": a.admin_pass, "newPassword": new_pw})
        changed = code in (200, 204)
        gate.check("đổi mật khẩu người dựng (phiên khác)", changed, f"{code} {body.get('code', '')}")
        if changed:
            gate.check("đổi mật khẩu ⇒ request xem thử kế tiếp 401", alive(base, pv3) == 401)
    finally:
        if changed:
            back, _, _ = login(base, a.tenant, a.admin, new_pw)
            code, body = _req("PUT", base + "/auth/me/password", back, {"currentPassword": new_pw, "newPassword": a.admin_pass})
            if code not in (200, 204):
                raise EnvError(f"KHÔNG đổi lại được mật khẩu {a.admin} ({code} {body.get('code', '')}) — mật khẩu hiện là: {new_pw}")
            login(base, a.tenant, a.admin, a.admin_pass)  # proves the restore


def audit_label_ok(rows, session_id, workshop=None):
    """(ok, detail): every `__preview__` row of the session names the builder as "Họ tên (Tên xưởng)" — the
    label the customer's audit trail shows (specs/062 L3 actorLabelSQL), read from the workspace session."""
    mine = [r for r in rows if r.get("recordId") == session_id]
    if not mine:
        return False, "không có dòng Nhật ký của phiên"
    labels = {(r.get("actorLabel") or "").strip() for r in mine}
    for label in labels:
        if not label.endswith(")") or " (" not in label:
            return False, f"nhãn {sorted(labels)} không có dạng 'Họ tên (Tên xưởng)'"
        if workshop and not label.endswith(f"({workshop})"):
            return False, f"nhãn {sorted(labels)} không kết thúc bằng ({workshop})"
    return True, f"{len(mine)} dòng, nhãn {sorted(labels)}"


def open_workspace(base, home_token, slug):
    """Access token of a builder workspace session in the customer <slug> (POST /admin/workspace-sessions)."""
    code, body = _req("POST", base + "/admin/workspace-sessions", home_token, {"tenantSlug": slug})
    data = body.get("data") or {}
    if code not in (200, 201) or not data.get("accessToken"):
        raise EnvError(f"mở phiên dựng vào '{slug}' thất bại ({code} {body.get('code', '')})")
    return data["accessToken"]


def first_role_id(body):
    data = body.get("data")
    rows = data if isinstance(data, list) else ((data or {}).get("roles") or [])
    return next((r.get("id") for r in rows if r.get("id")), None)


def run_workspace(a, gate):
    base = a.base.rstrip("/")
    slug = a.workspace
    print(f"Server: {base} · xưởng {a.tenant} · người dựng {a.admin} · KHÁCH {slug} · nhân viên khách {a.workspace_staff}")

    adm, _, adm_user = login(base, a.tenant, a.admin, a.admin_pass)
    staff, _, staff_user = login(base, slug, a.workspace_staff, a.workspace_staff_pass)
    role_id = staff_user.get("roleId")
    if not role_id:
        raise EnvError(f"{a.workspace_staff} không có vai trong '{slug}' — không có gì để so trùng")
    builder_id = adm_user.get("id")
    ws = open_workspace(base, adm, slug)

    print("\n1. Mở phiên xem như vai TRONG không gian của khách")
    code, body = start_preview(base, ws, role_id)
    data = body.get("data") or {}
    gate.check("POST /admin/preview-sessions bằng token phiên dựng ⇒ 201", code == 201 and bool(data.get("token")), f"{code} {body.get('code', '')}")
    if code != 201:
        return
    pv, session_id = data["token"], data.get("sessionId")
    print(f"     vai {data.get('role', {}).get('name')} · hết hạn {data.get('expiresAt')}")

    print("\n2. So trùng với đăng nhập thật của nhân viên khách")
    c1, b1 = _req("GET", base + "/metadata/permissions/me", staff)
    c2, b2 = _req("GET", base + "/metadata/permissions/me", pv)
    staff_perms, pv_perms = perm_set(b1), perm_set(b2)
    gate.check("/metadata/permissions/me trùng", c1 == c2 == 200 and staff_perms is not None and staff_perms == pv_perms,
               f"nhân viên {c1}/{len(staff_perms or [])} quyền · xem thử {c2}/{len(pv_perms or [])} quyền")
    c1, b1 = _req("GET", base + "/apps", staff)
    c2, b2 = _req("GET", base + "/apps", pv)
    gate.check("/apps trùng", c1 == c2 == 200 and app_names(b1) == app_names(b2), f"{app_names(b1)} vs {app_names(b2)}")
    c, b = _req("GET", base + "/auth/me", pv)
    me = (b or {}).get("data") or {}
    gate.check("/auth/me = hồ sơ xem thử của khách (không admin, đúng vai, không phải người dựng)",
               c == 200 and me.get("isAdmin") is False and me.get("roleId") == role_id and me.get("id") != builder_id
               and me.get("tenantId") == staff_user.get("tenantId"),
               f"{c} isAdmin={me.get('isAdmin')} tenantId={me.get('tenantId')}")
    levels = view_levels(staff_perms or set())
    for entity in sorted(levels):
        lvl = levels[entity]
        cs, staff_ids = record_ids(base, staff, entity)
        cp, pv_ids = record_ids(base, pv, entity)
        if lvl == "all":
            gate.check(f"{entity} (xem: tất cả) — cùng tập id", cs == cp == 200 and staff_ids == pv_ids,
                       f"nhân viên {cs}/{len(staff_ids)} dòng · xem thử {cp}/{len(pv_ids)} dòng")
        elif lvl in ("owner", "team"):
            gate.check(f"{entity} (xem: {lvl}) — xem thử rỗng", cp == 200 and not pv_ids, f"xem thử {cp}/{len(pv_ids)} dòng")

    print("\n3. Chỉ đọc, không leo quyền, không sang tenant khác")
    target = a.entity if a.entity in levels else (sorted(levels)[0] if levels else a.entity)
    code, body = _req("POST", f"{base}/{target}", pv, {})
    gate.check(f"POST /{target} ⇒ 403 PREVIEW_READ_ONLY", code == 403 and body.get("code") == "PREVIEW_READ_ONLY", f"{code} {body.get('code', '')}")
    code, body = _req("GET", base + "/admin/users", pv)
    gate.check("GET /admin/users ⇒ 403 FORBIDDEN", code == 403 and body.get("code") == "FORBIDDEN", f"{code} {body.get('code', '')}")
    code, body = start_preview(base, pv, role_id)
    gate.check("token xem thử mở phiên khác ⇒ 403", code == 403, f"{code} {body.get('code', '')}")
    code, body = _req("GET", base + "/admin/roles", adm)
    home_role = first_role_id(body) if code == 200 else None
    if home_role:
        code, body = start_preview(base, ws, home_role)
        gate.check("vai của XƯỞNG (tenant khác) qua phiên dựng ⇒ 422 PREVIEW_ROLE_NOT_FOUND",
                   code == 422 and body.get("code") == "PREVIEW_ROLE_NOT_FOUND", f"{code} {body.get('code', '')}")
    else:
        print("  ⚠️  xưởng không có vai nào để thử chéo tenant — bỏ qua (không tính đạt)")
        gate.failures.append("vai của xưởng (không có vai để thử)")
    code, body = start_preview(base, adm, role_id)
    gate.check("vai của KHÁCH qua token NHÀ (không phiên dựng) ⇒ 422 PREVIEW_ROLE_NOT_FOUND",
               code == 422 and body.get("code") == "PREVIEW_ROLE_NOT_FOUND", f"{code} {body.get('code', '')}")

    print("\n4. Thoát + Nhật ký của khách")
    code, _ = _req("DELETE", base + "/preview-sessions/current?reason=exit", pv)
    gate.check("DELETE /preview-sessions/current ⇒ 204", code == 204, str(code))
    gate.check("token sau khi thoát ⇒ 401", alive(base, pv) == 401)
    ok, detail = False, "không đọc được Nhật ký"
    for _ in range(10):  # audit is written asynchronously
        code, body = _req("GET", base + "/audit-log?" + urllib.parse.urlencode(
            {"entityName": "__preview__", "recordId": session_id, "limit": "50"}), ws)
        if code == 200:
            rows = body.get("data") or []
            ok, detail = audit_ok(rows, session_id, builder_id)
            if ok:
                ok, detail = audit_label_ok(rows, session_id, a.workshop_name)
            if ok:
                break
        time.sleep(0.5)
    gate.check("Nhật ký của khách: create + delete mang tên người dựng kèm '(Tên xưởng)'", ok, detail)

    print("\n5. Đóng phiên dựng ('Về Nexus') và đăng xuất khỏi xưởng")
    code, body = start_preview(base, ws, role_id)
    pv2 = (body.get("data") or {}).get("token")
    gate.check("mở phiên xem thử thứ hai", code == 201 and bool(pv2), str(code))
    if pv2:
        gate.check("phiên thứ hai đang sống", alive(base, pv2) == 200)
        code, _ = _req("DELETE", base + "/admin/workspace-sessions/current", ws)
        gate.check("DELETE /admin/workspace-sessions/current ⇒ 204", code == 204, str(code))
        gate.check("đóng phiên dựng ⇒ request xem thử kế tiếp 401", alive(base, pv2) == 401)

    adm2, refresh2, _ = login(base, a.tenant, a.admin, a.admin_pass)
    ws2 = open_workspace(base, adm2, slug)
    code, body = start_preview(base, ws2, role_id)
    pv3 = (body.get("data") or {}).get("token")
    gate.check("mở phiên xem thử thứ ba", code == 201 and bool(pv3), str(code))
    if pv3:
        _req("POST", base + "/auth/logout", None, {"refreshToken": refresh2})
        gate.check("người dựng đăng xuất khỏi xưởng ⇒ request xem thử kế tiếp 401", alive(base, pv3) == 401)


# ── --scan-get (specs/064 S9-3, T063): "a GET never writes", provable instead of signed ─────────────────────────
# The write scan in Go proves every non-GET is refused. A GET is allowed through, so a handler that WRITES on GET
# (access log, "recently viewed", a cache row) or that dies on the synthetic subject (it has no users row) is only
# visible on a live server: scan every GET with a preview token, then count the principal in the database.

TENANT_ID_RE = r"^[A-Za-z0-9_-]{1,64}$"
IDENT_RE = r"^[A-Za-z0-9_]{1,64}$"
# The one place the synthetic principal legitimately lives: the preview session row that names it.
PRINCIPAL_EXEMPT = {("sys_preview_sessions", "principal_id")}
TEXT_TYPES = ("char", "varchar", "tinytext", "text", "mediumtext", "longtext")
SHARED_DEV_DB = "nexus_platform"


def read_routes(text):
    """Paths (without /api/v1) of the `protected` lines of the route list the Go test writes."""
    out = []
    for line in text.splitlines():
        kind, _, path = line.strip().partition(" ")
        if kind == "protected" and path.startswith("/api/v1"):
            out.append(path[len("/api/v1"):])
    return out


def fill_params(path, entity, rec_id, app, ids=None):
    """Real values for the params the engine knows (:entity, :id/:recordId, app :name), a placeholder for the rest.
    `ids` maps a top-level resource ("reports", "dashboards", "approvals") to one real id of it, so
    /reports/:id/run reaches the handler body instead of dying at "no such id"."""
    own = (ids or {}).get(path.split("/")[1] if path.startswith("/") else "", rec_id)
    values = {":entity": entity, ":id": own, ":recordId": rec_id, ":name": app, ":version": "1"}
    out = "/".join(values.get(seg, "x") if seg.startswith(":") else seg for seg in path.split("/"))
    return out + "?q=Nguyen" if out == "/search" else out


def first_id(body, key):
    """id of the first row of a list response: {"data": [..]} or {"data": {"<key>": [..]}}; None when empty."""
    rows = (body or {}).get("data")
    if isinstance(rows, dict):
        rows = rows.get(key)
    if isinstance(rows, list) and rows and isinstance(rows[0], dict):
        return rows[0].get("id") or None
    return None


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):  # an SSO start must not take the scan off this machine
        return None


def scan_get(base, token, paths, seen=None):
    """GET every path with the preview token; return [(path, status)] for each 5xx. 403/404/422 are the role
    speaking; a redirect is a answer, not followed. A server that cannot be reached is an EnvError, never a pass.
    `seen`, if given, is filled with {status class: count} so the report shows how far the scan reached (a route
    that only ever answers 404 for its placeholder id was not exercised past routing)."""
    opener = urllib.request.build_opener(_NoRedirect)
    bad = []
    for path in paths:
        req = urllib.request.Request(base + path, headers={"Authorization": "Bearer " + token}, method="GET")
        try:
            with opener.open(req, timeout=30) as r:
                code = r.status
        except urllib.error.HTTPError as e:
            code = e.code
        except Exception as e:
            raise EnvError(f"không gọi được GET {base + path} — {e}") from e
        if seen is not None:
            seen[f"{code // 100}xx"] = seen.get(f"{code // 100}xx", 0) + 1
        if code >= 500:
            bad.append((path, code))
    return bad


def parse_dsn(dsn):
    """`user:pass@tcp(host[:port])/db[?opts]` (Go driver style, the shape of DATABASE_URL). The shared dev DB is refused."""
    import re
    m = re.match(r"^([^:@]*):([^@]*)@tcp\(([^:)]+)(?::(\d+))?\)/([A-Za-z0-9_]+)(?:\?.*)?$", dsn or "")
    if not m:
        raise ValueError("NEXUS_ACCEPT_MYSQL_DSN phải có dạng user:pass@tcp(host:port)/tên_db")
    user, password, host, port, db = m.groups()
    if db == SHARED_DEV_DB:
        raise ValueError(f"DSN trỏ DB dùng chung {SHARED_DEV_DB} — dùng DB bản sao nexus_wt_<lát>")
    return {"user": user, "password": password, "host": host, "port": port or "3306", "db": db}


def make_mysql_runner(dsn):
    """run_sql(sql) -> rows (lists of str) through the `mysql` client on PATH, else through the dev compose
    container (NEXUS_ACCEPT_MYSQL_CONTAINER, default nexus-mysql). No Python driver: the gate stays stdlib-only."""
    import shutil
    import subprocess
    d = parse_dsn(dsn)
    env = dict(os.environ, MYSQL_PWD=d["password"])
    if shutil.which("mysql"):
        cmd = ["mysql", "--protocol=TCP", "-h", d["host"], "-P", d["port"], "-u", d["user"], "-N", "-B", d["db"]]  # TCP: "localhost" would pick the unix socket and ignore -P
    else:
        box = os.environ.get("NEXUS_ACCEPT_MYSQL_CONTAINER", "nexus-mysql")
        cmd = ["docker", "exec", "-i", "-e", "MYSQL_PWD=" + d["password"], box, "mysql", "-u", d["user"], "-N", "-B", d["db"]]

    def run(sql):
        p = subprocess.run(cmd, input=sql, capture_output=True, text=True, timeout=600, env=env)
        if p.returncode != 0:
            raise EnvError(f"mysql lỗi: {p.stderr.strip()[:300]}")
        return [line.split("\t") for line in p.stdout.splitlines() if line]
    return run


LIST_COLUMNS_SQL = (
    "SELECT c.table_name, c.column_name, c.data_type FROM information_schema.columns c "
    "WHERE c.table_schema = DATABASE() "
    "AND c.table_name IN (SELECT table_name FROM information_schema.columns WHERE table_schema = DATABASE() AND column_name = 'tenant_id') "
    "AND c.table_name IN (SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE') "
    "AND c.column_name <> 'tenant_id' "
    "AND c.data_type IN ('char','varchar','tinytext','text','mediumtext','longtext','json') ORDER BY 1, 2"
)


def principal_hits(run_sql, tenant_id, principal, stats=None):
    """[(table, column, n)] for every column that holds the synthetic principal in the tenant's rows. Columns come from
    information_schema (never a hand list: that misses updated_by, owner_id, claimed_by, ...); every string column
    is compared with `=`, every JSON column with JSON_SEARCH. audit_log is NOT exempt. Identifiers and values are
    validated, then quoted - the client cannot bind parameters, so nothing unchecked reaches the SQL text.
    An empty column list is an EnvError (a DB with no tenant tables would "pass" every scan). `stats`, if given,
    receives {"tables": n, "columns": n} so the report shows how much was looked at."""
    import re
    for v in (tenant_id, principal):
        if not re.match(TENANT_ID_RE, v or ""):
            raise ValueError(f"giá trị không hợp lệ cho SQL: {v!r}")
    cols = []
    for table, col, typ in run_sql(LIST_COLUMNS_SQL):
        if not (re.match(IDENT_RE, table) and re.match(IDENT_RE, col)):
            raise ValueError(f"tên bảng/cột lạ: {table}.{col}")
        if (table, col) not in PRINCIPAL_EXEMPT:
            cols.append((table, col, typ))
    if not cols:
        raise EnvError("DSN không có bảng nào mang tenant_id — không phải DB của server đang quét")
    if stats is not None:
        stats.update(tables=len({t for t, _, _ in cols}), columns=len(cols))
    stmts = []
    for table, col, typ in cols:
        test = (f"JSON_SEARCH(`{col}`, 'one', '{principal}') IS NOT NULL" if typ == "json" else f"`{col}` = '{principal}'")
        stmts.append(f"SELECT '{table}','{col}',COUNT(*) FROM `{table}` WHERE tenant_id = '{tenant_id}' AND {test}")
    hits = []
    for i in range(0, len(stmts), 200):
        for table, col, n in run_sql(" UNION ALL ".join(stmts[i:i + 200]) + ";\n"):
            if int(n) > 0:
                hits.append((table, col, int(n)))
    return hits


def session_anchor(run_sql, tenant_id, session_id):
    """Rows of sys_preview_sessions with this id in this tenant, not yet ended. The preview session this run just
    opened lives in the DB the server writes, so exactly 1 here proves the DSN is that DB; 0 means the DSN points
    elsewhere (another clone, an empty DB) and every "no hit" count would be vacuous. Values are validated, then quoted."""
    import re
    for v in (tenant_id, session_id):
        if not re.match(TENANT_ID_RE, v or ""):
            raise ValueError(f"giá trị không hợp lệ cho SQL: {v!r}")
    rows = run_sql(f"SELECT COUNT(*) FROM sys_preview_sessions WHERE tenant_id = '{tenant_id}' AND id = '{session_id}' AND ended_at IS NULL;")
    return int(rows[0][0]) if rows else 0


def go_route_list(path=None):
    """The GET route list: `path` if given, else written fresh by the Go test (same route table the write scan walks)."""
    import subprocess
    import tempfile
    if path:
        return read_routes(open(path, encoding="utf-8").read())
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    with tempfile.TemporaryDirectory() as tmp:
        out = os.path.join(tmp, "routes.txt")
        p = subprocess.run(["go", "test", "./internal/handlers", "-run", "TestPreviewRouteScan_GetRoutes", "-count=1", "-args", "-routes-out=" + out],
                           cwd=root, capture_output=True, text=True, timeout=600)
        if p.returncode != 0 or not os.path.exists(out):
            raise EnvError("không sinh được danh sách route GET: " + (p.stdout + p.stderr)[-300:])
        return read_routes(open(out, encoding="utf-8").read())


def run_scan_get(a, gate):
    base = a.base.rstrip("/")
    print("\n7. Quét mọi GET bằng token xem thử (không 5xx) và đếm chủ thể tổng hợp trong DB")
    # Logins are rate-limited (AUTH_RATELIMIT_MAX_ATTEMPTS, default 10/min) and steps 1-6 spend 8 of them:
    # reuse the staff session from step 1 (the builder's was revoked by step 6's password change).
    adm, _, _ = login(base, a.tenant, a.admin, a.admin_pass)
    staff, staff_user = getattr(a, "staff_session", None) or login(base, a.tenant, a.staff, a.staff_pass)[::2]
    role_id = staff_user.get("roleId")
    if not role_id:
        raise EnvError(f"{a.staff} không có vai")
    code, body = start_preview(base, adm, role_id)
    pv = (body.get("data") or {}).get("token")
    session_id = (body.get("data") or {}).get("sessionId")
    if code != 201 or not pv or not session_id:
        raise EnvError(f"không mở được phiên xem thử để quét ({code} {body.get('code', '')})")
    try:
        _, me = _req("GET", base + "/auth/me", pv)
        me = me.get("data") or {}
        principal, tenant_id = me.get("id"), me.get("tenantId")
        _, ids = record_ids(base, staff, a.entity, cap=1)
        rec_id = next(iter(ids), "x")
        _, apps = _req("GET", base + "/apps", staff)
        app = (app_names(apps) or ["x"])[0]
        ids = {}
        for res, listing in (("reports", "/reports"), ("dashboards", "/dashboards"), ("approvals", "/approvals/pending")):
            _, lb = _req("GET", base + listing, staff)
            if first_id(lb, res):
                ids[res] = first_id(lb, res)
        print(f"     id thật: {sorted(ids) or 'không có'} · bản ghi {a.entity} · app {app}")
        paths = [fill_params(r, a.entity, rec_id, app, ids) for r in go_route_list(a.routes_file)]
        seen = {}
        bad = scan_get(base, pv, paths, seen)
        gate.check(f"{len(paths)} route GET với token xem thử ({', '.join(f'{k}: {v}' for k, v in sorted(seen.items()))}): 0 lỗi 5xx", not bad,
                   "; ".join(f"{p} → {c}" for p, c in bad) if bad else "")
        if not a.mysql_dsn:
            gate.check("kiểm DB: không dòng nào mang chủ thể tổng hợp", False,
                       "thiếu NEXUS_ACCEPT_MYSQL_DSN (DB bản sao) — không chứng minh được 'GET không ghi'")
        else:
            run_sql = make_mysql_runner(a.mysql_dsn)
            # Control first: the open session must be visible through this DSN, else the count below proves nothing.
            if session_anchor(run_sql, tenant_id, session_id) != 1:
                raise EnvError("DSN không phải DB của server đang quét: phiên xem thử vừa mở không có trong sys_preview_sessions")
            stats = {}
            hits = principal_hits(run_sql, tenant_id, principal, stats)
            gate.check(f"kiểm DB ({stats['tables']} bảng / {stats['columns']} cột chuỗi+JSON có tenant_id; phiên xem thử thấy trong DB): không dòng nào mang chủ thể tổng hợp", not hits,
                       "; ".join(f"{t}.{c} = {n} dòng" for t, c, n in hits) if hits else "")
    finally:
        _req("DELETE", base + "/preview-sessions/current?reason=exit", pv)


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--base", required=True, help="server riêng + DB bản sao, vd http://localhost:8163/api/v1 (bắt buộc: script GHI)")
    p.add_argument("--tenant", default=DEFAULTS["tenant"])
    p.add_argument("--admin", default=DEFAULTS["admin"])
    p.add_argument("--staff", default=DEFAULTS["staff"])
    p.add_argument("--entity", default="crm_accounts", help="bảng thử POST bị chặn (mặc định crm_accounts)")
    p.add_argument("--workspace", help="slug khách: chạy chế độ S9-8 (xem như vai TRONG không gian của khách) thay cho 1-6")
    p.add_argument("--workspace-staff", default="test@example.com", help="nhân viên thật của khách <workspace> để so trùng (mật khẩu: NEXUS_WS_STAFF_PASS)")
    p.add_argument("--workshop-name", help="tên xưởng mong đợi trong nhãn Nhật ký, vd Nexus (mặc định: chỉ kiểm dạng 'Họ tên (Tên xưởng)')")
    p.add_argument("--allow-remote-write", action="store_true")
    p.add_argument("--scan-get", action="store_true",
                   help="quét mọi route GET bằng token xem thử (0 lỗi 5xx) + đếm chủ thể tổng hợp trong DB bản sao (NEXUS_ACCEPT_MYSQL_DSN)")
    p.add_argument("--routes-file", help="danh sách route GET sinh sẵn (mặc định: chạy go test TestPreviewRouteScan_GetRoutes)")
    a = p.parse_args()
    a.admin_pass, a.staff_pass = DEFAULTS["admin_pass"], DEFAULTS["staff_pass"]
    a.workspace_staff_pass = os.environ.get("NEXUS_WS_STAFF_PASS") or DEFAULTS["staff_pass"]
    a.mysql_dsn = os.environ.get("NEXUS_ACCEPT_MYSQL_DSN", "")
    if not (is_loopback(a.base) or a.allow_remote_write):
        print("Từ chối: script GHI (tài khoản tạm, đổi mật khẩu). --base phải là loopback hoặc có --allow-remote-write.", file=sys.stderr)
        return 2
    if is_shared_dev(a.base):
        print(f"Từ chối: :{SHARED_DEV_PORT} là server dev dùng chung — script GHI (đổi mật khẩu người dựng, tài khoản tạm). Dựng server riêng trên DB bản sao.", file=sys.stderr)
        return 2
    for s in (signal.SIGTERM, signal.SIGHUP):
        signal.signal(s, lambda signum, _f: sys.exit(128 + signum))
    gate = Gate()
    try:
        if a.workspace:
            run_workspace(a, gate)
        else:
            run(a, gate)
        if a.scan_get:
            run_scan_get(a, gate)
    except (EnvError, ValueError) as e:
        print(f"\nLỖI MÔI TRƯỜNG: {e}", file=sys.stderr)
        return 2
    if gate.failures:
        print(f"\n❌ CHƯA ĐẠT — {len(gate.failures)} điểm: " + "; ".join(gate.failures))
        return 1
    print("\n✅ ĐẠT — token xem thử thấy đúng như nhân viên thật, không ghi được, chết theo phiên người dựng.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
