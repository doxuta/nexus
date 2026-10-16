#!/usr/bin/env python3
"""Mẫu của gói ngành có sẵn (specs/064-preview-as-role-sample-data S9-6, T106) — nghiệm thu trên server THẬT.

Kịch bản (người dựng admin@example.com mở phiên dựng vào hai không gian khách MỚI, tạo riêng cho lần chạy):
  1. Khách A: cài gói CRM có sẵn ⇒ thẻ đếm đúng số dòng mẫu của gói, mọi dòng nguồn "pack" (0 generator/ai).
  2. Khách B: cài gói JSON xuất từ A kèm `includeData` ⇒ có dòng thật nhưng 0 dấu; Sinh "Từ gói" ⇒ 422
     SAMPLE_PACK_UNAVAILABLE (gói tải lên không phải nguồn).
  3. Khách A: gỡ sạch mẫu (đứng thay S9-7 "xoá mẫu": SQL trên DB bản sao) rồi Sinh "Từ gói" ⇒ mẫu về lại:
     đúng số dòng từng bảng, `@ref` đúng (mọi liên hệ trỏ tới khách hàng CÓ THẬT của A), đủ liên kết N-N.
  4. Sinh "Từ gói" lần hai ⇒ mọi bảng bị bỏ qua SAMPLE_ALREADY_PRESENT, số dòng không đổi.
  5. "Tự động" với một bảng dựng tay (không có trong gói) ⇒ bảng của gói bị bỏ qua, bảng dựng tay sinh theo trường;
     "Từ gói" chỉ bảng dựng tay ⇒ 422; nguồn lạ ⇒ 422 SAMPLE_SOURCE_INVALID.
  6. Người thứ ba: quản trị CỦA KHÁCH A (token thường, không phải phiên dựng) và nhân viên không-admin
     (test@example.com, tenant northwind) gọi route sinh ⇒ 403.

An toàn: `--base` phải là loopback; `--db` phải mang tiền tố `nexus_wt_`; không in token/mật khẩu. Không dọn
tenant tạo ra (DB bản sao sẽ bị bỏ cuối chiến dịch). Mã thoát: 0 đạt · 1 chưa đạt · 2 lỗi môi trường.

Dùng (từ nexus-platform/): PORT=8428 ./bin/server (DATABASE_URL → bản sao) rồi
    python3 scripts/acceptance-sample-from-pack.py --base http://localhost:8428/api/v1 --db nexus_wt_sample_from_pack

ponytail: stdlib thuần — tiền lệ acceptance-owner-tier.py.
"""

import argparse
import ipaddress
import json
import os
import re
import secrets
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request

failures = []


def check(ok, label, detail=""):
    print(f"  {'OK  ' if ok else 'ĐỎ  '} {label}" + (f" — {detail}" if detail and not ok else ""))
    if not ok:
        failures.append(label)
    return ok


def env_fail(msg):
    print(f"LỖI MÔI TRƯỜNG: {msg}", file=sys.stderr)
    sys.exit(2)


def req(method, url, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    r = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=60) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.load(e)
        except Exception:
            return e.code, {}
    except Exception as e:
        env_fail(f"không gọi được {url} — {e}")


def login(base, tenant, email, password):
    code, body = req("POST", base + "/auth/login", body={"tenant": tenant, "email": email, "password": password})
    tok = (body.get("data") or {}).get("accessToken")
    if code != 200 or not tok:
        env_fail(f"đăng nhập {email}@{tenant} thất bại ({code} {body.get('code', '')})")
    return tok


def sql(db, stmt):
    out = subprocess.run(["docker", "exec", "nexus-mysql", "mysql", "-uroot", "-N", "-B", db, "-e", stmt],
                         capture_output=True, text=True, timeout=60)
    if out.returncode != 0:
        env_fail(f"DB {db}: {out.stderr.strip()[:200]}")
    return out.stdout.strip()


def new_customer(base, home, db, label):
    slug = f"spk-{label}-{secrets.token_hex(3)}"
    code, body = req("POST", base + "/admin/tenants", home, {
        "slug": slug, "name": f"Khách nghiệm thu {label}", "adminEmail": f"owner@{slug}.example.com",
        "adminFullName": "Chủ nghiệm thu", "generatePassword": True,
    })
    d = body.get("data") or {}
    if code != 201:
        env_fail(f"tạo khách {slug} thất bại ({code} {body.get('code', '')})")
    pw = (d.get("oneTimeCredentials") or {}).get("password", "")
    code, body = req("POST", base + "/admin/workspace-sessions", home, {"tenantSlug": slug})
    ws = (body.get("data") or {}).get("accessToken")
    if code not in (200, 201) or not ws:
        env_fail(f"mở phiên dựng vào {slug} thất bại ({code} {body.get('code', '')})")
    return {"slug": slug, "id": d["tenant"]["id"], "ws": ws, "ownerEmail": f"owner@{slug}.example.com", "ownerPass": pw}


