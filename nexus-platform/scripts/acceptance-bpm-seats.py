#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S3a của specs/043 (`bpm-seats-dual-write`, DAILY-QUEUE #112, T251).

Migration A (`*_bpm_runtime_v2`) + ghi kép ghế phải KHÔNG đổi gì người duyệt thấy, và phải làm hồ sơ
trước 000065 có quy trình gốc đã xoá duyệt ĐƯỢC. Hai pha, trên DB CLONE (không bao giờ DB dev dùng chung):

  pre   — binary TRƯỚC lát, schema trước Migration A. Admin tạo quy trình thủ công 2 bước (duyet1 → duyet2)
          và một quy trình "cũ" tắt; test@example.com (không-admin) gửi Q1..Q4; duyet1 duyệt Q2 (sang s2),
          uỷ quyền Q3 cho duyet3; Q4 bị đưa về hình dạng hồ sơ trước 000065 (bỏ ghim, trỏ quy trình cũ —
          GHI THẲNG vào clone) rồi admin xoá quy trình cũ. Chụp /approvals/pending của duyet1/2/3 và TÁI
          HIỆN lỗi: duyet1 không duyệt được Q4.
  post  — sau `migrate up` và chạy binary CỦA lát trên cùng clone. Số ghế mở = số hồ sơ chờ có người duyệt
          (đọc, ghim tenant); Q4 được ghim từ hàng quy trình ĐÃ XOÁ, giờ đồng hồ; /approvals/pending của
          duyet1/2/3 khớp đúng tập đã chụp; người ngoài (hotro1) bị từ chối; Q1..Q4 đều duyệt được bởi đúng
          người và ghế phản chiếu từng lần; test@ gửi + rút Q5 trên binary mới. Cuối lượt xoá quy trình
          nghiệm thu (hồ sơ đã ghim vẫn chạy).

