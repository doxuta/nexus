#!/usr/bin/env python3
"""Nghiệm thu không-admin lát S8 của specs/043 (`bpm-reject-return`, DAILY-QUEUE #121, T143).

Từ chối thì TRẢ VỀ (bước trước hoặc người gửi sửa lại) thay vì kết thúc — bằng token NGƯỜI THẬT
không-admin (duyet1/duyet2 vai crm_sales_manager, test@ nhân viên gửi, hotro1 người ngoài). Chạy trên
server dựng từ DB CLONE (không bao giờ DB dev dùng chung), sau `scripts/fixture-bpm.py`:

  Quy trình thủ công trên crm_quotes: s1 «Trưởng phòng» (duyet1) → s2 «Kế toán» (duyet2).

  1. «từ chối → về người gửi» (outcome.returnedValue = «Cần bổ sung»): duyet2 từ chối lý do rỗng ⇒ 422
     APPROVAL_REJECT_REASON_REQUIRED, hồ sơ vẫn chờ; có lý do ⇒ 200, hồ sơ VẪN pending; test@ thấy việc
     «Sửa lại và gửi lại» ở view=mine kèm lý do (kind rework), báo giá thành «Cần bổ sung»; chi tiết
     /approvals/:id của test@ báo awaitingRework; duyet2 không duyệt được nữa; test@ sửa báo giá ⇒ KHÔNG có
     hồ sơ thứ hai; test@ Gửi lại (complete) ⇒ việc về hộp duyet2 với hạn mới, mục lịch sử «resubmitted».
  2. «từ chối → về bước s1»: duyet2 từ chối ⇒ việc về hộp duyet1; lệnh Duyệt CŨ của duyet2 (thấy bước 2) ⇒ 409
     APPROVAL_CONFLICT; hotro1 gọi thẳng ⇒ không lộ (404/403, không 409); duyet1 duyệt lại ⇒ lại tới duyet2.
  3. «về người gửi, gửi lại → làm lại từ đầu» (resumeAt start): Gửi lại ⇒ việc về duyet1.
  4. test@ thu hồi một hồ sơ đang ở người gửi ⇒ cancelled, ghế rework đóng.
  5. Cấu hình sai bị chặn ở API: đích lạ, đích là bước SAU, returnedValue không ai ghi.

    python3 scripts/fixture-bpm.py --base http://localhost:8703/api/v1
    python3 scripts/acceptance-bpm-reject-return.py --base http://localhost:8703/api/v1 --db nexus_wt_bpm_reject_return

Mã thoát: 0 = đạt · 1 = một kiểm tra hỏng.

ponytail: stdlib + `docker exec nexus-mysql mysql` (cùng khuôn acceptance-bpm-work-items.py, mà script này nạp lại).
"""
import argparse
import importlib.util
import os
import sys

_spec = importlib.util.spec_from_file_location(
    "acceptance_bpm_seats", os.path.join(os.path.dirname(os.path.abspath(__file__)), "acceptance-bpm-seats.py"))
seats_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(seats_mod)
Env, check, fails = seats_mod.Env, seats_mod.check, seats_mod.fails

PROC = "S8 nghiệm thu — báo giá trả về"
RETURNED = "Cần bổ sung"


def code(resp):
    return resp[1].get("code")


def steps(on_reject, sla2=None):
    return [{"id": "s1", "name": "Trưởng phòng", "approverType": "user", "approverId": "__d1__"},
            {"id": "s2", "name": "Kế toán", "approverType": "user", "approverId": "__d2__", "onReject": on_reject}]


