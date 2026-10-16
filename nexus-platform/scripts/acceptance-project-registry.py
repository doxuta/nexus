#!/usr/bin/env python3
"""Nghiệm thu sổ dự án (specs/062-customer-workspace L5, #377) bằng người dựng thật + người thứ ba thật.

Vì sao cần: người dựng là admin, và admin bỏ qua toàn bộ ACL — "tôi thử thấy chạy" đúng theo cấu tạo kể cả khi
cổng `RequirePlatformOperator` hỏng. Cổng chỉ chứng minh được bằng những người KHÔNG được vào:

  1. Người dựng (`admin@example.com`): `GET /admin/projects` — xưởng `northwind` KHÔNG có mặt; mọi dòng có `stage`,
     `health.status` và đủ 10 khoá `counts`; khách bị treo vẫn hiện kèm `tenantStatus`.
  2. `PATCH` — trial:true ⇒ `stage:"trial"`, trial:false ⇒ `building`; ngành 65 ký tự ⇒ 422
     `PROJECT_INDUSTRY_TOO_LONG`; ngành đặt xong đọc lại đúng; khách lạ ⇒ 404 `TENANT_NOT_FOUND`.
  3. `POST …/health-check` ⇒ 200 và bản chụp được LƯU (GET sau đó thấy `checkedAt`).
  4. Nhật ký xưởng có `__project__` (PATCH + health-check) mang tên người dựng.
  5. Người thứ ba: `test@example.com` (nhân viên không-admin), token phiên dựng (`POST /admin/workspace-sessions`) và
     không-token gọi 3 route ⇒ 403/403/401, thân 403 GIỐNG TỪNG BYTE thân 403 của `GET /admin/tenants` (route
     operator-only cũ). Phiên dựng được đóng lại trong `finally`.
  6. `--bench N`: đo N lần `GET /admin/projects`, in p50/p95 (ngưỡng p95 < 300 ms với 50 khách — dựng 50 khách
     giả bằng SQL trên DB bản sao trước).

Dùng (server riêng + DB bản sao, KHÔNG :8090 chung — script từ chối cổng đó; script GHI: đổi ngành/Chạy thử của một
khách rồi trả lại như cũ, mở + đóng một phiên dựng):
    python3 scripts/acceptance-project-registry.py --base http://localhost:8410/api/v1 --target-slug demo --bench 20

Mật khẩu: `NEXUS_ADMIN_PASS` / `NEXUS_STAFF_PASS`, thiếu thì rơi về tài khoản seed của tenant dev.
Mã thoát: 0 = đạt · 1 = lệch · 2 = lỗi môi trường.

ponytail: stdlib thuần, khuôn `acceptance-preview-role.py`.
"""

import argparse
import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

COUNT_KEYS = {"apps", "entities", "customLayouts", "workflows", "approvals", "roles", "users", "staffUsers", "reports", "dashboards"}


