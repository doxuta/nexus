#!/usr/bin/env python3
"""Nghiệm thu người thứ ba cho specs/055 #310 (đổi mật khẩu kết thúc phiên ngay) và #312 (xoay refresh
trong MỘT transaction), trên server THẬT bằng nhân viên không-admin `test@example.com`.

    python3 scripts/acceptance-session-hardening.py --base http://localhost:8507/api/v1

Bắt buộc --base (không mặc định: chạy nhầm vào :8090 dev dùng chung sẽ đổi mật khẩu thật). Mật khẩu lấy
từ NEXUS_ADMIN_PASS / NEXUS_STAFF_PASS như scripts/acceptance-acl.py. Script đổi mật khẩu của nhân viên
rồi ĐẶT LẠI về mật khẩu cũ ở khối finally.

#312: lặp N lần, đua POST /auth/refresh với POST /auth/logout-all. Sau khi cả hai xong, mọi cặp token mà
refresh trả 200 phải đã chết (access ⇒ 401, refresh ⇒ 401): trước khi gộp claim + insert vào một transaction,
logout-all rơi vào khe giữa hai câu lệnh để lại một dòng kế nhiệm còn sống.

#310: nhân viên đăng nhập hai máy.
  (a) máy A tự đổi mật khẩu ⇒ máy B nhận 401 ở request kế tiếp (không chờ access TTL), máy A vẫn sống;
  (b) admin đặt mật khẩu nhân viên ⇒ mọi máy của nhân viên 401 ngay.
Exit 0 = đạt, 1 = có điều kiện không đạt.

ponytail: stdlib thuần (urllib/threading), không thêm dep.
"""
import argparse
import json
import os
import random
import sys
import threading
import time
import urllib.error
import urllib.request

UA_A = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/128.0.0.0 Safari/537.36"
UA_B = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) Version/17.5 Mobile/15E148 Safari/604.1"

failures = []


def call(base, method, path, token="", body=None, ua=UA_A):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    req.add_header("User-Agent", ua)
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, {}


def check(ok, label):
    print(("  ĐẠT  " if ok else "  LỖI  ") + label)
    if not ok:
        failures.append(label)


def login(base, tenant, email, password, ua=UA_A):
    st, js = call(base, "POST", "/auth/login", body={"tenant": tenant, "email": email, "password": password}, ua=ua)
    if st != 200:
        sys.exit("login %s = %s %s" % (email, st, js))
    d = js["data"]
    return d["accessToken"], d["refreshToken"], d["user"]["id"]


def refresh_race(base, a):
    print("#312 — đua refresh với logout-all, %d vòng" % a.rounds)
    survivors = 0
    refreshed = 0
    for _ in range(a.rounds):
        access, refresh, _uid = login(base, a.tenant, a.staff, a.staff_pass)
        res = {}

        def do_refresh():
            res["refresh"] = call(base, "POST", "/auth/refresh", body={"refreshToken": refresh})

        def do_logout_all():
            time.sleep(random.uniform(0, 0.012))  # sweep the logout-all across the refresh's lifetime
            res["logout"] = call(base, "POST", "/auth/logout-all", token=access, body={})

        ts = [threading.Thread(target=do_refresh), threading.Thread(target=do_logout_all)]
        for t in ts:
            t.start()
        for t in ts:
            t.join()
        st, js = res["refresh"]
        if st == 200:
            refreshed += 1
            new_access, new_refresh = js["data"]["accessToken"], js["data"]["refreshToken"]
            if call(base, "GET", "/auth/sessions", token=new_access)[0] != 401:
                survivors += 1
            elif call(base, "POST", "/auth/refresh", body={"refreshToken": new_refresh})[0] != 401:
                survivors += 1
    check(survivors == 0, "mọi cặp token do refresh trả 200 đều đã chết sau logout-all (sống sót: %d/%d lần refresh thắng)" % (survivors, refreshed))
    check(refreshed >= 5, "cuộc đua có thật: refresh thắng ít nhất 5 vòng (thắng %d/%d)" % (refreshed, a.rounds))
    check(res.get("logout", (0,))[0] == 204, "logout-all trả 204")


