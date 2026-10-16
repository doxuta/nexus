#!/usr/bin/env python3
"""rowscope-by-user-field (#283) — third-person acceptance.

The journey: a clinic has a "lịch hẹn" table with a Người dùng field "Bác sĩ". The receptionist
creates every appointment (so she owns them); doctors must see only the appointments assigned to
them, WITHOUT the receptionist re-assigning each one ("Đổi chủ").

An ADMIN (the builder) creates the table + a role whose view/edit grants are "field:bac_si" +
two NON-ADMIN doctors and a receptionist. Then, as the doctors (never as admin — admin bypasses
ACL, so only a third person can hit the wall):
  - list shows exactly their own appointments, count agrees;
  - opening another doctor's appointment by id is 404; editing it is 404;
  - editing their own works; reassigning it to the other doctor makes it vanish from their list;
  - the receptionist (owner level) still sees what she created, not what a doctor created.
Everything created is removed in finally. Run against a server you own:
    python3 scripts/acceptance-rowscope-field.py --base http://localhost:8503/api/v1
"""
import argparse
import json
import sys
import urllib.error
import urllib.request
import uuid

ap = argparse.ArgumentParser()
ap.add_argument("--base", default="http://localhost:8090/api/v1")
ap.add_argument("--tenant", default="northwind")
args = ap.parse_args()
BASE = args.base
if not BASE.startswith(("http://localhost", "http://127.0.0.1")):
    sys.exit("HỦY: script này tạo/xoá bảng, vai và tài khoản — chỉ chạy với server loopback")


def call(method, path, token=None, body=None):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data) as r:
            payload = r.read()
            return r.status, (json.loads(payload) if payload else {})
    except urllib.error.HTTPError as e:
        payload = e.read()
        try:
            return e.code, json.loads(payload) if payload else {}
        except ValueError:
            return e.code, {"raw": payload.decode(errors="replace")}


def login(email, password):
    st, body = call("POST", "/auth/login", body={"tenant": args.tenant, "email": email, "password": password})
    assert st == 200, f"login {email}: {st} {body}"
    return body["data"]["accessToken"]


fails = []


def check(label, cond, detail=""):
    print(("  PASS  " if cond else "  FAIL  ") + label + (f"   {detail}" if detail else ""))
    if not cond:
        fails.append(label)


def rows(body):
    d = body.get("data", body)
    if isinstance(d, dict):
        d = d.get("records") or d.get("items") or []
    return d