class Run:
    def __init__(self, env):
        self.env = env
        self.t = env.tokens()
        users = {u["email"].lower(): u["id"] for u in env.req("GET", "/admin/users", self.t["admin"])[1]["data"]["users"]}
        self.d1, self.d2 = users["duyet1@example.com"], users["duyet2@example.com"]
        self.staff_id = users["test@example.com"]
        self.proc = None

    def body(self, on_reject, returned=True, name=PROC):
        st = steps(on_reject)
        st[0]["approverId"], st[1]["approverId"] = self.d1, self.d2
        out = {"name": name, "entityName": "crm_quotes", "trigger": "manual", "isActive": True, "steps": st,
               "outcome": {"statusField": "status", "approvedValue": "Chấp nhận"}}
        if returned:
            out["outcome"]["returnedValue"] = RETURNED
        return out

    def save(self, on_reject, returned=True):
        e, t = self.env, self.t
        if self.proc is None:
            c, b = e.req("POST", "/admin/approval-processes", t["admin"], self.body(on_reject, returned))
            assert c == 201, (c, b)
            self.proc = b["data"]["process"]["id"]
        else:
            c, b = e.req("PUT", f"/admin/approval-processes/{self.proc}", t["admin"], self.body(on_reject, returned))
            assert c == 200, (c, b)

    def quote(self, name):
        """test@ creates + submits a quote; returns (record id, instance id)."""
        e = self.env
        iid = e.send(self.t["staff"], name)
        rid = e.sql(f"SELECT record_id FROM sys_approval_instances WHERE tenant_id = '{e.tenant}' AND id = '{iid}'")[0][0]
        return rid, iid

    def status_of(self, rid):
        c, b = self.env.req("GET", f"/crm_quotes/{rid}", self.t["staff"])
        assert c == 200, (c, b)
        rec = b["data"].get("record") or b["data"]
        return (rec.get("data") or rec).get("status")

    def inst(self, iid):
        return self.env.sql(f"SELECT status, current_step, IFNULL(current_approver_id,''), sla_due_at IS NOT NULL "
                            f"FROM sys_approval_instances WHERE tenant_id = '{self.env.tenant}' AND id = '{iid}'")[0]

    def history(self, iid):
        import json
        return json.loads(self.env.sql(f"SELECT history FROM sys_approval_instances WHERE tenant_id = '{self.env.tenant}' AND id = '{iid}'")[0][0])

    def mine(self, tok, iid):
        c, b = self.env.req("GET", "/work-items?view=mine&limit=200", tok)
        assert c == 200, (c, b)
        return next((i for i in b["data"]["items"] if i["instanceId"] == iid), None)

    def instances_of(self, rid):
        return int(self.env.sql(f"SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = '{self.env.tenant}' "
                                f"AND entity_name = 'crm_quotes' AND record_id = '{rid}'")[0][0])


