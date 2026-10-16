#!/usr/bin/env python3
"""Luật nghiệm thu số 0 — chứng minh app dựng xong THẬT SỰ dùng được bởi người không phải admin.

Vì sao cần script này: `middleware/acl.go` cho `is_admin` bỏ qua TOÀN BỘ ACL, và customer-admin
sinh lúc bàn giao cũng là `IsAdmin: true, RoleID nil`. Người dựng (Founder) và admin khách đều
KHÔNG THỂ va vào bức tường quyền — nó chỉ tồn tại với người thứ ba. Mọi vòng kiểm thủ công vì thế
tự chấm điểm xanh cho chính nó. Script này ép người thứ ba có mặt.

Dùng:
    python3 scripts/acceptance-acl.py                      # so admin vs nhân viên mặc định
    python3 scripts/acceptance-acl.py --expect pm_projects,tk_tickets
    NEXUS_STAFF_PASS='X' python3 scripts/acceptance-acl.py --staff other@example.com
    NEXUS_WS_TOKEN=<token phiên dựng> NEXUS_STAFF_PASS='X' python3 scripts/acceptance-acl.py \\
        --base http://localhost:81xx/api/v1 --tenant <khách> --admin-token-env NEXUS_WS_TOKEN \\
        --staff <nhân viên không-admin của khách> --expect <bảng>
        (phía quản trị = token phiên dựng đọc từ biến môi trường, không đăng nhập, không bao giờ in ra;
         kèm --admin hoặc biến trống/thiếu ⇒ thoát 2)
    python3 scripts/acceptance-acl.py --expect products --role app_sales_staff

Mật khẩu: đường chính là biến môi trường `NEXUS_ADMIN_PASS` / `NEXUS_STAFF_PASS` (mật khẩu trên
dòng lệnh ai `ps` cũng thấy); thiếu thì rơi về tài khoản seed của tenant dev. `http://` tới máy
KHÔNG phải loopback bị từ chối (mật khẩu đi dạng rõ).

Người thứ ba cho từng entity trong --expect. Tài khoản nhân viên mặc định (`test@example.com`) mang
MỘT vai — `crm_sales_rep` — nên nó chỉ chấm được bảng của CRM. Trước đây mọi bảng ngoài CRM
(`tk_tickets`, `pm_*`…) đỏ sẵn với 403 dù vai trực phiếu đọc được bình thường, và ai chạy cũng
học cách bỏ qua cổng đỏ (DAILY-QUEUE #78). Nay có hai đường, cả hai đều hẹp:

1. TỰ ĐỔI người chấm (`keep_judge` trả None) chỉ khi ĐỦ cả năm: nhân viên mặc định bị 403 · tài
   khoản mang một vai có ít nhất một grant · vai đó KHÔNG có grant nào trên entity (kể cả `*`) ·
   entity NẰM trên nav của ít nhất một app (`GET /apps/:name/nav`) · và KHÔNG app nào trong số đó
   có bảng anh-em mà vai đang giữ grant.
   Khi đó script hỏi chính tenant vai nào được cấp `view` entity (`GET /admin/roles/:id`), chọn vai
   xem RỘNG nhất và ít quyền khác nhất (vai grant `*` KHÔNG được tự chọn — nó xem mọi bảng, không
   phải nhân viên của bảng nào), bật TÀI KHOẢN NGHIỆM THU không-admin của vai đó, đăng nhập thật,
   đọc, rồi tắt lại trong `finally`. Không vai nào được xem ⇒ ĐỎ (thiếu grant).
   Mọi trường hợp khác GIỮ người chấm mặc định và entity ĐỎ: 200 kèm 0 dòng là RỖNG; vai CÒN grant
   trên entity mà 403 là hồi quy của chính vai; bảng cùng app với bảng vai đang giữ (vd
   `crm_accounts` vừa bị gỡ grant cuối, một bảng `crm_*` mới quên cấp, `products` trong app `sales`)
   là thiếu grant của chính vai; bảng không nằm trên nav app nào (kể cả bảng tạo qua
   `POST /admin/entities {appId}` — được cấp `app_*` mà không lên nav) ⇒ không biết là bảng của ai,
   giữ ĐỎ; tài khoản không có vai / vai rỗng ⇒ thoát 2.
2. KHAI BÁO người chấm: `--role <tên vai>` ⇒ MỌI entity trong --expect chấm bằng tài khoản nghiệm
   thu mang đúng vai đó (không tự chọn gì). Dùng khi bảng thật sự không dành cho vai của --staff —
   người đọc log thấy rõ vai nào được khai, kể cả khi ai đó khai vai quản lý.

Tài khoản nghiệm thu (DAILY-QUEUE #98): MỘT tài khoản CỐ ĐỊNH mỗi vai,
`nghiemthu-<role id>@acceptance.invalid`, dùng lại mỗi lần chạy và CẤT giữa các lần = `disabled`
VÀ gỡ vai (bật + gán lại vai + mật khẩu ngẫu nhiên mới → đăng nhập → đọc → cất). Trước đây mỗi lần
chạy tạo một tài khoản rồi xoá — xoá là soft-delete và unique (tenant, email) tính cả hàng đã xoá, nên
mỗi lần để lại một hàng `users` chết mỗi vai. Tài khoản `disabled` không đăng nhập được và không làm
readiness app đọc "có người giữ vai" (`HasNonAdminUserAssigned` chỉ đếm `active`); gỡ vai vì xoá app
GIỮ vai seed `app_<tên>_staff`/`_manager` còn BẤT KỲ ai mang, kể cả người `disabled`
(`AppService.cleanupSeededRoles`) — cất mà giữ vai thì mọi app đã qua cổng để lại một vai mồ côi khi
xoá. Đầu MỖI lần chạy và trong `finally`, cổng quét và CẤT mọi tài khoản `@acceptance.invalid` chưa
cất (còn `active`, hoặc `disabled` mà còn vai: lần chạy bị giết cứng, POST mà server đã ghi nhưng phản
hồi không về, tài khoản do bản script cũ cất). SIGTERM/SIGHUP cũng chạy `finally`. Cất thất bại ⇒
thoát 2 và in đủ id còn sống. Hai lần chạy CÙNG LÚC trên cùng tenant dùng chung tài khoản nên có thể làm
nhau ĐỎ giả (tắt tài khoản của nhau) — không bao giờ XANH giả.
Chỉ GHI (bật/tắt/tạo tài khoản) khi `--base` là loopback, hoặc có `--allow-remote-write`; trước khi
ghi script in rõ base + tenant.

Mã thoát: 0 = đạt · 1 = có entity trong --expect mà nhân viên KHÔNG đọc được · 2 = lỗi môi trường
(kể cả dọn tài khoản nghiệm thu thất bại).

ponytail: stdlib thuần (urllib/argparse) — không thêm dep chỉ để gọi 4 endpoint.
"""

