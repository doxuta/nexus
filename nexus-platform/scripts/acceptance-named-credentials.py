#!/usr/bin/env python3
"""Kho khoá kết nối (specs/050-named-credentials) — kịch bản nghiệm thu người-thứ-ba, trên server THẬT.

Mỗi lát của specs/050 có một nhóm kiểm (`--slice`); lát nào land thì mở rộng script bằng nhóm của
mình (tasks.md T003 + "Nghiệm thu --slice Sx" của lát đó). Kịch bản từng lát bằng lời:
specs/050-named-credentials/quickstart.md.

Nhóm S0 (mặc định) đo KHOẢNG TRỐNG ĐẦU-CUỐI của feature, đúng như người dùng thật đi:
  1. một quản trị của khách (is_admin, KHÔNG operator — tài khoản tạm do script tạo) mở kho khoá,
     cất một khoá HMAC + 2 header bổ sung (`Authorization: Bearer …`, `X-Doi-Tac`) trỏ receiver;
  2. người dựng Nexus (admin@example.com) viết luật after_create crm_contacts gọi receiver bằng khoá đó
     (`{credentialId, path: "/orders"}`); kho chưa có hoặc engine chưa nhận dạng khoá ⇒ luật rơi về
     dạng DUY NHẤT trình dựng phát ra hôm nay: chỉ một URL (`{type, url}`);
  3. nhân viên test@example.com (vai crm_sales_rep, KHÔNG admin) lưu một liên hệ;
  4. receiver loopback phải nhận ĐÚNG MỘT POST tới /orders mang Authorization + X-Doi-Tac +
     X-Nexus-Signature (V1) + X-Nexus-Timestamp (lệch ≤ 5 s) + X-Nexus-Signature-V2 đúng công thức,
     và không API đọc nào (quy trình, kho khoá, lịch sử chạy, nhật ký kiểm toán
     `/audit-log?entityName=__named_credential__` mà mọi quản trị của tổ chức đọc được) lộ bí mật
     hay giá trị header bổ sung; nhật ký phải CÓ dòng của khoá vừa tạo, để phép đọc đó không trống.
Trên `main` trước S1a nó ĐỎ đúng khoảng trống (kho 404, POST đi ra không xác thực) — exit 1. Nó
chỉ xanh khi S1b + S2a + S2b cùng ở server.

An toàn khi chạy:
  - `--base` phải là loopback: receiver nghe 127.0.0.1 nên server phải chạy cùng máy, và script GHI
    (tài khoản tạm, luật, khoá, bản ghi) — không bao giờ trỏ vào môi trường người khác.
  - Mọi thứ script tạo ra mang dấu `nc-nghiemthu-<epoch>-<hex>` và bị xoá trong `finally`, kể cả
    khi nhận SIGTERM/SIGHUP (bài học audit #78 — tín hiệu mặc định bỏ qua `finally`). Đầu và cuối
    mỗi lần chạy có bước quét tự lành xoá thứ mang dấu đó cũ hơn 15 phút (lần chạy bị `kill -9`).
  - Không in bí mật: khoá, token, mật khẩu tài khoản tạm và giá trị header chỉ nằm trong RAM; output
    chỉ nói "khớp / không khớp / vắng" và TÊN header.
  - Dọn thất bại ⇒ exit 1 (không phải cảnh báo rồi thoát 0).

Nhóm S1a (lát nền mã hoá — chưa có route) không đăng nhập: nó tự khởi động/tắt một bản máy chủ RIÊNG
(`--binary`, cổng của `--base`, DB bản sao `--db nexus_wt_<slug>` — script từ chối mọi DB khác) ba
kiểu khoá chủ: vắng ⇒ lên + một dòng Warn; sai dạng / khoá chỉ-giải-mã trùng id ⇒ thoát, log nêu tên
biến; đúng ⇒ lên, kho bật. Log không bao giờ được chứa khoá. Cần `.env` đã nạp vào env
(`set -a && . ./.env && set +a`) và cổng đang trống.

Nhóm S2a (ký V2, chưa cần kho): luật có `secret` + header `Authorization` (từ S3a API không nhận khoá
trần nữa ⇒ luật cũ được cắm bằng một câu UPDATE trên `--db`); nhân viên lưu
một liên hệ ⇒ receiver thấy V1 giống từng byte công thức cũ + `X-Nexus-Timestamp` + V2 + header y
nguyên; lưu lại kiểu trình dựng cũ (`{type, url}`) ⇒ 409 `WEBHOOK_LEGACY_SECRET_PENDING`, luật không
đổi; luật thứ hai trỏ `http://user:pass@127.0.0.1:<cổng đóng>/bot<token>/send?token=…` ⇒ lỗi gửi được
ghi (lịch sử chạy, hoặc `sys_jobs.last_error` đọc trên `--db` khi hàng đợi bền bật) chỉ nêu
`scheme://host:port`.

Nhóm S2b (luật gọi đối tác bằng khoá trong kho): quản trị khách tạm cất một khoá header token, một khoá
HMAC tự sinh và một chỗ khoá chưa có bí mật; admin@example.com lưu bốn luật `{credentialId, path}` (luật chỉ
giữ id + path); luật trỏ khoá của tổ chức KHÁC (một dòng ghi thẳng vào `--db`) ⇒ 422
`WEBHOOK_CREDENTIAL_NOT_FOUND`; path `/%zz` (lúc gửi không parse được) ⇒ 422 `WEBHOOK_PATH_INVALID` ngay lúc
lưu; test@example.com lưu một liên hệ ⇒ đúng một POST `/orders` mang
`Authorization: Bearer <token>`, một POST `/signed` mang V1 + timestamp + V2 đúng công thức, chỗ khoá
⇒ 0 request + `CREDENTIAL_NOT_READY`, đối tác trả 302 ⇒ đích chuyển hướng nhận 0 request +
`WEBHOOK_REDIRECT_NOT_FOLLOWED`; đọc luật / lịch sử / chạy thử / `sys_jobs` không lộ token hay khoá;
chạy thử nêu tên khoá. Cần `--db` và server có `CREDENTIAL_ENC_KEY`.

Nhóm S3a (che khi đọc, cấm khoá trần mới): một luật "lưu trước S3a" — cắm bằng SQL trên `--db`: khoá
ký riêng + `Authorization: Bearer …` + URL mang userinfo `<tên ngẫu nhiên>:<mật khẩu>` và token query — đọc
qua `GET /admin/workflows[/:id]` không còn khoá, token, tên/mật khẩu userinfo hay giá trị query (tên tham
số query vẫn còn, giá trị là `••••`); lưu lại đúng bản vừa đọc ⇒ 422 `WEBHOOK_INLINE_SECRET_REMOVED`, kiểu trình dựng (`{type, url}`
đã che) ⇒ 422 `WEBHOOK_REDACTED_VALUE`, luật mới kèm `secret` ⇒ 422, và `sys_workflows.actions` không
đổi; luật dùng khoá có `path` mang token query cũng bị che — gửi lại ĐÚNG bản che dưới cùng khoá ⇒ 200 và
path thật được giữ (phản biện S3b), bản che khác path đang lưu ⇒ 422; SAU các lần bị từ
chối đó, test@example.com lưu một liên hệ ⇒ receiver vẫn nhận ĐÚNG khoá thật (Bearer, V1, token query của
URL cũ, token query của path) — đường gửi đọc bản gốc; chạy thử không lộ gì nhưng vẫn biết sẽ ký. Cần
`--db` và server có `CREDENTIAL_ENC_KEY` + `CREDENTIAL_ALLOW_INSECURE_HTTP=true`.

Nhóm S3b (Bác sĩ hệ thống C10 dời khoá trần vào kho): luật cũ cắm bằng SQL (khoá ký 24 ký tự +
`Authorization`); test@example.com lưu một liên hệ ⇒ ghi header đối tác nhận; admin@example.com quét
`/admin/doctor/scan` ⇒ C10 liệt kê luật (cần xác nhận, mô tả nói "một chiều" + "chuyển hướng", không lộ
khoá); sửa không xác nhận ⇒ 422; sửa có xác nhận ⇒ luật chỉ còn `{credentialId, path}`, kho có khoá HMAC
nhãn "(dời từ quy trình)" niêm phong; test@example.com lưu lần nữa ⇒ cùng đường dẫn, Authorization + V1 GIỐNG
TỪNG BYTE, thêm V2 + timestamp; sửa lần hai ⇒ 0 khoá mới; cuối cùng một bản máy chủ RIÊNG không
`CREDENTIAL_ENC_KEY` (`--binary`, `--nokey-port`, cùng `--db`) sửa C10 ⇒ `CREDENTIALS_NOT_CONFIGURED`, 0 dòng
đổi. Phản biện S3b thêm: một luật cũ ĐÃ XOÁ (xoá mềm) được liệt kê `deleted:<id>` và sau khi sửa không còn
khoá trong bản ghi (url chỉ còn địa chỉ gốc, không dời vào kho); lần giao của S3b.1 đổi thành `failed` trên DB
bản sao ⇒ finding báo trước "1 lần gửi đã lỗi … KHÔNG Gửi lại được", và sau khi dời "Gửi lại" nó ⇒ hỏng hẳn
"rule đã đổi", đối tác không nhận gì; trình dựng lưu lại luật đã dời (path đọc ra `/legacy?src=••••`) ⇒ 200,
path thật giữ nguyên. Cần `--db`, `.env` đã nạp, server ở `--base` có `CREDENTIAL_ENC_KEY` + `CREDENTIAL_ALLOW_INSECURE_HTTP=true`.

Nhóm S4 (xoay khoá có hiệu lực ngay, ân hạn HMAC): quản trị khách tạm cất khoá header token, khoá HMAC (kèm
header bổ sung) và một chỗ khoá chưa nhập; admin@example.com viết ba luật `{credentialId, path}`; xoay token ⇒
lần lưu KẾ TIẾP của test@example.com (< 30 s) mang token mới; xoay HMAC tự sinh với `graceHours=1` ⇒ receiver
thấy V1 + V2 bằng khoá mới và V2-Previous bằng khoá cũ, header bổ sung còn; chỗ khoá chưa nhập ⇒ 0 request +
`CREDENTIAL_NOT_READY`, nhập qua xoay ⇒ lần sau gửi có ký, không khoá cũ; PUT đổi host ⇒ 422, xoay đổi host
thiếu khoá mới ⇒ 422, kèm ân hạn ⇒ 422, kèm khoá mới ⇒ 200 và máy chủ mới (Host `localhost`) chỉ nhận khoá mới,
không header bổ sung cũ; test@example.com xoay ⇒ 403; nhật ký kiểm toán có dòng `rotate` và không lộ gì. Cần `--db`
và server có `CREDENTIAL_ENC_KEY` + `CREDENTIAL_ALLOW_INSECURE_HTTP=true` (như S2b).

Nhóm S5 (khoá nằm trong đường dẫn — Telegram bot / Slack): quản trị khách tạm cất khoá `url_token`
`http://127.0.0.1:<receiver>/bot{secret}`; base thiếu / thừa `{secret}` hoặc `{secret}` ở tên miền ⇒ 422
`CREDENTIAL_BASE_URL_INVALID`; admin@example.com viết luật `{credentialId, path: "/sendMessage"}`; test@example.com
lưu một liên hệ ⇒ receiver nhận ĐÚNG một POST `/bot<token>/sendMessage` (không Authorization, không chữ ký);
token có `/` `%` ⇒ đến nơi dạng đã mã hoá đoạn đường dẫn; chạy thử hiện `{secret}`; tắt receiver (cổng đóng)
⇒ `sys_jobs.last_error` / lịch sử chạy chỉ chứa `{secret}`, không chứa token; nhân viên không-admin tạo khoá
⇒ 403 (cặp với 201 của quản trị khách); đọc kho / nhật ký kiểm toán / lịch sử chạy không lộ token. Cần
`--db` và server có `CREDENTIAL_ENC_KEY` + `CREDENTIAL_ALLOW_INSECURE_HTTP=true` (như S2b).

Nhóm S7 (xoay khoá chủ, mất khoá, chặn ở deploy): server ở `--base` giữ khoá chủ A (env của script phải có
CÙNG `CREDENTIAL_ENC_KEY`); một khoá header token gửi đúng; rồi các bản máy chủ RIÊNG trên cùng `--db` (cổng
`--nokey-port`, lần lượt) đóng vai operator: B = khoá B chính + A trong RETIRED ⇒ nền niêm phong lại (key_id
sang mã B), nhân viên lưu vẫn ký đúng, Doctor C11 = 0; C = chỉ khoá B ⇒ vẫn đúng; D = khoá KHÁC cùng nhãn (DB
khôi phục sai môi trường) ⇒ C12 nêu tên khoá (critical, chỉ báo cáo), nhân viên lưu ⇒ đối tác 0 request,
`CREDENTIAL_KEY_UNAVAILABLE`. `deploy/nexus/scripts/admin-precheck.sh` THẬT chạy trên DB bản sao (docker shim):
exit 0 khi mọi mã niêm phong còn tạo lại được từ env, exit 1 khi không (kể cả nhãn v1 đặt trên khoá khác).
Cần `--db`, `--binary`, `.env` đã nạp, cổng `--nokey-port` trống, server ở `--base` có `CREDENTIAL_ENC_KEY` +
`CREDENTIAL_ALLOW_INSECURE_HTTP=true` + `JOBS_ENABLED=false` (để nó không giành các việc nền do server riêng xếp trên cùng DB).

Mật khẩu: `NEXUS_ADMIN_PASS` / `NEXUS_STAFF_PASS` (mặc định là mật khẩu demo của tenant dev `northwind`).
Mã thoát: 0 = đạt · 1 = chưa đạt (hoặc dọn chưa sạch) · 2 = lỗi môi trường / nhóm kiểm chưa viết.

Dùng (từ nexus-platform/):
    python3 scripts/acceptance-named-credentials.py                       # nhóm S0, :8090
    python3 scripts/acceptance-named-credentials.py --base http://localhost:8162/api/v1 --slice S0
    python3 scripts/acceptance-named-credentials.py --slice S1a --base http://localhost:8165/api/v1 \
        --db nexus_wt_named_credentials_crypto_store              # server riêng, 4 lần boot
    python3 scripts/acceptance-named-credentials.py --slice S2b --base http://localhost:8174/api/v1 \
        --db nexus_wt_named_credentials_webhook  # server riêng có CREDENTIAL_ENC_KEY + ALLOW_INSECURE_HTTP
    python3 scripts/acceptance-named-credentials.py --slice S3a --base http://localhost:8177/api/v1 \
        --db nexus_wt_named_credentials_redact   # như S2b
    python3 scripts/acceptance-named-credentials.py --slice S3b --base http://localhost:8180/api/v1 \
        --db nexus_wt_named_credentials_legacy_migrate --wait 40   # như S2b; :8181 trống cho bản không khoá chủ
    python3 scripts/acceptance-named-credentials.py --slice S4 --base http://localhost:8514/api/v1 \
        --db nexus_wt_named_credentials_rotate   # như S2b
    python3 scripts/acceptance-named-credentials.py --slice S5 --base http://localhost:8515/api/v1 \
        --db nexus_wt_named_credentials_url_token   # như S2b

    python3 scripts/acceptance-named-credentials.py --slice S7 --base http://localhost:8517/api/v1 \
        --db nexus_wt_named_credentials_master_key --nokey-port 8518   # như S2b; export cùng CREDENTIAL_ENC_KEY

ponytail: stdlib thuần (urllib/http.server/hmac) — tiền lệ acceptance-workflow-trail.py.
"""

import argparse
import base64
import datetime
import hashlib
import hmac
import http.server
import ipaddress
import json
import os
import re
import secrets
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ENTITY = "crm_contacts"
PATH = "/orders"
AUDIT_ENTITY = "__named_credential__"  # spec FR-012
MARK = "nc-nghiemthu"  # every row this script creates carries MARK-<epoch>-<hex>
MARK_RE = re.compile(r"\bnc-nghiemthu-(\d{10})-[0-9a-f]{8}\b")
STALE_AFTER = 15 * 60  # a leftover older than this cannot belong to a run still in progress
TS_SKEW = 5  # seconds (spec US2 scenario 2)
SLICES = ("S0", "S1a", "S1b", "S2a", "S2b", "S3a", "S3b", "S4", "S5", "S6", "S7", "S8a", "S8b", "S9")


# --------------------------------------------------------------------------------------------
# Pure helpers (pinned by scripts/test_acceptance_named_credentials.py).
# --------------------------------------------------------------------------------------------

def sig_v1(key, body):
    """X-Nexus-Signature: lowercase hex HMAC-SHA256(key, raw body) — today's formula, byte for byte."""
    return hmac.new(key.encode(), body, hashlib.sha256).hexdigest()


def sig_v2(key, ts, body):
    """X-Nexus-Signature-V2: lowercase hex HMAC-SHA256(key, ts + "." + raw body) (spec FR-020)."""
    return hmac.new(key.encode(), ts.encode() + b"." + body, hashlib.sha256).hexdigest()


def master_key_id(label, key_b64):
    """The id the keyring seals under (crypto.keyIDFor): "<label>.<first 8 hex of
    HMAC-SHA256(key, "nexus/crypto/keyring/key-id/v1")>" — a fingerprint of the key itself, so two
    environments that both keep the label v1 over different keys never share an id."""
    mac = hmac.new(base64.b64decode(key_b64), b"nexus/crypto/keyring/key-id/v1", hashlib.sha256)
    return label + "." + mac.hexdigest()[:8]


def is_loopback_base(base):
    """True only for http(s)://localhost | 127.0.0.0/8 | ::1 — the receiver listens on 127.0.0.1."""
    host = urllib.parse.urlsplit(base).hostname or ""
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def stale_mark(text, now):
    """True when text carries OUR marker minted more than STALE_AFTER seconds ago.

    This is the only thing that authorises the self-healing sweep to delete a user, rule or
    credential, so it matches the exact marker shape and nothing looser.
    """
    m = MARK_RE.search(text or "")
    return bool(m) and now - int(m.group(1)) > STALE_AFTER


def new_mark():
    return f"{MARK}-{int(time.time())}-{secrets.token_hex(4)}"


CLONE_DB_RE = re.compile(r"^nexus_wt_[a-z0-9_]+$")
DSN_DB_RE = re.compile(r"^([^@]*@tcp\([^)]*\)/)([^?]*)(\?.*)?$")


def clone_dsn(dsn, db):
    """The go-sql-driver DSN `dsn` repointed at database `db`, which MUST be a nexus_wt_* clone.

    The boot groups (S1a) start private servers that migrate nothing but still connect and write
    (sessions, janitor); they must never touch the shared nexus_platform database.
    """
    if not CLONE_DB_RE.match(db or ""):
        raise ValueError(f"--db {db!r} không phải DB bản sao nexus_wt_<slug>")
    m = DSN_DB_RE.match(dsn or "")
    if not m:
        raise ValueError("DATABASE_URL không đúng dạng user:pass@tcp(host:port)/db?params")
    return m.group(1) + db + (m.group(3) or "")


