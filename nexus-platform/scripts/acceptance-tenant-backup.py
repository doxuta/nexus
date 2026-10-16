#!/usr/bin/env python3
"""Nghiệm thu NGƯỜI-THỨ-BA của sao lưu / khôi phục từng khách (specs/068-tenant-backup-restore).

Vì sao cần script này: người vận hành xưởng và admin khách đều `is_admin`, nên "tôi bấm thử thấy
chạy" đúng theo cấu tạo kể cả khi nhân viên thật không còn vào được app sau khôi phục. Script ép
nhân viên KHÔNG-admin của khách có mặt ở cả hai đầu của vòng:

  1. bảng 403: admin khách và nhân viên đều bị từ chối trên MỌI route sao lưu / khôi phục / khoá ghi;
  2. người vận hành sao lưu khách → chờ "ready";
  3. thay đổi qua API: nhân viên đổi mật khẩu của chính mình, admin khách tạo thêm một bản ghi;
  4. người vận hành khôi phục bản đó bằng một lần gõ slug → chờ "done";
  5. nhân viên đăng nhập bằng MẬT KHẨU MỚI (danh tính giữ nguyên) và thấy đúng số bản ghi LÚC SAO LƯU;
  6. "Hoàn tác" (khôi phục dòng pre_restore) → số bản ghi quay về lúc trước khi khôi phục.

Dùng:
    NEXUS_OPERATOR_PASS=… NEXUS_ADMIN_PASS=… NEXUS_STAFF_PASS=… \\
    python3 scripts/acceptance-tenant-backup.py --base http://localhost:81xx/api/v1 \\
        --tenant qa-x --admin qa-admin-x@example.test --staff nv-test@example.test \\
        --entity crm_accounts --create-json '{"name":"acceptance"}'

Mật khẩu chỉ đi qua biến môi trường (dòng lệnh ai `ps` cũng thấy). Chỉ GHI khi `--base` là loopback
(hoặc `--allow-remote-write`): khôi phục XOÁ dữ liệu của khách. Mã thoát: 0 đạt · 1 có bước đỏ ·
2 lỗi môi trường.

ponytail: stdlib thuần, như acceptance-acl.py.
"""

import argparse
import ipaddress
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

POLL_SECONDS = 2
WAIT_SECONDS = 180


