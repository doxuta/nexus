#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S2 của specs/043 (DAILY-QUEUE #40): "hồ sơ này duyệt mất bao lâu" + lý do từ chối bắt buộc.

What it proves, on a server running over a CLONED database (never the shared dev DB):
  1. GET /approvals/:id carries `durations`, measured on the tenant's working calendar, and the figure
     equals the one computed by hand from that calendar — for the approver (duyet1) AND the submitter
     (test@example.com). The clock is never touched: elapsed time is measured at READ time, so the third
     person edits the clone's calendar AFTER the decision — today's hours stop at the first minute
     boundary after the submission and resume one minute later — and the expected working time is the
     real stretch minus that one-minute gap. (The hours-scale case, 16:00 Fri → 10:00 Mon = 3 working
     hours, is pinned by TestApprovalDetail_BusinessDurations_FriToMon_3h.)
  2. duyet1 rejecting with no reason — no body, "", blanks, U+0085 — by their OWN token is 422
     APPROVAL_REJECT_REASON_REQUIRED and the instance stays pending; a reason goes through.
  3. An outsider (hotro1, tk_agent) gets the unknown-id 404; with no default calendar the figure is
     wall clock under wall_no_calendar, with no working-time number at all.
The calendar is restored and the acceptance process switched off in `finally`.

Needs scripts/fixture-bpm.py run first on the same server (duyet1, hotro1).

    python3 scripts/acceptance-bpm-elapsed.py --base http://localhost:8134/api/v1

Waits ~2 minutes (two real minute boundaries between "sent" and "approved").
Exit: 0 = pass · 1 = a check failed.