Cần `scripts/fixture-bpm.py` chạy trước trên cùng server. Trạng thái giữa hai pha nằm ở `--state`.

    python3 scripts/acceptance-bpm-seats.py pre  --base http://localhost:8136/api/v1 --db nexus_wt_bpm_seats_dual_write
    (dừng server; migrate -path migrations -database "mysql://root:@tcp(127.0.0.1:3306)/<db>" up; chạy binary mới)
    python3 scripts/acceptance-bpm-seats.py post --base http://localhost:8136/api/v1 --db nexus_wt_bpm_seats_dual_write

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: stdlib + `docker exec nexus-mysql mysql` cho phần đọc bảng ghế (chưa có API đọc ghế tới S3a2).
"""
import argparse
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request

ADMIN = ("admin@example.com", "Demo-Admin-2026!")
STAFF = ("test@example.com", "Demo-Rep-2026!")  # the same defaults scripts/acceptance-acl.py uses
PROC = "S3a nghiệm thu — báo giá 2 bước"

_spec = importlib.util.spec_from_file_location(
    "fixture_bpm", os.path.join(os.path.dirname(os.path.abspath(__file__)), "fixture-bpm.py"))
fixture = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(fixture)

fails = []


def check(name, ok, detail=""):
    print(("PASS " if ok else "FAIL ") + name + (f" — {detail}" if detail else ""))
    if not ok:
        fails.append(name)


class Env:
    def __init__(self, base, db):
        self.base, self.db = base, db
        self.tenant = self.sql("SELECT id FROM tenants WHERE slug = 'northwind'")[0][0]

    def req(self, method, path, token=None, body=None):
        data = json.dumps(body).encode() if body is not None else None
        h = {"Content-Type": "application/json"} if data else {}
        if token:
            h["Authorization"] = "Bearer " + token
        r = urllib.request.Request(self.base + path, data=data, headers=h, method=method)
        try:
            with urllib.request.urlopen(r, timeout=15) as resp:
                raw = resp.read()
                return resp.status, (json.loads(raw) if raw else {})
        except urllib.error.HTTPError as e:
            try:
                return e.code, json.load(e)
            except Exception:
                return e.code, {}

    def sql(self, q):
        out = subprocess.run(["docker", "exec", "nexus-mysql", "mysql", "-uroot", "-N", "-B",
                              "--default-character-set=utf8mb4", self.db, "-e", q],
                             check=True, capture_output=True, text=True).stdout
        return [line.split("\t") for line in out.strip().splitlines() if line]

    def login(self, email, pw):
        c, b = self.req("POST", "/auth/login", body={"tenant": "northwind", "email": email, "password": pw})
        assert c == 200, (email, c, b.get("code"))
        return b["data"]["accessToken"]

    def tokens(self):
        t = {"staff": self.login(*STAFF), "admin": self.login(*ADMIN)}
        for n in ("duyet1", "duyet2", "duyet3", "hotro1"):
            t[n], _ = fixture.login_as(self.base, f"{n}@example.com")
        for who in ("staff", "duyet1", "duyet2", "duyet3", "hotro1"):
            c, me = self.req("GET", "/auth/me", t[who])
            u = me.get("data", {}).get("user", me.get("data", {}))
            check(f"{who} là non-admin", c == 200 and not u.get("isAdmin"), f"isAdmin={u.get('isAdmin')}")
        return t

    def pending(self, tok):
        c, b = self.req("GET", "/approvals/pending", tok)
        assert c == 200, (c, b)
        return sorted(r.get("ID") or r.get("id") for r in b["data"]["approvals"])

    def send(self, tok, name):
        c, b = self.req("POST", "/crm_quotes", tok, {"name": name, "status": "Nháp"})
        assert c == 201, (c, b)
        rid = b["data"].get("id") or b["data"].get("record", {}).get("id")
        c, b = self.req("POST", f"/crm_quotes/{rid}/approvals/submit", tok)
        assert c == 201, (c, b)
        inst = b["data"].get("approval") or b["data"]
        return inst.get("ID") or inst.get("id")

    def seats(self, iid):
        """[node, status, outcome, assignee, decided_by, entry_seq] per seat, oldest visit first."""
        return self.sql(f"SELECT node_id, status, IFNULL(outcome, ''), assignee_id, IFNULL(decided_by, ''), entry_seq "
                        f"FROM sys_bpm_tasks WHERE tenant_id = '{self.tenant}' AND instance_id = '{iid}' "
                        f"ORDER BY entry_seq, status = 'open', created_at")

    def parity(self):
        return self.sql(f"SELECT (SELECT COUNT(*) FROM sys_bpm_tasks WHERE tenant_id = '{self.tenant}' AND status = 'open'), "
                        f"(SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = '{self.tenant}' AND status = 'pending' "
                        f"AND current_approver_type IS NOT NULL AND current_approver_id IS NOT NULL)")[0]


def pre(env, state):
    t = env.tokens()
    procs = env.req("GET", "/admin/approval-processes", t["admin"])[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]]
    assert not rivals, f"another active manual crm_quotes process would take the submit: {rivals}"
    users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", t["admin"])[1]["data"]["users"]}
    d1, d2, d3 = (users[f"duyet{i}@example.com"] for i in (1, 2, 3))
    steps = [{"id": "s1", "name": "Trưởng phòng", "approverType": "user", "approverId": d1, "slaSeconds": 14400},
             {"id": "s2", "name": "Kế toán", "approverType": "user", "approverId": d2}]
    ids = {}
    for key, name, active in (("proc", PROC, True), ("old", "S3a nghiệm thu — quy trình cũ (sẽ xoá)", False)):
        c, b = env.req("POST", "/admin/approval-processes", t["admin"], {
            "name": name, "entityName": "crm_quotes", "trigger": "manual", "isActive": active, "steps": steps})
        assert c == 201, (c, b)
        ids[key] = b["data"]["process"]["id"]

    q = {n: env.send(t["staff"], f"S3a-{n}") for n in ("Q1", "Q2", "Q3", "Q4")}
    c, _ = env.req("POST", f"/approvals/{q['Q2']}/approve", t["duyet1"], {"note": "trước migrate"})
    check("pre: duyet1 duyệt Q2 (sang s2 của duyet2)", c == 200, str(c))
    c, _ = env.req("POST", f"/approvals/{q['Q3']}/delegate", t["duyet1"], {"toUserId": d3, "note": "nghỉ phép"})
    check("pre: duyet1 uỷ quyền Q3 cho duyet3", c == 200, str(c))
    # Q4 = the shape an instance armed before 000065 has (no pin), under a process deleted since.
    env.sql(f"UPDATE sys_approval_instances SET process_snapshot = NULL, process_id = '{ids['old']}' "
            f"WHERE tenant_id = '{env.tenant}' AND id = '{q['Q4']}'")
    c, _ = env.req("DELETE", f"/admin/approval-processes/{ids['old']}", t["admin"])
    check("pre: admin xoá quy trình cũ của Q4", c == 204, str(c))

    snap = {n: env.pending(t[n]) for n in ("duyet1", "duyet2", "duyet3")}
    check("pre: duyet1 thấy Q1 + Q4", {q["Q1"], q["Q4"]} <= set(snap["duyet1"]), f"{len(snap['duyet1'])} dòng")
    check("pre: duyet2 thấy Q2", q["Q2"] in snap["duyet2"])
    check("pre: duyet3 thấy Q3", q["Q3"] in snap["duyet3"])
    c, b = env.req("POST", f"/approvals/{q['Q4']}/approve", t["duyet1"], {"note": "thử"})
    check("pre: LỖI TÁI HIỆN — duyet1 KHÔNG duyệt được Q4 (chưa ghim, quy trình gốc đã xoá)", c != 200,
          f"{c} {b.get('code')}")
    json.dump({"q": q, "snap": snap, **ids, "d1": d1, "d2": d2, "d3": d3}, open(state, "w"), indent=1)


def post(env, state):
    st = json.load(open(state))
    q, snap = st["q"], st["snap"]
    t = env.tokens()
    seats, n_open_pending = env.seats, env.parity

    o, p = n_open_pending()
    check("sau migrate: số ghế mở của northwind = số hồ sơ chờ có người duyệt", o == p, f"{o} = {p}")
    pin = env.sql(f"SELECT JSON_EXTRACT(process_snapshot, '$.backfilled'), JSON_EXTRACT(process_snapshot, '$.steps[1].slaClock') "
                  f"FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{q['Q4']}'")[0]
    check("sau migrate: Q4 ghim từ quy trình ĐÃ XOÁ, backfilled, giờ đồng hồ", pin == ["true", '"wall"'], str(pin))
    q3 = [r for r in seats(q["Q3"]) if r[1] == "open"]
    check("sau migrate: ghế mở của Q3 = duyet3 trên s1", len(q3) == 1 and q3[0][0] == "s1" and q3[0][3] == st["d3"], str(q3))

    for n in ("duyet1", "duyet2", "duyet3"):
        now = env.pending(t[n])
        check(f"/approvals/pending của {n} KHỚP đúng tập đã chụp trước migrate", now == snap[n], f"{len(now)} dòng")
    c, b = env.req("GET", "/approvals/pending", t["hotro1"])
    check("/approvals/pending của hotro1 (người ngoài) rỗng", c == 200 and b["data"]["approvals"] == [], str(c))
    before = seats(q["Q1"])
    c, b = env.req("POST", f"/approvals/{q['Q1']}/approve", t["hotro1"], {"note": "x"})
    check("hotro1 duyệt Q1 ⇒ 422 APPROVAL_NOT_AUTHORIZED (api-error-codes.md), ghế không đổi",
          c == 422 and b.get("code") == "APPROVAL_NOT_AUTHORIZED" and seats(q["Q1"]) == before, f"{c} {b.get('code')}")

    c, _ = env.req("POST", f"/approvals/{q['Q1']}/approve", t["duyet1"], {"note": "ok sau migrate"})
    s = seats(q["Q1"])
    check("duyet1 duyệt Q1 ⇒ s1 done/approved bởi duyet1, s2 mở cho duyet2 ở lượt mới",
          c == 200 and q["Q1"] in env.pending(t["duyet2"]) and s[0][:3] == ["s1", "done", "approved"]
          and s[0][4] == st["d1"] and s[-1][:2] == ["s2", "open"] and s[-1][3] == st["d2"] and int(s[-1][5]) > int(s[0][5]), str(s))
    c, _ = env.req("POST", f"/approvals/{q['Q2']}/approve", t["duyet2"], {"note": "ok"})
    check("duyet2 duyệt Q2 (vào s2 trước migrate) ⇒ kết thúc, không còn ghế mở",
          c == 200 and not [r for r in seats(q["Q2"]) if r[1] == "open"], f"{c} {seats(q['Q2'])}")
    # A note past TEXT's 65 535 bytes: no cap upstream, the old path kept it in JSON history, so the
    # seat's comment column must take it too — or strict mode 1406s the whole decision into a 500.
    long = "ệ" * 22000
    c, b = env.req("POST", f"/approvals/{q['Q3']}/approve", t["duyet3"], {"note": long})
    kept = env.sql(f"SELECT LENGTH(comment) FROM sys_bpm_tasks WHERE tenant_id = '{env.tenant}' "
                   f"AND instance_id = '{q['Q3']}' AND decided_by = '{st['d3']}'")
    check("duyet3 duyệt Q3 (được uỷ quyền trước migrate) với ghi chú 66 000 byte ⇒ sang duyet2, ghế giữ đủ ghi chú",
          c == 200 and q["Q3"] in env.pending(t["duyet2"]) and kept == [[str(len(long.encode()))]], f"{c} {b.get('code')} {kept}")
    c, b = env.req("POST", f"/approvals/{q['Q4']}/approve", t["duyet1"], {"note": "hồ sơ trước 000065"})
    check("duyet1 duyệt ĐƯỢC Q4 (trước 000065, quy trình gốc đã xoá) ⇒ sang duyet2",
          c == 200 and q["Q4"] in env.pending(t["duyet2"]), f"{c} {b.get('code')}")

    q5 = env.send(t["staff"], "S3a-Q5 gửi sau migrate")
    s5 = seats(q5)
    seq5 = env.sql(f"SELECT seq FROM sys_approval_instances WHERE tenant_id = '{env.tenant}' AND id = '{q5}'")[0][0]
    check("test@ gửi Q5 ⇒ 1 ghế mở s1 cho duyet1, seq = 2 (tạo + mục 'đã gửi')",
          len(s5) == 1 and s5[0][:2] == ["s1", "open"] and s5[0][3] == st["d1"] and seq5 == "2", f"{s5} seq={seq5}")
    c, _ = env.req("POST", f"/approvals/{q5}/recall", t["staff"], {"note": "gửi nhầm"})
    s5 = seats(q5)
    check("test@ rút lại Q5 ⇒ ghế cancelled/recalled, không ai quyết",
          c == 200 and s5[0][1:3] == ["cancelled", "recalled"] and s5[0][4] == "", f"{c} {s5}")

    o, p = n_open_pending()
    check("cuối lượt: số ghế mở = số hồ sơ chờ có người duyệt", o == p, f"{o} = {p}")
    c, _ = env.req("DELETE", f"/admin/approval-processes/{st['proc']}", t["admin"])
    check("dọn: xoá quy trình nghiệm thu (hồ sơ đã ghim vẫn chạy)", c == 204, str(c))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("phase", choices=("pre", "post"))
    ap.add_argument("--base", required=True, help="API base of the server running over the clone")
    ap.add_argument("--db", required=True, help="the CLONE database the server runs on")
    ap.add_argument("--state", help="file carrying pre's snapshot to post (default: temp dir, per db)")
    a = ap.parse_args()
    if a.db == "nexus_platform":
        sys.exit("refusing the shared dev DB — clone it first (nexus_wt_<slug>)")
    state = a.state or os.path.join(tempfile.gettempdir(), f"acceptance-bpm-seats-{a.db}.json")
    {"pre": pre, "post": post}[a.phase](Env(a.base, a.db), state)
    print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)}): {fails}")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
