#!/usr/bin/env python3
"""Fixture người duyệt KHÔNG-admin cho nghiệm thu mọi lát BPM (specs/043-bpm-process-engine, T013).

Vì sao cần: `is_admin` bỏ qua toàn bộ ACL, và admin còn được `CanDecide` cho quyết mọi bước duyệt.
Nghiệm thu bằng admin vì thế luôn xanh theo cấu tạo. Duyệt song song, định tuyến theo nhóm/hàng
đợi, "một người không quyết hai ghế"… chỉ chấm được khi có NHIỀU người duyệt thật, không-admin.

Script đưa tenant `northwind` về đúng trạng thái dưới đây (qua API admin, không chạm DB) — chỉ tạo/sửa
cái còn thiếu hoặc lệch, chạy lại bao nhiêu lần cũng được:
    duyet1@example.com, duyet2@example.com, duyet3@example.com  vai crm_sales_manager, không-admin, đang hoạt động
    hotro1@example.com                                  vai tk_agent (persona "tk_agent" của US8/US11), không-admin
    nhóm  "Ban duyệt"  gồm duyet1..3
    hàng đợi "ke_toan" (nhãn "Kế toán") trên crm_quotes, vai crm_sales_manager
Tài khoản/nhóm/hàng đợi ở trên là đồ CỦA fixture: lệch (thành admin, đổi vai, sai mật khẩu) thì bị
đưa về đúng. Không đối tượng dùng chung nào khác (vai, người dùng khác, nhóm/hàng đợi khác) bị sửa —
nên `finally` không có gì phải trả lại; nó chỉ báo phần đã làm nếu lượt chạy dừng giữa chừng.

Dùng (luôn chỉ rõ server — KHÔNG có mặc định, để lỡ tay không ghi vào DB dev dùng chung):
    python3 scripts/fixture-bpm.py --base http://localhost:8131/api/v1             # dựng / sửa lệch
    python3 scripts/fixture-bpm.py --base http://localhost:8131/api/v1 --check     # chỉ kiểm, không ghi
    python3 scripts/fixture-bpm.py --base http://localhost:8131/api/v1 --teardown  # gỡ đồ của fixture

Script nghiệm thu khác lấy phiên đăng nhập bằng cách nạp module này (importlib) rồi gọi
`login_as(base, email)` — mật khẩu chỉ nằm ở đây, không in ra, không đưa vào chat. Mọi lượt dựng/kiểm
kết thúc bằng đăng nhập THẬT từng tài khoản: đăng nhập được, không-admin, đúng vai.

Mã thoát: 0 = đạt · 1 = lệch (chế độ --check) hoặc đăng nhập kiểm không khớp · 2 = lỗi môi trường.

ponytail: stdlib thuần (urllib/argparse), cùng khuôn scripts/acceptance-acl.py.
"""

import argparse
import json
import sys
import urllib.error
import urllib.request

TENANT = "northwind"
ADMIN = ("admin@example.com", "Demo-Admin-2026!")  # the demo admin — same constant scripts/acceptance-acl.py uses
FIXTURE_PASSWORD = "Duyet#Bpm-2026"  # never printed; other scripts go through login_as()

APPROVERS = [f"duyet{i}@example.com" for i in (1, 2, 3)]
SUPPORT = "hotro1@example.com"
ACCOUNTS = {email: "crm_sales_manager" for email in APPROVERS} | {SUPPORT: "tk_agent"}
TEAM = "Ban duyệt"
QUEUE = {"name": "ke_toan", "label": "Kế toán", "entityName": "crm_quotes", "role": "crm_sales_manager"}


class EnvError(Exception):
    """The environment is not usable (server down, admin refused, a role missing) — exit 2."""