import argparse
import ipaddress
import json
import os
import secrets
import signal
import sys
import urllib.error
import urllib.parse
import urllib.request

DEFAULTS = {
    "base": "http://localhost:8090/api/v1",
    "tenant": "northwind",
    "admin": "admin@example.com",
    # Env first: a password on argv is visible to every `ps` on the box. The literals are the
    # documented seed accounts of the dev tenant.
    "admin_pass": os.environ.get("NEXUS_ADMIN_PASS") or "Demo-Admin-2026!",
    "staff": "test@example.com",
    "staff_pass": os.environ.get("NEXUS_STAFF_PASS") or "Demo-Rep-2026!",
}

# The acceptance accounts' domain: .invalid is reserved (RFC 2606), so no mail can ever reach a
# real person, and the suffix is how the self-healing sweep recognises them.
HOLDER_DOMAIN = "@acceptance.invalid"
# Fallback logins per role: a slot whose row was soft-deleted (e.g. an admin tidying the user list)
# stays taken forever — unique (tenant, email) counts deleted rows — so the gate moves to the next.
HOLDER_SLOTS = 5


def _req(method, url, token=None, body=None, exit_on_error=True):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else {})  # DELETE → 204, empty body
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.load(e)
        except Exception:
            return e.code, {}
    except Exception as e:  # connection refused, DNS, timeout…
        print(f"LỖI MÔI TRƯỜNG: không gọi được {url} — {e}", file=sys.stderr)
        if exit_on_error:
            sys.exit(2)
        return 0, {}  # cleanup callers keep going and report what is left