def summary(base, tok):
    code, body = req("GET", base + "/admin/sample-data", tok)
    if code != 200:
        env_fail(f"GET /admin/sample-data → {code}")
    return (body.get("data") or {})


def generate(base, tok, entities, source, per=5):
    return req("POST", base + "/admin/sample-data/generate", tok, {"entities": entities, "perEntity": per, "source": source})


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--db", required=True)
    ap.add_argument("--tenant", default="northwind")
    a = ap.parse_args()
    base = a.base.rstrip("/")
    host = urllib.parse.urlparse(base).hostname or ""
    try:
        loop = host == "localhost" or ipaddress.ip_address(host).is_loopback
    except ValueError:
        loop = False
    if not loop:
        env_fail("--base phải là loopback")
    if not re.match(r"^nexus_wt_[a-z0-9_]+$", a.db):
        env_fail("--db phải mang tiền tố nexus_wt_")

    home = login(base, a.tenant, "admin@example.com", os.environ.get("NEXUS_ADMIN_PASS") or "Demo-Admin-2026!")

    print("1. Khách A cài gói CRM có sẵn")
    A = new_customer(base, home, a.db, "a")
    code, body = req("POST", base + "/admin/verticals/install", A["ws"], {"builtin": "crm"})
    res = ((body.get("data") or {}).get("result")) or {}
    if not check(code in (200, 201), "cài gói CRM vào khách A", f"{code} {body.get('code', '')}"):
        sys.exit(1)
    inserted = res.get("samplesInserted", -1)
    s1 = summary(base, A["ws"])
    per_entity = {e["entity"]: e for e in s1.get("entities", []) if e["count"] > 0}
    check(inserted > 0 and s1.get("total") == inserted, "thẻ đếm đúng số dòng mẫu của gói", f"total={s1.get('total')} samplesInserted={inserted}")
    check(all(e["bySource"]["pack"] == e["count"] and e["bySource"]["generator"] == 0 and e["bySource"]["ai"] == 0 for e in per_entity.values()),
          "mọi dòng mẫu của gói mang nguồn 'pack' (0 generator, 0 ai)")
    snapshot = {k: v["count"] for k, v in per_entity.items()}
    print(f"   {snapshot}")

    print("2. Khách B nhận gói JSON tải lên kèm dữ liệu")
    code, body = req("GET", base + "/admin/verticals/export?includeData=true", A["ws"])
    pack = ((body.get("data") or {}).get("pack"))
    check(code == 200 and bool(pack) and len(pack.get("samples") or []) > 0, "xuất gói từ A kèm includeData", f"{code}")
    B = new_customer(base, home, a.db, "b")
    code, body = req("POST", base + "/admin/verticals/install", B["ws"], {"pack": pack})
    check(code in (200, 201), "cài gói tải lên vào khách B", f"{code} {body.get('code', '')}")
    code, body = req("GET", base + "/crm_accounts?limit=100", B["ws"])
    check(code == 200 and len(body.get("data") or []) > 0, "B có dòng thật từ includeData", f"{code}")
    check(summary(base, B["ws"]).get("total") == 0, "gói tải lên: 0 dấu mẫu")
    code, body = generate(base, B["ws"], ["crm_accounts"], "pack")
    check(code == 422 and body.get("code") == "SAMPLE_PACK_UNAVAILABLE", "B: Sinh 'Từ gói' ⇒ 422 SAMPLE_PACK_UNAVAILABLE", f"{code} {body.get('code', '')}")

    print("3. Khách A: gỡ mẫu rồi sinh lại từ gói")
    tid = A["id"]
    if not re.match(r"^[0-9a-f\-]{36}$", tid):
        env_fail("tenant id sai dạng")
    sql(a.db, f"DELETE FROM rel_crm_contacts_crm_opportunities WHERE tenant_id='{tid}'")
    for ent in snapshot:
        if not re.match(r"^[a-z0-9_]+$", ent):
            env_fail("tên bảng sai dạng")
        sql(a.db, f"DELETE FROM obj_{ent} WHERE tenant_id='{tid}'")
    sql(a.db, f"DELETE FROM sys_sample_records WHERE tenant_id='{tid}'")
    sql(a.db, f"DELETE FROM sys_unique_index WHERE tenant_id='{tid}'")  # the unique-value slots the rows held
    check(summary(base, A["ws"]).get("total") == 0, "sau khi gỡ: thẻ đếm 0")
    code, body = generate(base, A["ws"], list(snapshot), "pack")
    d = body.get("data") or {}
    check(code == 201, "A: Sinh 'Từ gói' ⇒ 201", f"{code} {body.get('code', '')}")
    created = {c["entity"]: c["count"] for c in d.get("created", [])}
    check(created == snapshot and not d.get("failed") and not d.get("skipped"), "đúng số dòng từng bảng, 0 lỗi, 0 bỏ qua", f"created={created} failed={d.get('failed')} skipped={d.get('skipped')}")
    s2 = summary(base, A["ws"])
    got = {e["entity"]: e["bySource"]["pack"] for e in s2.get("entities", []) if e["count"] > 0}
    check(got == snapshot, "thẻ đếm khớp lúc cài, nguồn 'pack'", f"{got}")
    # @ref: every re-seeded contact points (account + primary_for) at an account row that exists in A.
    pointed = int(sql(a.db, f"SELECT COUNT(*) FROM obj_crm_contacts c JOIN obj_crm_accounts x ON x.id = c.account_id AND x.tenant_id = c.tenant_id "
                            f"WHERE c.tenant_id='{tid}' AND c.deleted_at IS NULL AND x.deleted_at IS NULL") or 0)
    check(pointed == snapshot.get("crm_contacts"), "@ref đúng: mọi liên hệ trỏ tới khách hàng thật của A", f"pointed={pointed}")
    links = int(sql(a.db, f"SELECT COUNT(*) FROM rel_crm_contacts_crm_opportunities WHERE tenant_id='{tid}'") or 0)
    check(links == 4, "có đủ 4 liên kết N-N liên hệ ↔ cơ hội", f"links={links}")
    marked = int(sql(a.db, f"SELECT COUNT(*) FROM sys_sample_records WHERE tenant_id='{tid}' AND source='pack'") or 0)
    batches = int(sql(a.db, f"SELECT COUNT(DISTINCT batch_id) FROM sys_sample_records WHERE tenant_id='{tid}'") or 0)
    check(marked == inserted and batches == 1 and d.get("batchId"), "đúng số dấu 'pack', một lô duy nhất", f"marked={marked} batches={batches}")
    jobs = int(sql(a.db, f"SELECT COUNT(*) FROM sys_workflow_runs WHERE tenant_id='{tid}'") or 0)
    check(jobs == 0, "không có lượt chạy luật nào trên dòng mẫu", f"runs={jobs}")

    print("4. Sinh lần hai")
    code, body = generate(base, A["ws"], list(snapshot), "pack")
    d = body.get("data") or {}
    skipped = {s["entity"]: s["code"] for s in d.get("skipped", [])}
    check(code == 201 and not d.get("created") and set(skipped) == set(snapshot) and set(skipped.values()) == {"SAMPLE_ALREADY_PRESENT"},
          "mọi bảng bị bỏ qua SAMPLE_ALREADY_PRESENT, không tạo thêm", f"{code} created={d.get('created')} skipped={skipped}")
    check(summary(base, A["ws"]).get("total") == inserted, "số dòng mẫu không đổi")

    print("5. Tự động / nguồn lạ")
    code, body = req("POST", base + "/admin/entities", A["ws"], {"name": "spk_notes", "label": "Ghi chú", "fields": [
        {"name": "title", "label": "Tiêu đề", "fieldType": "text", "isRequired": True}]})
    check(code in (200, 201), "dựng tay bảng spk_notes", f"{code} {body.get('code', '')}")
    code, body = generate(base, A["ws"], ["crm_accounts", "spk_notes"], "auto", 3)
    d = body.get("data") or {}
    check(code == 201 and {c["entity"]: c["count"] for c in d.get("created", [])} == {"spk_notes": 3}
          and {s["entity"]: s["code"] for s in d.get("skipped", [])} == {"crm_accounts": "SAMPLE_ALREADY_PRESENT"},
          "auto: bảng của gói bỏ qua, bảng dựng tay sinh 3 dòng theo trường", f"{code} {d}")
    s3 = {e["entity"]: e["bySource"] for e in summary(base, A["ws"]).get("entities", [])}
    check(s3.get("spk_notes", {}).get("generator") == 3, "thẻ: spk_notes nguồn 'Sinh tự động' 3")
    code, body = generate(base, A["ws"], ["spk_notes"], "pack")
    check(code == 422 and body.get("code") == "SAMPLE_PACK_UNAVAILABLE", "pack chỉ với bảng dựng tay ⇒ 422 SAMPLE_PACK_UNAVAILABLE", f"{code} {body.get('code', '')}")
    code, body = generate(base, A["ws"], ["spk_notes"], "wat")
    check(code == 422 and body.get("code") == "SAMPLE_SOURCE_INVALID", "nguồn lạ ⇒ 422 SAMPLE_SOURCE_INVALID", f"{code} {body.get('code', '')}")

    print("6. Người thứ ba")
    if A["ownerPass"]:
        owner = login(base, A["slug"], A["ownerEmail"], A["ownerPass"])
        code, body = generate(base, owner, ["spk_notes"], "pack")
        check(code == 403, "quản trị của khách A (token thường) gọi route sinh ⇒ 403", f"{code}")
    staff = login(base, a.tenant, "test@example.com", os.environ.get("NEXUS_STAFF_PASS") or "Demo-Rep-2026!")
    code, body = generate(base, staff, ["crm_accounts"], "pack")
    check(code == 403, "nhân viên không-admin test@example.com gọi route sinh ⇒ 403", f"{code}")

    if failures:
        print(f"\nCHƯA ĐẠT: {len(failures)} kiểm tra đỏ")
        sys.exit(1)
    print("\nĐẠT")


if __name__ == "__main__":
    main()