tag = uuid.uuid4().hex[:6]
ENTITY = f"zz_lich_hen_{tag}"
made = {"entity": False, "roles": [], "users": []}
admin = login("admin@example.com", "Demo-Admin-2026!")
try:
    st, body = call("POST", "/admin/entities", admin, {
        "name": ENTITY, "label": "Lịch hẹn (nghiệm thu)",
        "fields": [
            {"name": "ten", "label": "Tên", "fieldType": "text"},
            {"name": "bac_si", "label": "Bác sĩ", "fieldType": "user"},
        ],
    })
    assert st in (200, 201), f"create entity: {st} {body}"
    made["entity"] = True

    def mkrole(suffix, grants):
        st, b = call("POST", "/admin/roles", admin, {"name": f"zz_{suffix}_{tag}", "label": suffix, "actions": grants})
        assert st in (200, 201), f"create role {suffix}: {st} {b}"
        rid = (b.get("data", {}).get("role") or b.get("data") or {}).get("id")
        made["roles"].append(rid)
        return rid

    # ---- the builder-facing contract: invalid field levels are refused on save --------------
    st, b = call("POST", "/admin/roles", admin, {"name": f"zz_bad_{tag}", "label": "bad", "actions": [
        {"entityName": ENTITY, "action": "view", "accessLevel": "field:ten"}]})
    check("trường không phải Người dùng bị từ chối (422 ROLE_FIELD_SCOPE_INVALID)",
          st == 422 and b.get("code") == "ROLE_FIELD_SCOPE_INVALID", f"{st} {b.get('code')}")
    st, b = call("POST", "/admin/roles", admin, {"name": f"zz_bad2_{tag}", "label": "bad", "actions": [
        {"entityName": "*", "action": "view", "accessLevel": "field:bac_si"}]})
    check("dấu * không mang được mức theo trường (422)", st == 422 and b.get("code") == "ROLE_FIELD_SCOPE_INVALID", f"{st} {b.get('code')}")

    doctor_role = mkrole("bac_si", [
        {"entityName": ENTITY, "action": "view", "accessLevel": f"field:bac_si"},
        {"entityName": ENTITY, "action": "edit", "accessLevel": "field:bac_si"},
    ])
    desk_role = mkrole("le_tan", [
        {"entityName": ENTITY, "action": "view", "accessLevel": "owner"},
        {"entityName": ENTITY, "action": "create", "accessLevel": "all"},
        {"entityName": ENTITY, "action": "edit", "accessLevel": "owner"},
    ])

    def mkuser(who, role):
        email, pw = f"{who}-{tag}@example.com", "Demo-Check-2026!-" + tag
        st, b = call("POST", "/admin/users", admin, {"email": email, "password": pw, "fullName": who, "roleId": role})
        assert st in (200, 201), f"create user {who}: {st} {b}"
        u = (b.get("data", {}).get("user") or b.get("data") or {})
        made["users"].append(u["id"])
        return u["id"], login(email, pw)

    docA_id, docA = mkuser("bacsi-a", doctor_role)
    docB_id, docB = mkuser("bacsi-b", doctor_role)
    desk_id, desk = mkuser("letan", desk_role)

    # The receptionist creates 3 appointments: two for doctor A, one for doctor B.
    ids = {}
    for key, doc in (("a1", docA_id), ("a2", docA_id), ("b1", docB_id)):
        st, b = call("POST", f"/{ENTITY}", desk, {"ten": key, "bac_si": doc})
        assert st in (200, 201), f"receptionist create {key}: {st} {b}"
        ids[key] = (b.get("data", b)).get("id")

    # ---- doctors: list / get / edit ----------------------------------------------------------
    st, b = call("GET", f"/{ENTITY}", docA)
    got = sorted(r.get("data", r).get("ten") for r in rows(b)) if st == 200 else None
    check("bác sĩ A chỉ thấy 2 lịch giao cho mình, không cần đổi chủ", st == 200 and got == ["a1", "a2"], f"{st} {got}")
    st, b = call("GET", f"/{ENTITY}", docB)
    got = sorted(r.get("data", r).get("ten") for r in rows(b)) if st == 200 else None
    check("bác sĩ B chỉ thấy lịch của B", st == 200 and got == ["b1"], f"{st} {got}")

    st, _ = call("GET", f"/{ENTITY}/{ids['b1']}", docA)
    check("mở lịch của bác sĩ khác bằng id → 404", st == 404, f"{st}")
    st, _ = call("PUT", f"/{ENTITY}/{ids['b1']}", docA, {"ten": "hack"})
    check("sửa lịch của bác sĩ khác → 404", st == 404, f"{st}")
    st, _ = call("GET", f"/{ENTITY}/{ids['a1']}", docA)
    check("mở lịch giao cho mình → 200", st == 200, f"{st}")
    st, _ = call("PUT", f"/{ENTITY}/{ids['a1']}", docA, {"ten": "a1-da-kham"})
    check("sửa lịch giao cho mình → 200", st == 200, f"{st}")

    # ---- the receptionist is unchanged: owner level sees what she made --------------------------
    st, b = call("GET", f"/{ENTITY}", desk)
    check("lễ tân (mức Chỉ mình) vẫn thấy 3 lịch cô tạo", st == 200 and len(rows(b)) == 3, f"{st} {len(rows(b)) if st == 200 else ''}")

    # ---- reassign away: the row leaves doctor A's list -------------------------------------------
    st, _ = call("PUT", f"/{ENTITY}/{ids['a2']}", admin, {"bac_si": docB_id})
    assert st == 200, f"admin reassign: {st}"
    st, b = call("GET", f"/{ENTITY}", docA)
    got = sorted(r.get("data", r).get("ten") for r in rows(b)) if st == 200 else None
    check("giao lại cho bác sĩ B thì lịch rời khỏi danh sách của A", got == ["a1-da-kham"], f"{got}")
    st, _ = call("GET", f"/{ENTITY}/{ids['a2']}", docA)
    check("…và mở bằng id là 404", st == 404, f"{st}")
finally:
    for uid in made["users"]:
        call("DELETE", f"/admin/users/{uid}", admin)
    for rid in made["roles"]:
        call("DELETE", f"/admin/roles/{rid}", admin)
    if made["entity"]:
        st, _ = call("DELETE", f"/admin/entities/{ENTITY}", admin)
        if st not in (200, 204):
            print(f"  CẢNH BÁO: không xoá được bảng {ENTITY} ({st}) — xoá tay ở Studio", file=sys.stderr)

print("\nĐẠT" if not fails else f"\nCHƯA ĐẠT: {len(fails)} mục — {fails}")
sys.exit(0 if not fails else 1)