def scenario_submitter(r):
    e, t = r.env, r.t
    r.save({"to": "@submitter"})
    rid, iid = r.quote("S8-về người gửi")
    c, _ = e.req("POST", f"/approvals/{iid}/approve", t["duyet1"], {"note": "ok"})
    assert c == 200
    # An empty reason is refused with the approval untouched.
    for note in ("", "   "):
        x = e.req("POST", f"/approvals/{iid}/reject", t["duyet2"], {"note": note})
        check(f"duyet2 từ chối lý do {note!r} ⇒ 422 APPROVAL_REJECT_REASON_REQUIRED", x[0] == 422 and code(x) == "APPROVAL_REJECT_REASON_REQUIRED", f"{x[0]} {x[1]}")
    st = r.inst(iid)
    check("hồ sơ vẫn chờ duyet2 sau lần từ chối bị chặn", st[0] == "pending" and st[1] == "1" and st[2] == r.d2, str(st))

    x = e.req("POST", f"/approvals/{iid}/reject", t["duyet2"], {"note": "Sai đơn giá", "expectedStep": 1})
    check("duyet2 từ chối có lý do ⇒ 200", x[0] == 200, f"{x[0]} {x[1]}")
    st = r.inst(iid)
    check("hồ sơ VẪN pending, đứng ở bước 2, ghế người gửi, không hạn", st[0] == "pending" and st[1] == "1" and st[2] == r.staff_id and st[3] == "0", str(st))
    item = r.mine(t["staff"], iid)
    check("test@ thấy việc «Sửa lại và gửi lại» ở view=mine: kind rework, kèm lý do, tên bước (không mã)",
          bool(item) and item["kind"] == "rework" and item.get("returnReason") == "Sai đơn giá" and item["nodeName"] == "Kế toán", f"{item}")
    check("báo giá thành «Cần bổ sung» (outcome.returnedValue)", r.status_of(rid) == RETURNED, str(r.status_of(rid)))
    c, d = e.req("GET", f"/approvals/{iid}", t["staff"])
    dd = d.get("data", {}).get("approval") or d.get("data", {})
    check("chi tiết /approvals/:id của test@ báo awaitingRework + lý do",
          c == 200 and dd.get("awaitingRework") is True and dd.get("returnReason") == "Sai đơn giá", f"{c} {dd.get('awaitingRework')}")
    last = r.history(iid)[-1]
    check("lịch sử: rejected, returnedTo @submitter, target = test@", last["action"] == "rejected" and last.get("returnedTo") == "@submitter" and last.get("target") == r.staff_id, str(last))
    x = e.req("POST", f"/approvals/{iid}/approve", t["duyet2"], {"note": "x"})
    check("duyet2 không duyệt được khi hồ sơ đang ở người gửi (ghế không phải của họ)", x[0] in (403, 404, 409, 422) and x[0] != 200, f"{x[0]} {x[1]}")
    x = e.req("POST", f"/approvals/{iid}/approve", t["staff"], {"note": "tự duyệt"})
    check("test@ không tự duyệt hồ sơ của mình ⇒ 422 APPROVAL_STEP_KIND_MISMATCH", x[0] == 422 and code(x) == "APPROVAL_STEP_KIND_MISMATCH", f"{x[0]} {x[1]}")
    check("duyet2 không còn thấy hồ sơ ở hộp duyệt", iid not in e.pending(t["duyet2"]))

    # The submitter fixes the quote: no second approval is armed.
    x = e.req("PUT", f"/crm_quotes/{rid}", t["staff"], {"name": "S8-về người gửi (đã sửa)"})
    check("test@ sửa báo giá ⇒ 200", x[0] == 200, f"{x[0]} {x[1]}")
    check("sửa báo giá KHÔNG sinh hồ sơ duyệt thứ hai", r.instances_of(rid) == 1, str(r.instances_of(rid)))

    x = e.req("POST", f"/work-items/{item['id']}/complete", t["staff"], {"note": "đã sửa đơn giá"})
    check("test@ Gửi lại (complete trên việc sửa lại) ⇒ 200", x[0] == 200, f"{x[0]} {x[1]}")
    st = r.inst(iid)
    check("hồ sơ về bước 2 của duyet2 với hạn mới là tuỳ cấu hình", st[0] == "pending" and st[1] == "1" and st[2] == r.d2, str(st))
    check("hồ sơ lại nằm ở hộp duyệt của duyet2", iid in e.pending(t["duyet2"]))
    last = r.history(iid)[-1]
    check("lịch sử: resubmitted bởi test@ kèm ghi chú", last["action"] == "resubmitted" and last["actor"] == r.staff_id and last.get("comment") == "đã sửa đơn giá", str(last))
    seats = e.seats(iid)
    check("ghế: rework đã completed, ghế duyệt mới của duyet2 đang mở",
          any(s[1] == "done" and s[2] == "completed" for s in seats) and any(s[1] == "open" and s[3] == r.d2 for s in seats), str(seats))
    x = e.req("POST", f"/approvals/{iid}/approve", t["duyet2"], {"note": "ok"})
    check("duyet2 duyệt bản đã sửa ⇒ 200, hồ sơ approved", x[0] == 200 and r.inst(iid)[0] == "approved", f"{x[0]} {x[1]}")
    check("báo giá thành «Chấp nhận»", r.status_of(rid) == "Chấp nhận", str(r.status_of(rid)))
    check("đến cuối vẫn đúng MỘT hồ sơ duyệt cho báo giá", r.instances_of(rid) == 1, str(r.instances_of(rid)))


