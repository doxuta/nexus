#!/usr/bin/env python3
"""Nghiệm thu người-thứ-ba cho specs/048-before-save-hooks (quy tắc trước khi lưu / trước khi xoá).

Mỗi lát một `--slice`. Admin (`admin@example.com`) CHỈ dùng để dựng quy tắc qua API — kết quả luôn chấm
bằng tài khoản không-admin (`test@example.com`, vai crm_sales_rep). Vai đó không có quyền xoá cơ hội, nên
lát S3 cấp tạm "xoá: của mình" rồi trả lại trong `finally` (cùng cách s1b-files cấp quyền xoá hợp đồng)
thay vì dựa vào tài khoản fixture `duyet1@example.com` — DB bản sao không chắc đã chạy scripts/fixture-bpm.py.
`is_admin` bỏ qua toàn bộ phân quyền, nên admin tự thử là tự chấm điểm cho mình.

Chạy trên DB BẢN SAO, luôn chỉ rõ server (không có mặc định — lỡ tay không ghi vào DB dev dùng chung):
    python3 scripts/acceptance-before-save-hooks.py --base http://localhost:8160/api/v1 --slice s1a

Mọi quy tắc và bản ghi script tạo ra bị tắt + xoá trong `finally`, kể cả khi một kỳ vọng lệch.

--slice s1a (nền + cấu hình):
  - admin lưu quy tắc before_save/veto (saveOn=update) ở trạng thái TẮT ⇒ 201, GET trả lại đủ
    trigger / saveOn / veto field+message;
  - before_delete bỏ trống isActive (mặc định bật) ⇒ 201, đang bật (tới S3 bị cổng bật R13 từ chối — cổng đã
    xoá vì mọi quy tắc trước đều có đường chạy; điều kiện của quy tắc thử không khớp bản ghi nào);
  - before_save kèm email_alert ⇒ 422 WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER;
  - test@example.com KHÔNG tạo được quy tắc (403) và vẫn tạo/sửa cơ hội của mình như thường — kể cả
    sửa đúng thứ quy tắc (đang tắt) mô tả: Giai đoạn sang Thắng, Ngày chốt trống ⇒ 200, không đổi hành vi.

--slice s1b (CHẶN khi sửa):
  - admin BẬT quy tắc "Giai đoạn vừa đổi thành Thắng VÀ Ngày chốt trống ⇒ chặn, gắn Ngày chốt" (saveOn=update)
    ⇒ 201;
  - test@example.com: PUT kiểu form (kèm version, Ngày chốt null) và PUT kiểu Kanban (chỉ Giai đoạn) ⇒ 422
    RECORD_HOOK_VETO đúng câu, details.field=close_date, không lộ ruleId; tải lại thấy Giai đoạn + version cũ;
    điền Ngày chốt ⇒ 200; cơ hội đã Thắng từ trước chỉ sửa Tên ⇒ 200;
  - admin ẩn Ngày chốt với vai của test@example.com ⇒ lỗi chặn không nêu tên trường ở đâu cả (trả lại quyền
    trường ngay sau đó và trong finally);
  - admin tắt quy tắc ⇒ lần lưu vi phạm qua ngay.

--slice s1b-files (tệp đính kèm trên hồ sơ đang khoá — spec Nợ 16):
  - admin cho vai của test@example.com quyền xoá hợp đồng của mình (gỡ tệp cần quyền xoá; trả lại trong finally);
  - test@example.com tạo hợp đồng, đính kèm bản ký ⇒ 201; admin bật quy tắc before_save saveOn=update khoá đúng hợp
    đồng đó;
  - test@example.com gỡ bản ký ⇒ 422 RECORD_HOOK_VETO đúng câu; tải bản ký về VẪN được đúng nội dung; hợp đồng vẫn liệt
    kê nó (trước bản sửa: tải về 404, tệp mất hẳn ở lần ghi mảng kế tiếp);
  - test@example.com đính kèm thêm phụ lục ⇒ 422, hợp đồng chỉ còn bản ký;
  - admin tắt quy tắc ⇒ gỡ bản ký ⇒ 204, tải về 404, hợp đồng không còn tệp nào (phụ lục bị từ chối không quay lại).

--slice s2 (CHẶN khi TẠO, trên mọi đường tạo):
  - admin BẬT quy tắc "Giai đoạn là Thắng VÀ Ngày chốt trống ⇒ chặn, gắn Ngày chốt", áp cả tạo mới lẫn sửa ⇒ 201;
  - test@example.com: (1) tạo cơ hội ở Thắng, Ngày chốt trống ⇒ 422 RECORD_HOOK_VETO đúng câu, gắn Ngày chốt, không
    lộ ruleId; cơ hội hợp lệ tạo ngay trước và ngay sau nhận mã OPP- liền số;
    (2) tạo nhiều dòng "tất cả hoặc không" 3 dòng, dòng 2 vi phạm ⇒ 422, dòng 2 báo đúng câu, hai dòng kia "bỏ qua",
    không dòng nào được tạo; cùng lô ở chế độ "tạo những dòng hợp lệ" ⇒ tạo 2, dòng 2 báo đúng câu;
    (3) nhập CSV 3 dòng, dòng 2 vi phạm ⇒ nhập 2, lỗi 1: dòng 2 — đúng câu, gắn Ngày chốt;
    (4) mọi cơ hội đã tạo được ở (1)-(3) + một cơ hội cuối mang mã OPP- liền một dải, không lỗ (lần tạo bị
    chặn không đốt số);
  - admin đổi quy tắc sang chỉ áp khi SỬA ⇒ tạo vi phạm qua được (201); sửa chính cơ hội đó ⇒ 422 đúng câu.

--slice s3 (CHẶN khi XOÁ + lịch sử "Đã chặn"):
  - admin cấp tạm vai của test@example.com quyền xoá cơ hội của mình (trả lại trong finally);
  - test@example.com tạo 3 cơ hội: một Thắng, hai đang Tìm kiếm; admin BẬT quy tắc trước khi xoá "Giai đoạn là
    Thắng ⇒ chặn: Không xoá cơ hội đã Thắng" (isActive bỏ trống = bật) ⇒ 201, đang bật;
  - test@example.com: (1) xoá cơ hội Thắng ⇒ 422 RECORD_HOOK_VETO đúng câu, không nêu trường, không lộ ruleId; tải lại
    vẫn còn; (2) xoá cơ hội Tìm kiếm ⇒ 204; (3) xoá hàng loạt [Thắng, Tìm kiếm] ⇒ xoá 1, dòng Thắng mang
    RECORD_HOOK_VETO + đúng câu (không phải mã), cơ hội Thắng vẫn còn;
  - admin đọc lịch sử chạy của quy tắc (GET /admin/workflows/:id/runs): đúng 2 dòng `blocked`, trigger
    before_delete, trỏ cơ hội Thắng, actionsResult không chứa tên / giai đoạn / thẻ nghiệm thu;
  - admin tắt quy tắc ⇒ test@example.com xoá được cơ hội Thắng (204).

--slice s4 (TỰ ĐIỀN giá trị khi lưu):
  - admin: quy tắc trước khi lưu nhắm trường quan hệ ⇒ 422 WORKFLOW_BEFORE_TARGET_UNSUPPORTED gắn đúng trường; hằng
    "Website" (nhãn, không phải giá trị của lựa chọn) ⇒ 422 WORKFLOW_BEFORE_VALUE_INVALID lý do RECORD_INVALID_PICKLIST;
  - admin BẬT quy tắc 1 "Giai đoạn vừa đổi thành Thắng ⇒ Xác suất = 100" trên cơ hội và quy tắc 2 "Nguồn trống ⇒
    Website (chỉ khi trống, chỉ khi tạo)" trên lead; GET trả lại onlyIfEmpty;
  - test@example.com: tạo cơ hội Báo giá, Giá trị 1.000.000, Xác suất 40, có Ngày chốt; đổi Giai đoạn sang Thắng (chỉ gửi
    Giai đoạn, kiểu Kanban) ⇒ 200; tải lại: Xác suất 100, Giá trị kỳ vọng = Giá trị; tab Lịch sử có ĐÚNG MỘT dòng sửa
    của test@example.com gồm cả Giai đoạn lẫn Xác suất;
  - test@example.com: tạo lead không chọn Nguồn ⇒ Nguồn = Website (Web); tạo lead chọn Sự kiện ⇒ giữ Sự kiện;
  - admin đọc lịch sử chạy: mỗi quy tắc đúng một dòng Thành công trỏ đúng bản ghi, không chứa giá trị nào.

--slice s4-review (review 2026-09-30 của S4 — nguồn giá trị + picklist phụ thuộc), trên lead, trường riêng mỗi lần chạy:
  - admin: mẫu chữ `{{company.name}}` ⇒ 422 WORKFLOW_BEFORE_VALUE_INVALID lý do template_source_unknown; `{{lead_code}}`
    trên quy tắc có áp khi tạo và `{{assigned_to}}` ⇒ template_source_unsupported; mẫu trống ⇒ template_empty;
  - admin thêm 4 trường cho cơ hội (ghi chú, mã, CMND cũ, một trường luôn trống) + 2 cho lead (tỉnh, quận phụ thuộc
    tỉnh) và bật 3 quy tắc: ghi chú = mẫu `{{trường trống}}` (khi sửa), mã = chép CMND cũ (khi sửa) — trên cơ hội, vì lead vừa tạo bị quy tắc chia việc
    chuyển chủ nên test@example.com không sửa được; quận = "Ba Đình" (chỉ khi trống, khi tạo) trên lead;
  - test@example.com: tạo cơ hội có ghi chú viết tay + CMND cũ; sửa tên ⇒ ghi chú GIỮ NGUYÊN (mẫu ra rỗng không xoá), mã =
    CMND cũ (nguồn còn sống thì chép);
  - admin XOÁ trường CMND cũ; test@example.com gõ mã "Gõ tay" ⇒ mã GIỮ "Gõ tay" (không chép giá trị của trường đã xoá);
  - test@example.com tạo lead tỉnh Đà Nẵng ⇒ không có quận (Ba Đình không thuộc Đà Nẵng); tỉnh Hà Nội ⇒ quận Ba Đình;
  - admin đọc lịch sử chạy: quy tắc mẫu 0 dòng; quy tắc chép Thành công rồi Lỗi WORKFLOW_BEFORE_VALUE_INVALID; quy tắc
    quận một Lỗi RECORD_DEPENDENT_PICKLIST_INVALID + một Thành công; không dòng nào chứa giá trị.

--slice s5 (CHUẨN HOÁ chữ + tự điền trên mọi đường tạo/sửa), trên lead:
  - admin: phép lặp (trim, trim) ⇒ 422 NORMALIZE_OPS_INVALID gắn Email; chuẩn hoá trường lựa chọn (Nguồn) ⇒ 422
    NORMALIZE_FIELD_TYPE_INVALID gắn Nguồn; chuẩn hoá trên after_create ⇒ 422 WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER;
  - admin tạm tắt quy tắc chia lead after_create (lead mới bị giao người khác thì test@example.com không sửa hàng loạt
    được lead của mình; trả lại trong finally) rồi bật 3 quy tắc before_save: Email trim+lower, Họ tên
    trim+collapse_spaces, Nguồn trống ⇒ Web; GET giữ ops đúng thứ tự;
  - test@example.com: (1) form tạo lead "  Nguyễn   Văn  An … " / "  An.Nguyen.…@EXAMPLE.COM " ⇒ chi tiết "Nguyễn Văn An …" /
    "an.nguyen.…@example.com"; (2) tạo nhiều dòng (atomic) 3 lead và nhập CSV 3 lead email lộn xộn ⇒ chi tiết / danh sách đã
    chuẩn hoá, Nguồn Web; (3) sửa hàng loạt Email của 2 lead ⇒ cả hai chuẩn hoá;
  - admin xoá lựa chọn Web khỏi Nguồn (trả lại trong finally); test@example.com tạo lead không chọn Nguồn ⇒ vẫn 201,
    Nguồn trống;
  - admin đọc lịch sử chạy: quy tắc Email có đúng một dòng tổng hợp cho lô và một cho tệp nhập
    ({applied:3, blocked:0, failed:0}, Thành công) + dòng của form và sửa hàng loạt trỏ đúng bản ghi; quy tắc Nguồn một
    dòng Lỗi RECORD_INVALID_PICKLIST trỏ lead cuối; không dòng nào chứa giá trị.

--slice s6 (CHẠY THỬ quy tắc trước khi bật + miễn chặn khi ghi kết quả duyệt), cần fixture BPM (`duyet1@example.com`,
chạy `scripts/fixture-bpm.py --base … ` trước; thiếu ⇒ thoát 2):
  - admin lưu (TẮT) quy tắc chặn S1b trên cơ hội + một quy tắc tự điền "vừa đổi thành Thắng ⇒ Xác suất 100"; test@example.com
    tạo cơ hội của mình (Báo giá, Xác suất 40). Admin chạy thử với thay đổi giả lập Giai đoạn = Thắng ⇒ "Sẽ chặn" đúng
    câu (wouldBlock, gắn Ngày chốt); thêm Ngày chốt giả lập ⇒ không chặn; không thay đổi ⇒ không chặn (vừa đổi thành cần
    thay đổi); tạo giả lập không bản ghi ⇒ quy tắc chỉ-khi-sửa không chạy; quy tắc tự điền ⇒ Xác suất 40 → 100; thay đổi
    trường lạ ⇒ 422 WORKFLOW_TEST_CHANGES_INVALID gắn trường; test@example.com gọi chạy thử ⇒ 403; tải lại cơ hội bằng
    test@example.com: Giai đoạn / Xác suất / version không đổi; hai quy tắc không có dòng lịch sử chạy nào;
  - admin bật quy tắc chặn báo giá "Trạng thái vừa đổi thành Chấp nhận ⇒ Báo giá chỉ được Chấp nhận qua quy trình duyệt"
    (gắn Trạng thái) và quy trình duyệt tay một bước (duyet1@example.com) ghi Trạng thái = Chấp nhận khi duyệt;
  - test@example.com: (1) sửa báo giá của mình sang Chấp nhận (kiểu form, kèm version) ⇒ 422 RECORD_HOOK_VETO đúng câu, gắn
    Trạng thái, không ruleId; tải lại vẫn Nháp; (2) Gửi duyệt ⇒ 201; duyet1@example.com thấy yêu cầu trong /approvals/pending
    và bấm Duyệt ⇒ 200; test@example.com tải lại báo giá ⇒ Chấp nhận;
  - admin đọc lịch sử chạy của quy tắc chặn báo giá: một dòng Đã chặn (lần sửa tay) + một dòng Bỏ qua lý do
    approval_outcome, cả hai trỏ đúng báo giá, không chứa giá trị nào.

--slice s7a (nhập CSV ghi lịch sử "Tạo" cho từng dòng), trên lead, cần `--db <tên DB bản sao>` (chạy lại job bằng docker exec):
  - test@example.com nhập CSV 3 lead ⇒ chờ tối đa vài giây ⇒ lịch sử của MỖI lead có đúng một dòng "Tạo" của test@example.com;
  - script đánh dấu job record_import_events của lần nhập về `queued` (sys_jobs) rồi chờ job chạy lại ⇒ mỗi lead vẫn
    đúng một dòng "Tạo" (đã-xử-lý nhận ra bằng chính dòng lịch sử).

Mã thoát: 0 = đạt · 1 = một kỳ vọng lệch · 2 = lỗi môi trường.

ponytail: stdlib thuần (urllib/argparse), cùng khuôn scripts/acceptance-acl.py.
"""