def login(base, tenant, email, password):
    code, body = _req("POST", base + "/auth/login",
                      body={"tenant": tenant, "email": email, "password": password})
    data = body.get("data") or {}
    if code != 200 or not data.get("accessToken"):
        if data.get("mfaPending"):
            print(f"LỖI MÔI TRƯỜNG: {email} đang bật MFA — script không tự vượt bước 2.", file=sys.stderr)
            sys.exit(2)
        print(f"LỖI MÔI TRƯỜNG: đăng nhập {email} thất bại ({code} {body.get('code','')}).", file=sys.stderr)
        sys.exit(2)
    return data["accessToken"], data.get("user") or {}


def readable(admin_code, admin_total, staff_code, staff_total):
    """(readable, blind) for one entity read by one non-admin.

    200 IS NOT ENOUGH. A grant of view=team on a table whose rows were all created by the
    installer resolves to zero visible rows: the staff member gets a clean 200 with
    pagination.total = 0 and an EMPTY SCREEN. The gate used to score that as a pass, which is
    exactly how crm_sales_rep shipped unable to see a single customer account.

    So "readable" means: the admin can see rows AND the staff member can see at least one of them.
    An entity that is empty for EVERYBODY is not evidence of a permission bug, so it stays a pass —
    the comparison is against what the admin sees, never against zero.
    """
    if staff_code != 200:
        return False, False
    admin_rows = admin_total if isinstance(admin_total, int) and admin_code == 200 else 0
    staff_rows = staff_total if isinstance(staff_total, int) else 0
    if admin_rows > 0 and staff_rows == 0:
        return False, True  # 200 with nothing in it — the empty-screen failure
    return True, False


# Row scope of a view grant, widest first ("none" is a revoked grant, not a candidate).
_VIEW_RANK = {"all": 0, "team": 1, "owner": 2}


def grants_on(role, entity):
    """The role's grants that apply to entity (named directly or via '*'); [] for no role."""
    return [g for g in ((role or {}).get("actions") or []) if g.get("entityName") in (entity, "*")]


def view_candidates(roles, entity):
    """Roles the tenant grants `view` on entity BY NAME, best third party first.

    A '*' grant does not make a candidate (#98 L3): that role sees every table, so it is nobody's
    staff role for this one and would score green where the real staff role reads an empty screen.
    `--role` can still name such a role explicitly — then the log says who chose it.

    Order: widest view scope (a fresh account owns nothing and sits in no team, so only an `all`
    grant can show it the rows that already exist) → fewest other grants on the entity (a staff
    role, not a manager) → name, so the pick is deterministic run to run.
    """
    ranked = []
    for r in roles:
        mine = [g for g in (r.get("actions") or []) if g.get("entityName") == entity]
        views = [_VIEW_RANK[g.get("accessLevel")] for g in mine
                 if g.get("action") == "view" and g.get("accessLevel") in _VIEW_RANK]
        if views:
            ranked.append(((min(views), len(mine), r.get("name", "")), r))
    ranked.sort(key=lambda t: t[0])
    return [r for _, r in ranked]


def keep_judge(own, entity, staff_code, apps):
    """None when — and only when — the default staff account's answer on `entity` says nothing
    about its own role, so a holder of another role may judge it. Otherwise the reason it STAYS
    the judge (and the entity stays red):

      "not-403"        a 200 with zero rows is the role's own empty screen, not someone else's table
      "no-role"        the account holds no role, or a role with no grant at all: nothing says which
                       tables it is for (the caller exits 2)
      "own-grant"      the role holds a grant on the entity (directly or '*'): a 403 is its regression
      "off-nav"        the entity is on no app nav (or the catalog is empty): nothing says whose table
                       it is. POST /admin/entities {appId} grants app_* roles WITHOUT a nav item, so
                       "no app" must not read as "some other app's table".
      "same-app:a,b"   the entity sits in app a/b where the role holds grants on a sibling table —
                       a revoked last grant, a new table the seed forgot, or a PUT that REPLACED the
                       grant set without it. That is the role's own missing grant.

    `own` is the role WITH grants (GET /admin/roles/:id); `apps` is {app name: {entity names}}.
    """
    if staff_code != 403:
        return "not-403"
    mine = {g.get("entityName") for g in ((own or {}).get("actions") or [])}
    if not mine:
        return "no-role"
    if grants_on(own, entity):
        return "own-grant"
    homes = [name for name, ents in apps.items() if entity in ents]
    if not homes:
        return "off-nav"
    shared = sorted(name for name in homes if apps[name] & mine)
    if shared:
        return "same-app:" + ",".join(shared)
    return None