# --------------------------------------------------------------------------------------------
# HTTP + receiver.
# --------------------------------------------------------------------------------------------

class EnvError(Exception):
    """The environment cannot run the check (exit 2) — never a verdict on the feature."""


def call(base, method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, (json.loads(raw) if raw else {})
        except ValueError:
            return e.code, {}
    except (urllib.error.URLError, OSError) as e:
        raise EnvError(f"không gọi được {method} {base}{path} — {e}") from e


class Receiver:
    """Loopback partner endpoint: records every request, answers 200."""

    def __init__(self):
        self.hits = []
        self.lock = threading.Lock()
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def _record(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(n) if n else b""
                with outer.lock:
                    outer.hits.append({"method": self.command, "path": self.path,
                                       "headers": dict(self.headers.items()), "body": body,
                                       "at": time.time()})
                if self.path.startswith("/redirect"):  # S2b: a partner bouncing the call elsewhere
                    self.send_response(302)
                    self.send_header("Location", outer.base + "/stolen")
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b"{}")

            do_POST = do_PUT = do_GET = _record

            def log_message(self, *_):  # silence: headers may carry secrets
                pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.base = f"http://127.0.0.1:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def for_run(self, wf_id, record_id):
        out = []
        with self.lock:
            hits = list(self.hits)
        for h in hits:
            try:
                p = json.loads(h["body"] or b"{}")
            except ValueError:
                continue
            if p.get("workflowId") == wf_id and p.get("recordId") == record_id:
                out.append(h)
        return out

    def close(self):
        self.server.shutdown()
        self.server.server_close()


def header(hit, name):
    for k, v in hit["headers"].items():
        if k.lower() == name.lower():
            return v
    return None


# --------------------------------------------------------------------------------------------
# Run context: logins, temp customer admin, verdicts, cleanup.
# --------------------------------------------------------------------------------------------

class Run:
    def __init__(self, a):
        self.a = a
        self.fails = []
        self.cleanup = []  # (label, fn) run in reverse in finally
        self.admin = self.login(a.admin, os.environ.get("NEXUS_ADMIN_PASS", "Demo-Admin-2026!"))[0]
        self.staff, staff_user = self.login(a.staff, os.environ.get("NEXUS_STAFF_PASS", "Demo-Rep-2026!"))
        if staff_user.get("isAdmin"):  # an admin bypasses ACL: it would score the gate green
            raise EnvError(f"{a.staff} là admin — không dùng làm người thứ ba.")

    def call(self, method, path, token=None, body=None):
        return call(self.a.base, method, path, token, body)

    def login(self, email, password):
        st, body = self.call("POST", "/auth/login",
                             body={"tenant": self.a.tenant, "email": email, "password": password})
        data = body.get("data") or {}
        if st != 200 or not data.get("accessToken"):
            raise EnvError(f"đăng nhập {email} thất bại ({st} {body.get('code', '')}).")
        return data["accessToken"], data.get("user") or {}

    def check(self, label, cond, detail=""):
        print(("  PASS  " if cond else "  FAIL  ") + label + (f"   [{detail}]" if detail else ""))
        if not cond:
            self.fails.append(label)

    def on_cleanup(self, label, fn):
        self.cleanup.append((label, fn))

    def customer_admin(self, mark):
        """A throwaway is_admin account WITHOUT the platform-operator flag — the customer's admin.

        Registered for deletion BEFORE its login can fail; the password is random and never printed.
        """
        email, password = f"{mark}@acceptance.invalid", secrets.token_urlsafe(24)
        st, body = self.call("POST", "/admin/users", self.admin, {
            "email": email, "password": password, "isAdmin": True,
            "fullName": "Nghiệm thu kho khoá (xoá ngay sau khi chạy)",
        })
        user = (body.get("data") or {}).get("user") or {}
        if st != 201 or not user.get("id"):
            raise EnvError(f"không tạo được quản trị khách tạm ({st} {body.get('code', '')}).")
        uid = user["id"]
        self.on_cleanup(f"tài khoản tạm {uid}", lambda: self.call("DELETE", f"/admin/users/{uid}", self.admin)[0] == 204)
        token, _ = self.login(email, password)
        st, me = self.call("GET", "/auth/me", token)
        me = (me.get("data") or {}).get("user") or me.get("data") or {}
        if st != 200 or not me.get("isAdmin") or me.get("isPlatformOperator") is not False:
            raise EnvError(f"tài khoản tạm không phải quản trị-khách-không-operator ({st} "
                           f"isAdmin={me.get('isAdmin')} isPlatformOperator={me.get('isPlatformOperator')}).")
        return token

    def sweep(self):
        """Self-healing: delete OUR leftovers older than STALE_AFTER (a run killed with -9).

        Rules first (a rule references the credential), then credentials, then users. Returns
        False when something stale could not be removed.
        """
        now, ok = time.time(), True
        st, body = self.call("GET", "/admin/workflows", self.admin)
        for w in ((body.get("data") or {}).get("workflows") or []) if st == 200 else []:
            if stale_mark(w.get("name"), now):
                ok &= self.call("DELETE", f"/admin/workflows/{w['id']}", self.admin)[0] in (200, 204)
        st, body = self.call("GET", "/admin/credentials", self.admin)
        for c in ((body.get("data") or {}).get("credentials") or []) if st == 200 else []:
            if stale_mark(c.get("name"), now):
                ok &= self.call("DELETE", f"/admin/credentials/{c['id']}", self.admin)[0] in (200, 204)
        st, body = self.call("GET", "/admin/users", self.admin)
        for u in ((body.get("data") or {}).get("users") or []) if st == 200 else []:
            if stale_mark(u.get("email"), now) and (u.get("email") or "").endswith("@acceptance.invalid"):
                ok &= self.call("DELETE", f"/admin/users/{u['id']}", self.admin)[0] == 204
        return ok

    def run_cleanup(self):
        dirty = []
        for label, fn in reversed(self.cleanup):
            try:
                if not fn():
                    dirty.append(label)
            except EnvError:
                dirty.append(label)
        self.cleanup = []
        return dirty


# --------------------------------------------------------------------------------------------
# Slice groups.
# --------------------------------------------------------------------------------------------

def slice_s0(run, rx):
    """End-to-end gap: customer admin stores a key; an employee's save fires an authenticated call."""
    mark = new_mark()
    cadm = run.customer_admin(mark)

    st_adm, _ = run.call("GET", "/admin/credentials", cadm)
    run.check("S0.1 quản trị của khách (không operator) mở được kho khoá — GET /admin/credentials 200",
              st_adm == 200, f"HTTP {st_adm}")
    # A 403 alone proves nothing: without the route, /admin/credentials falls into the dynamic
    # record route /:entity/:id and the record ACL answers 403 to a non-admin (measured on main,
    # 2026-09-29). The wall only counts when the SAME path answers 200 to an admin.
    st, _ = run.call("GET", "/admin/credentials", run.staff)
    run.check("S0.2 nhân viên không-admin bị chặn khỏi kho khoá — 403 trên route có thật",
              st == 403 and st_adm == 200,
              f"HTTP {st}" + ("" if st_adm == 200 else f"; route chưa có (quản trị nhận {st_adm}) — 403 này "
                                                       f"của route động /:entity/:id, không phải cổng quản trị"))

    secret = secrets.token_urlsafe(32)  # 43 chars ≥ 32 (CREDENTIAL_SECRET_WEAK floor)
    bearer = secrets.token_urlsafe(24)
    partner = secrets.token_hex(8)
    st, body = run.call("POST", "/admin/credentials", cadm, {
        "name": mark, "label": "Đối tác nghiệm thu (xoá sau khi chạy)", "baseUrl": rx.base,
        "authType": "hmac", "secret": secret,
        "extraHeaders": {"Authorization": "Bearer " + bearer, "X-Doi-Tac": partner},
    })
    cred_id = ((body.get("data") or {}).get("credential") or {}).get("id")
    if cred_id:
        run.on_cleanup(f"khoá {cred_id}",
                       lambda: run.call("DELETE", f"/admin/credentials/{cred_id}", run.admin)[0] in (200, 204))
    run.check("S0.3 quản trị của khách cất khoá HMAC + 2 header bổ sung — 201",
              st == 201 and bool(cred_id), f"HTTP {st} {body.get('code', '')}".strip())

    rule = {"name": f"zz nghiệm thu kho khoá {mark}", "entityName": ENTITY, "trigger": "after_create"}
    wf_id, shape = None, "credentialId + path"
    if cred_id:
        st, body = run.call("POST", "/admin/workflows", run.admin,
                            dict(rule, actions=[{"type": "outbound_webhook", "credentialId": cred_id, "path": PATH}]))
        wf_id = ((body.get("data") or {}).get("workflow") or {}).get("id")
        if not wf_id:
            print(f"         luật dùng khoá bị từ chối: HTTP {st} {body.get('code', '')}")
    if not wf_id:
        shape = "chỉ URL — dạng duy nhất trình dựng phát ra hôm nay"
        st, body = run.call("POST", "/admin/workflows", run.admin,
                            dict(rule, actions=[{"type": "outbound_webhook", "url": rx.base + PATH}]))
        wf_id = ((body.get("data") or {}).get("workflow") or {}).get("id")
        if not wf_id:
            raise EnvError(f"không tạo được luật webhook dạng URL ({st} {body.get('code', '')}).")
    run.on_cleanup(f"luật {wf_id}", lambda: run.call("DELETE", f"/admin/workflows/{wf_id}", run.admin)[0] in (200, 204))
    run.check("S0.4 luật gọi ra tham chiếu khoá trong kho (credentialId + path, không bí mật trong luật)",
              shape == "credentialId + path", shape)

    st, body = run.call("POST", f"/{ENTITY}", run.staff, {"first_name": "Kiểm Thử", "last_name": mark})
    record_id = (body.get("data") or body).get("id")
    if record_id:
        run.on_cleanup(f"bản ghi {record_id}",
                       lambda: run.call("DELETE", f"/{ENTITY}/{record_id}", run.admin)[0] in (200, 204))
    run.check(f"S0.5 nhân viên test@example.com lưu {ENTITY} được (lưu bản ghi không bị chặn)",
              st in (200, 201) and bool(record_id), f"HTTP {st}")
    if not record_id:
        return

    deadline = time.time() + run.a.wait
    while time.time() < deadline and not rx.for_run(wf_id, record_id):
        time.sleep(0.1)
    time.sleep(1.0)  # settle: a duplicate send must be visible before we count
    hits = rx.for_run(wf_id, record_id)
    run.check(f"S0.6 đối tác nhận ĐÚNG MỘT POST cho bản ghi vừa lưu (≤ {run.a.wait:g} s)",
              len(hits) == 1 and hits[0]["method"] == "POST", f"{len(hits)} request")
    if hits:
        h, body = hits[0], hits[0]["body"]
        names = sorted(k for k in h["headers"] if k.lower() not in ("host", "content-length",
                                                                     "accept-encoding", "user-agent"))
        print(f"         đối tác thấy header: {', '.join(names)}")
        run.check(f"S0.7 tới đúng đường dẫn {PATH}", urllib.parse.urlsplit(h["path"]).path == PATH, h["path"])
        auth = header(h, "Authorization")
        run.check("S0.8 mang Authorization của khoá (header bổ sung đã mở niêm phong)",
                  auth == "Bearer " + bearer, "vắng" if auth is None else "khớp" if auth == "Bearer " + bearer else "sai giá trị")
        doi_tac = header(h, "X-Doi-Tac")
        run.check("S0.9 mang header bổ sung thứ hai X-Doi-Tac", doi_tac == partner,
                  "vắng" if doi_tac is None else "khớp" if doi_tac == partner else "sai giá trị")
        v1 = header(h, "X-Nexus-Signature")
        run.check("S0.10 X-Nexus-Signature (V1) = hex(HMAC-SHA256(khoá, body)) — công thức cũ",
                  v1 is not None and hmac.compare_digest(v1, sig_v1(secret, body)),
                  "vắng" if v1 is None else "khớp" if hmac.compare_digest(v1, sig_v1(secret, body)) else "sai")
        ts = header(h, "X-Nexus-Timestamp")
        fresh = ts is not None and ts.isdigit() and abs(h["at"] - int(ts)) <= TS_SKEW
        run.check(f"S0.11 X-Nexus-Timestamp (giây Unix) lệch ≤ {TS_SKEW} s", fresh,
                  "vắng" if ts is None else f"lệch {h['at'] - int(ts):.1f} s" if ts.isdigit() else "không phải số")
        v2 = header(h, "X-Nexus-Signature-V2")
        good_v2 = v2 is not None and ts is not None and hmac.compare_digest(v2, sig_v2(secret, ts, body))
        run.check('S0.12 X-Nexus-Signature-V2 = hex(HMAC-SHA256(khoá, ts + "." + body))', good_v2,
                  "vắng" if v2 is None else "khớp" if good_v2 else "sai")

    # The audit trail is tenant-wide and read by EVERY admin of the tenant (adminGated), so it is
    # read here as the customer admin. /audit-log/export.csv serves the same seven columns
    # (services/audit_export.go auditCSVHeader), so the JSON list covers what the file carries.
    st_audit, audit = run.call("GET", f"/audit-log?entityName={AUDIT_ENTITY}", cadm)
    blobs = [json.dumps(audit, ensure_ascii=False)]
    for tok, path in ((run.admin, f"/admin/workflows/{wf_id}"), (run.admin, "/admin/workflows"),
                      (run.admin, f"/admin/workflows/{wf_id}/runs"), (cadm, "/admin/credentials"),
                      (cadm, f"/admin/credentials/{cred_id}" if cred_id else None)):
        if path:
            blobs.append(json.dumps(run.call("GET", path, tok)[1], ensure_ascii=False))
    leaked = [n for n, v in (("khoá HMAC", secret), ("token Bearer", bearer), ("giá trị X-Doi-Tac", partner))
              if any(v in b for b in blobs)]
    # Without a stored key there is nothing that COULD leak — a clean read then proves nothing.
    run.check("S0.13 đọc quy trình / lịch sử chạy / kho khoá / nhật ký kiểm toán không lộ bí mật nào",
              bool(cred_id) and not leaked,
              ", ".join(leaked) or ("sạch" if cred_id else "chưa có khoá trong kho — không có gì để chứng minh"))
    # Same vacuity trap for the trail: no row for this key ⇒ S0.13 read nothing there. Either
    # recordId convention (emitMetaAudit "" vs emitMetaAuditFor id) is accepted; the row must
    # name the key by id or by its machine name.
    ours = [r for r in ((audit.get("data") or []) if st_audit == 200 else [])
            if cred_id and (r.get("recordId") == cred_id or mark in json.dumps(r, ensure_ascii=False))]
    run.check(f"S0.14 nhật ký kiểm toán có dòng {AUDIT_ENTITY} của khoá vừa tạo (S0.13 đọc dòng có thật)",
              bool(ours), f"HTTP {st_audit}, {len(ours)} dòng" if cred_id else "chưa có khoá trong kho")


BOOT_WAIT = 30.0  # seconds for a private server to come up (or die) — DB connect + wiring


def boot(a, extra_env, label, port=None, probe=None):
    """Start ONE private server (a.binary) on `port` (default: --base's port) against the --db clone
    with the named-credential env replaced by extra_env; wait until it is healthy or has exited,
    call probe(base) while it is up (S1b: the inert vault's 503), then stop it.

    Returns (healthy, exit_code_or_None, log_text). The child gets its own process group and is
    killed as a group in `finally` (a killed shim must not orphan the server).
    """
    port = port or urllib.parse.urlsplit(a.base).port
    with socket.socket() as s:
        if s.connect_ex(("127.0.0.1", port)) == 0:
            raise EnvError(f"cổng {port} đang có tiến trình khác nghe — tắt nó trước (nohup giấu lỗi bind).")
    env = {k: v for k, v in os.environ.items() if not k.startswith("CREDENTIAL_")}
    env.update(extra_env, DATABASE_URL=a.clone_dsn, PORT=str(port))
    fd, log_path = tempfile.mkstemp(prefix="nexus-s1a-", suffix=".log")
    proc = None
    try:
        with os.fdopen(fd, "wb") as log:
            proc = subprocess.Popen([a.binary], env=env, stdout=log, stderr=subprocess.STDOUT,
                                    start_new_session=True)
        healthy, deadline = False, time.time() + BOOT_WAIT
        while time.time() < deadline and proc.poll() is None:
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{port}/health/live", timeout=1) as r:
                    healthy = r.status == 200
            except (urllib.error.URLError, OSError):
                pass
            if healthy:
                break
            time.sleep(0.2)
        # The 200 must come from OUR child (the port was free before it started).
        healthy = healthy and proc.poll() is None
        if healthy and probe:
            probe(f"http://127.0.0.1:{port}/api/v1")
        code = proc.poll()
    finally:
        if proc and proc.poll() is None:
            os.killpg(proc.pid, signal.SIGTERM)
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait()
        with open(log_path, encoding="utf-8", errors="replace") as f:
            text = f.read()
        os.unlink(log_path)
    print(f"         [{label}] {'lên (health 200)' if healthy else f'thoát mã {code}' if code is not None else 'không lên trong hạn'}")
    return healthy, code, text


def slice_s1a(a):
    """Master-key boot matrix on a private server + DB clone (quickstart §2, tasks T017).

    Absent key ⇒ the server boots, vault inert, one Warn line. Malformed ⇒ the process stops and
    its log names the variable. Valid ⇒ boots, vault enabled. No key material ever reaches the log.
    Returns the failed labels.
    """
    fails = []

    def check(label, cond, detail=""):
        print(("  PASS  " if cond else "  FAIL  ") + label + (f"   [{detail}]" if detail else ""))
        if not cond:
            fails.append(label)

    key = base64.b64encode(secrets.token_bytes(32)).decode()
    other = base64.b64encode(secrets.token_bytes(32)).decode()

    up, _, log = boot(a, {}, "vắng CREDENTIAL_ENC_KEY")
    check("S1a.1 vắng khoá chủ ⇒ server vẫn lên (kho nằm im, không sập)", up)
    warn = [ln for ln in log.splitlines() if "Named-credential vault disabled" in ln]
    check("S1a.2 … và log đúng một dòng Warn 'Named-credential vault disabled'",
          len(warn) == 1 and '"level":"warn"' in warn[0], f"{len(warn)} dòng")

    up, code, log = boot(a, {"CREDENTIAL_ENC_KEY": "abc"}, "CREDENTIAL_ENC_KEY=abc")
    check("S1a.3 khoá chủ sai dạng ⇒ tiến trình thoát ngay, không phục vụ", not up and code not in (None, 0),
          f"mã thoát {code}")
    check("S1a.4 … và log nêu đúng tên biến CREDENTIAL_ENC_KEY",
          "Failed to load named-credential config" in log and "CREDENTIAL_ENC_KEY" in log)

    up, code, log = boot(a, {"CREDENTIAL_ENC_KEY": key, "CREDENTIAL_ENC_RETIRED_KEYS": "v1:" + other},
                         "RETIRED trùng id khoá chính")
    check("S1a.5 khoá chỉ-giải-mã trùng id khoá chính ⇒ không lên", not up and code not in (None, 0), f"mã thoát {code}")
    check("S1a.6 … log nêu CREDENTIAL_ENC_RETIRED_KEYS, không lộ khoá",
          "CREDENTIAL_ENC_RETIRED_KEYS" in log and key not in log and other not in log)

    up, _, log = boot(a, {"CREDENTIAL_ENC_KEY": key}, "khoá chủ đúng")
    kid = master_key_id("v1", key)
    check(f"S1a.7 khoá chủ đúng ⇒ server lên, kho bật với mã khoá {kid} (nhãn v1 + dấu vân tay khoá)",
          up and "Named-credential vault enabled" in log and f'"credential_key_id":"{kid}"' in log)
    check("S1a.8 log khởi động không chứa khoá chủ", key not in log)
    return fails


UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")


def clone_sql(a, query, *ids):
    """Run one statement on the --db CLONE through `docker exec <container> mysql` and return the rows
    (tab-separated). The CLI has no placeholders, so the only values ever spliced in are UUIDs this
    script got from the API, each checked against UUID_RE first; the database is the checked
    nexus_wt_* clone. Never pointed at nexus_platform.
    """
    if not CLONE_DB_RE.match(a.db or ""):
        raise EnvError(f"--db {a.db!r} không phải DB bản sao nexus_wt_<slug> — nhóm S1b đọc DB bản sao")
    for v in ids:
        if not UUID_RE.match(v or ""):
            raise EnvError(f"id {v!r} không phải UUID — không ghép vào câu SQL")
    # utf8mb4 on the wire: run errors are Vietnamese, and the client's default charset would hand
    # back bytes that are not UTF-8.
    out = subprocess.run(["docker", "exec", a.mysql_container, "mysql", "-uroot", "--default-character-set=utf8mb4",
                          "-N", "-B", "-e", query % ids, a.db],
                         capture_output=True, text=True, timeout=30)
    if out.returncode != 0:
        raise EnvError(f"mysql trên {a.db} lỗi: {out.stderr.strip()[:200]}")
    return [ln.split("\t") for ln in out.stdout.splitlines() if ln]


SAFE_JSON_RE = re.compile(r'^[A-Za-z0-9 _:/@.?=&,{}\[\]"-]+$')
MASK = "••••"  # workflow.RedactedMarker


def legacy_actions_sql(tenant_id, wf_id, actions):
    """The UPDATE that gives one rule `actions` as a rule saved before S3a holds them (an inline key no
    API write accepts any more). The mysql CLI has no placeholders, so only a UUID pair and JSON drawn
    from a fixed safe alphabet — no quote, backslash or percent — are ever spliced in."""
    for v in (tenant_id, wf_id):
        if not UUID_RE.match(v or ""):
            raise ValueError(f"id {v!r} không phải UUID — không ghép vào câu SQL")
    raw = json.dumps(actions, separators=(",", ":"), ensure_ascii=True)
    if not SAFE_JSON_RE.match(raw):
        raise ValueError("actions có ký tự ngoài bảng an toàn — không ghép vào câu SQL")
    return (f"UPDATE sys_workflows SET actions = '{raw}' "
            f"WHERE tenant_id = '{tenant_id}' AND id = '{wf_id}' AND deleted_at IS NULL")


def stored_actions(a, tenant_id, wf_id):
    """sys_workflows.actions of one rule as the engine reads it (the --db clone), parsed."""
    rows = clone_sql(a, "SELECT CAST(actions AS CHAR) FROM sys_workflows WHERE tenant_id = '%s' AND id = '%s'",
                     tenant_id, wf_id)
    return json.loads(rows[0][0]) if rows else None


def legacy_rule(run, rule, actions):
    """A rule as saved before S3a: created through the API without its keys, then its actions replaced
    on the --db clone by `actions` (inline key and all). No event fires in between, so the engine's
    rule cache (invalidated by the create) loads the planted row. Returns (rule id, tenant id)."""
    a = run.a
    if not CLONE_DB_RE.match(a.db or ""):
        raise EnvError("luật mang khoá trần chỉ còn tạo được bằng SQL trên DB bản sao — cần --db nexus_wt_<slug>")
    plain = [{k: v for k, v in act.items() if k not in ("secret", "headers")} for act in actions]
    st, body = run.call("POST", "/admin/workflows", run.admin, dict(rule, actions=plain))
    wf = (body.get("data") or {}).get("workflow") or {}
    if not wf.get("id"):
        raise EnvError(f"không tạo được luật nền ({st} {body.get('code', '')}).")
    wf_id, tenant_id = wf["id"], wf.get("tenantId") or ""
    run.on_cleanup(f"luật {wf_id}", lambda: run.call("DELETE", f"/admin/workflows/{wf_id}", run.admin)[0] in (200, 204, 404))
    try:
        clone_sql(a, legacy_actions_sql(tenant_id, wf_id, actions))
    except ValueError as e:
        raise EnvError(str(e)) from e
    if stored_actions(a, tenant_id, wf_id) != actions:
        raise EnvError(f"không cắm được luật cũ lên {a.db}")
    return wf_id, tenant_id


def slice_s1b(run, rx):
    """Vault API as the customer admin (tasks T027, quickstart §4 S1b): store, read back nothing
    secret, sealed at rest, the admin wall in PAIRS, in-use 409 then shred, audit clean, inert 503."""
    a = run.a
    if not CLONE_DB_RE.match(a.db or ""):
        raise EnvError("nhóm S1b cần --db nexus_wt_<slug> — DB bản sao của server đang chạy ở --base")
    mark = new_mark()
    cadm = run.customer_admin(mark)
    token, partner = secrets.token_urlsafe(24), secrets.token_hex(8)

    st, body = run.call("POST", "/admin/credentials", cadm, {
        "name": mark, "label": "Nghiệm thu kho khoá (xoá sau khi chạy)", "baseUrl": rx.base,
        "authType": "header", "secret": token, "extraHeaders": {"X-Doi-Tac": partner},
    })
    data = body.get("data") or {}
    cred_id = (data.get("credential") or {}).get("id")
    if cred_id:
        run.on_cleanup(f"khoá {cred_id}",
                       lambda: run.call("DELETE", f"/admin/credentials/{cred_id}", run.admin)[0] in (204, 404))
    run.check("S1b.1 quản trị của khách (không operator) tạo khoá header token — 201",
              st == 201 and bool(cred_id), f"HTTP {st} {body.get('code', '')}".strip())
    if not cred_id:
        return
    run.check("S1b.2 token tự nhập KHÔNG được trả lại trong phản hồi tạo",
              "secret" not in data and token not in json.dumps(body))

    st_l, lst = run.call("GET", "/admin/credentials", cadm)
    st_g, one = run.call("GET", f"/admin/credentials/{cred_id}", cadm)
    blob = json.dumps([lst, one])
    leaks = [n for n, v in (("token", token), ("giá trị X-Doi-Tac", partner), ("envelope v1:", "v1:"),
                            ("trường secret", '"secret"'), ("secretEnc", "secretEnc"), ("keyId", "keyId")) if v in blob]
    run.check("S1b.3 danh sách + chi tiết 200, không bí mật / envelope / mã khoá chủ",
              st_l == 200 and st_g == 200 and not leaks, ", ".join(leaks) or f"HTTP {st_l}/{st_g}")
    view = (one.get("data") or {}).get("credential") or {}
    run.check("S1b.4 chi tiết vẫn đủ để nhận ra khoá: trạng thái ready + TÊN header bổ sung",
              view.get("status") == "ready" and view.get("extraHeaderNames") == ["X-Doi-Tac"],
              f"status={view.get('status')} extraHeaderNames={view.get('extraHeaderNames')}")

    rows = clone_sql(a, "SELECT secret_enc, key_id FROM sys_named_credentials WHERE id = '%s'", cred_id)
    env, kid = (rows[0] + ["", ""])[:2] if rows else ("", "")
    want_kid = master_key_id("v1", os.environ["CREDENTIAL_ENC_KEY"]) if os.environ.get("CREDENTIAL_ENC_KEY") else None
    sealed = (bool(re.match(r"^v1:[a-z0-9]{1,23}\.[0-9a-f]{8}:", env)) and env.split(":")[1] == kid
              and (want_kid is None or kid == want_kid) and token not in env)
    run.check("S1b.5 trong DB bản sao secret_enc = v1:<mã khoá chủ>:… (khớp key_id), không chứa token",
              sealed, f"key_id={kid}" + ("" if want_kid is None else f", mong {want_kid}"))

    st, body = run.call("POST", "/admin/credentials", cadm, {
        "name": mark + "-hmac", "label": "Nghiệm thu HMAC", "baseUrl": rx.base, "authType": "hmac", "generateSecret": True,
    })
    gen = (body.get("data") or {}).get("secret") or ""
    hmac_id = ((body.get("data") or {}).get("credential") or {}).get("id")
    if hmac_id:
        run.on_cleanup(f"khoá {hmac_id}",
                       lambda: run.call("DELETE", f"/admin/credentials/{hmac_id}", run.admin)[0] in (204, 404))
    _, again = run.call("GET", f"/admin/credentials/{hmac_id}", cadm) if hmac_id else (0, {})
    run.check("S1b.6 HMAC tự sinh: khoá nxw_… có trong ĐÚNG phản hồi tạo, GET sau không còn",
              st == 201 and gen.startswith("nxw_") and len(gen) == 47 and gen not in json.dumps(again)
              and '"secret"' not in json.dumps(again), f"HTTP {st}")

    for method, body in (("GET", None), ("POST", {"name": mark + "-staff", "label": "x", "baseUrl": rx.base, "authType": "hmac"})):
        st_staff, _ = run.call(method, "/admin/credentials", run.staff, body)
        # The pair (S0 finding 1): 403 counts only when the SAME path is the real vault route for the
        # customer admin — measured above (S1b.1 POST 201, S1b.3 GET 200).
        admin_ok = st_l == 200 if method == "GET" else bool(cred_id)
        run.check(f"S1b.7 nhân viên test@example.com {method} /admin/credentials ⇒ 403 trong khi quản trị khách được",
                  st_staff == 403 and admin_ok, f"nhân viên HTTP {st_staff}")

    # In use: the workflow API learns credentialId only in S2b, so the reference is written straight
    # into the CLONE, on a DISABLED rule (it never fires; disabled rules still count — spec edge case).
    st, body = run.call("POST", "/admin/workflows", run.admin, {
        "name": f"zz nghiệm thu kho khoá {mark}", "entityName": ENTITY, "trigger": "after_create", "isActive": False,
        "actions": [{"type": "outbound_webhook", "url": rx.base + "/never"}],
    })
    wf_id = ((body.get("data") or {}).get("workflow") or {}).get("id")
    if not wf_id:
        raise EnvError(f"không tạo được luật để kiểm 409 ({st} {body.get('code', '')}).")
    run.on_cleanup(f"luật {wf_id}",
                   lambda: run.call("DELETE", f"/admin/workflows/{wf_id}", run.admin)[0] in (200, 204, 404))
    clone_sql(a, "UPDATE sys_workflows SET actions = JSON_ARRAY(JSON_OBJECT('type','outbound_webhook',"
                 "'credentialId','%s','path','/orders')) WHERE id = '%s'", cred_id, wf_id)
    st, body = run.call("DELETE", f"/admin/credentials/{cred_id}", cadm)
    usages = (body.get("details") or {}).get("usages") or []
    run.check("S1b.8 xoá khoá đang được luật dùng ⇒ 409 CREDENTIAL_IN_USE kèm luật đó",
              st == 409 and body.get("code") == "CREDENTIAL_IN_USE" and any(u.get("workflowId") == wf_id for u in usages),
              f"HTTP {st} {body.get('code', '')}, {len(usages)} luật")
    run.call("DELETE", f"/admin/workflows/{wf_id}", run.admin)
    st, _ = run.call("DELETE", f"/admin/credentials/{cred_id}", cadm)
    rows = clone_sql(a, "SELECT deleted_at IS NOT NULL, secret_enc IS NULL, extra_headers_enc IS NULL, key_id IS NULL "
                        "FROM sys_named_credentials WHERE id = '%s'", cred_id)
    run.check("S1b.9 gỡ luật rồi xoá ⇒ 204, dòng xoá mềm + huỷ mọi envelope (secret, header bổ sung, key_id)",
              st == 204 and rows == [["1", "1", "1", "1"]], f"HTTP {st} {rows}")

    st, audit = run.call("GET", f"/audit-log?entityName={AUDIT_ENTITY}", cadm)
    trail = [r for r in ((audit.get("data") or []) if st == 200 else []) if r.get("recordId") in (cred_id, hmac_id)]
    blob = json.dumps(trail, ensure_ascii=False)
    actions = sorted({r.get("action") for r in trail if r.get("recordId") == cred_id})
    leaks = [n for n, v in (("token", token), ("khoá tự sinh", gen), ("giá trị X-Doi-Tac", partner), ("envelope v1:", "v1:"))
             if v and v in blob]
    run.check(f"S1b.10 quản trị khách đọc nhật ký {AUDIT_ENTITY}: có dòng tạo + xoá, không bí mật / giá trị header",
              "create" in actions and "delete" in actions and not leaks,
              ", ".join(leaks) or f"HTTP {st}, hành động {actions}")

    # Inert vault: a private server on the NEXT port, same clone, no CREDENTIAL_ENC_KEY.
    seen = {}

    def probe(base):
        st, body = call(base, "POST", "/auth/login", body={"tenant": a.tenant, "email": a.admin,
                                                           "password": os.environ.get("NEXUS_ADMIN_PASS", "Demo-Admin-2026!")})
        tok = (body.get("data") or {}).get("accessToken")
        seen["get"] = call(base, "GET", "/admin/credentials", tok) if tok else (st, body)
        seen["post"] = call(base, "POST", "/admin/credentials", tok, {"name": mark + "-inert", "label": "x",
                                                                     "baseUrl": rx.base, "authType": "hmac"}) if tok else (st, body)

    if not os.path.isfile(a.binary):
        raise EnvError(f"không thấy {a.binary} — cần để dựng server không khoá chủ (S1b.11).")
    try:
        a.clone_dsn = clone_dsn(os.environ.get("DATABASE_URL", ""), a.db)
    except ValueError as e:
        raise EnvError(str(e)) from e
    up, _, _ = boot(a, {}, "server riêng KHÔNG CREDENTIAL_ENC_KEY", port=urllib.parse.urlsplit(a.base).port + 1, probe=probe)
    g, p = seen.get("get", (0, {})), seen.get("post", (0, {}))
    run.check("S1b.11 server không khoá chủ vẫn lên; GET + POST /admin/credentials ⇒ 503 CREDENTIALS_NOT_CONFIGURED",
              up and all(x[0] == 503 and x[1].get("code") == "CREDENTIALS_NOT_CONFIGURED" for x in (g, p)),
              f"lên={up} GET {g[0]} POST {p[0]}")


def closed_port():
    """A loopback port nothing listens on (bound, then released) — a delivery there is refused."""
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def slice_s2a(run, rx):
    """Signing V2 on the wire for a key set on the rule by API (tasks T035, quickstart §4 S2a): V1
    unchanged byte for byte, a fresh timestamp, V2 over it, the rule's own header intact; the old
    builder's {type,url} save refused 409 with the rule untouched; a failed delivery recorded with
    the origin only — never the userinfo, path or query of the target."""
    a = run.a
    mark = new_mark()
    secret, bearer = secrets.token_urlsafe(32), secrets.token_urlsafe(24)
    rule = {"name": f"zz nghiệm thu S2a {mark}", "entityName": ENTITY, "trigger": "after_create"}
    legacy_url = rx.base + "/legacy"
    # Since S3a no API write accepts a key on the rule: the rule is planted as saved before S3a.
    wf_id, _ = legacy_rule(run, rule, [{"type": "outbound_webhook", "url": legacy_url, "secret": secret,
                                        "headers": {"Authorization": "Bearer " + bearer}}])

    # A second rule whose target carries a credential in every place a URL can — userinfo, a
    # Telegram-style path token, a query token — aimed at a port nothing listens on.
    pw, path_tok, q_tok = secrets.token_hex(8), secrets.token_hex(8), secrets.token_hex(8)
    origin = f"http://127.0.0.1:{closed_port()}"
    bad_url = origin.replace("http://", f"http://nguoidung:{pw}@") + f"/bot{path_tok}/send?token={q_tok}"
    st, body = run.call("POST", "/admin/workflows", run.admin, dict(rule, name=f"zz nghiệm thu S2a lỗi {mark}",
                                                                     actions=[{"type": "outbound_webhook", "url": bad_url}]))
    bad_id = ((body.get("data") or {}).get("workflow") or {}).get("id")
    if not bad_id:
        raise EnvError(f"không tạo được luật trỏ cổng đóng ({st} {body.get('code', '')}).")
    run.on_cleanup(f"luật {bad_id}", lambda: run.call("DELETE", f"/admin/workflows/{bad_id}", run.admin)[0] in (200, 204, 404))

    st, body = run.call("POST", f"/{ENTITY}", run.staff, {"first_name": "Kiểm Thử", "last_name": mark})
    record_id = (body.get("data") or body).get("id")
    if record_id:
        run.on_cleanup(f"bản ghi {record_id}",
                       lambda: run.call("DELETE", f"/{ENTITY}/{record_id}", run.admin)[0] in (200, 204))
    run.check(f"S2a.1 nhân viên test@example.com lưu {ENTITY} được (luật chạy sau khi lưu)",
              st in (200, 201) and bool(record_id), f"HTTP {st}")
    if not record_id:
        return

    deadline = time.time() + run.a.wait
    while time.time() < deadline and not rx.for_run(wf_id, record_id):
        time.sleep(0.1)
    time.sleep(1.0)  # settle: a duplicate send must be visible before we count
    hits = rx.for_run(wf_id, record_id)
    run.check("S2a.2 đối tác nhận ĐÚNG MỘT POST tới /legacy",
              len(hits) == 1 and urllib.parse.urlsplit(hits[0]["path"]).path == "/legacy", f"{len(hits)} request")
    if hits:
        h, raw = hits[0], hits[0]["body"]
        v1 = header(h, "X-Nexus-Signature")
        good_v1 = v1 is not None and hmac.compare_digest(v1, sig_v1(secret, raw))
        run.check("S2a.3 X-Nexus-Signature (V1) = hex(HMAC-SHA256(khoá, body)) — công thức cũ từng byte",
                  good_v1, "vắng" if v1 is None else "khớp" if good_v1 else "sai")
        ts = header(h, "X-Nexus-Timestamp")
        fresh = ts is not None and ts.isdigit() and abs(h["at"] - int(ts)) <= TS_SKEW
        run.check(f"S2a.4 X-Nexus-Timestamp (giây Unix) lệch ≤ {TS_SKEW} s", fresh,
                  "vắng" if ts is None else f"lệch {h['at'] - int(ts):.1f} s" if ts.isdigit() else "không phải số")
        v2 = header(h, "X-Nexus-Signature-V2")
        good_v2 = v2 is not None and ts is not None and hmac.compare_digest(v2, sig_v2(secret, ts, raw))
        run.check('S2a.5 X-Nexus-Signature-V2 = hex(HMAC-SHA256(khoá, ts + "." + body))', good_v2,
                  "vắng" if v2 is None else "khớp" if good_v2 else "sai")
        auth = header(h, "Authorization")
        run.check("S2a.6 header của luật (Authorization) đi ra y nguyên", auth == "Bearer " + bearer,
                  "vắng" if auth is None else "khớp" if auth == "Bearer " + bearer else "sai giá trị")

    # The old Studio builder only knows {type, url}: its save must be refused, the key kept.
    def actions_of(resp):
        return ((resp.get("data") or {}).get("workflow") or {}).get("actions")

    st_b, before = run.call("GET", f"/admin/workflows/{wf_id}", run.admin)
    st, body = run.call("PUT", f"/admin/workflows/{wf_id}", run.admin,
                        dict(rule, actions=[{"type": "outbound_webhook", "url": legacy_url}]))
    st_a, after = run.call("GET", f"/admin/workflows/{wf_id}", run.admin)
    field = (body.get("details") or {}).get("field")
    run.check("S2a.7 lưu kiểu trình dựng cũ ({type,url} cùng URL, không khoá) ⇒ 409 WEBHOOK_LEGACY_SECRET_PENDING",
              st == 409 and body.get("code") == "WEBHOOK_LEGACY_SECRET_PENDING" and field == "actions[0].url",
              f"HTTP {st} {body.get('code', '')} field={field}")
    run.check("S2a.8 luật không đổi sau lần lưu bị từ chối (khoá vẫn còn)",
              st_b == 200 and st_a == 200 and bool(actions_of(before)) and actions_of(before) == actions_of(after),
              f"GET {st_b}/{st_a}")

    # The failed delivery: durable queue ⇒ sys_jobs.last_error (read on the --db clone); inline
    # (JOBS_ENABLED=false) ⇒ the run's error / actionsResult. Wait until one of them has it.
    texts, deadline = [], time.time() + 25
    while time.time() < deadline:
        st, runs = run.call("GET", f"/admin/workflows/{bad_id}/runs", run.admin)
        texts = [json.dumps(runs, ensure_ascii=False)] if st == 200 else []
        jobs = []
        if CLONE_DB_RE.match(a.db or ""):
            jobs = [r[0] for r in clone_sql(a, "SELECT COALESCE(last_error, '') FROM sys_jobs WHERE job_type = 'outbound_webhook' "
                                                "AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.workflowId')) = '%s'", bad_id)]
        texts += jobs
        if any(origin in t for t in texts):
            break
        time.sleep(0.5)
    leaked = [n for n, v in (("mật khẩu userinfo", pw), ("token trong đường dẫn", path_tok), ("token trong query", q_tok))
              if any(v in t for t in texts)]
    recorded = any(origin in t for t in texts)
    run.check("S2a.9 lần gửi lỗi được ghi, nêu đích bằng scheme://host:port (lịch sử chạy / sys_jobs.last_error)",
              recorded, "có" if recorded else ("chưa thấy lỗi nào — cần --db nexus_wt_<slug> khi hàng đợi bền bật"
                                               if not CLONE_DB_RE.match(a.db or "") else "chưa thấy lỗi nào sau 25 s"))
    run.check("S2a.10 lỗi đó không chứa mật khẩu userinfo, token trong đường dẫn hay trong query",
              recorded and not leaked, ", ".join(leaked) or ("sạch" if recorded else "không có lỗi để kiểm"))


def job_errors(a, wf_id):
    """sys_jobs.last_error of every outbound_webhook job of one rule, read on the --db clone."""
    return [r[0] for r in clone_sql(a, "SELECT COALESCE(last_error, '') FROM sys_jobs WHERE job_type = 'outbound_webhook' "
                                       "AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.workflowId')) = '%s'", wf_id)]


def slice_s2b(run, rx):
    """Rules that call a partner through a vault key (tasks T054, quickstart §4 S2b): the customer
    admin stores the keys, the Nexus builder writes {credentialId, path} rules, a non-admin employee's
    save fires them — the key read at send time, bound to its host, never followed through a 3xx,
    never sent at all when it cannot be used, never readable back."""
    a = run.a
    if not CLONE_DB_RE.match(a.db or ""):
        raise EnvError("nhóm S2b cần --db nexus_wt_<slug> — đọc sys_jobs và ghi một khoá tổ chức khác trên DB bản sao")
    mark = new_mark()
    cadm = run.customer_admin(mark)
    token = secrets.token_urlsafe(24)

    def credential(suffix, body):
        st, resp = run.call("POST", "/admin/credentials", cadm, dict(body, name=f"{mark}-{suffix}",
                                                                   label=f"Nghiệm thu S2b {suffix}", baseUrl=rx.base))
        data = resp.get("data") or {}
        cid = (data.get("credential") or {}).get("id")
        if cid:
            run.on_cleanup(f"khoá {cid}", lambda: run.call("DELETE", f"/admin/credentials/{cid}", run.admin)[0] in (204, 404))
        return st, cid, data

    st1, bearer_id, _ = credential("bearer", {"authType": "header", "secret": token})
    st2, hmac_id, hdata = credential("hmac", {"authType": "hmac", "generateSecret": True})
    gen = hdata.get("secret") or ""
    st3, slot_id, sdata = credential("slot", {"authType": "hmac"})
    run.check("S2b.1 quản trị của khách cất khoá header token + khoá HMAC tự sinh + một chỗ khoá chưa có bí mật",
              (st1, st2, st3) == (201, 201, 201) and bool(bearer_id and hmac_id and slot_id) and gen.startswith("nxw_")
              and (sdata.get("credential") or {}).get("status") == "needs_secret", f"HTTP {st1}/{st2}/{st3}")
    if not (bearer_id and hmac_id and slot_id):
        return

    rule = {"entityName": ENTITY, "trigger": "after_create"}

    def workflow(suffix, action):
        st, resp = run.call("POST", "/admin/workflows", run.admin,
                            dict(rule, name=f"zz nghiệm thu S2b {suffix} {mark}", actions=[action]))
        wf = (resp.get("data") or {}).get("workflow") or {}
        if wf.get("id"):
            run.on_cleanup(f"luật {wf['id']}", lambda: run.call("DELETE", f"/admin/workflows/{wf['id']}", run.admin)[0] in (200, 204, 404))
        return st, resp, wf

    st, resp, bearer_wf = workflow("bearer", {"type": "outbound_webhook", "credentialId": bearer_id, "path": PATH})
    stored = bearer_wf.get("actions") or []
    run.check("S2b.2 người dựng Nexus lưu luật {credentialId, path} — luật chỉ giữ id khoá + path, không url/bí mật",
              st == 201 and stored == [{"type": "outbound_webhook", "credentialId": bearer_id, "path": PATH}],
              f"HTTP {st} {resp.get('code', '')} actions={stored}")
    _, _, hmac_wf = workflow("hmac", {"type": "outbound_webhook", "credentialId": hmac_id, "path": "/signed"})
    _, _, slot_wf = workflow("slot", {"type": "outbound_webhook", "credentialId": slot_id, "path": "/never"})
    _, _, redir_wf = workflow("redirect", {"type": "outbound_webhook", "credentialId": bearer_id, "path": "/redirect"})
    if not all(w.get("id") for w in (bearer_wf, hmac_wf, slot_wf, redir_wf)):
        raise EnvError("không tạo được đủ bốn luật dùng khoá — xem S2b.2")

    # Another tenant's key: a row written straight into the clone under a tenant id that is not ours.
    foreign_id, foreign_tenant = str(uuid.uuid4()), str(uuid.uuid4())
    clone_sql(a, "INSERT INTO sys_named_credentials (id, tenant_id, name, label, base_url, auth_type) "
                 "VALUES ('%s', '%s', 'khoa-to-chuc-khac', 'Khoá tổ chức khác', 'https://b.example.vn', 'header')",
              foreign_id, foreign_tenant)
    run.on_cleanup(f"khoá tổ chức khác {foreign_id}",
                   lambda: clone_sql(a, "DELETE FROM sys_named_credentials WHERE id = '%s' AND tenant_id = '%s'",
                                     foreign_id, foreign_tenant) == [])
    st, resp = run.call("POST", "/admin/workflows", run.admin, dict(rule, name=f"zz nghiệm thu S2b cross {mark}",
                                                                     actions=[{"type": "outbound_webhook", "credentialId": foreign_id}]))
    if ((resp.get("data") or {}).get("workflow") or {}).get("id"):
        wid = resp["data"]["workflow"]["id"]
        run.on_cleanup(f"luật {wid}", lambda: run.call("DELETE", f"/admin/workflows/{wid}", run.admin)[0] in (200, 204, 404))
    run.check("S2b.3 luật trỏ khoá của tổ chức khác ⇒ 422 WEBHOOK_CREDENTIAL_NOT_FOUND tại actions[0].credentialId",
              st == 422 and resp.get("code") == "WEBHOOK_CREDENTIAL_NOT_FOUND"
              and (resp.get("details") or {}).get("field") == "actions[0].credentialId",
              f"HTTP {st} {resp.get('code', '')} field={(resp.get('details') or {}).get('field')}")

    # A path the send-time join cannot parse ("/%zz") must be refused at save time, not saved and then
    # failed on every delivery (config/send parity — the one ValidateCredentialPath rule).
    st, resp = run.call("POST", "/admin/workflows", run.admin, dict(rule, name=f"zz nghiệm thu S2b escape {mark}",
                                                                     actions=[{"type": "outbound_webhook", "credentialId": bearer_id, "path": "/%zz"}]))
    if ((resp.get("data") or {}).get("workflow") or {}).get("id"):
        wid = resp["data"]["workflow"]["id"]
        run.on_cleanup(f"luật {wid}", lambda: run.call("DELETE", f"/admin/workflows/{wid}", run.admin)[0] in (200, 204, 404))
    run.check("S2b.3b path có % lẻ (/%zz) ⇒ 422 WEBHOOK_PATH_INVALID tại actions[0].path — không lưu rồi hỏng mọi lần giao",
              st == 422 and resp.get("code") == "WEBHOOK_PATH_INVALID"
              and (resp.get("details") or {}).get("field") == "actions[0].path",
              f"HTTP {st} {resp.get('code', '')} field={(resp.get('details') or {}).get('field')}")

    st, body = run.call("POST", f"/{ENTITY}", run.staff, {"first_name": "Kiểm Thử", "last_name": mark})
    record_id = (body.get("data") or body).get("id")
    if record_id:
        run.on_cleanup(f"bản ghi {record_id}", lambda: run.call("DELETE", f"/{ENTITY}/{record_id}", run.admin)[0] in (200, 204))
    run.check(f"S2b.4 nhân viên test@example.com lưu {ENTITY} được — khoá hỏng của một luật không chặn lưu bản ghi",
              st in (200, 201) and bool(record_id), f"HTTP {st}")
    if not record_id:
        return

    deadline = time.time() + max(run.a.wait, 10)
    while time.time() < deadline and not (rx.for_run(bearer_wf["id"], record_id) and rx.for_run(hmac_wf["id"], record_id)):
        time.sleep(0.1)
    time.sleep(1.5)  # settle: a duplicate or a followed redirect must be visible before we count
    hits = rx.for_run(bearer_wf["id"], record_id)
    auth = header(hits[0], "Authorization") if hits else None
    run.check(f"S2b.5 đối tác nhận ĐÚNG MỘT POST {PATH} mang Authorization: Bearer <token của khoá>",
              len(hits) == 1 and urllib.parse.urlsplit(hits[0]["path"]).path == PATH and auth == "Bearer " + token,
              f"{len(hits)} request, Authorization " + ("vắng" if auth is None else "khớp" if auth == "Bearer " + token else "sai"))

    hits = rx.for_run(hmac_wf["id"], record_id)
    ok = False
    if len(hits) == 1:
        h, raw = hits[0], hits[0]["body"]
        v1, ts, v2 = header(h, "X-Nexus-Signature"), header(h, "X-Nexus-Timestamp"), header(h, "X-Nexus-Signature-V2")
        ok = (v1 is not None and hmac.compare_digest(v1, sig_v1(gen, raw)) and ts is not None and ts.isdigit()
              and abs(h["at"] - int(ts)) <= TS_SKEW and v2 is not None and hmac.compare_digest(v2, sig_v2(gen, ts, raw)))
    run.check("S2b.6 khoá HMAC: một POST /signed mang V1 = HMAC(khoá, body), timestamp lệch ≤ 5 s, V2 = HMAC(khoá, ts.body)",
              ok, f"{len(hits)} request")

    with rx.lock:
        raw_hits = list(rx.hits)
    never = [h for h in raw_hits if h["path"].startswith("/never")]
    redirect = [h for h in raw_hits if h["path"].startswith("/redirect")]
    stolen = [h for h in raw_hits if h["path"].startswith("/stolen")]

    def job_error_for(wf_id, code):
        texts, until = [], time.time() + 25
        while time.time() < until:
            texts = job_errors(a, wf_id)
            st, runs = run.call("GET", f"/admin/workflows/{wf_id}/runs", run.admin)
            texts += [json.dumps(runs, ensure_ascii=False)] if st == 200 else []
            if any(code in t for t in texts):
                return True, texts
            time.sleep(0.5)
        return False, texts

    found, _ = job_error_for(slot_wf["id"], "CREDENTIAL_NOT_READY")
    run.check("S2b.7 chỗ khoá chưa có bí mật ⇒ đối tác nhận 0 request, lần gửi ghi CREDENTIAL_NOT_READY",
              not never and found, f"{len(never)} request; mã {'có' if found else 'chưa thấy sau 25 s'}")
    found, texts = job_error_for(redir_wf["id"], "WEBHOOK_REDIRECT_NOT_FOLLOWED")
    run.check("S2b.8 đối tác trả 302 ⇒ không đi theo (đích chuyển hướng nhận 0 request), ghi WEBHOOK_REDIRECT_NOT_FOLLOWED",
              len(redirect) == 1 and not stolen and found,
              f"/redirect {len(redirect)}, /stolen {len(stolen)}; mã {'có' if found else 'chưa thấy'}")

    blobs = texts + job_errors(a, bearer_wf["id"]) + job_errors(a, hmac_wf["id"])
    blobs += [r[0] for r in clone_sql(a, "SELECT CAST(payload AS CHAR) FROM sys_jobs WHERE job_type = 'outbound_webhook' "
                                         "AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.workflowId')) = '%s'", bearer_wf["id"])]
    for path in (f"/admin/workflows/{bearer_wf['id']}", f"/admin/workflows/{hmac_wf['id']}", "/admin/workflows",
                 f"/admin/workflows/{bearer_wf['id']}/runs", f"/admin/workflows/{hmac_wf['id']}/runs"):
        blobs.append(json.dumps(run.call("GET", path, run.admin)[1], ensure_ascii=False))
    st, dry = run.call("POST", f"/admin/workflows/{bearer_wf['id']}/test", run.admin, {"recordId": record_id})
    blobs.append(json.dumps(dry, ensure_ascii=False))
    leaked = [n for n, v in (("token Bearer", token), ("khoá HMAC tự sinh", gen)) if any(v in b for b in blobs)]
    run.check("S2b.9 đọc luật / lịch sử chạy / chạy thử / sys_jobs (payload + lỗi) không lộ token hay khoá HMAC",
              not leaked, ", ".join(leaked) or "sạch")
    steps = ((dry.get("data") or {}).get("test") or {}).get("trace") or []
    web = next((s.get("detail") or {} for s in steps if (s.get("detail") or {}).get("type") == "outbound_webhook"), {})
    cred = web.get("credential") or {}
    run.check("S2b.10 chạy thử nêu tên khoá + kiểu + trạng thái + URL hiển thị, sẽ gửi",
              st == 200 and cred.get("name") == f"{mark}-bearer" and cred.get("authType") == "header"
              and cred.get("status") == "ready" and web.get("url") == rx.base + PATH and web.get("wouldSend") is True,
              f"HTTP {st} credential={cred} url={web.get('url')}")


def slice_s3a(run, rx):
    """Mask on read, refuse on write (tasks T065, quickstart §4 S3a). A rule saved before S3a reads back
    through every admin route without its key, header value, userinfo (name too) or query values; a
    save still carrying the mask or a key is refused and the stored row stays; after those refusals a
    non-admin employee's save still sends the REAL values — the engine reads the stored row."""
    a = run.a
    if not CLONE_DB_RE.match(a.db or ""):
        raise EnvError("nhóm S3a cần --db nexus_wt_<slug> — luật cũ mang khoá trần chỉ còn cắm được bằng SQL")
    mark = new_mark()
    # uname: the key sits in the userinfo NAME as often as in the password (Freshdesk `KEY:X`, Stripe
    # `KEY:`) — it is a secret here like the others.
    secret, bearer, uname, pw, qtok, ktok = (secrets.token_hex(12) for _ in range(6))
    port = urllib.parse.urlsplit(rx.base).port
    rule = {"name": f"zz nghiệm thu S3a {mark}", "entityName": ENTITY, "trigger": "after_create"}
    legacy = {"type": "outbound_webhook", "url": f"http://{uname}:{pw}@127.0.0.1:{port}/legacy?token={qtok}&src=nexus",
              "secret": secret, "headers": {"Authorization": "Bearer " + bearer}}
    wf_id, tenant_id = legacy_rule(run, rule, [legacy])
    planted = stored_actions(a, tenant_id, wf_id)

    # A vault rule whose path carries a query token (S2b shape) — the same mask, the same refusal.
    cadm = run.customer_admin(mark)
    st, resp = run.call("POST", "/admin/credentials", cadm, {"name": f"{mark}-s3a", "label": "Nghiệm thu S3a",
                                                             "baseUrl": rx.base, "authType": "header",
                                                             "secret": secrets.token_urlsafe(24)})
    cred_id = ((resp.get("data") or {}).get("credential") or {}).get("id")
    if not cred_id:
        raise EnvError(f"không cất được khoá tạm ({st} {resp.get('code', '')}) — server cần CREDENTIAL_ENC_KEY")
    run.on_cleanup(f"khoá {cred_id}", lambda: run.call("DELETE", f"/admin/credentials/{cred_id}", run.admin)[0] in (204, 404))
    vrule = dict(rule, name=f"zz nghiệm thu S3a khoá {mark}")
    st, resp = run.call("POST", "/admin/workflows", run.admin,
                        dict(vrule, actions=[{"type": "outbound_webhook", "credentialId": cred_id, "path": f"/orders?key={ktok}"}]))
    vault_wf = (resp.get("data") or {}).get("workflow") or {}
    if not vault_wf.get("id"):
        raise EnvError(f"không tạo được luật dùng khoá ({st} {resp.get('code', '')}).")
    vault_id = vault_wf["id"]
    run.on_cleanup(f"luật {vault_id}", lambda: run.call("DELETE", f"/admin/workflows/{vault_id}", run.admin)[0] in (200, 204, 404))

    def actions_of(resp):
        return ((resp.get("data") or {}).get("workflow") or {}).get("actions") or []

    secrets_named = (("khoá ký", secret), ("token Bearer", bearer), ("tên userinfo", uname), ("mật khẩu userinfo", pw),
                     ("token query của URL", qtok), ("token query của path", ktok))

    def leaks(blobs):
        return [n for n, v in secrets_named if any(v in b for b in blobs)]

    reads = {}
    for path in (f"/admin/workflows/{wf_id}", f"/admin/workflows/{vault_id}", "/admin/workflows"):
        st, body = run.call("GET", path, run.admin)
        reads[path] = (st, body)
    blobs = [json.dumps(b, ensure_ascii=False) for _, b in reads.values()] + [json.dumps(resp, ensure_ascii=False)]
    leaked = leaks(blobs)
    run.check("S3a.1 GET /admin/workflows[/:id] + phản hồi POST không lộ khoá ký, token Bearer, tên/mật khẩu userinfo hay giá trị query",
              all(st == 200 for st, _ in reads.values()) and not leaked,
              ", ".join(leaked) or "sạch")
    got = (actions_of(reads[f"/admin/workflows/{wf_id}"][1]) or [{}])[0]
    vgot = (actions_of(reads[f"/admin/workflows/{vault_id}"][1]) or [{}])[0]
    want_url = f"http://{MASK}@127.0.0.1:{port}/legacy?token={MASK}&src={MASK}"
    shape = ("secret" not in got and (got.get("headers") or {}).get("Authorization") == MASK and got.get("url") == want_url
             and vgot.get("path") == f"/orders?key={MASK}")
    run.check("S3a.2 bản đọc giữ tên, che giá trị: header Authorization = ••••, không còn `secret`, "
              "URL ••••@…?token=••••&src=•••• (che cả userinfo), path /orders?key=••••",
              shape, "khớp" if shape else "không khớp (không in giá trị)")

    def refused(label, path, method, body, code, field):
        st, resp = run.call(method, path, run.admin, body)
        if st in (200, 201):
            wid = ((resp.get("data") or {}).get("workflow") or {}).get("id")
            if wid and wid not in (wf_id, vault_id):
                run.on_cleanup(f"luật {wid}", lambda: run.call("DELETE", f"/admin/workflows/{wid}", run.admin)[0] in (200, 204, 404))
        f = (resp.get("details") or {}).get("field")
        run.check(label, st == 422 and resp.get("code") == code and f == field, f"HTTP {st} {resp.get('code', '')} field={f}")

    refused("S3a.3 lưu lại đúng bản vừa GET (header ••••) ⇒ 422 WEBHOOK_INLINE_SECRET_REMOVED tại actions[0].headers",
            f"/admin/workflows/{wf_id}", "PUT", dict(rule, actions=[got]), "WEBHOOK_INLINE_SECRET_REMOVED", "actions[0].headers")
    refused("S3a.4 lưu kiểu trình dựng cũ ({type, url} đã che) ⇒ 422 WEBHOOK_REDACTED_VALUE tại actions[0].url",
            f"/admin/workflows/{wf_id}", "PUT", dict(rule, actions=[{"type": "outbound_webhook", "url": got.get("url") or want_url}]),
            "WEBHOOK_REDACTED_VALUE", "actions[0].url")
    # Phản biện S3b: the SAME key's masked path sent back as read (the builder shows it read-only) keeps
    # the stored path — a mask that is not the stored path's is still refused.
    st, resp = run.call("PUT", f"/admin/workflows/{vault_id}", run.admin,
                        dict(vrule, actions=[{"type": "outbound_webhook", "credentialId": cred_id, "path": vgot.get("path") or f"/orders?key={MASK}"}]))
    run.check("S3a.5 luật dùng khoá gửi lại ĐÚNG path đã che (cùng khoá, như trình dựng) ⇒ 200, path thật được giữ (S3a.7/S3a.10 kiểm tiếp)",
              st == 200, f"HTTP {st} {resp.get('code', '')}")
    refused("S3a.5b luật dùng khoá gửi path che KHÁC path đang lưu ⇒ 422 WEBHOOK_REDACTED_VALUE tại actions[0].path",
            f"/admin/workflows/{vault_id}", "PUT",
            dict(vrule, actions=[{"type": "outbound_webhook", "credentialId": cred_id, "path": f"/orders?key={MASK}&extra={MASK}"}]),
            "WEBHOOK_REDACTED_VALUE", "actions[0].path")
    refused("S3a.6 luật MỚI kèm `secret` riêng ⇒ 422 WEBHOOK_INLINE_SECRET_REMOVED tại actions[0].secret (chỉ đường sang Kho khoá kết nối)",
            "/admin/workflows", "POST", dict(rule, name=f"zz nghiệm thu S3a mới {mark}",
                                             actions=[{"type": "outbound_webhook", "url": rx.base + "/never", "secret": secrets.token_hex(16)}]),
            "WEBHOOK_INLINE_SECRET_REMOVED", "actions[0].secret")
    after = stored_actions(a, tenant_id, wf_id)
    vafter = stored_actions(a, tenant_id, vault_id)
    kept = after == planted
    vkept = bool(vafter) and vafter[0].get("path") == f"/orders?key={ktok}"
    run.check("S3a.7 sys_workflows.actions không đổi sau các lần bị từ chối (khoá, token, path thật vẫn nguyên)",
              kept and vkept, f"luật cũ {'nguyên' if kept else 'ĐÃ ĐỔI'}, luật dùng khoá {'nguyên' if vkept else 'ĐÃ ĐỔI'}")

    st, body = run.call("POST", f"/{ENTITY}", run.staff, {"first_name": "Kiểm Thử", "last_name": mark})
    record_id = (body.get("data") or body).get("id")
    if record_id:
        run.on_cleanup(f"bản ghi {record_id}", lambda: run.call("DELETE", f"/{ENTITY}/{record_id}", run.admin)[0] in (200, 204))
    run.check(f"S3a.8 nhân viên test@example.com lưu {ENTITY} được (luật chạy sau khi lưu)", st in (200, 201) and bool(record_id), f"HTTP {st}")
    if not record_id:
        return
    deadline = time.time() + max(run.a.wait, 10)
    while time.time() < deadline and not (rx.for_run(wf_id, record_id) and rx.for_run(vault_id, record_id)):
        time.sleep(0.1)
    time.sleep(1.0)  # settle: a duplicate send must be visible before we count
    hits = rx.for_run(wf_id, record_id)
    ok = False
    if len(hits) == 1:
        h = hits[0]
        u = urllib.parse.urlsplit(h["path"])
        q = urllib.parse.parse_qs(u.query)
        v1 = header(h, "X-Nexus-Signature")
        ok = (u.path == "/legacy" and q.get("token") == [qtok] and q.get("src") == ["nexus"]
              and header(h, "Authorization") == "Bearer " + bearer
              and v1 is not None and hmac.compare_digest(v1, sig_v1(secret, h["body"])))
    run.check("S3a.9 đối tác nhận ĐÚNG MỘT POST /legacy mang token query thật, Authorization thật, V1 ký bằng khoá thật "
              "— đường gửi đọc bản gốc, không bản che", ok, f"{len(hits)} request, " + ("khớp" if ok else "không khớp"))
    vhits = rx.for_run(vault_id, record_id)
    vok = len(vhits) == 1 and urllib.parse.parse_qs(urllib.parse.urlsplit(vhits[0]["path"]).query).get("key") == [ktok]
    run.check("S3a.10 luật dùng khoá gửi path với token query THẬT (không phải ••••)", vok,
              f"{len(vhits)} request, " + ("khớp" if vok else "không khớp"))

    st, dry = run.call("POST", f"/admin/workflows/{wf_id}/test", run.admin, {"recordId": record_id})
    steps = ((dry.get("data") or {}).get("test") or {}).get("trace") or []
    web = next((x.get("detail") or {} for x in steps if (x.get("detail") or {}).get("type") == "outbound_webhook"), {})
    dleak = leaks([json.dumps(dry, ensure_ascii=False)])
    run.check("S3a.11 chạy thử không lộ gì (URL đã che như GET) nhưng vẫn biết sẽ ký (đọc khoá gốc)",
              st == 200 and not dleak and web.get("url") == want_url and web.get("wouldSign") is True,
              ", ".join(dleak) or f"HTTP {st} wouldSign={web.get('wouldSign')} url {'khớp' if web.get('url') == want_url else 'không khớp'}")


C10 = "C10_WEBHOOK_INLINE_SECRET"  # the Doctor step of S3b (C9 was already taken — queue #143)
LEGACY_LABEL_SUFFIX = " (dời từ quy trình)"


def slice_s3b(run, rx):
    """Doctor C10 moves an inline key into the vault (tasks T073, quickstart §4 S3b). A rule saved before
    the vault (signing key + Authorization, planted by SQL) sends; the operator scans, is refused without
    confirm, confirms; the rule now holds only {credentialId, path}, the key sits sealed in the vault, and
    a non-admin employee's next save reaches the partner with the SAME path, Authorization and V1 (plus
    V2 + timestamp); a second fix stores no second key; a private server WITHOUT a master key refuses the
    move (CREDENTIALS_NOT_CONFIGURED) and changes nothing."""
    a = run.a
    if not CLONE_DB_RE.match(a.db or ""):
        raise EnvError("nhóm S3b cần --db nexus_wt_<slug> — luật cũ mang khoá trần chỉ còn cắm được bằng SQL")
    mark = new_mark()
    secret, bearer = secrets.token_hex(12), secrets.token_hex(16)  # 24-char key: the short-key path too
    rule = {"name": f"zz nghiệm thu S3b {mark}", "entityName": ENTITY, "trigger": "after_create"}
    legacy = {"type": "outbound_webhook", "url": rx.base + "/legacy?src=nexus", "secret": secret,
              "headers": {"Authorization": "Bearer " + bearer}}
    wf_id, tenant_id = legacy_rule(run, rule, [legacy])

    def live_keys():
        return int(clone_sql(a, "SELECT COUNT(*) FROM sys_named_credentials WHERE tenant_id = '%s' AND deleted_at IS NULL",
                             tenant_id)[0][0])

    def staff_save(label):
        st, body = run.call("POST", f"/{ENTITY}", run.staff, {"first_name": "Kiểm Thử", "last_name": mark})
        record_id = (body.get("data") or body).get("id")
        if record_id:
            run.on_cleanup(f"bản ghi {record_id}", lambda: run.call("DELETE", f"/{ENTITY}/{record_id}", run.admin)[0] in (200, 204))
        run.check(f"{label} nhân viên test@example.com lưu {ENTITY} được (luật chạy sau khi lưu)",
                  st in (200, 201) and bool(record_id), f"HTTP {st}")
        if not record_id:
            return []
        deadline = time.time() + max(a.wait, 10)
        while time.time() < deadline and not rx.for_run(wf_id, record_id):
            time.sleep(0.1)
        time.sleep(1.0)  # settle: a duplicate send must be visible before we count
        return rx.for_run(wf_id, record_id)

    # Phản biện S3b: a rule DELETED before C10 keeps its actions — key included — in sys_workflows.
    gone_id, _ = legacy_rule(run, dict(rule, name=f"zz nghiệm thu S3b đã xoá {mark}"), [legacy])
    st, _ = run.call("DELETE", f"/admin/workflows/{gone_id}", run.admin)
    if st not in (200, 204):
        raise EnvError(f"không xoá được luật nền đã xoá ({st})")

    before = staff_save("S3b.1")
    ok = (len(before) == 1 and before[0]["path"] == "/legacy?src=nexus"
          and header(before[0], "Authorization") == "Bearer " + bearer
          and hmac.compare_digest(header(before[0], "X-Nexus-Signature") or "", sig_v1(secret, before[0]["body"])))
    run.check("S3b.2 TRƯỚC khi dời: đối tác nhận đúng một POST /legacy?src=nexus, Authorization + V1 bằng khoá trần",
              ok, f"{len(before)} request, " + ("khớp" if ok else "không khớp"))
    if not ok:
        return

    # C10 leaves a rule alone while its deliveries are still parked in sys_jobs (moving it would drop
    # them): the partner has the POST, but the job row may not be marked done yet.
    deadline = time.time() + 30
    while time.time() < deadline and int(clone_sql(
            a, "SELECT COUNT(*) FROM sys_jobs WHERE tenant_id = '%s' AND job_type = 'outbound_webhook' "
               "AND status IN ('queued', 'running') AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.workflowId')) = '%s'",
            tenant_id, wf_id)[0][0]) > 0:
        time.sleep(0.5)

    # Phản biện S3b: that delivery failed (the partner was down) — it keeps its payload and "Gửi lại" could
    # still send it until the slot names a key. Planted on the clone: the done job becomes a failed one.
    failed_job = (clone_sql(a, "SELECT id FROM sys_jobs WHERE tenant_id = '%s' AND job_type = 'outbound_webhook' AND status = 'done' "
                               "AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.workflowId')) = '%s' LIMIT 1", tenant_id, wf_id) or [[""]])[0][0]
    if not UUID_RE.match(failed_job):
        raise EnvError("không thấy job giao webhook của S3b.1 trong sys_jobs")
    clone_sql(a, "UPDATE sys_jobs SET status = 'failed', last_error = 'HTTP 503 (nghiệm thu S3b)' WHERE tenant_id = '%s' AND id = '%s'",
              tenant_id, failed_job)

    keys_before = live_keys()
    st, scan = run.call("POST", "/admin/doctor/scan", run.admin)
    finding = next((f for f in ((scan.get("data") or {}).get("findings") or []) if f.get("checkId") == C10), None)
    item = next((i for i in ((finding or {}).get("items") or []) if i.get("key") == wf_id), None)
    detail = (item or {}).get("detail") or ""
    shown = st == 200 and finding is not None and finding.get("autoFixSafe") is False and item is not None
    clean = secret not in detail and bearer not in detail and "src=nexus" not in detail
    run.check(f"S3b.3 Bác sĩ hệ thống liệt kê luật ở {C10} (cần xác nhận), câu mô tả nêu 'một chiều' + không theo chuyển hướng, "
              "không lộ khoá / token / query", shown and clean and "một chiều" in detail and "chuyển hướng" in detail,
              f"HTTP {st}, " + ("có" if item else "KHÔNG có luật") + (", sạch" if clean else ", LỘ"))
    run.check("S3b.3b finding của luật báo trước '1 lần gửi đã lỗi … KHÔNG Gửi lại được sau khi dời'",
              "1 lần gửi đã lỗi" in detail and "Gửi lại" in detail, "có" if "1 lần gửi đã lỗi" in detail else "không có")
    gone = next((i for i in ((finding or {}).get("items") or []) if i.get("key") == "deleted:" + gone_id), None)
    gdetail = (gone or {}).get("detail") or ""
    run.check("S3b.3c luật ĐÃ XOÁ còn khoá trần cũng được liệt kê (deleted:<id>, 'ĐÃ XOÁ', khuyên xoay) — không lộ khoá",
              gone is not None and "ĐÃ XOÁ" in gdetail and "xoay" in gdetail and secret not in gdetail and bearer not in gdetail,
              "có" if gone else "KHÔNG có")

    st, body = run.call("POST", "/admin/doctor/fix", run.admin, {"checkIds": [C10]})
    planted = stored_actions(a, tenant_id, wf_id)
    run.check("S3b.4 sửa KHÔNG kèm xác nhận ⇒ 422 DOCTOR_CONFIRM_REQUIRED, luật và kho không đổi",
              st == 422 and body.get("code") == "DOCTOR_CONFIRM_REQUIRED" and planted == [legacy] and live_keys() == keys_before,
              f"HTTP {st} {body.get('code', '')}")

    st, body = run.call("POST", "/admin/doctor/fix", run.admin, {"checkIds": [C10], "confirm": True})
    res = next((r for r in ((body.get("data") or {}).get("results") or []) if r.get("checkId") == C10), {})
    mine = [e for e in (res.get("errors") or []) if wf_id in e]
    run.check("S3b.5 sửa CÓ xác nhận ⇒ 200, luật của lượt này dời được (không lỗi nào nêu nó)",
              st == 200 and (res.get("fixed") or 0) >= 1 and not mine, f"HTTP {st} fixed={res.get('fixed')} failed={res.get('failed')}")

    moved = stored_actions(a, tenant_id, wf_id) or [{}]
    cred_id = moved[0].get("credentialId") or ""
    if UUID_RE.match(cred_id):
        # C10 named the key (no run mark, so the sweep cannot find it): delete it LAST — after every rule
        # using it is gone (cleanup runs in reverse), or the vault answers 409 CREDENTIAL_IN_USE.
        run.cleanup.insert(0, (f"khoá {cred_id}",
                               lambda: run.call("DELETE", f"/admin/credentials/{cred_id}", run.admin)[0] in (204, 404)))
    raw = json.dumps(moved)
    run.check("S3b.6 sys_workflows.actions chỉ còn {type, credentialId, path} — không còn secret, headers hay url",
              set(moved[0]) == {"type", "credentialId", "path"} and moved[0].get("path") == "/legacy?src=nexus"
              and secret not in raw and bearer not in raw, ", ".join(sorted(moved[0])))
    wiped = stored_actions(a, tenant_id, gone_id) or [{}]
    wraw = json.dumps(wiped)
    run.check("S3b.6b luật ĐÃ XOÁ: khoá bị xoá khỏi bản ghi (không secret, headers, path/query), url chỉ còn địa chỉ gốc, không dời vào kho",
              secret not in wraw and bearer not in wraw and "legacy" not in wraw and "secret" not in wiped[0]
              and "headers" not in wiped[0] and wiped[0].get("url") == rx.base and not wiped[0].get("credentialId"),
              ", ".join(sorted(wiped[0])))

    st, body = run.call("GET", f"/admin/credentials/{cred_id}", run.admin) if UUID_RE.match(cred_id) else (0, {})
    view = (body.get("data") or {}).get("credential") or {}
    sealed = clone_sql(a, "SELECT COUNT(*) FROM sys_named_credentials WHERE tenant_id = '%s' AND id = '%s' "
                          "AND secret_enc LIKE 'v1:%%' AND extra_headers_enc LIKE 'v1:%%'", tenant_id, cred_id) if cred_id else [["0"]]
    blob = json.dumps(body, ensure_ascii=False)
    run.check("S3b.7 kho có khoá HMAC nhãn '(dời từ quy trình)', header bổ sung Authorization, niêm phong trong DB, "
              "đọc qua API không lộ khoá hay token",
              st == 200 and view.get("authType") == "hmac" and (view.get("label") or "").endswith(LEGACY_LABEL_SUFFIX)
              and view.get("extraHeaderNames") == ["Authorization"] and sealed[0][0] == "1"
              and secret not in blob and bearer not in blob, f"HTTP {st} {view.get('authType')} niêm phong={sealed[0][0]}")

    # Phản biện S3b: the builder re-saves the moved rule as it read it — path shown masked, read-only.
    st, body = run.call("GET", f"/admin/workflows/{wf_id}", run.admin)
    shown_act = ((((body.get("data") or {}).get("workflow") or {}).get("actions")) or [{}])[0]
    st, body = run.call("PUT", f"/admin/workflows/{wf_id}", run.admin, dict(rule, actions=[
        {"type": "outbound_webhook", "credentialId": shown_act.get("credentialId"), "path": shown_act.get("path")}]))
    kept = (stored_actions(a, tenant_id, wf_id) or [{}])[0]
    run.check("S3b.7b trình dựng lưu lại luật đã dời (path đọc ra đã che /legacy?src=••••) ⇒ 200, path thật giữ nguyên",
              shown_act.get("path") == "/legacy?src=" + MASK and st == 200 and kept.get("path") == "/legacy?src=nexus"
              and kept.get("credentialId") == cred_id, f"HTTP {st} {body.get('code', '')}")

    after = staff_save("S3b.8")
    ok = False
    if len(after) == 1:
        h, body_raw = after[0], after[0]["body"]
        ts = header(h, "X-Nexus-Timestamp") or ""
        ok = (h["path"] == before[0]["path"] and header(h, "Authorization") == header(before[0], "Authorization")
              and hmac.compare_digest(header(h, "X-Nexus-Signature") or "", sig_v1(secret, body_raw))
              and ts.isdigit() and abs(h["at"] - int(ts)) <= TS_SKEW
              and hmac.compare_digest(header(h, "X-Nexus-Signature-V2") or "", sig_v2(secret, ts, body_raw)))
    run.check("S3b.9 SAU khi dời: đối tác nhận đúng một POST, cùng đường dẫn, Authorization GIỐNG TỪNG BYTE, V1 vẫn bằng "
              "khoá cũ, thêm timestamp + V2", ok, f"{len(after)} request, " + ("khớp" if ok else "không khớp"))

    # The warning was true: "Gửi lại" of the delivery that failed before the move is accepted, then fails
    # for good ("rule đã đổi") and the partner receives nothing.
    hits_before = len(rx.hits)
    st, _ = run.call("POST", f"/admin/jobs/{failed_job}/retry", run.admin)
    deadline, last = time.time() + 30, ["", ""]
    while time.time() < deadline:
        last = (clone_sql(a, "SELECT status, COALESCE(last_error, '') FROM sys_jobs WHERE tenant_id = '%s' AND id = '%s'",
                          tenant_id, failed_job) or [["", ""]])[0]
        if last[0] == "failed":
            break
        time.sleep(0.5)
    run.check("S3b.9b 'Gửi lại' lần gửi đã lỗi trước khi dời ⇒ được nhận rồi hỏng hẳn 'rule đã đổi' (đúng như finding báo trước)",
              st == 200 and last[0] == "failed" and "rule đã đổi" in last[1]
              and len(rx.hits) == hits_before, f"HTTP {st}, {last[0]}")

    keys_mid = live_keys()
    st, body = run.call("POST", "/admin/doctor/fix", run.admin, {"checkIds": [C10], "confirm": True})
    run.check("S3b.10 bấm C10 lần hai ⇒ 0 khoá mới trong kho", st == 200 and live_keys() == keys_mid,
              f"HTTP {st}, khoá {keys_mid} ⇒ {live_keys()}")

    # A private server with no master key, on the same clone: the move is refused, nothing changes.
    try:
        a.clone_dsn = clone_dsn(os.environ.get("DATABASE_URL", ""), a.db)
    except ValueError as e:
        raise EnvError(str(e)) from e
    if not os.path.isfile(a.binary) or not os.environ.get("JWT_SECRET"):
        raise EnvError("S3b.11 cần --binary đã build và .env đã nạp (`set -a && . ./.env && set +a`)")
    wf2, _ = legacy_rule(run, dict(rule, name=f"zz nghiệm thu S3b không khoá chủ {mark}"), [legacy])
    keys_nokey = live_keys()
    seen = {}

    def probe(base):
        st, body = call(base, "POST", "/auth/login", body={"tenant": a.tenant, "email": a.admin,
                                                           "password": os.environ.get("NEXUS_ADMIN_PASS", "Demo-Admin-2026!")})
        token = (body.get("data") or {}).get("accessToken")
        if st != 200 or not token:
            return
        seen["st"], seen["body"] = call(base, "POST", "/admin/doctor/fix", token, {"checkIds": [C10], "confirm": True})

    up, _, _ = boot(a, {}, "vắng CREDENTIAL_ENC_KEY", port=a.nokey_port, probe=probe)
    res = next((r for r in (((seen.get("body") or {}).get("data") or {}).get("results") or []) if r.get("checkId") == C10), {})
    refused = [e for e in (res.get("errors") or []) if wf2 in e and "CREDENTIALS_NOT_CONFIGURED" in e]
    run.check("S3b.11 máy chủ riêng KHÔNG khoá chủ: sửa C10 ⇒ luật báo CREDENTIALS_NOT_CONFIGURED, 0 dòng sys_workflows / kho đổi",
              up and seen.get("st") == 200 and bool(refused) and (res.get("fixed") or 0) == 0
              and stored_actions(a, tenant_id, wf2) == [legacy] and live_keys() == keys_nokey,
              f"lên={up} HTTP {seen.get('st')} fixed={res.get('fixed')} từ chối={'có' if refused else 'không'}")


def slice_s4(run, rx):
    """Rotation (tasks T083, quickstart §4 S4): the customer admin rotates keys and a NON-admin employee's
    next save carries the new material — a header token within 30 s, an hmac key signing V2 under the new
    key AND V2-Previous under the old one during the grace, a slot filled by rotation (no previous key), and
    a host change that leaves the old extra header and the old key behind. Nothing secret is read back."""
    a = run.a
    if not CLONE_DB_RE.match(a.db or ""):
        raise EnvError("nhóm S4 cần --db nexus_wt_<slug> — đọc sys_jobs.last_error của chỗ khoá chưa nhập")
    mark = new_mark()
    cadm = run.customer_admin(mark)
    tok1, tok2 = secrets.token_urlsafe(24), secrets.token_urlsafe(24)
    key1, key2, key3 = (secrets.token_urlsafe(32) for _ in range(3))  # 43 chars >= 32
    extra = secrets.token_hex(8)

    def credential(suffix, body):
        st, resp = run.call("POST", "/admin/credentials", cadm, dict(body, name=f"{mark}-{suffix}",
                                                                   label=f"Nghiệm thu S4 {suffix}", baseUrl=rx.base))
        cid = ((resp.get("data") or {}).get("credential") or {}).get("id")
        if cid:
            run.on_cleanup(f"khoá {cid}", lambda: run.call("DELETE", f"/admin/credentials/{cid}", run.admin)[0] in (204, 404))
        return st, cid

    def workflow(suffix, cid, path):
        st, resp = run.call("POST", "/admin/workflows", run.admin,
                            {"name": f"zz nghiệm thu S4 {suffix} {mark}", "entityName": ENTITY, "trigger": "after_create",
                             "actions": [{"type": "outbound_webhook", "credentialId": cid, "path": path}]})
        wid = ((resp.get("data") or {}).get("workflow") or {}).get("id")
        if wid:
            run.on_cleanup(f"luật {wid}", lambda: run.call("DELETE", f"/admin/workflows/{wid}", run.admin)[0] in (200, 204, 404))
        return wid

    st1, hdr_id = credential("header", {"authType": "header", "secret": tok1})
    st2, hm_id = credential("hmac", {"authType": "hmac", "secret": key1, "extraHeaders": {"X-Doi-Tac": extra}})
    st3, slot_id = credential("slot", {"authType": "hmac"})
    run.check("S4.1 quản trị của khách cất khoá header token + khoá HMAC (kèm header bổ sung) + một chỗ khoá chưa nhập",
              (st1, st2, st3) == (201, 201, 201) and bool(hdr_id and hm_id and slot_id), f"HTTP {st1}/{st2}/{st3}")
    if not (hdr_id and hm_id and slot_id):
        return
    wf = {"hdr": workflow("header", hdr_id, "/orders"), "hm": workflow("hmac", hm_id, "/signed"),
          "slot": workflow("slot", slot_id, "/slot")}
    if not all(wf.values()):
        raise EnvError("không tạo được đủ ba luật dùng khoá")

    def staff_save(label, expect):
        """A non-admin employee saves a contact; wait for every rule in `expect` to reach the partner."""
        st, body = run.call("POST", f"/{ENTITY}", run.staff, {"first_name": "Kiểm Thử", "last_name": f"{mark}-{label}"})
        rid = (body.get("data") or body).get("id")
        if rid:
            run.on_cleanup(f"bản ghi {rid}", lambda: run.call("DELETE", f"/{ENTITY}/{rid}", run.admin)[0] in (200, 204))
        if st not in (200, 201) or not rid:
            raise EnvError(f"nhân viên không lưu được {ENTITY} ({st} {body.get('code', '')})")
        deadline = time.time() + max(run.a.wait, 10)
        while time.time() < deadline and not all(rx.for_run(wf[k], rid) for k in expect):
            time.sleep(0.1)
        time.sleep(1.5)  # settle: a duplicate must be visible before we count
        return {k: rx.for_run(wf[k], rid) for k in wf}

    def signed_ok(hit, key, prev=None):
        """V1 / V2 under `key`, V2-Previous under `prev` (None means the header must be absent)."""
        raw, ts = hit["body"], header(hit, "X-Nexus-Timestamp")
        v1, v2 = header(hit, "X-Nexus-Signature"), header(hit, "X-Nexus-Signature-V2")
        v2p = header(hit, "X-Nexus-Signature-V2-Previous")
        good = (v1 is not None and hmac.compare_digest(v1, sig_v1(key, raw)) and ts is not None and ts.isdigit()
                and v2 is not None and hmac.compare_digest(v2, sig_v2(key, ts, raw)))
        if prev is None:
            return good and v2p is None
        return good and v2p is not None and hmac.compare_digest(v2p, sig_v2(prev, ts, raw))

    hits = staff_save("truoc", ("hdr", "hm"))
    run.check("S4.2 trước khi xoay: header token và khoá HMAC cũ đi ra đúng, chưa có V2-Previous",
              len(hits["hdr"]) == 1 and header(hits["hdr"][0], "Authorization") == "Bearer " + tok1
              and len(hits["hm"]) == 1 and signed_ok(hits["hm"][0], key1) and header(hits["hm"][0], "X-Doi-Tac") == extra,
              f"hdr {len(hits['hdr'])}, hmac {len(hits['hm'])}")

    # --- header token: the very next save carries the new token (< 30 s) ---
    st, resp = run.call("POST", f"/admin/credentials/{hdr_id}/rotate", cadm, {"secret": tok2})
    rotated_at = time.time()
    cred = (resp.get("data") or {}).get("credential") or {}
    run.check("S4.3 quản trị khách xoay khoá header token ⇒ 200, trạng thái ready, response KHÔNG trả lại khoá tự gõ",
              st == 200 and cred.get("status") == "ready" and bool(cred.get("rotatedAt"))
              and tok2 not in json.dumps(resp, ensure_ascii=False) and "secret" not in (resp.get("data") or {}),
              f"HTTP {st} {resp.get('code', '')}")
    hits = staff_save("sau-header", ("hdr", "hm"))
    got = header(hits["hdr"][0], "Authorization") if hits["hdr"] else None
    run.check("S4.4 lần lưu KẾ TIẾP của test@example.com trong < 30 s mang token MỚI (không còn token cũ)",
              len(hits["hdr"]) == 1 and got == "Bearer " + tok2 and hits["hdr"][0]["at"] - rotated_at < 30,
              f"{len(hits['hdr'])} request, " + ("vắng" if got is None else "token mới" if got == "Bearer " + tok2 else "token cũ/sai")
              + (f", sau {hits['hdr'][0]['at'] - rotated_at:.1f} s" if hits["hdr"] else ""))

    # --- hmac key: new key signs V2, old key signs V2-Previous for the grace ---
    st, resp = run.call("POST", f"/admin/credentials/{hm_id}/rotate", cadm, {"generateSecret": True, "graceHours": 1})
    gen = (resp.get("data") or {}).get("secret") or ""
    cred = (resp.get("data") or {}).get("credential") or {}
    try:
        left = (datetime.datetime.fromisoformat((cred.get("previousExpiresAt") or "").replace("Z", "+00:00"))
                - datetime.datetime.now(datetime.timezone.utc)).total_seconds()
    except ValueError:
        left = -1
    run.check("S4.5 xoay khoá HMAC tự sinh với graceHours=1 ⇒ 200, khoá mới (nxw_) hiện MỘT lần, trạng thái grace, hết ân hạn sau ~1 giờ",
              st == 200 and gen.startswith("nxw_") and cred.get("status") == "grace" and 3000 < left < 3700,
              f"HTTP {st} {resp.get('code', '')} còn {left:.0f} s")
    hits = staff_save("sau-hmac", ("hdr", "hm"))
    run.check("S4.6 receiver thấy V2 bằng khoá MỚI và V2-Previous bằng khoá CŨ (V1 bằng khoá mới), header bổ sung còn nguyên",
              len(hits["hm"]) == 1 and bool(gen) and signed_ok(hits["hm"][0], gen, prev=key1)
              and header(hits["hm"][0], "X-Doi-Tac") == extra, f"{len(hits['hm'])} request")

    # --- slot: nothing is sent until the secret is entered; entering it by rotation makes it ready ---
    with rx.lock:
        slot_before = [h for h in rx.hits if h["path"].startswith("/slot")]
    found, until = False, time.time() + 25
    while time.time() < until and not found:
        found = any("CREDENTIAL_NOT_READY" in t for t in job_errors(a, wf["slot"]))
        if not found:
            st_r, runs = run.call("GET", f"/admin/workflows/{wf['slot']}/runs", run.admin)
            found = st_r == 200 and "CREDENTIAL_NOT_READY" in json.dumps(runs, ensure_ascii=False)
        time.sleep(0.5)
    run.check("S4.7 chỗ khoá chưa nhập bí mật ⇒ đối tác nhận 0 request /slot, lần gửi ghi CREDENTIAL_NOT_READY",
              not slot_before and found, f"{len(slot_before)} request; mã {'có' if found else 'chưa thấy sau 25 s'}")
    st, resp = run.call("POST", f"/admin/credentials/{slot_id}/rotate", cadm, {"secret": key2})
    cred = (resp.get("data") or {}).get("credential") or {}
    run.check("S4.8 nhập khoá qua xoay ⇒ 200, chỗ khoá thành ready, không có khoá cũ (previousExpiresAt rỗng)",
              st == 200 and cred.get("status") == "ready" and not cred.get("previousExpiresAt"), f"HTTP {st} {resp.get('code', '')}")
    hits = staff_save("sau-slot", ("hdr", "hm", "slot"))
    run.check("S4.9 lần lưu kế tiếp: chỗ khoá vừa nhập gửi có ký (V1 + V2 bằng khoá vừa nhập), không có V2-Previous",
              len(hits["slot"]) == 1 and signed_ok(hits["slot"][0], key2), f"{len(hits['slot'])} request")

    # --- host change: only through rotation, with a fresh secret, leaving the old material behind ---
    other = rx.base.replace("127.0.0.1", "localhost")
    st, resp = run.call("PUT", f"/admin/credentials/{hm_id}", cadm, {"baseUrl": other})
    run.check("S4.10 PUT đổi máy chủ ⇒ 422 CREDENTIAL_ORIGIN_CHANGE_REQUIRES_ROTATE",
              st == 422 and resp.get("code") == "CREDENTIAL_ORIGIN_CHANGE_REQUIRES_ROTATE", f"HTTP {st} {resp.get('code', '')}")
    st_a, resp_a = run.call("POST", f"/admin/credentials/{hm_id}/rotate", cadm, {"baseUrl": other, "extraHeaders": {}})
    st_b, resp_b = run.call("POST", f"/admin/credentials/{hm_id}/rotate", cadm, {"baseUrl": other, "secret": key3, "graceHours": 1})
    run.check("S4.11 xoay đổi máy chủ KHÔNG kèm khoá mới ⇒ 422 CREDENTIAL_SECRET_REQUIRED; kèm ân hạn > 0 ⇒ 422 CREDENTIAL_GRACE_INVALID",
              (st_a, resp_a.get("code"), st_b, resp_b.get("code")) == (422, "CREDENTIAL_SECRET_REQUIRED", 422, "CREDENTIAL_GRACE_INVALID"),
              f"HTTP {st_a} {resp_a.get('code', '')} / {st_b} {resp_b.get('code', '')}")
    st, resp = run.call("POST", f"/admin/credentials/{hm_id}/rotate", cadm, {"baseUrl": other, "secret": key3})
    cred = (resp.get("data") or {}).get("credential") or {}
    run.check("S4.12 xoay đổi máy chủ kèm khoá mới ⇒ 200, ready, không còn ân hạn, không còn header bổ sung cũ",
              st == 200 and cred.get("baseUrl") == other and cred.get("status") == "ready" and not cred.get("previousExpiresAt")
              and not cred.get("extraHeaderNames"), f"HTTP {st} {resp.get('code', '')}")
    hits = staff_save("sau-host", ("hdr", "hm", "slot"))
    h = hits["hm"][0] if len(hits["hm"]) == 1 else None
    run.check("S4.13 máy chủ mới nhận khoá MỚI (V2 bằng khoá vừa nhập, không V2-Previous) và KHÔNG nhận header bổ sung cũ",
              h is not None and (header(h, "Host") or "").startswith("localhost") and signed_ok(h, key3)
              and header(h, "X-Doi-Tac") is None, f"{len(hits['hm'])} request" + (f", Host {header(h, 'Host')}" if h else ""))

    # --- the wall + what the trail and the reads carry ---
    st, _ = run.call("POST", f"/admin/credentials/{hdr_id}/rotate", run.staff, {"secret": secrets.token_urlsafe(24)})
    run.check("S4.14 nhân viên không-admin xoay khoá ⇒ 403 (cặp với 200 của quản trị khách ở S4.3, cùng đường dẫn)",
              st == 403, f"HTTP {st}")
    st_audit, audit = run.call("GET", f"/audit-log?entityName={AUDIT_ENTITY}", cadm)
    rows = (audit.get("data") or []) if st_audit == 200 else []
    rotates = [r for r in rows if r.get("action") == "rotate"]
    by_key = {k: sum(1 for r in rotates if r.get("recordId") == v) for k, v in (("hdr", hdr_id), ("hm", hm_id), ("slot", slot_id))}
    run.check("S4.15 nhật ký kiểm toán có dòng `rotate` của từng khoá đã xoay (header 1, HMAC 2, chỗ khoá 1)",
              by_key["hdr"] >= 1 and by_key["hm"] >= 2 and by_key["slot"] >= 1, f"HTTP {st_audit}, dòng rotate theo khoá: {by_key}")
    blobs = [json.dumps(audit, ensure_ascii=False)]
    for tok, path in ((cadm, "/admin/credentials"), (cadm, f"/admin/credentials/{hm_id}"), (cadm, f"/admin/credentials/{hdr_id}"),
                      (run.admin, "/admin/workflows"), (run.admin, f"/admin/workflows/{wf['hm']}/runs"),
                      (run.admin, f"/admin/workflows/{wf['hdr']}/runs")):
        blobs.append(json.dumps(run.call("GET", path, tok)[1], ensure_ascii=False))
    blobs += job_errors(a, wf["hdr"]) + job_errors(a, wf["hm"]) + job_errors(a, wf["slot"])
    leaked = [n for n, v in (("token cũ", tok1), ("token mới", tok2), ("khoá HMAC cũ", key1), ("khoá HMAC tự sinh", gen),
                              ("khoá của chỗ khoá", key2), ("khoá sau đổi máy chủ", key3), ("giá trị header bổ sung", extra))
              if v and any(v in b for b in blobs)]
    run.check("S4.16 đọc kho khoá / nhật ký kiểm toán / lịch sử chạy / sys_jobs sau các lần xoay không lộ khoá, token hay header bổ sung nào",
              not leaked and bool(rotates), ", ".join(leaked) or ("sạch" if rotates else "nhật ký không có dòng rotate — không có gì để chứng minh"))


def slice_s5(run, rx):
    """url_token (tasks T089, quickstart §4 S5): the key rides in the PATH (Telegram /bot<TOKEN>/…, Slack
    incoming webhooks). The customer admin stores `<base>/bot{secret}`; the partner receives the token in
    the path; every place Nexus WRITES the target — the credential list, errors, run history, sys_jobs, the
    dry-run, the audit trail — shows `{secret}` only."""
    a = run.a
    if not CLONE_DB_RE.match(a.db or ""):
        raise EnvError("nhóm S5 cần --db nexus_wt_<slug> — đọc sys_jobs.last_error khi đối tác tắt")
    mark = new_mark()
    cadm = run.customer_admin(mark)
    tok1 = "tg" + secrets.token_hex(10) + ":" + secrets.token_urlsafe(18)  # Telegram-like, has a colon
    tok2 = "T0/" + secrets.token_hex(6) + "%x"  # a "/" and a "%" must be path-escaped, not split the path
    tok3 = secrets.token_urlsafe(24)
    with socket.socket() as sk:  # a port nobody listens on: the "partner is down" case
        sk.bind(("127.0.0.1", 0))
        dead = f"http://127.0.0.1:{sk.getsockname()[1]}"

    def credential(suffix, base, secret):
        body = {"authType": "url_token", "name": f"{mark}-{suffix}", "label": f"Nghiệm thu S5 {suffix}", "baseUrl": base}
        if secret:
            body["secret"] = secret
        st, resp = run.call("POST", "/admin/credentials", cadm, body)
        cred = (resp.get("data") or {}).get("credential") or {}
        if cred.get("id"):
            cid = cred["id"]
            run.on_cleanup(f"khoá {cid}", lambda: run.call("DELETE", f"/admin/credentials/{cid}", run.admin)[0] in (204, 404))
        return st, cred, resp

    def workflow(suffix, cid, path):
        st, resp = run.call("POST", "/admin/workflows", run.admin,
                            {"name": f"zz nghiệm thu S5 {suffix} {mark}", "entityName": ENTITY, "trigger": "after_create",
                             "actions": [{"type": "outbound_webhook", "credentialId": cid, "path": path}]})
        wid = ((resp.get("data") or {}).get("workflow") or {}).get("id")
        if wid:
            run.on_cleanup(f"luật {wid}", lambda: run.call("DELETE", f"/admin/workflows/{wid}", run.admin)[0] in (200, 204, 404))
        return wid

    bad = [("thiếu {secret}", rx.base + "/bot"), ("hai {secret}", rx.base + "/{secret}/{secret}"),
           ("{secret} ở tên miền", "http://{secret}.127.0.0.1:1/bot")]
    codes = [run.call("POST", "/admin/credentials", cadm, {"authType": "url_token", "name": f"{mark}-bad{i}", "baseUrl": b,
                                                           "secret": tok3})[1].get("code") for i, (_, b) in enumerate(bad)]
    run.check("S5.1 base url_token thiếu / thừa / sai chỗ {secret} ⇒ 422 CREDENTIAL_BASE_URL_INVALID, không lưu gì",
              all(c == "CREDENTIAL_BASE_URL_INVALID" for c in codes), f"mã: {codes}")

    st1, c1, r1 = credential("tele", rx.base + "/bot{secret}", tok1)
    st2, c2, r2 = credential("slack", rx.base + "/services/{secret}", tok2)
    st3, c3, r3 = credential("down", dead + "/bot{secret}", tok3)
    run.check("S5.2 quản trị khách cất ba khoá url_token ⇒ 201, địa chỉ gốc vẫn giữ {secret}, trạng thái ready, không trả lại token",
              (st1, st2, st3) == (201, 201, 201) and c1.get("baseUrl") == rx.base + "/bot{secret}"
              and c1.get("status") == "ready" and not any(t in json.dumps(r, ensure_ascii=False) for r in (r1, r2, r3) for t in (tok1, tok2, tok3)),
              f"HTTP {st1}/{st2}/{st3}")
    if not (c1.get("id") and c2.get("id") and c3.get("id")):
        return
    wf = {"tele": workflow("tele", c1["id"], "/sendMessage"), "slack": workflow("slack", c2["id"], "/T"),
          "down": workflow("down", c3["id"], "/sendMessage")}
    if not all(wf.values()):
        raise EnvError("không tạo được đủ ba luật dùng khoá")

    st, body = run.call("POST", f"/{ENTITY}", run.staff, {"first_name": "Kiểm Thử", "last_name": f"{mark}-s5"})
    rid = (body.get("data") or body).get("id")
    if rid:
        run.on_cleanup(f"bản ghi {rid}", lambda: run.call("DELETE", f"/{ENTITY}/{rid}", run.admin)[0] in (200, 204))
    if st not in (200, 201) or not rid:
        raise EnvError(f"nhân viên không lưu được {ENTITY} ({st} {body.get('code', '')})")
    deadline = time.time() + max(a.wait, 10)
    while time.time() < deadline and not (rx.for_run(wf["tele"], rid) and rx.for_run(wf["slack"], rid)):
        time.sleep(0.1)
    time.sleep(1.5)  # settle: a duplicate must be visible before we count
    h1, h2 = rx.for_run(wf["tele"], rid), rx.for_run(wf["slack"], rid)
    want1 = "/bot" + urllib.parse.quote(tok1, safe=":") + "/sendMessage"
    run.check("S5.3 test@example.com lưu liên hệ ⇒ receiver nhận ĐÚNG một POST /bot<token>/sendMessage, không Authorization, không chữ ký",
              len(h1) == 1 and h1[0]["path"] == want1 and header(h1[0], "Authorization") is None
              and header(h1[0], "X-Nexus-Signature") is None,
              f"{len(h1)} request" + (", đường dẫn khớp" if h1 and h1[0]["path"] == want1 else ", đường dẫn không khớp"))
    want2 = "/services/" + urllib.parse.quote(tok2, safe="") + "/T"
    run.check("S5.4 token có / và % đến nơi dạng đã mã hoá đoạn đường dẫn (không tách thêm đoạn)",
              len(h2) == 1 and h2[0]["path"] == want2, f"{len(h2)} request" + (", khớp" if h2 and h2[0]["path"] == want2 else ", không khớp"))

    st, dry = run.call("POST", f"/admin/workflows/{wf['tele']}/test", run.admin, {"recordId": rid})
    steps = ((dry.get("data") or {}).get("test") or {}).get("trace") or []
    web = next((x.get("detail") or {} for x in steps if (x.get("detail") or {}).get("type") == "outbound_webhook"), {})
    run.check("S5.5 chạy thử hiện địa chỉ có {secret}, không có token, wouldSign=false",
              st == 200 and web.get("url") == rx.base + "/bot{secret}/sendMessage" and web.get("wouldSign") is False
              and tok1 not in json.dumps(dry, ensure_ascii=False), f"HTTP {st} url={web.get('url')}")

    found, blobs, until = False, [], time.time() + 25
    while time.time() < until and not found:
        blobs = job_errors(a, wf["down"])
        st_r, runs = run.call("GET", f"/admin/workflows/{wf['down']}/runs", run.admin)
        blobs.append(json.dumps(runs, ensure_ascii=False) if st_r == 200 else "")
        found = any("{secret}" in b for b in blobs)
        if not found:
            time.sleep(0.5)
    run.check("S5.6 đối tác tắt ⇒ lỗi trong sys_jobs / lịch sử chạy chỉ nói {secret}, không chứa token",
              found and not any(tok3 in b for b in blobs), "có {secret}" if found else "chưa thấy lỗi sau 25 s")

    st, _ = run.call("POST", "/admin/credentials", run.staff, {"authType": "url_token", "name": f"{mark}-staff",
                                                              "baseUrl": rx.base + "/bot{secret}", "secret": tok3})
    run.check("S5.7 nhân viên không-admin tạo khoá ⇒ 403 (cặp với 201 của quản trị khách ở S5.2, cùng đường dẫn)",
              st == 403, f"HTTP {st}")

    st_audit, audit = run.call("GET", f"/audit-log?entityName={AUDIT_ENTITY}", cadm)
    all_blobs = [json.dumps(audit, ensure_ascii=False)] + blobs
    for tok, path in ((cadm, "/admin/credentials"), (cadm, f"/admin/credentials/{c1['id']}"), (run.admin, "/admin/workflows"),
                      (run.admin, f"/admin/workflows/{wf['tele']}/runs"), (run.admin, f"/admin/workflows/{wf['slack']}/runs")):
        all_blobs.append(json.dumps(run.call("GET", path, tok)[1], ensure_ascii=False))
    leaked = [n for n, v in (("token Telegram", tok1), ("token Slack", tok2), ("token của đối tác tắt", tok3))
              if any(v in b for b in all_blobs)]
    rows = (audit.get("data") or []) if st_audit == 200 else []
    run.check("S5.8 đọc kho khoá / nhật ký kiểm toán / lịch sử chạy / sys_jobs không lộ token nào",
              not leaked and any(r.get("recordId") == c1["id"] for r in rows),
              ", ".join(leaked) or ("sạch" if rows else "nhật ký trống — không có gì để chứng minh"))


def slice_s6(run, rx):
    """Pack carries the key SLOT, not the key (tasks T094, quickstart §4 S6): the Nexus builder exports a
    pack from the tenant that owns a rule using a vault key; the pack file holds the key's NAME and
    definition only; installing it into ANOTHER tenant (operator ?targetTenant=) makes an empty slot
    there, repoints the rule at THAT tenant's key and says aloud that the secret is still to be entered.
    The source tenant's key keeps working for its own employees."""
    a = run.a
    if not CLONE_DB_RE.match(a.db or ""):
        raise EnvError("nhóm S6 cần --db nexus_wt_<slug> — đọc tổ chức đích trên DB bản sao")
    mark = new_mark()
    if not re.fullmatch(r"[A-Za-z0-9-]+", mark):
        raise EnvError("dấu nghiệm thu có ký tự lạ — không ghép vào câu SQL")
    cadm = run.customer_admin(mark)
    token = "s6" + secrets.token_hex(12)
    name = f"{mark}-pay"

    st, resp = run.call("POST", "/admin/credentials", cadm, {"authType": "header", "name": name, "label": "Nghiệm thu S6",
                                                             "baseUrl": rx.base, "secret": token})
    src = (resp.get("data") or {}).get("credential") or {}
    if st != 201 or not src.get("id"):
        raise EnvError(f"không cất được khoá nguồn ({st} {resp.get('code', '')})")
    run.on_cleanup(f"khoá {src['id']}", lambda: run.call("DELETE", f"/admin/credentials/{src['id']}", run.admin)[0] in (204, 404))
    rule_name = f"zz nghiệm thu S6 {mark}"
    st, resp = run.call("POST", "/admin/workflows", run.admin,
                        {"name": rule_name, "entityName": ENTITY, "trigger": "after_create",
                         "actions": [{"type": "outbound_webhook", "credentialId": src["id"], "path": "/orders"}]})
    wid = ((resp.get("data") or {}).get("workflow") or {}).get("id")
    if not wid:
        raise EnvError(f"không tạo được luật nguồn ({st} {resp.get('code', '')})")
    run.on_cleanup(f"luật {wid}", lambda: run.call("DELETE", f"/admin/workflows/{wid}", run.admin)[0] in (200, 204, 404))

    st, exp = run.call("GET", f"/admin/verticals/export?entities={ENTITY}", run.admin)
    pack = ((exp.get("data") or {}).get("pack")) or {}
    raw = json.dumps(pack, ensure_ascii=False)
    slot = next((c for c in pack.get("credentials") or [] if c.get("name") == name), {})
    mine = next((w for w in pack.get("workflows") or [] if w.get("name") == rule_name), {})
    act = (mine.get("actions") or [{}])[0]
    run.check("S6.1 xuất gói: file chỉ có TÊN khoá + định nghĩa chỗ khoá, 0 bí mật / id khoá / giá trị header",
              st == 200 and slot.get("baseUrl") == rx.base and slot.get("authType") == "header"
              and act.get("credentialName") == name and act.get("path") == "/orders"
              and token not in raw and src["id"] not in raw and "credentialId" not in json.dumps(mine) and "secretHint" not in raw,
              f"HTTP {st}, {len(raw)} byte")

    # Install into another tenant: only our rule + a one-field entity, so the proof does not hang on
    # whatever else tenant A's contacts drag in.
    target = "beta"
    rows = clone_sql(a, "SELECT id FROM tenants WHERE slug = '" + target + "'")
    if not rows:
        raise EnvError(f"DB bản sao không có tổ chức {target!r} để cài thử")
    tid = rows[0][0]
    probe = f"zz_s6_{mark.split('-')[-1]}"
    small = {"entities": [{"name": probe, "label": "S6", "fields": [{"name": "title", "label": "Tên", "fieldType": "text"}]}],
             "credentials": pack.get("credentials"),
             "workflows": [dict(mine, entityName=probe)]}
    st, inst = run.call("POST", f"/admin/verticals/install?targetTenant={target}", run.admin, {"pack": small})
    warns = " | ".join((((inst.get("data") or {}).get("result") or {}).get("warnings")) or [])
    run.check("S6.2 cài vào tổ chức khác: thấy cảnh báo 'Nhập bí mật ở Quản trị › Kho khoá kết nối' nêu đúng tên khoá",
              st in (200, 201) and "Nhập bí mật ở Quản trị › Kho khoá kết nối" in warns and name in warns,
              f"HTTP {st} {inst.get('code', '')}")
    b_key = clone_sql(a, "SELECT id, secret_enc IS NULL FROM sys_named_credentials WHERE tenant_id = '%s' AND deleted_at IS NULL "
                         "AND name = '" + name + "'", tid)
    b_rule = clone_sql(a, "SELECT id FROM sys_workflows WHERE tenant_id = '%s' AND deleted_at IS NULL AND name = '" + rule_name + "'", tid)
    b_actions = stored_actions(a, tid, b_rule[0][0]) if b_rule else None
    run.check("S6.3 tổ chức đích có chỗ khoá RỖNG (chưa bí mật), id KHÁC khoá nguồn; luật trỏ khoá của NÓ, không trỏ id nguồn",
              len(b_key) == 1 and b_key[0][1] == "1" and b_key[0][0] != src["id"] and b_actions
              and b_actions[0].get("credentialId") == b_key[0][0] and src["id"] not in json.dumps(b_actions),
              f"khoá đích {len(b_key)}, luật đích {len(b_rule)}")

    st, body = run.call("POST", f"/{ENTITY}", run.staff, {"first_name": "Kiểm Thử", "last_name": f"{mark}-s6"})
    rid = (body.get("data") or body).get("id")
    if rid:
        run.on_cleanup(f"bản ghi {rid}", lambda: run.call("DELETE", f"/{ENTITY}/{rid}", run.admin)[0] in (200, 204))
    until = time.time() + max(a.wait, 10)
    while time.time() < until and not rx.for_run(wid, rid):
        time.sleep(0.1)
    hits = rx.for_run(wid, rid)
    run.check("S6.4 tổ chức nguồn không hỏng: test@example.com lưu liên hệ ⇒ receiver nhận 1 request mang khoá của nguồn",
              st in (200, 201) and len(hits) == 1 and header(hits[0], "Authorization") == "Bearer " + token, f"{len(hits)} request")
    st_s, _ = run.call("GET", "/admin/verticals/export?entities=" + ENTITY, run.staff)
    run.check("S6.5 nhân viên không-admin xuất gói ⇒ 403 (cặp với 200 của người dựng ở S6.1, cùng đường dẫn)", st_s == 403, f"HTTP {st_s}")


def slice_s7(run, rx):
    """Master-key rotation, loss and the deploy gate (tasks T103, quickstart §4 S7). The server at --base
    holds key A (env CREDENTIAL_ENC_KEY exported to this script too). A rule sends through a vault key; then
    private servers on the same --db clone play the operator's steps:
      B  = key B active + A retired  ⇒ rows are re-sealed under B, the employee's save still signs, Doctor C11 = 0;
      C  = key B alone (RETIRED removed) ⇒ still signs;
      D  = ANOTHER key under the SAME label v1 (a DB restored into the wrong environment) ⇒ Doctor C12 names the
           key (critical), the employee's save sends NOTHING, the run says CREDENTIAL_KEY_UNAVAILABLE.
    The real deploy/nexus/scripts/admin-precheck.sh runs against the clone (docker shim) and must exit 0 while
    every sealed id is producible from the env and 1 once it is not — including label v1 over a different key."""
    a = run.a
    if not CLONE_DB_RE.match(a.db or ""):
        raise EnvError("nhóm S7 cần --db nexus_wt_<slug> — đọc key_id của khoá trên DB bản sao")
    for need in ("CREDENTIAL_ENC_KEY", "JWT_SECRET"):
        if not os.environ.get(need):
            raise EnvError(f"nhóm S7 cần {need} trong env (cùng khoá với server ở --base) — `set -a && . ./.env && set +a`")
    if not os.path.isfile(a.binary):
        raise EnvError(f"không thấy {a.binary} — go build -o bin/server ./cmd/server/ trước.")
    a.clone_dsn = clone_dsn(os.environ.get("DATABASE_URL", ""), a.db)
    mark = new_mark()
    label_a = os.environ.get("CREDENTIAL_ENC_KEY_ID", "v1")
    key_a = os.environ["CREDENTIAL_ENC_KEY"]
    key_b = base64.b64encode(secrets.token_bytes(32)).decode()
    key_x = base64.b64encode(secrets.token_bytes(32)).decode()
    id_a, id_b = master_key_id(label_a, key_a), master_key_id("v2", key_b)
    insecure = {"CREDENTIAL_ALLOW_INSECURE_HTTP": "true"}  # the receiver is http://127.0.0.1

    cadm = run.customer_admin(mark)
    token = "s7" + secrets.token_hex(12)
    name = f"{mark}-pay"
    st, resp = run.call("POST", "/admin/credentials", cadm, {"authType": "header", "name": name, "label": "Nghiệm thu S7",
                                                             "baseUrl": rx.base, "secret": token})
    cred = (resp.get("data") or {}).get("credential") or {}
    if st != 201 or not cred.get("id"):
        raise EnvError(f"không cất được khoá ({st} {resp.get('code', '')})")
    cid = cred["id"]
    run.on_cleanup(f"khoá {cid}", lambda: run.call("DELETE", f"/admin/credentials/{cid}", run.admin)[0] in (204, 404))
    st, resp = run.call("POST", "/admin/workflows", run.admin,
                        {"name": f"zz nghiệm thu S7 {mark}", "entityName": ENTITY, "trigger": "after_create",
                         "actions": [{"type": "outbound_webhook", "credentialId": cid, "path": "/orders"}]})
    wid = ((resp.get("data") or {}).get("workflow") or {}).get("id")
    if not wid:
        raise EnvError(f"không tạo được luật ({st} {resp.get('code', '')})")
    run.on_cleanup(f"luật {wid}", lambda: run.call("DELETE", f"/admin/workflows/{wid}", run.admin)[0] in (200, 204, 404))

    def row_key_id():
        rows = clone_sql(a, "SELECT key_id FROM sys_named_credentials WHERE id = '%s' AND deleted_at IS NULL", cid)
        return rows[0][0] if rows else ""

    def staff_save(base, label, expect_hits):
        """The employee saves a contact on the server at `base`; returns the receiver's hits for that record."""
        st, body = call(base, "POST", "/auth/login", body={"tenant": a.tenant, "email": a.staff,
                                                           "password": os.environ.get("NEXUS_STAFF_PASS", "Demo-Rep-2026!")})
        tok = (body.get("data") or {}).get("accessToken")
        if st != 200 or not tok:
            return None
        st, body = call(base, "POST", f"/{ENTITY}", tok, {"first_name": "Kiểm Thử", "last_name": f"{mark}-{label}"})
        rid = (body.get("data") or body).get("id")
        if rid:
            run.on_cleanup(f"bản ghi {rid}", lambda: run.call("DELETE", f"/{ENTITY}/{rid}", run.admin)[0] in (200, 204))
        if st not in (200, 201) or not rid:
            return None
        until = time.time() + (max(a.wait, 30) if expect_hits else 4)  # a fresh server polls sys_jobs every 5 s, slower under load
        while time.time() < until and not rx.for_run(wid, rid):
            time.sleep(0.1)
        time.sleep(1.0)
        return rx.for_run(wid, rid)

    def scan_items(base, check_prefix):
        st, body = call(base, "POST", "/auth/login", body={"tenant": a.tenant, "email": a.admin,
                                                           "password": os.environ.get("NEXUS_ADMIN_PASS", "Demo-Admin-2026!")})
        tok = (body.get("data") or {}).get("accessToken")
        if st != 200 or not tok:
            return None, None
        st, scan = call(base, "POST", "/admin/doctor/scan", tok)
        f = next((f for f in ((scan.get("data") or {}).get("findings") or []) if (f.get("checkId") or "").startswith(check_prefix)), None)
        return f, tok

    def precheck(label, env):
        """Run the REAL admin-precheck.sh against the clone: a docker shim stands in for the prod container."""
        script = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "deploy", "nexus", "scripts", "admin-precheck.sh")
        real = subprocess.run(["which", "docker"], capture_output=True, text=True).stdout.strip()
        if not real:
            raise EnvError("không thấy docker để chạy precheck")
        with tempfile.TemporaryDirectory(prefix="nc-s7-shim-") as d:
            shim = os.path.join(d, "docker")
            with open(shim, "w") as f:
                f.write(f'#!/usr/bin/env bash\nexec "{real}" exec -i {a.mysql_container} mysql -uroot '
                        f'--default-character-set=utf8mb4 -N -B {a.db}\n')
            os.chmod(shim, 0o755)
            e = {k: v for k, v in os.environ.items() if not k.startswith("CREDENTIAL_")}
            e.update(env, PATH=d + os.pathsep + e.get("PATH", ""))
            p = subprocess.run(["bash", script], env=e, capture_output=True, text=True, timeout=60)
        print(f"         [precheck {label}] exit {p.returncode}")
        return p.returncode, p.stdout + p.stderr

    A = {"CREDENTIAL_ENC_KEY": key_a, "CREDENTIAL_ENC_KEY_ID": label_a}

    # --- before rotation: key A serves, the precheck accepts exactly the env that holds A ---
    hits = staff_save(a.base, "a", True)
    run.check("S7.1 TRƯỚC khi xoay: nhân viên lưu liên hệ ⇒ đối tác nhận 1 request mang token của khoá",
              hits is not None and len(hits) == 1 and header(hits[0], "Authorization") == "Bearer " + token,
              "không lưu được" if hits is None else f"{len(hits)} request")
    run.check(f"S7.2 khoá đang niêm phong bằng mã khoá A ({id_a}) — nhãn + dấu vân tay", row_key_id() == id_a, row_key_id())
    rc_ok, _ = precheck("env giữ khoá A", A)
    rc_other, out_other = precheck("cùng nhãn, KHOÁ KHÁC", {"CREDENTIAL_ENC_KEY": key_x, "CREDENTIAL_ENC_KEY_ID": label_a})
    rc_none, _ = precheck("không có khoá nào", {})
    run.check("S7.3 precheck: env giữ khoá A ⇒ exit 0; cùng nhãn nhưng KHOÁ KHÁC ⇒ exit 1 nêu mã còn thiếu; không khoá ⇒ exit 1",
              rc_ok == 0 and rc_other == 1 and id_a in out_other and rc_none == 1, f"{rc_ok}/{rc_other}/{rc_none}")

    # --- B: new key active, A retired: boot reseal, still signs, C11 = 0 ---
    seen = {}

    def probe_b(base):
        until = time.time() + 40
        while time.time() < until and row_key_id() != id_b:
            time.sleep(0.5)
        seen["key"] = row_key_id()
        seen["hits"] = staff_save(base, "b", True)
        seen["c11"], _ = scan_items(base, "C11")
        seen["c12"], _ = scan_items(base, "C12")

    up, _, log = boot(a, dict(insecure, CREDENTIAL_ENC_KEY=key_b, CREDENTIAL_ENC_KEY_ID="v2",
                              CREDENTIAL_ENC_RETIRED_KEYS=f"{label_a}:{key_a}"), "khoá B chính + A trong RETIRED",
                      port=a.nokey_port, probe=probe_b)
    b_hits = seen.get("hits") or []
    run.check(f"S7.4 xoay khoá: khởi động niêm phong lại ⇒ key_id đổi sang {id_b}; log 'reseal done' (không lộ khoá)",
              up and seen.get("key") == id_b and "named-credential reseal done" in log and key_a not in log and key_b not in log,
              f"lên={up} key_id={seen.get('key')}")
    run.check("S7.5 … nhân viên lưu ⇒ đối tác VẪN nhận token đúng (khoá mở được sau khi xoay)",
              len(b_hits) == 1 and header(b_hits[0], "Authorization") == "Bearer " + token, f"{len(b_hits)} request")
    run.check("S7.6 … Doctor C11 về 0 và C12 = 0 (không dòng nào kẹt khoá cũ hay mất khoá)",
              up and (seen.get("c11") or {}).get("count") == 0 and (seen.get("c12") or {}).get("count") == 0,
              f"C11={(seen.get('c11') or {}).get('count')} C12={(seen.get('c12') or {}).get('count')}")
    rc_b, _ = precheck("khoá B + A trong RETIRED", {"CREDENTIAL_ENC_KEY": key_b, "CREDENTIAL_ENC_KEY_ID": "v2",
                                                     "CREDENTIAL_ENC_RETIRED_KEYS": f"{label_a}:{key_a}"})
    rc_b_only, _ = precheck("khoá B, đã gỡ RETIRED", {"CREDENTIAL_ENC_KEY": key_b, "CREDENTIAL_ENC_KEY_ID": "v2"})
    rc_a_only, out_a_only = precheck("chỉ còn khoá A (khoá mới bị mất)", A)
    run.check("S7.7 precheck sau xoay: B + RETIRED ⇒ 0; B một mình (đã gỡ A) ⇒ 0 (mọi dòng đã sang B); chỉ A ⇒ 1 nêu mã B",
              rc_b == 0 and rc_b_only == 0 and rc_a_only == 1 and id_b in out_a_only, f"{rc_b}/{rc_b_only}/{rc_a_only}")

    # --- C: RETIRED removed ---
    seen.clear()
    up, _, _ = boot(a, dict(insecure, CREDENTIAL_ENC_KEY=key_b, CREDENTIAL_ENC_KEY_ID="v2"), "chỉ khoá B",
                    port=a.nokey_port, probe=lambda base: seen.update(hits=staff_save(base, "c", True)))
    c_hits = seen.get("hits") or []
    run.check("S7.8 gỡ khoá cũ khỏi RETIRED rồi khởi động lại ⇒ nhân viên lưu, đối tác VẪN nhận token đúng",
              up and len(c_hits) == 1 and header(c_hits[0], "Authorization") == "Bearer " + token, f"lên={up} {len(c_hits)} request")

    # --- D: the DB restored where the label v1 is another key ---
    mark_pos = {}

    def probe_d(base):
        with rx.lock:
            mark_pos["before"] = len(rx.hits)
        mark_pos["hits"] = staff_save(base, "d", False)
        mark_pos["c12"], mark_pos["tok"] = scan_items(base, "C12")
        # The delivery job is claimed by THIS server's worker: read its verdict before the server stops.
        until = time.time() + 25
        while time.time() < until and not any("CREDENTIAL_KEY_UNAVAILABLE" in t for t in job_errors(a, wid)):
            time.sleep(0.5)

    up, _, _ = boot(a, dict(insecure, CREDENTIAL_ENC_KEY=key_x, CREDENTIAL_ENC_KEY_ID="v2"), "KHOÁ KHÁC (khôi phục sai môi trường)",
                    port=a.nokey_port, probe=probe_d)
    c12 = mark_pos.get("c12") or {}
    items = c12.get("items") or []
    run.check("S7.9 khôi phục sang env khoá khác: Doctor C12 nêu TÊN khoá, mức nghiêm trọng, chỉ báo cáo",
              up and c12.get("count", 0) >= 1 and any(name in (i.get("detail") or "") for i in items)
              and c12.get("severity") == "critical" and c12.get("reportOnly") is True,
              f"lên={up} count={c12.get('count')} sev={c12.get('severity')}")
    with rx.lock:
        new_orders = [h for h in rx.hits[mark_pos.get("before", 0):] if h["path"].startswith("/orders")]
    unavailable = any("CREDENTIAL_KEY_UNAVAILABLE" in t for t in job_errors(a, wid))
    run.check("S7.10 … nhân viên lưu ⇒ đối tác nhận 0 request (không bao giờ gửi không xác thực); lần gửi ghi CREDENTIAL_KEY_UNAVAILABLE",
              up and not mark_pos.get("hits") and not new_orders and unavailable,
              f"{len(new_orders)} request; mã {'có' if unavailable else 'không thấy'}")
    rc_x, out_x = precheck("DB mang mã B, env khoá X", {"CREDENTIAL_ENC_KEY": key_x, "CREDENTIAL_ENC_KEY_ID": "v2"})
    run.check("S7.11 precheck chặn deploy (exit 1, nêu mã B) khi env là khoá khác dù cùng nhãn v2", rc_x == 1 and id_b in out_x, f"exit {rc_x}")

    # Non-admin wall in a PAIR with the admin's 200 on the same path (B21).
    st_staff, _ = run.call("POST", "/admin/doctor/scan", run.staff)
    st_admin, _ = run.call("POST", "/admin/doctor/scan", run.admin)
    run.check("S7.12 Bác sĩ hệ thống: nhân viên không-admin ⇒ 403, người dựng ⇒ 200 (cùng đường dẫn)",
              st_staff == 403 and st_admin == 200, f"{st_staff}/{st_admin}")