def scenario_node(r):
    e, t = r.env, r.t
    r.save({"to": "s1"}, returned=False)
    rid, iid = r.quote("S8-về bước 1")
    e.req("POST", f"/approvals/{iid}/approve", t["duyet1"], {"note": "ok"})
    before, status_before = e.seats(iid), r.status_of(rid)
    x = e.req("POST", f"/approvals/{iid}/reject", t["duyet2"], {"note": "thiếu chứng từ", "expectedStep": 1})
    check("duyet2 từ chối ⇒ 200", x[0] == 200, f"{x[0]} {x[1]}")
    st = r.inst(iid)
    check("việc về hộp duyet1, bước 1, hạn mới (cấu hình wall không SLA ⇒ có thể không hạn), vẫn pending",
          st[0] == "pending" and st[1] == "0" and st[2] == r.d1, str(st))
    check("hộp duyet1 có hồ sơ; hộp duyet2 hết", iid in e.pending(t["duyet1"]) and iid not in e.pending(t["duyet2"]))
    seats = e.seats(iid)
    s1_entries = sorted({s[5] for s in seats if s[0] == "s1"})
    check("bước 1 là MỘT LẦN VÀO MỚI (visit): hai entry_seq khác nhau", len(s1_entries) == 2 and int(s1_entries[1]) > int(s1_entries[0]), f"{before} → {seats}")
    check("báo giá không đổi trạng thái (không returnedValue, không phải kết thúc)", r.status_of(rid) == status_before, f"{status_before} → {r.status_of(rid)}")
    x = e.req("POST", f"/approvals/{iid}/approve", t["duyet2"], {"note": "bấm lại từ tab cũ", "expectedStep": 1})
    check("lệnh Duyệt CŨ của duyet2 (màn hình thấy bước 2) ⇒ 409 APPROVAL_CONFLICT", x[0] == 409 and code(x) == "APPROVAL_CONFLICT", f"{x[0]} {x[1]}")
    x = e.req("POST", f"/approvals/{iid}/approve", t["hotro1"], {"note": "tò mò", "expectedStep": 1})
    check("hotro1 (chưa từng có ghế) gọi cùng lệnh ⇒ KHÔNG 409 (không lộ bước hiện tại)", x[0] != 409 and x[0] != 200, f"{x[0]} {x[1]}")
    check("hồ sơ không nhúc nhích sau hai lệnh bị từ chối", r.inst(iid)[1] == "0" and r.inst(iid)[0] == "pending")
    x = e.req("POST", f"/approvals/{iid}/approve", t["duyet1"], {"note": "ok lần hai"})
    check("duyet1 duyệt lại ⇒ 200, lại tới duyet2", x[0] == 200 and r.inst(iid)[2] == r.d2 and iid in e.pending(t["duyet2"]), f"{x[0]} {r.inst(iid)}")
    x = e.req("POST", f"/approvals/{iid}/approve", t["duyet2"], {"note": "ok"})
    check("duyet2 duyệt ⇒ approved", x[0] == 200 and r.inst(iid)[0] == "approved", f"{x[0]} {x[1]}")


def scenario_start(r):
    e, t = r.env, r.t
    r.save({"to": "@submitter", "resumeAt": "start"}, returned=False)
    rid, iid = r.quote("S8-làm lại từ đầu")
    e.req("POST", f"/approvals/{iid}/approve", t["duyet1"], {"note": "ok"})
    e.req("POST", f"/approvals/{iid}/reject", t["duyet2"], {"note": "làm lại"})
    item = r.mine(t["staff"], iid)
    check("việc sửa lại hiện ở test@", bool(item) and item["kind"] == "rework", f"{item}")
    x = e.req("POST", f"/work-items/{item['id']}/complete", t["staff"], {})
    st = r.inst(iid)
    check("Gửi lại với resumeAt=start ⇒ về duyet1 (bước 1)", x[0] == 200 and st[1] == "0" and st[2] == r.d1, f"{x[0]} {st}")
    # Recall during rework.
    rid2, iid2 = r.quote("S8-thu hồi khi sửa lại")
    e.req("POST", f"/approvals/{iid2}/approve", t["duyet1"], {"note": "ok"})
    e.req("POST", f"/approvals/{iid2}/reject", t["duyet2"], {"note": "sửa"})
    x = e.req("POST", f"/approvals/{iid2}/recall", t["staff"], {"note": "thôi"})
    check("test@ thu hồi hồ sơ đang ở mình ⇒ 200, cancelled", x[0] == 200 and r.inst(iid2)[0] == "cancelled", f"{x[0]} {x[1]}")
    check("ghế rework đóng (không còn việc sửa lại)", r.mine(t["staff"], iid2) is None and not [s for s in e.seats(iid2) if s[1] == "open"], str(e.seats(iid2)))
    e.req("POST", f"/approvals/{iid}/recall", t["staff"], {"note": "dọn"})