def is_loopback(base):
    """True when --base points at this machine: `localhost` or a literal loopback IP."""
    host = (urllib.parse.urlparse(base).hostname or "").lower()
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def call(method, url, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.load(e)
        except Exception:
            return e.code, {}
    except Exception as e:
        print(f"LỖI MÔI TRƯỜNG: không gọi được {url} — {e}", file=sys.stderr)
        sys.exit(2)


def login(base, tenant, email, password):
    code, body = call("POST", base + "/auth/login", body={"tenant": tenant, "email": email, "password": password})
    data = body.get("data") or {}
    if code != 200 or not data.get("accessToken"):
        return None
    return data["accessToken"]


def need(name):
    v = os.environ.get(name, "")
    if not v:
        print(f"LỖI MÔI TRƯỜNG: thiếu biến môi trường {name}.", file=sys.stderr)
        sys.exit(2)
    return v


def total_of(body):
    """Row count of a list answer: pagination.total (record lists) or meta.total, else len(data)."""
    for key in ("pagination", "meta"):
        box = body.get(key) or {}
        if isinstance(box.get("total"), int):
            return box["total"]
    data = body.get("data")
    return len(data) if isinstance(data, list) else -1


def forbidden_table(tenant_id):
    """Every route of slices A–D, as (method, path, body)."""
    b = f"/admin/tenants/{tenant_id}"
    zero = "00000000-0000-4000-8000-000000000000"
    return [
        ("GET", b + "/backups", None),
        ("POST", b + "/backups", {}),
        ("GET", f"{b}/backups/{zero}", None),
        ("GET", f"{b}/backups/{zero}/file", None),
        ("POST", b + "/maintenance", {"minutes": 5}),
        ("DELETE", b + "/maintenance", None),
        ("POST", b + "/restores", {"backupId": zero}),
        ("POST", b + "/restores", {"backupId": zero, "confirmSlug": "x", "dryRun": False}),
    ]


def wait_backup(base, op, tenant_id, backup_id):
    deadline = time.time() + WAIT_SECONDS
    while time.time() < deadline:
        code, body = call("GET", f"{base}/admin/tenants/{tenant_id}/backups/{backup_id}", op)
        row = body.get("data") or {}
        if code == 200 and row.get("status") not in (None, "running"):
            return row
        time.sleep(POLL_SECONDS)
    return {}


def wait_restore(base, op, tenant_id, restore_id):
    deadline = time.time() + WAIT_SECONDS
    while time.time() < deadline:
        code, body = call("GET", f"{base}/admin/tenants/{tenant_id}/backups/{restore_id}", op)
        r = (body.get("data") or {}).get("restore") or {}
        if code == 200 and r.get("status") in ("done", "failed"):
            return r
        time.sleep(POLL_SECONDS)
    return {}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", default="http://localhost:8080/api/v1")
    ap.add_argument("--operator-tenant", default="northwind")
    ap.add_argument("--operator", default="admin@example.com")
    ap.add_argument("--tenant", required=True, help="slug của khách được sao lưu / khôi phục")
    ap.add_argument("--admin", required=True, help="email admin của khách")
    ap.add_argument("--staff", required=True, help="email nhân viên KHÔNG-admin của khách")
    ap.add_argument("--entity", required=True, help="bảng mà nhân viên đọc được (đếm số bản ghi)")
    ap.add_argument("--create-json", default='{"name":"acceptance"}', help="body để admin khách tạo thêm một bản ghi")
    ap.add_argument("--allow-remote-write", action="store_true")
    a = ap.parse_args()

    if not is_loopback(a.base) and not a.allow_remote_write:
        print(f"TỪ CHỐI: --base {a.base} không phải loopback; khôi phục xoá dữ liệu khách. "
              "Thêm --allow-remote-write nếu đúng là cố ý.", file=sys.stderr)
        sys.exit(2)
    op_pass, admin_pass, staff_pass = need("NEXUS_OPERATOR_PASS"), need("NEXUS_ADMIN_PASS"), need("NEXUS_STAFF_PASS")
    print(f"GHI vào {a.base} · khách {a.tenant} (khôi phục thật, có hoàn tác)")

    op = login(a.base, a.operator_tenant, a.operator, op_pass)
    adm = login(a.base, a.tenant, a.admin, admin_pass)
    staff = login(a.base, a.tenant, a.staff, staff_pass)
    if not (op and adm and staff):
        print("LỖI MÔI TRƯỜNG: không đăng nhập được một trong ba tài khoản.", file=sys.stderr)
        sys.exit(2)
    code, body = call("GET", a.base + "/admin/tenants", op)
    items = body.get("data") or []
    items = items.get("items", []) if isinstance(items, dict) else items
    tenant_id = next((t.get("id") for t in items if t.get("slug") == a.tenant), None)
    if not tenant_id:
        print(f"LỖI MÔI TRƯỜNG: không thấy khách {a.tenant} trong /admin/tenants.", file=sys.stderr)
        sys.exit(2)

    failures = []

    def check(ok, what):
        print(("  ✓ " if ok else "  ✗ ") + what)
        if not ok:
            failures.append(what)

    print("1. bảng 403 — admin khách và nhân viên bị từ chối ở mọi route")
    for who, tok in (("admin khách", adm), ("nhân viên", staff)):
        for method, path, payload in forbidden_table(tenant_id):
            c, _ = call(method, a.base + path, tok, payload)
            check(c == 403, f"{who} {method} {path.replace(tenant_id, ':id')} → {c} (cần 403)")

    print("2. người vận hành sao lưu khách")
    c, body = call("POST", f"{a.base}/admin/tenants/{tenant_id}/backups", op, {"note": "acceptance"})
    backup_id = (body.get("data") or {}).get("id")
    check(c == 202 and bool(backup_id), f"POST backups → {c}")
    if not backup_id:
        sys.exit(1)
    row = wait_backup(a.base, op, tenant_id, backup_id)
    check(row.get("status") == "ready", f"bản sao lưu {row.get('status')}")

    c, body = call("GET", f"{a.base}/{a.entity}?limit=1", staff)
    at_backup = total_of(body)
    check(c == 200 and at_backup >= 0, f"nhân viên đọc {a.entity}: {at_backup} dòng lúc sao lưu")

    print("3. thay đổi qua API sau lúc sao lưu")
    new_pass = staff_pass + "-moi"
    c, _ = call("PUT", a.base + "/auth/me/password", staff, {"currentPassword": staff_pass, "newPassword": new_pass})
    check(c in (200, 204), f"nhân viên đổi mật khẩu → {c}")
    c, _ = call("POST", f"{a.base}/{a.entity}", adm, json.loads(a.create_json))
    check(c in (200, 201), f"admin khách tạo thêm 1 bản ghi → {c}")
    staff = login(a.base, a.tenant, a.staff, new_pass)
    check(staff is not None, "nhân viên đăng nhập bằng mật khẩu mới")
    if not staff:
        sys.exit(1)
    c, body = call("GET", f"{a.base}/{a.entity}?limit=1", staff)
    before_restore = total_of(body)
    check(before_restore == at_backup + 1, f"{a.entity} có {before_restore} dòng (cần {at_backup + 1})")

    print("4. khôi phục bằng một lần gõ slug")
    c, body = call("POST", f"{a.base}/admin/tenants/{tenant_id}/restores", op,
                   {"backupId": backup_id, "confirmSlug": a.tenant + "-sai", "dryRun": False})
    check(c == 422 and (body.get("code") == "TENANT_RESTORE_CONFIRM_MISMATCH"), f"gõ sai slug → {c} {body.get('code')}")
    c, body = call("POST", f"{a.base}/admin/tenants/{tenant_id}/restores", op,
                   {"backupId": backup_id, "confirmSlug": a.tenant, "dryRun": False})
    restore_id = (body.get("data") or {}).get("restoreId")
    check(c == 202 and bool(restore_id), f"khôi phục → {c} {body.get('code', '')}")
    if not restore_id:
        sys.exit(1)
    r = wait_restore(a.base, op, tenant_id, restore_id)
    check(r.get("status") == "done", f"khôi phục kết thúc: {r.get('status')}")

    print("5. nhân viên: mật khẩu MỚI vẫn dùng được, dữ liệu về lúc sao lưu")
    staff = login(a.base, a.tenant, a.staff, new_pass)
    check(staff is not None, "đăng nhập bằng mật khẩu mới (danh tính giữ nguyên)")
    if staff:
        c, body = call("GET", f"{a.base}/{a.entity}?limit=1", staff)
        after = total_of(body)
        check(c == 200 and after == at_backup, f"{a.entity} có {after} dòng (cần {at_backup})")

    print("6. Hoàn tác — khôi phục dòng pre_restore")
    c, body = call("POST", f"{a.base}/admin/tenants/{tenant_id}/restores", op,
                   {"backupId": restore_id, "confirmSlug": a.tenant, "dryRun": False})
    undo_id = (body.get("data") or {}).get("restoreId")
    check(c == 202 and bool(undo_id), f"hoàn tác → {c} {body.get('code', '')}")
    if undo_id:
        r = wait_restore(a.base, op, tenant_id, undo_id)
        check(r.get("status") == "done", f"hoàn tác kết thúc: {r.get('status')}")
        staff = login(a.base, a.tenant, a.staff, new_pass)
        if staff:
            c, body = call("GET", f"{a.base}/{a.entity}?limit=1", staff)
            after = total_of(body)
            check(after == before_restore, f"{a.entity} có {after} dòng (cần {before_restore}, như trước khi khôi phục)")

    if failures:
        print(f"\nĐỎ — {len(failures)} bước chưa đạt.")
        sys.exit(1)
    print("\nXANH — vòng sao lưu → khôi phục → hoàn tác đạt với nhân viên không-admin.")


if __name__ == "__main__":
    main()
