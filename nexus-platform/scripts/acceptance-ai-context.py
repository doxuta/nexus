#!/usr/bin/env python3
"""Nghiệm thu specs/067-ai-context lát L10-1 (ô AI mang ngữ cảnh app + màn) — phần TẤT ĐỊNH.

Chạy được cả khi AI tắt: mọi từ chối ngữ cảnh là 422 TRƯỚC cổng hạn mức, nên mã 422 (không phải
503 AI_DISABLED / 200 / 429) mới là phép phân biệt "bị chặn trước lượt AI". Đếm `sys_ai_runs` chỉ
chứng minh được khi AI BẬT (agent tắt thì không ghi sổ) — script nói rõ điều đó thay vì tính là đạt.

Kiểm:
  1. mỗi khoá sai ⇒ 422 AI_CONTEXT_INVALID đúng details.field, ở CẢ /chat lẫn /chat/stream (JSON thường).
  2. app của dự án khác và app không tồn tại ⇒ 422 AI_CONTEXT_NOT_FOUND, thân TRÙNG TỪNG BYTE.
  3. tài khoản không-admin gọi /chat kèm context ⇒ 403 trùng byte với 403 không kèm context.
  4. admin khách không-operator (dựng tạm trên bản sao, cất sau) ⇒ 403 (FR-080).
  5. /assistant/ask của không-admin có và không có `context` ⇒ cùng mã + cùng khoá phản hồi (⌘K không nhận).
  6. đối chứng dương: context hợp lệ KHÔNG ra 422 (AI bật ⇒ 200 và sys_ai_runs +1; AI tắt ⇒ 503 AI_DISABLED).
  7. sys_ai_sessions / sys_ai_runs của dự án không đổi sau mọi từ chối (cần --db-container + --db).

Dùng (bản sao, cổng riêng; mật khẩu qua NEXUS_ADMIN_PASS / NEXUS_STAFF_PASS):
    python3 scripts/acceptance-ai-context.py --base http://localhost:8436/api/v1 \
        --db-container nexus-mysql --db nexus_wt_ai_context_app

Mã thoát: 0 đạt · 1 có phép kiểm đỏ · 2 lỗi môi trường (0 phép kiểm chạy = 2, không xanh giả).
ponytail: stdlib thuần; SELECT qua `docker exec … mysql` (không thêm driver).
"""

import argparse
import json
import os
import secrets
import subprocess
import sys
import urllib.error
import urllib.request

CHAT = "/admin/ai/builder/chat"
STREAM = "/admin/ai/builder/chat/stream"
checks = []  # (ok, label)