import argparse
import importlib.util
import json
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

TENANT = "northwind"
ADMIN = ("admin@example.com", "Demo-Admin-2026!")  # same demo constants scripts/acceptance-acl.py uses
STAFF = ("test@example.com", "Demo-Rep-2026!")
ENTITY = "crm_opportunities"
VETO_MSG = "Chốt Thắng phải có Ngày chốt"
CONTRACTS = "crm_contracts"
LOCK_MSG = "Hợp đồng đã khoá, không sửa được"


class EnvError(Exception):
    """The environment is not usable (server down, login refused) — exit 2."""


def req(base, method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    r = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=15) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, (json.loads(raw) if raw else {})
        except ValueError:
            return e.code, {}
    except Exception as e:  # connection refused, timeout…
        raise EnvError(f"không gọi được {base + path} — {e}") from e


def raw(base, method, path, token, data=None, ctype=None):
    """Like req() but for bytes in/out (multipart upload, file download)."""
    headers = {"Authorization": "Bearer " + token}
    if ctype:
        headers["Content-Type"] = ctype
    r = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=15) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:
        raise EnvError(f"không gọi được {base + path} — {e}") from e


def upload(base, token, rid, field, filename, content):
    boundary = "nexus" + uuid.uuid4().hex
    body = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{filename}"\r\n'
            f"Content-Type: text/plain\r\n\r\n").encode() + content + f"\r\n--{boundary}--\r\n".encode()
    code, data = raw(base, "POST", f"/{CONTRACTS}/{rid}/files?field={field}", token, body,
                     f"multipart/form-data; boundary={boundary}")
    try:
        return code, (json.loads(data) if data else {})
    except ValueError:
        return code, {}


def login(base, email, password):
    code, body = req(base, "POST", "/auth/login", body={"tenant": TENANT, "email": email, "password": password})
    data = body.get("data") or {}
    if code != 200 or not data.get("accessToken"):
        raise EnvError(f"đăng nhập {email} thất bại ({code} {body.get('code', '')})")
    return data["accessToken"], data.get("user") or {}


fails = []


def check(label, cond, detail=""):
    print(("  ĐẠT   " if cond else "  LỆCH  ") + label + (f"   [{detail}]" if detail else ""))
    if not cond:
        fails.append(label)


def veto_rule(name, save_on="update", **over):
    cond = {"rules": [
        {"field": "stage", "op": "changedTo", "value": "Closed Won"},
        {"field": "close_date", "op": "isnull"},
    ]}
    if save_on:
        cond["saveOn"] = save_on
    rule = {
        "name": name, "entityName": ENTITY, "trigger": "before_save", "isActive": False,
        "condition": cond,
        "actions": [{"type": "veto", "field": "close_date", "message": VETO_MSG}],
    }
    rule.update(over)
    return rule


def slice_s1a(base, adm, staff, cleanup, _staff_user):
    tag = uuid.uuid4().hex[:8]

    print("\n[admin] quy tắc chặn lưu ở trạng thái tắt")
    code, body = req(base, "POST", "/admin/workflows", adm, veto_rule(f"nt-s1a-{tag}"))
    wf = (body.get("data") or {}).get("workflow") or {}
    check("POST before_save/veto isActive:false ⇒ 201", code == 201, f"{code} {body.get('code', '')}")
    if code != 201:
        return
    cleanup.append(("workflow", wf["id"]))
    code, body = req(base, "GET", f"/admin/workflows/{wf['id']}", adm)
    got = (body.get("data") or {}).get("workflow") or {}
    cond, acts = got.get("condition") or {}, got.get("actions") or []
    check("GET giữ trigger before_save + saveOn update", got.get("trigger") == "before_save" and cond.get("saveOn") == "update",
          f"trigger={got.get('trigger')} saveOn={cond.get('saveOn')}")
    check("GET giữ veto field + message", len(acts) == 1 and acts[0].get("type") == "veto"
          and acts[0].get("field") == "close_date" and acts[0].get("message") == VETO_MSG, json.dumps(acts, ensure_ascii=False))

    # S3 deleted the R13 activation gate: every kind of before rule runs, so it switches on. The
    # condition names this run's tag, so the rule matches no record while it is on.
    rule = {"name": f"nt-s1a-del-{tag}", "entityName": ENTITY, "trigger": "before_delete",
            "condition": {"rules": [{"field": "name", "op": "eq", "value": f"nt-s1a-{tag}-never"}]},
            "actions": [{"type": "veto", "message": "Không xoá cơ hội đã Thắng"}]}
    code, body = req(base, "POST", "/admin/workflows", adm, rule)
    if code == 201:
        cleanup.append(("workflow", body["data"]["workflow"]["id"]))
    check("before_delete bỏ trống isActive (mặc định bật) ⇒ 201, đang bật",
          code == 201 and ((body.get("data") or {}).get("workflow") or {}).get("isActive") is True, f"{code} {body.get('code', '')}")

    print("\n[admin] hành động có tác dụng phụ trên quy tắc trước khi lưu")
    rule = veto_rule(f"nt-s1a-mail-{tag}", actions=[{"type": "email_alert", "to": "a@example.com", "subject": "s", "body": "b"}])
    code, body = req(base, "POST", "/admin/workflows", adm, rule)
    if code == 201:
        cleanup.append(("workflow", body["data"]["workflow"]["id"]))
    check("before_save + email_alert ⇒ 422 WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER",
          code == 422 and body.get("code") == "WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER", f"{code} {body.get('code', '')}")

    print("\n[test@example.com] người không-admin")
    code, body = req(base, "POST", "/admin/workflows", staff, veto_rule(f"nt-s1a-staff-{tag}"))
    if code == 201:
        cleanup.append(("workflow", body["data"]["workflow"]["id"]))
    check("không tạo được quy tắc ⇒ 403", code == 403, f"{code} {body.get('code', '')}")

    code, body = req(base, "POST", f"/{ENTITY}", staff, {"name": f"Nghiệm thu S1a {tag}", "stage": "Proposal"})
    rid = (body.get("data") or {}).get("id")
    check("tạo cơ hội của mình ⇒ 201", code == 201 and bool(rid), f"{code} {body.get('code', '')}")
    if code != 201 or not rid:
        return
    cleanup.append(("record", rid))
    code, body = req(base, "PUT", f"/{ENTITY}/{rid}", staff, {"stage": "Closed Won"})
    check("sửa sang Thắng khi Ngày chốt trống ⇒ 200 (quy tắc tắt, không đổi hành vi)", code == 200, f"{code} {body.get('code', '')}")
    code, body = req(base, "GET", f"/{ENTITY}/{rid}", staff)
    stage = (((body.get("data") or {}).get("data")) or {}).get("stage")
    check("tải lại thấy Giai đoạn Thắng đã lưu", code == 200 and stage == "Closed Won", f"{code} stage={stage}")


def create_opp(base, staff, cleanup, name, stage):
    code, body = req(base, "POST", f"/{ENTITY}", staff, {"name": name, "stage": stage})
    rid = (body.get("data") or {}).get("id")
    check(f"tạo cơ hội của mình ở {stage} ⇒ 201", code == 201 and bool(rid), f"{code} {body.get('code', '')}")
    if code == 201 and rid:
        cleanup.append(("record", rid))
    return rid if code == 201 else None


def read_opp(base, staff, rid):
    code, body = req(base, "GET", f"/{ENTITY}/{rid}", staff)
    rec = body.get("data") or {}
    return code, rec.get("data") or {}, rec.get("version")


def wants_veto(label, code, body, anchored=True):
    check(f"{label} ⇒ 422 RECORD_HOOK_VETO", code == 422 and body.get("code") == "RECORD_HOOK_VETO", f"{code} {body.get('code', '')}")
    check("câu lỗi đúng thông điệp của quy tắc", body.get("error") == VETO_MSG, body.get("error", ""))
    details = body.get("details") or {}
    vs = details.get("violations") or []
    if anchored:
        check("gắn vào ô Ngày chốt (details.field)", details.get("field") == "close_date", json.dumps(details, ensure_ascii=False))
    check("một vi phạm, không lộ mã quy tắc cho người không-admin",
          len(vs) == 1 and vs[0].get("message") == VETO_MSG and "ruleId" not in vs[0], json.dumps(vs, ensure_ascii=False))
    return details, vs


def slice_s1b(base, adm, staff, cleanup, staff_user):
    tag = uuid.uuid4().hex[:8]

    print("\n[admin] bật quy tắc chặn khi SỬA (saveOn=update)")
    code, body = req(base, "POST", "/admin/workflows", adm, veto_rule(f"nt-s1b-{tag}", isActive=True))
    wf = (body.get("data") or {}).get("workflow") or {}
    check("POST before_save/veto saveOn=update isActive:true ⇒ 201, đang bật", code == 201 and wf.get("isActive") is True,
          f"{code} {body.get('code', '')}")
    if code != 201:
        return
    cleanup.append(("workflow", wf["id"]))

    name = f"Nghiệm thu S1b {tag}"
    print("\n[test@example.com] (1) form sửa: Giai đoạn sang Thắng, Ngày chốt để trống")
    rid = create_opp(base, staff, cleanup, name, "Proposal")
    if not rid:
        return
    _, _, version = read_opp(base, staff, rid)
    # The form resends the edited fields with the version and an emptied optional field as null.
    code, body = req(base, "PUT", f"/{ENTITY}/{rid}", staff,
                     {"name": name, "stage": "Closed Won", "close_date": None, "version": version})
    wants_veto("PUT form", code, body)
    code, data, v2 = read_opp(base, staff, rid)
    check("tải lại thấy Giai đoạn cũ, phiên bản không đổi", code == 200 and data.get("stage") == "Proposal" and v2 == version,
          f"stage={data.get('stage')} version {version}→{v2}")

    print("\n[test@example.com] (2) kéo thẻ sang cột Thắng trên Kanban (PUT chỉ Giai đoạn)")
    code, body = req(base, "PUT", f"/{ENTITY}/{rid}", staff, {"stage": "Closed Won"})
    wants_veto("PUT Kanban", code, body)

    print("\n[test@example.com] (3) điền Ngày chốt rồi lưu")
    code, body = req(base, "PUT", f"/{ENTITY}/{rid}", staff,
                     {"name": name, "stage": "Closed Won", "close_date": "2026-10-15", "version": version})
    check("PUT có Ngày chốt ⇒ 200", code == 200, f"{code} {body.get('code', '')} {body.get('error', '')}")
    _, data, _ = read_opp(base, staff, rid)
    check("tải lại thấy Thắng + Ngày chốt", data.get("stage") == "Closed Won" and data.get("close_date") == "2026-10-15",
          f"stage={data.get('stage')} close_date={data.get('close_date')}")

    print("\n[test@example.com] (4) cơ hội đã Thắng từ trước (Ngày chốt trống), chỉ sửa Tên")
    rid_won = create_opp(base, staff, cleanup, f"{name} — đã thắng", "Closed Won")
    if rid_won:
        code, body = req(base, "PUT", f"/{ENTITY}/{rid_won}", staff, {"name": f"{name} — đổi tên"})
        check("sửa Tên ⇒ 200 (\"vừa đổi thành\" không bắt bản ghi cũ)", code == 200, f"{code} {body.get('code', '')}")

    print("\n[admin] vai của test@example.com KHÔNG được xem Ngày chốt")
    role_id = staff_user.get("roleId")
    code, body = req(base, "GET", f"/admin/field-acl?roleId={role_id}&entity={ENTITY}", adm)
    prev = [g for g in ((body.get("data") or {}).get("grants") or []) if g.get("fieldName") == "close_date"]
    check("đọc được cấu hình quyền trường hiện có", code == 200 and bool(role_id), f"{code} roleId={role_id}")
    if code != 200 or not role_id:
        return

    def restore_field_acl():
        if prev:
            g = prev[0]
            req(base, "PUT", "/admin/field-acl", adm, {"grants": [{"roleId": role_id, "entityName": ENTITY, "fieldName": "close_date",
                                                                   "canView": g.get("canView"), "canEdit": g.get("canEdit")}]})
        else:
            req(base, "DELETE", f"/admin/field-acl?roleId={role_id}&entity={ENTITY}&field=close_date", adm)

    code, body = req(base, "PUT", "/admin/field-acl", adm, {"grants": [
        {"roleId": role_id, "entityName": ENTITY, "fieldName": "close_date", "canView": False, "canEdit": False}]})
    cleanup.append(("call", restore_field_acl))
    check("ẩn Ngày chốt với vai crm_sales_rep ⇒ 200", code == 200, f"{code} {body.get('code', '')}")
    rid_hidden = create_opp(base, staff, cleanup, f"{name} — trường ẩn", "Proposal")
    if rid_hidden:
        code, body = req(base, "PUT", f"/{ENTITY}/{rid_hidden}", staff, {"stage": "Closed Won"})
        details, vs = wants_veto("PUT khi Ngày chốt bị ẩn", code, body, anchored=False)
        check("không gắn vào trường ẩn, không nêu tên trường",
              "field" not in details and all("field" not in v for v in vs) and "close_date" not in json.dumps(body),
              json.dumps(body, ensure_ascii=False))
    restore_field_acl()

    print("\n[admin] tắt quy tắc ⇒ lần lưu vi phạm qua ngay")
    code, body = req(base, "PUT", f"/admin/workflows/{wf['id']}", adm, veto_rule(f"nt-s1b-{tag}", isActive=False))
    check("PUT isActive:false ⇒ 200", code == 200, f"{code} {body.get('code', '')}")
    if rid_hidden:
        code, body = req(base, "PUT", f"/{ENTITY}/{rid_hidden}", staff, {"stage": "Closed Won"})
        check("test@example.com lưu lại lần vi phạm cũ ⇒ 200", code == 200, f"{code} {body.get('code', '')}")