def scenario_config(r):
    e, t = r.env, r.t
    for name, onrej, returned in (
        ("đích lạ", {"to": "khong-co"}, False),
        ("đích là bước SAU (s2 trỏ s3 không có) ", {"to": "s2"}, False),
        ("resumeAt lạ", {"to": "@submitter", "resumeAt": "giua"}, False),
        ("returnedValue không ai ghi", {"to": "@end"}, True),
    ):
        b = r.body(onrej, returned, name=PROC + " lỗi " + name)
        b["isActive"] = False
        x = e.req("POST", "/admin/approval-processes", t["admin"], b)
        want = "APPROVAL_OUTCOME_VALUE_UNUSED" if returned else "APPROVAL_STEP_INVALID"
        check(f"cấu hình sai «{name}» ⇒ 422 {want}", x[0] == 422 and code(x) == want, f"{x[0]} {x[1]}")
    b = r.body({"to": "@submitter"}, True, name=PROC + " lỗi 2")
    b["steps"][0]["onReject"] = {"to": "s2"}
    b["isActive"] = False
    x = e.req("POST", "/admin/approval-processes", t["admin"], b)
    check("bước 1 trỏ về bước 2 (đứng SAU) ⇒ 422 APPROVAL_STEP_INVALID", x[0] == 422 and code(x) == "APPROVAL_STEP_INVALID", f"{x[0]} {x[1]}")


def picklist_status(e, admin):
    c, b = e.req("GET", "/metadata/fields/crm_quotes", admin)
    assert c == 200, (c, b)
    flds = b["data"]["fields"] if isinstance(b["data"], dict) else b["data"]
    return next(f for f in flds if f["name"] == "status")


def run(env):
    r = Run(env)
    procs = env.req("GET", "/admin/approval-processes", r.t["admin"])[1]["data"]["processes"]
    rivals = [p["name"] for p in procs if p["entityName"] == "crm_quotes" and p["trigger"] == "manual" and p["isActive"]]
    assert not rivals, f"another active manual crm_quotes process would take the submit: {rivals}"
    # "Cần bổ sung" is not an option of crm_quotes.status out of the box: add it for the run (restored after).
    f = picklist_status(env, r.t["admin"])
    opts = dict(f.get("options") or {})
    values = list(opts.get("values") or [])
    added = RETURNED not in values
    if added:
        c, b = env.req("PUT", f"/admin/fields/{f['id']}", r.t["admin"], {"fieldType": "picklist", "options": {**opts, "values": values + [RETURNED]}})
        assert c == 200, (c, b)
    try:
        scenario_config(r)
        scenario_submitter(r)
        scenario_node(r)
        scenario_start(r)
    finally:
        if r.proc:
            env.req("DELETE", f"/admin/approval-processes/{r.proc}", r.t["admin"])
        if added:
            env.req("PUT", f"/admin/fields/{f['id']}", r.t["admin"], {"fieldType": "picklist", "options": {**opts, "values": values}})


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--db", required=True)
    a = ap.parse_args()
    if a.db == "nexus_platform":
        sys.exit("refusing the shared dev DB — clone it first (nexus_wt_<slug>)")
    run(Env(a.base, a.db))
    print("KẾT QUẢ:", "ĐẠT" if not fails else f"CHƯA ĐẠT ({len(fails)}): {fails}")
    sys.exit(1 if fails else 0)
