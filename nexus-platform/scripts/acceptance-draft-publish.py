#!/usr/bin/env python3
"""Nghiệm thu người-thứ-ba của tầng nháp → xuất bản (specs/063-draft-publish lát S7-1: luật + Xuất bản lần đầu +
"Tắt ngay cho khách"; tasks T004/T047/T051d).

Vì sao cần: người dựng là admin và admin bỏ qua toàn bộ phân quyền, nên "tôi sửa luật rồi thấy chạy" chứng minh
đúng một nửa. Nửa kia là người dùng THẬT của khách: nhân viên `test@example.com` (vai crm_sales_rep, KHÔNG admin) phải
tiếp tục chạy theo luật CŨ cho tới khi người dựng bấm Xuất bản, và quản trị viên khách di sản (admin KHÔNG phải người
vận hành — tài khoản tạm `nghiemthu-draft-legacy-admin@acceptance.invalid`, cất lại trong `finally`) không được thấy
nháp, không gọi được route xuất bản, và đọc `/audit-log` không thấy thân tài liệu hay khoá.

Phép kiểm (mọi kết quả phía khách chấm bằng nhân viên không-admin; cơ hội thử tạo bằng `test@example.com`):
  1. 403 BYTE-GIỐNG: nhân viên và quản trị viên khách di sản gọi `GET /admin/publish/state`, `POST …/baseline`,
     `POST /admin/publish`, `DELETE …/changes` đều nhận đúng thân 403 của `GET /admin/users` mà nhân viên nhận
     (không dò được route). Nếu có `owner@example.com` (chủ khách, không quản trị — specs/065 lát B) cũng thế; không có
     thì bỏ qua, in rõ, không tính là phép kiểm.
  2. Chưa giao (chỉ khi bản sao CHƯA giao): luật X lưu thẳng và chạy ngay với nhân viên; Xuất bản ⇒ 409 NOT_DELIVERED;
     "Xuất bản lần đầu" ⇒ 201 phiên bản 1; lần hai ⇒ 409 ALREADY_DELIVERED. Bản sao đã giao từ lần chạy trước: bỏ qua
     nhóm này (in rõ) và dựng luật X bằng nháp + xuất bản.
  3. Nháp: người dựng tắt luật X + thêm luật Y ⇒ `draftState` edited/new; nhân viên VẪN bị luật X cũ chặn và KHÔNG bị luật
     Y chặn; quản trị viên khách di sản chỉ thấy bản sống; danh sách thay đổi có 2 dòng (sửa/mới).
  4. Cổng đầu vào: hash sai dạng / ghi chú > 500 ký tự ⇒ 422 INVALID_BODY; hash cũ ⇒ 409 DRAFTS_CHANGED.
  5. Hai tab Xuất bản cùng lúc: đúng MỘT 201, tab kia 409; số phiên bản tăng đúng 1. Sau đó nhân viên tạo ở Thắng ⇒
     được (X đã tắt) và ở Đàm phán ⇒ 422 (Y chạy) — hiệu lực ngay trên máy chủ nhận thao tác.
  6. Nhật ký: quản trị viên khách di sản đọc `/audit-log?entityName=__meta_version__`: người thực hiện = người dựng,
     không có tên luật, không có thông điệp chặn, không có khoá.
  7. Xung đột: người dựng sửa X (nháp) rồi quản trị viên khách di sản sửa X (sống, như hôm nay) ⇒ Xuất bản 409
     PUBLISH_CONFLICT nêu đúng X; `force` ⇒ 201.
  8. Tắt ngay: người dựng có nháp trên Y rồi "Tắt ngay cho khách" ⇒ 200 `isActive:false`, nhân viên tạo ở Đàm phán ⇒
     được NGAY; nháp của Y còn nguyên, Xuất bản ⇒ 409 PUBLISH_CONFLICT nêu đúng Y; luật chỉ-có-trong-nháp ⇒ 404; nhân viên /
     quản trị viên khách di sản ⇒ 403.
  9. Mã nháp lạ / của tenant khác / không phải UUID ⇒ 404 DRAFT_NOT_FOUND, không đổi gì.
 10. Cài gói vào dự án đã giao ⇒ 409 PROJECT_DELIVERED_INSTALL_BLOCKED, không ghi gì.

 11. Báo cáo + dashboard qua nháp (specs/063 lát S7-2, T062): người dựng tạo/sửa báo cáo R và dashboard D ⇒ `draftState`
     new/edited; người dựng + header `X-Nexus-Draft: 1` thấy bản nháp ở `/reports`, `/reports/:id/run`, `…/export.csv`,
     `/dashboards`, `/dashboards/:id/run`; nhân viên + header, quản trị viên khách di sản + header, và người dựng KHÔNG
     header đều thấy bản sống; hai lần lưu nháp liên tiếp giữ cả hai thay đổi; lưu y hệt bản sống rồi Xuất bản không
     PUBLISH_CONFLICT; xoá D ở nháp KHÔNG gỡ mục menu (khách vẫn thấy tab + chạy được), Xuất bản gỡ mục menu cùng lúc.

 12. Màn (form), thương hiệu và "xem thử bản nháp" (specs/063 lát S7-3, T075): người dựng lưu form của bảng ⇒ khách
     (nhân viên `test@example.com`, quản trị viên khách di sản) vẫn thấy form cũ, người dựng + header thấy form nháp
     (`draftState`), Xuất bản ⇒ nhân viên thấy form mới; đổi màu thương hiệu ⇒ `GET /auth/branding/northwind` (công khai) VẪN
     màu cũ kể cả khi gửi token người dựng + header, `GET /admin/tenant/branding` của người dựng thấy màu nháp, Xuất bản ⇒
     trang công khai đổi màu; người dựng + header gọi ghi (POST hồ sơ / nhập CSV / sửa hàng loạt / tải file / `/admin/users`
     / `/admin/roles/:id`) ⇒ 403 `PREVIEW_READ_ONLY`, còn nhân viên + header giả và người dựng KHÔNG header không bị chặn
     bởi mã này.

 13. Menu + thuộc tính app qua nháp (specs/063 lát S7-4, T085): người dựng đổi nhãn app A và thêm dashboard N (chỉ có
     trong nháp) vào menu ⇒ người dựng + header thấy nhãn mới + tab N (`draftState` edited), nhân viên (kể cả gửi header
     giả) và quản trị viên khách di sản vẫn thấy nhãn + menu cũ; menu trỏ vào dashboard đang "Sẽ xoá" ⇒ 422
     APP_NAV_REF_INVALID; Xuất bản ⇒ nhân viên thấy tab N và mở được; xoá dashboard E đang nằm trong menu rồi Xuất bản ⇒
     mục E biến mất khỏi menu CÙNG lần xuất bản (thay đổi app tổng hợp tính vào `docCount`), nhân viên không bao giờ thấy tab
     chết; bỏ nháp ⇒ xem trước về nhãn/menu đang chạy; xoá hẳn app là ÁP NGAY (nhân viên 404 ngay, không có nháp nào).

 15. Mục áp ngay (specs/063 lát S7-5, T094) - cần `--db` (đọc `sys_meta_versions` qua `docker exec nexus-mysql`): nhân viên
     `test@example.com` bị từ chối NGAY ở route vai; người dựng tạo/sửa vai ⇒ mục `direct` (version_no NULL, người thực hiện =
     người dựng, có trước/sau); xoá vai có người giữ + 2 dòng quyền trường ⇒ mục ghi đủ `assignedUserIds` và `fieldAcl`;
     quản trị viên khách di sản sửa luật ⇒ áp NGAY (nhân viên bị chặn liền) + mục mang TÊN người đó; `acl-backfill` bỏ qua bảng
     chỉ có trong menu NHÁP; chủ khách `owner@example.com` (nếu có) ⇒ 403 thân giống `RequireAdmin`, không mục direct.

 14. Trường và bảng mới (specs/063 lát S7-8, T125): người dựng thêm trường BẮT BUỘC và một bảng mới ⇒ nhân viên
     (`test@example.com`) và quản trị viên khách di sản không thấy trường mới ở `/metadata/fields`, `/metadata/layouts`; bảng mới
     không hiện ở `/metadata/entities` với quản trị viên khách di sản (nhân viên chưa có grant trên bảng mới nên vốn không thấy;
     `/metadata/fields/<bảng mới>` ⇒ 404), vẫn tạo được cơ hội; người dựng + header thấy chúng
     (`draftState new`, `pendingRequired`); `options.indexed` áp ngay (sắp xếp theo trường đó ⇒ 200); sửa nhãn và sắp xếp
     lại là nháp (nhân viên giữ nhãn/thứ tự cũ); đổi kiểu ⇒ 422 FIELD_TYPE_IMMUTABLE kèm hint; Xuất bản ⇒ nhân viên thấy
     trường + bảng mới và tạo cơ hội thiếu trường bắt buộc ⇒ 422.

 15. Xoá trường/bảng chờ tới Xuất bản (specs/063 lát S7-9, T133): người dựng xoá một trường và một bảng khách đang dùng ⇒
     204 nhưng chỉ là nháp: nhân viên VẪN thấy và ghi được vào trường, quản trị viên khách di sản vẫn thấy bảng + đủ hồ sơ;
     người dựng + header thấy `draftState deleted`; sửa trường đang "sẽ xoá" ⇒ 409; Hoàn tác (bỏ thay đổi) ⇒ hết dấu. Một
     công thức bắt đầu đọc trường SAU khi nháp xoá đã lập ⇒ Xuất bản 201 nhưng `schemaStatus failed` (bảng đã xoá, trường báo
     lý do), `state.schemaRetry` nêu phiên bản; nhân viên / quản trị viên khách di sản gọi `retry-schema` ⇒ 403, mã lạ hoặc
     không phải UUID ⇒ 404 VERSION_NOT_FOUND; thử lại khi công thức còn ⇒ vẫn failed; xoá công thức rồi thử lại ⇒ done,
     `schemaRetry` về null; thử lại lần nữa ⇒ 409 SCHEMA_RETRY_NOT_NEEDED.

 17. Quay lại một phiên bản (specs/063 lát S7-7, T111): dựng lịch sử (C, khách sửa C áp ngay, A = bản đích, B + sửa A/C, xoá A) ⇒
     nhân viên / quản trị viên khách di sản gọi `rollback-plan` + `rollback` ⇒ 403 thân giống `RequireAdmin`; người dựng thấy kế hoạch
     (A dựng lại, B xoá, C đưa về) không kèm thân tài liệu; mã lạ ⇒ 404 `VERSION_NOT_FOUND`; `HEAD_MOVED`, `DRAFTS_PENDING` không đổi gì;
     Quay lại ⇒ 201 `kind rollback`, nhân viên chạy lại được A (cùng mã), không còn thấy B, C giữ sửa áp-ngay của khách trước bản đích;
     quay lại lần nữa ⇒ 409 `NOTHING_TO_PUBLISH`.

 18. Hoàn tác mục áp ngay (specs/063 lát S7-7b, T116): `test@example.com` giữ vai V (V ẩn một trường của bảng thử); người dựng
     xoá V ⇒ nhân viên mất quyền mở bảng; `GET /admin/publish/versions/:id` của mục xoá vai liệt kê nhân viên trong `reassign`
     (kèm `createdByLabel`); nhân viên / quản trị viên khách di sản gọi `revert` ⇒ 403 thân giống RequireAdmin; mã lạ ⇒ 404;
     phiên bản đánh số ⇒ 422 `REVERT_USE_ROLLBACK`; Hoàn tác ⇒ 200, V trở lại CÙNG MÃ, nhân viên mở lại được bảng VÀ vẫn không
     thấy trường V ẩn; hoàn tác lần hai ⇒ 409 `REVERT_STALE`.

 20. Trường/bảng thêm SAU khi giao (specs/063 FR-060, lỗi "trường nháp vô hình"): người dựng thêm một trường KHÔNG bắt buộc (không có
     nháp nào đi kèm) và một bảng mới ⇒ `pendingCount` 2, danh sách thay đổi có đúng hai dòng `op create` (`discardable:false`, không
     xung đột), nhân viên `test@example.com` chưa thấy trường ở `/metadata/fields`, "Bỏ tất cả" không xoá chúng; Xuất bản ⇒ 201 (`docCount` 2),
     nhân viên thấy trường, quản trị viên khách di sản thấy bảng, `pendingCount` 0, Xuất bản lần nữa ⇒ 409 `NOTHING_TO_PUBLISH`.

 21. Quan hệ nhiều–nhiều đăng ký SAU khi giao giữa hai bảng khách đã có (`POST /admin/relationships`, không sinh trường, không nháp):
     nhân viên chưa thấy ở `/metadata/relationships`, `pendingCount` 1, danh sách có đúng một dòng `relationship` `op create`
     (`discardable:false`); Xuất bản ⇒ 201 `docCount` 1 và `GET /admin/publish/versions/:id` có đúng 1 tài liệu (`docCount` nói thật),
     nhân viên thấy quan hệ. Quay lại phiên bản trước (bản sau chỉ có dòng ẩn) ⇒ kế hoạch `changes` rỗng + `rehides` nêu quan hệ,
     201 (không phải `NOTHING_TO_PUBLISH`), nhân viên KHÔNG còn thấy, quan hệ vẫn sống và lại chờ (`pendingCount` 1); quay lại khi còn
     dòng chờ ⇒ `DRAFTS_PENDING`; Xuất bản lại ⇒ nhân viên thấy lại.

Script GHI: bật chế độ nháp cho tenant `northwind` (một chiều), tạo/xoá luật + cơ hội thử, tạo/cất một tài khoản admin tạm.
Vì vậy:
  * CHỈ chạy trên server riêng + DB bản sao, KHÔNG BAO GIỜ trên nexus_platform dùng chung — script từ chối cổng 8090
    và base không phải loopback (trừ `--allow-remote-write`);
  * `--base` bắt buộc.

    python3 scripts/acceptance-draft-publish.py --base http://localhost:8412/api/v1

Mã thoát: 0 = đạt · 1 = lệch · 2 = lỗi môi trường (server chết, đăng nhập lỗi, không cất được tài khoản tạm, 0 phép kiểm chạy).
0 phép kiểm chạy = exit 1 (không xanh giả).

ponytail: stdlib thuần, khuôn `acceptance-before-save-hooks.py` / `acceptance-preview-role.py`.
"""

import argparse
import concurrent.futures
import ipaddress
import json
import os
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

TENANT = "northwind"
ADMIN = ("admin@example.com", os.environ.get("NEXUS_ADMIN_PASS") or "Demo-Admin-2026!")
STAFF = ("test@example.com", os.environ.get("NEXUS_STAFF_PASS") or "Demo-Rep-2026!")
OWNER = ("owner@example.com", os.environ.get("NEXUS_OWNER_PASS") or "Demo-Owner-2026!")
LEGACY_EMAIL = "nghiemthu-draft-legacy-admin@acceptance.invalid"
ENTITY = "crm_opportunities"
X_MSG = "NT063 chan Thang"
Y_MSG = "NT063 chan Dam phan"
SHARED_DEV_PORT = 8090
HEX64_BAD = "z" * 64


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


DRAFT_HDR = {"X-Nexus-Draft": "1"}


def req(base, method, path, token=None, body=None, raw_body=False, headers_extra=None):
    """(status, json, raw-bytes)."""
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    if headers_extra:
        headers.update(headers_extra)
    r = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            raw = resp.read()
    except urllib.error.HTTPError as e:
        raw = e.read()
        code = e.code
    except Exception as e:  # connection refused, timeout…
        raise EnvError(f"không gọi được {base + path} — {e}") from e
    else:
        code = resp.status
    try:
        parsed = json.loads(raw) if raw else {}
    except ValueError:
        parsed = {}
    return code, parsed, raw


def login(base, email, password):
    code, body, _ = req(base, "POST", "/auth/login", body={"tenant": TENANT, "email": email, "password": password})
    data = body.get("data") or {}
    if code != 200 or not data.get("accessToken"):
        if data.get("mfaPending"):
            raise EnvError(f"{email} đang bật MFA — script không tự vượt bước 2")
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


def veto_rule(name, stage, msg):
    return {
        "name": name, "entityName": ENTITY, "trigger": "before_save", "isActive": True,
        "condition": {"rules": [{"field": "stage", "op": "eq", "value": stage}]},
        "actions": [{"type": "veto", "field": "stage", "message": msg}],
    }


def rule_update(rec, **over):
    """A PUT body that re-sends rule `rec` as the builder read it (full replace)."""
    body = {"name": rec["name"], "entityName": rec["entityName"], "trigger": rec["trigger"],
            "condition": rec.get("condition"), "actions": rec.get("actions"), "isActive": rec.get("isActive", True)}
    body.update(over)
    return body


def wf(body):
    return (body.get("data") or {}).get("workflow") or {}


def create_opp(base, staff, cleanup, stage):
    name = f"NT063 {stage} {uuid.uuid4().hex[:6]}"
    code, body, _ = req(base, "POST", f"/{ENTITY}", staff, {"name": name, "stage": stage})
    rid = (body.get("data") or {}).get("id")
    if code == 201 and rid:
        cleanup.append(("record", rid))
    return code, body


def vetoed(code, body, msg):
    return code == 422 and body.get("code") == "RECORD_HOOK_VETO" and body.get("error") == msg


def state(base, adm):
    code, body, _ = req(base, "GET", "/admin/publish/state", adm)
    return code, (body.get("data") or {})


def changes(base, adm):
    code, body, _ = req(base, "GET", "/admin/publish/changes", adm)
    return code, (body.get("data") or []), (body.get("meta") or {}).get("changesetHash", "")


def publish(base, adm, force=False, note=None):
    code, items, h = changes(base, adm)
    if code != 200:
        return code, {}
    body = {"changesetHash": h, "force": force}
    if note:
        body["note"] = note
    c, b, _ = req(base, "POST", "/admin/publish", adm, body)
    return c, b


