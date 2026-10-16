#!/usr/bin/env python3
"""Nghiệm thu người-thứ-ba của cờ "xem bản nháp" khi xem như vai (specs/064-preview-as-role-sample-data S9-9, #401).

Vì sao cần: người dựng là admin nên thấy bản nháp bằng header `X-Nexus-Draft`; cờ mới phải cho một phiên XEM NHƯ VAI
đọc được nháp, và KHÔNG ai khác: nhân viên thật (`test@example.com`, vai `crm_sales_rep`) đăng nhập thật không bao giờ thấy
nháp, phiên xem thử không bật cờ cũng không, header `X-Nexus-Draft` không mở cờ cho token xem thử. Kịch bản (nháp = một
form `crm_opportunities` có tiêu đề riêng):

  1. Chưa có nháp nào: `POST /admin/preview-sessions {draft:true}` ⇒ 422 `PREVIEW_DRAFT_UNAVAILABLE`; bỏ trống `draft`
     ⇒ 201 `draft:false`.
  2. Người dựng lưu form nháp. Nhân viên thật (có/không header giả) KHÔNG thấy. Mở phiên bỏ trống `draft` ⇒ 201
     `draft:true` và `GET /metadata/layouts/crm_opportunities?view=edit` bằng token xem thử THẤY form nháp; `draft:false`
     ⇒ không thấy (kể cả gửi `X-Nexus-Draft: 1`); `draft:true` tường minh ⇒ thấy.
  3. Phiên nháp là chỉ đọc (ghi ⇒ 403 `PREVIEW_READ_ONLY`), `/auth/me` báo `preview.draft:true`, vẫn không phải admin.
  4. Xuất bản ⇒ nhân viên thật thấy form; phiên `draft:false` cũng thấy (bản đang chạy đã là bản mới).
  5. Dự án chưa có nháp lại (sau xuất bản) ⇒ `draft:true` ⇒ 422, bỏ trống ⇒ `draft:false`.

Dùng (server riêng + DB bản sao, KHÔNG dùng :8090 chung — Xuất bản lần đầu là một chiều):
    python3 scripts/acceptance-preview-draft.py --base http://localhost:8431/api/v1

Mã thoát: 0 = đạt · 1 = lệch (kể cả 0 phép kiểm) · 2 = lỗi môi trường.

ponytail: stdlib thuần, khuôn `acceptance-draft-publish.py`.
"""

import argparse
import ipaddress
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

TENANT = "northwind"
ADMIN = ("admin@example.com", os.environ.get("NEXUS_ADMIN_PASS") or "Demo-Admin-2026!")
STAFF = ("test@example.com", os.environ.get("NEXUS_STAFF_PASS") or "Demo-Rep-2026!")
ENTITY = "crm_opportunities"
MARKER = "NT064 form nháp"
SHARED_DEV_PORT = 8090
DRAFT_HDR = {"X-Nexus-Draft": "1"}


class EnvError(Exception):
    """Environment problem — exit 2."""


fails = []
ran = 0


def check(label, cond, detail=""):
    global ran
    ran += 1
    print(("  ĐẠT   " if cond else "  LỆCH  ") + label + (f"   [{detail}]" if detail else ""))
    if not cond:
        fails.append(label)