def call(base, method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            raw = r.read()
            return r.status, raw
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:  # server down, DNS, timeout
        print(f"LỖI MÔI TRƯỜNG: không gọi được {base + path} — {e}", file=sys.stderr)
        sys.exit(2)


def jcall(base, method, path, token=None, body=None):
    code, raw = call(base, method, path, token, body)
    try:
        return code, json.loads(raw) if raw else {}
    except ValueError:
        return code, {"_raw": raw.decode(errors="replace")}


def login(base, tenant, email, password):
    code, body = jcall(base, "POST", "/auth/login", body={"tenant": tenant, "email": email, "password": password})
    data = body.get("data") or {}
    if code != 200 or not data.get("accessToken"):
        print(f"LỖI MÔI TRƯỜNG: đăng nhập {email} thất bại ({code} {body.get('code', '')}).", file=sys.stderr)
        sys.exit(2)
    return data["accessToken"]


class Check:
    def __init__(self):
        self.failed = 0

    def ok(self, cond, msg):
        print(("  ĐẠT  " if cond else "  LỆCH ") + msg)
        if not cond:
            self.failed += 1


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--tenant", default="northwind")
    ap.add_argument("--admin", default="admin@example.com")
    ap.add_argument("--staff", default="test@example.com")
    ap.add_argument("--target-slug", default="demo", help="khách dùng để PATCH / khám (phải tồn tại, chưa ngưng)")
    ap.add_argument("--bench", type=int, default=0)
    a = ap.parse_args()
    if urllib.parse.urlparse(a.base).port == 8090:
        print("LỖI MÔI TRƯỜNG: từ chối :8090 (DB dùng chung) — dùng server riêng + DB bản sao.", file=sys.stderr)
        sys.exit(2)
    base = a.base.rstrip("/")
    admin_tok = login(base, a.tenant, a.admin, os.environ.get("NEXUS_ADMIN_PASS") or "Demo-Admin-2026!")
    staff_tok = login(base, a.tenant, a.staff, os.environ.get("NEXUS_STAFF_PASS") or "Demo-Rep-2026!")
    c = Check()
    ws_session = None

    try:
        print("1. Danh sách")
        code, body = jcall(base, "GET", "/admin/projects", admin_tok)
        c.ok(code == 200, f"GET /admin/projects = {code}")
        rows = body.get("data") or []
        slugs = {r["slug"]: r for r in rows}
        c.ok(a.tenant not in slugs, f"xưởng '{a.tenant}' KHÔNG có trong danh sách ({len(rows)} dòng)")
        c.ok(all(r.get("stage") in ("building", "trial", "live", "needs_attention") for r in rows), "mọi dòng có stage hợp lệ")
        c.ok(all(set(r.get("counts", {})) == COUNT_KEYS for r in rows), "mọi dòng có đủ 10 khoá counts")
        c.ok(all((r.get("health") or {}).get("status") in ("ok", "warn", "critical", "unknown") for r in rows), "mọi dòng có health.status")
        sus = [r for r in rows if r.get("tenantStatus") == "suspended"]
        c.ok(all(r["slug"] != a.tenant for r in sus), f"khách bị treo vẫn hiện ({len(sus)} dòng)")
        target = slugs.get(a.target_slug)
        if target is None:
            print(f"LỖI MÔI TRƯỜNG: không có khách '{a.target_slug}'.", file=sys.stderr)
            sys.exit(2)
        tid = target["tenantId"]
        was_trial = target.get("trialSince") is not None
        was_industry = target.get("industry")

        _, pre = jcall(base, "GET", "/audit-log?entityName=__project__&limit=100", admin_tok)
        seen_before = {e.get("id") for e in (pre.get("data") or [])}  # rows of earlier runs are not this run's evidence

        print("2. PATCH")
        code, body = jcall(base, "PATCH", f"/admin/projects/{tid}", admin_tok, {"trial": True})
        c.ok(code == 200 and body["data"]["stage"] in ("trial", "live", "needs_attention") or code == 422,
             f"trial:true = {code}, stage={((body.get('data') or {}).get('stage'))}")
        if code == 200:
            c.ok(body["data"]["stage"] == "trial" or body["data"]["deliveredAt"] is not None, "chưa giao ⇒ stage trial")
        code, body = jcall(base, "PATCH", f"/admin/projects/{tid}", admin_tok, {"trial": False})
        c.ok(code == 200 and body["data"]["trialSince"] is None, f"trial:false = {code}, trialSince rỗng")
        if code == 200 and body["data"]["deliveredAt"] is None:
            c.ok(body["data"]["stage"] == "building", "tắt Chạy thử ⇒ building")
        code, body = jcall(base, "PATCH", f"/admin/projects/{tid}", admin_tok, {"industry": "x" * 65})
        c.ok(code == 422 and body.get("code") == "PROJECT_INDUSTRY_TOO_LONG", f"ngành 65 ký tự = {code} {body.get('code')}")
        code, body = jcall(base, "PATCH", f"/admin/projects/{tid}", admin_tok, {"industry": "Phòng khám nha khoa"})
        c.ok(code == 200 and body["data"]["industry"] == "Phòng khám nha khoa", f"đặt ngành = {code}")
        code, body = jcall(base, "PATCH", "/admin/projects/00000000-0000-4000-8000-000000000000", admin_tok, {"trial": True})
        c.ok(code == 404 and body.get("code") == "TENANT_NOT_FOUND", f"khách lạ = {code} {body.get('code')}")
        code, body = jcall(base, "GET", "/admin/projects", admin_tok)
        again = {r["slug"]: r for r in body.get("data", [])}.get(a.target_slug) or {}
        c.ok(again.get("industry") == "Phòng khám nha khoa", "đọc lại thấy ngành vừa đặt")

        print("3. Khám sức khoẻ")
        code, body = jcall(base, "POST", f"/admin/projects/{tid}/health-check", admin_tok)
        h = body.get("data") or {}
        c.ok(code == 200 and h.get("status") in ("ok", "warn", "critical"), f"health-check = {code}, status={h.get('status')}")
        c.ok(h.get("checkedAt"), "có checkedAt")
        code, body = jcall(base, "GET", "/admin/projects", admin_tok)
        again = {r["slug"]: r for r in body.get("data", [])}.get(a.target_slug) or {}
        c.ok((again.get("health") or {}).get("status") == h.get("status") and (again.get("health") or {}).get("checkedAt"),
             "GET sau đó thấy bản chụp đã lưu (không quét lại)")

        print("4. Nhật ký xưởng")
        code, body = jcall(base, "GET", "/audit-log?entityName=__project__&limit=20", admin_tok)
        entries = body.get("data") or []
        mine = [e for e in entries if e.get("recordId") == tid and e.get("id") not in seen_before]
        c.ok(code == 200 and mine, f"có dòng __project__ cho khách ({len(entries)} dòng)")
        c.ok(mine and all(e.get("actorUserId") for e in mine), f"{len(mine)} dòng __project__ của lượt này đều có tên người dựng (actorUserId khác rỗng)")

        print("5. Người thứ ba")
        _, ref = call(base, "GET", "/admin/tenants", staff_tok)  # thân 403 chuẩn của route operator-only cũ
        code0, ref_admin = call(base, "GET", "/admin/tenants", admin_tok)
        c.ok(code0 == 200, "người dựng gọi /admin/tenants = 200 (đối chứng)")
        code, body = jcall(base, "POST", "/admin/workspace-sessions", admin_tok, {"tenantSlug": a.target_slug})
        ws_tok = ((body.get("data") or {}).get("accessToken"))
        ws_session = ws_tok
        c.ok(code in (200, 201) and ws_tok, f"mở phiên dựng vào '{a.target_slug}' = {code}")
        routes = [("GET", "/admin/projects", None), ("PATCH", f"/admin/projects/{tid}", {"trial": True}),
                  ("POST", f"/admin/projects/{tid}/health-check", None)]
        for who, tok in (("test@example.com", staff_tok), ("token phiên dựng", ws_tok)):
            for m, p, b in routes:
                code, raw = call(base, m, p, tok, b)
                c.ok(code == 403 and raw == ref, f"{who} {m} {p.split('/')[2] if p.count('/') > 2 else p} = {code}, thân 403 giống từng byte")
        for m, p, b in routes:
            code, _ = call(base, m, p, None, b)
            c.ok(code == 401, f"không token {m} = {code}")
    finally:
        # trả lại như cũ: ngành + Chạy thử của khách thử; đóng phiên dựng
        if "tid" in locals():
            jcall(base, "PATCH", f"/admin/projects/{tid}", admin_tok, {"industry": was_industry, "trial": was_trial})
        if ws_session:
            call(base, "DELETE", "/admin/workspace-sessions/current", ws_session)

    if a.bench:
        ms = []
        for _ in range(a.bench):
            t0 = time.perf_counter()
            code, raw = call(base, "GET", "/admin/projects", admin_tok)
            ms.append((time.perf_counter() - t0) * 1000)
            assert code == 200
        ms.sort()
        p95 = ms[min(len(ms) - 1, int(len(ms) * 0.95))]
        n = len(json.loads(raw)["data"])
        print(f"6. Đo {a.bench} lần với {n} khách: p50={statistics.median(ms):.0f} ms p95={p95:.0f} ms")
        c.ok(p95 < 300, "p95 < 300 ms")

    print("ĐẠT" if not c.failed else f"LỆCH {c.failed} mục")
    sys.exit(1 if c.failed else 0)


if __name__ == "__main__":
    main()