def ensure_legacy_admin(base, adm):
    password = "Dp-" + secrets.token_urlsafe(18)
    code, body, _ = req(base, "POST", "/admin/users", adm, {
        "email": LEGACY_EMAIL, "password": password, "fullName": "Nghiệm thu nháp (tạm)", "status": "active", "isAdmin": True})
    if code == 201:
        return ((body.get("data") or {}).get("user") or body.get("data") or {}).get("id"), password
    if code != 409:
        raise EnvError(f"tạo admin tạm thất bại ({code} {body.get('code', '')})")
    code, body, _ = req(base, "GET", "/admin/users", adm)
    users = ((body.get("data") or {}).get("users")) or []
    uid = next((u.get("id") for u in users if u.get("email") == LEGACY_EMAIL), None)
    if not uid:
        raise EnvError("admin tạm đã tồn tại (409) nhưng không thấy trong danh sách")
    code, body, _ = req(base, "PUT", f"/admin/users/{uid}", adm, {"status": "active", "isAdmin": True, "password": password})
    if code != 200:
        raise EnvError(f"bật lại admin tạm thất bại ({code} {body.get('code', '')})")
    return uid, password


def stow_legacy_admin(base, adm, uid):
    code, body, _ = req(base, "PUT", f"/admin/users/{uid}", adm, {"status": "disabled", "isAdmin": False})
    if code != 200:
        raise EnvError(f"KHÔNG cất được admin tạm {uid} ({code}) — tắt tay trong Admin › Người dùng")