def req(base, method, path, token=None, body=None, headers_extra=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    if headers_extra:
        headers.update(headers_extra)
    r = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            raw, code = resp.read(), resp.status
    except urllib.error.HTTPError as e:
        raw, code = e.read(), e.code
    except Exception as e:  # connection refused, timeout…
        raise EnvError(f"không gọi được {base + path} — {e}") from e
    try:
        parsed = json.loads(raw) if raw else {}
    except ValueError:
        parsed = {}
    return code, parsed


def login(base, email, password):
    code, body = req(base, "POST", "/auth/login", body={"tenant": TENANT, "email": email, "password": password})
    data = body.get("data") or {}
    if code != 200 or not data.get("accessToken"):
        raise EnvError(f"đăng nhập {email} thất bại ({code} {body.get('code', '')})")
    return data["accessToken"], data.get("user") or {}


def is_loopback(base):
    host = (urllib.parse.urlparse(base).hostname or "").lower()
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def titles(body):
    view = (((body.get("data") or {}).get("views") or {}).get("edit")) or {}
    return [s.get("title") for s in (view.get("sections") or [])]


def start(base, adm, role_id, **extra):
    code, body = req(base, "POST", "/admin/preview-sessions", adm, {"roleId": role_id, **extra})
    return code, body, (body.get("data") or {})


def run(base, ended):
    adm, _ = login(base, *ADMIN)
    staff, staff_user = login(base, *STAFF)
    if staff_user.get("isAdmin"):
        raise EnvError(f"{STAFF[0]} đang là admin — nghiệm thu bằng admin tự chấm điểm cho mình")
    role_id = staff_user.get("roleId")  # the role of the real employee: the session is compared with exactly this person
    if not role_id:
        raise EnvError(f"{STAFF[0]} không có roleId")

    code, body = req(base, "GET", "/admin/publish/state", adm)
    st = body.get("data") or {}
    if code != 200:
        raise EnvError(f"GET /admin/publish/state → {code}")
    path = f"/metadata/layouts/{ENTITY}?view=edit"

    print("\n1. Chưa có nháp")
    if not st.get("delivered"):
        c, b, d = start(base, adm, role_id, draft=True)
        check("chưa giao + draft:true ⇒ 422 PREVIEW_DRAFT_UNAVAILABLE", c == 422 and b.get("code") == "PREVIEW_DRAFT_UNAVAILABLE", f"{c} {b.get('code')}")
        c, b, d = start(base, adm, role_id)
        check("chưa giao + bỏ trống draft ⇒ 201 draft:false", c == 201 and d.get("draft") is False, f"{c} {d.get('draft')}")
        ended.append(d.get("token"))
        c, b = req(base, "POST", "/admin/publish/baseline", adm, {"note": "nghiem thu S9-9"})
        check("Xuất bản lần đầu ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    else:
        print("  (tenant đã giao từ trước — bỏ phép kiểm 'chưa giao')")
    code, body = req(base, "GET", "/admin/publish/changes", adm)
    h = (body.get("meta") or {}).get("changesetHash", "")
    if (body.get("data") or []):
        req(base, "DELETE", "/admin/publish/changes?changesetHash=" + h, adm)  # clean slate
    c, b, d = start(base, adm, role_id, draft=True)
    check("đã giao nhưng 0 nháp + draft:true ⇒ 422 PREVIEW_DRAFT_UNAVAILABLE", c == 422 and b.get("code") == "PREVIEW_DRAFT_UNAVAILABLE", f"{c} {b.get('code')}")
    c, b, d = start(base, adm, role_id)
    check("đã giao, 0 nháp, bỏ trống draft ⇒ 201 draft:false", c == 201 and d.get("draft") is False, f"{c} {d.get('draft')}")
    ended.append(d.get("token"))

    print("\n2. Có một form nháp")
    c, live_before = req(base, "GET", path, staff)
    check("nhân viên thật đọc form hiện tại ⇒ 200, chưa có form nháp", c == 200 and MARKER not in titles(live_before), f"{c}")
    layout = {"version": 1, "sections": [{"title": MARKER, "columns": 2, "fields": ["name", "stage"]}]}
    c, b = req(base, "POST", "/admin/layouts", adm, {"entityName": ENTITY, "viewType": "edit", "layoutJson": layout})
    lid = (b.get("data") or {}).get("id")
    check("người dựng lưu form ⇒ 200 (vào nháp)", c == 200 and bool(lid), f"{c} {b.get('code', '')}")
    if not lid:
        return
    c, b = req(base, "GET", path, staff)
    check("nhân viên thật (đăng nhập thật) KHÔNG thấy nháp", c == 200 and MARKER not in titles(b), str(titles(b)))
    c, b = req(base, "GET", path, staff, headers_extra=DRAFT_HDR)
    check("nhân viên thật + header giả KHÔNG thấy nháp", c == 200 and MARKER not in titles(b), str(titles(b)))

    c, b, d = start(base, adm, role_id)
    check("bỏ trống draft, có nháp ⇒ 201 draft:true (mặc định)", c == 201 and d.get("draft") is True, f"{c} {d.get('draft')}")
    pv_default = d.get("token")
    ended.append(pv_default)
    c, b = req(base, "GET", path, pv_default)
    check("token xem thử draft:true THẤY form nháp", c == 200 and MARKER in titles(b), str(titles(b)))

    c, b, d = start(base, adm, role_id, draft=True)
    check("draft:true tường minh ⇒ 201 draft:true", c == 201 and d.get("draft") is True, f"{c} {d.get('draft')}")
    ended.append(d.get("token"))

    c, b, d = start(base, adm, role_id, draft=False)
    check("draft:false ⇒ 201 draft:false", c == 201 and d.get("draft") is False, f"{c} {d.get('draft')}")
    pv_live = d.get("token")
    ended.append(pv_live)
    c, b = req(base, "GET", path, pv_live)
    check("token xem thử draft:false KHÔNG thấy nháp", c == 200 and MARKER not in titles(b), str(titles(b)))
    c, b = req(base, "GET", path, pv_live, headers_extra=DRAFT_HDR)
    check("token xem thử draft:false + header X-Nexus-Draft KHÔNG thấy nháp", c == 200 and MARKER not in titles(b), str(titles(b)))

    print("\n3. Phiên nháp chỉ đọc, không phải admin")
    c, b = req(base, "POST", f"/{ENTITY}", pv_default, {"name": "x"})
    check("ghi bằng token nháp ⇒ 403 PREVIEW_READ_ONLY", c == 403 and b.get("code") == "PREVIEW_READ_ONLY", f"{c} {b.get('code')}")
    c, b = req(base, "POST", "/admin/layouts", pv_default, {"entityName": ENTITY, "viewType": "edit", "layoutJson": layout})
    check("ghi form (route admin) bằng token nháp ⇒ bị chặn, không vào nháp", c == 403, f"{c} {b.get('code')}")
    c, b = req(base, "GET", "/auth/me", pv_default)
    me = b.get("data") or {}
    check("/auth/me ⇒ preview.draft:true, không admin, không người vận hành",
          c == 200 and (me.get("preview") or {}).get("draft") is True and me.get("isAdmin") is False and me.get("isPlatformOperator") is False,
          f"{c} {me.get('preview')} admin={me.get('isAdmin')}")
    c, b = req(base, "GET", "/admin/publish/state", pv_default)
    check("token nháp không đọc được trạng thái xuất bản (route người dựng) ⇒ 403", c == 403, f"{c}")

    print("\n4. Xuất bản")
    c, ch = req(base, "GET", "/admin/publish/changes", adm)
    h = (ch.get("meta") or {}).get("changesetHash", "")
    c, b = req(base, "POST", "/admin/publish", adm, {"changesetHash": h, "force": False, "note": "nghiem thu S9-9"})
    check("Xuất bản ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    c, b = req(base, "GET", path, staff)
    check("nhân viên thật THẤY form sau khi xuất bản", c == 200 and MARKER in titles(b), str(titles(b)))
    c, b = req(base, "GET", path, pv_live)
    check("token draft:false thấy form (bản đang chạy giờ đã là bản mới)", c == 200 and MARKER in titles(b), str(titles(b)))

    print("\n5. Hết nháp lại")
    c, b, d = start(base, adm, role_id, draft=True)
    check("sau xuất bản (0 nháp) + draft:true ⇒ 422", c == 422 and b.get("code") == "PREVIEW_DRAFT_UNAVAILABLE", f"{c} {b.get('code')}")
    c, b, d = start(base, adm, role_id)
    check("sau xuất bản + bỏ trống draft ⇒ 201 draft:false", c == 201 and d.get("draft") is False, f"{c} {d.get('draft')}")
    ended.append(d.get("token"))

    # Put the clone back: the published form goes away through the draft layer and is published (0 pending).
    c, b = req(base, "DELETE", f"/admin/layouts/{lid}", adm)
    c, ch = req(base, "GET", "/admin/publish/changes", adm)
    if ch.get("data"):
        req(base, "POST", "/admin/publish", adm, {"changesetHash": (ch.get("meta") or {}).get("changesetHash", ""), "force": True, "note": "nghiem thu S9-9: don"})


def main():
    ap = argparse.ArgumentParser(description="Nghiệm thu không-admin cho cờ bản nháp của xem-như-vai (specs/064 S9-9).")
    ap.add_argument("--base", required=True, help="vd http://localhost:8431/api/v1 (server riêng trên DB bản sao)")
    ap.add_argument("--allow-remote-write", action="store_true")
    a = ap.parse_args()
    base = a.base.rstrip("/")
    if urllib.parse.urlparse(base).port == SHARED_DEV_PORT:
        print(f"TỪ CHỐI: cổng {SHARED_DEV_PORT} là server dev dùng chung — Xuất bản lần đầu là một chiều. Dùng server riêng + DB bản sao.")
        return 2
    if not (is_loopback(base) or a.allow_remote_write):
        print("TỪ CHỐI: --base không phải loopback (script GHI). Thêm --allow-remote-write nếu chắc chắn.")
        return 2
    ended = []
    try:
        run(base, ended)
    except EnvError as e:
        print(f"LỖI MÔI TRƯỜNG: {e}")
        return 2
    finally:
        for tok in ended:
            if tok:
                req(base, "DELETE", "/preview-sessions/current?reason=exit", tok)
    if ran == 0:
        print("LỆCH — 0 phép kiểm chạy (không xanh giả)")
        return 1
    if fails:
        print(f"\nLỆCH {len(fails)}/{ran}")
        return 1
    print(f"\nĐẠT {ran}/{ran}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