GROUPS = {"S7": slice_s7, "S0": slice_s0, "S1b": slice_s1b, "S2a": slice_s2a, "S2b": slice_s2b, "S3a": slice_s3a, "S3b": slice_s3b, "S4": slice_s4, "S5": slice_s5, "S6": slice_s6}
BOOT_GROUPS = {"S1a": slice_s1a}  # start their own private servers — no login, no receiver


def main():
    ap = argparse.ArgumentParser(description="Nghiệm thu kho khoá kết nối (specs/050) — người thứ ba thật")
    ap.add_argument("--base", default="http://localhost:8090/api/v1")
    ap.add_argument("--tenant", default="northwind")
    ap.add_argument("--admin", default="admin@example.com", help="người dựng Nexus (operator)")
    ap.add_argument("--staff", default="test@example.com", help="nhân viên không-admin kích hoạt luật")
    ap.add_argument("--slice", default="S0", choices=SLICES)
    ap.add_argument("--wait", type=float, default=5.0, help="giây chờ receiver nhận POST (SC-004: ≤ 5)")
    ap.add_argument("--db", default="", help="(S1a, S1b, S2a, S2b, S3a, S3b, S4, S5, S6) DB bản sao nexus_wt_<slug> (server riêng; S1b/S2a/S2b/S3a/S3b đọc/ghi DB)")
    ap.add_argument("--binary", default="bin/server", help="(S1a, S1b, S3b) server đã build, chạy từ nexus-platform/")
    ap.add_argument("--mysql-container", default="nexus-mysql", help="(S1b, S2a, S2b, S3a, S3b) container MySQL chứa --db")
    ap.add_argument("--nokey-port", type=int, default=0,
                    help="(S3b) cổng trống cho bản máy chủ riêng KHÔNG khoá chủ (mặc định: cổng của --base + 1)")
    a = ap.parse_args()
    a.base = a.base.rstrip("/")
    a.nokey_port = a.nokey_port or (urllib.parse.urlsplit(a.base).port or 8090) + 1

    group = GROUPS.get(a.slice) or BOOT_GROUPS.get(a.slice)
    if group is None:
        print(f"Nhóm kiểm của lát {a.slice} CHƯA VIẾT — lát đó mở rộng script này "
              f"(specs/050-named-credentials/tasks.md). Không phải kết quả đạt.", file=sys.stderr)
        return 2
    if not is_loopback_base(a.base):
        print(f"LỖI MÔI TRƯỜNG: --base {a.base} không phải loopback. Receiver nghe 127.0.0.1 và script "
              f"GHI vào tenant — chỉ chạy với server trên máy này.", file=sys.stderr)
        return 2

    # SIGTERM/SIGHUP default action skips `finally` (audit #78): turn them into SystemExit.
    for sig in (signal.SIGTERM, signal.SIGHUP):
        signal.signal(sig, lambda s, _f: sys.exit(128 + s))

    if a.slice in BOOT_GROUPS:
        print(f"Nghiệm thu kho khoá — lát {a.slice} · server RIÊNG {a.binary} trên {a.base} · DB {a.db}")
        try:
            if not os.path.isfile(a.binary):
                raise EnvError(f"không thấy {a.binary} — go build -o bin/server ./cmd/server/ trước.")
            if not os.environ.get("JWT_SECRET"):
                raise EnvError("env chưa có .env — chạy `set -a && . ./.env && set +a` trước.")
            try:
                a.clone_dsn = clone_dsn(os.environ.get("DATABASE_URL", ""), a.db)
            except ValueError as e:
                raise EnvError(str(e)) from e
            fails = group(a)
        except EnvError as e:
            print(f"LỖI MÔI TRƯỜNG: {e}", file=sys.stderr)
            return 2
        print()
        if fails:
            print(f"KẾT QUẢ lát {a.slice}: CHƯA ĐẠT ({len(fails)} điểm)")
            return 1
        print(f"KẾT QUẢ lát {a.slice}: ĐẠT")
        return 0

    print(f"Nghiệm thu kho khoá — lát {a.slice} · server {a.base} · tenant {a.tenant} (sẽ GHI rồi dọn)")
    rx = run = None
    dirty = []
    try:
        run = Run(a)
        if not run.sweep():
            dirty.append("quét tự lành đầu lượt")
        rx = Receiver()
        group(run, rx)
    except EnvError as e:
        print(f"LỖI MÔI TRƯỜNG: {e}", file=sys.stderr)
        return 2
    finally:
        if run:
            dirty += run.run_cleanup()
            try:
                if not run.sweep():
                    dirty.append("quét tự lành cuối lượt")
            except EnvError:
                dirty.append("quét tự lành cuối lượt")
            if dirty:
                print(f"⚠️  DỌN CHƯA SẠCH: {', '.join(dirty)} — xoá tay; lần chạy sau (≥ 15 phút) tự quét.",
                      file=sys.stderr)
        if rx:
            rx.close()

    print()
    if run.fails or dirty:
        print(f"KẾT QUẢ lát {a.slice}: CHƯA ĐẠT ({len(run.fails)} điểm"
              + (", dọn chưa sạch" if dirty else "") + ")")
        return 1
    print(f"KẾT QUẢ lát {a.slice}: ĐẠT")
    return 0


if __name__ == "__main__":
    sys.exit(main())