def _req(method, url, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.load(e)
        except Exception:
            return e.code, {}
    except Exception as e:  # connection refused, DNS, timeout…
        raise EnvError(f"không gọi được {url} — {e}") from e


def _call(method, url, token, body, ok, what):
    code, resp = _req(method, url, token, body)
    if code not in ok:
        raise EnvError(f"{what} → {code} {resp.get('code', '')} {resp.get('error', '')}")
    return resp.get("data") or {}


def _list(base, path, token, key):
    return _call("GET", base + path, token, None, (200,), "GET " + path).get(key) or []


def _login(base, email, password):
    code, body = _req("POST", base + "/auth/login", body={"tenant": TENANT, "email": email, "password": password})
    data = body.get("data") or {}
    if code != 200 or not data.get("accessToken"):
        return None, {"code": code, "err": body.get("code", "")}
    return data["accessToken"], data.get("user") or {}


def login_as(base, email):
    """Log a FIXTURE account in; returns (token, user). The password never leaves this module."""
    if email not in ACCOUNTS:
        raise ValueError(f"{email} không phải tài khoản fixture")
    token, user = _login(base, email, FIXTURE_PASSWORD)
    if token is None:
        raise EnvError(f"đăng nhập {email} thất bại ({user['code']} {user['err']}) — chạy lại fixture")
    return token, user


class Tenant:
    """The tenant as the admin API sees it — re-read after every write that changes it."""

    def __init__(self, base, adm):
        self.base, self.adm = base, adm
        self.roles = {r["name"]: r["id"] for r in _list(base, "/admin/roles", adm, "roles")}
        for need in set(ACCOUNTS.values()) | {QUEUE["role"]}:
            if need not in self.roles:
                raise EnvError(f"thiếu vai {need} trong tenant {TENANT} — cài gói CRM/Ticketing trước")
        self.users = {u["email"].lower(): u for u in _list(base, "/admin/users", adm, "users")}
        self.team = next((t for t in _list(base, "/admin/teams", adm, "teams") if t["name"] == TEAM), None)
        self.queue = next((q for q in _list(base, "/admin/work-queues", adm, "queues") if q["name"] == QUEUE["name"]), None)

    def account_drift(self, email):
        u, role = self.users.get(email), ACCOUNTS[email]
        if u is None:
            return "chưa có"
        if u.get("isAdmin") or u.get("roleId") != self.roles[role] or u.get("status") != "active":
            return f"lệch (isAdmin={u.get('isAdmin')}, trạng thái={u.get('status')}, đúng vai {role}: {u.get('roleId') == self.roles[role]})"
        return None

    def team_missing(self):
        if self.team is None:
            return list(APPROVERS)
        members = {m["email"].lower() for m in _list(self.base, f"/admin/teams/{self.team['id']}/members", self.adm, "members")}
        return [e for e in APPROVERS if e not in members]

    def queue_drift(self):
        q = self.queue
        return q is None or q.get("roleId") != self.roles[QUEUE["role"]] or q.get("entityName") != QUEUE["entityName"]


def converge(base, adm, done):
    t = Tenant(base, adm)
    for email, role in ACCOUNTS.items():
        drift = t.account_drift(email)
        if drift == "chưa có":
            _call("POST", base + "/admin/users", adm, {
                "email": email, "password": FIXTURE_PASSWORD, "fullName": "Nghiệm thu BPM — " + email.split("@")[0],
                "isAdmin": False, "roleId": t.roles[role]}, (201,), f"tạo {email}")
            done.append(f"tạo {email}")
        elif drift:
            _call("PUT", f"{base}/admin/users/{t.users[email]['id']}", adm, {
                "isAdmin": False, "roleId": t.roles[role], "status": "active"}, (200,), f"sửa {email}")
            done.append(f"sửa {email}")
    t = Tenant(base, adm)  # ids of the accounts just created
    if t.team is None:
        t.team = _call("POST", base + "/admin/teams", adm, {"name": TEAM}, (201,), "tạo nhóm").get("team") or {}
        done.append(f"tạo nhóm «{TEAM}»")
    for email in t.team_missing():
        _call("POST", f"{base}/admin/teams/{t.team['id']}/members", adm, {"userId": t.users[email]["id"]},
              (200, 201), f"thêm {email} vào nhóm")
        done.append(f"thêm {email} vào «{TEAM}»")
    if t.queue_drift():
        body = {"name": QUEUE["name"], "label": QUEUE["label"], "entityName": QUEUE["entityName"],
                "roleId": t.roles[QUEUE["role"]], "filters": []}
        if t.queue is None:
            _call("POST", base + "/admin/work-queues", adm, body, (201,), "tạo hàng đợi")
        else:
            _call("PUT", f"{base}/admin/work-queues/{t.queue['id']}", adm, body, (200,), "sửa hàng đợi")
        done.append(f"hàng đợi «{QUEUE['label']}»")
    return t


def verify(base, t, fix):
    """A REAL login per account: it works, it is not admin, it holds the expected role. With fix, a
    fixture account whose password drifted gets it reset once (it is the fixture's own account)."""
    bad = []
    for email, role in ACCOUNTS.items():
        token, user = _login(base, email, FIXTURE_PASSWORD)
        if token is None and fix and email in t.users:
            _call("PUT", f"{base}/admin/users/{t.users[email]['id']}", t.adm, {"password": FIXTURE_PASSWORD},
                  (200,), f"đặt lại mật khẩu {email}")
            token, user = _login(base, email, FIXTURE_PASSWORD)
        if token is None:
            bad.append(f"{email} không đăng nhập được ({user['code']} {user['err']})")
            continue
        if user.get("isAdmin"):
            bad.append(f"{email} là admin — chấm bằng tài khoản này là tự lừa")
        if user.get("roleId") != t.roles[role]:
            bad.append(f"{email} không mang vai {role}")
    return bad


def teardown(base, adm):
    """Remove ONLY fixture-owned objects (accounts, team, queue)."""
    t = Tenant(base, adm)
    if t.queue:
        _call("DELETE", f"{base}/admin/work-queues/{t.queue['id']}", adm, None, (200, 204), "xoá hàng đợi")
    if t.team:
        _call("DELETE", f"{base}/admin/teams/{t.team['id']}", adm, None, (200, 204), "xoá nhóm")
    for email in ACCOUNTS:
        if email in t.users:
            _call("DELETE", f"{base}/admin/users/{t.users[email]['id']}", adm, None, (200, 204), f"xoá {email}")


def main():
    ap = argparse.ArgumentParser(description="Fixture người duyệt không-admin cho nghiệm thu BPM (specs/043).")
    ap.add_argument("--base", required=True, help="vd http://localhost:8131/api/v1 (server trên DB bản sao)")
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--check", action="store_true", help="chỉ so, không ghi; lệch ⇒ thoát 1")
    mode.add_argument("--teardown", action="store_true", help="gỡ tài khoản/nhóm/hàng đợi của fixture")
    a = ap.parse_args()
    base = a.base.rstrip("/")

    done, finished = [], False
    try:
        adm, _ = _login(base, *ADMIN)  # ONE admin login: /auth/* is capped at 10 a minute
        if adm is None:
            raise EnvError(f"đăng nhập admin {ADMIN[0]} thất bại")
        if a.teardown:
            teardown(base, adm)
            print("✅ Đã gỡ tài khoản/nhóm/hàng đợi của fixture BPM.")
            finished = True
            return 0
        if a.check:
            t = Tenant(base, adm)
            gaps = [f"{e}: {d}" for e in ACCOUNTS if (d := t.account_drift(e))]
            gaps += [f"«{TEAM}» thiếu {e}" for e in t.team_missing()]
            gaps += [f"hàng đợi «{QUEUE['label']}» chưa có/lệch"] if t.queue_drift() else []
            gaps += verify(base, t, fix=False)
        else:
            t = converge(base, adm, done)
            gaps = verify(base, t, fix=True)
        for g in gaps:
            print(f"✗ {g}")
        finished = True
        if gaps:
            return 1
        print(f"✅ Fixture BPM đạt: {', '.join(ACCOUNTS)} (không-admin, đăng nhập thật đã kiểm), "
              f"nhóm «{TEAM}», hàng đợi «{QUEUE['label']}».")
        return 0
    except EnvError as e:
        print(f"LỖI MÔI TRƯỜNG: {e}", file=sys.stderr)
        return 2
    finally:
        # Nothing shared is modified, so nothing needs restoring. A run that stopped half-way
        # says what it already did — re-run (idempotent) or --teardown.
        if done and not finished:
            print("Đã làm trước khi dừng: " + "; ".join(done), file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