ponytail: stdlib only, same shape as scripts/fixture-bpm.py (logins go through its login_as()).
"""
import argparse
import datetime as dt
import importlib.util
import json
import os
import sys
import time
import urllib.error
import urllib.request

ICT = dt.timezone(dt.timedelta(hours=7))
PROC = "S2 nghiệm thu — duyệt báo giá 1 bước"
ADMIN = ("admin@example.com", "Demo-Admin-2026!")
STAFF = ("test@example.com", "Demo-Rep-2026!")  # the same defaults scripts/acceptance-acl.py uses

_spec = importlib.util.spec_from_file_location(
    "fixture_bpm", os.path.join(os.path.dirname(os.path.abspath(__file__)), "fixture-bpm.py"))
fixture = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(fixture)

results = []


def check(name, ok, detail=""):
    results.append((name, ok))
    print(("PASS " if ok else "FAIL ") + name + (f" — {detail}" if detail else ""))


class API:
    def __init__(self, base):
        self.base = base

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

    def login(self, email, pw):
        c, b = self.req("POST", "/auth/login", body={"tenant": "northwind", "email": email, "password": pw})
        assert c == 200, (email, c, b.get("code"))
        return b["data"]["accessToken"]

    def new_quote(self, tok, name):
        c, b = self.req("POST", "/crm_quotes", tok, {"name": name, "status": "Nháp"})
        assert c == 201, (c, b)
        d = b["data"]
        return d.get("id") or d.get("record", {}).get("id")

    def submit(self, tok, rid):
        c, b = self.req("POST", f"/crm_quotes/{rid}/approvals/submit", tok)
        assert c == 201, (c, b)
        d = b["data"]
        inst = d.get("approval") or d.get("instance") or d
        return inst.get("ID") or inst.get("id")

    def detail(self, tok, iid):
        c, b = self.req("GET", f"/approvals/{iid}", tok)
        return c, (b.get("data") or {}).get("approval") or b


def parse(ts):
    return dt.datetime.fromisoformat(ts.replace("Z", "+00:00"))


def run(api):
    adm = api.login(*ADMIN)
    staff = api.login(*STAFF)
    d1, _ = fixture.login_as(api.base, "duyet1@example.com")
    outsider, _ = fixture.login_as(api.base, "hotro1@example.com")
    for who, tok in (("test@example.com", staff), ("duyet1", d1), ("hotro1", outsider)):
        c, me = api.req("GET", "/auth/me", tok)
        u = me.get("data", {}).get("user", me.get("data", {}))
        check(f"{who} là non-admin", c == 200 and not u.get("isAdmin"), f"isAdmin={u.get('isAdmin')}")

    # One manual one-step process (duyet1, 4h): "approved" is terminal, so the screen says "Mất".
    procs = api.req("GET", "/admin/approval-processes", adm)[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]]
    assert not rivals, f"another active manual crm_quotes process would take the submit: {rivals}"
    users = {u["email"].lower(): u for u in api.req("GET", "/admin/users", adm)[1]["data"]["users"]}
    step = {"id": "s1", "name": "Trưởng phòng", "approverType": "user",
            "approverId": users["duyet1@example.com"]["id"], "slaSeconds": 14400}
    c, b = api.req("POST", "/admin/approval-processes", adm, {
        "name": PROC, "entityName": "crm_quotes", "trigger": "manual", "isActive": True, "steps": [step]})
    assert c == 201, (c, b)
    proc_id = b["data"]["process"]["id"]

    cals = api.req("GET", "/admin/business-calendars", adm)[1]["data"]["calendars"]
    default = next(c for c in cals if c.get("isDefault"))
    original = {k: default[k] for k in ("name", "timezone", "hours", "holidays", "isDefault")}
    try:
        # (1) test@ sends Q1; while it waits, the detail already measures it.
        i1 = api.submit(staff, api.new_quote(staff, "S2-Q1 thời gian duyệt"))
        c, a = api.detail(d1, i1)
        dur = a.get("durations") or {}
        check("Q1 đang chờ: có durations, căn cứ business, bước đang mở chưa có endedAt",
              c == 200 and a.get("status") == "pending" and dur.get("basis") == "business"
              and len(dur.get("perNode", [])) == 1 and "endedAt" not in dur["perNode"][0], json.dumps(dur)[:160])

        # (2) duyet1 rejects with no reason, by their own token: refused, the instance untouched.
        for label, body in (("không body", None), ("note rỗng", {"note": ""}),
                            ("note khoảng trắng", {"note": "   "}), ("note U+0085", {"note": "\u0085"})):
            c, b = api.req("POST", f"/approvals/{i1}/reject", d1, body)
            check(f"duyet1 từ chối {label} ⇒ 422 APPROVAL_REJECT_REASON_REQUIRED",
                  c == 422 and b.get("code") == "APPROVAL_REJECT_REASON_REQUIRED", f"{c} {b.get('code')}")
        c, a = api.detail(d1, i1)
        check("Q1 vẫn chờ, lịch sử không có mục từ chối",
              a.get("status") == "pending" and not any(h["action"] == "rejected" for h in a.get("history", [])), str(a.get("status")))

        # (3) Two real minute boundaries later, duyet1 approves.
        created = parse(a["createdAt"])
        gap_start = (created + dt.timedelta(minutes=1)).replace(second=0, microsecond=0)
        gap_end = gap_start + dt.timedelta(minutes=1)
        if gap_end.astimezone(ICT).date() != created.astimezone(ICT).date():
            raise SystemExit("khoảng nghỉ vắt qua nửa đêm — chạy lại sau 00:05")
        wait = (gap_end + dt.timedelta(seconds=25) - dt.datetime.now(dt.timezone.utc)).total_seconds()
        print(f"gửi lúc {created.astimezone(ICT):%H:%M:%S}; khoảng nghỉ {gap_start.astimezone(ICT):%H:%M}–"
              f"{gap_end.astimezone(ICT):%H:%M}; chờ {wait:.0f}s rồi duyệt")
        time.sleep(max(0, wait))
        c, _ = api.req("POST", f"/approvals/{i1}/approve", d1, {"note": "ok"})
        check("duyet1 duyệt Q1", c == 200, str(c))

        # (4) The third person edits the clone's calendar: today works until gap_start, resumes at gap_end.
        today = created.astimezone(ICT)
        key = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"][today.weekday()]
        hours = {k: [{"start": "08:00", "end": "17:00"}] for k in ("mon", "tue", "wed", "thu", "fri")}
        hours.update({"sat": [], "sun": []})
        hours[key] = [{"start": "00:00", "end": f"{gap_start.astimezone(ICT):%H:%M}"},
                      {"start": f"{gap_end.astimezone(ICT):%H:%M}", "end": "24:00"}]
        c, b = api.req("PUT", f"/admin/business-calendars/{default['id']}", adm, dict(original, hours=hours))
        assert c == 200, (c, b)
        print(f"lịch: {key} 00:00–{gap_start.astimezone(ICT):%H:%M} + {gap_end.astimezone(ICT):%H:%M}–24:00")

        c, a = api.detail(d1, i1)
        mark = next((h for h in a.get("history", []) if h["action"] == "approved"), None)
        check("Q1 kết thúc 'approved' với mốc duyệt trong lịch sử", a.get("status") == "approved" and mark is not None, str(a.get("status")))
        if mark is None:
            return
        decided = parse(mark["at"])
        want_biz = int((gap_start - created).total_seconds() + (decided - gap_end).total_seconds())
        want_wall = int((decided - created).total_seconds())
        dur = a.get("durations") or {}
        tot_b, tot_w = dur.get("totalBusinessSeconds"), dur.get("totalWallSeconds")
        check("Q1 (duyet1): thời gian làm việc = tính tay từ lịch (bỏ khoảng nghỉ 1 phút)",
              dur.get("basis") == "business" and tot_b is not None and abs(tot_b - want_biz) <= 1,
              f"API {tot_b}s, tay {want_biz}s; giờ đồng hồ API {tot_w}s, tay {want_wall}s")
        check("Q1: giờ đồng hồ = gửi→duyệt, hơn giờ làm việc đúng 60s",
              tot_w is not None and tot_b is not None and abs(tot_w - want_wall) <= 1 and abs((tot_w - tot_b) - 60) <= 1)
        node = (dur.get("perNode") or [{}])[0]
        check("Q1 perNode: s1 'Trưởng phòng', đóng tại mốc duyệt",
              node.get("node") == "s1" and node.get("stepName") == "Trưởng phòng"
              and bool(node.get("endedAt")) and parse(node["endedAt"]) == decided, json.dumps(node, ensure_ascii=False)[:200])
        check("Q1 lịch sử mang tên bước (dòng thời gian hiện tên, không mã)",
              all(h.get("stepName") == "Trưởng phòng" for h in a["history"] if h["action"] in ("submitted", "approved")))

        c, s = api.detail(staff, i1)
        check("Q1 (test@example.com, người gửi): cùng con số",
              c == 200 and (s.get("durations") or {}).get("totalBusinessSeconds") == tot_b, f"{c}")
        c, o = api.detail(outsider, i1)
        check("Q1 (hotro1, người ngoài) ⇒ 404 APPROVAL_INSTANCE_NOT_FOUND",
              c == 404 and o.get("code") == "APPROVAL_INSTANCE_NOT_FOUND", f"{c} {o.get('code')}")

        # (5) A reason given goes through.
        i2 = api.submit(staff, api.new_quote(staff, "S2-Q2 từ chối có lý do"))
        c, b = api.req("POST", f"/approvals/{i2}/reject", d1, {"note": "Sai đơn giá"})
        check("duyet1 từ chối Q2 có lý do ⇒ 200, hồ sơ 'rejected'",
              c == 200 and b["data"]["approval"]["status"] == "rejected", f"{c} {b.get('code', '')}")
        c, a = api.detail(staff, i2)
        check("Q2 (người gửi) thấy lý do + thời gian",
              c == 200 and any(h.get("comment") == "Sai đơn giá" for h in a.get("history", []))
              and (a.get("durations") or {}).get("totalWallSeconds") is not None, str(c))

        # (6) No default calendar: wall clock, labelled, and no working-time figure at all.
        c, b = api.req("PUT", f"/admin/business-calendars/{default['id']}", adm, dict(original, isDefault=False))
        assert c == 200, (c, b)
        c, a = api.detail(d1, i1)
        dur = a.get("durations") or {}
        check("không lịch mặc định: căn cứ wall_no_calendar, KHÔNG có số giờ làm việc, giờ đồng hồ giữ nguyên",
              dur.get("basis") == "wall_no_calendar" and "totalBusinessSeconds" not in dur and dur.get("totalWallSeconds") == tot_w,
              json.dumps({k: v for k, v in dur.items() if k != "perNode"}))
    finally:
        c, _ = api.req("PUT", f"/admin/business-calendars/{default['id']}", adm, original)
        check("finally: lịch trả nguyên trạng", c == 200, str(c))
        c, _ = api.req("PUT", f"/admin/approval-processes/{proc_id}", adm, {
            "name": PROC, "entityName": "crm_quotes", "trigger": "manual", "isActive": False, "steps": [step]})
        check("finally: tắt quy trình nghiệm thu", c == 200, str(c))
        c, b = api.req("GET", f"/admin/business-calendars/{default['id']}", adm)
        cal = (b.get("data") or {}).get("calendar", b.get("data") or {})
        check("lịch sau finally khớp bản gốc", cal.get("hours") == original["hours"] and cal.get("isDefault") == original["isDefault"])


def main():
    ap = argparse.ArgumentParser(description="Nghiệm thu không-admin specs/043 S2 (#40).")
    ap.add_argument("--base", required=True, help="vd http://localhost:8134/api/v1 (server trên DB bản sao)")
    run(API(ap.parse_args().base.rstrip("/")))
    bad = [n for n, ok in results if not ok]
    print(f"\n{len(results) - len(bad)}/{len(results)} PASS")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