def load_apps(base, adm, entity_by_id):
    """{app name: {entity names on its nav}} — read as admin, so the nav is unfiltered.

    Fails CLOSED (exit 2): an app map that came back short would make every table look like it
    sits outside the role's apps, and keep_judge would switch the judge on all of them.
    """
    code, body = _req("GET", base + "/apps", adm)
    if code != 200:
        print(f"LỖI MÔI TRƯỜNG: không đọc được danh mục app ({code} {body.get('code', '')}).", file=sys.stderr)
        sys.exit(2)
    out = {}
    for app in (body.get("data") or {}).get("apps") or []:
        c, b = _req("GET", f"{base}/apps/{app['name']}/nav", adm)
        if c != 200:
            print(f"LỖI MÔI TRƯỜNG: không đọc được nav app {app['name']} ({c}).", file=sys.stderr)
            sys.exit(2)
        ents = set()
        for it in (b.get("data") or {}).get("items") or []:
            if it.get("type") != "entity":
                continue
            if it.get("refId") not in entity_by_id:  # GetNav drops orphans, so this is OUR map
                print(f"LỖI MÔI TRƯỜNG: nav app {app['name']} trỏ entity {it.get('refId')} "
                      f"không có trong /metadata/entities.", file=sys.stderr)
                sys.exit(2)
            ents.add(entity_by_id[it["refId"]])
        out[app["name"]] = ents
    return out


def load_roles(base, adm):
    """Every role in the tenant WITH its grants (the list endpoint omits them)."""
    code, body = _req("GET", base + "/admin/roles", adm)
    if code != 200:
        print(f"LỖI MÔI TRƯỜNG: không đọc được danh sách vai ({code} {body.get('code', '')}).", file=sys.stderr)
        sys.exit(2)
    out = []
    for r in (body.get("data") or {}).get("roles") or []:
        c, b = _req("GET", f"{base}/admin/roles/{r['id']}", adm)
        if c != 200:
            print(f"LỖI MÔI TRƯỜNG: không đọc được vai {r.get('name')} ({c}).", file=sys.stderr)
            sys.exit(2)
        out.append((b.get("data") or {}).get("role") or {})
    return out


def read_as_role(a, adm, role, entity, holders, tokens):
    """Read `entity` as the role's NON-admin acceptance account (one login per role per run —
    /auth/* is rate-limited to 10 attempts a minute, so never one login per entity)."""
    if role["id"] not in tokens:
        tokens[role["id"]] = _enable_holder(a, adm, role, holders)
    code, body = _req("GET", f"{a.base}/{entity}?limit=1", tokens[role["id"]])
    total = body.get("pagination", {}).get("total", "?") if code == 200 else body.get("code", code)
    return code, total


def holder_emails(role):
    """The fixed login of `role`'s acceptance account, then its fallback slots (HOLDER_SLOTS).
    Keyed by role id, not name: an id never needs escaping and survives a rename."""
    stem = f"nghiemthu-{role['id']}"
    return [stem + HOLDER_DOMAIN] + [f"{stem}-{n}{HOLDER_DOMAIN}" for n in range(2, HOLDER_SLOTS + 1)]


def live_holders(users):
    """Ids of acceptance accounts not yet PARKED (GET /admin/users hides deleted rows). Parked =
    disabled AND role cleared: one that can still log in is live, and so is a disabled one still
    holding a role — deleting an app keeps a seeded role any user holds, disabled or not
    (AppService.cleanupSeededRoles), so it would pin the app_<name>_staff the gate judged with."""
    return [u["id"] for u in users
            if (u.get("email") or "").endswith(HOLDER_DOMAIN)
            and (u.get("status") == "active" or u.get("roleId"))]