def password_paths(base, a):
    print("#310 — đổi mật khẩu / admin đặt mật khẩu kết thúc phiên ngay")
    admin_tok, _, _ = login(base, a.tenant, a.admin, a.admin_pass)
    new_pw = "Rotated-Pw-2026!"
    tok_a, ref_a, uid = login(base, a.tenant, a.staff, a.staff_pass, ua=UA_A)
    tok_b, ref_b, _ = login(base, a.tenant, a.staff, a.staff_pass, ua=UA_B)
    current = a.staff_pass
    try:
        check(call(base, "GET", "/auth/sessions", token=tok_b)[0] == 200, "máy B đọc được trước khi đổi")
        st, js = call(base, "PUT", "/auth/me/password", token=tok_a, body={"currentPassword": current, "newPassword": new_pw})
        check(st == 200, "máy A đổi mật khẩu = 200 (nhận %s)" % st)
        current = new_pw if st == 200 else current
        check(call(base, "GET", "/auth/sessions", token=tok_b)[0] == 401, "máy B: request kế tiếp = 401 ngay (không chờ 15')")
        check(call(base, "POST", "/auth/refresh", body={"refreshToken": ref_b})[0] == 401, "máy B: refresh = 401")
        check(call(base, "GET", "/auth/sessions", token=tok_a)[0] == 200, "máy A (máy vừa đổi) vẫn đang đăng nhập")

        tok_c, _, _ = login(base, a.tenant, a.staff, current, ua=UA_B)
        check(call(base, "GET", "/auth/sessions", token=tok_c)[0] == 200, "máy C đăng nhập bằng mật khẩu mới")
        st, _ = call(base, "PUT", "/admin/users/" + uid, token=admin_tok, body={"password": a.staff_pass})
        check(st == 200, "admin đặt mật khẩu nhân viên = 200 (nhận %s)" % st)
        current = a.staff_pass if st == 200 else current
        check(call(base, "GET", "/auth/sessions", token=tok_c)[0] == 401, "máy C: admin đặt mật khẩu ⇒ 401 ngay")
        check(call(base, "GET", "/auth/sessions", token=tok_a)[0] == 401, "máy A: admin đặt mật khẩu ⇒ 401 ngay (kể cả máy tự đổi lúc trước)")
    finally:
        if current != a.staff_pass:  # chỉ xảy ra khi script dừng giữa chừng
            call(base, "PUT", "/admin/users/" + uid, token=admin_tok, body={"password": a.staff_pass})


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--base", required=True, help="vd http://localhost:8507/api/v1 (bắt buộc)")
    p.add_argument("--tenant", default="northwind")
    p.add_argument("--admin", default="admin@example.com")
    p.add_argument("--admin-pass", default=os.environ.get("NEXUS_ADMIN_PASS") or "Demo-Admin-2026!")
    p.add_argument("--staff", default="test@example.com")
    p.add_argument("--staff-pass", default=os.environ.get("NEXUS_STAFF_PASS") or "Demo-Rep-2026!")
    p.add_argument("--rounds", type=int, default=80)
    p.add_argument("--only", choices=["310", "312"], default="")
    a = p.parse_args()
    if ":8090" in a.base:
        sys.exit("từ chối chạy trên :8090 (DB dev dùng chung): dùng server + DB clone riêng")
    if a.only != "310":
        refresh_race(a.base, a)
    if a.only != "312":
        password_paths(a.base, a)
    if failures:
        print("\nKHÔNG ĐẠT: %d điều kiện" % len(failures))
        sys.exit(1)
    print("\nĐẠT")


if __name__ == "__main__":
    main()