def run(a, cleanup):
    base = a.base.rstrip("/")
    print(f"Server: {base} · tenant {TENANT} · người dựng {ADMIN[0]} · nhân viên {STAFF[0]}")
    adm, adm_user = login(base, *ADMIN)
    staff, staff_user = login(base, *STAFF)
    if staff_user.get("isAdmin"):
        raise EnvError(f"{STAFF[0]} đang là admin — nghiệm thu bằng admin tự chấm điểm cho mình")
    builder_id = adm_user.get("id")

    legacy_uid, legacy_pw = ensure_legacy_admin(base, adm)
    cleanup.append(("call", lambda: stow_legacy_admin(base, adm, legacy_uid)))
    legacy, _ = login(base, LEGACY_EMAIL, legacy_pw)

    owner = None
    try:
        owner, _ = login(base, *OWNER)
    except EnvError:
        print(f"  (bỏ qua, không tính phép kiểm: không đăng nhập được {OWNER[0]} — chưa dựng tầng chủ khách)")

    # ── 1. 403 byte-giống ────────────────────────────────────────────────────────────────────────────
    print("\n1. Ai gọi được route xuất bản")
    _, _, reference = req(base, "GET", "/admin/users", staff)
    routes = [("GET", "/admin/publish/state"), ("GET", "/admin/publish/changes"), ("POST", "/admin/publish/baseline"),
              ("POST", "/admin/publish"), ("DELETE", "/admin/publish/changes?changesetHash=" + "a" * 64)]
    who = [("nhân viên", staff), ("quản trị viên khách di sản", legacy)] + ([("chủ khách (không quản trị)", owner)] if owner else [])
    for label, tok in who:
        bad = []
        for m, p in routes:
            code, _, raw = req(base, m, p, tok, {} if m == "POST" else None)
            if code != 403 or raw != reference:
                bad.append(f"{m} {p} → {code}")
        check(f"{label}: mọi route xuất bản ⇒ 403 thân giống hệt route quản trị khác", not bad, "; ".join(bad))
    # disable-now sits on adminGated (like every rule route): the legacy admin reaches the service, which has no
    # such rule in THIS tenant under a random id ⇒ 404, never a write. Staff stays 403.
    code, body, _ = req(base, "POST", f"/admin/workflows/{uuid.uuid4()}/disable-now", legacy, {})
    check("disable-now của quản trị viên khách di sản trên mã lạ ⇒ 404 WORKFLOW_NOT_FOUND (không ghi gì)",
          code == 404 and body.get("code") == "WORKFLOW_NOT_FOUND", f"{code} {body.get('code', '')}")

    # ── 2. Chưa giao / giao ──────────────────────────────────────────────────────────────────────────
    code, st = state(base, adm)
    if code != 200:
        raise EnvError(f"GET /admin/publish/state ⇒ {code} — người dựng không gọi được")
    x = None
    if not st.get("delivered"):
        print("\n2. Chưa giao: ghi thẳng, Xuất bản lần đầu")
        code, body, _ = req(base, "POST", "/admin/workflows", adm, veto_rule("NT063 X", "Closed Won", X_MSG))
        x = wf(body)
        check("luật X tạo ⇒ 201 và sống ngay (chưa giao nên không qua nháp)", code == 201 and x.get("draftState") is None, f"{code} draftState={x.get('draftState')}")
        if code != 201:
            return
        cleanup.append(("workflow", x["id"]))
        c, b = create_opp(base, staff, cleanup, "Closed Won")
        check("nhân viên tạo cơ hội Thắng ⇒ 422 (luật X chạy ngay)", vetoed(c, b, X_MSG), f"{c} {b.get('code', '')}")
        _, items, h = changes(base, adm)
        c, b, _ = req(base, "POST", "/admin/publish", adm, {"changesetHash": h})
        check("Xuất bản khi chưa giao ⇒ 409 NOT_DELIVERED", c == 409 and b.get("code") == "NOT_DELIVERED", f"{c} {b.get('code', '')}")
        c, b, _ = req(base, "POST", "/admin/publish/baseline", adm, {"note": "nghiem thu"})
        d = b.get("data") or {}
        check("Xuất bản lần đầu ⇒ 201 phiên bản 1 (kind baseline)", c == 201 and d.get("versionNo") == 1 and d.get("kind") == "baseline" and d.get("docCount", 0) >= 1,
              f"{c} {json.dumps(d)}")
        c, b, _ = req(base, "POST", "/admin/publish/baseline", adm, {})
        check("lần hai ⇒ 409 ALREADY_DELIVERED", c == 409 and b.get("code") == "ALREADY_DELIVERED", f"{c} {b.get('code', '')}")
    else:
        print("\n2. Bản sao đã giao từ lần chạy trước: bỏ qua nhóm 'chưa giao', dựng luật X bằng nháp + xuất bản")
        code, body, _ = req(base, "POST", "/admin/workflows", adm, veto_rule("NT063 X", "Closed Won", X_MSG))
        x = wf(body)
        check("luật X tạo ⇒ 201 vào nháp (draftState new)", code == 201 and x.get("draftState") == "new", f"{code} draftState={x.get('draftState')}")
        if code != 201:
            return
        cleanup.append(("workflow", x["id"]))
        c, b = publish(base, adm)
        check("Xuất bản luật X ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    for label, tok in [("nhân viên", staff), ("quản trị viên khách di sản", legacy)]:
        c, b, _ = req(base, "POST", "/admin/publish/baseline", tok, {})
        check(f"{label} bấm Xuất bản lần đầu ⇒ 403", c == 403, f"{c} {b.get('code', '')}")

    # ── 3. Nháp ──────────────────────────────────────────────────────────────────────────────────────
    print("\n3. Sửa luật trên bản nháp: khách chưa thấy")
    c, st0 = state(base, adm)
    version0 = (st0.get("headVersion") or {}).get("versionNo", 0)
    c, body, _ = req(base, "PUT", f"/admin/workflows/{x['id']}", adm, rule_update(x, isActive=False))
    edited = wf(body)
    check("người dựng tắt luật X ⇒ 200, draftState edited, isActive false",
          c == 200 and edited.get("draftState") == "edited" and edited.get("isActive") is False, f"{c} {edited.get('draftState')} {edited.get('isActive')}")
    c, body, _ = req(base, "POST", "/admin/workflows", adm, veto_rule("NT063 Y", "Negotiation", Y_MSG))
    y = wf(body)
    check("người dựng tạo luật Y ⇒ 201, draftState new", c == 201 and y.get("draftState") == "new", f"{c} {y.get('draftState')}")
    if c != 201:
        return
    cleanup.append(("workflow", y["id"]))
    c, b = create_opp(base, staff, cleanup, "Closed Won")
    check("nhân viên VẪN bị luật X cũ chặn (bản nháp tắt X chưa đến khách)", vetoed(c, b, X_MSG), f"{c} {b.get('code', '')}")
    c, b = create_opp(base, staff, cleanup, "Negotiation")
    check("nhân viên KHÔNG bị luật Y chặn (Y mới chỉ là nháp) ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    c, body, _ = req(base, "GET", "/admin/workflows", legacy)
    seen = {r.get("id"): r for r in (body.get("data") or {}).get("workflows") or []}
    check("quản trị viên khách di sản chỉ thấy bản sống: X còn bật, không có Y, không có draftState",
          c == 200 and seen.get(x["id"], {}).get("isActive") is True and y["id"] not in seen
          and all(r.get("draftState") in (None, "") for r in seen.values()), f"{c} X={seen.get(x['id'], {}).get('isActive')} Y={'có' if y['id'] in seen else 'không'}")
    c, items, h = changes(base, adm)
    ops = {it.get("docKey"): it.get("op") for it in items}
    check("danh sách thay đổi có đúng 2 dòng: X sửa, Y mới", c == 200 and ops == {x["id"]: "update", y["id"]: "create"}, json.dumps(ops))
    check("không dòng nào mang xung đột", not any(it.get("conflict") for it in items))
    c, st1 = state(base, adm)
    check("state: đã giao, 2 thay đổi, dấu vân tay khớp danh sách", c == 200 and st1.get("delivered") and st1.get("pendingCount") == 2 and st1.get("changesetHash") == h,
          json.dumps({k: st1.get(k) for k in ("delivered", "pendingCount")}))

    # ── 4. cổng đầu vào ──────────────────────────────────────────────────────────────────────────────
    print("\n4. Cổng đầu vào của Xuất bản")
    for label, payload, want_status, want_code in [
        ("hash sai dạng", {"changesetHash": HEX64_BAD}, 422, "INVALID_BODY"),
        ("hash thiếu", {}, 422, "INVALID_BODY"),
        ("ghi chú > 500 ký tự", {"changesetHash": h, "note": "x" * 501}, 422, "INVALID_BODY"),
        ("hash cũ (đúng dạng, không phải tập hiện tại)", {"changesetHash": "0" * 64}, 409, "DRAFTS_CHANGED"),
    ]:
        c, b, _ = req(base, "POST", "/admin/publish", adm, payload)
        check(f"{label} ⇒ {want_status} {want_code}", c == want_status and b.get("code") == want_code, f"{c} {b.get('code', '')}")
    c, st2 = state(base, adm)
    check("các lần bị từ chối không tiêu nháp nào", st2.get("pendingCount") == 2)

    # ── 5. hai tab ───────────────────────────────────────────────────────────────────────────────────
    print("\n5. Hai tab bấm Xuất bản cùng lúc")
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
        futs = [pool.submit(req, base, "POST", "/admin/publish", adm, {"changesetHash": h, "note": "Sua luat nhac hen"}) for _ in range(2)]
        outs = [f.result() for f in futs]
    codes = sorted(o[0] for o in outs)
    check("đúng một 201, tab kia 409", codes[0] == 201 and codes[1] == 409, str([(o[0], o[1].get("code")) for o in outs]))
    other = next((o for o in outs if o[0] == 409), None)
    check("tab thua nhận một trong NOTHING_TO_PUBLISH / DRAFTS_CHANGED / PUBLISH_BUSY",
          other is not None and other[1].get("code") in ("NOTHING_TO_PUBLISH", "DRAFTS_CHANGED", "PUBLISH_BUSY"), other[1].get("code", "") if other else "")
    c, st3 = state(base, adm)
    check("số phiên bản tăng đúng 1 và nháp về 0", (st3.get("headVersion") or {}).get("versionNo") == version0 + 1 and st3.get("pendingCount") == 0,
          f"{version0} → {(st3.get('headVersion') or {}).get('versionNo')}, nháp {st3.get('pendingCount')}")
    c, b = create_opp(base, staff, cleanup, "Closed Won")
    check("sau Xuất bản nhân viên tạo cơ hội Thắng ⇒ 201 (X đã tắt)", c == 201, f"{c} {b.get('code', '')}")
    c, b = create_opp(base, staff, cleanup, "Negotiation")
    check("…và ở Đàm phán ⇒ 422 (Y chạy)", vetoed(c, b, Y_MSG), f"{c} {b.get('code', '')}")

    # ── 6. Nhật ký ───────────────────────────────────────────────────────────────────────────────────
    print("\n6. Nhật ký kiểm tra không lộ thân tài liệu")
    rows, text = [], ""
    for _ in range(12):  # audit is written asynchronously
        c, body, raw = req(base, "GET", "/audit-log?" + urllib.parse.urlencode({"entityName": "__meta_version__", "limit": "100"}), legacy)
        rows = body.get("data") or []
        text = raw.decode("utf-8", "replace")
        if any(r.get("action") == "publish" for r in rows):
            break
        time.sleep(0.5)
    check("quản trị viên khách di sản đọc được nhật ký __meta_version__ (có dòng publish)", any(r.get("action") == "publish" for r in rows), f"{len(rows)} dòng")
    pub_rows = [r for r in rows if r.get("action") in ("publish", "baseline")]
    check("người thực hiện của mọi dòng = người dựng", bool(pub_rows) and all(r.get("actorUserId") == builder_id for r in pub_rows),
          str({r.get("actorUserId") for r in pub_rows}))
    leaked = [s for s in ("NT063 X", "NT063 Y", X_MSG, Y_MSG, "veto", "secret") if s in text]
    check("không có tên luật, thông điệp chặn hay khoá trong nhật ký", not leaked, f"lộ: {leaked}")

    # ── 7. Xung đột ──────────────────────────────────────────────────────────────────────────────────
    print("\n7. Xung đột với quản trị viên khách di sản")
    c, body, _ = req(base, "GET", f"/admin/workflows/{x['id']}", adm)
    xr = wf(body)
    req(base, "PUT", f"/admin/workflows/{x['id']}", adm, rule_update(xr, name="NT063 X (nháp người dựng)"))
    c, body, _ = req(base, "GET", f"/admin/workflows/{x['id']}", legacy)
    xl = wf(body)
    c, body, _ = req(base, "PUT", f"/admin/workflows/{x['id']}", legacy, rule_update(xl, name="NT063 X (khách đổi)"))
    check("quản trị viên khách di sản sửa luật X ⇒ 200 (áp thẳng như hôm nay)", c == 200 and wf(body).get("draftState") in (None, ""), f"{c}")
    c, b = publish(base, adm)
    d = b.get("details") or {}
    check("Xuất bản ⇒ 409 PUBLISH_CONFLICT nêu đúng luật X", c == 409 and b.get("code") == "PUBLISH_CONFLICT"
          and [i.get("docKey") for i in d.get("conflicts", [])] == [x["id"]], f"{c} {b.get('code', '')} {d}")
    c, st4 = state(base, adm)
    check("lần bị từ chối không tiêu nháp", st4.get("pendingCount") == 1, str(st4.get("pendingCount")))
    c, b = publish(base, adm, force=True)
    check("force ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    c, body, _ = req(base, "GET", f"/admin/workflows/{x['id']}", legacy)
    check("bản sống giờ là bản của người dựng (đã Ghi đè)", wf(body).get("name") == "NT063 X (nháp người dựng)", wf(body).get("name", ""))

    # ── 8. Tắt ngay ──────────────────────────────────────────────────────────────────────────────────
    print("\n8. Tắt ngay cho khách")
    c, body, _ = req(base, "GET", f"/admin/workflows/{y['id']}", adm)
    yr = wf(body)
    c, body, _ = req(base, "PUT", f"/admin/workflows/{y['id']}", adm, rule_update(yr, name="NT063 Y (nháp)"))
    check("người dựng sửa Y vào nháp", c == 200 and wf(body).get("draftState") == "edited", f"{c} {wf(body).get('draftState')}")
    c, b, _ = req(base, "POST", f"/admin/workflows/{y['id']}/disable-now", staff, {})
    check("nhân viên bấm Tắt ngay ⇒ 403", c == 403, f"{c}")
    c, b = create_opp(base, staff, cleanup, "Negotiation")
    check("trước khi tắt: nhân viên tạo ở Đàm phán ⇒ 422 (Y chạy)", vetoed(c, b, Y_MSG), f"{c} {b.get('code', '')}")
    c, body, _ = req(base, "POST", f"/admin/workflows/{y['id']}/disable-now", adm, {})
    off = wf(body)
    check("Tắt ngay ⇒ 200, bản sống isActive:false, tên sống không đổi", c == 200 and off.get("isActive") is False and off.get("name") == yr.get("name"),
          f"{c} active={off.get('isActive')} name={off.get('name')}")
    c, b = create_opp(base, staff, cleanup, "Negotiation")
    check("nhân viên tạo ở Đàm phán ⇒ 201 NGAY (Y đã tắt, không cần Xuất bản)", c == 201, f"{c} {b.get('code', '')}")
    c, st5 = state(base, adm)
    check("nháp của Y còn nguyên (không đụng nháp)", st5.get("pendingCount") == 1, str(st5.get("pendingCount")))
    c, b = publish(base, adm)
    d = b.get("details") or {}
    check("Xuất bản kế tiếp ⇒ 409 PUBLISH_CONFLICT nêu đúng Y", c == 409 and b.get("code") == "PUBLISH_CONFLICT"
          and [i.get("docKey") for i in d.get("conflicts", [])] == [y["id"]], f"{c} {b.get('code', '')} {d}")
    c, body, _ = req(base, "POST", "/admin/workflows", adm, veto_rule("NT063 Z", "Prospecting", "NT063 Z"))
    z = wf(body)
    if c == 201:
        cleanup.append(("workflow", z["id"]))
    c, b, _ = req(base, "POST", f"/admin/workflows/{z.get('id', '')}/disable-now", adm, {})
    check("luật chỉ có trong nháp ⇒ Tắt ngay 404 WORKFLOW_NOT_FOUND", c == 404 and b.get("code") == "WORKFLOW_NOT_FOUND", f"{c} {b.get('code', '')}")

    # ── 9. mã nháp lạ ────────────────────────────────────────────────────────────────────────────────
    print("\n9. Mã nháp lạ")
    before = state(base, adm)[1].get("pendingCount")
    for label, ident in [("UUID lạ", str(uuid.uuid4())), ("không phải UUID", "khong-phai-uuid")]:
        c, b, _ = req(base, "DELETE", f"/admin/publish/changes/{ident}", adm)
        check(f"bỏ thay đổi bằng {label} ⇒ 404 DRAFT_NOT_FOUND", c == 404 and b.get("code") == "DRAFT_NOT_FOUND", f"{c} {b.get('code', '')}")
    check("không nháp nào bị đổi", state(base, adm)[1].get("pendingCount") == before)

    # ── 10. cài gói ──────────────────────────────────────────────────────────────────────────────────
    print("\n10. Cài gói vào dự án đã giao")
    n_before = len(((req(base, "GET", "/admin/workflows", adm)[1].get("data") or {}).get("workflows")) or [])
    c, b, _ = req(base, "POST", "/admin/verticals/install", adm, {"builtin": "crm"})
    check("cài gói ⇒ 409 PROJECT_DELIVERED_INSTALL_BLOCKED", c == 409 and b.get("code") == "PROJECT_DELIVERED_INSTALL_BLOCKED", f"{c} {b.get('code', '')}")
    n_after = len(((req(base, "GET", "/admin/workflows", adm)[1].get("data") or {}).get("workflows")) or [])
    check("không ghi gì (số luật không đổi)", n_before == n_after, f"{n_before} → {n_after}")

    run_s72(base, adm, staff, legacy, cleanup)
    run_s73(base, adm, staff, legacy, cleanup)
    run_s74(base, adm, staff, legacy, cleanup)
    run_s75(base, adm, adm_user, staff, legacy, legacy_uid, owner, cleanup, a.db)  # before 14: its required field would taint staff creates
    run_s78(base, adm, staff, legacy, cleanup)
    run_s76(base, adm, staff, legacy, owner, cleanup, a.db)
    run_s79(base, adm, staff, legacy, cleanup)
    run_s77(base, adm, staff, legacy, owner, cleanup)
    run_s710(base, adm, staff, legacy, owner, cleanup)
    run_s711b(base, adm, staff, cleanup)  # before 19: group 19 leaves the staff on a role that sees only ENTITY until cleanup
    run_s77b(base, adm, staff, staff_user, legacy, cleanup)
    run_s711(base, adm, staff, legacy, cleanup)


def rep(body):
    return (body.get("data") or {}).get("report") or {}


def dash(body):
    return (body.get("data") or {}).get("dashboard") or {}


def run_columns(body):
    return [c.get("name") for c in (body.get("data") or {}).get("columns") or []]


def run_s72(base, adm, staff, legacy, cleanup):
    """Group 11 — reports + dashboards through the draft layer (specs/063 S7-2, T062)."""
    print("\n11. Báo cáo + dashboard qua nháp (S7-2)")
    c, st = state(base, adm)
    if not st.get("delivered"):
        raise EnvError("nhóm 11 cần tenant đã giao (nhóm 2 phải chạy trước)")
    H = DRAFT_HDR
    # Group 8 leaves a pending draft of Y that conflicts with its "Tắt ngay": start from a clean slate, so a
    # PUBLISH_CONFLICT below can only be about the report / dashboard.
    c, _, h = changes(base, adm)
    c, b, _ = req(base, "DELETE", "/admin/publish/changes?changesetHash=" + h, adm)
    check("bỏ toàn bộ nháp còn lại của nhóm 8 ⇒ 200/204, về 0 nháp", c in (200, 204) and state(base, adm)[1].get("pendingCount") == 0, f"{c} {b.get('code', '')}")

    # -- report: create in the draft, invisible to the customer until published
    cfg = {"columns": ["name"]}
    c, body, _ = req(base, "POST", "/admin/reports", adm, {"name": "NT063 R", "entityName": ENTITY, "type": "tabular", "config": cfg, "isActive": True})
    r = rep(body)
    check("người dựng tạo báo cáo R ⇒ 201, draftState new", c == 201 and r.get("draftState") == "new", f"{c} {r.get('draftState')}")
    if c != 201:
        return
    cleanup.append(("report", r["id"]))
    rid = r["id"]
    names = lambda b: [x.get("id") for x in (b.get("data") or {}).get("reports") or []]
    c, b, _ = req(base, "GET", "/reports", adm, headers_extra=H)
    check("người dựng + header: GET /reports có R", c == 200 and rid in names(b), f"{c}")
    c, b, _ = req(base, "GET", "/reports", adm)
    check("người dựng KHÔNG header: GET /reports không có R", c == 200 and rid not in names(b), f"{c}")
    for label, tok in [("nhân viên", staff), ("quản trị viên khách di sản", legacy)]:
        c, b, _ = req(base, "GET", "/reports", tok, headers_extra=H)
        check(f"{label} + header giả: GET /reports không có R", c == 200 and rid not in names(b), f"{c}")
        c, b, _ = req(base, "GET", f"/reports/{rid}/run", tok, headers_extra=H)
        check(f"{label} + header giả: chạy R ⇒ không chạy được (R chỉ có trong nháp)", c != 200, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", f"/reports/{rid}/run", adm, headers_extra=H)
    check("người dựng + header chạy R chưa xuất bản ⇒ 200, draftState new", c == 200 and (b.get("data") or {}).get("draftState") == "new", f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", f"/admin/reports/{rid}", adm)
    check("mở R ở khung sửa (GET /admin/reports/:id) không 404", c == 200 and rep(b).get("id") == rid, f"{c}")

    c, b = publish(base, adm)
    check("Xuất bản báo cáo mới ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", f"/reports/{rid}/run", staff)
    check("sau Xuất bản nhân viên chạy R ⇒ 200 với cột [name]", c == 200 and run_columns(b) == ["name"], f"{c} {run_columns(b)}")

    # -- report: two consecutive draft saves keep both changes
    c, b, _ = req(base, "PUT", f"/admin/reports/{rid}", adm, {"name": "NT063 R", "entityName": ENTITY, "type": "tabular", "config": {"columns": ["name", "stage"]}, "isActive": True})
    check("lưu nháp lần 1 (thêm cột stage) ⇒ 200 draftState edited", c == 200 and rep(b).get("draftState") == "edited", f"{c} {rep(b).get('draftState')}")
    c, b, _ = req(base, "GET", f"/admin/reports/{rid}", adm)
    cur = rep(b)
    cols = list((cur.get("config") or {}).get("columns") or [])
    check("mở lại: khung sửa đọc cột stage từ bản nháp", cols == ["name", "stage"], str(cols))
    cur_cfg = dict(cur.get("config") or {})
    cur_cfg["columns"] = cols + ["amount"]
    c, b, _ = req(base, "PUT", f"/admin/reports/{rid}", adm, {"name": "NT063 R", "entityName": ENTITY, "type": "tabular", "config": cur_cfg, "isActive": True})
    check("lưu nháp lần 2 (thêm cột amount) ⇒ 200", c == 200, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", f"/admin/reports/{rid}", adm)
    check("cả hai thay đổi còn trong nháp: [name, stage, amount]", list((rep(b).get("config") or {}).get("columns") or []) == ["name", "stage", "amount"], str((rep(b).get("config") or {}).get("columns")))
    c, items, _ = changes(base, adm)
    n_r = sum(1 for it in items if it.get("docKey") == rid)
    check("danh sách thay đổi có ĐÚNG một dòng cho R", c == 200 and n_r == 1, str(n_r))

    # -- who sees the draft: run + CSV
    c, b, _ = req(base, "GET", f"/reports/{rid}/run", adm, headers_extra=H)
    check("người dựng + header chạy R ⇒ 3 cột + draftState edited", c == 200 and run_columns(b) == ["name", "stage", "amount"] and (b.get("data") or {}).get("draftState") == "edited",
          f"{c} {run_columns(b)}")
    for label, tok, hdr in [("người dựng KHÔNG header", adm, None), ("nhân viên + header giả", staff, H), ("quản trị viên khách di sản + header giả", legacy, H)]:
        c, b, _ = req(base, "GET", f"/reports/{rid}/run", tok, headers_extra=hdr)
        check(f"{label}: chạy R ⇒ bản sống (1 cột [name], không draftState)", c == 200 and run_columns(b) == ["name"] and "draftState" not in (b.get("data") or {}),
              f"{c} {run_columns(b)}")
    c, _, raw = req(base, "GET", f"/reports/{rid}/export.csv", adm, headers_extra=H)
    first = raw.decode("utf-8", "replace").lstrip("\ufeff").splitlines()[0] if raw else ""
    check("CSV của người dựng + header có 3 cột (Giai đoạn/stage…)", c == 200 and first.count(",") == 2, first)
    c, _, raw = req(base, "GET", f"/reports/{rid}/export.csv", staff, headers_extra=H)
    first = raw.decode("utf-8", "replace").lstrip("\ufeff").splitlines()[0] if raw else ""
    check("CSV của nhân viên + header giả chỉ có 1 cột", c == 200 and first.count(",") == 0, first)
    c, b, _ = req(base, "GET", "/admin/reports", legacy)
    check("quản trị viên khách di sản (admin, không phải người dựng) chỉ thấy bản sống: không draftState", c == 200 and all(x.get("draftState") in (None, "") for x in (b.get("data") or {}).get("reports") or []), f"{c}")

    c, b = publish(base, adm)
    check("Xuất bản sửa báo cáo ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", f"/reports/{rid}/run", staff)
    check("nhân viên chạy R ⇒ 3 cột sau Xuất bản", c == 200 and run_columns(b) == ["name", "stage", "amount"], f"{c} {run_columns(b)}")

    # -- SC-006: a draft identical to the live report is not a conflict
    c, b, _ = req(base, "GET", f"/admin/reports/{rid}", adm)
    same = rep(b)
    c, b, _ = req(base, "PUT", f"/admin/reports/{rid}", adm, {"name": same["name"], "entityName": same["entityName"], "type": same["type"], "config": same["config"], "isActive": same["isActive"]})
    c2, b2 = publish(base, adm)
    check("lưu y hệt bản sống rồi Xuất bản ⇒ 201, không PUBLISH_CONFLICT", c2 == 201 and b2.get("code") != "PUBLISH_CONFLICT", f"{c2} {b2.get('code', '')}")

    # -- dashboard: create, publish, put it in an app menu, delete in the draft
    widget = {"id": "w1", "title": "NT063 KPI", "type": "kpi", "entity": ENTITY, "metric": "count", "x": 0, "y": 0, "w": 3, "h": 2}
    c, body, _ = req(base, "POST", "/admin/dashboards", adm, {"name": "NT063 D", "widgets": [widget]})
    d = dash(body)
    check("người dựng tạo dashboard D ⇒ 201, draftState new", c == 201 and d.get("draftState") == "new", f"{c} {d.get('draftState')}")
    if c != 201:
        return
    cleanup.append(("dashboard", d["id"]))
    did = d["id"]
    dnames = lambda b: [x.get("id") for x in (b.get("data") or {}).get("dashboards") or []]
    c, b, _ = req(base, "GET", "/dashboards", adm, headers_extra=H)
    check("người dựng + header: GET /dashboards có D", c == 200 and did in dnames(b), f"{c}")
    for label, tok, hdr in [("người dựng KHÔNG header", adm, None), ("nhân viên + header giả", staff, H)]:
        c, b, _ = req(base, "GET", "/dashboards", tok, headers_extra=hdr)
        check(f"{label}: GET /dashboards không có D", c == 200 and did not in dnames(b), f"{c}")
        c, b, _ = req(base, "GET", f"/dashboards/{did}/run", tok, headers_extra=hdr)
        check(f"{label}: chạy D ⇒ không chạy được", c != 200, f"{c}")
    c, b, _ = req(base, "GET", f"/dashboards/{did}/run", adm, headers_extra=H)
    check("người dựng + header chạy D ⇒ 200, draftState new", c == 200 and ((b.get("data") or {}).get("dashboard") or {}).get("draftState") == "new", f"{c}")
    c, b = publish(base, adm)
    check("Xuất bản dashboard mới ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", f"/dashboards/{did}/run", staff)
    check("sau Xuất bản nhân viên chạy D ⇒ 200", c == 200, f"{c} {b.get('code', '')}")

    slug = "nt063" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/apps", adm, {"name": slug, "label": "NT063 app"})
    app_id = ((b.get("data") or {}).get("app") or b.get("data") or {}).get("id")
    if c != 201 or not app_id:
        raise EnvError(f"tạo app thử thất bại ({c} {b.get('code', '')})")
    cleanup.append(("app", app_id))
    c, b, _ = req(base, "PUT", f"/admin/apps/{app_id}/nav", adm, [{"itemType": "dashboard", "refId": did}])
    check("đặt D vào menu của app ⇒ 200 (nay là nháp: S7-4)", c == 200, f"{c} {b.get('code', '')}")
    c, b = publish(base, adm)
    check("Xuất bản menu có D ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    nav_has = lambda: any(i.get("refId") == did for i in ((req(base, "GET", f"/apps/{slug}/nav", legacy)[1].get("data") or {}).get("items") or []))
    check("khách (quản trị viên khách di sản) thấy tab D trong menu", nav_has())

    c, b, _ = req(base, "DELETE", f"/admin/dashboards/{did}", adm)
    check("người dựng xoá D ⇒ 204/200 (vào nháp)", c in (200, 204), f"{c} {b.get('code', '')}")
    check("xoá ở nháp KHÔNG gỡ mục menu: khách vẫn thấy tab D", nav_has())
    c, b, _ = req(base, "GET", f"/dashboards/{did}/run", staff)
    check("…và nhân viên vẫn chạy được D (không tab chết)", c == 200, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", f"/dashboards/{did}/run", adm, headers_extra=H)
    check("người dựng + header thấy D đánh dấu 'Sẽ xoá'", c == 200 and ((b.get("data") or {}).get("dashboard") or {}).get("draftState") == "deleted", f"{c}")
    c, b = publish(base, adm)
    check("Xuất bản việc xoá D ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    check("sau Xuất bản mục menu của D biến mất trong cùng giao dịch (không tab chết)", not nav_has())
    c, b, _ = req(base, "GET", f"/dashboards/{did}/run", staff)
    check("nhân viên chạy D đã xoá ⇒ không còn (404)", c in (404, 422), f"{c} {b.get('code', '')}")


def edit_view(body):
    return (((body.get("data") or {}).get("views") or {}).get("edit")) or {}


def section_titles(view):
    return [x.get("title") for x in view.get("sections") or []]


def public_accent(base, token=None, headers=None):
    c, b, _ = req(base, "GET", f"/auth/branding/{TENANT}", token, headers_extra=headers)
    return c, (((b.get("data") or {}).get("branding")) or {}).get("accentColor", "")


def run_s73(base, adm, staff, legacy, cleanup):
    """Group 12 — form + branding through the draft layer and the read-only draft preview (specs/063 S7-3, T075)."""
    print("\n12. Form, thương hiệu, xem thử bản nháp chỉ đọc (S7-3)")
    c, st = state(base, adm)
    if not st.get("delivered"):
        raise EnvError("nhóm 12 cần tenant đã giao (nhóm 2 phải chạy trước)")
    H = DRAFT_HDR
    c, _, h = changes(base, adm)
    req(base, "DELETE", "/admin/publish/changes?changesetHash=" + h, adm)  # clean slate: the next Publish is only about this group

    # -- layout: a draft form is invisible to the customer until Publish
    path = f"/metadata/layouts/{ENTITY}?view=edit"
    c, live_before, _ = req(base, "GET", path, staff)
    check("nhân viên đọc form hiện tại ⇒ 200", c == 200, f"{c}")
    titles_before = section_titles(edit_view(live_before))
    marker = "NT063 form"
    layout = {"version": 1, "sections": [{"title": marker, "columns": 2, "fields": ["name", "stage"]}]}
    c, body, _ = req(base, "POST", "/admin/layouts", adm, {"entityName": ENTITY, "viewType": "edit", "layoutJson": layout})
    lid = (body.get("data") or {}).get("id")
    check("người dựng lưu form ⇒ 200 (vào nháp) có id", c == 200 and bool(lid), f"{c} {body.get('code', '')}")
    if not lid:
        return
    cleanup.append(("layout", lid))
    c, b, _ = req(base, "GET", path, staff, headers_extra=H)
    check("nhân viên (+ header giả) VẪN thấy form cũ", c == 200 and section_titles(edit_view(b)) == titles_before and marker not in section_titles(edit_view(b)), str(section_titles(edit_view(b))))
    c, b, _ = req(base, "GET", path, legacy, headers_extra=H)
    check("quản trị viên khách di sản (+ header giả) VẪN thấy form cũ", c == 200 and marker not in section_titles(edit_view(b)), str(section_titles(edit_view(b))))
    c, b, _ = req(base, "GET", path, adm)
    check("người dựng KHÔNG header thấy form sống (không có form nháp)", c == 200 and marker not in section_titles(edit_view(b)), str(section_titles(edit_view(b))))
    c, b, _ = req(base, "GET", path, adm, headers_extra=H)
    v = edit_view(b)
    check("người dựng + header thấy form nháp, draftState new/edited", c == 200 and marker in section_titles(v) and v.get("draftState") in ("new", "edited"), f"{section_titles(v)} {v.get('draftState')}")
    c, items, _ = changes(base, adm)
    check("danh sách thay đổi có dòng layout", c == 200 and any(it.get("docType") == "layout" for it in items), str([it.get("docType") for it in items]))

    # -- branding: the public login page never shows the draft
    c, b, _ = req(base, "GET", "/admin/tenant/branding", adm)
    orig = ((b.get("data") or {}).get("branding")) or {}
    c, live_accent = public_accent(base)
    new_accent = "#123456" if live_accent.lower() != "#123456" else "#654321"
    c, b, _ = req(base, "PUT", "/admin/tenant/branding", adm, {**orig, "accentColor": new_accent})
    check("người dựng đổi màu ⇒ 200, draftState edited", c == 200 and (b.get("data") or {}).get("draftState") == "edited", f"{c} {(b.get('data') or {}).get('draftState')}")
    cleanup.append(("branding", orig))
    c, a = public_accent(base)
    check("trang đăng nhập CÔNG KHAI vẫn màu cũ", c == 200 and a == live_accent, f"{a} vs {live_accent}")
    c, a = public_accent(base, adm, H)
    check("…kể cả khi gửi token người dựng + header (không bao giờ trả nháp)", c == 200 and a == live_accent, f"{a} vs {live_accent}")
    c, b, _ = req(base, "GET", "/admin/tenant/branding", adm)
    check("người dựng đọc /admin/tenant/branding thấy màu nháp", c == 200 and ((b.get("data") or {}).get("branding") or {}).get("accentColor") == new_accent, str(b.get("data")))
    c, b, _ = req(base, "GET", "/admin/tenant/branding", legacy)
    check("quản trị viên khách di sản đọc thấy màu sống, không draftState", c == 200 and ((b.get("data") or {}).get("branding") or {}).get("accentColor", "") == live_accent and "draftState" not in (b.get("data") or {}), str(b.get("data")))

    # -- the preview tab never writes: builder + header → 403 PREVIEW_READ_ONLY on every write route
    fake = str(uuid.uuid4())
    for label, method, p, body in [
        ("tạo hồ sơ", "POST", f"/{ENTITY}", {"name": "NT063 khong duoc ghi"}),
        ("sửa hồ sơ", "PUT", f"/{ENTITY}/{fake}", {"name": "x"}),
        ("xoá hồ sơ", "DELETE", f"/{ENTITY}/{fake}", None),
        ("nhập CSV", "POST", f"/{ENTITY}/import", {}),
        ("sửa hàng loạt", "POST", f"/{ENTITY}/bulk-update", {}),
        ("tải file lên", "POST", f"/{ENTITY}/{fake}/files", {}),
        ("tạo người dùng (khu quản trị)", "POST", "/admin/users", {}),
        ("sửa vai (khu quản trị)", "PUT", f"/admin/roles/{fake}", {}),
        ("lưu form", "POST", "/admin/layouts", {"entityName": ENTITY, "viewType": "edit", "layoutJson": layout}),
    ]:
        c, b, _ = req(base, method, p, adm, body, headers_extra=H)
        check(f"người dựng + header: {label} ⇒ 403 PREVIEW_READ_ONLY", c == 403 and b.get("code") == "PREVIEW_READ_ONLY", f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "POST", f"/{ENTITY}", staff, {}, headers_extra=H)
    check("nhân viên + header giả: ghi KHÔNG bị mã PREVIEW_READ_ONLY (header bị bỏ qua)", b.get("code") != "PREVIEW_READ_ONLY", f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "POST", "/admin/users", adm, {})
    check("người dựng KHÔNG header: ghi khu quản trị không bị PREVIEW_READ_ONLY (Studio vẫn ghi)", b.get("code") != "PREVIEW_READ_ONLY", f"{c} {b.get('code', '')}")

    # -- Publish: the customer gets the new form and the public page the new colour
    c, b = publish(base, adm)
    check("Xuất bản form + màu ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", path, staff)
    check("sau Xuất bản nhân viên thấy form mới", c == 200 and marker in section_titles(edit_view(b)) and edit_view(b).get("draftState") is None, str(section_titles(edit_view(b))))
    c, a = public_accent(base)
    check("sau Xuất bản trang công khai đổi sang màu mới", c == 200 and a == new_accent, f"{a} vs {new_accent}")

    # -- "Về mặc định" is a draft too
    c, b, _ = req(base, "DELETE", f"/admin/layouts/{lid}", adm)
    check("người dựng bấm 'Về mặc định' ⇒ 200/204 (vào nháp)", c in (200, 204), f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", path, staff)
    check("khách vẫn thấy form tuỳ biến tới khi Xuất bản", c == 200 and marker in section_titles(edit_view(b)), str(section_titles(edit_view(b))))
    c, b = publish(base, adm)
    check("Xuất bản việc 'về mặc định' ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", path, staff)
    check("sau Xuất bản nhân viên thấy form mặc định (không còn form NT063)", c == 200 and marker not in section_titles(edit_view(b)), str(section_titles(edit_view(b))))


def app_nav(base, slug, token, headers=None):
    c, b, _ = req(base, "GET", f"/apps/{slug}/nav", token, headers_extra=headers)
    d = b.get("data") or {}
    return c, (d.get("app") or {}), [i.get("refId") for i in d.get("items") or []]


def run_s74(base, adm, staff, legacy, cleanup):
    """Group 13 - app properties + menu through the draft layer (specs/063 S7-4, T085)."""
    print("\n13. Menu + thuộc tính app qua nháp (S7-4)")
    st = state(base, adm)[1]
    if not st.get("delivered"):
        raise EnvError("nhóm 13 cần tenant đã giao (nhóm 2 phải chạy trước)")
    H = DRAFT_HDR
    c, _, h = changes(base, adm)
    req(base, "DELETE", "/admin/publish/changes?changesetHash=" + h, adm)  # clean slate

    widget = lambda t: {"id": "w1", "title": t, "type": "kpi", "entity": ENTITY, "metric": "count", "x": 0, "y": 0, "w": 3, "h": 2}
    slug = "nt074" + uuid.uuid4().hex[:6]
    # -- creating an app applies at once (Áp ngay): the customer side sees it with no publish
    c, b, _ = req(base, "POST", "/admin/apps", adm, {"name": slug, "label": "NT074 cũ"})
    app_id = ((b.get("data") or {}).get("app") or b.get("data") or {}).get("id")
    if c != 201 or not app_id:
        raise EnvError(f"tạo app thử thất bại ({c} {b.get('code', '')})")
    cleanup.append(("app", app_id))
    c, app, _ = app_nav(base, slug, staff)
    check("tạo app là ÁP NGAY: nhân viên thấy app mới không cần Xuất bản", c == 200 and app.get("label") == "NT074 cũ", f"{c}")
    check("…và không sinh nháp nào", state(base, adm)[1].get("pendingCount") == 0)

    # two dashboards: N (draft-only) and E (live, then put in the menu and published)
    c, body, _ = req(base, "POST", "/admin/dashboards", adm, {"name": "NT074 E", "widgets": [widget("E")]})
    eid = dash(body).get("id")
    c, body, _ = req(base, "POST", "/admin/dashboards", adm, {"name": "NT074 N", "widgets": [widget("N")]})
    nid = dash(body).get("id")
    if not eid or not nid:
        raise EnvError("tạo dashboard thử thất bại")
    cleanup.append(("dashboard", eid))
    cleanup.append(("dashboard", nid))
    c, b = publish(base, adm)
    check("Xuất bản hai dashboard E, N ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "PUT", f"/admin/apps/{app_id}/nav", adm, [{"itemType": "dashboard", "refId": eid}])
    check("đặt E vào menu ⇒ 200 (nháp)", c == 200, f"{c} {b.get('code', '')}")
    c, b = publish(base, adm)
    check("Xuất bản menu có E ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    check("nhân viên thấy tab E", eid in app_nav(base, slug, staff)[2])
    c, body, _ = req(base, "POST", "/admin/dashboards", adm, {"name": "NT074 M", "widgets": [widget("M")]})
    mid = dash(body).get("id")  # created AFTER the last publish: stays draft-only for the checks below
    cleanup.append(("dashboard", mid))

    # -- US5 #1: label + a draft-only dashboard in the menu
    c, b, _ = req(base, "PUT", f"/admin/apps/{app_id}", adm, {"label": "NT074 mới", "description": "", "icon": "", "color": "", "isDefault": False, "sortOrder": 0})
    check("người dựng đổi nhãn app ⇒ 200, draftState edited", c == 200 and (((b.get("data") or {}).get("app") or b.get("data") or {}).get("draftState") == "edited"), f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "PUT", f"/admin/apps/{app_id}/nav", adm, [{"itemType": "dashboard", "refId": eid}, {"itemType": "dashboard", "refId": mid}])
    check("thêm dashboard CHỈ-CÓ-TRONG-NHÁP M vào menu ⇒ 200 (trước S7-4: APP_NAV_REF_INVALID)", c == 200, f"{c} {b.get('code', '')}")
    c, app, refs = app_nav(base, slug, adm, H)
    check("người dựng + header: nhãn mới và tab M", c == 200 and app.get("label") == "NT074 mới" and mid in refs, f"{c} {app.get('label')} {refs}")
    for label, tok, hdr in [("nhân viên", staff, None), ("nhân viên + header giả", staff, H), ("quản trị viên khách di sản", legacy, None), ("người dựng KHÔNG header", adm, None)]:
        c, app, refs = app_nav(base, slug, tok, hdr)
        check(f"{label}: vẫn nhãn cũ + menu cũ (không tab M)", c == 200 and app.get("label") == "NT074 cũ" and mid not in refs and eid in refs, f"{c} {app.get('label')} {refs}")
    c, rows, _ = changes(base, adm)
    check("danh sách thay đổi có đúng MỘT dòng app (thuộc tính + menu gộp) và dashboard M", sum(1 for r in rows if r.get("docType") == "app") == 1 and any(r.get("docKey") == mid for r in rows), f"{[r.get('docType') for r in rows]}")

    # -- menu cannot point at a dashboard marked for deletion
    c, b, _ = req(base, "DELETE", f"/admin/dashboards/{nid}", adm)
    check("người dựng xoá N ở nháp ⇒ 204/200", c in (200, 204), f"{c}")
    c, b, _ = req(base, "PUT", f"/admin/apps/{app_id}/nav", adm, [{"itemType": "dashboard", "refId": nid}])
    check("menu trỏ dashboard đang 'Sẽ xoá' ⇒ 422 APP_NAV_REF_INVALID", c == 422 and b.get("code") == "APP_NAV_REF_INVALID", f"{c} {b.get('code', '')}")

    # -- US5 #3: E is deleted by the same publish that makes M visible: no dead tab, ever
    c, b, _ = req(base, "DELETE", f"/admin/dashboards/{eid}", adm)
    check("người dựng xoá E (đang nằm trong menu) ở nháp ⇒ 204/200", c in (200, 204), f"{c}")
    check("xoá ở nháp chưa gỡ tab E của khách", eid in app_nav(base, slug, staff)[2])
    c, app, refs = app_nav(base, slug, adm, H)
    check("xem trước đã không còn tab E (không dẫn tới dashboard đã xoá)", eid not in refs and mid in refs, f"{refs}")
    c, b = publish(base, adm)
    check("Xuất bản ⇒ 201, docCount 4 (M mới + xoá N + xoá E + app)", c == 201 and ((b.get("data") or b).get("docCount") == 4), f"{c} {b.get('code', '')} {b.get('data')}")
    c, app, refs = app_nav(base, slug, staff)
    check("nhân viên: nhãn mới, tab M có, tab E biến mất cùng lần xuất bản", c == 200 and app.get("label") == "NT074 mới" and mid in refs and eid not in refs, f"{c} {app.get('label')} {refs}")
    c, b, _ = req(base, "GET", f"/dashboards/{mid}/run", staff)
    check("nhân viên mở được tab M ⇒ 200", c == 200, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", f"/dashboards/{eid}/run", staff)
    check("E đã xoá ⇒ nhân viên không mở được (không tab chết)", c in (404, 422), f"{c}")

    # -- US5 #4: dropping the draft returns the preview to the running menu
    c, b, _ = req(base, "PUT", f"/admin/apps/{app_id}", adm, {"label": "NT074 nháp", "description": "", "icon": "", "color": "", "isDefault": False, "sortOrder": 0})
    c, app, _ = app_nav(base, slug, adm, H)
    check("sửa nhãn lần nữa: xem trước thấy nhãn nháp", app.get("label") == "NT074 nháp", f"{app.get('label')}")
    c, _, h = changes(base, adm)
    c, b, _ = req(base, "DELETE", "/admin/publish/changes?changesetHash=" + h, adm)
    check("bỏ nháp ⇒ 204/200", c in (200, 204), f"{c} {b.get('code', '')}")
    c, app, _ = app_nav(base, slug, adm, H)
    check("xem trước quay về nhãn đang chạy", app.get("label") == "NT074 mới", f"{app.get('label')}")

    # -- US5 #5: deleting an app applies at once and leaves no draft
    c, b, _ = req(base, "PUT", f"/admin/apps/{app_id}", adm, {"label": "NT074 sắp xoá", "description": "", "icon": "", "color": "", "isDefault": False, "sortOrder": 0})
    c, b, _ = req(base, "DELETE", f"/admin/apps/{app_id}", adm)
    check("xoá app ⇒ 204/200 (ÁP NGAY)", c in (200, 204), f"{c} {b.get('code', '')}")
    c, _, _ = app_nav(base, slug, staff)
    check("nhân viên: app đã biến mất NGAY (404)", c == 404, f"{c}")
    check("xoá app dọn luôn nháp của nó (không còn nháp nào để hồi sinh app)", state(base, adm)[1].get("pendingCount") == 0, str(state(base, adm)[1].get("pendingCount")))


def fields_of(base, entity, token, headers=None):
    c, b, _ = req(base, "GET", f"/metadata/fields/{entity}", token, headers_extra=headers)
    data = b.get("data") or []
    return c, {f.get("name"): f for f in data}, [f.get("name") for f in data]


def run_s78(base, adm, staff, legacy, cleanup):
    """Group 14 - new fields/tables hidden until Publish, required deferred, edits and reorder as drafts
    (specs/063 S7-8, T125). Everything on the customer side is judged with the non-admin staff account."""
    print("\n14. Trường và bảng mới chỉ hiện với khách khi Xuất bản (S7-8)")
    st = state(base, adm)[1]
    if not st.get("delivered"):
        raise EnvError("nhóm 14 cần tenant đã giao (nhóm 2 phải chạy trước)")
    H = DRAFT_HDR
    c, _, h = changes(base, adm)
    req(base, "DELETE", "/admin/publish/changes?changesetHash=" + h, adm)  # clean slate
    time.sleep(1.2)  # created_at has second precision: be strictly after the last publish's second

    fname = "nt078_" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/fields", adm, {"entityName": ENTITY, "name": fname, "label": "NT078 ưu tiên", "fieldType": "text", "isRequired": True})
    fid = ((b.get("data") or {}).get("field") or {}).get("id")
    if c != 201 or not fid:
        raise EnvError(f"tạo trường thử thất bại ({c} {b.get('code', '')} {b.get('error', '')})")
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/fields/{fid}", adm)))

    # 1. field list
    c, sf, _ = fields_of(base, ENTITY, staff)
    check("nhân viên KHÔNG thấy trường mới trong /metadata/fields", c == 200 and fname not in sf and "stage" in sf, f"{c} thấy={fname in sf}")
    c, lf, _ = fields_of(base, ENTITY, legacy)
    check("quản trị viên khách di sản KHÔNG thấy trường mới", c == 200 and fname not in lf, f"{c}")
    c, bf, _ = fields_of(base, ENTITY, adm, H)
    nf = bf.get(fname) or {}
    check("người dựng + header thấy trường mới: draftState new + pendingRequired + isRequired",
          c == 200 and nf.get("draftState") == "new" and nf.get("pendingRequired") is True and nf.get("isRequired") is True,
          str({k: nf.get(k) for k in ("draftState", "pendingRequired", "isRequired")}))
    c, nl, _ = fields_of(base, ENTITY, adm)
    check("người dựng KHÔNG header (đọc sống) cũng không thấy", c == 200 and fname not in nl, f"{c}")

    # 2. forms
    c, b, _ = req(base, "GET", f"/metadata/layouts/{ENTITY}", staff)
    check("form mặc định của nhân viên không có trường mới", c == 200 and fname not in json.dumps(edit_view(b)), f"{c}")
    c, b, _ = req(base, "GET", f"/metadata/layouts/{ENTITY}", adm, headers_extra=H)
    check("form của người dựng + header có trường mới", c == 200 and fname in json.dumps(edit_view(b)), f"{c}")

    # 3. required is deferred: staff keeps creating records
    c, b = create_opp(base, staff, cleanup, "Prospecting")
    check("nhân viên tạo cơ hội ⇒ 201 dù trường bắt buộc mới chưa Xuất bản", c == 201, f"{c} {b.get('code', '')}")

    # 4. a new table
    ent = "nt078e" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/entities", adm, {"name": ent, "label": "NT078 bảng", "fields": [{"name": "title", "label": "Tiêu đề", "fieldType": "text"}]})
    if c != 201:
        raise EnvError(f"tạo bảng thử thất bại ({c} {b.get('code', '')} {b.get('error', '')})")
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/entities/{ent}", adm)))
    names = lambda body: [e.get("name") for e in (body.get("data") or [])]
    # a staff role has no grant on a table nobody gave it to, so the table is hidden from them either way:
    # the cutoff is judged with the legacy customer admin (admin bypasses the table ACL, so only the cutoff hides it)
    c, b, _ = req(base, "GET", "/metadata/entities", legacy)
    check("quản trị viên khách di sản KHÔNG thấy bảng mới trong /metadata/entities", c == 200 and ent not in names(b) and ENTITY in names(b), f"{c}")
    c, b, _ = req(base, "GET", "/metadata/entities", adm, headers_extra=H)
    check("người dựng + header thấy bảng mới", c == 200 and ent in names(b), f"{c}")
    c, b, _ = req(base, "GET", f"/metadata/fields/{ent}", legacy)
    check("trường của bảng mới ⇒ 404 ENTITY_NOT_FOUND với quản trị viên khách di sản", c == 404 and b.get("code") == "ENTITY_NOT_FOUND", f"{c} {b.get('code', '')}")

    # 5. edit: label to the draft, options.indexed now
    c, b, _ = req(base, "PUT", f"/admin/fields/{fid}", adm, {"options": {"indexed": True}})
    check("bật indexed trên trường mới ⇒ 200", c == 200, f"{c} {b.get('code', '')} {b.get('error', '')}")
    c, b, _ = req(base, "GET", f"/{ENTITY}?sort={fname}&limit=1", staff)
    check("indexed ÁP NGAY: sắp xếp theo trường đó ⇒ 200 (không SORT_NOT_ALLOWED)", c == 200, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "PUT", f"/admin/fields/{fid}", adm, {"fieldType": "number"})
    d = b.get("details") or {}
    check("đổi kiểu ⇒ 422 FIELD_TYPE_IMMUTABLE kèm details.hint", c == 422 and b.get("code") == "FIELD_TYPE_IMMUTABLE" and bool(d.get("hint")), f"{c} {b.get('code', '')} {d}")
    c, sf, _ = fields_of(base, ENTITY, staff)
    stage = sf.get("stage") or {}
    old_label = stage.get("label")
    c, b, _ = req(base, "PUT", f"/admin/fields/{stage.get('id')}", adm, {"label": "NT078 nhãn nháp"})
    check("sửa nhãn trường đang chạy ⇒ 200 (vào nháp)", c == 200, f"{c} {b.get('code', '')}")
    c, sf, _ = fields_of(base, ENTITY, staff)
    check("nhân viên VẪN thấy nhãn cũ", c == 200 and (sf.get("stage") or {}).get("label") == old_label, str((sf.get("stage") or {}).get("label")))
    c, bf, _ = fields_of(base, ENTITY, adm, H)
    check("người dựng + header thấy nhãn nháp, draftState edited",
          c == 200 and (bf.get("stage") or {}).get("label") == "NT078 nhãn nháp" and (bf.get("stage") or {}).get("draftState") == "edited",
          str({k: (bf.get("stage") or {}).get(k) for k in ("label", "draftState")}))

    # 6. reorder: drafts only, the customer's order stays
    c, names_before, order_before = fields_of(base, ENTITY, staff)
    ids = [names_before[n].get("id") for n in order_before]
    orders_before = [names_before[n].get("fieldOrder") for n in order_before]
    swapped = [ids[1], ids[0]] + ids[2:] if len(ids) > 2 else ids
    c, b, _ = req(base, "PUT", "/admin/fields/reorder", adm, {"entityName": ENTITY, "order": swapped})
    check("sắp xếp lại ⇒ 200", c == 200, f"{c} {b.get('code', '')} {b.get('error', '')}")
    c, names_after, order_after = fields_of(base, ENTITY, staff)
    check("thứ tự (và field_order) mà nhân viên thấy KHÔNG đổi",
          order_after == order_before and [names_after[n].get("fieldOrder") for n in order_after] == orders_before, f"{order_before[:3]} → {order_after[:3]}")
    c, _, border = fields_of(base, ENTITY, adm, H)
    check("người dựng + header thấy thứ tự mới", c == 200 and border[:2] == [order_before[1], order_before[0]], str(border[:3]))

    # 7. drop the unrelated drafts (label, reorder) so only the new field is released
    c, items, _ = changes(base, adm)
    check("danh sách thay đổi có loại field", any(i.get("docType") == "field" for i in items), str([(i.get("docType"), i.get("label")) for i in items][:6]))
    for i in items:
        if i.get("docType") == "field" and i.get("docKey") != fid:
            req(base, "DELETE", f"/admin/publish/changes/{i.get('id')}", adm)
    c, items, _ = changes(base, adm)
    check("còn đúng nháp của trường mới (và bảng mới, chưa có nháp, nằm ở danh sách dạng `create`)",
          [i.get("docKey") for i in items if i.get("docType") == "field"] == [fid]
          and [(i.get("docKey"), i.get("op"), i.get("discardable")) for i in items if i.get("docType") == "entity"] == [(ent_id(items), "create", False)],
          str([(i.get("docType"), i.get("docKey"), i.get("op")) for i in items]))

    # 8. publish releases everything
    c, b = publish(base, adm, note="nghiem thu S7-8")
    check("Xuất bản ⇒ 201", c == 201, f"{c} {b.get('code', '')} {b.get('error', '')}")
    c, sf, _ = fields_of(base, ENTITY, staff)
    check("sau Xuất bản nhân viên thấy trường mới, đã bắt buộc", c == 200 and (sf.get(fname) or {}).get("isRequired") is True, str((sf.get(fname) or {}).get("isRequired")))
    c, b, _ = req(base, "GET", f"/metadata/layouts/{ENTITY}", staff)
    check("sau Xuất bản form của nhân viên có trường mới", c == 200 and fname in json.dumps(edit_view(b)), f"{c}")
    c, b, _ = req(base, "GET", "/metadata/entities", legacy)
    check("sau Xuất bản quản trị viên khách di sản thấy bảng mới", c == 200 and ent in names(b), f"{c}")
    c, b = create_opp(base, staff, cleanup, "Prospecting")
    check("sau Xuất bản tạo cơ hội thiếu trường bắt buộc ⇒ 422", c == 422, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "GET", f"/{ENTITY}?limit=1", staff)
    check("nhân viên vẫn đọc được danh sách cơ hội", c == 200, f"{c}")


def ent_id(items):
    """The doc key of the single entity row in a change list ('' when there is none or more than one)."""
    ids = [i.get("docKey") for i in items if i.get("docType") == "entity"]
    return ids[0] if len(ids) == 1 else ""


def entity_names(body):
    return [e.get("name") for e in (body.get("data") or [])]


def run_s79(base, adm, staff, legacy, cleanup):
    """Group 15 - deleting a field / table is deferred to Publish, with a status and Thử lại (specs/063 S7-9, T133).
    Customer side judged with staff `test@example.com` (fields) and the legacy customer admin (tables: a staff role has no
    grant on a table nobody gave it to). The failure that makes the retry real is a formula that starts reading the
    field AFTER the delete was drafted: the delete guard (FIELD_HAS_FORMULA_DEPENDANTS) refuses at Publish time."""
    print("\n15. Xoá trường/bảng chờ tới Xuất bản, Thử lại khi xoá lỗi (S7-9)")
    if not state(base, adm)[1].get("delivered"):
        raise EnvError("nhóm 15 cần tenant đã giao (nhóm 2 phải chạy trước)")
    H = DRAFT_HDR
    c, _, h = changes(base, adm)
    req(base, "DELETE", "/admin/publish/changes?changesetHash=" + h, adm)  # clean slate
    time.sleep(1.2)

    # ── 1. build what the customer already uses, and release it ──
    fname = "nt079_" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/fields", adm, {"entityName": ENTITY, "name": fname, "label": "NT079 số", "fieldType": "number"})
    fid = ((b.get("data") or {}).get("field") or {}).get("id")
    if c != 201 or not fid:
        raise EnvError(f"tạo trường thử thất bại ({c} {b.get('code', '')} {b.get('error', '')})")
    ent = "nt079e" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/entities", adm, {"name": ent, "label": "NT079 bảng", "fields": [{"name": "title", "label": "Tiêu đề", "fieldType": "text"}]})
    if c != 201:
        raise EnvError(f"tạo bảng thử thất bại ({c} {b.get('code', '')} {b.get('error', '')})")
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/fields/{fid}", adm)))  # 404 once the run removed it
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/entities/{ent}", adm)))
    # A new field / table is created live but hidden; only a publish releases them, and a publish needs a pending change:
    # a label edit of the new field is that change.
    c, b, _ = req(base, "PUT", f"/admin/fields/{fid}", adm, {"label": "NT079 số (đã dựng)"})
    if c != 200:
        raise EnvError(f"sửa nhãn trường thử thất bại ({c} {b.get('code', '')})")
    time.sleep(1.2)  # created_at has second precision (rounded): publish strictly after the new field's second, or it stays hidden
    c, b = publish(base, adm, note="nghiem thu S7-9: dung")
    check("Xuất bản trường + bảng thử để khách dùng ⇒ 201", c == 201, f"{c} {b.get('code', '')} {b.get('error', '')}")
    for i in range(2):
        c, b, _ = req(base, "POST", f"/{ent}", adm, {"title": f"NT079 dòng {i}"})
        if c != 201:
            raise EnvError(f"tạo hồ sơ thử trong bảng mới thất bại ({c} {b.get('code', '')})")
    count = lambda: len(((req(base, "GET", f"/{ent}?limit=50", legacy)[1]).get("data")) or [])
    c, sf, _ = fields_of(base, ENTITY, staff)
    check("nhân viên thấy trường vừa xuất bản", c == 200 and fname in sf, f"{c}")
    check("quản trị viên khách di sản thấy bảng + 2 hồ sơ", entity_names(req(base, "GET", "/metadata/entities", legacy)[1]).count(ent) == 1 and count() == 2, f"{count()}")

    # ── 2. the builder deletes both: drafts only, the customer carries on ──
    c, b, _ = req(base, "DELETE", f"/admin/fields/{fid}", adm)
    check("xoá trường ⇒ 204 (nháp)", c == 204, f"{c} {b.get('code', '')} {b.get('error', '')}")
    c, b, _ = req(base, "DELETE", f"/admin/entities/{ent}", adm)
    check("xoá bảng ⇒ 204 (nháp)", c == 204, f"{c} {b.get('code', '')} {b.get('error', '')}")
    c, sf, _ = fields_of(base, ENTITY, staff)
    check("nhân viên VẪN thấy trường, không dấu nháp", c == 200 and fname in sf and not (sf[fname].get("draftState")), str(sf.get(fname, {}).get("draftState")))
    # earlier groups published required fields: fill whatever is required so the create is judged on the 'sẽ xoá' field only
    extra = {n: "x" for n, f in sf.items() if f.get("isRequired") and f.get("fieldType") == "text" and n not in ("name", "stage")}
    c, b, _ = req(base, "POST", f"/{ENTITY}", staff, {"name": "NT079 " + uuid.uuid4().hex[:6], "stage": "Prospecting", fname: 7, **extra})
    rid = (b.get("data") or {}).get("id")
    if c == 201 and rid:
        cleanup.append(("record", rid))
    check("nhân viên vẫn tạo được cơ hội và GHI vào trường 'sẽ xoá' (dữ liệu còn nguyên)", c == 201, f"{c} {b.get('code', '')} {b.get('error', '')}")
    check("quản trị viên khách di sản vẫn thấy bảng trong danh sách và đủ 2 hồ sơ",
          entity_names(req(base, "GET", "/metadata/entities", legacy)[1]).count(ent) == 1 and count() == 2, f"{count()}")
    c, bf, _ = fields_of(base, ENTITY, adm, H)
    check("người dựng + header thấy trường draftState deleted", c == 200 and (bf.get(fname) or {}).get("draftState") == "deleted", str((bf.get(fname) or {}).get("draftState")))
    ents = {e.get("name"): e for e in (req(base, "GET", "/metadata/entities", adm, headers_extra=H)[1].get("data") or [])}
    check("người dựng + header thấy bảng draftState deleted", (ents.get(ent) or {}).get("draftState") == "deleted", str((ents.get(ent) or {}).get("draftState")))
    ents_live = {e.get("name"): e for e in (req(base, "GET", "/metadata/entities", legacy)[1].get("data") or [])}
    check("quản trị viên khách di sản KHÔNG thấy dấu nháp trên bảng", not (ents_live.get(ent) or {}).get("draftState"), str(ents_live.get(ent)))
    c, b, _ = req(base, "PUT", f"/admin/fields/{fid}", adm, {"label": "NT079 sửa"})
    check("sửa trường đang 'sẽ xoá' ⇒ 409 FIELD_PENDING_DELETE (không ghi đè nháp xoá)", c == 409 and b.get("code") == "FIELD_PENDING_DELETE", f"{c} {b.get('code', '')}")
    c, items, _ = changes(base, adm)
    mine = {(i.get("docType"), i.get("op")) for i in items if i.get("docKey") in (fid, None) or i.get("entityName") == ent or i.get("label") == "NT079 bảng"}
    check("danh sách thay đổi có xoá trường + xoá bảng", ("field", "delete") in mine and ("entity", "delete") in mine, str(sorted(mine)))

    # ── 3. Hoàn tác = bỏ thay đổi ──
    chg = next((i for i in items if i.get("docType") == "entity" and i.get("op") == "delete"), {})
    c, _, _ = req(base, "DELETE", f"/admin/publish/changes/{chg.get('id')}", adm)
    ents = {e.get("name"): e for e in (req(base, "GET", "/metadata/entities", adm, headers_extra=H)[1].get("data") or [])}
    check("Hoàn tác xoá bảng ⇒ 204 và hết dấu 'sẽ xoá'", c == 204 and not (ents.get(ent) or {}).get("draftState"), f"{c} {(ents.get(ent) or {}).get('draftState')}")
    c, _, _ = req(base, "DELETE", f"/admin/entities/{ent}", adm)
    check("xoá bảng lại ⇒ 204 (nháp mới)", c == 204, f"{c}")

    # ── 4. Xuất bản với một lỗi xoá có thật: công thức bắt đầu đọc trường SAU khi nháp xoá đã lập ──
    fx = "nt079f_" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/fields", adm, {"entityName": ENTITY, "name": fx, "label": "NT079 công thức", "fieldType": "formula",
                                                      "options": {"formula": f"{fname} * 2", "resultType": "number"}})
    fxid = ((b.get("data") or {}).get("field") or {}).get("id")
    if c != 201 or not fxid:
        raise EnvError(f"tạo công thức thử thất bại ({c} {b.get('code', '')} {b.get('error', '')})")
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/fields/{fxid}", adm)))
    c, items, hh = changes(base, adm)
    # the formula field was born after the last publish: it waits as a read-only create row (FR-060), next to the two deletes
    check("trước khi xuất bản: 2 thao tác xoá chờ + trường công thức mới (dòng create)",
          sorted((i.get("docType"), i.get("op"), i.get("docKey") if i.get("op") == "create" else "") for i in items if i.get("docType") in ("field", "entity"))
          == [("entity", "delete", ""), ("field", "create", fxid), ("field", "delete", "")],
          str([(i.get("docType"), i.get("op")) for i in items]))
    c, b, _ = req(base, "POST", "/admin/publish", adm, {"changesetHash": hh, "note": "nghiem thu S7-9: xoa"})
    d = b.get("data") or {}
    ops = {o.get("docType"): o for o in (d.get("schemaOps") or [])}
    check("Xuất bản ⇒ 201, schemaStatus failed (xoá trường bị công thức chặn)", c == 201 and d.get("schemaStatus") == "failed", f"{c} {b.get('code', '')} {d.get('schemaStatus')}")
    check("thao tác xoá bảng xong, xoá trường lỗi kèm lý do nêu công thức",
          (ops.get("entity") or {}).get("applyStatus") == "applied" and (ops.get("field") or {}).get("applyStatus") == "failed" and "công thức" in ((ops.get("field") or {}).get("error") or ""),
          str(ops))
    check("khách: bảng đã biến mất (danh sách + hồ sơ), trường còn", entity_names(req(base, "GET", "/metadata/entities", legacy)[1]).count(ent) == 0
          and req(base, "GET", f"/{ent}", legacy)[0] == 404 and fname in fields_of(base, ENTITY, staff)[1], "")
    st = state(base, adm)[1]
    retry = st.get("schemaRetry") or {}
    check("state.schemaRetry nêu phiên bản + đúng 1 thao tác còn lại (trường)", bool(retry.get("versionId")) and [o.get("docType") for o in (retry.get("ops") or [])] == ["field"], str(retry))
    vid = retry.get("versionId", "")

    # ── 5. thử lại: cổng, rồi sửa nguyên nhân, rồi chạy ──
    for who, tok in (("nhân viên", staff), ("quản trị viên khách di sản", legacy)):
        c, b, _ = req(base, "POST", f"/admin/publish/versions/{vid}/retry-schema", tok)
        check(f"{who} gọi retry-schema ⇒ 403", c == 403, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{uuid.uuid4()}/retry-schema", adm)
    check("người dựng + mã phiên bản lạ ⇒ 404 VERSION_NOT_FOUND (cặp với 403 ở trên: route có thật)", c == 404 and b.get("code") == "VERSION_NOT_FOUND", f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "POST", "/admin/publish/versions/not-a-uuid/retry-schema", adm)
    check("mã không phải UUID ⇒ 404 VERSION_NOT_FOUND (giống hệt 'không tồn tại')", c == 404 and b.get("code") == "VERSION_NOT_FOUND", f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{vid}/retry-schema", adm)
    check("thử lại khi công thức còn đó ⇒ 200 vẫn failed, không đổi gì", c == 200 and (b.get("data") or {}).get("schemaStatus") == "failed", f"{c} {(b.get('data') or {}).get('schemaStatus')}")
    # the cause is fixed the way the builder fixes anything: delete the formula (a draft), publish, then retry the old delete
    c, b, _ = req(base, "DELETE", f"/admin/fields/{fxid}", adm)
    check("người dựng xoá công thức ⇒ 204 (nháp xoá)", c == 204, f"{c} {b.get('code', '')}")
    c, b = publish(base, adm, note="nghiem thu S7-9: go cong thuc")
    check("Xuất bản xoá công thức ⇒ 201, xoá xong (schemaStatus done)", c == 201 and (b.get("data") or {}).get("schemaStatus") == "done", f"{c} {b.get('code', '')} {(b.get('data') or {}).get('schemaStatus')}")
    check("phiên bản cũ vẫn đang chờ thử lại (xuất bản mới không nuốt nó)", ((state(base, adm)[1].get("schemaRetry") or {}).get("versionId")) == vid, str(state(base, adm)[1].get("schemaRetry")))
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{vid}/retry-schema", adm)
    d = b.get("data") or {}
    check("thử lại ⇒ 200 schemaStatus done", c == 200 and d.get("schemaStatus") == "done", f"{c} {b.get('code', '')} {d.get('schemaStatus')}")
    c, sf, _ = fields_of(base, ENTITY, staff)
    check("nhân viên không còn thấy trường đã xoá", c == 200 and fname not in sf, f"{c}")
    st = state(base, adm)[1]
    check("state.schemaRetry về null, không còn nháp", st.get("schemaRetry") is None and st.get("pendingCount") == 0, f"{st.get('schemaRetry')} {st.get('pendingCount')}")
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{vid}/retry-schema", adm)
    check("thử lại lần nữa ⇒ 409 SCHEMA_RETRY_NOT_NEEDED", c == 409 and b.get("code") == "SCHEMA_RETRY_NOT_NEEDED", f"{c} {b.get('code', '')}")


def sql(db, query):
    """Rows (list of tab-split strings) of one read-only query on the clone DB, through the compose MySQL."""
    out = subprocess.run(["docker", "exec", "nexus-mysql", "mysql", "-uroot", "--default-character-set=utf8mb4", "-N", "-B", db, "-e", query],
                         capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=30)
    if out.returncode != 0:
        raise EnvError(f"đọc DB {db} thất bại: {out.stderr.strip()[:200]}")
    return [line.split("\t") for line in out.stdout.splitlines() if line]


def direct_entries(db, tid):
    return sql(db, "SELECT id, IFNULL(version_no,'NULL'), created_by, created_by_label FROM sys_meta_versions "
                   f"WHERE tenant_id='{tid}' AND kind='direct' ORDER BY seq")


def entry_doc(db, tid, version_id):
    rows = sql(db, "SELECT doc_type, op, IFNULL(CAST(before_json AS CHAR),''), IFNULL(CAST(after_json AS CHAR),''), "
                   f"IFNULL(CAST(meta AS CHAR),'') FROM sys_meta_version_docs WHERE tenant_id='{tid}' AND version_id='{version_id}'")
    return rows[0] if rows else []


def run_s75(base, adm, adm_user, staff, legacy, legacy_uid, owner, cleanup, db):
    """Group 15 - direct version entries (specs/063 S7-5, T094)."""
    print("\n15. Mục áp ngay (S7-5)")
    if not db:
        print("  (bỏ qua, không tính phép kiểm: thiếu --db nên không đọc được sys_meta_versions)")
        return
    st = state(base, adm)[1]
    if not st.get("delivered"):
        raise EnvError("nhóm 15 cần tenant đã giao (nhóm 2 phải chạy trước)")
    tid = sql(db, f"SELECT id FROM tenants WHERE slug='{TENANT}'")[0][0]
    builder_id, builder_name = adm_user.get("id"), adm_user.get("fullName") or adm_user.get("email")

    # 1. the staff member is refused at once, on the role routes themselves
    c, b, raw = req(base, "GET", "/admin/roles", staff)
    _, _, reference = req(base, "GET", "/admin/users", staff)
    check("nhân viên gọi /admin/roles ⇒ 403 NGAY", c == 403 and raw == reference, f"{c}")
    if owner:
        c, _, raw = req(base, "PUT", f"/admin/workflows/{uuid.uuid4()}", owner, {})
        n_before_owner = len(direct_entries(db, tid))
        check("chủ khách không quản trị: PUT /admin/workflows/:id ⇒ 403 thân giống RequireAdmin", c == 403 and raw == reference, f"{c}")
        check("…và không sinh mục direct", len(direct_entries(db, tid)) == n_before_owner)

    # 2. the builder creates a role: one direct entry, in the builder's name, version_no NULL, with an after image
    n0 = len(direct_entries(db, tid))
    rname = "nt075_" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/roles", adm, {"name": rname, "label": "NT075 vai"})
    role = ((b.get("data") or {}).get("role")) or {}
    rid = role.get("id")
    if c != 201 or not rid:
        raise EnvError(f"tạo vai thử thất bại ({c} {b.get('code', '')})")
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/roles/{rid}", adm)))
    check("tạo vai ⇒ 201 và không kèm versionWarning", "versionWarning" not in b, json.dumps(b)[:120])
    es = direct_entries(db, tid)
    check("tạo vai ⇒ đúng MỘT mục direct, version_no NULL, người thực hiện = người dựng",
          len(es) == n0 + 1 and es[-1][1] == "NULL" and es[-1][2] == builder_id and es[-1][3] == builder_name, f"{len(es) - n0} {es[-1] if es else ''}")
    d = entry_doc(db, tid, es[-1][0])
    check("…op create, có ảnh sau, không ảnh trước", d[:2] == ["role", "create"] and d[2] == "" and rname in d[3], str(d)[:100])

    # 3. grants: before/after differ
    c, b, _ = req(base, "PUT", f"/admin/roles/{rid}", adm, {"actions": [{"entityName": ENTITY, "action": "view", "accessLevel": "all"}]})
    es2 = direct_entries(db, tid)
    d = entry_doc(db, tid, es2[-1][0]) if len(es2) > len(es) else []
    check("lưu ma trận grant ⇒ 200 + mục direct có trước/sau khác nhau (trước 0 grant, sau 1)",
          c == 200 and len(es2) == len(es) + 1 and d[:2] == ["role", "update"] and ENTITY not in d[2] and ENTITY in d[3], f"{c} {str(d)[:100]}")

    # 4. role delete: holders + both field-permission rows are read BEFORE the FKs take them
    c, fb, _ = req(base, "GET", f"/metadata/fields/{ENTITY}", adm)
    fields = [f for f in (fb.get("data") if isinstance(fb.get("data"), list) else (fb.get("data") or {}).get("fields") or []) if isinstance(f, dict) and not f.get("isRequired")
              and f.get("fieldType") in ("text", "number", "date", "select") and f.get("name") not in ("id", "stage")]
    if len(fields) >= 2:
        grants = [{"roleId": rid, "entityName": ENTITY, "fieldName": f["name"], "canView": True, "canEdit": False} for f in fields[:2]]
        c, b, _ = req(base, "PUT", "/admin/field-acl", adm, {"grants": grants})
        check("lưu quyền theo trường (2 dòng) ⇒ 200", c == 200, f"{c} {b.get('code', '')}")
        c, b, _ = req(base, "PUT", f"/admin/users/{legacy_uid}", adm, {"roleId": rid})
        check("gán vai cho tài khoản thử ⇒ 200", c == 200, f"{c} {b.get('code', '')}")
        n1 = len(direct_entries(db, tid))
        c, _, _ = req(base, "DELETE", f"/admin/roles/{rid}", adm)
        es3 = direct_entries(db, tid)
        d = entry_doc(db, tid, es3[-1][0]) if len(es3) > n1 else []
        meta = {}
        try:
            meta = json.loads(d[4]) if d and d[4] else {}
        except ValueError:
            pass
        check("xoá vai ⇒ 204 + mục direct ghi người đang giữ vai VÀ đủ 2 dòng quyền trường",
              c == 204 and len(es3) == n1 + 1 and d[:2] == ["role", "delete"] and legacy_uid in (meta.get("assignedUserIds") or [])
              and len(meta.get("fieldAcl") or []) == 2, f"{c} {str(meta)[:140]}")
    else:
        print("  (bỏ qua xoá-vai-có-quyền-trường: bảng thử không đủ 2 trường không-bắt-buộc)")

    # 5. a legacy customer admin's rule write applies at once AND leaves an entry in THEIR name
    msg = "NT075 chan Thua"
    n2 = len(direct_entries(db, tid))
    c, body, _ = req(base, "POST", "/admin/workflows", legacy, veto_rule("NT075 R", "Closed Lost", msg))
    rule = wf(body)
    if c == 201 and rule.get("id"):
        cleanup.append(("workflow", rule["id"]))
    check("quản trị viên khách di sản tạo luật ⇒ 201, sống ngay (không draftState)", c == 201 and rule.get("draftState") is None, f"{c} {rule.get('draftState')}")
    c, b = create_opp(base, staff, cleanup, "Closed Lost")
    check("nhân viên bị luật đó chặn NGAY (áp ngay, không đợi Xuất bản)", vetoed(c, b, msg), f"{c} {b.get('code', '')}")
    es4 = direct_entries(db, tid)
    check("…mục direct mang TÊN và id quản trị viên khách di sản", len(es4) == n2 + 1 and es4[-1][2] == legacy_uid
          and es4[-1][3] == "Nghiệm thu nháp (tạm)", f"{es4[-1] if es4 else ''}")
    if rule.get("id"):
        c, b, _ = req(base, "PUT", f"/admin/workflows/{rule['id']}", legacy, rule_update(rule, isActive=False))
        es5 = direct_entries(db, tid)
        d = entry_doc(db, tid, es5[-1][0]) if len(es5) > len(es4) else []
        check("sửa luật ⇒ mục thứ hai có trước ≠ sau", c == 200 and len(es5) == len(es4) + 1 and d[:2] == ["workflow", "update"] and d[2] != d[3], f"{c}")

    # 6. acl-backfill decides from the LIVE menu: an entity only in the DRAFT menu is skipped, nothing is written
    slug = "nt075" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/apps", adm, {"name": slug, "label": "NT075 app"})
    app_id = ((b.get("data") or {}).get("app") or b.get("data") or {}).get("id")
    if c != 201 or not app_id:
        raise EnvError(f"tạo app thử thất bại ({c} {b.get('code', '')})")
    cleanup.append(("app", app_id))
    es6 = direct_entries(db, tid)
    d = entry_doc(db, tid, es6[-1][0])
    check("tạo app ⇒ mục direct docType app, op create", d[:2] == ["app", "create"], str(d)[:80])
    ents = (req(base, "GET", "/metadata/entities", adm)[1].get("data")) or []
    ents = ents.get("entities", []) if isinstance(ents, dict) else ents
    eid = next((e.get("id") for e in ents if e.get("name") == ENTITY), None)
    c, b, _ = req(base, "PUT", f"/admin/apps/{app_id}/nav", adm, [{"itemType": "entity", "refId": eid}])
    check("đặt bảng vào menu (nháp) ⇒ 200", c == 200 and bool(eid), f"{c} {b.get('code', '')}")
    n3 = len(direct_entries(db, tid))
    c, b, _ = req(base, "POST", f"/admin/apps/{app_id}/acl-backfill?dryRun=false", adm, {})
    res = b.get("data") or {}
    check("acl-backfill bỏ qua bảng chỉ có trong menu NHÁP: không seed, không mục direct",
          c == 200 and not res.get("seeded") and len(direct_entries(db, tid)) == n3, f"{c} seeded={res.get('seeded')}")


SECRET_KEY = "NT076-SIGNING-KEY-xyz"
SECRET_HDR = "Bearer NT076-HDR-abc"


def run_s76(base, adm, staff, legacy, owner, cleanup, db):
    """Group 16 - what exactly changes + publish history (specs/063 S7-6, T104)."""
    print("\n16. Đổi gì và lịch sử xuất bản (S7-6)")
    _, _, reference = req(base, "GET", "/admin/users", staff)

    # 1. an update lists WHERE it differs; a brand-new rule lists nothing
    c, body, _ = req(base, "POST", "/admin/workflows", adm, {
        "name": "NT076 hook", "entityName": ENTITY, "trigger": "after_create", "isActive": True,
        "actions": [{"type": "outbound_webhook", "url": "https://example.com/hook"}]})
    rule = wf(body)
    if c != 201 or not rule.get("id"):
        raise EnvError(f"tạo luật webhook thử thất bại ({c} {body.get('code', '')})")
    cleanup.append(("workflow", rule["id"]))
    c, items, _ = changes(base, adm)
    new = next((i for i in items if i.get("docKey") == rule["id"]), {})
    check("luật mới trong nháp ⇒ op create, changedPaths rỗng", new.get("op") == "create" and new.get("changedPaths") == [], str(new.get("changedPaths")))
    c, b = publish(base, adm, note="NT076 webhook")
    check("Xuất bản luật webhook ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    if db:  # a rule saved before the key vault (specs/050) still carries its signing key + headers inline
        sql(db, "UPDATE sys_workflows SET actions = JSON_ARRAY(JSON_OBJECT('type','outbound_webhook','url','https://example.com/hook',"
                f"'secret','{SECRET_KEY}','headers',JSON_OBJECT('Authorization','{SECRET_HDR}'))) WHERE id='{rule['id']}'")
    c, b, _ = req(base, "PUT", f"/admin/workflows/{rule['id']}", adm,
                  rule_update(rule, name="NT076 hook đổi tên", actions=[{"type": "outbound_webhook", "url": "https://example.com/hook2"}]))
    check("sửa tên + URL luật ⇒ 200 (vào nháp)", c == 200, f"{c} {b.get('code', '')}")
    c, items, _ = changes(base, adm)
    upd = next((i for i in items if i.get("docKey") == rule["id"]), {})
    check("sửa luật ⇒ op update, changedPaths có name và actions[0].url",
          upd.get("op") == "update" and "name" in upd.get("changedPaths", []) and "actions[0].url" in upd.get("changedPaths", []), str(upd.get("changedPaths")))
    c, b = publish(base, adm, note="NT076 doi ten")
    check("Xuất bản bản sửa ⇒ 201", c == 201, f"{c} {b.get('code', '')}")

    # 2. who may read the history
    for who, tok in [("nhân viên", staff), ("quản trị viên khách di sản", legacy), ("chủ khách", owner)]:
        if not tok:
            continue
        for path in ("/admin/publish/versions", f"/admin/publish/versions/{uuid.uuid4()}"):
            c, _, raw = req(base, "GET", path, tok)
            check(f"{who} GET {path.split('/admin/publish')[1][:10]}… ⇒ 403 thân giống RequireAdmin", c == 403 and raw == reference, f"{c}")
    c, b, raw_list = req(base, "GET", "/admin/publish/versions?limit=2", adm)
    vs = b.get("data") or []
    check("người dựng đọc lịch sử ⇒ 200, mới nhất trước, limit=2 tôn trọng, có total",
          c == 200 and len(vs) == 2 and vs[0]["seq"] > vs[1]["seq"] and (b.get("pagination") or {}).get("total", 0) >= 3 and vs[0]["note"] == "NT076 doi ten",
          f"{c} n={len(vs)} {b.get('pagination')}")

    # 3. detail: before/after + changedPaths, and no secret anywhere
    first = next((v for v in (req(base, "GET", "/admin/publish/versions?limit=100", adm)[1].get("data") or []) if v.get("note") == "NT076 webhook"), {})
    c, b, raw_first = req(base, "GET", f"/admin/publish/versions/{first.get('id', '')}", adm)
    d0 = ((b.get("data") or {}).get("docs") or [{}])[0]
    check("chi tiết phiên bản đầu: 1 mục create có after", c == 200 and d0.get("op") == "create" and d0.get("after") is not None, f"{c} {d0.get('op')}")
    c, b, raw_last = req(base, "GET", f"/admin/publish/versions/{vs[0]['id']}", adm)
    d1 = ((b.get("data") or {}).get("docs") or [{}])[0]
    check("chi tiết phiên bản sửa: op update, changedPaths có name", c == 200 and d1.get("op") == "update" and "name" in d1.get("changedPaths", []), str(d1.get("changedPaths")))
    c, _, raw_changes = req(base, "GET", "/admin/publish/changes", adm)
    _, _, raw_audit = req(base, "GET", "/audit-log?" + urllib.parse.urlencode({"limit": "200"}), legacy)
    _, _, raw_csv = req(base, "GET", "/audit-log/export.csv", legacy)
    blob = b"".join([raw_list, raw_first, raw_last, raw_changes, raw_audit, raw_csv])
    leaked = [k for k in (SECRET_KEY, SECRET_HDR) if k.encode() in blob]
    check("không phản hồi/nhật ký nào chứa khoá ký hay giá trị header webhook", not leaked, str(leaked))

    # 4. unknown / malformed id: one answer
    c1, b1, raw1 = req(base, "GET", f"/admin/publish/versions/{uuid.uuid4()}", adm)
    c2, b2, raw2 = req(base, "GET", "/admin/publish/versions/khong-phai-uuid", adm)
    check("mã phiên bản lạ / không phải UUID ⇒ 404 VERSION_NOT_FOUND, thân giống hệt",
          c1 == 404 and c2 == 404 and b1.get("code") == "VERSION_NOT_FOUND" and raw1 == raw2, f"{c1} {c2} {b1.get('code', '')}")


def head_no(base, adm):
    return ((state(base, adm)[1].get("headVersion") or {}).get("versionNo")) or 0


def report_cols(base, tok, rid):
    c, b, _ = req(base, "GET", f"/reports/{rid}/run", tok)
    return c, run_columns(b)


def report_names(base, tok):
    c, b, _ = req(base, "GET", "/reports", tok)
    return {x.get("id"): x.get("name") for x in (b.get("data") or {}).get("reports") or []}


def run_s77(base, adm, staff, legacy, owner, cleanup):
    """Group 17 - go back to an earlier version (specs/063 S7-7, T111)."""
    print("\n17. Quay lại một phiên bản cũ (S7-7)")
    _, _, reference = req(base, "GET", "/admin/users", staff)
    c, _, h = changes(base, adm)
    req(base, "DELETE", "/admin/publish/changes?changesetHash=" + h, adm)  # clean slate
    cfg = {"columns": ["name"]}

    def new_report(name, cols):
        c, body, _ = req(base, "POST", "/admin/reports", adm, {"name": name, "entityName": ENTITY, "type": "tabular", "config": {"columns": cols}, "isActive": True})
        r = rep(body)
        if c != 201 or not r.get("id"):
            raise EnvError(f"tạo báo cáo thử {name} thất bại ({c} {body.get('code', '')})")
        cleanup.append(("report", r["id"]))
        return r["id"]

    def edit_report(rid, name, cols):
        return req(base, "PUT", f"/admin/reports/{rid}", adm, {"name": name, "entityName": ENTITY, "type": "tabular", "config": {"columns": cols}, "isActive": True})[0]

    # history: C published, a customer admin edits C at once, A published (the target), then B + edits + A deleted
    rc = new_report("NT077 C", ["name"])
    c, b = publish(base, adm, note="NT077 C")
    check("dựng lịch sử: Xuất bản C ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    cc, cb, _ = req(base, "PUT", f"/admin/reports/{rc}", legacy, {"name": "NT077 C (khách sửa)", "entityName": ENTITY, "type": "tabular", "config": cfg, "isActive": True})
    check("quản trị viên khách di sản sửa C ⇒ áp NGAY (mục direct, trước bản đích)", cc == 200, f"{cc} {cb.get('code', '')}")
    ra = new_report("NT077 A", ["name"])
    c, b = publish(base, adm, note="NT077 A")
    check("Xuất bản A ⇒ 201 (đây là bản đích)", c == 201, f"{c} {b.get('code', '')}")
    target_no = head_no(base, adm)
    rb = new_report("NT077 B", ["name"])
    edit_report(ra, "NT077 A", ["name", "stage"])
    edit_report(rc, "NT077 C v3", ["name"])
    c, b = publish(base, adm, note="NT077 B + sửa A, C")
    check("Xuất bản B + sửa A, C ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    req(base, "DELETE", f"/admin/reports/{ra}", adm)
    c, b = publish(base, adm, note="NT077 xoá A")
    check("Xuất bản xoá A ⇒ 201", c == 201, f"{c} {b.get('code', '')}")
    head = head_no(base, adm)
    versions = (req(base, "GET", "/admin/publish/versions?limit=100", adm)[1].get("data") or [])
    tid = next((v["id"] for v in versions if v.get("versionNo") == target_no), "")
    c, cols_a = report_cols(base, staff, ra)
    check("trước khi quay lại: nhân viên không chạy được A (đã xoá), thấy B", c != 200 and rb in report_names(base, staff), f"{c}")

    # who may call it: 403 byte-identical, paired with the builder's 200 on the same path
    for who, tok in [("nhân viên", staff), ("quản trị viên khách di sản", legacy), ("chủ khách", owner)]:
        if not tok:
            continue
        c1, _, r1 = req(base, "GET", f"/admin/publish/versions/{tid}/rollback-plan", tok)
        c2, _, r2 = req(base, "POST", f"/admin/publish/versions/{tid}/rollback", tok, {"expectedHeadVersion": head})
        check(f"{who} gọi rollback-plan / rollback ⇒ 403 thân giống RequireAdmin", c1 == 403 and c2 == 403 and r1 == reference and r2 == reference, f"{c1} {c2}")
    c, plan, _ = req(base, "GET", f"/admin/publish/versions/{tid}/rollback-plan", adm)
    pd = plan.get("data") or {}
    ops = {(x["docType"], x["docKey"]): x["op"] for x in pd.get("changes") or []}
    check("rollback-plan (người dựng) ⇒ 200: A dựng lại, B xoá, C đưa về bản khách sửa",
          c == 200 and ops.get(("report", ra)) == "create" and ops.get(("report", rb)) == "delete" and ops.get(("report", rc)) == "update"
          and pd.get("targetVersionNo") == target_no and pd.get("headVersionNo") == head, f"{c} {ops}")
    check("rollback-plan không chứa thân tài liệu", all(set(x) <= {"docType", "docKey", "label", "op"} for x in pd.get("changes") or []))
    c1, b1, r1 = req(base, "GET", f"/admin/publish/versions/{uuid.uuid4()}/rollback-plan", adm)
    c2, b2, r2 = req(base, "POST", "/admin/publish/versions/khong-phai-uuid/rollback", adm, {"expectedHeadVersion": head})
    check("mã phiên bản lạ / không phải UUID ⇒ 404 VERSION_NOT_FOUND", c1 == 404 and c2 == 404 and b1.get("code") == b2.get("code") == "VERSION_NOT_FOUND", f"{c1} {c2}")

    # refusals change nothing
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{tid}/rollback", adm, {"expectedHeadVersion": head - 1})
    check("expectedHeadVersion cũ ⇒ 409 HEAD_MOVED", c == 409 and b.get("code") == "HEAD_MOVED", f"{c} {b.get('code', '')}")
    edit_report(rc, "NT077 C nháp", ["name"])
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{tid}/rollback", adm, {"expectedHeadVersion": head})
    check("còn nháp ⇒ 409 DRAFTS_PENDING", c == 409 and b.get("code") == "DRAFTS_PENDING", f"{c} {b.get('code', '')}")
    c, _, h = changes(base, adm)
    req(base, "DELETE", "/admin/publish/changes?changesetHash=" + h, adm)
    check("sau các lần từ chối: số phiên bản không đổi", head_no(base, adm) == head)

    # the rollback itself
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{tid}/rollback", adm, {"expectedHeadVersion": head, "note": "NT077 quay lại"})
    data = b.get("data") or {}
    check("Quay lại ⇒ 201 kind rollback, phiên bản kế tiếp", c == 201 and data.get("kind") == "rollback" and data.get("versionNo") == head + 1, f"{c} {b.get('code', '')} {data}")
    c, cols_a = report_cols(base, staff, ra)
    check("nhân viên chạy A ⇒ 200 với cột [name] (bản đích, cùng mã)", c == 200 and cols_a == ["name"], f"{c} {cols_a}")
    names = report_names(base, staff)
    check("nhân viên không còn thấy B (tạo sau bản đích)", rb not in names, str(names.get(rb)))
    check("C về đúng bản quản trị viên khách di sản sửa trước bản đích (mục direct được giữ)", names.get(rc) == "NT077 C (khách sửa)", str(names.get(rc)))
    versions = (req(base, "GET", "/admin/publish/versions?limit=1", adm)[1].get("data") or [{}])
    check("lịch sử: mục mới nhất kind rollback, rollbackOf = bản đích", versions[0].get("kind") == "rollback" and versions[0].get("rollbackOf") == target_no, str(versions[0]))
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{tid}/rollback", adm, {"expectedHeadVersion": head + 1})
    check("quay lại lần nữa khi bản sống đã giống ⇒ 409 NOTHING_TO_PUBLISH, không phiên bản mới", c == 409 and b.get("code") == "NOTHING_TO_PUBLISH" and head_no(base, adm) == head + 1, f"{c} {b.get('code', '')}")


def run_s710(base, adm, staff, legacy, owner, cleanup):
    """Group 18 - going back over a published delete brings the field and the table back (specs/063 S7-10, T139/T139a).
    Customer side judged with staff `test@example.com` (the restored field and its value) and the legacy customer admin (the
    restored table with exactly the records the delete trashed; the record binned BEFORE stays binned)."""
    print("\n18. Quay lại qua phiên bản xoá trường/bảng (S7-10)")
    if not state(base, adm)[1].get("delivered"):
        raise EnvError("nhóm 18 cần tenant đã giao (nhóm 2 phải chạy trước)")
    _, _, reference = req(base, "GET", "/admin/users", staff)
    c, _, h = changes(base, adm)
    req(base, "DELETE", "/admin/publish/changes?changesetHash=" + h, adm)  # clean slate
    time.sleep(1.2)

    # ── 1. build and release: a UNIQUE field with a value, and a table with 3 records (one binned earlier) ──
    fname = "nt710_" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/fields", adm, {"entityName": ENTITY, "name": fname, "label": "NT710 mã", "fieldType": "text", "isUnique": True})
    fid = ((b.get("data") or {}).get("field") or {}).get("id")
    if c != 201 or not fid:
        raise EnvError(f"tạo trường duy nhất thử thất bại ({c} {b.get('code', '')} {b.get('error', '')})")
    ent = "nt710e" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/entities", adm, {"name": ent, "label": "NT710 bảng", "fields": [{"name": "title", "label": "Tiêu đề", "fieldType": "text"}]})
    if c != 201:
        raise EnvError(f"tạo bảng thử thất bại ({c} {b.get('code', '')} {b.get('error', '')})")
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/fields/{fid}", adm)))
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/entities/{ent}", adm)))
    c, b, _ = req(base, "PUT", f"/admin/fields/{fid}", adm, {"label": "NT710 mã (đã dựng)"})  # a pending change so Publish has something to carry
    if c != 200:
        raise EnvError(f"sửa nhãn trường thử thất bại ({c} {b.get('code', '')})")
    time.sleep(1.2)
    c, b = publish(base, adm, note="nghiem thu S7-10: dung")
    check("Xuất bản trường + bảng thử để khách dùng ⇒ 201", c == 201, f"{c} {b.get('code', '')} {b.get('error', '')}")
    target_no = head_no(base, adm)
    ids = []
    for i in range(3):
        c, b, _ = req(base, "POST", f"/{ent}", adm, {"title": f"NT710 dòng {i}"})
        if c != 201:
            raise EnvError(f"tạo hồ sơ thử trong bảng mới thất bại ({c} {b.get('code', '')})")
        ids.append((b.get("data") or {}).get("id"))
    c, _, _ = req(base, "DELETE", f"/{ent}/{ids[0]}", legacy)
    check("quản trị viên khách di sản bỏ 1 hồ sơ vào thùng rác TRƯỚC khi bảng bị xoá ⇒ 2xx", c in (200, 204), f"{c}")
    c, sf, _ = fields_of(base, ENTITY, staff)
    extra = {n: "x" for n, f in sf.items() if f.get("isRequired") and f.get("fieldType") == "text" and n not in ("name", "stage", fname)}
    value = "NT710-" + uuid.uuid4().hex[:8]
    c, b, _ = req(base, "POST", f"/{ENTITY}", staff, {"name": "NT710 " + uuid.uuid4().hex[:6], "stage": "Prospecting", fname: value, **extra})
    rid = (b.get("data") or {}).get("id")
    if c == 201 and rid:
        cleanup.append(("record", rid))
    check("nhân viên ghi giá trị vào trường duy nhất ⇒ 201", c == 201, f"{c} {b.get('code', '')} {b.get('error', '')}")
    binned = lambda: len(((req(base, "GET", f"/{ent}/deleted?limit=50", legacy)[1]).get("data")) or [])
    live = lambda: len(((req(base, "GET", f"/{ent}?limit=50", legacy)[1]).get("data")) or [])
    check("trước khi xoá: bảng có 2 hồ sơ sống + 1 trong thùng rác", live() == 2 and binned() == 1, f"{live()} {binned()}")

    # ── 2. delete both and publish ──
    req(base, "DELETE", f"/admin/fields/{fid}", adm)
    req(base, "DELETE", f"/admin/entities/{ent}", adm)
    c, b = publish(base, adm, note="nghiem thu S7-10: xoa")
    d = b.get("data") or {}
    check("Xuất bản xoá trường + bảng ⇒ 201, schemaStatus done", c == 201 and d.get("schemaStatus") == "done", f"{c} {b.get('code', '')} {d.get('schemaStatus')}")
    head = head_no(base, adm)
    check("khách: nhân viên không còn thấy trường, quản trị viên khách di sản không còn thấy bảng",
          fname not in fields_of(base, ENTITY, staff)[1] and entity_names(req(base, "GET", "/metadata/entities", legacy)[1]).count(ent) == 0, "")
    versions = (req(base, "GET", "/admin/publish/versions?limit=100", adm)[1].get("data") or [])
    tid = next((v["id"] for v in versions if v.get("versionNo") == target_no), "")

    # ── 3. who may call; the plan lists both ──
    for who, tok in [("nhân viên", staff), ("quản trị viên khách di sản", legacy), ("chủ khách", owner)]:
        if not tok:
            continue
        c1, _, r1 = req(base, "GET", f"/admin/publish/versions/{tid}/rollback-plan", tok)
        c2, _, r2 = req(base, "POST", f"/admin/publish/versions/{tid}/rollback", tok, {"expectedHeadVersion": head})
        check(f"{who} gọi rollback-plan / rollback ⇒ 403 thân giống RequireAdmin", c1 == 403 and c2 == 403 and r1 == reference and r2 == reference, f"{c1} {c2}")
    c, plan, _ = req(base, "GET", f"/admin/publish/versions/{tid}/rollback-plan", adm)
    pd = plan.get("data") or {}
    sr = {x.get("docType"): x for x in pd.get("schemaRestores") or []}
    check("rollback-plan (người dựng) ⇒ 200: khôi phục 1 trường + 1 bảng kèm 2 hồ sơ (không tính hồ sơ thùng rác cũ)",
          c == 200 and set(sr) == {"field", "entity"} and sr["entity"].get("sweptCount") == 2, f"{c} {sr}")

    # ── 4. the rollback ──
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{tid}/rollback", adm, {"expectedHeadVersion": head, "expectedPlanToken": pd.get("planToken"), "note": "NT710 quay lại"})
    data = b.get("data") or {}
    check("Quay lại ⇒ 201 kind rollback", c == 201 and data.get("kind") == "rollback" and data.get("versionNo") == head + 1, f"{c} {b.get('code', '')} {b.get('error', '')} {data}")
    c, sf, _ = fields_of(base, ENTITY, staff)
    check("nhân viên thấy lại trường", c == 200 and fname in sf, f"{c}")
    c, b, _ = req(base, "GET", f"/{ENTITY}/{rid}", staff)
    check("nhân viên thấy lại GIÁ TRỊ cũ của trường", c == 200 and ((b.get("data") or {}).get("data") or {}).get(fname) == value, f"{c} {((b.get('data') or {}).get('data') or {}).get(fname)}")
    extra = {n: "x" for n, f in sf.items() if f.get("isRequired") and f.get("fieldType") == "text" and n not in ("name", "stage", fname)}
    c, b, _ = req(base, "POST", f"/{ENTITY}", staff, {"name": "NT710 dup", "stage": "Prospecting", fname: value, **extra})
    if c == 201:
        cleanup.append(("record", (b.get("data") or {}).get("id")))
    check("trường duy nhất có hiệu lực lại: ghi trùng giá trị ⇒ bị từ chối", c in (409, 422), f"{c} {b.get('code', '')}")
    check("quản trị viên khách di sản thấy lại bảng với đúng 2 hồ sơ sống, hồ sơ thùng rác cũ vẫn trong thùng rác",
          entity_names(req(base, "GET", "/metadata/entities", legacy)[1]).count(ent) == 1 and live() == 2 and binned() == 1, f"{live()} {binned()}")
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{tid}/rollback", adm, {"expectedHeadVersion": head + 1})
    check("quay lại lần nữa khi bản sống đã giống ⇒ 409 NOTHING_TO_PUBLISH", c == 409 and b.get("code") == "NOTHING_TO_PUBLISH", f"{c} {b.get('code', '')}")


def run_s77b(base, adm, staff, staff_user, legacy, cleanup):
    """Group 19 - undo one applied-at-once entry (specs/063 S7-7b, T116)."""
    print("\n19. Hoàn tác mục áp ngay (S7-7b)")
    _, _, reference = req(base, "GET", "/admin/users", staff)
    c, fb, _ = req(base, "GET", f"/metadata/fields/{ENTITY}", adm)
    flist = fb.get("data") if isinstance(fb.get("data"), list) else (fb.get("data") or {}).get("fields") or []
    sample = {"text": "bí mật NT077b", "number": 77, "date": "2026-12-31"}
    hf = next((f for f in flist if isinstance(f, dict) and f.get("fieldType") in sample and not f.get("isRequired") and f.get("name") not in ("id", "name", "stage")), None)
    if not hf:
        print("  (bỏ qua, không tính phép kiểm: bảng thử không có trường text/số/ngày không-bắt-buộc để ẩn)")
        return
    hidden, hidden_val = hf["name"], sample[hf["fieldType"]]
    users = ((req(base, "GET", "/admin/users", adm)[1].get("data") or {}).get("users")) or []
    me = next((u for u in users if u.get("email") == STAFF[0]), {})
    uid, old_role = me.get("id"), me.get("roleId")
    if not uid:
        raise EnvError("không thấy test@example.com trong danh sách người dùng")

    # V: opens the table, hides one field
    c, b, _ = req(base, "POST", "/admin/roles", adm, {"name": "nt077b_" + uuid.uuid4().hex[:6], "label": "NT077b vai",
                  "actions": [{"entityName": ENTITY, "action": a, "accessLevel": "all"} for a in ("view", "create", "edit")]})
    rid = ((b.get("data") or {}).get("role") or {}).get("id")
    if c != 201 or not rid:
        raise EnvError(f"tạo vai thử thất bại ({c} {b.get('code', '')})")

    def restore_staff():
        req(base, "PUT", f"/admin/users/{uid}", adm, {"roleId": old_role or ""})
        req(base, "DELETE", f"/admin/roles/{rid}", adm)
    cleanup.append(("call", restore_staff))
    c, b, _ = req(base, "PUT", "/admin/field-acl", adm, {"grants": [{"roleId": rid, "entityName": ENTITY, "fieldName": hidden, "canView": False, "canEdit": False}]})
    check("V ẩn trường " + hidden + " ⇒ 200", c == 200, f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "PUT", f"/admin/users/{uid}", adm, {"roleId": rid})
    check("gán V cho test@example.com ⇒ 200", c == 200, f"{c} {b.get('code', '')}")
    extra = {f["name"]: "x" for f in flist if isinstance(f, dict) and f.get("isRequired") and f.get("fieldType") == "text" and f["name"] not in ("name", "stage")}  # required fields earlier groups published
    c, b, _ = req(base, "POST", f"/{ENTITY}", adm, {"name": "NT077b " + uuid.uuid4().hex[:6], "stage": "Prospecting", hidden: hidden_val, **extra})
    rec = (b.get("data") or {}).get("id")
    if c != 201 or not rec:
        raise EnvError(f"tạo hồ sơ thử thất bại ({c} {b.get('code', '')} {b.get('error', '')})")
    cleanup.append(("record", rec))
    ca, ba, _ = req(base, "GET", f"/{ENTITY}/{rec}", adm)
    da = ba.get("data") or {}
    da = (da.get("data") if isinstance(da, dict) else None) or {}  # the dynamic fields sit under data
    check("người dựng thấy trường " + hidden + " của hồ sơ (để phép kiểm 'ẩn' có nghĩa)", ca == 200 and hidden in da, f"{ca} {sorted(da)[:8]}")

    def staff_view():
        c, b, _ = req(base, "GET", f"/{ENTITY}/{rec}", staff)
        d = b.get("data") or {}
        return c, ((d.get("data") if isinstance(d, dict) else None) or {}) if c == 200 else {}
    c, d = staff_view()
    check("trước khi xoá: nhân viên mở được hồ sơ và KHÔNG thấy trường ẩn", c == 200 and hidden not in d and bool(d.get("name")), f"{c} {sorted(d)[:8]}")

    c, _, _ = req(base, "DELETE", f"/admin/roles/{rid}", adm)
    check("người dựng xoá V ⇒ 204", c == 204, f"{c}")
    c, _ = staff_view()
    check("nhân viên mất quyền mở hồ sơ (403)", c == 403, f"{c}")

    versions = (req(base, "GET", "/admin/publish/versions?limit=5", adm)[1].get("data") or [])
    entry = next((v for v in versions if v.get("kind") == "direct"), {})
    eid = entry.get("id", "")
    c, b, _ = req(base, "GET", f"/admin/publish/versions/{eid}", adm)
    docs = (b.get("data") or {}).get("docs") or []
    re = next((x for x in (docs[0].get("reassign") or []) if x.get("userId") == uid), None) if docs else None
    check("chi tiết mục xoá vai: reassign liệt kê test@example.com (chưa giữ vai nào) + createdByLabel",
          c == 200 and re is not None and re.get("currentRoleId") == "" and bool((b.get("data") or {}).get("createdByLabel")), f"{c} {docs[0].get('reassign') if docs else ''}")

    for who, tok in [("nhân viên", staff), ("quản trị viên khách di sản", legacy)]:
        c, _, raw = req(base, "POST", f"/admin/publish/versions/{eid}/revert", tok, {})
        check(f"{who} gọi revert ⇒ 403 thân giống RequireAdmin", c == 403 and raw == reference, f"{c}")
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{uuid.uuid4()}/revert", adm, {})
    check("mã lạ ⇒ 404 VERSION_NOT_FOUND", c == 404 and b.get("code") == "VERSION_NOT_FOUND", f"{c} {b.get('code', '')}")
    numbered = next((v for v in (req(base, "GET", "/admin/publish/versions?limit=100", adm)[1].get("data") or []) if v.get("versionNo")), {})
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{numbered.get('id', '')}/revert", adm, {})
    check("phiên bản đánh số ⇒ 422 REVERT_USE_ROLLBACK", c == 422 and b.get("code") == "REVERT_USE_ROLLBACK", f"{c} {b.get('code', '')}")
    c, _ = staff_view()
    check("các lần từ chối không đổi gì: nhân viên vẫn 403", c == 403, f"{c}")

    c, b, _ = req(base, "POST", f"/admin/publish/versions/{eid}/revert", adm, {})
    data = b.get("data") or {}
    check("người dựng Hoàn tác ⇒ 200, trả vai cho 1 người, có mục direct mới", c == 200 and data.get("reassigned") == 1 and (data.get("entry") or {}).get("kind") == "direct", f"{c} {b.get('code', '')} {data}")
    c, b, _ = req(base, "GET", f"/admin/roles/{rid}", adm)
    check("V trở lại CÙNG MÃ", c == 200, f"{c}")
    c, d = staff_view()
    check("nhân viên mở lại được hồ sơ VÀ vẫn không thấy trường V ẩn", c == 200 and hidden not in d and bool(d.get("name")), f"{c} keys={sorted(d)[:6]}")
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{eid}/revert", adm, {})
    check("hoàn tác lần hai ⇒ 409 REVERT_STALE", c == 409 and b.get("code") == "REVERT_STALE", f"{c} {b.get('code', '')}")


def run_s711(base, adm, staff, legacy, cleanup):
    """Group 20 - a field / table added AFTER delivery is a pending change that Publish releases (specs/063 FR-060).
    Regression: a plain new field had no draft, so pendingCount stayed 0, /admin/publish/changes stayed [] and
    Publish answered NOTHING_TO_PUBLISH - an invisible field nobody could release. Judged with the non-admin staff."""
    print("\n20. Trường/bảng thêm sau khi giao là thay đổi chờ Xuất bản (FR-060)")
    st = state(base, adm)[1]
    if not st.get("delivered"):
        raise EnvError("nhóm 20 cần tenant đã giao (nhóm 2 phải chạy trước)")
    if st.get("pendingCount"):
        publish(base, adm, force=True, note="nghiem thu 20: sach chang")
    time.sleep(1.2)  # created_at has second precision: be strictly after the last publish's second

    fname = "nt19f_" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/fields", adm, {"entityName": ENTITY, "name": fname, "label": "NT19 trường thường", "fieldType": "text"})
    fid = ((b.get("data") or {}).get("field") or {}).get("id")
    if c != 201 or not fid:
        raise EnvError(f"tạo trường thử thất bại ({c} {b.get('code', '')} {b.get('error', '')})")
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/fields/{fid}", adm)))
    ent = "nt19e" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/entities", adm, {"name": ent, "label": "NT19 bảng", "fields": [{"name": "title", "label": "Tiêu đề", "fieldType": "text"}]})
    if c != 201:
        raise EnvError(f"tạo bảng thử thất bại ({c} {b.get('code', '')} {b.get('error', '')})")
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/entities/{ent}", adm)))

    c, sf, _ = fields_of(base, ENTITY, staff)
    check("nhân viên chưa thấy trường mới", c == 200 and fname not in sf, f"{c}")
    c, st = state(base, adm)
    check("pendingCount = 2 (trường + bảng) dù không nháp nào được ghi", st.get("pendingCount") == 2, str(st.get("pendingCount")))
    c, items, h = changes(base, adm)
    by = {i.get("docType"): i for i in items}
    check("danh sách có đúng 2 dòng: field + entity, op create, không xung đột, discardable:false",
          c == 200 and sorted(by) == ["entity", "field"] and all(i.get("op") == "create" and i.get("conflict") is False and i.get("discardable") is False for i in items),
          str([(i.get("docType"), i.get("op"), i.get("discardable")) for i in items]))
    check("dòng trường mang đúng mã + bảng chứa; dòng bảng mang tên bảng",
          (by.get("field") or {}).get("docKey") == fid and (by.get("field") or {}).get("entityName") == ENTITY and (by.get("entity") or {}).get("entityName") == ent,
          str({k: (v.get("docKey"), v.get("entityName")) for k, v in by.items()}))
    c, b, _ = req(base, "DELETE", f"/admin/publish/changes/{(by.get('field') or {}).get('id', '')}", adm)
    check("bỏ từng dòng trường mới ⇒ 404 (không có nháp để bỏ)", c == 404 and b.get("code") == "DRAFT_NOT_FOUND", f"{c} {b.get('code', '')}")
    c, b, _ = req(base, "DELETE", "/admin/publish/changes?changesetHash=" + h, adm)
    check("Bỏ tất cả ⇒ 204 nhưng KHÔNG xoá trường/bảng mới", c == 204 and state(base, adm)[1].get("pendingCount") == 2, f"{c} pending={state(base, adm)[1].get('pendingCount')}")

    c, b = publish(base, adm, note="nghiem thu 20")
    check("Xuất bản ⇒ 201, docCount 2", c == 201 and (b.get("data") or {}).get("docCount") == 2, f"{c} {b.get('code', '')} {b.get('error', '')} {(b.get('data') or {}).get('docCount')}")
    c, sf, _ = fields_of(base, ENTITY, staff)
    check("nhân viên thấy trường mới sau Xuất bản", c == 200 and fname in sf, f"{c}")
    c, b, _ = req(base, "GET", "/metadata/entities", legacy)
    check("quản trị viên khách di sản thấy bảng mới sau Xuất bản", c == 200 and ent in entity_names(b), f"{c}")
    c, st = state(base, adm)
    check("pendingCount về 0", st.get("pendingCount") == 0, str(st.get("pendingCount")))
    c, b = publish(base, adm)
    check("Xuất bản lần nữa ⇒ 409 NOTHING_TO_PUBLISH", c == 409 and b.get("code") == "NOTHING_TO_PUBLISH", f"{c} {b.get('code', '')}")


def rel_names(base, tok, entity, headers=None):
    c, b, _ = req(base, "GET", "/metadata/relationships?entity=" + urllib.parse.quote(entity), tok, headers_extra=headers)
    return c, [r.get("name") for r in (b.get("data") or [])]


def run_s711b(base, adm, staff, cleanup):
    """Group 21 - an N-N relation registered AFTER delivery between two tables the customer already has is a pending
    change (specs/063 FR-060): it writes no field and no draft, yet GET /metadata/relationships hides it by the cutoff.
    Regression: pendingCount 0, Publish NOTHING_TO_PUBLISH, the relation hidden for ever. Then the rollback past a
    publish that only released hidden rows must tell the truth (FR-052): docCount = docs, not NOTHING_TO_PUBLISH."""
    print("\n21. Quan hệ N-N thêm sau khi giao là thay đổi chờ Xuất bản; quay lại qua nó ẩn lại (FR-060, FR-052)")
    if state(base, adm)[1].get("pendingCount"):
        publish(base, adm, force=True, note="nghiem thu 21: sach chang")
    time.sleep(1.2)  # created_at has second precision: be strictly after the last publish's second

    c, b, _ = req(base, "GET", "/metadata/entities", staff)
    staff_tables = [n for n in entity_names(b) if n != ENTITY]
    c2, rb, _ = req(base, "GET", "/metadata/relationships", adm)
    paired = {r.get("childEntityName") if r.get("parentEntityName") == ENTITY else r.get("parentEntityName")
              for r in (rb.get("data") or []) if r.get("relationshipType") == "N-N" and ENTITY in (r.get("parentEntityName"), r.get("childEntityName"))}
    other = next((n for n in staff_tables if n not in paired), "")
    if c != 200 or c2 != 200 or not other:
        raise EnvError(f"nhóm 21 cần một bảng thứ hai nhân viên thấy, chưa nối N-N với {ENTITY} ({c} {c2} thấy={staff_tables} đã nối={sorted(paired)})")
    name = "nt21_" + uuid.uuid4().hex[:6]
    c, b, _ = req(base, "POST", "/admin/relationships", adm, {"name": name, "entityA": ENTITY, "entityB": other})
    rid = (b.get("data") or {}).get("id")
    if c != 201 or not rid:
        raise EnvError(f"đăng ký quan hệ thử thất bại ({c} {b.get('code', '')} {b.get('error', '')})")
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/relationships/{rid}", adm)))

    c, rels = rel_names(base, staff, ENTITY)
    check("nhân viên chưa thấy quan hệ N-N mới", c == 200 and name not in rels, f"{c}")
    c, st = state(base, adm)
    check("pendingCount = 1 (quan hệ N-N, không nháp nào được ghi)", st.get("pendingCount") == 1, str(st.get("pendingCount")))
    c, items, _ = changes(base, adm)
    check("danh sách có đúng 1 dòng relationship, op create, discardable:false",
          c == 200 and [(i.get("docType"), i.get("docKey"), i.get("op"), i.get("discardable")) for i in items] == [("relationship", rid, "create", False)],
          str([(i.get("docType"), i.get("op"), i.get("discardable")) for i in items]))
    before = head_no(base, adm)
    c, b = publish(base, adm, note="nghiem thu 21")
    data = b.get("data") or {}
    check("Xuất bản ⇒ 201, docCount 1", c == 201 and data.get("docCount") == 1, f"{c} {b.get('code', '')} {data.get('docCount')}")
    versions = (req(base, "GET", "/admin/publish/versions?limit=100", adm)[1].get("data") or [])
    vid = next((v["id"] for v in versions if v.get("versionNo") == data.get("versionNo")), "")
    tid = next((v["id"] for v in versions if v.get("versionNo") == before), "")
    c, vb, _ = req(base, "GET", f"/admin/publish/versions/{vid}", adm)
    vd = vb.get("data") or {}
    check("phiên bản nói thật: docCount 1 = 1 tài liệu relationship op create",
          c == 200 and vd.get("docCount") == 1 and [(d.get("docType"), d.get("docKey"), d.get("op")) for d in vd.get("docs") or []] == [("relationship", rid, "create")],
          f"{c} docCount={vd.get('docCount')} docs={[(d.get('docType'), d.get('op')) for d in vd.get('docs') or []]}")
    c, rels = rel_names(base, staff, ENTITY)
    check("nhân viên thấy quan hệ sau Xuất bản", c == 200 and name in rels, f"{c}")

    c, pb, _ = req(base, "GET", f"/admin/publish/versions/{tid}/rollback-plan", adm)
    plan = pb.get("data") or {}
    check("kế hoạch quay lại bản trước: không đổi tài liệu nào, ẩn lại đúng quan hệ",
          c == 200 and plan.get("changes") == [] and [(r.get("docType"), r.get("docKey")) for r in plan.get("rehides") or []] == [("relationship", rid)],
          f"{c} changes={plan.get('changes')} rehides={plan.get('rehides')}")
    head = head_no(base, adm)
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{tid}/rollback", adm, {"expectedHeadVersion": head, "expectedPlanToken": plan.get("planToken", ""), "note": "NT21 quay lai"})
    check("Quay lại ⇒ 201 (không phải NOTHING_TO_PUBLISH)", c == 201 and (b.get("data") or {}).get("kind") == "rollback", f"{c} {b.get('code', '')}")
    c, rels = rel_names(base, staff, ENTITY)
    check("nhân viên KHÔNG còn thấy quan hệ sau Quay lại", c == 200 and name not in rels, f"{c}")
    c, rels = rel_names(base, adm, ENTITY, DRAFT_HDR)
    check("quan hệ vẫn sống với người dựng (xem trước có nháp; không bị xoá)", c == 200 and name in rels, f"{c}")
    check("quan hệ lại là thay đổi chờ (pendingCount 1)", state(base, adm)[1].get("pendingCount") == 1, str(state(base, adm)[1].get("pendingCount")))
    c, b, _ = req(base, "POST", f"/admin/publish/versions/{vid}/rollback", adm, {"expectedHeadVersion": head + 1})
    check("quay lại khi còn dòng chờ ⇒ DRAFTS_PENDING", c >= 400 and b.get("code") == "DRAFTS_PENDING", f"{c} {b.get('code', '')}")
    c, b = publish(base, adm, note="nghiem thu 21: phat hanh lai")
    c2, rels = rel_names(base, staff, ENTITY)
    check("Xuất bản lại ⇒ 201, nhân viên thấy lại quan hệ", c == 201 and c2 == 200 and name in rels, f"{c} {b.get('code', '')} {c2}")


def cleanup_all(base, adm, cleanup):
    """Remove what the run created. Rules go through the draft layer on a delivered tenant, so deletions are
    published (force) — the clone ends with 0 pending drafts. Records are deleted straight."""
    wf_ids = [rid for kind, rid in cleanup if kind == "workflow"]
    for kind, rid in reversed(cleanup):
        if kind == "record":
            c, _, _ = req(base, "DELETE", f"/{ENTITY}/{rid}", adm)
            if c not in (200, 204, 404):
                print(f"  (dọn cơ hội {rid} → {c})")
        elif kind == "call":
            rid()
    for rid in wf_ids:
        req(base, "DELETE", f"/admin/workflows/{rid}", adm)
    for kind, rid in cleanup:
        if kind == "report":
            req(base, "DELETE", f"/admin/reports/{rid}", adm)
        elif kind == "dashboard":
            req(base, "DELETE", f"/admin/dashboards/{rid}", adm)
        elif kind == "app":
            req(base, "DELETE", f"/admin/apps/{rid}", adm)
        elif kind == "layout":
            req(base, "DELETE", f"/admin/layouts/{rid}", adm)  # 404 when the run already removed it
        elif kind == "branding":
            req(base, "PUT", "/admin/tenant/branding", adm, rid)  # back to the colour the run found (published below)
    c, st = state(base, adm)
    if c == 200 and st.get("delivered") and st.get("pendingCount"):
        pc, pb = publish(base, adm, force=True, note="nghiem thu: don")
        if pc != 201:
            print(f"  (dọn: Xuất bản bản dọn → {pc} {pb.get('code', '')}; còn {st.get('pendingCount')} nháp trên bản sao)")


def main():
    ap = argparse.ArgumentParser(description="Nghiệm thu không-admin cho specs/063 nháp → xuất bản.")
    ap.add_argument("--base", required=True, help="vd http://localhost:8412/api/v1 (server riêng trên DB bản sao)")
    ap.add_argument("--allow-remote-write", action="store_true")
    ap.add_argument("--db", default="", help="tên DB bản sao (vd nexus_wt_publish_direct_entries) để nhóm 15 đọc sys_meta_versions")
    a = ap.parse_args()
    base = a.base.rstrip("/")
    if urllib.parse.urlparse(base).port == SHARED_DEV_PORT:
        print(f"TỪ CHỐI: cổng {SHARED_DEV_PORT} là server dev dùng chung — Xuất bản lần đầu là một chiều. Dùng server riêng + DB bản sao.")
        return 2
    if not (is_loopback(base) or a.allow_remote_write):
        print("TỪ CHỐI: --base không phải loopback (script GHI). Thêm --allow-remote-write nếu chắc chắn.")
        return 2

    cleanup, adm = [], None
    try:
        adm, _ = login(base, *ADMIN)
        run(a, cleanup)
    except EnvError as e:
        print(f"LỖI MÔI TRƯỜNG: {e}")
        return 2
    finally:
        if adm:
            try:
                cleanup_all(base, adm, cleanup)
            except EnvError as e:
                print(f"LỖI DỌN: {e}")
                fails.append("dọn dẹp")

    if ran == 0:
        print("LỆCH — 0 phép kiểm chạy (không xanh giả)")
        return 1
    print(f"\n{'ĐẠT' if not fails else 'LỆCH'} — {ran} phép kiểm, {len(fails)} lệch")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