def _enable_holder(a, adm, role, holders):
    """Log in as `role`'s acceptance account: re-enable it with a fresh random password (never
    printed), or create it the first time — non-admin, holding exactly `role`.

    Its id joins `holders` BEFORE the enabling write, so `finally` disables it whatever happens
    next. A POST whose reply is lost after the server committed never yields an id — the sweep in
    disable_live_holders finds that account by its domain instead.
    """
    code, body = _req("GET", a.base + "/admin/users", adm)
    if code != 200:
        print(f"LỖI MÔI TRƯỜNG: không đọc được danh sách người dùng ({code}).", file=sys.stderr)
        sys.exit(2)
    by_email = {u.get("email"): u for u in (body.get("data") or {}).get("users") or []}
    emails = holder_emails(role)
    password = secrets.token_urlsafe(24)
    user = next((by_email[e] for e in emails if e in by_email), None)
    if user:
        holders.append(user["id"])
        change = {"status": "active", "password": password}
        if user.get("isAdmin"):
            change["isAdmin"] = False
        if user.get("roleId") != role["id"]:
            change["roleId"] = role["id"]
        code, body = _req("PUT", f"{a.base}/admin/users/{user['id']}", adm, change)
        want = 200
    else:
        for email in emails:  # 409 = a soft-deleted row still owns this slot → try the next one
            code, body = _req("POST", a.base + "/admin/users", adm, {
                "email": email, "password": password, "isAdmin": False, "roleId": role["id"],
                "status": "active",
                "fullName": f"Nghiệm thu tự động — vai {role.get('name')} (tắt giữa các lần chạy)",
            })
            if code != 409:
                break
        want = 201
    user = (body.get("data") or {}).get("user") or {}
    if code != want or not user.get("id"):
        print(f"LỖI MÔI TRƯỜNG: không bật được tài khoản nghiệm thu vai {role.get('name')} "
              f"({code} {body.get('code', '')}).", file=sys.stderr)
        sys.exit(2)
    if user["id"] not in holders:
        holders.append(user["id"])
    # An admin would bypass ACL and score the gate green by construction; another role would judge
    # someone else's table.
    if user.get("isAdmin") or user.get("roleId") != role["id"]:
        print("LỖI MÔI TRƯỜNG: tài khoản nghiệm thu là admin hoặc mang sai vai — không dùng để chấm.",
              file=sys.stderr)
        sys.exit(2)
    token, _ = login(a.base, a.tenant, user["email"], password)
    return token


def disable_live_holders(base, adm, known=()):
    """Park every live acceptance account (live_holders) — this run's, a killed run's, and one whose
    POST reply was lost after the server committed: disable it AND clear its role (_enable_holder
    gives the role back). Returns what may still be live: ids whose park failed, plus a marker when
    the user list itself is unreadable (then `known` is tried).

    Never exits: a cleanup that dies half-way hides everything after the failure.
    """
    code, body = _req("GET", base + "/admin/users", adm, exit_on_error=False)
    left = []
    if code == 200:
        ids = live_holders((body.get("data") or {}).get("users") or [])
    else:
        ids = list(known)
        left.append(f"(không liệt kê được /admin/users — {code}; tài khoản sống khác có thể còn)")
    for uid in ids:
        c, _ = _req("PUT", f"{base}/admin/users/{uid}", adm, {"status": "disabled", "roleId": ""},
                    exit_on_error=False)
        if c != 200:
            left.append(uid)
    return left


def is_loopback(base):
    """True when --base points at this machine: `localhost` or a literal loopback IP — never a
    name that merely starts with 127."""
    host = (urllib.parse.urlparse(base).hostname or "").lower()
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def _exit_on_signal(signum, _frame):
    """SIGTERM/SIGHUP used to end the gate WITHOUT running `finally` (exit 143/129, accounts left
    live). SystemExit unwinds through `finally` the way Ctrl-C does."""
    sys.exit(128 + signum)


def warn_if_binary_stale():
    """Cảnh báo khi server đang chạy được build từ code CŨ hơn cây hiện tại.

    Đây là bẫy đã cắn thật (2026-07-29): binary build 03:28 trong khi code vừa sửa lúc 11:xx —
    chạy cổng lúc đó sẽ báo ĐỎ/XANH theo bản cũ và người đọc kết luận sai về lát vừa land.
    Không chặn (script vẫn chạy), chỉ nói thẳng.
    """
    import os
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    binary = os.path.join(root, "bin", "server")
    if not os.path.exists(binary):
        return
    bin_mtime = os.path.getmtime(binary)
    newer = []
    for dirpath, _dirs, files in os.walk(os.path.join(root, "internal")):
        for f in files:
            if f.endswith(".go") and not f.endswith("_test.go"):
                fp = os.path.join(dirpath, f)
                if os.path.getmtime(fp) > bin_mtime:
                    newer.append(os.path.relpath(fp, root))
    if newer:
        print(f"⚠️  BINARY CŨ HƠN CODE — {len(newer)} file .go mới hơn bin/server "
              f"(vd {newer[0]}). Kết quả dưới đây phản ánh bản ĐANG CHẠY, không phải code hiện tại.\n"
              f"   Build lại rồi chạy lại: go build -o bin/server cmd/server/main.go && "
              f"bash /tmp/nexus-be-run.sh\n", file=sys.stderr)