def grant_own_delete(base, adm, cleanup, role_id, entity):
    """Give role_id delete:owner on entity if it lacks any delete grant; finally restores the role."""
    code, body = req(base, "GET", f"/admin/roles/{role_id}", adm)
    actions = ((body.get("data") or {}).get("role") or {}).get("actions")
    check("đọc được quyền của vai", code == 200 and actions is not None, f"{code} roleId={role_id}")
    if code != 200 or actions is None:
        return False
    original = [{k: a[k] for k in ("entityName", "action", "accessLevel")} for a in actions]
    if not any(a["entityName"] == entity and a["action"] == "delete" for a in original):
        code, body = req(base, "PUT", f"/admin/roles/{role_id}", adm,
                         {"actions": original + [{"entityName": entity, "action": "delete", "accessLevel": "owner"}]})
        cleanup.append(("call", lambda: req(base, "PUT", f"/admin/roles/{role_id}", adm, {"actions": original})))
        check(f"thêm quyền xoá:của mình trên {entity} ⇒ 200", code == 200, f"{code} {body.get('code', '')}")
    return True


def slice_s1b_files(base, adm, staff, cleanup, staff_user):
    tag = uuid.uuid4().hex[:8]

    print("\n[admin] vai của test@example.com được xoá hợp đồng của mình (gỡ tệp cần quyền xoá)")
    if not grant_own_delete(base, adm, cleanup, staff_user.get("roleId"), CONTRACTS):
        return

    # The CRM pack's file fields were retired on the northwind demo tenant, so the slice adds its own and
    # soft-deletes it in finally (a fresh name per run: a retired name stays reserved).
    field = f"nt_ban_ky_{tag}"
    print(f"\n[admin] thêm trường tệp {field} cho hợp đồng")
    code, body = req(base, "POST", "/admin/fields", adm,
                     {"entityName": CONTRACTS, "name": field, "label": "Bản ký (nghiệm thu)", "fieldType": "file"})
    check("POST trường tệp ⇒ 201", code in (200, 201), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code not in (200, 201):
        return
    _, listed = req(base, "GET", f"/metadata/fields/{CONTRACTS}", adm)
    fields = listed.get("data", listed) if isinstance(listed, dict) else listed
    field_id = next((f.get("id") for f in (fields or []) if f.get("name") == field), None)
    if field_id:
        cleanup.append(("field", field_id))

    print("\n[test@example.com] tạo hợp đồng, đính kèm bản ký")
    name = f"Nghiệm thu khoá tệp {tag}"
    code, body = req(base, "POST", f"/{CONTRACTS}", staff, {"name": name, "status": "Nháp"})
    rid = (body.get("data") or {}).get("id")
    check("tạo hợp đồng của mình ⇒ 201", code == 201 and bool(rid), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201 or not rid:
        return
    cleanup.append(("contract", rid))
    signed = b"ban ky hop dong " + tag.encode()
    code, body = upload(base, staff, rid, field, "ban-ky.txt", signed)
    fid = (body.get("data") or {}).get("id")
    check("đính kèm bản ký ⇒ 201", code == 201 and bool(fid), f"{code} {body.get('code', '')}")
    if not fid:
        return

    def attached():
        _, b = req(base, "GET", f"/{CONTRACTS}/{rid}", staff)
        refs = (((b.get("data") or {}).get("data")) or {}).get(field) or []
        return [r.get("id") for r in refs]

    print("\n[admin] bật quy tắc khoá đúng hợp đồng này (before_save, saveOn=update)")
    rule = {"name": f"nt-s1b-files-{tag}", "entityName": CONTRACTS, "trigger": "before_save", "isActive": True,
            "condition": {"rules": [{"field": "name", "op": "eq", "value": name}], "saveOn": "update"},
            "actions": [{"type": "veto", "message": LOCK_MSG}]}
    code, body = req(base, "POST", "/admin/workflows", adm, rule)
    wf = (body.get("data") or {}).get("workflow") or {}
    check("POST quy tắc khoá ⇒ 201, đang bật", code == 201 and wf.get("isActive") is True, f"{code} {body.get('code', '')}")
    if code != 201:
        return
    cleanup.append(("workflow", wf["id"]))

    print("\n[test@example.com] gỡ bản ký khỏi hợp đồng đang khoá")
    code, body = req(base, "DELETE", f"/files/{fid}", staff)
    check("DELETE /files/:id ⇒ 422 RECORD_HOOK_VETO", code == 422 and body.get("code") == "RECORD_HOOK_VETO",
          f"{code} {body.get('code', '')}")
    check("câu lỗi đúng thông điệp của quy tắc", body.get("error") == LOCK_MSG, body.get("error", ""))
    code, data = raw(base, "GET", f"/files/{fid}", staff)
    check("tải bản ký về VẪN được, đúng nội dung", code == 200 and data == signed, f"{code} {len(data)} byte")
    check("hợp đồng vẫn liệt kê bản ký", attached() == [fid], json.dumps(attached()))

    print("\n[test@example.com] đính kèm thêm phụ lục khi đang khoá")
    code, body = upload(base, staff, rid, field, "phu-luc.txt", b"phu luc")
    check("tải phụ lục ⇒ 422 RECORD_HOOK_VETO", code == 422 and body.get("code") == "RECORD_HOOK_VETO",
          f"{code} {body.get('code', '')}")
    check("hợp đồng chỉ còn bản ký", attached() == [fid], json.dumps(attached()))

    print("\n[admin] tắt quy tắc ⇒ gỡ được")
    code, body = req(base, "PUT", f"/admin/workflows/{wf['id']}", adm, dict(rule, isActive=False))
    check("PUT isActive:false ⇒ 200", code == 200, f"{code} {body.get('code', '')}")
    code, body = req(base, "DELETE", f"/files/{fid}", staff)
    check("gỡ bản ký ⇒ 204", code == 204, f"{code} {body.get('code', '')}")
    code, _ = raw(base, "GET", f"/files/{fid}", staff)
    check("tải bản ký về ⇒ 404", code == 404, str(code))
    check("hợp đồng không còn tệp nào (phụ lục bị từ chối không quay lại)", attached() == [], json.dumps(attached()))


def import_csv(base, token, text, mapping):
    boundary = "nexus" + uuid.uuid4().hex
    body = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="co-hoi.csv"\r\n'
            f"Content-Type: text/csv\r\n\r\n").encode() + text.encode() + (
        f'\r\n--{boundary}\r\nContent-Disposition: form-data; name="mapping"\r\n\r\n{json.dumps(mapping)}'
        f"\r\n--{boundary}--\r\n").encode()
    code, data = raw(base, "POST", f"/{ENTITY}/import", token, body, f"multipart/form-data; boundary={boundary}")
    try:
        return code, (json.loads(data) if data else {})
    except ValueError:
        return code, {}


def tagged_opps(base, token, tag):
    """Every opportunity whose name carries tag, as {name: (id, opp_code)}."""
    _, body = req(base, "GET", f"/{ENTITY}?q={tag}&limit=100", token)
    out = {}
    for rec in body.get("data") or []:
        data = rec.get("data") or {}
        out[data.get("name")] = (rec.get("id"), data.get("opp_code"))
    return out


def code_number(code):
    try:
        return int(str(code).rsplit("-", 1)[-1])
    except ValueError:
        return None


def slice_s2(base, adm, staff, cleanup, _staff_user):
    tag = uuid.uuid4().hex[:8]
    name = f"Nghiệm thu S2 {tag}"
    rule = {"name": f"nt-s2-{tag}", "entityName": ENTITY, "trigger": "before_save", "isActive": True,
            "condition": {"rules": [{"field": "stage", "op": "eq", "value": "Closed Won"}, {"field": "close_date", "op": "isnull"}]},
            "actions": [{"type": "veto", "field": "close_date", "message": VETO_MSG}]}

    def forget_created():
        # Records the script created without an id in hand (bulk, import) are found by their tag.
        for rid, _ in tagged_opps(base, adm, tag).values():
            req(base, "DELETE", f"/{ENTITY}/{rid}", adm)

    cleanup.append(("call", forget_created))

    print("\n[admin] bật quy tắc chặn áp cả khi TẠO MỚI lẫn khi sửa (saveOn bỏ trống)")
    code, body = req(base, "POST", "/admin/workflows", adm, rule)
    wf = (body.get("data") or {}).get("workflow") or {}
    check("POST before_save/veto (tạo mới + sửa) isActive:true ⇒ 201, đang bật", code == 201 and wf.get("isActive") is True,
          f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201:
        return
    cleanup.append(("workflow", wf["id"]))

    print("\n[test@example.com] (1) form tạo mới: Thắng, Ngày chốt để trống")
    create_opp(base, staff, cleanup, f"{name} — 1", "Proposal")
    code, body = req(base, "POST", f"/{ENTITY}", staff, {"name": f"{name} — bị chặn", "stage": "Closed Won", "close_date": None})
    wants_veto("POST tạo mới", code, body)
    create_opp(base, staff, cleanup, f"{name} — 2", "Proposal")
    made = tagged_opps(base, staff, tag)
    n1, n2 = code_number(made.get(f"{name} — 1", (None, None))[1]), code_number(made.get(f"{name} — 2", (None, None))[1])
    check("cơ hội hợp lệ kế tiếp nhận mã OPP- liền số", n1 is not None and n2 == n1 + 1,
          f"{made.get(f'{name} — 1', ('', ''))[1]} → {made.get(f'{name} — 2', ('', ''))[1]}")
    check("lần tạo bị chặn không để lại bản ghi", f"{name} — bị chặn" not in made)

    batch = [{"name": f"{name} — lô 1", "stage": "Proposal"},
             {"name": f"{name} — lô 2", "stage": "Closed Won"},
             {"name": f"{name} — lô 3", "stage": "Closed Won", "close_date": "2026-10-20"}]
    print("\n[test@example.com] (2) tạo nhiều dòng, chế độ \"tất cả hoặc không\", dòng 2 vi phạm")
    code, body = req(base, "POST", f"/{ENTITY}/bulk?mode=atomic", staff, {"records": batch})
    data = body.get("data") or {}
    rows = data.get("results") or []
    check("tạo nhiều dòng (tất cả hoặc không) ⇒ 422, không dòng nào được tạo", code == 422 and data.get("created") == 0,
          f"{code} created={data.get('created')}")
    row2 = rows[1] if len(rows) == 3 else {}
    check("dòng 2 báo đúng câu của quy tắc, gắn Ngày chốt",
          row2.get("code") == "RECORD_HOOK_VETO" and row2.get("error") == VETO_MSG and (row2.get("details") or {}).get("field") == "close_date",
          json.dumps(row2, ensure_ascii=False))
    check("dòng 1 và 3 bị bỏ qua vì dòng khác lỗi", len(rows) == 3 and rows[0].get("status") == "skipped" and rows[2].get("status") == "skipped",
          json.dumps([r.get("status") for r in rows]))
    check("không bản ghi nào của lô tồn tại", not any(k.startswith(f"{name} — lô") for k in tagged_opps(base, staff, tag)))

    print("\n[test@example.com] cùng lô, chế độ \"tạo những dòng hợp lệ\"")
    code, body = req(base, "POST", f"/{ENTITY}/bulk?mode=partial", staff, {"records": batch})
    data = body.get("data") or {}
    rows = data.get("results") or []
    check("tạo 2, dòng 2 báo đúng câu",
          code == 200 and data.get("created") == 2 and len(rows) == 3 and rows[1].get("code") == "RECORD_HOOK_VETO"
          and rows[1].get("error") == VETO_MSG, f"{code} created={data.get('created')} {json.dumps(rows[1:2], ensure_ascii=False)}")

    print("\n[test@example.com] (3) nhập CSV 3 dòng, dòng 2 vi phạm")
    csv_text = (f"name,stage,close_date\n{name} — nhập 1,Proposal,\n{name} — nhập 2,Closed Won,\n"
                f"{name} — nhập 3,Closed Won,2026-10-21\n")
    code, body = import_csv(base, staff, csv_text, {"name": "name", "stage": "stage", "close_date": "close_date"})
    data = body.get("data") or {}
    errs = data.get("errors") or []
    check("nhập 2, lỗi 1", code == 200 and data.get("inserted") == 2 and data.get("failed") == 1,
          f"{code} {body.get('code', '')} inserted={data.get('inserted')} failed={data.get('failed')}")
    check("dòng 2 — đúng câu của quy tắc, gắn Ngày chốt",
          len(errs) == 1 and errs[0].get("row") == 2 and errs[0].get("code") == "RECORD_HOOK_VETO"
          and errs[0].get("message") == VETO_MSG and errs[0].get("field") == "close_date", json.dumps(errs, ensure_ascii=False))

    print("\n[test@example.com] (4) cơ hội cuối + dải mã OPP- không lỗ")
    create_opp(base, staff, cleanup, f"{name} — 3", "Proposal")
    made = tagged_opps(base, staff, tag)
    nums = sorted(n for n in (code_number(c) for _, c in made.values()) if n is not None)
    check("7 cơ hội được tạo (2 form + 2 lô + 2 CSV + 1 cuối), không cái bị chặn nào",
          len(made) == 7 and not any("bị chặn" in k or k.endswith(("lô 2", "nhập 2")) for k in made), json.dumps(sorted(made), ensure_ascii=False))
    check("mã OPP- liền một dải, không lỗ (lần bị chặn không đốt số)",
          len(nums) == 7 and nums == list(range(nums[0], nums[0] + 7)), json.dumps(sorted(c for _, c in made.values())))

    print("\n[admin] đổi quy tắc sang chỉ áp khi SỬA")
    code, body = req(base, "PUT", f"/admin/workflows/{wf['id']}", adm, dict(rule, condition=dict(rule["condition"], saveOn="update")))
    check("PUT saveOn=update ⇒ 200", code == 200, f"{code} {body.get('code', '')}")
    rid = create_opp(base, staff, cleanup, f"{name} — thắng không ngày", "Closed Won")
    if rid:
        code, body = req(base, "PUT", f"/{ENTITY}/{rid}", staff, {"name": f"{name} — thắng không ngày (sửa)"})
        wants_veto("sửa cơ hội vẫn vi phạm", code, body)


DEL_MSG = "Không xoá cơ hội đã Thắng"


def slice_s3(base, adm, staff, cleanup, staff_user):
    tag = uuid.uuid4().hex[:8]
    name = f"Nghiệm thu S3 {tag}"

    print("\n[admin] vai của test@example.com được xoá cơ hội của mình (crm_sales_rep không có quyền xoá cơ hội)")
    if not grant_own_delete(base, adm, cleanup, staff_user.get("roleId"), ENTITY):
        return

    print("\n[test@example.com] tạo một cơ hội Thắng và hai cơ hội đang Tìm kiếm")
    ids = {}
    for key, stage, extra in (("won", "Closed Won", {"close_date": "2026-10-15"}), ("open1", "Prospecting", {}), ("open2", "Prospecting", {})):
        code, body = req(base, "POST", f"/{ENTITY}", staff, dict({"name": f"{name} — {key}", "stage": stage}, **extra))
        rid = (body.get("data") or {}).get("id")
        check(f"tạo cơ hội {key} ⇒ 201", code == 201 and bool(rid), f"{code} {body.get('code', '')} {body.get('error', '')}")
        if code != 201 or not rid:
            return
        cleanup.append(("record", rid))
        ids[key] = rid

    # Created AFTER the records: finally runs in reverse, so the rule is gone before the records are.
    print("\n[admin] bật quy tắc trước khi xoá: Giai đoạn là Thắng ⇒ chặn (isActive bỏ trống = bật)")
    rule = {"name": f"nt-s3-{tag}", "entityName": ENTITY, "trigger": "before_delete",
            "condition": {"rules": [{"field": "stage", "op": "eq", "value": "Closed Won"}]},
            "actions": [{"type": "veto", "message": DEL_MSG}]}
    code, body = req(base, "POST", "/admin/workflows", adm, rule)
    wf = (body.get("data") or {}).get("workflow") or {}
    check("POST before_delete/veto ⇒ 201, đang bật", code == 201 and wf.get("isActive") is True,
          f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201:
        return
    cleanup.append(("workflow", wf["id"]))

    print("\n[test@example.com] (1) xoá cơ hội Thắng")
    code, body = req(base, "DELETE", f"/{ENTITY}/{ids['won']}", staff)
    check("DELETE cơ hội Thắng ⇒ 422 RECORD_HOOK_VETO", code == 422 and body.get("code") == "RECORD_HOOK_VETO", f"{code} {body.get('code', '')}")
    check("câu lỗi đúng thông điệp của quy tắc", body.get("error") == DEL_MSG, body.get("error", ""))
    details = body.get("details") or {}
    vs = details.get("violations") or []
    check("không gắn trường, không lộ mã quy tắc",
          "field" not in details and len(vs) == 1 and vs[0].get("message") == DEL_MSG and "ruleId" not in vs[0],
          json.dumps(details, ensure_ascii=False))
    code, stored, _ = read_opp(base, staff, ids["won"])
    check("tải lại: cơ hội Thắng vẫn còn", code == 200 and stored.get("stage") == "Closed Won", f"{code} stage={stored.get('stage')}")

    print("\n[test@example.com] (2) xoá cơ hội đang Tìm kiếm")
    code, body = req(base, "DELETE", f"/{ENTITY}/{ids['open1']}", staff)
    check("DELETE cơ hội Tìm kiếm ⇒ 204", code == 204, f"{code} {body.get('code', '')}")

    print("\n[test@example.com] (3) xoá hàng loạt [Thắng, Tìm kiếm]")
    code, body = req(base, "POST", f"/{ENTITY}/bulk-delete", staff, {"ids": [ids["won"], ids["open2"]]})
    data = body.get("data") or {}
    rows = {r.get("id"): r for r in data.get("results") or []}
    check("xoá hàng loạt ⇒ 200, xoá 1, lỗi 1", code == 200 and data.get("deleted") == 1 and data.get("failed") == 1,
          f"{code} deleted={data.get('deleted')} failed={data.get('failed')}")
    won_row = rows.get(ids["won"]) or {}
    check("dòng Thắng: RECORD_HOOK_VETO + đúng câu của quy tắc (không phải mã)",
          won_row.get("ok") is False and won_row.get("code") == "RECORD_HOOK_VETO" and won_row.get("error") == DEL_MSG,
          json.dumps(won_row, ensure_ascii=False))
    check("dòng Tìm kiếm được xoá", (rows.get(ids["open2"]) or {}).get("ok") is True, json.dumps(rows.get(ids["open2"]), ensure_ascii=False))
    code, _, _ = read_opp(base, staff, ids["won"])
    check("cơ hội Thắng vẫn còn sau xoá hàng loạt", code == 200, str(code))

    print("\n[admin] lịch sử chạy của quy tắc")
    runs = []
    for _ in range(20):  # the run history is written off the request's path (RunSink)
        _, body = req(base, "GET", f"/admin/workflows/{wf['id']}/runs?limit=20", adm)
        runs = (body.get("data") or {}).get("runs") or []
        if len(runs) >= 2:
            break
        time.sleep(0.25)
    check("đúng 2 dòng Đã chặn (xoá lẻ + dòng của xoá hàng loạt)",
          len(runs) == 2 and all(r.get("status") == "blocked" for r in runs), json.dumps([r.get("status") for r in runs]))
    check("trigger trước khi xoá, trỏ đúng cơ hội Thắng",
          all(r.get("triggerType") == "before_delete" and r.get("recordId") == ids["won"] for r in runs),
          json.dumps([(r.get("triggerType"), r.get("recordId")) for r in runs]))
    raw_runs = json.dumps([r.get("actionsResult") for r in runs], ensure_ascii=False)
    check("không chứa giá trị nào của bản ghi (tên, giai đoạn, thẻ nghiệm thu)",
          all(v not in raw_runs for v in (tag, "Closed Won", "Nghiệm thu")) and '"veto"' in raw_runs, raw_runs)

    print("\n[admin] tắt quy tắc ⇒ xoá được")
    code, body = req(base, "PUT", f"/admin/workflows/{wf['id']}", adm, dict(rule, isActive=False))
    check("PUT isActive:false ⇒ 200", code == 200, f"{code} {body.get('code', '')}")
    code, body = req(base, "DELETE", f"/{ENTITY}/{ids['won']}", staff)
    check("DELETE cơ hội Thắng ⇒ 204", code == 204, f"{code} {body.get('code', '')}")


LEADS = "crm_leads"


def wait_runs(base, adm, wf_id, want):
    runs = []
    for _ in range(20):  # the run history is written off the request's path (RunSink)
        _, body = req(base, "GET", f"/admin/workflows/{wf_id}/runs?limit=20", adm)
        runs = (body.get("data") or {}).get("runs") or []
        if len(runs) >= want:
            break
        time.sleep(0.25)
    return runs


def slice_s4(base, adm, staff, cleanup, staff_user):
    tag = uuid.uuid4().hex[:8]
    name = f"Nghiệm thu S4 {tag}"

    print("\n[admin] cấu hình tự điền sai bị từ chối lúc lưu")
    bad = {"name": f"nt-s4-bad-{tag}", "entityName": ENTITY, "trigger": "before_save", "isActive": False,
           "actions": [{"type": "field_update", "field": "account", "value": None}]}
    code, body = req(base, "POST", "/admin/workflows", adm, bad)
    if code == 201:
        cleanup.append(("workflow", body["data"]["workflow"]["id"]))
    check("đích là trường quan hệ ⇒ 422 WORKFLOW_BEFORE_TARGET_UNSUPPORTED, gắn Khách hàng",
          code == 422 and body.get("code") == "WORKFLOW_BEFORE_TARGET_UNSUPPORTED" and (body.get("details") or {}).get("field") == "account",
          f"{code} {body.get('code', '')} {json.dumps(body.get('details'), ensure_ascii=False)}")
    bad = dict(bad, entityName=LEADS, actions=[{"type": "field_update", "field": "source", "value": "Website", "onlyIfEmpty": True}])
    code, body = req(base, "POST", "/admin/workflows", adm, bad)
    if code == 201:
        cleanup.append(("workflow", body["data"]["workflow"]["id"]))
    details = body.get("details") or {}
    check("hằng \"Website\" không phải giá trị lựa chọn ⇒ 422 WORKFLOW_BEFORE_VALUE_INVALID (RECORD_INVALID_PICKLIST)",
          code == 422 and body.get("code") == "WORKFLOW_BEFORE_VALUE_INVALID" and details.get("field") == "source"
          and details.get("reason") == "RECORD_INVALID_PICKLIST", f"{code} {body.get('code', '')} {json.dumps(details, ensure_ascii=False)}")

    print("\n[test@example.com] tạo cơ hội Báo giá, Xác suất 40, có Ngày chốt")
    amount = {"amount": 1000000, "currencyCode": "VND"}
    code, body = req(base, "POST", f"/{ENTITY}", staff, {"name": name, "stage": "Proposal", "amount": amount,
                                                          "probability": 40, "close_date": "2026-10-15"})
    opp = (body.get("data") or {}).get("id")
    check("tạo cơ hội ⇒ 201", code == 201 and bool(opp), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201 or not opp:
        return
    cleanup.append(("record", opp))

    # Created AFTER the records: finally runs in reverse, so the rules are gone before the records are.
    print("\n[admin] bật quy tắc 1 (cơ hội) và quy tắc 2 (lead)")
    won = {"name": f"nt-s4-won-{tag}", "entityName": ENTITY, "trigger": "before_save", "isActive": True,
           "condition": {"rules": [{"field": "stage", "op": "changedTo", "value": "Closed Won"}]},
           "actions": [{"type": "field_update", "field": "probability", "value": 100}]}
    code, body = req(base, "POST", "/admin/workflows", adm, won)
    won_wf = (body.get("data") or {}).get("workflow") or {}
    check("quy tắc 1 ⇒ 201, đang bật", code == 201 and won_wf.get("isActive") is True, f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201:
        return
    cleanup.append(("workflow", won_wf["id"]))
    web = {"name": f"nt-s4-web-{tag}", "entityName": LEADS, "trigger": "before_save", "isActive": True,
           "condition": {"saveOn": "create"},
           "actions": [{"type": "field_update", "field": "source", "value": "Web", "onlyIfEmpty": True}]}
    code, body = req(base, "POST", "/admin/workflows", adm, web)
    web_wf = (body.get("data") or {}).get("workflow") or {}
    check("quy tắc 2 ⇒ 201, đang bật", code == 201 and web_wf.get("isActive") is True, f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201:
        return
    cleanup.append(("workflow", web_wf["id"]))
    code, body = req(base, "GET", f"/admin/workflows/{web_wf['id']}", adm)
    acts = (((body.get("data") or {}).get("workflow") or {}).get("actions")) or []
    check("GET giữ onlyIfEmpty", len(acts) == 1 and acts[0].get("onlyIfEmpty") is True, json.dumps(acts, ensure_ascii=False))

    print("\n[test@example.com] đổi Giai đoạn sang Thắng (chỉ gửi Giai đoạn)")
    code, body = req(base, "PUT", f"/{ENTITY}/{opp}", staff, {"stage": "Closed Won"})
    check("PUT Giai đoạn = Thắng ⇒ 200", code == 200, f"{code} {body.get('code', '')} {body.get('error', '')}")
    # Plan R9: the rule wrote probability 100; the response must not echo record data back (a role
    # that may not see a rule-filled field would otherwise read it from the reply).
    check("phản hồi PUT chỉ có id, không mang dữ liệu bản ghi (R9)", body == {"data": {"id": opp}}, json.dumps(body, ensure_ascii=False))
    code, stored, version = read_opp(base, staff, opp)
    weighted = stored.get("weighted_value") or {}
    check("tải lại: Xác suất 100 (quy tắc tự điền)", code == 200 and stored.get("probability") == 100, f"probability={stored.get('probability')}")
    check("Giá trị kỳ vọng = Giá trị (công thức tính lại cùng lần lưu)",
          isinstance(weighted, dict) and weighted.get("amount") == amount["amount"], json.dumps(weighted, ensure_ascii=False))

    print("\n[test@example.com] tab Lịch sử của cơ hội")
    updates = []
    for _ in range(20):  # the audit row is written off the request's path
        code, body = req(base, "GET", f"/audit-log/{ENTITY}/{opp}?limit=20", staff)
        updates = [e for e in (body.get("data") or []) if e.get("action") == "update"]
        if updates:
            break
        time.sleep(0.25)
    time.sleep(0.5)  # a second (wrong) update row would land in the same window — look again
    code, body = req(base, "GET", f"/audit-log/{ENTITY}/{opp}?limit=20", staff)
    updates = [e for e in (body.get("data") or []) if e.get("action") == "update"]
    after = ((updates[0].get("diff") or {}).get("after") or {}) if updates else {}
    check("đọc được lịch sử ⇒ 200", code == 200, f"{code} {body.get('code', '')}")
    check("ĐÚNG MỘT dòng sửa, của test@example.com", len(updates) == 1 and updates[0].get("actorUserId") == staff_user.get("id"),
          json.dumps([(e.get("actorUserId"), sorted(((e.get("diff") or {}).get("after") or {}).keys())) for e in updates], ensure_ascii=False))
    check("dòng đó gồm cả Giai đoạn lẫn Xác suất", after.get("stage") == "Closed Won" and after.get("probability") == 100,
          json.dumps(after, ensure_ascii=False))

    print("\n[test@example.com] tạo lead")
    leads = {}
    for key, extra in (("blank", {}), ("event", {"source": "Event"})):
        code, body = req(base, "POST", f"/{LEADS}", staff, dict({"full_name": f"{name} — {key}"}, **extra))
        rid = (body.get("data") or {}).get("id")
        check(f"tạo lead {key} ⇒ 201", code == 201 and bool(rid), f"{code} {body.get('code', '')} {body.get('error', '')}")
        if code != 201 or not rid:
            return
        cleanup.append(("lead", rid))
        leads[key] = rid
    for key, want in (("blank", "Web"), ("event", "Event")):
        code, body = req(base, "GET", f"/{LEADS}/{leads[key]}", staff)
        src = (((body.get("data") or {}).get("data")) or {}).get("source")
        check(f"lead {key}: Nguồn = {want}", code == 200 and src == want, f"{code} source={src}")

    print("\n[admin] lịch sử chạy của hai quy tắc")
    for wf, rid, field in ((won_wf, opp, "probability"), (web_wf, leads["blank"], "source")):
        runs = wait_runs(base, adm, wf["id"], 1)
        check(f"{field}: đúng một dòng Thành công trỏ đúng bản ghi",
              len(runs) == 1 and runs[0].get("status") == "success" and runs[0].get("recordId") == rid
              and runs[0].get("triggerType") == "before_save",
              json.dumps([(r.get("status"), r.get("recordId"), r.get("triggerType")) for r in runs]))
        raw_runs = json.dumps([r.get("actionsResult") for r in runs], ensure_ascii=False)
        check(f"{field}: không chứa giá trị nào (tên, Web, 100, thẻ nghiệm thu)",
              all(v not in raw_runs for v in (tag, "Nghiệm thu", '"Web"', "100")) and f'"{field}"' in raw_runs, raw_runs)


def add_field(base, adm, cleanup, entity, spec):
    """POST /admin/fields and queue its soft delete; returns the new field's id (None = refused)."""
    code, body = req(base, "POST", "/admin/fields", adm, dict(spec, entityName=entity))
    check(f"POST trường {spec['name']} ⇒ 201", code in (200, 201), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code not in (200, 201):
        return None
    _, listed = req(base, "GET", f"/metadata/fields/{entity}", adm)
    fields = listed.get("data", listed) if isinstance(listed, dict) else listed
    fid = next((f.get("id") for f in (fields or []) if f.get("name") == spec["name"]), None)
    if fid:
        cleanup.append(("field", fid))
    return fid


def slice_s4_review(base, adm, staff, cleanup, _staff_user):
    tag = uuid.uuid4().hex[:8]
    name = f"Nghiệm thu S4 review {tag}"
    note, code_f, cmnd, empty, prov, dist = (f"nt_ghi_chu_{tag}", f"nt_ma_{tag}", f"nt_cmnd_{tag}", f"nt_trong_{tag}",
                                             f"nt_tinh_{tag}", f"nt_quan_{tag}")

    print("\n[admin] mẫu chữ đọc thứ không render được ⇒ từ chối lúc lưu")
    for i, (label, tmpl, save_on, reason) in enumerate((
            ("qua liên kết {{company.name}}", "{{company.name}}", "", "template_source_unknown"),
            ("mã tự tăng trên quy tắc có áp khi tạo", "{{lead_code}}", "", "template_source_unsupported"),
            ("người dùng (chỉ ra id)", "{{assigned_to}}", "update", "template_source_unsupported"),
            ("mẫu trống", "   ", "", "template_empty"))):
        rule = {"name": f"nt-s4r-bad{i}-{tag}", "entityName": LEADS, "trigger": "before_save", "isActive": False,
                "actions": [{"type": "field_update", "field": "company", "value": tmpl, "valueSource": "template"}]}
        if save_on:
            rule["condition"] = {"saveOn": save_on}
        code, body = req(base, "POST", "/admin/workflows", adm, rule)
        if code == 201:
            cleanup.append(("workflow", body["data"]["workflow"]["id"]))
        details = body.get("details") or {}
        check(f"{label} ⇒ 422 WORKFLOW_BEFORE_VALUE_INVALID ({reason}), gắn Công ty",
              code == 422 and body.get("code") == "WORKFLOW_BEFORE_VALUE_INVALID" and details.get("reason") == reason
              and details.get("field") == "company", f"{code} {body.get('code', '')} {json.dumps(details, ensure_ascii=False)}")

    print("\n[admin] thêm 4 trường cho cơ hội, 2 cho lead")
    fids = {}
    for entity, spec in ((ENTITY, {"name": note, "label": "Ghi chú (nt)", "fieldType": "text"}),
                         (ENTITY, {"name": code_f, "label": "Mã (nt)", "fieldType": "text"}),
                         (ENTITY, {"name": cmnd, "label": "CMND cũ (nt)", "fieldType": "text"}),
                         (ENTITY, {"name": empty, "label": "Luôn trống (nt)", "fieldType": "text"}),
                         (LEADS, {"name": prov, "label": "Tỉnh (nt)", "fieldType": "picklist", "options": {"values": ["Hà Nội", "Đà Nẵng"]}}),
                         (LEADS, {"name": dist, "label": "Quận (nt)", "fieldType": "picklist", "options": {
                             "values": ["Ba Đình", "Hoàn Kiếm", "Hải Châu"], "controllingField": prov,
                             "dependentValues": {"Hà Nội": ["Ba Đình", "Hoàn Kiếm"], "Đà Nẵng": ["Hải Châu"]}}})):
        fids[spec["name"]] = add_field(base, adm, cleanup, entity, spec)
        if not fids[spec["name"]]:
            return

    print("\n[test@example.com] tạo cơ hội có ghi chú viết tay + CMND cũ")
    code, body = req(base, "POST", f"/{ENTITY}", staff, {"name": name, "stage": "Proposal", "close_date": "2026-10-15",
                                                          note: "Ghi chú viết tay", cmnd: "012345678"})
    opp = (body.get("data") or {}).get("id")
    check("tạo cơ hội ⇒ 201", code == 201 and bool(opp), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201 or not opp:
        return
    cleanup.append(("record", opp))

    print("\n[admin] bật 3 quy tắc")
    wfs = {}
    for key, entity, cond, action in (
            ("tmpl", ENTITY, {"saveOn": "update"}, {"type": "field_update", "field": note, "value": "{{" + empty + "}}", "valueSource": "template"}),
            ("copy", ENTITY, {"saveOn": "update"}, {"type": "field_update", "field": code_f, "value": cmnd, "valueSource": "field"}),
            ("dist", LEADS, {"saveOn": "create"}, {"type": "field_update", "field": dist, "value": "Ba Đình", "onlyIfEmpty": True})):
        code, body = req(base, "POST", "/admin/workflows", adm, {"name": f"nt-s4r-{key}-{tag}", "entityName": entity,
                                                                  "trigger": "before_save", "isActive": True, "condition": cond, "actions": [action]})
        wf = (body.get("data") or {}).get("workflow") or {}
        check(f"quy tắc {key} ⇒ 201, đang bật", code == 201 and wf.get("isActive") is True, f"{code} {body.get('code', '')} {body.get('error', '')}")
        if code != 201:
            return
        cleanup.append(("workflow", wf["id"]))
        wfs[key] = wf

    def read(entity, rid):
        code, body = req(base, "GET", f"/{entity}/{rid}", staff)
        return code, (((body.get("data") or {}).get("data")) or {})

    print("\n[test@example.com] sửa tên cơ hội")
    code, body = req(base, "PUT", f"/{ENTITY}/{opp}", staff, {"name": name + " (sửa)"})
    check("PUT tên ⇒ 200", code == 200, f"{code} {body.get('code', '')} {body.get('error', '')}")
    code, data = read(ENTITY, opp)
    check("ghi chú GIỮ nguyên — mẫu {{trường trống}} ra rỗng không xoá", code == 200 and data.get(note) == "Ghi chú viết tay", f"{note}={data.get(note)!r}")
    check("mã = CMND cũ — nguồn còn sống thì chép", data.get(code_f) == "012345678", f"{code_f}={data.get(code_f)!r}")

    print("\n[admin] xoá trường CMND cũ; [test@example.com] gõ mã tay")
    code, body = req(base, "DELETE", f"/admin/fields/{fids[cmnd]}", adm)
    check("DELETE trường CMND cũ ⇒ 200/204", code in (200, 204), f"{code} {body.get('code', '')}")
    code, body = req(base, "PUT", f"/{ENTITY}/{opp}", staff, {code_f: "Gõ tay"})
    check("PUT mã ⇒ 200", code == 200, f"{code} {body.get('code', '')} {body.get('error', '')}")
    code, data = read(ENTITY, opp)
    check("mã GIỮ \"Gõ tay\" — không chép giá trị của trường đã xoá", code == 200 and data.get(code_f) == "Gõ tay", f"{code_f}={data.get(code_f)!r}")

    print("\n[test@example.com] tạo lead theo tỉnh")
    made = {}
    for key, province in (("dn", "Đà Nẵng"), ("hn", "Hà Nội")):
        code, body = req(base, "POST", f"/{LEADS}", staff, {"full_name": f"{name} — {key}", prov: province})
        rid = (body.get("data") or {}).get("id")
        check(f"tạo lead {province} ⇒ 201", code == 201 and bool(rid), f"{code} {body.get('code', '')} {body.get('error', '')}")
        if code != 201 or not rid:
            return
        cleanup.append(("lead", rid))
        made[key] = rid
    _, data = read(LEADS, made["dn"])
    check("Đà Nẵng: KHÔNG có quận (Ba Đình không thuộc Đà Nẵng)", not data.get(dist), f"{dist}={data.get(dist)!r}")
    _, data = read(LEADS, made["hn"])
    check("Hà Nội: quận = Ba Đình", data.get(dist) == "Ba Đình", f"{dist}={data.get(dist)!r}")

    print("\n[admin] lịch sử chạy")
    time.sleep(1)  # the runs are written off the request's path — give the sink time before counting zero
    runs = wait_runs(base, adm, wfs["tmpl"]["id"], 0)
    check("quy tắc mẫu: 0 dòng (không đổi gì)", runs == [], json.dumps([(r.get("status"), r.get("error")) for r in runs]))
    runs = sorted(wait_runs(base, adm, wfs["copy"]["id"], 2), key=lambda r: r.get("startedAt") or "")
    check("quy tắc chép: Thành công rồi Lỗi WORKFLOW_BEFORE_VALUE_INVALID",
          [(r.get("status"), r.get("error") or "") for r in runs] == [("success", ""), ("failed", "WORKFLOW_BEFORE_VALUE_INVALID")],
          json.dumps([(r.get("status"), r.get("error")) for r in runs]))
    runs = wait_runs(base, adm, wfs["dist"]["id"], 2)
    by_rec = {r.get("recordId"): (r.get("status"), r.get("error") or "") for r in runs}
    check("quy tắc quận: Đà Nẵng Lỗi RECORD_DEPENDENT_PICKLIST_INVALID, Hà Nội Thành công",
          by_rec == {made["dn"]: ("failed", "RECORD_DEPENDENT_PICKLIST_INVALID"), made["hn"]: ("success", "")}, json.dumps(by_rec))
    raw_runs = json.dumps([r.get("actionsResult") for wf in wfs.values() for r in wait_runs(base, adm, wf["id"], 0)], ensure_ascii=False)
    check("không dòng nào chứa giá trị", all(v not in raw_runs for v in ("012345678", "Gõ tay", "Ba Đình", "Đà Nẵng", "Ghi chú viết tay")), raw_runs)


def pause_lead_routing(base, adm, cleanup):
    """Switch off, until finally, every active after_create `assign` rule on crm_leads (the CRM pack's
    "Định tuyến lead mới"): it hands a new lead to someone else, and test@example.com could no longer
    mass-edit the leads it just created. Restored with the exact body GET returned."""
    _, body = req(base, "GET", "/admin/workflows", adm)
    for row in (body.get("data") or {}).get("workflows") or []:
        if row.get("entityName") != LEADS or row.get("trigger") != "after_create" or not row.get("isActive"):
            continue
        _, got = req(base, "GET", f"/admin/workflows/{row['id']}", adm)
        wf = (got.get("data") or {}).get("workflow") or {}
        if not any(a.get("type") == "assign" for a in wf.get("actions") or []):
            continue
        put = {k: wf.get(k) for k in ("name", "entityName", "trigger", "condition", "actions")}
        code, _ = req(base, "PUT", f"/admin/workflows/{wf['id']}", adm, dict(put, isActive=False))
        check(f"tạm tắt quy tắc chia lead \"{wf.get('name')}\" ⇒ 200", code == 200, str(code))
        cleanup.append(("call", lambda body=dict(put, isActive=True), wid=wf["id"]: req(base, "PUT", f"/admin/workflows/{wid}", adm, body)))


def import_leads(base, token, text, run_automations=None):
    boundary = "nexus" + uuid.uuid4().hex
    body = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="lead.csv"\r\n'
            f"Content-Type: text/csv\r\n\r\n").encode() + text.encode() + (
        f'\r\n--{boundary}\r\nContent-Disposition: form-data; name="mapping"\r\n\r\n'
        + json.dumps({"full_name": "full_name", "email": "email"}) + "\r\n").encode()
    if run_automations is not None:
        body += (f'--{boundary}\r\nContent-Disposition: form-data; name="runAutomations"\r\n\r\n'
                 + ("true" if run_automations else "false") + "\r\n").encode()
    body += f"--{boundary}--\r\n".encode()
    code, data = raw(base, "POST", f"/{LEADS}/import", token, body, f"multipart/form-data; boundary={boundary}")
    try:
        return code, (json.loads(data) if data else {})
    except ValueError:
        return code, {}


def read_lead(base, token, rid):
    code, body = req(base, "GET", f"/{LEADS}/{rid}", token)
    return code, ((body.get("data") or {}).get("data")) or {}


def slice_s5(base, adm, staff, cleanup, _staff_user):
    tag = uuid.uuid4().hex[:8]

    print("\n[admin] cấu hình chuẩn hoá sai bị từ chối lúc lưu")
    base_rule = {"name": f"nt-s5-bad-{tag}", "entityName": LEADS, "trigger": "before_save", "isActive": False}
    for label, over, want in (
        ("phép lặp (trim, trim) ⇒ NORMALIZE_OPS_INVALID gắn Email",
         {"actions": [{"type": "normalize", "field": "email", "ops": ["trim", "trim"]}]}, ("NORMALIZE_OPS_INVALID", "email")),
        ("đích là lựa chọn (Nguồn) ⇒ NORMALIZE_FIELD_TYPE_INVALID gắn Nguồn",
         {"actions": [{"type": "normalize", "field": "source", "ops": ["lower"]}]}, ("NORMALIZE_FIELD_TYPE_INVALID", "source")),
        ("chuẩn hoá trên quy tắc sau khi tạo ⇒ WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER",
         {"trigger": "after_create", "actions": [{"type": "normalize", "field": "email", "ops": ["lower"]}]},
         ("WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER", None)),
    ):
        code, body = req(base, "POST", "/admin/workflows", adm, dict(base_rule, **over))
        if code == 201:
            cleanup.append(("workflow", body["data"]["workflow"]["id"]))
        details = body.get("details") or {}
        check(label, code == 422 and body.get("code") == want[0] and (want[1] is None or details.get("field") == want[1]),
              f"{code} {body.get('code', '')} {json.dumps(details, ensure_ascii=False)}")

    print("\n[admin] tạm tắt quy tắc chia lead (trả lại trong finally)")
    pause_lead_routing(base, adm, cleanup)

    print("\n[admin] bật 3 quy tắc trên lead: Email bỏ khoảng trắng + chữ thường; Họ tên bỏ khoảng trắng + gộp; Nguồn trống ⇒ Web")
    wfs = {}
    for key, actions in (
        ("email", [{"type": "normalize", "field": "email", "ops": ["trim", "lower"]}]),
        ("name", [{"type": "normalize", "field": "full_name", "ops": ["trim", "collapse_spaces"]}]),
        ("web", [{"type": "field_update", "field": "source", "value": "Web", "onlyIfEmpty": True}]),
    ):
        code, body = req(base, "POST", "/admin/workflows", adm,
                         {"name": f"nt-s5-{key}-{tag}", "entityName": LEADS, "trigger": "before_save", "isActive": True, "actions": actions})
        wf = (body.get("data") or {}).get("workflow") or {}
        check(f"quy tắc {key} ⇒ 201, đang bật", code == 201 and wf.get("isActive") is True, f"{code} {body.get('code', '')} {body.get('error', '')}")
        if code != 201:
            return
        cleanup.append(("workflow", wf["id"]))
        wfs[key] = wf
    _, body = req(base, "GET", f"/admin/workflows/{wfs['email']['id']}", adm)
    acts = (((body.get("data") or {}).get("workflow") or {}).get("actions")) or []
    check("GET giữ ops theo đúng thứ tự", len(acts) == 1 and acts[0].get("ops") == ["trim", "lower"], json.dumps(acts, ensure_ascii=False))

    def forget_leads():
        _, listed = req(base, "GET", f"/{LEADS}?q={tag}&limit=100", adm)
        for rec in listed.get("data") or []:
            req(base, "DELETE", f"/{LEADS}/{rec.get('id')}", adm)

    cleanup.append(("call", forget_leads))

    print("\n[test@example.com] (1) form tạo lead với chữ lộn xộn")
    code, body = req(base, "POST", f"/{LEADS}", staff, {"full_name": f"  Nguyễn   Văn  An  {tag} ", "email": f"  An.Nguyen.{tag}@EXAMPLE.COM "})
    form_id = (body.get("data") or {}).get("id")
    check("tạo lead ⇒ 201", code == 201 and bool(form_id), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if not form_id:
        return
    code, got = read_lead(base, staff, form_id)
    check("chi tiết: Họ tên \"Nguyễn Văn An …\", Email chữ thường không khoảng trắng",
          code == 200 and got.get("full_name") == f"Nguyễn Văn An {tag}" and got.get("email") == f"an.nguyen.{tag}@example.com",
          f"{code} {got.get('full_name')!r} {got.get('email')!r}")

    print("\n[test@example.com] (2a) tạo nhiều dòng (tất cả hoặc không), 3 lead email lộn xộn")
    batch = [{"full_name": f" Lô  {i} {tag}", "email": f" Lo{i}.{tag}@example.com "} for i in (1, 2, 3)]
    code, body = req(base, "POST", f"/{LEADS}/bulk?mode=atomic", staff, {"records": batch})
    data = body.get("data") or {}
    bulk_ids = [r.get("id") for r in data.get("results") or [] if r.get("status") == "created"]
    check("tạo nhiều dòng ⇒ tạo 3", data.get("created") == 3 and len(bulk_ids) == 3, f"{code} created={data.get('created')} {body.get('code', '')}")
    for i, rid in enumerate(bulk_ids, start=1):
        code, got = read_lead(base, staff, rid)
        check(f"lô dòng {i}: đã chuẩn hoá + Nguồn Web",
              got.get("full_name") == f"Lô {i} {tag}" and got.get("email") == f"lo{i}.{tag}@example.com" and got.get("source") == "Web",
              f"{code} {got.get('full_name')!r} {got.get('email')!r} {got.get('source')!r}")

    print("\n[test@example.com] (2b) nhập CSV 3 lead email lộn xộn")
    csv_text = "full_name,email\n" + "".join(f"Nhập   {i}  {tag}, Nhap{i}.{tag}@example.com \n" for i in (1, 2, 3))
    code, body = import_leads(base, staff, csv_text)
    data = body.get("data") or {}
    check("nhập CSV ⇒ nhập 3, lỗi 0", code == 200 and data.get("inserted") == 3 and data.get("failed") == 0,
          f"{code} {body.get('code', '')} inserted={data.get('inserted')} failed={data.get('failed')} {json.dumps(data.get('errors'), ensure_ascii=False)}")
    _, listed = req(base, "GET", f"/{LEADS}?q={tag}&limit=100", staff)
    imported = {(r.get("data") or {}).get("full_name"): (r.get("data") or {}).get("email")
                for r in listed.get("data") or [] if str((r.get("data") or {}).get("full_name")).startswith("Nhập")}
    want = {f"Nhập {i} {tag}": f"nhap{i}.{tag}@example.com" for i in (1, 2, 3)}
    check("danh sách: 3 lead nhập đã chuẩn hoá", imported == want, json.dumps(imported, ensure_ascii=False))

    print("\n[test@example.com] (3) sửa hàng loạt Email của 2 lead")
    pair = [form_id] + bulk_ids[:1]
    code, body = req(base, "POST", f"/{LEADS}/bulk-update", staff, {"ids": pair, "values": {"email": f"  Sales.{tag}@EXAMPLE.COM "}})
    data = body.get("data") or {}
    check("sửa hàng loạt ⇒ sửa 2", code == 200 and data.get("updated") == 2, f"{code} {body.get('code', '')} {json.dumps(data, ensure_ascii=False)}")
    for rid in pair:
        code, got = read_lead(base, staff, rid)
        check("lead sau sửa hàng loạt: Email chữ thường không khoảng trắng", got.get("email") == f"sales.{tag}@example.com", f"{code} {got.get('email')!r}")

    print("\n[admin] xoá lựa chọn Web khỏi Nguồn (trả lại trong finally)")
    _, listed = req(base, "GET", f"/metadata/fields/{LEADS}", adm)
    fields = listed.get("data", listed) if isinstance(listed, dict) else listed
    src = next((f for f in fields or [] if f.get("name") == "source"), None)
    opts = (src or {}).get("options") or {}
    if isinstance(opts, str):
        opts = json.loads(opts or "{}")
    if not src or "Web" not in (opts.get("values") or []):
        check("Nguồn của lead có lựa chọn Web", False, json.dumps(opts, ensure_ascii=False))
        return
    trimmed = dict(opts, values=[v for v in opts["values"] if v != "Web"])
    if isinstance(opts.get("valueLabels"), dict):
        trimmed["valueLabels"] = {k: v for k, v in opts["valueLabels"].items() if k != "Web"}
    code, body = req(base, "PUT", f"/admin/fields/{src['id']}", adm, {"options": trimmed})
    check("PUT Nguồn không còn Web ⇒ 200", code == 200, f"{code} {body.get('code', '')} {body.get('error', '')}")
    cleanup.append(("call", lambda: req(base, "PUT", f"/admin/fields/{src['id']}", adm, {"options": opts})))

    print("\n[test@example.com] (4) tạo lead không chọn Nguồn khi lựa chọn Web đã bị xoá")
    code, body = req(base, "POST", f"/{LEADS}", staff, {"full_name": f"Không nguồn {tag}", "email": f"khong.{tag}@example.com"})
    orphan = (body.get("data") or {}).get("id")
    check("vẫn lưu được ⇒ 201", code == 201 and bool(orphan), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if orphan:
        code, got = read_lead(base, staff, orphan)
        check("Nguồn để trống (quy tắc không ghi được Web)", code == 200 and not got.get("source"), f"{code} source={got.get('source')!r}")

    print("\n[admin] lịch sử chạy")
    runs = wait_runs(base, adm, wfs["email"]["id"], 5)
    summaries = sorted(json.dumps(r.get("actionsResult"), sort_keys=True) for r in runs if r.get("recordId") is None)
    check("Email: MỘT dòng tổng hợp cho lô và MỘT cho tệp nhập, mỗi dòng 3 bản ghi Thành công",
          summaries == [json.dumps({"applied": 3, "blocked": 0, "failed": 0}, sort_keys=True)] * 2
          and all(r.get("status") == "success" for r in runs if r.get("recordId") is None), json.dumps(summaries))
    check("Email: dòng của form và của sửa hàng loạt trỏ đúng bản ghi",
          {r.get("recordId") for r in runs if r.get("recordId")} >= set(pair), json.dumps([r.get("recordId") for r in runs]))
    runs = wait_runs(base, adm, wfs["web"]["id"], 1)
    failed = [r for r in runs if r.get("recordId") == orphan]
    check("Nguồn: lần tạo sau khi xoá Web ⇒ Lỗi RECORD_INVALID_PICKLIST",
          len(failed) == 1 and failed[0].get("status") == "failed" and failed[0].get("error") == "RECORD_INVALID_PICKLIST",
          json.dumps([(r.get("status"), r.get("error"), r.get("recordId")) for r in runs]))
    raw_runs = json.dumps([r.get("actionsResult") for wf in wfs.values() for r in wait_runs(base, adm, wf["id"], 0)], ensure_ascii=False)
    check("không dòng nào chứa giá trị (tên, email, thẻ nghiệm thu, Web)",
          all(v not in raw_runs for v in (tag, "northwind", "Nexus", "Nguyễn", '"Web"')), raw_runs)


def history_creates(base, token, rid):
    _, body = req(base, "GET", f"/audit-log/{LEADS}/{rid}", token)
    data = body.get("data")
    rows = data.get("rows", data.get("items", [])) if isinstance(data, dict) else (data or [])
    return [r for r in rows if r.get("action") == "create"]


def wait_creates(base, token, ids, want=1):
    got = {}
    for _ in range(40):  # the job runs off the request's path
        got = {rid: history_creates(base, token, rid) for rid in ids}
        if all(len(v) >= want for v in got.values()):
            break
        time.sleep(0.5)
    return got


def slice_s7a(base, adm, staff, cleanup, staff_user, db=None):
    import subprocess
    tag = uuid.uuid4().hex[:8]
    print("\n[test@example.com] nhập CSV 3 lead")
    csv_text = "full_name,email\n" + "".join(f"Nhập S7a {i} {tag},s7a{i}.{tag}@example.com\n" for i in (1, 2, 3))
    code, body = import_leads(base, staff, csv_text)
    data = body.get("data") or {}
    check("nhập CSV ⇒ nhập 3, lỗi 0", code == 200 and data.get("inserted") == 3 and data.get("failed") == 0,
          f"{code} {body.get('code', '')} {json.dumps(data, ensure_ascii=False)[:200]}")
    check("không có job lịch sử nào chưa xếp được", not data.get("auditJobsFailed"), str(data.get("auditJobsFailed")))
    _, listed = req(base, "GET", f"/{LEADS}?q={tag}&limit=100", staff)
    ids = [r.get("id") for r in listed.get("data") or [] if str((r.get("data") or {}).get("full_name")).startswith("Nhập S7a")]
    check("thấy 3 lead vừa nhập", len(ids) == 3, str(len(ids)))
    for rid in ids:
        cleanup.append(("lead", rid))
    got = wait_creates(base, staff, ids)
    for rid, rows in got.items():
        check(f"lead {rid[:8]}: đúng một dòng Tạo của test@example.com",
              len(rows) == 1 and rows[0].get("actorUserId") == staff_user.get("id"),
              json.dumps([(r.get("action"), r.get("actorUserId")) for r in rows]))
    if not db:
        check("chạy lại job (cần --db)", False, "thiếu --db")
        return
    print("\n[hạ tầng] đánh dấu job nhập về queued, chờ chạy lại")
    sql = ("UPDATE sys_jobs SET status='queued', attempts=0, run_at=NOW(), claimed_by=NULL, finished_at=NULL "
           "WHERE job_type='record_import_events' AND status='done' AND payload LIKE '%" + ids[0] + "%'")
    out = subprocess.run(["docker", "exec", "nexus-mysql", "mysql", "-uroot", db, "-N", "-e", sql + "; SELECT ROW_COUNT();"],
                         capture_output=True, text=True)
    check("job nhập đã được đưa về queued (đúng 1 dòng khớp)",
          out.returncode == 0 and out.stdout.strip().splitlines()[-1:] == ["1"], (out.stdout + out.stderr).strip()[:200])
    for _ in range(40):
        done = subprocess.run(["docker", "exec", "nexus-mysql", "mysql", "-uroot", db, "-N", "-e",
                               "SELECT CONCAT(status, ':', attempts) FROM sys_jobs WHERE job_type='record_import_events' AND payload LIKE '%" + ids[0] + "%'"],
                              capture_output=True, text=True).stdout.strip()
        if done == "done:1":
            break
        time.sleep(0.5)
    check("job chạy lại xong (done, attempts=1 — đã chạy lại thật)", done == "done:1", done)
    for rid in ids:
        rows = history_creates(base, staff, rid)
        check(f"lead {rid[:8]}: sau chạy lại vẫn đúng một dòng Tạo", len(rows) == 1, str(len(rows)))


def slice_s7b(base, adm, staff, cleanup, staff_user):
    """runAutomations: test@example.com imports 3 leads ticked -> the CRM rule "Định tuyến lead mới" fills
    assigned_to on all three; 3 more unticked -> no one is assigned. Both sets have one "Tạo" line."""
    tag = uuid.uuid4().hex[:8]
    _, body = req(base, "GET", "/admin/workflows", adm)
    routing = [w for w in (body.get("data") or {}).get("workflows") or []
               if w.get("entityName") == LEADS and w.get("trigger") == "after_create" and w.get("isActive")]
    check("có quy tắc chia lead đang bật (Định tuyến lead mới)", bool(routing), str(len(routing)))

    def run(label, ticked):
        print(f"\n[test@example.com] nhập CSV 3 lead {label}")
        csv_text = "full_name,email\n" + "".join(f"Nhập S7b {label} {i} {tag},s7b{label}{i}.{tag}@example.com\n" for i in (1, 2, 3))
        code, body = import_leads(base, staff, csv_text, run_automations=ticked)
        data = body.get("data") or {}
        check(f"[{label}] nhập CSV ⇒ nhập 3, lỗi 0", code == 200 and data.get("inserted") == 3 and data.get("failed") == 0,
              f"{code} {body.get('code', '')} {json.dumps(data, ensure_ascii=False)[:200]}")
        _, listed = req(base, "GET", f"/{LEADS}?q={tag}&limit=100", adm)
        ids = [r.get("id") for r in listed.get("data") or []
               if f"Nhập S7b {label} " in str((r.get("data") or {}).get("full_name"))]
        check(f"[{label}] thấy 3 lead vừa nhập", len(ids) == 3, str(len(ids)))
        for rid in ids:
            cleanup.append(("lead", rid))
        got = wait_creates(base, adm, ids)
        for rid, rows in got.items():
            check(f"[{label}] lead {rid[:8]}: đúng một dòng Tạo của test@example.com",
                  len(rows) == 1 and rows[0].get("actorUserId") == staff_user.get("id"),
                  json.dumps([(r.get("action"), r.get("actorUserId")) for r in rows]))
        return ids

    ids = run("tick", True)
    owners = {}
    for _ in range(40):  # the rule runs off the job's path
        owners = {rid: read_lead(base, adm, rid)[1].get("assigned_to") for rid in ids}
        if all(owners.values()):
            break
        time.sleep(0.5)
    check("tick ⇒ ô Phân công cho của cả 3 lead được điền", all(owners.values()), json.dumps(owners))
    ids = run("khong", False)
    time.sleep(3)
    owners = {rid: read_lead(base, adm, rid)[1].get("assigned_to") for rid in ids}
    check("không tick ⇒ không lead nào được chia việc", not any(owners.values()), json.dumps(owners))


QUOTES = "crm_quotes"
ACCEPT_MSG = "Báo giá chỉ được Chấp nhận qua quy trình duyệt"


def fixture_login(base, email):
    """A BPM fixture account's session through scripts/fixture-bpm.py (the password never leaves it)."""
    spec = importlib.util.spec_from_file_location("fixture_bpm", Path(__file__).with_name("fixture-bpm.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    try:
        return mod.login_as(base, email)
    except mod.EnvError as e:
        raise EnvError(f"{e} — chạy scripts/fixture-bpm.py --base {base} trước") from e


def dry_run(base, token, wf_id, body):
    code, resp = req(base, "POST", f"/admin/workflows/{wf_id}/test", token, body)
    test = (resp.get("data") or {}).get("test") or {}
    trace = test.get("trace") or []
    last = (trace[-1].get("detail") or {}) if trace else {}
    return code, resp, test, last


def slice_s6(base, adm, staff, cleanup, _staff_user):
    tag = uuid.uuid4().hex[:8]
    name = f"Nghiệm thu S6 {tag}"
    approver, approver_user = fixture_login(base, "duyet1@example.com")
    if approver_user.get("isAdmin"):
        raise EnvError("duyet1@example.com đang là admin — chạy lại scripts/fixture-bpm.py")

    print("\n[test@example.com] tạo cơ hội của mình: Báo giá, Xác suất 40, Ngày chốt trống")
    code, body = req(base, "POST", f"/{ENTITY}", staff, {"name": name, "stage": "Proposal", "probability": 40})
    opp = (body.get("data") or {}).get("id")
    check("tạo cơ hội ⇒ 201", code == 201 and bool(opp), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201 or not opp:
        return
    cleanup.append(("record", opp))
    _, _, version = read_opp(base, staff, opp)

    print("\n[admin] lưu (TẮT) quy tắc chặn S1b + quy tắc tự điền Xác suất")
    code, body = req(base, "POST", "/admin/workflows", adm, veto_rule(f"nt-s6-{tag}"))
    veto_wf = (body.get("data") or {}).get("workflow") or {}
    check("quy tắc chặn (tắt) ⇒ 201", code == 201 and veto_wf.get("isActive") is False, f"{code} {body.get('code', '')}")
    if code != 201:
        return
    cleanup.append(("workflow", veto_wf["id"]))
    fill = {"name": f"nt-s6-fill-{tag}", "entityName": ENTITY, "trigger": "before_save", "isActive": False,
            "condition": {"rules": [{"field": "stage", "op": "changedTo", "value": "Closed Won"}]},
            "actions": [{"type": "field_update", "field": "probability", "value": 100}]}
    code, body = req(base, "POST", "/admin/workflows", adm, fill)
    fill_wf = (body.get("data") or {}).get("workflow") or {}
    check("quy tắc tự điền (tắt) ⇒ 201", code == 201, f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201:
        return
    cleanup.append(("workflow", fill_wf["id"]))

    print("\n[admin] chạy thử quy tắc chặn trên cơ hội của test@example.com")
    code, resp, test, last = dry_run(base, adm, veto_wf["id"], {"recordId": opp, "changes": {"stage": "Closed Won"}})
    check("Giai đoạn = Thắng giả lập ⇒ 200, Sẽ chặn đúng câu, gắn Ngày chốt",
          code == 200 and test.get("matched") is True and last.get("wouldBlock") is True and last.get("message") == VETO_MSG
          and last.get("field") == "close_date" and test.get("op") == "update", f"{code} {resp.get('code', '')} {json.dumps(last, ensure_ascii=False)}")
    code, resp, test, last = dry_run(base, adm, veto_wf["id"], {"recordId": opp, "changes": {"stage": "Closed Won", "close_date": "2026-10-15"}})
    check("thêm Ngày chốt giả lập ⇒ không chặn", code == 200 and last.get("wouldBlock") is False, f"{code} {json.dumps(last, ensure_ascii=False)}")
    code, resp, test, last = dry_run(base, adm, veto_wf["id"], {"recordId": opp})
    check("không thay đổi gì ⇒ không chặn (vừa đổi thành cần một thay đổi)", code == 200 and last.get("wouldBlock") is False,
          f"{code} {json.dumps(last, ensure_ascii=False)}")
    code, resp, test, last = dry_run(base, adm, veto_wf["id"], {"op": "create", "changes": {"name": name, "stage": "Closed Won"}})
    trig = ((test.get("trace") or [{}])[0].get("matched"))
    check("tạo giả lập không bản ghi ⇒ quy tắc chỉ-khi-sửa không chạy", code == 200 and test.get("recordId") == ""
          and trig is False and last.get("wouldBlock") is False, f"{code} {resp.get('code', '')} trigger={trig} {json.dumps(last, ensure_ascii=False)}")
    code, resp, test, last = dry_run(base, adm, fill_wf["id"], {"recordId": opp, "changes": {"stage": "Closed Won"}})
    check("quy tắc tự điền ⇒ Xác suất 40 → 100, sẽ ghi", code == 200 and last.get("before") == 40 and last.get("after") == 100
          and last.get("wouldApply") is True, f"{code} {json.dumps(last, ensure_ascii=False)}")
    code, resp, _, _ = dry_run(base, adm, veto_wf["id"], {"recordId": opp, "changes": {f"khong_co_{tag}": 1}})
    check("thay đổi trường lạ ⇒ 422 WORKFLOW_TEST_CHANGES_INVALID gắn trường", code == 422
          and resp.get("code") == "WORKFLOW_TEST_CHANGES_INVALID" and (resp.get("details") or {}).get("field") == f"khong_co_{tag}",
          f"{code} {resp.get('code', '')} {json.dumps(resp.get('details'), ensure_ascii=False)}")
    code, resp, _, _ = dry_run(base, staff, veto_wf["id"], {"recordId": opp, "changes": {"stage": "Closed Won"}})
    check("test@example.com gọi chạy thử ⇒ 403", code == 403, f"{code} {resp.get('code', '')}")
    code, stored, after_version = read_opp(base, staff, opp)
    check("test@example.com tải lại: cơ hội KHÔNG đổi (Báo giá, Xác suất 40, cùng version)",
          code == 200 and stored.get("stage") == "Proposal" and stored.get("probability") == 40 and after_version == version,
          f"{code} stage={stored.get('stage')} probability={stored.get('probability')} version {version}→{after_version}")
    time.sleep(1)  # a run written off the request's path would have landed by now
    runs = wait_runs(base, adm, veto_wf["id"], 1) + wait_runs(base, adm, fill_wf["id"], 1)
    check("chạy thử không ghi dòng lịch sử chạy nào", runs == [], json.dumps([r.get("status") for r in runs]))

    print("\n[test@example.com] tạo báo giá của mình (Nháp)")
    code, body = req(base, "POST", f"/{QUOTES}", staff, {"name": name, "status": "Nháp"})
    quote = (body.get("data") or {}).get("id")
    check("tạo báo giá ⇒ 201", code == 201 and bool(quote), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201 or not quote:
        return
    cleanup.append(("quote", quote))

    # Created AFTER the records: finally runs in reverse, so the rule and the process go first.
    print("\n[admin] bật quy tắc chặn báo giá + quy trình duyệt tay một bước (duyet1@example.com)")
    rule = {"name": f"nt-s6-quote-{tag}", "entityName": QUOTES, "trigger": "before_save", "isActive": True,
            "condition": {"saveOn": "update", "rules": [{"field": "status", "op": "changedTo", "value": "Chấp nhận"}]},
            "actions": [{"type": "veto", "field": "status", "message": ACCEPT_MSG}]}
    code, body = req(base, "POST", "/admin/workflows", adm, rule)
    quote_wf = (body.get("data") or {}).get("workflow") or {}
    check("quy tắc chặn báo giá ⇒ 201, đang bật", code == 201 and quote_wf.get("isActive") is True,
          f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201:
        return
    cleanup.append(("workflow", quote_wf["id"]))
    process = {"name": f"nt-s6-duyet-{tag}", "entityName": QUOTES, "trigger": "manual", "isActive": True,
               "steps": [{"name": "Trưởng nhóm", "approverType": "user", "approverId": approver_user.get("id")}],
               "outcome": {"statusField": "status", "approvedValue": "Chấp nhận", "rejectedValue": "Từ chối"}}
    code, body = req(base, "POST", "/admin/approval-processes", adm, process)
    proc = (body.get("data") or {}).get("process") or {}
    check("quy trình duyệt ⇒ 201", code == 201 and bool(proc.get("id")), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201:
        return
    cleanup.append(("call", lambda: req(base, "DELETE", f"/admin/approval-processes/{proc['id']}", adm)))

    print("\n[test@example.com] (1) tự sửa báo giá sang Chấp nhận")
    code, body = req(base, "GET", f"/{QUOTES}/{quote}", staff)
    qversion = (body.get("data") or {}).get("version")
    code, body = req(base, "PUT", f"/{QUOTES}/{quote}", staff, {"status": "Chấp nhận", "version": qversion})
    details = body.get("details") or {}
    vs = details.get("violations") or []
    check("PUT kiểu form ⇒ 422 RECORD_HOOK_VETO đúng câu, gắn Trạng thái, không lộ ruleId",
          code == 422 and body.get("code") == "RECORD_HOOK_VETO" and body.get("error") == ACCEPT_MSG
          and details.get("field") == "status" and len(vs) == 1 and "ruleId" not in vs[0],
          f"{code} {body.get('code', '')} {json.dumps(details, ensure_ascii=False)}")
    code, body = req(base, "GET", f"/{QUOTES}/{quote}", staff)
    status = (((body.get("data") or {}).get("data")) or {}).get("status")
    check("tải lại: vẫn Nháp", code == 200 and status == "Nháp", f"{code} status={status}")

    print("\n[test@example.com] (2) Gửi duyệt ⇒ [duyet1@example.com] Duyệt")
    code, body = req(base, "POST", f"/{QUOTES}/{quote}/approvals/submit", staff)
    inst = (body.get("data") or {}).get("approval") or {}
    inst_id = inst.get("id")  # camelCase since DAILY-QUEUE #443
    check("Gửi duyệt ⇒ 201", code == 201 and bool(inst_id), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if code != 201 or not inst_id:
        return
    code, body = req(base, "GET", "/approvals/pending", approver)
    pending = json.dumps(body.get("data"), ensure_ascii=False)
    check("duyet1@example.com thấy yêu cầu trong hộp chờ duyệt", code == 200 and inst_id in pending, f"{code} {body.get('code', '')}")
    code, body = req(base, "POST", f"/approvals/{inst_id}/approve", approver, {"comment": "Đồng ý"})
    check("duyet1@example.com bấm Duyệt ⇒ 200", code == 200, f"{code} {body.get('code', '')} {body.get('error', '')}")
    code, body = req(base, "GET", f"/{QUOTES}/{quote}", staff)
    status = (((body.get("data") or {}).get("data")) or {}).get("status")
    check("test@example.com tải lại: báo giá Chấp nhận (quy tắc chặn không chặn kết quả duyệt)", code == 200 and status == "Chấp nhận",
          f"{code} status={status}")

    print("\n[admin] lịch sử chạy của quy tắc chặn báo giá")
    runs = wait_runs(base, adm, quote_wf["id"], 2)
    by_status = {r.get("status"): r for r in runs}
    skipped = by_status.get("skipped") or {}
    reasons = [a.get("skipped") for a in (skipped.get("actionsResult") or []) if isinstance(a, dict)]
    check("một dòng Đã chặn (sửa tay) + một dòng Bỏ qua lý do approval_outcome",
          len(runs) == 2 and "blocked" in by_status and reasons == ["approval_outcome"],
          json.dumps([(r.get("status"), r.get("actionsResult")) for r in runs], ensure_ascii=False))
    check("cả hai trỏ đúng báo giá", all(r.get("recordId") == quote for r in runs), json.dumps([r.get("recordId") for r in runs]))
    raw_runs = json.dumps([r.get("actionsResult") for r in runs], ensure_ascii=False)
    check("không chứa giá trị nào (Chấp nhận, Nháp, tên, thẻ nghiệm thu)",
          all(v not in raw_runs for v in ("Chấp nhận", "Nháp", tag, "Nghiệm thu")), raw_runs)


def slice_fe3(base, adm, staff, cleanup, _staff_user):
    """FE-before-save-hooks-3 (queue #187): the Studio's MutateActionEditor / NormalizeActionEditor emit
    exactly these bodies, and opening a stored rule and saving it back must change nothing (the old
    loader turned ops into "trim,lower" and sent {type} only => 422). Round-trip = GET the actions the
    server returned, PUT them back unchanged."""
    tag = uuid.uuid4().hex[:8]
    pause_lead_routing(base, adm, cleanup)
    rules = {}
    print("\n[admin] dựng quy tắc như Studio: chuẩn hoá Email (trim, lower) + tự điền Công ty từ mẫu Lead {{full_name}} khi trống")
    for key, actions in (
        ("norm", [{"type": "normalize", "field": "email", "ops": ["trim", "lower"]}]),
        ("tmpl", [{"type": "field_update", "field": "company", "value": "Lead {{full_name}}", "valueSource": "template", "onlyIfEmpty": True}]),
    ):
        code, body = req(base, "POST", "/admin/workflows", adm,
                         {"name": f"nt-fe3-{key}-{tag}", "entityName": LEADS, "trigger": "before_save", "isActive": True, "actions": actions})
        wf = (body.get("data") or {}).get("workflow") or {}
        check(f"quy tắc {key} ⇒ 201", code == 201, f"{code} {body.get('code', '')} {body.get('error', '')}")
        if code != 201:
            return
        cleanup.append(("workflow", wf["id"]))
        rules[key] = (wf["id"], actions)

    print("\n[admin] mở rồi Lưu (PUT nguyên actions đã đọc) ⇒ 200 và giữ nguyên ops / valueSource / onlyIfEmpty")
    for key, (wid, sent) in rules.items():
        _, got = req(base, "GET", f"/admin/workflows/{wid}", adm)
        wf = (got.get("data") or {}).get("workflow") or {}
        code, body = req(base, "PUT", f"/admin/workflows/{wid}", adm, {"name": wf.get("name"), "entityName": LEADS, "trigger": "before_save", "isActive": True,
                                                                     "condition": wf.get("condition"), "actions": wf.get("actions")})
        check(f"{key}: PUT lại ⇒ 200", code == 200, f"{code} {body.get('code', '')} {body.get('error', '')}")
        _, again = req(base, "GET", f"/admin/workflows/{wid}", adm)
        acts = (((again.get("data") or {}).get("workflow") or {}).get("actions")) or []
        check(f"{key}: actions sau khi lưu lại == lúc dựng", acts == sent, json.dumps(acts, ensure_ascii=False))

    def forget_leads():
        _, listed = req(base, "GET", f"/{LEADS}?q={tag}&limit=100", adm)
        for rec in listed.get("data") or []:
            req(base, "DELETE", f"/{LEADS}/{rec.get('id')}", adm)

    cleanup.append(("call", forget_leads))
    print("\n[test@example.com] lead có email lộn xộn, công ty trống")
    code, body = req(base, "POST", f"/{LEADS}", staff, {"full_name": f"Fe3 {tag}", "email": f"  Fe3.{tag}@EXAMPLE.COM "})
    rid = (body.get("data") or {}).get("id")
    check("tạo lead ⇒ 201", code == 201 and bool(rid), f"{code} {body.get('code', '')} {body.get('error', '')}")
    if not rid:
        return
    code, got = read_lead(base, staff, rid)
    check("Email đã chuẩn hoá, Công ty tự điền từ mẫu",
          got.get("email") == f"fe3.{tag}@example.com" and got.get("company") == f"Lead Fe3 {tag}",
          f"{code} {got.get('email')!r} {got.get('company')!r}")


SLICES = {"fe3": slice_fe3, "s1a": slice_s1a, "s1b": slice_s1b, "s1b-files": slice_s1b_files, "s2": slice_s2, "s3": slice_s3, "s4": slice_s4,
          "s4-review": slice_s4_review, "s5": slice_s5, "s6": slice_s6, "s7a": slice_s7a, "s7b": slice_s7b}


def main():
    ap = argparse.ArgumentParser(description="Nghiệm thu không-admin cho specs/048 before-save hooks.")
    ap.add_argument("--base", required=True, help="vd http://localhost:8160/api/v1 (server chạy trên DB bản sao)")
    ap.add_argument("--slice", required=True, choices=sorted(SLICES))
    ap.add_argument("--db", help="tên DB bản sao (chỉ s7a: chạy lại job bằng docker exec nexus-mysql)")
    args = ap.parse_args()
    base = args.base.rstrip("/")

    cleanup, adm = [], None
    try:
        adm, _ = login(base, *ADMIN)
        staff, user = login(base, *STAFF)
        if user.get("isAdmin"):
            raise EnvError(f"{STAFF[0]} đang là admin — nghiệm thu bằng admin tự chấm điểm cho mình")
        if args.slice == "s7a":
            slice_s7a(base, adm, staff, cleanup, user, args.db)
        else:
            SLICES[args.slice](base, adm, staff, cleanup, user)
    except EnvError as e:
        print(f"LỖI MÔI TRƯỜNG: {e}")
        return 2
    finally:
        for kind, rid in reversed(cleanup if adm else []):
            if kind == "call":
                rid()  # restores config the slice changed (idempotent)
                continue
            if kind == "workflow":
                code, _ = req(base, "DELETE", f"/admin/workflows/{rid}", adm)
            elif kind == "field":
                code, _ = req(base, "DELETE", f"/admin/fields/{rid}", adm)
            else:
                entity = {"contract": CONTRACTS, "lead": LEADS, "quote": QUOTES}.get(kind, ENTITY)
                code, _ = req(base, "DELETE", f"/{entity}/{rid}", adm)
            if code not in (200, 204, 404):
                print(f"  (dọn {kind} {rid} → {code})")

    print(f"\n{'ĐẠT' if not fails else 'LỆCH'} — slice {args.slice}: {len(fails)} kỳ vọng lệch")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