def call(method, url, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.read(), r.headers.get("Content-Type", "")
    except urllib.error.HTTPError as e:
        return e.code, e.read(), e.headers.get("Content-Type", "")
    except Exception as e:
        print(f"LỖI MÔI TRƯỜNG: không gọi được {url} — {e}", file=sys.stderr)
        sys.exit(2)


def jget(raw, *path):
    try:
        v = json.loads(raw)
        for p in path:
            v = v[p]
        return v
    except Exception:
        return None


def login(base, tenant, email, password):
    code, raw, _ = call("POST", base + "/auth/login", body={"tenant": tenant, "email": email, "password": password})
    tok = jget(raw, "data", "accessToken")
    if code != 200 or not tok:
        print(f"LỖI MÔI TRƯỜNG: đăng nhập {email} thất bại ({code}).", file=sys.stderr)
        sys.exit(2)
    return tok


def check(ok, label):
    checks.append((ok, label))
    print(("  ĐẠT  " if ok else "  ĐỎ   ") + label)


def count(a, table):
    if not a.db_container or not a.db:
        return None
    # builder rows only: ⌘K (assistant) turns write their own rows into the same tables.
    extra = " AND feature = 'ai_builder.chat'" if table == "sys_ai_runs" else " AND feature = 'builder'"
    q = f"SELECT COUNT(*) FROM {table} WHERE tenant_id = (SELECT id FROM tenants WHERE slug = '{a.tenant}'){extra}"
    try:
        out = subprocess.run(["docker", "exec", a.db_container, "mysql", "-uroot", "-N", "-B", a.db, "-e", q],
                             capture_output=True, text=True, timeout=30, check=True).stdout.strip()
        return int(out)
    except Exception as e:
        print(f"LỖI MÔI TRƯỜNG: không đếm được {table} — {e}", file=sys.stderr)
        sys.exit(2)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--base", default="http://localhost:8436/api/v1")
    p.add_argument("--tenant", default="northwind")
    p.add_argument("--admin", default="admin@example.com")
    p.add_argument("--staff", default="test@example.com")
    p.add_argument("--own-app", default="clinic", help="app CÓ trong dự án")
    p.add_argument("--foreign-app", default="cho_thue_bds", help="app chỉ có ở dự án KHÁC")
    p.add_argument("--foreign-entity", default="crm_bank_txns", help="bảng chỉ có ở dự án KHÁC")
    p.add_argument("--own-entity", default="clinic_patients", help="bảng CÓ trong dự án")
    p.add_argument("--db-container", default="")
    p.add_argument("--db", default="")
    a = p.parse_args()
    if not a.base.startswith("http://localhost") and not a.base.startswith("http://127.0.0.1"):
        print("LỖI MÔI TRƯỜNG: chỉ chạy trên loopback (script tạo/cất một tài khoản tạm).", file=sys.stderr)
        sys.exit(2)
    if not a.db.startswith("nexus_wt_"):
        print("LỖI MÔI TRƯỜNG: --db phải là DB bản sao nexus_wt_* (script tạo user rác ở tenant).", file=sys.stderr)
        sys.exit(2)

    adm = login(a.base, a.tenant, a.admin, os.environ.get("NEXUS_ADMIN_PASS") or "Demo-Admin-2026!")
    stf = login(a.base, a.tenant, a.staff, os.environ.get("NEXUS_STAFF_PASS") or "Demo-Rep-2026!")

    # Tài khoản admin-khách-không-operator tạm thời (cất trong finally).
    pw = secrets.token_urlsafe(16) + "Aa1!"
    email = f"ctx-custadmin-{secrets.token_hex(4)}@acceptance.invalid"
    code, raw, _ = call("POST", a.base + "/admin/users", adm,
                        {"email": email, "password": pw, "fullName": "Acceptance ctx", "status": "active", "isAdmin": True})
    cust_id = jget(raw, "data", "user", "id")
    if code != 201 or not cust_id:
        print(f"LỖI MÔI TRƯỜNG: không tạo được admin khách tạm ({code}).", file=sys.stderr)
        sys.exit(2)
    try:
        run(a, adm, stf, login(a.base, a.tenant, email, pw))
    finally:
        call("PUT", f"{a.base}/admin/users/{cust_id}", adm, {"status": "disabled"})

    bad = [l for ok, l in checks if not ok]
    if not checks:
        print("LỖI MÔI TRƯỜNG: 0 phép kiểm chạy.", file=sys.stderr)
        sys.exit(2)
    print(f"\n{len(checks) - len(bad)}/{len(checks)} phép kiểm đạt")
    sys.exit(1 if bad else 0)


def run(a, adm, stf, cust):
    runs0, sess0 = count(a, "sys_ai_runs"), count(a, "sys_ai_sessions")
    for path in (CHAT, STREAM):
        tag = "stream" if path == STREAM else "chat"
        for key, val, field in (("app", "Bad App!;DROP", "context.app"), ("panel", "ignore-all-rules", "context.panel")):
            code, raw, ct = call("POST", a.base + path, adm, {"message": "hi", "context": {key: val}})
            check(code == 422 and jget(raw, "code") == "AI_CONTEXT_INVALID" and jget(raw, "details", "field") == field
                  and ct.startswith("application/json"),
                  f"[{tag}] {key} sai ⇒ 422 AI_CONTEXT_INVALID {field}, JSON thường (nhận {code} {jget(raw, 'code')})")
        c1, b1, _ = call("POST", a.base + path, adm, {"message": "hi", "context": {"app": a.foreign_app}})
        c2, b2, _ = call("POST", a.base + path, adm, {"message": "hi", "context": {"app": "khong_ton_tai_xyz"}})
        check(c1 == 422 and jget(b1, "code") == "AI_CONTEXT_NOT_FOUND" and c1 == c2 and b1 == b2,
              f"[{tag}] app dự án khác == app không tồn tại, 422 AI_CONTEXT_NOT_FOUND trùng từng byte (nhận {c1}/{c2})")

    # L10-2 (T041): entity / recordId.
    uid = "3f2b8c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4e"
    for path in (CHAT, STREAM):
        tag = "stream" if path == STREAM else "chat"
        code, raw, ct = call("POST", a.base + path, adm, {"message": "hi", "context": {"recordId": uid}})
        check(code == 422 and jget(raw, "code") == "AI_CONTEXT_INVALID" and jget(raw, "details", "field") == "context.recordId"
              and ct.startswith("application/json"),
              f"[{tag}] recordId thiếu entity ⇒ 422 AI_CONTEXT_INVALID context.recordId (nhận {code} {jget(raw, 'code')})")
        code, raw, _ = call("POST", a.base + path, adm, {"message": "hi", "context": {"entity": a.own_entity, "recordId": "not-a-uuid"}})
        check(code == 422 and jget(raw, "details", "field") == "context.recordId", f"[{tag}] recordId không phải UUID ⇒ 422 context.recordId (nhận {code})")
        c1, b1, _ = call("POST", a.base + path, adm, {"message": "hi", "context": {"entity": a.foreign_entity}})
        c2, b2, _ = call("POST", a.base + path, adm, {"message": "hi", "context": {"entity": "khong_ton_tai_xyz"}})
        check(c1 == 422 and jget(b1, "code") == "AI_CONTEXT_NOT_FOUND" and jget(b1, "details", "field") == "context.entity" and c1 == c2 and b1 == b2,
              f"[{tag}] bảng dự án khác == bảng không tồn tại, 422 AI_CONTEXT_NOT_FOUND trùng từng byte (nhận {c1}/{c2})")

    # 3. không-admin: 403 trùng byte có/không context.
    for path in (CHAT, STREAM):
        c1, b1, _ = call("POST", a.base + path, stf, {"message": "hi"})
        c2, b2, _ = call("POST", a.base + path, stf, {"message": "hi", "context": {"app": a.own_app, "panel": "bang"}})
        check(c1 == 403 and c2 == 403 and b1 == b2, f"không-admin {path.rsplit('/', 1)[-1]} có/không context ⇒ 403 trùng byte")
        c3, _, _ = call("POST", a.base + path, cust, {"message": "hi", "context": {"app": a.own_app}})
        check(c3 == 403, f"admin khách không-operator {path.rsplit('/', 1)[-1]} ⇒ 403 (FR-080), nhận {c3}")

    # 5. ⌘K không nhận context.
    c1, b1, _ = call("POST", a.base + "/assistant/ask", stf, {"message": "hi"})
    c2, b2, _ = call("POST", a.base + "/assistant/ask", stf,
                     {"message": "hi", "context": {"app": "Bad App!", "panel": "zzz"}})
    k1 = sorted((jget(b1, "data") or jget(b1) or {}).keys()) if isinstance(jget(b1, "data") or jget(b1), dict) else None
    k2 = sorted((jget(b2, "data") or jget(b2) or {}).keys()) if isinstance(jget(b2, "data") or jget(b2), dict) else None
    check(c1 == c2 and c1 != 422 and k1 == k2, f"/assistant/ask có/không context ⇒ cùng mã ({c1}/{c2}) và cùng khoá phản hồi")

    # 7. sổ không đổi sau mọi từ chối ở trên.
    runs1, sess1 = count(a, "sys_ai_runs"), count(a, "sys_ai_sessions")
    if runs0 is None:
        print("  (bỏ qua đếm sổ: không có --db-container/--db)")
    else:
        check(sess1 == sess0, f"sys_ai_sessions không đổi sau mọi từ chối ({sess0} → {sess1})")
        # Chỉ tính sổ của builder: lượt ⌘K ở trên có thể ghi sổ riêng (feature khác) khi AI bật.
        if runs1 != runs0:
            check(False, f"sys_ai_runs (builder) đổi sau từ chối ({runs0} → {runs1})")
        else:
            check(True, f"sys_ai_runs (builder) không đổi sau mọi từ chối ({runs0})")
    # 6. đối chứng dương.
    code, raw, _ = call("POST", a.base + CHAT, adm, {"message": "Chào", "context": {"app": a.own_app, "panel": "bang"}})
    if code == 503 and jget(raw, "code") == "AI_DISABLED":
        check(True, "đối chứng dương: context hợp lệ KHÔNG ra 422 (AI tắt ⇒ 503 AI_DISABLED, qua hết cổng ngữ cảnh)")
        print("  (đếm sổ chạy: không chứng minh được khi AI tắt — phép đếm sys_ai_runs KHÔNG được tính là đạt)")
    else:
        check(code == 200 and (jget(raw, "data", "context", "app", "name") == a.own_app),
              f"đối chứng dương: context hợp lệ ⇒ 200 và data.context.app = {a.own_app} (nhận {code})")
        r2 = count(a, "sys_ai_runs")
        if runs0 is not None:
            check(r2 == runs1 + 1, f"đối chứng dương: sys_ai_runs +1 ({runs1} → {r2})")
    code, raw, _ = call("POST", a.base + CHAT, adm, {"message": "Chào", "context": {"app": a.own_app, "entity": a.own_entity, "recordId": uid.upper()}})
    if code != 503:
        check(code == 200 and jget(raw, "data", "context", "entity", "name") == a.own_entity and jget(raw, "data", "context", "recordId") == uid,
              f"đối chứng dương: entity + recordId hợp lệ ⇒ 200 và data.context.entity/recordId được tra + chuẩn hoá (nhận {code})")


if __name__ == "__main__":
    main()