def resolve_admin(admin, token_env, environ):
    """Phía quản trị của cổng: ("token", <token>) đọc từ biến môi trường, hoặc ("login", <email>).

    `admin` là giá trị `--admin` TRUYỀN TƯỜNG MINH (None nếu không truyền — mặc định `admin@example.com`
    chỉ giải ra sau phép kiểm này, để phân biệt "không truyền" với "truyền đúng giá trị mặc định").
    `--admin-token-env VAR` dành cho token phiên dựng (specs/062-customer-workspace): token KHÔNG
    bao giờ nằm trên argv (ai `ps` cũng thấy) và KHÔNG bao giờ được in ra. Kèm `--admin` là mâu
    thuẫn, biến rỗng/thiếu là thiếu thông tin ⇒ thoát 2 (thông báo chỉ nêu TÊN biến).
    """
    if not token_env:
        return "login", admin or DEFAULTS["admin"]
    if admin:
        print(f"LỖI MÔI TRƯỜNG: --admin và --admin-token-env {token_env} loại trừ nhau — một phía quản "
              f"trị chỉ có MỘT nguồn (đăng nhập hoặc token).", file=sys.stderr)
        sys.exit(2)
    token = (environ.get(token_env) or "").strip()
    if not token:
        print(f"LỖI MÔI TRƯỜNG: biến môi trường {token_env} trống hoặc chưa đặt — --admin-token-env cần "
              f"token phiên dựng còn hạn (≤ 15 phút), lấy mới ngay trước khi chạy.", file=sys.stderr)
        sys.exit(2)
    return "token", token


def main():
    p = argparse.ArgumentParser(description="Luật nghiệm thu số 0 — admin vs nhân viên thật")
    for k, v in DEFAULTS.items():
        # --admin has no argparse default: resolve_admin must tell "not passed" from "passed the default".
        p.add_argument("--" + k.replace("_", "-"), default=None if k == "admin" else v)
    p.add_argument("--admin-token-env", default="", metavar="VAR",
                   help="Tên BIẾN MÔI TRƯỜNG chứa token phía quản trị (token phiên dựng của "
                        "specs/062) thay cho đăng nhập --admin. Token không bao giờ trên argv/in ra.")
    p.add_argument("--expect", default="",
                   help="Danh sách entity (phẩy) mà nhân viên PHẢI đọc được. Thiếu một cái = thoát 1.")
    p.add_argument("--role", default="",
                   help="Khai báo người chấm: MỌI entity trong --expect chấm bằng tài khoản nghiệm thu "
                        "không-admin mang vai này (tên vai), thay cho --staff.")
    p.add_argument("--allow-remote-write", action="store_true",
                   help="Cho phép bật/tắt/tạo tài khoản nghiệm thu khi --base KHÔNG phải loopback.")
    a = p.parse_args()
    for sig in (signal.SIGTERM, signal.SIGHUP):
        signal.signal(sig, _exit_on_signal)

    if urllib.parse.urlparse(a.base).scheme != "https" and not is_loopback(a.base):
        print(f"LỖI MÔI TRƯỜNG: từ chối gửi mật khẩu qua {a.base} — không phải https mà cũng không "
              f"phải loopback.", file=sys.stderr)
        return 2
    # The gate WRITES to whatever tenant --base points at (acceptance accounts); on a remote host
    # that must be a decision, not a default.
    writable = is_loopback(a.base) or a.allow_remote_write

    warn_if_binary_stale()
    admin_kind, admin_value = resolve_admin(a.admin, a.admin_token_env, os.environ)
    if admin_kind == "token":
        adm = admin_value  # a builder workspace-session token: no login, never printed
    else:
        adm, _ = login(a.base, a.tenant, admin_value, a.admin_pass)
    staff, staff_user = login(a.base, a.tenant, a.staff, a.staff_pass)
    if staff_user.get("isAdmin"):
        print(f"LỖI MÔI TRƯỜNG: {a.staff} là admin — admin bỏ qua ACL, cổng sẽ xanh theo cấu tạo. "
              f"Dùng một tài khoản nhân viên.", file=sys.stderr)
        return 2

    # Self-heal: a run killed hard (kill -9) between enabling and parking left an account live.
    if writable:
        left = disable_live_holders(a.base, adm)
        if left:
            print(f"LỖI MÔI TRƯỜNG: không tắt được tài khoản nghiệm thu còn sống: {', '.join(left)}",
                  file=sys.stderr)
            return 2

    code, meta = _req("GET", a.base + "/metadata/entities", adm)
    entities = [e["name"] for e in (meta.get("data") or [])]
    entity_by_id = {e.get("id"): e["name"] for e in (meta.get("data") or [])}
    if not entities:
        print("LỖI MÔI TRƯỜNG: admin không thấy entity nào — tenant rỗng?", file=sys.stderr)
        sys.exit(2)

    # Nhân viên thấy TÊN bao nhiêu entity — lỗ METADATA-ACL-FILTER hiện ra ngay ở đây.
    _, staff_meta = _req("GET", a.base + "/metadata/entities", staff)
    staff_named = len(staff_meta.get("data") or [])

    rows, can_read, seen = [], set(), {}
    for name in entities:
        ca, da = _req("GET", f"{a.base}/{name}?limit=1", adm)
        cs, ds = _req("GET", f"{a.base}/{name}?limit=1", staff)
        ta = da.get("pagination", {}).get("total", "?") if ca == 200 else da.get("code", ca)
        ts = ds.get("pagination", {}).get("total", "?") if cs == 200 else ds.get("code", cs)
        ok, blind = readable(ca, ta, cs, ts)
        if ok:
            can_read.add(name)
        seen[name] = (ca, ta, cs)
        rows.append((name, f"{ca} {ta}", f"{cs} {ts}" + ("  ← RỖNG" if blind else ""), ok))

    print(f"{'ENTITY':<24}{'ADMIN':<14}{'NHÂN VIÊN':<18}")
    print("-" * 56)
    for name, av, sv, ok in rows:
        print(f"{name:<24}{av:<14}{sv:<18}{'✓' if ok else '✗'}")
    print("-" * 56)
    print(f"Nhân viên đọc được DỮ LIỆU: {len(can_read)}/{len(entities)} entity")
    print(f"Nhân viên thấy TÊN:         {staff_named}/{len(entities)} entity"
          + ("  ⚠️  /metadata/entities chưa lọc CanViewEntity (METADATA-ACL-FILTER)"
             if staff_named > len(can_read) else ""))

    want = [e.strip() for e in a.expect.split(",") if e.strip()]
    if not want:
        print("\n(Không có --expect: chỉ báo cáo, không phán quyết. "
              "Dùng --expect <entity vừa dựng> để biến đây thành cổng nghiệm thu.)")
        return 0

    judge, no_grant = {}, []  # judge: entity -> role whose throwaway holder reads it instead
    if a.role:
        roles = load_roles(a.base, adm)
        declared = next((r for r in roles if r.get("name") == a.role), None)
        if declared is None:
            print(f"LỖI MÔI TRƯỜNG: tenant không có vai '{a.role}'.", file=sys.stderr)
            return 2
        judge = {e: declared for e in want if e in seen}
        can_read -= set(judge)  # the verdict is the declared role's, not the default account's
        print(f"\nNgười chấm KHAI BÁO (--role {a.role}) thay cho {a.staff} — mọi entity trong --expect:")
    else:
        # The default staff account holds ONE role. keep_judge decides, per 403, whether that says
        # anything about the role — see its docstring; every "no" leaves the entity red.
        forbidden = [e for e in want if e in seen and e not in can_read and seen[e][2] == 403]
        if forbidden:
            roles = load_roles(a.base, adm)
            own = next((r for r in roles if r.get("id") == staff_user.get("roleId")), None)
            own_name = (own or {}).get("name", "không có vai")
            apps = load_apps(a.base, adm, entity_by_id)
            print(f"\nNgười thứ ba theo vai (nhân viên {a.staff} — vai {own_name} — bị 403):")
            for e in forbidden:
                why = keep_judge(own, e, seen[e][2], apps)
                cands = view_candidates(roles, e)
                hint = ("\n  " + " " * 22 + " vai này PHẢI đọc được bảng ⇒ trả grant cho vai; bảng KHÔNG "
                        "dành cho vai này ⇒ khai --role "
                        + (" | ".join(c["name"] for c in cands) or "<vai> (hiện không vai nào được xem)"))
                if why == "no-role":
                    print(f"LỖI MÔI TRƯỜNG: {a.staff} không mang vai nào (hoặc vai không còn grant nào) "
                          f"— không còn người thứ ba để chấm. Gán vai cho tài khoản, hoặc khai --role.",
                          file=sys.stderr)
                    return 2
                if why is None:  # the ONLY switch; any other answer below leaves e red
                    if cands:
                        judge[e] = cands[0]
                    else:
                        no_grant.append(e)
                        print(f"  {e:<22} không vai không-admin nào được cấp quyền xem  ✗")
                elif why == "own-grant":
                    print(f"  {e:<22} vai {own_name} CÓ grant trên bảng này mà vẫn 403 — hồi quy của "
                          f"chính vai này, không đổi người chấm  ✗")
                elif why.startswith("same-app:"):
                    print(f"  {e:<22} cùng app {why.split(':', 1)[1]} với bảng vai {own_name} đang giữ — "
                          f"thiếu grant của chính vai này, không đổi người chấm  ✗{hint}")
                else:  # "off-nav" (and anything added later)
                    print(f"  {e:<22} không nằm trên nav app nào — không biết bảng dành cho vai nào ({why}), "
                          f"không đổi người chấm  ✗{hint}")

    left = []
    if judge:
        if not writable:
            print(f"LỖI MÔI TRƯỜNG: cần tài khoản nghiệm thu để chấm {', '.join(judge)} nhưng {a.base} không "
                  f"phải loopback — thêm --allow-remote-write nếu thật sự muốn ghi vào tenant đó.",
                  file=sys.stderr)
            return 2
        print(f"  (bật tài khoản nghiệm thu trên {a.base} · tenant {a.tenant} — tắt lại ngay sau khi đọc)")
        holders, tokens = [], {}
        try:
            for e, r in judge.items():
                cs, ts = read_as_role(a, adm, r, e, holders, tokens)
                ok, blind = readable(seen[e][0], seen[e][1], cs, ts)
                if ok:
                    can_read.add(e)
                others = "" if a.role else "   (vai khác được xem: " + (
                    ", ".join(c["name"] for c in view_candidates(roles, e)[1:]) or "—") + ")"
                print(f"  {e:<22} vai {r['name']} (tài khoản nghiệm thu): {cs} {ts}"
                      + ("  ← RỖNG" if blind else "") + f"  {'✓' if ok else '✗'}{others}")
        finally:
            # A second signal must not cut the cleanup short; every call inside is time-bounded.
            for sig in (signal.SIGTERM, signal.SIGHUP, signal.SIGINT):
                signal.signal(sig, signal.SIG_IGN)
            left = disable_live_holders(a.base, adm, holders)
            if left:
                print(f"LỖI MÔI TRƯỜNG: KHÔNG tắt được tài khoản nghiệm thu còn sống: {', '.join(left)} "
                      f"— tắt tay ở /admin/users.", file=sys.stderr)
    if left:
        return 2

    missing = [e for e in want if e not in can_read]
    unknown = [e for e in want if e not in entities]
    if unknown:
        print(f"\n⚠️  Không tồn tại trong tenant: {', '.join(unknown)}", file=sys.stderr)
    if missing:
        print(f"\n❌ CHƯA ĐẠT — nhân viên KHÔNG đọc được (hoặc đọc ra MÀN TRỐNG): {', '.join(missing)}")
        if no_grant:
            print(f"   Thiếu grant: không vai nào được xem {', '.join(no_grant)}. Cấp ở Vai trò & quyền,")
            print("   hoặc POST /admin/apps/:id/acl-backfill cho app chứa bảng.")
        print("   Lưu ý: 200 kèm 0 dòng cũng tính là TRƯỢT — người dùng mở ra không thấy gì thì")
        print("   quyền có hay không cũng như nhau. Thường là view=team trên bảng mà mọi dòng do")
        print("   trình cài tạo ra (không thuộc đội nào).")
        print("   App này với người thứ ba là màn trống. Xem TRỤ 2 mục 4 (seed vai + grant) ở")
        print("   nexus-platform/docs/NEXUS-MOT-XUONG-2026-07-29.md")
        return 1
    print(f"\n✅ ĐẠT — người thứ ba đọc được cả {len(want)} entity yêu cầu.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
